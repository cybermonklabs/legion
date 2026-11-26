import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { Artifact, CommentWithTrees, GraphEdge, GraphReferences } from "../../api/types";
import { DocumentReferences } from "./document-references";
import { NODE_REFERENCES_WORDS } from "./page-words";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";

/**
 * What references a document opened in place of the outline (part N's R2 and R12): the rows
 * Details' 'What references this row' draws, from part M's `referenceRows`, with the issue's own
 * questions, comments and messages kept, since no outline is drawn beside an opened document.
 */

const KEY = "CORE-1";
const NOTES_REFERENCE = `dispatch://${KEY}/artifact/notes-md`;
const NOTES: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-notes",
  issue_key: KEY,
  kind: "doc",
  name: "notes.md",
  primary: false,
  project: "CORE",
  ref_key: `${KEY}/notes-md`,
  session_id: null,
  slug: "notes-md",
  versions: [],
};

const OWN_COMMENT = "11111111-1111-4111-8111-111111111111";
const OWN_WORDS = "CORE-1's own comment cites the notes.";
const OTHER_COMMENT = "22222222-2222-4222-8222-222222222222";

/** The issue's own comment, as the outline's read of the issue's comments holds it: its body is the
 *  markdown its author typed, and its reading is the words its row in the outline draws. */
const COMMENTS: readonly CommentWithTrees[] = [
  {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "CORE-1's **own** comment cites the notes.",
    body_tree: {
      content: [
        {
          content: [
            { text: "CORE-1's ", type: "text" },
            { marks: [{ type: "strong" }], text: "own", type: "text" },
            { text: " comment cites the notes.", type: "text" },
          ],
          type: "paragraph",
        },
      ],
      type: "doc",
    },
    created_at: "2026-11-15T10:00:00Z",
    edited_at: null,
    id: OWN_COMMENT,
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  },
];

// A reference's day is drawn without its year when it is the reader's year (`node-time.ts`), so
// each case runs at one fixed midday of the fixtures' year, and none reads the wall clock.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

/** `hour` o'clock of the reader's own `day` of November 2026, as the read writes a moment. */
function november(day: number, hour: number): string {
  return new Date(2026, 10, day, hour).toISOString();
}

/** The own comment's edge: what the issue's own comment cites of the document, with the stored
 *  markdown the read's excerpt is, which no row draws. */
const OWN_EDGE: GraphEdge = {
  created_at: november(15, 10),
  direction: "in",
  excerpt: { text: `CORE-1's **own** comment cites [the notes](${NOTES_REFERENCE}).` },
  kind: "mentions",
  node: graphNode({
    id: OWN_COMMENT,
    issue_key: KEY,
    kind: "comment",
    project: "CORE",
    ref: `dispatch://${KEY}/comment/${OWN_COMMENT}`,
  }),
  source_seq: 7,
};

/** A check of the issue's Verification document, which serves a statement of the notes. */
function checkEdge(index: number): GraphEdge {
  return {
    created_at: november(14, 9),
    direction: "in",
    kind: "serves",
    node: graphNode({
      block_id: `c${index}`,
      document: `dispatch://${KEY}/artifact/verification-md`,
      id: `verification-md#c${index}`,
      issue_key: KEY,
      kind: "block",
      ref: `dispatch://${KEY}/artifact/verification-md#b-c${index}`,
    }),
    source_seq: null,
  };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

/** `edges` drawn as the opened document `NOTES` draws its list, with the issue's own comments read
 *  as the outline has read them. */
function draw(edges: readonly GraphEdge[], comments: readonly CommentWithTrees[] = COMMENTS) {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [...edges],
    node: graphNode({ id: "artifact-notes", kind: "artifact", ref: NOTES_REFERENCE }),
  } satisfies GraphReferences);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["comments", KEY], comments);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentReferences artifact={NOTES} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
    client.clear();
  };
  return { getReferences };
}

test("an opened document's Referenced by keeps the issue's own comment", async () => {
  const { getReferences } = draw([OWN_EDGE]);
  const list = screen.getByRole("region", { name: "Referenced by" });
  // The comment is of the document's own issue, which the new page's Referenced by panel leaves to
  // the outline (`listedEdges`); it is named by its words, as the outline's row names it, with the
  // marks its author typed gone.
  const own = await within(list).findByRole("link", { name: OWN_WORDS });
  expect(own).toHaveAttribute("href", `/issues/${KEY}/comments/${OWN_COMMENT}`);
  expect(own, "the comment's words are not marked as an author's text").toHaveAttribute(
    "data-author-text"
  );
  expect(getReferences.mock.calls).toEqual([[NOTES_REFERENCE]]);
});

