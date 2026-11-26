import { afterEach, expect, spyOn, test } from "bun:test";
import { act, type RenderResult, waitFor } from "@testing-library/react";

import { quietConsoleError } from "../../../__tests__/act-warnings";
import { api } from "../../../api/client";
import type { Anchor } from "../../../api/types";
import { KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { readComment } from "../outline-fixtures";
import { inDocument, treeOf } from "../slots/stage-fixtures";
import { commentNode, treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import {
  answerSeededReads,
  columnOf,
  type DrawnPage,
  drawPage,
  newClient,
  ON_PLAN_TASK,
  ON_STATEMENT,
  PLAN_TASK,
  REPLY_ON_STATEMENT,
  RESULT,
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

/** Answers the tree read by its query, as the server does (`api/queries.ts`), over the reads
 *  `answerSeededReads` answers: Conversation's read, which names the kinds it reads
 *  (`CONVERSATION_KINDS`), holds no plan task and no result, nor the comment on the plan task's
 *  words; Graph's read, which names neither kinds nor a view, answers `graph` of the page's
 *  cursor; and a view's read answers as those reads answer it. Answers the cursor each Graph read
 *  asked for, undefined for its first page, in the order the page asked for them. */
function answerTreeByKind(
  graph: (cursor: string | undefined) => TreeAnswer | Promise<TreeAnswer>
): (string | undefined)[] {
  const conversation = treeOf(
    seededNodes([ON_STATEMENT], { without: [PLAN_TASK, RESULT] }),
    SERVES.filter((tuple) => tuple.from.id !== PLAN_TASK)
  );
  const tree = spyOn(api, "getIssueTree");
  const seeded = tree.getMockImplementation();
  if (seeded === undefined) throw new Error("the seeded reads answer no tree read");
  const graphReads: (string | undefined)[] = [];
  stubs.push(tree);
  tree.mockImplementation(async (key, query = {}, init) => {
    if (query.kind !== undefined) return conversation;
    if (query.view !== undefined) return seeded(key, query, init);
    graphReads.push(query.cursor);
    return graph(query.cursor);
  });
  return graphReads;
}

// R8, decision 452: Conversation's read holds no plan task and no comment on one, so it places the
// plan task's comment in no view, and the page reads Graph's read, which holds both: the address
// opens Graph at the comment. Until Graph's read answers, the address stays as it was, while every
// other read of the page answers, and Graph draws from the read the address sent.
test("a comment on a plan task that Conversation's read does not hold opens Graph, which holds it", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_PLAN_TASK] });
  const graph = Promise.withResolvers<TreeAnswer>();
  const graphReads = answerTreeByKind(() => graph.promise);
  const client = newClient();
  const task = ON_PLAN_TASK.comment.id;
  const view = drawPage(client, "conversation", { after: `?comment=${task}` });
  try {
    await waitFor(() =>
      expect(graphReads, "the page never asked for Graph's read").toHaveLength(1)
    );
    await waitFor(() => expect(client.isFetching(), "a read besides Graph's is out").toBe(1));
    expect(addressOf(view), "the address moved while Graph's read was out").toBe(
      `/issues/${KEY}?comment=${task}`
    );
    await act(async () => graph.resolve(treeOf(seededNodes([ON_STATEMENT, ON_PLAN_TASK]), SERVES)));
    await focusInThread(view, task, "the plan task's comment");
    expect(addressOf(view), "the address moved to Graph, at the comment").toBe(
      `/issues/${KEY}/graph?node=${task}`
    );
    expect(rowOf(view, ON_PLAN_TASK.node), "Graph draws the plan task").not.toBeNull();
    expect(graphReads, "Graph sent a read of its own").toHaveLength(1);
  } finally {
    view.finish();
    client.clear();
  }
});

