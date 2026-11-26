import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { realClock, settle, tick } from "../__tests__/fake-clock";
import {
  type TreeNodeExtra,
  treeAnswer,
  treeNode,
} from "../features/issue-page/tree-read/tree-fixtures";
import { type TreeAnswer, viewKey } from "../features/issue-page/tree-read/tree-read";
import { eventRefreshTargets } from "./event-refresh";
import { issueTreeKey } from "./queries";
import { refreshQueries, trackQueryFetches } from "./query-refresh";
import type { DispatchEvent, IssueTreeParent } from "./types";

/**
 * The issue page's tree read is told of every event of its issue but a person's edit notice: each
 * one names the tree's key (`issueTreeKey`), by prefix, so the views under it
 * (`["issue-tree", key, view]`) are reached too. An event of another issue reaches a tree or a
 * view where its answer holds a node of that issue, or a node standing under one, of a kind the
 * event can move - for an ask's event, one the ask is on - and an issue write that may have moved
 * its issue's parent each ancestor's tree it names; an event of no issue reaches none.
 */

/** An event of `type`, with the payload its reader reads; the feed's union is wider than this
 *  test needs. */
function eventOf(
  type: string,
  payload: Record<string, unknown>,
  issueKey: string | null
): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    artifact_id: typeof payload.artifact_id === "string" ? payload.artifact_id : null,
    created_at: "2026-11-05T00:00:00Z",
    id: 1,
    issue_key: issueKey,
    notify: true,
    payload,
    project: "CORE",
    seq: 1,
    type,
  } as unknown as DispatchEvent;
}

const ISSUE_EVENTS: readonly DispatchEvent[] = [
  eventOf("comment.created", { body: "A reply.", id: "comment-1", kind: "comment" }, "CORE-1"),
  eventOf("decision.kept", { block_id: "block-1" }, "CORE-1"),
  eventOf("issue.updated", { changed: ["status"] }, "CORE-1"),
  eventOf("artifact.version", { artifact_id: "artifact-1", number: 2 }, "CORE-1"),
  eventOf("ask.opened", { id: "ask-1" }, "CORE-1"),
  eventOf("issue.claimed", {}, "CORE-1"),
  eventOf("handoff.recorded", {}, "CORE-1"),
  eventOf("child.status", {}, "CORE-1"),
];

test("every event of an issue names its tree read, by prefix", () => {
  for (const event of ISSUE_EVENTS) {
    expect(eventRefreshTargets(event, "alice"), event.type).toContainEqual({
      queryKey: issueTreeKey("CORE-1"),
    });
  }
});

test("an event of an issue marks its tree and each view stale, and no other issue's", () => {
  const client = new QueryClient();
  const tree = issueTreeKey("CORE-1");
  const view = [...tree, "spec"];
  const other = issueTreeKey("CORE-2");
  for (const queryKey of [tree, view, other]) client.setQueryData(queryKey, { nodes: [] });
  refreshQueries(client, eventRefreshTargets(ISSUE_EVENTS[0] as DispatchEvent, "alice"));
  expect(client.getQueryState(tree)?.isInvalidated, "the tree").toBe(true);
  expect(client.getQueryState(view)?.isInvalidated, "a view under it").toBe(true);
  expect(client.getQueryState(other)?.isInvalidated, "another issue's tree").toBe(false);
  const issueless = eventOf("project.updated", {}, null);
  expect(
    eventRefreshTargets(issueless, "alice").some((target) => target.queryKey[0] === "issue-tree"),
    "an event of no issue"
  ).toBe(false);
});

// A person's edit notice comes once the document has been quiet after the edits it tells of,
// each of which sent its own version: it moves nothing the issue's tree or a view of it draws,
// so it reads neither, while the issue's detail and log take its line as before.
test("a person's edit notice marks no tree or view of its issue, and marks its detail and its log", () => {
  const client = new QueryClient();
  const tree = issueTreeKey("CORE-1");
  const plan = viewKey("CORE-1", "plan");
  for (const queryKey of [tree, plan]) {
    client.setQueryData(queryKey, treeAnswer([treeNode("CORE-1", "issue", null)]));
  }
  client.setQueryData(["issue", "CORE-1"], { key: "CORE-1" });
  client.setQueryData(["events", "CORE-1"], { pageParams: [null], pages: [[]] });
  const notice = eventOf(
    "artifact.human_edited",
    { artifact_id: "artifact-1", from_version: 4, to_version: 7 },
    "CORE-1"
  );
  refreshQueries(client, eventRefreshTargets(notice, "alice"));
  expect({
    detail: client.getQueryState(["issue", "CORE-1"])?.isInvalidated,
    log: client.getQueryState(["events", "CORE-1"])?.isInvalidated,
    plan: client.getQueryState(plan)?.isInvalidated,
    tree: client.getQueryState(tree)?.isInvalidated,
  }).toEqual({ detail: true, log: true, plan: false, tree: false });
});

/** The child CORE-2's Verification view, which carries its parent CORE-1's requirement as
 *  context, read-only, as the route answers it. */
function viewHoldingParent(client: QueryClient): readonly unknown[] {
  const childView = viewKey("CORE-2", "verification");
  const requirement = treeNode("r1", "requirement", null, {
    in: "spec",
    issue: "CORE-1",
    read_only: true,
    role: "context",
    text: "The outline draws every child.",
  });
  client.setQueryData(
    childView,
    treeAnswer([treeNode("CORE-2", "issue", null), requirement], [], {
      issue: "CORE-2",
      view: "verification",
    })
  );
  return childView;
}

test("an event of a parent's spec marks stale a child's view that holds a node of the parent, and no tree that holds none", () => {
  const client = new QueryClient();
  const childView = viewHoldingParent(client);
  // The child's whole tree and another issue's tree, each holding nodes of its own issue alone.
  const childTree = issueTreeKey("CORE-2");
  client.setQueryData(
    childTree,
    treeAnswer([treeNode("CORE-2", "issue", null)], [], { issue: "CORE-2" })
  );
  const other = issueTreeKey("CORE-3");
  client.setQueryData(
    other,
    treeAnswer(
      [treeNode("CORE-3", "issue", null), treeNode("comment-3", "comment", "CORE-3")],
      [],
      { issue: "CORE-3" }
    )
  );
  const specVersion = eventOf(
    "artifact.version",
    { artifact_id: "artifact-spec", number: 2 },
    "CORE-1"
  );
  refreshQueries(client, eventRefreshTargets(specVersion, "alice"));
  expect(
    client.getQueryState(childView)?.isInvalidated,
    "the child's view that holds the parent's requirement"
  ).toBe(true);
  expect(
    client.getQueryState(childTree)?.isInvalidated,
    "the child's tree, which holds no node of the parent"
  ).toBe(false);
  expect(client.getQueryState(other)?.isInvalidated, "another issue's tree").toBe(false);
});

