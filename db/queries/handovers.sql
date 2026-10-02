-- Bukti kondisi.  (S1-034 .. S1-036)
--
-- INSERT and SELECT only. There is no UPDATE or DELETE query here and there
-- cannot be one that works: 000014 revokes both from app_user (BR-037).

-- name: InsertHandover :exec
INSERT INTO handovers (id, owner_id, booking_id, direction, performed_by, meter_value,
                       checklist, condition_notes, late_fee_waived, waiver_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertHandoverPhoto :exec
INSERT INTO handover_photos (id, owner_id, handover_id, object_key) VALUES ($1, $2, $3, $4);

-- name: ListHandovers :many
SELECT h.id, h.direction, u.name AS performed_by, h.performed_at, h.meter_value,
       h.checklist, h.condition_notes, h.late_fee_waived, h.waiver_reason
  FROM handovers h
  JOIN users u ON u.id = h.performed_by
 WHERE h.booking_id = $1
 ORDER BY h.performed_at;

-- name: ListHandoverPhotos :many
SELECT p.id, p.handover_id, p.object_key, p.captured_at
  FROM handover_photos p
  JOIN handovers h ON h.id = p.handover_id
 WHERE h.booking_id = $1
 ORDER BY p.captured_at, p.id;

-- name: HandoverExists :one
SELECT EXISTS (SELECT 1 FROM handovers WHERE id = $1);
