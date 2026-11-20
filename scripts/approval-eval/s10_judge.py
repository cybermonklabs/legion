"""The claim run's fourth stage: S10, frozen, judges each request of the inventory (point 14 (b)).

After the build (`build-corpus.ts`), the draw (`draw.ts`) and the inputs (`s10-inputs.ts`), and
before the analysis (`analyze.ts --claim-s10`), under the registered read and the claim run:

    python3 scripts/approval-eval/s10_judge.py --inputs "$CLAIM/s10-inputs" \\
      --out "$CLAIM/s10-judged" --code <code-txt file> --skill <SKILL.md> \\
      --never-decide <references/never-decide.md> --image <image>@sha256:<digest>

The admission. Before anything else is read, `s10-judge-admit.ts` decides whether this run may
read the reserve, by the one implementation every stage of the claim uses (`reserveAccessOf` in
`reserve.ts`, with the run digest of this command and its arguments, `runDigestOf`), and checks
that `--inputs` is the inputs stage this claim run wrote under this read: its record written, its
claim run the one `APPROVAL_EVAL_CLAIM_RUN` names, and every file it names there with the bytes it
names, each read back by the shapes `s10-inputs-io.ts` holds. This driver reads only those files,
and each only with the sha256 the admission read.

The frozen code. `--code` is the code-txt file of point 2 as `dispatch doc-read` prints it: its
two header lines, then the text whose sha256 point 2 fixes, split into its five files by their
`=== FILE: <name> ===` lines, each checked against the sha256 point 2 gives it. Its `shadow.py` is
loaded as written, from the copy this stage keeps, with `DISPATCH_URL` and `DISPATCH_TOKEN_FILE`
set to values that reach nothing; then the reads of point 3 are replaced, and nothing else: its
`api`, through which its `issue`, `search`, `doc_version` and the ask reads of its `neighbours`
go, by a reader over the request's inputs (`RequestReader`), and its `reserved_neighbour` by the
reserve's decision the inputs stage recorded (point 4 (d)). Every other read of the network or of
a command (`subprocess`, `urllib`) raises. `JUDGE_PROMPT` and `build_bundle` are checked against
point 2's `prompt_sha256` and `bundle_code_sha256`, and the skill files against its
`skill_sha256`; they are put where its `skill_bytes` reads them, as the code-txt's own header says.

Each request, once, in the inventory's order (point 6):

- A request the inputs stage could not give as of its judged moment is a deviation (`inputs`).
- S10's `legion_fact` is run over the request file's ask and issue, and a fact that is not the
  one the file records is a deviation (`fact`).
- S10's own `start_judgement` runs: `prepare` and the fact check, each document at its version,
  `neighbours` over the archived search, and `run_judges`, which builds the five bundles under
  `WORK` with its own `time.time()` seed and makes its five `agent()` calls, which are recorded
  here and not made. A read the inputs cannot give is a deviation (`inputs`). A fact that holds
  ends there: `finish_judgement` gives `never_decide:waiting-legion-root` with no judge.
- Else the five bundles and the five calls are kept under `--out`, and only then is the request
  sent to the sandbox (`s10_sandbox.py`, one Kubernetes Job), whose one `omp -p` makes those five
  calls. Its result is read from the Job's log, the Job is deleted, and every file of the result
  is kept. Then, in code and before any answer is compared (point 5): the session's own transcript
  holds the one `eval` call and nothing else (`session`); there are five judges' transcripts
  (`transcript`); no judge made a tool call but `read`, `grep` or `glob` of its own bundle file or
  of a `skill://approval-review` file (`tool-call`); each judge was asked point 2's model, none is
  marked a fallback, and each model omp reports serving it is that model (`model`). Then five
  valid answers (`answers`); the verdict is S10's `aggregate` over them, through its
  `finish_judgement`, and it must be the verdict point 2's rule gives (`verdict`).

A request is started once its bundles and calls are kept, and a request started and not finished
is a deviation (`started`) that no later run judges again. A sandbox that refuses to start a judge
stops the run, since every later request would meet the same sandbox; any stop, an interrupt
included, writes every request not yet judged as a deviation (`unjudged`), so the stage always
reports every request of the inventory, each once.

The stage record, `s10-judged.json`, chains this stage to the inputs: the claim run, the read,
this run's digest, the sha256 of `s10-inputs.json` and of each file read there, the frozen code,
skill and image; and names the sha256 of every file written under `--out`. A file is committed by
writing it beside its place, writing the record that names it, then moving it into its place. A
run into an `--out` that holds another run's record, or files of none, is refused; a run into its
own resumes it, taking only the files its record names, with the bytes it names.
"""

import sys

# Nothing this imports may leave a `__pycache__` in scripts/approval-eval: it would change the run
# digest, which covers every file of that directory.
sys.dont_write_bytecode = True

import argparse  # noqa: E402
import base64  # noqa: E402
import copy  # noqa: E402
import datetime  # noqa: E402
import hashlib  # noqa: E402
import importlib.util  # noqa: E402
import inspect  # noqa: E402
import json  # noqa: E402
import lzma  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
import shutil  # noqa: E402
import signal  # noqa: E402
import socket  # noqa: E402
import subprocess  # noqa: E402
import time  # noqa: E402
from collections import Counter  # noqa: E402
from dataclasses import dataclass, field  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import s10_sandbox as SANDBOX  # noqa: E402

USAGE = """usage: python3 scripts/approval-eval/s10_judge.py --inputs <inputs dir> --out <dir>
    --code <code-txt file> --skill <SKILL.md> --never-decide <never-decide.md>
    --image <image>@sha256:<digest>

Judges each request of the claim's inventory with S10 frozen: the frozen code builds and keeps
each request's five evidence bundles here, and five judges answer in a sandbox Job that holds the
bundles and the runtime alone. Runs only under the claim run's registered read, over the inputs
stage that claim run wrote.

Options:
  --inputs <dir>        Required. The claim run's inputs stage (s10-inputs.ts --out).
  --out <dir>           Required. Where the judged stage is written: an absolute path outside this
                        repository's workspace. A directory this command wrote in the same claim
                        run, over the same inputs, is resumed.
  --code <file>         Required. The frozen S10 code-txt file, as `dispatch doc-read` prints it.
  --skill <file>        Required. The skill's SKILL.md at the commit S10 froze.
  --never-decide <file> Required. The skill's references/never-decide.md at that commit.
  --image <ref>         Required. The sandbox's image, by digest: <name>@sha256:<64 hex digits>.

Environment:
  APPROVAL_EVAL_RESERVE=read:<reference>, APPROVAL_EVAL_CLAIM_RUN=<start reply id>
                        Required: the read claim.ts registers for this run's digest, and the
                        claim run the inputs stage recorded.

Exit status: 0 when every request has its outcome, 2 when the command could not run or stopped
(a stopped run still writes every request it did not judge as a deviation)."""

