package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A dashboard as gravel defines it: rows of panels, each panel one or more PromQL targets.
// gen renders it as Grafana's dashboard JSON; rules and check read the JSON back, so what they
// check is exactly what ships.
type dashboard struct {
	UID, Title, Description string
	BuildInfo               string // the build-info metric the job variable lists jobs from
	Rows                    []row
}

type row struct {
	Title  string
	Panels []panel
}

type panel struct {
	Title, Description string
	Kind               string // "stat" or "timeseries"
	Unit               string
	Width, Height      int
	Targets            []target
	Mappings           []valueMap // stat: value → text and colour
	Thresholds         []step     // ascending; the first step's value is ignored (the base)
	ThresholdLine      bool       // timeseries: draw the thresholds as lines
	TextMode           string     // stat: "value" (default) or "name" (show the legend)
	// Optional says why a panel may have no data on a healthy deployment (a feature not
	// configured, nothing happened yet). check reports such a panel instead of failing it.
	Optional string
}

type target struct{ Expr, Legend string }

type valueMap struct {
	Value       string
	Text, Color string
}

type step struct {
	Value float64
	Color string
}

// sel is the selector every gravel series gets: the chosen job, the chosen instances. job and
// instance are Prometheus's own target labels: a metric's label of the same name is stored as
// exported_job (exported_instance), so a panel groups and labels by that (the job runners'
// gravel_hub_jobs_total and gravel_bot_jobs_total).
const sel = `job="$job", instance=~"$instance"`

// render builds Grafana's dashboard model (schemaVersion 41) with a datasource variable and
// job/instance variables, so nothing names a datasource uid or a job.
func (d dashboard) render() map[string]any {
	ds := map[string]any{"type": "prometheus", "uid": "${datasource}"}
	var panels []any
	id, y := 1, 0
	for _, r := range d.Rows {
		panels = append(panels, map[string]any{
			"id": id, "type": "row", "title": r.Title, "collapsed": false,
			"gridPos": map[string]any{"x": 0, "y": y, "w": 24, "h": 1}, "panels": []any{},
		})
		id++
		y++
		x, rowH := 0, 0
		for _, p := range r.Panels {
			w, h := p.Width, p.Height
			if w == 0 {
				w = 6
			}
			if h == 0 {
				h = 4
				if p.Kind == "timeseries" {
					h = 8
				}
			}
			if x+w > 24 {
				x, y = 0, y+rowH
				rowH = 0
			}
			panels = append(panels, p.render(id, ds, x, y, w, h))
			id++
			x += w
			if h > rowH {
				rowH = h
			}
		}
		y += rowH
	}
	return map[string]any{
		"uid": d.UID, "title": d.Title, "description": d.Description,
		"tags": []string{"gravel"}, "editable": false, "schemaVersion": 41,
		"time": map[string]any{"from": "now-6h", "to": "now"}, "refresh": "1m",
		"timezone": "browser", "graphTooltip": 1,
		"templating": map[string]any{"list": []any{
			map[string]any{"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus", "hide": 0},
			map[string]any{
				"name": "job", "label": "Job", "type": "query", "datasource": ds, "refresh": 2, "sort": 1, "hide": 0,
				"query":      map[string]any{"query": "label_values(" + d.BuildInfo + ", job)", "refId": "job"},
				"definition": "label_values(" + d.BuildInfo + ", job)",
			},
			map[string]any{
				"name": "instance", "label": "Instance", "type": "query", "datasource": ds, "refresh": 2, "sort": 1, "hide": 0,
				"multi": true, "includeAll": true, "allValue": ".+",
				"current":    map[string]any{"text": "All", "value": "$__all"},
				"query":      map[string]any{"query": "label_values(" + d.BuildInfo + `{job="$job"}, instance)`, "refId": "instance"},
				"definition": "label_values(" + d.BuildInfo + `{job="$job"}, instance)`,
			},
		}},
		"annotations": map[string]any{"list": []any{}},
		"panels":      panels,
	}
}

