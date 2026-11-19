package emporia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var httpClient = http.Client{Timeout: 20 * time.Second}

// TokenSource supplies the token sent in the authtoken header.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Expire()
}

// Client calls Emporia's API.
type Client struct {
	Tokens TokenSource
	HTTP   *http.Client
	Base   string // defaults to https://api.emporiaenergy.com
}

// Device is one monitor (a Vue, its circuit expansion, a smart plug).
type Device struct {
	Gid       int
	Model     string
	Firmware  string
	Name      string  // from the app; inherited from the parent if unset
	Price     float64 // per kWh in the account's currency, 0 if unset
	Connected *bool   // nil when the API does not say
	Channels  []Channel
	Parent    int // Gid of the device this one is nested under, or 0
}

// Channel is a measured circuit as configured in the app.
type Channel struct {
	Num  string // "1,2,3" is the mains, "1".."16" circuits
	Name string
}

// Usage is one channel's energy or cost over one interval.
type Usage struct {
	DeviceGid int
	Num       string
	Name      string
	Value     *float64 // kWh or dollars; nil while Emporia has no data for the interval
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	base := c.Base
	if base == "" {
		base = "https://api.emporiaenergy.com"
	}
	hc := c.HTTP
	if hc == nil {
		hc = &httpClient
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.Tokens.Token(ctx)
		if err != nil {
			return fmt.Errorf("login: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("authtoken", tok)
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.Tokens.Expire()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
		}
		return json.Unmarshal(data, out)
	}
}

type wireDevice struct {
	DeviceGid       int
	Model           string
	Firmware        string
	ParentDeviceGid *int
	DeviceConnected *struct{ Connected bool }
	// Prices are cents in the account's currency.
	LocationProperties *struct {
		DeviceName         string
		UsageCentPerKwHour float64
	}
	Channels []struct {
		ChannelNum string
		Name       *string
	}
	Devices []wireDevice
}

// Devices lists the account's devices, nested ones after their parent.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var r struct{ Devices []wireDevice }
	if err := c.get(ctx, "customers/devices", &r); err != nil {
		return nil, err
	}
	var out []Device
	var walk func(ws []wireDevice, parent *Device)
	walk = func(ws []wireDevice, parent *Device) {
		for _, w := range ws {
			d := Device{Gid: w.DeviceGid, Model: w.Model, Firmware: w.Firmware}
			if lp := w.LocationProperties; lp != nil {
				d.Name, d.Price = lp.DeviceName, lp.UsageCentPerKwHour/100
			}
			if parent != nil {
				d.Parent = parent.Gid
				if d.Name == "" {
					d.Name = parent.Name
				}
				if d.Price == 0 {
					d.Price = parent.Price
				}
			}
			if w.DeviceConnected != nil {
				v := w.DeviceConnected.Connected
				d.Connected = &v
			}
			for _, ch := range w.Channels {
				n := ""
				if ch.Name != nil {
					n = *ch.Name
				}
				d.Channels = append(d.Channels, Channel{Num: ch.ChannelNum, Name: n})
			}
			out = append(out, d)
			walk(w.Devices, &d)
		}
	}
	walk(r.Devices, nil)
	return out, nil
}

type wireUsageDevice struct {
	DeviceGid     int
	ChannelUsages []struct {
		DeviceGid     int
		ChannelNum    string
		Name          string
		Usage         *float64
		NestedDevices []wireUsageDevice
	}
}

// Units for Usage. Dollars prices the energy with the utility rate plan
// selected in the app (tiers, billing cycle), or the flat price without one.
const (
	KWh     = "KilowattHours"
	Dollars = "Dollars"
)

// Usage returns each channel's energy (or its cost, with unit Dollars) for the
// scale-long interval ending at instant (scale "1MIN", "1H", ...). Emporia
// fills the current minute late, so ask for one that ended at least a minute
// ago.
func (c *Client) Usage(ctx context.Context, gids []int, instant time.Time, scale, unit string) ([]Usage, error) {
	if len(gids) == 0 {
		return nil, errors.New("no devices")
	}
	ids := make([]string, len(gids))
	for i, g := range gids {
		ids[i] = strconv.Itoa(g)
	}
	q := "apiMethod=getDeviceListUsages&deviceGids=" + strings.Join(ids, "+") +
		"&instant=" + url.QueryEscape(instant.UTC().Format("2006-01-02T15:04:05Z")) +
		"&scale=" + url.QueryEscape(scale) + "&energyUnit=" + url.QueryEscape(unit)
	var r struct {
		DeviceListUsages struct{ Devices []wireUsageDevice }
	}
	if err := c.get(ctx, "AppAPI?"+q, &r); err != nil {
		return nil, err
	}
	var out []Usage
	seen := map[[2]string]bool{}
	var walk func([]wireUsageDevice)
	walk = func(ds []wireUsageDevice) {
		for _, d := range ds {
			for _, u := range d.ChannelUsages {
				gid := u.DeviceGid
				if gid == 0 {
					gid = d.DeviceGid
				}
				k := [2]string{strconv.Itoa(gid), u.ChannelNum}
				if !seen[k] {
					seen[k] = true
					out = append(out, Usage{DeviceGid: gid, Num: u.ChannelNum, Name: u.Name, Value: u.Usage})
				}
				walk(u.NestedDevices)
			}
		}
	}
	walk(r.DeviceListUsages.Devices)
	return out, nil
}
