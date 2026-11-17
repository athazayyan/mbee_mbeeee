package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/booking"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

const (
	expiryBatchSize   = 100
	expiryInterval    = 5 * time.Second
	expiryTimeout     = 15 * time.Second
	expiryReadyWindow = 2*expiryInterval + expiryTimeout
)

type expiryReadiness struct {
	lastSuccess atomic.Pointer[time.Time]
}

func (r *expiryReadiness) record(err error, now time.Time) {
	if err != nil {
		r.lastSuccess.Store(nil)
		return
	}
	r.lastSuccess.Store(&now)
}

func (r *expiryReadiness) ready(now time.Time) bool {
	last := r.lastSuccess.Load()
	return last != nil && now.Sub(*last) < expiryReadyWindow
}

func (r *expiryReadiness) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if !r.ready(time.Now()) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	return mux
}

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
	var refunds payment.Store
	if os.Getenv("EVENT_REFUND_SANDBOX_ENABLED") == "true" {
		if os.Getenv("MIDTRANS_ENV") != "sandbox" || os.Getenv("MIDTRANS_SERVER_KEY") == "" {
			return errors.New("sandbox refunds require MIDTRANS_ENV=sandbox and MIDTRANS_SERVER_KEY")
		}
		refunds = payment.Store{DB: pool, Provider: payment.NewMidtrans(os.Getenv("MIDTRANS_SERVER_KEY"), "sandbox")}
	}
	runBatch := func() error {
		if err := expireBatch(ctx, store, logger); err != nil {
			return err
		}
		if refunds.Provider == nil {
			return nil
		}
		probe, stop := context.WithTimeout(ctx, expiryTimeout)
		defer stop()
		err := refunds.ProcessCancellationRefund(probe)
		if err != nil {
			logger.Error("event refund processing failed", "error_code", expiryErrorCode(err))
		}
		return err
	}
	var readiness expiryReadiness
	var listenerErrors <-chan error
	if address := os.Getenv("WORKER_HEALTH_ADDR"); !once && address != "" {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("worker readiness listener: %w", err)
		}
		server := &http.Server{
			Handler: readiness.handler(), ReadHeaderTimeout: 2 * time.Second,
			ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second,
		}
		defer server.Close()
		failure := make(chan error, 1)
		listenerErrors = failure
		go func() { failure <- server.Serve(listener) }()
	}
	logger.Info("worker started", "interval", expiryInterval.String(), "once", once)
	if once {
		return runBatch()
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return nil
		case err := <-listenerErrors:
			return fmt.Errorf("worker readiness server: %w", err)
		case <-timer.C:
			// A failure is logged with its job ID and retried on the next cycle.
			err := runBatch()
			if ctx.Err() == nil {
				readiness.record(err, time.Now())
			}
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
