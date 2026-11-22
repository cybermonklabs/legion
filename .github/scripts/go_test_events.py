#!/usr/bin/env python3
"""Run a CI step's `go test` and keep the result of every test it ran, as go's own test2json
events, in a file the job uploads, while the step's log shows what go test prints without -json.

Without -v or -json, `go test` prints one `ok <package> <time>` line for a package that passed,
so a step's log says nothing about which tests ran, passed or skipped, and a reader who must know
that one named test passed in one job cannot tell it from the log. -v would say it, and would
grow a log such as daemon-go's 31,000 lines many times over. So each go test step of the CI
router's jobs runs under this script, which runs the command with -json, appends every event to
the step's events file and prints the log the command prints without -json: the step's verdict,
its exit status and its time limits are go test's own.

    go_test_events.py go [-C DIR] test [flags] [packages] [-args ...]
    go_test_events.py COMMAND [ARGS...]

The first form runs `go test -json` with the step's own flags, -json added first, and refuses a
step that passes -json itself, or that hands the test binary a -test.v of its own (after `-args`,
`--` or the package list), which would decide what -json reports apart from go test's -v. The
second runs COMMAND, a script that runs go test -json itself
and reads the report (packages/envoy/scripts/database-skips.sh, backend-count.sh), with
TEST_EVENTS_FILE set to the events file: such a script tees each event there as go writes it, on
the go test's own line, and leaves its own output as it is. A script that runs the first form
(packages/envoy/infra/scripts/race-unit.sh) appends to the same file.

The events file is `$RUNNER_TEMP/go-test-events/<step id>.jsonl`, the step id being
GITHUB_ACTION, which GitHub sets to the step's `id:`. A step with no `id:` gets `__run` or
`__run_<n>`, a name that moves when a step is added above it, so this script refuses one: the
file's name is what a reader asks for. The job's upload step sends that directory as the artifact
`go-test-events-<job id>` (the job's check run id, which is the id the jobs API and a job's URL
give). TEST_EVENTS_FILE names the file instead, for a run under another one of this script or
outside CI; under a runner (RUNNER_TEMP set) it must be a file of that directory, so nothing is
kept where the upload does not look, and it must be a regular file wherever it is, so /dev/null
and the like are refused. Each line of the file is one event as go wrote it, and a run of one test
is found as `{"Action":"pass","Package":...,"Test":"<the test's full name>"}`; a skipped test's
event says "skip", so a skip is no pass.

The step fails when the file holds no event from this run once it has ended, whatever go test's
own status: an empty or missing file would otherwise read as a step that ran no test. A go test
that ran builds or tests always writes events, a build failure among them, so this fires only when
the events were lost: a stand-in go, a script that kept its report to itself, a file that could not
be written or kept.

The log is the one go test prints without -json, rebuilt from the events. go test prints a test
binary's output as the binary writes it: what a test prints to stdout, where it prints it; a failed
test's report (its `--- FAIL:` line, then its t.Log and t.Error lines indented under it, each failed
subtest nested in its parent) once the test ends; and every line the package prints outside a test.
It holds that output back until the package ends, prints it only for a package that failed (or,
with -list, for every package), ends a line the binary left open before the package's summary
line (`ok`, `?`, `FAIL`), and prints the packages in the order they started; with no package
named, with -bench or with -fuzz it prints the output as it comes, a passing package's too; and it
ends a run of named packages, -fuzz aside, with its own `FAIL` line when a package failed. A run
never ended (an exit inside a test, a timeout) prints no report, as go test prints none of its
buffered lines, and its dump is the binary's own output. Build output goes to stderr, as go test
writes it. Flags are read as go reads them (GoArgs, the reader of go test's words this script and
.github/scripts/go_test_steps.py, rule 8 of the workflow file check, share), GOFLAGS first and the
last of a flag winning: with -v true (`-v`, `-v=1`, `-test.v`, in GOFLAGS) the log is the one -v
prints, every line, with the `=== NAME` lines -json leaves out. -json reports each test only with
-v unset or true, so the script takes every -v of go test's off the command line and out of
GOFLAGS and passes its own -v when the step's last one is true: a step's `-v=false` keeps every
test's result too.

-json gives one thing no event says: which of a test's lines it logged and which it printed. A
test's lines are its log when the testing package marked them as an error (OutputType "error" or
"error-continue"), when they are laid out as t.Log lays a line out (four spaces, then
`<file>:<line>: `, the file a //line directive may name) or continue such a line (eight spaces,
or the rest of a line test2json cut in pieces), and when they come after the test's own `--- FAIL:`
line, which is what a fuzz test's report holds; a print that ended no line and the t.Log line
test2json joins to it are told apart where that layout starts. Every other line is a print. What
this cannot tell apart is a print whose text holds that layout itself, since its events are the
same bytes as a print and a t.Log line's: from where the layout starts the line is read as the
test's log, so in a failed test it is printed in the test's report rather than before it, and for
a test that passed in a failed package it is not printed. A t.Log line is never read as a print,
so the log never gains a line. A line of go's stdout that is no event is printed as it is and
kept out of the file.

Exit status: go test's (or COMMAND's), 1 when the run wrote no event, 2 when this script was
called wrongly or cannot name or open the events file.
"""

