// Package ratelimit counts requests in a fixed window.  (S1-082, §7)
//
// The numbers live in the caller, not here: 04-api-spec.md §7 ends with "Angka-
// angka ini konfigurasi, bukan konstanta di kode", and a package that owns the
// limits is a package that has to be edited to change one.
package ratelimit

import (
	"context"
	"fmt"
	"time"
)

// Counter is the Redis operation this needs, declared here in the consumer so
// internal/queue stays unaware of rate limiting.
type Counter interface {
	Incr(ctx context.Context, key string, window time.Duration) (int64, error)
}

// Limiter answers whether one more request fits in the current window.
type Limiter struct {
	counter Counter
}

func New(counter Counter) *Limiter { return &Limiter{counter: counter} }

// Allow reports whether the caller is under the limit, and how long until the
// window resets when it is not.
//
// Fail-open on a Redis error, and deliberately: the limiter protects against
// abuse, but refusing every registration because a cache is down turns a
// degraded dependency into a full outage on the one endpoint that creates
// customers. The error is logged by the caller, which is where the trace id is.
func (l *Limiter) Allow(ctx context.Context, key string, limit int64, window time.Duration) (bool, time.Duration, error) {
	n, err := l.counter.Incr(ctx, "ratelimit:"+key, window)
	if err != nil {
		return true, 0, fmt.Errorf("rate limit %s: %w", key, err)
	}
	if n > limit {
		return false, window, nil
	}
	return true, 0, nil
}
