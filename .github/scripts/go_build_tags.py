#!/usr/bin/env python3
"""The tag sets a vet of a Go module for another platform runs under, so that between them they
compile every file of the module that platform builds under some choice of tags.

A tag adds files to a package and a negated tag takes them out, so no one `-tags` list compiles
both a `//go:build feature` file and a `//go:build !feature` one, and a list kept by hand falls
behind the module the day a new tag lands: `go vet -tags A,B ./...` passes over a file outside
that list and stays green, since it never read the file. This prints the sets instead, read off
the module's own files, one per line, each as the whole go flag that selects it: `-tags=A,B`,
and `-tags=` for the set with no tag. No line is ever empty, since a shell's command
substitution drops trailing empty lines and with them a last set with no tag:

    cd packages/envoy && flags=$(python3 ../../.github/scripts/go_build_tags.py . darwin arm64)
    while IFS= read -r flag; do GOOS=darwin GOARCH=arm64 go vet "$flag" ./...; done <<< "$flags"

The first set is every tag a build constraint of the module names. Then, for each file no set
so far compiles whose constraint names a tag, every choice of that file's own tags is tried
over the rest, and the first one under which the file compiles is added as a set. Whether a
file compiles is never decided here: `go list -find`, run with the module's own toolchain for
the target platform and the set, says so, so Go's own constraint grammar, file-name suffixes
and toolchain names decide every answer. This script only reads which names a constraint uses
(constraint_names) and which of those are tags (is_tag). check-ci-filters.sh's rule 20 asks go
list again, file by file from disk, whether the sets a vet step loops over miss any file, and
fails naming it.
"""

import itertools
import json
import os
import re
import subprocess
import sys
from pathlib import Path

# The names a constraint may use that no vet chooses, beyond the platform names and the
# patterns below: the toolchain sets each (gc, gccgo, the sanitizer modes the -race, -msan and
# -asan builds turn on, cgo, unix, boringcrypto, which Go reads as goexperiment.boringcrypto), or
# nothing ever does (ignore, for a file no build compiles, such as a generator `go run` starts),
# or only a build against a patched dependency does (odatrace: its files call hooks that only
# the copy of nats-server packages/envoy/scripts/forced-order.sh patches declares, so no vet
# against the module's own nats-server compiles them; that script's go test is where they are
# compiled).
# Passing one as a tag would make a constraint on it true where the toolchain makes it false.
NOT_TAGS = frozenset({"unix", "cgo", "gc", "gccgo", "race", "msan", "asan", "ignore", "boringcrypto", "odatrace"})
NOT_TAG_PATTERN = re.compile(r"go1\.\d+|goexperiment\..+")
# The GOOS and GOARCH values go/build knows beyond `go tool dist list`'s ports (its syslist):
# names a constraint can use that are platforms, so never tags.
EXTRA_OS = frozenset({"hurd", "nacl", "zos"})
EXTRA_ARCH = frozenset({"amd64p32", "armbe", "arm64be", "mips64p32", "mips64p32le", "ppc", "riscv",
                        "s390", "sparc", "sparc64"})
# A constraint line, either syntax, and the names it uses: any run of characters that is no
# operator, parenthesis, comma or space, so a name in any script Go accepts is read whole.
CONSTRAINT_LINE = re.compile(r"^//(?:go:build|\s*\+build)(?:\s(.*))?$")
NAME = re.compile(r"[^\s()!&|,]+")
# A file whose constraint names more tags than this is refused rather than searched.
MAX_FILE_TAGS = 6
LISTED = ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles")
# The variables the go command reads (`go help environment`): its own (GO_EXTLINK_ENABLED and
# GCCGOTOOLDIR among them), GIT_ALLOW_PROTOCOL, and the C tools that configure cgo.
GO_VARIABLE = re.compile(
    r"GO[A-Z0-9_]*|CGO_\w+|AR|CC|CXX|FC|GCCGO|GCCGOTOOLDIR|GIT_ALLOW_PROTOCOL|PKG_CONFIG")
# Those of them that say only where the go command keeps files or fetches modules from, which
# changes no answer here.
GO_LOCATIONS = frozenset({"GIT_ALLOW_PROTOCOL", "GOAUTH", "GOCACHE", "GOINSECURE", "GOMODCACHE",
                          "GONOPROXY", "GONOSUMDB", "GOPATH", "GOPRIVATE", "GOPROXY", "GOSUMDB",
                          "GOTMPDIR", "GOVCS"})


