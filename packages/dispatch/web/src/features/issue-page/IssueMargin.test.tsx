import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { MAX_ISSUE_ADVANCES, MAX_ISSUE_BLOCKERS } from "@legion/contracts";
import {
  type InfiniteData,
  QueryClient,
  QueryClientProvider,
  useQuery,
} from "@tanstack/react-query";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for an Escape that ends an IME composition, which user-event has no form for
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { holdNotifications } from "../../__tests__/commits";
import { realClock } from "../../__tests__/fake-clock";
import { nextFrame } from "../../__tests__/frames";
import { graphNode } from "../../__tests__/graph-fixture";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { eventRefreshTargets } from "../../api/event-refresh";
import { primarySpec } from "../../api/issue-cache";
import { issueQuery, issueTreeKey, referenceTargetsQuery, userStateQuery } from "../../api/queries";
import { refreshQueries } from "../../api/query-refresh";
import type {
  Actor,
  ArchitectureSource,
  CommentWithTrees,
  DispatchEvent,
  DisplayTree,
  GraphEdge,
  GraphReferences,
  Issue,
  IssueDetails,
  MessageWithTrees,
  Priorities,
  ReferenceTargets,
  UserState,
} from "../../api/types";
import { linkText, pageFailedText, pageHeaderControl, pageInkText } from "../../theme/classes";
import { assigneeControlName } from "../issue/AssigneeControl";
import { componentsControlName } from "../issue/IssueComponentsLine";
import { labelsControlName } from "../issue/IssueLabels";
import { conversationItemId, eventItemId, pinnedItemMarker, stateForIssue } from "../issue/pins";
import { repoControlName, repoTextControlName } from "../issue/RepoControl";
import { type DocumentBridge, MarginProvider, useMargin } from "../margin/margin-context";
import { issue as marginIssue, specArtifact } from "../margin/margin-fixture";
import {
  addPriorityControlName,
  IssuePriorityLine,
  priorityTriggerName,
} from "../priorities/IssuePriorityLine";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../shell/keymap";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { blockedByControlName, referencedByControlName } from "./details-names";
import { emptyDetailsReads } from "./details-reads-fixture";
import {
  DETAILS_OPENER_ATTRIBUTE,
  DETAILS_ROW_ATTRIBUTE,
  DETAILS_ROW_REQUEST_EVENT,
  type DetailsRow,
  detailsOpener,
} from "./details-rows";
import { Opener } from "./details-rows-fixture";
import type { IssueField } from "./IssueFieldForms";
import { IssueMarginPanel, PARENT_ERROR_ID, TITLES_READ_FAILED } from "./IssueMargin";
import {
  narrowIssue,
  patchAnswer,
  reviewerClaim,
  sessionClaim,
  settleFocus,
} from "./issue-fixtures";
import {
  DETAILS_ATTRIBUTE,
  FilledMarginSlot,
  type MarginDrawer,
  MarginSlotProvider,
  useMarginSlotFill,
} from "./margin-slot";
import { NoTooltips } from "./NoTooltips";
import { MARGIN_SECTION_NAMES, PROPERTY_LABELS } from "./page-structure";
import {
  ANSWER_LINES,
  BLOCKED_BY_REFUSALS,
  CONTROL_LABELS,
  issueStatusWord,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  STATE_LINES,
  sentenceCase,
} from "./page-words";
import { architecture, offPageColours } from "./shared-controls-fixture";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";
import { DetailsSlot } from "./slots/details";
import { treeAnswer, treeNode, treeTuple } from "./tree-read/tree-fixtures";
import { issueTreeQuery, subscribeTreeFreshness, type TreeAnswer } from "./tree-read/tree-read";
import { firstUsableIn, usableControl } from "./usable-control";
import { blockedByRefusal } from "./useIssueBlockedBy";
import { type IssueHeaderWrites, useIssueHeaderWrites } from "./useIssueHeaderWrites";
import { appClient, focusPastStaleness, restoreWindowFocus } from "./window-focus-fixture";

const issue: IssueDetails = { ...marginIssue, last_seq: 250, status: "todo" };

/** The fixture's issue, closed. */
const closedIssue: IssueDetails = { ...issue, closed_at: "2026-09-10T00:00:00Z", status: "done" };

/** The fixture's issue with its spec and `count` notes, named in order. */
function withNotes(count: number): IssueDetails {
  const notes = Array.from({ length: count }, (_, index) => {
    const slug = `notes-${String(index + 1).padStart(2, "0")}`;
    return {
      ...specArtifact,
      id: slug,
      name: `${slug}.md`,
      primary: false,
      ref_key: `CORE-1/${slug}`,
      slug,
    };
  });
  return { ...issue, artifacts: [specArtifact, ...notes] };
}

/** What references CORE-1, the answer of Referenced by's list: nothing. */
const NOTHING_REFERENCES: GraphReferences = {
  edges: [],
  node: graphNode({
    id: "CORE-1",
    issue_key: "CORE-1",
    kind: "issue",
    ref: buildDispatchReference({ key: "CORE-1", kind: "issue" }),
  }),
};

/** What references CORE-1 once another issue waits on it: one source, which the list links. */
const WAITER_REFERENCES: GraphReferences = {
  ...NOTHING_REFERENCES,
  edges: [waitingEdge("CORE-5", "Ship the release")],
};

/** What references CORE-1 once another session changes which issue waits on it: CORE-6 in place
 *  of CORE-5. */
const OTHER_WAITER_REFERENCES: GraphReferences = {
  ...NOTHING_REFERENCES,
  edges: [waitingEdge("CORE-6", "Write the notes")],
};

/** Reads what waits on CORE-1 again, the Blocks row's read, as an event of its references reads
 *  it: `getReferences` answers `answer` from then on, or, where `answer` is a failure, fails this
 *  read alone. */
async function readWhatWaits(
  queryClient: QueryClient,
  getReferences: Mock<typeof api.getReferences>,
  answer: GraphReferences | ApiError
): Promise<void> {
  if (answer instanceof ApiError) getReferences.mockRejectedValueOnce(answer);
  else getReferences.mockResolvedValue(answer);
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  await act(() =>
    queryClient.refetchQueries({ queryKey: ["references", "to", reference, "blocked_by"] })
  );
}

/** The failure a read of Details meets in the cases below. */
function readFailure(): ApiError {
  return new ApiError(500, { code: "INTERNAL", error: "read failed" });
}

/** The refusal a priority write meets in the cases below where another session moved the issue's
 *  set of priorities meanwhile (409 ADVANCES_REVISION). */
function advancesRefusal(): ApiError {
  return new ApiError(409, { code: "ADVANCES_REVISION", error: "the priorities moved meanwhile" });
}

/** The keys CORE-2, CORE-3 and on, `count` of them. */
function keysFrom2(count: number): string[] {
  return Array.from({ length: count }, (_, index) => `CORE-${index + 2}`);
}

/** How many entries each list of Details draws at first, and how many more each press of its
 *  Show more draws: 20. It is written here rather than read from the page, so a page that draws
 *  any other number of entries fails the cases below. */
const PAGE = 20;

/** The row of Details' Files and documents, whose label carries the count of its entries. */
function filesRow(): HTMLElement {
  const row = screen.getByTestId("issue-files").closest(".grid");
  if (!(row instanceof HTMLElement)) throw new Error("Details draws no Files and documents row");
  return row;
}

/** An open Details drawer, whose `setOpen` and `setPage` do nothing. */
const OPEN_DRAWER: MarginDrawer = {
  open: true,
  setOpen: () => undefined,
  setPage: () => undefined,
};

/** The same drawer, shut. */
const SHUT_DRAWER: MarginDrawer = { ...OPEN_DRAWER, open: false };

/** The page's words the labels control, the components picker and the Repository row's text
 *  control are named from inside `NoTooltips` (`labelsControlName`, `componentsControlName`,
 *  `repoTextControlName`). */
const CONTROL_NAME_WORDS = { ANSWER_LINES, CONTROL_LABELS };

/** The issue's tree read with no node and no tuple in it, as the outline reads it before Details
 *  draws (`tree-read/tree-read.ts`), in the route's shape (`tree-read/tree-fixtures.ts`). */
const EMPTY_TREE: TreeAnswer = treeAnswer([]);

/** A command bar run the reader asked that changed the issue, and the question they asked. */
const RUN = "6b1d3f5a-8c2e-4a7b-9d0f-1e2c3b4a5d6f";
const RUN_QUESTION = "Close this once the checks pass";

/** The issue's tree read as the asker of the command bar run `run` reads it once that run, asked
 *  as `question`, changed the issue: the `changed_by_run` tuple from the issue to the run, and the
 *  run's node, as the read answers a run such a tuple of a node it holds names
 *  (`issue_tree_read.go`'s `readContext`): a context node of the `interaction` kind and supertag,
 *  by the run's id, the asker's alone, with the question as its text. */
function treeWithRun(run: string, question: string): TreeAnswer {
  return treeAnswer(
    [
      treeNode(run, "interaction", null, {
        made_at: "2026-11-17T09:00:00Z",
        role: "context",
        text: question,
        visibility: "author",
      }),
    ],
    [treeTuple("changed_by_run", "CORE-1", run)]
  );
}

/** The client every read of Details goes through by default (`renderDetails`): one whose answered
 *  reads are never stale, so none of those is read again by its age or a window focus; a read that
 *  holds no answer, as one whose first read failed does, is stale all the same. */
function detailsQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
}

/** Details as the details slot draws it (`slots/details.tsx`), alone and inside `NoTooltips`:
 *  the panel over its own set of the issue's writes, on the cached issue, so a write's landing
 *  is what the panel draws next. `beside` is drawn above it, over writes of its own. `drawer` is
 *  the Details drawer it is drawn in; with none, Details makes none of the reads it makes only
 *  while open. `redraw` draws the same Details in the drawer it is handed, as a drawer that
 *  opens or closes does. `queryClient` is the client every read goes through, by default a new
 *  `detailsQueryClient()`. `heldTree` is the tree read's answer the page holds as Details draws,
 *  by default `EMPTY_TREE`; with null the page holds none. */
function renderDetails(
  beside?: (issue: IssueDetails) => ReactNode,
  drawer?: MarginDrawer,
  queryClient = detailsQueryClient(),
  heldTree: TreeAnswer | null = EMPTY_TREE
) {
  // The tree read with no view, as the page holds it once Graph has read it: Details' run rows
  // read it at the list's opening only where the page holds none (`useRunRows`), so a case that
  // opens the list over this answer stubs no tree read. A case that needs another answer puts it
  // in the client this returns.
  if (heldTree !== null) queryClient.setQueryData(issueTreeKey(issue.key), heldTree);
  const tree = (inDrawer: MarginDrawer | undefined) => (
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawer={inDrawer}>
            <NoTooltips>
              <CachedIssue>
                {(details, state) => (
                  <>
                    {beside?.(details)}
                    <Details issue={details} state={state} />
                  </>
                )}
              </CachedIssue>
            </NoTooltips>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(drawer));
  return {
    queryClient,
    redraw: (inDrawer: MarginDrawer) => view.rerender(tree(inDrawer)),
    unmount: view.unmount,
  };
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

/** The reads Details makes of the fixture's issue: the issue's own, answered with `issue`, and
 *  every other read as `emptyDetailsReads` answers it, the reader's state holding the issue
 *  unread and unpinned; and the issue's PATCH answered as the server does, with the narrow
 *  `Issue`. */
function stubIssueApi() {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const reads = emptyDetailsReads();
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) =>
    patchAnswer(issue, input)
  );

  return {
    getIssue,
    getMyState: reads.getMyState,
    patchIssue,
    pinReads: reads.pinReads,
    restore: () => {
      getIssue.mockRestore();
      reads.restore();
      patchIssue.mockRestore();
    },
  };
}

/** Draws `details` as the issue Dispatch holds now: the cached issue Details draws, and the
 *  answer to the issue's next read. */
function drawIssue(
  getIssue: Mock<typeof api.getIssue>,
  queryClient: QueryClient,
  details: IssueDetails
): void {
  getIssue.mockResolvedValue(details);
  act(() => {
    queryClient.setQueryData(issueQuery("CORE-1").queryKey, details);
  });
}

/** One row of Details, by the name in its label column (`details-leaves.tsx`'s `Row`). */
function detailsRow(name: string): HTMLElement {
  const row = screen
    .getByText(name, { exact: true, selector: "[data-page-chrome]" })
    .closest(".grid");
  if (!(row instanceof HTMLElement)) throw new Error(`Details draws no ${name} row`);
  return row;
}

/** The row of Details a control elsewhere on the page opens it at (`details-rows.ts`). */
function rowAt(row: DetailsRow): HTMLElement {
  const found = document.querySelector(`[${DETAILS_ROW_ATTRIBUTE}="${row}"]`);
  if (!(found instanceof HTMLElement)) throw new Error(`Details draws no ${row} row`);
  return found;
}

/** How many times `getReferences` read what waits on `reference`, the Blocks row's read: the
 *  references of kind `blocked_by` that point at it. */
function blocksReads(getReferences: Mock<typeof api.getReferences>, reference: string): number {
  return getReferences.mock.calls.filter(
    ([to, options]) => to === reference && options?.kind === "blocked_by"
  ).length;
}

/** How many times `titles` read the title of the issue at `address`: the Blocked by row's read
 *  of the titles of the issues this one waits on, for a blocker's, and the Blocks row's read of
 *  the titles of the issues that wait on it, for one of those. */
function titleReads(titles: Mock<typeof api.getReferenceTargets>, address: string): number {
  return titles.mock.calls.filter(([refs]) => refs.includes(address)).length;
}

/** The names read's answer (`getReferenceTargets`) for `refs`: each issue `titles` holds a title
 *  for, by key, named by that title where `refs` names its address, and nothing else. */
function issueNames(
  refs: readonly string[],
  titles: Readonly<Record<string, string>>
): ReferenceTargets {
  return {
    targets: Object.entries(titles).flatMap(([key, name]) => {
      const ref = buildDispatchReference({ key, kind: "issue" });
      return refs.includes(ref) ? [{ gone: false, kind: "issue", name, ref }] : [];
    }),
  };
}

/** The edge by which the issue `key` waits on CORE-1, as this Dispatch answers the Blocks row's
 *  read (`getReferences` of kind `blocked_by`): of kind `needs`, unless a case names another, and
 *  `excerpt` is the title the edge carries. */
function waitingEdge(key: string, excerpt: string, kind: GraphEdge["kind"] = "needs"): GraphEdge {
  return {
    created_at: "2026-10-26T00:00:00Z",
    direction: "in",
    excerpt: { text: excerpt },
    kind,
    node: graphNode({
      id: key,
      issue_key: key,
      kind: "issue",
      ref: buildDispatchReference({ key, kind: "issue" }),
    }),
    source_seq: 4,
  };
}

/** The `issue.updated` event of `key`, which waits on CORE-1 (`blocked_by`), titled `title`, that
 *  moved the fields `changed`, at `seq` in the feed, as the feed hands it to the refresh: the
 *  payload as the server writes it - the issue after the write, flat, with what the write moved,
 *  and CORE-1 named in `references_changed`, since the edge into it moved - in the shape the
 *  feed's own test builds it (`event-feed.test.ts`'s `issue`). */
function waiterUpdated(
  key: string,
  title: string,
  changed: readonly string[],
  seq: number
): DispatchEvent {
  const payload: Record<string, unknown> = {
    ancestors: [],
    assignee: null,
    blocked_by: ["CORE-1"],
    changed,
    closed_at: null,
    key,
    parent: null,
    project: "CORE",
    references_changed: [{ id: "CORE-1", kind: "issue" }],
    status: "todo",
    title,
  };
  return {
    actor: { id: "planner", kind: "session" },
    created_at: "2026-10-26T00:00:00Z",
    id: 12,
    issue_key: key,
    notify: true,
    payload,
    project: "CORE",
    seq,
    type: "issue.updated",
  } as DispatchEvent;
}

/** Every Escape that reaches the window from now on, by whether a control marked it taken: the
 *  page's keys pass over a taken press (`shell/keymap.ts`), and the phone's Details sheet closes
 *  on an Escape no control took (`shell/useDialog.ts`). */
function watchEscapes(): { readonly stop: () => void; readonly taken: boolean[] } {
  const taken: boolean[] = [];
  const record = (event: KeyboardEvent) => {
    if (event.key === "Escape") taken.push(event.defaultPrevented);
  };
  window.addEventListener("keydown", record);
  return { stop: () => window.removeEventListener("keydown", record), taken };
}

test("the blocked-by editor stays open on a refusal and says what Dispatch refused", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    )
  );
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    await user.click(keys);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-9"] },
        keyedAttempt
      )
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("There is no issue CORE-9.");
    expect(alert).not.toHaveTextContent("blocked_by");
    // The key is the reader's own text inside the page's sentence (c-plain-words).
    expect(within(alert).getByText("CORE-9")).toHaveAttribute("data-author-text");
    expect(
      (screen.getByLabelText("Issues CORE-1 waits on") as HTMLInputElement).value,
      "the refused list is no longer in the box for the reader to mend"
    ).toBe("CORE-9");
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A blocked-by save the reader stopped may have been saved: the editor says so, in the page's
// words and not as Dispatch's, and stays open with the list in it, rather than say the save
// failed. Its Save is a press of its own, so the sentence says Save sends what the box holds then
// as a new write, and Save does: under a key of its own, not the stopped save's.
test("a blocked-by save the reader stopped says it may have been saved, and Save sends it anew", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-9");
    jest.useFakeTimers();
    const held = userOnFakeClock();
    await held.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await stopTheLateWrite(queryClient);
    const stoppedLine = screen.getByRole("alert");
    expect(stoppedLine.textContent).toBe(
      stoppedWriteNote("what CORE-1 waits on", "saved", {
        anew: "Saving again sends what the box holds now as a new write.",
      })
    );
    expect(stoppedLine, "the stopped sentence is not marked as the page's words").toHaveAttribute(
      "data-page-chrome"
    );
    expect(stoppedLine, "the stopped sentence is marked as Dispatch's words").not.toHaveAttribute(
      "data-author-text"
    );
    expect(
      (screen.getByLabelText("Issues CORE-1 waits on") as HTMLInputElement).value,
      "the stopped list is no longer in the box"
    ).toBe("CORE-9");

    const stoppedKey = patchIssue.mock.calls[0]?.[2].key;
    const made = patchIssue.mock.calls.length;
    const saved = Promise.withResolvers<Issue>();
    patchIssue.mockImplementation(() => saved.promise);
    await held.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(made + 1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { blocked_by: ["CORE-9"] }, keyedAttempt);
    expect(
      patchIssue.mock.calls.at(-1)?.[2].key,
      "Save asked the stopped save again under its key"
    ).not.toBe(stoppedKey);
    // The new save lands, which closes the editor.
    await act(async () => saved.resolve(narrowIssue({ ...issue, blocked_by: ["CORE-9"] })));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
  } finally {
    realClock();
    unmount();
    restore();
  }
});

// A blocked-by save that ends without Dispatch's answer - an error of the page's own, which ends
// the write at once (`writeUnderKey`) - says the page's own line, marked as the page's words and
// not as Dispatch's. A refusal the page has no sentence for is what Dispatch said, marked so.
test("a blocked-by save that fails without Dispatch's answer says the page's line in the page's words", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockRejectedValueOnce(new Error("the page could not build the request"));
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    const failedLine = await screen.findByRole("alert");
    expect(failedLine.textContent).toBe("Could not save what CORE-1 waits on.");
    expect(failedLine, "the page's line is not marked as the page's words").toHaveAttribute(
      "data-page-chrome"
    );
    expect(failedLine, "the page's line is marked as Dispatch's words").not.toHaveAttribute(
      "data-author-text"
    );

    const said = "blocked_by must list issue keys";
    patchIssue.mockRejectedValueOnce(new ApiError(400, { code: "INVALID_INPUT", error: said }));
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(said));
    expect(
      screen.getByRole("alert"),
      "Dispatch's own words are not marked as Dispatch's"
    ).toHaveAttribute("data-author-text");
    expect(
      screen.getByRole("alert"),
      "Dispatch's own words are marked as the page's"
    ).not.toHaveAttribute("data-page-chrome");
  } finally {
    unmount();
    restore();
  }
});

test("a blocked-by that is set can be changed and cleared from the panel", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-2"] });
  const { unmount } = renderDetails();

  try {
    expect(await screen.findByRole("link", { name: "CORE-2" })).toBeInTheDocument();
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 1) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    expect(keys.value).toBe("CORE-2");

    await user.clear(keys);
    await user.paste("core-3");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-3"] },
        keyedAttempt
      )
    );
    // The editor closes on a landing, and the written list is what the panel draws.
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
    expect(await screen.findByRole("link", { name: "CORE-3" })).toBeInTheDocument();

    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 1) })
    );
    await user.clear(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { blocked_by: [] }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

// The refusal is drawn in the editor and nowhere else, so a reader who closed the editor while
// the save was out was shown no refusal at all. Cancel and Escape wait for the answer, and
// Cancel says so: it is drawn and announced as held for as long as the save is out, rather than
// as a control whose press does nothing. An Escape held so is still the editor's, in the box or
// on Cancel, and goes no further.
test("the blocked-by editor refuses Cancel and Escape while the save is out", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    await user.click(keys);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));

    const cancel = screen.getByRole("button", { name: CONTROL_LABELS.cancel });
    expect(
      cancel,
      "Cancel is drawn as a control while the save it would leave is out"
    ).toHaveAttribute("aria-disabled", "true");
    await user.click(cancel);
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Cancel closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    await user.click(keys);
    await user.keyboard("{Escape}");
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Escape closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    act(() => cancel.focus());
    await user.keyboard("{Escape}");
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Escape on Cancel closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    expect(
      escapes.taken,
      "an Escape the editor held while its save is out went on to the page's keys"
    ).toEqual([true, true]);

    // The answer closes it, which is the one thing that may.
    saving.resolve(narrowIssue({ ...issue, blocked_by: ["CORE-9"] }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

// A refusal belongs to the attempt that earned it. The editor held the last one until the next
// save started, so opening it again drew a refusal of a list the reader had already dropped.
test("the blocked-by editor opened again draws no refusal from the attempt before", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    )
  );
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    expect(await screen.findByRole("alert")).toHaveTextContent("There is no issue CORE-9.");
    // The answer gives Cancel back: the editor is the reader's to leave again. A refused Cancel
    // is `aria-disabled` rather than `disabled`, so both are read.
    expect(screen.getByRole("button", { name: CONTROL_LABELS.cancel })).toBeEnabled();
    expect(screen.getByRole("button", { name: CONTROL_LABELS.cancel })).not.toHaveAttribute(
      "aria-disabled",
      "true"
    );

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.cancel }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));
    expect(await screen.findByLabelText("Issues CORE-1 waits on")).toBeInTheDocument();
    expect(
      screen.queryByRole("alert"),
      "the editor draws the refusal of a save this reader never made"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A closed section hides its rows, and the blocked-by editor's state is the panel's, which stays
// drawn for the life of the page: a reader who closes Development while the save is out finds
// the editor, the list they typed and the refusal that landed meanwhile when they open it again.
test("closing Development keeps the blocked-by editor, its typed list and a refusal that lands after the close", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-2 CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-2", "CORE-9"] },
        keyedAttempt
      )
    );

    const development = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });
    await user.click(development);
    expect(
      screen.queryByRole("textbox", { name: "Issues CORE-1 waits on" }),
      "a closed Development still shows the blocked-by editor"
    ).toBeNull();
    saving.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    );
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    await user.click(development);
    expect(
      screen.queryAllByLabelText("Issues CORE-1 waits on"),
      "closing Development threw away the open blocked-by editor"
    ).toHaveLength(1);
    expect(
      screen.getByLabelText<HTMLInputElement>("Issues CORE-1 waits on").value,
      "closing Development threw away the list typed into the editor"
    ).toBe("CORE-2 CORE-9");
    expect(
      screen.queryByRole("alert")?.textContent,
      "the refusal that landed while Development was closed is drawn nowhere"
    ).toBe("There is no issue CORE-9.");
  } finally {
    unmount();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

// A closed section unmounted its rows, so what a row of a shared control holds of its own was
// thrown away as the reader closed Development: here the repository's box, with a name it refused
// as the reader left it for the section's heading, which the row alone says. The section hides
// its rows instead: the reader sees none of them while it is closed, and finds the box as they
// left it, the refused name still in it, when it opens again.
test("closing Development hides its rows and keeps the repository box's refused draft for its next open", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();
  const box = () => screen.queryByRole("combobox", { name: repoControlName("CORE-1") });

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    const typed = await screen.findByRole("combobox", { name: repoControlName("CORE-1") });
    await user.click(typed);
    await user.paste("acme");

    const development = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });
    await user.click(development);
    expect(development).toHaveAttribute("aria-expanded", "false");
    expect(box(), "a closed Development still shows the repository's box").toBeNull();
    expect(
      screen.queryByRole("button", { name: blockedByControlName("CORE-1", 0) }),
      "a closed Development still shows the Blocked by row's control"
    ).toBeNull();

    await user.click(development);
    expect(
      box(),
      "closing Development threw away the repository's box and its draft"
    ).not.toBeNull();
    expect(box(), "closing Development threw away the name typed into the box").toHaveValue("acme");
    expect(
      box(),
      "closing Development threw away the box's refusal of the name typed into it"
    ).toHaveAttribute("aria-invalid", "true");
  } finally {
    unmount();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

// A closed section keeps its rows on the page, hidden (`PropertySection`), so a Details opened
// with Development closed read what that section's rows draw, for rows its reader could not see:
// GitHub for each link, which reads it as it is drawn (`GitHubLink`), the titles of the issues
// this one waits on, and what waits on it. Each is read once Development opens.
test("Details open with Development closed reads no GitHub link, title or Blocks, and opening Development reads each", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({
    ...issue,
    blocked_by: ["CORE-3"],
    external_links: [
      { kind: "github_pr", url: "https://github.com/acme/web/pull/7" },
      { url: "https://github.com/acme/web/issues/8" },
    ],
  });
  // A reference with no head, so a pull request's read reads no checks after it.
  const githubRest = spyOn(api, "githubRest").mockImplementation(
    async () =>
      new Response(JSON.stringify({ state: "open", title: "Ship the web app" }), {
        headers: { "Content-Type": "application/json" },
      })
  );
  // The titles of the issues this one waits on, and what waits on it.
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const titles = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const kept = userPreferenceStorageKey("alice", "issue.margin-sections");
  window.localStorage.setItem(kept, "development");
  const { redraw, unmount } = renderDetails(undefined, SHUT_DRAWER);

  try {
    const development = await screen.findByRole("button", {
      name: MARGIN_SECTION_NAMES.development,
    });
    await waitFor(() => expect(development).toHaveAttribute("aria-expanded", "false"));
    redraw(OPEN_DRAWER);
    // The open drawer's reads start in the commit that opens it: its effects run here.
    await act(async () => {});
    expect(
      githubRest,
      "Details read GitHub for the links of a closed Development"
    ).not.toHaveBeenCalled();
    expect(
      titleReads(titles, blocker),
      "Details read the titles of the issues a closed Development's Blocked by waits on"
    ).toBe(0);
    expect(
      blocksReads(getReferences, reference),
      "Details read what waits on the issue for a closed Development"
    ).toBe(0);

    await user.click(development);
    await waitFor(() =>
      expect(
        githubRest.mock.calls.map(([path]) => path).sort(),
        "opening Development did not read each of its links"
      ).toEqual(["repos/acme/web/issues/8", "repos/acme/web/pulls/7"])
    );
    await waitFor(() => {
      expect(
        titleReads(titles, blocker),
        "opening Development did not read the titles of the issues it waits on"
      ).toBeGreaterThan(0);
      expect(
        blocksReads(getReferences, reference),
        "opening Development did not read what waits on the issue"
      ).toBeGreaterThan(0);
    });
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    githubRest.mockRestore();
    restore();
    window.localStorage.removeItem(kept);
  }
});

// Escape in the blocked-by box is the editor's: it closes it and is taken there, so the phone's
// Details sheet, which closes on an Escape no control took (`useDialog.ts`), stays open.
test("Escape in the blocked-by box closes its editor and goes no further", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = await screen.findByLabelText("Issues CORE-1 waits on");
    await waitFor(() => expect(keys).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(
      screen.queryByLabelText("Issues CORE-1 waits on"),
      "Escape left the blocked-by editor open"
    ).not.toBeInTheDocument();
    expect(
      escapes.taken,
      "Escape in the blocked-by box went on to the page's keys, which close Details on a phone"
    ).toEqual([true]);
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

// An Escape that ends an IME composition gives up what the reader was composing, and the
// blocked-by editor closed on it, with the list typed into it. The editor leaves that press alone,
// unmarked, as the page's other editors do, and a plain Escape still closes it.
test("an Escape that ends an IME composition leaves the blocked-by editor open with its list", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = await screen.findByLabelText<HTMLInputElement>("Issues CORE-1 waits on");
    await user.click(keys);
    await user.paste("CORE-2");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(keys, { isComposing: true, key: "Escape" });
    expect(
      screen.queryByLabelText("Issues CORE-1 waits on"),
      "an Escape that ended an IME composition closed the blocked-by editor"
    ).toBe(keys);
    expect(keys.value, "an Escape that ended an IME composition threw the typed list away").toBe(
      "CORE-2"
    );

    await user.keyboard("{Escape}");
    expect(
      screen.queryByLabelText("Issues CORE-1 waits on"),
      "Escape left the blocked-by editor open"
    ).not.toBeInTheDocument();
    expect(
      escapes.taken,
      "the editor marked the Escape that ended a composition taken, or left a plain one unmarked"
    ).toEqual([false, true]);
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

/** Opens an editor of a row by the row's control named `control`, then presses Escape on the
 *  editor's Save (named `save`) and, the editor opened again, on its Cancel. Each press is the
 *  editor's, as Escape in its box is: the editor (its box named `box`) is closed, the row's
 *  control holds the focus again, and the press reached the window, where the page's own Escape
 *  that closes Details and the phone's Details sheet listens (`shell/keymap.ts`, `useDialog.ts`),
 *  only marked taken, which that Escape passes over, or not at all, where the editor stopped it. */
async function expectEscapeLeavesEditor(
  user: UserEvent,
  { box, control, save }: { box: string; control: string; save: string }
): Promise<void> {
  const escapes = watchEscapes();
  try {
    for (const button of [save, CONTROL_LABELS.cancel]) {
      await user.click(await screen.findByRole("button", { name: control }));
      await waitFor(() => expect(screen.getByLabelText(box)).toHaveFocus());
      const pressed = screen.getByRole("button", { name: button });
      act(() => pressed.focus());
      const before = escapes.taken.length;
      await user.keyboard("{Escape}");
      expect(
        screen.queryByLabelText(box),
        `Escape on the editor's ${button} left the editor open`
      ).not.toBeInTheDocument();
      expect(
        document.activeElement,
        `Escape on the editor's ${button} did not give the focus back to the row's control`
      ).toBe(screen.getByRole("button", { name: control }));
      expect(
        escapes.taken.slice(before).every(Boolean),
        `Escape on the editor's ${button} went on to the page's keys, which close Details`
      ).toBe(true);
    }
  } finally {
    escapes.stop();
  }
}

// Escape in an editor's box closed that editor alone, but no control took an Escape on its Save
// or its Cancel, so the page's own Escape closed the whole of Details - the phone's Details sheet
// with it - and the focus fell to the page's body. Escape anywhere in an editor is the editor's.
test("Escape on the parent's Save or Cancel closes its editor alone, with the focus back on Set parent issue", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await expectEscapeLeavesEditor(user, {
      box: PROPERTY_LABELS.parent,
      control: CONTROL_LABELS.set_parent,
      save: CONTROL_LABELS.save_parent,
    });
  } finally {
    unmount();
    restore();
  }
});

test("Escape on the route's Save or Cancel closes its editor alone, with the focus back on the route's control", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await expectEscapeLeavesEditor(user, {
      box: PROPERTY_LABELS.route,
      control: "Messages default to no route",
      save: CONTROL_LABELS.save_route,
    });
  } finally {
    unmount();
    restore();
  }
});

