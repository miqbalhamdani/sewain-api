package storage

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// S1-033, against the real local MinIO -- no outside account (backlog).
// Needs `make storage-init` once.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := FromConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("object store not reachable: %v\n\nIs MinIO running, and was `make storage-init` run?", err)
	}
	return s
}

func put(t *testing.T, url, contentType string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

func TestPresignedUploadBindsTypeAndLength(t *testing.T) {
	s := newStore(t)
	owner := uuid.Must(uuid.NewV7())
	body := bytes.Repeat([]byte{0xff}, 1024)

	key := PendingKey(owner)
	url, err := s.PresignPut(t.Context(), key, "image/jpeg", int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}

	if code := put(t, url, "image/png", body); code < 400 {
		t.Errorf("PUT with another Content-Type = %d, want refused by the signature", code)
	}
	if code := put(t, url, "image/jpeg", append(body, 0)); code < 400 {
		t.Errorf("PUT with another length = %d, want refused by the signature", code)
	}
	if code := put(t, url, "image/jpeg", body); code != http.StatusOK {
		t.Fatalf("PUT as signed = %d, want 200", code)
	}

	// Objects are never public: an unsigned GET is refused.
	res, err := http.Get(strings.Split(url, "?")[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode < 400 {
		t.Errorf("unsigned GET = %d, want refused", res.StatusCode)
	}

	// Promote: another rental's prefix and a key never uploaded both read as
	// "not found"; the real one is copied and readable through a signed GET.
	var known *apperrors.Error
	if _, _, err := s.Promote(t.Context(), uuid.Must(uuid.NewV7()), key, "handovers/x", "photo_keys", ImageTypes); !errors.As(err, &known) ||
		known.Code != apperrors.CodeUploadNotFound {
		t.Errorf("promote with another owner = %v, want upload-not-found", err)
	}
	if _, _, err := s.Promote(t.Context(), owner, PendingKey(owner), "handovers/x", "photo_keys", ImageTypes); !errors.As(err, &known) ||
		known.Code != apperrors.CodeUploadNotFound {
		t.Errorf("promote of a key never uploaded = %v, want upload-not-found", err)
	}
	_, final, err := s.Promote(t.Context(), owner, key, "handovers/"+owner.String()+"/b", "photo_keys", ImageTypes)
	if err != nil {
		t.Fatal(err)
	}
	get, err := s.PresignGet(t.Context(), final, HandoverPhotoTTL)
	if err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(get)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("signed GET of promoted object = %d, %d bytes", res.StatusCode, len(got))
	}
}

func TestPromoteRefusesWrongType(t *testing.T) {
	s := newStore(t)
	owner := uuid.Must(uuid.NewV7())
	key := PendingKey(owner)
	url, _ := s.PresignPut(t.Context(), key, "text/plain", 3)
	if code := put(t, url, "text/plain", []byte("abc")); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	var known *apperrors.Error
	if _, _, err := s.Promote(t.Context(), owner, key, "handovers/x", "photo_keys", ImageTypes); !errors.As(err, &known) ||
		known.Code != apperrors.CodeUploadTypeMismatch {
		t.Fatalf("promote of text/plain = %v, want upload-type-mismatch", err)
	}
}
