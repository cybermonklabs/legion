#!/usr/bin/env bash
# The tailnet leg of the outside agent's acceptance, and the one command that runs both legs: a
# developer box on the private network runs it, with its project's key as the one argument. It
# reads the deployed pins and refuses to start unless the secrets broker's build holds the
# enrollment refusal; makes the scratch project's issue the public leg's GitHub row reads; starts
# a scratch agent-secrets-helper whose machine login the test person approves from the public
# leg's browser; starts our peer session; starts the public leg (outside-agent-public.sh, rows
# P1-P9) on a runner on the public internet; runs rows T1-T4 against the broker's private origin,
# the one place its enrollment route is served; reads the public leg's TSV; asks the operator, on
# this terminal, to remove the test person from the project's access list and reads that the test
# person's live device login stops (T5); and prints the gate. outside-agent-acceptance.md, beside
# this file, says what each row reads and what the gate holds.
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
#   OUTSIDE_TAILNET_BROKER_URL   the broker's origin on the private network.
#   OUTSIDE_TAILNET_DISPATCH_URL  Dispatch's origin on the private network, where T5 reads the
#                                project's access list with this box's own credential.
#   OUTSIDE_LISTENER_URL         the edge listener, where the peer session registers.
#   OUTSIDE_INTERNAL_LISTENER_URL  a listener on the private network, where a session outside the
#                                project (<peer>-foreign) registers for P6's control.
#   OUTSIDE_PERSON               the test person's address.
#   OUTSIDE_CANARY               the name of the canary secret T2 and T3 ask for.
#   OUTSIDE_REPOSITORY           owner/name of the repository the scratch project's issue names.
#   OUTSIDE_WORKFLOW_REPO        owner/name of the repository whose workflow runs the public leg;
#                                OUTSIDE_WORKFLOW names the workflow (outside-edge-e2e.yaml), and
#                                OUTSIDE_WORKFLOW_INPUTS holds its other inputs, one name=value a
#                                line. Or OUTSIDE_PUBLIC_COMMAND: a command line, split on blanks,
#                                that runs the public leg here instead, for a local stack; it gets
#                                OUTSIDE_TAILNET_USER_CODE, OUTSIDE_TAILNET_HOLD_CODE (the code of
#                                T5's hold login, which it approves and does not revoke),
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
# comment with `dispatch comment`; OUTSIDE_ALLOW_HTTP=1, which lets http://127.0.0.1 origins
# through for a local stack.
#
# T3, the positive control, and the issue run as this box's own: its `agent-secrets` helper and
# its `dispatch` command, as the caller's environment has them. The scratch helper of T1, T2 and
# T4 runs with HOME, XDG_RUNTIME_DIR, XDG_STATE_HOME and every AGENT_SECRETS_* path of its own,
# all in one `mktemp -d` directory, so it shares no key, socket or record with the box's.
#
# Output: on stdout a TSV, the header `row want got verdict`, the rows pins, issue, T1-T4, the
# public leg's P1-P9 as its TSV has them, and the gate. Progress goes to stderr. A row it could not
# run reads UNKNOWN naming what it could not read, never PASS. Exit 0 when the gate passed, 1 when
# a row failed, 2 otherwise. It prints no secret value: T2 and T3 read the canary's length alone.
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
rest_rows="pins issue T1 T2 T3 T4 T5 $public_rows"
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

want_pins="the broker's pin holds ${commit:-<OUTSIDE_ENROLL_REFUSAL_COMMIT>}; the pins printed"
problem=$(lacking OUTSIDE_PINS OUTSIDE_ENROLL_REFUSAL_COMMIT)
[ -n "$project" ] || problem="${problem:+$problem; }the project key (\$1) is not given"
[ -n "$work" ] || problem="${problem:+$problem; }mktemp -d made no scratch directory"
if [ -z "$problem" ] && [ ! -r "$OUTSIDE_PINS" ]; then
  problem="OUTSIDE_PINS names $OUTSIDE_PINS, which cannot be read"
fi
if [ -n "$problem" ]; then
  row pins "$want_pins" "$problem" UNKNOWN
  rest_rows="issue T1 T2 T3 T4 T5 $public_rows"
  skip_from "the pins were not read: $problem" issue
  gate
fi
read_pins
if [ -n "$pins_problem" ]; then
  row pins "$want_pins" "$pins_problem" UNKNOWN
  rest_rows="issue T1 T2 T3 T4 T5 $public_rows"
  skip_from "the pins were not read" issue
  gate
fi
pins_at_start=$pins_text
broker_revision=''
while IFS=' ' read -r name image revision; do
  [ "$name" = broker ] && broker_revision=$revision
