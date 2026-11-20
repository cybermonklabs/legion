"""The claim run's fourth stage: S10, frozen, judges each request of the inventory (point 14 (b)).

After the build (`build-corpus.ts`), the draw (`draw.ts`) and the inputs (`s10-inputs.ts`), and
before the analysis (`analyze.ts --claim-s10`), under the registered read and the claim run:

    python3 scripts/approval-eval/s10_judge.py --inputs "$CLAIM/s10-inputs" \\
      --out "$CLAIM/s10-judged" --code <code-txt file> --skill <SKILL.md> \\
      --never-decide <references/never-decide.md> --image <image>@sha256:<digest>

The modules. This file is the driver: the admission, the frozen code, the inputs and each
request's judging. The stage on disk is `s10_store.py`, the sandbox's Job `s10_job.py`, the
program the sandbox runs `s10_sandbox.py`, and point 5's checks of the transcripts
`s10_transcripts.py`. `s10-judged-io.ts` reads the stage's files back for the analysis.

The admission. Before anything else is read, `s10-judge-admit.ts` decides whether this run may
read the reserve, by the one implementation every stage of the claim uses (`reserveAccessOf` in
`reserve.ts`, with the run digest of this command and its arguments, `runDigestOf`), and checks
that `--inputs` is the inputs stage this claim run wrote under this read: its record written, its
claim run the one `APPROVAL_EVAL_CLAIM_RUN` names, and every file it names there with the bytes it
names, each read back by the shapes `s10-inputs-io.ts` holds. `--out` must be exactly
`<claim>/s10-judged`, `<claim>` being the one directory that holds the build and the draw the
inputs stage's record names and `--inputs` itself, so the claim run's judging has one place. This
driver reads only those files, and each only with the sha256 the admission read. `--image` must
be the image this driver's own pod runs, by its digest (`image_refusal`): the session image the
claim's run digest names, so the sandbox's own files are that image's. The sandbox runs as
`--service-account`, one of its own unless the option names another, and never the account this
driver's own pod runs as.

The frozen code. `--code` is the code-txt file of point 2 as `dispatch doc-read` prints it: its
two header lines, then the text whose sha256 point 2 fixes, split into its five files by their
`=== FILE: <name> ===` lines, each checked against the sha256 point 2 gives it. Every `DISPATCH_`
variable leaves this process's environment once the admission has run. Its `shadow.py` is kept
under `--out` and run from the bytes the code-txt held, checked again against point 2's sha256
and against the kept copy just before it runs, with `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` set
to values that reach nothing while it loads and removed after; then the reads of point 3 are
replaced, and nothing else: its `api`, through which its `issue`, `search`, `doc_version` and the
ask reads of its `neighbours` go, by a reader over the request's inputs (`RequestReader`), and its
`reserved_neighbour` by the reserve's decision the inputs stage recorded (point 4 (d)). Its
`subprocess` and `urllib` raise on any use, and while any of S10's code runs (`sealed`) every way
out of the process raises for every module, under any name it was imported by: the socket
constructors and lookups, `subprocess.Popen` and `os`'s process calls, restored once its code
returns (the driver's own commands, `kubectl` and `bun`, run outside). `JUDGE_PROMPT` and
`build_bundle` are checked against point 2's `prompt_sha256` and `bundle_code_sha256`,
`JUDGE_SCHEMA` against the keywords the answer check reads, and the skill files against point 2's
`skill_sha256`; they are put where its `skill_bytes` reads them, as the code-txt's own header says.

Each request, once, in the inventory's order (point 6):

- A request the inputs stage could not give as of its judged moment is a deviation (`inputs`).
- S10's `legion_fact` is run over the request file's ask and issue, and a fact that is not the
  one the file records is a deviation (`fact`).
- `started.json` is kept, and S10's own `start_judgement` runs: `prepare` and the fact check,
  each document at its version, `neighbours` over the archived search, and `run_judges`, which
  builds the five bundles under `WORK` with its own `time.time()` seed and makes its five
  `agent()` calls, which are recorded here and not made. `WORK/bundles` is a link to
  `--out/building` while the run lasts (`link_bundles`), so every bundle it writes, at the path
  its prompt names, lands under `--out`. A read the inputs cannot give, or an error S10's own code
  raises on them, is a deviation (`inputs`); an error of this driver stops the run. A fact that
  holds ends there: `finish_judgement` gives `never_decide:waiting-legion-root` with no judge.
  Before the run's first `started.json`, the sandbox's preflight runs once (`preflight`): the
  sandbox's Job with no bundle, every check of it made and made again here, its result kept
  under `preflights/`; any failure stops the run before a request has started.
- Else the five bundles and the five calls are kept under `--out`, and only then is the request
  sent to the sandbox (`s10_sandbox.py`, one Kubernetes Job, `s10_job.py`), whose one `omp -p`
  makes those five calls. Its result is read from the Job's log, the Job is deleted, and every
  file of the result is kept, the gateway key's bytes replaced. Then, in code and before any
  answer is compared (point 5, version 6, `s10_transcripts.py`): the sandbox's checks, made again
  here from its record, and a result whose files can be read (`sandbox`); the work tree and the
  skill tree had the required manifests before the first judge and the same after the last
  (`manifest`); no kept file held the gateway key's bytes (`key`); the session's own transcript
  holds the one `eval` call and nothing else (`session`); there are five judges' transcripts
  (`transcript`); no judge made a tool call but its `yield` answer, a text read (a `read` of a
  file, any `grep`) of only its own bundle file or `skill://approval-review` files, or a listing
  (a `read` the checker establishes is of a directory of the two trees and answers only names,
  sizes and ages, or any `glob`) whose whole scope lies inside the work tree or the skill tree,
  every target as given, defaulted and resolved, and every entry a result returns one its tree's
  manifest holds (`tool-call`); each judge was asked point 2's model, none is marked a fallback,
  and each model omp reports serving it is that model (`model`). Then five valid answers
  (`answers`); the verdict is S10's `aggregate` over them, through its `finish_judgement`, and it
  must be the verdict point 2's rule gives (`verdict`). Each outcome also records its outcome under
  the tool rule of versions 3, 4, 5 and 6, with the calls each makes a deviation (`byRule`), so the
  analysis reports what changes under each with no further judging; version 6's is the one that
  counts.

Stops. A request is started once its `started.json` is kept, before S10's code runs on it, and a
request started and not finished is a deviation (`started`) that no later run judges again (point
6): the next run records it so before it judges anything else. A stop writes nothing more for the
request in flight, records itself, and leaves the stage `started` for the same command to resume:
a SIGTERM or an interrupt; a sandbox that refuses to start a judge, since every later request
would meet the same sandbox until its cause is cleared; a failed preflight; and a Kubernetes API
call that still fails once it has been tried again (`s10_job.py`), which is no fault of the
request's. A Job that ran and gave no readable result is that request's own `sandbox` deviation.
An error of this driver also writes nothing more and leaves the stage `started`, but records no
stop: it ends the command with its traceback. Only a run that gives every request of the
inventory its outcome writes `outcomes.jsonl` and marks the stage written; a request that no run
reached is reported by the analysis (`s10-judged-io.ts`), as `started` where its `started.json`
is kept and as `unjudged` where it is not, and this stage writes neither for it.
"""

