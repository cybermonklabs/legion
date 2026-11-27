#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
# The tailnet leg of the outside agent's acceptance, and the one command that runs both legs: a
# developer box on the private network runs it, with its project's key as the one argument. It
# reads the deployed pins and Dispatch's serving build, and refuses to start unless the secrets
# broker's build holds the enrollment refusal and Dispatch's build holds the changes the check
# rests on; makes the scratch project's issue the public leg's GitHub row reads; starts a scratch
# agent-secrets-helper whose machine login the test person approves from the public leg's browser;
# starts our peer session; starts the public leg (outside-agent-public.sh, rows P1-P10 and its
# Codex leg's C0-C7) on a runner on the public internet; runs rows T1-T4 against the broker's
# private origin, the one place its enrollment route is served; reads, with a full member's browser
# session, the positive controls rows T3, P7 and sweep are held to; reads the public leg's TSV;
# asks the operator, on this terminal, to remove the test person from the project's access list and
# reads that the test person's live device login stops (T5); reads the pins and Dispatch's serving
# build again; and prints the gate. outside-agent-acceptance.md, beside this file, says what each
# row reads and what the gate holds.
#
# It sources three support files from beside it: outside-agent-lib.sh (what both legs share),
# outside-agent-peer.sh (the peer of P4 and the session outside the project of P6) and
# outside-agent-sweep.sh (the read sweep's full-member half and its controls).
#
# OUTSIDE_GATE chooses the gate: `all` (the default), both legs as above, or `browser`, the browser
# leg alone, which runs while nothing of the agents' path serves (no edge listener, no broker route
# at the edge, no broker project check). The browser gate reads the pins of the broker and Dispatch
# alone; starts no scratch helper, no peer and no session outside the project, so it runs no T1-T4
# and needs none of OUTSIDE_TAILNET_BROKER_URL, OUTSIDE_LISTENER_URL,
# OUTSIDE_INTERNAL_LISTENER_URL, OUTSIDE_FOREIGN_PROJECT and OUTSIDE_CANARY; has the project's
# owner, through the owner commands below and never a person's browser session, put the test
# person on the access list and make their invitation link, and asks the owner's invite for an
# address on no list (row unlisted); starts the workflow with `checks=browser`,
# `routed_controls=dispatch-healthz`, the link as `invite_url` and the issue row's key as
# `sweep_member_issue`; removes the test person through the owner's remove command once
# `gh run view --json jobs` reads the workflow's step `The delisting window` in_progress, and judges
# T5 from the rows T5-cookie (30 s) and T5-passkey (325 s) of that step's artifact
# outside-edge-t5; and, in place of P1-P10, prints the rows of the workflow's own checks from their
# artifacts: refusals and reach (each check's exit, its `.code`), and the driver's sign-in,
# projects and limits (outside-edge.tsv); and the row grants, the driver's probe of other sessions'
# credential grants (outside-edge-grants), held to a full member's list read with
# OUTSIDE_SWEEP_FULL_TOKEN_FILE seconds before the run starts and again after it ends. Its row
# public is the run's conclusion. Its sweep's GitHub proxy reads use OUTSIDE_FULL_COOKIE_FILE, a
# full member's browser session, where it is given, and else the full member's token
# (OUTSIDE_SWEEP_FULL_TOKEN_FILE), which a Dispatch build that reads the proxy as a browser's person
# alone refuses, so the sweep row then reads UNKNOWN naming that refusal; the same session makes
# the owner's acts where their commands are unset (below). It refuses OUTSIDE_PUBLIC_COMMAND, since
# only the workflow's step status opens its window. Its TSV's rows are pins, issue, unlisted, T5,
# refusals, reach, sign-in, projects, limits, grants, public, sweep, pins-end and the gate. Any
# other value of OUTSIDE_GATE is refused: every row UNKNOWN, exit 2.
#
# Inputs (environment), each named by the row that cannot run without it:
#   $1                           the scratch project's key.
#   OUTSIDE_PINS                 the deployed pins, one `name file key` line each for `broker`,
#                                `dispatch` and `edge-listener` (the browser gate reads the first
#                                two alone): the configuration file and the
#                                key whose value is that deployment's image; `#` starts a
#                                comment. With OUTSIDE_PINS_REPO set, each file is a path in that
#                                repository, read at its main with `gh api`, at the start and again
#                                at the end; without it, a local file, and row pins-end reads
#                                UNKNOWN, since a local file cannot show the main moved.
#   OUTSIDE_PINS_REPO            owner/name of the deployment repository the pins are read from.
#   OUTSIDE_ENROLL_REFUSAL_COMMIT  the merge commit of the enrollment refusal (a restricted
#                                member's machine login enrolls nothing), which the broker's
#                                build must hold.
#   OUTSIDE_DISPATCH_COMMITS     the merge commits, blank-separated, that Dispatch's build must
#                                hold: the change that gives no broker token the approve scope,
#                                and the change that refuses a restricted member an issue's
#                                repository.
#   OUTSIDE_TAILNET_BROKER_URL   the broker's origin on the private network.
#   OUTSIDE_TAILNET_DISPATCH_URL  Dispatch's origin on the private network: its /healthz names the
#                                build it serves, so the pins row needs it and no leg starts without
#                                it; the full member's browser session reads T5's access list and
#                                the positive controls there.
#   OUTSIDE_LISTENER_URL         the edge listener, where the peer session registers.
#   OUTSIDE_INTERNAL_LISTENER_URL  a listener on the private network, where a session outside the
#                                project (<peer>-foreign) registers for P6's control.
#   OUTSIDE_FOREIGN_PROJECT      a project of this box's person other than the scratch project,
#                                which <peer>-foreign's token names alone (INFRA in production).
#   OUTSIDE_PERSON               the test person's address.
#   OUTSIDE_CANARY               the name of the canary secret T2 and T3 ask for.
#   OUTSIDE_REPOSITORY           owner/name of the repository the scratch project's issue names.
#   OUTSIDE_SWEEP_PROJECT, OUTSIDE_SWEEP_FULL_TOKEN_FILE, OUTSIDE_SWEEP_CONVERSATION_FILE,
#   OUTSIDE_SWEEP_WRITE_ISSUE    the read sweep's full-member half (restricted-sweep.sh, row
#                                sweep): the project the test person must not see, which the leg
#                                also passes to the workflow as `sweep_project`, a file holding a
#                                full member's personal token of the person scope for Dispatch's
#                                private origin (the sweep's reads, its writer's comment and direct
#                                message, and the agent view, and the browser gate's grants
#                                control: its list of credential grants and its whoami), the
#                                conversation file of a session that worked in that project alone,
#                                and a scratch issue of it the writer comments on every 15 s while
#                                the public leg runs.
#   OUTSIDE_FULL_COOKIE_FILE     a file holding a full member's browser session at Dispatch's
#                                private origin, `<name>=<value>` as the Cookie header carries it.
#                                The positive controls of T3 and P7 read with it, so those rows
#                                need it; the sweep's GitHub proxy reads use it where it is given
#                                (as SWEEP_FULL_COOKIE), and the full member's token where it is
#                                not; and the browser gate's owner acts are made with it where
#                                their commands are unset.
#   OUTSIDE_WORKFLOW_REPO        owner/name of the repository whose workflow runs the public leg;
#                                OUTSIDE_WORKFLOW names the workflow (outside-edge-e2e.yaml), and
#                                OUTSIDE_WORKFLOW_INPUTS holds its other inputs, one name=value a
#                                line: `invite_url`, the owner's single-use enrollment link for the
#                                test person, which the workflow masks before any step prints and
#                                the run spends. The inputs reach `gh workflow run` as JSON on its
#                                stdin, never its command line, which any account on the box can
#                                read. Or OUTSIDE_PUBLIC_COMMAND: a command line, split on blanks,
#                                that runs the public leg here instead, for a local stack; it gets
#                                OUTSIDE_TAILNET_USER_CODE, OUTSIDE_PEER_SESSION,
#                                OUTSIDE_SWEEP_FILLS and OUTSIDE_PUBLIC_RESULTS in its environment
#                                and writes outside-agent-public.tsv and
#                                outside-edge-revocations.tsv there.
# Optional: OUTSIDE_PEER, P4's peer: `omp` (the default), an Oh My Pi session of this box whose
# model answers, run with only the envoy extension (dist/envoy.js) of the plugin
# OUTSIDE_PEER_PLUGIN_DIR names (default this checkout's packages/pi-envoy, built), no built-in
# tool, and a scratch HOME holding only the model's configuration and credential: the files
# OUTSIDE_PEER_AGENT_FILES names (default `config.yml models.yml agent.db`) out of
# OUTSIDE_PEER_AGENT_DIR (default ~/.omp/agent), and the variables OUTSIDE_PEER_ENV names; or
# `probe`, a scripted peer for a local stack, only with OUTSIDE_ALLOW_HTTP=1, named
# OUTSIDE_PEER_SESSION (default outside-e2e-peer-<random>). OUTSIDE_OWNER_REMOVE_COMMAND, a command
# line that removes the test person from the project's access list as its owner, run with the
# person and the project as its last two words, in place of T5's prompt (a harness that acts as the
# owner's browser). OUTSIDE_IMAGE_REVISION_COMMAND, a command line that prints the commit an image
# was built from, run with the image as its last word (default: `crane config`, read for
# org.opencontainers.image.revision); OUTSIDE_TAILNET_TSV, a file that gets a copy of the TSV;
# OUTSIDE_RESULT_ISSUE, an issue the result is posted to as one comment with `dispatch comment`;
# OUTSIDE_LEGION_CHECKOUT, the git checkout the pins row reads commits in (default this file's
# own); OUTSIDE_ALLOW_HTTP=1, which lets http://127.0.0.1 origins through for a local stack.
# The browser gate's owner commands, each a command line split on blanks that acts as the project's
# owner with a credential of its own, which this leg never holds; the leg prints no word of what
# they print but what is named here, since a link is a secret. OUTSIDE_OWNER_ADD_COMMAND puts an
# address on the project's access list and OUTSIDE_OWNER_REMOVE_COMMAND takes it off, each run
# with the address and the project as its last two words, exit 0 when done;
# OUTSIDE_OWNER_INVITE_COMMAND, run likewise, makes the address's invitation link and prints it
# alone (exit 0), or prints Dispatch's refusal as `<status> <code>` on its first line (exit 1), or
# exits 2 when it could not tell; OUTSIDE_OWNER_LIST_COMMAND, run with the project as its last word,
# prints the addresses on the project's access list, one a line (exit 0). An owner command left
# unset is made by the leg itself where OUTSIDE_FULL_COOKIE_FILE holds the owner's own browser
# session (the project's owner, who holds the person and approve scopes no agent token holds
# today): at OUTSIDE_TAILNET_DISPATCH_URL, PUT and DELETE /api/v1/projects/<key>/access/people/
# <address>, GET /api/v1/projects/<key>/access and POST .../people/<address>/invitations, read as
# the command's contract above (owner_by_browser). The browser gate needs each act, by its command
# or by that session, and asks no terminal; with neither, the rows that need it read UNKNOWN naming
# both. Its `invite_url` is the owner invite's link, and an `invite_url` line in
# OUTSIDE_WORKFLOW_INPUTS is left out. OUTSIDE_UNLISTED_PERSON is the address row unlisted asks a
# link for (default outside-e2e-unlisted-<random>@example.com, which no list names).
#
# What each service says of the build it serves: Dispatch's /healthz names `commit`, the legion
# commit its binary was built from, and `fleet_commit`, the commit every live server process
# serves (each null when unknown); the broker's and the edge listener's /healthz name a status
# alone. So Dispatch's serving build is read and held to its pin, and the broker's and the edge
# listener's builds are their pins'.
#
# The issue runs as this box's own `dispatch` command. The scratch helper of T1, T2 and T4, and the
# full member's helper of T3 and P7's control, each run with HOME, XDG_RUNTIME_DIR, XDG_STATE_HOME
# and every AGENT_SECRETS_* path of its own, in one `mktemp -d` directory, with
# OUTSIDE_TAILNET_BROKER_URL as its broker, so neither shares a key, socket or record with the box's
# and both enroll at the one broker.
#
# Output: on stdout a TSV, the header `row want got verdict`, the rows pins, issue, T1-T5, the
# public leg's P1-P10 and its Codex leg's C0-C7 as its TSV has them, public (the public leg's own
# verdict, revocations and end), sweep, pins-end and the gate, whose `got` names each row that did
# not pass. Progress goes to stderr. A row it could not run reads UNKNOWN naming what it could not
# read, never PASS. Exit 0 when the gate passed, 1 when a row failed, 2
# otherwise. It prints no secret value: T2 and T3 read the canary's length alone, and the full
# member's cookie reaches curl in a header file alone. It prints no word gh said or answered but a
# value it expects (a visibility, a run's or a step's status, a conclusion, a run id, a verdict, an
# exit, a commit id, an image's sha256 digest): what gh says and answers, the pin files it reads and
# the run's artifacts it downloads included, is kept in a directory `mktemp -d` makes under TMPDIR
# (outside-agent-tailnet-gh.XXXXXX), which stderr names and the leg leaves in place, and a row names
# the file that holds what it does not print.
#
# Usage: OUTSIDE_PINS=pins.txt OUTSIDE_ENROLL_REFUSAL_COMMIT=<sha> ... bash outside-agent-tailnet.sh EXAMPLE

if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash outside-agent-tailnet.sh <project>'
  exit 2
fi
set -u
LC_ALL=C
export LC_ALL

