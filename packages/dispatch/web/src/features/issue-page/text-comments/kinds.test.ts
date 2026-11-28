import { beforeAll, describe, expect, test } from "bun:test";
import type { CommentNodeAnchor } from "@legion/contracts";

import type { DisplayTree } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { node } from "../decisions-filter/decisions-fixtures";
import { NODE_TYPE_NAMES } from "../page-words";
import { GATES, type GateName } from "./gates";
import { clearSelection, drawTree, recorded, rowOf, setSelection } from "./margin-fixtures";
import { quoteOf, rangeOf } from "./selection-quote";
import {
  composerAddressOf,
  type Place,
  subjectOf,
  type TextWrite,
  WRITES,
  writeFor,
} from "./text-comment-write";

/**
 * R5: each node kind has one write, or no control with its reason. The write is that of the
 * place the node stands in, as the server refuses an anchor by the document and the quote alone
 * and never by a block's type (INFRA-1253 `api/anchors.go`): a block of a document, whatever its
 * type, anchors in that document; a block written in the room anchors in the issue's `tree`
 * (G3a), a criterion's control waiting for part Y; a part of an ask, a comment and a message write
 * a reply with a quote (G4); a lens, a finding, a risk, a constraint and a result anchor on the
 * node (G2).
 */

// A row draws its words through the shared renderer, whose Markdown engine loads lazily: until
// it has loaded, a row draws no words for `rangeOf` to find. The rows run in random order, so
// the file loads the engine before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The kinds of 1253 R30, which the server's walk names; each is also a `NodeType`. */
const R30_KINDS = [
  "callout",
  "ask",
  "criterion",
  "requirement",
  "check",
  "plan_task",
  "decision",
  "reading",
  "comment",
  "message",
  "issue",
  "lens",
  "risk",
  "constraint",
  "result",
  "finding",
  "prose",
] as const;

/** A write of the table: a kind's request in a place, and the gate it needs. */
function write(place: Place, request: TextWrite["request"], gate: GateName): TextWrite {
  return { gate, place, request };
}

/** A kind that draws no control: its row says so with a reason, whose words are the table's. */
const NONE = "no control" as const;

/** The writes each kind has, by the plan's table ("The four lists"), or no control. */
const EXPECTED: Readonly<Record<string, readonly TextWrite[] | typeof NONE>> = {
  ask: [write("item", "ask_part", "REPLY_QUOTE")],
  callout: [
    write("document", "block", "COMPOSER"),
    write("room", "tree_block", "TREE_ARTIFACT_ANCHOR"),
  ],
  check: [write("document", "block", "COMPOSER")],
  child_issue: NONE,
  comment: [write("item", "comment", "REPLY_QUOTE")],
  constraint: [write("item", "node", "NODE_ANCHOR")],
  criterion: [write("room", "tree_block", "PART_Y")],
  decision: [write("document", "block", "COMPOSER")],
  document: NONE,
  finding: [write("item", "node", "NODE_ANCHOR")],
  handoff: NONE,
  image: NONE,
  issue: NONE,
  lens: [write("item", "node", "NODE_ANCHOR")],
  message: [write("item", "message", "REPLY_QUOTE")],
  plan_task: [write("document", "block", "COMPOSER")],
  prose: [
    write("document", "block", "COMPOSER"),
    write("room", "tree_block", "TREE_ARTIFACT_ANCHOR"),
  ],
  reading: [
    write("document", "block", "COMPOSER"),
    write("room", "tree_block", "TREE_ARTIFACT_ANCHOR"),
  ],
  requirement: [write("document", "block", "COMPOSER")],
  result: [write("item", "node", "NODE_ANCHOR")],
  risk: [write("item", "node", "NODE_ANCHOR")],
};

/** Every kind the page names: each `NodeType` and each kind of 1253 R30. */
const KINDS = [...new Set<string>([...Object.keys(NODE_TYPE_NAMES), ...R30_KINDS])].sort();

/** Every gate on, except the one `off` names where one is given. */
function gatesWith(off?: GateName): Record<GateName, boolean> {
  const all: Record<GateName, boolean> = { ...GATES };
  for (const name of Object.keys(all) as GateName[]) all[name] = name !== off;
  return all;
}

