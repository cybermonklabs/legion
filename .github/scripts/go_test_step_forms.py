#!/usr/bin/env python3
"""Generated go test forms, run under bash and read by rule 8 of check-workflow-files.sh
(.github/scripts/go_test_steps.py), so that a form rule 8 misses is found by the generator rather
than by a reviewer planting it by hand.

A form is a context crossed with a go spelling and an indirection:
  - a context puts one command, `x`, at one place of a shell script: plain; behind `!`, as
    `if !`, `while !` and `until !`; in a brace group (also `{ x; } 2>&1 | tee`), a subshell, the
    body of a function the step calls, and each shape of a case arm; as an if, while or until
    condition, a then, else or loop body, after
    `&&` or `||`, before `&&`, `|` or `|&`, before the redirections `&>` and `2>&1`, after a
    redirection (`>/dev/null x`, `2>&1 x`, `</dev/null x`, `<<<y x`); behind assignments, time,
    command, exec, env, nice, timeout, nohup, setsid -w, xargs and find's -exec, -execdir, -ok and
    -okdir; in env -S's string and in the string of `bash -c` (`$'...'` too, its escapes standing
    for the go test's blank or a newline), `bash -o pipefail -c`, `bash --noprofile --norc -c`,
    `bash -n +n -c`, `bash -o noexec +o noexec -c` and `sh -c`, on one line or over several; in a
    heredoc a shell runs; before or after an assignment of GO no run of the go test sees (after it,
    in a subshell, a `bash -c`, an `if false` or an uncalled function); in the background with a
    `wait` for it (on the next line or the same, in a brace group, of `$!` or of a variable assigned
    `$!`, another job started after that assignment or not); in the background (alone, at a
    pipeline's or an and-or list's end, in a subshell or a brace group, in a `bash -c` string, an
    env -S string or a heredoc a shell runs) or behind `setsid -f` with no wait for it, or with a
    wait that waits for nothing: in a subshell, a pipeline, an `if false`, an uncalled function,
    itself in the background, outside the subshell the `&` stands in, or naming `$!` or a pid
    variable once another job (`&`, `<(...)`) has changed it (DETACHED); and behind each command
    rule 8 does not read (UNSEEN);
  - a go spelling is the command word of go test: `go`, `$GO`, `"$GO"`, `${GO}`, `"${GO}"`, the
    stand-in's absolute path (the shape of /usr/local/go/bin/go), `"$GOROOT/bin/go"` and
    `"$(go env GOROOT)/bin/go"`; env -S expands `${NAME}` alone and runs no `$(...)`, so its
    contexts take the spellings ONLY_SPELLINGS names;
  - an indirection is where the form stands: in the step's run:; in a script the step runs; in a
    script the step runs at the context's place (`called`: the context wraps the step's run of
    the script, and the script holds the go test alone); in a sibling that script runs as
    `"$(dirname "$0")/inner.sh"`; and behind a cycle (a.sh runs b.sh and the form, b.sh runs a.sh,
    an environment variable ending the recursion when bash runs them), with the step that runs
    a.sh before the step that runs b.sh and after it.
A composed form is one of OUTERS around each context, in the step's run:, with the spellings `go`,
`"$GO"` and `${GO}` (`composed/<outer>/<context>/<spelling>`): a compound or a runner that holds a
whole script (in the background, with a wait after it or not, behind `!`, in a subshell, a brace
group, an if or after `&&`, behind time, in `bash -c`, `sh -c` or a heredoc bash runs, and in `bash
-c` behind env, nice, timeout, setsid -w, xargs and a redirection). It keeps its events when both
keep them, runs detached when either does, and is unseen when its context is. dash, which runs
`sh -c`, has none of the forms SH_SKIPS names, so `sh -c` holds no such context.
Each form is a step of a workflow of its own, with an id and the job's upload after it. Bare, rule
8 must refuse each of its steps by number as running go test other than through the writer. Kept,
the go test runs behind .github/scripts/go_test_events.py (written as a step at the root names
it): rule 8 must pass the workflow and count one command for each step, or, for a DETACHED
context, refuse each step as one that can end before its events are written. The writer's first
form takes the word `go` alone (its main()), so the kept form of every spelling is routed as the
writer takes it, `<writer> go test`: `<writer> "$GO" test` would run go test through the writer's
second form, which keeps no event. That form is the same for each spelling, so it is made and
read once for each context and indirection, as the variant `kept` of the spelling `go`.
A negative form (`negative/<name>/<run or script>`, NEGATIVES) is a step that names go test where
bash runs none: a shell that runs nothing (`bash --help -c`, `-n`, `-o noexec`, `+n -n`,
`-D +n`), an argument, a quoted text, a runner's --help, a go test that compiles or lists only, a
variable the step assigns no go, and code no run reaches (`if false`). Rule 8 must refuse no
negative form, but one a row of the list names: it reads a go test in every branch, and a variable
go word whatever the step assigns it, so the `dead-` and `literal-` forms are refused as an
over-refusal the list accepts.

bash runs every form, the kept variants too, and every step, with a recording stand-in go first on
PATH (and GO and GOROOT naming it), which writes one test's events when go test has -json: a bare
step must run `go test ./pkg`, a kept one must also leave an event in the TEST_EVENTS_FILE the
writer appends to, and a negative one must run no test (a `-c` or a `-list` runs none). A form bash
does not run that way is an error of this generator (exit 2). Whether a kept step ran detached is
read from bash itself, not from the form's label: a kept step that can put a command in the
background holds its go test at a gate, which the runner opens on what it sees the step do (runs()),
and a form whose label that reading contradicts fails the generator (MISLABELLED). Before any step
runs, probe() holds the gate's detector to this machine's kernel, and stops the generator (exit 2)
where it would be blind.

It fails (exit 1) on:
  - a miss: a bare step rule 8 does not refuse, a kept form it counts short, or a DETACHED kept
    step it does not refuse, that no row of .github/go-test-step-misses.tsv (a pattern of form
    ids and the reason it is a known limit) matches;
  - a false refusal: a kept form of a context that keeps its events which rule 8 refuses, and a
    negative form it refuses or counts that no row of that list names;
  - a row of that list that matches no miss and no over-refusal.
A later reviewer extends CONTEXTS, OUTERS, NEGATIVES, SPELLINGS or INDIRECTIONS rather than
planting forms by hand.

  python3 .github/scripts/go_test_step_forms.py [--module DIR] [--only PATTERN] [--list]
    --module DIR      read the forms with the go_test_steps.py of DIR (and the modules beside it it
                      imports), such as an earlier tree's, or a copy with one rule taken out
    --only PATTERN    make only the forms whose id matches PATTERN (fnmatch), and hold only the
                      rows of the list that match one of them; a pattern that matches none is an
                      error (exit 2)
    --list            print each form's verdicts
.github/scripts/go_test_steps.test.sh runs it, and controls of it with one rule taken out.
"""
from __future__ import annotations

