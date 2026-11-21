package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/emporia"
	"github.com/hughobrien/emporiad/internal/promwrite"
)

// listAPI is the fake with its own device list and usage reply, for trees
// that setup's fixture does not have.
type listAPI struct {
	*fakeAPI
	devs  []emporia.Device
	reply func() []emporia.Usage
}

func (a *listAPI) Devices(context.Context) ([]emporia.Device, error) { return a.devs, nil }

func (a *listAPI) Usage(_ context.Context, _ []int, at time.Time, _, _ string) ([]emporia.Usage, error) {
	a.calls = append(a.calls, "usage")
	a.usageAsks = append(a.usageAsks, at)
	if a.reply == nil {
		return nil, nil
	}
	return a.reply(), nil
}

func withDevices(devs []emporia.Device) (*Exporter, *listAPI, *fakeStore, *time.Time) {
	e, api, store, now := setup()
	l := &listAPI{fakeAPI: api, devs: devs}
	e.api = l
	return e, l, store, now
}

func plugs(first, second int, name string) []emporia.Device {
	on := true
	mk := func(g int) emporia.Device {
		return emporia.Device{Gid: g, Model: "SSO001", Firmware: "fw1", Name: name, Price: 0.25, Connected: &on,
			Channels: []emporia.Channel{{Num: "1,2,3"}}}
	}
	return []emporia.Device{mk(first), mk(second)}
}

func page(t *testing.T, e *Exporter) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Code, rec.Body.String()
}

// hasLine reports whether a line of a family carries the label text.
func hasLine(body, fam, label string) bool {
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, fam+"{") && strings.Contains(l, label) {
			return true
		}
	}
	return false
}

func TestSameModelDevicesStayApart(t *testing.T) {
	for _, c := range []struct {
		name string
		want []string // device labels, lowest gid first
	}{{"", []string{"", "(2)"}}, {"Plug", []string{"Plug", "Plug (2)"}}} {
		for _, order := range [][2]int{{1, 2}, {2, 1}} {
			t.Run(fmt.Sprintf("%q/%v", c.name, order), func(t *testing.T) {
				e, _, store, _ := withDevices(plugs(order[0], order[1], c.name))
				e.poll(context.Background())
				code, body := page(t, e)
				if code != 200 {
					t.Fatalf("/metrics answered %d: %s", code, body)
				}
				for _, d := range c.want {
					for _, fam := range []string{"emporia_device_info", "emporia_device_connected", "emporia_price_per_kwh", "emporia_power_watts"} {
						if !hasLine(body, fam, fmt.Sprintf(`device=%q,`, d)) {
							t.Errorf("no %s for device %q:\n%s", fam, d, body)
						}
					}
				}
				seen := map[string]int{}
				for _, w := range store.writes {
					for _, s := range w {
						if s.Labels["__name__"] == "emporia_power_watts" && s.Labels["channel_num"] == "1,2,3" {
							seen[s.Labels["device"]]++
						}
					}
				}
				if len(seen) != 2 || seen[c.want[0]] != 1 || seen[c.want[1]] != 1 {
					t.Errorf("want one pushed series for each of %q, got %v", c.want, seen)
				}
			})
		}
	}
}

func TestSuffixIsStableAcrossOrder(t *testing.T) {
	names := func(ds []emporia.Device) map[int]string {
		m := map[int]string{}
		for _, d := range nameDevices(ds) {
			m[d.Gid] = d.Name
		}
		return m
	}
	a, b := names(plugs(1, 2, "Plug")), names(plugs(2, 1, "Plug"))
	if a[1] != "Plug" || a[2] != "Plug (2)" || b[1] != "Plug" || b[2] != "Plug (2)" {
		t.Errorf("lowest gid should keep the name: %v %v", a, b)
	}
}

