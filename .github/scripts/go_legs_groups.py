#!/usr/bin/env python3
"""Write a split package's test groups, .github/go-legs/<module>/<package>.yaml, balanced by the
tests' measured times.

    go_legs_groups.py <module> <package> <groups> <parallel> <list> <source> <run>...

<list> is the output of `go test -list . <the leg's build flags> ./<package>/` in <module>, every
top-level test the legs must run; each <run> is a `go test -json` run of the package, or the
verbose lines of one where a wrapper printed them. A test's time is the greatest `Elapsed` of the
top-level test and of its subtests, across all the runs given: Go stops a test's clock when its
function returns, before the parallel subtests it started run, so a test whose work is in
parallel subtests reads close to 0 s by its own clock. Whether it ran beside others is read from
its `pause` event (t.Parallel). <source> is the line the file's header gives as where the times
came from (the run and the job). Measure on the runner the legs run on: the groups are only as
even as the times are the legs' own.

<parallel> is the package's own -parallel, which its TestMain sets (internal/daemon 4, the
Dispatch API's 8): a leg's time is modelled as its serial tests' sum plus its parallel tests' sum
over that number, so each test weighs its time, or its time over <parallel> when it runs beside
others. The tests are dealt longest weight
first to the group whose modelled time is least so far (ties to the lower number). A test the
list prints and no run timed (a new test, or one the leg skips by name) weighs the median time,
as a parallel test. Each group's names are written sorted, each with its measured seconds, so a
new test goes in by name and two pull requests that each add one meet in different places.

The groups' totals, and the spread between the longest and the shortest, go to standard error.
"""

import json
import re
import statistics
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / ".github/scripts"))
import go_legs  # noqa: E402

# A verbose line a test binary prints for a test or a subtest, and the pause t.Parallel prints.
RESULT_LINE = re.compile(r"^\s*--- (?:PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)")
PAUSE_LINE = re.compile(r"^=== PAUSE (\S+)")


def timings(paths: list) -> tuple:
    """Each top-level test's time, the most over the runs, and the set that paused for t.Parallel.

    A test's time is its own `Elapsed`, or its longest subtest's where that is longer. Go stops a
    test's clock when its function returns, before the parallel subtests it started run, so a test
    whose work is in parallel subtests reads close to 0 s by its own Elapsed while it holds its leg
    for as long as those subtests take (a test running its rows on eight worlds read 0.0 s at
    #5634's first head). The longest subtest is a floor on that hold, not the sum: the subtests run
    beside each other.

    A run is read as a `go test -json` stream, or, where a file holds no JSON event of a test, as
    the verbose lines of that stream: a step's log under .github/scripts/go-test.sh, whose
    gotestsum prints each event's Output (its standard-verbose format), leaves those lines and
    gotestsum's summary after them, beside the lines race_memory.py writes in the document
    service's lane, and they carry each test's and subtest's own seconds and the pause."""
    seconds, parallel = {}, set()

    def took(name: str, elapsed: float) -> None:
        top = name.split("/", 1)[0]
        seconds[top] = max(seconds.get(top, 0.0), elapsed)

    for path in paths:
        text = Path(path).read_text().splitlines()
        events = [json.loads(line) for line in text if line.startswith("{")]
        if any("Test" in event for event in events):
            for event in events:
                name = event.get("Test")
                if not name:
                    continue
                if event.get("Action") == "pause" and "/" not in name:
                    parallel.add(name)
                elif event.get("Action") in ("pass", "fail", "skip"):
                    took(name, float(event.get("Elapsed", 0.0)))
            continue
        for line in text:
            result = RESULT_LINE.match(line)
            if result:
                took(result.group(1), float(result.group(2)))
            paused = PAUSE_LINE.match(line)
            if paused and "/" not in paused.group(1):
                parallel.add(paused.group(1))
        # A file of neither shape has timed nothing, and a balance from no time is no balance.
        if not seconds:
            raise SystemExit(f"go_legs_groups.py: {path} holds neither a go test -json event of a test nor a "
                             f"`--- PASS: <test> (<seconds>s)` line, so it times nothing")
    return seconds, {name for name in parallel if name in seconds}


def deal(names: list, seconds: dict, parallel: set, groups: int, slots: int) -> tuple:
    """The groups, each a list of names, and each group's modelled time."""
    median = statistics.median(seconds.values()) if seconds else 0.0

    def weight(name):
        taken = seconds.get(name, median)
        return taken / slots if name in parallel or name not in seconds else taken

    dealt = {group: [] for group in range(1, groups + 1)}
    load = {group: 0.0 for group in dealt}
    for name in sorted(names, key=lambda n: (-weight(n), n)):
        group = min(load, key=lambda g: (load[g], g))
        dealt[group].append(name)
        load[group] += weight(name)
    return dealt, load


def render(module: str, package: str, source: str, dealt: dict, load: dict, seconds: dict, slots: int) -> str:
    lines = [
        f"# The groups of {module}/{package}'s top-level tests, one per leg of .github/go-legs.yaml that",
        "# runs a share of the package (`- <package>: <group>`). What the legs and the groups must hold to,",
        "# and what is an error and what a warning, is in .github/ci-notes/ci-job-daemon-go.md, section",
        "# \"Why internal/daemon's tests run in three legs\"; .github/scripts/go_legs.py holds it, and prints",
        "# a leg's group as -run=^(<name>|...)$. A test no group names runs in the group that file's rule",
        "# gives it, the same in every leg, and this file is written again to put it where its time belongs:",
        "# .github/scripts/go_legs_groups.py, from a run of the package on the runner the legs run on.",
        f"# Times: {source}.",
        f"# Modelled leg time, serial tests' sum plus parallel tests' sum over {slots}: "
        + ", ".join(f"group {group} {load[group]:.0f} s" for group in dealt) + ".",
    ]
    for group, names in dealt.items():
        lines.append(f"{group}:")
        for name in sorted(names):
            taken = seconds.get(name)
            lines.append(f"  - {name}" + ("" if taken is None else f"  # {taken:.1f} s"))
    return "\n".join(lines) + "\n"


def main(argv: list) -> int:
    if len(argv) < 7 or not argv[2].isdigit() or int(argv[2]) < 2 or not argv[3].isdigit() or int(argv[3]) < 1:
        print("go_legs_groups.py: usage: go_legs_groups.py <module> <package> <groups> <parallel> <list> "
              "<source> <run>..., <groups> a number above 1 and <parallel> the package's own -parallel",
              file=sys.stderr)
        return 2
    module, package, groups, slots = argv[0], argv[1], int(argv[2]), int(argv[3])
    listing, source, runs = argv[4], argv[5], argv[6:]
    names = [line for line in Path(listing).read_text().splitlines() if go_legs.TEST_NAME.match(line)]
    seconds, parallel = timings(runs)
    dealt, load = deal(names, seconds, parallel, groups, slots)
    path = ROOT / go_legs.groups_path(module, package)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(render(module, package, source, dealt, load,
                           {n: seconds[n] for n in names if n in seconds}, slots))
    untimed = [name for name in names if name not in seconds]
    print(f"go_legs_groups.py: {len(names)} tests in {groups} groups, {len(untimed)} untimed; modelled "
          + ", ".join(f"{group}: {load[group]:.0f} s ({len(dealt[group])} tests)" for group in dealt)
          + f"; spread {max(load.values()) - min(load.values()):.1f} s", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
