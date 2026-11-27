#!/usr/bin/env python3
"""Print the wall-clock start and end of every test case each pooled shell harness of a CI run
ran, one JSON object per line.

harness-pool.sh runs a job's shell harnesses in the background, each into a log of its own, and
only the job's wait step prints those logs into the job's log, once every harness has ended.
GitHub stamps each line of a job's log with the time the line reached it, so every line a pooled
harness wrote carries the time the wait printed it, and the time each of its cases started and
ended was lost: in the job's log a case of two minutes looks like a case of two milliseconds. So
the pool now stamps each line of a harness's log with the time the line was written, and each job
that waits for the pool uploads the pool's records as the artifact `harness-times-<job id>`,
where the job id is the number in the job's URL (`.../job/<id>`). This script reads them back.

It reads, for each job of the run, the job's steps from the run's jobs payload, and, from the
job's artifact, the pool's numbered entries (`NNN` = 000, 001, ...), one per harness:

    NNN.name       the harness's name
    NNN.started    when it took a slot, epoch seconds (absent: it never took one)
    NNN.seconds    `<wall> <cpu> <waited>`
    NNN.exit       its exit status (absent: its recorder died without writing one)
    NNN.lines      one record per line of its log, `<epoch seconds>\\t<the line's bytes>\\n`
    NNN.lines.end  the bytes of the log the stamper read, written once it read to the end
    NNN.logbytes   the log's size, written by the recorder once the stamper finished

and one file of the whole pool, which every wait writes before it waits for anything, `--now`
and `--allow-none` included, so a job whose wait ran has an artifact even when it ran nothing:

    judged         the number of entries the wait found, 0 when no harness was queued

A job is read when it has a step named `Wait for the script harnesses and judge them` or `Name
the script harnesses a cancelled job left` that was not skipped, or when the run has its
artifact: either says the job ran a pool. Any other job prints nothing, and so does a job whose
artifact's `judged` is 0 and holds no entry: every harness step of a job can have an `if:` of its
own, and a change that reaches none of them rightly leaves its pool with nothing to run, which is
no case and nothing unknown.

A case is a line of the log under the grammar of the weekly test inventory's log parser, which
this replaces, tested in this order against the line with its surrounding white space removed
(`.github/scripts/test-lib.sh#check` prints `  PASS: ...` and `  FAIL: ...` indented):

    case: <name>                    a block's head; its checks print a line only when they fail
    PASS|FAIL|ok|not ok [N][:| -] <name>
                                    a case; a failing one under an open head takes the head's name
    <name>: PASS|FAIL               a case named before its verdict

That grammar is the inventory parser's, rule for rule, so that these rows read as the rows they
replace: a verdict that names nothing (a bare `not ok`) and a named verdict whose name is shorter
than four characters are a case to neither of them.

A case ends at the time of its own line and starts at the time of the harness's case line before
it, or at the harness's start for its first.

A harness's exit status is part of its record and is never inferred from its case lines: a
harness that printed only passing cases can still have failed, as one its `--minutes` bound
stopped or `set -e` ended in the middle of a case has, and the time after its last case line is
where a hang lives. So a harness that exited other than 0 always has a row of its own, of kind
`harness` and no case, that fails: it starts at the time of its last case line and ends at the
harness's end, its `.started` plus its wall seconds, a span that belongs to no passed case. A
harness that exited 0 has that row too, passing, when its end is past its last case line by more
than one of the stamper's polls, 0.05 s, so that the time after its last case is in the rows as
well. A harness whose log has no case line has that row alone, from its start for its wall
seconds. The recorder rounds the wall seconds to a tenth, so a line read in the stamper's last
poll can carry a time past that end; a failing row then ends at that line and holds no seconds,
rather than ending before it starts. Each such row carries the harness's exit status as `exit`.

UNKNOWN. A row whose status and kind are `unknown`, with a `why` saying what is missing, stands
for what the record cannot tell: a job that ran a pool but has no artifact in the run, an expired
one, or one whose download failed; an artifact with no `judged` that is a whole number, which
cannot say whether the entries it holds, or its holding none, are all its pool had; an artifact
whose `judged` is not the number of entries it holds, a part of the record lost on its way, so
the entries it does hold print no case either; a harness that never took a slot, or whose
recorder died without its exit status; a harness whose records the stamper did not finish (no
`NNN.lines.end`, the stamper was killed or the job ended while the harness still ran), whose
records do not add up to the log they came from, or whose records' times do not run forwards: a
time that is no finite number, one earlier than the harness's start, or one earlier than the
record before it, which no stamper reading a clock wrote. Such a harness prints no case: what its
records hold may be any part of what it ran. A `.started` or a wall second that is no finite
number is no time either, and makes its harness UNKNOWN the same way, since when it started or
ended is not known; the UNKNOWN row of a wall second that is no number still carries the
harness's `exit`. A row is printed for each of these rather than left out because a reader of the
rows sees only what is there: a job or a harness left out reads as one that ran no case, and a
total of case times over the run would count a lost record as no time at all. The exit status
says it too: 1 when any row is UNKNOWN; otherwise 3 when any row fails, a case's or a harness's,
so that a harness's exit status reaches the reader's; otherwise 0; and 2 for a usage error or a
`gh` call that failed outright.

GitHub keeps the artifacts for 90 days, so a run older than that reads as one UNKNOWN row per job
that ran a pool: the record is gone, and the reader says so.

It refuses to guess: a record it cannot read is never a pass, and it talks to GitHub only through
the `gh` CLI, with gh's own credentials, and holds no token and makes no HTTP request itself.

    harness_case_times.py --run <run id> [--repo <owner/name>] [--job <job id>]...
    harness_case_times.py --from-dir <dir> --jobs-json <file> [--job <job id>]...

`--run` asks gh for the run's jobs and artifacts, a page of 100 at a time, and downloads each
job's artifact into a temporary directory of its own, which it removes at the end. `--from-dir`
reads no network: <dir> holds one directory `harness-times-<job id>` per job, each the artifact's
files, and <file> the jobs payload of the run (`GET /repos/{owner}/{repo}/actions/runs/{run_id}/
jobs`) or a JSON list of its pages.

Its tests are in harness_case_times.test.sh.
"""

