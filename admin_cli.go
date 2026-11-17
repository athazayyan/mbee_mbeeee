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
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
)

// manageAdmin memberi atau mencabut admin platform. Sengaja hanya lewat perintah server dengan
// role migration, sehingga admin tidak pernah bisa didapat dari registrasi atau API.
func manageAdmin(ctx context.Context, grant bool, args []string) error {
	command := "revoke-admin"
	if grant {
		command = "grant-admin"
	}
	usage := fmt.Errorf("usage: tenantku %s <email>", command)
	if grant {
		usage = fmt.Errorf("usage: tenantku grant-admin <email> [permission,permission,...]  (default: all of %s)",
			strings.Join(adminperm.All, ", "))
	}
	if len(args) < 1 || len(args) > 2 || (!grant && len(args) != 1) || !strings.Contains(args[0], "@") {
		return usage
	}
	permissions := adminperm.All
	if len(args) == 2 {
		permissions = strings.Split(args[1], ",")
		for _, permission := range permissions {
			if !adminperm.Valid(permission) {
				return fmt.Errorf("unknown permission %q; choose from %s", permission, strings.Join(adminperm.All, ", "))
			}
		}
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
		// Granting again replaces the permission list, which is how an admin is narrowed.
		_, err = conn.Exec(ctx, `INSERT INTO platform_admins (user_id, note, permissions) VALUES ($1, 'granted via CLI', $2)
			ON CONFLICT (user_id) DO UPDATE SET permissions = EXCLUDED.permissions`, userID, permissions)
	} else {
		_, err = conn.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, userID)
	}
	if err != nil {
		return err
	}
	slog.Info("platform admin updated", "command", command, "user_id", userID, "permissions", permissions)
	return nil
}
