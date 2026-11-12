#!/usr/bin/env bash
# Isolated CPA environment for verifying the three quota providers against the
# real upstreams (ollama.com, api.commandcode.ai, opencode.ai).
#
#   scripts/real-env/cpa-real-env.sh up       # start, seed real keys, wait until usable
#   scripts/real-env/cpa-real-env.sh verify   # fetch quota once + credential leak check
#   scripts/real-env/cpa-real-env.sh status
#   scripts/real-env/cpa-real-env.sh logs [-f]
#   scripts/real-env/cpa-real-env.sh down     # remove this container and its state dir
#
# Isolation: nothing here shares a port, container name or directory with
# scripts/dev-env (cpa-dev-env-t7, port 18600, stub upstream).
#
# Secrets: the only place real keys live is <repo>/.env.real-keys (gitignored).
# This script never echoes a value; `verify` greps the container log for every
# value and fails if one shows up.
set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT="$(pwd)"

PREFIX="cpa-real-env-t7"
CONTAINER="$PREFIX"
PORT="${CPARE_PORT:-18700}"
CPA_IMAGE="${CPARE_CPA_IMAGE:-eceasy/cli-proxy-api:v8.0.47}"
PLUGIN_SRC="${CPARE_PLUGIN:-$REPO_ROOT/dist/ollama-quota-v0.3.0.so}"
PANEL_VERSION="${CPARE_PANEL_VERSION:-v1.27.3}"
STATE_DIR="${CPARE_STATE_DIR:-$REPO_ROOT/scripts/real-env/.runtime}"
KEYS_FILE="${CPARE_KEYS_FILE:-$REPO_ROOT/.env.real-keys}"
SENTINEL=".cpa-real-env-owned"

# Only these variables are read from .env.real-keys.
KEY_VARS=(CMC_KEY_1 CMC_KEY_2 CMC_KEY_3 OPC_KEY_1 OPC_KEY_2 OLLAMA_API_KEY OLLAMA_SESSION_COOKIE)

log()  { printf '\n=== %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

if command -v python >/dev/null 2>&1; then PYTHON="python"
elif command -v python3 >/dev/null 2>&1; then PYTHON="python3"
else fail "python3 (standard library only) is required"; fi

host_path() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }
port_in_use() { "$PYTHON" scripts/port_in_use.py "$1"; }

