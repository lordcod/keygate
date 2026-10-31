package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
	"github.com/uptrace/bun"
)

// All replicas serialize YooKassa charge creation, fulfilment and cancellation.
// Network calls are bounded; no database row locks are held across them.
const yooLock int64 = store.YooKassaOperationLock

type YooKassaHandler struct {
	resolved                   bool
	Store                      *store.Store
	ShopID, SecretKey, BaseURL string
	TestMode                   bool
	VATCode                    int // 0: receipts handled outside this integration.
	Email                      *service.EmailService
	WebhookSvc                 *service.WebhookService
}

type yooAmount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}
type yooPayment struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Paid      bool      `json:"paid"`
	Test      bool      `json:"test"`
	Amount    yooAmount `json:"amount"`
	Recipient struct {
		AccountID string `json:"account_id"`
	} `json:"recipient"`
	Metadata     map[string]string `json:"metadata"`
	Confirmation struct {
		URL string `json:"confirmation_url"`
	} `json:"confirmation"`
	PaymentMethod struct {
		ID    string `json:"id"`
		Saved bool   `json:"saved"`
	} `json:"payment_method"`
}

func (h *YooKassaHandler) Enabled() bool { return h.ShopID != "" && h.SecretKey != "" }

// Do not park a connection per waiter: fulfilment needs another connection
// and a blocking advisory lock could otherwise exhaust the database pool.
func (h *YooKassaHandler) withLock(ctx context.Context, fn func(context.Context, bun.Tx) error) error {
	return h.Store.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var acquired bool
		if err := tx.NewRaw("SELECT pg_try_advisory_xact_lock(?)", yooLock).Scan(ctx, &acquired); err != nil {
			return err
		}
		if !acquired {
			return fmt.Errorf("YooKassa operation in progress; retry shortly")
		}
		return fn(ctx, tx)
	})
}
func yooMoney(amount int64) string { return fmt.Sprintf("%d.%02d", amount/100, amount%100) }

