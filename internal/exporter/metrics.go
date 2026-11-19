package exporter

import (
	"io"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	powerDesc = prometheus.NewDesc("emporia_power_watts",
		"Average power over the last complete minute. channel_num 1,2,3 is the mains, Balance is what no circuit sensor covers.",
		[]string{"device", "model", "channel", "channel_num"}, nil)
	costDesc = prometheus.NewDesc("emporia_cost_per_hour",
		"Cost of the last complete minute, as a rate per hour, priced by Emporia with the utility rate plan selected in the app (the flat price without one). Same labels as emporia_power_watts.",
		[]string{"device", "model", "channel", "channel_num"}, nil)
	priceDesc = prometheus.NewDesc("emporia_price_per_kwh",
		"Flat energy price set in the Emporia app, in the account's currency. Not used for emporia_cost_per_hour when a rate plan is selected.",
		[]string{"device", "model"}, nil)
	connectedDesc = prometheus.NewDesc("emporia_device_connected",
		"1 if Emporia's cloud says the device is online.",
		[]string{"device", "model"}, nil)
	infoDesc = prometheus.NewDesc("emporia_device_info",
		"Device model and firmware.",
		[]string{"device", "model", "firmware"}, nil)
	lastOKDesc = prometheus.NewDesc("emporia_last_success_timestamp_seconds",
		"Unix time of the last successful usage poll.", nil, nil)
	errorsDesc = prometheus.NewDesc("emporia_poll_errors_total",
		"Failed API calls since emporiad started, by stage (devices, usage, cost).", []string{"stage"}, nil)
)

// descs lists every family the collector emits (the dashboard test reads it).
var descs = []*prometheus.Desc{powerDesc, costDesc, priceDesc, connectedDesc, infoDesc, lastOKDesc, errorsDesc}

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
	for _, p := range s.power {
		ch <- prometheus.MustNewConstMetric(powerDesc, g, p.watts, p.device, p.model, p.channel, p.num)
		if p.costPerHour != nil {
			ch <- prometheus.MustNewConstMetric(costDesc, g, *p.costPerHour, p.device, p.model, p.channel, p.num)
		}
	}
	if !s.lastOK.IsZero() {
		ch <- prometheus.MustNewConstMetric(lastOKDesc, g, float64(s.lastOK.Unix()))
	}
	for _, stage := range []string{"devices", "usage", "cost"} {
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
