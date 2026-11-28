package exporter

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// The Grafana dashboard in grafana/emporiad.json queries these metrics. If a
// metric is renamed or dropped the dashboard silently shows nothing, so check
// every emporia_ and weather_ name it uses is still served or pushed, and that it stays importable
// (datasource is an input, not a UID from one particular Grafana).
func TestDashboardMatchesMetrics(t *testing.T) {
	raw, err := os.ReadFile("../../grafana/emporiad.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}

	have := map[string]bool{}
	name := regexp.MustCompile(`fqName: "([a-z_]+)"`)
	for _, d := range descs {
		if g := name.FindStringSubmatch(d.String()); g != nil {
			have[g[1]] = true
		}
	}
	if len(have) != len(descs) {
		t.Fatal("could not read every metric name; the pattern for Desc.String() is stale")
	}
	for _, n := range Pushed {
		have[n] = true
	}

	used := map[string]bool{}
	use := regexp.MustCompile(`\b(?:emporia|weather)_[a-z_]+\b`)
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			// Panel queries are "expr"; a variable's is "query" (a string
			// inside its "query" object) and "definition".
			for _, k := range []string{"expr", "query", "definition"} {
				if e, ok := x[k].(string); ok {
					for _, n := range use.FindAllString(e, -1) {
						used[n] = true
					}
				}
			}
			if uid, ok := x["uid"].(string); ok && x["type"] == "prometheus" && uid != "${DS_PROMETHEUS}" {
				t.Errorf("datasource uid %q is tied to one Grafana; use ${DS_PROMETHEUS}", uid)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(dash)
	if len(used) == 0 {
		t.Fatal("the dashboard queries no emporia_ metrics")
	}
	for n := range used {
		if !have[n] {
			t.Errorf("dashboard uses %s, which the exporter does not export", n)
		}
	}
	if _, ok := dash["uid"]; ok {
		t.Error("drop the dashboard uid so imports do not collide")
	}
}
