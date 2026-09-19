package httpapi

import (
	"net/http"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// requirePermission wraps a handler so it runs only for a caller whose role
// grants the permission.
//
// The check is at the handler boundary on purpose. Putting it deeper -- in a
// service, or in a query -- means every new call path has to remember it, and
// the one that forgets is a silent authorisation hole rather than a compile
// error.
//
// The 403 names the permission that was missing. That is the difference between
// "you cannot do this" and "you cannot do this yet, ask your owner for
// users:write", and flows.md 6 makes it an acceptance criterion.
// Nothing calls this yet, and that is correct rather than dead: every route in
// this phase is open to both roles. S1-009's PATCH /settings is the first
// owner-only endpoint and the first caller.
//
// It stays because it is the pattern CLAUDE.md mandates -- the permission check
// wraps the generated method at the handler boundary, never in a service and
// never in a query. Deleting it would mean the next person invents their own
// placement, and the wrong placement is a silent authorisation hole rather than
// a compile error.
//
//nolint:unused // first caller arrives with S1-009; see above.
func requirePermission(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := auth.RoleFromContext(r.Context())
		if !ok {
			// No role in the context means the request never passed
			// authentication. Fail closed rather than treating it as a role
			// with no permissions, which would report the wrong problem.
			writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
			return
		}
		if !auth.Can(role, permission) {
			writeError(w, r, apperrors.PermissionDenied(permission))
			return
		}
		next(w, r)
	}
}
