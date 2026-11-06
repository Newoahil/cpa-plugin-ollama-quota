#!/usr/bin/env bash
# Same-artifact hot-swap regression: the candidate is the party being replaced.
#
# The PR #21 review (F1) found that a long stream broke when the plugin was
# hot-replaced by a byte-identical artifact from a new directory: the old
# executor pump's host callbacks were routed through the new generation's
# host_ctx, and the host rejected them as belonging to another plugin instance.
#
# This script reproduces that exact shape against a real CPA process:
#   1. register an ollama auth and enable the plugin from /plugins/a;
#   2. start a long streaming request that CPA routes into the plugin executor
#      (the model comes from the plugin's model discovery, not from an
#      openai-compatibility upstream — that would bypass the pump and prove
#      nothing);
#   3. PATCH the plugins config to /plugins/b, which holds the identical bytes;
#   4. wait for CPA's own "plugin hot reloaded" log line naming both paths.
#
# The fake Ollama endpoint is an HTTPS fixture reached as ollama.com through a
# Docker network alias. It holds no real credential: the auth file under test is
# a fixture API key, and the fixture rejects anything else.
#
# PASS requires all of:
#   * the fixture saw the chat request on ollama.com with the auth matched;
#   * CPA logged the hot reload from /plugins/a to /plugins/b while the request
#     was open (a PATCH 200 is not evidence that a replacement happened);
#   * the response carried every token in order, finish_reason=stop, usage and
#     exactly one [DONE];
#   * at least one token arrived after the reload (the old pump survived it);
#   * the quiesce barrier reported the live executor obligation to the host, so
#     the swap really raced the pump rather than an idle library;
#   * PID, StartedAt and RestartCount did not change. Those are necessary, not
#     sufficient: the same checks pass while the stream is truncated.
#
#   scripts/verify-hotswap-same-artifact.sh            # acceptance run
#   scripts/verify-hotswap-same-artifact.sh --keep     # keep the temp dir
#   scripts/verify-hotswap-same-artifact.sh --debug    # plumbing only, never PASS
#   scripts/verify-hotswap-same-artifact.sh --timeout 240
#
# This script never touches a running CPA. Ownership is by container ID: only a
# container this run created (its ID was printed by this run's `docker run`) is
# ever removed, and every preflight refusal exits before any container exists.
# The network is removed only when this run created it.
#
# Requires docker, curl and python3 (stdlib only). No Go on the host: the plugin
# and the fixture are built in golang:1.26-bookworm.
#
# Environment knobs: SAME_ARTIFACT_PORT, SAME_ARTIFACT_STREAM_SECONDS,
# SAME_ARTIFACT_SWAP_AT_SECONDS, SAME_ARTIFACT_CPA_IMAGE, SAME_ARTIFACT_GO_IMAGE.
set -euo pipefail

cd "$(dirname "$0")/.."

# ---------------------------------------------------------------- identities
CONTAINER="ollama-same-artifact-test"
FIXTURE_CONTAINER="ollama-same-artifact-fixture"
NETWORK="ollama-same-artifact-net"
PORT="${SAME_ARTIFACT_PORT:-18998}"
CPA_IMAGE="${SAME_ARTIFACT_CPA_IMAGE:-eceasy/cli-proxy-api:v8.0.47}"
GO_IMAGE="${SAME_ARTIFACT_GO_IMAGE:-golang:1.26-bookworm}"
MGMT_KEY="same-artifact-local-mgmt-key"
PLUGIN_ID="ollama-quota"
CLIENT_KEY="same-artifact-local-client-key"
FIXTURE_KEY="same-artifact-fixture-key-not-real"
MODEL="same-artifact-model"

# The stream must outlive 30 seconds (SPEC 4.1) and must still be emitting when
# the reload lands. The replacement waits behind the old generation's quiesce:
# the collector's stop budget (attemptTimeout + grace) plus the bridge budget is
# about 27 seconds, so the default stream leaves room for tokens on both sides.
MIN_ACCEPTANCE_SECONDS=31
STREAM_SECONDS="${SAME_ARTIFACT_STREAM_SECONDS:-60}"
SWAP_AT_SECONDS="${SAME_ARTIFACT_SWAP_AT_SECONDS:-5}"
REQUEST_TIMEOUT=240
# The replacement does not start until quiesce gives up on the live pump; give
# the host time to log it rather than assuming a fixed quiesce duration.
RELOAD_TIMEOUT=150
KEEP=0
DEBUG_ONLY=0

# Container IDs this run created. Empty means the run owns nothing and the
# cleanup path must not touch Docker at all.
OWNED_CPA_ID=""
OWNED_FIXTURE_ID=""
OWNED_NETWORK=0
BUILD_SRC_DIR=""

