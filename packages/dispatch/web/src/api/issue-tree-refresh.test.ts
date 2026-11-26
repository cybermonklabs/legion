import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { realClock, settle, tick } from "../__tests__/fake-clock";
import {
  type TreeNodeExtra,
  treeAnswer,
  treeNode,
} from "../features/issue-page/tree-read/tree-fixtures";
import { type TreeAnswer, viewKey } from "../features/issue-page/tree-read/tree-read";
import { dedupeRefreshTargets } from "./event-feed";
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

/** The grandchild CORE-3's ancestors, parent first up to the root, as the server names them on
 *  each event of a document of the grandchild: its parent CORE-2, then the root CORE-1. */
const GRANDCHILD_ANCESTORS: readonly string[] = ["CORE-2", "CORE-1"];

/** An event of `type` of the grandchild CORE-3's Verification document, naming `ancestors` where
 *  the case gives them, as the server writes it: a version names its document by id, and an
 *  upload and a removal carry the document itself. */
function grandchildDocumentEvent(type: string, ancestors?: readonly string[]): DispatchEvent {
  return eventOf(
    type,
    {
      artifact: { id: "artifact-3", name: "verification.md", slug: "verification-md" },
      artifact_id: "artifact-3",
      number: 2,
      ...(ancestors === undefined ? {} : { ancestors }),
    },
    "CORE-3"
  );
}

/** The coverage read of issue `key`, which counts the checks of every issue under it as well as
 *  its own. */
function coverageKey(key: string): readonly unknown[] {
  return ["issue", key, "coverage"];
}

/** A coverage read's answer, which no case reads: only whether the read is marked counts. */
const COVERAGE = { gaps: [], has_plan: false, requirements: [] };

/** Whether each read `keys` names is marked stale, by its name in `keys`. */
function marked(
  client: QueryClient,
  keys: Readonly<Record<string, readonly unknown[]>>
): Record<string, boolean> {
  return Object.fromEntries(
    Object.entries(keys).map(([name, queryKey]) => [
      name,
      client.getQueryState(queryKey)?.isInvalidated === true,
    ])
  );
}

/** An observer of `queryKey`, as the page observes its reads: it holds `answer` fresh until an
 *  event marks it, reads `answer` again whenever the scheduler sends a read, and is enabled
 *  unless `enabled` says otherwise. Answers its stop. */
function observe(
  client: QueryClient,
  queryKey: readonly unknown[],
  answer: unknown,
  enabled = true
): () => void {
  return new QueryObserver(client, {
    enabled,
    queryFn: async () => answer,
    queryKey,
    staleTime: Number.POSITIVE_INFINITY,
  }).subscribe(() => undefined);
}

/** The root CORE-1's reads as its new page holds them open on Verification: its outline,
 *  observed and idle, holding its child CORE-2's node and no node of the grandchild CORE-3; its
 *  Verification view, holding its requirement alone; and its coverage read. */
function rootOnVerification(client: QueryClient): {
  readonly marks: () => Record<string, boolean>;
  readonly stop: () => void;
} {
  const reads = {
    coverage: coverageKey("CORE-1"),
    outline: issueTreeKey("CORE-1"),
    verification: viewKey("CORE-1", "verification"),
  };
  client.setQueryData(reads.outline, OUTLINE_WITH_CHILD);
  client.setQueryData(reads.verification, VERIFICATION_WITH_REQUIREMENT);
  client.setQueryData(reads.coverage, COVERAGE);
  const stop = observe(client, reads.outline, OUTLINE_WITH_CHILD);
  return {
    marks: () => marked(client, reads),
    stop: () => {
      stop();
      client.clear();
    },
  };
}

/** The root's three reads of `rootOnVerification`, each marked. */
const ROOT_MARKED = { coverage: true, outline: true, verification: true };

/** None of the root's three reads of `rootOnVerification` marked. */
const ROOT_UNMARKED = { coverage: false, outline: false, verification: false };

// ST-1, S1. A document event of an issue names the issue's ancestors, so the root's open page
// reaches the root's outline and every view of it the page holds by the event alone - though the
// outline holds no node of the grandchild yet, and nothing the page holds places the grandchild -
// and the root's coverage, which counts the grandchild's checks, as a version of the root's own
// documents marks the root's: a grandchild's first check that serves the root's requirement
// moves the counts the Verification view draws.
test("a grandchild's document version that names its ancestors marks the root's outline, its Verification view and its coverage", () => {
  const client = new QueryClient();
  const root = rootOnVerification(client);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.version", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(root.marks()).toEqual(ROOT_MARKED);
  } finally {
    root.stop();
  }
});

// ST-1, S2. An upload of a document of the grandchild can bring its first check in as a version
// can, and marks the issue's own coverage as a version does, so it reaches the root alike.
test("a grandchild's document upload that names its ancestors marks the root's outline, its Verification view and its coverage", () => {
  const client = new QueryClient();
  const root = rootOnVerification(client);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.created", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(root.marks()).toEqual(ROOT_MARKED);
  } finally {
    root.stop();
  }
});

// ST-1, S3. A removal takes the document's nodes out of every tree that holds them, so it reaches
// the root's outline and view; it writes no version, and marks the coverage of no issue, its own
// included, so it leaves the root's coverage alone.
test("a grandchild's document removal that names its ancestors marks the root's outline and its Verification view, and leaves the root's coverage unmarked", () => {
  const client = new QueryClient();
  const root = rootOnVerification(client);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.removed", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(root.marks()).toEqual({ coverage: false, outline: true, verification: true });
  } finally {
    root.stop();
  }
});

// ST-1, S4. Today's issue page reads the same coverage keys and holds no tree read, so a
// grandchild's document event marks no ancestor's coverage there, nor any ancestor's detail, and
// that page refreshes as before.
test("a grandchild's document version that names its ancestors marks no ancestor's coverage or detail where the cache holds no tree read, as today's page holds none", () => {
  const client = new QueryClient();
  const reads = {
    childCoverage: coverageKey("CORE-2"),
    childDetail: ["issue", "CORE-2"],
    rootCoverage: coverageKey("CORE-1"),
    rootDetail: ["issue", "CORE-1"],
  };
  client.setQueryData(reads.rootDetail, { key: "CORE-1", parent: null });
  client.setQueryData(reads.childDetail, { key: "CORE-2", parent: "CORE-1" });
  client.setQueryData(reads.rootCoverage, COVERAGE);
  client.setQueryData(reads.childCoverage, COVERAGE);
  refreshQueries(
    client,
    eventRefreshTargets(grandchildDocumentEvent("artifact.version", GRANDCHILD_ANCESTORS), "alice")
  );
  expect(marked(client, reads)).toEqual({
    childCoverage: false,
    childDetail: false,
    rootCoverage: false,
    rootDetail: false,
  });
});