from __future__ import annotations

import json
import os
import re
import signal
import subprocess
import sys
from pathlib import Path

ENV_FILE = "TEST_EVENTS_FILE"
DIRECTORY = "go-test-events"
# A step id as GitHub reads one; the generated ids (`__run`, `__run_2`, `__owner_action`) start
# with two underscores.
STEP_ID = re.compile(r"^[A-Za-z_][A-Za-z0-9_-]*$")
# go test's own flags (go help test, go help testflag, go help build), each read with or without
# the `test.` the test binary's own spelling carries: those that take no value, and those whose
# value is the next word when it is not written with `=`. Every other flag is the test binary's,
# which go test hands on.
BOOL_FLAGS = frozenset("""
    a artifacts asan benchmem buildvcs c cover failfast fullpath json linkshared modcacherw msan n
    race short trimpath v work x
""".split())
VALUE_FLAGS = frozenset("""
    C asmflags bench benchtime blockprofile blockprofilerate buildmode compiler count
    coverpkg covermode coverprofile cpu cpuprofile exec fuzz fuzzcachedir fuzzminimizetime
    fuzztime gccgoflags gcflags installsuffix ldflags list memprofile memprofilerate mod modfile
    mutexprofile mutexprofilefraction o outputdir overlay p parallel pgo pkgdir run shuffle
    skip tags timeout toolexec trace vet
""".split())
# The test-level actions that end a run of a test.
ENDED = frozenset({"pass", "fail", "skip"})
INDENT = "    "
# A line as t.Log and t.Error lay it out: four spaces, the file and line, then the message; and
# the eight spaces each further line of one message starts with, the label being the file's name
# or what a //line directive names. LOG_INSIDE finds such a start after a print that ended no
# line, which test2json joins to it.
LOG_START = re.compile(r"^    [^\s:][^:\n]*:\d+: ")
LOG_INSIDE = re.compile(r"    [^\s:][^:\n]*:\d+: ")
LOG_CONTINUE = INDENT * 2
# The values Go's boolean flags take (strconv.ParseBool).
TRUE = frozenset({"1", "t", "T", "TRUE", "true", "True"})
# go test's summary of a package, before which it ends a line the binary left open.
SUMMARY = re.compile(r"^(?:ok  |FAIL|\?   )\t")


class Refusal(Exception):
    """A call this script will not run: printed, and the exit status is 2."""


def events_path(environ) -> Path:
    """The file the step's events go to: the step's own file under $RUNNER_TEMP/go-test-events,
    named by its id, or TEST_EVENTS_FILE, which a runner's step must keep in that directory. The
    path is absolute, since a script it is handed to may run from another directory."""
    named = environ.get(ENV_FILE, "")
    temp = environ.get("RUNNER_TEMP", "")
    if named:
        path = Path(named).absolute()
        if temp:
            uploaded = (Path(temp) / DIRECTORY).resolve()
            if uploaded not in path.resolve().parents:
                raise Refusal(f"{ENV_FILE} is {named}, outside {uploaded}, the directory this job "
                              f"uploads, so its events would be kept nowhere a reader looks")
        return path
    step = environ.get("GITHUB_ACTION", "")
    if not step or not temp:
        raise Refusal(f"GITHUB_ACTION and RUNNER_TEMP name the events file in a CI step, and "
                      f"{'neither is' if not step and not temp else 'GITHUB_ACTION is not' if not step else 'RUNNER_TEMP is not'} "
                      f"set; outside CI, set {ENV_FILE} to the file the events go to")
    if step.startswith("__") or not STEP_ID.match(step):
        raise Refusal(f"this step's id is `{step}`, which GitHub made up because the step has no "
                      f"`id:` of its own and which moves when a step is added above it; give the "
                      f"step an `id:`, the name its events file and its reader go by")
    return Path(temp).absolute() / DIRECTORY / f"{step}.jsonl"


