"""Tests of the claim's judging stage: `s10_judge.py`, the driver, and `s10_sandbox.py`, the program
its sandbox runs. `s10-judge.test.ts` runs them with `python3 -m unittest`.

The frozen code is a stand-in of the same shape, `fixtures/s10-code.txt`, with its own sha256: a
code-txt whose `shadow.py` has the names the driver runs and replaces, and none of the real code's
text, which this public repository may not hold. Its pins are taken from the fixture itself
(`pins_for`). The admission is handed in, as `s10-judge-admit.ts`'s own tests cover it, and the
sandbox is a fake that writes the files a real sandbox's result holds: the record, the listing,
the session's transcript, each judge's transcript in omp's own entries, and the answers.
"""

import sys

sys.dont_write_bytecode = True

import contextlib  # noqa: E402
import hashlib  # noqa: E402
import importlib.util  # noqa: E402
import inspect  # noqa: E402
import io  # noqa: E402
import json  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
import shutil  # noqa: E402
import tempfile  # noqa: E402
import unittest  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import s10_judge as J  # noqa: E402
import s10_sandbox as SANDBOX  # noqa: E402

CODE = os.path.join(HERE, "fixtures", "s10-code.txt")
PERSON = "person@example.test"
SKILL = b"# The skill, as the tests give it\n\nJudge the request.\n"
NEVER = b"# What the agent never decides, as the tests give it\n\nNothing here.\n"
IMAGE = "registry.example.test/sandbox@sha256:" + "a" * 64
HARNESS = "/opt/legion/pi-envoy/dist/skills/approval-review"
OPENED = "2026-11-17T10:00:00.000001Z"
EARLIER = "2026-11-16T09:00:00.000001Z"
LATER = "2026-11-18T09:00:00.000001Z"


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
        with open(source, "wb") as handle:
            handle.write(files["shadow.py"])
        os.environ.update(J.UNREACHABLE)
        spec = importlib.util.spec_from_file_location("s10_pins_shadow", source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        prompt, bundle = sha(module.JUDGE_PROMPT), sha(inspect.getsource(module.build_bundle))
    return J.Pins(code_sha256=sha(text), files={name: sha(data)[:8] for name, data in files.items()},
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


def request_of(tail, issue_key, *, hold=False, fact=None, pages=None, candidates=None):
    """One request file, and the page records its search uses."""
    ask = {"id": ask_id(tail), "kind": "approval", "state": "open", "issue_key": issue_key,
           "question": f"Approve the spec of {issue_key}?",
           "approval": {"artifact_id": f"doc-{tail}", "name": "spec.md", "version": 2},
           "approval_artifact": {"primary": hold}, "created_at": OPENED, "answer": None}
    issue = {"key": issue_key, "title": f"The work of {issue_key}", "status": "backlog" if hold else "todo",
             "labels": ["legion"] if hold else [], "parent": None, "primary_artifact_id": None}
    markdown = f"# The spec of {issue_key}\n"
    computed = {"holds": hold, "legion": hold, "primary": hold, "root": True, "status": issue["status"]}
    request = {"version": 4, "askId": ask_id(tail), "issueKey": issue_key, "group": "report", "cases": 1,
               "judged": {"eventId": 50, "type": "ask.opened", "createdAt": OPENED}, "ask": ask, "issue": issue,
               "issueAsOfEventId": 10,
               "documents": [{"artifact_id": f"doc-{tail}", "name": "spec.md", "version": 2, "markdown": markdown,
                              "sha256": sha(markdown)}],
               "fact": fact or computed, "factHold": (fact or computed)["holds"],
               "search": None, "candidates": None, "counts": None, "dropScanThrough": None}
    records = {}
    if not hold:
        if candidates is None:
            candidates = {ask_id("c1"): candidate("c1"), ask_id("c2"): {"held": True, "reserve": "held"},
                          ask_id("c3"): {"dropped": "later-event", "seen": "log"}}
        hits = pages if pages is not None else [
            [{"id": ask_id("c2"), "kind": "ask", "rank": 0.9}, {"id": ask_id("c3"), "kind": "ask", "rank": 0.8},
             {"id": "ACME-9", "kind": "issue", "rank": 0.7}, {"id": ask_id("c1"), "kind": "ask", "rank": 0.6}]]
        queries = {}
        for name, query in (("title", issue["title"]), ("question", ask["question"])):
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
    return {"reference": "registration-1", "claimRun": "start-1", "runDigest": "d" * 64, "out": out,
            "inputs": {"directory": directory, "stageSha256": "e" * 64, "files": files}}


# ---------- a fake sandbox, writing what a real one's result holds ----------

def answer(verdict="approve", kind="none"):
    return {"never_decide": kind, "never_decide_line": None, "addresses_reviewer": False,
            "injected_instruction": None, "verdict": verdict, "reason": "One sentence."}


def entries_text(entries):
    return ("\n".join(json.dumps(entry) for entry in entries) + "\n").encode()


def tool_entries(calls, upstream=None):
    """omp's entries of each tool call: the assistant's call, its execution's start, its result."""
    entries = []
    for number, (name, arguments, details) in enumerate(calls):
        call_id = f"toolu_{number}"
        entries.append({"type": "message", "message": {
            "role": "assistant", "provider": "anthropic", "model": "claude-opus-5-5", "upstreamModel": upstream,
            "content": [{"type": "thinking", "thinking": "..."},
                        {"type": "toolCall", "id": call_id, "name": name, "arguments": arguments}]}})
        entries.append({"type": "custom", "customType": "tool_execution_start",
                        "data": json.dumps({"toolCallId": call_id, "toolName": name, "args": arguments})})
        entries.append({"type": "message", "message": {"role": "toolResult", "toolCallId": call_id, "toolName": name,
                                                       "isError": False, "details": details, "content": []}})
    return entries


class FakeSandbox:
    """The sandbox, as a fake: `judge_tools`, `answers`, `model_changes`, `served`, `resolved`,
    `top_calls`, `record`, `drop` and `failure` change what it gives back."""

    def __init__(self, test, **changes):
        self.test = test
        self.runs = []
        self.judge_tools = None
        self.answers = None
        self.model_changes = None
        self.served = {}
        self.resolved = {}
        self.top_calls = None
        self.record = {}
        self.drop = set()
        self.failure = None
        self.image_id = IMAGE
        self.raise_on = None
        for name, value in changes.items():
            setattr(self, name, value)

    def run(self, ask, files, extra_env=None):
        calls = json.loads(files[SANDBOX.CALLS_FILE])
        kept = os.path.join(self.test.out, "requests", ask)
        self.runs.append({"ask": ask, "files": files, "calls": calls,
                          "kept": {name: got(os.path.join(kept, name)) for name in sorted(os.listdir(kept))}})
        if self.raise_on is not None:
            raise self.raise_on
        if self.failure is not None:
            return J.SandboxRun(job="fake-job", failure=self.failure)
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
        listing = [f"f 644 10 {HARNESS}/{name}" for name in
                   ("SKILL.md", "references/never-decide.md", "references/reviewer-principles.md")]
        result["listing.txt"] = ("\n".join(listing) + "\n").encode()
        record = {"refused": None, "environment": ["HOME", "PATH", "S10_SANDBOX"], "mounts": ["/", "/work"],
                  "listing": {"lines": len(listing), "sha256": sha("\n".join(listing) + "\n")},
                  "runtime": dict(SANDBOX.RUNTIME, piConfigFiles=SANDBOX.OVERLAY_FILE),
                  "metadata": {"refused": True, "status": 403}, "omp": {"returncode": 0}, **self.record}
        result["record.json"] = json.dumps(record).encode()
        return J.SandboxRun(job=f"fake-job-{len(self.runs)}", files=result,
                            facts={"pod": "fake-pod", "imageId": self.image_id})


class Killed(BaseException):
    """A kill the driver cannot catch, as the pod's end would be."""


class Base(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="s10-judge-")
        self.work = os.path.join(self.root, "shadow")
        self.inputs = os.path.join(self.root, "inputs")
        self.out = os.path.join(self.root, "out")
        self.saved = os.environ.get("S10_TEST_WORK")
        os.environ["S10_TEST_WORK"] = self.work
        self.skill = os.path.join(self.root, "skill", "SKILL.md")
        self.never = os.path.join(self.root, "skill", "references", "never-decide.md")
        os.makedirs(os.path.dirname(self.never))
        put(self.skill, SKILL)
        put(self.never, NEVER)
        self.pins = pins_for(CODE, self.work)

    def tearDown(self):
        if self.saved is None:
            os.environ.pop("S10_TEST_WORK", None)
        else:
            os.environ["S10_TEST_WORK"] = self.saved
        shutil.rmtree(self.root, ignore_errors=True)

    def argv(self, code=CODE, image=IMAGE):
        return ["--inputs", self.inputs, "--out", self.out, "--code", code, "--skill", self.skill,
                "--never-decide", self.never, "--image", image]

    def judge(self, admission, sandbox, pins=None, code=CODE):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            status = J.main(self.argv(code), pins=pins or self.pins, admit=lambda argv, environ: admission,
                            runner_of=lambda image, environ: sandbox)
        self.stderr = stderr.getvalue()
        return status

    def outcome(self, tail):
        with open(os.path.join(self.out, "requests", ask_id(tail), "outcome.json")) as handle:
            return json.load(handle)

    def outcomes(self):
        with open(os.path.join(self.out, J.OUTCOMES_FILE)) as handle:
            return [json.loads(line) for line in handle if line.strip()]

    def record(self):
        with open(os.path.join(self.out, J.STAGE_FILE)) as handle:
            return json.load(handle)

    def one_request(self, **changes):
        return write_inputs(self.inputs, self.out, [request_of("a1", "ACME-1", **changes)])

    def judged_kind(self, **sandbox):
        """The outcome's deviation kind, or its verdict, of one request judged by this sandbox."""
        status = self.judge(self.one_request(), FakeSandbox(self, **sandbox))
        self.assertEqual(status, 0, self.stderr)
        found = self.outcome("a1")
        return found["deviation"]["kind"] if found["deviation"] else found["verdict"]


# ---------- the frozen code ----------

class FrozenCodeTest(Base):
    def test_takes_the_code_txt_whose_every_part_is_pinned(self):
        files = J.frozen_files(CODE, self.pins)
        self.assertEqual(list(files), ["shadow.py", "render.py", "round.py", "kernel_round.py", "tick.sh"])
        path = os.path.join(self.root, "shadow.py")
        put(path, files["shadow.py"])
        shadow = J.load_shadow(path, self.pins)
        self.assertEqual(shadow.WORK, self.work)

    def test_refuses_a_code_txt_whose_text_is_not_the_pinned(self):
        changed = os.path.join(self.root, "code.txt")
        put(changed, got(CODE).replace(b"A test judge.", b"A test judge!"))
        with self.assertRaisesRegex(J.CannotRun, "point 2 fixes"):
            J.frozen_files(changed, self.pins)

    def test_refuses_a_file_a_prompt_or_a_bundle_code_not_the_pinned(self):
        with self.assertRaisesRegex(J.CannotRun, "tick.sh"):
            J.frozen_files(CODE, J.Pins(**{**self.pins.__dict__, "files": {**self.pins.files, "tick.sh": "00000000"}}))
        path = os.path.join(self.root, "shadow.py")
        put(path, J.frozen_files(CODE, self.pins)["shadow.py"])
        for field, name in (("prompt_sha256", "JUDGE_PROMPT"), ("bundle_code_sha256", "build_bundle"),
                            ("asked", "ASKED"), ("work", "WORK")):
            with self.subTest(field=field), self.assertRaisesRegex(J.CannotRun, name):
                J.load_shadow(path, J.Pins(**{**self.pins.__dict__, field: "other"}))

    def test_every_read_it_does_not_replace_raises_before_the_network(self):
        path = os.path.join(self.root, "shadow.py")
        put(path, J.frozen_files(CODE, self.pins)["shadow.py"])
        before = os.environ.get("DISPATCH_URL")
        shadow = J.load_shadow(path, self.pins)
        self.assertEqual(shadow.DISPATCH, (J.UNREACHABLE["DISPATCH_URL"], J.UNREACHABLE["DISPATCH_TOKEN_FILE"]))
        self.assertEqual(os.environ.get("DISPATCH_URL"), before)
        with self.assertRaises(J.InputsGap):
            shadow.configuration()
        with self.assertRaises(J.InputsGap):
            shadow.reserved_neighbour(ask_id("c1"), "ACME-9", EARLIER, [])
        # Its own read stops at the token file that is not there, before any request is made.
        with self.assertRaises(OSError):
            shadow.api("/api/v1/issues/ACME-1")

    def test_puts_the_pinned_skill_where_s10_reads_it_and_refuses_another(self):
        path = os.path.join(self.root, "shadow.py")
        put(path, J.frozen_files(CODE, self.pins)["shadow.py"])
        shadow = J.load_shadow(path, self.pins)
        J.place_skill(shadow, J.skill_files(self.skill, self.never, self.pins))
        self.assertEqual(shadow.skill_bytes()["SKILL.md"], SKILL.decode())
        put(self.never, NEVER + b"\nOne more line.\n")
        with self.assertRaisesRegex(J.CannotRun, "never-decide"):
            J.skill_files(self.skill, self.never, self.pins)
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(self.one_request(), sandbox), 2)
        self.assertFalse(os.path.exists(self.out))


# ---------- point 3's reads ----------

class ReaderTest(Base):
    def shadow_over(self, request, records):
        path = os.path.join(self.root, "shadow.py")
        put(path, J.frozen_files(CODE, self.pins)["shadow.py"])
        shadow = J.load_shadow(path, self.pins)
        admission = write_inputs(self.inputs, self.out, [(request, records)])
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
            with self.subTest(path=path), self.assertRaises(J.InputsGap):
                shadow.api(path)


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
        self.assertFalse(os.path.exists(os.path.join(self.work, "bundles", ask_id("a1"))))
        found = self.outcome("a1")
        self.assertEqual((found["outcome"], found["verdict"], found["deviation"]), ("verdict", "approve", None))
        self.assertEqual(found["judgement"]["votes"], {"approve": 5})
        self.assertEqual(found["judgement"]["neighbour_asks"], [ask_id("c1")])
        self.assertEqual(found["judgement"]["judge_models"],
                         [{"asked": self.pins.asked, "served": None, "fallback": False}] * 5)
        self.assertEqual([line["askId"] for line in self.outcomes()], [ask_id("a1")])

    def test_the_stage_record_chains_to_the_inputs_and_names_every_file(self):
        admission = self.one_request()
        self.judge(admission, FakeSandbox(self))
        record = self.record()
        self.assertEqual((record["state"], record["claimRun"], record["reference"]),
                         ("written", "start-1", "registration-1"))
        self.assertEqual(record["inputs"], {"stageSha256": "e" * 64, "files": admission["inputs"]["files"]})
        self.assertEqual(record["frozen"]["codeSha256"], self.pins.code_sha256)
        written = {}
        for directory, _, names in os.walk(self.out):
            for name in names:
                path = os.path.join(directory, name)
                if name != J.STAGE_FILE:
                    written[os.path.relpath(path, self.out)] = sha(got(path))
        self.assertEqual(written, record["files"])
        self.assertIn(f"requests/{ask_id('a1')}/sandbox/listing.txt.xz", written)
        self.assertEqual(record["counts"]["byVerdict"], {"approve": 1})

    def test_a_deviation_of_the_inputs_and_a_fact_that_holds_start_no_judge(self):
        admission = write_inputs(self.inputs, self.out, [
            deviation_line("b1", "the log gives no title of ACME-5 as of event 7"),
            request_of("a2", "ACME-2", hold=True)])
        sandbox = FakeSandbox(self)
        self.assertEqual(self.judge(admission, sandbox), 0, self.stderr)
        self.assertEqual(sandbox.runs, [])
        self.assertEqual(self.outcome("b1")["deviation"]["kind"], "inputs")
        held = self.outcome("a2")
        self.assertEqual((held["outcome"], held["verdict"], held["judgement"]["judges"]),
                         ("hold", "never_decide:waiting-legion-root", 0))
        self.assertEqual([line["outcome"] for line in self.outcomes()], ["deviation", "hold"])

    def test_a_fact_the_frozen_check_reads_otherwise_is_a_deviation(self):
        request, records = request_of("a1", "ACME-1", hold=True)
        request["fact"] = dict(request["fact"], holds=False)
        request["factHold"] = False
        sandbox = FakeSandbox(self)
        self.judge(write_inputs(self.inputs, self.out, [(request, records)]), sandbox)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "fact")
        self.assertEqual(sandbox.runs, [])

    def test_an_input_the_inputs_cannot_give_is_a_deviation_before_any_judge(self):
        full = [{"id": f"ACME-{number}", "kind": "issue", "rank": 1.0} for number in range(50)]
        sandbox = FakeSandbox(self)
        self.judge(write_inputs(self.inputs, self.out, [request_of("a1", "ACME-1", pages=[full])]), sandbox)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "inputs")
        self.assertEqual(sandbox.runs, [])
        self.assertFalse(os.path.exists(os.path.join(self.work, "bundles", ask_id("a1"))))

    def test_only_reads_of_the_own_bundle_or_a_skill_file_and_the_answer_pass(self):
        allowed = lambda index, bundle: [  # noqa: E731
            ("read", {"path": os.path.relpath(bundle, self.work) + ":1-40"}, None),
            ("grep", {"pattern": "verdict", "path": bundle}, None),
            ("glob", {"path": bundle}, None),
            ("read", {"path": "skill://approval-review/references/never-decide.md:raw"},
             {"resolvedPath": HARNESS + "/references/never-decide.md"}),
            ("read", {"path": HARNESS + "/references/reviewer-principles.md"}, None),
            ("yield", {"data": "{}"}, None)]
        self.assertEqual(self.judged_kind(judge_tools=allowed), "approve")

    def test_any_other_tool_call_is_a_deviation(self):
        cases = {
            "another bundle": lambda index, bundle: [("read", {"path": bundle.replace("judge-1", "judge-2")}, None)]
            if index == 0 else [],
            "the bundle's directory": lambda index, bundle: [("glob", {"path": os.path.dirname(bundle)}, None)],
            "a search": lambda index, bundle: [("web_search", {"query": "his answer"}, None)],
            "a grep with no path": lambda index, bundle: [("grep", {"pattern": "x"}, None)],
            "a skill resolved elsewhere": lambda index, bundle: [
                ("read", {"path": "skill://approval-review"}, {"resolvedPath": "/etc/passwd"})],
            "a file outside the skill": lambda index, bundle: [("read", {"path": "skill://approval-review/../x"}, None)],
            "a list with one other path": lambda index, bundle: [("grep", {"pattern": "x", "path": f"{bundle};/work"}, None)],
        }
        for name, tools in cases.items():
            with self.subTest(name=name):
                self.assertEqual(self.judged_kind(judge_tools=tools), "tool-call")
                shutil.rmtree(self.out)

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
            self.assertEqual(J.rule_verdict(answers, kinds), verdict)

    def test_a_sandbox_that_refuses_stops_the_run_and_every_later_request_is_unjudged(self):
        admission = write_inputs(self.inputs, self.out, [request_of("a1", "ACME-1"), request_of("a2", "ACME-2")])
        sandbox = FakeSandbox(self, record={"refused": "the variable GITHUB_TOKEN names a credential"})
        self.assertEqual(self.judge(admission, sandbox), 2)
        self.assertEqual(len(sandbox.runs), 1)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "sandbox")
        self.assertEqual(self.outcome("a2")["deviation"]["kind"], "unjudged")
        self.assertIn("GITHUB_TOKEN", self.record()["stopped"])
        self.assertEqual([line["deviation"] for line in self.outcomes()], ["sandbox", "unjudged"])

    def test_a_job_with_no_result_is_its_requests_deviation_and_the_run_goes_on(self):
        admission = write_inputs(self.inputs, self.out, [request_of("a1", "ACME-1"), request_of("a2", "ACME-2")])

        class Once(FakeSandbox):
            def run(self, ask, files, extra_env=None):
                self.failure = "the Job did not finish by its deadline" if not self.runs else None
                return super().run(ask, files)

        self.assertEqual(self.judge(admission, Once(self)), 0, self.stderr)
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "sandbox")
        self.assertEqual(self.outcome("a2")["verdict"], "approve")

    def test_a_pod_that_ran_another_image_is_a_deviation(self):
        self.assertEqual(self.judged_kind(image_id="registry.example.test/sandbox@sha256:" + "b" * 64), "sandbox")


