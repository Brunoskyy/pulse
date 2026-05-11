// Package web serves the status page, the JSON API and /metrics.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/monitor"
	"github.com/Brunoskyy/pulse/internal/store"
)

//go:embed assets
var assets embed.FS

// Server renders pages from the monitor's live state and the store's history.
type Server struct {
	Config  *config.Config
	Store   *store.Store
	Monitor *monitor.Monitor
	Clock   clock.Clock
	Log     *slog.Logger
	// CacheFor is how long a built page is reused. The page runs a few
	// queries per check; a status page that goes viral should not turn every
	// visitor into that many queries.
	CacheFor time.Duration

	tmpl   *template.Template
	mu     sync.Mutex
	cached *Page
	until  time.Time
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	s.tmpl = template.Must(template.New("page.html").Funcs(funcs).ParseFS(assets, "assets/page.html"))
	static, _ := fs.Sub(assets, "assets")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /api/incidents", s.apiIncidents)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))
	return secure(mux)
}

func staticHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".html") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// secure sets headers that cost nothing: the page is read-only and loads
// nothing from anywhere else, so the policy can be strict.
func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) load(ctx context.Context) (*Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Clock.Now()
	if s.cached != nil && now.Before(s.until) {
		return s.cached, nil
	}
	p, err := s.build(ctx)
	if err != nil {
		return nil, err
	}
	s.cached, s.until = p, now.Add(s.CacheFor)
	return p, nil
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	p, err := s.load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := s.tmpl.Execute(w, p); err != nil {
		s.Log.Error("render", "err", err)
	}
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	p, err := s.load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) apiIncidents(w http.ResponseWriter, r *http.Request) {
	p, err := s.load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"incidents": p.Incidents})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("build page", "err", err)
	http.Error(w, "status is temporarily unavailable", http.StatusServiceUnavailable)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// metrics writes the Prometheus text exposition format by hand. It is a few
// lines, and not pulling in the client library keeps the binary small.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	live, counts := s.Monitor.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	ids := make([]string, 0, len(s.Config.Checks))
	for _, c := range s.Config.Checks {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)

	b.WriteString("# HELP pulse_check_up Whether the check is currently considered up (1) or in an incident (0).\n# TYPE pulse_check_up gauge\n")
	for _, id := range ids {
		if l, ok := live[id]; ok && l.HasData {
			v := 1
			if l.Down {
				v = 0
			}
			fmt.Fprintf(&b, "pulse_check_up{check=%s} %d\n", label(id), v)
		}
	}
	b.WriteString("# HELP pulse_check_latency_seconds Latency of the most recent probe.\n# TYPE pulse_check_latency_seconds gauge\n")
	for _, id := range ids {
		if l, ok := live[id]; ok && l.HasData {
			fmt.Fprintf(&b, "pulse_check_latency_seconds{check=%s} %g\n", label(id), l.Last.Latency.Seconds())
		}
	}
	b.WriteString("# HELP pulse_probes_total Probes run since start, by result.\n# TYPE pulse_probes_total counter\n")
	for _, id := range ids {
		c := counts[id]
		fmt.Fprintf(&b, "pulse_probes_total{check=%s,result=\"ok\"} %d\n", label(id), c.OK)
		fmt.Fprintf(&b, "pulse_probes_total{check=%s,result=\"failed\"} %d\n", label(id), c.Failed)
	}
	b.WriteString("# HELP pulse_skipped_ticks_total Ticks skipped because the previous probe of the same check was still running.\n# TYPE pulse_skipped_ticks_total counter\n")
	fmt.Fprintf(&b, "pulse_skipped_ticks_total %d\n", s.Monitor.Skipped())
	_, _ = w.Write([]byte(b.String()))
}

// label quotes a Prometheus label value: backslash, double quote and newline
// are the three characters the format requires escaping.
func label(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}

var funcs = template.FuncMap{
	"pct": func(u float64) string {
		if u < 0 {
			return "—"
		}
		if u >= 1 {
			return "100%"
		}
		// Never round 99.996% up to 100%: a status page that says 100% while
		// listing an incident is lying.
		v := float64(int(u*10000)) / 100
		return fmt.Sprintf("%.2f%%", v)
	},
	"ms": func(v float64) string {
		if v <= 0 {
			return "—"
		}
		if v >= 1000 {
			return fmt.Sprintf("%.1fs", v/1000)
		}
		return fmt.Sprintf("%.0f ms", v)
	},
	"when": func(t time.Time) string { return t.UTC().Format("Jan 2, 15:04 UTC") },
	"iso":  func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"ago": func(now time.Time, t *time.Time) string {
		if t == nil {
			return "never"
		}
		d := now.Sub(*t)
		if d < time.Minute {
			return fmt.Sprintf("%ds ago", int(d.Seconds()))
		}
		return HumanDuration(d) + " ago"
	},
	"daytitle": func(d DayCell) string {
		if d.Uptime < 0 {
			return d.Date + ": no data"
		}
		v := float64(int(d.Uptime*10000)) / 100
		return fmt.Sprintf("%s: %.2f%% up", d.Date, v)
	},
	"strip": func(days []DayCell) string {
		good, bad, none := 0, 0, 0
		for _, d := range days {
			switch d.Level {
			case "ok":
				good++
			case "none":
				none++
			default:
				bad++
			}
		}
		return fmt.Sprintf("Last %d days: %d without issues, %d with downtime, %d without data", len(days), good, bad, none)
	},
}
