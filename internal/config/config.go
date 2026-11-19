// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every runtime setting. All values come from MINO_* environment variables.
type Config struct {
	Addr          string        // MINO_ADDR, default ":1337"
	DatabaseURL   string        // MINO_DATABASE_URL (required)
	DBMaxConns    int32         // MINO_DB_MAX_CONNS, default 5
	PublicURL     string        // MINO_PUBLIC_URL, e.g. https://cms.example.com (used for absolute media URLs and cookie security)
	UploadDir     string        // MINO_UPLOAD_DIR, default "./uploads"
	UploadMaxSize int64         // MINO_UPLOAD_MAX_MB in megabytes, default 25
	SessionTTL    time.Duration // MINO_SESSION_TTL, idle timeout, default 12h
	SessionMaxAge time.Duration // MINO_SESSION_MAX_AGE, absolute lifetime, default 7 days
	SecureCookies bool          // MINO_SECURE_COOKIES, default true when PublicURL is https
	TrustProxy    bool          // MINO_TRUST_PROXY, read client IP from X-Forwarded-For
	LogLevel      slog.Level    // MINO_LOG_LEVEL: debug|info|warn|error
	LogFormat     string        // MINO_LOG_FORMAT: text|json, default text
	Dev           bool          // MINO_DEV: serve static assets from disk, verbose errors
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Addr:        env("MINO_ADDR", ":1337"),
		DatabaseURL: os.Getenv("MINO_DATABASE_URL"),
		PublicURL:   strings.TrimRight(os.Getenv("MINO_PUBLIC_URL"), "/"),
		UploadDir:   env("MINO_UPLOAD_DIR", "./uploads"),
		LogFormat:   env("MINO_LOG_FORMAT", "text"),
	}
	var err error
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("MINO_DATABASE_URL is required")
	}
	maxConns, err := envInt("MINO_DB_MAX_CONNS", 5)
	if err != nil {
		return nil, err
	}
	c.DBMaxConns = int32(maxConns)
	mb, err := envInt("MINO_UPLOAD_MAX_MB", 25)
	if err != nil {
		return nil, err
	}
	c.UploadMaxSize = int64(mb) << 20
	if c.SessionTTL, err = envDuration("MINO_SESSION_TTL", 12*time.Hour); err != nil {
		return nil, err
	}
	if c.SessionMaxAge, err = envDuration("MINO_SESSION_MAX_AGE", 7*24*time.Hour); err != nil {
		return nil, err
	}
	if c.SecureCookies, err = envBool("MINO_SECURE_COOKIES", strings.HasPrefix(c.PublicURL, "https://")); err != nil {
		return nil, err
	}
	if c.TrustProxy, err = envBool("MINO_TRUST_PROXY", false); err != nil {
		return nil, err
	}
	if c.Dev, err = envBool("MINO_DEV", false); err != nil {
		return nil, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("MINO_LOG_LEVEL", "info"))); err != nil {
		return nil, fmt.Errorf("MINO_LOG_LEVEL: %w", err)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return nil, fmt.Errorf("MINO_LOG_FORMAT must be text or json")
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return b, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 12h", key)
	}
	return d, nil
}
