// Command emporiad reads Emporia's per-minute history for Vue energy monitors
// and pushes it with remote write to each --push target; /metrics serves the
// newest minute and the exporter's health.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
	"github.com/hughobrien/emporiad/internal/exporter"
	"github.com/hughobrien/emporiad/internal/promwrite"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "emporiad:", err)
		os.Exit(1)
	}
}

func run() error {
	var targets promwrite.Targets
	flag.Var(&targets, "push", "name=url of a remote write receiver (VictoriaMetrics, or Prometheus with its receiver on) to push every minute to and to ask where the last run stopped; repeat for more. Its own scrape of /metrics should drop pushed_<name>=\"true\"")
	var (
		listen    = flag.String("listen", "127.0.0.1:9882", "HTTP listen address")
		credsPath = flag.String("credentials", "", "file with the Emporia account email (line 1) and password (line 2)")
		poll      = flag.Duration("poll-interval", 15*time.Minute, "how often to fetch the minutes since the last poll (17 Emporia calls each, plus 34 for a week of hourly history until it is all in)")
		job       = flag.String("job", "emporiad", "job label on pushed series")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}
	if *credsPath == "" {
		return errors.New("--credentials is required")
	}
	if err := checkPollInterval(*poll); err != nil {
		return err
	}
	user, pass, err := readCreds(*credsPath)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	api := &emporia.Client{Tokens: emporia.NewSession(user, pass)}
	var sinks []exporter.Target
	for _, t := range targets {
		sinks = append(sinks, exporter.Target{Name: t.Name, Store: &promwrite.Client{Base: t.URL}})
	}
	e := exporter.New(exporter.Config{PollInterval: *poll, Job: *job}, api, sinks, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go e.Run(ctx)

	srv := &http.Server{Addr: *listen, Handler: e.Handler(version), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// checkPollInterval rejects what time.NewTicker would panic on (or, for zero,
// quietly replace with the default).
func checkPollInterval(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("--poll-interval must be positive, got %v", d)
	}
	return nil
}

func readCreds(path string) (string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" || lines[1] == "" {
		return "", "", fmt.Errorf("%s: want the email on line 1 and the password on line 2", path)
	}
	return strings.TrimSpace(lines[0]), strings.TrimRight(lines[1], "\r"), nil
}
