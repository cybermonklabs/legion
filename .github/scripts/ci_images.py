"""How this repository reads .github/ci-images.yaml: the grammar of a container image
reference, the repository a row names with Docker Hub's implicit `library/` written out, the
kind of row it is, the tag that kind and its digest give it, and, for a row this repository
builds, the hash of the whole recipe its image is built from.

check-ci-images.sh, mirror-ci-images.sh, check-bun-version.sh and check-playwright-version.sh,
and the `row` and `record` steps of .github/workflows/browser-image.yaml, import it from the
repository they work on, running `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3`
from that repository's root. The `row` step reads which row is built from the Dockerfile and
hashes the whole build recipe with recipe_sha256, the hash the gate's rule 10 reads back out of
the published image's label; the `record` step takes that image's tag from row_tag. One
module, because the gate and the copy must read a row the same way: a row the gate counts as the
library/golang row and the copy did not would escape the copy's Go check, and a row one reader
counts as built and another as copied would be `crane copy`'d from a Dockerfile or held to an
upstream tag it has not got.
"""

import hashlib
import re
from pathlib import Path, PurePosixPath

# ---------------------------------------------------------------- reference grammar
HOST = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?")
COMPONENT = re.compile(r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*")
TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9._-]{0,127}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
# The host spellings of Docker Hub, where a repository of one path element is under `library/`.
DOCKER_HUB_HOSTS = {"", "docker.io", "index.docker.io", "registry-1.docker.io"}


def split_reference(text: str) -> tuple[str, str, str, str] | None:
    """(host, path, tag, digest) for a string shaped like an image reference, else None. `host`
    and `tag` and `digest` are "" where the reference carries none."""
    if not text or any(character.isspace() for character in text) or "//" in text:
        return None
    body, at, digest = text.partition("@")
    if at and not DIGEST.fullmatch(digest):
        return None
    head, slash, last = body.rpartition("/")
    name, tag = body, ""
    if ":" in last:
        base, _, tag = last.partition(":")
        name = head + slash + base
        if not TAG.fullmatch(tag):
            return None
    parts = name.split("/")
    host = ""
    if len(parts) > 1 and ("." in parts[0] or ":" in parts[0] or parts[0] == "localhost"):
        host, parts = parts[0], parts[1:]
        if not HOST.fullmatch(host):
            return None
    if not parts or not all(COMPONENT.fullmatch(part) for part in parts):
        return None
    return host, "/".join(parts), tag, digest


def upstream_parts(upstream: str) -> tuple[str, str, str, str]:
    """(host, path, tag, digest) of a row's upstream, each "" where the reference carries none.

    `path` carries Docker Hub's implicit `library/`, which a reference may leave out:
    docker.io/golang and docker.io/library/golang are one image, and a rule that compared the
    text of the row would read the first as a repository no pin of this repository names. Every
    rule that compares a row's repository reads it from here, so both spellings answer alike."""
    host, path, tag, digest = split_reference(upstream) or ("", "", "", "")
    if path and host in DOCKER_HUB_HOSTS and "/" not in path:
        path = f"library/{path}"
    return host, path, tag, digest


# ---------------------------------------------------------------- a row's kind, and its tag
# The two kinds of row, each saying where the row's bytes came from: a row carrying `upstream:`
# is a copy .github/scripts/mirror-ci-images.sh makes with `crane copy`, and a row carrying
# `built:` is an image this repository builds from the Dockerfile that key names, which
# .github/workflows/browser-image.yaml builds and pushes. A row carries exactly one of them.
UPSTREAM = "upstream"
BUILT = "built"


def row_kind(row: object) -> tuple[str, str] | None:
    """(kind, source) of a row: UPSTREAM and the reference it is copied from, or BUILT and the
    Dockerfile it is built from, each stripped of the space around it. None for a row that says
    neither: one that is not a mapping, one carrying both keys or neither, and one whose key is
    not a non-empty string.

    Every reader of a row's kind reads it here, because a row one reader counts as built and
    another as copied escapes a rule: the copy would ask `crane copy` for a Dockerfile, or a
    gate would read a Playwright release out of an upstream tag a built row has not got. Which
    of those shapes a None row is remains check-ci-images.sh's to say, in the message its rule 1
    prints; every other reader only needs the kind it got."""
    if not isinstance(row, dict):
        return None
    upstream, built = row.get(UPSTREAM), row.get(BUILT)
    # A row with both says two different things about one digest, and a row with neither says
    # nothing about it at all; both are the same answer here.
    if (upstream is None) == (built is None):
        return None
    kind, value = (BUILT, built) if built is not None else (UPSTREAM, upstream)
    if not isinstance(value, str) or not value.strip():
        return None
    return kind, value.strip()