STAGE_FILE = "s10-judged.json"
OUTCOMES_FILE = "outcomes.jsonl"
STAGE_VERSION = 1
CODE_DIRECTORY = "code"
REQUESTS_DIRECTORY = "requests"
JUDGES = 5

# The deviation kinds, in the order a request's problems are weighed: its first is its kind.
DEVIATION_KINDS = ("inputs", "fact", "sandbox", "session", "transcript", "tool-call", "model",
                   "answers", "verdict", "started", "unjudged")

# The tool omp's subagents return their schema's answer by: the answer itself, not a read.
ANSWER_TOOL = "yield"
# The tools a judge may call, and only on its own bundle or a skill file (point 5).
READ_TOOLS = ("read", "grep", "glob")
SKILL_SCHEME = "skill://"
SKILL_NAME = "approval-review"


@dataclass(frozen=True)
class Pins:
    """What point 2 fixes of the frozen code: a test of this driver hands in its own."""

    code_sha256: str
    files: dict
    prompt_sha256: str
    bundle_code_sha256: str
    skill_sha256: dict
    asked: str
    model: str
    work: str


# Point 2: the code-txt's text after its two header lines and the start of each file's sha256;
# `JUDGE_PROMPT`'s and `build_bundle`'s; the skill's files at the commit S10 froze; the model asked
# and the model that must serve it; and the directory the live prompts name the bundles under.
FROZEN = Pins(
    code_sha256="8c1f0ea604030ba779285862e184d5e7d4178ead7c89be989430d8b1c38e4e2d",
    files={"shadow.py": "8da30263", "render.py": "c00a796b", "round.py": "e8a7a343",
           "kernel_round.py": "a6107153", "tick.sh": "6238bc5f"},
    prompt_sha256="84f0f498e7e7d2622b3cdb553296356670331b5b8ee3550eafea2c9ac3877659",
    bundle_code_sha256="ef222a12033d1a3bd3a074a22d6896e0239c867c8acc0de8d977f32e29d5c004",
    skill_sha256=dict([
        ("SKILL.md", "67e522cb41bf13952446f313cd4b34b08b936d6a9938526bb0499e6c65755de9"),
        ("references/never-decide.md", "698e5af8120cc626cd66207cb74f5943146214ffab088eb9d65ec2d0b7f1d40a"),
    ]),
    asked="anthropic/claude-opus-5-5:xhigh",
    model="anthropic/claude-opus-5-5",
    work="/work/shadow",
)

# What `shadow.py` reads from the environment when it is loaded, given values that reach nothing.
UNREACHABLE = {"DISPATCH_URL": "s10-judge-no-dispatch:", "DISPATCH_TOKEN_FILE": "/nonexistent/s10-judge/token"}


class CannotRun(Exception):
    """The command cannot run as the registered text fixes it, so it stops before any judgement."""


class Stopped(Exception):
    """The run stops: every request it has not judged is written as a deviation."""


class InputsGap(Exception):
    """S10 read something the inputs do not give as of the judged moment (point 3)."""


def sha256_of(data):
    """The sha256 of some bytes, or of a text's UTF-8 bytes."""
    if isinstance(data, str):
        data = data.encode("utf-8")
    return hashlib.sha256(data).hexdigest()


def utc_now():
    """The time now, as the records here write a time."""
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")


