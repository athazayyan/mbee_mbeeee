package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// One account has one organizer profile, like one business.
func TestOneOrganizationPerAccount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "identity_org")
	s := Store{DB: pool}
	user := testdb.User(t, ctx, pool, "eo")
	if _, err := s.createOrganization(ctx, user, "Penyelenggara Satu"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createOrganization(ctx, user, "Penyelenggara Dua"); !errors.Is(err, errOrganizationAlreadyExists) {
		t.Fatalf("second organization: %v", err)
	}
	other := testdb.User(t, ctx, pool, "eo2")
	if _, err := s.createOrganization(ctx, other, "Penyelenggara Lain"); err != nil {
		t.Fatalf("another account: %v", err)
	}
}