def row_tag(kind: str, source: str, digest: str) -> str:
    """The tag the rule gives a row of that kind, source and digest. For a row the mirror
    copies: the upstream repository's last path element, then the upstream tag when the row
    names one, then the first eight hex characters of the digest. For a row this repository
    builds: the stem of the Dockerfile that builds it, which is its name without the
    .Dockerfile suffix, then those same eight characters.

    The rule is here and not in its gate because the build writes the tag and the gate holds the
    row to it: .github/workflows/browser-image.yaml tags what it pushed with what this returns,
    and check-ci-images.sh refuses a row whose tag is not what this returns, so a second copy of
    the rule would be a tag the build writes and the gate refuses. The eight characters are what
    keep one tag on one digest."""
    short = digest.partition(":")[2][:8]
    if kind == BUILT:
        # A repository path, so PurePosixPath and not Path: the separator is the one the row is
        # written with, whatever the reader's platform.
        return f"{PurePosixPath(source).stem}-{short}"
    _, path, carried, _ = upstream_parts(source)
    parts = [part for part in (path.rpartition("/")[2], carried, short) if part]
    return "-".join(parts)


# ---------------------------------------------------------------- a built row's build recipe
def recipe_sha256(dockerfile: str, workflow: str) -> str:
    """The sha256 of the whole recipe a `built:` row's image is built from: the Dockerfile's own
    path, every file of the build context directory, which is the directory that Dockerfile sits
    in, and the workflow file that runs the build. Both paths are relative to the repository
    root, which is where every reader of this runs.

    Each file is folded in as its path and the sha256 of its bytes, in the order those paths
    sort, so a byte changed in any of them moves the hash, and so do a file added to the
    context and a file renamed inside it — both of which change what the build receives while
    every file it reads today stays as it was.

    The Dockerfile's path is folded in ahead of them because the context is the directory and
    not the file: two rows naming two Dockerfiles of one directory read the same files, so
    without that path they would hash alike, and a row built from the other file of
    .github/docker would carry the hash the browser image's own label holds — rule 10 of
    .github/scripts/check-ci-images.sh would pass it on that label, and rule 8, which refuses
    such a row in lint because rule 10 refuses it over the network, would be resting on a
    refusal that never came. The build is told which file to build (the build step's `file:`),
    so that name is as much of the recipe as the bytes it names.

    That paragraph is the one place this repository argues for the path being in the hash.
    Rule 8 and rule 10 of .github/scripts/check-ci-images.sh, the BUILT_DOCKERFILE comment
    beside them, the comment over rule 8's loop and rule 8's own refusal message each say that
    the path is in the hash and point here for why, so the argument is changed in one place.

    All three are in it because all three decide the bytes that get pushed. The Dockerfile and
    every other file of the context, because the build reads them: a `COPY` takes a context
    file into the image, and a context file no `COPY` names today is one the next edit of the
    Dockerfile can name. The workflow, because the build is more than its Dockerfile: its
    `labels:`, `outputs:`, `platforms:`, `provenance:` and build-args, the pin of the action
    that runs the build and the pin of the BuildKit image it runs in each decide what the push
    leaves in the package while no file of the context has moved.

    One function, and three readers of it: .github/workflows/browser-image.yaml hashes the
    recipe to decide whether this head's image is the published one, stamps the hash it got
    into the image it pushes as the legion.browser-image.source-sha256 label, and rule 10 of
    .github/scripts/check-ci-images.sh reads that label back out of the published image and
    holds it equal to this same hash. Two copies of the rule would differ on the first change
    neither reader covered alike: the build would push an image its own gate then refuses, or
    the gate would pass a row whose image was built from something else.

    A tree holding no such Dockerfile raises rather than hashing what is there: the walk of a
    directory that is not there yields nothing, and the hash of nothing is a perfectly good
    hash of the wrong recipe."""
    source = Path(dockerfile)
    if not source.is_file():
        raise FileNotFoundError(
            f"{dockerfile} is not a file of this tree, so there is no build recipe to hash"
        )
    context = sorted(
        (path for path in source.parent.rglob("*") if path.is_file()),
        key=lambda path: path.as_posix(),
    )
    folded = hashlib.sha256()
    # Which file of the context the build builds, before the files themselves, closed by a NUL
    # like each of them: no path can hold one, so no pair of recipes folds to the same bytes.
    folded.update(source.as_posix().encode())
    folded.update(b"\0")
    for path in (*context, Path(workflow)):
        # The path and the bytes both, each closed by a NUL: a hash folding bytes alone would
        # not move when two context files swapped names, and the file's own digest is what
        # keeps one file's bytes from running into the next one's.
        folded.update(path.as_posix().encode())
        folded.update(b"\0")
        folded.update(hashlib.sha256(path.read_bytes()).hexdigest().encode())
        folded.update(b"\0")
    return folded.hexdigest()
