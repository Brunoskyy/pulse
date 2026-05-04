// Package notify tells the outside world when an incident opens or resolves.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Event is what gets sent.
type Event struct {
	Type       string     `json:"type"` // "incident.opened" or "incident.resolved"
	IncidentID int64      `json:"incident_id"`
	CheckID    string     `json:"check_id"`
	CheckName  string     `json:"check_name"`
	Reason     string     `json:"reason,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	StatusURL  string     `json:"status_url,omitempty"`
}

// Sender delivers one event.
type Sender interface {
	Send(ctx context.Context, e Event) error
}

// SignatureHeader carries `t=<unix seconds>,v1=<hex hmac-sha256>`, where the
// MAC covers "<t>.<body>". The same scheme Stripe uses, so any receiver that
// already verifies those can verify these.
const SignatureHeader = "Pulse-Signature"

// Sign returns the header value for body at time t.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Webhook posts the event as JSON, signed when a secret is set.
type Webhook struct {
	URL    string
	Secret []byte
	Client *http.Client
	Now    func() time.Time
}

func (w *Webhook) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pulse/1")
	if len(w.Secret) > 0 {
		now := time.Now
		if w.Now != nil {
			now = w.Now
		}
		req.Header.Set(SignatureHeader, Sign(w.Secret, now(), body))
	}
	return do(w.Client, req)
}

// Slack posts a one-line message to an incoming-webhook URL. Anything that
// accepts Slack's `{"text": ...}` shape works, which includes Mattermost and
// Discord's /slack endpoint.
type Slack struct {
	URL    string
	Client *http.Client
}

func (s *Slack) Send(ctx context.Context, e Event) error {
	var text string
	switch e.Type {
	case "incident.opened":
		text = fmt.Sprintf(":red_circle: %s is down: %s", e.CheckName, e.Reason)
	default:
		d := time.Duration(0)
		if e.EndedAt != nil {
			d = e.EndedAt.Sub(e.StartedAt).Round(time.Second)
		}
		text = fmt.Sprintf(":large_green_circle: %s recovered after %s", e.CheckName, d)
	}
	if e.StatusURL != "" {
		text += " (" + e.StatusURL + ")"
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(s.Client, req)
}

func do(c *http.Client, req *http.Request) error {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// Dispatcher sends events in the background so a slow or dead receiver never
// delays a probe. Each event is retried a few times with backoff; the queue is
// bounded, and when it is full new events are dropped and logged rather than
// blocking the monitor.
type Dispatcher struct {
	Senders  []Sender
	Log      *slog.Logger
	Attempts int
	Backoff  func(attempt int) time.Duration

	queue chan Event
	wg    sync.WaitGroup
	once  sync.Once
}

func NewDispatcher(log *slog.Logger, senders ...Sender) *Dispatcher {
	return &Dispatcher{
		Senders:  senders,
		Log:      log,
		Attempts: 4,
		Backoff:  func(a int) time.Duration { return time.Duration(1<<a) * time.Second },
		queue:    make(chan Event, 256),
	}
}

// Start runs the delivery goroutine until ctx is done or Close is called.
func (d *Dispatcher) Start(ctx context.Context) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for e := range d.queue {
			for _, s := range d.Senders {
				d.deliver(ctx, s, e)
			}
		}
	}()
}

// Notify queues an event without blocking.
func (d *Dispatcher) Notify(e Event) {
	if len(d.Senders) == 0 {
		return
	}
	select {
	case d.queue <- e:
	default:
		d.Log.Warn("notification queue full, dropping event", "type", e.Type, "check", e.CheckID)
	}
}

// Close stops accepting events and waits for the queue to drain. Retries that
// are sleeping are cut short when the Start context is cancelled.
func (d *Dispatcher) Close() {
	d.once.Do(func() { close(d.queue) })
	d.wg.Wait()
}

func (d *Dispatcher) deliver(ctx context.Context, s Sender, e Event) {
	for attempt := range d.Attempts {
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.Send(sendCtx, e)
		cancel()
		if err == nil {
			return
		}
		d.Log.Warn("notification failed", "type", e.Type, "check", e.CheckID, "attempt", attempt+1, "err", err)
		if attempt == d.Attempts-1 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.Backoff(attempt)):
		}
	}
}
