"""Tests of the claim's judging stage: the driver (`s10_judge.py`), its store (`s10_store.py`), its
sandbox Job (`s10_job.py`), point 5's checks (`s10_transcripts.py`) and the program the sandbox
runs (`s10_sandbox.py`). `s10-judge.test.ts` runs them with `python3 -m unittest`.

The frozen code is a stand-in of the same shape, `fixtures/s10-code.txt`, with its own sha256: a
code-txt whose `shadow.py` has the names the driver runs and replaces, and none of the real code's
text, which this public repository may not hold. Its pins are taken from the fixture itself
(`pins_for`). The admission is handed in, as `s10-judge-admit.ts`'s own tests cover it, and the
sandbox is a fake that writes the files a real sandbox's result holds: the record, the listing,
the session's transcript, each judge's transcript in omp's own entries, and the answers, with a
layout whose emptyDir is the test's own root, where the bundles and the calls are; and its
preflight.

Under `s10-judge.test.ts`, three variables join this suite to the TypeScript side:
`S10_INPUTS_FIXTURE` names an inputs stage that test wrote and read back through
`s10-inputs-io.ts`'s parsers, with the admission `s10-judge-admit.ts` gave over it, which a test
here judges; `S10_JUDGED_KEEP` names a directory where every inputs stage written here
(`admitted`) and every stage a run here judges whole (`keep_for_typescript`) is copied, with the
constants both sides hold equal, which that test reads back through `s10-inputs-io.ts` and
`s10-judged-io.ts`: so every inputs stage judged here is one the inputs stage writes, and every
kind of outcome this stage writes is parsed there; and `S10_BUN` names the Bun the read-back test
runs `s10-judged-check.ts` with.
"""

import sys

sys.dont_write_bytecode = True

import contextlib  # noqa: E402
import hashlib  # noqa: E402
import http.client  # noqa: E402
import importlib.util  # noqa: E402
import inspect  # noqa: E402
import io  # noqa: E402
import json  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
import shutil  # noqa: E402
import signal  # noqa: E402
import socket  # noqa: E402
import subprocess  # noqa: E402
import tempfile  # noqa: E402
import threading  # noqa: E402
import types  # noqa: E402
import unittest  # noqa: E402
from collections import Counter  # noqa: E402
from unittest import mock  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import s10_frozen as F  # noqa: E402
import s10_job as JOB  # noqa: E402
import s10_judge as J  # noqa: E402
import s10_sandbox as SANDBOX  # noqa: E402
import s10_store as STORE  # noqa: E402
import s10_transcripts as CHECKS  # noqa: E402

CODE = os.path.join(HERE, "fixtures", "s10-code.txt")
PERSON = "person@example.test"
SKILL = b"# The skill, as the tests give it\n\nJudge the request.\n"
NEVER = b"# What the agent never decides, as the tests give it\n\nNothing here.\n"
IMAGE = "registry.example.test/sandbox@sha256:" + "a" * 64
HARNESS = "/opt/legion/pi-envoy/dist/skills/approval-review"
OPENED = "2026-11-17T10:00:00.000001Z"
EARLIER = "2026-11-16T09:00:00.000001Z"
KEY = b"gateway-key-of-the-tests-0123456789"
# The inputs stage's version, which `s10-inputs-io.ts` gives as `INPUTS_VERSION`; `s10-judge.test.ts`
# compares the two, and reads every inputs stage written here with that module's parsers.
INPUTS_VERSION = 4


def sha(data):
    return hashlib.sha256(data if isinstance(data, bytes) else data.encode("utf-8")).hexdigest()


def put(path, data):
    """Writes a file whole, from text or bytes."""
    with open(path, "wb") as handle:
        handle.write(data if isinstance(data, bytes) else data.encode("utf-8"))


def got(path):
    with open(path, "rb") as handle:
        return handle.read()


def ask_id(tail):
    return f"00000000-0000-4000-8000-{tail:0>12}"


def code_files(path):
    text = got(path).split(b"\n", 2)[2]
    pieces = re.split(rb"^=== FILE: (\S+) ===\n", text, flags=re.M)
    return text, dict(zip((name.decode() for name in pieces[1::2]), pieces[2::2]))


def pins_for(path, work):
    """The pins of a code-txt, read from the file itself, as point 2 reads the real one's."""
    text, files = code_files(path)
    with tempfile.TemporaryDirectory() as scratch:
        source = os.path.join(scratch, "shadow.py")
        put(source, files["shadow.py"])
        os.environ.update(F.UNREACHABLE)
        try:
            spec = importlib.util.spec_from_file_location("s10_pins_shadow", source)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
        finally:
            for name in F.UNREACHABLE:
                os.environ.pop(name, None)
        prompt, bundle = sha(module.JUDGE_PROMPT), sha(inspect.getsource(module.build_bundle))
    return F.Pins(code_sha256=sha(text), files={name: sha(data)[:8] for name, data in files.items()},
                  prompt_sha256=prompt, bundle_code_sha256=bundle, skill_sha256=dict(module.SKILL_SHA),
                  asked=module.ASKED, model=module.MODEL, work=work)


# ---------- the inputs stage, in the shapes s10-inputs-io.ts reads back ----------

def candidate(tail, *, issue="ACME-9", at=EARLIER, kind="approval", document="doc-earlier"):
    return {"asOfEventId": 40, "issue": {"key": issue, "title": "Earlier work"},
            "ask": {"id": ask_id(tail), "kind": kind, "state": "answered", "issue_key": issue,
                    "question": "Approve the earlier spec?", "options": [], "multiple": False,
                    "approval": {"artifact_id": document, "name": "earlier.md", "version": 1},
                    "answer": {"user": PERSON, "selected": ["Approve"], "text": "Fine.", "at": at},
                    "created_at": EARLIER}}


def request_of(tail, issue_key, *, hold=False, fact=None, pages=None, candidates=None, ask_changes=None):
    """One request file, in the inputs stage's shape (`s10-inputs-io.ts`'s `RequestInputs`), and the
    page records its search uses: a search, candidates and counts unless its fact holds, as the
    inputs stage writes one. `fact` stands in for the fact the stage would compute from its ask and
    issue, for a row where S10's own `legion_fact` must read them otherwise."""
    ask = {"id": ask_id(tail), "kind": "approval", "state": "open", "issue_key": issue_key,
           "question": f"Approve the spec of {issue_key}?",
           "approval": {"artifact_id": f"doc-{tail}", "name": "spec.md", "version": 2},
           "approval_artifact": {"primary": hold}, "created_at": OPENED, "answer": None, **(ask_changes or {})}
    issue = {"key": issue_key, "title": f"The work of {issue_key}", "status": "backlog" if hold else "todo",
             "labels": ["legion"] if hold else [], "parent": None, "primary_artifact_id": None}
    markdown = f"# The spec of {issue_key}\n"
    computed = {"holds": hold, "legion": hold, "primary": hold, "root": True, "status": issue["status"]}
    request = {"version": INPUTS_VERSION, "askId": ask_id(tail), "issueKey": issue_key, "group": "report", "cases": 1,
               "judged": {"eventId": 50, "type": "ask.opened", "createdAt": OPENED}, "ask": ask, "issue": issue,
               "issueAsOfEventId": 10,
               "documents": [{"artifact_id": f"doc-{tail}", "name": "spec.md", "version": 2, "markdown": markdown,
                              "sha256": sha(markdown)}],
               "fact": fact or computed, "factHold": (fact or computed)["holds"],
               "search": None, "candidates": None, "counts": None, "dropScanThrough": None}
    records = {}
    if not request["factHold"]:
        if candidates is None:
            candidates = {ask_id("c1"): candidate("c1"), ask_id("c2"): {"held": True, "reserve": "held"},
                          ask_id("c3"): {"dropped": "later-event", "seen": "log"}}
        hits = pages if pages is not None else [
            [{"id": ask_id("c2"), "kind": "ask", "rank": 0.9}, {"id": ask_id("c3"), "kind": "ask", "rank": 0.8},
             {"id": "ACME-9", "kind": "issue", "rank": 0.7}, {"id": ask_id("c1"), "kind": "ask", "rank": 0.6}]]
        queries = {}
        for name, query in (("title", issue["title"]), ("question", ask.get("question") or "")):
            listed = []
            for offset, page_hits in zip(range(0, 50 * len(hits), 50), hits):
                record = {"hits": page_hits, "reads": [{"query": query, "offset": offset, "readAt": OPENED}]}
                record["sha256"] = sha(f"{query}@{offset}")
                records[record["sha256"]] = record
                listed.append({"sha256": record["sha256"], "offset": offset})
            queries[name] = {"query": query, "pages": listed,
                             "hits": [{"id": hit["id"], "rank": hit["rank"], "page": listed[0]["sha256"], "position": 0}
                                      for hit in hits[0] if hit["kind"] == "ask"]}
        request.update(search=queries, candidates=candidates, dropScanThrough=60,
                       counts={"askHits": 3, "candidates": 3, "held": 1, "reserveErrors": 0, "kept": 1,
                               "droppedLaterEvent": {"log": 1, "no-event-through-pin": 0, "after-pin": 0}})
    return request, records


def ready_line(request):
    return {"askId": request["askId"], "issueKey": request["issueKey"], "group": "report", "cases": 1,
            "judged": request["judged"], "state": "ready", "reason": None, "requestSha256": None,
            "counts": request["counts"], "dropScanThrough": request["dropScanThrough"]}


def deviation_line(tail, reason):
    return {"askId": ask_id(tail), "issueKey": "ACME-5", "group": "report", "cases": 2, "judged": None,
            "state": "deviation", "reason": reason, "requestSha256": None, "counts": None, "dropScanThrough": None}


