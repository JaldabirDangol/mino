// Package settings stores installation-wide settings.
package settings

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/schema"
)

// General holds the settings edited on the General Settings page.
type General struct {
	SiteName        string `json:"siteName"` // admin panel title
	DefaultPageSize int    `json:"defaultPageSize"`
	MaxPageSize     int    `json:"maxPageSize"`
}

var defaults = General{SiteName: "Mino", DefaultPageSize: 25, MaxPageSize: 100}

// Service reads and writes settings. Values are read on every API request, so the current
// value is kept in memory and replaced on save (one process owns the database writes).
type Service struct {
	pool    *pgxpool.Pool
	current atomic.Pointer[General]
}

// NewService creates a settings service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// General returns the current general settings.
func (s *Service) General(ctx context.Context) General {
	if g := s.current.Load(); g != nil {
		return *g
	}
	g := defaults
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'general'`).Scan(&raw); err == nil {
		_ = json.Unmarshal(raw, &g)
	}
	s.current.Store(&g)
	return g
}

// SaveGeneral validates and stores general settings.
func (s *Service) SaveGeneral(ctx context.Context, g General) error {
	ve := &schema.ValidationError{Message: "Invalid settings"}
	g.SiteName = strings.TrimSpace(g.SiteName)
	if g.SiteName == "" || len(g.SiteName) > 60 {
		ve.Add("Name must be 1 to 60 characters", "siteName")
	}
	if g.MaxPageSize < 1 || g.MaxPageSize > 1000 {
		ve.Add("Must be between 1 and 1000", "maxPageSize")
	}
	if g.DefaultPageSize < 1 || g.DefaultPageSize > g.MaxPageSize {
		ve.Add("Must be between 1 and the maximum page size", "defaultPageSize")
	}
	if err := ve.OrNil(); err != nil {
		return err
	}
	raw, _ := json.Marshal(g)
	if _, err := s.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('general', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, raw); err != nil {
		return err
	}
	s.current.Store(&g)
	return nil
}
