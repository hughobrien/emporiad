package exporter

import (
	"io"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	priceDesc = prometheus.NewDesc("emporia_price_per_kwh",
		"Flat energy price set in the Emporia app, in the account's currency. Not used for emporia_hourly_cost when a rate plan is selected.",
		[]string{"device", "model"}, nil)
	connectedDesc = prometheus.NewDesc("emporia_device_connected",
		"1 if Emporia's cloud says the device is online.",
		[]string{"device", "model"}, nil)
	infoDesc = prometheus.NewDesc("emporia_device_info",
		"Device model and firmware.",
		[]string{"device", "model", "firmware"}, nil)
	lastOKDesc = prometheus.NewDesc("emporia_last_success_timestamp_seconds",
		"Unix time of the last poll in which every call succeeded.", nil, nil)
	newestDesc = prometheus.NewDesc("emporia_newest_minute_timestamp_seconds",
		"Unix time at the end of the newest minute pushed to Prometheus.", nil, nil)
	errorsDesc = prometheus.NewDesc("emporia_poll_errors_total",
		"Failed calls since emporiad started, by stage (devices, resume, usage, cost, push).", []string{"stage"}, nil)
)

// descs lists every family on /metrics (the dashboard test reads it; see also Pushed).
var descs = []*prometheus.Desc{priceDesc, connectedDesc, infoDesc, lastOKDesc, newestDesc, errorsDesc}

type collector struct{ e *Exporter }

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descs {
		ch <- d
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	s := c.e.snapshot()
	g := prometheus.GaugeValue
	for _, d := range s.devices {
		ch <- prometheus.MustNewConstMetric(infoDesc, g, 1, d.Name, d.Model, d.Firmware)
		if d.Parent == 0 && d.Price > 0 {
			ch <- prometheus.MustNewConstMetric(priceDesc, g, d.Price, d.Name, d.Model)
		}
		if d.Connected != nil {
			ch <- prometheus.MustNewConstMetric(connectedDesc, g, b2f(*d.Connected), d.Name, d.Model)
		}
	}
	if !s.lastOK.IsZero() {
		ch <- prometheus.MustNewConstMetric(lastOKDesc, g, float64(s.lastOK.Unix()))
	}
	if !s.newest.IsZero() {
		ch <- prometheus.MustNewConstMetric(newestDesc, g, float64(s.newest.Unix()))
	}
	for _, stage := range stages {
		ch <- prometheus.MustNewConstMetric(errorsDesc, prometheus.CounterValue, float64(s.errors[stage]), stage)
	}
}

// Handler serves /metrics and /healthz.
func (e *Exporter) Handler(version string) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "emporiad_build_info",
			Help:        "emporiad version.",
			ConstLabels: prometheus.Labels{"version": version},
		}, func() float64 { return 1 }),
		collector{e},
	)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}
