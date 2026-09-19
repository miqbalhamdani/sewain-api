// Package db owns every connection to PostgreSQL.
//
// It is the only package permitted to import pgx (see CLAUDE.md's import
// rules); go-arch-lint enforces that from P1-005 onward.
package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns the connection pool and is the entry point for every query.
//
// tdd.md 3.2 and CLAUDE.md both call it Store: it is not merely a pool, it is
// the thing that hands out owner-scoped transactions. See tx.go.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a pool and verifies the server is reachable before returning it.
// A pool that cannot be reached is an error here rather than a surprise on the
// first query.
func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases every connection in the pool.
func (s *Store) Close() { s.pool.Close() }

// ServerVersion reports the version the server itself claims, e.g. "18.4" --
// which PostgreSQL was actually reached, not which one was configured.
//
// Homebrew and Debian builds append a packaging suffix ("18.4 (Homebrew)"), so
// only the leading version token is returned.
func (s *Store) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := s.pool.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return "", fmt.Errorf("read server_version: %w", err)
	}
	if token, _, found := strings.Cut(v, " "); found {
		return token, nil
	}
	return v, nil
}
