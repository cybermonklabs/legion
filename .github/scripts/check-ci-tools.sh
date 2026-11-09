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
#      nothing — is read as text, and a GitHub release-asset URL is refused whatever repository
#      it names, unless one of these holds, tested in this order. WHICH addresses are read as
#      one release asset, and which are deliberately not read at all, is stated once beside the
#      patterns themselves — ASSET_URL and API_ASSET_URL below — each with the measurement that
#      earned it. It is not restated here: this header carried its own copy of that list for one
#      round, the patterns grew, and the two then said different things about the same code.
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
#      the rules below, each refused in its own words:
#        * its `url` occurs verbatim in some file under .github/ that this rule reads, the
#          manifest itself not counted, so an entry whose install was deleted is refused and the
#          list prunes itself;
#        * its `pinned_in` is neither this manifest nor a file whose name holds `.test.`: the
#          entry's own `digest:` line is in the manifest and a fixture's digests are fed to a
#          gate, so either would satisfy the rule below while nothing a job runs checked the
#          bytes;
#        * its `digest` is 64 lower-case bare hex, the file `pinned_in` names can be read as
#          text, and that digest occurs in it, each refused in its own words, so one source
#          never means no checksum;
#        * no row already covers its repository, compared without regard to case because
#          github.com reads an owner and a repository that way, so a decline never bypasses a
#          tool that has a second source;
#        * `why` is a sentence rather than a placeholder.
#      Whether that reason is TRUE is the reviewer's judgement and no gate's: the rules above
#      are the whole of what a program can check here.
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
# The repository the bun rows cover although no row of them writes its URL: `oven-sh/setup-bun`
# fetches that release asset, so every bun row's `primary` is empty and its second source is
# npm. A row contributes the repository of the URLs it writes, and these rows write none, so
# without this the covered-repository rule below would read Bun as covered by nothing and let an
# entry decline an asset that has npm behind it.
BUN_REPOSITORY = "oven-sh/bun"
# What a rule 3 refusal ends with, per kind of address and per reason it is refused. None of
# them depends on the URL found, so each is written once here rather than rebuilt at every
# match. The two built remedies differ in their last clause alone: an entry declines one GitHub
# release-asset address, so advising an entry for a built npm URL is advice the entry shape rule
# then refuses.
ASSET_REMEDY = (
    f"Add a row naming its second source and fetch it with {HELPER}, or, where those bytes are "
    f"published nowhere but there, a {NO_SECOND_SOURCE} entry carrying {', '.join(ENTRY_FIELDS)}."
)
ASSET_BUILT_REMEDY = (
    f"Read the address from the row with {HELPER} instead, or, where those bytes have no second "
    f"source, write this URL's text verbatim as a {NO_SECOND_SOURCE} entry's url."
)
NPM_REMEDY = f"Add the row, or fetch it with {HELPER}."
NPM_BUILT_REMEDY = (
    f"Read the address from the row with {HELPER} instead. No {NO_SECOND_SOURCE} entry can "
    f"decline this address: an entry takes a GitHub release-asset URL and nothing else."
)
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
# GitHub serves one release asset at many addresses, and a rule that compares text reaches only
# the ones it spells out. Every form below was measured against github.com on 2026-11-09 with
# `curl -sS -I`, and each answered with the asset or a redirect to it.
#
# THE HOST IS READ WITHOUT REGARD TO CASE, because a URL's host is case-insensitive:
# `https://GitHub.com/…` answered 302 with the asset. A trailing dot (`github.com.`, 301 to the
# canonical form) and an explicit port (`github.com:443`, 302; `http://github.com:80`, 301) are
# the same host written two other ways, so both are read too, as is the `www.` prefix.
#
# THE SCHEME IS OPTIONAL AND READ WITHOUT REGARD TO CASE, AND USERINFO IS READ: a scheme is
# case-insensitive, so `HTTPS://…` is the same address; `curl` and `wget` are given a bare
# `github.com/…` and supply the scheme themselves; and `https://user@github.com/…` carries a
# credential this server ignores. Each of the three fetches the asset, so each is one spelling
# of one install.
#
# WHICH IS WHY THE START OF A MATCH IS GUARDED. An optional scheme would otherwise read any
# host whose text ENDS in github.com: `https://notgithub.com/<o>/<r>/releases/download/…` is a
# third party's address and no row of this manifest has anything to say about it. So a match
# begins at the scheme, or at a position no host character precedes — no letter, digit, `_`,
# `.`, `-`, `/` or `@`.
# `https://evil.example/github.com/<o>/<r>/releases/download/…` is NOT read either, by the `/`
# in that class, and that is a decision rather than a leftover: those bytes come from
# evil.example, the class this rule closes is the release assets github.com serves, and reading
# that address would name a repository of a host that has none. A third party that lays
# GitHub's path under its own host is a rule about hosts, which this is not.
#
# AN ACTIONS EXPRESSION IS READ AS A PIECE OF PATH, because `${{ inputs.repo }}` holds spaces
# and a job writes it where <owner>/<repo> stands, so no element class can match it. Such a URL
# holds a `$` and is refused as an address the job builds, which is the refusal that sends its
# writer to the row; what the expression stands for cannot be known here.
#
# THE PATH IS COMPARED LITERALLY, because github.com's path IS case-sensitive:
# `/RELEASES/download/` and `/releases/DOWNLOAD/` each answered 404, so a rule that read the
# whole address without regard to case would refuse an address GitHub does not answer. Two paths
# are read: the tag's own, and the `latest` permalink GitHub documents.
#
# AND THE API SERVES THE SAME BYTES at /repos/<owner>/<repo>/releases/assets/<id>, which answered
# 302 to the asset under `Accept: application/octet-stream`, so that address is read as well.
# Its host carries the owner one path element later than github.com's does, so it is a pattern
# of its own rather than another branch of the first.
#
# WHAT IS NOT READ, and why each one is out rather than in. Four spellings of the same path are
# invisible to these patterns, and the reason is the patterns' own shape rather than anything a
# server does: an extra slash before the owner or after it leaves a path element empty, which
# PATH_ELEMENT cannot match; an extra slash after `releases` puts `/download` where `download`
# must stand; and a `/.` segment takes the owner's place, so `/releases/download/` then falls in
# the wrong element. Driven at this head, the gate exits 0 on all four.
# An extra slash after `download` is NOT one of them: the trailing `[^\s"'`]*` swallows it, so
# the rule reads that URL and refuses it, and the suite drives it.
# github.com answered 302 with the asset to each of the four, read between two readings of the
# canonical address and counted only where both answered 302 — a release asset answering 504 in
# an outage and one answering 504 because GitHub does not serve that shape look alike, and that
# control is what tells them apart. Whether it serves the after-`download` shape is not stated
# anywhere, here or in AGENTS.md, because it decides nothing: that shape is read whatever the
# server answers, and a reading of the world cannot settle a question about the text.
# Closing the four would mean normalising a path — percent-encoding and dot segments included —
# which is not a class a text comparison can close. Nor is any other host that can serve the
# bytes, the `release-assets.githubusercontent.com` address these redirects land on among them.
URL_START = r"(?:(?i:https?)://(?:[^/\s\"'`@]*@)?|(?<![\w.\-/@]))"
GITHUB_HOST = r"(?i:(?:www\.)?github\.com)\.?(?::(?:80|443))?"
# One element of a path: anything up to the next `/`, or an Actions expression, which is one
# element's worth of path however many spaces and slashes it holds.
PATH_ELEMENT = r"(?:\$\{\{[^{}]*\}\}|[^/\s\"'`])+"
# <owner>/<repo>, read as one group: every rule that uses it compares the pair. One expression
# standing for the pair is the second branch — a job writing `${{ inputs.repo }}` there writes
# the owner and the repository in one piece of path.
REPOSITORY = "(?P<repository>" + PATH_ELEMENT + "/" + PATH_ELEMENT + r"|\$\{\{[^{}]*\}\})"
ASSET_URL = re.compile(
    URL_START + GITHUB_HOST + "/" + REPOSITORY
    + r"/releases/(?:download|latest/download)/[^\s\"'`]*"
)
API_ASSET_URL = re.compile(
    URL_START + r"(?i:api\.github\.com)\.?(?::(?:80|443))?"
    + "/repos/" + REPOSITORY + r"/releases/assets/[0-9]+"
)
ASSET_PATTERNS = (ASSET_URL, API_ASSET_URL)
NPM_URL = re.compile(r"(?:https://)?registry\.npmjs\.org/(?P<rest>[^\s\"'`]+)")

