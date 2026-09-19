// Package owner carries the current owner through a request.
//
// The owner is derived from the caller's token and nothing else -- never a
// header, a query parameter or a request body (API spec.md 1). Accepting it
// from the request would make cross-owner access a matter of editing a header.
//
// P1-011's auth middleware is what puts a value in here. Until then the only
// callers are tests.
package owner

import (
	"context"

	"github.com/google/uuid"
)

// contextKey is unexported and of a type declared here, so no other package can
// construct an equal key. A plain string key would let any package -- or any
// dependency -- overwrite the owner by accident or on purpose.
type contextKey struct{}

var key contextKey

// NewContext returns a copy of ctx carrying id as the current owner.
func NewContext(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, key, id)
}

// FromContext returns the owner carried by ctx.
//
// The boolean is the whole safety property: callers must decide what to do when
// there is no owner, and db.InOwnerTx decides to refuse. There is deliberately
// no variant that returns a zero UUID and lets the caller carry on -- a zero
// owner matches no rows, which reads as "empty table" rather than as a bug.
func FromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(key).(uuid.UUID)
	return id, ok
}
