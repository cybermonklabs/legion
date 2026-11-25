import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { ANSWER_LINES } from "../page-words";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import fixture from "./__fixtures__/graph-edit.json";
import {
  ENTRIES,
  type Entry,
  type PlacementInput,
  placementOf,
  placingOf,
  RETYPE_KINDS,
  type Verdict,
} from "./placement";

/**
 * Which entry of the node menu is grey, and the body of each write that is not: `graph-edit/placement.ts`
 * (R2 to R5 of INFRA-5705's spec, its Design's "Which entry is grey").
 *
 * The interface this file expects of `placement.ts` (the task that writes it reads it here). It is
 * a pure module: it reads the held tree read and nothing else, holds no rule only the server
 * decides (whether a parent is in the walk, whether a move empties a container, whether a delete
 * is possible), and imports no React.
 *
 * - `ENTRIES: readonly Entry[]`, R1's nine entries in R1's order: `new_below`, `new_under`,
 *   `retype`, `indent`, `outdent`, `move_up`, `move_down`, `delete`, `lens`. The word of an entry is
 *   `CONTROL_LABELS["edit_" + entry]` of `page-words-r.ts`; Lens's is part W's `LENS_WORDS.act`.
 * - `RETYPE_KINDS: readonly RetypeKind[]`, the four kinds Retype offers in order: `criterion`,
 *   `statement` (the tree read's supertag `requirement`), `check`, `plan_task`.
 * - `placementOf(input: PlacementInput): Placement`, with `Placement = Record<Entry, Verdict>`:
 *   - `input.held: TreeAnswer`, the outline read the page holds (Graph's, every page it has loaded
 *     merged into it; a `cursor` that is not null says that pages remain);
 *   - `input.node: string`, the node's read id (`TreeAnswerNode.id`);
 *   - `input.status: string`, the issue's status; `"done"` closes the issue;
 *   - `input.row?: "own" | { under: string }`, the row the menu opens from: the node's own row
 *     (the default), or its second row under the node of that read id that it `serves`;
 *   - `input.zoomRoot?: string`, the read id of the node Graph is zoomed to;
 *   - `input.kinds?: readonly RetypeKind[]`, the kinds the editor's schema for the fragment lets
 *     the editor make (the default is `RETYPE_KINDS`; a child that is a view of its ancestor's
 *     spec gives the schema in which `requirement` may not be made, so no `statement`);
 *   - `input.unplaced?: true`, a new row the reader typed in that no `PATCH` has placed yet.
 *   A node id the held read lacks is a bug of the caller: `placementOf` throws.
 * - A `Verdict` is one of
 *   - `{ kind: "grey", reason }`, `reason` a key of `ANSWER_LINES` of `page-words-r.ts` that starts
 *     `grey_`;
 *   - `{ kind: "move", body }` for Indent, Outdent, Move up and Move down: `body` is the `PATCH`
 *     body `{ parent, after? }`. A node is named by its route id: a block by its read id, any
 *     other node by `<kind>/<read id>`, so the issue is `issue/<key>`. Outdent has no `after`
 *     where the old parent is not a block of the node's own document, and the others name
 *     `after: null` where the node becomes the first of its document under that parent;
 *   - `{ kind: "read_next" }` where the body names a sibling or a last child past the loaded page
 *     (the held read's `cursor`): the caller reads the next page (`readNextTreePage`) and asks
 *     again;
 *   - `{ kind: "new", place, line? }` for New node below and New node under: `place.body` is the
 *     `PATCH` that gives the new block its place once it holds a rank and a `placement_seq`,
 *     `place.after` the read id of the row the new row is drawn right after under
 *     `place.body.parent`, null where it is that parent's first child; `line: "spec_band"` where
 *     the new row stands after the spec's blocks and not right below the focused one (a key of
 *     `ANSWER_LINES`);
 *   - `{ kind: "retype", kinds }` for Retype on a paragraph: `kinds` has one member for each kind
 *     of `input.kinds`, in `RETYPE_KINDS`' order: `{ kind: "grey", reason }`, or
 *     `{ kind: "to", attributes }` where `attributes` are the new block's `parent` or `serves`,
 *     as the `dispatch://` address of the node that the node stands under;
 *   - `{ kind: "ask" }`: the server decides (Delete: the dry run);
 *   - `{ kind: "open" }`: W's own act (Lens), which this module never greys.
 * - `placingOf({ held, node, place }): Placing`, the new node's `PATCH` (R3): `{ kind: "wait" }`
 *   until `held` holds the node with a `rank` and a `placement_seq`, then
 *   `{ kind: "send", body: place.body, placementSeq }`, `place` the `NewRowPlace` of the entry
 *   that made the row.
 */

