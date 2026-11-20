"""Point 5's checks of the claim's judging, decided from the transcripts the sandbox kept, in code,
before any answer is compared ("The claim's bundle and its arguments", version 6).

- The session's own transcript holds the one `eval` call of the sandbox's `CELL` and nothing else
  (`session_problems`); each recorded `agent()` call has one judge transcript (`judge_transcripts`).
- No judge made a tool call, attempted, failed, denied or answered, but what point 5's tool rule
  allows (`tool_call_problems`). The rule is applied as each of its versions writes it
  (`RULE_VERSIONS`), over the same transcripts, and version 6's is the one that counts:
  - version 3: a `read`, `grep` or `glob` of its own bundle file or of a `skill://approval-review`
    file, and nothing else, its `yield` answer included;
  - version 4: a `read` or `grep` of its own bundle file or of a skill file; a `glob` whose scope
    is exactly one of four, its own bundle file, the bundle directory, a skill file or the skill
    directory; and its `yield` answer, exempt from the path check;
  - version 5: version 4's, and a `read` of the bundle directory, of the skill directory or of its
    `references/`, which answers the names, sizes and ages of a directory's entries and no text;
  - version 6: a text read (a `read` of a file, any `grep`) whose every target is its own bundle
    file or a skill file, a `grep` of a directory being a text read of every file in it; a listing
    (any `glob`, and a `read` that the checker establishes resolves to a directory of the two
    trees and answers only names, sizes and ages, `listed_directory`) whose whole scope lies inside
    the work tree, the judges' working directory, or the skill tree, every entry it returns one
    its tree's manifest recorded before the first judge holds; and its `yield` answer
    (`text_or_listing_problem`).
  Each target is checked as given, as the tool's default fills it, and normalized as it resolves
  on the sandbox's file system (`target_of`, `resolved`), and so is every scope the tool's result
  says it read; every path the result returns must lie inside the manifested trees
  (`detail_paths`, `in_trees`). A target is read in the grammar this check models of omp
  18.16.0's path and glob syntax (`expansions`, `SELECTOR`); one that holds any other syntax, or
  brace alternatives or a character class that could spell a parent step, a `/` or an absolute
  part, is refused, not placed.
- Each judge was asked point 2's model, none is marked a fallback, and each model omp reports
  serving it is that model (`model_problems`).
- Each answer is valid by the frozen `JUDGE_SCHEMA` (`answer_problem`), which may use only the
  keywords this validator reads (`schema_refusal`); the verdict is recomputed by point 2's rule
  (`rule_verdict`).
"""

import sys

sys.dont_write_bytecode = True

import json  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
from collections import Counter  # noqa: E402
from dataclasses import dataclass  # noqa: E402

import s10_sandbox as SANDBOX  # noqa: E402
from s10_store import under  # noqa: E402

# Point 5's tool rule, by the versions of the section that wrote it, and the one that counts.
RULE_VERSIONS = (3, 4, 5, 6)
COUNTING_RULE = 6

# A `read` of a directory answers a listing (omp 18.16.0's read tool, two levels deep): lines that
# name no entry (the root, an empty directory, a continuation, a line asked past the end), and one
# line an entry, indented, `- `, its name (a directory's with a trailing `/`), then its size and age.
LISTING_NOTE = re.compile(r"\.|\(empty directory\)|\[\d+ more lines in listing\.[^\]]*\]|"
                          r"Line \d+ is beyond end of listing .*")
LISTING_ENTRY = re.compile(r"\s*- (?P<name>[^\s/]+)/?(?: .*)?")

# The tool omp's subagents return their schema's answer by, exempt from the path check from version
# 4; the tools a judge may call on a file system target; and, by tool, the arguments that name no
# target (search text, a count, a switch, the call's stated intent), as omp 18.16.0's schemas name
# them. Any other argument is taken for a target the rule cannot place, which makes the case a
# deviation.
ANSWER_TOOL = "yield"
READ_TOOLS = ("read", "grep", "glob")
NO_TARGET_ARGUMENTS = {
    "read": {"i"},
    "grep": {"pattern", "case", "gitignore", "skip", "i"},
    "glob": {"limit", "hidden", "gitignore", "i"},
}
SKILL_SCHEME = "skill://"
SKILL_NAME = "approval-review"
# Where omp 18.16.0's glob tool starts a path's pattern: the first `/`-part holding one of these
# (`J3`/`FB` in its path helpers); the parts before it are the directory its walk starts from.
GLOB_START = re.compile(r"[*?\[{]")

