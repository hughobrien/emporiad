package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
	"github.com/hughobrien/emporiad/internal/promwrite"
)

var t0 = time.Date(2026, 11, 5, 9, 4, 30, 0, time.UTC)

type chartCall struct {
	gid        int
	num        string
	start, end time.Time
}

// fakeAPI serves one Vue (mains) with a circuit expansion (Oven, and an
// unnamed circuit 2). Every minute's kWh comes from kwh; minutes from
// filledTo on are not filled yet.
type fakeAPI struct {
	kwh       func(num string, minute time.Time) *float64
	filledTo  time.Time
	chartErr  map[string]error
	charts    []chartCall
	usage     map[string][]emporia.Usage // by unit
	usageAsks []time.Time

	// Hourly history: each hour's value by channel, unit and the hour's
	// start; nil means Emporia has nothing (the default: no history).
	hourly     func(num, unit string, hour time.Time) *float64
	hourErr    map[string]error
	hourCharts []hourCall
	calls      []string // chart, usage, hour and sleep, in order

	// newestMainsLow makes the newest minute a chart call returns for mains
	// read two thirds of its value, as Emporia's did on 2026-11-19.
	newestMainsLow bool
}

type hourCall struct {
	gid        int
	num, unit  string
	start, end time.Time
}

func (a *fakeAPI) ChartHours(_ context.Context, gid int, num string, start, end time.Time, unit string) ([]*float64, error) {
	a.calls = append(a.calls, "hour")
	a.hourCharts = append(a.hourCharts, hourCall{gid, num, unit, start, end})
	if err := a.hourErr[num]; err != nil {
		return nil, err
	}
	var out []*float64
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		var v *float64
		if a.hourly != nil {
			v = a.hourly(num, unit, h)
		}
		out = append(out, v)
	}
	return out, nil
}

func f(v float64) *float64 { return &v }

func (a *fakeAPI) Devices(context.Context) ([]emporia.Device, error) {
	on := true
	return []emporia.Device{
		{Gid: 1, Model: "VUE003", Firmware: "fw1", Name: "Home", Price: 0.25, Connected: &on,
			Channels: []emporia.Channel{{Num: "1,2,3"}}},
		{Gid: 2, Model: "WAT001", Name: "Home", Price: 0.25, Parent: 1,
			Channels: []emporia.Channel{{Num: "1", Name: "Oven"}, {Num: "2"}}},
	}, nil
}

func (a *fakeAPI) Chart(_ context.Context, gid int, num string, start, end time.Time, unit string) ([]*float64, error) {
	a.calls = append(a.calls, "chart")
	a.charts = append(a.charts, chartCall{gid, num, start, end})
	if err := a.chartErr[num]; err != nil {
		return nil, err
	}
	var out []*float64
	for m := start; m.Before(end) && m.Before(a.filledTo); m = m.Add(time.Minute) {
		out = append(out, a.kwh(num, m))
	}
	if a.newestMainsLow && num == "1,2,3" && len(out) > 0 && out[len(out)-1] != nil {
		out[len(out)-1] = f(*out[len(out)-1] * 2 / 3)
	}
	return out, nil
}

func (a *fakeAPI) Usage(_ context.Context, gids []int, at time.Time, scale, unit string) ([]emporia.Usage, error) {
	if scale != "1H" || len(gids) != 2 {
		return nil, fmt.Errorf("unexpected usage call %v %s", gids, scale)
	}
	a.calls = append(a.calls, "usage")
	a.usageAsks = append(a.usageAsks, at)
	return a.usage[unit], nil
}

type fakeStore struct {
	selectors []string
	writes    [][]promwrite.Series
	newest    map[string]time.Time
	oldest    map[string]time.Time
	stale     map[string]time.Time // newest samples further back than resume looks
	held      []time.Time          // hourly kWh samples the target held before any write (the mains series)
	writeErr  error
	newestErr error
}

func (s *fakeStore) Write(_ context.Context, series []promwrite.Series) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.writes = append(s.writes, series)
	return nil
}