def write_inputs(directory, out, entries):
    """An inputs stage of `entries` (a request and its pages, or an inventory line), and the
    admission `s10-judge-admit.ts` would give a run over it."""
    files = {}

    def keep(name, data):
        path = os.path.join(directory, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        put(path, data)
        files[name] = sha(data)

    lines = []
    for entry in entries:
        if "state" in entry:
            lines.append(entry)
            continue
        request, records = entry
        for key, record in records.items():
            keep(f"pages/{key}.json", json.dumps(record).encode())
        data = json.dumps(request, indent=2).encode()
        keep(f"requests/{request['askId']}.json", data)
        lines.append(dict(ready_line(request), requestSha256=sha(data)))
    keep("inventory.jsonl", "".join(json.dumps(line) + "\n" for line in lines).encode())
    return {"reference": "registration-1", "claimRun": "start-1", "runDigest": "d" * 64,
            "claimDirectory": os.path.dirname(out), "out": out,
            "inputs": {"directory": directory, "stageSha256": "e" * 64, "files": files}}


# ---------- a fake sandbox, writing what a real one's result holds ----------

def answer(verdict="approve", kind="none"):
    return {"never_decide": kind, "never_decide_line": None, "addresses_reviewer": False,
            "injected_instruction": None, "verdict": verdict, "reason": "One sentence."}


def entries_text(entries):
    return ("\n".join(json.dumps(entry) for entry in entries) + "\n").encode()


def tool_entries(calls, upstream=None):
    """omp's entries of each tool call: the assistant's call, its execution's start, its result. A
    call is `(name, arguments, details)`, or with a fourth part, the text its result answered."""
    entries = []
    for number, call in enumerate(calls):
        name, arguments, details = call[:3]
        text = call[3] if len(call) > 3 else None
        call_id = f"toolu_{number}"
        entries.append({"type": "message", "message": {
            "role": "assistant", "provider": "anthropic", "model": "claude-opus-5-5", "upstreamModel": upstream,
            "content": [{"type": "thinking", "thinking": "..."},
                        {"type": "toolCall", "id": call_id, "name": name, "arguments": arguments}]}})
        entries.append({"type": "custom", "customType": "tool_execution_start",
                        "data": json.dumps({"toolCallId": call_id, "toolName": name, "args": arguments})})
        content = [] if text is None else [{"type": "text", "text": text}]
        entries.append({"type": "message", "message": {"role": "toolResult", "toolCallId": call_id, "toolName": name,
                                                       "isError": False, "details": details, "content": content}})
    return entries


def listing_answer(lines):
    """A `read`'s answer that lists a directory as omp 18.16.0 prints one: its root `.`, then each
    line as given (indentation, `- `, name, and a file's size and age, or none where the age is
    empty)."""
    return "\n".join([".", *lines])


# The manifests a fake sandbox records unless a test hands it others: the ones its request requires.
REQUIRED = "required"


class FakeSandbox:
    """The sandbox, as a fake: `judge_tools`, `answers`, `model_changes`, `served`, `resolved`,
    `top_calls`, `record`, `manifests`, `drop`, `failure`, `listing`, `copied`, `files` and `key`
    change what it gives back or holds; `preflight_record` and `preflight_failure` change its
    preflight. `copied` names a path outside the work tree that holds the first bundle's bytes.
    Its layout puts the emptyDir at the test's own root, where the driver's bundles and calls are.
    Each run records where the driver's `WORK/bundles` led while it ran; `during` is called in the
    middle of a run."""

    def __init__(self, test, **changes):
        self.test = test
        self.runs = []
        self.preflights = []
        self.judge_tools = None
        self.answers = None
        self.model_changes = None
        self.served = {}
        self.resolved = {}
        self.top_calls = None
        self.manifests = REQUIRED
        self.listing = []
        self.record = {}
        self.preflight_record = {}
        self.preflight_listing = []
        self.preflight_image_id = IMAGE
        self.preflight_failure = None
        self.drop = set()
        self.failure = None
        self.image_id = IMAGE
        self.raise_on = None
        self.during = None
        self.files = {}
        self.copied = None
        self.key = None
        for name, value in changes.items():
            setattr(self, name, value)

    def layout(self):
        return {"key": "/run/model-gateway-key", "models": SANDBOX.MODELS_FILE, "overlay": SANDBOX.OVERLAY_FILE,
                "work": self.test.root}

    def checked_record(self, files, preflight):
        """The record and listing of a sandbox whose every check passed over the files sent: the
        skill's files and directories, each file sent and the directories of the work tree above
        them, and the test's own lines."""
        layout = self.layout()
        directories = {HARNESS, HARNESS + "/references"}
        for path in files:
            parent = os.path.dirname(path)
            while STORE.under(parent, self.test.work):
                directories.add(parent)
                parent = os.path.dirname(parent)
        bundles = sorted(path for path in files if path.endswith(".md"))
        copies = [f"f 644 {len(files[bundles[0]])} {self.copied}"] if self.copied and bundles else []
        listing = sorted([f"f 644 10 {HARNESS}/{name}" for name in SANDBOX.SKILL_FILES]
                         + [f"d 755 4096 {path}" for path in directories]
                         + [f"f 644 {len(data)} {path}" for path, data in files.items()]
                         + copies + (self.preflight_listing if preflight else self.listing))
        record = {"refused": None, "preflight": preflight, "layout": layout, "payload": {
                      path: sha(data) for path, data in files.items()},
                  "environment": ["HOME", "PATH", "S10_SANDBOX", "S10_SANDBOX_MODE"],
                  "mounts": ["/", "/proc", layout["key"], layout["models"], layout["overlay"], layout["work"]],
                  "listing": {"lines": len(listing), "sha256": sha("\n".join(listing) + "\n")},
                  "sameSize": {self.copied: sha(files[bundles[0]])} if copies else {},
                  "runtime": dict(SANDBOX.RUNTIME, piConfigFiles=SANDBOX.OVERLAY_FILE),
                  "metadata": [{"url": url, "connected": True, "status": 403} for url in SANDBOX.METADATA_REQUESTS],
                  "keyFound": []}
        return record, listing

    def preflight(self):
        self.preflights.append({"link": os.readlink(os.path.join(self.test.work, "bundles"))
                                if os.path.islink(os.path.join(self.test.work, "bundles")) else None})
        if self.preflight_failure is not None:
            return JOB.SandboxRun(job="fake-preflight", failure=self.preflight_failure)
        record, listing = self.checked_record({}, True)
        record.update(self.preflight_record)
        return JOB.SandboxRun(job=f"fake-preflight-{len(self.preflights)}",
                              files={"listing.txt": ("\n".join(listing) + "\n").encode(),
                                     "record.json": json.dumps(record).encode()},
                              facts={"pod": "fake-preflight-pod", "imageId": self.preflight_image_id})

    def run(self, ask, files, extra_env=None):
        calls = json.loads(files[SANDBOX.CALLS_FILE])
        kept = os.path.join(self.test.out, "requests", ask)
        link = os.path.join(self.test.work, "bundles")
        self.runs.append({"ask": ask, "files": files, "calls": calls,
                          "kept": {name: got(os.path.join(kept, name)) for name in sorted(os.listdir(kept))},
                          "link": os.readlink(link) if os.path.islink(link) else None})
        if self.during is not None:
            self.during()
        if self.raise_on is not None:
            raise self.raise_on
        if self.failure is not None:
            return JOB.SandboxRun(job="fake-job", failure=self.failure)
        bundles = sorted(path for path in files if path.endswith(".md"))
        result = {"sessions/top.jsonl": entries_text(
            [{"type": "session", "cwd": SANDBOX.WORK},
             {"type": "model_change", "model": "anthropic/claude-opus-5-5", "resolvedModelIsFallback": False},
             *tool_entries(self.top_calls or [("eval", {"language": "py", "code": SANDBOX.CELL}, None)])])}
        results = []
        for index, call in enumerate(calls):
            agent = f"judge{index + 1}"
            tools = (self.judge_tools or (lambda index, bundle: [
                ("read", {"path": bundle}, {"meta": {}}),
                ("read", {"path": "skill://approval-review"}, {"resolvedPath": HARNESS + "/SKILL.md"}),
                ("yield", {"data": json.dumps(answer())}, None)]))(index, bundles[index])
            changes = (self.model_changes or (lambda index: [
                {"type": "model_change", "model": "anthropic/claude-opus-5-5", "resolvedModelIsFallback": False}]))(index)
            entries = [{"type": "session", "cwd": SANDBOX.WORK}, *changes,
                       {"type": "session_init", "agent": call["agent"], "task": "Complete assignment thoroughly:\n\n"
                        + call["prompt"], "resolvedModel": self.resolved.get(index, call["model"]),
                        "outputSchema": call["schema"]},
                       {"type": "message", "message": {"role": "user", "content": [{"type": "text", "text": "..."}]}},
                       *tool_entries(tools, self.served.get(index))]
            if index not in self.drop:
                result[f"sessions/top/{agent}.jsonl"] = entries_text(entries)
            chosen = (self.answers or (lambda index: answer()))(index)
            results.append({"id": agent, "result": chosen})
        result["results.json"] = json.dumps(results).encode()
        record, listing = self.checked_record(files, False)
        result["listing.txt"] = ("\n".join(listing) + "\n").encode()
        result.update(self.files)
        manifests = self.manifests
        if manifests == REQUIRED:
            wanted = {"work": SANDBOX.work_manifest(ask), "skill": SANDBOX.SKILL_MANIFEST}
            manifests = {"before": wanted, "after": wanted}
        record.update({"manifests": manifests, "omp": {"returncode": 0}, **self.record})
        result["record.json"] = json.dumps(record).encode()
        return JOB.SandboxRun(job=f"fake-job-{len(self.runs)}", files=result,
                              facts={"pod": "fake-pod", "imageId": self.image_id})


class Killed(BaseException):
    """A kill the driver cannot catch, as the pod's end would be."""


class Base(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="s10-judge-")
        self.work = os.path.join(self.root, "shadow")
        self.inputs = os.path.join(self.root, "s10-inputs")
        self.out = os.path.join(self.root, "s10-judged")
        self.saved = os.environ.get("S10_TEST_WORK")
        os.environ["S10_TEST_WORK"] = self.work
        self.skill = os.path.join(self.root, "skill", "SKILL.md")
        self.never = os.path.join(self.root, "skill", "references", "never-decide.md")
        os.makedirs(os.path.dirname(self.never))
        put(self.skill, SKILL)
        put(self.never, NEVER)
        self.pins = pins_for(CODE, self.work)
        # The calls go beside the bundles, under the root the fake sandbox's layout names its emptyDir.
        calls = mock.patch.object(SANDBOX, "CALLS_FILE", os.path.join(self.root, "s10", "calls.json"))
        calls.start()
        self.addCleanup(calls.stop)

    def tearDown(self):
        if self.saved is None:
            os.environ.pop("S10_TEST_WORK", None)
        else:
            os.environ["S10_TEST_WORK"] = self.saved
        shutil.rmtree(self.root, ignore_errors=True)

    def argv(self, code=CODE, image=IMAGE, more=()):
        return ["--inputs", self.inputs, "--out", self.out, "--code", code, "--skill", self.skill,
                "--never-decide", self.never, "--image", image, *more]

    def judge(self, admission, sandbox, pins=None, code=CODE, more=(), read_back_of=None):
        """One run of the driver over `admission` and `sandbox`: its exit status. Each outcome is
        read back by `read_back_of`, by default one that refuses none; `s10-judge.test.ts` reads
        every stage a run here judges whole with the analysis's own parser after."""
        stdout, stderr = io.StringIO(), io.StringIO()
        self.runners = []

        def runner_of(image, account, environ):
            self.runners.append((image, account))
            return sandbox

        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            status = J.main(self.argv(code, more=more), pins=pins or self.pins, admit=lambda argv, environ: admission,
                            runner_of=runner_of, read_back_of=read_back_of or (lambda text, path: None))
        self.stderr = stderr.getvalue()
        if status == 0:
            self.keep_for_typescript()
        return status

    def keep_for_typescript(self):
        """Where `s10-judge.test.ts` asks (`S10_JUDGED_KEEP`), a copy of each stage a run of this
        suite judged whole, one directory a run under `stages/`, named by the test, and the shared
        constants the TypeScript parsers hold equal (`constants.json`)."""
        keep = os.environ.get("S10_JUDGED_KEEP")
        if not keep or not os.path.exists(os.path.join(self.out, STORE.STAGE_FILE)):
            return
        stages = os.path.join(keep, "stages")
        os.makedirs(stages, exist_ok=True)
        name = f"{self.id().rsplit('.', 1)[-1]}-{len(os.listdir(stages))}"
        shutil.copytree(self.out, os.path.join(stages, name))
        put(os.path.join(keep, "constants.json"), json.dumps({
            "deviationKinds": list(J.DEVIATION_KINDS),
            "ruleVersions": [f"v{version}" for version in CHECKS.RULE_VERSIONS],
            "countingRule": f"v{CHECKS.COUNTING_RULE}",
            "stageVersion": STORE.STAGE_VERSION,
            "startedFile": STORE.STARTED_FILE,
            "startedReason": STORE.STARTED_REASON,
            "inputsVersion": INPUTS_VERSION,
        }))

    def outcome(self, tail):
        with open(os.path.join(self.out, "requests", ask_id(tail), STORE.OUTCOME_FILE)) as handle:
            return json.load(handle)

    def has_outcome(self, tail):
        return os.path.exists(os.path.join(self.out, "requests", ask_id(tail), STORE.OUTCOME_FILE))

    def outcomes(self):
        with open(os.path.join(self.out, STORE.OUTCOMES_FILE)) as handle:
            return [json.loads(line) for line in handle if line.strip()]

    def record(self):
        with open(os.path.join(self.out, STORE.STAGE_FILE)) as handle:
            return json.load(handle)

    def admitted(self, *entries):
        """The inputs stage of `entries` (each a request and its pages, as `request_of` gives them,
        or an inventory line), written for this test, and the admission over it. Where
        `s10-judge.test.ts` asks (`S10_JUDGED_KEEP`), a copy goes under `inputs/`, which it reads
        with `s10-inputs-io.ts`'s own parsers: no stage written here holds a shape the inputs stage
        does not write."""
        admission = write_inputs(self.inputs, self.out, entries)
        keep = os.environ.get("S10_JUDGED_KEEP")
        if keep:
            kept = os.path.join(keep, "inputs")
            os.makedirs(kept, exist_ok=True)
            shutil.copytree(self.inputs, os.path.join(kept, f"{self.id().rsplit('.', 1)[-1]}-{len(os.listdir(kept))}"))
        return admission

    def one_request(self, **changes):
        return self.admitted(request_of("a1", "ACME-1", **changes))

    def two_requests(self):
        return self.admitted(request_of("a1", "ACME-1"), request_of("a2", "ACME-2"))

    def judged_kind(self, **sandbox):
        """The outcome's deviation kind, or its verdict, of one request judged by this sandbox."""
        status = self.judge(self.one_request(), FakeSandbox(self, **sandbox))
        self.assertEqual(status, 0, self.stderr)
        found = self.outcome("a1")
        return found["deviation"]["kind"] if found["deviation"] else found["verdict"]


def shadow_of(test):
    """The stand-in `shadow.py`, loaded as the driver loads the frozen one."""
    path = os.path.join(test.root, "shadow.py")
    data = F.frozen_files(CODE, test.pins)["shadow.py"]
    put(path, data)
    return F.load_shadow(path, test.pins, data)


# ---------- the frozen code (`s10_frozen.py`) ----------

class FrozenCodeTest(Base):
    def test_takes_the_code_txt_whose_every_part_is_pinned(self):
        files = F.frozen_files(CODE, self.pins)
        self.assertEqual(list(files), ["shadow.py", "render.py", "round.py", "kernel_round.py", "tick.sh"])
        self.assertEqual(shadow_of(self).WORK, self.work)

    def test_refuses_a_code_txt_whose_text_is_not_the_pinned(self):
        changed = os.path.join(self.root, "code.txt")
        put(changed, got(CODE).replace(b"A test judge.", b"A test judge!"))
        with self.assertRaisesRegex(STORE.CannotRun, "point 2 fixes"):
            F.frozen_files(changed, self.pins)

    def test_refuses_a_file_a_prompt_or_a_bundle_code_not_the_pinned(self):
        with self.assertRaisesRegex(STORE.CannotRun, "tick.sh"):
            F.frozen_files(CODE, F.Pins(**{**self.pins.__dict__, "files": {**self.pins.files, "tick.sh": "00000000"}}))
        path = os.path.join(self.root, "shadow.py")
        data = F.frozen_files(CODE, self.pins)["shadow.py"]
        put(path, data)
        for field, name in (("prompt_sha256", "JUDGE_PROMPT"), ("bundle_code_sha256", "build_bundle"),
                            ("asked", "ASKED"), ("work", "WORK")):
            with self.subTest(field=field), self.assertRaisesRegex(STORE.CannotRun, name):
                F.load_shadow(path, F.Pins(**{**self.pins.__dict__, field: "other"}), data)

    def test_shadow_py_runs_only_as_pinned_and_as_kept(self):
        path = os.path.join(self.root, "shadow.py")
        data = F.frozen_files(CODE, self.pins)["shadow.py"]
        put(path, data + b"\n# changed after it was kept\n")
        with self.assertRaisesRegex(STORE.CannotRun, "not the shadow.py"):
            F.load_shadow(path, self.pins, data)
        put(path, data)
        with self.assertRaisesRegex(STORE.CannotRun, "point 2 gives"):
            F.load_shadow(path, self.pins, data + b"\n")

    def test_every_way_out_of_the_process_raises_while_s10_runs_and_is_restored_after(self):
        shadow = shadow_of(self)
        self.assertEqual(shadow.DISPATCH, (F.UNREACHABLE["DISPATCH_URL"], F.UNREACHABLE["DISPATCH_TOKEN_FILE"]))
        self.assertFalse([name for name in os.environ if name in F.UNREACHABLE])
        with self.assertRaises(F.InputsGap):
            shadow.configuration()
        with self.assertRaises(F.InputsGap):
            shadow.reserved_neighbour(ask_id("c1"), "ACME-9", EARLIER, [])
        originals = (socket.socket, socket.create_connection, subprocess.Popen, os.system)
        aliased = importlib.import_module("subprocess")
        with F.sealed():
            for reach in (lambda: os.system("true"), lambda: socket.socket(),
                          lambda: socket.create_connection(("192.0.2.1", 80)),
                          lambda: aliased.run(["true"]), lambda: subprocess.check_output(["true"]),
                          lambda: os.posix_spawn("/bin/true", ["true"], {}), lambda: socket.getaddrinfo("x", 80)):
                with self.assertRaises(F.InputsGap):
                    reach()
            self.assertEqual(shadow.os.path.join("a", "b"), "a/b")
        self.assertEqual((socket.socket, socket.create_connection, subprocess.Popen, os.system), originals)
        self.assertEqual(subprocess.run(["true"]).returncode, 0)
        # A name S10 takes from `socket` while it loads is the sealed one for good.
        frozen = types.ModuleType("frozen_alias")
        with F.sealed():
            exec("from socket import socket as taken", frozen.__dict__)  # noqa: S102 - a test of the seal
        with self.assertRaises(F.InputsGap):
            frozen.taken()
        # Its own read stops at the token file that is not there, before any request is made.
        with self.assertRaises(OSError):
            shadow.api("/api/v1/issues/ACME-1")

    def test_a_run_leaves_no_dispatch_variable_in_its_environment(self):
        with mock.patch.dict(os.environ, {"DISPATCH_URL": "http://dispatch.example.test", "DISPATCH_TOKEN": "x"}):
            self.judge(self.one_request(), FakeSandbox(self))
            self.assertFalse([name for name in os.environ if name.startswith("DISPATCH_")])

    def test_refuses_a_judge_schema_with_a_keyword_the_answer_check_does_not_read(self):
        schema = {"type": "object", "properties": {"verdict": {"type": "string", "enum": ["approve"]}}, "required": []}
        self.assertIsNone(CHECKS.schema_refusal(schema))
        self.assertIn("additionalProperties", CHECKS.schema_refusal(dict(schema, additionalProperties=False)))
        self.assertIn("minLength", CHECKS.schema_refusal(
            dict(schema, properties={"reason": {"type": "string", "minLength": 1}})))
        self.assertIn("'tuple'", CHECKS.schema_refusal(dict(schema, type="tuple")))
        code = os.path.join(self.root, "code.txt")
        put(code, got(CODE).replace(b'"reason": TEXT}', b'"reason": dict(TEXT, minLength=1)}'))
        path = os.path.join(self.root, "shadow.py")
        data = F.frozen_files(code, pins_for(code, self.work))["shadow.py"]
        put(path, data)
        with self.assertRaisesRegex(STORE.CannotRun, "minLength"):
            F.load_shadow(path, pins_for(code, self.work), data)

    def test_puts_the_pinned_skill_where_s10_reads_it_and_refuses_another(self):
        shadow = shadow_of(self)
        F.place_skill(shadow, F.skill_files(self.skill, self.never, self.pins))
        self.assertEqual(shadow.skill_bytes()["SKILL.md"], SKILL.decode())
        put(self.never, NEVER + b"\nOne more line.\n")
        with self.assertRaisesRegex(STORE.CannotRun, "never-decide"):
            F.skill_files(self.skill, self.never, self.pins)
        self.assertEqual(self.judge(self.one_request(), FakeSandbox(self)), 2)
        self.assertFalse(os.path.exists(self.out))


# ---------- point 3's reads ----------

class ReaderTest(Base):
    def shadow_over(self, request, records):
        shadow = shadow_of(self)
        admission = self.admitted((request, records))
        reader = J.RequestReader(request, J.Inputs(admission))
        shadow.api, shadow.reserved_neighbour = reader.api, reader.reserved_neighbour
        return shadow, reader

    def test_neighbours_keep_the_kept_candidate_and_pass_over_the_held_and_the_dropped(self):
        request, records = request_of("a1", "ACME-1")
        shadow, reader = self.shadow_over(request, records)
        kept, read, same, after = shadow.neighbours(request["ask"], "ACME-1", request["issue"]["title"], OPENED,
                                                    request["documents"], [])
        self.assertEqual([one["ask_id"] for one in kept], [ask_id("c1")])
        self.assertEqual(read, [ask_id("c1")])
        self.assertEqual(reader.gaps, [])

    def test_a_search_page_the_inputs_did_not_archive_is_a_gap(self):
        full = [{"id": f"ACME-{number}", "kind": "issue", "rank": 1.0} for number in range(50)]
        request, records = request_of("a1", "ACME-1", pages=[full])
        shadow, reader = self.shadow_over(request, records)
        shadow.neighbours(request["ask"], "ACME-1", request["issue"]["title"], OPENED, request["documents"], [])
        self.assertIn("offset 50", reader.gaps[0])

    def test_a_read_no_input_gives_raises(self):
        request, records = request_of("a1", "ACME-1")
        shadow, reader = self.shadow_over(request, records)
        self.assertEqual(shadow.issue("ACME-1")["title"], "The work of ACME-1")
        self.assertEqual(shadow.doc_version("doc-a1", 2)[0], "# The spec of ACME-1\n")
        for path in ("/api/v1/issues/ACME-2", "/api/v1/artifacts/doc-a1/versions/1", f"/api/v1/asks/{ask_id('c2')}",
                     "/api/v1/events/log"):
            with self.subTest(path=path), self.assertRaises(F.InputsGap):
                shadow.api(path)

    def test_a_held_or_dropped_mark_comes_before_any_field_and_both_at_once_is_a_gap(self):
        held = dict(candidate("c2"), held=True, reserve="held")
        held["ask"] = dict(held["ask"], answer={"user": PERSON, "selected": ["Approve"], "text": "HELD", "at": EARLIER})
        dropped = {"dropped": "later-event", "seen": "log", **candidate("c3")}
        for name, marked in (("held", held), ("dropped", dropped)):
            with self.subTest(name=name):
                request, records = request_of("a1", "ACME-1", candidates={
                    ask_id("c1"): candidate("c1"), ask_id("c2"): marked, ask_id("c3"): marked})
                shadow, reader = self.shadow_over(request, records)
                kept, *_ = shadow.neighbours(request["ask"], "ACME-1", request["issue"]["title"], OPENED,
                                             request["documents"], [])
                self.assertEqual([one["ask_id"] for one in kept], [ask_id("c1")])
                self.assertIn("held or dropped and kept", reader.gaps[0])


# ---------- one terminal outcome for each request ----------

class JudgingTest(Base):
    def test_a_request_is_judged_once_by_the_five_calls_s10_records(self):
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(self.one_request(), sandbox), 0, self.stderr)
        [run] = sandbox.runs
        calls = run["calls"]
        self.assertEqual([call["agent"] for call in calls], ["scout"] * 5)
        self.assertEqual({call["model"] for call in calls}, {self.pins.asked})
        self.assertEqual([call["label"] for call in calls], [f"judge ACME-1 {index}" for index in range(1, 6)])
        bundles = {path: data for path, data in run["files"].items() if path.endswith(".md")}
        self.assertEqual(sorted(bundles), [os.path.join(self.work, "bundles", ask_id("a1"), f"judge-{index}.md")
                                           for index in range(1, 6)])
        for index, call in enumerate(calls, 1):
            self.assertIn(f"`{os.path.join(self.work, 'bundles', ask_id('a1'), f'judge-{index}.md')}`", call["prompt"])
            # Kept under --out before its judges started, as sent.
            self.assertEqual(run["kept"][f"judge-{index}.md"], bundles[call["prompt"].split("`")[1]])
        self.assertEqual(json.loads(run["kept"]["calls.json"]), calls)
        # The frozen code wrote under --out, through the link, and the link is gone after the run.
        self.assertEqual(run["link"], os.path.join(self.out, STORE.BUILDING_DIRECTORY))
        self.assertIn(STORE.STARTED_FILE, run["kept"])
        self.assertFalse(os.path.lexists(os.path.join(self.work, "bundles")))
        self.assertFalse(os.path.exists(os.path.join(self.out, STORE.BUILDING_DIRECTORY)))
        found = self.outcome("a1")
        self.assertEqual((found["outcome"], found["verdict"], found["deviation"]), ("verdict", "approve", None))
        self.assertEqual(sorted(found), sorted(["askId", "issueKey", "group", "cases", "outcome", "verdict",
                                                "deviation", "judgement", "judges", "sandbox", "judgedAt", "byRule"]))
        # Its `yield` answers are version 3's deviations, and allowed by versions 4, 5 and 6.
        self.assertEqual({name: (one["outcome"], one["verdict"], one["deviation"], len(one["toolCallDeviations"]))
                          for name, one in found["byRule"].items()},
                         {"v3": ("deviation", None, "tool-call", 5), "v4": ("verdict", "approve", None, 0),
                          "v5": ("verdict", "approve", None, 0), "v6": ("verdict", "approve", None, 0)})
        self.assertEqual(found["judgement"]["votes"], {"approve": 5})
        self.assertEqual(found["judgement"]["neighbour_asks"], [ask_id("c1")])
        self.assertEqual(found["judgement"]["judge_models"],
                         [{"asked": self.pins.asked, "served": None, "fallback": False}] * 5)
        self.assertEqual([line["askId"] for line in self.outcomes()], [ask_id("a1")])
        self.assertEqual(self.runners, [(IMAGE, JOB.SERVICE_ACCOUNT)])
        # One preflight, while the bundles' link was there, before the request started.
        self.assertEqual(len(sandbox.preflights), 1)
        self.assertIn(f"{STORE.PREFLIGHT_DIRECTORY}/0/record.json", self.record()["files"])

    def test_the_stage_record_chains_to_the_inputs_and_names_every_file(self):
        admission = self.one_request()
        self.judge(admission, FakeSandbox(self), more=("--service-account", "session-account"))
        record = self.record()
        self.assertEqual((record["state"], record["claimRun"], record["reference"], record["serviceAccount"]),
                         ("written", "start-1", "registration-1", "session-account"))
        self.assertEqual(self.runners, [(IMAGE, "session-account")])
        self.assertEqual(record["inputs"], {"directory": self.inputs, "stageSha256": "e" * 64,
                                            "files": admission["inputs"]["files"]})
        self.assertEqual(record["claimDirectory"], self.root)
        self.assertEqual(record["frozen"]["codeSha256"], self.pins.code_sha256)
        written = {}
        for directory, _, names in os.walk(self.out):
            for name in names:
                path = os.path.join(directory, name)
                if name != STORE.STAGE_FILE:
                    written[os.path.relpath(path, self.out)] = sha(got(path))
        self.assertEqual(written, record["files"])
        self.assertIn(f"requests/{ask_id('a1')}/sandbox/listing.txt.xz", written)
        self.assertEqual(record["counts"]["byVerdict"], {"approve": 1})
        counted = {name: (one["verdicts"], one["deviationsByKind"]) for name, one in record["counts"]["byRule"].items()}
        self.assertEqual(counted, {"v3": (0, {"tool-call": 1}), "v4": (1, {}), "v5": (1, {}), "v6": (1, {})})
        self.assertEqual(self.outcomes()[0]["byRule"]["v3"], {"outcome": "deviation", "verdict": None,
                                                              "deviation": "tool-call"})

    def test_a_deviation_of_the_inputs_and_a_fact_that_holds_start_no_judge(self):
        admission = self.admitted(deviation_line("b1", "the log gives no title of ACME-5 as of event 7"),
                                  request_of("a2", "ACME-2", hold=True))
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(admission, sandbox), 0, self.stderr)
        self.assertEqual(sandbox.runs, [])
        self.assertEqual(self.outcome("b1")["deviation"]["kind"], "inputs")
        held = self.outcome("a2")
        self.assertEqual((held["outcome"], held["verdict"], held["judgement"]["judges"]),
                         ("hold", "never_decide:waiting-legion-root", 0))
        self.assertEqual([line["outcome"] for line in self.outcomes()], ["deviation", "hold"])

    def test_a_fact_the_frozen_check_reads_otherwise_is_a_deviation(self):
        held = {"holds": False, "legion": True, "primary": True, "root": True, "status": "backlog"}
        sandbox = FakeSandbox(self)
        self.judge(self.admitted(request_of("a1", "ACME-1", hold=True, fact=held)), sandbox)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "fact")
        self.assertEqual(sandbox.runs, [])

    def test_an_input_the_inputs_cannot_give_is_a_deviation_before_any_judge(self):
        full = [{"id": f"ACME-{number}", "kind": "issue", "rank": 1.0} for number in range(50)]
        sandbox = FakeSandbox(self)
        self.judge(self.admitted(request_of("a1", "ACME-1", pages=[full])), sandbox)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "inputs")
        self.assertEqual(sandbox.runs, [])
        self.assertFalse(os.path.exists(os.path.join(self.work, "bundles", ask_id("a1"))))

    def test_an_error_s10s_own_code_raises_on_its_inputs_is_an_inputs_deviation(self):
        request, records = request_of("a1", "ACME-1")
        del request["ask"]["question"]
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(self.admitted((request, records)), sandbox), 0)
        found = self.outcome("a1")
        self.assertEqual(found["deviation"]["kind"], "inputs")
        self.assertIn("S10's own code stopped on its inputs: KeyError", found["deviation"]["reason"])
        self.assertEqual(sandbox.runs, [])

    def test_an_error_of_the_driver_stops_the_run_and_closes_nothing(self):
        with mock.patch.object(J, "built_bundles", side_effect=RuntimeError("a bug of the driver")):
            with self.assertRaisesRegex(RuntimeError, "a bug of the driver"):
                self.judge(self.two_requests(), FakeSandbox(self))
        self.assertEqual(self.record()["state"], "started")
        self.assertFalse(self.has_outcome("a1"))
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(self.two_requests(), resumed), 0, self.stderr)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")
        self.assertEqual(self.outcome("a2")["verdict"], "approve")

    def test_point_five_version_six_lets_these_calls_pass(self):
        def allowed(index, bundle):
            directory = os.path.dirname(bundle)
            ask = os.path.basename(directory)
            relative = os.path.relpath(directory, self.work)
            names = [f"judge-{n}.md" for n in range(1, 6)]
            listed = {"scopePath": relative, "cwd": self.work,
                      "files": [os.path.relpath(os.path.join(directory, name), self.work) for name in names]}
            return [
                ("read", {"path": os.path.relpath(bundle, self.work) + ":1-40", "i": "Reading"}, None),
                ("read", {"path": bundle + ":5-16,960-973"}, None),
                ("grep", {"pattern": "verdict", "path": bundle},
                 {"scopePath": os.path.relpath(bundle, self.work), "searchPath": bundle, "cwd": self.work,
                  "files": [os.path.relpath(bundle, self.work)]}),
                ("glob", {"path": bundle}, None),
                ("glob", {"path": directory}, listed),
                ("glob", {"path": directory + "/*"}, listed),
                ("glob", {"path": relative + "/judge-*.md", "limit": 10}, listed),
                ("glob", {"path": directory + "/judge-{1,{2,3}}.md"}, None),
                ("glob", {"path": directory + "/judge-[1-5].md"}, None),
                ("glob", {"path": HARNESS + "/**"}, {"scopePath": HARNESS, "files": [HARNESS + "/references/"]}),
                ("glob", {"path": "skill://approval-review"},
                 {"scopePath": "skill://approval-review", "files": ["skill://approval-review/references/never-decide.md"]}),
                ("glob", {"path": "skill://approval-review/references/never-decide.md"}, None),
                ("glob", {"path": f"{directory}/*;skill://approval-review/**"},
                 {"scopePath": f"{relative}/*, skill://approval-review/**", "cwd": self.work}),
                ("read", {"path": "skill://approval-review"}, {"resolvedPath": HARNESS + "/SKILL.md"}),
                ("read", {"path": "skill://approval-review/references/never-decide.md:raw"},
                 {"resolvedPath": HARNESS + "/references/never-decide.md"}),
                ("read", {"path": HARNESS + "/references/reviewer-principles.md"}, None),
                # Directory reads whose answers are listings, in omp's own format: its bundle directory
                # (ages empty), the skill tree two levels deep, references/.
                ("read", {"path": relative}, {"isDirectory": True, "resolvedPath": directory},
                 listing_answer([f"  - {name}" for name in names])),
                ("read", {"path": HARNESS}, {"isDirectory": True, "resolvedPath": HARNESS},
                 listing_answer(["  - SKILL.md 10B just now", "  - references/ 3m ago",
                                 "    - never-decide.md 10B 2h ago", "    - reviewer-principles.md 10B 5d ago"])),
                ("read", {"path": "skill://approval-review/references"},
                 {"isDirectory": True, "resolvedPath": HARNESS + "/references"},
                 listing_answer(["  - never-decide.md 10B just now", "  - reviewer-principles.md 10B 1mo ago",
                                 "[2 more lines in listing. Use :4 to continue]"])),
                # Version 6's: any listing inside the work tree or the skill tree, and a grep of the skill tree.
                ("glob", {"path": "."}, {"scopePath": ".", "cwd": self.work,
                                         "files": ["bundles/", f"bundles/{ask}/", f"bundles/{ask}/judge-1.md"]}),
                ("glob", {"path": self.work + "/**"}, None),
                ("glob", {"path": "skill://approval-review/references/*"},
                 {"files": ["skill://approval-review/references/never-decide.md"]}),
                ("grep", {"pattern": "never", "path": HARNESS},
                 {"searchPath": HARNESS, "fileMatches": [{"path": HARNESS + "/references/never-decide.md"}]}),
                ("read", {"path": self.work}, {"isDirectory": True, "resolvedPath": self.work},
                 listing_answer(["  - bundles/ just now", f"    - {ask}/ just now"])),
                ("glob", {}, {"scopePath": ".", "cwd": self.work, "files": ["bundles/"]}),
                ("yield", {"data": json.dumps({"path": "/etc/passwd"})}, None)]
        self.assertEqual(self.judged_kind(judge_tools=allowed), "approve")
        by_rule = self.outcome("a1")["byRule"]
        self.assertEqual(by_rule["v6"], {"outcome": "verdict", "verdict": "approve", "deviation": None,
                                         "toolCallDeviations": []})
        # Version 5 refuses the six calls of each judge only version 6 allows; version 4 its three
        # directory reads beside them.
        for version, count in (("v5", 6), ("v4", 9)):
            refused = by_rule[version]["toolCallDeviations"]
            self.assertEqual((by_rule[version]["outcome"], by_rule[version]["deviation"]), ("deviation", "tool-call"))
            self.assertEqual(sorted(Counter(one["judge"] for one in refused).values()), [count] * 5, version)
        self.assertEqual(by_rule["v3"]["deviation"], "tool-call")

    def test_any_other_tool_call_is_a_deviation(self):
        def own(make):
            return lambda index, bundle: make(bundle, os.path.dirname(bundle))

        def globbed(path):
            return own(lambda bundle, directory: [("glob", {"path": path(bundle, directory)}, None)])

        def other(bundle, directory):
            return os.path.join(directory, "judge-2.md" if bundle.endswith("judge-1.md") else "judge-1.md")

        def directory_read(path, text):
            return own(lambda bundle, directory: [
                ("read", {"path": path(bundle, directory)}, {"isDirectory": True, "resolvedPath": path(bundle, directory)},
                 text)])

        cases = {
            "a read of another bundle": own(lambda bundle, directory: [("read", {"path": other(bundle, directory)}, None)]),
            "a read of no file": own(lambda bundle, directory: [("read", {"path": bundle[:-4] + "x.md"}, None)]),
            "a grep of the skill tree reporting a bundle": own(lambda bundle, directory: [
                ("grep", {"pattern": "x", "path": HARNESS}, {"searchPath": HARNESS, "files": [other(bundle, directory)]})]),
            "a directory read no answer establishes": own(lambda bundle, directory: [
                ("read", {"path": os.path.dirname(directory)}, None)]),
            "a directory read with an empty answer": directory_read(lambda bundle, directory: directory, ""),
            "a directory read naming an entry no manifest holds": directory_read(
                lambda bundle, directory: directory, listing_answer(["  - judge-1.md", "  - secret.md"])),
            "a directory read placing an entry at the wrong path": directory_read(
                lambda bundle, directory: self.work, listing_answer(["  - judge-1.md 1KB just now"])),
            "a directory read giving a directory as a file": directory_read(
                lambda bundle, directory: self.work, listing_answer(["  - bundles just now"])),
            "a directory read with text for its size and age": directory_read(
                lambda bundle, directory: self.work, listing_answer(["  - bundles/ THIS IS FILE TEXT, NOT SIZES OR AGES"])),
            "a directory read with a size not the file's": directory_read(
                lambda bundle, directory: HARNESS, listing_answer(["  - SKILL.md 26.5KB just now"])),
            "a directory read with a size but no age": directory_read(
                lambda bundle, directory: HARNESS, listing_answer(["  - SKILL.md 10B"])),
            "a directory read without its root": directory_read(
                lambda bundle, directory: directory, "  - judge-1.md\n  - judge-2.md"),
            "a directory read one space off": directory_read(lambda bundle, directory: directory, ".\n - judge-1.md"),
            "a directory read answering a file's text": directory_read(lambda bundle, directory: directory, "# The spec\n"),
            "a directory read outside the trees": directory_read(lambda bundle, directory: self.root,
                                                                 listing_answer(["  - shadow/"])),
            "a grep of the bundle directory": own(lambda bundle, directory: [
                ("grep", {"pattern": "x", "path": directory}, None)]),
            "a grep of the work tree": own(lambda bundle, directory: [("grep", {"pattern": "x", "path": self.work}, None)]),
            "a grep of the skill tree's parent": own(lambda bundle, directory: [
                ("grep", {"pattern": "x", "path": os.path.dirname(HARNESS)}, None)]),
            "a grep with no path": own(lambda bundle, directory: [("grep", {"pattern": "x"}, None)]),
            "a glob of the work tree's parent": globbed(lambda bundle, directory: os.path.dirname(self.work) + "/*"),
            "a glob of the root": globbed(lambda bundle, directory: "/*"),
            "a glob of the run's own output": globbed(lambda bundle, directory: os.path.join(self.root, "s10") + "/**"),
            "a glob that climbs out of the work tree": globbed(lambda bundle, directory: directory + "/../../../*"),
            "a glob whose pattern climbs": globbed(lambda bundle, directory: directory + "/*/../*"),
            "a glob whose pattern holds a dot part": globbed(lambda bundle, directory: directory + "/*/./x"),
            "a glob of another skill": globbed(lambda bundle, directory: "skill://dispatch/**"),
            "a glob its result says left": own(lambda bundle, directory: [
                ("glob", {"path": directory}, {"scopePath": "/etc", "files": ["/etc/passwd"]})]),
            "a glob that returns a path no manifest holds": own(lambda bundle, directory: [
                ("glob", {"path": directory}, {"scopePath": directory, "files": ["../outside.md"], "cwd": directory})]),
            "a glob that returns a sixth bundle": own(lambda bundle, directory: [
                ("glob", {"path": directory}, {"scopePath": directory, "files": [directory + "/judge-6.md"]})]),
            "a glob over two scopes, one left": own(lambda bundle, directory: [
                ("glob", {"path": directory}, {"scopePath": f"{directory}, /etc"})]),
            "a glob with an argument that may be a target": own(lambda bundle, directory: [
                ("glob", {"path": directory, "pattern": "/etc/*"}, None)]),
            "a search": own(lambda bundle, directory: [("web_search", {"query": "his answer"}, None)]),
            "a find": own(lambda bundle, directory: [("find", {"query": "bundle"}, None)]),
            "a skill resolved elsewhere": own(lambda bundle, directory: [
                ("read", {"path": "skill://approval-review"}, {"resolvedPath": "/etc/passwd"})]),
            "a file outside the skill": own(lambda bundle, directory: [
                ("read", {"path": "skill://approval-review/../x"}, None)]),
            "a list with one other path": own(lambda bundle, directory: [
                ("grep", {"pattern": "x", "path": f"{bundle};/work"}, None)]),
            # Point 5's glob grammar: what could spell a parent step, a `/` or a split, or is not modelled.
            "brace alternatives that climb": globbed(lambda bundle, directory: HARNESS + "/{..,..}/agent-secrets/**"),
            "one brace alternative that climbs": globbed(lambda bundle, directory: directory + "/{..}/*"),
            "a nested alternative that climbs": globbed(lambda bundle, directory: directory + "/{a,{b,..}}/*"),
            "a brace alternative with a slash": globbed(lambda bundle, directory: directory + "/{../x,y}/*"),
            "a brace alternative that is absolute": globbed(lambda bundle, directory: directory + "/{/etc,y}/*"),
            "an empty brace alternative": globbed(lambda bundle, directory: directory + "/{,a}/*"),
            "an unbalanced brace": globbed(lambda bundle, directory: directory + "/{a/*"),
            "a closing brace alone": globbed(lambda bundle, directory: directory + "/a}/*"),
            "a list split inside braces": globbed(lambda bundle, directory: directory + "/{a;../..}/*"),
            "too many expansions": globbed(lambda bundle, directory: directory + "/" + "{a,b}" * 9),
            "a class of dots": globbed(lambda bundle, directory: directory + "/[.][.]/*"),
            "a negated class": globbed(lambda bundle, directory: directory + "/[!a]*"),
            "a caret class": globbed(lambda bundle, directory: directory + "/[^a]*"),
            "a range over the dot": globbed(lambda bundle, directory: directory + "/[--0]*"),
            "a backwards range": globbed(lambda bundle, directory: directory + "/[z-a]*"),
            "a POSIX class": globbed(lambda bundle, directory: directory + "/[[:alpha:]]*"),
            "an unclosed class": globbed(lambda bundle, directory: directory + "/[a*"),
            "an extended glob": globbed(lambda bundle, directory: directory + "/@(..)/*"),
            "a negated extended glob": globbed(lambda bundle, directory: directory + "/!(x)"),
            "backslashes": globbed(lambda bundle, directory: directory + "\\..\\..\\*"),
            "a list split on a comma": globbed(lambda bundle, directory: directory + "/*,/etc/*"),
            "a file and a comma": globbed(lambda bundle, directory: bundle + ",/etc"),
            "a list split on a blank": globbed(lambda bundle, directory: directory + "/* /etc/*"),
            "a quoted path": globbed(lambda bundle, directory: f'"{directory}/../.."'),
            "a percent escape": globbed(lambda bundle, directory: "skill://approval-review/%2e%2e/*"),
            "a selector that is a path": own(lambda bundle, directory: [
                ("read", {"path": bundle + ":5-16,/etc/passwd"}, None)]),
        }
        for name, tools in cases.items():
            with self.subTest(name=name):
                shutil.rmtree(self.out, ignore_errors=True)
                self.assertEqual(self.judged_kind(judge_tools=tools), "tool-call")

    def test_a_text_read_reporting_another_bundle_is_refused_under_every_version(self):
        def other(bundle):
            directory = os.path.dirname(bundle)
            return os.path.join(directory, "judge-2.md" if bundle.endswith("judge-1.md") else "judge-1.md")

        cases = {
            "read": lambda index, bundle: [
                ("read", {"path": bundle}, {"resolvedPath": bundle, "fileMatches": [{"path": other(bundle)}]})],
            "grep": lambda index, bundle: [
                ("grep", {"pattern": "x", "path": bundle}, {"searchPath": bundle, "fileMatches": [{"path": other(bundle)}]})],
        }
        for tool, tools in cases.items():
            with self.subTest(tool=tool):
                shutil.rmtree(self.out, ignore_errors=True)
                self.assertEqual(self.judged_kind(judge_tools=tools), "tool-call")
                by_rule = self.outcome("a1")["byRule"]
                for version in ("v3", "v4", "v5", "v6"):
                    refused = [one for one in by_rule[version]["toolCallDeviations"] if one["tools"] == [tool]]
                    self.assertEqual(by_rule[version]["deviation"], "tool-call", version)
                    self.assertEqual(len(refused), 5, version)
                    self.assertTrue(all("neither its own bundle file nor a skill file" in one["reason"]
                                        for one in refused), version)

    def test_a_listings_sizes_are_printed_as_omp_prints_them(self):
        cases = {0: "0B", 10: "10B", 1023: "1023B", 1024: "1.0KB", 1280: "1.3KB", 27156: "26.5KB",
                 1024 ** 2: "1.0MB", 5 * 1024 ** 3 // 2: "2.5GB"}
        for size, printed in cases.items():
            with self.subTest(size=size):
                self.assertEqual(CHECKS.listing_size(size), printed)
        # Lines as omp 18.16.0 prints them, the first byte for byte as a judge's read of its bundle
        # directory answered it in a smoke.
        self.assertTrue(all(CHECKS.LISTING_ENTRY.fullmatch(line) for line in [
            "  - judge-3.md 26.5KB just now", "  - references/ 3m ago", "    - never-decide.md 1.2KB 2h ago"]))

    def test_a_link_the_listing_records_is_followed_before_the_scope_is_read(self):
        def through_link(index, bundle):
            return [("read", {"path": HARNESS + "/references/never-decide.md"}, None)]

        listing = [f"l 777 4 {HARNESS}/references -> /etc"]
        self.assertEqual(self.judged_kind(judge_tools=through_link, listing=listing), "tool-call")

    def test_manifests_that_are_not_the_required_trees_or_that_change_are_a_deviation(self):
        wanted = {"work": SANDBOX.work_manifest(ask_id("a1")), "skill": SANDBOX.SKILL_MANIFEST}
        work = wanted["work"]
        cases = {
            "a changed after": {"before": wanted, "after": dict(wanted, work=work + [["notes.txt", "f"]])},
            "a stray file in the work tree": {"before": dict(wanted, work=sorted(work + [["notes.txt", "f"]]))},
            "a second request's directory": {"before": dict(wanted, work=sorted(work + [["bundles/other", "d"]]))},
            "another request's bundles": {"before": dict(wanted, work=SANDBOX.work_manifest(ask_id("a2")))},
            "a link before": {"before": dict(wanted, skill=SANDBOX.SKILL_MANIFEST[:3] + [
                ["references/reviewer-principles.md", "l"]])},
            "none recorded": None,
            "a finished result missing only after": {"before": wanted},
            "after that is no record": {"before": wanted, "after": None},
        }
        for name, manifests in cases.items():
            with self.subTest(name=name):
                self.assertEqual(self.judged_kind(manifests=manifests), "manifest")
                shutil.rmtree(self.out)
        # Versions 4 and 5 hold the bundle directory and the skill tree; a stray file beside
        # `bundles/` breaks version 6's manifest alone.
        stray = dict(wanted, work=sorted(work + [["notes.txt", "f"]]))
        self.assertEqual(self.judged_kind(manifests={"before": stray, "after": stray}), "manifest")
        by_rule = self.outcome("a1")["byRule"]
        self.assertEqual({version: by_rule[version]["deviation"] or by_rule[version]["verdict"]
                          for version in ("v4", "v5", "v6")}, {"v4": "approve", "v5": "approve", "v6": "manifest"})
        shutil.rmtree(self.out)

        # A sandbox that started no judge, its manifest refused before them: no version has a judge.
        class NoJudge(FakeSandbox):
            def run(self, ask, files, extra_env=None):
                result = super().run(ask, files, extra_env)
                record = json.loads(result.files["record.json"])
                del record["omp"]
                result.files["record.json"] = json.dumps(record).encode()
                return result

        self.assertEqual(self.judge(self.one_request(), NoJudge(self, manifests={"before": stray})), 0, self.stderr)
        by_rule = self.outcome("a1")["byRule"]
        # Version 3 holds no manifest, so its outcome is the missing transcripts; versions 4 and 5
        # find no manifest after the judges, and version 6 a stray file before them.
        self.assertEqual({version: one["deviation"] for version, one in by_rule.items()},
                         {"v3": "transcript", "v4": "manifest", "v5": "manifest", "v6": "manifest"})
        self.assertIn("the sandbox started no judge, so no judge has a transcript",
                      self.outcome("a1")["deviation"]["problems"])

    def test_the_gateway_key_is_kept_in_no_file_and_a_request_whose_files_held_it_is_a_deviation(self):
        leaked = {"sessions/top/judge2.jsonl.extra": b"read " + KEY + b" here\n"}
        self.assertEqual(self.judged_kind(files=leaked, key=KEY), "key")
        for directory, _, names in os.walk(self.out):
            for name in names:
                self.assertNotIn(KEY, got(os.path.join(directory, name)), name)
        shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(record={"keyFound": ["sessions/top/judge1.jsonl"]}), "key")

    def test_a_judge_asked_or_served_another_model_or_marked_a_fallback_is_a_deviation(self):
        cases = {
            "a fallback": {"model_changes": lambda index: [{"type": "model_change", "model": "anthropic/claude-opus-5-5",
                                                            "resolvedModelIsFallback": index == 2}]},
            "a switch": {"model_changes": lambda index: [{"type": "model_change", "model": "anthropic/claude-sonnet-5"}]},
            "another served": {"served": {3: "claude-sonnet-5"}},
            "another asked": {"resolved": {1: "anthropic/claude-opus-5-5:high"}},
        }
        for name, changes in cases.items():
            with self.subTest(name=name):
                self.assertEqual(self.judged_kind(**changes), "model")
                shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(served={0: "claude-opus-5-5", 1: "anthropic/claude-opus-5-5"}), "approve")

    def test_any_count_of_valid_answers_but_five_is_a_deviation(self):
        cases = {
            "a malformed answer": lambda index: dict(answer(), verdict="maybe") if index == 0 else answer(),
            "a missing answer": lambda index: None if index == 4 else answer(),
            "an unparsed answer": lambda index: "not json" if index == 1 else answer(),
            "an answer missing a field": lambda index: {"verdict": "approve"} if index == 2 else answer(),
        }
        for name, answers in cases.items():
            with self.subTest(name=name):
                self.assertEqual(self.judged_kind(answers=answers), "answers")
                shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(answers=lambda index: json.dumps(answer("request_changes"))),
                         "request_changes")

    def test_a_missing_transcript_is_a_deviation(self):
        self.assertEqual(self.judged_kind(drop={3}), "transcript")

    def test_an_unreadable_transcript_or_answers_file_is_its_requests_sandbox_deviation_and_the_run_goes_on(self):
        for name, files in (("a transcript line", {"sessions/top/judge3.jsonl": b"{\"type\": \"sess\n"}),
                            ("the answers", {"results.json": b"[{\"id\": "})):
            with self.subTest(name=name):
                admission = self.two_requests()

                class Once(FakeSandbox):
                    def run(self, ask, sent, extra_env=None):
                        self.files = files if not self.runs else {}
                        return super().run(ask, sent)

                self.assertEqual(self.judge(admission, Once(self)), 0, self.stderr)
                self.assertEqual(self.outcome("a1")["deviation"]["kind"], "sandbox")
                self.assertIn("cannot be read", self.outcome("a1")["deviation"]["reason"])
                self.assertEqual(self.outcome("a2")["verdict"], "approve")
                shutil.rmtree(self.out)

    def test_a_session_that_makes_any_call_but_the_one_cell_is_a_deviation(self):
        cell = ("eval", {"language": "py", "code": SANDBOX.CELL}, None)
        cases = {
            "a second call": {"top_calls": [cell, ("read", {"path": SANDBOX.CALLS_FILE}, None)]},
            "another cell": {"top_calls": [("eval", {"language": "py", "code": SANDBOX.CELL + "print(1)\n"}, None)]},
            "omp's exit": {"record": {"omp": {"returncode": 1}}},
        }
        for name, changes in cases.items():
            with self.subTest(name=name):
                self.assertEqual(self.judged_kind(**changes), "session")
                shutil.rmtree(self.out)

    def test_a_recorded_verdict_unlike_the_rules_is_a_deviation(self):
        text = got(CODE).replace(b"if 2 * sum(named.values()) > len(good):", b"if 4 * sum(named.values()) > len(good):")
        code = os.path.join(self.root, "code.txt")
        put(code, text)
        two = lambda index: answer(kind="credential") if index < 2 else answer()  # noqa: E731
        status = self.judge(self.one_request(), FakeSandbox(self, answers=two), pins=pins_for(code, self.work), code=code)
        self.assertEqual(status, 0, self.stderr)
        found = self.outcome("a1")
        self.assertEqual(found["deviation"]["kind"], "verdict")
        self.assertIsNone(found["verdict"])

    def test_the_rule_recomputes_the_verdict(self):
        kinds = ["credential", "destructive-act"]
        cases = [([answer(kind="credential")] * 3 + [answer()] * 2, "never_decide:credential"),
                 ([answer(kind="credential")] * 2 + [answer(kind="destructive-act")] * 2 + [answer()],
                  "never_decide:credential+destructive-act"),
                 ([answer(kind="credential")] * 2 + [answer("request_changes")] * 3, "request_changes"),
                 ([answer("request_changes")] * 2 + [answer()] * 3, "approve")]
        for answers, verdict in cases:
            self.assertEqual(CHECKS.rule_verdict(answers, kinds), verdict)

    def test_a_job_with_no_result_is_its_requests_deviation_and_the_run_goes_on(self):
        class Once(FakeSandbox):
            def run(self, ask, files, extra_env=None):
                self.failure = "the Job did not finish by its deadline" if not self.runs else None
                return super().run(ask, files)

        self.assertEqual(self.judge(self.two_requests(), Once(self)), 0, self.stderr)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "sandbox")
        self.assertEqual(self.outcome("a2")["verdict"], "approve")

    def test_a_pod_that_ran_another_image_or_a_metadata_server_that_answered_is_a_deviation(self):
        self.assertEqual(self.judged_kind(image_id="registry.example.test/sandbox@sha256:" + "b" * 64), "sandbox")
        shutil.rmtree(self.out)
        answered = [{"url": url, "connected": True, "status": 200} for url in SANDBOX.METADATA_REQUESTS]
        self.assertEqual(self.judged_kind(record={"metadata": answered}), "sandbox")

    def test_the_driver_makes_the_sandboxs_mount_and_listing_checks_again(self):
        stray = [f"f 644 1 {self.root}/stray"]
        self.assertEqual(self.judged_kind(listing=stray), "sandbox")
        self.assertIn("outside the bundles", self.outcome("a1")["deviation"]["reason"])
        shutil.rmtree(self.out)
        # Admission: the five sent are the only evidence bundles anywhere in the sandbox, by name and
        # by bytes.
        self.assertEqual(self.judged_kind(listing=["f 644 1 /opt/legion/examples/judge-1.md"]), "sandbox")
        self.assertIn("evidence bundle's name", self.outcome("a1")["deviation"]["reason"])
        shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(copied="/opt/legion/examples/copy.txt"), "sandbox")
        self.assertIn("holds the bytes of an evidence bundle", self.outcome("a1")["deviation"]["reason"])
        shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(copied="/opt/legion/examples/copy.txt", record={"sameSize": {}}), "sandbox")
        self.assertIn("recorded no sha256", self.outcome("a1")["deviation"]["reason"])
        shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(record={"mounts": ["/", "/proc", "/elsewhere"]}), "sandbox")
        shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(record={"layout": None}), "sandbox")
        shutil.rmtree(self.out)
        self.assertEqual(self.judged_kind(record={"preflight": True}), "sandbox")

    def test_a_deviation_names_a_kind_of_this_stage(self):
        with self.assertRaises(ValueError):
            J.deviation({"askId": "a", "issueKey": None, "group": "report", "cases": 1}, "unjudged", "no")