# Syntax a target may not hold, because omp 18.16.0 reads it in a way this check does not model:
# its path helpers take a backslash for a `/`, strip enclosing double quotes and, where a path as
# written does not exist, split it on blank space or a `,` outside braces (and always on `;`,
# which `call_problem` splits; `expansions` refuses a `,` outside braces); its URL patterns decode
# `%` escapes; and parentheses, `|` and `!` are the syntax of extended globs and negation. A target
# holding any of them is refused, never placed.
UNMODELLED = re.compile(r"[\\\"'%()|!\s]")

# A line or page selector after a path (`judge-1.md:1-40`, `:raw`, `:5-16,960-973`), as omp's read
# tool takes one; any other text after the first `:` is refused.
SELECTOR = re.compile(r"(?::(?:raw|img|conflicts|-\d+|\d+(?:-\d*|\+\d+)?(?:,\d+(?:-\d+)?)*))*")

# What a character class may hold: letters, digits, `_` and ranges between two of them, with a
# literal `-` first or last. Every such character and range lies above `/` and `.`, so no class can
# spell a parent step or a separator; a negated, empty or other class is refused.
CLASS = re.compile(r"-?(?:[A-Za-z0-9_](?:-[A-Za-z0-9_])?)+-?")

# The most strings one target's brace alternatives may expand to before it is refused.
MAX_EXPANSIONS = 256

# The JSON Schema keywords and types `answer_problem` reads: a schema with any other is refused, so
# no answer passes a rule this validator would not check.
SCHEMA_KEYWORDS = {"type", "enum", "required", "properties"}
SCHEMA_TYPES = {"object", "string", "null", "boolean", "array", "integer", "number"}


def entries_of(data):
    """A transcript's entries, one JSON object a line; `ValueError` where a line is no JSON."""
    entries = [json.loads(line) for line in data.decode("utf-8").splitlines() if line.strip()]
    if not all(isinstance(entry, dict) for entry in entries):
        raise ValueError("a transcript line is no object")
    return entries


def tool_calls_of(entries):
    """Every tool call a transcript records, attempted, failed, denied or answered, by its id: its
    name and every set of arguments recorded for it, from the assistant's call, the execution's
    start and the result, wherever an entry names a tool call; and each result's details with its
    text (`answers`)."""
    calls = {}

    def note(call_id, name, arguments=None, details=None, content=None):
        one = calls.setdefault(str(call_id), {"names": set(), "arguments": [], "details": [], "answers": []})
        if name is not None:
            one["names"].add(name)
        if arguments is not None:
            one["arguments"].append(arguments)
        if details is not None:
            one["details"].append(details)
        if isinstance(content, list):
            text = "".join(part.get("text", "") for part in content
                           if isinstance(part, dict) and isinstance(part.get("text"), str))
            one["answers"].append({"details": details, "text": text})

    def walk(value):
        if isinstance(value, dict):
            if value.get("type") == "toolCall":
                note(value.get("id"), value.get("name"), arguments=value.get("arguments", value.get("input")))
            if "toolCallId" in value:
                note(value.get("toolCallId"), value.get("toolName"), arguments=value.get("args"),
                     details=value.get("details"), content=value.get("content"))
            for key, inner in value.items():
                if key == "data" and isinstance(inner, str) and inner.startswith("{"):
                    try:
                        inner = json.loads(inner)
                    except ValueError:
                        pass
                walk(inner)
        elif isinstance(value, list):
            for inner in value:
                walk(inner)

    walk(entries)
    return calls


@dataclass(frozen=True)
class Scope:
    """What point 5 (each version) lets one judge's `read`, `grep` and `glob` reach, every path as it
    resolves on the sandbox's file system: the judge's own bundle file and the directory that holds
    it, the skill directory that `skill://approval-review` resolves to (the skill tree) and its three
    files, the working directory a path is relative to (the work tree, from version 6), the links
    the sandbox's listing records; each entry of the two trees that listing records, by its path,
    with its kind (`d`, `f`, `l` or `o`); and each entry the two trees' manifests recorded before
    the first judge hold, the trees themselves among them, by its path, with its kind."""

    work: str
    bundle: str
    bundle_dir: str
    skill_dir: str
    skill_files: frozenset
    links: dict
    work_tree: str = ""
    tree_entries: dict = None
    manifested: dict = None


