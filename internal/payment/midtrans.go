// Package payment integrates Midtrans Snap (sandbox first) with bookings and the ledger.
package payment

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	sandboxSnapURL = "https://app.sandbox.midtrans.com/snap/v1/transactions"
	liveSnapURL    = "https://app.midtrans.com/snap/v1/transactions"
)

var ErrProvider = errors.New("payment provider request failed")

// Midtrans creates Snap transactions and verifies notification signatures.
type Midtrans struct {
	ServerKey string
	SnapURL   string
	HTTP      *http.Client
}

// NewMidtrans returns nil when serverKey is empty so callers can leave payments disabled.
func NewMidtrans(serverKey, env string) *Midtrans {
	if serverKey == "" {
		return nil
	}
	url := sandboxSnapURL
	if env == "production" {
		url = liveSnapURL
	}
	return &Midtrans{ServerKey: serverKey, SnapURL: url, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

type snapRequest struct {
	TransactionDetails struct {
		OrderID     string `json:"order_id"`
		GrossAmount int64  `json:"gross_amount"`
	} `json:"transaction_details"`
	CustomerDetails struct {
		Email string `json:"email,omitempty"`
	} `json:"customer_details"`
	ItemDetails []snapItem `json:"item_details"`
	Expiry      struct {
		Unit     string `json:"unit"`
		Duration int    `json:"duration"`
	} `json:"expiry"`
}

type snapItem struct {
	ID       string `json:"id"`
	Price    int64  `json:"price"`
	Quantity int    `json:"quantity"`
	Name     string `json:"name"`
}

type SnapResult struct {
	Token       string `json:"token"`
	RedirectURL string `json:"redirect_url"`
}

// CreateSnap opens a Snap transaction. expiry is rounded up to whole minutes, minimum one.
func (m *Midtrans) CreateSnap(ctx context.Context, orderID string, amount int64, email string, items []snapItem, expiry time.Duration) (SnapResult, error) {
	var body snapRequest
	body.TransactionDetails.OrderID = orderID
	body.TransactionDetails.GrossAmount = amount
	body.CustomerDetails.Email = email
	body.ItemDetails = items
	body.Expiry.Unit = "minute"
	body.Expiry.Duration = max(1, int((expiry+time.Minute-1)/time.Minute))
	payload, err := json.Marshal(body)
	if err != nil {
		return SnapResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.SnapURL, bytes.NewReader(payload))
	if err != nil {
		return SnapResult{}, err
	}
	req.SetBasicAuth(m.ServerKey, "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := m.HTTP.Do(req)
	if err != nil {
		return SnapResult{}, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return SnapResult{}, fmt.Errorf("%w: status %d", ErrProvider, res.StatusCode)
	}
	var result SnapResult
	if json.Unmarshal(raw, &result) != nil || result.Token == "" || result.RedirectURL == "" {
		return SnapResult{}, fmt.Errorf("%w: unexpected response", ErrProvider)
	}
	return result, nil
}

// Notification is the subset of the Midtrans HTTP notification this service reads.
type Notification struct {
	OrderID           string `json:"order_id"`
	StatusCode        string `json:"status_code"`
	GrossAmount       string `json:"gross_amount"`
	SignatureKey      string `json:"signature_key"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	TransactionID     string `json:"transaction_id"`
	Currency          string `json:"currency"`
}

// ValidSignature checks SHA512(order_id + status_code + gross_amount + server_key).
func (m *Midtrans) ValidSignature(n Notification) bool {
	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + m.ServerKey))
	want := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(n.SignatureKey)), []byte(want)) == 1
}

// wholeRupiah parses gross_amount such as "150000.00". Fractions other than zero are rejected.
func wholeRupiah(value string) (int64, bool) {
	whole, frac, _ := strings.Cut(value, ".")
	if strings.Trim(frac, "0") != "" || whole == "" || len(whole) > 16 {
		return 0, false
	}
	var n int64
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// outcome maps a Midtrans transaction_status to an attempt status.
func (n Notification) outcome() (status string, ok bool) {
	switch n.TransactionStatus {
	case "settlement":
		return "paid", true
	case "capture":
		if n.FraudStatus == "" || n.FraudStatus == "accept" {
			return "paid", true
		}
		return "pending", true // challenge: wait for a later decision
	case "pending":
		return "pending", true
	case "deny", "cancel", "failure":
		return "failed", true
	case "expire":
		return "expired", true
	}
	return "", false
}
