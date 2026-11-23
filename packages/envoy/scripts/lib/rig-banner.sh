# shellcheck shell=bash
# packages/envoy/scripts/lib/rig-banner.sh
#
# The readers of a local rig's banner. dev-stack.sh, dev-broker.sh and dev-airlock.sh each print one
# block once they are up, from its first line to its last, and print it once. The log
# a caller keeps holds other text around that block: the build, the processes' own output, and in
# dev-airlock.sh's log dev-broker.sh's whole log. A line there that looks like an export is no
# export of the banner's, and a process that prints `  export X=$(...)` must reach no eval. So each
# reader takes the lines of the banner's own block alone, and only once its last line is written,
# however many writes the block took.
# A log that holds the banner's first line more than once holds a block that is not the rig's, and
# nothing tells the two apart, so such a log is refused: each reader says so, by name, on standard
# error, reads nothing and returns 3. A rig prints each path an export names with printf %q, so an
# export line is read as the shell that sources it reads it: evaluated, in a subshell, rather than
# cut at its "=".
#
# What this does not close: a process that prints a whole fake block, the banner's exact first
# line, its exports and the banner's exact last line, before the rig prints its own banner. A reader
# that reads between the fake block's last line and the real banner's first sees one whole block,
# the fake one, and reads it; once the real banner is written, the log holds the first line twice
# and is refused.
#
# The callers: callout-check.sh, project-secrets-check.sh and scripts/e2e/nats-callout-sessions.sh
# read dev-stack.sh's banner; airlock-check.sh, judge-check.sh and dev-airlock-lease.test.sh read
# dev-broker.sh's and then dev-airlock.sh's; github-profile-check.sh and dev-airlock.sh read
# dev-broker.sh's. lib/rig-banner.test.sh holds each array below to its rig's own banner text.

# Each rig's banner: its first line and its last, each a whole line of the log.
# shellcheck disable=SC2034 # each caller uses the one its rig prints
DEV_STACK_BANNER=("dev-stack: ready." "  Press Ctrl-C to stop every process and drop both databases.")
# shellcheck disable=SC2034
DEV_BROKER_BANNER=("dev-broker: ready." "  and run e2e/read-token-live.ts as its header says.")
# shellcheck disable=SC2034
DEV_AIRLOCK_BANNER=("dev-airlock: ready." "  airlock's stack and the one to export.")

# rig_banner_block <log> <first> <last> prints the lines of the block in <log> that runs from the
# line that is <first> to the next line that is <last>, and nothing while that block is not whole.
# It returns 3, printing nothing, when <log> holds <first> on more than one line.
rig_banner_block() {
  FIRST="$2" LAST="$3" awk '
    $0 == ENVIRON["FIRST"] { firsts++; if (!whole) { inside = 1; block = "" } }
    inside { block = block $0 "\n" }
    inside && $0 == ENVIRON["LAST"] { whole = 1; inside = 0 }
    END { if (firsts > 1) exit 3; if (whole) printf "%s", block }
  ' "$1" 2>/dev/null || [ "$?" -ne 3 ] || return 3
}

# rig_banner_lines <log> <first> <last> is rig_banner_block, and on a refused log it says so.
rig_banner_lines() {
  rig_banner_block "$@" && return 0
  echo "rig-banner: $1 holds the line '$2' more than once, and no rig prints its banner twice, so no banner of it is read" >&2
  return 3
}

# rig_banner_complete <log> <first> <last> is true once the banner's block is whole in <log>, and once
# <log> is refused, so a caller's wait ends there and its read says why it read nothing.
rig_banner_complete() {
  local block
  block="$(rig_banner_block "$@")" || return 0
  [ -n "$block" ]
}

# rig_banner_exports <log> <first> <last> prints the block's `  export NAME=value` lines.
rig_banner_exports() {
  local lines
  lines="$(rig_banner_lines "$@")" || return
  [ -n "$lines" ] || return 0
  printf '%s\n' "$lines" | sed -n 's/^\(  export [A-Z][A-Z_0-9]*=.*\)$/\1/p'
}

# rig_banner_value <log> <first> <last> <NAME> prints the value the block's first export of NAME
# gives, read as the shell that sources its line reads it; nothing when the block exports no NAME,
# or its line does not evaluate.
rig_banner_value() {
  local exports line
  exports="$(rig_banner_exports "$1" "$2" "$3")" || return
  line="$(printf '%s\n' "$exports" | sed -n "s/^  export $4=/$4=/p" | head -n1)"
  [ -n "$line" ] || return 0
  (eval "$line" && printf '%s' "${!4}") 2>/dev/null || true
}

# rig_banner_read <log> <first> <last> sets, in the caller's shell, each NAME the block exports, to
# the value the shell that sources its line would give it, in the block's order, so a later export
# of one NAME wins as it would there. A line that does not evaluate, such as dev-stack.sh's
# `<this process's own session name>` placeholder, is left out. The names are set, not exported, as
# the dev-stack readers have always set them; rig_banner_export also exports each. On a refused log
# it sets nothing and returns 3.
rig_banner_read() {
  local exports banner_line banner_name banner_value
  exports="$(rig_banner_exports "$@")" || return
  while IFS= read -r banner_line; do
    [ -n "$banner_line" ] || continue
    banner_name="${banner_line#  export }"
    banner_name="${banner_name%%=*}"
    banner_value="$( (eval "${banner_line#  export }" && printf '%s' "${!banner_name}") 2>/dev/null)" || continue
    printf -v "$banner_name" '%s' "$banner_value"
    [ -z "${RIG_BANNER_EXPORT:-}" ] || export "${banner_name?}"
  done <<<"$exports"
}

# rig_banner_export <log> <first> <last> is rig_banner_read with each NAME also exported, as
# sourcing the banner's lines would export it: the readers of dev-broker.sh's and dev-airlock.sh's
# banners run the stack's clients with it.
rig_banner_export() {
  RIG_BANNER_EXPORT=1 rig_banner_read "$@"
}
