#!/usr/bin/env python3
"""Which bun tests a CI run's jobs ran, from the records they uploaded.

Every bun test run of ci.yaml's run leaves the record .github/scripts/bun_test_record.py
describes, and each job uploads its records as the artifact `bun-test-reports-<the job's check
run id>` in its step "Keep the bun test reports". This reads them back:

  bun_test_reports.py <run id> [--store DIR]
  bun_test_reports.py --store DIR --offline

It prints one JSON object per line, one per test:

  {"job_id", "job", "attempt", "report", "file", "name", "status", "seconds", "line"}

`attempt` is the run attempt that ran the job: a later attempt lists each job it did not re-run
again, under a new id and with no artifact, and that copy is read as the job it copies. `file` is
the test's file from the repository's root (the report's directory before the file bun names);
`name` is the test's describe blocks and its own name joined by " > ", as bun's console prints it;
`status` is pass, fail, skip, todo or unselected; `seconds` is bun's own time for the test. bun
writes a test that a run's `-t` pattern does not select as skipped, as it writes a test.skip, so a
skipped test whose name (its describe blocks and its own, joined by spaces, as bun matches them)
the run's pattern does not find is `unselected`: it did not run here, and it was not skipped in
its code.

Where a record is not whole it prints an `unknown` line instead of keeping quiet, and exits 1.
These are every such line:

  {"job_id", "job", "attempt", "report", "unknown"}, for one run of a job, `report` naming it:
    - one line for each clause of bun_test_record.py's rule the run breaks (`clause N: …`), but
      clause 4, which gives one for each failed entry, beside the tests its report holds.
  {"job_id", "job", "attempt", "report": null, "unknown"}, for a job's whole record:
    - the job ended cancelled or timed out (beside the runs it has): its steps after the cancel
      did not run;
    - its artifact is missing or expired, or, with --offline, the store holds none for it;
    - its artifact could not be downloaded or unpacked, or is larger than ARTIFACT_LIMIT, as GitHub
      lists it or as its members unpack (a run's own tests can write into it);
    - its artifact is not unpacked because it holds more than MEMBER_LIMIT zip members;
    - its artifact holds no run.
  {"run", "unknown"}, `run` null with --offline:
    - the run's job list or artifact list could not be read, or gh could not start;
    - with --offline, the store holds no jobs.json, or one this reader did not write;
    - an artifact of the run (or a directory of the store) is named for no job that has a step
      named "Keep the bun test reports".

A job with no step of that name runs no bun test (rule 8 of check-workflow-files.sh holds every
job that does to one), and is not listed.

--store DIR keeps each artifact it downloads, unpacked, under DIR/<artifact name>/, with the run's
job list as DIR/jobs.json, and reads a kept one before it downloads, so a second read of a run
costs no download. --offline reads DIR alone. Its tests are in bun_test_reports.test.sh.
"""
import argparse
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from bun_test_record import PREFIX, UPLOAD_STEP, read_run, runs  # noqa: E402

# A job that ended so: its steps after the cancel did not run.
CUT_SHORT = ("cancelled", "timed_out")
# An artifact is written by the run's own tests too, so one above this is refused by name, never
# downloaded or unpacked in part: the largest of the 17 artifacts of run 46075808979, dispatch-ui's,
# is 415,736 bytes.
ARTIFACT_LIMIT = 64 * 1024 * 1024
# The most members one of those 17 artifacts holds is 38, envoy-session-stall's.
MEMBER_LIMIT = 10_000


def gh(repo: str, path: str, paginate: bool = False):
    """The answer to one REST read, parsed; with `paginate`, every page of the answer, in order, as
    one list (`gh api --paginate --slurp`)."""
    args = ["gh", "api", *(["--paginate", "--slurp"] if paginate else []), f"repos/{repo}/{path}"]
    return json.loads(subprocess.run(args, check=True, capture_output=True, text=True).stdout)


