package tools

// NewLoopbackHTTPExecutor is for tests only: an http executor that may connect to loopback and
// does not verify TLS certificates, so a test can run a fake upstream on httptest. Everything
// else (address checks for other ranges, redirect rules, limits) is the production behaviour.
// The controller never uses it.
func NewLoopbackHTTPExecutor() *HTTPExecutor {
	return &HTTPExecutor{allowLoopback: true, insecureTLS: true}
}