done <<EOF
$pins_text
EOF
checkout=${OUTSIDE_LEGION_CHECKOUT:-$here}
held=''
if ! command -v git >/dev/null 2>&1; then
  held=unread
  problem='git not on PATH'
else
  for sha in "$commit" "$broker_revision"; do
    git -C "$checkout" cat-file -e "$sha^{commit}" 2>/dev/null ||
      git -C "$checkout" fetch -q origin "$sha" >/dev/null 2>&1 </dev/null
  done
  git -C "$checkout" merge-base --is-ancestor "$commit" "$broker_revision" 2>"$work/merge-base.err"
  case $? in
    0) held=yes ;;
    1) held=no ;;
    *)
      held=unread
      problem="git merge-base --is-ancestor $commit $broker_revision: $(tr '\n' ' ' <"$work/merge-base.err")"
      ;;
  esac
fi
pins_line=$(printf '%s' "$pins_text" | tr '\n' ';')
case $held in
  yes) row pins "$want_pins" "$pins_line" PASS ;;
  no)
    row pins "$want_pins" "the broker's build $broker_revision does not hold $commit; $pins_line" FAIL
    rest_rows="issue T1 T2 T3 T4 T5 $public_rows"
    skip_from "the broker's pin lacks the enrollment refusal, so the legs do not start" issue
    gate
    ;;
  *)
    row pins "$want_pins" "$problem; $pins_line" UNKNOWN
    rest_rows="issue T1 T2 T3 T4 T5 $public_rows"
    skip_from "whether the broker's pin holds the enrollment refusal is unread" issue
    gate
    ;;
