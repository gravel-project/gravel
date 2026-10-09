package main

// The dashboards gravel ships (ADR-0009). Every gravel_* metric the hub or the bot registers is
// on one of them; dashboards_test.go fails when one is missing or a panel reads a name gravel
// doesn't register.

var (
	upMap = []valueMap{{Value: "0", Text: "Down", Color: "red"}, {Value: "1", Text: "Up", Color: "green"}}
	okMap = []valueMap{{Value: "0", Text: "No", Color: "red"}, {Value: "1", Text: "Yes", Color: "green"}}
)

func rate(metric, by, extra string) string {
	s := sel
	if extra != "" {
		s += ", " + extra
	}
	return "sum by (" + by + ") (rate(" + metric + "{" + s + "}[$__rate_interval]))"
}

func quantile(q, metric, by string) string {
	return "histogram_quantile(" + q + ", sum by (le, " + by + ") (rate(" + metric + "_bucket{" + sel + "}[$__rate_interval])))"
}

// errorRatio is 0, not "no data", while there are requests and no errors.
func errorRatio(metric, by, errMatch string) string {
	errs := rate(metric, by, errMatch)
	all := rate(metric, by, "")
	return "(" + errs + " or " + all + " * 0) / " + all
}

func runtimeRow() row {
	return row{Title: "Runtime", Panels: []panel{
		{Title: "Goroutines", Kind: "timeseries", Width: 8, Targets: []target{{Expr: `go_goroutines{` + sel + `}`, Legend: "{{instance}}"}}},
		{Title: "Heap in use", Kind: "timeseries", Unit: "bytes", Width: 8, Targets: []target{{Expr: `go_memstats_heap_inuse_bytes{` + sel + `}`, Legend: "{{instance}}"}}},
		{Title: "Resident memory", Kind: "timeseries", Unit: "bytes", Width: 8, Targets: []target{{Expr: `process_resident_memory_bytes{` + sel + `}`, Legend: "{{instance}}"}}},
		{Title: "CPU", Kind: "timeseries", Unit: "percentunit", Width: 8, Targets: []target{{Expr: `rate(process_cpu_seconds_total{` + sel + `}[$__rate_interval])`, Legend: "{{instance}}"}}},
		{Title: "GC pause per second", Kind: "timeseries", Unit: "s", Width: 8, Targets: []target{{Expr: `rate(go_gc_duration_seconds_sum{` + sel + `}[$__rate_interval])`, Legend: "{{instance}}"}}},
		{Title: "Open file descriptors", Kind: "timeseries", Width: 8, Targets: []target{
			{Expr: `process_open_fds{` + sel + `}`, Legend: "open {{instance}}"},
			{Expr: `process_max_fds{` + sel + `}`, Legend: "limit {{instance}}"},
		}},
	}}
}

