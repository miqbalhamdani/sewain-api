// Package errors is the error taxonomy every handler answers with.
//
// It defines what an error *is* -- a code, a status, and something a person can
// read -- and deliberately not how it is written to the wire. The wire shape is
// RFC 9457 and lives in internal/http, which owns the generated Problem type;
// nothing imports internal/http, so the taxonomy cannot live there.
package errors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// The canonical codes for this phase (API spec.md 1.1). The code is also the
// last segment of the type URI a client receives.
const (
	CodeValidationFailed = "validation-failed"
	CodePermissionDenied = "permission-denied"
	CodeNotFound         = "not-found"
	CodeRateLimited      = "rate-limited"
	CodeUnauthenticated  = "unauthenticated"
	CodeInternal         = "internal"

	// Business-rule codes. Each one is added by the backlog item that owns it,
	// and each one is in ../docs/04-api-spec.md section 2 -- the catalogue here
	// and the catalogue there are the same list or S1-011 is not done.
	CodeEmailTaken  = "email-taken"  // BR-004, S1-010
	CodeSlugTaken   = "slug-taken"   // BR-025, S1-009
	CodeSlugInvalid = "slug-invalid" // BR-025, S1-009

	CodeEmailNotVerified         = "email-not-verified"         // BR-006, S1-084
	CodeVerificationTokenInvalid = "verification-token-invalid" // BR-006, S1-084
	CodeRequestInFlight          = "request-in-flight"          // BR-090, S1-012

	CodeBookingConflict     = "booking-conflict"      // BR-022, S1-022
	CodeDurationOutOfRange  = "duration-out-of-range" // BR-021, S1-026
	CodeCustomerBlacklisted = "customer-blacklisted"  // BR-028, S1-026
	CodeUnitNotSwappable    = "unit-not-swappable"    // BR-029, S1-027

	CodeEvidenceImmutable           = "evidence-immutable"             // BR-037, S1-034
	CodeUploadNotFound              = "upload-not-found"               // BR-093, S1-033
	CodeUploadTypeMismatch          = "upload-type-mismatch"           // BR-093, S1-033
	CodeHandoverPhotoRequired       = "handover-photo-required"        // BR-036, S1-035
	CodeMeterValueRequired          = "meter-value-required"           // BR-036, S1-035
	CodePaymentRequiredBeforePickup = "payment-required-before-pickup" // BR-038, S1-035
	CodePhysicalConflictUnconfirmed = "physical-conflict-unconfirmed"  // BR-042, S1-035
	CodeWaiverReasonRequired        = "waiver-reason-required"         // BR-051, S1-036

	CodeDepositNotSettled    = "deposit-not-settled"    // BR-049, S1-042
	CodeDepositNotCollected  = "deposit-not-collected"  // BR-048, S1-042
	CodeDepositAlreadyPaid   = "deposit-already-paid"   // BR-051, S1-042
	CodeDepositNotApplicable = "deposit-not-applicable" // BR-016, S1-042
	CodeInvoiceAlreadyPaid   = "invoice-already-paid"   // BR-060, S1-044

	CodeInvalidAPIKey    = "invalid-api-key"    // BR-031, S1-080
	CodeOriginNotAllowed = "origin-not-allowed" // BR-031, S1-080
	CodeQuotaExceeded    = "quota-exceeded"     // BR-031, S1-080
)

// Conflict is one booking that holds a unit over the range somebody asked for.
// It rides on booking-conflict so the screen can point at it rather than say
// "failed" (04-api-spec.md section 2).
type Conflict struct {
	Code    string
	StartAt time.Time
	EndAt   time.Time
	Status  string
}

// Field is one entry in a Problem's errors array: which field, and what about
// it. Extra carries whatever the specific failure needs -- a version conflict
// reports expected and supplied.
type Field struct {
	Name   string
	Detail string
	Extra  map[string]any
}

// Error is a failure a client is allowed to see.
//
// Anything that is not one of these is an internal error: the detail on those
// is fixed and uninformative on purpose, because the alternative is leaking a
// database message or a file path into a response.
type Error struct {
	Code   string
	Status int
	Title  string
	Detail string
	Fields []Field

	// Conflicts is set only on booking-conflict.
	Conflicts []Conflict

	// cause is logged, never sent. It is what makes a trace_id worth quoting:
	// the id reaches a support ticket, this reaches the operator.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.cause)
	}
	return e.Code + ": " + e.Detail
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches the underlying failure for logging. It never reaches the
// client.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

