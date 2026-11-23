package weather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const mac = "00:11:22:AA:BB:CC" // invented

// ambientFake answers device-data as ambientweather.net does: the readings
// after start and not after end, newest first, every five minutes, with
// fields the client must not keep.
type ambientFake struct {
	srv    *httptest.Server
	asked  [][2]time.Time
	status int
	gap    func(time.Time) bool // readings left out (the station was off)
}

func newAmbientFake(t *testing.T) *ambientFake {
	k := &ambientFake{status: http.StatusOK}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/device-data" || q.Get("macAddress") != mac || q.Get("res") != "5" {
			http.Error(w, `{"name":"NotFound","message":"Page not found","code":404}`, http.StatusNotFound)
			return
		}
		ms := func(k string) time.Time {
			v, _ := strconv.ParseInt(q.Get(k), 10, 64)
			return time.UnixMilli(v).UTC()
		}
		start, end := ms("start"), ms("end")
		k.asked = append(k.asked, [2]time.Time{start, end})
		if k.status != http.StatusOK {
			w.WriteHeader(k.status)
			json.NewEncoder(w).Encode(map[string]any{"name": "GeneralError", "message": "Request failed", "code": k.status})
			return
		}
		data := []map[string]any{}
		for t := end.Truncate(5 * time.Minute); t.After(start); t = t.Add(-5 * time.Minute) {
			if k.gap != nil && k.gap(t) {
				continue
			}
			data = append(data, map[string]any{
				"dateutc": t.UnixMilli(), "tempf": 32 + 1.8*float64(t.Minute()), "humidity": 70,
				"PASSKEY": "not-for-keeping", "ip": "192.0.2.1",
			})
			if len(data) == 500 {
				break // the reply's cap
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *ambientFake) client() *Ambient { return &Ambient{MAC: mac, Base: k.srv.URL} }

func TestAmbientAsksADayAtATimeAndReturnsOldestFirst(t *testing.T) {
	k := newAmbientFake(t)
	from := now.Add(-50 * time.Hour)
	rs, err := k.client().Readings(context.Background(), from, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.asked) != 3 {
		t.Fatalf("asked %v, want three calls of at most a day", k.asked)
	}
	for _, a := range k.asked {
		if a[1].Sub(a[0]) > 24*time.Hour {
			t.Errorf("asked %v: more than a day, which could pass the reply's cap", a)
		}
	}
	if n := len(rs); n != 50*12 {
		t.Fatalf("%d readings, want every five minutes for 50 hours", n)
	}
	if !rs[0].T.Equal(from.Add(5*time.Minute)) || !rs[len(rs)-1].T.Equal(now) {
		t.Errorf("readings %v to %v, want after from and up to now", rs[0].T, rs[len(rs)-1].T)
	}
	for i := 1; i < len(rs); i++ {
		if !rs[i].T.After(rs[i-1].T) {
			t.Fatalf("not oldest first at %d: %v then %v", i, rs[i-1].T, rs[i].T)
		}
	}
	if r := rs[1]; r.Celsius != float64(r.T.Minute()) {
		t.Errorf("%v: %v °C, want the reading converted from °F", r.T, r.Celsius)
	}
}

func TestAmbientSkipsReadingsWithoutATemperature(t *testing.T) {
	k := newAmbientFake(t)
	k.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"data":[{"dateutc":` + strconv.FormatInt(now.UnixMilli(), 10) + `,"humidity":70},` +
			`{"dateutc":` + strconv.FormatInt(now.Add(-5*time.Minute).UnixMilli(), 10) + `,"tempf":50}]}`
		w.Write([]byte(body))
	})
	rs, err := k.client().Readings(context.Background(), now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Celsius != 10 {
		t.Errorf("readings %v, want only the one with tempf, at 10 °C", rs)
	}
}

func TestAmbientErrorsNameNeitherTheStationNorItsKey(t *testing.T) {
	k := newAmbientFake(t)
	k.status = http.StatusInternalServerError
	_, err := k.client().Readings(context.Background(), now.Add(-time.Hour), now)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "Request failed") {
		t.Fatalf("err %v, want the status and message", err)
	}
	if strings.Contains(err.Error(), mac) || strings.Contains(err.Error(), "macAddress") {
		t.Errorf("err %q names the station", err)
	}
	c := &Ambient{MAC: mac, Base: "http://127.0.0.1:1"}
	_, err = c.Readings(context.Background(), now.Add(-time.Hour), now)
	if err == nil || strings.Contains(err.Error(), mac) || strings.Contains(err.Error(), "macAddress") {
		t.Errorf("err %v: want a failure that does not name the station", err)
	}
}

func TestAmbientEmptyWindowIsNoError(t *testing.T) {
	k := newAmbientFake(t)
	k.gap = func(time.Time) bool { return true }
	rs, err := k.client().Readings(context.Background(), now.Add(-30*time.Hour), now)
	if err != nil || len(rs) != 0 {
		t.Errorf("got %v, %v: want nothing, no error, before the station existed", rs, err)
	}
}
