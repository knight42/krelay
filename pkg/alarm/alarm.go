package alarm

import (
	"time"
)

type Alarm struct {
	d time.Duration

	reset chan struct{}
	done  chan struct{}
}

func New(d time.Duration) Alarm {
	return Alarm{
		d: d,

		reset: make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		<-t.C
	}
}

func (a *Alarm) Start() {
	go func() {
		t := time.NewTimer(a.d)
		for {
			select {
			case <-t.C:
				close(a.done)
				stopTimer(t)
				return

			case <-a.reset:
				stopTimer(t)
				t.Reset(a.d)
			}
		}
	}()
}

func (a *Alarm) Reset() {
	select {
	case a.reset <- struct{}{}:
	case <-a.done:
	}
}

func (a *Alarm) Done() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}
