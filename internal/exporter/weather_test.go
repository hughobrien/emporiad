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

	"github.com/hughobrien/emporiad/internal/promwrite"
	"github.com/hughobrien/emporiad/internal/weather"
)

// fakeWeather answers every hour after from up to to, valued by its hour of
// day, as Open-Meteo would.
type fakeWeather struct {
	asks [][2]time.Time
	err  error
}

func (w *fakeWeather) Readings(_ context.Context, from, to time.Time) ([]weather.Reading, error) {
	w.asks = append(w.asks, [2]time.Time{from, to})
	if w.err != nil {
		return nil, w.err
	}
	var out []weather.Reading
	for t := from.Truncate(time.Hour).Add(time.Hour); !t.After(to); t = t.Add(time.Hour) {
		out = append(out, weather.Reading{T: t, Celsius: float64(t.Hour())})
	}
	return out, nil
}

func temps(ws [][]promwrite.Series) []promwrite.Sample {
	var out []promwrite.Sample
	for _, w := range ws {
		for _, s := range w {
			if s.Labels["__name__"] == weatherName {
				out = append(out, s.Samples...)
			}
		}
	}
	return out
}

func withWeather() (*Exporter, *fakeAPI, *fakeStore, *time.Time, *fakeWeather) {
	e, api, store, now := setup()
	w := &fakeWeather{}
	e.cfg.Weather = w
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 4, 0, 0, time.UTC)
	store.newest["emporia_hourly_cost"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	return e, api, store, now, w
}

func TestWeatherBackfillsAYearThenSendsOnlyNewHours(t *testing.T) {
	e, _, store, now, w := withWeather()
	ctx := context.Background()
	e.poll(ctx) // 09:04:30, the target holds no temperature
	start := time.Date(2025, 11, 5, 9, 0, 0, 0, time.UTC)
	if len(w.asks) != 1 || !w.asks[0][0].Equal(start) {
		t.Fatalf("asks %v, want one from a year back, %v", w.asks, start)
	}
	got := temps(store.writes)
	if len(got) != 365*24 {
		t.Fatalf("%d hours sent, want a year's", len(got))
	}
	if !got[0].T.Equal(start.Add(time.Hour)) || !got[len(got)-1].T.Equal(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("sent %v to %v", got[0].T, got[len(got)-1].T)
	}
	for _, s := range store.writes[len(store.writes)-1] {
		if s.Labels["__name__"] == weatherName {
			if l := s.Labels; l["job"] != "emporiad" || l["source"] != "open-meteo" || len(l) != 3 {
				t.Errorf("labels %v", l)
			}
		}
	}
	ms := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC).UnixMilli()
	if m := scrapeAll(t, e); !strings.Contains(m, fmt.Sprintf(`weather_temperature_celsius{pushed_vm="true",source="open-meteo"} 9 %d`, ms)) {
		t.Errorf("/metrics lacks the newest temperature:\n%s", m)
	}

	*now = t0.Add(15 * time.Minute) // 09:19:30: no new hour
	e.poll(ctx)
	if len(w.asks) != 1 {
		t.Errorf("asked again with no new hour: %v", w.asks)
	}
	*now = t0.Add(time.Hour) // 10:04:30
	e.poll(ctx)
	if len(w.asks) != 2 || !w.asks[1][0].Equal(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("asks %v, want the second from 09:00", w.asks)
	}
	got = temps(store.writes)
	if n := len(got); n != 365*24+1 || !got[n-1].T.Equal(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("after the next hour: %d sent, last %v", n, got[n-1].T)
	}
}

func TestWeatherResumesFromTheTarget(t *testing.T) {
	e, _, store, _, w := withWeather()
	held := time.Date(2026, 11, 5, 6, 0, 0, 0, time.UTC)
	store.newest[weatherName] = held
	e.poll(context.Background())
	want := `weather_temperature_celsius{job="emporiad",instance="",source="open-meteo"}`
	found := false
	for _, s := range store.selectors {
		found = found || s == want
	}
	if !found {
		t.Errorf("selectors %v, want %s", store.selectors, want)
	}
	if len(w.asks) != 1 || !w.asks[0][0].Equal(held) {
		t.Fatalf("asks %v, want from %v", w.asks, held)
	}
	for _, p := range temps(store.writes) {
		if !p.T.After(held) {
			t.Errorf("resent %v, which the target holds", p.T)
		}
	}
	if n := len(temps(store.writes)); n != 3 {
		t.Errorf("%d hours sent, want 07:00 to 09:00", n)
	}
}

func TestWeatherErrorsAreTheirOwnStage(t *testing.T) {
	e, _, store, now, w := withWeather()
	w.err = errors.New("GET /v1/forecast: HTTP 502: Bad Gateway")
	ctx := context.Background()
	e.poll(ctx)
	*now = t0.Add(15 * time.Minute)
	e.poll(ctx)
	if len(temps(store.writes)) != 0 {
		t.Error("sent temperatures after an error")
	}
	if len(find(store.writes, "emporia_power_watts", "1,2,3")) == 0 {
		t.Error("a weather error stopped the minutes")
	}
	m := scrape(t, e)
	for _, want := range []string{`emporia_poll_errors_total{stage="weather"} 2`, "emporia_last_success_timestamp_seconds"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %s:\n%s", want, m)
		}
	}
	w.err = nil
	*now = t0.Add(30 * time.Minute)
	e.poll(ctx)
	if len(temps(store.writes)) != 365*24 {
		t.Errorf("after recovery: %d hours, want the year", len(temps(store.writes)))
	}
}

func TestNoWeatherMeansNoWeatherQueries(t *testing.T) {
	e, _, store, _ := setup()
	e.poll(context.Background())
	for _, s := range store.selectors {
		if strings.Contains(s, weatherName) {
			t.Errorf("asked %s with weather off", s)
		}
	}
	if m := scrapeAll(t, e); strings.Contains(m, "weather_temperature_celsius{") {
		t.Errorf("served a temperature with weather off:\n%s", m)
	}
}

// scrapeAll is the whole of /metrics: scrape keeps only the emporia lines.
func scrapeAll(t *testing.T, e *Exporter) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestWeatherSendsEachTargetOnlyWhatItLacks(t *testing.T) {
	_, api, _, now := setup()
	behind := &fakeStore{newest: map[string]time.Time{}, oldest: map[string]time.Time{}}
	level := &fakeStore{newest: map[string]time.Time{}, oldest: map[string]time.Time{}}
	held := time.Date(2026, 11, 5, 6, 0, 0, 0, time.UTC)
	level.newest[weatherName] = held
	w := &fakeWeather{}
	e := New(Config{Weather: w}, api, []Target{{"behind", behind}, {"level", level}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return *now }
	e.sleep = func(context.Context, time.Duration) error { return nil }
	e.poll(context.Background())
	if len(w.asks) != 1 {
		t.Fatalf("asks %v, want one fetch for both", w.asks)
	}
	if n := len(temps(behind.writes)); n != 365*24 {
		t.Errorf("behind got %d hours, want the year", n)
	}
	got := temps(level.writes)
	if len(got) != 3 || !got[0].T.After(held) {
		t.Errorf("level got %d hours from %v, want only the 3 after %v", len(got), got, held)
	}
}
