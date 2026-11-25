import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  type RenderResult,
  render,
  renderHook,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";
import { MemoryRouter, useLocation, useNavigationType } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { graphNodeFields } from "../../__tests__/graph-fixture";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { api } from "../../api/client";
import type { Actor, Agent, DisplayTree, IssueChild, ReferenceTarget } from "../../api/types";
import { pageKindBullet, pageKindTag } from "../../theme/classes";
import type { ReplyTarget } from "../conversation/composer-model";
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { actorLabelText } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import * as markdownEngine from "../refs/markdown-engine";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  NOTHING_PUBLISHED,
  PublishedReferenceNames,
  type PublishedReferences,
  ReferenceNamesProvider,
  usePublishReferenceNames,
} from "../refs/reference-names";
import { measureNearWords } from "./clamp-fixtures";
import {
  artifact,
  DECISIONS_DOCUMENT,
  decision,
  decisionsRead,
  issueDetails,
  KEY,
  shaped,
} from "./decisions-filter/decisions-fixtures";
import {
  FIRST_LINE_LIMIT,
  type IssueTreeReads,
  type NodeChip,
  type ParentLine,
  type TreeNode,
  targetNaming,
} from "./issue-tree";
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
  NODE_TYPE_NAMES,
  type NodeType,
  PRESET_NAMES,
  wordsIn,
} from "./page-words";
import { publishedChips, publishedNames } from "./projection-fixture";
import { RowText } from "./RowText";
import { ProvidersSlot } from "./slots/providers";
import { treeContext } from "./tree-drafts";
import { nodesInOrder } from "./tree-keys";
import { treeDocumentIds, useIssueTree } from "./useIssueTree";
import { overflowing, wordsScrollRules, wordsScrollSheet } from "./words-scroll-fixtures";

/**
 * What one row of the tree draws, read as a reader reads it (R1, R6, R9,
 * R34, and the architect's ruling 1 on R1).
 *
 * The row is rendered on its own rather than through `IssueTree`, because what these rows are
 * about is the words and the shapes one node draws: the tree's fold, its keys and its placements
 * are read in `IssueTree.test.tsx` and `issue-tree.test.ts`. Where a row is cut is read from the
 * browser's own layout, which no DOM of a unit test lays out, so the cut is read by the browser
 * suite's c-row.
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

/** The parties Dispatch knows, as a party's own read finds them (`useReferenceTarget`): a
 *  session the registry lists, a role a listed session holds, and a person's login. */
interface Parties {
  readonly login?: string;
  readonly role?: string;
  readonly session?: Agent;
}

/** Answers the registry's and the people's reads with `parties`, as the shell has read them. */
function knowParties(queryClient: QueryClient, { login, role, session }: Parties): void {
  const agents = [
    ...(session === undefined ? [] : [session]),
    ...(role === undefined
      ? []
      : [{ roles: [role], session_id: "role-holder", title: "Holder" } as unknown as Agent]),
  ];
  queryClient.setQueryData(["agents"], agents);
  if (login !== undefined) queryClient.setQueryData(["users"], [{ login }]);
}

/** Publishes `published` to the names store while it is drawn, and hands it to what it draws in
 *  the render that draws it, as the page publishes its one batched read and hands it its rows
 *  (`useIssueTree.ts`, `IssueTree.tsx`); nothing where it is undefined. */
function Named({
  children,
  published,
}: {
  children: ReactNode;
  published: PublishedReferences | undefined;
}): ReactNode {
  usePublishReferenceNames(published);
  return (
    <PublishedReferenceNames published={published ?? NOTHING_PUBLISHED}>
      {children}
    </PublishedReferenceNames>
  );
}

/** One row, drawn with its body open, as the focused row draws it, unless `body` is false;
 *  `author` is who made it, as the tree names them (`AgentText.author`); `answerer` the person
 *  whose answer an ask carries; `expanded` and `folded` its fold, for a node that holds any;
 *  `focused` whether it holds the tree's focus. It is drawn inside `NoTooltips`, as the page's
 *  slots draw every row, so an author is named as the page names one, and under the names
 *  store, with `published` published to it where it is given, as the page publishes its batched
 *  read. `redraw` draws the same row of another node, as a later read of the tree does;
 *  `publish` publishes another value, as a later answer of the batched read does. */