describe("each node kind has one write, or no control with its reason", () => {
  test("a kind with no row in WRITES is a defect", () => {
    const unnamed = KINDS.filter((kind) => EXPECTED[kind] === undefined);
    expect(unnamed, "a kind of the page that this test's table does not name: name it").toEqual([]);
    const missing = KINDS.filter((kind) => WRITES[kind] === undefined);
    expect(missing, "the kinds with no row in WRITES (text-comment-write.ts)").toEqual([]);
  });

  test("each kind's row is the one the plan's table gives its place, and no control has its reason", () => {
    const rows = Object.fromEntries(
      KINDS.map((kind) => {
        const row = WRITES[kind];
        return [kind, row === undefined || row.control ? row?.writes : NONE];
      })
    );
    expect(rows).toEqual(Object.fromEntries(KINDS.map((kind) => [kind, EXPECTED[kind]])));
    const unsaid = KINDS.filter((kind) => {
      const row = WRITES[kind];
      return row?.control === false && row.reason.trim() === "";
    });
    expect(unsaid, "the kinds that draw no control and give no reason").toEqual([]);
  });
});

describe("no control on an author-only node, a child issue or a kind whose gate is off", () => {
  const WITH_CONTROL = KINDS.flatMap((kind) => {
    const row = EXPECTED[kind];
    return row === undefined || row === NONE ? [] : row.map((one) => ({ kind, one }));
  });

  test("a kind's write stands where its gate and the composer's are on", () => {
    const stands = WITH_CONTROL.map(({ kind, one }) =>
      writeFor({ issue: "CORE-1", kind, place: one.place, visibility: "project" }, gatesWith())
    );
    expect(stands).toEqual(WITH_CONTROL.map(({ one }) => ({ ...one, issue: "CORE-1" })));
  });

  test("a kind whose gate is off draws none, and none stands on an author-only node or a child issue", () => {
    // The control that stands with every gate on is the positive the three refusals refuse.
    const standing = writeFor(
      { issue: "CORE-1", kind: "requirement", place: "document", visibility: "project" },
      gatesWith()
    );
    expect(standing, "no write stands for a requirement with every gate on").not.toBeNull();
    for (const { kind, one } of WITH_CONTROL) {
      const subject = { issue: "CORE-1", kind, place: one.place, visibility: "project" } as const;
      expect(
        writeFor(subject, gatesWith(one.gate)),
        `${kind} in ${one.place} draws a control with ${one.gate} off`
      ).toBeNull();
      expect(
        writeFor(subject, gatesWith("COMPOSER")),
        `${kind} in ${one.place} draws a control with COMPOSER off`
      ).toBeNull();
      expect(
        writeFor({ ...subject, visibility: "author" }, gatesWith()),
        `${kind} in ${one.place} draws a control on an author-only node`
      ).toBeNull();
      expect(
        writeFor(subject),
        `${kind} in ${one.place} draws not the control this base's gates give it`
      ).toEqual(GATES.COMPOSER && GATES[one.gate] ? { ...one, issue: "CORE-1" } : null);
    }
    expect(
      writeFor(
        { issue: "CORE-1", kind: "child_issue", place: "item", visibility: "project" },
        gatesWith()
      ),
      "a child issue's row draws a control"
    ).toBeNull();
  });
});

// R5: a node of another issue takes the control and writes on its own issue's route, from the
// answer of the tree read that draws it (G3c).
test.skipIf(!GATES.OTHER_ISSUE_COMMENTS)(
  "a node of another issue writes on its own issue's route",
  () => {
    const write = writeFor(
      { issue: "OTHER-2", kind: "requirement", place: "document", visibility: "project" },
      gatesWith()
    );
    expect(write?.issue, "the write goes to the page's own issue").toBe("OTHER-2");
  }
);

// R5 (G2): with `NODE_ANCHOR` on, a lens, a finding (whose tree-read supertag is `block` under a
// lens), a risk, a constraint and a result take the control and build `anchor {node, quote,
// occurrence}` naming each node by the store address `POST /api/v1/issues/{key}/comments` takes.
test("a lens, finding, risk, constraint and result take the control and address their node", () => {
  const cases = [
    {
      node: { id: "lens-uuid", kind: "lens", supertag: "lens", visibility: "project" as const },
      want: "lens/lens-uuid",
    },
    {
      node: {
        id: "lens/lens-uuid#f1",
        kind: "block",
        parent: { first_line: "Verdict", id: "lens-uuid", kind: "lens", supertag: "lens" },
        supertag: "block",
        visibility: "project" as const,
      },
      want: "lens/lens-uuid#f1",
    },
    {
      node: { id: "risk-uuid", kind: "risk", supertag: "risk", visibility: "project" as const },
      want: "risk/risk-uuid",
    },
    {
      node: {
        id: "constraint-uuid",
        kind: "constraint",
        supertag: "constraint",
        visibility: "project" as const,
      },
      want: "constraint/constraint-uuid",
    },
    {
      node: {
        id: "result-uuid",
        kind: "result",
        supertag: "result",
        visibility: "project" as const,
      },
      want: "result/result-uuid",
    },
  ];
  for (const { node, want } of cases) {
    const write = writeFor(subjectOf(node, "CORE-1"));
    expect(write, `no live write stands for ${want}`).toEqual({
      gate: "NODE_ANCHOR",
      issue: "CORE-1",
      place: "item",
      request: "node",
    });
    if (write === null) continue;
    expect(
      composerAddressOf(write, node, { block: null, occurrence: 1, quote: "words" }),
      `the composer address for ${want}`
    ).toEqual({
      anchor: { node: want, occurrence: 1, quote: "words" },
      replyTo: null,
    });
  }
});

