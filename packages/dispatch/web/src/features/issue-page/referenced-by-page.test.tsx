import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { issueQuery, userStateQuery } from "../../api/queries";
import type { GraphEdge, GraphReferences, IssueDetails, UserState } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { stateForIssue } from "../issue/pins";
import { MarginProvider } from "../margin/margin-context";
import { issue as marginIssue } from "../margin/margin-fixture";
import type { BlockItemKind } from "../refs/block-reference";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference } from "../refs/routes";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { referencedByControlName } from "./details-names";
import { emptyDetailsReads } from "./details-reads-fixture";
import type { IssueField } from "./IssueFieldForms";
import { IssueMarginPanel } from "./IssueMargin";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { NoTooltips } from "./NoTooltips";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";
import { treeAnswer } from "./tree-read/tree-fixtures";
import { issueTreeQuery, type TreeAnswer } from "./tree-read/tree-read";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

// On the new page the outline is the issue's conversation, so Details' list of what references
// the issue leaves out what the issue's own asks, comments and messages cite of it, and its
// control counts the rows the list shows.

const REFERENCE = buildDispatchReference({ key: "CORE-1", kind: "issue" });
const CREATED_AT = "2026-11-16T10:00:00Z";

// Each reference says how long ago it was made ("3 days ago"), which reads the clock. Each case
// runs at one fixed midday after `CREATED_AT`, so none reads the wall clock. `setSystemTime`
// holds `Date` alone; the timers the cases wait on (React Query's, `waitFor`'s) run on the real
// clock as before.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

const OWN_COMMENT = "11111111-1111-4111-8111-111111111111";
const OWN_ASK = "22222222-2222-4222-8222-222222222222";
const OWN_MESSAGE = "33333333-3333-4333-8333-333333333333";
const OTHER_COMMENT = "44444444-4444-4444-8444-444444444444";

const WORDS = {
  otherComment: "CORE-2's comment cites CORE-1.",
  ownAsk: "CORE-1's own ask cites it.",
  ownBlock: "A block of CORE-1's own comment cites it.",
  ownComment: "CORE-1's own comment cites it.",
  ownMessage: "CORE-1's own message cites it.",
} as const;

/** An edge of kind `mentions` from an ask, comment or message of `issueKey`, whose text cites
 *  CORE-1. */
function itemEdge(issueKey: string, kind: BlockItemKind, id: string, text: string): GraphEdge {
  return {
    created_at: CREATED_AT,
    direction: "in",
    excerpt: { text },
    kind: "mentions",
    node: graphNode({
      id,
      issue_key: issueKey,
      kind,
      project: "CORE",
      ref: `dispatch://${issueKey}/${kind}/${id}`,
    }),
    source_seq: 7,
  };
}

const OWN_COMMENT_REFERENCE = `dispatch://CORE-1/comment/${OWN_COMMENT}`;

/** The one read of what references CORE-1: its own comment, a block of it, its own ask, its own
 *  message, and CORE-2's comment. */
const READ: GraphReferences = {
  edges: [
    itemEdge("CORE-1", "comment", OWN_COMMENT, WORDS.ownComment),
    // Block `b2` of CORE-1's own comment, as the graph lists a mention a block of a comment
    // holds: its node is the block, naming no document, and its parent the comment.
    {
      created_at: CREATED_AT,
      direction: "in",
      excerpt: { block_id: "b2", text: WORDS.ownBlock },
      kind: "mentions",
      node: graphNode({
        block_id: "b2",
        id: `comment/${OWN_COMMENT}#b2`,
        issue_key: "CORE-1",
        kind: "block",
        parent: { id: OWN_COMMENT, kind: "comment", ref: OWN_COMMENT_REFERENCE },
        project: "CORE",
        ref: `${OWN_COMMENT_REFERENCE}#b-b2`,
      }),
      source_seq: 8,
    },
    itemEdge("CORE-1", "ask", OWN_ASK, WORDS.ownAsk),
    itemEdge("CORE-1", "message", OWN_MESSAGE, WORDS.ownMessage),
    itemEdge("CORE-2", "comment", OTHER_COMMENT, WORDS.otherComment),
  ],
  node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: REFERENCE }),
};

