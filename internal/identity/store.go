package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errBusinessAlreadyExists = errors.New("account already owns a business")

type Store struct {
	DB *pgxpool.Pool
}

type business struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CategoryID string `json:"category_id"`
}

type organization struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	VerificationStatus string `json:"verification_status"`
}

type membership struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
}

type category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type consentEvent struct {
	ID            string    `json:"id"`
	BusinessID    string    `json:"business_id"`
	Purpose       string    `json:"purpose"`
	Granted       bool      `json:"granted"`
	PolicyVersion string    `json:"policy_version"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `
		SELECT 1 FROM memberships, businesses, organizations, business_categories, consent_events, signup_profiles LIMIT 0
	`)
	return err
}

func (s Store) memberships(ctx context.Context, userID string) ([]membership, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT 'business', b.id, b.name, m.role FROM memberships m
		JOIN businesses b ON b.id = m.business_id WHERE m.user_id = $1
		UNION ALL
		SELECT 'organization', o.id, o.name, m.role FROM memberships m
		JOIN organizations o ON o.id = m.organization_id WHERE m.user_id = $1
		ORDER BY 1, 3, 2`, userID)
	return collect(rows, err, pgx.RowToStructByPos[membership])
}

// signup returns the phone and planned first event name given at registration, if any.
func (s Store) signup(ctx context.Context, userID string) (phone, eventName *string, err error) {
	err = s.DB.QueryRow(ctx, `SELECT phone, event_name FROM signup_profiles WHERE user_id = $1`, userID).Scan(&phone, &eventName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return phone, eventName, err
}

func (s Store) isPlatformAdmin(ctx context.Context, userID string) (bool, error) {
	var admin bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_admins WHERE user_id = $1)`, userID).Scan(&admin)
	return admin, err
}

func (s Store) categories(ctx context.Context) ([]category, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, name FROM business_categories ORDER BY name, id`)
	return collect(rows, err, pgx.RowToStructByPos[category])
}

func (s Store) createBusiness(ctx context.Context, userID, name, categoryID string) (business, error) {
	var created business
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// Lock per owner before reading, including for accounts created under the old rule.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended('tenantku.identity.business:' || $1::uuid::text, 0))`, userID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM businesses WHERE owner_id = $1
		)`, userID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errBusinessAlreadyExists
		}
		var err error
		created, err = scanBusiness(tx.QueryRow(ctx, `
			WITH created AS (
				INSERT INTO businesses (owner_id, name, category_id) VALUES ($1, $2, $3)
				RETURNING id, name, category_id
			), joined AS (
				INSERT INTO memberships (user_id, business_id, role)
				SELECT $1, id, 'owner' FROM created RETURNING business_id
			)
			SELECT id, name, category_id FROM created JOIN joined ON business_id = id
		`, userID, name, categoryID))
		return err
	})
	return created, err
}

func (s Store) business(ctx context.Context, userID, id string) (business, error) {
	row := s.DB.QueryRow(ctx, `
		SELECT b.id, b.name, b.category_id FROM businesses b
		JOIN memberships m ON m.business_id = b.id AND m.user_id = $1
		WHERE b.id = $2`, userID, id)
	return scanBusiness(row)
}

func (s Store) updateBusiness(ctx context.Context, userID, id string, name, categoryID *string) (business, error) {
	row := s.DB.QueryRow(ctx, `
		UPDATE businesses b SET name = COALESCE($3, b.name), category_id = COALESCE($4, b.category_id)
		FROM memberships m
		WHERE b.id = $2 AND m.business_id = b.id AND m.user_id = $1 AND m.role = 'owner'
		RETURNING b.id, b.name, b.category_id`, userID, id, name, categoryID)
	return scanBusiness(row)
}

func (s Store) createOrganization(ctx context.Context, userID, name string) (organization, error) {
	row := s.DB.QueryRow(ctx, `
		WITH created AS (
			INSERT INTO organizations (owner_id, name) VALUES ($1, $2)
			RETURNING id, name, verification_status
		), joined AS (
			INSERT INTO memberships (user_id, organization_id, role)
			SELECT $1, id, 'owner' FROM created RETURNING organization_id
		)
		SELECT id, name, verification_status FROM created JOIN joined ON organization_id = id
	`, userID, name)
	return scanOrganization(row)
}

func (s Store) organization(ctx context.Context, userID, id string) (organization, error) {
	row := s.DB.QueryRow(ctx, `
		SELECT o.id, o.name, o.verification_status FROM organizations o
		JOIN memberships m ON m.organization_id = o.id AND m.user_id = $1
		WHERE o.id = $2`, userID, id)
	return scanOrganization(row)
}

func (s Store) updateOrganization(ctx context.Context, userID, id, name string) (organization, error) {
	row := s.DB.QueryRow(ctx, `
		UPDATE organizations o SET name = $3
		FROM memberships m
		WHERE o.id = $2 AND m.organization_id = o.id AND m.user_id = $1 AND m.role = 'owner'
		RETURNING o.id, o.name, o.verification_status`, userID, id, name)
	return scanOrganization(row)
}

func (s Store) consents(ctx context.Context, userID, id string) ([]consentEvent, error) {
	var events []consentEvent
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var owned string
		if err := tx.QueryRow(ctx, `
			SELECT business_id FROM memberships WHERE user_id = $1 AND business_id = $2 AND role = 'owner'
			FOR SHARE`, userID, id).Scan(&owned); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id, business_id, purpose, granted, policy_version, created_at FROM consent_events
			WHERE business_id = $1 ORDER BY created_at DESC, id DESC LIMIT 100`, owned)
		events, err = collect(rows, err, pgx.RowToStructByPos[consentEvent])
		return err
	})
	return events, err
}

func (s Store) recordConsent(ctx context.Context, userID, id, purpose string, granted bool, version string) (consentEvent, error) {
	var event consentEvent
	err := s.DB.QueryRow(ctx, `
		INSERT INTO consent_events (business_id, actor_id, purpose, granted, policy_version)
		SELECT business_id, user_id, $3, $4, $5 FROM memberships
		WHERE user_id = $1 AND business_id = $2 AND role = 'owner'
		FOR SHARE
		RETURNING id, business_id, purpose, granted, policy_version, created_at
	`, userID, id, purpose, granted, version).
		Scan(&event.ID, &event.BusinessID, &event.Purpose, &event.Granted, &event.PolicyVersion, &event.CreatedAt)
	return event, err
}

func scanBusiness(row pgx.Row) (business, error) {
	var value business
	err := row.Scan(&value.ID, &value.Name, &value.CategoryID)
	return value, err
}

func scanOrganization(row pgx.Row) (organization, error) {
	var value organization
	err := row.Scan(&value.ID, &value.Name, &value.VerificationStatus)
	return value, err
}

func collect[T any](rows pgx.Rows, err error, scan pgx.RowToFunc[T]) ([]T, error) {
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(make([]T, 0), rows, scan)
}
