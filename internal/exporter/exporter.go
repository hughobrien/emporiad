// Package exporter reads Emporia's per-minute history and pushes it to
// Prometheus with remote write, one sample per minute at the minute's own
// time. Health and device facts stay on /metrics for Prometheus to scrape.
//
// Why push: Emporia keeps every minute, but any reading taken on a timer and
// served at scrape time loses whatever falls between the two timers. Pushing
// past minutes with their timestamps keeps all of them, at any poll rate.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
	"github.com/hughobrien/emporiad/internal/promwrite"
)

// API is the part of emporia.Client the exporter uses.
type API interface {
	Devices(ctx context.Context) ([]emporia.Device, error)
	Chart(ctx context.Context, gid int, channel string, start, end time.Time, unit string) ([]*float64, error)
	Usage(ctx context.Context, gids []int, instant time.Time, scale, unit string) ([]emporia.Usage, error)
}

// Store is where samples go, and where the exporter learns what it already
// sent.
type Store interface {
	Write(ctx context.Context, series []promwrite.Series) error
	Newest(ctx context.Context, selector string, lookback time.Duration) (time.Time, bool, error)
}

// Pushed lists the metric families sent by remote write (not on /metrics).
var Pushed = []string{"emporia_power_watts", "emporia_hourly_energy_kwh", "emporia_hourly_cost"}

// Config sets the cadence. Zero values take the defaults in New.
type Config struct {
	PollInterval time.Duration // 15m: how often to fetch the minutes since the last poll
	Job          string        // the job label on pushed series
	Settle       time.Duration // 2m: only take minutes that ended this long ago
	NullWait     time.Duration // 30m: how long to wait for a minute Emporia has not filled
	Window       time.Duration // 50m: the oldest sample Prometheus takes without an out-of-order window
	HourSettle   time.Duration // 5m: wait this long after an hour ends before asking for its cost
	CallGap      time.Duration // 2s between Emporia calls, to stay gentle
}

// Exporter polls Emporia and pushes what it finds.
type Exporter struct {
	cfg   Config
	api   API
	store Store
	log   *slog.Logger
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// Owned by the poll loop.
	resumed      bool
	minute, hour time.Time // the next minute and hour to fetch

	mu      sync.Mutex
	devices []emporia.Device
	lastOK  time.Time
	newest  time.Time // end of the newest minute pushed
	errors  map[string]int
}

// Stages counted in emporia_poll_errors_total.
var stages = []string{"devices", "resume", "usage", "cost", "push"}

// New returns an exporter; call Run to start polling.
func New(cfg Config, api API, store Store, log *slog.Logger) *Exporter {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&cfg.PollInterval, 15*time.Minute)
	def(&cfg.Settle, 2*time.Minute)
	def(&cfg.NullWait, 30*time.Minute)
	def(&cfg.Window, 50*time.Minute)
	def(&cfg.HourSettle, 5*time.Minute)
	def(&cfg.CallGap, 2*time.Second)
	if cfg.Job == "" {
		cfg.Job = "emporiad"
	}
	return &Exporter{cfg: cfg, api: api, store: store, log: log, now: time.Now, sleep: sleepCtx, errors: map[string]int{}}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run polls until ctx ends.
