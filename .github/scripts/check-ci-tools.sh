#!/usr/bin/env bash
# The gate on .github/ci-tools.yaml: it holds every row to its shape, to the pin that decides
# which version a job installs, and it refuses any other address for a tool asset in CI's reach.
# A tool this repository installs from a GitHub release asset has a second source, because an
# hour of 504s on those assets otherwise holds every merge (dispatch://INFRA-1873), and that
# manifest is the one place a row's version, its pinned digest and both of its addresses are
# written.
#
# IT READS THE TREE ALONE AND MAKES NO NETWORK READ, so ci.yaml's lint job runs it beside
# `.github/scripts/check-ci-images.sh tree`, and lint runs on every non-draft run.
# It has no mode that reads the package.
# .github/scripts/mirror-ci-tools.sh already reads the package on every run of the ci-images job,
# and it is the only judge of a copy's state.
# A second reader with a rule of its own is how one row could be allowed and refused inside one
# job: a cold row — one whose asset did not answer and whose blob is absent — is green to the
# filler, and nothing else may redden it during the outage the copy exists to survive.
#
# THE RULES. Each fails naming the file, the line and the remedy.
#
#   1. Shape.
#      Every row carries `name`, `version`, `digest`, `digest_of`, `primary`, `second_source` and
#      `max_time`, a `ghcr` row carries `mirror_tag`, `mirror_from` and `mirror_member` as well,
#      and an `npm` row carries `npm_package` and `npm_member`.
#      `digest` is 64 lower-case hex characters, written bare, because that one literal goes into
#      a `sha256sum` line and into a ghcr blob URL alike.
#      Two rows never share a name, and `second_source` is `ghcr` or `npm`.
#      A ghcr row's `mirror_tag` is exactly `tool-<name>-<version>-<the first eight characters of
#      the digest>`, so one tag names one digest and a row that moves to a new digest takes a new
#      tag with it.
#      A row's `primary`, where it is not empty, holds the row's `version`, and so does its
#      `mirror_from`: a row whose version and URL disagree installs one version and pins another.
#
#   2. Each row agrees with the pin that decides which version a job installs.
#      Every `bun-*` row's `version` is the content of .bun-version.
#      The `mise` row's `version` is `ARG MISE_VERSION` of
#      packages/daemon/docker/worker.Dockerfile without its leading `v`, and its `digest` is that
#      file's `ARG MISE_SHA256`, so the copy this repository fills and the mise the worker image
#      builds with are one set of bytes.
#      The `jj` row's `version` is the part of that file's `ARG JJ_TOOL` after the `@`.
#      Each refusal names both values and both files.
#
#   3. No file under .github/actions/ or .github/workflows/ fetches a tool asset no row names.
#      Every *.yml and *.yaml under those two directories is read as text, and a
#      https://github.com/<owner>/<repo>/releases/download/ URL whose <owner>/<repo> this
#      manifest covers is refused unless the whole URL is exactly a row's `primary`, a row's
#      `mirror_from`, or the npm tarball URL a row derives.
#      A registry.npmjs.org URL under a scope a row's `npm_package` names is refused the same
#      way.
#      The covered repositories are the rows' own `primary` and `mirror_from` URLs, plus
#      oven-sh/bun, whose rows carry no primary at all because oven-sh/setup-bun fetches that
#      asset and this repository names it nowhere.
#      A URL holding a `${` is refused too, naming it: a row's URL is the one place a tool
#      asset's address is written, and a built one cannot be compared with a row.
#      Out of scope and never refused: every other repository, install-uv, install-elan,
#      install-pulumi, install-omp, install-claude-code and ci.yaml's pulumi-docker fetch.
#
#   4. No short reference to a tool blob.
#      A `<the manifest's registry>@sha256:<d>` under .github/ whose <d> is a row's digest is
#      refused.
#      Rule 6 of .github/scripts/check-ci-images.sh refuses such a reference whose digest no row
#      of .github/ci-images.yaml holds, so a tool blob written that way would fail that gate for
#      the wrong reason, or pass it if that gate ever loosened.
#      Every reference this repository writes to a tool blob is the full
#      https://ghcr.io/v2/…/blobs/sha256:… URL .github/scripts/ci-tools.sh builds, and this rule
#      keeps it so.
#      The sweep reads every file under .github/ and skips .github/ci-images.yaml, whose own rows
#      are images and not tools.
#
# Run from anywhere: .github/scripts/check-ci-tools.sh
# CI runs it in ci.yaml's lint job, beside the tree mode of .github/scripts/check-ci-images.sh.
# Its tests run in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 python3 <<'PY'
import re
import sys
from pathlib import Path