def go_split(text: str) -> list[str]:
    """GOFLAGS's words as go reads them (cmd/internal/quoted.Split): split at blanks, a word that
    starts with ' or " running to the same quote. A quote left open raises ValueError."""
    words, text = [], text
    while True:
        text = text.lstrip(" \t\n\r")
        if not text:
            return words
        if text[0] in "'\"":
            end = text.find(text[0], 1)
            if end < 0:
                raise ValueError(f"GOFLAGS holds an unterminated {text[0]} string")
            words.append(text[1:end])
            text = text[end + 1:]
            continue
        end = min((at for at in (text.find(blank) for blank in " \t\n\r") if at >= 0), default=len(text))
        words.append(text[:end])
        text = text[end:]


def go_join(words: list[str]) -> str:
    """The words written back as go_split reads them (cmd/internal/quoted.Join)."""
    out = []
    for word in words:
        if not any(char in word for char in " \t\n\r'\""):
            out.append(word)
        elif "'" not in word:
            out.append(f"'{word}'")
        elif '"' not in word:
            out.append(f'"{word}"')
        else:
            raise ValueError(f"GOFLAGS's word {word!r} holds both quotes")
    return " ".join(out)


def parse_event(line: bytes) -> dict | None:
    """The test2json event a line of go's stdout is, or None for any other line."""
    try:
        event = json.loads(line)
    except ValueError:
        return None
    return event if isinstance(event, dict) and "Action" in event else None


class GoArgs:
    """go test's words as cmd/go's testFlags reads them (src/cmd/go/internal/test/testflag.go):

    - `flags`: each of go test's own flags, by its name less `test.`, and its last value; a flag
      of BOOL_FLAGS written bare reads "true", and one of VALUE_FLAGS written bare takes the next
      word, whatever it is (so `-skip -args` is a pattern). A value this reader cannot know (a word
      given as None) is None.
    - `packages`: the words go test reads as packages.
    - `own`: the spans (start, end) of the words of go test's own flags, in order, and their names.
    - `rest`: where the words stop being go test's: at a `--`, which with every word after it go
      test hands to the test binary unread; after a bare `-args`; at a word that is no flag once
      the package list has ended and the word before it is no flag of the binary's that may take
      it as a value; or at the end.

    A flag that is not go test's is the binary's: go test hands it on, and the word after one
    written without `=` may be its value, so it is passed over as cmd/go passes it over. A word
    given as None is read as no flag."""

    __slots__ = ("flags", "packages", "own", "rest")

    def __init__(self, words: list[str | None]) -> None:
        self.flags: dict[str, str | None] = {}
        self.packages: list[str | None] = []
        self.own: list[tuple[int, int, str]] = []
        self.rest = len(words)
        list_ended = in_list = after_bare_unknown = False
        index = 0
        while index < len(words):
            word = words[index]
            was_after_bare_unknown, after_bare_unknown = after_bare_unknown, False
            if word == "--":
                self.rest = index
                return
            if word is None or not word.startswith("-") or word == "-":
                if not in_list and list_ended:
                    if was_after_bare_unknown:
                        index += 1
                        continue
                    self.rest = index
                    return
                in_list = list_ended = True
                self.packages.append(word)
                index += 1
                continue
            in_list = False
            name, eq, value = word[2:].partition("=") if word.startswith("--") else word[1:].partition("=")
            base = name.removeprefix("test.")
            if base in BOOL_FLAGS:
                self.flags[base] = value if eq else "true"
                self.own.append((index, index + 1, base))
                index += 1
            elif base in VALUE_FLAGS:
                if not eq:
                    value = words[index + 1] if index + 1 < len(words) else ""
                self.flags[base] = value
                end = index + (1 if eq else 2)
                self.own.append((index, min(end, len(words)), base))
                index = end
            else:
                list_ended = True
                if name == "args" and not eq:
                    self.rest = index + 1
                    return
                after_bare_unknown = not eq
                index += 1


