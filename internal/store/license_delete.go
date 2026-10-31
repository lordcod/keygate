package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/tabloy/keygate/internal/model"
	"github.com/uptrace/bun"
)

const YooKassaOperationLock int64 = 0x594f4f4b41535341
const DeletedCheckoutProvider = "stripe_deleted_checkout"

var ErrLicenseBillingLinked = errors.New("cancel and unlink the Stripe subscription before deleting the license")
var ErrLicensePaymentPending = errors.New("a payment or checkout is pending; resolve it before deleting the license")
var ErrLicenseBillingBusy = errors.New("billing is being updated; retry shortly")
var ErrDeletedCheckout = errors.New("license for this checkout was permanently deleted")

// Serializes deletion against an in-flight initial Stripe fulfilment, including
// a worker that checked sessionFulfilled before the deletion committed.
func lockCheckoutLifecycle(ctx context.Context, db bun.IDB, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := db.NewRaw("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "license_checkout:"+sessionID).Exec(ctx)
	return err
}

// DeleteLicense removes the licence and its cascading dependent rows. Only
// payment replay markers and the operator audit survive; customer accounts do.
func (s *Store) DeleteLicense(ctx context.Context, id, actorID, ip string) error {
	return s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Same lock as YooKassa creation/cancellation. A pending charge may still
		// succeed, so refusing deletion is safer than dropping a paid entitlement.
		var acquired bool
		if err := tx.NewRaw("SELECT pg_try_advisory_xact_lock(?)", YooKassaOperationLock).Scan(ctx, &acquired); err != nil {
			return err
		}
		if !acquired {
			return ErrLicenseBillingBusy
		}
		var before model.License
		if err := tx.NewSelect().Model(&before).Where("id = ?", id).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLicenseNotFound
			}
			return err
		}
		if err := LockReferencedRowsIn(ctx, tx, before.PlanID, before.ProductID); err != nil {
			return err
		}
		if err := lockCheckoutLifecycle(ctx, tx, before.StripeCheckoutSessionID); err != nil {
			return err
		}
		var lic model.License
		if err := tx.NewSelect().Model(&lic).Where("id = ?", id).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLicenseNotFound
			}
			return err
		}
		if lic.StripeSubscriptionID != "" {
			return ErrLicenseBillingLinked
		}
		if lic.StripeCheckoutSessionID != before.StripeCheckoutSessionID || lic.PlanID != before.PlanID {
			return ErrLicenseBillingBusy
		}
		pending, err := tx.NewSelect().Table("yookassa_orders").Where("(license_id = ? OR renewal_license_id = ?) AND status = 'pending'", id, id).Exists(ctx)
		if err != nil {
			return err
		}
		if pending {
			return ErrLicensePaymentPending
		}
		pending, err = tx.NewSelect().Table("license_upgrades").Where("license_id = ? AND state = 'pending'", id).Exists(ctx)
		if err != nil {
			return err
		}
		if pending {
			return ErrLicensePaymentPending
		}

		sessions := []string{lic.StripeCheckoutSessionID}
		for _, table := range []string{"license_renewals", "license_upgrades"} {
			var ids []string
			if err := tx.NewSelect().Table(table).Column("stripe_checkout_session_id").Where("license_id = ?", id).Scan(ctx, &ids); err != nil {
				return err
			}
			sessions = append(sessions, ids...)
		}
		for _, session := range sessions {
			if session == "" {
				continue
			}
			if _, err := ClaimProcessedEventIn(ctx, tx, DeletedCheckoutProvider, session); err != nil {
				return err
			}
		}
		// Keep only the order's replay identity/terms. Clearing the FKs allows a
		// physical deletion without changing an already deployed migration.
		if _, err := tx.NewRaw(`UPDATE yookassa_orders SET license_id = NULL, renewal_license_id = NULL,
    status = 'deleted', email = '', request = '{}'::jsonb
    WHERE license_id = ? OR renewal_license_id = ?`, id, id).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model(&lic).WherePK().Exec(ctx); err != nil {
			return err
		}
		// Audit is in the same transaction: failure rolls the deletion back too.
		audit := &model.AuditLog{ID: newID(), Entity: "license", EntityID: id, ProductID: lic.ProductID, Action: "deleted", ActorType: "admin", ActorID: actorID, IPAddress: ip, Changes: map[string]any{"product_id": lic.ProductID}}
		_, err = tx.NewInsert().Model(audit).Exec(ctx)
		return err
	})
}
