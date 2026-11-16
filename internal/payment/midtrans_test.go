package payment

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sign(n Notification, key string) string {
	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + key))
	return hex.EncodeToString(sum[:])
}

func TestStatus(t *testing.T) {
	orderID := "590c5927-511a-4dc0-9b8d-ec7795d94db4"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != testKey || pass != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/"+orderID+"/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"order_id":"` + orderID + `","transaction_status":"settlement","status_code":"200","gross_amount":"45000.00","currency":"IDR","signature_key":"omitted-from-audit"}`))
	}))
	defer provider.Close()
	m := NewMidtrans(testKey, "sandbox")
	m.StatusURL = provider.URL
	n, err := m.Status(context.Background(), orderID)
	if err != nil || n.OrderID != orderID || n.TransactionStatus != "settlement" || n.SignatureKey != "" {
		t.Fatalf("verified status %+v: %v", n, err)
	}
}

func TestValidSignature(t *testing.T) {
	m := NewMidtrans("Mid-server-test", "sandbox")
	n := Notification{OrderID: "o-1", StatusCode: "200", GrossAmount: "150000.00"}
	n.SignatureKey = sign(n, "Mid-server-test")
	if !m.ValidSignature(n) {
		t.Fatal("expected valid signature")
	}
	n.GrossAmount = "1.00"
	if m.ValidSignature(n) {
		t.Fatal("tampered amount must fail")
	}
	n.SignatureKey = ""
	if m.ValidSignature(n) {
		t.Fatal("empty signature must fail")
	}
}

func TestWholeRupiah(t *testing.T) {
	for in, want := range map[string]int64{"150000.00": 150000, "150000": 150000, "7.0": 7} {
		if got, ok := wholeRupiah(in); !ok || got != want {
			t.Errorf("%q = %d,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", "1.5", "-3", "abc", ".00", "1e5"} {
		if _, ok := wholeRupiah(in); ok {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestOutcome(t *testing.T) {
	cases := []struct{ status, fraud, want string }{
		{"settlement", "", "paid"}, {"capture", "accept", "paid"}, {"capture", "challenge", "pending"},
		{"pending", "", "pending"}, {"deny", "", "failed"}, {"cancel", "", "failed"}, {"expire", "", "expired"},
	}
	for _, c := range cases {
		if got, ok := (Notification{TransactionStatus: c.status, FraudStatus: c.fraud}).outcome(); !ok || got != c.want {
			t.Errorf("%s/%s = %q", c.status, c.fraud, got)
		}
	}
	if _, ok := (Notification{TransactionStatus: "refund"}).outcome(); ok {
		t.Error("unknown status must not map")
	}
}

func TestCreateSnap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := r.BasicAuth()
		if !ok || user != "Mid-server-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"tok","redirect_url":"https://pay.example/tok"}`))
	}))
	defer srv.Close()
	m := NewMidtrans("Mid-server-test", "sandbox")
	m.SnapURL = srv.URL
	res, err := m.CreateSnap(context.Background(), "o-1", 1000, "a@b.c", nil, 90*time.Second)
	if err != nil || res.Token != "tok" {
		t.Fatalf("%v %+v", err, res)
	}
	m.ServerKey = "wrong"
	if _, err := m.CreateSnap(context.Background(), "o-1", 1000, "", nil, time.Minute); err == nil {
		t.Fatal("expected provider error")
	}
}