// ST-1, S5: decision 411 (2). The grandchild was moved under a new parent, CORE-4: its detail,
// marked stale since, names its old parent CORE-2, and the new parent's outline, read before the
// move, holds no node of it. The event's ancestors name the new parent, so the new parent's
// outline and coverage are marked, and the old parent's outline, which holds no node of the
// grandchild either, is not.
test("a grandchild moved under a new parent reaches that parent's outline, read before the move, and its coverage through its event's ancestors, though its stale detail names its old parent", () => {
  const client = new QueryClient();
  client.setQueryData(["issue", "CORE-3"], { key: "CORE-3", parent: "CORE-2" });
  void client.invalidateQueries({ exact: true, queryKey: ["issue", "CORE-3"] });
  const newParentAnswer = treeAnswer([treeNode("CORE-4", "issue", null)], [], {
    issue: "CORE-4",
  });
  const reads = {
    newParentCoverage: coverageKey("CORE-4"),
    newParentOutline: issueTreeKey("CORE-4"),
    oldParentOutline: issueTreeKey("CORE-2"),
  };
  client.setQueryData(reads.newParentOutline, newParentAnswer);
  client.setQueryData(reads.newParentCoverage, COVERAGE);
  client.setQueryData(
    reads.oldParentOutline,
    treeAnswer([treeNode("CORE-2", "issue", null)], [], { issue: "CORE-2" })
  );
  const stop = observe(client, reads.newParentOutline, newParentAnswer);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.version", ["CORE-4", "CORE-1"]),
        "alice"
      )
    );
    expect(marked(client, reads)).toEqual({
      newParentCoverage: true,
      newParentOutline: true,
      oldParentOutline: false,
    });
  } finally {
    stop();
    client.clear();
  }
});

// ST-1, S6. A document event from a Dispatch older than the field names no ancestors, and the
// page places the grandchild from what it holds (`issuesAbove`): its detail names its parent,
// whose node the root's outline holds. The root's outline and view are marked, as before, and the
// root's coverage with them, by that one placement.
test("a grandchild's document version that names no ancestors marks the root's outline, view and coverage where what the page holds places the grandchild under the root", () => {
  const client = new QueryClient();
  const reads = { coverage: coverageKey("CORE-1"), ...grandparentHolding(client, "detail") };
  client.setQueryData(reads.coverage, COVERAGE);
  const stops = [
    observe(client, reads.outline, OUTLINE_WITH_CHILD),
    observe(client, reads.verification, VERIFICATION_WITH_REQUIREMENT),
  ];
  try {
    refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
    expect(marked(client, reads)).toEqual(ROOT_MARKED);
  } finally {
    for (const stop of stops) stop();
    client.clear();
  }
});

// ST-1, S7: decision 410. A person's edit notice comes after the versions that carried each of
// the edits it tells of, and moves no node: it reaches no ancestor, whatever its payload names.
test("a grandchild's edit notice that carries its ancestors marks no read of the root's", () => {
  const client = new QueryClient();
  const root = rootOnVerification(client);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.human_edited", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(root.marks()).toEqual(ROOT_UNMARKED);
  } finally {
    root.stop();
  }
});

// ST-1, S8. A root's document event names no ancestor (`[]`): it marks the root's own outline and
// coverage as before, no other issue's tree or coverage, and the page never asks the cache where
// the root stands. `issuesAbove` starts its walk by asking the cache for the issue's detail by
// that read's hash, its key as JSON, which this cache does not hold; it runs for an event that
// names no ancestors alone.
test("a root's document version, whose ancestors are none, marks its own outline and coverage, no other issue's, and walks no cache to place the root", () => {
  const client = new QueryClient();
  const reads = {
    coverage: coverageKey("CORE-1"),
    otherCoverage: coverageKey("CORE-9"),
    otherOutline: issueTreeKey("CORE-9"),
    outline: issueTreeKey("CORE-1"),
  };
  client.setQueryData(reads.outline, treeAnswer([treeNode("CORE-1", "issue", null), REQUIREMENT]));
  client.setQueryData(reads.coverage, COVERAGE);
  client.setQueryData(
    reads.otherOutline,
    treeAnswer([treeNode("CORE-9", "issue", null)], [], { issue: "CORE-9" })
  );
  client.setQueryData(reads.otherCoverage, COVERAGE);
  const cache = client.getQueryCache();
  const get = cache.get.bind(cache);
  const rootDetail = JSON.stringify(["issue", "CORE-1"]);
  let askedWhereTheRootStands = 0;
  spyOn(cache, "get").mockImplementation(((hash: string) => {
    if (hash === rootDetail) askedWhereTheRootStands += 1;
    return get(hash);
  }) as typeof cache.get);
  const version = eventOf(
    "artifact.version",
    { ancestors: [], artifact_id: "artifact-1", number: 2 },
    "CORE-1"
  );
  refreshQueries(client, eventRefreshTargets(version, "alice"));
  expect({ asked: askedWhereTheRootStands, marks: marked(client, reads) }).toEqual({
    asked: 0,
    marks: { coverage: true, otherCoverage: false, otherOutline: false, outline: true },
  });
});

// ST-1, S9. The page can hold a view of the root with no outline beside it, and the event names
// the root, so the view is marked by it; a view is a read under the root's tree read, so the
// root's coverage is marked with it.
test("a grandchild's document version that names its ancestors marks the root's Verification view and coverage where the page holds that view and no outline", () => {
  const client = new QueryClient();
  const reads = {
    coverage: coverageKey("CORE-1"),
    verification: viewKey("CORE-1", "verification"),
  };
  client.setQueryData(reads.verification, VERIFICATION_WITH_REQUIREMENT);
  client.setQueryData(reads.coverage, COVERAGE);
  const stop = observe(client, reads.verification, VERIFICATION_WITH_REQUIREMENT);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.version", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(marked(client, reads)).toEqual({ coverage: true, verification: true });
  } finally {
    stop();
    client.clear();
  }
});

