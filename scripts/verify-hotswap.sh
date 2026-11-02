#!/usr/bin/env bash
# Hot-swap verification: install plugin v1, hold a slow inference request open,
# swap the plugin file to v2 mid-stream, and prove the request finishes intact
# while the CPA process keeps its PID.
#
#   scripts/verify-hotswap.sh                # full acceptance run (request > 30s)
#   scripts/verify-hotswap.sh --keep         # keep the temp dir for inspection
#   scripts/verify-hotswap.sh --debug        # short plumbing run, NEVER prints PASS
#   scripts/verify-hotswap.sh --timeout 180  # bound the streaming request (s)
#
# What PASS means: the script only prints `PASS` after asserting, from its own
# measured start/end timestamps, that the streaming request actually ran longer
# than 30 seconds. A shorter run cannot produce an acceptance verdict; use
# --debug for a fast plumbing check, which prints neither PASS nor "verified".
#
# This script never touches a running CPA. Ownership is by container ID: only a
# container this run created (its ID was printed by this run's `docker run`) is
# ever removed. Every preflight refusal exits before any container exists, so a
# refusal performs no Docker mutation at all (see scripts/verify-hotswap-guards.sh,
# which proves that with a recording docker stand-in).
#
# Requires docker, curl and python3 (stdlib only). No Go on the host: the plugin
# versions and the fake upstream are built in golang:1.26-bookworm.
#
# Environment knobs: HOTSWAP_PORT, HOTSWAP_UPSTREAM_PORT, HOTSWAP_STREAM_SECONDS,
# HOTSWAP_SWAP_AT_SECONDS.
set -euo pipefail

cd "$(dirname "$0")/.."

# ---------------------------------------------------------------- identities
CONTAINER="ollama-hotswap-test"
UPSTREAM_CONTAINER="ollama-hotswap-upstream"
PORT="${HOTSWAP_PORT:-18999}"
UPSTREAM_PORT="${HOTSWAP_UPSTREAM_PORT:-18080}"
CPA_IMAGE="eceasy/cli-proxy-api:v8.0.47"
GO_IMAGE="golang:1.26-bookworm"
MGMT_KEY="hotswap-local-mgmt-key"
PLUGIN_ID="ollama-quota"
CLIENT_KEY="hotswap-local-client-key"

# An acceptance run needs the request to demonstrably outlive 30 seconds, which
# is the SPEC 4.1 acceptance shape (SPEC 4.1: "发起一个耗时 30 秒以上的推理请求").
# Anything shorter is plumbing-only and runs under --debug.
MIN_ACCEPTANCE_SECONDS=31
STREAM_SECONDS="${HOTSWAP_STREAM_SECONDS:-40}"
SWAP_AT_SECONDS="${HOTSWAP_SWAP_AT_SECONDS:-8}"
REQUEST_TIMEOUT=180
KEEP=0
DEBUG_ONLY=0

# Container IDs this run created. Empty means the run owns nothing and the
# cleanup path must not touch Docker at all.
OWNED_CPA_ID=""
OWNED_UPSTREAM_ID=""

# Build scratch lives outside the source tree; see build_version.
BUILD_SRC_DIR=""

SOURCE_VERSION=""
V1_VERSION=""
V2_VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --debug) DEBUG_ONLY=1; shift ;;
    --timeout) REQUEST_TIMEOUT="${2:-180}"; shift 2 ;;
    -h|--help) sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '\n=== %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

RESULTS=()
verdict() { # verdict <ok|bad> <label> [detail]
  if [[ "$1" == "ok" ]]; then RESULTS+=("PASS  $2${3:+ — $3}"); else RESULTS+=("FAIL  $2${3:+ — $3}"); fi
}

# python vs python3 depends on the host; resolve it once. This happens before
# any preflight so every call site uses the same binary.
if command -v python >/dev/null 2>&1; then
  PYTHON="python"
elif command -v python3 >/dev/null 2>&1; then
  PYTHON="python3"
else
  printf '\nFAIL: python3 (standard library only) is needed to read JSON and probe ports\n' >&2
  exit 1
fi