# ---------- stops, and the stage on disk ----------

def bun_cannot_run(text, path):
    """A read-back that does not run, as `read_back_with_bun` stops where Bun cannot run."""
    raise J.Stopped("bun could not run")


class StageTest(Base):
    def test_a_written_stage_is_never_judged_again(self):
        admission = self.one_request()
        self.judge(admission, FakeSandbox(self))
        before = got(os.path.join(self.out, STORE.STAGE_FILE))
        again = FakeSandbox(self)
        self.assertEqual(self.judge(admission, again), 0, self.stderr)
        self.assertEqual(again.runs, [])
        self.assertEqual(got(os.path.join(self.out, STORE.STAGE_FILE)), before)

    def test_a_sandbox_that_refuses_after_the_preflight_stops_the_run_with_no_outcome_for_its_request(self):
        admission = self.two_requests()
        sandbox = FakeSandbox(self, record={"refused": "the variable GITHUB_TOKEN names a credential"})
        self.assertEqual(self.judge(admission, sandbox), 2)
        self.assertEqual(len(sandbox.runs), 1)
        self.assertFalse(self.has_outcome("a1"))
        self.assertFalse(self.has_outcome("a2"))
        record = self.record()
        self.assertEqual(record["state"], "started")
        self.assertIn("GITHUB_TOKEN", record["stops"][0]["reason"])
        self.assertIn(f"requests/{ask_id('a1')}/sandbox/record.json", record["files"])
        self.assertFalse(os.path.exists(os.path.join(self.out, STORE.OUTCOMES_FILE)))
        # Its bundles were made and kept and its Job ran: point 6 never judges it again.
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a2")])
        self.assertEqual([line["deviation"] for line in self.outcomes()], ["started", None])

    def test_a_preflight_that_fails_stops_the_run_before_any_request_starts(self):
        cases = {
            "a refusal": {"preflight_record": {"refused": "the variable GITHUB_TOKEN names a credential"}},
            "a metadata server that answered": {"preflight_record": {"metadata": [
                {"url": url, "connected": True, "status": 200} for url in SANDBOX.METADATA_REQUESTS]}},
            "another image": {"preflight_image_id": "registry.example.test/sandbox@sha256:" + "b" * 64},
            "no readable result": {"preflight_failure": "the Job did not finish by its deadline"},
            "a file outside": {"preflight_listing": [f"f 644 1 {self.root}/stray"]},
            "an evidence bundle": {"preflight_listing": ["f 644 1 /tmp/judge-1.md"]},
            "a record of a request's Job": {"preflight_record": {"preflight": False}},
        }
        for name, changes in cases.items():
            with self.subTest(name=name):
                admission = self.two_requests()
                sandbox = FakeSandbox(self, **changes)
                self.assertEqual(self.judge(admission, sandbox), 2)
                self.assertEqual((len(sandbox.preflights), sandbox.runs), (1, []))
                record = self.record()
                self.assertEqual(record["state"], "started")
                self.assertIn("preflight", record["stops"][0]["reason"])
                self.assertFalse([name for name in record["files"] if name.startswith("requests/")])
                self.assertIn(f"{STORE.PREFLIGHT_DIRECTORY}/0/job.json", record["files"])
                resumed = FakeSandbox(self)
                self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
                self.assertEqual([line["verdict"] for line in self.outcomes()], ["approve", "approve"])
                self.assertIn(f"{STORE.PREFLIGHT_DIRECTORY}/1/record.json", self.record()["files"])
                shutil.rmtree(self.out)

    def test_the_preflight_runs_once_a_run_and_only_before_a_request_starts(self):
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(self.two_requests(), sandbox), 0, self.stderr)
        self.assertEqual(len(sandbox.preflights), 1)
        shutil.rmtree(self.out)
        only_deviations = self.admitted(deviation_line("b1", "no title"))
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(only_deviations, sandbox), 0, self.stderr)
        self.assertEqual(sandbox.preflights, [])

    def test_a_kubernetes_api_failure_stops_the_run_with_no_outcome_and_is_resumed(self):
        admission = self.two_requests()
        sandbox = FakeSandbox(self, raise_on=JOB.KubeUnavailable("kubectl get failed 5 times, last exited 1"))
        self.assertEqual(self.judge(admission, sandbox), 2)
        self.assertFalse(self.has_outcome("a1"))
        record = self.record()
        self.assertEqual(record["state"], "started")
        self.assertIn("kubectl get failed", record["stops"][0]["reason"])
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a2")])
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")
        self.assertEqual(self.outcome("a1")["deviation"]["reason"], STORE.STARTED_REASON)

    def test_an_outcome_the_analysis_cannot_read_is_a_shape_deviation_kept_beside_its_place(self):
        admission = self.two_requests()
        refused = []

        def read_back(text, path):
            if ask_id("a1") in path and '"shape"' not in text:
                refused.append(text)
                return "CorpusError: a judged document holds no sha256"
            return None

        self.assertEqual(self.judge(admission, FakeSandbox(self), read_back_of=read_back), 0, self.stderr)
        found = self.outcome("a1")
        self.assertEqual((found["outcome"], found["deviation"]["kind"], found["judgement"]), ("deviation", "shape", None))
        self.assertIn("a judged document holds no sha256", found["deviation"]["reason"])
        self.assertEqual({one["deviation"] for one in found["byRule"].values()}, {"shape"})
        unread = os.path.join(self.out, "requests", ask_id("a1"), STORE.UNREAD_FILE)
        self.assertEqual(got(unread).decode(), refused[0])
        self.assertEqual(json.loads(refused[0])["verdict"], "approve")
        self.assertIn(f"requests/{ask_id('a1')}/{STORE.UNREAD_FILE}", self.record()["files"])
        self.assertEqual(self.outcome("a2")["verdict"], "approve")

    def test_a_read_back_that_cannot_run_stops_the_run_and_its_outcome_is_committed_on_resume(self):
        admission = self.one_request()
        first = FakeSandbox(self)
        self.assertEqual(self.judge(admission, first, read_back_of=bun_cannot_run), 2)
        self.assertEqual([run["ask"] for run in first.runs], [ask_id("a1")])
        self.assertFalse(self.has_outcome("a1"))
        self.assertIn("bun could not run", self.record()["stops"][0]["reason"])
        # The outcome built whole is kept, held to its sha256 like every file of the stage.
        pending = f"requests/{ask_id('a1')}/{STORE.PENDING_FILE}"
        self.assertIn(pending, self.record()["files"])
        kept = got(os.path.join(self.out, pending))
        self.assertEqual(json.loads(kept)["verdict"], "approve")
        # A second run whose read-back cannot run either stops again, the same outcome kept.
        self.assertEqual(self.judge(admission, FakeSandbox(self), read_back_of=bun_cannot_run), 2)
        self.assertEqual(len(self.record()["stops"]), 2)
        self.assertEqual(got(os.path.join(self.out, pending)), kept)
        # A run whose read-back runs commits that outcome, and judges nothing again (point 6).
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual(resumed.runs, [])
        self.assertEqual(self.outcome("a1")["verdict"], "approve")
        self.assertEqual(got(os.path.join(self.out, "requests", ask_id("a1"), STORE.OUTCOME_FILE)), kept)
        with mock.patch.object(J, "BUN", os.path.join(self.root, "no-bun")):
            with self.assertRaisesRegex(J.Stopped, "could not read back"):
                J.read_back_with_bun("{}", "requests/a/outcome.json")
        # A Bun whose check prints no JSON, or exits 2 with its usage, stops the run the same way.
        for name, body in {"no JSON": "echo not json", "exit 2": "echo usage >&2; exit 2"}.items():
            with self.subTest(bun=name):
                stand_in = os.path.join(self.root, "bun-" + name.replace(" ", "-"))
                put(stand_in, f"#!/bin/sh\n{body}\n")
                os.chmod(stand_in, 0o755)
                with mock.patch.object(J, "BUN", stand_in):
                    with self.assertRaisesRegex(J.Stopped, "could not read back"):
                        J.read_back_with_bun("{}", "requests/a/outcome.json")

    def test_an_interrupt_or_a_sigterm_during_the_read_back_keeps_the_outcome_pending(self):
        def interrupted(text, path):
            raise KeyboardInterrupt

        def terminated(text, path):
            os.kill(os.getpid(), signal.SIGTERM)
            self.fail("the SIGTERM did not stop the read-back")

        for name, read_back in (("an interrupt", interrupted), ("a SIGTERM", terminated)):
            with self.subTest(name=name):
                admission = self.one_request()
                first = FakeSandbox(self)
                self.assertEqual(self.judge(admission, first, read_back_of=read_back), 2)
                self.assertEqual([run["ask"] for run in first.runs], [ask_id("a1")])
                self.assertFalse(self.has_outcome("a1"))
                self.assertEqual(len(self.record()["stops"]), 1)
                pending = f"requests/{ask_id('a1')}/{STORE.PENDING_FILE}"
                self.assertIn(pending, self.record()["files"])
                kept = got(os.path.join(self.out, pending))
                self.assertEqual(json.loads(kept)["verdict"], "approve")
                # The resume commits the outcome kept, and judges nothing again (point 6).
                resumed = FakeSandbox(self)
                self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
                self.assertEqual((resumed.runs, resumed.preflights), ([], []))
                self.assertEqual(got(os.path.join(self.out, "requests", ask_id("a1"), STORE.OUTCOME_FILE)), kept)
                shutil.rmtree(self.out)

    def test_a_pending_outcome_the_parser_refuses_on_resume_is_a_shape_deviation_and_no_judge_starts(self):
        def refusing(text, path):
            return None if '"shape"' in text else "CorpusError: a judged document holds no sha256"

        admission = self.one_request()
        self.assertEqual(self.judge(admission, FakeSandbox(self), read_back_of=bun_cannot_run), 2)
        directory = os.path.join(self.out, "requests", ask_id("a1"))
        kept = got(os.path.join(directory, STORE.PENDING_FILE))
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed, read_back_of=refusing), 0, self.stderr)
        self.assertEqual((resumed.runs, resumed.preflights), ([], []))
        found = self.outcome("a1")
        self.assertEqual((found["outcome"], found["deviation"]["kind"], found["judgement"]), ("deviation", "shape", None))
        self.assertIn("a judged document holds no sha256", found["deviation"]["reason"])
        self.assertEqual(found["judgedAt"], json.loads(kept)["judgedAt"])
        # The outcome refused is the one kept pending, byte for byte, beside the deviation in its place.
        self.assertEqual(got(os.path.join(directory, STORE.UNREAD_FILE)), kept)
        self.assertIn(f"requests/{ask_id('a1')}/{STORE.UNREAD_FILE}", self.record()["files"])

    def test_a_pending_inputs_or_fact_deviation_has_no_started_json_and_is_committed_on_resume(self):
        held = {"holds": False, "legion": True, "primary": True, "root": True, "status": "backlog"}
        cases = {"inputs": (deviation_line("b1", "the log gives no title of ACME-5 as of event 7"), "b1"),
                 "fact": (request_of("a1", "ACME-1", hold=True, fact=held), "a1")}
        for kind, (entry, tail) in cases.items():
            with self.subTest(kind=kind):
                admission = self.admitted(entry)
                first = FakeSandbox(self)
                self.assertEqual(self.judge(admission, first, read_back_of=bun_cannot_run), 2)
                self.assertEqual((first.runs, first.preflights), ([], []))
                # Its outcome was built before the request started, so the pending outcome is all it holds.
                directory = os.path.join(self.out, "requests", ask_id(tail))
                self.assertEqual(os.listdir(directory), [STORE.PENDING_FILE])
                kept = got(os.path.join(directory, STORE.PENDING_FILE))
                resumed = FakeSandbox(self)
                self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
                self.assertEqual((resumed.runs, resumed.preflights), ([], []))
                self.assertEqual(self.outcome(tail)["deviation"]["kind"], kind)
                self.assertEqual(got(os.path.join(directory, STORE.OUTCOME_FILE)), kept)
                self.assertEqual(sorted(os.listdir(directory)), [STORE.OUTCOME_FILE, STORE.PENDING_FILE])
                shutil.rmtree(self.out)

    def test_an_outcome_that_is_no_json_keeps_no_pending_file_and_the_stop_goes_on(self):
        load = J.load_shadow

        def no_json(*arguments):
            shadow = load(*arguments)
            finish = shadow.finish_judgement

            def finish_judgement(*inner):
                return dict(finish(*inner), seen={"a set, which JSON cannot hold"})

            shadow.finish_judgement = finish_judgement
            return shadow

        admission = self.one_request()
        first = FakeSandbox(self)
        with mock.patch.object(J, "load_shadow", no_json):
            self.assertEqual(self.judge(admission, first, read_back_of=bun_cannot_run), 2)
        self.assertEqual([run["ask"] for run in first.runs], [ask_id("a1")])
        self.assertIn("bun could not run", self.record()["stops"][0]["reason"])
        self.assertFalse(self.has_outcome("a1"))
        pending = f"requests/{ask_id('a1')}/{STORE.PENDING_FILE}"
        self.assertNotIn(pending, self.record()["files"])
        self.assertFalse(os.path.exists(os.path.join(self.out, pending)))
        # With no outcome kept, the request is one a run started and did not complete (point 6).
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual(resumed.runs, [])
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")

    def test_a_pending_outcome_of_another_request_is_refused_and_nothing_is_committed(self):
        admission = self.one_request()
        self.assertEqual(self.judge(admission, FakeSandbox(self), read_back_of=bun_cannot_run), 2)
        pending = f"requests/{ask_id('a1')}/{STORE.PENDING_FILE}"
        kept = got(os.path.join(self.out, pending))
        opened = self.record()
        for key, other in (("askId", ask_id("a2")), ("issueKey", "ACME-2"), ("group", "another-tree"), ("cases", 2)):
            with self.subTest(key=key):
                # A pending file the record holds by its sha256, of a request other than the one it is kept for.
                changed = STORE.json_bytes(dict(json.loads(kept), **{key: other}))
                put(os.path.join(self.out, pending), changed)
                record = dict(opened, files=dict(opened["files"], **{pending: sha(changed)}))
                put(os.path.join(self.out, STORE.STAGE_FILE), STORE.json_bytes(record))
                resumed = FakeSandbox(self)
                self.assertEqual(self.judge(admission, resumed), 2)
                self.assertIn(f"{pending} is no outcome of the request it is kept for: its {key} is", self.stderr)
                self.assertEqual((resumed.runs, resumed.preflights), ([], []))
                self.assertFalse(self.has_outcome("a1"))
                # Nothing is committed and no stop is recorded: the record is the one this run opened.
                self.assertEqual(self.record(), record)
        # The pending outcome of that request, put back, is committed.
        put(os.path.join(self.out, pending), kept)
        put(os.path.join(self.out, STORE.STAGE_FILE), STORE.json_bytes(opened))
        self.assertEqual(self.judge(admission, FakeSandbox(self)), 0, self.stderr)
        self.assertEqual(got(os.path.join(self.out, "requests", ask_id("a1"), STORE.OUTCOME_FILE)), kept)

    def test_the_analysis_parser_reads_each_outcome_back_through_bun(self):
        bun = os.environ.get("S10_BUN")
        if not bun:
            self.skipTest("s10-judge.test.ts runs this with its own Bun (S10_BUN)")
        admission = self.one_request()
        with mock.patch.object(J, "BUN", bun):
            self.assertEqual(self.judge(admission, FakeSandbox(self), read_back_of=J.read_back_with_bun), 0,
                             self.stderr)
            self.assertEqual(self.outcome("a1")["verdict"], "approve")
            self.assertIn("outcome.json", J.read_back_with_bun("{}", "requests/a/outcome.json"))
            # S10's code returning a judgement the parser refuses: a document with no sha256.
            shutil.rmtree(self.out)
            load = J.load_shadow

            def without_sha256(*arguments):
                shadow = load(*arguments)
                finish = shadow.finish_judgement

                def finish_judgement(*inner):
                    judgement = finish(*inner)
                    for document in judgement["documents"]:
                        document.pop("sha256")
                    return judgement

                shadow.finish_judgement = finish_judgement
                return shadow

            with mock.patch.object(J, "load_shadow", without_sha256):
                self.assertEqual(self.judge(admission, FakeSandbox(self), read_back_of=J.read_back_with_bun), 0,
                                 self.stderr)
        found = self.outcome("a1")
        self.assertEqual(found["deviation"]["kind"], "shape")
        self.assertIn("sha256", found["deviation"]["reason"])
        unread = json.loads(got(os.path.join(self.out, "requests", ask_id("a1"), STORE.UNREAD_FILE)))
        self.assertNotIn("sha256", unread["judgement"]["documents"][0])

    def test_a_kill_leaves_the_stage_started_and_its_request_started(self):
        admission = self.two_requests()
        with self.assertRaises(Killed):
            self.judge(admission, FakeSandbox(self, raise_on=Killed()))
        self.assertEqual(self.record()["state"], "started")
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a2")])
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")
        self.assertEqual(self.outcome("a2")["verdict"], "approve")

    def test_an_interrupt_or_a_sigterm_closes_nothing_and_a_resume_judges_the_rest(self):
        for name, sandbox in (
                ("an interrupt", lambda: FakeSandbox(self, raise_on=KeyboardInterrupt())),
                ("a SIGTERM", lambda: FakeSandbox(self, during=lambda: os.kill(os.getpid(), signal.SIGTERM)))):
            with self.subTest(name=name):
                admission = self.two_requests()
                self.assertEqual(self.judge(admission, sandbox()), 2)
                record = self.record()
                self.assertEqual(record["state"], "started")
                self.assertEqual(len(record["stops"]), 1)
                self.assertFalse(self.has_outcome("a1"))
                self.assertFalse(self.has_outcome("a2"))
                self.assertFalse(os.path.lexists(os.path.join(self.work, "bundles")))
                resumed = FakeSandbox(self)
                self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
                self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a2")])
                self.assertEqual([line["deviation"] for line in self.outcomes()], ["started", None])
                shutil.rmtree(self.out)

    def test_a_stop_during_setup_closes_nothing(self):
        admission = self.two_requests()
        with mock.patch.object(J, "place_skill", side_effect=KeyboardInterrupt):
            self.assertEqual(self.judge(admission, FakeSandbox(self)), 2)
        self.assertEqual(self.record()["state"], "started")
        self.assertFalse(self.has_outcome("a1"))
        self.assertFalse(os.path.lexists(os.path.join(self.work, "bundles")))
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a1"), ask_id("a2")])

    def test_a_stop_while_s10_builds_the_bundles_leaves_none_outside_out_and_the_request_started(self):
        admission = self.one_request()
        with mock.patch.object(J, "built_bundles", side_effect=KeyboardInterrupt):
            self.assertEqual(self.judge(admission, FakeSandbox(self)), 2)
        self.assertFalse(os.path.lexists(os.path.join(self.work, "bundles")))
        self.assertEqual(sorted(os.listdir(os.path.join(self.out, "requests", ask_id("a1")))), [STORE.STARTED_FILE])
        self.assertEqual(self.judge(admission, FakeSandbox(self)), 0, self.stderr)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")

    def test_a_resume_removes_bundles_no_record_kept_and_never_judges_their_request(self):
        admission = self.two_requests()
        with self.assertRaises(Killed):
            with mock.patch.object(J, "built_bundles", side_effect=Killed):
                self.judge(admission, FakeSandbox(self))
        left = os.path.join(self.out, STORE.BUILDING_DIRECTORY, ask_id("a1"), "judge-1.md")
        os.makedirs(os.path.dirname(left), exist_ok=True)
        put(left, "a bundle the run was writing")
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a2")])
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")
        self.assertFalse(os.path.exists(left))

    def test_another_runs_out_is_refused(self):
        admission = self.one_request()
        self.judge(admission, FakeSandbox(self))
        other = dict(admission, claimRun="start-2")
        self.assertEqual(self.judge(other, FakeSandbox(self)), 2)
        self.assertIn("another run's judging", self.stderr)

    def test_a_resume_takes_only_the_files_its_record_names(self):
        admission = self.one_request()
        self.judge(admission, FakeSandbox(self))
        stray = os.path.join(self.out, "requests", "stray.json")
        put(stray, "{}")
        self.assertEqual(self.judge(admission, FakeSandbox(self)), 2)
        self.assertIn("does not name", self.stderr)
        os.remove(stray)
        outcome = os.path.join(self.out, "requests", ask_id("a1"), STORE.OUTCOME_FILE)
        put(outcome, got(outcome) + b" ")
        self.assertEqual(self.judge(admission, FakeSandbox(self)), 2)
        self.assertIn("is not the file the stage record names", self.stderr)

    def test_an_out_with_files_and_no_record_is_refused(self):
        os.makedirs(self.out)
        put(os.path.join(self.out, "left.txt"), "left")
        self.assertEqual(self.judge(self.one_request(), FakeSandbox(self)), 2)
        self.assertIn("no s10-judged.json", self.stderr)

    def test_a_work_bundles_that_is_no_link_into_out_is_refused_before_anything_is_written(self):
        os.makedirs(os.path.join(self.work, "bundles"))
        self.assertEqual(self.judge(self.one_request(), FakeSandbox(self)), 2)
        self.assertIn("is there and is no link", self.stderr)
        self.assertFalse(os.path.exists(self.out))

    def test_a_refused_admission_writes_nothing(self):
        def refuse(argv, environ):
            raise STORE.CannotRun("claim.ts registers no read under registration-1")

        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            status = J.main(self.argv(), pins=self.pins, admit=refuse, runner_of=lambda image, account, environ: None)
        self.assertEqual(status, 2)
        self.assertIn("registers no read", stderr.getvalue())
        self.assertFalse(os.path.exists(self.out))

    def test_an_image_by_no_digest_or_a_service_account_by_no_name_is_refused(self):
        for more, wanted in (({"image": "registry.example.test/sandbox:latest"}, "by its digest"),
                             ({"more": ("--service-account", "No Name")}, "ServiceAccount's name")):
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr):
                status = J.main(self.argv(**more), pins=self.pins, admit=lambda argv, environ: self.fail("admitted"))
            self.assertEqual(status, 2)
            self.assertIn(wanted, stderr.getvalue())

    def test_inputs_the_typescript_parsers_read_are_judged(self):
        fixture = os.environ.get("S10_INPUTS_FIXTURE")
        if not fixture:
            self.skipTest("s10-judge.test.ts writes this fixture through s10-inputs-io.ts's shapes")
        with open(os.path.join(fixture, "admission.json")) as handle:
            admission = dict(json.load(handle), out=self.out, claimDirectory=self.root)
        self.assertEqual(self.judge(admission, FakeSandbox(self)), 0, self.stderr)
        self.assertEqual([line["outcome"] for line in self.outcomes()], ["verdict", "deviation"])