test("Escape on the blocked-by Save or Cancel closes its editor alone, with the focus back on its Add", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await expectEscapeLeavesEditor(user, {
      box: "Issues CORE-1 waits on",
      control: blockedByControlName("CORE-1", 0),
      save: CONTROL_LABELS.save_blocked_by,
    });
  } finally {
    unmount();
    restore();
  }
});

// The box goes with the editor, so leaving it - by Cancel or by a save that lands - gives the
// focus back to the control that opened it, not to the page.
test("leaving the blocked-by editor gives the focus back to its Add or Edit", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      document.activeElement,
      "Cancel in the blocked-by editor left the focus on the page"
    ).toBe(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));

    await user.click(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-2");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        document.activeElement,
        "a save of the blocked-by list that landed left the focus on the page"
      ).toBe(screen.queryByRole("button", { name: blockedByControlName("CORE-1", 1) }))
    );
  } finally {
    unmount();
    restore();
  }
});

// On a closed issue the Blocked by row draws no control, so leaving its editor once the issue had
// closed under it gave the focus to no control that takes it, and it stayed on the page's body.
// It goes to the first issue the row waits on, and where the row waits on none, to Development's
// heading, the button that opens and closes the section the row is in.
test("on a closed issue, leaving the blocked-by editor hands the focus to the first issue it waits on, else to Development's heading", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await screen.findByLabelText("Issues CORE-1 waits on");
    drawIssue(getIssue, queryClient, closedIssue);
    await user.keyboard("{Escape}");
    expect(
      document.activeElement,
      "Escape in the blocked-by editor of a closed issue that waits on none did not give the focus to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));

    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-3"] });
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 1) })
    );
    await screen.findByLabelText("Issues CORE-1 waits on");
    drawIssue(getIssue, queryClient, { ...closedIssue, blocked_by: ["CORE-3"] });
    await user.keyboard("{Escape}");
    expect(
      document.activeElement,
      "Escape in the blocked-by editor of a closed issue did not give the focus to the first issue it waits on"
    ).toBe(within(detailsRow(PROPERTY_LABELS.blocked_by)).getByRole("link", { name: "CORE-3" }));
  } finally {
    unmount();
    restore();
  }
});

// Each refusal of a blocked-by list is a sentence in the page's words, with the key the refusal
// is about inside it as the reader's own text: a key Dispatch knows no issue for, the issue
// itself, and a list longer than an issue waits on.
test("each refusal of a blocked-by list is the page's sentence about the key it names", () => {
  const refused = (code: string, error: string) => new ApiError(400, { code, error });
  expect(
    blockedByRefusal(refused("BLOCKED_BY_INPUT", "blocked_by issue CORE-9 not found"), "CORE-1", [
      "CORE-2",
      "CORE-9",
    ])
  ).toEqual({ key: "CORE-9", words: "not_found" });
  expect(
    blockedByRefusal(refused("BLOCKED_BY_INPUT", "CORE-1 cannot be blocked by itself"), "CORE-1", [
      "CORE-1",
    ])
  ).toEqual({ key: "CORE-1", words: "itself" });
  const many = keysFrom2(MAX_ISSUE_BLOCKERS + 1);
  expect(
    blockedByRefusal(
      refused("BLOCKED_BY_INPUT", "blocked_by is 1 over the 20-item limit (21/20)"),
      "CORE-1",
      many
    )
  ).toEqual({ words: "too_many" });
  // Any other answer is none of these refusals, and is said as it was before: the server's own
  // words, or the page's line for a save that never reached it.
  expect(
    blockedByRefusal(new ApiError(500, { code: "INTERNAL", error: "boom" }), "CORE-1", ["CORE-2"])
  ).toBeUndefined();
});

// A list that would make the issue wait on itself through the issues it waits on, its parent or
// its children is refused as `DEPENDENCY_CYCLE`, whose message names the field by its wire token
// (`CORE-1 would wait on itself through blocked_by and parent links`). It is said in the page's
// words as the other refusals are, with the issue the server names as the reader's own text; a
// chain too long for the server to follow names none.
test("a blocked-by list that would make a loop is the page's sentence about the issue it names", () => {
  const refused = (error: string) => new ApiError(409, { code: "DEPENDENCY_CYCLE", error });
  expect(
    blockedByRefusal(
      refused("CORE-1 would wait on itself through blocked_by and parent links"),
      "CORE-1",
      ["CORE-7"]
    )
  ).toEqual({ key: "CORE-1", words: "cycle" });
  expect(
    blockedByRefusal(refused("the dependency graph is deeper than 64 links"), "CORE-1", ["CORE-7"])
  ).toEqual({ words: "too_deep" });

  // Each sentence is the page's own: no wire token, and the key in it only where one goes.
  const sentences = Object.values(BLOCKED_BY_REFUSALS).map(
    ({ after, before }) => `${before}KEY${after}`
  );
  expect(sentences).toEqual([
    "KEY would then wait on itself, through the issues it waits on, its parent or its children.",
    "KEY is this issue, and an issue cannot wait on itself.",
    "There is no issue KEY.",
    "These issues wait on one another through more links than Dispatch follows, so it cannot check them for a loop.KEY",
    `An issue can wait on at most ${MAX_ISSUE_BLOCKERS} issues.KEY`,
  ]);
  for (const sentence of sentences) {
    expect(sentence).not.toMatch(/blocked_by|_/u);
  }
});

// Only a list of unbounded length draws 20 entries at a time under Show more. An issue waits on at
// most `MAX_ISSUE_BLOCKERS` issues, so the Blocked by row draws every one of them and no Show more.
test("the blockers never fold", async () => {
  const { getIssue, restore } = stubIssueApi();
  const blockers = keysFrom2(MAX_ISSUE_BLOCKERS);
  getIssue.mockResolvedValue({ ...issue, blocked_by: blockers });
  const { unmount } = renderDetails();

  try {
    await screen.findByRole("link", { name: "CORE-2" });
    const row = detailsRow(PROPERTY_LABELS.blocked_by);
    expect(
      within(row)
        .getAllByRole("link")
        .map((link) => link.textContent),
      "the Blocked by row folds the issues this one waits on"
    ).toEqual(blockers);
    expect(
      within(row).queryByRole("button", { name: SHOW_MORE }),
      "the Blocked by row offers to show a list it already draws whole"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// What waits on the issue is read from the references that point at it, which another issue's
// write moves: the Blocks row is read again on the event of an issue that starts waiting on this
// one, as the backlink panels are (`api/event-refresh.ts`'s rule for `blocked_by`).
test("Details' Blocks is read again when another issue starts waiting on this one", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const node = graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference });
  let edges: GraphReferences["edges"] = [];
  const getReferences = spyOn(api, "getReferences").mockImplementation(async () => ({
    edges,
    node,
  }));
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    issueNames(refs, { "CORE-5": "Ship the release" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() => expect(blocksReads(getReferences, reference)).toBe(1));
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent(CONTROL_LABELS.none)
    );

    // CORE-5 starts waiting on CORE-1, and its event is what the feed hands the refresh, as
    // `__tests__/event-feed.test.ts` drives a blocker change.
    edges = [waitingEdge("CORE-5", "Ship the release")];
    refreshQueries(
      queryClient,
      eventRefreshTargets(waiterUpdated("CORE-5", "Ship the release", ["blocked_by"], 4), "alice")
    );

    await waitFor(() =>
      expect(
        blocksReads(getReferences, reference),
        "Blocks was not read again on CORE-5's event"
      ).toBe(2)
    );
    const row = detailsRow(PROPERTY_LABELS.blocks);
    expect(
      await within(row).findByRole("link", { name: "CORE-5" }),
      "Blocks does not draw the issue that started waiting on CORE-1"
    ).toHaveAttribute("href", "/issues/CORE-5");
    await waitFor(() => expect(row).toHaveTextContent("Ship the release"));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks draws each title from a names read of the waiting issues' addresses, as Blocked by
// draws its own, so the rename of an issue that waits on this one reads that title again
// (`api/event-refresh.ts`'s `namesReadsNaming`). The title an edge of what waits on the issue
// carries is not read again by a rename, which moves no `blocked_by`: the backlink panel the
// rename names (`references_changed`) is refreshed by its exact key, and this read's is longer.
test("Details' Blocks draws the new title of an issue that waits on this one once it is renamed", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  // The server's title of CORE-5, in each read that carries it: the excerpt of the edge into
  // CORE-1, and the name the names read gives its address.
  let title = "Ship the release";
  const getReferences = spyOn(api, "getReferences").mockImplementation(async () => ({
    edges: [waitingEdge("CORE-5", title)],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  }));
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    issueNames(refs, { "CORE-5": title })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.blocks));
    await within(row).findByRole("link", { name: "CORE-5" });
    await waitFor(() => expect(row).toHaveTextContent("Ship the release"));

    // CORE-5 is renamed. The payload is the server's: the issue after the write, with what it
    // moved, and the issues it waits on named in `references_changed`, since the excerpt of
    // each edge into them moved (`issueEdgeChanges`).
    title = "Ship the second release";
    refreshQueries(
      queryClient,
      eventRefreshTargets(waiterUpdated("CORE-5", title, ["title"], 5), "alice")
    );

    await waitFor(() =>
      expect(row, "Blocks keeps the old title of CORE-5 after its rename").toHaveTextContent(
        "Ship the second release"
      )
    );
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A change to the issues that wait on this one gives the names read of their titles a new key,
// which holds no answer until that read answers. While it is out, Blocks keeps drawing the titles
// it drew rather than drawing every issue by its key alone. Once it fails, Blocks draws the keys
// with its failure line, as it does for a first read that fails, and keeps that line, with its
// Retry and a reader's focus, while the read Retry sent is out.
test("Details' Blocks keeps the titles it drew while the names read of a changed set is out, until that read fails", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const waiter = buildDispatchReference({ key: "CORE-5", kind: "issue" });
  const newcomer = buildDispatchReference({ key: "CORE-6", kind: "issue" });
  let edges: GraphReferences["edges"] = [waitingEdge("CORE-5", "Ship the release")];
  const getReferences = spyOn(api, "getReferences").mockImplementation(async () => ({
    edges,
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  }));
  const held = Promise.withResolvers<ReferenceTargets>();
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    refs.includes(newcomer) ? held.promise : issueNames(refs, { "CORE-5": "Ship the release" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release")
    );

    // CORE-6 starts waiting on CORE-1 too, and the names read of the two is held out.
    edges = [...edges, waitingEdge("CORE-6", "Write the notes")];
    refreshQueries(
      queryClient,
      eventRefreshTargets(waiterUpdated("CORE-6", "Write the notes", ["blocked_by"], 4), "alice")
    );
    await blocksRow().findByRole("link", { name: "CORE-6" });
    await waitFor(() => expect(titleReads(titles, newcomer)).toBe(1));
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks drew CORE-5 by its key alone while the names read of the new set is out"
    ).toHaveTextContent("Ship the release");

    await act(async () =>
      held.reject(new ApiError(500, { code: "INTERNAL", error: "read failed" }))
    );
    const retry = await blocksRow().findByRole("button", { name: CONTROL_LABELS.retry });
    expect(blocksRow().getByRole("alert")).toHaveTextContent(TITLES_READ_FAILED);
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks drew a title its failed read holds no answer for"
    ).not.toHaveTextContent("Ship the release");

    const retried = Promise.withResolvers<ReferenceTargets>();
    titles.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titleReads(titles, newcomer)).toBe(2));
    expect(
      retry,
      "the line and its Retry went while the read Retry sent is out"
    ).toBeInTheDocument();
    expect(document.activeElement, "Retry lost the focus while the read it sent is out").toBe(
      retry
    );
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks drew the titles of the set before while the read Retry sent is out"
    ).not.toHaveTextContent("Ship the release");

    await act(async () =>
      retried.resolve(
        issueNames([waiter, newcomer], {
          "CORE-5": "Ship the release",
          "CORE-6": "Write the notes",
        })
      )
    );
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Write the notes")
    );
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Blocked by draws its titles from a names read of the issues it waits on, which a change to them
// gives a new key: while that read is out, the titles the row drew stay drawn.
test("Blocked by keeps the titles it drew while the names read of a changed list is out", async () => {
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const added = buildDispatchReference({ key: "CORE-4", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const held = Promise.withResolvers<ReferenceTargets>();
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    refs.includes(added) ? held.promise : issueNames(refs, { "CORE-3": "Fix the build" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );

    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-3", "CORE-4"] });
    await blockedBy().findByRole("link", { name: "CORE-4" });
    await waitFor(() => expect(titleReads(titles, added)).toBe(1));
    expect(
      detailsRow(PROPERTY_LABELS.blocked_by),
      "Blocked by drew CORE-3 by its key alone while the names read of the new list is out"
    ).toHaveTextContent("Fix the build");

    await act(async () =>
      held.resolve(
        issueNames([blocker, added], { "CORE-3": "Fix the build", "CORE-4": "Pin the toolchain" })
      )
    );
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Pin the toolchain")
    );
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks says None only for an answer that holds nothing waiting on the issue: while that answer
// is still out the row says nothing, rather than read as an issue nothing waits on.
test("Details' Blocks says nothing while its read is out, and None once it answers with none", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const answer = Promise.withResolvers<GraphReferences>();
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => answer.promise);
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() =>
      expect(getReferences).toHaveBeenCalledWith(reference, { kind: "blocked_by" })
    );
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks reads None while its read is out"
    ).not.toHaveTextContent(CONTROL_LABELS.none);

    answer.resolve({
      edges: [],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    });
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.blocks),
        "Blocks does not read None for an answer that holds nothing waiting on the issue"
      ).toHaveTextContent(CONTROL_LABELS.none)
    );
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// This Dispatch answers an issue's wait on this one as `needs` under the Blocks row's read: the
// row draws each issue waiting through it, and no issue whose edge into this one is of another
// kind.
test("Details' Blocks draws each issue that waits on this one through needs, and none of another kind", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [
      waitingEdge("CORE-5", "Ship the release"),
      waitingEdge("CORE-6", "Write the notes"),
      waitingEdge("CORE-7", "Cite the issue", "mentions"),
    ],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    issueNames(refs, {
      "CORE-5": "Ship the release",
      "CORE-6": "Write the notes",
      "CORE-7": "Cite the issue",
    })
  );
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() =>
      expect(
        within(detailsRow(PROPERTY_LABELS.blocks))
          .queryAllByRole("link")
          .map((link) => link.textContent),
        "Blocks does not draw each issue that waits on CORE-1, and only those"
      ).toEqual(["CORE-5", "CORE-6"])
    );
    expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release");
    expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Write the notes");
    expect(detailsRow(PROPERTY_LABELS.blocks)).not.toHaveTextContent("Cite the issue");
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A refresh of a read Details holds an answer for - on an event, or a window focus - that fails
// leaves the read standing on that answer (`standsFailed`): the row keeps what the answer drew and
// says no failure, since nothing it draws is missing. Each read of the panel whose failure Details
// says in a line of its own (`DetailsErrorLine`) is refreshed here with a failure: the titles of
// the issues this one waits on, what waits on it, and the titles of those. Referenced by's list,
// which draws its own line, has a case of its own.
test("a refresh that fails over an answer a row of Details holds keeps that answer drawn and says no failure", async () => {
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(WAITER_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    issueNames(refs, { "CORE-3": "Fix the build", "CORE-5": "Ship the release" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() => {
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build");
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release");
    });

    titles.mockRejectedValue(readFailure());
    await act(() => queryClient.refetchQueries({ queryKey: ["reference-targets"] }));
    await readWhatWaits(queryClient, getReferences, readFailure());
    const refreshed = [
      referenceTargetsQuery([buildDispatchReference({ key: "CORE-3", kind: "issue" })]).queryKey,
      referenceTargetsQuery([buildDispatchReference({ key: "CORE-5", kind: "issue" })]).queryKey,
      ["references", "to", reference, "blocked_by"],
    ].map((queryKey) => queryClient.getQueryState(queryKey));
    expect(
      refreshed.map((state) => [state?.status, state?.data !== undefined]),
      "a read of the panel was not refreshed with a failure over the answer it holds"
    ).toEqual([
      ["error", true],
      ["error", true],
      ["error", true],
    ]);

    for (const label of [PROPERTY_LABELS.blocked_by, PROPERTY_LABELS.blocks]) {
      const row = within(detailsRow(label));
      expect(
        row.queryByRole("alert"),
        `${label} says a failure over the answer it holds`
      ).toBeNull();
      expect(
        row.queryByRole("button", { name: CONTROL_LABELS.retry }),
        `${label} offers a Retry of a read that holds its answer`
      ).toBeNull();
    }
    expect(
      detailsRow(PROPERTY_LABELS.blocked_by),
      "Blocked by took away the titles its failed refresh left it"
    ).toHaveTextContent("Fix the build");
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks took away the titles its failed refresh left it"
    ).toHaveTextContent("Ship the release");
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A read that failed again was said on the same node in the same words, so a screen reader said
// nothing of it. Each failure is drawn afresh (`errorUpdateCount`), here of a read that failed on
// its first load and so holds no answer, while Retry stays the same button, holding the focus of a
// reader who pressed it by keyboard.
test("a read that fails again in the same words is said again, while Retry keeps the focus", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(readFailure());
  const retried = Promise.withResolvers<ReferenceTargets>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(1));
    const failed = await blockedBy().findByRole("alert");
    const retry = blockedBy().getByRole("button", { name: CONTROL_LABELS.retry });

    titles.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () =>
      retried.reject(new ApiError(500, { code: "INTERNAL", error: "read failed" }))
    );
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );

    const failedAgain = blockedBy().getByRole("alert");
    expect(failedAgain).toHaveTextContent(TITLES_READ_FAILED);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);
  } finally {
    stopBlurring();
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A read that failed on its first load holds no answer, and a fetch of such a read clears its
// failure, so the press of its Retry took the line, and the Retry with it, away while the read it
// sent was out: the focus of a reader who pressed Retry by keyboard went to the page's body. The
// line stays, its Retry busy and holding the focus, until the read answers, and the focus then
// goes on in the row rather than to the page: in Blocked by to the row's control, and in Blocks,
// which has no control, to the first issue the answer drew.
test("a read that failed on its first load keeps its Retry, with the focus, while the read is out, and hands the focus on in its row once it answers", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const getReferences = spyOn(api, "getReferences").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const titlesRetry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const titlesHeld = Promise.withResolvers<ReferenceTargets>();
    titles.mockImplementationOnce(() => titlesHeld.promise);
    act(() => titlesRetry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        titlesRetry,
        "the line and its Retry went while the titles Retry read again are out"
      ).toBeInTheDocument();
      expect(
        titlesRetry,
        "Retry does not read busy while the titles it read again are out"
      ).toHaveAttribute("aria-busy", "true");
    });
    expect(
      document.activeElement,
      "Retry lost the focus while the titles it read again are out"
    ).toBe(titlesRetry);
    await act(async () => titlesHeld.resolve({ targets: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the titles answered, and the focus fell to the page rather than to the row's control"
    ).toBe(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));

    const blocksRetry = await blocksRow().findByRole("button", { name: CONTROL_LABELS.retry });
    const blocksHeld = Promise.withResolvers<GraphReferences>();
    getReferences.mockImplementationOnce(() => blocksHeld.promise);
    act(() => blocksRetry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        blocksRetry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(
        blocksRetry,
        "Retry does not read busy while the read it sent again is out"
      ).toHaveAttribute("aria-busy", "true");
    });
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      blocksRetry
    );
    await act(async () =>
      blocksHeld.resolve({
        edges: [waitingEdge("CORE-5", "Ship the release")],
        node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
      })
    );
    const waiting = await blocksRow().findByRole("link", { name: "CORE-5" });
    expect(
      document.activeElement,
      "Blocks answered, and the focus fell to the page rather than to the first issue it drew"
    ).toBe(waiting);
  } finally {
    stopBlurring();
    unmount();
    getReferences.mockRestore();
    titles.mockRestore();
    restore();
  }
});