const STATUS = "in_progress";

function answer(body: unknown): TreeAnswer {
  return body as TreeAnswer;
}

const converted = answer(fixture.tree.converted.person.body);
const done = answer(fixture.tree.done.person.body);
const child = answer(fixture.tree.child.person.body);
const owed = answer(fixture.owed.owed.body);
const indexed = answer(fixture.owed.indexed.body);
// The Graph read cut at its first page: the walk's page size of the fixture's `paged` member.
const firstPage = answer(fixture.paged.pages[0]?.body);
// The child's read of its spec view, the statements of the issue above (a member a later regeneration
// of the fixture gives).
const trees: Readonly<Record<string, { person: { body: unknown } } | undefined>> = fixture.tree;
const childSpec = trees.child_spec === undefined ? undefined : answer(trees.child_spec.person.body);

function nodeOf(held: TreeAnswer, id: string): TreeAnswerNode {
  const found = held.nodes?.find((each) => each.id === id);
  if (found === undefined) throw new Error(`the fixture's read holds no node ${id}`);
  return found;
}

/** The nine verdicts for `node` of `held`, as the page asks them for the node's own row. */
function place(node: string, extra: Partial<PlacementInput> = {}, held: TreeAnswer = converted) {
  return placementOf({ held, node, status: STATUS, ...extra });
}

const EDITING: readonly Entry[] = ENTRIES.filter((entry) => entry !== "lens");

/** A grey verdict, whose reason must be a key of the page's words. */
function grey(reason: string): Verdict {
  expect(Object.hasOwn(ANSWER_LINES, reason), `${reason} is no key of ANSWER_LINES`).toBe(true);
  return { kind: "grey", reason } as Verdict;
}

/** The kinds of node the tree read tells apart (`api/issue_tree_kinds.go`) and the typed blocks of the
 *  schema (`pmdoc/schema/blocks.json`), read from those two files so a kind added to either is one
 *  this file has to name. */
const BLOCKS = new URL(
  "../../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json",
  import.meta.url
);
const KINDS_GO = new URL(
  "../../../../../../envoy/internal/dispatch/api/issue_tree_kinds.go",
  import.meta.url
);

const TYPED = new Set(
  (JSON.parse(readFileSync(BLOCKS, "utf8")) as { types: { name: string }[] }).types.map(
    (type) => type.name
  )
);

/** Every kind of `treeKinds`: each typed block of the schema, the three kinds the read works out
 *  itself (`treeKindFinding`, `treeKindProse`, `treeKindMemo`) and the supertags of
 *  `treeSupertagKinds`. */
function kindsOfTheServer(): string[] {
  const go = readFileSync(KINDS_GO, "utf8");
  const own = [...go.matchAll(/^\s*treeKind\w+\s*=\s*"(\w+)"/gmu)].map((one) => one[1] ?? "");
  const listed = /var treeSupertagKinds = \[\]string\{([^}]*)\}/u.exec(go)?.[1] ?? "";
  const supertags = [...listed.matchAll(/graphstore\.Kind(\w+)/gu)].map((one) =>
    (one[1] ?? "").toLowerCase()
  );
  return [...new Set([...TYPED, ...own, ...supertags])].sort();
}