# ---------- the sandbox's own checks ----------

LAYOUT = {"key": "/run/model-gateway-key", "models": SANDBOX.MODELS_FILE, "overlay": SANDBOX.OVERLAY_FILE,
          "work": "/work"}
MOUNTS = ["/", "/proc", "/dev", "/dev/pts", "/dev/mqueue", "/sys", "/sys/fs/cgroup", "/work", "/etc/hosts",
          "/dev/termination-log", "/etc/hostname", "/etc/resolv.conf", "/dev/shm", "/run/model-gateway-key",
          SANDBOX.OVERLAY_FILE, SANDBOX.MODELS_FILE, "/proc/bus", "/proc/kcore", "/sys/firmware"]
PAYLOAD = {"/work/shadow/bundles/a/judge-1.md": b"one", SANDBOX.CALLS_FILE: b"[]"}
LISTING = ["d 755 0 /work", "d 755 0 /work/s10", "f 644 2 /work/s10/calls.json", "d 755 0 /work/shadow",
           "d 755 0 /work/shadow/bundles", "d 755 0 /work/shadow/bundles/a", "f 644 3 /work/shadow/bundles/a/judge-1.md",
           "d 1777 0 /run/model-gateway-key", "d 755 0 /run/model-gateway-key/..2026_11_20_15_47_29.1",
           "f 644 40 /run/model-gateway-key/..2026_11_20_15_47_29.1/key",
           "l 777 31 /run/model-gateway-key/..data -> ..2026_11_20_15_47_29.1", "l 777 13 /run/model-gateway-key/key -> ..data/key",
           "f 755 100 /opt/legion/bin/agent-secrets", "f 644 10 /opt/legion/pi-envoy/dist/skills/agent-secrets/SKILL.md",
           "d 755 0 /home/legion/.local/state/dispatch", "l 777 3 /usr/lib/git-core/git-credential-store -> git",
           "d 755 0 /run/legion/agent-secrets", "f 644 10 /opt/go/src/go/token/token.go"]


