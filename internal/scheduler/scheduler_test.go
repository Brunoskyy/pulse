package scheduler

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/pool"
)

func TestJitterStaysWithinTenPercent(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.5, 0.999} {
		d := Jitter(30*time.Second, r)
		if d < 27*time.Second || d >= 33*time.Second {
			t.Fatalf("Jitter(30s, %v) = %s", r, d)
		}
	}
}

// waitFor polls until cond holds; the scheduler's goroutines need a moment to
// reach their next timer after the fake clock moves.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunsOnIntervalWithFakeClock(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	p := pool.New(2)
	var runs atomic.Int64
	s := &Scheduler{Clock: fc, Pool: p, Rand: func() float64 { return 0.5 },
		Run: func(context.Context, config.Check) { runs.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx, []config.Check{{ID: "a", Interval: 10 * time.Second}})

	waitFor(t, func() bool { return fc.Waiters() == 1 })
	fc.Advance(4 * time.Second) // first run is at 5s (0.5 of the interval)
	if runs.Load() != 0 {
		t.Fatal("ran before its start offset")
	}
	fc.Advance(time.Second)
	waitFor(t, func() bool { return runs.Load() == 1 })
	// Next one is 10s * (0.9 + 0.2*0.5) = 10s later.
	waitFor(t, func() bool { return fc.Waiters() == 1 })
	fc.Advance(10 * time.Second)
	waitFor(t, func() bool { return runs.Load() == 2 })

	cancel()
	s.Wait()
	p.Close()
}

func TestNeverOverlapsItself(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	p := pool.New(4)
	release := make(chan struct{})
	var runs, concurrent, peak atomic.Int64
	s := &Scheduler{Clock: fc, Pool: p, Rand: func() float64 { return 0 },
		Run: func(context.Context, config.Check) {
			n := concurrent.Add(1)
			if n > peak.Load() {
				peak.Store(n)
			}
			runs.Add(1)
			<-release
			concurrent.Add(-1)
		}}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx, []config.Check{{ID: "slow", Interval: 10 * time.Second}})
	waitFor(t, func() bool { return runs.Load() == 1 })
	for range 3 {
		waitFor(t, func() bool { return fc.Waiters() == 1 })
		fc.Advance(10 * time.Second)
	}
	waitFor(t, func() bool { return s.Skipped.Load() == 3 })
	if peak.Load() != 1 || runs.Load() != 1 {
		t.Fatalf("peak=%d runs=%d: a check must not run twice at once", peak.Load(), runs.Load())
	}
	close(release)
	cancel()
	s.Wait()
	p.Close()
}

func TestSpreadsStartTimes(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	p := pool.New(8)
	var mu sync.Mutex
	first := map[string]time.Time{}
	vals := []float64{0.1, 0.4, 0.7}
	var i atomic.Int64
	s := &Scheduler{Clock: fc, Pool: p,
		Rand: func() float64 { return vals[int(i.Add(1)-1)%len(vals)] },
		Run: func(_ context.Context, c config.Check) {
			mu.Lock()
			if _, ok := first[c.ID]; !ok {
				first[c.ID] = fc.Now()
			}
			mu.Unlock()
		}}
	ctx, cancel := context.WithCancel(context.Background())
	checks := []config.Check{{ID: "a", Interval: 10 * time.Second}, {ID: "b", Interval: 10 * time.Second}, {ID: "c", Interval: 10 * time.Second}}
	s.Start(ctx, checks)
	waitFor(t, func() bool { return fc.Waiters() == 3 })
	for range 10 {
		fc.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(first) == 3 })
	mu.Lock()
	seen := map[int64]bool{}
	for _, at := range first {
		seen[at.Unix()] = true
	}
	mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("checks started at the same moment: %v", first)
	}
	cancel()
	s.Wait()
	p.Close()
}

func TestStopsWithoutLeaking(t *testing.T) {
	before := runtime.NumGoroutine()
	p := pool.New(4)
	s := &Scheduler{Clock: clock.Real{}, Pool: p, Run: func(context.Context, config.Check) {}}
	ctx, cancel := context.WithCancel(context.Background())
	var checks []config.Check
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		checks = append(checks, config.Check{ID: id, Interval: time.Hour})
	}
	s.Start(ctx, checks)
	cancel()
	s.Wait()
	p.Close()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("leaked goroutines: %d > %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
