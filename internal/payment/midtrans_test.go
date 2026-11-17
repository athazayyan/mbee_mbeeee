package payment

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	res, err := m.CreateSnap(context.Background(), SnapInput{OrderID: "o-1", Amount: 1000, Email: "a@b.c", Expiry: 90 * time.Second})
	if err != nil || res.Token != "tok" {
		t.Fatalf("%v %+v", err, res)
	}
	m.ServerKey = "wrong"
	if _, err := m.CreateSnap(context.Background(), SnapInput{OrderID: "o-1", Amount: 1000, Expiry: time.Minute}); err == nil {
		t.Fatal("expected provider error")
	}
}

func TestExpire(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want ExpireResult
		err  bool
	}{
		"closed":       {`{"status_code":"200","transaction_status":"expire"}`, Expired, false},
		"sandbox 407":  {`{"status_code":"407","status_message":"Success, transaction is expired","transaction_status":"expire"}`, Expired, false},
		"not created":  {`{"status_code":"404","status_message":"Transaction doesn't exist."}`, Absent, false},
		"settled":      {`{"status_code":"412","status_message":"Transaction status cannot be updated."}`, Refused, false},
		"unexpected":   {`{"status_code":"500"}`, Refused, true},
		"unreadable":   {`<html>`, Refused, true},
		"wrong status": {`{"status_code":"200","transaction_status":"settlement"}`, Refused, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/order-1/expire" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		m := NewMidtrans(testKey, "sandbox")
		m.StatusURL = srv.URL
		got, err := m.Expire(context.Background(), "order-1")
		srv.Close()
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: got %v %v", name, got, err)
		}
	}
}

func TestCreateSnapSendsChannelsAndFinishURL(t *testing.T) {
	var got snapRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"t","redirect_url":"https://pay.example/t"}`))
	}))
	defer srv.Close()
	m := NewMidtrans(testKey, "sandbox")
	m.SnapURL = srv.URL
	_, err := m.CreateSnap(context.Background(), SnapInput{
		OrderID: "o", Amount: 5, Expiry: time.Minute, Channels: []string{"gopay"}, FinishURL: "https://app.example.com/done",
	})
	if err != nil || len(got.EnabledPayments) != 1 || got.EnabledPayments[0] != "gopay" ||
		got.Callbacks == nil || got.Callbacks.Finish != "https://app.example.com/done" {
		t.Fatalf("%v %+v", err, got)
	}
	// Without a finish URL no callbacks object is sent.
	got = snapRequest{}
	if _, err := m.CreateSnap(context.Background(), SnapInput{OrderID: "o", Amount: 5, Expiry: time.Minute}); err != nil || got.Callbacks != nil {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestProviderMessage(t *testing.T) {
	if got := providerMessage([]byte(`{"error_messages":["Payment channel is not activated.","x"]}`)); got != "Payment channel is not activated.; x" {
		t.Fatal(got)
	}
	if providerMessage([]byte(`not json`)) == "" {
		t.Fatal("unreadable body should still explain itself")
	}
}

func TestEnabledChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); !ok || user != "Mid-client-test" || r.URL.Path != "/tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"enabled_payments":["gopay",{"type":"bca_va"},"other_qris"]}`))
	}))
	defer srv.Close()
	m := NewMidtrans(testKey, "sandbox")
	m.SnapURL = srv.URL
	if _, err := m.EnabledChannels(context.Background(), "tok"); err == nil {
		t.Fatal("a missing client key must be reported, not silently pass")
	}
	m.ClientKey = "Mid-client-test"
	got, err := m.EnabledChannels(context.Background(), "tok")
	if err != nil || strings.Join(got, ",") != "gopay,bca_va,other_qris" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := m.EnabledChannels(context.Background(), "other"); err == nil {
		t.Fatal("expected provider error")
	}
}