test("with page false an issue's own comment that references it is listed, and with page true it, a block of it, an own ask and an own message are left out while another issue's comment is kept", async () => {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(READ);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const panel = (page: boolean) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy page={page} reference={REFERENCE} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(panel(false));

  try {
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(await within(list).findByText(WORDS.ownComment)).toBeInTheDocument();
    expect(within(list).getByRole("link", { name: "Comment · CORE-1" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/comments/${OWN_COMMENT}`
    );
    expect(within(list).getByRole("heading")).toHaveTextContent("Referenced by (5)");

    view.rerender(panel(true));
    const pageList = screen.getByRole("region", { name: "Referenced by" });
    expect(
      within(pageList).queryByText(WORDS.ownComment),
      "the page lists the issue's own comment"
    ).toBeNull();
    expect(
      within(pageList).queryByText(WORDS.ownBlock),
      "the page lists a block of the issue's own comment"
    ).toBeNull();
    expect(
      within(pageList).queryByText(WORDS.ownAsk),
      "the page lists the issue's own ask"
    ).toBeNull();
    expect(
      within(pageList).queryByText(WORDS.ownMessage),
      "the page lists the issue's own message"
    ).toBeNull();
    const sources = pageList.querySelectorAll<HTMLElement>(":scope > ul > li");
    expect(sources).toHaveLength(1);
    const kept = within(sources[0] as HTMLElement);
    expect(kept.getByRole("link", { name: "Comment · CORE-2" })).toHaveAttribute(
      "href",
      `/issues/CORE-2/comments/${OTHER_COMMENT}`
    );
    expect(kept.getByText(WORDS.otherComment)).toBeInTheDocument();
    expect(within(pageList).getByRole("heading")).toHaveTextContent("Referenced by (1)");
    // Every colour the list draws in Details is the page's: its link and its
    // excerpt as much as its heading and its lines, so no element carries the application's.
    const applicationColours = new Set(
      [linkText, linkHoverText, textSecondaryOnSurface, textMutedOnSurface, borderDefault].flatMap(
        (classes) => classes.split(" ")
      )
    );
    expect(
      [pageList, ...pageList.querySelectorAll("*")]
        .filter((element) => [...element.classList].some((name) => applicationColours.has(name)))
        .map((element) => element.outerHTML),
      "the page's list draws the application's colours"
    ).toEqual([]);
  } finally {
    view.unmount();
    client.clear();
    getReferences.mockRestore();
  }
});

// In Details the Retry of a failed read of what references the issue is not disabled while it
// reads: Chromium would drop its focus to the page (`chromium-focus-fixup.ts`). A keyboard
// reader on the Retry of the list's failed first read stays on it while the read is out. Details
// passes `retried` beside `page`, so the case does too. A second press while that read is out
// joins it whatever the press asks, since the query holds no data to keep (TanStack's
// `Query.fetch`), so no case counts its reads.
test("Details' Referenced by Retry pressed by keyboard after the list's first read failed keeps the focus while it reads", async () => {
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new Error("the references are down")
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy page reference={REFERENCE} retried={{ current: false }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  let restoreFocus: (() => void) | undefined;

  try {
    const list = await screen.findByRole("region", { name: "Referenced by" });
    const retry = await within(list).findByRole("button", { name: "Retry" });
    restoreFocus = emulateChromiumFocusFixup();
    // The read the Retry sends is held out until the case lets it land.
    const again = Promise.withResolvers<GraphReferences>();
    getReferences.mockImplementation(() => again.promise);
    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-disabled", "true"));
    expect(
      retry,
      "Details' Referenced by Retry left the focus on the page while it read"
    ).toHaveFocus();
    await act(async () => {
      again.resolve(READ);
    });
    await within(list).findByText(WORDS.otherComment);
    expect(within(list).queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    restoreFocus?.();
    view.unmount();
    client.clear();
    getReferences.mockRestore();
  }
});

const issue: IssueDetails = { ...marginIssue, last_seq: 250, status: "todo" };

/** Details as the details slot draws it (`slots/details.tsx`), on the cached issue, inside the
 *  Details drawer `drawer` (`IssueMargin.test.tsx`'s `renderDetails`). */
function renderDetails(drawer: MarginDrawer) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  // The tree read with no view, as the page holds it once Graph has read it, here with no run in
  // it: Details' run rows read it at the list's opening only where the page holds none
  // (`useRunRows`), so the list's cases here stub no tree read.
  queryClient.setQueryData<TreeAnswer>(issueTreeQuery("CORE-1").queryKey, treeAnswer([]));
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawer={drawer}>
            <NoTooltips>
              <CachedIssue>
                {(details, state) => <Details issue={details} state={state} />}
              </CachedIssue>
            </NoTooltips>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { unmount: view.unmount };
}

function CachedIssue({
  children,
}: {
  children: (issue: IssueDetails, state: UserState | undefined) => ReactNode;
}): ReactNode {
  const details = useQuery(issueQuery("CORE-1")).data;
  const state = useQuery(userStateQuery()).data;
  return details === undefined ? null : children(details, state);
}

function Details({ issue, state }: { issue: IssueDetails; state: UserState | undefined }) {
  const writes = useIssueHeaderWrites(issue, stateForIssue(state, issue.key));
  const [editingField, setEditingField] = useState<IssueField | null>(null);
  return (
    <IssueMarginPanel
      documentArtifact={primarySpec(issue)}
      editingField={editingField}
      isClosed={issue.closed_at !== null}
      issue={issue}
      onEditingField={setEditingField}
      state={state}
      writes={writes}
    />
  );
}

/** The reads open Details makes of the fixture's issue beside the references: the issue's own,
 *  answered with `details`, and every other read as `emptyDetailsReads` answers it, the reader's
 *  state holding the issue unread and unpinned. */
function stubDetailsReads(details: IssueDetails): () => void {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(details);
  const reads = emptyDetailsReads();
  return () => {
    getIssue.mockRestore();
    reads.restore();
  };
}

test("Details' Referenced by reads no count until its list is read, then the rows it lists", async () => {
  const user = userEvent.setup();
  const restore = stubDetailsReads({ ...issue, referenced_by_count: 5 });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(READ);
  // The list's own read: the issue's reference with no kind. Details' Blocks reads the same
  // reference for `blocked_by` alone while the drawer is open.
  const listReads = () =>
    getReferences.mock.calls.filter(([to, options]) => to === REFERENCE && options === undefined)
      .length;
  const drawer: MarginDrawer = { open: true, setOpen: () => undefined, setPage: () => undefined };
  const { unmount } = renderDetails(drawer);

  try {
    const toggle = await screen.findByRole("button", { name: referencedByControlName(undefined) });
    await waitFor(() =>
      expect(getReferences).toHaveBeenCalledWith(REFERENCE, { kind: "blocked_by" })
    );
    expect(toggle).toHaveAccessibleName(referencedByControlName(undefined));
    expect(listReads(), "Details read the list before the reader opened it").toBe(0);

    await user.click(toggle);
    expect(
      await screen.findByRole("button", { name: referencedByControlName(1) }),
      "Referenced by's control does not read the rows its list shows"
    ).toBe(toggle);
    expect(listReads()).toBe(1);
    const list = screen.getByRole("region", { name: "Referenced by" });
    expect(within(list).getByRole("link", { name: "Comment · CORE-2" })).toBeInTheDocument();
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// On every other page an excerpt longer than three lines is clamped to them, with Show more to
// open it. With `page`, Details draws each citing comment's excerpt whole, with no clamp and no
// Show more, since a row of Details' references is no place the page shortens.
test("with page each excerpt is drawn whole, with no clamp and no Show more, and without page it is clamped as before", async () => {
  const excerpt = Array.from(
    { length: 6 },
    (_, index) => `Line ${index + 1} of CORE-2's comment cites CORE-1.`
  ).join("\n");
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    ...READ,
    edges: [itemEdge("CORE-2", "comment", OTHER_COMMENT, excerpt)],
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const panel = (page: boolean) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy page={page} reference={REFERENCE} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(panel(false));
  const list = () => screen.getByRole("region", { name: "Referenced by" });
  const drawnExcerpt = () =>
    [...list().querySelectorAll("p")].find((line) => line.textContent === excerpt);

  try {
    await waitFor(() => expect(drawnExcerpt(), "the list draws no excerpt").toBeDefined());
    expect(drawnExcerpt()).toHaveClass("line-clamp-3");
    expect(within(list()).getByRole("button", { name: "Show more" })).toBeInTheDocument();

    view.rerender(panel(true));
    expect(drawnExcerpt(), "Details does not draw the excerpt whole").toBeDefined();
    expect(drawnExcerpt(), "Details clamps the excerpt").not.toHaveClass("line-clamp-3");
    expect(
      within(list()).queryByRole("button", { name: /^Show (more|less)$/u }),
      "Details offers more of an excerpt it draws whole"
    ).toBeNull();
  } finally {
    view.unmount();
    client.clear();
    getReferences.mockRestore();
  }
});

/** The `n`th comment of CORE-2 that cites CORE-1, each a source of its own. */
function otherIssueComment(n: number): GraphEdge {
  return itemEdge(
    "CORE-2",
    "comment",
    `44444444-4444-4444-8444-${String(n).padStart(12, "0")}`,
    `CORE-2's comment ${n} cites CORE-1.`
  );
}

/** Block `block` of CORE-3's notes, which cites CORE-1: the notes are one source, whose blocks
 *  are its rows. */
function notesBlock(block: string): GraphEdge {
  return {
    created_at: CREATED_AT,
    direction: "in",
    excerpt: { block_id: block, text: `Block ${block} of CORE-3's notes cites CORE-1.` },
    kind: "mentions",
    node: graphNode({
      block_id: block,
      document: "dispatch://CORE-3/artifact/notes-md",
      id: `notes-md#${block}`,
      issue_key: "CORE-3",
      kind: "block",
      project: "CORE",
      ref: `dispatch://CORE-3/artifact/notes-md#b-${block}`,
    }),
    source_seq: 9,
  };
}

// Details draws what references the issue 20 rows at a time, with Show more under them saying how
// many rows are left, and its control counts every row. The rows are one list across the sources
// that cite the issue: a source's card the cut falls in draws its rows before the cut, and the
// next press draws the rest of them, with the focus on the first row it drew; a press whose first
// row opens a card moves the focus to that card's link, which names the source.
test("Details' Referenced by draws 20 rows at a time as one list across its sources, and its control counts them all", async () => {
  const user = userEvent.setup();
  const restore = stubDetailsReads({ ...issue, referenced_by_count: 45 });
  const comments = Array.from({ length: 42 }, (_, index) => otherIssueComment(index + 1));
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [
      ...comments.slice(0, 19),
      notesBlock("b1"),
      notesBlock("b2"),
      notesBlock("b3"),
      ...comments.slice(19),
    ],
    node: READ.node,
  });
  const drawer: MarginDrawer = { open: true, setOpen: () => undefined, setPage: () => undefined };
  const { unmount } = renderDetails(drawer);

  try {
    await user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    expect(
      await screen.findByRole("button", { name: referencedByControlName(45) }),
      "Referenced by's control does not count every row of the list"
    ).toBeInTheDocument();
    const list = screen.getByRole("region", { name: "Referenced by" });
    const rows = () => [...list.querySelectorAll<HTMLElement>(":scope > ul > li > ul > li")];
    expect(rows(), "the list does not draw its first 20 rows").toHaveLength(20);
    const notes = rows()[19]?.parentElement;
    expect(notes?.children, "the notes' card does not draw its rows before the cut").toHaveLength(
      1
    );
    const more = within(list).getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(25));

    act(() => more.focus());
    await user.keyboard("{Enter}");
    expect(rows(), "Show more did not draw the next 20 rows").toHaveLength(40);
    expect(notes?.children, "the notes' card did not draw the rest of its rows").toHaveLength(3);
    expect(more.textContent).toBe(showMoreName(5));
    expect(
      rows()[20]?.contains(document.activeElement),
      "Show more did not move the focus to the first row it drew"
    ).toBe(true);

    act(() => more.focus());
    await user.keyboard("{Enter}");
    expect(rows()).toHaveLength(45);
    expect(more.isConnected, "Show more stayed once every row was drawn").toBe(false);
    const opened = rows()[40]?.parentElement?.parentElement;
    if (opened === null || opened === undefined) throw new Error("the list drew no 41st row");
    expect(
      document.activeElement,
      "Show more did not move the focus to the link of the card its first row opens"
    ).toBe(opened.querySelector("a[href]"));
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});
