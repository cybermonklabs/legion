#!/usr/bin/env python3
r"""Split one Go package's tests into shards by name, and prove that the shards' `-run` patterns
select every test of the package exactly once.

It exists for envoy-go-race-api. `go test -race ./internal/dispatch/api/`
took 913 s to 1,527 s of its 30-minute `-timeout` on one day's runners, so whether the job passed
depended on how fast the runner was. The job is now a matrix, and each leg runs one shard of the
package. A shard is a set of top-level test names, written as the anchored alternation
`go test -run` takes, `^(TestA|TestB|…)$`.

Usage:

    go-test-shards.py pattern <index> <total> [<build flag>...] <package>
    go-test-shards.py check [<build flag>...] <package> -- <pattern>...

`pattern` prints the pattern of shard <index> (0 to <total> - 1) and nothing else on standard
output, after it has run `check` over all <total> patterns. A leg's step assigns it on a line of
its own and passes it to `go test -run`, so under `bash -e` a refusal fails the step and never
runs `-run ""`, which matches every test:

    pattern=$(../../.github/scripts/go-test-shards.py pattern ${{ strategy.job-index }} ${{ strategy.job-total }} -race ./internal/dispatch/api/)
    go test -count=1 -race -timeout 30m -run "$pattern" ./internal/dispatch/api/

The leg's index and the leg count are the matrix's own (`strategy.job-index`,
`strategy.job-total`), so a leg added to the matrix changes the denominator with it, and no leg
can be left out of the split.

`check` proves a set of patterns, given one per shard, select each test exactly once. A test no
pattern selects runs in no leg, and a test two patterns select runs twice; it names each one.
`pattern` runs it over the patterns it made, in every leg, before the leg runs its tests.

A pattern has one shape, the one `pattern` makes: `^(Name|Name|…)$`, an alternation of names,
every one of them, byte for byte, a name the test binary listed (`-test.list .`). `check` fails any
other pattern, naming it and each name in it the binary did not list, before it lists anything,
because `-test.list` matches each top-level name against the whole pattern while `go test -run`
first splits the pattern at every `|` and `/` outside brackets and parentheses, rewrites each part
(testing's rewrite: a space becomes `_`, a character Go does not print becomes its escape) and
matches the parts one by one. Measured on Go 1.27 with TestAlpha, Test_Alpha and TestBeta:

    ^TestBeta$|^TestAlpha$/child          lists TestBeta, runs TestAlpha too: a `/` starts a test
                                          for its subtests
    ^TestBeta$|^\QTestAlpha|\Qjunk\E$     lists TestBeta, runs TestAlpha too: the split cuts the
                                          quote in two
    (?i)^TestAlpha$|^Testbeta$            lists TestAlpha and TestBeta, runs TestAlpha alone: `(?i)`
                                          covers its own part
    ^(Test<U+00A0>Alpha|TestBeta)$        lists TestBeta, runs Test_Alpha too: the no-break space
                                          is rewritten to `_`

The names a test binary lists are Go identifiers: no character of them is one Go's regexp gives a
meaning to, and none is one the rewrite changes. So a pattern made only of listed names, with every
`|` inside its parentheses, selects the same names under -list and under -run, whatever Python or
Unicode version reads it: the proof rests on membership, not on any character class.

Where the names come from. Both commands build the package's test binary with
`go test -c` under the build flags given (`-race`, `-tags T`), which are the flags the leg's own
`go test` uses, then ask the binary itself: `-test.list .` for every name, and `-test.list
<pattern>` for the names one pattern selects. The binary is the truth about which tests exist in
that build: the race build of internal/dispatch/api leaves out the files that carry
`//go:build !race`, and a reading of the source would have to agree with the go tool about every
constraint to know that. The patterns are matched by Go's own regexp in the binary, so `check`
proves what `go test -run` will do, not what a second engine thinks it would do. The build is the
one the leg's `go test` makes next, so that `go test` takes every compiled package from the build
cache and links once more; nothing is compiled twice. The binary lists in the package's own
directory, which `go list -f '{{.Dir}}'` names under the same flags, because go test runs a test
binary there: a TestMain, an init or a package-level variable that reads a path relative to the
package (`testdata/golden.txt`) reads the file go test gives it, where in the caller's directory
it would panic, and every leg would refuse a package go test passes.

Which shard a test is in. A test is in shard `crc32(name) mod total`, the IEEE CRC-32 that
Python's zlib and Go's hash/crc32 both compute. The assignment needs no list kept by hand: a new
test lands in exactly one shard the first time a leg runs, and a test stays in its shard until it
is renamed or the leg count changes. A list of measured durations would balance the shards more
closely, but it goes stale as tests are added and needs regenerating; the hash needs nothing.
The legs' balance is measured, not assumed (the comment on the matrix in
.github/workflows/ci-job-envoy-go-race-api.yaml gives the numbers).

The names in the split are every name `-test.list .` prints but benchmarks: `Test…`, `Fuzz…` and
`Example…`. `-test.list` prints an example only when it has an output comment, which is when
`go test` runs it, and `-run` never starts a benchmark. TestMain is never printed. A name the
package's internal and external test packages both declare (`package api` and
`package api_test`) is printed twice and is one name of the split: it is in one shard, and that
leg's `go test -run` runs both tests, as `go test` does without `-run`.

Refusals, each on standard error with nothing on standard output: a usage error, an index outside
the leg count, a build or listing that fails, a package with no test, a shard with no test (more
legs than the package needs), and a pattern longer than 131,060 bytes: the test binary takes it in
one argument after `-test.list=`, and Linux refuses an argument longer than 131,071 bytes
(MAX_ARG_STRLEN), so the binary could not be started with it; add a leg.

Exit status: 0 when every test is in exactly one shard, 1 when a test is in no shard or in two,
or a pattern is not of that one shape, 2 for every refusal above.

Run from the directory the package path is relative to, as `go test` is: the leg runs it in
packages/envoy. Needs python3 and go on PATH; `-race` needs cgo and a C compiler, as it does for
`go test`. Its tests are in go-test-shards.test.sh, which ci.yaml's test job runs.
"""