case $0 in */*) here=${0%/*} ;; *) here=. ;; esac
here=$(cd "$here" && pwd)
for support in outside-agent-lib.sh outside-agent-peer.sh outside-agent-sweep.sh outside-agent-pins.sh outside-agent-grants.sh; do
  if [ ! -r "$here/$support" ]; then
    echo "UNKNOWN: $support is not beside this file in $here"
    exit 2
  fi
done
# shellcheck source=outside-agent-lib.sh
. "$here/outside-agent-lib.sh"
# shellcheck source=outside-agent-peer.sh
. "$here/outside-agent-peer.sh"
# shellcheck source=outside-agent-sweep.sh
. "$here/outside-agent-sweep.sh"
# shellcheck source=outside-agent-pins.sh
. "$here/outside-agent-pins.sh"
# shellcheck source=outside-agent-grants.sh
. "$here/outside-agent-grants.sh"
tab=$(printf '\t')
newline='
'
allow_http=${OUTSIDE_ALLOW_HTTP:-}

tsv=${OUTSIDE_TAILNET_TSV:-}
if [ -n "$tsv" ]; then
  : >"$tsv" || tsv=''
fi
emit() {
  printf '%s\n' "$1"
  [ -z "$tsv" ] || printf '%s\n' "$1" >>"$tsv"
  [ -z "${work:-}" ] || printf '%s\n' "$1" >>"$work/result.tsv"
}
fails=0
unknowns=0
# Each row that did not pass, `<row> <verdict>`, which the gate names.
not_passed=''
# row NAME WANT GOT VERDICT: prints the row and keeps its verdict as verdict_<NAME>. A verdict that
# is none of PASS, FAIL and UNKNOWN counts as UNKNOWN, so no word this leg copies from another
# file lets the gate pass.
row() {
  emit "$1$tab$(cell "$2")$tab$(cell "$3")$tab$4"
  # A name such as pins-end or sign-in keeps its verdict with each `-` read as `_`, which a bash
  # name may hold.
  printf -v "verdict_${1//-/_}" '%s' "$4"
  case $4 in
    PASS) ;;
    FAIL) fails=$((fails + 1)) ;;
    *) unknowns=$((unknowns + 1)) ;;
  esac
  [ "$4" = PASS ] || not_passed="${not_passed:+$not_passed, }$1 $(cell "$4")"
}
passed() {
  local name="verdict_${1//-/_}"
  [ "${!name:-}" = PASS ]
}
say() { printf 'outside-agent-tailnet: %s\n' "$1" >&2; }
# mark NAME TEXT: the number after `NAME=` in TEXT, or nothing.
mark() {
  local rest=${2#*"$1="}
  [ "$rest" != "$2" ] || return 0
  rest=${rest%%[!0-9]*}
  printf '%s' "$rest"
}
# mark_word NAME TEXT: the word after ` NAME=` (or `NAME=` at TEXT's start) in TEXT, up to a blank
# or a semicolon, or nothing.
mark_word() {
  local rest=" $2"
  rest=${rest#*" $1="}
  [ "$rest" != " $2" ] || return 0
  printf '%s' "${rest%%[ ;]*}"
}

# --- What the leg prints of what gh said or answered. Whatever answers gh's calls in GitHub's
# place (a GH_HOST, a proxy, a stand-in) holds gh's token and writes all that gh hands on: gh's own
# error message, its answer, the pin files `gh api` reads and the run's artifacts `gh run download`
# writes. So none of those is printed but a closed value this leg expects (a visibility, a status
# or a conclusion GitHub gives, a run id, a verdict, an exit, a count of seconds, a commit id, an
# image's sha256 digest): each is kept in a file of gh_kept, a directory beside the scratch one that
# outlives the leg, and a row names that file in this leg's own words.
# A public leg run here (OUTSIDE_PUBLIC_COMMAND) writes its results itself, with no gh between,
# so their words are printed as they are. ---
gh_kept=''
# results_gh: set once the public leg's results are the run's artifacts, which gh downloaded.
results_gh=''
# keep_gh: makes gh_kept, mode 0700, the first time a gh call needs it, and says where on stderr.
keep_gh() {
  [ -z "$gh_kept" ] || return 0
  gh_kept=$(mktemp -d "${TMPDIR:-/tmp}/outside-agent-tailnet-gh.XXXXXX") || gh_kept=''
  if [ -n "$gh_kept" ]; then
    say "what gh says and answers is kept in $gh_kept"
  else
    gh_kept="$work/gh"
    mkdir -p "$gh_kept"
    say "mktemp -d made no directory to keep what gh says and answers in: it is kept in $gh_kept, which goes with the scratch directory"
  fi
}
# artifact_text FILE TEXT: TEXT, words FILE holds, a file of the public leg's results, as a row
# prints them: TEXT itself where the public leg ran here, and where FILE is an artifact gh
# downloaded, words naming FILE in its place.
artifact_text() {
  if [ -n "$results_gh" ]; then
    printf 'kept in %s, which gh downloaded, so not printed' "$1"
  else
    printf '%s' "$2"
  fi
}
# artifact_word FILE WORD CLOSED...: WORD as artifact_text prints it, but printed as it is where it
# is one of CLOSED, the values the leg expects of it.
artifact_word() {
  local file=$1 word=$2 each
  shift 2
  for each in "$@"; do
    [ "$word" != "$each" ] || {
      printf '%s' "$word"
      return 0
    }
  done
  artifact_text "$file" "$word"
}

public_rows='P1 P2 P3 P4 P5 P6 P7 P8 P9 P10 C0 C1 C2 C3 C4 C5 C6 C7'
project=${1:-}
person=${OUTSIDE_PERSON:-}
canary=${OUTSIDE_CANARY:-}
repository=${OUTSIDE_REPOSITORY:-}
commit=${OUTSIDE_ENROLL_REFUSAL_COMMIT:-}
nonce=$(od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
[ -n "$nonce" ] || nonce=$$
peer=${OUTSIDE_PEER_SESSION:-outside-e2e-peer-$nonce}
# The gate this run holds (the header): `all`, both legs, or `browser`, the browser leg alone, which
# prints no P row and, in their place, the rows of the workflow's own checks.
outside_gate=${OUTSIDE_GATE:-all}
browser_rows='refusals reach sign-in projects limits grants'
gate_words='both legs'
if [ "$outside_gate" = browser ]; then
  public_rows=''
  gate_words='the browser leg'
fi

# --- Scratch directory and cleanup: the full member's helper's machine login revoked, every
# background child stopped and waited for, then the peer's tmux server, then the directory. ---
work=''
background=''
peer_tmux=''
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  # However the leg ends, an INT, a TERM or an abort included, the full member's helper's machine
  # login is revoked: its key goes with the directory, and the credential would stay live at the
  # broker until it expires. The normal path revokes it first and clears control_credential.
  [ -z "${control_credential:-}" ] ||
    browser POST "/api/v1/machine-logins/$control_credential/revoke" "$work/control-revoke.json" >/dev/null 2>&1
  reap
  peer_stop
  [ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
work=$(mktemp -d "${TMPDIR:-/tmp}/outside-agent-tailnet.XXXXXX") || work=''
tools="$work/tools"
# A program real_tools could not link holds every row from the pins on, as a missing scratch
# directory does: the helper, the probe and the peer all start under that PATH.
tools_problem=''
[ -z "$work" ] || tools_problem=$(real_tools "$tools" bun omp tmux agent-secrets agent-secrets-helper)

emit "row${tab}want${tab}got${tab}verdict"

# skip_from REASON FROM: every row from FROM on (the names in `rest_rows`) UNKNOWN for REASON.
rest_rows="pins issue T1 T2 T3 T4 T5 $public_rows public sweep pins-end"
[ "$outside_gate" != browser ] || rest_rows="pins issue unlisted T5 $browser_rows public sweep pins-end"
skip_from() {
  local reason=$1 from=$2 name on=''
  for name in $rest_rows; do
    [ "$name" = "$from" ] && on=yes
    [ -n "$on" ] || continue
    row "$name" "the row as outside-agent-acceptance.md states it" "not run: $reason" UNKNOWN
  done
}
# gate: the last row, and the exit. Only PASS passes: a row of any other verdict holds the gate,
# and the gate's `got` names it.
gate() {
  local verdict=PASS
  [ "$unknowns" -eq 0 ] || verdict=UNKNOWN
  [ "$fails" -eq 0 ] || verdict=FAIL
  emit "gate${tab}every row PASS${tab}$(cell "$fails FAIL, $unknowns UNKNOWN${not_passed:+: $not_passed}")${tab}$verdict"
  if [ -n "${OUTSIDE_RESULT_ISSUE:-}" ] && [ -n "$work" ] && command -v dispatch >/dev/null 2>&1; then
    {
      printf 'The outside acceptance of %s, %s, %s:\n\n```\n' "$project" "$gate_words" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
      cat "$work/result.tsv"
      printf '```\n'
    } | dispatch comment --issue "$OUTSIDE_RESULT_ISSUE" --body-file - >&2 ||
      say "the result could not be posted to $OUTSIDE_RESULT_ISSUE"
  fi
  case $verdict in
    PASS) exit 0 ;;
    FAIL) exit 1 ;;
  esac
  exit 2
}
case $outside_gate in
  all | browser) ;;
  *)
    skip_from "OUTSIDE_GATE must be all or browser, not $outside_gate" pins
    gate
    ;;
esac

# --- pins: the deployed images, the commit each was built from, and the build Dispatch serves. The
# pins are read at the start and again at the end (row pins-end), from the deployment repository's
# main when OUTSIDE_PINS_REPO names it; the reads are outside-agent-pins.sh's. ---

dispatch_commits=${OUTSIDE_DISPATCH_COMMITS:-}
# The deployments whose pins are read: the browser gate runs against the broker and Dispatch alone,
# and reads no edge listener's pin.
pin_names='broker dispatch edge-listener'
[ "$outside_gate" != browser ] || pin_names='broker dispatch'
want_pins="the broker's pin holds ${commit:-<OUTSIDE_ENROLL_REFUSAL_COMMIT>}; Dispatch serves its pin's build on every process (/healthz commit and fleet_commit), and that build holds ${dispatch_commits:-<OUTSIDE_DISPATCH_COMMITS>}; the pins printed"
problem=$(lacking OUTSIDE_PINS OUTSIDE_ENROLL_REFUSAL_COMMIT OUTSIDE_DISPATCH_COMMITS OUTSIDE_TAILNET_DISPATCH_URL)
[ -n "$project" ] || problem="${problem:+$problem; }the project key (\$1) is not given"
[ -n "$work" ] || problem="${problem:+$problem; }mktemp -d made no scratch directory"
[ -z "$tools_problem" ] || problem="${problem:+$problem; }$tools_problem"
if [ -z "$problem" ] && [ ! -r "$OUTSIDE_PINS" ]; then
  problem="OUTSIDE_PINS names $OUTSIDE_PINS, which cannot be read"
fi
# Each commit the builds are held to is a whole commit id: a list of blanks holds Dispatch's build
# to nothing, and a word that is no commit id is read by git as whatever it names.
read -r -a dispatch_shas <<<"$dispatch_commits"
if [ -z "$problem" ] && [ "${#dispatch_shas[@]}" -eq 0 ]; then
  problem="OUTSIDE_DISPATCH_COMMITS names no commit"
fi
for sha in "$commit" "${dispatch_shas[@]}"; do
  case $sha in
    *[!0-9a-f]* | '') ;;
    *) [ "${#sha}" -eq 40 ] && continue ;;
  esac
  [ -n "$problem" ] || problem="${sha:-an empty word} is no 40-character commit id (OUTSIDE_ENROLL_REFUSAL_COMMIT, OUTSIDE_DISPATCH_COMMITS)"
done
if [ -n "$problem" ]; then
  row pins "$want_pins" "$problem" UNKNOWN
  skip_from "the pins were not read: $problem" issue
  gate
fi
read_pins
if [ -n "$pins_problem" ]; then
  row pins "$want_pins" "$pins_problem" UNKNOWN
  skip_from "the pins were not read" issue
  gate
fi
pins_start=$pins_text
pins_start_said=$pins_said
read_serving
serving_start=$serving_commit
broker_revision=''
dispatch_revision=''
# A pin's revision is its line's last word, a whole commit id (read_pins): the image before it is
# the pin file's, which may hold a blank.
while IFS= read -r line; do
  case ${line%% *} in
    broker) broker_revision=${line##* } ;;
    dispatch) dispatch_revision=${line##* } ;;
  esac
done <<EOF
$pins_text
EOF
checkout=${OUTSIDE_LEGION_CHECKOUT:-$here}
# The broker's build must hold the enrollment refusal (T2), and Dispatch's build each commit
# OUTSIDE_DISPATCH_COMMITS names: the change that gives no broker token the approve scope, which
# P7's refusal and its control rest on, and the change that refuses a restricted member an issue's
# repository, which decides the repositories P5 and the sweep's GitHub proxy rows read. Dispatch's
# build is the one its /healthz says it serves, which must be its pin's and every process's: a
# deployment that serves another build than its pin, or is half rolled out, holds the gate.
lacks=''
problem=$serving_problem
dispatch_build=${serving_commit:-$dispatch_revision}
if [ -n "$serving_commit" ]; then
  case $serving_commit in
    "$dispatch_revision"*) ;;
    *) problem="Dispatch serves $serving_commit, and its pin names $dispatch_revision" ;;
  esac
fi
if ! command -v git >/dev/null 2>&1; then
  problem="${problem:+$problem; }git not on PATH"
else
  held=$(holds "$broker_revision" "$commit")
  case $held in
    yes) ;;
    no) lacks="the broker's build $broker_revision does not hold $commit" ;;
    *) problem="${problem:+$problem; }$held" ;;
  esac
  for sha in "${dispatch_shas[@]}"; do
    held=$(holds "$dispatch_build" "$sha")
    case $held in
      yes) ;;
      no) lacks="${lacks:+$lacks; }Dispatch's build $dispatch_build does not hold $sha" ;;
      *) problem="${problem:+$problem; }$held" ;;
    esac
  done
fi
# The pins as the rows print them (pins_said): an image gh read is named by the file that keeps it.
pins_line="$(printf '%s' "$pins_said" | tr '\n' ';') Dispatch serves ${serving_commit:-no commit read} (fleet ${serving_fleet:-none})"
if [ -n "$lacks" ]; then
  row pins "$want_pins" "$lacks${problem:+; $problem}; $pins_line" FAIL
  skip_from "a pin lacks a change the check needs, so the legs do not start" issue
  gate
elif [ -n "$problem" ]; then
  row pins "$want_pins" "$problem; $pins_line" UNKNOWN
  skip_from "the builds the check runs against are not read as it needs: $problem" issue
  gate
fi
row pins "$want_pins" "$pins_line" PASS

# --- issue: the scratch project's issue whose repository the edge relays (P5's admitted row), and
# which the Codex leg comments on (C3). ---
issue_title=$(outside_issue_title)
want_issue="an issue of $project named '$issue_title' on ${repository:-<OUTSIDE_REPOSITORY>}"
problem=$(lacking OUTSIDE_REPOSITORY)
[ -n "$problem" ] || problem=$(tool_problem dispatch)
if [ -n "$problem" ]; then
  row issue "$want_issue" "$problem" UNKNOWN
else
  listed=$(dispatch issues --project "$project" --limit 250 2>&1 </dev/null)
  key=''
  while IFS= read -r line; do
    case $line in *"$issue_title"*)
      line=${line#*"$project"-}
      line=${line%%[!0-9]*}
      [ -z "$line" ] || key="$project-$line"
      ;;
    esac
    [ -z "$key" ] || break
  done <<EOF
$listed
EOF
  if [ -z "$key" ]; then
    made=$(dispatch issue --project "$project" --title "$issue_title" --repo "$repository" --force 2>&1 </dev/null)
    case $made in *"$project-"[0-9]*)
      made=${made#*"$project"-}
      key="$project-${made%%[!0-9]*}"
      ;;
    esac
  fi
  if [ -z "$key" ]; then
    row issue "$want_issue" "no issue found or made: ${made:-$listed}" UNKNOWN
  else
    read=$(dispatch read --issue "$key" 2>&1 </dev/null)
    lower=$(printf '%s' "$repository" | tr '[:upper:]' '[:lower:]')
    case $(printf '%s' "$read" | tr '[:upper:]' '[:lower:]') in
      *"$lower"*) row issue "$want_issue" "$key names $repository" PASS ;;
      *) row issue "$want_issue" "$key does not name $repository: ${read:0:300}" FAIL ;;
    esac
  fi
fi
# The browser gate's workflow writes the person's own comment and issue message on this issue, the
# control of the sweep's member feed (`sweep_member_issue`), so it sends the key only of an issue
# the row read.
issue_key=''
! passed issue || issue_key=$key

# --- The scratch helper, its machine login and its code. ---
# helper_env DIR OPERATOR: sets helper_vars, the environment of a scratch agent-secrets-helper that
# keeps HOME, XDG_RUNTIME_DIR, XDG_STATE_HOME and every AGENT_SECRETS_* path in DIR, which it
# makes, with OPERATOR in its operator file and the broker's private origin as its broker.
helper_env() {
  mkdir -p "$1/home" "$1/run" "$1/state" "$1/keys"
  chmod 700 "$1" "$1/home" "$1/run" "$1/state" "$1/keys"
  printf '%s\n' "$2" >"$1/operator"
  helper_vars=(
    "PATH=$tools:$PATH"
    "HOME=$1/home"
    "XDG_RUNTIME_DIR=$1/run"
    "XDG_STATE_HOME=$1/state"
    "TMPDIR=${TMPDIR:-/tmp}"
    "AGENT_SECRETS_HELPER_SOCK=$1/helper.sock"
    "AGENT_SECRETS_MACHINE_FILE=$1/machine.json"
    "AGENT_SECRETS_STATE_FILE=$1/sessions.json"
    "AGENT_SECRETS_KEY_DIR=$1/keys"
    "AGENT_SECRETS_OPERATOR_FILE=$1/operator"
    "AGENT_SECRETS_URL=${OUTSIDE_TAILNET_BROKER_URL:-}"
  )
}
# start_login DIR: starts the helper helper_vars describes, its process id in helper_pid, and then
# its machine login, in the background, its process id in login_pid, writing DIR/helper.log and
# DIR/login.log. Sets login_code to the login's code, or login_problem. The socket and the code are
# looked at every pause, and each wait ends at once when the process that would make it has ended.
start_login() {
  local deadline line login_ended=''
  login_code=''
  login_problem=''
  login_pid=''
  env -i "${helper_vars[@]}" agent-secrets-helper serve >"$1/helper.log" 2>&1 </dev/null &
  helper_pid=$!
  background="$background $helper_pid"
  deadline=$(($(now) + 30))
  while [ ! -S "$1/helper.sock" ] && running_child "$helper_pid" && [ "$(now)" -lt "$deadline" ]; do
    pause
  done
  if [ ! -S "$1/helper.sock" ]; then
    if running_child "$helper_pid"; then
      login_problem="the scratch helper made no socket within 30 s: $(text_of "$1/helper.log")"
    else
      login_problem="the scratch helper ended before it made a socket: $(text_of "$1/helper.log")"
    fi
    return
  fi
  # The log exists before the login starts, so the read below never meets a missing file.
  : >"$1/login.log"
  env -i "${helper_vars[@]}" agent-secrets machine login >"$1/login.log" 2>&1 </dev/null &
  login_pid=$!
  background="$background $login_pid"
  deadline=$(($(now) + 60))
  # Whether the login still runs is read before its log: a login that has ended has written all it
  # printed, so a code it printed as it ended is still read, and the wait ends on that read.
  while [ -z "$login_code" ] && [ -z "$login_ended" ] && [ "$(now)" -lt "$deadline" ]; do
    running_child "$login_pid" || login_ended=yes
    while IFS= read -r line; do
      case $line in 'machine login code: '*) login_code=${line#machine login code: } ;; esac
    done <"$1/login.log"
    [ -n "$login_code" ] || [ -n "$login_ended" ] || pause
  done
  if [ -z "$login_code" ] && [ -n "$login_ended" ]; then
    login_problem="agent-secrets machine login ended and printed no code: $(text_of "$1/login.log")"
  elif [ -z "$login_code" ]; then
    login_problem="agent-secrets machine login printed no code within 60 s: $(text_of "$1/login.log")"
  fi
}
scratch="$work/scratch"
code=''
scratch_login_pid=''
helper_problem=''
# The browser gate starts no scratch helper: T1-T4 are the agents' path's.
if [ "$outside_gate" = all ]; then
  helper_env "$scratch" "$person"
  scratch_env=("${helper_vars[@]}")
  problem=$(lacking OUTSIDE_TAILNET_BROKER_URL OUTSIDE_PERSON OUTSIDE_CANARY)
  [ -n "$problem" ] || problem=$(tool_problem agent-secrets agent-secrets-helper)
  helper_problem=$problem
  if [ -z "$helper_problem" ]; then
    say "starting the scratch helper in $scratch"
    start_login "$scratch"
    code=$login_code
    scratch_login_pid=$login_pid
    helper_problem=$login_problem
  fi
fi

# --- The peer session, and the session outside the project P6 must not list
# (outside-agent-peer.sh). The probe runs with this box's broker identity alone: its AGENT_SECRETS_*
# settings, XDG directories, metadata host, HOME and TMPDIR, and no Dispatch or Envoy setting. The
# browser gate starts neither: its window opens on the workflow's step status, not to a peer. ---
if [ "$outside_gate" = all ]; then
  probe_env=()
  while IFS= read -r name; do
    case $name in AGENT_SECRETS_* | XDG_* | GCE_METADATA_HOST | HOME | TMPDIR) ;; *) continue ;; esac
    probe_env+=("$name=${!name}")
  done <<EOF
$(compgen -e)
EOF
  probe_env+=("PATH=$tools:$PATH")
  peer_setup
  peer_start
  foreign_start
fi

# --- The full member's browser session (OUTSIDE_FULL_COOKIE_FILE) at Dispatch's private origin.
# No broker token a machine holds approves, so the positive controls of T3 and P7 read with it; the
# browser gate's owner acts are made with it where their commands are unset; and the sweep's GitHub
# proxy reads use it where it is given and the full member's token (below) where it is not, which a
# build of Dispatch that reads the proxy as a browser's person alone refuses. The cookie reaches curl
# in a
# header file of mode 0600 alone, never on a command line. whoami names the member, whose address
# is the operator of the full member's helper (T3 and P7's control). The pins row has already held
# OUTSIDE_TAILNET_DISPATCH_URL. ---
browser_header="$work/full-browser.h"
full_login=''
browser_problem=$(lacking OUTSIDE_FULL_COOKIE_FILE)
if [ -z "$browser_problem" ] && [ ! -r "$OUTSIDE_FULL_COOKIE_FILE" ]; then
  browser_problem="OUTSIDE_FULL_COOKIE_FILE names $OUTSIDE_FULL_COOKIE_FILE, which cannot be read"
fi
if [ -z "$browser_problem" ]; then
  case $(tr -d '\r\n' <"$OUTSIDE_FULL_COOKIE_FILE") in
    *=*) (umask 077 && printf 'Cookie: %s\n' "$(tr -d '\r\n' <"$OUTSIDE_FULL_COOKIE_FILE")" >"$browser_header") ||
      browser_problem='the cookie header file could not be written' ;;
    *) browser_problem='OUTSIDE_FULL_COOKIE_FILE holds no <name>=<value>, as the Cookie header carries it' ;;
  esac
fi
# browser METHOD PATH OUT [BODY]: METHOD PATH at Dispatch's private origin as the full member's
# browser, with the origin's Origin as the dashboard sends it and the JSON file BODY; prints the
# answer's status, 000 when none came, and leaves its body in OUT.
browser() {
  local status
  local -a data=()
  [ -z "${4:-}" ] || data=(-H 'Content-Type: application/json' --data-binary "@$4")
  status=$(curl -q -sS -g --noproxy '*' --max-time 15 -X "$1" -o "$3" -w '%{http_code}' \
    -H "@$browser_header" -H "Origin: $OUTSIDE_TAILNET_DISPATCH_URL" ${data[@]+"${data[@]}"} \
    "$OUTSIDE_TAILNET_DISPATCH_URL$2" 2>"$work/browser.err" </dev/null)
  [ "${#status}" -eq 3 ] || status=000
  printf '%s' "$status"
}
if [ -z "$browser_problem" ]; then
  status=$(browser GET /api/v1/whoami "$work/full-whoami.json")
  answer=$(tr -d ' \n' <"$work/full-whoami.json" 2>/dev/null)
  full_login=$(json_field login "$answer")
  case $status:$answer in
    200:*'"kind":"user"'*) [ -n "$full_login" ] || browser_problem="whoami with the full member's cookie names no login: ${answer:0:200}" ;;
    *) browser_problem="whoami with the full member's cookie answered $status: ${answer:0:200} $(text_of "$work/browser.err")" ;;
  esac
fi

# --- The full member's personal token (OUTSIDE_SWEEP_FULL_TOKEN_FILE, the person scope) at
# Dispatch's private origin: the sweep's proxy control where no browser session is given, and the
# browser gate's grants control. It reaches curl in a header file of mode 0600 alone, never on a
# command line. ---
token_header="$work/full-token.h"
token_problem=$(token_setup)

# --- The read sweep's full-member half, its writer and its proxy control (outside-agent-sweep.sh).
sweep_setup
sweep_prepare
sweep_proxy_control

# --- The browser gate's owner acts (the header's owner commands), before the workflow starts: the
# test person goes on the project's access list, the list names them (the list command's control),
# and the owner's invite makes their link, which the workflow spends; then row unlisted asks the
# owner's invite for an address on no list. A command's output is never quoted, since a link is a
# secret: a row says its exit and, of a refusal, the `<status> <code>` it printed alone. ---
# owner NAME OUT WORD...: runs the owner command the variable NAME holds, split on blanks, with
# WORD... as its last words, its stdout in OUT and its stderr in OUT.err; prints its exit status.
# With NAME unset and the owner's browser session read (OUTSIDE_FULL_COOKIE_FILE), the leg makes
# the act itself with that session (owner_by_browser); with neither, it prints 127.
owner() {
  local name=$1 out=$2
  local -a command
  shift 2
  set -f
  # shellcheck disable=SC2206 # a command line, split on blanks on purpose
  command=(${!name:-})
  set +f
  if [ "${#command[@]}" -eq 0 ]; then
    if [ -z "$browser_problem" ]; then
      owner_by_browser "$name" "$out" "$@"
    else
      printf 127
    fi
    return
  fi
  "${command[@]}" "$@" >"$out" 2>"$out.err" </dev/null
  printf '%s' "$?"
}
# owner_input NAME: nothing where the owner act NAME names can be made, by its command or by the
# owner's browser session; else why not, naming both.
owner_input() {
  [ -z "${!1:-}" ] || return 0
  [ -n "$browser_problem" ] || return 0
  printf 'neither %s nor the owner'"'"'s browser session OUTSIDE_FULL_COOKIE_FILE is given (%s)' "$1" "$browser_problem"
}
# owner_by_browser NAME OUT WORD...: the owner act NAME's command makes, made with the owner's
# browser session at Dispatch's private origin (`browser`), its output in OUT and its exit printed
# as the command's contract says (the header): the add and the remove print nothing and exit 0 on
# a 2xx, the list prints the access list's people one a line, and the invite prints the link
# alone; a refusal prints `<status> <code>` and exits 1; no answer, an answer that is not the
# route's, or an address or project a path cannot carry as written, exits 2.
owner_by_browser() {
  local name=$1 out=$2 address='' key status answer words path rest each
  shift 2
  case $name in
    OUTSIDE_OWNER_LIST_COMMAND) key=$1 ;;
    *) address=$1 key=$2 ;;
  esac
  : >"$out"
  : >"$out.err"
  case "$address $key" in *[!A-Za-z0-9@._+\ -]*)
    printf 2
    return
    ;;
  esac
  path="/api/v1/projects/$key/access"
  printf '{}\n' >"$work/owner-body.json"
  case $name in
    OUTSIDE_OWNER_ADD_COMMAND) status=$(browser PUT "$path/people/$address" "$out.json" "$work/owner-body.json") ;;
    OUTSIDE_OWNER_REMOVE_COMMAND) status=$(browser DELETE "$path/people/$address" "$out.json") ;;
    OUTSIDE_OWNER_INVITE_COMMAND) status=$(browser POST "$path/people/$address/invitations" "$out.json" "$work/owner-body.json") ;;
    OUTSIDE_OWNER_LIST_COMMAND) status=$(browser GET "$path" "$out.json") ;;
    *)
      printf 127
      return
      ;;
  esac
  answer=$(tr -d ' \n' <"$out.json" 2>/dev/null)
  words=$(tr -d '\n' <"$out.json" 2>/dev/null)
  # The invite's answer holds the link: it lives in OUT alone from here, as a command's would.
  rm -f "$out.json"
  case $status in
    2??) ;;
    000)
      printf 2
      return
      ;;
    *)
      # Dispatch's refusal names its code, or, where its body holds none (the router's own, such as
      # 403 `invalid request origin` for a cookie write whose Origin is not the origin called), its
      # words, which hold no link.
      each=$(json_field code "$answer")
      [ -n "$each" ] || each=$(json_field error "$words" | tr -cd 'A-Za-z0-9_ ')
      printf '%s %s\n' "$status" "$each" >"$out"
      printf 1
      return
      ;;
  esac
  case $name in
    OUTSIDE_OWNER_LIST_COMMAND)
      case $answer in *'"people":['*) ;; *)
        printf 2
        return
        ;;
      esac
      rest=${answer#*\"people\":[}
      rest=${rest%%]*}
      rest=${rest//\"/}
      while [ -n "$rest" ]; do
        each=${rest%%,*}
        [ -z "$each" ] || printf '%s\n' "$each" >>"$out"
        [ "$each" != "$rest" ] || break
        rest=${rest#*,}
      done
      ;;
    OUTSIDE_OWNER_INVITE_COMMAND)
      each=$(json_field url "$answer")
      if [ -z "$each" ]; then
        printf 2
        return
      fi
      printf '%s\n' "$each" >"$out"
      ;;
  esac
  printf 0
}
# owner_lists ADDRESS: whether the owner's list command names ADDRESS on the project's access list,
# case ignored: true, false, or what was read.
owner_lists() {
  local listed line want found=false
  listed=$(owner OUTSIDE_OWNER_LIST_COMMAND "$work/owner-list" "$project")
  if [ "$listed" != 0 ]; then
    printf 'the owner list command exited %s' "$listed"
    return
  fi
  want=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  while IFS= read -r line || [ -n "$line" ]; do
    line=$(printf '%s' "${line//[[:space:]]/}" | tr '[:upper:]' '[:lower:]')
    [ "$line" != "$want" ] || found=true
  done <"$work/owner-list"
  printf '%s' "$found"
}
# owner_refusal OUT: the `<status> <code>` of a refusal OUT's first line holds, or nothing; the code
# may be a refusal's words where Dispatch named no code (owner_by_browser), and holds no `:`, `/` or
# `#`, so no link reads as one.
owner_refusal() {
  local first=''
  IFS= read -r first <"$1"
  first=${first%$'\r'}
  [[ $first =~ ^[1-5][0-9][0-9]\ [A-Za-z][A-Za-z0-9_\ ]*$ ]] && printf '%s' "$first"
}
invite_link=''
link_problem=''
if [ "$outside_gate" = browser ]; then
  link_problem=$(lacking OUTSIDE_PERSON)
  for each in OUTSIDE_OWNER_ADD_COMMAND OUTSIDE_OWNER_LIST_COMMAND OUTSIDE_OWNER_INVITE_COMMAND OUTSIDE_OWNER_REMOVE_COMMAND; do
    problem=$(owner_input "$each")
    [ -z "$problem" ] || link_problem="${link_problem:+$link_problem; }$problem"
  done
  [ -n "$link_problem" ] || [ -n "$work" ] || link_problem='mktemp -d made no scratch directory'
  if [ -z "$link_problem" ]; then
    say "the owner puts $person on $project's access list and makes their link"
    added=$(owner OUTSIDE_OWNER_ADD_COMMAND "$work/owner-add" "$person" "$project")
    listed=$(owner_lists "$person")
    if [ "$listed" != true ]; then
      link_problem="the owner add command exited $added$(added_said=$(owner_refusal "$work/owner-add") && printf ' (%s)' "$added_said"), and then $person is not read on $project's access list ($listed)"
    else
      invited=$(owner OUTSIDE_OWNER_INVITE_COMMAND "$work/owner-invite" "$person" "$project")
      line=''
      IFS= read -r line <"$work/owner-invite"
      line=${line%$'\r'}
      refused=''
      [ "$invited" != 1 ] || refused=$(owner_refusal "$work/owner-invite")
      # The link lives in this variable alone from here: it reaches the workflow on gh's stdin.
      rm -f "$work/owner-invite" "$work/owner-invite.err"
      case $invited:$line in
        0:*[[:space:][:cntrl:]]*) link_problem="the owner invite command printed a first line holding a blank or a control character, which is no link (its output is not quoted)" ;;
        0:https://?*\#?*) invite_link=$line ;;
        0:http://127.0.0.1:*\#?* | 0:http://localhost:*\#?*)
          if [ "$allow_http" = 1 ]; then
            invite_link=$line
          else
            link_problem="the owner invite command printed an http link, which only OUTSIDE_ALLOW_HTTP=1 lets through"
          fi
          ;;
        0:*) link_problem="the owner invite command exited 0 and printed no https://<host>/<path>#<token> link on its first line (its output is not quoted)" ;;
        1:*) link_problem="the owner invite command was refused ${refused:-with no <status> <code> on its first line}" ;;
        *) link_problem="the owner invite command exited $invited (its output is not quoted)" ;;
      esac
    fi
  fi

  # --- unlisted: an address no list of the project names cannot get a link. The refusal means
  # something only where the same command made a link for a listed address: the test person's,
  # above. ---
  unlisted=${OUTSIDE_UNLISTED_PERSON:-outside-e2e-unlisted-$nonce@example.com}
  want_unlisted="the owner's invite for $unlisted, an address on no access list of $project, refused 409 PERSON_NOT_LISTED, beside the owner's invite for ${person:-<OUTSIDE_PERSON>} answered with a link; a link made reads FAIL, any other answer UNKNOWN"
  problem=$(owner_input OUTSIDE_OWNER_INVITE_COMMAND)
  [ -n "$problem" ] || [ -n "$work" ] || problem='mktemp -d made no scratch directory'
  if [ -n "$problem" ]; then
    row unlisted "$want_unlisted" "$problem" UNKNOWN
  else
    invited=$(owner OUTSIDE_OWNER_INVITE_COMMAND "$work/owner-unlisted" "$unlisted" "$project")
    refused=''
    [ "$invited" != 1 ] || refused=$(owner_refusal "$work/owner-unlisted")
    rm -f "$work/owner-unlisted" "$work/owner-unlisted.err"
    case $invited:$refused in
      0:*) row unlisted "$want_unlisted" "the owner's invite made a link for $unlisted (exit 0; the link is not printed)" FAIL ;;
      1:'409 PERSON_NOT_LISTED')
        if [ -n "$invite_link" ]; then
          row unlisted "$want_unlisted" "refused 409 PERSON_NOT_LISTED, and the owner's invite made ${person}'s link" PASS
        else
          row unlisted "$want_unlisted" "refused 409 PERSON_NOT_LISTED, and no positive control: $link_problem" UNKNOWN
        fi
        ;;
      1:?*) row unlisted "$want_unlisted" "refused $refused, which is not the refusal of an address no list names" UNKNOWN ;;
      1:) row unlisted "$want_unlisted" "the owner invite command exited 1 with no <status> <code> on its first line (its output is not quoted)" UNKNOWN ;;
      *) row unlisted "$want_unlisted" "the owner invite command exited $invited (its output is not quoted)" UNKNOWN ;;
    esac
  fi
fi

# --- The browser gate's grants control (row grants, outside-agent-grants.sh): the full member's own
# list of credential grants, read with OUTSIDE_SWEEP_FULL_TOKEN_FILE seconds before the run starts
# (grants_read_before, which picks the probe grant) and again once it has ended. A grant every
# signed-in person is listed that both reads hold was live while the runner read the test person's
# own list, which must hold no such grant; the row passes only where there is one. The driver reads
# its own list again just before its revoke, and sends none where that list shows the probe grant
# naming an approver or an operator. `bash outside-agent-grants.sh` alone is the same read, the
# preflight a person runs before asking for a live gate. ---
grants_problem=''
grants_probe=''
grants_before=''
[ "$outside_gate" != browser ] || grants_problem=$token_problem

# --- The public leg: the workflow on a runner on the public internet, or a local command. The
# browser gate starts the workflow alone, and only with the owner's link for the test person. ---
results="$work/public"
mkdir -p "$results"
public_problem=''
public_end=''
# The public leg's end as a row prints it: its exit, or the run's conclusion where it is GitHub's.
end_said=''
public_pid=''
run_id=''
if [ "$outside_gate" = browser ] && [ -n "$link_problem" ]; then
  public_problem="not started: no invitation link for ${person:-<OUTSIDE_PERSON>}: $link_problem"
elif [ "$outside_gate" = browser ] && [ -n "${OUTSIDE_PUBLIC_COMMAND:-}" ]; then
  public_problem="not started: OUTSIDE_PUBLIC_COMMAND runs the agents' public leg here, and the browser gate opens its window only on the workflow's step status"
elif [ -n "$helper_problem" ]; then
  public_problem="not started: the scratch helper has no code: $helper_problem"
elif [ -n "${OUTSIDE_PUBLIC_COMMAND:-}" ]; then
  say "starting the public leg here: $OUTSIDE_PUBLIC_COMMAND"
  set -f
  # shellcheck disable=SC2086 # a command line, split on blanks on purpose
  set -- $OUTSIDE_PUBLIC_COMMAND
  set +f
  OUTSIDE_TAILNET_USER_CODE=$code OUTSIDE_PEER_SESSION=$peer \
    OUTSIDE_SWEEP_FILLS=$sweep_fills OUTSIDE_PUBLIC_RESULTS=$results \
    "$@" >"$work/public.log" 2>&1 </dev/null &
  public_pid=$!
  background="$background $public_pid"
else
  problem=$(lacking OUTSIDE_WORKFLOW_REPO)
  [ -n "$problem" ] || problem=$(tool_problem gh)
  # A run's inputs (the project, the sweep's project and fills naming the proxy's repository) are
  # as readable as its repository, so the workflow starts only in a private or an internal one: an
  # internal repository is readable by the owning enterprise's members alone, who can read those
  # inputs already. A public one, or a visibility not read, starts no run; a `gh repo view` that
  # failed has read nothing, whatever it printed. Its answer is printed only as one of GitHub's
  # three visibilities, and kept in gh_kept.
  if [ -z "$problem" ]; then
    keep_gh
    gh repo view "$OUTSIDE_WORKFLOW_REPO" --json visibility --jq .visibility >"$gh_kept/repo-view" 2>/dev/null </dev/null
    viewed=$?
    visibility=$(<"$gh_kept/repo-view")
    case $viewed:$visibility in
      0:PRIVATE | 0:INTERNAL) ;;
      0:) problem="the visibility of $OUTSIDE_WORKFLOW_REPO could not be read, and a run's inputs are as readable as its repository" ;;
      0:PUBLIC) problem="$OUTSIDE_WORKFLOW_REPO is PUBLIC, not PRIVATE or INTERNAL, and a run's inputs are as readable as its repository" ;;
      0:*) problem="gh repo view answered a visibility of $OUTSIDE_WORKFLOW_REPO that is none of PRIVATE, INTERNAL and PUBLIC (its answer is kept in $gh_kept/repo-view), and a run's inputs are as readable as its repository" ;;
      *) problem="the visibility of $OUTSIDE_WORKFLOW_REPO could not be read (gh repo view exited $viewed), and a run's inputs are as readable as its repository" ;;
    esac
  fi
  if [ -n "$problem" ]; then
    public_problem="not started: $problem"
  else
    if [ "$outside_gate" = browser ]; then
      # The browser checks alone, with Dispatch's route the one routed control, the owner's link,
      # and the issue the person's own comment and issue message go on.
      inputs="{\"checks\":\"browser\",\"project\":$(json_string "$project"),\"routed_controls\":\"dispatch-healthz\",\"invite_url\":$(json_string "$invite_link")"
      [ -z "$issue_key" ] || inputs="$inputs,\"sweep_member_issue\":$(json_string "$issue_key")"
      # The probe grant, read seconds before the run starts.
      grants_read_before
      [ -z "$grants_probe" ] || inputs="$inputs,\"grants_probe_id\":$(json_string "$grants_probe")"
    else
      inputs="{\"checks\":\"all\",\"project\":$(json_string "$project"),\"peer_session\":$(json_string "$peer"),\"tailnet_user_code\":$(json_string "$code")"
    fi
    # The sweep's project goes with its fills: the runner's half of the sweep reads that project.
    [ -z "${OUTSIDE_SWEEP_PROJECT:-}" ] || inputs="$inputs,\"sweep_project\":$(json_string "$OUTSIDE_SWEEP_PROJECT")"
    [ -z "$sweep_fills" ] || inputs="$inputs,\"sweep_fills\":$(json_string "$sweep_fills")"
    while IFS= read -r pair; do
      if [ "$outside_gate" = browser ] && [ "${pair%%=*}" = invite_url ]; then
        say "OUTSIDE_WORKFLOW_INPUTS holds invite_url, which the browser gate takes from the owner's invite; it is left out"
        continue
      fi
      case $pair in
        '') ;;
        *=*) inputs="$inputs,$(json_string "${pair%%=*}"):$(json_string "${pair#*=}")" ;;
        *) say "OUTSIDE_WORKFLOW_INPUTS holds a line with no name=value; it is left out" ;;
      esac
    done <<EOF
${OUTSIDE_WORKFLOW_INPUTS:-}
EOF
    inputs="$inputs}"
    workflow=${OUTSIDE_WORKFLOW:-outside-edge-e2e.yaml}
    say "gh workflow run $workflow -R $OUTSIDE_WORKFLOW_REPO --json, its inputs on stdin"
    # What gh printed, its words and its answer alike, is kept: only the run's id is printed.
    printf '%s' "$inputs" | gh workflow run "$workflow" -R "$OUTSIDE_WORKFLOW_REPO" --json >"$gh_kept/workflow-run" 2>&1
    started=$(<"$gh_kept/workflow-run")
    case $started in */actions/runs/[0-9]*)
      run_id=${started##*/actions/runs/}
      run_id=${run_id%%[!0-9]*}
      ;;
    esac
    [ -n "$run_id" ] || public_problem="gh workflow run printed no run URL; what it printed is kept in $gh_kept/workflow-run"
  fi
fi

# --- T1: the scratch helper's machine login completes, approved as the test person. The browser
# gate runs no T1-T4 and no full member's helper: they are the agents' path's (the header). ---
want_t1="the scratch helper's machine login completes: its machine file holds a credential, and machine list names ${person:-<OUTSIDE_PERSON>} as its approver"
credential=''
if [ "$outside_gate" = browser ]; then
  :
elif [ -n "$helper_problem" ]; then
  row T1 "$want_t1" "$helper_problem" UNKNOWN
elif [ -n "$public_problem" ]; then
  # Only the public leg's browser approves the code, so with no public leg nothing will.
  row T1 "$want_t1" "not run: only the public leg approves code $code, and it did not start: $public_problem" UNKNOWN
else
  say "T1: waiting for the code $code to be approved (the code waits 15 minutes)"
  await_child "$scratch_login_pid" 960
  exited=$child_exit
  [ ! -f "$scratch/machine.json" ] || credential=$(json_field credential_id "$(tr -d ' \n' <"$scratch/machine.json")")
  listed=$(env -i "${scratch_env[@]}" agent-secrets machine list --json 2>&1 </dev/null)
  person_lower=$(printf '%s' "$person" | tr '[:upper:]' '[:lower:]')
  if [ -z "$exited" ]; then
    row T1 "$want_t1" "code $code was not approved within 16 minutes" UNKNOWN
  elif [ "$exited" -ne 0 ]; then
    # A code nobody approved ends the login `expired` after 15 minutes: the public leg never
    # approved it, which is a missing step, not a crossing. A denial, or any other end, is.
    case $(tr '\n' ' ' <"$scratch/login.log") in
      *'machine login: expired'*)
        row T1 "$want_t1" "code $code was never approved: the login expired" UNKNOWN
        ;;
      *) row T1 "$want_t1" "machine login exited $exited: $(text_of "$scratch/login.log")" FAIL ;;
    esac
  elif [ -z "$credential" ]; then
    row T1 "$want_t1" "machine login exited 0 and the machine file holds no credential_id" FAIL
  else
    case $(printf '%s' "$listed" | tr '[:upper:]' '[:lower:]') in
      *"\"credential_id\":\"$credential\""*"\"approved_by\":\"$person_lower\""*)
        row T1 "$want_t1" "credential $credential, approved by $person_lower" PASS
        ;;
      *) row T1 "$want_t1" "credential $credential; machine list: ${listed:0:300}" FAIL ;;
    esac
  fi
