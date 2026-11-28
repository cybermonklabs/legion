#!/usr/bin/env python3
"""Read how much the wall time of each envoy-go job spreads over several runs of one commit, and
the factors that may account for that spread.

A change to the envoy-go jobs counts as a gain only once it is larger than the spread of one
commit's times between runs. This script measures that spread from GitHub Actions runs of ci.yaml
in cybermonklabs/legion, and weighs the factors a run's jobs API and its jobs' logs give for each
job-run against it, so that the cause it names rests on numbers a reader can read again.

    go_cpu_record.py spread --runs <run id> <run id> ...
    go_cpu_record.py factors --runs <run id> <run id> ...
    go_cpu_record.py rule --table <file.json> [--method spearman|pearson]
    go_cpu_record.py postgres-packages [--root <dir>]

The jobs counted are those of dispatch://INFRA-6312's plan (D11), by the names the jobs API gives
(COUNTED_JOBS): legs 1-5, alone, race-impossible, skip counter and natslive of `envoy-go`, the
aggregator `envoy-go / envoy-go`, and `envoy-go-checks / envoy-go-checks`. A job-run counts when
its conclusion is `success`; every other job-run is listed with its conclusion. A job-run's wall is
its `completed_at` less its `started_at`, in seconds, which leaves out the time it queued.

`spread` and `factors` read, through gh, each run (`gh api repos/cybermonklabs/legion/actions/
runs/<id>`: its head_sha, run_attempt and created_at) and every page of 100 of the jobs of its
first attempt (`.../runs/<id>/attempts/1/jobs?per_page=100&page=<n>`, until the pages hold the
run's total_count; a run holds about 111 jobs, and the aggregator is on page 2). Both refuse,
with exit 2 and the reason, before they read any run's jobs: fewer than five runs or a run given
twice (a spread over fewer runs says little, and the same run twice is no second run); runs whose
head_sha differ, naming each run and its sha (the spread is that of one commit); and a run whose
run_attempt is not 1 (the reader reads attempt 1's jobs, and a re-run is a run of its own).
`factors` refuses the same, because its bar is the median spread of the same runs.

`spread` prints, for each counted job in COUNTED_JOBS' order, n (its successful job-runs), their
min, median and max wall in seconds, the spread (max − min) ÷ median as a percentage with one
decimal, the job ids of the min and the max (the first in the order of --runs where several
job-runs tie), and the conclusions of its other job-runs; a job with no successful job-run has no
spread. A median of an even count is the mean of the middle two, so 525.5 can occur. Then, for
each run, each counted job's id, conclusion and wall. It exits 1, naming on stderr each job with
fewer than five successful job-runs, whose spread is too thin to compare a change against, and
still prints everything else; 0 otherwise.

`factors` (the runs of one kind, pooled; its caller runs it once per kind) prints one row per
job-run of the counted jobs, with its conclusion, wall, test seconds, d and d_test, and these
factors, each UNKNOWN where its source does not give it:

    queue_s            the job's started_at less the run's created_at (jobs API)
    region, worker_id, image_version
                       the job log's Set up job lines: `Region: <region>`, `Worker ID: {...}`, and
                       the `Version: <version>` after `Image: ubuntu-24.04-arm` (an earlier
                       `Version:` is the Runner Image Provisioner's)
    model, cpus, probe_one, probe_all
                       the log's `go-cpu runner:` line (.github/scripts/go-cpu-runner.py)
    busy, steal, iowait, new_cache_entries, postgres_cpu
                       the log's `go-cpu usage:` line; envoy-go-checks prints it without the last
                       two, and postgres_cpu is NONE for a job given no container, so both are
                       UNKNOWN there
    cache_key, cache_kind
                       actions/cache's lines in the log: `Cache restored from key: <key>` is a
                       hit, or a restore-key hit after `Cache hit for restore-key: <key>`; `Cache
                       not found for input keys: ...` is a miss, which restored no key (`none`)
    cache_restore_s    the seconds of the job's steps whose names start CACHE_RESTORE_STEP
    setup <step>       the seconds of the job's steps whose names start each other prefix of
                       SETUP_STEPS

The log is read with `gh api --allow-escape-sequences repos/cybermonklabs/legion/actions/jobs/
<id>/logs`: gh refuses to print a job log without that flag ("the response contains terminal
escape sequences"). The reader strips each \\x1b before it parses, so a line a step echoes in
colour starts `[36;1m` and is no fact; each line starts with a 28-character timestamp and a space,
and the first fact of each kind is the one read, since the Set up job lines come first. A log gh
cannot give (GitHub drops it after the repository's retention, 90 days by default) leaves its
job-run's log factors UNKNOWN, and is listed under `logs not read` with gh's reason.

d is the job-run's wall ÷ the median wall of that job over every job-run of it in the runs given,
less 1, and d_test the same of its test steps' seconds (the test steps: every step that is neither
a setup step nor in NOT_TEST_STEP_NAMES or NOT_TEST_STEP_PREFIXES). A skipped job-run never ran on a
runner, so it has no d and is in no median and no statistic.

Then the decision rule, over the job-runs given, pooled, for each of eight branches: successful
job-runs only and all job-runs; d and d_test; Spearman and Pearson. For each factor it prints n,
the job-runs left out because the factor is UNKNOWN in their rows, the number of values, the
statistic and the effect, and whether the factor meets its bar:

  * a numeric factor meets it when |ρ| (Spearman's, with average ranks for ties; Pearson's r in
    the Pearson branches) between d and the factor is at least 0.5, and its effect, the slope of
    a least-squares fit of d on the factor times the factor's range over the job-runs, is in size
    at least half the median spread (a fraction) of the counted jobs, each job's spread as
    `spread` prints it over the same runs;
  * a categorical factor meets it when η², the share of d's variance between its values, is at
    least 0.5 and each value has at least two job-runs; its effect is the range of the per-value
    mean of d;
  * a factor with one value over the job-runs is excluded, with that value shown.

The cause is the factor that meets its bar with the largest effect. Where two or more meet it,
each is printed (`meets:`), and the first's statistic is computed again within each value of the
second (each half, at the second's median, for a numeric one), which says whether the first
still tracks d once the second is held. Where none meets it: `cause: not separated`. The bar is
the whole wall's median spread in every branch.

The per-package ratios of the analysis plan need the CPU rows of the jobs' artifacts, which a
later round reads; until then `factors` says so in one line. The same round adds the subcommands
that read those rows, `cpu --run <id>` and `coverage --run <id>`, where `parse` marks their place.

`rule` runs the same rule over a table in a file, so it can be tested on numbers chosen by hand:
`{"median_spread": <fraction>, "rows": [{"d": <number>, "<factor>": <value>, ...}, ...]}`, a
factor numeric where its values are numbers and categorical where they are strings, and null
UNKNOWN.

`postgres-packages` prints each package directory under --root (packages/envoy by default) and
whether its tests use Postgres: a directory is a Postgres user when one of its `_test.go` files
imports a Go import path ending in one of POSTGRES_IMPORTS, or names one of POSTGRES_VARIABLES.
Only test files count, since it is the tests that open the database. The later round splits the
per-package ratios by it.

Exit 2 is a usage error, a refused set of runs, or a gh call for a run or its jobs that failed
outright, with the reason on stderr. The script talks to GitHub only through the gh CLI and gh's
own credentials, holds every response in memory, and writes no file.

Standard library only. Its tests are in go_cpu_record.test.sh.
"""

