package uploads

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	_ "image/jpeg" // Dekoder untuk memeriksa dimensi gambar.
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxSize       = 5 << 20
	intentTTL     = 15 * time.Minute
	pendingLimit  = 20
	pendingWindow = time.Hour
	maxDimension  = 8000
)

var (
	errNotFound  = errors.New("upload not found")
	errTooLarge  = errors.New("upload exceeds declared size")
	errLimited   = errors.New("too many pending uploads")
	errBadToken  = errors.New("upload token invalid or expired")
	errNoContent = errors.New("upload has no content yet")
)

// rejected membawa alasan file ditolak saat diselesaikan.
type rejected struct{ reason string }

func (r rejected) Error() string { return "upload rejected: " + r.reason }

var allowedTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

type Store struct {
	DB      *pgxpool.Pool
	Storage LocalStorage
	// Key menandatangani URL unggah. Diturunkan dari rahasia server, tidak pernah dikirim ke klien.
	Key []byte
	// Origin adalah alamat app; URL unggah melewati proxy same-origin app.
	Origin string
}

// SigningKey menurunkan kunci unggah dari rahasia server dengan pemisah domain.
func SigningKey(secret string) []byte {
	sum := sha256.Sum256([]byte("tenantku-uploads:" + secret))
	return sum[:]
}

func (s Store) Ready(ctx context.Context) error {
	if _, err := s.DB.Exec(ctx, `SELECT 1 FROM uploads LIMIT 0`); err != nil {
		return err
	}
	return os.MkdirAll(s.Storage.Dir, 0o750)
}

func (s Store) sign(id string, expires int64) string {
	mac := hmac.New(sha256.New, s.Key)
	mac.Write([]byte(id + "|" + strconv.FormatInt(expires, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s Store) verify(id, expires, signature string, now time.Time) bool {
	value, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || now.Unix() > value {
		return false
	}
	return hmac.Equal([]byte(signature), []byte(s.sign(id, value)))
}

type Intent struct {
	UploadID  string            `json:"upload_id"`
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type presignInput struct {
	Purpose     string `json:"purpose"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	OwnerID     string `json:"owner_id"`
}

// ownerFor: foto produk milik usaha (hanya pemilik), banner milik organisasi (anggota mana pun).
func ownerFor(purpose string) (ownerType, query string) {
	switch purpose {
	case "product_photo":
		return "business", `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = $1 AND business_id = $2 AND role = 'owner')`
	case "event_banner":
		return "organization", `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = $1 AND organization_id = $2)`
	default:
		return "", ""
	}
}

func (s Store) presign(ctx context.Context, user string, input presignInput) (Intent, error) {
	var intent Intent
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		ownerType, query := ownerFor(input.Purpose)
		var allowed bool
		if err := tx.QueryRow(ctx, query, user, input.OwnerID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return errNotFound
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE created_by = $1 AND status = 'pending'
			AND created_at > clock_timestamp() - make_interval(secs => $2)`, user, pendingWindow.Seconds()).Scan(&pending); err != nil {
			return err
		}
		if pending >= pendingLimit {
			return errLimited
		}
		var id string
		var expires time.Time
		if err := tx.QueryRow(ctx, `INSERT INTO uploads (id, purpose, owner_type, owner_id, content_type, size_bytes, storage_key,
			created_by, expires_at)
			SELECT g.id, $1, $2, $3, $4, $5, $1 || '/' || g.id::text, $6, clock_timestamp() + make_interval(secs => $7)
			FROM (SELECT gen_random_uuid() AS id) g RETURNING id::text, expires_at`,
			input.Purpose, ownerType, input.OwnerID, input.ContentType, input.SizeBytes, user, intentTTL.Seconds()).
			Scan(&id, &expires); err != nil {
			return err
		}
		unix := expires.Unix()
		intent = Intent{
			UploadID: id, Method: http.MethodPut, ExpiresAt: expires,
			URL:     s.Origin + "/api/v1/uploads/" + id + "/content?expires=" + strconv.FormatInt(unix, 10) + "&signature=" + s.sign(id, unix),
			Headers: map[string]string{"Content-Type": input.ContentType},
		}
		return nil
	})
	return intent, err
}

type pendingUpload struct {
	key, contentType string
	size             int64
}