# ---------- the stage on disk ----------

class StageTest(Base):
    def test_a_written_stage_is_never_judged_again(self):
        admission = self.one_request()
        self.judge(admission, FakeSandbox(self))
        before = got(os.path.join(self.out, J.STAGE_FILE))
        again = FakeSandbox(self)
        self.assertEqual(self.judge(admission, again), 0, self.stderr)
        self.assertEqual(again.runs, [])
        self.assertEqual(got(os.path.join(self.out, J.STAGE_FILE)), before)

    def test_a_request_started_and_not_finished_is_a_deviation_never_judged_again(self):
        admission = write_inputs(self.inputs, self.out, [request_of("a1", "ACME-1"), request_of("a2", "ACME-2")])
        with self.assertRaises(Killed):
            self.judge(admission, FakeSandbox(self, raise_on=Killed()))
        self.assertEqual(self.record()["state"], "started")
        resumed = FakeSandbox(self)
        self.assertEqual(self.judge(admission, resumed), 0, self.stderr)
        self.assertEqual([run["ask"] for run in resumed.runs], [ask_id("a2")])
        self.assertEqual(self.outcome("a1")["deviation"]["kind"], "started")
        self.assertEqual(self.outcome("a2")["verdict"], "approve")

    def test_an_interrupted_run_writes_every_request_it_did_not_judge(self):
        admission = write_inputs(self.inputs, self.out, [request_of("a1", "ACME-1"), request_of("a2", "ACME-2")])
        self.assertEqual(self.judge(admission, FakeSandbox(self, raise_on=KeyboardInterrupt())), 2)
        self.assertEqual([line["deviation"] for line in self.outcomes()], ["started", "unjudged"])
        self.assertEqual(self.record()["state"], "written")

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
        outcome = os.path.join(self.out, "requests", ask_id("a1"), "outcome.json")
        put(outcome, got(outcome) + b" ")
        self.assertEqual(self.judge(admission, FakeSandbox(self)), 2)
        self.assertIn("is not the file the stage record names", self.stderr)

    def test_an_out_with_files_and_no_record_is_refused(self):
        os.makedirs(self.out)
        put(os.path.join(self.out, "left.txt"), "left")
        self.assertEqual(self.judge(self.one_request(), FakeSandbox(self)), 2)
        self.assertIn("no s10-judged.json", self.stderr)

    def test_a_refused_admission_writes_nothing(self):
        def refuse(argv, environ):
            raise J.CannotRun("claim.ts registers no read under registration-1")

        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            status = J.main(self.argv(), pins=self.pins, admit=refuse, runner_of=lambda image, environ: None)
        self.assertEqual(status, 2)
        self.assertIn("registers no read", stderr.getvalue())
        self.assertFalse(os.path.exists(self.out))

    def test_an_image_by_no_digest_is_refused(self):
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            status = J.main(self.argv(image="registry.example.test/sandbox:latest"), pins=self.pins,
                            admit=lambda argv, environ: self.fail("admitted"), runner_of=None)
        self.assertEqual(status, 2)
        self.assertIn("by its digest", stderr.getvalue())


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
                 "f 644 10 /work/shadow/inventory.jsonl": "outside the bundles",
                 "f 644 10 /run/model-gateway-key/..2026_11_20_15_47_29.1/token": "one item"}
        for line, reason in cases.items():
            with self.subTest(line=line):
                self.assertIn(reason, SANDBOX.listing_refusal(LISTING + [line], PAYLOAD, LAYOUT))
        self.assertIn("not there", SANDBOX.listing_refusal([one for one in LISTING if "judge-1" not in one],
                                                           PAYLOAD, LAYOUT))

    def test_a_runtime_other_than_point_twos_is_refused(self):
        measured = dict(SANDBOX.RUNTIME, piConfigFiles=SANDBOX.OVERLAY_FILE)
        self.assertIsNone(SANDBOX.runtime_refusal(measured))
        for name in SANDBOX.RUNTIME:
            with self.subTest(name=name):
                self.assertIn(name, SANDBOX.runtime_refusal(dict(measured, **{name: "0" * 64})))
        self.assertIn("PI_CONFIG_FILES", SANDBOX.runtime_refusal(dict(measured, piConfigFiles="/tmp/other.yml")))

    def test_the_result_travels_whole_or_is_refused(self):
        files = {"record.json": b"{}", "sessions/top.jsonl": b"x" * 200_000}
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            SANDBOX.print_result(files)
        log = "some line omp printed\n" + printed.getvalue()
        self.assertEqual(J.result_of(log), files)
        lines = log.splitlines()
        with self.assertRaisesRegex(ValueError, "whole"):
            J.result_of("\n".join(line for line in lines if not line.startswith(SANDBOX.RESULT_LINE + " 0 ")))
        end = next(line for line in lines if line.startswith(SANDBOX.RESULT_END))
        with self.assertRaisesRegex(ValueError, "sha256"):
            J.result_of(log.replace(end, end[:-1] + ("0" if end[-1] != "0" else "1")))
        with self.assertRaises(ValueError):
            SANDBOX.unpack(SANDBOX.pack({"../out": b"x"}))

    def test_the_payload_travels_in_variables_each_under_the_limit_and_is_checked(self):
        files = {f"/work/shadow/bundles/a/judge-{index}.md": os.urandom(150_000) for index in range(1, 3)}
        variables = J.payload_variables(files)
        self.assertTrue(all(len(name) + 1 + len(value) < 128 * 1024 for name, value in variables.items()))
        self.assertEqual(SANDBOX.payload_of(variables), files)
        with self.assertRaisesRegex(ValueError, "sha256"):
            SANDBOX.payload_of(dict(variables, S10_PAYLOAD_SHA256="0" * 64))
        with self.assertRaisesRegex(ValueError, "emptyDir"):
            SANDBOX.place({"/etc/judge-1.md": b"x"}, LAYOUT)

    def test_the_listing_names_every_entry_without_following_a_link(self):
        with tempfile.TemporaryDirectory() as root:
            os.makedirs(os.path.join(root, "a"))
            put(os.path.join(root, "a", "file"), "12345")
            os.symlink("/etc", os.path.join(root, "link"))
            lines = SANDBOX.listing_of(root)
        self.assertEqual([line.split(" ")[0] for line in lines], ["d", "f", "l"])
        self.assertTrue(lines[1].endswith(" 5 " + os.path.join(root, "a", "file")))
        self.assertTrue(lines[2].endswith(" -> /etc"))

    def test_the_one_cell_makes_the_recorded_calls_and_waits_as_s10_waits(self):
        self.assertIn(SANDBOX.CALLS_FILE, SANDBOX.CELL)
        self.assertIn("wait(handles, timeout=1500, raise_errors=False)", SANDBOX.CELL)
        self.assertIn(SANDBOX.CELL, SANDBOX.PROMPT)
        compile(SANDBOX.CELL, "cell", "exec")


