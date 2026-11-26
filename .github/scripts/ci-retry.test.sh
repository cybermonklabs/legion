#!/usr/bin/env bash
# Tests for `ci-retry.sh`: what the helper does, measured by running it, never read off its
# source. The attempt count is counted from a file each fake command appends to, the sleeps and
# the per-attempt bound are read from the times in that file, and the floor R1 sets — five
# attempts, the fifth starting at least 120 s after the first — is the measured span between the
# first and the fifth line of it. Every number the cases assert is printed beside the assertion,
# so a reader of the log sees the measurement rather than the expectation.
#
# The helper is sourced into this harness, which runs with `set -euo pipefail` on: that is how a
# workflow step uses it, and it is also the proof that a failing attempt does not end the caller.
# The case that fails twice and then succeeds calls `retry_fetch` bare, with no `set +e` around
# it, so a leaked failure would kill this script at its first failed attempt and no summary would
# be printed at all.
#
# It costs about 360 s, almost all of it in two cases that each pay the helper's 150 s of
# sleeps: the five attempts against a command that hangs for ever, which the 120 s floor is
# measured from, and the five against a command that writes an error body and fails, which is how
# the cases below see that none of those bytes reach a caller capturing the value, that the last
# attempt's own body is printed on standard error, and that a body larger than the report prints
# is cut. Those three readings share that one case's sleeps rather than paying them again.
#
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/ci-retry.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
helper="$script_dir/ci-retry.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The one call that matters: sourced into a shell that has errexit and pipefail on, exactly as a
# workflow step sources it.
# shellcheck source=.github/scripts/ci-retry.sh
. "$helper"

# run_retry <arguments...>: calls retry_fetch, leaving its merged output in $out, its exit status
# in $status and its wall time in whole seconds in $elapsed.
run_retry() {
  local start
  start=$(date +%s)
  set +e
  out=$(retry_fetch "$@" 2>&1)
  status=$?
  set -e
  elapsed=$(($(date +%s) - start))
}

# near <actual> <expected> <tolerance>: true/false for check.
near() {
  if [ "$1" -ge "$(($2 - $3))" ] && [ "$1" -le "$(($2 + $3))" ]; then echo true; else echo false; fi
}

# The status the local `timeout` returns when it kills: 124 from GNU coreutils, 143 from BusyBox.
# The helper may only call 124 the bound's own kill, so which one this machine gives decides which
# wording the hanging case expects.
set +e
timeout 1 sleep 5
kill_status=$?
set -e
echo "measured: this machine's \`timeout\` exits $kill_status when it kills"

echo "case: sourcing the helper sets no shell option and adds retry_fetch and ci-log-guard.sh's names alone"
check "errexit is still on in this harness" "$(contains "$(set +o)" '^set -o errexit$')"
check "pipefail is still on in this harness" "$(contains "$(set +o)" '^set -o pipefail$')"
cat > "$work/probe" <<'PROBE'
#!/usr/bin/env bash
# Sources the helper in a shell that has set no option at all, and reports what sourcing it
# changed: the shell's options, and the function names it added.
before_options=$(set +o)
before_functions=$(declare -F | awk '{print $NF}' | sort)
# shellcheck source=/dev/null
. "$1"
after_options=$(set +o)
after_functions=$(declare -F | awk '{print $NF}' | sort)
if [ "$before_options" = "$after_options" ]; then echo "options: unchanged"; else echo "options: changed"; fi
printf 'added: %s\n' "$(comm -13 <(echo "$before_functions") <(echo "$after_functions") | paste -sd,)"
PROBE
chmod +x "$work/probe"
probe_out=$(bash "$work/probe" "$helper")
indent "$probe_out"
check "an option-less shell has the same options after sourcing" "$(contains "$probe_out" '^options: unchanged$')"
# ci-retry.sh sources ci-log-guard.sh, the file beside it, so it adds that file's functions and its
# own one. The library's names are read off the library by the same probe, so this pins none of
# them; ci_log_block_open, which retry_fetch calls, must be among them.
library_added=$(bash "$work/probe" "$script_dir/ci-log-guard.sh" | sed -n 's/^added: //p')
check "the library adds ci_log_block_open among its functions" "$(contains "$library_added" '\(^\|,\)ci_log_block_open\(,\|$\)')"
check "and the helper adds exactly the library's functions and one of its own, retry_fetch" \
  "$(contains "$probe_out" "^added: $( { tr ',' '\n' <<<"$library_added"; echo retry_fetch; } | sort | paste -sd,)\$")"

