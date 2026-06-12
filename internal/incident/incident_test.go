package incident

import (
	"strings"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
)

func feed(tr *Tracker, pattern string) []string {
	var out []string
	at := time.Unix(1000, 0)
	for i, ch := range pattern {
		r := check.Result{At: at.Add(time.Duration(i) * time.Minute), OK: ch == '+', Error: "boom"}
		switch t, when, _ := tr.Observe(r); t {
		case Opened:
			out = append(out, "open@"+when.Sub(at).String())
		case Resolved:
			out = append(out, "resolve@"+when.Sub(at).String())
		}
	}
	return out
}

func TestThresholds(t *testing.T) {
	cases := []struct {
		pattern string
		want    []string
	}{
		{"+-+-+--+", nil},                              // blips below the threshold
		{"++---", []string{"open@2m0s"}},               // dated from the first failure
		{"---+--", []string{"open@0s"}},                // one good probe does not end it
		{"---++", []string{"open@0s", "resolve@3m0s"}}, // dated from the first success
		{"---+-++", []string{"open@0s", "resolve@5m0s"}},
		{"---++---++", []string{"open@0s", "resolve@3m0s", "open@5m0s", "resolve@8m0s"}},
	}
	for _, c := range cases {
		tr := &Tracker{FailAfter: 3, RecoverAfter: 2}
		got := feed(tr, c.pattern)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.pattern, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.pattern, got, c.want)
			}
		}
	}
}

func TestResumedTrackerOnlyResolves(t *testing.T) {
	tr := &Tracker{FailAfter: 3, RecoverAfter: 2}
	tr.Resume()
	if got := feed(tr, "---"); len(got) != 0 {
		t.Fatalf("a resumed outage must not open a second incident: %v", got)
	}
	if got := feed(tr, "++"); len(got) != 1 || got[0] != "resolve@0s" {
		t.Fatalf("got %v", got)
	}
}

func TestFlappingOpensAnIncident(t *testing.T) {
	// Two failures in every three probes never makes three in a row.
	noFlap := &Tracker{FailAfter: 3, RecoverAfter: 2}
	if got := feed(noFlap, "--+--+--+--+"); got != nil {
		t.Fatalf("without flap detection nothing opens: %v", got)
	}
	tr := &Tracker{FailAfter: 3, RecoverAfter: 2, FlapWindow: 10, FlapFailures: 6}
	got := feed(tr, "--+--+--+--+++---")
	// Six failures within the window by the 9th probe (index 7), dated from
	// the first failure in the window; two good probes close it; the next
	// three failures open a fresh one, not a flap left over from before.
	want := []string{"open@0s", "resolve@11m0s", "open@14m0s"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAGapBreaksTheStreak(t *testing.T) {
	tr := &Tracker{FailAfter: 3, RecoverAfter: 2, MaxGap: time.Minute}
	at := time.Unix(1000, 0)
	obs := func(d time.Duration, ok bool) Transition {
		tt, _, _ := tr.Observe(check.Result{At: at.Add(d), OK: ok, Error: "boom"})
		return tt
	}
	obs(0, false)
	obs(30*time.Second, false)
	// Pulse was down for three days; this failure starts a new streak.
	if obs(72*time.Hour, false) != None {
		t.Fatal("failures before the gap must not count")
	}
	obs(72*time.Hour+30*time.Second, false)
	tt, when, _ := tr.Observe(check.Result{At: at.Add(72*time.Hour + time.Minute), OK: false})
	if tt != Opened || !when.Equal(at.Add(72*time.Hour)) {
		t.Fatalf("incident should open dated after the gap: %v %s", tt, when)
	}
}