SOURCE_VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --debug) DEBUG_ONLY=1; shift ;;
    --timeout) REQUEST_TIMEOUT="${2:-240}"; shift 2 ;;
    -h|--help) sed -n '2,48p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
  if [[ -n "$OWNED_CPA_ID" ]]; then
    if docker rm -f "$OWNED_CPA_ID" >/dev/null 2>&1; then
      note "removed the CPA container this run started ($OWNED_CPA_ID)"
    fi
  fi
  if [[ -n "$OWNED_FIXTURE_ID" ]]; then
    if docker rm -f "$OWNED_FIXTURE_ID" >/dev/null 2>&1; then
      note "removed the fixture container this run started ($OWNED_FIXTURE_ID)"
    fi
  fi
  if [[ "$OWNED_NETWORK" -eq 1 ]]; then
    if docker network rm "$NETWORK" >/dev/null 2>&1; then
      note "removed the network this run created ($NETWORK)"
    else
      note "could not remove the network $NETWORK (left in place)"
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

# ---------------------------------------------------------------- preflight
log "preflight"
command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
command -v curl   >/dev/null 2>&1 || fail "curl is not on PATH"

if port_in_use "$PORT"; then
  cat >&2 <<MSG
FAIL: port $PORT already has a listener.
    Refusing to start: this script must never share a port with a running CPA
    or anything else. Choose another port with SAME_ARTIFACT_PORT=..., or stop
    whatever holds it.
    Nothing was started and nothing was removed.
MSG
  exit 1
fi
note "port $PORT is free"

for c in "$CONTAINER" "$FIXTURE_CONTAINER"; do
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

if docker network inspect "$NETWORK" >/dev/null 2>&1; then
  cat >&2 <<MSG
FAIL: network $NETWORK already exists.
    Refusing to start: it may belong to a concurrent run. This script removes a
    network only when it created it. Inspect it; if it is stale, remove it
    yourself with
        docker network rm $NETWORK
    Nothing was started and nothing was removed.
MSG
  exit 1
fi
note "network name $NETWORK is unused"

