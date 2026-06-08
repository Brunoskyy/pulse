// Package check runs one probe and reports what it saw.
package check

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Brunoskyy/pulse/internal/config"
)

// Result is one probe's outcome.
type Result struct {
	CheckID string
	At      time.Time
	OK      bool
	Latency time.Duration
	// Error is a short human reason when OK is false.
	Error string
}

// Runner executes checks. It holds the HTTP client and resolver so they can be
// replaced in tests.
type Runner struct {
	HTTP     *http.Client
	Resolver *net.Resolver
	Dialer   *net.Dialer
	Now      func() time.Time
	// RootCAs overrides the system pool for TLS checks; nil means system roots.
	RootCAs *x509.CertPool
}

// NewRunner returns a Runner with sane transport settings. Redirects are
// followed up to five hops; keep-alives are off so every probe measures a
// fresh connection, which is what a user on a cold path would see.
func NewRunner() *Runner {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &Runner{
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext:            dialer.DialContext,
				DisableKeepAlives:      true,
				TLSHandshakeTimeout:    10 * time.Second,
				ResponseHeaderTimeout:  10 * time.Second,
				MaxResponseHeaderBytes: 64 << 10,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("more than 5 redirects")
				}
				return nil
			},
		},
		Resolver: net.DefaultResolver,
		Dialer:   dialer,
		Now:      time.Now,
	}
}

// Run executes one check with its own timeout. It never panics and never
// blocks longer than the check's timeout.
func (r *Runner) Run(ctx context.Context, c config.Check) Result {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	start := r.Now()
	var err error
	switch c.Kind {
	case config.KindHTTP:
		err = r.http(ctx, c)
	case config.KindTCP:
		err = r.tcp(ctx, c)
	case config.KindDNS:
		err = r.dns(ctx, c)
	case config.KindTLS:
		err = r.tls(ctx, c, start)
	default:
		err = fmt.Errorf("unknown kind %q", c.Kind)
	}
	res := Result{CheckID: c.ID, At: start, Latency: r.Now().Sub(start), OK: err == nil}
	if err != nil {
		res.Error = describe(ctx, err)
	}
	return res
}

func (r *Runner) http(ctx context.Context, c config.Check) error {
	req, err := http.NewRequestWithContext(ctx, c.Method, c.Target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pulse/1 (+uptime check)")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	expected := c.ExpectStatus
	ok := false
	if len(expected) == 0 {
		ok = resp.StatusCode >= 200 && resp.StatusCode < 400
	} else {
		ok = slices.Contains(expected, resp.StatusCode)
	}
	if !ok {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if c.ExpectContains != "" {
		// Only the first megabyte is searched; a status page is not a download.
		found, err := contains(io.LimitReader(resp.Body, 1<<20), c.ExpectContains)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("body does not contain %q", c.ExpectContains)
		}
	}
	return nil
}

func contains(r io.Reader, needle string) (bool, error) {
	br := bufio.NewReaderSize(r, 32<<10)
	var window strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			window.Write(buf[:n])
			s := window.String()
			if strings.Contains(s, needle) {
				return true, nil
			}
			// Keep the tail so a match split across reads is still found.
			if keep := len(needle) - 1; len(s) > keep {
				tail := s[len(s)-keep:]
				window.Reset()
				window.WriteString(tail)
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func (r *Runner) tcp(ctx context.Context, c config.Check) error {
	conn, err := r.Dialer.DialContext(ctx, "tcp", c.Target)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (r *Runner) dns(ctx context.Context, c config.Check) error {
	addrs, err := r.Resolver.LookupHost(ctx, c.Target)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return errors.New("no addresses")
	}
	return nil
}

func (r *Runner) tls(ctx context.Context, c config.Check, now time.Time) error {
	host, _, err := net.SplitHostPort(c.Target)
	if err != nil {
		return err
	}
	d := tls.Dialer{NetDialer: r.Dialer, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: r.RootCAs}}
	conn, err := d.DialContext(ctx, "tcp", c.Target)
	if err != nil {
		return err
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	// The chain expires when its first certificate does: a leaf good for a
	// year behind an intermediate that lapses next week fails next week.
	expires := state.PeerCertificates[0].NotAfter
	if len(state.VerifiedChains) > 0 {
		for _, cert := range state.VerifiedChains[0] {
			if cert.NotAfter.Before(expires) {
				expires = cert.NotAfter
			}
		}
	}
	left := expires.Sub(now)
	if left < c.MinValidity {
		return fmt.Errorf("certificate expires in %s", humanDays(left))
	}
	return nil
}

func humanDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days <= 0 {
		return "less than a day"
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// describe turns a Go error into the short reason shown on the status page.
func describe(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "no such host"
		}
		return "dns error"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		if strings.Contains(opErr.Err.Error(), "refused") {
			return "connection refused"
		}
		return "cannot connect"
	}
	// url.Error prefixes the method and the full URL, query string included.
	// The URL may be a redirect target chosen by the site being probed, so
	// only the underlying error is kept.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
