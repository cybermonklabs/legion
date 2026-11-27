r"""Tests of the corpus `harvest.py` writes and `load_fixture.py` loads, each a row at a time.

Run from this directory with `python3 -m unittest stream_test`. A deployment's documents are more
than a gigabyte of markdown, so the harvest writes each row as its chunk of answers comes and the
load reads each large corpus file a line at a time; these tests hold both to what reading everything
at once wrote and loaded. A stand-in Dispatch answers the harvest's GETs from dicts, and a stand-in
psql records each statement and each COPY file it was handed, so neither needs a network or a
Postgres.

What the load hands psql is read back here as PostgreSQL reads COPY's text format, so a text holding
a line that is only `\.`, which psql reads as the end of a COPY's data, is held to loading whole
without a server. One test does need a server: it runs `load_fixture.copy()` through the real psql
into a database of its own, which it drops after, and reads each value back. Its server is
SEARCH_MEASURE_TEST_PG_DSN, else LEGION_TEST_PG_DSN; with neither set, as in CI, it skips.
"""

import sys

sys.dont_write_bytecode = True

import contextlib  # noqa: E402
import io  # noqa: E402
import json  # noqa: E402
import os  # noqa: E402
import pathlib  # noqa: E402
import re  # noqa: E402
import tempfile  # noqa: E402
import unittest  # noqa: E402
import uuid  # noqa: E402
from unittest import mock  # noqa: E402

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import corpus  # noqa: E402
import dispatch_read  # noqa: E402
import harvest  # noqa: E402
import load_fixture  # noqa: E402
import pg  # noqa: E402

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
# A text holding a line that is only `\.`, which psql reads as the end of a COPY's inline data
# wherever it stands, beside a backslash, a tab, a CR and a CRLF.
AWKWARD_TEXT = "A path, C:\\new\\table,\ta tab,\ra CR and a CRLF.\r\n\\.\nThe line after the \\. line.\n"


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
            {"message": message("m-4", target="role:reviewer"), "replies": []},
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
# Every row the harvest of harvest_routes writes of asks.jsonl and of messages.jsonl, whole, as the
# harvest that held every answer at once wrote them: an ask's owner is its issue or the document of
# no issue it was read from, and a message's is its issue.
HARVESTED_ASKS = [
    {"id": "ask-a1", "issue_key": "ALPHA-1", "artifact_id": None, "question": "The question of ask-a1?",
     "options": [], "answer": None, "state": "open", "created_at": "2026-11-03T09:00:00Z"},
    {"id": "ask-b1", "issue_key": "BETA-1", "artifact_id": None, "question": "The question of ask-b1?",
     "options": ["yes", "no"], "answer": {"choice": "yes"}, "state": "answered", "created_at": "2026-11-03T09:00:00Z"},
    {"id": "ask-plan", "issue_key": None, "artifact_id": "plan", "question": "The question of ask-plan?",
     "options": [], "answer": None, "state": "open", "created_at": "2026-11-03T09:00:00Z"},
]
HARVESTED_MESSAGES = [
    {"id": "m-1", "issue_key": "ALPHA-1", "body": "The body of m-1", "created_at": "2026-11-03T10:00:00Z",
     "in_reply_to": None, "target": None},
    {"id": "m-2", "issue_key": "ALPHA-1", "body": "The body of m-2", "created_at": "2026-11-03T10:00:00Z",
     "in_reply_to": "m-1", "target": None},
    {"id": "m-3", "issue_key": "ALPHA-1", "body": "The body of m-3", "created_at": "2026-11-03T10:00:00Z",
     "in_reply_to": "m-2", "target": None},
    {"id": "m-4", "issue_key": "ALPHA-1", "body": "The body of m-4", "created_at": "2026-11-03T10:00:00Z",
     "in_reply_to": None, "target": "role:reviewer"},
    {"id": "m-5", "issue_key": "BETA-1", "body": "The body of m-5", "created_at": "2026-11-03T10:00:00Z",
     "in_reply_to": None, "target": None},
    {"id": "m-6", "issue_key": "BETA-1", "body": "The body of m-6", "created_at": "2026-11-03T10:00:00Z",
     "in_reply_to": "m-5", "target": None},
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
        self.assertEqual([json.loads(line) for line in whole["asks.jsonl"].decode().splitlines()], HARVESTED_ASKS)
        self.assertEqual([json.loads(line) for line in whole["messages.jsonl"].decode().splitlines()],
                         HARVESTED_MESSAGES)
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
    # newline="" keeps each CR the file holds, as psql reads it.
    with open(given, newline="") as handle:
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
    the parent of ALPHA-3 and the issue of `stray`), so each of the load's filters has rows to drop,
    and texts a COPY has to carry whole: AWKWARD_TEXT as a document's markdown and as a comment's
    body, an empty markdown, and an empty message body beside the message's null target."""
    corpus.write_json(directory, "projects.json", [{"key": "ALPHA", "name": "Alpha"}])
    corpus.write_jsonl(directory, "issues.jsonl", [
        harvest.issue_row(issue("ALPHA-1")), harvest.issue_row(issue("ALPHA-2", parent="ALPHA-1")),
        harvest.issue_row(issue("ALPHA-3", parent="ALPHA-9")),
    ])
    corpus.write_jsonl(directory, "documents.jsonl", [
        harvest.document_row(artifact(artifact_id, "ALPHA", artifact_id, issue_key=key), {"markdown": markdown})
        for artifact_id, key, markdown in (("doc-1", "ALPHA-1", AWKWARD_TEXT), ("doc-2", None, MARKDOWN),
                                           ("stray", "ALPHA-9", "Not loaded."), ("doc-4", "ALPHA-2", ""))
    ])
    corpus.write_jsonl(directory, "comments.jsonl", [
        harvest.comment_row(comment(comment_id, **extra), key, artifact_id)
        for comment_id, key, artifact_id, extra in (
            ("c-1", "ALPHA-1", None, {"body": AWKWARD_TEXT}), ("c-2", "ALPHA-9", None, {}),
            ("c-3", None, "doc-2", {}), ("c-4", None, "stray", {}), ("c-5", "ALPHA-3", None, {}))
    ])
    corpus.write_jsonl(directory, "asks.jsonl", [
        harvest.ask_row(ask(ask_id, **extra), key, artifact_id)
        for ask_id, key, artifact_id, extra in (
            ("a-1", "ALPHA-2", None, {"state": "answered", "answer": {"choice": "yes"}, "options": ["yes", "no"]}),
            ("a-2", None, "doc-2", {}), ("a-3", None, "stray", {}), ("a-4", "ALPHA-9", None, {}),
            ("a-5", "ALPHA-1", None, {"state": "resolved"}))
    ])
    messages = []
    harvest.flatten_threads([{"message": message("m-1", target="session:s1"), "replies": [message("m-4", "m-1")]}],
                            "ALPHA-1", messages)
    harvest.flatten_threads([{"message": message("m-2")}], "ALPHA-9", messages)
    harvest.flatten_threads([{"message": message("m-3", body="")}], "ALPHA-3", messages)
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
# A COPY statement of a script load_fixture.copy writes, and the table it names.
COPY = re.compile(r"^copy (\w+) \([^)]*\) from stdin with \(format text\);\n", re.M)
# A backslash of COPY's text format and what it escapes, as PostgreSQL reads them
# (CopyReadAttributesText in copyfromparse.c): one to three octal digits, an x and one or two hex
# digits, or any one character, a raw tab and a raw newline included.
ESCAPE = re.compile(rb"\\(?:([0-7]{1,3})|x([0-9A-Fa-f]{1,2})|(.))", re.S)
# The letters that escape a control character; any other character escaped stands for itself.
CONTROLS = {b"b": b"\b", b"f": b"\f", b"n": b"\n", b"r": b"\r", b"t": b"\t", b"v": b"\v"}
# A field and the tab, newline or end of the data no backslash takes that ends it. A raw CR is in none.
FIELD = re.compile(rb"((?:\\.|[^\\\t\n\r])*)([\t\n]|\Z)", re.S)