WORKDIR=""
CLEANED=0
cleanup() {
  local status=$?
  [[ $CLEANED -eq 1 ]] && return
  CLEANED=1
  set +e
  log "cleanup"
  # Ownership is by ID, never by name. A preflight refusal leaves both IDs
  # empty, so this branch does nothing at all on a refusal.
  if [[ -n "$OWNED_CPA_ID" ]]; then
    if docker rm -f "$OWNED_CPA_ID" >/dev/null 2>&1; then
      note "removed the CPA container this run started ($OWNED_CPA_ID)"
    fi
  fi
  if [[ -n "$OWNED_UPSTREAM_ID" ]]; then
    if docker rm -f "$OWNED_UPSTREAM_ID" >/dev/null 2>&1; then
      note "removed the upstream container this run started ($OWNED_UPSTREAM_ID)"
    fi
  fi
  if [[ -n "$BUILD_SRC_DIR" && -d "$BUILD_SRC_DIR" ]]; then
    rm -rf "$BUILD_SRC_DIR" && note "removed build scratch $BUILD_SRC_DIR"
  fi
  if [[ -n "$WORKDIR" && -d "$WORKDIR" ]]; then
    if [[ $KEEP -eq 1 ]]; then note "kept $WORKDIR (--keep)"; else rm -rf "$WORKDIR" && note "removed $WORKDIR"; fi
  fi
  exit $status
}
trap cleanup EXIT INT TERM

port_in_use() {
  # The probe lives in scripts/port_in_use.py. This is the one check on the
  # Windows path that a shell cannot do reliably: Git Bash's /dev/tcp reports a
  # live listener as free, and its pipe into grep mangles netstat's output.
  "$PYTHON" scripts/port_in_use.py "$1"
}

