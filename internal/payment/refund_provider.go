package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type providerRefund struct {
	Key             string `json:"refund_key"`
	Amount          string `json:"refund_amount"`
	BankConfirmedAt string `json:"bank_confirmed_at"`
}

// requestRefund submits a partial refund with the caller's stable reference.
// An accepted request must be reconciled with the provider's refund details.
func (m *Midtrans) requestRefund(ctx context.Context, orderID, key string, amount int64) error {
	if orderID == "" || key == "" || amount < 1 {
		return fmt.Errorf("%w: invalid refund instruction", ErrProvider)
	}
	payload, err := json.Marshal(struct {
		Key    string `json:"refund_key"`
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
	}{Key: key, Amount: amount, Reason: "Acara dibatalkan penyelenggara"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.StatusURL+"/"+url.PathEscape(orderID)+"/refund", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.SetBasicAuth(m.ServerKey, "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := m.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: refund request: %v", ErrProvider, err)
	}
	defer res.Body.Close()
	var reply struct {
		Code    string `json:"status_code"`
		OrderID string `json:"order_id"`
		Key     string `json:"refund_key"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&reply); err != nil ||
		res.StatusCode != http.StatusOK || reply.Code != "200" || reply.OrderID != orderID || reply.Key != key {
		return fmt.Errorf("%w: refund response HTTP %d, status code %s", ErrProvider, res.StatusCode, reply.Code)
	}
	return nil
}

func (n Notification) confirmsRefund(key string, amount int64) bool {
	if n.StatusCode != "200" || (n.TransactionStatus != "partial_refund" && n.TransactionStatus != "refund") {
		return false
	}
	for _, refund := range n.Refunds {
		if refunded, ok := wholeRupiah(refund.Amount); ok && refund.Key == key &&
			refunded == amount && refund.BankConfirmedAt != "" {
			return true
		}
	}
	return false
}
