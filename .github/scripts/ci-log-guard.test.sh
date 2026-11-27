#!/usr/bin/env bash
# Tests for ci-log-guard.sh: a server's text that a guarded command copies to the log reaches the
# runner as no workflow command, in any of the three forms, from either stream, on a failed
# attempt and on a successful one; the step's own commands around the block still work; the
# command's exit status and a captured standard output come back unchanged.
#
# The log is read by a parser that applies the runner's rules (actions/runner v2.338.2):
#   - a line ends at `\n`, at `\r\n` and at a lone `\r` (StreamReader.ReadLine,
#     src/Runner.Sdk/ProcessInvoker.cs:513);
#   - a line is a command when, after the white space .NET's TrimStart removes, it starts with
#     `::<registered command>…::`, or when its first `##[` starts `##[<registered command>…]`
#     (src/Runner.Common/ActionCommand.cs:51-189), the names compared without case;
#   - `::stop-commands::<token>` stops every command until a line that parses as the command
#     `<token>` (src/Runner.Worker/ActionCommandManager.cs:86-124).
# The runner keeps one stop state for the step and handles the two streams in an order nobody
# promises, so the parser reads each log in four orders, and a case passes only when all four
# agree: each stream with a stop state of its own ("per-stream", where a block on one stream
# covers nothing on the other), all of standard error and then all of standard output under one
# state ("stderr-first", what one batch of ProcessInvoker.cs:417-440 does), the reverse
# ("stdout-first"), and standard output read while standard error's first block is open
# ("stdout-in-block", the order that drops a step's own command written on stdout beside a block).
#
# The controls: the same stand-in client run with no guard shows every planted command in every
# order, so the parser can see each form; a stand-in guard that writes its markers on standard
# output, around a client writing on standard error, shows the client's commands, so the parser
# can see a block on the wrong stream; and a step's own warning written on standard output beside
# a block is dropped in the stdout-in-block order, so the parser can see that loss.
#
# The eight release steps that hold credentials define the guard inline (they run no script of
# this repository): each one's run: text is read out of its workflow, its definition driven with
# the stand-in client as the library is, and each npm publish step run whole against a stand-in
# npm; the control is the same text with the inline guard taken out.
#
# Run from anywhere: .github/scripts/ci-log-guard.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

cat > "$work/parse.py" <<'PY'
"""Reads one step's standard output and standard error as the runner does.

Prints one line for each workflow command the runner would act on, as
`<order>\t<stream>\t<command>\t<data>`, and `<order>\t<stream>\tSTOPPED-AT-END\t<token>` for a
stop that no line of that order resumed.
"""
import sys
import unicodedata

# Every name the runner registers (src/Runner.Worker/ActionCommandManager.cs: each extension's
# Command, and stop-commands itself).
REGISTERED = {
    "internal-set-repo-path", "set-env", "set-output", "save-state", "add-mask", "add-path",
    "add-matcher", "remove-matcher", "debug", "warning", "error", "notice", "group", "endgroup",
    "echo", "stop-commands",
}


def is_white(c):
    # .NET Char.IsWhiteSpace, which String.TrimStart() with no argument removes.
    return c in "\t\n\v\f\r\x85\xa0" or unicodedata.category(c) in ("Zs", "Zl", "Zp")


def lines_of(data):
    text = data.decode("utf-8", errors="replace")
    out, cur, i, n = [], [], 0, len(text)
    while i < n:
        c = text[i]
        if c == "\n":
            out.append("".join(cur))
            cur = []
        elif c == "\r":
            out.append("".join(cur))
            cur = []
            if i + 1 < n and text[i + 1] == "\n":
                i += 1
        else:
            cur.append(c)
        i += 1
    if cur:
        out.append("".join(cur))
    return out


