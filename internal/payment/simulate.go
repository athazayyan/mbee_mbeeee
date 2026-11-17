package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// SimulateSuccess marks one of the caller's own open attempts as settled, for the Midtrans sandbox where nothing can
// really be paid. It applies a settlement through the same path as a verified webhook (booking confirmed, ledger
// booked, plan activated), but skips the provider status check, which would still say "pending".
//
// It is enabled only by Store.Simulation, which main sets from PAYMENT_SIMULATION=true, and payments themselves only run
// against the sandbox. Calling it for an attempt that is already paid changes nothing.
func (s Store) SimulateSuccess(ctx context.Context, actor, attemptID string) error {
	if !s.Simulation || s.DB == nil {
		return ErrUnavailable
	}
	var status, method string
	var amount int64
	err := s.DB.QueryRow(ctx, `SELECT status, method, amount FROM payment_attempts WHERE id = $1 AND created_by = $2`,
		attemptID, actor).Scan(&status, &method, &amount)
	if err != nil {
		return err
	}
	switch status {
	case "paid":
		return nil
	case "created", "pending":
	default:
		return ErrNotPayable
	}
	// Report the payment type the chosen method accepts, so the method check passes like a real payment would.
	paymentType := "bank_transfer"
	var confirm []string
	if err := s.DB.QueryRow(ctx, `SELECT confirm_types FROM payment_methods WHERE code = $1`, method).Scan(&confirm); err == nil && len(confirm) > 0 {
		paymentType = confirm[0]
	} else if err != nil && err != pgx.ErrNoRows {
		return err
	}
	n := Notification{
		OrderID: attemptID, StatusCode: "200", GrossAmount: fmt.Sprintf("%d.00", amount), Currency: "IDR",
		TransactionStatus: "settlement", FraudStatus: "accept", TransactionID: "simulated-" + attemptID, PaymentType: paymentType,
	}
	payload, err := json.Marshal(map[string]any{"simulated": true, "notification": n})
	if err != nil {
		return err
	}
	return s.ApplyNotification(ctx, n, payload)
}

func (s Store) simulationRoutes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	// Clients ask first, so the button only appears when the server allows it.
	api.GET("/payments/simulation", func(c *gin.Context) {
		httpapi.Success(c, http.StatusOK, gin.H{"enabled": s.Simulation && s.Provider != nil})
	})
	if !s.Simulation {
		return
	}
	api.POST("/payments/:id/simulate-success", httpapi.RequireOrigin(origin), sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		id := c.Param("id")
		if !httpapi.ValidID(id) {
			httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Pembayaran tidak ditemukan.", nil)
			return
		}
		switch err := s.SimulateSuccess(c.Request.Context(), account.ID, id); {
		case err == nil:
			c.Status(http.StatusNoContent)
		case err == pgx.ErrNoRows:
			httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Pembayaran tidak ditemukan.", nil)
		case err == ErrNotPayable:
			httpapi.Error(c, http.StatusConflict, "PAYMENT_NOT_OPEN", "Pembayaran ini sudah ditutup dan tidak dapat disimulasikan.", nil)
		default:
			httpapi.LogInternalError(c, "payment simulation failed", err)
			httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Simulasi pembayaran belum dapat diproses.", nil)
		}
	}))
}
