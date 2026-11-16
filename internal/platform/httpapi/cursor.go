package httpapi

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

func EncodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.Format(time.RFC3339Nano) + "/" + id))
}

func DecodeCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	if len(cursor) > 120 {
		return time.Time{}, "", errors.New("cursor is too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	date, id, ok := strings.Cut(string(raw), "/")
	if !ok || !ValidID(id) {
		return time.Time{}, "", errors.New("invalid cursor ID")
	}
	at, err := time.Parse(time.RFC3339Nano, date)
	return at, id, err
}