def links_of(listing):
    """Every link the sandbox's listing records, by its path, with its target as written."""
    links = {}
    for line in listing:
        if line.startswith("l "):
            path, target = line.split(" ", 3)[3].split(" -> ", 1)
            links[path] = target
    return links


def resolved(path, links):
    """`path` as it resolves on the sandbox's file system: each link the listing records followed
    where it stands, and each parent step (`..`) taken after the link before it, as the kernel
    takes them. None where the links loop."""
    parts = [part for part in path.split("/") if part not in ("", ".")]
    done, hops = "/", 0
    while parts:
        part = parts.pop(0)
        if part == "..":
            done = os.path.dirname(done)
            continue
        candidate = os.path.join(done, part)
        target = links.get(candidate)
        if target is None:
            done = candidate
            continue
        hops += 1
        if hops > 40:
            return None
        if target.startswith("/"):
            done = "/"
        parts = [one for one in target.split("/") if one not in ("", ".")] + parts
    return done


def scope_of(bundle, work, listing, manifests=None):
    """One judge's scope, from its bundle's path, the sandbox's listing, and the manifests the
    sandbox recorded around the judges (`manifests`, their `before`)."""
    links = links_of(listing)
    skill_dir = resolved(SANDBOX.SKILL_DIR, links)
    own = resolved(bundle, links)
    work_tree = resolved(work, links)
    entries = {}
    for line in listing:
        path = SANDBOX.listed_path(line)
        if under(path, work_tree) or under(path, skill_dir):
            entries[path] = line[0]
    before = (manifests or {}).get("before") if isinstance(manifests, dict) else None
    manifested = {}
    for name, tree in (("work", work_tree), ("skill", skill_dir)):
        held = (before or {}).get(name) if isinstance(before, dict) else None
        if isinstance(held, list):
            manifested[tree] = "d"
            for one in held:
                if isinstance(one, list) and len(one) == 2 and isinstance(one[0], str):
                    manifested[os.path.join(tree, one[0])] = one[1]
    return Scope(work=work, bundle=own, bundle_dir=os.path.dirname(own), skill_dir=skill_dir,
                 skill_files=frozenset(os.path.join(skill_dir, name) for name in SANDBOX.SKILL_FILES),
                 links=links, work_tree=work_tree, tree_entries=entries, manifested=manifested)


def class_taken(content):
    """Whether a character class's content is one `CLASS` takes, each range running forwards."""
    return CLASS.fullmatch(content) is not None and all(
        low <= high for low, high in re.findall(r"([A-Za-z0-9_])-([A-Za-z0-9_])", content))


def expansions(text):
    """Every string `text`'s brace alternatives expand to, nested ones included, its character
    classes kept as written; None where `text` holds syntax this check does not model: an
    unbalanced brace or bracket, a `,` outside braces, an empty alternative or one holding a `/`,
    a class `class_taken` refuses, or more than `MAX_EXPANSIONS` strings. A brace alternative holds
    no `/`, so every expansion has the `/`-parts of `text` in the same places."""

    def sequence(at, inside):
        found = [""]
        while at < len(text):
            char = text[at]
            if char in ",}":
                return (found, at) if inside else (None, at)
            if char == "]":
                return None, at
            if char == "{":
                alternatives, at = braces(at + 1)
                if alternatives is None:
                    return None, at
                found = [one + two for one in found for two in alternatives]
            elif char == "[":
                end = text.find("]", at + 1)
                if end < 0 or not class_taken(text[at + 1:end]):
                    return None, at
                found = [one + text[at:end + 1] for one in found]
                at = end + 1
            else:
                found = [one + char for one in found]
                at += 1
            if len(found) > MAX_EXPANSIONS:
                return None, at
        return (None if inside else found), at

    def braces(at):
        alternatives = []
        while True:
            found, at = sequence(at, True)
            if found is None or any(one == "" or "/" in one for one in found):
                return None, at
            alternatives.extend(found)
            if len(alternatives) > MAX_EXPANSIONS:
                return None, at
            closed = text[at] == "}"
            at += 1
            if closed:
                return alternatives, at

    return sequence(0, False)[0]