def parse(line, registered):
    s = line
    while s and is_white(s[0]):
        s = s[1:]
    if s.startswith("::"):
        end = s.find("::", 2)
        if end >= 0:
            name = s[2:end].split(" ", 1)[0]
            if name.lower() in registered:
                return name.lower(), s[end + 2:]
    p = line.find("##[")
    if p >= 0:
        rb = line.find("]", p)
        if rb >= 0:
            name = line[p + 3:rb].split(" ", 1)[0]
            if name.lower() in registered:
                return name.lower(), line[rb + 1:]
    return None


class State:
    def __init__(self):
        self.token = None

    def feed(self, order, stream, line, out):
        registered = REGISTERED | ({self.token} if self.token else set())
        cmd = parse(line, registered)
        if cmd is None:
            return
        name, data = cmd
        if self.token is not None:
            if name == self.token:
                self.token = None
            return
        if name == "stop-commands":
            if not data or data.lower() in REGISTERED:
                out.append(f"{order}\t{stream}\tREFUSED-STOP\t{data}")
                return
            self.token = data.lower()
            return
        out.append(f"{order}\t{stream}\t{name}\t{data}")

    def end(self, order, stream, out):
        if self.token is not None:
            out.append(f"{order}\t{stream}\tSTOPPED-AT-END\t{self.token}")


streams = {}
for stream, path in (("stdout", sys.argv[1]), ("stderr", sys.argv[2])):
    with open(path, "rb") as fh:
        streams[stream] = lines_of(fh.read())

out = []
for stream in ("stdout", "stderr"):
    st = State()
    for line in streams[stream]:
        st.feed("per-stream", stream, line, out)
    st.end("per-stream", stream, out)
for order, first, second in (("stderr-first", "stderr", "stdout"), ("stdout-first", "stdout", "stderr")):
    st = State()
    for stream in (first, second):
        for line in streams[stream]:
            st.feed(order, stream, line, out)
    st.end(order, first + "+" + second, out)
# stdout-in-block: standard error up to its first stop, then all of standard output, then the rest
# of standard error, all under one state. A batch hands the parser its standard error lines first
# (ProcessInvoker.cs:417-440), so a stdout line written beside a block can reach it while the block
# is open; the runner's own reader dropped a step's own `::warning::` written on stdout just after
# a block in 32 of 50 runs. A step's own command beside a block survives this order only on stderr.
stops = [i for i, line in enumerate(streams["stderr"]) if (parse(line, REGISTERED) or ("",))[0] == "stop-commands"]
cut = stops[0] + 1 if stops else len(streams["stderr"])
st = State()
for stream, lines in (("stderr", streams["stderr"][:cut]), ("stdout", streams["stdout"]), ("stderr", streams["stderr"][cut:])):
    for line in lines:
        st.feed("stdout-in-block", stream, line, out)
st.end("stdout-in-block", "stderr+stdout", out)
print("\n".join(out))
PY

# parse <stdout file> <stderr file>: the commands the runner would act on, one per line.
parse() { python3 "$work/parse.py" "$1" "$2"; }

# count <text> <regex>: how many lines of the parse match.
count() { grep -c -- "$2" <<<"$1" || true; }

# The stand-in client: what go, gh or crane print when a server answers 504 with a body that
# plants each form, on both streams, and then exits with $1. The last write of each stream has no
# newline, as a body cut short would.
cat > "$work/client" <<'SH'
#!/usr/bin/env bash
for s in out err; do
  fd=1
  [ "$s" = err ] && fd=2
  {
    printf 'GET https://registry.example/pkg: 504 upstream timed out\r::warning::planted-%s-cr\n' "$s"
    printf '\t::warning::planted-%s-tab\n' "$s"
    printf '   ::warning::planted-%s-space\n' "$s"
    printf 'upstream said ##[warning]planted-%s-legacy\n' "$s"
    printf 'body cut short'
  } >&"$fd"
done
exit "$1"
SH
chmod +x "$work/client"

lib="$script_dir/ci-log-guard.sh"

