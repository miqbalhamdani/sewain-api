package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	const password = "correct horse battery staple"

	t.Run("verifies the password it hashed", func(t *testing.T) {
		hash, err := HashPassword(password)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if err := VerifyPassword(hash, password); err != nil {
			t.Errorf("verify: %v", err)
		}
	})

	t.Run("rejects a wrong password", func(t *testing.T) {
		hash, err := HashPassword(password)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if err := VerifyPassword(hash, password+"!"); !errors.Is(err, ErrMismatch) {
			t.Errorf("got %v, want ErrMismatch", err)
		}
	})

	// A per-hash salt is what stops one precomputed table from breaking every
	// account that chose the same password.
	t.Run("hashes the same password differently every time", func(t *testing.T) {
		a, err := HashPassword(password)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		b, err := HashPassword(password)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if a == b {
			t.Error("two hashes of the same password are identical; the salt is not random")
		}
	})

	// The parameters are read back out of the stored hash, so raising the cost
	// later does not invalidate everyone's password.
	t.Run("carries its parameters", func(t *testing.T) {
		hash, err := HashPassword(password)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		for _, want := range []string{"$argon2id$", "m=65536", "t=3", "p=4"} {
			if !strings.Contains(hash, want) {
				t.Errorf("hash %q does not carry %q", hash, want)
			}
		}
	})

	t.Run("refuses a malformed hash rather than accepting it", func(t *testing.T) {
		for _, bad := range []string{
			"",
			"not-a-hash",
			"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0",  // wrong variant
			"$argon2id$v=99$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0", // wrong version
			"$argon2id$v=19$nonsense$c2FsdA$ZGlnZXN0",
		} {
			if err := VerifyPassword(bad, password); err == nil {
				t.Errorf("VerifyPassword(%q) = nil, want an error", bad)
			}
		}
	})

	// The service verifies against this when there is no user, so that an
	// unknown email costs the same as a known one.
	t.Run("the constant-time dummy hash is well formed", func(t *testing.T) {
		if err := VerifyPassword(dummyHash, "anything"); !errors.Is(err, ErrMismatch) {
			t.Errorf("got %v, want ErrMismatch -- the dummy hash must verify like a real one", err)
		}
	})
}
