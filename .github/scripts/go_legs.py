#!/usr/bin/env python3
"""The packages, and for a split package the tests, one leg of a Go job runs.

    go_legs.py <module> <job> <leg> <legs> [<build flag>...]

prints on its standard output, one line each and nothing else, what the leg's `go test` is to be
given: the packages .github/go-legs.yaml lists for leg <leg> of job <job> under <module> (a
directory of the repository, `packages/daemon`), each as `./<directory>/`, in the file's order,
which is the order go test starts them in; or, for a leg that runs a group of one package's tests,
that group's `-run=^(<name>|…)$` and then the package. <legs> is how many legs the job's matrix
runs, `${{ strategy.job-total }}`; the build flags are the leg's own `go test`'s (`-race`), under
which a split package's tests are listed, so the listing is the build the run then reuses. A leg's
go test step pipes what this prints to xargs:

    python3 ../../.github/scripts/go_legs.py packages/daemon daemon-go ${{ matrix.leg }} ${{ strategy.job-total }} | xargs go test …

The rules the legs and the groups keep, and what is refused as an error and what passes with a
warning, are in .github/ci-notes/ci-job-daemon-go.md#why-the-daemons-tests-run-in-legs and
#why-internaldaemons-tests-run-in-three-legs. In short: every package of the module in exactly one
place of the file, every top-level test of a split package in exactly one group, and a leg that
runs a group runs that package alone. A problem that would run a test twice, or nothing at all,
is an error and the leg prints nothing; a list that has merely gone stale against the source — a
test no group names, a group name the package no longer declares — is a warning, and the leg
places that test itself, by the same rule in every leg.

`go list ./...` decides what a package is, as it decides what `go test ./...` would run: a
directory whose every .go file a build constraint excludes is none, and one with only _test.go
files is one. The packages are compared by directory under the module, read from go list's
`{{.Dir}}`. `go test -list .` decides what a test is, under the same build flags as the run, so a
test behind a build tag the leg does not pass is in neither the listing nor the run.

Everything but the lines for xargs goes to standard error, the problems as GitHub error and
warning annotations on the file they are about, so nothing else reaches the go test.

It exits 1 on a problem that is an error, naming each, and 2 on a usage error and when the file
has no such module, job or leg.

The repository is the one this script sits in, so a copy of it in a fixture tree reads that
tree's file and module: go-legs.test.sh runs it so.
"""

import re
import subprocess
import sys
import zlib
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
LEGS = ".github/go-legs.yaml"
ELSEWHERE = "elsewhere"
# Where a split package's groups are: GROUPS/<module>/<package>.yaml.
GROUPS = ".github/go-legs"
# A name `go test -run` matches at the top level: what `go test -list .` prints for each test,
# fuzz test and example, less the benchmarks, which no -run without -bench runs. `\w` is Unicode
# in Python 3, as a Go identifier is: `go test` runs a `func TestÄ…` and lists it, and a name this
# pattern dropped would be in no group and in no leg's -run, with nothing to say so.
TEST_NAME = re.compile(r"^(?:Test|Fuzz|Example)\w*$")
# The longest -run pattern a group may print. go test hands the pattern to the test binary as one
# argument, `-test.run=<pattern>`, and Linux refuses one argument over 131,072 bytes
# (MAX_ARG_STRLEN); GNU xargs builds a command line of at most 128 KiB by default and, past it,
# starts a second go test with the arguments that did not fit, which here would be the package with
# no -run. This bound leaves the step's own flags their room under both.
PATTERN_LIMIT = 100_000


class Place:
    """One place .github/go-legs.yaml names a package: a leg of a job, with the group of that
    package's tests the leg runs where it runs a group, or `elsewhere` with the job that runs it
    outside the legs."""

    def __init__(self, package: str, job: str, leg=None, group=None, runner=None):
        self.package, self.job, self.leg, self.group, self.runner = package, job, leg, group, runner

    def shown(self) -> str:
        if self.job == ELSEWHERE:
            return f"{ELSEWHERE} ({self.runner})"
        shown = f"{self.job} leg {self.leg}"
        return shown if self.group is None else f"{shown}, group {self.group}"


def module_entry(module: str, path: Path) -> dict:
    """The file's entry for `module`: each job to its legs ({leg number: [entry, ...]}, an entry
    being a package or a `{package: group}` mapping), and `elsewhere` to {package: the job that
    runs it}."""
    document = yaml.safe_load(path.read_text()) or {}
    entry = document.get(module)
    if not isinstance(entry, dict):
        raise LookupError(f"{path} has no entry for {module}")
    return entry


