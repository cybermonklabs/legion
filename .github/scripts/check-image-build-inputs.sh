#!/usr/bin/env bash
# An image stage that runs a Bun build copies the repository into itself a piece at a time, so a
# build that starts to import a file outside the pieces it copies fails only when the image is
# built - and the worker image is built on a pull request only once it leaves draft, and the
# release image after the merge. The worker image's plugin stage stopped building that way once
# the contracts began to import the dashboard's pages file from packages/envoy, which that stage
# never copies. So every file such a build reads must be put in place by a COPY of its own stage,
# above the RUN that runs it.
#
# What a build reads is derived, never listed: for each row of image_build_entries.py's STAGES,
# this check runs each build the row names - `bun build <entry> --metafile=<scratch>/meta.json`,
# whose metafile's `inputs` are every module the bundler loaded, or the build script's own function
# that returns every file it read - and keeps the files inside the repository and outside
# node_modules, which `bun install` puts in place from the manifests and the lockfile
# (check-bun-workspace-manifests.sh holds those COPY lines). It then reads the stage as Docker
# would: each COPY and ADD of the stage above the step's RUN instruction, and each bind mount of
# that RUN, with the WORKDIR it runs under, puts the context files its sources match - after the
# .dockerignore that shapes the context (`<Dockerfile>.dockerignore` beside the Dockerfile, else
# the context's own) - where Docker puts them: a file source at its destination, or under it where
# the destination ends in `/` or the line names more than one source or a wildcard, and a
# directory source's contents under its destination. A file the build reads counts as copied only
# where a COPY puts it at `<root>/<its path>`, the place the build resolves it from. A COPY below
# the step, a COPY from another stage or image, and a COPY to another place count for nothing.
#
# Fails on:
#   1. a file a build reads that no COPY above its step puts in place, naming the step's line, the
#      file and the entry that reads it,
#   2. a build that does not run, leaves no metafile or report this check can read, or reads no
#      file of the repository, since that is a build of unknown inputs, never one of none,
#   3. a row whose Dockerfile, stage or step this check cannot find, or whose step matches more than
#      one RUN of its stage,
#   4. a RUN instruction of any Dockerfile of the repository that runs a Bun build - `bun build`,
#      `bun run`, `bun pm pack` (whose prepack builds), `vite build`, or Bun on a script of the
#      context - that no row's step names, so a new build stage is read the day it is written,
#   5. no row at all, since that checks nothing.
#
# What it does not read: a file a build reads by path at run time rather than imports (the
# skills pi-plugin-prepack.sh stages, which `COPY skills skills` puts in place; a JSON schema a
# generator opens), save where the build script reports it itself (claude-envoy's
# buildLegionPlugin); a file another stage writes, which counts as not copied; and how a build
# resolves what node_modules holds.
#
# Run from anywhere, after `bun install`: .github/scripts/check-image-build-inputs.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The table and the path model this check reads, beside this script in the repository it checks.
# PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import os
import posixpath
import re
import subprocess
import sys
import tempfile
from pathlib import Path

from image_build_entries import STAGES
from workflow_paths import (DockerfileUnreadable, bind_mounts, copy_operands, dockerfile_instructions,
                            dockerignore_filter, glob_regex, read_docker_file, walk)

REPO = os.path.realpath(".")
problems: list[str] = []

# A RUN instruction that runs a Bun build (rule 4): a bundler, a package script, a pack (its
# prepack builds), Vite, or Bun on a script of the context by a relative path. An absolute path is
# a bind mount's target or a file an earlier step wrote, and an install builds nothing.
BUILD_RUN = re.compile(
    r"\bbun\s+(?:build|run|pm\s+pack)\b"
    r"|\bbunx?\s+vite\b|\bvite\s+build\b"
    r"|\bbun\s+(?![-/$])[\w./-]+\.(?:[cm]?[jt]sx?)\b"
)
WILDCARD = re.compile(r"[*?\[]")


def error(path: str, message: str) -> None:
    problems.append(f"::error file={path}::{message}")


def dockerfiles(files: list[str]) -> list[str]:
    """Every Dockerfile of the repository, by the names Docker and the image workflows give them."""
    found = []
    for path in files:
        name = posixpath.basename(path)
        if name.endswith(".dockerignore"):
            continue
        if name == "Dockerfile" or name.startswith("Dockerfile.") or name.endswith(".Dockerfile"):
            found.append(path)
    return found


def instructions(path: str) -> list[dict]:
    """Each instruction of the Dockerfile (dockerfile_instructions, the grammar that
    check-image-trigger-paths.sh reads too) with its line, the stage it is in and the WORKDIR it
    runs under. A stage built FROM an earlier stage starts in that stage's WORKDIR, any other in
    `/`."""
    out = []
    stage, workdir, finals = None, "/", {}
    index = 0
    for line, command, arguments in dockerfile_instructions(read_docker_file(path)):
        if command == "FROM":
            if stage is not None:
                finals[stage] = workdir
            words = [word for word in arguments.split() if not word.startswith("--")]
            base = words[0] if words else ""
            index += 1
            stage = words[2] if len(words) >= 3 and words[1].upper() == "AS" else f"#{index}"
            workdir = finals.get(base, "/")
        elif command == "WORKDIR":
            workdir = posixpath.normpath(posixpath.join(workdir, arguments))
        out.append({"line": line, "command": command, "arguments": arguments, "stage": stage,
                    "workdir": workdir})
    return out