def target_of(raw, tool, scope):
    """One file system target a call names, as point 5 checks it: its literal part, resolved, and
    every pattern part after it over every expansion of its brace alternatives; None where it names
    no place the rule can allow (another scheme, another skill, a home path, an empty path) or holds
    syntax this check does not model (`UNMODELLED`, `SELECTOR`, `expansions`). An exclusion (`!…`)
    names a place too. A line or page selector after a name (`judge-1.md:1-40`) is the same file.
    `skill://approval-review` alone is the skill directory to a `glob` and its `SKILL.md` to a
    `read` or `grep`, as each tool's default fills it; a relative path is from the working
    directory. The literal part ends where omp's glob tool starts the pattern (`GLOB_START`)."""
    if not isinstance(raw, str):
        return None
    text = raw.strip()
    if text.startswith("!"):
        text = text[1:]
    url = "://" in text
    if url:
        scheme, text = text.split("://", 1)
        if f"{scheme}://" != SKILL_SCHEME:
            return None
    text, colon, selector = text.partition(":")
    if not SELECTOR.fullmatch(colon + selector):
        return None
    if url:
        name, _, tail = text.partition("/")
        if name != SKILL_NAME:
            return None
        if tail == "" and tool != "glob":
            tail = "SKILL.md"
        text = scope.skill_dir + ("/" + tail if tail else "")
    if text == "" or text.startswith("~") or UNMODELLED.search(text):
        return None
    found = expansions(text)
    if found is None:
        return None

    def parts_of(one):
        return (one if one.startswith("/") else os.path.join(scope.work, one)).split("/")

    parts = parts_of(text)
    first = next((index for index, part in enumerate(parts) if GLOB_START.search(part)), len(parts))
    pattern = [part for one in found for part in parts_of(one)[first:] if part]
    return resolved("/".join(parts[:first]) or "/", scope.links), pattern


def in_scope(tool, target, scope, version=COUNTING_RULE):
    """Whether one target a call names, an argument as given or as the tool's default fills it, or
    the scope its result says it read, is one version `version` of point 5's rule lets `tool` name:
    under every version, exactly its own bundle file or one of the skill's files, with no pattern;
    from version 4, a `glob` whose scope is exactly one of four, its own bundle file, the bundle
    directory, a skill file or the skill directory, with no `.` or `..` part in any expansion of
    its pattern; from version 5, a `read` of the bundle directory, the skill directory or its
    `references/` itself."""
    if target is None or target[0] is None:
        return False
    literal, pattern = target
    files = {scope.bundle, *scope.skill_files}
    if not pattern and literal in files:
        return True
    if version >= 4 and tool == "glob":
        return not {".", ".."} & set(pattern) and literal in files | {scope.bundle_dir, scope.skill_dir}
    directories = (scope.bundle_dir, scope.skill_dir, os.path.join(scope.skill_dir, "references"))
    return version >= 5 and tool == "read" and not pattern and literal in directories


def in_trees(target, scope, version=COUNTING_RULE):
    """Whether a path a call's result returns lies inside the trees the sandbox manifests, with no
    pattern: up to version 5 the bundle directory's and the skill directory's, from version 6 the
    work tree's and the skill tree's. A glob of the bundle directory returns the other judges' file
    names, and that is all a result may name."""
    if target is None or target[0] is None or target[1]:
        return False
    trees = (scope.work_tree, scope.skill_dir) if version >= 6 else (scope.bundle_dir, scope.skill_dir)
    return any(under(target[0], tree) for tree in trees)


def listed_directory(answer, scope):
    """The directory of the two trees a `read`'s answer lists, or None where the answer does not
    establish one: its details mark a directory and name it (`resolvedPath`); that path, resolved,
    is a directory of the work tree or the skill tree both in the listing taken before the first
    judge and in its tree's manifest recorded then; and every line of its text is a note of the
    listing or an entry that names, with its size and age, an entry that manifest holds below that
    directory (version 6: every entry a listing returns is compared with its tree's manifest)."""
    details = answer.get("details") if isinstance(answer, dict) else None
    if not isinstance(details, dict) or details.get("isDirectory") is not True:
        return None
    path = details.get("resolvedPath")
    if not isinstance(path, str) or not path:
        return None
    path = resolved(path if path.startswith("/") else os.path.join(scope.work, path), scope.links)
    if path is None or scope.tree_entries.get(path) != "d" or scope.manifested.get(path) != "d":
        return None
    names = {os.path.basename(one) for one in scope.manifested if one != path and under(one, path)}
    lines = [line for line in str(answer.get("text") or "").splitlines() if line.strip()]
    if not lines:
        return None
    for line in lines:
        if LISTING_NOTE.fullmatch(line.strip()):
            continue
        entry = LISTING_ENTRY.fullmatch(line)
        if entry is None or entry.group("name") not in names:
            return None
    return path


