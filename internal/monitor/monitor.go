// Package monitor wires probes, storage, incidents and notifications together.
package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/incident"
	"github.com/Brunoskyy/pulse/internal/notify"
	"github.com/Brunoskyy/pulse/internal/pool"
	"github.com/Brunoskyy/pulse/internal/scheduler"
	"github.com/Brunoskyy/pulse/internal/store"
)

// Prober runs one check. check.Runner satisfies it; tests use fakes.
type Prober interface {
	Run(ctx context.Context, c config.Check) check.Result
}

// Notifier is the part of the dispatcher the monitor uses.
type Notifier interface {
	Notify(e notify.Event)
}

// Monitor owns the live state of every check.
type Monitor struct {
	Config    *config.Config
	Store     *store.Store
	Prober    Prober
	Notifier  Notifier
	Clock     clock.Clock
	Log       *slog.Logger
	StatusURL string
	// Rand overrides the scheduler's random source; tests set it.
	Rand func() float64

	mu       sync.Mutex
	trackers map[string]*incident.Tracker
	open     map[string]openIncident
	live     map[string]Live
	counts   map[string]*Counts

	sched *scheduler.Scheduler
	pool  *pool.Pool
}

type openIncident struct {
	id      int64
	started time.Time
}

// Live is the latest state of one check, kept in memory for the status page
// and /metrics so neither has to query the database for it.
type Live struct {
	Last    check.Result
	HasData bool
	Down    bool
}

// Counts are monotonically increasing totals for /metrics.
type Counts struct {
	OK, Failed int64
}

// Load restores open incidents from the store, so a restart in the middle of
// an outage keeps the incident it already had instead of opening another.
func (m *Monitor) Load(ctx context.Context) error {
	m.trackers = map[string]*incident.Tracker{}
	m.open = map[string]openIncident{}
	m.live = map[string]Live{}
	m.counts = map[string]*Counts{}
	open, err := m.Store.OpenIncidents(ctx)
	if err != nil {
		return err
	}
	for _, c := range m.Config.Checks {
		t := &incident.Tracker{FailAfter: c.FailAfter, RecoverAfter: c.RecoverAfter}
		if in, ok := open[c.ID]; ok {
			t.Resume()
			m.open[c.ID] = openIncident{id: in.ID, started: in.StartedAt}
		}
		m.trackers[c.ID] = t
		m.counts[c.ID] = &Counts{}
		if err := m.replay(ctx, c, t); err != nil {
			return err
		}
	}
	// An incident left open for a check that is no longer configured would
	// stay open forever; close it at the time of its last known state.
	for id, in := range open {
		if _, ok := m.trackers[id]; !ok {
			_ = m.Store.CloseIncident(ctx, in.ID, m.Clock.Now())
		}
	}
	return nil
}

// replay feeds the most recent results back into a fresh tracker, so a
// restart remembers that the last two probes failed instead of starting the
// streak from zero. It also repairs the one inconsistency a crash can leave:
// results that crossed a threshold without the incident row being written
// (or closed) before the process died.
func (m *Monitor) replay(ctx context.Context, c config.Check, t *incident.Tracker) error {
	n := max(c.FailAfter, c.RecoverAfter)
	recent, err := m.Store.Recent(ctx, c.ID, n)
	if err != nil {
		return err
	}
	for _, r := range recent {
		switch tr, at, reason := t.Observe(r); tr {
		case incident.Opened:
			id, err := m.Store.OpenIncident(ctx, c.ID, at, reason)
			if err != nil {
				return err
			}
			m.open[c.ID] = openIncident{id: id, started: at}
			m.Log.Warn("incident opened on restart", "check", c.ID, "reason", reason)
		case incident.Resolved:
			if in, ok := m.open[c.ID]; ok {
				if err := m.Store.CloseIncident(ctx, in.id, at); err != nil {
					return err
				}
				delete(m.open, c.ID)
				m.Log.Info("incident closed on restart", "check", c.ID)
			}
		}
	}
	if len(recent) > 0 {
		m.live[c.ID] = Live{Last: recent[len(recent)-1], HasData: true, Down: t.Open()}
	}
	return nil
}