function renderRow(
  drawn: TreeNode,
  {
    answerer,
    author = NOBODY_RECORDED,
    body = true,
    editable,
    expanded,
    focused = false,
    folded,
    parties,
    published,
  }: {
    answerer?: string;
    author?: Author;
    body?: boolean;
    editable?: boolean;
    expanded?: boolean;
    focused?: boolean;
    folded?: number;
    parties?: Parties;
    published?: PublishedReferences;
  } = {}
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (parties !== undefined) knowParties(queryClient, parties);
  const row = (node: TreeNode, names: PublishedReferences | undefined) => (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named published={names}>
            <NoTooltips>
              <NodeRow
                {...(answerer === undefined ? {} : { answerer })}
                author={author}
                body={body}
                context={false}
                {...(editable === undefined ? {} : { editable })}
                {...(expanded === undefined ? {} : { expanded })}
                {...(folded === undefined ? {} : { folded })}
                focused={focused}
                isClosed={false}
                level={1}
                node={node}
                onFocus={() => {}}
                onPin={() => {}}
                onToggle={() => {}}
                ownAnswer={false}
                pinned={false}
                rowKey="row"
                tabStop={false}
              />
            </NoTooltips>
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  let shown = drawn;
  let publishedNow = published;
  const view = render(row(drawn, published));
  return Object.assign(view, {
    publish: (next: PublishedReferences) => {
      publishedNow = next;
      view.rerender(row(shown, publishedNow));
    },
    queryClient,
    redraw: (next: TreeNode) => {
      shown = next;
      view.rerender(row(shown, publishedNow));
    },
  });
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R9: a node whose author the model holds says who made it by a name and a shape, never by
// colour alone and never by an id, and two authors of different kinds never share a shape.
// Ruling 5: a node the model holds no author for draws none on its row, and its opened body
// says so. Ruling 6: an agent is named by its role's word, and its opened body gives its whole
// title.
test("an author is drawn by a name and a shape of its own kind", () => {
  // A comment, which a person and an agent both write: a decision's body draws Keep and
  // Change, which are writes, and this row is about the words and the shape alone.
  const drawn = (author: Author, body = true) => {
    const view = renderRow(node({ id: "comment-1", type: "comment" }), { author, body });
    try {
      const shown = view.container.querySelector("[data-node-author]");
      const shape = shown?.querySelector("svg[data-glyph]");
      return {
        body: shown === null ? null : shown.closest("[data-node-body]") !== null,
        chrome: shown === null ? null : shown.querySelector("[data-page-chrome]") !== null,
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
    body: false,
    chrome: false,
    margin: true,
    name: "Written by a person",
    role: "img",
    text: "alice",
    title: null,
    token: "author-person",
  });
  expect(drawn({ kind: "agent", name: "Planner", reader: false })).toEqual({
    body: false,
    chrome: false,
    margin: true,
    name: "Written by an agent",
    role: "img",
    text: "Planner",
    title: null,
    token: "author-agent",
  });
  // An agent whose session's title says more than its role is named by the role's word on the
  // row, and the opened body gives the whole title; a row that is not open gives none.
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
  expect(drawn(architect, false)).toMatchObject({ margin: true, text: "architect", title: null });
  // Where the model holds no author the row draws none, and its opened body says so in the
  // page's own words, which are marked as the page's rather than as somebody's name, with a
  // shape neither other kind draws.
  expect(drawn(NOBODY_RECORDED, false)).toMatchObject({ margin: null, text: null });
  expect(drawn(NOBODY_RECORDED)).toEqual({
    body: true,
    chrome: true,
    margin: false,
    name: "Written by an author nobody recorded",
    role: "img",
    text: "Author unknown",
    title: null,
    token: "author-unknown",
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
        folded ? { body: false, expanded: false, folded: 1 } : { body: false }
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
      { author: planner, body: false, ...(folded ? { expanded: false, folded: 1 } : {}) }
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
      { body: false }
    );
  const deleted = under({
    deletedAt: DELETED_AT,
    deletedBy: UNTITLED,
    id: "comment-1",
    reference: "dispatch://CORE-1/comment/comment-1",
    text: `Deleted by ${actorLabelText(UNTITLED)} · ${DELETED_AT}`,
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
    reference: "dispatch://CORE-1/message/message-1",
    text: "Ship it.",
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
      folded ? { body: false, expanded: false, folded: 1 } : { body: false }
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
    { author: { kind: "person", name: "alice", reader: false }, body: false }
  );
  try {
    shown.edited = chrome(edited.container.querySelector("[data-node-edited]"));
  } finally {
    edited.unmount();
  }
  const under = (line: ParentLine) =>
    renderRow(
      node({ id: "plan-md#b-t1", parentLine: line, text: "Stand it up.", type: "plan_task" }),
      {
        body: false,
      }
    );
  const deletedParent = under({
    deletedAt: DELETED_AT,
    deletedBy: alice,
    id: "comment-1",
    reference: "dispatch://CORE-1/comment/comment-1",
    text: `Deleted by alice · ${DELETED_AT}`,
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
    reference: "dispatch://CORE-1/message/message-1",
    text: "Ship it.",
    type: "message",
  });
  try {
    shown.editedParent = chrome(editedParent.container.querySelector("[data-node-parent-line] a"));
  } finally {
    editedParent.unmount();
  }
  const markedChips: NodeChip[] = [
    {
      answered: true,
      deletedAt: DELETED_AT,
      deletedBy: UNTITLED,
      gone: false,
      reading: { words: `Deleted by ${actorLabelText(UNTITLED)}` },
      ref: "dispatch://CORE-1/comment/c-deleted",
    },
    {
      answered: true,
      editedAt: "2026-11-05T13:00:00Z",
      gone: false,
      reading: { words: "Ship it." },
      ref: "dispatch://CORE-1/message/m-edited",
    },
  ];
  const chips = renderRow(
    node({ chips: markedChips, id: "comment-9", text: "Read these.", type: "comment" }),
    { body: false, published: publishedChips(markedChips) }
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

// R1, ruling 1: a folded branch is the one place a row draws one line of its node: the first
// line, cut at its first sentence end, and how many nodes it holds. The focus draws its words
// whole, and keeps the count of what the fold still hides after them.
test("a folded branch draws its first line and its count, and its focus its words", () => {
  const text = "The page draws the whole issue in one reading. Nothing is lost on the way.";
  const folded = node({ held: 3, id: "comment-1", text, type: "comment" });
  const shut = renderRow(folded, { body: false, expanded: false, folded: 3 });
  try {
    // A line that is a whole sentence ends on the sentence's own mark, with no ellipsis after it.
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
  const focused = renderRow(folded, { body: true, expanded: false, folded: 3 });
  try {
    expect(focused.container.querySelector("[data-node-first-line]")).toBeNull();
    expect(
      focused.container.querySelector("[data-node-words] [data-author-text]")?.textContent
    ).toBe(text);
    expect(
      focused.container.querySelector("[data-node-words] [data-node-count]")?.textContent,
      "a focused folded branch no longer says how many rows it hides"
    ).toBe(`3 ${CONTROL_LABELS.folded_rows}`);
  } finally {
    focused.unmount();
  }
});

/** The reading of `markdown` the server would send (`displayTree`), which the engine always
 *  gives for the plain words these rows write. */
function readingOf(markdown: string): DisplayTree {
  const tree = displayTree(markdown);
  if (tree === undefined) throw new Error(`the engine read no tree of "${markdown}"`);
  return tree;
}

/** The reads `view`'s query client holds that name `reference`: what a line that read its own
 *  target for it would have made. */
function readsOf(view: { queryClient: QueryClient }, reference: string): unknown[] {
  const id = reference.replace("dispatch://", "");
  return view.queryClient
    .getQueryCache()
    .getAll()
    .map((query) => query.queryKey)
    .filter((key) => JSON.stringify(key).includes(id.split("/")[0] ?? id));
}

// issue page decision 350, which replaces decision 95's short form: a folded branch's one line
// reads a reference in its words as the shared projection reads one (`usePlainHeadline`), the
// way the row's own words read it (`RowText.tsx`): as the words its author wrote while the
// page's batched read has not named it - with no read of its own made for a reference the page
// is reading - and as its target's name once the page publishes one. The line still ends where
// the node's first sentence does (R1).
test("a folded branch's line reads a reference as its target's name, and as its author's words until it resolves", async () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const text = `Read ${address} before the rows. Then the rest.`;
  const folded = node({ held: 1, id: "comment-1", text, tree: readingOf(text), type: "comment" });
  const shut = renderRow(folded, {
    body: false,
    expanded: false,
    folded: 1,
    published: publishedNames(new Map(), { asked: [address] }),
  });
  try {
    const line = () => shut.container.querySelector("[data-node-first-line]")?.textContent;
    await waitFor(() => expect(line()).toBe(`Read ${address} before the rows.`));
    expect(readsOf(shut, address), "the line read a target the page is reading").toEqual([]);
    act(() => shut.publish(publishedNames(new Map([[address, "Draw the rows"]]))));
    await waitFor(() => expect(line()).toBe("Read Draw the rows before the rows."));
    expect(readsOf(shut, address)).toEqual([]);
  } finally {
    shut.unmount();
  }
});

// R1 over issue page decision 350: a folded branch's one line is cut by R1's first-line rule
// over the words the projection draws, so a name that lands after the line was first drawn cuts
// it again. A first sentence that fits the line with its address as written runs past R1's limit
// with its target's name in its place - a name the page cuts to a chip's 32 characters - and is
// then cut at a word, with the ellipsis, never through a word or past the line's room.
test("a folded branch's line is cut again over a name that lands after it is drawn", async () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const name = "Stand the harness up before rows";
  const sentence = (reference: string) =>
    `Read ${reference} and then keep every one of the rows on the page as it draws them.`;
  const text = `${sentence(address)} Then the rest.`;
  expect(sentence(address).length).toBeLessThanOrEqual(FIRST_LINE_LIMIT);
  expect(sentence(name).length).toBeGreaterThan(FIRST_LINE_LIMIT);
  const folded = node({ held: 1, id: "comment-1", text, tree: readingOf(text), type: "comment" });
  const shut = renderRow(folded, {
    body: false,
    expanded: false,
    folded: 1,
    published: publishedNames(new Map(), { asked: [address] }),
  });
  try {
    const line = () => shut.container.querySelector("[data-node-first-line]")?.textContent ?? "";
    await waitFor(() => expect(line()).toBe(sentence(address)));
    act(() => shut.publish(publishedNames(new Map([[address, name]]))));
    await waitFor(() => expect(line().endsWith("…")).toBe(true));
    const kept = line().slice(0, -1);
    expect(kept.startsWith(`Read ${name} `)).toBe(true);
    expect(kept.length).toBeLessThanOrEqual(FIRST_LINE_LIMIT);
    expect(sentence(name).startsWith(kept), "the line was not cut from the named words").toBe(true);
    expect(sentence(name).charAt(kept.length), "the line was cut through a word").toBe(" ");
  } finally {
    shut.unmount();
  }
});

// Decisions 66 and 88: a chip's name and a node's one line are plain words, read with no list
// or block reading but for an ordered list's number, which the shared projection writes as a
// rendered body draws it. A step stored as `1. Stand the harness up.` is a list of one item: a
// chip named from the reading the page publishes for it, and the folded line of a node whose
// words open with it, each keep its number.
test("a numbered step keeps its number in a chip and in a folded line", async () => {
  const chips: NodeChip[] = [
    {
      answered: true,
      gone: false,
      label: "Needs",
      reading: { tree: readingOf("1. Stand the harness up.") },
      ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
    },
    {
      answered: true,
      gone: false,
      reading: { tree: readingOf("2. Stand the harness up and read the rows") },
      ref: "dispatch://CORE-1/artifact/plan-md#b-t3",
    },
  ];
  const view = renderRow(
    node({ chips, id: "plan-md#b-t2", text: "Draw the rows.", type: "plan_task" }),
    { body: false, published: publishedChips(chips) }
  );
  try {
    await waitFor(() =>
      expect(
        [...view.container.querySelectorAll("[data-node-chip]")].map((chip) => chip.textContent)
      ).toEqual(["Needs 1. Stand the harness up.", "2. Stand the harness up and…"])
    );
  } finally {
    view.unmount();
  }
  const text = "1. Stand the harness up. Run it.";
  const shut = renderRow(
    node({ held: 1, id: "plan-md#b-t1", text, tree: readingOf(text), type: "plan_task" }),
    { body: false, expanded: false, folded: 1 }
  );
  try {
    await waitFor(() =>
      expect(shut.container.querySelector("[data-node-first-line]")?.textContent).toBe(
        "1. Stand the harness up."
      )
    );
  } finally {
    shut.unmount();
  }
});

// R8 over ruling 5: the page publishes a target's name as a link to the target reads it - a
// comment's words within a link's 60 characters - as every surface reads the target by it, and
// a chip cuts it to a chip's room where it draws it. So the plain line a row draws beside the
// chip - here its parent's line - reads the target by that name, and only the chip by the cut.
test("a plain line beside a chip reads the chip's target by the name a link reads, not the chip's cut", async () => {
  const target = "dispatch://CORE-1/comment/c9";
  const whole = "Hold the deploy until the plan lands and the gate is green";
  const chip: NodeChip = {
    answered: true,
    gone: false,
    reading: { tree: readingOf(whole) },
    ref: target,
  };
  const words = `Wait on ${target} first.`;
  const view = renderRow(
    node({
      chips: [chip],
      id: "plan-md#b-t1",
      parentLine: {
        id: "spec#b-r2",
        reference: "dispatch://CORE-1/spec#b-r2",
        text: words,
        tree: readingOf(words),
        type: "requirement",
      },
      text: "Draw the rows.",
      type: "plan_task",
    }),
    { body: false, published: publishedChips([chip]) }
  );
  try {
    await waitFor(() => {
      expect(view.container.querySelector("[data-node-parent-line] a")?.textContent).toBe(
        `Wait on ${whole} first.`
      );
      expect(view.container.querySelector("[data-chip-name]")?.textContent).toBe(
        "Hold the deploy until the plan…"
      );
    });
  } finally {
    view.unmount();
  }
});

// The architect's ruling on block names, by R8: a block reads in a plain line as a link to it
// reads it - the headline of its whole text, cut to a link's 60 characters - so the page
// publishes the block's whole reading (`tree-drafts.ts`'s `treeContext`). A chip of the block
// keeps G's own rule (decisions 66 and 88): it is named by the first block of its words that
// holds any, here the decision's heading, from the reading it carries. Both are read from the
// one batched read's answer for the block, through the page's own code.
test("a two-paragraph block reads alike in a plain line and a link, and its chip reads its heading", async () => {
  const block = "dispatch://CORE-2/spec#b-r1";
  const text = "## Keep it\n\nKeep the model pure, and draw every row from the one tree it builds.";
  const title = "Keep it Keep the model pure, and draw every row from the one…";
  const reads = {
    issue: issueDetails(),
    targets: [{ gone: false, kind: "block", name: text, ref: block, text_tree: readingOf(text) }],
  } satisfies IssueTreeReads;
  const chip = treeContext(reads).chipOf(block);
  const words = `Serve ${block} first.`;
  const view = renderRow(
    node({
      chips: [chip],
      id: "plan-md#b-t1",
      parentLine: {
        id: "spec#b-r2",
        reference: "dispatch://CORE-1/spec#b-r2",
        text: words,
        tree: readingOf(words),
        type: "requirement",
      },
      text: "Draw the rows.",
      type: "plan_task",
    }),
    { body: false, published: { ...targetNaming(reads), asked: new Set([block]) } }
  );
  // The link to the block, which reads the block itself, as every rendered body's does.
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: {
      ...graphNodeFields(),
      block_id: "r1",
      document: "dispatch://CORE-2/spec",
      id: "CORE-2/spec#r1",
      issue_key: "CORE-2",
      kind: "block",
      project: "CORE",
      ref: block,
      text,
      text_tree: readingOf(text),
    },
  });
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-2",
    project: "CORE",
    status: "in_progress",
    title: "Drain the queue",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const body = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <MarkdownBody markdown={words} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await within(body.container).findByRole("link", { name: title });
    await waitFor(() => {
      expect(view.container.querySelector("[data-node-parent-line] a")?.textContent).toBe(
        `Serve ${title} first.`
      );
      expect(view.container.querySelector("[data-chip-name]")?.textContent).toBe("Keep it");
    });
  } finally {
    getReferences.mockRestore();
    getIssueSummary.mockRestore();
    body.unmount();
    view.unmount();
  }
});

// R8: the page publishes a whole document's name as a link to it reads it - the issue's spec by
// the issue's title, and any other document by its own name, its file name - so a plain line
// reads the document as a link to it does, whatever word the batched read or the page has for
// it. Every chip keeps the page's word for the document, the word for the part it plays, since a
// chip never draws a document's file name (decision 39): a tie's, and one in place in a node's
// words, which the names store feeds. All of it is read from the one batched read's answer,
// through the page's own code.
test("a whole document reads alike in a plain line and a link, and by the page's word in every chip", async () => {
  const spec = `dispatch://${KEY}/spec`;
  const notes = `dispatch://${KEY}/artifact/notes-md`;
  const decisions = `dispatch://${KEY}/artifact/${DECISIONS_DOCUMENT.slug}`;
  const title = "Draw one ordered body";
  const notesFile = { ...artifact("artifact-notes", "notes-md"), name: "notes.md" };
  const decisionsFile = { ...DECISIONS_DOCUMENT, name: "decisions.md" };
  const issue = issueDetails();
  const held = (issue.artifacts ?? []).filter((one) => one.id !== decisionsFile.id);
  const reads = {
    issue: { ...issue, artifacts: [...held, notesFile, decisionsFile], title },
    // The batched read names a document by its file name, the spec's included.
    targets: [
      { gone: false, kind: "artifact", name: "spec.md", ref: spec },
      { gone: false, kind: "artifact", name: notesFile.name, ref: notes },
      { gone: false, kind: "artifact", name: decisionsFile.name, ref: decisions },
    ],
  } satisfies IssueTreeReads;
  const context = treeContext(reads);
  const words = `Read ${spec}, ${notes} and ${decisions} first.`;
  const view = renderRow(
    node({
      chips: [context.chipOf(spec), context.chipOf(notes), context.chipOf(decisions)],
      id: "plan-md#b-t1",
      parentLine: {
        id: "spec#b-r2",
        reference: `dispatch://${KEY}/spec#b-r2`,
        text: words,
        tree: readingOf(words),
        type: "requirement",
      },
      text: words,
      tree: readingOf(words),
      type: "plan_task",
    }),
    {
      body: false,
      published: { ...targetNaming(reads), asked: new Set([spec, notes, decisions]) },
    }
  );
  // The links, which read the issue's summary, as every rendered body's do.
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [notesFile, decisionsFile].map((file) => ({
      id: file.id,
      kind: "doc",
      latest_version: 1,
      name: file.name,
      primary: false,
      slug: file.slug,
    })),
    key: KEY,
    project: "CORE",
    status: "todo",
    title,
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const body = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <MarkdownBody markdown={words} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  const pageWords = [PRESET_NAMES.spec, NODE_TYPE_NAMES.document, PRESET_NAMES.decisions];
  const inWords = (reference: string) =>
    view.container.querySelector(`[data-node-words] a[data-row-reference="${reference}"]`)
      ?.textContent;
  try {
    for (const name of [title, notesFile.name, decisionsFile.name]) {
      await within(body.container).findByRole("link", { name });
    }
    await waitFor(() => {
      expect(view.container.querySelector("[data-node-parent-line] a")?.textContent).toBe(
        `Read ${title}, ${notesFile.name} and ${decisionsFile.name} first.`
      );
      expect([inWords(spec), inWords(notes), inWords(decisions)]).toEqual(pageWords);
      expect(
        [...view.container.querySelectorAll("[data-chip-name]")].map((chip) => chip.textContent)
      ).toEqual(pageWords);
    });
  } finally {
    getIssueSummary.mockRestore();
    body.unmount();
    view.unmount();
  }
});

// R8 over the row above, for another issue's spec cited alone: a link titles it by that issue's
// title, which the batched read answers only under the issue's own address, so the page asks its
// read for the issue beside the spec (`useIssueTree.ts`'s `treeReferences`). A plain line the
// page's read feeds then reads the spec as a line's own read and a link do, and every chip still
// draws the page's word for another issue's document. The names come from the page's own reads,
// whose names read answers each address it is asked for, as the server does, and no other.
test("another issue's spec reads alike in a line the page feeds, a line's own read and a link, and by the page's word in every chip", async () => {
  const other = "dispatch://CORE-9";
  const spec = `${other}/spec`;
  const title = "Other issue title";
  const words = `Wait for ${spec} to be approved.`;
  const issue = issueDetails();
  // Every read of the page but the names', answered: a decision of the issue ties to the spec,
  // and its words cite it.
  const reads = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tie = shaped(spec);
  const made = decision("d1", {
    links: [{ ...tie, to: { ...tie.to, kind: "artifact" } }],
    text: words,
  });
  reads.setQueryData(["whoami"], { kind: "user", login: "alice" });
  reads.setQueryData(["issue", KEY, "coverage"], {
    gaps: [],
    has_plan: false,
    issue: KEY,
    requirements: [],
    spec_artifact_id: "",
  });
  reads.setQueryData(["issue", KEY, "results"], {
    checks: [],
    issue: KEY,
    verification_artifact_id: "",
  });
  reads.setQueryData(["decisions", KEY], decisionsRead([made]));
  for (const queryKey of [
    ["asks", KEY],
    ["comments", KEY],
    ["issue-messages", KEY],
  ]) {
    reads.setQueryData(queryKey, []);
  }
  for (const id of treeDocumentIds(issue)) reads.setQueryData(["artifact", id, "blocks"], []);
  // The names read names the spec by its file name and the issue by its title.
  const getReferenceTargets = spyOn(api, "getReferenceTargets").mockImplementation(
    async (refs) => ({
      targets: refs.flatMap((ref): ReferenceTarget[] => {
        if (ref === spec) return [{ gone: false, kind: "artifact", name: "spec.md", ref }];
        return ref === other ? [{ gone: false, kind: "issue", name: title, ref }] : [];
      }),
    })
  );
  // The link and a line's own read, which read the other issue's summary.
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-9",
    project: "CORE",
    status: "todo",
    title,
  });
  const page = renderHook(() => useIssueTree(issue), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={reads}>{children}</QueryClientProvider>
    ),
  });
  const drawn: RenderResult[] = [];
  try {
    await waitFor(() => expect(page.result.current.namesHeld).toBe(true));
    const { published, root } = page.result.current;
    const row = node({
      chips: nodesInOrder(root).find((one) => one.type === "decision")?.chips ?? [],
      id: "plan-md#b-t1",
      parentLine: {
        id: "spec#b-r2",
        reference: `dispatch://${KEY}/spec#b-r2`,
        text: words,
        tree: readingOf(words),
        type: "requirement",
      },
      text: words,
      tree: readingOf(words),
      type: "plan_task",
    });
    // The row under the page's names, and the same row under none, whose line reads its own.
    const fed = renderRow(row, { body: false, published });
    const own = renderRow(row, { body: false });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const body = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <MarkdownBody markdown={words} />
        </MemoryRouter>
      </QueryClientProvider>
    );
    drawn.push(fed, own, body);
    await within(body.container).findByRole("link", { name: title });
    await waitFor(() => {
      expect(
        [fed, own].map(
          (view) => view.container.querySelector("[data-node-parent-line] a")?.textContent
        ),
        "the line the page's read feeds, then a line's own read"
      ).toEqual([`Wait for ${title} to be approved.`, `Wait for ${title} to be approved.`]);
      expect(
        fed.container.querySelector(`[data-node-words] a[data-row-reference="${spec}"]`)
          ?.textContent
      ).toBe(NODE_TYPE_NAMES.document);
      expect(
        [...fed.container.querySelectorAll("[data-chip-name]")].map((chip) => chip.textContent)
      ).toEqual([NODE_TYPE_NAMES.document]);
    });
  } finally {
    getReferenceTargets.mockRestore();
    getIssueSummary.mockRestore();
    for (const view of drawn) view.unmount();
    page.unmount();
  }
});