import argparse
import datetime
import json
import math
import os
import re
import statistics
import subprocess
import sys
from pathlib import Path

REPO = "cybermonklabs/legion"
# The largest page of jobs GitHub gives.
PAGE = 100
# The fewest runs a spread is measured over.
MIN_RUNS = 5
UNKNOWN = "UNKNOWN"

# The jobs counted (D11), in the order the tables print them, by the names the jobs API gives.
COUNTED_JOBS = (
    "envoy-go / envoy-go (leg 1)",
    "envoy-go / envoy-go (leg 2)",
    "envoy-go / envoy-go (leg 3)",
    "envoy-go / envoy-go (leg 4)",
    "envoy-go / envoy-go (leg 5)",
    "envoy-go / envoy-go (alone)",
    "envoy-go / envoy-go (race-impossible)",
    "envoy-go / envoy-go (skip counter)",
    "envoy-go / envoy-go (natslive)",
    "envoy-go / envoy-go",
    "envoy-go-checks / envoy-go-checks",
)

# The setup steps, by the starts of their names as the jobs API gives them: what a job does before
# its tests (a leg's steps: `Set up job`, `Initialize containers`, `Run actions/checkout@<sha>`,
# `Run ./.github/actions/setup-bun`, `Run bun install --frozen-lockfile`, `Run actions/setup-go@
# <sha>`, `Run actions/cache/restore@<sha>`, `Read the service's settings back`, ...). Each is a
# factor of its own, since one slow setup step stretches the wall without touching the tests.
SETUP_STEPS = (
    "Set up job",
    "Initialize containers",
    "Run actions/checkout",
    "Run ./.github/actions/setup-bun",
    "Run bun install",
    "Run actions/setup-go",
    "Run ./.github/actions/install-",
    "Install ",
    "Run actions/cache/restore",
    "Read the service's settings back",
    "Check the service allows",
    "Postgres service version",
    "Pull test container images",
    "Record the runner's facts",
    "Sample the service's connections",
    "Start a Postgres that preloads",
)
# The setup step whose seconds are the factor cache_restore_s, so it has no `setup` column too.
CACHE_RESTORE_STEP = "Run actions/cache/restore"
# The steps that are neither setup nor test: what runs after the tests to report and clean up.
# Every other step is a test step; in envoy-go-checks those are the harness steps, which start
# the pool's harnesses, and the pool's wait step, which waits for them.
NOT_TEST_STEP_NAMES = ("Stop containers", "Complete job")
NOT_TEST_STEP_PREFIXES = ("Post ", "Report ", "Upload ")