func TestNameDevices(t *testing.T) {
	vue := func(g int, n string) []emporia.Device {
		return []emporia.Device{
			{Gid: g, Model: "VUE003", Name: n, Channels: []emporia.Channel{{Num: "1,2,3"}}},
			{Gid: g, Model: "WAT001", Name: n, Parent: g, Channels: []emporia.Channel{{Num: "1"}}},
		}
	}
	show := func(ds []emporia.Device) string {
		var s []string
		for _, d := range ds {
			s = append(s, fmt.Sprintf("%d/%s/%q", d.Gid, d.Model, d.Name))
		}
		return strings.Join(s, " ")
	}
	t.Run("standard fixture is untouched", func(t *testing.T) {
		in := vue(7, "Home")
		if got, want := show(nameDevices(in)), show(in); got != want {
			t.Errorf("got %s want %s", got, want)
		}
	})
	t.Run("the same device listed twice is dropped, not renamed", func(t *testing.T) {
		in := append(vue(7, "Home"), vue(7, "Home")...)
		if got, want := show(nameDevices(in)), show(vue(7, "Home")); got != want {
			t.Errorf("got %s want %s", got, want)
		}
	})
	t.Run("two monitors with the same name carry their expansions along", func(t *testing.T) {
		in := append(vue(7, "Home"), vue(4, "Home")...)
		got := show(nameDevices(in))
		want := `7/VUE003/"Home (2)" 7/WAT001/"Home (2)" 4/VUE003/"Home" 4/WAT001/"Home"`
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	})
	t.Run("a nested device with its own gid and the same model is told apart", func(t *testing.T) {
		in := append(vue(7, "Home"), emporia.Device{Gid: 8, Model: "WAT001", Name: "Home", Parent: 7})
		got := show(nameDevices(in))
		want := `7/VUE003/"Home" 7/WAT001/"Home" 8/WAT001/"Home (2)"`
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	})
	t.Run("input is not modified", func(t *testing.T) {
		in := plugs(2, 1, "Plug")
		nameDevices(in)
		if in[0].Name != "Plug" || in[1].Name != "Plug" {
			t.Error("nameDevices wrote into its argument")
		}
	})
}

func TestNestedDeviceReusingAChannelNumber(t *testing.T) {
	devs := []emporia.Device{
		{Gid: 1, Model: "VUE003", Name: "Home", Channels: []emporia.Channel{{Num: "1,2,3"}}},
		{Gid: 2, Model: "WAT001", Name: "Home", Parent: 1, Channels: []emporia.Channel{{Num: "1", Name: "Oven"}}},
		{Gid: 5, Model: "WAT001", Name: "Home", Parent: 1, Channels: []emporia.Channel{{Num: "1", Name: "Shed"}}},
	}
	e, api, store, now := withDevices(devs)
	api.reply = func() []emporia.Usage {
		return []emporia.Usage{
			{DeviceGid: 1, Num: "1,2,3", Value: f(0.6)},
			{DeviceGid: 2, Num: "1", Value: f(0.12)},
			{DeviceGid: 5, Num: "1", Value: f(0.5)},
			{DeviceGid: 1, Num: "Balance", Value: f(-0.02)},
		}
	}
	store.newest["emporia_power_watts"] = time.Date(2026, 11, 5, 9, 50, 0, 0, time.UTC)
	store.newest["emporia_hourly_cost"] = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	*now = time.Date(2026, 11, 5, 10, 6, 30, 0, time.UTC)
	e.poll(context.Background())
	got := map[string]float64{}
	for _, w := range store.writes {
		for _, s := range w {
			l := s.Labels
			if l["__name__"] == "emporia_hourly_energy_kwh" && l["channel_num"] == "1" {
				got[l["device"]+"/"+l["channel"]] = s.Samples[0].V
			}
		}
	}
	want := map[string]float64{"Home/Oven": 0.12, "Home (2)/Shed": 0.5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("hourly circuit 1: got %v want %v", got, want)
	}
	if s := find(store.writes, "emporia_hourly_energy_kwh", "Balance"); len(s) != 1 {
		t.Errorf("Balance should still match: %v", s)
	}
}

func TestCollectorSkipsADuplicate(t *testing.T) {
	var logs strings.Builder
	e, _, _, _ := setup()
	e.log = slog.New(slog.NewTextHandler(&logs, nil))
	// Past nameDevices: the same facts twice, and a reading twice.
	e.devices = plugs(1, 2, "Plug")
	r := promwrite.Series{Labels: map[string]string{"__name__": "emporia_power_watts", "device": "Plug", "model": "SSO001",
		"channel": "Main", "channel_num": "1,2,3"}, Samples: []promwrite.Sample{{T: t0, V: 5}}}
	e.latest["a"], e.latest["b"] = r, r
	logged := 0
	for i := range 2 {
		code, body := page(t, e)
		if code != 200 {
			t.Fatalf("/metrics answered %d: %s", code, body)
		}
		for _, fam := range []string{"emporia_device_info{", "emporia_device_connected{", "emporia_price_per_kwh{", "emporia_power_watts{"} {
			if n := strings.Count(body, "\n"+fam); n != 1 {
				t.Errorf("%s: %d copies, want 1", fam, n)
			}
		}
		n := strings.Count(logs.String(), "duplicate")
		if i == 0 && n == 0 {
			t.Fatalf("want the duplicates logged: %q", logs.String())
		}
		if i == 0 {
			logged = n
		} else if n != logged {
			t.Errorf("logged again on a later scrape: %d lines, then %d", logged, n)
		}
	}
}
