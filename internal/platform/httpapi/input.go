package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

func ReadJSON[T any](c *gin.Context, maxBytes int64) (T, bool) {
	var input *T
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input == nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		var zero T
		Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Isi request tidak valid.", nil)
		return zero, false
	}
	return *input, true
}

func ValidID(id string) bool {
	var uuid pgtype.UUID
	return len(id) == 36 && id[8] == '-' && id[13] == '-' && id[18] == '-' && id[23] == '-' &&
		uuid.Scan(id) == nil
}

func ValidSingleLine(value string, maxRunes int) bool {
	if value == "" || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}