test("an opened document's list of what references it names each row as Details' list does", async () => {
  const check = checkEdge(1);
  const elsewhere: GraphEdge = {
    created_at: november(13, 10),
    direction: "in",
    excerpt: { text: `It cites **the notes** at [${NOTES_REFERENCE}](${NOTES_REFERENCE}).` },
    kind: "mentions",
    node: graphNode({
      id: OTHER_COMMENT,
      issue_key: "CORE-2",
      kind: "comment",
      project: "CORE",
      ref: `dispatch://CORE-2/comment/${OTHER_COMMENT}`,
    }),
    source_seq: 9,
  };
  // An edge of a kind this page does not know, which a newer Dispatch can answer with, is left
  // out, as every list of a node's references leaves it.
  const unknown = { ...checkEdge(2), kind: "verifies" } as unknown as GraphEdge;
  draw([OWN_EDGE, check, elsewhere, unknown]);
  const list = screen.getByRole("region", { name: "Referenced by" });
  await within(list).findByRole("link", { name: OWN_WORDS });
  const items = within(list).getAllByRole("listitem");
  expect(items, "the list draws the unknown edge, or leaves out a row it should draw").toHaveLength(
    3
  );
  const text = (item: HTMLElement | undefined) => (item?.textContent ?? "").replace(/\s+/g, " ");

  // The issue's own comment: its words, beside the page's word for what it is, with no issue
  // named, and the day it was made.
  expect(text(items[0])).toContain(NODE_REFERENCES_WORDS.kinds.comment);
  expect(items[0]?.querySelector("[data-reference-source]")).toBeNull();
  expect(items[0]?.querySelector("[data-reference-time]")?.textContent).toBe("15 Nov");

  // A check of the Verification document: the word of its tie, which is its link, and never its
  // address, its slug or its block's id.
  const served = within(items[1] as HTMLElement).getByRole("link", {
    name: NODE_REFERENCES_WORDS.edges.serves,
  });
  expect(served).toHaveAttribute("href", `/issues/${KEY}/artifacts/verification-md#b-c1`);
  for (const name of ["dispatch://", "verification-md", "c1", "#b-"]) {
    expect(text(items[1]), `the check's row names it by "${name}"`).not.toContain(name);
  }
  expect(items[1]?.querySelector("[data-reference-time]")?.textContent).toBe("14 Nov");

  // A comment of another issue: the page holds no words of it, so it is named by its kind's word,
  // with its issue and its day; the read's excerpt, the markup its author typed, is drawn nowhere.
  const other = within(items[2] as HTMLElement).getByRole("link", {
    name: NODE_REFERENCES_WORDS.kinds.comment,
  });
  expect(other).toHaveAttribute("href", `/issues/CORE-2/comments/${OTHER_COMMENT}`);
  expect(items[2]?.querySelector("[data-reference-source]")?.textContent).toBe("CORE-2");
  expect(items[2]?.querySelector("[data-reference-time]")?.textContent).toBe("13 Nov");
  for (const stored of ["**", "dispatch://", "It cites"]) {
    expect(text(items[2]), `the row draws "${stored}" of the stored excerpt`).not.toContain(stored);
  }
});

test("the list draws 20 rows and a control that names how many are left", async () => {
  // Twenty-five checks of the Verification document, each a node of its own.
  draw(Array.from({ length: 25 }, (_, index) => checkEdge(index + 1)));
  const list = screen.getByRole("region", { name: "Referenced by" });
  const rows = () => within(list).getAllByRole("listitem");
  await waitFor(() => expect(rows(), "the list does not draw its first 20 rows").toHaveLength(20));
  const more = within(list).getByRole("button", { name: SHOW_MORE });
  expect(more.textContent).toBe(showMoreName(5));

  await userEvent.click(more);
  await waitFor(() => expect(rows()).toHaveLength(25));
  expect(
    within(list).queryByRole("button", { name: SHOW_MORE }),
    "Show more stayed once every row was drawn"
  ).toBeNull();
});
