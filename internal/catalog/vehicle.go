package catalog

import (
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
)

// Atribut kendaraan.  (S1-085, BR-094)
//
// Dua tabel pendamping 1:1, bukan kolom di resources dan bukan jsonb. Alasan
// lengkapnya di BR-094 dan di 000010_vehicle_details.up.sql; yang perlu dipegang
// saat ngoding cuma dua:
//
//  1. Preset `vehicle_rental` WAJIB punya spek, preset lain DILARANG punya.
//     Kewajiban itu dijaga paket ini, bukan database -- "anak wajib ada" tidak
//     bisa ditegakkan dengan murah. Yang dijaga database adalah kebalikannya:
//     anak tidak bisa menunjuk induk pemilik lain (FK komposit, BR-001).
//  2. `VehicleType` dikunci sesudah resource dibuat, jadi ia tidak ada di
//     `VehicleSpecPatch` sama sekali.

// VehicleSpec is one resource's vehicle attributes.
type VehicleSpec struct {
	VehicleType  string
	Transmission string

	// Seats is non-nil exactly when VehicleType is "car". The database
	// enforces both halves with a single equality CHECK, so a motorcycle that
	// arrives carrying seats is refused rather than quietly stripped.
	Seats *int32
	Fuel  string
}

// VehicleSpecPatch replaces the whole nested object.
//
// Not a field-by-field COALESCE, and that is deliberate: `vehicle` is a nested
// object the form renders whole, so what the client sends is what the resource
// should have. A car that forgets to send Seats violates
// vehicle_specs_seats_car and gets a 422 -- rather than silently keeping a value
// the person believed they had cleared.
//
// VehicleType is absent because it is locked (BR-094).
type VehicleSpecPatch struct {
	Transmission string
	Seats        *int32
	Fuel         string
}

// VehicleUnitDetail is one physical unit's vehicle attributes.
//
// None of these ever reaches the public surface. `resource_units.code` is the
// number plate and BR-025 already bars it; the tax and registration dates are
// the owner's notes about their own fleet.
type VehicleUnitDetail struct {
	Year                   int32
	Color                  *string
	TaxDueOn               *time.Time
	RegistrationValidUntil *time.Time
}

// vehicleSpecOf reads the LEFT JOIN half of a resource row.
//
// nil when the join found nothing, which is the normal state for every preset
// that is not vehicle_rental. VehicleType being non-nil is the marker: the
// column is NOT NULL in its own table, so it can only be nil here because the
// join missed.
func vehicleSpecOf(row sqlcgen.GetResourceRow) *VehicleSpec {
	if row.VehicleType == nil {
		return nil
	}
	return &VehicleSpec{
		VehicleType:  *row.VehicleType,
		Transmission: derefOr(row.Transmission),
		Seats:        row.Seats,
		Fuel:         derefOr(row.Fuel),
	}
}

// vehicleUnitDetailOf reads the LEFT JOIN half of a unit row. Year is the
// marker, for the same reason VehicleType is above.
func vehicleUnitDetailOf(row sqlcgen.GetUnitRow) *VehicleUnitDetail {
	if row.Year == nil {
		return nil
	}
	return &VehicleUnitDetail{
		Year:                   *row.Year,
		Color:                  row.Color,
		TaxDueOn:               row.TaxDueOn,
		RegistrationValidUntil: row.RegistrationValidUntil,
	}
}

func derefOr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
