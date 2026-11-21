#!/usr/bin/env bash
# The tailnet leg of the outside agent's acceptance, and the one command that runs both legs: a
# developer box on the private network runs it, with its project's key as the one argument. It
# reads the deployed pins and refuses to start unless the secrets broker's build holds the
# enrollment refusal and Dispatch's build holds the changes the check rests on; makes the scratch
# project's issue the public leg's GitHub row reads; starts a scratch agent-secrets-helper whose
# machine login the test person approves from the public leg's browser; starts our peer session;
# starts the public leg (outside-agent-public.sh, rows P1-P9) on a runner on the public internet;
# runs rows T1-T4 against the broker's private origin, the one place its enrollment route is
# served; reads, with a full member's browser session, the positive controls rows P7 and sweep are
# held to; reads the public leg's TSV; asks the operator, on this terminal, to remove the test
# person from the project's access list and reads that the test person's live device login stops
# (T5); and prints the gate. outside-agent-acceptance.md, beside this file, says what each row
# reads and what the gate holds.
#
# Inputs (environment), each named by the row that cannot run without it:
#   $1                           the scratch project's key.
#   OUTSIDE_PINS                 the deployed pins, one `name file key` line each for `broker`,
#                                `dispatch` and `edge-listener`: the configuration file and the
#                                key whose value is that deployment's image; `#` starts a
#                                comment. The files are the deployment repository's, at its main.
#   OUTSIDE_ENROLL_REFUSAL_COMMIT  the merge commit of the enrollment refusal (a restricted
#                                member's machine login enrolls nothing), which the broker's
#                                build must hold.
#   OUTSIDE_DISPATCH_COMMITS     the merge commits, blank-separated, that Dispatch's build must
#                                hold: the change that gives no broker token the approve scope,
#                                and the change that refuses a restricted member an issue's
#                                repository.
#   OUTSIDE_TAILNET_BROKER_URL   the broker's origin on the private network.
#   OUTSIDE_TAILNET_DISPATCH_URL  Dispatch's origin on the private network, where T5 reads the
#                                project's access list with this box's own credential, and the
#                                full member's browser session reads the positive controls.
#   OUTSIDE_LISTENER_URL         the edge listener, where the peer session registers.
#   OUTSIDE_INTERNAL_LISTENER_URL  a listener on the private network, where a session outside the
#                                project (<peer>-foreign) registers for P6's control.
#   OUTSIDE_PERSON               the test person's address.
#   OUTSIDE_CANARY               the name of the canary secret T2 and T3 ask for.
#   OUTSIDE_REPOSITORY           owner/name of the repository the scratch project's issue names.
#   OUTSIDE_SWEEP_PROJECT, OUTSIDE_SWEEP_FULL_TOKEN_FILE, OUTSIDE_SWEEP_CONVERSATION_FILE,
#   OUTSIDE_SWEEP_WRITE_ISSUE    the read sweep's full-member half (restricted-sweep.sh, row
#                                sweep): the project the test person must not see, a file holding a
#                                full member's bearer for Dispatch's private origin, the
#                                conversation file of a session that worked in that project alone,
#                                and a scratch issue of it the writer comments on every 15 s while
#                                the public leg runs.
#   OUTSIDE_FULL_COOKIE_FILE     a file holding a full member's browser session at Dispatch's
#                                private origin, `<name>=<value>` as the Cookie header carries it.
#                                The positive controls of P7 and of the sweep's GitHub proxy rows
#                                read with it, and the sweep's fills mode reads the proxy with it
#                                (as SWEEP_FULL_COOKIE), so rows P7 and sweep need it.
#   OUTSIDE_WORKFLOW_REPO        owner/name of the repository whose workflow runs the public leg;
#                                OUTSIDE_WORKFLOW names the workflow (outside-edge-e2e.yaml), and
#                                OUTSIDE_WORKFLOW_INPUTS holds its other inputs, one name=value a
#                                line. Or OUTSIDE_PUBLIC_COMMAND: a command line, split on blanks,
#                                that runs the public leg here instead, for a local stack; it gets
#                                OUTSIDE_TAILNET_USER_CODE,
#                                OUTSIDE_PEER_SESSION and OUTSIDE_PUBLIC_RESULTS in its environment
#                                and writes outside-agent-public.tsv and
#                                outside-edge-revocations.tsv there.
# Optional: OUTSIDE_PEER, P4's peer: `omp` (the default), an Oh My Pi session of this box whose
# model answers, run with the box's own model profile and only the envoy plugin
# OUTSIDE_PEER_PLUGIN_DIR names (default this checkout's packages/pi-envoy, built); or `probe`, a
# scripted peer for a local stack, only with OUTSIDE_ALLOW_HTTP=1, named OUTSIDE_PEER_SESSION
# (default outside-e2e-peer-<random>). OUTSIDE_OWNER_REMOVE_COMMAND, a command line that removes
# the test person from the project's access list as its owner, run with the person and the
# project as its last two words, in place of T5's prompt (a harness that acts as the owner's
# browser). OUTSIDE_IMAGE_REVISION_COMMAND,
# a command line that prints the commit an image was built from, run with the image as its last
# word (default: `crane config`, read for org.opencontainers.image.revision); OUTSIDE_TAILNET_TSV,
# a file that gets a copy of the TSV; OUTSIDE_RESULT_ISSUE, an issue the result is posted to as one
# comment with `dispatch comment`; OUTSIDE_LEGION_CHECKOUT, the git checkout the pins row reads
# commits in (default this file's own); OUTSIDE_ALLOW_HTTP=1, which lets http://127.0.0.1 origins
# through for a local stack.
#
# T3, the positive control, and the issue run as this box's own: its `agent-secrets` helper and
# its `dispatch` command, as the caller's environment has them. The scratch helper of T1, T2 and
# T4, and the one of P7's control, each run with HOME, XDG_RUNTIME_DIR, XDG_STATE_HOME and every
# AGENT_SECRETS_* path of its own, in one `mktemp -d` directory, so neither shares a key, socket or
# record with the box's.
#
# Output: on stdout a TSV, the header `row want got verdict`, the rows pins, issue, T1-T5, the
# public leg's P1-P9 as its TSV has them, sweep, and the gate. Progress goes to stderr. A row it
# could not run reads UNKNOWN naming what it could not read, never PASS. Exit 0 when the gate
# passed, 1 when a row failed, 2 otherwise. It prints no secret value: T2 and T3 read the canary's
# length alone, and the full member's cookie reaches curl in a header file alone.
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
tab=$(printf '\t')
newline='
'

tsv=${OUTSIDE_TAILNET_TSV:-}
if [ -n "$tsv" ]; then
  : >"$tsv" || tsv=''