import argparse
import json
import math
import os
import re
import shutil
import subprocess
import sys
import tempfile

DEFAULT_REPO = "cybermonklabs/legion"
# The largest page GitHub gives. Both listings share the REST budget of every agent and job, so a
# listing stops at the first page short of a full one, or once it holds the total it was told of.
PAGE = 100
POOL_STEPS = frozenset(
    {
        "Wait for the script harnesses and judge them",
        "Name the script harnesses a cancelled job left",
    }
)
# The stamper's poll, harness_line_times.py's default: a harness that exited 0 and ended within one
# of them of its last case line has no time after that line that a row could hold.
POLL_SECONDS = 0.05

CASE_HEAD = re.compile(r"^case: (.+)$")
CASE_LINE = re.compile(r"^(PASS|FAIL|ok|not ok)\b(?: \d+)?(?::| -)? ?(.*)$")
CASE_TAIL = re.compile(r"^(\S.{3,}?): (PASS|FAIL)$")
ARTIFACT_DIR = re.compile(r"harness-times-\d+")
ENTRY_FILE = re.compile(r"(\d+)\..+")
WHOLE = re.compile(rb"\s*(\d+)\s*")
NUMBER = re.compile(rb"\s*(\d+(?:\.\d+)?)\s*")
STAMP = re.compile(rb"\d+(?:\.\d+)?")


class Failure(Exception):
    """A usage error, or a gh call that failed outright: its reason goes to stderr, and exit 2."""


def artifact_name(job_id: int) -> str:
    return f"harness-times-{job_id}"


def last_line(text: str) -> str:
    lines = [line for line in text.splitlines() if line.strip()]
    return lines[-1].strip() if lines else "it printed nothing"


def run_gh(args: list[str]) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(["gh", *args], capture_output=True, text=True, check=False)
    except OSError as error:
        raise Failure(f"could not run gh: {error}") from error


