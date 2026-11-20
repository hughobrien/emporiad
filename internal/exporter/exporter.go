// Package exporter reads Emporia's per-minute history and pushes it with
// remote write to each named target, one sample per minute at the minute's
// own time. /metrics serves the newest minute and hour too (each sample with
// its own timestamp, labelled pushed_<target>="true"), with health and device
// facts.
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
	"maps"
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
	ChartHours(ctx context.Context, gid int, channel string, start, end time.Time, unit string) ([]*float64, error)
}

// Store is one push target: where samples go, and where the exporter learns
// what that target already has.
type Store interface {
	Write(ctx context.Context, series []promwrite.Series) error
	Newest(ctx context.Context, selector string, lookback time.Duration) (time.Time, bool, error)
	Oldest(ctx context.Context, selector string, lookback time.Duration) (time.Time, bool, error)
}

// Target is a named Store; the name is the one in pushed_<name>.
type Target struct {
	Name  string
	Store Store
}

// Pushed lists the metric families sent by remote write. /metrics serves
// their newest sample too.
var Pushed = []string{"emporia_power_watts", "emporia_hourly_energy_kwh", "emporia_hourly_cost"}

// Config sets the cadence. Zero values take the defaults in New.
type Config struct {
	PollInterval time.Duration // 15m: how often to fetch the minutes since the last poll
	Job          string        // the job label on pushed series
	Settle       time.Duration // 2m: only take minutes that ended this long ago
	NullWait     time.Duration // 30m: how long to wait for a minute Emporia has not filled
	Backfill     time.Duration // 7d: how far back to start for a target that has nothing (Emporia keeps about a week)
	MaxSpan      time.Duration // 12h: the most minutes fetched in one poll (Emporia refuses more than 13h20m), so a backfill comes in steps
	MaxHours     int           // 12: the most hours priced in one poll
	HourSettle   time.Duration // 5m: wait this long after an hour ends before asking for its cost
	CallGap      time.Duration // 2s between Emporia calls, to stay gentle
	HistorySpan  time.Duration // 168h: how much of Emporia's hourly history one poll fetches, going back
	BackfillGap  time.Duration // 20s between history calls: it has all the time it needs
}

// sink is one target and how far it has got.
type sink struct {
	Target
	resumed      bool
	minute, hour time.Time // the next minute and hour it needs
	newest       time.Time // end of the newest minute it took
	errors       int       // failed writes
	oldest       time.Time // its oldest hourly sample, held or about to be: history goes before it
	historyDone  bool      // Emporia has nothing older, or the target refuses it
	// After an outage longer than resume looks back, the hours between the
	// target's old newest (gapFloor) and the forward start are walked first;
	// then the walk carries on from gapThen, its real oldest.
	gapFloor, gapThen time.Time
}

// Exporter polls Emporia and pushes what it finds.
type Exporter struct {
	cfg   Config
	api   API
	log   *slog.Logger
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// Owned by the poll loop.
	started      bool
	minute, hour time.Time // the next minute and hour to fetch for /metrics
	sinks        []*sink
	pushFailed   bool // a target failed a write this poll

	mu      sync.Mutex // guards the fields below and each sink's newest and errors
	devices []emporia.Device
	lastOK  time.Time
	newest  time.Time // end of the newest minute fetched
	errors  map[string]int
	latest  map[string]promwrite.Series // newest sample of each pushed series, for /metrics
	pushed  map[string]string           // pushed_<name>="true" for every target
}

// Stages counted in emporia_poll_errors_total.
var stages = []string{"devices", "resume", "usage", "cost", "push"}

