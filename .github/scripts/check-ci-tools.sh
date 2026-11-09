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
#      `max_time`, a `ghcr-mirror` row carries `mirror_from` and `mirror_member` as well, and an
#      `npm` row carries `npm_package` and `npm_member`.
#      `digest` is 64 lower-case hex characters, written bare, because that one literal goes into
#      a `sha256sum` line and into a ghcr blob URL alike.
#      Two rows never share a name, and `second_source` is `ghcr-mirror` or `npm`, the words
#      .github/scripts/ci-tools.sh names those sources by in every line and every refusal.
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
#   3. No file under .github/ fetches a tool asset no row of this manifest names and no
#      no_second_source entry of it declines.
#      Every file under .github/ but this manifest — which declares addresses and fetches
#      nothing — is read as text, and a GitHub release-asset URL — one of the shape
#      `https://github.com/` <owner>/<repo> `/releases/download/` … — is refused whatever
#      repository it names, unless one of these holds, tested in this order:
#        a. its <owner>/<repo> is this repository's own, which .github/scripts/repo-paths.sh
#           derives. This repository publishes those bytes and installs none of them.
#        b. the file's own name holds `.test.`: its URLs are fixtures a test feeds to a gate,
#           and the gate that reads them is the code under test. A real install could hide
#           there, and the alternative is the hand-kept exemption list this rule exists to
#           remove.
#        c. the URL text is exactly a row's `primary`, a row's `mirror_from`, or the npm tarball
#           URL a row derives.
#        d. the URL text is exactly the `url` of a `no_second_source` entry.
#      A URL none of those allows and whose text holds a `$` is refused as a built address
#      rather than as an unnamed one: a row's URL is the one place a tool asset's address is
#      written, and an address a job assembles cannot be compared with a row. It is tested after
#      a to d, so an entry may decline a URL a job builds by writing that URL's text verbatim.
#      A registry.npmjs.org URL under a scope a row's `npm_package` names is refused the same
#      way, but in a reach of its own, which this rule claims and no more: every *.yml and
#      *.yaml under .github/actions/ and .github/workflows/, AND NOTHING ELSE — no file under
#      .github/scripts/, no file of any other suffix, and not this manifest. So an npm fetch
#      written in a shell script under .github/scripts/ is invisible to this gate.
#      The narrower reach is deliberate: the class this rule closes is GitHub release assets,
#      and this repository writes the npm registry's address outside those two directories for
#      reasons that fetch nothing (.github/scripts/check-bun-version.sh:137 lists
#      `registry.npmjs.org/@oven/bun` as a string to detect).
#
#      A `no_second_source` entry records a URL that has no second source anywhere, and allows
#      that one URL. It carries `url`, `digest`, `pinned_in` and `why`, and the gate holds it to
#      four rules, each refused in its own words:
#        * its `url` occurs verbatim in some file under .github/ that this rule reads, the
#          manifest itself not counted, so an entry whose install was deleted is refused and the
#          list prunes itself;
#        * its `digest` is 64 lower-case bare hex and occurs in the file `pinned_in` names, so
#          one source never means no checksum;
#        * no row's `primary` or `mirror_from` already covers its repository, so a decline never
#          bypasses a tool that has a second source;
#        * `why` is a sentence rather than a placeholder.
#      Whether that reason is TRUE is the reviewer's judgement and no gate's: the three rules
#      above are the whole of what a program can check here.
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
#      are images and not tools. It is the same pass rule 3 makes.
#
# Run from anywhere: .github/scripts/check-ci-tools.sh
# CI runs it in ci.yaml's lint job, beside the tree mode of .github/scripts/check-ci-images.sh.
# Its tests run in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 python3 <<'PY'
import os
import re
import subprocess
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
# Rule 3's npm half reads these two directories; its release-asset half reads every file under
# .github/, and so does rule 4.
NPM_DIRECTORIES = (Path(".github/actions"), Path(".github/workflows"))
HELPER = ".github/scripts/ci-tools.sh"
REPO_PATHS = Path(".github/scripts/repo-paths.sh")

# Every row carries these, and each kind of second source carries its own two more.
COMMON_FIELDS = ("name", "version", "digest", "digest_of", "primary", "second_source", "max_time")
GHCR_FIELDS = ("mirror_from", "mirror_member")
NPM_FIELDS = ("npm_package", "npm_member")
# A field whose value may be empty: `primary` on a row whose bytes a third party fetches, and
# `mirror_member` on a row that pushes an archive whole.
MAY_BE_EMPTY = {"primary", "mirror_member"}
# The words a row writes its second source as, which are the words the helper's own lines and
# refusals say.
SECOND_SOURCES = ("ghcr-mirror", "npm")
DIGEST = re.compile(r"[0-9a-f]{64}")