def json_bytes(value):
    """A record as a file holds it."""
    return (json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")


def under(path, tree):
    """Whether `path` is `tree` or inside it."""
    return path == tree or path.startswith(tree.rstrip("/") + "/")


# ---------- flags and the admission ----------

class _Parser(argparse.ArgumentParser):
    def error(self, message):
        raise CannotRun(f"{message}\n\n{USAGE}")


def flags_of(argv):
    """The flags, each required, as `s10-judge-admit.ts` reads the same arguments."""
    parser = _Parser(add_help=False, usage=USAGE)
    for name in ("inputs", "out", "code", "skill", "never-decide", "image"):
        parser.add_argument(f"--{name}", required=True)
    flags = parser.parse_args(argv)
    if not re.fullmatch(r"[^@\s]+@sha256:[0-9a-f]{64}", flags.image.strip()):
        raise CannotRun(f"--image takes an image by its digest, <name>@sha256:<64 hex digits>, not {flags.image}")
    return flags


def admit_with_bun(argv, environ):
    """The admission, as `s10-judge-admit.ts` decides it for these arguments, or `CannotRun`."""
    entry = os.path.join(HERE, "s10-judge-admit.ts")
    completed = subprocess.run(["bun", entry, *argv], env=dict(environ), capture_output=True, text=True)
    if completed.returncode != 0:
        raise CannotRun(completed.stderr.strip() or f"{entry} exited {completed.returncode}")
    return json.loads(completed.stdout.strip().splitlines()[-1])


# ---------- the frozen code ----------

def frozen_files(path, pins):
    """The code-txt's five files, by name, each checked against point 2."""
    with open(path, "rb") as handle:
        raw = handle.read()
    parts = raw.split(b"\n", 2)
    if len(parts) < 3 or not parts[0].startswith(b"File "):
        raise CannotRun(f"{path} is not a code-txt file as `dispatch doc-read` prints one")
    text = parts[2]
    if sha256_of(text) != pins.code_sha256:
        raise CannotRun(f"{path} holds text whose sha256 is {sha256_of(text)}, and point 2 fixes {pins.code_sha256}")
    pieces = re.split(rb"^=== FILE: (\S+) ===\n", text, flags=re.M)
    names = [name.decode() for name in pieces[1::2]]
    if names != list(pins.files):
        raise CannotRun(f"{path} holds the files {names}, and point 2 names {list(pins.files)}")
    files = dict(zip(names, pieces[2::2]))
    for name, prefix in pins.files.items():
        if not sha256_of(files[name]).startswith(prefix):
            raise CannotRun(f"{name} in {path} has sha256 {sha256_of(files[name])}, and point 2 gives {prefix}…")
    return files


class _Refused:
    """A module S10 imported whose every use raises: a read the claim does not replace."""

    def __init__(self, name):
        self._name = name

    def __getattr__(self, attribute):
        raise InputsGap(f"S10 reached {self._name}.{attribute}, which no input of the claim gives")


def load_shadow(path, pins):
    """`shadow.py` loaded as written, with its reads of the network and of commands refused."""
    saved = {name: os.environ.get(name) for name in UNREACHABLE}
    os.environ.update(UNREACHABLE)
    try:
        spec = importlib.util.spec_from_file_location("s10_frozen_shadow", path)
        shadow = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(shadow)
    finally:
        for name, value in saved.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value
    shadow.subprocess = _Refused("subprocess")
    shadow.urllib = _Refused("urllib")
    measured = {
        "JUDGE_PROMPT": (sha256_of(shadow.JUDGE_PROMPT), pins.prompt_sha256),
        "build_bundle": (sha256_of(inspect.getsource(shadow.build_bundle)), pins.bundle_code_sha256),
        "SKILL_SHA": (shadow.SKILL_SHA, pins.skill_sha256),
        "ASKED": (shadow.ASKED, pins.asked),
        "MODEL": (shadow.MODEL, pins.model),
        "WORK": (shadow.WORK, pins.work),
    }
    for name, (found, wanted) in measured.items():
        if found != wanted:
            raise CannotRun(f"the frozen code's {name} is {found!r}, and point 2 fixes {wanted!r}")
    return shadow


def skill_files(skill_path, never_decide_path, pins):
    """The skill's two files, by their path in the skill, each checked against point 2."""
    files = {}
    for name, path in (("SKILL.md", skill_path), ("references/never-decide.md", never_decide_path)):
        with open(path, "rb") as handle:
            files[name] = handle.read()
        if sha256_of(files[name]) != pins.skill_sha256[name]:
            raise CannotRun(f"{path} has sha256 {sha256_of(files[name])}, and point 2 fixes "
                            f"{pins.skill_sha256[name]} for {name}")
    return files


def place_skill(shadow, files):
    """The skill's files where `skill_bytes` reads them, which it checks again."""
    for name, data in files.items():
        target = os.path.join(shadow.SKILL_DIR, name)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        with open(target, "wb") as handle:
            handle.write(data)
    shadow.skill_bytes()


# ---------- the inputs ----------

class Inputs:
    """The inputs stage's files, each read only with the sha256 the admission read it with."""

    def __init__(self, admission):
        self.directory = admission["inputs"]["directory"]
        self.files = admission["inputs"]["files"]
        self._pages = {}
        self.inventory = [json.loads(line) for line in self.text("inventory.jsonl").splitlines() if line]

    def text(self, name):
        if name not in self.files:
            raise CannotRun(f"the admission read no {name} of {self.directory}")
        with open(os.path.join(self.directory, name), "rb") as handle:
            data = handle.read()
        if sha256_of(data) != self.files[name]:
            raise CannotRun(f"{name} of {self.directory} changed after the admission read it")
        return data.decode("utf-8")

    def request(self, ask_id):
        return json.loads(self.text(f"requests/{ask_id}.json"))

    def page(self, sha256):
        if sha256 not in self._pages:
            self._pages[sha256] = json.loads(self.text(f"pages/{sha256}.json"))
        return self._pages[sha256]


class RequestReader:
    """Point 3's reads of one request, from its inputs: S10's `api` and `reserved_neighbour`.

    A read the inputs cannot give is recorded as a gap. A gap in the search is answered with no
    hits, since S10's `neighbours` passes over a search that raises; any other raises."""

    def __init__(self, request, inputs):
        self.request = request
        self.inputs = inputs
        self.gaps = []

    def gap(self, what):
        self.gaps.append(what)
        return InputsGap(what)

    def api(self, path, params=None, tries=4):
        request = self.request
        match = re.fullmatch(r"/api/v1/issues/([^/?]+)", path)
        if match:
            if match.group(1) != request["issue"]["key"]:
                raise self.gap(f"S10 read the issue {match.group(1)}, and the request's is {request['issue']['key']}")
            return copy.deepcopy(request["issue"])
        match = re.fullmatch(r"/api/v1/artifacts/([^/?]+)/versions/(\d+)", path)
        if match:
            for document in request["documents"]:
                if document["artifact_id"] == match.group(1) and str(document["version"]) == match.group(2):
                    return {"markdown": document["markdown"], "sha256": document["sha256"]}
            raise self.gap(f"S10 read version {match.group(2)} of {match.group(1)}, which the request names not")
        match = re.fullmatch(r"/api/v1/asks/([^/?]+)", path)
        if match:
            candidate = (request.get("candidates") or {}).get(match.group(1))
            if candidate is None or "ask" not in candidate:
                raise self.gap(f"S10 read the ask {match.group(1)}, which the inputs keep no fields of")
            return {"ask": copy.deepcopy(candidate["ask"])}
        if path == "/api/v1/search/results":
            return self.search(dict(params or {}))
        raise self.gap(f"S10 read {path}, which no input of the claim gives")

    def search(self, params):
        """One search page as S10 asked it, from the page the inputs stage archived."""
        search = self.request.get("search") or {}
        query = next((one for one in (search.get("title"), search.get("question"))
                      if one is not None and one["query"] == params.get("q")), None)
        page = None if query is None else next(
            (one for one in query["pages"] if one["offset"] == params.get("offset")), None)
        if page is None or params.get("limit") != 50:
            self.gap(f"S10 searched at offset {params.get('offset')} for a query the inputs archived no page of")
            return {"results": []}
        candidates = self.request.get("candidates") or {}
        results = []
        for hit in self.inputs.page(page["sha256"])["hits"]:
            # A rank the server gave none of is 0, as the inputs stage ordered the hits (`neighbourOrder`).
            one = {"id": hit["id"], "kind": hit["kind"], "rank": 0 if hit["rank"] is None else hit["rank"]}
            if hit["kind"] == "ask":
                candidate = candidates.get(hit["id"])
                if candidate is None:
                    self.gap(f"the inputs record no outcome of the ask hit {hit['id']}")
                elif "ask" in candidate:
                    # Kept as of the judged moment. A held or a dropped one carries no fields, so
                    # S10 passes over it as it passes over a hit that is no answered decision.
                    one["ask"] = copy.deepcopy(candidate["ask"])
                    if candidate.get("issue") is not None:
                        one["issue"] = copy.deepcopy(candidate["issue"])
            results.append(one)
        return {"results": results}

    def reserved_neighbour(self, hit_ask_id, issue_key, answered_at, errors):
        """Point 4 (d): the reserve's decision, made by the inputs stage outside the read."""
        candidate = (self.request.get("candidates") or {}).get(hit_ask_id)
        if candidate is not None and "ask" in candidate:
            return False
        self.gap(f"S10 asked the reserve about {hit_ask_id}, which the inputs did not keep")
        return True


class Recorder:
    """S10's `agent_fn`: records each call `run_judges` makes, and starts no agent."""

    def __init__(self):
        self.calls = []

    def __call__(self, prompt, **options):
        if sorted(options) != ["agent", "label", "model", "schema"]:
            raise InputsGap(f"run_judges called agent() with {sorted(options)}")
        self.calls.append({"prompt": prompt, **options})
        return len(self.calls) - 1


# ---------- the stage on disk ----------

class Stage:
    """The stage record and every file it names, under `--out`."""

    def __init__(self, directory, record):
        self.directory = directory
        self.record = record

    @classmethod
    def open(cls, directory, chain):
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, STAGE_FILE)
        if not os.path.exists(path):
            if os.listdir(directory):
                raise CannotRun(f"{directory} holds files and no {STAGE_FILE}, so no run here can show they are its own")
            stage = cls(directory, {"stage": "s10-judged", "version": STAGE_VERSION, "state": "started", **chain,
                                    "startedAt": utc_now(), "finishedAt": None, "stopped": None, "counts": None,
                                    "files": {}})
            stage.save()
            return stage
        with open(path, "rb") as handle:
            record = json.loads(handle.read())
        if record.get("stage") != "s10-judged" or record.get("version") != STAGE_VERSION:
            raise CannotRun(f"{path} is no record of version {STAGE_VERSION} of the judged stage")
        for name in chain:
            if name != "directories" and record.get(name) != chain[name]:
                raise CannotRun(f"{directory} holds another run's judging: its {name} is not this run's. "
                                "A resume takes only its own claim run's directory, over the same inputs and code")
        stage = cls(directory, record)
        stage.hold_to_record()
        return stage

    def hold_to_record(self):
        """Every file there must be one the record names, with its bytes, and every one there."""
        files = self.record["files"]
        for root, _, names in os.walk(self.directory):
            for name in names:
                path = os.path.join(root, name)
                relative = os.path.relpath(path, self.directory)
                if relative == STAGE_FILE or relative.startswith(STAGE_FILE + "."):
                    if relative != STAGE_FILE:
                        os.remove(path)
                    continue
                if relative.endswith(".partial"):
                    whole = relative[:-len(".partial")]
                    with open(path, "rb") as handle:
                        data = handle.read()
                    if files.get(whole) == sha256_of(data):
                        os.replace(path, os.path.join(self.directory, whole))
                    else:
                        os.remove(path)
        for root, _, names in os.walk(self.directory):
            for name in names:
                relative = os.path.relpath(os.path.join(root, name), self.directory)
                if relative != STAGE_FILE and relative not in files:
                    raise CannotRun(f"{os.path.join(self.directory, relative)} is a file the stage record does not name, "
                                    "so no run here can show it is its own")
        for relative, wanted in files.items():
            path = os.path.join(self.directory, relative)
            if not os.path.exists(path):
                raise CannotRun(f"the stage record names {path}, which is not there")
            with open(path, "rb") as handle:
                if sha256_of(handle.read()) != wanted:
                    raise CannotRun(f"{path} is not the file the stage record names")

    def save(self):
        path = os.path.join(self.directory, STAGE_FILE)
        partial = f"{path}.{os.getpid()}.partial"
        with open(partial, "wb") as handle:
            handle.write(json_bytes(self.record))
        os.replace(partial, path)

    def has(self, relative):
        return relative in self.record["files"]

    def read(self, relative):
        with open(os.path.join(self.directory, relative), "rb") as handle:
            return handle.read()

    def commit(self, files):
        """Writes files beside their places, the record naming them, then each into its place."""
        for relative in files:
            if os.path.isabs(relative) or ".." in relative.split("/") or self.has(relative):
                raise CannotRun(f"{relative} is no new file of this stage")
        for relative, data in files.items():
            path = os.path.join(self.directory, relative)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path + ".partial", "wb") as handle:
                handle.write(data)
            self.record["files"][relative] = sha256_of(data)
        self.save()
        for relative in files:
            path = os.path.join(self.directory, relative)
            os.replace(path + ".partial", path)


