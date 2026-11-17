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
	sandboxSnapURL   = "https://app.sandbox.midtrans.com/snap/v1/transactions"
	liveSnapURL      = "https://app.midtrans.com/snap/v1/transactions"
	sandboxStatusURL = "https://api.sandbox.midtrans.com/v2"
	liveStatusURL    = "https://api.midtrans.com/v2"
)

var ErrProvider = errors.New("payment provider request failed")

// Midtrans creates Snap transactions and verifies notification signatures.
type Midtrans struct {
	ServerKey string
	SnapURL   string
	StatusURL string
	HTTP      *http.Client
	// ClientKey is only needed by EnabledChannels, the sandbox capability check.
	ClientKey string
}

// NewMidtrans returns nil when serverKey is empty so callers can leave payments disabled.
func NewMidtrans(serverKey, env string) *Midtrans {
	if serverKey == "" {
		return nil
	}
	url, statusURL := sandboxSnapURL, sandboxStatusURL
	if env == "production" {
		url, statusURL = liveSnapURL, liveStatusURL
	}
	return &Midtrans{ServerKey: serverKey, SnapURL: url, StatusURL: statusURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

type snapRequest struct {
	TransactionDetails struct {
		OrderID     string `json:"order_id"`
		GrossAmount int64  `json:"gross_amount"`
	} `json:"transaction_details"`
	CustomerDetails struct {
		Email string `json:"email,omitempty"`
	} `json:"customer_details"`
	ItemDetails     []snapItem `json:"item_details"`
	EnabledPayments []string   `json:"enabled_payments,omitempty"`
	Callbacks       *struct {
		Finish string `json:"finish"`
	} `json:"callbacks,omitempty"`
	Expiry struct {
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

// SnapInput describes one Snap transaction. Channels limits the hosted page to the chosen method;
// FinishURL is where Midtrans sends the browser after the payer finishes.
type SnapInput struct {
	OrderID   string
	Amount    int64
	Email     string
	Items     []snapItem
	Expiry    time.Duration
	Channels  []string
	FinishURL string
}

// CreateSnap opens a Snap transaction. Expiry is rounded up to whole minutes, minimum one.
func (m *Midtrans) CreateSnap(ctx context.Context, in SnapInput) (SnapResult, error) {
	var body snapRequest
	body.TransactionDetails.OrderID = in.OrderID
	body.TransactionDetails.GrossAmount = in.Amount
	body.CustomerDetails.Email = in.Email
	body.ItemDetails = in.Items
	body.EnabledPayments = in.Channels
	if in.FinishURL != "" {
		body.Callbacks = &struct {
			Finish string `json:"finish"`
		}{Finish: in.FinishURL}
	}
	body.Expiry.Unit = "minute"
	body.Expiry.Duration = max(1, int((in.Expiry+time.Minute-1)/time.Minute))
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
		return SnapResult{}, fmt.Errorf("%w: status %d: %s", ErrProvider, res.StatusCode, providerMessage(raw))
	}
	var result SnapResult
	if json.Unmarshal(raw, &result) != nil || result.Token == "" || result.RedirectURL == "" {
		return SnapResult{}, fmt.Errorf("%w: unexpected response", ErrProvider)
	}
	return result, nil
}

// providerMessage extracts Midtrans error_messages for server logs and the sandbox check.
func providerMessage(raw []byte) string {
	var body struct {
		ErrorMessages []string `json:"error_messages"`
		StatusMessage string   `json:"status_message"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "unreadable response"
	}
	text := strings.Join(body.ErrorMessages, "; ")
	if text == "" {
		text = body.StatusMessage
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return text
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
	// PaymentType is the method the payer actually used, for example bank_transfer, qris, or gopay.
	PaymentType string `json:"payment_type,omitempty"`
	VANumbers   []struct {
		Bank string `json:"bank"`
	} `json:"va_numbers,omitempty"`
	PermataVANumber string           `json:"permata_va_number,omitempty"`
	Acquirer        string           `json:"acquirer,omitempty"`
	Refunds         []providerRefund `json:"refunds,omitempty"`
}

// Detail names the bank or acquirer behind PaymentType when Midtrans reports it.
func (n Notification) Detail() string {
	switch {
	case len(n.VANumbers) > 0:
		return n.VANumbers[0].Bank
	case n.PermataVANumber != "":
		return "permata"
	}
	return n.Acquirer
}

// Status verifies the transaction state with Midtrans after a signed notification.
func (m *Midtrans) Status(ctx context.Context, orderID string) (Notification, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.StatusURL+"/"+orderID+"/status", nil)
	if err != nil {
		return Notification{}, ErrProvider
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(m.ServerKey, "")
	res, err := m.HTTP.Do(req)
	if err != nil {
		return Notification{}, ErrProvider
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Notification{}, fmt.Errorf("%w: status lookup returned HTTP %d", ErrProvider, res.StatusCode)
	}
	var n Notification
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&n); err != nil ||
		n.OrderID != orderID || n.TransactionStatus == "" {
		return Notification{}, ErrProvider
	}
	if outcome, _ := n.outcome(); outcome == "paid" &&
		(n.StatusCode != "200" || n.Currency != "IDR" || (n.FraudStatus != "" && n.FraudStatus != "accept")) {
		return Notification{}, ErrProvider
	}
	n.SignatureKey = ""
	return n, nil
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

// ExpireResult is what Midtrans did with an expire request.
type ExpireResult int

const (
	// Expired means the provider closed the transaction.
	Expired ExpireResult = iota
	// Absent means the provider has no transaction yet, which is normal until the payer picks a method.
	Absent
	// Refused means the provider could not expire it: it is already settled, or already closed (check Status).
	Refused
)

// Expire closes an unpaid transaction so a new attempt can replace it.
func (m *Midtrans) Expire(ctx context.Context, orderID string) (ExpireResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.StatusURL+"/"+orderID+"/expire", nil)
	if err != nil {
		return Refused, ErrProvider
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(m.ServerKey, "")
	res, err := m.HTTP.Do(req)
	if err != nil {
		return Refused, ErrProvider
	}
	defer res.Body.Close()
	var body struct {
		StatusCode        string `json:"status_code"`
		TransactionStatus string `json:"transaction_status"`
	}
	// Midtrans reports many failures inside a 200 response, so the body's status_code decides.
	if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body) != nil {
		return Refused, ErrProvider
	}
	switch {
	// Sandbox answers 407 ("expired") when it closes a pending transaction; 200 is accepted too.
	case (body.StatusCode == "200" || body.StatusCode == "407") && body.TransactionStatus == "expire":
		return Expired, nil
	case body.StatusCode == "404" || res.StatusCode == http.StatusNotFound:
		return Absent, nil
	case body.StatusCode == "412":
		return Refused, nil
	}
	return Refused, fmt.Errorf("%w: expire returned %q", ErrProvider, body.StatusCode)
}

// EnabledChannels returns the channels Midtrans will actually offer on a Snap token's payment page.
// Creating a token accepts any channel name, so this is the only way to learn whether a channel is
// activated for the merchant account. It uses the same endpoint Snap's own page loads.
func (m *Midtrans) EnabledChannels(ctx context.Context, token string) ([]string, error) {
	if m.ClientKey == "" {
		return nil, errors.New("MIDTRANS_CLIENT_KEY is required to read enabled channels")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.SnapURL+"/"+token, nil)
	if err != nil {
		return nil, ErrProvider
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(m.ClientKey, "")
	res, err := m.HTTP.Do(req)
	if err != nil {
		return nil, ErrProvider
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: token lookup returned HTTP %d", ErrProvider, res.StatusCode)
	}
	var body struct {
		Enabled []json.RawMessage `json:"enabled_payments"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&body); err != nil {
		return nil, ErrProvider
	}
	var channels []string
	for _, raw := range body.Enabled {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			channels = append(channels, name)
			continue
		}
		var object struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &object) == nil && object.Type != "" {
			channels = append(channels, object.Type)
		}
	}
	return channels, nil
}