func (h *YooKassaHandler) api(ctx context.Context, method, path, key string, body []byte) (*yooPayment, error) {
	if !h.Enabled() {
		return nil, fmt.Errorf("YooKassa is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.yookassa.ru/v3/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(h.ShopID, h.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotence-Key", key)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("YooKassa HTTP %d", res.StatusCode)
	}
	var p yooPayment
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func yooNextPeriod(start time.Time, interval string) time.Time {
	// Clamp to the last day rather than Go's March overflow for January 31.
	months := 1
	if interval == "year" {
		months = 12
	}
	first := time.Date(start.Year(), start.Month(), 1, start.Hour(), start.Minute(), start.Second(), start.Nanosecond(), time.UTC).AddDate(0, months, 0)
	last := first.AddDate(0, 1, -1).Day()
	day := start.Day()
	if day > last {
		day = last
	}
	return first.AddDate(0, 0, day-1)
}

func (h *YooKassaHandler) payload(o *model.YooKassaOrder, description, method string) ([]byte, error) {
	amount := yooAmount{yooMoney(o.Amount), "RUB"}
	desc := []rune(description)
	if len(desc) > 128 {
		desc = desc[:128]
	}
	body := map[string]any{"amount": amount, "capture": true, "description": string(desc), "metadata": map[string]string{"keygate_order_id": o.ID}}
	if method == "" {
		body["confirmation"] = map[string]string{"type": "redirect", "return_url": strings.TrimRight(h.BaseURL, "/") + "/pay/yookassa/status/" + o.ID}
		if o.Recurring {
			body["save_payment_method"] = true
		}
	} else {
		body["payment_method_id"] = method
	}
	if h.VATCode > 0 {
		body["receipt"] = map[string]any{"customer": map[string]string{"email": o.Email}, "items": []any{map[string]any{
			"description": string(desc), "quantity": "1.00", "amount": amount, "vat_code": h.VATCode, "payment_mode": "full_payment", "payment_subject": "intellectual_activity",
		}}}
	}
	return json.Marshal(body)
}

// CheckoutByPlan displays the price and obtains explicit recurring consent.
func (h *YooKassaHandler) CheckoutByPlan(c *gin.Context) {
	if !h.resolved {
		configured, err := h.configured(c.Request.Context())
		if err != nil {
			response.Internal(c, err)
			return
		}
		configured.CheckoutByPlan(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	plan, err := h.Store.FindPlanByCheckoutID(c, c.Param("checkout_id"))
	if err != nil || !plan.Active || plan.YooKassaAmount <= 0 {
		c.String(404, "Тариф недоступен")
		return
	}
	if !h.Enabled() {
		c.String(503, "ЮKassa не настроена")
		return
	}
	if !(&StripeHandler{Store: h.Store}).boundedPlanSellable(c, plan) {
		c.String(503, "Тариф временно недоступен")
		return
	}
	quantity := 1
	if plan.PerSeat {
		quantity = plan.MinQuantity
	}
	rawQuantity := c.Query("quantity")
	if c.Request.Method == http.MethodPost {
		rawQuantity = c.PostForm("quantity")
	}
	if rawQuantity != "" {
		quantity, err = strconv.Atoi(rawQuantity)
		if err != nil {
			c.String(400, "Некорректное количество")
			return
		}
	}
	if quantity < 1 || (!plan.PerSeat && quantity != 1) || (plan.PerSeat && (quantity < plan.MinQuantity || quantity > plan.MaxQuantity)) {
		c.String(400, "Количество вне допустимого диапазона")
		return
	}
	amount := plan.YooKassaAmount * int64(quantity)
	if amount > 100000000 {
		c.String(400, "Сумма платежа слишком велика")
		return
	}
	recurring := plan.LicenseType == "subscription"
	if plan.LicenseType != "perpetual" && !recurring {
		c.String(400, "Этот тип тарифа не поддерживается")
		return
	}
	if recurring && plan.BillingInterval != "month" && plan.BillingInterval != "year" {
		c.String(400, "Не задан период подписки")
		return
	}
	if c.Request.Method == http.MethodGet {
		period := "месяц"
		if plan.BillingInterval == "year" {
			period = "год"
		}
		h.page(c, yooPage{Title: plan.Name, Amount: yooMoney(amount), Quantity: quantity, Recurring: recurring, Period: period, Form: true})
		return
	}
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		c.String(400, "Укажите корректный email")
		return
	}
	if recurring && c.PostForm("consent") != "yes" {
		c.String(400, "Для подписки нужно согласие на автоплатежи")
		return
	}
	o := &model.YooKassaOrder{ShopID: h.ShopID, TestMode: h.TestMode, ID: uuid.NewString(), PlanID: plan.ID, Email: email, Amount: amount, Quantity: quantity, LicenseType: plan.LicenseType, BillingInterval: plan.BillingInterval, UpdatesDays: plan.UpdatesDays, Recurring: recurring, Status: "pending", CreatedAt: time.Now().UTC()}
	o.Request, err = h.payload(o, plan.Name, "")
	if err != nil {
		response.Internal(c, err)
		return
	}
	if _, err = h.Store.DB.NewInsert().Model(o).Exec(c); err != nil {
		response.Internal(c, err)
		return
	}
	p, err := h.process(c.Request.Context(), o.ID, "")
	if err != nil {
		slog.Error("yookassa checkout pending", "order_id", o.ID, "error", err)
	}
	if p != nil && p.Status == "pending" && p.Confirmation.URL != "" {
		u, err := url.Parse(p.Confirmation.URL)
		if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
			c.Redirect(303, u.String())
			return
		}
	}
	c.Redirect(303, h.BaseURL+"/pay/yookassa/status/"+o.ID)
}

func (h *YooKassaHandler) process(ctx context.Context, id, notifiedPayment string) (*yooPayment, error) {
	var payment *yooPayment
	err := h.withLock(ctx, func(ctx context.Context, _ bun.Tx) error {
		var o model.YooKassaOrder
		if err := h.Store.DB.NewSelect().Model(&o).Where("id = ?", id).Scan(ctx); err != nil {
			return err
		}
		if o.ShopID != h.ShopID || o.TestMode != h.TestMode {
			return fmt.Errorf("order belongs to another YooKassa shop or environment")
		}
		if o.Status != "pending" {
			return nil
		}
		paymentID := o.PaymentID
		if notifiedPayment != "" {
			if paymentID != "" && paymentID != notifiedPayment {
				return fmt.Errorf("notification payment mismatch")
			}
			paymentID = notifiedPayment
		}
		var err error
		if paymentID != "" {
			payment, err = h.api(ctx, "GET", "payments/"+url.PathEscape(paymentID), "", nil)
		} else {
			// YooKassa only guarantees idempotency for 24 hours. Never create
			// a second payment after an ambiguous response ages beyond that.
			if time.Since(o.CreatedAt) > 23*time.Hour {
				return fmt.Errorf("payment creation outcome unknown; reconcile order %s in YooKassa", o.ID)
			}
			if o.RenewalLicenseID != "" {
				var sub model.YooKassaSubscription
				if err := h.Store.DB.NewSelect().Model(&sub).Where("license_id = ?", o.RenewalLicenseID).Scan(ctx); err != nil {
					return err
				}
				lic, err := h.Store.FindLicenseByID(ctx, o.RenewalLicenseID)
				if err != nil {
					return err
				}
				if sub.Canceled || lic.Quantity != sub.Quantity || lic.ValidUntil == nil || !lic.ValidUntil.Equal(sub.NextChargeAt) || (lic.Status != model.StatusActive && lic.Status != model.StatusExpired && lic.Status != model.StatusPastDue) || lic.PlanID != o.PlanID || lic.PaymentProvider != "yookassa" {
					return nil // Never initiate another charge after cancellation/revocation.
				}
			}
			payment, err = h.api(ctx, "POST", "payments", o.ID, o.Request)
		}
		if err != nil {
			return err
		}
		if payment.ID == "" || (paymentID != "" && payment.ID != paymentID) || payment.Metadata["keygate_order_id"] != o.ID || payment.Amount.Value != yooMoney(o.Amount) || payment.Amount.Currency != "RUB" || payment.Recipient.AccountID != h.ShopID || payment.Test != h.TestMode {
			return fmt.Errorf("YooKassa payment does not match order or configured environment")
		}
		if _, err := h.Store.DB.NewRaw("UPDATE yookassa_orders SET payment_id = ? WHERE id = ?", payment.ID, o.ID).Exec(ctx); err != nil {
			return err
		}
		o.PaymentID = payment.ID
		switch payment.Status {
		case "succeeded":
			if !payment.Paid {
				return fmt.Errorf("successful payment is not paid")
			}
			return h.fulfill(ctx, &o, payment)
		case "canceled":
			return h.Store.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				if _, err := tx.NewRaw("UPDATE yookassa_orders SET status = 'canceled' WHERE id = ?", o.ID).Exec(ctx); err != nil {
					return err
				}
				// A declined recurring charge stops auto-renewal. No surprise retries.
				if o.RenewalLicenseID != "" {
					if _, err := tx.NewRaw("UPDATE yookassa_subscriptions SET canceled = true WHERE license_id = ?", o.RenewalLicenseID).Exec(ctx); err != nil {
						return err
					}
					_, err := tx.NewRaw("UPDATE subscriptions SET cancel_at_period_end = true, updated_at = now() WHERE license_id = ?", o.RenewalLicenseID).Exec(ctx)
					return err
				}
				return nil
			})
		}
		return nil
	})
	return payment, err
}

