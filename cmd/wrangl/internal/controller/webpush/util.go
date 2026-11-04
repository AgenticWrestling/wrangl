package webpush

import (
	"database/sql"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"time"
)

func sqlNow() sql.NullString {
	return sql.NullString{String: dbtime.Format(time.Now()), Valid: true}
}
