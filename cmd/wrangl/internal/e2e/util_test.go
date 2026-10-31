package e2e

import "encoding/base64"

func decodeB64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}

type tlog struct{ t interface{ Log(...any) } }

func (w tlog) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func testLogWriter(t interface{ Log(...any) }) tlog { return tlog{t} }