def run_jobs(repo: str, run: str) -> list:
    """Every job of every attempt of the run, with the attempt that ran it and its steps' names and
    conclusions, one attempt at a time and 50 jobs a page: for run 45993859209's second attempt
    (2026-11-27), pages of 100 jobs, with or without filter=all, answered HTTP 502 on every try,
    and pages of 50 answered."""
    attempts = gh(repo, f"actions/runs/{run}")["run_attempt"]
    jobs = {}
    for attempt in range(1, attempts + 1):
        for page in gh(repo, f"actions/runs/{run}/attempts/{attempt}/jobs?per_page=50", paginate=True):
            for j in page["jobs"]:
                # A later attempt lists the jobs it did not re-run again, under new ids but with
                # their first run's start and end (lint 140488599221 and 140499801160 of run
                # 45993859209); the artifact is the first id's, so the copy is folded into it.
                jobs.setdefault((j["name"], j["started_at"], j["completed_at"]), {**j, "attempt": attempt})
    return [{"id": j["id"], "name": j["name"], "conclusion": j["conclusion"], "attempt": j["attempt"],
             "steps": [{"name": s["name"], "conclusion": s["conclusion"]} for s in j["steps"]]}
            for j in jobs.values()]


def load_jobs(path: Path) -> list:
    """The job list --store kept, held to the shape run_jobs writes: a store of another reader, or
    of this one before it kept the attempt, is refused rather than read with a value it lacks."""
    jobs = json.loads(path.read_text())
    if not isinstance(jobs, list):
        raise ValueError("it is not a list")
    for job in jobs:
        if not (isinstance(job.get("id"), int) and isinstance(job.get("name"), str)
                and isinstance(job.get("attempt"), int) and "conclusion" in job
                and all(isinstance(s.get("name"), str) and "conclusion" in s for s in job["steps"])):
            raise ValueError(f"the job {json.dumps(job)[:200]} lacks its id, name, conclusion, attempt or steps")
    return jobs


def run_artifacts(repo: str, run: str) -> dict:
    return {a["name"]: a for page in gh(repo, f"actions/runs/{run}/artifacts?per_page=100", paginate=True)
            for a in page["artifacts"] if a["name"].startswith(PREFIX)}


