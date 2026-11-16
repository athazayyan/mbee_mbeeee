package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type holdPolicyInput struct {
	Version     string                     `json:"version"`
	EffectiveAt time.Time                  `json:"effective_at"`
	PlatformFee *int64                     `json:"platform_fee"`
	ProviderFee *int64                     `json:"provider_fee"`
	RefundTerms string                     `json:"refund_terms"`
	Refund      map[string]json.RawMessage `json:"refund_policy"`
}

func installHoldPolicy(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: tenantku hold-policy <policy.json>")
	}
	input, err := readHoldPolicy(args[0])
	if err != nil {
		return err
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required to install a hold policy")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return errors.New("could not connect to PostgreSQL for hold policy installation")
	}
	defer conn.Close(context.Background())

	values := []any{input.Version, input.EffectiveAt, *input.PlatformFee, *input.ProviderFee, input.RefundTerms, input.Refund}
	tag, err := conn.Exec(ctx, `INSERT INTO hold_policy_versions
		(version, effective_at, platform_fee, provider_fee, refund_terms, refund_policy)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, values...)
	if err != nil {
		var dbError *pgconn.PgError
		if errors.As(err, &dbError) {
			switch dbError.Code {
			case "23514":
				return errors.New("hold policy violates a database constraint: check version, fees, and refund terms")
			case "42501":
				return errors.New("install hold policies with the database migration role")
			case "42P01":
				return errors.New("hold policy table is unavailable; run the database migrations")
			}
		}
		return errors.New("could not insert hold policy")
	}
	if tag.RowsAffected() == 1 {
		slog.Info("hold policy installed", "version", input.Version, "effective_at", input.EffectiveAt.UTC())
		return nil
	}

	var matches bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hold_policy_versions
		WHERE version = $1 AND effective_at = $2 AND platform_fee = $3 AND provider_fee = $4
		AND refund_terms = $5 AND refund_policy = $6)`, values...).Scan(&matches)
	if err != nil {
		return errors.New("could not compare existing hold policy")
	}
	if !matches {
		return errors.New("hold policy version or effective time is already in use; provide a new version and time")
	}
	slog.Info("hold policy already installed", "version", input.Version, "effective_at", input.EffectiveAt.UTC())
	return nil
}

func readHoldPolicy(path string) (holdPolicyInput, error) {
	file, err := os.Open(path)
	if err != nil {
		return holdPolicyInput{}, fmt.Errorf("open hold policy file: %w", err)
	}
	defer file.Close()

	var input holdPolicyInput
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("decode hold policy: %w", err)
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return input, errors.New("hold policy file must contain exactly one JSON object")
	}
	if input.Version == "" || input.EffectiveAt.IsZero() || input.PlatformFee == nil || input.ProviderFee == nil ||
		input.RefundTerms == "" || len(input.Refund) == 0 {
		return input, errors.New("hold policy requires version, effective_at, both fees, refund_terms, and a non-empty refund_policy object")
	}
	return input, nil
}