wait_for() { # wait_for <seconds> <command...>
  local limit="$1"; shift
  local deadline=$((SECONDS + limit))
  while (( SECONDS < deadline )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

host_path() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

active_version() {
  curl -fsS -m 5 -H "Authorization: Bearer $MGMT_KEY" "http://127.0.0.1:$PORT/v0/management/plugins" \
    | "$PYTHON" -c '
import json, sys
for entry in json.load(sys.stdin).get("plugins", []):
    if entry.get("id") == sys.argv[1] and entry.get("registered"):
        print((entry.get("metadata") or {}).get("version", ""))
        break
' "$PLUGIN_ID"
}

# ---------------------------------------------------------------- preflight
# Everything below exits before a single container exists, so a refusal here
# cannot leave anything behind and cannot remove anything it did not make.
log "preflight"
command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
command -v curl   >/dev/null 2>&1 || fail "curl is not on PATH"

for p in "$PORT" "$UPSTREAM_PORT"; do
  if port_in_use "$p"; then
    cat >&2 <<MSG
FAIL: port $p already has a listener.
    Refusing to start: this script must never share a port with a running CPA
    or anything else. Choose another port with HOTSWAP_PORT=... and
    HOTSWAP_UPSTREAM_PORT=..., or stop whatever holds it.
    Nothing was started and nothing was removed.
MSG
    exit 1
  fi
  note "port $p is free"
done

for c in "$CONTAINER" "$UPSTREAM_CONTAINER"; do
  if docker container inspect "$c" >/dev/null 2>&1; then
    cat >&2 <<MSG
FAIL: container $c already exists.
    Refusing to start: a previous run did not clean up, or someone else owns
    that name. This script removes containers by ID, and it has no ID for that
    one, so it will not touch it. Inspect it; if it is stale, remove it
    yourself with
        docker rm -f $c
    Nothing was started and nothing was removed.
MSG
    exit 1
  fi
  note "container name $c is unused"
done

# The two versions this swap runs between, read from the source the builds will use.
SOURCE_VERSION="$(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' internal/quotaplugin/plugin.go | head -1)"
if [[ ! "$SOURCE_VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  fail "cannot read an x.y.z Version from internal/quotaplugin/plugin.go (found '$SOURCE_VERSION')"
fi
V1_VERSION="$SOURCE_VERSION"
V2_VERSION="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))"
note "plugin versions: v$V1_VERSION (committed) -> v$V2_VERSION (second build)"

# Accepting a short stream would let the script print an acceptance PASS for a
# request that never outlived 30 seconds, which is not what this ticket claims.
if ! [[ "$STREAM_SECONDS" =~ ^[0-9]+$ ]]; then
  fail "HOTSWAP_STREAM_SECONDS must be an integer (got '$STREAM_SECONDS')"
fi
if [[ $DEBUG_ONLY -eq 0 && "$STREAM_SECONDS" -lt "$MIN_ACCEPTANCE_SECONDS" ]]; then
  cat >&2 <<MSG
FAIL: HOTSWAP_STREAM_SECONDS=$STREAM_SECONDS is too short for an acceptance run.
    An acceptance run needs a request that outlives 30 seconds
    (HOTSWAP_STREAM_SECONDS >= $MIN_ACCEPTANCE_SECONDS). For a fast plumbing
    check that never prints PASS, pass --debug.
    Nothing was started and nothing was removed.
MSG
  exit 1
fi
if ! [[ "$SWAP_AT_SECONDS" =~ ^[0-9]+$ ]] || [[ "$SWAP_AT_SECONDS" -ge "$STREAM_SECONDS" ]]; then
  fail "HOTSWAP_SWAP_AT_SECONDS must be an integer below HOTSWAP_STREAM_SECONDS (got '$SWAP_AT_SECONDS')"
fi
if [[ $DEBUG_ONLY -eq 1 ]]; then
  note "DEBUG MODE: this run proves plumbing only and will not print PASS"
fi

# mktemp -d creates a fresh directory; assert it rather than assume it.
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/ollama-hotswap.XXXXXX")"
[[ -d "$WORKDIR" && -z "$(ls -A "$WORKDIR")" ]] || fail "temp dir $WORKDIR is not new and empty"
note "temp dir $WORKDIR is new"

# A brand-new directory is not on its own proof that no running CPA can see it:
# if a CPA has a parent directory bind-mounted, a fresh temp dir inside it is
# visible to that CPA. Compare against every running container's mount sources
# and refuse on any equality, ancestor, or descendant overlap. This is read-only:
# it inspects, and never touches those containers.
#
# The check must distinguish three outcomes, and only one of them may continue:
#   * helper exit 1  -> the check ran and found no overlap         -> continue
#   * helper exit 0  -> overlap found (its lines name the mounts)  -> refuse
#   * anything else  -> ps/inspect failed, helper crashed, timeout -> refuse
# Swallowing a failure here would turn a broken safety check into a silent pass,
# which is exactly the isolation hole this preflight exists to close.
mount_check_failure() { # mount_check_failure <what went wrong>
  {
    printf '\nFAIL: the mount-overlap check could not be completed.\n'
    printf '    %s\n' "$1"
    printf '    Refusing to start: "no running container can see %s"\n' "$WORKDIR"
    printf '    was NOT proven, so this run cannot claim to be isolated.\n'
    printf '    Nothing was started and nothing was removed.\n'
  } >&2
  exit 1
}

mount_overlap_scan() {
  local ps_out inspect_out helper_out helper_rc
  local -a running_ids

  # `docker ps -q` failing must not read as "no containers are running".
  if ! ps_out="$(docker ps -q 2>"$WORKDIR/docker-ps.err")"; then
    mount_check_failure "docker ps -q failed: $(head -3 "$WORKDIR/docker-ps.err" 2>/dev/null | tr '\n' ' ')"
  fi
  mapfile -t running_ids <<<"$ps_out"
  # Drop the empty element mapfile produces for an empty stream.
  running_ids=("${running_ids[@]/#/}")
  local kept=()
  for id in "${running_ids[@]}"; do [[ -n "$id" ]] && kept+=("$id"); done
  running_ids=("${kept[@]}")

  if [[ ${#running_ids[@]} -eq 0 ]]; then
    note "no running containers; the mount-overlap check does not apply"
    return 0
  fi
  note "checking ${#running_ids[@]} running container(s) for mount overlap"

  if ! inspect_out="$(docker inspect --format '{{.Name}}|{{range .Mounts}}{{.Source}}|{{end}}' \
        "${running_ids[@]}" 2>"$WORKDIR/docker-inspect.err")"; then
    mount_check_failure "docker inspect failed: $(head -3 "$WORKDIR/docker-inspect.err" 2>/dev/null | tr '\n' ' ')"
  fi

  # The helper runs to completion so its exit code is meaningful; `|| true` here
  # would erase exactly the signal being tested.
  set +e
  helper_out="$(printf '%s\n' "$inspect_out" | "$PYTHON" scripts/mount_overlap.py "$WORKDIR" "$(host_path "$WORKDIR")" 2>"$WORKDIR/mount-overlap.err")"
  helper_rc=$?
  set -e

  case "$helper_rc" in
    0)
      {
        printf '\nFAIL: the temp tree overlaps an existing container'"'"'s bind mount.\n'
        printf '    Refusing to start: a running container can already see inside\n'
        printf '    %s, so this test instance would not be isolated.\n' "$WORKDIR"
        printf '    Overlapping mount(s):\n'
        printf '%s\n' "$helper_out" | sed 's/^/      /'
        printf '    Choose a different location (for example a TMPDIR outside any\n'
        printf '    mounted tree). Nothing was started and nothing was removed.\n'
      } >&2
      exit 1
      ;;
    1)
      note "no running container mounts a tree containing $WORKDIR"
      return 0
      ;;
    *)
      mount_check_failure "the overlap helper exited $helper_rc: $(head -3 "$WORKDIR/mount-overlap.err" 2>/dev/null | tr '\n' ' ')"
      ;;
  esac
}

mount_overlap_scan

mkdir -p "$WORKDIR/plugins" "$WORKDIR/auths" "$WORKDIR/build"

for image in "$CPA_IMAGE" "$GO_IMAGE"; do
  docker image inspect "$image" >/dev/null 2>&1 || { log "pulling $image"; docker pull "$image" >/dev/null || fail "cannot pull $image"; }
done
note "images present: $CPA_IMAGE, $GO_IMAGE"

# --------------------------------------------------------------- fixtures
log "building the fixtures (fake upstream + plugin v$V1_VERSION and v$V2_VERSION)"

cat > "$WORKDIR/build/upstream.go" <<'GO'
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	addr := os.Getenv("ADDR")
	seconds := 40
	if raw := os.Getenv("STREAM_SECONDS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			seconds = parsed
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		send := func(payload any) {
			raw, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": "chatcmpl-hotswap", "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": "fake-slow-model",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}
		send(chunk(map[string]any{"role": "assistant"}, nil))
		for i := 1; i <= seconds; i++ {
			select {
			case <-r.Context().Done():
				log.Printf("client disconnected at token %d", i)
				return
			case <-time.After(time.Second):
			}
			send(chunk(map[string]any{"content": fmt.Sprintf("TOKEN-%03d ", i)}, nil))
		}
		send(chunk(map[string]any{}, "stop"))
		send(map[string]any{
			"id": "chatcmpl-hotswap", "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": "fake-slow-model", "choices": []any{},
			"usage": map[string]any{"prompt_tokens": 8, "completion_tokens": seconds, "total_tokens": 8 + seconds},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		log.Printf("stream of %d tokens complete", seconds)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Printf("fake upstream listening on %s, %d tokens, one per second", addr, seconds)
	log.Fatal(http.ListenAndServe(addr, mux))
}
GO

printf 'module hotswapfixture\n\ngo 1.26\n' > "$WORKDIR/build/go.mod"

MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$(host_path "$WORKDIR/build"):/fixture" -w /fixture \
  "$GO_IMAGE" \
  sh -c 'mkdir -p /tmp/gocache && export GOCACHE=/tmp/gocache && go build -o /fixture/upstream .' \
  || fail "could not build the fake upstream"
# -s, not -x: this script also runs under Git Bash on Windows, where the
# execute bit on a Linux binary in a mounted temp dir does not round-trip.
[[ -s "$WORKDIR/build/upstream" ]] || fail "the fake upstream binary is missing"

# Version is a Go constant, so v2 is a genuine second compile of the same source
# with Version changed. The -v<version> filename suffix is what makes CPA treat
# the new file as an upgrade instead of a plugin with the same identity.
#
# The build happens in a copy of the tree, never in place. Stamping the working
# copy in place would mean an interrupt (Ctrl-C, a dropped terminal, SIGTERM from
# a CI timeout) could leave plugin.go at the bumped version, and the next run
# would silently swap 0.1.3 -> 0.1.4 instead of the version under review.
PLUGIN_ID_DIRS=(cmd internal go.mod go.sum)
stage_build_tree() {
  BUILD_SRC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/hotswap-src.XXXXXX")"
  local entry
  for entry in "${PLUGIN_ID_DIRS[@]}"; do
    cp -R "$entry" "$BUILD_SRC_DIR/"
  done
}

build_version() { # build_version <version>
  local version="$1"
  local copied_plugin_go="$BUILD_SRC_DIR/internal/quotaplugin/plugin.go"
  local applied

  sed -E "s/^([[:space:]]*Version[[:space:]]*=[[:space:]]*)\"0\.[0-9]+\.[0-9]+\"/\1\"$version\"/" \
    "$copied_plugin_go" > "$copied_plugin_go.tmp"
  mv "$copied_plugin_go.tmp" "$copied_plugin_go"
  applied="$(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' "$copied_plugin_go" | head -1)"
  if [[ "$applied" != "$version" ]]; then
    fail "could not stamp Version=$version into the build copy (found '$applied')"
  fi

  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(host_path "$BUILD_SRC_DIR"):/src" -v "$(host_path "$WORKDIR/build"):/out" -w /src \
    "$GO_IMAGE" \
    sh -c "mkdir -p /tmp/gocache && export GOCACHE=/tmp/gocache && \
           CGO_ENABLED=1 go build -buildmode=c-shared -o '/out/$PLUGIN_ID-v$version.so' ./cmd/plugin" \
    || fail "build of $version failed"
}

stage_build_tree
build_version "$V1_VERSION"
build_version "$V2_VERSION"

for version in "$V1_VERSION" "$V2_VERSION"; do
  [[ -s "$WORKDIR/build/$PLUGIN_ID-v$version.so" ]] || fail "the $version build produced no library"
done
if cmp -s "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" "$WORKDIR/build/$PLUGIN_ID-v$V2_VERSION.so"; then
  fail "v1 and v2 are byte-identical; the swap would not be an upgrade"
fi
# The working tree was never stamped: assert that rather than claim it.
if ! grep -qE "^[[:space:]]*Version[[:space:]]*=[[:space:]]*\"$V1_VERSION\"" internal/quotaplugin/plugin.go; then
  fail "the working tree's plugin.go is no longer at v$V1_VERSION; the build stamped it in place"
fi
note "v1 $(wc -c < "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" | tr -d ' ') bytes"
note "v2 $(wc -c < "$WORKDIR/build/$PLUGIN_ID-v$V2_VERSION.so" | tr -d ' ') bytes, differs from v1"
note "the working tree's plugin.go was never modified (still v$V1_VERSION)"

# The CPA config for this run only. Its plugin dir, auth dir and port are all
# unique to this temp directory.
cat > "$WORKDIR/config.yaml" <<YAML
config-version: 8
server:
  host: "0.0.0.0"
  port: $PORT
management:
  allow-remote: true
  secret-key: "$MGMT_KEY"
access:
  api-keys:
    - "$CLIENT_KEY"
oauth:
  auth-dir: "/data/auths"
plugins:
  enabled: true
  dir: "/data/plugins"
  configs: {}
openai-compatibility:
  - name: "hotswap-fake-upstream"
    base-url: "http://host.docker.internal:$UPSTREAM_PORT/v1"
    api-key-entries:
      - api-key: "fixture-upstream-key"
    models:
      - name: "fake-slow-model"
        alias: "fake-slow-model"
YAML

# ------------------------------------------------------------------- start
# From here on the run owns containers, so their IDs are what cleanup removes.
log "starting the fake upstream on 127.0.0.1:$UPSTREAM_PORT"
OWNED_UPSTREAM_ID="$(MSYS_NO_PATHCONV=1 docker run -d --rm --name "$UPSTREAM_CONTAINER" \
  -p "127.0.0.1:$UPSTREAM_PORT:$UPSTREAM_PORT" \
  -e "ADDR=:$UPSTREAM_PORT" -e "STREAM_SECONDS=$STREAM_SECONDS" \
  -v "$(host_path "$WORKDIR/build"):/fixture:ro" \
  "$GO_IMAGE" /fixture/upstream)" || fail "the fake upstream container did not start"
[[ -n "$OWNED_UPSTREAM_ID" ]] || fail "docker run returned no container ID for the upstream"
note "upstream container $OWNED_UPSTREAM_ID"

upstream_health() { curl -fsS -m 2 "http://127.0.0.1:$UPSTREAM_PORT/healthz" >/dev/null; }
if ! wait_for 45 upstream_health; then
  docker logs "$OWNED_UPSTREAM_ID" 2>&1 | tail -20 >&2 || true
  fail "the fake upstream never became healthy"
fi
note "upstream healthy"

log "starting the isolated CPA instance on 127.0.0.1:$PORT"
cp "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" "$WORKDIR/plugins/"
OWNED_CPA_ID="$(MSYS_NO_PATHCONV=1 docker run -d --rm --name "$CONTAINER" \
  -p "127.0.0.1:$PORT:$PORT" \
  -v "$(host_path "$WORKDIR/config.yaml"):/CLIProxyAPI/config.yaml" \
  -v "$(host_path "$WORKDIR/plugins"):/data/plugins" \
  -v "$(host_path "$WORKDIR/auths"):/data/auths" \
  "$CPA_IMAGE" ./CLIProxyAPI --config /CLIProxyAPI/config.yaml)" \
  || fail "the CPA container did not start"
[[ -n "$OWNED_CPA_ID" ]] || fail "docker run returned no container ID for CPA"
note "CPA container $OWNED_CPA_ID"

cpa_health() { curl -fsS -m 3 -H "Authorization: Bearer $MGMT_KEY" "http://127.0.0.1:$PORT/v0/management/plugins" >/dev/null; }
if ! wait_for 60 cpa_health; then
  docker logs "$OWNED_CPA_ID" 2>&1 | tail -30 >&2 || true
  fail "the CPA instance never answered on port $PORT"
fi
note "CPA answered"

mgmt() { curl -fsS -m 10 -H "Authorization: Bearer $MGMT_KEY" "$@"; }

log "enabling plugin v$V1_VERSION"
mgmt -X PATCH -H "Content-Type: application/json" \
     -d '{"enabled":true}' "http://127.0.0.1:$PORT/v0/management/plugins/$PLUGIN_ID/enabled" >/dev/null \
  || fail "could not enable the plugin"

registered_v1() { [[ "$(active_version)" == "$V1_VERSION" ]]; }
if ! wait_for 30 registered_v1; then
  mgmt "http://127.0.0.1:$PORT/v0/management/plugins" >&2 || true
  fail "the plugin never registered as v$V1_VERSION"
fi
note "plugin registered as v$V1_VERSION"

routable() { curl -fsS -m 5 -H "Authorization: Bearer $CLIENT_KEY" "http://127.0.0.1:$PORT/v1/models" | grep -q fake-slow-model; }
if ! wait_for 30 routable; then
  fail "CPA never listed the fake upstream model, so the request would not be routable"
fi
note "model fake-slow-model is routable"

# -------------------------------------------------------------- experiment
cpa_process() { docker inspect "$1" --format '{{.State.Pid}}|{{.State.StartedAt}}|{{.RestartCount}}' 2>/dev/null || true; }

before="$(cpa_process "$OWNED_CPA_ID")"
[[ -n "$before" ]] || fail "cannot inspect the CPA container"
BEFORE_PID="${before%%|*}"
rest="${before#*|}"; BEFORE_STARTED="${rest%%|*}"; BEFORE_RESTARTS="${rest##*|}"
[[ "$BEFORE_PID" != "0" && -n "$BEFORE_PID" ]] || fail "the CPA process is not running"

log "starting a ${STREAM_SECONDS}s streaming request"
# Wall clock, not $SECONDS: the acceptance assertion below compares these two
# values, and a coarser or non-monotonic source would make it approximate.
REQUEST_START_EPOCH="$(date +%s)"
REQUEST_STARTED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

curl -sS -N --max-time "$REQUEST_TIMEOUT" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{"model":"fake-slow-model","stream":true,"messages":[{"role":"user","content":"prove the plugin swap does not cut this stream"}]}' \
  "http://127.0.0.1:$PORT/v1/chat/completions" \
  > "$WORKDIR/stream.out" 2> "$WORKDIR/stream.err" &
REQUEST_PID=$!

saw_token() { grep -q 'TOKEN-' "$WORKDIR/stream.out"; }
if ! wait_for 45 saw_token; then
  cat "$WORKDIR/stream.err" >&2 || true
  fail "the streaming request never produced a token"
fi
note "request is streaming ($(grep -o 'TOKEN-[0-9]*' "$WORKDIR/stream.out" | wc -l | tr -d ' ') tokens so far)"

# Let the stream establish itself so the swap lands mid-body, not during setup.
sleep "$SWAP_AT_SECONDS"
if ! kill -0 "$REQUEST_PID" 2>/dev/null; then
  fail "the streaming request ended before the swap, so it proves nothing; raise HOTSWAP_STREAM_SECONDS"
fi

mid="$(cpa_process "$OWNED_CPA_ID")"
MID_PID="${mid%%|*}"
note "PID during the request: $MID_PID (before: $BEFORE_PID)"

log "swapping the plugin file to v$V2_VERSION while the request is open"
# CPA re-reads the plugin directory whenever its config is applied. Writing the
# plugin config through the management API is a real config change, which is the
# same path the plugin page's save button takes.
cp "$WORKDIR/build/$PLUGIN_ID-v$V2_VERSION.so" "$WORKDIR/plugins/"
mgmt -X PATCH -H "Content-Type: application/json" \
     -d '{"interval_seconds":901}' "http://127.0.0.1:$PORT/v0/management/plugins/$PLUGIN_ID/config" >/dev/null \
  || fail "could not trigger the config re-apply that drives the swap"

swapped_to_v2() { [[ "$(active_version)" == "$V2_VERSION" ]]; }
if ! wait_for 45 swapped_to_v2; then
  docker logs "$OWNED_CPA_ID" 2>&1 | tail -30 >&2 || true
  fail "the plugin never reported v$V2_VERSION"
fi
SWAP_CONFIRMED_EPOCH="$(date +%s)"
note "host reports v$V2_VERSION"

if ! kill -0 "$REQUEST_PID" 2>/dev/null; then
  fail "the request finished before the swap completed; raise HOTSWAP_STREAM_SECONDS"
fi
note "the request is still streaming across the swap"

wait "$REQUEST_PID"; REQUEST_STATUS=$?
REQUEST_END_EPOCH="$(date +%s)"
REQUEST_ENDED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Measured, not configured: this is the number the acceptance gate below uses.
REQUEST_SECONDS=$((REQUEST_END_EPOCH - REQUEST_START_EPOCH))

after="$(cpa_process "$OWNED_CPA_ID")"
AFTER_PID="${after%%|*}"
rest="${after#*|}"; AFTER_STARTED="${rest%%|*}"; AFTER_RESTARTS="${rest##*|}"

# -------------------------------------------------------------- assertions
log "assertions"
body_bytes="$(wc -c < "$WORKDIR/stream.out" | tr -d ' ')"
tokens_seen="$(grep -o 'TOKEN-[0-9]*' "$WORKDIR/stream.out" | sort -u | wc -l | tr -d ' ')"
has_done=0;     grep -q '\[DONE\]' "$WORKDIR/stream.out" && has_done=1
has_finish=0;   grep -q '"finish_reason":"stop"' "$WORKDIR/stream.out" && has_finish=1
has_usage=0;    grep -q '"completion_tokens"' "$WORKDIR/stream.out" && has_usage=1
final_version="$(active_version)"

printf '\nrequest timeline (UTC)\n'
printf '  started         %s\n' "$REQUEST_STARTED_UTC"
printf '  swap confirmed  +%ss\n' "$((SWAP_CONFIRMED_EPOCH - REQUEST_START_EPOCH))"
printf '  ended           %s\n' "$REQUEST_ENDED_UTC"
printf '  measured        %ss (the acceptance gate needs >30s)\n' "$REQUEST_SECONDS"
printf '  response        %s bytes, %s/%s distinct tokens\n' "$body_bytes" "$tokens_seen" "$STREAM_SECONDS"
printf '\nCPA process\n'
printf '  PID           before=%s  mid=%s  after=%s\n' "$BEFORE_PID" "$MID_PID" "$AFTER_PID"
printf '  StartedAt     before=%s  after=%s\n' "$BEFORE_STARTED" "$AFTER_STARTED"
printf '  RestartCount  before=%s  after=%s\n' "$BEFORE_RESTARTS" "$AFTER_RESTARTS"
printf '\n'

[[ $REQUEST_STATUS -eq 0 ]] && verdict ok "the in-flight request returned cleanly" "curl exit 0" \
  || verdict bad "the in-flight request returned cleanly" "curl exit $REQUEST_STATUS: $(tail -2 "$WORKDIR/stream.err" 2>/dev/null | tr '\n' ' ')"
[[ "$body_bytes" -gt 0 ]] && verdict ok "the response carried a body" "$body_bytes bytes" \
  || verdict bad "the response carried a body" "empty"
[[ $has_done -eq 1 ]] && verdict ok "the stream reached its terminator" "saw [DONE]" \
  || verdict bad "the stream reached its terminator" "no [DONE]: truncated"
[[ $has_finish -eq 1 ]] && verdict ok "the upstream finished normally" "finish_reason=stop" \
  || verdict bad "the upstream finished normally" "no finish_reason=stop"
[[ $has_usage -eq 1 ]] && verdict ok "the final usage chunk arrived" "completion_tokens present" \
  || verdict bad "the final usage chunk arrived" "missing"
[[ "$tokens_seen" -eq "$STREAM_SECONDS" ]] && verdict ok "every token arrived" "$tokens_seen/$STREAM_SECONDS" \
  || verdict bad "every token arrived" "$tokens_seen/$STREAM_SECONDS"

[[ $REQUEST_START_EPOCH -lt $SWAP_CONFIRMED_EPOCH && $REQUEST_END_EPOCH -gt $SWAP_CONFIRMED_EPOCH ]] \
  && verdict ok "the request spanned the swap" \
       "started ${REQUEST_STARTED_UTC}, swap at +$((SWAP_CONFIRMED_EPOCH - REQUEST_START_EPOCH))s, ended ${REQUEST_ENDED_UTC}" \
  || verdict bad "the request spanned the swap" "it does not straddle the swap"

# The acceptance gate itself: measured wall clock, not the configured length.
# Under --debug a short run is the point, so the gate is reported as SKIP: it is
# not a passing verdict, and it does not mask a real plumbing failure either.
if [[ $DEBUG_ONLY -eq 1 ]]; then
  RESULTS+=("SKIP  the request outlived 30 seconds — not applicable in --debug")
elif [[ "$REQUEST_SECONDS" -gt 30 ]]; then
  verdict ok "the request outlived 30 seconds" "measured ${REQUEST_SECONDS}s"
else
  verdict bad "the request outlived 30 seconds" "measured only ${REQUEST_SECONDS}s"
fi

[[ "$BEFORE_PID" == "$AFTER_PID" && "$BEFORE_PID" == "$MID_PID" ]] \
  && verdict ok "the CPA process kept its PID" "$BEFORE_PID throughout" \
  || verdict bad "the CPA process kept its PID" "before=$BEFORE_PID mid=$MID_PID after=$AFTER_PID"
[[ "$BEFORE_STARTED" == "$AFTER_STARTED" ]] \
  && verdict ok "the container was not restarted" "StartedAt unchanged" \
  || verdict bad "the container was not restarted" "$BEFORE_STARTED -> $AFTER_STARTED"
[[ "$BEFORE_RESTARTS" == "$AFTER_RESTARTS" && "$AFTER_RESTARTS" == "0" ]] \
  && verdict ok "no restart was counted" "RestartCount 0" \
  || verdict bad "no restart was counted" "RestartCount $BEFORE_RESTARTS -> $AFTER_RESTARTS"
[[ "$final_version" == "$V2_VERSION" ]] \
  && verdict ok "the new plugin version is live" "v$V2_VERSION" \
  || verdict bad "the new plugin version is live" "reports '$final_version'"

CPA_LOG="$WORKDIR/cpa.log"
docker logs "$OWNED_CPA_ID" > "$CPA_LOG" 2>&1 || true
if grep -qiE "plugin hot reloaded.*active_version=$V2_VERSION" "$CPA_LOG"; then
  verdict ok "CPA logged a hot reload, not a reload from scratch" "active_version=$V2_VERSION"
else
  verdict bad "CPA logged a hot reload, not a reload from scratch" "no 'plugin hot reloaded' line for v$V2_VERSION"
fi
# A timed-out quiesce drain must be visible here, not silently accepted. Clean
# runs must show none; a run that shows one has a real finding to report.
if grep -qiE 'quiesce drain timed out|quiesce_drain_timeout' "$CPA_LOG"; then
  drain_timeouts="$(grep -icE 'quiesce drain timed out|quiesce_drain_timeout' "$CPA_LOG" || true)"
  verdict bad "no quiesce drain timed out" "$drain_timeouts timeout line(s): the barrier did not hold"
else
  verdict ok "no quiesce drain timed out" "every quiesce drained within the bound"
fi
error_lines="$(grep -icE '\[(error|fatal)\]' "$CPA_LOG" || true)"
[[ "$error_lines" == "0" ]] && verdict ok "CPA logged no errors" "0 error/fatal lines" \
  || verdict bad "CPA logged no errors" "$error_lines error/fatal lines"
if grep -qiE '^panic|\bpanic:' "$CPA_LOG"; then
  verdict bad "CPA did not panic" "panic in the log"
else
  verdict ok "CPA did not panic" "clean"
fi

# ------------------------------------------------------------------ verdict
printf 'results\n'
for line in "${RESULTS[@]}"; do printf '  %s\n' "$line"; done
printf '\n'

failures=0
for line in "${RESULTS[@]}"; do [[ "$line" == FAIL* ]] && failures=$((failures + 1)); done

if [[ $failures -ne 0 ]]; then
  log "diagnostics"
  printf '  cpa log (last 40 lines):\n';        tail -40 "$CPA_LOG" 2>/dev/null | sed 's/^/    /' || true
  printf '  upstream log (last 20 lines):\n';   docker logs "$OWNED_UPSTREAM_ID" 2>&1 | tail -20 | sed 's/^/    /' || true
  printf '  plugin directory:\n';               ls -la "$WORKDIR/plugins" 2>/dev/null | sed 's/^/    /' || true
  printf '  temp dir kept at %s\n' "$WORKDIR"
  KEEP=1
  fail "$failures assertion(s) failed"
fi

if [[ $DEBUG_ONLY -eq 1 ]]; then
  printf 'DEBUG RUN OK  plumbing works end to end (%ss request), but this is NOT an acceptance verdict:\n' "$REQUEST_SECONDS"
  printf '              rerun without --debug and with HOTSWAP_STREAM_SECONDS >= %s to produce PASS\n' "$MIN_ACCEPTANCE_SECONDS"
  exit 0
fi

printf 'PASS  hot swap verified: a %ss request (measured) crossed the swap intact, the CPA process kept PID %s, and v%s is live\n' "$REQUEST_SECONDS" "$BEFORE_PID" "$V2_VERSION"
