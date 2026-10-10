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
		Description: "The gravel hub: members, HTTP and API traffic, logins, backups, the servers it polls, its background jobs and the Go runtime (gravel docs/hub.md, Observability).",
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
			{Title: "Servers", Panels: []panel{
				{Title: "Reachable", Kind: "stat", Width: 8, TextMode: "value_and_name", Mappings: upMap, Thresholds: []step{{Color: "red"}, {Value: 1, Color: "green"}},
					Description: "Down after two failed polls in a row (ADR-0010); the hub's log says why.", Optional: "no server is registered (servers.yaml)",
					Targets: []target{{Expr: `gravel_server_reachable{` + sel + `}`, Legend: "{{server}}"}}},
				{Title: "Since the last good poll", Kind: "stat", Unit: "s", Width: 8, TextMode: "value_and_name", Optional: "no server is registered, or none has answered yet",
					Thresholds: []step{{Color: "green"}, {Value: 120, Color: "orange"}, {Value: 600, Color: "red"}},
					Targets:    []target{{Expr: `time() - gravel_server_last_observed_timestamp_seconds{` + sel + `}`, Legend: "{{server}}"}}},
				{Title: "Players now", Kind: "stat", Width: 8, TextMode: "value_and_name", Optional: "no server is registered, or none has answered yet",
					Targets: []target{{Expr: `gravel_server_players{` + sel + `}`, Legend: "{{server}}"}}},
				{Title: "Players over time", Kind: "timeseries", Width: 24, Optional: "no server is registered, or none has answered yet",
					Description: "The max line is the public slots (War Dogs: MaxPlayers less MaxReservedSlots).",
					Targets: []target{
						{Expr: `gravel_server_players{` + sel + `}`, Legend: "{{server}}"},
						{Expr: `gravel_server_max_players{` + sel + `}`, Legend: "{{server}} max"},
					}},
				{Title: "Build", Kind: "stat", Width: 12, TextMode: "name", Optional: "no server is registered, or none has answered yet",
					Description: "The build each server runs, as the watcher last read it (every minute, ADR-0010).",
					Targets:     []target{{Expr: `gravel_driver_build_info{` + sel + `}`, Legend: "{{server}}: {{build}}"}}},
				{Title: "Build changes", Kind: "timeseries", Width: 12, Optional: "no server changed build in the range",
					Description: "A game update: the watcher read a new build and the capabilities again. Re-record the fixtures (games/wardogs/README.md).",
					Targets:     []target{{Expr: `sum by (server) (increase(gravel_driver_build_changes_total{` + sel + `}[$__rate_interval]))`, Legend: "{{server}}"}}},
				{Title: "Moderation calls", Kind: "timeseries", Width: 24, Optional: "no moderation call was made in the range",
					Description: "Kicks, bans, messages and moves the hub sent, by outcome; each is a row in the audit log (ListAuditLog).",
					Targets:     []target{{Expr: `sum by (server, action, outcome) (increase(gravel_moderation_actions_total{` + sel + `}[$__rate_interval]))`, Legend: "{{server}} {{action}} {{outcome}}"}}},
			}},
			{Title: "Stats", Panels: []panel{
				{Title: "Matches recorded", Kind: "timeseries", Width: 8, Optional: "no match was played in the range",
					Description: "Matches the stats store started from the polls (ADR-0012): a rotation, every counter dropping, or the first player on an empty server.",
					Targets:     []target{{Expr: `sum by (server) (increase(gravel_stats_matches_total{` + sel + `}[$__rate_interval]))`, Legend: "{{server}}"}}},
				{Title: "Player rows written", Kind: "timeseries", Width: 8, Optional: "nobody was on a server in the range",
					Description: "One row per player on a server per good poll.",
					Targets:     []target{{Expr: `sum by (server) (increase(gravel_stats_rows_written_total{` + sel + `}[$__rate_interval]))`, Legend: "{{server}}"}}},
				{Title: "Polls not recorded", Kind: "timeseries", Width: 8, Optional: "every poll was recorded in the range",
					Description: "The stats store could not write a poll (the database); the next poll records from the store, so only that poll's time on is lost.",
					Targets:     []target{{Expr: `sum by (server) (increase(gravel_stats_record_errors_total{` + sel + `}[$__rate_interval]))`, Legend: "{{server}}"}}},
				{Title: "Rows rolled up", Kind: "timeseries", Width: 12,
					Description: "Per-match player rows past stats.raw_retention_months, moved into monthly totals and deleted (ADR-0012 §6). Boards keep their numbers.",
					Targets:     []target{{Expr: `sum(increase(gravel_stats_rolled_rows_total{` + sel + `}[$__rate_interval]))`, Legend: "rows"}}},
				{Title: "Since the last rollup", Kind: "stat", Unit: "s", Width: 12,
					Description: "The rollup job runs every 6 hours; much over that means it is failing (gravel_hub_jobs_total{job=\"stats_rollup\",result=\"error\"}).",
					Targets:     []target{{Expr: `time() - max(gravel_hub_job_last_success_timestamp_seconds{job="stats_rollup",` + sel + `})`}}},
			}},
			{Title: "Ingestion", Panels: []panel{
				{Title: "Since the last batch", Kind: "stat", Unit: "s", Width: 8, TextMode: "value_and_name", Optional: "no server has a feed (servers.yaml feed), or none has posted yet",
					Description: "A feed posts only while players fight, so a quiet server is quiet here too; compare with Players now (ADR-0011).",
					Thresholds:  []step{{Color: "green"}, {Value: 600, Color: "orange"}, {Value: 3600, Color: "red"}},
					Targets:     []target{{Expr: `time() - gravel_ingest_last_batch_timestamp_seconds{` + sel + `}`, Legend: "{{server}}"}}},
				{Title: "Ingest requests by result", Kind: "timeseries", Width: 16, Optional: "nothing posted to the ingest route in the range",
					Description: "stored: kept before the answer; unauthorized: no valid feed token (server unknown); rate_limited, too_large, bad_request: refused; error: not stored, so lost (the sources do not retry).",
					Targets:     []target{{Expr: `sum by (server, result) (increase(gravel_ingest_batches_total{` + sel + `}[$__rate_interval]))`, Legend: "{{server}} {{result}}"}}},
			}},
			{Title: "Background jobs", Panels: []panel{
				{Title: "Job runs by result", Kind: "timeseries", Width: 16,
					Description: "A failed run is retried with backoff and never stops the hub; server_poll failures are a server that did not answer.",
					Targets:     []target{{Expr: `sum by (job, result) (increase(gravel_hub_jobs_total{` + sel + `}[$__rate_interval]))`, Legend: "{{job}} {{result}}"}}},
				{Title: "Since each job last succeeded", Kind: "stat", Unit: "s", Width: 8, TextMode: "value_and_name",
					Description: "prune runs every 10 minutes, servers_reconcile and ingest_feeds every 30 seconds, ingest_prune hourly, server_poll at each server's interval.",
					Targets:     []target{{Expr: `time() - max by (job) (gravel_hub_job_last_success_timestamp_seconds{` + sel + `})`, Legend: "{{job}}"}}},
			}},
			runtimeRow(),
		},
	}
}

