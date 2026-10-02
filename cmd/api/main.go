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
	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/catalog"
	"github.com/miqbalhamdani/sewain-api/internal/customer"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	httpapi "github.com/miqbalhamdani/sewain-api/internal/http"
	"github.com/miqbalhamdani/sewain-api/internal/jobs"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/platform/mail"
	"github.com/miqbalhamdani/sewain-api/internal/platform/ratelimit"
	"github.com/miqbalhamdani/sewain-api/internal/platform/telemetry"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
	"github.com/miqbalhamdani/sewain-api/internal/settings"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
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

	// A non-loopback SMTP host is refused rather than accepted: this sender
	// has no auth and no TLS, and pointing it at a real relay would send
	// verification links in the clear. Falling back to Discard keeps a
	// developer who has not started Mailpit from hitting a wall at register --
	// the link lands in the log instead.
	var mailer mail.Mailer
	if smtp, err := mail.NewSMTP(config.SMTPAddr(), config.MailFrom()); err == nil {
		mailer = smtp
	} else {
		slog.Warn("no mail sender; verification links will go to the log", "error", err)
		mailer = mail.Discard{}
	}

	authSvc := auth.NewService(pool, signer).WithMail(redis, mailer, config.AppBaseURL())

	store, err := storage.FromConfig()
	if err != nil {
		return err
	}

	identityKey, err := config.IdentityKey()
	if err != nil {
		return err
	}
	customerSvc, err := customer.New(pool, identityKey, store)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	// Not a contract endpoint, so not generated and not under BasePath.
	mux.Handle("GET /healthz", newHealthHandler(
		checker{name: "postgres", version: pool.ServerVersion},
		checker{name: "redis", version: redis.ServerVersion},
		objectStoreChecker(store),
	))
	mux.Handle(httpapi.BasePath+"/", httpapi.NewRouter(
		httpapi.NewServer(authSvc, settings.New(pool), catalog.New(pool),
			customerSvc, booking.New(pool, store).WithJobs(jobs.NewQueue(redis.Raw(), jobs.Default), booking.NoScanner{}), store,
			ratelimit.New(redis), !config.IsDevelopment()),
		signer,
		redis,
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