// R8 over the row above, before the page's read answers: a chip the page has given no word yet
// for a whole document - one in place in a node's words, and a tie's whose reading carries none
// - names it by the store's rule as far as the address and the row's issue say it
// (`node-row-chip.ts`'s `documentWord`): the row's own issue's spec as the Spec, and another
// issue's as a document, the words the store gives both once it answers.
test("before the page's read answers, a chip names another issue's spec as a document and its own issue's as the Spec", async () => {
  const other = "dispatch://CORE-9/spec";
  const own = `dispatch://${KEY}/spec`;
  const words = `Wait for ${other} and ${own}.`;
  const view = renderRow(
    node({
      chips: [other, own].map((ref): NodeChip => ({ answered: false, gone: false, ref })),
      id: "plan-md#b-t1",
      source: { id: "artifact-plan", issue: KEY, kind: "artifact" },
      text: words,
      tree: readingOf(words),
      type: "plan_task",
    }),
    // The page has asked for both, and its read has not answered.
    { body: false, published: publishedNames(new Map(), { asked: [other, own] }) }
  );
  try {
    await waitFor(() => {
      expect(
        [other, own].map(
          (ref) =>
            view.container.querySelector(`[data-node-words] a[data-row-reference="${ref}"]`)
              ?.textContent
        ),
        "a chip in place"
      ).toEqual([NODE_TYPE_NAMES.document, PRESET_NAMES.spec]);
      expect(
        [...view.container.querySelectorAll("[data-chip-name]")].map((chip) => chip.textContent),
        "a tie's chip"
      ).toEqual([NODE_TYPE_NAMES.document, PRESET_NAMES.spec]);
    });
  } finally {
    view.unmount();
  }
});

