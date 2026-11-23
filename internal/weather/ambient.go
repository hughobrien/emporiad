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

// AmbientKeeps is about how far back ambientweather.net has a station's
// readings: on 2026-11-22 one station's went back to 2026-05-30, nearly six
// months, every five minutes.
const AmbientKeeps = 180 * 24 * time.Hour

// ambientDay is the most one call asks: 288 readings, under the 500 a reply
// holds (asking for more with limit= fails past 2000).
const ambientDay = 24 * time.Hour

// Ambient reads a weather station's outdoor temperature from
// ambientweather.net, as the page of a station its owner has shared does:
// by the station's MAC address, with no key.
type Ambient struct {
	MAC  string
	HTTP *http.Client
	Base string // default https://lightning.ambientweather.net
}

// Readings returns the station's readings after from and not after to,
// oldest first, asked a day at a time.
func (a *Ambient) Readings(ctx context.Context, from, to time.Time) ([]Reading, error) {
	from, to = from.UTC(), to.UTC()
	var out []Reading
	for start := from; start.Before(to); start = start.Add(ambientDay) {
		end := start.Add(ambientDay)
		if end.After(to) {
			end = to
		}
		rs, err := a.get(ctx, start, end)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	slices.SortFunc(out, func(a, b Reading) int { return a.T.Compare(b.T) })
	out = slices.CompactFunc(out, func(a, b Reading) bool { return a.T.Equal(b.T) })
	return slices.DeleteFunc(out, func(r Reading) bool { return !r.T.After(from) || r.T.After(to) }), nil
}

// ambientReply keeps only the time and temperature: a reading also carries
// the station's upload key and address.
type ambientReply struct {
	Message string
	Data    []struct {
		DateUTC int64    `json:"dateutc"`
		TempF   *float64 `json:"tempf"`
	}
}

// get asks for one window. Errors name the path only: the query names the
// station.
func (a *Ambient) get(ctx context.Context, start, end time.Time) ([]Reading, error) {
	q := url.Values{
		"macAddress": {a.MAC},
		"start":      {strconv.FormatInt(start.UnixMilli(), 10)},
		"end":        {strconv.FormatInt(end.UnixMilli(), 10)},
		"res":        {"5"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, or(a.Base, "https://lightning.ambientweather.net")+"/device-data?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	hc := a.HTTP
	if hc == nil {
		hc = &httpClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err // its message would repeat the URL, station and all
		}
		return nil, fmt.Errorf("GET %s: %w", req.URL.Path, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var r ambientReply
	jerr := json.Unmarshal(data, &r)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(r.Message)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, msg)
	}
	if jerr != nil {
		return nil, fmt.Errorf("GET %s: %w", req.URL.Path, jerr)
	}
	var out []Reading
	for _, d := range r.Data {
		if d.TempF == nil {
			continue
		}
		out = append(out, Reading{time.UnixMilli(d.DateUTC).UTC(), (*d.TempF - 32) * 5 / 9})
	}
	return out, nil
}