# step <name> <shell body>: runs the body as a run: step does (bash -e, the library sourced), and
# keeps its two streams as the runner receives them, and its exit status.
step() {
  local name=$1 body=$2 status=0
  bash -e -c ". '$lib'; $body" >"$work/$name.out" 2>"$work/$name.err" || status=$?
  echo "$status" >"$work/$name.status"
}

echo "Controls: the parser sees what the runner would act on"

for attempt in 0 7; do
  step "bare-$attempt" "'$work/client' $attempt"
  got=$(parse "$work/bare-$attempt.out" "$work/bare-$attempt.err")
  for order in per-stream stderr-first stdout-first stdout-in-block; do
    for s in out err; do
      for form in cr tab space legacy; do
        check "no guard, exit $attempt, $order: the parser sees planted-$s-$form" \
          "$(is "$(count "$got" "^$order	std$s	warning	planted-$s-$form\$")" 1)"
      done
    done
  done
done

# The stand-in guard on the wrong stream: its markers on standard output, the client's text on
# standard error, where the runner does not keep them in order with the markers.
step wrong-stream "
  t=\$(ci_log_token)
  printf '::stop-commands::%s\n' \"\$t\"
  '$work/client' 0 >/dev/null
  printf '\n::%s::\n' \"\$t\""
got=$(parse "$work/wrong-stream.out" "$work/wrong-stream.err")
for form in cr tab space legacy; do
  check "markers on stdout, text on stderr, per-stream: planted-err-$form leaks" \
    "$(is "$(count "$got" "^per-stream	stderr	warning	planted-err-$form\$")" 1)"
  check "markers on stdout, text on stderr, stderr-first: planted-err-$form leaks" \
    "$(is "$(count "$got" "^stderr-first	stderr	warning	planted-err-$form\$")" 1)"
done

echo
echo "ci_log_guard: the server's text is stopped on both streams"

for attempt in 0 7; do
  step "guard-$attempt" "
    echo '::warning::own-before' >&2
    ci_log_guard '$work/client' $attempt && rc=0 || rc=\$?
    echo '::warning::own-after' >&2
    exit \$rc"
  got=$(parse "$work/guard-$attempt.out" "$work/guard-$attempt.err")
  check "guard, exit $attempt: the parser sees no planted command in any order" \
    "$(is "$(count "$got" planted)" 0)"
  check "guard, exit $attempt: no block is left stopped" \
    "$(is "$(count "$got" STOPPED-AT-END)" 0)"
  for order in per-stream stderr-first stdout-first stdout-in-block; do
    check "guard, exit $attempt, $order: the step's own warning before the block works" \
      "$(is "$(count "$got" "^$order	stderr	warning	own-before\$")" 1)"
    check "guard, exit $attempt, $order: the step's own warning after the block works" \
      "$(is "$(count "$got" "^$order	stderr	warning	own-after\$")" 1)"
  done
  check "guard, exit $attempt: the command's exit status comes back" \
    "$(is "$(cat "$work/guard-$attempt.status")" "$attempt")"
  check "guard, exit $attempt: nothing reaches stdout, so no block can be on it" \
    "$(is "$(wc -c <"$work/guard-$attempt.out")" 0)"
  check "guard, exit $attempt: the client's stdout text is in the log (on stderr)" \
    "$(contains "$(cat "$work/guard-$attempt.err")" 'planted-out-legacy')"
done

# Under bash -e a failing command would end the shell before the close if the guard ran it as a
# plain command; the step's last line runs only if the shell is still there.
step guard-errexit "
  ci_log_guard '$work/client' 9 || true
  ci_log_guard '$work/client' 9
  echo '::warning::never-reached' >&2"
got=$(parse "$work/guard-errexit.out" "$work/guard-errexit.err")
check "guard under bash -e: a failed command still closes its block" \
  "$(is "$(count "$got" STOPPED-AT-END)" 0)"
check "guard under bash -e: the step fails with the command's status" \
  "$(is "$(cat "$work/guard-errexit.status")" 9)"