func hubDashboard() dashboard {
	return dashboard{
		UID: "gravel-hub", Title: "gravel hub", BuildInfo: "gravel_build_info",
		Description: "The gravel hub: members, HTTP and API traffic, logins, backups and the Go runtime (gravel docs/hub.md, Observability).",
		Rows: []row{
			{Title: "Overview", Panels: []panel{
				{Title: "Up", Kind: "stat", Width: 3, Mappings: upMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}}, Targets: []target{{Expr: `up{` + sel + `}`, Legend: "{{instance}}"}}},
				{Title: "Version", Kind: "stat", Width: 5, TextMode: "name", Targets: []target{{Expr: `gravel_build_info{` + sel + `}`, Legend: "{{version}} ({{go_version}})"}}},
				{Title: "Members", Kind: "stat", Width: 4, Targets: []target{{Expr: `sum(gravel_users{` + sel + `})`}}},
				{Title: "Linked identities", Kind: "stat", Width: 6, TextMode: "value_and_name", Optional: "no login provider is configured (auth)", Targets: []target{{Expr: `sum by (provider) (gravel_identities{` + sel + `})`, Legend: "{{provider}}"}}},
				{Title: "Database size", Kind: "stat", Unit: "bytes", Width: 3, Targets: []target{{Expr: `sum(gravel_database_size_bytes{` + sel + `})`}}},
				{Title: "Counts readable", Kind: "stat", Width: 3, Mappings: okMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}},
					Description: "0 when the database did not answer the scrape's count queries in time.",
					Targets:     []target{{Expr: `min(gravel_store_stats_readable{` + sel + `})`}}},
				{Title: "Members over time", Kind: "timeseries", Width: 12, Targets: []target{{Expr: `sum(gravel_users{` + sel + `})`, Legend: "members"}}},
				{Title: "Linked identities over time", Kind: "timeseries", Width: 12, Optional: "no login provider is configured (auth)", Targets: []target{{Expr: `sum by (provider) (gravel_identities{` + sel + `})`, Legend: "{{provider}}"}}},
			}},
			{Title: "HTTP", Panels: []panel{
				{Title: "Requests by handler", Kind: "timeseries", Unit: "reqps", Width: 8, Targets: []target{{Expr: rate("gravel_http_requests_total", "handler", ""), Legend: "{{handler}}"}}},
				{Title: "5xx ratio by handler", Kind: "timeseries", Unit: "percentunit", Width: 8, Targets: []target{{Expr: errorRatio("gravel_http_requests_total", "handler", `code=~"5.."`), Legend: "{{handler}}"}}},
				{Title: "Latency by handler", Kind: "timeseries", Unit: "s", Width: 8, Targets: []target{
					{Expr: quantile("0.5", "gravel_http_request_duration_seconds", "handler"), Legend: "p50 {{handler}}"},
					{Expr: quantile("0.95", "gravel_http_request_duration_seconds", "handler"), Legend: "p95 {{handler}}"},
					{Expr: quantile("0.99", "gravel_http_request_duration_seconds", "handler"), Legend: "p99 {{handler}}"},
				}},
			}},
			{Title: "API", Panels: []panel{
				{Title: "Calls by procedure", Kind: "timeseries", Unit: "reqps", Width: 8, Description: "Includes the pages' in-process calls (ADR-0005).",
					Targets: []target{{Expr: rate("gravel_rpc_requests_total", "procedure", ""), Legend: "{{procedure}}"}}},
				{Title: "Errors by procedure and code", Kind: "timeseries", Unit: "reqps", Width: 8, Optional: "no call has failed in the range",
					Targets: []target{{Expr: rate("gravel_rpc_requests_total", "procedure, code", `code!="ok"`), Legend: "{{procedure}} {{code}}"}}},
				{Title: "p95 latency by procedure", Kind: "timeseries", Unit: "s", Width: 8,
					Targets: []target{{Expr: quantile("0.95", "gravel_rpc_request_duration_seconds", "procedure"), Legend: "{{procedure}}"}}},
			}},
			{Title: "Login", Panels: []panel{
				{Title: "Login, link and Linked Roles attempts", Kind: "timeseries", Width: 12, Optional: "nobody logged in or linked in the range",
					Targets: []target{{Expr: `sum by (provider, intent, result) (increase(gravel_auth_completions_total{` + sel + `}[$__rate_interval]))`, Legend: "{{provider}} {{intent}} {{result}}"}}},
				{Title: "Refused by a rate limit", Kind: "timeseries", Width: 12, Optional: "no request was rate limited in the range",
					Targets: []target{{Expr: `sum by (scope) (increase(gravel_rate_limited_total{` + sel + `}[$__rate_interval]))`, Legend: "{{scope}}"}}},
			}},
			{Title: "Backups", Panels: []panel{
				{Title: "Since the last base backup", Kind: "stat", Unit: "s", Width: 4, Optional: "backups are not configured (backup.status_file) or none has succeeded yet",
					Description: "Red after 26 hours: the nightly backup missed a night.",
					Thresholds:  []step{{Color: "green"}, {Value: 93600, Color: "red"}},
					Targets:     []target{{Expr: `time() - max(gravel_backup_last_success_timestamp_seconds{` + sel + `})`}}},
				{Title: "Since the last run", Kind: "stat", Unit: "s", Width: 4, Optional: "backups are not configured or have never run",
					Targets: []target{{Expr: `time() - max(gravel_backup_last_run_timestamp_seconds{` + sel + `})`}}},
				{Title: "Last run succeeded", Kind: "stat", Width: 4, Mappings: okMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}}, Optional: "backups are not configured or have never run",
					Targets: []target{{Expr: `min(gravel_backup_last_run_ok{` + sel + `})`}}},
				{Title: "Status file readable", Kind: "stat", Width: 4, Mappings: okMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}}, Optional: "backups are not configured (backup.status_file is empty)",
					Targets: []target{{Expr: `min(gravel_backup_status_readable{` + sel + `})`}}},
				{Title: "Since the last WAL segment archived", Kind: "stat", Unit: "s", Width: 4, Optional: "WAL archiving is off or has archived nothing yet",
					Description: "Red after 10 minutes: the archive trails the database by more than archive_timeout allows.",
					Thresholds:  []step{{Color: "green"}, {Value: 600, Color: "red"}},
					Targets:     []target{{Expr: `time() - max(gravel_wal_last_archived_timestamp_seconds{` + sel + `})`}}},
				{Title: "Archiver readable", Kind: "stat", Width: 4, Mappings: okMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}},
					Targets: []target{{Expr: `min(gravel_wal_archiver_readable{` + sel + `})`}}},
				{Title: "WAL segments archived", Kind: "timeseries", Width: 12, Optional: "WAL archiving is off",
					Targets: []target{{Expr: `sum(increase(gravel_wal_archived_total{` + sel + `}[$__rate_interval]))`, Legend: "archived"}}},
				{Title: "WAL archive failures", Kind: "timeseries", Width: 8,
					Targets: []target{{Expr: `sum(increase(gravel_wal_archive_failed_total{` + sel + `}[$__rate_interval]))`, Legend: "failed"}}},
				{Title: "Since the last WAL failure", Kind: "stat", Unit: "s", Width: 4, Optional: "no WAL upload has ever failed",
					Targets: []target{{Expr: `time() - max(gravel_wal_last_failed_timestamp_seconds{` + sel + `})`}}},
			}},
			runtimeRow(),
		},
	}
}