def copy_data(script):
    r"""The table a psql script COPYs into and the lines psql sends it as the COPY's data: every line
    after the COPY statement up to the first that is only `\.`, where psql ends the data."""
    statement = COPY.search(script)
    lines = script[statement.end():].split("\n")
    return statement[1], lines[:lines.index("\\.")]


def copy_text_rows(lines):
    r"""The rows of the lines psql sends as a COPY's data, each field as FIELD reads it, a field that is
    `\N` NULL, and every backslash escape of the others decoded, an octal or hex byte included. A CR
    no backslash takes is no character of a row: PostgreSQL ends the row at it or refuses it."""
    data, row, at = "".join(line + "\n" for line in lines).encode(), [], 0
    while at < len(data) and (field := FIELD.match(data, at)):
        row.append(None if field[1] == b"\\N" else ESCAPE.sub(unescape, field[1]).decode())
        at = field.end()
        if field[2] != b"\t":
            yield row
            row = []
    if at < len(data):
        raise AssertionError(f"a raw CR, or a backslash with no character after it: {data[at:]!r}")


def unescape(escape):
    octal, hexadecimal, character = escape.groups()
    if octal:
        return bytes([int(octal, 8) & 0o377])
    if hexadecimal:
        return bytes([int(hexadecimal, 16)])
    # A backslash and a period end the data: PostgreSQL 18 refuses one that is not alone on its
    # line, and an earlier server stops reading there.
    if character == b".":
        raise AssertionError("an end-of-copy marker inside a row")
    return CONTROLS.get(character, character)


