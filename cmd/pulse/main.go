// Command pulse is an uptime monitor with a status page, in one binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Brunoskyy/pulse/internal/check"
	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
	"github.com/Brunoskyy/pulse/internal/demo"
	"github.com/Brunoskyy/pulse/internal/monitor"
	"github.com/Brunoskyy/pulse/internal/notify"
	"github.com/Brunoskyy/pulse/internal/store"
	"github.com/Brunoskyy/pulse/internal/web"
)

var version = "dev"

const usage = `pulse: uptime checks and a status page, in one binary.

Usage:
  pulse run   [-config pulse.yaml]   monitor the checks in the config and serve the status page
  pulse check [-config pulse.yaml]   validate the config and exit
  pulse demo  [-listen :8080]        start fake services with 90 days of history and monitor them
  pulse version
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(log, os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "demo":
		err = cmdDemo(log, os.Args[2:])
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := fs.String("config", "pulse.yaml", "path to the config file (ignored when PULSE_CONFIG is set)")
	_ = fs.Parse(args)
	c, err := loadConfig(*path)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d checks, %d notifiers, listening on %s\n", *path, len(c.Checks), len(c.Notifiers), c.Listen)
	return nil
}

func cmdRun(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "pulse.yaml", "path to the config file (ignored when PULSE_CONFIG is set)")
	_ = fs.Parse(args)
	c, err := loadConfig(*path)
	if err != nil {
		return err
	}
	return serve(log, c, nil)
}

func cmdDemo(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "address for the status page")
	db := fs.String("database", "", "database file (default: a fresh temporary one)")
	_ = fs.Parse(args)
	fleet, err := demo.Start(demo.DefaultServices())
	if err != nil {
		return err
	}
	defer fleet.Close()
	path := *db
	if path == "" {
		dir, err := os.MkdirTemp("", "pulse-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "pulse.db")
	}
	c := fleet.Config(*listen, path)
	seed := func(st *store.Store) error {
		return demo.Backfill(context.Background(), st, fleet.Services, time.Now())
	}
	log.Info("demo services running; take one down with:", "cmd", "curl -X POST "+fleet.Control+"/down/search")
	return serve(log, c, seed)
}

func serve(log *slog.Logger, c *config.Config, seed func(*store.Store) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(c.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	if seed != nil {
		if err := seed(st); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	var senders []notify.Sender
	for _, n := range c.Notifiers {
		switch n.Kind {
		case "webhook":
			senders = append(senders, &notify.Webhook{URL: n.URL, Secret: []byte(n.Secret)})
		case "slack":
			senders = append(senders, &notify.Slack{URL: n.URL})
		}
	}
	notifyCtx, cancelNotify := context.WithCancel(context.Background())
	defer cancelNotify()
	dispatcher := notify.NewDispatcher(log, senders...)
	dispatcher.Start(notifyCtx)

	m := &monitor.Monitor{
		Config: c, Store: st, Prober: check.NewRunner(), Notifier: dispatcher,
		Clock: clock.Real{}, Log: log, StatusURL: publicURL(c.Listen),
	}
	if err := m.Load(ctx); err != nil {
		dispatcher.Close()
		return err
	}
	m.Start(ctx)
	pruneDone := make(chan struct{})
	go func() { defer close(pruneDone); m.Prune(ctx) }()

	srv := &http.Server{
		Addr:              c.Listen,
		Handler:           (&web.Server{Config: c, Store: st, Monitor: m, Clock: clock.Real{}, Log: log, CacheFor: 5 * time.Second}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		stop()
		m.Stop()
		cancelNotify()
		dispatcher.Close()
		return err
	}
	log.Info("pulse is up", "status_page", "http://"+ln.Addr().String(), "checks", len(c.Checks), "version", version)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
		}
		stop()
	}

	// Shutdown order: stop accepting page requests, stop scheduling, let
	// running probes finish and be stored, then flush notifications.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	m.Stop()
	<-pruneDone
	done := make(chan struct{})
	go func() { dispatcher.Close(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		cancelNotify()
		<-done
	}
	cancelNotify()
	return nil
}

// loadConfig reads the YAML from PULSE_CONFIG when it is set, which is how a
// container gets its config from a secret store without a file on disk, and
// from the file otherwise.
func loadConfig(path string) (*config.Config, error) {
	if raw := os.Getenv("PULSE_CONFIG"); raw != "" {
		return config.Parse([]byte(raw))
	}
	return config.Load(path)
}

func publicURL(listen string) string {
	if v := os.Getenv("PULSE_PUBLIC_URL"); v != "" {
		return v
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}