func (s *fakeStore) Newest(_ context.Context, sel string, lookback time.Duration) (time.Time, bool, error) {
	s.selectors = append(s.selectors, sel)
	if s.newestErr != nil {
		return time.Time{}, false, s.newestErr
	}
	// The real store refuses a subquery of more than 100000 points; Newest steps by the minute.
	if lookback/time.Minute > 100000 {
		return time.Time{}, false, fmt.Errorf("too many points: %d for a 1m step", lookback/time.Minute)
	}
	for k, v := range s.newest {
		if sel == k || strings.HasPrefix(sel, k+"{") {
			return v, true, nil
		}
	}
	return time.Time{}, false, nil
}

func (s *fakeStore) NewestHourly(_ context.Context, sel string, lookback time.Duration) (time.Time, bool, error) {
	s.selectors = append(s.selectors, sel)
	if s.newestErr != nil {
		return time.Time{}, false, s.newestErr
	}
	for k, v := range s.newest {
		if sel == k || strings.HasPrefix(sel, k+"{") {
			return v, true, nil
		}
	}
	for k, v := range s.stale { // older than resume's lookback: only a long one finds it
		if (sel == k || strings.HasPrefix(sel, k+"{")) && lookback > 9*24*time.Hour {
			return v, true, nil
		}
	}
	return time.Time{}, false, nil
}

func (s *fakeStore) Oldest(_ context.Context, sel string, _ time.Duration) (time.Time, bool, error) {
	s.selectors = append(s.selectors, sel)
	if s.newestErr != nil {
		return time.Time{}, false, s.newestErr
	}
	for k, v := range s.oldest {
		if sel == k || strings.HasPrefix(sel, k+"{") {
			return v, true, nil
		}
	}
	return time.Time{}, false, nil
}

// hours returns the times of the mains hourly kWh samples the store holds:
// the seeded ones and every one written.
func (s *fakeStore) hours() map[time.Time]bool {
	out := map[time.Time]bool{}
	for _, h := range s.held {
		out[h] = true
	}
	for _, p := range find(s.writes, "emporia_hourly_energy_kwh", "1,2,3") {
		out[p.T] = true
	}
	return out
}

// RunStart answers from the samples held, as the store's query does: the
// newest sample whose hour before has none.
func (s *fakeStore) RunStart(_ context.Context, sel string, _ time.Duration) (time.Time, bool, error) {
	s.selectors = append(s.selectors, sel)
	var out time.Time
	for h := range s.hours() {
		if !s.hours()[h.Add(-time.Hour)] && h.After(out) {
			out = h
		}
	}
	return out, !out.IsZero(), nil
}

// NewestHourlyBefore is the newest sample held at or before the time.
func (s *fakeStore) NewestHourlyBefore(_ context.Context, sel string, before time.Time, _ time.Duration) (time.Time, bool, error) {
	s.selectors = append(s.selectors, sel)
	var out time.Time
	for h := range s.hours() {
		if !h.After(before) && h.After(out) {
			out = h
		}
	}
	return out, !out.IsZero(), nil
}

// Mains 0.01 kWh a minute (600 W), Oven 0.002 (120 W), circuit 2 0.001 (60 W).
func steady(num string, _ time.Time) *float64 {
	return map[string]*float64{"1,2,3": f(0.01), "1": f(0.002), "2": f(0.001)}[num]
}

func setup() (*Exporter, *fakeAPI, *fakeStore, *time.Time) {
	api := &fakeAPI{kwh: steady, filledTo: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), chartErr: map[string]error{}, hourErr: map[string]error{},
		usage: map[string][]emporia.Usage{
			emporia.KWh: {
				{DeviceGid: 2, Num: "1,2,3", Name: "Main", Value: f(0.6)},
				{DeviceGid: 2, Num: "1", Name: "Oven", Value: f(0.12)},
				{DeviceGid: 2, Num: "2", Value: nil},
				{DeviceGid: 2, Num: "Balance", Name: "Balance", Value: f(0.42)},
				{DeviceGid: 9, Num: "1,2,3", Name: "Plug", Value: f(1)}, // another device's channel
			},
			emporia.Dollars: {
				{DeviceGid: 2, Num: "1,2,3", Name: "Main", Value: f(0.15)},
				{DeviceGid: 2, Num: "Balance", Name: "Balance", Value: f(0.1)},
			},
		}}
	store := &fakeStore{newest: map[string]time.Time{}, oldest: map[string]time.Time{}}
	now := t0
	e := New(Config{}, api, []Target{{"vm", store}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return now }
	e.sleep = func(context.Context, time.Duration) error { return nil }
	return e, api, store, &now
}

