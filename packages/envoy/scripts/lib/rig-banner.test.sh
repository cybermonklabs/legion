#!/usr/bin/env bash
# shellcheck disable=SC2016 # each decoy line holds a literal $(...), which only a reader's eval runs
# packages/envoy/scripts/lib/rig-banner.test.sh
#
# Holds lib/rig-banner.sh to what its callers need, on logs alone: no stack, no network.
#
# For each rig's banner array (dev-stack.sh's, dev-broker.sh's, dev-airlock.sh's): its first line and
# its last are whole lines of that rig's own banner, in its script, the first before the last, and
# the last ends the banner's heredoc. Then, in a log that holds lines that look like exports and run
# a command when evaluated, ahead of a banner whole from its first line to its last and after it,
# as a process of the stack's could print them:
#   - rig_banner_read sets each name the banner exports, a path holding a space and a quote and
#     printed through printf %q read whole, a later export of one name winning, a line that does not
#     evaluate left out; it runs neither command, sets no name the banner does not export, and
#     exports nothing;
#   - rig_banner_export sets the same and exports each;
#   - rig_banner_value gives one value, and rig_banner_complete is true.
# A log that holds a whole fake block ahead of the real banner, the banner's first line twice, is
# refused by name: nothing in it is read or run. Two rigs' banners in one log, as dev-airlock.sh's
# log holds dev-broker.sh's and its own, are each read. And a banner whose last line is not written
# yet reads as no banner, nothing in it run.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=packages/envoy/scripts/lib/rig-banner.sh
source "${RIG_BANNER_LIB:-$here/rig-banner.sh}"
work="$(mktemp -d "${TMPDIR:-/tmp}/rig-banner-test.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT

passed=0
failed=0
expect() { # <label> <command...>
  if "${@:2}"; then
    printf 'PASS: %s\n' "$1"
    passed=$((passed + 1))
  else
    printf 'FAIL: %s\n' "$1"
    failed=$((failed + 1))
  fi
}
# not <command...> is true when the command, run in this shell, is false.
not() { ! "$@"; }

# read_in_child <how> <log> <first> <last>: what a shell that read the banner with <how>
# (rig_banner_read or rig_banner_export) holds, one field per name, and what its child sees of NAMED.
read_in_child() {
  (
    unset NAMED PLAIN TWICE PLACEHOLDER DECOY_AHEAD DECOY_AFTER
    "$1" "$2" "$3" "$4"
    printf '%s|%s|%s|%s|%s|%s|child:%s' "${NAMED-unset}" "${PLAIN-unset}" "${TWICE-unset}" \
      "${PLACEHOLDER-unset}" "${DECOY_AHEAD-unset}" "${DECOY_AFTER-unset}" "$(printenv NAMED || echo unset)"
  )
}

for rig in dev-stack dev-broker dev-airlock; do
  case "$rig" in
  dev-stack) banner=("${DEV_STACK_BANNER[@]}") ;;
  dev-broker) banner=("${DEV_BROKER_BANNER[@]}") ;;
  dev-airlock) banner=("${DEV_AIRLOCK_BANNER[@]}") ;;
  esac
  script="$here/../$rig.sh"
  first_at="$(grep -nxF -- "${banner[0]}" "$script" | head -n1 | cut -d: -f1 || true)"
  last_at="$(grep -nxF -- "${banner[1]}" "$script" | head -n1 | cut -d: -f1 || true)"
  expect "$rig: the banner's first line, '${banner[0]}', is a whole line of $rig.sh" test -n "$first_at"
  expect "$rig: the banner's last line, '${banner[1]}', is a whole line of $rig.sh after it" \
    test "${last_at:-0}" -gt "${first_at:-0}"
  expect "$rig: that last line ends the banner's heredoc" test "$(sed -n "$((${last_at:-0} + 1))p" "$script")" = EOF

  dir="$work/$rig/a dir's name"
  mkdir -p "$dir"
  ahead="$work/$rig/an export ahead of the banner ran"
  after="$work/$rig/an export after the banner ran"
  log="$work/$rig/log"
  {
    printf '%s: building\n' "$rig"
    printf '  export DECOY_AHEAD=$(: >%s)\n' "$(printf %q "$ahead")"
    printf '  export NAMED=ahead\n'
    printf '\n%s\n\n' "${banner[0]}"
    printf '  export NAMED=%s\n' "$(printf %q "$dir")"
    printf '  export PLAIN=plain\n'
    printf '  export TWICE=first\n'
    printf "  export PLACEHOLDER=<this process's own session name>\n"
    printf '  export TWICE=second\n'
    printf '\n%s\n' "${banner[1]}"
    printf '  export DECOY_AFTER=$(: >%s)\n' "$(printf %q "$after")"
    printf '  export NAMED=after\n'
  } >"$log"

  want="$dir|plain|second|unset|unset|unset|child:unset"
  expect "$rig: rig_banner_read sets the banner's names, the path whole, and exports none" \
    test "$(read_in_child rig_banner_read "$log" "${banner[@]}")" = "$want"
  want="$dir|plain|second|unset|unset|unset|child:$dir"
  expect "$rig: rig_banner_export sets the same and exports each" \
    test "$(read_in_child rig_banner_export "$log" "${banner[@]}")" = "$want"
  expect "$rig: rig_banner_value gives the banner's NAMED" test "$(rig_banner_value "$log" "${banner[@]}" NAMED)" = "$dir"
  expect "$rig: rig_banner_value of a name the banner does not export is empty" \
    test -z "$(rig_banner_value "$log" "${banner[@]}" DECOY_AHEAD)"
  expect "$rig: rig_banner_complete is true of the whole banner" rig_banner_complete "$log" "${banner[@]}"
  expect "$rig: no export line ahead of the banner was evaluated" test ! -e "$ahead"
  expect "$rig: no export line after the banner was evaluated" test ! -e "$after"
