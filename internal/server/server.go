// Package server assembles the HTTP application: REST API, admin panel, uploads and health.
package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/media"
)

// Options configures the router.
type Options struct {
	Log        *slog.Logger
	Pool       *pgxpool.Pool
	API        http.Handler // mounted at /api
	Admin      http.Handler // mounted at /admin (may be nil)
	Docs       http.Handler // mounted at /docs (may be nil)
	Local      *media.Local // serves /uploads when media is stored on disk
	TrustProxy bool
}

// New returns the root handler.
func New(o Options) http.Handler {
	r := chi.NewRouter()
	if o.TrustProxy {
		r.Use(proxyIP)
	} else {
		r.Use(warnUntrustedProxy(o.Log))
	}
	r.Use(recoverer(o.Log), requestLog(o.Log), securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := o.Pool.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	r.Mount("/api", o.API)
	if o.Docs != nil {
		r.Mount("/docs", o.Docs)
	}
	if o.Admin != nil {
		r.Mount("/admin", o.Admin)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusFound) })
	}
	if o.Local != nil {
		r.Get("/uploads/*", serveLocal(o.Local))
	}
	return r
}

// serveLocal serves uploaded files. Files are served with a sandboxing CSP and, unless they
// are media a browser can display safely, as attachments.
func serveLocal(l *media.Local) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "*")
		f, err := l.Open(key)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		ctype := mime.TypeByExtension(path.Ext(key))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'")
		inline := strings.HasPrefix(ctype, "image/") || strings.HasPrefix(ctype, "video/") ||
			strings.HasPrefix(ctype, "audio/") || ctype == "application/pdf"
		if !inline {
			w.Header().Set("Content-Disposition", "attachment")
		}
		http.ServeContent(w, r, "", st.ModTime(), f)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if strings.HasPrefix(r.URL.Path, "/admin") {
			h.Set("X-Frame-Options", "DENY")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (flush, deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/admin/static/") ||
				strings.HasPrefix(r.URL.Path, "/docs/static/") {
				return
			}
			level := slog.LevelInfo
			if sw.status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Int("status", sw.status),
				slog.Int("bytes", sw.bytes), slog.Duration("duration", time.Since(start)), slog.String("ip", ClientIP(r)))
		})
	}
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.Error("panic", "path", r.URL.Path, "panic", v)
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// proxyIP rewrites RemoteAddr from X-Forwarded-For when Mino runs behind one trusted reverse
// proxy. Only the right-most entry is used: it is the address the proxy itself saw, while
// entries further left are supplied by the client and can be forged.
func proxyIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// warnUntrustedProxy logs once when requests arrive through a proxy that Mino does not trust:
// every client then shares the proxy's IP, so login rate limiting would throttle everyone together.
func warnUntrustedProxy(log *slog.Logger) func(http.Handler) http.Handler {
	var once sync.Once
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Forwarded-For") != "" {
				once.Do(func() {
					log.Warn("requests carry X-Forwarded-For but MINO_TRUST_PROXY is off; set MINO_TRUST_PROXY=true when running behind a reverse proxy so rate limits and logs see real client IPs")
				})
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the client address used for logging and rate limiting.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