import yaml


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where it was written. The lines are attributes, not keys: a
    document is never searched for a key the loader invented."""

    line: int
    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)


def line_of(value: object) -> int:
    return getattr(value, "line", 0)


def key_line(mapping: object, name: str) -> int:
    return getattr(mapping, "lines", {}).get(name, line_of(mapping))


# ---------------------------------------------------------------- data
CI_TOOLS = Path(".github/ci-tools.yaml")
CI_IMAGES = Path(".github/ci-images.yaml")
BUN_PIN = Path(".bun-version")
WORKER_DOCKERFILE = Path("packages/daemon/docker/worker.Dockerfile")
GITHUB = Path(".github")
REACH_DIRECTORIES = (Path(".github/actions"), Path(".github/workflows"))
HELPER = ".github/scripts/ci-tools.sh"

# Every row carries these, and each kind of second source carries its own two or three more.
COMMON_FIELDS = ("name", "version", "digest", "digest_of", "primary", "second_source", "max_time")
GHCR_FIELDS = ("mirror_tag", "mirror_from", "mirror_member")
NPM_FIELDS = ("npm_package", "npm_member")
# A field whose value may be empty: `primary` on a row whose bytes a third party fetches, and
# `mirror_member` on a row that pushes an archive whole.
MAY_BE_EMPTY = {"primary", "mirror_member"}
SECOND_SOURCES = ("ghcr", "npm")
DIGEST = re.compile(r"[0-9a-f]{64}")

# The npm registry, which is CI_TOOLS_NPM_BASE's default in the helper: this gate compares a URL
# in the tree with the URL a job reads when no seam is set.
NPM_BASE = "https://registry.npmjs.org"

# oven-sh/setup-bun fetches Bun's own release asset, so no bun row carries a primary and no URL
# of this repository names that repository. It is covered all the same: a workflow that reached
# for the asset itself would name a Bun no row pins.
BUN_UPSTREAM = "oven-sh/bun"

ARG_MISE_VERSION = re.compile(r"^ARG\s+MISE_VERSION=(\S+)\s*$")
ARG_MISE_SHA256 = re.compile(r"^ARG\s+MISE_SHA256=(\S+)\s*$")
ARG_JJ_TOOL = re.compile(r"^ARG\s+JJ_TOOL=(\S+)\s*$")
# A URL ends at whitespace or at a quote of any of the three kinds a YAML or a shell line uses.
ASSET_URL = re.compile(
    r"https://github\.com/(?P<owner>[^/\s\"'`]+)/(?P<repo>[^/\s\"'`]+)/releases/download/[^\s\"'`]*"
)
NPM_URL = re.compile(r"(?:https://)?registry\.npmjs\.org/(?P<rest>[^\s\"'`]+)")

problems: list[str] = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: a block scalar in a workflow would
    otherwise reach the Actions log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def fatal(message: str) -> None:
    """Nothing downstream can be judged without the rows, so stop here."""
    print(f"::error file={CI_TOOLS}::{message}", file=sys.stderr)
    raise SystemExit(1)


def read_text(path: Path) -> str | None:
    """A file's text, or None where it is not decodable as UTF-8."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError:
        return None


