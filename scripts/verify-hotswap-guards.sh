#!/usr/bin/env bash
# Reverse verification for scripts/verify-hotswap.sh, using a docker stand-in.
#
#   scripts/verify-hotswap-guards.sh     # run every scenario; exit 1 on any miss
#
# Why this exists: the guarantees that matter most are about what the script
# does NOT do — it must never remove a container it did not create, must not
# run inside a tree an existing CPA already has mounted, and must not print an
# acceptance PASS for a request that never outlived 30 seconds. None of those
# paths run during an ordinary passing run, so asserting them in prose proves
# nothing. This harness runs the real script against a recording `docker` stub
# and checks the recorded calls.
#
# No real Docker is contacted and no container is started: the stub answers from
# environment variables and every docker invocation is appended to a log file.
#
# Scenarios (each maps to a review finding):
#   R1  a container already has our name        -> exit 1, zero `docker rm`
#   R1  a test port already has a listener      -> exit 1, zero `docker rm`
#   R1  a run that starts containers            -> `docker rm -f <id>`, never by name
#   R5  an existing container mounts our parent -> exit 1, zero `docker rm`
#   R4  a stream shorter than 31s               -> exit 1, never prints PASS
#   R3  the working tree is never stamped       -> plugin.go unchanged after builds
set -uo pipefail

cd "$(dirname "$0")/.."

PASS_COUNT=0
FAIL_COUNT=0
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/hotswap-guards.XXXXXX")"
PRISTINE_PLUGIN_GO="$SCRATCH/plugin.go.pristine"
cp internal/quotaplugin/plugin.go "$PRISTINE_PLUGIN_GO"
SCRIPT="scripts/verify-hotswap.sh"

cleanup() {
  local status=$?
  set +e
  if ! cmp -s internal/quotaplugin/plugin.go "$PRISTINE_PLUGIN_GO"; then
    cp "$PRISTINE_PLUGIN_GO" internal/quotaplugin/plugin.go
    echo "note: restored internal/quotaplugin/plugin.go" >&2
  fi
  [[ "${KEEP_SCRATCH:-0}" == "1" ]] || rm -rf "$SCRATCH"
  exit $status
}
trap cleanup EXIT INT TERM

ok()  { PASS_COUNT=$((PASS_COUNT + 1)); printf 'PASS  %s\n' "$*"; }
bad() { FAIL_COUNT=$((FAIL_COUNT + 1)); printf 'FAIL  %s\n' "$*"; }
rm_calls() { grep -c '^docker rm ' "$1" 2>/dev/null || true; }

# The docker stand-in. It answers the few call shapes the script reaches before
# the first real container would start, and records every invocation verbatim so
# the assertions can check "what did it do" rather than "what did it print".
write_docker_stub() {
  cat > "$SCRATCH/docker.stub" <<'STUB'
docker() {
  printf 'docker %s\n' "$*" >> "$DOCKER_CALL_LOG"
  local sub="${1:-}"
  case "$sub" in
    container) return "${STUB_NAME_EXISTS:-1}" ;;          # 0 = "name is taken"
    image)     return 0 ;;                                  # image already present
    pull)      return 0 ;;
    ps)
      if [[ "${STUB_RUNNING:-0}" == "1" ]]; then
        printf '%s\n' "${STUB_RUNNING_ID:-deadbeefcafe}"
      fi
      return 0 ;;
    inspect)
      # The mount scan passes a --format naming .Mounts; the process probe does not.
      local format="" prev=""
      for arg in "$@"; do
        [[ "$prev" == "--format" ]] && format="$arg"
        prev="$arg"
      done
      if [[ "$format" == *Mounts* ]]; then
        printf '%s|%s|\n' "${STUB_MOUNT_NAME:-/existing-cpa}" "${STUB_MOUNT_SOURCE:-}"
        return 0
      fi
      printf '%s|%s|0\n' "${STUB_PID:-4242}" "${STUB_STARTED:-2026-01-01T00:00:00Z}"
      return 0 ;;
    run)
      # `docker run -d ...` prints an ID. The script must clean up by that ID.
      local index=0
      while [[ -f "$DOCKER_CALL_LOG.run.$index" ]]; do index=$((index + 1)); done
      : > "$DOCKER_CALL_LOG.run.$index"
      if [[ "$index" -eq 0 ]]; then printf '%s\n' "${STUB_UPSTREAM_ID:-aaaa1111}"; else printf '%s\n' "${STUB_CPA_ID:-bbbb2222}"; fi
      return 0 ;;
    logs|rm)   return 0 ;;
    *)         return 0 ;;
  esac
}
STUB
}

