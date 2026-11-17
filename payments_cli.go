package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

const paymentsUsage = "usage: tenantku payments <check|reconcile>"

// runPayments holds the operator commands for Midtrans sandbox: a capability check per method (E08-01)
// and a reconciliation pass against the provider and the ledger (E08-04).
func runPayments(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New(paymentsUsage)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	key := os.Getenv("MIDTRANS_SERVER_KEY")
	if databaseURL == "" || key == "" {
		return errors.New("DATABASE_URL and MIDTRANS_SERVER_KEY are required")
	}
	if os.Getenv("MIDTRANS_ENV") != "sandbox" {
		return errors.New("MIDTRANS_ENV must be sandbox")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return errors.New("invalid PostgreSQL configuration")
	}
	defer pool.Close()
	provider := payment.NewMidtrans(key, "sandbox")
	provider.ClientKey = os.Getenv("MIDTRANS_CLIENT_KEY")
	store := payment.Store{DB: pool, Provider: provider}
	if err := store.Ready(ctx); err != nil {
		return fmt.Errorf("payment schema unavailable; run migrate: %w", err)
	}

	switch args[0] {
	case "check":
		results, err := store.CheckMethods(ctx)
		if err != nil {
			return err
		}
		failed := 0
		for _, r := range results {
			switch {
			case r.Err != nil:
				failed++
				fmt.Printf("FAIL  %-14s fee %-9s %v\n", r.Code, r.Fee, r.Err)
			case len(r.Missing) > 0:
				failed++
				fmt.Printf("FAIL  %-14s fee %-9s channels not offered by Midtrans (not activated?): %v\n", r.Code, r.Fee, r.Missing)
			default:
				fmt.Printf("OK    %-14s fee %-9s channels offered on the payment page: %v\n", r.Code, r.Fee, r.Channels)
			}
		}
		fmt.Println("\nNotification URL to set in the Midtrans sandbox dashboard (Settings > Payment > Notification URL):")
		fmt.Println("  <public https origin of this API>/api/v1/webhooks/payments/midtrans")
		if failed > 0 {
			return fmt.Errorf("%d method(s) rejected by Midtrans; disable them in payment_methods or activate the channel", failed)
		}
		return nil
	case "reconcile":
		report, err := store.Reconcile(ctx, 2*time.Minute, 500)
		if err != nil {
			return err
		}
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
		if !report.Balanced || report.LedgerMissing > 0 || report.LedgerMismatch > 0 {
			return errors.New("reconciliation found ledger problems; see payment_exceptions")
		}
		return nil
	}
	return errors.New(paymentsUsage)
}