NUMERIC = "numeric"
CATEGORICAL = "categorical"
# The factors of a row, in the order its columns print, each with its kind.
LOG_FACTORS = (
    ("region", CATEGORICAL),
    ("worker_id", CATEGORICAL),
    ("image_version", CATEGORICAL),
    ("model", CATEGORICAL),
    ("cpus", NUMERIC),
    ("probe_one", NUMERIC),
    ("probe_all", NUMERIC),
    ("busy", NUMERIC),
    ("steal", NUMERIC),
    ("iowait", NUMERIC),
    ("cache_key", CATEGORICAL),
    ("cache_kind", CATEGORICAL),
)
SETUP_COLUMNS = tuple(f"setup {prefix.strip()}" for prefix in SETUP_STEPS if prefix != CACHE_RESTORE_STEP)
FACTORS = (
    (("queue_s", NUMERIC),)
    + LOG_FACTORS
    + (("cache_restore_s", NUMERIC), ("new_cache_entries", NUMERIC), ("postgres_cpu", NUMERIC))
    + tuple((column, NUMERIC) for column in SETUP_COLUMNS)
)
ROW_COLUMNS = ("run", "job", "id", "conclusion", "wall_s", "test_s", "d", "d_test") + tuple(
    name for name, _ in FACTORS
)

# The decision rule's bars, each as the header gives it.
RANK_BAR = 0.5
ETA_BAR = 0.5
MIN_JOB_RUNS_A_VALUE = 2
EFFECT_SHARE_OF_SPREAD = 0.5
METHODS = {"spearman": ("Spearman", "rho"), "pearson": ("Pearson", "r")}

# The rule of dispatch://INFRA-6312's analysis plan for the packages that use Postgres: the test
# helpers that open a database, as the ends of Go import paths, and the variables that name one.
POSTGRES_IMPORTS = ("storetest", "testpg", "testpg/testdb", "cipher/store/storetest", "pgmigrate/pgmigratetest")
POSTGRES_VARIABLES = ("DISPATCH_TEST_DATABASE_URL", "BROKER_TEST_DATABASE_URL")
DEFAULT_PACKAGES_ROOT = Path(__file__).resolve().parents[2] / "packages" / "envoy"

LOG_LINE = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{7}Z ")
FIELD = re.compile(r'(\w+)=("[^"]*"|\S*)')
GO_IMPORT_ONE = re.compile(r'^import\s+(?:[\w.]+\s+)?["`]([^"`]+)["`]', re.M)
GO_IMPORT_BLOCK = re.compile(r"^import\s*\((.*?)^\)", re.M | re.S)
GO_IMPORT_SPEC = re.compile(r'^\s*(?:[\w.]+\s+)?["`]([^"`]+)["`]', re.M)


class Failure(Exception):
    """A usage error, a refused set of runs, or a gh call that failed outright: exit 2."""


# ------------------------------------------------------------------------------------ GitHub


def last_line(text: str) -> str:
    lines = [line for line in text.splitlines() if line.strip()]
    return lines[-1].strip() if lines else "it printed nothing"


def gh(args: list[str]) -> tuple[int, bytes, str]:
    try:
        done = subprocess.run(["gh", *args], capture_output=True, check=False)
    except OSError as error:
        raise Failure(f"could not run gh: {error}") from error
    return done.returncode, done.stdout, done.stderr.decode("utf-8", "replace")


def gh_json(endpoint: str) -> object:
    code, out, err = gh(["api", endpoint])
    if code != 0:
        raise Failure(f"gh api {endpoint} exited {code}: {last_line(err)}")
    try:
        return json.loads(out)
    except ValueError as error:
        raise Failure(f"gh api {endpoint} printed no JSON: {error}") from error


def read_jobs(run_id: int) -> list[dict]:
    """Every job of the run's first attempt, a page of PAGE at a time, until the pages hold the
    run's total_count."""
    jobs: list[dict] = []
    page = 1
    while True:
        endpoint = f"repos/{REPO}/actions/runs/{run_id}/attempts/1/jobs?per_page={PAGE}&page={page}"
        payload = gh_json(endpoint)
        batch = payload.get("jobs") if isinstance(payload, dict) else None
        total = payload.get("total_count") if isinstance(payload, dict) else None
        if not isinstance(batch, list) or not isinstance(total, int):
            raise Failure(f"gh api {endpoint} gave no jobs list and total_count")
        jobs.extend(job for job in batch if isinstance(job, dict))
        if len(jobs) >= total:
            return jobs
        if not batch:
            raise Failure(f"gh api {endpoint} gave no jobs, with {len(jobs)} of the run's {total} read")
        page += 1


def read_log(job_id: int) -> tuple[str | None, str | None]:
    """The job's log, or None and gh's reason."""
    endpoint = f"repos/{REPO}/actions/jobs/{job_id}/logs"
    code, out, err = gh(["api", "--allow-escape-sequences", endpoint])
    if code != 0:
        return None, f"gh api --allow-escape-sequences {endpoint} exited {code}: {last_line(err)}"
    return out.decode("utf-8", "replace"), None


# ------------------------------------------------------------------------------ the set of runs


def parse_time(text: object) -> datetime.datetime | None:
    if not isinstance(text, str):
        return None
    try:
        return datetime.datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return None


def seconds_between(start: object, end: object) -> float | None:
    a, b = parse_time(start), parse_time(end)
    return None if a is None or b is None else (b - a).total_seconds()


