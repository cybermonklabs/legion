"""Tests of the corpus `harvest.py` writes and `load_fixture.py` loads, each a row at a time.

Run from this directory with `python3 -m unittest stream_test`. A deployment's documents are more
than a gigabyte of markdown, so the harvest writes each row as its chunk of answers comes and the
load reads each large corpus file a line at a time; these tests hold both to what reading everything
at once wrote and loaded. A stand-in Dispatch answers the harvest's GETs from dicts, and a stand-in
psql records each statement and each COPY file it was handed, so neither needs a network or a
Postgres.
"""

import sys

sys.dont_write_bytecode = True

import contextlib  # noqa: E402
import csv  # noqa: E402
import io  # noqa: E402
import json  # noqa: E402
import os  # noqa: E402
import pathlib  # noqa: E402
import re  # noqa: E402
import tempfile  # noqa: E402
import unittest  # noqa: E402
from unittest import mock  # noqa: E402

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import corpus  # noqa: E402
import dispatch_read  # noqa: E402
import harvest  # noqa: E402
import load_fixture  # noqa: E402

# A chunk larger than the corpus: every answer of a phase held at once, as harvest.py read them
# before it read a chunk at a time.
WHOLE = 1000
# The phases that read a chunk at a time, by the name of the function each maps over its items.
CHUNKED = ("text_of", "issue_items", "document_items")
PROGRESS = re.compile(r"  [a-z ]+: \d+/\d+$")


class StandInDispatch(dispatch_read.ReadOnlyDispatch):
    """A Dispatch that answers each GET from routes, by path, and refuses every other path as a
    server refuses what it does not hold. `paged` is the client's own."""

    def __init__(self, routes):
        self.routes = routes
        self.asked = []

    def get(self, path, tries=5):
        self.asked.append(path)
        if path not in self.routes:
            raise dispatch_read.DispatchRefused(404, "not found")
        # A fresh decode for every call, as an answer off the wire is.
        return json.loads(json.dumps(self.routes[path]))


def issue(key, parent=None, primary=None, labels=("search",)):
    return {"key": key, "title": f"The title of {key}", "status": "open", "labels": list(labels),
            "rank": "A", "assignee": "someone@example.invalid", "parent": parent,
            "updated_at": "2026-11-02T09:00:00Z", "primary_artifact_id": primary}


def artifact(artifact_id, project, slug, issue_key=None, kind="doc", primary=False, versions=2):
    return {"id": artifact_id, "kind": kind, "project": project, "slug": slug, "name": f"The {slug}",
            "issue_key": issue_key, "primary": primary, "created_at": "2026-11-01T09:00:00Z",
            "versions": [{"number": number, "created_at": f"2026-11-0{number + 1}T09:00:00Z",
                          "named": number == 2, "summary": "named" if number == 2 else None}
                         for number in range(1, versions + 1)]}


def comment(comment_id, **extra):
    return {"id": comment_id, "body": f"The body of {comment_id}", "created_at": "2026-11-03T09:00:00Z",
            **extra}


def ask(ask_id, **extra):
    return {"id": ask_id, "question": f"The question of {ask_id}?", "created_at": "2026-11-03T09:00:00Z",
            **extra}


def message(message_id, in_reply_to=None, **extra):
    return {"id": message_id, "body": f"The body of {message_id}", "created_at": "2026-11-03T10:00:00Z",
            "in_reply_to": in_reply_to, "target": None, **extra}


MARKDOWN = '# A heading\n\nA café\'s "quoted", comma-laden line,\twith a tab.\n\nAnd a second paragraph.\n'