// R8, decision 456: where Graph's read holds no node of the comment's words either - the plan task
// is gone - no view draws the thread, and the address opens the issue's own address with no line,
// once Graph's read, asked once, has answered.
test("a comment on a plan task that Conversation's read does not hold, where Graph's read holds no host either, opens the issue's own address with no line, and Graph's read was asked once", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_PLAN_TASK], without: [PLAN_TASK] });
  const graphReads = answerTreeByKind(() =>
    treeOf(
      seededNodes([ON_STATEMENT], { without: [PLAN_TASK] }),
      SERVES.filter((tuple) => tuple.from.id !== PLAN_TASK)
    )
  );
  const client = newClient();
  const view = drawPage(client, "conversation", { after: `?comment=${ON_PLAN_TASK.comment.id}` });
  try {
    await waitFor(() =>
      expect(addressOf(view), "the address kept the comment").toBe(`/issues/${KEY}`)
    );
    await waitFor(() =>
      expect(rowOf(view, ON_STATEMENT.node), "the outline is not drawn").not.toBeNull()
    );
    await waitFor(() => expect(client.isFetching(), "a read of the page is out").toBe(0));
    expect(
      view.queryByRole("heading", { name: /not found/i }),
      "a line says the comment is gone"
    ).toBeNull();
    expect(view.queryByText(/^Back to /), "a line leads back to the issue").toBeNull();
    expect(graphReads, "Graph's read was not asked once").toHaveLength(1);
  } finally {
    view.finish();
    client.clear();
  }
});

// R8 and part T's R15: Graph's first page holds no plan task; its next page, which the page reads
// for the address since Conversation's address reads no page of Graph's itself, brings the plan
// task and its comment, and the address opens Graph at the comment.
test("a comment on a plan task that only the next page of Graph's read holds opens Graph", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_PLAN_TASK] });
  const nodes = seededNodes([ON_STATEMENT, ON_PLAN_TASK]);
  // The plan task and the comment on its words, which Graph's next page holds.
  const onTask = nodes.filter(
    (node) => node.block_id === PLAN_TASK || node.id === ON_PLAN_TASK.comment.id
  );
  const graphReads = answerTreeByKind((cursor) =>
    cursor === undefined
      ? treeAnswer(
          [treeNode(KEY, "issue", null), ...nodes.filter((node) => !onTask.includes(node))],
          SERVES.filter((tuple) => tuple.from.id !== PLAN_TASK),
          { cursor: "graph-2" }
        )
      : treeAnswer(
          onTask,
          SERVES.filter((tuple) => tuple.from.id === PLAN_TASK),
          { cursor: null }
        )
  );
  const client = newClient();
  const task = ON_PLAN_TASK.comment.id;
  const view = drawPage(client, "conversation", { after: `?comment=${task}` });
  try {
    await focusInThread(view, task, "the comment on the plan task of Graph's next page");
    expect(addressOf(view), "the address moved to Graph, at the comment").toBe(
      `/issues/${KEY}/graph?node=${task}`
    );
    expect(rowOf(view, ON_PLAN_TASK.node), "Graph draws the plan task").not.toBeNull();
    expect(graphReads, "the pages of Graph's read the page asked for").toEqual([
      undefined,
      "graph-2",
    ]);
  } finally {
    view.finish();
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
        [...seededNodes([]), { ...commentNode(unplaced, KEY), anchor: treeAnchorOf(anchor) }],
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

// R8, decision 452 (G2): the address of a comment on a constraint's words opens Graph, since
// Conversation draws no row of a constraint (`CONVERSATION_KINDS` holds none) and Graph does. The
// browser row reads the address of a risk's comment, which stays in Conversation, on the real stack:
// a lens and its risk come from the lenses read, which a hand-made tree answer does not carry.
test("the address of a comment on a constraint's words opens Graph", async () => {
  const host = "constraint-1";
  const comment = readComment("on-constraint", 11);
  stubs = answerSeededReads({ posts: [comment], threads: [] });
  stubs.push(
    spyOn(api, "getIssueTree").mockImplementation(async () =>
      treeOf(
        [
          ...seededNodes([]),
          treeNode(host, "constraint", KEY, {
            attributes: { standard: "The standard holds." },
            text: "The standard holds where the standard is read.",
          }),
          {
            ...commentNode(comment, host),
            anchor: {
              node: { id: host, kind: "constraint", state: "live" },
              occurrence: 1,
              orphaned: false,
              quote: "standard",
            },
          },
        ],
        SERVES
      )
    )
  );
  quietConsoleError();
  const client = newClient();
  const view = drawPage(client, "conversation", { after: `?comment=${comment.id}` });
  try {
    await focusInThread(view, comment.id, "the constraint's comment");
    expect(addressOf(view), "the address of the constraint's comment").toBe(
      `/issues/${KEY}/graph?node=${comment.id}`
    );
  } finally {
    view.finish();
    client.clear();
  }
});