fi
emit() {
  printf '%s\n' "$1"
  [ -z "$tsv" ] || printf '%s\n' "$1" >>"$tsv"
  [ -z "${work:-}" ] || printf '%s\n' "$1" >>"$work/result.tsv"
}
# text_of FILE: FILE's first 300 characters, on one line.
text_of() {
  local text
  text=$(tr '\n' ' ' <"$1" 2>/dev/null)
  printf '%s' "${text:0:300}"
}
cell() {
  local text=$1
  text=${text//$tab/ }
  text=${text//$newline/ }
  text=${text//$'\r'/ }
  printf '%s' "$text"
}
fails=0
unknowns=0
row() {
  emit "$1$tab$(cell "$2")$tab$(cell "$3")$tab$4"
  printf -v "verdict_$1" '%s' "$4"
  case $4 in
    FAIL) fails=$((fails + 1)) ;;
    UNKNOWN) unknowns=$((unknowns + 1)) ;;
    PASS) ;;
  esac
}
passed() {
  local name="verdict_$1"
  [ "${!name:-}" = PASS ]
}
say() { printf 'outside-agent-tailnet: %s\n' "$1" >&2; }
now() { date -u '+%s'; }
json_field() {
  local rest=${2#*\""$1"\":}
  [ "$rest" != "$2" ] || return 1
  case $rest in
    \"*)
      rest=${rest#\"}
      printf '%s' "${rest%%\"*}"
      ;;
    *) printf '%s' "${rest%%[,\}]*}" ;;
  esac
}
tool_problem() {
  local name out=''
  for name in "$@"; do
    command -v "$name" >/dev/null 2>&1 || out="${out:+$out, }$name"
  done
  [ -z "$out" ] || printf '%s not on PATH' "$out"
}
allow_http=${OUTSIDE_ALLOW_HTTP:-}
origin_problem() {
  local value
  eval "value=\${$1:-}"
  case $value in
    '') printf '%s is unset' "$1" ;;
    https://*/* | http://*/*) printf '%s must be an origin alone, with no path: %s' "$1" "$value" ;;
    https://?*) ;;
    http://127.0.0.1:* | http://localhost:*)
      [ "$allow_http" = 1 ] || printf '%s may be http only with OUTSIDE_ALLOW_HTTP=1: %s' "$1" "$value"
      ;;
    *) printf '%s must be an https origin: %s' "$1" "$value" ;;
  esac
}
lacking() {
  local name value problem out=''
  for name in "$@"; do
    case $name in
      *_URL) problem=$(origin_problem "$name") ;;
      *)
        eval "value=\${$name:-}"
        problem=''
        [ -n "$value" ] || problem="$name is unset"
        ;;
    esac
    [ -z "$problem" ] || out="${out:+$out; }$problem"
  done
  printf '%s' "$out"
}

public_rows='P1 P2 P3 P4 P5 P6 P7 P8 P9'
project=${1:-}
person=${OUTSIDE_PERSON:-}
canary=${OUTSIDE_CANARY:-}
repository=${OUTSIDE_REPOSITORY:-}
commit=${OUTSIDE_ENROLL_REFUSAL_COMMIT:-}
nonce=$(od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
[ -n "$nonce" ] || nonce=$$
peer=${OUTSIDE_PEER_SESSION:-outside-e2e-peer-$nonce}

# --- Scratch directory and cleanup: the helper, the peer, the public command, by their pids. ---
work=''
background=''
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  local pid
  for pid in $background; do kill "$pid" 2>/dev/null; done
  [ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
work=$(mktemp -d "${TMPDIR:-/tmp}/outside-agent-tailnet.XXXXXX") || work=''

# real_tools DIR NAME...: a link in DIR to the real program behind each NAME. A version manager's
# shim (mise's, or a wrapper that calls mise) finds its program through the caller's HOME, and the
# children this leg starts under a HOME of their own have none, so they run the real programs.
real_tools() {
  local dir=$1 name found
  shift
  mkdir -p "$dir" || return
  for name in "$@"; do
    found=''
    if command -v mise >/dev/null 2>&1; then found=$(mise which "$name" 2>/dev/null </dev/null); fi
    [ -n "$found" ] || found=$(command -v "$name" 2>/dev/null)
    case $found in /*) ln -sf "$found" "$dir/$name" ;; esac
  done
}
tools="$work/tools"
[ -z "$work" ] || real_tools "$tools" bun omp tmux agent-secrets agent-secrets-helper

emit "row${tab}want${tab}got${tab}verdict"

# skip_rest REASON FROM: every row from FROM on (the names in `rest_rows`) UNKNOWN for REASON.
rest_rows="pins issue T1 T2 T3 T4 T5 $public_rows sweep"
skip_from() {
  local reason=$1 from=$2 name on=''
  for name in $rest_rows; do
    [ "$name" = "$from" ] && on=yes
    [ -n "$on" ] || continue
    row "$name" "the row as outside-agent-acceptance.md states it" "not run: $reason" UNKNOWN
  done
}
# gate: the last row, and the exit.
gate() {
  local verdict=PASS
  [ "$unknowns" -eq 0 ] || verdict=UNKNOWN
  [ "$fails" -eq 0 ] || verdict=FAIL
  [ -z "${pins_moved:-}" ] || { [ "$verdict" = FAIL ] || verdict=UNKNOWN; }
  emit "gate${tab}every row PASS, against pins that did not move between the legs${tab}$(cell "$fails FAIL, $unknowns UNKNOWN${pins_moved:+; $pins_moved}")${tab}$verdict"
  if [ -n "${OUTSIDE_RESULT_ISSUE:-}" ] && [ -n "$work" ] && command -v dispatch >/dev/null 2>&1; then
    {
      printf 'The outside acceptance of %s, both legs, %s:\n\n```\n' "$project" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
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

# --- pins: the deployed images, and the commit each was built from. ---
# read_pins: sets pins_text, the `name image revision` lines, or pins_problem.
read_pins() {
  local line name file key value image revision found
  pins_text=''
  pins_problem=''
  for name in broker dispatch edge-listener; do
    found=''
    while IFS= read -r line || [ -n "$line" ]; do
      line=${line%%#*}
      set -f
      # shellcheck disable=SC2086 # the fields of one pin line, split on purpose
      set -- $line
      set +f
      [ "$#" -eq 3 ] && [ "$1" = "$name" ] || continue
      file=$2 key=$3 found=yes
      image=''
      if [ -r "$file" ]; then
        while IFS= read -r value || [ -n "$value" ]; do
          case $value in *"$key:"*) image=${value#*"$key:"} ;; esac
        done <"$file"
      fi
      image=${image#"${image%%[! ]*}"}
      image=${image%"${image##*[! ]}"}
      if [ -z "$image" ]; then
        pins_problem="${pins_problem:+$pins_problem; }$name: $file holds no $key"
        continue
      fi
      revision=''
      if [ -n "${OUTSIDE_IMAGE_REVISION_COMMAND:-}" ]; then
        set -f
        # shellcheck disable=SC2086 # a command line, split on blanks on purpose
        revision=$($OUTSIDE_IMAGE_REVISION_COMMAND "$image" 2>/dev/null </dev/null)
        set +f
      elif command -v crane >/dev/null 2>&1; then
        revision=$(json_field org.opencontainers.image.revision "$(crane config "$image" 2>/dev/null </dev/null)")
      fi
      case $revision in
        [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
        *)
          pins_problem="${pins_problem:+$pins_problem; }$name: no commit read for $image${OUTSIDE_IMAGE_REVISION_COMMAND:+ through OUTSIDE_IMAGE_REVISION_COMMAND}$(command -v crane >/dev/null 2>&1 || [ -n "${OUTSIDE_IMAGE_REVISION_COMMAND:-}" ] || printf ' (crane not on PATH)')"
          continue
          ;;
      esac
      pins_text="${pins_text:+$pins_text$newline}$name $image $revision"
    done <"$OUTSIDE_PINS"
    [ -n "$found" ] || pins_problem="${pins_problem:+$pins_problem; }$OUTSIDE_PINS has no $name line"
  done
}

dispatch_commits=${OUTSIDE_DISPATCH_COMMITS:-}
want_pins="the broker's pin holds ${commit:-<OUTSIDE_ENROLL_REFUSAL_COMMIT>}; Dispatch's pin holds ${dispatch_commits:-<OUTSIDE_DISPATCH_COMMITS>}; the pins printed"
problem=$(lacking OUTSIDE_PINS OUTSIDE_ENROLL_REFUSAL_COMMIT OUTSIDE_DISPATCH_COMMITS)
[ -n "$project" ] || problem="${problem:+$problem; }the project key (\$1) is not given"
[ -n "$work" ] || problem="${problem:+$problem; }mktemp -d made no scratch directory"
if [ -z "$problem" ] && [ ! -r "$OUTSIDE_PINS" ]; then
  problem="OUTSIDE_PINS names $OUTSIDE_PINS, which cannot be read"
fi
if [ -n "$problem" ]; then
  row pins "$want_pins" "$problem" UNKNOWN
  rest_rows="issue T1 T2 T3 T4 T5 $public_rows sweep"
  skip_from "the pins were not read: $problem" issue
  gate
fi
read_pins
if [ -n "$pins_problem" ]; then
  row pins "$want_pins" "$pins_problem" UNKNOWN
  rest_rows="issue T1 T2 T3 T4 T5 $public_rows sweep"
  skip_from "the pins were not read" issue
  gate
fi
pins_at_start=$pins_text
broker_revision=''
dispatch_revision=''
while IFS=' ' read -r name image revision; do
  case $name in
    broker) broker_revision=$revision ;;
    dispatch) dispatch_revision=$revision ;;
  esac
done <<EOF
$pins_text
EOF
checkout=${OUTSIDE_LEGION_CHECKOUT:-$here}
# holds BUILD COMMIT: whether the build made from commit BUILD holds COMMIT: yes, no, or what could
# not be read.
holds() {
  local sha
  for sha in "$2" "$1"; do
    git -C "$checkout" cat-file -e "$sha^{commit}" 2>/dev/null ||
      git -C "$checkout" fetch -q origin "$sha" >/dev/null 2>&1 </dev/null
  done
  git -C "$checkout" merge-base --is-ancestor "$2" "$1" 2>"$work/merge-base.err"
  case $? in
    0) printf yes ;;
    1) printf no ;;
    *) printf 'git merge-base --is-ancestor %s %s: %s' "$2" "$1" "$(tr '\n' ' ' <"$work/merge-base.err")" ;;
  esac
}
# The broker's build must hold the enrollment refusal (T2), and Dispatch's build each commit
# OUTSIDE_DISPATCH_COMMITS names: the change that gives no broker token the approve scope, which
# P7's refusal and its control rest on, and the change that refuses a restricted member an issue's
# repository, which decides the repositories P5 and the sweep's GitHub proxy rows read.
lacks=''
problem=''
read -r -a dispatch_shas <<<"$dispatch_commits"
if ! command -v git >/dev/null 2>&1; then
  problem='git not on PATH'
else
  held=$(holds "$broker_revision" "$commit")
  case $held in
    yes) ;;
    no) lacks="the broker's build $broker_revision does not hold $commit" ;;
    *) problem=$held ;;
  esac
  for sha in "${dispatch_shas[@]}"; do
    held=$(holds "$dispatch_revision" "$sha")
    case $held in
      yes) ;;
      no) lacks="${lacks:+$lacks; }Dispatch's build $dispatch_revision does not hold $sha" ;;
      *) problem="${problem:+$problem; }$held" ;;
    esac
  done
fi
pins_line=$(printf '%s' "$pins_text" | tr '\n' ';')
if [ -n "$lacks" ]; then
  row pins "$want_pins" "$lacks${problem:+; $problem}; $pins_line" FAIL
  rest_rows="issue T1 T2 T3 T4 T5 $public_rows sweep"
  skip_from "a pin lacks a change the check needs, so the legs do not start" issue
  gate
elif [ -n "$problem" ]; then
  row pins "$want_pins" "$problem; $pins_line" UNKNOWN
  rest_rows="issue T1 T2 T3 T4 T5 $public_rows sweep"
  skip_from "whether the pins hold the changes the check needs is unread" issue
  gate
fi
row pins "$want_pins" "$pins_line" PASS

# --- issue: the scratch project's issue whose repository the edge relays (P5's admitted row). ---
issue_title="Outside acceptance: the repository the edge relays to the project"
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
# its machine login, in the background, writing DIR/helper.log, DIR/login.log and, when the login
# ends, its exit in DIR/login.exit. Sets login_code to the login's code, or login_problem.
start_login() {
  local deadline line
  login_code=''
  login_problem=''
  env -i "${helper_vars[@]}" agent-secrets-helper serve >"$1/helper.log" 2>&1 </dev/null &
  helper_pid=$!
  background="$background $helper_pid"
  deadline=$(($(now) + 30))
  while [ ! -S "$1/helper.sock" ] && [ "$(now)" -lt "$deadline" ]; do sleep 1; done
  if [ ! -S "$1/helper.sock" ]; then
    login_problem="the scratch helper made no socket within 30 s: $(text_of "$1/helper.log")"
    return
  fi
  (
    env -i "${helper_vars[@]}" agent-secrets machine login >"$1/login.log" 2>&1 </dev/null
    echo "$?" >"$1/login.exit"
  ) &
  background="$background $!"
  deadline=$(($(now) + 60))
  while [ -z "$login_code" ] && [ "$(now)" -lt "$deadline" ] && [ ! -f "$1/login.exit" ]; do
    while IFS= read -r line; do
      case $line in 'machine login code: '*) login_code=${line#machine login code: } ;; esac
    done <"$1/login.log"
    [ -n "$login_code" ] || sleep 1
  done
  [ -n "$login_code" ] ||
    login_problem="agent-secrets machine login printed no code within 60 s: $(text_of "$1/login.log")"
}
scratch="$work/scratch"
helper_env "$scratch" "$person"
scratch_env=("${helper_vars[@]}")
code=''
problem=$(lacking OUTSIDE_TAILNET_BROKER_URL OUTSIDE_PERSON OUTSIDE_CANARY)
[ -n "$problem" ] || problem=$(tool_problem agent-secrets agent-secrets-helper)
helper_problem=$problem
if [ -z "$helper_problem" ]; then
  say "starting the scratch helper in $scratch"
  start_login "$scratch"
  code=$login_code
  helper_problem=$login_problem
fi

# --- The peer session, and the session outside the project P6 must not list. ---
probe_env=()
while IFS= read -r name; do
  case $name in AGENT_SECRETS_* | DISPATCH_* | ENVOY_* | NATS_* | XDG_* | GCE_METADATA_HOST | HOME | PATH | TMPDIR) ;; *) continue ;; esac
  probe_env+=("$name=${!name}")
done <<EOF
$(compgen -e)
EOF
probe_env+=("PATH=$tools:$PATH")
# The peer of P4 is an Oh My Pi session of this box, OUTSIDE_PEER=omp (the default): the box's own
# model profile, with only the envoy plugin OUTSIDE_PEER_PLUGIN_DIR names (default this checkout's
# packages/pi-envoy, built) and no other extension, skill, rule or context file, its sessions kept
# in the scratch directory. It registers at the edge listener with this box's identity and a token
# naming the project, and its model answers the public leg's messages. OUTSIDE_PEER=probe is
# outside-agent-probe.ts's scripted peer instead, for a local stack with no model, and only with
# OUTSIDE_ALLOW_HTTP=1. Either way `peer_seen` gets one line per ping and reply the peer was shown:
# `<word> <nonce> <sender's stamp> <this box's clock when it was seen>`, in Unix ms.
peer_kind=${OUTSIDE_PEER:-omp}
peer_seen="$work/peer-seen.txt"
: >"$peer_seen"
peer_tmux="$work/peer-tmux.sock"
peer_problem=$(lacking OUTSIDE_LISTENER_URL)
[ -n "$peer_problem" ] || peer_problem=$(tool_problem bun)
case $peer_kind in
  omp)
    plugin_dir=${OUTSIDE_PEER_PLUGIN_DIR:-$here/../../../pi-envoy}
    [ -n "$peer_problem" ] || peer_problem=$(tool_problem omp tmux)
    [ -n "$peer_problem" ] || [ -d "$plugin_dir/dist" ] ||
      peer_problem="OUTSIDE_PEER_PLUGIN_DIR names $plugin_dir, which holds no built plugin (dist/)"
    ;;
  # The scripted peer is P4's control: it answers with no model in the loop, so a model peer's
  # failure where it passes is the model's. Its own PASS proves no model turn, so the gate reads it
  # UNKNOWN (below).
  probe) ;;
  *) peer_problem="OUTSIDE_PEER must be omp or probe, not $peer_kind" ;;
esac
# peer_text: what the peer has been shown so far: its pane, or the scripted peer's log.
peer_text() {
  if [ "$peer_kind" = omp ]; then
    tmux -S "$peer_tmux" capture-pane -p -J -S -5000 -t outside-e2e-peer 2>/dev/null
  else
    cat "$work/peer.log" 2>/dev/null
  fi
}
# watch_peer: runs until it is stopped, adding to `peer_seen` each ping and reply the peer is
# shown for the first time, with the time it was first seen.
# shellcheck disable=SC2329 # started in the background below
watch_peer() {
  local text rest word n stamped
  while :; do
    text=$(peer_text)
    for word in ping reply; do
      rest=$text
      while case $rest in *"outside-e2e $word "*) true ;; *) false ;; esac; do
        rest=${rest#*"outside-e2e $word "}
        n=${rest%% *}
        stamped=${rest#"$n at "}
        [ "$stamped" != "$rest" ] || continue
        stamped=${stamped%%[!0-9]*}
        [ -n "$stamped" ] || continue
        case $(cat "$peer_seen") in *"$word $n "*) ;; *)
          printf '%s %s %s %s000\n' "$word" "$n" "$stamped" "$(now)" >>"$peer_seen"
          ;;
        esac
      done
    done
    sleep 1
  done
}
peer_prompt="You are the peer session of an automated check. Follow these instructions exactly and take no other action. Wait for Envoy messages. When a message 'outside-e2e ping <N> at <T>' arrives from a session, reply to that session: run the bash command date +%s%3N, then call envoy_send to send that session exactly 'outside-e2e pong <N> at <the number date printed>'; then run date +%s%3N again and call envoy_send to send it exactly 'outside-e2e hello <N> at <the number date printed>'. When a message 'outside-e2e reply <N> at <T>' arrives from a session, run date +%s%3N and call envoy_send to send that session exactly 'outside-e2e done <N> at <the number date printed>'. Then wait again."
if [ -z "$peer_problem" ] && [ "$peer_kind" = omp ]; then
  peer_dir="$work/outside-e2e-peer-$nonce"
  mkdir -p "$peer_dir" "$work/peer-sessions"
  peer_dir=$(cd "$peer_dir" && pwd -P)
  say "starting the peer, omp in tmux (tmux -S $peer_tmux attach -t outside-e2e-peer)"
  env -u ENVOY_NATS_URL -u ENVOY_TOKEN -u ENVOY_TOKEN_FILE -u DISPATCH_TOKEN -u DISPATCH_TOKEN_FILE \
    PATH="$tools:$PATH" OMP_SKIP_SETUP=1 \
    ENVOY_URL="$OUTSIDE_LISTENER_URL" ENVOY_INBOUND=listener AGENT_SECRETS_PROJECTS="$project" \
    tmux -S "$peer_tmux" new-session -d -s outside-e2e-peer -x 220 -y 50 -c "$peer_dir" \
    omp --no-extensions --plugin-dir "$plugin_dir" --no-skills --no-rules --no-context-files \
    --session-dir "$work/peer-sessions" </dev/null
  ready=''
  deadline=$(($(now) + 60))
  while [ -z "$ready" ] && [ "$(now)" -lt "$deadline" ]; do
    case $(peer_text) in *"outside-e2e-peer-$nonce"*) ready=yes ;; *) sleep 1 ;; esac
  done
  peer=''
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -n "$ready" ] || break
    peer=$(json_field at_dir "$(env -i "${probe_env[@]}" AGENT_SECRETS_PROJECTS="$project" \
      OUTSIDE_PROBE_SESSION="outside-e2e-peer-finder-$nonce" OUTSIDE_LISTENER_URL="$OUTSIDE_LISTENER_URL" \
      OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" bun "$here/outside-agent-probe.ts" sessions "$peer_dir" \
      2>>"$work/probe.log" </dev/null)")
    [ -z "$peer" ] || [ "$peer" = null ] || break
    peer=''
    sleep 3
  done
  if [ -z "$ready" ]; then
    peer_problem="the peer's Oh My Pi TUI did not name its directory within 60 s"
  elif [ -z "$peer" ]; then
    peer_problem="the peer's Oh My Pi session is not on $OUTSIDE_LISTENER_URL's session list"
  else
    tmux -S "$peer_tmux" send-keys -t outside-e2e-peer -l "$peer_prompt"
    sleep 1
    tmux -S "$peer_tmux" send-keys -t outside-e2e-peer Enter
  fi
elif [ -z "$peer_problem" ]; then
  say "starting the scripted peer $peer at $OUTSIDE_LISTENER_URL"
  env -i "${probe_env[@]}" AGENT_SECRETS_PROJECTS="$project" OUTSIDE_PROBE_SESSION="$peer" \
    OUTSIDE_LISTENER_URL="$OUTSIDE_LISTENER_URL" OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" \
    bun "$here/outside-agent-probe.ts" peer >"$work/peer.log" 2>&1 </dev/null &
  background="$background $!"
  deadline=$(($(now) + 60))
  while [ "$(now)" -lt "$deadline" ]; do
    case $(cat "$work/peer.log") in *'"ready":true'* | *'"unknown"'*) break ;; esac
    sleep 1
  done
  case $(cat "$work/peer.log") in
    *'"ready":true'*) ;;
    *) peer_problem="the peer $peer did not start: $(text_of "$work/peer.log")" ;;
  esac
fi
if [ -z "$peer_problem" ]; then
  watch_peer &
  background="$background $!"
fi
# The session outside the project, `<peer>-foreign`, which P6 must not find on the edge's list.
foreign_problem=$(lacking OUTSIDE_INTERNAL_LISTENER_URL)
[ -n "$foreign_problem" ] || foreign_problem=$(tool_problem bun)
[ -n "$foreign_problem" ] || [ -n "$peer" ] || foreign_problem="no peer session id, which it is named after"
if [ -z "$foreign_problem" ]; then
  env -i "${probe_env[@]}" OUTSIDE_PROBE_SESSION="$peer" \
    OUTSIDE_LISTENER_URL="http://127.0.0.1:1" OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" \
    OUTSIDE_FOREIGN_LISTENER_URL="$OUTSIDE_INTERNAL_LISTENER_URL" \
    bun "$here/outside-agent-probe.ts" foreign >"$work/foreign.log" 2>&1 </dev/null &
  background="$background $!"
  deadline=$(($(now) + 60))
  while [ "$(now)" -lt "$deadline" ]; do
    case $(cat "$work/foreign.log") in *'"foreign":"'* | *'"unknown"'*) break ;; esac
    sleep 1
  done
  case $(cat "$work/foreign.log") in
    *'"foreign":"'*'"registered":2'*'"listed":true'*) ;;
    *) foreign_problem="the session $peer-foreign was not registered and listed at $OUTSIDE_INTERNAL_LISTENER_URL: $(text_of "$work/foreign.log")" ;;
  esac
fi

# --- The full member's browser session (OUTSIDE_FULL_COOKIE_FILE) at Dispatch's private origin.
# Dispatch's GitHub proxy reads as the browser's person and never as a token, and no broker token
# approves, so the positive controls of the sweep's proxy rows and of P7 read with it. The cookie
# reaches curl in a header file of mode 0600 alone, never on a command line. whoami names the
# member, whose address is the operator of P7's control login. ---
browser_header="$work/full-browser.h"
full_login=''
browser_problem=$(lacking OUTSIDE_TAILNET_DISPATCH_URL OUTSIDE_FULL_COOKIE_FILE)
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

# --- The read sweep's full-member half (restricted-sweep.sh beside this file): a full member has
# no cookie at the outside origin, and no full member's credential goes on the runner. So this box
# runs the sweep's fills mode, which makes the full member's half of every row, and passes the file
# to the workflow as `sweep_fills`; and it runs the sweep's writer, which comments on the write
# issue every 15 s and reads the full member's own feed, as the live socket's control. The token
# and the cookie reach the sweep through the environment of a subshell alone. Fills mode writes
# the full member's half of the GitHub proxy rows (a `can_fail` line for each read that can fail)
# only with the full member's browser session, since the proxy reads no token, so the sweep needs
# OUTSIDE_FULL_COOKIE_FILE. ---
sweep_script="$here/restricted-sweep.sh"
sweep_fills=''
sweep_problem=$(lacking OUTSIDE_TAILNET_DISPATCH_URL OUTSIDE_SWEEP_PROJECT OUTSIDE_SWEEP_FULL_TOKEN_FILE OUTSIDE_SWEEP_CONVERSATION_FILE OUTSIDE_SWEEP_WRITE_ISSUE OUTSIDE_FULL_COOKIE_FILE)
[ -n "$sweep_problem" ] || [ -f "$sweep_script" ] || sweep_problem="restricted-sweep.sh is not beside this file in $here"
[ -n "$sweep_problem" ] || [ -r "${OUTSIDE_SWEEP_FULL_TOKEN_FILE:-}" ] ||
  sweep_problem="OUTSIDE_SWEEP_FULL_TOKEN_FILE names $OUTSIDE_SWEEP_FULL_TOKEN_FILE, which cannot be read"
[ -n "$sweep_problem" ] || [ -z "$browser_problem" ] ||
  sweep_problem="the sweep's fills mode writes its GitHub proxy rows only with the full member's browser session, which was not read: $browser_problem"
# sweep_run MODE OUT: the sweep in MODE with the box's full member, its output in OUT.
sweep_run() {
  (
    SWEEP_FULL_TOKEN=$(tr -d '\r\n' <"$OUTSIDE_SWEEP_FULL_TOKEN_FILE")
    export SWEEP_FULL_TOKEN
    SWEEP_FULL_COOKIE=$(tr -d '\r\n' <"$OUTSIDE_FULL_COOKIE_FILE")
    export SWEEP_FULL_COOKIE
    SWEEP_MODE=$1 SWEEP_ORIGIN=$OUTSIDE_TAILNET_DISPATCH_URL SWEEP_PROJECT=$OUTSIDE_SWEEP_PROJECT \
      SWEEP_MEMBER=$person SWEEP_CONVERSATION_FILE=$OUTSIDE_SWEEP_CONVERSATION_FILE \
      SWEEP_FILLS_FILE="$work/sweep-fills" \
      SWEEP_WRITE_ISSUE=$OUTSIDE_SWEEP_WRITE_ISSUE SWEEP_WRITE_SECONDS=3600 SWEEP_WRITE_EVERY=15 \
      exec bash "$sweep_script" >"$2" 2>&1 </dev/null
  )
}
if [ -z "$sweep_problem" ]; then
  say "the sweep's fills mode"
  if ! sweep_run fills "$work/sweep-fills.log" || [ ! -s "$work/sweep-fills" ]; then
    sweep_problem="the sweep's fills mode made no file: $(text_of "$work/sweep-fills.log")"
  elif [ "$(wc -c <"$work/sweep-fills")" -gt 40960 ]; then
    sweep_problem="the sweep's fills file is $(wc -c <"$work/sweep-fills") bytes, past the 40 KiB one workflow input holds"
  else
    sweep_fills=$(base64 <"$work/sweep-fills" | tr -d '\n')
    # The writer comments every 15 s until the public leg ends, when it is stopped: it runs on a
    # scratch issue (OUTSIDE_SWEEP_WRITE_ISSUE), about four comments a minute of the run.
    sweep_run writer "$work/sweep-writer.log" &
    writer_pid=$!
    background="$background $writer_pid"
  fi
fi

# The sweep's proxy rows' positive control. The runner reads the GitHub proxy as the test person
# for the repository the fills name as one only the sweep's project's issues resolve to, and must
# be answered as for a repository the App is not installed on. That answer means something only
# where the proxy answers the same repository to a person who may see it. So the fills must hold
# the full member's answer that can fail for each proxy read the runner makes (a `can_fail` line
# each), and this box's browser reads, through the proxy, the workflow runs of the commit the
# sweep's own proxy rows name, and must get 200 with a list of workflow runs. Without either, the
# sweep row reads UNKNOWN.
proxy_sha=0123456789abcdef0123456789abcdef01234567
proxy_reads='GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}
GET /api/github/rest/repos/{owner}/{repo}/issues/{number}
GET /api/github/rest/repos/{owner}/{repo}/commits/{sha}/check-runs
GET /api/github/rest/repos/{owner}/{repo}/actions/runs?head_sha={sha}
GET /api/v1/github/app-installation/{owner}/{repo}'
proxy_control=''
proxy_control_problem=$browser_problem
[ -n "$proxy_control_problem" ] || [ -z "$sweep_problem" ] ||
  proxy_control_problem="the sweep's fills, which name the repository, were not made: $sweep_problem"
if [ -z "$proxy_control_problem" ]; then
  proxy_repo=''
  can_fail=$newline
  while IFS="$tab" read -r name value; do
    case $name in
      proxy_repo) proxy_repo=$value ;;
      can_fail) can_fail="$can_fail$value$newline" ;;
    esac
  done <"$work/sweep-fills"
  blind=''
  while IFS= read -r each; do
    case $can_fail in *"$newline$each$newline"*) ;; *) blind="${blind:+$blind, }$each" ;; esac
  done <<EOF
$proxy_reads
EOF
  [ -z "$blind" ] ||
    proxy_control_problem="the sweep's fills hold no full member's answer that can fail for $blind"
fi
if [ -z "$proxy_control_problem" ]; then
  case $proxy_repo in
    '') proxy_control_problem="the sweep's fills name no repository that only $OUTSIDE_SWEEP_PROJECT's issues resolve to" ;;
    *[!A-Za-z0-9_./-]* | */*/* | /* | */) proxy_control_problem="the sweep's fills name no owner/name repository: $proxy_repo" ;;
    */*)
      status=$(browser GET "/api/github/rest/repos/$proxy_repo/actions/runs?head_sha=$proxy_sha" "$work/proxy-control.json")
      answer=$(tr -d ' \n' <"$work/proxy-control.json" 2>/dev/null)
      case $status:$answer in
        200:*'"workflow_runs":'*) proxy_control="the full member's browser read $proxy_repo's workflow runs through the proxy: 200" ;;
        *) proxy_control_problem="the full member's browser read of $proxy_repo through the proxy answered $status: ${answer:0:200} $(text_of "$work/browser.err")" ;;
      esac
      ;;
    *) proxy_control_problem="the sweep's fills name no owner/name repository: $proxy_repo" ;;
  esac
fi

# --- The public leg: the workflow on a runner on the public internet, or a local command. ---
results="$work/public"
mkdir -p "$results"
public_problem=''
run_id=''
if [ -n "$helper_problem" ]; then
  public_problem="not started: the scratch helper has no code: $helper_problem"
elif [ -n "${OUTSIDE_PUBLIC_COMMAND:-}" ]; then
  say "starting the public leg here: $OUTSIDE_PUBLIC_COMMAND"
  set -f
  # shellcheck disable=SC2086 # a command line, split on blanks on purpose
  set -- $OUTSIDE_PUBLIC_COMMAND
  set +f
  (
    OUTSIDE_TAILNET_USER_CODE=$code OUTSIDE_PEER_SESSION=$peer \
      OUTSIDE_SWEEP_FILLS=$sweep_fills OUTSIDE_PUBLIC_RESULTS=$results \
      "$@" >"$work/public.log" 2>&1 </dev/null
    echo "$?" >"$work/public.exit"
  ) &
  background="$background $!"
else
  problem=$(lacking OUTSIDE_WORKFLOW_REPO)
  [ -n "$problem" ] || problem=$(tool_problem gh)
  if [ -n "$problem" ]; then
    public_problem="not started: $problem"
  else
    fields=(-f checks=all -f "project=$project" -f "peer_session=$peer" -f "tailnet_user_code=$code")
    [ -z "$sweep_fills" ] || fields+=(-f "sweep_fills=$sweep_fills")
    while IFS= read -r pair; do
      [ -z "$pair" ] || fields+=(-f "$pair")
    done <<EOF
${OUTSIDE_WORKFLOW_INPUTS:-}
EOF
    workflow=${OUTSIDE_WORKFLOW:-outside-edge-e2e.yaml}
    say "gh workflow run $workflow -R $OUTSIDE_WORKFLOW_REPO"
    started=$(gh workflow run "$workflow" -R "$OUTSIDE_WORKFLOW_REPO" "${fields[@]}" 2>&1 </dev/null)
    case $started in */actions/runs/[0-9]*)
      run_id=${started##*/actions/runs/}
      run_id=${run_id%%[!0-9]*}
      ;;
    esac
    [ -n "$run_id" ] || public_problem="gh workflow run printed no run URL: ${started:0:300}"
  fi
fi

# --- T1: the scratch helper's machine login completes, approved as the test person. ---
want_t1="the scratch helper's machine login completes: its machine file holds a credential, and machine list names ${person:-<OUTSIDE_PERSON>} as its approver"
credential=''
if [ -n "$helper_problem" ]; then
  row T1 "$want_t1" "$helper_problem" UNKNOWN
else
  say "T1: waiting for the code $code to be approved (the code waits 15 minutes)"
  deadline=$(($(now) + 960))
  while [ ! -f "$scratch/login.exit" ] && [ "$(now)" -lt "$deadline" ]; do sleep 2; done
  exited=$(cat "$scratch/login.exit" 2>/dev/null)
  [ ! -f "$scratch/machine.json" ] || credential=$(json_field credential_id "$(tr -d ' \n' <"$scratch/machine.json")")
  listed=$(env -i "${scratch_env[@]}" agent-secrets machine list --json 2>&1 </dev/null)
  person_lower=$(printf '%s' "$person" | tr '[:upper:]' '[:lower:]')
  if [ -z "$exited" ]; then
    row T1 "$want_t1" "code $code was not approved within 16 minutes" UNKNOWN
  elif [ "$exited" -ne 0 ]; then
    row T1 "$want_t1" "machine login exited $exited: $(text_of "$scratch/login.log")" FAIL
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
if ! passed T1; then
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

# --- T3: the positive control, the box's own helper granted the canary at once. ---
want_t3="a fresh session enrolled through this box's own helper is granted ${canary:-<OUTSIDE_CANARY>} at once, a value 64 long"
problem=$(lacking OUTSIDE_CANARY)
[ -n "$problem" ] || problem=$(tool_problem agent-secrets)
if [ -n "$problem" ]; then
  row T3 "$want_t3" "$problem" UNKNOWN
else
  agent-secrets register --session-id "outside-e2e-t3-$nonce" --wait 30 \
    --exec -- sh "$work/length.sh" "$canary" >"$work/t3.out" 2>"$work/t3.err" </dev/null
  read_length "$work/t3.out"
  case $request_exit:$length in
    0:64) row T3 "$want_t3" "granted at once, a value 64 long" PASS ;;
    0:*[0-9]*) row T3 "$want_t3" "granted, a value $length long" FAIL ;;
    75:*) row T3 "$want_t3" "the request waits for a person's approval (exit 75), not granted at once" FAIL ;;
    77:*) row T3 "$want_t3" "the request was denied (exit 77): $request_said" FAIL ;;
    *) row T3 "$want_t3" "request exit ${request_exit:-none}, length ${length:-none}: $request_said $(text_of "$work/t3.err")" UNKNOWN ;;
  esac