def request_path(ask_id, name):
    return f"{REQUESTS_DIRECTORY}/{ask_id}/{name}"


# ---------- what a judge did, from its transcript ----------

def entries_of(data):
    """A transcript's entries, one JSON object a line."""
    return [json.loads(line) for line in data.decode("utf-8").splitlines() if line.strip()]


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


def skill_files_of(listing, harness):
    """The files of the harness's `approval-review` skill, by their path in it, from the listing."""
    files = set()
    for line in listing:
        if line.startswith("f "):
            path = SANDBOX.listed_path(line)
            if under(path, harness) and path != harness:
                files.add(os.path.relpath(path, harness))
    return files


def target_allowed(raw, bundle, work, harness, skill_files):
    """Whether one path a judge named is its own bundle file or a `skill://approval-review` file,
    by its skill reference, its default reference (the skill itself is its SKILL.md), or its
    resolved path; a relative path is from `work`, where the judges run, and a line or page
    selector after the name is the same file."""
    if not isinstance(raw, str):
        return False
    path = raw.strip()
    if path.startswith(SKILL_SCHEME):
        rest = path[len(SKILL_SCHEME):].split(":", 1)[0].rstrip("/")
        if rest == SKILL_NAME:
            return "SKILL.md" in skill_files
        return rest.startswith(SKILL_NAME + "/") and rest[len(SKILL_NAME) + 1:] in skill_files
    base = re.sub(r":[^/]*$", "", path)
    full = os.path.normpath(os.path.join(work, base))
    if full == bundle:
        return True
    return under(full, harness) and os.path.relpath(full, harness) in skill_files


def tool_call_problems(label, calls, bundle, work, harness, skill_files):
    """Point 5's tool calls of one judge: each that is not its answer, or a `read`, `grep` or `glob`
    of its own bundle file or of a skill file, every path it names and resolves to included."""
    problems = []
    for call_id, call in sorted(calls.items()):
        names = call["names"]
        if names == {ANSWER_TOOL}:
            continue
        if len(names) != 1 or not names <= set(READ_TOOLS):
            problems.append(f"{label} called {sorted(names) or 'a tool with no name'} ({call_id})")
            continue
        targets = []
        for arguments in call["arguments"]:
            value = arguments.get("path") if isinstance(arguments, dict) else None
            if value is None:
                targets.append(None)
            else:
                targets.extend(value.split(";") if isinstance(value, str) else [value])
        if not call["arguments"]:
            targets.append(None)
        for details in call["details"]:
            if isinstance(details, dict) and details.get("resolvedPath") is not None:
                targets.append(details["resolvedPath"])
        refused = [target for target in targets if not target_allowed(target, bundle, work, harness, skill_files)]
        if refused:
            problems.append(f"{label} called {next(iter(names))} on {refused[0]!r}, which is neither its "
                            f"bundle nor a {SKILL_SCHEME}{SKILL_NAME} file ({call_id})")
    return problems