def harvest_routes():
    """Every answer the harvest reads of a small corpus, by path.

    Each path harvest.py asks for that is not here is refused: GAMMA's artifact list, ALPHA-3's own
    artifact list and its comments, the text of `broken`, and the items of `design`. Of the project
    lists, `stray` is a document of an issue the harvest did not read, `diagram` is no document and
    `draft` has no version.
    """
    notes = artifact("notes", "ALPHA", "notes", issue_key="ALPHA-2", versions=1)
    issues = {
        "ALPHA": [issue("ALPHA-1", primary="spec-a1", labels=("search", "a,b")),
                  issue("ALPHA-2", parent="ALPHA-1"), issue("ALPHA-3", primary="spec-a3")],
        "BETA": [issue("BETA-1", primary="spec-b1")],
        "GAMMA": [],
    }
    routes = {"/api/v1/projects": [{"key": key, "name": key.title(), "default_assignee": "someone"}
                                   for key in issues]}
    for key, rows in issues.items():
        routes[f"/api/v1/issues?project={key}&limit=200&offset=0"] = {"issues": rows, "total": len(rows)}
    routes.update({
        "/api/v1/projects/ALPHA/artifacts": [
            artifact("plan", "ALPHA", "plan"), notes, artifact("stray", "ALPHA", "stray", issue_key="ALPHA-9"),
            artifact("diagram", "ALPHA", "diagram", kind="image"), artifact("draft", "ALPHA", "draft", versions=0),
            artifact("broken", "ALPHA", "broken"), artifact("design", "ALPHA", "design"),
        ],
        "/api/v1/projects/BETA/artifacts": [artifact("review", "BETA", "review", issue_key="BETA-1")],
        "/api/v1/issues/ALPHA-1/artifacts": [artifact("spec-a1", "ALPHA", "spec", issue_key="ALPHA-1", primary=True),
                                             artifact("sketch", "ALPHA", "sketch", issue_key="ALPHA-1")],
        "/api/v1/issues/BETA-1/artifacts": [artifact("spec-b1", "BETA", "spec", issue_key="BETA-1", primary=True, versions=1),
                                            artifact("review", "BETA", "review", issue_key="BETA-1")],
        "/api/v1/projects/ALPHA/artifacts/plan/text": {"version": 1, "markdown": MARKDOWN},
        "/api/v1/issues/ALPHA-2/artifacts/notes/text": {"markdown": "The notes of ALPHA-2."},
        "/api/v1/projects/ALPHA/artifacts/design/text": {"version": 2, "markdown": None},
        "/api/v1/issues/BETA-1/artifacts/review/text": {"version": 2, "markdown": "A review."},
        "/api/v1/issues/ALPHA-1/artifacts/spec/text": {"version": 2, "markdown": "The spec of ALPHA-1."},
        "/api/v1/issues/BETA-1/artifacts/spec/text": {"version": 1, "markdown": "The spec of BETA-1."},
        "/api/v1/issues/ALPHA-1/comments": [comment("c-a1-1"), comment("c-a1-2", reply_to="c-a1-1", resolved=True)],
        "/api/v1/issues/ALPHA-1/asks": [ask("ask-a1")],
        "/api/v1/issues/ALPHA-1/messages": [
            {"message": message("m-1"), "replies": [message("m-2", "m-1", replies=[message("m-3", "m-2")])]},
            {"message": message("m-4"), "replies": []},
        ],
        "/api/v1/issues/ALPHA-2/comments": [],
        "/api/v1/issues/ALPHA-2/asks": [],
        "/api/v1/issues/ALPHA-2/messages": [],
        "/api/v1/issues/BETA-1/comments": [comment("c-b1")],
        "/api/v1/issues/BETA-1/asks": [ask("ask-b1", options=["yes", "no"], answer={"choice": "yes"}, state="answered")],
        "/api/v1/issues/BETA-1/messages": [{"message": message("m-5", replies=[message("m-6", "m-5")])}],
        "/api/v1/artifacts/plan/comments": [comment("c-plan")],
        "/api/v1/artifacts/plan/asks": [ask("ask-plan")],
        "/api/v1/search/coverage": {"identity": {"embedder": "model.example.invalid"}, "total": {"items": 19},
                                    "kinds": {"issue": {"items": 4, "with_search": 4},
                                              "document": {"items": 6, "with_search": 6}}},
    })
    return routes


