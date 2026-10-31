package stack_test

import (
	"database/sql"
	"strconv"
)

func sqlNull(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
func itoa(v int64) string           { return strconv.FormatInt(v, 10) }