fi

# --- P7's positive control. No broker token approves, the full member's own included, so the 403
# that P7 reads for the agent's token says something about that token only where a browser session
# is answered on the same routes. The full member's browser mints a personal agent token (201),
# revoked again at once, and approves a machine login (200), a scratch helper's whose operator is
# that member, revoked again at once. Without both answers P7 reads UNKNOWN. The mint's answer, the
# one place the token's plaintext is shown, is removed as soon as its id is read. ---
p7_control=''
p7_control_problem=$browser_problem
[ -n "$p7_control_problem" ] || p7_control_problem=$(lacking OUTSIDE_TAILNET_BROKER_URL)
[ -n "$p7_control_problem" ] || p7_control_problem=$(tool_problem agent-secrets agent-secrets-helper)
if [ -z "$p7_control_problem" ]; then
  say "P7's control: the full member's browser mints an agent token and approves a machine login"
  printf '{"name":"outside-e2e-control-%s"}\n' "$nonce" >"$work/mint-body.json"
  minted=$(browser POST /api/v1/me/agent-tokens "$work/mint.json" "$work/mint-body.json")
  token_id=$(json_field id "$(tr -d ' \n' <"$work/mint.json" 2>/dev/null)")
  rm -f "$work/mint.json"
  dropped=none
  [ -z "$token_id" ] || dropped=$(browser DELETE "/api/v1/me/agent-tokens/$token_id" "$work/mint-revoke.json")
  control="$work/control"
  helper_env "$control" "$full_login"
  start_login "$control"
  approved=''
  revoked=none
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
      if [ -n "$control_credential" ]; then
        revoked=$(browser POST "/api/v1/machine-logins/$control_credential/revoke" "$work/control-revoke.json")
      else
        approved="$approved, with no credential_id: $(text_of "$work/approve.json")"
      fi
    fi
  fi
  kill "$helper_pid" 2>/dev/null
  case $revoked in 2??) ;; *) say "P7's control: the control's machine login was not revoked ($revoked)" ;; esac
  case $dropped in 2??) ;; *) say "P7's control: the control's agent token was not revoked ($dropped)" ;; esac
  case $minted:$approved in
    201:2??) p7_control="the full member's browser minted an agent token, 201 (revoked: $dropped), and approved a machine login, $approved (revoked: $revoked)" ;;
    201:*) p7_control_problem="the full member's browser approved no machine login: $approved" ;;
    *) p7_control_problem="the full member's browser mint of an agent token answered $minted; the approve: $approved" ;;
  esac
