package exporter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/promwrite"
	"github.com/hughobrien/emporiad/internal/weather"
)

// fakeStation reads every five minutes from since on, valued by the minute,
// as an Ambient station would.
type fakeStation struct {
	since time.Time
	asks  [][2]time.Time
	err   error
}

func (w *fakeStation) Readings(_ context.Context, from, to time.Time) ([]weather.Reading, error) {
	w.asks = append(w.asks, [2]time.Time{from, to})
	if w.err != nil {
		return nil, w.err
	}
	var out []weather.Reading
	for t := from.Truncate(5 * time.Minute).Add(5 * time.Minute); !t.After(to); t = t.Add(5 * time.Minute) {
		if !t.Before(w.since) {
			out = append(out, weather.Reading{T: t, Celsius: float64(t.Minute())})
		}
	}
	return out, nil
}

const stationSel = `weather_temperature_celsius{job="emporiad",instance="",source="ambient"}`

func bySource(ws [][]promwrite.Series, source string) []promwrite.Sample {
	var out []promwrite.Sample
	for _, w := range ws {
		for _, s := range w {
			if s.Labels["__name__"] == weatherName && s.Labels["source"] == source {
				out = append(out, s.Samples...)
			}
		}
	}
	return out
}

func withStation() (*Exporter, *fakeStore, *time.Time, *fakeStation) {
	e, _, store, now := setup()
	w := &fakeStation{since: t0.UTC().Add(-20 * 24 * time.Hour).Truncate(5 * time.Minute)}
	e.cfg.Station = w
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 4, 0, 0, time.UTC)
	store.newest["emporia_hourly_cost"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	return e, store, now, w
}

func TestStationBackfillsAWeekAPollAcrossEmptyMonths(t *testing.T) {
	e, store, now, w := withStation()
	ctx := context.Background()
	start := t0.UTC().Add(-180 * 24 * time.Hour).Truncate(time.Hour)
	for i := range 30 {
		*now = t0.Add(time.Duration(i) * 15 * time.Minute)
		e.poll(ctx)
	}
	if !w.asks[0][0].Equal(start) {
		t.Fatalf("first ask %v, want from 180 days back, %v", w.asks[0], start)
	}
	for i, a := range w.asks {
		if a[1].Sub(a[0]) > 7*24*time.Hour {
			t.Errorf("ask %d %v: more than a week", i, a)
		}
		if i > 0 && a[0].Before(w.asks[i-1][0]) {
			t.Errorf("ask %d %v went back from %v", i, a, w.asks[i-1])
		}
	}
	// 160 empty days take 23 polls, then the 20 days with readings 3 more.
	if n := len(w.asks); n != 30 {
		t.Errorf("%d asks in 30 polls, want one each", n)
	}
	got := bySource(store.writes, "ambient")
	if len(got) == 0 {
		t.Fatal("no station readings sent")
	}
	if !got[0].T.Equal(w.since) {
		t.Errorf("first reading %v, want the station's first, %v", got[0].T, w.since)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].T.After(got[i-1].T) {
			t.Fatalf("sent %v after %v: out of order or twice", got[i].T, got[i-1].T)
		}
	}
	if last := got[len(got)-1].T; now.Sub(last) > 5*time.Minute {
		t.Errorf("newest sent %v, want caught up to %v", last, *now)
	}
	if bySource(store.writes, "open-meteo") != nil {
		t.Error("sent Open-Meteo without it configured")
	}
}