check "guard under bash -e: the step stops at the failed command" \
  "$(is "$(count "$got" never-reached)" 0)"

# Two broken copies of the library, so the cases above can fail: one whose close does not start a
# line of its own, after a body with no newline at its end, and one that leaves the command's
# stdout on stdout.
real_lib=$lib
lib="$work/close-no-newline.sh"
# shellcheck disable=SC2016,SC2028 # the function's text, written into the copy unexpanded
{ cat "$real_lib"; echo 'ci_log_block_close() { printf "::%s::\n" "$1" >&2; }'; } >"$lib"
step mutant-close "
  ci_log_guard '$work/client' 0
  echo '::warning::own-after' >&2"
got=$(parse "$work/mutant-close.out" "$work/mutant-close.err")
check "control: a close glued to the body's last line leaves the step stopped" \
  "$(is "$(count "$got" "^per-stream	stderr	STOPPED-AT-END")" 1)"
check "control: a close glued to the body's last line drops the step's own warning after it" \
  "$(is "$(count "$got" own-after)" 0)"
lib="$work/stdout-left.sh"
{ cat "$real_lib"; echo 'ci_log_guard() { ci_log_guard_stderr "$@"; }'; } >"$lib"
step mutant-stdout "ci_log_guard '$work/client' 0"
got=$(parse "$work/mutant-stdout.out" "$work/mutant-stdout.err")
for order in per-stream stderr-first stdout-first; do
  check "control: a guard that leaves stdout alone lets planted-out-legacy through, $order" \
    "$(is "$(count "$got" "^$order	stdout	warning	planted-out-legacy\$")" 1)"
done
lib=$real_lib

# A step's own warning written on standard output just after a block, where the runner's own
# reader dropped it in 32 of 50 runs: the stdout-in-block order drops it, and the same warning on
# standard error survives every order.
step own-on-stdout "
  ci_log_guard '$work/client' 0
  echo '::warning::own-on-stdout'
  echo '::warning::own-on-stderr' >&2"
got=$(parse "$work/own-on-stdout.out" "$work/own-on-stdout.err")
check "control: an own warning on stdout beside a block is dropped in the stdout-in-block order" \
  "$(is "$(count "$got" "^stdout-in-block	stdout	warning	own-on-stdout\$")" 0)"
check "control: and the per-stream order still sees it, so the order is what drops it" \
  "$(is "$(count "$got" "^per-stream	stdout	warning	own-on-stdout\$")" 1)"
for order in per-stream stderr-first stdout-first stdout-in-block; do
  check "the same own warning on stderr beside a block works, $order" \
    "$(is "$(count "$got" "^$order	stderr	warning	own-on-stderr\$")" 1)"
done

echo
echo "ci_log_guard_stderr: a captured stdout is a value with no marker in it"

for attempt in 0 7; do
  step "value-$attempt" "
    v=\$(ci_log_guard_stderr '$work/client' $attempt) && rc=0 || rc=\$?
    printf '%s' \"\$v\" >'$work/value-$attempt.value'
    echo '::warning::own-after' >&2
    exit \$rc"
  got=$(parse "$work/value-$attempt.out" "$work/value-$attempt.err")
  check "stderr guard, exit $attempt: the parser sees no planted command in any order" \
    "$(is "$(count "$got" planted)" 0)"
  check "stderr guard, exit $attempt: the step's own warning after the block works" \
    "$(is "$(count "$got" "^per-stream	stderr	warning	own-after\$")" 1)"
  check "stderr guard, exit $attempt: the command's exit status comes back" \
    "$(is "$(cat "$work/value-$attempt.status")" "$attempt")"
  "$work/client" "$attempt" 2>/dev/null >"$work/value-$attempt.expected" || true
  check "stderr guard, exit $attempt: the captured value is the command's stdout, byte for byte" \
    "$(is "$(cmp -s "$work/value-$attempt.value" "$work/value-$attempt.expected" && echo same)" same)"
