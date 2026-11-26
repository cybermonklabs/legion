import { expect, test } from "bun:test";
import { screen, waitFor } from "@testing-library/react";

import type { Message } from "../../api/types";
import { KEY } from "./decisions-filter/decisions-fixtures";
import { conversationPath } from "./decisions-filter/decisions-parts";
import { agentsOnlyItem, issueAddress, pathToTarget, presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { type NodeType, PRESET_ORDER, type Preset } from "./page-words";
import { drawStage, stubReads } from "./slots/stage-fixtures";
import { CHORDS } from "./useIssueKeymap";

/** A message of the issue, which the rows below give an author and an id. */
const messageFixture: Message = {
  author: { id: "alice", kind: "user" },
  body: "A message.",
  broadcast_id: null,
  created_at: "2026-09-11T00:00:00Z",
  deleted_at: null,
  deleted_by: null,
  deliveries: [],
  edited_at: null,
  id: "message",
  in_reply_to: null,
  issue_key: "CORE-1",
  reply_block: null,
  target: null,
};

function node(id: string, type: NodeType, over: Partial<TreeNode> = {}): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...over,
  };
}

const askNode = node("spec#b-q-open", "ask", {
  reference: "dispatch://CORE-1/ask/ask-9",
});
const commentNode = node("comment-7", "comment", {
  reference: "dispatch://CORE-1/comment/comment-7",
});
const tree = node("CORE-1", "issue", {
  children: [node("spec", "document", { children: [askNode] }), commentNode],
});

test("each control of the filter row carries its own address (R2, R20)", () => {
  expect(presetPath("CORE-1", "dashboard")).toBe("/issues/CORE-1/dashboard");
  expect(presetPath("CORE-1", "conversation")).toBe("/issues/CORE-1/conversation");
  expect(presetPath("CORE-1", "spec")).toBe("/issues/CORE-1/spec");
  expect(presetPath("CORE-1", "verification")).toBe("/issues/CORE-1/verification");
  expect(presetPath("CORE-1", "plan")).toBe("/issues/CORE-1/plan");
  expect(presetPath("CORE-1", "decisions")).toBe("/issues/CORE-1/decisions");
});

test("the address says which filter it opens, which document, and which node it names", () => {
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "", "spec").preset).toBe("dashboard");
  expect(issueAddress({ key: "CORE-1", kind: "conversation" }, "", "", "spec").preset).toBe(
    "conversation"
  );
  for (const kind of ["dashboard", "spec", "verification", "plan", "decisions"] as const) {
    expect(issueAddress({ key: "CORE-1", kind }, "", "", "spec").preset).toBe(kind);
  }
  // A route naming the issue's own spec by its slug opens the spec itself, in place of the
  // outline, as every document's own address does; `/spec` is the Spec filter (R3, R20).
  const spec = issueAddress({ key: "CORE-1", kind: "artifact", slug: "spec" }, "?v=2", "", "spec");
  expect(spec.preset).toBe("conversation");
  expect(spec.document).toBe("spec");
  expect(spec.version).toBe(2);
  expect(issueAddress({ key: "CORE-1", kind: "spec" }, "", "", "spec").document).toBeUndefined();
  expect(issueAddress({ key: "CORE-1", kind: "artifacts" }, "", "", "spec").document).toBe(
    undefined
  );
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "?ask=ask-9", "", "spec").target).toEqual({
    id: "ask-9",
    kind: "item",
  });
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-r3", "spec").target).toEqual({
    id: "r3",
    kind: "block",
  });
  // An ask, a comment or a message an address names by its own route is that node of the
  // outline, which the tree puts the focus on, wherever the item is anchored (R20): no document
  // is opened for it.
  for (const kind of ["ask", "comment", "message"] as const) {
    const item = issueAddress({ id: "item-9", key: "CORE-1", kind }, "", "", "spec");
    expect(item.target, `/${kind}s/<id> names no node to focus`).toEqual({
      id: "item-9",
      kind: "item",
    });
    expect(item.document, `/${kind}s/<id> opens a document`).toBeUndefined();
    expect(item.preset, `/${kind}s/<id> opens a filter`).toBe("conversation");
  }
});