def item(listed) -> tuple:
    """A leg's entry as (package, group): `- <package>` is (package, None), the whole package, and
    `- <package>: <group>` is (package, group), the group of its tests that leg runs. An entry of
    neither shape is (None, None), which places() reports once."""
    if isinstance(listed, str):
        return listed, None
    if isinstance(listed, dict) and len(listed) == 1:
        (package, group), = listed.items()
        if isinstance(package, str) and isinstance(group, int) and not isinstance(group, bool):
            return package, group
    return None, None


def job_packages(legs: dict) -> list:
    """Each package a job's legs run, once, in the file's order: a package several legs run a
    group of the tests of is one package, which those legs together run once."""
    found = []
    for packages in (legs or {}).values():
        for listed in packages or []:
            package, _ = item(listed)
            if package is not None and package not in found:
                found.append(package)
    return found


def groups_path(module: str, package: str) -> str:
    return f"{GROUPS}/{module}/{package}.yaml"


def read_groups(module: str, package: str) -> dict:
    """A split package's groups file, each group number to its test names. LookupError when the
    file is missing or is not that shape."""
    path = groups_path(module, package)
    try:
        document = yaml.safe_load((ROOT / path).read_text())
    except FileNotFoundError:
        raise LookupError(f"{path}, which holds the groups of {module}/{package}'s tests that its legs run, is "
                          f"not there: write it, each group number to the names of the tests that group runs")
    if not isinstance(document, dict) or not all(
            isinstance(group, int) and isinstance(names, list) and all(isinstance(name, str) for name in names)
            for group, names in document.items()):
        raise LookupError(f"{path} is not a map of each group number to a list of test names")
    return document


def places(entry: dict) -> tuple:
    """Every Place the entry names, and the entries of no shape `item` reads, each as
    (job, leg, the entry as written)."""
    named, malformed = [], []
    for job, legs in entry.items():
        if job == ELSEWHERE:
            named += [Place(package, ELSEWHERE, runner=runner) for package, runner in (legs or {}).items()]
            continue
        for leg, listed in (legs or {}).items():
            for written in listed or []:
                package, group = item(written)
                if package is None:
                    malformed.append((job, leg, written))
                else:
                    named.append(Place(package, job, leg=leg, group=group))
    return named, malformed


def once(expected: list, where: dict) -> tuple:
    """What keeps `where` from holding each of `expected` exactly once: (missing, twice, unknown)
    - the expected names no key holds, those a key holds more than once, and the keys that are not
    expected. `where` maps a name to the places that hold it."""
    held = set(expected)
    missing = [name for name in expected if not where.get(name)]
    twice = [name for name in expected if len(where.get(name, [])) > 1]
    unknown = [name for name in where if name not in held]
    return missing, twice, unknown


def package_argument(package: str) -> str:
    """A package as a go command names it, from the module's root."""
    return "./" if package == "." else f"./{package}/"


def split_problems(module: str, package: str, held: list, legs_path: str) -> list:
    """What keeps a package named in several places from being one package split by test across
    legs of one job: each place a group of it, the places all in one job, and each group in one
    leg."""
    shown = ", ".join(place.shown() for place in held)
    if any(place.group is None for place in held) or len({place.job for place in held}) > 1:
        return [f"{module}/{package} is in {len(held)} places of {legs_path} ({shown}), so it would run in each: "
                f"keep it in one, or split its tests into groups, each place `- {package}: <group>` in a leg of "
                f"one job"]
    by_group = {}
    for place in held:
        by_group.setdefault(place.group, []).append(place)
    _, twice, _ = once(sorted(by_group), by_group)
    return [f"group {group} of {module}/{package}'s tests is in {len(by_group[group])} legs of {legs_path} "
            f"({shown}), so it would run in each: keep it in one" for group in twice]


def problems(module: str, entry: dict, packages: list, legs_path: str = LEGS) -> list:
    """What keeps the entry from running each of `packages` (directories under the module) in
    exactly one place: a package in no place, a package in two (unless the two are groups of its
    tests, split_problems), a place that names no package, and an entry of no shape this script
    reads."""
    named, malformed = places(entry)
    found = [f"{legs_path} gives job {job} leg {leg} the entry {written!r}, which is neither "
             f"`- <package>` nor `- <package>: <group>`: write it as one of the two"
             for job, leg, written in malformed]
    where = {}
    for place in named:
        where.setdefault(place.package, []).append(place)
    missing, _, unknown = once(packages, where)
    found += [f"{module}/{package} is a package `go list ./...` prints, and no leg of {legs_path} "
              f"runs it: add it to the leg of the job that should run it, or to {ELSEWHERE} with the "
              f"job that runs it" for package in missing]
    for package in packages:
        held = where.get(package, [])
        if len(held) > 1 or (held and held[0].group is not None):
            found += split_problems(module, package, held, legs_path)
    found += [f"{legs_path} names {module}/{package} ({', '.join(place.shown() for place in where[package])}), "
              f"which `go list ./...` does not print as a package: take it out, or follow the package where it "
              f"went" for package in unknown]
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