func scrape(t *testing.T, e *Exporter) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var keep []string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(l, "emporia") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// find returns the pushed series with this name and channel_num.
func find(ws [][]promwrite.Series, name, num string) []promwrite.Sample {
	var out []promwrite.Sample
	for _, w := range ws {
		for _, s := range w {
			if s.Labels["__name__"] == name && s.Labels["channel_num"] == num {
				out = append(out, s.Samples...)
			}
		}
	}
	return out
}

func labelsOf(ws [][]promwrite.Series, name, num string) map[string]string {
	for _, w := range ws {
		for _, s := range w {
			if s.Labels["__name__"] == name && s.Labels["channel_num"] == num {
				return s.Labels
			}
		}
	}
	return nil
}

func TestPushesEveryMinute(t *testing.T) {
	e, api, store, now := setup()
	ctx := context.Background()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 4, 0, 0, time.UTC)
	store.newest["emporia_hourly_cost"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	e.poll(ctx) // 09:04:30, the target has up to 09:04: nothing settled since
	if len(store.writes) != 0 || len(api.charts) != 0 {
		t.Fatalf("first poll should only find its start: %d writes, %d charts", len(store.writes), len(api.charts))
	}
	*now = t0.Add(15 * time.Minute) // 09:19:30: minutes 09:04 to 09:16 have settled; 09:16, the newest, waits for the next poll
	e.poll(ctx)
	if len(api.charts) != 3 {
		t.Fatalf("want one chart call per channel, got %v", api.charts)
	}
	want := chartCall{1, "1,2,3", time.Date(2026, 11, 5, 9, 4, 0, 0, time.UTC), time.Date(2026, 11, 5, 9, 17, 0, 0, time.UTC)}
	if api.charts[0] != want || api.charts[1].gid != 2 || api.charts[1].num != "1" {
		t.Errorf("charts %v, want mains first from the Vue %v", api.charts, want)
	}
	for num, w := range map[string]float64{"1,2,3": 600, "1": 120, "2": 60, "Balance": 420} {
		s := find(store.writes, "emporia_power_watts", num)
		if len(s) != 12 {
			t.Fatalf("channel %s: %d samples, want 12", num, len(s))
		}
		for i, p := range s {
			if at := time.Date(2026, 11, 5, 9, 5+i, 0, 0, time.UTC); !p.T.Equal(at) || p.V < w-1e-9 || p.V > w+1e-9 {
				t.Fatalf("channel %s sample %d: %v, want %v W at the end of its minute, %v", num, i, p, w, at)
			}
		}
	}
	if l := labelsOf(store.writes, "emporia_power_watts", "1,2,3"); l["channel"] != "Main" || l["model"] != "VUE003" || l["job"] != "emporiad" || l["device"] != "Home" {
		t.Errorf("mains labels %v", l)
	}
	if l := labelsOf(store.writes, "emporia_power_watts", "2"); l["channel"] != "Channel 2" || l["model"] != "WAT001" {
		t.Errorf("unnamed circuit labels %v", l)
	}
	got := scrape(t, e)
	for _, w := range []string{
		fmt.Sprintf("emporia_newest_minute_timestamp_seconds %v", float64(time.Date(2026, 11, 5, 9, 16, 0, 0, time.UTC).Unix())),
		`emporia_price_per_kwh{device="Home",model="VUE003"} 0.25`,
		`emporia_device_connected{device="Home",model="VUE003"} 1`,
		`emporia_poll_errors_total{stage="push"} 0`,
		"emporia_last_success_timestamp_seconds",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s\n--- got:\n%s", w, got)
		}
	}
	// The newest minute is served too, at its own time, marked as pushed to
	// vm (whose scrape drops it), and without job (the scraper sets that).
	ms := time.Date(2026, 11, 5, 9, 16, 0, 0, time.UTC).UnixMilli()
	for _, w := range []string{
		fmt.Sprintf(`emporia_power_watts{channel="Main",channel_num="1,2,3",device="Home",model="VUE003",pushed_vm="true"} 600 %d`, ms),
		fmt.Sprintf(`emporia_power_watts{channel="Balance",channel_num="Balance",device="Home",model="VUE003",pushed_vm="true"} 420 %d`, ms),
		`emporia_push_errors_total{target="vm"} 0`,
		fmt.Sprintf(`emporia_pushed_minute_timestamp_seconds{target="vm"} %v`, float64(ms/1000)),
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s\n--- got:\n%s", w, got)
		}
	}

	*now = t0.Add(30 * time.Minute)
	e.poll(ctx)
	if c, held := api.charts[len(api.charts)-1], want.end.Add(-time.Minute); !c.start.Equal(held) {
		t.Errorf("next poll starts at %v, want %v, the minute the last held back", c.start, held)
	}
}

