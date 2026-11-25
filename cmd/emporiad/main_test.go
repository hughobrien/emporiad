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