class SandboxTest(unittest.TestCase):
    def test_a_variable_naming_a_credential_is_found_and_the_images_own_pass(self):
        image = ["AWS_CA_BUNDLE", "CGO_ENABLED", "COREPACK_DEFAULT_TO_LATEST", "DO_NOT_TRACK", "HOME", "HOSTNAME",
                 "KUBERNETES_PORT", "KUBERNETES_SERVICE_HOST", "LC_CTYPE", "LEGION_OMP_PATH", "OMP_PROFILE", "PATH",
                 "PI_CODING_AGENT_DIR", "PI_CONFIG_FILES", "PI_PROFILE", "PYTHONDONTWRITEBYTECODE", "S10_SANDBOX",
                 "S10_PAYLOAD_0", "S10_PAYLOAD_SHA256"]
        self.assertEqual(SANDBOX.credential_variables(image), [])
        planted = ["GITHUB_TOKEN", "GH_TOKEN", "DISPATCH_TOKEN_FILE", "DISPATCH_URL", "ENVOY_URL", "AGENT_SECRETS_URL",
                   "OMP_SESSION_SQL_DSN_SECRET"]
        self.assertEqual(SANDBOX.credential_variables(image + planted), sorted(planted))

    def test_a_mount_outside_the_runtime_is_refused(self):
        self.assertIsNone(SANDBOX.mount_refusal(MOUNTS, LAYOUT))
        self.assertIn("/run/secrets/kubernetes.io/serviceaccount",
                      SANDBOX.mount_refusal(MOUNTS + ["/run/secrets/kubernetes.io/serviceaccount"], LAYOUT))
        self.assertIn("not mounted", SANDBOX.mount_refusal([point for point in MOUNTS if point != "/work"], LAYOUT))

    def test_a_file_outside_the_bundles_or_naming_a_credential_is_refused(self):
        self.assertIsNone(SANDBOX.listing_refusal(LISTING, PAYLOAD, LAYOUT))
        cases = {"f 600 10 /home/legion/.config/gh/hosts.yml": "credential",
                 "f 600 10 /run/secrets/session/DISPATCH_TOKEN": "credential",
                 "f 600 10 /home/legion/.git-credentials": "credential",
                 "f 600 10 /home/legion/.config/dispatch/token": "credential",
                 "f 600 10 /home/legion/github_token": "credential",
                 "f 600 10 /run/agent-secrets/key": "credential",
                 "f 644 10 /work/shadow/notes.txt": "outside the bundles",
                 "f 644 10 /run/model-gateway-key/..2026_11_20_15_47_29.1/token": "one item",
                 "f 644 10 /opt/legion/examples/judge-1.md": "evidence bundle's name",
                 "f 644 10 /tmp/JUDGE-3.MD": "evidence bundle's name",
                 "l 777 9 /usr/share/judge-2.md -> /dev/null": "evidence bundle's name"}
        for line, reason in cases.items():
            with self.subTest(line=line):
                self.assertIn(reason, SANDBOX.listing_refusal(LISTING + [line], PAYLOAD, LAYOUT))
        self.assertIn("not there", SANDBOX.listing_refusal([one for one in LISTING if "judge-1" not in one],
                                                           PAYLOAD, LAYOUT))

    def test_a_bundle_is_held_by_its_name_and_its_bytes_and_a_bundles_path_in_the_image_is_none(self):
        image = ["d 755 0 /usr/lib/node_modules/x/bundles", "f 644 10 /usr/lib/node_modules/x/bundles/a/judge.js",
                 "f 644 10 /opt/legion/bundles/index.md", "f 644 10 /usr/share/judge-6.md"]
        self.assertIsNone(SANDBOX.listing_refusal(LISTING + image, PAYLOAD, LAYOUT))
        same = "f 644 3 /usr/share/doc/same-size.txt"
        self.assertIsNone(SANDBOX.listing_refusal(LISTING + [same], PAYLOAD, LAYOUT,
                                                  {"/usr/share/doc/same-size.txt": sha(b"two")}))
        # One the program cannot read, as no judge running as its user can.
        self.assertIsNone(SANDBOX.listing_refusal(LISTING + [same], PAYLOAD, LAYOUT, {"/usr/share/doc/same-size.txt": None}))
        self.assertIn("holds the bytes", SANDBOX.listing_refusal(LISTING + [same], PAYLOAD, LAYOUT,
                                                                  {"/usr/share/doc/same-size.txt": sha(b"one")}))
        self.assertIn("recorded no sha256", SANDBOX.listing_refusal(LISTING + [same], PAYLOAD, LAYOUT, {}))
        # The sandbox's own digests: every file of a sent bundle's size outside the work tree.
        with tempfile.TemporaryDirectory() as root:
            put(os.path.join(root, "copy.txt"), b"one")
            put(os.path.join(root, "other.txt"), b"four")
            lines = [f"f 644 3 {root}/copy.txt", f"f 644 4 {root}/other.txt", f"f 644 3 {root}/gone.txt",
                     "f 644 3 /work/shadow/bundles/a/judge-1.md"]
            self.assertEqual(SANDBOX.same_size_digests(lines, PAYLOAD, dict(LAYOUT, work="/work/shadow")),
                             {f"{root}/copy.txt": sha(b"one"), f"{root}/gone.txt": None})

    def test_a_root_a_run_can_write_holds_only_its_own_the_mounts_and_the_images_runtime(self):
        allowed = ["f 600 10 /home/legion/.omp/profiles/legion/agent/agent.db", f"f 444 10 {SANDBOX.MODELS_FILE}",
                   "f 600 10 /tmp/node-compile-cache/v24/0c8afd4a", "d 1777 0 /tmp", "d 700 0 /root",
                   "e 0 0 /root (Permission denied)", "l 777 8 /run/shm -> /dev/shm"]
        self.assertIsNone(SANDBOX.listing_refusal(LISTING + allowed, PAYLOAD, LAYOUT))
        cases = {"f 600 40 /tmp/corpus.jsonl": "a claim stage writes",
                 "f 644 40 /opt/data/draw.json": "a claim stage writes",
                 f"f 644 40 /opt/data/requests/{ask_id('a1')}.json": "a claim stage writes",
                 f"f 644 40 /usr/share/pages/{'f' * 64}.json": "a claim stage writes",
                 "f 600 40 /home/legion/held-answer.txt": "a root a run can write",
                 "f 600 40 /var/tmp/notes": "a root a run can write",
                 "l 777 10 /home/legion/x -> /opt/data": "a root a run can write"}
        for line, reason in cases.items():
            with self.subTest(line=line):
                self.assertIn(reason, SANDBOX.listing_refusal(LISTING + [line], PAYLOAD, LAYOUT))

    def test_a_runtime_other_than_point_twos_is_refused(self):
        measured = dict(SANDBOX.RUNTIME, piConfigFiles=SANDBOX.OVERLAY_FILE)
        self.assertIsNone(SANDBOX.runtime_refusal(measured))
        for name in SANDBOX.RUNTIME:
            with self.subTest(name=name):
                self.assertIn(name, SANDBOX.runtime_refusal(dict(measured, **{name: "0" * 64})))
        self.assertIn("PI_CONFIG_FILES", SANDBOX.runtime_refusal(dict(measured, piConfigFiles="/tmp/other.yml")))

    def test_the_metadata_servers_must_each_fail_or_refuse_a_request_with_their_header(self):
        urls = SANDBOX.METADATA_REQUESTS
        self.assertEqual(len({url.split("/computeMetadata")[0] for url in urls}), 2)
        self.assertIsNone(SANDBOX.metadata_refusal([
            {"url": url, "connected": not index % 2, "status": None if index % 2 else 404}
            for index, url in enumerate(urls)]))
        for status in (200, 302, 500):
            answers = [{"url": url, "connected": True, "status": 403} for url in urls]
            answers[-1]["status"] = status
            with self.subTest(status=status):
                self.assertIn(str(status), SANDBOX.metadata_refusal(answers))
        silent = [{"url": url, "connected": True, "status": 403} for url in urls]
        silent[1] = {"url": urls[1], "connected": True, "status": None, "error": "TimeoutError"}
        self.assertIn("no proof", SANDBOX.metadata_refusal(silent))
        self.assertIn("not each asked", SANDBOX.metadata_refusal([{"url": urls[0], "status": 403}]))
        self.assertIn("not each asked", SANDBOX.metadata_refusal(None))

    def test_the_metadata_probe_tells_no_connection_from_no_answer(self):
        seen = []
        listeners = []

        def server(answers):
            listener = socket.socket()
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            listeners.append(listener)

            def loop():
                held = []
                while True:
                    try:
                        connection, _ = listener.accept()
                    except OSError:
                        return
                    held.append(connection)
                    seen.append(connection.recv(65536))
                    if answers:
                        connection.sendall(b"HTTP/1.0 404 Not Found\r\nContent-Length: 0\r\n\r\n")
                        connection.close()

            threading.Thread(target=loop, daemon=True).start()
            return f"http://127.0.0.1:{listener.getsockname()[1]}"

        # A port held bound and never listening refuses every connection, and stays held for the test.
        closed = socket.socket()
        closed.bind(("127.0.0.1", 0))
        refused = f"http://127.0.0.1:{closed.getsockname()[1]}"
        silent = server(False)
        original = http.client.HTTPConnection.getresponse

        def getresponse(connection):
            # The silent server takes the request and never answers: its wait ends as the
            # probe's timeout ends it, at once rather than after the seconds it gives.
            if f"http://{connection.host}:{connection.port}" == silent:
                raise TimeoutError("timed out")
            return original(connection)

        try:
            urls = (f"{server(True)}/computeMetadata/v1/token", f"{refused}/computeMetadata/v1/token",
                    f"{silent}/computeMetadata/v1/token")
            with mock.patch.object(SANDBOX, "METADATA_REQUESTS", urls), \
                    mock.patch.object(http.client.HTTPConnection, "getresponse", getresponse):
                asked = SANDBOX.ask_metadata()
                self.assertEqual([(one["connected"], one["status"], one.get("error")) for one in asked],
                                 [(True, 404, None), (False, None, "ConnectionRefusedError"),
                                  (True, None, "TimeoutError")])
                self.assertIn("no proof", SANDBOX.metadata_refusal(asked))
                self.assertIsNone(SANDBOX.metadata_refusal(asked[:2] + [asked[1]]))
        finally:
            closed.close()
            for listener in listeners:
                listener.close()
        self.assertTrue(seen and all(b"Metadata-Flavor: Google" in one for one in seen))

    def test_the_gateway_key_is_redacted_from_every_file(self):
        files = {"a": b"x " + KEY + b" y " + KEY, "b": b"nothing"}
        redacted, held = SANDBOX.redact_key(files, KEY)
        self.assertEqual(held, ["a"])
        self.assertNotIn(KEY, redacted["a"])
        self.assertEqual(redacted["a"].count(SANDBOX.KEY_REDACTION), 2)
        self.assertEqual(SANDBOX.redact_key(files, None), (files, []))

    def test_the_result_travels_whole_or_is_refused(self):
        files = {"record.json": b"{}", "sessions/top.jsonl": b"x" * 200_000}
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            SANDBOX.print_result(files)
        log = "some line omp printed\n" + printed.getvalue()
        self.assertEqual(JOB.result_of(log), files)
        lines = log.splitlines()
        with self.assertRaisesRegex(ValueError, "whole"):
            JOB.result_of("\n".join(line for line in lines if not line.startswith(SANDBOX.RESULT_LINE + " 0 ")))
        end = next(line for line in lines if line.startswith(SANDBOX.RESULT_END))
        with self.assertRaisesRegex(ValueError, "sha256"):
            JOB.result_of(log.replace(end, end[:-1] + ("0" if end[-1] != "0" else "1")))
        with self.assertRaises(ValueError):
            SANDBOX.unpack(SANDBOX.pack({"../out": b"x"}))

    def test_the_payload_travels_in_variables_each_under_the_limit_and_is_checked(self):
        files = {f"/work/shadow/bundles/a/judge-{index}.md": os.urandom(150_000) for index in range(1, 3)}
        variables = JOB.payload_variables(files)
        self.assertTrue(all(len(name) + 1 + len(value) < 128 * 1024 for name, value in variables.items()))
        self.assertEqual(SANDBOX.payload_of(variables), files)
        with self.assertRaisesRegex(ValueError, "sha256"):
            SANDBOX.payload_of(dict(variables, S10_PAYLOAD_SHA256="0" * 64))
        with self.assertRaisesRegex(ValueError, "emptyDir"):
            SANDBOX.place({"/etc/judge-1.md": b"x"}, LAYOUT)

    def test_the_mode_is_named_and_must_match_the_payload(self):
        files = {"/work/shadow/bundles/a/judge-1.md": b"one"}
        cases = {"no mode": ({}, files, "none of"), "another mode": ({SANDBOX.MODE_VARIABLE: "judge"}, files, "none of"),
                 "a preflight sent files": ({SANDBOX.MODE_VARIABLE: "preflight"}, files, "a preflight was sent 1"),
                 "a request sent none": ({SANDBOX.MODE_VARIABLE: "request"}, {}, "a request was sent 0")}
        for name, (mode, sent, reason) in cases.items():
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, reason):
                SANDBOX.checked({**mode, **JOB.payload_variables(sent)}, LAYOUT, {})

    def test_the_listing_names_every_entry_without_following_a_link(self):
        with tempfile.TemporaryDirectory() as root:
            os.makedirs(os.path.join(root, "a"))
            put(os.path.join(root, "a", "file"), "12345")
            os.symlink("/etc", os.path.join(root, "link"))
            lines = SANDBOX.listing_of(root)
        self.assertEqual([line.split(" ")[0] for line in lines], ["d", "f", "l"])
        self.assertTrue(lines[1].endswith(" 5 " + os.path.join(root, "a", "file")))
        self.assertTrue(lines[2].endswith(" -> /etc"))

    def test_the_manifests_are_recursive_relative_path_and_type_lists(self):
        with tempfile.TemporaryDirectory() as root:
            os.makedirs(os.path.join(root, "references"))
            for name in SANDBOX.SKILL_FILES:
                put(os.path.join(root, name), "x")
            self.assertEqual(SANDBOX.manifest_of(root), SANDBOX.SKILL_MANIFEST)
            os.symlink("/etc/passwd", os.path.join(root, "references", "more.md"))
            self.assertIn(["references/more.md", "l"], SANDBOX.manifest_of(root))
            self.assertIsNone(SANDBOX.manifest_of(os.path.join(root, "SKILL.md")))
        with tempfile.TemporaryDirectory() as root:
            os.makedirs(os.path.join(root, "bundles", "a1x"))
            for name in SANDBOX.BUNDLE_NAMES:
                put(os.path.join(root, "bundles", "a1x", name), "x")
            self.assertEqual(SANDBOX.manifest_of(root), SANDBOX.work_manifest("a1x"))
        wanted = {"work": SANDBOX.work_manifest("a1x"), "skill": SANDBOX.SKILL_MANIFEST}
        self.assertEqual(wanted["work"][:2], [["bundles", "d"], ["bundles/a1x", "d"]])
        self.assertIsNone(SANDBOX.manifest_problem({"before": wanted}, "a1x", finished=False))
        self.assertIsNone(SANDBOX.manifest_problem({"before": wanted, "after": wanted}, "a1x"))
        self.assertIn("after the last judge", SANDBOX.manifest_problem({"before": wanted}, "a1x"))
        self.assertIn("work tree", SANDBOX.manifest_problem({"before": wanted}, "a2x"))
        self.assertIn("work tree", SANDBOX.manifest_problem({"before": wanted}, None, finished=False))
        self.assertIn("work tree", SANDBOX.manifest_problem({"before": dict(wanted, work=None)}, "a1x", finished=False))
        self.assertIn("skill tree", SANDBOX.manifest_problem({"before": dict(wanted, skill=wanted["skill"][:3])}, "a1x"))
        self.assertIn("after the last judge",
                      SANDBOX.manifest_problem({"before": wanted, "after": dict(wanted, skill=[])}, "a1x"))
        # Versions 4 and 5: the bundle directory, taken from the work tree's manifest, and the skill tree.
        stray = dict(wanted, work=sorted(wanted["work"] + [["notes.txt", "f"]]))
        self.assertEqual(SANDBOX.bundle_manifest_of(stray["work"], "a1x"), [[name, "f"] for name in SANDBOX.BUNDLE_NAMES])
        self.assertIsNone(SANDBOX.manifest_problem({"before": stray, "after": wanted}, "a1x", trees="bundle"))
        self.assertIn("work tree", SANDBOX.manifest_problem({"before": stray, "after": stray}, "a1x"))
        self.assertIn("bundle directory", SANDBOX.manifest_problem({"before": wanted}, "a2x", finished=False,
                                                                   trees="bundle"))
        sixth = dict(wanted, work=sorted(wanted["work"] + [["bundles/a1x/judge-6.md", "f"]]))
        self.assertIn("bundle directory", SANDBOX.manifest_problem({"before": sixth, "after": sixth}, "a1x",
                                                                   trees="bundle"))
        self.assertIn("after the last judge", SANDBOX.manifest_problem({"before": wanted, "after": sixth}, "a1x",
                                                                       trees="bundle"))

    def test_the_one_cell_makes_the_recorded_calls_and_waits_as_s10_waits(self):
        calls = [{"prompt": f"p{index}", "agent": "scout", "label": f"l{index}", "schema": {"type": "object"},
                  "model": "m"} for index in range(5)]
        made, waited, written = [], [], {}

        class Handle:
            def __init__(self, index):
                self.id = f"agent{index}"

        def agent(prompt, **options):
            made.append({"prompt": prompt, **options})
            return Handle(len(made))

        def wait(handles, **options):
            waited.append(options)
            return [{"verdict": "approve"} for _ in handles]

        def opened(path, mode="r"):
            if path == SANDBOX.CALLS_FILE:
                return io.StringIO(json.dumps(calls))
            buffer = io.StringIO()
            buffer.close = lambda: written.__setitem__(path, buffer.getvalue())
            return buffer

        with contextlib.redirect_stdout(io.StringIO()):
            exec(compile(SANDBOX.CELL, "cell", "exec"), {"agent": agent, "wait": wait, "open": opened})
        self.assertEqual(made, calls)
        self.assertEqual(waited, [{"timeout": 1500, "raise_errors": False}])
        self.assertIn(SANDBOX.CELL, SANDBOX.PROMPT)


