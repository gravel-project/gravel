#!/usr/bin/env bash
# The dashboards against a live hub (ADR-0009): one throwaway pod with Postgres, the hub image,
# Prometheus and Grafana (all on the pod's localhost, the pinned images, the provisioning in
# this directory), some traffic, then `dashboards check`, which fails on a hub panel with no
# data that isn't marked optional. The bot dashboard is left out: the bot needs a Discord
# application. Nothing is published but Grafana and the hub on 127.0.0.1, and the pod is removed
# at exit. CI runs it in the observability job; locally: make observability-check.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
hub_image="${HUB_IMAGE:-localhost/gravel-hub:dev}"
prometheus_image="$(sed -n 's/^Image=//p' "$root/deploy/quadlet/observability/gravel-prometheus.container")"
grafana_image="$(sed -n 's/^Image=//p' "$root/deploy/quadlet/observability/gravel-grafana.container")"
grafana_port="${GRAFANA_PORT:-13000}"
hub_port="${HUB_PORT:-18090}"
pod="gravel-observability-check-$$"
work="$(mktemp -d)"

cleanup() {
  podman pod rm -f -t 2 "$pod" >/dev/null 2>&1 || true
  rm -rf -- "$work"
}
trap cleanup EXIT

# Throwaway credentials, inline on purpose: nothing here outlives the pod.
password="check-$(head -c 12 /dev/urandom | base64 | tr -d '/+=\n')"
printf '%s\n' "$password" >"$work/grafana-password"
cat >"$work/hub.yaml" <<EOF
version: 1
organization:
  name: Observability check
server:
  listen: 0.0.0.0:8080
  internal_listen: 127.0.0.1:9090
database:
  url: postgres://gravel:check@127.0.0.1:5432/gravel?sslmode=disable
  migrate: auto
log:
  level: warn
  format: json
EOF
# Scrape fast so rate() has samples within seconds.
sed -e 's/scrape_interval: 15s/scrape_interval: 2s/' \
  -e 's/"hub:9090"/"127.0.0.1:9090"/' -e 's/"localhost:9090"/"127.0.0.1:9092"/' \
  -e '/job_name: gravel-bot/,/targets/d' \
  "$here/prometheus/compose.yml" >"$work/prometheus.yml"
chmod 0644 "$work"/*

podman pod create --name "$pod" -p "127.0.0.1:$grafana_port:3000" -p "127.0.0.1:$hub_port:8080" >/dev/null
podman run -d --pod "$pod" --name "$pod-postgres" -e POSTGRES_USER=gravel -e POSTGRES_PASSWORD=check -e POSTGRES_DB=gravel public.ecr.aws/docker/library/postgres:17 >/dev/null
podman run -d --pod "$pod" --name "$pod-prometheus" -v "$work/prometheus.yml:/etc/prometheus/prometheus.yml:ro,Z" \
  "$prometheus_image" --config.file=/etc/prometheus/prometheus.yml --web.listen-address=127.0.0.1:9092 >/dev/null
podman run -d --pod "$pod" --name "$pod-grafana" \
  -e GF_SECURITY_ADMIN_PASSWORD="$password" -e GF_ANALYTICS_REPORTING_ENABLED=false -e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
  -e GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false -e GF_NEWS_NEWS_FEED_ENABLED=false -e GRAVEL_PROMETHEUS_URL=http://127.0.0.1:9092 \
  -v "$here/grafana/provisioning:/etc/grafana/provisioning:ro,Z" -v "$here/grafana/dashboards:/etc/grafana/dashboards/gravel:ro,Z" \
  "$grafana_image" >/dev/null
for i in $(seq 1 60); do
  podman exec "$pod-postgres" pg_isready -q -U gravel -d gravel && break
  [[ $i -eq 60 ]] && { echo "postgres not ready"; exit 1; }
  sleep 1
done
podman run -d --pod "$pod" --name "$pod-hub" -v "$work/hub.yaml:/etc/gravel/hub.yaml:ro,Z" "$hub_image" serve --config /etc/gravel/hub.yaml >/dev/null

wait_for() { # url name
  for i in $(seq 1 90); do
    curl -sf -o /dev/null "$1" && return 0
    sleep 1
  done
  echo "$2 not ready: $1"
  podman pod logs --tail 30 "$pod" || true
  exit 1
}
wait_for "http://127.0.0.1:$hub_port/readyz" hub
wait_for "http://127.0.0.1:$grafana_port/api/health" grafana

# Traffic for the HTTP and API panels: health probes, pages (which call the API in process), a 404.
for _ in $(seq 1 15); do
  curl -s -o /dev/null "http://127.0.0.1:$hub_port/healthz"
  curl -s -o /dev/null "http://127.0.0.1:$hub_port/readyz"
  curl -s -o /dev/null "http://127.0.0.1:$hub_port/login"
  curl -s -o /dev/null "http://127.0.0.1:$hub_port/"
  sleep 1
done

# Grafana's provisioning logs an error per bad file; any is a failure.
if podman logs "$pod-grafana" 2>&1 | grep -iE 'level=error.*provision|failed to (load|provision)'; then
  echo "grafana reported provisioning errors"
  exit 1
fi

cd "$root"
go run ./internal/tools/dashboards check --grafana "http://127.0.0.1:$grafana_port" --password-file "$work/grafana-password" --skip gravel-bot
