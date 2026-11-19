package media

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Storage persists file bytes. Keys look like "<siteID>/<random><ext>".
type Storage interface {
	Name() string
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType, sha256Hex string) error
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every object whose key starts with prefix.
	DeletePrefix(ctx context.Context, prefix string) error
	// URL is the address clients use to download the file.
	URL(key string) string
}

// Local stores files on disk and serves them under BaseURL (default "/uploads").
type Local struct {
	Dir     string
	BaseURL string
}

func (l *Local) Name() string { return "local" }

func (l *Local) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") || clean == "/" {
		return "", errors.New("invalid storage key")
	}
	return filepath.Join(l.Dir, filepath.FromSlash(clean)), nil
}

func (l *Local) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, _, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) DeletePrefix(_ context.Context, prefix string) error {
	prefix = strings.TrimSuffix(prefix, "/")
	p, err := l.path(prefix)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

func (l *Local) URL(key string) string {
	base := l.BaseURL
	if base == "" {
		base = "/uploads"
	}
	return base + "/" + (&url.URL{Path: key}).EscapedPath()
}

// Open returns a file for serving; used by the HTTP handler for local storage.
func (l *Local) Open(key string) (*os.File, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return os.Open(p)
}