// Blocks has no control of its own, so where the read its Retry sent answers with nothing
// waiting on the issue, the focus Retry held goes to the None the row says then.
test("Blocks hands the focus of its Retry to its None when the read answers with none", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValue({
      edges: [],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    })
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    const none = await blocksRow().findByText(CONTROL_LABELS.none);
    expect(
      document.activeElement,
      "Blocks answered with none, and the focus fell to the page rather than to its None"
    ).toBe(none);
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Where the titles of the issues that wait on this one could not be read, Blocks draws each by its
// key and says so, with Retry, as Blocked by says its own; once the titles answer the line goes,
// and the focus its Retry held goes to the first issue the row draws rather than to the page.
test("Blocks says the titles it could not read, and its Retry draws them and hands the focus to the first issue", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const waiter = buildDispatchReference({ key: "CORE-5", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [waitingEdge("CORE-5", "Ship the release")],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const answered = Promise.withResolvers<ReferenceTargets>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    expect(blocksRow().getByRole("alert")).toHaveTextContent(TITLES_READ_FAILED);
    expect(
      blocksRow().getByRole("link", { name: "CORE-5" }),
      "Blocks drew no issue whose title it could not read"
    ).toHaveAttribute("href", "/issues/CORE-5");
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks drew a title it could not read"
    ).not.toHaveTextContent("Ship the release");

    titles.mockImplementationOnce(() => answered.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titleReads(titles, waiter)).toBe(2));
    await act(async () => answered.resolve(issueNames([waiter], { "CORE-5": "Ship the release" })));
    await waitFor(() =>
      expect(
        blocksRow().queryByRole("alert"),
        "the titles answered, but Blocks still says they failed"
      ).toBeNull()
    );
    expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release");
    expect(
      document.activeElement,
      "the titles answered, and the focus fell to the page rather than to the first issue Blocks draws"
    ).toBe(blocksRow().getByRole("link", { name: "CORE-5" }));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks said a read that failed again in the same words on the same node, so a screen reader
// said nothing of it. Each failure is drawn afresh (`errorUpdateCount`), here of a read that
// failed on its first load and so holds no answer, while Retry stays the same button, holding
// the focus of a reader who pressed it by keyboard.
test("Blocks says a read that fails again in the same words again, while its Retry keeps the focus", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValue({
      edges: [],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    })
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
  const retried = Promise.withResolvers<GraphReferences>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const failed = blocksRow().getByRole("alert");

    getReferences.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () =>
      retried.reject(new ApiError(500, { code: "INTERNAL", error: "read failed" }))
    );
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );

    const failedAgain = blocksRow().getByRole("alert");
    expect(failedAgain).toHaveTextContent("Could not read what this issue blocks.");
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);
  } finally {
    stopBlurring();
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks' line goes once what waits on the issue answers. Read again as an event of an issue
// that starts waiting on this one reads it, rather than by the reader's press of its Retry, that
// answer is no press of the reader's, so the focus Retry held stays on the page's body, as for
// every change the reader did not make, rather than going to the first issue the row draws.
test("Blocks read again on an event, not by its Retry, leaves the focus its Retry held on the page's body", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValue({
      edges: [waitingEdge("CORE-5", "Ship the release")],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    })
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    act(() => retry.focus());
    await act(() =>
      queryClient.refetchQueries({ queryKey: ["references", "to", reference, "blocked_by"] })
    );
    await blocksRow().findByRole("link", { name: "CORE-5" });
    expect(retry, "Blocks answered, but its Retry is still drawn").not.toBeInTheDocument();
    await settleFocus();
    expect(
      document.activeElement,
      "Blocks answered on an event, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks has a Retry for what waits on the issue and one for the titles of those, each with a
// press of its own. The reader presses the titles' Retry, and while its read is out an event reads
// what waits on the issue again and that read fails. Blocks still holds the answer it drew its
// list from, so that read does not stand failed (`standsFailed`): the list stays, and the titles'
// line with it, whose Retry keeps the focus and reads busy while its own read is out.
test("a failed refresh of what waits on the issue keeps Blocks' list, and the titles' Retry with its focus", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(WAITER_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    expect(blocksRow().getByRole("alert")).toHaveTextContent(TITLES_READ_FAILED);
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await readWhatWaits(queryClient, getReferences, readFailure());
    expect(
      queryClient.getQueryState(["references", "to", reference, "blocked_by"])?.status,
      "what waits on the issue was not read again with a failure"
    ).toBe("error");
    expect(
      blocksRow().queryByText("Could not read what this issue blocks."),
      "Blocks says a failure over the list it holds"
    ).toBeNull();
    expect(
      blocksRow().queryByRole("link", { name: "CORE-5" }),
      "Blocks took away the list a failed refresh left it"
    ).not.toBeNull();
    expect(
      retry,
      "the titles' Retry went with a refresh that failed over Blocks' list"
    ).toBeInTheDocument();
    expect(
      retry,
      "the titles' Retry stopped reading busy as a refresh failed over Blocks' list, though its own read is still out"
    ).toHaveAttribute("aria-busy", "true");
    await settleFocus();
    expect(
      document.activeElement,
      "the titles' Retry lost the focus as what waits on the issue failed again"
    ).toBe(retry);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// While the Blocked by editor is open the row draws no control of its own, so as the titles the
// Retry sent answer, the focus it held goes to the first issue the row waits on rather than to
// the page.
test("while the Blocked by editor is open, the titles' Retry hands its focus to the first issue the row waits on", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const answered = Promise.withResolvers<ReferenceTargets>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    await user.click(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
    await blockedBy().findByRole("textbox", { name: "Issues CORE-1 waits on" });

    titles.mockImplementationOnce(() => answered.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await act(async () => answered.resolve({ targets: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the titles answered with the editor open, and the focus went elsewhere than the first issue the row waits on"
    ).toBe(blockedBy().getByRole("link", { name: "CORE-3" }));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A closed issue's Blocked by draws no control, so as the titles the Retry sent answer, the
// focus it held goes to the first issue the row waits on rather than to the page.
test("on a closed issue, the titles' Retry hands its focus to the first issue the row waits on", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...closedIssue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const answered = Promise.withResolvers<ReferenceTargets>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    expect(
      blockedBy().queryByRole("button", { name: blockedByControlName("CORE-1", 1) }),
      "a closed issue's Blocked by draws its control"
    ).toBeNull();

    titles.mockImplementationOnce(() => answered.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await act(async () => answered.resolve({ targets: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the titles answered on a closed issue, and the focus fell to the page rather than to the first issue the row waits on"
    ).toBe(blockedBy().getByRole("link", { name: "CORE-3" }));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The read the titles' Retry sent answers, and a refresh of the same titles fails over that answer
// before the page draws it: the app tells the page of a read's change a task later (TanStack's
// notify scheduler, a timeout outside tests), so both can reach it in one draw. The answer reached
// what the press asked for, and the refresh that failed over it draws no line (`standsFailed`), so
// the line goes for the press's answer, and the focus Retry held goes on to the row's control
// rather than to the page.
test("the titles' Retry whose answer reaches the page with a refresh that failed over it hands its focus to the row's control", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const titlesKey = referenceTargetsQuery([blocker]).queryKey;
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(readFailure());
  const answered = Promise.withResolvers<ReferenceTargets>();
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    titles.mockImplementationOnce(() => answered.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titleReads(titles, blocker)).toBe(2));

    // The page is told of nothing until both have landed. The answer lands before the refresh is
    // sent: one sent while the read is out would join it rather than fail on its own.
    const held = holdNotifications();
    try {
      answered.resolve(issueNames([blocker], { "CORE-3": "Fix the build" }));
      await waitFor(() => expect(queryClient.getQueryState(titlesKey)?.status).toBe("success"));
      titles.mockRejectedValueOnce(readFailure());
      await queryClient.refetchQueries({ exact: true, queryKey: titlesKey });
    } finally {
      held.tell();
    }
    const titlesRead = queryClient.getQueryState(titlesKey);
    expect(
      [titlesRead?.status, titlesRead?.errorUpdateCount, titlesRead?.data !== undefined],
      "the refresh did not fail over the titles' answer"
    ).toEqual(["error", 2, true]);
    expect(
      blockedBy().queryByRole("alert"),
      "Blocked by says a failure over the titles it holds"
    ).toBeNull();
    expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build");
    expect(
      document.activeElement,
      "the titles answered the press, and the focus fell to the page rather than to the row's control"
    ).toBe(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// While the Blocked by editor is open, another session's write can leave the issue waiting on
// nothing: the titles' failure goes with the list, and its Retry with the focus. The reader pressed
// nothing, so the focus stays on the page's body, as for every change the reader did not make,
// rather than going to the editor's box.
test("while the Blocked by editor is open, a write elsewhere that leaves the issue waiting on nothing leaves the titles' Retry's focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    await user.click(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
    await blockedBy().findByRole("textbox", { name: "Issues CORE-1 waits on" });
    act(() => retry.focus());

    const read = queryClient.getQueryData<IssueDetails>(issueQuery("CORE-1").queryKey);
    if (read === undefined) throw new Error("Details reads no cached issue");
    act(() => queryClient.setQueryData(issueQuery("CORE-1").queryKey, { ...read, blocked_by: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the issue waits on nothing, but Blocked by still says its titles failed"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the issue waits on nothing with the editor open, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A press of the titles' Retry whose read fails again took nothing away: the line stays, with
// the focus on its Retry, and the press is answered. The titles answering later, read again as an
// event of a blocker's rename reads them, are no press of the reader's, so the focus stays on the
// page's body rather than going to the row's control.
test("the titles' Retry whose read failed again leaves its focus on the page's body when the titles answer later on an event", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets")
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }))
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const failed = blockedBy().getByRole("alert");
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(
        blockedBy().getByRole("alert"),
        "the titles failed again, said in the same line"
      ).not.toBe(failed)
    );
    expect(document.activeElement, "Retry lost the focus when its read failed again").toBe(retry);

    await act(() => queryClient.refetchQueries({ queryKey: ["reference-targets"] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the titles answered on an event after Retry's read failed, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The reader presses the titles' Retry, and while its read is out another session changes the
// issues this one waits on: the row reads the titles of the new set, and the line goes with the
// focus on its Retry. The read the press sent has not answered, so the focus stays on the page's
// body rather than going to the row's control.
test("the titles' Retry taken away by blockers changed elsewhere while its read is out leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-4"] });
    await blockedBy().findByRole("link", { name: "CORE-4" });
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the issue waits on another set, but Blocked by still says the titles of the old one failed"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the blockers changed elsewhere while the titles' read was out, and the focus left the page's body though that read had not answered"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The reader presses the titles' Retry of a set of blockers another session made, and while its
// read is out that session puts back the set before, whose titles Details read then: the row draws
// that cached answer, and the line goes with the focus on its Retry. That answer is no answer of
// the read the press sent, so the focus stays on the page's body rather than going to the row's
// control.
test("the titles' Retry taken away by blockers changed back elsewhere to a set already read leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    issueNames(refs, { "CORE-3": "Fix the build" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );
    titles.mockRejectedValueOnce(readFailure());
    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-4"] });
    const retry = await blockedBy().findByRole("button", { name: CONTROL_LABELS.retry });
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-3"] });
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );
    expect(
      blockedBy().queryByRole("alert"),
      "the issue waits on the set read before, but Blocked by still says the titles of the other failed"
    ).toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "the blockers changed back elsewhere while the titles' read was out, and the focus left the page's body though that read had not answered"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The titles of the issues this one waits on failed their first read, and answered as an event read
// them again. Another session then changes the set, whose titles fail, and the reader presses that
// line's Retry; while its read is out the session puts back the set before: the row draws its
// cached answer, and the line goes with the focus on its Retry. That read failed and then answered
// before the press, and is not the read the press was for, so the focus stays on the page's body
// rather than going to the row's control.
test("the titles' Retry taken away by blockers changed back elsewhere to a set whose read failed and then answered leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets")
    .mockImplementation(async (refs) => issueNames(refs, { "CORE-3": "Fix the build" }))
    .mockRejectedValueOnce(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    await waitFor(() =>
      expect(blockedBy().getByRole("alert")).toHaveTextContent(TITLES_READ_FAILED)
    );
    await act(() => queryClient.refetchQueries({ queryKey: ["reference-targets"] }));
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );
    titles.mockRejectedValueOnce(readFailure());
    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-4"] });
    const retry = await blockedBy().findByRole("button", { name: CONTROL_LABELS.retry });
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-3"] });
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );
    expect(
      blockedBy().queryByRole("alert"),
      "the issue waits on the set read before, but Blocked by still says the titles of the other failed"
    ).toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "the blockers changed back elsewhere to a set whose read failed and then answered before the press, and the focus left the page's body though the read the press was for had not answered"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The reader presses the titles' Retry, and while its read is out another session changes the
// issues this one waits on to a set whose titles Details failed to read too: that read stands
// failed, so the line stays, with its Retry and the focus. The read the press sent then fails
// while the row draws the other set, and the cache drops it once nothing has observed it, and no
// fetch of it is out, for its `gcTime` (dropped here by hand). The session then puts back the set
// the press was for: the row reads those titles afresh, and the line goes with the focus on its
// Retry. The press's read ended under the other set, so the row forgot the press then, and no
// read answered it: the focus stays on the page's body rather than going to the row's control.
test("the titles' Retry whose read failed while its blockers changed elsewhere, and was dropped from the cache before they changed back, leaves its focus on the page's body", async () => {
  const details = titlesRetryDetails();

  try {
    const retry = await details.press();
    details.drawBlockers(["CORE-4"]);
    await details.blockedBy().findByRole("link", { name: "CORE-4" });
    expect(
      retry,
      "the issue waits on a set whose titles failed too, but the titles' Retry went"
    ).toBeInTheDocument();
    expect(document.activeElement, "the titles' Retry lost the focus as the set changed").toBe(
      retry
    );

    await act(async () => details.failPress());
    await waitFor(() =>
      expect(
        details.queryClient.getQueryState(details.pressedFor)?.fetchStatus,
        "the read the press sent did not end"
      ).toBe("idle")
    );
    act(() => details.queryClient.removeQueries({ exact: true, queryKey: details.pressedFor }));
    details.drawBlockers(["CORE-3"]);
    await waitFor(() =>
      expect(
        retry,
        "the set pressed for came back unread, but the titles' Retry is still drawn"
      ).not.toBeInTheDocument()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the set pressed for came back after its read failed under another, and the focus left the page's body though no read answered the press"
    ).toBe(document.body);
  } finally {
    details.restore();
  }
});

// The reader presses the titles' Retry, and while its read is out another session changes the
// issues this one waits on to a set whose titles Details failed to read too, whose read stands
// failed while it is out, so the line stays, with its Retry and the focus, and then puts back the
// set the press was for. Drawing another set stopped no read: the press's read is still out, so
// the line stands, and once that read answers the line goes for the press's answer, so the focus
// Retry held goes on to the row's control rather than to the page's body.
test("the titles' Retry whose blockers changed elsewhere and back while its read is out hands its focus to the row's control once that read answers", async () => {
  const details = titlesRetryDetails();

  try {
    const retry = await details.press();
    details.drawBlockers(["CORE-4"]);
    await details.blockedBy().findByRole("link", { name: "CORE-4" });
    details.drawBlockers(["CORE-3"]);
    await details.blockedBy().findByRole("link", { name: "CORE-3" });
    expect(
      retry,
      "the issue waits again on the set pressed for, whose read is still out, but the titles' Retry went"
    ).toBeInTheDocument();
    expect(
      document.activeElement,
      "the titles' Retry lost the focus as the set changed and back"
    ).toBe(retry);

    await act(async () => details.answerPress());
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );
    expect(retry, "the titles answered, but their Retry is still drawn").not.toBeInTheDocument();
    expect(
      document.activeElement,
      "the read the press sent answered after the set changed elsewhere and back, and the focus fell to the page rather than to the row's control"
    ).toBe(details.blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
  } finally {
    details.restore();
  }
});

// The same, but drawing the other set reads its titles again, since a read that holds no answer
// is always stale, and that read fails too before the session puts back the set the press was
// for. The line stays throughout, with its Retry and the focus, and says each read's failure as
// the row draws it: the other set's second, then the pressed set's first again, from its read
// before the press. Neither is the press's read failing, so once that read answers the line goes
// for the press's answer, and the focus Retry held goes on to the row's control rather than to
// the page's body.
test("the titles' Retry whose blockers changed elsewhere to a set whose read failed again, and back, while its read is out hands its focus to the row's control once that read answers", async () => {
  const details = titlesRetryDetails();

  try {
    const retry = await details.press();
    details.titles.mockRejectedValueOnce(readFailure());
    details.drawBlockers(["CORE-4"]);
    await waitFor(() =>
      expect(
        details.queryClient.getQueryState(details.otherSet)?.errorUpdateCount,
        "the other set's titles were not read again with a failure"
      ).toBe(2)
    );
    details.drawBlockers(["CORE-3"]);
    await details.blockedBy().findByRole("link", { name: "CORE-3" });
    expect(
      retry,
      "the issue waits again on the set pressed for, whose read is still out, but the titles' Retry went"
    ).toBeInTheDocument();
    expect(
      document.activeElement,
      "the titles' Retry lost the focus as the set changed and back"
    ).toBe(retry);

    await act(async () => details.answerPress());
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Fix the build")
    );
    expect(retry, "the titles answered, but their Retry is still drawn").not.toBeInTheDocument();
    expect(
      document.activeElement,
      "the read the press sent answered after the set changed to one whose read failed again and back, and the focus fell to the page rather than to the row's control"
    ).toBe(details.blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
  } finally {
    details.restore();
  }
});

// The reader presses the titles' Retry, and while its read is out another session changes the
// issues this one waits on to a set whose titles Details failed to read too. Drawing that set reads
// its titles again, and that read stands failed while it is out, so the line stays, with its Retry
// and the focus, and the reader presses Retry again, which joins that read. The session then puts
// back the set of the first press, whose read fails, so the line stays, and changes the set again
// to that of the second press, whose read is still out. The first press's read failing answers the
// first press alone: once the second press's read answers, the line goes for that answer, and the
// focus Retry held goes on to the row's control rather than to the page's body.
test("the titles' Retry pressed again under another set hands its focus to the row's control once that press's read answers, though the first press's read failed meanwhile", async () => {
  const details = titlesRetryDetails();
  const otherRead = Promise.withResolvers<ReferenceTargets>();

  try {
    const retry = await details.press();
    details.titles.mockImplementationOnce(() => otherRead.promise);
    details.drawBlockers(["CORE-4"]);
    await details.blockedBy().findByRole("link", { name: "CORE-4" });
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    expect(document.activeElement, "the titles' Retry lost the focus as the set changed").toBe(
      retry
    );
    await details.user.keyboard("{Enter}");

    details.drawBlockers(["CORE-3"]);
    await details.blockedBy().findByRole("link", { name: "CORE-3" });
    await act(async () => details.failPress());
    await waitFor(() =>
      expect(
        details.queryClient.getQueryState(details.pressedFor)?.errorUpdateCount,
        "the first press's read did not fail"
      ).toBe(2)
    );
    details.drawBlockers(["CORE-4"]);
    await details.blockedBy().findByRole("link", { name: "CORE-4" });
    expect(
      retry,
      "the issue waits again on the set of the second press, whose read is still out, but the titles' Retry went"
    ).toBeInTheDocument();
    expect(
      document.activeElement,
      "the titles' Retry lost the focus as the set changed and back"
    ).toBe(retry);

    await act(async () =>
      otherRead.resolve(issueNames([details.otherBlocker], { "CORE-4": "Ship the release" }))
    );
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocked_by)).toHaveTextContent("Ship the release")
    );
    expect(retry, "the titles answered, but their Retry is still drawn").not.toBeInTheDocument();
    expect(
      document.activeElement,
      "the read the second press joined answered after the first press's read failed, and the focus fell to the page rather than to the row's control"
    ).toBe(details.blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
  } finally {
    details.restore();
  }
});

// The reader presses the Retry of the titles of what waits on the issue, and while its read is
// out another session changes which issues wait on it: the row reads the titles of the new set,
// and the line goes with the focus on its Retry. The read the press sent has not answered, so the
// focus stays on the page's body rather than going to the first issue the row draws.
test("Blocks' titles Retry taken away by what waits on the issue changed elsewhere while its read is out leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(WAITER_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await readWhatWaits(queryClient, getReferences, OTHER_WAITER_REFERENCES);
    await blocksRow().findByRole("link", { name: "CORE-6" });
    await waitFor(() =>
      expect(
        blocksRow().queryByRole("alert"),
        "other issues wait on this one, but Blocks still says the titles of the old ones failed"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "what waits on the issue changed elsewhere while the titles' read was out, and the focus left the page's body though that read had not answered"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The reader presses the Retry of the titles of a set of waiting issues another session made, and
// while its read is out that session puts back the set before, whose titles Details read then: the
// row draws that cached answer, and the line goes with the focus on its Retry. That answer is no
// answer of the read the press sent, so the focus stays on the page's body rather than going to
// the first issue the row draws.
test("Blocks' titles Retry taken away by what waits on the issue changed back elsewhere to a set already read leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(WAITER_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets").mockImplementation(async (refs) =>
    issueNames(refs, { "CORE-5": "Ship the release" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release")
    );
    titles.mockRejectedValueOnce(readFailure());
    await readWhatWaits(queryClient, getReferences, OTHER_WAITER_REFERENCES);
    const retry = await blocksRow().findByRole("button", { name: CONTROL_LABELS.retry });
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await readWhatWaits(queryClient, getReferences, WAITER_REFERENCES);
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release")
    );
    expect(
      blocksRow().queryByRole("alert"),
      "the set read before waits on the issue again, but Blocks still says the titles of the other failed"
    ).toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "what waits on the issue changed back elsewhere while the titles' read was out, and the focus left the page's body though that read had not answered"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The titles of what waits on the issue failed their first read, and answered as an event read
// them again. Another session then changes which issues wait on it, whose titles fail, and the
// reader presses that line's Retry; while its read is out the session puts back the set before:
// the row draws its cached answer, and the line goes with the focus on its Retry. That read failed
// and then answered before the press, and is not the read the press was for, so the focus stays on
// the page's body rather than going to the first issue the row draws.
test("Blocks' titles Retry taken away by what waits on the issue changed back elsewhere to a set whose read failed and then answered leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(WAITER_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets")
    .mockImplementation(async (refs) => issueNames(refs, { "CORE-5": "Ship the release" }))
    .mockRejectedValueOnce(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    await waitFor(() =>
      expect(blocksRow().getByRole("alert")).toHaveTextContent(TITLES_READ_FAILED)
    );
    await act(() => queryClient.refetchQueries({ queryKey: ["reference-targets"] }));
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release")
    );
    titles.mockRejectedValueOnce(readFailure());
    await readWhatWaits(queryClient, getReferences, OTHER_WAITER_REFERENCES);
    const retry = await blocksRow().findByRole("button", { name: CONTROL_LABELS.retry });
    titles.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await readWhatWaits(queryClient, getReferences, WAITER_REFERENCES);
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent("Ship the release")
    );
    expect(
      blocksRow().queryByRole("alert"),
      "the set read before waits on the issue again, but Blocks still says the titles of the other failed"
    ).toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "what waits on the issue changed back elsewhere to a set whose read failed and then answered before the press, and the focus left the page's body though the read the press was for had not answered"
    ).toBe(document.body);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

/** How many entries each list of `MANY_ENTRIES` holds: two pages of 20 and five entries more. */
const MANY = 2 * PAGE + 5;

/** `count` names, `stem` and a number from 1, padded so that they sort as they are numbered. */
function numbered(stem: string, count: number): string[] {
  return Array.from(
    { length: count },
    (_, index) => `${stem}${String(index + 1).padStart(2, "0")}`
  );
}

/** The fixture's issue with `MANY` entries in each list of unbounded length its rows of Details
 *  draw: its pull requests, its other links, its components (25 live and the rest retired, which
 *  the row draws after them as one list), and its documents. */
const MANY_ENTRIES: IssueDetails = {
  ...withNotes(MANY - 1),
  components: {
    ids: numbered("panel-", 25),
    inherited_from: null,
    mode: "explicit",
    reason: null,
    unknown: numbered("retired-", MANY - 25),
  },
  external_links: [
    ...numbered("", MANY).map((number) => ({
      kind: "github_pr" as const,
      url: `https://github.com/acme/web/pull/${number}`,
    })),
    ...numbered("", MANY).map((number) => ({
      url: `https://github.com/acme/web/issues/${number}`,
    })),
  ],
};

/** `MANY` pins of the reader's: comments of CORE-1, and lines of its history after them, which
 *  Pinned lists as one list. */
const MANY_PINS = [
  ...numbered("00000000-0000-4000-8000-0000000000", PAGE + 5).map((id) =>
    conversationItemId("comment", id)
  ),
  ...Array.from({ length: MANY - PAGE - 5 }, (_, index) => eventItemId({ id: 900 + index })),
];

/** The reads of Details of `MANY_ENTRIES`, with the reader's `MANY_PINS` and `MANY` issues waiting
 *  on it, each link's read of GitHub answered, and the project's architecture read; the call that
 *  puts every read back. */
function stubManyEntries(): () => void {
  const { getIssue, getMyState, restore } = stubIssueApi();
  getIssue.mockResolvedValue(MANY_ENTRIES);
  getMyState.mockResolvedValue(pinnedState(...MANY_PINS));
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const stubs = [
    spyOn(api, "getReferences").mockResolvedValue({
      edges: keysFrom2(MANY).map((key) => waitingEdge(key, `Waits for ${key}`)),
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    }),
    // A reference with no head, so a pull request's read reads no checks after it.
    spyOn(api, "githubRest").mockImplementation(
      async () =>
        new Response(JSON.stringify({ state: "open", title: "Ship the web app" }), {
          headers: { "Content-Type": "application/json" },
        })
    ),
    spyOn(api, "getArchitecture").mockResolvedValue(architecture),
  ];
  return () => {
    for (const stub of stubs) stub.mockRestore();
    restore();
  };
}

/** The chips drawn of the box of chips with the test id `testId`: the components'. */
function drawnChips(testId: string): HTMLElement[] {
  return [...screen.getByTestId(testId).querySelectorAll<HTMLElement>(":scope > span")].filter(
    (chip) => !chip.hidden
  );
}

/** One list of Details that `MANY_ENTRIES` fills: the box it and its Show more are drawn in, and
 *  each entry it draws, in order. */
interface PagedList {
  readonly box: () => HTMLElement;
  readonly entries: () => readonly Element[];
}

/** Each link a row of Details draws, by the row's name. */
function linksOfRow(name: string): PagedList {
  return {
    box: () => detailsRow(name),
    entries: () => within(detailsRow(name)).getAllByRole("link"),
  };
}

/** Every list of unbounded length that Details' own rows and Pinned draw. */
const PAGED_LISTS: readonly (readonly [string, PagedList])[] = [
  ["Blocks", linksOfRow(PROPERTY_LABELS.blocks)],
  ["Pull request", linksOfRow(PROPERTY_LABELS.pull_request)],
  ["Links", linksOfRow(PROPERTY_LABELS.external_links)],
  [
    "Components, the retired ones after the live ones",
    {
      box: () => detailsRow(PROPERTY_LABELS.components),
      entries: () => drawnChips("issue-components"),
    },
  ],
  [
    "Files and documents",
    {
      box: () => filesRow(),
      entries: () => within(screen.getByTestId("issue-files")).getAllByRole("listitem"),
    },
  ],
  [
    "Pinned, the history's lines after the comments",
    {
      box: () => screen.getByRole("region", { name: CONTROL_LABELS.pinned }),
      entries: () =>
        within(screen.getByRole("region", { name: CONTROL_LABELS.pinned })).getAllByRole(
          "listitem"
        ),
    },
  ],
];

// Each list of unbounded length draws its first 20 entries and, under them, Show more, which says
// how many entries are left. A press keeps the entries drawn, draws the next 20 and moves the focus
// to the first of them, whether the control stays or goes, rather than let it fall to the page; the
// control goes once every entry is drawn. A list drawn from two sources - a component and a
// retired one, a pinned comment and a pinned line of the history - is one list, whose pages run
// on from one source into the other and whose count is of both.
test.each(
  PAGED_LISTS
)("%s: 20 entries at a time under Show more, which says how many are left, moves the focus to the first entry it drew and goes at the end", async (_name, list) => {
  const user = userEvent.setup();
  const restoreReads = stubManyEntries();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() =>
      expect(list.entries(), "the list does not draw its first 20 entries").toHaveLength(PAGE)
    );
    const more = within(list.box()).getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(MANY - PAGE));

    act(() => more.focus());
    await user.keyboard("{Enter}");
    expect(list.entries(), "Show more did not draw the next 20 entries").toHaveLength(2 * PAGE);
    expect(more.textContent).toBe(showMoreName(MANY - 2 * PAGE));
    expect(
      list.entries()[PAGE]?.contains(document.activeElement),
      "Show more did not move the focus to the first entry it drew"
    ).toBe(true);

    act(() => more.focus());
    await user.keyboard("{Enter}");
    expect(list.entries()).toHaveLength(MANY);
    expect(more.isConnected, "Show more stayed once every entry was drawn").toBe(false);
    expect(
      list.entries()[2 * PAGE]?.contains(document.activeElement),
      "Show more went with the last entries and did not move the focus to the first it drew"
    ).toBe(true);
  } finally {
    unmount();
    restoreReads();
  }
});

// Details draws its own control of Referenced by: the application's `ReferencedByToggle` is in
// its link blue, one sky-blue control among values in the page's ink.
test("Referenced by's control is in the page's ink, opens the list and reads the count", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    const toggle = await screen.findByRole("button", { name: referencedByControlName(undefined) });
    expect(toggle, "Referenced by's control is not in the page's ink").toHaveClass(pageInkText);
    for (const token of linkText.split(" ")) {
      expect(toggle, "Referenced by's control is in the application's link blue").not.toHaveClass(
        token
      );
    }
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await user.click(toggle);
    expect(toggle, "Referenced by's control does not say the list is open").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(toggle, "Referenced by's control names no list it opens").toHaveAttribute(
      "aria-controls",
      list.id
    );
    expect(
      await screen.findByRole("button", { name: referencedByControlName(0) }),
      "Referenced by's control does not read the rows of the list it opened"
    ).toBe(toggle);
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// A command bar run the reader asked is in no reader's `referenced_by_count`, so Details' control
// of Referenced by is a control at every count, and it reads the rows its list shows, the list's
// own and the reader's runs, only once both reads have answered: a tree read that stands failed
// adds no run to the count, and one still out holds the count back.
test("Referenced by counts the reader's runs once both reads answer, and is a control at zero", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const otherComment = "44444444-4444-4444-8444-444444444444";
  // CORE-2's comment cites CORE-1: a row of the list's own (`referenced-by-page.test.tsx`).
  const cited: GraphEdge = {
    created_at: "2026-11-16T10:00:00Z",
    direction: "in",
    excerpt: { text: "CORE-2's comment cites CORE-1." },
    kind: "mentions",
    node: graphNode({
      id: otherComment,
      issue_key: "CORE-2",
      kind: "comment",
      project: "CORE",
      ref: `dispatch://CORE-2/comment/${otherComment}`,
    }),
    source_seq: 7,
  };
  let listed = NOTHING_REFERENCES;
  // The list reads what references the issue with no kind; Blocks reads one kind of it.
  const getReferences = spyOn(api, "getReferences").mockImplementation(async (_to, options) =>
    options === undefined ? listed : NOTHING_REFERENCES
  );
  const listReads = () =>
    getReferences.mock.calls.filter(([to, options]) => to === reference && options === undefined)
      .length;
  const getIssueTree = spyOn(api, "getIssueTree").mockRejectedValue(readFailure());
  const runKind = sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction);
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 0 });
  let view = renderDetails(undefined, OPEN_DRAWER);

  try {
    // Nothing references the issue and the reader asked no run: a control all the same.
    const zero = await screen.findByRole("button", { name: referencedByControlName(undefined) });
    expect(zero).toHaveAttribute("aria-expanded", "false");
    await user.click(zero);
    expect(
      await screen.findByRole("button", { name: referencedByControlName(0) }),
      "Referenced by's control does not read the rows of the list it opened"
    ).toBe(zero);
    view.unmount();

    // The reader's run changed the issue, and CORE-2's comment cites it: the issue's count holds
    // the comment alone.
    listed = { ...NOTHING_REFERENCES, edges: [cited] };
    getReferences.mockClear();
    getIssue.mockResolvedValue({ ...issue, referenced_by_count: 1 });
    view = renderDetails(undefined, OPEN_DRAWER);
    const { queryClient } = view;
    act(() => {
      queryClient.setQueryData(issueTreeKey(issue.key), treeWithRun(RUN, RUN_QUESTION));
    });
    const control = await screen.findByRole("button", {
      name: referencedByControlName(undefined),
    });
    await waitFor(() =>
      expect(getReferences).toHaveBeenCalledWith(reference, { kind: "blocked_by" })
    );
    expect(listReads(), "Details read the list before the reader opened it").toBe(0);
    await user.click(control);
    expect(
      await screen.findByRole("button", { name: referencedByControlName(2) }),
      "Referenced by's control does not count the reader's run with the list's own row"
    ).toBe(control);
    const list = screen.getByRole("region", { name: "Referenced by" });
    const listedRow = within(list).getByRole("link", { name: "Comment · CORE-2" });
    const runRows = list.querySelector<HTMLElement>("[data-extra-references]");
    if (runRows === null) throw new Error("the list draws no row of the reader's run");
    expect(within(runRows).getByRole("link", { name: runKind })).toHaveAttribute(
      "href",
      `/runs/${RUN}`
    );
    expect(runRows, "the run's row does not hold its question").toHaveTextContent(RUN_QUESTION);
    // The list draws the rows its host adds after its own (`ReferencedBy`'s `extraRows`).
    expect(
      listedRow.compareDocumentPosition(runRows) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the run's row stands before the list's own"
    ).toBeTruthy();
    view.unmount();

    // The tree read with no view stands failed: the list keeps its own row, with the failure's
    // line after it, and the control counts that row alone.
    view = renderDetails(undefined, OPEN_DRAWER);
    const failing = view.queryClient;
    await act(async () => {
      await failing.resetQueries({ exact: true, queryKey: issueTreeKey(issue.key) });
      await failing.prefetchQuery(issueTreeQuery(issue.key));
    });
    const unread = await screen.findByRole("button", { name: referencedByControlName(undefined) });
    await user.click(unread);
    expect(
      await screen.findByRole("button", { name: referencedByControlName(1) }),
      "Referenced by's control does not read the list's own rows while the tree read stands failed"
    ).toBe(unread);
    const kept = within(screen.getByRole("region", { name: "Referenced by" })).getByRole("link", {
      name: "Comment · CORE-2",
    });
    const failure = screen.getByText(ANSWER_LINES.references_unreadable);
    expect(
      kept.compareDocumentPosition(failure) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the tree read's line stands before the list's own row"
    ).toBeTruthy();
    const line = failure.parentElement;
    if (line === null) throw new Error("the tree read's failure stands in no line");
    const treeReads = getIssueTree.mock.calls.length;
    await user.click(within(line).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() =>
      expect(getIssueTree, "Retry did not read the tree again").toHaveBeenCalledTimes(treeReads + 1)
    );
    view.unmount();

    // The tree read is out as the reader opens the list: the list's own read answers, and the
    // control holds its count back until the tree read answers too, with the reader's run in it.
    const treeRead = Promise.withResolvers<TreeAnswer>();
    getIssueTree.mockImplementation(() => treeRead.promise);
    view = renderDetails(undefined, OPEN_DRAWER);
    const reading = view.queryClient;
    await act(async () => {
      await reading.resetQueries({ exact: true, queryKey: issueTreeKey(issue.key) });
    });
    act(() => {
      void reading.prefetchQuery(issueTreeQuery(issue.key));
    });
    const holding = await screen.findByRole("button", { name: referencedByControlName(undefined) });
    await user.click(holding);
    const opened = await screen.findByRole("region", { name: "Referenced by" });
    await within(opened).findByRole("link", { name: "Comment · CORE-2" });
    expect(
      holding,
      "Referenced by counted the list's rows before the tree read answered"
    ).toHaveAccessibleName(referencedByControlName(undefined));
    await act(async () => treeRead.resolve(treeWithRun(RUN, RUN_QUESTION)));
    expect(
      await screen.findByRole("button", { name: referencedByControlName(2) }),
      "Referenced by does not count the reader's run once the tree read answers"
    ).toBe(holding);
  } finally {
    view.unmount();
    getIssueTree.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Details draws the list 20 rows at a time, the reader's runs after its own rows in the one
// sequence (`ReferencedBy`'s `pages`, `extraRows`): the control counts every row of the list, its
// own and the runs, and not the rows its first page draws, and Show more under them says how many
// of both are left.
test("N counts every row of the list and not the rows its pages draw", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  // 19 comments of CORE-2, each citing CORE-1: rows of the list's own.
  const cited = Array.from({ length: 19 }, (_, index): GraphEdge => {
    const id = `44444444-4444-4444-8444-${String(index + 1).padStart(12, "0")}`;
    return {
      created_at: "2026-11-16T10:00:00Z",
      direction: "in",
      excerpt: { text: `CORE-2's comment ${index + 1} cites CORE-1.` },
      kind: "mentions",
      node: graphNode({
        id,
        issue_key: "CORE-2",
        kind: "comment",
        project: "CORE",
        ref: `dispatch://CORE-2/comment/${id}`,
      }),
      source_seq: 7,
    };
  });
  // Three runs the reader asked, each of which changed the issue (`treeWithRun`).
  const runs = [1, 2, 3].map((n) => `6b1d3f5a-8c2e-4a7b-9d0f-00000000000${n}`);
  const tree = treeAnswer(
    runs.map((run, index) =>
      treeNode(run, "interaction", null, {
        made_at: `2026-11-17T0${index}:00:00Z`,
        role: "context",
        text: `Question ${index + 1}`,
        visibility: "author",
      })
    ),
    runs.map((run) => treeTuple("changed_by_run", "CORE-1", run))
  );
  // The list reads what references the issue with no kind; Blocks reads one kind of it.
  const getReferences = spyOn(api, "getReferences").mockImplementation(async (_to, options) =>
    options === undefined ? { ...NOTHING_REFERENCES, edges: cited } : NOTHING_REFERENCES
  );
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 19 });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER, detailsQueryClient(), tree);

  try {
    const control = await screen.findByRole("button", {
      name: referencedByControlName(undefined),
    });
    await user.click(control);
    expect(
      await screen.findByRole("button", { name: referencedByControlName(22) }),
      "Referenced by's control does not count every row of the list, its own and the runs"
    ).toBe(control);
    const list = screen.getByRole("region", { name: "Referenced by" });
    expect(
      list.querySelectorAll(":scope > ul > li > ul > li"),
      "the list does not draw the 20 rows of its first page"
    ).toHaveLength(20);
    expect(
      within(list).getAllByRole("link", {
        name: sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction),
      }),
      "the first page does not draw the newest run after the list's 19 own rows"
    ).toHaveLength(1);
    expect(
      within(list).getByRole("button", { name: SHOW_MORE }).textContent,
      "Show more does not count the rows of both left"
    ).toBe(showMoreName(2));
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// The list's runs come from the tree read with no view, which the page reads for Graph alone: the
// page's load sends none of it for runs, and the list's first opening sends it once where the
// page does not hold it, and none where it does. A read Dispatch refused is not sent again at the
// list's next opening, only by its Retry.
test("the list's first opening reads the tree with no view where the page does not hold it, and the load reads none", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const getIssueTree = spyOn(api, "getIssueTree").mockResolvedValue(treeWithRun(RUN, RUN_QUESTION));
  const treeReads = () => getIssueTree.mock.calls.length;
  const runKind = sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction);
  // The page holds no tree read: the load, with Details and References open and the list shut.
  let view = renderDetails(undefined, OPEN_DRAWER, detailsQueryClient(), null);
  let stopFreshness: (() => void) | undefined;

  try {
    const control = await screen.findByRole("button", {
      name: referencedByControlName(undefined),
    });
    await waitFor(() =>
      expect(getReferences).toHaveBeenCalledWith(reference, { kind: "blocked_by" })
    );
    expect(treeReads(), "the page's load read the tree for runs").toBe(0);
    await user.click(control);
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(await within(list).findByRole("link", { name: runKind })).toHaveAttribute(
      "href",
      `/runs/${RUN}`
    );
    expect(
      getIssueTree.mock.calls.map(([key, query]) => [key, query]),
      "the list's first opening did not read the tree once, with no view and no kind"
    ).toEqual([["CORE-1", {}]]);
    // Shut and opened again, the list draws the answer the page now holds.
    await user.click(control);
    await user.click(control);
    const reopened = await screen.findByRole("region", { name: "Referenced by" });
    await within(reopened).findByRole("link", { name: runKind });
    expect(treeReads(), "the list's second opening read the tree again").toBe(1);
    view.unmount();

    // The page holds the tree read, as it does once Graph has read it.
    getIssueTree.mockClear();
    view = renderDetails(
      undefined,
      OPEN_DRAWER,
      detailsQueryClient(),
      treeWithRun(RUN, RUN_QUESTION)
    );
    await user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    const held = await screen.findByRole("region", { name: "Referenced by" });
    expect(await within(held).findByRole("link", { name: runKind })).toHaveAttribute(
      "href",
      `/runs/${RUN}`
    );
    expect(treeReads(), "the list's opening read a tree the page holds").toBe(0);
    view.unmount();

    // The page holds no tree read, and Dispatch refuses it. The page's subscription to the tree's
    // freshness, which its stage holds (`ViewReadProvider`), marks a refused read valid, so only
    // a new ask of it - its Retry, an event of the issue - lets a new observer read it again.
    getIssueTree.mockClear();
    getIssueTree.mockRejectedValue(readFailure());
    const refusing = detailsQueryClient();
    const release = subscribeTreeFreshness(refusing, issue.key);
    // The subscription ends once released and once the client holds the tree no more.
    stopFreshness = () => {
      release();
      refusing.clear();
    };
    view = renderDetails(undefined, OPEN_DRAWER, refusing, null);
    const refused = await screen.findByRole("button", {
      name: referencedByControlName(undefined),
    });
    await user.click(refused);
    await screen.findByText(ANSWER_LINES.references_unreadable);
    expect(treeReads(), "the list's first opening did not read the tree once").toBe(1);
    await user.click(refused);
    await user.click(refused);
    const line = (await screen.findByText(ANSWER_LINES.references_unreadable)).parentElement;
    if (line === null) throw new Error("the tree read's failure stands in no line");
    expect(treeReads(), "the list's next opening read the refused tree again").toBe(1);
    await user.click(within(line).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(treeReads(), "Retry did not read the tree again").toBe(2));
  } finally {
    view.unmount();
    stopFreshness?.();
    getIssueTree.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Where the tree read stands failed, a press of its line's Retry reads the tree again, and once
// that read answers the line goes: that press is the reader's, so the focus goes on in the row,
// and since the list draws no link, to Referenced by's own control, which opens and closes the
// list, as Referenced by's own Retry hands it on.
test("the tree read's Retry hands the focus on when its line goes", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const getIssueTree = spyOn(api, "getIssueTree").mockRejectedValue(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await act(async () => {
      await queryClient.resetQueries({ exact: true, queryKey: issueTreeKey(issue.key) });
      await queryClient.prefetchQuery(issueTreeQuery(issue.key));
    });
    const control = await screen.findByRole("button", {
      name: referencedByControlName(undefined),
    });
    await user.click(control);
    const list = await screen.findByRole("region", { name: "Referenced by" });
    await within(list).findByText("Nothing references this yet.");
    const line = (await screen.findByText(ANSWER_LINES.references_unreadable)).parentElement;
    if (line === null) throw new Error("the tree read's failure stands in no line");
    const retry = within(line).getByRole("button", { name: CONTROL_LABELS.retry });

    const treeRead = Promise.withResolvers<TreeAnswer>();
    getIssueTree.mockImplementation(() => treeRead.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () => treeRead.resolve(EMPTY_TREE));
    await waitFor(() =>
      expect(retry, "the tree read answered, but its Retry is still drawn").not.toBeInTheDocument()
    );
    await waitFor(() => expect(control).toHaveFocus());
  } finally {
    stopBlurring();
    unmount();
    getIssueTree.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// The tree read's line stands while that read stands failed (`standsFailed`), as each line of
// Details' own does. A refresh that fails over the answer the read holds keeps the run's row and
// draws no line. A Retry of a read that stands failed keeps its line, busy and with the focus,
// while the read it sent is out, and the line stays, with the focus on Retry, once that read
// fails again.
test("a failed refresh over the tree read's answer draws no line, and a Retry of a read that stands failed keeps its line and its focus while it runs and fails again", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const getIssueTree = spyOn(api, "getIssueTree").mockRejectedValue(readFailure());
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const treeKey = issueTreeKey(issue.key);

  try {
    act(() => {
      queryClient.setQueryData(treeKey, treeWithRun(RUN, RUN_QUESTION));
    });
    await user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    const runRow = await within(list).findByRole("link", {
      name: sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction),
    });

    // A refresh of the tree read fails over the answer it holds.
    await act(async () => {
      await queryClient.prefetchQuery({ ...issueTreeQuery(issue.key), staleTime: 0 });
    });
    const refreshed = queryClient.getQueryState(treeKey);
    expect(
      [refreshed?.status, refreshed?.data !== undefined],
      "the tree read was not read again with a failure over the answer it holds"
    ).toEqual(["error", true]);
    expect(runRow, "a failed refresh took the run's row away").toBeInTheDocument();
    expect(
      screen.queryByText(ANSWER_LINES.references_unreadable),
      "Details says a failure over the tree read's answer"
    ).toBeNull();

    // The tree read holds no answer, and its read fails: it stands failed.
    await act(async () => {
      await queryClient.resetQueries({ exact: true, queryKey: treeKey });
      await queryClient.prefetchQuery(issueTreeQuery(issue.key));
    });
    const line = (await screen.findByText(ANSWER_LINES.references_unreadable)).parentElement;
    if (line === null) throw new Error("the tree read's failure stands in no line");
    const retry = within(line).getByRole("button", { name: CONTROL_LABELS.retry });

    const treeRead = Promise.withResolvers<TreeAnswer>();
    getIssueTree.mockImplementation(() => treeRead.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    expect(
      screen.getByText(ANSWER_LINES.references_unreadable).parentElement,
      "the tree read's line went while the read its Retry sent is out"
    ).toBe(line);
    expect(retry).toHaveFocus();

    await act(async () => treeRead.reject(readFailure()));
    await waitFor(() => expect(retry).not.toHaveAttribute("aria-busy"));
    expect(
      screen.getByText(ANSWER_LINES.references_unreadable).parentElement,
      "the tree read's line is not the one that stood before its read failed again"
    ).toBe(line);
    expect(retry).toHaveFocus();
  } finally {
    stopBlurring();
    unmount();
    getIssueTree.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// Inside Details, Referenced by's line stands while the list holds no answer, as each line of
// Details' own does, and its Retry keeps the focus while the read it sent is out: it reads
// `aria-disabled` there rather than turning disabled, which in Chromium drops the focus to the
// page's body, and a second press joins the read that is out.
test("in Details, Referenced by's Retry keeps the focus while the read it sent is out, and a second press sends none", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  // The list reads what references the issue with no kind, and its first read fails; Blocks reads
  // one kind of it.
  const getReferences = spyOn(api, "getReferences").mockImplementation((_to, options) =>
    options === undefined ? Promise.reject(readFailure()) : Promise.resolve(NOTHING_REFERENCES)
  );
  const listReads = () =>
    getReferences.mock.calls.filter(([to, options]) => to === reference && options === undefined)
      .length;
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    const retry = await within(list).findByRole("button", { name: "Retry" });

    getReferences.mockImplementationOnce(() => new Promise(() => undefined));
    const reads = listReads();
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    expect(listReads(), "Retry did not read the list again").toBe(reads + 1);
    await waitFor(() =>
      expect(
        retry,
        "Retry is not aria-disabled while the read it sent is out: it turned disabled, or still takes a press"
      ).toHaveAttribute("aria-disabled", "true")
    );
    expect(retry, "Retry turned disabled while the read it sent is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus while the read it sent is out").toBe(
      retry
    );
    await user.keyboard("{Enter}");
    expect(listReads(), "a press of Retry while its read is out sent another").toBe(reads + 1);
    expect(document.activeElement, "Retry lost the focus at a second press").toBe(retry);
  } finally {
    stopBlurring();
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Where the list's first read failed, the reader presses Retry by keyboard and its read is out: the
// list holds no answer, so its line stands, with Retry refusing a press as its read is out, and the
// line takes the place of the list's loading line, as each line of Details' own does while its
// Retry's read is out. Drawing both would say at once that the list failed and that it loads.
test("in Details, Referenced by's line stands without the list's loading line while its Retry's read is out", async () => {
  const details = referencedByDetails();

  try {
    const { list, retry } = await details.open();
    details.answerList(() => new Promise(() => undefined));
    act(() => retry.focus());
    await details.user.keyboard("{Enter}");
    expect(
      within(list).queryByText("Referenced by unavailable."),
      "the list holds no answer, but its failure line went as Retry's read went out"
    ).not.toBeNull();
    await waitFor(() =>
      expect(retry, "Retry takes a press while the read it sent is out").toHaveAttribute(
        "aria-disabled",
        "true"
      )
    );
    expect(
      within(list).queryByText("Loading references…"),
      "the list says it loads beside the line that says it failed"
    ).toBeNull();
  } finally {
    details.restore();
  }
});

// Where the list's first read failed, a press of Referenced by's Retry reads it again, and the line
// stays, with the focus the reader left on Retry, while that read is out. Once it answers with
// nothing to list the line goes: that press is the reader's, so the focus goes on in the row, and
// since the list draws no link, to Referenced by's own control, which opens and closes the list.
test("in Details, Referenced by's Retry, pressed after the list's first read failed, hands its focus to Referenced by's control once the list answers with nothing", async () => {
  const details = referencedByDetails();

  try {
    const { control, list, retry } = await details.open();
    const answer = Promise.withResolvers<GraphReferences>();
    details.answerList(() => answer.promise);
    act(() => retry.focus());
    await details.user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-disabled", "true"));
    expect(document.activeElement, "Retry lost the focus while the read it sent is out").toBe(
      retry
    );
    await act(async () => answer.resolve(NOTHING_REFERENCES));
    await within(list).findByText("Nothing references this yet.");
    expect(retry, "the list answered, but its Retry is still drawn").not.toBeInTheDocument();
    await settleFocus();
    expect(
      document.activeElement,
      "the read Retry sent answered with nothing to list, and the focus fell to the page's body rather than to Referenced by's control"
    ).toBe(control);
  } finally {
    details.restore();
  }
});

/**
 * Details with Referenced by's list, whose reads fail until `answerList` says otherwise; Blocks
 * reads nothing. `open` opens the list with Referenced by's control, which it returns, and the
 * list's first read fails, so the list draws its line and Retry over no list. `listKey` is the
 * key the list is read under, and `readListAgain` reads it again as an event about the issue's
 * references reads it. `restore` takes Details off the page and puts the spies back.
 */
function referencedByDetails() {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  const listKey = ["references", "to", buildDispatchReference({ key: "CORE-1", kind: "issue" })];
  let listAnswer = (): Promise<GraphReferences> => Promise.reject(readFailure());
  // The list reads what references the issue with no kind; Blocks reads one kind of it.
  const getReferences = spyOn(api, "getReferences").mockImplementation((_to, options) =>
    options === undefined ? listAnswer() : Promise.resolve(NOTHING_REFERENCES)
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const answerList = (answer: () => Promise<GraphReferences>) => {
    listAnswer = answer;
  };
  const readListAgain = () =>
    act(() => queryClient.refetchQueries({ exact: true, queryKey: listKey }));
  return {
    answerList,
    listKey,
    open: async () => {
      const control = await screen.findByRole("button", {
        name: referencedByControlName(undefined),
      });
      await user.click(control);
      const list = await screen.findByRole("region", { name: "Referenced by" });
      return { control, list, retry: await within(list).findByRole("button", { name: "Retry" }) };
    },
    queryClient,
    readListAgain,
    restore: () => {
      stopBlurring();
      unmount();
      getReferences.mockRestore();
      restore();
    },
    user,
  };
}

/**
 * Details of an issue that waits on CORE-4, whose blockers `drawBlockers` changes as another
 * session's change does; Blocks reads nothing. The first read of each set's titles fails, and every
 * read after those but the press's is out until the end: `press` waits for CORE-4's line, changes
 * the set to CORE-3, whose titles then fail too, and presses that line's Retry by keyboard, which
 * it returns once the read it sent is out. That read is out until `answerPress` answers it with
 * CORE-3's title or `failPress` fails it; `pressedFor` is its key. `otherBlocker` is CORE-4's
 * reference and `otherSet` the key of its titles' read; `titles` is the spy of the titles' reads,
 * and `user` the reader who pressed. `restore` takes Details off the page and puts the spies back.
 */
function titlesRetryDetails() {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-4"] });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const titles = spyOn(api, "getReferenceTargets")
    .mockImplementation(() => new Promise(() => undefined))
    .mockRejectedValueOnce(readFailure())
    .mockRejectedValueOnce(readFailure());
  const pressed = Promise.withResolvers<ReferenceTargets>();
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const pressedFor = referenceTargetsQuery([blocker]).queryKey;
  const otherBlocker = buildDispatchReference({ key: "CORE-4", kind: "issue" });
  const otherSet = referenceTargetsQuery([otherBlocker]).queryKey;
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));
  const drawBlockers = (keys: string[]) =>
    drawIssue(getIssue, queryClient, { ...issue, blocked_by: keys });
  return {
    answerPress: () => pressed.resolve(issueNames([blocker], { "CORE-3": "Fix the build" })),
    blockedBy,
    drawBlockers,
    failPress: () => pressed.reject(readFailure()),
    otherBlocker,
    otherSet,
    press: async () => {
      await waitFor(() =>
        expect(blockedBy().getByRole("alert")).toHaveTextContent(TITLES_READ_FAILED)
      );
      drawBlockers(["CORE-3"]);
      await waitFor(() => expect(queryClient.getQueryState(pressedFor)?.errorUpdateCount).toBe(1));
      const retry = await blockedBy().findByRole("button", { name: CONTROL_LABELS.retry });
      titles.mockImplementationOnce(() => pressed.promise);
      act(() => retry.focus());
      await user.keyboard("{Enter}");
      await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
      return retry;
    },
    pressedFor,
    queryClient,
    restore: () => {
      unmount();
      titles.mockRestore();
      getReferences.mockRestore();
      restore();
    },
    titles,
    user,
  };
}

// Where the list's first read failed, its line stands, and Retry keeps the focus while the read it
// sent is out. Once that read answers the line goes, with the focus on Retry; the reader's press
// took it away, so the focus goes on to the list's first link.
test("in Details, Referenced by's Retry hands its focus to the list's first link once the read it sent answers", async () => {
  const details = referencedByDetails();

  try {
    const { list, retry } = await details.open();
    const answer = Promise.withResolvers<GraphReferences>();
    details.answerList(() => answer.promise);
    act(() => retry.focus());
    await details.user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-disabled", "true"));
    expect(document.activeElement, "Retry lost the focus while the read it sent is out").toBe(
      retry
    );

    await act(async () => answer.resolve(WAITER_REFERENCES));
    await waitFor(() =>
      expect(retry, "the list answered, but its Retry is still drawn").not.toBeInTheDocument()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the read Retry sent answered, and the focus fell to the page's body rather than to the list's first link"
    ).toBe(within(list).getAllByRole("link")[0]);
  } finally {
    details.restore();
  }
});

// The line goes too once an event reads the list again and that read answers. That is no press
// of the reader's, so the focus they left on Retry stays on the page's body.
test("in Details, Referenced by's line, gone as an event's read of the list answers, leaves the focus its Retry held on the page's body", async () => {
  const details = referencedByDetails();

  try {
    const { list, retry } = await details.open();
    details.answerList(() => Promise.resolve(WAITER_REFERENCES));
    act(() => retry.focus());
    await details.readListAgain();
    await waitFor(() =>
      expect(retry, "the list answered, but its Retry is still drawn").not.toBeInTheDocument()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "an event's read of the list answered, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
    expect(
      within(list).getAllByRole("link")[0],
      "the list's first link took the focus"
    ).not.toHaveFocus();
  } finally {
    details.restore();
  }
});

// A press of Retry whose read fails again takes nothing away: the line stays, with the focus on
// Retry. Once an event then reads the list again and that read answers, the line goes, and that
// is no press of the reader's, so the focus stays on the page's body.
test("in Details, Referenced by's Retry whose read failed again hands nothing on once an event's read of the list answers", async () => {
  const details = referencedByDetails();

  try {
    const { list, retry } = await details.open();
    const answer = Promise.withResolvers<GraphReferences>();
    details.answerList(() => answer.promise);
    act(() => retry.focus());
    await details.user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-disabled", "true"));
    await act(async () => answer.reject(readFailure()));
    await waitFor(() =>
      expect(retry, "Retry still refuses a press after its read failed again").not.toHaveAttribute(
        "aria-disabled"
      )
    );
    expect(document.activeElement, "Retry lost the focus as the read it sent failed again").toBe(
      retry
    );

    details.answerList(() => Promise.resolve(WAITER_REFERENCES));
    await details.readListAgain();
    await waitFor(() =>
      expect(retry, "the list answered, but its Retry is still drawn").not.toBeInTheDocument()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "an event's read of the list answered after Retry's read failed, and the focus left the page's body"
    ).toBe(document.body);
    expect(
      within(list).getAllByRole("link")[0],
      "the list's first link took the focus"
    ).not.toHaveFocus();
  } finally {
    details.restore();
  }
});

// Inside Details the list says its read failed only while it holds no answer, as Details says each
// of its own reads failed: a read of the list that fails after it answered - on an event, or a
// window focus - keeps that answer drawn and draws no line over it, since nothing the list draws
// is missing.
test("in Details, a refresh that fails over the list Referenced by holds keeps the list and draws no line", async () => {
  const details = referencedByDetails();

  try {
    details.answerList(() => Promise.resolve(WAITER_REFERENCES));
    await details.user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    await within(list).findAllByRole("link");
    details.answerList(() => Promise.reject(readFailure()));
    await details.readListAgain();
    const read = details.queryClient.getQueryState(details.listKey);
    expect(
      [read?.status, read?.data !== undefined],
      "the list was not read again with a failure over the answer it holds"
    ).toEqual(["error", true]);
    expect(
      within(list).queryByText("Referenced by unavailable."),
      "Referenced by says a failure over the list it holds"
    ).toBeNull();
    expect(
      within(list).queryByRole("button", { name: "Retry" }),
      "Referenced by offers a Retry of a read that holds its answer"
    ).toBeNull();
    expect(list, "Referenced by took away the list a failed refresh left it").toHaveTextContent(
      "Ship the release"
    );
  } finally {
    details.restore();
  }
});

// Details draws Referenced by's control at every count, since a command bar run the reader asked
// is in no reader's `referenced_by_count`: it stays as the count Details draws falls to 0, as
// another session's change makes it, and so does the list the reader opened. A press of the
// list's Retry there takes its line away once the read it sent answers, here with nothing to
// list, and with no link in the row, the focus goes on to Referenced by's control rather than to
// the page's body.
test("in Details, Referenced by's Retry pressed at a count of 0 hands its focus to Referenced by's control, which stays", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  // The list reads what references the issue with no kind; Blocks reads one kind of it.
  const getReferences = spyOn(api, "getReferences").mockImplementation((_to, options) =>
    options === undefined ? Promise.reject(readFailure()) : Promise.resolve(NOTHING_REFERENCES)
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const control = await screen.findByRole("button", {
      name: referencedByControlName(undefined),
    });
    await user.click(control);
    const list = await screen.findByRole("region", { name: "Referenced by" });
    const retry = await within(list).findByRole("button", { name: "Retry" });
    drawIssue(getIssue, queryClient, { ...issue, referenced_by_count: 0 });
    expect(
      screen.queryByText(referencedByControlName(0)),
      "Details draws the count of 0 as a fact in place of Referenced by's control"
    ).toBeNull();
    expect(control, "Referenced by's control went at a count of 0").toBeInTheDocument();

    getReferences.mockResolvedValue(NOTHING_REFERENCES);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await within(list).findByText("Nothing references this yet.");
    expect(retry, "the list answered, but its Retry is still drawn").not.toBeInTheDocument();
    await waitFor(() => expect(control).toHaveFocus());
  } finally {
    stopBlurring();
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Outside the new issue page Referenced by is as on every other page: a read that fails after the
// list answered draws its line over that list, and Retry is disabled while the read it sent is
// out.
test("outside Details, Referenced by draws its line over the list it holds, and its Retry is disabled while the read it sent is out", async () => {
  const user = userEvent.setup();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(NOTHING_REFERENCES);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ReferencedBy reference={reference} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Nothing references this yet.");
    getReferences.mockRejectedValueOnce(readFailure());
    await act(() => queryClient.refetchQueries({ queryKey: ["references", "to", reference] }));
    const retry = await screen.findByRole("button", { name: "Retry" });
    expect(
      screen.queryByText("Nothing references this yet."),
      "outside Details, the line took away the list it is drawn over"
    ).toBeInTheDocument();
    getReferences.mockImplementationOnce(() => new Promise(() => undefined));
    await user.click(retry);
    await waitFor(() =>
      expect(retry, "Retry outside Details is not disabled while its read is out").toBeDisabled()
    );
    expect(retry, "Retry outside Details reads aria-disabled").not.toHaveAttribute("aria-disabled");
  } finally {
    view.unmount();
    queryClient.clear();
    getReferences.mockRestore();
  }
});

// A closed section keeps its rows on the page, hidden (`PropertySection`), as a closed Details
// keeps the panel (`slots/details.tsx`), so a Referenced by list opened before References or
// Details closed stayed drawn under it, its read live and read again on each event about the
// issue's references, for a list its reader could not see. The list is drawn only while Details
// and References are open, and is drawn again, still open, once the one closed opens.
test("closing References or Details takes an open Referenced by list, and its read, off the page", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const { queryClient, redraw, unmount } = renderDetails(undefined, OPEN_DRAWER);
  // What reads the list (`ReferencedBy`'s `referencesQuery`). The control's own watch of that
  // read's cache, which counts the rows it lists, is `enabled: false` and reads nothing.
  const listReaders = () =>
    (
      queryClient.getQueryCache().find({ exact: true, queryKey: ["references", "to", reference] })
        ?.observers ?? []
    ).filter((observer) => observer.options.enabled !== false).length;

  try {
    await user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(listReaders(), "the open Referenced by list does not read what references it").toBe(1);

    const references = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.references });
    await user.click(references);
    expect(references).toHaveAttribute("aria-expanded", "false");
    expect(list, "a closed References still draws the Referenced by list").not.toBeInTheDocument();
    expect(listReaders(), "a closed References still reads what references the issue").toBe(0);

    await user.click(references);
    expect(
      await screen.findByRole("region", { name: "Referenced by" }),
      "References opened again without the Referenced by list the reader left open"
    ).toBeInTheDocument();

    redraw(SHUT_DRAWER);
    expect(
      screen.queryByRole("region", { name: "Referenced by" }),
      "a closed Details still draws the Referenced by list"
    ).not.toBeInTheDocument();
    expect(listReaders(), "a closed Details still reads what references the issue").toBe(0);

    redraw(OPEN_DRAWER);
    expect(
      await screen.findByRole("region", { name: "Referenced by" }),
      "Details opened again without the Referenced by list the reader left open"
    ).toBeInTheDocument();
    expect(listReaders(), "the list drawn again does not read what references the issue").toBe(1);
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

// Details' reads of the titles of the issues this one waits on, of what waits on it and the
// titles of those, of what references it and of the projects, whose gated steps a top-level
// issue's approvals name and whose default repository the Repository row suggests, are each read
// again by the events that change them (`api/event-refresh.ts`), so in Details a window focus
// past the app's 30 s staleness reads none of them again. The reader's state keeps the app's
// defaults, so its second read shows the focus was handled.
test("in Details, a window focus past 30 s reads no blocker's title, Blocks, Blocks' titles, Referenced by or project again", async () => {
  const user = userEvent.setup();
  const { getIssue, getMyState, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"], referenced_by_count: 3 });
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const waiter = buildDispatchReference({ key: "CORE-12", kind: "issue" });
  const titles = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  // Blocks is answered with one issue that waits on this one, whose title Blocks then reads, and
  // the Referenced by list with nothing.
  const getReferences = spyOn(api, "getReferences").mockImplementation(async (_to, options) => ({
    edges: options?.kind === "blocked_by" ? [waitingEdge("CORE-12", "Wire the release")] : [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  }));
  // The Referenced by list's read, which names no kind of reference (`referencesQuery`).
  const listReads = () =>
    getReferences.mock.calls.filter(([to, options]) => to === reference && options === undefined)
      .length;
  const client = appClient();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER, client);

  try {
    await user.click(
      await screen.findByRole("button", { name: referencedByControlName(undefined) })
    );
    await waitFor(() => {
      expect(titleReads(titles, blocker)).toBe(1);
      expect(blocksReads(getReferences, reference)).toBe(1);
      expect(titleReads(titles, waiter)).toBe(1);
      expect(listReads()).toBe(1);
      expect(api.listProjects).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    await focusPastStaleness(client);
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(2));
    expect(
      titleReads(titles, blocker),
      "a window focus read the titles of the issues this one waits on again"
    ).toBe(1);
    expect(
      blocksReads(getReferences, reference),
      "a window focus read what waits on the issue again"
    ).toBe(1);
    expect(
      titleReads(titles, waiter),
      "a window focus read the titles of the issues that wait on this one again"
    ).toBe(1);
    expect(listReads(), "a window focus read what references the issue again").toBe(1);
    expect(api.listProjects, "a window focus read the projects again").toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
    restoreWindowFocus();
  }
});

// The menu lists the properties with no value and presses the control that sets each one,
// rather than drawing a second copy of it. It finds that control by the name exported for it
// (`blockedByControlName`), so the two sides cannot spell it apart: while each typed
// its own, a word changed in one made the entry disappear with no error anywhere.
test("the add menu opens the control the panel draws, under the name exported for it", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    expect(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) }),
      "the panel draws no blocked-by control under the name exported for it"
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.blocked_by }));
    expect(
      await screen.findByLabelText("Issues CORE-1 waits on"),
      "the menu entry reached no control, so picking it opened nothing"
    ).toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// The add menu is a menu a reader on the keyboard can use: it opens with the focus on its first
// entry, the arrow keys walk it, and Escape closes it with the focus back on Add and is taken
// there, so the phone's Details sheet, which closes on an Escape no control took, stays open.
test("the add menu opened by the keyboard takes the focus, walks by the arrows and gives it back on Escape", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    const add = await screen.findByRole("button", { name: CONTROL_LABELS.add });
    add.focus();
    await user.keyboard("{Enter}");
    const entries = await screen.findAllByRole("menuitem");
    expect(entries.length, "the add menu lists fewer than two entries to walk").toBeGreaterThan(1);
    await waitFor(() =>
      expect(document.activeElement, "the add menu opened with the focus left on Add").toBe(
        entries[0]
      )
    );
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement, "ArrowDown did not move to the next entry").toBe(entries[1]);
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement, "ArrowUp did not move back to the first entry").toBe(entries[0]);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu"), "Escape left the add menu open").not.toBeInTheDocument();
    expect(document.activeElement, "Escape did not give the focus back to Add").toBe(add);
    expect(
      escapes.taken,
      "Escape in the add menu went on to the page's keys, which close Details on a phone"
    ).not.toContain(false);

    // An entry chosen by the keyboard opens its control, as one chosen by the pointer does.
    await user.keyboard("{Enter}");
    await waitFor(() => expect(document.activeElement).toBe(screen.getAllByRole("menuitem")[0]));
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        screen.getByLabelText(PROPERTY_LABELS.parent),
        "the menu's Parent opened no parent's editor"
      ).toHaveFocus()
    );
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

/** No priority is live: the read the Advances row makes, answered empty. */
const noLivePriorities = {
  empty: "No priority is live now, so the list is empty.",
  priorities: [],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

/** One priority is live, which the picker of an issue that advances none offers. */
const oneLivePriority: Priorities = {
  empty: "",
  priorities: [
    {
      description: "",
      id: "evals-now",
      made_at: "2026-11-01T00:00:00Z",
      made_by: { id: "alice", kind: "user" },
      owner: "CORE controller",
      rank: "evals-now",
      state: "live",
      title: "Evals collecting data now",
      work: {
        by_status: {},
        claimed: 0,
        closed_last_7_days: 0,
        newest_event_at: null,
        open_asks: 0,
      },
    },
  ],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

// The add control lists what has no value yet, so where nothing can be added - a closed issue,
// or one with a value for every property the menu lists - it is not drawn at all.
test("the add control is not drawn on a closed issue, nor when every property has a value", async () => {
  const { getIssue, restore } = stubIssueApi();
  // The Advances row reads the priorities, to name the one the issue advances.
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(closedIssue);
  let view = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.add }),
      "the add control is drawn on a closed issue"
    ).not.toBeInTheDocument();
    view.unmount();

    // A value for each of the eight properties the menu lists: the components are the parent's.
    getIssue.mockResolvedValue({
      ...issue,
      advances: [{ id: "PRI-7", inherited: false }],
      assignee: "alice",
      blocked_by: ["CORE-3"],
      components: {
        mode: "inherit",
        ids: ["api"],
        unknown: [],
        reason: null,
        inherited_from: "CORE-2",
      },
      labels: ["bug"],
      parent: "CORE-2",
      repo: "acme/widgets",
      resolved_repo: "acme/widgets",
      route: "role:legion-controller-core",
    });
    view = renderDetails();
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.add }),
      "the add control is drawn on an issue with a value for every property it lists"
    ).not.toBeInTheDocument();
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** Opens Add's menu, then draws `away`, the issue as another session changed it, on which Add is
 *  not drawn, and `back`, the issue as it was (`drawIssue`): fails unless the menu opened, listing
 *  `entries` where they are given, `away` took Add off the page (`change` names what did), and
 *  Add came back with its menu closed. */
async function expectAddMenuShut(
  user: UserEvent,
  getIssue: Mock<typeof api.getIssue>,
  queryClient: QueryClient,
  {
    away,
    back,
    change,
    entries,
  }: {
    readonly away: IssueDetails;
    readonly back: IssueDetails;
    readonly change: string;
    readonly entries?: readonly string[];
  }
): Promise<void> {
  await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
  const menu = await screen.findByRole("menu", { name: CONTROL_LABELS.add });
  expect(menu).toBeInTheDocument();
  if (entries !== undefined) {
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((entry) => entry.textContent),
      `Add lists other than ${entries.join(", ")}`
    ).toEqual([...entries]);
  }
  drawIssue(getIssue, queryClient, away);
  await waitFor(() => expect(addMenuControl(), `${change} left Add on the page`).toBeNull());
  drawIssue(getIssue, queryClient, back);
  const add = await screen.findByRole("button", { name: CONTROL_LABELS.add });
  expect(add, "Add came back with its menu open").toHaveAttribute("aria-expanded", "false");
  expect(screen.queryByRole("menu"), "Add came back with its menu drawn").not.toBeInTheDocument();
}

// Add's menu shuts as another session closes the issue, though the reader pressed nothing: left
// open, it would come back drawn open when the issue reopens, with the focus not in it.
test("a close made elsewhere shuts Add's open menu, so a reopen draws Add with its menu closed", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();

  try {
    await expectAddMenuShut(user, getIssue, queryClient, {
      away: closedIssue,
      back: issue,
      change: "the close",
    });
  } finally {
    unmount();
    restore();
  }
});

/** An issue with a value for every property Add lists but its parent, so Parent is the one entry
 *  Add lists. */
const orphan: IssueDetails = {
  ...issue,
  advances: [{ id: "PRI-7", inherited: false }],
  assignee: "alice",
  blocked_by: ["CORE-3"],
  components: {
    mode: "inherit",
    ids: ["api"],
    unknown: [],
    reason: null,
    inherited_from: "CORE-2",
  },
  labels: ["bug"],
  parent: null,
  repo: "acme/widgets",
  resolved_repo: "acme/widgets",
  route: "role:legion-controller-core",
};

// Add draws nothing once the issue has a value for every property it lists, and its menu shuts
// then too: on an issue whose parent is its one empty property, another session sets a parent and
// then takes it away again. Left open, the menu would come back drawn open, with the focus not in
// it, though the reader pressed nothing.
test("a value set elsewhere for Add's last entry shuts its open menu, so Add comes back with its menu closed", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  // The Advances row reads the priorities, to name the one the issue advances.
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(orphan);
  const { queryClient, unmount } = renderDetails();

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    await expectAddMenuShut(user, getIssue, queryClient, {
      away: { ...orphan, parent: "CORE-2" },
      back: orphan,
      change: "the parent",
      entries: [PROPERTY_LABELS.parent],
    });
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// Add is drawn while a property it lists has no value and its control is on the page. An issue
// with a value for every other property loses its last label; while the save is out the labels'
// control is named "Saving labels of CORE-1", and in the render where the save landed Add looked
// for "Add a label to CORE-1", which that render's commit had not drawn yet, so it drew nothing
// and nothing drew it again.
test("removing the last label draws Add, whose Labels the row's control now answers", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  const full: IssueDetails = {
    ...issue,
    advances: [{ id: "PRI-7", inherited: false }],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    route: "role:legion-controller-core",
  };
  getIssue.mockResolvedValue(full);
  patchIssue.mockImplementation(async (_key, update) =>
    narrowIssue({ ...full, labels: [...(update.labels ?? full.labels ?? [])] })
  );
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: CONTROL_LABELS.add })).not.toBeInTheDocument();
    await user.click(
      screen.getByRole("button", {
        name: labelsControlName("CORE-1", {
          labels: ["bug"],
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      })
    );
    await user.click(await screen.findByRole("option", { name: "bug" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(
      await screen.findByRole("button", {
        name: labelsControlName("CORE-1", {
          labels: [],
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      }),
      "the label was not removed"
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.add }),
        "Labels has no value and its control is drawn, but Details draws no Add"
      ).toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      await screen.findByRole("menuitem", { name: PROPERTY_LABELS.labels }),
      "the add menu lists no Labels for the control the row draws"
    ).toBeInTheDocument();
  } finally {
    view.unmount();
    listIssues.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

// On an issue with a value for every property but the priorities it advances, Add lists Advances
// alone. Picking it presses "Add a priority to CORE-1", which opens the priorities' picker with
// the focus in its search box; Escape closes the picker, gives the focus back to that control and
// writes nothing.
test("Advances opens the priorities' picker with the focus in its search box, and Escape gives the focus back and writes nothing", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue({
    ...issue,
    advances: [],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    route: "role:legion-controller-core",
  });
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.advances }));
    expect(
      await screen.findByRole("combobox", { name: CONTROL_LABELS.priority_to_advance }),
      "the menu's Advances opened no priorities' picker"
    ).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(
      await screen.findByRole("button", { name: addPriorityControlName("CORE-1") }),
      "the priority's add control did not take the focus back as the picker closed"
    ).toHaveFocus();
    expect(patchIssue, "a picker closed with nothing ticked wrote").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** "Evals collecting data now" in the priorities' picker, once the picker is open and the read of
 *  the priorities has answered. The picker's list is portalled to the page's body, and is found
 *  there by its name, which no other control carries. */
async function evalsOption(): Promise<HTMLElement> {
  const listbox = await screen.findByRole("listbox", { name: "Priorities options" });
  return within(listbox).findByRole("option", { name: /Evals collecting data now$/ });
}

/**
 * Ticks or unticks "Evals collecting data now" in the priorities' picker whose trigger is
 * `trigger`, by the keyboard: Enter on the trigger opens the picker with the focus in its search
 * box, Enter there toggles its first row, the one priority the case offers, once the read of the
 * priorities has answered, and Escape closes it, which gives the focus back to the trigger and
 * sends the line's one write of the set it holds then (`IssuePriorityLine`).
 */
async function toggleEvals(user: UserEvent, trigger: HTMLElement): Promise<void> {
  act(() => trigger.focus());
  await user.keyboard("{Enter}");
  await evalsOption();
  await user.keyboard("{Enter}{Escape}");
}

/** The Advances row's picker trigger, "Edit priorities" (`priorityTriggerName`); throws while
 *  Details draws no such row, so `waitFor` waits for it. */
function advancesRowTrigger(): HTMLElement {
  return within(detailsRow(PROPERTY_LABELS.advances)).getByRole("button", {
    name: priorityTriggerName("CORE-1", false),
  });
}

/** Add's own control, found whether or not it is drawn on the screen, and never another button
 *  named Add: only Add's control opens a menu. */
function addMenuControl(): Element | null {
  return document.querySelector(
    `[aria-haspopup="menu"][aria-label="${CSS.escape(CONTROL_LABELS.add)}"]`
  );
}

// A press on an entry of Add opens that property's control. While the priorities' save is out,
// "Add a priority to CORE-1" takes no press, so a press on Add's Advances opened nothing: with
// the save held out, Add listed Advances, and pressing it left the menu closed, the picker shut
// and the focus on the body. Add lists an entry only while its control can be pressed, and lists
// it again once a refused save gives that control back.
test("Add lists no entry whose control is disabled, and lists it again once it is enabled", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue({
    ...issue,
    advances: [],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    route: "role:legion-controller-core",
  });
  const save = Promise.withResolvers<never>();
  patchIssue.mockImplementationOnce(() => save.promise);
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.advances }));
    // The menu's Advances opened the picker: the priority ticked, and the picker closed, which
    // sends the write the case holds out.
    await user.click(await evalsOption());
    await user.keyboard("{Escape}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    // Refused while its write is out, by the rule Add reads (`usableControl`): `disabled`, or
    // `aria-disabled` where the shared control stays focusable while it is busy.
    await waitFor(() =>
      expect(usableControl(control), "the control still takes a press while its write is out").toBe(
        false
      )
    );
    expect(
      addMenuControl(),
      "Add lists Advances while its control is disabled, so a press on it opens nothing"
    ).toBeNull();
    await act(async () =>
      save.reject(new ApiError(409, { code: "REVISION_CONFLICT", error: "the revision moved" }))
    );
    await waitFor(() =>
      expect(
        usableControl(screen.getByRole("button", { name: addPriorityControlName("CORE-1") })),
        "the control still takes no press"
      ).toBe(true)
    );
    await waitFor(() =>
      expect(
        addMenuControl(),
        "Advances has no value and its control is enabled again, but Details draws no Add"
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** The issue of the case above: a value for every property but Advances, so Add's one entry is
 *  Advances, and Add is drawn exactly while "Add a priority to CORE-1" takes a press. */
const advancesOnly: IssueDetails = {
  ...issue,
  advances: [],
  assignee: "alice",
  blocked_by: ["CORE-3"],
  components: {
    mode: "inherit",
    ids: ["api"],
    unknown: [],
    reason: null,
    inherited_from: "CORE-2",
  },
  labels: ["bug"],
  parent: "CORE-2",
  repo: "acme/widgets",
  resolved_repo: "acme/widgets",
  route: "role:legion-controller-core",
};

/** Sets `name` on `element`, and returns what takes it off again. */
function setAttributeUntil(element: Element, name: string, value: string): () => void {
  element.setAttribute(name, value);
  return () => element.removeAttribute(name);
}

/** Each way a control Add reaches stops taking a press (`usableControl`), or stops being found by
 *  the name Add looks for, set on "Add a priority to CORE-1" or on the element around it, with
 *  what undoes it. */
const refusals = [
  ["disabled", (control: HTMLElement) => setAttributeUntil(control, "disabled", "")],
  [
    'aria-disabled="true"',
    (control: HTMLElement) => setAttributeUntil(control, "aria-disabled", "true"),
  ],
  [
    "under an element that is hidden",
    (_control: HTMLElement, around: HTMLElement) => setAttributeUntil(around, "hidden", ""),
  ],
  [
    "under an element that is inert",
    (_control: HTMLElement, around: HTMLElement) => setAttributeUntil(around, "inert", ""),
  ],
  [
    'under an element that is aria-hidden="true"',
    (_control: HTMLElement, around: HTMLElement) =>
      setAttributeUntil(around, "aria-hidden", "true"),
  ],
  [
    "under an element that is not drawn",
    (_control: HTMLElement, around: HTMLElement) => {
      around.style.display = "none";
      return () => around.style.removeProperty("display");
    },
  ],
  [
    "under an element of visibility: hidden",
    (_control: HTMLElement, around: HTMLElement) => {
      around.style.visibility = "hidden";
      return () => around.style.removeProperty("visibility");
    },
  ],
  [
    "under an element whose class the stylesheet draws as nothing",
    (_control: HTMLElement, around: HTMLElement) => {
      const sheet = document.createElement("style");
      sheet.textContent = ".drawn-as-nothing { display: none; }";
      document.head.append(sheet);
      around.classList.add("drawn-as-nothing");
      return () => {
        around.classList.remove("drawn-as-nothing");
        sheet.remove();
      };
    },
  ],
  [
    // Another component's own commit renames its control in place, which draws Add no further
    // time: Add finds the control again only by watching its name.
    "renamed in place",
    (control: HTMLElement) => {
      const name = control.getAttribute("aria-label") ?? "";
      control.setAttribute("aria-label", `${name}, renamed`);
      return () => control.setAttribute("aria-label", name);
    },
  ],
  [
    "removed from the page",
    (control: HTMLElement) => {
      const parent = control.parentNode;
      const next = control.nextSibling;
      control.remove();
      return () => {
        parent?.insertBefore(control, next);
      };
    },
  ],
] as const;

// Add lists an entry only while its control takes a press (`usableControl`), whatever stops it
// taking one - on the control or on an element around it - and lists it again once that is
// undone, in a change no component of Add's draws. Each row is one rule of `usableControl`, or
// the name Add finds the control by, and each is a change Add's watch must see.
test.each(
  refusals
)("Add lists no entry whose control is %s, and lists it again once it is not", async (_name, refuse) => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(advancesOnly);
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Advances"
    ).toEqual([PROPERTY_LABELS.advances]);
    await user.keyboard("{Escape}");
    const control = screen.getByRole("button", { name: addPriorityControlName("CORE-1") });
    let undo = () => {};
    await act(async () => {
      undo = refuse(control, screen.getByTestId("issue-advances"));
    });
    await waitFor(() =>
      expect(addMenuControl(), "Add lists Advances while its control takes no press").toBeNull()
    );
    await act(async () => undo());
    await waitFor(() =>
      expect(
        addMenuControl(),
        "Advances' control takes a press again, but Details draws no Add"
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// Details draws the priority's line in one of two places: beside Add while the issue advances
// none, and in the Advances row once it advances one. The first priority added moves it, and the
// focus its add control held as the add landed - the picker gives it back to its trigger as it
// closes - went with the place the line left, to the page's body. It goes on in the Advances row:
// to the line's picker trigger there, "Edit priorities", whose next press adds another or takes
// one off; or, where the read after the add also draws the issue closed, as another session
// closed it while the add was out, which disables that trigger, to the link of the priority the
// reader added. Each row is the issue read back after the add, whether that read disables the
// trigger, and the element the focus goes to in the Advances row.
const FIRST_ADDED: readonly (readonly [
  string,
  IssueDetails,
  boolean,
  (row: HTMLElement) => HTMLElement,
])[] = [
  [
    "the Advances row's picker trigger",
    { ...advancesOnly, advances: [{ id: "evals-now", inherited: false }] },
    false,
    (row) => within(row).getByRole("button", { name: priorityTriggerName("CORE-1", false) }),
  ],
  [
    "the link of the priority added, on an issue closed meanwhile",
    {
      ...advancesOnly,
      advances: [{ id: "evals-now", inherited: false }],
      closed_at: "2026-09-10T00:00:00Z",
      status: "done",
    },
    true,
    (row) => within(row).getByRole("link", { name: /Evals collecting data now$/ }),
  ],
];

test.each(
  FIRST_ADDED
)("the first priority added hands the focus from the add control beside Add to %s", async (name, readBack, disabled, focused) => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  const view = renderDetails();
  try {
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    // The issue's read after the write is held, so the write lands, and the picker has given the
    // focus back to its trigger beside Add, before the line moves.
    const reread = Promise.withResolvers<IssueDetails>();
    getIssue.mockImplementation(() => reread.promise);
    await toggleEvals(user, control);
    await waitFor(() =>
      expect(
        document.activeElement,
        "the add landed, and the line gave the focus back to no add control beside Add"
      ).toBe(screen.queryByRole("button", { name: addPriorityControlName("CORE-1") }))
    );
    await act(async () => reread.resolve(readBack));
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    expect(
      within(row).getByRole("button", { name: priorityTriggerName("CORE-1", false) }),
      disabled
        ? "the Advances row's picker trigger is not disabled on a closed issue"
        : "the Advances row's picker trigger is disabled on an open issue"
    ).toHaveProperty("disabled", disabled);
    expect(
      document.activeElement,
      `the line moved into the Advances row, and the focus fell to the page rather than to ${name}`
    ).toBe(focused(row));
    expect(patchIssue, "other than one write went out").toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// The last priority removed moves the line back beside Add, and the focus its picker's trigger
// held went with the Advances row to the page's body. It goes on to the add control beside Add,
// the one control the line draws there; or, where the read after the removal also draws the issue
// closed, as another session closed it while the removal was out, which disables that control, to
// Development's heading, the button that opens and closes the section. The trigger holds the focus
// until the line moves: the picker gives it back as it closes, and while its write is out the
// trigger is `aria-disabled`, never `disabled`. Each row is the issue read back after the
// removal, whether that read disables the add control, and the element the focus goes to.
const LAST_REMOVED: readonly (readonly [string, IssueDetails, boolean, () => HTMLElement])[] = [
  [
    "the add control beside Add",
    advancesOnly,
    false,
    () => screen.getByRole("button", { name: addPriorityControlName("CORE-1") }),
  ],
  [
    "Development's heading, on an issue closed meanwhile",
    { ...advancesOnly, closed_at: "2026-09-10T00:00:00Z", status: "done" },
    true,
    () => screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }),
  ],
];

test.each(
  LAST_REMOVED
)("removing the last priority hands the focus from its picker's trigger to %s", async (name, readBack, disabled, focused) => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue({
    ...advancesOnly,
    advances: [{ id: "evals-now", inherited: false }],
  });
  const view = renderDetails();
  try {
    const trigger = await waitFor(advancesRowTrigger);
    // The issue's read after the write is held, so the removal lands before the line moves.
    const reread = Promise.withResolvers<IssueDetails>();
    getIssue.mockImplementation(() => reread.promise);
    const reads = getIssue.mock.calls.length;
    await toggleEvals(user, trigger);
    await waitFor(() =>
      expect(
        getIssue.mock.calls.length,
        "the removal landed, and the issue was not read again"
      ).toBeGreaterThan(reads)
    );
    // The shared trigger stays refused, and keeps the focus, until the new read is drawn.
    expect(trigger, "the trigger took a press again before the new read was drawn").toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(document.activeElement, "the trigger lost the focus before the line moved").toBe(
      trigger
    );
    await act(async () => reread.resolve(readBack));
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    expect(
      screen.getByRole("button", { name: addPriorityControlName("CORE-1") }),
      disabled
        ? "the add control beside Add is not disabled on a closed issue"
        : "the add control beside Add is disabled on an open issue"
    ).toHaveProperty("disabled", disabled);
    expect(
      document.activeElement,
      `the line moved back beside Add, and the focus fell to the page rather than to ${name}`
    ).toBe(focused());
    expect(patchIssue, "other than one write went out").toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** The fixture's issue advancing the priority "Evals collecting data now", with its own set at
 *  `revision`. */
function advancingEvals(revision: number): IssueDetails {
  return {
    ...advancesOnly,
    advances: [{ id: "evals-now", inherited: false }],
    advances_revision: revision,
  };
}

// The reader's removal of the last priority, and the read of the issue it sets off as it settles,
// can both end while Details still draws the issue before it, so the Advances row leaves only
// later, with the picker's trigger no longer `aria-disabled`. The move is still the reader's own:
// the issue now drawn holds the set their write sent, at the revision Dispatch answered that write
// with, so the focus the trigger held goes on to the add control beside Add rather than to the
// page's body.
test("the last priority removed hands the focus from its picker's trigger to the add control beside Add when the row leaves after the write settled", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  // The read the write sets off answers the issue before it, as a read Dispatch answered before
  // it had the write does.
  getIssue.mockResolvedValue(advancingEvals(4));
  const removed: IssueDetails = { ...advancesOnly, advances_revision: 5 };
  patchIssue.mockImplementationOnce(async (_key, input) => patchAnswer(removed, input));
  const { queryClient, unmount } = renderDetails();
  try {
    const trigger = await waitFor(advancesRowTrigger);
    await toggleEvals(user, trigger);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        trigger,
        "the trigger still refuses a press after its write settled"
      ).not.toHaveAttribute("aria-disabled")
    );
    expect(document.activeElement, "the trigger lost the focus before the row left").toBe(trigger);

    drawIssue(getIssue, queryClient, removed);
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the reader's own removal moved the line after its write settled, and the focus fell to the page rather than to the add control beside Add"
    ).toBe(screen.getByRole("button", { name: addPriorityControlName("CORE-1") }));
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// The reader's removal of the last priority lands with no answer to give (Dispatch answers its key
// `KEY_COMMITTED`), and the write and the read of the issue it sets off as it settles both end
// while Details still draws the issue before it, so the Advances row leaves only later. With no
// answer there is no revision to match, so the set the write sent, which the issue now drawn
// holds, tells the move is the reader's own: the focus the picker's trigger held goes on to the
// add control beside Add rather than to the page's body.
test("the last priority removed with no answer hands the focus from its picker's trigger to the add control beside Add when the row leaves after the write settled", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancingEvals(4));
  patchIssue.mockRejectedValueOnce(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const { queryClient, unmount } = renderDetails();
  try {
    const trigger = await waitFor(advancesRowTrigger);
    await toggleEvals(user, trigger);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        trigger,
        "the trigger still refuses a press after its write settled"
      ).not.toHaveAttribute("aria-disabled")
    );
    const write = queryClient.getMutationCache().getAll().at(-1)?.state;
    expect(write?.status, "the removal answered KEY_COMMITTED did not land").toBe("success");
    expect(write?.data, "the removal answered KEY_COMMITTED landed with an answer").toBeNull();
    expect(document.activeElement, "the trigger lost the focus before the row left").toBe(trigger);

    drawIssue(getIssue, queryClient, { ...advancesOnly, advances_revision: 5 });
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the reader's own removal, landed with no answer, moved the line after its write settled, and the focus fell to the page rather than to the add control beside Add"
    ).toBe(screen.getByRole("button", { name: addPriorityControlName("CORE-1") }));
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// After the reader's removal of the last priority has landed, other sessions add a priority and
// take it away again, with the focus on the Advances row's picker trigger: the row leaves, and the
// issue holds the set the reader's write sent, but at a newer revision than Dispatch answered that
// write with. That move is another session's, so the focus stays on the page's body.
test("the last priority taken away elsewhere after the reader's own removal landed leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancingEvals(4));
  const removed: IssueDetails = { ...advancesOnly, advances_revision: 5 };
  patchIssue.mockImplementationOnce(async (_key, input) => patchAnswer(removed, input));
  const { queryClient, unmount } = renderDetails();
  try {
    const trigger = await waitFor(advancesRowTrigger);
    getIssue.mockResolvedValue(removed);
    await toggleEvals(user, trigger);
    await waitFor(() =>
      expect(
        document.activeElement,
        "the reader's own removal moved the line, and the focus did not go to the add control beside Add"
      ).toBe(screen.queryByRole("button", { name: addPriorityControlName("CORE-1") }))
    );

    drawIssue(getIssue, queryClient, advancingEvals(6));
    const again = await waitFor(advancesRowTrigger);
    act(() => again.focus());
    drawIssue(getIssue, queryClient, { ...advancesOnly, advances_revision: 7 });
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "another session took the last priority away after the reader's removal landed, and the focus left the page's body"
    ).toBe(document.body);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// The reader's removal of the last priority has landed and moved the line beside Add, with the
// focus on its add control. An issue above this one then gains a priority, which the issue draws as
// inherited, with its own set and that set's revision as the reader's write left them: the line
// moves into the Advances row. No press of the reader's moved it, so the focus stays on the page's
// body rather than going to the row's picker trigger.
test("the line moved into the Advances row by a priority an issue above gains after the reader's own removal landed leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancingEvals(4));
  const removed: IssueDetails = { ...advancesOnly, advances_revision: 5 };
  patchIssue.mockImplementationOnce(async (_key, input) => patchAnswer(removed, input));
  const { queryClient, unmount } = renderDetails();
  try {
    const trigger = await waitFor(advancesRowTrigger);
    getIssue.mockResolvedValue(removed);
    await toggleEvals(user, trigger);
    const control = await waitFor(() => {
      const beside = screen.getByRole("button", { name: addPriorityControlName("CORE-1") });
      expect(
        document.activeElement,
        "the reader's own removal moved the line, and the focus did not go to the add control beside Add"
      ).toBe(beside);
      return beside;
    });
    await waitFor(() =>
      expect(
        control,
        "the add control beside Add still refuses a press after the removal settled"
      ).not.toHaveAttribute("aria-disabled")
    );

    drawIssue(getIssue, queryClient, {
      ...removed,
      advances: [{ id: "evals-now", inherited: true }],
    });
    await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    await settleFocus();
    expect(
      document.activeElement,
      "an issue above gained a priority after the reader's removal landed, and the focus left the page's body though no press of theirs moved the line"
    ).toBe(document.body);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// The reader adds a priority to another issue, on that issue's page, and the write lands with that
// issue's answer. On this issue's page the focus is then on the add control beside Add, and another
// session gives this issue the set that write sent, at the revision Dispatch answered it with: the
// line moves into the Advances row. The newest priority write sent that set and landed with that
// revision, but on another issue, so the move is not the reader's, and the focus stays on the
// page's body.
test("the line moved into the Advances row elsewhere after the reader's own add to another issue landed leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  patchIssue.mockImplementationOnce(async (_key, input) =>
    patchAnswer({ ...advancingEvals(5), key: "CORE-9" }, input)
  );
  const queryClient = detailsQueryClient();
  // The other issue's page, whose priority line is the one the page draws: a line in no row of
  // Details, so its picker's trigger is "Edit priorities" (`priorityTriggerName`).
  const otherPage = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <NoTooltips>
          <IssuePriorityLine disabled={false} issue={{ ...advancesOnly, key: "CORE-9" }} />
        </NoTooltips>
      </QueryClientProvider>
    </MemoryRouter>
  );
  let unmount = otherPage.unmount;
  try {
    await toggleEvals(
      user,
      await screen.findByRole("button", { name: priorityTriggerName("CORE-9", false) })
    );
    await waitFor(() =>
      expect(
        queryClient.getMutationCache().getAll().at(-1)?.state.status,
        "the reader's add to CORE-9 did not land"
      ).toBe("success")
    );
    expect(patchIssue.mock.calls.map(([key]) => key)).toEqual(["CORE-9"]);
    otherPage.unmount();

    unmount = renderDetails(undefined, undefined, queryClient).unmount;
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    act(() => control.focus());
    drawIssue(getIssue, queryClient, advancingEvals(5));
    await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    await settleFocus();
    expect(
      document.activeElement,
      "another session moved the line after the reader's add to another issue landed, and the focus left the page's body though no write to this issue moved it"
    ).toBe(document.body);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

/** Adds the priority "Evals collecting data now" from the add control beside Add, by keyboard
 *  (`toggleEvals`), and returns once the picker has closed, which sends the write and gives the
 *  focus back to the add control beside Add while the write is out. */
async function addFirstPriority(user: UserEvent): Promise<void> {
  await toggleEvals(
    user,
    await screen.findByRole("button", { name: addPriorityControlName("CORE-1") })
  );
}

// The reader's first priority is refused (409 ADVANCES_REVISION, another session having moved the
// set), and the read of the issue the refusal sets off draws the priority that session added: the
// line moves into the Advances row with the focus on the add control beside Add, which the
// reader's write held refused until that read was drawn. The write saved nothing, so the move is
// not the reader's, and the focus stays on the page's body.
test("a refused first priority whose read draws another session's priority leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  patchIssue.mockImplementationOnce(() => Promise.reject(advancesRefusal()));
  const view = renderDetails();
  try {
    await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    // The issue's read after the refusal is held, so the line moves only once it is drawn.
    const reread = Promise.withResolvers<IssueDetails>();
    getIssue.mockImplementation(() => reread.promise);
    await addFirstPriority(user);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        document.activeElement,
        "the line gave the focus back to no add control beside Add"
      ).toBe(screen.queryByRole("button", { name: addPriorityControlName("CORE-1") }))
    );
    await act(async () =>
      reread.resolve({ ...advancesOnly, advances: [{ id: "PRI-9", inherited: false }] })
    );
    await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    await settleFocus();
    expect(
      document.activeElement,
      "the refused write's read moved the line, and the focus left the page's body though the reader's write saved nothing"
    ).toBe(document.body);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// While the reader's first priority is out, an event's read draws the priority another session
// added: the line moves into the Advances row with the focus on the add control beside Add, which
// the reader's write holds refused. That write has not landed, so the move is not the reader's,
// and the focus stays on the page's body.
test("an event's read of another session's first priority while the reader's is out leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  // The reader's write is held out for the whole case.
  patchIssue.mockImplementationOnce(() => new Promise(() => undefined));
  const { queryClient, unmount } = renderDetails();
  try {
    await addFirstPriority(user);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    const control = screen.getByRole("button", { name: addPriorityControlName("CORE-1") });
    await waitFor(() => expect(document.activeElement).toBe(control));
    expect(
      control,
      "the add control does not refuse a press while the write is out"
    ).toHaveAttribute("aria-disabled", "true");
    drawIssue(getIssue, queryClient, {
      ...advancesOnly,
      advances: [{ id: "PRI-9", inherited: false }],
    });
    await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    await settleFocus();
    expect(
      document.activeElement,
      "an event's read moved the line while the reader's write was out, and the focus left the page's body"
    ).toBe(document.body);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// The reader's first priority is refused (409 ADVANCES_REVISION), and the read of the issue the
// refusal sets off still draws none, so the line stays beside Add with the focus on its add
// control. Once the write has ended, an event's read draws the very priority it sent, which
// another session added: the line moves into the Advances row. The reader's write saved nothing,
// so the move is not theirs, and the focus stays on the page's body.
test("a refused first priority whose set an event's read draws after the write ended leaves the focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  patchIssue.mockImplementationOnce(() => Promise.reject(advancesRefusal()));
  const { queryClient, unmount } = renderDetails();
  try {
    await addFirstPriority(user);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    const control = screen.getByRole("button", { name: addPriorityControlName("CORE-1") });
    await waitFor(() =>
      expect(
        control,
        "the add control still refuses a press after the refused write ended"
      ).not.toHaveAttribute("aria-disabled")
    );
    expect(
      document.activeElement,
      "the add control beside Add lost the focus as the write was refused"
    ).toBe(control);

    drawIssue(getIssue, queryClient, {
      ...advancesOnly,
      advances: [{ id: "evals-now", inherited: false }],
    });
    await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    await settleFocus();
    expect(
      document.activeElement,
      "an event's read drew the set of the reader's refused write after it ended, and the focus left the page's body though that write saved nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// On a closed issue another session's write takes away the last priority, and the Advances row
// with it, with the focus on its priority's link. The reader pressed nothing, so the focus stays on
// the page's body, as for every change the reader did not make.
test("on a closed issue, the last priority taken away elsewhere leaves the focus of its link on the page's body", async () => {
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  const closed: IssueDetails = {
    ...advancesOnly,
    advances: [{ id: "evals-now", inherited: false }],
    closed_at: "2026-09-10T00:00:00Z",
    status: "done",
  };
  getIssue.mockResolvedValue(closed);
  const { queryClient, unmount } = renderDetails();
  try {
    const link = await waitFor(() =>
      within(detailsRow(PROPERTY_LABELS.advances)).getByRole("link")
    );
    act(() => link.focus());
    act(() => queryClient.setQueryData(issueQuery("CORE-1").queryKey, { ...closed, advances: [] }));
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the Advances row went on a closed issue, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// Another session's write moves the priority's line into an Advances row at the cap, with the
// focus on the add control beside Add. The reader pressed nothing, so the focus stays on the
// page's body rather than going to the row's picker trigger. That trigger is drawn at the cap
// too, since it takes a priority off as well as adding one; what the cap changes is the picker,
// which refuses one priority more: it ticks none past the cap, says why, and closing it writes
// nothing.
test("the line moved by a write elsewhere into an Advances row at the cap leaves the focus of the add control beside Add on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  const { queryClient, unmount } = renderDetails();
  try {
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    act(() => control.focus());
    const atCap = Array.from({ length: MAX_ISSUE_ADVANCES }, (_, index) => ({
      id: `priority-${index + 1}`,
      inherited: false,
    }));
    act(() =>
      queryClient.setQueryData(issueQuery("CORE-1").queryKey, {
        ...advancesOnly,
        advances: atCap,
      })
    );
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    await settleFocus();
    expect(
      document.activeElement,
      "the line moved into the Advances row at the cap, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);

    await user.click(
      within(row).getByRole("button", { name: priorityTriggerName("CORE-1", false) })
    );
    const evals = await evalsOption();
    await user.click(evals);
    expect(evals, "the picker at the cap ticked one priority more").toHaveAttribute(
      "aria-selected",
      "false"
    );
    expect(
      screen.getByText(STATE_LINES.advances_at_cap),
      "the picker at the cap refused one priority more and said nothing of why"
    ).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(patchIssue, "the picker at the cap wrote").not.toHaveBeenCalled();
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// `checkVisibility()` is missing from browsers Dispatch still runs on (`shell/roving.ts`), and
// there `usableControl` reads the attributes alone: a control that takes a press is still
// listed, and an element that has left the page still takes none.
test("with no checkVisibility, Add still lists a control that takes a press", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(advancesOnly);
  const checkVisibility = Object.getOwnPropertyDescriptor(Element.prototype, "checkVisibility");
  // Unmounts what the case drew, once it has drawn it.
  let unmount = () => {};
  try {
    Reflect.deleteProperty(Element.prototype, "checkVisibility");
    unmount = renderDetails().unmount;
    expect(checkVisibility, "the DOM has no checkVisibility of its own to take away").toBeDefined();
    expect(typeof document.body.checkVisibility).toBe("undefined");
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "with no checkVisibility, Add does not list Advances, whose control takes a press"
    ).toEqual([PROPERTY_LABELS.advances]);
    expect(
      usableControl(document.createElement("button")),
      "with no checkVisibility, a control that is not on the page takes a press"
    ).toBe(false);
  } finally {
    unmount();
    if (checkVisibility !== undefined) {
      Object.defineProperty(Element.prototype, "checkVisibility", checkVisibility);
    }
    priorities.mockRestore();
    restore();
  }
});

/** Details as the shell's drawer draws it: the details slot, whose panel the drawer's `aside`
 *  holds under a box of the sheet's own (`MarginSheet.tsx`'s `SheetBody`, which the phone's
 *  sheet hides while a margin thread is open), the box found by the test id `sheet-body`. */
function renderDetailsDrawer(details: IssueDetails) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  return render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <DetailsSlot issue={details} route={{ key: "CORE-1", kind: "issue" }} />
            <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
              <div data-testid="sheet-body">
                <MarginBody />
              </div>
            </aside>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// The phone's sheet hides the box that holds the panel while a margin thread is open
// (`MarginSheet.tsx`'s `SheetBody`). That box is between the panel and Details' `aside`, where
// `usableControl` reads every element, so a control of Details takes no press while it is
// hidden: Add lists no entry then, and lists it again once the box is shown. Neither change is a
// commit of Add's or a change inside the panel, so Add sees them only by watching the whole
// `aside`; watching the panel alone, a read made while the box was hidden left Add gone.
test("Add lists no entry while the box between the panel and Details' aside is hidden, and lists it again once it is shown", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  const view = renderDetailsDrawer(advancesOnly);
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Advances"
    ).toEqual([PROPERTY_LABELS.advances]);
    await user.keyboard("{Escape}");
    const box = screen.getByTestId("sheet-body");
    expect(
      box.contains(addMenuControl()) && box.closest(`aside[${DETAILS_ATTRIBUTE}]`) !== null,
      "Add is not under the box in Details' aside"
    ).toBe(true);
    await act(async () => box.setAttribute("hidden", ""));
    await waitFor(() =>
      expect(
        addMenuControl(),
        "Add lists Advances while the box that holds the panel is hidden"
      ).toBeNull()
    );
    await act(async () => box.removeAttribute("hidden"));
    await waitFor(() =>
      expect(
        addMenuControl(),
        "the box that holds the panel is shown again, but Details draws no Add"
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// Details closed - its `aside` hidden (`Margin.tsx`), which a browser draws as `display: none`
// and happy-dom draws as nothing at all, so the case sets that style too - is no refusal
// (`usableControl`): Details opens on what Add listed while it was closed, so that is what Add
// lists once Details is open, and no read on opening has to put an entry back.
test("Add lists the same entries while Details is closed as once it is open again", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  const view = renderDetailsDrawer(advancesOnly);
  // Add's entries, read whether or not Details is drawn.
  const listed = () =>
    screen.queryAllByRole("menuitem", { hidden: true }).map((entry) => entry.textContent);
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(listed(), "Add lists other than Advances").toEqual([PROPERTY_LABELS.advances]);
    const details = addMenuControl()?.closest<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}]`);
    if (details == null) throw new Error("Add is not in Details' aside");
    await act(async () => {
      details.setAttribute("hidden", "");
      details.style.display = "none";
    });
    expect(listed(), "Add lists other entries while Details is closed").toEqual([
      PROPERTY_LABELS.advances,
    ]);
    await act(async () => {
      details.removeAttribute("hidden");
      details.style.removeProperty("display");
    });
    expect(listed(), "Add lists other entries once Details is open again").toEqual([
      PROPERTY_LABELS.advances,
    ]);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** The issue of the two cases below: a value for every property Add lists but the assignee, so
 *  Add's one entry is Assignee. */
const assigneeOnly: IssueDetails = {
  ...advancesOnly,
  advances: [{ id: "PRI-7", inherited: false }],
  assignee: null,
};

/** The page's header, not yet on the page, with the assignee's control as the header draws it:
 *  a select named by `assigneeControlName` (`AssigneeControl.tsx`), which Add focuses rather
 *  than clicks. */
function assigneeHeader(): { assignee: HTMLSelectElement; header: HTMLElement } {
  const header = document.createElement("div");
  header.setAttribute("data-issue-header", "");
  const assignee = document.createElement("select");
  assignee.setAttribute("aria-label", assigneeControlName("CORE-1"));
  header.append(assignee);
  return { assignee, header };
}

// The assignee's control is the header's, and the header can be drawn after Details. Add then
// finds no header to watch, so it watches the body for one drawn later, and lists Assignee once
// a header that draws the assignee's control is on the page, though that is no commit of Add's.
test("Add lists Assignee once a header drawn after it draws the assignee's control", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(assigneeOnly);
  const { header } = assigneeHeader();
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    expect(
      addMenuControl(),
      "Add is drawn before any header draws the assignee's control, which its one entry reaches"
    ).toBeNull();
    await act(async () => document.body.append(header));
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Assignee once the header draws the assignee's control"
    ).toEqual([PROPERTY_LABELS.assignee]);
  } finally {
    header.remove();
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// The assignee's control is in the header, which the phone's Details sheet covers, so Add's
// Assignee closes an open Details and reaches the control two frames on, once the drawer has
// given the focus back as it closes. The header draws that control as a select, and a script's
// click opens no select's list, so Add puts the focus on it.
test("Add's Assignee closes an open Details, then puts the focus on the header's assignee select", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  // Open, Details reads what waits on the issue.
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  getIssue.mockResolvedValue(assigneeOnly);
  const { assignee, header } = assigneeHeader();
  document.body.append(header);
  const setOpen = jest.fn((_open: boolean) => undefined);
  const { unmount } = renderDetails(undefined, { ...OPEN_DRAWER, setOpen });
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(screen.getByRole("menuitem", { name: PROPERTY_LABELS.assignee }));
    expect(setOpen.mock.calls, "Add's Assignee left Details open over the header").toEqual([
      [false],
    ]);
    await waitFor(() =>
      expect(assignee, "Add's Assignee put no focus on the header's select").toHaveFocus()
    );
  } finally {
    header.remove();
    unmount();
    getReferences.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

// With Details open, the repository's control drew its select only once a read of its own
// answered, in a commit of that control alone, and Add, read after its own commits only, never
// saw it, so on an issue with a value for every other property Details drew no Add beside the
// control it would press. The control is drawn at once, whatever its read of the project list,
// which its suggestion comes from, answers: on an issue that resolves to no repository the row
// reads "None", a button whose press draws the box in its place. So nothing here waits on that
// read, which is held out throughout; what the case still holds is that Add is drawn beside that
// one control, lists Repository, and that choosing it reaches the box.
test("Add lists Repository on an empty repository row and presses None, which opens the box", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  // Open, Details reads what waits on the issue.
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  // The control reads the project list as it is drawn; the read never answers here, and the
  // control is drawn without it.
  const listProjects = spyOn(api, "listProjects").mockImplementation(
    () => new Promise(() => undefined)
  );
  getIssue.mockResolvedValue({
    ...issue,
    advances: [{ id: "PRI-7", inherited: false }],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    route: "role:legion-controller-core",
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent(CONTROL_LABELS.none)
    );
    expect(listProjects).toHaveBeenCalled();
    const none = within(detailsRow(PROPERTY_LABELS.repository)).getByRole("button", {
      name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
    });
    expect(none, "the empty Repository row does not read None").toHaveTextContent(
      CONTROL_LABELS.none
    );
    expect(
      screen.queryByRole("combobox", { name: repoControlName("CORE-1") }),
      "the empty Repository row draws its box before None is pressed"
    ).not.toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.add }),
        "Repository has no value and its row draws None, but Details draws no Add"
      ).toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    const entry = await screen.findByRole("menuitem", { name: PROPERTY_LABELS.repository });
    expect(
      entry,
      "the add menu lists no Repository for the None the row draws"
    ).toBeInTheDocument();
    await user.click(entry);
    const box = await screen.findByRole("combobox", { name: repoControlName("CORE-1") });
    await waitFor(() =>
      expect(box, "the menu's Repository put no focus in the repository's box").toHaveFocus()
    );
    expect(none, "the menu's Repository left None drawn beside the box").not.toBeInTheDocument();
  } finally {
    unmount();
    listProjects.mockRestore();
    getReferences.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

// An issue with no repository of its own that inherits one has no value for Repository, so Add
// lists it; its row draws the inherited repository as text, a button whose press draws the box in
// its place. Add has to press that text: it finds neither "None" nor the box, which the row draws
// only once the text is pressed, and with neither it listed no Repository for the issue at all.
test("Add lists Repository on a row that inherits its repository and presses its text, which opens the box", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  // Open, Details reads what waits on the issue.
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  // A value for every property Add lists but the repository, which the issue inherits: the API
  // answers no repository of its own and the one it resolves to.
  getIssue.mockResolvedValue({
    ...advancesOnly,
    advances: [{ id: "PRI-7", inherited: false }],
    repo: null,
    resolved_repo: "acme/web",
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    const text = await within(detailsRow(PROPERTY_LABELS.repository)).findByRole("button", {
      name: repoTextControlName("CORE-1", { resolvedRepo: "acme/web", words: CONTROL_NAME_WORDS }),
    });
    expect(text, "the Repository row does not draw the repository it inherits").toHaveTextContent(
      "acme/web"
    );
    expect(
      screen.queryByRole("combobox", { name: repoControlName("CORE-1") }),
      "the Repository row draws its box before its text is pressed"
    ).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Repository for an issue that inherits its repository"
    ).toEqual([PROPERTY_LABELS.repository]);
    await user.click(screen.getByRole("menuitem", { name: PROPERTY_LABELS.repository }));
    const box = await screen.findByRole("combobox", { name: repoControlName("CORE-1") });
    await waitFor(() =>
      expect(box, "the menu's Repository put no focus in the repository's box").toHaveFocus()
    );
    expect(
      text,
      "the menu's Repository left the inherited repository drawn beside the box"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    getReferences.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

/** That `alert`, a failure said in `row` of Details, is Details' own line (`details-error.tsx`):
 *  in the page's colour for what failed, with nothing in the row in a colour that is not the
 *  page's (`offPageColours`), as the shared `QueryError`'s danger colour is not, and with the
 *  page's Retry. */
function expectPageFailure(row: HTMLElement, alert: HTMLElement): void {
  expect(alert, "the failure is not in the page's colour for what failed").toHaveClass(
    pageFailedText
  );
  expect(
    offPageColours(PROPERTY_LABELS.repository, [row, ...row.querySelectorAll("*")]).offenders,
    "the row draws the failure in a colour that is not the page's"
  ).toEqual([]);
  expect(
    within(row).getByRole("button", { name: CONTROL_LABELS.retry }),
    "Retry is not drawn as the page's control"
  ).toHaveClass(pageHeaderControl);
}

// A repository Dispatch refuses is said in the Repository row as Details says its own failures
// (`expectPageFailure`), in Dispatch's words, and its Retry makes the refused pick again.
test("a repository Dispatch refuses is said in the page's colours, with Retry", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const reason = 'repo must be "owner/name"';
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "INVALID_ISSUE", error: reason }))
  );
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/web" }, keyedAttempt)
    );
    const row = detailsRow(PROPERTY_LABELS.repository);
    const alert = await within(row).findByRole("alert");
    expectPageFailure(row, alert);
    expect(alert.textContent).toBe(reason);
    await user.click(within(row).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      patchIssue.mock.calls.map(([, input]) => input),
      "Retry did not send the refused repository again"
    ).toEqual([{ repo: "acme/web" }, { repo: "acme/web" }]);
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert"),
        "the refusal is still said after the repository landed"
      ).not.toBeInTheDocument()
    );
  } finally {
    unmount();
    restore();
  }
});

// A refused repository's line offers its Retry while the issue is open. With the reader's focus
// on that Retry, a close made in another session takes it away, as a closed issue's line offers
// none. The reader pressed nothing on that line, so the focus stays on the page's body.
test("a close made elsewhere that takes the refused repository's Retry leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "repository refused" }))
  );
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    const row = detailsRow(PROPERTY_LABELS.repository);
    await within(row).findByRole("alert");
    const retry = within(row).getByRole("button", { name: CONTROL_LABELS.retry });
    act(() => retry.focus());

    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(retry, "the close left the refused write's Retry").not.toBeInTheDocument()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "a close elsewhere took the refused write's Retry, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A press of a refused repository's Retry clears the refusal as it sends the pick again, so the
// line and its Retry go at the press, and the focus of a reader who pressed Retry by keyboard
// went with them to the page's body. It goes to the row's box instead, which the row draws while
// the write is out, and from there to the row's text once the repository lands.
test("a refused repository's Retry hands the focus to the row's box as its line goes", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "repository refused" }))
  );
  const { unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    const row = detailsRow(PROPERTY_LABELS.repository);
    await within(row).findByRole("alert");
    const retry = within(row).getByRole("button", { name: CONTROL_LABELS.retry });

    const retried = Promise.withResolvers<Issue>();
    patchIssue.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert"),
        "the refusal is still said while the pick is sent again"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the refusal's line went, and the focus fell to the page rather than to the row's box"
    ).toBe(within(row).getByRole("combobox", { name: repoControlName("CORE-1") }));

    // Dispatch answers the write, and every read of the issue after it, with the issue's own
    // repository as the one it resolves to.
    const landed = { ...issue, repo: "acme/web", resolved_repo: "acme/web" };
    getIssue.mockResolvedValue(landed);
    await act(async () => retried.resolve(narrowIssue(landed)));
    await waitFor(() =>
      expect(
        within(row).queryByRole("combobox", { name: repoControlName("CORE-1") }),
        "the row still draws its box after the repository landed"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the repository landed, and the focus fell to the page rather than to the row's control"
    ).toBe(
      within(row).getByRole("button", {
        name: repoTextControlName("CORE-1", {
          resolvedRepo: "acme/web",
          words: CONTROL_NAME_WORDS,
        }),
      })
    );
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A repository still out as the issue closes is refused (`ISSUE_CLOSED`): Dispatch writes no
// repository of a closed issue, so the refusal is said with no Retry, which Dispatch would refuse
// again.
test("a repository refused because the issue closed is said without Retry", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const reason = "CORE-1 is closed; reopen it first";
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/web" }, keyedAttempt)
    );

    // The issue closes while the write is out, and Dispatch refuses the write for it.
    drawIssue(getIssue, queryClient, closedIssue);
    saving.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: reason }));

    const row = detailsRow(PROPERTY_LABELS.repository);
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert")?.textContent,
        "the Repository row does not say why Dispatch refused the repository"
      ).toBe(reason)
    );
    expect(
      within(row).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the refusal of a repository on a closed issue offers a Retry Dispatch would refuse again"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A repository pick the reader stopped may have been saved. On an open issue its line says so
// with the Retry that asks again under the same key. A closed issue's row draws no Retry, since
// Dispatch refuses every repository of a closed issue, so there the line says the issue is closed
// rather than offer that Retry.
test("a stopped repository pick offers its Retry while the issue is open, and says it is closed once it is", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    jest.useFakeTimers();
    await userOnFakeClock().keyboard("{Enter}");
    await stopTheLateWrite(queryClient);
    const row = detailsRow(PROPERTY_LABELS.repository);
    expect(within(row).getByRole("alert").textContent).toBe(
      stoppedWriteNote("the repository of CORE-1", "saved", {
        method: "PATCH",
        stopped: new SendStoppedError(),
      })
    );
    expect(within(row).queryByRole("button", { name: CONTROL_LABELS.retry })).not.toBeNull();

    drawIssue(getIssue, queryClient, closedIssue);
    expect(
      within(row).getByRole("alert").textContent,
      "the stopped pick's line on a closed issue offers a Retry the row does not draw"
    ).toBe(
      stoppedWriteNote("the repository of CORE-1", "saved", {
        anew: "The issue is closed, so the repository cannot be saved again.",
      })
    );
    expect(within(row).queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull();
  } finally {
    realClock();
    unmount();
    restore();
  }
});

/** A second caller of the issue's writes on the page, as the header slot is: slots share no
 *  state, so each makes its own set (`slots/details.tsx`). */
function OtherWrites({ issue: details }: { issue: IssueDetails }): ReactNode {
  const writes: IssueHeaderWrites = useIssueHeaderWrites(
    details,
    stateForIssue(undefined, details.key)
  );
  return (
    <button
      onClick={() =>
        writes.updateIssue.mutate({ input: { status: "in_progress" }, issueKey: details.key })
      }
      type="button"
    >
      Start it from the header
    </button>
  );
}

// The page never says two statuses. The header and Details each call the writes, so Details
// reads every pending write of the issue's fields by their one mutation key, whichever caller
// made it (`useIssueHeaderWrites.ts`'s `pendingStatus`).
test("Details' Status reads the status another caller of the writes is saving", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { unmount } = renderDetails((details) => <OtherWrites issue={details} />);

  try {
    await screen.findByText(PROPERTY_LABELS.status, { selector: "[data-page-chrome]" });
    expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("todo"));
    await user.click(screen.getByRole("button", { name: "Start it from the header" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.status),
        "Details' Status kept the saved status while the header's write of another was out"
      ).toHaveTextContent(issueStatusWord("in_progress"))
    );

    saving.resolve(narrowIssue({ ...issue, status: "in_progress" }));
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("in_progress"))
    );
  } finally {
    unmount();
    restore();
  }
});

/** A second caller that saves a title and picks a status while that title is out, as the header
 *  does when the reader picks a status before the title they saved has landed. */
function RetitleThenStart({ issue: details }: { issue: IssueDetails }): ReactNode {
  const writes = useIssueHeaderWrites(details, stateForIssue(undefined, details.key));
  return (
    <>
      <button
        onClick={() => {
          writes.drafts.writeTitle("Review the spec again");
          writes.drafts.requestTitleSubmit();
        }}
        type="button"
      >
        Retitle it from the header
      </button>
      <button onClick={() => writes.pickStatus("in_progress")} type="button">
        Start it from the header
      </button>
    </>
  );
}

// A status picked while another write of the issue is out is held for every caller by
// `pickStatus` (`useIssueHeaderWrites.ts`), so Details draws it at once, and it is sent once,
// after that write.
test("Details' Status reads a status picked behind another write", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const retitling = Promise.withResolvers<Issue>();
  patchIssue
    .mockImplementationOnce(() => retitling.promise)
    .mockImplementationOnce(async () =>
      narrowIssue({ ...issue, status: "in_progress", title: "Review the spec again" })
    );
  const { queryClient, unmount } = renderDetails((details) => <RetitleThenStart issue={details} />);

  try {
    await screen.findByText(PROPERTY_LABELS.status, { selector: "[data-page-chrome]" });
    expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("todo"));
    await user.click(screen.getByRole("button", { name: "Retitle it from the header" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { title: "Review the spec again" },
        keyedAttempt
      )
    );

    await user.click(screen.getByRole("button", { name: "Start it from the header" }));
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.status),
        "Details' Status kept the saved status while a pick held behind the header's title stood"
      ).toHaveTextContent(issueStatusWord("in_progress"))
    );
    expect(
      patchIssue,
      "the pick went out while the title write before it was out"
    ).toHaveBeenCalledTimes(1);

    retitling.resolve(narrowIssue({ ...issue, title: "Review the spec again" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      patchIssue.mock.calls.map(([, input]) => input),
      "the held status did not go out after the title write it was held behind"
    ).toEqual([{ title: "Review the spec again" }, { status: "in_progress" }]);

    // Once no write is out, the row reads the status the server answered with.
    await waitFor(() => {
      expect(queryClient.isMutating()).toBe(0);
      expect(queryClient.getQueryData(issueQuery("CORE-1").queryKey)?.status).toBe("in_progress");
    });
    expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("in_progress"));
    expect(patchIssue, "the held status was sent more than once").toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

/** The margin's own content, drawn through the fill the details slot put in its slot, as the
 *  shell's Details drawer draws it (`MarginSheet.tsx`'s `SheetBody`). */
function MarginBody(): ReactNode {
  return (
    <FilledMarginSlot fill={useMarginSlotFill()}>
      <p>A thread anchored in the document</p>
    </FilledMarginSlot>
  );
}

/** What a document's editor does to the margin while it is mounted (`ProofDocument.tsx`). */
function DocumentEditor(): ReactNode {
  const { registerDocument } = useMargin();
  return (
    <>
      <button
        onClick={() => registerDocument({ artifactId: "artifact-1" } as unknown as DocumentBridge)}
        type="button"
      >
        Open the document
      </button>
      <button onClick={() => registerDocument(undefined)} type="button">
        Close the document
      </button>
    </>
  );
}

// Details holds no card of the margin while no document's editor is on the page, and while one
// is, the margin's own cards stand above the sections, for a comment on a selection.
test("the margin's own cards are drawn only while a document's editor is on the page", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <DetailsSlot issue={issue} route={{ key: "CORE-1", kind: "issue" }} />
            <MarginBody />
            <DocumentEditor />
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const development = await screen.findByRole("button", {
      name: MARGIN_SECTION_NAMES.development,
    });
    expect(
      screen.queryByText("A thread anchored in the document"),
      "Details draws a card of the margin with no document's editor on the page"
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Open the document" }));
    const card = await screen.findByText("A thread anchored in the document");
    expect(
      card.compareDocumentPosition(development) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the margin's own cards are not above Details' sections"
    ).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Close the document" }));
    await waitFor(() =>
      expect(screen.queryByText("A thread anchored in the document")).not.toBeInTheDocument()
    );
    expect(
      screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }),
      "the panel was drawn again when the margin's cards went"
    ).toBe(development);
  } finally {
    view.unmount();
    restore();
  }
});

// A control elsewhere on the page that opens Details at one of its rows asks the drawer for that
// row first: the section that holds the row opens for it as its heading opens it, so the
// reader's kept choice follows, and a section already open stays open.
test("a request for the Labels row opens a closed Development in the drawer that holds the panel", async () => {
  const { restore } = stubIssueApi();
  const kept = userPreferenceStorageKey("alice", "issue.margin-sections");
  window.localStorage.setItem(kept, "development");
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <DetailsSlot issue={issue} route={{ key: "CORE-1", kind: "issue" }} />
            <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
              <MarginBody />
            </aside>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const development = await screen.findByRole("button", {
      name: MARGIN_SECTION_NAMES.development,
    });
    await waitFor(() => expect(development).toHaveAttribute("aria-expanded", "false"));
    const drawer = development.closest(`aside[${DETAILS_ATTRIBUTE}]`);
    expect(drawer, "the panel is not in the drawer's aside").not.toBeNull();
    const request = (row: DetailsRow) =>
      act(() => {
        drawer?.dispatchEvent(new CustomEvent(DETAILS_ROW_REQUEST_EVENT, { detail: row }));
      });

    request("labels");
    expect(development, "a request for the Labels row left Development closed").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    expect(
      document.querySelector(`[${DETAILS_ROW_ATTRIBUTE}="labels"]`),
      "Development opened without its Labels row"
    ).not.toBeNull();
    expect(window.localStorage.getItem(kept), "the reader's kept choice did not follow").toBe("");

    request("labels");
    expect(development, "a request for a row of an open Development closed it").toHaveAttribute(
      "aria-expanded",
      "true"
    );
  } finally {
    view.unmount();
    restore();
    window.localStorage.removeItem(kept);
  }
});

// Each row draws its name one time. The Labels row of an issue with no label reads its name,
// None, and a control that adds the first label; that control's visible word is not the row's
// name again, which would read "Labels None Labels".
test("the Labels row of an issue with no label draws its name one time", async () => {
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    const row = await waitFor(() => rowAt("labels"));
    expect(row).toHaveTextContent(CONTROL_LABELS.none);
    const control = within(row).getByRole("button", {
      name: labelsControlName("CORE-1", { labels: [], saving: false, words: CONTROL_NAME_WORDS }),
    });
    expect(control, "the Labels row of an issue with no label draws no Edit labels").toBeEnabled();
    expect(control, "the Labels row's control is busy, and takes no press").not.toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(
      (row.textContent ?? "").split(PROPERTY_LABELS.labels).length - 1,
      "the Labels row draws its name two times on an issue with no label"
    ).toBe(1);
  } finally {
    unmount();
    restore();
  }
});

/** The issue page's scope of keys, pushed as the page pushes it (`useIssueKeymap.ts`), so a key
 *  the details slot binds in that scope is read. */
function IssueKeys(): null {
  useKeymapScope("issue");
  return null;
}

/** The details slot and the margin's own content in a column, under the issue page's keys
 *  (`IssueKeys`), so the key `l` reads the labels' control the slot draws for `details`. */
function renderWithKeys(details: IssueDetails) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  return render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssueKeys />
          <MarginProvider>
            <MarginSlotProvider drawsSlot>
              <DetailsSlot issue={details} route={{ key: "CORE-1", kind: "issue" }} />
              <MarginBody />
            </MarginSlotProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Whether the key `l` is on, as the list of keys says it. */
function labelsKey(): boolean | undefined {
  return appKeymap.describe().find((entry) => entry.id === "labels")?.enabled;
}

// On an issue with no label the labels' control is named for adding one (`labelsControlName`),
// and the add menu's Labels and the key `l` find it by that name. While both looked for "Edit
// labels", the menu listed no Labels on such an issue and `l` did nothing.
test("on an issue with no label, the Add menu's Labels and the key l reach the labels control", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  // The labels' popover reads the project's issues for the labels it offers.
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const view = renderWithKeys(issue);

  try {
    const control = within(await waitFor(() => rowAt("labels"))).getByRole("button", {
      name: labelsControlName("CORE-1", { labels: [], saving: false, words: CONTROL_NAME_WORDS }),
    });
    expect(control).toHaveAttribute("aria-expanded", "false");

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.labels }));
    await waitFor(() =>
      expect(control, "the add menu's Labels did not open the labels control").toHaveAttribute(
        "aria-expanded",
        "true"
      )
    );
    await user.keyboard("{Escape}");
    await waitFor(() => expect(control).toHaveAttribute("aria-expanded", "false"));

    await user.keyboard("l");
    await waitFor(() =>
      expect(control, "l did not open the labels control").toHaveAttribute("aria-expanded", "true")
    );
  } finally {
    view.unmount();
    listIssues.mockRestore();
    restore();
  }
});

// While a save of the labels is out, the labels' control is named for it ("Saving labels of
// CORE-1", `labelsControlName` given the page's `ANSWER_LINES`), and the key `l` finds it by that
// name; the control is then busy, `aria-disabled` and refusing to open, so `l` is off until the
// save lands. While `l` read only `disabled`, the list of keys named it enabled for the length of
// the save and the key pressed a control that opened nothing.
test("while the labels are saving, the key l is off, and on again once the save lands", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  // The labels' popover reads the project's issues for the labels it offers.
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const labelled: IssueDetails = { ...issue, labels: ["bug"] };
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const view = renderWithKeys(labelled);
  const savingName = labelsControlName("CORE-1", {
    labels: [],
    saving: true,
    words: CONTROL_NAME_WORDS,
  });

  try {
    const row = await waitFor(() => rowAt("labels"));
    await user.click(
      within(row).getByRole("button", {
        name: labelsControlName("CORE-1", {
          labels: ["bug"],
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      })
    );
    await user.click(await screen.findByRole("option", { name: "bug" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(
      await within(row).findByRole("button", { name: savingName }),
      "the labels control is not named for its save while the save is out"
    ).toBeInTheDocument();
    expect(
      labelsKey(),
      "l is enabled while the labels control it presses is busy with the save"
    ).toBe(false);

    saving.resolve(narrowIssue({ ...labelled, labels: [] }));
    await waitFor(() =>
      expect(within(row).queryByRole("button", { name: savingName })).not.toBeInTheDocument()
    );
    expect(labelsKey(), "l is not on again once the labels' save has landed").toBe(true);
  } finally {
    view.unmount();
    listIssues.mockRestore();
    restore();
  }
});

// `l` opens Details at the Labels row before it presses the labels' control
// (`useOpenDetailsAt`), so Details being closed - its drawer's `aside` hidden, and the control
// under it not drawn - leaves `l` on (`usableControl`, which reads a closed Details as no
// refusal), while an element inside the drawer that hides the row or takes it out of reach
// still turns it off.
test("with Details closed, l is on for the labels control, and off while its row inside Details is hidden or inert", async () => {
  const { restore } = stubIssueApi();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssueKeys />
          <MarginProvider>
            <MarginSlotProvider drawsSlot>
              <DetailsSlot issue={issue} route={{ key: "CORE-1", kind: "issue" }} />
              {/* Closed, as the drawer draws it (`Margin.tsx`): `hidden`, which a browser draws
                  as `display: none` and happy-dom draws as nothing at all. */}
              <aside hidden style={{ display: "none" }} {...{ [DETAILS_ATTRIBUTE]: "" }}>
                <MarginBody />
              </aside>
            </MarginSlotProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const row = await waitFor(() => rowAt("labels"));
    expect(
      row.closest(`aside[${DETAILS_ATTRIBUTE}]`),
      "the Labels row is not in the closed drawer"
    ).toHaveAttribute("hidden");
    const control = row.querySelector(
      `button[aria-label="${labelsControlName("CORE-1", { labels: [], saving: false, words: CONTROL_NAME_WORDS })}"]`
    );
    expect(control, "the Labels row draws no enabled labels control").toBeEnabled();
    expect(control, "the Labels row's control is busy, and takes no press").not.toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(labelsKey(), "l is off while Details is closed, though it opens Details first").toBe(
      true
    );
    for (const attribute of ["hidden", "inert"]) {
      row.setAttribute(attribute, "");
      expect(labelsKey(), `l is on while the Labels row inside Details is ${attribute}`).toBe(
        false
      );
      row.removeAttribute(attribute);
      expect(labelsKey(), `l is not on again once the Labels row is no longer ${attribute}`).toBe(
        true
      );
    }
  } finally {
    view.unmount();
    restore();
  }
});

// A closed Development keeps the Labels row on the page, hidden, and `l` reads a control under a
// hidden element as one that takes no press (`usableControl`): the key stays off while the reader
// keeps the section closed, as it was while a closed section drew no rows, and is on again once
// the section opens.
test("with Development closed, l is off, and on again once Development opens", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const view = renderWithKeys(issue);

  try {
    await waitFor(() => rowAt("labels"));
    expect(labelsKey(), "l is off while Development is open").toBe(true);
    const development = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });
    await user.click(development);
    expect(development).toHaveAttribute("aria-expanded", "false");
    expect(labelsKey(), "l is on while Development is closed").toBe(false);
    await user.click(development);
    expect(labelsKey(), "l is not on again once Development opens").toBe(true);
  } finally {
    view.unmount();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

/** The project's one architecture source, so the components line draws its picker
 *  (`shared-writes.test.ts`'s `architectureSource`). */
const architectureSource: ArchitectureSource = {
  branch: "main",
  components: 1,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: "3be61df5be90",
  last_error: null,
  last_sync_at: "2026-09-17T00:00:00Z",
  project: "CORE",
  repo: "legion/legion",
};

// On an issue no component is attached to, the components picker's trigger draws "Set
// components" and is named for it inside `NoTooltips` (`componentsControlName` given the
// attachment the line draws), and the add menu's Components finds it by that name. While the
// menu looked for `main`'s "Edit components of CORE-1", it listed no Components on such an
// issue.
test("on an issue with no component, the Add menu's Components reaches the picker", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  // The fixture's issue is not attached. A source draws the picker, and the open picker reads
  // the project's architecture for the components it offers.
  const sources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([
    architectureSource,
  ]);
  const tree = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  const { unmount } = renderDetails();

  try {
    const picker = await screen.findByRole("button", {
      name: componentsControlName("CORE-1", {
        components: issue.components,
        saving: false,
        words: CONTROL_NAME_WORDS,
      }),
    });
    expect(picker).toHaveAttribute("aria-expanded", "false");

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.components }));
    await waitFor(() =>
      expect(picker, "the add menu's Components did not open the picker").toHaveAttribute(
        "aria-expanded",
        "true"
      )
    );
  } finally {
    unmount();
    tree.mockRestore();
    sources.mockRestore();
    restore();
  }
});

// The components picker stands in Details only while the project has an architecture source, so
// a read of the sources that answers with none takes the picker away while its popover is open:
// the trigger and the popover go together, with the focus in the popover's search box. A read
// that answers is no press of the reader's, so the focus stays on the page's body, as for every
// change the reader did not make.
test("a sources read that takes the open components picker away leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const sources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([
    architectureSource,
  ]);
  const tree = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  const { queryClient, unmount } = renderDetails();

  try {
    const picker = await screen.findByRole("button", {
      name: componentsControlName("CORE-1", {
        components: issue.components,
        saving: false,
        words: CONTROL_NAME_WORDS,
      }),
    });
    await user.click(picker);
    await screen.findByRole("option", { name: "dispatch-server · Dispatch server" });
    expect(screen.getByRole("combobox", { name: "Search components" })).toHaveFocus();

    sources.mockResolvedValue([]);
    await act(() => queryClient.refetchQueries({ queryKey: ["architecture-sources"] }));
    expect(picker, "the sources read left the picker on the page").not.toBeInTheDocument();
    expect(screen.queryByRole("listbox"), "the picker left its popover open").toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "the popover went with its trigger, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    tree.mockRestore();
    sources.mockRestore();
    restore();
  }
});

// A press at the Parent row (`useOpenDetailsAt`, as the header's Set parent issue makes it)
// presses the control the row marks as its opener (`DETAILS_OPENER_ATTRIBUTE`), and an open
// without a press focuses the row's first control: both are the control that opens the parent's
// editor, which stands before the parent's link.
test("the Parent row's first control is its marked opener, which opens the parent's editor, before the parent's link", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();

  try {
    const empty = renderDetails();
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    const none = document.querySelector(`[${DETAILS_ROW_ATTRIBUTE}="parent"]`);
    expect(firstUsableIn(none)).toHaveTextContent(CONTROL_LABELS.set_parent);
    expect(firstUsableIn(none)).toHaveAttribute(
      DETAILS_OPENER_ATTRIBUTE,
      detailsOpener[DETAILS_OPENER_ATTRIBUTE]
    );
    expect(none).toHaveTextContent(CONTROL_LABELS.none);
    empty.unmount();

    getIssue.mockResolvedValue({ ...issue, parent: "CORE-2" });
    const { unmount } = renderDetails();
    const link = await screen.findByRole("link", { name: "CORE-2" });
    const row = link.closest(`[${DETAILS_ROW_ATTRIBUTE}="parent"]`);
    expect(row, "the parent's link is outside the Parent row").not.toBeNull();
    const control = firstUsableIn(row);
    expect(control).toHaveTextContent(CONTROL_LABELS.edit_parent);
    expect(control).toHaveAttribute(
      DETAILS_OPENER_ATTRIBUTE,
      detailsOpener[DETAILS_OPENER_ATTRIBUTE]
    );
    expect(
      (control?.compareDocumentPosition(link) ?? 0) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the parent's link stands before the control that edits it"
    ).toBeTruthy();

    await user.click(control as HTMLElement);
    const box = await screen.findByLabelText(PROPERTY_LABELS.parent);
    await waitFor(() => expect(box, "the parent's editor took no focus").toHaveFocus());
    unmount();
  } finally {
    restore();
  }
});

/** The address the router is at, which a link followed on the page changes. */
function PageAddress(): ReactNode {
  return <output data-testid="page-address">{useLocation().pathname}</output>;
}

/** Details on the issue `getIssue` answers, beside a control elsewhere on the page that presses
 *  at `row` (`Opener`), as the header's Set or Edit parent issue does at Parent
 *  (`slots/header.tsx`; nothing presses at the route row yet, which the cases press too so its
 *  marks are read), and the address the router is at. `press` presses that control; `stayed`
 *  checks that the reader is still on the issue. */
function renderPressAt(row: DetailsRow) {
  const { unmount } = renderDetails(() => (
    <>
      <Opener press row={row} />
      <PageAddress />
    </>
  ));
  return {
    press: (user: UserEvent) => user.click(screen.getByRole("button", { name: `Press at ${row}` })),
    stayed: () =>
      expect(
        screen.getByTestId("page-address").textContent,
        `a press at the ${row} row left the issue`
      ).toBe("/issues/CORE-1"),
    unmount,
  };
}

/** Records, from this call on, the text of every control of `row` a click reaches. */
function clicksIn(row: DetailsRow): string[] {
  const clicked: string[] = [];
  rowAt(row).addEventListener(
    "click",
    (event) => {
      if (event.target instanceof Element) clicked.push(event.target.textContent ?? "");
    },
    true
  );
  return clicked;
}

/** The rows whose editor Details draws itself, each with the issue and the name of the control
 *  that opens the editor: the parent's with no parent and with one, whose link stands before the
 *  editor, and the route's. */
const EDITOR_ROWS = [
  { details: issue, opener: CONTROL_LABELS.set_parent, row: "parent" },
  { details: { ...issue, parent: "CORE-2" }, opener: CONTROL_LABELS.edit_parent, row: "parent" },
  { details: issue, opener: "Messages default to no route", row: "route" },
] as const;

// Details keeps an open editor of the parent or the route while it is closed
// (`slots/details.tsx`), and the row then draws the editor in place of the control that opens
// it. A press at the row (`useOpenDetailsAt`, as the header's Set or Edit parent issue makes it)
// pressed the row's first control there: the parent's link, drawn before the editor, which took
// the reader to the parent issue, or else the editor's box, where a script's click puts no focus.
// It puts the focus in the editor's box and presses nothing.
test("a press at the Parent or route row with its editor open puts the focus in the editor's box and presses nothing", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();

  try {
    for (const { details, opener, row } of EDITOR_ROWS) {
      getIssue.mockResolvedValue(details);
      const { press, stayed, unmount } = renderPressAt(row);
      try {
        await user.click(await screen.findByRole("button", { name: opener }));
        const box = await screen.findByLabelText(PROPERTY_LABELS[row]);
        await waitFor(() => expect(box, `the ${row}'s editor took no focus`).toHaveFocus());
        const clicked = clicksIn(row);
        // The press moves the focus to its own control, out of the box.
        await press(user);
        await waitFor(() =>
          expect(
            box,
            `the press at ${row} (parent ${details.parent}) put no focus in the open editor's box`
          ).toHaveFocus()
        );
        expect(clicked, `a press at the ${row} row clicked a control of the row`).toEqual([]);
        stayed();
      } finally {
        unmount();
      }
    }
  } finally {
    restore();
  }
});

// A press at a row whose editor is shut presses the control the row marks as the one that opens
// it, and the editor, as it opens, puts the focus in its box.
test("a press at the Parent or route row with its editor shut opens the editor, with the focus in its box", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();

  try {
    for (const { details, opener, row } of EDITOR_ROWS) {
      getIssue.mockResolvedValue(details);
      const { press, stayed, unmount } = renderPressAt(row);
      try {
        await screen.findByRole("button", { name: opener });
        await press(user);
        await waitFor(() =>
          expect(
            document.activeElement,
            `the press at ${row} (parent ${details.parent}) put no focus in a new editor's box`
          ).toBe(screen.queryByLabelText(PROPERTY_LABELS[row]))
        );
        stayed();
      } finally {
        unmount();
      }
    }
  } finally {
    restore();
  }
});

// A closed issue's Parent row draws no control that opens the parent's editor, only the parent's
// link. A press at the row (`useOpenDetailsAt`), which finds no marked opener there, pressed the
// row's first control that takes a press, the link, which took the reader to the parent issue.
// It presses nothing.
test("a press at a closed issue's Parent row, which draws no opener, presses nothing", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...closedIssue, parent: "CORE-2" });
  const { press, stayed, unmount } = renderPressAt("parent");

  try {
    const link = await screen.findByRole("link", { name: "CORE-2" });
    expect(firstUsableIn(rowAt("parent")), "the parent's link takes no press").toBe(link);
    expect(
      rowAt("parent").querySelector(`[${DETAILS_OPENER_ATTRIBUTE}]`),
      "the closed issue's Parent row draws an opener"
    ).toBeNull();
    const clicked = clicksIn("parent");
    await press(user);
    // The opener reaches the row two frames on; these two frames end after its own.
    await act(async () => {
      await nextFrame();
      await nextFrame();
    });
    expect(clicked, "a press at the closed issue's Parent row clicked a control of it").toEqual([]);
    stayed();
  } finally {
    unmount();
    restore();
  }
});

// The parent is set, changed and cleared in its own row, from the control that row draws
// first, where the old page opened its editor from the overflow menu.
test("Details saves a parent from the Parent row's editor", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.set_parent,
      })
    );
    await waitFor(() =>
      expect(
        screen.queryAllByLabelText(PROPERTY_LABELS.parent),
        "the Parent row's control opened no editor"
      ).toHaveLength(1)
    );
    await user.click(screen.getByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: "CORE-9" }, keyedAttempt)
    );
    // The row draws the parent it saved as the way to it.
    const link = await within(detailsRow(PROPERTY_LABELS.parent)).findByRole("link", {
      name: "CORE-9",
    });
    expect(link).toHaveAttribute("href", "/issues/CORE-9");
  } finally {
    unmount();
    restore();
  }
});

test("Details clears the parent with an empty editor save", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, parent: "CORE-9" });
  const { unmount } = renderDetails();

  try {
    await screen.findByRole("link", { name: "CORE-9" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.edit_parent,
      })
    );
    const parent = (await screen.findByLabelText(PROPERTY_LABELS.parent)) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.clear(parent);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: null }, keyedAttempt)
    );
    // The row's marked opener, the control the header's Set parent issue presses, sets a parent.
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.parent).querySelector(`[${DETAILS_OPENER_ATTRIBUTE}]`),
        "the Parent row still offers to edit a parent the issue no longer has"
      ).toHaveTextContent(CONTROL_LABELS.set_parent)
    );
    expect(screen.queryByRole("link", { name: "CORE-9" })).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// The editor closes as the save goes out, so a parent Dispatch refuses is said in the row the
// reader saved it from, in Dispatch's own words, and not as the line for a write that failed. The
// focus is back on the row's control by then, so the line is an alert: it is announced as it
// arrives.
test("a parent Dispatch refuses is said in the Parent row with Dispatch's reason", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const reason = "would create a cycle: CORE-1 → CORE-9 → CORE-1";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(409, { code: "PARENT_INPUT", error: reason }))
  );
  const { unmount } = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.set_parent,
      })
    );
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.parent).querySelector(`#${PARENT_ERROR_ID}`)?.textContent,
        "the Parent row does not say why Dispatch refused the parent"
      ).toBe(reason)
    );
    expect(
      within(detailsRow(PROPERTY_LABELS.parent)).queryByRole("alert")?.textContent,
      "the Parent row does not announce why Dispatch refused the parent"
    ).toBe(reason);
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A parent save the reader stopped may have been saved. On an open issue its line says saving it
// again sends it as a new write, as the header says it. A closed issue's row draws no control
// that saves a parent, since Dispatch refuses every parent of a closed issue, so there the line
// says the issue is closed rather than offer that save.
test("a stopped parent save says it can be saved anew while the issue is open, and that it is closed once it is", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const { queryClient, unmount } = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.set_parent,
      })
    );
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    jest.useFakeTimers();
    await userOnFakeClock().click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));
    await stopTheLateWrite(queryClient);
    const line = () =>
      detailsRow(PROPERTY_LABELS.parent).querySelector(`#${PARENT_ERROR_ID}`)?.textContent;
    expect(line()).toBe(
      stoppedWriteNote("the new parent", "saved", {
        anew: "Saving it again sends it as a new write.",
      })
    );

    drawIssue(getIssue, queryClient, closedIssue);
    expect(
      line(),
      "the stopped save's line on a closed issue offers a save the row does not draw"
    ).toBe(
      stoppedWriteNote("the new parent", "saved", {
        anew: "The issue is closed, so the parent cannot be saved again.",
      })
    );
  } finally {
    realClock();
    unmount();
    restore();
  }
});