func botDashboard() dashboard {
	return dashboard{
		UID: "gravel-bot", Title: "gravel bot", BuildInfo: "gravel_bot_build_info",
		Description: "The gravel Discord bot: the gateway, interactions against Discord's three seconds, role sync, Linked Roles, the server cards and the Go runtime (gravel docs/bot.md, Observability).",
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
			{Title: "Server cards", Panels: []panel{
				{Title: "Since the last server card pass", Kind: "stat", Unit: "s", Width: 6, Optional: "the server cards are off",
					Description: "A pass that brought every card up to date (or found none placed). Orange after a minute, red after five: a card that keeps failing keeps this growing.",
					Thresholds:  []step{{Color: "green"}, {Value: 60, Color: "orange"}, {Value: 300, Color: "red"}},
					Targets:     []target{{Expr: `time() - max(gravel_bot_servercards_last_success_timestamp_seconds{` + sel + `})`}}},
				{Title: "Server card updates", Kind: "timeseries", Width: 18, Optional: "the server cards are off or changed nothing in the range",
					Description: "posted: a card that was not in its channel; edited: a card that changed; forbidden: the bot may not use the channel; error: Discord failed, retried at the next pass.",
					Targets:     []target{{Expr: `sum by (result) (increase(gravel_bot_servercards_updates_total{` + sel + `}[$__rate_interval]))`, Legend: "{{result}}"}}},
			}},
			runtimeRow(),
		},
	}
}

func dashboards() []dashboard { return []dashboard{hubDashboard(), botDashboard()} }