// An issue write with no `ancestors` - one an older server wrote mid-rollout, or one written
// before the field existed and replayed with the feed's gap - falls back to what any ancestor
// could be, as the issue's detail does (every loaded detail refreshes). A new child's create is
// the one event that reaches its parent: the parent's tree holds no node of the child yet.
test("an issue write that names no ancestors marks every held tree stale, as it marks every issue's detail", () => {
  const client = new QueryClient();
  const parent = issueTreeKey("CORE-1");
  client.setQueryData(
    parent,
    treeAnswer([treeNode("CORE-1", "issue", null)], [], { issue: "CORE-1" })
  );
  client.setQueryData(["issue", "CORE-1"], { key: "CORE-1" });
  const created = eventOf("issue.created", { parent: "CORE-1" }, "CORE-2");
  refreshQueries(client, eventRefreshTargets(created, "alice"));
  expect(
    {
      detail: client.getQueryState(["issue", "CORE-1"])?.isInvalidated,
      tree: client.getQueryState(parent)?.isInvalidated,
    },
    "the parent's detail and its tree, after a child's create that names no ancestors"
  ).toEqual({ detail: true, tree: true });
});

/** What the route marks each node of the child CORE-2 with in an answer of another issue. */
const CHILD: TreeNodeExtra = { issue: "CORE-2", read_only: true };

/** The parent CORE-1's requirement, as its outline and its Verification view hold it. */
const REQUIREMENT = treeNode("r1", "requirement", "CORE-1", {
  in: "spec",
  text: "Every child is drawn.",
});

/** The parent CORE-1's outline, holding its child CORE-2's node and no other node of the child. */
const OUTLINE_WITH_CHILD = treeAnswer([
  treeNode("CORE-1", "issue", null),
  REQUIREMENT,
  treeNode("CORE-2", "issue", "CORE-1", { ...CHILD, text: "Draw each child" }),
]);

/** The parent CORE-1's Verification view, holding its requirement alone: no node of an issue, so
 *  only its outline can say it holds a node an event of another issue moves. */
const VERIFICATION_WITH_REQUIREMENT = treeAnswer([REQUIREMENT], [], { view: "verification" });

/** The parent CORE-1's answers that hold nodes of its child CORE-2, as the route answers them:
 *  its outline, which holds the child's node, a check of the child's that serves the parent's
 *  requirement under that node, and a result under the check; and its Verification view, which
 *  holds the requirement, the check and the result, the check under the child's node, which
 *  that view does not hold. */
function holdingChild(client: QueryClient): {
  readonly outline: readonly unknown[];
  readonly verification: readonly unknown[];
} {
  const check = (parent: string | IssueTreeParent) =>
    treeNode("k1", "check", parent, { ...CHILD, in: "verification", text: "Draw CORE-2." });
  const result = treeNode("result-1", "result", "k1", CHILD);
  const outline = issueTreeKey("CORE-1");
  client.setQueryData(
    outline,
    treeAnswer([
      treeNode("CORE-1", "issue", null),
      REQUIREMENT,
      treeNode("CORE-2", "issue", "CORE-1", { ...CHILD, text: "Draw each child" }),
      check("CORE-2"),
      result,
    ])
  );
  const verification = viewKey("CORE-1", "verification");
  const childNode: IssueTreeParent = {
    first_line: "Draw each child",
    id: "CORE-2",
    issue: "CORE-2",
    kind: "issue",
    supertag: "issue",
  };
  client.setQueryData(
    verification,
    treeAnswer([treeNode("CORE-1", "issue", null), REQUIREMENT, check(childNode), result], [], {
      view: "verification",
    })
  );
  return { outline, verification };
}

// A child's comment, message or follower moves nothing of the child's that its parent's answers
// hold - its node, its check, the result under the check - and no answer holds a comment of
// another issue, so neither answer is told of it. A version of the child's documents writes its
// checks, and a result stands under its check: both reach each answer that holds the check.
test("a child's comment or message marks stale no tree of its parent that holds the child's nodes, and its document version or result does", () => {
  const cases: readonly (readonly [DispatchEvent, boolean])[] = [
    [
      eventOf("comment.created", { body: "A reply.", id: "comment-2", kind: "comment" }, "CORE-2"),
      false,
    ],
    [eventOf("comment.edited", { body: "A reply, reworded.", id: "comment-2" }, "CORE-2"), false],
    [
      eventOf("message.created", { body: "Look at the outline.", id: "message-2" }, "CORE-2"),
      false,
    ],
    [eventOf("ask.follower_added", { ask_id: "ask-2", follower: "bob" }, "CORE-2"), false],
    [eventOf("artifact.version", { artifact_id: "artifact-2", number: 2 }, "CORE-2"), true],
    [
      eventOf(
        "result.recorded",
        { check_block_id: "k1", check_issue: "CORE-2", issue: "CORE-2", verdict: "pass" },
        "CORE-2"
      ),
      true,
    ],
  ];
  for (const [event, marks] of cases) {
    const client = new QueryClient();
    const { outline, verification } = holdingChild(client);
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    expect(
      {
        outline: client.getQueryState(outline)?.isInvalidated,
        verification: client.getQueryState(verification)?.isInvalidated,
      },
      event.type
    ).toEqual({ outline: marks, verification: marks });
  }
});

// A descendant's view carries its ancestor's requirement as context, and a version of the
// ancestor's documents, which can move the requirement, marks the view stale (the case of a
// parent's spec above). The ancestor's comment or message moves no node of the ancestor's that
// the view holds, and the view holds no comment of another issue, so neither marks it stale.
test("an ancestor's comment or message marks stale no descendant's view that holds the ancestor's requirement", () => {
  const events: readonly DispatchEvent[] = [
    eventOf("comment.created", { body: "A reply.", id: "comment-1", kind: "comment" }, "CORE-1"),
    eventOf("comment.edited", { body: "A reply, reworded.", id: "comment-1" }, "CORE-1"),
    eventOf("message.created", { body: "Look at the spec.", id: "message-1" }, "CORE-1"),
  ];
  for (const event of events) {
    const client = new QueryClient();
    const childView = viewHoldingParent(client);
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    expect(client.getQueryState(childView)?.isInvalidated, event.type).toBe(false);
  }
});

