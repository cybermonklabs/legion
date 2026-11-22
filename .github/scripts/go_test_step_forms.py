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
    -okdir; in env -S's string and in the string of `bash -c`, `bash -o pipefail -c`, `bash
    --noprofile --norc -c` and `sh -c`, on one line or over several; in a heredoc a shell runs; in
    the background with a `wait` for it (on the next line or the same, in a brace group, of `$!`
    or of a variable assigned `$!`); in the background (alone, at a pipeline's or an and-or list's
    end, in a subshell or a brace group, in a `bash -c` string, an env -S string or a heredoc a
    shell runs) or behind `setsid -f` with no wait for it, or with a wait that waits for nothing:
    in a subshell, a pipeline, an `if false`, an uncalled function, itself in the background, or
    outside the subshell the `&` stands in (DETACHED); and behind each command rule 8 does not
    read (UNSEEN);
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
A composed form is one of OUTERS around each context, the spelling `go`, in the step's run:
(`composed/<outer>/<context>/go`): a compound or a runner that holds a whole script (in the
background, with a wait after it or not, behind `!`, in a subshell, a brace group, an if or after
`&&`, behind time, in `bash -c`, `sh -c` or a heredoc bash runs, and in `bash -c` behind env, nice,
timeout, setsid -w, xargs and a redirection). It keeps its events when both keep them, runs
detached when either does, and is unseen when its context is. dash, which runs `sh -c`, has none
of the forms SH_SKIPS names (`|&`, `&>`, `time`, coproc, `<<<`), so `sh -c` holds no such context.
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
bash runs none: a shell that runs nothing (`bash --help -c`, `-n`, `-o noexec`), a variable the
step assigns no go, an argument, a quoted text, a runner's --help, a go test that compiles or lists
only, and code no run reaches (`if false`). Rule 8 must refuse no negative form, but one a row of
the list names: it reads a go test in every branch, so the `dead-` forms are refused as an
over-refusal the list accepts.

bash runs every form, the kept variants too, and every step, with a recording stand-in go first on
PATH (and GO and GOROOT naming it), which writes one test's events when go test has -json: a bare
step must run `go test ./pkg`, a kept one must also leave an event in the TEST_EVENTS_FILE the
writer appends to, and a negative one must run no test (a `-c` or a `-list` runs none). A form bash
does not run that way is an error of this generator (exit 2).

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

import fnmatch
import inspect
import json
import os
import shutil
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
STAND_IN = """#!/bin/sh
printf '%s\\n' "$*" >> "$FORMS_RECORD"
if [ "$1" = env ] && [ "$2" = GOROOT ]; then printf '%s\\n' "$GOROOT"; exit 0; fi
for word in "$@"; do
  if [ "$word" = -json ]; then printf '%s\\n' {events}; fi
done
""".format(events=" ".join("'" + event + "'" for event in EVENTS))


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
# `&>` it reads as `&` and `>`, and it has no `time` keyword, no coproc and no `function` keyword.
SH_SKIPS = ("before-pipe-both", "redirect-both", "redirect-first-here-string", "time", "time-p", "coproc",
            "here-string", "function-keyword")
# Each negative form: a step that names go test where bash runs none.
NEGATIVES = {
    "bash-help": f"bash --help -c {sq(GO_LINE)}",
    "bash-version": f"bash --version -c {sq(GO_LINE)}",
    "bash-noexec": f"bash -n -c {sq(GO_LINE)}",
    "bash-noexec-cluster": f"bash -nc {sq(GO_LINE)}",
    "bash-noexec-option": f"bash -o noexec -c {sq(GO_LINE)}",
    "bash-dump-strings": f"bash --dump-strings -c {sq(GO_LINE)}",
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
            form = f"composed/{outer}/{context}/go"
            out[form] = {variant: indirect("run", lambda x: around(wrap(x)), command, "")
                         for variant, command in (("bare", GO_LINE), ("kept", f"{writer} {GO_LINE}"))}
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


def runs(tree: Path, env: dict, text: str, variant: str, scratch: Path) -> bool:
    """Whether bash runs the step `text` at the tree's root as its variant asks: a bare step runs
    the stand-in's go test, a kept one also leaves an event of it in TEST_EVENTS_FILE, and a
    negative one runs no go test of the package."""
    scratch.mkdir(parents=True)
    record, events, step = scratch / "record", scratch / "events.jsonl", scratch / "step.sh"
    step.write_text(text + "\n")
    run_env = dict(env, FORMS_RECORD=str(record), TEST_EVENTS_FILE=str(events), FORMS_LOCK=str(scratch / "lock"))
    try:
        subprocess.run(["bash", "--noprofile", "--norc", "-eo", "pipefail", str(step)], cwd=tree, env=run_env,
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60)
    except subprocess.TimeoutExpired:
        return False
    if variant == "negative":
        return not ran_test(record.read_text().split("\n") if record.is_file() else [])
    # A detached form's go test may still be running when bash has returned.
    deadline = time.monotonic() + 10
    while True:
        ran = ran_test(record.read_text().split("\n") if record.is_file() else [])
        kept = variant == "bare" or (events.is_file() and EVENTS[1] in events.read_text())
        if (ran and kept) or time.monotonic() > deadline:
            return ran and kept
        time.sleep(0.05)


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


def verdict(keeps: str, variant: str, read: dict, steps: int) -> str | None:
    """What is wrong with rule 8's reading of one variant of a form of class `keeps`: None, or
    `miss: ...` or `false: ...` (`over: ...` for a negative form, which a row may list)."""
    problems = read["problems"]
    if variant == "negative":
        return f"over: refused or counted: {' | '.join(problems) or read['commands']}" if problems or read["commands"] else None
    if variant == "bare":
        unnamed = [n for n in range(1, steps + 1) if not any("other than through" in t for t in names(problems, n))]
        return f"miss: bare, step(s) {', '.join(map(str, unnamed))} not refused" if unnamed else None
    if keeps == DETACHED:
        unnamed = [n for n in range(1, steps + 1)
                   if not any("before the command has written them" in t for t in names(problems, n))]
        return f"miss: detached, step(s) {', '.join(map(str, unnamed))} not refused" if unnamed else None
    if problems and keeps == KEEPS:
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
        with ThreadPoolExecutor(max_workers=len(os.sched_getaffinity(0))) as pool:
            ran = list(pool.map(lambda unit: runs(tree, env, unit[3], unit[1], work / "runs" / str(unit[4])),
                                [(*unit, number) for number, unit in enumerate(units)]))
        not_run = [f"{form} ({variant}, step {index + 1})" for (form, variant, index, _), did in zip(units, ran) if not did]
        read = read_with(module, tree, {f"{form}#{variant}": workflow(steps)
                                        for form, variants in generated.items()
                                        for variant, (_, steps) in variants.items()})
    finally:
        shutil.rmtree(work, ignore_errors=True)
    for unit in not_run:
        print(f"GENERATOR ERROR {unit}: bash does not run it as its variant asks (the module header says how)")
    rows, problems = listed_misses()
    used, unlisted, listed, false, over = set(), [], [], [], []
    for form, variants in generated.items():
        for variant, (_, steps) in variants.items():
            found = verdict(classes[form], variant, read[f"{form}#{variant}"], len(steps))
            if found is None:
                if "--list" in args:
                    print(f"{form}\t{variant}\tread")
                continue
            row = next((pattern for pattern in rows if fnmatch.fnmatchcase(form, pattern)), None)
            if found.startswith("false:") or (found.startswith("over:") and not row):
                false.append((form, variant, found))
                continue
            if row:
                used.add(row)
            (over if found.startswith("over:") else listed if row else unlisted).append((form, variant, found))
            if "--list" in args:
                print(f"{form}\t{variant}\t{found}" + (f"\tlisted ({MISSES.name}:{rows[row]})" if row else ""))
    problems += [f"{MISSES.name}:{number}: {pattern} names no form rule 8 misses or refuses: remove the row"
                 for pattern, number in sorted(rows.items(), key=lambda row: row[1])
                 if pattern not in used and any(fnmatch.fnmatchcase(form, pattern) for form in generated)]
    for form, variant, found in unlisted:
        print(f"UNLISTED {form} ({variant}): {found.removeprefix('miss: ')}; {MISSES.name} does not list it")
    for form, variant, found in false:
        print(f"FALSE REFUSAL {form} ({variant}): {found.split(': ', 1)[1]}")
    for problem in problems:
        print(problem)
    variants = sum(len(v) for v in generated.values())
    negatives = sum(1 for form in generated if classes[form] == "negative")
    print(f"go_test_step_forms: {len(generated)} forms ({len(CONTEXTS)} contexts, {len(SPELLINGS)} go spellings, "
          f"{len(INDIRECTIONS)} indirections, {len(OUTERS)} outers, {len(NEGATIVES)} negatives"
          f"{'' if only == '*' else f', those --only {only} matches'}), {negatives} of them negative: {variants} "
          f"form variants, {len(units)} steps; bash runs {len(units) - len(not_run)} of them as their variant asks; "
          f"rule 8 of {module} misses {len(listed) + len(unlisted)} form variant(s), {len(listed)} listed in "
          f"{MISSES.name} and {len(unlisted)} unlisted, refuses {len(over)} negative form(s) the list accepts, and "
          f"refuses {len(false)} form variant(s) falsely")
    if not_run:
        return 2
    return 1 if unlisted or false or problems else 0


if __name__ == "__main__":
    sys.exit(main())
