// Package admin menyediakan panel admin platform minimum (E16): verifikasi penyelenggara,
// daftar pengguna, log audit, dan katalog paket acara.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
)

// Rechecker asks the payment provider for the real status of an attempt and applies it exactly like a webhook.
type Rechecker interface {
	Recheck(ctx context.Context, attemptID string) error
}

type Store struct {
	DB       *pgxpool.Pool
	Payments Rechecker
}

var errForbidden = errors.New("platform admin required")

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT permissions FROM platform_admins LIMIT 0`)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `SELECT 1 FROM admin_audit, plan_price_versions, plan_prices, payment_methods LIMIT 0`)
	return err
}

// IsAdmin dipakai juga oleh GET /me untuk menentukan menu admin.
func (s Store) IsAdmin(ctx context.Context, userID string) (bool, error) {
	permissions, err := adminperm.Of(ctx, s.DB, userID)
	return len(permissions) > 0, err
}

type Overview struct {
	Users                 int64 `json:"users"`
	UsersVerified         int64 `json:"users_verified"`
	Businesses            int64 `json:"businesses"`
	Organizations         int64 `json:"organizations"`
	OrganizationsPending  int64 `json:"organizations_unverified"`
	EventsDraft           int64 `json:"events_draft"`
	EventsPublished       int64 `json:"events_published"`
	BookingsActive        int64 `json:"bookings_active"`
	BookingsConfirmed     int64 `json:"bookings_confirmed"`
	SalesLast7Days        int64 `json:"sales_last_7_days"`
	SalesRevenueLast7Days int64 `json:"sales_revenue_last_7_days"`
}

func (s Store) overview(ctx context.Context) (Overview, error) {
	var o Overview
	err := s.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM users),
		(SELECT count(*) FROM users WHERE email_verified_at IS NOT NULL),
		(SELECT count(*) FROM businesses),
		(SELECT count(*) FROM organizations),
		(SELECT count(*) FROM organizations WHERE verification_status = 'unverified'),
		(SELECT count(*) FROM events WHERE status = 'draft'),
		(SELECT count(*) FROM events WHERE status IN ('published', 'live')),
		(SELECT count(*) FROM bookings WHERE status IN ('held', 'payment_pending')),
		(SELECT count(*) FROM bookings WHERE status = 'confirmed'),
		(SELECT count(*) FROM sales WHERE client_created_at >= now() - interval '7 days'),
		(SELECT COALESCE(sum(total), 0) FROM sales WHERE client_created_at >= now() - interval '7 days')`).
		Scan(&o.Users, &o.UsersVerified, &o.Businesses, &o.Organizations, &o.OrganizationsPending, &o.EventsDraft,
			&o.EventsPublished, &o.BookingsActive, &o.BookingsConfirmed, &o.SalesLast7Days, &o.SalesRevenueLast7Days)
	return o, err
}

