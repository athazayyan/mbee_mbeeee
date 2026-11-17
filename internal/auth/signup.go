package auth

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// Signup is the optional profile asked for at registration. A tenant names their business and an
// organizer names their organization and first event. The accounts it creates cannot sign in
// until the email is verified, so everything is stored in the same transaction as the account.
type Signup struct {
	Role             string `json:"role"`
	Phone            string `json:"phone"`
	BusinessName     string `json:"business_name"`
	CategoryID       string `json:"category_id"`
	OrganizationName string `json:"organization_name"`
	EventName        string `json:"event_name"`
}

var mobilePhone = regexp.MustCompile(`^(?:\+62|62|0)(8[0-9]{8,12})$`)

func name(value *string, limit int, fields map[string]string, field, message string) {
	*value = strings.Join(strings.Fields(*value), " ")
	if count := utf8.RuneCountInString(*value); count < 1 || count > limit {
		fields[field] = message
	}
}

// normalize trims the profile in place and returns field errors keyed like the form.
func (s *Signup) normalize() map[string]string {
	fields := make(map[string]string)
	digits := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(s.Phone)
	if match := mobilePhone.FindStringSubmatch(digits); match != nil {
		s.Phone = "+62" + match[1]
	} else {
		fields["phone"] = "Masukkan nomor HP Indonesia, misalnya 0812 3456 7890."
	}
	switch s.Role {
	case "tenant":
		name(&s.BusinessName, 120, fields, "business_name", "Masukkan nama usaha 1–120 karakter.")
		s.CategoryID = strings.ToLower(s.CategoryID)
		if !httpapi.ValidID(s.CategoryID) {
			fields["category_id"] = "Pilih kategori usaha."
		}
		s.OrganizationName, s.EventName = "", ""
	case "organizer":
		name(&s.OrganizationName, 120, fields, "organization_name", "Masukkan nama penyelenggara 1–120 karakter.")
		name(&s.EventName, 160, fields, "event_name", "Masukkan nama acara 1–160 karakter.")
		s.BusinessName, s.CategoryID = "", ""
	default:
		fields["role"] = "Pilih daftar sebagai tenant atau penyelenggara."
	}
	return fields
}

func (s Signup) store(ctx context.Context, tx pgx.Tx, userID string) error {
	var eventName *string
	if s.Role == "organizer" {
		eventName = &s.EventName
	}
	if _, err := tx.Exec(ctx, `INSERT INTO signup_profiles (user_id, role, phone, event_name) VALUES ($1, $2, $3, $4)`,
		userID, s.Role, s.Phone, eventName); err != nil {
		return err
	}
	if s.Role == "tenant" {
		_, err := tx.Exec(ctx, `WITH created AS (
				INSERT INTO businesses (owner_id, name, category_id) VALUES ($1, $2, $3) RETURNING id
			) INSERT INTO memberships (user_id, business_id, role) SELECT $1, id, 'owner' FROM created`,
			userID, s.BusinessName, s.CategoryID)
		return err
	}
	_, err := tx.Exec(ctx, `WITH created AS (
			INSERT INTO organizations (owner_id, name) VALUES ($1, $2) RETURNING id
		) INSERT INTO memberships (user_id, organization_id, role) SELECT $1, id, 'owner' FROM created`,
		userID, s.OrganizationName)
	return err
}
