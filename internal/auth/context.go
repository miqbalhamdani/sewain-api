package auth

import (
	"context"

	"github.com/google/uuid"
)

// roleKey is unexported and of a type declared here, so no other package can
// construct an equal key and put a role of its own choosing into a request.
type roleKey struct{}

// NewRoleContext returns a copy of ctx carrying the caller's role.
//
// Set once, by the authentication middleware, from the verified access token.
// Never from anything a client sends directly.
func NewRoleContext(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleKey{}, role)
}

// RoleFromContext returns the caller's role.
//
// The boolean distinguishes "no role" from "a role with no permissions". The
// first means the request was never authenticated and should be a 401; the
// second is a real 403.
func RoleFromContext(ctx context.Context) (string, bool) {
	role, ok := ctx.Value(roleKey{}).(string)
	return role, ok
}

// userKey carries the authenticated user's id, for handlers that need to know
// who rather than only which rental and what role.
//
// GetMe is the first of those. It stays in the context rather than being
// re-parsed from the Authorization header in each handler: the token is
// verified exactly once, in the middleware, and anything that re-reads it is a
// second place that could get the verification wrong.
type userKey struct{}

// NewUserContext returns a copy of ctx carrying the caller's user id.
//
// Set once, by the authentication middleware, from the verified access token.
func NewUserContext(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

// UserFromContext returns the caller's user id.
func UserFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userKey{}).(uuid.UUID)
	return id, ok
}