def listing(repo: str, run: int, path: str, key: str) -> list[object]:
    """Every item of one of the run's listings, `jobs` or `artifacts`, a page at a time."""
    items: list[object] = []
    page = 1
    while True:
        query = "filter=all&" if key == "jobs" else ""
        endpoint = f"repos/{repo}/actions/runs/{run}/{path}?{query}per_page={PAGE}&page={page}"
        done = run_gh(["api", endpoint])
        if done.returncode != 0:
            raise Failure(f"gh api {endpoint} exited {done.returncode}: {last_line(done.stderr)}")
        try:
            payload = json.loads(done.stdout)
        except json.JSONDecodeError as error:
            raise Failure(f"gh api {endpoint} printed no JSON: {error}") from error
        batch = payload.get(key) if isinstance(payload, dict) else None
        if not isinstance(batch, list):
            raise Failure(f"gh api {endpoint} gave no {key} list")
        items.extend(batch)
        total = payload.get("total_count")
        if len(batch) < PAGE or (isinstance(total, int) and len(items) >= total):
            return items
        page += 1


def jobs_from_file(path: str) -> list[object]:
    try:
        with open(path, encoding="utf-8") as file:
            payload = json.load(file)
    except (OSError, ValueError) as error:
        raise Failure(f"--jobs-json {path} could not be read as JSON: {error}") from error
    jobs: list[object] = []
    for page in payload if isinstance(payload, list) else [payload]:
        batch = page.get("jobs") if isinstance(page, dict) else None
        if not isinstance(batch, list):
            raise Failure(
                f"--jobs-json {path} is neither a run's jobs payload nor a list of them: a page has no jobs list"
            )
        jobs.extend(batch)
    return jobs


def checked_jobs(jobs: list[object]) -> list[dict]:
    """The run's jobs in the payload's order, each once, every one with a whole-number id."""
    seen: set[int] = set()
    out: list[dict] = []
    for job in jobs:
        if not isinstance(job, dict) or type(job.get("id")) is not int:
            raise Failure(f"the jobs payload holds a job with no whole-number id: {json.dumps(job)[:200]}")
        if job["id"] not in seen:
            seen.add(job["id"])
            out.append(job)
    if not out:
        raise Failure("the jobs payload lists no job, so there is nothing to read")
    return out


def ran_pool(job: dict) -> bool:
    """Whether the job ran a pool wait step. A null conclusion is a step of a job still running."""
    steps = job.get("steps") or []
    return any(
        isinstance(step, dict) and step.get("name") in POOL_STEPS and step.get("conclusion") != "skipped"
        for step in steps
    )


def row(job: dict, harness, case, status, start, end, kind, why=None, exit_status=None) -> dict:
    out = {
        "job": job["id"],
        "job_name": job.get("name"),
        "harness": harness,
        "case": case,
        "status": status,
        "start": start,
        "end": end,
        "seconds": None if start is None or end is None else round(end - start, 6),
        "kind": kind,
    }
    if exit_status is not None:
        out["exit"] = exit_status
    if why is not None:
        out["why"] = why
    return out


def unknown(job: dict, harness, why: str, exit_status=None) -> dict:
    return row(job, harness, None, "unknown", None, None, "unknown", why, exit_status)


def read_file(path: str) -> bytes | None:
    try:
        with open(path, "rb") as file:
            return file.read()
    except OSError:
        return None


def whole(data: bytes | None) -> int | None:
    match = WHOLE.fullmatch(data) if data is not None else None
    return int(match.group(1)) if match else None


def number(data: bytes | None) -> float | None:
    """A number of seconds, or None; a number too large for a float is none."""
    match = NUMBER.fullmatch(data) if data is not None else None
    value = float(match.group(1)) if match else None
    return value if value is not None and math.isfinite(value) else None


