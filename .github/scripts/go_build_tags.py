#!/usr/bin/env python3
"""The tag sets a vet of a Go module for another platform runs under, so that between them they
compile every file of the module that platform can build.

A tag adds files to a package and a negated tag takes them out, so no one `-tags` list compiles
both a `//go:build feature` file and a `//go:build !feature` one, and a list kept by hand falls
behind the module the day a new tag lands: `go vet -tags A,B ./...` passes over a file outside
that list and stays green, since it never read the file. This prints the sets instead, one per
line, comma-joined (an empty line is the set with no tag), read off the module's own files:

    sets=$(python3 .github/scripts/go_build_tags.py packages/envoy darwin arm64)
    while IFS= read -r tags; do GOOS=darwin GOARCH=arm64 go vet -tags "$tags" ./...; done <<< "$sets"

The first set is every tag a constraint of the module names. For each file the target can build
under some choice of tags (its name and its constraint allow the target platform) and no set so
far builds, one more set is added: every tag, with that file's own tags set to a choice that
builds it. A tag is a name of a constraint that no toolchain sets (is_tag): not a GOOS or GOARCH,
not one of an architecture's feature names (`amd64.v1`, `arm64.v8.0`), and none of NOT_TAGS, the
release tags or the GOEXPERIMENT ones. A constraint is the file's `//go:build` line, else its
`// +build` lines, read as Go reads them (constraint), with names in any script Go accepts.

What the target's toolchain sets is asked of the go command, never written here (toolchain_names):
its tool and release tags and whether cgo is on. check-ci-filters.sh's rule 20 then asks go list
itself, under each set, which files the vet compiles, and fails on a file go would build for the
target under some tags while no set does; a set this script gets wrong fails there, by name.
"""

import itertools
import os
import re
import subprocess
import sys
from pathlib import Path

# The names a constraint may use that no vet chooses, beyond the platform names: the toolchain
# sets each (gc, gccgo, the sanitizer modes the -race, -msan and -asan builds turn on, cgo,
# unix, boringcrypto, which Go reads as goexperiment.boringcrypto), or nothing ever does
# (ignore, for a file no build compiles, such as a generator `go run` starts).
NOT_TAGS = frozenset({"unix", "cgo", "gc", "gccgo", "race", "msan", "asan", "ignore", "boringcrypto"})
NOT_TAG_PATTERN = re.compile(r"go1\.\d+|goexperiment\..+")
# The GOOS and GOARCH values go/build knows beyond `go tool dist list`'s ports (syslist.go):
# a file named for one, foo_zos.go say, is still a file of that platform alone.
EXTRA_OS = frozenset({"hurd", "nacl", "zos"})
EXTRA_ARCH = frozenset({"amd64p32", "armbe", "arm64be", "mips64p32", "mips64p32le", "ppc", "riscv",
                        "s390", "sparc", "sparc64"})
# The GOOS values the `unix` constraint matches (go/build's unixOS).
UNIX_OS = frozenset({"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios",
                     "linux", "netbsd", "openbsd", "solaris"})
# A GOOS that matches another's name too, as go/build's matchTag has it.
IMPLIES = {"android": "linux", "illumos": "solaris", "ios": "darwin"}
GO_BUILD = re.compile(r"^//go:build(?:\s(.*))?$")
PLUS_BUILD = re.compile(r"^//\s*\+build(?:\s(.*))?$")
# A name is any run of characters that is no operator, parenthesis, comma or space: Go's own
# lexer takes letters and digits of every script, `_` and `.`, so this never cuts one short.
NAME = re.compile(r"[^\s()!&|,]+")
TOKEN = re.compile(r"\s*(&&|\|\||!|\(|\)|[^\s()!&|]+)")
# A file whose constraint names more tags than this is refused rather than searched.
MAX_FILE_TAGS = 10