def copy_parts(instruction: dict, path: str):
    """(sources, destination) of a COPY or ADD that reads the context (copy_operands); None for one
    from another stage or image (`--from`), or whose form the grammar cannot read, which it
    refuses."""
    try:
        return copy_operands(instruction["arguments"])
    except DockerfileUnreadable as exc:
        error(path, f"{path}:{instruction['line']}: {instruction['command']}: {exc}")
        return None


def mounts_of(instruction: dict, path: str) -> list:
    """(source, target) of each bind mount of a RUN from the context that names its target
    (bind_mounts); a RUN whose words the grammar cannot read is refused."""
    try:
        return [(source, target) for source, target in bind_mounts(instruction["arguments"]) if target is not None]
    except DockerfileUnreadable as exc:
        error(path, f"{path}:{instruction['line']}: RUN: {exc}")
        return []


def placed(context: list[str], copies: list, root: str) -> set:
    """The repository paths that `copies` - (sources, destination, workdir, single) for each COPY,
    ADD or bind mount - put at `<root>/<path>`. `single` is a bind mount, whose target is the
    source's own place whatever its form."""
    found = set()
    for sources, destination, workdir, single in copies:
        target_dir = posixpath.normpath(posixpath.join(workdir, destination))
        into_dir = (not single) and (destination.endswith("/") or len(sources) > 1
                                      or any(WILDCARD.search(s) for s in sources))
        for source in sources:
            relative = posixpath.normpath(source.lstrip("/"))
            regex = None if relative == "." else glob_regex(relative)
            for path in context:
                if regex is None:
                    target = posixpath.join(target_dir, path)
                elif regex.fullmatch(path):
                    target = posixpath.join(target_dir, posixpath.basename(path)) if into_dir else target_dir
                else:
                    parts = path.split("/")
                    above = next((p for p in ("/".join(parts[:n]) for n in range(1, len(parts)))
                                  if regex.fullmatch(p)), None)
                    if above is None:
                        continue
                    target = posixpath.join(target_dir, posixpath.relpath(path, above))
                if target == posixpath.join(root, path):
                    found.add(path)
    return found


def scrubbed(text: str, scratch: str) -> str:
    """`text` with this run's scratch directory shown as `<scratch>`, so a refusal names no path
    that is gone by the time anyone reads it."""
    for form in sorted({scratch, os.path.realpath(scratch)}, key=len, reverse=True):
        text = text.replace(form, "<scratch>")
    return text


def kept(paths, base: str) -> list:
    """The paths, read against `base`, that are files inside the repository and outside
    node_modules, repository-relative and sorted."""
    out = set()
    for path in paths:
        if not isinstance(path, str):
            continue
        real = os.path.realpath(os.path.join(base, path))
        relative = os.path.relpath(real, REPO)
        if relative.startswith("..") or os.path.isabs(relative) or not os.path.isfile(real):
            continue
        if "node_modules" in relative.split(os.sep):
            continue
        out.add(relative.replace(os.sep, "/"))
    return sorted(out)


def build_reads(path: str, line: int, build) -> tuple[str, list] | None:
    """(what the build is, the repository files it reads), or None when this check cannot know."""
    if not isinstance(build, (tuple, list)) or len(build) != 4 or build[0] not in ("bundle", "reported"):
        error(path, f"image_build_entries.py: a build of the step at {path}:{line} is {build!r}; write it as "
                    f"(\"bundle\", directory, entry, args) or (\"reported\", directory, module, functions)")
        return None
    kind, directory, name, extra = build
    base = os.path.join(REPO, directory)
    with tempfile.TemporaryDirectory(prefix="check-image-build-inputs.") as scratch:
        if kind == "bundle":
            what = f"`bun build {name}` in {directory}"
            command = ["bun", "build", name, "--outdir", f"{scratch}/out", f"--metafile={scratch}/meta.json", *extra]
            env = None
        else:
            what = f"{', '.join(extra)} of {posixpath.join(directory, name)}"
            module = json.dumps(os.path.join(base, name))
            script = (f"const built = await import({module}); const read = [];"
                      " for (const name of JSON.parse(process.env.CHECK_IMAGE_BUILD_FUNCTIONS))"
                      " read.push(...(await built[name](`${process.env.CHECK_IMAGE_BUILD_SCRATCH}/${name}`)));"
                      " console.log(`check-image-build-inputs: ${JSON.stringify(read)}`);")
            command = ["bun", "--eval", script]
            env = {**os.environ, "CHECK_IMAGE_BUILD_FUNCTIONS": json.dumps(list(extra)),
                   "CHECK_IMAGE_BUILD_SCRATCH": scratch}
        result = subprocess.run(command, cwd=base, capture_output=True, text=True, env=env)
        said = scrubbed((result.stderr.strip() or result.stdout.strip())[-1500:], scratch)
        if result.returncode != 0:
            error(path, f"{path}:{line}: {what} exited {result.returncode}, so what the step reads is unknown: "
                        f"{said}")
            return None
        try:
            if kind == "bundle":
                inputs = list(json.loads((Path(scratch) / "meta.json").read_text())["inputs"])
            else:
                lines = [l for l in result.stdout.splitlines() if l.startswith("check-image-build-inputs: ")]
                inputs = json.loads(lines[-1].removeprefix("check-image-build-inputs: "))
        except (OSError, ValueError, KeyError, TypeError, IndexError) as exc:
            error(path, f"{path}:{line}: {what} exited 0 and left nothing this check can read "
                        f"({type(exc).__name__}: {scrubbed(str(exc), scratch)}), so what the step reads is unknown")
            return None
    files = kept(inputs, base)
    if not files:
        error(path, f"{path}:{line}: {what} read no file of the repository outside node_modules; a build reads "
                    f"at least its entry, so this check could see nothing")
        return None
    return what, files


