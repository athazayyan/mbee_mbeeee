package uploads

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSignature(t *testing.T) {
	s := Store{Key: SigningKey("rahasia")}
	now := time.Unix(1_800_000_000, 0)
	expires := now.Add(time.Minute).Unix()
	sig := s.sign("abc", expires)
	if !s.verify("abc", strconv.FormatInt(expires, 10), sig, now) {
		t.Fatal("valid signature rejected")
	}
	if s.verify("abd", strconv.FormatInt(expires, 10), sig, now) {
		t.Fatal("signature reused for another upload")
	}
	if s.verify("abc", strconv.FormatInt(expires, 10), sig, now.Add(2*time.Minute)) {
		t.Fatal("expired signature accepted")
	}
	if s.verify("abc", strconv.FormatInt(expires+60, 10), sig, now) {
		t.Fatal("extended expiry accepted")
	}
	other := Store{Key: SigningKey("lain")}
	if other.verify("abc", strconv.FormatInt(expires, 10), sig, now) {
		t.Fatal("signature from another key accepted")
	}
}

func TestValidatePresign(t *testing.T) {
	owner := "6f1f9d1e-2b7c-4d5e-9a3b-1c2d3e4f5a6b"
	cases := []struct {
		input  presignInput
		status int
		field  string
	}{
		{presignInput{"product_photo", "image/png", 1000, owner}, 0, ""},
		{presignInput{"product_photo", " IMAGE/JPEG ", 1000, owner}, 0, ""},
		{presignInput{"product_photo", "image/svg+xml", 1000, owner}, http.StatusUnsupportedMediaType, "content_type"},
		{presignInput{"product_photo", "text/html", 1000, owner}, http.StatusUnsupportedMediaType, "content_type"},
		{presignInput{"product_photo", "image/png", maxSize + 1, owner}, http.StatusRequestEntityTooLarge, "size_bytes"},
		{presignInput{"product_photo", "image/png", 0, owner}, http.StatusUnprocessableEntity, "size_bytes"},
		{presignInput{"dispute_evidence", "image/png", 10, owner}, http.StatusUnprocessableEntity, "purpose"},
		{presignInput{"avatar", "image/png", 10, owner}, http.StatusUnprocessableEntity, "purpose"},
		{presignInput{"product_photo", "image/png", 10, "bukan-uuid"}, http.StatusUnprocessableEntity, "owner_id"},
	}
	for i, tc := range cases {
		status, fields := validatePresign(&tc.input)
		if status != tc.status || (tc.field != "" && fields[tc.field] == "") {
			t.Errorf("case %d: status %d fields %v", i, status, fields)
		}
	}
}

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 252, G: 194, B: 16, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInspect(t *testing.T) {
	good := pngBytes(t, 4, 3)
	if reason := inspect(good[:min(512, len(good))], bytes.NewReader(good), "image/png"); reason != "" {
		t.Fatalf("valid png rejected: %s", reason)
	}
	if reason := inspect(good, bytes.NewReader(good), "image/jpeg"); reason == "" {
		t.Fatal("png declared as jpeg accepted")
	}
	html := []byte("<html><script>alert(1)</script></html>")
	if reason := inspect(html, bytes.NewReader(html), "image/png"); reason == "" {
		t.Fatal("html disguised as png accepted")
	}
	// Pemeriksaan membaca header saja; file yang terpotong sebelum header lengkap harus ditolak.
	broken := append([]byte{}, good[:20]...)
	if reason := inspect(broken, bytes.NewReader(broken), "image/png"); reason == "" {
		t.Fatal("truncated png accepted")
	}
}

func TestLocalStorage(t *testing.T) {
	storage := LocalStorage{Dir: t.TempDir()}
	if _, err := storage.Write("product_photo/abc", strings.NewReader("12345"), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storage.Dir, "product_photo", "abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Write("product_photo/big", strings.NewReader("123456"), 5); err == nil {
		t.Fatal("oversized write accepted")
	}
	if _, err := os.Stat(filepath.Join(storage.Dir, "product_photo", "big")); err == nil {
		t.Fatal("oversized object left on disk")
	}
	for _, key := range []string{"../escape", "a/../b", "/abs", "Upper", "a//b", "a/./b"} {
		if _, err := storage.Write(key, strings.NewReader("x"), 1); err == nil {
			t.Errorf("unsafe key %q accepted", key)
		}
	}
	if err := storage.Remove("product_photo/abc"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Remove("product_photo/abc"); err != nil {
		t.Fatal("removing missing object must be idempotent")
	}
}
