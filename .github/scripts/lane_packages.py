#!/usr/bin/env python3
"""The slow suites' own packages (.github/lane-packages.yaml), read two ways that share no matching.

A pull request runs a slow suite only under a label; the merge queue runs every slow suite the
change's paths select, and that is where completeness belongs. This script holds a merge group to
it at the level of each suite's own packages, by a reading of the change that is not `changes`'.

`filters <file>` writes one dorny/paths-filter filter per job of the table, named for its `changes`
output, which the changes job's `own` step evaluates over a merge group's (and a dispatched
branch's) change: each such output turns its suite on when the change touches one of the suite's
own packages, so a suite whose own package changed runs in the group even where the computed
filter, which reads build tags, leaves the file out (`.github/workflows/ci.yaml#own`).

`gate` is ci-ok's check that it did. On a merge_group run it reads the files the group changes out
of git (its head against its base_sha, fetched one commit deep when the checkout lacks it), maps
each file to the jobs that own it here, in Python, and fails when such a job did not succeed in this
run, by the job's own result in NEEDS (`toJSON(needs)`). It reads none of `changes`' outputs: a
`changes` that dropped a job (a broken own filter, output term or `if:`) would skip the job and, read
there, agree with itself, which is the fault this check exists to see. On any other run it says it
does not judge it: a pull request holds the slow suites back on purpose.

What a job owns (the table's header says it at length): the packages .github/go-legs.yaml places
under its go-legs keys, every .go file of a module no place there names when one of its keys is the
module's `unlisted` (outside the `nested` modules), the Go package directories it lists (each its
files but *.md, and its testdata/ tree), the globs it lists, and the script harnesses its workflow's
steps run with the files beside each whose name is the harness's stem.

.github/scripts/lane-packages.test.sh holds the two readings to the same answer over every file of
the repository, and the gate to its verdicts.
.github/ci-notes/ci.md#ci-ok-fails-a-merge-group-that-skipped-a-slow-suite-whose-own-package-it-touches
says why.

Usage: lane_packages.py filters <file>
       NEEDS=<toJSON(needs)> EVENT=<event> [MERGE_GROUP_BASE=<sha>] lane_packages.py gate
       lane_packages.py owners <path>...   (each path's owning jobs, one line each)
Run from anywhere; it reads the repository it sits in."""
import json
import os
import re
import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent.parent
TABLE = ".github/lane-packages.yaml"
LEGS = ".github/go-legs.yaml"
# A harness a step's run: names: `<dir>/<stem>.test.sh`, `<stem>_test.sh` or `<stem>.test.ts`.
HARNESS = re.compile(r"(?<![\w./-])([\w./-]+(?:\.test\.(?:sh|ts)|_test\.sh))(?![\w./-])")
HARNESS_SUFFIX = re.compile(r"(?:\.test\.(?:sh|ts)|_test\.sh)$")


class Lane:
    def __init__(self, job, output):
        self.job, self.output = job, output
        self.packages = set()   # package directories, repository-relative
        self.unlisted = []      # (module, the module's placed packages, nested dirs)
        self.paths = []         # globs, `!` ones excluding
        self.harnesses = []     # (dir, stem, the harness's own path)


def placed_packages(module_entry):
    """Every package a go-legs.yaml module entry names, with the keys that name it."""
    placed = {}
    for key, value in module_entry.items():
        if key == "unlisted":
            continue
        if key in ("elsewhere", "twice"):
            for package, owner in (value or {}).items():
                placed.setdefault(str(package), set()).add(str(owner) if key == "elsewhere" else "")
            continue
        for leg in (value or {}).values():
            for item in leg or []:
                package = item if isinstance(item, str) else next(iter(item))
                placed.setdefault(str(package), set()).add(str(key))
    return placed


