// Package dbnull builds database/sql null values from Go zero values.
package dbnull

import "database/sql"

// Str is NULL for the empty string.
func Str(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// Int is NULL for zero.
func Int(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }
