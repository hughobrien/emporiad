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
			if ue := (*url.Error)(nil); errors.As(err, &ue) {
				err = ue.Err // its message would repeat the URL, query and deviceGid
			}
			return fmt.Errorf("GET %s: %w", strings.SplitN(path, "?", 2)[0], err)
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
			err := fmt.Errorf("GET %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
			if msg := snippet(data); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return json.Unmarshal(data, out)
	}
}

// snippet is the start of an error reply on one line: Emporia's 400s say what
// was wrong (such as the span limit), which the status alone hides.
func snippet(body []byte) string {
	if len(body) > 200 {
		body = body[:200]
	}
	return strings.Join(strings.Fields(strings.ToValidUTF8(string(body), "")), " ")
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

// Chart returns one channel's energy (kWh, or cost with unit Dollars) for each
// minute from start up to end (both on minute boundaries), oldest first; nil
// where Emporia has no data. Minutes Emporia has not filled yet are missing
// from the end, so the result can be shorter than asked.
//
// Two quirks shape this: the reply's bucket for the minute containing the
// requested end is wrong (16:10 read 2958 W for a range ending at 16:10, 5286
// W in a range that went past it), so we ask for one minute more and drop it;
// and asking for several channels at once returns their sum, not a list.
func (c *Client) Chart(ctx context.Context, gid int, channel string, start, end time.Time, unit string) ([]*float64, error) {
	return c.chart(ctx, gid, channel, start, end, "1MIN", time.Minute, unit)
}

// ChartHours is Chart by the hour: one value per hour from start up to end
// (both truncated to the hour), oldest first, nil where Emporia has no data.
// Emporia keeps hours back to the monitor's install, where minutes go back
// a week.
//
// At 1H one call may span at most 800 hours (HTTP 400 "larger than the
// allowed limit of PT800H"). Buckets are labelled by their start hour, and
// the bucket at end is right at this scale, but it is dropped all the same
// so both scales answer alike. The current hour comes back unfinished.
func (c *Client) ChartHours(ctx context.Context, gid int, channel string, start, end time.Time, unit string) ([]*float64, error) {
	return c.chart(ctx, gid, channel, start, end, "1H", time.Hour, unit)
}

func (c *Client) chart(ctx context.Context, gid int, channel string, start, end time.Time, scale string, step time.Duration, unit string) ([]*float64, error) {
	start, end = start.UTC().Truncate(step), end.UTC().Truncate(step)
	if !end.After(start) {
		return nil, nil
	}
	q := "apiMethod=getChartUsage&deviceGid=" + strconv.Itoa(gid) + "&channel=" + url.QueryEscape(channel) +
		"&start=" + url.QueryEscape(start.Format(time.RFC3339)) + "&end=" + url.QueryEscape(end.Format(time.RFC3339)) +
		"&scale=" + scale + "&energyUnit=" + url.QueryEscape(unit)
	var r struct {
		UsageList         []*float64
		FirstUsageInstant string
	}
	if err := c.get(ctx, "AppAPI?"+q, &r); err != nil {
		return nil, err
	}
	if len(r.UsageList) == 0 {
		return nil, nil
	}
	if first, err := time.Parse(time.RFC3339, r.FirstUsageInstant); err != nil || !first.Equal(start) {
		return nil, fmt.Errorf("chart starts at %q, asked for %s", r.FirstUsageInstant, start.Format(time.RFC3339))
	}
	want := int(end.Sub(start) / step)
	if len(r.UsageList) > want { // the bucket at end, or anything past it
		r.UsageList = r.UsageList[:want]
	}
	return r.UsageList, nil
}

// Usage returns each channel's energy (or its cost, with unit Dollars) for an
// interval of the given scale. With "1H" it is the hour containing instant,
// unfinished if that is the current hour. Don't use it for "1MIN": it answers
// on a two-minute grid whose phase moves (asking for 16:43 gave 16:42, 16:44
// and 16:45 both gave 16:44; earlier that day the grid was on odd minutes).
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