// receive menyimpan isi unggahan yang masih pending. Unggah ulang sebelum diselesaikan menimpa isi lama.
func (s Store) receive(ctx context.Context, id, contentType string, body io.Reader) error {
	var upload pendingUpload
	err := s.DB.QueryRow(ctx, `SELECT storage_key, content_type, size_bytes FROM uploads
		WHERE id = $1 AND status = 'pending' AND expires_at > clock_timestamp()`, id).
		Scan(&upload.key, &upload.contentType, &upload.size)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if contentType != upload.contentType {
		return rejected{reason: "Tipe file tidak sesuai izin unggah."}
	}
	written, err := s.Storage.Write(upload.key, body, upload.size)
	if errors.Is(err, errTooLarge) {
		return errTooLarge
	}
	if err != nil {
		return err
	}
	if written != upload.size {
		_ = s.Storage.Remove(upload.key)
		return rejected{reason: "Ukuran file tidak sesuai izin unggah."}
	}
	return nil
}

type Status struct {
	UploadID string  `json:"upload_id"`
	Status   string  `json:"status"`
	Reason   *string `json:"reason,omitempty"`
}

// inspect memeriksa isi file: tanda tangan biner harus cocok dengan tipe yang diizinkan dan dimensi gambar wajar.
func inspect(head []byte, file io.ReadSeeker, contentType string) string {
	if detected := http.DetectContentType(head); detected != contentType {
		return "Isi file bukan gambar yang diizinkan."
	}
	if contentType == "image/webp" {
		return ""
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "File tidak dapat dibaca."
	}
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return "Gambar rusak atau tidak dapat dibaca."
	}
	if config.Width < 1 || config.Height < 1 || config.Width > maxDimension || config.Height > maxDimension {
		return "Ukuran gambar paling besar 8000 × 8000 piksel."
	}
	return ""
}

// complete memverifikasi file yang sudah diunggah. File tidak lolos ditolak dan dihapus dari penyimpanan.
func (s Store) complete(ctx context.Context, user, id string) (Status, error) {
	result := Status{UploadID: id}
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var key, contentType, status string
		var size int64
		err := tx.QueryRow(ctx, `SELECT storage_key, content_type, size_bytes, status FROM uploads
			WHERE id = $1 AND created_by = $2 FOR UPDATE`, id, user).Scan(&key, &contentType, &size, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if status != "pending" {
			result.Status = status
			return nil
		}
		reason := ""
		file, err := s.Storage.Open(key)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return errNoContent
		case err != nil:
			return err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		head := make([]byte, 512)
		n, _ := io.ReadFull(file, head)
		if info.Size() != size {
			reason = "Ukuran file tidak sesuai izin unggah."
		} else {
			reason = inspect(bytes.Clone(head[:n]), file, contentType)
		}
		if reason != "" {
			_ = file.Close()
			if err := s.Storage.Remove(key); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'rejected', reject_reason = $2, completed_at = clock_timestamp()
				WHERE id = $1`, id, reason); err != nil {
				return err
			}
			result.Status, result.Reason = "rejected", &reason
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'ready', completed_at = clock_timestamp() WHERE id = $1`, id); err != nil {
			return err
		}
		result.Status = "ready"
		return nil
	})
	return result, err
}

type readyUpload struct {
	key, contentType string
	size             int64
}

// readable mengembalikan unggahan ready yang boleh dibaca akun: anggota usaha atau organisasi pemiliknya.
func (s Store) readable(ctx context.Context, user, id string) (readyUpload, error) {
	var value readyUpload
	err := s.DB.QueryRow(ctx, `SELECT u.storage_key, u.content_type, u.size_bytes FROM uploads u
		WHERE u.id = $1 AND u.status = 'ready' AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $2
			AND CASE u.owner_type WHEN 'business' THEN m.business_id = u.owner_id ELSE m.organization_id = u.owner_id END)`,
		id, user).Scan(&value.key, &value.contentType, &value.size)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, errNotFound
	}
	return value, err
}

// public mengembalikan banner acara yang sudah ready; tujuan lain tidak pernah publik.
func (s Store) public(ctx context.Context, id string) (readyUpload, error) {
	var value readyUpload
	err := s.DB.QueryRow(ctx, `SELECT storage_key, content_type, size_bytes FROM uploads
		WHERE id = $1 AND status = 'ready' AND purpose = 'event_banner'`, id).Scan(&value.key, &value.contentType, &value.size)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, errNotFound
	}
	return value, err
}