func TestResumesFromTheTarget(t *testing.T) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 8, 50, 0, 0, time.UTC)
	*now = t0.Add(10 * time.Minute) // 09:14:30
	e.poll(context.Background())
	if c := api.charts[0]; !c.start.Equal(time.Date(2026, 11, 5, 8, 50, 0, 0, time.UTC)) {
		t.Fatalf("started at %v, want 08:50 where the target's data ends", c.start)
	}
	for _, sel := range store.selectors {
		if !strings.Contains(sel, `instance=""`) || !strings.Contains(sel, `job="emporiad"`) {
			t.Errorf("selector %s could match scraped series from an older emporiad", sel)
		}
	}
	if s := find(store.writes, "emporia_power_watts", "1,2,3"); len(s) != 21 || !s[0].T.Equal(time.Date(2026, 11, 5, 8, 51, 0, 0, time.UTC)) {
		t.Fatalf("want 08:51 to 09:11 (09:12, the newest, waits), got %d samples from %v", len(s), s[0].T)
	}
}

func TestBackfillsTwelveHoursAPoll(t *testing.T) {
	e, api, store, now := setup()
	behind := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	store.newest["emporia_power_watts"] = behind
	*now = t0.Add(15 * time.Minute)
	e.poll(context.Background())
	if c := api.charts[0]; !c.start.Equal(behind) || !c.end.Equal(behind.Add(12*time.Hour)) {
		t.Fatalf("fetched %v to %v, want 12h from %v (Emporia refuses over 13h20m)", c.start, c.end, behind)
	}
	if s := find(store.writes, "emporia_power_watts", "1,2,3"); len(s) != 12*60 {
		t.Fatalf("want 12h of minutes pushed, got %d", len(s))
	}
	*now = now.Add(15 * time.Minute)
	e.poll(context.Background())
	if c := api.charts[len(api.charts)-1]; !c.start.Equal(behind.Add(12 * time.Hour)) {
		t.Errorf("second poll starts at %v, want where the first stopped", c.start)
	}
	// A backfill never moves /metrics back: it shows the newest minute seen.
	if got := scrape(t, e); !strings.Contains(got, fmt.Sprintf("600 %d", behind.Add(24*time.Hour).UnixMilli())) {
		t.Errorf("want the newest fetched minute on /metrics:\n%s", got)
	}
}

func TestEmptyTargetBackfillsAWeek(t *testing.T) {
	e, api, _, _ := setup()
	e.poll(context.Background())
	if c := api.charts[0]; !c.start.Equal(t0.Add(-7 * 24 * time.Hour).Truncate(time.Minute)) {
		t.Fatalf("started at %v, want a week back", c.start)
	}
}

func TestNoTargetsServesTheNewestMinute(t *testing.T) {
	_, api, _, now := setup()
	e := New(Config{}, api, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return *now }
	e.sleep = func(context.Context, time.Duration) error { return nil }
	e.poll(context.Background())
	*now = t0.Add(15 * time.Minute)
	e.poll(context.Background())
	got := scrape(t, e)
	if !strings.Contains(got, `emporia_power_watts{channel="Main",channel_num="1,2,3",device="Home",model="VUE003"} 600`) || strings.Contains(got, "pushed_") {
		t.Errorf("want unmarked power on /metrics:\n%s", got)
	}
	if c := api.charts[0]; !c.start.Equal(t0.Truncate(time.Minute)) {
		t.Errorf("with nothing to backfill for, start at the current minute, not %v", c.start)
	}
}