def model_problems(label, entries, pins):
    """Whether the judge was asked point 2's model, marked no fallback, and served by that model
    wherever omp reports what served it; and the model omp reports for its last message."""
    problems = []
    init = [entry for entry in entries if entry.get("type") == "session_init"]
    if len(init) != 1 or init[0].get("resolvedModel") != pins.asked:
        problems.append(f"{label} was asked {[entry.get('resolvedModel') for entry in init]}, not {pins.asked}")
    changes = [entry for entry in entries if entry.get("type") == "model_change"]
    if not changes:
        problems.append(f"{label}'s transcript records no model")
    for change in changes:
        if change.get("resolvedModelIsFallback") is True:
            problems.append(f"{label} is marked a fallback, to {change.get('model')}")
        elif change.get("model") != pins.model:
            problems.append(f"{label} was switched to {change.get('model')}")
    served_names = {pins.model, pins.model.split("/", 1)[-1]}
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


def answer_problem(answer, schema):
    """Why one answer is not an object of `schema`, or None."""
    def of_type(value, kind):
        return {"object": isinstance(value, dict), "string": isinstance(value, str), "null": value is None,
                "boolean": isinstance(value, bool), "array": isinstance(value, list),
                "integer": isinstance(value, int) and not isinstance(value, bool),
                "number": isinstance(value, (int, float)) and not isinstance(value, bool)}.get(kind, False)

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


# ---------- the sandbox ----------

@dataclass
class SandboxRun:
    """What one Job gave back: its name, the files of its result, and the facts of its pod."""

    job: str
    files: dict = field(default_factory=dict)
    facts: dict = field(default_factory=dict)
    failure: str = None


def payload_variables(files):
    """The bundles and the calls, as the environment variables the Job carries them in."""
    text = SANDBOX.pack({path.lstrip("/"): data for path, data in files.items()})
    width = 100_000
    variables = {f"{SANDBOX.PAYLOAD_VARIABLE}{index}": text[at:at + width]
                 for index, at in enumerate(range(0, len(text), width))}
    variables[SANDBOX.PAYLOAD_SHA256_VARIABLE] = SANDBOX.sha256_of(text)
    return variables


def program_variable():
    """The sandbox's program, as the variable the Job runs it from."""
    with open(os.path.join(HERE, "s10_sandbox.py"), "rb") as handle:
        source = handle.read()
    return base64.b64encode(lzma.compress(source)).decode("ascii")


BOOT = ("import base64, lzma, os; exec(compile(lzma.decompress(base64.b64decode(os.environ['S10_SANDBOX'])), "
        "'s10_sandbox.py', 'exec'))")

# The variables the profile needs, copied from this pod's own environment.
PROFILE_VARIABLES = ("OMP_PROFILE", "PI_PROFILE", "PI_CODING_AGENT_DIR", "PI_CONFIG_FILES", "HOME")
# The largest pod spec the Job may carry, in bytes of its variables' values.
PAYLOAD_LIMIT = 900_000


def job_spec(name, pod, image, variables, environ):
    """The sandbox Job of one request, from this pod's own spec: its namespace, service account,
    security contexts, resources, tolerations and the three mounts of the runtime (the gateway
    key's one item, `models.yml` and the overlay), beside one `emptyDir`. No token is mounted, no
    variable comes from a Secret, and no other volume or variable is copied."""
    spec = pod["spec"]
    container = next((one for one in spec["containers"]
                      if any(mount.get("mountPath") == SANDBOX.KEY_MOUNT for mount in one.get("volumeMounts", []))), None)
    if container is None:
        raise CannotRun(f"this pod mounts no gateway key at {SANDBOX.KEY_MOUNT} for the sandbox to copy")
    volumes = {volume["name"]: volume for volume in spec.get("volumes", [])}
    mounts = {mount["mountPath"]: mount for mount in container.get("volumeMounts", [])}
    key_volume = volumes.get(mounts[SANDBOX.KEY_MOUNT]["name"], {})
    secret = key_volume.get("secret") or {}
    items = secret.get("items") or []
    if len(items) != 1 or items[0].get("path") != "key" or mounts[SANDBOX.KEY_MOUNT].get("subPath"):
        raise CannotRun(f"this pod's {SANDBOX.KEY_MOUNT} is not one item of one Secret, named key")
    route_items = []
    route_name = None
    for path in (SANDBOX.MODELS_FILE, SANDBOX.OVERLAY_FILE):
        mount = mounts.get(path)
        config_map = (volumes.get((mount or {}).get("name"), {}) or {}).get("configMap")
        if mount is None or config_map is None or not mount.get("subPath"):
            raise CannotRun(f"this pod mounts no {path} from a ConfigMap for the sandbox to copy")
        if route_name not in (None, config_map["name"]):
            raise CannotRun("this pod mounts models.yml and the overlay from two ConfigMaps")
        route_name = config_map["name"]
        item = next((one for one in config_map.get("items") or [] if one.get("path") == mount["subPath"]),
                    {"key": mount["subPath"], "path": mount["subPath"]})
        route_items.append({"key": item["key"], "path": item["path"]})
    own_env = {one["name"]: one.get("value") for one in container.get("env", []) if "valueFrom" not in one}
    env = [{"name": one, "value": own_env.get(one) or environ.get(one, "")} for one in PROFILE_VARIABLES]
    env.append({"name": "PYTHONDONTWRITEBYTECODE", "value": "1"})
    env.extend({"name": one, "value": value} for one, value in sorted(variables.items()))
    if sum(len(one["value"]) for one in env) > PAYLOAD_LIMIT:
        raise ValueError("the request's bundles do not fit in the sandbox Job's variables")
    pod_spec = {
        "serviceAccountName": spec["serviceAccountName"],
        "automountServiceAccountToken": False,
        "enableServiceLinks": False,
        "restartPolicy": "Never",
        "securityContext": spec.get("securityContext") or {},
        "containers": [{
            "name": "sandbox",
            "image": image,
            "imagePullPolicy": "IfNotPresent",
            "command": ["python3", "-c", BOOT],
            "workingDir": SANDBOX.WORK_MOUNT,
            "env": env,
            "securityContext": container.get("securityContext") or {},
            "resources": container.get("resources") or {},
            "volumeMounts": [
                {"name": "gateway-key", "mountPath": SANDBOX.KEY_MOUNT, "readOnly": True},
                {"name": "route", "mountPath": SANDBOX.MODELS_FILE, "subPath": route_items[0]["path"], "readOnly": True},
                {"name": "route", "mountPath": SANDBOX.OVERLAY_FILE, "subPath": route_items[1]["path"], "readOnly": True},
                {"name": "work", "mountPath": SANDBOX.WORK_MOUNT},
            ],
        }],
        "volumes": [
            {"name": "gateway-key", "secret": {"secretName": secret["secretName"], "items": [dict(items[0])],
                                               **({"defaultMode": secret["defaultMode"]} if "defaultMode" in secret else {})}},
            {"name": "route", "configMap": {"name": route_name, "items": route_items}},
            {"name": "work", "emptyDir": {}},
        ],
    }
    for copied in ("tolerations", "nodeSelector"):
        if spec.get(copied):
            pod_spec[copied] = spec[copied]
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {"name": name, "namespace": pod["metadata"]["namespace"],
                     "labels": {"app.kubernetes.io/name": "s10-judge-sandbox"}},
        "spec": {"backoffLimit": 0, "activeDeadlineSeconds": SANDBOX.OMP_TIMEOUT + 900,
                 "ttlSecondsAfterFinished": 86400,
                 "template": {"metadata": {"labels": {"app.kubernetes.io/name": "s10-judge-sandbox"}}, "spec": pod_spec}},
    }