// Start begins probing. It returns immediately; Stop waits for everything
// started here to finish.
func (m *Monitor) Start(ctx context.Context) {
	m.pool = pool.New(m.Config.Workers)
	m.sched = &scheduler.Scheduler{
		Clock: m.Clock,
		Pool:  m.pool,
		Rand:  m.Rand,
		Run: func(ctx context.Context, c config.Check) {
			r := m.Prober.Run(ctx, c)
			// A probe cut short because Pulse is stopping says nothing about
			// the target. Recording it would put a failure on the page after
			// every deploy.
			if ctx.Err() != nil {
				return
			}
			m.Record(ctx, c, r)
		},
	}
	m.sched.Start(ctx, m.Config.Checks)
}

// Stop waits for the scheduler loops (which exit on ctx cancel) and then for
// probes already running.
func (m *Monitor) Stop() {
	if m.sched != nil {
		m.sched.Wait()
	}
	if m.pool != nil {
		m.pool.Close()
	}
}

// Skipped reports how many ticks were skipped because a probe was still running.
func (m *Monitor) Skipped() int64 {
	if m.sched == nil {
		return 0
	}
	return m.sched.Skipped.Load()
}

// Record stores a result and advances the check's incident state. Results for
// one check arrive in order because the scheduler never overlaps a check with
// itself.
func (m *Monitor) Record(ctx context.Context, c config.Check, r check.Result) {
	// Writes use a context detached from shutdown: a probe that finished is
	// worth keeping even if the signal to stop arrived while it ran.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.Store.AddResult(wctx, r); err != nil {
		m.Log.Error("store result", "check", c.ID, "err", err)
	}

	m.mu.Lock()
	t := m.trackers[c.ID]
	tr, at, reason := t.Observe(r)
	cnt := m.counts[c.ID]
	if r.OK {
		cnt.OK++
	} else {
		cnt.Failed++
	}
	openIn := m.open[c.ID]
	m.mu.Unlock()

	switch tr {
	case incident.Opened:
		id, err := m.Store.OpenIncident(wctx, c.ID, at, reason)
		if err != nil {
			m.Log.Error("open incident", "check", c.ID, "err", err)
		}
		m.mu.Lock()
		m.open[c.ID] = openIncident{id: id, started: at}
		m.mu.Unlock()
		m.Log.Warn("incident opened", "check", c.ID, "reason", reason)
		m.notify(notify.Event{Type: "incident.opened", IncidentID: id, CheckID: c.ID, CheckName: c.Name, Reason: reason, StartedAt: at})
	case incident.Resolved:
		if openIn.id != 0 {
			if err := m.Store.CloseIncident(wctx, openIn.id, at); err != nil {
				m.Log.Error("close incident", "check", c.ID, "err", err)
			}
		}
		m.mu.Lock()
		delete(m.open, c.ID)
		m.mu.Unlock()
		end := at
		m.Log.Info("incident resolved", "check", c.ID, "after", at.Sub(openIn.started).Round(time.Second))
		m.notify(notify.Event{Type: "incident.resolved", IncidentID: openIn.id, CheckID: c.ID, CheckName: c.Name, StartedAt: openIn.started, EndedAt: &end})
	}

	m.mu.Lock()
	m.live[c.ID] = Live{Last: r, HasData: true, Down: t.Open()}
	m.mu.Unlock()
}

func (m *Monitor) notify(e notify.Event) {
	if m.Notifier == nil {
		return
	}
	e.StatusURL = m.StatusURL
	m.Notifier.Notify(e)
}

// Snapshot returns a copy of every check's live state and counters.
func (m *Monitor) Snapshot() (map[string]Live, map[string]Counts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	live := make(map[string]Live, len(m.live))
	for k, v := range m.live {
		live[k] = v
	}
	counts := make(map[string]Counts, len(m.counts))
	for k, v := range m.counts {
		counts[k] = *v
	}
	return live, counts
}

// Prune runs retention on an hourly loop until ctx is done.
func (m *Monitor) Prune(ctx context.Context) {
	for {
		n, err := m.Store.Prune(ctx, m.Clock.Now().Add(-m.Config.Retention))
		if err != nil && ctx.Err() == nil {
			m.Log.Error("prune", "err", err)
		} else if n > 0 {
			m.Log.Info("pruned old results", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.Clock.After(time.Hour):
		}
	}
}
