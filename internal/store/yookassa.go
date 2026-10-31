package store

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/tabloy/keygate/internal/model"
	"github.com/uptrace/bun"
	"time"
)

// CreateYooKassaLicense commits the order, saved method and licence together.
// The deferred foreign keys permit the guard to reference the new licence.
func (s *Store) CreateYooKassaLicense(ctx context.Context, order *model.YooKassaOrder, lic *model.License, plan *model.Plan, sub *model.YooKassaSubscription) error {
	return s.createLicenseWithSubscription(ctx, lic, plan, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewRaw("UPDATE yookassa_orders SET status = 'succeeded', license_id = ? WHERE id = ? AND status = 'pending'", lic.ID, order.ID).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("order already processed")
		}
		if sub != nil {
			_, err = tx.NewInsert().Model(sub).Exec(ctx)
		}
		return err
	})
}

func (s *Store) RenewYooKassaLicense(ctx context.Context, order *model.YooKassaOrder, until time.Time) error {
	return s.DB.RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		var lic model.License
		if err := tx.NewSelect().Model(&lic).Where("id = ?", order.RenewalLicenseID).For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		if lic.PaymentProvider != "yookassa" || lic.PlanID != order.PlanID || lic.Quantity != order.Quantity || lic.ValidUntil == nil || order.PeriodStart == nil || !lic.ValidUntil.Equal(*order.PeriodStart) || (lic.Status != model.StatusActive && lic.Status != model.StatusExpired && lic.Status != model.StatusPastDue) {
			return fmt.Errorf("paid renewal requires manual review: license terms or status changed")
		}
		res, err := tx.NewRaw("UPDATE yookassa_orders SET status = 'succeeded', license_id = ? WHERE id = ? AND status = 'pending'", lic.ID, order.ID).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return nil
		}
		lic.Status, lic.ValidUntil, lic.PastDueAt = model.StatusActive, &until, nil
		if err := UpdateLicenseIn(ctx, tx, &lic, "status", "valid_until", "past_due_at"); err != nil {
			return err
		}
		if _, err := tx.NewRaw("UPDATE subscriptions SET status = 'active', current_period_end = ?, updated_at = now() WHERE license_id = ?", until, lic.ID).Exec(ctx); err != nil {
			return err
		}
		_, err = tx.NewRaw("UPDATE yookassa_subscriptions SET next_charge_at = ? WHERE license_id = ?", until, lic.ID).Exec(ctx)
		return err
	})
}
