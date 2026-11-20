package exporter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
	"github.com/hughobrien/emporiad/internal/promwrite"
)

// h0 is the oldest hourly sample the target holds in these tests.
var h0 = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// Each hour: mains 0.6 kWh ($0.15), Oven 0.12 ($0.03), circuit 2 0.06
// ($0.015), for hours starting at or after from.
func historyFrom(from time.Time) func(num, unit string, hour time.Time) *float64 {
	return func(num, unit string, hour time.Time) *float64 {
		if hour.Before(from) {
			return nil
		}
		kwh := map[string]float64{"1,2,3": 0.6, "1": 0.12, "2": 0.06}[num]
		if unit == emporia.Dollars {
			return f(kwh / 4)
		}
		return f(kwh)
	}
}

// historySetup is setup with a target that has minutes and hours up to now
// and hourly samples back to h0, so a poll's only hourly writes are history.
func historySetup() (*Exporter, *fakeAPI, *fakeStore, *time.Time) {
	e, api, store, now := setup()
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 4, 0, 0, time.UTC)
	store.newest["emporia_hourly_cost"] = h0
	store.oldest["emporia_hourly_cost"] = h0
	return e, api, store, now
}

// historyOf returns the pushed hourly samples older than before.
func historyOf(ws [][]promwrite.Series, name, num string, before time.Time) []promwrite.Sample {
	var out []promwrite.Sample
	for _, p := range find(ws, name, num) {
		if p.T.Before(before) {
			out = append(out, p)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHistoryWalksBackAWeekAPoll(t *testing.T) {
	e, api, store, now := historySetup()
	api.hourly = historyFrom(h0.Add(-200 * time.Hour))
	ctx := context.Background()
	e.poll(ctx)
	if len(api.hourCharts) != 6 {
		t.Fatalf("want mains and both circuits in kWh and dollars, got %v", api.hourCharts)
	}
	first := hourCall{1, "1,2,3", emporia.KWh, h0.Add(-169 * time.Hour), h0.Add(-time.Hour)}
	if api.hourCharts[0] != first {
		t.Fatalf("first call %v, want mains kWh %v", api.hourCharts[0], first)
	}
	s := historyOf(store.writes, "emporia_hourly_energy_kwh", "1,2,3", h0)
	if len(s) != 168 || !s[0].T.Equal(h0.Add(-168*time.Hour)) || !s[167].T.Equal(h0.Add(-time.Hour)) || s[0].V != 0.6 {
		t.Fatalf("want 168 hours ending at h0-1h, got %d from %v", len(s), s)
	}
	if l := labelsOf(store.writes, "emporia_hourly_cost", "1"); l["channel"] != "Oven" || l["model"] != "WAT001" {
		t.Errorf("history labels %v", l)
	}

	*now = now.Add(15 * time.Minute)
	e.poll(ctx)
	if c := api.hourCharts[len(api.hourCharts)-1]; !c.end.Equal(h0.Add(-169 * time.Hour)) {
		t.Fatalf("second week ends at %v, want where the first began", c.end)
	}
	if s := historyOf(store.writes, "emporia_hourly_cost", "1,2,3", h0); len(s) != 168+31 || !s[168].T.Equal(h0.Add(-199*time.Hour)) {
		t.Fatalf("want the 31 hours from the history's start added, got %d", len(s)-168)
	}

	n, w := len(api.hourCharts), len(store.writes)
	*now = now.Add(15 * time.Minute)
	e.poll(ctx)
	if len(api.hourCharts) != n+1 || historyOf(store.writes[w:], "emporia_hourly_cost", "1,2,3", h0) != nil {
		t.Fatalf("an empty week should cost one mains call and push nothing: %v", api.hourCharts[n:])
	}
	*now = now.Add(15 * time.Minute)
	e.poll(ctx)
	if len(api.hourCharts) != n+1 {
		t.Fatalf("asked again after the history ended: %v", api.hourCharts[n+1:])
	}

	// A restart after the walk ended: one mains call finds nothing older.
	store.oldest["emporia_hourly_cost"] = h0.Add(-199 * time.Hour)
	e2 := New(Config{}, api, []Target{{"vm", store}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e2.now = func() time.Time { return *now }
	e2.sleep = func(context.Context, time.Duration) error { return nil }
	n, w = len(api.hourCharts), len(store.writes)
	e2.poll(ctx)
	if len(api.hourCharts) != n+1 || historyOf(store.writes[w:], "emporia_hourly_cost", "1,2,3", h0) != nil {
		t.Fatalf("restart: want one mains call and no history, got %v", api.hourCharts[n:])
	}
}

func TestHistoryBalance(t *testing.T) {
	e, api, store, _ := historySetup()
	gap := h0.Add(-10 * time.Hour) // circuit 2 has nothing for the hour starting here
	full := historyFrom(h0.Add(-1000 * time.Hour))
	api.hourly = func(num, unit string, hour time.Time) *float64 {
		if num == "2" && hour.Equal(gap) {
			return nil
		}
		return full(num, unit, hour)
	}
	e.poll(context.Background())
	for _, c := range []struct {
		name string
		v    float64
	}{{"emporia_hourly_energy_kwh", 0.42}, {"emporia_hourly_cost", 0.105}} {
		s := historyOf(store.writes, c.name, "Balance", h0)
		if len(s) != 167 {
			t.Fatalf("%s Balance: %d hours, want 167 (one circuit missing an hour)", c.name, len(s))
		}
		for _, p := range s {
			if p.T.Equal(gap.Add(time.Hour)) {
				t.Errorf("%s: Balance for the hour circuit 2 is missing", c.name)
			}
			if !near(p.V, c.v) {
				t.Fatalf("%s Balance %v, want %v", c.name, p.V, c.v)
			}
		}
		if l := labelsOf(store.writes, c.name, "Balance"); l["channel"] != "Balance" || l["model"] != "VUE003" {
			t.Errorf("Balance labels %v", l)
		}
	}
}

func TestHistoryEmptyTargetStartsWhereHoursStart(t *testing.T) {
	e, api, store, _ := setup()
	api.hourly = historyFrom(t0.Add(-1000 * time.Hour))
	e.poll(context.Background())
	forward := map[time.Time]bool{}
	var hist []promwrite.Sample
	firstHour := t0.Add(-7 * 24 * time.Hour).Truncate(time.Hour)
	for _, p := range find(store.writes, "emporia_hourly_cost", "1,2,3") {
		if p.T.After(firstHour) {
			if forward[p.T] {
				t.Fatalf("hour %v pushed twice", p.T)
			}
			forward[p.T] = true
		} else {
			hist = append(hist, p)
		}
	}
	if len(forward) == 0 || len(hist) != 168 {
		t.Fatalf("want forward hours and a week of history, got %d and %d", len(forward), len(hist))
	}
	if !hist[len(hist)-1].T.Equal(firstHour) || !forward[firstHour.Add(time.Hour)] {
		t.Errorf("history ends at %v, want %v, right before the first forward hour", hist[len(hist)-1].T, firstHour)
	}
}

func TestHistoryErrorRetriesTheWeek(t *testing.T) {
	e, api, store, now := historySetup()
	api.hourly = historyFrom(h0.Add(-1000 * time.Hour))
	api.hourErr["2"] = errors.New("HTTP 500")
	e.poll(context.Background())
	if s := historyOf(store.writes, "emporia_hourly_cost", "1,2,3", h0); len(s) != 0 {
		t.Fatalf("a failed call should push nothing of the week, got %d samples", len(s))
	}
	if got := scrape(t, e); !strings.Contains(got, `emporia_poll_errors_total{stage="usage"} 1`) {
		t.Errorf("want the error counted:\n%s", got)
	}
	failed := api.hourCharts[0]
	delete(api.hourErr, "2")
	*now = now.Add(15 * time.Minute)
	n := len(api.hourCharts)
	e.poll(context.Background())
	if c := api.hourCharts[n]; c.start != failed.start || c.end != failed.end {
		t.Fatalf("retry asked %v, want the same week %v", c, failed)
	}
	if s := historyOf(store.writes, "emporia_hourly_cost", "1,2,3", h0); len(s) != 168 {
		t.Fatalf("retry pushed %d hours, want 168", len(s))
	}
}

func TestHistoryRefusedStops(t *testing.T) {
	e, api, store, now := historySetup()
	api.hourly = historyFrom(h0.Add(-1000 * time.Hour))
	store.writeErr = &promwrite.RefusedError{Status: 400, Body: "too old"}
	e.poll(context.Background())
	n := len(api.hourCharts)
	if n == 0 {
		t.Fatal("no history asked")
	}
	*now = now.Add(15 * time.Minute)
	e.poll(context.Background())
	if len(api.hourCharts) != n {
		t.Fatalf("kept walking a target that refuses old samples: %v", api.hourCharts[n:])
	}
}

func TestHistorySpacedAndLast(t *testing.T) {
	e, api, _, _ := setup() // an empty target: minutes, hours and history all in one poll
	api.hourly = historyFrom(t0.Add(-1000 * time.Hour))
	e.sleep = func(_ context.Context, d time.Duration) error {
		api.calls = append(api.calls, "sleep "+d.String())
		return nil
	}
	e.poll(context.Background())
	first := -1
	for i, c := range api.calls {
		switch c {
		case "hour":
			if first < 0 {
				first = i
			}
			if i == 0 || api.calls[i-1] != "sleep 20s" {
				t.Fatalf("call %d: history call not 20s after the last: %v", i, api.calls[max(0, i-3):i+1])
			}
		case "chart", "usage":
			if first >= 0 {
				t.Fatalf("a fresh %s call after the history began", c)
			}
		}
	}
	if first < 0 || !strings.Contains(strings.Join(api.calls[:first], " "), "chart") || !strings.Contains(strings.Join(api.calls[:first], " "), "usage") {
		t.Fatalf("want fresh minutes and hours, then history: %v", api.calls)
	}
}

func TestHistoryTwoTargets(t *testing.T) {
	_, api, _, now := setup()
	api.hourly = historyFrom(h0.Add(-5000 * time.Hour))
	mk := func(oldest time.Time) *fakeStore {
		return &fakeStore{
			newest: map[string]time.Time{"emporia_power_watts": time.Date(2026, 11, 5, 9, 4, 0, 0, time.UTC), "emporia_hourly_cost": h0},
			oldest: map[string]time.Time{"emporia_hourly_cost": oldest},
		}
	}
	a, b := mk(h0), mk(h0.Add(-168*time.Hour))
	e := New(Config{}, api, []Target{{"a", a}, {"b", b}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return *now }
	e.sleep = func(context.Context, time.Duration) error { return nil }
	e.poll(context.Background())
	if s := historyOf(a.writes, "emporia_hourly_cost", "1,2,3", h0); len(s) != 168 {
		t.Fatalf("a: %d hours, want the week before h0", len(s))
	}
	if len(b.writes) != 0 {
		t.Fatalf("b already has that week: %d writes", len(b.writes))
	}
	*now = now.Add(15 * time.Minute)
	e.poll(context.Background())
	for name, s := range map[string]*fakeStore{"a": a, "b": b} {
		got := historyOf(s.writes, "emporia_hourly_cost", "1,2,3", h0)
		if want := map[string]int{"a": 336, "b": 168}[name]; len(got) != want {
			t.Errorf("%s: %d hours, want %d", name, len(got), want)
		}
		seen := map[time.Time]bool{}
		for _, p := range got {
			if seen[p.T] {
				t.Errorf("%s: %v pushed twice", name, p.T)
			}
			seen[p.T] = true
		}
	}
	if c := api.hourCharts[len(api.hourCharts)-1]; !c.end.Equal(h0.Add(-169 * time.Hour)) {
		t.Errorf("second poll's week ends %v, want h0-169h", c.end)
	}
}

// emporiad was off for longer than resume looks back: the hours between the
// target's old newest and where the forward poll starts are walked first,
// then the walk jumps to the target's oldest and carries on from there.
func TestHistoryFillsALongOutage(t *testing.T) {
	e, api, store, _ := setup()
	api.hourly = historyFrom(t0.Add(-5000 * time.Hour))
	first := t0.Add(-7 * 24 * time.Hour).Truncate(time.Hour) // forward hours start here
	oldNewest := first.Add(-200 * time.Hour)
	oldest := oldNewest.Add(-1000 * time.Hour)
	store.stale = map[string]time.Time{"emporia_hourly_cost": oldNewest}
	store.oldest["emporia_hourly_cost"] = oldest
	ctx := context.Background()
	for range 3 {
		e.poll(ctx)
	}
	got := map[time.Time]bool{}
	for _, p := range find(store.writes, "emporia_hourly_cost", "1,2,3") {
		if got[p.T] {
			t.Fatalf("%v pushed twice", p.T)
		}
		got[p.T] = true
	}
	for h := oldNewest.Add(time.Hour); !h.After(first); h = h.Add(time.Hour) {
		if !got[h] {
			t.Fatalf("hour ending %v in the outage was not filled", h)
		}
	}
	for h := oldest; !h.After(oldNewest); h = h.Add(time.Hour) {
		if got[h] {
			t.Fatalf("hour ending %v was already held but pushed again", h)
		}
	}
	// The third poll walks on from the target's oldest.
	if c := api.hourCharts[len(api.hourCharts)-1]; !c.end.Equal(oldest.Add(-time.Hour)) {
		t.Errorf("after the outage the walk asked up to %v, want %v", c.end, oldest.Add(-time.Hour))
	}
}

// A target that is down gets no history fetched for it: the week would be
// fetched again every poll until it is back.
func TestHistoryWaitsForADownTarget(t *testing.T) {
	e, api, store, _ := historySetup()
	api.hourly = historyFrom(h0.Add(-1000 * time.Hour))
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC) // so this poll writes minutes
	store.writeErr = errors.New("connection refused")
	e.poll(context.Background())
	if len(api.hourCharts) != 0 {
		t.Fatalf("fetched history for a target that is down: %v", api.hourCharts)
	}
}