import fcntl
import fnmatch
import inspect
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
MISSES = HERE.parent / "go-test-step-misses.tsv"
PACKAGE = "./pkg"
GO_LINE = f"go test {PACKAGE}"
JOB = "forms"
WORKFLOW = Path(".github/workflows/forms.yaml")
# The events a stand-in go writes for -json: one test's run and pass, and its package's.
EVENTS = ('{"Action":"start","Package":"forms/pkg"}', '{"Action":"run","Package":"forms/pkg","Test":"TestForm"}',
          '{"Action":"pass","Package":"forms/pkg","Test":"TestForm","Elapsed":0}',
          '{"Action":"pass","Package":"forms/pkg","Elapsed":0}')
# A kept step that can detach holds its go test at a gate until the runner has seen which way the
# step goes (runs()), so the boundary is read from an order the gate makes and not from a race: the
# stand-in takes the gate's lock (`flock`), which the runner holds, before it answers. The runner
# opens the gate either after the step's shell has returned and the marker is written, so the held
# go test writes its events after the marker (detached), or when the step's shell is asleep and
# nothing of its process tree can wake it but the held go test (quiescent()), so the shell is
# waiting on that go test and returns only after it has written them (kept). No step waits a set
# time. Bash detaches a command only behind a lone `&`, `setsid` or `coproc` (CAN_DETACH), so a step
# whose text holds none of them returns after its go test whatever its label says and needs no gate,
# and a bare or negative step measures no boundary at all.
CAN_DETACH = re.compile(r"(?<![&>|<])&(?![&>])|\bsetsid\b|\bcoproc\b")
# What the runner appends to the events file once the step's shell has returned, before it opens
# the gate: every event line before it was written while the step ran, and every line after it came
# too late for the job's upload.
BOUNDARY = '{"Action":"BOUNDARY"}'
STAND_IN = """#!/bin/sh
if [ -n "${{FORMS_GATE:-}}" ]; then flock "$FORMS_GATE" true; fi
printf '%s\\n' "$*" >> "$FORMS_RECORD"
if [ "$1" = env ] && [ "$2" = GOROOT ]; then printf '%s\\n' "$GOROOT"; exit 0; fi
for word in "$@"; do
  if [ "$word" = -json ]; then printf '%s\\n' {events}; fi
done
""".format(events=" ".join("'" + event + "'" for event in EVENTS))
# The system calls, by number on each machine, in which a process of a step sleeps until another
# process of the step, or the gate, acts: a wait for a child (wait4, waitid, rt_sigsuspend, pause),
# a read or poll of a pipe (read, readv, pread64, poll, select, pselect6, ppoll, epoll_wait,
# epoll_pwait) and a lock (flock). A sleep on a timer (nanosleep, clock_nanosleep) ends by itself,
# and a call this table does not name is not known to wait on the step: either keeps the step
# from reading as quiescent. The numbers are each kernel's own (arch/x86/entry/syscalls and
# include/uapi/asm-generic/unistd.h); probe() holds them to what the running kernel reports.
WAITS_ON_THE_STEP = {
    "x86_64": frozenset({61, 247, 130, 34, 0, 19, 17, 7, 23, 270, 271, 232, 281, 73}),
    "aarch64": frozenset({260, 95, 133, 63, 65, 67, 72, 73, 22, 32}),
}


def sq(text: str) -> str:
    """`text` as one single-quoted shell word."""
    return "'" + text.replace("'", "'\\''") + "'"