# ---------- the sandbox Job ----------

def own_pod():
    """A pod spec of the shape the driver copies from: more volumes and variables than it takes."""
    return {"metadata": {"namespace": "sessions", "name": "session-1"},
            "spec": {"serviceAccountName": "session", "securityContext": {"runAsUser": 1000, "runAsNonRoot": True},
                     "runtimeClassName": "gvisor",
                     "tolerations": [{"key": "kubernetes.io/arch", "value": "amd64", "effect": "NoSchedule"}],
                     "volumes": [{"name": "secrets", "secret": {"secretName": "session-secrets"}},
                                 {"name": "token", "projected": {"sources": [{"serviceAccountToken": {"path": "token"}}]}},
                                 {"name": "route", "configMap": {"name": "route-map", "items": [
                                     {"key": "models.yml", "path": "models.yml"}, {"key": "overlay.yml", "path": "overlay.yml"},
                                     {"key": "extra.md", "path": "extra.md"}]}},
                                 {"name": "gateway", "secret": {"secretName": "session-secrets", "defaultMode": 288,
                                                                "items": [{"key": "GATEWAY_KEY", "path": "key"}]}},
                                 {"name": "work", "ephemeral": {}}],
                     "containers": [{"name": "session", "image": "registry.example.test/session@sha256:" + "c" * 64,
                                     "securityContext": {"allowPrivilegeEscalation": False},
                                     "resources": {"limits": {"memory": "8Gi"}},
                                     "env": [{"name": "SESSION_TOKEN", "valueFrom": {"secretKeyRef": {"name": "s"}}},
                                             {"name": "PI_CONFIG_FILES", "value": SANDBOX.OVERLAY_FILE},
                                             {"name": "FORGE_URL", "value": "http://forge.example.test"}],
                                     "volumeMounts": [
                                         {"name": "secrets", "mountPath": "/var/run/secrets/session"},
                                         {"name": "token", "mountPath": "/var/run/secrets/token"},
                                         {"name": "route", "mountPath": SANDBOX.MODELS_FILE, "subPath": "models.yml"},
                                         {"name": "route", "mountPath": SANDBOX.OVERLAY_FILE, "subPath": "overlay.yml"},
                                         {"name": "gateway", "mountPath": SANDBOX.KEY_MOUNT},
                                         {"name": "work", "mountPath": "/work"}]}]}}


