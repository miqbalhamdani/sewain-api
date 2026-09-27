package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "test-signing-key-at-least-32-bytes-long"

func TestSigner(t *testing.T) {
	signer, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	userID, ownerID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now()

	t.Run("round trips the owner and user", func(t *testing.T) {
		raw, err := signer.Issue(userID, ownerID, RoleOwner, true, now)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		claims, err := signer.Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if claims.OwnerID != ownerID {
			t.Errorf("owner = %s, want %s", claims.OwnerID, ownerID)
		}
		if claims.UserID() != userID {
			t.Errorf("user = %s, want %s", claims.UserID(), userID)
		}
		if claims.Role != RoleOwner {
			t.Errorf("role = %q, want %q", claims.Role, RoleOwner)
		}
	})

	t.Run("rejects a token signed with another key", func(t *testing.T) {
		other, err := NewSigner("a-completely-different-key-32-bytes-x")
		if err != nil {
			t.Fatalf("new signer: %v", err)
		}
		raw, err := other.Issue(userID, ownerID, RoleOwner, true, now)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := signer.Parse(raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("got %v, want ErrInvalidToken", err)
		}
	})

	t.Run("rejects an expired token", func(t *testing.T) {
		raw, err := signer.Issue(userID, ownerID, RoleOwner, true, now.Add(-2*AccessTokenTTL))
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := signer.Parse(raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("got %v, want ErrInvalidToken", err)
		}
	})

	// The classic JWT forgery: a token whose header claims no signature was
	// needed. A parser that trusts the header accepts anything.
	t.Run("rejects alg=none", func(t *testing.T) {
		unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
			OwnerID: ownerID,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   userID.String(),
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
		}).SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatalf("build unsigned token: %v", err)
		}
		if _, err := signer.Parse(unsigned); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("got %v, want ErrInvalidToken -- an unsigned token was accepted", err)
		}
	})

	// HS256's security is the secret. A short one is brute-forceable offline
	// from a single captured token.
	t.Run("refuses a short secret at construction", func(t *testing.T) {
		if _, err := NewSigner("too-short"); err == nil {
			t.Error("NewSigner accepted a 9-byte secret")
		}
	})
}

func TestRefreshToken(t *testing.T) {
	t.Run("hashes deterministically and uniquely", func(t *testing.T) {
		token, hash, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if HashRefreshToken(token) != hash {
			t.Error("hashing the returned token does not reproduce the returned hash")
		}
		if strings.Contains(hash, token) {
			t.Error("the hash contains the token itself")
		}

		other, _, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if other == token {
			t.Error("two refresh tokens are identical; they are not random")
		}
	})
}
