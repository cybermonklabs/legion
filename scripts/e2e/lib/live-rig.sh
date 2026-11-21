# shellcheck shell=bash disable=SC2154 # root is the caller's (see the header)
# The run a live proof on the rig of this change's checks makes around its driver
# (packages/daemon/internal/daemon/RIG.md): what scripts/e2e/lib/live-daemon-proof.sh's run makes,
# but with the listener production pins and the grants production renders. Sourced by the entry
# script, never run; the entry script sets `set -euo pipefail` and root, the checkout, and calls
# live_rig_proof. A negative control calls it in a subshell of its own whose root is a worktree of
# the base commit, as busy-relaunch-delivery.sh's calls live_daemon_proof.
#
# live_rig_proof LABEL SLUG PATTERN EVIDENCE [EXPECT] runs five checks, each printing `== <check>`,
# what it observed and `ok <check> (<s>s)`, the first that fails printing `FAIL <check>: <why>`
# and ending the run non-zero (exit 2 for an input of the caller's, exit 1 for every other):
#
#   prerequisites   LEGION_TEST_PG_DSN, on a loopback host (lib/loopback-dsn.sh), which nothing
#                   here starts; LEGION_OMP_PATH, an executable Oh My Pi; the panes' model, a base
#                   URL (LEGION_E2E_MODEL_BASE_URL, else ANTHROPIC_BASE_URL) and a key, as an Oh My
#                   Pi `!command` that prints it (LEGION_E2E_MODEL_KEY_COMMAND) or a file that holds
#                   it (LEGION_E2E_MODEL_KEY_FILE), which a driver whose rig runs a stand-in model
#                   does not read but which this run asks for all the same, so a run never passes
#                   for a stand-in it did not mean; go, git, bun, awk and python3; and the
#                   nats-server live-daemon-proof.sh installs (LEGION_E2E_NATS_BIN, or its pin).
#   pinned-listener the listener production pins (packages/daemon/internal/testenvoy/pinned.go,
#                   PinnedRevision), built as its image's Dockerfile builds it: a detached worktree
#                   of that commit under the run's work directory, the Go contracts regenerated
#                   from their schemas (`bun run gen:go`), which must leave its tracked files as
#                   they are, and `CGO_ENABLED=0 go build ./cmd/listener` in packages/envoy. Then
#                   that tree's own grants: lib/live-rig-grants.go run in its packages/envoy/infra,
#                   which the evidence keeps as grants.json. With LEGION_E2E_LISTENER_FROM_CHECKOUT=1
#                   (any other value is refused) this check is checkout-listener instead: the same
#                   build and grants from a detached worktree of root's own HEAD, whose
#                   packages/envoy and packages/contracts must match that commit, so the listener is
#                   the checkout's; the driver is told the commit (LEGION_E2E_LISTENER_REVISION) and
#                   takes no other (testenvoy.RigBinary). Either way listener.txt in the evidence
#                   names the source and the commit. cut-turn-note.sh sets it; no other check does.
#   driver          the daemon package's `-tags e2e` test binary, from root.
#   proof           the driver run with PATTERN as -test.run and LIVE_RIG_TEST_TIMEOUT (90m when unset)
#                   as -test.timeout, its environment holding none of the
#                   caller's own Envoy, Dispatch, NATS or Oh My Pi session variables, since this
#                   may run inside an agent's session; with EXPECT `pass` (the default) it holds
#                   that the top-level test passed, and with `fail` only that it ran. The driver's
#                   -test.timeout is LIVE_RIG_TEST_TIMEOUT, 90m when it is unset, which a proof
#                   longer than that (daemon-wait.sh's two 55-minute outages, cut-turn-note.sh's
#                   two rigs) sets.
#
# SLUG names the run's files: the work directory legion-e2e-SLUG.<pid>.* under TMPDIR (or /tmp),
# removed on any exit with the listener's worktree, and the evidence directory EVIDENCE, or a fresh
# legion-e2e-SLUG-evidence.* beside it, kept and printed: the transcript, the driver's output
# (proof.log), the daemon's log, the nats-server's, each pane's, and grants.json.