import sys

# Nothing this imports may leave a `__pycache__` in scripts/approval-eval: it would change the run
# digest, which covers every file of that directory.
sys.dont_write_bytecode = True

import _posixsubprocess  # noqa: E402
import _socket  # noqa: E402
import argparse  # noqa: E402
import contextlib  # noqa: E402
import copy  # noqa: E402
import inspect  # noqa: E402
import json  # noqa: E402
import lzma  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
import shutil  # noqa: E402
import signal  # noqa: E402
import socket  # noqa: E402
import subprocess  # noqa: E402
import traceback  # noqa: E402
import types  # noqa: E402
from collections import Counter  # noqa: E402
from dataclasses import dataclass  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import s10_job as JOB  # noqa: E402
import s10_sandbox as SANDBOX  # noqa: E402
import s10_transcripts as CHECKS  # noqa: E402
from s10_store import (  # noqa: E402
    BUILDING_DIRECTORY,
    CODE_DIRECTORY,
    OUTCOME_FILE,
    OUTCOMES_FILE,
    PREFLIGHT_DIRECTORY,
    STARTED_FILE,
    CannotRun,
    Stage,
    json_bytes,
    request_path,
    sha256_of,
    utc_now,
)

USAGE = """usage: python3 scripts/approval-eval/s10_judge.py --inputs <inputs dir> --out <dir>
    --code <code-txt file> --skill <SKILL.md> --never-decide <never-decide.md>
    --image <image>@sha256:<digest> [--service-account <name>]

Judges each request of the claim's inventory with S10 frozen: the frozen code builds and keeps
each request's five evidence bundles here, and five judges answer in a sandbox Job that holds the
bundles and the runtime alone. Runs only under the claim run's registered read, over the inputs
stage that claim run wrote.

Options:
  --inputs <dir>        Required. The claim run's inputs stage: <claim>/s10-inputs.
  --out <dir>           Required. Where the judged stage is written: <claim>/s10-judged, beside the
                        inputs. A directory this command wrote in the same claim run, over the
                        same inputs, is resumed.
  --code <file>         Required. The frozen S10 code-txt file, as `dispatch doc-read` prints it.
  --skill <file>        Required. The skill's SKILL.md at the commit S10 froze.
  --never-decide <file> Required. The skill's references/never-decide.md at that commit.
  --image <ref>         Required. The image this driver's own pod runs, by digest:
                        <name>@sha256:<64 hex digits>.
  --service-account <name>
                        The ServiceAccount the sandbox runs as. Default: s10-judge-sandbox.

Environment:
  APPROVAL_EVAL_RESERVE=read:<reference>, APPROVAL_EVAL_CLAIM_RUN=<start reply id>
                        Required: the read claim.ts registers for this run's digest, and the
                        claim run the inputs stage recorded.

Exit status: 0 when every request has its outcome, 2 when the command could not run or stopped
(a stopped run leaves its stage to be resumed by the same command)."""

JUDGES = 5

# The deviation kinds this stage writes, in the order a request's problems are weighed: its first
# is its kind. `s10-judged-io.ts` reads the same list, and adds `unjudged` for a request no run
# reached.
DEVIATION_KINDS = ("inputs", "fact", "sandbox", "manifest", "key", "session", "transcript", "tool-call",
                   "model", "answers", "verdict", "started")