// A write of a child or a grandchild that moves nothing the parent's answers hold of it - its
// status, assignee, priority, labels, or nothing at all - leaves the parent's tree and views
// alone: the tree's issue node carries none of those fields, and the child row draws its status
// and holder from the parent's issue detail, which the write refreshes by its `ancestors`. The
// event names each ancestor, as the server writes it; only a create or a parent move can put the
// issue under a tree whose answer does not hold it yet.
test("a write of a child that moves nothing its parent's answers hold of it marks no tree or view of the parent stale", () => {
  const cases: readonly (readonly [string, Record<string, unknown>, string])[] = [
    ["status", { ancestors: ["CORE-1"], changed: ["status"] }, "CORE-2"],
    ["assignee", { ancestors: ["CORE-1"], changed: ["assignee"] }, "CORE-2"],
    ["priority", { ancestors: ["CORE-1"], changed: ["priority"] }, "CORE-2"],
    ["labels", { ancestors: ["CORE-1"], changed: ["labels"] }, "CORE-2"],
    ["nothing", { ancestors: ["CORE-1"], changed: [] }, "CORE-2"],
    ["a grandchild's status", { ancestors: ["CORE-2", "CORE-1"], changed: ["status"] }, "CORE-3"],
  ];
  for (const [moved, payload, key] of cases) {
    const client = new QueryClient();
    const { outline, verification } = holdingChild(client);
    client.setQueryData(["issue", "CORE-1"], { key: "CORE-1" });
    refreshQueries(client, eventRefreshTargets(eventOf("issue.updated", payload, key), "alice"));
    expect(
      {
        detail: client.getQueryState(["issue", "CORE-1"])?.isInvalidated,
        outline: client.getQueryState(outline)?.isInvalidated,
        verification: client.getQueryState(verification)?.isInvalidated,
      },
      `a write of ${key} that moves ${moved}`
    ).toEqual({ detail: true, outline: false, verification: false });
  }
});

// A write of a child reads each ancestor's tree it names, and every view of it, where it may have
// moved the child's parent: a create under the ancestor and a parent move put the child's nodes in
// an answer that holds none of them yet, and a write that does not say what it moved may have
// moved the parent. A write that moved no field reads none of them.
test("an issue write marks stale each ancestor's tree it names only where it may have moved the issue's parent", () => {
  const cases: readonly (readonly [string, string, Record<string, unknown>, boolean])[] = [
    [
      "a create under the parent",
      "issue.created",
      { ancestors: ["CORE-1"], changed: ["title", "status", "parent"] },
      true,
    ],
    ["a parent move", "issue.updated", { ancestors: ["CORE-1"], changed: ["parent"] }, true],
    ["a write that does not say what it moved", "issue.updated", { ancestors: ["CORE-1"] }, true],
    ["a write that moved no field", "issue.updated", { ancestors: ["CORE-1"], changed: [] }, false],
  ];
  for (const [name, type, payload, marks] of cases) {
    const client = new QueryClient();
    const parent = issueTreeKey("CORE-1");
    const spec = viewKey("CORE-1", "spec");
    client.setQueryData(
      parent,
      treeAnswer([treeNode("CORE-1", "issue", null)], [], { issue: "CORE-1" })
    );
    client.setQueryData(
      spec,
      treeAnswer([treeNode("r1", "requirement", null, { in: "spec" })], [], { view: "spec" })
    );
    refreshQueries(client, eventRefreshTargets(eventOf(type, payload, "CORE-2"), "alice"));
    expect(
      {
        spec: client.getQueryState(spec)?.isInvalidated,
        tree: client.getQueryState(parent)?.isInvalidated,
      },
      name
    ).toEqual({ spec: marks, tree: marks });
  }
});

// A child's title is a member of the node its parent's outline holds of it, so the outline reads
// it again through that node. A grandparent's answer holds no node of the child - it lists its own
// children alone - so neither its outline nor a view of it reads the title again, though the
// write names the grandparent among the child's ancestors.
test("a child's title change marks its parent's outline that holds its node, and no grandparent's outline or view that holds none", () => {
  const client = new QueryClient();
  const { outline } = holdingChild(client);
  const grandparent = issueTreeKey("CORE-0");
  const grandparentSpec = viewKey("CORE-0", "spec");
  client.setQueryData(
    grandparent,
    treeAnswer(
      [
        treeNode("CORE-0", "issue", null),
        treeNode("CORE-1", "issue", "CORE-0", { issue: "CORE-1", read_only: true }),
      ],
      [],
      { issue: "CORE-0" }
    )
  );
  client.setQueryData(
    grandparentSpec,
    treeAnswer([treeNode("r0", "requirement", null, { in: "spec" })], [], {
      issue: "CORE-0",
      view: "spec",
    })
  );
  const retitled = eventOf(
    "issue.updated",
    { ancestors: ["CORE-1", "CORE-0"], changed: ["title"], title: "Draw each child, once" },
    "CORE-2"
  );
  refreshQueries(client, eventRefreshTargets(retitled, "alice"));
  expect({
    grandparent: client.getQueryState(grandparent)?.isInvalidated,
    grandparentSpec: client.getQueryState(grandparentSpec)?.isInvalidated,
    parent: client.getQueryState(outline)?.isInvalidated,
  }).toEqual({ grandparent: false, grandparentSpec: false, parent: true });
});

// An ask's event moves only what the ask is on - the block it is written as, or the block its
// anchor's mark is in, each matched by its document and its block id together, and every node of
// a document it is anchored in with no block - and a block found invalid or repaired only that
// block. So a tree of the parent reads a child's ask event again where its answer holds one of
// those. A plain ask of the child, with no block and no anchor, is on none of them, and neither
// is a block ask on another block of the document of the child's check, nor an ask anchored on a
// block of the check's id in another document of the child.
test("a child's ask event marks stale a tree of its parent only where its answer holds what the ask is on", () => {
  const plain = { anchor: null, id: "ask-2", question: "Which outline?", state: "open" };
  const anchoredIn = (artifact: string, slug: string, block: string | null) => ({
    anchor: {
      artifact_id: artifact,
      block_id: block,
      mark_id: "mark-1",
      orphaned: false,
      quote: "Draw",
      version: 2,
    },
    anchor_artifact: { name: `${slug}.md`, primary: false, project: "CORE", slug },
  });
  const cases: readonly (readonly [string, DispatchEvent, boolean])[] = [
    ["a plain ask opened", eventOf("ask.opened", plain, "CORE-2"), false],
    [
      "a plain ask answered",
      eventOf("ask.answered", { ...plain, state: "answered" }, "CORE-2"),
      false,
    ],
    [
      "a block ask written on another block of the document of the child's check",
      eventOf(
        "ask.opened",
        {
          ...plain,
          block_artifact: { id: "artifact-2", primary: false, slug: "verification" },
          block_id: "q1",
        },
        "CORE-2"
      ),
      false,
    ],
    [
      "an ask anchored in the child's check",
      eventOf(
        "ask.edited",
        { ...plain, ...anchoredIn("artifact-2", "verification", "k1") },
        "CORE-2"
      ),
      true,
    ],
    [
      "an ask anchored on a block of the check's id in another document of the child",
      eventOf("ask.edited", { ...plain, ...anchoredIn("artifact-9", "spec", "k1") }, "CORE-2"),
      false,
    ],
    [
      "an ask anchored in the document of the child's check, in no block of it",
      eventOf(
        "ask.opened",
        { ...plain, ...anchoredIn("artifact-2", "verification", null) },
        "CORE-2"
      ),
      true,
    ],
    [
      "the child's check found invalid",
      eventOf(
        "block.invalid",
        { block_id: "k1", block_type: "check", reason: "no such job", version: 3 },
        "CORE-2"
      ),
      true,
    ],
    [
      "another block of the child repaired",
      eventOf("block.repaired", { block_id: "q9", version: 3 }, "CORE-2"),
      false,
    ],
  ];
  for (const [name, event, marks] of cases) {
    const client = new QueryClient();
    const { outline, verification } = holdingChild(client);
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    expect(
      {
        outline: client.getQueryState(outline)?.isInvalidated,
        verification: client.getQueryState(verification)?.isInvalidated,
      },
      name
    ).toEqual({ outline: marks, verification: marks });
  }
});