def copied(calls):
    """Each COPY's table and its rows, each read as PostgreSQL reads COPY's text format, in the order
    psql was handed them."""
    tables = {}
    for call in calls:
        if "-f" in call:
            table, data = copy_data(call["-f"])
            tables[table] = list(copy_text_rows(data))
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
        copies = copied(streamed)
        self.assertEqual({table: [row[0] for row in rows] for table, rows in copies.items()}, LOADED)
        self.assertEqual([line for line in streamed_said if line.startswith("copy ")],
                         [f"copy {len(rows)} rows into {table}" for table, rows in LOADED.items()])
        # Each text reads back whole, the line that is only `\.` and every line after it included,
        # and an empty text is not a NULL.
        self.assertEqual({row[0]: row[2] for row in copies["artifact_versions"]},
                         {"doc-1": AWKWARD_TEXT, "doc-2": MARKDOWN, "doc-4": ""})
        self.assertEqual([row[4] for row in copies["comments"]], [AWKWARD_TEXT, "The body of c-3", "The body of c-5"])
        self.assertEqual([row[3:5] for row in copies["messages"]],
                         [["The body of m-1", "session:s1"], ["The body of m-4", None], ["", None]])
        # Each boolean is copied as PostgreSQL's t or f, and the corpus holds both.
        self.assertEqual([[row[6] for row in copies["artifacts"]], [row[4] for row in copies["artifact_versions"]],
                          [row[5] for row in copies["comments"]]], [["f", "f", "f"], ["t", "t", "t"], ["f", "f", "f"]])
        # What psql is handed for doc-1, escaped by hand: each backslash doubled, and each newline, CR
        # and tab written as \n, \r and \t.
        escaped = r"A path, C:\\new\\table,\ta tab,\ra CR and a CRLF.\r\n\\.\nThe line after the \\. line.\n"
        self.assertIn(f"doc-1\t2\t{escaped}\t",
                      next(call["-f"] for call in streamed if call.get("-f", "").startswith("copy artifact_versions ")))
        self.assertEqual([call["-c"] for call in streamed if "-c" in call][:3],
                         ['drop database if exists "searchfix"', 'create database "searchfix"',
                          "select count(*) from schema_migrations"])


# Values a COPY has to carry whole, each beside what makes it awkward.
AWKWARD = [
    ("a line that is only \\. inside a text", "before\n\\.\nafter"),
    ("a value that is exactly \\.", "\\."),
    ("a newline, a CR, a CRLF and a tab inside", "one\ntwo\rthree\r\nfour\tfive"),
    ("an empty string", ""),
    ("a None", None),
    ("a trailing backslash", "C:\\temp\\"),
    ("a value that is exactly \\N", "\\N"),
    ("a backslash before each letter COPY escapes with", "\\b\\f\\n\\r\\t\\v\\x41\\101\\N\\."),
    ("non-ASCII", "café, naïve, 東京, 🙂"),
]