class Mode:
    """What go test prints for these arguments, read as go reads its flags (GoArgs): GOFLAGS
    first, then the command line, and the last of a flag winning. It is -v's log or the plain one;
    it streams a binary's output as it comes with no package named, -bench or -fuzz; it prints a
    passing package's output with -list; and it ends a run of named packages with its own `FAIL`
    line, -fuzz aside. The words go test hands the test binary (after a `--`, after `-args`, or
    after the package list) are the binary's own, and go test's -v alone decides the log.

    -json reports each test only in verbose mode, so a step's `-v=false` would leave no test's
    result in the events: the writer takes every -v of go test's off the command line and out of
    GOFLAGS (`args`, `goflags`) and passes its own -v on the command line when the step's last one
    is true, the one form whose -json stream lays a run out as go test -v prints it. A -test.v the
    step hands the binary would decide what -json reports apart from go test's -v, as the
    binary's own flags read it, so the writer refuses one (`binary_verbose`) rather than guess."""

    def __init__(self, args: list[str], environ) -> None:
        self.goflags = None
        values: dict[str, str | None] = {}
        if "GOFLAGS" in environ:
            try:
                words = go_split(environ["GOFLAGS"])
                scanned = GoArgs(words)
                values.update(scanned.flags)
                verbose_spans = [(start, end) for start, end, name in scanned.own if name == "v"]
                if verbose_spans:
                    # Only a GOFLAGS that holds a -v is written anew, each word quoted as go reads it.
                    self.goflags = go_join(drop_spans(words, verbose_spans))
            except ValueError:
                # A GOFLAGS go cannot read either: go test says so, as it would without this script.
                values.clear()
        scanned = GoArgs(args)
        self.json = scanned.flags.get("json", "false") in TRUE
        values.update(scanned.flags)
        own = drop_spans(args[: scanned.rest], [(start, end) for start, end, name in scanned.own if name == "v"])
        handed = args[scanned.rest:]
        self.binary_verbose = next((word for word in handed if BINARY_VERBOSE.match(word)), None)
        self.args = own + handed
        self.verbose = values.get("v", "false") in TRUE
        fuzz, bench, listing = (values.get(name, "") or "" for name in ("fuzz", "bench", "list"))
        self.streams = not scanned.packages or bool(fuzz) or bool(bench)
        self.shows_pass = bool(listing)
        self.closes = bool(scanned.packages) and not fuzz


# The test binary's own verbosity flag, as a word go test hands it.
BINARY_VERBOSE = re.compile(r"^--?test\.v(?:=|$)")


def drop_spans(words: list[str], spans: list[tuple[int, int]]) -> list[str]:
    """The words less those in the spans."""
    dropped = {at for start, end in spans for at in range(start, end)}
    return [word for at, word in enumerate(words) if at not in dropped]


class Run:
    """One run of one test: its log lines and its failed subtests in the order they came, the
    `--- FAIL:` line that heads its report, and how its last line was read."""

    __slots__ = ("name", "depth", "parent", "items", "header", "ended", "open", "logging")

    def __init__(self, name: str, depth: int, parent: Run | None) -> None:
        self.name = name
        self.depth = depth
        self.parent = parent
        self.items: list[tuple[str, bool] | Run] = []
        self.header = ""
        self.ended = False
        self.open = False  # the last line came in pieces, and this is not the first
        self.logging = False  # the last line was the test's log

    def report(self, out: list[str]) -> None:
        """The report go test prints for this run when it failed: the header, then the test's own
        lines and each failed subtest's report, indented one level deeper than the run, at the
        start of each line alone."""
        pad = INDENT * self.depth
        out.append(pad + (self.header or f"--- FAIL: {self.name}\n"))
        for item in self.items:
            if isinstance(item, Run):
                item.report(out)
            else:
                text, starts = item
                out.append(pad + text if starts else text)


