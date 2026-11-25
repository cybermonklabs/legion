#!/usr/bin/env python3
"""The packages one leg of a Go job runs, read from .github/go-legs.yaml, after a check that the
file runs every package of the module exactly once.

    go_legs.py <module> <job> <leg> <legs>

prints on its standard output, one per line and nothing else, the packages .github/go-legs.yaml
lists for leg <leg> of job <job> under <module> (a directory of the repository,
`packages/daemon`), each as `./<directory>/`, in the file's order, which is the order go test
starts them in. <legs> is how many legs the job's matrix runs, `${{ strategy.job-total }}`. A
leg's go test step pipes the packages to xargs:

    python3 ../../.github/scripts/go_legs.py packages/daemon daemon-go ${{ matrix.leg }} ${{ strategy.job-total }} | xargs go test …

Before it prints anything it runs `go list ./...` in <module> and compares what that prints with
the whole of the module's entry, every leg of every job and `elsewhere`, the packages a job runs
outside the legs. It exits 1, naming each, when a package go list prints is in no place of the
file, when a package is in more than one place, when a place names a directory go list does not
print as a package, when a leg lists no package (xargs would then run go test on the module's
root), and when the job's legs in the file are not exactly legs 1 to <legs>, the legs its matrix
runs (a leg the file has and the matrix does not would run in no job). So a leg never runs while
a package of its module runs in two places or in no place, or in a leg its own job's matrix does
not launch, whichever leg it is: a new package fails every leg until the file names it. A leg
sees only its own job, so a package filed under a job key no matrix job runs is ci_job_reads.py's
to refuse (hold_daemon_legs, which the changes job imports). It exits 2 on a usage error, and when
the file has no such module,
job or leg. Everything but the packages goes to standard error, the problems as GitHub error
annotations on the file, so nothing else reaches the go test the packages are piped to.

`go list ./...` decides what a package is, as it decides what `go test ./...` would run: a
directory whose every .go file a build constraint excludes is none, and one with only _test.go
files is one. The packages are compared by directory under the module, read from go list's
`{{.Dir}}`.

The repository is the one this script sits in, so a copy of it in a fixture tree reads that
tree's file and module: go-legs.test.sh runs it so.
"""

import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
LEGS = ".github/go-legs.yaml"
ELSEWHERE = "elsewhere"


def module_entry(module: str, path: Path) -> dict:
    """The file's entry for `module`: each job to its legs ({leg number: [package, ...]}), and
    `elsewhere` to {package: the job that runs it}."""
    document = yaml.safe_load(path.read_text()) or {}
    entry = document.get(module)
    if not isinstance(entry, dict):
        raise LookupError(f"{path} has no entry for {module}")
    return entry


def places(entry: dict) -> list:
    """Every (package, place) the entry names, a place being `job leg N` or `elsewhere (job)`."""
    named = []
    for job, legs in entry.items():
        if job == ELSEWHERE:
            named += [(package, f"{ELSEWHERE} ({runner})") for package, runner in (legs or {}).items()]
            continue
        for leg, packages in (legs or {}).items():
            named += [(package, f"{job} leg {leg}") for package in packages or []]
    return named


def problems(module: str, entry: dict, packages: list, legs_path: str = LEGS) -> list:
    """What keeps the entry from running each of `packages` (directories under the module) in
    exactly one place: a package in no place, a package in two, and a place that names no
    package."""
    found = []
    where = {}
    for package, place in places(entry):
        where.setdefault(package, []).append(place)
    for package in packages:
        held = where.get(package, [])
        if not held:
            found.append(f"{module}/{package} is a package `go list ./...` prints, and no leg of {legs_path} "
                         f"runs it: add it to the leg of the job that should run it, or to {ELSEWHERE} with the "
                         f"job that runs it")
        elif len(held) > 1:
            found.append(f"{module}/{package} is in {len(held)} places of {legs_path} ({', '.join(held)}), so it "
                         f"would run in each: keep it in one")
    listed = set(packages)
    for package, held in where.items():
        if package not in listed:
            found.append(f"{legs_path} names {module}/{package} ({', '.join(held)}), which `go list ./...` does "
                         f"not print as a package: take it out, or follow the package where it went")
    return found


def go_packages(module: str) -> list:
    """The packages `go list ./...` prints in the module, as directories under it."""
    directory = ROOT / module
    listing = subprocess.run(["go", "list", "-f", "{{.Dir}}", "./..."], cwd=directory, capture_output=True,
                             text=True)
    if listing.returncode != 0:
        sys.stderr.write(listing.stderr)
        raise RuntimeError(f"`go list ./...` in {module} failed (its errors are above), so which packages the "
                           f"legs must run is unknown")
    found = []
    for line in listing.stdout.splitlines():
        relative = Path(line).resolve().relative_to(directory.resolve()).as_posix()
        found.append(relative)
    if not found:
        raise RuntimeError(f"`go list ./...` in {module} printed no package, so there is nothing to compare "
                           f"the legs with")
    return found


def leg_problems(job: str, legs: dict, launched: int, legs_path: str = LEGS) -> list:
    """What keeps a job's legs in the file from being the legs its matrix runs, 1 to `launched`,
    each listing at least one package."""
    found = []
    # The file's leg numbers in number order (a digit string's length first, then its text).
    numbers = sorted((str(number) for number in legs), key=lambda n: (len(n), n))
    if numbers != [str(number) for number in range(1, launched + 1)]:
        found.append(f"{legs_path} gives job {job} legs {', '.join(numbers)}, and its matrix runs legs 1 to "
                     f"{launched}: a leg the file has and the matrix does not runs in no job, so make the two "
                     f"the same")
    for number, packages in legs.items():
        if not packages:
            found.append(f"{legs_path} gives leg {number} of job {job} no package, and xargs would hand go test "
                         f"none, which tests the module's root: give the leg a package, or take it out of the file "
                         f"and the matrix")
    return found


def main(argv: list) -> int:
    if len(argv) != 4 or not argv[3].isdigit() or int(argv[3]) < 1:
        print("go_legs.py: usage: go_legs.py <module> <job> <leg> <legs>, <legs> a number above 0",
              file=sys.stderr)
        return 2
    module, job, leg, launched = argv[0], argv[1], argv[2], int(argv[3])
    path = ROOT / LEGS
    try:
        entry = module_entry(module, path)
    except LookupError as missing:
        print(f"go_legs.py: {missing}", file=sys.stderr)
        return 2
    legs = entry.get(job) if job != ELSEWHERE else None
    mine = next((packages for number, packages in (legs or {}).items() if str(number) == leg), None)
    if mine is None:
        print(f"go_legs.py: {LEGS} has no leg {leg} of job {job} under {module}", file=sys.stderr)
        return 2
    try:
        packages = go_packages(module)
    except RuntimeError as failed:
        print(f"go_legs.py: {failed}", file=sys.stderr)
        return 1
    found = leg_problems(job, legs, launched) + problems(module, entry, packages)
    if found:
        for problem in found:
            print(f"::error file={LEGS}::{problem}", file=sys.stderr)
        print(f"go_legs.py: {len(found)} problem(s) above; leg {leg} of {job} runs nothing until the file runs "
              f"every package of {module} once, in the legs the job's matrix runs", file=sys.stderr)
        return 1
    print(f"go_legs.py: leg {leg} of {job} runs {len(mine)} of the {len(packages)} packages of {module}",
          file=sys.stderr)
    for package in mine:
        print("./" if package == "." else f"./{package}/")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