// R5 (G3a): a block written in the conversation - a paragraph, which the tree read answers with
// the supertag `block` and no document, or a typed block of the room - takes the control and
// anchors in the issue's `tree` artifact by its block, and a paragraph of a document anchors in
// that document; a criterion, also written into the issue, takes none until part Y is on the base.
test("a block written in the room anchors in the issue's tree, and a criterion waits for part Y", () => {
  const words = { block: null, occurrence: 1, quote: "note" } as const;
  const paragraph = {
    block_id: "p1",
    id: "CORE-1/tree#p1",
    supertag: "block",
    visibility: "project" as const,
  };
  const write = writeFor(subjectOf(paragraph, "CORE-1"));
  expect(write, "the room's paragraph takes the room's write").toEqual({
    gate: "TREE_ARTIFACT_ANCHOR",
    issue: "CORE-1",
    place: "room",
    request: "tree_block",
  });
  if (write !== null) {
    expect(
      composerAddressOf(write, { ...paragraph, tree: "tree-1" }, words),
      "the room's paragraph anchors in the issue's tree by its block"
    ).toEqual({
      anchor: { artifact: "tree-1", block_id: "p1", occurrence: 1, quote: "note" },
      replyTo: null,
    });
  }
  const prose = {
    block_id: "p2",
    document: { artifact_id: "spec-1", position: 3, slug: "spec" },
    id: "CORE-1/spec#p2",
    supertag: "block",
    visibility: "project" as const,
  };
  expect(writeFor(subjectOf(prose, "CORE-1")), "a paragraph of a document").toEqual({
    gate: "COMPOSER",
    issue: "CORE-1",
    place: "document",
    request: "block",
  });
  const criterion = {
    block_id: "k1",
    id: "CORE-1/tree#k1",
    supertag: "criterion",
    visibility: "project" as const,
  };
  expect(
    writeFor(subjectOf(criterion, "CORE-1")),
    "a criterion takes the control only once part Y is on the base"
  ).toEqual(
    GATES.PART_Y ? { gate: "PART_Y", issue: "CORE-1", place: "room", request: "tree_block" } : null
  );
});

interface RecordedNodeQuote {
  readonly item: string;
  readonly text: string;
  readonly text_tree: DisplayTree;
  readonly writes: readonly {
    readonly answer: { readonly status: number; readonly node_anchor?: CommentNodeAnchor };
    readonly selection: { readonly quote: string; readonly occurrence: number };
  }[];
}

const NODE_QUOTES = recorded<{ nodeCases?: readonly RecordedNodeQuote[] }>("quotes.json").nodeCases;
const NEEDS_G2_RECORDING = NODE_QUOTES === undefined;

for (const kind of ["lens", "risk"] as const) {
  test.skipIf(NEEDS_G2_RECORDING)(`the server takes the ${kind}'s quote at each occurrence`, () => {
    const fixture = NODE_QUOTES?.find(({ item }) => item === `${kind}-anchor`);
    if (fixture === undefined) throw new Error(`the G2 recording holds no ${kind} quote`);
    const view = drawTree([
      node({ id: kind, text: fixture.text, tree: fixture.text_tree, type: kind }),
    ]);
    try {
      const row = rowOf(view, kind);
      if (row === null) throw new Error(`the ${kind} draws no row`);
      const source = { text: fixture.text, text_tree: fixture.text_tree };
      expect(fixture.writes.map(({ selection }) => selection.occurrence)).toEqual([0, 1]);
      for (const { answer, selection } of fixture.writes) {
        const what = `${kind} occurrence ${selection.occurrence}`;
        expect(answer.status, `${what}: the server did not take the quote`).toBe(201);
        expect(answer.node_anchor, `${what}: the read did not keep the quote`).toMatchObject({
          node: { kind },
          occurrence: selection.occurrence,
          quote: selection.quote,
        });
        const range = rangeOf(row, selection.quote, selection.occurrence, { node: source });
        if (range === null) throw new Error(`${what}: the quoted words are not drawn`);
        setSelection(range);
        const selected = window.getSelection();
        if (selected === null) throw new Error("the window has no selection");
        expect(quoteOf(selected, row, source), what).toEqual({
          block: null,
          occurrence: selection.occurrence,
          quote: selection.quote,
        });
      }
    } finally {
      clearSelection();
      view.unmount();
      view.client.clear();
    }
  });
}
