// Command api serves the Phase 1 HTTP API.
//
// At P1-000 it exposes only /healthz -- enough to prove the process reaches
// PostgreSQL and Redis. Documented endpoints arrive with the generated server
// interface in P1-005.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	httpapi "github.com/miqbalhamdani/sewain-api/internal/http"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/platform/telemetry"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Before anything that can fail with a trace id in it.
	flushTraces, err := telemetry.Setup(ctx,
		config.ServiceName(), config.ServiceVersion(), config.Environment(), config.OTLPEndpoint())
	if err != nil {
		return err
	}
	defer func() {
		if err := flushTraces(context.WithoutCancel(ctx)); err != nil {
			slog.Warn("flushing traces", "error", err)
		}
	}()

	pool, err := db.New(ctx, config.AppDatabaseURL())
	if err != nil {
		return err
	}
	defer pool.Close()

	redis, err := queue.New(ctx, config.RedisURL())
	if err != nil {
		return err
	}
	defer func() { _ = redis.Close() }()

	secret, err := config.JWTSecret()
	if err != nil {
		return err
	}
	signer, err := auth.NewSigner(secret)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	// Not a contract endpoint, so not generated and not under /v1.
	mux.Handle("GET /healthz", newHealthHandler(
		checker{name: "postgres", version: pool.ServerVersion},
		checker{name: "redis", version: redis.ServerVersion},
	))
	mux.Handle("/v1/", httpapi.NewRouter(
		httpapi.NewServer(auth.NewService(pool, signer), !config.IsDevelopment()),
		signer,
	))

	addr := ":" + config.Getenv("PORT", config.DefaultPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if config.IsDevelopment() {
		slog.Warn("development mode: signing tokens with the built-in key and issuing non-Secure cookies")
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
