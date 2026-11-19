package emporia

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hughobrien/emporiad/internal/cognito"
)

// Shapes copied from the real API; names, ids and values are invented.
const devicesJSON = `{"customerGid":1,"email":"x@example.com","devices":[
 {"deviceGid":100,"model":"VUE003","firmware":"Vue3-1","parentDeviceGid":null,
  "deviceConnected":{"deviceGid":100,"connected":true,"offlineSince":null},
  "locationProperties":{"deviceName":"Home","usageCentPerKwHour":25.5},
  "channels":[{"deviceGid":100,"name":null,"channelNum":"1,2,3","channelMultiplier":1.0}],
  "devices":[
   {"deviceGid":101,"model":"WAT001","firmware":"","parentDeviceGid":100,"locationProperties":null,
    "channels":[{"channelNum":"1","name":"Oven"},{"channelNum":"2","name":null}],"devices":[]}]}]}`

const usageJSON = `{"deviceListUsages":{"instant":"2026-11-05T09:03:00Z","scale":"1MIN","energyUnit":"KilowattHours","devices":[
 {"deviceGid":101,"channelUsages":[
  {"name":"Main","usage":0.01,"deviceGid":101,"channelNum":"1,2,3","nestedDevices":[
   {"deviceGid":102,"channelUsages":[{"name":"Plug","usage":0.001,"deviceGid":102,"channelNum":"1,2,3","nestedDevices":[]}]}]},
  {"name":"Oven","usage":null,"deviceGid":101,"channelNum":"1","nestedDevices":[]},
  {"name":"Balance","usage":0.002,"deviceGid":101,"channelNum":"Balance","nestedDevices":[]}]}]}}`

type staticTokens struct {
	tok     string
	expired int
}

func (s *staticTokens) Token(context.Context) (string, error) { return s.tok, nil }
func (s *staticTokens) Expire()                               { s.expired++; s.tok = "fresh" }

func TestDevices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/customers/devices" || r.Header.Get("authtoken") != "tok" {
			t.Errorf("got %s authtoken=%q", r.URL, r.Header.Get("authtoken"))
		}
		w.Write([]byte(devicesJSON))
	}))
	defer srv.Close()
	c := &Client{Tokens: &staticTokens{tok: "tok"}, Base: srv.URL}
	ds, err := c.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("got %d devices", len(ds))
	}
	vue, ext := ds[0], ds[1]
	if vue.Name != "Home" || vue.Price != 0.255 || vue.Connected == nil || !*vue.Connected || vue.Parent != 0 {
		t.Errorf("vue %+v", vue)
	}
	if ext.Name != "Home" || ext.Parent != 100 || ext.Connected != nil || ext.Price != 0.255 {
		t.Errorf("nested device should inherit name and price: %+v", ext)
	}
	if len(ext.Channels) != 2 || ext.Channels[0] != (Channel{"1", "Oven"}) || ext.Channels[1].Name != "" {
		t.Errorf("channels %+v", ext.Channels)
	}
}

func TestUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("deviceGids") != "100 101" || q.Get("instant") != "2026-11-05T09:03:00Z" || q.Get("scale") != "1MIN" || q.Get("energyUnit") != "Dollars" {
			t.Errorf("query %v", q)
		}
		w.Write([]byte(usageJSON))
	}))
	defer srv.Close()
	c := &Client{Tokens: &staticTokens{tok: "tok"}, Base: srv.URL}
	at := time.Date(2026, 11, 5, 1, 3, 0, 0, time.FixedZone("PST", -8*3600))
	us, err := c.Usage(context.Background(), []int{100, 101}, at, "1MIN", Dollars)
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 4 {
		t.Fatalf("got %d usages: %+v", len(us), us)
	}
	if us[0].Num != "1,2,3" || *us[0].Value != 0.01 || us[1].DeviceGid != 102 || us[1].Name != "Plug" {
		t.Errorf("nested usage not flattened in order: %+v %+v", us[0], us[1])
	}
	if us[2].Value != nil {
		t.Errorf("null usage should stay nil")
	}
}