def go_environment() -> dict:
    """This process's environment for a go command, less everything in it the go command reads to
    choose its toolchain or to decide what a package holds: every variable GO_VARIABLE names but
    GO_LOCATIONS, and the user's `go env -w` file (GOENV=off), which an empty variable does not
    override, since the go command reads an empty GOFLAGS or GOWORK from that file. The go command
    then takes its root from its own binary and its defaults from that root's go.env. Each such
    setting is the caller's or a python3 shim's, never the job's (check-ci-filters.sh's header says
    what mise's shim did). CI's one, setup-go's GOTOOLCHAIN=local, picks the same toolchain once
    dropped: the version packages/daemon/go.mod names, which setup-go installs and go.work names
    too. This script's go commands run under it, and check-ci-filters.sh's engine
    (ci_filter_coverage.py) and go-test-step-expansion.py import it from here."""
    env = {name: value for name, value in os.environ.items()
           if name in GO_LOCATIONS or not GO_VARIABLE.fullmatch(name)}
    env["GOENV"] = "off"
    return env


def go(module: Path, args: list, **env) -> str:
    """The output of a go command run in the module, so with its own toolchain, under
    go_environment() with `env` added (GOOS and GOARCH, for a listing of another platform): a
    caller's GOTOOLCHAIN=local with an older go first on PATH, or a caller's GOFLAGS, then changes
    no set. go_environment() drops setup-go's GOTOOLCHAIN=local too, so where go.work asks for a
    newer go than the one installed, the first go command here downloads that toolchain, in the vet
    steps and as ci_job_reads.py is imported. go's standard error therefore goes to this script's
    as go writes it, so a download that stalls is named while it waits; standard output is kept."""
    result = subprocess.run(["go", *args], cwd=module, env={**go_environment(), **env},
                            stdout=subprocess.PIPE, text=True)
    if result.returncode != 0:
        raise SystemExit(f"go_build_tags.py: in {module}, go {' '.join(args[:3])} ... exited "
                         f"{result.returncode}; its standard error is above")
    return result.stdout


_TOOLCHAINS: dict = {}


def toolchain_go(module: Path) -> str:
    """The go binary of the toolchain the module selects, by its path, resolved once per module.

    A go command of this repository runs under go_environment(), which drops the caller's
    GOTOOLCHAIN, so a go older than go.work's switches to the toolchain go.work names. That switch
    needs the toolchain in the module cache or a proxy to fetch it from, and a command that sets
    GOPROXY=off for its own listing forbids the fetch and dies with `toolchain not available`. So
    the toolchain is resolved first, by a `go env GOROOT` that writes nothing and runs under this
    machine's own cache and proxy (go(), which sets neither), and the caller then runs its listing
    with the binary this names under GOTOOLCHAIN=local: no switch, no fetch, and the listing may
    set GOPROXY=off. ci_filter_coverage.Session.toolchain() does the same for the engine's go
    commands. Where the answer is no file, the plain `go` is given back, as that function does.
    """
    key = str(module.resolve())
    if key not in _TOOLCHAINS:
        root = go(module, ["env", "GOROOT"]).strip()
        exe = Path(root) / "bin" / "go" if root else None
        _TOOLCHAINS[key] = str(exe) if exe and exe.is_file() else "go"
    return _TOOLCHAINS[key]


def platforms(module: Path) -> tuple:
    """(every GOOS, every GOARCH) go/build knows: `go tool dist list`'s ports, with the module's
    own toolchain, and the extra names above."""
    pairs = [line.split("/") for line in go(module, ["tool", "dist", "list"]).split()]
    return frozenset(o for o, _ in pairs) | EXTRA_OS, frozenset(a for _, a in pairs) | EXTRA_ARCH


def constraint_names(path: Path) -> set:
    """The names a Go file's build constraint lines use, `//go:build` and `// +build` alike, read
    from the file's header as go/build's parseFileHeader reads it: a byte-order mark aside, each
    line trimmed, line comments and `/* */` comments passed over (a constraint line inside a
    block comment is none), up to the first text that is no comment."""
    names, in_block = set(), False
    for raw in path.read_text(encoding="utf-8-sig", errors="replace").splitlines():
        line = raw.strip()
        if not in_block:
            found = CONSTRAINT_LINE.match(line)
            if found:
                names |= set(NAME.findall(found.group(1) or ""))
        while line:
            if in_block:
                end = line.find("*/")
                if end < 0:
                    break
                in_block, line = False, line[end + 2:].strip()
            elif line.startswith("//"):
                break
            elif line.startswith("/*"):
                in_block, line = True, line[2:].strip()
            else:
                return names
    return names


