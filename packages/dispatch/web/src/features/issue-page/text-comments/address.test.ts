import { afterEach, expect, spyOn, test } from "bun:test";
import { type RenderResult, waitFor } from "@testing-library/react";

import { quietConsoleError } from "../../../__tests__/act-warnings";
import { api } from "../../../api/client";
import type { Anchor } from "../../../api/types";
import { KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { readComment } from "../outline-fixtures";
import { inDocument, treeOf } from "../slots/stage-fixtures";
import { treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import {
  answerSeededReads,
  columnOf,
  type DrawnPage,
  drawPage,
  newClient,
  ON_PLAN_TASK,
  ON_STATEMENT,
  REPLY_ON_STATEMENT,
  rowOf,
  SERVES,
  STATEMENT,
  seededNodes,
  threadOn,
  treeAnchorOf,
  VERIFICATION_DOCUMENT,
} from "./margin-fixtures";

/**
 * R8: an address that names a comment or a message on text, or a reply in its thread, opens the
 * thread's node and the thread beside it, with the focus on the item. The page is drawn by its own
 * root (`drawPage`) at the address, so the outline, the column and the router are the page's. An
 * address of a message needs a message on text, which comes with INFRA-5703's reply quote (G4):
 * the browser row reads it there.
 */

const COLUMN = "Comments on text";

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** The address the router stands at, as the page last drew it. */
function addressOf(view: DrawnPage): string {
  const at = view.held.location;
  return at === undefined ? "" : `${at.pathname}${at.search}`;
}

/** Waits for the column, and for the focus to be in the card of the thread whose root is `root`;
 *  answers the element that holds it. */
async function focusInThread(view: RenderResult, root: string, what: string): Promise<Element> {
  expect(await columnOf(view, COLUMN, 8000), `${what}: no column is drawn`).not.toBeNull();
  return waitFor(() => {
    const held = document.activeElement;
    if (held?.closest(`[data-margin-item="${root}"]`) == null) {
      throw new Error(`${what}: the focus is not in the thread of ${root}`);
    }
    return held;
  });
}

// R8, decision 452: the address of a comment on text opens Conversation where Conversation draws
// the thread's node, and Graph where it does not: a plan task with no step is a row of Plan and of
// Graph, and none of Conversation. A reply in a thread opens its thread, with the focus on it.
test("a comment address on text opens Conversation where it draws the node, else Graph", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_PLAN_TASK] });
  // The page leaves Conversation for Graph while the outline's own landing read of the item is out,
  // and React warns that a store of the old outline changed as the new one rendered
  // (`useAddressFallback`'s cache subscription).
  quietConsoleError();
  const client = newClient();
  const reply = REPLY_ON_STATEMENT.id;
  const statement = drawPage(client, "conversation", { after: `/comments/${reply}` });
  try {
    const held = await focusInThread(statement, ON_STATEMENT.comment.id, "the reply's address");
    expect(held.tagName, "the focus is not on a reply's row").toBe("LI");
    expect(held.getAttribute("aria-current"), "the reply is not the one the address names").toBe(
      "true"
    );
    expect(
      held.closest('ol[aria-label="Replies"]'),
      "the focused row is not a row of the thread's replies"
    ).not.toBeNull();
    expect(addressOf(statement), "Conversation kept its address").toBe(
      `/issues/${KEY}/comments/${reply}`
    );
    expect(
      rowOf(statement, ON_STATEMENT.node),
      "Conversation draws the statement the thread is on"
    ).not.toBeNull();
  } finally {
    statement.finish();
  }
  const task = ON_PLAN_TASK.comment.id;
  const planTask = drawPage(client, "conversation", { after: `?comment=${task}` });
  try {
    await focusInThread(planTask, task, "the plan task's comment");
    expect(addressOf(planTask), "the address moved to Graph, at the comment").toBe(
      `/issues/${KEY}/graph?node=${task}`
    );
    expect(rowOf(planTask, ON_PLAN_TASK.node), "Graph draws the plan task").not.toBeNull();
  } finally {
    planTask.finish();
    client.clear();
  }
});

/** The checks the statement holds past the first page of the tree read, a block of the
 *  Verification document each. */
function checks(from: number, to: number): TreeAnswerNode[] {
  return Array.from({ length: to - from + 1 }, (_unused, at) => {
    const number = from + at;
    return treeNode(`ck${number}`, "check", STATEMENT, {
      ...inDocument(VERIFICATION_DOCUMENT, 10 + number),
      text: `Check ${number} holds.`,
    });
  });
}