# What the harvest of harvest_routes says beside its progress lines, the same whatever the chunk.
HARVEST_SAID = [
    "3 projects",
    "4 issues",
    "  project GAMMA artifacts unread: DispatchRefused('404: not found')",
    "5 documents from the project lists",
    "7 documents, 1 issues' primary unread",
    "6 documents with text, 1 unread",
    "4 comments, 3 asks, 6 messages, 1 issues unread",
]


def ids(text, key):
    return [json.loads(line)[key] for line in text.decode().splitlines()]


class HarvestTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = pathlib.Path(directory.name)
        environment = mock.patch.dict(os.environ, {"TMPDIR": str(self.root)})
        environment.start()
        self.addCleanup(environment.stop)

    def harvest(self, chunk):
        """The files a harvest wrote, the lines it said, the paths it asked for, and the number of
        items each pmap of a chunked phase was given."""
        out = self.root / f"chunk-{chunk}"
        dispatch = StandInDispatch(harvest_routes())
        mapped = []

        def recorded_pmap(function, items, *rest, **options):
            if function.__name__ in CHUNKED:
                mapped.append((function.__name__, len(items)))
            return dispatch_read.pmap(function, items, *rest, **options)

        said = io.StringIO()
        with mock.patch.object(harvest, "ReadOnlyDispatch", lambda: dispatch), \
                mock.patch.object(harvest, "pmap", recorded_pmap), \
                mock.patch.object(sys, "argv", ["harvest.py", "--out", str(out), "--workers", "3", "--chunk", str(chunk)]), \
                contextlib.redirect_stderr(said):
            harvest.main()
        files = {path.name: path.read_bytes() for path in sorted(out.iterdir())}
        return files, said.getvalue().splitlines(), sorted(dispatch.asked), mapped

    def test_a_chunked_harvest_writes_the_bytes_a_harvest_holding_every_answer_wrote(self):
        whole, whole_said, whole_asked, whole_mapped = self.harvest(WHOLE)
        self.assertEqual(set(harvest_routes()) - set(whole_asked), set())
        self.assertEqual(sorted(whole), ["asks.jsonl", "comments.jsonl", "coverage.json", "documents.jsonl",
                                         "issues.jsonl", "messages.jsonl", "projects.json"])
        self.assertEqual(ids(whole["issues.jsonl"], "key"), ["ALPHA-1", "ALPHA-2", "ALPHA-3", "BETA-1"])
        documents = [json.loads(line) for line in whole["documents.jsonl"].decode().splitlines()]
        self.assertEqual([(document["artifact_id"], document["issue_key"]) for document in documents],
                         [("plan", None), ("notes", "ALPHA-2"), ("design", None), ("review", "BETA-1"),
                          ("spec-a1", "ALPHA-1"), ("spec-b1", "BETA-1")])
        self.assertEqual(documents[0]["markdown"], MARKDOWN)
        self.assertEqual([[line[key] for key in ("id", "issue_key", "artifact_id")]
                          for line in map(json.loads, whole["comments.jsonl"].decode().splitlines())],
                         [["c-a1-1", "ALPHA-1", None], ["c-a1-2", "ALPHA-1", None], ["c-b1", "BETA-1", None],
                          ["c-plan", None, "plan"]])
        self.assertEqual(ids(whole["asks.jsonl"], "id"), ["ask-a1", "ask-b1", "ask-plan"])
        self.assertEqual([(line["id"], line["in_reply_to"])
                          for line in map(json.loads, whole["messages.jsonl"].decode().splitlines())],
                         [("m-1", None), ("m-2", "m-1"), ("m-3", "m-2"), ("m-4", None), ("m-5", None), ("m-6", "m-5")])
        self.assertEqual(json.loads(whole["coverage.json"]), {"total_items": 19, "kinds": {"issue": 4, "document": 6}})
        self.assertEqual([line for line in whole_said if not PROGRESS.match(line)], HARVEST_SAID)
        self.assertEqual(whole_mapped, [("text_of", 7), ("issue_items", 4), ("document_items", 2)])

        for chunk in (1, 2):
            with self.subTest(chunk=chunk):
                files, said, asked, mapped = self.harvest(chunk)
                self.assertEqual(files, whole)
                self.assertEqual(asked, whole_asked)
                self.assertEqual([line for line in said if not PROGRESS.match(line)], HARVEST_SAID)
                self.assertTrue(all(size <= chunk for _, size in mapped), mapped)
                for name, total in (("text_of", 7), ("issue_items", 4), ("document_items", 2)):
                    self.assertEqual(sum(size for mapped_name, size in mapped if mapped_name == name), total)
                # The progress lines say the running total after each chunk.
                self.assertEqual([line for line in said if line.startswith("  document text: ")],
                                 [f"  document text: {min(done, 7)}/7" for done in range(chunk, 7 + chunk, chunk)])
                self.assertEqual([line for line in said if line.startswith("  issue items: ")],
                                 [f"  issue items: {min(done, 4)}/4" for done in range(chunk, 4 + chunk, chunk)])