// The route's save is Details' own write, and inline, so the write store leaves its refusal to
// the row: a route Dispatch refuses is said where it was saved, in Dispatch's words and with
// Retry, as a refused parent is in the Parent row.
test("a route Dispatch refuses is said in the Messages go to row with Dispatch's reason and Retry", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  const reason = "invalid route";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: reason }))
  );
  const { unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledWith("CORE-1", { route }, keyedAttempt));

    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert")?.textContent,
        "the Messages go to row does not say why Dispatch refused the route"
      ).toBe(reason)
    );
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      patchIssue.mock.calls.map(([, input]) => input),
      "Retry did not send the refused route again"
    ).toEqual([{ route }, { route }]);
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the refusal is still said after the route landed"
      ).not.toBeInTheDocument()
    );
  } finally {
    unmount();
    restore();
  }
});

// A route save the reader stopped may have been saved: the row says so in the header's words for
// a title or a route save, whose Retry is this row's too, rather than that the save failed. A
// closed issue's route line draws no Retry, since Dispatch refuses every route of a closed issue,
// so once another session closes the issue the line says in Details' own sentence that the route
// cannot be saved again, as the parent's and the repository's lines say.
test("a route save the reader stopped says in its row that it may have been saved, not that it failed, and that the issue is closed once it is", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    jest.useFakeTimers();
    await userOnFakeClock().click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await stopTheLateWrite(queryClient);
    expect(within(rowAt("route")).getByRole("alert").textContent).toBe(
      stoppedWriteNote("this change to the issue", "saved", {
        anew: "Retry sends what the field holds now as a new write.",
      })
    );
    expect(
      within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the sentence names a Retry the row does not offer"
    ).not.toBeNull();

    drawIssue(getIssue, queryClient, closedIssue);
    expect(
      within(rowAt("route")).getByRole("alert").textContent,
      "the stopped save's line on a closed issue names a Retry the row does not draw"
    ).toBe(
      stoppedWriteNote("this change to the issue", "saved", {
        anew: "The issue is closed, so the route cannot be saved again.",
      })
    );
    expect(within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull();
  } finally {
    realClock();
    unmount();
    restore();
  }
});

