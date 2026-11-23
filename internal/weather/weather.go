// Package weather reads hourly outdoor temperature from Open-Meteo, which
// needs no account or key: the archive API for anything older than a week,
// the forecast API (its past days) for the week up to now.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Recent is how far back the forecast API is asked; older hours come from
// the archive.
const Recent = 7 * 24 * time.Hour

var httpClient = http.Client{Timeout: 60 * time.Second}

// Hour is the temperature 2 m above ground at T, which is on the hour.
type Hour struct {
	T       time.Time
	Celsius float64
}

// Client asks Open-Meteo about one place.
type Client struct {
	Latitude, Longitude float64
	HTTP                *http.Client
	Forecast, Archive   string // base URLs; default to Open-Meteo's
}

// Hours returns the hours after from and not after to, oldest first. An hour
// both APIs have comes from the archive.
func (c *Client) Hours(ctx context.Context, from, to time.Time) ([]Hour, error) {
	from, to = from.UTC(), to.UTC()
	got := map[time.Time]float64{}
	recent := to.Add(-Recent)
	if from.Before(recent) {
		q := c.query()
		q.Set("start_date", from.Format(time.DateOnly))
		q.Set("end_date", recent.Format(time.DateOnly))
		hs, err := c.get(ctx, or(c.Archive, "https://archive-api.open-meteo.com")+"/v1/archive", q)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			got[h.T] = h.Celsius
		}
	}
	q := c.query()
	q.Set("past_days", strconv.Itoa(int(Recent/(24*time.Hour))))
	q.Set("forecast_days", "1")
	hs, err := c.get(ctx, or(c.Forecast, "https://api.open-meteo.com")+"/v1/forecast", q)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if _, ok := got[h.T]; !ok {
			got[h.T] = h.Celsius
		}
	}
	var out []Hour
	for t, v := range got {
		if t.After(from) && !t.After(to) {
			out = append(out, Hour{t, v})
		}
	}
	slices.SortFunc(out, func(a, b Hour) int { return a.T.Compare(b.T) })
	return out, nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (c *Client) query() url.Values {
	return url.Values{
		"latitude":  {strconv.FormatFloat(c.Latitude, 'f', -1, 64)},
		"longitude": {strconv.FormatFloat(c.Longitude, 'f', -1, 64)},
		"hourly":    {"temperature_2m"},
		"timezone":  {"UTC"},
	}
}

type reply struct {
	Reason string
	Hourly struct {
		Time          []string
		Temperature2m []*float64 `json:"temperature_2m"`
	}
}

// get asks one API. Errors name the path only: the query holds the location.
func (c *Client) get(ctx context.Context, base string, q url.Values) ([]Hour, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = &httpClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err // its message would repeat the URL, location and all
		}
		return nil, fmt.Errorf("GET %s: %w", req.URL.Path, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var r reply
	jerr := json.Unmarshal(data, &r)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(r.Reason)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, msg)
	}
	if jerr != nil {
		return nil, fmt.Errorf("GET %s: %w", req.URL.Path, jerr)
	}
	h := r.Hourly
	if len(h.Time) != len(h.Temperature2m) {
		return nil, fmt.Errorf("GET %s: %d times but %d temperatures", req.URL.Path, len(h.Time), len(h.Temperature2m))
	}
	var out []Hour
	for i, s := range h.Time {
		if h.Temperature2m[i] == nil {
			continue // not available (yet)
		}
		t, err := time.Parse("2006-01-02T15:04", s)
		if err != nil {
			return nil, fmt.Errorf("GET %s: time %q: %w", req.URL.Path, s, err)
		}
		out = append(out, Hour{t, *h.Temperature2m[i]})
	}
	return out, nil
}
