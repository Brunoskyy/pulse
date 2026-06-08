// Package notify tells the outside world when an incident opens or resolves.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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

// slackEscape applies Slack's own escaping. The reason comes from whatever
// the probed target answered, so without this a monitored site could put an
// @channel ping or a link with a made-up label into the on-call channel.
var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

func (s *Slack) Send(ctx context.Context, e Event) error {
	var text string
	switch e.Type {
	case "incident.opened":
		text = fmt.Sprintf(":red_circle: %s is down: %s", slackEscape(e.CheckName), slackEscape(e.Reason))
	default:
		d := time.Duration(0)
		if e.EndedAt != nil {
			d = e.EndedAt.Sub(e.StartedAt).Round(time.Second)
		}
		text = fmt.Sprintf(":large_green_circle: %s recovered after %s", slackEscape(e.CheckName), d)
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
		return &StatusError{Code: resp.StatusCode}
	}
	return nil
}

// StatusError is a receiver answering with something other than 2xx.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("status %d", e.Code) }

// Retryable reports whether trying the same request again could succeed.
// Network errors, 5xx, 408 and 429 can; any other 4xx (a revoked Slack URL, a
// receiver that rejects the payload) will fail the same way every time.
func Retryable(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return true
	}
	return se.Code >= 500 || se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests
}

// Dispatcher sends events in the background so a slow or dead receiver never
// delays a probe. Every sender has its own bounded queue and goroutine, so a
// Slack URL that blackholes does not hold back the webhook. Each event is
// retried with backoff while the failure is retryable; when a queue is full
// new events for that sender are dropped and logged rather than blocking.
type Dispatcher struct {
	Senders  []Sender
	Log      *slog.Logger
	Attempts int
	Backoff  func(attempt int) time.Duration

	queues []chan Event
	wg     sync.WaitGroup
	once   sync.Once
	start  sync.Once
}

const queueSize = 256

func NewDispatcher(log *slog.Logger, senders ...Sender) *Dispatcher {
	d := &Dispatcher{
		Senders:  senders,
		Log:      log,
		Attempts: 4,
		Backoff:  func(a int) time.Duration { return time.Duration(1<<a) * time.Second },
	}
	for range senders {
		d.queues = append(d.queues, make(chan Event, queueSize))
	}
	return d
}

// Start runs one delivery goroutine per sender. Retries that are waiting are
// cut short when ctx is done; events still queued at that point are dropped
// and counted in the log.
func (d *Dispatcher) Start(ctx context.Context) {
	d.start.Do(func() {
		for i, s := range d.Senders {
			d.wg.Add(1)
			go func(s Sender, q chan Event) {
				defer d.wg.Done()
				for e := range q {
					if ctx.Err() != nil {
						d.Log.Warn("shutting down, notification not sent", "type", e.Type, "check", e.CheckID)
						continue
					}
					d.deliver(ctx, s, e)
				}
			}(s, d.queues[i])
		}
	})
}

// Notify queues an event for every sender without blocking.
func (d *Dispatcher) Notify(e Event) {
	for _, q := range d.queues {
		select {
		case q <- e:
		default:
			d.Log.Warn("notification queue full, dropping event", "type", e.Type, "check", e.CheckID)
		}
	}
}

// Close stops accepting events and waits for the queues to drain, which is
// bounded by the Start context: cancel it at the shutdown deadline.
func (d *Dispatcher) Close() {
	d.once.Do(func() {
		for _, q := range d.queues {
			close(q)
		}
	})
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
		if attempt == d.Attempts-1 || !Retryable(err) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.Backoff(attempt)):
		}
	}
}