KEEPS, DETACHED, UNSEEN = "keeps", "detached", "unseen"
# Each context: how it places the command `x`, and what the writer before x does for its events.
# KEEPS: they are all written before the step ends. DETACHED: the step can end first, so rule 8
# refuses the kept form too. UNSEEN: rule 8 does not read the command x stands behind, and
# .github/go-test-step-misses.tsv says why; a kept form keeps its events all the same.
CONTEXTS = {
    "plain": (lambda x: x, KEEPS),
    "negation": (lambda x: f"! {x}", KEEPS),
    "if-negation": (lambda x: f"if ! {x}; then exit 1; fi", KEEPS),
    "while-negation": (lambda x: f"while ! {x}; do break; done", KEEPS),
    "until-negation": (lambda x: f"until ! {x}; do break; done", KEEPS),
    "brace": (lambda x: f"{{ {x}; }}", KEEPS),
    "brace-tee": (lambda x: f"{{ {x}; }} 2>&1 | tee /dev/null", KEEPS),
    "brace-lines": (lambda x: f"{{\n{x}\n}}", KEEPS),
    "subshell": (lambda x: f"( {x} )", KEEPS),
    "function": (lambda x: f"f() {{ {x}; }}\nf", KEEPS),
    "function-keyword": (lambda x: f"function f {{ {x}; }}\nf", KEEPS),
    "function-lines": (lambda x: f"f() {{\n{x}\n}}\nf", KEEPS),
    "case-arm": (lambda x: f"case a in a) {x} ;; esac", KEEPS),
    "case-arm-parenthesized": (lambda x: f"case a in (a) {x} ;; esac", KEEPS),
    "case-arm-alternatives": (lambda x: f"case a in b | a) {x} ;; esac", KEEPS),
    "case-arm-second": (lambda x: f"case a in b) : ;; a) {x} ;; esac", KEEPS),
    "case-arm-line": (lambda x: f"case a in\n  b) : ;;\n  a) {x} ;;\nesac", KEEPS),
    "if-condition": (lambda x: f"if {x}; then :; fi", KEEPS),
    "then": (lambda x: f"if true; then {x}; fi", KEEPS),
    "else": (lambda x: f"if false; then :; else {x}; fi", KEEPS),
    "while-condition": (lambda x: f"while {x}; do break; done", KEEPS),
    "until-condition": (lambda x: f"until {x}; do :; done", KEEPS),
    "loop-body": (lambda x: f"for i in 1; do {x}; done", KEEPS),
    "and": (lambda x: f"true && {x}", KEEPS),
    "or": (lambda x: f"false || {x}", KEEPS),
    "before-and": (lambda x: f"{x} && true", KEEPS),
    "before-pipe": (lambda x: f"{x} | cat", KEEPS),
    "before-pipe-both": (lambda x: f"{x} |& cat", KEEPS),
    "redirect-both": (lambda x: f"{x} &> /dev/null", KEEPS),
    "redirect-duplicated": (lambda x: f"{x} > /dev/null 2>&1", KEEPS),
    "redirect-first": (lambda x: f">/dev/null {x}", KEEPS),
    "redirect-first-duplicated": (lambda x: f"2>&1 {x}", KEEPS),
    "redirect-first-input": (lambda x: f"</dev/null {x}", KEEPS),
    "redirect-first-here-string": (lambda x: f"<<<y {x}", KEEPS),
    "assignment": (lambda x: f"GOFLAGS= {x}", KEEPS),
    "time": (lambda x: f"time {x}", KEEPS),
    "time-p": (lambda x: f"time -p {x}", KEEPS),
    "command": (lambda x: f"command {x}", KEEPS),
    "exec": (lambda x: f"exec {x}", KEEPS),
    "env": (lambda x: f"env {x}", KEEPS),
    "env-assignment": (lambda x: f"env -u GOFLAGS GOWORK=off {x}", KEEPS),
    "env-split": (lambda x: f"env -S {sq(x)}", KEEPS),
    "nice": (lambda x: f"nice -n 1 {x}", KEEPS),
    "timeout": (lambda x: f"timeout 60 {x}", KEEPS),
    "nohup": (lambda x: f"nohup {x}", KEEPS),
    "setsid-w": (lambda x: f"setsid -w {x}", KEEPS),
    "xargs": (lambda x: f": | xargs {x}", KEEPS),
    "find-exec": (lambda x: f"find . -maxdepth 0 -exec {x} \\;", KEEPS),
    "find-execdir": (lambda x: f"find . -maxdepth 0 -execdir {x} \\;", KEEPS),
    "find-ok": (lambda x: f"echo y | find . -maxdepth 0 -ok {x} \\;", KEEPS),
    "find-okdir": (lambda x: f"echo y | find . -maxdepth 0 -okdir {x} \\;", KEEPS),
    "bash-c": (lambda x: f"bash -c {sq(x)}", KEEPS),
    "bash-c-lines": (lambda x: f"bash -c '\n{x}\n'", KEEPS),
    "bash-o-pipefail-c": (lambda x: f"bash -o pipefail -c {sq(x)}", KEEPS),
    "bash-long-options-c": (lambda x: f"bash --noprofile --norc -c {sq(x)}", KEEPS),
    "bash-noexec-reenabled": (lambda x: f"bash -n +n -c {sq(x)}", KEEPS),
    "bash-noexec-option-reenabled": (lambda x: f"bash -o noexec +o noexec -c {sq(x)}", KEEPS),
    "bash-c-ansi": (lambda x: f"bash -c $'{x}'", KEEPS),
    "bash-c-ansi-escape": (lambda x: "bash -c $'" + x.replace(" test ", r" test\x20") + "'", KEEPS),
    "bash-c-ansi-octal": (lambda x: "bash -c $'" + x.replace(" test ", r" test\040") + "'", KEEPS),
    "bash-c-ansi-lines": (lambda x: f"bash -c $'true\\n{x}'", KEEPS),
    "assignment-after": (lambda x: f"{x}\nGO=true", KEEPS),
    "assignment-in-subshell": (lambda x: f"( GO=true )\n{x}", KEEPS),
    "assignment-in-bash-c": (lambda x: f"bash -c 'GO=true'\n{x}", KEEPS),
    "assignment-in-branch": (lambda x: f"if false; then GO=true; fi\n{x}", KEEPS),
    "assignment-in-function": (lambda x: f"f() {{ GO=true; }}\n{x}", KEEPS),
    "sh-c": (lambda x: f"sh -c {sq(x)}", KEEPS),
    "heredoc-bash": (lambda x: f"bash <<'SCRIPT'\n{x}\nSCRIPT", KEEPS),
    "heredoc-bash-options": (lambda x: f"bash -o pipefail <<'SCRIPT'\n{x}\nSCRIPT", KEEPS),
    "background-wait": (lambda x: f"{x} &\nwait", KEEPS),
    "background-wait-same-line": (lambda x: f"{x} & wait", KEEPS),
    "background-wait-brace": (lambda x: f"{x} &\n{{ wait; }}", KEEPS),
    "background-wait-last": (lambda x: f"{x} &\nwait $!", KEEPS),
    "background-wait-pid": (lambda x: f"{x} &\npid=$!\nwait \"$pid\"", KEEPS),
    "background-brace-lines-wait": (lambda x: f"{{\n{x}\n}} &\nwait", KEEPS),
    "background": (lambda x: f"{x} &", DETACHED),
    "background-pipeline": (lambda x: f"{x} | cat &", DETACHED),
    "background-and-list": (lambda x: f"{x} && true &", DETACHED),
    "background-subshell": (lambda x: f"( {x} ) &", DETACHED),
    "background-brace": (lambda x: f"{{ {x}; }} &", DETACHED),
    "background-brace-list": (lambda x: f"{{ {x}; true; }} &", DETACHED),
    "background-brace-lines": (lambda x: f"{{\n{x}\n}} &", DETACHED),
    "background-in-subshell-wait": (lambda x: f"( {x} & )\nwait", DETACHED),
    "background-wait-subshell": (lambda x: f"{x} &\n(wait)", DETACHED),
    "background-wait-conditional": (lambda x: f"{x} &\nif false; then wait; fi", DETACHED),
    "background-wait-function": (lambda x: f"{x} &\nf() {{ wait; }}", DETACHED),
    "background-wait-pipeline": (lambda x: f"{x} &\nwait | cat", DETACHED),
    "background-wait-backgrounded": (lambda x: f"{x} & wait &", DETACHED),
    "background-wait-last-other": (lambda x: f"{x} &\nsleep 0 &\nwait $!", DETACHED),
    "background-wait-pid-other": (lambda x: f"{x} &\nsleep 0 &\npid=$!\nwait \"$pid\"", DETACHED),
    "background-wait-last-substitution": (lambda x: f"{x} &\n: <(true)\nwait $!", DETACHED),
    "background-wait-pid-reassigned": (lambda x: f"{x} &\npid=$!\npid=0\nwait \"$pid\" || true", DETACHED),
    "background-wait-pid-before-other": (lambda x: f"{x} &\npid=$!\nsleep 0 &\nwait \"$pid\"", KEEPS),
    "background-wait-pid-conditional": (lambda x: f"{x} &\npid=0\nif false; then pid=$!; fi\nwait \"$pid\" || true", DETACHED),
    "background-wait-pid-subshell": (lambda x: f"pid=0\n{x} &\n( pid=$! )\nwait \"$pid\" || true", DETACHED),
    "background-wait-pid-or": (lambda x: f"{x} &\npid=0\nfalse || pid=$!\nwait \"$pid\" || true", KEEPS),
    "background-wait-helper-before": (lambda x: f"f() {{ :; }}\n{x} &\nwait $!", KEEPS),
    "background-wait-helper-after": (lambda x: f"{x} &\nwait $!\nf() {{ :; }}", KEEPS),
    "background-wait-helper-called": (lambda x: f"f() {{ :; }}\n{x} &\nf\nwait $!", KEEPS),
    "background-wait-job-quoted-function": (lambda x: f"f() {{ sleep 0 & }}\n{x} &\n\"f\"\nwait $!", DETACHED),
    "background-wait-job-escaped-function": (lambda x: f"f() {{ sleep 0 & }}\n{x} &\n\\f\nwait $!", DETACHED),
    "background-wait-job-variable-function": (lambda x: f"f() {{ sleep 0 & }}\nFN=f\n{x} &\n\"$FN\"\nwait $!", DETACHED),
    "background-wait-job-quoted-eval": (lambda x: f"{x} &\n\"eval\" 'sleep 0 &'\nwait $!", DETACHED),
    "background-wait-pid-kept-subshell": (lambda x: f"{x} &\npid=$!\n( pid=0 )\nwait \"$pid\"", KEEPS),
    "background-wait-pid-kept-branch": (lambda x: f"{x} &\npid=$!\nif false; then pid=0; fi\nwait \"$pid\"", KEEPS),
    "background-wait-word-argument": (lambda x: f"{x} &\necho source\nwait $!", KEEPS),
    "background-wait-word-value": (lambda x: f"{x} &\nlabel=source\nwait $!", KEEPS),
    "background-wait-or-continuation": (lambda x: f"{x} &\ntrue ||\nwait\ntrue", DETACHED),
    "background-wait-and-continuation": (lambda x: f"{x} &\nfalse &&\nwait\ntrue", DETACHED),
    "bash-c-ansi-scalar": (lambda x: "bash -c $'" + x + r" #\UFFFFFFFF'", KEEPS),
    "bash-c-ansi-surrogate": (lambda x: "bash -c $'" + x + r" #\uD800'", KEEPS),
    "bash-c-background": (lambda x: f"bash -c {sq(x)} &", DETACHED),
    "env-split-background": (lambda x: f"env -S {sq(x)} &", DETACHED),
    "heredoc-bash-background": (lambda x: f"bash <<'SCRIPT' &\n{x}\nSCRIPT", DETACHED),
    "setsid-f": (lambda x: f"setsid -f {x}", DETACHED),
    "eval": (lambda x: f"eval {sq(x)}", UNSEEN),
    "coproc": (lambda x: f"coproc {x}\nwait", UNSEEN),
    "flock": (lambda x: f'flock "$FORMS_LOCK" {x}', UNSEEN),
    "pipe-to-bash": (lambda x: f"printf '%s\\n' {sq(x)} | bash", UNSEEN),
    "here-string": (lambda x: f"bash <<< {sq(x)}", UNSEEN),
    "python3-c": (lambda x: f"python3 -c 'import os, sys; os.execvp(sys.argv[1], sys.argv[1:])' {x}", UNSEEN),
    "stdbuf": (lambda x: f"stdbuf -oL {x}", UNSEEN),
    "ionice": (lambda x: f"ionice -c 3 {x}", UNSEEN),
    "taskset": (lambda x: f"taskset -c 0 {x}", UNSEEN),
}
# Each go spelling; {stand_in} is the stand-in's absolute path.
SPELLINGS = {
    "go": "go",
    "dollar": "$GO",
    "quoted-dollar": '"$GO"',
    "braced": "${GO}",
    "quoted-braced": '"${GO}"',
    "absolute": "{stand_in}",
    "goroot-variable": '"$GOROOT/bin/go"',
    "goroot-substitution": '"$(go env GOROOT)/bin/go"',
}
# env -S expands `${NAME}` and nothing else: a `$NAME` it refuses, a `$(...)` it runs no command for.
ONLY_SPELLINGS = {name: ("go", "braced", "quoted-braced", "absolute") for name in ("env-split", "env-split-background")}
# Each outer: how it holds a whole script, `t`, which may span lines, and what it does for the
# events of the script's go test.
OUTERS = {
    "background": (lambda t: f"{{\n{t}\n}} &", DETACHED),
    "background-wait": (lambda t: f"{{\n{t}\n}} &\nwait", KEEPS),
    "negation": (lambda t: f"! {{\n{t}\n}}", KEEPS),
    "subshell": (lambda t: f"(\n{t}\n)", KEEPS),
    "brace": (lambda t: f"{{\n{t}\n}}", KEEPS),
    "if": (lambda t: f"if true; then\n{t}\nfi", KEEPS),
    "and": (lambda t: f"true && {{\n{t}\n}}", KEEPS),
    "time": (lambda t: f"time {{\n{t}\n}}", KEEPS),
    "bash-c": (lambda t: f"bash -c {sq(t)}", KEEPS),
    "sh-c": (lambda t: f"sh -c {sq(t)}", KEEPS),
    "heredoc-bash": (lambda t: f"bash <<'OUTER'\n{t}\nOUTER", KEEPS),
    "env": (lambda t: f"env bash -c {sq(t)}", KEEPS),
    "nice": (lambda t: f"nice -n 1 bash -c {sq(t)}", KEEPS),
    "timeout": (lambda t: f"timeout 60 bash -c {sq(t)}", KEEPS),
    "setsid-w": (lambda t: f"setsid -w bash -c {sq(t)}", KEEPS),
    "xargs": (lambda t: f": | xargs bash -c {sq(t)}", KEEPS),
    "redirection": (lambda t: f">/dev/null bash -c {sq(t)}", KEEPS),
}
# The contexts dash, which runs `sh -c`, does not run as bash does: `|&` and `<<<` it cannot parse,
# `&>` it reads as `&` and `>`, and it has no `time` keyword, no coproc, no `function` keyword, no
# `$'...'` string and no process substitution.
SH_SKIPS = ("before-pipe-both", "redirect-both", "redirect-first-here-string", "time", "time-p", "coproc",
            "here-string", "function-keyword", "bash-c-ansi", "bash-c-ansi-escape", "bash-c-ansi-octal",
            "bash-c-ansi-lines", "bash-c-ansi-scalar", "bash-c-ansi-surrogate",
            "background-wait-last-substitution")
