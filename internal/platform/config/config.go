// Package config reads the handful of settings the binaries need from the
// environment.
//
// It exists because cmd/api and cmd/migrate both reach PostgreSQL and would
// otherwise each carry their own copy of the defaults -- two copies that can
// drift, pointing two binaries at two different databases or the wrong role.
package config

import (
	"errors"
	"os"
)

// Defaults target a host install -- Homebrew's postgresql@18 and redis on their
// standard ports (S1-001). They are compiled in so that a clean
// checkout runs without a .env; anything else overrides via the environment.
const (
	// The schema owner. Migrations run as this role; the API never does.
	DefaultDatabaseURL = "postgres://localhost:5432/sewain_dev?sslmode=disable"

	// app_user, which owns nothing. FORCE ROW LEVEL SECURITY binds a table's
	// owner too, but a superuser bypasses RLS outright -- and the local owner
	// is one. Connecting the API as app_user is what makes the policies real
	// rather than decorative. See tdd.md 3.1.
	DefaultAppDatabaseURL = "postgres://app_user@localhost:5432/sewain_dev?sslmode=disable"

	DefaultRedisURL = "redis://localhost:6379/0"

	// MinIO as a single binary on the host, not a container (S1-001). In
	// production this is R2 instead; the adapter is the same either way,
	// because both speak S3 (S1-033).
	DefaultObjectStoreURL = "http://localhost:9000"
	DefaultPort           = "8080"

	// Local development only. There is no safe default for a signing secret,
	// so this one is obviously not a secret -- JWTSecret refuses it whenever
	// the environment is not "development".
	devJWTSecret = "insecure-development-only-signing-key-32b"
)

// Getenv returns the value of key, or fallback when it is unset or empty.
func Getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// DatabaseURL is the owning role's DSN. Migrations only -- it can create and
// drop tables, and locally it is a superuser that RLS does not apply to.
func DatabaseURL() string { return Getenv("DATABASE_URL", DefaultDatabaseURL) }

// AppDatabaseURL is the DSN the application serves requests on, as app_user.
func AppDatabaseURL() string { return Getenv("APP_DATABASE_URL", DefaultAppDatabaseURL) }

// RedisURL is the Redis DSN.
func RedisURL() string { return Getenv("REDIS_URL", DefaultRedisURL) }

func ObjectStoreURL() string { return Getenv("OBJECT_STORE_URL", DefaultObjectStoreURL) }

// OTLPEndpoint is where traces are sent. Empty means nowhere: ids are still
// real and still land in the logs, which is all there is to do before P1-001
// provides a collector.
func OTLPEndpoint() string { return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") }

// ServiceName and ServiceVersion label this process in a trace.
func ServiceName() string    { return Getenv("OTEL_SERVICE_NAME", "sewain-api") }
func ServiceVersion() string { return Getenv("SERVICE_VERSION", "dev") }

// Environment is "development" unless something says otherwise. Anything else
// is treated as a deployed environment and held to deployed standards.
func Environment() string { return Getenv("ENVIRONMENT", "development") }

// IsDevelopment reports whether this is a developer's machine.
func IsDevelopment() bool { return Environment() == "development" }

// JWTSecret returns the access-token signing key.
//
// Outside development there is no default and no fallback: a missing secret is
// a startup failure, not a warning. A service that boots with a well-known key
// signs tokens anyone can forge, and it boots silently.
func JWTSecret() (string, error) {
	if v := os.Getenv("JWT_SECRET"); v != "" {
		return v, nil
	}
	if IsDevelopment() {
		return devJWTSecret, nil
	}
	return "", errors.New("JWT_SECRET is required when ENVIRONMENT is not \"development\"")
}