def go_tests(module: str, package: str, flags: list) -> list:
    """The top-level tests `go test -list .` prints for the package under the leg's build flags:
    each name -run matches, a benchmark's apart."""
    target = package_argument(package)
    listing = subprocess.run(["go", "test", "-list", ".", *flags, target], cwd=ROOT / module,
                             capture_output=True, text=True)
    if listing.returncode != 0:
        sys.stderr.write(listing.stdout + listing.stderr)
        raise RuntimeError(f"`go test -list . {' '.join(flags + [target])}` in {module} failed (its output is "
                           f"above), so which tests the groups must run is unknown")
    found = [line for line in listing.stdout.splitlines() if TEST_NAME.match(line)]
    if not found:
        raise RuntimeError(f"`go test -list . {' '.join(flags + [target])}` in {module} printed no test, so "
                           f"there is nothing to compare the groups with")
    return found


def pattern(names: list) -> str:
    """A group's -run pattern: its names as one anchored alternation, which matches each name
    whole at the top level and runs that test's subtests with it."""
    return f"^({'|'.join(names)})$"


def placed(name: str, groups: list) -> int:
    """The group a test no group names runs in: the same group in every leg, from the name alone,
    so the legs agree without the file being edited. A rebalance moves it where its time belongs
    and the warning stops."""
    return sorted(groups)[zlib.crc32(name.encode()) % len(groups)]


def group_problems(module: str, package: str, groups: dict, tests: list) -> tuple:
    """The groups each of `tests` (the package's top-level tests) runs in, with what is wrong:
    (running, errors, warnings).

    `running` is each group number to the names its leg runs, the tests no group names placed by
    `placed`. An error is a reading under which a test would run twice or a leg would run nothing:
    a test in two groups, an empty group, a pattern too long to hand go test. A warning is a list
    that has gone stale against the source, which changes nothing about what runs: a test no group
    names, placed here, and a group name the package no longer declares, which matches nothing."""
    path = groups_path(module, package)
    errors, warnings = [], []
    where = {}
    for group, names in groups.items():
        for name in names:
            where.setdefault(name, []).append(group)
    missing, twice, unknown = once(tests, where)
    listed = set(tests)
    running = {group: [name for name in names if name in listed] for group, names in groups.items()}
    for name in missing:
        running[placed(name, list(groups))].append(name)
    errors += [f"{name} is in groups {', '.join(map(str, where[name]))} of {path}, so it would run in each: "
               f"keep it in one" for name in twice]
    # A group runs nothing when the file gives it no test, and equally when every name it gives has
    # gone from the package and no placement fell to it: its leg would run `-run=^()$`, which
    # matches nothing, and pass. The test is therefore on what the leg would really run.
    errors += [f"{path} gives group {group} no test the package has, so its leg would run none: give it "
               f"tests, or take the group out of the file and out of {LEGS}"
               for group, names in sorted(running.items()) if not names]
    warnings += [f"{name} is a test of {module}/{package} (`go test -list .`) that no group of {path} names, so "
                 f"group {placed(name, list(groups))} runs it, in this leg and in every other: add it to a group "
                 f"to choose where, or rebalance the file with .github/scripts/go_legs_groups.py"
                 for name in missing]
    warnings += [f"group {', '.join(map(str, where[name]))} of {path} names {name}, which `go test -list .` does "
                 f"not print as a test of {module}/{package}, so it runs nothing: take it out, or follow the test "
                 f"where it went" for name in unknown]
    for group, names in sorted(running.items()):
        if names and len(pattern(names)) > PATTERN_LIMIT:
            errors.append(f"group {group} of {path} makes a -run pattern of {len(pattern(names)):,} bytes, over "
                          f"the {PATTERN_LIMIT:,} go test and xargs can take as one argument: split the package "
                          f"into more groups")
    return running, errors, warnings