// R2 and issue page decision 350: a filter's row names a parent it draws no row for by the
// parent's own first line, projected from its words as every one line of a node is: a
// reference in it reads as its target's name once the page names it. A parent whose words hold
// none is named by the page's word for its kind (R22).
test("a parent line is its parent's projected first line, a reference in it read by its target's name", async () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const words = `Serve ${address} first. Then the rest.`;
  const view = renderRow(
    node({
      id: "plan-md#b-t1",
      parentLine: {
        id: "spec#b-r2",
        reference: "dispatch://CORE-1/spec#b-r2",
        text: words,
        tree: readingOf(words),
        type: "requirement",
      },
      text: "Draw the rows.",
      type: "plan_task",
    }),
    { body: false, published: publishedNames(new Map([[address, "The rows hold"]])) }
  );
  try {
    await waitFor(() =>
      expect(view.container.querySelector("[data-node-parent-line] a")?.textContent).toBe(
        "Serve The rows hold first."
      )
    );
  } finally {
    view.unmount();
  }
  const empty = renderRow(
    node({
      id: "comment-2",
      parentLine: { id: "ask-1", text: "", type: "ask" },
      text: "Keep one reading.",
      type: "comment",
    }),
    { body: false }
  );
  try {
    expect(
      empty.container.querySelector("[data-node-parent-line]")?.lastElementChild?.textContent
    ).toBe(NODE_TYPE_NAMES.ask);
  } finally {
    empty.unmount();
  }
});

