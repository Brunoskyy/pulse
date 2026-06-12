package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseFillsDefaults(t *testing.T) {
	c, err := Parse([]byte(`
checks:
  - name: Public API
    kind: http
    target: https://api.example.com/health
  - id: db
    kind: tcp
    target: db.internal:5432
    interval: 5s
    timeout: 30s
  - id: cert
    kind: tls
    target: example.com:443
`))
	if err != nil {
		t.Fatal(err)
	}
	api := c.Checks[0]
	if api.ID != "public-api" || api.Method != "GET" || api.Interval != 30*time.Second || api.Timeout != 10*time.Second {
		t.Fatalf("defaults not applied: %+v", api)
	}
	if api.MaxGap != time.Minute {
		t.Fatalf("max_gap should default to twice the interval: %s", api.MaxGap)
	}
	if api.FailAfter != 3 || api.RecoverAfter != 2 {
		t.Fatalf("thresholds: %+v", api)
	}
	if c.Checks[1].Timeout != 4*time.Second {
		t.Fatalf("timeout should be capped at 0.8 of the interval, got %s", c.Checks[1].Timeout)
	}
	if api.Flap != (Flap{Window: 10, Failures: 6}) {
		t.Fatalf("flap default: %+v", api.Flap)
	}
	if c.Checks[2].MinValidity != 14*24*time.Hour {
		t.Fatalf("tls min validity default: %s", c.Checks[2].MinValidity)
	}
	if c.Listen != ":8080" || c.Workers != 16 || c.Retention != 90*24*time.Hour {
		t.Fatalf("top-level defaults: %+v", c)
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
checks:
  - id: Bad_ID
    kind: http
    target: ftp://nope
  - id: x
    kind: carrier-pigeon
  - id: x
    kind: tcp
    target: nocolon
    interval: 100ms
notifiers:
  - kind: email
    url: "::"
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"lowercase", "http(s) URL", "kind must be", "duplicate id", "host:port", "at least 1s", "webhook or slack", "url is not valid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, err)
		}
	}
}

func TestParseRejectsUnknownFieldsAndExpandsEnv(t *testing.T) {
	if _, err := Parse([]byte("checks:\n  - id: a\n    kind: dns\n    target: example.com\n    intervall: 5s\n")); err == nil {
		t.Fatal("a misspelled field should be an error, not silently ignored")
	}
	t.Setenv("PULSE_TEST_SECRET", "s3cr3t")
	c, err := Parse([]byte("checks:\n  - id: a\n    kind: dns\n    target: example.com\nnotifiers:\n  - kind: webhook\n    url: https://hooks.example.com/x\n    secret: ${PULSE_TEST_SECRET}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Notifiers[0].Secret != "s3cr3t" {
		t.Fatalf("secret not expanded: %q", c.Notifiers[0].Secret)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Public API": "public-api", "  Checkout / EU ": "checkout-eu", "a--b": "a-b"} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvExpansionIsNarrowAndStrict(t *testing.T) {
	base := "checks:\n  - id: a\n    kind: http\n    target: https://example.com\n    expect_contains: \"$5.00 ${NOT_A_FIELD_WE_EXPAND}\"\nnotifiers:\n  - kind: webhook\n    url: ${PULSE_TEST_URL}\n    secret: ${PULSE_TEST_SECRET}\n"
	t.Setenv("PULSE_TEST_URL", "https://hooks.example.com/x")
	t.Setenv("PULSE_TEST_SECRET", "")
	if _, err := Parse([]byte(base)); err == nil || !strings.Contains(err.Error(), "PULSE_TEST_SECRET is not set") {
		t.Fatalf("an empty secret variable must be an error: %v", err)
	}
	t.Setenv("PULSE_TEST_SECRET", "s3cr3t")
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Checks[0].ExpectContains; got != "$5.00 ${NOT_A_FIELD_WE_EXPAND}" {
		t.Fatalf("only url and secret are expanded, and a bare $ is kept: %q", got)
	}
	if c.Notifiers[0].URL != "https://hooks.example.com/x" || c.Notifiers[0].Secret != "s3cr3t" {
		t.Fatalf("notifier not expanded: %+v", c.Notifiers[0])
	}
}

func TestFlapMustFitItsWindow(t *testing.T) {
	_, err := Parse([]byte("checks:\n  - id: a\n    kind: dns\n    target: example.com\n    flap: {window: 3, failures: 5}\n"))
	if err == nil || !strings.Contains(err.Error(), "flap.failures") {
		t.Fatalf("expected a flap error, got %v", err)
	}
	c, err := Parse([]byte("checks:\n  - id: a\n    kind: dns\n    target: example.com\n    flap: {window: 5, failures: 0}\n"))
	if err != nil || c.Checks[0].Flap.Failures != 0 {
		t.Fatalf("failures 0 turns flap detection off: %v %+v", err, c)
	}
}