// ST-1, S10: against decision 408 (1). The root's outline has its first read out, so it holds no
// answer a check could judge, and nothing the page holds places the grandchild. The event names
// the root, so the outline is marked all the same and owes a read once that first one lands -
// its answer then stays marked stale - and the root's idle view and its coverage are marked.
test("a grandchild's document version that names its ancestors marks the root's outline whose first read is out, which then owes a read, and the root's idle view and coverage", async () => {
  jest.useFakeTimers();
  const client = new QueryClient();
  const reads = {
    coverage: coverageKey("CORE-1"),
    outline: issueTreeKey("CORE-1"),
    verification: viewKey("CORE-1", "verification"),
  };
  try {
    client.setQueryData(reads.verification, VERIFICATION_WITH_REQUIREMENT);
    client.setQueryData(reads.coverage, COVERAGE);
    const response = Promise.withResolvers<unknown>();
    const read = client.fetchQuery({ queryFn: () => response.promise, queryKey: reads.outline });
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.version", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    const atOnce = marked(client, reads);
    response.resolve(OUTLINE_WITH_CHILD);
    await read;
    // The scheduler takes up what a query owes on a zero-delay timer once its read settles.
    await tick(0);
    expect({ atOnce, landed: marked(client, { outline: reads.outline }) }).toEqual({
      atOnce: ROOT_MARKED,
      landed: { outline: true },
    });
  } finally {
    client.clear();
    realClock();
  }
});

// ST-1, S11. The reader moved from the root's new page to the child's: the root's outline and
// view stay in the cache with nothing observing them, and the child's outline, which holds the
// grandchild's node, is observed. The root's reads are marked all the same, its coverage among
// them, so the root's page draws the new counts when the reader comes back to it.
test("a grandchild's document version that names its ancestors marks the root's outline, view and coverage that nothing observes once the reader has moved to the child's new page", () => {
  const client = new QueryClient();
  const reads = {
    coverage: coverageKey("CORE-1"),
    outline: issueTreeKey("CORE-1"),
    verification: viewKey("CORE-1", "verification"),
  };
  client.setQueryData(reads.outline, OUTLINE_WITH_CHILD);
  client.setQueryData(reads.verification, VERIFICATION_WITH_REQUIREMENT);
  client.setQueryData(reads.coverage, COVERAGE);
  const childAnswer = treeAnswer(
    [
      treeNode("CORE-2", "issue", null),
      treeNode("CORE-3", "issue", "CORE-2", { issue: "CORE-3", read_only: true }),
    ],
    [],
    { issue: "CORE-2" }
  );
  client.setQueryData(issueTreeKey("CORE-2"), childAnswer);
  const stop = observe(client, issueTreeKey("CORE-2"), childAnswer);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.version", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(marked(client, reads)).toEqual(ROOT_MARKED);
  } finally {
    stop();
    client.clear();
  }
});

// ST-1, S12. A project's document belongs to no issue: its event names no ancestors, and it
// reaches no issue's tree or coverage.
test("a project document's version marks no read of the root's", () => {
  const client = new QueryClient();
  const root = rootOnVerification(client);
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        eventOf("artifact.version", { artifact_id: "artifact-9", number: 2 }, null),
        "alice"
      )
    );
    expect(root.marks()).toEqual(ROOT_UNMARKED);
  } finally {
    root.stop();
  }
});

// ST-1, S13. While a write of the root is out, its page holds the root's outline on hold - its
// observer is not enabled (`mayReadNow`) - and reads the Verification view through an observer
// that is never enabled, as `treeRead` makes one. The event names the root, so both are marked,
// and its coverage too: the cache holds the root's tree read, whatever observes it, and the page
// reads all three once the write settles.
test("a grandchild's document version that names its ancestors marks the root's outline and view that a write of the root holds from reading, and its coverage", () => {
  const client = new QueryClient();
  const reads = {
    coverage: coverageKey("CORE-1"),
    outline: issueTreeKey("CORE-1"),
    verification: viewKey("CORE-1", "verification"),
  };
  client.setQueryData(reads.outline, OUTLINE_WITH_CHILD);
  client.setQueryData(reads.verification, VERIFICATION_WITH_REQUIREMENT);
  client.setQueryData(reads.coverage, COVERAGE);
  const stops = [
    observe(client, reads.outline, OUTLINE_WITH_CHILD, false),
    observe(client, reads.verification, VERIFICATION_WITH_REQUIREMENT, false),
  ];
  try {
    refreshQueries(
      client,
      eventRefreshTargets(
        grandchildDocumentEvent("artifact.version", GRANDCHILD_ANCESTORS),
        "alice"
      )
    );
    expect(marked(client, reads)).toEqual(ROOT_MARKED);
  } finally {
    for (const stop of stops) stop();
    client.clear();
  }
});

// ST-1, S14. A document event that names no ancestors reaches the coverage of an issue the page
// places above the event's issue only where the cache holds that issue's tree read, as an event
// that names them does: the details place the root above the grandchild here, and the root's
// coverage is marked once the root's outline is held, and not before.
test("a grandchild's document version that names no ancestors marks the coverage of an issue the page places above it only where the cache holds that issue's tree read", () => {
  const seen: Record<string, boolean> = {};
  for (const [name, outlineHeld] of [
    ["no tree read of the root", false],
    ["the root's outline", true],
  ] as const) {
    const client = new QueryClient();
    client.setQueryData(["issue", "CORE-3"], { key: "CORE-3", parent: "CORE-2" });
    client.setQueryData(["issue", "CORE-2"], { key: "CORE-2", parent: "CORE-1" });
    client.setQueryData(coverageKey("CORE-1"), COVERAGE);
    if (outlineHeld) client.setQueryData(issueTreeKey("CORE-1"), OUTLINE_WITH_CHILD);
    refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
    seen[name] = client.getQueryState(coverageKey("CORE-1"))?.isInvalidated === true;
  }
  expect(seen).toEqual({ "no tree read of the root": false, "the root's outline": true });
});