# Each negative form: a step that names go test where bash runs none.
NEGATIVES = {
    "bash-help": f"bash --help -c {sq(GO_LINE)}",
    "bash-version": f"bash --version -c {sq(GO_LINE)}",
    "bash-noexec": f"bash -n -c {sq(GO_LINE)}",
    "bash-noexec-cluster": f"bash -nc {sq(GO_LINE)}",
    "bash-noexec-option": f"bash -o noexec -c {sq(GO_LINE)}",
    "bash-dump-strings": f"bash --dump-strings -c {sq(GO_LINE)}",
    "bash-noexec-last": f"bash +n -n -c {sq(GO_LINE)}",
    "bash-noexec-option-last": f"bash +o noexec -o noexec -c {sq(GO_LINE)}",
    "bash-dump-reenabled": f"bash -D +n -c {sq(GO_LINE)}",
    "sh-noexec": f"sh -n -c {sq(GO_LINE)}",
    "bash-help-heredoc": f"bash --help <<'SCRIPT'\n{GO_LINE}\nSCRIPT",
    "bash-noexec-heredoc": f"bash -n <<'SCRIPT'\n{GO_LINE}\nSCRIPT",
    "literal-variable": f'TOOL=true\n"$TOOL" test {PACKAGE}',
    "literal-variable-path": f'TOOL=/usr/bin/true; "${{TOOL}}" test {PACKAGE}',
    "literal-go-variable": f"GO=true\n$GO test {PACKAGE}",
    "colon": f": {GO_LINE}",
    "echo": f"echo {GO_LINE}",
    "quoted": f"echo '{GO_LINE}'",
    "text-heredoc": f"cat <<'TEXT'\n{GO_LINE}\nTEXT",
    "comment": f"true # {GO_LINE}",
    "assigned-string": f'x="{GO_LINE}"',
    "substitution-pattern": "y=a; y=${y/go test/z}",
    "nice-help": f"nice --help {GO_LINE}",
    "timeout-help": f"timeout --help 60 {GO_LINE}",
    "env-version": f"env --version {GO_LINE}",
    "xargs-version": f": | xargs --version {GO_LINE}",
    "command-v": f"command -v {GO_LINE} || true",
    "compile-only": f"go test -c -o /dev/null {PACKAGE}",
    "list": f"go test -list . {PACKAGE}",
    "dead-if-false": f"if false; then {GO_LINE}; fi",
    "dead-and-false": f"false && {GO_LINE}",
    "dead-function-uncalled": f"f() {{ {GO_LINE}; }}",
    "dead-case-unmatched": f"case b in a) {GO_LINE} ;; esac",
    "dead-after-exit": f"exit 0\n{GO_LINE}",
    "ansi-nul": r"bash -c $':\0; go test ./pkg'",
    "ansi-nul-octal": r"bash -c $':\000; go test ./pkg'",
}
SHEBANG = "#!/usr/bin/env bash\n"