def records(data: bytes, entry: str, started: float) -> tuple[list[tuple[float, bytes]], int, str | None]:
    """The (time, line) records of an entry's `.lines`, the bytes of the log they stand for, and
    why they cannot be read, or None. Lines read in one poll share its time, so a time equal to
    the one before it, or to the harness's start, runs forwards; an earlier one does not."""
    if not data:
        return [], 0, None
    if not data.endswith(b"\n"):
        return [], 0, f"its {entry}.lines ends in the middle of a record, with no newline after its last one"
    out: list[tuple[float, bytes]] = []
    covered = 0
    previous = started
    for count, record in enumerate(data[:-1].split(b"\n"), start=1):
        stamp, tab, text = record.partition(b"\t")
        if not tab:
            return [], 0, f"record {count} of its {entry}.lines has no tab between its time and its line"
        if not STAMP.fullmatch(stamp):
            shown = stamp[:40].decode("utf-8", "replace")
            return [], 0, f"record {count} of its {entry}.lines begins '{shown}', which is not a time"
        value = float(stamp)
        shown = stamp.decode("ascii")
        if not math.isfinite(value):
            return (
                [],
                0,
                f"record {count} of its {entry}.lines has a time of {len(stamp)} digits, {shown[:20]}..., which is no finite number of seconds",
            )
        if value < previous:
            before = (
                f"the harness's start {started:.6f} in its {entry}.started"
                if count == 1
                else f"the time {out[-1][0]:.6f} of record {count - 1} before it"
            )
            return [], 0, f"record {count} of its {entry}.lines has the time {shown}, earlier than {before}: its times do not run forwards"
        out.append((value, text))
        previous = value
        # The line's bytes and the newline that ended it in the log.
        covered += len(text) + 1
    return out, covered, None


def classify(line: str, head: str | None) -> tuple[str, str, bool] | None:
    """(name, status, whether it is a head) for a case line, None for any other line."""
    text = line.strip()
    match = CASE_HEAD.match(text)
    if match:
        return match.group(1).strip(), "pass", True
    match = CASE_LINE.match(text)
    if match and match.group(2).strip():
        status = "pass" if match.group(1) in ("PASS", "ok") else "fail"
        name = head if status == "fail" and head is not None else match.group(2).strip()
        return name, status, False
    match = CASE_TAIL.match(text)
    if match:
        return match.group(1).strip(), "pass" if match.group(2) == "PASS" else "fail", False
    return None


def entry_rows(job: dict, base: str, entry: str) -> list[dict]:
    raw_name = read_file(f"{base}.name")
    if raw_name is None:
        return [unknown(job, None, f"entry {entry} of its artifact has no {entry}.name, so which harness it was is not known")]
    harness = raw_name.decode("utf-8", "replace").split("\n", 1)[0].rstrip("\r")

    raw = read_file(f"{base}.started")
    if raw is None:
        return [unknown(job, harness, f"it has no {entry}.started: it never took a slot, so it never ran")]
    started = number(raw)
    if started is None:
        return [unknown(job, harness, f"its {entry}.started is not a finite number of seconds")]
    started = round(started, 6)

    raw = read_file(f"{base}.exit")
    if raw is None:
        return [unknown(job, harness, f"it has no {entry}.exit: its recorder ended without writing the harness's status")]
    status = whole(raw)
    if status is None:
        return [unknown(job, harness, f"its {entry}.exit is not a whole number")]
    wall = number((read_file(f"{base}.seconds") or b"").split(b" ", 1)[0])
    if wall is None:
        return [
            unknown(
                job,
                harness,
                f"its {entry}.seconds gives no finite number of wall seconds, so when it ended is not known",
                status,
            )
        ]
    end = round(started + wall, 6)

    lines = read_file(f"{base}.lines")
    if lines is None:
        return [unknown(job, harness, f"its {entry}.lines is missing or cannot be read")]
    lines_end = whole(read_file(f"{base}.lines.end"))
    if lines_end is None:
        return [
            unknown(
                job,
                harness,
                f"its {entry}.lines.end is missing or not a whole number: the stamper never read its log to the end",
            )
        ]
    logbytes = whole(read_file(f"{base}.logbytes"))
    if logbytes is None:
        return [unknown(job, harness, f"its {entry}.logbytes is missing or not a whole number, so its log's size is not known")]
    if lines_end != logbytes:
        return [
            unknown(
                job,
                harness,
                f"its {entry}.lines.end says the stamper read {lines_end} bytes of its log, but its {entry}.logbytes says the log held {logbytes}",
            )
        ]
    stamped, covered, why = records(lines, entry, started)
    if why is not None:
        return [unknown(job, harness, why)]
    # A last line with no newline after it was one byte shorter in the log than its record says.
    if covered != lines_end and not (stamped and covered - 1 == lines_end):
        return [
            unknown(
                job,
                harness,
                f"the records of its {entry}.lines stand for {covered} bytes of its log, but its {entry}.lines.end says the stamper read {lines_end}",
            )
        ]

    rows: list[dict] = []
    previous = started
    head: str | None = None
    for stamp, text in stamped:
        case = classify(text.decode("utf-8", "replace"), head)
        if case is None:
            continue
        name, verdict, is_head = case
        if is_head:
            head = name
        rows.append(row(job, harness, name, verdict, previous, stamp, "case"))
        previous = stamp
    # The time after the last case line, or the whole harness when it printed none. A harness that
    # failed always has it, so that its failure is a row however its cases went.
    if not rows or status != 0 or round(end - previous, 6) > POLL_SECONDS:
        verdict = "pass" if status == 0 else "fail"
        rows.append(row(job, harness, None, verdict, previous, max(end, previous), "harness", exit_status=status))
    return rows


