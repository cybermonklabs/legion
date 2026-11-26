import { expect, test } from "bun:test";

import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";
import { runRows } from "./run-references";

/** The issue the tree read answers, as `treeAnswer` answers it. */
const KEY = "CORE-1";
const COMMENT = "7d1e4c2a-9b3f-4e8d-a6c5-2f0b1d9e7a34";
const SECOND_COMMENT = "8e2f5d3b-0c4a-4f9e-b7d6-3a1c2e0f8b45";
const RUN = "3f2b8c1e-5d4a-4b6f-9e2d-1a7c0b9e8f43";
const OTHER_RUN = "5a4d0e3f-7b6c-4d8e-9f1a-2b3c4d5e6f70";
const FIRST_RUN = "1c9e7a5b-3d2f-4e1a-8b6c-0d4f2a9e7b51";
const ASKED_AT = "2026-11-17T09:00:00Z";
const QUESTION = "When does the **release** ship?\n\nAnd who owns it?";
/** `QUESTION` as plain words: its marks gone, its two paragraphs on one line. */
const QUESTION_WORDS = "When does the release ship? And who owns it?";

/** The issue's own node. */
const ISSUE_NODE = treeNode(KEY, "issue", null, { text: "Ship the release" });

/** A comment of the issue. */
function commentNode(id: string): TreeAnswerNode {
  return treeNode(id, "comment", KEY, { text: "The release waits on the **build**." });
}

/** A command bar run, as the asker's tree read answers it (`issue_tree_read.go`'s
 *  `readContext`): a context node of the `interaction` kind and supertag, under no node, by the
 *  run's id, its asker's alone, with the question as its text. */
function runNode(id: string, madeAt: string | null = ASKED_AT, text = QUESTION): TreeAnswerNode {
  return treeNode(id, "interaction", null, {
    made_at: madeAt,
    role: "context",
    text,
    visibility: "author",
  });
}

/** The tuple a run writes from a node it created or changed to itself: its far end is the run, by
 *  its store kind and id, whether or not the read holds the run's node. */
function changedBy(from: string, run: string): TreeTuple {
  return { ...treeTuple("changed_by_run", from, run), to: { id: run, kind: "interaction" } };
}

/** One page of the tree read, which holds the whole tree where it is the read's one page. */
function treeOf(nodes: readonly TreeAnswerNode[], tuples: readonly TreeTuple[]): TreeAnswer {
  return treeAnswer(nodes, tuples, { converted: true, tree: "tree-core-1" });
}

test("an interaction of the tree read is a row with the page's words and the run's words", () => {
  const rows = runRows([
    treeOf([ISSUE_NODE, commentNode(COMMENT), runNode(RUN)], [changedBy(COMMENT, RUN)]),
  ]);
  expect(rows).toHaveLength(1);
  const [row] = rows;
  expect(row.key).toBe(RUN);
  expect(row.kind).toBe(sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction));
  expect(row.edge).toBe(NODE_REFERENCES_WORDS.edges.changed_by_run);
  expect(row.excerpt).toBe(QUESTION_WORDS);
  expect(row.at).toBe(ASKED_AT);
  expect(row.href).toBe(`/runs/${RUN}`);
  const { href: _href, key: _key, ...words } = row;
  const drawn = JSON.stringify(words);
  expect(drawn).not.toContain("interaction");
  expect(drawn).not.toContain("changed_by_run");
  expect(drawn).not.toContain(RUN);
});

// A run's row reads its question's words from the server's reading of it where the tree read
// sends one (`text_tree`), not from the question as stored.
test("a run's row reads its question's words from the reading the tree read sends", () => {
  const reading = {
    content: [{ content: [{ text: "Who ships the release?", type: "text" }], type: "paragraph" }],
    type: "doc",
  };
  const rows = runRows([
    treeOf(
      [ISSUE_NODE, commentNode(COMMENT), { ...runNode(RUN), text_tree: reading }],
      [changedBy(COMMENT, RUN)]
    ),
  ]);
  expect(rows.map((row) => row.excerpt)).toEqual(["Who ships the release?"]);
});

