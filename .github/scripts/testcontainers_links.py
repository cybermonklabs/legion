#!/usr/bin/env python3
"""Fail when a command, or a test binary that starts itself, links testcontainers-go.

testcontainers-go's package init (internal/core/bootstrap) reads every process on the machine,
through gopsutil's process.Processes(), at each start of a binary that links it: 3.0-3.3 s and
379 MB of allocations with 8,300 processes on the dev box, 5.8 s at 16,551 and 8-15 s at 20,000
to 28,000. A test pays that once per package, but a command pays it at
every start, and a test binary that starts itself pays it at every start of its child: the
Dispatch browser row that ran requestseed six times paid it six times, and cmd/legion's tests,
which start their own binary 69 times in one test, ran out their package deadline on it. So no
binary of that kind may link it. The fix is never a longer deadline: move what the binary needs
out of the package that brings testcontainers in (internal/cipher/ciphertest/podclient is the
precedent, split from the cipher rig so requestseed would not link storetest's testpg), or drop
the import.

What it checks, per Go module (packages/envoy and packages/daemon unless others are named):

  * every main package of `go list ./...`, and every main package in a testdata directory of the
    module, which `./...` does not list but a test can build and start (the daemon's
    internal/runtime/tmux/testdata/omp-standin.go is one): `go list -deps` of each;
  * every test binary that starts itself, under the build that compiles the code that starts it:
    `go list -deps -test` of each. A package's test binary starts itself when one of its own
    _test.go files names `os.Args[0]`, `os.Executable(` or `testenv.Executable(` on a line that
    is not a `//` comment, or when its tests import a test helper of the module that does: a
    package importing "testing" whose own files name one of the three, or that imports such a
    helper (internal/stacktest's Under is one). A _test.go file that names one but that the
    default build leaves out is read under its one tag when its constraint is that tag alone
    (`//go:build memory`), and refused in any other shape, since which build compiles it is then
    not known here.

The rule reads the source, not what a run does. Product code that starts the test binary through
os.Executable on a test's behalf, with nothing in the test naming it, is not seen (the launcher's
roleinit re-exec is seen only because launcher's own tests name os.Executable too); and a test
that names os.Args[0] for another purpose than starting it is counted, which errs towards the
check. Every self-starting test binary it finds is printed, so the set is read, not assumed.

For each binary that links testcontainers it prints an error naming the binary, why it is
checked (a command, or the file and line that starts it), and the shortest import chain from it
to the first testcontainers package, so a reader knows which import to move. It exits 1 then,
and also when a module has no main package at all, since a module that offers nothing to check
has not passed, and when a go command fails, since a closure go could not list is unknown. It
exits 0 when every binary it found is clean, and 2 on a usage error.

Usage: testcontainers_links.py [MODULE_DIR ...], each directory as your shell reads it.
Go modules are downloaded as `go list` needs them, so it runs where the module cache can fill.
CI runs it in ci.yaml's ci-filters job; its tests (testcontainers_links.test.sh) run in ci.yaml's
test job.
"""

import os
import re
import subprocess
import sys
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path

import go_build_tags
REPO = Path(__file__).resolve().parents[2]
DEFAULT_MODULES = [REPO / "packages" / "envoy", REPO / "packages" / "daemon"]
FORBIDDEN = "github.com/testcontainers/"
SELF_START = re.compile(r"\bos\.Args\[0\]|\bos\.Executable\(|\btestenv\.Executable\(")
BARE_TAG = re.compile(r"^[A-Za-z0-9_.]+$")
EXCLUDED_BY_CONSTRAINTS = "build constraints exclude all Go files"
PACKAGE_FIELDS = "ImportPath,Name,Dir,GoFiles,TestGoFiles,XTestGoFiles,IgnoredGoFiles,Imports,TestImports,XTestImports"


class Refusal(Exception):
    """A module this script cannot judge, with what it could not read."""


@dataclass
class SelfStart:
    """A package whose test binary starts itself under one build: tags "" is the default."""

    package: str
    tags: str
    evidence: str


@dataclass
class Binary:
    """One binary to hold to the rule: its root in the import graph go list gave for its build,
    and what to call it and why it is checked."""

    root: str
    edges: dict
    what: str
    why: str


@dataclass
class Report:
    """What one module holds: every binary checked, the self-starting test builds among them, and
    each binary that links testcontainers with its chain."""

    binaries: list = field(default_factory=list)
    self_starts: list = field(default_factory=list)
    offenders: list = field(default_factory=list)