/** The kind a node has in the sense of `kind=` of the tree read: a typed block by its supertag, a
 *  finding (a block under a lens), prose (any other block), and any other node by its own kind. */
function kindOf(node: TreeAnswerNode): string {
  if (node.kind !== "block") return node.kind;
  if (TYPED.has(node.supertag)) return node.supertag;
  return node.parent?.kind === "lens" ? "finding" : "prose";
}

/** What R2 and R3 say of each kind: whether the six entries act on it, and whether New node under
 *  is offered on it. */
const R2: Readonly<Record<string, { admitted: boolean; under: boolean }>> = {
  ask: { admitted: false, under: true },
  callout: { admitted: false, under: false },
  check: { admitted: true, under: true },
  comment: { admitted: false, under: true },
  constraint: { admitted: false, under: false },
  criterion: { admitted: true, under: true },
  decision: { admitted: false, under: false },
  finding: { admitted: false, under: false },
  issue: { admitted: false, under: false },
  lens: { admitted: false, under: false },
  message: { admitted: false, under: true },
  plan_task: { admitted: true, under: true },
  prose: { admitted: true, under: true },
  reading: { admitted: false, under: false },
  requirement: { admitted: true, under: true },
  result: { admitted: false, under: false },
  risk: { admitted: false, under: false },
};

const R2_REASONS = ["grey_kind", "grey_own_issue", "grey_closed"];

// The inventory of R2: each kind the server tells apart, against each of the nine entries. A kind
// the files add and this table does not name fails the first line; a kind no node of the fixture
// is of fails the loop.
test("every node kind takes or greys each editing entry as R2 says", () => {
  // R1's nine entries in R1's order, and the four kinds Retype offers.
  expect(ENTRIES).toEqual([
    "new_below",
    "new_under",
    "retype",
    "indent",
    "outdent",
    "move_up",
    "move_down",
    "delete",
    "lens",
  ]);
  expect(RETYPE_KINDS).toEqual(["criterion", "statement", "check", "plan_task"]);
  expect(Object.keys(place("<statement-3>"))).toEqual([...ENTRIES]);
  expect(Object.keys(R2).sort()).toEqual(kindsOfTheServer());
  const own = (converted.nodes ?? []).filter(
    (node) => node.id !== converted.issue && (node.issue ?? converted.issue) === converted.issue
  );
  for (const [kind, rule] of Object.entries(R2)) {
    const nodes = own.filter((node) => kindOf(node) === kind);
    expect(nodes.length, `the fixture holds no node of kind ${kind}`).toBeGreaterThan(0);
    for (const node of nodes) {
      const verdicts = place(node.id);
      const named = `${kind} ${node.id}`;
      expect(verdicts.lens, `Lens on ${named}`).toEqual({ kind: "open" });
      for (const entry of [
        "retype",
        "indent",
        "outdent",
        "move_up",
        "move_down",
        "delete",
      ] as const) {
        const verdict = verdicts[entry];
        if (rule.admitted) {
          const reason = verdict.kind === "grey" ? verdict.reason : undefined;
          expect(R2_REASONS, `${entry} on ${named} is grey as R2 does not admit it`).not.toContain(
            reason
          );
        } else {
          expect(verdict, `${entry} on ${named}`).toEqual(grey("grey_kind"));
        }
      }
      if (rule.admitted) {
        const reason = verdicts.new_below.kind === "grey" ? verdicts.new_below.reason : undefined;
        expect(R2_REASONS, `new_below on ${named}`).not.toContain(reason);
      } else {
        expect(verdicts.new_below, `new_below on ${named}`).toEqual(grey("grey_kind"));
      }
      if (rule.under) {
        expect(verdicts.new_under.kind, `new_under on ${named}`).toBe("new");
      } else {
        expect(verdicts.new_under, `new_under on ${named}`).toEqual(grey("grey_kind"));
      }
    }
  }
});

