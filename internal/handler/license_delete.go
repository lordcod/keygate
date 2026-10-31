package handler

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
	"net/http"
)

func (h *AdminHandler) DeleteLicense(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	if err := h.Store.DeleteLicense(c, id, adminID(c), c.ClientIP()); err != nil {
		switch {
		case errors.Is(err, store.ErrLicenseNotFound):
			response.NotFound(c, "license not found")
		case errors.Is(err, store.ErrLicenseBillingLinked):
			response.Err(c, http.StatusConflict, "LICENSE_DELETE_SUBSCRIPTION_LINKED", err.Error())
		case errors.Is(err, store.ErrLicensePaymentPending):
			response.Err(c, http.StatusConflict, "LICENSE_DELETE_PAYMENT_PENDING", err.Error())
		case errors.Is(err, store.ErrLicenseBillingBusy):
			response.Err(c, http.StatusConflict, "LICENSE_DELETE_BILLING_BUSY", err.Error())
		default:
			response.Internal(c, err)
		}
		return
	}
	response.OK(c, gin.H{"status": "deleted"})
}
