// Package weather reads outdoor temperature from a weather station shared on
// ambientweather.net, which needs no account or key.
package weather

import (
	"net/http"
	"time"
)

var httpClient = http.Client{Timeout: 60 * time.Second}

// Reading is an outdoor temperature at T, whenever the station reported.
type Reading struct {
	T       time.Time
	Celsius float64
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
