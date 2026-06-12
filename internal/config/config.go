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
	// MaxGap is the longest stretch one probe can stand for in uptime maths.
	// Beyond it the monitor was not looking, which is no data. Defaults to
	// twice the interval; raise it if the interval was once longer and that
	// history should keep its weight.
	MaxGap time.Duration `yaml:"max_gap"`
	// Consecutive failures before an incident opens, and successes before it closes.
	FailAfter    int `yaml:"fail_after"`
	RecoverAfter int `yaml:"recover_after"`
	// Flap opens an incident when Failures of the last Window probes failed,
	// even if they never ran FailAfter in a row. A service failing two probes
	// in three is down for its users. Failures 0 turns it off.
	Flap Flap `yaml:"flap"`
}

type Flap struct {
	Window   int `yaml:"window"`
	Failures int `yaml:"failures"`
}

type Notifier struct {
	Kind   string `yaml:"kind"` // "webhook" or "slack"
	URL    string `yaml:"url"`
	Secret string `yaml:"secret"` // webhook only; may be "${ENV_VAR}"
	// URL may also be "${ENV_VAR}". Only these two fields are expanded, only
	// the ${NAME} form, and a reference to an unset variable is an error: a
	// missing secret would otherwise send every webhook unsigned.
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
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
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
		if ch.MaxGap == 0 {
			ch.MaxGap = 2 * ch.Interval
		}
		if ch.MaxGap < ch.Interval {
			errs = append(errs, fmt.Errorf("%s: max_gap cannot be shorter than the interval", where))
		}
		// The next tick can come as early as 0.9 of the interval (jitter), so
		// a probe that uses the full interval would make every other tick be
		// skipped. 0.8 leaves room.
		if limit := ch.Interval * 8 / 10; ch.Timeout > limit {
			ch.Timeout = limit
		}
		if ch.FailAfter <= 0 {
			ch.FailAfter = 3
		}
		if ch.RecoverAfter <= 0 {
			ch.RecoverAfter = 2
		}
		if ch.Flap.Window == 0 && ch.Flap.Failures == 0 {
			ch.Flap = Flap{Window: 10, Failures: 6}
		}
		if ch.Flap.Failures < 0 || ch.Flap.Window < 0 || ch.Flap.Failures > ch.Flap.Window {
			errs = append(errs, fmt.Errorf("%s: flap.failures must be between 0 and flap.window", where))
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
	for i := range c.Notifiers {
		n := &c.Notifiers[i]
		var err error
		if n.URL, err = expand(n.URL); err != nil {
			errs = append(errs, fmt.Errorf("notifiers[%d].url: %w", i, err))
		}
		if n.Secret, err = expand(n.Secret); err != nil {
			errs = append(errs, fmt.Errorf("notifiers[%d].secret: %w", i, err))
		}
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

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand replaces ${NAME} with the environment variable, and fails when one
// is unset or empty. A bare $ is left as written.
func expand(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return out, nil
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