def line_at(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def verdict(summary: str) -> None:
    """Prints the problems this run found, and its own summary line."""
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(
            f"::error::{len(problems)} problem(s) between {CI_TOOLS} and the tools CI installs",
            file=sys.stderr,
        )
        raise SystemExit(1)
    print(summary)
    raise SystemExit(0)


# ---------------------------------------------------------------- the rows
if not CI_TOOLS.is_file():
    fatal(
        f"{CI_TOOLS} is missing: it is the one place that says which version of a tool a job "
        f"installs, which digest both of its sources are checked against, and where its second "
        f"source is"
    )
try:
    document = yaml.load(CI_TOOLS.read_text(), Loader=LineLoader)
except yaml.YAMLError as error:
    fatal(f"{CI_TOOLS} is not valid YAML: {one_line(error)}")
if not isinstance(document, dict):
    fatal(f"{CI_TOOLS} is not a mapping of `registry:` and `tools:`")
registry = document.get("registry")
if not isinstance(registry, str) or not registry.strip():
    fatal(
        f"{CI_TOOLS}:{key_line(document, 'registry')}: this file names no registry:, and every "
        f"ghcr row's copy lives in that package"
    )
registry = registry.strip()
rows = document.get("tools")
if not isinstance(rows, list) or not rows:
    fatal(f"{CI_TOOLS}:{key_line(document, 'tools')}: tools is empty, so nothing is checked")

# ---------------------------------------------------------------- rule 1: shape
readable: list[Mapping] = []  # the rows the rules below can read
names: dict[str, int] = {}
for row in rows:
    line = line_of(row) or key_line(document, "tools")
    if not isinstance(row, dict):
        problems.append(
            f"{CI_TOOLS}:{line}: this row is not a mapping of {', '.join(COMMON_FIELDS)}"
        )
        continue
    name = row.get("name")
    if not isinstance(name, str) or not name.strip():
        problems.append(
            f"{CI_TOOLS}:{key_line(row, 'name')}: this row has no name, and a row's name is what "
            f"a call site, every log line and every refusal says"
        )
        continue
    name = name.strip()
    earlier = names.get(name)
    if earlier is not None:
        problems.append(
            f"{CI_TOOLS}:{line}: {name} is also the name of the row on line {earlier}, and a call "
            f"site naming it would read one of the two. Give one of them another name."
        )
        continue
    names[name] = line

    second = row.get("second_source")
    if second not in SECOND_SOURCES:
        problems.append(
            f"{CI_TOOLS}:{key_line(row, 'second_source')}: the {name} row's second_source is "
            f"{one_line(second)}, and {HELPER} serves {' and '.join(SECOND_SOURCES)}."
        )
        continue
    wanted = COMMON_FIELDS + (GHCR_FIELDS if second == "ghcr" else NPM_FIELDS)
    missing = [field for field in wanted if field not in row]
    if missing:
        problems.append(
            f"{CI_TOOLS}:{line}: the {name} row carries no {', '.join(missing)}, and a "
            f"{second} row carries {', '.join(wanted)}."
        )
        continue
    empty = [
        field
        for field in wanted
        if field not in MAY_BE_EMPTY and not str(row.get(field) or "").strip()
    ]
    if empty:
        problems.append(
            f"{CI_TOOLS}:{line}: the {name} row leaves {', '.join(empty)} empty, and a reader of "
            f"this row cannot install the tool without it."
        )
        continue

    digest = str(row["digest"]).strip()
    if not DIGEST.fullmatch(digest):
        problems.append(
            f"{CI_TOOLS}:{key_line(row, 'digest')}: the {name} row's digest is "
            f"{one_line(digest)}, which is not 64 lower-case hex characters. Write it bare, with "
            f"no sha256: prefix: the one literal goes into a sha256sum line and into a ghcr blob "
            f"URL alike."
        )
        continue

    version = str(row["version"]).strip()
    if second == "ghcr":
        tag = str(row["mirror_tag"]).strip()
        rule = f"tool-{name}-{version}-{digest[:8]}"
        if tag != rule:
            problems.append(
                f"{CI_TOOLS}:{key_line(row, 'mirror_tag')}: the {name} row's mirror_tag is {tag} "
                f"and the rule gives it {rule}. The eight characters of the digest are what keep "
                f"one tag on one digest: a row that moves to a new digest takes a new tag with "
                f"it, and no fill moves a tag off a digest an install reads. Name it {rule}."
            )
    for field in ("primary", "mirror_from"):
        url = str(row.get(field) or "").strip()
        if url and version not in url:
            problems.append(
                f"{CI_TOOLS}:{key_line(row, field)}: the {name} row's {field} is {one_line(url)} "
                f"and this row's version is {version}, which that URL does not hold. A row whose "
                f"version and URL disagree installs one version and pins another."
            )
    readable.append(row)

# ---------------------------------------------------------------- rule 2: the pins
bun_pin = ""
if BUN_PIN.is_file():
    bun_pin = BUN_PIN.read_text().strip()
else:
    problems.append(
        f"{BUN_PIN}:1: this file is missing, and every bun row of {CI_TOOLS} carries the version "
        f"it pins"
    )
worker_text = read_text(WORKER_DOCKERFILE)
if worker_text is None:
    problems.append(
        f"{WORKER_DOCKERFILE}:1: this file is missing or is not readable as text, and the mise "
        f"and jj rows of {CI_TOOLS} carry the versions its ARG MISE_VERSION and ARG JJ_TOOL pin"
    )


def last_arg(pattern: re.Pattern[str], text: str) -> str:
    """The value of the last `ARG <name>=<value>` the Dockerfile declares. A later bare `ARG
    <name>` re-declares the build argument in another stage and carries no value, so it is no
    match of these patterns."""
    found = ""
    for line in text.splitlines():
        match = pattern.match(line)
        if match:
            found = match.group(1)
    return found


mise_version = mise_sha256 = jj_tool = ""
if worker_text is not None:
    mise_version = last_arg(ARG_MISE_VERSION, worker_text)
    mise_sha256 = last_arg(ARG_MISE_SHA256, worker_text)
    jj_tool = last_arg(ARG_JJ_TOOL, worker_text)
    for declared, name in (
        (mise_version, "ARG MISE_VERSION"),
        (mise_sha256, "ARG MISE_SHA256"),
        (jj_tool, "ARG JJ_TOOL"),
    ):
        if not declared:
            problems.append(
                f"{WORKER_DOCKERFILE}:1: this file declares no {name}=<value>, and a row of "
                f"{CI_TOOLS} carries what it pins"
            )

for row in readable:
    name = str(row["name"]).strip()
    version = str(row["version"]).strip()
    digest = str(row["digest"]).strip()
    if name.startswith("bun-") and bun_pin and version != bun_pin:
        problems.append(
            f"{CI_TOOLS}:{key_line(row, 'version')}: the {name} row's version is {version} and "
            f"{BUN_PIN} pins {bun_pin}. Every job installs the version that file pins, so this "
            f"row pins the digest of a Bun no job runs. Raising Bun is two commits: this row "
            f"first, then {BUN_PIN} and every call site."
        )
    if name == "mise" and mise_version:
        wanted = mise_version[1:] if mise_version.startswith("v") else mise_version
        if version != wanted:
            problems.append(
                f"{CI_TOOLS}:{key_line(row, 'version')}: the mise row's version is {version} and "
                f"{WORKER_DOCKERFILE} pins ARG MISE_VERSION={mise_version}, which is {wanted} "
                f"without its leading v. .github/actions/install-jj installs the row's mise and "
                f"the worker image installs the ARG's, and they are one tool."
            )
    if name == "mise" and mise_sha256 and digest != mise_sha256:
        problems.append(
            f"{CI_TOOLS}:{key_line(row, 'digest')}: the mise row's digest is {digest} and "
            f"{WORKER_DOCKERFILE} pins ARG MISE_SHA256={mise_sha256}. Both check the same "
            f"archive, so one of the two admits bytes the other refuses."
        )
    if name == "jj" and jj_tool:
        wanted = jj_tool.rpartition("@")[2]
        if version != wanted:
            problems.append(
                f"{CI_TOOLS}:{key_line(row, 'version')}: the jj row's version is {version} and "
                f"{WORKER_DOCKERFILE} pins ARG JJ_TOOL={jj_tool}, whose version is {wanted}. "
                f".github/actions/install-jj installs the tool spec that ARG names and checks it "
                f"against this row."
            )

# ---------------------------------------------------------------- rule 3: the addresses
# The repositories this manifest covers, read off the rows' own URLs, and the one address of
# each row a file of CI's reach may name.
covered_repositories = {BUN_UPSTREAM}
covered_scopes: set[str] = set()
allowed: dict[str, str] = {}
for row in readable:
    name = str(row["name"]).strip()
    version = str(row["version"]).strip()
    for field in ("primary", "mirror_from"):
        url = str(row.get(field) or "").strip()
        if not url:
            continue
        allowed[url] = f"the {name} row's {field}"
        match = ASSET_URL.match(url)
        if match:
            covered_repositories.add(f"{match.group('owner')}/{match.group('repo')}")
    package = str(row.get("npm_package") or "").strip()
    if package:
        scope = package.partition("/")[0]
        if scope.startswith("@"):
            covered_scopes.add(scope)
        # npm serves a scoped package's tarball under the package name without its scope, which
        # is the URL .github/scripts/ci-tools.sh builds for this row.
        derived = f"{NPM_BASE}/{package}/-/{package.rpartition('/')[2]}-{version}.tgz"
        allowed[derived] = f"the {name} row's npm tarball"

reach: list[Path] = []
for directory in REACH_DIRECTORIES:
    for suffix in ("*.yml", "*.yaml"):
        reach.extend(sorted(directory.rglob(suffix)))
reach = sorted(set(reach))

named_rows = 0
for file in reach:
    text = read_text(file)
    if text is None:
        continue
    found: list[tuple[int, str, str]] = []  # offset, the URL, what it names
    for match in ASSET_URL.finditer(text):
        repository = f"{match.group('owner')}/{match.group('repo')}"
        if repository not in covered_repositories:
            continue
        found.append(
            (match.start(), match.group(0).rstrip(",);"), f"a release asset of {repository}")
        )
    for match in NPM_URL.finditer(text):
        rest = match.group("rest")
        scope = rest.partition("/")[0]
        if scope not in covered_scopes:
            continue
        found.append((match.start(), match.group(0).rstrip(",);"), f"an npm package under {scope}"))
    for offset, url, what in found:
        line = line_at(text, offset)
        if "${" in url:
            problems.append(
                f"{file}:{line}: this file builds the address of {what} ({one_line(url)}). A "
                f"row of {CI_TOOLS} is the one place a tool asset's address is written, and a "
                f"built URL cannot be compared with a row. Read the address from the row with "
                f"{HELPER} instead."
            )
            continue
        if url not in allowed:
            problems.append(
                f"{file}:{line}: this file fetches {what} at {one_line(url)}, and no row of "
                f"{CI_TOOLS} names that URL. A tool CI installs from a release asset has a "
                f"second source, and a fetch no row names has none. Add the row, or fetch it "
                f"with {HELPER}."
            )
            continue
        named_rows += 1

# ---------------------------------------------------------------- rule 4: short references
# A tool blob is read at the full https://<base>/v2/<path>/blobs/sha256:<digest> URL the helper
# builds. Written as a registry reference instead, its digest would meet rule 6 of
# check-ci-images.sh, which refuses a digest no row of .github/ci-images.yaml holds.
short_references = {f"{registry}@sha256:{str(row['digest']).strip()}": str(row["name"]).strip()
                    for row in readable}
swept = 0
for file in sorted(GITHUB.rglob("*")):
    if not file.is_file() or file == CI_IMAGES:
        continue
    text = read_text(file)
    if text is None:
        continue
    swept += 1
    for reference, name in short_references.items():
        offset = text.find(reference)
        if offset < 0:
            continue
        problems.append(
            f"{file}:{line_at(text, offset)}: this file names the {name} row's blob as "
            f"{reference}, a registry reference. Rule 6 of .github/scripts/check-ci-images.sh "
            f"refuses a reference of this shape whose digest no row of {CI_IMAGES} holds, so "
            f"this one fails that gate for the wrong reason. A tool blob is read at the full "
            f"blobs/sha256: URL {HELPER} builds from the row."
        )

verdict(
    f"{CI_TOOLS} holds {len(readable)} row(s), each agreeing with the pin that installs it; "
    f"{named_rows} tool asset URL(s) in {len(reach)} file(s) under "
    f"{' and '.join(str(directory) for directory in REACH_DIRECTORIES)} name one of those rows; "
    f"of {swept} file(s) under {GITHUB} swept, none names a tool blob by a registry reference"
)
PY