wait_for() { # wait_for <seconds> <command...>
  local limit="$1"; shift
  local deadline=$((SECONDS + limit))
  while (( SECONDS < deadline )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

container_id() { docker container inspect --format '{{.Id}}' "$CONTAINER" 2>/dev/null || true; }

ensure_state_dir() {
  if [[ ! -d "$STATE_DIR" ]]; then
    mkdir -p "$STATE_DIR"
    ( cd "$STATE_DIR" && pwd -P ) > "$STATE_DIR/$SENTINEL"
    note "created $STATE_DIR"
    return
  fi
  [[ -f "$STATE_DIR/$SENTINEL" ]] \
    || fail "$STATE_DIR exists but was not created by this script (no $SENTINEL); refusing to use it"
}

delete_state_dir() {
  [[ -d "$STATE_DIR" ]] || return 0
  [[ -f "$STATE_DIR/$SENTINEL" ]] || fail "$STATE_DIR has no $SENTINEL; refusing to delete it"
  local recorded actual
  recorded="$(tr -d '\r\n' < "$STATE_DIR/$SENTINEL")"
  actual="$(cd "$STATE_DIR" && pwd -P)"
  [[ -n "$recorded" && "$recorded" == "$actual" ]] \
    || fail "state dir $actual does not match its ownership record ('$recorded'); refusing to delete it"
  rm -rf -- "$actual"
  note "removed $actual"
}

# load_keys reads .env.real-keys into an associative array. Nothing is echoed.
# CR/LF and surrounding whitespace are trimmed so a CRLF save cannot smuggle a
# \r into a header, and surrounding quotes are dropped because pasted values
# often carry them.
declare -A KEY=()
load_keys() {
  [[ -f "$KEYS_FILE" ]] || fail "$KEYS_FILE does not exist; create it from the template first"
  local line name value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*# ]] && continue
    [[ "$line" == *=* ]] || continue
    name="${line%%=*}"
    value="${line#*=}"
    name="$(printf '%s' "$name" | tr -d '[:space:]')"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then value="${value:1:${#value}-2}"; fi
    KEY["$name"]="$value"
  done < "$KEYS_FILE"
}

report_missing() {
  local missing=() name
  for name in "${KEY_VARS[@]}"; do
    [[ -n "${KEY[$name]:-}" ]] || missing+=("$name")
  done
  if (( ${#missing[@]} > 0 )); then
    note "空着的项（这些上游不会被验证）：${missing[*]}"
  else
    note "七项都已填"
  fi
}

write_config() {
  local dir="$1"
  # The management key is local-only and may be printed; it is what opens the
  # panel. Upstream keys are never printed.
  cat > "$dir/config.yaml" <<YAML
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
  configs:
    "ollama-quota":
      enabled: true
      interval_seconds: 900
YAML
  note "wrote $dir/config.yaml"
}

# json_string renders a value as a JSON string literal; the value travels on
# stdin so it never lands in a process listing either.
json_string() {
  "$PYTHON" -c 'import json,sys; sys.stdout.write(json.dumps(sys.stdin.read()))' <<<"$1"
}

write_auths() {
  local dir="$1/auths"
  mkdir -p "$dir"
  local n value
  local -a written=()
  for n in 1 2 3; do
    value="${KEY[CMC_KEY_$n]:-}"
    [[ -n "$value" ]] || continue
    {
      printf '{\n  "type": "openai-compatible-command code",\n  "api_key": %s,\n' "$(json_string "$value")"
      printf '  "base_url": "https://api.commandcode.ai/provider/v1"\n}\n'
    } > "$dir/cmc-$n.json"
    written+=("cmc-$n")
  done
  for n in 1 2; do
    value="${KEY[OPC_KEY_$n]:-}"
    [[ -n "$value" ]] || continue
    {
      printf '{\n  "type": "openai-compatible-opencode",\n  "api_key": %s,\n' "$(json_string "$value")"
      printf '  "base_url": "https://opencode.ai/zen/go"\n}\n'
    } > "$dir/opc-$n.json"
    written+=("opc-$n")
  done
  local ollama_key="${KEY[OLLAMA_API_KEY]:-}" ollama_cookie="${KEY[OLLAMA_SESSION_COOKIE]:-}"
  if [[ -n "$ollama_key" || -n "$ollama_cookie" ]]; then
    {
      printf '{\n  "type": "ollama"'
      [[ -n "$ollama_key" ]] && printf ',\n  "api_key": %s' "$(json_string "$ollama_key")"
      [[ -n "$ollama_cookie" ]] && printf ',\n  "cookie": %s' "$(json_string "$ollama_cookie")"
      printf '\n}\n'
    } > "$dir/ollama.json"
    written+=("ollama")
  fi
  (( ${#written[@]} > 0 )) || fail "no credential could be written: $KEYS_FILE has no values yet"
  note "wrote ${#written[@]} credential file(s): ${written[*]}"
}

fetch_panel() {
  local target="$1/static/management.html"
  mkdir -p "$1/static"
  [[ -s "$target" ]] && { note "management.html already present"; return 0; }
  local vendored
  for vendored in "$REPO_ROOT/scripts/dev-env/vendor/management.html" \
                  "$REPO_ROOT/../11-10-t7-s4-claude/scripts/dev-env/vendor/management.html"; do
    if [[ -s "$vendored" ]]; then
      cp "$vendored" "$target"
      note "management.html copied from $vendored"
      return 0
    fi
  done
  local url="https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/download/$PANEL_VERSION/management.html"
  log "downloading management.html ($PANEL_VERSION)"
  curl -fsSL -m 180 -o "$target" "$url" \
    || fail "could not download management.html; the panel would 404 without it"
}

stage_plugin() {
  local dir="$1/plugins"
  mkdir -p "$dir"
  rm -f "$dir"/*.so "$dir"/*.dylib "$dir"/*.dll
  [[ -f "$PLUGIN_SRC" ]] || fail "plugin $PLUGIN_SRC is missing; build it (scripts/build.sh) first"
  [[ "$(head -c 4 "$PLUGIN_SRC" | od -An -tx1 | tr -d ' \n')" == "7f454c46" ]] \
    || fail "$PLUGIN_SRC is not a Linux ELF library"
  cp "$PLUGIN_SRC" "$dir/"
  note "installed $(basename "$PLUGIN_SRC")"
}

mgmt() { curl -fsS -m 20 -H "Authorization: Bearer $MGMT_KEY" "$@"; }
cpa_ready() { mgmt "http://127.0.0.1:$PORT/v0/management/plugins" >/dev/null; }

plugin_registered() {
  mgmt "http://127.0.0.1:$PORT/v0/management/plugins" | "$PYTHON" -c '
import json, sys
for entry in json.load(sys.stdin).get("plugins", []):
    if entry.get("id") == "ollama-quota":
        raise SystemExit(0 if entry.get("registered") and entry.get("effective_enabled") else 1)
raise SystemExit(1)
'
}

quota_providers_present() {
  mgmt "http://127.0.0.1:$PORT/v0/management/quota/providers" | "$PYTHON" -c '
import json, sys
providers = json.load(sys.stdin).get("providers") or []
want = {"ollama", "openai-compatible-command code", "openai-compatible-opencode"}
have = set()
for entry in providers:
    have.add(entry.get("provider"))
    have.update(entry.get("supported_providers") or [])
raise SystemExit(0 if want <= have else 1)
'
}

cmd_up() {
  command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
  command -v curl   >/dev/null 2>&1 || fail "curl is not on PATH"

  log "preflight"
  if port_in_use "$PORT"; then
    fail "port $PORT already has a listener; nothing was started"
  fi
  note "port $PORT is free"
  if [[ -n "$(container_id)" ]]; then
    fail "container $CONTAINER already exists; run '$0 down' first. Nothing was started."
  fi
  note "container name $CONTAINER is unused"
  docker image inspect "$CPA_IMAGE" >/dev/null 2>&1 || fail "image $CPA_IMAGE is missing; pull it first"
  note "image $CPA_IMAGE present"

  load_keys
  report_missing

  ensure_state_dir
  MGMT_KEY="$(cat "$STATE_DIR/mgmt-key" 2>/dev/null || true)"
  if [[ -z "$MGMT_KEY" ]]; then
    MGMT_KEY="$("$PYTHON" -c 'import secrets; print(secrets.token_urlsafe(18))')"
    printf '%s' "$MGMT_KEY" > "$STATE_DIR/mgmt-key"
  fi
  CLIENT_KEY="cpa-real-env-local-client"

  write_config "$STATE_DIR"
  write_auths "$STATE_DIR"
  stage_plugin "$STATE_DIR"
  fetch_panel "$STATE_DIR"

  log "starting CPA on 127.0.0.1:$PORT"
  local id
  id="$(MSYS_NO_PATHCONV=1 docker run -d --rm --name "$CONTAINER" \
    -p "127.0.0.1:$PORT:$PORT" \
    -e "MANAGEMENT_STATIC_PATH=/data/static" \
    -v "$(host_path "$STATE_DIR/config.yaml"):/CLIProxyAPI/config.yaml:ro" \
    -v "$(host_path "$STATE_DIR/auths"):/data/auths" \
    -v "$(host_path "$STATE_DIR/plugins"):/data/plugins" \
    -v "$(host_path "$STATE_DIR/static"):/data/static" \
    "$CPA_IMAGE")" || fail "the CPA container did not start"
  [[ -n "$id" ]] || fail "docker run returned no container ID"
  note "container $id"

  if ! wait_for 90 cpa_ready; then
    docker logs "$CONTAINER" 2>&1 | tail -30 >&2 || true
    fail "CPA never answered on 127.0.0.1:$PORT"
  fi
  note "CPA answered"

  if ! wait_for 60 plugin_registered; then
    docker logs "$CONTAINER" 2>&1 | tail -20 >&2 || true
    fail "plugin ollama-quota never registered"
  fi
  note "plugin ollama-quota registered and effective"

  if ! wait_for 30 quota_providers_present; then
    mgmt "http://127.0.0.1:$PORT/v0/management/quota/providers" >&2 || true
    fail "quota providers do not list all three upstreams"
  fi
  note "quota providers: ollama + command code + opencode"

  cat <<MSG

Ready: http://127.0.0.1:$PORT/management.html
  management key: $MGMT_KEY
  Quota page:     sidebar "Quota Management" / "API Key Quota" (/quota)
  state dir:      $STATE_DIR

Next: $0 verify   (fetch one round of quota for every seeded credential)
MSG
}

cmd_verify() {
  [[ -n "$(container_id)" ]] || fail "container $CONTAINER is not running; run '$0 up' first"
  load_keys
  MGMT_KEY="$(cat "$STATE_DIR/mgmt-key")"

  log "credentials CPA sees"
  mgmt "http://127.0.0.1:$PORT/v0/management/auth-files" > "$STATE_DIR/auth-files.json"
  "$PYTHON" - "$STATE_DIR/auth-files.json" "$STATE_DIR/auth-indexes.txt" <<'PY'
import json, sys

source, target = sys.argv[1], sys.argv[2]
data = json.load(open(source, encoding="utf-8"))
files = data.get("files") if isinstance(data, dict) else data
if isinstance(files, dict):
    files = files.get("items") or files.get("files") or []
rows = []
for entry in files or []:
    index = entry.get("auth_index") or entry.get("authIndex") or entry.get("index") or ""
    provider = entry.get("provider") or entry.get("type") or ""
    name = entry.get("name") or entry.get("id") or ""
    if not index:
        continue
    rows.append((index, provider))
    print("    %-16s %-34s %s" % (index, provider, name))
with open(target, "w", encoding="utf-8") as handle:
    for index, provider in rows:
        handle.write("%s\t%s\n" % (index, provider))
PY

  log "fetching quota for every credential"
  "$PYTHON" - "$PORT" "$MGMT_KEY" "$STATE_DIR/auth-indexes.txt" <<'PY'
import json, sys, urllib.request

port, key, index_file = sys.argv[1], sys.argv[2], sys.argv[3]
rows = []
with open(index_file, encoding="utf-8") as handle:
    for line in handle:
        parts = line.rstrip("\n").split("\t")
        if len(parts) == 2 and parts[0]:
            rows.append((parts[0], parts[1]))

for auth_index, provider in rows:
    request = urllib.request.Request(
        "http://127.0.0.1:%s/v0/management/quota/fetch" % port,
        data=json.dumps({"auth_index": auth_index}).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=90) as response:
            body = json.load(response)
    except Exception as exc:
        print("    %-16s %-34s FETCH FAILED: %s" % (auth_index, provider, exc))
        continue
    print("    %-16s %-34s %s" % (auth_index, provider, json.dumps(body, ensure_ascii=False)))
PY

  log "credential leak check (container log)"
  local name value hits=0
  local logfile="$STATE_DIR/container.log"
  docker logs "$CONTAINER" > "$logfile" 2>&1 || true
  for name in "${KEY_VARS[@]}"; do
    value="${KEY[$name]:-}"
    [[ -n "$value" ]] || continue
    if grep -Fq -- "$value" "$logfile"; then
      note "$name APPEARS IN THE LOG"
      hits=$((hits + 1))
    fi
  done
  if (( hits == 0 )); then
    note "0 hits: none of the filled keys appear in the container log"
  else
    fail "$hits credential value(s) found in the container log"
  fi
}

cmd_status() {
  local id
  id="$(container_id)"
  if [[ -z "$id" ]]; then echo "container: absent"; return 0; fi
  echo "container: $id running=$(docker container inspect --format '{{.State.Running}}' "$id")"
  echo "state dir: $STATE_DIR"
  echo "port:      $PORT"
  if curl -fsS -m 5 -o /dev/null "http://127.0.0.1:$PORT/management.html" 2>/dev/null; then
    echo "panel:     ready (http://127.0.0.1:$PORT/management.html)"
  else
    echo "panel:     not answering yet"
  fi
}

cmd_down() {
  log "stopping the environment"
  local id
  id="$(container_id)"
  if [[ -n "$id" ]]; then
    docker rm -f "$id" >/dev/null && note "removed container $id"
  else
    note "no container named $CONTAINER"
  fi
  delete_state_dir
}

usage() { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; }

case "${1:-}" in
  up) shift; cmd_up "$@" ;;
  verify) shift; cmd_verify "$@" ;;
  status) shift; cmd_status "$@" ;;
  logs) shift; docker logs "${1:-}" "$(container_id)" ;;
  down) shift; cmd_down "$@" ;;
  -h|--help|help) usage ;;
  *) usage; exit 2 ;;
esac
