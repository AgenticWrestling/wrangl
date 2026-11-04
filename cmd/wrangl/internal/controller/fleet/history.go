package fleet

import (
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// HistoryLen is how many heartbeats per runner are kept in memory for sparklines
// (360 x 10 s = one hour). Not persisted (spec §11).
const HistoryLen = 360

// Sample is one heartbeat's utilisation.
type Sample struct {
	At             time.Time
	CPU, Mem, Disk float64
}

type history struct {
	mu sync.Mutex
	m  map[int64][]Sample
}

func (h *history) add(id int64, hb proto.Heartbeat, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[int64][]Sample{}
	}
	s := append(h.m[id], Sample{At: at, CPU: hb.CPUPercent, Mem: hb.MemPercent, Disk: hb.DiskPercent})
	if len(s) > HistoryLen {
		s = s[len(s)-HistoryLen:]
	}
	h.m[id] = s
}

// History returns a runner's recent samples, oldest first.
func (s *Service) History(id int64) []Sample {
	s.hist.mu.Lock()
	defer s.hist.mu.Unlock()
	return append([]Sample(nil), s.hist.m[id]...)
}
