#!/usr/bin/env python3
"""Run one bun test command of a CI step, and leave the record bun_test_record.py describes: bun's
JUnit report where the job's upload looks for it, a marker of the run's start and, once bun has
ended, its exit status and its count of errors beside it.

    bun_test_run.py <name> bun test [ARGS...]
    bun_test_run.py <name> bun run <script> [ARGS...]

It runs the command with `--reporter=junit --reporter-outfile=<report>` added at its end and exits
with bun's exit status. The report is `$RUNNER_TEMP/bun-test-reports/<the working directory, from
GITHUB_WORKSPACE>/<name>.xml`. Before bun starts it writes `<report>.run` and, for a -t
(--test-name-pattern) pattern on the command line or in the script `bun run` runs, `<report>.t`;
once bun has ended, or could not start, it writes `<report>.exit`. bun's standard output reaches
the step as bun writes it. The wrapper's copier copies bun's standard error byte for byte.
The copier may walk a summary block before the wrapper reaps bun. At exit status 0 the wrapper
records zero errors regardless of what the copier found: bun 1.4.4 exits non-zero whenever its
summary counts an error. Each `Ran N tests across M files.` line may end a summary block. The
copier walks back over the preceding lines, split only at line feeds, ignoring ANSI codes.
A whole block starts at ` N pass` and may include `snapshots: ` and other count lines. Counts
must have 1 to 19 ASCII digits. A whole block sets the count to its ` N error` count, or zero
if it has none. A line outside the block leaves the count unchanged. At a non-zero status the
count is null when the wrapper read no whole summary block (for example, bun printed none,
the start was cut from the wrapper's 64 KiB buffer, a line inside the block was not a summary
line, or the step's log reader stalled past DRAIN_SECONDS before the wrapper reached the block).
bun's report leaves out a file that did not load and an error between tests. When bun has ended,
the copy goes on for at most DRAIN_SECONDS more, for a process that still holds bun's standard
error.

When bun exits 0 and its run breaks a clause of bun_test_record.py's rule (bun 1.4.4 exits 0 and
writes no report when it cannot write its outfile, and when a test calls process.exit), the
wrapper exits 1 and names each clause. This fails the step for a plain run. For a run
`.github/scripts/harness-pool.sh` starts, the step that queues it passes. The pool's wait step
reports the wrapper's verdict, names the suite, and fails if the wrapper exits 1.
A non-zero exit is bun's, and the record says why.

A SIGINT or SIGTERM sent to this script is passed to bun, and bun's exit status is still written.
On a cancel or a step's timeout-minutes the runner signals the step's shell alone, not this
script, so this script and bun run on until the job ends, and an upload before then finds the
run's marker with no `.exit`: clause 1 makes that run unknown.

Exit status: bun's, 1 as above, 127 when bun could not start, or 2, before bun starts, when this
was called wrongly: a name NAME does not match; a command that is not `bun test` or `bun run`;
a command, or the script it runs, that names its own reporter or gives two -t patterns; a -t
pattern that is not a literal name, whether the pattern is attached to the flag or given as the
next word (bun_test_record.literal_fault); a name another run of the job took; a `bun run` from a
directory with no package.json (bun would run at the package's root, so each test's file would
be read from the wrong directory); or a script with a line break or any other form that is not
one `bun test` command (bun gives the reporter flags to a script's last command alone). It also
refuses to run outside a runner (no RUNNER_TEMP or GITHUB_WORKSPACE, or a working directory outside
GITHUB_WORKSPACE).
.github/scripts/check-workflow-files.sh's rule 8 (bun_test_steps.py) holds every bun test step of
ci.yaml's run to this script; its tests are in bun_test_reports.test.sh, which runs it with the
real bun.
"""
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import threading
from pathlib import Path

# The record's module beside this file; importing it leaves no __pycache__ in the checkout.
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from bun_test_record import (  # noqa: E402
    EXIT, MARKER, NAME, PATTERN, REPORTS_DIR, beside, literal_fault, read_run, report_of, write_exit)

PATTERN_FLAGS = ("-t", "--test-name-pattern")
# One `bun test` command with no shell operator: the only script `bun run` may run here.
ONE_BUN_TEST = re.compile(r"bun[ \t]+test(?:[ \t]+[^\s;&|<>()`$'\"\\]+)*")
SUMMARY_LINE = re.compile(r"\s*[0-9]{1,19} \S.*|snapshots: .*")
SUMMARY_START = re.compile(r"\s*[0-9]{1,19} pass\s*")
ERRORS = re.compile(r"\s*([0-9]{1,19}) errors?\s*")
ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")
SUMMARY_END_BYTES = re.compile(
    rb"^(?:" + ANSI.pattern.encode() + rb")*Ran [0-9]{1,19} tests? across [0-9]{1,19} files?\.",
    re.MULTILINE,
)
NULL = None
NOT_A_BLOCK = object()
TAIL_BYTES = 64 * 1024
DRAIN_SECONDS = 5


def refuse(message: str) -> int:
    print(f"::error::bun_test_run.py: {message}", file=sys.stderr)
    return 2


def patterns_of(command: list) -> list:
    """Every pattern from `-t <p>`, `-t<p>`, `-t=<p>`, or the long flag with a space or `=`."""
    found = []
    for at, word in enumerate(command):
        if word in PATTERN_FLAGS and at + 1 < len(command):
            found.append(command[at + 1])
        elif word.startswith("-t") and len(word) > 2:
            found.append(word[2:].removeprefix("="))
        elif word.startswith("--test-name-pattern="):
            found.append(word.split("=", 1)[1])
    return found