// The refusal's line was drawn only while the route's save stood refused, so a press of its Retry
// took the line, and the Retry with it, away while the save it sent again was out: the focus of a
// reader who pressed Retry by keyboard went to the page's body, and stayed there when Dispatch
// refused the route again and the line came back. The line stays, with its Retry holding the
// focus, until the save is answered; a second refusal is drawn afresh with Dispatch's new reason;
// and once the route lands and the line goes, the focus goes to the row's own control rather than
// to the page.
test("the refused route's Retry keeps the focus while the save is out and refused again, and hands it to the row's control once the route lands", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
  );
  const { unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();
  const retryOfRoute = () =>
    within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry });

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );
    const refused = within(rowAt("route")).getByRole("alert");
    const retry = retryOfRoute();
    if (retry === null) throw new Error("the refused route offers no Retry");

    const retried = Promise.withResolvers<Issue>();
    patchIssue.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      retryOfRoute(),
      "the line and its Retry went while the route they sent again is out"
    ).toBe(retry);
    expect(
      document.activeElement,
      "Retry lost the focus while the route it sent again is out"
    ).toBe(retry);
    await waitFor(() =>
      expect(
        retry,
        "Retry does not refuse a press while the route it sent again is out"
      ).toHaveAttribute("aria-disabled", "true")
    );
    expect(document.activeElement, "Retry lost the focus once it refused a press").toBe(retry);
    await user.keyboard("{Enter}");
    expect(patchIssue, "a press of Retry while its save is out sent another").toHaveBeenCalledTimes(
      2
    );

    const reason = "no session holds role:legion-controller-core";
    await act(async () =>
      retried.reject(new ApiError(400, { code: "ROUTE_INVALID", error: reason }))
    );
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert")?.textContent,
        "the row does not say why Dispatch refused the route again"
      ).toBe(reason)
    );
    expect(
      within(rowAt("route")).getByRole("alert"),
      "the second refusal was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(refused);
    expect(retryOfRoute(), "the line came back with another Retry").toBe(retry);
    expect(
      document.activeElement,
      "Retry lost the focus when Dispatch refused the route again"
    ).toBe(retry);
    expect(
      retry,
      "Retry still refuses a press after Dispatch refused the route again"
    ).not.toHaveAttribute("aria-disabled", "true");

    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(3));
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the refusal is still said after the route landed"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the route landed, and the focus fell to the page rather than to the row's control"
    ).toBe(within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` }));
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// The reader presses the refused route's Retry by keyboard, and while the route it sent is out a
// close made in another session takes the Retry away, as a closed issue's line offers none; the
// line stays while that save is out. The save has not answered and the line did not go, so the
// focus stays on the page's body.
test("a close made elsewhere that takes the refused route's Retry while its route is out leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
  );
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste("role:legion-controller-core");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );
    const retry = within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.retry });

    // The route Retry sends is held out for the rest of the case.
    patchIssue.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(retry).toHaveAttribute("aria-disabled", "true"));

    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(retry, "the close left the route's Retry on the page").not.toBeInTheDocument()
    );
    expect(
      within(rowAt("route")).queryByRole("alert"),
      "the line went with its Retry while the route it sent is out"
    ).not.toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "a close elsewhere took the Retry while its route was out, and the focus left the page's body"
    ).toBe(document.body);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// The reader presses the refused route's Retry by keyboard, and while the route it sent is out
// another session sets the issue's route to that same route: the draft then holds the issue's own
// route, so the line goes with the focus on its Retry. That reached what the press asked for, so
// Details takes it for the press's answer, and the focus goes on to the row's control rather than
// to the page's body.
test("the refused route's line taken away by the route set elsewhere while its Retry's route is out hands its focus to the row's control", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
  );
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );
    const retry = within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.retry });

    // The route Retry sends is held out for the rest of the case.
    patchIssue.mockImplementationOnce(() => new Promise(() => undefined));
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));

    drawIssue(getIssue, queryClient, { ...issue, route });
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the issue holds the route, but the row still says it was refused"
      ).toBeNull()
    );
    await settleFocus();
    expect(
      document.activeElement,
      "the route set elsewhere took the line while its Retry's route was out, and the focus fell to the page rather than to the row's control"
    ).toBe(within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` }));
  } finally {
    unmount();
    restore();
  }
});

