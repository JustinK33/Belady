#!/usr/bin/env bash
# Poll every service's /healthz from the host until all answer 200.
#
# The service images are distroless, so they contain no shell and no curl and a
# Compose healthcheck has nothing to run. Polling from outside is the whole
# reason the debug ports are published.
set -euo pipefail

TIMEOUT="${TIMEOUT:-90}"
INTERVAL="${INTERVAL:-1}"

ports=(
  "gateway:${GATEWAY_DEBUG_PORT:-9090}"
  "registry:${REGISTRY_DEBUG_PORT:-9102}"
  "origin:${ORIGIN_DEBUG_PORT:-9101}"
  "cachenode-0:${CACHENODE_0_DEBUG_PORT:-9201}"
  "cachenode-1:${CACHENODE_1_DEBUG_PORT:-9202}"
  "cachenode-2:${CACHENODE_2_DEBUG_PORT:-9203}"
)

deadline=$(( $(date +%s) + TIMEOUT ))
pending=("${ports[@]}")

while ((${#pending[@]})); do
  still=()
  for entry in "${pending[@]}"; do
    port="${entry##*:}"
    if curl -fs -m 2 -o /dev/null "http://localhost:${port}/healthz"; then
      echo "  ready ${entry%%:*}"
    else
      still+=("$entry")
    fi
  done
  # bash 3.2, which is what macOS ships, treats "${empty[@]}" as unbound under set -u.
  pending=(${still[@]+"${still[@]}"})

  ((${#pending[@]})) || break

  if (( $(date +%s) >= deadline )); then
    echo "timed out after ${TIMEOUT}s waiting for: ${pending[*]}" >&2
    exit 1
  fi
  sleep "$INTERVAL"
done

echo "all services healthy"
