# shellcheck shell=bash
# The run the two live proofs of the tree's cut-over and rollback (tree-cutover.sh, k31, and
# tree-rollback.sh, k44, of dispatch://INFRA-4730) make around their drivers: both boot the release
# before and this release on one database. Sourced by the entry script, never run. The entry script
# sets `set -euo pipefail` and root, the checkout it sources this file from, and calls tree_proof as
# its last command.
#
# tree_release_before finds the release before, in a check of its own, `the release before`, and
# builds nothing, so an entry script that runs something slow before tree_proof (tree-cutover.sh)
# calls it first, and a checkout with no release before is refused before that slow run. It first
# refuses a host without bun or the checkout's node_modules (require_dispatch_cli, which names the
# checkout in LEGION_E2E_REPO_ROOT: the driver builds Dispatch from it and reads
# docs/kubernetes.md's way back from it), and a shallow checkout, CI's, fetches its history from its
# own remote, the public repository (fetch_history), and origin/main too when it has none. The
# release before is a fixed commit, so the proof runs alike on this release's pull request, on main
# once this release is there, and on every later branch:
#   - `git merge-base HEAD origin/main`, read now, while that commit does not carry this release's
#     conversion (packages/daemon/internal/store/migrations/20261122154645_issue_facts.up.sql), so a
#     branch that merged main forward compares itself with that main;
#   - once it does, main as it stood before this release: the parent of the first-parent commit of
#     origin/main that added that file. A merge base that carries the file while origin/main's
#     first-parent history adds it nowhere has no release before, and the run is refused with exit 2.
#
# tree_proof LABEL SLUG TEST EVIDENCE runs tree_release_before unless the caller has, then two
# checks of its own, then lib/live-daemon-proof.sh's run of TEST, this checkout's driver, with
# EVIDENCE as its evidence directory (empty: a fresh one under TMPDIR, or /tmp when it is unset):
#   - `the release before's driver` takes go.work, go.work.sum, packages/daemon and packages/envoy,
#     the modules go.work names, of the release before with `git archive` into
#     legion-e2e-SLUG-before-<its sha12> under TMPDIR, removed first and on any exit (a fixed name,
#     so Go's build cache serves a later run of the same commit; two runs of one proof on one host
#     at once would share it), lays this checkout's
#     packages/daemon/internal/daemon/tree_release_before_live_test.go over it, as
#     busy-relaunch-delivery.sh's negative control lays its driver, builds that tree's daemon
#     package's `-tags e2e` test binary (build_daemon_driver), and names it in
#     LEGION_E2E_BEFORE_DRIVER: the driver runs it as a child process for each part the release
#     before plays. That driver names only what both releases carry, so it builds there;
#   - `the oldest contract` makes the root of the oldest plugin contract in the fleet
#     (lib/old-plugin-root.sh, with TREE_PROOF_CONTRACT, default 144, the lowest the fleet's daemons
#     served on 2026-11-20), with this checkout's packages/pi-legion/test/daemon-client-driver.ts
#     and gate-holder-driver.ts copied in, and names it in LEGION_E2E_OLD_TOOL_ROOT; that root is
#     kept by commit under OLD_PLUGIN_ROOT_CACHE.
# Each check prints `== <check>`, what it observed and `ok <check> (<s>s)`, the first that fails
# `FAIL <check>: <why>` and ends the run non-zero. Once live_daemon_proof runs, the run's last line
# is its `LABEL e2e: PASS in <n>s (evidence: <dir>)`, or its `LABEL e2e: FAIL at <check> …` line. A
# refusal of the host, or a failure of a check of this file's own, ends the run on its own `FAIL …`
# line, with no verdict line after it, and a signal before live_daemon_proof ends it with none.

# shellcheck source-path=SCRIPTDIR source=live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

# The conversion this release adds: a commit that carries it stores no tree.
tree_conversion=packages/daemon/internal/store/migrations/20261122154645_issue_facts.up.sql

# The tree proof's own state, global because tree_proof reads what tree_release_before found and
# its EXIT trap reads the directory.
tree_base=''
tree_base_sha=''
tree_before_work=''

tree_cleanup() {
  [ -z "$tree_before_work" ] || rm -rf "$tree_before_work"
}