/** The child CORE-2's ask block q7 in its Decisions document, as a node standing under it names
 *  it. */
const ASK_BLOCK: IssueTreeParent = {
  first_line: "Which outline?",
  id: "CORE-2/decisions#q7",
  issue: "CORE-2",
  kind: "block",
  supertag: "ask",
};

/** An event of `type` of the child CORE-2's block ask q7, in `state`, written in the document
 *  `document` names: its Decisions document unless the case says otherwise, or none, as an event
 *  settlement writes names none. */
function blockAsk(
  type: string,
  state: string,
  document: Record<string, unknown> = {
    block_artifact: { id: "artifact-decisions", primary: false, slug: "decisions" },
  }
): DispatchEvent {
  return eventOf(
    type,
    { anchor: null, block_id: "q7", id: "ask-7", question: "Which outline?", state, ...document },
    "CORE-2"
  );
}

// A block ask is on its block and on a node standing under it, which names the block by its
// address (`<KEY>/<slug>#<id>`): a block id is the block's id within its document alone, so a
// node standing under a block of that id in another document of the child is under no ask. An
// event settlement writes names no document, and is on each block of the child of that id.
test("a child's block ask marks stale a tree of its parent that holds a node standing under the ask's block, and none whose node stands under a block of that id in another document", () => {
  const seen: Record<string, boolean> = {};
  for (const [name, event] of [
    ["the ask's block", blockAsk("ask.opened", "open")],
    [
      "a block of its id in another document",
      blockAsk("ask.opened", "open", {
        block_artifact: { id: "artifact-notes", primary: false, slug: "notes" },
      }),
    ],
    [
      "the ask's block, from settlement, which names no document",
      blockAsk("ask.opened", "open", {}),
    ],
  ] as const) {
    const client = new QueryClient();
    const outline = issueTreeKey("CORE-1");
    client.setQueryData(
      outline,
      treeAnswer([
        ...(OUTLINE_WITH_CHILD.nodes ?? []),
        treeNode("c1", "criterion", ASK_BLOCK, { ...CHILD, in: "decisions", text: "One outline." }),
      ])
    );
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    seen[name] = client.getQueryState(outline)?.isInvalidated === true;
  }
  expect(seen).toEqual({
    "a block of its id in another document": false,
    "the ask's block": true,
    "the ask's block, from settlement, which names no document": true,
  });
});

// A child's view carries as context an ask of its parent's that a person answered and that
// shaped a node of the view - the parent's requirement it carries - and the ask's event, which
// can reword it or take its answer back, reaches the view by the ask's own node, wherever the
// ask's typed block is written.
test("an ancestor's ask event marks stale a descendant's view that carries the answered ask", () => {
  const client = new QueryClient();
  const childView = viewKey("CORE-2", "spec");
  client.setQueryData(
    childView,
    treeAnswer(
      [
        treeNode("CORE-2", "issue", null),
        treeNode("r1", "requirement", null, {
          in: "spec",
          issue: "CORE-1",
          read_only: true,
          role: "context",
        }),
        treeNode("ask-1", "ask", null, { decision: {}, role: "context", text: "Which outline?" }),
      ],
      [],
      { issue: "CORE-2", view: "spec" }
    )
  );
  const reworded = eventOf(
    "ask.edited",
    {
      anchor: null,
      block_artifact: { id: "artifact-decisions", primary: false, slug: "decisions" },
      block_id: "q1",
      id: "ask-1",
    },
    "CORE-1"
  );
  refreshQueries(client, eventRefreshTargets(reworded, "alice"));
  expect(client.getQueryState(childView)?.isInvalidated, "the child's view").toBe(true);
});

// A person's answer to a block ask brings the ask into a view as the tie of a block of the view
// it shaped, and `shaped` names blocks of the ask's own issue alone: so the answer marks a view of
// another issue that holds a block of the ask's issue, though the view holds no node of the ask
// yet. A plain ask's answer, which no `shaped` comes with, marks none of it.
test("an ancestor's block ask answered marks stale a descendant's view that holds a block of the ancestor, and a plain ask answered does not", () => {
  const answered = (payload: Record<string, unknown>) =>
    eventOf(
      "ask.answered",
      { anchor: null, id: "ask-1", question: "Which outline?", state: "answered", ...payload },
      "CORE-1"
    );
  const cases = [
    [
      "a block ask",
      answered({
        block_artifact: { id: "artifact-decisions", primary: false, slug: "decisions" },
        block_id: "q1",
      }),
      true,
    ],
    ["a plain ask", answered({}), false],
  ] as const;
  for (const [name, event, marks] of cases) {
    const client = new QueryClient();
    const childView = viewKey("CORE-2", "spec");
    client.setQueryData(
      childView,
      treeAnswer(
        [
          treeNode("CORE-2", "issue", null),
          treeNode("r1", "requirement", null, {
            in: "spec",
            issue: "CORE-1",
            read_only: true,
            role: "context",
          }),
        ],
        [],
        { issue: "CORE-2", view: "spec" }
      )
    );
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    expect(client.getQueryState(childView)?.isInvalidated, name).toBe(marks);
  }
});

