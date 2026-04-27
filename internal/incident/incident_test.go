package incident

import (
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