func (h *YooKassaHandler) fulfill(ctx context.Context, o *model.YooKassaOrder, p *yooPayment) error {
	now := time.Now().UTC()
	if o.RenewalLicenseID != "" {
		if o.PeriodStart == nil {
			return fmt.Errorf("renewal has no period")
		}
		start := *o.PeriodStart
		if start.Before(now) {
			start = now
		}
		return h.Store.RenewYooKassaLicense(ctx, o, yooNextPeriod(start, o.BillingInterval))
	}
	plan, err := h.Store.FindPlanByID(ctx, o.PlanID)
	if err != nil {
		return err
	}
	if plan.LicenseType != o.LicenseType {
		return fmt.Errorf("paid plan was retyped; manual reconciliation required")
	}
	key, err := h.Store.NewLicenseKey(ctx, plan.ProductID)
	if err != nil {
		return err
	}
	if err := h.Store.UpsertUser(ctx, &model.User{Email: o.Email}); err != nil {
		return err
	}
	user, err := h.Store.FindUserByEmail(ctx, o.Email)
	if err != nil {
		return err
	}
	lic := &model.License{ID: uuid.NewString(), ProductID: plan.ProductID, PlanID: plan.ID, UserID: user.ID, Email: o.Email, LicenseKey: key, PaymentProvider: "yookassa", Status: model.StatusActive, Quantity: o.Quantity, ValidFrom: now, UpdatesUntil: model.UpdatesUntilFor(o.LicenseType, o.UpdatesDays, now), UpdatesTermsSet: true}
	var sub *model.YooKassaSubscription
	if o.Recurring {
		until := yooNextPeriod(now, o.BillingInterval)
		lic.ValidUntil = &until
		sub = &model.YooKassaSubscription{PlanID: o.PlanID, Quantity: o.Quantity, ShopID: h.ShopID, TestMode: h.TestMode, LicenseID: lic.ID, Amount: o.Amount, BillingInterval: o.BillingInterval, NextChargeAt: until, PaymentMethodID: p.PaymentMethod.ID, Canceled: !p.PaymentMethod.Saved || p.PaymentMethod.ID == ""}
	}
	if err := h.Store.CreateYooKassaLicense(ctx, o, lic, plan, sub); err != nil {
		return err
	}
	// The licence is already durable. Notification failure must not issue it twice.
	if h.Email != nil {
		productName, download := "Your Software", ""
		if prod, err := h.Store.FindProductByID(ctx, plan.ProductID); err == nil {
			productName, download = prod.Name, prod.DownloadURL
		}
		subject, body := h.Email.RenderLicenseCreated(productName, plan.Name, h.Store.DecryptLicenseKey(lic), download)
		if err := h.Store.EnqueueEmail(ctx, o.Email, subject, body); err != nil {
			slog.Error("yookassa license email enqueue failed", "license_id", lic.ID, "error", err)
		}
	}
	h.Store.Audit(ctx, &model.AuditLog{Entity: "license", EntityID: lic.ID, Action: "created", ActorType: "yookassa", Changes: map[string]any{"payment_id": p.ID}})
	if h.WebhookSvc != nil {
		h.WebhookSvc.Dispatch(ctx, lic.ProductID, "license.created", map[string]any{"license_id": lic.ID, "email": lic.Email, "plan_id": lic.PlanID})
	}
	return nil
}