# run_case <tag> [VAR=value ...] -- [script args...]
run_case() {
  local tag="$1"; shift
  local -a envs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
  [[ "${1:-}" == "--" ]] && shift

  local call_log="$SCRATCH/$tag.calls"
  : > "$call_log"
  # Run the real script from the real repo, with only docker/python shadowed.
  (
    export DOCKER_CALL_LOG="$call_log"
    # shellcheck disable=SC1090
    source "$SCRATCH/docker.stub"
    export -f docker
    # Shadow only the two helpers that must not touch the live machine: the
    # port probe and the mount-overlap scan. Everything else is genuinely run.
    python() {
      case "${1:-}" in
        *port_in_use.py)
          printf 'python %s\n' "$*" >> "$DOCKER_CALL_LOG"
          [[ "${STUB_PORT_BUSY:-0}" == "1" ]] && return 0
          return 1 ;;
        *mount_overlap.py)
          printf 'python %s\n' "$*" >> "$DOCKER_CALL_LOG"
          if [[ "${STUB_MOUNT_OVERLAP:-0}" == "1" ]]; then
            # Consume stdin the way the real helper does, then report a hit.
            cat >/dev/null
            echo "existing-cpa mount source /tmp overlaps $2"
            return 0
          fi
          cat >/dev/null
          return 1 ;;
        *)
          command python "$@" ;;
      esac
    }
    export -f python
    for assignment in "${envs[@]}"; do export "$assignment"; done
    exec bash "$SCRIPT" "$@"
  ) > "$SCRATCH/$tag.out" 2>&1
  echo $? > "$SCRATCH/$tag.rc"
}

write_docker_stub

echo "scripts/verify-hotswap-guards.sh — reverse verification (no real Docker)"
echo

# ------------------------------------------------------- R1: same-name refusal
run_case name "STUB_NAME_EXISTS=0" --
rc="$(cat "$SCRATCH/name.rc")"
[[ "$rc" == "1" ]] && ok "R1 name taken: exits 1" || bad "R1 name taken: exit $rc, want 1 (see $SCRATCH/name.out)"
[[ "$(rm_calls "$SCRATCH/name.calls")" == "0" ]] \
  && ok "R1 name taken: zero docker rm" \
  || bad "R1 name taken: removed something anyway: $(grep '^docker rm ' "$SCRATCH/name.calls" | tr '\n' ' ')"
grep -q 'Refusing to start' "$SCRATCH/name.out" && ok "R1 name taken: says it refused" || bad "R1 name taken: no refusal wording"
grep -q 'Nothing was started and nothing was removed' "$SCRATCH/name.out" \
  && ok "R1 name taken: states zero side effects" || bad "R1 name taken: side-effect statement missing"

# -------------------------------------------------------- R1: port busy refusal
run_case port "STUB_PORT_BUSY=1" --
rc="$(cat "$SCRATCH/port.rc")"
[[ "$rc" == "1" ]] && ok "R1 port busy: exits 1" || bad "R1 port busy: exit $rc, want 1"
[[ "$(rm_calls "$SCRATCH/port.calls")" == "0" ]] \
  && ok "R1 port busy: zero docker rm" \
  || bad "R1 port busy: removed something anyway: $(grep '^docker rm ' "$SCRATCH/port.calls" | tr '\n' ' ')"

# --------------------------------------------------- R5: mount overlap refusal
run_case mount "STUB_RUNNING=1" "STUB_MOUNT_OVERLAP=1" "STUB_MOUNT_SOURCE=/tmp" --
rc="$(cat "$SCRATCH/mount.rc")"
[[ "$rc" == "1" ]] && ok "R5 mount overlap: exits 1" || bad "R5 mount overlap: exit $rc, want 1 (see $SCRATCH/mount.out)"
[[ "$(rm_calls "$SCRATCH/mount.calls")" == "0" ]] \
  && ok "R5 mount overlap: zero docker rm" \
  || bad "R5 mount overlap: removed something anyway: $(grep '^docker rm ' "$SCRATCH/mount.calls" | tr '\n' ' ')"
grep -q 'overlaps an existing container' "$SCRATCH/mount.out" \
  && ok "R5 mount overlap: names the reason" || bad "R5 mount overlap: reason missing"
# The scan is read-only: it may inspect, and must not run/start anything.
if grep -q '^docker run ' "$SCRATCH/mount.calls"; then
  bad "R5 mount overlap: started a container before refusing"
else
  ok "R5 mount overlap: no container started"
fi

# ------------------------------------------------------ R4: short stream refusal
run_case short "HOTSWAP_STREAM_SECONDS=5" --
rc="$(cat "$SCRATCH/short.rc")"
[[ "$rc" == "1" ]] && ok "R4 short stream: exits 1" || bad "R4 short stream: exit $rc, want 1 (see $SCRATCH/short.out)"
# Match a verdict line, not the word wherever it appears: the refusal message
# quotes the flag's absence of PASS, so a bare substring search is meaningless.
if grep -qE '^(PASS  |DEBUG RUN OK|PASS  hot swap verified)' "$SCRATCH/short.out"; then
  bad "R4 short stream: printed a verdict line"