def harnesses_of(workflow, job_id, root):
    """(dir, stem, path) of every script harness the steps of `job_id` in `workflow` run, each path
    read against its step's working-directory."""
    document = yaml.safe_load((root / workflow).read_text())
    job = (document.get("jobs") or {})[job_id]
    workdir = ((job.get("defaults") or {}).get("run") or {}).get("working-directory", "")
    found = set()
    for step in job.get("steps") or []:
        base = step.get("working-directory", workdir) or ""
        for path in HARNESS.findall(str(step.get("run", ""))):
            full = os.path.normpath(os.path.join(base, path))
            found.add((os.path.dirname(full), HARNESS_SUFFIX.sub("", os.path.basename(full)), full))
    if not found:
        raise ValueError(f"{workflow} job {job_id} runs no harness this reader finds")
    return sorted(found)


def load(root=ROOT):
    table = yaml.safe_load((root / TABLE).read_text())
    legs = yaml.safe_load((root / LEGS).read_text())
    nested = [str(n).rstrip("/") for n in table.get("nested") or []]
    lanes = {}
    for job, entry in (table.get("jobs") or {}).items():
        lane = Lane(str(job), str(entry["output"]))
        for module, keys in (entry.get("go") or {}).items():
            module_entry = legs[module]
            keys = {str(k) for k in keys}
            placed = placed_packages(module_entry)
            lane.packages |= {f"{module}/{p}" for p, owners in placed.items() if owners & keys}
            if str(module_entry.get("unlisted")) in keys:
                lane.unlisted.append((module, {f"{module}/{p}" for p in placed},
                                      [n for n in nested if n.startswith(module + "/")]))
        lane.packages |= {str(p).rstrip("/") for p in entry.get("packages") or []}
        lane.paths = [str(p) for p in entry.get("paths") or []]
        if entry.get("harnesses"):
            spec = entry["harnesses"]
            lane.harnesses = harnesses_of(spec["workflow"], spec["job"], root)
        lanes[lane.job] = lane
    return lanes


def stem_class(stem):
    """The stem as a glob, `-` and `_` read alike."""
    return re.sub(r"[-_]", "[-_]", stem)


# The gate's reading: a file's owners, decided here and not through any filter.

def package_of(path):
    """The package directory a file belongs to: a file of a testdata/ tree belongs to the package
    above it; a *.md file outside one to none."""
    directory, name = os.path.split(path)
    parts = directory.split("/") if directory else []
    if "testdata" in parts:
        return "/".join(parts[:parts.index("testdata")])
    return None if name.endswith(".md") else directory


def same_name(a, b):
    return a.replace("-", "_") == b.replace("-", "_")


def owns(lane, path, paths_filter):
    package = package_of(path)
    if package is not None and package in lane.packages:
        return True
    if path.endswith(".go") and package is not None:
        for module, placed, nested in lane.unlisted:
            if ((package == module or package.startswith(module + "/")) and package not in placed
                    and not any(package == n or package.startswith(n + "/") for n in nested)):
                return True
    if lane.paths and paths_filter(lane.paths).covers(path):
        return True
    directory, name = os.path.split(path)
    for hdir, stem, harness in lane.harnesses:
        if path == harness:
            return True
        if directory == hdir and name[len(stem):len(stem) + 1] == "." and same_name(name[:len(stem)], stem):
            return True
        rest = path[len(hdir) + 1:] if hdir and path.startswith(hdir + "/") else (path if not hdir else None)
        if rest is not None and "/" in rest and same_name(rest.split("/", 1)[0], stem):
            return True
    return False


def owners(lanes, path):
    from workflow_paths import PathsFilter
    cache = {}

    def paths_filter(patterns):
        key = tuple(patterns)
        if key not in cache:
            cache[key] = PathsFilter(list(patterns), "some-with-excludes")
        return cache[key]
    return sorted(job for job, lane in lanes.items() if owns(lane, path, paths_filter))


# The changes job's reading: patterns dorny/paths-filter evaluates under some-with-excludes.