func (e *Exporter) Run(ctx context.Context) {
	tick := time.NewTicker(e.cfg.PollInterval)
	defer tick.Stop()
	for {
		e.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Exporter) fail(stage string, err error) {
	e.mu.Lock()
	e.errors[stage]++
	e.mu.Unlock()
	e.log.Warn("poll failed", "stage", stage, "err", err)
}

func (e *Exporter) poll(ctx context.Context) {
	ds, err := e.api.Devices(ctx)
	e.mu.Lock()
	if err == nil {
		e.devices = ds
	}
	ds = e.devices
	e.mu.Unlock()
	if err != nil {
		e.fail("devices", err)
	}
	if len(ds) == 0 {
		return
	}
	if !e.resumed && !e.resume(ctx) {
		return
	}
	trees := buildTrees(ds)
	ok := e.pollMinutes(ctx, trees)
	ok = ok && e.pollHours(ctx, trees)
	if ok && err == nil {
		e.mu.Lock()
		e.lastOK = e.now()
		e.mu.Unlock()
	}
}

// resume finds where the last run stopped, from what Prometheus holds, so a
// restart neither leaves a gap it could fill nor resends older samples (which
// Prometheus refuses, along with the rest of the request). With nothing there
// it starts at the current minute and hour.
func (e *Exporter) resume(ctx context.Context) bool {
	const lookback = 8 * 24 * time.Hour // Emporia keeps about a week of minutes
	sel := func(name string) string { return fmt.Sprintf(`%s{job=%q,channel_num="1,2,3"}`, name, e.cfg.Job) }
	m, okM, err := e.store.Newest(ctx, sel("emporia_power_watts"), lookback)
	if err != nil {
		e.fail("resume", err)
		return false
	}
	h, okH, err := e.store.Newest(ctx, sel("emporia_hourly_cost"), lookback)
	if err != nil {
		e.fail("resume", err)
		return false
	}
	now := e.now().UTC()
	e.minute, e.hour = now.Truncate(time.Minute), now.Truncate(time.Hour)
	if okM {
		e.minute = m.Truncate(time.Minute) // samples sit at the end of their minute
		e.mu.Lock()
		e.newest = m
		e.mu.Unlock()
	}
	if okH {
		e.hour = h.Truncate(time.Hour)
	}
	e.resumed = true
	e.log.Info("resuming", "minute", e.minute, "hour", e.hour, "found_minute", okM, "found_hour", okH)
	return true
}

// channel is one measured channel and the labels it is pushed with.
type channel struct {
	gid           int
	num, name     string
	device, model string
}

// tree is a top-level device and everything nested under it: a Vue with its
// mains, plus the circuit expansion's channels.
type tree struct {
	gids     []int
	mains    *channel // channel 1,2,3 of the top-level device
	circuits []channel
	balance  channel // labels for Balance: mains minus every circuit
}

func buildTrees(ds []emporia.Device) []tree {
	byGid := map[int]emporia.Device{}
	for _, d := range ds {
		byGid[d.Gid] = d
	}
	// A circuit expansion has the same deviceGid as its Vue (it is one
	// monitor), so the walk up stops at a repeat, not only at the top.
	root := func(d emporia.Device) emporia.Device {
		seen := map[int]bool{}
		for d.Parent != 0 && !seen[d.Parent] {
			seen[d.Gid] = true
			p, ok := byGid[d.Parent]
			if !ok || p.Gid == d.Gid {
				break
			}
			d = p
		}
		return d
	}
	idx := map[int]int{}
	var out []tree
	for _, d := range ds { // Devices lists a parent before its children
		top := root(d)
		r := top.Gid
		i, ok := idx[r]
		if !ok {
			i, idx[r] = len(out), len(out)
			out = append(out, tree{})
		}
		t := &out[i]
		if !slices.Contains(t.gids, d.Gid) {
			t.gids = append(t.gids, d.Gid)
		}
		isTop := d.Parent == 0
		for _, c := range d.Channels {
			name := c.Name
			if name == "" {
				name = "Channel " + c.Num
			}
			ch := channel{gid: d.Gid, num: c.Num, name: name, device: d.Name, model: d.Model}
			if isTop && c.Num == "1,2,3" {
				if name == "Channel 1,2,3" {
					ch.name = "Main"
				}
				t.mains = &ch
				t.balance = channel{gid: d.Gid, num: "Balance", name: "Balance", device: d.Name, model: d.Model}
			} else {
				t.circuits = append(t.circuits, ch)
			}
		}
	}
	keep := out[:0]
	for _, t := range out {
		if t.mains != nil {
			keep = append(keep, t)
		}
	}
	return keep
}

func (e *Exporter) labels(name string, c channel) map[string]string {
	return map[string]string{"__name__": name, "job": e.cfg.Job,
		"device": c.device, "model": c.model, "channel": c.name, "channel_num": c.num}
}

// pollMinutes fetches every minute from e.minute up to the newest settled
// one and pushes them. It reports whether nothing failed.
func (e *Exporter) pollMinutes(ctx context.Context, trees []tree) bool {
	now := e.now().UTC()
	if oldest := now.Add(-e.cfg.Window).Truncate(time.Minute).Add(time.Minute); e.minute.Before(oldest) {
		e.log.Warn("skipping minutes Prometheus would refuse as too old",
			"from", e.minute, "to", oldest, "minutes", int(oldest.Sub(e.minute)/time.Minute))
		e.minute = oldest
	}
	end := now.Add(-e.cfg.Settle).Truncate(time.Minute)
	n := int(end.Sub(e.minute) / time.Minute)
	if n <= 0 {
		return true
	}

	type fetched struct {
		c    channel
		vals []*float64
	}
	var all []fetched
	mains := make([][]*float64, len(trees))
	circuits := make([][][]*float64, len(trees))
	for ti, t := range trees {
		for ci, c := range append([]channel{*t.mains}, t.circuits...) {
			if len(all) > 0 {
				if e.sleep(ctx, e.cfg.CallGap) != nil {
					return false
				}
			}
			vals, err := e.api.Chart(ctx, c.gid, c.num, e.minute, end, emporia.KWh)
			if err != nil {
				e.fail("usage", fmt.Errorf("channel %s: %w", c.num, err))
				return false
			}
			all = append(all, fetched{c, vals})
			if ci == 0 {
				mains[ti] = vals
			} else {
				circuits[ti] = append(circuits[ti], vals)
			}
		}
	}
	at := func(vs []*float64, i int) *float64 {
		if i < len(vs) {
			return vs[i]
		}
		return nil
	}

	// A minute is ready once every mains has it. Emporia fills a minute for
	// all channels together, so a circuit still empty then has no data. A
	// minute whose mains stays empty past NullWait is skipped.
	ts := func(i int) time.Time { return e.minute.Add(time.Duration(i+1) * time.Minute) }
	ready := 0
	var newest time.Time
	for i := range n {
		filled := true
		for ti := range trees {
			filled = filled && at(mains[ti], i) != nil
		}
		if !filled && now.Sub(ts(i)) < e.cfg.NullWait {
			break
		}
		if filled {
			newest = ts(i)
		}
		ready = i + 1
	}
	if ready == 0 {
		return true
	}
	watts := func(kwh float64) float64 { return kwh * 60 * 1000 }

	var series []promwrite.Series
	for _, f := range all {
		s := promwrite.Series{Labels: e.labels("emporia_power_watts", f.c)}
		for i := range ready {
			if v := at(f.vals, i); v != nil {
				s.Samples = append(s.Samples, promwrite.Sample{T: ts(i), V: watts(*v)})
			}
		}
		if len(s.Samples) > 0 {
			series = append(series, s)
		}
	}
	for ti, t := range trees {
		s := promwrite.Series{Labels: e.labels("emporia_power_watts", t.balance)}
	minutes:
		for i := range ready {
			m := at(mains[ti], i)
			if m == nil {
				continue
			}
			b := *m
			for _, c := range circuits[ti] {
				v := at(c, i)
				if v == nil {
					continue minutes
				}
				b -= *v
			}
			s.Samples = append(s.Samples, promwrite.Sample{T: ts(i), V: watts(b)})
		}
		if len(s.Samples) > 0 {
			series = append(series, s)
		}
	}
	if !e.write(ctx, series) {
		return false
	}
	e.minute = e.minute.Add(time.Duration(ready) * time.Minute)
	if !newest.IsZero() {
		e.mu.Lock()
		e.newest = newest
		e.mu.Unlock()
	}
	return true
}

// write pushes series. A write Prometheus refuses outright is logged and
// dropped: the same request would be refused again, and stopping would stall
// every later minute behind it.
func (e *Exporter) write(ctx context.Context, series []promwrite.Series) bool {
	if len(series) == 0 {
		return true
	}
	err := e.store.Write(ctx, series)
	if err == nil {
		return true
	}
	e.fail("push", err)
	var refused *promwrite.RefusedError
	return errors.As(err, &refused)
}

// pollHours pushes Emporia's energy and plan-priced cost for each finished
// hour whose minutes have all been fetched.
func (e *Exporter) pollHours(ctx context.Context, trees []tree) bool {
	for range 6 { // at most six hours a poll; the rest next time
		now := e.now().UTC()
		end := e.hour.Add(time.Hour)
		if end.After(e.minute) || now.Sub(end) < e.cfg.HourSettle {
			return true
		}
		var series []promwrite.Series
		for _, t := range trees {
			byNum := map[string]channel{t.mains.num: *t.mains, "Balance": t.balance}
			for _, c := range t.circuits {
				byNum[c.num] = c
			}
			for _, u := range []struct{ unit, name string }{
				{emporia.KWh, "emporia_hourly_energy_kwh"},
				{emporia.Dollars, "emporia_hourly_cost"},
			} {
				if e.sleep(ctx, e.cfg.CallGap) != nil {
					return false
				}
				us, err := e.api.Usage(ctx, t.gids, e.hour, "1H", u.unit)
				if err != nil {
					e.fail("cost", err)
					return false
				}
				inTree := map[int]bool{}
				for _, g := range t.gids {
					inTree[g] = true
				}
				seen := map[string]bool{}
				mainsFilled := false
				for _, x := range us {
					c, ok := byNum[x.Num]
					if !ok || !inTree[x.DeviceGid] || seen[x.Num] || x.Value == nil {
						continue
					}
					seen[x.Num] = true
					mainsFilled = mainsFilled || x.Num == t.mains.num
					series = append(series, promwrite.Series{Labels: e.labels(u.name, c),
						Samples: []promwrite.Sample{{T: end, V: *x.Value}}})
				}
				if !mainsFilled && now.Sub(end) < e.cfg.NullWait {
					return true // not filled in yet; ask again next poll
				}
			}
		}
		if end.After(now.Add(-e.cfg.Window)) && !e.write(ctx, series) {
			return false
		}
		e.hour = end
	}
	return true
}

type snapshot struct {
	devices []emporia.Device
	lastOK  time.Time
	newest  time.Time
	errors  map[string]int
}

func (e *Exporter) snapshot() snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := snapshot{devices: append([]emporia.Device(nil), e.devices...), lastOK: e.lastOK, newest: e.newest, errors: map[string]int{}}
	for k, v := range e.errors {
		s.errors[k] = v
	}
	return s
}