func (h *YooKassaHandler) Webhook(c *gin.Context) {
	if !h.resolved {
		configured, err := h.configured(c.Request.Context())
		if err != nil {
			response.Internal(c, err)
			return
		}
		configured.Webhook(c)
		return
	}
	if !h.Enabled() {
		c.Status(503)
		return
	}
	var notification struct {
		Event  string `json:"event"`
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(c.Request.Body, 65536)).Decode(&notification); err != nil {
		c.Status(400)
		return
	}
	if notification.Event != "payment.succeeded" && notification.Event != "payment.canceled" {
		c.Status(200)
		return
	}
	if _, err := uuid.Parse(notification.Object.ID); err != nil {
		c.Status(400)
		return
	}
	// Webhook bodies are untrusted. Fetch the object with merchant credentials.
	p, err := h.api(c.Request.Context(), "GET", "payments/"+url.PathEscape(notification.Object.ID), "", nil)
	if err != nil {
		c.Status(503)
		return
	}
	id := p.Metadata["keygate_order_id"]
	if _, err := uuid.Parse(id); err != nil {
		c.Status(200)
		return
	}
	var exists bool
	exists, err = h.Store.DB.NewSelect().Model((*model.YooKassaOrder)(nil)).Where("id = ?", id).Exists(c)
	if err != nil {
		c.Status(503)
		return
	}
	if !exists {
		c.Status(200)
		return
	}
	if _, err := h.process(c.Request.Context(), id, notification.Object.ID); err != nil {
		slog.Error("yookassa webhook", "order_id", id, "error", err)
		c.Status(503)
		return
	}
	c.Status(200)
}