def go(module: Path, *args: str) -> str:
    result = subprocess.run(["go", *args], cwd=module, capture_output=True, text=True)
    if result.returncode != 0:
        raise Refusal(f"`go {' '.join(args)}` failed ({result.returncode}), so what it lists is unknown:\n"
                      f"{result.stderr.strip()}")
    return result.stdout




def first_self_start_line(path: Path):
    """The first line of path that names a way to start the test binary, as `file:line`, or
    None. A `//` comment line does not count."""
    with open(path, encoding="utf-8") as source:
        for number, line in enumerate(source, 1):
            if line.lstrip().startswith("//"):
                continue
            if SELF_START.search(line):
                return f"{os.path.relpath(path)}:{number}"
    return None




def testdata_dirs(packages: list, module: Path) -> list:
    """Each directory under a testdata directory of a listed package that holds a .go file other
    than a test file, relative to module, as `./<path>`."""
    dirs = set()
    for package in packages:
        testdata = Path(package["Dir"]) / "testdata"
        if not testdata.is_dir():
            continue
        for current, subdirs, names in os.walk(testdata):
            if os.path.isfile(os.path.join(current, "go.mod")):
                subdirs[:] = []  # a nested module: go list of this module cannot load its packages
                continue
            if any(name.endswith(".go") and not name.endswith("_test.go") for name in names):
                dirs.add("./" + os.path.relpath(current, module))
    return sorted(dirs)


def helpers_that_start_the_binary(packages: list) -> dict:
    """Test helpers of the module that start the test binary which runs them: package -> evidence.
    A helper imports "testing"; it starts the binary when one of its own files names a way to, or
    when it imports a helper that does."""
    helpers = {}
    by_path = {p["ImportPath"]: p for p in packages}
    for package in packages:
        if "testing" not in package.get("Imports", []):
            continue
        for name in package.get("GoFiles", []):
            evidence = first_self_start_line(Path(package["Dir"]) / name)
            if evidence:
                helpers[package["ImportPath"]] = evidence
                break
    changed = True
    while changed:
        changed = False
        for path, package in by_path.items():
            if path in helpers or "testing" not in package.get("Imports", []):
                continue
            for imported in package.get("Imports", []):
                if imported in helpers:
                    helpers[path] = f"imports {imported}, which starts it ({helpers[imported]})"
                    changed = True
                    break
    return helpers


def self_starting_tests(packages: list) -> list:
    """Every (package, build) whose test binary starts itself, with the evidence for each."""
    helpers = helpers_that_start_the_binary(packages)
    found = []
    for package in packages:
        directory = Path(package["Dir"])
        evidence = None
        for name in package.get("TestGoFiles", []) + package.get("XTestGoFiles", []):
            evidence = first_self_start_line(directory / name)
            if evidence:
                break
        if not evidence:
            for imported in package.get("TestImports", []) + package.get("XTestImports", []):
                if imported in helpers:
                    evidence = f"its tests import {imported}, which starts it ({helpers[imported]})"
                    break
        if evidence:
            found.append(SelfStart(package["ImportPath"], "", evidence))
        tagged = {}
        for name in package.get("IgnoredGoFiles", []):
            if not name.endswith("_test.go"):
                continue
            path = directory / name
            line = first_self_start_line(path)
            if not line:
                continue
            constraints = go_build_tags.constraint_lines(path)
            constraint = constraints[0] if len(constraints) == 1 else ""
            if not BARE_TAG.fullmatch(constraint):
                raise Refusal(
                    f"{line} starts the test binary, and the default build leaves the file out under "
                    f"the constraint '{constraint or '(its file name)'}', which is not one tag alone, "
                    f"so which build compiles it is not known here. Put that code in a file "
                    f"constrained by one tag alone, or teach this script the shape.")
            tagged.setdefault(constraint, line)
        for tag, line in sorted(tagged.items()):
            found.append(SelfStart(package["ImportPath"], tag, line))
    return found


def graph(module: Path, *args: str) -> dict:
    """ImportPath -> Imports over one `go list -deps` run."""
    return {p["ImportPath"]: p.get("Imports", [])
            for p in go_build_tags.decode_stream(go(module, "list", "-deps", "-json=ImportPath,Imports", *args))}


def base(path: str) -> str:
    """An import path without the ` [x.test]` a test variant carries."""
    return path.split(" [", 1)[0]


