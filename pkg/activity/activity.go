// Package activity tracks connection activity to drive idle timeouts.
package activity

import (
	"sync/atomic"
	"time"
)

// Tracker counts active connections and remembers when the last one ended,
// so an idle monitor can tell how long there has been nothing to do.
type Tracker struct {
	activeConns  atomic.Int64
	lastActivity atomic.Int64 // unix nano
}

// NewTracker returns a Tracker whose idle time starts counting now.
func NewTracker() *Tracker {
	t := &Tracker{}
	t.lastActivity.Store(time.Now().UnixNano())
	return t
}

// ConnStarted records a new active connection.
func (t *Tracker) ConnStarted() {
	t.lastActivity.Store(time.Now().UnixNano())
	t.activeConns.Add(1)
}

// ConnEnded records that a connection finished.
func (t *Tracker) ConnEnded() {
	// Refresh lastActivity before decrementing so the idle monitor never
	// observes zero connections alongside a stale timestamp.
	t.lastActivity.Store(time.Now().UnixNano())
	t.activeConns.Add(-1)
}

// IdleFor reports how long there has been no active connection. ok is false
// while at least one connection is active.
func (t *Tracker) IdleFor() (idle time.Duration, ok bool) {
	if t.activeConns.Load() > 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, t.lastActivity.Load())), true
}
