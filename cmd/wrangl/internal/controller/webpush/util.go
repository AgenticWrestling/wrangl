package webpush

import (
	"database/sql"
	"time"
)

func sqlNow() sql.NullString {
	return sql.NullString{String: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Valid: true}
}
