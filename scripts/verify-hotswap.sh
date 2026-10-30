#!/usr/bin/env bash
# Hot-swap verification: install plugin v1, hold a slow inference request open,
# swap the plugin file to v2 mid-stream, and prove the request finishes intact
# while the CPA process keeps its PID.
#
#   scripts/verify-hotswap.sh                # build both versions, run, report
#   scripts/verify-hotswap.sh --keep         # keep the temp dir for inspection
#   scripts/verify-hotswap.sh --timeout 180  # bound the streaming request (s)
#
# This script never touches a running CPA. It refuses to start when the fixed
# test port is taken, when a container with its name already exists, or when its
# temp directory is not brand new; and it deletes its containers and temp
# directory on every exit path.
#
# Requires docker, curl and python3 (stdlib only). No Go on the host: both plugin
# versions and the fake upstream are built in golang:1.26-bookworm.
#
# Environment knobs: HOTSWAP_PORT, HOTSWAP_UPSTREAM_PORT, HOTSWAP_STREAM_SECONDS,
# HOTSWAP_SWAP_AT_SECONDS.
set -euo pipefail

cd "$(dirname "$0")/.."

# ---------------------------------------------------------------- identities
# These are asserted during preflight; do not point them at a real CPA.
CONTAINER="ollama-hotswap-test"
UPSTREAM_CONTAINER="ollama-hotswap-upstream"
PORT="${HOTSWAP_PORT:-18999}"
UPSTREAM_PORT="${HOTSWAP_UPSTREAM_PORT:-18080}"
CPA_IMAGE="eceasy/cli-proxy-api:v8.0.47"
GO_IMAGE="golang:1.26-bookworm"
MGMT_KEY="hotswap-local-mgmt-key"
PLUGIN_ID="ollama-quota"
CLIENT_KEY="hotswap-local-client-key"

# The two versions are derived from the source, not hard-coded, so the script
# stays correct when the tree's Version changes. v1 is whatever is committed;
# v2 is a second genuine compile with the patch level bumped by one.
# Resolved in preflight, once fail() exists and the source file is readable.
SOURCE_VERSION=""
V1_VERSION=""
V2_VERSION=""
STREAM_SECONDS="${HOTSWAP_STREAM_SECONDS:-40}"
SWAP_AT_SECONDS="${HOTSWAP_SWAP_AT_SECONDS:-8}"
REQUEST_TIMEOUT=180
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --timeout) REQUEST_TIMEOUT="${2:-180}"; shift 2 ;;
    -h|--help) sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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