# The npm registry, which is CI_TOOLS_NPM_BASE's default in the helper: this gate compares a URL
# in the tree with the URL a job reads when no seam is set.
NPM_BASE = "https://registry.npmjs.org"

# A `no_second_source` entry: the one way a URL with no second source anywhere is allowed.
NO_SECOND_SOURCE = "no_second_source"
ENTRY_FIELDS = ("url", "digest", "pinned_in", "why")
# A reason that is one of these, or shorter than this, says nothing a reader could act on. The
# bound is stated in the refusal, so the writer is told what the gate wants rather than guessing.
REASON_PLACEHOLDERS = {"todo", "tbd", "n/a", "na", "none", "-", "--", "unknown", "?", "fixme"}
REASON_MINIMUM = 40
# A file whose name holds this feeds URLs to a gate as fixtures; rule 3 reads none of them.
TEST_FILE = ".test."

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
        f"ghcr-mirror row's copy lives in that package"
    )
registry = registry.strip()
rows = document.get("tools")
if not isinstance(rows, list) or not rows:
    fatal(f"{CI_TOOLS}:{key_line(document, 'tools')}: tools is empty, so nothing is checked")

# ------------------------------------------------- the declines, read for their shape alone
# An entry's four rules need the rows' repositories and the text of every file rule 3 reads, so
# they are applied in rule 3's own section below. Shape is read here, because an entry the gate
# cannot read at all allows nothing and is refused once rather than four times.
declines: list[Mapping] = []
entries = document.get(NO_SECOND_SOURCE)
if entries is None:
    entries = []
if not isinstance(entries, list):
    fatal(
        f"{CI_TOOLS}:{key_line(document, NO_SECOND_SOURCE)}: {NO_SECOND_SOURCE} is not a list of "
        f"entries, each carrying {', '.join(ENTRY_FIELDS)}"
    )
declared_urls: dict[str, int] = {}
for entry in entries:
    line = line_of(entry) or key_line(document, NO_SECOND_SOURCE)
    if not isinstance(entry, dict):
        problems.append(
            f"{CI_TOOLS}:{line}: this {NO_SECOND_SOURCE} entry is not a mapping of "
            f"{', '.join(ENTRY_FIELDS)}"
        )
        continue
    missing = [field for field in ENTRY_FIELDS if not str(entry.get(field) or "").strip()]
    if missing:
        problems.append(
            f"{CI_TOOLS}:{line}: this {NO_SECOND_SOURCE} entry carries no "
            f"{', '.join(missing)}, and an entry carries {', '.join(ENTRY_FIELDS)}: the URL it "
            f"declines, the digest its bytes are pinned at, the file that pins them, and why "
            f"there is no second source."
        )
        continue
    url = str(entry["url"]).strip()
    if not ASSET_URL.fullmatch(url):
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'url')}: this {NO_SECOND_SOURCE} entry's url is "
            f"{one_line(url)}, which is not a GitHub release-asset address. An entry declines "
            f"one such URL, written exactly as the file that fetches it writes it."
        )
        continue
    earlier = declared_urls.get(url)
    if earlier is not None:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'url')}: this URL is also declined by the "
            f"{NO_SECOND_SOURCE} entry on line {earlier}, and two entries for one URL carry two "
            f"reasons a reader must choose between. Keep one."
        )
        continue
    declared_urls[url] = key_line(entry, "url")
    declines.append(entry)

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
    wanted = COMMON_FIELDS + (GHCR_FIELDS if second == "ghcr-mirror" else NPM_FIELDS)
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
            f"row pins the digest of a Bun no job runs. Raising Bun takes two commits, unlike "
            f"raising any other tool here: the new oven/bun row of {CI_IMAGES} first, then "
            f"{BUN_PIN}, the ARG BUN_VERSION default of {WORKER_DOCKERFILE} and every FROM on "
            f"the row it leaves, and the three bun-* rows of this file move in that second "
            f"commit, with {BUN_PIN}. No call site moves, because a call site names a row "
            f"rather than a version; that is also why a second row of this tool would have no "
            f"name one could use."
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
# This repository's own release assets are bytes it publishes and installs nowhere, and
# .github/scripts/repo-paths.sh is the one place its own addresses are derived. GITHUB_REPOSITORY
# is taken out of that call's environment on purpose: this gate reads the tree alone, so the name
# it compares against is the one the tree carries (repo-paths.sh's own default), not the one the
# runner was handed. A fork's run carries its own GITHUB_REPOSITORY, and reading it here would
# turn ci.yaml's deliberately pinned cybermonklabs/legion URL into a refusal on every fork.
environment = {key: value for key, value in os.environ.items() if key != "GITHUB_REPOSITORY"}
try:
    own_url = subprocess.run(
        ["bash", str(REPO_PATHS), "plugin_download_url"],
        capture_output=True,
        text=True,
        check=True,
        env=environment,
    ).stdout.strip()