// The tie a block ask's answer brings is carried by the views that read ties alone - the Spec
// and the Verification views (`readTies`) - so the answer marks such a view that holds a block of
// the ask's issue, the parent's Verification view holding the child's check here, and no Plan
// view, whatever block of the child it holds. The outline carries no tie: it is marked only where
// it holds a node the ask is on, and every view follows it then.
test("a child's block ask answered marks the parent's views that carry ties and hold a block of the child, and its outline and every view only where the outline holds what the ask is on", () => {
  const answered = blockAsk("ask.answered", "answered");
  const check = treeNode("k1", "check", "CORE-2", { ...CHILD, in: "verification" });
  const cases = [
    ["an outline holding no node the ask is on", []],
    [
      "an outline holding a node under the ask's block",
      [treeNode("c1", "criterion", ASK_BLOCK, { ...CHILD, in: "decisions" })],
    ],
  ] as const;
  const seen: Record<string, unknown> = {};
  for (const [name, underAsk] of cases) {
    const client = new QueryClient();
    const { marks } = parentBeforeTheCheck(
      client,
      treeAnswer([...(OUTLINE_WITH_CHILD.nodes ?? []), check, ...underAsk])
    );
    client.setQueryData(
      viewKey("CORE-1", "verification"),
      treeAnswer(
        [
          { ...PARENT_REQUIREMENT, role: "context" },
          { ...check, role: "context" },
        ],
        [],
        {
          view: "verification",
        }
      )
    );
    client.setQueryData(
      viewKey("CORE-1", "plan"),
      treeAnswer(
        [treeNode("t2", "plan_task", "CORE-2", { ...CHILD, in: "plan", role: "query" })],
        [],
        {
          view: "plan",
        }
      )
    );
    refreshQueries(client, eventRefreshTargets(answered, "alice"));
    seen[name] = marks();
  }
  expect(seen).toEqual({
    "an outline holding a node under the ask's block": EVERY_ANSWER,
    "an outline holding no node the ask is on": { ...NO_ANSWER, verification: true },
  });
});

/** The parent CORE-1's outline, which holds its child CORE-2's node, and its Verification view,
 *  which holds its requirement alone, the child's documents holding no check serving the
 *  requirement yet, so the view is judged by its outline; the view's query made first where
 *  `viewFirst` says so. */
function holdingChildNode(
  client: QueryClient,
  viewFirst = false
): { readonly outline: readonly unknown[]; readonly verification: readonly unknown[] } {
  const outline = issueTreeKey("CORE-1");
  const verification = viewKey("CORE-1", "verification");
  const reads = [
    [outline, OUTLINE_WITH_CHILD],
    [verification, VERIFICATION_WITH_REQUIREMENT],
  ] as const;
  for (const [queryKey, answer] of viewFirst ? [...reads].reverse() : reads) {
    client.setQueryData(queryKey, answer);
  }
  return { outline, verification };
}

/** A version of the child CORE-2's Verification document, which can add its first check serving
 *  the parent's requirement. */
const CHILD_VERSION = eventOf(
  "artifact.version",
  { artifact_id: "artifact-2", number: 2 },
  "CORE-2"
);

// A view holds only the nodes its view takes, so a child's version that adds the child's first
// check serving the parent's requirement moves the parent's Verification view, which holds no
// node of the child yet, as it moves the parent's outline, which holds the child's node: whatever
// marks an issue's outline stale marks every view of that issue the page holds.
test("a child's document version marks stale the parent's Verification view that holds no node of the child, where the parent's outline holds the child's node", () => {
  const client = new QueryClient();
  const { outline, verification } = holdingChildNode(client);
  refreshQueries(client, eventRefreshTargets(CHILD_VERSION, "alice"));
  expect({
    outline: client.getQueryState(outline)?.isInvalidated,
    verification: client.getQueryState(verification)?.isInvalidated,
  }).toEqual({ outline: true, verification: true });
});

// The scheduler judges each query of a refresh once, on its own answer: one that is idle at
// once, in the order the cache made them, and one with a fetch out on the response that lands.
// A view is judged by the answer its outline holds when the view is judged, so it is marked
// whether its query was made before the outline's, and when the view's own read, out as the
// event lands, answers, since the client is handed to its check then too. While the outline is
// out for a read the event lands during, the view is judged by its own answer alone, and is
// marked once the outline's answer lands holding the child's node.
test("a parent's view is marked with its outline where the view's query was made first, once the outline's read out answers, and where the view is", async () => {
  const made = new QueryClient();
  const viewFirst = holdingChildNode(made, true);
  refreshQueries(made, eventRefreshTargets(CHILD_VERSION, "alice"));
  expect(
    made.getQueryState(viewFirst.verification)?.isInvalidated,
    "the view whose query was made before the outline's"
  ).toBe(true);

  const reading = new QueryClient();
  const out = holdingChildNode(reading);
  const response = Promise.withResolvers<unknown>();
  const read = reading.fetchQuery({ queryFn: () => response.promise, queryKey: out.outline });
  expect(reading.getQueryState(out.outline)?.fetchStatus, "the outline's read").toBe("fetching");
  refreshQueries(reading, eventRefreshTargets(CHILD_VERSION, "alice"));
  expect(
    reading.getQueryState(out.verification)?.isInvalidated,
    "the view, while the outline is being read"
  ).toBe(false);
  response.resolve(reading.getQueryData(out.outline));
  await read;
  expect(
    reading.getQueryState(out.verification)?.isInvalidated,
    "the view, once the outline's read answers with the child's node"
  ).toBe(true);

  const viewing = new QueryClient();
  const { verification } = holdingChildNode(viewing);
  const viewResponse = Promise.withResolvers<unknown>();
  const viewRead = viewing.fetchQuery({
    queryFn: () => viewResponse.promise,
    queryKey: verification,
  });
  refreshQueries(viewing, eventRefreshTargets(CHILD_VERSION, "alice"));
  viewResponse.resolve(viewing.getQueryData(verification));
  await viewRead;
  expect(
    viewing.getQueryState(verification)?.isInvalidated,
    "the view, once the read it was out for as the event landed answers"
  ).toBe(true);
});

/** No answer of the parent CORE-1's marked: its outline, nor any of its views. */
const NO_ANSWER = { outline: false, plan: false, spec: false, verification: false };

/** How many listeners the refresh checks keep on `client`'s cache, past the scheduler's own, which
 *  starts here: each one counted from its subscribe to its unsubscribe. */
function checkListeners(client: QueryClient): () => number {
  trackQueryFetches(client);
  const cache = client.getQueryCache();
  const subscribe = cache.subscribe.bind(cache);
  let listening = 0;
  spyOn(cache, "subscribe").mockImplementation((listener) => {
    listening += 1;
    const stop = subscribe(listener);
    return () => {
      listening -= 1;
      stop();
    };
  });
  return () => listening;
}

// An outline whose first read is out holds no answer yet, so the page does not guess: each view
// of its issue is judged by its own answer alone, and the views it leaves unmarked wait for the
// outline's, with one listener on the cache for the outline whatever the number of its views.
// Once the outline answers holding the node the event moves, the views are marked with it, and
// the listener goes. An event that moves no node, such as a child's comment, marks none and waits
// for nothing.
test("a parent's views are judged by their own answers while the outline's first read is out, and marked once its answer lands holding what the event moves", async () => {
  const comment = eventOf(
    "comment.created",
    { body: "A reply.", id: "comment-2", kind: "comment" },
    "CORE-2"
  );
  const cases = [
    ["a child's comment", comment, 0, NO_ANSWER],
    ["a child's document version", CHILD_VERSION, 1, EVERY_ANSWER],
  ] as const;
  for (const [name, event, waiting, landed] of cases) {
    const client = new QueryClient();
    const listeners = checkListeners(client);
    const { marks } = parentBeforeTheCheck(client, undefined);
    const response = Promise.withResolvers<unknown>();
    const read = client.fetchQuery({
      queryFn: () => response.promise,
      queryKey: issueTreeKey("CORE-1"),
    });
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    expect(
      { listeners: listeners(), marks: marks() },
      `${name}, while the outline's first read is out`
    ).toEqual({ listeners: waiting, marks: NO_ANSWER });
    response.resolve(OUTLINE_WITH_CHILD);
    await read;
    expect(
      { listeners: listeners(), marks: marks() },
      `${name}, once the outline's answer lands`
    ).toEqual({ listeners: 0, marks: landed });
  }
});