WORKDIR=""
CLEANED=0
cleanup() {
  local status=$?
  [[ $CLEANED -eq 1 ]] && return
  CLEANED=1
  set +e
  log "cleanup"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 && note "removed container $CONTAINER"
  docker rm -f "$UPSTREAM_CONTAINER" >/dev/null 2>&1 && note "removed container $UPSTREAM_CONTAINER"
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

# python vs python3 depends on the host; resolve it once so every call site uses
# the same binary. This runs before the preflight that would otherwise report it.
if command -v python >/dev/null 2>&1; then
  PYTHON="python"
elif command -v python3 >/dev/null 2>&1; then
  PYTHON="python3"
else
  printf '\nFAIL: python3 (standard library only) is needed to read JSON and probe ports\n' >&2
  exit 1
fi

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
log "preflight"
command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
command -v curl   >/dev/null 2>&1 || fail "curl is not on PATH"
if ! command -v python >/dev/null 2>&1 && ! command -v python3 >/dev/null 2>&1; then
  fail "python3 (standard library only) is needed to read JSON responses"
fi

for p in "$PORT" "$UPSTREAM_PORT"; do
  if port_in_use "$p"; then
    cat >&2 <<MSG
FAIL: port $p already has a listener.
    Refusing to start: this script must never share a port with a running CPA
    or anything else. Choose another port with HOTSWAP_PORT=... and
    HOTSWAP_UPSTREAM_PORT=..., or stop whatever holds it.
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
    that name. Inspect it; if it is stale, remove it yourself with
        docker rm -f $c
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

# mktemp -d creates a fresh directory; assert it rather than assume it.
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/ollama-hotswap.XXXXXX")"
[[ -d "$WORKDIR" && -z "$(ls -A "$WORKDIR")" ]] || fail "temp dir $WORKDIR is not new and empty"
note "temp dir $WORKDIR is new"
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
# the new file as an upgrade instead of the same plugin.
#
# plugin.go is stamped in place and restored from a saved copy on every path, so
# a failed build cannot leave the tree stamped.
PLUGIN_GO="internal/quotaplugin/plugin.go"
build_version() { # build_version <version>
  local version="$1" saved applied
  saved="$(cat "$PLUGIN_GO")"
  restore() { printf '%s\n' "$saved" > "$PLUGIN_GO"; }

  printf '%s\n' "$saved" \
    | sed -E "s/^([[:space:]]*Version[[:space:]]*=[[:space:]]*)\"0\.[0-9]+\.[0-9]+\"/\1\"$version\"/" \
    > "$PLUGIN_GO"
  applied="$(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' "$PLUGIN_GO" | head -1)"
  if [[ "$applied" != "$version" ]]; then
    restore
    fail "could not stamp Version=$version into $PLUGIN_GO (found '$applied')"
  fi

  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(host_path "$PWD"):/src" -v "$(host_path "$WORKDIR/build"):/out" -w /src \
    "$GO_IMAGE" \
    sh -c "mkdir -p /tmp/gocache && export GOCACHE=/tmp/gocache && \
           CGO_ENABLED=1 go build -buildmode=c-shared -o '/out/$PLUGIN_ID-v$version.so' ./cmd/plugin" \
    || { restore; fail "build of $version failed"; }
  restore
}

build_version "$V1_VERSION"
build_version "$V2_VERSION"

for version in "$V1_VERSION" "$V2_VERSION"; do
  [[ -s "$WORKDIR/build/$PLUGIN_ID-v$version.so" ]] || fail "the $version build produced no library"
done
if cmp -s "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" "$WORKDIR/build/$PLUGIN_ID-v$V2_VERSION.so"; then
  fail "v1 and v2 are byte-identical; the swap would not be an upgrade"
fi
note "v1 $(wc -c < "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" | tr -d ' ') bytes"
note "v2 $(wc -c < "$WORKDIR/build/$PLUGIN_ID-v$V2_VERSION.so" | tr -d ' ') bytes, differs from v1"
note "the tree's plugin.go is back to $(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' "$PLUGIN_GO" | head -1)"

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
log "starting the fake upstream on 127.0.0.1:$UPSTREAM_PORT"
MSYS_NO_PATHCONV=1 docker run -d --rm --name "$UPSTREAM_CONTAINER" \
  -p "127.0.0.1:$UPSTREAM_PORT:$UPSTREAM_PORT" \
  -e "ADDR=:$UPSTREAM_PORT" -e "STREAM_SECONDS=$STREAM_SECONDS" \
  -v "$(host_path "$WORKDIR/build"):/fixture:ro" \
  "$GO_IMAGE" /fixture/upstream >/dev/null || fail "the fake upstream container did not start"

upstream_health() { curl -fsS -m 2 "http://127.0.0.1:$UPSTREAM_PORT/healthz" >/dev/null; }
if ! wait_for 45 upstream_health; then
  docker logs "$UPSTREAM_CONTAINER" 2>&1 | tail -20 >&2 || true
  fail "the fake upstream never became healthy"
fi
note "upstream healthy"

log "starting the isolated CPA instance on 127.0.0.1:$PORT"
cp "$WORKDIR/build/$PLUGIN_ID-v$V1_VERSION.so" "$WORKDIR/plugins/"
MSYS_NO_PATHCONV=1 docker run -d --rm --name "$CONTAINER" \
  -p "127.0.0.1:$PORT:$PORT" \
  -v "$(host_path "$WORKDIR/config.yaml"):/CLIProxyAPI/config.yaml" \
  -v "$(host_path "$WORKDIR/plugins"):/data/plugins" \
  -v "$(host_path "$WORKDIR/auths"):/data/auths" \
  "$CPA_IMAGE" ./CLIProxyAPI --config /CLIProxyAPI/config.yaml >/dev/null \
  || fail "the CPA container did not start"

cpa_health() { curl -fsS -m 3 -H "Authorization: Bearer $MGMT_KEY" "http://127.0.0.1:$PORT/v0/management/plugins" >/dev/null; }
if ! wait_for 60 cpa_health; then
  docker logs "$CONTAINER" 2>&1 | tail -30 >&2 || true
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
cpa_process() { docker inspect "$CONTAINER" --format '{{.State.Pid}}|{{.State.StartedAt}}|{{.RestartCount}}' 2>/dev/null || true; }

before="$(cpa_process)"
[[ -n "$before" ]] || fail "cannot inspect the CPA container"
BEFORE_PID="${before%%|*}"
rest="${before#*|}"; BEFORE_STARTED="${rest%%|*}"; BEFORE_RESTARTS="${rest##*|}"
[[ "$BEFORE_PID" != "0" && -n "$BEFORE_PID" ]] || fail "the CPA process is not running"

log "starting a ${STREAM_SECONDS}s streaming request"
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

mid="$(cpa_process)"
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
  docker logs "$CONTAINER" 2>&1 | tail -30 >&2 || true
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

after="$(cpa_process)"
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
printf '  ended           %s (after %ss)\n' "$REQUEST_ENDED_UTC" "$((REQUEST_END_EPOCH - REQUEST_START_EPOCH))"
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
docker logs "$CONTAINER" > "$CPA_LOG" 2>&1 || true
if grep -qiE "plugin hot reloaded.*active_version=$V2_VERSION" "$CPA_LOG"; then
  verdict ok "CPA logged a hot reload, not a reload from scratch" "active_version=$V2_VERSION"
else
  verdict bad "CPA logged a hot reload, not a reload from scratch" "no 'plugin hot reloaded' line for v$V2_VERSION"
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
  printf '  upstream log (last 20 lines):\n';   docker logs "$UPSTREAM_CONTAINER" 2>&1 | tail -20 | sed 's/^/    /' || true
  printf '  plugin directory:\n';               ls -la "$WORKDIR/plugins" 2>/dev/null | sed 's/^/    /' || true
  printf '  temp dir kept at %s\n' "$WORKDIR"
  KEEP=1
  fail "$failures assertion(s) failed"
fi

printf 'PASS  hot swap verified: the request crossed the swap intact, the CPA process kept PID %s, and v%s is live\n' "$BEFORE_PID" "$V2_VERSION"