// New returns an exporter; call Run to start polling. With no targets it
// only serves the newest minute on /metrics.
func New(cfg Config, api API, targets []Target, log *slog.Logger) *Exporter {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&cfg.PollInterval, 15*time.Minute)
	def(&cfg.Settle, 2*time.Minute)
	def(&cfg.NullWait, 30*time.Minute)
	def(&cfg.Backfill, 7*24*time.Hour)
	def(&cfg.MaxSpan, 12*time.Hour)
	def(&cfg.HourSettle, 5*time.Minute)
	def(&cfg.CallGap, 2*time.Second)
	def(&cfg.HistorySpan, 168*time.Hour)
	def(&cfg.BackfillGap, 20*time.Second)
	if cfg.MaxHours == 0 {
		cfg.MaxHours = 12
	}
	if cfg.Job == "" {
		cfg.Job = "emporiad"
	}
	e := &Exporter{cfg: cfg, api: api, log: log, now: time.Now, sleep: sleepCtx,
		errors: map[string]int{}, latest: map[string]promwrite.Series{}, pushed: map[string]string{}}
	for _, t := range targets {
		e.sinks = append(e.sinks, &sink{Target: t})
		e.pushed["pushed_"+t.Name] = "true"
	}
	return e
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
	if !e.started {
		now := e.now().UTC()
		e.minute, e.hour = now.Truncate(time.Minute), now.Truncate(time.Hour)
		e.started = true
	}
	resumed := e.resume(ctx)
	trees := buildTrees(ds)
	e.pushFailed = false
	ok := e.pollMinutes(ctx, trees)
	ok = ok && e.pollHours(ctx, trees)
	if ok && resumed && !e.pushFailed && err == nil {
		e.mu.Lock()
		e.lastOK = e.now()
		e.mu.Unlock()
	}
	if ok && !e.pushFailed {
		e.pollHistory(ctx, trees) // last, and only while Emporia and every target are answering
	}
}

// resume asks each target not yet resumed where its data ends, so a restart
// neither leaves a gap it could fill nor resends what the target has. A
// target with nothing starts Backfill ago. A target that cannot be asked is
// left out of this poll and asked again next time. It reports whether every
// target is resumed.
func (e *Exporter) resume(ctx context.Context) bool {
	const lookback = 8 * 24 * time.Hour // a little more than Emporia keeps
	// instance="" picks the pushed series: scraped copies carry an instance
	// label, should a target scrape emporiad without dropping them.
	sel := func(name string) string {
		return fmt.Sprintf(`%s{job=%q,instance="",channel_num="1,2,3"}`, name, e.cfg.Job)
	}
	all := true
	for _, s := range e.sinks {
		if s.resumed {
			continue
		}
		m, okM, err := s.Store.Newest(ctx, sel("emporia_power_watts"), lookback)
		var h, o time.Time
		okH, okO := false, false
		if err == nil {
			h, okH, err = s.Store.Newest(ctx, sel("emporia_hourly_cost"), lookback)
		}
		if err == nil {
			o, okO, err = s.Store.Oldest(ctx, sel("emporia_hourly_energy_kwh"), 5*365*24*time.Hour) // the series the walk stops on
		}
		if err != nil {
			e.fail("resume", fmt.Errorf("%s: %w", s.Name, err))
			all = false
			continue
		}
		from := e.now().UTC().Add(-e.cfg.Backfill)
		s.minute, s.hour = from.Truncate(time.Minute), from.Truncate(time.Hour)
		if okM {
			s.minute = m.Truncate(time.Minute) // samples sit at the end of their minute
			e.mu.Lock()
			s.newest = m
			e.mu.Unlock()
		}
		if okH {
			s.hour = h.Truncate(time.Hour)
		}
		s.oldest = s.hour.Add(time.Hour) // the first sample pollHours writes
		s.gapFloor, s.gapThen = time.Time{}, time.Time{}
		if okO && !okH {
			// It holds hours, none recent: find where they end.
			n, okN, err := s.Store.Newest(ctx, sel("emporia_hourly_energy_kwh"), 5*365*24*time.Hour)
			if err != nil {
				e.fail("resume", fmt.Errorf("%s: %w", s.Name, err))
				all = false
				continue
			}
			if okN && n.Before(s.oldest) {
				s.gapFloor, s.gapThen = n, o
				okO = false
			}
		}
		if okO {
			s.oldest = o.Truncate(time.Hour) // samples sit on the hour; a stray one must not shift every week
		}
		s.resumed = true
		e.log.Info("resuming", "target", s.Name, "minute", s.minute, "hour", s.hour, "oldest_hour", s.oldest,
			"found_minute", okM, "found_hour", okH, "found_oldest", okO)
	}
	return all
}