fi

# length.sh NAME: asks for NAME and prints three lines, never the value itself: `request-exit`, the
# request's exit; `request-said`, what it printed on stderr, on one line; and `length`, the length
# of the value the session was given, empty when it was given none.
cat >"$work/length.sh" <<'SH'
#!/bin/sh
said=$(agent-secrets request "$1" 2>&1 >/dev/null)
echo "request-exit $?"
printf 'request-said %s\n' "$(printf '%s' "$said" | tr '\n' ' ')"
printf 'length %s\n' "$(agent-secrets "$1" -- sh -c 'printenv "$0" | tr -d "\n" | wc -c' "$1" 2>/dev/null | tr -d ' ')"
SH
chmod 700 "$work/length.sh"
# read_length FILE: length.sh's three lines in FILE, as request_exit, request_said and length.
read_length() {
  local line
  request_exit='' request_said='' length=''
  while IFS= read -r line; do
    case $line in
      'request-exit '*) request_exit=${line#request-exit } ;;
      'request-said '*) request_said=${line#request-said } ;;
      'length '*) length=${line#length } ;;
    esac
  done <"$1"
}

# --- T2: the scratch helper's enrollment is refused, and the canary reaches nobody through it,
# each for the member kind. A canary that got no value for another reason (a requesters list on
# the secret that names someone else, a session the helper never asked for) says nothing of the
# member kind, so that reads FAIL too. ---
member_refusal='403 RESTRICTED_MEMBER_ENROLLS_NOTHING'
want_t2="an enrollment through the scratch helper refused $member_refusal; ${canary:-<OUTSIDE_CANARY>} asked for through it gets no value, its session refused by that code"
if [ "$outside_gate" = browser ]; then
  :
