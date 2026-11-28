"""The record a bun test run of ci.yaml's run leaves, and the one rule that says when it is whole.

Every bun test command of ci.yaml's run runs under WRAPPER (.github/scripts/bun_test_run.py). It
writes, under `$RUNNER_TEMP/REPORTS_DIR/` at the path of the working directory followed by the
run's name and REPORT (`packages/pi-shared/test.xml`; at the repository's root, `<name>.xml`):

  <report>.run   the command, before bun starts;
  <report>.t     the run's -t pattern, before bun starts, when it has one;
  <report>       bun's JUnit report (`--reporter=junit --reporter-outfile`), which bun writes when
                 its run ends;
  <report>.exit  once bun has ended, or could not start, a JSON object: "status", bun's exit status
                 (128 plus the signal's number when a signal ended it, 127 when it could not
                 start); "errors", zero at status 0 because bun 1.4.4 exits non-zero when its
                 summary counts an error, otherwise the count from bun's summary, or null if
                 bun printed no summary or the wrapper's buffer cut the start of its block;
                 and, when bun could not start, "start", why.

runs() finds a directory's runs, and report_of() names one. The job's step named UPLOAD_STEP
(UPLOAD_ACTION) uploads that directory as the artifact `PREFIX<the job's check run id>`.
.github/scripts/check-workflow-files.sh's rule 8 (bun_test_steps.py) holds every bun test step of
ci.yaml's run to the wrapper, with a report name NAME matches, every such job to that step, and
the action's own upload to PREFIX and REPORTS_DIR. These names have this one spelling: the wrapper,
the rule, the reader (bun_test_reports.py) and ci_job_reads.py import them from here, and rule 8
holds the action's copy of the two to them.

The rule. A run, found by its `.run` marker or by its report, is whole, and its report's tests are
every test bun ran in it, exactly when:

  1. its `.exit` is there, as the wrapper writes it;
  2. its report is there and is bun 1.4.4's JUnit: a <testsuites> root whose `tests` is the number
     of its <testcase> entries (the count has at most TEST_COUNT_LIMIT characters), with at most
     NESTING_LIMIT nested <testsuite> levels. Each entry is a test, with a finite `time`, a `line`
     and a `file`, or one of bun's two entries with no line, skipped or failed: HOOK for a
     beforeAll or afterAll hook, CRASH for a --parallel worker; and
     the run's `.t` pattern, when it has one, is a literal name (literal_fault), which the reader
     matches as a substring, as bun's regular expression of it matches;
  3. bun started, printed its summary, and the summary counts no error;
  4. no entry with no line is a failure: a beforeAll or afterAll hook that threw (the tests of its
     describe or file may not have run, and those that did not are not in the report) or a worker
     that crashed (the tests of that file it had not reported are not in the report). A skipped
     entry with no line is a hook of a skipped describe, and no test;
  5. a non-zero exit status comes with a failure the record shows: a failing test, an entry of
     clause 4, or an error of clause 3.

read_run gives a run's tests as rows and its reasons, each starting `clause N:`: one for each
clause the run breaks, but clause 4, which gives one for each failed entry. Clauses 4 and 5 are
read only from a report clause 2 accepts, and clauses 3 and 5 only from a `.exit` clause 1
accepts. The reader prints each reason as an `unknown` line beside the rows, and exits 1; the
wrapper fails a step whose bun exited 0 when its run breaks a clause.

Everything under REPORTS_DIR is written by the run's own tests too, if they choose: an artifact is
untrusted input. So a `.t` is matched as a literal substring, never compiled, and the reader
bounds an artifact's size and member count before it unpacks it.
"""
import json
import math
import os
import re
import xml.etree.ElementTree as ElementTree
from pathlib import Path

WRAPPER = ".github/scripts/bun_test_run.py"
UPLOAD_ACTION = "./.github/actions/bun-test-reports-upload"
UPLOAD_ACTION_DIR = UPLOAD_ACTION.removeprefix("./")
UPLOAD_STEP = "Keep the bun test reports"
REPORTS_DIR = "bun-test-reports"
PREFIX = f"{REPORTS_DIR}-"
# The run's name, which names its report: the wrapper refuses any other, and rule 8 reads only
# this one as a name.
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")
REPORT = ".xml"
MARKER = ".run"
EXIT = ".exit"
PATTERN = ".t"
# A -t pattern the record keeps is a literal name: none of a JavaScript regular expression's
# special characters, so bun's match of it and the reader's substring match are one match, and
# at most PATTERN_LIMIT characters.
REGEX_CHARACTERS = set("\\^$.|?*+()[]{}")
PATTERN_LIMIT = 1000
# bun nests one <testsuite> per file and one per describe. Bound the number of levels a test's
# printed name can include.
NESTING_LIMIT = 100
# More digits than any report can have entries, but few enough to convert and print safely.
TEST_COUNT_LIMIT = 64
# The attributes bun 1.4.4 writes on every <testcase> of a test.
CASE_ATTRIBUTES = ("time", "line", "file")
# The names bun 1.4.4 gives its <testcase> entries with no line: a beforeAll or afterAll hook
# (skipped under a skipped describe, failed when it threw), and a --parallel worker that crashed.
HOOK = "(unnamed)"
CRASH = "(worker crashed)"