done

# The stderr guard leaves stdout where the caller sends it: here a file, as retry_fetch sends it.
step value-file "ci_log_guard_stderr '$work/client' 0 >'$work/value-file.value'"
check "stderr guard to a file: the file holds no marker" \
  "$(is "$(grep -c -e 'stop-commands' -e '^::[0-9a-f]\{32\}::$' "$work/value-file.value" || true)" 0)"

echo
echo "The inline guard of the release steps that hold credentials"

# Each step of the two release workflows that defines ci_log_guard_stderr inline, read out of the
# workflow as GitHub hands it to bash: its run: text in inline.<n>.run, the definition alone in
# inline.<n>.def, and the text with the guard taken out (the definition dropped, each call left
# as the bare command) in inline.<n>.bare. One line per step: <n>, file, step name, and whether it
# is an npm publish step.
repo=$(cd "$script_dir/../.." && pwd)
mapfile -t inline < <(python3 - "$repo" "$work" <<'PY'
import pathlib
import re
import sys

import yaml

repo, work = sys.argv[1:3]
n = 0
for rel in (".github/workflows/release.yaml", ".github/workflows/release-envoy-listener.yaml"):
    for job in (yaml.safe_load(pathlib.Path(repo, rel).read_text()).get("jobs") or {}).values():
        for step in job.get("steps") or []:
            run = step.get("run") or ""
            lines = run.split("\n")
            starts = [i for i, line in enumerate(lines) if line.strip() == "ci_log_guard_stderr() {"]
            if not starts:
                continue
            start = starts[0]
            end = next(i for i in range(start, len(lines)) if lines[i].strip() == "}")
            n += 1
            pathlib.Path(work, f"inline.{n}.run").write_text(run)
            pathlib.Path(work, f"inline.{n}.def").write_text("\n".join(lines[start:end + 1]) + "\n")
            bare = lines[:start] + lines[end + 1:]
            bare = [re.sub(r"\bci_log_guard_stderr ", "", line) for line in bare]
            pathlib.Path(work, f"inline.{n}.bare").write_text("\n".join(bare))
            publish = "npm publish" in run
            print(f"{n}\t{rel}\t{step.get('name')}\t{'publish' if publish else 'other'}")
PY
)
check "the reader finds the eight release steps that define the guard inline" "$(is "${#inline[@]}" 8)"

# A stand-in npm for a whole publish step: `npm view` finds no published version, and `npm publish`
# answers as a registry's 4xx would have it print, planting each form on both streams, and exits
# with $NPM_EXIT.
mkdir -p "$work/npm-bin" "$work/package"
cat > "$work/npm-bin/npm" <<SH
#!/usr/bin/env bash
case "\$1" in
  view) exit 1 ;;
  publish) exec '$work/client' "\${NPM_EXIT:-0}" ;;
esac
echo "unexpected npm \$*" >&2
exit 2
SH
chmod +x "$work/npm-bin/npm"
echo '{"name": "@example/plugin"}' >"$work/package/package.json"

# publish_step <name> <script>: runs a publish step's text as the runner does (bash -e, in the
# package's directory, the step's env set), npm the stand-in.
publish_step() {
  local name=$1 script=$2 status=0
  (cd "$work/package" && PATH="$work/npm-bin:$PATH" VERSION=1.2.3 STEM=example RUNNER_TEMP="$work" \
    bash -e "$script") >"$work/$name.out" 2>"$work/$name.err" || status=$?
  echo "$status" >"$work/$name.status"
}