def entries_held(count: int) -> str:
    return "no entry" if count == 0 else f"{count} entr{'y' if count == 1 else 'ies'}"


def job_rows(job: dict, directory: str) -> list[dict]:
    name = artifact_name(job["id"])
    try:
        files = os.listdir(directory)
    except OSError as error:
        return [unknown(job, None, f"its artifact {name} could not be read: {error.strerror}")]
    entries = sorted({match.group(1) for file in files if (match := ENTRY_FILE.fullmatch(file))}, key=int)
    raw = read_file(os.path.join(directory, "judged"))
    judged = whole(raw)
    if judged is None:
        what = "no judged file" if raw is None else "a judged file that is not a whole number"
        return [
            unknown(
                job,
                None,
                f"its artifact {name} holds {entries_held(len(entries))} of the pool and {what}, so how many harnesses its pool held is not known",
            )
        ]
    if judged != len(entries):
        return [
            unknown(
                job,
                None,
                f"its artifact {name} holds {entries_held(len(entries))} of the pool, but its judged says the wait found {judged}",
            )
        ]
    rows: list[dict] = []
    for entry in entries:
        rows.extend(entry_rows(job, os.path.join(directory, entry), entry))
    return rows


class RunArtifacts:
    """The artifacts of a run on GitHub, each downloaded through gh into a directory of ours."""

    def __init__(self, repo: str, run: int, scratch: str):
        self.repo, self.run, self.scratch = repo, run, scratch
        # Name to whether it has expired; a name with a copy that has not expired is not expired.
        self.expired: dict[str, bool] = {}
        for item in listing(repo, run, "artifacts", "artifacts"):
            if isinstance(item, dict) and isinstance(item.get("name"), str):
                name = item["name"]
                self.expired[name] = bool(item.get("expired")) and self.expired.get(name, True)

    def has(self, name: str) -> bool:
        return name in self.expired

    def fetch(self, name: str) -> tuple[str | None, str | None]:
        if name not in self.expired:
            return None, f"the run has no artifact {name}"
        if self.expired[name]:
            return None, f"its artifact {name} has expired: GitHub keeps it for 90 days"
        target = os.path.join(self.scratch, name)
        done = run_gh(["run", "download", str(self.run), "-R", self.repo, "-n", name, "-D", target])
        if done.returncode != 0:
            return None, f"gh run download of its artifact {name} exited {done.returncode}: {last_line(done.stderr)}"
        return target, None


