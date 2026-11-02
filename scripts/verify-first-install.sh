#!/usr/bin/env bash
# First installation during a long stream; adapted from verify-hotswap.sh.
# Isolated containers, ports 19999/19080, fake inference upstream.
set -euo pipefail

cd "$(dirname "$0")/.."

# ---------------------------------------------------------------- identities
CONTAINER="ollama-first-install-test"
UPSTREAM_CONTAINER="ollama-first-install-upstream"
PORT="${FIRST_INSTALL_PORT:-19999}"
UPSTREAM_PORT="${FIRST_INSTALL_UPSTREAM_PORT:-19080}"
CPA_IMAGE="eceasy/cli-proxy-api:v8.0.47"
GO_IMAGE="golang:1.26-bookworm"
MGMT_KEY="hotswap-local-mgmt-key"
PLUGIN_ID="ollama-quota"
CLIENT_KEY="hotswap-local-client-key"

# An acceptance run needs the request to demonstrably outlive 30 seconds, which
# is the SPEC 4.1 acceptance shape (SPEC 4.1: "发起一个耗时 30 秒以上的推理请求").
# Anything shorter is plumbing-only and runs under --debug.
MIN_ACCEPTANCE_SECONDS=31
STREAM_SECONDS="${FIRST_INSTALL_STREAM_SECONDS:-40}"
ENABLE_AT_SECONDS="${FIRST_INSTALL_ENABLE_AT_SECONDS:-8}"
REQUEST_TIMEOUT=180
DEBUG_ONLY=0

# Container IDs this run created. Empty means the run owns nothing and the
# cleanup path must not touch Docker at all.
OWNED_CPA_ID=""
OWNED_UPSTREAM_ID=""

# Build scratch lives outside the source tree; see build_version.
BUILD_SRC_DIR=""

SOURCE_VERSION=""
V1_VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --debug) DEBUG_ONLY=1; shift ;;
    --timeout) REQUEST_TIMEOUT="${2:-180}"; shift 2 ;;
    -h|--help) echo "Usage: $0 [--debug] [--timeout SECONDS]"; exit 0 ;;
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
  if [[ -n "${REQUEST_PID:-}" ]]; then kill "$REQUEST_PID" 2>/dev/null; wait "$REQUEST_PID" 2>/dev/null; fi
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
    rm -rf "$WORKDIR" && note "removed $WORKDIR"
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
  [[ "$p" =~ ^[0-9]+$ && "$p" -gt 0 && "$p" -le 65535 ]] || fail "invalid port: $p"
  [[ "$p" != 18999 && "$p" != 18080 ]] || fail "hotswap ports reserved"
done
[[ "$PORT" != "$UPSTREAM_PORT" ]] || fail "ports must differ"
[[ "$STREAM_SECONDS" =~ ^[0-9]+$ && "$REQUEST_TIMEOUT" =~ ^[0-9]+$ ]] || fail "stream/timeout must be integers"
[[ "$REQUEST_TIMEOUT" -gt "$STREAM_SECONDS" ]] || fail "timeout must exceed stream duration"


for p in "$PORT" "$UPSTREAM_PORT"; do
  if port_in_use "$p"; then
    cat >&2 <<MSG
FAIL: port $p already has a listener.
    Refusing to start: this script must never share a port with a running CPA
    or anything else. Choose another port with FIRST_INSTALL_PORT=... and
    FIRST_INSTALL_UPSTREAM_PORT=..., or stop whatever holds it.
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