problems: list[str] = []


def asset_at(url: str) -> re.Match[str] | None:
    """The release-asset address this text begins with, read by either pattern, or None."""
    for pattern in ASSET_PATTERNS:
        match = pattern.match(url)
        if match:
            return match
    return None


def asset_whole(url: str) -> re.Match[str] | None:
    """The same, where the whole text is one address and nothing more."""
    for pattern in ASSET_PATTERNS:
        match = pattern.fullmatch(url)
        if match:
            return match
    return None


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
# The rest of an entry's rules need the rows' repositories and the text of every file rule 3
# reads, so they are applied in rule 3's own section below. Shape is read here, because an entry
# the gate cannot read at all allows nothing and is refused once rather than rule by rule.
# Each entry is kept with the <owner>/<repo> its url names, read by the pattern that matched it
# here: the section below needs that pair and the url has been read once already.
declines: list[tuple[Mapping, str]] = []
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
    url_match = asset_whole(url)
    if not url_match:
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
    declines.append((entry, url_match.group("repository")))

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
own_match = asset_at(own_url)
if not own_match:
    fatal(
        f"{REPO_PATHS} answered plugin_download_url with {one_line(own_url)}, which is not a "
        f"GitHub release-asset address, so this gate cannot read this repository's own name out "
        f"of it."
    )