func TestChart(t *testing.T) {
	// Asked for 09:00 to 09:03: three minutes, plus Emporia's bad bucket at 09:03.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("apiMethod") != "getChartUsage" || q.Get("deviceGid") != "101" || q.Get("channel") != "1,2,3" ||
			q.Get("start") != "2026-11-05T09:00:00Z" || q.Get("end") != "2026-11-05T09:03:00Z" ||
			q.Get("scale") != "1MIN" || q.Get("energyUnit") != "KilowattHours" {
			t.Errorf("query %v", q)
		}
		w.Write([]byte(`{"firstUsageInstant":"2026-11-05T09:00:00Z","usageList":[0.01,null,0.02,0.004]}`))
	}))
	defer srv.Close()
	c := &Client{Tokens: &staticTokens{tok: "tok"}, Base: srv.URL}
	start := time.Date(2026, 11, 5, 1, 0, 30, 0, time.FixedZone("PST", -8*3600)) // truncated to 09:00Z
	vs, err := c.Chart(context.Background(), 101, "1,2,3", start, start.Add(3*time.Minute), KWh)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || *vs[0] != 0.01 || vs[1] != nil || *vs[2] != 0.02 {
		t.Fatalf("got %v, want three minutes with the end bucket dropped", vs)
	}
}

func TestChartShortAndMisaligned(t *testing.T) {
	reply := `{"firstUsageInstant":"2026-11-05T09:00:00Z","usageList":[0.01]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(reply)) }))
	defer srv.Close()
	c := &Client{Tokens: &staticTokens{tok: "tok"}, Base: srv.URL}
	start := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	if vs, err := c.Chart(context.Background(), 1, "6", start, start.Add(10*time.Minute), KWh); err != nil || len(vs) != 1 {
		t.Errorf("unfilled minutes should just be missing: %v %v", vs, err)
	}
	reply = `{"firstUsageInstant":"2026-11-05T08:59:00Z","usageList":[0.01]}`
	if _, err := c.Chart(context.Background(), 1, "6", start, start.Add(10*time.Minute), KWh); err == nil {
		t.Error("a reply starting at another minute should be an error")
	}
}

func TestRetriesOnceAfter401(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("authtoken") != "fresh" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()
	ts := &staticTokens{tok: "stale"}
	c := &Client{Tokens: ts, Base: srv.URL}
	if _, err := c.Devices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || ts.expired != 1 {
		t.Errorf("calls %d, expired %d", calls, ts.expired)
	}
}

type fakeAuth struct {
	logins, refreshes int
	refreshErr        error
}

func (f *fakeAuth) Login(context.Context, string, string) (*cognito.Tokens, error) {
	f.logins++
	return &cognito.Tokens{IDToken: "login", RefreshToken: "r", Expires: time.Unix(1000, 0)}, nil
}

func (f *fakeAuth) Refresh(context.Context, string) (*cognito.Tokens, error) {
	f.refreshes++
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return &cognito.Tokens{IDToken: "refreshed", RefreshToken: "r", Expires: time.Unix(2000, 0)}, nil
}

func TestSessionRefreshesThenLogsInAgain(t *testing.T) {
	now := time.Unix(0, 0)
	fa := &fakeAuth{}
	s := &Session{Auth: fa, Now: func() time.Time { return now }}
	ctx := context.Background()
	tok := func() string {
		t.Helper()
		v, err := s.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if tok() != "login" || tok() != "login" || fa.logins != 1 {
		t.Fatalf("first login not cached (logins %d)", fa.logins)
	}
	now = time.Unix(950, 0) // within a minute of expiry
	if tok() != "refreshed" || fa.refreshes != 1 {
		t.Fatal("did not refresh before expiry")
	}
	now = time.Unix(5000, 0)
	fa.refreshErr = &cognito.Error{Type: "NotAuthorizedException", Message: "expired"}
	if tok() != "login" || fa.logins != 2 {
		t.Fatal("did not log in again after the refresh token was refused")
	}
	s.Expire()
	fa.refreshErr = errors.New("network down")
	if _, err := s.Token(ctx); err == nil || fa.logins != 2 {
		t.Fatal("a network error on refresh should fail, not log in again")
	}
}
