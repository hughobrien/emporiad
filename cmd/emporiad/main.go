// Command emporiad reads Emporia's per-minute history for Vue energy monitors
// and pushes it with remote write to each --push target, with a weather
// station's outdoor temperature if --ambient-mac is given; /metrics serves
// the newest minute and the exporter's health.
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
	"strings"
	"syscall"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
	"github.com/hughobrien/emporiad/internal/exporter"
	"github.com/hughobrien/emporiad/internal/promwrite"
	"github.com/hughobrien/emporiad/internal/weather"
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
		poll      = flag.Duration("poll-interval", 15*time.Minute, "how often to fetch the minutes since the last poll (one Emporia call per channel each, plus two per channel for a week of hourly history until it is all in)")
		job       = flag.String("job", "emporiad", "job label on pushed series")
		station   = flag.String("ambient-mac", "", "MAC address of a weather station shared on ambientweather.net, pushed as weather_temperature_celsius{source=\"ambient\"} every five minutes; else no station")
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
	ambient, err := parseStation(*station)
	if err != nil {
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
	cfg := exporter.Config{PollInterval: *poll, Job: *job}
	if ambient != nil {
		cfg.Station = ambient
	}
	e := exporter.New(cfg, api, sinks, log)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		e.Run(ctx)
	}()

	srv := &http.Server{Addr: *listen, Handler: e.Handler(version), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", *listen)
	err = srv.ListenAndServe()
	cancel()
	<-runDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// parseStation reads --ambient-mac. The error leaves the address out: it
// names the station.
func parseStation(mac string) (*weather.Ambient, error) {
	if mac == "" {
		return nil, nil
	}
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return nil, errors.New("--ambient-mac: want six bytes, as 00:11:22:AA:BB:CC")
	}
	return &weather.Ambient{MAC: strings.ToUpper(hw.String())}, nil
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