test("a node R2 does not admit greys every editing entry", () => {
  const others = [
    "<question>",
    "<comment-on-statement-3>",
    "<message>",
    "<result>",
    "<decision>",
    "<reading>",
    "<lens>",
    "<finding>",
    "<risk>",
    "<constraint>",
    "<memo>",
    "<child>",
  ];
  for (const id of others) {
    const verdicts = place(id);
    for (const entry of EDITING) {
      // New node under stays offered on an ask, a comment and a message (R3).
      if (
        entry === "new_under" &&
        ["<question>", "<comment-on-statement-3>", "<message>"].includes(id)
      ) {
        expect(verdicts[entry].kind, `${entry} on ${id}`).toBe("new");
        continue;
      }
      expect(verdicts[entry], `${entry} on ${id}`).toEqual(grey("grey_kind"));
    }
    expect(verdicts.lens, `Lens on ${id}`).toEqual({ kind: "open" });
  }
  // Every other node the read holds, the context among them (a person, a session, a kind of
  // lens, the blocks of an ask, a comment and a message).
  for (const node of converted.nodes ?? []) {
    if (node.id === converted.issue || kindOf(node) in R2) continue;
    for (const entry of EDITING) {
      expect(place(node.id)[entry], `${entry} on ${node.kind} ${node.id}`).toEqual(
        grey("grey_kind")
      );
    }
  }
});

test("a node of another issue greys its entries as edited on its own issue", () => {
  // A check of a child issue under this issue's statement, read from this issue.
  const check = nodeOf(converted, "<child-check>");
  expect(check.issue, "the fixture's check belongs to the child").toBe("<child>");
  for (const entry of EDITING) {
    expect(place("<child-check>")[entry], `${entry} on the child's check`).toEqual(
      grey("grey_own_issue")
    );
  }
  // The check of this issue under the same statement is edited here.
  expect(place("<check-shared>").delete).toEqual({ kind: "ask" });
  // A statement of the issue above, read from the child that is a view of its spec.
  expect(child.view_of).toBe("<issue>");
  const above = (childSpec?.nodes ?? []).filter(
    (node) => node.issue !== undefined && node.issue !== null && node.issue !== childSpec?.issue
  );
  expect(above.length, "the child's spec read holds no node of the issue above").toBeGreaterThan(0);
  for (const node of above) {
    for (const entry of EDITING) {
      expect(
        place(node.id, {}, childSpec as TreeAnswer)[entry],
        `${entry} on ${node.id} of the issue above`
      ).toEqual(grey("grey_own_issue"));
    }
  }
});

test("a closed issue greys every editing entry", () => {
  expect(done.converted).toBe(true);
  for (const id of ["<done-criterion>", "<done-statement>"]) {
    const closed = placementOf({ held: done, node: id, status: "done" });
    for (const entry of EDITING) {
      expect(closed[entry], `${entry} on ${id}`).toEqual(grey("grey_closed"));
    }
    expect(closed.lens).toEqual({ kind: "open" });
    // The same node of an issue that is not closed takes its entries.
    const open = placementOf({ held: done, node: id, status: STATUS });
    expect(open.new_under.kind).toBe("new");
    expect(open.delete).toEqual({ kind: "ask" });
  }
});

test("an agent's node takes every entry its kind takes", () => {
  const agent = nodeOf(converted, "<statement-2>");
  expect(agent.author?.[0]?.kind, "the fixture's second statement is an agent's").toBe("session");
  const verdicts = place("<statement-2>");
  expect(verdicts.new_below.kind).toBe("new");
  expect(verdicts.new_under.kind).toBe("new");
  expect(verdicts.retype).toEqual(grey("grey_not_paragraph"));
  for (const entry of ["indent", "outdent", "move_up", "move_down"] as const) {
    expect(verdicts[entry].kind, entry).toBe("move");
  }
  expect(verdicts.delete).toEqual({ kind: "ask" });
  // The agent's check of the Verification document too.
  expect(nodeOf(converted, "<check-agent>").author?.[0]?.kind).toBe("session");
  expect(place("<check-agent>").delete).toEqual({ kind: "ask" });
});

