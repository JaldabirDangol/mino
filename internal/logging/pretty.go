// Package logging provides a human-friendly slog handler for terminals.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// IsTerminal reports whether f is an interactive terminal (no external dependency needed).
func IsTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Pretty writes one short, aligned line per record:
//
//	12:42:08  GET     403  /api/acme-blog/articles        5ms
//	12:42:09  WARN    login rate limited  ip=::1
//
// Request records (msg "request") are shown as method, status, path and duration.
type Pretty struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler
	color bool
	attrs []slog.Attr
}

// NewPretty creates a handler. Colors are used when color is true.
func NewPretty(w io.Writer, level slog.Leveler, color bool) *Pretty {
	return &Pretty{mu: &sync.Mutex{}, w: w, level: level, color: color}
}

const (
	reset  = "\033[0m"
	dim    = "\033[2m"
	bold   = "\033[1m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

func (h *Pretty) paint(code, s string) string {
	if !h.color {
		return s
	}
	return code + s + reset
}

func (h *Pretty) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *Pretty) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &c
}

func (h *Pretty) WithGroup(string) slog.Handler { return h }

func (h *Pretty) Handle(_ context.Context, r slog.Record) error {
	attrs := append([]slog.Attr{}, h.attrs...)
	r.Attrs(func(a slog.Attr) bool { attrs = append(attrs, a); return true })

	var b strings.Builder
	b.WriteString(h.paint(dim, r.Time.Format("15:04:05")))
	b.WriteString("  ")

	if r.Message == "request" {
		h.request(&b, attrs)
	} else {
		switch {
		case r.Level >= slog.LevelError:
			b.WriteString(h.paint(red+bold, "ERROR "))
		case r.Level >= slog.LevelWarn:
			b.WriteString(h.paint(yellow+bold, "WARN  "))
		case r.Level < slog.LevelInfo:
			b.WriteString(h.paint(dim, "DEBUG "))
		default:
			b.WriteString(h.paint(cyan, "INFO  "))
		}
		b.WriteString(r.Message)
		for _, a := range attrs {
			b.WriteString("  ")
			b.WriteString(h.paint(dim, a.Key+"="))
			b.WriteString(fmt.Sprint(a.Value.Any()))
		}
	}
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *Pretty) request(b *strings.Builder, attrs []slog.Attr) {
	var method, path, ip string
	var status int64
	var dur time.Duration
	for _, a := range attrs {
		switch a.Key {
		case "method":
			method = a.Value.String()
		case "path":
			path = a.Value.String()
		case "status":
			status = a.Value.Int64()
		case "duration":
			dur = a.Value.Duration()
		case "ip":
			ip = a.Value.String()
		}
	}
	statusColor := green
	switch {
	case status >= 500:
		statusColor = red + bold
	case status >= 400:
		statusColor = yellow
	case status >= 300:
		statusColor = cyan
	}
	fmt.Fprintf(b, "%-6s  %s  %-40s %s", method, h.paint(statusColor, fmt.Sprint(status)), path, h.paint(dim, fmt.Sprintf("%6s", short(dur))))
	if ip != "" && ip != "::1" && ip != "127.0.0.1" {
		b.WriteString(h.paint(dim, "  "+ip))
	}
}

// short formats durations compactly: 840µs, 5ms, 1.2s.
func short(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
