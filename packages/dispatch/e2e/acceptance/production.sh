#!/usr/bin/env bash
# The driver of this issue's one `after_merge` check, `c-production`. It reads what the Dispatch at
# DISPATCH_URL is running, judges it against the commit this tree merged as, and prints a record:
# its whole output is what the role working the check files as that check's result. That role is
# the one `projects.<KEY>.production_check` names — `off` or `tester`, and `tester` by default —
# and the daemon refuses a report from any other with CHECK_ROLE_MISMATCH, so it is not the
# implementer's unless the check was opened before that setting existed.
#
# It does not make the assertions itself. legion/checks/dispatch-production-check.sh, in
# theorem-labs/agent-infra, makes them and prints one `PASS <text>` or `FAIL <text>` line for each:
# /healthz answers 200 and names the commit the running build was built from; GitHub reports that
# commit identical to or ahead of the merged one; the issue page is served and is the application
# shell; the entry script that shell names is served; and each named route exists in the running
# build. This driver fetches that script, runs it, prints everything it printed, and reads its own
# verdict out of those per-assertion lines rather than out of its exit status alone.
#
# Inputs: DISPATCH_URL, the address of the running Dispatch, and MERGE_SHA, the commit this tree
# merged as. NEXT_LOOK_SECONDS is optional, and only the waiting verdict below reads it. There is
# no bearer input: the bearer is this pod's own, which lib.sh's `read_bearer` takes from
# DISPATCH_TOKEN or DISPATCH_TOKEN_FILE and never prints. The driver writes it to a file of this
# run's own under TMPDIR, readable by its owner alone, names that file to the check script as
# DISPATCH_TOKEN_FILE, which the script requires for a route read, and removes it in the cleanup
# trap. There is no PROJECT: the check script reads no project. The driver sets no GH_TOKEN and no
# GITHUB_TOKEN, because a Legion role's `gh` refuses every command run under either of them; `gh`
# is authenticated from GH_CONFIG_DIR instead.
#
# Every call it makes is a read: of GitHub, with the role's own credential, and of Dispatch. It
# writes nothing to either.
#
# Its exit statuses are the three verdicts a production check takes:
#
#   0, passed    the check script's last line reads `all N assertions passed`.
#   1, failed    the script fetch failed, or a route assertion failed, or the script refused its
#                arguments or its environment, or any assertion other than the commit comparison
#                failed. A failed route prints the route and the status the script read from it.
#   3, waiting   the commit comparison is the only failed assertion. The driver prints the commit
#                the running build carries beside MERGE_SHA, says that the running build does not
#                carry this tree's change, and prints the time of its next look.
#
# A missing or malformed input of the driver's own exits 2, as the drivers in this directory do.
#
# The commit comparison has a status of its own because nothing deploys Dispatch on a merge today:
# waiting is the state production is in at every merge, so it is the answer this check gives most
# often. Were it one status with a failure, a route this change needs and the running build does
# not serve would read exactly like a build that has not rolled yet, and the record would not tell
# a reader which it was. The route assertions therefore gate and the commit comparison does not.
# Rebuilding the deploy pipeline, so that a merge reaches production with nobody deploying by hand,
# is dispatch://INFRA-64's work; this check records that gap rather than closing it.
#
# The waiting verdict states the time of the next look itself, because the daemon refuses a
# `pending` on any look but the last unless it carries one, and that refusal leaves the look open.
# The time is 30 minutes ahead, or NEXT_LOOK_SECONDS ahead where the caller gives it, and never
# nearer than 60 seconds: the daemon raises anything nearer than that to 60 seconds ahead, so a
# nearer time buys nothing and would only put a time in the record that the daemon does not keep.
#
# The check script is fetched into a file and run from that file, never `gh api … | bash`. A pipe
# makes the fetch the left-hand command of a pipeline, whose failure the shell's status never
# carries: a fetch that hands `bash` nothing leaves `bash` reading an empty program and exiting 0,
# and the check passes having asserted nothing at all. This driver refuses a fetch that failed and
# a file that came back empty, which is the hole its second negative control opens.
#
# Each of this driver's own assertions prints one `PASS: <section> <what>` or
# `FAIL: <section> <what>: <observed>` line. Every check is `cmd || fail …`, never part of an `&&`
# list: under `set -e` a failing command anywhere but last in such a list does not stop the script.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=<the running Dispatch, which a Legion pod already exports> \
         MERGE_SHA=<merge commit> [NEXT_LOOK_SECONDS=1800] production.sh
       (the bearer is this pod's own, read from DISPATCH_TOKEN or DISPATCH_TOKEN_FILE and never
       printed; there is no PROJECT, since the check script reads no project;
       NEXT_LOOK_SECONDS, read only by the waiting verdict, is how far ahead the next look is,
       30 minutes by default and never nearer than 60 seconds)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL MERGE_SHA
