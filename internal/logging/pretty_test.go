package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPretty(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewPretty(&buf, slog.LevelInfo, false))
	log.Info("request", "method", "GET", "path", "/api/blog/articles", "status", 200, "duration", 5*time.Millisecond, "ip", "10.0.0.7")
	log.Warn("Too many login attempts", "email", "a@b.co")
	log.Debug("hidden")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (debug filtered), got %q", buf.String())
	}
	for _, want := range []string{"GET", "200", "/api/blog/articles", "5ms", "10.0.0.7"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("request line %q lacks %q", lines[0], want)
		}
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Error("colors written although disabled")
	}
	if !strings.Contains(lines[1], "WARN") || !strings.Contains(lines[1], "email=a@b.co") {
		t.Errorf("warn line: %q", lines[1])
	}
}