// A refused route stays the draft until the reader leaves the editor, which puts the saved route
// back: the refusal is said while the draft still holds the route Dispatch refused, and not once
// it no longer does, when its Retry would send nothing.
test("a refused route is said only while the draft still holds it", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
  );
  const { unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );

    await user.click(
      within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` })
    );
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      within(rowAt("route")).queryByRole("button", { name: "Messages default to no route" }),
      "Cancel did not put the saved route back"
    ).toBeInTheDocument();
    expect(
      within(rowAt("route")).queryByRole("alert"),
      "the row still says the refusal of a route the reader left, with a Retry that sends nothing"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A route still out as the issue closes is refused (`ISSUE_CLOSED`): Dispatch saves no route of
// a closed issue, so the refusal is said with no Retry, which would send nothing, and the row's
// control, disabled now, names the route the issue holds rather than the one refused.
test("a route refused because the issue closed is said without Retry, and the row names the saved route", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const saved = "role:legion-controller-core";
  const refused = "role:legion-reviewer";
  const reason = "CORE-1 is closed; reopen it first";
  getIssue.mockResolvedValue({ ...issue, route: saved });
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: `Messages default to ${saved}` }));
    await user.clear(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(refused);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { route: refused }, keyedAttempt)
    );

    // The issue closes while the save is out, and Dispatch refuses the save for it.
    drawIssue(getIssue, queryClient, { ...closedIssue, route: saved });
    saving.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: reason }));

    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert")?.textContent,
        "the Messages go to row does not say why Dispatch refused the route"
      ).toBe(reason)
    );
    expect(
      within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the refusal of a route on a closed issue offers a Retry that sends nothing"
    ).not.toBeInTheDocument();
    expect(
      within(rowAt("route"))
        .getByRole("button", { name: /^Messages default to / })
        .getAttribute("aria-label"),
      "the route's control does not name the route the closed issue holds"
    ).toBe(`Messages default to ${saved}`);
  } finally {
    unmount();
    restore();
  }
});

// The box an editor of a row holds the focus in goes with the editor, so leaving it - by a save,
// by Cancel or by Escape - gives the focus back to the row's own control, not to the page.
test("leaving the parent's or the route's editor gives the focus back to its row's control", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();
  const control = (row: DetailsRow) => firstUsableIn(rowAt(row)) ?? null;

  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.set_parent }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(document.activeElement, "Save parent by Enter left the focus on the page").toBe(
        control("parent")
      )
    );

    await within(rowAt("parent")).findByRole("link", { name: "CORE-9" });
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.edit_parent }));
    await user.click(within(rowAt("parent")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(document.activeElement, "Cancel of the parent's editor left the focus on the page").toBe(
      control("parent")
    );
    expect(control("parent")).toHaveTextContent(CONTROL_LABELS.edit_parent);

    await user.click(screen.getByRole("button", { name: "Messages default to no route" }));
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(document.activeElement, "Cancel of the route's editor left the focus on the page").toBe(
      control("route")
    );
  } finally {
    unmount();
    restore();
  }
});

// On a closed issue the Parent row draws no control and the route's control is disabled, so
// leaving either editor once the issue had closed under it gave the focus to no control that
// takes it, and it stayed on the page's body. It goes to Development's heading, the button that
// opens and closes the section these rows are in, where the row holds nothing else to take it.
test("on a closed issue, leaving the parent's or the route's editor hands the focus to Development's heading", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();
  const heading = () => screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });

  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.set_parent }));
    await screen.findByLabelText(PROPERTY_LABELS.parent);
    drawIssue(getIssue, queryClient, closedIssue);
    await user.click(within(rowAt("parent")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      document.activeElement,
      "Cancel of the parent's editor on a closed issue did not give the focus to Development's heading"
    ).toBe(heading());

    drawIssue(getIssue, queryClient, issue);
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await screen.findByLabelText(PROPERTY_LABELS.route);
    drawIssue(getIssue, queryClient, closedIssue);
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      document.activeElement,
      "Cancel of the route's editor on a closed issue did not give the focus to Development's heading"
    ).toBe(heading());
  } finally {
    unmount();
    restore();
  }
});

// While the route's editor is open the row draws no control of its own, so a Retry pressed there,
// whose route then landed, left the focus on the page's body. The focus goes on to the editor's
// box. A close made in another session takes away a Retry the reader did not press, since a closed
// issue's line offers none: that is no press of the reader's, so the focus stays on the page's
// body, as for every change the reader did not make.
test("the refused route's Retry hands its focus to the editor's box while it is open, and a close made elsewhere that takes it away leaves its focus on the page's body", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  const refuse = () =>
    patchIssue.mockImplementationOnce(() =>
      Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
    );
  const { queryClient, unmount } = renderDetails();
  const retryOfRoute = () =>
    within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.retry });
  const refused = () =>
    waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );

  try {
    refuse();
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await refused();
    // The editor opened again over the refused route, which the line still says.
    await user.click(
      within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` })
    );
    const box = await screen.findByLabelText(PROPERTY_LABELS.route);
    act(() => retryOfRoute().focus());
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the refusal is still said after the route landed"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the route landed with its editor open, and the focus did not go to the editor's box"
    ).toBe(box);

    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    refuse();
    await user.click(screen.getByRole("button", { name: `Messages default to ${route}` }));
    await user.clear(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await refused();
    act(() => retryOfRoute().focus());
    drawIssue(getIssue, queryClient, { ...closedIssue, route });
    expect(
      within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the refusal of a route on a closed issue offers a Retry that sends nothing"
    ).toBeNull();
    await settleFocus();
    expect(
      document.activeElement,
      "the Retry went as the issue closed elsewhere, and the focus left the page's body though the reader pressed nothing"
    ).toBe(document.body);
  } finally {
    unmount();
    restore();
  }
});