def platforms(module: Path) -> tuple:
    """(every GOOS, every GOARCH) go/build knows: `go tool dist list`'s ports, with the module's
    own toolchain, and the extra names above."""
    listed = subprocess.run(["go", "tool", "dist", "list"], cwd=module, capture_output=True, text=True, check=True)
    pairs = [line.split("/") for line in listed.stdout.split()]
    return frozenset(o for o, _ in pairs) | EXTRA_OS, frozenset(a for _, a in pairs) | EXTRA_ARCH


def toolchain_names(module: Path, goos: str, goarch: str) -> frozenset:
    """Every constraint name the module's toolchain sets for goos/goarch, as go/build's matchTag
    reads it: the platform and its implied names, gc, unix on a unix GOOS, cgo when the go command
    says cgo is on for that target, and the context's tool tags (architecture features,
    GOEXPERIMENTs) and release tags, read from the go command itself."""
    form = '{{join context.ToolTags " "}}|{{join context.ReleaseTags " "}}|{{context.CgoEnabled}}'
    listed = subprocess.run(["go", "list", "-e", "-f", form, "runtime"], cwd=module, capture_output=True,
                            text=True, check=True, env={**os.environ, "GOOS": goos, "GOARCH": goarch})
    tool, release, cgo = listed.stdout.strip().split("|")
    names = set(tool.split()) | set(release.split()) | {goos, goarch, "gc"}
    if goos in IMPLIES:
        names.add(IMPLIES[goos])
    if goos in UNIX_OS:
        names.add("unix")
    if cgo == "true":
        names.add("cgo")
    return frozenset(names)


def constraint(path: Path) -> str | None:
    """A Go file's build constraint as a `//go:build` expression: its `//go:build` line, else its
    `// +build` lines (a line's space-separated options or'd, an option's comma-separated terms
    and'd, the lines and'd), else None. Only the comments above the package clause count."""
    plus = []
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        found = GO_BUILD.match(line)
        if found:
            return (found.group(1) or "").strip()
        found = PLUS_BUILD.match(line)
        if found:
            options = (found.group(1) or "").split()
            plus.append("(" + " || ".join("(" + " && ".join(option.split(",")) + ")" for option in options) + ")")
        elif line.strip() and not line.lstrip().startswith("//"):
            break
    return " && ".join(plus) if plus else None


def names_of(expression: str) -> set:
    """The names a constraint expression uses, its operators and parentheses aside."""
    return set(NAME.findall(expression))


def parse(expression: str):
    """A `//go:build` expression as a tree of ("or", a, b), ("and", a, b), ("not", a) and
    ("name", n). Raises ValueError on anything Go would refuse to read."""
    tokens, at = [], 0
    while at < len(expression.rstrip()):
        found = TOKEN.match(expression, at)
        if not found:
            raise ValueError(f"cannot read {expression[at:]!r}")
        tokens.append(found.group(1))
        at = found.end()
    position = 0

    def take(expected=None):
        nonlocal position
        if position >= len(tokens) or (expected and tokens[position] != expected):
            raise ValueError(f"expected {expected or 'more'} in {expression!r}")
        position += 1
        return tokens[position - 1]

    def either():
        node = both()
        while position < len(tokens) and tokens[position] == "||":
            take()
            node = ("or", node, both())
        return node

    def both():
        node = single()
        while position < len(tokens) and tokens[position] == "&&":
            take()
            node = ("and", node, single())
        return node

    def single():
        token = take()
        if token == "!":
            return ("not", single())
        if token == "(":
            node = either()
            take(")")
            return node
        if token in ("&&", "||", ")"):
            raise ValueError(f"unexpected {token} in {expression!r}")
        return ("name", token)

    tree = either()
    if position != len(tokens):
        raise ValueError(f"unexpected {tokens[position]} in {expression!r}")
    return tree


def evaluate(tree, true_names) -> bool:
    """Whether the expression holds when exactly the names in true_names are set."""
    kind = tree[0]
    if kind == "name":
        return tree[1] in true_names
    if kind == "not":
        return not evaluate(tree[1], true_names)
    left, right = evaluate(tree[1], true_names), evaluate(tree[2], true_names)
    return left or right if kind == "or" else left and right