require_commands bash curl date gh grep jq
read_bearer
# The check script reads DISPATCH_URL from its environment. read_bearer has unset both bearer
# variables, so the only bearer that reaches the script is the file this driver names below.
export DISPATCH_URL

# shellcheck disable=SC2153 # MERGE_SHA is the caller's input, checked by name above
readonly merge_sha="$MERGE_SHA"
# The issue this check judges, its page, and the two reads the record is about: the decisions read
# behind the decisions view (b-r-decisions-view) and the inbox read (b-r-inbox). Both paths are
# the ones packages/envoy/internal/dispatch/api/testdata/routes.txt lists.
readonly issue="INFRA-1098"
readonly page="/issues/$issue"
readonly routes=("/api/v1/issues/$issue/decisions" "/api/v1/inbox")

next_look_seconds="${NEXT_LOOK_SECONDS:-1800}"
[[ "$next_look_seconds" =~ ^[0-9]+$ ]] || {
  printf 'NEXT_LOOK_SECONDS is %s, not a number of seconds\n' "$next_look_seconds" >&2
  usage
  exit 2
}
((next_look_seconds >= 60)) || next_look_seconds=60
readonly next_look_seconds

umask 077
work="$(mktemp -d)"
readonly work
# The bearer is written here and nowhere else: a file of this run's own, readable by its owner
# alone, named to the check script and removed by the cleanup trap below.
readonly token_file="$work/dispatch-token"
readonly check_script="$work/dispatch-production-check.sh"

# The EXIT trap below invokes this; shellcheck misses that in a script whose every path exits.
# shellcheck disable=SC2329
cleanup() {
  local code=$?
  rm -f "$token_file"
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s\n' "$token" >"$token_file"
chmod 600 "$token_file"
token=""

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() { printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2; }
# refuse SECTION WHAT OBSERVED: an assertion whose failure leaves nothing after it to judge. It is
# the failed verdict, so it exits 1.
refuse() {
  fail "$@"
  exit 1
}

printf 'production.sh at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'dispatch: %s\n' "$DISPATCH_URL"
printf 'merge commit (MERGE_SHA): %s\n' "$merge_sha"
printf 'page: %s\n' "$page"
printf 'routes: %s\n\n' "${routes[*]}"

# The check script, into a file. `gh api … | bash` would hide a failed fetch, which is what the
# `-s` test below and the second negative control are about.
readonly check_source='repos/theorem-labs/agent-infra/contents/legion/checks/dispatch-production-check.sh?ref=main'
gh api "$check_source" -H 'Accept: application/vnd.github.raw' >"$check_script" 2>"$work/gh.err" ||
  refuse fetch "the check script came from gh api $check_source" \
    "gh api exited $?: $(head -c 600 "$work/gh.err")"
[[ -s "$check_script" ]] ||
  refuse fetch "the check script came from gh api $check_source" \
    "gh api exited 0 and wrote an empty file, so there is no check to run"
pass fetch "the check script came from gh api $check_source ($(wc -c <"$check_script") bytes)"

script_args=(--commit "$merge_sha" --page "$page")
for route in "${routes[@]}"; do
  script_args+=(--route "$route")
done

script_status=0
DISPATCH_TOKEN_FILE="$token_file" bash "$check_script" "${script_args[@]}" >"$work/out" 2>&1 ||
  script_status=$?
# The script's whole output is the record, so all of it is printed before anything is judged.
cat "$work/out"
printf '\n'

last_line="$(tail -n 1 "$work/out")"
case "$script_status" in
0)
  [[ "$last_line" =~ ^all\ [0-9]+\ assertions\ passed$ ]] ||
    refuse run "the check script ends in its own summary line" \
      "it exited 0 and its last line reads \"$last_line\""
  ;;