// dispatch://INFRA-5702/spec#b-r-new-page (R2): the issue's own address opens Dashboard where it
// names no node and no words filter, as `/dashboard` does, and Conversation at what it names
// where it names a block (`#b-`), an ask (`?ask=`), a comment (`?comment=`) or words (`?q=`), as
// Conversation's own address does.
test("the issue's own address opens Dashboard unless it names a node or words, and each of those opens Conversation at it", () => {
  const own = { key: "CORE-1", kind: "issue" } as const;
  for (const [address, route, search, hash, preset, target, filter] of [
    ["/issues/CORE-1", own, "", "", "dashboard", undefined, ""],
    [
      "/issues/CORE-1/dashboard",
      { key: "CORE-1", kind: "dashboard" },
      "",
      "",
      "dashboard",
      undefined,
      "",
    ],
    [
      "/issues/CORE-1/conversation",
      { key: "CORE-1", kind: "conversation" },
      "",
      "",
      "conversation",
      undefined,
      "",
    ],
    ["/issues/CORE-1#b-r3", own, "", "#b-r3", "conversation", { id: "r3", kind: "block" }, ""],
    [
      "/issues/CORE-1?ask=ask-9",
      own,
      "?ask=ask-9",
      "",
      "conversation",
      { id: "ask-9", kind: "item" },
      "",
    ],
    [
      "/issues/CORE-1?comment=comment-7",
      own,
      "?comment=comment-7",
      "",
      "conversation",
      { id: "comment-7", kind: "item" },
      "",
    ],
    ["/issues/CORE-1?q=routes", own, "?q=routes", "", "conversation", undefined, "routes"],
    [
      "/issues/CORE-1/conversation#b-r3",
      { key: "CORE-1", kind: "conversation" },
      "",
      "#b-r3",
      "conversation",
      { id: "r3", kind: "block" },
      "",
    ],
  ] as const) {
    const opened = issueAddress(route, search, hash, "spec");
    expect(opened.preset, `${address} opens another view`).toBe(preset satisfies Preset);
    expect(opened.target, `${address} names another node`).toEqual(target);
    expect(opened.filter, `${address} narrows to other words`).toBe(filter);
  }
});

// dispatch://INFRA-5702/spec#b-r-new-page (R2): Conversation's own address is `/conversation`,
// since the issue's own opens Dashboard, and every place of the page that writes an address for
// Conversation writes that one: the view row's control (`presetPath`), the Decisions view's link
// to a node in the conversation (`conversationPath`), the chords that turn the views off and
// `t c` (`useIssueKeymap.ts`), and the way back from a line that says an address names nothing
// the issue holds (`slots/body.tsx`).
test("Conversation's address is /conversation from each place that writes it", async () => {
  const conversation = `/issues/${KEY}/conversation`;
  expect(presetPath(KEY, "conversation")).toBe(conversation);
  expect(conversationPath({ reference: `dispatch://${KEY}/spec#b-r1` })).toBe(
    `${conversation}#b-r1`
  );
  for (const chord of CHORDS) {
    const active = PRESET_ORDER.find((view) => view === chord.tab);
    if (active === undefined) throw new Error(`the chord ${chord.keys} names no view`);
    expect(chord.path(KEY, active), `${chord.keys} on its own view`).toBe(conversation);
  }
  const reads = stubReads();
  const view = drawStage("#b-no-such-block", { preset: "conversation" });
  try {
    const back = await waitFor(() => screen.getByRole("link", { name: `Back to ${KEY}` }));
    expect(back.getAttribute("href")).toBe(conversation);
  } finally {
    view.unmount();
    for (const spy of reads) spy.mockRestore();
  }
});