def redacted(spec):
    """A Job spec as it is kept: each variable the driver sent its payload in, by its sha256."""
    kept = copy.deepcopy(spec)
    for container in kept["spec"]["template"]["spec"]["containers"]:
        for one in container["env"]:
            if one["name"].startswith("S10_"):
                one["value"] = "sha256:" + sha256_of(one["value"])
    return kept


def result_of(log):
    """The result the sandbox printed on its log, checked by its count and sha256."""
    chunks = {}
    end = None
    for line in log.splitlines():
        parts = line.split(" ")
        if len(parts) == 3 and parts[0] == SANDBOX.RESULT_LINE:
            chunks[int(parts[1])] = parts[2]
        elif len(parts) == 3 and parts[0] == SANDBOX.RESULT_END:
            end = (int(parts[1]), parts[2])
    if end is None or sorted(chunks) != list(range(end[0])):
        raise ValueError("the sandbox's log holds no whole result")
    text = "".join(chunks[index] for index in range(end[0]))
    if SANDBOX.sha256_of(text) != end[1]:
        raise ValueError("the sandbox's result is not the one its sha256 names")
    return SANDBOX.unpack(text)


class KubeRunner:
    """Runs one request's sandbox as a Job in this pod's namespace, reads its log, deletes it."""

    def __init__(self, image, environ, poll=15):
        self.image = image
        self.environ = environ
        self.poll = poll
        self.program = program_variable()
        self._pod = None

    def kubectl(self, *arguments, stdin=None, check=True):
        completed = subprocess.run(["kubectl", "-n", self.namespace, *arguments], input=stdin,
                                   capture_output=True, text=True)
        if check and completed.returncode != 0:
            raise RuntimeError(f"kubectl {arguments[0]} exited {completed.returncode}: {completed.stderr.strip()[:500]}")
        return completed.stdout

    def own_pod(self):
        if self._pod is None:
            with open("/var/run/secrets/kubernetes.io/serviceaccount/namespace") as handle:
                self.namespace = handle.read().strip()
            self._pod = json.loads(self.kubectl("get", "pod", socket.gethostname(), "-o", "json"))
        return self._pod

    def run(self, ask_id, files, extra_env=None):
        pod = self.own_pod()
        name = "s10-judge-" + sha256_of(f"{ask_id} {time.time()} {os.getpid()}")[:16]
        variables = {SANDBOX.PROGRAM_VARIABLE: self.program, **payload_variables(files), **(extra_env or {})}
        run = SandboxRun(job=name)
        try:
            spec = job_spec(name, pod, self.image, variables, self.environ)
            run.facts["spec"] = redacted(spec)
            self.kubectl("create", "-f", "-", stdin=json.dumps(spec))
            deadline = time.time() + spec["spec"]["activeDeadlineSeconds"] + 600
            while True:
                status = json.loads(self.kubectl("get", "job", name, "-o", "json")).get("status") or {}
                if status.get("succeeded") or status.get("failed"):
                    break
                if time.time() > deadline:
                    raise RuntimeError("the Job did not finish by its deadline")
                time.sleep(self.poll)
            pods = json.loads(self.kubectl("get", "pods", "-l", f"job-name={name}", "-o", "json"))["items"]
            if len(pods) != 1:
                raise RuntimeError(f"the Job ran {len(pods)} pods")
            statuses = pods[0].get("status", {}).get("containerStatuses") or [{}]
            terminated = (statuses[0].get("state") or {}).get("terminated") or {}
            run.facts.update({"pod": pods[0]["metadata"]["name"], "imageId": statuses[0].get("imageID"),
                              "phase": pods[0].get("status", {}).get("phase"), "exitCode": terminated.get("exitCode")})
            run.files = result_of(self.kubectl("logs", pods[0]["metadata"]["name"], "-c", "sandbox"))
        except (RuntimeError, ValueError, OSError, KeyError) as error:
            run.failure = str(error)
        finally:
            self.kubectl("delete", "job", name, "--cascade=background", "--wait=false", "--ignore-not-found", check=False)
        return run


# ---------- one request ----------

@dataclass
class Context:
    """What judging each request reads: the frozen module, its pins, the inputs, the stage, the
    sandbox, and the image the sandbox must run."""

    shadow: object
    pins: Pins
    inputs: Inputs
    stage: Stage
    runner: object
    image: str


def deviation(line, kind, reason, problems=None, **more):
    """A deviation outcome of one request: its kind, the reason, every problem found."""
    return {**base_of(line), "outcome": "deviation", "verdict": None,
            "deviation": {"kind": kind, "reason": reason, "problems": problems or [reason]}, **more}


def built_bundles(shadow, ask_id, calls):
    """The five bundles `run_judges` wrote, by path, each named by its own call's prompt."""
    directory = os.path.join(shadow.WORK, "bundles", ask_id)
    names = sorted(os.listdir(directory))
    wanted = sorted(f"judge-{index}.md" for index in range(1, JUDGES + 1))
    if names != wanted or len(calls) != JUDGES:
        raise InputsGap(f"run_judges wrote {names} and made {len(calls)} calls, not five of each")
    bundles = {}
    for index, call in enumerate(calls, 1):
        path = os.path.join(directory, f"judge-{index}.md")
        if f"`{path}`" not in call["prompt"]:
            raise InputsGap(f"call {index}'s prompt does not name {path}")
        with open(path, "rb") as handle:
            bundles[path] = handle.read()
    return bundles


def base_of(line):
    """What every outcome carries of its inventory line."""
    return {"askId": line["askId"], "issueKey": line["issueKey"], "group": line["group"], "cases": line["cases"]}


def sandbox_record(run):
    """The record the sandbox wrote, or None where its Job gave no result."""
    if run.failure is not None or "record.json" not in run.files:
        return None
    return json.loads(run.files["record.json"])


