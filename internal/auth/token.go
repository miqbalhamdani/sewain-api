package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// API spec.md 1: 15-minute access token. Short enough that a leaked one is
	// briefly useful, long enough that refresh is not on every request.
	AccessTokenTTL = 15 * time.Minute

	// The refresh token is the long-lived credential. It lives only in an
	// httpOnly cookie and is rotated on every use.
	RefreshTokenTTL = 30 * 24 * time.Hour

	refreshTokenBytes = 32
)

// ErrInvalidToken covers every way an access token can fail to be usable:
// wrong signature, expired, malformed, wrong algorithm. Callers must not tell
// them apart to a client.
var ErrInvalidToken = errors.New("invalid access token")

// Claims is what an access token carries. The owner is in here and nowhere
// else -- API spec.md 1: derived from the token, never accepted from a header,
// query parameter or body.
type Claims struct {
	OwnerID uuid.UUID `json:"tid"`
	Role    string    `json:"role"`

	// EmailVerified gates the whole backoffice (BR-006). It rides in the token
	// for the same reason OwnerID and Role do: reading it from the database on
	// every request would double the query count of the cheapest endpoints,
	// and BR-004 already establishes the token as the carrier for facts that
	// come from the users row.
	//
	// The cost is that verifying takes effect on the next token, not the next
	// request -- so POST /auth/verify-email is followed by /auth/refresh. That
	// is one extra call on a once-per-account path, against one saved query on
	// every request forever.
	EmailVerified bool `json:"ev"`

	jwt.RegisteredClaims
}

// Signer issues and verifies access tokens.
type Signer struct {
	secret []byte
}

// NewSigner fails on a short secret rather than accepting one. HS256's security
// rests entirely on this value, and a deployment that starts with a weak one
// tends to keep it.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("JWT secret must be at least 32 bytes, got %d", len(secret))
	}
	return &Signer{secret: []byte(secret)}, nil
}

// Issue returns a signed access token for a user.
func (s *Signer) Issue(userID, ownerID uuid.UUID, role string, emailVerified bool, now time.Time) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		OwnerID:       ownerID,
		Role:          role,
		EmailVerified: emailVerified,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
	})
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return signed, nil
}

// Parse verifies a token and returns its claims.
func (s *Signer) Parse(raw string) (*Claims, error) {
	// The algorithm is pinned. Without this, a token whose header says "none"
	// -- or an RS256 token verified with the public key as an HMAC secret --
	// parses successfully. It is the classic JWT forgery.
	parsed, err := jwt.ParseWithClaims(raw, &Claims{},
		func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := parsed.Claims.(*Claims)
	if !ok || claims.OwnerID == uuid.Nil {
		return nil, ErrInvalidToken
	}
	if _, err := uuid.Parse(claims.Subject); err != nil {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// UserID returns the subject as a UUID. Parse has already validated it.
func (c *Claims) UserID() uuid.UUID { return uuid.MustParse(c.Subject) }

// NewRefreshToken returns a random token and the hash to store for it.
//
// Only the hash is persisted. The database is the thing most likely to be
// exfiltrated, and a stolen table of hashes is not a set of usable sessions.
// SHA-256 rather than argon2id is correct here: the input is 32 random bytes,
// not a human-chosen password, so there is no dictionary to slow an attacker
// down against.
func NewRefreshToken() (token, hash string, err error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken is how a presented token is matched to a stored row.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