class Package:
    """What one package's binary printed, held until the package ends where go test holds it."""

    __slots__ = ("lines", "runs", "summary")

    def __init__(self) -> None:
        self.lines: list[str] = []
        self.runs: dict[str, Run] = {}
        self.summary = ""


class InOrder:
    """go test's order of packages: -json streams the packages' events as they come, while go test
    prints each package's lines in the order the packages started, which is the order of their
    `start` events (cmd/go starts a package's test only after the one before it wrote its start).
    The first package not yet ended writes as it goes, and each later one is held until every
    package before it has ended."""

    def __init__(self, write_out) -> None:
        self.out = write_out
        self.queue: list[str] = []
        self.held: dict[str, list[str]] = {}
        self.ended: set[str] = set()

    def start(self, name: str) -> None:
        if name not in self.held:
            self.held[name] = []
            self.queue.append(name)

    def write(self, name: str, text: str) -> None:
        self.start(name)
        if self.queue and self.queue[0] == name:
            self.out(text)
        elif name in self.queue:
            self.held[name].append(text)
        else:
            self.out(text)

    def end(self, name: str) -> None:
        self.start(name)
        self.ended.add(name)
        while self.queue and self.queue[0] in self.ended:
            self.queue.pop(0)
            if self.queue:
                self.out("".join(self.held.pop(self.queue[0])))
                self.held[self.queue[0]] = []

    def finish(self) -> None:
        for name in self.queue:
            self.out("".join(self.held.get(name, [])))
        self.queue.clear()


class Quiet:
    """The log go test prints without -v, rebuilt from the events (the module header says what it
    holds)."""

    def __init__(self, mode: Mode, write_out, write_err) -> None:
        self.mode = mode
        self.order = InOrder(write_out)
        self.out = write_out
        self.err = write_err
        self.packages: dict[str, Package] = {}
        self.failed = False

    def event(self, event: dict) -> None:
        action = event.get("Action")
        if action == "build-output":
            self.err(event.get("Output") or "")
            return
        name = event.get("Package")
        if not name:
            return
        self.order.start(name)
        package = self.packages.setdefault(name, Package())
        test = event.get("Test") or ""
        if action == "output":
            self.output(name, package, test, event)
        elif action == "run" and test:
            parent = self.parent_of(package, test)
            package.runs[test] = Run(test, parent.depth + 1 if parent else 0, parent)
        elif action in ENDED and test:
            run = package.runs.pop(test, None)
            if run is None:
                return
            # An ended run leaves the table: a line filed under its name from now on is another's
            # and is the package's own, and a run that passed or skipped prints nothing, a
            # subtest of a failed test among them, so nothing of it is kept.
            run.ended = True
            if action != "fail":
                return
            if run.parent is not None and not run.parent.ended:
                run.parent.items.append(run)
            else:
                out: list[str] = []
                run.report(out)
                self.binary(name, package, "".join(out))
        elif action in ENDED:
            self.end(name, package, action)

    @staticmethod
    def parent_of(package: Package, test: str) -> Run | None:
        """The running test a subtest is under: the longest running name its own is a path below,
        since a subtest's own name may hold a `/`."""
        name = test
        while "/" in name:
            name = name.rsplit("/", 1)[0]
            parent = package.runs.get(name)
            if parent is not None:
                return parent
        return None

    def binary(self, name: str, package: Package, text: str) -> None:
        """A piece of the binary's own output, in its place: printed now where go test streams it,
        else held for the package's end."""
        if self.mode.streams:
            self.order.write(name, text)
        else:
            package.lines.append(text)

    def output(self, name: str, package: Package, test: str, event: dict) -> None:
        text = event.get("Output") or ""
        run = package.runs.get(test) if test else None
        if run is None:
            # A line outside any test, or filed under a run that has ended (another goroutine's,
            # a data race's, a panic's after its test failed).
            self.binary(name, package, text)
            if not test:
                package.summary = text
            return
        if event.get("OutputType") == "frame":
            # `=== RUN`, `=== PAUSE`, `=== CONT` and `=== NAME` are -v's alone; the `--- FAIL:`
            # line heads the report, and what the test writes after it is the report's.
            if text.lstrip().startswith("--- "):
                run.header = text.lstrip()
            run.open = False
            return
        if text == f"{run.name}\n" and run.name.startswith("Benchmark") and not run.open:
            # The line naming a benchmark before its result, which testing prints in verbose mode
            # alone (-json's among them).
            return
        error = event.get("OutputType") in ("error", "error-continue")
        if not run.logging and not error and not LOG_START.match(text):
            # A print that ended no line, then a t.Log line, which test2json reads as one line:
            # the print is the binary's, and the log line starts where its layout does.
            inside = LOG_INSIDE.search(text)
            if inside:
                self.piece(name, package, run, text[: inside.start()], False, False)
                run.open = False
                text = text[inside.start():]
        if run.open and not (not run.logging and LOG_START.match(text)):
            logging = run.logging
        else:
            logging = (error or bool(run.header) or bool(LOG_START.match(text))
                       or (run.logging and text.startswith(LOG_CONTINUE)))
        self.piece(name, package, run, text, logging, not run.open)

    def piece(self, name: str, package: Package, run: Run, text: str, logging: bool, starts: bool) -> None:
        """One piece of a test's output: its log, kept for its report, or its print, the binary's."""
        run.logging, run.open = logging, not text.endswith("\n")
        if logging:
            run.items.append((text, starts))
        else:
            self.binary(name, package, text)

    def end(self, name: str, package: Package, action: str) -> None:
        # A run the binary never ended prints nothing of its log: go test prints none of a test's
        # buffered t.Log lines that never reached its report, and what the test printed, a
        # timeout's dump among it, is already the binary's output.
        del self.packages[name]
        if action == "fail":
            self.failed = True
        if not self.mode.streams:
            if action == "fail" or self.mode.shows_pass:
                text, summary = "".join(package.lines), package.summary
                # go test ends a line the binary left open before the package's summary.
                body = text[: len(text) - len(summary)] if summary and text.endswith(summary) else text
                if body and not body.endswith("\n") and SUMMARY.match(summary):
                    text = body + "\n" + summary
                self.order.write(name, text)
            else:
                self.order.write(name, package.summary)
        self.order.end(name)

    def finish(self) -> None:
        # A package whose stream stopped before its end (go was killed) prints what it holds, as
        # a failed one does; go test's own last `FAIL` line follows a package that failed.
        failed = self.failed
        for name in list(self.packages):
            self.end(name, self.packages[name], "fail")
        self.order.finish()
        if failed and self.mode.closes:
            self.out("FAIL\n")