// ST-2, the shared wait on an outline (R3: decision 412's cost 3). The parent CORE-1 has its
// outline's refetch out when the events land, and CORE-2 and CORE-4 are its children. The
// versions name no ancestors, as an older server's do: a version that names CORE-1 marks its
// outline and views with no check (D-R2-presence), and so waits on nothing. A view that holds no
// node of the child waits for the outline's answer, and every event that waits on one outline
// shares one listener on the cache, which goes once the refetch ends and marks then, once, the
// views of each event whose node the answer holds. Each row's views read are those the base reads
// (J8, 00a3d845, as each join before it and U2, 647c646e); there L2, L3, L5, L6 and L7 held ten
// listeners, one for each event, and L4 two.

/** The parent CORE-1's outline holding no node of its children, as ST-2's outline holds it before
 *  its refetch answers. */
const OUTLINE_WITH_NO_CHILD = treeAnswer([treeNode("CORE-1", "issue", null), PARENT_REQUIREMENT]);

/** The parent CORE-1's outline holding its child CORE-4's node alone: CORE-2 moved away. */
const OUTLINE_WITH_D = treeAnswer([
  treeNode("CORE-1", "issue", null),
  PARENT_REQUIREMENT,
  treeNode("CORE-4", "issue", "CORE-1", { issue: "CORE-4", read_only: true, text: "Draw D" }),
]);

/** The views of the parent CORE-1 that ST-2's page holds beside its outline. */
const ST2_VIEWS = ["plan", "spec", "verification"] as const;

/** Each view of ST-2's page read once, and none read. */
const EACH_VIEW_READ = { plan: 1, spec: 1, verification: 1 };
const NO_VIEW_READ = { plan: 0, spec: 0, verification: 0 };

/** Ten versions of the child CORE-2's Verification document, which name no ancestors. */
const TEN_CHILD_VERSIONS: readonly DispatchEvent[] = Array.from({ length: 10 }, (_, index) =>
  eventOf("artifact.version", { artifact_id: "artifact-2", number: index + 2 }, "CORE-2")
);

/**
 * ST-2's page on `client`: the parent CORE-1's outline, holding `outline`, and its three views
 * (`parentBeforeTheCheck`), each observed as the new page observes them, each view's reads
 * counted, and a refetch of the outline out until `end` answers it or refuses it. The listeners
 * counted are the refresh checks' (`checkListeners`).
 */
function outlineRefetchOut(client: QueryClient, outline: TreeAnswer) {
  const listeners = checkListeners(client);
  parentBeforeTheCheck(client, outline);
  const reads = { plan: 0, spec: 0, verification: 0 };
  const stops = [observe(client, issueTreeKey("CORE-1"), outline)];
  for (const view of ST2_VIEWS) {
    const queryKey = viewKey("CORE-1", view);
    const answer = client.getQueryData(queryKey);
    stops.push(
      new QueryObserver(client, {
        queryFn: async () => {
          reads[view] += 1;
          return answer;
        },
        queryKey,
        staleTime: Number.POSITIVE_INFINITY,
      }).subscribe(() => undefined)
    );
  }
  const response = Promise.withResolvers<unknown>();
  const refetch = client
    .fetchQuery({ queryFn: () => response.promise, queryKey: issueTreeKey("CORE-1"), staleTime: 0 })
    .catch(() => undefined);
  return {
    end: async (ending: TreeAnswer | Error) => {
      if (ending instanceof Error) response.reject(ending);
      else response.resolve(ending);
      await refetch;
      await tick(3_000, 100);
    },
    listeners,
    reads: () => ({ ...reads }),
    stop: () => {
      for (const stop of stops) stop();
      client.clear();
    },
  };
}

/**
 * One row of ST-2 on a fresh client: the page of `outlineRefetchOut`, whose outline holds
 * `outline`; then `events`, each its own refresh; then `before`, a read the row makes while the
 * refetch is out; then the refetch's ending: an answer, a refusal, or the outline removed before
 * its answer lands. Answers the listeners the refresh checks held while the refetch was out and
 * once it ended, and each view's reads from the ending on.
 */
async function sharedWait(
  outline: TreeAnswer,
  events: readonly DispatchEvent[],
  ending: TreeAnswer | Error | "removed",
  before?: (client: QueryClient) => Promise<unknown>
): Promise<{ readonly listeners: readonly number[]; readonly viewsRead: Record<string, number> }> {
  jest.useFakeTimers();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = outlineRefetchOut(client, outline);
  try {
    for (const event of events) refreshQueries(client, eventRefreshTargets(event, "alice"));
    const during = page.listeners();
    await before?.(client);
    const readBefore = page.reads();
    if (ending === "removed") {
      client.removeQueries({ exact: true, queryKey: issueTreeKey("CORE-1") });
    }
    await page.end(ending === "removed" ? OUTLINE_WITH_CHILD : ending);
    const readAfter = page.reads();
    return {
      listeners: [during, page.listeners()],
      viewsRead: Object.fromEntries(
        ST2_VIEWS.map((view) => [view, readAfter[view] - readBefore[view]])
      ),
    };
  } finally {
    page.stop();
    realClock();
  }
}

// ST-2, L1. One version of the child waits on the outline's refetch with one listener, and the
// response, which holds the child's node, reads each view once.
test("a child's version during its parent's outline refetch holds one listener, and reads each view of the parent once where the response holds the child's node", async () => {
  expect(
    await sharedWait(OUTLINE_WITH_NO_CHILD, TEN_CHILD_VERSIONS.slice(0, 1), OUTLINE_WITH_CHILD)
  ).toEqual({ listeners: [1, 0], viewsRead: EACH_VIEW_READ });
});

// ST-2, L2, and P3, decision 412's cost 3: ten versions of the child, each its own refresh, wait on
// the outline's refetch with one listener on the cache, where the base held one for each event,
// and the response reads each view once.
test("ten versions of a child, each its own refresh, during its parent's outline refetch hold one listener, and read each view of the parent once where the response holds the child's node", async () => {
  // Measured at J8 (00a3d845), the base, and at each join before it and U2 (647c646e), by this
  // row: one listener for each event.
  const AT_BASE = 10;
  const AT_HEAD = 1;
  expect(
    await sharedWait(OUTLINE_WITH_NO_CHILD, TEN_CHILD_VERSIONS, OUTLINE_WITH_CHILD),
    `listeners while the refetch is out: ${AT_BASE} at the base (J8, 00a3d845), ${AT_HEAD} at the head`
  ).toEqual({ listeners: [AT_HEAD, 0], viewsRead: EACH_VIEW_READ });
});

