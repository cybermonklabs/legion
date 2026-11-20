"""Point 5's checks of the claim's judging, decided from the transcripts the sandbox kept, in code,
before any answer is compared (version 4 of "The claim's bundle and its arguments").

- The session's own transcript holds the one `eval` call of the sandbox's `CELL` and nothing else
  (`session_problems`); each recorded `agent()` call has one judge transcript (`judge_transcripts`).
- No judge made a tool call, attempted, failed, denied or answered, but its `yield` answer, which is
  exempt from the path check; a `read` or `grep` of its own bundle file or of a
  `skill://approval-review` file; or a `glob` whose scope stays inside its bundle directory or the
  skill directory (`tool_call_problems`). Each target is checked as given, as the tool's default
  fills it, and normalized as it resolves on the sandbox's file system (`target_of`, `resolved`),
  with every path the tool's result says it reached (`detail_paths`).
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

# Point 5, version 4. The tool omp's subagents return their schema's answer by, exempt from the
# path check; the tools a judge may call on a file system target; and, by tool, the arguments that
# name no target (search text, a count, a switch, the call's stated intent), as omp 18.16.0's
# schemas name them. Any other argument is taken for a target the rule cannot place, which makes
# the case a deviation.
ANSWER_TOOL = "yield"
READ_TOOLS = ("read", "grep", "glob")
NO_TARGET_ARGUMENTS = {
    "read": {"i"},
    "grep": {"pattern", "case", "gitignore", "skip", "i"},
    "glob": {"limit", "hidden", "gitignore", "i"},
}
SKILL_SCHEME = "skill://"
SKILL_NAME = "approval-review"
# A part of a path that is a pattern, which a `glob` matches and no `read` or `grep` names.
GLOB_CHARACTERS = re.compile(r"[*?\[\]{}]")

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
    start and the result, wherever an entry names a tool call."""
    calls = {}

    def note(call_id, name, arguments=None, details=None):
        one = calls.setdefault(str(call_id), {"names": set(), "arguments": [], "details": []})
        if name is not None:
            one["names"].add(name)
        if arguments is not None:
            one["arguments"].append(arguments)
        if details is not None:
            one["details"].append(details)

    def walk(value):
        if isinstance(value, dict):
            if value.get("type") == "toolCall":
                note(value.get("id"), value.get("name"), arguments=value.get("arguments", value.get("input")))
            if "toolCallId" in value:
                note(value.get("toolCallId"), value.get("toolName"), arguments=value.get("args"),
                     details=value.get("details"))
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
    """What point 5 (version 4) lets one judge's `read`, `grep` and `glob` reach, every path as it
    resolves on the sandbox's file system: the judge's own bundle file and the directory that holds
    it, the skill directory that `skill://approval-review` resolves to and its three files, the
    working directory a path is relative to, and the links the sandbox's listing records."""

    work: str
    bundle: str
    bundle_dir: str
    skill_dir: str
    skill_files: frozenset
    links: dict


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


def scope_of(bundle, work, listing):
    """One judge's scope, from its bundle's path and the sandbox's listing."""
    links = links_of(listing)
    skill_dir = resolved(SANDBOX.SKILL_DIR, links)
    own = resolved(bundle, links)
    return Scope(work=work, bundle=own, bundle_dir=os.path.dirname(own), skill_dir=skill_dir,
                 skill_files=frozenset(os.path.join(skill_dir, name) for name in SANDBOX.SKILL_FILES),
                 links=links)


def target_of(raw, tool, scope):
    """One file system target a call names, as point 5 checks it: its literal part, resolved, and
    the pattern parts after it; None where it names no place the rule can allow (another scheme,
    another skill, a home path, an empty path). An exclusion (`!…`) names a place too. A line or
    page selector after a name (`judge-1.md:1-40`) is the same file. `skill://approval-review`
    alone is the skill directory to a `glob` and its `SKILL.md` to a `read` or `grep`, as each
    tool's default fills it; a relative path is from the working directory."""
    if not isinstance(raw, str):
        return None
    text = raw.strip()
    if text.startswith("!"):
        text = text[1:]
    if "://" in text:
        scheme, rest = text.split("://", 1)
        name, _, tail = rest.split(":", 1)[0].partition("/")
        if f"{scheme}://" != SKILL_SCHEME or name != SKILL_NAME:
            return None
        if tail == "" and tool != "glob":
            tail = "SKILL.md"
        text = scope.skill_dir + ("/" + tail if tail else "")
    else:
        text = text.split(":", 1)[0]
    if text == "" or text.startswith("~"):
        return None
    path = text if text.startswith("/") else os.path.join(scope.work, text)
    parts = path.split("/")
    first = next((index for index, part in enumerate(parts) if GLOB_CHARACTERS.search(part)), len(parts))
    return resolved("/".join(parts[:first]) or "/", scope.links), [part for part in parts[first:] if part]


def in_scope(tool, target, scope):
    """Whether one target stays where point 5 lets `tool` reach: for a `glob`, its literal part
    inside the bundle directory or the skill directory and no parent step in its pattern; for a
    `read` or `grep`, no pattern and exactly its own bundle file or one of the skill's files."""
    if target is None or target[0] is None:
        return False
    literal, pattern = target
    if tool == "glob":
        return ".." not in pattern and (under(literal, scope.bundle_dir) or under(literal, scope.skill_dir))
    return not pattern and (literal == scope.bundle or literal in scope.skill_files)


def detail_paths(details, work):
    """The paths a tool's result says it reached: the path it resolved, the scope and the search
    path it read, and each file it found or matched, each from the result's own working directory."""
    if not isinstance(details, dict):
        return []
    cwd = details["cwd"] if isinstance(details.get("cwd"), str) else work
    found = [details.get(key) for key in ("resolvedPath", "searchPath", "scopePath")]
    found += list(details.get("files") or [])
    found += [one.get("path") for one in details.get("fileMatches") or [] if isinstance(one, dict)]
    source = (details.get("meta") or {}).get("source") if isinstance(details.get("meta"), dict) else None
    if isinstance(source, dict) and source.get("type") == "path":
        found.append(source.get("value"))
    return [one if one.startswith("/") else os.path.join(cwd, one) for one in found if isinstance(one, str) and one]


def call_problem(tool, call, scope):
    """Why one `read`, `grep` or `glob` call reaches outside point 5's scope, or None: every target
    it names as given, the tool's default where it names none, and every path its result reports."""
    targets = []
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
        targets.extend(detail_paths(details, scope.work))
    for raw in targets:
        if not in_scope(tool, target_of(raw, tool, scope), scope):
            return repr(raw)
    return None


def tool_call_problems(label, calls, scope):
    """Point 5's tool calls of one judge (version 4): its `yield` answer is exempt from the path
    check; a `read` or `grep` may reach only its own bundle file or a skill file, a `glob` only
    inside its bundle directory or the skill directory; every other call is a deviation, attempted,
    failed, denied or answered."""
    problems = []
    for call_id, call in sorted(calls.items()):
        names = call["names"]
        if names == {ANSWER_TOOL}:
            continue
        if len(names) != 1 or not names <= set(READ_TOOLS):
            problems.append(f"{label} called {sorted(names) or 'a tool with no name'} ({call_id})")
            continue
        tool = next(iter(names))
        reason = call_problem(tool, call, scope)
        if reason is not None:
            allowed = ("its bundle directory or the skill directory" if tool == "glob"
                       else f"its own bundle file or a {SKILL_SCHEME}{SKILL_NAME} file")
            problems.append(f"{label} called {tool} on {reason}, which reaches outside {allowed} ({call_id})")
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