done

# A whole fake block ahead of the real banner: the log holds the banner's first line twice, so each
# reader refuses it by name, returns 3 and reads nothing of it, and the wait for the banner ends.
banner=("${DEV_STACK_BANNER[@]}")
twice="$work/twice.log"
faked="$work/an export of a fake banner block ran"
{
  printf '%s\n' "${banner[0]}"
  printf '  export DECOY_AHEAD=$(: >%s)\n' "$(printf %q "$faked")"
  printf '  export NAMED=fake\n'
  printf '%s\n' "${banner[1]}"
  printf '%s\n' "${banner[0]}"
  printf '  export NAMED=real\n'
  printf '%s\n' "${banner[1]}"
} >"$twice"
# refused <how> <log> <first> <last>: <how> on <log> returns 3, prints nothing on standard output, and
# names the twice-printed first line on standard error.
refused() {
  local out status=0
  out="$("$@" 2>"$work/refused.err")" || status=$?
  [ "$status" -eq 3 ] && [ -z "$out" ] && grep -qF "holds the line '$3' more than once" "$work/refused.err"
}
expect "rig_banner_read refuses a log holding the banner's first line twice, by name" \
  refused rig_banner_read "$twice" "${banner[@]}"
expect "rig_banner_export refuses it too" refused rig_banner_export "$twice" "${banner[@]}"
expect "rig_banner_value refuses it too" refused rig_banner_value "$twice" "${banner[@]}" NAMED
expect "nothing of the fake block was evaluated" test ! -e "$faked"
expect "rig_banner_complete ends the wait on a refused log" rig_banner_complete "$twice" "${banner[@]}"

# Two rigs' banners in one log, as dev-airlock.sh's log holds dev-broker.sh's banner and then its
# own: each first line is there once, so each banner is read, the later one winning.
both="$work/both.log"
{
  printf '%s\n' "${DEV_BROKER_BANNER[0]}" '  export NAMED=broker' '  export PLAIN=broker' "${DEV_BROKER_BANNER[1]}"
  printf '%s\n' "${DEV_AIRLOCK_BANNER[0]}" '  export NAMED=airlock' "${DEV_AIRLOCK_BANNER[1]}"
} >"$both"
expect "two rigs' banners in one log are each read, the later one winning" test "$(
  unset NAMED PLAIN
  rig_banner_read "$both" "${DEV_BROKER_BANNER[@]}"
  rig_banner_read "$both" "${DEV_AIRLOCK_BANNER[@]}"
  printf '%s|%s' "$NAMED" "$PLAIN"
)" = "airlock|broker"

# A banner whose last line is not written yet reads as no banner, and nothing in it runs.
partial="$work/partial.log"
inside="$work/an export of a partial banner ran"
{
  printf '%s\n\n' "${banner[0]}"
  printf '  export PLAIN=plain\n'
  printf '  export DECOY_AHEAD=$(: >%s)\n' "$(printf %q "$inside")"
} >"$partial"
expect "a banner whose last line is not written is not complete" not rig_banner_complete "$partial" "${banner[@]}"
expect "a banner whose last line is not written sets nothing" test "$(read_in_child rig_banner_read "$partial" "${banner[@]}")" = \
  "unset|unset|unset|unset|unset|unset|child:unset"
expect "nothing in a partial banner was evaluated" test ! -e "$inside"

# 3 rigs of 10 rows each and 9 more; a run that held fewer read nothing it should have.
want_rows=39
echo "rig-banner.test.sh: $passed passed, $failed failed of $want_rows"
[ "$failed" -eq 0 ] && [ "$passed" -eq "$want_rows" ]