// ST-2, L3. A response that holds no node of the child moves nothing the versions move, so no view
// is read, and the one listener goes.
test("ten versions of a child during its parent's outline refetch hold one listener, and read no view of the parent where the response holds no node of the child", async () => {
  expect(
    await sharedWait(OUTLINE_WITH_NO_CHILD, TEN_CHILD_VERSIONS, OUTLINE_WITH_NO_CHILD)
  ).toEqual({ listeners: [1, 0], viewsRead: NO_VIEW_READ });
});

// ST-2, L4. Versions of two children wait on one listener, each event with its own views and its
// own test: the response holds CORE-4's node alone - CORE-2 moved away - so the views are read
// once, for CORE-4's version.
test("versions of two children during their parent's outline refetch hold one listener, and read each view of the parent once where the response holds one child's node alone", async () => {
  // Measured at J8 (00a3d845), the base, and at each join before it and U2 (647c646e), by this
  // row: one listener for each event.
  const AT_BASE = 2;
  const AT_HEAD = 1;
  expect(
    await sharedWait(
      OUTLINE_WITH_NO_CHILD,
      [
        eventOf("artifact.version", { artifact_id: "artifact-2", number: 2 }, "CORE-2"),
        eventOf("artifact.version", { artifact_id: "artifact-4", number: 2 }, "CORE-4"),
      ],
      OUTLINE_WITH_D
    ),
    `listeners while the refetch is out: ${AT_BASE} at the base (J8, 00a3d845), ${AT_HEAD} at the head`
  ).toEqual({ listeners: [AT_HEAD, 0], viewsRead: EACH_VIEW_READ });
});

// ST-2, L5. A refused refetch leaves the answer the outline held, which holds the child's node, so
// the views are read once by it, as at the base.
test("ten versions of a child during its parent's outline refetch that is refused hold one listener, and read each view of the parent once by the answer the outline keeps", async () => {
  expect(await sharedWait(OUTLINE_WITH_CHILD, TEN_CHILD_VERSIONS, new Error("refused"))).toEqual({
    listeners: [1, 0],
    viewsRead: EACH_VIEW_READ,
  });
});

// ST-2, L6. An outline removed while its refetch is out marks no view, and the listener goes with
// it.
test("ten versions of a child during its parent's outline refetch hold one listener, and read no view of the parent once the outline is removed", async () => {
  expect(await sharedWait(OUTLINE_WITH_NO_CHILD, TEN_CHILD_VERSIONS, "removed")).toEqual({
    listeners: [1, 0],
    viewsRead: NO_VIEW_READ,
  });
});

// ST-2, L7. A view read again after the events judged it, and before the response, holds what its
// read answered, so the response reads every view but that one.
test("ten versions of a child during its parent's outline refetch hold one listener, and read every view of the parent but the one read again before the response", async () => {
  expect(
    await sharedWait(OUTLINE_WITH_NO_CHILD, TEN_CHILD_VERSIONS, OUTLINE_WITH_CHILD, (client) =>
      client.refetchQueries({ exact: true, queryKey: viewKey("CORE-1", "verification") })
    )
  ).toEqual({ listeners: [1, 0], viewsRead: { ...EACH_VIEW_READ, verification: 0 } });
});

// ST-2, L8. The waits are each client's own: the one version's checks, handed to two clients that
// each hold the parent's page with its outline's refetch out, hold one listener on each client's
// cache, and each response reads that client's views once.
test("a child's version on two clients, each with its parent's outline refetch out, holds one listener on each client, and reads each client's views once", async () => {
  jest.useFakeTimers();
  const pages = [0, 1].map(() => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return { client, page: outlineRefetchOut(client, OUTLINE_WITH_NO_CHILD) };
  });
  try {
    const targets = eventRefreshTargets(TEN_CHILD_VERSIONS[0] as DispatchEvent, "alice");
    for (const { client } of pages) refreshQueries(client, targets);
    const during = pages.map(({ page }) => page.listeners());
    for (const { page } of pages) await page.end(OUTLINE_WITH_CHILD);
    expect(
      pages.map(({ page }, at) => ({
        listeners: [during[at], page.listeners()],
        viewsRead: page.reads(),
      }))
    ).toEqual([
      { listeners: [1, 0], viewsRead: EACH_VIEW_READ },
      { listeners: [1, 0], viewsRead: EACH_VIEW_READ },
    ]);
  } finally {
    for (const { page } of pages) page.stop();
    realClock();
  }
});

