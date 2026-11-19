import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { createElement } from "react";

import { api } from "../../../api/client";
import { issueTreeKey } from "../../../api/queries";
import { issueDetails } from "../decisions-filter/decisions-fixtures";
import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { plainWords } from "../tree-drafts";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";
import { type RunRowsRead, runRows, useRunRows } from "./run-references";

const KEY = "CORE-1";
const COMMENT = "7d1e4c2a-9b3f-4e8d-a6c5-2f0b1d9e7a34";
const SECOND_COMMENT = "8e2f5d3b-0c4a-4f9e-b7d6-3a1c2e0f8b45";
const RUN = "3f2b8c1e-5d4a-4b6f-9e2d-1a7c0b9e8f43";
const OTHER_RUN = "5a4d0e3f-7b6c-4d8e-9f1a-2b3c4d5e6f70";
const ASKED_AT = "2026-11-17T09:00:00Z";
const QUESTION = "When does the **release** ship?\n\nAnd who owns it?";

/** One node of the tree read, with every member the read always answers. */
function treeNode(
  partial: Partial<TreeAnswerNode> & Pick<TreeAnswerNode, "id" | "supertag">
): TreeAnswerNode {
  return {
    attributes: {},
    author: [{ id: "alice", kind: "user" }],
    made_at: "2026-11-16T08:00:00Z",
    parent: KEY,
    rank: null,
    state: "live",
    text: "",
    visibility: "project",
    ...partial,
  };
}

/** The issue's own node. */
const ISSUE_NODE = treeNode({ id: KEY, parent: null, supertag: "issue", text: "Ship the release" });

/** A comment of the issue. */
function commentNode(id: string): TreeAnswerNode {
  return treeNode({ id, supertag: "comment", text: "The release waits on the **build**." });
}

/** A command bar run, as the asker's tree read answers it: context, by its asker alone. */
function runNode(id: string, madeAt = ASKED_AT, text = QUESTION): TreeAnswerNode {
  return treeNode({
    id,
    made_at: madeAt,
    mark: "context",
    supertag: "interaction",
    text,
    visibility: "author",
  });
}

/** The tuple a run writes from a node it created or changed to itself. */
function changedBy(from: string, run: string): TreeTuple {
  return { from, kind: "changed_by_run", to: run };
}

/** The whole tree read, one page. */
function treeOf(nodes: readonly TreeAnswerNode[], tuples: readonly TreeTuple[]): TreeAnswer {
  return {
    converted: true,
    cursor: null,
    nodes: [...nodes],
    tree: "tree-core-1",
    tuples: [...tuples],
    view_of: null,
  };
}

test("an interaction of the tree read is a row with the page's words and the run's words", () => {
  const rows = runRows(
    treeOf([ISSUE_NODE, commentNode(COMMENT), runNode(RUN)], [changedBy(COMMENT, RUN)])
  );
  expect(rows).toHaveLength(1);
  const [row] = rows;
  expect(row.key).toBe(RUN);
  expect(row.kind).toBe(sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction));
  expect(row.edge).toBe(NODE_REFERENCES_WORDS.edges.changed_by_run);
  expect(row.excerpt).toBe(plainWords(QUESTION));
  expect(row.at).toBe(ASKED_AT);
  expect(row.href).toBe(`/runs/${RUN}`);
  const { href: _href, key: _key, ...words } = row;
  const drawn = JSON.stringify(words);
  expect(drawn).not.toContain("interaction");
  expect(drawn).not.toContain("changed_by_run");
  expect(drawn).not.toContain(RUN);
});

test("one run is one row however many writes it made", () => {
  const rows = runRows(
    treeOf(
      [ISSUE_NODE, commentNode(COMMENT), commentNode(SECOND_COMMENT), runNode(RUN)],
      [changedBy(KEY, RUN), changedBy(COMMENT, RUN), changedBy(SECOND_COMMENT, RUN)]
    )
  );
  expect(rows.map((row) => row.key)).toEqual([RUN]);
});

test("rows stand newest first by made_at", () => {
  const newestFirst = runRows(
    treeOf(
      [ISSUE_NODE, runNode(OTHER_RUN, "2026-11-16T09:00:00Z"), runNode(RUN, ASKED_AT)],
      [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
    )
  );
  expect(newestFirst.map((row) => row.key)).toEqual([RUN, OTHER_RUN]);

  // `RUN` sorts before `OTHER_RUN` by id, and stands after it in the read.
  const sameTime = runRows(
    treeOf(
      [ISSUE_NODE, runNode(OTHER_RUN, ASKED_AT), runNode(RUN, ASKED_AT)],
      [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
    )
  );
  expect(sameTime.map((row) => row.key)).toEqual([RUN, OTHER_RUN]);
});

test("a tree read with no run draws no row", () => {
  // Another reader's tree: the tuple a run wrote, and no run, since only its asker reads it.
  expect(
    runRows(treeOf([ISSUE_NODE, commentNode(COMMENT)], [changedBy(COMMENT, RUN)]))
  ).toHaveLength(0);
  // A run that changed nothing of this issue.
  expect(
    runRows(
      treeOf(
        [ISSUE_NODE, commentNode(COMMENT), runNode(RUN)],
        [{ from: COMMENT, kind: "replies_to", to: KEY }]
      )
    )
  ).toHaveLength(0);
  expect(runRows(undefined)).toHaveLength(0);
});

test("the hook reads the cached tree and sends no request", async () => {
  const getIssueTree = spyOn(api, "getIssueTree").mockResolvedValue(treeOf([ISSUE_NODE], []));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const issue = issueDetails();
  let read: RunRowsRead | undefined;
  function Rows() {
    read = useRunRows(issue);
    return null;
  }
  const view = render(createElement(QueryClientProvider, { client }, createElement(Rows)));
  try {
    expect(read?.settled).toBe(false);
    expect(read?.rows).toHaveLength(0);

    act(() => {
      client.setQueryData(
        issueTreeKey(issue.key),
        treeOf([ISSUE_NODE, commentNode(COMMENT), runNode(RUN)], [changedBy(COMMENT, RUN)])
      );
    });
    await waitFor(() => expect(read?.settled).toBe(true));
    expect(read?.rows.map((row) => row.key)).toEqual([RUN]);
    expect(getIssueTree).toHaveBeenCalledTimes(0);
  } finally {
    view.unmount();
    client.clear();
    getIssueTree.mockRestore();
  }
});
