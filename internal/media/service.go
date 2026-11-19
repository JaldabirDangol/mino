package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register decoders for dimension detection
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/schema"
)

var (
	// ErrNotFound is returned when a file does not exist in the site.
	ErrNotFound = errors.New("media file not found")
	// ErrTooLarge is returned when an upload exceeds the configured limit.
	ErrTooLarge = errors.New("file is too large")
	// ErrType is returned for disallowed file types.
	ErrType = errors.New("file type is not allowed")
)

// Service manages a site's media library.
type Service struct {
	pool    *pgxpool.Pool
	storage Storage
	maxSize int64
	log     *slog.Logger
}

// NewService creates a media service.
func NewService(pool *pgxpool.Pool, storage Storage, maxSize int64, log *slog.Logger) *Service {
	return &Service{pool: pool, storage: storage, maxSize: maxSize, log: log}
}

// Storage returns the configured backend.
func (s *Service) Storage() Storage { return s.storage }

// MaxSize returns the upload size limit in bytes.
func (s *Service) MaxSize() int64 { return s.maxSize }

// URL returns a file's public URL.
func (s *Service) URL(f *File) string { return s.storage.URL(f.Key) }

// allowedTypes maps sniffed MIME types to the file extension used for storage.
// SVG and HTML are deliberately excluded: they can carry scripts.
var allowedTypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/avif":      ".avif",
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"audio/mpeg":      ".mp3",
	"audio/wave":      ".wav",
	"audio/ogg":       ".ogg",
	"application/ogg": ".ogg",
	"application/pdf": ".pdf",
	"application/zip": ".zip",
	"text/plain":      ".txt",
}

// zipBased are formats that sniff as ZIP; the extension decides the precise type.
var zipBased = map[string]string{
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
}

// textBased are plain-text formats identified by extension.
var textBased = map[string]string{
	".txt": "text/plain", ".csv": "text/csv", ".md": "text/markdown", ".json": "application/json",
}

// blockedExtensions can execute script in a browser; they are rejected whatever their content sniffs as.
var blockedExtensions = map[string]bool{
	".svg": true, ".svgz": true, ".html": true, ".htm": true, ".xhtml": true, ".xml": true, ".js": true, ".mjs": true, ".swf": true,
}

// detectType sniffs content and returns the MIME type and storage extension.
func detectType(head []byte, filename string) (string, string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if blockedExtensions[ext] {
		return "", "", fmt.Errorf("%w (%s files can contain scripts)", ErrType, ext)
	}
	mime := http.DetectContentType(head)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	// Go's sniffer does not know AVIF; recognize its ISO-BMFF brand.
	if len(head) >= 12 && string(head[4:8]) == "ftyp" && (string(head[8:12]) == "avif" || string(head[8:12]) == "avis") {
		mime = "image/avif"
	}
	switch mime {
	case "application/zip":
		if t, ok := zipBased[ext]; ok {
			return t, ext, nil
		}
	case "text/plain":
		if t, ok := textBased[ext]; ok {
			return t, ext, nil
		}
	}
	storeExt, ok := allowedTypes[mime]
	if !ok {
		return "", "", fmt.Errorf("%w (%s)", ErrType, mime)
	}
	return mime, storeExt, nil
}

func randomKey(siteID int64, ext string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strconv.FormatInt(siteID, 10) + "/" + hex.EncodeToString(b) + ext
}

func cleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if !utf8.ValidString(name) || name == "." || name == "/" || name == "" {
		name = "file"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// Upload validates and stores a file in a site's library.
func (s *Service) Upload(ctx context.Context, siteID, userID int64, filename string, r io.Reader) (*File, error) {
	tmp, err := os.CreateTemp("", "mino-upload-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, s.maxSize+1))
	if err != nil {
		return nil, err
	}
	if n > s.maxSize {
		return nil, ErrTooLarge
	}
	if n == 0 {
		return nil, fmt.Errorf("%w (empty file)", ErrType)
	}
	head := make([]byte, 512)
	if _, err := tmp.ReadAt(head, 0); err != nil && err != io.EOF {
		return nil, err
	}
	if n < 512 {
		head = head[:n]
	}
	name := cleanName(filename)
	mime, ext, err := detectType(head, name)
	if err != nil {
		return nil, err
	}

	f := &File{Name: name, Mime: mime, Size: n, Storage: s.storage.Name(), Key: randomKey(siteID, ext)}
	if strings.HasPrefix(mime, "image/") {
		if _, err := tmp.Seek(0, io.SeekStart); err == nil {
			if cfg, _, err := image.DecodeConfig(tmp); err == nil {
				f.Width, f.Height = &cfg.Width, &cfg.Height
			} else if w, hgt, ok := webpSize(head); ok {
				f.Width, f.Height = &w, &hgt
			}
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := s.storage.Put(ctx, f.Key, tmp, n, mime, hex.EncodeToString(h.Sum(nil))); err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}

	var creator *int64
	if userID > 0 {
		creator = &userID
	}
	out, err := Scan(s.pool.QueryRow(ctx, `INSERT INTO media AS m (site_id, name, mime, size, width, height, storage, storage_key, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+Columns,
		siteID, f.Name, f.Mime, f.Size, f.Width, f.Height, f.Storage, f.Key, creator))
	if err != nil {
		if derr := s.storage.Delete(context.WithoutCancel(ctx), f.Key); derr != nil {
			s.log.Warn("could not remove orphaned upload", "key", f.Key, "err", derr)
		}
		return nil, err
	}
	out.URL = s.URL(out)
	return out, nil
}

// webpSize reads dimensions from VP8, VP8L and VP8X headers.
func webpSize(b []byte) (int, int, bool) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		return int(b[26]) | int(b[27]&0x3f)<<8, int(b[28]) | int(b[29]&0x3f)<<8, true
	case "VP8L":
		bits := uint32(b[21]) | uint32(b[22])<<8 | uint32(b[23])<<16 | uint32(b[24])<<24
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
	case "VP8X":
		return 1 + (int(b[24]) | int(b[25])<<8 | int(b[26])<<16), 1 + (int(b[27]) | int(b[28])<<8 | int(b[29])<<16), true
	}
	return 0, 0, false
}

// ListQuery filters the library.
type ListQuery struct {
	Search   string
	Category string // images | videos | audios | files
	Page     int
	PageSize int
}

// List returns a page of a site's files, newest first.
func (s *Service) List(ctx context.Context, siteID int64, q ListQuery) ([]*File, int, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 || q.PageSize > 100 {
		q.PageSize = 24
	}
	where := "m.site_id = $1"
	args := []any{siteID}
	if q.Search != "" {
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Search)+"%")
		where += fmt.Sprintf(" AND (m.name ILIKE $%d OR m.alt ILIKE $%d OR m.caption ILIKE $%d)", len(args), len(args), len(args))
	}
	switch q.Category {
	case "images", "videos", "audios":
		args = append(args, map[string]string{"images": "image/", "videos": "video/", "audios": "audio/"}[q.Category]+"%")
		where += fmt.Sprintf(" AND m.mime LIKE $%d", len(args))
	case "files":
		where += " AND m.mime NOT LIKE 'image/%' AND m.mime NOT LIKE 'video/%' AND m.mime NOT LIKE 'audio/%'"
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM media m WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+Columns+` FROM media m WHERE `+where+
		fmt.Sprintf(` ORDER BY m.created_at DESC, m.id DESC LIMIT %d OFFSET %d`, q.PageSize, (q.Page-1)*q.PageSize), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := Scan(rows)
		if err != nil {
			return nil, 0, err
		}
		f.URL = s.URL(f)
		out = append(out, f)
	}
	return out, total, rows.Err()
}

// Get returns one file of a site.
func (s *Service) Get(ctx context.Context, siteID, id int64) (*File, error) {
	f, err := Scan(s.pool.QueryRow(ctx, `SELECT `+Columns+` FROM media m WHERE m.id = $1 AND m.site_id = $2`, id, siteID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.URL = s.URL(f)
	return f, nil
}

// Update changes a file's descriptive metadata.
func (s *Service) Update(ctx context.Context, siteID, id int64, name, alt, caption string) (*File, error) {
	ve := &schema.ValidationError{Message: "Invalid file details"}
	name = strings.TrimSpace(name)
	if name == "" {
		ve.Add("Name is required", "name")
	}
	if len(alt) > 500 {
		ve.Add("Alternative text must be at most 500 characters", "alt")
	}
	if len(caption) > 1000 {
		ve.Add("Caption must be at most 1000 characters", "caption")
	}
	if err := ve.OrNil(); err != nil {
		return nil, err
	}
	f, err := Scan(s.pool.QueryRow(ctx, `UPDATE media AS m SET name = $3, alt = $4, caption = $5, updated_at = now()
		WHERE m.id = $1 AND m.site_id = $2 RETURNING `+Columns, id, siteID, cleanName(name), strings.TrimSpace(alt), strings.TrimSpace(caption)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.URL = s.URL(f)
	return f, nil
}

// Usage returns how many entry fields reference each of the given files.
func (s *Service) Usage(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT media_id, count(*) FROM media_links WHERE media_id = ANY($1) GROUP BY 1`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Delete removes a file from the library (and from every entry using it) and from storage.
func (s *Service) Delete(ctx context.Context, siteID, id int64) error {
	var key string
	err := s.pool.QueryRow(ctx, `DELETE FROM media WHERE id = $1 AND site_id = $2 RETURNING storage_key`, id, siteID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.storage.Delete(ctx, key); err != nil {
		// The database row is gone; an orphaned object is harmless but worth reporting.
		s.log.Warn("could not delete stored file", "key", key, "err", err)
	}
	return nil
}

// DeleteSiteFiles removes every stored object of a site. Call it before deleting the site.
func (s *Service) DeleteSiteFiles(ctx context.Context, siteID int64) error {
	return s.storage.DeletePrefix(ctx, strconv.FormatInt(siteID, 10)+"/")
}
