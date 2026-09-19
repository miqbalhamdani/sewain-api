package owner_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

func TestFromContext(t *testing.T) {
	id := uuid.Must(uuid.NewV7())

	t.Run("round trips", func(t *testing.T) {
		got, ok := owner.FromContext(owner.NewContext(t.Context(), id))
		if !ok {
			t.Fatal("no owner found in a context that carries one")
		}
		if got != id {
			t.Errorf("got %s, want %s", got, id)
		}
	})

	t.Run("reports absence rather than a zero value", func(t *testing.T) {
		got, ok := owner.FromContext(t.Context())
		if ok {
			t.Fatalf("found owner %s in an empty context", got)
		}
		// The zero UUID matches no rows. Returning it without the false would
		// read as "this owner owns nothing" instead of "there is no owner".
		if got != uuid.Nil {
			t.Errorf("got %s, want the zero UUID", got)
		}
	})

	t.Run("another package's key cannot reach the value", func(t *testing.T) {
		// contextKey is unexported, so this is the closest an outside package
		// can get: same underlying shape, different type, no match.
		type contextKey struct{}
		ctx := context.WithValue(t.Context(), contextKey{}, uuid.Must(uuid.NewV7()))

		if _, ok := owner.FromContext(ctx); ok {
			t.Error("a foreign key was read as the owner; the key type is not private enough")
		}
	})
}
