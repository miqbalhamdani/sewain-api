// Package queue owns the connection to Redis.
//
// It carries the liveness check, the short-lived single-use tokens (S1-084),
// and the rate-limit counters (S1-082). The Redis Streams job runner arrives
// in S1-040. It exists now so that when it does, nothing else has grown its
// own client.
//
// Nothing here knows what a token or a rate limit means -- the operations are
// generic on purpose, so the meaning stays in the domain package that owns it.
package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNotFound is a key that is absent or has already been claimed. Callers
// cannot tell those apart, and for a single-use token that is correct.
var ErrNotFound = errors.New("key not found")

// Client is a Redis connection.
type Client struct {
	rdb *redis.Client
}

// New dials Redis and verifies it answers before returning.
func New(ctx context.Context, url string) (*Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Client{rdb: rdb}, nil
}

// Raw is the underlying client, for internal/jobs only: the Streams runner
// (S1-040) needs XREADGROUP, XAUTOCLAIM and Lua, and wrapping each one here would
// be a second API for no gain. Everything else keeps to the methods below.
func (c *Client) Raw() *redis.Client { return c.rdb }

// Close releases the connection.
func (c *Client) Close() error { return c.rdb.Close() }

// SetNX stores a value only if the key is absent, with a TTL. It reports
// whether the write happened.
//
// The absent-only form is what makes a token single-use: the second caller to
// claim the same key gets false rather than silently overwriting the first.
func (c *Client) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	ok, err := c.rdb.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis SETNX %s: %w", key, err)
	}
	return ok, nil
}

// GetDel reads a key and removes it in one round trip.
//
// One round trip, not two: a token read and then deleted in separate calls can
// be redeemed twice by two requests that interleave between them, and
// single-use has to mean single-use under concurrency or it means nothing.
// ErrNotFound when the key is absent or already claimed.
func (c *Client) GetDel(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redis GETDEL %s: %w", key, err)
	}
	return v, nil
}

// Get reads a key without removing it.
//
// The counterpart to GetDel, for values that are read many times rather than
// redeemed once -- a stored idempotency result is replayed to every retry that
// presents the same key, not consumed by the first.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redis GET %s: %w", key, err)
	}
	return v, nil
}

// Set writes a key whether or not it exists, keeping the TTL already on it.
//
// KEEPTTL matters: an idempotency key is claimed when the request starts and
// overwritten with the response when it finishes, and a plain SET would reset
// the 24 hours from the moment of completion rather than from the moment the
// client first asked. A slow handler would quietly extend its own window.
func (c *Client) Set(ctx context.Context, key, value string) error {
	if err := c.rdb.Set(ctx, key, value, redis.KeepTTL).Err(); err != nil {
		return fmt.Errorf("redis SET %s: %w", key, err)
	}
	return nil
}

// Del removes a key.
func (c *Client) Del(ctx context.Context, key string) error {
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("redis DEL %s: %w", key, err)
	}
	return nil
}

// Incr increments a counter and returns its new value, setting the TTL on the
// first increment of a window.
//
// Fixed window rather than sliding: it allows a burst at a window boundary,
// and for "5 registrations an hour" that is a rounding error against the cost
// of keeping a sorted set per IP.
func (c *Client) Incr(ctx context.Context, key string, window time.Duration) (int64, error) {
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	// NX so a long-running window is not extended by its own traffic, which
	// would turn a rate limit into a permanent ban for a busy caller.
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("redis INCR %s: %w", key, err)
	}
	return incr.Val(), nil
}

// ServerVersion reports the redis_version field of INFO server, e.g. "8.4.0".
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	info, err := c.rdb.Info(ctx, "server").Result()
	if err != nil {
		return "", fmt.Errorf("redis INFO server: %w", err)
	}
	for line := range strings.SplitSeq(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("redis INFO server: no redis_version field in response")
}