// A read of the outline that is refused leaves the answer the outline held, and the views that
// wait for it are judged by that answer, as the scheduler judges the outline itself: a refused
// refetch leaves the answer that holds the child's node, and the views are marked; a refused first
// read leaves no answer, and none is. An outline removed while its read is out marks no view. The
// listener goes with each. (A refused read marks the outline stale itself, whatever it held.)
test("a parent's views waiting on the outline's read are judged by the answer a refused read leaves, and by none once the outline is removed", async () => {
  const seen: Record<string, unknown> = {};
  for (const ending of ["a refetch refused", "a first read refused", "the outline removed"]) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const listeners = checkListeners(client);
    const { marks } = parentBeforeTheCheck(
      client,
      ending === "a refetch refused" ? OUTLINE_WITH_CHILD : undefined
    );
    const response = Promise.withResolvers<unknown>();
    const read = client
      .fetchQuery({
        queryFn: () => response.promise,
        queryKey: issueTreeKey("CORE-1"),
        staleTime: 0,
      })
      .catch(() => undefined);
    refreshQueries(client, eventRefreshTargets(CHILD_VERSION, "alice"));
    const waiting = listeners();
    if (ending === "the outline removed") {
      client.removeQueries({ exact: true, queryKey: issueTreeKey("CORE-1") });
      response.resolve(OUTLINE_WITH_CHILD);
    } else {
      response.reject(new Error("refused"));
    }
    await read;
    const { plan, spec, verification } = marks();
    seen[ending] = { listeners: [waiting, listeners()], views: { plan, spec, verification } };
  }
  const none = { plan: false, spec: false, verification: false };
  expect(seen).toEqual({
    "a first read refused": { listeners: [1, 0], views: none },
    "a refetch refused": {
      listeners: [1, 0],
      views: { plan: true, spec: true, verification: true },
    },
    "the outline removed": { listeners: [1, 0], views: none },
  });
});

/** The parent CORE-1's outline and Verification view (`holdingChildNode`), each observed as the
 *  page observes the open filter's, the outline's query made first unless `viewFirst` says
 *  otherwise: each read of the outline answers `outlineRead`, and each read of the view is
 *  counted. */
function observedParent(
  client: QueryClient,
  outlineRead: Promise<unknown>,
  viewFirst = false
): { readonly stop: () => void; readonly viewReads: () => number } {
  trackQueryFetches(client);
  const { outline, verification } = holdingChildNode(client, viewFirst);
  let viewReads = 0;
  const observers = [
    new QueryObserver(client, {
      queryFn: () => outlineRead,
      queryKey: outline,
      staleTime: Number.POSITIVE_INFINITY,
    }),
    new QueryObserver(client, {
      queryFn: async () => {
        viewReads += 1;
        return VERIFICATION_WITH_REQUIREMENT;
      },
      queryKey: verification,
      staleTime: Number.POSITIVE_INFINITY,
    }),
  ];
  const stops = observers.map((observer) => observer.subscribe(() => undefined));
  return {
    stop: () => {
      for (const stop of stops) stop();
      client.clear();
    },
    viewReads: () => viewReads,
  };
}

// On the page the outline and the open view are both observed. The event's refresh judges the
// outline's answer, which holds the child's node, and the view by that answer, before it starts
// any read: it then starts the outline's read, which answers nothing the view waits for, so the
// view is read at once, beside the outline, whichever query was made first.
test("a parent's observed view is read at once where the event's own refresh starts its outline's read, whichever query was made first", async () => {
  const seen: Record<string, unknown> = {};
  for (const viewFirst of [false, true]) {
    const client = new QueryClient();
    const response = Promise.withResolvers<unknown>();
    const parent = observedParent(client, response.promise, viewFirst);
    refreshQueries(client, eventRefreshTargets(CHILD_VERSION, "alice"));
    await settle();
    seen[viewFirst ? "view first" : "outline first"] = {
      outline: client.getQueryState(issueTreeKey("CORE-1"))?.fetchStatus,
      viewReads: parent.viewReads(),
    };
    response.resolve(OUTLINE_WITH_CHILD);
    await settle();
    parent.stop();
  }
  const atOnce = { outline: "fetching", viewReads: 1 };
  expect(seen, "before the outline's read answers").toEqual({
    "outline first": atOnce,
    "view first": atOnce,
  });
});

// Two events of one batch each wait on the outline's read that was out before them, and each
// marks the open view once that read answers: the view is read once, by the first, and the
// second finds it read and reads it no more.
test("a parent's observed view that two events of one batch wait on is read once the outline's read answers, and once only", async () => {
  jest.useFakeTimers();
  const client = new QueryClient();
  const response = Promise.withResolvers<unknown>();
  const parent = observedParent(client, response.promise);
  try {
    const read = client.fetchQuery({
      queryFn: () => response.promise,
      queryKey: issueTreeKey("CORE-1"),
      staleTime: 0,
    });
    const versions = ["artifact-2", "artifact-3"].map((artifact) =>
      eventOf("artifact.version", { artifact_id: artifact, number: 2 }, "CORE-2")
    );
    refreshQueries(
      client,
      versions.flatMap((version) => eventRefreshTargets(version, "alice"))
    );
    await settle();
    const waiting = parent.viewReads();
    response.resolve(OUTLINE_WITH_CHILD);
    await read;
    await tick(3_000, 100);
    expect({ answered: parent.viewReads(), waiting }).toEqual({ answered: 1, waiting: 0 });
  } finally {
    parent.stop();
    realClock();
  }
});

/** The grandparent CORE-1's outline, which holds its child CORE-2's node and no node of the
 *  grandchild CORE-3, and its Verification view, which holds its requirement and no node of
 *  either; and what else the page holds that places CORE-3 under CORE-2: CORE-3's detail, CORE-2's
 *  outline holding CORE-3's node, or nothing. */
