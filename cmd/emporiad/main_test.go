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