def is_tag(name: str, known: tuple) -> bool:
    """Whether a constraint name is a tag a vet chooses, given (every GOOS, every GOARCH): no
    platform name, no architecture feature name (`<GOARCH>.<feature>`, as amd64.v1), none of
    NOT_TAGS, and no release (go1.N) or GOEXPERIMENT name."""
    oses, arches = known
    return not (name in oses or name in arches or name in NOT_TAGS or NOT_TAG_PATTERN.fullmatch(name)
                or ("." in name and name.split(".", 1)[0] in arches))


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
    """Every .go file under `directory` that its `./...` pattern walks, less the names starting
    with `_` or `.`, which the go command never reads."""
    for path in sorted(directory.iterdir()):
        if path.is_dir():
            if not go_skips_dir(directory, path.name):
                yield from go_files(path)
        elif path.suffix == ".go" and not path.name.startswith(("_", ".")):
            yield path


def decode_stream(text: str) -> list:
    """The JSON objects of a stream such as `go list -json` prints, one after another. The one
    copy: ci_filter_coverage.py's closure engine and rule 20 read it from here too."""
    decoder, index, out = json.JSONDecoder(), 0, []
    while True:
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            return out
        obj, index = decoder.raw_decode(text, index)
        out.append(obj)


def compiled(module: Path, goos: str, goarch: str, tags: list, dirs: list, **env) -> set:
    """The files, as resolved paths, that go list compiles in `dirs` (each named as a directory,
    not a pattern, so a directory whose every file is excluded is still listed) for goos/goarch
    under `tags`: a package's files and its test files, which a vet reads too. `-find` keeps go
    list from loading the packages these import; in module mode go still resolves each import,
    which would download every module a cold runner's cache lacks. So it runs with GOPROXY=off:
    such an import is then a load error -e keeps, and the file lists, which go reads off each
    directory under the build context, are the same. `env` adds to the environment, as
    CGO_ENABLED=1 does for a listing of a native runner's build from a machine of another GOARCH,
    where go would otherwise turn cgo off."""
    base = module.resolve()
    patterns = ["." if d.resolve() == base else f"./{d.resolve().relative_to(base).as_posix()}" for d in dirs]
    out = go(module, ["list", "-e", "-find", f"-json=Dir,{','.join(LISTED)}", "-tags", ",".join(tags), *patterns],
             GOOS=goos, GOARCH=goarch, GOPROXY="off", **env)
    return {str((Path(package["Dir"]) / name).resolve())
            for package in decode_stream(out) for field in LISTED for name in package.get(field) or []}


def tag_sets(module: Path, goos: str, goarch: str) -> list:
    """The tag sets, as sorted lists, under which vets of every package of the module for
    goos/goarch compile between them each file that platform builds under some choice of tags."""
    if not (module / "go.mod").is_file():
        raise SystemExit(f"go_build_tags.py: {module} holds no go.mod, so it is no Go module")
    known = platforms(module)
    files = list(go_files(module))
    if not files:
        raise SystemExit(f"go_build_tags.py: {module} holds no .go file")
    own = {path: sorted(n for n in constraint_names(path) if is_tag(n, known)) for path in files}
    every = sorted(set().union(*own.values()))
    dirs = sorted({path.parent for path in files})
    sets = [every]
    covered = compiled(module, goos, goarch, every, dirs)
    for path, tags in own.items():
        if not tags or str(path.resolve()) in covered:
            continue
        if len(tags) > MAX_FILE_TAGS:
            raise SystemExit(f"go_build_tags.py: {path} names {len(tags)} tags, more than the "
                             f"{MAX_FILE_TAGS} this script tries to find a set that builds it")
        for bits in itertools.product((True, False), repeat=len(tags)):
            chosen = sorted((set(every) - set(tags)) | {tag for tag, on in zip(tags, bits) if on})
            if chosen in sets:
                continue
            if str(path.resolve()) in compiled(module, goos, goarch, chosen, [path.parent]):
                sets.append(chosen)
                covered |= compiled(module, goos, goarch, chosen, dirs)
                break
    return sets


if __name__ == "__main__":
    if len(sys.argv) != 4:
        raise SystemExit("usage: go_build_tags.py <module directory> <GOOS> <GOARCH>")
    for chosen in tag_sets(Path(sys.argv[1]), sys.argv[2], sys.argv[3]):
        print(f"-tags={','.join(chosen)}")