from __future__ import annotations

import contextlib
import re
import subprocess
import sys
import tempfile
import zlib
from pathlib import Path
from typing import NamedTuple

USAGE = """\
usage: go-test-shards.py pattern <index> <total> [<build flag>...] <package>
       go-test-shards.py check [<build flag>...] <package> -- <pattern>...
A <pattern> is ^(Name|Name|...)$ of names the test binary lists, the shape `pattern` makes: -list
and go test -run read any other differently."""

# A name `-test.list` prints that `-run` can start: a top-level test, fuzz target or example.
# Benchmarks, which it also prints, are left out (`-run` never starts one), and a line with a space
# in it (a TestMain that prints a sentence) names no test. After the prefix the name is any run of
# non-space characters, not a character class: the binary printed it, so Go has already decided it
# is a test, and Python's own Unicode tables are older than Go's (Python 3.12 has Unicode 15.0, Go
# 1.27 17.0), so `TestᲉ` (U+1C89, Unicode 16.0), which go test runs, failed `\w` and was in no
# shard while the proof, reading the same listing, passed. A TestMain that printed one word starting
# with Test while the binary lists would be read as a test too, and then trusted as a listed name;
# it would list under every pattern, and the proof fails such a word by name as a test in every
# shard. No package this script splits prints anything there (internal/dispatch/api's TestMain is
# storetest.Main, which prints only after the tests, to standard error).
SPLIT = re.compile(r"^(Test|Fuzz|Example)\S*$")
# The outer shape of a pattern: `^(` … `)$`, the whole argument (fullmatch, and `.` takes a line
# break too, so a trailing newline is no part of it). Its inside is cut at `|` and each part must
# be a listed name (prove() says why); a part holding `(`, `)` or anything else no test is named is
# then no listed name.
SHAPE = re.compile(r"\^\((.*)\)\$", re.DOTALL)
# Linux's MAX_ARG_STRLEN, 32 pages of 4 KiB: the longest single argument exec accepts, terminator
# included. The pattern reaches the test binary in one argument after a flag: LIST_FLAG here, and
# `-test.run=`, one byte shorter, when the leg's go test runs it. So LIST_FLAG and the pattern
# together must be shorter than this.
MAX_ARGUMENT = 131072
LIST_FLAG = "-test.list="


class Refused(Exception):
    """A refusal: exit 2, its message on standard error."""


class Uncovered(Exception):
    """Tests in no shard or in two: exit 1, one line per test on standard error."""


def shard_of(name: str, total: int) -> int:
    """The shard, 0 to total - 1, that a test of this name is in."""
    return zlib.crc32(name.encode()) % total


def alternation(names) -> str:
    """The -run pattern that selects exactly these names, sorted, each once."""
    return "^(" + "|".join(sorted(set(names))) + ")$"


class TestBinary(NamedTuple):
    """A package's test binary and the directory go test runs it in."""
    path: Path
    directory: Path


