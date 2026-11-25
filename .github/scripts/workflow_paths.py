"""What a GitHub Actions workflow's path filters cover, read as GitHub and dorny/paths-filter read
them: a walk of the repository's files, a path glob as a regex, an ordered pattern list where `!`
negates, a dorny/paths-filter filter under its predicate quantifier, and a workflow's events; the
YAML those filters are written in, read as strictly as those readers read it: a mapping's repeated
keys, and a loader that refuses a file holding one. And what a Docker build's context holds: a
`.dockerignore` read as Docker reads it, and the instructions of a Dockerfile, the sources and the
destination of a COPY or ADD, and the bind mounts of a RUN, each read one way for each check that
reads them.

check-image-trigger-paths.sh, check-image-build-inputs.sh and check-ci-filters.sh (through
ci_filter_coverage.py) import it from the repository they check, running
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root.
check-workflow-files.sh imports it from beside its own file, which a test fixture's link to that
script leads back to.
"""

import json
import posixpath
import re
import shlex
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


def dockerignore_filter(path: str | None):
    """True for a context path the .dockerignore drops: a pattern matches the path or a
    directory above it, and a later `!` line takes it back."""
    if path is None:
        return lambda _: False
    rules = []
    for line in docker_lines(read_docker_file(path)):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        negated = line.startswith("!")
        pattern = posixpath.normpath(line.removeprefix("!").lstrip("/"))
        rules.append((negated, glob_regex(pattern)))

    def ignored(relative: str) -> bool:
        parts = relative.split("/")
        prefixes = ["/".join(parts[: n + 1]) for n in range(len(parts))]
        dropped = False
        for negated, regex in rules:
            if any(regex.fullmatch(prefix) for prefix in prefixes):
                dropped = not negated
        return dropped

    return ignored


def read_docker_file(path: str | Path) -> str:
    """The text of a Dockerfile or a .dockerignore as its bytes hold it: read with no translation of
    its line ends, which Path.read_text would make, turning a lone carriage return into a line
    feed before docker_lines reads it."""
    with open(path, newline="") as file:
        return file.read()


def docker_lines(text: str) -> list[str]:
    """The lines of a Dockerfile or a .dockerignore as Docker reads them: Go's bufio.ScanLines,
    which BuildKit's parser and the .dockerignore reader scan with, ends a line at a line feed
    alone and drops one carriage return before it. So a vertical tab, a form feed or a lone
    carriage return stays in its line, where str.splitlines would end one there, and inside an
    instruction it is white space (INSTRUCTION_WORD_END). Read the text with read_docker_file."""
    lines = text.split("\n")
    if lines[-1] == "":
        lines.pop()
    return [line.removesuffix("\r") for line in lines]


class DockerfileUnreadable(ValueError):
    """An instruction of a Dockerfile whose operands a check cannot read; the message says how to
    write it so a check can."""


# The white space that ends the word of a Dockerfile instruction: BuildKit's reWhitespace
# (frontend/dockerfile/parser/parser.go), at which its splitCommand splits the trimmed instruction
# once, into the command and its arguments. So `COPY<tab>src dst` is a COPY, as Docker reads it.
INSTRUCTION_WORD_END = re.compile(r"[\t\v\f\r ]+")


def _command(instruction: str) -> tuple[str, str]:
    """(command in upper case, arguments) of one instruction, split as BuildKit's splitCommand
    splits it (INSTRUCTION_WORD_END)."""
    command, *arguments = INSTRUCTION_WORD_END.split(instruction.strip(), maxsplit=1)
    return command.upper(), "".join(arguments).strip()


def dockerfile_instructions(text: str) -> Iterator[tuple[int, str, str]]:
    """(line, command, arguments) of each instruction of a Dockerfile, as Docker reads one: its
    lines as BuildKit ends them (docker_lines), comment lines dropped, also inside a continuation,
    continuation lines joined, blank lines skipped, the command in upper case and ended by any white
    space (_command), the line the one the instruction starts on. check-image-trigger-paths.sh
    reads the context sources of each, and check-image-build-inputs.sh where each puts them."""
    pending, start = "", 0
    for number, raw in enumerate(docker_lines(text), start=1):
        stripped = raw.strip()
        if not pending:
            start = number
        if stripped.startswith("#") or (not pending and not stripped):
            continue
        if stripped.endswith("\\"):
            pending += stripped[:-1] + " "
            continue
        command, arguments = _command(pending + stripped)
        pending = ""
        yield start, command, arguments
    if pending:
        yield start, *_command(pending)


