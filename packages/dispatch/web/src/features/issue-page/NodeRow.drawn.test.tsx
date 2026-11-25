import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";
import { MemoryRouter, useLocation, useNavigationType } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { api } from "../../api/client";
import type { Actor, Agent, Decision, DisplayTree, IssueChild } from "../../api/types";
import { pageKindBullet, pageKindTag } from "../../theme/classes";
import type { ReplyTarget } from "../conversation/composer-model";
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { actorLabelText, useAgentText } from "../refs/AgentName";
import { agentsQuery } from "../refs/agent-registry";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { PostDeletion } from "../refs/post-marks";
import { measureNearWords } from "./clamp-fixtures";
import {
  AGENT,
  decision,
  decisionsRead,
  issueDetails,
  KEY,
  SPEC_DOCUMENT,
  SPEC_SLUG,
} from "./decisions-filter/decisions-fixtures";
import { buildIssueTree, type NodeChip, type ParentLine, type TreeNode } from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { KeepOpen, NodeOptions, NodeRow, useRowOnScreen } from "./NodeRow";
import { NoTooltips } from "./NoTooltips";
import * as nodeRowChip from "./node-row-chip";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";
import { AUTHOR_TEXT_ATTRIBUTE, PAGE_CHROME_ATTRIBUTE } from "./page-structure";
import {
  type AuthorKind,
  authorKind,
  BLOCK_TYPE_NAMES,
  CONTROL_LABELS,
  DECLARED_WORDS,
  type GapKind,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  isReader,
  NODE_TYPE_NAMES,
  type NodeType,
  wordsIn,
} from "./page-words";
import { RowText } from "./RowText";
import { ReferenceNamesProvider, usePublishReferenceNames } from "./reference-names";
import { ProvidersSlot } from "./slots/providers";
import { addressesAsShortForms, blockNodeId } from "./tree-drafts";
import { treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import { overflowing, wordsScrollRules, wordsScrollSheet } from "./words-scroll-fixtures";

/**
 * What one row of the tree draws, read as a reader reads it (R1, R6, R9, R11 and R34).
 *
 * The row is rendered on its own rather than through `IssueTree`, because what these rows are
 * about is the words and the shapes one node draws: the tree's fold, its keys and its placements
 * are read in `IssueTree.test.tsx`, `tree-fold.test.ts` and `issue-tree.test.ts`. A row of the
 * outline draws its words whole, which the browser suite's c-row-face reads in a laid-out page.
 */

function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** Who made a node, as the tree names them (`AgentText.author`). */
type Author = { kind: AuthorKind; name: string; reader: boolean };

/** Who made a node the model holds no author for, as the tree names one (`AgentText.author`). */
const NOBODY_RECORDED: Author = {
  kind: authorKind(undefined),
  name: CONTROL_LABELS.unknown_author,
  reader: false,
};

/** One row, drawn as the outline draws every row, with what it holds on its face;
 *  `author` is who made it, as the tree names them (`AgentText.author`); `answerer` the person
 *  whose answer an ask carries; `expanded` and `folded` its fold, for a node that holds any;
 *  `focused` whether it holds the tree's focus. It is drawn inside `NoTooltips`, as the page's
 *  slots draw every row, so an author is named as the page names one. `redraw` draws the same
 *  row of another node, as a later read of the tree does. */
function renderRow(
  drawn: TreeNode,
  {
    answerer,
    author = NOBODY_RECORDED,
    editable,
    expanded,
    focused = false,
    folded,
  }: {
    answerer?: string;
    author?: Author;
    editable?: boolean;
    expanded?: boolean;
    focused?: boolean;
    folded?: number;
  } = {}
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const row = (shown: TreeNode) => (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>
          <NodeRow
            {...(answerer === undefined ? {} : { answerer })}
            author={author}
            context={false}
            {...(editable === undefined ? {} : { editable })}
            {...(expanded === undefined ? {} : { expanded })}
            {...(folded === undefined ? {} : { folded })}
            focused={focused}
            isClosed={false}
            level={1}
            node={shown}
            onFocus={() => {}}
            onToggle={() => {}}
            ownAnswer={false}
            rowKey="row"
            tabStop={false}
          />
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(row(drawn));
  return Object.assign(view, { redraw: (next: TreeNode) => view.rerender(row(next)) });
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R9: a node whose author the model holds says who made it by a name and a shape, never by
// colour alone and never by an id, and two authors of different kinds never share a shape.
// Ruling 5: a node the model holds no author for draws none in its row's margin, and its row
// says so under its words. Ruling 6: an agent is named by its role's word, and its row gives
// its whole title under its words, whatever the focus (R11).
test("an author is drawn by a name and a shape of its own kind", () => {
  // A comment, which a person and an agent both write: a decision's face draws Keep and
  // Change, which are writes, and this row is about the words and the shape alone.
  const drawn = (author: Author) => {
    const view = renderRow(node({ id: "comment-1", type: "comment" }), { author });
    try {
      const shown = view.container.querySelector("[data-node-author]");
      const shape = shown?.querySelector("svg[data-glyph]");
      return {
        chrome: shown === null ? null : shown.querySelector("[data-page-chrome]") !== null,
        face: shown === null ? null : shown.closest("[data-node-body]") !== null,
        margin: shown === null ? null : shown.closest("[data-node-margin]") !== null,
        name: shape?.getAttribute("aria-label") ?? null,
        role: shape?.getAttribute("role") ?? null,
        text: shown?.textContent ?? null,
        title: view.container.querySelector("[data-node-author-title]")?.textContent ?? null,
        token: shape?.getAttribute("data-glyph") ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  // The word is the author's name, and the shape is drawn beside it rather than in place of it,
  // exactly as the type's shape and the state's shape are, at the right of the row (R34). The
  // shape carries its own accessible name: a reader who cannot see it still hears what kind of
  // author it stands for.
  expect(drawn({ kind: "person", name: "alice", reader: false })).toEqual({
    chrome: false,
    face: false,
    margin: true,
    name: "Written by a person",
    role: "img",
    text: "alice",
    title: null,
    token: "author-person",
  });
  expect(drawn({ kind: "agent", name: "Planner", reader: false })).toEqual({
    chrome: false,
    face: false,
    margin: true,
    name: "Written by an agent",
    role: "img",
    text: "Planner",
    title: null,
    token: "author-agent",
  });
  // An agent whose session's title says more than its role is named by the role's word in the
  // margin, and the row gives the whole title under its words.
  const architect: Author = {
    kind: "agent",
    name: "Legion architect · CORE-1",
    reader: false,
  };
  expect(drawn(architect)).toMatchObject({
    margin: true,
    text: "architect",
    title: "Legion architect · CORE-1",
  });
  // Where the model holds no author the margin draws none, and the row says so under its words
  // in the page's own words, which are marked as the page's rather than as somebody's name, with
  // a shape neither other kind draws.
  expect(drawn(NOBODY_RECORDED)).toEqual({
    chrome: true,
    face: true,
    margin: false,
    name: "Written by an author nobody recorded",
    role: "img",
    text: "Author unknown",
    title: null,
    token: "author-unknown",
  });
});

/** One row of the tree as `ConversationRows` draws it: the node as the tree model built it, its
 *  author named by the page's one rule for naming one (`AgentText.author`). */
function TreeRow({ node: drawn }: { node: TreeNode }): ReactNode {
  const agentText = useAgentText();
  return (
    <NodeRow
      author={{
        kind: authorKind(drawn.author),
        name: agentText.author(drawn.author),
        reader: isReader(drawn.author, undefined),
      }}
      context={false}
      focused={false}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => {}}
      onToggle={() => {}}
      ownAnswer={false}
      rowKey="row"
      tabStop={false}
    />
  );
}

// R9: an agent's decision is drawn with who decided it as the decisions read's entry for it
// records (`DecisionEntry.decided_by`), which the read's `decisions` member does not carry. The
// session that wrote it is named by the title the page's sessions read gives it, as any agent's
// is: its role's word on the row and its whole title in the opened body. A session no registry
// lists any longer is named by the title it stamped on what it wrote, which the entry carries
// (`decided_by.title`), in the same two places. A decision whose entry names nobody draws no
// author on its row, and its opened body says "Author unknown". Each decision is a node of the
// tree read, a `decision` block of the spec, whose `decision` names no decider, so the decider is
// its entry's.
test("an agent's decision draws the session its entry names as its decider, by the title it stamped where no registry lists it, and Author unknown where it names none", () => {
  const title = "Legion architect · CORE-1";
  const storedTitle = "Legion planner · CORE-1";
  const named = decision("d-named");
  const unnamed = decision("d-unnamed");
  const stored = decision("d-stored");
  const read = decisionsRead([named, unnamed, stored]);
  const root = buildIssueTree({
    decisions: {
      ...read,
      entries: (read.entries ?? []).map((entry) => {
        if (entry.ref === unnamed.ref) return { ...entry, decided_by: null };
        if (entry.ref !== stored.ref) return entry;
        return {
          ...entry,
          decided_by: { id: "session-gone", kind: "session", title: storedTitle },
        };
      }),
    },
    issue: issueDetails(),
    tree: treeAnswer(
      [named, unnamed, stored].map((made, position) =>
        treeNode(made.block_id, "decision", KEY, {
          decision: { ref: made.ref, waiting_review: true },
          document: { artifact_id: SPEC_DOCUMENT.id, position, slug: SPEC_SLUG },
          text: made.heading,
        })
      )
    ),
  });
  const registered: Agent = {
    capabilities: [],
    dir: "/w",
    host: "host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "host",
    open_asks: 0,
    roles: [],
    session_id: AGENT.id,
    state: "live",
    title,
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(agentsQuery().queryKey, [registered]);
  const drawn = (made: Decision) => {
    const id = blockNodeId(SPEC_SLUG, made.block_id);
    const shown = root.children.find((child) => child.id === id);
    if (shown === undefined) throw new Error(`the tree draws no decision ${id}`);
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <NoTooltips>
            <TreeRow node={shown} />
          </NoTooltips>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      const text = (selector: string) =>
        view.container.querySelector(selector)?.textContent ?? null;
      return {
        face: text("[data-node-body] [data-node-author]"),
        kind: view.container.querySelector("[data-author-kind]")?.getAttribute("data-author-kind"),
        row: text("[data-node-margin] [data-node-author]"),
        title: text("[data-node-author-title]"),
        type: view.container.querySelector("[data-node-row]")?.getAttribute("data-node-type"),
      };
    } finally {
      view.unmount();
    }
  };
  expect(drawn(named)).toEqual({
    face: null,
    kind: "agent",
    row: "architect",
    title,
    type: "decision",
  });
  expect(drawn(unnamed)).toEqual({
    face: CONTROL_LABELS.unknown_author,
    kind: "unknown",
    row: null,
    title: null,
    type: "decision",
  });
  expect(drawn(stored)).toEqual({
    face: null,
    kind: "agent",
    row: "planner",
    title: storedTitle,
    type: "decision",
  });
});

/** A session nobody named, which the page calls by its word for an agent. */
const UNTITLED: Actor = { id: "0f8e2c4a-1111-4222-8333-444455556666", kind: "session" };

/** Who deleted a post, in each form the page names an author by, and the name the page gives
 *  each: a session nobody named, a titled session, a person, and a person acting through a
 *  session. */
const DELETERS: readonly (readonly [Actor, string])[] = [
  [UNTITLED, CONTROL_LABELS.unnamed_author],
  [
    {
      id: "1a2b3c4d-1111-4222-8333-444455556666",
      kind: "session",
      origin: { session_title: "Planner" },
    },
    "Planner",
  ],
  [{ id: "alice", kind: "user" }, "alice"],
  [{ act: { session: UNTITLED.id }, id: "alice", kind: "user" }, "alice, acting through an agent"],
];

/** When the posts below were deleted. */
const DELETED_AT = "2026-11-05T12:00:00Z";

/** A comment its author deleted, as the model holds it (`tree-drafts.ts`'s `addTurns`): its
 *  delete, and the deleted line as its text, the actor named as no registry knows it and the
 *  time as stored, with no reading. */
function deletedTurn(by: Actor, more: Partial<TreeNode> = {}): TreeNode {
  return node({
    author: { id: "bob", kind: "user" },
    deletedAt: DELETED_AT,
    deletedBy: by,
    id: "comment-1",
    reference: "dispatch://CORE-1/comment/comment-1",
    text: `Deleted by ${actorLabelText(by)} · ${DELETED_AT}`,
    type: "comment",
    ...more,
  });
}

// R3 of dispatch://INFRA-1523, and INFRA-1196's decision 342: a comment its author deleted keeps
// its row, which draws the deleted line where its words stood, as every surface draws it
// (`DeletedBody`, in its italic), whole and as a folded branch's one line. The actor is named as
// the page names an author, by the rule the row's own author is named by: a session nobody named
// reads "An agent" and never its short id, and a person acting through a session reads as the
// page names one. None of the model's text is drawn, which names the actor as no registry knows
// it and the time as stored.
test("a deleted comment's row draws the deleted line, its actor named as the page names an author", () => {
  for (const [by, name] of DELETERS) {
    for (const folded of [false, true]) {
      const where = `${name}${folded ? ", folded" : ""}`;
      const view = renderRow(
        deletedTurn(by, folded ? { held: 1 } : {}),
        folded ? { expanded: false, folded: 1 } : {}
      );
      try {
        const words = view.container.querySelector(
          folded ? "[data-node-first-line]" : "[data-node-words]"
        );
        const line = words?.querySelector("[data-post-deleted]");
        const opening = `Deleted by ${name} · `;
        expect(line?.textContent?.slice(0, opening.length), where).toBe(opening);
        expect(line?.textContent?.length ?? 0, where).toBeGreaterThan(opening.length);
        expect(line?.className, where).toContain("italic");
        expect(words?.textContent, where).not.toContain("session:");
        expect(words?.textContent, where).not.toContain(DELETED_AT);
      } finally {
        view.unmount();
      }
    }
  }
});

// R2 of dispatch://INFRA-1523: a message its author edited says ` · edited` after its time, as
// its byline does on every surface (`EditedMark`). The row says it at its right, outside its
// words, so no cut of the words takes it: an agent's words cut at six lines, or a folded
// branch's one line. A message nobody edited says nothing of an edit.
test("an edited message's row says edited after its time, whole or folded, outside its words", () => {
  const at = `${new Date().getFullYear()}-11-05T12:00:00Z`;
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  const drawn = (editedAt: string | undefined, folded: boolean) =>
    renderRow(
      node({
        at,
        ...(editedAt === undefined ? {} : { editedAt }),
        held: folded ? 1 : 0,
        id: "message-1",
        text: "Ship it.",
        type: "message",
      }),
      { author: planner, ...(folded ? { expanded: false, folded: 1 } : {}) }
    );
  for (const folded of [false, true]) {
    const view = drawn("2026-11-05T13:00:00Z", folded);
    try {
      const where = folded ? "folded" : "whole";
      expect(view.container.querySelector("[data-node-byline]")?.textContent, where).toMatch(
        /^Planner · \S.* · edited$/u
      );
      const mark = view.container.querySelector("[data-node-edited]");
      expect(mark?.closest("[data-node-margin]"), where).not.toBeNull();
      expect(mark?.closest("[data-node-words], [data-node-first-line]"), where).toBeNull();
    } finally {
      view.unmount();
    }
  }
  const unedited = drawn(undefined, false);
  try {
    expect(unedited.container.querySelector("[data-node-byline]")?.textContent).not.toContain(
      "edited"
    );
    expect(unedited.container.querySelector("[data-node-edited]")).toBeNull();
  } finally {
    unedited.unmount();
  }
});

// R2 and R3 of dispatch://INFRA-1523 under a filter, and INFRA-1196's decision 342: a node whose
// parent is no row draws the parent as one line, and a parent its author deleted is drawn as the
// deleted line, its actor named as the page names an author, as the parent's own row draws it,
// never as the model's line holds it; one its author edited says ` · edited` after its line.
test("a parent line draws a deleted parent as its deleted line, and an edited one with its mark", () => {
  const under = (line: ParentLine) =>
    renderRow(
      node({
        id: "plan-md#b-t1",
        parentLine: line,
        text: "Stand the harness up.",
        type: "plan_task",
      }),
      {}
    );
  const deleted = under({
    deletedAt: DELETED_AT,
    deletedBy: UNTITLED,
    id: "comment-1",
    line: `Deleted by ${actorLabelText(UNTITLED)} · ${DELETED_AT}`,
    reference: "dispatch://CORE-1/comment/comment-1",
    type: "comment",
  });
  try {
    const parent = deleted.container.querySelector("[data-node-parent-line]");
    const opening = `Deleted by ${CONTROL_LABELS.unnamed_author} · `;
    const line = parent?.querySelector("a [data-post-deleted]");
    expect(line?.textContent?.slice(0, opening.length)).toBe(opening);
    expect(parent?.textContent).not.toContain("session:");
    expect(parent?.textContent).not.toContain(DELETED_AT);
  } finally {
    deleted.unmount();
  }
  const edited = under({
    editedAt: "2026-11-05T13:00:00Z",
    id: "message-1",
    line: "Ship it.",
    reference: "dispatch://CORE-1/message/message-1",
    type: "message",
  });
  try {
    expect(edited.container.querySelector("[data-node-parent-line] a")?.textContent).toBe(
      "Ship it. · edited"
    );
  } finally {
    edited.unmount();
  }
});

/** The words of `region` by the mark each is read under, as a reader of the page's words reads
 *  them (`page-words.ts`): a word whose nearest marked element is `[data-page-chrome]` is the
 *  page's own, one whose nearest is `[data-author-text]` is its author's, and any other is
 *  marked neither. */
function wordsByMark(region: Element | null | undefined) {
  const marked = { author: [] as string[], chrome: [] as string[], unmarked: [] as string[] };
  if (region === null || region === undefined) return marked;
  const walker = document.createTreeWalker(region, NodeFilter.SHOW_TEXT);
  for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
    const holder = text.parentElement?.closest(
      `[${PAGE_CHROME_ATTRIBUTE}], [${AUTHOR_TEXT_ATTRIBUTE}]`
    );
    const words = wordsIn(text.textContent ?? "");
    if (holder === null || holder === undefined) marked.unmarked.push(...words);
    else if (holder.hasAttribute(PAGE_CHROME_ATTRIBUTE)) marked.chrome.push(...words);
    else marked.author.push(...words);
  }
  return marked;
}

// The architect's ruling of 2026-11-20 07:52Z on dispatch://INFRA-1523: the deleted line
// ("Deleted by …", "Deleted") and the edited mark (" · edited") a row draws are the page's own
// words, so each carries the page-chrome mark and is declared in `page-words.ts`, as its rule says;
// the actor's name, and the time, stay its author's. That holds of the row's words whole and
// folded, of its margin, of a parent line, and of a chip.
test("a row's deleted line and edited mark are the page's own words, and the actor's name stays the author's", () => {
  const chrome = (region: Element | null | undefined) => {
    const { author, chrome: own, unmarked } = wordsByMark(region);
    return {
      author: author.filter((word) => ["alice", "an", "agent", "ship", "it"].includes(word)),
      chrome: own,
      undeclared: own.filter((word) => !DECLARED_WORDS.includes(word)),
      unmarked,
    };
  };
  const deletedLine = {
    author: ["alice"],
    chrome: ["deleted", "by"],
    undeclared: [],
    unmarked: [],
  };
  const alice: Actor = { id: "alice", kind: "user" };
  const shown: Record<string, unknown> = {};

  for (const folded of [false, true]) {
    const view = renderRow(
      deletedTurn(alice, folded ? { held: 1 } : {}),
      folded ? { expanded: false, folded: 1 } : {}
    );
    try {
      const words = view.container.querySelector(
        folded ? "[data-node-first-line]" : "[data-node-words]"
      );
      shown[folded ? "folded" : "whole"] = chrome(words?.querySelector("[data-post-deleted]"));
    } finally {
      view.unmount();
    }
  }
  const edited = renderRow(
    node({
      at: "2026-11-05T12:00:00Z",
      editedAt: "2026-11-05T13:00:00Z",
      id: "m-1",
      type: "message",
    }),
    { author: { kind: "person", name: "alice", reader: false } }
  );
  try {
    shown.edited = chrome(edited.container.querySelector("[data-node-edited]"));
  } finally {
    edited.unmount();
  }
  const under = (line: ParentLine) =>
    renderRow(
      node({ id: "plan-md#b-t1", parentLine: line, text: "Stand it up.", type: "plan_task" }),
      {}
    );
  const deletedParent = under({
    deletedAt: DELETED_AT,
    deletedBy: alice,
    id: "comment-1",
    line: `Deleted by alice · ${DELETED_AT}`,
    reference: "dispatch://CORE-1/comment/comment-1",
    type: "comment",
  });
  try {
    shown.deletedParent = chrome(
      deletedParent.container.querySelector("[data-node-parent-line] [data-post-deleted]")
    );
  } finally {
    deletedParent.unmount();
  }
  const editedParent = under({
    editedAt: "2026-11-05T13:00:00Z",
    id: "message-1",
    line: "Ship it.",
    reference: "dispatch://CORE-1/message/message-1",
    type: "message",
  });
  try {
    shown.editedParent = chrome(editedParent.container.querySelector("[data-node-parent-line] a"));
  } finally {
    editedParent.unmount();
  }
  const chips = renderRow(
    node({
      chips: [
        {
          answered: true,
          deletedAt: DELETED_AT,
          deletedBy: UNTITLED,
          gone: false,
          name: `Deleted by ${actorLabelText(UNTITLED)}`,
          ref: "dispatch://CORE-1/comment/c-deleted",
        },
        {
          answered: true,
          editedAt: "2026-11-05T13:00:00Z",
          gone: false,
          name: "Ship it.",
          ref: "dispatch://CORE-1/message/m-edited",
        },
      ],
      id: "comment-9",
      text: "Read these.",
      type: "comment",
    }),
    {}
  );
  try {
    const [deletedChip, editedChip] = [...chips.container.querySelectorAll("[data-chip-name]")];
    shown.deletedChip = chrome(deletedChip);
    shown.editedChip = chrome(editedChip);
  } finally {
    chips.unmount();
  }

  expect(shown).toEqual({
    deletedChip: {
      author: ["an", "agent"],
      chrome: ["deleted", "by"],
      undeclared: [],
      unmarked: [],
    },
    deletedParent: deletedLine,
    edited: { author: [], chrome: ["edited"], undeclared: [], unmarked: [] },
    editedChip: { author: ["ship", "it"], chrome: ["edited"], undeclared: [], unmarked: [] },
    editedParent: { author: ["ship", "it"], chrome: ["edited"], undeclared: [], unmarked: [] },
    folded: deletedLine,
    whole: deletedLine,
  });
});

// A view that edits rows in place passes `editable`, and the row element itself carries
// `data-editable` for that view's style. Every other view passes nothing, and its rows carry none.
test("a row carries data-editable where its view passes editable, and none by default", () => {
  const marked = (editable?: boolean) => {
    const view = renderRow(node({ id: "comment-1", type: "comment" }), { editable });
    try {
      const row = view.container.querySelector("[data-node-row]");
      if (row === null) throw new Error("the row draws no [data-node-row]");
      return {
        anywhere: view.container.querySelectorAll("[data-editable]").length,
        row: row.getAttribute("data-editable"),
      };
    } finally {
      view.unmount();
    }
  };
  expect(marked(true), "editable left the row unmarked").toEqual({ anywhere: 1, row: "" });
  // The negative controls: a row its view passes nothing, and one passed false, carry none.
  expect(marked(), "a row with no editable is marked").toEqual({ anywhere: 0, row: null });
  expect(marked(false), "a row passed editable false is marked").toEqual({
    anywhere: 0,
    row: null,
  });
});

// R34: a typed node draws its kind as one tag, its word and its shape, in the margin at the
// right of its row and never before its words; a turn of the conversation draws no tag. R9: the
// time it was made is drawn after its author in the page's short form.
test("a typed node draws its kind as one tag at the right, and a turn draws none", () => {
  // Noon of a day of the year the test runs in, so its day is the 5th from UTC-11 to UTC+11 and
  // the page's short form draws no year.
  const at = `${new Date().getFullYear()}-11-05T12:00:00Z`;
  const drawn = (type: NodeType, more: Partial<TreeNode> = {}) => {
    const view = renderRow(
      node({ at, id: `${type}-1`, text: "Words of the node.", type, ...more })
    );
    try {
      const tag = view.container.querySelector("[data-node-tag]");
      return {
        afterWords: tag === null || tag.closest("[data-node-margin]") !== null,
        tag: tag?.textContent ?? null,
        token: tag?.querySelector("svg")?.getAttribute("data-glyph") ?? null,
        time: view.container.querySelector("[data-node-time]")?.textContent ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  expect(drawn("requirement", { statement: true })).toEqual({
    afterWords: true,
    tag: NODE_TYPE_NAMES.requirement,
    time: "5 Nov",
    token: "requirement",
  });
  expect(drawn("check")).toMatchObject({ tag: NODE_TYPE_NAMES.check, token: "check" });
  expect(drawn("comment")).toMatchObject({ tag: null, time: "5 Nov" });
  expect(drawn("message")).toMatchObject({ tag: null });
});

// Decision 246: a `requirement` block is a statement only in the issue's spec (`isStatement`).
// The same block of the plan, or of any other document, is a block and nothing more, so its row
// draws its type's own word and none of a statement's marks - not its word, its shape or its
// colour - as a `check` block outside the Verification document draws no state word.
test("a plan's requirement row reads requirement with no statement mark, and a spec's reads as a statement", () => {
  const drawn = (id: string, more: Partial<TreeNode>) => {
    const view = renderRow(
      node({ id, text: "The page draws no raw source.", type: "requirement", ...more })
    );
    try {
      const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
      const dot = view.container.querySelector("[data-node-bullet] i");
      return {
        bullet: dot?.classList.contains(pageKindBullet.statement) ?? null,
        colour: tag?.classList.contains(pageKindTag.statement) ?? null,
        tag: tag?.textContent ?? null,
        token: tag?.querySelector("svg")?.getAttribute("data-glyph") ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  expect(drawn("plan-md#b-r1", {}), "a plan's requirement row drew a statement's mark").toEqual({
    bullet: false,
    colour: false,
    tag: BLOCK_TYPE_NAMES.requirement,
    token: null,
  });
  expect(
    drawn("spec-md#b-r1", { statement: true }),
    "a spec's requirement row did not draw a statement"
  ).toEqual({
    bullet: true,
    colour: true,
    tag: NODE_TYPE_NAMES.requirement,
    token: "requirement",
  });
});

// R34, R9: a reading is a typed node - an agent's reading of a person's answer, inside the ask or
// the decision it reads - so its row draws its kind as one tag, its word and a shape no other
// kind draws; and it is always an agent's words, so its bullet and its tag take an agent's quiet
// grey, never the colour of the question or the decision it reads, and it never reads as the
// person's decision itself.
test("a reading row draws its own word and shape, in an agent's grey", () => {
  const drawn = (type: NodeType) => {
    const view = renderRow(node({ id: `spec#b-${type}`, text: "Keep the rail.", type }));
    try {
      const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
      const dot = view.container.querySelector("[data-node-bullet] i");
      return {
        agentBullet: dot?.classList.contains(pageKindBullet.agent) ?? null,
        agentTag: tag?.classList.contains(pageKindTag.agent) ?? null,
        shape: tag?.querySelector("svg path")?.getAttribute("d") ?? null,
        tag: tag?.textContent ?? null,
        token: tag?.querySelector("svg")?.getAttribute("data-glyph") ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  const reading = drawn("reading");
  expect(reading).toMatchObject({
    agentBullet: true,
    agentTag: true,
    tag: NODE_TYPE_NAMES.reading,
    token: "reading",
  });
  // The negative controls: the question and the decision a reading reads draw their own colours,
  // and no other shape the page draws - a kind's, a state's or an author's - is the reading's.
  for (const type of ["ask", "decision"] as const) {
    expect(drawn(type), `the ${type} drew an agent's grey`).toMatchObject({
      agentBullet: false,
      agentTag: false,
    });
  }
  const shapes = Object.keys(GLYPH_NAMES)
    .filter((token) => token !== GLYPH_TOKENS.types.reading)
    .map((token) => {
      const view = render(<NodeGlyph token={token} />);
      try {
        return view.container.querySelector("path")?.getAttribute("d") ?? null;
      } finally {
        view.unmount();
      }
    });
  expect(shapes.length).toBeGreaterThan(0);
  expect(shapes, "another shape the page draws is a reading's").not.toContain(reading.shape);
});

// R21: a kind is told by its word and a shape of its own, never by colour alone, so every type
// token draws a shape no other token draws, and none draws the dash a token with no shape falls
// back to. `NodeGlyph.tsx` has no test file of its own, so its shapes are read here, drawn.
test("every type token has its own glyph path and none falls back", () => {
  const shapeOf = (token: string) => {
    const view = render(<NodeGlyph token={token} />);
    try {
      return view.container.querySelector("path")?.getAttribute("d") ?? null;
    } finally {
      view.unmount();
    }
  };
  const fallback = shapeOf("a token with no shape");
  const types: readonly string[] = Object.values(GLYPH_TOKENS.types);
  const shapes = new Map(Object.keys(GLYPH_NAMES).map((token) => [token, shapeOf(token)]));
  expect(
    types.filter((token) => shapes.get(token) === fallback),
    "a type token drew the fallback"
  ).toEqual([]);
  expect(
    types.filter((token) =>
      [...shapes].some(([other, shape]) => other !== token && shape === shapes.get(token))
    ),
    "a type token drew a shape another token draws"
  ).toEqual([]);
});

// R11: a node the reader folded is the one place a row draws one line of its node: the first
// line, cut at its first sentence end, and how many nodes it holds, whatever the focus - the
// focus changes no row's text.
test("a folded branch draws its first line and its count, focused or not", () => {
  const text = "The page draws the whole issue in one reading. Nothing is lost on the way.";
  const folded = node({ held: 3, id: "comment-1", text, type: "comment" });
  for (const focused of [false, true]) {
    const shut = renderRow(folded, { expanded: false, focused, folded: 3 });
    try {
      // A line that is a whole sentence ends on the sentence's own mark, with no ellipsis after
      // it.
      expect(shut.container.querySelector("[data-node-first-line]")?.textContent).toBe(
        "The page draws the whole issue in one reading."
      );
      expect(shut.container.querySelector("[data-node-count]")?.textContent).toBe(
        `3 ${CONTROL_LABELS.folded_rows}`
      );
      expect(shut.container.querySelector("[data-node-words]")).toBeNull();
      // The bullet is the branch's fold control, named by what pressing it does.
      expect(shut.getByRole("button", { name: CONTROL_LABELS.unfold })).toBeDefined();
    } finally {
      shut.unmount();
    }
  }
});

// Decision 95: a folded branch's one line writes a bare address in its words as its target's
// short form, as every one line of a node does (`lineWords`); its words, unfolded, keep the
// address as its author wrote it.
test("a folded branch's line writes a bare address as its short form", () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const text = `Read ${address} before the rows.`;
  const folded = node({ held: 1, id: "comment-1", text, tree: displayTree(text), type: "comment" });
  const shut = renderRow(folded, { expanded: false, folded: 1 });
  try {
    const line = shut.container.querySelector("[data-node-first-line]")?.textContent ?? "";
    expect(line, "the folded line draws the address as written").not.toContain("dispatch://");
    expect(line).toBe(`Read ${addressesAsShortForms(address)} before the rows.`);
  } finally {
    shut.unmount();
  }
});

// R1, R9, ruling 1: an answered ask carries the person's answer in its own row,
// and no row of its own: who answered by a name and a shape, what they chose among the ask's own
// options, and what they wrote as the store holds it, character for character and whole.
test("an answered ask draws the answer in its own row, word for word", () => {
  const text = "The preset row keeps its labels above that width.\n\nBelow it, one control.";
  const asked = node({
    answer: {
      at: "2026-11-06T00:00:00Z",
      by: { id: "alice", kind: "user" },
      selected: ["1280 px"],
      text,
    },
    id: "ask-1",
    options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
    state: "answered",
    text: "At which width does the identity block condense?",
    type: "ask",
  });
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  const view = renderRow(asked, { answerer: "alice", author: planner });
  try {
    const rows = view.container.querySelectorAll("[data-node-row]");
    expect(rows.length).toBe(1);
    const line = view.container.querySelector("[data-node-answer]");
    expect(line?.querySelector("[data-page-chrome]")?.textContent).toBe("Answer");
    expect(line?.querySelector("[data-answer-author]")?.textContent).toBe("alice");
    expect(line?.querySelector("[data-answer-author] svg")?.getAttribute("data-glyph")).toBe(
      "author-person"
    );
    expect(line?.querySelector("[data-answer-text]")?.textContent).toBe(text);
    // The option the person chose is marked in the ask's own list, and the row adds what an
    // option says beside its name.
    expect(
      [...view.container.querySelectorAll("[data-node-option]")].map((one) => [
        one.getAttribute("data-node-option"),
        one.textContent,
      ])
    ).toEqual([
      ["chosen", "●1280 px — Where the rail fits."],
      ["", "○1024 px"],
    ]);
    // The row's own author is still the asker's, and the answerer's name is no row author.
    expect(view.container.querySelector("[data-node-author]")?.textContent).toBe("Planner");
  } finally {
    view.unmount();
  }
});

// R9: a decision the model marks unsure says so in one word on its own row, and a decision it
// does not mark says nothing of the kind.
test("a decision the model marks unsure says so in one word", () => {
  const unsure = (marked: boolean) => {
    const view = renderRow(
      node({ id: "spec#b-d3", type: "decision", ...(marked ? { unsure: true } : {}) })
    );
    try {
      return view.container.querySelector("[data-node-unsure]")?.textContent ?? null;
    } finally {
      view.unmount();
    }
  };
  expect(unsure(true)).toBe("Unsure");
  expect(unsure(false)).toBeNull();
});

/** The few words a row draws for `gap`, and the sentence its face draws under them. */
function drawnGap(drawn: TreeNode): { chip: string | undefined; line: string | undefined } {
  const view = renderRow(drawn);
  try {
    return {
      chip: view.container.querySelector("[data-gap]")?.textContent ?? undefined,
      line: [...view.container.querySelectorAll("[data-node-body] p")]
        .map((paragraph) => paragraph.textContent ?? "")
        .find((text) => text.endsWith(".")),
    };
  } finally {
    view.unmount();
  }
}

// R8: a citation that must name a requirement and names something else is a gap, drawn on the
// node that holds the citation and never as an error.
test("a check whose citation names no statement says so, in words", () => {
  const gaps: GapKind[] = ["serves_not_requirement"];
  expect(drawnGap(node({ gaps, id: "verification-md#b-c1", type: "check" }))).toEqual({
    chip: "Not a statement",
    line: "What this says it serves is not a statement.",
  });
});

// R8: a plan task that names no requirement it serves and no declaration it attempts is work
// no reason stands behind, and the row says that rather than leaving the reader to notice it,
// in words that pass no judgement on the work.
test("a plan task nobody asked for says so, in words", () => {
  const gaps: GapKind[] = ["nobody_asked"];
  expect(drawnGap(node({ gaps, id: "plan-md#b-t9", type: "plan_task" }))).toEqual({
    chip: "No reason given",
    line: "This task names no statement it serves and nothing a person asked for.",
  });
});

// While a document of the issue is still being indexed no statement is drawn as lacking a check:
// its face draws, where its gaps are, that document's line in the server's own words, and its row
// draws no gap.
test("a statement whose coverage waits on a document still being indexed draws its line and no gap", () => {
  const owed = "spec.md version 12 is still being indexed; its coverage shows once that ends.";
  expect(
    drawnGap(
      node({ coverageNotes: [owed], id: "spec-md#b-r1", statement: true, type: "requirement" })
    )
  ).toEqual({ chip: undefined, line: owed });
});

/** One row the way the tree draws it: unfocused until a focus lands inside it, and then the
 *  focused row, so a test reads what that focus does to the page. */
function FocusableRow({ drawn }: { drawn: TreeNode }): ReactNode {
  const [focused, setFocused] = useState(false);
  return (
    <NodeRow
      author={NOBODY_RECORDED}
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onToggle={() => {}}
      ownAnswer={false}
      rowKey="row"
      tabStop={false}
    />
  );
}

// R9 brings the focused row on screen, but a press inside an unfocused row focuses the row
// before the press ends: a scroll then moves what the pointer went down on, and the press ends
// on something else and does nothing. Measured at 1440 px on the Decisions preset, with the
// row's top 24 px under the identity block, the page scrolled 32 px between mousedown and
// mouseup and a reader's Keep was never sent. A key's focus still brings the row on screen.
test("a press inside a row does not scroll the page under it, and a key's focus still does", async () => {
  const user = userEvent.setup();
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  // The row's top sits above the line the identity block leaves clear, so R9 would scroll. On
  // Element, which owns the method: a spy on HTMLElement leaves an own property behind once
  // restored, which hides every later spy on Element (Margin.landing.test.tsx's `stubRects`).
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, -24, 400, 300)
  );
  const drawn = node({ id: "comment-1", type: "comment" });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const draw = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <FocusableRow drawn={drawn} />
        </MemoryRouter>
      </QueryClientProvider>
    );
  const pressed = draw();
  try {
    // The row's words, which a press focuses the row through: Keep open leaves the focus where
    // it is (decision 141).
    const words = pressed.container.querySelector("[data-node-words]");
    if (words === null) throw new Error("the row draws no words");
    await user.pointer({ keys: "[MouseLeft>]", target: words });
    expect(
      pressed.container.querySelector("[data-node-row]")?.contains(document.activeElement)
    ).toBe(true);
    expect(scrollBy, "the press scrolled the page under the pointer").not.toHaveBeenCalled();
    await user.pointer({ keys: "[/MouseLeft]" });
  } finally {
    pressed.unmount();
  }
  const keyed = draw();
  try {
    const row = keyed.container.querySelector<HTMLElement>("[data-node-row]");
    act(() => row?.focus());
    expect(scrollBy, "a key's focus no longer brings the row on screen").toHaveBeenCalledTimes(1);
  } finally {
    keyed.unmount();
    box.mockRestore();
    scrollBy.mockRestore();
  }
});

/** A stored text holding a heading, bold, a list item and a code span: every one of them a
 *  symbol a reader must never see drawn as written. */
const MARKED = "## Store the notices\n\nA **decided** thing.\n\n- one `code` item";

/** What a reader reads of one drawn row: whether its words hold any symbol the author typed,
 *  and what its words format. */
function drawnWords(view: RenderResult) {
  const words = view.container.querySelector("[data-node-words]");
  return {
    bold: words?.querySelector("strong")?.textContent,
    code: words?.querySelector("code")?.textContent,
    item: words?.querySelector("li")?.textContent,
    symbols: /\*\*|##|`/u.test(view.container.querySelector("[data-node-row]")?.textContent ?? ""),
  };
}

/** What `drawnWords` reads of a row drawn from the reading of `MARKED`. */
const MARKED_DRAWN = {
  bold: "decided",
  code: "code",
  item: "one code item",
  symbols: false,
};

// R20, c-plain-words: a decision's text is stored markdown, and the decisions read sends the
// server's reading of it (`Decision.text_tree`). The row draws that reading, so the reader sees
// a heading, bold words and a list, never the `##`, `**` and `-` the author typed, and the
// browser parses nothing to get there. Folded, its one line holds none of those symbols either,
// and is cut from its first block's words alone (`lineWords`): the heading never runs into the
// paragraph after it.
test("a decision whose text holds bold draws no literal asterisks", () => {
  const decision = node({
    id: "spec#b-d1",
    text: MARKED,
    tree: displayTree(MARKED),
    type: "decision",
  });
  const view = renderRow(decision);
  try {
    expect(drawnWords(view)).toEqual(MARKED_DRAWN);
  } finally {
    view.unmount();
  }
  const folded = renderRow({ ...decision, held: 1 }, { expanded: false, folded: 1 });
  try {
    expect(folded.container.querySelector("[data-node-first-line]")?.textContent).toBe(
      "Store the notices"
    );
  } finally {
    folded.unmount();
  }
});

// The same for every other kind whose read sends the server's reading of its text: an ask's
// question (`question_tree`), a comment's and a message's body (`body_tree`).
test("an ask, a comment and a message draw the server's reading of their text", () => {
  for (const [type, id] of [
    ["ask", "ask-1"],
    ["comment", "comment-1"],
    ["message", "message-1"],
  ] as const) {
    const view = renderRow(node({ id, text: MARKED, tree: displayTree(MARKED), type }));
    try {
      expect({ type, ...drawnWords(view) }).toEqual({ type, ...MARKED_DRAWN });
    } finally {
      view.unmount();
    }
  }
});

// R20: a node whose read sends no reading of its text - a requirement, a check, a result, and a
// block of a document named by the batched reference read - draws the text as stored. The page
// parses no stored markdown of its own to draw it otherwise.
test("a node whose read sends no reading of its text draws the text as stored", () => {
  const view = renderRow(node({ id: "spec#b-r1", text: MARKED, type: "requirement" }));
  try {
    const words = view.container.querySelector("[data-node-words]");
    expect(words?.textContent).toContain("A **decided** thing.");
    expect(words?.querySelector("strong")).toBeNull();
  } finally {
    view.unmount();
  }
});

// Decision 282: at 390 px a note whose body holds a table of sixteen columns, drawn in its row,
// made the whole page 794 px wide. A wide block in a node's words - a table, a code block -
// scrolls sideways inside itself, as a table does in a document's reading, so neither the row
// nor the page does. The row's own blocks are the ones a rule of `styles.css` gives that scroll,
// drawn as a block held to the width of what holds it; the widths are the browser suite's to
// read, since no DOM of a unit test lays any out.
test("a table and a code block in a row's words each scroll sideways on their own", () => {
  const text = "| One | Two |\n| --- | --- |\n| 1 | 2 |\n\n```\nconst wide = true;\n```";
  const view = renderRow(
    node({ id: "notes-md#b-n1", text, tree: displayTree(text), type: "callout" })
  );
  try {
    const rules = wordsScrollRules().filter(({ declared }) =>
      ["display: block", "max-width: 100%"].every((one) => declared.has(one))
    );
    for (const block of ["table", "pre"]) {
      const drawn = view.container.querySelector(`[data-node-words] ${block}`);
      if (drawn === null) throw new Error(`the row's words draw no ${block}`);
      expect(
        rules.some(({ selectors }) => [...document.querySelectorAll(selectors)].includes(drawn)),
        `no rule of styles.css scrolls the ${block} in a row's words sideways on its own`
      ).toBe(true);
    }
  } finally {
    view.unmount();
  }
});

// Decision 317: the outline is one tab stop, and only the focused row's scrollers keep a tab stop
// of their own, each named, so a reader on the keyboard can scroll a wide code block or table
// sideways; every other row's stay out of the tab order. The row hands its focus to its words.
test("out of the tab order, a focused row's code block and table keep their tab stop, named, and an unfocused row's do not", () => {
  const text = "| One | Two |\n| --- | --- |\n| 1 | 2 |\n\n```\nconst wide = true;\n```";
  // The rules that make each block scroll, which the hook reads through the computed style, and
  // each block's content laid out past its box, as a wide one's is.
  const sheet = wordsScrollSheet();
  document.head.append(sheet);
  const restore = overflowing("[data-node-words] pre, [data-node-words] table");
  const drawn = (focused: boolean) => {
    const view = renderRow(
      node({ id: "notes-md#b-n1", text, tree: displayTree(text), type: "callout" }),
      { focused }
    );
    try {
      return ["pre", "table"].map((block) => {
        const scroller = view.container.querySelector(`[data-node-words] ${block}`);
        if (scroller === null) throw new Error(`the row's words draw no ${block}`);
        return {
          label: scroller.getAttribute("aria-label"),
          role: scroller.getAttribute("role"),
          tabIndex: scroller.getAttribute("tabindex"),
        };
      });
    } finally {
      view.unmount();
    }
  };
  try {
    expect(drawn(true)).toEqual([
      { label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" },
      { label: CONTROL_LABELS.scrolls_table, role: null, tabIndex: "0" },
    ]);
    expect(drawn(false)).toEqual([
      { label: null, role: null, tabIndex: "-1" },
      { label: null, role: null, tabIndex: "-1" },
    ]);
  } finally {
    restore();
    sheet.remove();
  }
});

// Part G's R4, the tester's F20: a decision's text holds the source of a typed block inside its
// own block, and the row draws the block's words but none of its source lines, whether it draws
// the text as stored or from its reading.
test("a decision whose text quotes a typed block draws none of its source lines", async () => {
  const text = "It names the statement.\n\n:::requirement{#r1}\nThe page draws no raw source.\n:::";
  for (const tree of [undefined, displayTree(text)]) {
    const view = renderRow(
      node({
        id: "spec#b-d1",
        text,
        type: "decision",
        ...(tree === undefined ? {} : { tree }),
      })
    );
    try {
      await waitFor(() => {
        const words = view.container.querySelector("[data-node-words]")?.textContent ?? "";
        expect(words).toContain("The page draws no raw source.");
        expect(words).not.toContain(":::");
      });
    } finally {
      view.unmount();
    }
  }
});

// R9: the tree is one tab stop and nothing inside a row takes one; a key reaches a row's links
// (`useTreeKeymap.ts`'s `rowStops`). Words drawn from a reading hold real links, and each is
// out of the tab order too.
test("the links a row's words draw are out of the tab order", async () => {
  const text =
    "Read [the notes](https://example.com/notes) and [the plan](https://example.com/plan).";
  const view = renderRow(node({ id: "comment-1", text, tree: displayTree(text), type: "comment" }));
  try {
    await waitFor(() => {
      const links = [...view.container.querySelectorAll<HTMLElement>("[data-node-words] a[href]")];
      expect(links.length).toBe(2);
      expect(links.map((link) => link.tabIndex)).toEqual([-1, -1]);
    });
  } finally {
    view.unmount();
  }
});

// Ruling 5: a tie is a chip at the end of the node's words, named by its target's first words
// cut at a word within 32 characters, and never by an address, a slug or a document's name
// (R22). A target the reference read has not named yet draws nothing.
test("a chip names its target by its first words and never by an address", () => {
  const view = renderRow(
    node({
      chips: [
        {
          answered: true,
          gone: false,
          name: "The page draws the whole issue in one reading and nothing is lost.",
          ref: "dispatch://CORE-1/spec#b-r2",
        },
        { answered: false, gone: false, name: "", ref: "dispatch://CORE-1/artifact/plan-md#b-t4" },
        { answered: true, gone: true, name: "", ref: "dispatch://CORE-1/spec#b-r9" },
        {
          answered: true,
          gone: false,
          label: "Needs",
          name: "Add the routes.",
          ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
        },
      ],
      id: "spec#b-d1",
      text: "Keep one reading.",
      type: "decision",
    }),
    {}
  );
  try {
    const chips = [...view.container.querySelectorAll("[data-node-chip]")];
    expect(chips.map((chip) => chip.textContent)).toEqual([
      "The page draws the whole issue…",
      CONTROL_LABELS.target_gone,
      "Needs Add the routes.",
    ]);
    for (const chip of chips) {
      const name = chip.querySelector("[data-chip-name]")?.textContent ?? "";
      expect(name.length, `"${name}" is longer than a chip's 32 characters`).toBeLessThanOrEqual(
        32
      );
    }
    const row = view.container.querySelector("[data-node-row]")?.textContent ?? "";
    expect(row).not.toContain("dispatch://");
    expect(row).not.toMatch(/CORE-1 (spec|plan-md)/u);
    // The chips stand inside the words' own box, after the words, and on no line of their own.
    const trail = view.container.querySelector("[data-node-trail]");
    expect(trail?.parentElement?.hasAttribute("data-node-words")).toBe(true);
    expect(trail?.previousElementSibling?.textContent).toBe("Keep one reading.");
    // The named chip is the link a reader follows to its target.
    expect(chips[0]?.querySelector("a")?.getAttribute("href")).toBe("/issues/CORE-1/spec#b-r2");
  } finally {
    view.unmount();
  }
});

// Decision 66: a target's name is plain words already (`tree-drafts.ts`'s `treeContext` names a
// block by the words of the server's reading of it), so a chip draws it as it stands, with no
// markdown pass, which would read a numbered step as a list and drop its number.
test("a chip named as a numbered step draws its number", () => {
  const view = renderRow(
    node({
      chips: [
        {
          answered: true,
          gone: false,
          label: "Needs",
          name: "1. Stand the harness up.",
          ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
        },
        {
          answered: true,
          gone: false,
          name: "2. Stand the harness up and read the rows",
          ref: "dispatch://CORE-1/artifact/plan-md#b-t3",
        },
      ],
      id: "plan-md#b-t2",
      text: "Draw the rows.",
      type: "plan_task",
    }),
    {}
  );
  try {
    const chips = [...view.container.querySelectorAll("[data-node-chip]")];
    expect(chips.map((chip) => chip.textContent)).toEqual([
      "Needs 1. Stand the harness up.",
      "2. Stand the harness up and…",
    ]);
  } finally {
    view.unmount();
  }
});

// Decisions 62 and 89: a short reference that two or more targets answer to names no one target,
// and the read answers it gone and ambiguous. Its chip draws the reference as its author wrote
// it, as words with no link, and never as a target deleted.
test("a chip of an ambiguous short reference draws the reference as written, not as deleted", () => {
  const ambiguous: NodeChip = {
    ambiguous: true,
    answered: true,
    gone: true,
    name: "",
    ref: "dispatch://CORE-1/comment/c1",
  };
  const view = renderRow(
    node({
      chips: [ambiguous],
      id: "spec#b-d1",
      text: "Keep one reading.",
      type: "decision",
    }),
    {}
  );
  try {
    const chip = view.container.querySelector("[data-node-chip]");
    expect(chip?.textContent).toBe("dispatch://CORE-1/comment/c1");
    expect(chip?.querySelector("a"), "an ambiguous reference is a link to one target").toBeNull();
  } finally {
    view.unmount();
  }
});

// R2 and R3 of dispatch://INFRA-1523 on a tie's chip, and INFRA-1196's decision 342: a chip
// naming a comment its author deleted names it by the deleted line without its time, in the
// deleted line's italic, its actor named as the page names an author and not as the model's name
// holds it; a chip naming a message its author edited names it by its words cut to the chip's 32
// characters, then ` · edited`, so no cut takes the mark.
test("a chip names a deleted post by its deleted line, and an edited one by its cut words then edited", () => {
  const words = "Hold the deploy until the plan lands and the reviewer signs off";
  const view = renderRow(
    node({
      chips: [
        {
          answered: true,
          deletedAt: DELETED_AT,
          deletedBy: UNTITLED,
          gone: false,
          name: `Deleted by ${actorLabelText(UNTITLED)}`,
          ref: "dispatch://CORE-1/comment/c-deleted",
        },
        {
          answered: true,
          editedAt: "2026-11-05T13:00:00Z",
          gone: false,
          name: words,
          ref: "dispatch://CORE-1/message/m-edited",
        },
      ],
      id: "comment-9",
      text: "Read these.",
      type: "comment",
    }),
    {}
  );
  try {
    const chips = [...view.container.querySelectorAll("[data-node-chip]")];
    expect(chips.map((chip) => chip.textContent)).toEqual([
      `Deleted by ${CONTROL_LABELS.unnamed_author}`,
      `${nodeRowChip.chipWords(words)} · edited`,
    ]);
    expect(chips[0]?.querySelector("a [data-post-deleted]")?.className).toContain("italic");
    expect(chips[1]?.querySelector("[data-post-deleted]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 07:52Z, on a chip cut
// short: a deleted post's chip whose actor is named by one long word is cut right after "Deleted
// by" (`chipWords`), and that cut still draws the deleted line, in its italic, its opening marked
// as the page's own words and none of it as the author's.
test("a deleted post's chip cut right after Deleted by keeps the deleted line's italic and its page-chrome mark", () => {
  const title = "legion-infra-1523-implementer";
  const by: Actor = {
    id: "2b3c4d5e-1111-4222-8333-444455556666",
    kind: "session",
    origin: { session_title: title },
  };
  // The title's one word runs past the chip's room, so the cut ends at the space before it.
  expect(nodeRowChip.chipWords(`Deleted by ${title}`)).toBe("Deleted by…");
  const view = renderRow(
    node({
      chips: [
        {
          answered: true,
          deletedAt: DELETED_AT,
          deletedBy: by,
          gone: false,
          name: `Deleted by ${actorLabelText(by)}`,
          ref: "dispatch://CORE-1/comment/c-deleted",
        },
      ],
      id: "comment-9",
      text: "Read these.",
      type: "comment",
    }),
    {}
  );
  try {
    const name = view.container.querySelector("[data-chip-name]");
    const line = name?.querySelector("[data-post-deleted]");
    expect({
      italic: line?.classList.contains("italic"),
      line: line?.textContent,
      words: wordsByMark(name),
    }).toEqual({
      italic: true,
      line: "Deleted by…",
      words: { author: [], chrome: ["deleted", "by"], unmarked: [] },
    });
  } finally {
    view.unmount();
  }
});

/** The names the page's one batched read brought, and the references it read as ambiguous, as
 *  edited and as deleted, published as the page publishes them. */
function Named({
  ambiguous,
  children,
  deleted,
  edited,
  names,
}: {
  ambiguous?: ReadonlySet<string>;
  children: ReactNode;
  deleted?: ReadonlyMap<string, PostDeletion>;
  edited?: ReadonlySet<string>;
  names: ReadonlyMap<string, string>;
}): ReactNode {
  usePublishReferenceNames(names, ambiguous, edited, deleted);
  return children;
}

// F18 and decision 66 in a node's words: a chip in place is named from the batched read, which
// the page publishes as plain words (`useIssueTree.ts`), so it draws them with no markdown pass,
// as the chip at the end of the words does, a numbered step's number kept. The words are drawn
// under the page's own providers (`ProvidersSlot`), as the page draws them, so a second reading
// of the names mounted there would fail on the number.
test("a chip in a node's words draws its target's plain words, a numbered step's number kept", async () => {
  const target = "dispatch://CORE-1/artifact/plan-md#b-t1";
  const text = `It follows ${target}.`;
  const names = new Map([[target, "1. Stand the harness up and read the rows"]]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The reads the page's writer makes (`outline-writer.tsx`), answered before it mounts.
  for (const key of [
    ["asks", KEY, "thread"],
    ["comments", KEY],
    ["issue-messages", KEY],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named names={names}>
            <ProvidersSlot issue={issueDetails()} route={{ key: KEY, kind: "issue" }}>
              <RowText markdown={text} tree={displayTree(text)} />
            </ProvidersSlot>
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await waitFor(() => {
      const chip = view.container.querySelector(`a[data-row-reference="${target}"]`);
      expect(chip?.textContent).toBe("1. Stand the harness up and…");
    });
  } finally {
    view.unmount();
  }
});

// Decision 102: the batched read's names are named as chips once for each answer of the read,
// not once for each row. A block's name is its whole reading, so naming every name again for
// every row cost the page seconds on an issue of 650 rows.
test("the read's names are named as chips once per answer, not once per row", async () => {
  const names = new Map(
    Array.from({ length: 30 }, (_unused, at) => [
      `dispatch://CORE-1/spec#b-r${at}`,
      `The words of statement ${at}, which run on well past the room a chip has.`,
    ])
  );
  const target = "dispatch://CORE-1/spec#b-r0";
  const text = `It follows ${target}.`;
  const named = spyOn(nodeRowChip, "chipWords");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named names={names}>
            {Array.from({ length: 20 }, (_unused, at) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: twenty rows of one text.
              <RowText key={at} markdown={text} tree={displayTree(text)} />
            ))}
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await waitFor(() => {
      const chips = view.container.querySelectorAll(`a[data-row-reference="${target}"]`);
      expect(chips).toHaveLength(20);
      expect(chips[19]?.textContent).toBe("The words of statement 0, which…");
    });
    // A row names a reference the read has not answered by its address alone (`""`), which
    // costs nothing; each of the read's names is named once.
    const ofNames = named.mock.calls.filter(([name]) => name !== "");
    expect(ofNames.length, "every row named the read's names again").toBeLessThanOrEqual(
      names.size
    );
  } finally {
    view.unmount();
    named.mockRestore();
  }
});

// Decisions 62 and 89: a short reference two or more targets answer to names none of them, so
// the page publishes it as ambiguous, apart from its names, and a node's words draw it as its
// author wrote it, with no link, never as a target that is gone. A reference the read named
// nothing for and did not read as ambiguous keeps its fallback: a link, by its short form. Both
// the words drawn from their reading and the words drawn as stored are read.
test("an ambiguous short reference in a node's words is drawn as written with no link, and a gone one keeps its link", async () => {
  const written = "dispatch://CORE-1/spec#b-1a2b3c4d";
  const gone = "dispatch://CORE-1/spec#b-9f8e7d6c";
  const text = `It follows ${written} and ${gone}.`;
  const ambiguous: ReadonlySet<string> = new Set([written]);
  const names: ReadonlyMap<string, string> = new Map();
  for (const tree of [displayTree(text), undefined]) {
    const drawn = tree === undefined ? "as stored" : "from their reading";
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <ReferenceNamesProvider>
            <Named ambiguous={ambiguous} names={names}>
              <RowText markdown={text} {...(tree === undefined ? {} : { tree })} />
            </Named>
          </ReferenceNamesProvider>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      await waitFor(() =>
        expect(
          view.container.querySelector(`a[data-row-reference="${gone}"]`),
          `the gone reference drawn ${drawn} lost its link`
        ).not.toBeNull()
      );
      expect(
        view.container.querySelector(`a[data-row-reference="${written}"]`),
        `the ambiguous reference drawn ${drawn} is a link`
      ).toBeNull();
      expect(view.container.textContent).toContain(`It follows ${written} and `);
    } finally {
      view.unmount();
    }
  }
});

// R2 and R3 of dispatch://INFRA-1523 in a node's words, and INFRA-1196's decision 342: a
// reference to a post its author edited reads its words, cut, then ` · edited`; one to a post its
// author deleted reads the deleted line without its time, in the deleted line's italic, its actor
// named as the page names an author and not as the published name holds it. The words drawn from
// their reading and the words drawn as stored are both read.
test("a node's words name an edited post with its mark, and a deleted one by its deleted line", async () => {
  const edited = "dispatch://CORE-1/comment/c-edited";
  const deleted = "dispatch://CORE-1/message/m-deleted";
  const text = `See ${edited} and ${deleted}.`;
  const editedWords = "Hold the deploy until the plan lands and the reviewer signs off";
  const names = new Map([
    [edited, editedWords],
    [deleted, `Deleted by ${actorLabelText(UNTITLED)}`],
  ]);
  const deletions = new Map([[deleted, { at: DELETED_AT, by: UNTITLED }]]);
  const marked = new Set([edited]);
  for (const tree of [displayTree(text), undefined]) {
    const drawn = tree === undefined ? "as stored" : "from their reading";
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <NoTooltips>
            <ReferenceNamesProvider>
              <Named deleted={deletions} edited={marked} names={names}>
                <RowText markdown={text} {...(tree === undefined ? {} : { tree })} />
              </Named>
            </ReferenceNamesProvider>
          </NoTooltips>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      const link = (reference: string) =>
        view.container.querySelector(`a[data-row-reference="${reference}"]`);
      await waitFor(() =>
        expect(link(edited)?.textContent, drawn).toBe(
          `${nodeRowChip.chipWords(editedWords)} · edited`
        )
      );
      const gone = link(deleted);
      expect(gone?.textContent, drawn).toBe(`Deleted by ${CONTROL_LABELS.unnamed_author}`);
      expect(
        gone?.matches("[data-post-deleted]") === true ||
          gone?.querySelector("[data-post-deleted]") !== null,
        `the deleted post drawn ${drawn} is not in the deleted line's italic`
      ).toBe(true);
      expect(link(edited)?.querySelector("[data-post-deleted]"), drawn).toBeNull();
    } finally {
      view.unmount();
    }
  }
});

/** Where the router stands and how it got there, drawn so a test reads them. */
function Where(): ReactNode {
  const at = useLocation();
  const how = useNavigationType();
  return <output data-how={how} data-where="">{`${at.pathname}${at.hash}`}</output>;
}

/** A node's words drawn at the address `at` under the read's `names`, with the router's place
 *  beside them. `click` dispatches a click on `link` and answers whether the browser was left to
 *  follow it, and then stops the browser: the unit suite's document loads no page. */
function drawWordsAt(
  at: string,
  text: string,
  tree: DisplayTree | undefined,
  names: ReadonlyMap<string, string>
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[at]}>
        <ReferenceNamesProvider>
          <Named names={names}>
            <RowText markdown={text} {...(tree === undefined ? {} : { tree })} />
            <Where />
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const place = () => view.container.querySelector("[data-where]");
  return {
    click(link: Element | null, init: MouseEventInit = {}): boolean {
      let left = false;
      const stop = (event: Event) => {
        left = !event.defaultPrevented;
        event.preventDefault();
      };
      document.addEventListener("click", stop);
      try {
        act(() => {
          link?.dispatchEvent(
            new MouseEvent("click", { bubbles: true, cancelable: true, ...init })
          );
        });
      } finally {
        document.removeEventListener("click", stop);
      }
      return left;
    },
    how: () => place()?.getAttribute("data-how"),
    view,
    where: () => place()?.textContent,
  };
}

// A reference in a row's words is a link of this application: a plain click follows it in place,
// as a link the router draws does, and never loads the whole page again. A click with a key
// held is the browser's, which opens it elsewhere. Both the words drawn as stored and the words
// drawn from their reading, whose links the row's own pass makes, are read.
test("a reference in a row's words is followed in place on a plain click", async () => {
  const target = "dispatch://CORE-1/spec#b-r1";
  const text = `It follows ${target}.`;
  const names = new Map([[target, "Draw the rows."]]);
  for (const tree of [undefined, displayTree(text)]) {
    const drawn = drawWordsAt("/issues/CORE-1", text, tree, names);
    try {
      const link = await waitFor(() => {
        const found = drawn.view.container.querySelector(`a[data-row-reference="${target}"]`);
        expect(found?.textContent).toBe("Draw the rows.");
        return found;
      });
      expect(drawn.click(link, { ctrlKey: true }), "a click with a key held was stopped").toBe(
        true
      );
      expect(drawn.where(), "a click with a key held was followed in place").toBe("/issues/CORE-1");
      expect(drawn.click(link), "a plain click was left to load the page").toBe(false);
      expect(drawn.where()).toBe("/issues/CORE-1/spec#b-r1");
      expect(drawn.how()).toBe("PUSH");
    } finally {
      drawn.view.unmount();
    }
  }
});

// The origin also serves paths the server answers itself, which the page has no route for: a
// link to its API is the browser's to load, never the router's, which would draw "not found".
test("a link in a row's words to a path the server answers is left to the browser", async () => {
  const text = "Read [the issue](/api/v1/issues/CORE-1) as JSON.";
  const drawn = drawWordsAt("/issues/CORE-1", text, displayTree(text), new Map());
  try {
    const link = await waitFor(() => {
      const found = drawn.view.container.querySelector('a[href="/api/v1/issues/CORE-1"]');
      expect(found).not.toBeNull();
      return found;
    });
    expect(drawn.click(link), "the router followed a path the server answers").toBe(true);
    expect(drawn.where()).toBe("/issues/CORE-1");
  } finally {
    drawn.view.unmount();
  }
});

// The router's own `Link` replaces the reader's place when it points at that place, so Back
// leaves the page; a link in a row's words does the same, as a tie's chip to that address does.
test("a link in a row's words to the address the reader is at replaces it", async () => {
  const target = "dispatch://CORE-1/spec#b-r1";
  const text = `It follows ${target}.`;
  const drawn = drawWordsAt(
    "/issues/CORE-1/spec#b-r1",
    text,
    undefined,
    new Map([[target, "Draw the rows."]])
  );
  try {
    const link = await waitFor(() => {
      const found = drawn.view.container.querySelector(`a[data-row-reference="${target}"]`);
      expect(found).not.toBeNull();
      return found;
    });
    expect(drawn.click(link)).toBe(false);
    expect(drawn.where()).toBe("/issues/CORE-1/spec#b-r1");
    expect(drawn.how(), "a link to the reader's own place pushed a second entry").toBe("REPLACE");
  } finally {
    drawn.view.unmount();
  }
});

/** Lays every element out `height` px tall for the rows drawn until the answer is called: no
 *  DOM of a unit test lays anything out, and a cut, where one were drawn, reads the words' own
 *  boxes (`clamp-measurer.ts`'s `measureClamp`). */
function layOut(height: number): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return height;
    },
  });
  // On Element, which owns the method, for the reason the press case above gives.
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 0, 600, height)
  );
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
    box.mockRestore();
  };
}

// R11: a row of the outline draws its node's words whole, with no limit of lines: an agent's
// message and statement as much as a person's words, focused or not, and no row draws
// "more · N words". The page would measure the words of rows near the viewport, so each row's
// words are brought near and measured before the row is read.
test("a row draws its words whole, an agent's as much as a person's, and never more", async () => {
  const text = Array.from({ length: 3000 }, (_unused, at) => `word${at + 1}`).join(" ");
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  const person: Author = { kind: "person", name: "bob", reader: false };
  // 3,000 words over 400 lines of 24 px, which the six-line cut held back 394 lines of.
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  try {
    for (const [drawn, author] of [
      [node({ id: "message-1", text, type: "message" }), planner],
      [node({ id: "spec#b-r1", text, type: "requirement" }), planner],
      [node({ id: "comment-1", text, type: "comment" }), person],
    ] as const) {
      for (const focused of [false, true]) {
        const view = renderRow(drawn, { author, focused });
        try {
          await measureNearWords(near);
          const words = view.container.querySelector<HTMLElement>("[data-node-words]");
          expect(view.container.querySelector("[data-node-more]"), drawn.id).toBeNull();
          expect(words?.style.maxHeight, drawn.id).toBe("");
          expect(words?.textContent?.endsWith("word3000"), drawn.id).toBe(true);
        } finally {
          view.unmount();
        }
      }
    }
  } finally {
    near.restore();
    restore();
  }
});

// R16: an open question's options are the choices of its answer. A press on one leaves the
// focus where it is until the press ends, so it ends on the option it went down on, and only a
// question that waits on the reader draws them in the waiting colour.
test("an option's press keeps the focus, and only a waiting question's are in the waiting colour", async () => {
  const user = userEvent.setup();
  const picked: string[] = [];
  const writer = {
    answer: undefined,
    pick: (_node: TreeNode, label: string) => picked.push(label),
  } as unknown as OutlineWriter;
  const asked = (waiting: boolean) =>
    node({
      id: "ask-1",
      options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
      reference: "dispatch://CORE-1/ask/ask-1",
      state: "open",
      text: "At which width does the identity block condense?",
      type: "ask",
      waiting,
    });
  const view = render(<NodeOptions body={false} node={asked(true)} writer={writer} />);
  try {
    const option = view.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    expect(option).not.toBeNull();
    if (option === null) return;
    // The press is held: a mousedown that moved the focus would have put it on the control, and
    // through it on the row.
    await user.pointer({ keys: "[MouseLeft>]", target: option });
    expect(
      view.container.contains(document.activeElement),
      "the press moved the focus into the row"
    ).toBe(false);
    await user.pointer({ keys: "[/MouseLeft]" });
    expect(picked).toEqual(["1024 px"]);
    expect(option.className).toContain("text-page-waiting-text");
  } finally {
    view.unmount();
  }
  const other = render(<NodeOptions body={false} node={asked(false)} writer={writer} />);
  try {
    const option = other.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    expect(option?.className).toContain("text-page-quiet");
    expect(option?.className).not.toContain("text-page-waiting-text");
  } finally {
    other.unmount();
  }
});

/** One row the way the conversation's rows draw it: not focused until it takes the focus, under
 *  the page's writer. */
function RowFocusedOnFocus({ drawn }: { drawn: TreeNode }): ReactNode {
  const [focused, setFocused] = useState(false);
  return (
    <NodeRow
      author={{ kind: "agent", name: "Planner", reader: false }}
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onToggle={() => {}}
      ownAnswer={false}
      rowKey="row"
      tabStop
      tabStops
    />
  );
}

// Decision 120: a control a reader can see acts on its first press. An option drawn on a row
// that does not hold the focus is picked by that press, and the same press moves the focus to
// the row: the reader never presses twice.
test("a press on an option of an unfocused row picks it and focuses the row in one", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [
    ["asks", "CORE-1", "thread"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const asked = node({
    id: "ask-1",
    options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "open",
    text: "At which width does the identity block condense?",
    type: "ask",
    waiting: true,
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>
          <OutlineWriterProvider issueKey="CORE-1">
            <RowFocusedOnFocus drawn={asked} />
          </OutlineWriterProvider>
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    const row = view.container.querySelector<HTMLElement>("[data-node-row]");
    expect(row?.contains(document.activeElement)).toBe(false);
    const option = view.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    if (option === null) throw new Error("the unfocused row draws no option to press");
    await user.click(option);
    await waitFor(() => {
      expect(
        view.container.querySelector('[data-pick-option="1024 px"]')?.getAttribute("aria-pressed")
      ).toBe("true");
      expect(document.activeElement, "the press picked the option and left the row unfocused").toBe(
        row
      );
    });
  } finally {
    view.unmount();
  }
});

// Ruling 5: a child issue's row is one line - its title, then how it stands and what blocks it -
// and its key is the row's tag, which opens the child in the children's section above the outline
// (R9, R14) and links nowhere itself; it names no author and no time.
test("a child issue's row draws its title, its facts and its key as the tag", () => {
  const child: IssueChild = {
    active_at: "2026-11-05T12:00:00Z",
    external_links: null,
    key: "CORE-2",
    needs: ["CORE-3"],
    status: "backlog",
    subtree_done: 0,
    subtree_total: 0,
    title: "Draw the children in the outline",
  };
  const view = renderRow(node({ child, id: "CORE-2", text: child.title, type: "child_issue" }));
  try {
    expect(view.container.querySelector("[data-node-words] [data-author-text]")?.textContent).toBe(
      child.title
    );
    expect(view.container.querySelector("[data-child-facts]")?.textContent).toContain("CORE-3");
    const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
    expect(tag?.textContent).toBe("CORE-2");
    expect(tag?.querySelector("button[data-child-key]")?.textContent).toBe("CORE-2");
    expect(tag?.querySelector("a"), "the child's tag is a link").toBeNull();
    expect(view.container.querySelector("[data-node-author]")).toBeNull();
    expect(view.container.querySelector("[data-node-time]")).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The writer's acts a row draws for `drawn` (`[data-row-act]`), with the page's writer above it
 *  or with none, and whether it draws Copy reference (`[data-row-copy]`). The row holds no focus:
 *  its acts are drawn whatever the focus (R11). `ownAnswer` says the answer an ask carries is the
 *  reader's own, `reader` that the reader wrote the node, and `context` that the view draws the
 *  row as context. The writer's reads are answered before it mounts, under the keys the tree
 *  reads them by, so it makes no request. */
function rowControls(
  drawn: TreeNode,
  withWriter: boolean,
  {
    context = false,
    ownAnswer = false,
    reader = false,
  }: { context?: boolean; ownAnswer?: boolean; reader?: boolean } = {}
): { acts: string[]; copy: boolean } {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [
    ["asks", "CORE-1", "thread"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const row = (
    <NodeRow
      author={{ kind: "person", name: "bob", reader }}
      context={context}
      focused={false}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => {}}
      onToggle={() => {}}
      ownAnswer={ownAnswer}
      rowKey="row"
      tabStop
    />
  );
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>
          {withWriter ? (
            <OutlineWriterProvider issueKey="CORE-1">{row}</OutlineWriterProvider>
          ) : (
            row
          )}
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    return {
      acts: [...view.container.querySelectorAll("[data-row-act]")].map(
        (act) => act.getAttribute("data-row-act") ?? ""
      ),
      copy: view.container.querySelector("[data-row-copy]") !== null,
    };
  } finally {
    view.unmount();
  }
}

/** The writer's acts a row draws for `drawn` (`rowControls`). */
function rowActs(
  drawn: TreeNode,
  withWriter: boolean,
  options?: Parameters<typeof rowControls>[2]
): string[] {
  return rowControls(drawn, withWriter, options).acts;
}

// R7 and R11: every row, focused or not, draws Reply on a comment, a message and an ask
// in any state, and Answer on an open question; a statement, a check and a decision draw none,
// since today's model holds no reply to a block; and with no writer above it the row draws no
// act at all.
test("Reply is drawn on each node that takes one, Answer on an open question, and no action without a writer", () => {
  const by = { id: "bob", kind: "user" };
  const turn = (type: "ask" | "comment" | "message", id: string, state?: TreeNode["state"]) =>
    node({
      author: by,
      id,
      reference: `dispatch://CORE-1/${type}/${id}`,
      ...(state === undefined ? {} : { state }),
      type,
    });
  expect(rowActs(turn("comment", "comment-1"), true)).toEqual(["reply"]);
  expect(rowActs(turn("message", "message-1"), true)).toEqual(["reply"]);
  expect(rowActs(turn("ask", "ask-1", "answered"), true)).toEqual(["reply"]);
  expect(rowActs(turn("ask", "ask-2", "resolved"), true)).toEqual(["reply"]);
  expect(rowActs(turn("ask", "ask-3", "open"), true)).toEqual(["answer", "reply"]);
  const block = (type: NodeType, id: string) =>
    node({ author: by, id, reference: `dispatch://CORE-1/spec#b-${id}`, type });
  expect(rowActs(block("requirement", "r1"), true)).toEqual([]);
  expect(rowActs(block("check", "c1"), true)).toEqual([]);
  expect(rowActs(block("decision", "d1"), true)).toEqual([]);
  expect(rowActs(turn("ask", "ask-4", "open"), false)).toEqual([]);
  expect(rowActs(turn("comment", "comment-2"), false)).toEqual([]);
});

// R11: the reader's own comment carries Edit beside its Reply, and a comment of anyone else's
// none; a message's context row - a message under which a node it produced stands, which
// Conversation draws as context (R9) - carries no Reply, since its thread is replied to in
// Threads, while a message drawn as no context keeps it.
test("the reader's own comment carries Edit, and a message's context row no Reply", () => {
  const by = { id: "bob", kind: "user" };
  const comment = node({
    author: by,
    id: "comment-1",
    reference: "dispatch://CORE-1/comment/comment-1",
    type: "comment",
  });
  expect(rowActs(comment, true, { reader: true })).toEqual(["reply", "edit"]);
  expect(rowActs(comment, true)).toEqual(["reply"]);
  const thread = node({
    author: by,
    children: [node({ id: "spec#b-r9", type: "requirement" })],
    id: "message-1",
    reference: "dispatch://CORE-1/message/message-1",
    type: "message",
  });
  expect(rowActs(thread, true, { context: true })).toEqual([]);
  expect(rowActs(thread, true)).toEqual(["reply"]);
});

// Part G's R7: Change answer is drawn on an answered question whose answer is the reader's own,
// beside its Reply, and on no other answered question.
test("Change answer is drawn on an answered question the reader answered, and on no other", () => {
  const answered = node({
    author: { id: "bob", kind: "user" },
    id: "ask-1",
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "answered",
    type: "ask",
  });
  expect(rowActs(answered, true, { ownAnswer: true })).toEqual(["change-answer", "reply"]);
  expect(rowActs(answered, true)).toEqual(["reply"]);
  expect(rowActs(answered, false, { ownAnswer: true })).toEqual([]);
});

// R6: a row offers Copy reference on a node that has an address, with a writer above it
// or with none, since it writes nothing; a node with no address draws none.
test("Copy reference is drawn on a node with an address, and on none without", () => {
  const comment = (reference?: string) =>
    node({
      author: { id: "bob", kind: "user" },
      id: "comment-1",
      ...(reference === undefined ? {} : { reference }),
      type: "comment",
    });
  expect(rowControls(comment("dispatch://CORE-1/comment/comment-1"), true).copy).toBe(true);
  expect(rowControls(comment("dispatch://CORE-1/comment/comment-1"), false).copy).toBe(true);
  expect(rowControls(comment(), true).copy).toBe(false);
});

// R3 of dispatch://INFRA-1523: Dispatch takes no reply to a comment or a message its author
// deleted (409 `POST_DELETED`), so its row draws no Reply. Copy reference still stands, since it
// writes nothing.
test("a deleted comment's or message's row draws no Reply, and still Copy reference", () => {
  for (const type of ["comment", "message"] as const) {
    const deleted = deletedTurn(UNTITLED, {
      id: `${type}-1`,
      reference: `dispatch://CORE-1/${type}/${type}-1`,
      type,
    });
    expect(rowControls(deleted, true), type).toEqual({ acts: [], copy: true });
  }
});

// Part G's R16: while a send of the line's own is out, a press that would set the line to
// something else is refused, as a Reply is, so the line keeps the reply that send answers.
test("while the line's send is out, Answer, an option, Edit and the issue's Reply keep its reply", () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  for (const key of [
    ["asks", "CORE-1", "thread"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const reply: ReplyTarget = {
    actor: { id: "bob", kind: "user" },
    excerpt: "Earlier comment",
    id: "comment-1",
    parentKind: "comment",
  };
  const request: SentRequest = {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body: "A reply on its way", mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: "CORE-1", kind: "issue" },
    plainDelivery: "steer",
    replyTo: reply,
    sendKey: "send-1",
  };
  const asked = node({
    id: "ask-1",
    options: [{ label: "1024 px" }],
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "open",
    type: "ask",
  });
  // The send stays out: Dispatch never answers it.
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const seen: { writer?: OutlineWriter } = {};
  function Probe(): ReactNode {
    seen.writer = useOutlineWriter();
    return null;
  }
  try {
    act(() => {
      heldSends(queryClient).send(["issue-composer", "CORE-1"], request);
    });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <NoTooltips>
            <OutlineWriterProvider issueKey="CORE-1">
              <Probe />
            </OutlineWriterProvider>
          </NoTooltips>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      expect(seen.writer?.sendKey).toEqual(["issue-composer", "CORE-1"]);
      expect(seen.writer?.reply).toEqual(reply);
      act(() => seen.writer?.pick(asked, "1024 px"));
      act(() => seen.writer?.answerNode(asked));
      act(() => seen.writer?.edit("comment-2", "My own words"));
      act(() => seen.writer?.writeOnIssue());
      act(() => seen.writer?.clear());
      expect(seen.writer?.reply).toEqual(reply);
      expect(seen.writer?.answer).toBeUndefined();
      expect(seen.writer?.editing).toBeUndefined();
    } finally {
      view.unmount();
    }
  } finally {
    createComment.mockRestore();
  }
});

/** A focused row whose box the browser lays out at `top`, 24 px tall, placed by the hook every
 *  row of the outline places itself with, by its default step or by `below`. */
function FocusedRowAt({ below, top }: { below?: number; top: number }): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  useRowOnScreen(row, true, below);
  return (
    <div
      ref={(element) => {
        row.current = element;
        if (element !== null) element.getBoundingClientRect = () => new DOMRect(0, top, 320, 24);
      }}
    />
  );
}

// Decision 113: a focused row stands below everything that stays at the top of the screen. At
// 760 px and below the view row stays there, and the page's `scroll-padding-top` is its height
// (`styles.css`, `slots/view-row.tsx`), while a header that publishes no band of its own leaves
// `bringOnScreen` nothing to clear: so a focused row 10 px from the top lay under the view row.
// It is scrolled to 8 px below it, and a row already below it is left where it is.
test("a focused row under the view row that stays is placed below it, and one below it is left", () => {
  document.documentElement.style.setProperty("scroll-padding-top", "44px");
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const under = render(<FocusedRowAt top={10} />);
    expect(
      scrollBy,
      "the focused row was left under the view row that stays"
    ).toHaveBeenCalledTimes(1);
    expect(scrollBy).toHaveBeenCalledWith({ top: 10 - 44 - 8 });
    under.unmount();
    scrollBy.mockClear();
    render(<FocusedRowAt top={60} />).unmount();
    expect(scrollBy, "a row already below the view row was scrolled").not.toHaveBeenCalled();
  } finally {
    scrollBy.mockRestore();
    document.documentElement.style.removeProperty("scroll-padding-top");
  }
});

// A view that places its rows by a step of its own passes it, and a focused row
// under what stays is placed that far below it rather than 8 px.
test("a focused row under the view row that stays is placed the step it is given below it", () => {
  document.documentElement.style.setProperty("scroll-padding-top", "44px");
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    render(<FocusedRowAt below={20} top={10} />).unmount();
    expect(scrollBy).toHaveBeenCalledTimes(1);
    expect(scrollBy).toHaveBeenCalledWith({ top: 10 - 44 - 20 });
  } finally {
    scrollBy.mockRestore();
    document.documentElement.style.removeProperty("scroll-padding-top");
  }
});

// Decision 141: a press on Keep open or Let close leaves the focus where it is, as the row's
// other press controls do. Under part I's press deferral, Let close on a pinned row that does
// not hold the focus took it, closed the body under the pointer and dropped the focus to the page.
test("a press on Keep open or Let close leaves the focus where it is", async () => {
  const user = userEvent.setup();
  for (const pinned of [true, false]) {
    const pressed: boolean[] = [];
    const view = render(
      <>
        <button type="button">Held</button>
        <KeepOpen onPin={() => pressed.push(true)} pinned={pinned} tabStops={false} />
      </>
    );
    try {
      const held = view.getByRole("button", { name: "Held" });
      act(() => held.focus());
      const control = view.getByRole("button", {
        name: pinned ? CONTROL_LABELS.let_close : CONTROL_LABELS.keep_open,
      });
      await user.pointer({ keys: "[MouseLeft>]", target: control });
      expect(document.activeElement, `a press on ${control.textContent} took the focus`).toBe(held);
      await user.pointer({ keys: "[/MouseLeft]", target: control });
      expect(pressed, `the press on ${control.textContent} did nothing`).toEqual([true]);
      expect(document.activeElement).toBe(held);
    } finally {
      view.unmount();
    }
  }
});