# The Bun that runs the admission, by its path in the session image, never one `PATH` finds.
BUN = "/usr/local/bin/bun"


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


class Stopped(Exception):
    """The run stops: it writes nothing more, and the same command resumes the stage."""


class InputsGap(Exception):
    """S10 read something the inputs do not give as of the judged moment (point 3)."""


# ---------- flags and the admission ----------

class _Parser(argparse.ArgumentParser):
    def error(self, message):
        raise CannotRun(f"{message}\n\n{USAGE}")


def flags_of(argv):
    """The flags, as `s10-judge-admit.ts` reads the same arguments."""
    parser = _Parser(add_help=False, usage=USAGE)
    for name in ("inputs", "out", "code", "skill", "never-decide", "image"):
        parser.add_argument(f"--{name}", required=True)
    parser.add_argument("--service-account", default=JOB.SERVICE_ACCOUNT)
    flags = parser.parse_args(argv)
    if not re.fullmatch(r"[^@\s]+@sha256:[0-9a-f]{64}", flags.image.strip()):
        raise CannotRun(f"--image takes an image by its digest, <name>@sha256:<64 hex digits>, not {flags.image}")
    if not re.fullmatch(r"[a-z0-9]([-a-z0-9.]*[a-z0-9])?", flags.service_account):
        raise CannotRun(f"--service-account takes a ServiceAccount's name, not {flags.service_account}")
    return flags


def admit_with_bun(argv, environ):
    """The admission, as `s10-judge-admit.ts` decides it for these arguments, or `CannotRun`."""
    if not (os.path.isfile(BUN) and os.access(BUN, os.X_OK)):
        raise CannotRun(f"{BUN}, the Bun the admission runs on, is not there")
    entry = os.path.join(HERE, "s10-judge-admit.ts")
    completed = subprocess.run([BUN, entry, *argv], env=dict(environ), capture_output=True, text=True)
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


def _refused(name):
    def refused(*arguments, **options):
        raise InputsGap(f"S10 reached {name}, which no input of the claim gives")
    return refused


# The `os` calls that run a command or start a process.
OS_COMMANDS = re.compile(r"^(system|popen|fork|forkpty|exec\w*|spawn\w*|posix_spawn\w*|kill|killpg)$")


@contextlib.contextmanager
def sealed():
    """While S10's code runs, every way out of the process raises, for every module and under any
    name it was imported by: a connection (`socket`'s constructors and lookups, and `_socket`'s), a
    command (`subprocess.Popen`, under every function of `subprocess`, and `_posixsubprocess`), and
    `os`'s process calls. All is restored once its code returns, so the driver's own `kubectl` and
    `bun` run as before."""
    targets = [(socket, ("socket", "create_connection", "socketpair", "fromfd", "getaddrinfo")),
               (_socket, ("socket",)), (subprocess, ("Popen",)), (_posixsubprocess, ("fork_exec",)),
               (os, tuple(name for name in dir(os) if OS_COMMANDS.match(name)))]
    saved = []
    try:
        for module, names in targets:
            for name in names:
                if hasattr(module, name):
                    saved.append((module, name, getattr(module, name)))
                    setattr(module, name, _refused(f"{module.__name__}.{name}"))
        yield
    finally:
        for module, name, value in reversed(saved):
            setattr(module, name, value)


def load_shadow(path, pins, data):
    """`shadow.py` loaded as written: `data`, the bytes the code-txt held, held once more to the
    sha256 point 2 gives it and to the kept copy at `path` its tracebacks and `inspect` read, then
    run from those bytes inside `sealed`, with what reaches out refused (the module's docstring
    names each), and its pins checked."""
    if not sha256_of(data).startswith(pins.files["shadow.py"]):
        raise CannotRun(f"shadow.py has sha256 {sha256_of(data)}, and point 2 gives {pins.files['shadow.py']}…")
    with open(path, "rb") as handle:
        if handle.read() != data:
            raise CannotRun(f"{path} is not the shadow.py of the code-txt it was kept from")
    shadow = types.ModuleType("s10_frozen_shadow")
    shadow.__file__ = path
    os.environ.update(UNREACHABLE)
    try:
        with sealed():
            exec(compile(data, path, "exec"), shadow.__dict__)  # noqa: S102 - the pinned frozen code
    finally:
        for name in UNREACHABLE:
            os.environ.pop(name, None)
    shadow.subprocess = _Refused("subprocess")
    shadow.urllib = _Refused("urllib")
    measured = {
        "JUDGE_PROMPT": (sha256_of(shadow.JUDGE_PROMPT), pins.prompt_sha256),
        "build_bundle": (sha256_of(inspect.getsource(shadow.build_bundle)), pins.bundle_code_sha256),
        "SKILL_SHA": (shadow.SKILL_SHA, pins.skill_sha256),
        "ASKED": (shadow.ASKED, pins.asked),
        "MODEL": (shadow.MODEL, pins.model),
        "WORK": (shadow.WORK, pins.work),
        "HARNESS_SKILL_DIR": (shadow.HARNESS_SKILL_DIR, SANDBOX.SKILL_DIR),
    }
    for name, (found, wanted) in measured.items():
        if found != wanted:
            raise CannotRun(f"the frozen code's {name} is {found!r}, and point 2 fixes {wanted!r}")
    refusal = CHECKS.schema_refusal(shadow.JUDGE_SCHEMA)
    if refusal is not None:
        raise CannotRun(refusal)
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
    with sealed():
        shadow.skill_bytes()