except (OSError, subprocess.CalledProcessError) as error:
    fatal(
        f"{REPO_PATHS} did not answer plugin_download_url ({one_line(error)}), and this gate "
        f"allows a release-asset URL of this repository's own by reading that answer. Without "
        f"it every rule below would judge this repository's own published address as a fetch."
    )
own_match = ASSET_URL.match(own_url)
if not own_match:
    fatal(
        f"{REPO_PATHS} answered plugin_download_url with {one_line(own_url)}, which is not a "
        f"GitHub release-asset address, so this gate cannot read this repository's own name out "
        f"of it."
    )
OWN_REPOSITORY = f"{own_match.group('owner')}/{own_match.group('repo')}"

# The one address of each row a file under .github/ may name, the repositories the rows cover,
# and the npm scopes they cover.
row_repositories: dict[str, str] = {}
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
            row_repositories.setdefault(
                f"{match.group('owner')}/{match.group('repo')}", f"the {name} row's {field}"
            )
    package = str(row.get("npm_package") or "").strip()
    if package:
        scope = package.partition("/")[0]
        if scope.startswith("@"):
            covered_scopes.add(scope)
        # npm serves a scoped package's tarball under the package name without its scope, which
        # is the URL .github/scripts/ci-tools.sh builds for this row.
        derived = f"{NPM_BASE}/{package}/-/{package.rpartition('/')[2]}-{version}.tgz"
        allowed[derived] = f"the {name} row's npm tarball"
for entry in declines:
    allowed[str(entry["url"]).strip()] = (
        f"the {NO_SECOND_SOURCE} entry on line {line_of(entry)}"
    )

npm_reach: set[Path] = set()
for directory in NPM_DIRECTORIES:
    for suffix in ("*.yml", "*.yaml"):
        npm_reach.update(directory.rglob(suffix))

# ------------------------------------------------- one pass: rule 3 and rule 4 over .github/
# Rule 4: a tool blob is read at the full https://<base>/v2/<path>/blobs/sha256:<digest> URL the
# helper builds. Written as a registry reference instead, its digest would meet rule 6 of
# check-ci-images.sh, which refuses a digest no row of .github/ci-images.yaml holds.
short_references = {f"{registry}@sha256:{str(row['digest']).strip()}": str(row["name"]).strip()
                    for row in readable}