def is_tag(name: str, known: tuple) -> bool:
    """Whether a constraint name is a tag a vet chooses, given (every GOOS, every GOARCH): no
    platform name, no architecture feature name (`<GOARCH>.<feature>`), none the toolchain sets."""
    oses, arches = known
    return not (name in oses or name in arches or name in NOT_TAGS or NOT_TAG_PATTERN.fullmatch(name)
                or ("." in name and name.split(".", 1)[0] in arches))


def name_allows(file_name: str, goos: str, goarch: str, known: tuple) -> bool:
    """Whether a file's name lets goos/goarch build it, as go/build's goodOSArchFile reads a
    `_<GOOS>`, `_<GOARCH>` or `_<GOOS>_<GOARCH>` suffix (before `_test`)."""
    oses, arches = known
    stem = file_name[: -len(".go")] if file_name.endswith(".go") else file_name
    if "_" not in stem:
        return True
    parts = stem[stem.index("_"):].split("_")
    if parts and parts[-1] == "test":
        parts = parts[:-1]
    target = {goos, goarch, IMPLIES.get(goos, goos)}
    if len(parts) >= 2 and parts[-2] in oses and parts[-1] in arches:
        return parts[-2] in target and parts[-1] in target
    if parts and (parts[-1] in oses or parts[-1] in arches):
        return parts[-1] in target
    return True


def go_skips_dir(parent: Path, name: str) -> bool:
    """Whether the go command passes over a directory of a `<root>/...` pattern: testdata, a name
    starting with `.` or `_`, node_modules, and a nested module. The one copy of that rule:
    go_files below, ci_filter_coverage.py's closure engine and ci_job_reads.py's race-lane walk
    all read it, so their walks cannot drift. It lives here, in a module that imports nothing
    beyond the standard library, so the vet steps can run this script on a runner with no
    PyYAML."""
    return (name == "testdata" or name.startswith((".", "_")) or name == "node_modules"
            or (parent / name / "go.mod").is_file())


def go_files(directory: Path):
    """Every .go file under `directory` that its `./...` pattern walks."""
    for path in sorted(directory.iterdir()):
        if path.is_dir():
            if not go_skips_dir(directory, path.name):
                yield from go_files(path)
        elif path.suffix == ".go":
            yield path


def tag_sets(module: Path, goos: str, goarch: str) -> list:
    """The tag sets, as sorted lists, under which a vet of every package of the module for
    goos/goarch compiles each file that platform can build under some choice of tags."""
    if not (module / "go.mod").is_file():
        raise SystemExit(f"go_build_tags.py: {module} holds no go.mod, so it is no Go module")
    known = platforms(module)
    toolchain = toolchain_names(module, goos, goarch)
    constrained = []
    for path in go_files(module):
        expression = constraint(path)
        if expression is None:
            continue
        try:
            tree = parse(expression)
        except ValueError as refusal:
            raise SystemExit(f"go_build_tags.py: {path}: its build constraint cannot be read: {refusal}")
        constrained.append((path, tree, sorted(n for n in names_of(expression) if is_tag(n, known))))
    every = frozenset(tag for _, _, tags in constrained for tag in tags)
    sets = [every]
    for path, tree, tags in constrained:
        if not tags or not name_allows(path.name, goos, goarch, known):
            continue
        if any(evaluate(tree, toolchain | chosen) for chosen in sets):
            continue
        if len(tags) > MAX_FILE_TAGS:
            raise SystemExit(f"go_build_tags.py: {path} names {len(tags)} tags, more than the "
                             f"{MAX_FILE_TAGS} this script searches for a set that builds it")
        for bits in itertools.product((True, False), repeat=len(tags)):
            chosen = (every - set(tags)) | {tag for tag, on in zip(tags, bits) if on}
            if evaluate(tree, toolchain | chosen):
                sets.append(frozenset(chosen))
                break
    return [sorted(chosen) for chosen in sets]


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit("usage: go_build_tags.py <module directory> <GOOS> <GOARCH>")
    for chosen in tag_sets(Path(sys.argv[1]), sys.argv[2], sys.argv[3]):
        print(",".join(chosen))
