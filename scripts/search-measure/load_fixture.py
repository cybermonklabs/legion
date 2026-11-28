#!/usr/bin/env python3
"""Load a harvested corpus into a Dispatch database of this tree's own schema.

usage: load_fixture.py --corpus $TMPDIR/<dir> --database searchfix \\
         --dsn "$LEGION_TEST_PG_DSN" --server ./envoy-dispatch

It creates the database, brings it to this tree's schema with the server's own migration runner -
`envoy-dispatch census`, which calls store.Migrate before it counts anything, the same call the
server makes at boot - and copies the corpus into `issues`, `artifacts`, `artifact_versions`,
`comments`, `asks` and `messages`. Every `search` vector is filled by the schema's own triggers;
nothing here writes a vector, an embedding, or anything an embedder would have written.

Only the columns search reads, and the columns the schema requires, are written. A row's author
is a constant actor (the API's list routes do not carry one for every kind, and no search leg
reads it beyond the author filter this measurement does not use); thread links (`reply_to`,
`in_reply_to`, `ask_id`), deliveries, events and block indexes are not written at all. An ask is
loaded as a question whatever kind it has in production, since `asks.approval` carries a
schema-checked shape the read API does not return whole, and neither `kind` nor `approval` is
read by `asks_search` or by any search leg.
"""

import argparse
import json
import os
import subprocess
import urllib.parse

import pg
from corpus import JsonlRows, read_json, read_jsonl, say