STAND_IN_PSQL = """#!/usr/bin/env python3
import json, os, sys
arguments = sys.argv[1:]
flag = "-f" if "-f" in arguments else "-c"
given = arguments[arguments.index(flag) + 1]
if flag == "-f":
    with open(given) as handle:
        given = handle.read()
with open(os.environ["STREAM_TEST_CALLS"], "a") as calls:
    calls.write(json.dumps({"url": arguments[0], flag: given}) + "\\n")
if given == "select count(*) from schema_migrations":
    print(" count \\n-------\\n   312\\n(1 row)\\n")
"""
# The server's migration runner, which load_fixture.py reaches through `backfill-block-ids`.
STAND_IN_SERVER = """#!/bin/sh
test "$1" = backfill-block-ids
"""
DSN = "postgres://tester@127.0.0.1:5999/postgres"


def write_load_corpus(directory):
    """A corpus written with harvest.py's own rows, holding an issue the load leaves out (ALPHA-9,
    the parent of ALPHA-3 and the issue of `stray`), so each of the load's filters has rows to drop."""
    corpus.write_json(directory, "projects.json", [{"key": "ALPHA", "name": "Alpha"}])
    corpus.write_jsonl(directory, "issues.jsonl", [
        harvest.issue_row(issue("ALPHA-1")), harvest.issue_row(issue("ALPHA-2", parent="ALPHA-1")),
        harvest.issue_row(issue("ALPHA-3", parent="ALPHA-9")),
    ])
    corpus.write_jsonl(directory, "documents.jsonl", [
        harvest.document_row(artifact(artifact_id, "ALPHA", artifact_id, issue_key=key), {"markdown": markdown})
        for artifact_id, key, markdown in (("doc-1", "ALPHA-1", "The first."), ("doc-2", None, MARKDOWN),
                                           ("stray", "ALPHA-9", "Not loaded."), ("doc-4", "ALPHA-2", ""))
    ])
    corpus.write_jsonl(directory, "comments.jsonl", [
        harvest.comment_row(comment(comment_id), key, artifact_id)
        for comment_id, key, artifact_id in (("c-1", "ALPHA-1", None), ("c-2", "ALPHA-9", None), ("c-3", None, "doc-2"),
                                             ("c-4", None, "stray"), ("c-5", "ALPHA-3", None))
    ])
    corpus.write_jsonl(directory, "asks.jsonl", [
        harvest.ask_row(ask(ask_id, **extra), key, artifact_id)
        for ask_id, key, artifact_id, extra in (
            ("a-1", "ALPHA-2", None, {"state": "answered", "answer": {"choice": "yes"}, "options": ["yes", "no"]}),
            ("a-2", None, "doc-2", {}), ("a-3", None, "stray", {}), ("a-4", "ALPHA-9", None, {}),
            ("a-5", "ALPHA-1", None, {"state": "resolved"}))
    ])
    messages = []
    harvest.flatten_threads([{"message": message("m-1"), "replies": [message("m-4", "m-1")]}], "ALPHA-1", messages)
    harvest.flatten_threads([{"message": message("m-2")}], "ALPHA-9", messages)
    harvest.flatten_threads([{"message": message("m-3")}], "ALPHA-3", messages)
    corpus.write_jsonl(directory, "messages.jsonl", messages)