def indirect(kind: str, wrap, command: str, home: str) -> tuple[dict[str, str], list[str]]:
    """The scripts (by path under the tree) and the steps' run texts that put `command`, at the
    place `wrap` puts it, at the indirection `kind`; the scripts are under `home`, a directory of
    the form's own."""
    form = wrap(command)
    if kind == "run":
        return {}, [form]
    if kind == "script":
        return {f"{home}/unit.sh": SHEBANG + form + "\n"}, [f"{home}/unit.sh"]
    if kind == "called":
        return {f"{home}/unit.sh": SHEBANG + command + "\n"}, [wrap(f"{home}/unit.sh")]
    if kind == "sibling":
        return ({f"{home}/outer.sh": SHEBANG + '"$(dirname "$0")/inner.sh"\n', f"{home}/inner.sh": SHEBANG + form + "\n"},
                [f"{home}/outer.sh"])
    scripts = {
        f"{home}/a.sh": SHEBANG + f'[ -n "${{FORMS_IN_A:-}}" ] || FORMS_IN_A=1 bash {home}/b.sh\n{form}\n',
        f"{home}/b.sh": SHEBANG + f'[ -n "${{FORMS_IN_A:-}}" ] || bash {home}/a.sh\n',
    }
    order = ["a.sh", "b.sh"] if kind == "cycle-a-first" else ["b.sh", "a.sh"]
    return scripts, [f"{home}/{name}" for name in order]


INDIRECTIONS = ("run", "script", "called", "sibling", "cycle-a-first", "cycle-b-first")


def workflow(steps: list[str]) -> str:
    """A workflow whose one job runs each text as a step with an id, then the job's upload."""
    out = ["name: forms", "on: workflow_call", "jobs:", f"  {JOB}:", "    runs-on: ubuntu-latest", "    steps:"]
    for number, text in enumerate(steps, 1):
        out += [f"      - name: Form step {number}", f"        id: step-{number}", "        run: |"]
        out += [f"          {line}" if line else "" for line in text.split("\n")]
    out += ["      - name: Upload the go test events", "        if: always()",
            "        uses: ./.github/actions/go-test-events-upload", "        with:",
            "          check-run-id: ${{ job.check_run_id }}"]
    return "\n".join(out) + "\n"


def composed_class(outer: str, inner: str) -> str:
    if inner == UNSEEN:
        return UNSEEN
    return DETACHED if DETACHED in (outer, inner) else KEEPS


def forms(stand_in: str, writer: str) -> tuple[dict, dict]:
    """({form id: {variant: (its scripts, its steps)}}, {form id: its class}): the variant bare of
    every form and the variant kept of each form whose spelling is `go` (the module header says
    why), and the variant negative of each negative form."""
    out, classes, number = {}, {}, 0
    for kind in INDIRECTIONS:
        for context, (wrap, keeps) in CONTEXTS.items():
            for spelling, word in SPELLINGS.items():
                if spelling not in ONLY_SPELLINGS.get(context, SPELLINGS):
                    continue
                number += 1
                home, form = f"scripts/f{number}", f"{kind}/{context}/{spelling}"
                out[form] = {"bare": indirect(kind, wrap, f"{word.replace('{stand_in}', stand_in)} test {PACKAGE}", f"{home}b")}
                if spelling == "go":
                    out[form]["kept"] = indirect(kind, wrap, f"{writer} go test {PACKAGE}", f"{home}k")
                classes[form] = keeps
    for outer, (around, outer_keeps) in OUTERS.items():
        for context, (wrap, keeps) in CONTEXTS.items():
            if outer == "sh-c" and context in SH_SKIPS:
                continue
            for spelling in ("go", "quoted-dollar", "braced"):
                if spelling not in ONLY_SPELLINGS.get(context, SPELLINGS):
                    continue
                form = f"composed/{outer}/{context}/{spelling}"
                commands = [("bare", f"{SPELLINGS[spelling]} test {PACKAGE}")]
                commands += [("kept", f"{writer} {GO_LINE}")] if spelling == "go" else []
                out[form] = {variant: indirect("run", lambda x: around(wrap(x)), command, "") for variant, command in commands}
                classes[form] = composed_class(outer_keeps, keeps)
    for name, text in NEGATIVES.items():
        for kind in ("run", "script"):
            number += 1
            form = f"negative/{name}/{kind}"
            out[form] = {"negative": indirect(kind, lambda _: text, "", f"scripts/f{number}n")}
            classes[form] = "negative"
    return out, classes


def ran_test(lines: list[str]) -> bool:
    """Whether the stand-in's record holds a go test of the package that runs its tests: one with
    neither -c nor -list among its words."""
    return any(words[:1] == ["test"] and PACKAGE in words and "-c" not in words and "-list" not in words
               for words in (line.split() for line in lines))


def gated(variant: str, texts: list[str]) -> bool:
    """Whether a step's go test waits at the gate for the runner (runs()): a kept step whose run: or
    a script of its form holds what can put a command in the background (CAN_DETACH). The test is on
    the form's text and not its label, so a label that calls a backgrounding form keeps, or one that
    cannot detach detached, is measured all the same, and MISLABELLED names it. An UNSEEN form is
    gated too: its label is not held to the boundary, but its verdict reads the boundary, and an
    ungated one raced, flipping between two misses the list names from one run to the next."""
    return variant == "kept" and any(CAN_DETACH.search(text) for text in texts)


def pipe_ends(pid: int, fds: tuple) -> set:
    """The pipes a process holds as the given fds (`pipe:[inode]`), those it holds at all."""
    ends = set()
    for fd in fds:
        try:
            end = os.readlink(f"/proc/{pid}/fd/{fd}")
        except OSError:
            continue
        if end.startswith("pipe:"):
            ends.add(end)
    return ends


