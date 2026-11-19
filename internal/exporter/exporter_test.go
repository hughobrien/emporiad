package exporter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
)

type stubAPI struct {
	devices        []emporia.Device
	usage, cost    []emporia.Usage
	usageErr, cErr error
	asked          time.Time
}

func (s *stubAPI) Devices(context.Context) ([]emporia.Device, error) { return s.devices, nil }
func (s *stubAPI) Usage(_ context.Context, _ []int, at time.Time, _, unit string) ([]emporia.Usage, error) {
	s.asked = at
	if unit == emporia.Dollars {
		return s.cost, s.cErr
	}
	return s.usage, s.usageErr
}

func f(v float64) *float64 { return &v }

func setup() (*Exporter, *stubAPI, *time.Time) {
	on := true
	api := &stubAPI{
		devices: []emporia.Device{
			{Gid: 1, Model: "VUE003", Firmware: "fw1", Name: "Home", Price: 0.25, Connected: &on},
			{Gid: 2, Model: "WAT001", Name: "Home", Price: 0.25, Parent: 1},
		},
		usage: []emporia.Usage{
			{DeviceGid: 2, Num: "1,2,3", Name: "Main", Value: f(0.01)},
			{DeviceGid: 2, Num: "4", Name: "", Value: f(0.0005)},
			{DeviceGid: 2, Num: "5", Name: "Oven", Value: nil},
		},
		cost: []emporia.Usage{
			{DeviceGid: 2, Num: "1,2,3", Name: "Main", Value: f(0.002)},
			{DeviceGid: 2, Num: "4", Name: "", Value: nil},
		},
	}
	now := time.Date(2026, 11, 5, 9, 4, 30, 0, time.UTC)
	e := New(Config{PollInterval: time.Minute, DevicesRefresh: 5 * time.Minute, Lag: time.Minute, Stale: 5 * time.Minute},
		api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return now }
	return e, api, &now
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

func TestPollAndExport(t *testing.T) {
	e, api, _ := setup()
	ctx := context.Background()
	e.refreshDevices(ctx)
	e.pollUsage(ctx)
	if want := time.Date(2026, 11, 5, 9, 3, 0, 0, time.UTC); !api.asked.Equal(want) {
		t.Errorf("asked for %v, want the minute ending %v", api.asked, want)
	}
	got := scrape(t, e)
	for _, want := range []string{
		`emporia_power_watts{channel="Main",channel_num="1,2,3",device="Home",model="WAT001"} 600`,
		`emporia_power_watts{channel="Channel 4",channel_num="4",device="Home",model="WAT001"} 30`,
		`emporia_cost_per_hour{channel="Main",channel_num="1,2,3",device="Home",model="WAT001"} 0.12`,
		`emporia_price_per_kwh{device="Home",model="VUE003"} 0.25`,
		`emporia_device_connected{device="Home",model="VUE003"} 1`,
		`emporia_device_info{device="Home",firmware="fw1",model="VUE003"} 1`,
		`emporia_poll_errors_total{stage="usage"} 0`,
		`emporiad_build_info{version="test"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\n--- got:\n%s", want, got)
		}
	}
	if strings.Contains(got, `emporia_cost_per_hour{channel="Channel 4"`) {
		t.Error("a channel with no cost data should have no cost series")
	}
	if strings.Contains(got, `channel="Oven"`) {
		t.Error("a channel with no data yet should not be exported")
	}
	if strings.Count(got, "emporia_price_per_kwh") != 1 {
		t.Error("price should come from the top-level device only")
	}
}

func TestStaleReadingsDisappear(t *testing.T) {
	e, api, now := setup()
	ctx := context.Background()
	e.refreshDevices(ctx)
	e.pollUsage(ctx)
	api.usageErr = errors.New("cloud down")
	*now = now.Add(3 * time.Minute) // the reading now covers a minute 4.5 min ago
	e.pollUsage(ctx)
	if got := scrape(t, e); !strings.Contains(got, `channel="Main"`) || !strings.Contains(got, `emporia_poll_errors_total{stage="usage"} 1`) {
		t.Fatalf("a short outage should keep the last reading and count the error:\n%s", got)
	}
	*now = now.Add(2 * time.Minute)
	if got := scrape(t, e); strings.Contains(got, "emporia_power_watts") {
		t.Fatalf("readings older than Stale should vanish:\n%s", got)
	}
}

func TestCostFailureKeepsPower(t *testing.T) {
	e, api, _ := setup()
	api.cErr = errors.New("dollars refused")
	ctx := context.Background()
	e.refreshDevices(ctx)
	e.pollUsage(ctx)
	got := scrape(t, e)
	if !strings.Contains(got, `emporia_power_watts{channel="Main"`) || strings.Contains(got, "emporia_cost_per_hour") ||
		!strings.Contains(got, `emporia_poll_errors_total{stage="cost"} 1`) {
		t.Fatalf("a failed cost call should keep power, drop cost and count the error:\n%s", got)
	}
}