// R20: an ask or a comment an address names beside it (`?ask=`, `?comment=`) is a node of the
// outline, so the address opens the outline on it whichever document's address it is: `/spec`,
// `/artifacts/<slug>`, or the spec's own. A version pin beside it (`?v=`) keeps the document,
// whose quote as it stood then only that version shows, and so does `?mark=`, the address "View
// in document" builds, which names an ask or a comment by its id alone (`mark`, which the page
// tells apart). `/spec#b-` still names content in the spec, so it opens the spec, as its own
// address does with the same fragment; plain `/spec`, and `/spec` with a `?q=`, is the Spec
// filter. Which block of the spec opens the outline instead is the page's to say.
test("an address naming an ask or a comment opens the outline, unless it pins a version", () => {
  const spec = { key: "CORE-1", kind: "spec" } as const;
  for (const [route, search] of [
    [spec, "?comment=comment-7"],
    [spec, "?ask=comment-7"],
    [{ key: "CORE-1", kind: "artifact", slug: "spec" }, "?comment=comment-7"],
    [{ key: "CORE-1", kind: "artifact", slug: "notes-md" }, "?ask=comment-7"],
  ] as const) {
    const named = issueAddress(route, search, "#b-p1", "spec");
    expect(named.document, `${route.kind} ${search} opens a document`).toBeUndefined();
    expect(named.documentItem, `${route.kind} ${search} lands in a document`).toBeUndefined();
    expect(named.target).toEqual({ id: "comment-7", kind: "item" });
  }
  for (const [route, search, document, item] of [
    [spec, "?v=1&comment=comment-7", "spec", { id: "comment-7", kind: "comment" }],
    [
      { key: "CORE-1", kind: "artifact", slug: "notes-md" },
      "?v=2&ask=ask-7",
      "notes-md",
      { id: "ask-7", kind: "ask" },
    ],
    [
      { key: "CORE-1", kind: "artifact", slug: "spec" },
      "?mark=comment-7",
      "spec",
      { id: "comment-7", kind: "mark" },
    ],
    [spec, "?mark=ask-7", "spec", { id: "ask-7", kind: "mark" }],
  ] as const) {
    const opened = issueAddress(route, search, "", "spec");
    expect(opened.document, `${route.kind} ${search} opens no document`).toBe(document);
    expect(opened.documentItem).toEqual(item);
  }
  // A version pin on the issue's own address names no document, so the outline stands.
  const own = issueAddress({ key: "CORE-1", kind: "issue" }, "?v=1&comment=comment-7", "", "spec");
  expect(own.document).toBeUndefined();
  expect(own.target).toEqual({ id: "comment-7", kind: "item" });
  expect(issueAddress(spec, "?comment=comment-7", "", "spec").preset).toBe("spec");
  const block = issueAddress(spec, "?v=3", "#b-p1", "spec");
  expect(block.document).toBe("spec");
  expect(block.version).toBe(3);
  expect(block.target).toEqual({ document: "spec", id: "p1", kind: "block" });
  expect(issueAddress(spec, "", "", "spec").document).toBeUndefined();
  expect(issueAddress(spec, "?q=routes", "", "spec").document).toBeUndefined();
  // Until the issue's documents are read there is no spec to open.
  expect(issueAddress(spec, "?v=3", "#b-p1", undefined).document).toBeUndefined();
});