def leg_problems(job: str, legs: dict, launched: int, legs_path: str = LEGS) -> list:
    """What keeps a job's legs in the file from being the legs its matrix runs, 1 to `launched`,
    each listing at least one package, and a leg that runs a group of a package's tests from
    running anything else."""
    found = []
    # The file's leg numbers in number order (a digit string's length first, then its text).
    numbers = sorted((str(number) for number in legs), key=lambda n: (len(n), n))
    if numbers != [str(number) for number in range(1, launched + 1)]:
        found.append(f"{legs_path} gives job {job} legs {', '.join(numbers)}, and its matrix runs legs 1 to "
                     f"{launched}: a leg the file has and the matrix does not runs in no job, so make the two "
                     f"the same")
    for number, listed in legs.items():
        if not listed:
            found.append(f"{legs_path} gives leg {number} of job {job} no package, and xargs would hand go test "
                         f"none, which tests the module's root: give the leg a package, or take it out of the file "
                         f"and the matrix")
        elif len(listed) > 1 and any(item(written)[1] is not None for written in listed):
            found.append(f"{legs_path} gives leg {number} of job {job} a group of a package's tests beside other "
                         f"packages, and the group's -run would choose the tests of all of them: give the group "
                         f"a leg of its own")
    return found


def say(problems: list, kind: str, path: str) -> None:
    """Each problem as a GitHub annotation of its kind on the file it is about."""
    for problem in problems:
        print(f"::{kind} file={path}::{problem}", file=sys.stderr)


def run_group(module: str, job: str, leg: str, package: str, group: int, by_legs: list, flags: list) -> int:
    """A leg that runs one group of a package's tests: its -run and its package, once the file's
    groups are the groups the job's legs run (`by_legs`) and the groups and the package's own
    listing agree on which test runs where."""
    path = groups_path(module, package)
    try:
        groups = read_groups(module, package)
        tests = go_tests(module, package, flags)
    except (LookupError, RuntimeError) as failed:
        print(f"go_legs.py: {failed}", file=sys.stderr)
        return 1
    if sorted(by_legs) != sorted(groups):
        print(f"::error file={LEGS}::the legs of {LEGS} run groups {', '.join(map(str, sorted(by_legs)))} of "
              f"{module}/{package}'s tests, and {path} has groups {', '.join(map(str, sorted(groups)))}: a group "
              f"no leg runs runs in no job, and a leg whose group the file does not have runs nothing, so make "
              f"the two the same", file=sys.stderr)
        print(f"go_legs.py: 1 problem(s) above; leg {leg} of {job} runs nothing until the legs and {path} name "
              f"the same groups", file=sys.stderr)
        return 1
    running, errors, warnings = group_problems(module, package, groups, tests)
    say(warnings, "warning", path)
    if errors:
        say(errors, "error", path)
        print(f"go_legs.py: {len(errors)} problem(s) above; leg {leg} of {job} runs nothing until each test of "
              f"{module}/{package} is in one group of {path}", file=sys.stderr)
        return 1
    print(f"go_legs.py: leg {leg} of {job} runs group {group} of {module}/{package}'s tests, "
          f"{len(running[group])} of its {len(tests)} ({path})", file=sys.stderr)
    print(f"-run={pattern(running[group])}")
    print(package_argument(package))
    return 0


def main(argv: list) -> int:
    if len(argv) < 4 or not argv[3].isdigit() or int(argv[3]) < 1 or not all(f.startswith("-") for f in argv[4:]):
        print("go_legs.py: usage: go_legs.py <module> <job> <leg> <legs> [<build flag>...], <legs> a number "
              "above 0, each build flag one of the leg's go test's own (-race)", file=sys.stderr)
        return 2
    module, job, leg, launched, flags = argv[0], argv[1], argv[2], int(argv[3]), argv[4:]
    try:
        entry = module_entry(module, ROOT / LEGS)
    except LookupError as missing:
        print(f"go_legs.py: {missing}", file=sys.stderr)
        return 2
    legs = entry.get(job) if job != ELSEWHERE else None
    mine = next((listed for number, listed in (legs or {}).items() if str(number) == leg), None)
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
        say(found, "error", LEGS)
        print(f"go_legs.py: {len(found)} problem(s) above; leg {leg} of {job} runs nothing until the file runs "
              f"every package of {module} once, in the legs the job's matrix runs", file=sys.stderr)
        return 1
    mine = [item(written) for written in mine]
    split = [(package, group) for package, group in mine if group is not None]
    if split:
        package, group = split[0]
        by_legs = [place.group for place in places(entry)[0]
                   if place.job == job and place.package == package and place.group is not None]
        return run_group(module, job, leg, package, group, by_legs, flags)
    print(f"go_legs.py: leg {leg} of {job} runs {len(mine)} of the {len(packages)} packages of {module}",
          file=sys.stderr)
    for package, _ in mine:
        print(package_argument(package))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
