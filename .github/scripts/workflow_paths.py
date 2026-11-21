"""What a GitHub Actions workflow's path filters cover, read as GitHub and dorny/paths-filter read
them: a walk of the repository's files, a path glob as a regex, an ordered pattern list where `!`
negates, a dorny/paths-filter filter under its predicate quantifier, and a workflow's events; and
the YAML those filters are written in, read as strictly as those readers read it: a mapping's
repeated keys, a loader that refuses a file holding one, and the filters read by the very js-yaml
the action reads them with (load_paths_filter).

check-image-trigger-paths.sh and check-ci-filters.sh (through ci_filter_coverage.py) import it from
the repository they check, running `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3`
from that repository's root. check-workflow-files.sh imports it from beside its own file, which a
test fixture's link to that script leads back to.
"""

import base64
import datetime
import functools
import json
import math
import re
import shutil
import subprocess
from collections.abc import Callable, Iterator
from pathlib import Path

import yaml

# Directories a walk never enters: version control and installed dependencies.
SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}


def walk(root: Path = Path(".")) -> list[str]:
    """Every file under root, as a path relative to the working directory. It walks the tree rather
    than asking git, so an unsnapshotted change cannot read green locally."""
    found = []
    stack = [root]
    while stack:
        for entry in sorted(stack.pop().iterdir()):
            if entry.is_symlink() and entry.is_dir():
                continue
            if entry.is_dir():
                if entry.name not in SKIP_DIRS:
                    stack.append(entry)
            else:
                found.append(entry.as_posix())
    return found


def glob_regex(pattern: str) -> re.Pattern:
    """A path glob as a regex over a repository-relative path, with picomatch's meaning — the
    matcher dorny/paths-filter runs (https://github.com/micromatch/picomatch#globbing-features):
    `**` spans directories, `*` and `?` stay inside one. `{` and `}` read as literal characters,
    where picomatch expands `{a,b}` and GitHub documents no meaning for them, so a check refuses
    them in either (unread_patterns, unread_filter_patterns). GitHub's own `paths` agree except
    for `?`, `+` and `\\`, which unread_patterns finds. A .dockerignore line and a COPY source read
    the same: Docker matches them with Go's filepath.Match, where `{` is a literal character."""
    out, index = "", 0
    while index < len(pattern):
        if pattern.startswith("**/", index):
            out += "(?:.*/)?"
            index += 3
        elif pattern.startswith("**", index):
            out += ".*"
            index += 2
        elif pattern[index] == "*":
            out += "[^/]*"
            index += 1
        elif pattern[index] == "?":
            out += "[^/]"
            index += 1
        elif pattern[index] == "[" and "]" in pattern[index + 1 :]:
            end = pattern.index("]", index + 1)
            out += pattern[index : end + 1]
            index = end + 1
        else:
            out += re.escape(pattern[index])
            index += 1
    return re.compile(out)


class Filter:
    """An ordered pattern list where the last match wins and `!` negates, as GitHub reads
    `paths`. `everything` is an event with no path filter."""

    def __init__(self, patterns: list[str] | None, ignore: bool = False):
        self.everything = patterns is None
        self.ignore = ignore
        self.rules = [
            (p.startswith("!"), glob_regex(p.removeprefix("!"))) for p in (patterns or [])
        ]

    def covers(self, path: str) -> bool:
        if self.everything:
            return True
        matched = False
        for negated, regex in self.rules:
            if regex.fullmatch(path):
                matched = not negated
        return not matched if self.ignore else matched


class PathsFilter:
    """One dorny/paths-filter filter, as v4.0.3 evaluates it (src/filter.ts:58-66,133-149,195-213
    at ceb8a2b8f2d89434be7ff52d3de7ec3738c5cc9d), under one of the two quantifiers this models.
    Under `some`, the default, a file matches when any pattern matches it; picomatch reads a
    leading `!` as "not this pattern", so there a `!` pattern matches every file outside it. Under
    `some-with-excludes` a file matches when a pattern without `!` matches it and no `!` pattern
    does, so a `!` pattern excludes."""

    QUANTIFIERS = ("some", "some-with-excludes")

    def __init__(self, patterns: list[str], quantifier: str):
        if quantifier not in self.QUANTIFIERS:
            raise ValueError(f"predicate-quantifier {quantifier} is not modelled")
        self.excludes = quantifier == "some-with-excludes"
        self.rules = [
            (p.startswith("!"), glob_regex(p.removeprefix("!"))) for p in patterns
        ]

    def covers(self, path: str) -> bool:
        if self.excludes:
            matched = [negated for negated, regex in self.rules if regex.fullmatch(path)]
            return False in matched and True not in matched
        return any(bool(regex.fullmatch(path)) != negated for negated, regex in self.rules)


