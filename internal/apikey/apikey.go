// Package apikey issues and checks the keys for api.<apex>.  (S1-079, S1-080, BR-031)
//
// A key is shown once and never stored: the row keeps an argon2id hash and the
// 8-character prefix that finds it. Revoked, never deleted.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

const (
	// Scheme marks the key as Sewain's and live, so a leaked one is
	// recognisable in a code scanner.
	Scheme    = "swn_live_"
	prefixLen = 8
	secretLen = 32
	alphabet  = "abcdefghijklmnopqrstuvwxyz0123456789"
	// How long a verified key skips argon2id. Revocation is still read on
	// every request, so this only saves the hash, never extends a key's life.
	verifiedTTL = 10 * time.Minute
)

// ErrInvalid is any key that does not resolve: malformed, unknown, revoked.
// One answer for all three (invalid-api-key).
var ErrInvalid = errors.New("invalid api key")

type Service struct {
	store    *db.Store
	verified sync.Map // sha256(key) -> verifiedEntry
}

type verifiedEntry struct {
	keyID uuid.UUID
	until time.Time
}

func New(store *db.Store) *Service { return &Service{store: store} }

// Key is one key without its secret.
type Key struct {
	ID              uuid.UUID
	Name            string
	Prefix          string
	RateLimitPerMin int
	LastUsedAt      *time.Time
	RevokedAt       *time.Time
	CreatedAt       time.Time
}

// Resolved is a presented key that checked out: whose it is and its quota.
type Resolved struct {
	KeyID, OwnerID  uuid.UUID
	RateLimitPerMin int
}

// Create issues a key and returns its secret -- the only time it exists
// outside the caller's hands.
func (s *Service) Create(ctx context.Context, actor uuid.UUID, name string, perMin int) (Key, string, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Key{}, "", db.ErrNoOwnerContext
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 80 {
		return Key{}, "", apperrors.ValidationFailed("name is 1 to 80 characters.").
			WithFields(apperrors.Field{Name: "name"})
	}
	if perMin == 0 {
		perMin = 60
	}
	if perMin < 1 || perMin > 600 {
		return Key{}, "", apperrors.ValidationFailed("rate_limit_per_min is 1 to 600.").
			WithFields(apperrors.Field{Name: "rate_limit_per_min"})
	}
	for range 3 { // a prefix collision is 1 in 36^8 per key; retry rather than fail
		prefix, rest := random(prefixLen), random(secretLen)
		secret := Scheme + prefix + rest
		hash, err := auth.HashPassword(secret)
		if err != nil {
			return Key{}, "", err
		}
		var row sqlcgen.InsertApiKeyRow
		err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
			var err error
			row, err = sqlcgen.New(tx).InsertApiKey(ctx, sqlcgen.InsertApiKeyParams{
				ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, Name: name, KeyPrefix: prefix,
				KeyHash: hash, RateLimitPerMin: int32(perMin), CreatedBy: &actor, //nolint:gosec // 1..600
			})
			return err
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "api_keys_prefix_unique" {
			continue
		}
		if err != nil {
			return Key{}, "", fmt.Errorf("create api key: %w", err)
		}
		return Key{ID: row.ID, Name: row.Name, Prefix: row.KeyPrefix, RateLimitPerMin: int(row.RateLimitPerMin),
			LastUsedAt: row.LastUsedAt, RevokedAt: row.RevokedAt, CreatedAt: row.CreatedAt}, secret, nil
	}
	return Key{}, "", errors.New("create api key: three prefix collisions in a row")
}

func (s *Service) List(ctx context.Context) ([]Key, error) {
	var rows []sqlcgen.ListApiKeysRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListApiKeys(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	out := make([]Key, 0, len(rows))
	for _, r := range rows {
		out = append(out, Key{ID: r.ID, Name: r.Name, Prefix: r.KeyPrefix, RateLimitPerMin: int(r.RateLimitPerMin),
			LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// Revoke sets revoked_at. Another rental's id is a 404 (RLS hides the row).
func (s *Service) Revoke(ctx context.Context, id uuid.UUID) error {
	var n int64
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		n, err = sqlcgen.New(tx).RevokeApiKey(ctx, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if n == 0 {
		return apperrors.NotFound("No such API key in this business.")
	}
	return nil
}

// Resolve checks a presented key. The prefix lookup runs every time, so a
// revoked key fails on its next request; argon2id runs only when this process
// has not verified that exact key in the last 10 minutes.
func (s *Service) Resolve(ctx context.Context, presented string) (Resolved, error) {
	if len(presented) != len(Scheme)+prefixLen+secretLen || !strings.HasPrefix(presented, Scheme) {
		return Resolved{}, ErrInvalid
	}
	k, err := s.store.LookupAPIKey(ctx, presented[len(Scheme):len(Scheme)+prefixLen])
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolved{}, ErrInvalid
	}
	if err != nil {
		return Resolved{}, err
	}
	if k.Revoked {
		return Resolved{}, ErrInvalid
	}
	sum := sha256.Sum256([]byte(presented))
	if v, ok := s.verified.Load(sum); !ok || v.(verifiedEntry).keyID != k.ID || time.Now().After(v.(verifiedEntry).until) {
		if err := auth.VerifyPassword(k.KeyHash, presented); err != nil {
			return Resolved{}, ErrInvalid
		}
		s.verified.Store(sum, verifiedEntry{keyID: k.ID, until: time.Now().Add(verifiedTTL)})
	}
	// last_used_at, throttled in the query itself.
	_ = s.store.InOwnerTx(owner.NewContext(ctx, k.OwnerID), func(tx pgx.Tx) error {
		return sqlcgen.New(tx).TouchApiKey(ctx, k.ID)
	})
	return Resolved{KeyID: k.ID, OwnerID: k.OwnerID, RateLimitPerMin: k.RateLimitPerMin}, nil
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