test("a second row under another serves has its moves grey", () => {
  // The shared check serves all three statements and stands under the first: Graph draws it a
  // second time under each of the others (part T's second row).
  expect(
    converted.tuples?.some(
      (tuple) =>
        tuple.kind === "serves" &&
        tuple.from.id === "<check-shared>" &&
        tuple.to.id === "<statement-2>"
    )
  ).toBe(true);
  const second = place("<check-shared>", { row: { under: "<statement-2>" } });
  for (const entry of ["indent", "outdent", "move_up", "move_down"] as const) {
    expect(second[entry], entry).toEqual(grey("grey_own_row"));
  }
  expect(second.delete).toEqual({ kind: "ask" });
  // On its own row the same moves are the node's own, grey or not by its place.
  const own = place("<check-shared>");
  expect(own.outdent.kind).toBe("move");
  expect(own.indent).toEqual(grey("grey_no_sibling_above"));
});

test("an indent names the stored sibling above as parent and its last ranked child of the node's document as after", () => {
  // The server's own indent of the third statement under the second, which has no child of the
  // spec: after is null.
  const request = fixture.moved.indent.request.body;
  expect(place("<statement-3>").indent).toStrictEqual({ body: request, kind: "move" });
  // Where the sibling above holds children of the node's own document, the last of them is after
  // (a table after a list of the issue's own fragment); the children of another document are
  // none of it (a paragraph after a decision, whose one child is of the spec).
  expect(place("<tree/table>").indent).toStrictEqual({
    body: { after: "<tree/bullet_list/paragraph>", parent: "<tree/bullet_list>" },
    kind: "move",
  });
  expect(nodeOf(converted, "<reading>").parent?.id).toBe("<decision>");
  expect(place("<tree/paragraph>").indent).toStrictEqual({
    body: { after: null, parent: "<decision>" },
    kind: "move",
  });
});

// No real read makes an Indent ask for the next page: the walk puts a sibling's whole subtree
// before the node, so a node held after its sibling above holds that sibling's children. This
// case holds the rule on the first page of the fixture's `paged` read, which ends inside the first
// statement's check: an Indent whose sibling above closes before the node is answered from the
// page alone, and the Moves of a node that stands where the page ends wait for the page after it.
test("an indent past the loaded page reads the next pages first", () => {
  expect(firstPage.cursor, "the first page names the next").not.toBeNull();
  const cut = (node: string) => placementOf({ held: firstPage, node, status: STATUS });
  expect(cut("<tree/code_block>").indent).toStrictEqual({
    body: { after: null, parent: "<tree/table>" },
    kind: "move",
  });
  expect(cut("<statement-1>").move_down).toEqual({ kind: "read_next" });
  // With the whole read held nothing is left to read.
  expect(place("<statement-1>").move_down.kind).toBe("move");
});

test("an outdent names the old parent as after only within its document", () => {
  // A paragraph of the list, both of the issue's own fragment: the list is after.
  expect(place("<tree/bullet_list/paragraph>").outdent).toStrictEqual({
    body: { after: "<tree/bullet_list>", parent: "issue/<issue>" },
    kind: "move",
  });
  // A check of the Verification document under a statement of the spec: no after.
  expect(nodeOf(converted, "<check-shared>").parent?.id).toBe("<statement-1>");
  expect(place("<check-shared>").outdent).toStrictEqual({
    body: { parent: "<criterion>" },
    kind: "move",
  });
});