class CopyTextFormatTest(unittest.TestCase):
    r"""The data load_fixture.copy writes for a row holding each awkward value, read as psql and
    PostgreSQL read it: psql ends the data at the first line that is only `\.`, and PostgreSQL reads a
    character at a time, each backslash taking the one after it. copy's psql call is replaced with one
    that keeps the script, so no server is needed."""

    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        environment = mock.patch.dict(os.environ, {"TMPDIR": directory.name})
        environment.start()
        self.addCleanup(environment.stop)

    def written(self, row):
        """The psql script load_fixture.copy writes for a COPY of the one row."""
        scripts = []

        def kept(dsn, *arguments, **options):
            # newline="" keeps each CR the script holds, as psql reads it.
            with open(arguments[arguments.index("-f") + 1], newline="") as handle:
                scripts.append(handle.read())

        with mock.patch.object(load_fixture.pg, "run", kept), contextlib.redirect_stderr(io.StringIO()):
            load_fixture.copy(DSN, "awkward", [f"c{index}" for index in range(len(row))], [row])
        [script] = scripts
        return script

    def test_each_awkward_value_is_one_field_of_one_line_and_reads_back_exact(self):
        for what, value in AWKWARD:
            # Alone in a one-column row, where a value written as it stands would be a whole line of
            # the data, and between an empty string and a NULL.
            for row in ([value], ["", value, None]):
                with self.subTest(what=what, row=row):
                    script = self.written(row)
                    self.assertEqual([line for line in script.split("\n") if line == "\\."], ["\\."],
                                     "a line that is only \\. before the end of the data")
                    self.assertRegex(script, COPY, "a COPY of any format but the text format alone")
                    table, data = copy_data(script)
                    self.assertEqual(table, "awkward")
                    self.assertEqual(len(data), 1, f"a newline inside the row: {data!r}")
                    self.assertEqual(data[0].count("\t"), len(row) - 1, f"a tab that separates no columns: {data[0]!r}")
                    self.assertEqual(list(copy_text_rows(data)), [row])


class CopyIntoPostgresTest(unittest.TestCase):
    """load_fixture.copy run by the real psql against a real server, and every value read back.

    The server is SEARCH_MEASURE_TEST_PG_DSN, else LEGION_TEST_PG_DSN, and the test skips when neither
    is set. It copies into a database of its own, named afresh for each run, and drops it after."""

    def setUp(self):
        server = os.environ.get("SEARCH_MEASURE_TEST_PG_DSN") or os.environ.get("LEGION_TEST_PG_DSN")
        if not server:
            self.skipTest("no Postgres server: set SEARCH_MEASURE_TEST_PG_DSN or LEGION_TEST_PG_DSN")
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        environment = mock.patch.dict(os.environ, {"TMPDIR": directory.name})
        environment.start()
        self.addCleanup(environment.stop)
        database = f"search_measure_copy_{uuid.uuid4().hex}"
        load_fixture.psql(server, f'create database "{database}"')
        self.addCleanup(load_fixture.psql, server, f'drop database if exists "{database}"')
        self.dsn = load_fixture.replace_database(server, database)

    def test_each_awkward_value_reads_back_exact(self):
        load_fixture.psql(self.dsn, "create table awkward (id integer primary key, value text, flag boolean, "
                                    "labels text[], options jsonb, beside text)")
        # Each value as a text, as an element of a text[] beside a quote and a backslash, and as a
        # JSON string beside a backslash, with a boolean and an empty string or a NULL beside it.
        sources = [{"id": number, "value": value, "flag": (True, False, None)[number % 3],
                    "labels": ['a "quoted" label', "a back\\slash", "a,b {c}"] + ([] if value is None else [value]),
                    "options": {"value": value, "path": "C:\\temp"}, "beside": None if number % 2 else ""}
                   for number, (_, value) in enumerate(AWKWARD)]
        with contextlib.redirect_stderr(io.StringIO()):
            load_fixture.copy(self.dsn, "awkward", ("id", "value", "flag", "labels", "options", "beside"),
                              ([source["id"], source["value"], source["flag"], load_fixture.pg_array(source["labels"]),
                                json.dumps(source["options"]), source["beside"]] for source in sources))
        read = pg.rows(self.dsn, "select id, value, flag, labels, options, beside from awkward order by id")
        self.assertEqual(len(read), len(sources))
        for (what, _), source, row in zip(AWKWARD, sources, read):
            with self.subTest(what=what):
                self.assertEqual(row, source)


if __name__ == "__main__":
    unittest.main()