class Verbose:
    """The log go test -v prints: every line, in go test's order of packages, with the
    `=== NAME` line -v prints before a test's line when another test's came last, which -json
    leaves out (testing's chattyPrinter: a `=== RUN`, `=== PAUSE`, `=== CONT` or `--- ` line
    names its test, and a line a test logs after another test's line is headed by its name)."""

    def __init__(self, mode: Mode, write_out, write_err) -> None:
        self.mode = mode
        self.order = InOrder(write_out)
        self.out = write_out
        self.err = write_err
        self.last: dict[str, str] = {}
        self.failed = False

    def event(self, event: dict) -> None:
        action = event.get("Action")
        text = event.get("Output") or ""
        if action == "build-output":
            self.err(text)
            return
        name = event.get("Package")
        if not name:
            self.out(text)
            return
        self.order.start(name)
        test = event.get("Test") or ""
        if text:
            if test and event.get("OutputType") == "frame":
                self.last[name] = test
            elif test:
                last = self.last.get(name, "")
                if last and last != test:
                    text = f"=== NAME  {test}\n{text}"
                self.last[name] = test
            self.order.write(name, text)
        if action in ENDED and not test:
            self.failed |= action == "fail"
            self.order.end(name)

    def finish(self) -> None:
        self.order.finish()
        if self.failed and self.mode.closes:
            self.out("FAIL\n")


def writer(stream):
    def write(text: str) -> None:
        if text:
            stream.write(text)
            stream.flush()
    return write


def forward_signals(child: subprocess.Popen) -> None:
    """A cancelled step's SIGINT or SIGTERM goes on to the command, and this script keeps reading
    until the command exits, so the events of the tests that ran are still written."""
    def forward(signum, _frame) -> None:
        if child.poll() is None:
            child.send_signal(signum)
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, forward)


def status_of(code: int) -> int:
    """A signalled command's status as a shell reports it."""
    return code if code >= 0 else 128 - code