esac

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
scratch="$work/scratch"
mkdir -p "$scratch/home" "$scratch/run" "$scratch/state" "$scratch/keys"
chmod 700 "$scratch" "$scratch/home" "$scratch/run" "$scratch/state" "$scratch/keys"
printf '%s\n' "$person" >"$scratch/operator"
scratch_env=(
  "PATH=$tools:$PATH"
  "HOME=$scratch/home"
  "XDG_RUNTIME_DIR=$scratch/run"
  "XDG_STATE_HOME=$scratch/state"
  "TMPDIR=${TMPDIR:-/tmp}"
  "AGENT_SECRETS_HELPER_SOCK=$scratch/helper.sock"
  "AGENT_SECRETS_MACHINE_FILE=$scratch/machine.json"
  "AGENT_SECRETS_STATE_FILE=$scratch/sessions.json"
  "AGENT_SECRETS_KEY_DIR=$scratch/keys"
  "AGENT_SECRETS_OPERATOR_FILE=$scratch/operator"
  "AGENT_SECRETS_URL=${OUTSIDE_TAILNET_BROKER_URL:-}"
)
code=''
problem=$(lacking OUTSIDE_TAILNET_BROKER_URL OUTSIDE_PERSON OUTSIDE_CANARY)
[ -n "$problem" ] || problem=$(tool_problem agent-secrets agent-secrets-helper)
helper_problem=$problem
if [ -z "$helper_problem" ]; then
  say "starting the scratch helper in $scratch"
  env -i "${scratch_env[@]}" agent-secrets-helper serve >"$work/helper.log" 2>&1 </dev/null &
  background="$background $!"
  deadline=$(($(now) + 30))
  while [ ! -S "$scratch/helper.sock" ] && [ "$(now)" -lt "$deadline" ]; do sleep 1; done
  if [ ! -S "$scratch/helper.sock" ]; then
    helper_problem="the scratch helper made no socket within 30 s: $(text_of "$work/helper.log")"
  else
    (
      env -i "${scratch_env[@]}" agent-secrets machine login >"$work/login.log" 2>&1 </dev/null
      echo "$?" >"$work/login.exit"
    ) &
    background="$background $!"
    deadline=$(($(now) + 60))
    while [ -z "$code" ] && [ "$(now)" -lt "$deadline" ] && [ ! -f "$work/login.exit" ]; do
      while IFS= read -r line; do
        case $line in 'machine login code: '*) code=${line#machine login code: } ;; esac
      done <"$work/login.log"
      [ -n "$code" ] || sleep 1
    done
    [ -n "$code" ] || helper_problem="agent-secrets machine login printed no code within 60 s: $(text_of "$work/login.log")"
  fi
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
    PATH="$tools:$PATH" \
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

# --- T5's hold login: a device login of the test person, the one the outside agent runs, whose
# code the public leg's browser approves and its last step leaves live (`tailnet_hold_code`). Its
# key and refresh token live only in the scratch directory, which the cleanup removes, so no one
# can trade it after this run. ---
hold_dir="$work/hold"
mkdir -p "$hold_dir/home"
chmod 700 "$hold_dir" "$hold_dir/home"
hold_file="$hold_dir/device-login.json"
hold_env=(
  "PATH=$tools:$PATH"
  "HOME=$hold_dir/home"
  "TMPDIR=${TMPDIR:-/tmp}"
  "AGENT_SECRETS_URL=${OUTSIDE_TAILNET_BROKER_URL:-}"
  "AGENT_SECRETS_MACHINE_IDENTITY=device-login"
  "AGENT_SECRETS_DEVICE_LOGIN_FILE=$hold_file"
  "AGENT_SECRETS_PROJECTS=$project"
)
hold_code=''
hold_problem=$helper_problem
dispatch_cli="$here/../../../envoy-client/bin/dispatch.ts"
[ -n "$hold_problem" ] || hold_problem=$(tool_problem bun)
[ -n "$hold_problem" ] || [ -f "$dispatch_cli" ] || hold_problem="no dispatch CLI at $dispatch_cli"
if [ -z "$hold_problem" ]; then
  (
    env -i "${hold_env[@]}" bun "$dispatch_cli" login --device --person "$person" \
      >"$work/hold.log" 2>&1 </dev/null
    echo "$?" >"$work/hold.exit"
  ) &
  background="$background $!"
  deadline=$(($(now) + 60))
  while [ -z "$hold_code" ] && [ "$(now)" -lt "$deadline" ] && [ ! -f "$work/hold.exit" ]; do
    while IFS= read -r line; do
      case $line in *'and enter the code '*) hold_code=${line##*and enter the code } ;; esac
    done <"$work/hold.log"
    [ -n "$hold_code" ] || sleep 1
  done
  [ -n "$hold_code" ] || hold_problem="dispatch login --device printed no code within 60 s: $(text_of "$work/hold.log")"
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
    OUTSIDE_TAILNET_USER_CODE=$code OUTSIDE_TAILNET_HOLD_CODE=$hold_code OUTSIDE_PEER_SESSION=$peer \
      OUTSIDE_PUBLIC_RESULTS=$results \
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
    [ -z "$hold_code" ] || fields+=(-f "tailnet_hold_code=$hold_code")
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
  while [ ! -f "$work/login.exit" ] && [ "$(now)" -lt "$deadline" ]; do sleep 2; done
  exited=$(cat "$work/login.exit" 2>/dev/null)
  [ ! -f "$scratch/machine.json" ] || credential=$(json_field credential_id "$(tr -d ' \n' <"$scratch/machine.json")")
  listed=$(env -i "${scratch_env[@]}" agent-secrets machine list --json 2>&1 </dev/null)
  person_lower=$(printf '%s' "$person" | tr '[:upper:]' '[:lower:]')
  if [ -z "$exited" ]; then
    row T1 "$want_t1" "code $code was not approved within 16 minutes" UNKNOWN
  elif [ "$exited" -ne 0 ]; then
    row T1 "$want_t1" "machine login exited $exited: $(text_of "$work/login.log")" FAIL
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

# length.sh NAME: asks for NAME and prints the request's exit and the length of its value, never
# the value itself.
cat >"$work/length.sh" <<'SH'
#!/bin/sh
agent-secrets request "$1" >/dev/null 2>&1
echo "request-exit $?"
agent-secrets "$1" -- sh -c 'printenv "$0" | tr -d "\n" | wc -c' "$1" 2>/dev/null
SH
chmod 700 "$work/length.sh"

# --- T2: the scratch helper's enrollment is refused, and the canary reaches nobody through it. ---
want_t2="an enrollment through the scratch helper refused 403 RESTRICTED_MEMBER_ENROLLS_NOTHING; ${canary:-<OUTSIDE_CANARY>} asked for through it gets no value"
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
  lengths=$(env -i "${scratch_env[@]}" agent-secrets register --session-id "outside-e2e-t2b-$nonce" \
    --exec -- sh "$work/length.sh" "$canary" 2>/dev/null </dev/null | tr '\n' ' ')
  length=${lengths##*request-exit }
  length=${length#* }
  length=${length// /}
  case $said in
    *enrollment_id*)
      row T2 "$want_t2" "the scratch helper's session enrolled: ${said:0:300}" FAIL
      ;;
    *)
      case $refusal in
        *'403 RESTRICTED_MEMBER_ENROLLS_NOTHING'*)
          if [ -n "$length" ] && [ "$length" != 0 ]; then
            row T2 "$want_t2" "refused, and yet the canary reached the session: a value $length long" FAIL
          else
            row T2 "$want_t2" "refused by that code, and whoami inside reads no enrollment; the canary asked for through it: ${lengths% } and no value" PASS
          fi
          ;;
        *MEMBER_KIND_UNCHECKABLE*)
          row T2 "$want_t2" "the broker could not ask Dispatch the member kind: ${refusal:0:300}" UNKNOWN
          ;;
        *) row T2 "$want_t2" "no RESTRICTED_MEMBER_ENROLLS_NOTHING: ${refusal:0:300}; the canary asked for through it: ${lengths% }" FAIL ;;
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
  lengths=$(agent-secrets register --session-id "outside-e2e-t3-$nonce" --wait 30 \
    --exec -- sh "$work/length.sh" "$canary" 2>"$work/t3.err" </dev/null | tr '\n' ' ')
  requested=${lengths#*request-exit }
  requested=${requested%% *}
  length=${lengths##*request-exit }
  length=${length#* }
  length=${length// /}
  case $requested:$length in
    0:64) row T3 "$want_t3" "granted at once, a value 64 long" PASS ;;
    0:*[0-9]*) row T3 "$want_t3" "granted, a value $length long" FAIL ;;
    75:*) row T3 "$want_t3" "the request waits for a person's approval (exit 75), not granted at once" FAIL ;;
    77:*) row T3 "$want_t3" "the request was denied (exit 77)" FAIL ;;
    *) row T3 "$want_t3" "request exit ${requested:-none}, length ${length:-none}: $(text_of "$work/t3.err")" UNKNOWN ;;
  esac
