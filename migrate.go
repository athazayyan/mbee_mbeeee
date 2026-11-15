package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/00001_auth.up.sql
var authMigration string

func migrate(ctx context.Context) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required to run the migration")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, authMigration); err != nil {
		return fmt.Errorf("apply 00001_auth.up.sql: %w", err)
	}
	log.Print("Applied 00001_auth.up.sql")
	return nil
}