class DirArtifacts:
    """Artifacts already downloaded, one directory `harness-times-<job id>` each under a root."""

    def __init__(self, root: str):
        if not os.path.isdir(root):
            raise Failure(f"--from-dir {root} is not a directory")
        self.root = root
        self.names = {
            name for name in os.listdir(root) if ARTIFACT_DIR.fullmatch(name) and os.path.isdir(os.path.join(root, name))
        }

    def has(self, name: str) -> bool:
        return name in self.names

    def fetch(self, name: str) -> tuple[str | None, str | None]:
        if name not in self.names:
            return None, f"{self.root} holds no directory {name}, the job's artifact"
        return os.path.join(self.root, name), None


def positive_int(text: str) -> int:
    if not re.fullmatch(r"[1-9][0-9]*", text):
        raise argparse.ArgumentTypeError(f"'{text}' is not a positive whole number")
    return int(text)


def parse(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="harness_case_times.py",
        description="Print, one JSON object per line, the wall-clock start and end of every test case each "
        "pooled shell harness of a GitHub Actions run ran, from the run's harness-times-<job id> artifacts.",
        epilog="Exit status: 1 when any row is UNKNOWN, a record that is missing or incomplete; otherwise 3 "
        "when any row fails, a case's or a harness's, so that a harness's exit status reaches this one; "
        "otherwise 0; and 2 for a usage error or a gh call that failed.",
    )
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--run", type=positive_int, metavar="RUN_ID", help="the run to read, through the gh CLI")
    source.add_argument(
        "--from-dir",
        metavar="DIR",
        help="read no network: DIR holds one directory harness-times-<job id> per job, each that artifact's files",
    )
    parser.add_argument(
        "--jobs-json",
        metavar="FILE",
        help="with --from-dir, and required with it: the run's jobs payload "
        "(GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs), or a JSON list of its pages",
    )
    parser.add_argument(
        "--repo", default=DEFAULT_REPO, metavar="OWNER/NAME", help=f"with --run: the repository (default {DEFAULT_REPO})"
    )
    parser.add_argument(
        "--job",
        type=positive_int,
        action="append",
        metavar="JOB_ID",
        help="read only this job of the run; may be given more than once",
    )
    args = parser.parse_args(argv)
    if args.from_dir is not None and args.jobs_json is None:
        parser.error("--from-dir needs --jobs-json, the run's jobs and their steps")
    if args.run is not None and args.jobs_json is not None:
        parser.error("--jobs-json goes with --from-dir; --run reads the run's jobs through gh")
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", args.repo):
        parser.error(f"--repo '{args.repo}' is not <owner>/<name>")
    return args


def main(argv: list[str]) -> int:
    args = parse(argv)
    scratch = tempfile.mkdtemp(prefix="harness-case-times.") if args.run is not None else None
    try:
        if args.run is not None:
            jobs = checked_jobs(listing(args.repo, args.run, "jobs", "jobs"))
            artifacts: RunArtifacts | DirArtifacts = RunArtifacts(args.repo, args.run, scratch)
        else:
            jobs = checked_jobs(jobs_from_file(args.jobs_json))
            artifacts = DirArtifacts(args.from_dir)
        if args.job:
            missing = sorted(set(args.job) - {job["id"] for job in jobs})
            if missing:
                raise Failure(f"the run has no job {', '.join(map(str, missing))}")
            jobs = [job for job in jobs if job["id"] in set(args.job)]
        printed = 0
        any_unknown = any_failed = False
        for job in jobs:
            name = artifact_name(job["id"])
            if not ran_pool(job) and not artifacts.has(name):
                continue
            directory, why = artifacts.fetch(name)
            rows = [unknown(job, None, why)] if directory is None else job_rows(job, directory)
            for one in rows:
                print(json.dumps(one, separators=(",", ":")), flush=True)
                any_unknown = any_unknown or one["kind"] == "unknown"
                any_failed = any_failed or one["status"] == "fail"
            printed += len(rows)
        if printed == 0:
            print(
                f"harness_case_times: none of the {len(jobs)} jobs read ran a harness in a pool, so there is no case to print",
                file=sys.stderr,
            )
        return 1 if any_unknown else 3 if any_failed else 0
    except Failure as failure:
        print(f"harness_case_times: {failure}", file=sys.stderr)
        return 2
    finally:
        if scratch is not None:
            shutil.rmtree(scratch, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