func (h *YooKassaHandler) CancelSubscription(c *gin.Context) {
	var req struct {
		LicenseID string `json:"license_id" binding:"required"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "license_id is required")
		return
	}
	lic, err := h.Store.FindLicenseByID(c, req.LicenseID)
	if err != nil {
		response.NotFound(c, "license not found")
		return
	}
	if !strings.EqualFold(c.GetString("email"), lic.Email) {
		response.Forbidden(c, "not your license")
		return
	}
	err = h.withLock(c, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewRaw("UPDATE yookassa_subscriptions SET canceled = true WHERE license_id = ?", lic.ID).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("no YooKassa subscription")
		}
		_, err = tx.NewRaw("UPDATE subscriptions SET cancel_at_period_end = true, updated_at = now() WHERE license_id = ?", lic.ID).Exec(ctx)
		return err
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"status": "canceled", "immediate": false})
}

// Sync is safe on every replica. Each renewal period has one durable order;
// retries reuse its exact payload and idempotence key, even after a restart.
func (h *YooKassaHandler) Sync(ctx context.Context) {
	if !h.resolved {
		configured, err := h.configured(ctx)
		if err != nil {
			slog.Error("yookassa settings", "error", err)
			return
		}
		configured.Sync(ctx)
		return
	}
	if !h.Enabled() {
		return
	}
	err := h.withLock(ctx, func(ctx context.Context, tx bun.Tx) error {
		var due []model.YooKassaSubscription
		if err := tx.NewSelect().Model(&due).Where("canceled = false AND next_charge_at <= now() AND shop_id = ? AND test_mode = ?", h.ShopID, h.TestMode).Order("next_charge_at ASC").Limit(100).Scan(ctx); err != nil {
			return err
		}
		for _, sub := range due {
			lic, err := h.Store.FindLicenseByID(ctx, sub.LicenseID)
			if err != nil {
				return err
			}
			if lic.PlanID != sub.PlanID || lic.Quantity != sub.Quantity || lic.ValidUntil == nil || !lic.ValidUntil.Equal(sub.NextChargeAt) || lic.PaymentProvider != "yookassa" || (lic.Status != model.StatusActive && lic.Status != model.StatusExpired && lic.Status != model.StatusPastDue) || lic.Plan == nil || lic.Plan.LicenseType != "subscription" {
				continue
			}
			o := &model.YooKassaOrder{ShopID: h.ShopID, TestMode: h.TestMode, ID: uuid.NewString(), PlanID: lic.PlanID, Email: lic.Email, Amount: sub.Amount, Quantity: lic.Quantity, LicenseType: "subscription", BillingInterval: sub.BillingInterval, RenewalLicenseID: lic.ID, PeriodStart: &sub.NextChargeAt, Status: "pending", CreatedAt: time.Now().UTC()}
			o.Request, err = h.payload(o, lic.Plan.Name, sub.PaymentMethodID)
			if err != nil {
				return err
			}
			if _, err := tx.NewInsert().Model(o).On("CONFLICT (renewal_license_id, period_start) DO NOTHING").Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("yookassa schedule", "error", err)
		return
	}
	var orders []model.YooKassaOrder
	// Cursor pagination prevents a permanently unreconciled order starving newer ones.
	after := "00000000-0000-0000-0000-000000000000"
	for batch := 0; batch < 20; batch++ {
		orders = nil
		if err := h.Store.DB.NewSelect().Model(&orders).Where("status = 'pending' AND id > ? AND shop_id = ? AND test_mode = ?", after, h.ShopID, h.TestMode).Order("id ASC").Limit(50).Scan(ctx); err != nil {
			slog.Error("yookassa pending", "error", err)
			return
		}
		for _, o := range orders {
			if ctx.Err() != nil {
				return
			}
			if _, err := h.process(ctx, o.ID, ""); err != nil {
				slog.Error("yookassa reconciliation", "order_id", o.ID, "error", err)
			}
			after = o.ID
		}
		if len(orders) < 50 {
			break
		}
	}
}

type yooPage struct {
	Title, Amount, Period, Message, PaymentURL string
	Quantity                                   int
	Recurring, Form, Pending                   bool
}

var yooTemplate = template.Must(template.New("yookassa").Parse(`<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}} · Keygate</title><style>body{font:16px/1.6 system-ui,sans-serif;margin:0;background:#fafafa;color:#18181b}main{max-width:32rem;margin:8vh auto;padding:24px}h1{font-size:1.8rem;line-height:1.2}label{display:block;margin:20px 0}input[type=email]{box-sizing:border-box;display:block;width:100%;padding:12px;font:inherit;border:1px solid #71717a;border-radius:6px}button{padding:12px 20px;font:inherit;background:#18181b;color:white;border:0;border-radius:6px;cursor:pointer}a{color:#1d4ed8}footer{margin-top:40px;font-size:14px}input:focus-visible,button:focus-visible,a:focus-visible{outline:3px solid #2563eb;outline-offset:3px}</style></head><body><main><h1>{{.Title}}</h1>{{if .Form}}<p>{{.Amount}} ₽{{if .Recurring}} / {{.Period}}{{end}}</p><form method="post"><input type="hidden" name="quantity" value="{{.Quantity}}"><label>Email для получения лицензии<input type="email" name="email" required maxlength="254" autocomplete="email"></label>{{if .Recurring}}<label><input type="checkbox" name="consent" value="yes" required> Согласен на сохранение способа оплаты и автоматическое списание {{.Amount}} ₽ каждый {{.Period}}. Автопродление можно отключить в личном кабинете; доступ останется до конца оплаченного периода.</label>{{end}}<button type="submit">Оплатить через ЮKassa</button></form>{{else}}<p>{{.Message}}</p>{{if .PaymentURL}}<p><a href="{{.PaymentURL}}">Перейти к оплате</a></p>{{end}}{{if .Pending}}<p><a href="">Проверить статус</a></p>{{end}}<p><a href="/portal">Открыть личный кабинет</a></p>{{end}}<footer><a href="https://keygate.app" target="_blank" rel="noopener noreferrer">Powered by Keygate</a></footer></main></body></html>`))

func (h *YooKassaHandler) page(c *gin.Context, page yooPage) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	if err := yooTemplate.Execute(c.Writer, page); err != nil {
		slog.Warn("yookassa render", "error", err)
	}
}