// ready lists the targets this poll pushes to.
func (e *Exporter) ready() []*sink {
	var out []*sink
	for _, s := range e.sinks {
		if s.resumed {
			out = append(out, s)
		}
	}
	return out
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

// pollMinutes fetches every minute from the oldest any target (or /metrics)
// needs up to the newest settled one, at most MaxSpan of them, and pushes
// each target the part it lacks. It reports whether Emporia answered.
func (e *Exporter) pollMinutes(ctx context.Context, trees []tree) bool {
	sinks := e.ready()
	from := e.minute
	for _, s := range sinks {
		if s.minute.Before(from) {
			from = s.minute
		}
	}
	now := e.now().UTC()
	end := now.Add(-e.cfg.Settle).Truncate(time.Minute)
	if end.Sub(from) > e.cfg.MaxSpan {
		end = from.Add(e.cfg.MaxSpan)
	}
	n := int(end.Sub(from) / time.Minute)
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
			vals, err := e.api.Chart(ctx, c.gid, c.num, from, end, emporia.KWh)
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

	// A minute is ready once every mains has it and the minute after it.
	// Emporia's newest minute for mains can read low (two thirds of the
	// circuits' sum, at the end of every poll from 2026-11-19 22:29Z), so it
	// waits for the next poll, which reads it again. Emporia fills a minute
	// for the circuits as it does for mains, so a circuit still empty then
	// has no data. A minute whose mains stays empty past NullWait is skipped,
	// and one that old does not wait for the next.
	ts := func(i int) time.Time { return from.Add(time.Duration(i+1) * time.Minute) }
	filled := func(i int) bool {
		if i >= n {
			return false
		}
		for ti := range trees {
			if at(mains[ti], i) == nil {
				return false
			}
		}
		return true
	}
	ready := 0
	var newest time.Time
	for i := range n {
		fresh := now.Sub(ts(i)) < e.cfg.NullWait
		if fresh && (!filled(i) || !filled(i+1)) {
			break
		}
		if filled(i) {
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
	e.remember(series)

	upTo := from.Add(time.Duration(ready) * time.Minute)
	for _, s := range sinks {
		if !s.minute.Before(upTo) {
			continue
		}
		if done, _ := e.write(ctx, s, after(series, s.minute)); done {
			s.minute = upTo
			if newest.After(s.newest) {
				e.mu.Lock()
				s.newest = newest
				e.mu.Unlock()
			}
		}
	}
	if upTo.After(e.minute) {
		e.minute = upTo
	}
	e.mu.Lock()
	if newest.After(e.newest) {
		e.newest = newest
	}
	e.mu.Unlock()
	return true
}

// after keeps the samples later than t (a sample sits at the end of its
// minute, so a target at minute t has everything up to t).
func after(series []promwrite.Series, t time.Time) []promwrite.Series {
	var out []promwrite.Series
	for _, s := range series {
		i, _ := slices.BinarySearchFunc(s.Samples, t, func(p promwrite.Sample, t time.Time) int {
			if p.T.After(t) {
				return 1
			}
			return -1
		})
		if i < len(s.Samples) {
			out = append(out, promwrite.Series{Labels: s.Labels, Samples: s.Samples[i:]})
		}
	}
	return out
}

// write pushes series to one target and reports whether that target is done
// with them, and whether it refused them. A write the target refuses outright
// counts as done: the same request would be refused again, and stopping would
// stall every later minute behind it. Any other failure keeps the target
// where it was, so the next poll fetches its minutes again.
func (e *Exporter) write(ctx context.Context, s *sink, series []promwrite.Series) (done, refused bool) {
	if len(series) == 0 {
		return true, false
	}
	err := s.Store.Write(ctx, series)
	if err == nil {
		return true, false
	}
	e.mu.Lock()
	s.errors++
	e.mu.Unlock()
	e.fail("push", fmt.Errorf("%s: %w", s.Name, err))
	if r := (*promwrite.RefusedError)(nil); errors.As(err, &r) {
		return true, true
	}
	e.pushFailed = true
	return false, false
}

// remember keeps the newest sample of each series for /metrics.
func (e *Exporter) remember(series []promwrite.Series) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range series {
		if len(s.Samples) == 0 {
			continue
		}
		l := s.Labels
		k := l["__name__"] + "\x00" + l["device"] + "\x00" + l["channel_num"]
		last := s.Samples[len(s.Samples)-1]
		if have, ok := e.latest[k]; ok && !last.T.After(have.Samples[0].T) {
			continue // a backfill is older than what /metrics already shows
		}
		e.latest[k] = promwrite.Series{Labels: l, Samples: []promwrite.Sample{last}}
	}
}

// pollHours pushes Emporia's energy and plan-priced cost for each finished
// hour whose minutes have been fetched, oldest first, to each target that
// lacks it. It reports whether Emporia answered.
func (e *Exporter) pollHours(ctx context.Context, trees []tree) bool {
	sinks := e.ready()
	failed := map[*sink]bool{}
	for range e.cfg.MaxHours { // the rest next poll
		hour := e.hour
		for _, s := range sinks {
			if !failed[s] && s.hour.Before(hour) {
				hour = s.hour
			}
		}
		now := e.now().UTC()
		end := hour.Add(time.Hour)
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
				us, err := e.api.Usage(ctx, t.gids, hour, "1H", u.unit)
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
		e.remember(series)
		for _, s := range sinks {
			if failed[s] || !s.hour.Equal(hour) {
				continue
			}
			if done, _ := e.write(ctx, s, series); done {
				s.hour = end
			} else {
				failed[s] = true // keep its hours in order; it catches up next poll
			}
		}
		if !e.hour.After(hour) {
			e.hour = end
		}
	}
	return true
}

// pollHistory walks back through Emporia's hourly history: it fetches the
// HistorySpan of hours before the oldest any unfinished target holds, every
// channel in kWh and dollars, BackfillGap apart, and sends each target the
// hours older than its own oldest. A span whose mains are empty in every hour
// is before the monitor was installed, and ends the walk. An Emporia error
// pushes nothing, so the same span is asked for next poll.
func (e *Exporter) pollHistory(ctx context.Context, trees []tree) {
	var sinks []*sink
	var to time.Time
	for _, s := range e.ready() {
		if !s.historyDone {
			sinks = append(sinks, s)
			if s.oldest.After(to) {
				to = s.oldest
			}
		}
	}
	if len(sinks) == 0 {
		return
	}
	// Sample i sits at the end of hour start+i.
	from := to.Add(-e.cfg.HistorySpan)
	start, end := from.Add(-time.Hour), to.Add(-time.Hour)
	ts := func(i int) time.Time { return start.Add(time.Duration(i+1) * time.Hour) }
	served := func(s *sink) bool { return s.oldest.After(from) }
	fetch := func(c channel, unit string) ([]*float64, bool) {
		if e.sleep(ctx, e.cfg.BackfillGap) != nil {
			return nil, false
		}
		vs, err := e.api.ChartHours(ctx, c.gid, c.num, start, end, unit)
		if err != nil {
			e.fail("usage", fmt.Errorf("hourly history, channel %s: %w", c.num, err))
			return nil, false
		}
		return vs, true
	}
	at := func(vs []*float64, i int) *float64 {
		if i < len(vs) {
			return vs[i]
		}
		return nil
	}
	n := int(end.Sub(start) / time.Hour)

	mainsKWh := make([][]*float64, len(trees))
	empty := true
	for ti, t := range trees {
		vs, ok := fetch(*t.mains, emporia.KWh)
		if !ok {
			return
		}
		mainsKWh[ti] = vs
		for i := range n {
			empty = empty && at(vs, i) == nil
		}
	}
	// jump ends an outage's gap once the walk has reached the old data.
	jump := func(s *sink) bool {
		if s.gapFloor.IsZero() || from.After(s.gapFloor) {
			return false
		}
		s.oldest, s.gapFloor, s.gapThen = s.gapThen, time.Time{}, time.Time{}
		return true
	}
	if empty {
		for _, s := range sinks {
			if !served(s) {
				continue
			}
			if !s.gapFloor.IsZero() { // the monitor was off for this week of the gap
				s.oldest, s.gapFloor, s.gapThen = s.gapThen, time.Time{}, time.Time{}
				continue
			}
			s.historyDone = true
			e.log.Info("hourly history complete", "target", s.Name, "oldest_hour", s.oldest)
		}
		return
	}

	var series []promwrite.Series
	add := func(name string, c channel, vs func(i int) *float64) {
		s := promwrite.Series{Labels: e.labels(name, c)}
		for i := range n {
			if v := vs(i); v != nil {
				s.Samples = append(s.Samples, promwrite.Sample{T: ts(i), V: *v})
			}
		}
		if len(s.Samples) > 0 {
			series = append(series, s)
		}
	}
	for ti, t := range trees {
		for _, u := range []struct{ unit, name string }{
			{emporia.KWh, "emporia_hourly_energy_kwh"},
			{emporia.Dollars, "emporia_hourly_cost"},
		} {
			mains := mainsKWh[ti]
			if u.unit != emporia.KWh {
				var ok bool
				if mains, ok = fetch(*t.mains, u.unit); !ok {
					return
				}
			}
			var circuits [][]*float64
			for _, c := range t.circuits {
				vs, ok := fetch(c, u.unit)
				if !ok {
					return
				}
				circuits = append(circuits, vs)
				add(u.name, c, func(i int) *float64 { return at(vs, i) })
			}
			add(u.name, *t.mains, func(i int) *float64 { return at(mains, i) })
			add(u.name, t.balance, func(i int) *float64 {
				m := at(mains, i)
				if m == nil {
					return nil
				}
				b := *m
				for _, c := range circuits {
					v := at(c, i)
					if v == nil {
						return nil
					}
					b -= *v
				}
				return &b
			})
		}
	}

	for _, s := range sinks {
		if !served(s) {
			continue
		}
		send := before(series, s.oldest)
		if !s.gapFloor.IsZero() {
			send = after(send, s.gapFloor)
		}
		done, refused := e.write(ctx, s, send)
		if refused {
			s.historyDone = true
			e.log.Info("hourly history refused; not sending more", "target", s.Name)
		}
		if done && !jump(s) {
			s.oldest = from
		}
	}
}

// before keeps the samples earlier than t.
func before(series []promwrite.Series, t time.Time) []promwrite.Series {
	var out []promwrite.Series
	for _, s := range series {
		i, _ := slices.BinarySearchFunc(s.Samples, t, func(p promwrite.Sample, t time.Time) int {
			if p.T.Before(t) {
				return -1
			}
			return 1
		})
		if i > 0 {
			out = append(out, promwrite.Series{Labels: s.Labels, Samples: s.Samples[:i]})
		}
	}
	return out
}

type pushState struct {
	name   string
	newest time.Time
	errors int
}

type snapshot struct {
	devices []emporia.Device
	lastOK  time.Time
	newest  time.Time
	errors  map[string]int
	latest  []promwrite.Series
	targets []pushState
}

func (e *Exporter) snapshot() snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := snapshot{devices: append([]emporia.Device(nil), e.devices...), lastOK: e.lastOK, newest: e.newest,
		errors: maps.Clone(e.errors)}
	for _, k := range slices.Sorted(maps.Keys(e.latest)) {
		s.latest = append(s.latest, e.latest[k])
	}
	for _, t := range e.sinks {
		s.targets = append(s.targets, pushState{t.Name, t.newest, t.errors})
	}
	return s
}
