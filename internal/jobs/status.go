package jobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Tracked jobs: the ones a person waits on through GET /jobs/{id} (BR-091
// obligation 4). Their state is a Redis hash with a 24h TTL -- job state lives
// in Redis, not a table (CLAUDE.md).

const statusTTL = 24 * time.Hour

// Status is one tracked job as GET /jobs/{id} reads it.
type Status struct {
	ID      string
	OwnerID uuid.UUID
	Type    string
	State   string // queued | running | done | failed
	FileKey string
	Error   string
}

// ErrNoJob is a job id that does not exist, expired, or belongs to another
// owner -- one answer for all three (BR-001).
var ErrNoJob = errors.New("no such job")

func statusKey(id string) string { return "job:" + id }

// Track creates the job's status and enqueues it, with the job id in the
// payload under "job_id" so the handler can report progress.
func (q *Queue) Track(ctx context.Context, typ string, ownerID uuid.UUID, payload map[string]any) (string, error) {
	id := uuid.Must(uuid.NewV7()).String()
	key := statusKey(id)
	if err := q.rdb.HSet(ctx, key, "owner_id", ownerID.String(), "type", typ, "state", "queued").Err(); err != nil {
		return "", fmt.Errorf("track job: %w", err)
	}
	q.rdb.Expire(ctx, key, statusTTL)
	payload["job_id"] = id
	if err := q.Enqueue(ctx, typ, ownerID, payload); err != nil {
		q.rdb.HSet(ctx, key, "state", "failed", "error", "could not be queued")
		return "", err
	}
	return id, nil
}

// SetState moves a tracked job along. fileKey and errMsg may be empty.
func (q *Queue) SetState(ctx context.Context, id, state, fileKey, errMsg string) error {
	return q.rdb.HSet(ctx, statusKey(id), "state", state, "file_key", fileKey, "error", errMsg).Err()
}

// Status reads a tracked job, refusing one that is not this owner's.
func (q *Queue) Status(ctx context.Context, id string, ownerID uuid.UUID) (Status, error) {
	m, err := q.rdb.HGetAll(ctx, statusKey(id)).Result()
	if err != nil {
		return Status{}, fmt.Errorf("job status: %w", err)
	}
	if len(m) == 0 || m["owner_id"] != ownerID.String() {
		return Status{}, ErrNoJob
	}
	return Status{ID: id, OwnerID: ownerID, Type: m["type"], State: m["state"], FileKey: m["file_key"], Error: m["error"]}, nil
}

// DeadLetter is one dead-lettered job, for the owner's dashboard (BR-072).
type DeadLetter struct {
	Type     string
	Error    string
	Attempt  int
	FailedAt time.Time
}

// DeadLetters returns this owner's most recent dead-lettered jobs, newest
// first. It scans the latest scanN entries of the shared stream and filters
// by owner. ponytail: a scan window, not an index -- a per-owner dead stream
// if one noisy tenant ever pushes another's failures out of the window.
func (q *Queue) DeadLetters(ctx context.Context, ownerID uuid.UUID, limit int) ([]DeadLetter, error) {
	const scanN = 500
	msgs, err := q.rdb.XRevRangeN(ctx, q.names.Dead, "+", "-", scanN).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("read dead letters: %w", err)
	}
	out := []DeadLetter{}
	for _, m := range msgs {
		if s, _ := m.Values["owner_id"].(string); s != ownerID.String() {
			continue
		}
		get := func(k string) string { s, _ := m.Values[k].(string); return s }
		attempt, _ := strconv.Atoi(get("attempt"))
		at, _ := time.Parse(time.RFC3339, get("failed_at"))
		out = append(out, DeadLetter{Type: get("type"), Error: get("error"), Attempt: attempt, FailedAt: at})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
