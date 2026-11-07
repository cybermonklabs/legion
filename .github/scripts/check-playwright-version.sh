#!/usr/bin/env bash
# Playwright is a client and a set of browsers built to answer it, and the two are one release:
# the browsers every CI job that can launch WebKit or Firefox drives are baked into the browser
# image (.github/docker/browser.Dockerfile), while the client that drives them is the Playwright
# bun.lock resolves. Nothing makes those two agree by construction — the image's Playwright is
# its base image's, and bun.lock's is the manifests' — so a bump of one without the other is a
# head where every browser job either fails on a protocol the other end does not speak, or, when
# the two are close enough to talk, passes while the suite drives browsers it was never run
# against. This check fails until the two name one release.
#
# Both versions are read out of the tree. No registry is called and nothing on the network is
# reached: the image's version is its base image's row in .github/ci-images.yaml, which is the
# one place the tree says which image a digest is a copy of, exactly as
# .github/scripts/check-bun-version.sh reads a Bun base's row. This check therefore says that the
# two strings agree; that the published bytes carry that Playwright is the image's own build.
#
# A tree holding no .github/docker/browser.Dockerfile passes first of all, saying so: no CI
# image carries Playwright's browsers there, so nothing can disagree with the lockfile and
# there is no base to read. A tree that holds the Dockerfile and no row for its base is rule 4's
# refusal, not that pass. Otherwise it fails, in code order:
#
#   1. bun.lock missing, or resolving no `playwright` — the client every suite imports,
#   2. .github/docker/browser.Dockerfile unreadable as text, or holding no base `FROM`,
#   3. that base `FROM` naming no digest, so no row can be found for it,
#   4. that digest held by no row of .github/ci-images.yaml, which that file missing or
#      unparseable also reaches,
#   5. that row carrying no `upstream:` tag a Playwright release reads out of — a row carries
#      exactly one of `upstream:` and `built:`, and an image this repository builds is a copy of
#      no published Playwright,
#   6. the two releases disagreeing.
#
# Every problem is printed with the file and line to edit, and the only non-zero exit is a named
# problem.
#
# Run from anywhere: .github/scripts/check-playwright-version.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The reference grammar is .github/scripts/ci_images.py's, beside this script in the repository
# it checks, which PYTHONPATH names: the digest this gate reads out of a FROM is the digest
# .github/scripts/check-ci-images.sh holds to a row, and the upstream tag it reads is the one
# that check reads a row's repository with. PYTHONDONTWRITEBYTECODE keeps the import from
# leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import re
import sys
from pathlib import Path

import yaml

from ci_images import split_reference, upstream_parts


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where it was written. The lines are attributes, not keys:
    the file is never searched for a key the loader invented, so a row holding a field named
    `__line__` is read like any other."""

    line: int
    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    # Also each key's own line, so a problem about one field of a row is reported where the
    # reader has to edit rather than at the row's first line.
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


def line_of(value: object) -> int:
    return getattr(value, "line", 0)


def key_line(mapping: object, name: str) -> int:
    return getattr(mapping, "lines", {}).get(name, line_of(mapping))


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

# The image every CI job that can launch a browser runs in, and the file that says which image
# each digest of this repository's own package is a copy of. Every image a CI job fetches is a
# copy in that package, named by digest, so the Dockerfile's base names a digest and no version:
# the row that digest belongs to is what says which Playwright it is.
DOCKERFILE = Path(".github/docker/browser.Dockerfile")
CI_IMAGES = Path(".github/ci-images.yaml")
MIRROR_REGISTRY = "ghcr.io/cybermonklabs/legion/ci"
LOCKFILE = Path("bun.lock")
# `bun install` writes one entry per resolved package, as
# `"playwright": ["playwright@1.64.1", …]`. The name is anchored to the start of its line, so a
# workspace-scoped entry (`"packages/x/playwright"`) is a different package and not this one.
LOCK_ENTRY = re.compile(r'^\s*"playwright":\s*\["playwright@([^"]+)"')
# Playwright's own image is published as v<X.Y.Z>-<variant>, noble today. The leading `v` and
# the variant are stripped by shape rather than by name, so a later base on another variant
# still reads.
TAG_RELEASE = re.compile(r"v?(\d+\.\d+\.\d+)(?:-\S+)?")

problems: list[str] = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: it would otherwise reach the Actions
    log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def at(path: Path, line: int, message: str) -> str:
    """A problem GitHub annotates with the file and line to edit, and that reads the same in a
    terminal as every other check's `path:line:` does."""
    return f"::error file={path},line={line}::{path}:{line}: {message}"


