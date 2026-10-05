// Package storage is the one S3-compatible adapter: MinIO locally, Cloudflare R2
// in production, identical code -- only endpoint and credentials differ.  (S1-033)
//
// Three rules from CLAUDE.md and BR-093 live here:
//   - the server chooses every key, always prefixed with owner_id
//   - bytes never pass through the API: browsers PUT to a presigned URL whose
//     signature binds Content-Type and Content-Length, so the store itself
//     refuses anything else
//   - nothing is trusted until HEAD says it exists; only then is it copied out
//     of pending/ into its final prefix and its key written to a row
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Read-URL lifetimes per kind (BR-077, BR-085, S1-033).
const (
	HandoverPhotoTTL = time.Hour
	ExportTTL        = 15 * time.Minute
	IdentityPhotoTTL = 5 * time.Minute

	// UploadTTL: a 5 MB photo on a bad mobile network can take longer than
	// five minutes, and a URL that dies mid-upload hits exactly the parking-lot
	// flow (04-api-spec.md 2.2).
	UploadTTL = 10 * time.Minute

	MaxUploadBytes = 10 << 20
)

// ImageTypes are what a photo may be: handover and identity (BR-093).
var ImageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

// ProofTypes add PDF, for a transfer proof only -- a bank's e-statement is a PDF
// more often than a screenshot (04-api-spec.md 2.2, S1-046). Per kind, so a
// handover never accepts a PDF as a photo of a car.
var ProofTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "application/pdf": true}

// ProofTTL is how long a transfer-proof read URL lives.
const ProofTTL = 15 * time.Minute

type Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

func New(endpoint, region, bucket, accessKey, secretKey string) *Store {
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		// Path-style: MinIO needs it, R2 accepts it, and it keeps the bucket
		// out of DNS on a developer's machine.
		UsePathStyle: true,
	})
	return &Store{client: client, presign: s3.NewPresignClient(client), bucket: bucket}
}

// PendingKey is where a fresh upload lands: pending/<owner>/<uuid>. The 24h
// lifecycle rule on pending/ removes whatever is never committed.
func PendingKey(ownerID uuid.UUID) string {
	return "pending/" + ownerID.String() + "/" + uuid.Must(uuid.NewV7()).String()
}

// PresignPut signs a PUT that only accepts exactly this type and length.
func (s *Store) PresignPut(ctx context.Context, key, contentType string, length int64) (string, error) {
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		ContentType:   &contentType,
		ContentLength: &length,
	}, s3.WithPresignExpires(UploadTTL))
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return req.URL, nil
}

// PresignGet signs a read of one object for ttl. Objects are never public.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return req.URL, nil
}

// Object is what HEAD says about a key.
type Object struct {
	Size        int64
	ContentType string
}

// ErrNotFound is a key HEAD cannot find.
var ErrNotFound = errors.New("object not found")

func (s *Store) Head(ctx context.Context, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
			return Object{}, ErrNotFound
		}
		return Object{}, fmt.Errorf("head %s: %w", key, err)
	}
	return Object{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}, nil
}

// Put uploads bytes the server produced itself -- report exports (S1-057).
// Everything a person uploads goes browser -> presigned PUT instead (BR-093).
func (s *Store) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, ContentType: &contentType,
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Copy is store-to-store: zero bytes through this process.
func (s *Store) Copy(ctx context.Context, src, dst string) error {
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     &s.bucket,
		Key:        &dst,
		CopySource: aws.String(s.bucket + "/" + url.PathEscape(src)),
	})
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}

// Promote verifies one pending upload and copies it to its final prefix,
// returning the final key. field names the request field for the 422.
//
// Called before the transaction that writes the row, so the row only ever
// points at an object that exists. ponytail: if that transaction then fails,
// the copied object is orphaned under its final prefix; pending/ has a
// lifecycle, final prefixes do not. Add a sweep if orphans ever add up.
func (s *Store) Promote(ctx context.Context, ownerID uuid.UUID, pendingKey, finalPrefix, field string,
	allowed map[string]bool) (Object, string, error) {
	// The prefix check is the tenancy check: a key from another rental's
	// pending/ is answered exactly like a key that does not exist.
	if !strings.HasPrefix(pendingKey, "pending/"+ownerID.String()+"/") || strings.Contains(pendingKey, "..") {
		return Object{}, "", apperrors.UploadNotFound(field)
	}
	obj, err := s.Head(ctx, pendingKey)
	if errors.Is(err, ErrNotFound) {
		return Object{}, "", apperrors.UploadNotFound(field)
	}
	if err != nil {
		return Object{}, "", err
	}
	if !allowed[obj.ContentType] || obj.Size > MaxUploadBytes || obj.Size == 0 {
		return Object{}, "", apperrors.UploadTypeMismatch(field)
	}
	final := finalPrefix + "/" + uuid.Must(uuid.NewV7()).String()
	if err := s.Copy(ctx, pendingKey, final); err != nil {
		return Object{}, "", err
	}
	return obj, final, nil
}

// Ping is the signed health probe: HEAD on the bucket. Unlike the old
// unsigned /minio/health/live GET, it works against R2 too.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	return err
}

// EnsureBucket creates the private bucket if missing and (re)applies the
// pending/ 24h expiry. Idempotent; run by cmd/storage-init, never by the API.
func (s *Store) EnsureBucket(ctx context.Context) error {
	if s.Ping(ctx) != nil {
		_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket})
		var owned *types.BucketAlreadyOwnedByYou
		if err != nil && !errors.As(err, &owned) {
			return fmt.Errorf("create bucket: %w", err)
		}
	}
	_, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: &s.bucket,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID:         aws.String("expire-pending-uploads"),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("pending/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
		}, {
			// Report exports hold renter data; the 15-minute link is the
			// gate, and the file itself does not outlive the day (BR-077).
			ID:         aws.String("expire-exports"),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("exports/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
		}}},
	})
	if err != nil {
		return fmt.Errorf("pending/ lifecycle: %w", err)
	}
	return nil
}

// FromConfig builds the store from the environment -- one place, for the API,
// storage-init and every test that touches it.
func FromConfig() (*Store, error) {
	access, secret, err := config.ObjectStoreCredentials()
	if err != nil {
		return nil, err
	}
	return New(config.ObjectStoreURL(), config.ObjectStoreRegion(), config.ObjectStoreBucket(), access, secret), nil
}