def tree_state(shell: int, gate: str, waits: frozenset) -> tuple | None:
    """One reading of a step's process tree, from its shell down: each process's id, the call it
    sleeps in and its context-switch counts, when every process is asleep in a call of `waits`, the
    go test's flock on the gate is among them, and every other process is an ancestor of that flock
    (its chain, read from each process's own parent in /proc) or a reader of the chain's own output:
    one whose standard input is a pipe a process of the chain writes as its standard output or
    error (`x | cat`, `{ x; } 2>&1 | tee`), which waits on the held go test as the chain does. The
    shell is then waiting on the held go test and on nothing beside it. None when one runs, waits on
    disk, is a zombie its parent has yet to reap, sleeps on a timer or in a call `waits` does not
    name, or cannot be read; when no process is held at the gate; and when a process beside the
    chain reads no pipe of it - another child the shell may be waiting on instead (a process
    substitution's `wait $!`), which can end and let the shell return without the go test."""
    seen, parents, held, pending = {}, {}, [], [shell]
    try:
        while pending:
            pid = pending.pop()
            with open(f"/proc/{pid}/stat") as file:
                state, parent = file.read().rsplit(")", 1)[1].split()[:2]
            with open(f"/proc/{pid}/syscall") as file:
                call = file.read().split()[0]
            if state != "S" or not call.isdigit() or int(call) not in waits:
                return None
            with open(f"/proc/{pid}/status") as file:
                switches = tuple(line.split()[1] for line in file if "ctxt_switches:" in line)
            with open(f"/proc/{pid}/cmdline", "rb") as file:
                if file.read().split(b"\0")[:2] == [b"flock", gate.encode()]:
                    held.append(pid)
            seen[pid], parents[pid] = (int(call), switches), int(parent)
            for task in os.listdir(f"/proc/{pid}/task"):
                with open(f"/proc/{pid}/task/{task}/children") as file:
                    pending += [int(child) for child in file.read().split()]
    except (OSError, ValueError, IndexError):
        return None
    chain = set()
    for pid in held:
        while pid != shell:
            if pid not in seen:
                return None
            chain.add(pid)
            pid = parents[pid]
    chain.add(shell)
    if not held:
        return None
    beside = set(seen) - chain
    if beside:
        written = set().union(*(pipe_ends(pid, (1, 2)) for pid in chain))
        if any(not pipe_ends(pid, (0,)) or not pipe_ends(pid, (0,)) <= written for pid in beside):
            return None
    return tuple(sorted((pid, *reading) for pid, reading in seen.items()))


def quiescent(shell: int, gate: str, waits: frozenset) -> bool:
    """Whether a step's shell is asleep and nothing of its process tree can wake it but the go test
    held at the gate: two readings of the tree, one after the other (tree_state), find every process
    asleep in a call only another process of the tree or the gate can end, the held flock among
    them, every other process its ancestor or a reader of its output, and the same processes in the
    same calls having run not once between the readings (the same context-switch counts). One
    reading could miss a process woken while it read the rest; a process that woke shows in the
    second as running or as switched."""
    first = tree_state(shell, gate, waits)
    return first is not None and tree_state(shell, gate, waits) == first