elif ! passed T1; then
  row T2 "$want_t2" "not run: T1 did not pass, so the scratch helper holds no login" UNKNOWN
else
  # Each register runs its own launcher (--exec), so the session it names is that process and not
  # this leg's shell: a register with no --exec registers the shell it runs in, and every later
  # agent-secrets call of this leg would then be refused IN_SESSION. Inside it, whoami reads the
  # session's enrollment, which a refused one has none of.
  env -i "${scratch_env[@]}" agent-secrets register --session-id "outside-e2e-t2-$nonce" --wait 15 \
    --exec -- agent-secrets whoami >"$work/t2.out" 2>"$work/t2.err" </dev/null
  said=$(tr '\n' ' ' <"$work/t2.out")
  refusal=$(tr '\n' ' ' <"$work/t2.err")
  env -i "${scratch_env[@]}" agent-secrets register --session-id "outside-e2e-t2b-$nonce" \
    --exec -- sh "$work/length.sh" "$canary" >"$work/t2b.out" 2>"$work/t2b.err" </dev/null
  read_length "$work/t2b.out"
  # What the canary's attempt was told: its session's refusal and the request's own words.
  canary_refusal="$(tr '\n' ' ' <"$work/t2b.err") $request_said"
  asked="the canary asked for through it: request exit ${request_exit:-none} (${request_said:-nothing said}), length ${length:-none}"
  case $said in
    *enrollment_id*)
      row T2 "$want_t2" "the scratch helper's session enrolled: ${said:0:300}" FAIL
      ;;
    *)
      case $refusal in
        *"$member_refusal"*)
          if [ -n "$length" ] && [ "$length" != 0 ]; then
            row T2 "$want_t2" "refused, and yet the canary reached the session: a value $length long" FAIL
          else
            case $canary_refusal in
              *"$member_refusal"*)
                row T2 "$want_t2" "refused by that code, and whoami inside reads no enrollment; the canary's session refused by that code too, and no value; $asked" PASS
                ;;
              *MEMBER_KIND_UNCHECKABLE*)
                row T2 "$want_t2" "the broker could not ask Dispatch the member kind for the canary's session: ${canary_refusal:0:300}" UNKNOWN
                ;;
              *)
                row T2 "$want_t2" "the enrollment was refused by that code, but the canary got no value for another reason, which says nothing of the member kind: ${canary_refusal:0:300}; $asked" FAIL
                ;;
            esac
          fi
          ;;
        *MEMBER_KIND_UNCHECKABLE*)
          row T2 "$want_t2" "the broker could not ask Dispatch the member kind: ${refusal:0:300}" UNKNOWN
          ;;
        *) row T2 "$want_t2" "no RESTRICTED_MEMBER_ENROLLS_NOTHING: ${refusal:0:300}; $asked" FAIL ;;
      esac
      ;;
  esac
