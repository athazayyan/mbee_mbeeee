package reports

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// E14-05 (T19): akses rapor lintas akun lewat ID, token kedaluwarsa/dicabut, dan cakupan data yang dibagikan.
func TestReportSharingIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "reports_test")
	s := Store{DB: pool}
	w := testdb.NewWorld(t, ctx, pool, "confirmed", false)
	stranger := testdb.User(t, ctx, pool, "luar")

	now := time.Now()
	input := reportInput{PeriodStart: now.Add(-24 * time.Hour), PeriodEnd: now, Timezone: "Asia/Jakarta"}
	if _, err := s.create(ctx, stranger, w.Business, input); !errors.Is(err, errNotFound) {
		t.Fatalf("stranger created report: %v", err)
	}
	report, err := s.create(ctx, w.Tenant, w.Business, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.get(ctx, stranger, report.ID); !errors.Is(err, errNotFound) {
		t.Fatalf("stranger read report by id: %v", err)
	}
	if _, err := s.get(ctx, w.Organizer, report.ID); !errors.Is(err, errNotFound) {
		t.Fatalf("organizer read tenant report: %v", err)
	}
	if _, err := s.createShare(ctx, stranger, report.ID, now.Add(time.Hour*2), "http://app"); !errors.Is(err, errNotFound) {
		t.Fatalf("stranger shared report: %v", err)
	}

	share, err := s.createShare(ctx, w.Tenant, report.ID, now.Add(2*time.Hour), "http://app")
	if err != nil {
		t.Fatal(err)
	}
	token := share.URL[strings.LastIndex(share.URL, "/")+1:]
	stored := testdb.Scalar(t, ctx, pool, `SELECT encode(token_hash, 'hex') FROM report_shares WHERE id = $1`, share.ID)
	if strings.Contains(stored, token) || len(token) < 20 {
		t.Fatal("token must be random and stored only as a hash")
	}
	public, err := s.public(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	// Cakupan publik: ringkasan saja, tanpa laba, HPP, atau produk.
	if public.BusinessName != "Kopi Senja" || public.VerificationCode != report.VerificationCode {
		t.Fatalf("public summary mismatch: %+v", public)
	}
	if _, err := s.public(ctx, token+"x"); !errors.Is(err, errNotFound) {
		t.Fatalf("tampered token accepted: %v", err)
	}

	if err := s.revokeShare(ctx, stranger, share.ID); !errors.Is(err, errNotFound) {
		t.Fatalf("stranger revoked share: %v", err)
	}
	if err := s.revokeShare(ctx, w.Tenant, share.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.public(ctx, token); !errors.Is(err, errNotFound) {
		t.Fatalf("revoked token still works: %v", err)
	}

	expiring, err := s.createShare(ctx, w.Tenant, report.ID, now.Add(2*time.Hour), "http://app")
	if err != nil {
		t.Fatal(err)
	}
	testdb.Exec(t, ctx, pool, `UPDATE report_shares SET expires_at = now() - interval '1 second' WHERE id = $1`, expiring.ID)
	if _, err := s.public(ctx, expiring.URL[strings.LastIndex(expiring.URL, "/")+1:]); !errors.Is(err, errNotFound) {
		t.Fatalf("expired token still works: %v", err)
	}
}