func TestEachTargetGetsWhatItLacks(t *testing.T) {
	_, api, _, now := setup()
	up := &fakeStore{newest: map[string]time.Time{"emporia_power_watts": time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}}
	down := &fakeStore{newest: map[string]time.Time{"emporia_power_watts": time.Date(2026, 11, 5, 8, 40, 0, 0, time.UTC)},
		writeErr: errors.New("connection refused")}
	e := New(Config{}, api, []Target{{"up", up}, {"down", down}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return *now }
	e.sleep = func(context.Context, time.Duration) error { return nil }
	*now = t0.Add(15 * time.Minute) // minutes settle to 09:17; that one waits for the next poll
	e.poll(context.Background())
	if c := api.charts[0]; !c.start.Equal(time.Date(2026, 11, 5, 8, 40, 0, 0, time.UTC)) {
		t.Fatalf("want one fetch from the furthest-behind target, got %v", c.start)
	}
	if s := find(up.writes, "emporia_power_watts", "1,2,3"); len(s) != 16 || !s[0].T.Equal(time.Date(2026, 11, 5, 9, 1, 0, 0, time.UTC)) {
		t.Fatalf("up should get only 09:01 to 09:16, got %d from %v", len(s), s)
	}
	if got := scrape(t, e); strings.Contains(got, `emporia_push_errors_total{target="down"} 0`) || strings.Contains(got, "emporia_last_success") {
		t.Errorf("want down's failure counted and the poll not a success:\n%s", got)
	}
	down.writeErr = nil
	*now = t0.Add(30 * time.Minute) // to 09:32, which waits
	e.poll(context.Background())
	if c := api.charts[len(api.charts)-3]; !c.start.Equal(time.Date(2026, 11, 5, 8, 40, 0, 0, time.UTC)) {
		t.Fatalf("down is still at 08:40, so fetch from there: %v", c.start)
	}
	if s := find(down.writes, "emporia_power_watts", "1,2,3"); len(s) != 51 {
		t.Errorf("down should catch up 08:41 to 09:31 in one write, got %d", len(s))
	}
	if s := find(up.writes, "emporia_power_watts", "1,2,3"); len(s) != 16+15 {
		t.Errorf("up should only get the new 15, got %d in all", len(s))
	}
}

func TestUnreachableTargetDoesNotStopTheOthers(t *testing.T) {
	_, api, _, now := setup()
	up := &fakeStore{newest: map[string]time.Time{"emporia_power_watts": time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}}
	gone := &fakeStore{newestErr: errors.New("connection refused")}
	e := New(Config{}, api, []Target{{"up", up}, {"gone", gone}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return *now }
	e.sleep = func(context.Context, time.Duration) error { return nil }
	*now = t0.Add(15 * time.Minute)
	e.poll(context.Background())
	if len(up.writes) == 0 {
		t.Fatal("a target that cannot be asked should not hold up the rest")
	}
	gone.newestErr = nil
	gone.newest = map[string]time.Time{"emporia_power_watts": time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)}
	*now = t0.Add(30 * time.Minute)
	e.poll(context.Background())
	if s := find(gone.writes, "emporia_power_watts", "1,2,3"); len(s) == 0 || !s[0].T.Equal(time.Date(2026, 11, 5, 8, 1, 0, 0, time.UTC)) {
		t.Errorf("once reachable it resumes from its own data: %v", s)
	}
}

func TestWaitsForUnfilledMinutes(t *testing.T) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	api.filledTo = time.Date(2026, 11, 5, 9, 10, 0, 0, time.UTC)
	*now = t0.Add(15 * time.Minute)
	e.poll(context.Background())
	if s := find(store.writes, "emporia_power_watts", "1,2,3"); len(s) != 9 {
		t.Fatalf("want the 10 filled minutes but the newest, which waits: got %d", len(s))
	}
	api.filledTo = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	*now = t0.Add(30 * time.Minute)
	e.poll(context.Background())
	if c := api.charts[len(api.charts)-1]; !c.start.Equal(time.Date(2026, 11, 5, 9, 9, 0, 0, time.UTC)) {
		t.Errorf("second poll started at %v, want 09:09, the minute held back", c.start)
	}
}