class JobSpecTest(unittest.TestCase):
    environ = {"OMP_PROFILE": "legion", "PI_PROFILE": "legion", "PI_CODING_AGENT_DIR": "/home/legion/agent",
               "HOME": "/home/legion", "SESSION_TOKEN": "secret"}

    def test_the_job_holds_the_three_mounts_beside_an_emptydir_and_no_credential(self):
        spec = JOB.job_spec("s10-judge-1", own_pod(), IMAGE, JOB.SERVICE_ACCOUNT,
                            {"S10_SANDBOX": "program", "S10_PAYLOAD_0": "x"}, self.environ)
        pod = spec["spec"]["template"]["spec"]
        self.assertEqual((spec["metadata"]["name"], spec["metadata"]["namespace"]), ("s10-judge-1", "sessions"))
        self.assertEqual((pod["serviceAccountName"], pod["automountServiceAccountToken"], pod["enableServiceLinks"]),
                         ("s10-judge-sandbox", False, False))
        self.assertEqual(spec["spec"]["template"]["metadata"]["labels"], {"app.kubernetes.io/name": "s10-judge-sandbox"})
        self.assertEqual(pod["runtimeClassName"], "gvisor")
        [container] = pod["containers"]
        self.assertEqual(container["image"], IMAGE)
        self.assertEqual([(one["mountPath"], one.get("subPath")) for one in container["volumeMounts"]],
                         [(SANDBOX.KEY_MOUNT, None), (SANDBOX.MODELS_FILE, "models.yml"),
                          (SANDBOX.OVERLAY_FILE, "overlay.yml"), ("/work", None)])
        volumes = {one["name"]: one for one in pod["volumes"]}
        self.assertEqual(volumes["gateway-key"]["secret"],
                         {"secretName": "session-secrets", "items": [{"key": "GATEWAY_KEY", "path": "key"}],
                          "defaultMode": 288})
        self.assertEqual(volumes["route"]["configMap"],
                         {"name": "s10-judge-route",
                          "items": [{"key": "models.yml", "path": "models.yml"}, {"key": "overlay.yml", "path": "overlay.yml"}]})
        self.assertEqual(volumes["work"], {"name": "work", "emptyDir": {}})
        self.assertEqual(len(volumes), 3)
        self.assertEqual([one["name"] for one in container["env"]],
                         [*JOB.PROFILE_VARIABLES, "PYTHONDONTWRITEBYTECODE", "S10_PAYLOAD_0", "S10_SANDBOX"])
        self.assertTrue(all("valueFrom" not in one for one in container["env"]))
        self.assertEqual(SANDBOX.credential_variables([one["name"] for one in container["env"]]), [])
        self.assertEqual(container["command"], ["python3", "-c", JOB.BOOT])
        self.assertEqual(pod["tolerations"], own_pod()["spec"]["tolerations"])
        kept = JOB.redacted(spec)["spec"]["template"]["spec"]["containers"][0]["env"]
        self.assertEqual(next(one["value"] for one in kept if one["name"] == "S10_SANDBOX"), "sha256:" + sha("program"))
        other = JOB.job_spec("s10-judge-1", own_pod(), IMAGE, "session-account", {}, self.environ)
        self.assertEqual(other["spec"]["template"]["spec"]["serviceAccountName"], "session-account")

    def test_the_route_is_point_twos_whatever_route_the_driver_mounts(self):
        # The driver's own route ConfigMap holds Legion's live route as of the session's last
        # apply, which can be another than point 2's: the Job names s10-judge-route, whose bytes
        # `runtime_refusal` checks, and never the driver's, even where the driver mounts none.
        unrouted = own_pod()
        unrouted["spec"]["volumes"] = [one for one in unrouted["spec"]["volumes"] if one["name"] != "route"]
        container = unrouted["spec"]["containers"][0]
        container["volumeMounts"] = [one for one in container["volumeMounts"] if one["name"] != "route"]
        for pod in (own_pod(), unrouted):
            spec = JOB.job_spec("s10-judge-1", pod, IMAGE, JOB.SERVICE_ACCOUNT, {}, self.environ)
            volumes = {one["name"]: one for one in spec["spec"]["template"]["spec"]["volumes"]}
            self.assertEqual(volumes["route"]["configMap"]["name"], "s10-judge-route")
            self.assertNotIn("route-map", json.dumps(spec))

    def test_a_pod_whose_gateway_key_is_not_one_item_is_refused(self):
        pod = own_pod()
        pod["spec"]["volumes"][3]["secret"]["items"].append({"key": "OTHER", "path": "other"})
        with self.assertRaisesRegex(STORE.CannotRun, "one item"):
            JOB.job_spec("s10-judge-1", pod, IMAGE, JOB.SERVICE_ACCOUNT, {}, self.environ)
        pod["spec"]["containers"][0]["volumeMounts"].pop(4)
        with self.assertRaisesRegex(STORE.CannotRun, "no gateway key"):
            JOB.job_spec("s10-judge-1", pod, IMAGE, JOB.SERVICE_ACCOUNT, {}, self.environ)

    def test_the_sandbox_runs_only_the_image_its_driver_runs(self):
        pod = own_pod()
        image = pod["spec"]["containers"][0]["image"]
        pod["status"] = {"containerStatuses": [{"name": "session", "imageID": image}]}
        self.assertIsNone(JOB.image_refusal(pod, image))
        self.assertIn("this driver's own pod runs", JOB.image_refusal(pod, IMAGE))
        pod["status"]["containerStatuses"][0]["imageID"] = "registry.example.test/session@sha256:" + "d" * 64
        self.assertIn("the sandbox runs only the image its driver runs", JOB.image_refusal(pod, image))

    def test_the_sandbox_never_runs_as_the_drivers_own_account(self):
        self.assertIn("this driver's own pod runs as", JOB.account_refusal(own_pod(), "session"))
        self.assertIsNone(JOB.account_refusal(own_pod(), JOB.SERVICE_ACCOUNT))
        pod = own_pod()
        del pod["spec"]["serviceAccountName"]
        self.assertIn("own pod runs as", JOB.account_refusal(pod, "default"))