test("one run is one row however many writes it made", () => {
  const rows = runRows([
    treeOf(
      [ISSUE_NODE, commentNode(COMMENT), commentNode(SECOND_COMMENT), runNode(RUN)],
      [changedBy(KEY, RUN), changedBy(COMMENT, RUN), changedBy(SECOND_COMMENT, RUN)]
    ),
  ]);
  expect(rows.map((row) => row.key)).toEqual([RUN]);
});

test("rows stand newest first by made_at", () => {
  const newestFirst = runRows([
    treeOf(
      [ISSUE_NODE, runNode(OTHER_RUN, "2026-11-16T09:00:00Z"), runNode(RUN, ASKED_AT)],
      [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
    ),
  ]);
  expect(newestFirst.map((row) => row.key)).toEqual([RUN, OTHER_RUN]);

  // `RUN` sorts before `OTHER_RUN` by id, and stands after it in the read.
  const sameTime = runRows([
    treeOf(
      [ISSUE_NODE, runNode(OTHER_RUN, ASKED_AT), runNode(RUN, ASKED_AT)],
      [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
    ),
  ]);
  expect(sameTime.map((row) => row.key)).toEqual([RUN, OTHER_RUN]);
});

test("a run the read answers no made_at for stands after the runs it answers one for", () => {
  // `FIRST_RUN` sorts before `RUN`, and `RUN` before `OTHER_RUN`, by id: by id alone the run with
  // no time would stand second.
  const rows = runRows([
    treeOf(
      [
        ISSUE_NODE,
        runNode(RUN, null),
        runNode(OTHER_RUN, "2026-11-16T09:00:00Z"),
        runNode(FIRST_RUN, ASKED_AT),
      ],
      [changedBy(KEY, RUN), changedBy(KEY, OTHER_RUN), changedBy(KEY, FIRST_RUN)]
    ),
  ]);
  expect(rows[2]?.at, "the run with no time reads a time").toBeUndefined();
});

test("a tree read with no run draws no row", () => {
  // Another reader's tree: the tuple a run wrote, and no run, since only its asker reads it.
  expect(
    runRows([treeOf([ISSUE_NODE, commentNode(COMMENT)], [changedBy(COMMENT, RUN)])])
  ).toHaveLength(0);
  // A run that changed nothing of this issue.
  expect(
    runRows([
      treeOf(
        [ISSUE_NODE, commentNode(COMMENT), runNode(RUN)],
        [treeTuple("replies_to", COMMENT, KEY)]
      ),
    ])
  ).toHaveLength(0);
  // A tuple of the run's kind whose far end is no run, though a run's node holds its id: the
  // tuples are set after `treeAnswer`, which would name a held end by its node's kind.
  expect(
    runRows([
      {
        ...treeOf([ISSUE_NODE, commentNode(COMMENT), runNode(RUN)], []),
        tuples: [
          {
            from: { id: COMMENT, kind: "comment" },
            kind: "changed_by_run",
            to: { id: RUN, kind: "comment" },
          },
        ],
      },
    ])
  ).toHaveLength(0);
  // A read that holds no page yet.
  expect(runRows([])).toHaveLength(0);
});

test("the runs of the tree read's pages are rows, one per run, and a page of older comments adds none", () => {
  // The first page: the issue, and the two runs that changed it, `RUN` the newer.
  const first = treeOf(
    [ISSUE_NODE, runNode(OTHER_RUN, "2026-11-16T09:00:00Z"), runNode(RUN, ASKED_AT)],
    [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
  );
  // A page of older comments (`comments_before`): a comment and its tuples and no other node.
  // `RUN` changed the comment too, and `FIRST_RUN` changed that comment and nothing else, so no
  // page holds its node.
  const olderComments = treeOf(
    [commentNode(COMMENT)],
    [changedBy(COMMENT, RUN), changedBy(COMMENT, FIRST_RUN)]
  );
  expect(runRows([first, olderComments]).map((row) => row.key)).toEqual([RUN, OTHER_RUN]);
});
