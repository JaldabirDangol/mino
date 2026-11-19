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
	CORSOrigins   []string      // MINO_CORS_ORIGINS, comma-separated origins allowed to call /api; default "*"

	// S3-compatible media storage (AWS S3, Cloudflare R2, MinIO, ...). When S3Bucket is empty,
	// files are stored on local disk in UploadDir.
	S3Endpoint  string // MINO_S3_ENDPOINT, e.g. https://s3.eu-west-1.amazonaws.com or https://<account>.r2.cloudflarestorage.com
	S3Region    string // MINO_S3_REGION, default us-east-1 (use "auto" for R2)
	S3Bucket    string // MINO_S3_BUCKET
	S3AccessKey string // MINO_S3_ACCESS_KEY
	S3SecretKey string // MINO_S3_SECRET_KEY
	S3PublicURL string // MINO_S3_PUBLIC_URL, base URL clients use to fetch files (bucket website, CDN or r2.dev domain)
	S3PathStyle bool   // MINO_S3_PATH_STYLE, default true (endpoint/bucket/key); false for bucket.endpoint/key
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

	for _, o := range strings.Split(env("MINO_CORS_ORIGINS", "*"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}

	c.S3Bucket = os.Getenv("MINO_S3_BUCKET")
	c.S3Endpoint = strings.TrimRight(os.Getenv("MINO_S3_ENDPOINT"), "/")
	c.S3Region = env("MINO_S3_REGION", "us-east-1")
	c.S3AccessKey = os.Getenv("MINO_S3_ACCESS_KEY")
	c.S3SecretKey = os.Getenv("MINO_S3_SECRET_KEY")
	c.S3PublicURL = strings.TrimRight(os.Getenv("MINO_S3_PUBLIC_URL"), "/")
	if c.S3PathStyle, err = envBool("MINO_S3_PATH_STYLE", true); err != nil {
		return nil, err
	}
	if c.S3Bucket != "" {
		var missing []string
		for k, v := range map[string]string{"MINO_S3_ENDPOINT": c.S3Endpoint, "MINO_S3_ACCESS_KEY": c.S3AccessKey,
			"MINO_S3_SECRET_KEY": c.S3SecretKey, "MINO_S3_PUBLIC_URL": c.S3PublicURL} {
			if v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("MINO_S3_BUCKET is set but %s missing", strings.Join(missing, ", "))
		}
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
