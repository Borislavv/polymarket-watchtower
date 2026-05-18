// Package repository wraps the sqlc-generated `sqlc` package and exports
// domain-typed APIs. Nothing above this layer should import sqlc directly;
// the conversion between pgtype.* and time.Time / *time.Time lives here.
package repository

import (
	"github.com/jackc/pgx/v5/pgtype"
	"time"
)

// tsTime converts a sqlc-generated pgtype.Timestamptz to a Go time.Time.
// Returns the zero time when the DB column was NULL — callers should
// check `.IsZero()` to detect "unset".
func tsTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// tsFromTime is the inverse of tsTime: a non-NULL pgtype.Timestamptz when
// the input is non-zero, otherwise NULL.
func tsFromTime(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// strPtr returns *string for non-empty s, nil otherwise. Used at the
// sqlc-boundary for nullable text columns where the upstream API hands us
// "" to mean "absent".
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefStr returns "" when p is nil, *p otherwise.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
