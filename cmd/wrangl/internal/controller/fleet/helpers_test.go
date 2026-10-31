package fleet

import (
	"encoding/base64"
	"testing"
)

func decodeUD(t *testing.T, b64 string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