test("a move names the ranked sibling of the same document that stands before the node", () => {
  // The server's own move up of the third statement: before the second, so after the first.
  expect(place("<statement-3>").move_up).toStrictEqual({
    body: fixture.moved.move_up.request.body,
    kind: "move",
  });
  expect(place("<statement-2>").move_up).toStrictEqual({
    body: { after: null, parent: "<criterion>" },
    kind: "move",
  });
  // A move down puts the node after the sibling below it.
  expect(place("<statement-1>").move_down).toStrictEqual({
    body: { after: "<statement-2>", parent: "<criterion>" },
    kind: "move",
  });
  expect(place("<statement-2>").move_down).toStrictEqual({
    body: { after: "<statement-3>", parent: "<criterion>" },
    kind: "move",
  });
  // A sibling of another document is no sibling: three statements stand above the paragraph of
  // the issue's own fragment under the criterion, and none is of its document.
  expect(place("<prose-under-criterion>").move_up).toEqual(grey("grey_first"));
  expect(place("<statement-3>").move_down).toEqual(grey("grey_last"));
});

// The first page of the fixture's `paged` read ends in the first statement's children: the
// siblings below the first statement, and below the check, may be on the next page.
test("a move past the loaded page reads the next page first", () => {
  const cut = (node: string) => placementOf({ held: firstPage, node, status: STATUS });
  expect(firstPage.cursor).not.toBeNull();
  expect(nodeOf(firstPage, "<statement-1>").id).toBe("<statement-1>");
  expect(firstPage.nodes?.some((node) => node.id === "<statement-2>")).toBe(false);
  expect(cut("<statement-1>").move_down).toEqual({ kind: "read_next" });
  expect(cut("<check-shared>").move_down).toEqual({ kind: "read_next" });
  expect(cut("<criterion>").move_down).toEqual({ kind: "read_next" });
  // The pages together answer it.
  expect(place("<statement-1>").move_down.kind).toBe("move");
  expect(place("<check-shared>").move_down).toEqual(grey("grey_last"));
});

test("indent, outdent and the two moves are grey at their edges, each with its reason", () => {
  const first = place("<statement-1>");
  expect(first.indent).toEqual(grey("grey_no_sibling_above"));
  expect(first.move_up).toEqual(grey("grey_first"));
  const last = place("<statement-3>");
  expect(last.move_down).toEqual(grey("grey_last"));
  // Outdent is grey on a node directly under the issue.
  expect(nodeOf(converted, "<tree/paragraph>").parent?.id).toBe(converted.issue);
  expect(place("<tree/paragraph>").outdent).toEqual(grey("grey_directly_under_issue"));
  // The edges are the node's, not the entry's: the middle statement has every move.
  const middle = place("<statement-2>");
  for (const entry of ["indent", "outdent", "move_up", "move_down"] as const) {
    expect(middle[entry].kind, entry).toBe("move");
  }
});

test("outdent is grey on a child of the zoomed node", () => {
  expect(place("<statement-1>", { zoomRoot: "<criterion>" }).outdent).toEqual(
    grey("grey_zoom_root")
  );
  // A node deeper than the zoomed node's children outdents as ever.
  expect(place("<check-shared>", { zoomRoot: "<criterion>" }).outdent.kind).toBe("move");
  expect(place("<statement-1>").outdent.kind).toBe("move");
});

// The tree read of the issue's own fragment never returns a block that still owes its index
// (`owed_documents` names the tree artifact until the index runs, and the block the room wrote is
// not in the read at all): the fixture's `owed` member is that read, and the read after the
// index. Its requirement, typed at once, is read with `rank: null` and a `placement_seq`.
test("a node with no rank has every move grey", () => {
  const unranked = nodeOf(owed, "<owed-requirement>");
  expect(unranked.rank).toBeNull();
  expect(unranked.placement_seq).not.toBeNull();
  const verdicts = placementOf({ held: owed, node: "<owed-requirement>", status: STATUS });
  for (const entry of ["indent", "outdent", "move_up", "move_down"] as const) {
    expect(verdicts[entry], entry).toEqual(grey("grey_no_rank"));
  }
  // Once the index ran the same node holds its rank and its moves are its place's.
  expect(nodeOf(indexed, "<owed-requirement>").rank).not.toBeNull();
  const ranked = placementOf({ held: indexed, node: "<owed-requirement>", status: STATUS });
  expect(ranked.indent.kind).toBe("move");
  expect(ranked.move_up.kind).toBe("move");
  expect(ranked.move_down).toEqual(grey("grey_last"));
});

