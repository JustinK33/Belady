#!/usr/bin/env bash
#
# Sweep ORIGIN_LATENCY across both policies and write one JSON object per run.
#
# The learned policy's value is entirely a function of what a miss costs, and this
# project has measured exactly one miss cost. This produces the curve; the analysis
# and the crossing point live in docs/03-performance.md.
#
# Why the points are comparable: origin object sizes come from Hash(key + "#size")
# in cmd/origin/main.go, so every point serves byte-identical objects and the only
# thing moving between them is the cost of a miss.
#
# What this deliberately does not do is train. The models volume must already hold
# exactly one model, because two fits of the same trace give different tree counts
# (13 versus 41 at the same boundary), and retraining mid-sweep would put a
# different model on each half of one curve. Run the reproduce block in
# docs/03-performance.md through `make train-compose` first, then this.

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

OUT=${OUT:-sweep.jsonl}

# to_ns converts a Go duration to nanoseconds, so a row of JSONL sorts and plots
# numerically instead of lexicographically ("2ms" sorts before "50us"). Only the
# suffixes a latency sweep uses, because a general parser here would be a second
# implementation of time.ParseDuration.
to_ns() {
  case $1 in
    0) echo 0 ;;
    *ns) echo "${1%ns}" ;;
    *us) awk -v v="${1%us}" 'BEGIN{printf "%d", v*1e3}' ;;
    *ms) awk -v v="${1%ms}" 'BEGIN{printf "%d", v*1e6}' ;;
    *s) awk -v v="${1%s}" 'BEGIN{printf "%d", v*1e9}' ;;
    *) echo "unsupported latency '$1': use 0, or a value in ns, us, ms or s" >&2; exit 2 ;;
  esac
}

# summarize is the whole analysis, and it reads a file rather than a run so that
# re-reading the numbers does not cost another sweep.
#
# Medians rather than means across repeats, because the thing being defended against
# is one run where something else on the machine woke up, and that shows up as an
# outlier rather than as spread.
#
# breakeven_us is the column to read first. It is spent/dhit, the marginal miss cost
# at which this point breaks even, and it depends only on the two quantities that are
# stable run to run. If it holds roughly constant down the table then the curve has
# one moving part and can be extrapolated past the endpoints; if it drifts, the
# result is interpolation between measured points and nothing more.
summarize() {
  local file=${1:?}
  jq -rs '
    def med(f): map(f) | sort | .[((length-1)/2)|floor];

    (group_by(.origin_latency_ns) | sort_by(.[0].origin_latency_ns))
    | map(
        (group_by(.policy) | map({ (.[0].policy): {
            hit:    med(.object_hit),
            evict:  med(.evict_ns_per_victim),
            perreq: med(.evictions_per_request),
            marg:   med(.marginal_miss_ns),
            mean:   med(.mean_ns),
            p99:    med(.p99_ns),
            runs:   length
          }}) | add) as $p
        | { latency: .[0].origin_latency, lru: $p.lru, lrb: $p.lrb }
      )
    | map(select(.lru != null and .lrb != null))
    | map(
        (.lrb.hit - .lru.hit) as $dhit
        # Both policies measure the same origin, so their marginal miss costs should
        # agree; averaging them uses both samples and makes a disagreement visible as
        # a mid-table wobble rather than hiding in whichever one was picked.
        | (((.lru.marg + .lrb.marg) / 2)) as $marg
        | ((.lrb.evict - .lru.evict) * .lrb.perreq) as $spent
        | ($dhit * $marg) as $saved
        | [ .latency, .lrb.runs,
            (.lru.hit), (.lrb.hit), (($dhit * 10000) | round),
            (.lru.evict | round), (.lrb.evict | round),
            (($marg / 1000) | round),
            ($spent | round), ($saved | round), (($saved - $spent) | round),
            (if $dhit > 0 then (($spent / $dhit / 1000) | round) else "n/a" end),
            ((.lru.p99 / 1000) | round), ((.lrb.p99 / 1000) | round) ])
    | ["latency","runs","lru_hit","lrb_hit","dhit_bp","lru_evict_ns","lrb_evict_ns",
       "marginal_us","spent_ns","saved_ns","net_ns","breakeven_us","lru_p99_us","lrb_p99_us"], .[]
    | @tsv
  ' "$file" | column -t
}

if [[ ${1:-} == --summarize ]]; then
  summarize "${2:-$OUT}"
  exit
fi

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

COMPOSE=(docker compose -f deploy/compose.yaml)
[[ -f .env ]] && COMPOSE+=(--env-file .env)

# loadgen runs on the host and dials the gateway's published port, which .env can move.
# Ask Compose where it actually landed rather than trusting a second copy of the number.
if [[ -z ${TARGET_ADDR:-} ]]; then
  TARGET_ADDR=localhost:$("${COMPOSE[@]}" port gateway 8080 | sed 's/.*://')
  export TARGET_ADDR
fi

