// Package exporter polls Emporia in the background and serves the latest
// readings as Prometheus metrics.
package exporter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
)

// API is the part of emporia.Client the exporter uses.
type API interface {
	Devices(ctx context.Context) ([]emporia.Device, error)
	Usage(ctx context.Context, gids []int, instant time.Time, scale string) ([]emporia.Usage, error)
}

// Config sets the polling cadence.
type Config struct {
	PollInterval   time.Duration // usage, one call for all devices
	DevicesRefresh time.Duration // device list: names, price, connected
	Lag            time.Duration // read the minute ending this long ago
	Stale          time.Duration // drop a channel's power after this long without data
}

type reading struct {
	name  string
	watts float64
	at    time.Time // end of the minute it averages
}

// Exporter holds the device table and the latest reading per channel.
type Exporter struct {
	cfg Config
	api API
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	devices  []emporia.Device
	readings map[key]reading
	lastOK   time.Time
	errors   map[string]int // by stage: devices, usage
}

type key struct {
	gid int
	num string
}

// New returns an exporter; call Run to start polling.
func New(cfg Config, api API, log *slog.Logger) *Exporter {
	return &Exporter{cfg: cfg, api: api, log: log, now: time.Now,
		readings: map[key]reading{}, errors: map[string]int{}}
}

// Run polls until ctx ends.
func (e *Exporter) Run(ctx context.Context) {
	poll := time.NewTicker(e.cfg.PollInterval)
	defer poll.Stop()
	var lastDevices time.Time
	for {
		if e.now().Sub(lastDevices) >= e.cfg.DevicesRefresh || !e.haveDevices() {
			if e.refreshDevices(ctx) {
				lastDevices = e.now()
			}
		}
		e.pollUsage(ctx)
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

func (e *Exporter) haveDevices() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.devices) > 0
}

func (e *Exporter) fail(stage string, err error) {
	e.mu.Lock()
	e.errors[stage]++
	e.mu.Unlock()
	e.log.Warn("poll failed", "stage", stage, "err", err)
}

func (e *Exporter) refreshDevices(ctx context.Context) bool {
	ds, err := e.api.Devices(ctx)
	if err != nil {
		e.fail("devices", err)
		return false
	}
	e.mu.Lock()
	first := len(e.devices) == 0
	e.devices = ds
	e.mu.Unlock()
	if first {
		e.log.Info("devices", "count", len(ds))
	}
	return true
}

func (e *Exporter) pollUsage(ctx context.Context) {
	e.mu.Lock()
	gids := make([]int, 0, len(e.devices))
	for _, d := range e.devices {
		gids = append(gids, d.Gid)
	}
	e.mu.Unlock()
	if len(gids) == 0 {
		return
	}
	at := e.now().Add(-e.cfg.Lag).Truncate(time.Minute)
	us, err := e.api.Usage(ctx, gids, at, "1MIN")
	if err != nil {
		e.fail("usage", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, u := range us {
		if u.KWh == nil {
			continue // not filled in yet; keep the previous minute until Stale
		}
		// kWh over one minute -> average watts.
		e.readings[key{u.DeviceGid, u.Num}] = reading{name: u.Name, watts: *u.KWh * 60 * 1000, at: at}
	}
	e.lastOK = e.now()
}

type sample struct {
	device, model, channel, num string
	watts                       float64
}

type snapshot struct {
	devices []emporia.Device
	power   []sample
	lastOK  time.Time
	errors  map[string]int
}

func (e *Exporter) snapshot() snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	byGid := map[int]emporia.Device{}
	for _, d := range e.devices {
		byGid[d.Gid] = d
	}
	s := snapshot{devices: append([]emporia.Device(nil), e.devices...), lastOK: e.lastOK, errors: map[string]int{}}
	for k, v := range e.errors {
		s.errors[k] = v
	}
	cutoff := e.now().Add(-e.cfg.Stale)
	for k, r := range e.readings {
		if r.at.Before(cutoff) {
			continue
		}
		d := byGid[k.gid]
		name := r.name
		if name == "" {
			name = "Channel " + k.num
		}
		s.power = append(s.power, sample{device: d.Name, model: d.Model, channel: name, num: k.num, watts: r.watts})
	}
	return s
}