// Emporia's newest minute for mains can read low (two thirds, at the end of
// every poll from 2026-11-19 22:29Z) while the circuits are right, so the
// newest minute waits for the next poll, which reads it again.
func TestNewestMainsMinuteWaitsForTheNextPoll(t *testing.T) {
	e, api, store, now := setup()
	api.newestMainsLow = true
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	*now = time.Date(2026, 11, 5, 9, 10, 30, 0, time.UTC) // minutes 09:00 to 09:07 settled
	e.poll(context.Background())
	*now = time.Date(2026, 11, 5, 9, 25, 30, 0, time.UTC)
	e.poll(context.Background())
	for _, num := range []string{"1,2,3", "Balance"} {
		s := find(store.writes, "emporia_power_watts", num)
		seen := map[time.Time]bool{}
		for _, p := range s {
			if seen[p.T] {
				t.Errorf("channel %s: %v pushed twice", num, p.T)
			}
			seen[p.T] = true
		}
		want := map[string]float64{"1,2,3": 600, "Balance": 420}[num]
		for _, p := range s {
			if p.V < want-1e-9 || p.V > want+1e-9 {
				t.Errorf("channel %s at %v: %v W, want %v (a low newest minute was kept)", num, p.T, p.V, want)
			}
		}
		if len(s) != 22 { // 09:01 to 09:22; 09:23 waits for the poll after
			t.Errorf("channel %s: %d samples, want 22", num, len(s))
		}
	}
}

func TestEmptyMainsMinute(t *testing.T) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	gap := time.Date(2026, 11, 5, 9, 3, 0, 0, time.UTC)
	api.kwh = func(num string, m time.Time) *float64 {
		if m.Equal(gap) {
			return nil
		}
		return steady(num, m)
	}
	*now = time.Date(2026, 11, 5, 9, 20, 30, 0, time.UTC) // the gap ended 16.5 min ago: wait
	e.poll(context.Background())
	if s := find(store.writes, "emporia_power_watts", "1,2,3"); len(s) != 2 {
		t.Fatalf("want two of the three minutes before the gap (the third waits on the next), got %d", len(s))
	}
	*now = time.Date(2026, 11, 5, 9, 40, 30, 0, time.UTC) // 36.5 min: give up on it
	e.poll(context.Background())
	s := find(store.writes, "emporia_power_watts", "1,2,3")
	if len(s) != 2+34 || !s[3].T.Equal(gap.Add(2*time.Minute)) {
		t.Fatalf("want the gap skipped and the rest pushed: %d samples, s[3] at %v", len(s), s[3].T)
	}
	if b := find(store.writes, "emporia_power_watts", "Balance"); len(b) != len(s) {
		t.Errorf("Balance %d samples, mains %d", len(b), len(s))
	}
}

func TestCircuitWithoutDataHasNoBalance(t *testing.T) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	api.kwh = func(num string, m time.Time) *float64 {
		if num == "2" && m.Minute() == 1 {
			return nil
		}
		return steady(num, m)
	}
	*now = time.Date(2026, 11, 5, 9, 6, 30, 0, time.UTC) // 09:00 to 09:02 ready; 09:03 waits
	e.poll(context.Background())
	if c, b := find(store.writes, "emporia_power_watts", "2"), find(store.writes, "emporia_power_watts", "Balance"); len(c) != 2 || len(b) != 2 {
		t.Fatalf("circuit 2 %d samples, Balance %d; want 2 each (09:01 has no circuit 2)", len(c), len(b))
	}
}

func TestChartErrorPushesNothing(t *testing.T) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	api.chartErr["1"] = errors.New("HTTP 500")
	*now = t0.Add(15 * time.Minute)
	e.poll(context.Background())
	if len(store.writes) != 0 || e.sinks[0].minute != time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) {
		t.Fatalf("a failed channel should push nothing and keep the start: %d writes, minute %v", len(store.writes), e.sinks[0].minute)
	}
	if got := scrape(t, e); !strings.Contains(got, `emporia_poll_errors_total{stage="usage"} 1`) || strings.Contains(got, "emporia_last_success_timestamp_seconds") {
		t.Errorf("want the error counted and no success:\n%s", got)
	}
}