def chain_to_forbidden(edges: dict, root: str):
    """The shortest import chain from root to a testcontainers package, or None."""
    if root not in edges:
        raise Refusal(f"go list listed no package {root}, so its closure is unknown")
    parent = {root: None}
    queue = deque([root])
    while queue:
        current = queue.popleft()
        if base(current).startswith(FORBIDDEN):
            path = []
            while current is not None:
                path.append(current)
                current = parent[current]
            return list(reversed(path))
        for imported in edges.get(current, []):
            if imported not in parent:
                parent[imported] = current
                queue.append(imported)
    return None


def shown(chain: list) -> str:
    """A chain as a reader follows it: one import path per step, the ` [x.test]` suffix of a test
    build dropped, and the package under test marked as compiled with its test files, since an
    import on that step may be in either."""
    steps = []
    for path in chain:
        name, _, variant = path.partition(" [")
        if variant.rstrip("]") == name + ".test":
            name += " (with its test files)"
        steps.append(name)
    return " -> ".join(steps)


def check_module(module: Path) -> Report:
    report = Report()
    packages = go_build_tags.decode_stream(go(module, "list", f"-json={PACKAGE_FIELDS}", "./..."))
    mains = [p["ImportPath"] for p in packages if p.get("Name") == "main"]
    extra = testdata_dirs(packages, module)
    if extra:
        for p in go_build_tags.decode_stream(go(module, "list", "-e", "-json=ImportPath,Name,Error", *extra)):
            error = (p.get("Error") or {}).get("Err", "")
            if error:
                if EXCLUDED_BY_CONSTRAINTS in error:
                    continue
                raise Refusal(f"go list cannot load {p['ImportPath']}, a testdata directory of the module: {error}")
            if p.get("Name") == "main":
                mains.append(p["ImportPath"])
    if not mains:
        raise Refusal("the module holds no main package, so this check saw no binary there: nothing was checked")

    edges = graph(module, *mains)
    for main in mains:
        report.binaries.append(Binary(main, edges, f"the command {main}", "a command pays the init at every start"))

    report.self_starts = self_starting_tests(packages)
    by_tags = {}
    for start in report.self_starts:
        by_tags.setdefault(start.tags, []).append(start)
    for tags, starts in sorted(by_tags.items()):
        test_edges = graph(module, "-test", *(["-tags", tags] if tags else []), *[s.package for s in starts])
        for start in starts:
            build = f" built with -tags {tags}" if tags else ""
            report.binaries.append(Binary(start.package + ".test", test_edges, f"the test binary of {start.package}{build}",
                                          f"it starts itself ({start.evidence})"))

    for binary in report.binaries:
        chain = chain_to_forbidden(binary.edges, binary.root)
        if chain:
            report.offenders.append((binary, chain))
    return report


def main(argv: list) -> int:
    if any(arg.startswith("-") for arg in argv):
        print("testcontainers_links.py: takes Go module directories and no option; usage: "
              "testcontainers_links.py [MODULE_DIR ...]", file=sys.stderr)
        return 2
    modules = [Path(arg).resolve() for arg in argv] or DEFAULT_MODULES
    refused = linked = False
    for module in modules:
        name = os.path.relpath(module)
        if not (module / "go.mod").is_file():
            print(f"::error::{name} is no Go module directory: it holds no go.mod")
            refused = True
            continue
        try:
            report = check_module(module)
        except Refusal as refusal:
            print(f"::error::{name}: {refusal}")
            refused = True
            continue
        print(f"{name}: {len(report.self_starts)} test binaries start themselves:")
        for start in report.self_starts:
            build = f" (built with -tags {start.tags})" if start.tags else ""
            print(f"  {start.package}{build}: {start.evidence}")
        for binary, chain in report.offenders:
            print(f"::error::{name}: {binary.what} links testcontainers-go, and {binary.why}. "
                  f"The import chain: {shown(chain)}")
        commands = len(report.binaries) - len(report.self_starts)
        print(f"{name}: {len(report.binaries) - len(report.offenders)} of {len(report.binaries)} binaries link no "
              f"testcontainers-go ({commands} commands, {len(report.self_starts)} self-starting test binaries)")
        linked = linked or bool(report.offenders)
    if linked:
        print("testcontainers-go's package init reads every process on the machine at each start of a binary "
              "that links it, so a command, or a test binary that starts itself, must not link it. Move what the "
              "binary uses out of the package the chain goes through into one that imports no testcontainers "
              "(internal/cipher/ciphertest/podclient is the precedent), or drop the import.")
    return 1 if refused or linked else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
