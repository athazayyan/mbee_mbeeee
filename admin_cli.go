package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// manageAdmin memberi atau mencabut admin platform. Sengaja hanya lewat perintah server dengan
// role migration, sehingga admin tidak pernah bisa didapat dari registrasi atau API.
func manageAdmin(ctx context.Context, grant bool, args []string) error {
	command := "revoke-admin"
	if grant {
		command = "grant-admin"
	}
	if len(args) != 1 || !strings.Contains(args[0], "@") {
		return fmt.Errorf("usage: tenantku %s <email>", command)
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required to manage platform admins")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return errors.New("could not connect to PostgreSQL to manage platform admins")
	}
	defer conn.Close(context.Background())

	var userID string
	if err := conn.QueryRow(ctx, `SELECT id::text FROM users WHERE email = $1`, email).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("no account uses that email; register the account first")
		}
		return err
	}
	if grant {
		_, err = conn.Exec(ctx, `INSERT INTO platform_admins (user_id, note) VALUES ($1, 'granted via CLI')
			ON CONFLICT (user_id) DO NOTHING`, userID)
	} else {
		_, err = conn.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, userID)
	}
	if err != nil {
		return err
	}
	slog.Info("platform admin updated", "command", command, "user_id", userID)
	return nil
}