def runs(tree: Path, env: dict, text: str, variant: str, scratch: Path, gate_it: bool,
         waits: frozenset) -> tuple[bool, bool]:
    """What bash does with the step `text` at the tree's root, under the shell and options a
    runner's `run:` uses: whether it runs it as its variant asks (a bare step runs the stand-in's go
    test, a kept one also leaves an event of it in TEST_EVENTS_FILE, and a negative one runs no go
    test of the package), and whether the step ran detached - the events of a kept step were not
    all there when bash returned, which is the runtime boundary a job's upload would read. The
    runner appends BOUNDARY to the events file once the step's shell has returned; a step that
    cannot detach has every command done by then. A gated step (gated()) holds its go test until
    the runner opens the gate: when the shell is quiescent, waiting on that go test, so the shell
    returns after it has written its events, or after the shell has returned and the marker is
    written, so the go test writes after the marker. Which comes first is what the step does, and
    the order of the marker and the events is then made by the gate, however the box is loaded.
    A bare or negative step is never detached: it keeps no events to be late."""
    scratch.mkdir(parents=True)
    record, events, step, gate = scratch / "record", scratch / "events.jsonl", scratch / "step.sh", scratch / "gate"
    step.write_text(text + "\n")
    run_env = dict(env, FORMS_RECORD=str(record), TEST_EVENTS_FILE=str(events), FORMS_LOCK=str(scratch / "lock"))
    lock = None
    if gate_it:
        lock = os.open(gate, os.O_CREAT | os.O_RDWR, 0o644)
        fcntl.flock(lock, fcntl.LOCK_EX)
        run_env["FORMS_GATE"] = str(gate)
    try:
        shell = subprocess.Popen(["bash", "--noprofile", "--norc", "-eo", "pipefail", str(step)], cwd=tree,
                                 env=run_env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + 60
        while lock is not None and shell.poll() is None and time.monotonic() < deadline:
            if quiescent(shell.pid, str(gate), waits):
                os.close(lock)
                lock = None
            else:
                time.sleep(0.001)
        try:
            shell.wait(timeout=max(0.1, deadline - time.monotonic()))
        except subprocess.TimeoutExpired:
            shell.kill()
            shell.wait()
            return False, False
        with open(events, "a") as file:
            file.write(BOUNDARY + "\n")
    finally:
        if lock is not None:
            os.close(lock)
    written = events.read_text()
    kept_at_boundary = EVENTS[1] in written.split(BOUNDARY)[0]
    if variant == "negative":
        return not ran_test(record.read_text().split("\n") if record.is_file() else []), False
    # A detached go test is still running when bash has returned: wait for what it writes, to
    # a deadline that bounds a step that never writes it.
    deadline = time.monotonic() + 10
    while True:
        ran = ran_test(record.read_text().split("\n") if record.is_file() else [])
        kept = variant == "bare" or EVENTS[1] in events.read_text()
        if (ran and kept) or time.monotonic() > deadline:
            return ran and kept, variant == "kept" and not kept_at_boundary
        time.sleep(0.005)


def probe(scratch: Path) -> tuple[frozenset | None, str]:
    """The calls of WAITS_ON_THE_STEP this machine's kernel uses, held to the kernel itself before
    any step runs: a process blocked reading a pipe must read as waiting on the step, and a process
    asleep on a timer must not. Then the detector itself, on shells that hold a go test at a gate:
    one waiting on that go test must read as quiescent, and so must one whose go test's output a
    pipeline's reader waits on beside it; one with a timer's sleep beside the held go test must not,
    and neither must one waiting on another child it started beside it (a process substitution's
    `wait $!`), which an earlier detector took for a shell waiting on the go test. This is the
    detector's control, and it fails for the reasons the detector would be blind - a machine the
    table does not name, a call the table misnumbers, process files this cannot read, a waited-for
    child taken for the go test - so the generator stops by name (exit 2) rather than hold every
    gated step to its deadline or open a gate early. (the calls, "") when every check holds, else
    (None, what failed)."""
    waits = WAITS_ON_THE_STEP.get(os.uname().machine)
    if waits is None:
        return None, f"WAITS_ON_THE_STEP names no calls for this machine ({os.uname().machine})"
    scratch.mkdir(parents=True)

    def call(pid: int) -> int | None:
        """The call a process sleeps in, once it sleeps, to a 10-second deadline."""
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                with open(f"/proc/{pid}/stat") as file:
                    state = file.read().rsplit(")", 1)[1].split()[0]
                with open(f"/proc/{pid}/syscall") as file:
                    number = file.read().split()[0]
            except (OSError, IndexError):
                return None
            if state == "S" and number.isdigit():
                return int(number)
            time.sleep(0.001)
        return None

    def asleep(shell: int, names: tuple) -> bool:
        """Whether a process of each name has started under the shell and sleeps, to a deadline."""
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            found, pending = {}, [shell]
            try:
                while pending:
                    pid = pending.pop()
                    with open(f"/proc/{pid}/comm") as file:
                        found.setdefault(file.read().strip(), pid)
                    for task in os.listdir(f"/proc/{pid}/task"):
                        with open(f"/proc/{pid}/task/{task}/children") as file:
                            pending += [int(child) for child in file.read().split()]
            except OSError:
                found = {}
            if all(name in found for name in names):
                return all(call(found[name]) is not None for name in names)
            time.sleep(0.001)
        return False

    def check(name: str, line: str, quiet: bool, sleepers: tuple = ()) -> str | None:
        """A shell running `line`, with {gate} its held gate: it must read as quiescent (`quiet`),
        to a deadline, or, once each of `sleepers` sleeps, must not. What failed, or None."""
        gate = scratch / f"gate-{name}"
        lock = os.open(gate, os.O_CREAT | os.O_RDWR, 0o644)
        fcntl.flock(lock, fcntl.LOCK_EX)
        shell = subprocess.Popen(["bash", "--noprofile", "--norc", "-c", line.format(gate=sq(str(gate)))],
                                 stdin=subprocess.DEVNULL, start_new_session=True)
        try:
            if quiet:
                deadline = time.monotonic() + 10
                while not quiescent(shell.pid, str(gate), waits):
                    if time.monotonic() >= deadline:
                        return f"{name}: a shell running `{line}` never reads as quiescent"
                    time.sleep(0.001)
            elif not asleep(shell.pid, sleepers + ("flock",)):
                return f"{name}: under a shell running `{line}`, {' and '.join(sleepers + ('flock',))} never sleep"
            elif quiescent(shell.pid, str(gate), waits):
                return f"{name}: a shell running `{line}` reads as quiescent"
            return None
        finally:
            os.close(lock)
            try:
                os.killpg(shell.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            shell.wait()

    reader, writer = os.pipe()
    fifo = scratch / "fifo"
    os.mkfifo(fifo)
    # A writer held open, so a `cat` of the fifo opens it at once and sleeps in its read.
    held_open = os.open(fifo, os.O_RDWR)
    timer = subprocess.Popen(["sleep", "30"])
    cat = subprocess.Popen(["cat"], stdin=reader, stdout=subprocess.DEVNULL)
    try:
        problems = [f"`{name}` sleeps in call {number}, which WAITS_ON_THE_STEP[{os.uname().machine!r}] "
                    f"{'does not name' if want else 'names'}"
                    for name, number, want in (("cat", call(cat.pid), True), ("sleep", call(timer.pid), False))
                    if number is None or (number in waits) != want]
        problems += [found for found in (
            check("waits-on-the-go-test", "flock {gate} true & wait", quiet=True),
            check("pipeline-reads-the-go-test", "{{ flock {gate} true | cat; }} & wait", quiet=True),
            check("timer-beside", "flock {gate} true & sleep 30 & wait", quiet=False, sleepers=("sleep",)),
            check("waits-on-another-child", f"flock {{gate}} true & : <(cat {sq(str(fifo))}); wait $!", quiet=False,
                  sleepers=("cat",)),
        ) if found]
    finally:
        for process in (timer, cat):
            process.kill()
            process.wait()
        for handle in (writer, reader, held_open):
            os.close(handle)
    return (None, "; ".join(problems)) if problems else (waits, "")


def read_with(directory: Path, tree: Path, texts: dict) -> dict:
    """{key: {"problems": [...], "commands": n}}: each workflow read by the go_test_steps.py of
    `directory`, in a process of its own whose working directory is the tree the scripts are in."""
    result = subprocess.run([sys.executable, __file__, "--read", str(directory)], input=json.dumps(texts), cwd=tree,
                            capture_output=True, text=True, env=dict(os.environ, PYTHONDONTWRITEBYTECODE="1"))
    if result.returncode != 0:
        sys.exit(f"go_test_step_forms: reading with {directory} failed:\n{result.stderr}")
    return json.loads(result.stdout)


def read_here(directory: str) -> None:
    """The --read mode: reads {key: workflow text} on stdin, prints each one's refusals and count.
    A module whose check takes a cache returns its census; an earlier one, whose census is the
    module's own dict, is read before and after."""
    sys.path.insert(0, directory)
    import yaml
    import go_test_steps
    check = go_test_steps.check_go_test_events
    cache = {} if "cache" in inspect.signature(check).parameters else None
    out = {}
    for key, text in json.load(sys.stdin).items():
        problems = []

        def problem(_path, message, line=None):
            problems.append(message)

        root = yaml.compose(text)
        if cache is None:
            before = go_test_steps.census["commands"]
            check(WORKFLOW, root, problem)
            commands = go_test_steps.census["commands"] - before
        else:
            commands = check(WORKFLOW, root, problem, cache).commands
        out[key] = {"problems": problems, "commands": commands}
    print(json.dumps(out))


def listed_misses() -> tuple[dict, list]:
    """({form id pattern: its row's number} of .github/go-test-step-misses.tsv, its problems). A
    pattern is an fnmatch pattern of form ids (`*/eval/*`)."""
    rows, problems = {}, []
    if not MISSES.is_file():
        return rows, problems
    for number, line in enumerate(MISSES.read_text().split("\n"), 1):
        if not line.strip() or line.startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) != 2 or not fields[1].strip():
            problems.append(f"{MISSES.name}:{number}: not a form id pattern and a reason, tab-separated")
            continue
        rows[fields[0]] = number
    return rows, problems


def names(problems: list[str], number: int) -> list[str]:
    return [text for text in problems if f"step {number} of job {JOB} (Form step {number}) runs " in text]


def verdict(keeps: str, variant: str, read: dict, steps: int, detached: bool) -> str | None:
    """What is wrong with rule 8's reading of one variant of a form: None, or `miss: ...` or
    `false: ...` (`over: ...` for a negative form, which a row may list). `detached` is the
    runtime boundary's answer, not the form's label: whether bash returned before the step's
    events were all written, which is what a job's upload would read. A kept step that ran
    detached must be refused for that; one that did not must be counted, whatever its class."""
    problems = read["problems"]
    if variant == "negative":
        return f"over: refused or counted: {' | '.join(problems) or read['commands']}" if problems or read["commands"] else None
    if variant == "bare":
        unnamed = [n for n in range(1, steps + 1) if not any("other than through" in t for t in names(problems, n))]
        return f"miss: bare, step(s) {', '.join(map(str, unnamed))} not refused" if unnamed else None
    if detached:
        unnamed = [n for n in range(1, steps + 1)
                   if not any("before the command has written them" in t for t in names(problems, n))]
        return f"miss: detached, step(s) {', '.join(map(str, unnamed))} not refused" if unnamed else None
    if problems and keeps != UNSEEN:
        return "false: refused: " + " | ".join(problems)
    if read["commands"] < steps:
        return f"miss: kept, {read['commands']} of {steps} command(s) counted"
    return None


def main() -> int:
    args = sys.argv[1:]
    if args[:1] == ["--read"]:
        read_here(args[1])
        return 0
    module = Path(args[args.index("--module") + 1]).resolve() if "--module" in args else HERE
    sys.path.insert(0, str(HERE))
    from workflow_paths import EVENTS_WRITER
    work = Path(tempfile.mkdtemp(prefix="go-test-step-forms."))
    try:
        waits, failed = probe(work / "probe")
        if waits is None:
            print(f"go_test_step_forms: the gate's detector fails its control on this machine, so no step is read: "
                  f"{failed}")
            return 2
        goroot = work / "go"
        (goroot / "bin").mkdir(parents=True)
        stand_in = goroot / "bin" / "go"
        stand_in.write_text(STAND_IN)
        stand_in.chmod(0o755)
        tree = work / "tree"
        (tree / Path(EVENTS_WRITER).parent).mkdir(parents=True)
        shutil.copy(HERE / Path(EVENTS_WRITER).name, tree / EVENTS_WRITER)
        generated, classes = forms(str(stand_in), EVENTS_WRITER)
        only = args[args.index("--only") + 1] if "--only" in args else "*"
        generated = {form: variants for form, variants in generated.items() if fnmatch.fnmatchcase(form, only)}
        if not generated:
            print(f"go_test_step_forms: --only {only} matches no form, so nothing was read")
            return 2
        for variants in generated.values():
            for scripts, _ in variants.values():
                for path, text in scripts.items():
                    (tree / path).parent.mkdir(parents=True, exist_ok=True)
                    (tree / path).write_text(text)
                    (tree / path).chmod(0o755)
        # find -execdir refuses a PATH with a relative directory in it; the runner's own
        # variables would point the writer at the runner's directory rather than TEST_EVENTS_FILE's.
        path = ":".join([str(goroot / "bin")] + [p for p in os.environ.get("PATH", "").split(":") if p.startswith("/")])
        env = {k: v for k, v in os.environ.items() if k not in ("RUNNER_TEMP", "GITHUB_ACTION", "GOFLAGS")}
        env.update(PATH=path, GO=str(stand_in), GOROOT=str(goroot))
        units = [(form, variant, index, text) for form, variants in generated.items()
                 for variant, (_, steps) in variants.items() for index, text in enumerate(steps)]
        gates = {(form, variant): gated(variant, steps + list(scripts.values()))
                 for form, variants in generated.items() for variant, (scripts, steps) in variants.items()}
        with ThreadPoolExecutor(max_workers=len(os.sched_getaffinity(0))) as pool:
            measured = list(pool.map(lambda unit: runs(tree, env, unit[3], unit[1], work / "runs" / str(unit[4]),
                                                       gates[(unit[0], unit[1])], waits),
                                     [(*unit, number) for number, unit in enumerate(units)]))
        ran = [did for did, _ in measured]
        boundary = dict.fromkeys(gates, False)
        for (form, variant, _, _), (_, late) in zip(units, measured):
            boundary[(form, variant)] = boundary[(form, variant)] or late
        not_run = [f"{form} ({variant}, step {index + 1})" for (form, variant, index, _), did in zip(units, ran) if not did]
        read = read_with(module, tree, {f"{form}#{variant}": workflow(steps)
                                        for form, variants in generated.items()
                                        for variant, (_, steps) in variants.items()})
    finally:
        shutil.rmtree(work, ignore_errors=True)
    for unit in not_run:
        print(f"GENERATOR ERROR {unit}: bash does not run it as its variant asks (the module header says how)")
    rows, problems = listed_misses()
    used, unlisted, listed, false, over, mislabelled = set(), [], [], [], [], []
    for form, variants in generated.items():
        for variant, (_, steps) in variants.items():
            detached = boundary[(form, variant)]
            # An UNSEEN form carries no claim about its events: rule 8 does not read its command.
            if variant == "kept" and classes[form] != UNSEEN and detached != (classes[form] == DETACHED):
                mislabelled.append((form, "detached" if detached else "keeps", classes[form]))
            found = verdict(classes[form], variant, read[f"{form}#{variant}"], len(steps), detached)
            if found is None:
                if "--list" in args:
                    print(f"{form}\t{variant}\tread")
                continue
            row = next((pattern for pattern in rows if fnmatch.fnmatchcase(form, pattern)), None)
            # A refusal of a form bash keeps, and a refusal of a negative form, are over-refusals:
            # each fails the generator unless a row of the list names it with its reason.
            if row is None and found.startswith(("false:", "over:")):
                false.append((form, variant, found))
                continue
            if row:
                used.add(row)
            (over if found.startswith(("false:", "over:")) else listed if row else unlisted).append((form, variant, found))
            if "--list" in args:
                print(f"{form}\t{variant}\t{found}" + (f"\tlisted ({MISSES.name}:{rows[row]})" if row else ""))
    problems += [f"{MISSES.name}:{number}: {pattern} names no form rule 8 misses or refuses: remove the row"
                 for pattern, number in sorted(rows.items(), key=lambda row: row[1])
                 if pattern not in used and any(fnmatch.fnmatchcase(form, pattern) for form in generated)]
    for form, variant, found in unlisted:
        print(f"UNLISTED {form} ({variant}): {found.removeprefix('miss: ')}; {MISSES.name} does not list it")
    for form, variant, found in false:
        print(f"FALSE REFUSAL {form} ({variant}): {found.split(': ', 1)[1]}")
    for form, measured_class, labelled in mislabelled:
        print(f"MISLABELLED {form}: bash leaves its kept step's events {'un' if measured_class == 'detached' else ''}written "
              f"when it returns, so its class is {measured_class}, and CONTEXTS or OUTERS calls it {labelled}")
    for problem in problems:
        print(problem)
    variants = sum(len(v) for v in generated.values())
    negatives = sum(1 for form in generated if classes[form] == "negative")
    print(f"go_test_step_forms: {len(generated)} forms ({len(CONTEXTS)} contexts, {len(SPELLINGS)} go spellings, "
          f"{len(INDIRECTIONS)} indirections, {len(OUTERS)} outers, {len(NEGATIVES)} negatives"
          f"{'' if only == '*' else f', those --only {only} matches'}), {negatives} of them negative: {variants} "
          f"form variants, {len(units)} steps, {sum(1 for form, variant, _, _ in units if gates[(form, variant)])} "
          f"of them holding their go test at the gate; bash runs {len(units) - len(not_run)} of them as "
          f"their variant asks and leaves {sum(1 for key in boundary if boundary[key])} kept form(s) detached at its return, which "
          f"{len(mislabelled)} form(s) are labelled against; rule 8 of {module} misses {len(listed) + len(unlisted)} "
          f"form variant(s), {len(listed)} listed in {MISSES.name} and {len(unlisted)} unlisted, refuses {len(over)} "
          f"negative form(s) the list accepts, and refuses {len(false)} form variant(s) falsely")
    if not_run:
        return 2
    return 1 if unlisted or false or problems or mislabelled else 0


if __name__ == "__main__":
    sys.exit(main())
