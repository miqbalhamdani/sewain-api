package auth

import (
	"slices"
	"sort"
)

// The role matrix, and the only definition of it (BR-003).
//
// Handlers, the login response and GET /me all read from here, so they cannot
// disagree. A second copy of "what may an operator do" is a second answer, and
// the one that drifts is the one nobody is looking at.
//
// Two roles, not five. `new-commerce` has owner/admin/ops/warehouse/viewer
// because a catalogue team has that many jobs; a rental has a juragan and the
// person at the counter. Adding a third is a contract change (BR-003), not a
// constant.
//
// Naming, since it reads oddly the first time: `owner` here is a ROLE. The
// business is also called an owner -- `owners`, `owner_id`, internal/owner.
// Both are fixed by the contract. `auth.RoleOwner` is what someone may do;
// `owner.FromContext` is which rental they do it in.
const (
	RoleOwner    = "owner"
	RoleOperator = "operator"
)

// Permissions are `resource:action`. They exist so the frontend can hide what a
// role cannot do rather than render it and let the server say no -- a disabled
// button advertises a capability and generates a support ticket (BR-003).
const (
	PermResourcesRead  = "resources:read"
	PermResourcesWrite = "resources:write"
	PermUnitsRead      = "units:read"
	PermUnitsWrite     = "units:write"
	PermCustomersRead  = "customers:read"
	PermCustomersWrite = "customers:write"
	PermBookingsRead   = "bookings:read"
	PermBookingsWrite  = "bookings:write"
	PermHandoversRead  = "handovers:read"
	PermHandoversWrite = "handovers:write"
	PermInvoicesRead   = "invoices:read"
	PermPaymentsWrite  = "payments:write"

	// Owner-only. Each one maps to a line in BR-003's "operator may not" list.
	PermPricingWrite      = "pricing:write"      // harga, deposit, denda
	PermReportsRead       = "reports:read"       // laporan keuangan
	PermUsersRead         = "users:read"         // mengelola pengguna
	PermUsersWrite        = "users:write"        //
	PermSettingsRead      = "settings:read"      //
	PermSettingsWrite     = "settings:write"     // pengaturan usaha
	PermSubscriptionRead  = "subscription:read"  // pengaturan langganan
	PermSubscriptionWrite = "subscription:write" //
	PermDelete            = "records:delete"     // menghapus data apa pun

	// BR-028: hanya pemilik yang bisa memblokir/membuka penyewa. Not one of
	// BR-003's lines, which is why it is its own permission rather than riding
	// on customers:write -- an operator has that, and must keep it.
	PermCustomersBlacklist = "customers:blacklist"

	// BR-051 as decided in M4: giving up a deposit is the owner's call.
	// Operators still waive late fees and damage at return, with a reason.
	PermDepositsWaive = "deposits:waive"
)

// operatorPermissions is BR-003's "operator boleh" list, verbatim: membuat &
// mengubah booking, memproses serah-terima, mencatat pembayaran, mengelola data
// penyewa. Reading the catalogue is implied by all four.
var operatorPermissions = []string{
	PermResourcesRead, PermUnitsRead,
	PermCustomersRead, PermCustomersWrite,
	PermBookingsRead, PermBookingsWrite,
	PermHandoversRead, PermHandoversWrite,
	PermInvoicesRead, PermPaymentsWrite,
}

// ownerPermissions is everything an operator has, plus the five things BR-003
// says an operator may not do.
//
// Note what `records:delete` being owner-only means in practice: an operator
// cannot delete anything at all. That is deliberate and it is the whole reason
// a juragan is willing to hand out an account.
var ownerPermissions = concat(operatorPermissions, []string{
	PermResourcesWrite, PermUnitsWrite,
	PermPricingWrite, PermReportsRead,
	PermUsersRead, PermUsersWrite,
	PermSettingsRead, PermSettingsWrite,
	PermSubscriptionRead, PermSubscriptionWrite,
	PermDelete, PermCustomersBlacklist, PermDepositsWaive,
})

var rolePermissions = map[string][]string{
	RoleOwner:    ownerPermissions,
	RoleOperator: operatorPermissions,
}

// PermissionsFor returns the permissions a role grants, sorted, as a copy.
//
// An unknown role gets an empty slice rather than a nil or a panic: a role that
// is not in the matrix can do nothing, which is the safe direction. The CHECK
// constraint on users.role means this should be unreachable, and "should be
// unreachable" is not a reason to fail open.
func PermissionsFor(role string) []string {
	perms, ok := rolePermissions[role]
	if !ok {
		return []string{}
	}
	out := slices.Clone(perms)
	sort.Strings(out)
	return out
}

// Can reports whether a role grants a permission.
func Can(role, permission string) bool {
	return slices.Contains(rolePermissions[role], permission)
}

// SeededRole is a role as the UI shows it in a picker.
type SeededRole struct {
	Name        string
	Description string
	Permissions []string
}

// SeededRoles returns both roles, in the order a picker should show them.
func SeededRoles() []SeededRole {
	return []SeededRole{
		{
			Name:        RoleOwner,
			Description: "Juragan. Akses penuh, termasuk harga, laporan, pengguna, dan langganan.",
			Permissions: PermissionsFor(RoleOwner),
		},
		{
			Name:        RoleOperator,
			Description: "Petugas harian. Booking, serah-terima, pembayaran, dan data penyewa. Tidak melihat laporan keuangan dan tidak bisa menghapus apa pun.",
			Permissions: PermissionsFor(RoleOperator),
		},
	}
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}