tree_release_before() {
  local added added_sha
  require_dispatch_cli
  fetch_history "the release before and the oldest contract's commit"
  begin 'the release before'
  command -v git >/dev/null || fail "git is not on PATH"
  if ! git -C "$root" rev-parse --verify --quiet refs/remotes/origin/main >/dev/null; then
    git -C "$root" fetch --quiet origin main:refs/remotes/origin/main ||
      fail "the checkout at $root has no origin/main and could not fetch it"
  fi
  tree_base=$(git -C "$root" merge-base HEAD refs/remotes/origin/main) || fail "HEAD and origin/main share no commit"
  tree_base_sha=$(git -C "$root" rev-parse --short=12 "$tree_base")
  if git -C "$root" cat-file -e "$tree_base:$tree_conversion" 2>/dev/null; then
    added=$(git -C "$root" log --first-parent --diff-filter=A --format=%H -1 refs/remotes/origin/main -- "$tree_conversion")
    [ -n "$added" ] ||
      refuse "the merge base $tree_base_sha carries this release's conversion, and no first-parent commit of origin/main adds it, so there is no release before to cut over from"
    added_sha=$(git -C "$root" rev-parse --short=12 "$added")
    tree_base=$(git -C "$root" rev-parse "$added^") || fail "could not read the parent of $added_sha"
    tree_base_sha=$(git -C "$root" rev-parse --short=12 "$tree_base")
    note "the merge base with origin/main carries this release's conversion, which origin/main's $added_sha added: the release before is its parent, $tree_base_sha: $(git -C "$root" log -1 --format=%s "$tree_base")"
  else
    note "the release before is the merge base with origin/main, $tree_base_sha: $(git -C "$root" log -1 --format=%s "$tree_base")"
  fi
  pass
}

tree_proof() {
  local label=$1 slug=$2 test_name=$3 old_root status
  [ -n "$tree_base" ] || tree_release_before
  # The evidence directory is made here, before live_daemon_proof's, so this run's own checks keep
  # their logs in it too.
  evidence=${4:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug-evidence.XXXXXXXX")}
  mkdir -p "$evidence"
  trap tree_cleanup EXIT

  begin "the release before's driver"
  command -v tar >/dev/null || fail "tar is not on PATH"
  tree_before_work=${TMPDIR:-/tmp}/legion-e2e-$slug-before-$tree_base_sha
  rm -rf -- "$tree_before_work"
  mkdir -p -- "$tree_before_work"
  git -C "$root" archive "$tree_base" go.work go.work.sum packages/daemon packages/envoy | tar -x -C "$tree_before_work" ||
    fail "could not take the tree of $tree_base_sha"
  cp "$root/packages/daemon/internal/daemon/tree_release_before_live_test.go" \
    "$tree_before_work/packages/daemon/internal/daemon/tree_release_before_live_test.go" ||
    fail "could not lay tree_release_before_live_test.go over the tree of $tree_base_sha"
  (
    root=$tree_before_work
    build_daemon_driver "$tree_before_work/before.test"
  ) >"$evidence/before-build.log" 2>&1 ||
    fail "the release before's driver did not build in the tree of $tree_base_sha; see $evidence/before-build.log"
  note "built the release before's driver in $tree_before_work from the tree of $tree_base_sha with this checkout's tree_release_before_live_test.go laid over it: sha256 $(sha256sum "$tree_before_work/before.test" | cut -d' ' -f1)"
  bash "$root/scripts/e2e/lib/built-from.sh" "$root" | while read -r line; do note "this release, $line"; done
  pass

  begin 'the oldest contract'
  old_root=$(bash "$root/scripts/e2e/lib/old-plugin-root.sh" "$root" "${TREE_PROOF_CONTRACT:-144}" \
    packages/pi-legion/test/daemon-client-driver.ts packages/pi-legion/test/gate-holder-driver.ts 2>"$evidence/old-plugin-root.log") ||
    fail "the oldest contract's plugin root could not be made; see $evidence/old-plugin-root.log"
  note "daemon API contract ${TREE_PROOF_CONTRACT:-144}'s plugin root: $old_root"
  pass

  export LEGION_E2E_OLD_TOOL_ROOT="$old_root" LEGION_E2E_BEFORE_DRIVER="$tree_before_work/before.test"
  # live_daemon_proof ends the shell it runs in, and its EXIT trap is that subshell's, so this run's
  # own trap still removes the release before's tree.
  set +e
  (
    set -e
    live_daemon_proof "$label" "$slug" "$test_name" "$evidence"
  )
  status=$?
  set -e
  return "$status"
}