1) ;;
2)
  refuse run "the check script took its arguments and its environment" \
    "it exited 2, a usage or environment error: $last_line"
  ;;
*)
  refuse run "the check script ran to a verdict" "it exited $script_status: $last_line"
  ;;
esac
pass run "the check script ran against $DISPATCH_URL and ended: $last_line"

# The verdict is read from the script's own per-assertion lines. Its exit status says only that
# something failed, and which assertion failed is the whole difference between failed and waiting.
mapfile -t failed_lines < <(grep '^FAIL ' "$work/out" || true)
if ((script_status != 0 && ${#failed_lines[@]} == 0)); then
  refuse run "the check script names every assertion it failed" \
    "it exited $script_status and printed no FAIL line: $last_line"
fi

# The commit the running build carries, as the script's own /healthz assertion names it.
running_commit=""
health_line="$(grep -m 1 '^PASS the running build answers /healthz' "$work/out" || true)"
if [[ "$health_line" =~ names\ commit\ ([0-9a-fA-F]{7,40}) ]]; then
  running_commit="${BASH_REMATCH[1]}"
fi

# is_commit_failure LINE: whether a FAIL line is the commit comparison's. Every failure of that
# assertion either quotes the compare path or says the compare was never asked for.
is_commit_failure() {
  [[ "$1" == *"repos/cybermonklabs/legion/compare/"* || "$1" == *"the compare was not asked for"* ]]
}

# route_failure ROUTE: prints the first FAIL line naming ROUTE, and refuses when none does.
route_failure() {
  local line
  for line in "${failed_lines[@]}"; do
    [[ "$line" == *"$1"* ]] || continue
    printf '%s' "$line"
    return 0
  done
  return 1
}

# route_status LINE: the status the check script read from a route, as its own FAIL line states
# it, or `no answer` for a route that answered nothing at all.
route_status() {
  if [[ "$1" =~ answered\ ([0-9]{3}) ]]; then
    printf 'HTTP %s' "${BASH_REMATCH[1]}"
  elif [[ "$1" =~ \(([0-9]{3})\) ]]; then
    printf 'HTTP %s' "${BASH_REMATCH[1]}"
  elif [[ "$1" =~ its\ ([0-9]{3})\  ]]; then
    printf 'HTTP %s' "${BASH_REMATCH[1]}"
  else
    printf 'no answer'
  fi
}

commit_failed=0
other_failed=0
for line in "${failed_lines[@]}"; do
  if is_commit_failure "$line"; then
    commit_failed=$((commit_failed + 1))
    continue
  fi
  other_failed=$((other_failed + 1))
done

for route in "${routes[@]}"; do
  if route_line="$(route_failure "$route")"; then
    fail route "$route exists in the running build" \
      "the running build answered $(route_status "$route_line") for $route: $route_line"
  else
    pass route "$route exists in the running build"
  fi
done

if ((commit_failed == 0)); then
  pass commit "the running build carries $merge_sha (it runs ${running_commit:-the commit /healthz named})"
else
  fail commit "the running build carries $merge_sha" \
    "it runs ${running_commit:-a commit /healthz named none of}"
fi

# The three verdicts a production check takes, and the three statuses this driver exits with.
printf '\n'
printf 'running build: %s\n' "${running_commit:-unknown: /healthz named no commit}"
printf 'merge commit (MERGE_SHA): %s\n' "$merge_sha"
if ((other_failed > 0)); then
  printf 'verdict: failed (exit 1), %d assertions of the check script failed\n' \
    "$((other_failed + commit_failed))"
  exit 1
elif ((commit_failed > 0)); then
  # The commit comparison alone failed: every route this change needs is served, and the build
  # serving them was built from some other commit.
  printf "the running build does not carry this tree's change.\n"
  printf 'Nothing deploys Dispatch on a merge today; dispatch://INFRA-64 closes that gap.\n'
  next_look="$(date -u -d "+$next_look_seconds seconds" +%Y-%m-%dT%H:%M:%SZ)" ||
    refuse waiting "state the time of the next look" \
      "date could not add $next_look_seconds seconds to now"
  printf 'next look: %s\n' "$next_look"
  printf 'verdict: waiting (exit 3), the commit comparison is the only failed assertion\n'
  exit 3
else
  printf 'verdict: passed (exit 0), %s\n' "$last_line"
  exit 0
fi