def text_or_listing_problem(tool, call, targets, returned, scope):
    """Version 6's rule on one `read`, `grep` or `glob` call, or None where it passes. A listing (a
    `glob`, or a `read` whose answer `listed_directory` establishes) passes where its whole scope,
    base and pattern, lies inside the work tree or the skill tree. A text read (a `read` of a file,
    any `grep`) passes where its every target is its own bundle file or a skill file; a `grep` of a
    directory reads every file in it, which passes only for a directory the skill tree's manifest
    holds, whose files are the skill's. A `read` whose type the answer does not establish is a
    deviation. Every path a result returns must be an entry its tree's manifest holds."""
    files = {scope.bundle, *scope.skill_files}
    trees = (scope.work_tree, scope.skill_dir)
    listed = {one for one in (listed_directory(answer, scope) for answer in call.get("answers") or []) if one}
    skill_directories = {path for path, kind in scope.manifested.items() if kind == "d" and under(path, scope.skill_dir)}
    for raw in targets:
        target = target_of(raw, tool, scope)
        if target is None or target[0] is None:
            return repr(raw)
        literal, pattern = target
        if tool == "glob":
            if {".", ".."} & set(pattern) or not any(under(literal, tree) for tree in trees):
                return f"{raw!r}, a listing whose scope leaves the work tree and the skill tree"
        elif pattern:
            return f"{raw!r}, a pattern where a {tool} names one file"
        elif not (literal in files or (tool == "grep" and literal in skill_directories)
                  or (tool == "read" and literal in listed)):
            what = "a listing its answer does not establish" if tool == "read" else "a text read"
            return f"{raw!r}, {what} of neither its own bundle file nor a skill file"
    for raw in returned:
        target = target_of(raw, tool, scope)
        if not in_trees(target, scope, 6):
            return f"{raw!r}, a path its result returns outside the manifested trees"
        if target[0] not in scope.manifested:
            return f"{raw!r}, an entry its result returns that its tree's manifest does not hold"
    return None


def detail_paths(details, work):
    """What a tool's result says it reached, each from the result's own working directory: the
    scope it read (the path it resolved, its search path, its scope, the source a read names),
    held as a target is; and the paths it returns (each file it found or matched), held to the
    trees. A glob over several targets writes its scope as their paths joined by `, `, which are
    split."""
    if not isinstance(details, dict):
        return [], []
    cwd = details["cwd"] if isinstance(details.get("cwd"), str) else work
    scopes = [details.get(key) for key in ("resolvedPath", "searchPath")]
    if isinstance(details.get("scopePath"), str):
        scopes += details["scopePath"].split(", ")
    source = (details.get("meta") or {}).get("source") if isinstance(details.get("meta"), dict) else None
    if isinstance(source, dict) and source.get("type") == "path":
        scopes.append(source.get("value"))
    found = list(details.get("files") or [])
    found += [one.get("path") for one in details.get("fileMatches") or [] if isinstance(one, dict)]

    def placed(paths):
        return [one if one.startswith("/") or "://" in one else os.path.join(cwd, one)
                for one in paths if isinstance(one, str) and one]

    return placed(scopes), placed(found)


def call_problem(tool, call, scope, version=COUNTING_RULE):
    """Why one `read`, `grep` or `glob` call reaches outside version `version`'s scope, or None:
    every target it names as given, the tool's default where it names none, and every scope its
    result says it read (`in_scope`); and every path its result returns (`in_trees`)."""
    targets, returned = [], []
    for arguments in call["arguments"] or [{}]:
        if not isinstance(arguments, dict):
            return "arguments that are no object"
        unplaced = sorted(set(arguments) - NO_TARGET_ARGUMENTS[tool] - {"path"})
        if unplaced:
            return f"the argument {unplaced[0]!r}, which names a target the rule cannot place"
        value = arguments.get("path")
        if value is None and tool == "read":
            return "no path"
        if value is None:
            targets.append(scope.work)
        elif not isinstance(value, str):
            return f"the path {value!r}"
        else:
            targets.extend(value.split(";"))
    for details in call["details"]:
        scopes, found = detail_paths(details, scope.work)
        targets.extend(scopes)
        returned.extend(found)
    if version >= 6:
        return text_or_listing_problem(tool, call, targets, returned, scope)
    for raw in targets:
        if not in_scope(tool, target_of(raw, tool, scope), scope, version):
            return repr(raw)
    for raw in returned:
        if not in_trees(target_of(raw, tool, scope), scope, version):
            return f"{raw!r}, a path its result returns outside the manifested trees"
    return None


