"""The TypeScript half of what a Bun job reads, listed by the tools that compile and load it.

ci_ts_programs.py pairs each changes filter with the tsc programs its job typechecks (PROGRAMS)
and the entries its job runs with Bun (BUNDLES). This module lists them: `bunx tsc -p <project>
--noEmit --listFilesOnly` for a program, and the `inputs` of the metafile `bun build <entry>
--target bun --metafile` writes for a bundle, each kept to the files inside the repository and
outside node_modules. Both need the workspace's node_modules, which `bun install` writes.

Two readers use it: the changes job's filter generation (`check-ci-filters.sh generate`), which
writes the files of each LISTED tree into that filter's listed block, and check-ts-filters.sh, which
holds every filter to the files its programs and bundles give. A source that cannot be listed is a
refusal naming the command and what it printed, never a source of no files, which every filter
would cover.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from ci_filter_coverage import REPO, error

# The files a program lists that no commit holds, each with the committed files it is rendered
# from: git ignores the file and every `bun install` writes it (the root package.json's
# postinstall), so no change ever carries it, and a filter holding it would select nothing. What
# changes it is a change to its sources, so the listing gives those in its place: the changes job's
# generation and check-ts-filters.sh hold a program's filter to them, and a listed block lists the
# sources under its tree, never the file. Each entry is generated file: (source, ...), every path
# repository-relative.
RENDERED = {
    "packages/contracts/src/dispatch-api.generated.ts": (
        "packages/contracts/schemas/dispatch-openapi.json",
        "packages/contracts/scripts/gen-api.ts",
    ),
}

# The files of this repository the listings resolve with besides the sources they list, which a
# change can edit to move a listing while none of the files it lists changes: each program's
# tsconfig and every tsconfig it extends or references, each bundle's nearest tsconfig and its
# chain, the package.json nearest each listed file (its `exports` and `imports` decide what an
# import of that package resolves to), the root package.json, bun.lock and bunfig.toml. Filled as
# the sources are listed; the generation hands it on with its own inputs (engine_inputs).
RESOLVED: set = set()
# A JSON string, kept whole, or a comment, dropped: a `//` inside a string ("https://...") is no comment.
JSON_COMMENT = re.compile(r'("(?:\\.|[^"\\])*")|//[^\n]*|/\*.*?\*/', re.S)
TRAILING_COMMA = re.compile(r",(\s*[}\]])")


def relative(path: Path) -> str | None:
    try:
        return path.resolve().relative_to(REPO).as_posix()
    except ValueError:
        return None


def tsconfig_chain(config: Path) -> None:
    """`config` and every tsconfig of this repository it extends or references, into RESOLVED. A
    file that cannot be read as JSON with comments is still recorded, its edges unknown."""
    path = relative(config)
    if path is None or path in RESOLVED or not config.is_file():
        return
    RESOLVED.add(path)
    try:
        text = JSON_COMMENT.sub(lambda m: m.group(1) or "", config.read_text())
        data = json.loads(TRAILING_COMMA.sub(r"\1", text))
    except (OSError, ValueError):
        return
    extends = data.get("extends") or []
    targets = [extends] if isinstance(extends, str) else list(extends)
    targets += [ref.get("path", "") for ref in data.get("references") or [] if isinstance(ref, dict)]
    for target in targets:
        if not isinstance(target, str) or not target.startswith("."):
            continue  # a package's tsconfig, which the lockfile pins
        candidate = (config.parent / target).resolve()
        if candidate.is_dir():
            candidate = candidate / "tsconfig.json"
        elif candidate.suffix != ".json":
            candidate = candidate.with_name(candidate.name + ".json")
        tsconfig_chain(candidate)


def nearest(start: Path, name: str) -> Path | None:
    """The file `name` in `start` or the nearest directory above it, inside the repository."""
    for directory in [start, *start.parents]:
        if (directory / name).is_file():
            return directory / name
        if directory == REPO:
            return None
    return None


def record_resolution(kind: str, path: str, files: list) -> None:
    """What the listing of one program or bundle resolved with, into RESOLVED. tsc resolves a
    program with that program's one tsconfig and its chain; bun resolves each module of a bundle
    with the tsconfig nearest that module, so a bundle records the chain nearest every module its
    metafile lists (`files`), the entry among them. Each listed file adds the package.json nearest
    it, whose `exports` and `imports` decide what an import of that package resolves to."""
    source = REPO / path
    if kind == "program":
        tsconfig_chain(source / "tsconfig.json" if source.is_dir() else source)
    for listed in [path, *files] if kind == "bundle" else files:
        directory = (REPO / listed).parent
        if kind == "bundle":
            config = nearest(directory, "tsconfig.json")
            if config:
                tsconfig_chain(config)
        manifest = nearest(directory, "package.json")
        if manifest and relative(manifest):
            RESOLVED.add(relative(manifest))
    for name in ("package.json", "bun.lock", "bunfig.toml"):
        if (REPO / name).is_file():
            RESOLVED.add(name)


def kept(paths) -> list:
    """The paths inside the repository and outside node_modules, repository-relative and sorted.
    A relative path is read against the repository root, which is where both commands run and
    what each prints a relative path against. A file no commit holds, which every `bun install`
    renders, stands for the committed files it is rendered from (RENDERED)."""
    files = set()
    for line in paths:
        if not line.strip():
            continue
        path = Path(line.strip())
        if not path.is_absolute():
            path = REPO / path
        try:
            relative = path.resolve().relative_to(REPO)
        except ValueError:  # tsc's own lib files and anything else outside this repository
            continue
        if "node_modules" in relative.parts:
            continue
        files.update(RENDERED.get(relative.as_posix(), (relative.as_posix(),)))
    return sorted(files)


def scrubbed(text: str, scratch: str) -> str:
    """`text` with the scratch directory, as it was given and as it resolves, shown as
    `<scratch>`: the directory is this run's own and gone when it ends, so a refusal naming it
    would name a path nobody can read and read differently on every run. The longer form is
    replaced first, so neither leaves part of the other behind."""
    for form in sorted({scratch, os.path.realpath(scratch)} if scratch else (), key=len, reverse=True):
        text = text.replace(form, "<scratch>")
    return text


def failed(path: str, command: str, result, holds: str, problems: list, scratch: str = "") -> None:
    """The refusal of a command that could not list a source's files. The source is then
    unknown, which is a failure of its own rather than a source of no files that every filter
    covers. A build passes the scratch directory it gave Bun, which what Bun printed then shows as
    `<scratch>` (scrubbed)."""
    output = " / ".join(scrubbed(result.stdout + result.stderr, scratch).split()) or "no output"
    problems.append(error(path, f"`{command}` failed (exit {result.returncode}), so the files its {holds} "
                                f"are unknown: {output[:500]}"))


def listed_paths(text: str) -> list:
    """The paths tsc's --listFilesOnly printed, one to a line. Where the repository root holds a
    newline, as a test's fixture repository under a TMPDIR holding one does, splitting at each
    newline would cut every path inside it in two, so the root is set aside as a NUL, which no path
    holds, while the text is split, and put back in each line after."""
    root = str(REPO)
    return [line.replace("\0", root) for line in text.replace(root, "\0").splitlines()]


def program_files(project: str, problems: list) -> list | None:
    """Every file of the project's tsc program that is inside the repository and outside
    node_modules, repository-relative. None when the command fails."""
    command = ["bunx", "tsc", "-p", project, "--noEmit", "--listFilesOnly"]
    result = subprocess.run(command, cwd=REPO, capture_output=True, text=True)
    if result.returncode != 0:
        failed(project, " ".join(command), result, "program holds", problems)
        return None
    return kept(listed_paths(result.stdout))


def bundle_files(entry: str, problems: list) -> list | None:
    """Every module `bun build` loads for the entry - the `inputs` of the metafile it writes,
    each a path relative to the directory Bun runs in - that is inside the repository and outside
    node_modules, repository-relative. The bundle and the metafile go to a scratch directory of
    this run's own, removed with them, and a refusal shows that directory as `<scratch>` - in the
    command, in what Bun printed and in the error a metafile it cannot read raised - so it reads
    the same on every run. None when the build fails, leaves no metafile to read, or leaves one
    whose `inputs` name no such module: a bundle loads at least its entry, so that metafile is no
    answer, and the empty set read from it would be one every filter covers."""
    with tempfile.TemporaryDirectory(prefix="ci-ts-listing.") as scratch:
        command = ["bun", "build", entry, "--target", "bun", "--outdir", scratch, f"--metafile={scratch}/meta.json"]
        shown = scrubbed(" ".join(command), scratch)
        result = subprocess.run(command, cwd=REPO, capture_output=True, text=True)
        if result.returncode != 0:
            failed(entry, shown, result, "bundle loads", problems, scratch)
            return None
        try:
            inputs = list(json.loads((Path(scratch) / "meta.json").read_text())["inputs"])
        except (OSError, ValueError, KeyError, TypeError) as exc:
            if isinstance(exc, OSError) and exc.filename:
                # The name scrubbed before Python quotes it: a name holding ' is quoted with ", so
                # quoting the raw name would make the refusal's text depend on TMPDIR.
                exc = type(exc)(exc.errno, exc.strerror, scrubbed(exc.filename, scratch))
            problems.append(error(entry, f"`{shown}` exited 0 and left no metafile whose `inputs` this check "
                                         f"can read ({type(exc).__name__}: {scrubbed(str(exc), scratch)}), so "
                                         f"the files its bundle loads are unknown"))
            return None
    files = kept(inputs)
    if not files:
        problems.append(error(entry, f"`{shown}` exited 0 and the `inputs` of its metafile name {len(inputs)} "
                                     f"module(s), none of them a file of this repository outside node_modules, "
                                     f"so the files its bundle loads are unknown: a bundle loads at least its "
                                     f"entry"))
        return None
    return files


def source_files(kind: str, path: str, problems: list) -> list | None:
    """A program's files or a bundle's, by the kind ci_ts_programs.py pairs it as, with what the
    listing resolved them with recorded in RESOLVED."""
    files = program_files(path, problems) if kind == "program" else bundle_files(path, problems)
    record_resolution(kind, path, files or [])
    return files


def pairs() -> list:
    """Every pair of the two tables as (kind, filter, path, why)."""
    from ci_ts_programs import BUNDLES, PROGRAMS
    return ([("program", name, project, why) for name, project, why in PROGRAMS]
            + [("bundle", name, entry, why) for name, entry, why in BUNDLES])


def listed_alone(kind: str, path: str) -> tuple:
    """source_files of one program or bundle, with the refusals it recorded."""
    refused: list = []
    return source_files(kind, path, refused), refused


def listed_files(filters, problems: list) -> dict:
    """Per LISTED entry of ci_ts_programs.py whose filter is in `filters`, the files of its tree
    that the programs and bundles paired with its sources give, sorted: the filter's listed block.
    A source that cannot be listed leaves the entry out with a refusal recorded, so a caller that
    finds a problem knows the block is unknown. The sources are listed at once, each tsc or Bun
    run on a processor of its own, and each one's refusals join `problems` where a listing one
    after another would have put them."""
    from ci_ts_programs import LISTED
    asked = dict.fromkeys((kind, path) for name, _tree, sources, _why in LISTED if name in filters
                          for source in sources for kind, pair, path, _ in pairs()
                          if pair == source and (REPO / path).exists())
    with ThreadPoolExecutor(max_workers=max(1, len(asked))) as pool:
        answers = {asks: pool.submit(listed_alone, *asks) for asks in asked}
    files_of: dict = {}
    blocks: dict = {}
    for name, tree, sources, _why in LISTED:
        if name not in filters:
            continue
        found, known = set(), bool(sources)
        for source in sources:
            sourced = [(kind, path) for kind, pair, path, _ in pairs() if pair == source]
            if not sourced:
                problems.append(error(".github/scripts/ci_ts_programs.py",
                                      f"LISTED takes filter {name}'s files under {tree} from filter {source}, which "
                                      f"PROGRAMS pairs with no tsc program and BUNDLES with no bundle"))
                known = False
            for kind, path in sourced:
                if (kind, path) not in files_of:
                    if (kind, path) in answers:
                        files_of[kind, path], refused = answers[kind, path].result()
                        problems += refused
                    else:
                        problems.append(error(".github/scripts/ci_ts_programs.py",
                                              f"{path} is no path of this repository, so filter {source}'s {kind} "
                                              f"gives filter {name}'s block nothing"))
                        files_of[kind, path] = None
                files = files_of[kind, path]
                if files is None:
                    known = False
                    continue
                found.update(f for f in files if f.startswith(f"{tree}/"))
        if known:
            blocks[name] = sorted(found)
    return blocks
