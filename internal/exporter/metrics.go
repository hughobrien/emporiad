package exporter

import (
	"io"
	"net/http"
	"strings"

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
		"Unix time at the end of the newest minute fetched from Emporia.", nil, nil)
	pushedDesc = prometheus.NewDesc("emporia_pushed_minute_timestamp_seconds",
		"Unix time at the end of the newest minute a push target took.", []string{"target"}, nil)
	pushErrorsDesc = prometheus.NewDesc("emporia_push_errors_total",
		"Failed remote writes since emporiad started, by target.", []string{"target"}, nil)
	errorsDesc = prometheus.NewDesc("emporia_poll_errors_total",
		"Failed calls since emporiad started, by stage (devices, resume, usage, cost, push, weather).", []string{"stage"}, nil)
)

// descs lists the health and device families on /metrics (the dashboard
// test reads it); the Pushed families are served too, by readingDescs.
var descs = []*prometheus.Desc{priceDesc, connectedDesc, infoDesc, lastOKDesc, newestDesc, pushedDesc, pushErrorsDesc, errorsDesc}

var readingHelp = map[string]string{
	"emporia_power_watts":       "Average power over the minute ending at the sample's time.",
	"emporia_hourly_energy_kwh": "Energy in the hour ending at the sample's time.",
	"emporia_hourly_cost":       "Cost of the hour ending at the sample's time, priced by the app's rate plan.",
	weatherName:                 "Outdoor temperature 2 m above ground at the sample's time, from Open-Meteo.",
}

// readingLabels are the labels a pushed family is served with on /metrics.
func readingLabels(name string) []string {
	if name == weatherName {
		return []string{"source"}
	}
	return []string{"device", "model", "channel", "channel_num"}
}

// readingDescs describe the newest pushed samples as served on /metrics,
// labelled pushed_<target>="true" so each target's own scrape can drop the
// copies it already has. Without job: the scraper sets that.
func readingDescs(pushed map[string]string) map[string]*prometheus.Desc {
	ds := map[string]*prometheus.Desc{}
	for _, n := range Pushed {
		ds[n] = prometheus.NewDesc(n, readingHelp[n], readingLabels(n), pushed)
	}
	return ds
}

type collector struct {
	e        *Exporter
	readings map[string]*prometheus.Desc
}

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descs {
		ch <- d
	}
	for _, d := range c.readings {
		ch <- d
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// once passes each metric on a single time per collection. A name and label
// values seen again would make promhttp answer 500 for the whole page, so the
// later copy is dropped and logged (once per process for each, not on every
// scrape: nameDevices should make this impossible).
type once struct {
	ch   chan<- prometheus.Metric
	e    *Exporter
	seen map[string]bool
}

func (o once) send(d *prometheus.Desc, m prometheus.Metric, labels ...string) {
	k := d.String() + "\x00" + strings.Join(labels, "\x00")
	if o.seen[k] {
		o.e.mu.Lock()
		first := !o.e.warned[k]
		o.e.warned[k] = true
		o.e.mu.Unlock()
		if first {
			o.e.log.Warn("duplicate metric skipped on /metrics", "desc", d.String(), "labels", labels)
		}
		return
	}
	o.seen[k] = true
	o.ch <- m
}

func (c collector) Collect(out chan<- prometheus.Metric) {
	s := c.e.snapshot()
	g := prometheus.GaugeValue
	o := once{out, c.e, map[string]bool{}}
	send := func(d *prometheus.Desc, t prometheus.ValueType, v float64, labels ...string) {
		o.send(d, prometheus.MustNewConstMetric(d, t, v, labels...), labels...)
	}
	for _, d := range s.devices {
		send(infoDesc, g, 1, d.Name, d.Model, d.Firmware)
		if d.Parent == 0 && d.Price > 0 {
			send(priceDesc, g, d.Price, d.Name, d.Model)
		}
		if d.Connected != nil {
			send(connectedDesc, g, b2f(*d.Connected), d.Name, d.Model)
		}
	}
	if !s.lastOK.IsZero() {
		send(lastOKDesc, g, float64(s.lastOK.Unix()))
	}
	if !s.newest.IsZero() {
		send(newestDesc, g, float64(s.newest.Unix()))
	}
	for _, t := range s.targets {
		if !t.newest.IsZero() {
			send(pushedDesc, g, float64(t.newest.Unix()), t.name)
		}
		send(pushErrorsDesc, prometheus.CounterValue, float64(t.errors), t.name)
	}
	for _, r := range s.latest {
		l, p := r.Labels, r.Samples[0]
		d := c.readings[l["__name__"]]
		var vals []string
		for _, k := range readingLabels(l["__name__"]) {
			vals = append(vals, l[k])
		}
		o.send(d, prometheus.NewMetricWithTimestamp(p.T, prometheus.MustNewConstMetric(d, g, p.V, vals...)), vals...)
	}
	for _, stage := range stages {
		send(errorsDesc, prometheus.CounterValue, float64(s.errors[stage]), stage)
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
		collector{e, readingDescs(e.pushed)},
	)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}