def tool_call_problems(label, calls, scope, version=COUNTING_RULE):
    """The tool calls of one judge that version `version` of point 5's rule makes a deviation,
    attempted, failed, denied or answered, each as `{judge, callId, tools, reason, problem}`: its
    `yield` answer is exempt from version 4; a `read`, `grep` or `glob` is held to that version's
    scope (`in_scope`); every other call is one."""
    problems = []
    for call_id, call in sorted(calls.items()):
        names = sorted(call["names"])
        found = {"judge": label, "callId": call_id, "tools": names}
        if names == [ANSWER_TOOL] and version >= 4:
            continue
        if len(names) != 1 or names[0] not in READ_TOOLS:
            problems.append({**found, "reason": "no file system tool the rule allows",
                             "problem": f"{label} called {names or 'a tool with no name'} ({call_id})"})
            continue
        reason = call_problem(names[0], call, scope, version)
        if reason is not None:
            problems.append({**found, "reason": reason,
                             "problem": f"{label} called {names[0]} on {reason}, which version {version} of point "
                                        f"5's rule does not allow ({call_id})"})
    return problems


def model_problems(label, entries, asked, model):
    """Whether the judge was asked point 2's model, marked no fallback, and served by that model
    wherever omp reports what served it; and the model omp reports for its last message."""
    problems = []
    init = [entry for entry in entries if entry.get("type") == "session_init"]
    if len(init) != 1 or init[0].get("resolvedModel") != asked:
        problems.append(f"{label} was asked {[entry.get('resolvedModel') for entry in init]}, not {asked}")
    changes = [entry for entry in entries if entry.get("type") == "model_change"]
    if not changes:
        problems.append(f"{label}'s transcript records no model")
    for change in changes:
        if change.get("resolvedModelIsFallback") is True:
            problems.append(f"{label} is marked a fallback, to {change.get('model')}")
        elif change.get("model") != model:
            problems.append(f"{label} was switched to {change.get('model')}")
    served_names = {model, model.split("/", 1)[-1]}
    served = None
    replies = 0
    for entry in entries:
        message = entry.get("message") if entry.get("type") == "message" else None
        if not isinstance(message, dict) or message.get("role") != "assistant":
            continue
        replies += 1
        served = message.get("upstreamModel")
        if served is not None and served not in served_names:
            problems.append(f"{label} was served by {served}")
    if replies == 0:
        problems.append(f"{label}'s transcript holds no reply of a model")
    return problems, served


def schema_refusal(schema, where="JUDGE_SCHEMA"):
    """Why `answer_problem` cannot hold an answer to `schema` whole, or None: every keyword
    anywhere in it is one it reads, and every type one it knows."""
    if not isinstance(schema, dict):
        return f"{where} is no object"
    unread = sorted(set(schema) - SCHEMA_KEYWORDS)
    if unread:
        return f"{where} uses {unread}, which the answer check does not read"
    kinds = schema.get("type")
    for kind in [kinds] if isinstance(kinds, str) else kinds or []:
        if kind not in SCHEMA_TYPES:
            return f"{where} names the type {kind!r}, which the answer check does not know"
    for name, inner in (schema.get("properties") or {}).items():
        refusal = schema_refusal(inner, f"{where}.{name}")
        if refusal is not None:
            return refusal
    return None