class ScriptedRunner(JOB.KubeRunner):
    """`KubeRunner`, built by its own constructor, over a stand-in `kubectl`: each call answered from
    `script` by its verb, the driver's own pod (`get pod`) the one the constructor reads, running the
    image the runner is given unless `image` names another, under `account` where one is named.
    `calls` holds the verbs called after the constructor's; a list handed in as `calls` holds every
    verb, the constructor's too, which a constructor that refuses leaves for the caller to read."""

    def __init__(self, script, namespace_file, image=None, account=JOB.SERVICE_ACCOUNT, calls=None):
        pod = own_pod()
        container = pod["spec"]["containers"][0]
        pod["status"] = {"containerStatuses": [{"name": container["name"], "imageID": container["image"]}]}
        self.script = {"get pod": [(0, json.dumps(pod))], **script}
        self.calls = [] if calls is None else calls
        with mock.patch.object(JOB, "NAMESPACE_FILE", namespace_file), \
                mock.patch.object(SANDBOX, "gateway_key", return_value=None):
            super().__init__(image or container["image"], account, {}, poll=0, pauses=(0, 0))
        self.constructed = list(self.calls)
        self.calls.clear()

    def invoke(self, arguments, stdin):
        verb = arguments[0] if arguments[0] != "get" else f"get {arguments[1]}"
        self.calls.append(verb)
        answers = self.script.get(verb) or [(0, "")]
        code, out = answers.pop(0) if len(answers) > 1 else answers[0]
        return subprocess.CompletedProcess(arguments, code, out if code == 0 else "", "" if code == 0 else out)


class KubeRunnerTest(unittest.TestCase):
    """The runner over a stand-in `kubectl` (`ScriptedRunner`)."""

    def setUp(self):
        root = tempfile.mkdtemp(prefix="s10-runner-")
        self.addCleanup(shutil.rmtree, root, ignore_errors=True)
        self.namespace_file = os.path.join(root, "namespace")
        put(self.namespace_file, "sessions\n")

    def runner(self, script):
        return ScriptedRunner(script, self.namespace_file)

    def test_the_constructor_reads_the_drivers_own_pod_and_namespace(self):
        runner = self.runner({})
        self.assertEqual(runner.constructed, ["get pod"])
        self.assertEqual((runner.namespace, runner.own_pod()["metadata"]["name"]), ("sessions", "session-1"))
        self.assertEqual(runner.calls, [])

    def test_the_constructor_refuses_another_image_and_the_drivers_own_account_before_any_call(self):
        cases = {
            "another image": ({"image": IMAGE}, "the sandbox runs only the image its driver runs"),
            "the driver's own account": ({"account": "session"}, "the account this driver's own pod runs as"),
        }
        for name, (given, refusal) in cases.items():
            with self.subTest(name=name):
                calls = []
                with self.assertRaisesRegex(STORE.CannotRun, refusal):
                    ScriptedRunner({}, self.namespace_file, calls=calls, **given)
                # The constructor read its own pod and called nothing past it.
                self.assertEqual(calls, ["get pod"])

    def pods(self):
        return json.dumps({"items": [{"metadata": {"name": "pod-1"}, "status": {
            "phase": "Succeeded", "containerStatuses": [{"imageID": IMAGE, "state": {"terminated": {"exitCode": 0}}}]}}]})

    def log(self, files):
        text = SANDBOX.pack(files)
        return f"{SANDBOX.RESULT_LINE} 0 {text}\n{SANDBOX.RESULT_END} 1 {SANDBOX.sha256_of(text)}\n"

    def test_a_transient_api_failure_is_tried_again_and_passes(self):
        runner = self.runner({"create": [(1, "Unable to connect to the server"), (1, "AlreadyExists"), (0, "")],
                              "get job": [(1, "i/o timeout"), (0, json.dumps({"status": {"succeeded": 1}}))],
                              "get pods": [(0, self.pods())], "logs": [(0, self.log({"record.json": b"{}"}))]})
        run = runner.run("a1", {"/work/s10/calls.json": b"[]"})
        self.assertIsNone(run.failure)
        self.assertEqual(run.files, {"record.json": b"{}"})
        self.assertEqual(runner.calls, ["create", "create", "get job", "get job", "get pods", "logs", "delete"])

    def test_an_api_failure_past_its_tries_stops_and_the_job_is_deleted(self):
        for verb in ("create", "get job", "get pods", "logs"):
            with self.subTest(verb=verb):
                script = {"get job": [(0, json.dumps({"status": {"succeeded": 1}}))], "get pods": [(0, self.pods())],
                          "logs": [(0, self.log({}))], verb: [(1, "connection refused")]}
                runner = self.runner(script)
                with self.assertRaisesRegex(JOB.KubeUnavailable, f"kubectl {verb.split()[0]} failed 3 times"):
                    runner.run("a1", {})
                self.assertEqual(runner.calls[-1], "delete")
        self.assertEqual(self.runner({"create": [(1, "forbidden")]}).kubectl("create", check=False), "")

    def test_a_job_that_ran_and_gave_no_readable_result_is_the_requests_own(self):
        cases = {"no whole result": [(0, "a log with no result\n")],
                 "a changed result": [(0, self.log({"record.json": b"{}"}).replace(" 0 ", " 0 x", 1))]}
        for name, logs in cases.items():
            with self.subTest(name=name):
                runner = self.runner({"get job": [(0, json.dumps({"status": {"failed": 1}}))],
                                      "get pods": [(0, self.pods())], "logs": logs})
                run = runner.run("a1", {})
                self.assertIsNotNone(run.failure)
        runner = self.runner({"get job": [(0, json.dumps({"status": {"succeeded": 1}}))],
                              "get pods": [(0, json.dumps({"items": []}))]})
        self.assertEqual(runner.run("a1", {}).failure, "the Job ran 0 pods")

    def test_the_preflight_sends_no_file_and_has_a_short_deadline(self):
        created = []
        runner = self.runner({"get job": [(0, json.dumps({"status": {"succeeded": 1}}))],
                              "get pods": [(0, self.pods())], "logs": [(0, self.log({}))]})
        original = runner.invoke

        def invoke(arguments, stdin):
            if arguments[0] == "create":
                created.append(json.loads(stdin))
            return original(arguments, stdin)

        runner.invoke = invoke
        self.assertIsNone(runner.preflight().failure)
        [spec] = created
        self.assertEqual(spec["spec"]["activeDeadlineSeconds"], JOB.PREFLIGHT_DEADLINE)
        env = {one["name"]: one["value"] for one in spec["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(SANDBOX.payload_of(env), {})
        self.assertEqual(env[SANDBOX.MODE_VARIABLE], "preflight")

    def test_a_requests_job_names_its_mode(self):
        created = []
        runner = self.runner({"get job": [(0, json.dumps({"status": {"succeeded": 1}}))],
                              "get pods": [(0, self.pods())], "logs": [(0, self.log({}))]})
        original = runner.invoke

        def invoke(arguments, stdin):
            if arguments[0] == "create":
                created.append(json.loads(stdin))
            return original(arguments, stdin)

        runner.invoke = invoke
        runner.run("a1", {"/work/s10/calls.json": b"[]"})
        env = {one["name"]: one["value"] for one in created[0]["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env[SANDBOX.MODE_VARIABLE], "request")


if __name__ == "__main__":
    unittest.main()