def copy_operands(arguments: str) -> tuple[list[str], str] | None:
    """(context sources, destination) of a COPY or ADD from its arguments, in its JSON form or its
    plain form; None for one that reads another stage or an image (`--from`). A URL or a git
    address is no source of the context. Raises DockerfileUnreadable for malformed JSON, quotes a
    shell could not read, no source and destination, and a heredoc source, whose content no file of
    the context holds."""
    if arguments.startswith("["):
        try:
            entries = json.loads(arguments)
        except json.JSONDecodeError as error:
            raise DockerfileUnreadable(
                f"malformed JSON-array form: {error.msg}; write it as valid JSON, or as a "
                f"plain argument list"
            ) from error
        flags = []
    else:
        try:
            words = shlex.split(arguments)
        except ValueError as error:
            raise DockerfileUnreadable(
                f"unparseable: {error}; balance its quotes so a shell could read it"
            ) from error
        flags = [word for word in words if word.startswith("--")]
        entries = [word for word in words if not word.startswith("--")]
    if any(flag.startswith("--from=") for flag in flags):
        return None
    if not isinstance(entries, list) or len(entries) < 2:
        raise DockerfileUnreadable("no source and destination; give it both")
    sources = []
    for source in entries[:-1]:
        if "://" in source or source.startswith("git@"):
            continue
        if source.startswith("<<"):
            raise DockerfileUnreadable(
                "a heredoc source, which this check cannot read; write the content into a "
                "file the context carries, and COPY that"
            )
        sources.append(source)
    return sources, entries[-1]


def bind_mounts(arguments: str) -> list[tuple[str, str | None]]:
    """(source, target) of each bind mount of a RUN, from its arguments, that reads the context: a
    `--mount=` of type bind, which is the type when it names none, and with no `from=`. Its source
    is `source` or `src`, the whole context when it names neither; its target is `target`, `dst` or
    `destination`, None when it names none. Raises DockerfileUnreadable for quotes a shell could not
    read."""
    try:
        words = shlex.split(arguments)
    except ValueError as error:
        raise DockerfileUnreadable(f"unparseable: {error}; balance its quotes so a shell could read it") from error
    mounts = []
    for word in words:
        if not word.startswith("--mount="):
            continue
        options = dict(
            item.split("=", 1) if "=" in item else (item, "")
            for item in word[len("--mount=") :].split(",")
        )
        if options.get("type", "bind") == "bind" and "from" not in options:
            source = options.get("source", options.get("src", "."))
            target = options.get("target", options.get("dst", options.get("destination")))
            mounts.append((source, target))
    return mounts


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
    """One dorny/paths-filter filter, as v4.0.3 evaluates it (its `PredicateQuantifier`, the
    `SOME_WITH_EXCLUDES` case of its predicate and `createRuleItem`, at
    https://github.com/dorny/paths-filter/blob/ceb8a2b8f2d89434be7ff52d3de7ec3738c5cc9d/src/filter.ts), under one of the two quantifiers this models.
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
) -> Iterator[tuple[yaml.ScalarNode, yaml.ScalarNode]]:
    """Each key of one composed mapping that repeats an earlier key of that same mapping, as the
    node of its first copy and the node of the repeat, in the order they are written. Two keys are
    one key when `normal` gives them the same text; a key it gives None is compared with none,
    and so is a key that is not a scalar. The mapping alone is read, not the nodes below it:
    whoever walks a tree calls this on each mapping of it. check-workflow-files.sh folds keys to
    upper case, as GitHub's workflow parser compares them; UniqueKeyLoader compares them exactly,
    as js-yaml does."""
    first_seen: dict[str, yaml.ScalarNode] = {}
    for name, _ in node.value:
        if not isinstance(name, yaml.ScalarNode):
            continue
        key = normal(name)
        if key is None:
            continue
        first = first_seen.setdefault(key, name)
        if first is not name:
            yield first, name


def written_key(name: yaml.ScalarNode) -> str | None:
    """A key as js-yaml compares it: its text exactly, and a merge key not at all, since js-yaml
    lets a mapping hold any number of `<<` keys (lib/loader.js, storeMappingPair)."""
    return None if name.tag == MERGE_TAG else name.value


class UniqueKeyLoader(yaml.SafeLoader):
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
    them: an alias is the node its anchor composed, tested once. The refusal names both lines
    itself and carries no mark, whose snippet would only repeat the line, and whose stream name is
    `<unicode string>` for the text every reader passes, where the reader names the file."""

    def compose_mapping_node(self, anchor):
        node = super().compose_mapping_node(anchor)
        for first, repeat in repeated_keys(node, written_key):
            raise yaml.composer.ComposerError(
                problem=f"the mapping key '{repeat.value}' of line {repeat.start_mark.line + 1} is the key of "
                        f"line {first.start_mark.line + 1} again: js-yaml (dorny/paths-filter's reader) and "
                        f"GitHub's workflow parser refuse a repeated key, where yaml.safe_load keeps the last "
                        f"copy and reports nothing")
        return node