// INV-7, the identity of a check (R3: decision 412's cost 2). The tree check of an event that
// moves a list of kinds reads the event's issue, those kinds, whether the event writes a
// document, and the ancestors it names (none, `[]` or a chain), and nothing else: two events that
// differ in one of them alone carry different identities (`same`), and two that agree in all of
// them carry one, which the feed's flush then runs once for each query. An ask's event is on its
// own target, which no identity names. An ancestor's coverage check is identified by the
// ancestor, and the coverage check of an event that names none by the event's issue.
test("the identity of a tree check and of an ancestor's coverage check differs where an input the check reads differs, and is one where every input is", () => {
  const version = (issue: string, payload: Record<string, unknown> = {}) =>
    eventOf("artifact.version", { artifact_id: `artifact-${issue}`, number: 2, ...payload }, issue);
  const sameOf = (event: DispatchEvent, queryKey: readonly unknown[]) =>
    eventRefreshTargets(event, "alice").find(
      (target) =>
        target.when !== undefined && JSON.stringify(target.queryKey) === JSON.stringify(queryKey)
    )?.same;
  const trees = (event: DispatchEvent) => sameOf(event, ["issue-tree"]);
  const differ = (a: string | undefined, b: string | undefined) =>
    a !== undefined && b !== undefined && a !== b;
  const one = (a: string | undefined, b: string | undefined) => a !== undefined && a === b;
  const chain = (ancestors?: readonly string[]) =>
    trees(version("CORE-2", ancestors === undefined ? {} : { ancestors }));
  const ask = eventOf("ask.opened", { anchor: null, id: "ask-1", state: "open" }, "CORE-2");
  expect({
    "a document write and a review": differ(
      trees(version("CORE-2", { ancestors: ["CORE-1"] })),
      trees(
        eventOf(
          "artifact.approved",
          { ancestors: ["CORE-1"], artifact_id: "artifact-CORE-2", ask_id: null },
          "CORE-2"
        )
      )
    ),
    "an ask's event": sameOf(ask, ["issue-tree"]) === undefined,
    "an ancestor's coverage, of two ancestors": differ(
      sameOf(version("CORE-3", { ancestors: ["CORE-2", "CORE-1"] }), coverageKey("CORE-2")),
      sameOf(version("CORE-3", { ancestors: ["CORE-2", "CORE-1"] }), coverageKey("CORE-1"))
    ),
    "an ancestor's coverage, of one ancestor": one(
      sameOf(version("CORE-2", { ancestors: ["CORE-1"] }), coverageKey("CORE-1")),
      sameOf(version("CORE-4", { ancestors: ["CORE-1"] }), coverageKey("CORE-1"))
    ),
    "coverage of an event that names no ancestors, of one issue": one(
      sameOf(version("CORE-2"), ["issue"]),
      sameOf(version("CORE-2", { number: 3 }), ["issue"])
    ),
    "coverage of an event that names no ancestors, of two issues": differ(
      sameOf(version("CORE-2"), ["issue"]),
      sameOf(version("CORE-4"), ["issue"])
    ),
    "every input equal": one(
      trees(version("CORE-2", { ancestors: ["CORE-1"] })),
      trees(version("CORE-2", { ancestors: ["CORE-1"], artifact_id: "artifact-7", number: 5 }))
    ),
    "no ancestors and a chain": differ(chain(undefined), chain(["CORE-1"])),
    "no ancestors and none": differ(chain(undefined), chain([])),
    "none and a chain": differ(chain([]), chain(["CORE-1"])),
    "the issue": differ(
      trees(version("CORE-2", { ancestors: ["CORE-1"] })),
      trees(version("CORE-4", { ancestors: ["CORE-1"] }))
    ),
    "the kinds": differ(
      trees(eventOf("issue.updated", { ancestors: ["CORE-1"], changed: ["title"] }, "CORE-2")),
      trees(eventOf("issue.updated", { ancestors: ["CORE-1"], changed: ["parent"] }, "CORE-2"))
    ),
    "two chains": differ(chain(["CORE-1"]), chain(["CORE-5", "CORE-1"])),
  }).toEqual({
    "a document write and a review": true,
    "an ask's event": true,
    "an ancestor's coverage, of one ancestor": true,
    "an ancestor's coverage, of two ancestors": true,
    "coverage of an event that names no ancestors, of one issue": true,
    "coverage of an event that names no ancestors, of two issues": true,
    "every input equal": true,
    "no ancestors and a chain": true,
    "no ancestors and none": true,
    "none and a chain": true,
    "the issue": true,
    "the kinds": true,
    "two chains": true,
  });
});

/** An answer whose nodes count each read of them in `scans`, under `name`: how many times a
 *  refresh scans the answer. */
function scanned(answer: TreeAnswer, name: string, scans: Map<string, number>): TreeAnswer {
  const { nodes, ...rest } = answer;
  return Object.defineProperty({ ...rest }, "nodes", {
    enumerable: true,
    get: () => {
      scans.set(name, (scans.get(name) ?? 0) + 1);
      return nodes;
    },
  }) as TreeAnswer;
}

/**
 * Fifty issues as a page that holds fifty open trees holds them, for the probes of decision 412's
 * costs (after K1 of INFRA-4062's round 18 probes): the parent CORE-1, whose outline holds its
 * child CORE-2's node, and 49 others, each with its outline and its Spec, Verification and Plan
 * views (200 tree reads), each holding 40 requirements of its issue and counting its scans in
 * `scans`, and each issue's detail and coverage read.
 */
function fiftyTrees(scans: Map<string, number>): QueryClient {
  const client = new QueryClient();
  for (let index = 0; index < 50; index += 1) {
    const key = index === 0 ? "CORE-1" : `BULK-${index}`;
    const own = Array.from({ length: 40 }, (_, at) =>
      treeNode(`b${at}`, "requirement", key, {
        in: "spec",
        issue: key === "CORE-1" ? undefined : key,
      })
    );
    const child = key === "CORE-1" ? [treeNode("CORE-2", "issue", "CORE-1", CHILD)] : [];
    client.setQueryData(
      issueTreeKey(key),
      scanned(
        treeAnswer([treeNode(key, "issue", null), ...own, ...child], [], { issue: key }),
        key,
        scans
      )
    );
    for (const view of ST2_VIEWS) {
      client.setQueryData(
        viewKey(key, view),
        scanned(treeAnswer(own, [], { issue: key, view }), `${key}/${view}`, scans)
      );
    }
    client.setQueryData(["issue", key], { key });
    client.setQueryData(coverageKey(key), COVERAGE);
  }
  return client;
}

/** A refresh target as `eventRefreshTargets` makes it: this file imports no name of
 *  `query-refresh.ts` but the two it reads. */
type Target = ReturnType<typeof eventRefreshTargets>[number];

/** `targets` with each check counted in `checks`, by the hash of the query it judges, and each
 *  check of D-R2-fallback's target (the one under `["issue"]` alone) in `fallback` too. */
function countedChecks(
  targets: readonly Target[],
  checks: Map<string, number>,
  fallback: { count: number }
): Target[] {
  return targets.map((target) => {
    const { when } = target;
    if (when === undefined) return target;
    const isFallback = target.queryKey.length === 1 && target.queryKey[0] === "issue";
    return {
      ...target,
      when: (data, query, client) => {
        checks.set(query.queryHash, (checks.get(query.queryHash) ?? 0) + 1);
        if (isFallback) fallback.count += 1;
        return when(data, query, client);
      },
    };
  });
}

/** The checks `checks` counted on tree reads (`["issue-tree", …]`, by their hash): in all, and
 *  the most on one read. */
function treeCheckCounts(checks: Map<string, number>): { checks: number; most: number } {
  const counts = [...checks].filter(([hash]) => hash.startsWith('["issue-tree"')).map(([, n]) => n);
  return { checks: counts.reduce((sum, n) => sum + n, 0), most: Math.max(0, ...counts) };
}