// R1, R9, ruling 1: an answered ask carries the person's answer in its own row,
// and no row of its own: who answered by a name and a shape, what they chose among the ask's own
// options, and what they wrote, word for word and whole. Here the read sent no reading of what
// they wrote, so the page parses it, and each paragraph drawn is the paragraph the store holds.
test("an answered ask draws the answer in its own row, word for word", () => {
  const paragraphs = [
    "The preset row keeps its labels above that width.",
    "Below it, one control.",
  ];
  const text = paragraphs.join("\n\n");
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
    expect(line?.querySelector("[data-answer-author] svg")).toHaveAttribute(
      "data-glyph",
      "author-person"
    );
    const words = line?.querySelector("[data-answer-text]");
    expect([...(words?.querySelectorAll("p") ?? [])].map((one) => one.textContent)).toEqual(
      paragraphs
    );
    expect(words?.textContent).toBe(paragraphs.join(""));
    // The option the person chose is marked in the ask's own list, and the body adds what an
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

// R6 over the row above: an answer the read sent with its reading is drawn by the one renderer
// from that reading, and what the person wrote and chose still reads word for word: each
// paragraph of the answer's text is the paragraph the store holds, and each chosen option is
// the option's name as the answer took it, where the ask offers no list to mark it in.
test("an answer drawn through the renderer reads word for word, its text and its choices", async () => {
  const paragraphs = [
    "The preset row keeps its labels above that width.",
    "Below it, one control, and the rail folds.",
  ];
  const text = paragraphs.join("\n\n");
  const selected = ["1280 px, the rail fits", "Keep the labels"];
  const asked = node({
    answer: {
      at: "2026-11-06T00:00:00Z",
      by: { id: "alice", kind: "user" },
      selected,
      selectedTrees: selected.map((one) => {
        const tree = displayTree(one);
        if (tree === undefined) throw new Error("the warmed engine reads every option");
        return tree;
      }),
      text,
      textTree: displayTree(text),
    },
    id: "ask-1",
    state: "answered",
    text: "At which width does the identity block condense?",
    type: "ask",
  });
  const view = renderRow(asked, { answerer: "alice" });
  try {
    const answer = view.container.querySelector("[data-node-answer]");
    await waitFor(() => {
      const drawn = [...(answer?.querySelectorAll("[data-answer-text] p") ?? [])];
      expect(
        drawn.map((one) => one.textContent),
        "each paragraph of the answer is the paragraph stored"
      ).toEqual(paragraphs);
    });
    expect(
      [...(answer?.querySelectorAll("[data-answer-choice]") ?? [])].map((one) => one.textContent),
      "each chosen option is the name the answer took"
    ).toEqual(selected);
    expect(
      answer?.querySelector("[data-answer-choice] .dispatch-markdown"),
      "a choice is drawn by the renderer"
    ).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// R9: a decision the model marks unsure says so in one word on its own row, and a decision it
// does not mark says nothing of the kind.
test("a decision the model marks unsure says so in one word", () => {
  const unsure = (marked: boolean) => {
    const view = renderRow(
      node({ id: "decisions-md#b-d3", type: "decision", ...(marked ? { unsure: true } : {}) })
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

/** The few words a row draws for `gap`, and the sentence its body draws under them. */
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

/** One row the way the tree draws it: unfocused until a focus lands inside it, and then the
 *  focused row, so a test reads what that focus does to the page. */
function FocusableRow({ drawn }: { drawn: TreeNode }): ReactNode {
  const [focused, setFocused] = useState(false);
  return (
    <NodeRow
      author={NOBODY_RECORDED}
      body
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onPin={() => {}}
      onToggle={() => {}}
      ownAnswer={false}
      pinned={false}
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
// and is projected from its first block alone (`lineReading`): the heading never runs into the
// paragraph after it.
test("a decision whose text holds bold draws no literal asterisks", () => {
  const decision = node({
    id: "decisions-md#b-d1",
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
  const folded = renderRow({ ...decision, held: 1 }, { body: false, expanded: false, folded: 1 });
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

// A node whose read sends no reading of its text - a server of the build before, during a
// rolling deploy - is parsed on the page by the shared engine and drawn through the renderer from
// that parse, as a reading is: the reader sees bold words, never the asterisks the author typed.
test("a node whose read sends no reading of its text draws its bold words in a strong, with no asterisks", () => {
  const view = renderRow(
    node({ id: "spec#b-r1", text: "Ship the **bold** words.", type: "requirement" })
  );
  try {
    const words = view.container.querySelector("[data-node-words]");
    expect(words?.querySelector("strong")?.textContent).toBe("bold");
    expect(words?.textContent).toBe("Ship the bold words.");
  } finally {
    view.unmount();
  }
});

// The same for a worded link to a role in such a text: it is drawn as a link in the words its
// author gave it, never as its markdown with the role's name written where its address stood.
test("a node whose read sends no reading of its text draws a worded link as its words, with none of its markdown", async () => {
  const view = renderRow(
    node({ id: "comment-1", text: "[my words](dispatch://role/reviewer)", type: "comment" }),
    { parties: { role: "reviewer" } }
  );
  try {
    const words = view.container.querySelector("[data-node-words]");
    const link = await waitFor(() => {
      const found = words?.querySelector("a");
      expect(found?.textContent).toBe("my words");
      return found;
    });
    expect(link).toHaveAttribute("href", "/agents?role=reviewer");
    expect(words?.textContent).toBe("my words");
  } finally {
    view.unmount();
  }
});

// Where the engine's load fails, such a text is never parsed, and the row draws its words as the
// characters they are, as a row with a reading draws its words with no engine, never nothing for
// good; while the load is pending it draws nothing, so no mark shows raw meanwhile. This file
// loads the engine, so the row stands a load that is pending, and then fails, in its place.
test("a node whose read sends no reading of its text draws nothing while the engine loads, and its characters once the load fails", async () => {
  const load = Promise.withResolvers<void>();
  const loaded = spyOn(markdownEngine, "loadedEngine").mockReturnValue(undefined);
  const warm = spyOn(markdownEngine, "warmMarkdownRenderer").mockReturnValue(load.promise);
  try {
    const text = "Ship the **bold** words.";
    const view = renderRow(node({ id: "spec#b-r1", text, type: "requirement" }));
    try {
      const words = () => view.container.querySelector("[data-node-words]")?.textContent;
      expect(words(), "the row drew its words while the engine was loading").toBe("");
      await act(async () => {
        load.reject(new Error("the engine's chunk did not load"));
      });
      await waitFor(() => expect(words()).toBe(text));
    } finally {
      view.unmount();
    }
  } finally {
    loaded.mockRestore();
    warm.mockRestore();
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
      { body: false, focused }
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

// Part G's R4, the tester's F20: a decision's text holds the source of a typed block its section
// quotes, and the row draws the block's words but none of its source lines, whether it draws the
// text from the page's parse or from its reading.
test("a decision whose text quotes a typed block draws none of its source lines", async () => {
  const text = "It names the statement.\n\n:::requirement{#r1}\nThe page draws no raw source.\n:::";
  for (const tree of [undefined, displayTree(text)]) {
    const view = renderRow(
      node({
        id: "decisions-md#b-d1",
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

// Ruling 5: a tie is a chip at the end of the node's words, named by its target's name cut at a
// word within 32 characters, and never by an address, a slug or a document's name (R22). A
// target the reference read has not named yet draws nothing.
test("a chip names its target by its first words and never by an address", async () => {
  const chips: NodeChip[] = [
    {
      answered: true,
      gone: false,
      reading: { words: "The page draws the whole issue in one reading and nothing is lost." },
      ref: "dispatch://CORE-1/spec#b-r2",
    },
    { answered: false, gone: false, ref: "dispatch://CORE-1/artifact/plan-md#b-t4" },
    { answered: true, gone: true, ref: "dispatch://CORE-1/spec#b-r9" },
    {
      answered: true,
      gone: false,
      label: "Needs",
      reading: { words: "Add the routes." },
      ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
    },
  ];
  const view = renderRow(
    node({ chips, id: "decisions-md#b-d1", text: "Keep one reading.", type: "decision" }),
    { body: false, published: publishedChips(chips) }
  );
  try {
    const drawn = () => [...view.container.querySelectorAll("[data-node-chip]")];
    await waitFor(() =>
      expect(drawn().map((chip) => chip.textContent)).toEqual([
        "The page draws the whole issue…",
        CONTROL_LABELS.target_gone,
        "Needs Add the routes.",
      ])
    );
    for (const chip of drawn()) {
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
    expect(trail?.parentElement).toHaveAttribute("data-node-words");
    expect(trail?.previousElementSibling?.textContent).toBe("Keep one reading.");
    // The named chip is the link a reader follows to its target.
    expect(drawn()[0]?.querySelector("a")).toHaveAttribute("href", "/issues/CORE-1/spec#b-r2");
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
    ref: "dispatch://CORE-1/comment/c1",
  };
  const view = renderRow(
    node({
      chips: [ambiguous],
      id: "decisions-md#b-d1",
      text: "Keep one reading.",
      type: "decision",
    }),
    { body: false, published: publishedChips([ambiguous]) }
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
// deleted line's italic, its actor named as the page names an author and not as the published
// name holds it; a chip naming a message its author edited names it by its words cut to the
// chip's 32 characters, then ` · edited`, so no cut takes the mark.
test("a chip names a deleted post by its deleted line, and an edited one by its cut words then edited", async () => {
  await warmMarkdownRenderer();
  const words = "Hold the deploy until the plan lands and the reviewer signs off";
  const chips: NodeChip[] = [
    {
      answered: true,
      deletedAt: DELETED_AT,
      deletedBy: UNTITLED,
      gone: false,
      reading: { words: `Deleted by ${actorLabelText(UNTITLED)}` },
      ref: "dispatch://CORE-1/comment/c-deleted",
    },
    {
      answered: true,
      editedAt: "2026-11-05T13:00:00Z",
      gone: false,
      reading: { words },
      ref: "dispatch://CORE-1/message/m-edited",
    },
  ];
  const view = renderRow(node({ chips, id: "comment-9", text: "Read these.", type: "comment" }), {
    body: false,
    published: publishedChips(chips),
  });
  try {
    const drawn = () =>
      [...view.container.querySelectorAll("[data-node-chip]")].map((chip) => chip.textContent);
    await waitFor(() =>
      expect(drawn()).toEqual([
        `Deleted by ${CONTROL_LABELS.unnamed_author}`,
        `${nodeRowChip.chipWords(words)} · edited`,
      ])
    );
    const drawnChips = [...view.container.querySelectorAll("[data-node-chip]")];
    expect(drawnChips[0]?.querySelector("a [data-post-deleted]")?.className).toContain("italic");
    expect(drawnChips[1]?.querySelector("[data-post-deleted]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 07:52Z, on a chip cut
// short: a deleted post's chip whose actor is named by one long word is cut right after "Deleted
// by" (`chipWords`), and that cut still draws the deleted line, in its italic, its opening marked
// as the page's own words and none of it as the author's.
test("a deleted post's chip cut right after Deleted by keeps the deleted line's italic and its page-chrome mark", async () => {
  await warmMarkdownRenderer();
  const title = "legion-infra-1523-implementer";
  const by: Actor = {
    id: "2b3c4d5e-1111-4222-8333-444455556666",
    kind: "session",
    origin: { session_title: title },
  };
  // The title's one word runs past the chip's room, so the cut ends at the space before it.
  expect(nodeRowChip.chipWords(`Deleted by ${title}`)).toBe("Deleted by…");
  const chips: NodeChip[] = [
    {
      answered: true,
      deletedAt: DELETED_AT,
      deletedBy: by,
      gone: false,
      reading: { words: `Deleted by ${actorLabelText(by)}` },
      ref: "dispatch://CORE-1/comment/c-deleted",
    },
  ];
  const view = renderRow(node({ chips, id: "comment-9", text: "Read these.", type: "comment" }), {
    body: false,
    published: publishedChips(chips),
  });
  try {
    await waitFor(() =>
      expect(view.container.querySelector("[data-chip-name] [data-post-deleted]")).not.toBeNull()
    );
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

/** A live session a chip names, and the path of its live view. */
const AGENT_SESSION = "7c0f3a52-5d0e-4b8e-9a51-2f4d6c8e1b90";
const AGENT_CHIP: NodeChip = {
  answered: true,
  gone: false,
  ref: `dispatch://agent/${AGENT_SESSION}`,
};

/** The live session `AGENT_CHIP` names, as the registry lists it. */
const AGENT = { roles: [], session_id: AGENT_SESSION, title: "Planner" } as unknown as Agent;

/** The address a reference to a person is written as: no page shows one (R5). */
const PERSON_REFERENCE = "dispatch://person/alice@example.com";

// R5: no page shows a person, so a chip that names one draws its login as text with no link, as
// a chip whose target nothing answers to does; an agent's chip beside it is the link to that
// agent's live view, named by its session's title. A party is never asked of the page's batched
// read, so each is named by its own read.
test("a chip naming a person draws its words with no link, beside an agent's chip that links", async () => {
  const view = renderRow(
    node({
      chips: [{ answered: true, gone: false, ref: PERSON_REFERENCE }, AGENT_CHIP],
      id: "decisions-md#b-d1",
      text: "Keep one reading.",
      type: "decision",
    }),
    { body: false, parties: { login: "alice@example.com", session: AGENT } }
  );
  try {
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-node-chip]")).toHaveLength(2)
    );
    const [person, agent] = view.container.querySelectorAll("[data-node-chip]");
    expect(person?.textContent).toBe("alice@example.com");
    expect(person?.querySelector("a"), "a person's chip links to a page no one shows").toBeNull();
    expect(agent?.textContent).toBe("Planner");
    expect(agent?.querySelector("a")).toHaveAttribute("href", `/agents/${AGENT_SESSION}/live`);
  } finally {
    view.unmount();
  }
});

// R2 and R5: a filter's row names its parent on one line, as the link to it; a parent that is a
// person, whom no page shows, is named by its words with no link, while an agent's chip on the
// same row is the link to that agent's live view.
test("a parent line naming a person draws its words with no link, beside an agent's chip that links", async () => {
  const view = renderRow(
    node({
      chips: [AGENT_CHIP],
      id: "comment-1",
      parentLine: {
        id: "person-alice",
        reference: PERSON_REFERENCE,
        text: "alice@example.com",
        type: "comment",
      },
      text: "Keep one reading.",
      type: "comment",
    }),
    { body: false, parties: { session: AGENT } }
  );
  try {
    const parent = view.container.querySelector("[data-node-parent-line]");
    await waitFor(() => expect(parent?.textContent).toContain("alice@example.com"));
    expect(parent?.querySelector("a"), "a person's parent line is a link").toBeNull();
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-node-chip] a"),
        "the agent's chip on the same row is no link to its live view"
      ).toHaveAttribute("href", `/agents/${AGENT_SESSION}/live`)
    );
  } finally {
    view.unmount();
  }
});

// F18 and decisions 66 and 88 in a node's words: a chip in place is named from the batched
// read, whose reading of a target's name the page publishes whole and the store projects in
// plain words, with no list or block reading but for an ordered list's number, and the row cuts
// it as a tie's chip is cut (`chipName`), so a numbered step keeps its number. The words are
// drawn under the page's own providers (`ProvidersSlot`), as the page draws them.
test("a chip in a node's words draws its target's plain words, a numbered step's number kept", async () => {
  const target = "dispatch://CORE-1/artifact/plan-md#b-t1";
  const text = `It follows ${target}.`;
  const published: PublishedReferences = {
    ambiguous: new Set(),
    asked: new Set([target]),
    deleted: new Map(),
    edited: new Set(),
    gone: new Set(),
    readings: new Map([[target, { tree: readingOf("1. Stand the harness up and read the rows") }]]),
    unnamed: new Set(),
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The reads the page's writer makes (`outline-writer.tsx`), answered before it mounts.
  for (const key of [
    ["asks", KEY],
    ["comments", KEY],
    ["issue-messages", KEY],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named published={published}>
            <ProvidersSlot issue={issueDetails()} route={{ key: KEY, kind: "issue" }}>
              <RowText markdown={text} tree={readingOf(text)} />
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

// Decision 102: the batched read's names are projected once for each answer of the read, not
// once for each row. A block's name is its whole reading, so naming every name again for every
// row cost the page seconds on an issue of 650 rows. The store projects each reading once
// (`refs/reference-names.tsx`), and every row draws from its one map, cutting its own chips'.
test("the read's names are named as chips once per answer, not once per row", async () => {
  const readings = new Map(
    Array.from({ length: 30 }, (_unused, at) => [
      `dispatch://CORE-1/spec#b-r${at}`,
      {
        tree: readingOf(
          `The words of statement ${at}, which run on well past the room a chip has.`
        ),
      },
    ])
  );
  const names = new Set<unknown>([...readings.values()].map((reading) => reading.tree));
  const engine = loadedEngine();
  if (engine === undefined) throw new Error("the markdown engine has not loaded");
  const draw = spyOn(engine, "draw");
  const published: PublishedReferences = {
    ambiguous: new Set(),
    asked: new Set(readings.keys()),
    deleted: new Map(),
    edited: new Set(),
    gone: new Set(),
    readings,
    unnamed: new Set(),
  };
  const target = "dispatch://CORE-1/spec#b-r0";
  const text = `It follows ${target}.`;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named published={published}>
            {Array.from({ length: 20 }, (_unused, at) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: twenty rows of one text.
              <RowText key={at} markdown={text} tree={readingOf(text)} />
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
    const projections = draw.mock.calls.filter(([tree]) => names.has(tree)).length;
    expect(projections, "every row named the read's names again").toBeLessThanOrEqual(
      readings.size
    );
  } finally {
    draw.mockRestore();
    view.unmount();
  }
});

// Decisions 62 and 89: a short reference two or more targets answer to names none of them, so
// the page publishes it as ambiguous, apart from its names, and a node's words draw it as its
// author wrote it, with no link, never as a target that is gone. A reference the read answered
// gone keeps its link, and reads as the words its author wrote rather than as its short form
// (issue page decision 350, which replaces decision 95). Both the words drawn from their reading
// and the words drawn from the page's parse are read.
test("an ambiguous short reference in a node's words is drawn as written with no link, and a gone one keeps its link", async () => {
  const written = "dispatch://CORE-1/spec#b-1a2b3c4d";
  const gone = "dispatch://CORE-1/spec#b-9f8e7d6c";
  const text = `It follows ${written} and ${gone}.`;
  const published = publishedNames(new Map(), {
    ambiguous: new Set([written]),
    gone: new Set([gone]),
  });
  for (const tree of [readingOf(text), undefined]) {
    const drawn = tree === undefined ? "from the page's parse" : "from their reading";
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <ReferenceNamesProvider>
            <Named published={published}>
              <RowText markdown={text} {...(tree === undefined ? {} : { tree })} />
            </Named>
          </ReferenceNamesProvider>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      await waitFor(() =>
        expect(
          view.container.querySelector(`a[data-row-reference="${gone}"]`)?.textContent,
          `the gone reference drawn ${drawn} lost its link or its author's words`
        ).toBe(gone)
      );
      expect(
        view.container.querySelector(`a[data-row-reference="${written}"]`),
        `the ambiguous reference drawn ${drawn} is a link`
      ).toBeNull();
      expect(view.container.textContent).toContain(`It follows ${written} and ${gone}.`);
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
              <Named published={publishedNames(names, { deleted: deletions, edited: marked })}>
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
 *  beside them, and with the parties Dispatch knows (`parties`) read. `click` dispatches a click
 *  on `link` and answers whether the browser was left to follow it, and then stops the browser:
 *  the unit suite's document loads no page. */
function drawWordsAt(
  at: string,
  text: string,
  tree: DisplayTree | undefined,
  names: ReadonlyMap<string, string>,
  parties: Parties = {}
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  knowParties(queryClient, parties);
  const published = publishedNames(names);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[at]}>
        <ReferenceNamesProvider>
          <Named published={published}>
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
// held is the browser's, which opens it elsewhere. Both the words drawn from the page's parse and
// the words drawn from their reading, whose links the row's own pass makes, are read.
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

// R5, R8 and issue page decision 350: a party - here a role - is never asked of the batched
// read, so a row names it by its own read, as every surface of Dispatch does: a role by its
// token, once a session the registry lists holds it, never as its address. Words whose read sent
// no reading of them, which the page parses, draw it the same way, and link it to the agents list
// that role filters (`referenceHref`).
test("a role in words whose read sent no reading reads as its token and links to its agents", async () => {
  const role = "dispatch://role/reviewer";
  const drawn = drawWordsAt("/issues/CORE-1", `Ask ${role} to look.`, undefined, new Map(), {
    role: "reviewer",
  });
  try {
    const link = await waitFor(() => {
      const found = drawn.view.container.querySelector(`a[data-row-reference="${role}"]`);
      expect(found?.textContent, "the role is not drawn by its token").toBe("reviewer");
      return found;
    });
    expect(link).toHaveAttribute("href", "/agents?role=reviewer");
    expect(drawn.view.container.textContent).not.toContain("dispatch://");
  } finally {
    drawn.view.unmount();
  }
});

/** Lays every element out `height` px tall for the rows drawn until the answer is called: no
 *  DOM of a unit test lays anything out, and the cut reads the words' own boxes
 *  (`clamp-measurer.ts`'s `measureClamp`). An element's words run `height` px, or what `words`
 *  gives for it. */
function layOut(
  height: number,
  words: (element: HTMLElement) => number = () => height
): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return words(this);
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

// Ruling 1, and the reviewer's reading of it: "more · N words" opens the rest of a cut text, so
// it is drawn only while the words are cut. A row whose body is open draws its words whole, and
// no control that would open what is already open. The count is written with its thousands
// separator. The page measures only the words of rows near the viewport, so each
// row's words are brought near and measured before the row is read.
test("more is drawn on a cut row and never beside words a body draws whole", async () => {
  const text = Array.from({ length: 3000 }, (_unused, at) => `word${at + 1}`).join(" ");
  const long = node({ id: "message-1", text, type: "message" });
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  // 3,000 words over 400 lines of 24 px: six lines are drawn and 394 of 400 parts are held back.
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  try {
    const cut = renderRow(long, { author: planner, body: false });
    try {
      await measureNearWords(near);
      expect(cut.container.querySelector("[data-node-more]")?.textContent).toBe(
        `${CONTROL_LABELS.read_more} · 2,955 ${CONTROL_LABELS.read_more_words}`
      );
      expect(cut.container.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight).toBe(
        "144px"
      );
    } finally {
      cut.unmount();
    }
    const open = renderRow(long, { author: planner, body: true });
    try {
      await measureNearWords(near);
      expect(
        open.container.querySelector("[data-node-more]"),
        "a row whose body is open still draws more"
      ).toBeNull();
      expect(open.container.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight).toBe(
        ""
      );
    } finally {
      open.unmount();
    }
    // Ruling 6: a statement is an agent's words as much as its comment is, and is cut the same;
    // a person's words never are.
    const statement = renderRow(node({ id: "spec#b-r1", text, type: "requirement" }), {
      body: false,
    });
    try {
      await measureNearWords(near);
      expect(statement.container.querySelector("[data-node-more]")).not.toBeNull();
    } finally {
      statement.unmount();
    }
    const person = renderRow(node({ id: "comment-1", text, type: "comment" }), {
      author: { kind: "person", name: "bob", reader: false },
      body: false,
    });
    try {
      await measureNearWords(near);
      expect(person.container.querySelector("[data-node-more]")).toBeNull();
    } finally {
      person.unmount();
    }
  } finally {
    near.restore();
    restore();
  }
});

/** 3,000 words, which run 400 lines of 24 px where they are drawn whole. */
const LONG_WORDS = Array.from({ length: 3000 }, (_unused, at) => `word${at + 1}`).join(" ");

/**
 * What a row in view draws in the commit that draws it from `next`, before the page is painted:
 * the row is drawn from `first` and measured as its coming near measures it, then, if `relaidOut`
 * is given, laid out again by it and measured again as it stands in view, and then drawn from
 * `next` with every animation frame held, so none runs. `words` is how tall the browser lays a
 * row's words out (`layOut`). `cutBefore` is whether the row was cut before `next` was drawn.
 */
async function redrawnInView({
  first,
  next,
  relaidOut,
  words,
}: {
  readonly first: TreeNode;
  readonly next: TreeNode;
  readonly relaidOut?: () => void;
  readonly words: (element: HTMLElement) => number;
}): Promise<{ cutBefore: boolean; maxHeight: string | undefined; more: string | undefined }> {
  const restore = layOut(9600, words);
  const near = installIntersectionObserver();
  try {
    const row = renderRow(first, { body: false });
    try {
      await measureNearWords(near);
      if (relaidOut !== undefined) {
        relaidOut();
        await measureNearWords(near);
      }
      const cutBefore = row.container.querySelector("[data-node-more]") !== null;
      const frames: FrameRequestCallback[] = [];
      const frame = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
        frames.push(callback);
        return frames.length;
      });
      try {
        await act(async () => {
          row.redraw(next);
        });
        return {
          cutBefore,
          maxHeight: row.container.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight,
          more: row.container.querySelector("[data-node-more]")?.textContent ?? undefined,
        };
      } finally {
        frame.mockRestore();
      }
    } finally {
      row.unmount();
    }
  } finally {
    near.restore();
    restore();
  }
}

// Decision 263: a row in view is cut before the page paints it, also where its words arrive
// after the row. A statement is drawn from its document's blocks before the coverage read
// brings its words, so its row mounts in view with none: the mount measures it whole, and the
// batch its coming near starts holds it in view at that length. The commit that brings its words
// measures it again as one that mounts, and it is drawn cut with no animation frame run. CI run
// 44547033390 caught it otherwise at 390 px: the words were drawn whole, 472 px tall, and stayed
// whole for the 59 frames recorded after.
test("a row in view whose words arrive after it is drawn cut before the page is painted", async () => {
  const statement = { id: "spec#b-r1", type: "requirement" } as const;
  const drawn = await redrawnInView({
    first: node({ ...statement, text: "" }),
    next: node({ ...statement, text: LONG_WORDS, tree: displayTree(LONG_WORDS) }),
    // Words not drawn yet run no height; drawn, they run 400 lines of 24 px.
    words: (element) => ((element.textContent ?? "").trim() === "" ? 0 : 9600),
  });
  expect(drawn.cutBefore, "a row with no words was cut").toBe(false);
  expect(drawn.more, "the words that arrived were drawn whole in view").toBe(
    `${CONTROL_LABELS.read_more} · 2,955 ${CONTROL_LABELS.read_more_words}`
  );
  expect(drawn.maxHeight).toBe("144px");
});

// Decision 261: a row in view keeps its length unless it draws other words. A new comment on the
// issue adds an address to the names read, which then reads under a key of its own, so every
// block row drawn from that read takes a new reading of the same words (`tree-drafts.ts`'s
// `treeOf`). A row measured whole, whose words then run past the cut, as a narrower width runs
// them, is held whole in view by the batch that measures it again; a new reading of the same
// words draws nothing new, so the row stays whole, with no animation frame run.
test("a row held whole in view stays whole when a later read draws the same words anew", async () => {
  const statement = { id: "spec#b-r1", text: LONG_WORDS, type: "requirement" } as const;
  // The server reads one text into one tree (`pmdoc.DisplayJSON`), where the page's own parse
  // gives each block a new id, so the later read's answer is a copy of the first.
  const reading = displayTree(LONG_WORDS);
  // Five lines of 24 px where the row is first measured, which six lines hold whole.
  let run = 120;
  const drawn = await redrawnInView({
    first: node({ ...statement, tree: reading }),
    next: node({ ...statement, tree: structuredClone(reading) }),
    // The words now run 400 lines, and the batch holds the row in view at its length.
    relaidOut: () => {
      run = 9600;
    },
    words: () => run,
  });
  expect(drawn.cutBefore, "the batch cut a row in view").toBe(false);
  expect(drawn.more, "a new reading of the same words cut the row held whole in view").toBe(
    undefined
  );
  expect(drawn.maxHeight).toBe("");
});

// issue page decision 350 and G's clamp: a reference in a row's words takes its name after the
// row was first drawn and measured - the page's batched read answers last - and its name can run
// the words longer or shorter than the words its author wrote. The row measures its cut again,
// as it does for words of another value (`useClamp`), in the commit the name lands in, before the
// page is painted: a row measured whole in view whose words the name runs past six lines is drawn
// cut with "more" under it, rather than run past its room under the length measured for the
// words written. With no animation frame run, only that commit's own measure can cut it.
test("a row whose reference takes its name after it is measured is measured again", async () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const text = `${LONG_WORDS} Read ${address} first.`;
  const said = node({ id: "message-1", text, tree: readingOf(text), type: "message" });
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  // The words with the address as written lay out five lines of 24 px, which six lines hold
  // whole; with its name in its place they lay out 400.
  const restore = layOut(9600, (element) =>
    (element.textContent ?? "").includes("Draw the rows") ? 9600 : 120
  );
  const near = installIntersectionObserver();
  try {
    const row = renderRow(said, {
      author: planner,
      body: false,
      published: publishedNames(new Map(), { asked: [address] }),
    });
    try {
      await measureNearWords(near);
      const words = () => row.container.querySelector<HTMLElement>("[data-node-words]");
      expect(words()?.textContent, "the address was not drawn as written").toContain(address);
      expect(
        row.container.querySelector("[data-node-more]"),
        "the words written were cut"
      ).toBeNull();
      const frames: FrameRequestCallback[] = [];
      const frame = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
        frames.push(callback);
        return frames.length;
      });
      try {
        await act(async () => {
          row.publish(publishedNames(new Map([[address, "Draw the rows"]])));
        });
        expect(words()?.textContent).toContain("Read Draw the rows first.");
        expect(
          row.container.querySelector("[data-node-more]")?.textContent,
          "the row was not measured again when the name ran its words past six lines"
        ).toContain(CONTROL_LABELS.read_more);
        expect(words()?.style.maxHeight).toBe("144px");
      } finally {
        frame.mockRestore();
      }
    } finally {
      row.unmount();
    }
  } finally {
    near.restore();
    restore();
  }
});

// R16: an open question's options are the choices of its answer. A press on one leaves the
// focus where it is - a row that took the focus on mousedown would open its body, whose
// options' own words push every option below one that has them down, and the press would end on
// another option - and only a question that waits on the reader draws them in the waiting
// colour.
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
  // Each option's words are drawn by the renderer, which reads what they cite through the query
  // client, so the options are drawn under one, as the page draws them.
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const drawOptions = (waiting: boolean) =>
    render(
      <QueryClientProvider client={queryClient}>
        <NodeOptions body={false} node={asked(waiting)} writer={writer} />
      </QueryClientProvider>
    );
  const view = drawOptions(true);
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
  const other = drawOptions(false);
  try {
    const option = other.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    expect(option?.className).toContain("text-page-quiet");
    expect(option?.className).not.toContain("text-page-waiting-text");
  } finally {
    other.unmount();
  }
});

// R9, and a reference's host: an option's words are the ask's stored text, drawn by the renderer,
// so a reference in them reads as its target. In an option the reader presses the press is the
// option's, so the reference sits in a `control` host and draws no link inside the button. In
// an option nobody presses it is a link, out of the tab order as every control inside a row is,
// and in it at the default order in a view whose rows take tab stops (`tabStops`).
test("a reference in an option draws no link inside its press, and elsewhere a link in the tab order only with tabStops", async () => {
  const label = "Follow dispatch://CORE-7";
  const asked = node({
    id: "ask-1",
    options: [{ label, labelTree: displayTree(label) }],
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "open",
    text: "Which way does the header go?",
    type: "ask",
    waiting: true,
  });
  const writer = { answer: undefined, pick: () => {} } as unknown as OutlineWriter;
  const drawOptions = (answering: OutlineWriter | undefined, tabStops: boolean) => {
    // The reference names CORE-7, whose summary the cache holds, so it resolves at once.
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(["issue-summary", "CORE-7"], {
      documents: [],
      key: "CORE-7",
      project: "CORE",
      status: "in_progress",
      title: "Draw the header",
    });
    return render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <NodeOptions body={false} node={asked} tabStops={tabStops} writer={answering} />
        </MemoryRouter>
      </QueryClientProvider>
    );
  };
  const pressable = drawOptions(writer, false);
  try {
    const option = pressable.container.querySelector(`[data-pick-option="${label}"]`);
    if (option === null) throw new Error("the open question draws no option to press");
    await waitFor(() => expect(option.textContent).toContain("CORE-7 Draw the header"));
    expect(option.querySelector("a"), "a reference inside the option's press is a link").toBeNull();
    expect(option.querySelector('[data-dispatch-ref="dispatch://CORE-7"]')).not.toBeNull();
  } finally {
    pressable.unmount();
  }
  for (const tabStops of [false, true]) {
    const view = drawOptions(undefined, tabStops);
    try {
      const link = await waitFor(() => {
        const found = view.container.querySelector('[data-node-option] a[href="/issues/CORE-7"]');
        expect(found?.textContent).toBe("CORE-7 Draw the header");
        return found;
      });
      await waitFor(() => {
        // jest-dom: Bun 1.4.4 prints no message of a jest-dom matcher, and this names the case.
        expect(
          link?.getAttribute("tabindex"),
          tabStops ? "the link left the tab order" : "the link kept its tab stop"
        ).toBe(tabStops ? null : "-1");
      });
    } finally {
      view.unmount();
    }
  }
});

/** One row the way the conversation's rows draw it: closed until it takes the focus, open from
 *  then on, under the page's writer. */
function RowOpenedByFocus({ drawn }: { drawn: TreeNode }): ReactNode {
  const [focused, setFocused] = useState(false);
  return (
    <NodeRow
      author={{ kind: "agent", name: "Planner", reader: false }}
      body={focused}
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onPin={() => {}}
      onToggle={() => {}}
      ownAnswer={false}
      pinned={false}
      rowKey="row"
      tabStop
      tabStops
    />
  );
}

// Decision 120: a control a reader can see acts on its first press. An option drawn on a row
// that is not open yet is picked by that press, and the same press opens the row: the reader
// never presses twice.
test("a press on an option of a closed row picks it and opens the row in one", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [
    ["asks", "CORE-1"],
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
            <RowOpenedByFocus drawn={asked} />
          </OutlineWriterProvider>
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    expect(view.container.querySelector("[data-node-body]")).toBeNull();
    const option = view.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    if (option === null) throw new Error("the closed row draws no option to press");
    await user.click(option);
    await waitFor(() => {
      expect(view.container.querySelector('[data-pick-option="1024 px"]')).toHaveAttribute(
        "aria-pressed",
        "true"
      );
      expect(
        view.container.querySelector("[data-node-body]"),
        "the press picked the option and left the row closed"
      ).not.toBeNull();
    });
  } finally {
    view.unmount();
  }
});

// Ruling 5: a child issue's row is one line - its title, then how it stands and what blocks it -
// and its key is the row's tag, a link to the child's own page; it names no author and no time.
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
  const view = renderRow(node({ child, id: "CORE-2", text: child.title, type: "child_issue" }), {
    body: false,
  });
  try {
    expect(view.container.querySelector("[data-node-words] [data-author-text]")?.textContent).toBe(
      child.title
    );
    expect(view.container.querySelector("[data-child-facts]")?.textContent).toContain("CORE-3");
    const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
    expect(tag?.textContent).toBe("CORE-2");
    expect(tag?.querySelector("a")).toHaveAttribute("href", "/issues/CORE-2");
    expect(view.container.querySelector("[data-node-author]")).toBeNull();
    expect(view.container.querySelector("[data-node-time]")).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The writer's acts the focused row draws for `drawn` (`[data-row-act]`), with the page's writer
 *  above it or with none, and whether it draws Copy reference (`[data-row-copy]`); `ownAnswer`
 *  says the answer an ask carries is the reader's own. The writer's reads are answered before it
 *  mounts, under the keys the tree reads them by, so it makes no request. */
function focusedControls(
  drawn: TreeNode,
  withWriter: boolean,
  ownAnswer = false
): { acts: string[]; copy: boolean } {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [
    ["asks", "CORE-1"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const row = (
    <NodeRow
      author={{ kind: "person", name: "bob", reader: false }}
      body={false}
      context={false}
      focused
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => {}}
      onPin={() => {}}
      onToggle={() => {}}
      ownAnswer={ownAnswer}
      pinned={false}
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

/** The writer's acts the focused row draws for `drawn` (`focusedControls`). */
function focusedActs(drawn: TreeNode, withWriter: boolean, ownAnswer = false): string[] {
  return focusedControls(drawn, withWriter, ownAnswer).acts;
}

// Part G's R7: the focused row draws Reply on a comment, a message and an ask in any state, and
// Answer on an open question; a statement, a check and a decision draw none, since today's model
// holds no reply to a block; and with no writer above it the row draws no act at all.
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
  expect(focusedActs(turn("comment", "comment-1"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("message", "message-1"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("ask", "ask-1", "answered"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("ask", "ask-2", "resolved"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("ask", "ask-3", "open"), true)).toEqual(["answer", "reply"]);
  const block = (type: NodeType, id: string) =>
    node({ author: by, id, reference: `dispatch://CORE-1/spec#b-${id}`, type });
  expect(focusedActs(block("requirement", "r1"), true)).toEqual([]);
  expect(focusedActs(block("check", "c1"), true)).toEqual([]);
  expect(focusedActs(block("decision", "d1"), true)).toEqual([]);
  expect(focusedActs(turn("ask", "ask-4", "open"), false)).toEqual([]);
  expect(focusedActs(turn("comment", "comment-2"), false)).toEqual([]);
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
  expect(focusedActs(answered, true, true)).toEqual(["change-answer", "reply"]);
  expect(focusedActs(answered, true, false)).toEqual(["reply"]);
  expect(focusedActs(answered, false, true)).toEqual([]);
});

// Part G's R6: the focused row offers Copy reference on a node that has an address, with a
// writer above it or with none, since it writes nothing; a node with no address draws none.
test("Copy reference is drawn on a focused node with an address, and on none without", () => {
  const comment = (reference?: string) =>
    node({
      author: { id: "bob", kind: "user" },
      id: "comment-1",
      ...(reference === undefined ? {} : { reference }),
      type: "comment",
    });
  expect(focusedControls(comment("dispatch://CORE-1/comment/comment-1"), true).copy).toBe(true);
  expect(focusedControls(comment("dispatch://CORE-1/comment/comment-1"), false).copy).toBe(true);
  expect(focusedControls(comment(), true).copy).toBe(false);
});

// R3 of dispatch://INFRA-1523: Dispatch takes no reply to a comment or a message its author
// deleted (409 `POST_DELETED`), so its focused row draws no Reply. Copy reference still stands,
// since it writes nothing.
test("a deleted comment's or message's focused row draws no Reply, and still Copy reference", () => {
  for (const type of ["comment", "message"] as const) {
    const deleted = deletedTurn(UNTITLED, {
      id: `${type}-1`,
      reference: `dispatch://CORE-1/${type}/${type}-1`,
      type,
    });
    expect(focusedControls(deleted, true), type).toEqual({ acts: [], copy: true });
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
    ["asks", "CORE-1"],
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
    draft: { body: "A reply on its way", replacement: "" },
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
      // jest-dom: Bun 1.4.4 prints no message of a jest-dom matcher, and this names the button.
      expect(document.activeElement, `a press on ${control.textContent} took the focus`).toBe(held);
      await user.pointer({ keys: "[/MouseLeft]", target: control });
      expect(pressed, `the press on ${control.textContent} did nothing`).toEqual([true]);
      expect(held).toHaveFocus();
    } finally {
      view.unmount();
    }
  }
});