func (p panel) render(id int, ds map[string]any, x, y, w, h int) map[string]any {
	var targets []any
	for i, t := range p.Targets {
		tg := map[string]any{"refId": string(rune('A' + i)), "datasource": ds, "expr": t.Expr, "range": true}
		if t.Legend != "" {
			tg["legendFormat"] = t.Legend
		}
		targets = append(targets, tg)
	}
	steps := []any{map[string]any{"color": "green", "value": nil}}
	if len(p.Thresholds) > 0 {
		steps = []any{map[string]any{"color": p.Thresholds[0].Color, "value": nil}}
		for _, s := range p.Thresholds[1:] {
			steps = append(steps, map[string]any{"color": s.Color, "value": s.Value})
		}
	}
	defaults := map[string]any{
		"unit":       p.Unit,
		"thresholds": map[string]any{"mode": "absolute", "steps": steps},
		"color":      map[string]any{"mode": "thresholds"},
	}
	if p.Kind == "timeseries" {
		defaults["color"] = map[string]any{"mode": "palette-classic"}
		custom := map[string]any{"drawStyle": "line", "lineWidth": 1, "fillOpacity": 10, "showPoints": "never", "spanNulls": false}
		if p.ThresholdLine {
			custom["thresholdsStyle"] = map[string]any{"mode": "line"}
		}
		defaults["custom"] = custom
	}
	if len(p.Mappings) > 0 {
		opts := map[string]any{}
		for i, m := range p.Mappings {
			opts[m.Value] = map[string]any{"text": m.Text, "color": m.Color, "index": i}
		}
		defaults["mappings"] = []any{map[string]any{"type": "value", "options": opts}}
	}
	out := map[string]any{
		"id": id, "type": p.Kind, "title": p.Title, "datasource": ds, "targets": targets,
		"gridPos":     map[string]any{"x": x, "y": y, "w": w, "h": h},
		"fieldConfig": map[string]any{"defaults": defaults, "overrides": []any{}},
	}
	desc := p.Description
	if p.Optional != "" {
		out["gravelOptional"] = p.Optional
		if desc != "" {
			desc += " "
		}
		desc += "No data is expected when " + p.Optional + "."
	}
	if desc != "" {
		out["description"] = desc
	}
	switch p.Kind {
	case "stat":
		textMode := "value"
		if p.TextMode != "" {
			textMode = p.TextMode
		}
		out["options"] = map[string]any{
			"reduceOptions": map[string]any{"calcs": []string{"lastNotNull"}, "fields": "", "values": false},
			"textMode":      textMode, "colorMode": "value", "graphMode": "none", "justifyMode": "auto", "orientation": "auto",
		}
	case "timeseries":
		out["options"] = map[string]any{
			"legend":  map[string]any{"displayMode": "list", "placement": "bottom", "showLegend": true},
			"tooltip": map[string]any{"mode": "multi", "sort": "desc"},
		}
	}
	return out
}

// The model gen writes and rules/check read back.

type fileDashboard struct {
	UID        string      `json:"uid"`
	Title      string      `json:"title"`
	Panels     []filePanel `json:"panels"`
	Templating struct {
		List []struct {
			Name       string `json:"name"`
			Definition string `json:"definition"`
		} `json:"list"`
	} `json:"templating"`
}

type filePanel struct {
	ID       int          `json:"id"`
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Optional string       `json:"gravelOptional"`
	Targets  []fileTarget `json:"targets"`
}

type fileTarget struct {
	RefID string `json:"refId"`
	Expr  string `json:"expr"`
}

func readDashboards(dir string) ([]fileDashboard, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no dashboards in %s", dir)
	}
	sort.Strings(paths)
	var out []fileDashboard
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var d fileDashboard
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// expand replaces the dashboard variables and Grafana's interval macros so an expression is
// plain PromQL.
func expand(expr string, vars map[string]string) string {
	r := strings.NewReplacer(
		"$__rate_interval", "5m", "$__interval", "1m", "$__range", "1h",
		`"$job"`, `"`+vars["job"]+`"`, `"$instance"`, `"`+vars["instance"]+`"`,
	)
	return r.Replace(expr)
}

var metricName = regexp.MustCompile(`\b(gravel_[a-z0-9_]+)\b`)

// metricNames returns the gravel_* names an expression reads, with histogram suffixes removed.
func metricNames(expr string) []string {
	var out []string
	for _, m := range metricName.FindAllString(expr, -1) {
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(m, "_seconds"+suffix) {
				m = strings.TrimSuffix(m, suffix)
			}
		}
		out = append(out, m)
	}
	return out
}
