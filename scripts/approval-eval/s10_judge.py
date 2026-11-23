"""The claim run's fourth stage: S10, frozen, judges each request of the inventory (point 14 (b)).

After the build (`build-corpus.ts`), the draw (`draw.ts`) and the inputs (`s10-inputs.ts`), and
before the analysis (`analyze.ts --claim-s10`), under the registered read and the claim run:

    python3 scripts/approval-eval/s10_judge.py --inputs "$CLAIM/s10-inputs" \\
      --out "$CLAIM/s10-judged" --code <code-txt file> --skill <SKILL.md> \\
      --never-decide <references/never-decide.md> --image <image>@sha256:<digest>

The modules. This file is the driver: the admission, the inputs and each request's judging. The
frozen code is loaded by `s10_frozen.py`, the stage on disk is `s10_store.py`, the sandbox's Job
`s10_job.py`, the program the sandbox runs `s10_sandbox.py`, and point 5's checks of the
transcripts `s10_transcripts.py`. `s10-judged-io.ts` reads the stage's files back for the
analysis, and `s10-judged-check.ts` reads each outcome back with it before it is committed.

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

The frozen code. `--code` is the code-txt file of point 2, each of its five files and the skill's
two checked against point 2, and its `shadow.py` run from the code-txt's bytes with every way out
of the process refused while S10's code runs (`s10_frozen.py` states each check). Every
`DISPATCH_` variable leaves this process's environment once the admission has run. Of S10's code
the reads of point 3 are replaced, and nothing else: its `api`, through which its `issue`,
`search`, `doc_version` and the ask reads of its `neighbours` go, by a reader over the request's
inputs (`RequestReader`), and its `reserved_neighbour` by the reserve's decision the inputs stage
recorded (point 4 (d)).

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
  answer is compared, point 5's checks in the order `DEVIATION_KINDS` weighs them: the sandbox's
  checks made again here from its record, and a result whose files can be read (`sandbox`); the
  manifests of the trees (`manifest`); no kept file holding the gateway key's bytes (`key`); and
  the transcripts, as `s10_transcripts.py` states each version of the rule (`session`,
  `transcript`, `tool-call`, `model`). Then five valid answers (`answers`); the verdict is S10's
  `aggregate` over them, through its `finish_judgement`, and it must be the verdict point 2's rule
  gives (`verdict`). Each outcome also records its outcome under each version of point 5's rule
  (`byRule`, one row of `s10_transcripts.py`'s `RULES` a version, with the calls and the manifest
  problem that version makes a deviation), so the analysis reports what changes under each with
  no further judging; version 6's is the one that counts.
- Before an outcome is committed, `s10-judged-check.ts` reads it back with the analysis's own
  parser (`judgedOutcomeOf`, `read_back`). An outcome it refuses, whatever S10's code returned in
  it, is kept beside its place as `unread-outcome.json`, and the request is a deviation of its own
  (`shape`): so the stage stays one the analysis reads, and the analysis's parser never changes
  after the reserve has been read.

Stops. A request is started once its `started.json` is kept, before S10's code runs on it, and a
request started and not finished is a deviation (`started`) that no later run judges again (point
6): the next run records it so before it judges anything else. A stop records itself and leaves
the stage `started` for the same command to resume: a SIGTERM or an interrupt; a sandbox that
refuses to start a judge, since every later request would meet the same sandbox until its cause is
cleared; a failed preflight; a Kubernetes API call that still fails once it has been tried again
(`s10_job.py`), which is no fault of the request's; and a read-back that Bun cannot run. It writes
nothing more for the request in flight, but for one case: a stop that comes while an outcome built
whole is read back (a read-back that cannot run, or a signal then) keeps that outcome as
`pending-outcome.json`, and the next run reads it back and commits it, judging nothing again. That
request was judged once and completed; only the check before its commit did not run, so it is not
the `started` deviation point 6 makes of a request started and not completed. A Job that ran and
gave no readable result is that request's own `sandbox` deviation.
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

import argparse  # noqa: E402
import copy  # noqa: E402
import json  # noqa: E402
import lzma  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
import shutil  # noqa: E402
import signal  # noqa: E402
import subprocess  # noqa: E402
from collections import Counter  # noqa: E402
from dataclasses import dataclass  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import s10_job as JOB  # noqa: E402
import s10_sandbox as SANDBOX  # noqa: E402
import s10_transcripts as CHECKS  # noqa: E402
from s10_frozen import (  # noqa: E402
    FROZEN,
    InputsGap,
    frozen_files,
    load_shadow,
    place_skill,
    raised_by_frozen_code,
    sealed,
    skill_files,
)
from s10_store import (  # noqa: E402
    BUILDING_DIRECTORY,
    CODE_DIRECTORY,
    OUTCOME_FILE,
    OUTCOMES_FILE,
    PENDING_FILE,
    PREFLIGHT_DIRECTORY,
    STARTED_FILE,
    STARTED_REASON,
    UNREAD_FILE,
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
                   "model", "answers", "verdict", "shape", "started")

# The Bun that runs the admission and the read-back, by its path in the session image, never one
# `PATH` finds.
BUN = "/usr/local/bin/bun"


class Stopped(Exception):
    """The run stops, and the same command resumes the stage. It writes nothing more for the
    request in flight, but for an outcome built whole whose read-back stopped: `judge_all` keeps
    that one as `PENDING_FILE`."""


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


def read_back_with_bun(text, path):
    """Why `s10-judged-io.ts`'s own `judgedOutcomeOf` refuses `text`, one outcome's file as it
    would be committed at `path`, or None where it reads it back (`s10-judged-check.ts`). A Bun
    that cannot run the check stops the run."""
    entry = os.path.join(HERE, "s10-judged-check.ts")
    env = {name: value for name, value in os.environ.items() if not name.startswith(("APPROVAL_EVAL_", "DISPATCH_"))}
    try:
        completed = subprocess.run([BUN, entry, path], input=text, env=env, capture_output=True, text=True)
        if completed.returncode != 0:
            raise ValueError(completed.stderr.strip() or f"exit {completed.returncode}")
        return json.loads(completed.stdout.strip().splitlines()[-1])["refused"]
    except (OSError, ValueError, IndexError, KeyError, TypeError) as error:
        raise Stopped(f"{entry} could not read back {path}: {error}") from error


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
    sandbox, the image the sandbox must run, the read-back each outcome passes before it is
    committed (`read_back_with_bun`), and whether this run's preflight passed."""

    shadow: object
    pins: object
    inputs: Inputs
    stage: Stage
    runner: object
    image: str
    read_back: object
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


