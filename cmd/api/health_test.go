package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

// TestHealthzReportsEveryService is S1-001's acceptance criterion: the API
// connects to PostgreSQL 18, Redis 8 and MinIO -- all three installed on the
// host, none in a container -- and /healthz reports all three.
//
// It fails rather than skips when a service is unreachable. A skipping test
// would let `make check` go green while proving nothing, and reaching all
// three services is the entire deliverable of this item.
func TestHealthzReportsEveryService(t *testing.T) {
	ctx := t.Context()

	pool, err := db.New(ctx, config.AppDatabaseURL())
	if err != nil {
		t.Fatalf("connect postgres: %v\n\nIs it running, and does the database exist?\n"+
			"  brew services start postgresql@18\n  make db-create", err)
	}
	t.Cleanup(pool.Close)

	redis, err := queue.New(ctx, config.RedisURL())
	if err != nil {
		t.Fatalf("connect redis: %v\n\nIs it running?\n  brew services start redis", err)
	}
	t.Cleanup(func() { _ = redis.Close() })

	rec := httptest.NewRecorder()
	newHealthHandler(
		checker{name: "postgres", version: pool.ServerVersion},
		checker{name: "redis", version: redis.ServerVersion},
		objectStoreChecker(config.ObjectStoreURL()),
	).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d\nbody: %s", rec.Code, http.StatusOK, rec.Body)
	}

	var got healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode /healthz body: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want %q", got.Status, "ok")
	}

	// The contract pins PostgreSQL 18 and Redis 8 (S1-001).
	// Asserting the major version means a downgraded host install fails here,
	// rather than in a migration months from now.
	for _, want := range []struct {
		service string
		major   int
	}{
		{service: "postgres", major: 18},
		{service: "redis", major: 8},
	} {
		svc, ok := got.Services[want.service]
		if !ok {
			t.Errorf("/healthz reported no %q service", want.service)
			continue
		}
		if svc.Status != "ok" {
			t.Errorf("%s status = %q (%s), want %q", want.service, svc.Status, svc.Error, "ok")
			continue
		}
		major, err := strconv.Atoi(strings.SplitN(svc.Version, ".", 2)[0])
		if err != nil {
			t.Errorf("%s version %q: cannot read a major version from it: %v", want.service, svc.Version, err)
			continue
		}
		if major < want.major {
			t.Errorf("%s is %s, want >= %d.x -- the contract pins %d", want.service, svc.Version, want.major, want.major)
		}
		t.Logf("%s %s", want.service, svc.Version)
	}

	// MinIO reports its product rather than its build, so there is no major
	// version to pin -- what matters is that it answered at all. Production
	// has no MinIO; this is the local stand-in for R2 (S1-033).
	store, ok := got.Services["objectstore"]
	if !ok {
		t.Fatal("/healthz reported no \"objectstore\" service")
	}
	if store.Status != "ok" {
		t.Errorf("objectstore status = %q (%s), want %q\n\nIs MinIO running?\n"+
			"  minio server --address=:9000 ~/minio-data", store.Status, store.Error, "ok")
	}
	t.Logf("objectstore %s", store.Version)
}
