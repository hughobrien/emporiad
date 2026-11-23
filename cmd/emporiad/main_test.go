package main

import (
	"strings"
	"testing"
	"time"
)

func TestPollIntervalMustBePositive(t *testing.T) {
	for _, d := range []time.Duration{-time.Minute, 0} {
		if checkPollInterval(d) == nil {
			t.Errorf("%v was accepted", d)
		}
	}
	if err := checkPollInterval(15 * time.Minute); err != nil {
		t.Error(err)
	}
}

func TestWeatherPlace(t *testing.T) {
	if p, err := parsePlace("", ""); p != nil || err != nil {
		t.Errorf("neither: %v %v, want off", p, err)
	}
	p, err := parsePlace("12.5", "-3.25")
	if err != nil || p.Latitude != 12.5 || p.Longitude != -3.25 {
		t.Errorf("both: %+v %v", p, err)
	}
	for _, c := range [][2]string{{"12.5", ""}, {"", "3"}, {"91", "0"}, {"0", "-181"}, {"north", "0"}} {
		if _, err := parsePlace(c[0], c[1]); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
}

func TestAmbientStation(t *testing.T) {
	if s, err := parseStation(""); s != nil || err != nil {
		t.Errorf("empty: %v %v, want off", s, err)
	}
	s, err := parseStation("00-11-22-aa-bb-cc") // invented
	if err != nil || s.MAC != "00:11:22:AA:BB:CC" {
		t.Errorf("got %+v %v, want the address as ambientweather.net writes it", s, err)
	}
	for _, bad := range []string{"00:11:22:aa:bb", "station", "00:11:22:aa:bb:cc:dd:ee"} {
		_, err := parseStation(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q repeats the address", err)
		}
	}
}