files = walk()
read_cache: dict[str, list] = {}


def parsed(path: str) -> list:
    if path not in read_cache:
        read_cache[path] = instructions(path)
    return read_cache[path]


if not STAGES:
    error("image_build_entries.py", "image_build_entries.py's STAGES names no step, so this check reads no build")

claimed: dict[tuple[str, int], str] = {}
steps = builds_read = files_read = 0
for row in STAGES:
    try:
        dockerfile, stage, root, step, builds = row
    except (TypeError, ValueError):
        error("image_build_entries.py", f"image_build_entries.py: a row of STAGES is {row!r}; write it as "
                                        f"(dockerfile, stage, root, step, builds)")
        continue
    if not Path(dockerfile).is_file():
        error(dockerfile, f"image_build_entries.py names {dockerfile}, which is no file; correct the row")
        continue
    lines = parsed(dockerfile)
    in_stage = [i for i in lines if i["stage"] == stage]
    if not in_stage:
        error(dockerfile, f"image_build_entries.py names stage {stage} of {dockerfile}, which has no such stage")
        continue
    runs = [i for i in in_stage if i["command"] == "RUN" and step in i["arguments"]]
    if len(runs) != 1:
        lines_named = ", ".join(str(i["line"]) for i in runs) or "none"
        error(dockerfile, f"image_build_entries.py's step {step!r} of stage {stage} of {dockerfile} matches "
                          f"{len(runs)} RUN instructions (lines {lines_named}); give words that only its one RUN holds")
        continue
    run = runs[0]
    claimed[(dockerfile, run["line"])] = step
    specific = f"{dockerfile}.dockerignore"
    ignored = dockerignore_filter(specific if Path(specific).is_file() else
                                  (".dockerignore" if Path(".dockerignore").is_file() else None))
    context = [path for path in files if not ignored(path)]
    copies = []
    for instruction in in_stage:
        if instruction["line"] >= run["line"]:
            break
        if instruction["command"] in ("COPY", "ADD"):
            parts = copy_parts(instruction, dockerfile)
            if parts is not None and parts[0]:
                copies.append((parts[0], parts[1], instruction["workdir"], False))
    for source, target in mounts_of(run, dockerfile):
        copies.append(([source], target, run["workdir"], True))
    there = placed(context, copies, root)
    steps += 1
    for build in builds:
        reads = build_reads(dockerfile, run["line"], build)
        if reads is None:
            continue
        what, read = reads
        builds_read += 1
        files_read += len(read)
        missing = [path for path in read if path not in there]
        for path in missing[:20]:
            error(dockerfile, f"{dockerfile}:{run['line']}: stage {stage}'s step ({step}) runs {what}, which reads "
                              f"{path}, and no COPY above line {run['line']} puts it at {posixpath.join(root, path)}; "
                              f"copy it there before the step")
        if len(missing) > 20:
            error(dockerfile, f"{dockerfile}:{run['line']}: and {len(missing) - 20} more files {what} reads")
        print(f"{dockerfile}:{run['line']}: stage {stage}, {what}: {len(read)} files read, "
              f"{len(read) - len(missing)} copied above the step")

for dockerfile in dockerfiles(files):
    for instruction in parsed(dockerfile):
        if instruction["command"] != "RUN" or not BUILD_RUN.search(instruction["arguments"]):
            continue
        if (dockerfile, instruction["line"]) not in claimed:
            error(dockerfile, f"{dockerfile}:{instruction['line']}: stage {instruction['stage']} runs a Bun build "
                              f"that no row of image_build_entries.py's STAGES names, so what it reads is never "
                              f"held to the stage's COPY lines; add a row for it")

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(f"::error::{len(problems)} problem(s) across {steps} image build step(s)", file=sys.stderr)
    raise SystemExit(1)
print(f"every image build step copies every file its builds read: {steps} steps, {builds_read} builds, "
      f"{files_read} files read")
PY