real_lib=$lib
for row in "${inline[@]}"; do
  IFS=$'\t' read -r n file name kind <<<"$row"
  lib="$work/inline.$n.def"
  for attempt in 0 7; do
    step "inline-$n-$attempt" "
      ci_log_guard_stderr '$work/client' $attempt >&2 && rc=0 || rc=\$?
      echo '::warning::own-after' >&2
      exit \$rc"
    got=$(parse "$work/inline-$n-$attempt.out" "$work/inline-$n-$attempt.err")
    check "$file \"$name\", its inline guard, exit $attempt: no planted command in any order" \
      "$(is "$(count "$got" planted)" 0)"
    check "$file \"$name\", its inline guard, exit $attempt: the step's own warning after it works" \
      "$(is "$(count "$got" "^stdout-in-block	stderr	warning	own-after\$")" 1)"
    check "$file \"$name\", its inline guard, exit $attempt: the command's exit status comes back" \
      "$(is "$(cat "$work/inline-$n-$attempt.status")" "$attempt")"
  done
  if [ "$kind" = publish ]; then
    for attempt in 0 1; do
      NPM_EXIT=$attempt publish_step "publish-$n-$attempt" "$work/inline.$n.run"
      got=$(parse "$work/publish-$n-$attempt.out" "$work/publish-$n-$attempt.err")
      check "$file \"$name\" run whole, npm publish exits $attempt: no planted command in any order" \
        "$(is "$(count "$got" planted)" 0)"
      check "$file \"$name\" run whole, npm publish exits $attempt: the step exits with npm's status" \
        "$(is "$(cat "$work/publish-$n-$attempt.status")" "$attempt")"
    done
    NPM_EXIT=0 publish_step "publish-$n-bare" "$work/inline.$n.bare"
    got=$(parse "$work/publish-$n-bare.out" "$work/publish-$n-bare.err")
    check "control: $file \"$name\" with its inline guard taken out lets all 8 planted commands through" \
      "$(is "$(count "$got" "^per-stream	std...	warning	planted-")" 8)"
  fi
done
# The control for a definition: the same call with the guard taken out of the function.
printf 'ci_log_guard_stderr() { "$@"; }\n' >"$work/no-guard.def"
lib="$work/no-guard.def"
step inline-bare "ci_log_guard_stderr '$work/client' 0 >&2"
got=$(parse "$work/inline-bare.out" "$work/inline-bare.err")
check "control: the inline call with no guard in its function lets the 8 planted commands through" \
  "$(is "$(count "$got" "^per-stream	std...	warning	planted-")" 8)"
lib=$real_lib

echo
echo "The pieces"

t1=$(bash -c ". '$lib'; ci_log_token")
t2=$(bash -c ". '$lib'; ci_log_token")
check "ci_log_token prints 32 lowercase hex characters" \
  "$(contains "$t1" '^[0-9a-f]\{32\}$')"
check "ci_log_token prints a fresh token each time" "$(is "$([ "$t1" != "$t2" ] && echo fresh)" fresh)"

step two-blocks "ci_log_guard true; ci_log_guard true"
mapfile -t opens < <(grep -o '^::stop-commands::[0-9a-f]*' "$work/two-blocks.err" || true)
check "each guarded command opens a block with a token of its own" \
  "$(is "$([ "${#opens[@]}" -eq 2 ] && [ "${opens[0]}" != "${opens[1]}" ] && echo two)" two)"

step bad-open "ci_log_block_open warning"
check "ci_log_block_open refuses a token that is a command name" \
  "$(is "$(cat "$work/bad-open.status")" 2)"
check "ci_log_block_open writes no marker for a refused token" \
  "$(is "$(grep -c '^::' "$work/bad-open.err" || true)" 0)"
step bad-close "ci_log_block_close ''"
check "ci_log_block_close refuses an empty token" "$(is "$(cat "$work/bad-close.status")" 2)"
step no-command "ci_log_guard"
check "ci_log_guard refuses to run no command" "$(is "$(cat "$work/no-command.status")" 2)"
step no-command-stderr "ci_log_guard_stderr"
check "ci_log_guard_stderr refuses to run no command" "$(is "$(cat "$work/no-command-stderr.status")" 2)"