OWN_REPOSITORY = own_match.group("repository").casefold()

# The one address of each row a file under .github/ may name, the repositories the rows cover,
# and the npm scopes they cover.
# A REPOSITORY IS KEYED CASEFOLDED, and so is the comparison against this repository's own
# name, because github.com reads an owner and a repository without regard to case: measured on
# 2026-11-09, the owner in another case, the repository in another case and both at once each
# answered 302 with the asset, between two readings of the canonical address that answered 302.
# What a row writes in `allowed` stays the exact text it writes: the manifest names one address
# per row, and a file that fetches a different spelling of it fetches something this manifest
# does not say is pinned.
row_repositories: dict[str, str] = {}
covered_scopes: set[str] = set()
allowed: set[str] = set()
for row in readable:
    name = str(row["name"]).strip()
    version = str(row["version"]).strip()
    for field in ("primary", "mirror_from"):
        url = str(row.get(field) or "").strip()
        if not url:
            continue
        allowed.add(url)
        match = asset_at(url)
        if match:
            row_repositories.setdefault(
                match.group("repository").casefold(), f"the {name} row's {field}"
            )
    if name.startswith("bun-"):
        row_repositories.setdefault(BUN_REPOSITORY.casefold(), f"the {name} row")
    package = str(row.get("npm_package") or "").strip()
    if package:
        scope = package.partition("/")[0]
        if scope.startswith("@"):
            covered_scopes.add(scope)
        # npm serves a scoped package's tarball under the package name without its scope, which
        # is the URL .github/scripts/ci-tools.sh builds for this row.
        derived = f"{NPM_BASE}/{package}/-/{package.rpartition('/')[2]}-{version}.tgz"
        allowed.add(derived)
for entry, _ in declines:
    allowed.add(str(entry["url"]).strip())

# ------------------------------------------------- one pass: rule 3 and rule 4 over .github/
# Rule 4: a tool blob is read at the full https://<base>/v2/<path>/blobs/sha256:<digest> URL the
# helper builds. Written as a registry reference instead, its digest would meet rule 6 of
# check-ci-images.sh, which refuses a digest no row of .github/ci-images.yaml holds.
short_references = {f"{registry}@sha256:{str(row['digest']).strip()}": str(row["name"]).strip()
                    for row in readable}
# Rule 4's sweep already reads every file under .github/, so rule 3 rides it rather than opening
# the tree twice. An entry's first rule — some file writes its url — is decided inside this pass:
# deciding it afterwards would mean holding the text of every file rule 3 reads until then.
# Rule 4 sweeps the manifest like any other file; rule 3 is the half that passes over it, since
# a row's own URL is not an install.
written: set[str] = set()
# The files the entries name as their pin. The pass keeps the text of any of them it reads, so
# an entry's digest rule does not open .github/workflows/ci.yaml a second time; a path the pass
# never reaches — the manifest, a .test. file, a file outside .github/ — is read where it is used.
pinned_paths = {Path(str(entry["pinned_in"]).strip()) for entry, _ in declines}
pin_texts: dict[Path, str] = {}
named = 0
read_files = 0
swept = 0
# A file neither rule could read. Named in the summary, because a sweep that passes over a file
# in silence claims a reach it did not have, and a reader of the line cannot tell a file that
# was read from one that was skipped.
unreadable: list[Path] = []
for file in sorted(GITHUB.rglob("*")):
    if not file.is_file():
        continue
    text = read_text(file)
    if text is None:
        unreadable.append(file)
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
    # Inside the pass, so no file's text outlives it: see `written` above.
    written.update(url for url in declared_urls if url in text)
    if file in pinned_paths:
        pin_texts[file] = text

    # offset, the URL as written, what it names, the repository it names, the remedy where
    # nothing names it, the remedy where the job builds it
    found: list[tuple[int, str, str, str | None, str, str]] = []
    for pattern in ASSET_PATTERNS:
        for match in pattern.finditer(text):
            repository = match.group("repository")
            found.append(
                (
                    match.start(),
                    match.group(0).rstrip(",);"),
                    f"a release asset of {repository}",
                    repository,
                    ASSET_REMEDY,
                    ASSET_BUILT_REMEDY,
                )
            )
    # Rule 3's npm half claims every *.yml and *.yaml under NPM_DIRECTORIES and nothing else.
    if file.name.endswith((".yml", ".yaml")) and any(
        directory in file.parents for directory in NPM_DIRECTORIES
    ):
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
                    NPM_REMEDY,
                    NPM_BUILT_REMEDY,
                )
            )

    # Two address patterns and the npm one each sweep the file, so the refusals are put back
    # into the order a reader meets them in that file.
    found.sort()
    for offset, url, what, repository, remedy, built_remedy in found:
        line = line_at(text, offset)
        if repository is not None and repository.casefold() == OWN_REPOSITORY:
            # This repository publishes those bytes; nothing here fetches them.
            continue
        if url in allowed:
            named += 1
            continue
        if "$" in url:
            problems.append(
                f"{file}:{line}: this file builds the address of {what} ({one_line(url)}). A "
                f"row of {CI_TOOLS} is the one place a tool asset's address is written, and a "
                f"built URL cannot be compared with a row. {built_remedy}"
            )
            continue
        problems.append(
            f"{file}:{line}: this file fetches {what} at {one_line(url)}, and no row of "
            f"{CI_TOOLS} names that URL. A tool CI installs from a release asset has a second "
            f"source, and a fetch nothing names has none. {remedy}"
        )