func botDashboard() dashboard {
	return dashboard{
		UID: "gravel-bot", Title: "gravel bot", BuildInfo: "gravel_bot_build_info",
		Description: "The gravel Discord bot: the gateway, interactions against Discord's three seconds, role sync, Linked Roles and the Go runtime (gravel docs/bot.md, Observability).",
		Rows: []row{
			{Title: "Overview", Panels: []panel{
				{Title: "Up", Kind: "stat", Width: 3, Mappings: upMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}}, Targets: []target{{Expr: `up{` + sel + `}`, Legend: "{{instance}}"}}},
				{Title: "Version", Kind: "stat", Width: 5, TextMode: "name", Targets: []target{{Expr: `gravel_bot_build_info{` + sel + `}`, Legend: "{{version}} ({{go_version}})"}}},
				{Title: "Gateway", Kind: "stat", Width: 4, Mappings: []valueMap{{Value: "0", Text: "Down", Color: "red"}, {Value: "1", Text: "Ready", Color: "green"}},
					Thresholds:  []step{{Color: "red"}, {Value: 1, Color: "green"}},
					Description: "Ready while the gateway session is up; also 0 when the bot runs without the gateway.",
					Targets:     []target{{Expr: `min(gravel_bot_gateway_connected{` + sel + `})`}}},
				{Title: "Gateway latency", Kind: "stat", Unit: "s", Width: 4, Optional: "the gateway is off or not ready",
					Thresholds: []step{{Color: "green"}, {Value: 0.5, Color: "orange"}, {Value: 1, Color: "red"}},
					Targets:    []target{{Expr: `max(gravel_bot_gateway_latency_seconds{` + sel + `})`}}},
				{Title: "Linked Roles schema", Kind: "stat", Width: 4, Mappings: []valueMap{{Value: "0", Text: "Not registered", Color: "orange"}, {Value: "1", Text: "Registered", Color: "green"}},
					Thresholds: []step{{Color: "orange"}, {Value: 1, Color: "green"}}, Optional: "Linked Roles are off (linked_roles.enabled)",
					Targets: []target{{Expr: `min(gravel_bot_linked_roles_schema_registered{` + sel + `})`}}},
				{Title: "Since the last role sync pass", Kind: "stat", Unit: "s", Width: 4, Optional: "role sync is off",
					Description: "Orange after 20 minutes and red after 30 (two and three default intervals).",
					Thresholds:  []step{{Color: "green"}, {Value: 1200, Color: "orange"}, {Value: 1800, Color: "red"}},
					Targets:     []target{{Expr: `time() - max(gravel_bot_rolesync_last_success_timestamp_seconds{` + sel + `})`}}},
				{Title: "Gateway over time", Kind: "timeseries", Width: 12, Targets: []target{
					{Expr: `min(gravel_bot_gateway_connected{` + sel + `})`, Legend: "ready"},
				}},
				{Title: "Gateway heartbeat latency", Kind: "timeseries", Unit: "s", Width: 12, Optional: "the gateway is off or not ready",
					Targets: []target{{Expr: `max(gravel_bot_gateway_latency_seconds{` + sel + `})`, Legend: "latency"}}},
			}},
			{Title: "Interactions", Panels: []panel{
				{Title: "Interactions by route and result", Kind: "timeseries", Width: 8, Optional: "no interaction arrived in the range",
					Targets: []target{{Expr: `sum by (route, result) (increase(gravel_bot_interactions_total{` + sel + `}[$__rate_interval]))`, Legend: "{{route}} {{result}}"}}},
				{Title: "Interaction latency", Kind: "timeseries", Unit: "s", Width: 8, ThresholdLine: true, Optional: "no interaction arrived in the range",
					Description: "Discord waits three seconds for the first answer; the red line is that deadline.",
					Thresholds:  []step{{Color: "green"}, {Value: 3, Color: "red"}},
					Targets: []target{
						{Expr: quantile("0.95", "gravel_bot_interaction_duration_seconds", "route"), Legend: "p95 {{route}}"},
						{Expr: quantile("0.99", "gravel_bot_interaction_duration_seconds", "route"), Legend: "p99 {{route}}"},
					}},
				{Title: "Interaction error ratio", Kind: "timeseries", Unit: "percentunit", Width: 8, Optional: "no interaction arrived in the range",
					Targets: []target{{Expr: errorRatio("gravel_bot_interactions_total", "route", `result="error"`), Legend: "{{route}}"}}},
			}},
			{Title: "Role sync and jobs", Panels: []panel{
				{Title: "Role sync passes", Kind: "timeseries", Width: 8, Optional: "role sync is off",
					Description: "idle means no mapping: the pass had nothing to do.",
					Targets:     []target{{Expr: `sum by (result) (increase(gravel_bot_rolesync_passes_total{` + sel + `}[$__rate_interval]))`, Legend: "{{result}}"}}},
				{Title: "Role changes", Kind: "timeseries", Width: 8, Optional: "role sync is off or changed nothing in the range",
					Description: "forbidden: the role sits above the bot's; gone: the member left first; dry_run: logged, not made.",
					Targets:     []target{{Expr: `sum by (action, result) (increase(gravel_bot_rolesync_changes_total{` + sel + `}[$__rate_interval]))`, Legend: "{{action}} {{result}}"}}},
				{Title: "Background jobs ended", Kind: "timeseries", Width: 8, Optional: "no job ended in the range (jobs run until shutdown)",
					Targets: []target{{Expr: `sum by (job, result) (increase(gravel_bot_jobs_total{` + sel + `}[$__rate_interval]))`, Legend: "{{job}} {{result}}"}}},
			}},
			runtimeRow(),
		},
	}
}

func dashboards() []dashboard { return []dashboard{hubDashboard(), botDashboard()} }