# Read the version to install from the unmodified source.
SOURCE_VERSION="$(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' internal/quotaplugin/plugin.go | head -1)"
if [[ ! "$SOURCE_VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  fail "cannot read an x.y.z Version from internal/quotaplugin/plugin.go (found '$SOURCE_VERSION')"
fi
V1_VERSION="$SOURCE_VERSION"
note "first install version: v$V1_VERSION"

# Accepting a short stream would let the script print an acceptance PASS for a
# request that never outlived 30 seconds, which is not what this ticket claims.
if ! [[ "$STREAM_SECONDS" =~ ^[0-9]+$ ]]; then
  fail "FIRST_INSTALL_STREAM_SECONDS must be an integer (got '$STREAM_SECONDS')"
fi
if [[ $DEBUG_ONLY -eq 0 && "$STREAM_SECONDS" -lt "$MIN_ACCEPTANCE_SECONDS" ]]; then
  cat >&2 <<MSG
FAIL: FIRST_INSTALL_STREAM_SECONDS=$STREAM_SECONDS is too short for an acceptance run.
    An acceptance run needs a request that outlives 30 seconds
    (FIRST_INSTALL_STREAM_SECONDS >= $MIN_ACCEPTANCE_SECONDS). For a fast plumbing
    check that never prints PASS, pass --debug.
    Nothing was started and nothing was removed.
MSG
  exit 1
fi
if ! [[ "$ENABLE_AT_SECONDS" =~ ^[0-9]+$ ]] || [[ "$ENABLE_AT_SECONDS" -ge "$STREAM_SECONDS" ]]; then
  fail "FIRST_INSTALL_ENABLE_AT_SECONDS must be an integer below FIRST_INSTALL_STREAM_SECONDS (got '$ENABLE_AT_SECONDS')"
fi
if [[ $DEBUG_ONLY -eq 1 ]]; then
  note "DEBUG MODE: this run proves plumbing only and will not print PASS"
fi

# mktemp -d creates a fresh directory; assert it rather than assume it.
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/ollama-first-install.XXXXXX")"
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

BUILD_SRC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/first-install-src.XXXXXX")"
saved_workdir="$WORKDIR"
WORKDIR="$BUILD_SRC_DIR"
mount_overlap_scan
WORKDIR="$saved_workdir"

# --------------------------------------------------------------- fixtures
log "building the fixtures (fake upstream + plugin v$V1_VERSION)"

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
  enabled: false
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
OWNED_CPA_ID="$(MSYS_NO_PATHCONV=1 docker run -d --rm --name "$CONTAINER" \
  -p "127.0.0.1:$PORT:$PORT" \
  -v "$(host_path "$WORKDIR/config.yaml"):/CLIProxyAPI/config.yaml" \
  -v "$(host_path "$WORKDIR/plugins"):/data/plugins" \
  -v "$(host_path "$WORKDIR/auths"):/data/auths" \
  "$CPA_IMAGE" ./CLIProxyAPI --config /CLIProxyAPI/config.yaml)" \
  || fail "the CPA container did not start"
[[ -n "$OWNED_CPA_ID" ]] || fail "docker run returned no container ID for CPA"
note "CPA container $OWNED_CPA_ID"
CPA_STARTED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

cpa_health() { curl -fsS -m 3 -H "Authorization: Bearer $MGMT_KEY" "http://127.0.0.1:$PORT/v0/management/plugins" >/dev/null; }
if ! wait_for 60 cpa_health; then
  docker logs "$OWNED_CPA_ID" 2>&1 | tail -30 >&2 || true
  fail "the CPA instance never answered on port $PORT"
fi
note "CPA answered"

mgmt() { curl -fsS -m 10 -H "Authorization: Bearer $MGMT_KEY" "$@"; }

log "proving initial disabled state"
mgmt "http://127.0.0.1:$PORT/v0/management/plugins" > "$WORKDIR/initial-plugins.json"
cat "$WORKDIR/initial-plugins.json"; echo
"$PYTHON" -c 'import json,sys; assert not any(p.get("id")=="ollama-quota" for p in json.load(open(sys.argv[1])).get("plugins",[]))' "$WORKDIR/initial-plugins.json" || fail "plugin already present"
mgmt "http://127.0.0.1:$PORT/v0/management/config" > "$WORKDIR/initial-config.json"
"$PYTHON" -c 'import json,sys; assert not json.load(open(sys.argv[1])).get("plugins",{}).get("enabled",False)' "$WORKDIR/initial-config.json" || fail "subsystem already enabled"
docker logs "$OWNED_CPA_ID" > "$WORKDIR/initial.log" 2>&1
if grep -q 'plugin loaded' "$WORKDIR/initial.log"; then fail "already loaded"; fi
note "plugins.enabled=false, no ollama-quota entry, no plugin loaded log"
# Build through the repository entrypoint, in a disposable copy.
for entry in cmd internal scripts go.mod go.sum; do cp -R "$entry" "$BUILD_SRC_DIR/"; done
MSYS_NO_PATHCONV=1 docker run --rm -v "$(host_path "$BUILD_SRC_DIR"):/src" -w /src "$GO_IMAGE" bash scripts/build.sh --go || fail "plugin build failed"
[[ -s "$BUILD_SRC_DIR/dist/$PLUGIN_ID-v$V1_VERSION.so" ]] || fail "library missing"
cp "$BUILD_SRC_DIR/dist/$PLUGIN_ID-v$V1_VERSION.so" "$WORKDIR/build/"

cp "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" "$WORKDIR/plugins/"
note "copied .so to isolated plugins directory; no restart"

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

# Let the stream establish itself so first enable lands mid-body, not during setup.
sleep "$ENABLE_AT_SECONDS"
if ! kill -0 "$REQUEST_PID" 2>/dev/null; then
  fail "the streaming request ended before first enable, so it proves nothing; raise FIRST_INSTALL_STREAM_SECONDS"
fi

mid="$(cpa_process "$OWNED_CPA_ID")"
MID_PID="${mid%%|*}"
note "PID during the request: $MID_PID (before: $BEFORE_PID)"

log "enabling subsystem during the stream"
ENABLE_STARTED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ENABLE_BODY='{"plugins":{"enabled":true,"dir":"/data/plugins","configs":{"ollama-quota":{"enabled":true}}}}'
ENABLE_ROUTE=""
for method in PATCH PUT; do
  code="$(curl -sS -m 10 -o "$WORKDIR/enable-$method.out" -w '%{http_code}' -H "Authorization: Bearer $MGMT_KEY" -H 'Content-Type: application/json' -X "$method" -d "$ENABLE_BODY" "http://127.0.0.1:$PORT/v8/management/config")"
  note "$method /v8/management/config -> HTTP $code: $(cat "$WORKDIR/enable-$method.out")"
  if [[ "$code" == 2* ]]; then ENABLE_ROUTE="$method /v8/management/config"; break; fi
  [[ "$code" == 404 || "$code" == 405 ]] || fail "unexpected API response: $code"
done
if [[ -z "$ENABLE_ROUTE" ]]; then
  # Write from inside the container: Desktop host writes may not emit inotify.
  # Only this run's isolated config is edited; retain the management secret hash.
  MSYS_NO_PATHCONV=1 docker exec "$OWNED_CPA_ID" sh -c 'sed "/^plugins:/,/^openai-compatibility:/s/enabled: false/enabled: true/; s/configs: {}/configs: {ollama-quota: {enabled: true}}/" /CLIProxyAPI/config.yaml > /tmp/first-install-config.yaml && cat /tmp/first-install-config.yaml > /CLIProxyAPI/config.yaml'
  ENABLE_ROUTE="config.yaml edit via docker exec (watcher)"
fi
note "actual enable path: $ENABLE_ROUTE"
registered_v1() { [[ "$(active_version)" == "$V1_VERSION" ]]; }
if ! wait_for 45 registered_v1; then
  docker logs "$OWNED_CPA_ID" 2>&1 | tail -30 >&2 || true
  fail "plugin did not register"
fi
ENABLE_CONFIRMED_EPOCH="$(date +%s)"
ENABLE_CONFIRMED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
mgmt "http://127.0.0.1:$PORT/v0/management/plugins" > "$WORKDIR/final-plugins.json"
cat "$WORKDIR/final-plugins.json"; echo
note "host reports v$V1_VERSION registered=true"

if ! kill -0 "$REQUEST_PID" 2>/dev/null; then
  fail "the request finished before first enable completed; raise FIRST_INSTALL_STREAM_SECONDS"
fi
note "the request is still streaming across first enable"

REQUEST_STATUS=0
wait "$REQUEST_PID" || REQUEST_STATUS=$?
REQUEST_PID=""
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
printf '  enable confirmed  +%ss\n' "$((ENABLE_CONFIRMED_EPOCH - REQUEST_START_EPOCH))"
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

[[ $REQUEST_START_EPOCH -lt $ENABLE_CONFIRMED_EPOCH && $REQUEST_END_EPOCH -gt $ENABLE_CONFIRMED_EPOCH ]] \
  && verdict ok "the request spanned first enable" \
       "started ${REQUEST_STARTED_UTC}, enable at +$((ENABLE_CONFIRMED_EPOCH - REQUEST_START_EPOCH))s, ended ${REQUEST_ENDED_UTC}" \
  || verdict bad "the request spanned first enable" "it does not straddle first enable"

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
[[ "$final_version" == "$V1_VERSION" ]] \
  && verdict ok "the new plugin version is live" "v$V1_VERSION" \
  || verdict bad "the new plugin version is live" "reports '$final_version'"

CPA_LOG="$WORKDIR/cpa.log"
docker logs "$OWNED_CPA_ID" > "$CPA_LOG" 2>&1 || true
for evidence in 'plugin loaded plugin_id=ollama-quota' 'plugin registered plugin_id=ollama-quota'; do
  if grep -F "$evidence" "$CPA_LOG"; then verdict ok "CPA logged $evidence";
  else verdict bad "CPA logged $evidence"; fi
done
[[ "$mid" == "$before" && "$after" == "$before" ]] && verdict ok "all identity fields unchanged before/during/after" || verdict bad "all identity fields unchanged before/during/after"
verdict ok "initial subsystem disabled without loaded plugin" "checked API and logs before copying .so"
printf '  CPA started     %s
  enable began    %s
  registered      %s
  enable path     %s
' "$CPA_STARTED_UTC" "$ENABLE_STARTED_UTC" "$ENABLE_CONFIRMED_UTC" "$ENABLE_ROUTE"
# A timed-out quiesce drain must be visible here, not silently accepted. Clean
# runs must show none; a run that shows one has a real finding to report.
if grep -qiE 'quiesce drain timed out|quiesce_drain_timeout' "$CPA_LOG"; then
  drain_timeouts="$(grep -icE 'quiesce drain timed out|quiesce_drain_timeout' "$CPA_LOG" || true)"
  verdict bad "no quiesce drain timed out" "$drain_timeouts timeout line(s): the barrier did not hold"
else
  verdict ok "no quiesce drain timeout was logged" "first installation does not exercise replacement drain"
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
  fail "$failures assertion(s) failed"
fi

if [[ $DEBUG_ONLY -eq 1 ]]; then
  printf 'DEBUG RUN OK  plumbing works end to end (%ss request), but this is NOT an acceptance verdict:\n' "$REQUEST_SECONDS"
  printf '              rerun without --debug and with FIRST_INSTALL_STREAM_SECONDS >= %s to produce PASS\n' "$MIN_ACCEPTANCE_SECONDS"
  exit 0
fi

printf 'PASS  first install verified: a %ss request (measured) crossed first enable intact, the CPA process kept PID %s, and v%s is live\n' "$REQUEST_SECONDS" "$BEFORE_PID" "$V1_VERSION"