func TestWriteFailures(t *testing.T) {
	start := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		err     error
		advance bool
	}{
		{errors.New("connection refused"), false},
		{&promwrite.RefusedError{Status: 400, Body: "out of order sample"}, true},
	} {
		e, _, store, now := setup()
		store.newest["emporia_power_watts"] = start
		store.writeErr = tc.err
		*now = t0.Add(15 * time.Minute)
		e.poll(context.Background())
		if moved := e.sinks[0].minute.After(start); moved != tc.advance {
			t.Errorf("%v: moved on %v, want %v", tc.err, moved, tc.advance)
		}
	}
}

func TestRefusedWriteIsNotASuccessfulPoll(t *testing.T) {
	e, _, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	store.writeErr = &promwrite.RefusedError{Status: 400, Body: "out of order sample"}
	*now = t0.Add(15 * time.Minute)
	e.poll(context.Background())
	if got := scrape(t, e); strings.Contains(got, "emporia_last_success_timestamp_seconds") {
		t.Errorf("a poll whose samples were refused counted as a success:\n%s", got)
	}
	if e.pushFailed {
		t.Error("a refusal set pushFailed, which means retry")
	}
	store.writeErr = nil
	*now = t0.Add(30 * time.Minute)
	e.poll(context.Background())
	if got := scrape(t, e); !strings.Contains(got, "emporia_last_success_timestamp_seconds") {
		t.Errorf("a clean poll after a refusal did not count:\n%s", got)
	}
}

func TestHourlyCost(t *testing.T) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 50, 0, 0, time.UTC)
	store.newest["emporia_hourly_cost"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	*now = time.Date(2026, 11, 5, 10, 3, 30, 0, time.UTC) // minutes reach 10:01, but the hour ended 3.5 min ago
	e.poll(context.Background())
	if len(api.usageAsks) != 0 {
		t.Fatalf("asked for an hour before it settled: %v", api.usageAsks)
	}
	*now = time.Date(2026, 11, 5, 10, 6, 30, 0, time.UTC)
	e.poll(context.Background())
	if len(api.usageAsks) != 2 || !api.usageAsks[0].Equal(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("want 09:00 asked in kWh and dollars, got %v", api.usageAsks)
	}
	end := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, num string
		v         float64
	}{
		{"emporia_hourly_cost", "1,2,3", 0.15}, {"emporia_hourly_cost", "Balance", 0.1},
		{"emporia_hourly_energy_kwh", "1,2,3", 0.6}, {"emporia_hourly_energy_kwh", "1", 0.12},
	} {
		s := find(store.writes, c.name, c.num)
		if len(s) != 1 || !s[0].T.Equal(end) || s[0].V != c.v {
			t.Errorf("%s{%s}: %v, want %v at %v", c.name, c.num, s, c.v, end)
		}
	}
	if l := labelsOf(store.writes, "emporia_hourly_cost", "1,2,3"); l["model"] != "VUE003" || l["channel"] != "Main" {
		t.Errorf("hourly mains should carry the same labels as its power: %v", l)
	}
	if s := find(store.writes, "emporia_hourly_energy_kwh", "2"); len(s) != 0 {
		t.Error("a null hour should push nothing")
	}
	n := len(api.usageAsks)
	*now = now.Add(15 * time.Minute)
	e.poll(context.Background())
	if len(api.usageAsks) != n {
		t.Error("asked for the same hour twice")
	}
}

// The real account: the circuit expansion shares its Vue's deviceGid.
func TestExpansionSharingTheVuesGid(t *testing.T) {
	ds := []emporia.Device{
		{Gid: 7, Model: "VUE003", Name: "Home", Channels: []emporia.Channel{{Num: "1,2,3"}}},
		{Gid: 7, Model: "WAT001", Name: "Home", Parent: 7, Channels: []emporia.Channel{{Num: "1", Name: "Oven"}, {Num: "2"}}},
	}
	done := make(chan []tree, 1)
	go func() { done <- buildTrees(ds) }()
	select {
	case ts := <-done:
		if len(ts) != 1 || len(ts[0].gids) != 1 || ts[0].mains == nil || ts[0].mains.model != "VUE003" || len(ts[0].circuits) != 2 {
			t.Fatalf("want one tree, one gid, Vue mains and two circuits: %+v", ts)
		}
	case <-time.After(time.Second):
		t.Fatal("buildTrees did not finish (parent loop)")
	}
}