# ---------- the sandbox Job ----------

def own_pod():
    """A pod spec of the shape the driver copies from: more volumes and variables than it takes."""
    return {"metadata": {"namespace": "sessions", "name": "session-1"},
            "spec": {"serviceAccountName": "session", "securityContext": {"runAsUser": 1000, "runAsNonRoot": True},
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
        spec = J.job_spec("s10-judge-1", own_pod(), IMAGE, {"S10_SANDBOX": "program", "S10_PAYLOAD_0": "x"}, self.environ)
        pod = spec["spec"]["template"]["spec"]
        self.assertEqual((spec["metadata"]["name"], spec["metadata"]["namespace"]), ("s10-judge-1", "sessions"))
        self.assertEqual((pod["serviceAccountName"], pod["automountServiceAccountToken"], pod["enableServiceLinks"]),
                         ("session", False, False))
        [container] = pod["containers"]
        self.assertEqual(container["image"], IMAGE)
        self.assertEqual([(one["mountPath"], one.get("subPath")) for one in container["volumeMounts"]],
                         [(SANDBOX.KEY_MOUNT, None), (SANDBOX.MODELS_FILE, "models.yml"),
                          (SANDBOX.OVERLAY_FILE, "overlay.yml"), ("/work", None)])
        volumes = {one["name"]: one for one in pod["volumes"]}
        self.assertEqual(volumes["gateway-key"]["secret"],
                         {"secretName": "session-secrets", "items": [{"key": "GATEWAY_KEY", "path": "key"}],
                          "defaultMode": 288})
        self.assertEqual(volumes["route"]["configMap"]["items"],
                         [{"key": "models.yml", "path": "models.yml"}, {"key": "overlay.yml", "path": "overlay.yml"}])
        self.assertEqual(volumes["work"], {"name": "work", "emptyDir": {}})
        self.assertEqual(len(volumes), 3)
        self.assertEqual([one["name"] for one in container["env"]],
                         [*J.PROFILE_VARIABLES, "PYTHONDONTWRITEBYTECODE", "S10_PAYLOAD_0", "S10_SANDBOX"])
        self.assertTrue(all("valueFrom" not in one for one in container["env"]))
        self.assertEqual(SANDBOX.credential_variables([one["name"] for one in container["env"]]), [])
        self.assertEqual(container["command"], ["python3", "-c", J.BOOT])
        self.assertEqual(pod["tolerations"], own_pod()["spec"]["tolerations"])
        kept = J.redacted(spec)["spec"]["template"]["spec"]["containers"][0]["env"]
        self.assertEqual(next(one["value"] for one in kept if one["name"] == "S10_SANDBOX"), "sha256:" + sha("program"))

    def test_a_pod_whose_gateway_key_is_not_one_item_is_refused(self):
        pod = own_pod()
        pod["spec"]["volumes"][3]["secret"]["items"].append({"key": "OTHER", "path": "other"})
        with self.assertRaisesRegex(J.CannotRun, "one item"):
            J.job_spec("s10-judge-1", pod, IMAGE, {}, self.environ)
        pod["spec"]["containers"][0]["volumeMounts"].pop(4)
        with self.assertRaisesRegex(J.CannotRun, "no gateway key"):
            J.job_spec("s10-judge-1", pod, IMAGE, {}, self.environ)


if __name__ == "__main__":
    unittest.main()
