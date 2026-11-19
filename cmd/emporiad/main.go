// Command emporiad reads Emporia's per-minute history for Vue energy monitors
// and pushes it to Prometheus with remote write; health stays on /metrics.
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
	var (
		listen    = flag.String("listen", "127.0.0.1:9882", "HTTP listen address")
		credsPath = flag.String("credentials", "", "file with the Emporia account email (line 1) and password (line 2)")
		poll      = flag.Duration("poll-interval", 15*time.Minute, "how often to fetch the minutes since the last poll (17 Emporia calls each)")
		promURL   = flag.String("prometheus-url", "http://127.0.0.1:9090", "Prometheus to push to (remote write receiver enabled) and to ask where the last run stopped")
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
	user, pass, err := readCreds(*credsPath)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	api := &emporia.Client{Tokens: emporia.NewSession(user, pass)}
	store := &promwrite.Client{Base: *promURL}
	e := exporter.New(exporter.Config{PollInterval: *poll, Job: *job}, api, store, log)

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
