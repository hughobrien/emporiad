package main

import (
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
