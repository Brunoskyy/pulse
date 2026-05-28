// Package demo starts a few fake services and fills 90 days of history, so
// the status page has something to show without pointing it at real systems.
package demo

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/store"
)

// Service is one fake HTTP endpoint with a latency profile and an on/off switch.
type Service struct {
	ID       string
	Name     string
	Group    string
	MinMS    int
	MaxMS    int
	FailRate float64 // probability a request answers 503

	down atomic.Bool
	addr string
	srv  *http.Server
}

// Down makes the service refuse with 503 until Up is called.
func (s *Service) Down()       { s.down.Store(true) }
func (s *Service) Up()         { s.down.Store(false) }
func (s *Service) URL() string { return "http://" + s.addr + "/health" }

// Fleet is the set of fake services plus a small control API.
type Fleet struct {
	Services []*Service
	Control  string // base URL of the control API
	ctl      *http.Server
	wg       sync.WaitGroup
}

// DefaultServices is what `pulse demo` runs.
func DefaultServices() []*Service {
	return []*Service{
		{ID: "api", Name: "Public API", Group: "Core", MinMS: 18, MaxMS: 70},
		{ID: "checkout", Name: "Checkout", Group: "Core", MinMS: 70, MaxMS: 190},
		{ID: "webhooks", Name: "Webhook delivery", Group: "Integrations", MinMS: 30, MaxMS: 120, FailRate: 0.04},
		{ID: "search", Name: "Search", Group: "Integrations", MinMS: 40, MaxMS: 260},
	}
}

// Start listens on loopback ports picked by the OS.
func Start(services []*Service) (*Fleet, error) {
	f := &Fleet{Services: services}
	for _, s := range services {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			f.Close()
			return nil, err
		}
		s.addr = ln.Addr().String()
		svc := s
		s.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(time.Duration(svc.MinMS+rand.IntN(svc.MaxMS-svc.MinMS+1)) * time.Millisecond)
			if svc.down.Load() || rand.Float64() < svc.FailRate {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintln(w, "ok")
		})}
		f.wg.Add(1)
		go func() { defer f.wg.Done(); _ = svc.srv.Serve(ln) }()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.Close()
		return nil, err
	}
	f.Control = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /down/{id}", f.toggle(true))
	mux.HandleFunc("POST /up/{id}", f.toggle(false))
	f.ctl = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	f.wg.Add(1)
	go func() { defer f.wg.Done(); _ = f.ctl.Serve(ln) }()
	return f, nil
}

func (f *Fleet) toggle(down bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, s := range f.Services {
			if s.ID == r.PathValue("id") {
				if down {
					s.Down()
				} else {
					s.Up()
				}
				fmt.Fprintf(w, "%s is %s\n", s.ID, map[bool]string{true: "down", false: "up"}[down])
				return
			}
		}
		http.NotFound(w, r)
	}
}

// Close stops every fake service and waits for them.
func (f *Fleet) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, s := range f.Services {
		if s.srv != nil {
			_ = s.srv.Shutdown(ctx)
		}
	}
	if f.ctl != nil {
		_ = f.ctl.Shutdown(ctx)
	}
	f.wg.Wait()
}

// Config returns a Pulse config pointing at the fleet, with short intervals
// so an incident opens and closes within a minute.
func (f *Fleet) Config(listen, database string) *config.Config {
	c := &config.Config{Title: "Northwind status", Listen: listen, Database: database, Retention: 90 * 24 * time.Hour, Workers: 8}
	for _, s := range f.Services {
		c.Checks = append(c.Checks, config.Check{
			ID: s.ID, Name: s.Name, Group: s.Group, Kind: config.KindHTTP, Target: s.URL(), Method: "GET",
			Interval: 5 * time.Second, Timeout: 2 * time.Second, FailAfter: 2, RecoverAfter: 2,
			// The backfill is one probe every ten minutes; let each of those
			// keep its ten minutes of weight next to the live five-second ones.
			MaxGap: 10 * time.Minute,
		})
	}
	return c
}

// outage is a scripted incident in the backfill.
type outage struct {
	check   string
	daysAgo int
	hour    int
	minutes int
	reason  string
}

var history = []outage{
	{"webhooks", 3, 14, 42, "status 503"},
	{"webhooks", 19, 2, 18, "timeout"},
	{"webhooks", 47, 9, 95, "status 502"},
	{"checkout", 11, 20, 26, "status 500"},
	{"api", 33, 6, 8, "connection refused"},
	{"search", 61, 16, 170, "timeout"},
	{"search", 74, 11, 12, "status 503"},
}

// Backfill writes 90 days of probes every ten minutes for each service,
// ending one interval before now, with the scripted outages above. It is
// deterministic, so the page looks the same every time the demo starts.
func Backfill(ctx context.Context, st *store.Store, services []*Service, now time.Time) error {
	rng := rand.New(rand.NewPCG(42, 7))
	step := 10 * time.Minute
	start := now.Add(-90 * 24 * time.Hour).Truncate(step).Add(step)
	end := now.Add(-step)
	for _, s := range services {
		var outs []outage
		for _, o := range history {
			if o.check == s.ID {
				outs = append(outs, o)
			}
		}
		var batch []check.Result
		for t := start; t.Before(end); t = t.Add(step) {
			r := check.Result{CheckID: s.ID, At: t, OK: true,
				Latency: time.Duration(s.MinMS+rng.IntN(s.MaxMS-s.MinMS+1)) * time.Millisecond}
			for _, o := range outs {
				if os, oe := o.window(now); !t.Before(os) && t.Before(oe) {
					r.OK, r.Error = false, o.reason
				}
			}
			batch = append(batch, r)
		}
		if err := st.AddResults(ctx, batch); err != nil {
			return err
		}
		for _, o := range outs {
			os, oe := o.window(now)
			if err := st.AddIncident(ctx, store.Incident{CheckID: s.ID, StartedAt: os, EndedAt: &oe, Reason: o.reason}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o outage) window(now time.Time) (time.Time, time.Time) {
	day := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -o.daysAgo)
	s := day.Add(time.Duration(o.hour) * time.Hour)
	return s, s.Add(time.Duration(o.minutes) * time.Minute)
}
