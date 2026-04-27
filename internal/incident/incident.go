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
type Tracker struct {
	FailAfter    int
	RecoverAfter int

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
func (t *Tracker) Observe(r check.Result) (Transition, time.Time, string) {
	if r.OK {
		t.failStreak = 0
		if t.okStreak == 0 {
			t.firstOK = r.At
		}
		t.okStreak++
		if t.open && t.okStreak >= t.RecoverAfter {
			t.open = false
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
	return None, time.Time{}, ""
}
