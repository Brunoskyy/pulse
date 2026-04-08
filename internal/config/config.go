// Package config reads pulse.yaml.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Kind string

const (
	KindHTTP Kind = "http"
	KindTCP  Kind = "tcp"
	KindDNS  Kind = "dns"
	KindTLS  Kind = "tls"
)

// Check is one thing to watch.
type Check struct {
	ID       string        `yaml:"id"`
	Name     string        `yaml:"name"`
	Group    string        `yaml:"group"`
	Kind     Kind          `yaml:"kind"`
	Target   string        `yaml:"target"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// HTTP only.
	Method         string `yaml:"method"`
	ExpectStatus   []int  `yaml:"expect_status"`
	ExpectContains string `yaml:"expect_contains"`
	// TLS only: fail when the certificate expires sooner than this.
	MinValidity time.Duration `yaml:"min_validity"`
	// Consecutive failures before an incident opens, and successes before it closes.
	FailAfter    int `yaml:"fail_after"`
	RecoverAfter int `yaml:"recover_after"`
}

type Notifier struct {
	Kind   string `yaml:"kind"` // "webhook" or "slack"
	URL    string `yaml:"url"`
	Secret string `yaml:"secret"` // webhook only; may be "${ENV_VAR}"
}

type Config struct {
	Title     string        `yaml:"title"`
	Listen    string        `yaml:"listen"`
	Database  string        `yaml:"database"`
	Retention time.Duration `yaml:"retention"`
	Workers   int           `yaml:"workers"`
	Checks    []Check       `yaml:"checks"`
	Notifiers []Notifier    `yaml:"notifiers"`
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse validates and fills defaults. Every problem is reported at once, so a
// typo in check 12 does not hide a typo in check 30.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(os.ExpandEnv(string(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("pulse.yaml: %w", err)
	}
	if c.Title == "" {
		c.Title = "Status"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Database == "" {
		c.Database = "pulse.db"
	}
	if c.Retention == 0 {
		c.Retention = 90 * 24 * time.Hour
	}
	if c.Workers <= 0 {
		c.Workers = 16
	}

	var errs []error
	seen := map[string]bool{}
	for i := range c.Checks {
		ch := &c.Checks[i]
		where := fmt.Sprintf("checks[%d]", i)
		if ch.ID == "" {
			ch.ID = slug(ch.Name)
		}
		if !idRE.MatchString(ch.ID) {
			errs = append(errs, fmt.Errorf("%s: id %q must be lowercase letters, digits and dashes", where, ch.ID))
		}
		if seen[ch.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", where, ch.ID))
		}
		seen[ch.ID] = true
		if ch.Name == "" {
			ch.Name = ch.ID
		}
		if ch.Interval == 0 {
			ch.Interval = 30 * time.Second
		}
		if ch.Interval < time.Second {
			errs = append(errs, fmt.Errorf("%s: interval must be at least 1s", where))
		}
		if ch.Timeout == 0 {
			ch.Timeout = 10 * time.Second
		}
		if ch.Timeout > ch.Interval {
			ch.Timeout = ch.Interval
		}
		if ch.FailAfter <= 0 {
			ch.FailAfter = 3
		}
		if ch.RecoverAfter <= 0 {
			ch.RecoverAfter = 2
		}
		switch ch.Kind {
		case KindHTTP:
			u, err := url.Parse(ch.Target)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("%s: http target must be an http(s) URL", where))
			}
			if ch.Method == "" {
				ch.Method = "GET"
			}
		case KindTCP, KindTLS:
			if !strings.Contains(ch.Target, ":") {
				errs = append(errs, fmt.Errorf("%s: %s target must be host:port", where, ch.Kind))
			}
			if ch.Kind == KindTLS && ch.MinValidity == 0 {
				ch.MinValidity = 14 * 24 * time.Hour
			}
		case KindDNS:
			if ch.Target == "" {
				errs = append(errs, fmt.Errorf("%s: dns target must be a hostname", where))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: kind must be http, tcp, dns or tls", where))
		}
	}
	for i, n := range c.Notifiers {
		if n.Kind != "webhook" && n.Kind != "slack" {
			errs = append(errs, fmt.Errorf("notifiers[%d]: kind must be webhook or slack", i))
		}
		if _, err := url.ParseRequestURI(n.URL); err != nil {
			errs = append(errs, fmt.Errorf("notifiers[%d]: url is not valid", i))
		}
	}
	if len(c.Checks) == 0 {
		errs = append(errs, errors.New("no checks configured"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &c, nil
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