// A close made elsewhere leaves the parent's link on the page, taking a press as before, so it
// takes nothing from a reader's focus on it, and nothing moves that focus.
test("a close made elsewhere leaves the focus on the parent's link, which still takes a press", async () => {
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, parent: "CORE-2" });
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const link = await screen.findByRole("link", { name: "CORE-2" });
    act(() => link.focus());
    drawIssue(getIssue, queryClient, { ...closedIssue, parent: "CORE-2" });
    await waitFor(() =>
      expect(
        within(rowAt("parent")).queryByRole("button", { name: CONTROL_LABELS.edit_parent }),
        "the close left Edit parent issue on the page"
      ).toBeNull()
    );
    expect(document.activeElement, "the close moved the focus off the parent's link").toBe(link);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A control busy with its own write refuses a press (`aria-disabled`) but keeps the focus, and a
// close made elsewhere takes nothing from it: the blocked-by editor stays open on a closed issue
// while its save is out, and the focus a reader had on its Save stays there.
test("a close made elsewhere leaves the focus on the blocked-by editor's Save while its save is out", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  // The save is held out for the whole case.
  patchIssue.mockImplementationOnce(() => new Promise(() => undefined));
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-3");
    const save = screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by });
    await user.click(save);
    await waitFor(() =>
      expect(save, "Save takes a press while its save is out").toHaveAttribute(
        "aria-disabled",
        "true"
      )
    );
    expect(document.activeElement, "the save moved the focus off Save").toBe(save);
    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Messages default to no route" }),
        "the close left the route's control enabled"
      ).toBeDisabled()
    );
    expect(document.activeElement, "the close moved the focus off the busy Save").toBe(save);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// The pins are read for their words only where there is a pin, so an issue with none costs no
// request of the questions, comments and messages.
test("with no pin, Pinned reads None and sends no request", async () => {
  const { pinReads, restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    expect(pins).toHaveTextContent(CONTROL_LABELS.none);
    expect(within(pins).queryAllByRole("link")).toHaveLength(0);
    // The panel's reads have gone out by the time the Repository row has read the project list.
    await waitFor(() => expect(api.listProjects).toHaveBeenCalled());
    expect(pinReads.asks, "Pinned read the issue's questions with no pin").not.toHaveBeenCalled();
    expect(
      pinReads.comments,
      "Pinned read the issue's comments with no pin"
    ).not.toHaveBeenCalled();
    expect(
      pinReads.messages,
      "Pinned read the issue's messages with no pin"
    ).not.toHaveBeenCalled();
  } finally {
    unmount();
    restore();
  }
});

// Pinned said None before the reader's state had answered, so a reader with pins read for a
// moment that they had none. Until that state answers the section draws its heading alone, and
// once it answers with no pin it says None.
test("Pinned says no None until the reader's state has answered, and None once it holds no pin", async () => {
  const { getMyState, restore } = stubIssueApi();
  const state = Promise.withResolvers<UserState>();
  getMyState.mockImplementation(() => state.promise);
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    await waitFor(() => expect(getMyState).toHaveBeenCalled());
    expect(pins, "Pinned says None before the reader's state has answered").not.toHaveTextContent(
      CONTROL_LABELS.none
    );
    await act(async () =>
      state.resolve({ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 } })
    );
    await waitFor(() =>
      expect(
        pins,
        "Pinned does not say None once the reader's state holds no pin"
      ).toHaveTextContent(CONTROL_LABELS.none)
    );
  } finally {
    unmount();
    restore();
  }
});