fi

# --- The full member's helper, the positive control of T3 and of P7. No broker token a machine
# holds approves, the full member's own included, so the 403 OAUTH_SCOPE_REQUIRED that P7 reads for
# the agent's token says something about that token only where a browser session is answered on
# the same routes. The full member's browser mints a personal agent token (201), revoked again at
# once, and approves a
# machine login (200) of a scratch helper whose operator is that member and whose broker is
# OUTSIDE_TAILNET_BROKER_URL, the broker T2's scratch helper enrolls at. T3 enrolls through that
# helper, and then its login is revoked and the helper stopped. T3 needs the helper alone
# (control_helper_problem); P7 needs the helper and the mint (p7_control_problem), so without both
# answers P7 reads UNKNOWN. The mint's answer, the one place the token's plaintext is shown, is
# removed as soon as its id is read. ---
p7_control=''
control_helper_problem=$browser_problem
[ "$outside_gate" = all ] || control_helper_problem='the browser gate makes no full member helper'
[ -n "$control_helper_problem" ] || control_helper_problem=$(lacking OUTSIDE_TAILNET_BROKER_URL)
[ -n "$control_helper_problem" ] || control_helper_problem=$(tool_problem agent-secrets agent-secrets-helper)
control_env=()
minted=''
approved=''
if [ -z "$control_helper_problem" ]; then
  say "the full member's helper: the browser mints an agent token and approves a machine login"
  printf '{"name":"outside-e2e-control-%s"}\n' "$nonce" >"$work/mint-body.json"
  minted=$(browser POST /api/v1/me/agent-tokens "$work/mint.json" "$work/mint-body.json")
  token_id=$(json_field id "$(tr -d ' \n' <"$work/mint.json" 2>/dev/null)")
  rm -f "$work/mint.json"
  dropped=none
  [ -z "$token_id" ] || dropped=$(browser DELETE "/api/v1/me/agent-tokens/$token_id" "$work/mint-revoke.json")
  control="$work/control"
  helper_env "$control" "$full_login"
  control_env=("${helper_vars[@]}")
  start_login "$control"
  control_login_pid=$login_pid
  control_helper_pid=$helper_pid
  control_credential=''
  if [ -n "$login_problem" ]; then
    approved="no code: $login_problem"
  else
    printf '{"code":"%s"}\n' "$login_code" >"$work/code-body.json"
    looked=$(browser POST /api/v1/credential-requests/machine-lookup "$work/lookup.json" "$work/code-body.json")
    record=$(json_field record_id "$(tr -d ' \n' <"$work/lookup.json" 2>/dev/null)")
    if [ "$looked" != 200 ] || [ -z "$record" ]; then
      approved="the lookup of its code answered $looked: $(text_of "$work/lookup.json")"
    else
      approved=$(browser POST "/api/v1/credential-requests/$record/approve" "$work/approve.json" "$work/code-body.json")
      control_credential=$(json_field credential_id "$(tr -d ' \n' <"$work/approve.json" 2>/dev/null)")
      [ -n "$control_credential" ] || approved="$approved, with no credential_id: $(text_of "$work/approve.json")"
    fi
  fi
  # The approved login completes and writes its machine file; then the helper can enroll.
  child_exit=''
  [ -z "$control_credential" ] || await_child "$control_login_pid" 30
  case $approved:$child_exit in
    2??:0) ;;
    2??:*) control_helper_problem="the full member's machine login did not complete within 30 s of its approval (${child_exit:+exit $child_exit}${child_exit:-still running})" ;;
    *) control_helper_problem="the full member's browser approved no machine login: $approved" ;;
  esac
fi
p7_control_problem=$control_helper_problem
if [ -z "$p7_control_problem" ]; then
  case $minted in
    201) p7_control="the full member's browser minted an agent token, 201 (revoked: $dropped), and approved a machine login, $approved" ;;
    *) p7_control_problem="the full member's browser mint of an agent token answered $minted" ;;
  esac
fi