def bun_words(command: list):
    """(the words of the `bun test` the command runs, None), or (None, why it cannot keep one
    report of its tests). For `bun run <script>`, those are the script's words, then the command's
    own after the script's name, as bun appends them."""
    if command[1] != "run":
        return command, None
    manifest = Path("package.json")
    if not manifest.is_file():
        return None, (f"`bun run` from {os.getcwd()}, which holds no package.json: bun runs the script at the "
                      "package's root, so each test's file would be read from the wrong directory")
    if len(command) < 3:
        return None, "`bun run` names no script"
    try:
        script = (json.loads(manifest.read_text()).get("scripts") or {}).get(command[2])
    except ValueError as error:
        return None, f"package.json does not parse: {error}"
    if not isinstance(script, str):
        return None, f"package.json has no script {command[2]!r}"
    if not ONE_BUN_TEST.fullmatch(script.strip(" \t")):
        return None, (f"the script {command[2]!r} is {script!r}, not one `bun test` command: bun gives the "
                      "reporter flags to a script's last command alone")
    return shlex.split(script) + command[3:], None


def block_errors(buffer: bytes | bytearray, end: int, trimmed: bool):
    """Read the block ending at the trigger's start offset, without reading earlier lines."""
    errors = None
    while end:
        line_end = end - 1  # Skip the line feed immediately before end.
        start = buffer.rfind(b"\n", 0, line_end) + 1
        if start == 0 and trimmed:
            return NULL
        line = ANSI.sub("", buffer[start:line_end].decode(errors="replace"))
        if SUMMARY_START.fullmatch(line):
            return 0 if errors is None else errors
        if not SUMMARY_LINE.fullmatch(line):
            return NOT_A_BLOCK
        if errors is None and (counted := ERRORS.fullmatch(line)):
            errors = int(counted.group(1))
        end = start
    return NULL if trimmed else NOT_A_BLOCK


def main(argv: list) -> int:
    if len(argv) < 3 or not NAME.fullmatch(argv[0]) or argv[1] != "bun" or argv[2] not in ("test", "run"):
        return refuse("give a report name, then the command, which is `bun test …` or `bun run <script> …`; "
                      f"got {shlex.join(argv)!r}")
    name, command = argv[0], argv[1:]
    words, fault = bun_words(command)
    if fault:
        return refuse(fault)
    # What bun runs, the script's words included, so a -t or a reporter a script gives is read too.
    shown = shlex.join(words) + (f" (`{shlex.join(command)}`)" if command[1] == "run" else "")
    if any(word.startswith("--reporter") for word in words):
        return refuse(f"the command names a reporter of its own, and this one adds bun's JUnit reporter: {shown}")
    patterns = patterns_of(words)
    if len(patterns) > 1:
        return refuse(f"the command gives {len(patterns)} -t patterns, and its report keeps one: {shown}")
    if patterns:
        fault = literal_fault(patterns[0])
        if fault:
            return refuse(f"the -t pattern {patterns[0]!r} is not a literal name: {fault}")
    temp, workspace = os.environ.get("RUNNER_TEMP"), os.environ.get("GITHUB_WORKSPACE")
    if not temp or not workspace:
        return refuse("RUNNER_TEMP and GITHUB_WORKSPACE name where the report goes; this runs under a runner")
    where = os.path.relpath(os.path.realpath(os.getcwd()), os.path.realpath(workspace))
    if where == ".." or where.startswith("../"):
        return refuse(f"the working directory {os.getcwd()} is outside GITHUB_WORKSPACE {workspace}")
    directory = Path(temp) / REPORTS_DIR / ("" if where == "." else where)
    report = report_of(directory, name)
    marker, ended, selection = (beside(report, suffix) for suffix in (MARKER, EXIT, PATTERN))
    directory.mkdir(parents=True, exist_ok=True)
    try:
        with marker.open("x") as file:
            file.write(shlex.join(command) + "\n")
    except FileExistsError:
        return refuse(f"another run of this job already wrote {marker}: give this run a name of its own")
    if patterns:
        selection.write_text(patterns[0])

    try:
        bun = subprocess.Popen([*command, "--reporter=junit", f"--reporter-outfile={report}"], stderr=subprocess.PIPE)
    except OSError as error:
        write_exit(ended, 127, None, start=str(error))
        print(f"::error::bun_test_run.py: bun could not start: {error}", file=sys.stderr)
        return 127
    last_errors = None

    def copy() -> None:
        nonlocal last_errors
        tail = bytearray()
        trimmed = False
        while chunk := os.read(bun.stderr.fileno(), 65536):
            sys.stderr.buffer.write(chunk)
            sys.stderr.buffer.flush()
            if bun.returncode == 0:
                continue
            old_end = len(tail)
            tail.extend(chunk)
            # Start at the last existing line so a trigger may span two reads.
            start = tail.rfind(b"\n", 0, old_end) + 1
            for match in SUMMARY_END_BYTES.finditer(tail, start):
                if match.end() > old_end:
                    errors = block_errors(tail, match.start(), trimmed)
                    if errors is not NOT_A_BLOCK:
                        last_errors = errors
            if len(tail) > TAIL_BYTES:
                del tail[:-TAIL_BYTES]
                trimmed = True

    copier = threading.Thread(target=copy, daemon=True)
    copier.start()

    def forward(signum, _frame):
        bun.send_signal(signum)

    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, forward)
    status = bun.wait()
    copier.join(DRAIN_SECONDS)
    status = 128 - status if status < 0 else status
    write_exit(ended, status, 0 if status == 0 else last_errors)
    if status == 0:
        _, reasons = read_run(report, report.name)
        for reason in reasons:
            print(f"::error::bun exited 0 and its run {report} breaks {reason}", file=sys.stderr)
        if reasons:
            return 1
    return status


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