// The outline reads the questions, the comments and the messages under the keys Pinned reads
// them by, and keeps them fresh by events (`useIssueTree.ts`), so Pinned reads them with the same
// options: a window focus past the app's 30 s staleness reads none of them again. The reader's
// state keeps the app's defaults, so its second read shows the focus was handled.
test("with a pin, a window focus past 30 s reads none of Pinned's questions, comments and messages again", async () => {
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(
    pinnedState(conversationItemId("comment", "4a7c2e19-8b3d-4f60-a1e5-6c9d0b2f3e47"))
  );
  const client = appClient();
  const { unmount } = renderDetails(undefined, undefined, client);

  try {
    await waitFor(() => {
      for (const read of Object.values(pinReads)) expect(read).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    await focusPastStaleness(client);
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(2));
    expect(pinReads.asks, "a window focus read Pinned's questions again").toHaveBeenCalledTimes(1);
    expect(pinReads.comments, "a window focus read Pinned's comments again").toHaveBeenCalledTimes(
      1
    );
    expect(pinReads.messages, "a window focus read Pinned's messages again").toHaveBeenCalledTimes(
      1
    );
  } finally {
    unmount();
    restore();
    restoreWindowFocus();
  }
});

/** The reader's state with each conversation `id` (`conversationItemId`'s `<kind>:<id>`) of
 *  CORE-1 pinned, as `pins.ts` writes a pin into `dismissed`. */
function pinnedState(...ids: string[]): UserState {
  return {
    "CORE-1": { dismissed: ids.map(pinnedItemMarker), last_read_seq: 0, pinned: false, seq: 0 },
  };
}

/** A comment of CORE-1 as the comments read answers it, with the server's reading of its body
 *  where `tree` is given. */
function commentOf(id: string, body: string, tree?: DisplayTree): CommentWithTrees {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    ...(tree === undefined ? {} : { body_tree: tree }),
    created_at: "2026-09-09T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** A root message of CORE-1 as the messages read answers it, with what `more` sets over it: its
 *  author's corrections of it, where they made one. */
function messageOf(
  id: string,
  body: string,
  more: Partial<MessageWithTrees> = {}
): MessageWithTrees {
  return {
    author: { id: "alice", kind: "user" },
    body,
    broadcast_id: null,
    created_at: "2026-09-09T00:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    reply_block: null,
    target: null,
    ...more,
  };
}

// Until the read that holds a pin's words answers, the pin is drawn by the page's word for its
// kind, never by the id it was pinned under; once it answers, by its first line.
test("a pin whose words are not read yet is drawn by its kind's word, never its id", async () => {
  const id = "5b0f9e64-2c1d-4a7e-8f3b-91d2c6a4e870";
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(pinnedState(conversationItemId("comment", id)));
  const comments = Promise.withResolvers<CommentWithTrees[]>();
  pinReads.comments.mockImplementation(() => comments.promise);
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const link = await within(pins).findByRole("link");
    await waitFor(() => expect(pinReads.comments).toHaveBeenCalled());
    expect(
      link.textContent,
      "the pin is not drawn by its kind's word while its words are unread"
    ).toBe(NODE_TYPE_NAMES.comment);
    for (const part of id.split("-")) {
      expect(link.textContent, "the pin is drawn by its comment's id").not.toContain(part);
    }

    comments.resolve([commentOf(id, "Ship the fix tonight. Then watch the board.")]);
    await waitFor(() =>
      expect(
        within(pins).getByRole("link").textContent,
        "the pin is not drawn by its comment's first line once the comments are read"
      ).toBe("Ship the fix tonight.")
    );
  } finally {
    unmount();
    restore();
  }
});

// No read holds a handoff's words, so its pin is drawn by the page's word for it, and it opens
// the history, where the handoff is.
test("a pinned handoff is drawn by its kind's word and opens the history", async () => {
  const { getMyState, restore } = stubIssueApi();
  getMyState.mockResolvedValue(
    pinnedState(conversationItemId("handoff", "8e2d4c1a-7b3f-4e9d-a6c5-0f1b2d3e4a5c"))
  );
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const link = await within(pins).findByRole("link");
    expect(link.textContent).toBe(NODE_TYPE_NAMES.handoff);
    expect(link, "the handoff's pin does not open the history").toHaveAttribute(
      "href",
      "/issues/CORE-1/activity"
    );
  } finally {
    unmount();
    restore();
  }
});

// A pin's line is its record's words as its row in the outline reads them, from the server's
// reading of the text, so the symbols that format the body are not drawn as text.
test("a pinned comment written in markdown is drawn by its words, without the marks", async () => {
  const id = "3c9a7e15-6d2b-4f80-b1e4-7a5c9d0e2f36";
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(pinnedState(conversationItemId("comment", id)));
  const reading: DisplayTree = {
    content: [
      {
        content: [
          { marks: [{ type: "strong" }], text: "Bold", type: "text" },
          { text: " words. Then more.", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  pinReads.comments.mockResolvedValue([commentOf(id, "**Bold** words. Then more.", reading)]);
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    await waitFor(() =>
      expect(
        within(pins).getByRole("link").textContent,
        "the pin is not drawn by its comment's words"
      ).toBe("Bold words.")
    );
    expect(within(pins).getByRole("link").textContent).not.toContain("*");
  } finally {
    unmount();
    restore();
  }
});

/** When the pinned posts below were edited, and when deleted. */
const PIN_EDITED_AT = "2026-09-09T01:00:00Z";
const PIN_DELETED_AT = "2026-09-09T02:00:00Z";

/** A session nobody named, which the page calls by its word for an agent and never by its id. */
const UNNAMED_SESSION: Actor = { id: "0f8e2c4a-1111-4222-8333-444455556666", kind: "session" };

/** Pinned's row of the pin `id` (`conversationItemId`'s `<kind>:<id>`, or `eventItemId`'s). */
function pinRow(id: string): HTMLElement {
  const row = document.querySelector(`[data-pin="${id}"]`);
  if (!(row instanceof HTMLElement)) throw new Error(`Pinned draws no row for ${id}`);
  return row;
}

/** The link of Pinned's row of the pinned conversation `id`, to what it pins. */
function pinLink(id: string): HTMLElement {
  return within(pinRow(id)).getByRole("link");
}

// R2 of dispatch://INFRA-1523: a pinned comment or message its author edited is drawn by its first
// line, then ` · edited`, inside its link, as a parent line in the outline says it (`NodeRow.tsx`'s
// `ParentLink`). The mark is the page's own word, marked as its chrome.
test("a pinned comment and a pinned message their authors edited say edited after their lines", async () => {
  const comment = "6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d";
  const message = "7b2c3d4e-5f6a-4b7c-9d8e-0f1a2b3c4d5e";
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(
    pinnedState(conversationItemId("comment", comment), conversationItemId("message", message))
  );
  pinReads.comments.mockResolvedValue([
    {
      ...commentOf(comment, "Ship the fix tonight. Then watch the board."),
      edited_at: PIN_EDITED_AT,
    },
  ]);
  pinReads.messages.mockResolvedValue([
    {
      message: messageOf(message, "Hold the deploy. Until Friday.", { edited_at: PIN_EDITED_AT }),
      replies: [],
    },
  ]);
  const { unmount } = renderDetails();

  try {
    await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const mark = ` · ${CONTROL_LABELS.post_edited}`;
    for (const [kind, id, line] of [
      ["comment", comment, "Ship the fix tonight."],
      ["message", message, "Hold the deploy."],
    ] as const) {
      const pin = conversationItemId(kind, id);
      await waitFor(() =>
        expect(
          pinLink(pin).textContent,
          `the pinned ${kind} its author edited is not drawn by its line, then the edited mark`
        ).toBe(`${line}${mark}`)
      );
      expect(
        pinLink(pin).querySelector("[data-page-chrome]")?.textContent,
        `the pinned ${kind}'s edited mark is not the page's own word`
      ).toBe(mark);
    }
  } finally {
    unmount();
    restore();
  }
});

// R3 of dispatch://INFRA-1523, and INFRA-1196's decision 342: a pinned comment or message its
// author deleted is drawn as the deleted line, from the delete and never from its words, as its
// row in the outline draws it (`DeletedLine`), and stays a link to the post. Its actor is named as
// the page names an author: a session nobody named reads as the page's word for an agent, never
// by its id. The read and the cache both hold a deleted post's body empty; the words are kept
// here so the line is shown to come from the delete and never from them.
test("a pinned comment and a pinned message their authors deleted are drawn as the deleted line", async () => {
  const comment = "8c3d4e5f-6a7b-4c8d-ae9f-1a2b3c4d5e6f";
  const message = "9d4e5f6a-7b8c-4d9e-bf0a-2b3c4d5e6f7a";
  const deletion = { deleted_at: PIN_DELETED_AT, deleted_by: UNNAMED_SESSION };
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(
    pinnedState(conversationItemId("comment", comment), conversationItemId("message", message))
  );
  pinReads.comments.mockResolvedValue([
    { ...commentOf(comment, "Ship the fix tonight."), ...deletion },
  ]);
  pinReads.messages.mockResolvedValue([
    { message: messageOf(message, "Hold the deploy.", deletion), replies: [] },
  ]);
  const { unmount } = renderDetails();

  try {
    await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const opening = `${CONTROL_LABELS.post_deleted_by} ${CONTROL_LABELS.unnamed_author} · `;
    for (const [kind, id, words] of [
      ["comment", comment, "Ship the fix tonight."],
      ["message", message, "Hold the deploy."],
    ] as const) {
      const pin = conversationItemId(kind, id);
      await waitFor(() =>
        expect(
          pinLink(pin).querySelector("[data-post-deleted]")?.textContent?.slice(0, opening.length),
          `the pinned ${kind} its author deleted is not drawn as the deleted line, its actor named as the page names an author`
        ).toBe(opening)
      );
      const link = pinLink(pin);
      expect(
        link.textContent,
        `the pinned ${kind} its author deleted draws its words`
      ).not.toContain(words);
      expect(
        link.textContent,
        `the pinned ${kind}'s deleted line names its actor by its id`
      ).not.toContain(UNNAMED_SESSION.id.slice(0, 8));
      expect(link, `the pinned ${kind} its author deleted is no link to it`).toHaveAttribute(
        "href",
        buildIssuePath({ id, key: "CORE-1", kind })
      );
    }
  } finally {
    unmount();
    restore();
  }
});

/** CORE-1's `comment.created` event `id`, of a comment its author edited at `editedAt` (null for
 *  one nobody edited), as the history's read answers it: the read stamps the post's own marks on
 *  the event (`attachPostMarks` in `api/events.go`). */
function commentCreated(
  id: number,
  body: string,
  editedAt: string | null
): Extract<DispatchEvent, { type: "comment.created" }> {
  // A person, typed as the event's actor union reads one (its kind a literal).
  const alice = { id: "alice", kind: "user" } as const;
  return {
    actor: alice,
    created_at: "2026-09-09T00:00:00Z",
    id,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      answers: null,
      artifact_id: null,
      artifact_name: "",
      ask_id: null,
      author: alice,
      body,
      created_at: "2026-09-09T00:00:00Z",
      deleted_at: null,
      deleted_by: null,
      deliveries: [],
      edited_at: editedAt,
      id: `comment-${id}`,
      issue_key: "CORE-1",
      mentions: [],
      reply_block: null,
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: id,
    type: "comment.created",
  };
}

// R2 of dispatch://INFRA-1523: a pinned line of the history whose post its author edited since
// says ` · edited` after the line, as the margin's Pinned tab says it (`EventEditedMark`). The
// mark is the page's own word, marked as its chrome. A line whose post nobody edited says nothing
// of an edit.
test("a pinned history line of an edited post's event says edited after the line", async () => {
  const edited = commentCreated(21, "Hold the deploy.", PIN_EDITED_AT);
  const standing = commentCreated(22, "Ship on Friday.", null);
  const { getMyState, restore } = stubIssueApi();
  getMyState.mockResolvedValue(pinnedState(eventItemId(edited), eventItemId(standing)));
  const client = detailsQueryClient();
  // The history's log, which Pinned draws a pinned line from and never reads (`IssuePins.tsx`).
  client.setQueryData<InfiniteData<DispatchEvent[]>>(["events", "CORE-1"], {
    pageParams: [undefined],
    pages: [[edited, standing]],
  });
  const { unmount } = renderDetails(undefined, undefined, client);

  try {
    await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const mark = ` · ${CONTROL_LABELS.post_edited}`;
    await waitFor(() =>
      expect(
        pinRow(eventItemId(edited)).textContent,
        "the pinned line of an edited post's event does not say edited after the line"
      ).toContain(`Hold the deploy.${mark}`)
    );
    expect(
      [...pinRow(eventItemId(edited)).querySelectorAll("[data-page-chrome]")].map(
        (chrome) => chrome.textContent
      ),
      "the pinned line's edited mark is not the page's own word"
    ).toContain(mark);
    const line = pinRow(eventItemId(standing)).textContent;
    expect(line).toContain("Ship on Friday.");
    expect(line, "the pinned line of a post nobody edited says edited").not.toContain(mark);
  } finally {
    unmount();
    restore();
  }
});

// An Unpin takes its pin's row away, so the focus goes on to the next pin's link, else the one
// before it, else the Pinned heading, rather than falling to the page.
test("Unpin gives the focus to the next pin, else the one before, else the Pinned heading", async () => {
  const user = userEvent.setup();
  const { getMyState, pinReads, restore } = stubIssueApi();
  const [first, second, third] = [
    "1d4f0a6e-3b2c-4e8d-9a7f-5c6b1e2d3f40",
    "2e5a1b7f-4c3d-4f9e-8b6a-6d7c2f3e4a51",
    "3f6b2c8a-5d4e-4a0f-9c7b-7e8d3a4f5b62",
  ] as const;
  getMyState.mockResolvedValue({
    "CORE-1": {
      dismissed: [first, second, third].map((id) =>
        pinnedItemMarker(conversationItemId("comment", id))
      ),
      last_read_seq: 0,
      pinned: false,
      seq: 0,
    },
  });
  pinReads.comments.mockResolvedValue([
    commentOf(first, "First pin."),
    commentOf(second, "Second pin."),
    commentOf(third, "Third pin."),
  ]);
  // Each unpin is held out, so the page shows its own state of the pins throughout.
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(new Promise(() => undefined));
  const { unmount } = renderDetails();
  const unpin = async (id: string) => {
    const row = document.querySelector(`[data-pin="${conversationItemId("comment", id)}"]`);
    if (!(row instanceof HTMLElement)) throw new Error(`Pinned draws no row for ${id}`);
    act(() => within(row).getByRole("button", { name: CONTROL_LABELS.unpin }).focus());
    await user.keyboard("{Enter}");
  };

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    await within(pins).findByRole("link", { name: "Third pin." });

    await unpin(second);
    await waitFor(() => expect(within(pins).getAllByRole("link")).toHaveLength(2));
    expect(document.activeElement, "Unpin did not give the focus to the next pin").toBe(
      within(pins).getByRole("link", { name: "Third pin." })
    );

    await unpin(third);
    await waitFor(() => expect(within(pins).getAllByRole("link")).toHaveLength(1));
    expect(
      document.activeElement,
      "Unpin of the last pin did not give the focus to the one before it"
    ).toBe(within(pins).getByRole("link", { name: "First pin." }));

    await unpin(first);
    await waitFor(() => expect(pins).toHaveTextContent(CONTROL_LABELS.none));
    expect(
      document.activeElement,
      "Unpin of the only pin did not give the focus to the Pinned heading"
    ).toBe(within(pins).getByRole("heading", { name: CONTROL_LABELS.pinned }));
  } finally {
    unmount();
    putIssueState.mockRestore();
    restore();
  }
});

// The two comments of CORE-1 the table below pins, each drawn by its words.
const FIRST_PIN = "4b7d1e2f-6a3c-4d8e-9f0a-1b2c3d4e5f60";
const SECOND_PIN = "5c8e2f3a-7b4d-4e9f-8a1b-2c3d4e5f6a71";
const PIN_WORDS: Readonly<Record<string, string>> = {
  [FIRST_PIN]: "First pin.",
  [SECOND_PIN]: "Second pin.",
};

// A route to an agent's session, which the route's row draws inside the agent's name, and one to
// a role, which it draws as its address.
const SESSION_ROUTE = "session:7d9f3a4b-8c5e-4fa0-9b2c-3d4e5f6a7b82";
const ROLE_ROUTE = "role:legion-controller-core";

/** What Dispatch holds in a case of the table below: the issue (the fixture's where none is
 *  named), the comments the reader has pinned, and the issues that wait on CORE-1. */
interface Held {
  readonly issue?: IssueDetails;
  readonly pins?: readonly string[];
  readonly waiting?: readonly string[];
}

/** One case of the table below: what Dispatch holds before the change made elsewhere and what the
 *  change moves (`after`, over `before`), and the element the reader's focus is on as it lands,
 *  which the change takes off the page or disables. */
interface TakenAway {
  readonly after: Held;
  readonly before: Held;
  readonly focused: (user: UserEvent) => Promise<HTMLElement>;
}

/** The row of Details named `name`, once Details is drawn. */
async function drawnRow(name: string): Promise<HTMLElement> {
  await screen.findByText(name, { exact: true, selector: "[data-page-chrome]" });
  return detailsRow(name);
}

/** Details' panel, the element its sections and Pinned are drawn in. */
function detailsPanel(): HTMLElement {
  const panel = screen
    .getByRole("button", { name: MARGIN_SECTION_NAMES.development })
    .closest("section")?.parentElement;
  if (!(panel instanceof HTMLElement)) throw new Error("Details draws no panel");
  return panel;
}

/** The first pin's link, once Pinned has read its words. */
async function firstPinLink(): Promise<HTMLElement> {
  const section = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
  return within(section).findByRole("link", { name: PIN_WORDS[FIRST_PIN] });
}

/** Add, once it is drawn. */
function drawnAdd(): Promise<HTMLElement> {
  return waitFor(() => {
    const control = addMenuControl();
    if (!(control instanceof HTMLElement)) throw new Error("Add is not drawn");
    return control;
  });
}

/** Opens Add's menu, which puts the focus on its first entry, and answers the menu. */
async function openAddMenu(user: UserEvent): Promise<HTMLElement> {
  await user.click(await drawnAdd());
  return screen.findByRole("menu", { name: CONTROL_LABELS.add });
}

/** Add's open menu's entry for `property`. */
function addEntry(property: string): HTMLElement {
  return within(screen.getByRole("menu", { name: CONTROL_LABELS.add })).getByRole("menuitem", {
    name: property,
  });
}

/** The route's control, drawn inside the agent's name or on its own. */
function routeControl(): HTMLElement {
  return within(rowAt("route")).getByRole("button", { name: /^Messages default to / });
}

/** The fixture's issue advancing PRI-7, its own. */
const advancingOne: IssueDetails = { ...issue, advances: [{ id: "PRI-7", inherited: false }] };

/**
 * The elements of Details below, each of which a change made elsewhere takes off the page, or
 * disables, while it holds the focus, and which one case can drive through the query cache: a
 * pin, a blocker, a file or a priority taken away, a route moved, a claim made,
 * released or taken again, a list's Show more as the list falls to the 20 entries it draws, a
 * value set for an entry of Add's open menu or for its only one, the repository's None and its
 * text drawn in each other's place, the priority's line moved by another session's write, and
 * the issue closed while the focus is on a control of Development. None of these is a press of
 * the reader's.
 */
const TAKEN_AWAY: readonly (readonly [string, TakenAway])[] = [
  [
    "Pinned's link, its pin unpinned in another session",
    {
      after: { pins: [SECOND_PIN] },
      before: { pins: [FIRST_PIN, SECOND_PIN] },
      focused: firstPinLink,
    },
  ],
  [
    "Pinned's Unpin, its pin unpinned in another session",
    {
      after: { pins: [SECOND_PIN] },
      before: { pins: [FIRST_PIN, SECOND_PIN] },
      focused: async () => {
        await firstPinLink();
        const row = document.querySelector(
          `[data-pin="${conversationItemId("comment", FIRST_PIN)}"]`
        );
        if (!(row instanceof HTMLElement)) throw new Error("Pinned draws no row for the first pin");
        return within(row).getByRole("button", { name: CONTROL_LABELS.unpin });
      },
    },
  ],
  [
    "Pinned's last link, its pin unpinned in another session",
    {
      after: { pins: [] },
      before: { pins: [FIRST_PIN] },
      focused: firstPinLink,
    },
  ],
  [
    "Blocks' None, another issue starting to wait on this one",
    {
      after: { waiting: ["CORE-5"] },
      before: { waiting: [] },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.blocks)).findByText(CONTROL_LABELS.none),
    },
  ],
  [
    "a waiting issue's link in Blocks, that issue no longer waiting",
    {
      after: { waiting: ["CORE-6"] },
      before: { waiting: ["CORE-5", "CORE-6"] },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.blocks)).findByRole("link", { name: "CORE-5" }),
    },
  ],
  [
    "Blocks' Show more, the issues that wait falling to 20",
    {
      after: { waiting: keysFrom2(PAGE) },
      before: { waiting: keysFrom2(PAGE + 1) },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.blocks)).findByRole("button", { name: SHOW_MORE }),
    },
  ],
  [
    "Blocked by's link, that blocker taken away in another session",
    {
      after: { issue: { ...issue, blocked_by: ["CORE-3"] } },
      before: { issue: { ...issue, blocked_by: ["CORE-2", "CORE-3"] } },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.blocked_by)).findByRole("link", { name: "CORE-2" }),
    },
  ],
  [
    "Blocked by's last link, that blocker taken away in another session",
    {
      after: { issue: { ...issue, blocked_by: [] } },
      before: { issue: { ...issue, blocked_by: ["CORE-2"] } },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.blocked_by)).findByRole("link", { name: "CORE-2" }),
    },
  ],
  [
    "the parent's link, the parent cleared in another session",
    {
      after: { issue: { ...issue, parent: null } },
      before: { issue: { ...issue, parent: "CORE-2" } },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.parent)).findByRole("link", { name: "CORE-2" }),
    },
  ],
  [
    "the route's control, the route moved into an agent's name in another session",
    {
      after: { issue: { ...issue, route: SESSION_ROUTE } },
      before: { issue: { ...issue, route: null } },
      focused: () => screen.findByRole("button", { name: "Messages default to no route" }),
    },
  ],
  [
    "the route's control, the route moved out of an agent's name in another session",
    {
      after: { issue: { ...issue, route: ROLE_ROUTE } },
      before: { issue: { ...issue, route: SESSION_ROUTE } },
      focused: async () => {
        await drawnRow(PROPERTY_LABELS.route);
        return routeControl();
      },
    },
  ],
  [
    "the agent's live link in the route's row, the route moved to a role in another session",
    {
      after: { issue: { ...issue, route: ROLE_ROUTE } },
      before: { issue: { ...issue, route: SESSION_ROUTE } },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.route)).findByRole("link", {
          name: /^Open live conversation with /,
        }),
    },
  ],
  [
    "the claim row's None, a claim made in another session",
    {
      after: { issue: { ...issue, claim: sessionClaim } },
      before: {},
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.claim)).findByText(CONTROL_LABELS.none),
    },
  ],
  [
    "the claim's holder, the claim forced from the agent by a person",
    {
      after: {
        issue: {
          ...issue,
          claim: { actor: { id: "alice", kind: "user" }, at: "2026-09-24T08:00:00Z" },
        },
      },
      before: { issue: { ...issue, claim: sessionClaim } },
      focused: async () => {
        const [holder] = await within(await drawnRow(PROPERTY_LABELS.claim)).findAllByRole("link");
        if (holder === undefined) throw new Error("the claim's row draws no link to its holder");
        return holder;
      },
    },
  ],
  [
    "Release, the claim released in another session",
    {
      after: { issue: { ...issue, claim: null } },
      before: { issue: { ...issue, claim: sessionClaim } },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.claim)).findByRole("button", {
          name: "Release the claim on CORE-1",
        }),
    },
  ],
  [
    "the failed release's Retry, the claim taken again in another session",
    {
      after: {
        issue: {
          ...issue,
          claim: reviewerClaim,
        },
      },
      before: { issue: { ...issue, claim: sessionClaim } },
      focused: async (user) => {
        // The reader's release fails, so the row draws its line with Retry. The change sends no
        // release, so the spy goes once the line is drawn.
        const release = spyOn(api, "releaseIssueClaim").mockRejectedValueOnce(
          new Error("network error")
        );
        try {
          const row = await drawnRow(PROPERTY_LABELS.claim);
          await user.click(
            await within(row).findByRole("button", { name: "Release the claim on CORE-1" })
          );
          return await within(row).findByRole("button", { name: CONTROL_LABELS.retry });
        } finally {
          release.mockRestore();
        }
      },
    },
  ],
  [
    "a file's link, that file taken away",
    {
      after: { issue: withNotes(1) },
      before: { issue: withNotes(2) },
      focused: async () =>
        within(await screen.findByTestId("issue-files")).findByRole("link", {
          name: /^notes-02\.md/,
        }),
    },
  ],
  [
    "Files' Show more, the files falling to 20",
    {
      after: { issue: withNotes(PAGE - 1) },
      before: { issue: withNotes(PAGE) },
      focused: async () => {
        await screen.findByTestId("issue-files");
        return within(filesRow()).getByRole("button", { name: SHOW_MORE });
      },
    },
  ],
  [
    "a priority's link in Advances, that priority taken away in another session",
    {
      after: { issue: { ...issue, advances: [{ id: "PRI-8", inherited: false }] } },
      before: {
        issue: {
          ...issue,
          advances: [
            { id: "PRI-7", inherited: false },
            { id: "PRI-8", inherited: false },
          ],
        },
      },
      focused: async () =>
        within(await screen.findByTestId("issue-advances")).findByRole("link", { name: "PRI-7" }),
    },
  ],
  [
    "the add control beside Add, the first priority added in another session",
    {
      after: { issue: advancingOne },
      before: {},
      focused: () => screen.findByRole("button", { name: addPriorityControlName("CORE-1") }),
    },
  ],
  [
    "the Advances row's picker trigger, the last priority taken away in another session",
    {
      after: { issue },
      before: { issue: advancingOne },
      focused: async () =>
        within(await screen.findByTestId("issue-advances")).findByRole("button", {
          name: priorityTriggerName("CORE-1", false),
        }),
    },
  ],
  [
    "Add's menu entry, its property set in another session",
    {
      after: { issue: { ...issue, parent: "CORE-2" } },
      before: {},
      focused: async (user) => {
        await openAddMenu(user);
        return addEntry(PROPERTY_LABELS.parent);
      },
    },
  ],
  [
    "Add's bottom menu entry, its property set in another session while Add stays",
    {
      after: { issue: advancingOne },
      before: {},
      focused: async (user) => {
        await openAddMenu(user);
        return addEntry(PROPERTY_LABELS.advances);
      },
    },
  ],
  [
    "Add's menu entry, with Add itself, its only property set in another session",
    {
      after: { issue: { ...orphan, parent: "CORE-2" } },
      before: { issue: orphan },
      focused: async (user) => {
        await openAddMenu(user);
        return addEntry(PROPERTY_LABELS.parent);
      },
    },
  ],
  [
    "the repository's None, the repository set in another session",
    {
      after: { issue: { ...issue, repo: "acme/web", resolved_repo: "acme/web" } },
      before: {},
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.repository)).findByRole("button", {
          name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
        }),
    },
  ],
  [
    "the repository's text, the repository cleared in another session",
    {
      after: { issue },
      before: { issue: { ...issue, repo: "acme/web", resolved_repo: "acme/web" } },
      focused: async () =>
        within(await drawnRow(PROPERTY_LABELS.repository)).findByRole("button", {
          name: repoTextControlName("CORE-1", {
            resolvedRepo: "acme/web",
            words: CONTROL_NAME_WORDS,
          }),
        }),
    },
  ],
  [
    "Add's menu entry, the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: async (user) => {
        await openAddMenu(user);
        return addEntry(PROPERTY_LABELS.parent);
      },
    },
  ],
  [
    "Add, the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: drawnAdd,
    },
  ],
  [
    "Set parent issue, the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: () => screen.findByRole("button", { name: CONTROL_LABELS.set_parent }),
    },
  ],
  [
    "Blocked by's control, the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: () => screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) }),
    },
  ],
  [
    "the route's control, disabled by the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: () => screen.findByRole("button", { name: "Messages default to no route" }),
    },
  ],
  [
    "the labels' control, disabled by the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: () =>
        screen.findByRole("button", {
          name: labelsControlName("CORE-1", {
            labels: [],
            saving: false,
            words: CONTROL_NAME_WORDS,
          }),
        }),
    },
  ],
  [
    "the add control beside Add, disabled by the issue closed in another session",
    {
      after: { issue: closedIssue },
      before: {},
      focused: () => screen.findByRole("button", { name: addPriorityControlName("CORE-1") }),
    },
  ],
];

// A change the reader did not make - another session's write, the read it sets off, a close made
// elsewhere - takes away the element of Details that held the focus. Details keeps the rule of
// every page of Dispatch: the focus stays on the page's body, where the browser put it, and no
// element of Details takes it, then or two frames and a task later, as a keeper that waits would.
// Each change here comes through the query cache, as the reads of such a change land; a control a
// close disables drops its focus to the body as Chromium drops it (`emulateChromiumFocusFixup`).
test.each(
  TAKEN_AWAY
)("%s: the focus stays on the page's body, and no element of Details takes it", async (_name, {
  after,
  before,
  focused,
}) => {
  const user = userEvent.setup();
  const { getIssue, getMyState, pinReads, restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences");
  // The Advances row reads the priorities, to name the one the issue advances.
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  pinReads.comments.mockResolvedValue(
    Object.entries(PIN_WORDS).map(([id, words]) => commentOf(id, words))
  );
  // Makes `held` what Dispatch answers and, once Details is drawn, what its reads hold.
  const hold = (held: Held, queryClient?: QueryClient) => {
    const details = held.issue ?? issue;
    const state = pinnedState(...(held.pins ?? []).map((id) => conversationItemId("comment", id)));
    const blocking: GraphReferences = {
      edges: (held.waiting ?? []).map((key) => waitingEdge(key, `Waits for ${key}`)),
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    };
    getIssue.mockResolvedValue(details);
    getMyState.mockResolvedValue(state);
    getReferences.mockResolvedValue(blocking);
    if (queryClient === undefined) return;
    act(() => {
      queryClient.setQueryData(issueQuery("CORE-1").queryKey, details);
      queryClient.setQueryData(userStateQuery().queryKey, state);
      queryClient.setQueryData(["references", "to", reference, "blocked_by"], blocking);
    });
  };
  hold(before);
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const lost = await focused(user);
    act(() => lost.focus());
    expect(document.activeElement, "the case's element did not take the focus").toBe(lost);

    hold({ ...before, ...after }, queryClient);
    await waitFor(() =>
      expect(
        lost.isConnected && !lost.matches(":disabled"),
        "the change left the focused element on the page, taking a press"
      ).toBe(false)
    );
    expect(
      document.activeElement,
      "the element went, and the focus is not on the page's body"
    ).toBe(document.body);
    await settleFocus();
    expect(
      document.activeElement,
      "the focus left the page's body after the element went, though the reader pressed nothing"
    ).toBe(document.body);
    expect(
      detailsPanel().contains(document.activeElement),
      "an element of Details took the focus"
    ).toBe(false);
  } finally {
    stopBlurring();
    unmount();
    priorities.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});
