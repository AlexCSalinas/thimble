package vnet

import (
	"sync"
	"time"
)

// Event is one guest connection attempt.
type Event struct {
	Time    time.Time `json:"time"`
	Proto   string    `json:"proto"` // tcp, udp
	Dst     string    `json:"dst"`   // ip:port
	Name    string    `json:"name,omitempty"`
	Verdict string    `json:"verdict"` // allow, deny
	Reason  string    `json:"reason"`
	// Filled in when an allowed TCP connection closes.
	BytesOut int64   `json:"bytesOut,omitempty"` // guest -> internet
	BytesIn  int64   `json:"bytesIn,omitempty"`  // internet -> guest
	Millis   float64 `json:"ms,omitempty"`
}

// ring keeps the most recent events for one sandbox.
type ring struct {
	mu   sync.Mutex
	buf  []Event
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]Event, n)} }

func (r *ring) add(e Event) {
	r.mu.Lock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// snapshot returns events oldest first.
func (r *ring) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]Event(nil), r.buf[:r.next]...)
	}
	return append(append([]Event(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}