def report_of(directory: Path, name: str) -> Path:
    return directory / f"{name}{REPORT}"


def beside(report: Path, suffix: str) -> Path:
    return report.with_name(report.name + suffix)


def runs(directory: Path) -> list:
    """Every run under `directory`, as its report's path, found by its report or its marker."""
    return sorted({p for p in directory.rglob(f"*{REPORT}") if p.is_file()}
                  | {p.with_name(p.name[:-len(MARKER)]) for p in directory.rglob(f"*{REPORT}{MARKER}") if p.is_file()})


def literal_fault(pattern: str):
    """Why a -t pattern is not a literal name the record can keep, or None."""
    if len(pattern) > PATTERN_LIMIT:
        return f"it is {len(pattern)} characters, above the bound of {PATTERN_LIMIT}"
    special = sorted(REGEX_CHARACTERS & set(pattern))
    if special:
        return (f"it holds {' '.join(special)}, a regular expression's special character, and the record "
                "matches a pattern only as a literal name")
    return None


def write_exit(path: Path, status: int, errors, start=None) -> None:
    record = {"status": status, "errors": errors}
    if start is not None:
        record["start"] = start
    path.write_text(json.dumps(record) + "\n")


def read_exit(path: Path):
    """The wrapper's `.exit`, None when there is none, or the reason it is not the wrapper's."""
    if not path.is_file():
        return None, None
    try:
        text = path.read_text()
    except (OSError, UnicodeDecodeError) as error:
        return None, f"its exit status file {path.name} cannot be read: {error}"
    try:
        record = json.loads(text)
    except (ValueError, RecursionError):
        record = None
    if not (isinstance(record, dict) and type(record.get("status")) is int
            and (record.get("errors") is None or type(record.get("errors")) is int)
            and (record.get("start") is None or isinstance(record.get("start"), str))):
        return None, f"its exit status file {path.name} holds {text.strip()[:60]!r}, not the wrapper's"
    return record, None


def read_pattern(path: Path):
    """(the run's -t pattern or None, the reason clause 2 refuses it or None)."""
    if not path.is_file():
        return None, None
    try:
        pattern = path.read_text()
    except (OSError, UnicodeDecodeError) as error:
        return None, f"its -t pattern {path.name} cannot be read: {error}"
    fault = literal_fault(pattern)
    return (None, f"its -t pattern {path.name} is not a literal name: {fault}") if fault else (pattern, None)


def status_of(case, full_name: str, pattern) -> str:
    if case.find("failure") is not None or case.find("error") is not None:
        return "fail"
    skipped = case.find("skipped")
    if skipped is not None:
        if skipped.get("message") == "TODO":
            return "todo"
        # bun writes a test its -t pattern does not select as <skipped />, as it writes a test.skip,
        # so the run keeps its pattern beside the report and this tells the two apart: bun matches
        # the pattern against the describe blocks and the test's name joined by spaces.
        if pattern is not None and pattern not in full_name:
            return "unselected"
        return "skip"
    return "pass"


class ReportNestingError(ValueError):
    pass


def cases(suite):
    """(describe blocks and name, case) for each <testcase> under a file's suite, in report order."""
    stack = [iter(suite)]
    describes = []
    while stack:
        child = next(stack[-1], None)
        if child is None:
            stack.pop()
            if describes:
                describes.pop()
        elif child.tag == "testsuite":
            if len(stack) == NESTING_LIMIT:
                raise ReportNestingError(
                    f"the report's <testsuite> nesting exceeds the bound of {NESTING_LIMIT} levels")
            stack.append(iter(child))
            describes.append(child.get("name", ""))
        elif child.tag == "testcase":
            yield [*describes, child.get("name", "")], child