def build(flags: list[str], package: str, directory: Path) -> TestBinary:
    """The package's test binary, built under the flags into directory. go's standard error, which
    names each module as go downloads it and holds a failed build's error, goes to this script's as
    go writes it: the race leg's setup-go keeps no module cache, so this build is the step that
    downloads the modules, and a download that stalls must be named in the log while it waits.
    go's standard output (`[no test files]`) is kept back, since this script's own is the pattern."""
    binary = directory / "package.test"
    command = ["go", "test", "-c", "-o", str(binary), *flags, package]
    done = subprocess.run(command, stdout=subprocess.PIPE, encoding="utf-8", check=False)
    if done.returncode != 0:
        output = f", and its standard output was:\n{done.stdout.rstrip()}" if done.stdout.strip() else ""
        raise Refused(f"`{' '.join(command)}` exited {done.returncode}; its standard error is above{output}")
    if not binary.is_file():
        raise Refused(f"`{' '.join(command)}` wrote no test binary: {package} has no test files")
    return TestBinary(binary, package_directory(flags, package))


def package_directory(flags: list[str], package: str) -> Path:
    """The package's own directory, as go names it under the build flags, which is where go test
    runs the package's test binary. It runs after the build, which has downloaded what it needs."""
    command = ["go", "list", "-f", "{{.Dir}}", *flags, package]
    done = subprocess.run(command, stdout=subprocess.PIPE, encoding="utf-8", check=False)
    directories = done.stdout.splitlines()
    if done.returncode != 0 or len(directories) != 1:
        raise Refused(f"`{' '.join(command)}` exited {done.returncode} and named {len(directories)} "
                      f"directories, not the package's one; its standard error is above")
    return Path(directories[0])


def listed(binary: TestBinary, pattern: str) -> list[str]:
    """The names of the split the binary lists for one pattern, listed in the package's directory."""
    argument = LIST_FLAG + pattern
    if len(argument.encode()) >= MAX_ARGUMENT:
        raise Refused(f"a pattern of {len(pattern.encode())} bytes is longer than "
                      f"{MAX_ARGUMENT - 1 - len(LIST_FLAG)} bytes: the test binary takes it in one argument "
                      f"after `{LIST_FLAG}`, and one argument may be {MAX_ARGUMENT - 1} bytes (MAX_ARG_STRLEN), "
                      f"so the binary could not be started with it; add a leg to the matrix, which shortens "
                      f"every shard's pattern")
    # UTF-8 whatever the locale says, since a test's name may hold any letter Go allows.
    done = subprocess.run([str(binary.path), argument], cwd=binary.directory, capture_output=True,
                          encoding="utf-8", check=False)
    if done.returncode != 0:
        raise Refused(f"`{binary.path.name} -test.list` in {binary.directory} exited {done.returncode}:\n"
                      f"{done.stderr}{done.stdout}")
    return [line for line in done.stdout.splitlines() if SPLIT.match(line)]


@contextlib.contextmanager
def package_tests(flags: list[str], package: str, purpose: str):
    """The package's test binary, built under the flags, and every name of the split it lists,
    for as long as the block runs; refused when it lists none."""
    with tempfile.TemporaryDirectory(prefix="go-test-shards.") as directory:
        binary = build(flags, package, Path(directory))
        everything = listed(binary, ".")
        if not everything:
            raise Refused(f"{package} lists no test, so there is nothing to {purpose}")
        yield binary, everything


def shown(text: str) -> str:
    """The text with every character but printable ASCII written as \\uXXXX, so a refusal naming
    a pattern or a name is one line and tells a no-break space from a space."""
    return "".join(c if " " < c <= "~" else f"\\u{ord(c):04x}" if ord(c) <= 0xFFFF else f"\\U{ord(c):08x}"
                   for c in text)