test("new node below on a spec block stands after the spec's blocks and says so", () => {
  // The first statement is not the last block of the spec under the criterion: the new row
  // stands after the third, whose own-fragment band follows, and the row says it.
  expect(nodeOf(converted, "<statement-1>").document?.slug).toBe("spec");
  expect(place("<statement-1>").new_below).toStrictEqual({
    kind: "new",
    line: "spec_band",
    place: { after: "<statement-3>", body: { parent: "<criterion>" } },
  });
  expect(Object.hasOwn(ANSWER_LINES, "spec_band")).toBe(true);
  // The last statement is right above that place: nothing to say.
  expect(place("<statement-3>").new_below).toStrictEqual({
    kind: "new",
    place: { after: "<statement-3>", body: { parent: "<criterion>" } },
  });
});

test("new node below is grey on a block of the Verification document or the plan", () => {
  for (const id of ["<check-shared>", "<check-agent>", "<plan-task>"]) {
    expect(nodeOf(converted, id).document?.slug, id).toMatch(/^(verification-md|plan-md)$/);
    expect(place(id).new_below, id).toEqual(grey("grey_below_document"));
  }
});

test("new node below on a block of the issue's own fragment stands right after it", () => {
  expect(nodeOf(converted, "<prose-under-criterion>").document).toBeUndefined();
  expect(place("<prose-under-criterion>").new_below).toStrictEqual({
    kind: "new",
    place: {
      after: "<prose-under-criterion>",
      body: { after: "<prose-under-criterion>", parent: "<criterion>" },
    },
  });
  expect(place("<tree/heading>").new_below).toStrictEqual({
    kind: "new",
    place: {
      after: "<tree/heading>",
      body: { after: "<tree/heading>", parent: "issue/<issue>" },
    },
  });
});

test("new node under stands first among the children of the issue's own fragment", () => {
  // The third statement has no child of the spec: the new block is its first child.
  expect(place("<statement-3>").new_under).toStrictEqual({
    kind: "new",
    place: { after: null, body: { after: null, parent: "<statement-3>" } },
  });
  // The criterion has three children of the spec, which stand before its own-fragment children:
  // the new row is drawn after the last of them, and is the first of the issue's own fragment.
  expect(place("<criterion>").new_under).toStrictEqual({
    kind: "new",
    place: { after: "<statement-3>", body: { after: null, parent: "<criterion>" } },
  });
});

test("a new row's moves are grey until the node holds a rank and a placement_seq", () => {
  // The block the page wrote into the tree artifact is not in the read until the index runs
  // (`owed`), and the read after the index holds it with a rank and a placement_seq (`indexed`).
  expect(owed.nodes?.some((node) => node.id === "<owed-paragraph>")).toBe(false);
  const written = nodeOf(indexed, "<owed-paragraph>");
  expect(written.rank).not.toBeNull();
  expect(written.placement_seq).not.toBeNull();
  for (const held of [owed, indexed]) {
    const verdicts = placementOf({
      held,
      node: "<owed-requirement>",
      status: STATUS,
      unplaced: true,
    });
    for (const entry of ["new_below", "indent", "outdent", "move_up", "move_down"] as const) {
      expect(verdicts[entry], entry).toEqual(grey("grey_not_placed"));
    }
  }
  // Placed, the same node's moves are no longer grey as unplaced.
  const placed = placementOf({ held: indexed, node: "<owed-paragraph>", status: STATUS });
  for (const entry of ["indent", "outdent", "move_up", "move_down"] as const) {
    const verdict = placed[entry];
    expect(verdict.kind === "grey" ? verdict.reason : "", entry).not.toBe("grey_not_placed");
  }
});