func (h *YooKassaHandler) Status(c *gin.Context) {
	if !h.resolved {
		configured, err := h.configured(c.Request.Context())
		if err != nil {
			response.Internal(c, err)
			return
		}
		configured.Status(c)
		return
	}
	id := c.Param("order_id")
	if _, err := uuid.Parse(id); err != nil {
		c.Status(404)
		return
	}
	p, err := h.process(c.Request.Context(), id, "")
	if err != nil {
		slog.Warn("yookassa status pending", "order_id", id, "error", err)
	}
	var order model.YooKassaOrder
	if err := h.Store.DB.NewSelect().Model(&order).Where("id = ?", id).Scan(c); err != nil {
		c.Status(404)
		return
	}
	page := yooPage{Title: "Проверка оплаты", Message: "Платёж ещё обрабатывается. Лицензия появится в личном кабинете после подтверждения оплаты.", Pending: true}
	if p != nil {
		page.PaymentURL = p.Confirmation.URL
	}
	if order.Status == "succeeded" {
		page.Title, page.Message, page.Pending = "Оплата прошла", "Лицензия доступна в личном кабинете. Войдите с email, который указали при оплате.", false
	}
	if order.Status == "canceled" {
		page.Title, page.Message, page.Pending = "Платёж отменён", "Оплата не завершена. Вернитесь к тарифу, чтобы попробовать снова.", false
	}
	if order.Status == "deleted" {
		page.Title, page.Message, page.Pending = "Лицензия удалена", "Продавец удалил лицензию для этого платежа. По вопросам доступа обратитесь к продавцу.", false
	}
	h.page(c, page)
}
