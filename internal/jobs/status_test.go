package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A tracked job is readable only by its owner, follows its handler to done,
// and a dead one is marked failed and listed for its owner alone (BR-091).
func TestTrackedJob(t *testing.T) {
	rdb, n := setup(t)
	q := NewQueue(rdb, n)
	a, b := uuid.New(), uuid.New()

	id, err := q.Track(t.Context(), "export", a, map[string]any{"report": "revenue"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), statusKey(id)) })
	if st, err := q.Status(t.Context(), id, a); err != nil || st.State != "queued" || st.Type != "export" {
		t.Errorf("own job = %+v, %v; want queued export", st, err)
	}
	if _, err := q.Status(t.Context(), id, b); !errors.Is(err, ErrNoJob) {
		t.Errorf("another owner's job: err = %v, want ErrNoJob", err)
	}

	w := NewWorker(rdb, n, "c1")
	w.Block = 50 * time.Millisecond
	w.OnDead = func(ctx context.Context, j Job, cause error) { _ = q.SetState(ctx, id, "failed", "", cause.Error()) }
	w.Handle("export", func(context.Context, Job) error { return ErrPermanent })
	_ = w.Tick(t.Context())

	if st, _ := q.Status(t.Context(), id, a); st.State != "failed" {
		t.Errorf("dead job state = %q, want failed", st.State)
	}
	if dl, err := q.DeadLetters(t.Context(), a, 10); err != nil || len(dl) != 1 || dl[0].Type != "export" || dl[0].FailedAt.IsZero() {
		t.Errorf("owner's dead letters = %+v, %v", dl, err)
	}
	if dl, _ := q.DeadLetters(t.Context(), b, 10); len(dl) != 0 {
		t.Errorf("another owner sees %d dead letters", len(dl))
	}
}