// P1, decision 412's cost 1: the checks one event runs over 200 open tree reads stay one for each
// read the event does not name. Any held answer can hold a node of the event's issue - a parent's
// outline its children's, a descendant's view its ancestors' requirements - so only a read of
// each answer tells; an index of which answers hold which issue would be a second record of the
// trees beside the cache, and would miss an answer that lands while a check waits for it. A
// version that names its chain marks the parent's four reads with no check (D-R2-presence), and
// scans no answer to place the child (`issuesAbove` runs for an event that names none alone), so
// its scans fall by half. A version that names none runs D-R2-fallback's check on every read under
// `["issue"]`, each issue's detail read and its coverage read (100 here), and the check passes the
// coverage reads alone. The milliseconds are not asserted: a failure's message gives them beside
// the base's counts, and the pull request's body gives them as measured.
test("one event over fifty open trees checks each tree read it does not name once, and a version that names its chain scans each answer once", () => {
  const events = {
    "C's plain ask": eventOf(
      "ask.opened",
      { anchor: null, id: "q", question: "?", state: "open" },
      "CORE-2"
    ),
    "C's title": eventOf(
      "issue.updated",
      { ancestors: ["CORE-1"], changed: ["title"], title: "T" },
      "CORE-2"
    ),
    "C's version with its chain": eventOf(
      "artifact.version",
      { ancestors: ["CORE-1"], artifact_id: "artifact-2", number: 2 },
      "CORE-2"
    ),
    "C's version with no chain": eventOf(
      "artifact.version",
      { artifact_id: "artifact-2", number: 2 },
      "CORE-2"
    ),
  };
  // Measured at J8 (00a3d845), the base, and at each join before it and U2 (647c646e), by this
  // case's cache and events: there a version reads no chain, and no coverage target of an ancestor
  // exists.
  const AT_BASE = {
    "C's plain ask": { checks: 200, fallbackChecks: 0, scans: 200 },
    "C's title": { checks: 200, fallbackChecks: 0, scans: 200 },
    "C's version with its chain": { checks: 200, fallbackChecks: 0, scans: 400 },
    "C's version with no chain": { checks: 200, fallbackChecks: 0, scans: 400 },
  };
  const AT_HEAD = {
    "C's plain ask": { checks: 200, fallbackChecks: 0, scans: 200 },
    "C's title": { checks: 200, fallbackChecks: 0, scans: 200 },
    "C's version with its chain": { checks: 196, fallbackChecks: 0, scans: 196 },
    "C's version with no chain": { checks: 200, fallbackChecks: 100, scans: 400 },
  };
  const seen: Record<string, unknown> = {};
  const milliseconds: Record<string, number> = {};
  for (const [name, event] of Object.entries(events)) {
    const scans = new Map<string, number>();
    const client = fiftyTrees(scans);
    const checks = new Map<string, number>();
    const fallback = { count: 0 };
    const targets = countedChecks(eventRefreshTargets(event, "alice"), checks, fallback);
    const started = performance.now();
    refreshQueries(client, targets);
    milliseconds[name] = Math.round((performance.now() - started) * 10) / 10;
    seen[name] = {
      checks: treeCheckCounts(checks).checks,
      fallbackChecks: fallback.count,
      scans: [...scans.values()].reduce((sum, n) => sum + n, 0),
    };
  }
  expect(
    seen,
    `at the base (J8, 00a3d845): ${JSON.stringify(AT_BASE)}; milliseconds at this run: ${JSON.stringify(milliseconds)}`
  ).toEqual(AT_HEAD);
});

// P2, decision 412's cost 2: the checks a batch of ten events runs on one tree read, through the
// feed's flush (`dedupeRefreshTargets`, then `refreshQueries`). Ten versions of the child carry
// one identity, so the flush keeps one tree check of them, which runs once on each tree read, and
// on none of the parent's, which the chain names. Ten plain asks of the child are each on a node
// of their own (`onItsTarget`), so no check stands for another and the count stays ten: one
// matcher for the batch would copy the frozen rule's block, anchor and document match, whose
// edges decision 412 found one round at a time.
test("a batch of ten versions of a child runs one check on a tree read through the feed's flush, and a batch of ten plain asks of it runs ten", () => {
  const batches = {
    "ten plain asks of C": Array.from({ length: 10 }, (_, index) =>
      eventOf(
        "ask.opened",
        { anchor: null, id: `q${index}`, question: "?", state: "open" },
        "CORE-2"
      )
    ),
    "ten versions of C": Array.from({ length: 10 }, (_, index) =>
      eventOf(
        "artifact.version",
        { ancestors: ["CORE-1"], artifact_id: `artifact-${index}`, number: 2 },
        "CORE-2"
      )
    ),
  };
  // Measured at J8 (00a3d845), the base, and at each join before it and U2 (647c646e), by this
  // case's cache and batches, each batch's targets handed to the refresh as they are: there the
  // flush keeps every target with a check, and a version reads no chain. A check that answers yes
  // on a query ends that query's checks, so the parent's four reads ran one check each there.
  const AT_BASE = {
    "ten plain asks of C": { checks: 2000, most: 10 },
    "ten versions of C": { checks: 1964, most: 10 },
  };
  const AT_HEAD = {
    "ten plain asks of C": { checks: 2000, most: 10 },
    "ten versions of C": { checks: 196, most: 1 },
  };
  const seen: Record<string, unknown> = {};
  for (const [name, batch] of Object.entries(batches)) {
    const client = fiftyTrees(new Map());
    const checks = new Map<string, number>();
    const targets = batch.flatMap((event) =>
      countedChecks(eventRefreshTargets(event, "alice"), checks, { count: 0 })
    );
    refreshQueries(client, dedupeRefreshTargets(targets));
    seen[name] = treeCheckCounts(checks);
  }
  expect(
    seen,
    `checks on tree reads, in all and on one at the most; at the base (J8, 00a3d845): ${JSON.stringify(AT_BASE)}`
  ).toEqual(AT_HEAD);
});

/**
 * The one cache E2 copies fresh for each batch, which holds, for each event of a batch, a read
 * that its own check alone marks: the parent CORE-1's outline, holding its children CORE-2's and
 * CORE-4's nodes, and its three views; CORE-2's outline, holding its child CORE-3's node, and its
 * Verification view, holding CORE-1's requirement; the Verification views of CORE-3 and of
 * CORE-4's child CORE-5, each holding its parent's requirement, which a version of that parent
 * moves and a title does not; CORE-0's outline, read before CORE-1 moved from under it to the
 * root, which holds CORE-1's node, and which a document write of CORE-1's child reaches only
 * where it places CORE-1 above the child; another project's outline; the detail and the coverage
 * of each issue, each detail naming its parent; CORE-2's asks and documents; and a project
 * document's read.
 */
