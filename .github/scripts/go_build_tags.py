#!/usr/bin/env python3
"""The build tags a Go module's files are built under, for a vet that must compile every one.

A tag adds files to a package, so `go vet -tags A,B ./...` compiles each file whose `//go:build`
line A and B satisfy, and passes over a file under a tag the list leaves out, saying nothing:
the vet stays green because it never read the file. A list kept by hand falls behind the
module the day a new tag lands. This prints the module's tags instead, read off the source:

    go vet -tags "$(python3 .github/scripts/go_build_tags.py packages/envoy)" ./...

It reads the `//go:build` line of every .go file the go command's `./...` walks (go_skips_dir
below says which directories it passes over) and prints every name such a line uses, sorted and
comma-joined, but the ones that are not a choice of the vet's:

  * every GOOS and GOARCH `go tool dist list` names: the target platform sets those, and a
    platform name passed as a tag would compile that platform's files on another one;
  * the names Go's toolchain sets itself (NOT_TAGS): `unix`, `cgo`, the compilers, the release
    tags `go1.N`, `goexperiment.*`, and the sanitizer modes, whose files the -race, -msan and
    -asan builds compile and a tag would compile without them (the `!race` files would drop
    out too);
  * `ignore`, the convention for a file no build compiles, such as a generator a `go run`
    starts.

A module whose files name no other tag prints an empty line, which `-tags ""` reads as none.
check-ci-filters.sh asks go list, under each darwin vet's own platform and tags, whether any file
that vet passes over would be compiled with a tag its line names (rule 20), so a tag this script
fails to print fails that check rather than going unvetted.
"""

import re
import subprocess
import sys
from pathlib import Path

# The names a //go:build line may use that no vet chooses: Go's toolchain sets each of them
# itself, or, for `ignore`, nothing ever does. check-ci-filters.sh's rule 20 decides what a tag
# is by is_tag below too, so the two agree on what a missing tag is.
NOT_TAGS = frozenset({"unix", "cgo", "gc", "gccgo", "race", "msan", "asan", "ignore"})
NOT_TAG_PATTERN = re.compile(r"go1\.\d+|goexperiment\..+")
CONSTRAINT = re.compile(r"^//go:build\s+(.+)$")
NAME = re.compile(r"[A-Za-z0-9_.]+")


def platform_names() -> frozenset:
    """Every GOOS and GOARCH the installed go knows, from `go tool dist list`."""
    listed = subprocess.run(["go", "tool", "dist", "list"], capture_output=True, text=True, check=True).stdout
    return frozenset(part for line in listed.split() for part in line.split("/"))


def constraint(path: Path) -> str | None:
    """A Go file's `//go:build` expression: the first such line above its package clause."""
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        found = CONSTRAINT.match(line)
        if found:
            return found.group(1)
        if line.startswith("package "):
            return None
    return None


def names_of(expression: str) -> set:
    """The names a `//go:build` expression uses, its operators and parentheses aside."""
    return set(NAME.findall(expression))


def go_skips_dir(parent: Path, name: str) -> bool:
    """Whether the go command passes over a directory of a `<root>/...` pattern: testdata, a name
    starting with `.` or `_`, node_modules, and a nested module. The one copy of that rule:
    go_files below, ci_filter_coverage.py's closure engine and ci_job_reads.py's race-lane walk
    all read it, so their walks cannot drift. It lives here, in a module that imports nothing
    beyond the standard library, so the darwin vet steps can run this script on a runner with no
    PyYAML."""
    return (name == "testdata" or name.startswith((".", "_")) or name == "node_modules"
            or (parent / name / "go.mod").is_file())


def go_files(module: Path):
    """Every .go file under `module` that its `./...` pattern walks."""
    for path in sorted(module.iterdir()):
        if path.is_dir():
            if not go_skips_dir(module, path.name):
                yield from go_files(path)
        elif path.suffix == ".go":
            yield path


def is_tag(name: str, platforms: frozenset) -> bool:
    """Whether a name of a //go:build line is a tag a vet chooses."""
    return not (name in platforms or name in NOT_TAGS or NOT_TAG_PATTERN.fullmatch(name))


def module_tags(module: Path) -> list:
    """The tags the files of the module at `module` are built under, sorted."""
    if not (module / "go.mod").is_file():
        raise SystemExit(f"go_build_tags.py: {module} holds no go.mod, so it is no Go module")
    platforms = platform_names()
    tags = set()
    for path in go_files(module):
        expression = constraint(path)
        if expression:
            tags |= {name for name in names_of(expression) if is_tag(name, platforms)}
    return sorted(tags)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: go_build_tags.py <module directory>")
    print(",".join(module_tags(Path(sys.argv[1]))))
