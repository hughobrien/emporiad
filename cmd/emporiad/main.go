// Command emporiad reads Emporia's per-minute history for Vue energy monitors
// and pushes it with remote write to each --push target, with outdoor
// temperature if --weather-lat and --weather-lon are given; /metrics serves
// the newest minute and the exporter's health.
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
	"strconv"
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
		lat       = flag.String("weather-lat", "", "latitude for outdoor temperature from Open-Meteo, pushed as weather_temperature_celsius; with --weather-lon, else no weather")
		lon       = flag.String("weather-lon", "", "longitude for outdoor temperature (see --weather-lat)")
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
	place, err := parsePlace(*lat, *lon)
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
	if place != nil {
		cfg.Weather = place
	}
	e := exporter.New(cfg, api, sinks, log)

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

// parsePlace reads --weather-lat and --weather-lon: both or neither.
func parsePlace(lat, lon string) (*weather.Client, error) {
	if lat == "" && lon == "" {
		return nil, nil
	}
	if lat == "" || lon == "" {
		return nil, errors.New("--weather-lat and --weather-lon go together")
	}
	la, err := strconv.ParseFloat(lat, 64)
	if err != nil || la < -90 || la > 90 {
		return nil, fmt.Errorf("--weather-lat %q: want degrees, -90 to 90", lat)
	}
	lo, err := strconv.ParseFloat(lon, 64)
	if err != nil || lo < -180 || lo > 180 {
		return nil, fmt.Errorf("--weather-lon %q: want degrees, -180 to 180", lon)
	}
	return &weather.Client{Latitude: la, Longitude: lo}, nil
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
