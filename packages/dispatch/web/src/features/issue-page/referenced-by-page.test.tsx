import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { issueQuery, issueTreeKey, userStateQuery } from "../../api/queries";
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
import type { IssueField } from "./IssueFieldForms";
import { IssueMarginPanel } from "./IssueMargin";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { NoTooltips } from "./NoTooltips";
import type { TreeAnswer } from "./tree-read/tree-read";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

// On the new page the outline is the issue's conversation, so Details' list of what references
// the issue leaves out what the issue's own asks, comments and messages cite of it, and its
// control counts the rows the list shows (dispatch://INFRA-3948 R10, decision 76).

const REFERENCE = buildDispatchReference({ key: "CORE-1", kind: "issue" });
const CREATED_AT = "2026-11-16T10:00:00Z";

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
    // Every colour the list draws in Details is the page's (R10, decision 56): its link and its
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

const issue: IssueDetails = { ...marginIssue, last_seq: 250, status: "todo" };

/** Details as the details slot draws it (`slots/details.tsx`), on the cached issue, inside the
 *  Details drawer `drawer` (`IssueMargin.test.tsx`'s `renderDetails`). */
function renderDetails(drawer: MarginDrawer) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  // The outline's tree read, as the page holds it by the time Details draws, here with no run in
  // it: Details' run rows only observe it (`useRunRows`), so Details drawn alone would wait on a
  // read nothing sends.
  queryClient.setQueryData<TreeAnswer>(issueTreeKey("CORE-1"), {
    converted: false,
    cursor: null,
    nodes: [],
    tree: null,
    tuples: [],
    view_of: null,
  });
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

/** The reads open Details makes of the fixture's issue beside the references, each answering the
 *  API's empty shape (`IssueMargin.test.tsx`'s `stubIssueApi`); a read that reached the network
 *  would fail the file (`__tests__/network.ts`). */
function stubDetailsReads(details: IssueDetails): () => void {
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(details),
    spyOn(api, "getMyState").mockResolvedValue({
      "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
    }),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "listRepoProjects").mockResolvedValue([]),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  return () => {
    for (const spy of reads) spy.mockRestore();
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
    const toggle = await screen.findByRole("button", { name: "Referenced by" });
    await waitFor(() =>
      expect(getReferences).toHaveBeenCalledWith(REFERENCE, { kind: "blocked_by" })
    );
    expect(toggle).toHaveAccessibleName("Referenced by");
    expect(listReads(), "Details read the list before the reader opened it").toBe(0);

    await user.click(toggle);
    expect(
      await screen.findByRole("button", { name: "Referenced by (1)" }),
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