def read_set(run_ids: list[int]) -> tuple[list[dict], str]:
    """The runs, refused as the header says before any of their jobs is read, and their commit."""
    if len(run_ids) < MIN_RUNS:
        raise Failure(
            f"at least five runs of one commit are needed to measure a spread, and {len(run_ids)} were given: "
            + " ".join(map(str, run_ids))
        )
    repeated = sorted({run for run in run_ids if run_ids.count(run) > 1})
    if repeated:
        raise Failure("run ids given more than once: " + " ".join(map(str, repeated)))
    runs = []
    for run_id in run_ids:
        run = gh_json(f"repos/{REPO}/actions/runs/{run_id}")
        if not isinstance(run, dict) or not isinstance(run.get("head_sha"), str):
            raise Failure(f"gh api repos/{REPO}/actions/runs/{run_id} gave no head_sha")
        runs.append({"id": run_id, **{key: run.get(key) for key in ("head_sha", "run_attempt", "created_at")}})
    problems = [
        f"run {run['id']} is attempt {run['run_attempt']}: the reader reads the jobs of attempt 1, "
        "and a re-run is a run of its own"
        for run in runs
        if run["run_attempt"] != 1
    ]
    if len({run["head_sha"] for run in runs}) > 1:
        problems.append(
            "the runs are of more than one commit, and a spread is one commit's:\n"
            + "\n".join(f"  {run['id']} {run['head_sha']}" for run in runs)
        )
    if problems:
        raise Failure("\n".join(problems))
    return runs, runs[0]["head_sha"]


def job_runs(runs: list[dict]) -> list[dict]:
    """One entry for each counted job of each run, in run order then COUNTED_JOBS' order: the job
    itself, or, for a job the run does not hold, an entry whose conclusion is `absent`."""
    entries = []
    for run in runs:
        by_name: dict[str, list[dict]] = {}
        for job in read_jobs(run["id"]):
            by_name.setdefault(job.get("name"), []).append(job)
        for name in COUNTED_JOBS:
            found = by_name.get(name, [])
            if len(found) > 1:
                raise Failure(f"run {run['id']} holds {len(found)} jobs named {name}")
            if not found:
                absent = {"run": run, "name": name, "id": None, "conclusion": "absent", "wall": None, "job": None}
                entries.append(absent)
                continue
            job = found[0]
            entries.append(
                {
                    "run": run,
                    "name": name,
                    "id": job.get("id"),
                    "conclusion": job.get("conclusion") or job.get("status") or UNKNOWN,
                    "wall": seconds_between(job.get("started_at"), job.get("completed_at")),
                    "job": job,
                }
            )
    return entries


def spread_of(entries: list[dict]) -> dict | None:
    """The successful job-runs' n, min, median, max and spread, with the job ids of the min and
    the max (the first of several that tie), or None where none succeeded."""
    walls = [(e["wall"], e["id"]) for e in entries if e["conclusion"] == "success" and e["wall"] is not None]
    if not walls:
        return None
    values = [wall for wall, _ in walls]
    low, high, median = min(values), max(values), statistics.median(values)
    return {
        "n": len(values),
        "min": low,
        "median": median,
        "max": high,
        "spread": (high - low) / median if median > 0 else None,
        "min_job": next(job for wall, job in walls if wall == low),
        "max_job": next(job for wall, job in walls if wall == high),
    }


def number(value: float | None, unknown: str = UNKNOWN) -> str:
    if value is None:
        return unknown
    if float(value).is_integer():
        return str(int(value))
    return f"{value:.10g}"


def fraction(value: float | None) -> str:
    return UNKNOWN if value is None else f"{value:.4f}"


def table(title: str, columns: tuple[str, ...] | list[str], rows: list[list[str]]) -> None:
    """A table as every subcommand prints it: a title line, a tab-separated header and rows, and
    a blank line after."""
    print(f"== {title}")
    print("\t".join(columns))
    for row in rows:
        print("\t".join(row))
    print()


def set_title(runs: list[dict], sha: str) -> str:
    return f"{len(runs)} runs of {sha}, attempt 1 ({' '.join(str(run['id']) for run in runs)})"


# ------------------------------------------------------------------------------------- spread


def spread_command(args: argparse.Namespace) -> int:
    runs, sha = read_set(args.runs)
    entries = job_runs(runs)
    rows = []
    thin = []
    for name in COUNTED_JOBS:
        mine = [e for e in entries if e["name"] == name]
        others: dict[str, int] = {}
        for e in mine:
            if e["conclusion"] != "success":
                others[e["conclusion"]] = others.get(e["conclusion"], 0) + 1
        not_counted = ", ".join(f"{conclusion} {count}" for conclusion, count in others.items()) or "-"
        stats = spread_of(mine)
        if stats is None:
            rows.append([name, "0", "-", "-", "-", "-", "-", "-", not_counted])
        else:
            shown = "-" if stats["spread"] is None else f"{100 * stats['spread']:.1f} %"
            rows.append(
                [name, str(stats["n"]), number(stats["min"]), number(stats["median"]), number(stats["max"]), shown]
                + [str(stats["min_job"]), str(stats["max_job"]), not_counted]
            )
        n = 0 if stats is None else stats["n"]
        if n < MIN_RUNS:
            thin.append(f"{name} ({n})")
    table(
        f"spread: {set_title(runs, sha)}",
        ("job", "n", "min_s", "median_s", "max_s", "spread", "min_job", "max_job", "not_counted"),
        rows,
    )
    table(
        "job-runs",
        ("run", "job", "id", "conclusion", "wall_s"),
        [[str(e["run"]["id"]), e["name"], number(e["id"], "-"), e["conclusion"], number(e["wall"])] for e in entries],
    )
    if thin:
        print(
            "go_cpu_record.py spread: fewer than five successful job-runs, too few for a spread to compare a change "
            "against: " + ", ".join(thin),
            file=sys.stderr,
        )
        return 1
    return 0