test("the new node's PATCH waits until the node holds a rank", () => {
  const under = place("<criterion>").new_under;
  if (under.kind !== "new") throw new Error("New node under is not offered on the criterion");
  // The tree artifact owes its index: the read does not hold the paragraph.
  expect(placingOf({ held: owed, node: "<owed-paragraph>", place: under.place })).toEqual({
    kind: "wait",
  });
  // A node that is read with no rank is waited for too.
  expect(placingOf({ held: owed, node: "<owed-requirement>", place: under.place })).toEqual({
    kind: "wait",
  });
  // The read after the index holds the paragraph with its rank and its placement_seq: one body.
  const written = nodeOf(indexed, "<owed-paragraph>");
  expect(placingOf({ held: indexed, node: "<owed-paragraph>", place: under.place })).toStrictEqual({
    body: under.place.body,
    kind: "send",
    placementSeq: written.placement_seq,
  });
});

test("retype is offered on a prose block that is one paragraph, and is grey on a prose heading, list, table or code block", () => {
  expect(place("<tree/paragraph>").retype.kind).toBe("retype");
  expect(place("<prose-under-criterion>").retype.kind).toBe("retype");
  for (const id of ["<tree/heading>", "<tree/bullet_list>", "<tree/table>", "<tree/code_block>"]) {
    expect(place(id).retype, id).toEqual(grey("grey_not_paragraph"));
  }
  // A statement is no paragraph, and says so; a node R2 does not admit says it is not edited here.
  expect(place("<statement-1>").retype).toEqual(grey("grey_not_paragraph"));
  expect(place("<check-shared>").retype).toEqual(grey("grey_not_paragraph"));
  expect(place("<decision>").retype).toEqual(grey("grey_kind"));
});

test("retype keeps the node's place in its parent and serves attributes and greys a kind its place cannot give", () => {
  // The paragraph under the criterion: a statement there serves the criterion; a criterion needs
  // an ask, a comment or a message above it; a check and a plan task need a statement.
  expect(place("<prose-under-criterion>").retype).toStrictEqual({
    kind: "retype",
    kinds: {
      criterion: grey("grey_criterion_parent"),
      statement: { attributes: { serves: "dispatch://<issue>/spec#b-k1" }, kind: "to" },
      check: grey("grey_check_parent"),
      plan_task: grey("grey_plan_task_parent"),
    },
  });
  // The paragraph under the issue itself stands under none of them.
  expect(place("<tree/paragraph>").retype).toStrictEqual({
    kind: "retype",
    kinds: {
      criterion: grey("grey_criterion_parent"),
      statement: grey("grey_statement_parent"),
      check: grey("grey_check_parent"),
      plan_task: grey("grey_plan_task_parent"),
    },
  });
});

test("retype offers the kinds the editor's schema lets it make, so a view offers no statement", () => {
  // The kinds the editor of a child that is a view of its ancestor's spec lets it make: the
  // schema in which `requirement` may not be made.
  expect(child.view_of, "the fixture's child is a view").toBe("<issue>");
  const onView = RETYPE_KINDS.filter((kind) => kind !== "statement");
  const view = place("<prose-under-criterion>", { kinds: onView }).retype;
  if (view.kind !== "retype") throw new Error("Retype is not offered on the paragraph");
  expect(Object.keys(view.kinds)).toEqual(["criterion", "check", "plan_task"]);
  expect(view.kinds.criterion).toEqual(grey("grey_criterion_parent"));
  // Every kind where the editor makes them all.
  const all = place("<prose-under-criterion>").retype;
  if (all.kind !== "retype") throw new Error("Retype is not offered on the paragraph");
  expect(Object.keys(all.kinds)).toEqual([...RETYPE_KINDS]);
});