SOURCE_VERSION="$(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' internal/quotaplugin/plugin.go | head -1)"
if [[ ! "$SOURCE_VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  fail "cannot read an x.y.z Version from internal/quotaplugin/plugin.go (found '$SOURCE_VERSION')"
fi
note "plugin version under test: v$SOURCE_VERSION"

if ! [[ "$STREAM_SECONDS" =~ ^[0-9]+$ ]]; then
  fail "SAME_ARTIFACT_STREAM_SECONDS must be an integer (got '$STREAM_SECONDS')"
fi
if [[ $DEBUG_ONLY -eq 0 && "$STREAM_SECONDS" -lt "$MIN_ACCEPTANCE_SECONDS" ]]; then
  cat >&2 <<MSG
FAIL: SAME_ARTIFACT_STREAM_SECONDS=$STREAM_SECONDS is too short for an acceptance run.
    An acceptance run needs a request that outlives 30 seconds
    (SAME_ARTIFACT_STREAM_SECONDS >= $MIN_ACCEPTANCE_SECONDS). For a fast
    plumbing check that never prints PASS, pass --debug.
    Nothing was started and nothing was removed.
MSG
  exit 1
fi
if ! [[ "$SWAP_AT_SECONDS" =~ ^[0-9]+$ ]] || [[ "$SWAP_AT_SECONDS" -ge "$STREAM_SECONDS" ]]; then
  fail "SAME_ARTIFACT_SWAP_AT_SECONDS must be an integer below SAME_ARTIFACT_STREAM_SECONDS (got '$SWAP_AT_SECONDS')"
fi
if [[ $DEBUG_ONLY -eq 1 ]]; then
  note "DEBUG MODE: this run proves plumbing only and will not print PASS"
fi

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/ollama-same-artifact.XXXXXX")"
[[ -d "$WORKDIR" && -z "$(ls -A "$WORKDIR")" ]] || fail "temp dir $WORKDIR is not new and empty"
note "temp dir $WORKDIR is new"

# A brand-new directory is not on its own proof that no running CPA can see it:
# if a CPA has a parent directory bind-mounted, a fresh temp dir inside it is
# visible to that CPA. Compare against every running container's mount sources
# and refuse on any equality, ancestor, or descendant overlap. Read-only.
mount_check_failure() {
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

  if ! ps_out="$(docker ps -q 2>"$WORKDIR/docker-ps.err")"; then
    mount_check_failure "docker ps -q failed: $(head -3 "$WORKDIR/docker-ps.err" 2>/dev/null | tr '\n' ' ')"
  fi
  mapfile -t running_ids <<<"$ps_out"
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

mkdir -p "$WORKDIR/plugins/a" "$WORKDIR/plugins/b" "$WORKDIR/auths" "$WORKDIR/build" "$WORKDIR/fixture"

for image in "$CPA_IMAGE" "$GO_IMAGE"; do
  docker image inspect "$image" >/dev/null 2>&1 || { log "pulling $image"; docker pull "$image" >/dev/null || fail "cannot pull $image"; }
done
note "images present: $CPA_IMAGE, $GO_IMAGE"

# --------------------------------------------------------------- fixtures
log "building the HTTPS Ollama fixture and the plugin artifact"

cat > "$WORKDIR/build/fixture.go" <<'GO'
// Fake Ollama endpoint for the same-artifact hot-swap regression.
//
// It serves HTTPS for the Docker network alias ollama.com with a certificate
// generated into /fixture-data at startup, and records every upstream request
// as one JSON object per line in /fixture-data/events.jsonl. The credential is
// a fixture key, never a real one.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const fixtureKey = "same-artifact-fixture-key-not-real"
const fixtureModel = "same-artifact-model"

var mu sync.Mutex
var events *os.File
var serial atomic.Int64

func record(id int64, event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["id"], fields["event"], fields["at"] = id, event, time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(fields)
	mu.Lock()
	if events != nil {
		_, _ = events.Write(append(raw, '\n'))
		_ = events.Sync()
	}
	mu.Unlock()
	fmt.Println(string(raw))
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func upstream(w http.ResponseWriter, r *http.Request) {
	id := serial.Add(1)
	matched := r.Header.Get("Authorization") == "Bearer "+fixtureKey
	record(id, "request", map[string]any{
		"path": r.URL.Path, "host": r.Host, "method": r.Method,
		"auth_match": matched, "accept": r.Header.Get("Accept"),
	})
	if !matched {
		reply(w, http.StatusUnauthorized, map[string]string{"error": "fixture authentication failed"})
		return
	}
	switch r.URL.Path {
	case "/api/tags":
		reply(w, http.StatusOK, map[string]any{
			"models": []any{map[string]string{"name": fixtureModel, "model": fixtureModel}},
		})
	case "/api/balance":
		reply(w, http.StatusOK, map[string]any{
			"plan": "pro",
			"included": map[string]any{
				"balance_usd": 12.4, "allowance_usd": 20,
				"session": map[string]any{"remaining_percent": 70, "resets_at": time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)},
				"weekly":  map[string]any{"remaining_percent": 20, "resets_at": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)},
			},
		})
	case "/v1/chat/completions":
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Model != fixtureModel {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid fixture payload"})
			return
		}
		tokens := 40
		if raw := os.Getenv("TOKENS"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				tokens = parsed
			}
		}
		record(id, "start", map[string]any{"stream": body.Stream, "tokens": tokens})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		emit := func(payload any) error {
			raw, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": "chatcmpl-same-artifact", "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": fixtureModel,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}
		sent := 0
		for i := 1; i <= tokens; i++ {
			if err := emit(chunk(map[string]any{"content": fmt.Sprintf("TOKEN-%03d ", i)}, nil)); err != nil {
				record(id, "cancelled", map[string]any{"sent_tokens": sent})
				return
			}
			sent = i
			if i == 1 {
				record(id, "first_frame", nil)
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-r.Context().Done():
				timer.Stop()
				record(id, "cancelled", map[string]any{"sent_tokens": sent})
				return
			case <-timer.C:
			}
		}
		_ = emit(chunk(map[string]any{}, "stop"))
		_ = emit(map[string]any{
			"id": "chatcmpl-same-artifact", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": fixtureModel, "choices": []any{},
			"usage": map[string]int{"prompt_tokens": 8, "completion_tokens": tokens, "total_tokens": 8 + tokens},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		record(id, "complete", map[string]any{"sent_tokens": sent})
	default:
		reply(w, http.StatusNotFound, map[string]string{"error": "unknown fixture path"})
	}
}

func main() {
	if os.Getenv("ASSISTANT_NO_SEND") != "1" {
		log.Fatal("isolation flag ASSISTANT_NO_SEND=1 is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "same-artifact regression fixture only"},
		DNSNames:              []string{"ollama.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	if err = os.WriteFile("/fixture-data/fixture-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		log.Fatal(err)
	}
	events, err = os.OpenFile("/fixture-data/events.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]bool{"ok": true})
	})
	go func() {
		if errServe := http.ListenAndServe(":8080", mux); errServe != nil {
			log.Fatal(errServe)
		}
	}()
	server := &http.Server{
		Addr:              ":443",
		Handler:           http.HandlerFunc(upstream),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		},
	}
	log.Printf("fixture listening as https://ollama.com:443 (TOKENS=%s)", os.Getenv("TOKENS"))
	log.Fatal(server.ListenAndServeTLS("", ""))
}
GO

printf 'module sameartifactfixture\n\ngo 1.26\n' > "$WORKDIR/build/go.mod"

MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$(host_path "$WORKDIR/build"):/fixture" -w /fixture \
  "$GO_IMAGE" \
  sh -c 'mkdir -p /tmp/gocache && export GOCACHE=/tmp/gocache CGO_ENABLED=0 && go build -o /fixture/fixture .' \
  || fail "could not build the HTTPS fixture"
[[ -s "$WORKDIR/build/fixture" ]] || fail "the HTTPS fixture binary is missing"

# Build in a copy so an interrupt can never leave the working tree modified.
BUILD_SRC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/same-artifact-src.XXXXXX")"
for entry in cmd internal go.mod go.sum; do
  cp -R "$entry" "$BUILD_SRC_DIR/"
done
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$(host_path "$BUILD_SRC_DIR"):/src" -v "$(host_path "$WORKDIR/build"):/out" -w /src \
  "$GO_IMAGE" \
  sh -c "mkdir -p /tmp/gocache && export GOCACHE=/tmp/gocache && \
         CGO_ENABLED=1 go build -mod=readonly -buildvcs=false -buildmode=c-shared -o '/out/$PLUGIN_ID.so' ./cmd/plugin" \
  || fail "build of the plugin failed"
[[ -s "$WORKDIR/build/$PLUGIN_ID.so" ]] || fail "the plugin build produced no library"

ARTIFACT_A="$WORKDIR/plugins/a/$PLUGIN_ID-v$SOURCE_VERSION.so"
ARTIFACT_B="$WORKDIR/plugins/b/$PLUGIN_ID-v$SOURCE_VERSION.so"
cp "$WORKDIR/build/$PLUGIN_ID.so" "$ARTIFACT_A"
cp "$WORKDIR/build/$PLUGIN_ID.so" "$ARTIFACT_B"
cmp -s "$ARTIFACT_A" "$ARTIFACT_B" || fail "the two plugin directories do not hold identical bytes"
ARTIFACT_SHA="$("$PYTHON" - "$ARTIFACT_A" <<'PY'
import hashlib, sys
print(hashlib.sha256(open(sys.argv[1], 'rb').read()).hexdigest())
PY
)"
note "both plugin directories hold byte-identical artifacts: $ARTIFACT_SHA"

printf '%s\n' "{\"type\":\"ollama\",\"api_key\":\"$FIXTURE_KEY\"}" > "$WORKDIR/auths/same-artifact-fixture.json"

cat > "$WORKDIR/config.yaml" <<YAML
config-version: 8
server:
  host: "0.0.0.0"
  port: $PORT
management:
  allow-remote: true
  secret-key: "$MGMT_KEY"
  disable-control-panel: true
  disable-auto-update-panel: true
access:
  api-keys:
    - "$CLIENT_KEY"
oauth:
  auth-dir: "/data/auths"
observability:
  logs:
    debug: false
    logging-to-file: false
    request-log: false
routing:
  retry:
    request-retry: 0
requests:
  streaming:
    keepalive-seconds: 0
    bootstrap-retries: 0
plugins:
  enabled: false
  dir: "/plugins/a"
  configs: {}
YAML

# ------------------------------------------------------------------- start
log "creating the isolated network and starting the HTTPS fixture"
docker network create "$NETWORK" >/dev/null || fail "could not create network $NETWORK"
OWNED_NETWORK=1
note "network $NETWORK"

OWNED_FIXTURE_ID="$(MSYS_NO_PATHCONV=1 docker run -d --rm --name "$FIXTURE_CONTAINER" \
  --network "$NETWORK" --network-alias ollama.com --network-alias fixture \
  -e ASSISTANT_NO_SEND=1 -e "TOKENS=$STREAM_SECONDS" \
  -v "$(host_path "$WORKDIR/build"):/fixture-bin:ro" \
  -v "$(host_path "$WORKDIR/fixture"):/fixture-data" \
  "$GO_IMAGE" /fixture-bin/fixture)" || fail "the fixture container did not start"
[[ -n "$OWNED_FIXTURE_ID" ]] || fail "docker run returned no container ID for the fixture"
note "fixture container $OWNED_FIXTURE_ID"

ca_written() { [[ -s "$WORKDIR/fixture/fixture-ca.pem" ]]; }
if ! wait_for 30 ca_written; then
  docker logs "$OWNED_FIXTURE_ID" 2>&1 | tail -20 >&2 || true
  fail "the fixture never wrote its CA certificate"
fi

FIXTURE_IP="$(docker inspect "$OWNED_FIXTURE_ID" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')"
[[ -n "$FIXTURE_IP" ]] || fail "cannot read the fixture container IP"

# Prove the alias wins before CPA starts. If it did not, the plugin would send
# a fixture key to the real ollama.com, which is exactly what this test must
# never do.
RESOLVED_IP="$(MSYS_NO_PATHCONV=1 docker run --rm --network "$NETWORK" "$GO_IMAGE" \
  sh -c 'getent ahostsv4 ollama.com | head -1 | cut -d" " -f1')" || fail "could not resolve ollama.com inside the network"
[[ "$RESOLVED_IP" == "$FIXTURE_IP" ]] \
  || fail "ollama.com resolves to '$RESOLVED_IP', not the fixture at '$FIXTURE_IP'; refusing to start CPA"
note "ollama.com resolves to the fixture ($FIXTURE_IP)"

fixture_ready() {
  MSYS_NO_PATHCONV=1 docker run --rm --network "$NETWORK" \
    -e "FIXTURE_KEY=$FIXTURE_KEY" \
    -v "$(host_path "$WORKDIR/fixture"):/fixture-data:ro" \
    "$GO_IMAGE" \
    sh -c 'for i in $(seq 1 30); do \
             curl -fsS --max-time 2 --cacert /fixture-data/fixture-ca.pem \
               -H "Authorization: Bearer $FIXTURE_KEY" \
               https://ollama.com/api/tags >/dev/null && exit 0; \
             sleep 1; \
           done; exit 1'
}
fixture_ready || fail "the HTTPS fixture did not answer as ollama.com with its CA"
note "fixture answers on https://ollama.com with the fixture CA"

log "starting the isolated CPA instance on 127.0.0.1:$PORT"
OWNED_CPA_ID="$(MSYS_NO_PATHCONV=1 docker run -d --rm --name "$CONTAINER" \
  --network "$NETWORK" \
  -p "127.0.0.1:$PORT:$PORT" \
  -e SSL_CERT_FILE=/fixture-data/fixture-ca.pem \
  -e HTTP_PROXY= -e HTTPS_PROXY= -e ALL_PROXY= '-e' 'NO_PROXY=*' \
  -v "$(host_path "$WORKDIR/config.yaml"):/CLIProxyAPI/config.yaml" \
  -v "$(host_path "$WORKDIR/plugins"):/plugins:ro" \
  -v "$(host_path "$WORKDIR/auths"):/data/auths" \
  -v "$(host_path "$WORKDIR/fixture"):/fixture-data:ro" \
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

# The v8 config route applies runtime plugin config from the persisted document.
# It is the same path the plugin page uses, and changing `dir` is what makes the
# host load the identical bytes from a second directory.
plugins_config() { # plugins_config <dir>
  curl -fsS -m 120 -X PATCH -H "Authorization: Bearer $MGMT_KEY" -H "Content-Type: application/json" \
    -d "{\"enabled\":true,\"dir\":\"$1\",\"configs\":{\"$PLUGIN_ID\":{\"enabled\":true,\"interval_seconds\":60}}}" \
    "http://127.0.0.1:$PORT/v8/management/config/plugins" >/dev/null
}

log "enabling plugin v$SOURCE_VERSION from /plugins/a"
plugins_config "/plugins/a" || fail "could not enable the plugin from /plugins/a"

registered() { [[ "$(active_version)" == "$SOURCE_VERSION" ]]; }
if ! wait_for 30 registered; then
  mgmt "http://127.0.0.1:$PORT/v0/management/plugins" >&2 || true
  fail "the plugin never registered as v$SOURCE_VERSION"
fi
note "plugin registered as v$SOURCE_VERSION"

routable() { curl -fsS -m 5 -H "Authorization: Bearer $CLIENT_KEY" "http://127.0.0.1:$PORT/v1/models" | grep -q "$MODEL"; }
if ! wait_for 30 routable; then
  curl -sS -m 5 -H "Authorization: Bearer $CLIENT_KEY" "http://127.0.0.1:$PORT/v1/models" >&2 || true
  fail "CPA never listed $MODEL, so the request would not reach the plugin executor"
fi
note "model $MODEL is routable through the plugin executor"

# -------------------------------------------------------------- experiment
cpa_process() { docker inspect "$1" --format '{{.State.Pid}}|{{.State.StartedAt}}|{{.RestartCount}}' 2>/dev/null || true; }

before="$(cpa_process "$OWNED_CPA_ID")"
[[ -n "$before" ]] || fail "cannot inspect the CPA container"
BEFORE_PID="${before%%|*}"
rest="${before#*|}"; BEFORE_STARTED="${rest%%|*}"; BEFORE_RESTARTS="${rest##*|}"
[[ "$BEFORE_PID" != "0" && -n "$BEFORE_PID" ]] || fail "the CPA process is not running"

log "starting a ${STREAM_SECONDS}s streaming request through the plugin executor"
REQUEST_START_EPOCH="$(date +%s)"
REQUEST_STARTED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

curl -sS -N --max-time "$REQUEST_TIMEOUT" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"prove an identical-artifact hot swap does not cut this stream\"}]}" \
  "http://127.0.0.1:$PORT/v1/chat/completions" \
  > "$WORKDIR/stream.out" 2> "$WORKDIR/stream.err" &
REQUEST_PID=$!

saw_token() { grep -q 'TOKEN-' "$WORKDIR/stream.out"; }
if ! wait_for 45 saw_token; then
  cat "$WORKDIR/stream.err" >&2 || true
  docker logs "$OWNED_CPA_ID" 2>&1 | tail -30 >&2 || true
  fail "the streaming request never produced a token"
fi
note "request is streaming ($(grep -o 'TOKEN-[0-9]*' "$WORKDIR/stream.out" | wc -l | tr -d ' ') tokens so far)"

sleep "$SWAP_AT_SECONDS"
if ! kill -0 "$REQUEST_PID" 2>/dev/null; then
  fail "the streaming request ended before the swap, so it proves nothing; raise SAME_ARTIFACT_STREAM_SECONDS"
fi

mid="$(cpa_process "$OWNED_CPA_ID")"
MID_PID="${mid%%|*}"
note "PID during the request: $MID_PID (before: $BEFORE_PID)"

log "switching the plugin dir to /plugins/b (identical bytes) while the request is open"
PATCH_SENT_EPOCH="$(date +%s)"
PATCH_STATUS=0
plugins_config "/plugins/b" > "$WORKDIR/patch.out" 2>&1 &
PATCH_PID=$!

# Wait for CPA's own reload evidence, not for the PATCH response. A PATCH 200
# only says the config was accepted; it does not say the replacement happened.
# Polling before `wait` also snapshots the stream at the reload, not after the
# PATCH unblocks.
reloaded_to_b() {
  docker logs "$OWNED_CPA_ID" 2>&1 \
    | grep -E 'plugin hot reloaded' \
    | grep -q 'active_path=/plugins/b/'
}
if ! wait_for "$RELOAD_TIMEOUT" reloaded_to_b; then
  kill "$PATCH_PID" 2>/dev/null || true
  docker logs "$OWNED_CPA_ID" 2>&1 | tail -40 >&2 || true
  fail "CPA never logged a hot reload to /plugins/b within ${RELOAD_TIMEOUT}s"
fi
SWAP_CONFIRMED_EPOCH="$(date +%s)"
cp "$WORKDIR/stream.out" "$WORKDIR/stream.at-reload.out"
TOKENS_AT_RELOAD="$(grep -o 'TOKEN-[0-9]*' "$WORKDIR/stream.at-reload.out" | sort -u | wc -l | tr -d ' ')"
note "hot reload to /plugins/b logged; ${TOKENS_AT_RELOAD}/${STREAM_SECONDS} tokens had arrived"

wait "$PATCH_PID" || PATCH_STATUS=$?
[[ $PATCH_STATUS -eq 0 ]] || fail "the PATCH to /plugins/b failed: $(tail -3 "$WORKDIR/patch.out" 2>/dev/null | tr '\n' ' ')"

if kill -0 "$REQUEST_PID" 2>/dev/null; then
  note "the request is still streaming across the reload"
else
  # Not fatal here: the reload evidence exists, so let the token, span and
  # completeness assertions judge the request. They are the point of the test.
  note "the request had already ended when the PATCH returned"
fi

wait "$REQUEST_PID"; REQUEST_STATUS=$?
REQUEST_END_EPOCH="$(date +%s)"
REQUEST_ENDED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
REQUEST_SECONDS=$((REQUEST_END_EPOCH - REQUEST_START_EPOCH))

after="$(cpa_process "$OWNED_CPA_ID")"
AFTER_PID="${after%%|*}"
rest="${after#*|}"; AFTER_STARTED="${rest%%|*}"; AFTER_RESTARTS="${rest##*|}"

# -------------------------------------------------------------- assertions
log "assertions"
body_bytes="$(wc -c < "$WORKDIR/stream.out" | tr -d ' ')"
tokens_seen="$(grep -o 'TOKEN-[0-9]*' "$WORKDIR/stream.out" | sort -u | wc -l | tr -d ' ')"
TOKENS_AFTER_RELOAD=$((tokens_seen - TOKENS_AT_RELOAD))

STREAM_VALIDATION="$("$PYTHON" - "$WORKDIR/stream.out" "$STREAM_SECONDS" <<'PY'
import json
import sys

path, want = sys.argv[1], int(sys.argv[2])
raw = open(path, 'r', encoding='utf-8', errors='replace').read()
frames = []
done = 0
for block in raw.replace('\r\n', '\n').split('\n\n'):
    data_lines = [line[len('data:'):].lstrip(' ') for line in block.split('\n') if line.startswith('data:')]
    if not data_lines:
        continue
    value = '\n'.join(data_lines)
    if value == '[DONE]':
        done += 1
        continue
    frames.append(json.loads(value))

tokens, finish, usage, errors = [], None, None, []
for frame in frames:
    if 'error' in frame:
        errors.append(str(frame['error']))
    if frame.get('usage'):
        usage = frame['usage']
    for choice in frame.get('choices') or []:
        if choice.get('finish_reason'):
            finish = choice['finish_reason']
        delta = choice.get('delta') or {}
        content = delta.get('content')
        if content:
            tokens.append(content)

expected = ['TOKEN-%03d ' % i for i in range(1, want + 1)]
problems = []
if errors:
    problems.append('error frame(s): %r' % (errors[:2],))
if done != 1:
    problems.append('DONE count %d, want 1' % done)
if tokens != expected:
    got = tokens[:3] + ['...'] + tokens[-3:] if len(tokens) > 6 else tokens
    problems.append('tokens %r, want %d in order (got %d)' % (got, want, len(tokens)))
if finish != 'stop':
    problems.append('finish_reason %r, want stop' % finish)
if not usage or usage.get('completion_tokens') != want:
    problems.append('usage %r, want completion_tokens=%d' % (usage, want))
if problems:
    print('INVALID: ' + '; '.join(problems))
    sys.exit(1)
print('OK %d tokens in order, stop, usage, exactly one DONE' % len(tokens))
PY
)" || true
[[ -z "$STREAM_VALIDATION" ]] && STREAM_VALIDATION="INVALID: validator produced no output"

FIXTURE_EVIDENCE="$("$PYTHON" - "$WORKDIR/fixture/events.jsonl" <<'PY'
import json
import sys

events = []
try:
    with open(sys.argv[1], 'r', encoding='utf-8', errors='replace') as handle:
        for line in handle:
            line = line.strip()
            if line:
                events.append(json.loads(line))
except FileNotFoundError:
    print('INVALID: the fixture recorded no events')
    sys.exit(1)

chat = [e for e in events if e.get('event') == 'request' and e.get('path') == '/v1/chat/completions']
if not chat:
    print('INVALID: the fixture saw no /v1/chat/completions request; the stream did not go through the plugin')
    sys.exit(1)
foreign = [e for e in chat if not str(e.get('host', '')).split(':')[0].endswith('ollama.com')]
if foreign:
    print('INVALID: a chat request carried host %r, want ollama.com' % (foreign[0].get('host'),))
    sys.exit(1)
if not any(e.get('auth_match') for e in chat):
    print('INVALID: the fixture rejected the plugin credential on every chat request')
    sys.exit(1)
ids = {e['id'] for e in chat if e.get('auth_match')}
if not any(e.get('event') == 'first_frame' and e.get('id') in ids for e in events):
    print('INVALID: the fixture never emitted a first frame for an authenticated chat request')
    sys.exit(1)
starts = [e for e in events if e.get('event') == 'start' and e.get('id') in ids]
if not starts:
    print('INVALID: the fixture saw no stream start for an authenticated chat request')
    sys.exit(1)
print('OK chat request hit ollama.com with auth matched; first frame recorded')
PY
)" || true
[[ -z "$FIXTURE_EVIDENCE" ]] && FIXTURE_EVIDENCE="INVALID: fixture evidence produced no output"

CPA_LOG="$WORKDIR/cpa.log"
docker logs "$OWNED_CPA_ID" > "$CPA_LOG" 2>&1 || true
hot_line="$(grep -E 'plugin hot reloaded' "$CPA_LOG" | grep 'active_path=/plugins/b/' | tail -1 || true)"
quiesce_lines="$(grep -icE 'plugin quiesce failed' "$CPA_LOG" || true)"
error_lines="$(grep -icE '\[(error|fatal)\]' "$CPA_LOG" || true)"
final_version="$(active_version)"

printf '\nrequest timeline (UTC)\n'
printf '  started         %s\n' "$REQUEST_STARTED_UTC"
printf '  patch sent      +%ss\n' "$((PATCH_SENT_EPOCH - REQUEST_START_EPOCH))"
printf '  reload logged   +%ss\n' "$((SWAP_CONFIRMED_EPOCH - REQUEST_START_EPOCH))"
printf '  ended           %s\n' "$REQUEST_ENDED_UTC"
printf '  measured        %ss\n' "$REQUEST_SECONDS"
printf '  tokens          %s/%s at reload, %s after it\n' "$TOKENS_AT_RELOAD" "$STREAM_SECONDS" "$TOKENS_AFTER_RELOAD"
printf '  response        %s bytes\n' "$body_bytes"
printf '\nCPA process\n'
printf '  PID           before=%s  mid=%s  after=%s\n' "$BEFORE_PID" "$MID_PID" "$AFTER_PID"
printf '  StartedAt     before=%s  after=%s\n' "$BEFORE_STARTED" "$AFTER_STARTED"
printf '  RestartCount  before=%s  after=%s\n' "$BEFORE_RESTARTS" "$AFTER_RESTARTS"
printf '\n'

[[ $REQUEST_STATUS -eq 0 ]] && verdict ok "the in-flight request returned cleanly" "curl exit 0" \
  || verdict bad "the in-flight request returned cleanly" "curl exit $REQUEST_STATUS: $(tail -2 "$WORKDIR/stream.err" 2>/dev/null | tr '\n' ' ')"
[[ "$body_bytes" -gt 0 ]] && verdict ok "the response carried a body" "$body_bytes bytes" \
  || verdict bad "the response carried a body" "empty"
[[ "$STREAM_VALIDATION" == OK* ]] && verdict ok "the stream content is complete" "$STREAM_VALIDATION" \
  || verdict bad "the stream content is complete" "$STREAM_VALIDATION"
[[ "$FIXTURE_EVIDENCE" == OK* ]] && verdict ok "the stream ran through the plugin executor" "$FIXTURE_EVIDENCE" \
  || verdict bad "the stream ran through the plugin executor" "$FIXTURE_EVIDENCE"
[[ "$tokens_seen" -eq "$STREAM_SECONDS" ]] && verdict ok "every token arrived" "$tokens_seen/$STREAM_SECONDS" \
  || verdict bad "every token arrived" "$tokens_seen/$STREAM_SECONDS"
[[ "$TOKENS_AFTER_RELOAD" -gt 0 ]] && verdict ok "tokens arrived after the reload" "$TOKENS_AFTER_RELOAD after it" \
  || verdict bad "tokens arrived after the reload" "none: the pump stopped at the swap"

[[ $REQUEST_START_EPOCH -lt $SWAP_CONFIRMED_EPOCH && $REQUEST_END_EPOCH -gt $SWAP_CONFIRMED_EPOCH ]] \
  && verdict ok "the request spanned the reload" \
       "started ${REQUEST_STARTED_UTC}, reload at +$((SWAP_CONFIRMED_EPOCH - REQUEST_START_EPOCH))s, ended ${REQUEST_ENDED_UTC}" \
  || verdict bad "the request spanned the reload" "it does not straddle the reload"

if [[ -n "$hot_line" ]]; then
  verdict ok "CPA logged the same-version hot reload" "$hot_line"
else
  verdict bad "CPA logged the same-version hot reload" "no line with active_path=/plugins/b/"
fi

[[ "$quiesce_lines" -ge 1 ]] \
  && verdict ok "the replacement raced a live executor obligation" "$quiesce_lines quiesce failure line(s)" \
  || verdict bad "the replacement raced a live executor obligation" "no 'plugin quiesce failed' line: the swap may not have raced the pump"

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
[[ "$final_version" == "$SOURCE_VERSION" ]] \
  && verdict ok "the same plugin version is live" "v$SOURCE_VERSION" \
  || verdict bad "the same plugin version is live" "reports '$final_version'"
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
  printf '  fixture log (last 20 lines):\n';    docker logs "$OWNED_FIXTURE_ID" 2>&1 | tail -20 | sed 's/^/    /' || true
  printf '  plugin directories:\n';             ls -la "$WORKDIR/plugins/a" "$WORKDIR/plugins/b" 2>/dev/null | sed 's/^/    /' || true
  printf '  temp dir kept at %s\n' "$WORKDIR"
  KEEP=1
  fail "$failures assertion(s) failed"
fi

if [[ $DEBUG_ONLY -eq 1 ]]; then
  printf 'DEBUG RUN OK  plumbing works end to end (%ss request), but this is NOT an acceptance verdict:\n' "$REQUEST_SECONDS"
  printf '              rerun without --debug and with SAME_ARTIFACT_STREAM_SECONDS >= %s to produce PASS\n' "$MIN_ACCEPTANCE_SECONDS"
  exit 0
fi

printf 'PASS  same-artifact hot swap verified: a %ss request (measured) survived the replacement, %s tokens arrived after the reload, the CPA process kept PID %s\n' \
  "$REQUEST_SECONDS" "$TOKENS_AFTER_RELOAD" "$BEFORE_PID"