def by_rule_of(problems, by_version, verdict):
    """A judged request's outcome under each version of point 5's rule, over the same transcripts
    and manifests: every problem but the tool calls and the manifest is the same under each;
    `by_version` holds, by version, the calls that version makes a deviation (`tool-call`) and the
    problem it finds in the manifests (`manifest`); `verdict` is the verdict five valid answers
    give, or None. Each version's outcome, its first deviation kind, and those calls."""
    by_rule = {}
    for version in CHECKS.RULE_VERSIONS:
        own = by_version[version]
        kinds = [kind for kind in DEVIATION_KINDS if (own[kind] if kind in own else problems[kind])]
        outcome = {"outcome": "deviation", "verdict": None, "deviation": kinds[0]} if kinds else \
            {"outcome": "verdict", "verdict": verdict, "deviation": None}
        outcome["toolCallDeviations"] = [{key: one[key] for key in ("judge", "callId", "tools", "reason")}
                                         for one in own["tool-call"]]
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
        same_size = record.get("sameSize") if isinstance(record.get("sameSize"), dict) else None
        refusal = (SANDBOX.mount_refusal(record.get("mounts") or [], layout)
                   or ("the sandbox's listing cannot be read" if listing is None else None)
                   or SANDBOX.listing_refusal(listing, payload, layout, same_size))
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
    by_version = {version: {"manifest": [], "tool-call": []} for version in CHECKS.RULE_VERSIONS}
    for version, rule in CHECKS.RULES.items():
        if rule.manifests is not None:
            manifest = SANDBOX.manifest_problem(record.get("manifests"), line["askId"], trees=rule.manifests)
            if manifest is not None:
                by_version[version]["manifest"].append(manifest)
    problems["manifest"] = by_version[CHECKS.COUNTING_RULE]["manifest"]
    held = sorted(set(record.get("keyFound") or []) | set(key_held))
    if held:
        problems["key"].append(f"the gateway key's bytes were in {held}; each is kept with them replaced")
    judges = []
    answers = [None] * JUDGES
    if "omp" not in record:
        problems["transcript"].append("the sandbox started no judge, so no judge has a transcript")
    else:
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
                by_version[version]["tool-call"].extend(CHECKS.tool_call_problems(call["label"], made, scope, version))
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
        problems["tool-call"] = [one["problem"] for one in by_version[CHECKS.COUNTING_RULE]["tool-call"]]
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
    by_rule = by_rule_of(problems, by_version, judgement and judgement.get("verdict"))
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
            held = judgement if isinstance(judgement, dict) else {}
            return outcome_of(line, "hold", verdict=held.get("verdict"), judgement=judgement,
                              judged_at=held.get("judged_at"))
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


