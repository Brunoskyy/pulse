package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pulse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func add(t *testing.T, s *Store, at time.Time, ok bool, ms int) {
	t.Helper()
	if err := s.AddResult(context.Background(), check.Result{CheckID: "api", At: at, OK: ok, Latency: time.Duration(ms) * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryUptimeAndP95(t *testing.T) {
	s := open(t)
	// 20 good probes with latencies 1..20 ms, and 2 failures at 999 ms.
	for i := 1; i <= 20; i++ {
		add(t, s, t0.Add(time.Duration(i)*time.Minute), true, i)
	}
	add(t, s, t0.Add(30*time.Minute), false, 999)
	add(t, s, t0.Add(31*time.Minute), false, 999)
	w, err := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w.Total != 22 || w.OK != 20 {
		t.Fatalf("counts: %+v", w)
	}
	// Probes 1..19 stand for a minute each, probe 20 for two (the cap, the
	// next probe is ten minutes later), the failure at 30 for one and the
	// failure at 31 for two (cap again, the window ends at 60): 21 of 24.
	if w.Up != 21*time.Minute || w.Observed != 24*time.Minute || w.Uptime() != 0.875 {
		t.Fatalf("up %s of %s (%v), want 21m of 24m", w.Up, w.Observed, w.Uptime())
	}
	// Nearest rank: ceil(0.95*20) = 19th smallest of the successful probes.
	if w.P95 != 19*time.Millisecond {
		t.Fatalf("p95 %s, want 19ms (failures excluded)", w.P95)
	}
}

func TestSummaryWithNoDataIsUnknownNotDown(t *testing.T) {
	s := open(t)
	w, err := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasData || w.Uptime() != -1 {
		t.Fatalf("no probes must read as no data: %+v uptime=%v", w, w.Uptime())
	}
}

func TestGapsAreNotDowntime(t *testing.T) {
	s := open(t)
	// Monitor ran for an hour, was off for a day, ran again for an hour.
	for i := range 60 {
		add(t, s, t0.Add(time.Duration(i)*time.Minute), true, 10)
		add(t, s, t0.Add(25*time.Hour+time.Duration(i)*time.Minute), true, 10)
	}
	w, _ := s.Summary(context.Background(), "api", t0, t0.Add(48*time.Hour), 2*time.Minute)
	if w.Uptime() != 1 {
		t.Fatalf("time with no probes counted as downtime: %v", w.Uptime())
	}
	if w.Observed > 125*time.Minute {
		t.Fatalf("the day nobody was looking was counted as observed: %s", w.Observed)
	}
}

func TestUptimeIsWeightedByTimeNotBySample(t *testing.T) {
	s := open(t)
	// Ten hours of probes every ten minutes, all fine, then one minute of
	// failures probed every five seconds.
	for i := range 60 {
		add(t, s, t0.Add(time.Duration(i)*10*time.Minute), true, 10)
	}
	failStart := t0.Add(600 * time.Minute)
	for i := range 12 {
		add(t, s, failStart.Add(time.Duration(i)*5*time.Second), false, 10)
	}
	end := failStart.Add(time.Minute)
	w, _ := s.Summary(context.Background(), "api", t0, end, 10*time.Minute)
	// 600 minutes up, 1 minute down. By sample count it would read 60/72.
	if got := w.Uptime(); got < 0.998 || got > 0.9984 {
		t.Fatalf("uptime %v, want 600/601", got)
	}
}

func TestDaysHasNoHoles(t *testing.T) {
	s := open(t)
	add(t, s, t0.Add(2*time.Hour), true, 10)
	add(t, s, t0.Add(3*time.Hour), false, 10)
	add(t, s, t0.Add(2*24*time.Hour+time.Hour), true, 10)
	days, err := s.Days(context.Background(), "api", t0, t0.Add(3*24*time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 4 {
		t.Fatalf("want 4 days (inclusive), got %d", len(days))
	}
	// Day 0: up for the hour until the failure, then the failure stands for
	// the two-hour cap.
	if days[0].Up != time.Hour || days[0].Observed != 3*time.Hour {
		t.Fatalf("day 0: %+v", days[0])
	}
	if days[1].Uptime() != -1 || days[2].Uptime() != 1 || days[3].Uptime() != -1 {
		t.Fatalf("days: %+v", days)
	}
}

func TestIncidentsLifecycleAndRestart(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, err := s.OpenIncident(ctx, "api", t0, "status 503")
	if err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenIncidents(ctx)
	if open["api"].ID != id {
		t.Fatalf("open incident not found: %+v", open)
	}
	end := t0.Add(10 * time.Minute)
	if err := s.CloseIncident(ctx, id, end); err != nil {
		t.Fatal(err)
	}
	// A second close must not move the end time.
	_ = s.CloseIncident(ctx, id, end.Add(time.Hour))
	list, _ := s.Incidents(ctx, t0.Add(-time.Hour), 10)
	if len(list) != 1 || list[0].EndedAt == nil || !list[0].EndedAt.Equal(end) {
		t.Fatalf("incident: %+v", list)
	}
	if d := list[0].Duration(time.Now()); d != 10*time.Minute {
		t.Fatalf("duration %s", d)
	}
	if open, _ := s.OpenIncidents(ctx); len(open) != 0 {
		t.Fatalf("closed incident still open: %+v", open)
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	add(t, s, t0, true, 1)
	add(t, s, t0.Add(48*time.Hour), true, 1)
	old := t0.Add(time.Hour)
	_ = s.AddIncident(ctx, Incident{CheckID: "api", StartedAt: t0, EndedAt: &old})
	openID, _ := s.OpenIncident(ctx, "api", t0, "still down")
	n, err := s.Prune(ctx, t0.Add(24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d rows, err %v", n, err)
	}
	list, _ := s.Incidents(ctx, time.Unix(0, 0), 10)
	if len(list) != 1 || list[0].ID != openID {
		t.Fatalf("an open incident must survive pruning, whatever its age: %+v", list)
	}
}

func TestConcurrentWritersDoNotFail(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				errs <- s.AddResult(context.Background(), check.Result{CheckID: "api", At: t0.Add(time.Duration(w*50+i) * time.Second), OK: true})
			}
		}()
	}
	// Readers alongside the writers.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour), time.Minute); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	w, _ := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour), time.Minute)
	if w.Total != 400 {
		t.Fatalf("lost writes: %d", w.Total)
	}
}

func TestPercentileRank(t *testing.T) {
	for _, c := range []struct{ n, want int }{{1, 1}, {19, 19}, {20, 19}, {100, 95}, {101, 96}} {
		if got := percentileRank(c.n, 0.95); got != c.want {
			t.Errorf("rank(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestPruneInBatches(t *testing.T) {
	s := open(t)
	var rs []check.Result
	for i := range PruneBatch*2 + 10 {
		rs = append(rs, check.Result{CheckID: "api", At: t0.Add(time.Duration(i) * time.Second), OK: true})
	}
	if err := s.AddResults(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(context.Background(), t0.Add(24*time.Hour))
	if err != nil || n != int64(len(rs)) {
		t.Fatalf("pruned %d of %d, err %v", n, len(rs), err)
	}
}

// Nothing observable breaks if Prune skips the write lock, because SQLite's
// busy_timeout queues the writers anyway, so this checks the lock itself.
func TestPruneWaitsForTheWriteLock(t *testing.T) {
	s := open(t)
	add(t, s, t0, true, 1)
	s.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prune(context.Background(), t0.Add(24*time.Hour)); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
		s.mu.Unlock()
		t.Fatal("Prune wrote while another write held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	s.mu.Unlock()
	<-done
}

func TestRecentIsOldestFirst(t *testing.T) {
	s := open(t)
	for i := range 5 {
		add(t, s, t0.Add(time.Duration(i)*time.Minute), i%2 == 0, i)
	}
	r, err := s.Recent(context.Background(), "api", 3)
	if err != nil || len(r) != 3 || !r[0].At.Equal(t0.Add(2*time.Minute)) || !r[2].At.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("recent: %+v %v", r, err)
	}
}