function grandparentHolding(
  client: QueryClient,
  placing: "detail" | "parent's outline" | "nothing"
): { readonly outline: readonly unknown[]; readonly verification: readonly unknown[] } {
  const outline = issueTreeKey("CORE-1");
  client.setQueryData(outline, OUTLINE_WITH_CHILD);
  const verification = viewKey("CORE-1", "verification");
  client.setQueryData(verification, VERIFICATION_WITH_REQUIREMENT);
  if (placing === "detail") {
    client.setQueryData(["issue", "CORE-3"], { key: "CORE-3", parent: "CORE-2" });
  }
  if (placing === "parent's outline") {
    client.setQueryData(
      issueTreeKey("CORE-2"),
      treeAnswer(
        [
          treeNode("CORE-2", "issue", null),
          treeNode("CORE-3", "issue", "CORE-2", { issue: "CORE-3", read_only: true }),
        ],
        [],
        { issue: "CORE-2" }
      )
    );
  }
  return { outline, verification };
}

/** A version of the grandchild CORE-3's Verification document, which can add its first check
 *  serving the grandparent's requirement. */
const GRANDCHILD_VERSION = eventOf(
  "artifact.version",
  { artifact_id: "artifact-3", number: 2 },
  "CORE-3"
);

// A document event names no ancestors, so the page places the grandchild from what it holds: its
// detail names its parent, whose node the grandparent's outline holds, and the grandparent's
// outline is read again, with the views of it the page holds.
test("a grandchild's document version marks its grandparent's outline and view where the grandchild's detail names its parent", () => {
  const client = new QueryClient();
  const { outline, verification } = grandparentHolding(client, "detail");
  refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
  expect({
    outline: client.getQueryState(outline)?.isInvalidated,
    verification: client.getQueryState(verification)?.isInvalidated,
  }).toEqual({ outline: true, verification: true });
});

// The feed refreshes every event of one window in one refresh, so an earlier event of the
// grandchild - a comment, a claim - names its detail before its version's checks run. No mark of
// a refresh decides a check of the same refresh: the grandchild's fresh detail places it under
// its parent all the same, and the grandparent's outline and view are read again.
test("a grandchild's comment or claim and then its document version, in one refresh, mark its grandparent's outline and view where the grandchild's fresh detail names its parent", () => {
  const comment = eventOf(
    "comment.created",
    { body: "A reply.", id: "comment-3", kind: "comment" },
    "CORE-3"
  );
  const claim = eventOf("issue.claimed", {}, "CORE-3");
  for (const [name, first] of [
    ["a comment", comment],
    ["a claim", claim],
  ] as const) {
    const client = new QueryClient();
    const { outline, verification } = grandparentHolding(client, "detail");
    refreshQueries(
      client,
      [first, GRANDCHILD_VERSION].flatMap((event) => eventRefreshTargets(event, "alice"))
    );
    expect(
      {
        outline: client.getQueryState(outline)?.isInvalidated,
        verification: client.getQueryState(verification)?.isInvalidated,
      },
      `${name}, then the version`
    ).toEqual({ outline: true, verification: true });
  }
});

// A detail marked stale - its issue may have moved since it was read - places nothing until it
// is read again, and the page places the issue from the trees it holds alone: no tree holds a
// node of the grandchild, so its document version reads nothing of the grandparent's, though
// its detail names the parent whose node the grandparent's outline holds.
test("a grandchild's document version marks nothing of its grandparent's where the grandchild's detail that names its parent is marked stale", () => {
  const client = new QueryClient();
  const { outline, verification } = grandparentHolding(client, "detail");
  void client.invalidateQueries({ exact: true, queryKey: ["issue", "CORE-3"] });
  refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
  expect({
    outline: client.getQueryState(outline)?.isInvalidated,
    verification: client.getQueryState(verification)?.isInvalidated,
  }).toEqual({ outline: false, verification: false });
});

// The parent's held outline places the grandchild too: it holds the grandchild's node under the
// parent's.
test("a grandchild's document version marks its grandparent's outline and view where its parent's held outline holds its node", () => {
  const client = new QueryClient();
  const { outline, verification } = grandparentHolding(client, "parent's outline");
  refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
  expect({
    outline: client.getQueryState(outline)?.isInvalidated,
    parent: client.getQueryState(issueTreeKey("CORE-2"))?.isInvalidated,
    verification: client.getQueryState(verification)?.isInvalidated,
  }).toEqual({ outline: true, parent: true, verification: true });
});

// Where nothing the page holds places the grandchild, its document version reads nothing of the
// grandparent's; and a grandchild's comment moves no node, so it reads nothing of the
// grandparent's wherever the page places it.
test("a grandchild's document version marks nothing of its grandparent's that nothing places it under, and its comment marks nothing of the grandparent's", () => {
  const comment = eventOf(
    "comment.created",
    { body: "A reply.", id: "comment-3", kind: "comment" },
    "CORE-3"
  );
  const cases = [
    ["a version, nothing placing the grandchild", "nothing", GRANDCHILD_VERSION],
    ["a comment, the grandchild's detail placing it", "detail", comment],
    ["a comment, its parent's outline placing it", "parent's outline", comment],
  ] as const;
  for (const [name, placing, event] of cases) {
    const client = new QueryClient();
    const { outline, verification } = grandparentHolding(client, placing);
    refreshQueries(client, eventRefreshTargets(event, "alice"));
    expect(
      {
        outline: client.getQueryState(outline)?.isInvalidated,
        verification: client.getQueryState(verification)?.isInvalidated,
      },
      name
    ).toEqual({ outline: false, verification: false });
  }
});

// A tree places a child under the node of its parent its `part_of` names, and that node is of the
// issue it names: a held view of another issue - the great-grandparent's Plan view, which holds
// every issue under it - that holds the grandchild's node under a plan task of its parent places
// the grandchild under that parent, not under the view's own issue, and the grandparent's outline
// holds the parent's node.
test("a grandchild's document version marks its grandparent's outline and view where a held view of another issue holds its node under a block that names its parent", () => {
  const client = new QueryClient();
  const { outline, verification } = grandparentHolding(client, "nothing");
  const parentsTask: IssueTreeParent = {
    first_line: "Draw the grandchild.",
    id: "CORE-2/plan#t2",
    issue: "CORE-2",
    kind: "block",
    supertag: "plan_task",
  };
  client.setQueryData(
    viewKey("CORE-0", "plan"),
    treeAnswer(
      [
        treeNode("CORE-3", "issue", parentsTask, {
          issue: "CORE-3",
          read_only: true,
          role: "query",
        }),
      ],
      [],
      { issue: "CORE-0", view: "plan" }
    )
  );
  refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
  expect({
    outline: client.getQueryState(outline)?.isInvalidated,
    verification: client.getQueryState(verification)?.isInvalidated,
  }).toEqual({ outline: true, verification: true });
});

