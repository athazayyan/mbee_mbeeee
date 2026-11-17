// Package adminperm holds the fine-grained permissions of platform admins (E16-01) and the redaction
// applied to audit snapshots. An account in platform_admins holds only the permissions listed on its row.
package adminperm

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
)

const (
	DirectoryRead       = "directory.read"       // overview, users, organizations, events, bookings
	OrganizationsVerify = "organizations.verify" // verify or unverify an organizer
	AuditRead           = "audit.read"
	PaymentsRead        = "payments.read"   // payment search and the exception queue
	PaymentsManage      = "payments.manage" // resolve exceptions and re-check a payment with the provider
	FundsManage         = "funds.manage"    // refunds, payouts, disputes
	PricingManage       = "pricing.manage"  // provider fees per method and plan price versions
	ModerationManage    = "moderation.manage"
)

// All lists every permission. A new admin receives all of them unless the command narrows it.
var All = []string{DirectoryRead, OrganizationsVerify, AuditRead, PaymentsRead, PaymentsManage, FundsManage, PricingManage, ModerationManage}

// Valid reports whether the permission exists.
func Valid(permission string) bool { return slices.Contains(All, permission) }

// Querier is satisfied by pgx pools and transactions.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Of returns the permissions of an account, or nil when it is not a platform admin.
func Of(ctx context.Context, q Querier, userID string) ([]string, error) {
	var permissions []string
	err := q.QueryRow(ctx, `SELECT permissions FROM platform_admins WHERE user_id = $1`, userID).Scan(&permissions)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return permissions, err
}

// Has reports whether the account is a platform admin holding the permission.
func Has(ctx context.Context, q Querier, userID, permission string) (bool, error) {
	permissions, err := Of(ctx, q, userID)
	return slices.Contains(permissions, permission), err
}

var sensitiveKey = regexp.MustCompile(`(?i)password|passcode|token|secret|authorization|cookie|card|cvv|account_number|signature|api_?key|server_key`)

// Redacted replaces the value of every sensitive key, at any depth, so a snapshot never carries a secret
// into the append-only audit log. It returns the JSON to store.
func Redacted(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return json.Marshal(redact(tree))
}

const hidden = "[disembunyikan]"

func redact(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if sensitiveKey.MatchString(key) && item != nil {
				v[key] = hidden
			} else {
				v[key] = redact(item)
			}
		}
		return v
	case []any:
		for i := range v {
			v[i] = redact(v[i])
		}
	}
	return value
}