fi

# --- T5's removal, during the public run's T5 window: the public leg tells the peer the window is
# open (`outside-e2e t5-open <nonce> at <ms>`), this leg has the owner remove the test person from
# the project's access list, on this terminal or with OUTSIDE_OWNER_REMOVE_COMMAND (a harness that
# acts as the owner's browser), and keeps the time; T5 is judged after the run, below. The list is
# read with this box's own credential at OUTSIDE_TAILNET_DISPATCH_URL, before (the control: the
# person is on it) and after (the removal was made). ---
# listed_now: whether the person is on the project's access list: true, false, or what was read.
listed_now() {
  local answer
  answer=$(env -i "${probe_env[@]}" OUTSIDE_PROBE_SESSION="outside-e2e-access-$nonce" \
    OUTSIDE_LISTENER_URL="http://127.0.0.1:1" OUTSIDE_DISPATCH_URL="${OUTSIDE_TAILNET_DISPATCH_URL:-}" \
    bun "$here/outside-agent-probe.ts" access "$project" "$person" 2>>"$work/probe.log" </dev/null)
  case $(json_field listed "$answer") in
    true | false) json_field listed "$answer" ;;
    *) printf '%s' "${answer:0:200}" ;;
  esac
}
# public_running: the public leg has not ended yet.
public_running() {
  if [ -n "$run_id" ]; then
    case $(gh run view "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --json status --jq .status 2>/dev/null </dev/null) in
      completed) return 1 ;;
    esac
    return 0
  fi
  [ ! -f "$work/public.exit" ]
}
removed_at=''
t5_problem=$(lacking OUTSIDE_TAILNET_DISPATCH_URL)
[ -n "$t5_problem" ] || [ -z "$public_problem" ] || t5_problem="the public leg did not start: $public_problem"
[ -n "$t5_problem" ] || [ -z "$peer_problem" ] || t5_problem="no peer, which the window is opened to: $peer_problem"
[ -n "$t5_problem" ] || [ -n "${OUTSIDE_OWNER_REMOVE_COMMAND:-}" ] || { : </dev/tty; } 2>/dev/null ||
  t5_problem="there is no terminal to ask the owner for the removal on, and no OUTSIDE_OWNER_REMOVE_COMMAND"
