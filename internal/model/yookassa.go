package model

import (
	"encoding/json"
	"github.com/uptrace/bun"
	"time"
)

// YooKassaOrder freezes the purchased terms and the exact retry payload.
// Payment methods and request bodies are never exposed in API responses.
type YooKassaOrder struct {
	ShopID           string
	TestMode         bool
	bun.BaseModel    `bun:"table:yookassa_orders"`
	ID               string `bun:",pk"`
	PlanID           string
	Email            string
	Amount           int64
	Quantity         int
	LicenseType      string
	BillingInterval  string
	UpdatesDays      int
	Recurring        bool
	RenewalLicenseID string `bun:",nullzero"`
	PeriodStart      *time.Time
	PaymentID        string          `bun:",nullzero"`
	Request          json.RawMessage `bun:"type:jsonb" json:"-"`
	Status           string
	LicenseID        string `bun:",nullzero"`
	CreatedAt        time.Time
}

type YooKassaSubscription struct {
	PlanID          string
	Quantity        int
	ShopID          string
	TestMode        bool
	bun.BaseModel   `bun:"table:yookassa_subscriptions"`
	LicenseID       string `bun:",pk"`
	PaymentMethodID string `json:"-"`
	Amount          int64
	BillingInterval string
	NextChargeAt    time.Time
	Canceled        bool
}
