package sessionhost

import "sync"

// Ring keeps the last N bytes of a session's raw output.
type Ring struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// NewRing returns a ring holding at most max bytes.
func NewRing(max int) *Ring { return &Ring{max: max} }

// Write appends p, discarding the oldest bytes beyond the bound.
func (r *Ring) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.max {
		r.buf = append(r.buf[:0], p[len(p)-r.max:]...)
		return
	}
	if over := len(r.buf) + len(p) - r.max; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	r.buf = append(r.buf, p...)
}

// Bytes returns a copy of the ring's contents, oldest first.
func (r *Ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}