# shellcheck source-path=SCRIPTDIR source=live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

# The machine the rig's listener says it serves, which the listener's grant is scoped to.
rig_machine=legion-rig

# The run's own state, global because live_rig_cleanup reads it from the EXIT trap.
rig_repo=''
rig_listener_src=''
live_rig_status=0

live_rig_cleanup() {
  local status=$?
  if [ -n "$rig_listener_src" ] && [ -n "$rig_repo" ]; then
    git -C "$rig_repo" worktree remove --force "$rig_listener_src" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "$proof_label e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "$proof_label e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}

live_rig_proof() {
  local slug=$2 pattern=$3 expect=${5:-pass} tool nats_bin db_host revision image test_name listener_source listener_revision
  proof_label=$1
  evidence=${4:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug-evidence.XXXXXXXX")}
  mkdir -p "$evidence/panes"
  transcript_to "$evidence/transcript.log"
  work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug.$$.XXXXXXXX")
  driver=$work/$slug.test
  started=$SECONDS
  check_started=$SECONDS
  trap live_rig_cleanup EXIT
  test_name=${pattern%%/*}
  test_name=${test_name#^}
  test_name=${test_name%\$}

  begin prerequisites
  [ -n "${LEGION_TEST_PG_DSN:-}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in go git bun awk python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  unset PGHOSTADDR PGSERVICE PGSERVICEFILE
  require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops a database there" ||
    refuse "$db_host"
  [ -n "${LEGION_OMP_PATH:-}" ] && [ -x "$LEGION_OMP_PATH" ] ||
    refuse "LEGION_OMP_PATH must name an executable Oh My Pi: every pane of the rig runs it"
  [ -n "${LEGION_E2E_MODEL_BASE_URL:-${ANTHROPIC_BASE_URL:-}}" ] ||
    refuse "name the panes' model: LEGION_E2E_MODEL_BASE_URL (an issue pod's ANTHROPIC_BASE_URL is taken when it is unset)"
  [ -n "${LEGION_E2E_MODEL_KEY_COMMAND:-}${LEGION_E2E_MODEL_KEY_FILE:-}" ] ||
    refuse "name the model's key: LEGION_E2E_MODEL_KEY_COMMAND (a command that prints it, run by Oh My Pi) or LEGION_E2E_MODEL_KEY_FILE (a file that holds it)"
  note "go $(go version | awk '{print $3}'), store LEGION_TEST_PG_DSN (set; not printed), Oh My Pi $("$LEGION_OMP_PATH" --version 2>/dev/null)"
  note "the panes' model: ${LEGION_E2E_MODEL:-claude-sonnet-5-5} at ${LEGION_E2E_MODEL_BASE_URL:-$ANTHROPIC_BASE_URL}, its key from ${LEGION_E2E_MODEL_KEY_COMMAND:+a command}${LEGION_E2E_MODEL_KEY_FILE:+a file}"
  nats_bin=${LEGION_E2E_NATS_BIN:-}
  if [ -z "$nats_bin" ]; then
    note "LEGION_E2E_NATS_BIN names none; installing nats-server $nats_version, live-daemon-proof.sh's pin, into this run's own GOBIN"
    local goroot
    goroot=$(go env GOROOT) || fail "go env GOROOT failed"
    if ! GOBIN="$work/bin" GOROOT=$goroot "$goroot/bin/go" install "github.com/nats-io/nats-server/v2@$nats_version"; then
      fail "could not install nats-server $nats_version; name a binary in LEGION_E2E_NATS_BIN instead"
    fi
    nats_bin=$work/bin/nats-server
  fi
  [ -x "$nats_bin" ] || fail "no executable nats-server at $nats_bin"
  note "nats-server $("$nats_bin" --version)"
  pass

  case "${LEGION_E2E_LISTENER_FROM_CHECKOUT:-}" in
    '')
      begin pinned-listener
      revision=$(awk -F'"' '/^\tPinnedRevision +=/ {print $2}' "$root/packages/daemon/internal/testenvoy/pinned.go")
      image=$(awk -F'"' '/^\tPinnedImage +=/ {print $2}' "$root/packages/daemon/internal/testenvoy/pinned.go")
      [ -n "$revision" ] || fail "packages/daemon/internal/testenvoy/pinned.go names no PinnedRevision"
      git -C "$root" cat-file -e "$revision^{commit}" 2>/dev/null ||
        fail "the checkout's history holds no commit $revision, the one $image was built from; fetch it"
      listener_source="the listener production pins, $image"
      listener_revision=''
      ;;
    1)
      begin checkout-listener
      revision=$(git -C "$root" rev-parse --verify 'HEAD^{commit}') || fail "the checkout $root has no HEAD commit"
      [ -z "$(git -C "$root" status --porcelain --untracked-files=no -- packages/envoy packages/contracts)" ] ||
        fail "the checkout's packages/envoy or packages/contracts differs from its HEAD $revision, so a listener built from that commit is not the checkout's: $(git -C "$root" status --porcelain --untracked-files=no -- packages/envoy packages/contracts | head -5)"
      image="no image: the checkout's own commit, not production's pin"
      listener_source="the listener of the checkout's own commit (LEGION_E2E_LISTENER_FROM_CHECKOUT=1), not production's pin"
      listener_revision=$revision
      ;;
    *)
      begin listener
      refuse "LEGION_E2E_LISTENER_FROM_CHECKOUT is '$LEGION_E2E_LISTENER_FROM_CHECKOUT': it is 1, for the listener of the checkout's own commit, or unset, for the pinned one"
      ;;
  esac
  rig_repo=$root
  rig_listener_src=$work/listener-src
  git -C "$root" worktree add --detach "$rig_listener_src" "$revision" >"$evidence/listener-build.log" 2>&1 ||
    fail "could not make a worktree of $revision; see $evidence/listener-build.log"
  (cd "$rig_listener_src" && bun install --frozen-lockfile --ignore-scripts --filter './' --filter './packages/contracts') \
    >>"$evidence/listener-build.log" 2>&1 || fail "bun install at $revision failed; see $evidence/listener-build.log"
  (cd "$rig_listener_src/packages/contracts" && bun run gen:go) >>"$evidence/listener-build.log" 2>&1 ||
    fail "regenerating the Go contracts at $revision failed; see $evidence/listener-build.log"
  [ -z "$(git -C "$rig_listener_src" status --porcelain --untracked-files=no)" ] ||
    fail "regenerating the Go contracts at $revision changed its tracked files: $(git -C "$rig_listener_src" status --porcelain --untracked-files=no | head -5)"
  (cd "$rig_listener_src/packages/envoy" && CGO_ENABLED=0 go build -o "$work/envoy-listener" ./cmd/listener) \
    >>"$evidence/listener-build.log" 2>&1 || fail "the listener at $revision did not build; see $evidence/listener-build.log"
  note "built $work/envoy-listener: CGO_ENABLED=0 go build ./cmd/listener in packages/envoy of $revision ($image), after bun run gen:go left its tracked files as they were"
  note "its build information: $(go version -m "$work/envoy-listener" | awk '$1 == "path" {printf "path=%s ", $2} $1 == "build" && $2 ~ /^vcs\.(revision|modified)=/ {printf "%s ", $2}')"
  printf 'source: %s\nrevision: %s\nbuild: %s\n' "$listener_source" "$revision" \
    "$(go version -m "$work/envoy-listener" | awk '$1 == "build" && $2 ~ /^vcs\.(revision|modified)=/ {printf "%s ", $2}')" >"$evidence/listener.txt"
  note "the rig's listener: $listener_source, revision $revision (recorded in $evidence/listener.txt)"
  cp "$root/scripts/e2e/lib/live-rig-grants.go" "$rig_listener_src/packages/envoy/infra/live-rig-grants.go" ||
    fail "could not lay live-rig-grants.go into the tree of $revision"
  (cd "$rig_listener_src/packages/envoy/infra" && go run ./live-rig-grants.go "$rig_machine") >"$work/grants.raw.json" 2>>"$evidence/listener-build.log" ||
    fail "the grants of $revision could not be read; see $evidence/listener-build.log"
  python3 - "$work/grants.raw.json" "$revision" >"$evidence/grants.json" <<'EOF' || fail "could not record the grants"
import json, sys
grants = json.load(open(sys.argv[1]))
grants["revision"] = sys.argv[2]
json.dump(grants, sys.stdout, indent=2)
EOF
  note "grants of $revision in $evidence/grants.json: the listener's user core.ListenerHostPermissions($rig_machine), the sessions' core.SessionPermissions, Dispatch's core.DispatchPermissions($rig_machine)"
  pass

  begin driver
  go -C "$root/packages/daemon" test -c -tags e2e -o "$driver" ./internal/daemon || fail "the driver did not build"
  note "built $(basename "$driver") from $root/packages/daemon with -tags e2e"
  pass

  begin proof
  : >"$evidence/daemon.log"
  rm -f "$evidence/proof.log"
  set +e
  env -u ENVOY_CONFIG_FILE -u ENVOY_MACHINE_ID -u ENVOY_URL -u ENVOY_TOKEN -u ENVOY_TOKEN_FILE -u ENVOY_NATS_URL \
    -u DISPATCH_URL -u DISPATCH_TOKEN -u DISPATCH_TOKEN_FILE -u DISPATCH_SESSION_ID -u DISPATCH_HOST -u DISPATCH_STATE_DIR \
    -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u NATS_DAEMON_NKEY_SEED -u NATS_DAEMON_NKEY_SEED_FILE \
    -u OMP_SESSION_ID -u PI_SESSION_FILE -u PI_SESSION_ID -u PI_CONFIG_FILES -u LEGION_E2E_NATS_URL \
    LEGION_E2E_NATS_BIN="$nats_bin" \
    LEGION_E2E_ENVOY_LISTENER="$work/envoy-listener" \
    LEGION_E2E_LISTENER_FROM_CHECKOUT="${LEGION_E2E_LISTENER_FROM_CHECKOUT:-}" \
    LEGION_E2E_LISTENER_REVISION="$listener_revision" \
    LEGION_E2E_RIG_GRANTS="$evidence/grants.json" \
    LEGION_E2E_DAEMON_LOG="$evidence/daemon.log" \
    TMPDIR="$work" \
    "$driver" -test.count=1 -test.timeout="${LIVE_RIG_TEST_TIMEOUT:-90m}" -test.v -test.run "$pattern" 2>&1 | tee "$evidence/proof.log"
  live_rig_status=${PIPESTATUS[0]}
  set -e
  grep -q "^=== RUN   $test_name\$" "$evidence/proof.log" ||
    fail "the driver ran no $test_name: -test.run matched nothing; see $evidence/proof.log"
  if grep -q -- "^--- SKIP: $test_name" "$evidence/proof.log"; then
    fail "the driver skipped $test_name, so it proved nothing; its skip line is in $evidence/proof.log"
  fi
  if [ "$expect" = pass ]; then
    if [ "$live_rig_status" -ne 0 ] || ! grep -q -- "^--- PASS: $test_name" "$evidence/proof.log"; then
      fail "$test_name did not pass; see $evidence/proof.log and $evidence/daemon.log"
    fi
  fi
  note "the driver exited $live_rig_status"
  pass
}