func TestStationResumesFromItsOwnSeries(t *testing.T) {
	e, store, _, w := withStation()
	e.cfg.Weather = &fakeWeather{}
	held := t0.UTC().Add(-time.Hour).Truncate(5 * time.Minute)
	store.newest[stationSel] = held
	store.newest[`weather_temperature_celsius{job="emporiad",instance="",source="open-meteo"}`] = time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	e.poll(context.Background())
	if len(w.asks) != 1 || !w.asks[0][0].Equal(held) {
		t.Fatalf("asks %v, want one from %v", w.asks, held)
	}
	got := bySource(store.writes, "ambient")
	if len(got) != 12 || !got[0].T.After(held) {
		t.Errorf("sent %d from %v, want the 12 readings after %v", len(got), got, held)
	}
	if n := len(bySource(store.writes, "open-meteo")); n != 1 {
		t.Errorf("Open-Meteo sent %d hours, want only 09:00: it resumes from its own series", n)
	}
	for _, sel := range store.selectors {
		if strings.HasPrefix(sel, weatherName) && !strings.Contains(sel, `source="`) {
			t.Errorf("asked %s, which mixes the sources", sel)
		}
	}
}

func TestStationAfterAnOutagePastNewestsReachStartsAnHourOn(t *testing.T) {
	e, store, _, w := withStation()
	onHour := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) // 65 days back: Newest (30d) cannot see it
	store.stale = map[string]time.Time{stationSel: onHour}
	e.poll(context.Background())
	if len(w.asks) != 1 || !w.asks[0][0].Equal(onHour.Add(time.Hour)) {
		t.Fatalf("asks %v, want from %v, past anything the target may hold", w.asks, onHour.Add(time.Hour))
	}
}

func TestStationFollowsLiveReadings(t *testing.T) {
	e, store, now, w := withStation()
	store.newest[stationSel] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	e.poll(ctx) // 09:04:30
	*now = t0.Add(15 * time.Minute)
	e.poll(ctx) // 09:19:30
	*now = t0.Add(30 * time.Minute)
	e.poll(ctx) // 09:34:30
	got := bySource(store.writes, "ambient")
	var ts []string
	for _, p := range got {
		ts = append(ts, p.T.Format("15:04"))
	}
	if want := "09:05 09:10 09:15 09:20 09:25 09:30"; strings.Join(ts, " ") != want {
		t.Errorf("sent %v, want %s", ts, want)
	}
	if len(w.asks) != 3 || !w.asks[2][0].Equal(time.Date(2026, 11, 5, 9, 15, 0, 0, time.UTC)) {
		t.Errorf("asks %v, want each from the newest sent", w.asks)
	}
	ms := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC).UnixMilli()
	if m := scrapeAll(t, e); !strings.Contains(m, fmt.Sprintf(`weather_temperature_celsius{pushed_vm="true",source="ambient"} 30 %d`, ms)) {
		t.Errorf("/metrics lacks the newest station reading:\n%s", m)
	}
}

func TestStationAndOpenMeteoBothOnMetrics(t *testing.T) {
	e, store, _, _ := withStation()
	e.cfg.Weather = &fakeWeather{}
	store.newest[stationSel] = time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC)
	e.poll(context.Background())
	m := scrapeAll(t, e)
	for _, want := range []string{`source="ambient"`, `source="open-meteo"`} {
		if !strings.Contains(m, "weather_temperature_celsius{pushed_vm=\"true\","+want) {
			t.Errorf("/metrics lacks %s:\n%s", want, m)
		}
	}
}

func TestStationErrorLeavesOpenMeteoAlone(t *testing.T) {
	e, store, now, w := withStation()
	e.cfg.Weather = &fakeWeather{}
	w.err = errors.New("GET /device-data: HTTP 500: Request failed")
	ctx := context.Background()
	e.poll(ctx)
	if len(bySource(store.writes, "ambient")) != 0 {
		t.Error("sent station readings after an error")
	}
	if len(bySource(store.writes, "open-meteo")) != 365*24 {
		t.Error("a station error stopped Open-Meteo")
	}
	if m := scrape(t, e); !strings.Contains(m, `emporia_poll_errors_total{stage="weather"} 1`) {
		t.Errorf("station error not counted:\n%s", m)
	}
	w.err = nil
	*now = t0.Add(15 * time.Minute)
	e.poll(ctx)
	if len(w.asks) != 2 || !w.asks[1][0].Equal(w.asks[0][0]) {
		t.Errorf("asks %v, want the failed window asked again", w.asks)
	}
}