# Rule 4's sweep already reads every file under .github/, so rule 3 rides it rather than opening
# the tree twice. `writers` is what an entry's first rule is decided on: the text of every file
# rule 3 reads, the manifest left out, since an entry naming only itself names no install.
writers: dict[Path, str] = {}
named = 0
read_files = 0
swept = 0
for file in sorted(GITHUB.rglob("*")):
    if not file.is_file():
        continue
    text = read_text(file)
    if text is None:
        continue

    if file != CI_IMAGES:
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

    # The manifest declares addresses and fetches nothing, so rule 3 does not read it: a row's
    # own URL is not an install, and an entry the shape check already refused would otherwise be
    # reported a second time as a file that fetches.
    if TEST_FILE in file.name or file == CI_TOOLS:
        continue
    read_files += 1
    writers[file] = text

    # offset, the URL as written, what it names, the repository it names, the remedy
    found: list[tuple[int, str, str, str | None, str]] = []
    for match in ASSET_URL.finditer(text):
        repository = f"{match.group('owner')}/{match.group('repo')}"
        found.append(
            (
                match.start(),
                match.group(0).rstrip(",);"),
                f"a release asset of {repository}",
                repository,
                f"Add a row naming its second source and fetch it with {HELPER}, or, where those "
                f"bytes are published nowhere but there, a {NO_SECOND_SOURCE} entry carrying "
                f"{', '.join(ENTRY_FIELDS)}.",
            )
        )
    if file in npm_reach:
        for match in NPM_URL.finditer(text):
            scope = match.group("rest").partition("/")[0]
            if scope not in covered_scopes:
                continue
            found.append(
                (
                    match.start(),
                    match.group(0).rstrip(",);"),
                    f"an npm package under {scope}",
                    None,
                    f"Add the row, or fetch it with {HELPER}.",
                )
            )

    for offset, url, what, repository, remedy in found:
        line = line_at(text, offset)
        if repository == OWN_REPOSITORY:
            # This repository publishes those bytes; nothing here fetches them.
            continue
        if url in allowed:
            named += 1
            continue
        if "$" in url:
            problems.append(
                f"{file}:{line}: this file builds the address of {what} ({one_line(url)}). A "
                f"row of {CI_TOOLS} is the one place a tool asset's address is written, and a "
                f"built URL cannot be compared with a row. Read the address from the row with "
                f"{HELPER} instead, or, where those bytes have no second source, write this "
                f"URL's text verbatim as a {NO_SECOND_SOURCE} entry's url."
            )
            continue
        problems.append(
            f"{file}:{line}: this file fetches {what} at {one_line(url)}, and no row of "
            f"{CI_TOOLS} names that URL. A tool CI installs from a release asset has a second "
            f"source, and a fetch nothing names has none. {remedy}"
        )

# ------------------------------------------- the four rules a no_second_source entry is held to
for entry in declines:
    url = str(entry["url"]).strip()
    line = line_of(entry)
    repository = ASSET_URL.match(url)
    repository = f"{repository.group('owner')}/{repository.group('repo')}"

    if not any(url in text for text in writers.values()):
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'url')}: no file under {GITHUB} writes "
            f"{one_line(url)}, so this entry declines a second source for an install that is no "
            f"longer here. Delete the entry, or correct its url to the text the file that "
            f"fetches those bytes writes."
        )

    digest = str(entry["digest"]).strip()
    pinned_in = Path(str(entry["pinned_in"]).strip())
    pin_text = read_text(pinned_in) if pinned_in.is_file() else None
    if not DIGEST.fullmatch(digest):
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'digest')}: this entry's digest is {one_line(digest)}, "
            f"which is not 64 lower-case hex characters. Write it bare, exactly as the file that "
            f"pins those bytes writes it."
        )
    elif pin_text is None:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'pinned_in')}: this entry names {pinned_in} as the file "
            f"that pins these bytes, and that file is missing or is not readable as text. A URL "
            f"with one source is pinned by a digest some file checks, or nothing holds it to "
            f"anything."
        )
    elif digest not in pin_text:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'digest')}: this entry's digest {digest} does not occur "
            f"in {pinned_in}, the file it names as the pin, so nothing checks the bytes this "
            f"entry declines a second source for. One source never means no checksum."
        )

    covered_by = row_repositories.get(repository)
    if covered_by:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'url')}: {covered_by} already covers {repository}, so "
            f"this tool has a second source and its call site reads it with {HELPER}. An entry "
            f"declines a URL nothing else serves; delete this one and fetch the bytes from the "
            f"row."
        )

    why = " ".join(str(entry["why"]).split())
    if why.strip(" .").lower() in REASON_PLACEHOLDERS or len(why) < REASON_MINIMUM:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'why')}: this entry's why is {one_line(why)}, which "
            f"says nothing a reader could act on. Write at least {REASON_MINIMUM} characters "
            f"naming which second source was looked for, why there is none today, and what "
            f"building one would take. Whether that reason is true is the reviewer's judgement "
            f"and not this gate's, which is why it has to be readable."
        )

# The middle clause counts installs, not addresses: this manifest is not among the files rule 3
# reads, so a row's own URL and an entry's own url are no part of it. A count that included them
# would rise when the manifest grew and say nothing about what CI fetches.
verdict(
    f"{CI_TOOLS} holds {len(readable)} row(s), each agreeing with the pin that installs it, and "
    f"{len(declines)} {NO_SECOND_SOURCE} entr{'y' if len(declines) == 1 else 'ies'}; "
    f"{named} tool asset URL(s) fetched by the {read_files} file(s) under {GITHUB} that rule 3 "
    f"reads, this manifest not among them, name one of those; "
    f"of {swept} file(s) under {GITHUB} swept, none names a tool blob by a registry reference"
)
PY
