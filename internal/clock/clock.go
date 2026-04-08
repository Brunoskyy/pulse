// Package clock lets the scheduler and the incident logic be driven by a fake
// time source in tests, so nothing waits on real seconds.
package clock

import (
	"sync"
	"time"
)

// Clock is the slice of the time package Pulse depends on.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a manually advanced clock. Timers fire when Advance moves past them.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func NewFake(start time.Time) *Fake { return &Fake{now: start} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := f.now.Add(d)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, waiter{at: at, ch: ch})
	return ch
}

// Advance moves time forward and fires every timer that is now due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	kept := f.waiters[:0]
	var due []waiter
	for _, w := range f.waiters {
		if !w.at.After(now) {
			due = append(due, w)
		} else {
			kept = append(kept, w)
		}
	}
	f.waiters = kept
	f.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

// Waiters reports how many timers are pending; tests use it to know when a
// goroutine has reached its next sleep.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}
