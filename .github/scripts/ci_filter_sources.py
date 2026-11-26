"""Where the hand-kept half of what each CI job reads lives, read back into the engine's records.

Two kinds of fact are written by hand, each beside its owner rather than in one shared table:

  .github/ci-gates/*.py   one file per gate of a changes filter: `GATE = gate(...)`, the job (or the
                          step) the filter gates, the go commands it runs and the files its other
                          steps read, run in the namespace of .github/scripts/ci_job_reads.py, which
                          keeps the helpers every gate shares. A filter the engine computes no read
                          set for is a file of its own too: `EXCLUDED = excluded(name, why,
                          patterns)`, its patterns kept by hand (worker_image, browser_image, whose
                          coverage .github/scripts/check-image-trigger-paths.sh proves).

  //ci: lines in Go files   what a test does outside its package when a run compiles it, in one
                          block at the top of the test file, right after its package clause, each
                          line with its why:
                            //ci:reads <glob> [!<glob>...] <why>
                              it reads the files the glob matches, but those an `!` glob matches;
                            //ci:reads-set <name> <why>
                              it reads a set ci_job_reads.py's READ_SETS computes (an import
                              closure, or a set other gates share);
                            //ci:builds <directory> <package>... [NAME=value...] <why>
                              it starts a go build of the packages in the directory, under the env.
                          A glob is relative to the repository root. The line is keyed on the file it
                          is in, which is what RULES was keyed on: it applies when a run compiles
                          that file (ci_filter_coverage.closure). A why names the function or the
                          test that does the read, never a line number of its own file, which every
                          edit above it would move.

Two pull requests that each add a read of a different test, or a gate of a different job, then edit
different files, where both used to edit one dict or one list of ci_job_reads.py at the same place.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, replace
from pathlib import Path

from ci_filter_coverage import REPO, Read, RepositoryInputError, read_repository_text, run

GATES_DIR = ".github/ci-gates"
# A //ci: line of a Go file: its kind and the rest of the line.
DIRECTIVE = re.compile(r"^\s*//ci:(\S*)(?:[ \t]+(.*?))?\s*$")
KINDS = ("reads", "reads-set", "builds")
ENV_WORD = re.compile(r"^[A-Z_][A-Z0-9_]*=\S*$")


@dataclass(frozen=True)
class Excluded:
    """A filter the engine computes no read set for, its patterns written by hand in its file."""

    filter: str
    why: str
    patterns: tuple
    source: str = ""


def excluded(filter, why, patterns):
    return Excluded(filter, why, tuple(patterns))


@dataclass(frozen=True)
class Directive:
    """One //ci: line: the file and line it is on, its kind, its globs (a reads line's glob and
    its `!` globs) and what it declares."""

    path: str
    line: int
    kind: str
    globs: tuple
    entries: tuple


def gate_files(root: Path = REPO) -> list:
    """Every file of .github/ci-gates, sorted, as a path relative to the repository root."""
    directory = root / GATES_DIR
    return sorted(f"{GATES_DIR}/{p.name}" for p in directory.glob("*.py")) if directory.is_dir() else []


OLD_TABLES = ("GATES", "RULES", "EXCLUDED", "SCRIPT_TEST_GATES", "SCRIPT_TEST_READS")


def load_gate_files(namespace: dict) -> tuple:
    """(the gates, {filter: Excluded}) of .github/ci-gates, each file run in a copy of
    `namespace` (ci_job_reads.py's globals), so a file names a helper or a constant the way a GATES
    entry did. A file must bind exactly one of GATE and EXCLUDED; a file that cannot be read, run
    or binds neither is a RepositoryInputError naming it, since the filter it declares is then
    unknown. Each gate reads its own file, as a job read the filters file that held its filter:
    a change to what a job is declared to read selects that job."""
    # A branch made before the gates and the rules moved out of the table keeps its old tables in
    # its copy of ci_job_reads.py when it merges main, and the loaded ones would replace them,
    # dropping its rows without a word.
    for old in OLD_TABLES:
        if old in namespace:
            raise RepositoryInputError(".github/scripts/ci_job_reads.py", RuntimeError(
                f"binds {old}, which this module no longer holds: each gate is a file of {GATES_DIR} and "
                f"each RULES row the //ci: lines of its test file. Run .github/scripts/ci-reads-convert, which "
                f"moves what a branch added to the old tables into those files"))
    gates, excluded_filters = [], {}
    for path in gate_files():
        scope = dict(namespace)
        try:
            exec(compile(read_repository_text(REPO / path), path, "exec"), scope)
        except RepositoryInputError:
            raise
        except Exception as exc:  # the file is repository input: name it, never trace it
            raise RepositoryInputError(path, exc) from exc
        gate, kept = scope.get("GATE"), scope.get("EXCLUDED")
        if (gate is None) == (kept is None):
            raise RepositoryInputError(path, RuntimeError("binds neither GATE nor EXCLUDED, or both; a file of "
                                                          f"{GATES_DIR} declares one gate or one excluded filter"))
        if gate is not None:
            gates.append(replace(gate, reads=tuple(gate.reads) + (
                Read(path, "the file that declares what the job reads, which its filter is computed from"),)))
        else:
            if kept.filter in excluded_filters:
                raise RepositoryInputError(path, RuntimeError(f"excludes filter {kept.filter}, which "
                                                              f"{excluded_filters[kept.filter].source} excludes too"))
            excluded_filters[kept.filter] = replace(kept, source=path)
    return gates, excluded_filters


def directive_files(files: list) -> list:
    """The Go files that hold a //ci: line, read as text."""
    found = []
    for path in files:
        if not path.endswith(".go"):
            continue
        try:
            text = (REPO / path).read_text(encoding="utf-8")
        except (OSError, UnicodeError):
            continue  # not a Go file go would compile either
        if "//ci:" in text:
            found.append((path, text))
    return found


@dataclass(frozen=True)
class Words:
    """One //ci: line's words as its grammar reads them, before any table is asked: a reads line's
    glob and then its `!` globs as written (`globs`), a reads-set line's set (`name`), a builds
    line's directory, packages and env (`directory`, `packages`, `env` as sorted pairs), and every
    line's why. The one reading of the grammar: parse_directive builds the engine's records from
    it, and .github/scripts/go_legs_select.py reads each line the selection follows from it too."""

    kind: str
    why: str
    globs: tuple = ()
    name: str = ""
    directory: str = ""
    packages: tuple = ()
    env: tuple = ()


def words(kind: str, rest: str) -> Words:
    """A //ci: line's kind and the rest of it as Words, or a ValueError saying what is wrong with it.
    A builds line's directory must be one of the repository."""
    split = rest.split()
    if kind == "reads":
        globs = [split[0]] if split else []
        while len(split) > len(globs) and split[len(globs)].startswith("!"):
            globs.append(split[len(globs)])
        why = " ".join(split[len(globs):])
        if not globs or globs[0].startswith("!") or not why:
            raise ValueError("a reads line is `//ci:reads <glob> [!<glob>...] <why>`")
        return Words(kind, why, globs=tuple(globs))
    if kind == "reads-set":
        if len(split) < 2:
            raise ValueError("a reads-set line is `//ci:reads-set <name> <why>`")
        return Words(kind, " ".join(split[1:]), name=split[0])
    if kind == "builds":
        if not split:
            raise ValueError("a builds line is `//ci:builds <directory> <package>... [NAME=value...] <why>`")
        directory, i = split[0], 1
        packages, env = [], {}
        while i < len(split) and (split[i] == "." or split[i].startswith("./")):
            packages.append(split[i])
            i += 1
        while i < len(split) and ENV_WORD.match(split[i]):
            name, value = split[i].split("=", 1)
            env[name] = value
            i += 1
        why = " ".join(split[i:])
        if not packages or not why or os.path.isabs(directory) or not (REPO / directory).is_dir():
            raise ValueError("a builds line is `//ci:builds <directory> <package>... [NAME=value...] <why>`, the "
                             "directory relative to the repository root and each package `.` or `./...`")
        return Words(kind, why, directory=directory, packages=tuple(packages), env=tuple(sorted(env.items())))
    raise ValueError(f"//ci:{kind} is no kind of line this table reads ({', '.join(KINDS)})")


def parse_directive(path: str, line: int, kind: str, rest: str, read_sets: dict) -> Directive:
    """One //ci: line as a Directive, or a ValueError saying what is wrong with it. `line` is the
    line the directive is on, which every read it declares names beside its why."""
    read = words(kind, rest)
    where = f"{path}:{line}"
    if read.kind == "reads":
        entries = (Read(read.globs[0], f"{where} {read.why}", tuple(g.removeprefix("!") for g in read.globs[1:])),)
        return Directive(path, line, read.kind, read.globs, entries)
    if read.kind == "reads-set":
        if read.name not in read_sets:
            raise ValueError(f"names read set {read.name}, which ci_job_reads.py's READ_SETS does not have "
                             f"({', '.join(sorted(read_sets)) or 'none'})")
        return Directive(path, line, read.kind, (), tuple(read_sets[read.name](f"{where} {read.why}")))
    return Directive(path, line, read.kind, (), (run(read.directory, read.packages, f"{where} {read.why}",
                                                     mode="build", env=dict(read.env)),))


def load_directives(files: list, read_sets: dict) -> tuple:
    """(RULES, the Directives) of every //ci: line of a Go file of `files`: RULES maps each file
    that holds one to what its lines declare, in the order they are written, the shape
    ci_filter_coverage.closure reads. A line that cannot be read is a RepositoryInputError naming
    it, since what its test reads is then unknown."""
    rules, directives = {}, []
    for path, text in directive_files(files):
        for number, line in enumerate(text.split("\n"), 1):
            match = DIRECTIVE.match(line)
            if not match:
                continue
            try:
                directive = parse_directive(path, number, match.group(1), match.group(2) or "", read_sets)
            except ValueError as exc:
                raise RepositoryInputError(path, ValueError(f"line {number}: {exc}")) from exc
            directives.append(directive)
            rules.setdefault(path, []).extend(directive.entries)
    return rules, directives
