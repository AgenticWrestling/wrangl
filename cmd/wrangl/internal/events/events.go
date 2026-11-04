// Package events is an in-process pub/sub of change notices (runner, run, task, question,
// pr). Subscribers (SSE clients: TUI, PWA) refetch what changed. Publishing never blocks:
// a slow subscriber drops notices, and refetching on the next one catches it up.
package events

import (
	"strconv"
	"sync"
	"time"
)

// Event kinds.
const (
	KindRun      = "run"
	KindPR       = "pr"
	KindQuestion = "question"
	KindTask     = "task"
	KindThread   = "thread"
	KindRunner   = "runner"
	KindLog      = "log"
)

// Event is a change notice.
type Event struct {
	Kind string    `json:"kind"`         // runner | run | task | question | pr
	ID   string    `json:"id,omitempty"` // runner name, run id, task sqid, question id, pr id
	At   time.Time `json:"at"`
}

// Bus fans events out to subscribers. The zero value is ready; a nil *Bus is a no-op.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// Publish sends an event to every subscriber without blocking.
func (b *Bus) Publish(kind, id string) {
	if b == nil {
		return
	}
	e := Event{Kind: kind, ID: id, At: time.Now().UTC()}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Run notes a change to a run (by id).
func (b *Bus) Run(id int64) { b.Publish(KindRun, strconv.FormatInt(id, 10)) }

// PR notes a change to a pull request (by id).
func (b *Bus) PR(id int64) { b.Publish(KindPR, strconv.FormatInt(id, 10)) }

// Question notes a change to a question (by id).
func (b *Bus) Question(id int64) { b.Publish(KindQuestion, strconv.FormatInt(id, 10)) }

// Task notes a change to a task (by sqid).
func (b *Bus) Task(sqid string) { b.Publish(KindTask, sqid) }

// Thread notes a change to a task's controller-agent thread (by task sqid).
func (b *Bus) Thread(sqid string) { b.Publish(KindThread, sqid) }

// Runner notes a change to a runner (by name).
func (b *Bus) Runner(name string) { b.Publish(KindRunner, name) }

// Log notes a new log entry (by subject).
func (b *Bus) Log(subject string) { b.Publish(KindLog, subject) }

// Subscribe returns a channel of events and a function to unsubscribe.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan Event]struct{}{}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}