// R8 and part T's R15: the address names a node the outline's first page does not hold, so the
// outline reads the next page, and draws the node's page of children, which is every child up to
// it, with the thread beside it and the focus in it.
test("a thread on the 55th child draws that child's page", async () => {
  const thread = threadOn("on-ck55", VERIFICATION_DOCUMENT, "ck55", "holds", 7);
  const reads = { threads: [thread] };
  stubs = answerSeededReads(reads);
  const serves = (from: number, to: number) =>
    checks(from, to).map((check) => treeTuple("serves", check.block_id ?? check.id, STATEMENT));
  const asked: (string | undefined)[] = [];
  stubs.push(
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
      asked.push(query.cursor);
      if (query.cursor === undefined) {
        return treeAnswer(
          [treeNode(KEY, "issue", null), ...seededNodes([]), ...checks(1, 40)],
          [...SERVES, ...serves(1, 40)],
          { cursor: "page-2" }
        );
      }
      return treeAnswer(
        [
          ...checks(41, 60),
          treeNode(thread.comment.id, "comment", "ck55", {
            ...(thread.comment.anchor === null
              ? {}
              : { anchor: treeAnchorOf(thread.comment.anchor) }),
            author: [thread.comment.author],
            made_at: thread.comment.created_at,
            text: thread.comment.body,
          }),
        ],
        serves(41, 60),
        { cursor: null }
      );
    })
  );
  const client = newClient();
  const view = drawPage(client, "conversation", { after: `/comments/${thread.comment.id}` });
  try {
    await focusInThread(view, thread.comment.id, "the address of the thread on the 55th check");
    expect(
      asked.filter((cursor) => cursor !== undefined),
      "the outline read the next page of the tree read, once"
    ).toEqual(["page-2"]);
    expect(rowOf(view, thread.node), "the 55th check's row is drawn").not.toBeNull();
    expect(addressOf(view), "Conversation kept its address").toBe(
      `/issues/${KEY}/comments/${thread.comment.id}`
    );
  } finally {
    view.finish();
    client.clear();
  }
});

// R8, decision 456: a comment on text whose words are in no node of the read and which the read
// hangs under no node a view draws stands in no margin, so no view draws its node: the address
// opens the issue's own address, with no line saying so (Threads takes it at G8).
test("an address whose node no view draws opens the issue's address with no line", async () => {
  const anchor: Anchor = {
    artifact_id: SPEC_DOCUMENT.id,
    block_id: "gone",
    mark_id: "",
    orphaned: true,
    quote: "words that are gone",
    version: 1,
  };
  const orphan = { ...readComment("on-gone", 9), anchor };
  stubs = answerSeededReads({ posts: [orphan], threads: [] });
  stubs.push(
    spyOn(api, "getIssueTree").mockImplementation(async () =>
      treeOf(
        [
          ...seededNodes([]),
          treeNode(orphan.id, "comment", "paged-away", {
            anchor: treeAnchorOf(anchor),
            author: [orphan.author],
            made_at: orphan.created_at,
            text: orphan.body,
          }),
        ],
        SERVES
      )
    )
  );
  const client = newClient();
  const view = drawPage(client, "conversation", { after: `/comments/${orphan.id}` });
  try {
    await waitFor(() =>
      expect(addressOf(view), "the address kept the comment").toBe(`/issues/${KEY}`)
    );
    await waitFor(() =>
      expect(rowOf(view, ON_STATEMENT.node), "the outline is not drawn").not.toBeNull()
    );
    expect(
      view.queryByRole("heading", { name: /not found/i }),
      "a line says the comment is gone"
    ).toBeNull();
    expect(view.queryByText(/^Back to /), "a line leads back to the issue").toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});

// R1, R8: a comment anchored in a block no node of the answer stands for, which the read hangs
// under the issue, is on text and stands in no margin: its address opens the issue's own address
// with no line.
test("an address of a comment anchored in an undrawn block under the issue opens the issue with no line", async () => {
  const anchor: Anchor = {
    artifact_id: SPEC_DOCUMENT.id,
    block_id: "undrawn",
    mark_id: "m-undrawn",
    orphaned: false,
    quote: "undrawn words",
    version: 1,
  };
  const unplaced = { ...readComment("on-undrawn", 10), anchor };
  stubs = answerSeededReads({ posts: [unplaced], threads: [] });
  stubs.push(
    spyOn(api, "getIssueTree").mockImplementation(async () =>
      treeOf(
        [
          ...seededNodes([]),
          treeNode(unplaced.id, "comment", KEY, {
            anchor: treeAnchorOf(anchor),
            author: [unplaced.author],
            made_at: unplaced.created_at,
            text: unplaced.body,
          }),
        ],
        SERVES
      )
    )
  );
  const client = newClient();
  const view = drawPage(client, "conversation", { after: `?comment=${unplaced.id}` });
  try {
    await waitFor(() =>
      expect(addressOf(view), "the address kept the unplaced comment").toBe(`/issues/${KEY}`)
    );
    await waitFor(() =>
      expect(rowOf(view, ON_STATEMENT.node), "the outline is not drawn").not.toBeNull()
    );
    expect(
      view.queryByRole("heading", { name: /not found/i }),
      "a line says the comment is gone"
    ).toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});