# --- T3: the positive control of T2: a fresh session enrolled through the full member's helper,
# whose broker is OUTSIDE_TAILNET_BROKER_URL, the broker T2's refusal came from, is granted the
# canary at once. The broker a session enrolls at is its helper's (the helper enrolls under its own
# AGENT_SECRETS_URL), so the helper is the one this leg started with that URL. ---
want_t3="a fresh session enrolled through a full member's scratch helper whose broker is ${OUTSIDE_TAILNET_BROKER_URL:-<OUTSIDE_TAILNET_BROKER_URL>} is granted ${canary:-<OUTSIDE_CANARY>} at once, a value 64 long"
problem=$(lacking OUTSIDE_CANARY)
[ -n "$problem" ] || problem=$control_helper_problem
if [ "$outside_gate" = browser ]; then
  :
elif [ -n "$problem" ]; then
  row T3 "$want_t3" "no full member's helper at the broker: $problem" UNKNOWN
else
  env -i "${control_env[@]}" agent-secrets register --session-id "outside-e2e-t3-$nonce" \
    --wait 30 --exec -- sh "$work/length.sh" "$canary" >"$work/t3.out" 2>"$work/t3.err" </dev/null
  read_length "$work/t3.out"
  case $request_exit:$length in
    0:64) row T3 "$want_t3" "granted at once, a value 64 long" PASS ;;
    0:*[0-9]*) row T3 "$want_t3" "granted, a value $length long" FAIL ;;
    75:*) row T3 "$want_t3" "the request waits for a person's approval (exit 75), not granted at once" FAIL ;;
    77:*) row T3 "$want_t3" "the request was denied (exit 77): $request_said" FAIL ;;
    *) row T3 "$want_t3" "request exit ${request_exit:-none}, length ${length:-none}: $request_said $(text_of "$work/t3.err")" UNKNOWN ;;
  esac
fi

# The full member's helper has served its two controls: its login is revoked, and it stops.
if [ -n "${control_helper_pid:-}" ]; then
  revoked=none
  [ -z "${control_credential:-}" ] ||
    revoked=$(browser POST "/api/v1/machine-logins/$control_credential/revoke" "$work/control-revoke.json")
  [ -z "${control_login_pid:-}" ] || stop_child "$control_login_pid"
  stop_child "$control_helper_pid"
  case $revoked in
    2??) control_credential='' ;;
    *) say "the full member's helper's machine login was not revoked ($revoked); the leg's cleanup asks again" ;;
  esac
  case $dropped in 2??) ;; *) say "the control's agent token was not revoked ($dropped)" ;; esac
  [ -z "$p7_control" ] || p7_control="$p7_control (revoked: $revoked)"
fi

# --- T5's removal, during the public run's T5 window: the public leg tells the peer the window is
# open (`outside-e2e t5-open <nonce> at <ms>`), this leg has the owner remove the test person from
# the project's access list, on this terminal or with OUTSIDE_OWNER_REMOVE_COMMAND (a harness that
# acts as the owner's browser), and keeps the clock before it asks (removal_from) and after the
# owner answers (removal_to): the removal was made between the two. T5 is judged after the run,
# below. The list is read with the full member's browser session at OUTSIDE_TAILNET_DISPATCH_URL
# (the route takes the person scope, which a box's broker token need not hold), before (the
# control: the person is on it) and after (the removal was made). The browser gate's window is the
# workflow's step `The delisting window`, read in_progress from `gh run view --json jobs`, and it
# reads the list and removes the person with the owner's commands alone, never on a terminal. ---
window_step='The delisting window'
# listed_now: whether the person is on the project's access list: true, false, or what was read.
listed_now() {
  local status answer people
  if [ "$outside_gate" = browser ]; then
    owner_lists "$person"
    return
  fi
  if [ -n "$browser_problem" ]; then
    printf 'the full member'"'"'s browser session was not read: %s' "$browser_problem"
    return
  fi
  status=$(browser GET "/api/v1/projects/$project/access" "$work/access.json")
  answer=$(tr -d ' \n' <"$work/access.json" 2>/dev/null)
  case $status:$answer in
    200:*'"people":['*)
      people=${answer#*\"people\":[}
      people=${people%%]*}
      case ",$(printf '%s' "$people" | tr '[:upper:]' '[:lower:]')," in
        *",\"$(printf '%s' "$person" | tr '[:upper:]' '[:lower:]')\","*) printf true ;;
        *) printf false ;;
      esac
      ;;
    *) printf 'GET /api/v1/projects/%s/access answered %s: %s' "$project" "$status" "${answer:0:200}" ;;
  esac
}
# window_read: the run's status and its step `The delisting window`'s, `<run> <step>` (the step
# `absent` while the run lists no such step), read in one call. gh's answer is kept in gh_kept as
# well: the last read is printed only through window_words.
window_read() {
  gh run view "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --json status,jobs \
    --jq ".status + \" \" + ([.jobs[].steps[]? | select(.name == \"$window_step\") | .status] | first // \"absent\")" \
    >"$gh_kept/window-read" 2>/dev/null </dev/null
  printf '%s' "$(<"$gh_kept/window-read")"
}
# window_words READ: READ, a window read, where the words before and after its first blank are
# each a status GitHub gives a run or a step, `skipped`, which the wait reads a step as, or
# `absent`, window_read's own word; nothing for an empty READ; else words naming the file that
# keeps the last read.
window_words() {
  local word
  [ -n "$1" ] || return 0
  for word in "${1%% *}" "${1#* }"; do
    case $word in
      queued | in_progress | completed | waiting | requested | pending | skipped | absent) ;;
      *)
        printf 'an answer of no statuses GitHub gives, kept in %s' "$gh_kept/window-read"
        return 0
        ;;
    esac
  done
  printf '%s' "$1"
}
# public_running: the public leg has not ended yet.
public_running() {
  if [ -n "$run_id" ]; then
    case $(gh run view "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --json status --jq .status 2>/dev/null </dev/null) in
      completed) return 1 ;;
    esac
    return 0
  fi
  [ -n "$public_pid" ] && running_child "$public_pid"
}
removal_from=''
removal_to=''
t5_problem=''
if [ "$outside_gate" = browser ]; then
  [ -z "$public_problem" ] || t5_problem="the workflow did not start: $public_problem"
else
  [ -n "$t5_problem" ] || [ -z "$public_problem" ] || t5_problem="the public leg did not start: $public_problem"
  [ -n "$t5_problem" ] || [ -z "$peer_problem" ] || t5_problem="no peer, which the window is opened to: $peer_problem"
  [ -n "$t5_problem" ] || [ -n "${OUTSIDE_OWNER_REMOVE_COMMAND:-}" ] || { : </dev/tty; } 2>/dev/null ||
    t5_problem="there is no terminal to ask the owner for the removal on, and no OUTSIDE_OWNER_REMOVE_COMMAND"
fi
if [ -z "$t5_problem" ]; then
  opened=''
  window_problem=''
  window_deadline=$(($(now) + 3600))
  if [ "$outside_gate" = browser ]; then
    # The step's status is read every 10 s, and the step gives the driver 1200 s, so the removal
    # comes early in its window. A step read `completed` (or `skipped`) before it was read running
    # ended without this leg's removal, as a step does at once when the driver kept no cookie, and
    # an hour bounds the wait, as the public leg's window is bounded below.
    say "T5: waiting for run $run_id's step '$window_step' to run"
    read_status=''
    while :; do
      read_status=$(window_read)
      case $read_status in
        *' in_progress') opened=yes && break ;;
        'completed '* | *' completed' | *' skipped') break ;;
      esac
      [ "$(now)" -lt "$window_deadline" ] || break
      sleep 10
    done
    # The read is gh's answer: printed through window_words alone. A step read `completed` or
    # `skipped` prints that word, which its pattern holds it to.
    read_said=$(window_words "$read_status")
    case $opened:$read_status in
      yes:*) ;;
      :'completed '*) window_problem="run $run_id ended before this leg read its step '$window_step' running (the step read ${read_said#completed })" ;;
      :*' completed' | :*' skipped') window_problem="run $run_id's step '$window_step' read ${read_status##* } before this leg read it running" ;;
      *) window_problem="run $run_id's step '$window_step' was not read running within an hour (last read: ${read_said:-nothing})" ;;
    esac
  else
    say "T5: waiting for the public leg to open its window"
    # The window line is read before whether the public leg still runs: a leg that opened its
    # window and has since ended still opened it. The public leg opens it after P10, well within an
    # hour. The peer's text is looked at every pause; a local public leg's end at each look, and a
    # workflow run's, which is GitHub's to answer, at most every 5 s.
    run_asked=0
    while [ -z "$opened" ]; do
      case $(peer_text) in *'outside-e2e t5-open '*) opened=yes && break ;; esac
      if [ -z "$run_id" ] || [ "$(now)" -ge $((run_asked + 5)) ]; then
        public_running || break
        run_asked=$(now)
      fi
      [ "$(now)" -lt "$window_deadline" ] || break
      pause
    done
    if [ -z "$opened" ] && public_running; then
      window_problem="the public leg had not opened its window within an hour"
    elif [ -z "$opened" ]; then
      window_problem="the public leg ended without opening its window"
    fi
  fi
  before=''
  [ -n "$window_problem" ] || before=$(listed_now)
  if [ -n "$window_problem" ]; then
    t5_problem=$window_problem
  elif [ "$before" != true ]; then
    t5_problem="before the removal $person is not read on the access list ($before), so a refusal would prove nothing"
  else
    removal_from=$(now)
    if [ "$outside_gate" = browser ]; then
      removed=$(owner OUTSIDE_OWNER_REMOVE_COMMAND "$work/t5.remove" "$person" "$project")
      [ "$removed" = 0 ] || say "T5: the owner remove command exited $removed"
    elif [ -n "${OUTSIDE_OWNER_REMOVE_COMMAND:-}" ]; then
      set -f
      # shellcheck disable=SC2086 # a command line, split on blanks on purpose
      set -- $OUTSIDE_OWNER_REMOVE_COMMAND
      set +f
      "$@" "$person" "$project" >"$work/t5.remove" 2>&1 </dev/null ||
        say "T5: the removal command exited non-zero: $(text_of "$work/t5.remove")"
    else
      printf 'outside-agent-tailnet: T5: remove %s from %s'"'"'s access list now, then press Enter here.\n' \
        "$person" "$project" >/dev/tty
      read -r _ </dev/tty
    fi
    removal_to=$(now)
    say "T5: the removal made between $((removal_to - removal_from)) s and 0 s ago, read at $(date -u '+%H:%M:%SZ')"
  fi
fi

# --- The public leg's end, its TSV and its revocations. ---
if [ -z "$public_problem" ] && [ -n "$run_id" ]; then
  # gh polls the run every 30 s; the leg holds that watch to two hours, past the workflow's own
  # budget of 115 minutes, and a run not ended by then, or a watch that ended before the run did,
  # is read as no end.
  say "waiting for run $run_id"
  gh run watch "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --interval 30 >/dev/null 2>&1 </dev/null &
  watch_pid=$!
  background="$background $watch_pid"
  await_child "$watch_pid" 7200
  [ -n "$child_exit" ] || stop_child "$watch_pid"
  if public_running; then
    public_problem="run $run_id had not ended within two hours, or its watch ended before it did"
  else
    artifacts='outside-agent-public outside-edge-revocations restricted-sweep'
    [ "$outside_gate" != browser ] ||
      artifacts='outside-edge-refusals outside-path-reach outside-edge outside-edge-grants restricted-sweep outside-edge-t5'
    # The artifacts are gh's answer: they go into gh_kept, which outlives the leg, and from here on
    # every row prints of them closed values alone and names the file for the rest (artifact_text).
    results="$gh_kept/artifacts"
    mkdir -p "$results"
    results_gh=yes
    for artifact in $artifacts; do
      gh run download "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" -n "$artifact" -D "$results" >/dev/null 2>&1 </dev/null
    done
    gh run view "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --json conclusion --jq .conclusion >"$gh_kept/run-conclusion" 2>/dev/null </dev/null
    public_end=$(<"$gh_kept/run-conclusion")
    # A conclusion is printed only as one GitHub gives a run.
    case $public_end in
      '' | success | failure | neutral | cancelled | skipped | timed_out | action_required | stale | startup_failure) end_said=$public_end ;;
      *) end_said="no conclusion GitHub gives, kept in $gh_kept/run-conclusion" ;;
    esac
    say "run $run_id: ${end_said:-no conclusion read}"
  fi
elif [ -z "$public_problem" ]; then
  say "waiting for the public leg's command"
  await_child "$public_pid" 3600
  if [ -z "$child_exit" ]; then
    public_problem="the public leg's command had not ended within an hour"
  else
    public_end=$child_exit
    end_said=$child_exit
  fi
fi
# The public leg has ended, so the sweep's writer has nothing more to show it.
sweep_writer_stop
public_tsv="$results/outside-agent-public.tsv"
[ "$outside_gate" = browser ] || [ -n "$public_problem" ] || [ -f "$public_tsv" ] ||
  public_problem="the public leg left no outside-agent-public.tsv"