def events(document: dict) -> dict:
    """The workflow's `on:` as a mapping; YAML 1.1 reads a bare `on` key as True."""
    triggers = document.get("on", document.get(True))
    if isinstance(triggers, str):
        return {triggers: None}
    if isinstance(triggers, list):
        return {name: None for name in triggers}
    return triggers if isinstance(triggers, dict) else {}


def unread_patterns(patterns: list) -> list:
    """The patterns glob_regex would misread as GitHub reads them: GitHub's filter patterns give
    `?` and `+` regex meanings (zero or one, one or more of the preceding character) and read `\\`
    as an escape
    (https://docs.github.com/en/actions/writing-workflows/workflow-syntax-for-github-actions#filter-pattern-cheat-sheet),
    and that cheat sheet documents no meaning for `{` or `}`, which glob_regex reads literally.
    A check refuses these rather than guess."""
    return [p for p in patterns if any(char in str(p) for char in "?+\\{}")]


def unread_filter_patterns(patterns: list) -> list:
    """The patterns glob_regex would misread as dorny/paths-filter reads them: picomatch expands a
    brace (`{a,b}`, `{a..c}`), which glob_regex reads as literal characters. A check refuses these
    rather than guess."""
    return [p for p in patterns if any(char in str(p) for char in "{}")]


# The tag a plain `<<` key resolves to: a YAML merge key, which hands its mapping's keys to the
# mapping it is written in rather than being one of them.
MERGE_TAG = "tag:yaml.org,2002:merge"


def repeated_keys(
    node: yaml.MappingNode, normal: Callable[[yaml.ScalarNode], str | None]
) -> Iterator[tuple[int, int]]:
    """Each key of one composed mapping that repeats an earlier key of that same mapping, as the
    positions in node.value of its first copy and of the repeat, in the order they are written. Two
    keys are one key when `normal` gives them the same text; a key it gives None is compared with
    none, and so is a key that is not a scalar. A key written again as an alias of itself is a
    repeat, though PyYAML composes the alias to the very node its anchor composed, so the two
    copies are one node: positions tell them apart where nodes cannot, and key_mark says where
    each is written. The mapping alone is read, not the nodes below it: whoever walks a tree calls
    this on each mapping of it. check-workflow-files.sh folds keys to upper case, as GitHub's
    workflow parser compares them; UniqueKeyLoader compares them exactly, as js-yaml does."""
    first_seen: dict[str, int] = {}
    for position, (name, _) in enumerate(node.value):
        if not isinstance(name, yaml.ScalarNode):
            continue
        key = normal(name)
        if key is None:
            continue
        if key in first_seen:
            yield first_seen[key], position
        else:
            first_seen[key] = position


class KeyMarkLoader(yaml.SafeLoader):
    """yaml.SafeLoader recording, on each mapping node it composes, where each of its keys is
    written and the alias it is written as, if any (`key_marks`, in the order of node.value). The
    node of a key written as an alias is the node its anchor composed, whose mark is the anchor's,
    so the node alone cannot say on which line a repeat through an alias stands."""

    def compose_node(self, parent, index):
        # A mapping's key is composed with no index; its value with the key as the index.
        if isinstance(parent, yaml.MappingNode) and index is None:
            event = self.peek_event()
            alias = event.anchor if isinstance(event, yaml.AliasEvent) else None
            parent.__dict__.setdefault("key_marks", []).append((event.start_mark, alias))
        return super().compose_node(parent, index)


def key_mark(node: yaml.MappingNode, position: int) -> tuple[yaml.Mark, str | None]:
    """Where the key at `position` of a mapping is written, and the alias it is written as or None:
    the record KeyMarkLoader kept while it composed the node, or the key node's own mark for a node
    another loader composed."""
    marks = node.__dict__.get("key_marks")
    if marks is not None and len(marks) == len(node.value):
        return marks[position]
    return node.value[position][0].start_mark, None


def written_key(name: yaml.ScalarNode) -> str | None:
    """A key as js-yaml compares it: its text exactly, and a merge key not at all, since js-yaml
    lets a mapping hold any number of `<<` keys (lib/loader.js, storeMappingPair)."""
    return None if name.tag == MERGE_TAG else name.value


