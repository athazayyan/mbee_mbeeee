package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/booking"
)

const (
	expiryBatchSize = 100
	expiryInterval  = 5 * time.Second
	expiryTimeout   = 15 * time.Second
)

func runWorker(ctx context.Context, args []string) error {
	once := len(args) == 1 && args[0] == "--once"
	if len(args) != 0 && !once {
		return errors.New("usage: tenantku worker [--once]")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required for the worker")
	}

	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	pool, err := pgxpool.New(startup, databaseURL)
	if err != nil {
		cancel()
		return errors.New("invalid PostgreSQL worker configuration")
	}
	defer pool.Close()
	store := booking.Store{DB: pool}
	err = store.Ready(startup)
	cancel()
	if err != nil {
		return fmt.Errorf("worker readiness check failed (%s); verify the database connection and run migrations", expiryErrorCode(err))
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "booking_expiry")
	logger.Info("worker started", "interval", expiryInterval.String(), "once", once)
	if once {
		return expireBatch(ctx, store, logger)
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return nil
		case <-timer.C:
			// A failure is logged with its job ID and retried on the next cycle.
			_ = expireBatch(ctx, store, logger)
			timer.Reset(expiryInterval)
		}
	}
}

func expireBatch(ctx context.Context, store booking.Store, logger *slog.Logger) error {
	probe, cancel := context.WithTimeout(ctx, expiryTimeout)
	defer cancel()
	job := logger.With("job_id", rand.Text())
	started := time.Now()
	var total int64
	for {
		count, err := store.Expire(probe, expiryBatchSize)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			code := expiryErrorCode(err)
			job.Error("booking expiry failed", "expired", total, "error_code", code)
			return fmt.Errorf("booking expiry failed (%s)", code)
		}
		total += count
		if count < expiryBatchSize {
			break
		}
	}
	if total > 0 {
		job.Info("booking expiry completed", "expired", total, "duration_ms", time.Since(started).Milliseconds())
	} else {
		job.Debug("booking expiry found no work")
	}
	return nil
}

func expiryErrorCode(err error) string {
	var database *pgconn.PgError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.As(err, &database):
		return database.Code
	default:
		return "database_unavailable"
	}
}