def read_text(path: Path) -> str | None:
    """None when the file cannot be decoded — named like any other problem, not raised. UTF-8
    explicitly: the gate must not depend on the runner's or a developer's locale."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as error:
        problems.append(f"::error file={path}::{path} is not readable as text: {one_line(error)}")
        return None


def joined_lines(text: str):
    """(line number, logical line) with Dockerfile backslash continuations joined, so a FROM
    split across lines is judged whole and reported at the line it starts on."""
    pending, start = "", 0
    for number, raw in enumerate(text.splitlines(), start=1):
        stripped = raw.strip()
        if not pending:
            start = number
        # Docker removes a comment line inside a continued instruction and carries on, and a
        # comment's own trailing backslash continues nothing.
        if stripped.startswith("#"):
            continue
        if stripped.endswith("\\"):
            pending += stripped[:-1] + " "
            continue
        yield start, (pending + stripped).strip()
        pending = ""
    if pending:
        yield start, pending.strip()


def base_from(instructions) -> tuple[int, str] | None:
    """(line, image) of the Dockerfile's base FROM: the one the built image is based on, or None
    where the file declares none.

    Decided by what each instruction says, not by where it sits. A `FROM <image> AS <name>`
    opens a named build stage, which exists only for what a later instruction copies out of it —
    .github/docker/browser.Dockerfile's first FROM is the `weed` stage, whose whole purpose is
    one `COPY --from=weed` — so the image it names is not what the built image is based on. The
    base is the FROM that opens no named stage, and the last of them where a file holds more
    than one, since Docker builds the last stage of a Dockerfile. A file that gains a stage above
    or below the base therefore still reads.
    """
    base = None
    for line, instruction in instructions:
        words = instruction.split()
        if not words or words[0].upper() != "FROM":
            continue
        # A flag (`--platform=…`) is not the image, and `AS <name>` follows it.
        rest = [word for word in words[1:] if not word.startswith("--")]
        if not rest or (len(rest) > 2 and rest[1].upper() == "AS"):
            continue
        base = (line, rest[0])
    return base


def mirror_rows() -> dict[str, Mapping]:
    """digest -> row, from CI_IMAGES. Read only when a base FROM names a digest: nothing else in
    this gate needs the file."""
    rows: dict[str, Mapping] = {}
    if not CI_IMAGES.is_file():
        problems.append(
            f"::error file={CI_IMAGES}::{CI_IMAGES} is missing: it is the one file that says "
            f"which image each {MIRROR_REGISTRY} digest is a copy of, and so which Playwright "
            f"the browser image carries"
        )
        return rows
    content = read_text(CI_IMAGES)
    if content is None:
        return rows
    try:
        document = yaml.load(content, Loader=LineLoader)
    except yaml.YAMLError as error:
        # One named line, never a traceback: this gate runs on every pull request, and a file it
        # cannot parse is that file's problem to report, not a crash for everyone.
        problems.append(f"::error file={CI_IMAGES}::{CI_IMAGES} is not valid YAML: {one_line(error)}")
        return rows
    listed = document.get("images") if isinstance(document, dict) else None
    for row in listed or []:
        if isinstance(row, dict) and isinstance(row.get("digest"), str):
            rows[row["digest"]] = row
    return rows


# ---------------------------------------------------------------- nothing to hold together
if not DOCKERFILE.is_file():
    # A tree that has not got the browser image yet carries Playwright's browsers in no CI
    # image, so there is nothing for the lockfile to disagree with and nothing below to read. A
    # tree that holds the Dockerfile and no row for its base is a refusal, not this pass.
    print(f"no {DOCKERFILE}: no CI image carries Playwright's browsers, so none can disagree with {LOCKFILE}")
    raise SystemExit(0)

# ---------------------------------------------------------------- what bun.lock resolves
lock_line, lock_version = 0, ""
if not LOCKFILE.is_file():
    problems.append(
        f"::error file={LOCKFILE}::{LOCKFILE} is missing: it is where the Playwright every "
        f"browser suite drives is resolved"
    )
else:
    lock_text = read_text(LOCKFILE)
    for number, text in enumerate((lock_text or "").splitlines(), start=1):
        resolved = LOCK_ENTRY.match(text)
        if resolved:
            lock_line, lock_version = number, resolved.group(1)
            break
    if lock_text is not None and not lock_version:
        problems.append(
            f'::error file={LOCKFILE}::{LOCKFILE} resolves no playwright: the entry reads '
            f'"playwright": ["playwright@<version>", …], and without it nothing here says which '
            f"Playwright the suites drive"
        )

# ---------------------------------------------------------------- what the image carries
image_version, upstream, upstream_line, from_line = "", "", 0, 0
dockerfile_text = read_text(DOCKERFILE)
base = base_from(joined_lines(dockerfile_text)) if dockerfile_text is not None else None
if dockerfile_text is not None and base is None:
    problems.append(
        f"::error file={DOCKERFILE}::{DOCKERFILE} holds no base FROM: every FROM in it opens a "
        f"named build stage (AS <name>), so what the built image is based on cannot be read here"
    )
elif base is not None:
    from_line, image = base
    parts = split_reference(image)
    digest = parts[3] if parts else ""
    if not digest:
        problems.append(at(
            DOCKERFILE, from_line,
            f"the base FROM is {one_line(image)}, which names no digest, so no row of "
            f"{CI_IMAGES} says which Playwright it carries — write "
            f"{MIRROR_REGISTRY}@<digest>, the digest of its row",
        ))
    else:
        row = mirror_rows().get(digest)
        if row is None:
            problems.append(at(
                DOCKERFILE, from_line,
                f"the base FROM names {digest}, which no row of {CI_IMAGES} holds — that row's "
                f"upstream tag is the one place the tree says which Playwright this image "
                f"carries, and {LOCKFILE}:{lock_line} resolves "
                f"playwright@{lock_version or '<none>'}",
            ))
        # `upstream:` and `built:` are the two row kinds, spelled as
        # .github/scripts/check-ci-images.sh spells them; that check owns the rule that a row
        # carries exactly one of the two, and this one only reads which kind it got. A row
        # carrying neither key reaches here too, since the rule refusing it is that check's, so
        # the message says so rather than quoting a `built:` the row has not got. The line is
        # the row's `built:` key, as every other field-level message of this gate names the key
        # it is about; key_line falls back to the row's own line when there is no such key.
        elif not isinstance(row.get("upstream"), str):
            problems.append(at(
                CI_IMAGES, key_line(row, "built"),
                f"the row {DOCKERFILE}:{from_line} builds on carries "
                f"built: {one_line(row.get('built') or '<neither key>')} and no upstream: — an "
                f"image this repository builds is a copy of no published Playwright, so nothing "
                f"here says which release it carries, and {LOCKFILE}:{lock_line} resolves "
                f"playwright@{lock_version or '<none>'}",
            ))
        else:
            upstream = row["upstream"]
            upstream_line = key_line(row, "upstream")
            tag = upstream_parts(upstream)[2]
            release = TAG_RELEASE.fullmatch(tag)
            if not release:
                problems.append(at(
                    CI_IMAGES, upstream_line,
                    f"the row {DOCKERFILE}:{from_line} builds on is a copy of "
                    f"{one_line(upstream)}, whose tag {one_line(tag) or '<none>'} carries no "
                    f"Playwright release — Playwright publishes its image as "
                    f"v<X.Y.Z>-<variant>, and this tag is where the image's version is read",
                ))
            else:
                image_version = release.group(1)

# ---------------------------------------------------------------- the two, held together
if image_version and lock_version and image_version != lock_version:
    problems.append(at(
        LOCKFILE, lock_line,
        f"{LOCKFILE} resolves playwright@{lock_version}, and the browser image carries "
        f"Playwright {image_version}: {CI_IMAGES}:{upstream_line} says the base FROM at "
        f"{DOCKERFILE}:{from_line} is a copy of {one_line(upstream)}. A client and the browsers "
        f"it drives are one release — raise both together: the new Playwright in every manifest "
        f"with a `bun install`, and that row and this base at the image's new digest",
    ))

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} place(s) disagree about which Playwright this repository runs",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(
    f"{LOCKFILE}:{lock_line} resolves playwright@{lock_version} and the browser image carries "
    f"Playwright {image_version} ({CI_IMAGES}:{upstream_line}, the copy of {upstream} "
    f"{DOCKERFILE}:{from_line} builds on): they agree"
)
PY