class UniqueKeyLoader(KeyMarkLoader):
    """yaml.SafeLoader, refusing a mapping that holds one key twice. yaml.safe_load keeps the last
    copy and reports nothing, so a second `ci_router:` holding only 'docs/**' would leave the
    filter that and nothing else; js-yaml, which dorny/paths-filter reads its filters with, and
    GitHub's workflow parser both refuse the file instead, and so does this loader, in a mapping
    at any depth.

    Only the keys written in a mapping are compared: a key a merge key (`<<`) supplies may be
    overridden by one written beside it, as js-yaml allows. So the test runs as each mapping node
    is composed, when it holds exactly what was written, and not in the constructor, whose
    flatten_mapping rewrites a mapping's node in place once another mapping merges it, which can
    be before that mapping is itself constructed. Anchors and aliases read as yaml.safe_load reads
    them: an alias is the node its anchor composed, tested once, and a key written again as an
    alias of a key or of a value is a repeated key, which js-yaml refuses as well. A merge key
    written as an alias is refused at that alias: js-yaml reads it as a key named `<<` (once, and
    as a duplicated key twice), where PyYAML merges it, so the two would read different mappings.
    The refusal names the line each copy is written on (KeyMarkLoader) and the alias a repeat is
    written as, and carries no mark, whose snippet would only repeat the line, and whose stream
    name is `<unicode string>` for the text every reader passes, where the reader names the file."""

    def compose_node(self, parent, index):
        # An alias as a mapping's key of a merge key is no merge to js-yaml (lib/loader.js,
        # readBlockMapping and storeMappingPair).
        if isinstance(parent, yaml.MappingNode) and index is None and self.check_event(yaml.AliasEvent):
            event = self.peek_event()
            merge = self.anchors.get(event.anchor)
            if isinstance(merge, yaml.ScalarNode) and merge.tag == MERGE_TAG:
                raise yaml.composer.ComposerError(
                    problem=f"the mapping key '<<' of line {event.start_mark.line + 1} is the merge key of line "
                            f"{merge.start_mark.line + 1} written as an alias (*{event.anchor}): js-yaml "
                            f"(dorny/paths-filter's reader) reads it as a key named '<<', where PyYAML merges it, "
                            f"so the two would read different mappings")
        return super().compose_node(parent, index)

    def compose_mapping_node(self, anchor):
        node = super().compose_mapping_node(anchor)
        for first, repeat in repeated_keys(node, written_key):
            (mark, alias), (first_mark, _) = key_mark(node, repeat), key_mark(node, first)
            written = f", written as the alias *{alias}," if alias is not None else ""
            raise yaml.composer.ComposerError(
                problem=f"the mapping key '{node.value[repeat][0].value}' of line {mark.line + 1}{written} is the key "
                        f"of line {first_mark.line + 1} again: js-yaml (dorny/paths-filter's reader) and GitHub's "
                        f"workflow parser refuse a repeated key, where yaml.safe_load keeps the last copy and "
                        f"reports nothing")
        return node


# The dorny/paths-filter commits this repository's workflows pin, each with the js-yaml version that
# commit's package-lock.json pins (packages["node_modules/js-yaml"]): the copy its dist/index.js
# bundles and calls as `jsyaml.load(text)` (src/filter.ts:104), under the Node its action.yml names
# (`runs.using: node24`). check-ci-filters.sh refuses a pin missing here (its rule 21), and
# load_paths_filter refuses an installed js-yaml that is not the version recorded, so a bump of the
# action's pin fails CI until its js-yaml is recorded here and package.json's alias names it.
PATHS_FILTER_JS_YAML = {"ceb8a2b8f2d89434be7ff52d3de7ec3738c5cc9d": "4.1.0"}
# Root package.json's npm alias of that js-yaml (`npm:js-yaml@4.1.0`), whose integrity bun.lock
# holds, beside the repository's own js-yaml. It is read from the checkout this file is in, which a
# test fixture's link to this file leads back to.
JS_YAML_ALIAS = "js-yaml-paths-filter"
JS_YAML_DIR = Path(__file__).resolve().parents[2] / "node_modules" / JS_YAML_ALIAS