fi

# --- The public leg's end, its TSV and its revocations. ---
if [ -z "$public_problem" ] && [ -n "$run_id" ]; then
  say "waiting for run $run_id"
  gh run watch "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --interval 30 >/dev/null 2>&1 </dev/null
  gh run download "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" -n outside-agent-public -D "$results" >/dev/null 2>&1 </dev/null
  gh run download "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" -n outside-edge-revocations -D "$results" >/dev/null 2>&1 </dev/null
  conclusion=$(gh run view "$run_id" -R "$OUTSIDE_WORKFLOW_REPO" --json url,conclusion --jq '.url + " " + .conclusion' 2>&1 </dev/null)
  say "run $run_id: $conclusion"
elif [ -z "$public_problem" ]; then
  say "waiting for the public leg's command"
  deadline=$(($(now) + 3600))
  while [ ! -f "$work/public.exit" ] && [ "$(now)" -lt "$deadline" ]; do sleep 5; done
  [ -f "$work/public.exit" ] || public_problem="the public leg's command had not ended within an hour"
fi
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

# --- T5, last and operator-prompted: the owner removes the test person from the access list, and
# the test person's agent stops. This box reads the part a developer box can: the hold login's
# trades and its socket. The test person's browser (its cookie, its passkey sign-in) lives on the
# public runner and has ended by now, so those parts are unread here and the row is UNKNOWN even
# when every part read here passes. ---
want_t5="once the owner removes ${person:-<OUTSIDE_PERSON>} from ${project:-<project>}'s access list: the test person's cookie gets 401 at once and their passkey sign-in fails (the public runner's), a fresh trade of their live device login is refused, and a relay socket opened with the token traded at the removal is closed, each within 325 s"
# hold_probe COMMAND...: the probe as the hold login's own session, at the edge listener.
hold_probe() {
  env -i "${hold_env[@]}" OUTSIDE_PROBE_SESSION="outside-e2e-t5-$nonce" \
    OUTSIDE_LISTENER_URL="${OUTSIDE_LISTENER_URL:-}" OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" \
    bun "$here/outside-agent-probe.ts" "$@" 2>>"$work/probe.log" </dev/null
}
# listed_now: whether the person is on the project's access list, read with this box's own
# credential at Dispatch's private origin: true, false, or what the read printed.
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
problem=$(lacking OUTSIDE_TAILNET_DISPATCH_URL OUTSIDE_LISTENER_URL)
if [ -n "$problem" ]; then
  row T5 "$want_t5" "$problem" UNKNOWN
elif [ -n "$hold_problem" ]; then
  row T5 "$want_t5" "not run: there is no hold login: $hold_problem" UNKNOWN
elif [ "$(cat "$work/hold.exit" 2>/dev/null)" != 0 ] || [ ! -f "$hold_file" ]; then
  row T5 "$want_t5" "not run: the hold login (code $hold_code) was not approved: $(text_of "$work/hold.log")" UNKNOWN
elif [ -z "${OUTSIDE_OWNER_REMOVE_COMMAND:-}" ] && ! { : </dev/tty; } 2>/dev/null; then
  row T5 "$want_t5" "not run: there is no terminal to ask the operator for the removal on, and no OUTSIDE_OWNER_REMOVE_COMMAND" UNKNOWN
