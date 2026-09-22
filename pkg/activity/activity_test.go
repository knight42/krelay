package activity

import (
	"testing"
	"time"
)

func TestTracker(t *testing.T) {
	testCases := map[string]struct {
		ops      func(tr *Tracker)
		wantIdle bool
	}{
		"fresh tracker is idle": {
			ops:      func(*Tracker) {},
			wantIdle: true,
		},
		"active connection is not idle": {
			ops:      func(tr *Tracker) { tr.ConnStarted() },
			wantIdle: false,
		},
		"one of two connections ended is not idle": {
			ops: func(tr *Tracker) {
				tr.ConnStarted()
				tr.ConnStarted()
				tr.ConnEnded()
			},
			wantIdle: false,
		},
		"all connections ended is idle": {
			ops: func(tr *Tracker) {
				tr.ConnStarted()
				tr.ConnEnded()
			},
			wantIdle: true,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			tr := NewTracker()
			tc.ops(tr)
			idle, ok := tr.IdleFor()
			if ok != tc.wantIdle {
				t.Fatalf("IdleFor() ok = %v, want %v", ok, tc.wantIdle)
			}
			if ok && idle < 0 {
				t.Fatalf("IdleFor() = %v, want >= 0", idle)
			}
		})
	}
}

func TestTrackerIdleGrows(t *testing.T) {
	tr := NewTracker()
	tr.ConnStarted()
	tr.ConnEnded()
	first, ok := tr.IdleFor()
	if !ok {
		t.Fatal("IdleFor() ok = false, want true")
	}
	time.Sleep(10 * time.Millisecond)
	second, ok := tr.IdleFor()
	if !ok {
		t.Fatal("IdleFor() ok = false, want true")
	}
	if second <= first {
		t.Fatalf("idle time did not grow: first=%v second=%v", first, second)
	}
}