def raised_by_frozen_code(error, shadow):
    """Whether S10's own code raised `error`: its traceback passes through the frozen file, and no
    frame of this driver's own files comes after the last one there."""
    frames = traceback.extract_tb(error.__traceback__)
    frozen = [index for index, frame in enumerate(frames) if frame.filename == shadow.__file__]
    if not frozen:
        return False
    return not any(os.path.dirname(os.path.abspath(frame.filename)) == HERE for frame in frames[frozen[-1] + 1:])


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
            candidate = self.candidate(match.group(1))
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
        results = []
        for hit in self.inputs.page(page["sha256"])["hits"]:
            # A rank the server gave none of is 0, as the inputs stage ordered the hits (`neighbourOrder`).
            one = {"id": hit["id"], "kind": hit["kind"], "rank": 0 if hit["rank"] is None else hit["rank"]}
            if hit["kind"] == "ask":
                candidate = self.candidate(hit["id"])
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
        candidate = self.candidate(hit_ask_id)
        if candidate is not None and "ask" in candidate:
            return False
        self.gap(f"S10 asked the reserve about {hit_ask_id}, which the inputs did not keep")
        return True

    def candidate(self, ask_id):
        """One candidate as the inputs record it, its held or dropped mark read before any field
        of its ask: a held or dropped one gives no field, and one that carries a mark and fields
        both, or neither, is a gap. None where the inputs record no outcome of the ask."""
        candidate = (self.request.get("candidates") or {}).get(ask_id)
        if candidate is None:
            return None
        marked = "held" in candidate or "dropped" in candidate
        if marked == ("ask" in candidate):
            self.gap(f"the inputs record the candidate {ask_id} as {'held or dropped and kept' if marked else 'nothing'}")
            return {}
        return {} if marked else candidate


class Recorder:
    """S10's `agent_fn`: records each call `run_judges` makes, and starts no agent."""

    def __init__(self):
        self.calls = []

    def __call__(self, prompt, **options):
        if sorted(options) != ["agent", "label", "model", "schema"]:
            raise InputsGap(f"run_judges called agent() with {sorted(options)}")
        self.calls.append({"prompt": prompt, **options})
        return len(self.calls) - 1


# ---------- one request ----------

@dataclass
class Context:
    """What judging each request reads: the frozen module, its pins, the inputs, the stage, the
    sandbox, the image the sandbox must run, and whether this run's preflight passed."""

    shadow: object
    pins: Pins
    inputs: Inputs
    stage: Stage
    runner: object
    image: str
    preflighted: bool = False


def outcome_of(line, outcome, verdict=None, deviation=None, judgement=None, judges=None, sandbox=None,
               judged_at=None, by_rule=None):
    """One request's outcome, every key of it present (`s10-judged-io.ts` reads the same shape).
    `byRule` is its outcome under each version of point 5's tool rule (`by_rule_of`); where no
    judge's transcript was read, each version's is the outcome itself, with no call."""
    if by_rule is None:
        same = {"outcome": outcome, "verdict": verdict, "deviation": deviation and deviation["kind"],
                "toolCallDeviations": []}
        by_rule = {f"v{version}": dict(same) for version in CHECKS.RULE_VERSIONS}
    return {"askId": line["askId"], "issueKey": line["issueKey"], "group": line["group"], "cases": line["cases"],
            "outcome": outcome, "verdict": verdict, "deviation": deviation, "judgement": judgement,
            "judges": judges or [], "sandbox": sandbox, "judgedAt": judged_at, "byRule": by_rule}


def deviation(line, kind, reason, problems=None, **more):
    """A deviation outcome of one request: its kind, one of `DEVIATION_KINDS`, the reason, and
    every problem found."""
    if kind not in DEVIATION_KINDS:
        raise ValueError(f"{kind!r} is no deviation kind of this stage")
    return outcome_of(line, "deviation", deviation={"kind": kind, "reason": reason, "problems": problems or [reason]},
                      **more)


def by_rule_of(problems, tool_calls, verdict):
    """A judged request's outcome under each version of point 5's tool rule, over the same
    transcripts: every problem but the tool calls is the same under each; `tool_calls` holds, by
    version, the calls that version makes a deviation; `verdict` is the verdict five valid answers
    give, or None. Each version's outcome, its first deviation kind, and those calls."""
    by_rule = {}
    for version in CHECKS.RULE_VERSIONS:
        kinds = [kind for kind in DEVIATION_KINDS
                 if (tool_calls[version] if kind == "tool-call" else problems[kind])]
        outcome = {"outcome": "deviation", "verdict": None, "deviation": kinds[0]} if kinds else \
            {"outcome": "verdict", "verdict": verdict, "deviation": None}
        outcome["toolCallDeviations"] = [{key: one[key] for key in ("judge", "callId", "tools", "reason")}
                                         for one in tool_calls[version]]
        by_rule[f"v{version}"] = outcome
    return by_rule


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


def sandbox_record(run):
    """The record the sandbox wrote, or None where its Job gave no result or no readable record."""
    if run.failure is not None or "record.json" not in run.files:
        return None
    try:
        record = json.loads(run.files["record.json"])
    except ValueError:
        return None
    return record if isinstance(record, dict) else None