// WithFields attaches field-level detail.
func (e *Error) WithFields(fields ...Field) *Error {
	e.Fields = append(e.Fields, fields...)
	return e
}

func Unauthenticated(detail string) *Error {
	return &Error{Code: CodeUnauthenticated, Status: http.StatusUnauthorized,
		Title: "Unauthenticated", Detail: detail}
}

// PermissionDenied names the permission that was required.
//
// That naming is an acceptance criterion, not a nicety (flows.md 6): it is the
// difference between "you cannot do this" and "ask your owner for users:write".
func PermissionDenied(permission string) *Error {
	return &Error{Code: CodePermissionDenied, Status: http.StatusForbidden,
		Title:  "Permission denied",
		Detail: "This action requires the " + permission + " permission."}
}

func NotFound(detail string) *Error {
	return &Error{Code: CodeNotFound, Status: http.StatusNotFound,
		Title: "Not found", Detail: detail}
}

func ValidationFailed(detail string) *Error {
	return &Error{Code: CodeValidationFailed, Status: http.StatusUnprocessableEntity,
		Title: "Validation failed", Detail: detail}
}

// EmailTaken is a 422, not a 409: the address is unique across the whole system
// (BR-004), so the caller has to supply a different one rather than retry.
//
// It says nothing about which rental holds the address. Answering "does this
// person have an account here" for an unauthenticated guess is the same leak
// login already refuses to make.
func EmailTaken() *Error {
	return (&Error{Code: CodeEmailTaken, Status: http.StatusUnprocessableEntity,
		Title:  "Email already in use",
		Detail: "That email address already has an account."}).
		WithFields(Field{Name: "email"})
}

func SlugTaken() *Error {
	return (&Error{Code: CodeSlugTaken, Status: http.StatusUnprocessableEntity,
		Title:  "Subdomain already taken",
		Detail: "That subdomain is already in use by another business."}).
		WithFields(Field{Name: "slug"})
}

// SlugInvalid covers all three schema constraints at once -- shape, punycode,
// and the reserved list -- because the caller's next move is the same for each:
// pick a different label.
func SlugInvalid(detail string) *Error {
	return (&Error{Code: CodeSlugInvalid, Status: http.StatusUnprocessableEntity,
		Title: "Subdomain not allowed", Detail: detail}).
		WithFields(Field{Name: "slug"})
}

// EmailNotVerified is the gate on the whole backoffice (BR-006).
//
// 403 rather than 401: the caller is authenticated, the session is real, and
// re-logging-in would change nothing. What is missing is a proof about the
// address, and the client redirects to the verification wall on this code
// rather than to the login screen.
func EmailNotVerified() *Error {
	return &Error{Code: CodeEmailNotVerified, Status: http.StatusForbidden,
		Title: "Email not verified",
		Detail: "Verify your email address before using this. " +
			"Check your inbox, or request a new link."}
}

// RateLimited is the 429. The detail says when to come back, because a limit
// with no stated window reads as a permanent refusal.
func RateLimited(detail string) *Error {
	return &Error{Code: CodeRateLimited, Status: http.StatusTooManyRequests,
		Title: "Too many requests", Detail: detail}
}

// InvalidAPIKey is a key on api.<apex> that is unknown, malformed or revoked
// -- one answer for the three (BR-031).
func InvalidAPIKey() *Error {
	return &Error{Code: CodeInvalidAPIKey, Status: http.StatusUnauthorized,
		Title: "Invalid API key", Detail: "The X-API-Key is unknown or has been revoked."}
}

// OriginNotAllowed is a browser Origin outside the owner's allowed_origins.
// A browser control, not a security one -- curl never sends it (BR-031).
func OriginNotAllowed() *Error {
	return &Error{Code: CodeOriginNotAllowed, Status: http.StatusForbidden,
		Title:  "Origin not allowed",
		Detail: "This Origin is not in the business's allowed origins. Add it on the API keys screen."}
}

// QuotaExceeded is a key past its per-minute quota (BR-031).
func QuotaExceeded(detail string) *Error {
	return &Error{Code: CodeQuotaExceeded, Status: http.StatusTooManyRequests,
		Title: "API key quota exceeded", Detail: detail}
}