# The load-bearing configuration, defaulting to the pinned block in
# docs/03-performance.md. None of these are the committed defaults, which is exactly
# why the script owns them: the five defects that pass found all came from parameters
# that were load-bearing and set by no documented command.
export CACHE_CAPACITY=${CACHE_CAPACITY:-6MiB}
export CACHE_SHARDS=${CACHE_SHARDS:-32}
export MODEL_BOUNDARY=${MODEL_BOUNDARY:-1s}
export REQUESTS=${REQUESTS:-400000}
export KEYSPACE=${KEYSPACE:-50000}
export ORIGIN_MIN_SIZE=${ORIGIN_MIN_SIZE:-512}
export ORIGIN_MAX_SIZE=${ORIGIN_MAX_SIZE:-65536}
export ORIGIN_SIZE_ALPHA=${ORIGIN_SIZE_ALPHA:-1.5}
# Jitter defaults to 1ms in compose, in the origin binary and in .env, so a flat
# origin has to say so out loud or most of the sweep measures a distribution.
export ORIGIN_JITTER=${ORIGIN_JITTER:-0}
# This sweep records, it does not gate. A MAX_P99 inherited from a CI-shaped .env
# would abort it halfway up the latency range, which is where the interesting points
# are.
export MIN_OBJECT_HIT=0 MAX_P99=0
export REPORT_JSON=true

# Five points: the transport floor, one below the predicted crossing, the anchor that
# reproduces the recorded pass, the committed default, and the top of the range the
# roadmap asked for.
read -ra latencies <<<"${LATENCIES:-0 50us 200us 2ms 20ms}"
# lrb first, so a missing or refused model fails on the very first run rather than
# after a policy's worth of measurement.
read -ra policies <<<"${POLICIES:-lrb lru}"
REPEATS=${REPEATS:-3}

if [[ -s $OUT ]]; then
  echo "$OUT already holds a sweep: move it aside, or set OUT to a new path." >&2
  exit 2
fi

run_one() {
  local lat=$1 policy=$2 rep=$3 ns row seen
  ns=$(to_ns "$lat")
  echo >&2
  echo "=== ORIGIN_LATENCY=$lat CACHE_POLICY=$policy repeat $rep/$REPEATS" >&2

  # Compose interpolates at create time and bakes the result into the container spec,
  # so `restart` keeps the old latency and only a recreate changes it. Recreating the
  # three nodes on every run is also what makes each one start cold, which is the
  # condition the recorded pass was measured under. Their traces volume survives,
  # which is harmless because nothing here retrains.
  ORIGIN_LATENCY=$lat CACHE_POLICY=$policy \
    "${COMPOSE[@]}" up -d --force-recreate origin cachenode-0 cachenode-1 cachenode-2
  ./scripts/wait-for-health.sh gateway origin cachenode-0 cachenode-1 cachenode-2

  row=$(ORIGIN_LATENCY=$lat go run ./cmd/loadgen |
    jq -c --argjson ns "$ns" --argjson rep "$rep" '. + {origin_latency_ns: $ns, repeat: $rep}')

  seen=$(jq -r '.policy' <<<"$row")
  case $seen in
    "$policy") ;;
    mixed) echo "FAIL the nodes disagree on policy, so no hit ratio here means anything" >&2; exit 1 ;;
    *) echo "FAIL asked for $policy, the cluster reports $seen" >&2; exit 1 ;;
  esac

  # A node that came up on a different MODEL_BOUNDARY refuses the model and serves the
  # fallback policy under the label lrb, which is invisible from outside the node logs.
  # Abort rather than record a row that is secretly sampled LRU.
  if [[ $policy == lrb && -z $(jq -r '.model_version' <<<"$row") ]]; then
    echo "FAIL lrb is running with no model, so this row would be sampled LRU under the lrb label." >&2
    echo "     Check MODEL_BOUNDARY=$MODEL_BOUNDARY against the published model, and the node logs." >&2
    exit 1
  fi

  printf '%s\n' "$row" >>"$OUT"
}

echo "belady origin-latency sweep -> $OUT" >&2
echo "  latencies   ${latencies[*]}" >&2
echo "  policies    ${policies[*]} x $REPEATS repeats" >&2
echo "  workload    $REQUESTS requests over $KEYSPACE keys, ${CACHE_CAPACITY} per node, $CACHE_SHARDS shards" >&2
echo "  origin      jitter $ORIGIN_JITTER, sizes $ORIGIN_MIN_SIZE-$ORIGIN_MAX_SIZE alpha $ORIGIN_SIZE_ALPHA" >&2
echo "  boundary    $MODEL_BOUNDARY" >&2

for lat in "${latencies[@]}"; do
  # Repeat outside policy, so the two policies at one point are adjacent in time. The
  # whole result is a difference between them, so anything that drifts over 35 minutes
  # on a shared machine has to drift through both sides of each comparison.
  for ((rep = 1; rep <= REPEATS; rep++)); do
    for policy in "${policies[@]}"; do
      run_one "$lat" "$policy" "$rep"
    done
  done
done

echo >&2
summarize "$OUT"
