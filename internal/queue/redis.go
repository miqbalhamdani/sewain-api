// Package queue owns the connection to Redis.
//
// Phase 1 uses it for little more than a liveness check; the Redis Streams job
// runner arrives in P1-060. It exists now so that when it does, nothing else
// has grown its own client.
package queue

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

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

// Close releases the connection.
func (c *Client) Close() error { return c.rdb.Close() }

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