def read_back(context, line, outcome):
    """The outcome a request is committed with, and the files that commit writes: `outcome` where
    `s10-judged-io.ts`'s own parser reads it back (`context.read_back`); else a `shape` deviation
    in its place, and the outcome it refused kept beside it (`UNREAD_FILE`), so the stage stays one
    the analysis reads whatever S10's code returned."""
    path = request_path(line["askId"], OUTCOME_FILE)
    try:
        data = json_bytes(outcome)
    except (TypeError, ValueError) as error:
        data, refused = repr(outcome).encode("utf-8"), f"the outcome is no JSON: {error}"
    else:
        refused = context.read_back(data.decode("utf-8"), path)
    if refused is None:
        return outcome, {path: data}
    judged_at = outcome.get("judgedAt") if isinstance(outcome.get("judgedAt"), str) else None
    shaped = deviation(line, "shape", f"the analysis's parser refuses the outcome as built: {refused}",
                       judged_at=judged_at)
    again = context.read_back(json_bytes(shaped).decode("utf-8"), path)
    if again is not None:
        raise RuntimeError(f"the analysis's parser refuses the shape deviation of {line['askId']} too: {again}")
    return shaped, {path: json_bytes(shaped), request_path(line["askId"], UNREAD_FILE): data}


def judge_all(context, stage, code, skill):
    """Every request of the inventory its outcome, in its order: a request whose outcome an earlier
    run built and could not read back is that outcome (`PENDING_FILE`), refused unless it names that
    request as the inventory does; a request a run started and did not finish is `started`; and
    every other is judged. Each outcome is read back before it is committed; a read-back that does
    not run keeps the outcome as pending and stops the run."""
    missing = {f"{CODE_DIRECTORY}/{name}": data for name, data in code.items()
               if not stage.has(f"{CODE_DIRECTORY}/{name}")}
    if missing:
        stage.commit(missing)
    context.shadow = load_shadow(os.path.join(stage.directory, CODE_DIRECTORY, "shadow.py"), context.pins,
                                 code["shadow.py"])
    place_skill(context.shadow, skill)
    link_bundles(context.shadow.WORK, stage.directory)
    for line in context.inputs.inventory:
        if stage.has(request_path(line["askId"], OUTCOME_FILE)):
            continue
        pending = request_path(line["askId"], PENDING_FILE)
        if stage.has(pending):
            outcome = json.loads(stage.read(pending))
            # No outcome is committed in the place of another request's.
            held = outcome if isinstance(outcome, dict) else {}
            differ = [f"its {key} is {held.get(key)!r}, and the inventory's {line[key]!r}"
                      for key in ("askId", "issueKey", "group", "cases") if held.get(key) != line[key]]
            if differ:
                raise CannotRun(f"{pending} is no outcome of the request it is kept for: {'; '.join(differ)}")
        elif stage.has(request_path(line["askId"], STARTED_FILE)):
            outcome = deviation(line, "started", STARTED_REASON)
        else:
            outcome = judge(context, line)
        try:
            outcome, files = read_back(context, line, outcome)
        except (Stopped, KeyboardInterrupt):
            # The outcome was built whole, and only the check before its commit did not run: it is
            # kept, so the next run commits that outcome rather than recording the request started.
            try:
                data = json_bytes(outcome)
            except (TypeError, ValueError):
                data = None
            if data is not None and not stage.has(pending):
                stage.commit({pending: data})
            raise
        stage.commit(files)
        print(f"  {line['askId']}: {outcome['outcome']} {outcome['verdict'] or outcome['deviation']['kind']}")


def run(flags, argv, environ, pins, admit, runner_of, read_back_of):
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
    context = Context(shadow=None, pins=pins, inputs=inputs, stage=stage, runner=runner, image=image,
                      read_back=read_back_of)
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


def main(argv, *, pins=FROZEN, admit=admit_with_bun, runner_of=JOB.KubeRunner, read_back_of=read_back_with_bun,
         environ=None):
    """The command: its flags, the run, and the exit status it takes. SIGTERM stops the run as an
    interrupt does."""
    environ = dict(os.environ if environ is None else environ)
    previous = signal.signal(signal.SIGTERM, _stop_on_signal)
    try:
        flags = flags_of(argv)
        return run(flags, argv, environ, pins, admit, runner_of, read_back_of)
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
