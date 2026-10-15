package dashboards

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Dashboard is one as gravel defines it: rows of panels, each panel one or more PromQL targets.
// Grafana renders it as Grafana's dashboard JSON; the generator's rules and check read that JSON
// back, so what they check is exactly what ships.
type Dashboard struct {
	UID, Title, Description string
	BuildInfo               string // the build-info metric the job variable lists jobs from
	Rows                    []Row
}

type Row struct {
	Title  string
	Panels []Panel
}

type Panel struct {
	Title, Description string
	Kind               string // "stat" or "timeseries"
	Unit               string
	Width, Height      int
	Targets            []Target
	Mappings           []ValueMap // stat: value → text and colour
	Thresholds         []Step     // ascending; the first step's value is ignored (the base)
	ThresholdLine      bool       // timeseries: draw the thresholds as lines
	TextMode           string     // stat: "value" (default) or "name" (show the legend)
	// Optional says why a panel may have no data on a healthy deployment (a feature not
	// configured, nothing happened yet). check reports such a panel instead of failing it.
	Optional string
}

type Target struct{ Expr, Legend string }

type ValueMap struct {
	Value       string
	Text, Color string
}

type Step struct {
	Value float64
	Color string
}

// sel is the selector every gravel series gets: the chosen job, the chosen instances. job and
// instance are Prometheus's own target labels: a metric's label of the same name is stored as
// exported_job (exported_instance), so a panel groups and labels by that (the job runners'
// gravel_hub_jobs_total and gravel_bot_jobs_total).
const Sel = `job="$job", instance=~"$instance"`

// Grafana builds Grafana's dashboard model (schemaVersion 41) with a datasource variable and
// job/instance variables, so nothing names a datasource uid or a job.
func (d Dashboard) Grafana() map[string]any {
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

func (p Panel) render(id int, ds map[string]any, x, y, w, h int) map[string]any {
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

// expand replaces the dashboard variables and Grafana's interval macros so an expression is
// plain PromQL.
func Expand(expr string, vars map[string]string) string {
	r := strings.NewReplacer(
		"$__rate_interval", "5m", "$__interval", "1m", "$__range", "1h",
		`"$job"`, `"`+vars["job"]+`"`, `"$instance"`, `"`+vars["instance"]+`"`,
	)
	return r.Replace(expr)
}

var metricName = regexp.MustCompile(`\b(gravel_[a-z0-9_]+)\b`)

// metricNames returns the gravel_* names an expression reads, with histogram suffixes removed.
func MetricNames(expr string) []string {
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

// ExpandFor makes a definition's expression plain PromQL for a range query the hub runs itself
// (gravel#204): $job is the scrape job (quoted as PromQL quotes strings), $instance every instance,
// $__rate_interval at least four 15 s scrapes and two steps, $__interval the step, $__range the
// range. Nothing in it comes from the viewer.
func ExpandFor(expr, job string, step, span time.Duration) string {
	r := strings.NewReplacer(
		"$__rate_interval", promDuration(max(time.Minute, 2*step)), "$__interval", promDuration(step), "$__range", promDuration(span),
		`"$job"`, strconv.Quote(job), `"$instance"`, `".+"`,
	)
	return r.Replace(expr)
}

// promDuration is a duration as PromQL writes one, in whole seconds (at least 1s).
func promDuration(d time.Duration) string {
	return strconv.Itoa(max(1, int(d.Round(time.Second).Seconds()))) + "s"
}

// Binary is the binary a dashboard reads: "hub" or "bot" (its scrape job, config Metrics.Job).
func (d Dashboard) Binary() string {
	if d.UID == "gravel-bot" {
		return "bot"
	}
	return "hub"
}