# Node's reading of the text on stdin with the js-yaml of argv[1], as a tree each value of which
# says its kind, since JSON alone would make a number of NaN null and a date text: {"doc": tree} or
# {"error": the first line of js-yaml's refusal}.
JS_YAML_READER = r"""
const yaml = require(process.argv[1]);
const chunks = [];
process.stdin.on("data", (chunk) => chunks.push(chunk));
process.stdin.on("end", () => {
  const tree = (v) =>
    v === null || v === undefined ? ["null"]
    : typeof v === "boolean" ? ["bool", v]
    : typeof v === "number" ? ["num", String(v)]
    : typeof v === "string" ? ["str", v]
    : v instanceof Date ? ["date", Number.isNaN(v.getTime()) ? "Invalid Date" : v.toISOString()]
    : v instanceof Uint8Array ? ["bytes", Buffer.from(v).toString("base64")]
    : Array.isArray(v) ? ["list", v.map(tree)]
    : typeof v === "object" ? ["map", Object.keys(v).map((k) => [k, tree(v[k])])]
    : ["other", typeof v];
  let out;
  try {
    out = { doc: tree(yaml.load(Buffer.concat(chunks).toString("utf8"))) };
  } catch (error) {
    out = { error: String((error && error.message) || error).split("\n")[0] };
  }
  process.stdout.write(JSON.stringify(out));
});
"""


class PathsFilterReadError(yaml.YAMLError):
    """dorny/paths-filter's js-yaml refuses the text, or reads it otherwise than PyYAML does."""


@functools.cache
def js_yaml_reading(text: str) -> dict:
    """The text as the js-yaml of JS_YAML_DIR reads it under Node: {"doc": tree} or {"error": why}.
    A missing node, a missing or other js-yaml, or a reader that did not answer ends the check by
    name, since none of them is an answer about the text."""
    node = shutil.which("node")
    if node is None:
        raise SystemExit("::error::node is not on PATH: dorny/paths-filter reads its filters with js-yaml under "
                         "Node, and this check reads them the same way; install Node (the action runs node24)")
    try:
        installed = json.loads((JS_YAML_DIR / "package.json").read_text()).get("version")
    except (OSError, ValueError) as exc:
        raise SystemExit(f"::error::{JS_YAML_DIR} is not an installed js-yaml ({exc}): run bun install "
                         f"--frozen-lockfile, which installs package.json's {JS_YAML_ALIAS} alias") from exc
    recorded = set(PATHS_FILTER_JS_YAML.values())
    if recorded != {installed}:
        raise SystemExit(f"::error::the installed {JS_YAML_ALIAS} is js-yaml {installed}, and the dorny/paths-filter "
                         f"pins of workflow_paths.PATHS_FILTER_JS_YAML read their filters with js-yaml "
                         f"{', '.join(sorted(recorded))}: set package.json's alias to npm:js-yaml@<that version>")
    # From js-yaml's own directory: Node reads the package.json nearest its working directory even
    # for -e, and the repository being checked may hold one it cannot parse, which is that check's
    # finding to report, not a reason the filters went unread.
    run = subprocess.run([node, "-e", JS_YAML_READER, str(JS_YAML_DIR)], input=text, capture_output=True,
                         text=True, encoding="utf-8", check=False, cwd=JS_YAML_DIR)
    try:
        answer = json.loads(run.stdout)
    except ValueError:
        answer = None
    if run.returncode != 0 or not isinstance(answer, dict) or not ({"doc", "error"} & answer.keys()):
        said = " ".join((run.stderr or run.stdout).split())[:300]
        raise SystemExit(f"::error::node exited {run.returncode} reading the filters with js-yaml {installed} "
                         f"and gave no reading: {said}")
    return answer


def _same_number(value, written: str) -> bool:
    """Whether PyYAML's number `value` is js-yaml's double, written as String() writes it: exactly,
    since Python compares an int with a float by their values (9007199254740993 is not the double
    9007199254740992, and an int past the largest double is not Infinity), NaN with NaN."""
    double = float(written)
    if isinstance(value, float) and math.isnan(value):
        return math.isnan(double)
    return value == double


def _instant(value) -> str | None:
    """A PyYAML date or timestamp as an ISO instant to the microsecond, or None."""
    if isinstance(value, datetime.datetime):
        if value.tzinfo is not None:
            value = value.astimezone(datetime.timezone.utc)
        return (f"{value.year:04d}-{value.month:02d}-{value.day:02d}T{value.hour:02d}:{value.minute:02d}:"
                f"{value.second:02d}.{value.microsecond:06d}Z")
    if isinstance(value, datetime.date):
        return f"{value.year:04d}-{value.month:02d}-{value.day:02d}T00:00:00.000000Z"
    return None