type Organization struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	VerificationStatus string     `json:"verification_status"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	OwnerEmail         string     `json:"owner_email"`
	EventsTotal        int64      `json:"events_total"`
	EventsPublished    int64      `json:"events_published"`
}

type pageFilter struct {
	Query        string
	Status       string
	ResourceType string
	ResourceID   string
	AfterAt      time.Time
	AfterID      string
	Limit        int
}

const organizationColumns = `o.id::text, o.name, o.verification_status, o.verified_at, o.created_at, u.email,
	(SELECT count(*) FROM events e WHERE e.organization_id = o.id),
	(SELECT count(*) FROM events e WHERE e.organization_id = o.id AND e.status IN ('published', 'live'))`

func scanOrganization(row pgx.Row) (Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.VerificationStatus, &o.VerifiedAt, &o.CreatedAt, &o.OwnerEmail,
		&o.EventsTotal, &o.EventsPublished)
	return o, err
}

func (s Store) organizations(ctx context.Context, filter pageFilter) ([]Organization, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+organizationColumns+` FROM organizations o JOIN users u ON u.id = o.owner_id
		WHERE ($1 = '' OR o.verification_status = $1)
		AND ($2 = '' OR strpos(lower(o.name), lower($2)) > 0 OR strpos(u.email, lower($2)) > 0)
		AND ($3::uuid IS NULL OR (o.created_at, o.id) < ($4, $3::uuid))
		ORDER BY o.created_at DESC, o.id DESC LIMIT $5`,
		filter.Status, filter.Query, nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Organization, error) { return scanOrganization(row) })
}

type auditRecord struct {
	actor, action, resourceType, resourceID, reason, requestID string
	before, after                                              any
}

// writeAudit appends one row to the audit log. Secrets in the snapshots are redacted before storing.
func writeAudit(ctx context.Context, tx pgx.Tx, record auditRecord) error {
	before, err := adminperm.Redacted(record.before)
	if err != nil {
		return err
	}
	after, err := adminperm.Redacted(record.after)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit (actor_id, action, resource_type, resource_id, reason, before, after, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, record.actor, record.action, record.resourceType, nilIfEmpty(record.resourceID),
		record.reason, before, after, record.requestID)
	return err
}

// setVerification mengubah status verifikasi penyelenggara dan mencatat audit dalam satu transaksi.
func (s Store) setVerification(ctx context.Context, actor, id, status, reason, requestID string) (Organization, error) {
	var result Organization
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor, adminperm.OrganizationsVerify); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRow(ctx, `SELECT verification_status FROM organizations WHERE id = $1 FOR UPDATE`, id).
			Scan(&current); err != nil {
			return err
		}
		if current != status {
			if _, err := tx.Exec(ctx, `UPDATE organizations SET verification_status = $2,
				verified_at = CASE WHEN $2 = 'verified' THEN clock_timestamp() END WHERE id = $1`, id, status); err != nil {
				return err
			}
			action := "organization.verify"
			if status == "unverified" {
				action = "organization.unverify"
			}
			if err := writeAudit(ctx, tx, auditRecord{
				actor: actor, action: action, resourceType: "organization", resourceID: id, reason: reason,
				requestID: requestID, before: map[string]string{"verification_status": current},
				after: map[string]string{"verification_status": status},
			}); err != nil {
				return err
			}
		}
		var err error
		result, err = scanOrganization(tx.QueryRow(ctx, `SELECT `+organizationColumns+`
			FROM organizations o JOIN users u ON u.id = o.owner_id WHERE o.id = $1`, id))
		return err
	})
	return result, err
}

// requireAdmin checks the permission inside the transaction that performs the action.
func requireAdmin(ctx context.Context, tx pgx.Tx, actor, permission string) error {
	allowed, err := adminperm.Has(ctx, tx, actor, permission)
	if err != nil {
		return err
	}
	if !allowed {
		return errForbidden
	}
	return nil
}

type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
	Businesses    int64     `json:"businesses"`
	Organizations int64     `json:"organizations"`
	Admin         bool      `json:"admin"`
}

func (s Store) users(ctx context.Context, filter pageFilter) ([]User, error) {
	rows, err := s.DB.Query(ctx, `SELECT u.id::text, u.email, u.email_verified_at IS NOT NULL, u.created_at,
		(SELECT count(*) FROM memberships m WHERE m.user_id = u.id AND m.business_id IS NOT NULL),
		(SELECT count(*) FROM memberships m WHERE m.user_id = u.id AND m.organization_id IS NOT NULL),
		EXISTS (SELECT 1 FROM platform_admins a WHERE a.user_id = u.id)
		FROM users u
		WHERE ($1 = '' OR strpos(u.email, lower($1)) > 0)
		AND ($2::uuid IS NULL OR (u.created_at, u.id) < ($3, $2::uuid))
		ORDER BY u.created_at DESC, u.id DESC LIMIT $4`,
		filter.Query, nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[User])
}

type AuditEvent struct {
	ID           string          `json:"id"`
	ActorID      string          `json:"actor_id"`
	ActorEmail   string          `json:"actor_email"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *string         `json:"resource_id,omitempty"`
	Reason       string          `json:"reason"`
	Before       json.RawMessage `json:"before,omitempty"`
	After        json.RawMessage `json:"after,omitempty"`
	OccurredAt   time.Time       `json:"occurred_at"`
	RequestID    string          `json:"request_id"`
}

func (s Store) audit(ctx context.Context, filter pageFilter) ([]AuditEvent, error) {
	rows, err := s.DB.Query(ctx, `SELECT a.id::text, a.actor_id::text, u.email, a.action, a.resource_type,
		a.resource_id::text, a.reason, a.before, a.after, a.occurred_at, a.request_id
		FROM admin_audit a JOIN users u ON u.id = a.actor_id
		WHERE ($1 = '' OR a.action = $1)
		AND ($5 = '' OR a.resource_type = $5)
		AND ($6 = '' OR a.resource_id::text = $6)
		AND ($2::uuid IS NULL OR (a.occurred_at, a.id) < ($3, $2::uuid))
		ORDER BY a.occurred_at DESC, a.id DESC LIMIT $4`,
		filter.Status, nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit, filter.ResourceType, filter.ResourceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AuditEvent])
}

// planCatalog membaca versi paket yang berlaku sekarang.
func (s Store) planCatalog(ctx context.Context) (PlanCatalog, error) {
	var catalog PlanCatalog
	err := s.DB.QueryRow(ctx, `SELECT id, effective_at, custom_price_per_tenant FROM plan_price_versions
		WHERE effective_at <= now() ORDER BY effective_at DESC LIMIT 1`).
		Scan(&catalog.VersionID, &catalog.EffectiveAt, &catalog.CustomPricePerTenant)
	if err != nil {
		return catalog, err
	}
	rows, err := s.DB.Query(ctx, `SELECT code, name, capacity, price FROM plan_prices
		WHERE version_id = $1 ORDER BY capacity`, catalog.VersionID)
	if err != nil {
		return catalog, err
	}
	catalog.Plans, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Plan, error) {
		var plan Plan
		err := row.Scan(&plan.Code, &plan.Name, &plan.Capacity, &plan.Price)
		plan.PerTenant = perTenant(plan.Price, plan.Capacity)
		return plan, err
	})
	return catalog, err
}

func nilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
