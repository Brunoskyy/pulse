package monitor

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/notify"
	"github.com/Brunoskyy/pulse/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recorder) Notify(e notify.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func setup(t *testing.T, path string) (*Monitor, *recorder) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec := &recorder{}
	cfg := &config.Config{Retention: 90 * 24 * time.Hour, Workers: 2, Checks: []config.Check{
		{ID: "api", Name: "Public API", Kind: config.KindHTTP, FailAfter: 2, RecoverAfter: 2},
	}}
	m := &Monitor{Config: cfg, Store: st, Notifier: rec, Clock: clock.NewFake(time.Unix(1_800_000_000, 0)),
		Log: slog.New(slog.DiscardHandler), StatusURL: "https://status.example.com"}
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, rec
}

func probe(at int, ok bool) check.Result {
	r := check.Result{CheckID: "api", At: time.Unix(1_800_000_000+int64(at), 0), OK: ok, Latency: 20 * time.Millisecond}
	if !ok {
		r.Error = "status 503"
	}
	return r
}

func TestIncidentOpensResolvesAndNotifies(t *testing.T) {
	m, rec := setup(t, filepath.Join(t.TempDir(), "p.db"))
	c := m.Config.Checks[0]
	ctx := context.Background()
	for i, ok := range []bool{true, false, false, false, true, true} {
		m.Record(ctx, c, probe(i*30, ok))
	}
	if len(rec.events) != 2 || rec.events[0].Type != "incident.opened" || rec.events[1].Type != "incident.resolved" {
		t.Fatalf("events: %+v", rec.events)
	}
	opened, resolved := rec.events[0], rec.events[1]
	if !opened.StartedAt.Equal(time.Unix(1_800_000_030, 0)) || opened.Reason != "status 503" {
		t.Fatalf("opened: %+v", opened)
	}
	if resolved.EndedAt == nil || !resolved.EndedAt.Equal(time.Unix(1_800_000_120, 0)) || !resolved.StartedAt.Equal(opened.StartedAt) {
		t.Fatalf("resolved: %+v", resolved)
	}
	if resolved.IncidentID != opened.IncidentID || resolved.StatusURL != "https://status.example.com" {
		t.Fatalf("ids/url: %+v %+v", opened, resolved)
	}
	list, _ := m.Store.Incidents(ctx, time.Unix(0, 0), 10)
	if len(list) != 1 || list[0].EndedAt == nil || list[0].Duration(time.Now()) != 90*time.Second {
		t.Fatalf("stored incident: %+v", list)
	}
	live, counts := m.Snapshot()
	if live["api"].Down || counts["api"].OK != 3 || counts["api"].Failed != 3 {
		t.Fatalf("snapshot: %+v %+v", live, counts)
	}
}

func TestRestartDuringOutageKeepsTheSameIncident(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	m, rec := setup(t, path)
	c := m.Config.Checks[0]
	ctx := context.Background()
	m.Record(ctx, c, probe(0, false))
	m.Record(ctx, c, probe(30, false))
	if len(rec.events) != 1 {
		t.Fatalf("want opened, got %+v", rec.events)
	}
	firstID := rec.events[0].IncidentID

	// A new process on the same database.
	m2, rec2 := setup(t, path)
	if live, _ := m2.Snapshot(); !live["api"].Down {
		t.Fatal("the restarted monitor should show the check as down")
	}
	m2.Record(ctx, c, probe(60, false))
	m2.Record(ctx, c, probe(90, false))
	m2.Record(ctx, c, probe(120, true))
	m2.Record(ctx, c, probe(150, true))
	if len(rec2.events) != 1 || rec2.events[0].Type != "incident.resolved" || rec2.events[0].IncidentID != firstID {
		t.Fatalf("after restart: %+v", rec2.events)
	}
	if !rec2.events[0].StartedAt.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("resolved event should carry the original start: %v", rec2.events[0].StartedAt)
	}
	list, _ := m2.Store.Incidents(ctx, time.Unix(0, 0), 10)
	if len(list) != 1 {
		t.Fatalf("a restart opened a second incident: %+v", list)
	}
}

type fakeProber struct{ now func() time.Time }

func (f fakeProber) Run(_ context.Context, c config.Check) check.Result {
	return check.Result{CheckID: c.ID, At: f.now(), OK: true, Latency: time.Millisecond}
}

func TestStartAndStop(t *testing.T) {
	m, _ := setup(t, filepath.Join(t.TempDir(), "p.db"))
	fc := m.Clock.(*clock.Fake)
	m.Prober = fakeProber{now: fc.Now}
	m.Config.Checks[0].Interval = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		fc.Advance(10 * time.Second)
		if _, counts := m.Snapshot(); counts["api"].OK >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probes did not run")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	m.Stop()
}

func TestRestartRemembersTheStreak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	m, rec := setup(t, path)
	c := m.Config.Checks[0]
	ctx := context.Background()
	m.Record(ctx, c, probe(0, true))
	m.Record(ctx, c, probe(30, false)) // one failure, threshold is two
	if len(rec.events) != 0 {
		t.Fatalf("opened too early: %+v", rec.events)
	}
	m2, rec2 := setup(t, path)
	m2.Record(ctx, c, probe(60, false))
	if len(rec2.events) != 1 || rec2.events[0].Type != "incident.opened" || !rec2.events[0].StartedAt.Equal(time.Unix(1_800_000_030, 0)) {
		t.Fatalf("the failure before the restart should count: %+v", rec2.events)
	}
}

func TestRestartRepairsAnIncidentTheCrashLeftUnwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Two failures stored, but the process died before writing the incident.
	_ = st.AddResult(context.Background(), probe(0, false))
	_ = st.AddResult(context.Background(), probe(30, false))
	st.Close()
	m, _ := setup(t, path)
	open, _ := m.Store.OpenIncidents(context.Background())
	if in, ok := open["api"]; !ok || !in.StartedAt.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("incident not repaired: %+v", open)
	}
	if live, _ := m.Snapshot(); !live["api"].Down {
		t.Fatal("check should show as down")
	}
}

type blockingProber struct{ started chan struct{} }

func (b blockingProber) Run(ctx context.Context, c config.Check) check.Result {
	close(b.started)
	<-ctx.Done()
	return check.Result{CheckID: c.ID, At: time.Now(), OK: false, Error: "context canceled"}
}

func TestShutdownDoesNotRecordCancelledProbes(t *testing.T) {
	m, _ := setup(t, filepath.Join(t.TempDir(), "p.db"))
	m.Clock = clock.Real{}
	bp := blockingProber{started: make(chan struct{})}
	m.Prober = bp
	m.Rand = func() float64 { return 0 } // first run immediately
	m.Config.Checks[0].Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	<-bp.started
	cancel()
	m.Stop()
	if _, counts := m.Snapshot(); counts["api"].Failed != 0 {
		t.Fatal("a probe cancelled by shutdown was recorded as a failure")
	}
}
