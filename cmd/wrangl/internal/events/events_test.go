package events

import (
	"testing"
	"time"
)

func TestBus(t *testing.T) {
	var b Bus
	ch, stop := b.Subscribe()
	b.Publish("run", "7")
	select {
	case e := <-ch:
		if e.Kind != "run" || e.ID != "7" {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	stop()
	for i := 0; i < 1000; i++ { // no subscribers, and a full buffer never blocks
		b.Publish("x", "")
	}
	var nilBus *Bus
	nilBus.Publish("x", "") // no-op
	slow, _ := b.Subscribe()
	for i := 0; i < 1000; i++ {
		b.Publish("x", "")
	}
	if len(slow) != cap(slow) {
		t.Fatalf("slow subscriber has %d", len(slow))
	}
}
