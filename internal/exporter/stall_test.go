package exporter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/promwrite"
)

// liveStore is a fakeStore that, like a real target, answers Newest and
// Oldest from what it has been sent. With landThenFail a write that errors
// has still landed, as when a request times out after the target took it.
type liveStore struct {
	*fakeStore
	landThenFail bool
}

func (s *liveStore) Write(_ context.Context, series []promwrite.Series) error {
	if s.writeErr != nil && !s.landThenFail {
		return s.writeErr
	}
	s.writes = append(s.writes, series)
	for _, x := range series {
		if x.Labels["channel_num"] != "1,2,3" {
			continue
		}
		name := x.Labels["__name__"]
		for _, p := range x.Samples {
			if name == "emporia_hourly_energy_kwh" {
				if o, ok := s.oldest[name]; !ok || p.T.Before(o) {
					s.oldest[name] = p.T
				}
			} else if p.T.After(s.newest[name]) {
				s.newest[name] = p.T
			}
		}
	}
	return s.writeErr
}

// stalled is two targets, both holding everything up to 09:00 and resumed
// by one poll, and the clock.
func stalled(t *testing.T) (*Exporter, *fakeAPI, *liveStore, *liveStore, *time.Time) {
	t.Helper()
	e0, api, _, now := setup()
	mk := func() *liveStore {
		return &liveStore{fakeStore: &fakeStore{
			newest: map[string]time.Time{"emporia_power_watts": h0, "emporia_hourly_cost": h0},
			oldest: map[string]time.Time{"emporia_hourly_energy_kwh": h0}}}
	}
	up, bad := mk(), mk()
	e := New(Config{}, api, []Target{{"up", up}, {"bad", bad}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = e0.now
	e.sleep = e0.sleep
	e.poll(context.Background())
	return e, api, up, bad, now
}

// minutes lists the 1,2,3 power sample times a store was sent, in order.
func minutes(s *liveStore) []time.Time {
	var out []time.Time
	for _, p := range find(s.writes, "emporia_power_watts", "1,2,3") {
		out = append(out, p.T)
	}
	return out
}

// A target that stops taking writes after it resumed must not pin the fetch
// window: the healthy target and /metrics keep up. Both ways it can fail.
func TestFailingTargetDoesNotStopTheOthers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(s *liveStore)
		calls int // most chart calls a poll may make: one fetch is 3, a second only for a target that answers
	}{
		{"unreachable", func(s *liveStore) { s.newestErr = errors.New("connection refused"); s.writeErr = s.newestErr }, 3},
		{"writes fail", func(s *liveStore) { s.writeErr = errors.New("connection refused") }, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, api, up, bad, now := stalled(t)
			*now = now.Add(15 * time.Minute)
			e.poll(context.Background()) // both take this one
			tc.fail(bad)
			for i := range 96 + 8 { // a day, and more
				*now = now.Add(15 * time.Minute)
				before := len(api.charts)
				e.poll(context.Background())
				if got := len(api.charts) - before; got > tc.calls {
					t.Fatalf("poll %d made %d chart calls, want at most %d", i, got, tc.calls)
				}
				ms := minutes(up)
				if lag := now.Sub(ms[len(ms)-1]); lag > 15*time.Minute {
					t.Fatalf("poll %d: healthy target's newest minute is %v behind", i, lag)
				}
				e.mu.Lock()
				lag := now.Sub(e.newest)
				e.mu.Unlock()
				if lag > 15*time.Minute {
					t.Fatalf("poll %d: /metrics newest minute is %v behind", i, lag)
				}
			}
			if hs := find(up.writes, "emporia_hourly_cost", "1,2,3"); len(hs) < 24 {
				t.Errorf("healthy target got %d hours in a day", len(hs))
			}
		})
	}
}

// When the failed target is back it gets every minute it lacks, in order,
// 12h a poll, with no gap and nothing twice; the healthy target gets the new
// minutes each poll meanwhile.
func TestFailedTargetCatchesUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(s *liveStore)
		heal func(s *liveStore)
	}{
		{"unreachable", func(s *liveStore) { s.newestErr = errors.New("refused"); s.writeErr = s.newestErr },
			func(s *liveStore) { s.newestErr, s.writeErr = nil, nil }},
		{"writes fail", func(s *liveStore) { s.writeErr = errors.New("refused") },
			func(s *liveStore) { s.writeErr = nil }},
		{"writes land then error", func(s *liveStore) { s.writeErr, s.landThenFail = errors.New("timeout"), true },
			func(s *liveStore) { s.writeErr, s.landThenFail = nil, false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, up, bad, now := stalled(t)
			tc.fail(bad)
			for range 96 + 4 { // 25 hours down
				*now = now.Add(15 * time.Minute)
				e.poll(context.Background())
			}
			tc.heal(bad)
			held := len(bad.writes)
			for i := range 6 {
				*now = now.Add(15 * time.Minute)
				ups := len(minutes(up))
				e.poll(context.Background())
				if len(minutes(up)) <= ups {
					t.Fatalf("recovery poll %d: healthy target got nothing new", i)
				}
			}
			ms := minutes(bad)
			for i := 1; i < len(ms); i++ {
				if d := ms[i].Sub(ms[i-1]); d != time.Minute {
					t.Fatalf("minute %d is %v after the one before (%v then %v)", i, d, ms[i-1], ms[i])
				}
			}
			if lag := now.Sub(ms[len(ms)-1]); lag > 15*time.Minute {
				t.Errorf("bad is still %v behind after six polls", lag)
			}
			if !ms[0].Equal(h0.Add(time.Minute)) {
				t.Errorf("first minute %v, want %v", ms[0], h0.Add(time.Minute))
			}
			for _, w := range bad.writes[held:] {
				if n := len(find([][]promwrite.Series{w}, "emporia_power_watts", "1,2,3")); n > 12*60 {
					t.Errorf("one write of %d minutes, want at most 12h", n)
				}
			}
		})
	}
}

