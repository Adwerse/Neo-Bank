#!/usr/bin/env bash
#
# One profile, one command, same numbers every time.
#
#   ./loadtest/run.sh distributed
#   ./loadtest/run.sh hotspot
#   ./loadtest/run.sh duplicates
#   ./loadtest/run.sh all
#
# Each invocation runs the profile at every VU level in VUS (default
# "10 30 60"), sampling Postgres alongside it, and finishes with the
# invariant suite. Everything lands in loadtest/results/.
#
# Prerequisites: the stack is up (`docker compose up -d`), fixtures exist
# (`go run ./loadtest/cmd/lt setup`), and fraud thresholds are raised
# (`go run ./loadtest/cmd/lt fraud -mode loadtest`). run.sh does NOT do
# those three itself, on purpose: setup provisions real users and the
# fraud step mutates a table the rest of the stack shares, and neither
# should happen as an invisible side effect of asking for a measurement.
set -euo pipefail

PROFILE="${1:-}"
if [[ -z "$PROFILE" ]]; then
  echo "usage: $0 <distributed|hotspot|duplicates|all>" >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Docker on Windows needs a Windows-style path for a bind mount, and Git
# Bash's $PWD is a POSIX one. `pwd -W` is the Git Bash builtin that
# converts; on Linux and macOS it does not exist and plain $PWD is right.
if HOST_ROOT="$(pwd -W 2>/dev/null)"; then :; else HOST_ROOT="$PWD"; fi

K6_IMAGE="${K6_IMAGE:-grafana/k6:0.55.0}"
NETWORK="${NETWORK:-neo-bank_default}"
# Four levels, chosen so the last one is unambiguously past the knee: a
# plateau is only legible if there is a point on the far side of it.
VUS_LIST="${VUS:-10 30 60 120}"
DURATION="${DURATION:-60s}"
FIXTURES_FILE="${FIXTURES_FILE:-fixtures.json}"
# How long to let in-flight settlement and the outbox relay finish before
# checking invariants. See verify.go's -settle-wait comment.
SETTLE_WAIT="${SETTLE_WAIT:-15s}"

mkdir -p loadtest/results

run_one() {
  local profile="$1" script="$2" vus="$3"
  local tag="${profile}-vus${vus}"
  local run_id="${tag}-$(date +%s)"

  echo
  echo "=== ${profile} @ ${vus} VUs for ${DURATION} ==============================="

  # Fresh tokens before EVERY level, not once per sweep. auth-svc's access
  # tokens live 15 minutes and a full sweep takes longer, so tokens expire
  # part-way through — and an expired token does not look like a failure,
  # it looks like a record-breaking result (the gateway 401s in under a
  # millisecond, so throughput appears to jump twentyfold at a zero error
  # rate). Reissuing costs one login per user and removes the whole
  # category. See refreshTokens in loadtest/cmd/lt/setup.go.
  go run ./loadtest/cmd/lt setup -refresh -out "loadtest/fixtures/${FIXTURES_FILE}"

  # The probe outlives k6 by 20 seconds so the tail of the run — the
  # outbox draining, transfers settling after the load stops — is in the
  # sample window too. That tail is where relay lag is actually visible.
  go run ./loadtest/cmd/lt probe \
    -duration "$(( $(to_seconds "$DURATION") + 20 ))s" \
    -interval 1s \
    -out "loadtest/results/${tag}.probe.csv" &
  local probe_pid=$!

  MSYS_NO_PATHCONV=1 docker run --rm \
    --network "$NETWORK" \
    -v "${HOST_ROOT}/loadtest/k6:/scripts:ro" \
    -v "${HOST_ROOT}/loadtest/fixtures:/fixtures:ro" \
    -v "${HOST_ROOT}/loadtest/results:/results" \
    -e "PROFILE=${profile}" \
    -e "VUS=${vus}" \
    -e "DURATION=${DURATION}" \
    -e "RUN_ID=${run_id}" \
    -e "FIXTURES=/fixtures/${FIXTURES_FILE}" \
    -e "HOT_DIRECTION=${HOT_DIRECTION:-inbound}" \
    -e "FANOUT=${FANOUT:-10}" \
    -e "AMOUNT=${AMOUNT:-100}" \
    "$K6_IMAGE" run --quiet "/scripts/${script}"

  wait "$probe_pid"
}

# to_seconds turns k6's duration strings into plain seconds for the probe's
# own -duration flag. Only the forms the scripts actually use are handled;
# anything else is a typo worth failing on rather than silently treating
# as zero.
to_seconds() {
  case "$1" in
    *ms) echo $(( ${1%ms} / 1000 )) ;;
    *s)  echo "${1%s}" ;;
    *m)  echo $(( ${1%m} * 60 )) ;;
    *)   echo "run.sh: cannot parse duration '$1'" >&2; exit 2 ;;
  esac
}

run_profile() {
  local profile="$1" script="$2"
  for vus in $VUS_LIST; do
    run_one "$profile" "$script" "$vus"
  done

  echo
  echo "=== invariants after ${profile} ======================================"
  go run ./loadtest/cmd/lt verify \
    -fixtures "loadtest/fixtures/${FIXTURES_FILE}" \
    -profile "$profile" \
    -settle-wait "$SETTLE_WAIT"
}

case "$PROFILE" in
  distributed) run_profile distributed distributed.js ;;
  hotspot)     run_profile hotspot hotspot.js ;;
  duplicates)  run_profile duplicates duplicates.js ;;
  all)
    run_profile distributed distributed.js
    run_profile hotspot hotspot.js
    run_profile duplicates duplicates.js
    ;;
  *)
    echo "unknown profile '$PROFILE'" >&2
    exit 2
    ;;
esac

go run ./loadtest/cmd/lt report
