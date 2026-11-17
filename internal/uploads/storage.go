// Package uploads menerbitkan izin unggah bertenggat, menerima file, memverifikasinya, dan menyajikannya
// kembali hanya kepada pihak yang berhak.
package uploads

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
)

// LocalStorage menyimpan objek di direktori persisten. Direktori harus berada di volume yang dibackup,
// bukan di folder build. Kunci objek selalu dibuat server, tidak pernah dari input klien.
type LocalStorage struct {
	Dir string
}

var keyPattern = regexp.MustCompile(`^[a-z0-9_-][a-z0-9/_-]{0,199}$`)

func (s LocalStorage) locate(key string) (string, error) {
	if !keyPattern.MatchString(key) || path.Clean(key) != key {
		return "", fmt.Errorf("invalid storage key")
	}
	return filepath.Join(s.Dir, filepath.FromSlash(key)), nil
}

// Write menulis objek secara atomik: file sementara lalu rename, sehingga pembaca tidak melihat isi setengah jadi.
func (s LocalStorage) Write(key string, body io.Reader, limit int64) (int64, error) {
	target, err := s.locate(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return 0, err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".upload-*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	written, err := io.Copy(temp, io.LimitReader(body, limit+1))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, errTooLarge
	}
	return written, os.Rename(temp.Name(), target)
}

func (s LocalStorage) Open(key string) (*os.File, error) {
	target, err := s.locate(key)
	if err != nil {
		return nil, err
	}
	return os.Open(target)
}

func (s LocalStorage) Remove(key string) error {
	target, err := s.locate(key)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