# ------------------------------------------------------------------------------------ factors


def as_number(text: object) -> float | None:
    """A figure as a log line spells it, or None for UNKNOWN, NONE or anything else no finite
    number."""
    try:
        value = float(text)
    except (TypeError, ValueError):
        return None
    return value if math.isfinite(value) else None


def log_lines(text: str):
    """The text of each line of a job log after its timestamp, with every \\x1b removed."""
    for line in text.lstrip("\ufeff").replace("\x1b", "").splitlines():
        if LOG_LINE.match(line):
            yield line[29:]


def fields(text: str) -> dict[str, str]:
    """The key=value pairs of a `go-cpu` line, a quoted value without its quotes."""
    return {key: value[1:-1] if value.startswith('"') else value for key, value in FIELD.findall(text)}


def log_facts(text: str) -> dict:
    """The factors a job's log gives, each read from its first line of the kind."""
    facts: dict = {}
    after_image = False
    restore_key = False
    for line in log_lines(text):
        if line.startswith("Region: "):
            facts.setdefault("region", line[len("Region: ") :].strip() or None)
        elif line.startswith("Worker ID: "):
            facts.setdefault("worker_id", line[len("Worker ID: ") :].strip() or None)
        elif line.startswith("Image: "):
            after_image = True
        elif line.startswith("Version: ") and after_image:
            facts.setdefault("image_version", line[len("Version: ") :].strip() or None)
        elif line.startswith("go-cpu runner: ") and "cpus" not in facts:
            runner = fields(line[len("go-cpu runner: ") :])
            model = runner.get("model")
            facts["model"] = model if model and model != UNKNOWN else None
            for key in ("cpus", "probe_one", "probe_all"):
                facts[key] = as_number(runner.get(key))
        elif line.startswith("go-cpu usage: ") and "busy" not in facts:
            usage = fields(line[len("go-cpu usage: ") :])
            for key in ("busy", "steal", "iowait", "new_cache_entries", "postgres_cpu"):
                facts[key] = as_number(usage.get(key))
        elif "cache_kind" in facts:
            continue
        elif line.startswith("Cache hit for restore-key: "):
            restore_key = True
        elif line.startswith("Cache restored from key: "):
            facts["cache_key"] = line[len("Cache restored from key: ") :].strip()
            facts["cache_kind"] = "restore-key" if restore_key else "hit"
        elif line.startswith("Cache not found for input keys"):
            facts["cache_key"] = "none"
            facts["cache_kind"] = "miss"
    return facts


def step_facts(job: dict) -> dict:
    """The job's test seconds, its cache restore's seconds and each setup column's seconds, from
    the jobs API's steps: each a sum over the steps it covers, None where it covers none or a step
    of it has no times. A skipped step without times took none."""
    sums: dict[str, float | None] = {}

    def add(key: str, seconds: float | None) -> None:
        if seconds is None or (key in sums and sums[key] is None):
            sums[key] = None
        else:
            sums[key] = (sums.get(key) or 0.0) + seconds

    for step in job.get("steps") or []:
        name = step.get("name") or ""
        seconds = seconds_between(step.get("started_at"), step.get("completed_at"))
        if seconds is None and step.get("conclusion") == "skipped":
            seconds = 0.0
        prefix = next((p for p in SETUP_STEPS if name.startswith(p)), None)
        if prefix == CACHE_RESTORE_STEP:
            add("cache_restore_s", seconds)
        elif prefix is not None:
            add(f"setup {prefix.strip()}", seconds)
        elif name not in NOT_TEST_STEP_NAMES and not name.startswith(NOT_TEST_STEP_PREFIXES):
            add("test_s", seconds)
    return sums


def ratio_less_one(value: float | None, median: float | None) -> float | None:
    return None if value is None or not median else value / median - 1


