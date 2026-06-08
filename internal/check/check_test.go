package check

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Brunoskyy/pulse/internal/config"
)

func httpCheck(target string) config.Check {
	return config.Check{ID: "c", Kind: config.KindHTTP, Target: target, Method: "GET", Timeout: 2 * time.Second}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "all good")
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		case "/big":
			// The needle straddles two reads, far past the first buffer.
			w.Write([]byte(strings.Repeat("x", 32<<10-3)))
			w.(http.Flusher).Flush()
			w.Write([]byte("needle-here"))
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	r := NewRunner()

	cases := []struct {
		name   string
		mod    func(*config.Check)
		path   string
		ok     bool
		reason string
	}{
		{"2xx is up", nil, "/ok", true, ""},
		{"redirects are followed", nil, "/redirect", true, ""},
		{"5xx is down", nil, "/fail", false, "status 500"},
		{"expected status is honoured", func(c *config.Check) { c.ExpectStatus = []int{418} }, "/teapot", true, ""},
		{"body must contain", func(c *config.Check) { c.ExpectContains = "good" }, "/ok", true, ""},
		{"body missing text", func(c *config.Check) { c.ExpectContains = "bad" }, "/ok", false, `body does not contain "bad"`},
		{"match across read boundary", func(c *config.Check) { c.ExpectContains = "needle" }, "/big", true, ""},
		{"timeout", func(c *config.Check) { c.Timeout = 50 * time.Millisecond }, "/slow", false, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := httpCheck(srv.URL + tc.path)
			if tc.mod != nil {
				tc.mod(&c)
			}
			res := r.Run(context.Background(), c)
			if res.OK != tc.ok || res.Error != tc.reason {
				t.Fatalf("got ok=%v err=%q, want ok=%v err=%q", res.OK, res.Error, tc.ok, tc.reason)
			}
			if res.Latency <= 0 {
				t.Fatal("latency not measured")
			}
		})
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	r := NewRunner()
	up := r.Run(context.Background(), config.Check{ID: "t", Kind: config.KindTCP, Target: ln.Addr().String(), Timeout: time.Second})
	if !up.OK {
		t.Fatalf("listening port should be up: %s", up.Error)
	}
	addr := ln.Addr().String()
	ln.Close()
	down := r.Run(context.Background(), config.Check{ID: "t", Kind: config.KindTCP, Target: addr, Timeout: time.Second})
	if down.OK || down.Error != "connection refused" {
		t.Fatalf("closed port: ok=%v err=%q", down.OK, down.Error)
	}
}

func TestDNS(t *testing.T) {
	r := NewRunner()
	if res := r.Run(context.Background(), config.Check{ID: "d", Kind: config.KindDNS, Target: "localhost", Timeout: 2 * time.Second}); !res.OK {
		t.Fatalf("localhost should resolve: %s", res.Error)
	}
	res := r.Run(context.Background(), config.Check{ID: "d", Kind: config.KindDNS, Target: "does-not-exist.invalid", Timeout: 2 * time.Second})
	if res.OK || res.Error != "no such host" {
		t.Fatalf(".invalid must not resolve: ok=%v err=%q", res.OK, res.Error)
	}
}

func TestTLSExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	r := NewRunner()
	r.RootCAs = pool
	// httptest certificates are issued for "example.com"; dial the loopback
	// address but verify against that name.
	target := strings.TrimPrefix(srv.URL, "https://")
	_, port, _ := net.SplitHostPort(target)
	r.Dialer = &net.Dialer{Timeout: time.Second}
	r.Resolver = net.DefaultResolver

	c := config.Check{ID: "tls", Kind: config.KindTLS, Target: net.JoinHostPort("127.0.0.1", port), Timeout: 2 * time.Second, MinValidity: time.Hour}
	if res := r.Run(context.Background(), c); !res.OK {
		t.Fatalf("valid cert should pass: %s", res.Error)
	}
	left := time.Until(srv.Certificate().NotAfter)
	c.MinValidity = left + 48*time.Hour
	res := r.Run(context.Background(), c)
	if res.OK || !strings.HasPrefix(res.Error, "certificate expires in") {
		t.Fatalf("cert expiring sooner than the minimum should fail: ok=%v err=%q", res.OK, res.Error)
	}

	r.RootCAs = nil
	if res := r.Run(context.Background(), config.Check{ID: "tls", Kind: config.KindTLS, Target: c.Target, Timeout: 2 * time.Second, MinValidity: time.Hour}); res.OK {
		t.Fatal("an untrusted certificate must fail")
	}
}

func TestReasonDropsTheRedirectURL(t *testing.T) {
	// A target that redirects forever to a URL of its choosing. The reason
	// must not carry that URL, which is what reaches Slack and the page.
	hostile := "/n?x=<!channel>+<https://evil.example|Reset+your+password>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, hostile, http.StatusFound)
	}))
	defer srv.Close()
	res := NewRunner().Run(context.Background(), httpCheck(srv.URL))
	if res.OK {
		t.Fatal("an endless redirect should fail")
	}
	if strings.Contains(res.Error, "channel") || strings.Contains(res.Error, "evil") || strings.Contains(res.Error, "http") {
		t.Fatalf("reason leaks the redirect target: %q", res.Error)
	}
}

// chainServer serves leaf <- intermediate <- root, where the intermediate
// expires long before the leaf.
func chainServer(t *testing.T, intermediateLeft time.Duration) (addr string, roots *x509.CertPool) {
	t.Helper()
	now := time.Now()
	mk := func(tmpl, parent *x509.Certificate, pub any, signer any) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c
	}
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	rootKey, midKey, leafKey := key(), key(), key()
	rootT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(10 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	root := mk(rootT, rootT, &rootKey.PublicKey, rootKey)
	midT := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "mid"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(intermediateLeft), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	mid := mk(midT, root, &midKey.PublicKey, rootKey)
	leafT := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "leaf"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leaf := mk(leafT, mid, &leafKey.PublicKey, midKey)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw, mid.Raw}, PrivateKey: leafKey}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()
	roots = x509.NewCertPool()
	roots.AddCert(root)
	return ln.Addr().String(), roots
}

func TestTLSExpiryLooksAtTheWholeChain(t *testing.T) {
	addr, roots := chainServer(t, 3*24*time.Hour+time.Hour)
	r := NewRunner()
	r.RootCAs = roots
	c := config.Check{ID: "tls", Kind: config.KindTLS, Target: addr, Timeout: 2 * time.Second, MinValidity: 14 * 24 * time.Hour}
	res := r.Run(context.Background(), c)
	if res.OK || res.Error != "certificate expires in 3 days" {
		t.Fatalf("the intermediate expiring should fail the check: ok=%v err=%q", res.OK, res.Error)
	}
	c.MinValidity = 24 * time.Hour
	if res := r.Run(context.Background(), c); !res.OK {
		t.Fatalf("chain valid beyond the minimum should pass: %s", res.Error)
	}
}