// RequestInFlight is the answer to a repeated Idempotency-Key whose first
// request has not finished yet (BR-090).
//
// 409, and the detail says wait rather than retry. A client that retries with
// a NEW key on seeing this has thrown away the entire point: the second key is
// a second intent, and the booking it creates is the duplicate the header
// existed to prevent.
func RequestInFlight() *Error {
	return &Error{Code: CodeRequestInFlight, Status: http.StatusConflict,
		Title: "Request already in flight",
		Detail: "An identical request is still being processed. Wait for it to finish; " +
			"do not send it again with a new Idempotency-Key."}
}

// BookingConflict is the answer to a unit already held on that range (BR-022).
//
// 409, and the same answer whether the app's pre-check caught it or the
// exclusion constraint did after losing a race -- two operators pressing save
// in the same second get exactly one success and this, never a 500.
func BookingConflict(conflicts []Conflict) *Error {
	return &Error{Code: CodeBookingConflict, Status: http.StatusConflict,
		Title:     "Unit already booked on that range",
		Detail:    "That unit is already held by another booking over this time. Pick another unit or another time.",
		Conflicts: conflicts}
}

// NothingAvailable is the public surface's booking-conflict: no unit of that
// resource is free over the range. Same code, but never a conflicts list --
// other renters' booking codes do not leave the backoffice (BR-025).
func NothingAvailable() *Error {
	return &Error{Code: CodeBookingConflict, Status: http.StatusConflict,
		Title:  "Nothing available on that range",
		Detail: "No unit is free over that time. Pick another time."}
}

// DurationOutOfRange names the bound that was crossed, in the resource's own
// pricing unit (BR-021).
func DurationOutOfRange(detail string) *Error {
	return (&Error{Code: CodeDurationOutOfRange, Status: http.StatusUnprocessableEntity,
		Title: "Duration out of range", Detail: detail}).
		WithFields(Field{Name: "end_at"})
}

// CustomerBlacklisted refuses a new booking for a blocked renter (BR-028). The
// reason is not in the detail: the backoffice reads it from the customer, and
// the public page must never see it.
func CustomerBlacklisted() *Error {
	return (&Error{Code: CodeCustomerBlacklisted, Status: http.StatusUnprocessableEntity,
		Title:  "Customer is blacklisted",
		Detail: "This customer is blocked from new bookings."}).
		WithFields(Field{Name: "customer_id"})
}

// CustomerBlacklistedPublic is BR-028 on the public page: the same code, but a
// neutral message and no field -- the renter is told to contact the business,
// never that or why they are blocked (04-api-spec.md §4).
func CustomerBlacklistedPublic() *Error {
	return &Error{Code: CodeCustomerBlacklisted, Status: http.StatusUnprocessableEntity,
		Title:  "Request cannot be processed",
		Detail: "This request cannot be processed. Please contact the business."}
}

// UnitNotSwappable is a swap attempted after the unit has left, or on a booking
// that no longer holds one (BR-029).
func UnitNotSwappable(status string) *Error {
	return &Error{Code: CodeUnitNotSwappable, Status: http.StatusConflict,
		Title:  "Unit cannot be swapped",
		Detail: "Only a reserved booking can change unit; this one is " + status + "."}
}

// EvidenceImmutable is the only answer PATCH or DELETE on a handover ever gets,
// for every role including the owner (BR-037).
func EvidenceImmutable() *Error {
	return &Error{Code: CodeEvidenceImmutable, Status: http.StatusMethodNotAllowed,
		Title:  "Evidence cannot be changed",
		Detail: "Handover records and their photos are append-only. Add a note instead of changing one."}
}

// UploadNotFound covers a key that does not exist, was never uploaded, or does
// not carry this rental's prefix -- one answer, so a guessed key learns nothing
// about another rental's objects (BR-001, BR-093).
func UploadNotFound(field string) *Error {
	return (&Error{Code: CodeUploadNotFound, Status: http.StatusUnprocessableEntity,
		Title:  "Upload not found",
		Detail: "That upload does not exist or does not belong to this business. Upload the photo again."}).
		WithFields(Field{Name: field})
}

