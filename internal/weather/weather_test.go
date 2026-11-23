package weather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 3, 20, 10, 40, 0, 0, time.UTC)

// series answers every hour from start for n hours, v(i) each (nil skips).
func series(start time.Time, n int, v func(i int) *float64) map[string]any {
	var ts []string
	var vs []*float64
	for i := range n {
		ts = append(ts, start.Add(time.Duration(i)*time.Hour).Format("2006-01-02T15:04"))
		vs = append(vs, v(i))
	}
	return map[string]any{"hourly": map[string]any{"time": ts, "temperature_2m": vs}}
}

func f(x float64) *float64 { return &x }

type fake struct {
	srv      *httptest.Server
	asked    map[string][]url.Values
	forecast map[string]any
	archive  map[string]any
	status   int
}

func newFake(t *testing.T) *fake {
	k := &fake{asked: map[string][]url.Values{}, status: http.StatusOK}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.asked[r.URL.Path] = append(k.asked[r.URL.Path], r.URL.Query())
		body := k.forecast
		if r.URL.Path == "/v1/archive" {
			body = k.archive
		}
		if k.status != http.StatusOK {
			body = map[string]any{"error": true, "reason": "Parameter 'start_date' is out of range"}
		}
		w.WriteHeader(k.status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *fake) client() *Client {
	return &Client{Latitude: 12.5, Longitude: -3.25, Forecast: k.srv.URL, Archive: k.srv.URL}
}

func TestRecentHoursComeFromTheForecastOnly(t *testing.T) {
	k := newFake(t)
	day := time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC) // past_days=7 starts here
	k.forecast = series(day, 8*24, func(i int) *float64 {
		if i == 7*24+9 {
			return nil // not available yet
		}
		return f(float64(i))
	})
	from := time.Date(2026, 3, 20, 6, 0, 0, 0, time.UTC)
	hs, err := k.client().Readings(context.Background(), from, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.asked["/v1/archive"]) != 0 {
		t.Errorf("a recent span should not ask the archive: %v", k.asked)
	}
	q := k.asked["/v1/forecast"][0]
	if q.Get("past_days") != "7" || q.Get("latitude") != "12.5" || q.Get("longitude") != "-3.25" || q.Get("timezone") != "UTC" || q.Get("hourly") != "temperature_2m" {
		t.Errorf("forecast query %v", q)
	}
	// After 06:00, up to 10:40: 07, 08, 10 (09 is null); 11:00 onwards is the future.
	var got []string
	for _, h := range hs {
		got = append(got, h.T.Format("15:04"))
	}
	if strings.Join(got, " ") != "07:00 08:00 10:00" {
		t.Errorf("hours %v", got)
	}
	if hs[0].Celsius != float64(7*24+7) {
		t.Errorf("value %v", hs[0])
	}
}

func TestOldHoursComeFromTheArchiveWhichWins(t *testing.T) {
	k := newFake(t)
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	k.archive = series(from, 41*24, func(int) *float64 { return f(100) }) // through end_date, 2026-03-13 23:00
	k.forecast = series(time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC), 8*24, func(int) *float64 { return f(-100) })
	hs, err := k.client().Readings(context.Background(), from, now)
	if err != nil {
		t.Fatal(err)
	}
	q := k.asked["/v1/archive"][0]
	if q.Get("start_date") != "2026-02-01" || q.Get("end_date") != "2026-03-13" {
		t.Errorf("archive dates %v", q)
	}
	byT := map[time.Time]float64{}
	for i, h := range hs {
		if i > 0 && !h.T.After(hs[i-1].T) {
			t.Fatalf("not oldest first, or a repeat, at %d: %v then %v", i, hs[i-1], h)
		}
		byT[h.T] = h.Celsius
	}
	if _, ok := byT[from]; ok {
		t.Error("the hour at from itself was returned: the caller already has it")
	}
	if v := byT[time.Date(2026, 3, 13, 5, 0, 0, 0, time.UTC)]; v != 100 {
		t.Errorf("an hour both have: %v, want the archive's", v)
	}
	if v := byT[time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)]; v != -100 {
		t.Errorf("a recent hour: %v, want the forecast's", v)
	}
	if last := hs[len(hs)-1].T; !last.Equal(time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("last hour %v", last)
	}
}

func TestErrorsSayWhyButNotWhere(t *testing.T) {
	k := newFake(t)
	k.status = http.StatusBadRequest
	_, err := k.client().Readings(context.Background(), now.Add(-time.Hour), now)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "12.5") || strings.Contains(err.Error(), "latitude") {
		t.Errorf("error gives the location away: %v", err)
	}

	c := &Client{Latitude: 12.5, Longitude: -3.25, Forecast: "http://127.0.0.1:1"} // nothing listens
	_, err = c.Readings(context.Background(), now.Add(-time.Hour), now)
	if err == nil || strings.Contains(err.Error(), "12.5") {
		t.Errorf("connection error %v: want one, without the location", err)
	}
}

func TestMismatchedReplyIsAnError(t *testing.T) {
	k := newFake(t)
	k.forecast = map[string]any{"hourly": map[string]any{"time": []string{"2026-03-20T09:00"}, "temperature_2m": []float64{}}}
	if _, err := k.client().Readings(context.Background(), now.Add(-2*time.Hour), now); err == nil {
		t.Error("want an error for times without temperatures")
	}
}