// resume run again mid-life must not rewind what /metrics serves for a
// target, nor reset its error count, nor restart a finished history walk.
func TestReResumeKeepsWhatIsServedAndTheHistoryWalk(t *testing.T) {
	e, api, _, bad, now := stalled(t)
	api.hourly = historyFrom(h0.Add(-30 * time.Hour))
	for range 3 { // the walk fetches the 30h, then finds a week empty
		*now = now.Add(15 * time.Minute)
		e.poll(context.Background())
	}
	s := e.sinks[1]
	if !s.historyDone {
		t.Fatal("setup: the history walk should be done")
	}
	walks, oldest, newest := len(api.hourCharts), s.oldest, s.newest
	bad.writeErr = errors.New("refused")
	*now = now.Add(15 * time.Minute)
	e.poll(context.Background())
	if s.resumed || s.errors != 1 {
		t.Fatalf("a failed write should un-resume the target and count it: resumed %v, errors %d", s.resumed, s.errors)
	}
	bad.writeErr = nil
	bad.newest["emporia_power_watts"] = newest.Add(-time.Hour) // a target that holds less than was sent
	held := len(bad.writes)
	*now = now.Add(15 * time.Minute)
	e.poll(context.Background())
	if !s.resumed || !s.historyDone || len(api.hourCharts) != walks || !s.oldest.Equal(oldest) {
		t.Errorf("history walk restarted: resumed %v done %v walks %d->%d oldest %v->%v",
			s.resumed, s.historyDone, walks, len(api.hourCharts), oldest, s.oldest)
	}
	if s.newest.Before(newest) || s.errors != 1 {
		t.Errorf("newest went back %v->%v or errors reset to %d", newest, s.newest, s.errors)
	}
	for _, p := range historyOf(bad.writes[held:], "emporia_hourly_energy_kwh", "1,2,3", oldest) {
		t.Errorf("history hour %v pushed again", p.T)
	}
}

// A target many hours behind on hours must not hold back the newest finished
// hour from the targets that are level: the level one and /metrics get it in
// the poll that finds it, and the laggard gets its own hours, oldest first,
// with what is left of the poll.
func TestHourCatchUpDoesNotHoldBackTheNewestHour(t *testing.T) {
	e0, api, _, now := setup()
	mk := func(costAt time.Time) *liveStore {
		return &liveStore{fakeStore: &fakeStore{
			newest: map[string]time.Time{"emporia_power_watts": h0, "emporia_hourly_cost": costAt},
			oldest: map[string]time.Time{"emporia_hourly_energy_kwh": costAt}}}
	}
	up, late := mk(h0), mk(h0.Add(-30*time.Hour))
	e := New(Config{}, api, []Target{{"up", up}, {"late", late}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = e0.now
	e.sleep = e0.sleep
	*now = h0.Add(80 * time.Minute) // 10:20: the 09:00 hour has ended and settled
	e.poll(context.Background())

	got := find(up.writes, "emporia_hourly_cost", "1,2,3")
	if len(got) != 1 || !got[0].T.Equal(h0.Add(time.Hour)) {
		t.Fatalf("level target should get the newest finished hour, got %v", got)
	}
	if s := scrape(t, e); !strings.Contains(s, `emporia_hourly_cost{`) {
		t.Fatalf("/metrics has no hourly cost:\n%s", s)
	}
	lag := find(late.writes, "emporia_hourly_cost", "1,2,3")
	for i, p := range lag {
		if want := h0.Add(time.Duration(i-29) * time.Hour); !p.T.Equal(want) {
			t.Fatalf("laggard hour %d at %v, want %v", i, p.T, want)
		}
	}
	if len(lag) != e.cfg.MaxHours-1 {
		t.Errorf("laggard got %d hours, want the %d the level target left", len(lag), e.cfg.MaxHours-1)
	}
}