def sandbox_problems(context, run, record, payload, preflight=False):
    """The sandbox held to what it must have been: its own refusal first; then each of its checks
    made again here from what it recorded (the variables' names; the mounts, and the listing
    against the files `payload` names, both held to the layout it recorded; the runtime; the
    metadata answers); the preflight's mark, true only for the preflight's own Job; and the image
    its pod ran."""
    if record is None:
        return [f"the sandbox Job {run.job} gave no readable result: {run.failure or 'its result holds no record'}"]
    if record.get("refused") is not None:
        return [f"the sandbox refused to start a judge: {record['refused']}"]
    problems = []
    if SANDBOX.credential_variables(record.get("environment") or []):
        problems.append("the sandbox's environment names a credential")
    layout = record.get("layout")
    if not isinstance(layout, dict) or sorted(layout) != ["key", "models", "overlay", "work"] or not all(
            isinstance(path, str) for path in layout.values()):
        problems.append("the sandbox recorded no layout its checks held the mounts and files to")
    else:
        try:
            listing = run.files.get("listing.txt", b"").decode("utf-8").splitlines()
        except UnicodeDecodeError:
            listing = None
        refusal = (SANDBOX.mount_refusal(record.get("mounts") or [], layout)
                   or ("the sandbox's listing cannot be read" if listing is None else None)
                   or SANDBOX.listing_refusal(listing, set(payload), layout))
        if refusal is not None:
            problems.append(refusal)
    refusal = SANDBOX.runtime_refusal(record.get("runtime") or {})
    if refusal is not None:
        problems.append(refusal)
    refusal = SANDBOX.metadata_refusal(record.get("metadata"))
    if refusal is not None:
        problems.append(refusal)
    if record.get("preflight") is not preflight:
        problems.append(f"the sandbox's record marks it a preflight {record.get('preflight')!r}, not {preflight!r}")
    digest = context.image.split("@", 1)[1]
    if not str(run.facts.get("imageId") or "").endswith("@" + digest):
        problems.append(f"the sandbox ran the image {run.facts.get('imageId')}, not {context.image}")
    return problems


def judged_request(context, line, calls, bundles, run, row, job, key_held, sent):
    """The outcome of one request sent to the sandbox, whose record refused nothing: the sandbox
    held to its checks first (`sandbox`, which ends there), then every check of point 5 on the
    transcripts, then the answers, then the verdict. `key_held` names the result's files this
    driver found the gateway key's bytes in; `sent`, the files it sent. Runs inside `sealed`."""
    pins, shadow, files = context.pins, context.shadow, run.files
    record = sandbox_record(run)
    judged_at = shadow.now()
    facts = {key: value for key, value in run.facts.items() if key != "spec"}
    sandbox = {"job": run.job, **facts,
               **{key: (record or {}).get(key) for key in ("refused", "listing", "metadata", "runtime", "manifests",
                                                           "keyFound")}}
    problems = {kind: [] for kind in DEVIATION_KINDS}

    def ended():
        listed = [problem for kind in DEVIATION_KINDS for problem in problems[kind]]
        return deviation(line, "sandbox", problems["sandbox"][0], listed, sandbox=sandbox, judged_at=judged_at)

    problems["sandbox"] = sandbox_problems(context, run, record, sent)
    if problems["sandbox"]:
        return ended()
    manifest = SANDBOX.manifest_problem(record.get("manifests"), line["askId"])
    if manifest is not None:
        problems["manifest"].append(manifest)
    held = sorted(set(record.get("keyFound") or []) | set(key_held))
    if held:
        problems["key"].append(f"the gateway key's bytes were in {held}; each is kept with them replaced")
    judges = []
    answers = [None] * JUDGES
    tool_calls = {version: [] for version in CHECKS.RULE_VERSIONS}
    if "omp" in record:
        try:
            found, directory = CHECKS.session_problems(files, record)
            transcripts, strays = ({}, []) if directory is None else CHECKS.judge_transcripts(files, directory, calls)
            listing = files.get("listing.txt", b"").decode("utf-8").splitlines()
            by_agent = CHECKS.answers_by_agent(files)
        except (ValueError, UnicodeDecodeError) as error:
            problems["sandbox"].append(f"the sandbox's result holds a file that cannot be read: {error}")
            return ended()
        for kind, listed in found.items():
            problems[kind].extend(listed)
        problems["session"].extend(strays)
        for index, call in enumerate(calls):
            if index not in transcripts:
                problems["transcript"].append(f"{call['label']} has no transcript")
                judges.append({"label": call["label"], "transcript": None})
                continue
            name, entries = transcripts[index]
            bundle = next(path for path in bundles if f"`{path}`" in call["prompt"])
            made = CHECKS.tool_calls_of(entries)
            scope = CHECKS.scope_of(bundle, shadow.WORK, listing, record.get("manifests"))
            for version in CHECKS.RULE_VERSIONS:
                tool_calls[version].extend(CHECKS.tool_call_problems(call["label"], made, scope, version))
            wrong, served = CHECKS.model_problems(call["label"], entries, pins.asked, pins.model)
            problems["model"].extend(wrong)
            agent = os.path.basename(name)[:-len(".jsonl")]
            answers[index] = by_agent.get(agent)
            judges.append({"label": call["label"], "bundle": bundle, "transcript": name, "agent": agent,
                           "asked": pins.asked, "served": served,
                           "fallback": any(entry.get("resolvedModelIsFallback") is True for entry in entries
                                           if entry.get("type") == "model_change"),
                           "toolCalls": [{"id": call_id, "names": sorted(one["names"])}
                                         for call_id, one in sorted(made.items())]})
        problems["tool-call"] = [one["problem"] for one in tool_calls[CHECKS.COUNTING_RULE]]
    checked = [CHECKS.answer_problem(answer, shadow.JUDGE_SCHEMA) for answer in answers]
    if "omp" in record:
        for index, problem in enumerate(checked):
            if problem is not None:
                problems["answers"].append(f"{calls[index]['label']}: {problem}")
    valid = [answer if problem is None else None for answer, problem in zip(answers, checked)]
    judgement = shadow.finish_judgement(job, valid, row, judged_at)
    if judgement is not None:
        judgement["judge_models"] = [{key: one[key] for key in ("asked", "served", "fallback")}
                                     for one in judges if one.get("transcript")]
    if judgement is not None and None not in valid:
        recomputed = CHECKS.rule_verdict(valid, shadow.KINDS)
        votes = sum((judgement.get("votes") or {}).values())
        named = sum((judgement.get("never_decide_votes") or {}).values())
        if judgement.get("verdict") != recomputed or votes != JUDGES or named != JUDGES:
            problems["verdict"].append(f"S10 recorded {judgement.get('verdict')} over {votes} votes and {named} "
                                       f"never-decide answers, and point 2's rule gives {recomputed} over five")
    by_rule = by_rule_of(problems, tool_calls, judgement and judgement.get("verdict"))
    more = {"judgement": judgement, "judges": judges, "sandbox": sandbox, "judged_at": judged_at, "by_rule": by_rule}
    listed = [problem for kind in DEVIATION_KINDS for problem in problems[kind]]
    if listed:
        kind = next(kind for kind in DEVIATION_KINDS if problems[kind])
        return deviation(line, kind, problems[kind][0], listed, **more)
    return outcome_of(line, "verdict", verdict=judgement["verdict"], **more)