// R20: a comment or a message agents wrote outside every thread a person wrote in lands in the
// history, not the outline; one in a thread a person wrote in, and a reply under an ask, is a
// node of the outline like any other.
test("an item in a thread no person wrote in is the history's, and every other the outline's", () => {
  const agent = { id: "session-1", kind: "session" } as const;
  const person = { id: "alice", kind: "user" } as const;
  const comments = [
    { ask_id: null, author: agent, id: "agents-root", reply_to: null },
    { ask_id: null, author: agent, id: "agents-reply", reply_to: "agents-root" },
    { ask_id: null, author: agent, id: "mixed-root", reply_to: null },
    { ask_id: null, author: person, id: "mixed-reply", reply_to: "mixed-root" },
    { ask_id: "ask-1", author: agent, id: "under-ask", reply_to: null },
  ];
  const messages = [
    {
      message: { ...messageFixture, author: agent, id: "agents-message" },
      replies: [{ ...messageFixture, author: agent, id: "agents-message-reply" }],
    },
    {
      message: { ...messageFixture, author: agent, id: "asked-message" },
      replies: [{ ...messageFixture, author: person, id: "answered-message" }],
    },
  ];
  const landing = (id: string) =>
    agentsOnlyItem({ id, kind: "item" }, comments, messages, undefined);
  for (const id of ["agents-root", "agents-reply", "agents-message", "agents-message-reply"]) {
    expect(landing(id), `${id} is drawn in the outline`).toBe(id);
  }
  for (const id of [
    "mixed-root",
    "mixed-reply",
    "under-ask",
    "asked-message",
    "answered-message",
  ]) {
    expect(landing(id), `${id} lands in the history`).toBeUndefined();
  }
  // Nothing lands in the history until the reads that tell have answered, nor for a block.
  expect(
    agentsOnlyItem({ id: "agents-root", kind: "item" }, undefined, undefined, undefined)
  ).toBeUndefined();
  expect(
    agentsOnlyItem({ id: "agents-root", kind: "block" }, comments, messages, undefined)
  ).toBeUndefined();
});

test("an item is found by its own id, and an ask by the address of the ask it was indexed from", () => {
  const byId = pathToTarget(tree, { id: "comment-7", kind: "item" });
  expect(byId?.map((found) => found.id)).toEqual(["CORE-1", "comment-7"]);

  const byReference = pathToTarget(tree, { id: "ask-9", kind: "item" });
  expect(byReference?.map((found) => found.id)).toEqual(["CORE-1", "spec", "spec#b-q-open"]);

  const byBlock = pathToTarget(tree, { id: "q-open", kind: "block" });
  expect(byBlock?.map((found) => found.id)).toEqual(["CORE-1", "spec", "spec#b-q-open"]);

  expect(pathToTarget(tree, { id: "nothing", kind: "item" })).toBeUndefined();
  expect(pathToTarget(tree, undefined)).toBeUndefined();
});

// A fragment is a reader's own text and can be anything: `#b-50%` is no percent-escape, and
// `decodeURIComponent` throws a `URIError` on it. This runs in a render, so a throw here puts
// the whole issue page in its error boundary; an unreadable fragment names no block instead,
// and the address falls back to what its route names.
test("an unreadable block fragment names no block and keeps the route's own target", () => {
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-50%", "spec").target).toBe(
    undefined
  );
  // A document's own address names the document it opens and no node: a document is none.
  const plan = issueAddress(
    { key: "CORE-1", kind: "artifact", slug: "plan-md" },
    "",
    "#b-50%",
    "spec"
  );
  expect(plan.target).toBeUndefined();
  expect(plan.document).toBe("plan-md");
  // An escape that does read is the block it names, decoded.
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-a%20b", "spec").target).toEqual({
    document: undefined,
    id: "a b",
    kind: "block",
  });
});

// An ask's, a comment's or a message's own address with a `#b-` fragment names a part of that
// item - an ask's question, an option or its answer, a block of a comment - which the reference
// graph names `dispatch://KEY/ask/<id>#b-<part>` (R20). No document holds that part, and the
// outline draws it inside the item's own row, so the address lands on the item's node.
test("an item's own address with a part fragment names the item's node", () => {
  for (const kind of ["ask", "comment", "message"] as const) {
    expect(issueAddress({ id: "item-7", key: "CORE-1", kind }, "", "#b-o2", "spec").target).toEqual(
      {
        id: "item-7",
        kind: "item",
      }
    );
  }
});