def sandbox_problems(context, run, record):
    """The sandbox held to what it must have been: its own refusal first, then each of its checks
    read again here, and the image its pod ran."""
    if record is None:
        return [f"the sandbox Job {run.job} gave no result: {run.failure or 'its result holds no record'}"]
    if record.get("refused") is not None:
        return [f"the sandbox refused to start a judge: {record['refused']}"]
    problems = []
    refusal = SANDBOX.runtime_refusal(record.get("runtime") or {})
    if refusal is not None:
        problems.append(refusal)
    if SANDBOX.credential_variables(record.get("environment") or []):
        problems.append("the sandbox's environment names a credential")
    digest = context.image.split("@", 1)[1]
    if not str(run.facts.get("imageId") or "").endswith("@" + digest):
        problems.append(f"the sandbox ran the image {run.facts.get('imageId')}, not {context.image}")
    if not (record.get("metadata") or {}).get("refused"):
        problems.append("the metadata server answered a request without its header")
    return problems


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
    is, as S10's round reads one."""
    answers = {}
    for one in json.loads(files["results.json"]) if "results.json" in files else []:
        answer = one.get("result")
        if isinstance(answer, str):
            try:
                answer = json.loads(answer)
            except ValueError:
                pass
        answers[str(one.get("id"))] = answer
    return answers


def judged_request(context, line, calls, bundles, run, row, job):
    """The outcome of one request sent to the sandbox: every check of point 5 on the transcripts
    first, then the answers, then the verdict."""
    pins, shadow, files = context.pins, context.shadow, run.files
    problems = {kind: [] for kind in DEVIATION_KINDS}
    record = sandbox_record(run)
    problems["sandbox"] = sandbox_problems(context, run, record)
    judges = []
    answers = [None] * JUDGES
    if not problems["sandbox"]:
        found, directory = session_problems(files, record)
        for kind, listed in found.items():
            problems[kind].extend(listed)
        transcripts, strays = ({}, []) if directory is None else judge_transcripts(files, directory, calls)
        problems["session"].extend(strays)
        listing = files.get("listing.txt", b"").decode("utf-8").splitlines()
        skill_files = skill_files_of(listing, shadow.HARNESS_SKILL_DIR)
        by_agent = answers_by_agent(files)
        for index, call in enumerate(calls):
            if index not in transcripts:
                problems["transcript"].append(f"{call['label']} has no transcript")
                judges.append({"label": call["label"], "transcript": None})
                continue
            name, entries = transcripts[index]
            bundle = next(path for path in bundles if f"`{path}`" in call["prompt"])
            made = tool_calls_of(entries)
            problems["tool-call"].extend(tool_call_problems(call["label"], made, bundle, shadow.WORK,
                                                            shadow.HARNESS_SKILL_DIR, skill_files))
            wrong, served = model_problems(call["label"], entries, pins)
            problems["model"].extend(wrong)
            agent = os.path.basename(name)[:-len(".jsonl")]
            answers[index] = by_agent.get(agent)
            judges.append({"label": call["label"], "bundle": bundle, "transcript": name, "agent": agent,
                           "asked": pins.asked, "served": served,
                           "fallback": any(entry.get("resolvedModelIsFallback") is True for entry in entries
                                           if entry.get("type") == "model_change"),
                           "toolCalls": [{"id": call_id, "names": sorted(one["names"])}
                                         for call_id, one in sorted(made.items())]})
        for index, answer in enumerate(answers):
            problem = "no answer" if answer is None else answer_problem(answer, shadow.JUDGE_SCHEMA)
            if problem is not None:
                problems["answers"].append(f"{calls[index]['label']}: {problem}")
    valid = [answer if answer is not None and answer_problem(answer, shadow.JUDGE_SCHEMA) is None else None
             for answer in answers]
    judged_at = shadow.now()
    judgement = shadow.finish_judgement(job, valid, row, judged_at)
    if judgement is not None:
        judgement["judge_models"] = [{key: one[key] for key in ("asked", "served", "fallback")}
                                     for one in judges if one.get("transcript")]
    if judgement is not None and None not in valid:
        recomputed = rule_verdict(valid, shadow.KINDS)
        votes = sum((judgement.get("votes") or {}).values())
        named = sum((judgement.get("never_decide_votes") or {}).values())
        if judgement.get("verdict") != recomputed or votes != JUDGES or named != JUDGES:
            problems["verdict"].append(f"S10 recorded {judgement.get('verdict')} over {votes} votes and {named} "
                                       f"never-decide answers, and point 2's rule gives {recomputed} over five")
    facts = {key: value for key, value in run.facts.items() if key != "spec"}
    sandbox = {"job": run.job, **facts, **{key: (record or {}).get(key) for key in ("refused", "listing", "metadata", "runtime")}}
    more = {"judgement": judgement, "judges": judges, "sandbox": sandbox, "judgedAt": judged_at}
    listed = [problem for kind in DEVIATION_KINDS for problem in problems[kind]]
    if listed:
        kind = next(kind for kind in DEVIATION_KINDS if problems[kind])
        return deviation(line, kind, problems[kind][0], listed, **more)
    return {**base_of(line), "outcome": "verdict", "verdict": judgement["verdict"], "deviation": None, **more}


def kept_result(ask_id, run):
    """The files of a sandbox's result as this stage keeps them: the listing compressed."""
    kept = {}
    for name, data in run.files.items():
        if name == "listing.txt":
            kept[request_path(ask_id, "sandbox/listing.txt.xz")] = lzma.compress(data)
        else:
            kept[request_path(ask_id, f"sandbox/{name}")] = data
    kept[request_path(ask_id, "sandbox/job.json")] = json_bytes({"job": run.job, "failure": run.failure, **run.facts})
    return kept


def judge(context, line):
    """One request's outcome, from its inputs to its verdict or its deviation."""
    shadow, stage = context.shadow, context.stage
    ask_id = line["askId"]
    if line["state"] != "ready":
        return deviation(line, "inputs", line["reason"])
    request = context.inputs.request(ask_id)
    fact = shadow.legion_fact(request["ask"], request["issue"])
    if fact != request["fact"] or bool(fact.get("holds")) != request["factHold"]:
        return deviation(line, "fact", f"S10's legion_fact reads {fact}, and the inputs record {request['fact']}")
    reader = RequestReader(request, context.inputs)
    shadow.api = reader.api
    shadow.reserved_neighbour = reader.reserved_neighbour
    recorder = Recorder()
    row = {"ask_id": ask_id, "issue": request["issueKey"], "opened_at": request["ask"].get("created_at")}
    scratch = os.path.join(shadow.WORK, "bundles", ask_id)
    shutil.rmtree(scratch, ignore_errors=True)
    try:
        job = shadow.start_judgement(recorder, request["ask"], row, [])
        bundles = None if job["hold"] or reader.gaps else built_bundles(shadow, ask_id, recorder.calls)
    except Exception as error:  # noqa: BLE001 - S10's own code stops on an input it cannot read
        reader.gaps.append(f"S10 stopped on its inputs: {type(error).__name__}: {error}")
    if reader.gaps:
        shutil.rmtree(scratch, ignore_errors=True)
        return deviation(line, "inputs", reader.gaps[0], reader.gaps)
    if job["hold"] is True:
        judgement = shadow.finish_judgement(job, [], row, shadow.now())
        return {**base_of(line), "outcome": "hold", "verdict": judgement["verdict"], "deviation": None,
                "judgement": judgement, "judges": [], "sandbox": None, "judgedAt": judgement["judged_at"]}
    kept = {request_path(ask_id, os.path.basename(path)): data for path, data in bundles.items()}
    kept[request_path(ask_id, "calls.json")] = json_bytes(recorder.calls)
    stage.commit(kept)
    shutil.rmtree(scratch, ignore_errors=True)
    sent = {**bundles, SANDBOX.CALLS_FILE: json.dumps(recorder.calls).encode("utf-8")}
    run = context.runner.run(ask_id, sent)
    stage.commit(kept_result(ask_id, run))
    outcome = judged_request(context, line, recorder.calls, bundles, run, row, job)
    refused = (sandbox_record(run) or {}).get("refused")
    if refused is not None:
        # The sandbox, not the request, refused: every later request would meet the same sandbox.
        stage.commit({request_path(ask_id, "outcome.json"): json_bytes(outcome)})
        raise Stopped(f"the sandbox refused to start a judge: {refused}")
    return outcome