def prove(binary: TestBinary, everything: list[str], patterns: list[str]) -> None:
    """Raise Uncovered naming each test of `everything` that no pattern, or more than one,
    selects in the binary; or, before it lists anything, each pattern that is not `^(` and `)$`
    around names the binary listed, `|` between them, whose selection -test.list cannot show (the
    module's head says why)."""
    total = len(patterns)
    known = set(everything)
    misshapen = []
    for number, pattern in enumerate(patterns, start=1):
        shape = SHAPE.fullmatch(pattern)
        if not shape:
            misshapen.append(f"pattern {number} of {total}, `{shown(pattern)}`, is not ^(Name|Name|...)$: go test "
                             f"-run splits a pattern at each `|` and `/` outside brackets and parentheses and "
                             f"-test.list does not, so for any other shape no listing can prove what a leg runs")
            continue
        unlisted = [name for name in shape.group(1).split("|") if name not in known]
        if unlisted:
            names = ", ".join(f"`{shown(name)}`" for name in unlisted)
            misshapen.append(f"pattern {number} of {total}, `{shown(pattern)}`, names {names}, which the test binary "
                             f"does not list: go test -run rewrites and splits what it does not know as a name, "
                             f"so a shard's pattern holds only names -test.list printed")
    if misshapen:
        raise Uncovered("\n".join(misshapen))
    # The shards of a name are a set: a name the internal and the external test package both
    # declare is listed twice, by every pattern that selects it, and is in two shards only when
    # two patterns select it.
    shards_of: dict[str, set[int]] = {name: set() for name in everything}
    for number, pattern in enumerate(patterns, start=1):
        for name in listed(binary, pattern):
            shards_of.setdefault(name, set()).add(number)
    problems = []
    for name in sorted(shards_of):
        shards = shards_of[name]
        if not shards:
            problems.append(f"{name} is in no shard: none of the {total} patterns selects it, so no leg runs it")
        elif len(shards) > 1:
            numbers = " and ".join(str(n) for n in sorted(shards))
            problems.append(f"{name} is in shards {numbers} of {total}: each of those legs runs it")
    if problems:
        raise Uncovered("\n".join(problems))


def split_package(arguments: list[str]) -> tuple[list[str], str]:
    """The build flags and the package: the last argument is the package."""
    if not arguments:
        raise Refused(f"no package given\n{USAGE}")
    package = arguments[-1]
    if package.startswith("-"):
        raise Refused(f"the last argument, {package}, is a flag, not a package\n{USAGE}")
    return arguments[:-1], package


def pattern_command(arguments: list[str]) -> str:
    if len(arguments) < 3:
        raise Refused(USAGE)
    try:
        index, total = int(arguments[0]), int(arguments[1])
    except ValueError:
        raise Refused(f"the index and the total must be whole numbers, got {arguments[0]!r} and {arguments[1]!r}\n{USAGE}")
    if total < 1 or not 0 <= index < total:
        raise Refused(f"index {index} is not a shard of {total}: an index runs from 0 to {total - 1}")
    flags, package = split_package(arguments[2:])
    with package_tests(flags, package, "split") as (binary, everything):
        # Each shard is the names shard_of gives its number, so a name given no shard's number is
        # in none, and prove() below refuses it by name rather than the split failing on an index.
        # go-test-shards.test.sh breaks shard_of that way to show this leg's own proof refuses it.
        shards = [[name for name in everything if shard_of(name, total) == number] for number in range(total)]
        empty = [str(number + 1) for number, names in enumerate(shards) if not names]
        if empty:
            raise Refused(f"shard(s) {', '.join(empty)} of {total} hold no test of the {len(everything)} "
                          f"{package} lists: the matrix has more legs than this package needs")
        patterns = [alternation(names) for names in shards]
        prove(binary, everything, patterns)
    print(f"go-test-shards.py: shard {index + 1} of {total} holds {len(shards[index])} of the "
          f"{len(everything)} tests of {package}; every test is in exactly one of the {total} shards",
          file=sys.stderr)
    return patterns[index]


def check_command(arguments: list[str]) -> None:
    if "--" not in arguments:
        raise Refused(f"no `--` before the patterns\n{USAGE}")
    cut = arguments.index("--")
    flags, package = split_package(arguments[:cut])
    patterns = arguments[cut + 1:]
    if not patterns:
        raise Refused(f"no pattern after `--`\n{USAGE}")
    with package_tests(flags, package, "check") as (binary, everything):
        prove(binary, everything, patterns)
    print(f"go-test-shards.py: every one of the {len(everything)} tests of {package} is in exactly one "
          f"of the {len(patterns)} shards", file=sys.stderr)


def main(arguments: list[str]) -> int:
    try:
        if arguments[:1] == ["pattern"]:
            print(pattern_command(arguments[1:]))
        elif arguments[:1] == ["check"]:
            check_command(arguments[1:])
        else:
            raise Refused(USAGE)
    except Refused as refusal:
        print(f"go-test-shards.py: {refusal}", file=sys.stderr)
        return 2
    except Uncovered as uncovered:
        for line in str(uncovered).splitlines():
            print(f"go-test-shards.py: {line}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
