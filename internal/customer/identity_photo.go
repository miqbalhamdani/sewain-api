package customer

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

// Identity photos.  (S1-021, closed on S1-033's storage)
//
// Not app-encrypted: the signed URL has to render straight in a browser, so
// R2's at-rest encryption is the layer (BR-085). What the app adds is the
// five-minute link and one audit row per opening, in the same transaction.

// SetIdentityPhoto verifies the upload, copies it under identity/, and stores
// the key with its document type. A replaced photo's old object stays in the
// store; nothing reads it any more.
func (s *Service) SetIdentityPhoto(ctx context.Context, id uuid.UUID, pendingKey, idType string) (Customer, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Customer{}, db.ErrNoOwnerContext
	}
	if _, err := s.Get(ctx, id); err != nil {
		return Customer{}, err
	}
	_, final, err := s.objects.Promote(ctx, ownerID, pendingKey,
		"identity/"+ownerID.String()+"/"+id.String(), "object_key", storage.ImageTypes)
	if err != nil {
		return Customer{}, err
	}
	var row sqlcgen.GetCustomerRow
	var found int64
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if found, err = q.SetCustomerIdentityPhoto(ctx, sqlcgen.SetCustomerIdentityPhotoParams{
			ID: id, IDPhotoKey: &final, IDType: &idType,
		}); err != nil || found == 0 {
			return err
		}
		row, err = q.GetCustomer(ctx, id)
		return err
	})
	if err != nil {
		return Customer{}, translate(err, "set identity photo")
	}
	if found == 0 {
		return Customer{}, notFound()
	}
	return customerOf(row), nil
}

// ViewIdentityPhoto returns a five-minute URL and writes the audit row in the
// same transaction: the URL is only signed once the row is committed, so there
// is no opening that is not on the record (BR-085).
func (s *Service) ViewIdentityPhoto(ctx context.Context, actorID, id uuid.UUID) (string, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return "", db.ErrNoOwnerContext
	}
	var key string
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		k, err := q.GetCustomerPhotoKey(ctx, id)
		if err != nil {
			return err
		}
		if k == nil {
			return pgx.ErrNoRows
		}
		key = *k
		return q.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
			ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, ActorUserID: actorID,
			Action: "customer.identity.viewed", Entity: "customer", EntityID: id, Metadata: []byte(`{}`),
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", notFound()
	}
	if err != nil {
		return "", fmt.Errorf("view identity photo: %w", err)
	}
	return s.objects.PresignGet(ctx, key, storage.IdentityPhotoTTL)
}