# ---------- the run ----------

def counts_of(outcomes):
    kinds = Counter(outcome["deviation"]["kind"] for outcome in outcomes if outcome["outcome"] == "deviation")
    verdicts = Counter(outcome["verdict"] for outcome in outcomes if outcome["outcome"] == "verdict")
    return {"requests": len(outcomes),
            "holds": sum(1 for outcome in outcomes if outcome["outcome"] == "hold"),
            "verdicts": sum(verdicts.values()),
            "byVerdict": dict(sorted(verdicts.items())),
            "deviations": sum(kinds.values()),
            "deviationsByKind": dict(sorted(kinds.items()))}


def finish(stage, inventory, stopped):
    """Every request of the inventory its one outcome, then the outcomes and the record written:
    a request started and not finished is `started`, one never reached `unjudged`."""
    for line in inventory:
        path = request_path(line["askId"], "outcome.json")
        if stage.has(path):
            continue
        if stage.has(request_path(line["askId"], "calls.json")):
            outcome = deviation(line, "started", "the request was started and not finished, and is never judged again")
        else:
            outcome = deviation(line, "unjudged", f"the run stopped before it judged the request: {stopped}")
        stage.commit({path: json_bytes(outcome)})
    outcomes = []
    lines = []
    for line in inventory:
        path = request_path(line["askId"], "outcome.json")
        outcome = json.loads(stage.read(path))
        outcomes.append(outcome)
        lines.append(json.dumps({"askId": line["askId"], "issueKey": line["issueKey"], "group": line["group"],
                                 "cases": line["cases"], "outcome": outcome["outcome"], "verdict": outcome["verdict"],
                                 "deviation": outcome["deviation"] and outcome["deviation"]["kind"],
                                 "outcomeSha256": stage.record["files"][path]}, sort_keys=True))
    text = ("\n".join(lines) + "\n").encode("utf-8") if lines else b""
    if not stage.has(OUTCOMES_FILE):
        stage.commit({OUTCOMES_FILE: text})
    elif stage.read(OUTCOMES_FILE) != text:
        raise CannotRun(f"{OUTCOMES_FILE} is not the outcomes this stage's files hold")
    stage.record.update({"state": "written", "stopped": stopped, "finishedAt": utc_now(),
                         "counts": counts_of(outcomes)})
    stage.save()
    return stage.record["counts"]


def _stop_on_signal(signum, frame):
    raise Stopped(f"the run received signal {signum}")


def run(flags, argv, environ, pins, admit, runner_of):
    admission = admit(argv, environ)
    code = frozen_files(flags.code, pins)
    skill = skill_files(flags.skill, flags.never_decide, pins)
    chain = {"claimRun": admission["claimRun"], "reference": admission["reference"],
             "runDigest": admission["runDigest"],
             "inputs": {"stageSha256": admission["inputs"]["stageSha256"], "files": admission["inputs"]["files"]},
             "frozen": {"codeSha256": pins.code_sha256, "files": {name: sha256_of(data) for name, data in code.items()},
                        "promptSha256": pins.prompt_sha256, "bundleCodeSha256": pins.bundle_code_sha256,
                        "skillSha256": pins.skill_sha256, "asked": pins.asked, "model": pins.model},
             "image": flags.image.strip(),
             "directories": {"inputs": admission["inputs"]["directory"]}}
    stage = Stage.open(admission["out"], chain)
    inputs = Inputs(admission)
    print(f"judged            {stage.directory}")
    print(f"claim run         {chain['claimRun']}, under {chain['reference']}")
    print(f"requests          {len(inputs.inventory)} in the inventory")
    if stage.record["state"] == "written":
        print(f"written before    {stage.record['counts']}")
        return 2 if stage.record["stopped"] else 0
    missing = {f"{CODE_DIRECTORY}/{name}": data for name, data in code.items()
               if not stage.has(f"{CODE_DIRECTORY}/{name}")}
    if missing:
        stage.commit(missing)
    shadow = load_shadow(os.path.join(stage.directory, CODE_DIRECTORY, "shadow.py"), pins)
    place_skill(shadow, skill)
    context = Context(shadow=shadow, pins=pins, inputs=inputs, stage=stage,
                      runner=runner_of(flags.image.strip(), environ), image=flags.image.strip())
    stopped = None
    previous = signal.signal(signal.SIGTERM, _stop_on_signal)
    try:
        for line in inputs.inventory:
            path = request_path(line["askId"], "outcome.json")
            if stage.has(path) or stage.has(request_path(line["askId"], "calls.json")):
                continue
            outcome = judge(context, line)
            stage.commit({path: json_bytes(outcome)})
            print(f"  {line['askId']}: {outcome['outcome']} {outcome['verdict'] or outcome['deviation']['kind']}")
    except (Stopped, KeyboardInterrupt) as error:
        stopped = str(error) or type(error).__name__
    except Exception as error:
        stopped = f"{type(error).__name__}: {error}"
        finish(stage, inputs.inventory, stopped)
        raise
    finally:
        signal.signal(signal.SIGTERM, previous)
    counts = finish(stage, inputs.inventory, stopped)
    print(f"written           {counts}")
    if stopped:
        print(f"stopped           {stopped}", file=sys.stderr)
        return 2
    return 0


def main(argv, *, pins=FROZEN, admit=admit_with_bun, runner_of=KubeRunner, environ=None):
    """The command: its flags, the run, and the exit status it takes."""
    environ = dict(os.environ if environ is None else environ)
    try:
        flags = flags_of(argv)
        return run(flags, argv, environ, pins, admit, runner_of)
    except CannotRun as error:
        print(f"S10JudgeError: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
