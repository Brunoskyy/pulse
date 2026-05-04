package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// verify is what a receiver would write: split the header, recompute the MAC
// over "<t>.<body>", compare in constant time, and reject old timestamps.
func verify(secret []byte, header string, body []byte, now time.Time) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return err
	}
	if now.Sub(time.Unix(sec, 0)) > 5*time.Minute {
		return errors.New("too old")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "." + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("bad signature")
	}
	return nil
}

func TestSignKnownVector(t *testing.T) {
	// Computed outside Go: printf '1700000000.{"a":1}' | openssl dgst -sha256 -hmac whsec
	got := Sign([]byte("whsec"), time.Unix(1700000000, 0), []byte(`{"a":1}`))
	want := "t=1700000000,v1=8ad37ba156048ae0e0a5533c75cdf26fee88b07f93cb57ee4c80adb053012032"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestWebhookIsSignedAndVerifiable(t *testing.T) {
	var gotHeader string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(SignatureHeader)
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	now := time.Unix(1_800_000_000, 0)
	wh := &Webhook{URL: srv.URL, Secret: []byte("s3cr3t"), Now: func() time.Time { return now }}
	e := Event{Type: "incident.opened", IncidentID: 7, CheckID: "api", CheckName: "Public API", Reason: "timeout", StartedAt: now}
	if err := wh.Send(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := verify([]byte("s3cr3t"), gotHeader, gotBody, now.Add(time.Minute)); err != nil {
		t.Fatalf("receiver could not verify: %v (%s)", err, gotHeader)
	}
	if err := verify([]byte("wrong"), gotHeader, gotBody, now); err == nil {
		t.Fatal("wrong secret verified")
	}
	var decoded Event
	if err := json.Unmarshal(gotBody, &decoded); err != nil || decoded.CheckID != "api" || decoded.IncidentID != 7 {
		t.Fatalf("body: %s", gotBody)
	}
}

func TestSlackText(t *testing.T) {
	var text string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		text = m["text"]
	}))
	defer srv.Close()
	s := &Slack{URL: srv.URL}
	start := time.Unix(0, 0)
	end := start.Add(90 * time.Second)
	_ = s.Send(context.Background(), Event{Type: "incident.resolved", CheckName: "Search", StartedAt: start, EndedAt: &end, StatusURL: "https://status.example.com"})
	if text != ":large_green_circle: Search recovered after 1m30s (https://status.example.com)" {
		t.Fatalf("text: %q", text)
	}
}

type flaky struct {
	fails int32
	calls atomic.Int32
	mu    sync.Mutex
	got   []Event
}

func (f *flaky) Send(_ context.Context, e Event) error {
	if f.calls.Add(1) <= f.fails {
		return errors.New("receiver down")
	}
	f.mu.Lock()
	f.got = append(f.got, e)
	f.mu.Unlock()
	return nil
}

func TestDispatcherRetriesThenDelivers(t *testing.T) {
	f := &flaky{fails: 2}
	d := NewDispatcher(slog.New(slog.DiscardHandler), f)
	d.Backoff = func(int) time.Duration { return time.Millisecond }
	d.Start(context.Background())
	d.Notify(Event{Type: "incident.opened", CheckID: "api"})
	d.Close()
	if f.calls.Load() != 3 || len(f.got) != 1 {
		t.Fatalf("calls=%d delivered=%d", f.calls.Load(), len(f.got))
	}
}

func TestDispatcherGivesUpAndNeverBlocks(t *testing.T) {
	f := &flaky{fails: 1000}
	d := NewDispatcher(slog.New(slog.DiscardHandler), f)
	d.Backoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	start := time.Now()
	for range 1000 { // far more than the queue holds
		d.Notify(Event{Type: "incident.opened"})
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Notify blocked on a full queue")
	}
	cancel() // cuts the hour-long backoff short
	d.Close()
}