else
  before=$(listed_now)
  control=$(hold_probe trade)
  if [ "$before" != true ]; then
    row T5 "$want_t5" "not run: before the removal $person is not read on the access list ($before), so a refusal would prove nothing" UNKNOWN
  elif [ "$(json_field traded "$control")" != true ]; then
    row T5 "$want_t5" "not run: before the removal the hold login's trade was not answered: ${control:0:200}" UNKNOWN
  else
    if [ -n "${OUTSIDE_OWNER_REMOVE_COMMAND:-}" ]; then
      # A harness that acts as the owner's browser session makes the removal.
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
    say "T5: removal read at $(date -u '+%H:%M:%SZ'); watching for 325 s"
    hold_probe hold 340 >"$work/t5.hold" &
    hold_pid=$!
    background="$background $hold_pid"
    refused_at=''
    refusal=''
    while [ -z "$refused_at" ] && [ "$(now)" -le $((removed_at + 325)) ]; do
      answer=$(hold_probe trade)
      if [ "$(json_field traded "$answer")" = false ]; then
        refused_at=$(now)
        refusal=$answer
      else
        sleep 10
      fi
    done
    wait "$hold_pid" 2>/dev/null
    after=$(listed_now)
    held=$(cat "$work/t5.hold" 2>/dev/null)
    closed_at=$(json_field closed_at "$held")
    close_code=$(json_field code "$held")
    wrong=''
    socket=''
    unread=''
    case $held in
      *'"opened_at"'*)
        if [ "$close_code" = null ] || [ -z "$closed_at" ] || [ "$closed_at" = null ]; then
          wrong="the socket opened at the removal was still open 340 s later"
        elif [ $((closed_at / 1000 - removed_at)) -gt 325 ]; then
          wrong="the socket opened at the removal closed $close_code $((closed_at / 1000 - removed_at)) s after it"
        else
          socket="the socket opened at the removal closed $close_code $((closed_at / 1000 - removed_at)) s after it"
        fi
        ;;
      # A trade already refused opens no socket: the stop came before the socket could.
      *'no broker token'*) socket="no socket could be opened at the removal, its trade already refused" ;;
      # Anything else (no relay socket served, no answer) says nothing of the stop.
      *) unread="the socket part was not read: ${held:0:200}" ;;
    esac
    if [ "$after" = true ]; then
      row T5 "$want_t5" "$person is still on $project's access list, so the removal was not made" UNKNOWN
    elif [ "$after" != false ]; then
      row T5 "$want_t5" "the access list could not be read after the removal: $after" UNKNOWN
    elif [ -z "$refused_at" ]; then
      row T5 "$want_t5" "a trade was still answered 325 s after the removal${wrong:+; $wrong}" FAIL
    elif [ -n "$wrong" ]; then
      row T5 "$want_t5" "$wrong; the trade was refused $((refused_at - removed_at)) s after the removal" FAIL
    elif [ -n "$unread" ]; then
      row T5 "$want_t5" "the trade was refused $((refused_at - removed_at)) s after the removal; $unread; the cookie and passkey parts are the public runner's and were not read" UNKNOWN
    else
      row T5 "$want_t5" "the trade refused $((refused_at - removed_at)) s after the removal (status $(json_field status "$refusal")); $socket; the cookie and passkey parts are the public runner's and were not read. Re-adding $person is the operator's own act" UNKNOWN
    fi
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
  # answering, so any verdict of it without the peer is unproven. A PASS of P5 or P6 rests on a
  # control this leg made: P5's admission on the issue naming the repository, P6's absences on
  # the peer and on a session outside the project that a private listener lists. A FAIL of
  # either is evidence by itself (a refusal missing, a foreign session listed) and stands.
  held_by=''
  case $name:$verdict in
    P4:*) held_by=$peer_problem ;;
    P5:PASS) passed issue || held_by="the issue row did not pass, so no repository is known to resolve to $project" ;;
    P6:PASS) held_by=$peer_problem${foreign_problem:+${peer_problem:+; }$foreign_problem} ;;
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
  else
    row "$name" "$want" "$got" "$verdict"
  fi
done

# --- The gate, against pins read again: a pin that moved between the legs holds it. ---
read_pins
if [ -n "$pins_problem" ]; then
  pins_moved="the pins could not be read again at the end: $pins_problem"
elif [ "$pins_text" != "$pins_at_start" ]; then
  pins_moved="a pin moved during the run: now $(printf '%s' "$pins_text" | tr '\n' ';')"
fi
gate