// A block id is the author's and unique inside its own document alone, so an address naming a
// document resolves inside that document: `/issues/CORE-1/artifacts/plan-md#b-t1` is the plan's
// `t1` and never the spec's. An address naming no document - the issue's own, with a fragment -
// has only the block id to go on.
test("a block of a document's address is found in that document, not in another one's", () => {
  const specTask = node("spec#b-t1", "plan_task");
  const planTask = node("plan-md#b-t1", "plan_task");
  const documents = node("CORE-1", "issue", {
    children: [
      node("spec", "document", { children: [specTask] }),
      node("plan-md", "document", { children: [planTask] }),
    ],
  });

  const address = issueAddress(
    { key: "CORE-1", kind: "artifact", slug: "plan-md" },
    "",
    "#b-t1",
    "spec"
  );
  expect(address.target).toEqual({ document: "plan-md", id: "t1", kind: "block" });
  expect(pathToTarget(documents, address.target)?.map((found) => found.id)).toEqual([
    "CORE-1",
    "plan-md",
    "plan-md#b-t1",
  ]);
  // A document the tree does not hold that block of answers nothing, rather than another
  // document's block of the same id.
  expect(pathToTarget(documents, { document: "verification-md", id: "t1", kind: "block" })).toBe(
    undefined
  );
  // The issue's own address with a fragment names no document: the first node carrying the
  // block id answers, which is the behaviour every such address has always had.
  expect(
    pathToTarget(
      documents,
      issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-t1", "spec").target
    )?.map((found) => found.id)
  ).toEqual(["CORE-1", "spec", "spec#b-t1"]);
});

// `/issues/CORE-1/spec#b-t1` names the spec as much as `/artifacts/<spec>#b-t1` does, so its
// block is the spec's own `t1` and never the plan's: matching by the block id alone landed a
// reader of the spec on another document's author-chosen `#b-t1`.
test("a block a spec address names is found in the spec, not in another document", () => {
  const planOnly = node("CORE-1", "issue", {
    children: [
      node("spec-md", "document"),
      node("plan-md", "document", { children: [node("plan-md#b-t1", "plan_task")] }),
    ],
  });
  const both = node("CORE-1", "issue", {
    children: [
      node("plan-md", "document", { children: [node("plan-md#b-t1", "plan_task")] }),
      node("spec-md", "document", { children: [node("spec-md#b-t1", "requirement")] }),
    ],
  });
  const addresses = [
    issueAddress({ key: "CORE-1", kind: "spec" }, "", "#b-t1", "spec-md"),
    issueAddress({ block: { id: "t1" }, key: "CORE-1", kind: "spec" }, "", "", "spec-md"),
  ];
  for (const address of addresses) {
    expect(
      pathToTarget(planOnly, address.target)?.map((found) => found.id),
      "the spec's address lands on another document's block of the same id"
    ).toBeUndefined();
    expect(pathToTarget(both, address.target)?.map((found) => found.id)).toEqual([
      "CORE-1",
      "spec-md",
      "spec-md#b-t1",
    ]);
    expect(address.target).toEqual({ document: "spec-md", id: "t1", kind: "block" });
  }
});

// R20: the children's own address (`/children`, today's Children tab) names the first row of
// their group in the outline, which the page unfolds the path to and puts the focus on; the
// documents' address (`/artifacts`) opens the node menu.
test("the children's address names the first row of their group, and the documents' address opens the node menu", () => {
  const children = issueAddress({ key: "CORE-1", kind: "children" }, "", "", "spec");
  expect(children.target).toEqual({ id: "CORE-1", kind: "children" });
  expect(children.preset).toBe("conversation");
  expect(children.nodeMenu).toBe(false);
  const withChildren = node("CORE-1", "issue", {
    children: [askNode, node("CORE-2", "child_issue"), node("CORE-3", "child_issue")],
  });
  expect(pathToTarget(withChildren, children.target)?.map((found) => found.id)).toEqual([
    "CORE-1",
    "CORE-2",
  ]);

  const documents = issueAddress({ key: "CORE-1", kind: "artifacts" }, "", "", "spec");
  expect(documents.nodeMenu).toBe(true);
  expect(documents.target).toBeUndefined();
});
