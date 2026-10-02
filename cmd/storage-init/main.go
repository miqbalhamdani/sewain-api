// Command storage-init prepares the object store: one private bucket and the
// 24h expiry on pending/ (BR-093). Idempotent. Locally it targets MinIO; for R2
// the bucket is made once by hand in the dashboard and this only applies the
// lifecycle rule.  (S1-033)
//
// CORS is not set here. MinIO allows any origin by default
// (MINIO_API_CORS_ALLOW_ORIGIN), which is what lets a browser PUT to a
// presigned URL locally. R2's CORS rule -- PUT from the backoffice origin,
// Content-Type header -- is part of S1-074's deploy checklist.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

func main() {
	s, err := storage.FromConfig()
	if err != nil {
		slog.Error("storage-init", "error", err)
		os.Exit(1)
	}
	if err := s.EnsureBucket(context.Background()); err != nil {
		slog.Error("storage-init", "error", err)
		os.Exit(1)
	}
	slog.Info("object store ready", "bucket", config.ObjectStoreBucket(), "endpoint", config.ObjectStoreURL())
}