def kept_result(directory, run, key):
    """The files of a sandbox's result as this stage keeps them under `directory`, the listing
    compressed, the Job's facts beside them (`job.json`), and the gateway key's bytes replaced
    wherever they are; and the names of the files that held them."""
    files, held = SANDBOX.redact_key(run.files, key)
    run.files = files
    kept = {}
    for name, data in files.items():
        if name == "listing.txt":
            kept[f"{directory}/listing.txt.xz"] = lzma.compress(data)
        else:
            kept[f"{directory}/{name}"] = data
    kept[f"{directory}/job.json"] = json_bytes({"job": run.job, "failure": run.failure, **run.facts})
    return kept, held


def preflight(context):
    """Once a run, before its first request starts: the sandbox with no bundle and no judge
    (`KubeRunner.preflight`), its result kept under `preflights/<n>/`, and every check of
    `sandbox_problems` made on it. A refusal, a failed check, a gateway key in its files or a Job
    that gave no readable result stops the run before any request has started, so no request
    meets a sandbox that would refuse it."""
    if context.preflighted:
        return
    stage = context.stage
    run = context.runner.preflight()
    done = {name.split("/")[1] for name in stage.record["files"] if name.startswith(PREFLIGHT_DIRECTORY + "/")}
    kept, held = kept_result(f"{PREFLIGHT_DIRECTORY}/{len(done)}", run, context.runner.key)
    stage.commit(kept)
    problems = sandbox_problems(context, run, sandbox_record(run), {}, preflight=True)
    if held:
        problems.append(f"the gateway key's bytes were in the preflight's {held}")
    if problems:
        raise Stopped(f"the sandbox's preflight, Job {run.job}, stops the run before any request: {problems[0]}")
    context.preflighted = True


