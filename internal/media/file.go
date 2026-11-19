// Package media manages uploaded files and their storage.
package media

import (
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// File is an uploaded asset.
type File struct {
	ID        int64
	Name      string
	Alt       string
	Caption   string
	Mime      string
	Size      int64
	Width     *int
	Height    *int
	Storage   string
	Key       string
	URL       string // public URL, filled in by the service
	CreatedBy *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Columns selects a File from the media table aliased as m.
const Columns = `m.id, m.name, m.alt, m.caption, m.mime, m.size, m.width, m.height, m.storage, m.storage_key, m.created_by, m.created_at, m.updated_at`

// Scan reads a row selected with Columns.
func Scan(row pgx.Row) (*File, error) { return ScanPrefixed(row) }

// ScanPrefixed reads a row whose leading columns go into prefix, followed by Columns.
func ScanPrefixed(row pgx.Row, prefix ...any) (*File, error) {
	f := &File{}
	dest := append(prefix, &f.ID, &f.Name, &f.Alt, &f.Caption, &f.Mime, &f.Size, &f.Width, &f.Height,
		&f.Storage, &f.Key, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return f, nil
}

// Category maps a MIME type to a media category used by field AllowedTypes.
func Category(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "images"
	case strings.HasPrefix(mime, "video/"):
		return "videos"
	case strings.HasPrefix(mime, "audio/"):
		return "audios"
	}
	return "files"
}

// IsImage reports whether the file can be previewed as an image.
func (f *File) IsImage() bool { return Category(f.Mime) == "images" }

// JSON is the public representation used by the REST API.
func (f *File) JSON() map[string]any {
	return map[string]any{
		"id":              f.ID,
		"name":            f.Name,
		"alternativeText": f.Alt,
		"caption":         f.Caption,
		"mime":            f.Mime,
		"size":            f.Size,
		"width":           f.Width,
		"height":          f.Height,
		"url":             f.URL,
		"createdAt":       f.CreatedAt,
		"updatedAt":       f.UpdatedAt,
	}
}
