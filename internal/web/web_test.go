package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/monitor"
	"github.com/Brunoskyy/pulse/internal/store"
)

var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func server(t *testing.T, checks ...config.Check) (*httptest.Server, *monitor.Monitor) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Title: "Northwind status", Retention: 90 * 24 * time.Hour, Workers: 1, Checks: checks}
	fc := clock.NewFake(now)
	m := &monitor.Monitor{Config: cfg, Store: st, Clock: fc, Log: slog.New(slog.DiscardHandler)}
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: cfg, Store: st, Monitor: m, Clock: fc, Log: slog.New(slog.DiscardHandler)}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, m
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestPageRendersAndEscapes(t *testing.T) {
	evil := config.Check{ID: "api", Name: `<script>alert(1)</script>`, Group: "Core", Kind: config.KindHTTP, FailAfter: 1, RecoverAfter: 1}
	fine := config.Check{ID: "db", Name: "Database", Group: "Core", Kind: config.KindTCP, FailAfter: 1, RecoverAfter: 1}
	ts, m := server(t, evil, fine)
	m.Record(context.Background(), evil, check.Result{CheckID: "api", At: now.Add(-time.Minute), OK: false, Error: `<img src=x onerror=alert(2)>`})
	m.Record(context.Background(), fine, check.Result{CheckID: "db", At: now.Add(-time.Minute), OK: true})
	res, body := get(t, ts.URL+"/")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
		t.Fatal("check name or error reached the page unescaped")
	}
	for _, want := range []string{"&lt;script&gt;", "1 service down", "Ongoing", "Core"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("csp: %q", csp)
	}
}

func TestStatusJSON(t *testing.T) {
	c := config.Check{ID: "api", Name: "Public API", Kind: config.KindHTTP, FailAfter: 3, RecoverAfter: 2}
	ts, m := server(t, c)
	for i := range 10 {
		m.Record(context.Background(), c, check.Result{CheckID: "api", At: now.Add(-time.Duration(10-i) * time.Minute), OK: i != 4, Latency: time.Duration(10+i) * time.Millisecond})
	}
	_, body := get(t, ts.URL+"/api/status")
	var p Page
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	row := p.Groups[0].Checks[0]
	if p.Overall != "operational" || row.State != "up" || row.Uptime24h != 0.9 || len(row.Days) != 90 {
		t.Fatalf("json: overall=%s state=%s uptime=%v days=%d", p.Overall, row.State, row.Uptime24h, len(row.Days))
	}
	if row.Days[89].Level != "major" || row.Days[0].Level != "none" {
		t.Fatalf("today should be major (90%%), 89 days ago none: %+v %+v", row.Days[89], row.Days[0])
	}
}

func TestMetricsFormat(t *testing.T) {
	c := config.Check{ID: "api", Name: "API", Kind: config.KindHTTP, FailAfter: 1, RecoverAfter: 1}
	ts, m := server(t, c)
	m.Record(context.Background(), c, check.Result{CheckID: "api", At: now, OK: true, Latency: 250 * time.Millisecond})
	res, body := get(t, ts.URL+"/metrics")
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", res.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		`pulse_check_up{check="api"} 1`,
		`pulse_check_latency_seconds{check="api"} 0.25`,
		`pulse_probes_total{check="api",result="ok"} 1`,
		`pulse_probes_total{check="api",result="failed"} 0`,
		"# TYPE pulse_probes_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	if got := label("a\"b\\c\nd"); got != `"a\"b\\c\nd"` {
		t.Fatalf("got %s", got)
	}
}

func TestPercentNeverRoundsUpToFull(t *testing.T) {
	pct := funcs["pct"].(func(float64) string)
	for in, want := range map[float64]string{1: "100%", 0.99996: "99.99%", 0.5: "50.00%", -1: "—"} {
		if got := pct(in); got != want {
			t.Errorf("pct(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestLevelsAndDurations(t *testing.T) {
	for in, want := range map[float64]string{-1: "none", 1: "ok", 0.999: "ok", 0.995: "minor", 0.98: "major"} {
		if got := Level(in); got != want {
			t.Errorf("Level(%v) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{30 * time.Second: "30s", 3 * time.Minute: "3m", 72 * time.Minute: "1h 12m", 52 * time.Hour: "2d 4h"} {
		if got := HumanDuration(in); got != want {
			t.Errorf("HumanDuration(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestStaticDoesNotServeTemplates(t *testing.T) {
	ts, _ := server(t, config.Check{ID: "a", Kind: config.KindDNS, FailAfter: 1, RecoverAfter: 1})
	if res, _ := get(t, ts.URL+"/static/page.html"); res.StatusCode != 404 {
		t.Fatalf("template served: %d", res.StatusCode)
	}
	if res, _ := get(t, ts.URL+"/static/style.css"); res.StatusCode != 200 {
		t.Fatalf("css: %d", res.StatusCode)
	}
}