def read_report(path: Path, report: str):
    """(rows, clause 4's reasons, clause 2's reason) for one report; `report` is its path in the
    artifact, whose directory is put before each test's file."""
    try:
        # An unsupported XML encoding can raise LookupError or ValueError, including UnicodeError.
        root = ElementTree.parse(path).getroot()
    except (ElementTree.ParseError, OSError, ValueError, LookupError) as error:
        return None, [], f"the report does not parse: {error}"
    if root.tag != "testsuites":
        return None, [], f"the report's root is <{root.tag}>, not <testsuites>"
    pattern, why = read_pattern(beside(path, PATTERN))
    if why:
        return None, [], why
    directory = os.path.dirname(report)
    rooted = lambda file: f"{directory}/{file}" if directory else file
    rows, failed, entries = [], [], 0
    try:
        for top in root.findall("testsuite"):
            for names, case in cases(top):
                entries += 1
                skipped, failure = case.find("skipped") is not None, case.find("failure") is not None
                if case.get("line") is None and names[-1] in (HOOK, CRASH) and skipped != failure:
                    if failure and names[-1] == CRASH:
                        failed.append(f"the --parallel worker running {rooted(case.get('file'))} crashed: the tests of "
                                      "that file it had not reported are not in the report")
                    elif failure:
                        where = f"of {' > '.join(names[:-1])!r}" if len(names) > 1 else "at the top"
                        scope = "describe" if len(names) > 1 else "file"
                        failed.append(f"a beforeAll or afterAll hook {where} in {rooted(case.get('file'))} threw: the "
                                      f"tests of that {scope} may not have run, and those that did not are not in the "
                                      "report")
                    continue
                missing = [a for a in CASE_ATTRIBUTES if case.get(a) is None]
                if missing:
                    return None, [], (f"the report does not parse: its entry {' > '.join(names)!r} has no "
                                      f"{' or '.join(missing)} attribute, and is neither a test nor an entry bun "
                                      "writes with no line")
                try:
                    seconds, line = float(case.get("time")), int(case.get("line"))
                except ValueError:
                    return None, [], (f"the report does not parse: its test {' > '.join(names)!r} has time "
                                      f"{case.get('time')!r} and line {case.get('line')!r}, not numbers")
                if not math.isfinite(seconds):
                    return None, [], f"its test {' > '.join(names)!r} has time {case.get('time')!r}, which is not finite"
                rows.append({"file": rooted(case.get("file")), "name": " > ".join(names),
                             "status": status_of(case, " ".join(names), pattern), "seconds": seconds, "line": line})
    except ReportNestingError as error:
        return None, [], str(error)
    declared = root.get("tests")
    if declared is not None and len(declared) > TEST_COUNT_LIMIT:
        return None, [], f"the report's test count is longer than {TEST_COUNT_LIMIT} characters"
    if declared is None or not declared.isdecimal() or int(declared) != entries:
        return None, [], f"the report says {declared} tests and holds {entries} entries"
    return rows, failed, None


def read_run(path: Path, report: str):
    """One run's rows (empty when its report cannot be read) and its reasons: one per clause of the
    rule it breaks, but clause 4, which gives one per failed entry."""
    ended, why = read_exit(beside(path, EXIT))
    reasons = []
    if why:
        reasons.append(f"clause 1: {why}")
    elif ended is None:
        reasons.append("clause 1: it has no exit status: bun had not ended, or the wrapper was killed, when the "
                       "job uploaded its reports (the runner signals a cancelled or timed-out step's shell alone, "
                       "so the wrapper and bun run on)")
    started = ended is not None and "start" not in ended
    if ended is not None and not started:
        reasons.append(f"clause 3: bun could not start ({ended['start']}), so it ran no test")
    elif started and ended["errors"] is None:
        reasons.append("clause 3: the wrapper read no whole summary block (bun printed none, or the block's "
                       "start was cut from its buffer), so its count of errors outside any test is unknown")
    elif started and ended["errors"]:
        reasons.append(f"clause 3: bun counted {ended['errors']} error(s) outside any test (a file that did not "
                       "load, an error between tests), which its report leaves out")
    if not path.is_file():
        reasons.append("clause 2: it has no report" + (
            f": bun exited {ended['status']} and wrote none (an outfile it could not write, a test that called "
            "process.exit, a signal or a crash)" if started else ""))
        return [], reasons
    rows, failed, why = read_report(path, report)
    if why:
        reasons.append(f"clause 2: {why}")
        return [], reasons
    reasons.extend(f"clause 4: {failure}" for failure in failed)
    if (started and ended["status"] and not failed and not ended["errors"]
            and not any(r["status"] == "fail" for r in rows)):
        reasons.append(f"clause 5: bun exited {ended['status']}, and the record shows no failure that explains it")
    return rows, reasons