def answer_problem(answer, schema):
    """Why one answer is not an object of `schema`, or None. `schema` uses only the keywords and
    types `schema_refusal` lets through, so nothing of it goes unchecked."""
    def of_type(value, kind):
        return {"object": isinstance(value, dict), "string": isinstance(value, str), "null": value is None,
                "boolean": isinstance(value, bool), "array": isinstance(value, list),
                "integer": isinstance(value, int) and not isinstance(value, bool),
                "number": isinstance(value, (int, float)) and not isinstance(value, bool)}[kind]

    def problem(value, rule, where):
        kinds = rule.get("type")
        if kinds is not None and not any(of_type(value, kind) for kind in ([kinds] if isinstance(kinds, str) else kinds)):
            return f"{where} is no {kinds}"
        if "enum" in rule and value not in rule["enum"]:
            return f"{where} is {value!r}, none of {rule['enum']}"
        if isinstance(value, dict):
            for name in rule.get("required", []):
                if name not in value:
                    return f"{where} has no {name}"
            for name, inner in (rule.get("properties") or {}).items():
                if name in value:
                    found = problem(value[name], inner, f"{where}.{name}")
                    if found:
                        return found
        return None

    if answer is None:
        return "no answer"
    return problem(answer, schema, "the answer")


def rule_verdict(answers, kinds):
    """Point 2's verdict rule ("The verdict") over five valid answers: `never_decide:<kind>` when
    three or more of the five name a kind (the most named; a tie, the tied kinds in alphabetical
    order joined by `+`); else the majority of the five verdicts."""
    named = Counter(answer["never_decide"] for answer in answers if answer["never_decide"] in kinds)
    if sum(named.values()) >= 3:
        top = max(named.values())
        return "never_decide:" + "+".join(sorted(kind for kind, count in named.items() if count == top))
    votes = Counter(answer["verdict"] for answer in answers)
    return max(votes, key=lambda verdict: votes[verdict])


def session_problems(files, record):
    """The session's own transcript: one, holding the one `eval` call of `CELL` and no other tool
    call. Returns its problems by kind, and the directory its judges' transcripts are in."""
    problems = {"session": [], "transcript": []}
    returncode = (record.get("omp") or {}).get("returncode")
    if returncode != 0:
        problems["session"].append(f"omp exited {returncode}")
    tops = sorted(name for name in files if re.fullmatch(r"sessions/[^/]+\.jsonl", name))
    if len(tops) != 1:
        problems["transcript"].append(f"the sandbox kept {len(tops)} session transcripts, not one")
        return problems, None
    calls = tool_calls_of(entries_of(files[tops[0]]))
    names = {name for call in calls.values() for name in call["names"]}
    cells = [one for call in calls.values() for one in call["arguments"] if isinstance(one, dict) and "code" in one]
    one_cell = bool(cells) and all(
        one.get("language") in ("py", "python") and str(one.get("code")).rstrip("\n") == SANDBOX.CELL.rstrip("\n")
        for one in cells)
    if len(calls) != 1 or names != {"eval"} or not one_cell:
        problems["session"].append(f"the session made {len(calls)} tool calls ({sorted(names)}), not the one eval call")
    return problems, tops[0][:-len(".jsonl")]


def judge_transcripts(files, directory, calls):
    """Each recorded call's transcript, by the call's index: the one whose task ends with the
    call's prompt, started as the call's agent. A transcript of no call is a problem of the
    session, which started an agent the calls do not name."""
    found = {}
    problems = []
    for name in sorted(one for one in files if one.startswith(directory + "/") and one.endswith(".jsonl")):
        entries = entries_of(files[name])
        init = next((entry for entry in entries if entry.get("type") == "session_init"), {})
        task = init.get("task") if isinstance(init.get("task"), str) else ""
        matched = [index for index, call in enumerate(calls) if task.endswith(call["prompt"])]
        if len(matched) != 1 or matched[0] in found or init.get("agent") != calls[matched[0]]["agent"]:
            problems.append(f"{name} is the transcript of no one recorded call")
            continue
        found[matched[0]] = (name, entries)
    return found, problems


def answers_by_agent(files):
    """Each judge's answer as `wait()` gave it, by the agent's id; a text is read as JSON where it
    is, as S10's round reads one. `ValueError` where `results.json` is no list of answers."""
    listed = json.loads(files["results.json"]) if "results.json" in files else []
    if not isinstance(listed, list) or not all(isinstance(one, dict) for one in listed):
        raise ValueError("results.json is no list of answers")
    answers = {}
    for one in listed:
        answer = one.get("result")
        if isinstance(answer, str):
            try:
                answer = json.loads(answer)
            except ValueError:
                pass
        answers[str(one.get("id"))] = answer
    return answers