if [ -z "$t5_problem" ]; then
  say "T5: waiting for the public leg to open its window"
  opened=''
  while [ -z "$opened" ] && public_running; do
    case $(peer_text) in *'outside-e2e t5-open '*) opened=yes ;; *) sleep 5 ;; esac
  done
  before=$(listed_now)
  if [ -z "$opened" ]; then
    t5_problem="the public leg ended without opening its window"
  elif [ "$before" != true ]; then
    t5_problem="before the removal $person is not read on the access list ($before), so a refusal would prove nothing"
  else
    if [ -n "${OUTSIDE_OWNER_REMOVE_COMMAND:-}" ]; then
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
    removed_at=$(now)
    say "T5: the removal read at $(date -u '+%H:%M:%SZ')"
  fi
fi

# --- The public leg's end, its TSV and its revocations. ---
if [ -z "$public_problem" ] && [ -n "$run_id" ]; then
  say "waiting for run $run_id"
  gh run watch "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --interval 30 >/dev/null 2>&1 </dev/null
  gh run download "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" -n outside-agent-public -D "$results" >/dev/null 2>&1 </dev/null
  gh run download "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" -n outside-edge-revocations -D "$results" >/dev/null 2>&1 </dev/null
  gh run download "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" -n restricted-sweep -D "$results" >/dev/null 2>&1 </dev/null
  conclusion=$(gh run view "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --json url,conclusion --jq '.url + " " + .conclusion' 2>&1 </dev/null)
  say "run $run_id: $conclusion"
elif [ -z "$public_problem" ]; then
  say "waiting for the public leg's command"
  deadline=$(($(now) + 3600))
  while [ ! -f "$work/public.exit" ] && [ "$(now)" -lt "$deadline" ]; do sleep 5; done
  [ -f "$work/public.exit" ] || public_problem="the public leg's command had not ended within an hour"
fi
# The public leg has ended, so the sweep's writer has nothing more to show it.
[ -z "${writer_pid:-}" ] || kill "$writer_pid" 2>/dev/null
public_tsv="$results/outside-agent-public.tsv"
[ -n "$public_problem" ] || [ -f "$public_tsv" ] || public_problem="the public leg left no outside-agent-public.tsv"

# --- T4: the scratch login is revoked on the test person's page, and the helper reads it ended. ---
want_t4="the public leg's last step revokes the scratch login (its revoke row PASS); machine list through the scratch helper is then refused LAUNCHER_INVALID"
if ! passed T1; then
  row T4 "$want_t4" "not run: T1 did not pass, so there is no scratch login" UNKNOWN
elif [ ! -f "$results/outside-edge-revocations.tsv" ]; then
  row T4 "$want_t4" "no outside-edge-revocations.tsv from the public leg${public_problem:+: $public_problem}" UNKNOWN
else
  revoked=''
  while IFS="$tab" read -r name _ _ verdict; do
    [ "$name" = "revoke-$credential" ] && revoked=$verdict
  done <"$results/outside-edge-revocations.tsv"
  ended=$(env -i "${scratch_env[@]}" agent-secrets machine list 2>&1 </dev/null)
  ended_exit=$?
  if [ -z "$revoked" ]; then
    row T4 "$want_t4" "the revocations hold no row for credential $credential" FAIL
  elif [ "$revoked" != PASS ]; then
    row T4 "$want_t4" "revoke-$credential reads $revoked" "$revoked"
  elif [ "$ended_exit" -eq 0 ]; then
    row T4 "$want_t4" "revoked, and machine list through the scratch helper still answers" FAIL
  else
    case $ended in
      *LAUNCHER_INVALID*) row T4 "$want_t4" "revoked; machine list refused LAUNCHER_INVALID" PASS ;;
      *) row T4 "$want_t4" "revoked; machine list exited $ended_exit: ${ended:0:300}" UNKNOWN ;;
    esac
  fi