echo "case: a command that succeeds on the first attempt returns 0 and sleeps nothing"
run_retry "a tool already on disk" 5 -- true
echo "  measured: exit $status after ${elapsed}s"
check "exits 0" "$(is "$status" 0)"
check "costs no sleep" "$([ "$elapsed" -lt 3 ] && echo true || echo false)"
check "prints no failure line" "$(is "$(contains "$out" 'attempt')" false)"
check "and no error annotation" "$(is "$(contains "$out" '::error::')" false)"

echo "case: a command that fails twice and then succeeds makes three attempts, 10 s and 20 s apart"
cat > "$work/flaky" <<FLAKY
#!/usr/bin/env bash
# Records the time this attempt started, then fails until the third one.
date +%s >> "$work/flaky.starts"
[ "\$(wc -l < "$work/flaky.starts")" -ge 3 ]
FLAKY
chmod +x "$work/flaky"
: > "$work/flaky.starts"
flaky_start=$(date +%s)
# Called bare, under this harness's errexit and pipefail: if a failed attempt leaked out of the
# helper, this script would end here and print no summary. CI_RETRY_ATTEMPT_LOG names the file
# the download-retry job's reports read which attempt passed from.
CI_RETRY_ATTEMPT_LOG="$work/flaky.attempts" retry_fetch "the twice-failing tool" 5 -- "$work/flaky" > "$work/flaky.log" 2>&1
flaky_elapsed=$(($(date +%s) - flaky_start))
flaky_out=$(cat "$work/flaky.log")
mapfile -t flaky_starts < "$work/flaky.starts"
indent "$flaky_out"
check "the caller's shell survived two failed attempts under set -e" true
check "exactly three attempts ran" "$(is "${#flaky_starts[@]}" 3)"
if [ "${#flaky_starts[@]}" -eq 3 ]; then
  echo "  measured: 3 attempts over ${flaky_elapsed}s, the second $((flaky_starts[1] - flaky_starts[0]))s after the first and the third $((flaky_starts[2] - flaky_starts[1]))s after the second"
  check "the first sleep was 10 s" "$(near "$((flaky_starts[1] - flaky_starts[0]))" 10 2)"
  check "the second was 20 s" "$(near "$((flaky_starts[2] - flaky_starts[1]))" 20 2)"
fi
check "the whole call took about 30 s" "$(near "$flaky_elapsed" 30 3)"
check "it named the first failure, its status and what was left" \
  "$(contains "$flaky_out" '^the twice-failing tool: attempt 1 of 5 failed (exit 1); 4 attempts left$')"
check "and the second" \
  "$(contains "$flaky_out" '^the twice-failing tool: attempt 2 of 5 failed (exit 1); 3 attempts left$')"
check "with CI_RETRY_ATTEMPT_LOG naming a file, each attempt appended its number and exit status, the third's 0" \
  "$(is "$(cat "$work/flaky.attempts" 2> /dev/null)" "$(printf 'attempt 1 exit 1\nattempt 2 exit 1\nattempt 3 exit 0')")"

echo "case: an attempt log that cannot be written changes nothing else"
# Called bare too, with no `||` that would suspend errexit inside the helper: a line that cannot
# be appended must not end a caller that has errexit on, and the call must still return the
# command's own status, 0, which this harness's errexit reads. A leak of either would end this
# script here, with no summary printed.
CI_RETRY_ATTEMPT_LOG="$work/no-such-directory/attempts" retry_fetch "a tool whose attempt log cannot be written" 5 -- true \
  2> "$work/unwritable.err"
indent "$(cat "$work/unwritable.err")"
check "the caller's shell survived the refused line under set -e, so the call returned 0" true
check "and bash's own refusal of the line is on standard error" \
  "$(contains "$(cat "$work/unwritable.err")" 'no-such-directory/attempts: No such file or directory')"

echo "case: the helper's own lines go to standard error, so a caller may capture the command's output"
# A caller that reads a download into a variable — `names=$(retry_fetch … -- gh api …)` — gets the
# command's own output and nothing of the helper's, or every such call site would have to strip
# the retry log out of the value it just fetched.
cat > "$work/noisy" <<NOISY
#!/usr/bin/env bash
date +%s >> "$work/noisy.starts"
if [ "\$(wc -l < "$work/noisy.starts")" -lt 2 ]; then exit 7; fi
echo "the payload"
NOISY
chmod +x "$work/noisy"
: > "$work/noisy.starts"
captured=$(retry_fetch "the noisy tool" 5 -- "$work/noisy" 2> "$work/noisy.err")
noisy_err=$(cat "$work/noisy.err")
indent "$noisy_err"
check "standard output carried the command's output alone" "$(is "$captured" 'the payload')"
check "and the failed attempt was reported on standard error" \
  "$(contains "$noisy_err" '^the noisy tool: attempt 1 of 5 failed (exit 7); 4 attempts left$')"
check "and printed no error annotation, since it succeeded" "$(is "$(contains "$flaky_out" '::error::')" false)"

# The payload a failed attempt writes in the three cases below, and its size in bytes, measured
# here rather than written out: gh copies an error answer's body to standard output as it copies
# a good one's, and this is the shape a refused `gh api` call prints there (gh 2.98.0, a 422).
# LEAKED-MARKER is what a leak looks like in a value the caller captured.
leak_payload='{"message":"LEAKED-MARKER","status":"422"}'
leak_bytes=$(printf '%s\n' "$leak_payload" | wc -c | tr -d ' ')
echo "measured: a failed attempt writes $leak_bytes bytes in the cases below"

echo "case: a failed attempt that wrote to standard output has those bytes withheld"
cat > "$work/leaky" <<LEAKY
#!/usr/bin/env bash
# Writes an error body and fails, as a refused \`gh api\` call does, until the second attempt.
date +%s >> "$work/leaky.starts"
if [ "\$(wc -l < "$work/leaky.starts")" -lt 2 ]; then
  echo '$leak_payload'
  exit 1
fi
echo "the good answer"
LEAKY
chmod +x "$work/leaky"
: > "$work/leaky.starts"
leaked=$(retry_fetch "the leaky tool" 5 -- "$work/leaky" 2> "$work/leaky.err")
leaky_err=$(cat "$work/leaky.err")
indent "$leaky_err"
check "the captured value is the successful attempt's output alone" "$(is "$leaked" 'the good answer')"
check "the failed attempt's bytes are not in front of it" \
  "$(is "$(contains "$leaked" 'LEAKED-MARKER')" false)"
check "and they reached standard error no more than standard output" \
  "$(is "$(contains "$leaky_err" 'LEAKED-MARKER')" false)"
check "standard error says how many bytes were withheld" \
  "$(contains "$leaky_err" "^the leaky tool: attempt 1 of 5 wrote $leak_bytes bytes to standard output; none of it is printed\$")"

echo "case: every attempt writes and fails, so nothing of any attempt reaches standard output, and the last attempt's body is printed and cut"
# 500 bytes more than the helper prints, so the whole is over the cut and the report must name
# both figures.
refusing_filler=$((4096 + 500 - ${#leak_payload}))
refusing_bytes=$((${#leak_payload} + refusing_filler + 1))
# Its body is larger than the helper prints, so this one case proves the cut as well as the
# withholding and pays the helper's 150 s of sleeps once rather than twice. The marker is at the
# front, where a cut cannot drop it.
cat > "$work/refusing" <<REFUSING
#!/usr/bin/env bash
date +%s >> "$work/refusing.starts"
printf '%s' '$leak_payload'
head -c $refusing_filler /dev/zero | tr '\\0' 'x'
echo
exit 1
REFUSING
chmod +x "$work/refusing"
: > "$work/refusing.starts"
set +e
refusing_out=$(retry_fetch "the refusing tool" 5 -- "$work/refusing" 2> "$work/refusing.err")
refusing_status=$?
set -e
mapfile -t refusing_starts < "$work/refusing.starts"
refusing_err=$(cat "$work/refusing.err")
indent "$refusing_err"
check "exactly five attempts ran" "$(is "${#refusing_starts[@]}" 5)"
check "standard output is empty" "$(is "$refusing_out" '')"
check "the four attempts a retry follows said what they withheld and printed none of it" \
  "$(is "$(grep -c "wrote $refusing_bytes bytes to standard output; none of it is printed" <<<"$refusing_err" || true)" 4)"
# The last attempt is the diagnosis: nothing retries after it, so its body is printed - on
# standard error, where it cannot reach a caller capturing this call's value. An apt outage read
# `677 bytes, none printed` on every one of fifteen attempts across three jobs of this
# repository's own CI and said nothing about which
# host or error (dispatch://INFRA-1545).
check "the last attempt's bytes are printed, on standard error" \
  "$(is "$(contains "$refusing_err" 'LEAKED-MARKER')" true)"
check "and it says it is printing them, naming the whole size and the part it prints" \
  "$(contains "$refusing_err" "^the refusing tool: the last attempt wrote $refusing_bytes bytes to standard output; the first 4096 of them follow on standard error\$")"
check "and it prints that many bytes of them and no more" \
  "$(is "$(grep -c "^{\"message\":\"LEAKED-MARKER\",\"status\":\"422\"}x\{$((4096 - ${#leak_payload}))\}\$" <<<"$refusing_err" || true)" 1)"
check "standard output still carries nothing of them" \
  "$(is "$(contains "$refusing_out" 'LEAKED-MARKER')" false)"
# Those bytes are a host's, and the runner reads its workflow commands on standard error as it
# reads them on standard output, so the copy goes inside the last attempt's stop-commands block,
# before its close: a body writing `::add-mask::` or `::error::` of its own is text there. Each of
# the five attempts runs inside a block of its own, so five blocks open, each with its own token.
refusing_tokens=$(sed -n 's/^::stop-commands::\(.*\)$/\1/p' <<<"$refusing_err")
check "each of the five attempts opens a stop-commands block" "$(is "$(grep -c . <<<"$refusing_tokens")" 5)"
# Not a pattern a hard-coded token would match: each token is 32 hex characters ci_log_token read
# from /dev/urandom, so what makes it unguessable is that it differs per attempt and per call, and
# the case below drives a second call to read a second one.
check "each token is 32 lower-case hex characters" \
  "$(is "$(grep -c '^[0-9a-f]\{32\}$' <<<"$refusing_tokens" || true)" 5)"
check "and no two of the five are the same" "$(is "$(sort -u <<<"$refusing_tokens" | grep -c .)" 5)"
refusing_stop=$(tail -n 1 <<<"$refusing_tokens")
check "the last block is closed again with that same token" \
  "$(contains "$refusing_err" "^::$refusing_stop::\$")"
check "the body sits inside that block" \
  "$(is "$(awk -v s="::stop-commands::$refusing_stop" -v e="::$refusing_stop::" '$0==s{i=1;next} $0==e{i=0} i&&/LEAKED-MARKER/{n++} END{print n+0}' "$work/refusing.err")" 1)"
check "it returns the last attempt's status" "$(is "$refusing_status" 1)"
check "and its last line is still the exhaustion annotation" \
  "$(is "$(tail -n 1 <<<"$refusing_err")" '::error::the refusing tool failed to download after 5 attempts (last exit 1)')"

echo "case: a command that fails at once on every attempt fails about 150 s after its first, naming the download"
# The refusing tool above fails at once on each of its five attempts, so the span from its first
# start to its fifth is the helper's four sleeps and nothing else: 10 + 20 + 40 + 80 = 150 s.
# Read off the start times that case wrote, so this one adds no seconds to the suite. A start
# that case did not write reads as 0 here, so a missing attempt fails the span rather than
# skipping it.
refusing_span=$((${refusing_starts[4]:-0} - ${refusing_starts[0]:-0}))
echo "  measured: ${#refusing_starts[@]} attempts, the fifth starting ${refusing_span}s after the first, exit $refusing_status"
check "the fifth attempt started 150 s after the first" "$(near "$refusing_span" 150 2)"
check "it returns 1, the last attempt's status" "$(is "$refusing_status" 1)"
check "and its last line is the error annotation, naming the download and the attempts" \
  "$(is "$(tail -n 1 <<<"$refusing_err")" '::error::the refusing tool failed to download after 5 attempts (last exit 1)')"

echo "case: a hostile body stays inside the stop block, under each caller's own shell options"
# Round 22 of dispatch://INFRA-1545 caught a branch that passed when driven bare and KILLED THE
# CALLER when driven under the flags a caller really sets, so this case drives the helper under
# both: `bash --noprofile --norc -eo pipefail`, which is GitHub's `shell: bash` for every
# composite action, and `dash -eu`, which scripts/go-third-party-notices.sh uses. dash sets no
# BASH_SOURCE, so its caller sources ci-log-guard.sh before the helper, as that script does.
# `sleep` is stubbed on PATH, so five attempts cost no seconds and this case adds nothing to the
# suite's 362 s.
mkdir -p "$work/stub"
cat > "$work/stub/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$work/stub/sleep"
# A body of exactly the lines the runner would act on, and no final newline.
cat > "$work/hostile" <<'HOSTILE'
#!/usr/bin/env bash
printf '::add-mask::x\n::error::x\n::stop-commands::deadbeef\n::::\n::deadbeef::\nHOSTILE-END'
exit 1
HOSTILE
chmod +x "$work/hostile"
for caller_shell in "bash --noprofile --norc -eo pipefail" "dash -eu"; do
  caller_name=${caller_shell%% *}
  if ! command -v "$caller_name" > /dev/null; then
    echo "  $caller_name is not on PATH, so that half is not driven"
    continue
  fi
  # dash sets no BASH_SOURCE, by which the helper finds ci-log-guard.sh, so a dash caller sources
  # the library first.
  if [ "$caller_name" = dash ]; then library='. "$1/ci-log-guard.sh"'; else library=:; fi
  # `set +e` as the cases above do: this call is meant to fail, and a bare failing command would
  # end this harness before it could read what the call printed.
  set +e
  PATH="$work/stub:$PATH" $caller_shell -c "$library"'
    . "$1/ci-retry.sh"
    retry_fetch "the hostile tool" 5 -- "$2"
  ' _ "$script_dir" "$work/hostile" > "$work/hostile.out" 2> "$work/hostile.err"
  hostile_status=$?
  set -e
  hostile_err=$(cat "$work/hostile.err")
  # The helper's blocks, one per attempt, each opened with a token of 32 lower-case hex
  # characters; the body's own `::stop-commands::deadbeef` is not one. The last, the fifth
  # attempt's, holds the copy of its standard output.
  hostile_tokens=$(sed -n 's/^::stop-commands::\([0-9a-f]\{32\}\)$/\1/p' <<<"$hostile_err")
  hostile_stop=$(tail -n 1 <<<"$hostile_tokens")
  check "$caller_name: it returns the command's own status rather than a helper's" \
    "$(is "$hostile_status" 1)"
  check "$caller_name: the exhaustion annotation is still printed" \
    "$(is "$(tail -n 1 <<<"$hostile_err")" '::error::the hostile tool failed to download after 5 attempts (last exit 1)')"
  check "$caller_name: five blocks open, each with a 32-character hex token of its own" \
    "$(is "$(sort -u <<<"$hostile_tokens" | grep -c .)" 5)"
  check "$caller_name: and it is not the token the case above read" \
    "$(is "$([ "$hostile_stop" = "$refusing_stop" ] && echo same || echo different)" different)"
  # Every line the runner would act on sits between the two markers, the body's own
  # `::deadbeef::` included, and the close is on a line of its own although the body ended
  # without a newline.
  check "$caller_name: each workflow command of the body is inside the block" \
    "$(is "$(awk -v s="::stop-commands::$hostile_stop" -v e="::$hostile_stop::" 'index($0, s) == 1 && length($0) == length(s) {i = 1; next} index($0, e) == 1 && length($0) == length(e) {i = 0; next} i && /^::/ {n++} END {print n + 0}' "$work/hostile.err")" 5)"
  check "$caller_name: the close stands on its own line after a body with no final newline" \
    "$(contains "$hostile_err" "^::$hostile_stop::\$")"
  check "$caller_name: the helper's own annotation is outside the block" \
    "$(is "$(awk -v e="::$hostile_stop::" 'index($0, e) == 1 && length($0) == length(e) {after = 1; next} after && /^::error::/ {n++} END {print n + 0}' "$work/hostile.err")" 1)"
  check "$caller_name: and nothing of it reached standard output" \
    "$(is "$(cat "$work/hostile.out")" '')"
done

# A stand-in client that writes, on standard output and on standard error, the three forms of a
# server's text the runner acts on (dispatch://INFRA-5224/artifact/server-text-audit-md): a
# command after a lone carriage return, one after a tab, which the runner removes with leading
# spaces, and the older `##[<command>]` form in the middle of a line. No final newline on either
# stream, so a close that did not start a line of its own would be seen. Its one argument is its
# exit status. `sleep` is the stub above, so its five failed attempts cost no seconds.
printf -v planted 'x\r::warning::planted-cr\n\t::warning::planted-tab\nsaid ##[warning]planted-legacy'
cat > "$work/planter" <<'PLANTER'
#!/usr/bin/env bash
form='x\r::warning::planted-cr\n\t::warning::planted-tab\nsaid ##[warning]planted-legacy'
printf "$form"
printf "$form" >&2
exit "$1"
PLANTER
chmod +x "$work/planter"
# commands_outside_blocks <log>: each line of <log> the runner would act on that no block covers,
# by the runner's rule (actions/runner v2.338.2, ci-log-guard.sh's header): the log split at CR
# and LF, as StreamReader.ReadLine splits it, a line is a command when, its leading spaces and
# tabs removed, it starts with `::`, or when it holds `##[`, a word and `]` anywhere. Such a line
# passes when it is a block's open (`::stop-commands::` and 32 hex characters), the close of the
# block open (`::<its token>::`), any line inside an open block, or the helper's own exhaustion
# `::error::` outside one. A block still open at the end is named too: every line after it would
# be text the runner skips, the step's own commands included.
commands_outside_blocks() {
  tr '\r' '\n' < "$1" | awk '
    { line = $0; sub(/^[ \t]+/, "", line) }
    open != "" { if (line == "::" open "::") open = ""; next }
    line ~ /^::stop-commands::[0-9a-f]+$/ && length(line) == 49 { open = substr(line, 18); next }
    line ~ /^::error::.* failed to download after 5 attempts \(last exit [0-9]+\)$/ { next }
    line ~ /^::/ || line ~ /##\[[A-Za-z][^]]*\]/ { print }
    END { if (open != "") print "a block left open: " open }
  '
}
# planted_lines <log>: how many lines of <log>, split at CR and LF, are one of the three forms. The
# legacy form is matched anywhere in its line, as the runner reads it: where a successful copy goes
# to standard error, the command's standard output follows its standard error's last line, which
# has no newline, on the same line.
planted_lines() {
  tr '\r' '\n' < "$1" |
    { grep -c -e '^::warning::planted-cr$' -e $'^\t::warning::planted-tab$' -e '##\[warning\]planted-legacy' || true; }
}

echo "case: a server's text on either stream of an attempt that fails every time reaches the log as no command"
# The log is standard error: what the caller captures here never reaches it. It holds each
# attempt's standard error and the last attempt's standard output, 5 x 3 + 3 lines of the forms.
set +e
PATH="$work/stub:$PATH" retry_fetch "the planting tool" 5 -- "$work/planter" 1 > "$work/planted-fail.out" 2> "$work/planted-fail.err"
planted_status=$?
set -e
check "failing: it returns the command's status" "$(is "$planted_status" 1)"
check "failing: the log holds the three forms of five attempts' standard error and the last one's standard output" \
  "$(is "$(planted_lines "$work/planted-fail.err")" 18)"
check "failing: split as the runner splits it, no line of the log is a command outside a block" \
  "$(is "$(commands_outside_blocks "$work/planted-fail.err")" '')"
check "failing: and nothing reached standard output" "$(is "$(cat "$work/planted-fail.out")" '')"

echo "case: a server's text on either stream of a first attempt that succeeds, its value captured, reaches the log as no command"
planted_value=$(PATH="$work/stub:$PATH" retry_fetch "the planting tool" 5 -- "$work/planter" 0 2> "$work/planted-captured.err")
check "captured: the value is the command's standard output byte for byte" "$(is "$planted_value" "$planted")"
check "captured: and holds no marker" \
  "$(is "$(grep -c -e 'stop-commands' -e '^::[0-9a-f]\{32\}::$' <<<"$planted_value" || true)" 0)"
check "captured: the log holds the three forms of the attempt's standard error" \
  "$(is "$(planted_lines "$work/planted-captured.err")" 3)"
check "captured: split as the runner splits it, no line of the log is a command outside a block" \
  "$(is "$(commands_outside_blocks "$work/planted-captured.err")" '')"

echo "case: a server's text on either stream of a first attempt that succeeds, sent with >&2, reaches the log as no command"
# `>&2` after `2>` points standard output at the same file, as a caller's `retry_fetch … >&2`
# points it at the step's standard error, so the log is both streams.
PATH="$work/stub:$PATH" retry_fetch "the planting tool" 5 -- "$work/planter" 0 2> "$work/planted-log.err" >&2
check "sent to standard error: the log holds the three forms of both streams" \
  "$(is "$(planted_lines "$work/planted-log.err")" 6)"
check "sent to standard error: split as the runner splits it, no line of the log is a command outside a block" \
  "$(is "$(commands_outside_blocks "$work/planted-log.err")" '')"

echo "case: under dash, with ci-log-guard.sh sourced first, a failed and a successful attempt are guarded as under bash"
# scripts/go-third-party-notices.sh (#!/bin/sh) and dispatch-browser.yaml's shard and alone jobs
# (`sh -e {0}`) source the helper under dash, which sets no BASH_SOURCE, so each sources
# ci-log-guard.sh first. `-eu` as the notices script runs; `||` keeps the failed call from ending
# the shell before the successful one.
if command -v dash > /dev/null; then
  set +e
  PATH="$work/stub:$PATH" dash -eu -c '
    . "$1/ci-log-guard.sh"
    . "$1/ci-retry.sh"
    retry_fetch "the planting tool" 5 -- "$2" 1 || echo "failed: $?" > "$3/dash.status"
    value=$(retry_fetch "the planting tool" 5 -- "$2" 0)
    echo "succeeded: $?" >> "$3/dash.status"
    printf "%s" "$value" > "$3/dash.value"
  ' _ "$script_dir" "$work/planter" "$work" > "$work/dash.out" 2> "$work/dash.err"
  dash_status=$?
  set -e
  check "dash: the shell ran to its end" "$(is "$dash_status" 0)"
  check "dash: the failed call returned the command's status, the successful one 0" \
    "$(is "$(cat "$work/dash.status" 2> /dev/null)" "$(printf 'failed: 1\nsucceeded: 0')")"
  check "dash: the captured value is the command's standard output byte for byte" \
    "$(cmp -s "$work/dash.value" <(printf '%s' "$planted") && echo true || echo false)"
  check "dash: six blocks open, five failed attempts' and the success's, each token its own" \
    "$(is "$(sed -n 's/^::stop-commands::\([0-9a-f]\{32\}\)$/\1/p' "$work/dash.err" | sort -u | grep -c .)" 6)"
  check "dash: the log holds the three forms of six attempts' standard error and the last failed one's standard output" \
    "$(is "$(planted_lines "$work/dash.err")" 21)"
  check "dash: split as the runner splits it, no line of the log is a command outside a block" \
    "$(is "$(commands_outside_blocks "$work/dash.err")" '')"
  check "dash: and nothing reached standard output" "$(is "$(cat "$work/dash.out")" '')"
else
  echo "  dash is not on PATH, so this case is not driven"
fi

echo "case: under dash, a helper sourced with no ci-log-guard.sh before it refuses by name and stops a sh -e caller"
# dash sets no BASH_SOURCE, so the helper cannot find the library beside it. It says so in one
# `::error::` and the source returns 2, where dash's own refusal would be a bare "can't open".
if command -v dash > /dev/null; then
  set +e
  dash -e -c '. "$1/ci-retry.sh"; echo "the line after the source ran"' _ "$script_dir" > "$work/refused.out" 2> "$work/refused.err"
  refused_status=$?
  set -e
  check "dash: the source returns 2 and the sh -e caller ends there" \
    "$(is "$refused_status|$(cat "$work/refused.out")" "2|")"
  check "dash: its one line is an ::error:: that names the rule" \
    "$(is "$(cat "$work/refused.err")" "::error::ci-retry.sh: ci_log_block_open is not defined and no ci-log-guard.sh can be read beside this file; a caller whose shell has no BASH_SOURCE (dash, sh) must source .github/scripts/ci-log-guard.sh before ci-retry.sh")"
else
  echo "  dash is not on PATH, so this case is not driven"
fi

echo "case: a first attempt that succeeds has its output copied byte for byte"
# The buffer must add nothing and drop nothing, a trailing newline the command did not write
# least of all: a caller writing the value into a file compares it with a checksum.
cat > "$work/exact" <<'EXACT'
#!/usr/bin/env bash
printf 'no trailing newline'
EXACT
chmod +x "$work/exact"
printf 'no trailing newline' > "$work/exact.want"
retry_fetch "the exact tool" 5 -- "$work/exact" > "$work/exact.got" 2> "$work/exact.err"
echo "  measured: $(wc -c < "$work/exact.want" | tr -d ' ') bytes written, $(wc -c < "$work/exact.got" | tr -d ' ') bytes captured"
check "the capture is byte for byte what the command wrote, with no newline added" \
  "$(cmp -s "$work/exact.want" "$work/exact.got" && echo true || echo false)"
# Nothing but the attempt's block, which a success opens and closes as a failure does: its open,
# the newline before its close, and its close. The copy went to a file, not to standard error, so
# it is after the close and outside the block.
check "and the helper printed nothing beside its block" \
  "$(is "$(sed 's/[0-9a-f]\{32\}/<token>/g' "$work/exact.err")" "$(printf '::stop-commands::<token>\n\n::<token>::')")"

echo "case: a copy the caller's standard output refuses is the call's status, not a silent 0"
# The successful attempt's buffer is copied with `cat`, and that copy can fail where the fetch
# did not: a full device, an I/O error, a reader that closed the pipe. The caller then holds a
# cut answer, so the call has to say so. /dev/full answers every write with ENOSPC, which is the
# cheapest of the three to drive; the command itself writes into the buffer and succeeds.
set +e
retry_fetch "the tool whose output a full device refuses" 5 -- printf payload > /dev/full 2> "$work/full.err"
full_status=$?
set -e
full_err=$(cat "$work/full.err")
indent "$full_err"
echo "  measured: exit $full_status"
check "the call fails rather than returning 0" "$([ "$full_status" -ne 0 ] && echo true || echo false)"
check "and returns the status cat gave" "$(is "$full_status" 1)"
check "cat's own message is on standard error" "$(contains "$full_err" 'No space left on device')"
check "no attempt is reported as failed, since the fetch itself worked" \
  "$(is "$(contains "$full_err" 'attempt 1 of 5')" false)"
check "and no exhaustion annotation is printed" "$(is "$(contains "$full_err" '::error::')" false)"

echo "case: a command that hangs for ever makes five bounded attempts, the fifth past the 120 s floor"
cat > "$work/hangs" <<HANGS
#!/usr/bin/env bash
# Records the time this attempt started, then hangs until \`timeout\` kills it. exec, so the kill
# reaches the sleep rather than a parent that would leave it running.
date +%s >> "$work/hangs.starts"
exec sleep 600
HANGS
chmod +x "$work/hangs"
: > "$work/hangs.starts"
run_retry "the stalled tool" 2 -- "$work/hangs"
mapfile -t hangs_starts < "$work/hangs.starts"
indent "$out"
check "exactly five attempts ran" "$(is "${#hangs_starts[@]}" 5)"
if [ "${#hangs_starts[@]}" -eq 5 ]; then
  span=$((hangs_starts[4] - hangs_starts[0]))
  gaps=""
  for i in 1 2 3 4; do gaps="$gaps $((hangs_starts[i] - hangs_starts[i - 1]))s"; done
  echo "  measured: attempt starts$gaps apart, ${span}s from the first attempt to the fifth, ${elapsed}s in all, exit $status"
  # Each gap is this attempt's 2 s bound plus the sleep after it. Against a command that would run
  # for 600 s, a gap of 12 s is the attempt ending at its bound and nowhere else.
  check "the first attempt ended at its 2 s bound and was followed by a 10 s sleep" "$(near "$((hangs_starts[1] - hangs_starts[0]))" 12 2)"
  check "the second, by a 20 s sleep" "$(near "$((hangs_starts[2] - hangs_starts[1]))" 22 2)"
  check "the third, by a 40 s sleep" "$(near "$((hangs_starts[3] - hangs_starts[2]))" 42 2)"
  check "the fourth, by an 80 s sleep" "$(near "$((hangs_starts[4] - hangs_starts[3]))" 82 2)"
  check "the fifth attempt started at least 120 s after the first" \
    "$([ "$span" -ge 120 ] && echo true || echo false)"
fi
check "it returns the last attempt's non-zero status" "$(is "$status" "$kill_status")"
if [ "$kill_status" -eq 124 ]; then
  check "each attempt is reported as having reached its bound" \
    "$(is "$(grep -c 'reached its 2s bound (exit 124)' <<<"$out")" 5)"
else
  check "no attempt is reported as having reached its bound, since this timeout exits $kill_status" \
    "$(is "$(grep -c "failed (exit $kill_status)" <<<"$out")" 5)"
fi
check "its last line is the error annotation, naming the download and the attempts" \
  "$(is "$(tail -n 1 <<<"$out")" '::error::the stalled tool failed to download after 5 attempts (last exit '"$kill_status"')')"

echo "case: a malformed call is refused, naming the mistake"
# Two words after the bound: a helper that read the first word as its `--` would run the second
# and exit 0, where with one word it would reach the refusal of nothing to run after `--`, which
# exits 2 as this one does (dispatch://INFRA-1545, round 70).
run_retry "a tool" 5 true true
check "a call with no -- separator is refused" "$(is "$status" 2)"
check "and names the missing --" "$(contains "$out" '^::error::.*--')"
check "it slept nothing" "$([ "$elapsed" -lt 3 ] && echo true || echo false)"

run_retry "a tool"
check "a call with fewer than three arguments is refused" "$(is "$status" 2)"
check "and names the 1 argument it got" "$(contains "$out" '^::error::.*\b1\b')"

run_retry "a tool" 30s -- true
check "a bound that is not a whole number is refused" "$(is "$status" 2)"
check "and names the bound" "$(contains "$out" '^::error::.*30s')"

run_retry "a tool" 0 -- true
check "a bound of zero is refused" "$(is "$status" 2)"
check "and names it" "$(contains "$out" '^::error::.*\b0\b')"

run_retry "a tool" 5 --
check "a call with nothing after -- is refused" "$(is "$status" 2)"
check "and names the --" "$(contains "$out" '^::error::.*--')"

summary "ci-retry.sh"
