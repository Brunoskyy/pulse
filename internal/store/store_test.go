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
	w, err := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if w.Total != 22 || w.OK != 20 {
		t.Fatalf("counts: %+v", w)
	}
	if got := w.Uptime(); got < 0.909 || got > 0.910 {
		t.Fatalf("uptime %v, want 20/22", got)
	}
	// Nearest rank: ceil(0.95*20) = 19th smallest of the successful probes.
	if w.P95 != 19*time.Millisecond {
		t.Fatalf("p95 %s, want 19ms (failures excluded)", w.P95)
	}
}

func TestSummaryWithNoDataIsUnknownNotDown(t *testing.T) {
	s := open(t)
	w, err := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour))
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
	w, _ := s.Summary(context.Background(), "api", t0, t0.Add(48*time.Hour))
	if w.Uptime() != 1 {
		t.Fatalf("time with no probes counted as downtime: %v", w.Uptime())
	}
}

func TestDaysHasNoHoles(t *testing.T) {
	s := open(t)
	add(t, s, t0.Add(2*time.Hour), true, 10)
	add(t, s, t0.Add(3*time.Hour), false, 10)
	add(t, s, t0.Add(2*24*time.Hour+time.Hour), true, 10)
	days, err := s.Days(context.Background(), "api", t0, t0.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 4 {
		t.Fatalf("want 4 days (inclusive), got %d", len(days))
	}
	if days[0].Total != 2 || days[0].OK != 1 || days[1].Total != 0 || days[2].Total != 1 || days[3].Total != 0 {
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
				if _, err := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour)); err != nil {
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
	w, _ := s.Summary(context.Background(), "api", t0, t0.Add(time.Hour))
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