def filters(lanes):
    out = {}
    for job, lane in lanes.items():
        positive, negative = [], []
        for package in sorted(lane.packages):
            positive += [f"{package}/*", f"{package}/testdata/**"]
            negative.append(f"!{package}/*.md")
        for module, placed, nested in lane.unlisted:
            positive.append(f"{module}/**/*.go")
            for package in sorted(placed - lane.packages):
                negative += [f"!{package}/*.go", f"!{package}/testdata/**"]
            negative += [f"!{n}/**" for n in nested]
        positive += [p for p in lane.paths if not p.startswith("!")]
        negative += [p for p in lane.paths if p.startswith("!")]
        for hdir, stem, harness in lane.harnesses:
            prefix = f"{hdir}/" if hdir else ""
            positive += [harness, f"{prefix}{stem_class(stem)}.*", f"{prefix}{stem_class(stem)}/**"]
        out[lane.output] = positive + negative
    return out


def git(*args):
    return subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True)


def changed_files():
    """The merge group's base, head and the files it changes, read out of git."""
    base = os.environ.get("MERGE_GROUP_BASE", "")
    if not re.fullmatch(r"[0-9a-f]{40}", base):
        raise ValueError(f"MERGE_GROUP_BASE is {base!r}, not the merge group's base commit")
    if git("cat-file", "-e", f"{base}^{{commit}}").returncode != 0:
        fetched = git("fetch", "--no-tags", "--depth=1", "origin", base)
        if fetched.returncode != 0:
            raise ValueError(f"could not fetch the merge group's base {base}: {fetched.stderr.strip()}")
    head = git("rev-parse", "HEAD").stdout.strip()
    diff = git("diff", "--no-renames", "--name-only", "-z", base, "HEAD")
    if diff.returncode != 0:
        raise ValueError(f"git diff {base} HEAD failed: {diff.stderr.strip()}")
    files = [f for f in diff.stdout.split("\0") if f]
    if not files:
        raise ValueError(f"git diff read no changed file between {base} and {head}, so which slow suites this "
                         f"group needs is UNKNOWN")
    return base, head, files


def gate():
    event = os.environ.get("EVENT", "")
    if event != "merge_group":
        print(f"lane check: not judged on a {event or 'unnamed'} run: a pull request holds the slow suites back, "
              f"and the merge group runs and judges them")
        return 0
    try:
        needs = json.loads(os.environ["NEEDS"])
        base, head, files = changed_files()
        lanes = load()
    except (KeyError, ValueError, json.JSONDecodeError, OSError, yaml.YAMLError) as refusal:
        print(f"::error::the lane check could not run: {refusal}")
        return 1
    needed = {}
    for path in files:
        for job in owners(lanes, path):
            needed.setdefault(job, []).append(path)
    print(f"lane check: {len(files)} files change between {base[:12]} and {head[:12]}; "
          f"{len(needed)} of {len(lanes)} slow suites own one of them")
    failed = 0
    for job in sorted(needed):
        result = (needs.get(job) or {}).get("result", "absent")
        touched = needed[job]
        shown = ", ".join(touched[:3]) + (f" and {len(touched) - 3} more" if len(touched) > 3 else "")
        if result == "success":
            print(f"{job}: success, and this change touches its own packages ({shown})")
            continue
        failed += 1
        print(f"::error::{job} is {result} in this merge group, and the group changes its own packages ({shown}): "
              f"a merge group runs every slow suite whose own package it changes, so the changes job or {job}'s "
              f"if: dropped it; read needs.changes.outputs.{lanes[job].output} and the changes job's own-packages "
              f"filter step")
    return 1 if failed else 0


def main(argv):
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    if len(argv) == 3 and argv[1] == "filters":
        Path(argv[2]).write_text(yaml.safe_dump(filters(load()), sort_keys=False, width=1000))
        return 0
    if len(argv) == 2 and argv[1] == "gate":
        return gate()
    if len(argv) >= 3 and argv[1] == "owners":
        lanes = load()
        for path in argv[2:]:
            print(f"{path}: {' '.join(owners(lanes, path))}")
        return 0
    print(__doc__.split("Usage:")[1].split("Run from")[0].strip(), file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