# tsv_part FILE NAME: FILE's NAME row (its worst copy, worst_row's) as `verdict<TAB>got`, or
# nothing.
tsv_part() {
  local each rest
  each=$(worst_row "$1" "$2")
  [ -n "$each" ] || return 0
  rest=${each#*"$tab"*"$tab"}
  printf '%s\t%s' "${rest##*"$tab"}" "${rest%"$tab"*}"
}
# public_part NAME: the public TSV's NAME row, as tsv_part reads it.
public_part() { tsv_part "$public_tsv" "$1"; }

# --- T4: the scratch login is revoked on the test person's page, and the helper reads it ended. ---
want_t4="the public leg's revoke step revokes the scratch login before the T5 window (its revoke row PASS); machine list through the scratch helper is then refused LAUNCHER_INVALID"
if [ "$outside_gate" = browser ]; then
  :
elif ! passed T1; then
  row T4 "$want_t4" "not run: T1 did not pass, so there is no scratch login" UNKNOWN
elif [ ! -f "$results/outside-edge-revocations.tsv" ]; then
  row T4 "$want_t4" "no outside-edge-revocations.tsv from the public leg${public_problem:+: $public_problem}" UNKNOWN
else
  # The revoke row is the proof of the revoke, its worst copy read: a retried revoke's FAIL then
  # PASS reads FAIL. machine list's LAUNCHER_INVALID after it is only the helper's launcher
  # refused, which an expired or skewed proof answers too, so it proves nothing of the revoke alone.
  revoked=$(worst_row "$results/outside-edge-revocations.tsv" "revoke-$credential")
  [ -z "$revoked" ] || revoked=${revoked##*"$tab"}
  ended=$(env -i "${scratch_env[@]}" agent-secrets machine list 2>&1 </dev/null)
  ended_exit=$?
  if [ -z "$revoked" ]; then
    row T4 "$want_t4" "the revocations hold no row for credential $credential" FAIL
  elif [ "$revoked" != PASS ]; then
    case $revoked in
      FAIL | UNKNOWN) row T4 "$want_t4" "revoke-$credential reads $revoked" "$revoked" ;;
      *) row T4 "$want_t4" "revoke-$credential reads $(artifact_text "$results/outside-edge-revocations.tsv" "$(cell "$revoked")"), which is no verdict" UNKNOWN ;;
    esac
  elif [ "$ended_exit" -eq 0 ]; then
    row T4 "$want_t4" "revoked, and machine list through the scratch helper still answers" FAIL
  else
    case $ended in
      *LAUNCHER_INVALID*) row T4 "$want_t4" "revoke-$credential reads PASS; machine list through the scratch helper is then refused LAUNCHER_INVALID" PASS ;;
      *) row T4 "$want_t4" "revoked; machine list exited $ended_exit: ${ended:0:300}" UNKNOWN ;;
    esac
  fi
fi

# --- T5, judged from the public leg's three T5 rows against the removal's two clocks. The removal
# was made at some time between removal_from and removal_to. A part must have stopped no earlier
# than removal_from, and within its bound of it to pass: 325 s (a token lives at most 300 s, and
# the edge checks an open socket every 20 s plus a 5 s cache), the cookie 30 s. A part that stopped
# more than its bound after removal_to fails. One that stopped before removal_from stopped for some
# other reason, and one between those bounds may or may not be within its bound of the removal:
# each reads UNKNOWN. The socket part counts only for a socket opened before removal_from, since a
# socket opened later may never have held a token from before the removal. The browser gate judges
# the cookie part (30 s) and the passkey part (325 s) alone, from its window step's artifact
# outside-edge-t5: no device login and no socket of the person's exist on its path. ---
want_t5="once the owner removes ${person:-<OUTSIDE_PERSON>} from ${project:-<project>}'s access list: the person's cookie gets 401 within 30 s, their passkey sign-in is refused, a fresh trade of their live device login is refused and no later trade answered, and the relay socket open then, opened before the removal, closes 4401 or 4403, each within 325 s"
# The bound of a trade's refusal, of the socket's close and of the passkey sign-in (above).
trade_bound=325
t5_tsv=$public_tsv
t5_source='the public TSV'
t5_parts="T5-agent:refused_at:$trade_bound T5-agent:socket_closed_at:$trade_bound T5-cookie:at:30 T5-passkey:at:$trade_bound"
t5_readd="Re-adding ${person:-<OUTSIDE_PERSON>} is the owner's own act"
if [ "$outside_gate" = browser ]; then
  want_t5="once the owner's remove command takes ${person:-<OUTSIDE_PERSON>} off ${project:-<project>}'s access list while the workflow's step '$window_step' runs: the person's cookie gets 401 within 30 s, and their passkey sign-in is refused within $trade_bound s"
  t5_tsv="$results/outside-edge-t5.tsv"
  t5_source='outside-edge-t5.tsv'
  t5_parts="T5-cookie:at:30 T5-passkey:at:$trade_bound"
  t5_readd="The next run's owner add puts ${person:-<OUTSIDE_PERSON>} back on the list"
fi
if [ -n "$t5_problem" ]; then
  row T5 "$want_t5" "$t5_problem" UNKNOWN
else
  after=$(listed_now)
  lags=''
  wrong=''
  unread=''
  # A trade answered after the removal's bound fails T5 even where no trade was refused: the
  # public leg keeps the time of the last answered trade (last_answered_at), and a removal the list
  # confirms stops every trade within trade_bound s of its second clock at the latest.
  agent_got=''
  [ "$outside_gate" = browser ] || agent_got=$(public_part T5-agent)
  agent_got=${agent_got#*"$tab"}
  last_answered=$(mark last_answered_at "$agent_got")
  if [ -n "$last_answered" ] && [ "$after" = false ] && [ $((last_answered - removal_to)) -gt "$trade_bound" ]; then
    wrong="T5-agent: a trade was answered $((last_answered - removal_to)) s after the removal, past $trade_bound s"
  fi
  for part in $t5_parts; do
    name=${part%%:*}
    rest=${part#*:}
    key=${rest%%:*}
    bound=${rest#*:}
    read_row=$(tsv_part "$t5_tsv" "$name")
    verdict=${read_row%%"$tab"*}
    got=${read_row#*"$tab"}
    at=$(mark "$key" "$got")
    opened_at=$(mark socket_opened_at "$got")
    if [ -z "$read_row" ]; then
      unread="${unread:+$unread; }$t5_source has no $name row"
    elif [ "$verdict" = FAIL ]; then
      wrong="${wrong:+$wrong; }$name: $(artifact_text "$t5_tsv" "$got")"
    elif [ "$verdict" != PASS ] || [ -z "$at" ]; then
      unread="${unread:+$unread; }$name ($key) not read: $(artifact_text "$t5_tsv" "${got:0:200}")"
    elif [ "$key" = socket_closed_at ] && { [ -z "$opened_at" ] || [ "$opened_at" -ge "$removal_from" ]; }; then
      unread="${unread:+$unread; }$name: the socket open at the refusal opened at ${opened_at:-no time read}, not before the removal began at $removal_from, so its close says nothing of the removal"
    elif [ "$at" -lt "$removal_from" ]; then
      unread="${unread:+$unread; }$name ($key) stopped $((removal_from - at)) s before the removal began, for another reason"
    elif [ $((at - removal_from)) -le "$bound" ]; then
      lags="${lags:+$lags, }$name $key at most $((at - removal_from)) s"
    elif [ $((at - removal_to)) -gt "$bound" ]; then
      wrong="${wrong:+$wrong; }$name ($key) $((at - removal_to)) s after the removal, past $bound s"
    else
      unread="${unread:+$unread; }$name ($key) $((at - removal_to)) to $((at - removal_from)) s after the removal, which was made somewhere in those $((removal_to - removal_from)) s, so whether it is within $bound s is unread"
    fi
  done
  if [ "$after" != false ]; then
    row T5 "$want_t5" "after the run the access list reads $after, so the removal was not made" UNKNOWN
  elif [ -n "$wrong" ]; then
    row T5 "$want_t5" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row T5 "$want_t5" "$unread${lags:+; read: $lags}" UNKNOWN
  else
    row T5 "$want_t5" "after the removal: $lags. $t5_readd" PASS
  fi
fi

# --- P1-P10 as the public leg's TSV has them, each held to what this leg saw beside it, and C0-C7
# as the TSV has them: the Codex leg's controls ran beside it on the runner. ---
for name in $public_rows; do
  line=''
  [ -n "$public_problem" ] || line=$(worst_row "$public_tsv" "$name")
  if [ -z "$line" ]; then
    row "$name" "the public leg's row $name" "${public_problem:-the public TSV has no $name row}" UNKNOWN
    continue
  fi
  IFS="$tab" read -r _ want got verdict <<EOF
$line
EOF
  # The row's want and got are the public leg's words, read for its decisions below as they are;
  # where gh downloaded them, the row prints in their place its own words, naming the TSV.
  want_said=$want
  [ -z "$results_gh" ] || want_said="the public leg's row $name"
  got_said=$(artifact_text "$public_tsv" "$got")
  case $verdict in PASS | FAIL | UNKNOWN) ;; *)
    row "$name" "$want_said" "the public TSV's verdict is $(artifact_text "$public_tsv" "$verdict: $got")" UNKNOWN
    continue
    ;;
  esac
  # A result this leg's own setup cannot stand behind reads UNKNOWN. P4 depends on the peer
  # answering, so any verdict of it without the peer is unproven. A PASS of P5, P6 or P7 rests on a
  # control this leg made: P5's admission on the issue naming the repository, P6's absences on
  # the peer and on a session outside the project that a private listener lists and the edge
  # refuses, P7's refusals on the full member's browser being answered 201 and 200 on the same
  # routes. A FAIL of any of them is evidence by itself (a refusal missing, a foreign session
  # listed) and stands, but for P6's parts about <peer>-foreign: its listing and the send to it
  # (marks foreign_listed and foreign_send) are no evidence where the edge admits that session's
  # token (foreign_in_project), since that session is then in the project. A P6 FAIL whose relay
  # sub (mark sub) was granted or refused with another code stands whatever the foreign session
  # did.
  held_by=''
  case $name:$verdict in
    P4:*) held_by=$peer_problem ;;
    P5:PASS) passed issue || held_by="the issue row did not pass, so no repository is known to resolve to $project" ;;
    P6:PASS) held_by=$peer_problem${foreign_problem:+${peer_problem:+; }$foreign_problem} ;;
    # C4's control is a send to <peer>-foreign answered 404, which says something only where that
    # session exists outside the project: the session P6's absences rest on (foreign_start).
    C4:PASS) [ -z "$foreign_problem" ] || held_by="its control's session outside the project: $foreign_problem" ;;
    P6:FAIL)
      case $(mark_word sub "$got") in
        refused | unread) [ -z "$foreign_in_project" ] || held_by=$foreign_in_project ;;
      esac
      ;;
    P7:PASS) held_by=${p7_control_problem:+no positive control: $p7_control_problem} ;;
  esac
  if [ -n "$held_by" ] && [ "$verdict" != UNKNOWN ]; then
    row "$name" "$want_said" "$verdict as the public leg read it, unproven here: $held_by; $got_said" UNKNOWN
  elif [ "$name:$verdict" = P4:PASS ]; then
    # The public leg read the peer's answers on its side; the peer's side is read here: its ping
    # and the reply must each be shown to the peer within 10 s of the public side's stamp.
    exchange=${got##*nonce=}
    exchange=${exchange%% *}
    # The nonce is the public leg's od hex (or its pid): from gh's download, printed only as that.
    exchange_said=$exchange
    case $results_gh:$exchange in
      yes:*[!0-9a-f]* | yes:) exchange_said=$(artifact_text "$public_tsv" "$exchange") ;;
    esac
    lags=''
    late=''
    ahead=''
    unseen=''
    for word in ping reply; do
      seen_line=''
      while IFS= read -r each; do
        case $each in "$word $exchange "*) seen_line=$each ;; esac
      done <"$peer_seen"
      if [ -z "$seen_line" ]; then
        unseen="${unseen:+$unseen, }$word"
        continue
      fi
      set -f
      # shellcheck disable=SC2086 # the four fields of one line of peer_seen, split on purpose
      set -- $seen_line
      set +f
      lag=$((($4 - $3) / 1000))
      lags="${lags:+$lags, }$word $lag s"
      [ "$lag" -le 10 ] || late="${late:+$late, }$word $lag s"
      # Shown before it was sent, by more than the 2 s the whole-second clocks round off: the
      # runner's clock runs ahead of this box's, so a lag within 10 s says nothing.
      [ "$lag" -ge -2 ] || ahead="${ahead:+$ahead, }$word $lag s"
    done
    if [ -n "$late" ]; then
      row P4 "$want_said" "the $peer_kind peer was shown the public side's $late after it was sent; $got_said" FAIL
    elif [ -n "$unseen" ]; then
      row P4 "$want_said" "PASS as the public leg read it, unproven here: the $peer_kind peer's transcript never held its $unseen of nonce $exchange_said; $got_said" UNKNOWN
    elif [ -n "$ahead" ]; then
      row P4 "$want_said" "PASS as the public leg read it, unproven here: the $peer_kind peer was shown the public side's $ahead, before it was sent, so the runner's clock and this box's disagree; $got_said" UNKNOWN
    elif [ "$peer_kind" = probe ]; then
      row P4 "$want_said" "$got_said; the scripted control peer was shown the $lags after they were sent: the path passes with no model in the loop, and the gate needs the model peer (OUTSIDE_PEER=omp)" UNKNOWN
    else
      row P4 "$want_said" "$got_said; the $peer_kind peer was shown the $lags after they were sent" PASS
    fi
  elif [ "$name:$verdict" = P7:PASS ]; then
    row P7 "$want_said" "$got_said; the positive control: $p7_control" PASS
  else
    row "$name" "$want_said" "$got_said" "$verdict"
  fi
done

