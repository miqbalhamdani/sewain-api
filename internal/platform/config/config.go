// Package config reads the handful of settings the binaries need from the
// environment.
//
// It exists because cmd/api and cmd/migrate both reach PostgreSQL and would
// otherwise each carry their own copy of the defaults -- two copies that can
// drift, pointing two binaries at two different databases or the wrong role.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
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

	// Mailpit as a single binary on the host, same philosophy as PostgreSQL,
	// Redis and MinIO in S1-001. Production picks a real relay in M6; the
	// Mailer interface is what makes that a swap rather than a rewrite.
	DefaultSMTPAddr = "localhost:1025"
	DefaultMailFrom = "no-reply@sewain.local"

	// Where the links in outgoing mail point. The backoffice origin, because
	// that is where /verify-email and /accept-invitation are rendered.
	DefaultAppBaseURL = "http://localhost:3000"
	DefaultPort       = "8080"

	// Local development only. There is no safe default for a signing secret,
	// so this one is obviously not a secret -- JWTSecret refuses it whenever
	// the environment is not "development".
	devJWTSecret = "insecure-development-only-signing-key-32b"

	// Exactly 32 bytes, and as obviously not a secret. IdentityKey refuses it
	// outside development.
	devIdentityKey = "insecure-dev-identity-key-32byte"
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

// ObjectStoreBucket is the one private bucket every object lives in. Tenancy is
// the owner_id key prefix, not a bucket per rental (CLAUDE.md, BR-093).
func ObjectStoreBucket() string { return Getenv("OBJECT_STORE_BUCKET", "sewain") }

// ObjectStoreRegion is "us-east-1" for MinIO; R2 accepts "auto".
func ObjectStoreRegion() string { return Getenv("OBJECT_STORE_REGION", "us-east-1") }

// ObjectStoreCredentials returns the access key pair. Same rule as JWTSecret:
// the MinIO defaults only exist in development, and a deployed environment
// without its own pair is a startup failure.
func ObjectStoreCredentials() (accessKey, secretKey string, err error) {
	accessKey, secretKey = os.Getenv("OBJECT_STORE_ACCESS_KEY"), os.Getenv("OBJECT_STORE_SECRET_KEY")
	if accessKey != "" && secretKey != "" {
		return accessKey, secretKey, nil
	}
	if IsDevelopment() {
		return "minioadmin", "minioadmin", nil
	}
	return "", "", errors.New("OBJECT_STORE_ACCESS_KEY and OBJECT_STORE_SECRET_KEY are required when ENVIRONMENT is not \"development\"")
}

func SMTPAddr() string   { return Getenv("SMTP_ADDR", DefaultSMTPAddr) }
func MailFrom() string   { return Getenv("MAIL_FROM", DefaultMailFrom) }
func AppBaseURL() string { return Getenv("APP_BASE_URL", DefaultAppBaseURL) }

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

// IdentityKey returns the 32-byte AES-256 key that encrypts identity numbers
// (BR-085), from IDENTITY_ENC_KEY as standard base64.
//
// Same rule as JWTSecret: outside development a missing key is a startup
// failure. The key lives outside the database on purpose -- a dump of the
// customers table without it is ciphertext and four digits.
func IdentityKey() ([]byte, error) {
	v := os.Getenv("IDENTITY_ENC_KEY")
	if v == "" {
		if IsDevelopment() {
			return []byte(devIdentityKey), nil
		}
		return nil, errors.New("IDENTITY_ENC_KEY is required when ENVIRONMENT is not \"development\"")
	}
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("IDENTITY_ENC_KEY is not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("IDENTITY_ENC_KEY decodes to %d bytes; AES-256 needs 32", len(key))
	}
	return key, nil
}

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

// ── M5: the public surface and the renter portal (04-api-spec.md §4, §5) ──────

const (
	// Local only, and obviously not secrets -- both refuse them outside
	// development, like devJWTSecret.
	devProxySecret  = "insecure-dev-proxy-secret"
	devPortalSecret = "insecure-dev-portal-secret-32byte"
)

// PublicApex is the domain tenant hosts live under: <slug>.<apex>, plus
// api.<apex> for the external API. Configuration, not a constant -- the same
// value Caddy and the web middleware read (05-backlog.md, S1-077).
func PublicApex() string { return Getenv("PUBLIC_APEX", "sewain.localhost") }

// TenantOriginFormat turns a slug into the origin a renter opens, for the
// links the API hands out (track_url, Booking.portal_url).
func TenantOriginFormat() string {
	return Getenv("TENANT_ORIGIN_FORMAT", "http://%s.sewain.localhost:8088")
}

// ProxySecret is the header Caddy adds to every request it forwards. Only a
// request carrying it may name a tenant by Host or trust X-Real-IP: without it,
// anyone who reaches the port directly could forge Host (BR-030).
func ProxySecret() (string, error) {
	return secret("PROXY_SECRET", devProxySecret)
}

// PortalSecret keys the renter-portal token HMAC. Rotating it revokes every
// portal link at once -- the tokens are never stored (04-api-spec.md §5).
func PortalSecret() (string, error) {
	return secret("PORTAL_SECRET", devPortalSecret)
}

func secret(key, dev string) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	if IsDevelopment() {
		return dev, nil
	}
	return "", fmt.Errorf("%s is required when ENVIRONMENT is not \"development\"", key)
}

// PublicLimits are 04-api-spec.md §7's numbers -- "konfigurasi, bukan
// konstanta di kode".
type PublicLimits struct {
	GetPerMinIP, GetPerMinOwner     int64 // GET /public/*
	PostPerHourIP, PostPerHourOwner int64 // POST /public/bookings
	PortalPerMinToken               int64 // /portal/*
}

func PublicRateLimits() PublicLimits {
	return PublicLimits{
		GetPerMinIP:       intEnv("PUBLIC_GET_PER_MIN_IP", 60),
		GetPerMinOwner:    intEnv("PUBLIC_GET_PER_MIN_OWNER", 600),
		PostPerHourIP:     intEnv("PUBLIC_POST_PER_HOUR_IP", 5),
		PostPerHourOwner:  intEnv("PUBLIC_POST_PER_HOUR_OWNER", 30),
		PortalPerMinToken: intEnv("PORTAL_PER_MIN_TOKEN", 120),
	}
}

func intEnv(key string, fallback int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && n > 0 {
		return n
	}
	return fallback
}
