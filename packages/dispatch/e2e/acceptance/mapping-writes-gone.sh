#!/usr/bin/env bash
# c-mapping-writes-gone (private projects, verification): the Dispatch at ORIGIN serves a build
# that holds the removal of the repository mapping's two writes, and neither write is there.
#
# It passes only when all three hold:
#   - ORIGIN's /healthz `fleet_commit`, the commit every live server process serves, has
#     MERGE_COMMIT as an ancestor (`git merge-base --is-ancestor`, in this checkout's clone of
#     cybermonklabs/legion, which fetches either commit it does not hold from `origin`);
#   - `PUT /api/v1/settings/repo-projects/acme/probe` with an empty body answers 404;
#   - `DELETE /api/v1/settings/repo-projects/acme/probe` answers 404.
# Both writes are sent with the operator's credential, DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (read
# here and never printed). A build that still has the routes answers the PUT 400, since an empty
# body names no project, and the DELETE 400 ACTOR_KIND, since a bearer's write names no actor
# session (measured against main, with the shared token and with a personal token alike), so
# neither write changes anything; the run names each status it was answered.
#
# Usage, from the repository's root:
#
#   DISPATCH_TOKEN=<the operator's bearer> \
#     bash packages/dispatch/e2e/acceptance/mapping-writes-gone.sh ORIGIN MERGE_COMMIT
#
# ORIGIN is the Dispatch's origin (https://dispatch.example), and MERGE_COMMIT the merge commit of
# the pull request that removes the two writes (`gh pr view <its number> -R cybermonklabs/legion
# --json mergeCommit -q .mergeCommit.oid`). Either argument missing or empty exits 2, naming it, so
# an unset variable on the command line never passes. A failed check exits 1, naming every check
# that failed; every check runs whatever the ones before it found.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_TOKEN=<the operator's bearer> mapping-writes-gone.sh ORIGIN MERGE_COMMIT
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[ -n "${1:-}" ] || {
  printf 'the first argument, the Dispatch origin, is missing or empty\n' >&2
  usage
  exit 2
}
[ -n "${2:-}" ] || {
  printf 'the second argument, the merge commit that removes the mapping writes, is missing or empty\n' >&2
  usage
  exit 2
}
[ $# -eq 2 ] || {
  printf 'this script takes two arguments, the origin and the merge commit; it was given %s\n' "$#" >&2
  usage
  exit 2
}
readonly base="${1%/}" merge_commit=$2
require_commands curl git jq
read_bearer

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
readonly root
work_dir
trap 'rm -rf "$work"' EXIT
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

failed=()

# have_commit COMMIT: this clone holds COMMIT, or fetches it from origin.
have_commit() {
  git -C "$root" cat-file -e "$1^{commit}" 2>/dev/null ||
    { git -C "$root" fetch --quiet origin "$1" >"$work/fetch" 2>&1 && git -C "$root" cat-file -e "$1^{commit}" 2>/dev/null; }
}

# The build ORIGIN serves holds the merge commit.
if ! curl -fsS --max-time 20 -o "$work/healthz" "$base/healthz" 2>"$work/curl"; then
  failed+=("GET $base/healthz failed: $(head -c 300 "$work/curl")")
else
  fleet_commit=$(jq -r '.fleet_commit // "null"' "$work/healthz" 2>/dev/null || echo 'not JSON')
  if [ "$fleet_commit" = null ]; then
    failed+=("/healthz names no fleet_commit (null): an unstamped build, or no announcement yet, so what it serves cannot be read")
  elif ! [[ "$fleet_commit" =~ ^[0-9a-f]{40}$ ]]; then
    failed+=("/healthz's fleet_commit is '$fleet_commit', not a commit")
  elif ! have_commit "$merge_commit"; then
    failed+=("this clone cannot read the merge commit $merge_commit, even after a fetch from origin: $(head -c 300 "$work/fetch" 2>/dev/null)")
  elif ! have_commit "$fleet_commit"; then
    failed+=("this clone cannot read the fleet commit $fleet_commit, even after a fetch from origin: $(head -c 300 "$work/fetch" 2>/dev/null)")
  else
    ancestor=0
    git -C "$root" merge-base --is-ancestor "$merge_commit" "$fleet_commit" || ancestor=$?
    case "$ancestor" in
      0) echo "ok fleet_commit $fleet_commit holds $merge_commit" ;;
      1) failed+=("fleet_commit $fleet_commit does not hold the merge commit $merge_commit") ;;
      *) failed+=("git merge-base --is-ancestor $merge_commit $fleet_commit exited $ancestor, so whether the build holds it is UNKNOWN") ;;
    esac
  fi
fi

# write METHOD: the mapping write METHOD, with an empty body and the operator's credential,
# answers 404.
write() {
  local method=$1 status
  status=$(curl -sS --max-time 20 -o "$work/body" -w '%{http_code}' -X "$method" -H @"$work/auth" \
    -H 'Content-Type: application/json' --data-binary '' \
    "$base/api/v1/settings/repo-projects/acme/probe" 2>"$work/curl") || status="curl failed: $(head -c 300 "$work/curl")"
  if [ "$status" = 404 ]; then
    echo "ok $method /api/v1/settings/repo-projects/acme/probe answered 404"
  else
    failed+=("$method /api/v1/settings/repo-projects/acme/probe answered $status, want 404: $(head -c 300 "$work/body" 2>/dev/null)")
  fi
}
write PUT
write DELETE

if [ "${#failed[@]}" -gt 0 ]; then
  printf 'FAIL c-mapping-writes-gone: %s of 3 checks failed:\n' "${#failed[@]}" >&2
  printf '  %s\n' "${failed[@]}" >&2
  exit 1
fi
echo "PASS c-mapping-writes-gone"
