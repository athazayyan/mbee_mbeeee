package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

func recordVARefund(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: tenantku record-va-refund <booking-id> <amount-idr> <bank-reference>")
	}
	amount, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || amount < 1 {
		return errors.New("refund amount must be a positive whole rupiah amount")
	}
	if os.Getenv("MIDTRANS_ENV") != "sandbox" || os.Getenv("MIDTRANS_SERVER_KEY") == "" || os.Getenv("DATABASE_URL") == "" {
		return errors.New("sandbox VA refund recording requires the database URL and Midtrans sandbox credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("invalid PostgreSQL configuration")
	}
	defer pool.Close()
	store := payment.Store{DB: pool, Provider: payment.NewMidtrans(os.Getenv("MIDTRANS_SERVER_KEY"), "sandbox")}
	return store.RecordManualVARefund(ctx, args[0], amount, args[2])
}
