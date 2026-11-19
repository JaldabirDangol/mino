// Command mino runs the Mino headless CMS.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"mino/internal/admin"
	"mino/internal/api"
	"mino/internal/apitokens"
	"mino/internal/auth"
	"mino/internal/config"
	"mino/internal/content"
	"mino/internal/db"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/server"
	"mino/internal/settings"
	"mino/internal/sites"
	"mino/internal/users"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mino:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	}
	log := slog.New(handler)

	// A soft memory limit keeps the heap small on low-memory hosts; GOMEMLIMIT overrides it.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(128 << 20)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool, log); err != nil {
		return err
	}

	var storage media.Storage
	var local *media.Local
	if cfg.S3Bucket != "" {
		storage = &media.S3{Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, PublicURL: cfg.S3PublicURL, PathStyle: cfg.S3PathStyle}
		log.Info("media storage", "backend", "s3", "bucket", cfg.S3Bucket)
	} else {
		local = &media.Local{Dir: cfg.UploadDir, BaseURL: cfg.PublicURL + "/uploads"}
		storage = local
		log.Info("media storage", "backend", "local", "dir", cfg.UploadDir)
	}

	siteSvc := sites.NewService(pool)
	schemas := schema.NewService(pool)
	mediaSvc := media.NewService(pool, storage, cfg.UploadMaxSize, log)
	engine := content.New(pool, schemas)
	engine.MediaURL = mediaSvc.URL
	tokens := apitokens.NewService(pool)
	settingsSvc := settings.NewService(pool)

	apiHandler := &api.Handler{Sites: siteSvc, Schemas: schemas, Engine: engine, Media: mediaSvc, Tokens: tokens,
		Settings: settingsSvc, Log: log, CORSOrigins: cfg.CORSOrigins}

	adminHandler := &admin.Handler{Sites: siteSvc, Schemas: schemas, Engine: engine, Media: mediaSvc,
		Users: users.NewService(pool), Sessions: auth.NewSessions(pool, cfg.SessionTTL, cfg.SessionMaxAge),
		Tokens: tokens, Settings: settingsSvc, Log: log, SecureCookies: cfg.SecureCookies,
		SessionMaxAge: cfg.SessionMaxAge, PublicURL: cfg.PublicURL, Dev: cfg.Dev}
	adminRoutes, err := adminHandler.Routes()
	if err != nil {
		return fmt.Errorf("admin templates: %w", err)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(server.Options{Log: log, Pool: pool, API: apiHandler.Routes(), Admin: adminRoutes,
			Local: local, TrustProxy: cfg.TrustProxy}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