def factor_rows(entries: list[dict]) -> tuple[list[dict], list[tuple[int, str]]]:
    """One row per job-run, and the logs not read, each with gh's reason."""
    rows = []
    unread = []
    for e in entries:
        if e["job"] is None:
            continue
        job = e["job"]
        ran = e["conclusion"] != "skipped"
        row: dict = {"run": e["run"]["id"], "job": e["name"], "id": e["id"], "conclusion": e["conclusion"], "ran": ran}
        row["wall_s"] = e["wall"] if ran else None
        steps = step_facts(job)
        row["test_s"] = steps.get("test_s") if ran else None
        row["queue_s"] = seconds_between(e["run"]["created_at"], job.get("started_at"))
        for column in ("cache_restore_s",) + SETUP_COLUMNS:
            row[column] = steps.get(column)
        facts: dict = {}
        if ran:
            text, why = read_log(e["id"])
            if text is None:
                unread.append((e["id"], why))
            else:
                facts = log_facts(text)
        for name, _ in LOG_FACTORS + (("new_cache_entries", NUMERIC), ("postgres_cpu", NUMERIC)):
            row[name] = facts.get(name)
        rows.append(row)
    for measure, d in (("wall_s", "d"), ("test_s", "d_test")):
        medians = {}
        for name in COUNTED_JOBS:
            values = [r[measure] for r in rows if r["job"] == name and r[measure] is not None]
            medians[name] = statistics.median(values) if values else None
        for r in rows:
            r[d] = ratio_less_one(r[measure], medians[r["job"]])
    return rows, unread


def shown(column: str, value: object) -> str:
    if value is None:
        return UNKNOWN
    if column in ("d", "d_test"):
        return f"{value:.4f}"
    if isinstance(value, float):
        return number(value)
    return str(value)


def factors_command(args: argparse.Namespace) -> int:
    runs, sha = read_set(args.runs)
    entries = job_runs(runs)
    rows, unread = factor_rows(entries)
    table(
        f"factors: {set_title(runs, sha)}; {len(rows)} job-runs of the counted jobs",
        ROW_COLUMNS,
        [[shown(column, row[column]) for column in ROW_COLUMNS] for row in rows],
    )
    table("logs not read", ("id", "why"), [[str(job), why] for job, why in unread])
    print("per-package ratios: need the CPU rows (later round)")
    print()
    spreads = []
    for name in COUNTED_JOBS:
        stats = spread_of([e for e in entries if e["name"] == name])
        if stats is not None and stats["spread"] is not None:
            spreads.append(stats["spread"])
    median_spread = statistics.median(spreads) if spreads else None
    bar = None if median_spread is None else EFFECT_SHARE_OF_SPREAD * median_spread
    print(
        f"median spread of the counted jobs: {fraction(median_spread)} over the {len(spreads)} jobs with a "
        f"successful job-run; a numeric factor's effect must be at least half of it: {fraction(bar)}"
    )
    print()
    populations = (
        ("successful job-runs", lambda r: r["conclusion"] == "success"),
        ("all job-runs", lambda r: r["ran"]),
    )
    for population, keep in populations:
        for measure, d in (("whole wall", "d"), ("test steps", "d_test")):
            for method in METHODS:
                chosen = [
                    {"d": r[d], **{name: r[name] for name, _ in FACTORS}} for r in rows if keep(r) and r[d] is not None
                ]
                decide(f"{population}, {measure}, {METHODS[method][0]}", chosen, FACTORS, method, bar)
    return 0


# ---------------------------------------------------------------------------- the decision rule


def ranks(values: list[float]) -> list[float]:
    """Each value's rank from 1, tied values sharing the mean of the ranks they span."""
    order = sorted(range(len(values)), key=lambda i: values[i])
    result = [0.0] * len(values)
    start = 0
    while start < len(order):
        end = start
        while end + 1 < len(order) and values[order[end + 1]] == values[order[start]]:
            end += 1
        for k in range(start, end + 1):
            result[order[k]] = (start + end) / 2 + 1
        start = end + 1
    return result


def pearson(xs: list[float], ys: list[float]) -> float | None:
    """Pearson's r, or None for fewer than three pairs or a side that does not vary."""
    if len(xs) < 3 or min(xs) == max(xs) or min(ys) == max(ys):
        return None
    mx, my = math.fsum(xs) / len(xs), math.fsum(ys) / len(ys)
    sxy = math.fsum((x - mx) * (y - my) for x, y in zip(xs, ys))
    sxx = math.fsum((x - mx) ** 2 for x in xs)
    syy = math.fsum((y - my) ** 2 for y in ys)
    return sxy / math.sqrt(sxx * syy)


def spearman(xs: list[float], ys: list[float]) -> float | None:
    return pearson(ranks(xs), ranks(ys))


def slope(xs: list[float], ys: list[float]) -> float:
    """The slope of the least-squares line of ys on xs, whose xs vary."""
    mx, my = math.fsum(xs) / len(xs), math.fsum(ys) / len(ys)
    return math.fsum((x - mx) * (y - my) for x, y in zip(xs, ys)) / math.fsum((x - mx) ** 2 for x in xs)


def eta_squared(groups: dict) -> float | None:
    """The share of the variance of all the groups' values that lies between the groups' means, or
    None where the values do not vary."""
    values = [y for ys in groups.values() for y in ys]
    if min(values) == max(values):
        return None
    mean = math.fsum(values) / len(values)
    total = math.fsum((y - mean) ** 2 for y in values)
    between = math.fsum(len(ys) * (math.fsum(ys) / len(ys) - mean) ** 2 for ys in groups.values())
    return between / total