def judge(context, line):
    """One request's outcome, from its inputs to its verdict or its deviation. S10's code runs
    inside `sealed`; the sandbox runs between. A sandbox that refuses to start a judge stops the
    run with no outcome for the request, as any stop does."""
    shadow, stage = context.shadow, context.stage
    ask_id = line["askId"]
    if line["state"] != "ready":
        return deviation(line, "inputs", line["reason"])
    request = context.inputs.request(ask_id)
    with sealed():
        fact = shadow.legion_fact(request["ask"], request["issue"])
    if fact != request["fact"] or bool(fact.get("holds")) != request["factHold"]:
        return deviation(line, "fact", f"S10's legion_fact reads {fact}, and the inputs record {request['fact']}")
    preflight(context)
    reader = RequestReader(request, context.inputs)
    shadow.api = reader.api
    shadow.reserved_neighbour = reader.reserved_neighbour
    recorder = Recorder()
    row = {"ask_id": ask_id, "issue": request["issueKey"], "opened_at": request["ask"].get("created_at")}
    scratch = os.path.join(shadow.WORK, "bundles", ask_id)
    stage.commit({request_path(ask_id, STARTED_FILE): json_bytes({"askId": ask_id, "startedAt": utc_now()})})
    try:
        try:
            with sealed():
                job = shadow.start_judgement(recorder, request["ask"], row, [])
                bundles = None if job["hold"] or reader.gaps else built_bundles(shadow, ask_id, recorder.calls)
        except InputsGap as error:
            if str(error) not in reader.gaps:
                reader.gaps.append(str(error))
        except Exception as error:
            if not raised_by_frozen_code(error, shadow):
                raise
            reader.gaps.append(f"S10's own code stopped on its inputs: {type(error).__name__}: {error}")
        if reader.gaps:
            return deviation(line, "inputs", reader.gaps[0], reader.gaps)
        if job["hold"] is True:
            with sealed():
                judgement = shadow.finish_judgement(job, [], row, shadow.now())
            return outcome_of(line, "hold", verdict=judgement["verdict"], judgement=judgement,
                              judged_at=judgement["judged_at"])
        kept = {request_path(ask_id, os.path.basename(path)): data for path, data in bundles.items()}
        kept[request_path(ask_id, "calls.json")] = json_bytes(recorder.calls)
        stage.commit(kept)
    finally:
        # What the frozen code wrote went to `--out/building/<ask id>` through the link; what a
        # record kept is under `requests/<ask id>/` now, and the rest is never kept.
        shutil.rmtree(scratch, ignore_errors=True)
    sent = {**bundles, SANDBOX.CALLS_FILE: json.dumps(recorder.calls).encode("utf-8")}
    run = context.runner.run(ask_id, sent)
    kept, held = kept_result(request_path(ask_id, "sandbox"), run, context.runner.key)
    stage.commit(kept)
    refused = (sandbox_record(run) or {}).get("refused")
    if refused is not None:
        # The sandbox, not the request, refused, though the preflight passed: every later request
        # would meet the same sandbox until its cause is cleared, and the same command resumes.
        raise Stopped(f"the sandbox refused to start a judge for {ask_id}: {refused}")
    with sealed():
        return judged_request(context, line, recorder.calls, bundles, run, row, job, held, sent)


# ---------- the run ----------

def counts_of(outcomes):
    """The stage's counts: by version 6's rule, which counts, and under each version (`byRule`)."""
    def counted(rows):
        kinds = Counter(row["deviation"] for row in rows if row["outcome"] == "deviation")
        verdicts = Counter(row["verdict"] for row in rows if row["outcome"] == "verdict")
        return {"requests": len(rows), "holds": sum(1 for row in rows if row["outcome"] == "hold"),
                "verdicts": sum(verdicts.values()), "byVerdict": dict(sorted(verdicts.items())),
                "deviations": sum(kinds.values()), "deviationsByKind": dict(sorted(kinds.items()))}

    counts = counted([dict(outcome, deviation=outcome["deviation"] and outcome["deviation"]["kind"])
                      for outcome in outcomes])
    counts["byRule"] = {f"v{version}": counted([outcome["byRule"][f"v{version}"] for outcome in outcomes])
                        for version in CHECKS.RULE_VERSIONS}
    return counts


def finish(stage, inventory):
    """Once every request of the inventory has its outcome: `outcomes.jsonl`, one line a request in
    the inventory's order, and the record marked written with the counts."""
    outcomes = []
    lines = []
    for line in inventory:
        path = request_path(line["askId"], OUTCOME_FILE)
        outcome = json.loads(stage.read(path))
        outcomes.append(outcome)
        lines.append(json.dumps({"askId": line["askId"], "issueKey": line["issueKey"], "group": line["group"],
                                 "cases": line["cases"], "outcome": outcome["outcome"], "verdict": outcome["verdict"],
                                 "deviation": outcome["deviation"] and outcome["deviation"]["kind"],
                                 "byRule": {name: {key: one[key] for key in ("outcome", "verdict", "deviation")}
                                            for name, one in outcome["byRule"].items()},
                                 "outcomeSha256": stage.record["files"][path]}, sort_keys=True))
    text = ("\n".join(lines) + "\n").encode("utf-8") if lines else b""
    if not stage.has(OUTCOMES_FILE):
        stage.commit({OUTCOMES_FILE: text})
    elif stage.read(OUTCOMES_FILE) != text:
        raise CannotRun(f"{OUTCOMES_FILE} is not the outcomes this stage's files hold")
    stage.record.update({"state": "written", "finishedAt": utc_now(), "counts": counts_of(outcomes)})
    stage.save()
    return stage.record["counts"]


def _stop_on_signal(signum, frame):
    raise Stopped(f"the run received signal {signum}")


def bundles_link_refusal(work, out):
    """Why `work/bundles` cannot be made the link to `out/building`, or None: anything there but
    that one link (a directory, a file, a link elsewhere) is another run's, or no run's."""
    link = os.path.join(work, "bundles")
    target = os.path.join(out, BUILDING_DIRECTORY)
    if not os.path.lexists(link) or (os.path.islink(link) and os.readlink(link) == target):
        return None
    return (f"{link} is there and is no link to {target}: the frozen run_judges writes each bundle there, and "
            "this stage takes that path only as its own link into --out")


def link_bundles(work, out):
    """Makes `work/bundles`, where the frozen `run_judges` writes each bundle (`WORK/bundles/<ask
    id>/judge-N.md`, the path each prompt names), a link to `out/building`, so every byte S10's code
    writes lands under `--out`."""
    refusal = bundles_link_refusal(work, out)
    if refusal is not None:
        raise CannotRun(refusal)
    link = os.path.join(work, "bundles")
    os.makedirs(os.path.join(out, BUILDING_DIRECTORY), exist_ok=True)
    os.makedirs(work, exist_ok=True)
    if not os.path.lexists(link):
        os.symlink(os.path.join(out, BUILDING_DIRECTORY), link)