func UploadTypeMismatch(field string) *Error {
	return (&Error{Code: CodeUploadTypeMismatch, Status: http.StatusUnprocessableEntity,
		Title:  "Upload type not allowed",
		Detail: "That upload is not an allowed image type or is larger than 10 MB."}).
		WithFields(Field{Name: field})
}

func HandoverPhotoRequired() *Error {
	return (&Error{Code: CodeHandoverPhotoRequired, Status: http.StatusUnprocessableEntity,
		Title:  "Photo required",
		Detail: "A handover needs at least one photo of the unit's condition."}).
		WithFields(Field{Name: "photo_keys"})
}

func MeterValueRequired() *Error {
	return (&Error{Code: CodeMeterValueRequired, Status: http.StatusUnprocessableEntity,
		Title:  "Odometer required",
		Detail: "A vehicle handover needs the odometer reading."}).
		WithFields(Field{Name: "meter_value"})
}

func PaymentRequiredBeforePickup() *Error {
	return &Error{Code: CodePaymentRequiredBeforePickup, Status: http.StatusConflict,
		Title:  "Payment required before pickup",
		Detail: "This business requires the rent invoice to be paid before the unit leaves."}
}

// PhysicalConflictUnconfirmed carries the booking whose unit is still out, in
// the same conflicts[] shape booking-conflict uses (BR-042).
func PhysicalConflictUnconfirmed(conflicts []Conflict) *Error {
	return &Error{Code: CodePhysicalConflictUnconfirmed, Status: http.StatusConflict,
		Title:     "Unit has not come back yet",
		Detail:    "The previous booking for this unit is past its end and has not been returned. Confirm that the unit is physically here to continue.",
		Conflicts: conflicts}
}

func WaiverReasonRequired() *Error {
	return (&Error{Code: CodeWaiverReasonRequired, Status: http.StatusUnprocessableEntity,
		Title:  "Waiver reason required",
		Detail: "Waiving any part of a late fee needs a reason."}).
		WithFields(Field{Name: "waiver_reason"})
}

func DepositNotSettled() *Error {
	return &Error{Code: CodeDepositNotSettled, Status: http.StatusConflict,
		Title:  "Deposit not settled",
		Detail: "Settle the deposit -- refund, deduct, or both -- before completing this booking."}
}

// DepositNotCollected: the invoice carrying the deposit is not paid, so there is
// nothing in the owner's hands to refund or deduct from (BR-048).
func DepositNotCollected() *Error {
	return &Error{Code: CodeDepositNotCollected, Status: http.StatusConflict,
		Title:  "Deposit not collected",
		Detail: "The deposit has not been paid yet. Record its payment first, or waive it."}
}

func DepositAlreadyPaid() *Error {
	return &Error{Code: CodeDepositAlreadyPaid, Status: http.StatusConflict,
		Title:  "Deposit already paid",
		Detail: "A paid deposit cannot be waived; it is refunded at settlement instead."}
}

func DepositNotApplicable() *Error {
	return &Error{Code: CodeDepositNotApplicable, Status: http.StatusConflict,
		Title:  "No deposit to act on",
		Detail: "This booking has no deposit, or it was waived."}
}

// InvoiceAlreadyPaid is the second successful payment on one invoice -- whether
// it arrived second or lost a race at payments_one_success_per_invoice (BR-060).
func InvoiceAlreadyPaid() *Error {
	return &Error{Code: CodeInvoiceAlreadyPaid, Status: http.StatusConflict,
		Title:  "Invoice already paid",
		Detail: "This invoice already has a successful payment."}
}

// Internal wraps anything the client has no business seeing.
//
// The detail is fixed. A database error, a connection string or a file path
// reaching a response body is a disclosure, and the useful version of that
// information goes to the log alongside the trace id instead.
func Internal(cause error) *Error {
	return &Error{Code: CodeInternal, Status: http.StatusInternalServerError,
		Title: "Internal error", Detail: "Something went wrong on our side.",
		cause: cause}
}

// From turns any error into one a client may see, so a handler can pass
// whatever it has and still not leak.
func From(err error) *Error {
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	return Internal(err)
}

// TraceID returns the OpenTelemetry trace id of the current span.
//
// This is the whole of "resolvable to a span": the id in a response is the id
// of the span that produced it, so a support ticket quoting one lands on the
// request that failed. It is empty only when there is no recording span, which
// means telemetry was never started -- possible in a unit test, and a
// misconfiguration anywhere else.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}