# Sourcing and calling leave the caller's shell as it was: no option set, no variable left. The
# two controls are copies of the library that leave a variable and that set an option.
cat >"$work/state.sh" <<'SH'
before= after=
snap() { set -o; shopt; compgen -v | grep -v -x -e _ -e BASH_COMMAND -e BASH_LINENO \
  -e BASH_SOURCE -e FUNCNAME -e LINENO -e PIPESTATUS -e SECONDS -e RANDOM -e SRANDOM \
  -e EPOCHSECONDS -e EPOCHREALTIME -e BASH_ARGC -e BASH_ARGV -e BASHPID -e COLUMNS -e LINES; }
before=$(snap)
. "$1"
ci_log_guard true 2>/dev/null
ci_log_guard_stderr true 2>/dev/null
ci_log_block_open "$(ci_log_token)" 2>/dev/null
after=$(snap)
if [ "$before" = "$after" ]; then echo same; else echo changed; fi
SH
{ cat "$lib"; echo 'ci_log_guard() { leaked=1; }'; } >"$work/leaks-variable.sh"
{ cat "$lib"; echo 'set -o pipefail'; } >"$work/sets-option.sh"
check "control: the shell-state check sees a variable left behind" \
  "$(is "$(bash "$work/state.sh" "$work/leaks-variable.sh")" changed)"
check "control: the shell-state check sees an option set" \
  "$(is "$(bash "$work/state.sh" "$work/sets-option.sh")" changed)"
check "sourcing and calling set no option and leave no variable" \
  "$(is "$(bash "$work/state.sh" "$lib")" same)"

echo
echo "Under dash, as a /bin/sh caller sources the library"

# The library is POSIX sh with `local`: a /bin/sh caller (dash on the CI image) sources it and gets
# the same blocks, statuses and value. A machine without dash fails here rather than skips.
if command -v dash >/dev/null; then
  cat >"$work/state-sh.sh" <<'SH'
before= after=
snap() { set -o; set | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' | grep -v -x -e _ -e LINENO -e OPTIND; }
before=$(snap)
. "$1"
ci_log_guard true 2>/dev/null
ci_log_guard_stderr true 2>/dev/null
ci_log_block_open "$(ci_log_token)" 2>/dev/null
after=$(snap)
if [ "$before" = "$after" ]; then echo same; else echo changed; fi
SH
  check "control: under dash the shell-state check sees a variable left behind" \
    "$(is "$(dash "$work/state-sh.sh" "$work/leaks-variable.sh" 2>&1)" changed)"
  for attempt in 0 7; do
    status=0
    dash -e -c ". '$lib'; ci_log_guard '$work/client' $attempt && rc=0 || rc=\$?
      echo '::warning::own-after' >&2; exit \$rc" >"$work/dash-$attempt.out" 2>"$work/dash-$attempt.err" || status=$?
    got=$(parse "$work/dash-$attempt.out" "$work/dash-$attempt.err")
    check "dash, exit $attempt: the parser sees no planted command in any order" "$(is "$(count "$got" planted)" 0)"
    check "dash, exit $attempt: the step's own warning after the block works" \
      "$(is "$(count "$got" "^stdout-in-block	stderr	warning	own-after\$")" 1)"
    check "dash, exit $attempt: the command's exit status comes back" "$(is "$status" "$attempt")"
  done
  dash -c ". '$lib'; v=\$(ci_log_guard_stderr '$work/client' 0); printf '%s' \"\$v\"" \
    >"$work/dash-value" 2>/dev/null || true
  "$work/client" 0 2>/dev/null >"$work/dash-expected" || true
  check "dash: the captured value is the command's stdout, byte for byte" \
    "$(is "$(cmp -s "$work/dash-value" "$work/dash-expected" && echo same)" same)"
  check "dash: sourcing and calling set no option and leave no variable" \
    "$(is "$(dash "$work/state-sh.sh" "$lib" 2>&1)" same)"
else
  check "dash is on this machine (the CI image's /bin/sh)" false
fi

summary "ci-log-guard.sh"