# --- The browser gate's rows of the workflow's own checks, in place of P1-P10, from the run's
# artifacts. refusals and reach are each check's exit, its `.code`, the exit the run's Verdict step
# reads (0 PASS, 1 FAIL, 2 UNKNOWN; none, or another word, UNKNOWN), beside the last line the check
# printed; sign-in, projects and limits are the driver's own rows (outside-edge.tsv), each read
# through worst_row, limits holding every `limit-*` row the driver wrote, at least one. ---
# check_row ROW NAME WANT: row ROW from check NAME's NAME.code and NAME.txt.
check_row() {
  local code last='' line
  if [ -n "$public_problem" ]; then
    row "$1" "$3" "$public_problem" UNKNOWN
    return
  fi
  [ ! -f "$results/$2.code" ] || code=$(tr -d ' \r\n' <"$results/$2.code")
  if [ -f "$results/$2.txt" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      [ -z "$line" ] || last=$line
    done <"$results/$2.txt"
  fi
  # The check's last line is the run's, which gh downloaded: printed through artifact_text alone.
  [ ! -f "$results/$2.txt" ] || last=$(artifact_text "$results/$2.txt" "${last:0:300}")
  case ${code:-} in
    0) row "$1" "$3" "$2 exited 0: $last" PASS ;;
    1) row "$1" "$3" "$2 exited 1: $last" FAIL ;;
    2) row "$1" "$3" "$2 exited 2: $last" UNKNOWN ;;
    '') row "$1" "$3" "the run left no $2.code, so its exit is not read: $last" UNKNOWN ;;
    *) row "$1" "$3" "$2.code holds $(artifact_text "$results/$2.code" "$(cell "${code:0:40}")"), which is no exit of 0, 1 or 2: $last" UNKNOWN ;;
  esac
}
# driver_row ROW WANT: the driver's row ROW from outside-edge.tsv.
driver_row() {
  local read_row verdict got
  if [ -n "$public_problem" ]; then
    row "$1" "$2" "$public_problem" UNKNOWN
    return
  fi
  read_row=$(tsv_part "$results/outside-edge.tsv" "$1")
  verdict=${read_row%%"$tab"*}
  got=${read_row#*"$tab"}
  if [ ! -f "$results/outside-edge.tsv" ]; then
    row "$1" "$2" "the run left no outside-edge.tsv" UNKNOWN
  elif [ -z "$read_row" ]; then
    row "$1" "$2" "outside-edge.tsv holds no $1 row" UNKNOWN
  else
    # The driver's words are the run's, which gh downloaded: printed through artifact_text alone.
    case $verdict in
      PASS | FAIL | UNKNOWN) row "$1" "$2" "$(artifact_text "$results/outside-edge.tsv" "$got")" "$verdict" ;;
      *) row "$1" "$2" "the driver's verdict is $(artifact_text "$results/outside-edge.tsv" "$(cell "$verdict"): $got")" UNKNOWN ;;
    esac
  fi
}
# limits_row WANT: the limits row from every `limit-*` row of outside-edge.tsv (and its `limits`
# row, the one it writes when it counted no request).
limits_row() {
  local tsv="$results/outside-edge.tsv" names='' each rest read_row verdict got failed='' unread='' read_ok='' each_said
  if [ -n "$public_problem" ]; then
    row limits "$1" "$public_problem" UNKNOWN
    return
  fi
  if [ ! -f "$tsv" ]; then
    row limits "$1" "the run left no outside-edge.tsv" UNKNOWN
    return
  fi
  while IFS="$tab" read -r each rest || [ -n "$each" ]; do
    case $each in
      limit-* | limits) case " $names " in *" $each "*) ;; *) names="$names $each" ;; esac ;;
    esac
  done <"$tsv"
  for each in $names; do
    read_row=$(tsv_part "$tsv" "$each")
    verdict=${read_row%%"$tab"*}
    got=${read_row#*"$tab"}
    # The rows are the run's, which gh downloaded: a row's name is printed only as limits or
    # limit-<count>-<seconds>s, as the driver names them, and its words through artifact_text.
    each_said=$each
    case $results_gh:$each in
      yes:limits) ;;
      yes:*) [[ $each =~ ^limit-[0-9]+-[0-9]+s$ ]] || each_said='a limit row' ;;
    esac
    got=$(artifact_text "$tsv" "$got")
    case $verdict in
      PASS) read_ok="${read_ok:+$read_ok; }$each_said: $got" ;;
      FAIL) failed="${failed:+$failed; }$each_said: $got" ;;
      *) unread="${unread:+$unread; }$each_said $(artifact_word "$tsv" "$(cell "$verdict")" UNKNOWN ''): $got" ;;
    esac
  done
  if [ -n "$failed" ]; then
    row limits "$1" "$failed${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row limits "$1" "$unread${read_ok:+; read: $read_ok}" UNKNOWN
  elif [ -z "$read_ok" ]; then
    row limits "$1" "outside-edge.tsv holds no limit row" UNKNOWN
  else
    row limits "$1" "$read_ok" PASS
  fi
}
# grants_row: row grants, from the driver's rows grants-list and grants-revoke (outside-edge-grants.tsv)
# and the full member's two reads. A FAIL of either driver row is evidence by itself and stands
# without the control; a PASS needs both rows PASS, a probe grant given, and at least one grant
# every signed-in person is listed in both reads.
grants_row() {
  local tsv="$results/outside-edge-grants.tsv" list_read revoke_read list_verdict revoke_verdict
  local want got control after_problem after='' before_count=0 after_count=0 both=0 id
  want="the test person's own credential-grants list holds no grant of another session (the driver's grants-list PASS), and their revoke of a grant no person may end, picked from a full member's list, is refused by Dispatch itself, 403 RESTRICTED_MEMBER (grants-revoke PASS); the control: a grant every signed-in person is listed, in the full member's list both before and after the run"
  if [ -n "$public_problem" ]; then
    row grants "$want" "$public_problem" UNKNOWN
    return
  fi
  list_read=$(tsv_part "$tsv" grants-list)
  revoke_read=$(tsv_part "$tsv" grants-revoke)
  list_verdict=${list_read%%"$tab"*}
  revoke_verdict=${revoke_read%%"$tab"*}
  # The driver's rows are the run's, which gh downloaded: their verdicts are printed only as
  # verdicts, and their words through artifact_text.
  got="grants-list $(artifact_word "$tsv" "${list_verdict:-absent}" PASS FAIL UNKNOWN absent)${list_read:+: $(artifact_text "$tsv" "${list_read#*"$tab"}")}; grants-revoke $(artifact_word "$tsv" "${revoke_verdict:-absent}" PASS FAIL UNKNOWN absent)${revoke_read:+: $(artifact_text "$tsv" "${revoke_read#*"$tab"}")}"
  [ -f "$tsv" ] || got="the run left no outside-edge-grants.tsv"
  if [ -n "$grants_problem" ]; then
    control="no control: $grants_problem"
  else
    after_problem=$(grants_read after)
    [ -n "$after_problem" ] || after=$(everyone_grants "$work/grants-after.rows")
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      before_count=$((before_count + 1))
      case "$newline$after$newline" in *"$newline$id$newline"*) both=$((both + 1)) ;; esac
    done <<EOF
$grants_before
EOF
    while IFS= read -r id; do [ -z "$id" ] || after_count=$((after_count + 1)); done <<EOF
$after
EOF
    control="the full member's list held $before_count grants every signed-in person is listed before the run and $after_count after, $both in both"
    [ -z "$after_problem" ] || control="$control; $after_problem"
    [ -n "$grants_probe" ] || control="$control; no grant of the list before the run had no approver and no operator, so no probe grant was given"
  fi
  case " $list_verdict $revoke_verdict " in
    *' FAIL '*) row grants "$want" "$got; $control" FAIL ;;
    ' PASS PASS ')
      if [ "$both" -gt 0 ] && [ -n "$grants_probe" ]; then
        row grants "$want" "$got; $control" PASS
      else
        row grants "$want" "$got; $control, so an empty list proves nothing" UNKNOWN
      fi
      ;;
    *) row grants "$want" "$got; $control" UNKNOWN ;;
  esac
}
if [ "$outside_gate" = browser ]; then
  check_row refusals outside-edge-refusals "the workflow's HTTPS refusal check (outside-edge-refusals.sh), with dispatch-healthz its one routed control, exits 0: that control shows its marker, and every refusal line is answered the edge's own 404"
  check_row reach outside-path-reach "the workflow's TCP reach check (outside-path-reach.sh) exits 0: no internal address, and no port of the edge address but 443, answers the runner"
  driver_row sign-in "the driver's row sign-in PASS: the owner's link enrolls one passkey and signs ${person:-<OUTSIDE_PERSON>} in at the edge's Dispatch origin, and GET /auth/whoami names them"
  driver_row projects "the driver's row projects PASS: GET /api/v1/projects lists exactly [$project]"
  limits_row "every limit row of the driver PASS, at least one: the run's peak stays under half of each rule of the edge's Dispatch origin"
  grants_row
fi

# --- public: the public leg's own last word. Each public row is held above to what this leg saw
# beside it; the public leg's verdict row, its revocations row and its end hold the gate too, so a
# public leg that failed, or left no verdict, never lets the gate pass. A run concludes `failure`
# for an exit 1 and an exit 2 alike, so its verdict row says which. ---
want_public="the public leg's verdict row PASS, its revocations row PASS, and its end: exit 0, or the run's conclusion success"
[ "$outside_gate" != browser ] ||
  want_public="the workflow run's conclusion success: its Verdict step read every check it ran PASS, and no output naming the runner or holding the link"
if [ -n "$public_problem" ]; then
  row public "$want_public" "$public_problem" UNKNOWN
elif [ "$outside_gate" = browser ]; then
  # A run concludes `failure` for a check's FAIL and its UNKNOWN alike, so the exits say which.
  exits=''
  failed=''
  for each in outside-edge-refusals.code outside-path-reach.code outside-edge.code outside-edge-grants.code restricted-sweep.exit outside-edge-t5.code; do
    read_exit=none
    [ ! -f "$results/$each" ] || read_exit=$(tr -d ' \r\n' <"$results/$each")
    # An exit file is the run's, which gh downloaded: printed only as an exit the leg reads.
    exits="${exits:+$exits, }${each%.*} $(artifact_word "$results/$each" "$(cell "${read_exit:0:40}")" 0 1 2 none)"
    [ "$read_exit" != 1 ] || failed=yes
  done
  read_public="conclusion ${end_said:-not read}; exits: $exits"
  case $public_end:$failed in
    success:*) row public "$want_public" "$read_public" PASS ;;
    failure:yes) row public "$want_public" "$read_public" FAIL ;;
    *) row public "$want_public" "$read_public" UNKNOWN ;;
  esac
else
  verdict_read=$(public_part verdict)
  revoke_read=$(public_part revocations)
  public_verdict=${verdict_read%%"$tab"*}
  revoke_verdict=${revoke_read%%"$tab"*}
  case $public_end in
    0 | success) end_verdict=PASS ;;
    1) end_verdict=FAIL ;;
    failure)
      end_verdict=UNKNOWN
      [ "$public_verdict" != FAIL ] || end_verdict=FAIL
      ;;
    *) end_verdict=UNKNOWN ;;
  esac
  # The verdict and revocations rows are the public leg's: where gh downloaded them, their verdicts
  # are printed only as verdicts and their words through artifact_text.
  read_public="verdict row $(artifact_word "$public_tsv" "${public_verdict:-absent}" PASS FAIL UNKNOWN absent)${verdict_read:+ ($(artifact_text "$public_tsv" "${verdict_read#*"$tab"}"))}, revocations row $(artifact_word "$public_tsv" "${revoke_verdict:-absent}" PASS FAIL UNKNOWN absent), end ${end_said:-not read}"
  case " $public_verdict $revoke_verdict $end_verdict " in
    *' FAIL '*) row public "$want_public" "$read_public" FAIL ;;
    ' PASS PASS PASS ') row public "$want_public" "$read_public" PASS ;;
    *) row public "$want_public" "$read_public" UNKNOWN ;;
  esac
fi

# --- sweep (outside-agent-sweep.sh). ---
sweep_row

# --- pins-end: the builds the legs ran against did not move between them. The pins are read again
# from the deployment repository's main, and Dispatch's /healthz again; a pin that moved, or a
# Dispatch that serves another build than at the start, holds the gate. ---
want_pins_end="the pins read again at the end, from ${OUTSIDE_PINS_REPO:-<OUTSIDE_PINS_REPO>}'s main, are those read at the start, and Dispatch serves the build it served at the start"
if [ -z "${OUTSIDE_PINS_REPO:-}" ]; then
  row pins-end "$want_pins_end" "OUTSIDE_PINS_REPO is unset: the pins are local files, so whether the deployment repository's main moved during the run is not read" UNKNOWN
else
  read_pins
  read_serving
  if [ -n "$pins_problem" ] || [ -n "$serving_problem" ]; then
    row pins-end "$want_pins_end" "at the end: $pins_problem${serving_problem:+${pins_problem:+; }$serving_problem}" UNKNOWN
  elif [ "$pins_text" != "$pins_start" ]; then
    row pins-end "$want_pins_end" "a pin moved between the legs: at the start $(printf '%s' "$pins_start_said" | tr '\n' ';'), at the end $(printf '%s' "$pins_said" | tr '\n' ';')" UNKNOWN
  elif [ "$serving_commit" != "$serving_start" ]; then
    row pins-end "$want_pins_end" "Dispatch served $serving_start at the start and serves $serving_commit (fleet ${serving_fleet:-none}) at the end" UNKNOWN
  else
    row pins-end "$want_pins_end" "the same pins at the end; Dispatch still serves $serving_commit on every process" PASS
  fi
fi

gate
