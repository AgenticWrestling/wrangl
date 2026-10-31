// Package errs holds the sentinel errors services return and the API maps to HTTP status
// codes. Wrap them with fmt.Errorf("%w: ...") to add detail.
package errs

import "errors"

var (
	NotFound  = errors.New("not found")       // 404
	Invalid   = errors.New("invalid request") // 400
	Forbidden = errors.New("forbidden")       // 403
	Conflict  = errors.New("conflict")        // 409
)