def unlink_bundles(work, out):
    """Removes the link `link_bundles` made, and whatever `out/building` still holds."""
    link = os.path.join(work, "bundles")
    if os.path.islink(link) and os.readlink(link) == os.path.join(out, BUILDING_DIRECTORY):
        os.remove(link)
    shutil.rmtree(os.path.join(out, BUILDING_DIRECTORY), ignore_errors=True)


def judge_all(context, stage, code, skill):
    """Every request of the inventory its outcome, in its order: a request a run started and did not
    finish is `started`, and every other is judged."""
    missing = {f"{CODE_DIRECTORY}/{name}": data for name, data in code.items()
               if not stage.has(f"{CODE_DIRECTORY}/{name}")}
    if missing:
        stage.commit(missing)
    context.shadow = load_shadow(os.path.join(stage.directory, CODE_DIRECTORY, "shadow.py"), context.pins,
                                 code["shadow.py"])
    place_skill(context.shadow, skill)
    link_bundles(context.shadow.WORK, stage.directory)
    for line in context.inputs.inventory:
        path = request_path(line["askId"], OUTCOME_FILE)
        if stage.has(path):
            continue
        if stage.has(request_path(line["askId"], STARTED_FILE)):
            outcome = deviation(line, "started", "a run started the request and stopped before its outcome; "
                                                 "it is never judged again")
        else:
            outcome = judge(context, line)
        stage.commit({path: json_bytes(outcome)})
        print(f"  {line['askId']}: {outcome['outcome']} {outcome['verdict'] or outcome['deviation']['kind']}")


def run(flags, argv, environ, pins, admit, runner_of):
    admission = admit(argv, environ)
    for name in [name for name in os.environ if name.startswith("DISPATCH_")]:
        del os.environ[name]
    code = frozen_files(flags.code, pins)
    skill = skill_files(flags.skill, flags.never_decide, pins)
    image = flags.image.strip()
    refusal = bundles_link_refusal(pins.work, admission["out"])
    if refusal is not None:
        raise CannotRun(refusal)
    runner = runner_of(image, flags.service_account, environ)
    chain = {"claimRun": admission["claimRun"], "reference": admission["reference"],
             "runDigest": admission["runDigest"], "claimDirectory": admission["claimDirectory"],
             "inputs": {"directory": admission["inputs"]["directory"], "stageSha256": admission["inputs"]["stageSha256"],
                        "files": admission["inputs"]["files"]},
             "frozen": {"codeSha256": pins.code_sha256, "files": {name: sha256_of(data) for name, data in code.items()},
                        "promptSha256": pins.prompt_sha256, "bundleCodeSha256": pins.bundle_code_sha256,
                        "skillSha256": pins.skill_sha256, "asked": pins.asked, "model": pins.model},
             "image": image, "serviceAccount": flags.service_account}
    stage = Stage.open(admission["out"], chain)
    inputs = Inputs(admission)
    print(f"judged            {stage.directory}")
    print(f"claim run         {chain['claimRun']}, under {chain['reference']}")
    print(f"requests          {len(inputs.inventory)} in the inventory")
    if stage.record["state"] == "written":
        print(f"written before    {stage.record['counts']}")
        return 0
    context = Context(shadow=None, pins=pins, inputs=inputs, stage=stage, runner=runner, image=image)
    stopped = None
    try:
        judge_all(context, stage, code, skill)
    except (Stopped, JOB.KubeUnavailable, KeyboardInterrupt) as error:
        stopped = str(error) or type(error).__name__
    finally:
        unlink_bundles(pins.work, stage.directory)
    if stopped is not None:
        # Nothing more is written but the stop itself: the stage stays started, and the same
        # command resumes it.
        stage.record["stops"] = [*stage.record.get("stops", []), {"at": utc_now(), "reason": stopped}]
        stage.save()
        done = sum(1 for line in inputs.inventory if stage.has(request_path(line["askId"], OUTCOME_FILE)))
        print(f"stopped           {stopped}; {done} of {len(inputs.inventory)} requests have their outcome, "
              "and the same command resumes the rest", file=sys.stderr)
        return 2
    counts = finish(stage, inputs.inventory)
    print(f"written           {counts}")
    return 0


def main(argv, *, pins=FROZEN, admit=admit_with_bun, runner_of=JOB.KubeRunner, environ=None):
    """The command: its flags, the run, and the exit status it takes. SIGTERM stops the run as an
    interrupt does."""
    environ = dict(os.environ if environ is None else environ)
    previous = signal.signal(signal.SIGTERM, _stop_on_signal)
    try:
        flags = flags_of(argv)
        return run(flags, argv, environ, pins, admit, runner_of)
    except CannotRun as error:
        print(f"S10JudgeError: {error}", file=sys.stderr)
        return 2
    except (Stopped, JOB.KubeUnavailable, KeyboardInterrupt) as error:
        print(f"S10JudgeError: the run stopped before it opened its stage: {error or type(error).__name__}",
              file=sys.stderr)
        return 2
    finally:
        signal.signal(signal.SIGTERM, previous)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