fi

# --- T5, judged from the public leg's three T5 rows against this leg's removal time. Each part
# must have stopped no earlier than the removal and within 325 s of it (a token lives at most 300 s,
# and the edge checks an open socket every 20 s plus a 5 s cache), the cookie within 30 s. A part
# that stopped before the removal stopped for some other reason, which says nothing of it. ---
want_t5="once the owner removes ${person:-<OUTSIDE_PERSON>} from ${project:-<project>}'s access list: the person's cookie gets 401 within 30 s, their passkey sign-in is refused, a fresh trade of their live device login is refused, and the relay socket open then closes, each within 325 s"
# public_part NAME: the public TSV's NAME row as `verdict<TAB>got`, or nothing.
public_part() {
  local each rest
  [ -f "$public_tsv" ] || return 0
  while IFS= read -r each; do
    case $each in "$1$tab"*)
      rest=${each#*"$tab"*"$tab"}
      printf '%s\t%s' "${rest##*"$tab"}" "${rest%"$tab"*}"
      ;;
    esac
  done <"$public_tsv"
}
# mark NAME TEXT: the number after `NAME=` in TEXT, or nothing.
mark() {
  local rest=${2#*"$1="}
  [ "$rest" != "$2" ] || return 0
  rest=${rest%%[!0-9]*}
  printf '%s' "$rest"
}
if [ -n "$t5_problem" ]; then
  row T5 "$want_t5" "$t5_problem" UNKNOWN
else
  after=$(listed_now)
  lags=''
  wrong=''
  unread=''
  for part in T5-agent:refused_at:325 T5-agent:socket_closed_at:325 T5-cookie:at:30 T5-passkey:at:325; do
    name=${part%%:*}
    rest=${part#*:}
    key=${rest%%:*}
    bound=${rest#*:}
    read_row=$(public_part "$name")
    verdict=${read_row%%"$tab"*}
    got=${read_row#*"$tab"}
    at=$(mark "$key" "$got")
    if [ -z "$read_row" ]; then
      unread="${unread:+$unread; }the public TSV has no $name row"
    elif [ "$verdict" = FAIL ]; then
      wrong="${wrong:+$wrong; }$name: $got"
    elif [ "$verdict" != PASS ] || [ -z "$at" ]; then
      unread="${unread:+$unread; }$name ($key) not read: ${got:0:200}"
    elif [ "$at" -lt "$removed_at" ]; then
      unread="${unread:+$unread; }$name ($key) stopped $((removed_at - at)) s before the removal, for another reason"
    elif [ $((at - removed_at)) -gt "$bound" ]; then
      wrong="${wrong:+$wrong; }$name ($key) $((at - removed_at)) s after the removal, past $bound s"
    else
      lags="${lags:+$lags, }$name $key $((at - removed_at)) s"
    fi
  done
  if [ "$after" != false ]; then
    row T5 "$want_t5" "after the run the access list reads $after, so the removal was not made" UNKNOWN
  elif [ -n "$wrong" ]; then
    row T5 "$want_t5" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row T5 "$want_t5" "$unread${lags:+; read: $lags}" UNKNOWN
  else
    row T5 "$want_t5" "after the removal: $lags. Re-adding $person is the owner's own act" PASS
  fi
fi

# --- P1-P9 as the public leg's TSV has them, each held to what this leg saw beside it. ---
for name in $public_rows; do
  line=''
  if [ -z "$public_problem" ]; then
    while IFS= read -r each; do
      case $each in "$name$tab"*) line=$each ;; esac
    done <"$public_tsv"
  fi
  if [ -z "$line" ]; then
    row "$name" "the public leg's row $name" "${public_problem:-the public TSV has no $name row}" UNKNOWN
    continue
  fi
  IFS="$tab" read -r _ want got verdict <<EOF
$line
EOF
  case $verdict in PASS | FAIL | UNKNOWN) ;; *)
    row "$name" "$want" "the public TSV's verdict is $verdict: $got" UNKNOWN
    continue
    ;;
  esac
  # A result this leg's own setup cannot stand behind reads UNKNOWN. P4 depends on the peer
  # answering, so any verdict of it without the peer is unproven. A PASS of P5, P6 or P7 rests on a
  # control this leg made: P5's admission on the issue naming the repository, P6's absences on
  # the peer and on a session outside the project that a private listener lists, P7's refusals on
  # the full member's browser being answered 201 and 200 on the same routes. A FAIL of any of them
  # is evidence by itself (a refusal missing, a foreign session listed) and stands.
  held_by=''
  case $name:$verdict in
    P4:*) held_by=$peer_problem ;;
    P5:PASS) passed issue || held_by="the issue row did not pass, so no repository is known to resolve to $project" ;;
    P6:PASS) held_by=$peer_problem${foreign_problem:+${peer_problem:+; }$foreign_problem} ;;
    P7:PASS) held_by=${p7_control_problem:+no positive control: $p7_control_problem} ;;
  esac
  if [ -n "$held_by" ] && [ "$verdict" != UNKNOWN ]; then
    row "$name" "$want" "$verdict as the public leg read it, unproven here: $held_by; $got" UNKNOWN
  elif [ "$name:$verdict" = P4:PASS ]; then
    # The public leg read the peer's answers on its side; the peer's side is read here: its ping
    # and the reply must each be shown to the peer within 10 s of the public side's stamp.
    exchange=${got##*nonce=}
    exchange=${exchange%% *}
    lags=''
    late=''
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
    done
    if [ -n "$late" ]; then
      row P4 "$want" "the $peer_kind peer was shown the public side's $late after it was sent; $got" FAIL
    elif [ -n "$unseen" ]; then
      row P4 "$want" "PASS as the public leg read it, unproven here: the $peer_kind peer's view never showed its $unseen of nonce $exchange; $got" UNKNOWN
    elif [ "$peer_kind" = probe ]; then
      row P4 "$want" "$got; the scripted control peer was shown the $lags after they were sent: the path passes with no model in the loop, and the gate needs the model peer (OUTSIDE_PEER=omp)" UNKNOWN
    else
      row P4 "$want" "$got; the $peer_kind peer was shown the $lags after they were sent" PASS
    fi
  elif [ "$name:$verdict" = P7:PASS ]; then
    row P7 "$want" "$got; the positive control: $p7_control" PASS
  else
    row "$name" "$want" "$got" "$verdict"
  fi
done

# --- sweep: the read sweep on the runner, held to this box's two controls: at least one of the
# full member's comments delivered strictly inside the window the sweep read the member's feed
# over, which is what makes its "nothing reached the member" mean something, and the full member's
# browser answered by the GitHub proxy for the repository the runner's proxy rows read, which is
# what makes their "answered as a repository the App is not installed on" mean something. A FAIL of
# the sweep stands without either. ---
want_sweep="restricted-sweep.sh on the runner PASS, with a full member's comment delivered inside its live window, and the full member's browser answered 200 by the GitHub proxy for the repository its proxy rows read"
sweep_log="$results/restricted-sweep.log"
if [ -n "$sweep_problem" ]; then
  row sweep "$want_sweep" "$sweep_problem" UNKNOWN
elif [ ! -f "$sweep_log" ]; then
  row sweep "$want_sweep" "the run left no restricted-sweep.log${public_problem:+: $public_problem}" UNKNOWN
else
  sweep_exit=$(tr -d ' \n' <"$results/restricted-sweep.exit" 2>/dev/null)
  window=''
  while IFS= read -r line; do
    case $line in 'live window '*) window=${line#live window } ;; esac
  done <"$sweep_log"
  w0=${window%% *}
  w1=${window#* }
  inside=''
  while IFS= read -r line; do
    case $line in 'write '*' delivered')
      at=${line#write }
      at=${at%% *}
      [[ -n "$w0" && "$at" > "$w0" && "$at" < "$w1" ]] && inside=$at
      ;;
    esac
  done <"$work/sweep-writer.log"
  # The verdict is the sweep's last line; the log's first is the workflow's own note.
  verdict_line=''
  while IFS= read -r line; do
    [ -z "$line" ] || verdict_line=$line
  done <"$sweep_log"
  verdict_line=${verdict_line:0:300}
  case $sweep_exit in
    1) row sweep "$want_sweep" "the sweep failed: $verdict_line" FAIL ;;
    0)
      if [ -n "$proxy_control_problem" ]; then
        row sweep "$want_sweep" "the sweep passed, and its GitHub proxy rows have no positive control: $proxy_control_problem" UNKNOWN
      elif [ -z "$window" ]; then
        row sweep "$want_sweep" "the sweep passed and printed no live window, so the writer's control cannot be held to it" UNKNOWN
      elif [ -z "$inside" ]; then
        row sweep "$want_sweep" "the sweep passed, and no comment of the writer was delivered inside its live window $window, so its live check proved nothing" UNKNOWN
      else
        row sweep "$want_sweep" "the sweep passed; the writer's comment at $inside was delivered inside its live window $window; $proxy_control" PASS
      fi
      ;;
    *) row sweep "$want_sweep" "the sweep exited ${sweep_exit:-with no code read}: $verdict_line" UNKNOWN ;;
  esac
fi

# --- The gate, against pins read again: a pin that moved between the legs holds it. ---
read_pins
if [ -n "$pins_problem" ]; then
  pins_moved="the pins could not be read again at the end: $pins_problem"
elif [ "$pins_text" != "$pins_at_start" ]; then
  pins_moved="a pin moved during the run: now $(printf '%s' "$pins_text" | tr '\n' ';')"
fi
gate