function oneCache(): QueryClient {
  const client = new QueryClient();
  client.setQueryData(
    issueTreeKey("CORE-1"),
    treeAnswer(
      [
        treeNode("CORE-1", "issue", null),
        PARENT_REQUIREMENT,
        treeNode("CORE-2", "issue", "CORE-1", CHILD),
        treeNode("CORE-4", "issue", "CORE-1", { issue: "CORE-4", read_only: true }),
      ],
      [],
      { issue: "CORE-1" }
    )
  );
  parentBeforeTheCheck(client, undefined);
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
  viewHoldingParent(client);
  for (const [key, parent] of [
    ["CORE-3", "CORE-2"],
    ["CORE-5", "CORE-4"],
  ] as const) {
    const requirement = treeNode("cr1", "requirement", null, {
      in: "spec",
      issue: parent,
      read_only: true,
      role: "context",
    });
    client.setQueryData(
      viewKey(key, "verification"),
      treeAnswer([treeNode(key, "issue", null), requirement], [], {
        issue: key,
        view: "verification",
      })
    );
  }
  client.setQueryData(
    issueTreeKey("CORE-0"),
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
    issueTreeKey("OTHER-9"),
    treeAnswer([treeNode("OTHER-9", "issue", null)], [], { issue: "OTHER-9" })
  );
  for (const [key, parent] of [
    ["CORE-0", null],
    ["CORE-1", null],
    ["CORE-2", "CORE-1"],
    ["CORE-3", "CORE-2"],
    ["CORE-4", "CORE-1"],
    ["CORE-5", "CORE-4"],
  ] as const) {
    client.setQueryData(["issue", key], { key, parent });
    client.setQueryData(coverageKey(key), COVERAGE);
  }
  client.setQueryData(["asks", "CORE-2"], []);
  client.setQueryData(["artifacts", "CORE-2"], []);
  client.setQueryData(["artifact", "artifact-9"], { id: "artifact-9" });
  return client;
}

// E2, for decision 412's cost 2: the flush's dedupe changes no read a batch marks. Each batch goes
// to a fresh copy of one cache twice, once through `dedupeRefreshTargets` and once with every
// target kept, and both mark the same reads: versions of one issue and of two, a version beside
// a title, a title beside a parent move, a review beside a version, asks, versions that name
// different chains or none, and a project document's version among them. Where two events of a
// batch differ in one input of their check, the cache holds a read that the later one alone
// marks.
test("a batch marks the same reads through the feed's dedupe as with every target kept", () => {
  const version = (issue: string, ancestors?: readonly string[]) =>
    eventOf(
      "artifact.version",
      { artifact_id: `artifact-${issue}`, number: 2, ...(ancestors ? { ancestors } : {}) },
      issue
    );
  const batches: Record<string, readonly DispatchEvent[]> = {
    "a mix with a project's event": [
      version("CORE-2", ["CORE-1"]),
      eventOf("artifact.version", { artifact_id: "artifact-9", number: 2 }, null),
      version("CORE-2", ["CORE-1"]),
      eventOf("artifact.version", { artifact_id: "artifact-9", number: 3 }, null),
    ],
    "a review and a version of C": [
      eventOf(
        "artifact.approved",
        { ancestors: ["CORE-1"], artifact_id: "artifact-CORE-2", ask_id: null },
        "CORE-2"
      ),
      version("CORE-2", ["CORE-1"]),
    ],
    "a title and a parent move of C": [
      eventOf("issue.updated", { ancestors: ["CORE-1"], changed: ["title"] }, "CORE-2"),
      eventOf("issue.updated", { ancestors: ["CORE-1"], changed: ["parent"] }, "CORE-2"),
    ],
    "a version and a title of C": [
      version("CORE-2", ["CORE-1"]),
      eventOf("issue.updated", { ancestors: ["CORE-1"], changed: ["title"] }, "CORE-2"),
      version("CORE-2", ["CORE-1"]),
    ],
    "ten asks of C": Array.from({ length: 10 }, (_, index) =>
      eventOf("ask.opened", { anchor: null, id: `q${index}`, state: "open" }, "CORE-2")
    ),
    "ten versions of C": Array.from({ length: 10 }, () => version("CORE-2", ["CORE-1"])),
    "versions of C and of D": [
      version("CORE-2", ["CORE-1"]),
      version("CORE-4", ["CORE-1"]),
      version("CORE-2", ["CORE-1"]),
      version("CORE-4", ["CORE-1"]),
    ],
    "versions of C with different chains": [
      version("CORE-2", []),
      version("CORE-2"),
      version("CORE-2", ["CORE-1"]),
      version("CORE-2", []),
    ],
    "a version of C with none, then one with no chain": [version("CORE-2", []), version("CORE-2")],
    "a version of C with none, then one with a chain": [
      version("CORE-2", []),
      version("CORE-2", ["CORE-1"]),
    ],
    "versions of a grandchild with different chains": [
      version("CORE-3", []),
      version("CORE-3"),
      version("CORE-3", GRANDCHILD_ANCESTORS),
      version("CORE-3", GRANDCHILD_ANCESTORS),
    ],
  };
  const marks = (batch: readonly DispatchEvent[], dedupe: boolean) => {
    const client = oneCache();
    const targets = batch.flatMap((event) => eventRefreshTargets(event, "alice"));
    refreshQueries(client, dedupe ? dedupeRefreshTargets(targets) : targets);
    return client
      .getQueryCache()
      .getAll()
      .filter((query) => query.state.isInvalidated)
      .map((query) => query.queryHash)
      .sort();
  };
  const deduped: Record<string, string[]> = {};
  const kept: Record<string, string[]> = {};
  for (const [name, batch] of Object.entries(batches)) {
    deduped[name] = marks(batch, true);
    kept[name] = marks(batch, false);
  }
  expect(
    Object.values(kept).every((marked) => marked.length > 0),
    "each batch marks a read"
  ).toBe(true);
  expect(deduped).toEqual(kept);
});