def _js_said(tree: list) -> str:
    """A js-yaml reading as a phrase."""
    kind = tree[0]
    if kind in ("map", "list"):
        return "a mapping" if kind == "map" else "a sequence"
    if kind == "null":
        return "null"
    if kind == "str":
        return f"the text {tree[1]!r}"
    return {"bool": "the boolean", "num": "the number", "date": "the instant", "bytes": "the bytes (base64)",
            "other": "a JavaScript"}[kind] + f" {str(tree[1]).lower() if kind == 'bool' else tree[1]}"


def reading_difference(tree: list, value, where: str = "the document") -> str | None:
    """Where PyYAML's reading `value` differs from js-yaml's reading `tree` (js_yaml_reading), as a
    sentence, or None where the two are one reading: the same keys in every mapping, compared as
    the text js-yaml keys every mapping by and the Python key PyYAML keys it by, the same items in
    every sequence, and the same scalar at every leaf (a number by its value, a date by its
    instant, bytes by their content)."""
    kind = tree[0]
    if kind == "map" and isinstance(value, dict):
        keys = [key for key, _ in tree[1]]
        only_js = [key for key in keys if key not in value]
        only_py = [key for key in value if key not in keys]
        if only_js or only_py:
            return (f"{where} holds the keys {', '.join(map(repr, only_js)) or 'no other'} to js-yaml and "
                    f"{', '.join(map(repr, only_py)) or 'no other'} to PyYAML")
        for key, item in tree[1]:
            found = reading_difference(item, value[key], f"{where} > {key!r}")
            if found:
                return found
        return None
    if kind == "list" and isinstance(value, list):
        if len(tree[1]) != len(value):
            return f"{where} holds {len(tree[1])} items to js-yaml and {len(value)} to PyYAML"
        for index, (item, py_item) in enumerate(zip(tree[1], value)):
            found = reading_difference(item, py_item, f"{where} > item {index + 1}")
            if found:
                return found
        return None
    same = {
        "null": lambda: value is None,
        "bool": lambda: isinstance(value, bool) and value == tree[1],
        "num": lambda: isinstance(value, (int, float)) and not isinstance(value, bool) and _same_number(value, tree[1]),
        "str": lambda: isinstance(value, str) and value == tree[1],
        # js-yaml's Date holds milliseconds (toISOString writes three digits), PyYAML's microseconds.
        "date": lambda: _instant(value) == re.sub(r"\.(\d{3})Z$", r".\g<1>000Z", tree[1]),
        "bytes": lambda: isinstance(value, bytes) and base64.b64encode(value).decode() == tree[1],
    }.get(kind, lambda: False)
    if same():
        return None
    return f"{where} is {_js_said(tree)} to js-yaml and {value!r} ({type(value).__name__}) to PyYAML"


def load_paths_filter(text: str):
    """dorny/paths-filter's filters, the text of .github/ci-filters.yaml or of a step's inline
    `filters`, as PyYAML reads them (UniqueKeyLoader, which refuses a repeated key itself) once the
    action's own js-yaml has read the text under Node (PATHS_FILTER_JS_YAML) and found what PyYAML
    found. A text js-yaml refuses is refused with js-yaml's reason: the action cannot read it, and
    every job it gates is skipped. A text the two read differently is refused naming the first
    difference: js-yaml keys a mapping by the text of each key's value with its default schema, so
    `01` and `1` are one key to it, where PyYAML's YAML 1.1 reads `on` and `yes` as one key True
    and `010` as 8, and either way this check would judge filters the action does not read."""
    reading = yaml.load(text, Loader=UniqueKeyLoader)
    answer = js_yaml_reading(text)
    version = PATHS_FILTER_JS_YAML[next(iter(PATHS_FILTER_JS_YAML))]
    if "error" in answer:
        raise PathsFilterReadError(f"js-yaml {version}, which dorny/paths-filter reads it with, refuses it: "
                                   f"{answer['error']}")
    difference = reading_difference(answer["doc"], reading)
    if difference:
        raise PathsFilterReadError(f"js-yaml {version}, which dorny/paths-filter reads it with, and PyYAML, which "
                                   f"this check reads it with, read it differently: {difference}; quote the "
                                   f"scalar so both read it as text")
    return reading