def judge(rows: list[dict], name: str, kind: str, method: str, bar: float | None) -> dict:
    """The factor's statistic over the rows (each with its `d`), its effect, and whether it meets
    its bar, with a note saying why not."""
    pairs = [(r[name], r["d"]) for r in rows if r.get(name) is not None]
    values = list(dict.fromkeys(value for value, _ in pairs))
    result = {
        "factor": name,
        "kind": kind,
        "n": len(pairs),
        "left_out": len(rows) - len(pairs),
        "values": len(values),
        "statistic": "-",
        "value": None,
        "effect": None,
        "meets": "no",
        "note": "",
    }
    if not pairs:
        result["note"] = "no job-run gives it"
        return result
    if len(values) == 1:
        result["meets"] = "excluded"
        result["note"] = f"one value: {shown(name, values[0])}"
        return result
    ds = [d for _, d in pairs]
    if kind == NUMERIC:
        xs = [float(x) for x, _ in pairs]
        result["statistic"] = METHODS[method][1]
        result["value"] = (spearman if method == "spearman" else pearson)(xs, ds)
        result["effect"] = slope(xs, ds) * (max(xs) - min(xs))
        if result["value"] is None:
            result["note"] = "fewer than three job-runs, or d does not vary"
        elif abs(result["value"]) < RANK_BAR:
            result["note"] = f"|{result['statistic']}| under {RANK_BAR}"
        elif bar is None:
            result["note"] = "no median spread to set the bar"
        elif abs(result["effect"]) < bar:
            result["note"] = "effect under the bar"
        else:
            result["meets"] = "yes"
        return result
    groups: dict = {}
    for value, d in pairs:
        groups.setdefault(value, []).append(d)
    means = [math.fsum(ds) / len(ds) for ds in groups.values()]
    result["statistic"] = "eta2"
    result["value"] = eta_squared(groups)
    result["effect"] = max(means) - min(means)
    if result["value"] is None:
        result["note"] = "d does not vary"
    elif result["value"] < ETA_BAR:
        result["note"] = f"eta2 under {ETA_BAR}"
    elif min(len(ds) for ds in groups.values()) < MIN_JOB_RUNS_A_VALUE:
        result["note"] = "a value with one job-run"
    else:
        result["meets"] = "yes"
    return result


def described(result: dict) -> str:
    return f"{result['factor']} ({result['statistic']}={result['value']:.3f}, effect={result['effect']:.4f})"


def within(first: dict, second: dict, kinds: dict, rows: list[dict], method: str, bar: float | None) -> list[str]:
    """The first factor's statistic within each value of the second, or each half of it split at
    its median for a numeric one."""
    name = second["factor"]
    held = [r for r in rows if r.get(name) is not None]
    if kinds[name] == CATEGORICAL:
        values = sorted({r[name] for r in held})
        parts = [(f"{name}={value}", [r for r in held if r[name] == value]) for value in values]
    else:
        median = statistics.median(float(r[name]) for r in held)
        parts = [
            (f"{name}<={number(median)}", [r for r in held if float(r[name]) <= median]),
            (f"{name}>{number(median)}", [r for r in held if float(r[name]) > median]),
        ]
    lines = []
    for label, part in parts:
        result = judge(part, first["factor"], kinds[first["factor"]], method, bar)
        statistic = METHODS[method][1] if kinds[first["factor"]] == NUMERIC else "eta2"
        value = "undefined" if result["value"] is None else f"{result['value']:.3f}"
        lines.append(f"within {label} (n={len(part)}): {first['factor']} {statistic}={value}")
    return lines


def decide(title: str, rows: list[dict], factors, method: str, bar: float | None) -> None:
    """Prints each factor's statistic over the rows, then the verdict."""
    kinds = dict(factors)
    results = [judge(rows, name, kind, method, bar) for name, kind in factors]
    table(
        f"statistics: {title}",
        ("factor", "kind", "n", "left_out", "values", "statistic", "value", "effect", "meets", "note"),
        [
            [r["factor"], r["kind"], str(r["n"]), str(r["left_out"]), str(r["values"]), r["statistic"]]
            + ["-" if r["value"] is None else f"{r['value']:.3f}", "-" if r["effect"] is None else f"{r['effect']:.4f}"]
            + [r["meets"], r["note"] or "-"]
            for r in results
        ],
    )
    meeting = sorted((r for r in results if r["meets"] == "yes"), key=lambda r: -abs(r["effect"]))
    if not meeting:
        print("cause: not separated")
    else:
        if len(meeting) > 1:
            for r in meeting:
                print(f"meets: {described(r)}")
        print(f"cause: {described(meeting[0])}")
        if len(meeting) > 1:
            print("\n".join(within(meeting[0], meeting[1], kinds, rows, method, bar)))
    print()