// A check handed no client - one called apart from a refresh - reads no cache to place the
// event's issue, and keeps nothing of that for the checks of the same event a refresh hands its
// client: they place the grandchild under its parent from the grandchild's held detail.
test("a check called first with no client keeps no placement for the checks of its event that are handed one", () => {
  const client = new QueryClient();
  const { outline, verification } = grandparentHolding(client, "detail");
  const targets = eventRefreshTargets(GRANDCHILD_VERSION, "alice");
  const check = targets.find(
    (target) =>
      target.queryKey.length === 1 &&
      target.queryKey[0] === "issue-tree" &&
      target.when !== undefined
  )?.when;
  const query = client.getQueryCache().find({ exact: true, queryKey: outline });
  expect(
    query === undefined ? undefined : check?.(query.state.data, query),
    "the grandparent's outline, judged with no client"
  ).toBe(false);
  refreshQueries(client, targets);
  expect({
    outline: client.getQueryState(outline)?.isInvalidated,
    verification: client.getQueryState(verification)?.isInvalidated,
  }).toEqual({ outline: true, verification: true });
});

/** The parent CORE-1's requirement, which a check of its child serves once the child writes one. */
const PARENT_REQUIREMENT = treeNode("r1", "requirement", "CORE-1", {
  in: "spec",
  text: "Every child is drawn.",
});

/** The parent CORE-1's answers before its child CORE-2 holds a check serving its requirement: its
 *  outline with the child's node, as given, and its Spec, Verification and Plan views, which hold
 *  no node of the child. */
function parentBeforeTheCheck(client: QueryClient, outline: TreeAnswer | undefined) {
  const requirement = PARENT_REQUIREMENT;
  if (outline !== undefined) client.setQueryData(issueTreeKey("CORE-1"), outline);
  const views = {
    plan: treeAnswer([treeNode("t1", "plan_task", "CORE-1", { in: "plan", role: "query" })], [], {
      view: "plan",
    }),
    spec: treeAnswer([{ ...requirement, role: "query" }], [], { view: "spec" }),
    verification: treeAnswer([{ ...requirement, role: "context" }], [], { view: "verification" }),
  };
  for (const [view, answer] of Object.entries(views)) {
    client.setQueryData(viewKey("CORE-1", view as keyof typeof views), answer);
  }
  const marks = () => ({
    outline: client.getQueryState(issueTreeKey("CORE-1"))?.isInvalidated === true,
    plan: client.getQueryState(viewKey("CORE-1", "plan"))?.isInvalidated === true,
    spec: client.getQueryState(viewKey("CORE-1", "spec"))?.isInvalidated === true,
    verification: client.getQueryState(viewKey("CORE-1", "verification"))?.isInvalidated === true,
  });
  return { marks };
}

const EVERY_ANSWER = { outline: true, plan: true, spec: true, verification: true };

// The outline's refetch is out - the child was just moved under the parent - when the child's
// document version lands. The outline is judged on its response, which holds the child's node,
// and is read again; its views must be marked with it, whatever the outline held before the
// refetch, or the Verification view leaves out the child's new check while its filter is on.
test("a view is marked with its outline where the outline's refetch is out and its response holds what the event moves", async () => {
  const client = new QueryClient();
  const requirement = PARENT_REQUIREMENT;
  const { marks } = parentBeforeTheCheck(
    client,
    treeAnswer([treeNode("CORE-1", "issue", null), requirement])
  );
  const response = Promise.withResolvers<unknown>();
  const read = client.fetchQuery({
    queryFn: () => response.promise,
    queryKey: issueTreeKey("CORE-1"),
    staleTime: 0,
  });
  const version = eventOf("artifact.version", { artifact_id: "artifact-2", number: 2 }, "CORE-2");
  refreshQueries(client, eventRefreshTargets(version, "alice"));
  response.resolve(
    treeAnswer([
      treeNode("CORE-1", "issue", null),
      requirement,
      treeNode("CORE-2", "issue", "CORE-1", { ...CHILD, text: "Draw each child" }),
    ])
  );
  await read;
  expect(marks(), "the parent's outline and every view, after the outline's response").toEqual(
    EVERY_ANSWER
  );
});

// A child stands where its part_of names a node of its parent - a requirement here - so the
// parent's held outline holds the grandchild's node under the parent's block, not under the
// parent's issue node. The page places the grandchild under that tree's issue all the same, so
// the grandchild's document version reaches the grandparent's outline and views.
test("a grandchild's document version marks its grandparent's outline and views where its parent's held outline holds its node under the parent's block", () => {
  const client = new QueryClient();
  const { marks } = parentBeforeTheCheck(
    client,
    treeAnswer([
      treeNode("CORE-1", "issue", null),
      treeNode("CORE-2", "issue", "CORE-1", { ...CHILD, text: "Draw each child" }),
    ])
  );
  const statement: IssueTreeParent = {
    first_line: "Each grandchild is drawn.",
    id: "CORE-2/spec#cr1",
    kind: "block",
    supertag: "requirement",
  };
  client.setQueryData(
    issueTreeKey("CORE-2"),
    treeAnswer([
      treeNode("CORE-2", "issue", null),
      treeNode("cr1", "requirement", "CORE-2", { in: "spec" }),
      treeNode("CORE-3", "issue", statement, {
        issue: "CORE-3",
        read_only: true,
        text: "Draw each grandchild",
      }),
    ])
  );
  const version = eventOf("artifact.version", { artifact_id: "artifact-3", number: 2 }, "CORE-3");
  refreshQueries(client, eventRefreshTargets(version, "alice"));
  expect(marks(), "the grandparent's outline and every view").toEqual(EVERY_ANSWER);
});

// The parent's held outline still holds the child's node - the child was moved away - when its
// refetch goes out and the child's document version lands. The page places the child under the
// parent from that answer, and the parent's outline holds its own node, as every outline does.
// The refetch answers with no node of the child: nothing the event moves is in the parent's
// answer, so neither the outline nor any view of it is read again for the event.
test("an outline whose refetch answers no node of the issue it was placed above is not read again, nor any view of it", async () => {
  const client = new QueryClient();
  const { marks } = parentBeforeTheCheck(
    client,
    treeAnswer([
      treeNode("CORE-1", "issue", null),
      PARENT_REQUIREMENT,
      treeNode("CORE-2", "issue", "CORE-1", { ...CHILD, text: "Draw each child" }),
    ])
  );
  const response = Promise.withResolvers<unknown>();
  const read = client.fetchQuery({
    queryFn: () => response.promise,
    queryKey: issueTreeKey("CORE-1"),
    staleTime: 0,
  });
  const version = eventOf("artifact.version", { artifact_id: "artifact-2", number: 2 }, "CORE-2");
  refreshQueries(client, eventRefreshTargets(version, "alice"));
  response.resolve(treeAnswer([treeNode("CORE-1", "issue", null), PARENT_REQUIREMENT]));
  await read;
  await Promise.resolve();
  expect(
    marks(),
    "the parent's outline and every view, after a response with no node of the child"
  ).toEqual({
    outline: false,
    plan: false,
    spec: false,
    verification: false,
  });
});
