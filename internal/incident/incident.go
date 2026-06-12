// Package incident turns a stream of probe results into outages.
package incident

import (
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
)

// Transition is what a result did to a check's state.
type Transition int

const (
	None Transition = iota
	Opened
	Resolved
)

// Tracker holds one check's streaks. It is not safe for concurrent use; the
// monitor feeds each check's results in order from one place.
//
// An incident opens after FailAfter consecutive failures and is dated from the
// first of them, not the one that crossed the threshold: the outage began when
// the first probe failed. It resolves after RecoverAfter consecutive successes
// and ends at the first of those. A single blip below the threshold never
// becomes an incident, and a single good probe in the middle of an outage does
// not end it.
//
// Two more rules. With FlapWindow and FlapFailures set, an incident also
// opens when FlapFailures of the last FlapWindow probes failed, so a service
// that fails two probes in three is not reported as healthy just because it
// never failed three in a row. And a gap longer than MaxGap between two
// results breaks every streak: Pulse was not watching in between, so
// failures before the gap say nothing about what came after.
type Tracker struct {
	FailAfter    int
	RecoverAfter int
	FlapWindow   int
	FlapFailures int
	MaxGap       time.Duration

	window  []windowEntry
	lastAt  time.Time
	hasLast bool

	failStreak int
	okStreak   int
	firstFail  time.Time
	firstOK    time.Time
	lastError  string
	open       bool
}

// Resume marks the tracker as already inside an incident, used after a
// restart when the store still has one open.
func (t *Tracker) Resume() { t.open = true }

// Open reports whether an incident is in progress.
func (t *Tracker) Open() bool { return t.open }

// Observe feeds one result and reports whether it opened or resolved an
// incident, with the time the transition should be dated at.
type windowEntry struct {
	at time.Time
	ok bool
}

func (t *Tracker) Observe(r check.Result) (Transition, time.Time, string) {
	if t.hasLast && t.MaxGap > 0 && r.At.Sub(t.lastAt) > t.MaxGap {
		t.failStreak, t.okStreak, t.window = 0, 0, nil
	}
	t.lastAt, t.hasLast = r.At, true
	if t.FlapWindow > 0 {
		t.window = append(t.window, windowEntry{at: r.At, ok: r.OK})
		if len(t.window) > t.FlapWindow {
			t.window = t.window[len(t.window)-t.FlapWindow:]
		}
	}
	if r.OK {
		t.failStreak = 0
		if t.okStreak == 0 {
			t.firstOK = r.At
		}
		t.okStreak++
		if t.open && t.okStreak >= t.RecoverAfter {
			t.open = false
			// Start the flap window fresh, or the failures that caused this
			// incident would reopen it on the next single failure.
			t.window = nil
			return Resolved, t.firstOK, ""
		}
		return None, time.Time{}, ""
	}
	t.okStreak = 0
	if t.failStreak == 0 {
		t.firstFail = r.At
	}
	t.failStreak++
	t.lastError = r.Error
	if !t.open && t.failStreak >= t.FailAfter {
		t.open = true
		return Opened, t.firstFail, r.Error
	}
	if !t.open && t.FlapFailures > 0 && len(t.window) >= t.FlapFailures {
		failed := 0
		var first time.Time
		for _, w := range t.window {
			if !w.ok {
				if failed == 0 {
					first = w.at
				}
				failed++
			}
		}
		if failed >= t.FlapFailures {
			t.open = true
			return Opened, first, r.Error
		}
	}
	return None, time.Time{}, ""
}