def rule_command(args: argparse.Namespace) -> int:
    try:
        with open(args.table, encoding="utf-8") as file:
            payload = json.load(file)
    except (OSError, ValueError) as error:
        raise Failure(f"--table {args.table} could not be read as JSON: {error}") from error
    spread = payload.get("median_spread") if isinstance(payload, dict) else None
    rows = payload.get("rows") if isinstance(payload, dict) else None
    if not isinstance(spread, (int, float)) or isinstance(spread, bool) or not isinstance(rows, list):
        raise Failure(f"--table {args.table} holds no median_spread number and rows list")
    names: dict[str, None] = {}
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get("d"), (int, float)) or isinstance(row.get("d"), bool):
            raise Failure(f"--table {args.table}: a row with no number d: {json.dumps(row)}")
        names.update(dict.fromkeys(key for key in row if key != "d"))
    factors = []
    for name in names:
        known = [row[name] for row in rows if row.get(name) is not None]
        if all(isinstance(v, (int, float)) and not isinstance(v, bool) for v in known):
            factors.append((name, NUMERIC))
        elif all(isinstance(v, str) for v in known):
            factors.append((name, CATEGORICAL))
        else:
            raise Failure(f"--table {args.table}: factor {name} holds both numbers and strings")
    bar = EFFECT_SHARE_OF_SPREAD * spread
    print(f"median spread: {fraction(spread)}; a numeric factor's effect must be at least half of it: {fraction(bar)}")
    print()
    table_rows = [{"d": row["d"], **{name: row.get(name) for name in names}} for row in rows]
    decide(f"{args.table}, {METHODS[args.method][0]}", table_rows, factors, args.method, bar)
    return 0


# ---------------------------------------------------------------------------- Postgres packages


def go_imports(text: str) -> list[str]:
    """The import paths of a Go file: its one-line imports and each spec of its import blocks; a
    comment line is no spec."""
    paths = GO_IMPORT_ONE.findall(text)
    for block in GO_IMPORT_BLOCK.findall(text):
        paths.extend(GO_IMPORT_SPEC.findall(block))
    return paths


def postgres_reason(root: Path, directory: Path, files: list[str]) -> str | None:
    """Why the package directory's tests use Postgres, or None where they do not."""
    for name in files:
        if not name.endswith("_test.go"):
            continue
        shown_path = (directory / name).relative_to(root).as_posix()
        text = (directory / name).read_text(encoding="utf-8", errors="replace")
        for path in go_imports(text):
            if any(path == end or path.endswith("/" + end) for end in POSTGRES_IMPORTS):
                return f"{shown_path} imports {path}"
        for variable in POSTGRES_VARIABLES:
            if variable in text:
                return f"{shown_path} names {variable}"
    return None


def packages_command(args: argparse.Namespace) -> int:
    root = Path(args.root)
    if not root.is_dir():
        raise Failure(f"--root {root} is no directory")
    rows = []
    for directory, subdirectories, files in os.walk(root):
        # The go command skips testdata, vendor and every name that starts with . or _, so a
        # directory there is no package of the module.
        subdirectories[:] = [d for d in subdirectories if d not in ("testdata", "vendor") and not d.startswith((".", "_"))]
        go_files = sorted(f for f in files if f.endswith(".go") and not f.startswith((".", "_")))
        if not go_files:
            continue
        package = Path(directory).relative_to(root).as_posix()
        why = postgres_reason(root, Path(directory), go_files)
        rows.append([package, "no" if why is None else "yes", why or "-"])
    table(f"packages: {root}", ("package", "postgres", "why"), sorted(rows))
    return 0


# ------------------------------------------------------------------------------------------ CLI


def run_id(text: str) -> int:
    if not text.isdigit() or int(text) <= 0:
        raise argparse.ArgumentTypeError(f"{text!r} is no run id")
    return int(text)


def parse(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Read the spread of the envoy-go jobs' wall times over runs of one commit, and the factors that may account for it.",
        epilog="Exit 2: a usage error, a refused set of runs, or a gh call that failed. spread exits 1 when a job has fewer than five successful job-runs.",
    )
    commands = parser.add_subparsers(dest="command", required=True)
    spread = commands.add_parser("spread", help="each counted job's min, median, max and spread, and each job-run's wall")
    spread.add_argument("--runs", type=run_id, nargs="+", required=True, metavar="RUN_ID")
    factors = commands.add_parser("factors", help="each job-run's factors, and the decision rule over them")
    factors.add_argument("--runs", type=run_id, nargs="+", required=True, metavar="RUN_ID")
    rule = commands.add_parser("rule", help="the decision rule over a table in a file")
    rule.add_argument("--table", required=True, metavar="FILE")
    rule.add_argument("--method", choices=tuple(METHODS), default="spearman")
    packages = commands.add_parser("postgres-packages", help="the package directories whose tests use Postgres")
    packages.add_argument("--root", default=str(DEFAULT_PACKAGES_ROOT), metavar="DIR")
    # A later round adds here `cpu --run <id>` and `coverage --run <id>`, which read the CPU rows
    # in the `cpu/` directory of each job's artifact `go-test-events-<job id>`.
    return parser.parse_args(argv)


COMMANDS = {
    "spread": spread_command,
    "factors": factors_command,
    "rule": rule_command,
    "postgres-packages": packages_command,
}


def main(argv: list[str]) -> int:
    args = parse(argv)
    try:
        return COMMANDS[args.command](args)
    except Failure as failure:
        print(f"go_cpu_record.py {args.command}: {failure}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