ACTOR = json.dumps({"kind": "session", "id": "search-measure-fixture"})
# Every project names a default assignee (projects.default_assignee is not null since migration
# 20261111210103), which the read API does not answer; one constant login stands in for it, as
# ACTOR does for authors. No search leg reads it.
DEFAULT_ASSIGNEE = "search-measure-fixture@example.invalid"
# Work the schema's AFTER triggers queue for Dispatch's background passes. None of it is a search
# vector: emptying it keeps a measurement's server from spending the run rebuilding block indexes
# and embedding queues for a corpus nobody will edit.
QUEUE_TABLES = (
    "embeddings_pending",
    "document_block_versions_owed",
    "item_blocks_owed",
    "block_grammar_owed",
    "search_term_queue",
    "doc_compaction_owed",
    "user_threads",
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--corpus", required=True)
    parser.add_argument("--database", required=True, help="database to create and load")
    parser.add_argument("--dsn", default=os.environ.get("LEGION_TEST_PG_DSN", ""), help="a DSN on the server the database is created on")
    parser.add_argument("--server", required=True, help="path of the envoy-dispatch binary built from this tree")
    parser.add_argument("--keep", action="store_true", help="load into the database as it stands instead of dropping it first")
    arguments = parser.parse_args()
    if not arguments.dsn:
        raise SystemExit("--dsn or LEGION_TEST_PG_DSN names the Postgres server")

    admin = arguments.dsn
    target = replace_database(arguments.dsn, arguments.database)
    if not arguments.keep:
        psql(admin, f'drop database if exists "{arguments.database}"')
        psql(admin, f'create database "{arguments.database}"')
    # `backfill-block-ids` is the shortest way into the server's own migration runner:
    # openMigrated calls store.Migrate, the call the server makes at boot, and on a database with
    # no document yet the backfill itself has nothing to do. `census` would not do: it answers
    # "fresh database, nothing to check" and migrates nothing.
    say(f"migrating {arguments.database} with {arguments.server}")
    migrate = subprocess.run(
        [os.path.abspath(arguments.server), "backfill-block-ids"],
        env={**os.environ, "DATABASE_URL": target},
        capture_output=True,
        text=True,
    )
    if migrate.returncode != 0:
        raise SystemExit(f"migrate: {migrate.stdout}\n{migrate.stderr}")
    applied = psql(target, "select count(*) from schema_migrations").split("\n")[2].strip()
    say(f"{applied} migrations applied")

    corpus = arguments.corpus
    projects = read_json(corpus, "projects.json")
    issues = read_jsonl(corpus, "issues.jsonl")
    # The other files are read a row at a time, once for each use below: documents.jsonl is every
    # document's markdown, more than a gigabyte of a deployment's, and parsed whole it is more than
    # the memory of the pod that loads it. Projects and issues are small.
    documents = JsonlRows(corpus, "documents.jsonl")
    comments = JsonlRows(corpus, "comments.jsonl")
    asks = JsonlRows(corpus, "asks.jsonl")
    messages = JsonlRows(corpus, "messages.jsonl")

    copy(target, "projects", ("key", "name", "next_number", "default_assignee"),
         ([project["key"], project["name"], 1, DEFAULT_ASSIGNEE] for project in projects))

    loaded_issues = {issue["key"] for issue in issues}
    copy(target, "issues",
         ("key", "project_key", "number", "title", "status", "labels", "rank", "assignee", "created_by", "updated_at"),
         ([issue["key"], issue["project"], issue["number"], issue["title"], issue["status"],
           pg_array(issue["labels"]), issue["rank"], issue["assignee"], ACTOR, issue["updated_at"]]
          for issue in issues))
    # A parent is set after every issue is in, so a child never arrives before its parent; it is
    # no part of any search vector, and the issue tree is what a project's reader sees.
    parents = [(issue["key"], issue["parent"]) for issue in issues
               if issue.get("parent") and issue["parent"] in loaded_issues]
    if parents:
        copy(target, "issue_parents_staging", ("key", "parent_key"), (list(pair) for pair in parents),
             create="create temporary table issue_parents_staging (key text, parent_key text)",
             after="update issues set parent_key = s.parent_key from issue_parents_staging s where issues.key = s.key")

    def loaded(document):
        return document["issue_key"] is None or document["issue_key"] in loaded_issues

    copy(target, "artifacts",
         ("id", "issue_key", "project_key", "slug", "name", "kind", "is_primary", "created_by", "created_at"),
         ([document["artifact_id"], document["issue_key"], document["project"], document["slug"],
           document["name"], "doc", document["primary"], ACTOR, document["created_at"]]
          for document in documents if loaded(document)))
    copy(target, "artifact_versions",
         ("artifact_id", "number", "markdown", "authors", "named", "summary", "created_at"),
         ([document["artifact_id"], document["version"], document["markdown"], f"[{ACTOR}]",
           document["version_named"], document["version_summary"], document["version_created_at"]]
          for document in documents if loaded(document)))

    documents_loaded = {document["artifact_id"] for document in documents if loaded(document)}
    asks = (ask for ask in asks
            if (ask["issue_key"] in loaded_issues if ask["issue_key"] else ask["artifact_id"] in documents_loaded))
    copy(target, "asks",
         ("id", "issue_key", "artifact_id", "author", "question", "options", "answer", "state", "resolution", "created_at"),
         ([ask["id"], ask["issue_key"], ask["artifact_id"], ACTOR, ask["question"], json.dumps(ask["options"]),
           json.dumps(ask["answer"]) if ask["state"] == "answered" and ask["answer"] else None,
           ask["state"] if not (ask["state"] == "answered" and not ask["answer"]) else "open",
           "{}" if ask["state"] == "resolved" else None,
           ask["created_at"]]
          for ask in asks))

    comments = (comment for comment in comments
                if (comment["issue_key"] in loaded_issues if comment["issue_key"] else comment["artifact_id"] in documents_loaded))
    copy(target, "comments",
         ("id", "issue_key", "artifact_id", "author", "body", "resolved", "created_at"),
         ([comment["id"], comment["issue_key"], comment["artifact_id"], ACTOR, comment["body"],
           comment["resolved"], comment["created_at"]]
          for comment in comments))

    messages = (message for message in messages if message["issue_key"] in loaded_issues)
    copy(target, "messages", ("id", "issue_key", "author", "body", "target", "created_at"),
         ([message["id"], message["issue_key"], ACTOR, message["body"], message["target"], message["created_at"]]
          for message in messages))

    for table in QUEUE_TABLES:
        psql(target, f"delete from {table}", tolerate=True)
    # A bulk COPY leaves every GIN index with a pending list a query would scan row by row, and
    # the planner with no statistics at all.
    psql(target, "vacuum analyze")
    print(row_counts(target))


def copy(dsn, table, columns, rows, create=None, after=None):
    r"""COPY rows into table in COPY's text format, so every row-level trigger the schema has fires.

    The rows go into the psql script itself, after the COPY statement, and psql ends a COPY's inline
    data at the first line that is only `\.`, inside a quoted CSV field too: as CSV, a document or a
    comment holding such a line stopped the load with `unterminated CSV quoted field`. In the text
    format a backslash is written `\\` and a newline, a carriage return and a tab (the column
    separator) `\n`, `\r` and `\t`, and every other character stands for itself; so a row is one
    line, its only tabs separate its columns, and no line of it can be `\.`, on every psql. NULL is
    `\N` and an empty string an empty field, so an empty body and a missing one are not the same
    cell. The rows are counted as they are written, so the line this says costs no pass over a
    corpus file of its own.
    """
    path = os.path.join(os.environ.get("TMPDIR", "/tmp"), f"search-measure-copy-{table}.sql")
    count = 0
    with open(path, "w") as handle:
        if create:
            handle.write(create + ";\n")
        handle.write(f"copy {table} ({', '.join(columns)}) from stdin with (format text);\n")
        for row in rows:
            handle.write("\t".join(_cell(value) for value in row) + "\n")
            count += 1
        handle.write("\\.\n")
        if after:
            handle.write(after + ";\n")
    say(f"copy {count} rows into {table}")
    pg.run(dsn, "-q", "-f", path, label=f"psql {table}")
    os.remove(path)


def _cell(value):
    if value is None:
        return "\\N"
    if value is True:
        return "t"
    if value is False:
        return "f"
    return str(value).replace("\\", "\\\\").replace("\n", "\\n").replace("\r", "\\r").replace("\t", "\\t")


def pg_array(values):
    """A text[] literal: every element quoted, so a label holding a comma or a brace survives."""
    return "{" + ",".join('"' + str(value).replace("\\", "\\\\").replace('"', '\\"') + '"' for value in values) + "}"


def psql(dsn, statement, tolerate=False):
    # The statement names the failure, so a run that stops says which of these calls stopped it.
    return pg.run(dsn, "-q", "-c", statement, label=f"psql {statement!r}", limit=None,
                  tolerate=tolerate)


def row_counts(dsn):
    return psql(dsn, FIXTURE_COUNTS)


# FIXTURE_COUNTS is the one statement the report's row counts come from: each searchable kind as
# the search legs count it, beside how many of its rows have a filled `search` vector.
FIXTURE_COUNTS = """
select 'issue' as kind, count(*) as rows, count(search) as with_search from issues
union all select 'document', count(*), count(search) from document_search
union all select 'document_version', count(*), count(search) from artifact_versions
union all select 'comment', count(*), count(search) from comments
union all select 'ask', count(*), count(search) from asks
union all select 'message', count(*), count(search) from messages
order by 1
"""


def replace_database(dsn, database):
    parts = urllib.parse.urlsplit(dsn)
    return urllib.parse.urlunsplit((parts.scheme, parts.netloc, "/" + database, parts.query, parts.fragment))


if __name__ == "__main__":
    main()