def download(repo: str, artifact: dict, into: Path):
    """Unpacks the artifact into `into`, or gives the reason it could not. Its size as GitHub lists
    it, and the size its members unpack to, are each held to ARTIFACT_LIMIT. The zip's member
    count must be at most MEMBER_LIMIT before any member is unpacked."""
    bound = f"the bound of {ARTIFACT_LIMIT:,} bytes"
    listed = artifact.get("size_in_bytes")
    if not isinstance(listed, int):
        return f"is not downloaded: the run lists no size for it ({listed!r}), so it cannot be held to {bound}"
    if listed > ARTIFACT_LIMIT:
        return f"is not downloaded: the run lists it at {listed:,} bytes, above {bound}"
    try:
        data = subprocess.run(["gh", "api", f"repos/{repo}/actions/artifacts/{artifact['id']}/zip"],
                              check=True, capture_output=True).stdout
    except subprocess.CalledProcessError as error:
        return f"could not be downloaded: {error.stderr.decode(errors='replace').strip() or error}"
    except OSError as error:
        return f"could not be downloaded: gh could not start: {error}"
    # A download cut short leaves its .partial directory behind; the next one starts it again.
    partial = into.with_name(into.name + ".partial")
    shutil.rmtree(partial, ignore_errors=True)
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as zipped:
            members = zipped.infolist()
            if len(members) > MEMBER_LIMIT:
                return f"is not unpacked: it holds {len(members):,} members, above the bound of {MEMBER_LIMIT:,}"
            unpacked = sum(member.file_size for member in members)
            if unpacked > ARTIFACT_LIMIT:
                return f"is not unpacked: its members unpack to {unpacked:,} bytes, above {bound}"
            zipped.extractall(partial)
    except (zipfile.BadZipFile, OSError) as error:
        return f"could not be unpacked: {error}"
    partial.rename(into)
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("run", nargs="?")
    parser.add_argument("--repo", default=os.environ.get("GH_REPO", "cybermonklabs/legion"))
    parser.add_argument("--store")
    parser.add_argument("--offline", action="store_true")
    args = parser.parse_args()
    if args.offline and not args.store or not args.offline and not args.run:
        parser.error("give a run id, or --store DIR with --offline")
    # Without --store, the artifacts are kept for this read alone.
    tmp = None if args.store else tempfile.TemporaryDirectory()
    store = Path(args.store or tmp.name)
    store.mkdir(parents=True, exist_ok=True)

    def run_unknown(why: str) -> int:
        # Without the job list no job's record can be judged, so the whole run is UNKNOWN.
        print(json.dumps({"run": args.run, "unknown": why}))
        print("bun_test_reports: the run is UNKNOWN", file=sys.stderr)
        return 1

    if args.offline:
        try:
            jobs = load_jobs(store / "jobs.json")
        except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
            return run_unknown(f"the store {store} holds no job list this reader wrote: jobs.json: {error}")
        artifacts = None
    else:
        try:
            jobs = run_jobs(args.repo, args.run)
            artifacts = run_artifacts(args.repo, args.run)
        except subprocess.CalledProcessError as error:
            return run_unknown("the run's jobs or artifacts could not be read: "
                               f"{(error.stderr or '').strip() or error}")
        except OSError as error:
            return run_unknown(f"the run's jobs or artifacts could not be read: gh could not start: {error}")
        except (ValueError, KeyError, TypeError) as error:
            return run_unknown(f"the run's jobs or artifacts could not be read: {type(error).__name__}: {error}")
        (store / "jobs.json").write_text(json.dumps(jobs))

    unknown = 0
    read = set()
    for job in jobs:
        upload = [s for s in job["steps"] if s["name"] == UPLOAD_STEP]
        if not upload:
            continue
        head = {"job_id": job["id"], "job": job["name"], "attempt": job["attempt"]}

        def job_unknown(why: str, report=None) -> None:
            nonlocal unknown
            print(json.dumps({**head, "report": report, "unknown": why}))
            unknown += 1

        if job["conclusion"] in CUT_SHORT:
            job_unknown(f"the job ended {job['conclusion']}: its bun test steps after the cancel did not run, and a "
                        "run the cancel cut short is unknown on its own line")
        name = f"{PREFIX}{job['id']}"
        read.add(name)
        where = store / name
        if not where.is_dir():
            if artifacts is None:
                job_unknown(f"the store {store} holds no {name}; its upload step concluded {upload[0]['conclusion']}")
                continue
            artifact = artifacts.get(name)
            if artifact is None or artifact["expired"]:
                why = "expired" if artifact else "missing (the run lists no such artifact)"
                job_unknown(f"the artifact {name} is {why}; its upload step concluded {upload[0]['conclusion']}")
                continue
            why = download(args.repo, artifact, where)
            if why:
                job_unknown(f"the artifact {name} {why}")
                continue
        found = runs(where)
        if not found:
            job_unknown(f"the artifact {name} holds no run: no report and no run marker")
            continue
        for path in found:
            report = path.relative_to(where).as_posix()
            rows, reasons = read_run(path, report)
            for row in rows:
                print(json.dumps({**head, "report": report, **row}))
            for reason in reasons:
                job_unknown(reason, report)
    # A job's artifact is found by the job's upload step, so an artifact of a job whose step has
    # another name, or of no job the run lists, would be passed over; it is named instead.
    named = set(artifacts or ()) | {p.name for p in store.iterdir() if p.is_dir() and p.name.startswith(PREFIX)
                                    and not p.name.endswith(".partial")}
    for name in sorted(named - read):
        print(json.dumps({"run": args.run, "unknown": f"the artifact {name} is named for no job of the run with a "
                                                      f"step named {UPLOAD_STEP!r}, so its reports are not read"}))
        unknown += 1
    if unknown:
        print(f"bun_test_reports: {unknown} record(s) UNKNOWN", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