else
  ok "R4 short stream: prints no verdict line"
fi
[[ "$(rm_calls "$SCRATCH/short.calls")" == "0" ]] \
  && ok "R4 short stream: zero docker rm" || bad "R4 short stream: removed something"

# ------------------------------------------- R1: cleanup removes by ID, not name
# Cleanup is the only code that removes containers, so test it directly: run the
# real cleanup function, extracted from the script under test, against the stub.
# This is faster and more precise than driving a whole run to the container stage.
write_cleanup_case() { # write_cleanup_case <file> <cpa-id> <upstream-id>
  {
    echo 'set -uo pipefail'
    echo 'CLEANED=0; KEEP=0; WORKDIR=""; BUILD_SRC_DIR=""'
    echo "OWNED_CPA_ID='$2'; OWNED_UPSTREAM_ID='$3'"
    echo 'CONTAINER="ollama-hotswap-test"; UPSTREAM_CONTAINER="ollama-hotswap-upstream"'
    echo 'log() { printf "=== %s\n" "$*"; }; note() { printf "    %s\n" "$*"; }'
    echo "DOCKER_CALL_LOG='$SCRATCH/cleanup.calls'; export DOCKER_CALL_LOG"
    cat "$SCRATCH/docker.stub"
    echo 'export -f docker'
    sed -n '/^cleanup() {/,/^}/p' "$SCRIPT"
    echo 'cleanup'
  } > "$1"
}

: > "$SCRATCH/cleanup.calls"
write_cleanup_case "$SCRATCH/cleanup-owned.sh" "aaaa1111" "bbbb2222"
bash "$SCRATCH/cleanup-owned.sh" > "$SCRATCH/cleanup-owned.out" 2>&1 || true
if [[ "$(rm_calls "$SCRATCH/cleanup.calls")" != "2" ]]; then
  bad "R1 ownership: cleanup issued $(rm_calls "$SCRATCH/cleanup.calls") rm call(s), want 2 (one per owned ID)"
elif grep -Eq '^docker rm -f (ollama-hotswap-test|ollama-hotswap-upstream)( |$)' "$SCRATCH/cleanup.calls"; then
  bad "R1 ownership: removed a container by NAME: $(grep '^docker rm ' "$SCRATCH/cleanup.calls" | tr '\n' ' ')"
elif ! grep -q '^docker rm -f aaaa1111$' "$SCRATCH/cleanup.calls" || ! grep -q '^docker rm -f bbbb2222$' "$SCRATCH/cleanup.calls"; then
  bad "R1 ownership: did not remove exactly the IDs it started: $(grep '^docker rm ' "$SCRATCH/cleanup.calls" | tr '\n' ' ')"
else
  ok "R1 ownership: cleanup removed exactly the two IDs it started"
fi

# The refusal path owns nothing, so cleanup must not call Docker at all.
: > "$SCRATCH/cleanup.calls"
write_cleanup_case "$SCRATCH/cleanup-none.sh" "" ""
bash "$SCRATCH/cleanup-none.sh" > "$SCRATCH/cleanup-none.out" 2>&1 || true
if [[ "$(rm_calls "$SCRATCH/cleanup.calls")" == "0" ]]; then
  ok "R1 ownership: cleanup with no owned containers issues zero docker rm"
else
  bad "R1 ownership: cleanup removed something with no owned IDs: $(grep '^docker rm ' "$SCRATCH/cleanup.calls" | tr '\n' ' ')"
fi

# ------------------------------------------------------ R3: tree never stamped
# The two real builds are expensive and need Go, so this checks the cheaper
# invariant directly: after the build helpers, plugin.go equals the committed
# file. A --debug run is impossible without Go here, so instead assert the
# script's own post-build check exists and the tree is untouched by this harness.
if grep -q 'the build stamped it in place' "$SCRIPT"; then
  ok "R3 tree stamp guard: script asserts the working tree was not stamped"
else
  bad "R3 tree stamp guard: no post-build assertion found in $SCRIPT"
fi
if grep -q 'stage_build_tree' "$SCRIPT" && ! grep -q '>\s*"\$PLUGIN_GO"' "$SCRIPT"; then
  ok "R3 build isolation: builds run in a staged copy, not in place"
else
  bad "R3 build isolation: script still writes the working tree's plugin.go"
fi
if cmp -s internal/quotaplugin/plugin.go "$PRISTINE_PLUGIN_GO"; then
  ok "R3 tree state: plugin.go is byte-identical to the committed file"
else
  bad "R3 tree state: plugin.go differs from the committed file"
fi

echo
printf 'guards: %d passed, %d failed\n' "$PASS_COUNT" "$FAIL_COUNT"
if [[ "$FAIL_COUNT" -ne 0 ]]; then
  echo "scratch kept at $SCRATCH"
  KEEP_SCRATCH=1
  exit 1
fi
exit 0