def open_events(path: Path):
    """The events file, opened to append, and where this run's events start in it. A path that is
    there and is not a regular file (/dev/null, a directory, a pipe) keeps no event to read back,
    and is refused."""
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists() and not path.is_file():
            raise Refusal(f"the events file {path} is not a regular file, so no event written there "
                          f"could be read back")
        events = open(path, "ab", buffering=0)
    except OSError as error:
        raise Refusal(f"cannot open the events file {path}: {error}") from None
    return events, path.stat().st_size


def kept_since(path: Path, start: int) -> int:
    """How many events the file holds from `start` on, read back from the file itself."""
    try:
        with open(path, "rb") as events:
            events.seek(start)
            return sum(1 for line in events if parse_event(line) is not None)
    except OSError:
        return 0


def no_events(path: Path, status: int) -> int:
    print(f"go_test_events: the run wrote no test2json event to {path} (exit {status}), so nothing "
          f"records which tests it ran; the step fails rather than read as a step that ran none",
          file=sys.stderr)
    return status or 1


def run_go_test(go: list[str], args: list[str], path: Path) -> int:
    """The first form: go test -json, every event kept and the log printed. `go` is the command
    up to `test`: `go`, or `go -C <directory>`."""
    mode = Mode(args, os.environ)
    if mode.json:
        raise Refusal("the step passes -json, which this script adds itself; take it off the step")
    if mode.binary_verbose:
        raise Refusal(f"the step hands the test binary its own `{mode.binary_verbose}`, which decides "
                      f"which tests -json reports, and what the log shows, apart from go test's -v; "
                      f"take it off the step, and give go test its -v instead")
    render = (Verbose if mode.verbose else Quiet)(mode, writer(sys.stdout), writer(sys.stderr))
    events, start = open_events(path)
    environ = os.environ if mode.goflags is None else {**os.environ, "GOFLAGS": mode.goflags}
    try:
        child = subprocess.Popen([*go, "test", "-json", *(["-v"] if mode.verbose else []), *mode.args],
                                 stdout=subprocess.PIPE, env=environ)
    except OSError as error:
        events.close()
        print(f"go_test_events: cannot run go: {error}", file=sys.stderr)
        return no_events(path, 127)
    forward_signals(child)
    with events:
        for line in child.stdout:
            event = parse_event(line)
            if event is None:
                sys.stdout.write(line.decode(errors="replace"))
                sys.stdout.flush()
                continue
            events.write(line if line.endswith(b"\n") else line + b"\n")
            render.event(event)
    status = status_of(child.wait())
    render.finish()
    return status if kept_since(path, start) else no_events(path, status)


def run_command(argv: list[str], path: Path) -> int:
    """The second form: COMMAND with TEST_EVENTS_FILE set, and the events it appended counted."""
    events, start = open_events(path)
    events.close()
    try:
        child = subprocess.Popen(argv, env={**os.environ, ENV_FILE: str(path)})
    except OSError as error:
        print(f"go_test_events: cannot run {argv[0]}: {error}", file=sys.stderr)
        return no_events(path, 127)
    forward_signals(child)
    status = status_of(child.wait())
    return status if kept_since(path, start) else no_events(path, status)


def main(argv: list[str]) -> int:
    # A test's output is the test's: a character this stream cannot encode is replaced rather
    # than allowed to stop the step halfway through its log.
    for stream in (sys.stdout, sys.stderr):
        stream.reconfigure(errors="replace")
    if not argv:
        print("usage: go_test_events.py go test [flags] [packages], or go_test_events.py COMMAND [ARGS...]",
              file=sys.stderr)
        return 2
    try:
        path = events_path(os.environ)
        if argv[0] == "go":
            # go's one flag before its command a step may give: -C, the directory it runs in.
            at = 3 if argv[1:2] == ["-C"] else 2 if argv[1:2] and argv[1].startswith("-C=") else 1
            if argv[at:at + 1] != ["test"]:
                raise Refusal(f"`{' '.join(argv[:at + 1])}` is not go test; this script runs go test, or a script that does")
            return run_go_test(argv[:at], argv[at + 1:], path)
        return run_command(argv, path)
    except Refusal as refusal:
        print(f"go_test_events: {refusal}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