# ------------------------------------------- the rules a no_second_source entry is held to
for entry, repository in declines:
    url = str(entry["url"]).strip()

    if url not in written:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'url')}: no file under {GITHUB} writes "
            f"{one_line(url)}, so this entry declines a second source for an install that is no "
            f"longer here. Delete the entry, or correct its url to the text the file that "
            f"fetches those bytes writes."
        )

    digest = str(entry["digest"]).strip()
    pinned_in = Path(str(entry["pinned_in"]).strip())
    pin_text = pin_texts.get(pinned_in)
    if pin_text is None and pinned_in.is_file():
        pin_text = read_text(pinned_in)
    if not DIGEST.fullmatch(digest):
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'digest')}: this entry's digest is {one_line(digest)}, "
            f"which is not 64 lower-case hex characters. Write it bare, exactly as the file that "
            f"pins those bytes writes it."
        )
    elif pinned_in == CI_TOOLS or TEST_FILE in pinned_in.name:
        # Before the rule below reads that file, because it would find the digest there: the
        # entry's own `digest:` line is in the manifest, and a fixture's digests are the ones a
        # test feeds to a gate.
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'pinned_in')}: this entry names {pinned_in} as the file "
            f"that pins these bytes, and that file fetches nothing and checks nothing, so it "
            f"cannot be what pins them. Name the file whose install of these bytes checks the "
            f"digest — the workflow, action or Dockerfile a job runs."
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

    covered_by = row_repositories.get(repository.casefold())
    if covered_by:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'url')}: {covered_by} already covers {repository}, so "
            f"this tool has a second source and its call site reads it with {HELPER}. An entry "
            f"declines a URL nothing else serves; delete this one and fetch the bytes from the "
            f"row."
        )

    why = one_line(entry["why"])
    if why.strip(" .").lower() in REASON_PLACEHOLDERS or len(why) < REASON_MINIMUM:
        problems.append(
            f"{CI_TOOLS}:{key_line(entry, 'why')}: this entry's why is {why}, which "
            f"says nothing a reader could act on. Write at least {REASON_MINIMUM} characters "
            f"naming which second source was looked for, why there is none today, and what "
            f"building one would take. Whether that reason is true is the reviewer's judgement "
            f"and not this gate's, which is why it has to be readable."
        )

# The middle clause counts installs, not addresses: this manifest is not among the files rule 3
# reads, so a row's own URL and an entry's own url are no part of it. A count that included them
# would rise when the manifest grew and say nothing about what CI fetches.
# A file neither rule could read is named at the end, and the clause is absent where there is
# none: a reader is told which files the counts above left out rather than told a number of
# them, since what a reader of this line does next is open them.
unread = ""
if unreadable:
    unread = (
        f"; {len(unreadable)} file(s) under {GITHUB} are not valid UTF-8, so neither rule read "
        f"them: {', '.join(str(file) for file in unreadable)}"
    )
verdict(
    f"{CI_TOOLS} holds {len(readable)} row(s), each agreeing with the pin that installs it, and "
    f"{len(declines)} {NO_SECOND_SOURCE} entr{'y' if len(declines) == 1 else 'ies'}; "
    f"{named} tool asset URL(s) fetched by the {read_files} file(s) under {GITHUB} that rule 3 "
    f"reads, this manifest not among them, name one of those; "
    f"of {swept} file(s) under {GITHUB} swept, none names a tool blob by a registry reference"
    f"{unread}"
)
PY