# What the load of write_load_corpus copies, table by table: the first cell of each row, in order.
LOADED = {
    "projects": ["ALPHA"],
    "issues": ["ALPHA-1", "ALPHA-2", "ALPHA-3"],
    "issue_parents_staging": ["ALPHA-2"],
    "artifacts": ["doc-1", "doc-2", "doc-4"],
    "artifact_versions": ["doc-1", "doc-2", "doc-4"],
    "asks": ["a-1", "a-2", "a-5"],
    "comments": ["c-1", "c-3", "c-5"],
    "messages": ["m-1", "m-4", "m-3"],
}
COPY = re.compile(r"^copy (\w+) \([^)]*\) from stdin with \(format csv, null ''\);\n(.*?)^\\\.\n", re.M | re.S)


def copied(calls):
    """Each COPY's table and the first cell of each of its rows, in the order psql was handed them."""
    tables = {}
    for call in calls:
        if "-f" in call:
            table, body = COPY.search(call["-f"]).groups()
            tables[table] = [row[0] for row in csv.reader(io.StringIO(body))]
    return tables


class LoadTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = pathlib.Path(directory.name)
        stand_ins = self.root / "bin"
        stand_ins.mkdir()
        for name, text in (("psql", STAND_IN_PSQL), ("envoy-dispatch", STAND_IN_SERVER)):
            (stand_ins / name).write_text(text)
            (stand_ins / name).chmod(0o755)
        self.server = stand_ins / "envoy-dispatch"
        self.corpus = self.root / "corpus"
        self.corpus.mkdir()
        write_load_corpus(str(self.corpus))
        environment = mock.patch.dict(os.environ, {
            "PATH": f"{stand_ins}{os.pathsep}{os.environ['PATH']}",
            "TMPDIR": str(self.root),
        })
        environment.start()
        self.addCleanup(environment.stop)

    def load(self, name, *patches):
        """The calls psql was handed by a load and the lines it said."""
        calls = self.root / f"calls-{name}"
        said = io.StringIO()
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            stack.enter_context(mock.patch.dict(os.environ, {"STREAM_TEST_CALLS": str(calls)}))
            stack.enter_context(mock.patch.object(sys, "argv", [
                "load_fixture.py", "--corpus", str(self.corpus), "--database", "searchfix",
                "--dsn", DSN, "--server", str(self.server)]))
            stack.enter_context(contextlib.redirect_stderr(said))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            load_fixture.main()
        return [json.loads(line) for line in calls.read_text().splitlines()], said.getvalue().splitlines()

    def test_a_streamed_load_copies_what_a_load_of_whole_files_copied(self):
        read_whole = []

        def recorded_read_jsonl(directory, name):
            read_whole.append(name)
            return corpus.read_jsonl(directory, name)

        streamed, streamed_said = self.load("streamed", mock.patch.object(load_fixture, "read_jsonl", recorded_read_jsonl))
        whole, whole_said = self.load("whole", mock.patch.object(load_fixture, "JsonlRows", corpus.read_jsonl))
        self.assertEqual(read_whole, ["issues.jsonl"])
        self.assertEqual(streamed, whole)
        self.assertEqual(streamed_said, whole_said)
        self.assertEqual(copied(streamed), LOADED)
        self.assertEqual([line for line in streamed_said if line.startswith("copy ")],
                         [f"copy {len(rows)} rows into {table}" for table, rows in LOADED.items()])
        self.assertIn('"doc-2","2","' + MARKDOWN.replace('"', '""') + '"',
                      next(call["-f"] for call in streamed if call.get("-f", "").startswith("copy artifact_versions ")))
        self.assertEqual([call["-c"] for call in streamed if "-c" in call][:3],
                         ['drop database if exists "searchfix"', 'create database "searchfix"',
                          "select count(*) from schema_migrations"])


if __name__ == "__main__":
    unittest.main()
