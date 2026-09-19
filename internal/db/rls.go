package db

import (
	"context"
	"fmt"
)

// RLSProblem is one owner-owned table whose row level security is not set up
// correctly, and what specifically is wrong with it.
type RLSProblem struct {
	Table  string
	Reason string
}

func (p RLSProblem) String() string { return p.Table + ": " + p.Reason }

// CheckOwnerRLS returns every table in the public schema that has an owner_id
// column but is not properly protected.
//
// Three things can be wrong and only one of them is loud:
//
//   - RLS not enabled -- the table is wide open, and every query returns every
//     owner's rows.
//   - RLS enabled but not FORCEd -- correct for everyone except the table's
//     owner, who silently sees everything. Nothing looks broken.
//   - RLS enabled but no policy -- the opposite failure. Every row is filtered
//     out and the table reads as permanently empty.
//
// The middle one is why this exists as a check rather than as a code review
// habit. See tdd.md 3.3.
func (s *Store) CheckOwnerRLS(ctx context.Context) ([]RLSProblem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind = 'r'
		   AND n.nspname = 'public'
		   AND EXISTS (
		       SELECT 1 FROM pg_attribute a
		        WHERE a.attrelid = c.oid AND a.attname = 'owner_id'
		          AND a.attnum > 0 AND NOT a.attisdropped)
		 ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("enumerate owner tables: %w", err)
	}
	defer rows.Close()

	var problems []RLSProblem
	for rows.Next() {
		var table string
		var enabled, forced, hasPolicy bool
		if err := rows.Scan(&table, &enabled, &forced, &hasPolicy); err != nil {
			return nil, fmt.Errorf("scan owner table: %w", err)
		}

		switch {
		case !enabled:
			// The other two are moot until this is fixed, and listing them
			// would read as three separate problems with one table.
			problems = append(problems, RLSProblem{table, fmt.Sprintf(
				"has owner_id but row level security is not enabled -- "+
					"its migration is missing SELECT enable_owner_rls('%s')", table)})
		default:
			if !forced {
				problems = append(problems, RLSProblem{table,
					"row level security is enabled but not FORCEd -- the table's owner bypasses its own policy"})
			}
			if !hasPolicy {
				problems = append(problems, RLSProblem{table,
					"row level security is enabled but there is no policy -- every row is filtered out"})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owner tables: %w", err)
	}
	return problems, nil
}
