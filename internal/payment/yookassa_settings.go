package payment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Resolve an immutable snapshot per operation. Database mode never mixes
// the shop's credentials with environment values.
func (h *YooKassaHandler) configured(ctx context.Context) (*YooKassaHandler, error) {
	values, err := h.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	configured := *h
	configured.resolved = true
	mode := values["yookassa_mode"]
	if mode == "" || mode == "env" {
		return &configured, nil
	}
	configured.ShopID, configured.SecretKey = "", ""
	if mode == "disabled" {
		return &configured, nil
	}
	if mode != "database" {
		return nil, fmt.Errorf("invalid YooKassa configuration mode")
	}
	configured.ShopID = strings.TrimSpace(values["yookassa_shop_id"])
	configured.SecretKey, err = h.Store.GetSecretSetting(ctx, "yookassa_secret_key")
	if err != nil {
		return nil, err
	}
	configured.TestMode = values["yookassa_test_mode"] == "true"
	configured.VATCode = 0
	if raw := values["yookassa_vat_code"]; raw != "" {
		configured.VATCode, err = strconv.Atoi(raw)
		if err != nil || configured.VATCode < 0 || configured.VATCode > 12 {
			return nil, fmt.Errorf("invalid YooKassa VAT code")
		}
	}
	return &configured, nil
}
