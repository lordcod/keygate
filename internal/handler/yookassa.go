package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func (h *AdminHandler) validateYooKassaSettings(ctx context.Context, writes map[string]string) error {
	touched := false
	for key := range writes {
		if strings.HasPrefix(key, "yookassa_") {
			touched = true
		}
	}
	if !touched {
		return nil
	}
	values, err := h.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	for key, value := range writes {
		values[key] = value
	}
	if raw := values["yookassa_vat_code"]; raw != "" {
		vat, err := strconv.Atoi(raw)
		if err != nil || vat < 0 || vat > 12 {
			return fmt.Errorf("yookassa_vat_code must be between 0 and 12")
		}
	}
	if values["yookassa_mode"] == "database" {
		shop := strings.TrimSpace(values["yookassa_shop_id"])
		if shop == "" || strings.IndexFunc(shop, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return fmt.Errorf("YooKassa shop ID must contain digits")
		}
		if strings.TrimSpace(values["yookassa_secret_key"]) == "" {
			return fmt.Errorf("YooKassa secret key is required")
		}
	}
	return nil
}

func validateYooKassaPlan(amount int64, kind, interval string, signup bool) error {
	if amount < 0 || amount > 100000000 {
		return fmt.Errorf("yookassa_amount must be between 0 and 100000000 kopecks")
	}
	if amount == 0 {
		return nil
	}
	if signup {
		return fmt.Errorf("YooKassa paid plans cannot enable self_signup")
	}
	if kind != "perpetual" && kind != "subscription" {
		return fmt.Errorf("YooKassa supports perpetual and subscription plans")
	}
	if kind == "subscription" && interval != "month" && interval != "year" {
		return fmt.Errorf("YooKassa subscriptions require a month or year billing_interval")
	}
	return nil
}
