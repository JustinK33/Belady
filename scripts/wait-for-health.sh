#!/usr/bin/env bash
# Poll every container's health endpoint from the host until all answer 200.
#
# The service images are distroless, so they contain no shell and no curl and a
# Compose healthcheck has nothing to run. Polling from outside is the whole
# reason the debug ports are published.
set -euo pipefail

TIMEOUT="${TIMEOUT:-90}"
INTERVAL="${INTERVAL:-1}"

# name:port:path, because Prometheus and Grafana do not serve /healthz.
ports=(
  "gateway:${GATEWAY_DEBUG_PORT:-9090}:/healthz"
  "registry:${REGISTRY_DEBUG_PORT:-9102}:/healthz"
  "origin:${ORIGIN_DEBUG_PORT:-9101}:/healthz"
  "cachenode-0:${CACHENODE_0_DEBUG_PORT:-9201}:/healthz"
  "cachenode-1:${CACHENODE_1_DEBUG_PORT:-9202}:/healthz"
  "cachenode-2:${CACHENODE_2_DEBUG_PORT:-9203}:/healthz"
  "prometheus:${PROMETHEUS_PORT:-9091}:/-/healthy"
  "grafana:${GRAFANA_PORT:-3000}:/api/health"
)

# Named arguments narrow the list, so the minimal stack can wait on the two
# containers it actually runs instead of timing out on the six it does not.
if (($#)); then
  wanted=()
  for want in "$@"; do
    for entry in "${ports[@]}"; do
      [ "${entry%%:*}" = "$want" ] && wanted+=("$entry")
    done
  done
  if ((${#wanted[@]} != $#)); then
    echo "unknown service in: $*" >&2
    exit 2
  fi
  ports=("${wanted[@]}")
fi

deadline=$(( $(date +%s) + TIMEOUT ))
pending=("${ports[@]}")

while ((${#pending[@]})); do
  still=()
  for entry in "${pending[@]}"; do
    path="${entry##*:}"
    port="${entry%:*}"
    port="${port##*:}"
    if curl -fs -m 2 -o /dev/null "http://localhost:${port}${path}"; then
      echo "  ready ${entry%%:*}"
    else
      still+=("$entry")
    fi
  done
  # bash 3.2, which is what macOS ships, treats "${empty[@]}" as unbound under set -u.
  pending=(${still[@]+"${still[@]}"})

  ((${#pending[@]})) || break

  if (( $(date +%s) >= deadline )); then
    names=""
    for entry in "${pending[@]}"; do names="${names} ${entry%%:*}"; done
    echo "timed out after ${TIMEOUT}s waiting for:${names}" >&2
    exit 1
  fi
  sleep "$INTERVAL"
done

echo "all containers healthy"
