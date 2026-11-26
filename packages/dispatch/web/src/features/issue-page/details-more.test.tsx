import { afterEach, beforeEach, expect, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect, useState } from "react";
import { MemoryRouter, type NavigateFunction, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { nextFrame } from "../../__tests__/frames";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { ApiError, api } from "../../api/client";
import { prependEventToLog } from "../../api/event-feed";
import {
  issueCoverageQuery,
  issueResultsQuery,
  issueStepsQuery,
  userStateQuery,
} from "../../api/queries";
import type {
  AskRead,
  AskThread,
  Comment,
  CommentDelivery,
  DispatchEvent,
  IssueAskRow,
  IssueCoverage,
  IssueDetails,
  MessageDelivery,
  MessageRead,
  Subscriber,
  UserState,
} from "../../api/types";
import { answeredWithErrorGuidance, queuedWait } from "../conversation/delivery";
import { conversationItemId, pinnedItemMarker } from "../issue/pins";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { issueDetails } from "./decisions-filter/decisions-fixtures";
import { DetailsFocusedNode, DetailsHistory, DetailsPeople } from "./details-more";
import { DETAILS_ROW_ATTRIBUTE } from "./details-rows";
import { type FocusedNode, FocusedNodeProvider, useSetFocusedNode } from "./focused-node";
import { DETAILS_ATTRIBUTE, type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";
import { PROPERTY_LABELS } from "./page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DELIVERY_STATE_WORDS,
  issueStatusWord,
  NODE_REFERENCES_WORDS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  STATE_LINES,
  sentenceCase,
} from "./page-words";
import { failRefreshOverHeld } from "./refresh-over-held-fixture";
import { PAGE_SIZE, SHOW_MORE, showMoreName } from "./show-more-fixture";
import { renderAtBrowserEntry } from "./slots/stage-fixtures";
import { commentNode as commentTreeNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import { issueTreeQuery, outlineQuery, type TreeAnswer } from "./tree-read/tree-read";
import { issueAskThreadsQuery } from "./useIssueTree";

const KEY = "CORE-1";

// These cases date the events, messages and delivery attempts Details reads some seconds, a
// minute or days before now, and Details reads them against the clock: its history draws every
// day before the reader's today folded, and an attempt keeps its Retry only for a while after it
// was made. Each case runs at one fixed midday, so none reads the wall clock: one started just
// after midnight would put its history's entries on yesterday. `setSystemTime` holds `Date`
// alone; the timers the cases wait on (React Query's, `waitFor`'s) run on the real clock.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

// Details' Steps reads the issue's step records each time Details opens, whatever the cache
// holds, through the Steps tab's catch-up (`DetailsSteps.tsx`): each case answers that read with
// the same no runs `readsHeld` holds, so it reaches no network and draws nothing new.
let stepsRead: { mockRestore(): void } | undefined;
beforeEach(() => {
  stepsRead = spyOn(api, "listSteps").mockResolvedValue({ issue: KEY, tasks: [] });
});
afterEach(() => stepsRead?.mockRestore());

const issue = {
  artifacts: [],
  key: KEY,
  status: "in_progress",
  title: "Review the spec",
} as unknown as IssueDetails;

/** The reader's own comment, which heads a thread and was delivered to nobody. */
const OWN_COMMENT = {
  ask_id: null,
  author: { id: "alice", kind: "user" },
  body: "The reader's own words.",
  created_at: "2026-11-15T10:00:00Z",
  deliveries: [],
  id: "comment-1",
  reply_to: null,
  resolved: false,
  suggestion: null,
} as unknown as Comment;

/** The tree read's answer for an issue that holds `comments` and nothing else, as
 *  `outline-reads.test.tsx` answers it: the issue, and each comment a node under the comment it
 *  replies to, else under the issue. */
function treeOf(comments: readonly Comment[]): TreeAnswer {
  return treeAnswer(
    [treeNode(KEY, "issue", null), ...comments.map((comment) => commentTreeNode(comment))],
    [],
    { issue: KEY }
  );
}

/** Holds `comments` as the issue's comments in `client`, each read with `options`, and the tree
 *  read's answer that holds them (`treeOf`), under the tree read's key and under Conversation's
 *  (`outlineQuery(KEY, "conversation")`), which the outline reads at the issue's address. The
 *  outline lands an address by the tree read too (`useOutlineReads`), so the client holds it as
 *  it holds the outline's other reads, and it holds the comments the client holds: a case that
 *  sets other comments sets their tree with them. */
function holdComments(
  client: QueryClient,
  comments: readonly Comment[],
  options: { readonly updatedAt?: number } = {}
): void {
  client.setQueryData(["comments", KEY], comments, options);
  client.setQueryData(issueTreeQuery(KEY).queryKey, treeOf(comments), options);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeOf(comments), options);
}

/** A client holding every read the facts and the writer make, the issue's asks both as the
 *  history reads them (`["asks", KEY]`) and with their threads as the outline and the writer read
 *  them (`issueAskThreadsQuery`), and the tree reads the outline lands an address by
 *  (`holdComments`), so the drawing sends none. */
function readsHeld(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["agents"], []);
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  holdComments(client, [OWN_COMMENT]);
  client.setQueryData(["issue-messages", KEY], []);
  client.setQueryData(["whoami"], { login: "alice" });
  // Details' Steps draws the issue's step records held here before its own read answers.
  client.setQueryData(issueStepsQuery(KEY).queryKey, { issue: KEY, tasks: [] });
  return client;
}

/** Gives `node` the reader's focus, as the outline does when a row takes it. */
function Focus({ node }: { node: FocusedNode }): ReactNode {
  const setFocused = useSetFocusedNode();
  useEffect(() => setFocused(node), [node, setFocused]);
  return null;
}

// Until the outline gives a row the focus, and whenever the reader
// is on no row, the facts are the issue's own - its kind, its title and its status - and the
// issue draws no list of what references it, because Details' "Referenced by" row holds it.
test("with no node focused, the facts are the issue's", () => {
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={readsHeld()}>
        <FocusedNodeProvider>
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const facts = screen.getByTestId("focused-node-facts");
    expect(facts.getAttribute("data-focused-node")).toBe(KEY);
    expect(
      within(facts).getByRole("heading", { name: PROPERTY_LABELS.focused_node })
    ).toBeDefined();
    const text = facts.textContent ?? "";
    expect(text).toContain(`${sentenceCase(NODE_TYPE_NAMES.issue)}:`);
    expect(text).toContain(issue.title);
    expect(text).toContain(issueStatusWord("in_progress"));
    expect(
      within(facts).queryByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
    ).toBeNull();
  } finally {
    view.unmount();
  }
});

// Edit on the reader's own comment is drawn only under the page's writer, and sets it to edit
// that comment and its words; the page draws no box that saves it yet.
test("Edit on the reader's own comment sets the writer to edit it", async () => {
  let writer: OutlineWriter | undefined;
  function Probe(): ReactNode {
    writer = useOutlineWriter();
    return null;
  }
  const comment: FocusedNode = {
    author: OWN_COMMENT.author,
    id: "comment-1",
    item: { id: "comment-1", kind: "comment" },
    reference: `dispatch://${KEY}/comment/comment-1`,
    text: OWN_COMMENT.body,
    type: "comment",
  };
  const facts = (
    <FocusedNodeProvider>
      <Focus node={comment} />
      <DetailsFocusedNode issue={issue} state={undefined} />
    </FocusedNodeProvider>
  );
  // With no writer above them, the facts draw no Edit.
  const alone = render(
    <MemoryRouter>
      <QueryClientProvider client={readsHeld()}>{facts}</QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(screen.getByTestId("focused-node-facts").getAttribute("data-focused-node")).toBe(
        "comment-1"
      )
    );
    expect(screen.queryByRole("button", { name: CONTROL_LABELS.edit })).toBeNull();
  } finally {
    alone.unmount();
  }

  const client = readsHeld();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <OutlineWriterProvider issueKey={KEY}>
          <Probe />
          {facts}
        </OutlineWriterProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const edit = await screen.findByRole("button", { name: CONTROL_LABELS.edit });
    expect(writer?.editing, "the writer edited something before Edit").toBeUndefined();

    await userEvent.click(edit);

    await waitFor(() =>
      expect(writer?.editing).toEqual({ body: OWN_COMMENT.body, id: "comment-1" })
    );
    expect(writer?.reply).toBeNull();

    // The reader's own comment is told by the page's one rule (`isReader`), as the outline's
    // row tells it: their login in any casing, and never words an agent wrote for them, which
    // the row calls the agent's.
    act(() => {
      holdComments(client, [{ ...OWN_COMMENT, author: { id: "Alice", kind: "user" } }]);
    });
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.edit }),
      "the reader's own comment under their login in another casing draws no Edit"
    ).not.toBeNull();
    act(() => {
      holdComments(client, [
        { ...OWN_COMMENT, author: { ...OWN_COMMENT.author, act: { session: "planner" } } },
      ]);
    });
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.edit }),
      "an agent's comment for the reader draws Edit"
    ).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The reader's state of the issue, read through `seq`, with nothing pinned. */
function readTo(seq: number): UserState {
  return { [KEY]: { dismissed: [], last_read_seq: seq, pinned: false, seq: 0 } };
}

/** An agent subscribed to the issue's own topic, so Unsubscribe can take it off. */
const SUBSCRIBER: Subscriber = {
  last_seen: Date.parse("2026-11-15T10:00:00Z"),
  live: false,
  removable: true,
  session_id: "planner",
  title: "Planner",
  topics: [`dispatch.issue.${KEY}`],
};

/** The People rows of Details, at the issue's own address, over the reads `readsHeld` holds:
 *  the subscribers' read is each case's own. */
function renderPeople() {
  const client = readsHeld();
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${KEY}`]}>
      <QueryClientProvider client={client}>
        <DetailsPeople issue={issue} state={undefined} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { client, unmount: view.unmount };
}

/** The Subscribers row's control, which carries the count once the read answers. */
const SUBSCRIBERS = new RegExp(`^${PROPERTY_LABELS.subscribers}`, "u");

/** Fails a refresh of the subscribers over the answer the People rows hold, and then their first
 *  read, each with the listener's refusal (`failRefreshOverHeld`). */
function failRefreshThenFirstRead(
  client: QueryClient,
  getIssueSubscribers: Mock<typeof api.getIssueSubscribers>,
  held: string,
  stillDrawn: () => void
): Promise<void> {
  const key = ["subscribers", KEY];
  return failRefreshOverHeld({
    client,
    failNextFetch: () =>
      getIssueSubscribers.mockRejectedValueOnce(
        new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" })
      ),
    held,
    queryKey: key,
    refresh: () => client.invalidateQueries({ queryKey: key }),
    stillDrawn,
  });
}

// The subscribers' read, which counts them on every load, says so
// where it failed, with no count it cannot make, and its Retry reads them again.
test("a failed read of the subscribed agents says so, with Retry", async () => {
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers")
    .mockRejectedValueOnce(
      new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" })
    )
    .mockResolvedValue([SUBSCRIBER]);
  const view = renderPeople();
  try {
    expect((await screen.findByRole("alert")).textContent).toBe(STATE_LINES.subscribers_unreadable);
    const subscribers = screen.getByRole("button", { name: SUBSCRIBERS });
    expect(subscribers.textContent).toBe(PROPERTY_LABELS.subscribers);

    await userEvent.click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));

    await waitFor(() => expect(getIssueSubscribers).toHaveBeenCalledTimes(2));
    expect(getIssueSubscribers).toHaveBeenLastCalledWith(KEY);
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    expect(screen.queryByRole("alert")).toBeNull();
    // Retry went with its line once the read landed, and the focus with it: it is on the control
    // of the list the line was about, not on the page.
    expect(document.activeElement, "the focus fell to the page with Retry").toBe(subscribers);

    // A read that fails once the row holds an answer keeps what it holds - the count and the
    // agents - and says no failure beside them: neither a line nor a Retry.
    await userEvent.click(subscribers);
    await screen.findByRole("button", { name: "Unsubscribe" });
    getIssueSubscribers.mockRejectedValueOnce(
      new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" })
    );
    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
    expect(
      view.client.getQueryState(["subscribers", KEY])?.status,
      "the refresh did not fail"
    ).toBe("error");
    expect(
      screen.queryByRole("alert"),
      "a failed refetch said a failure over the agents it held"
    ).toBeNull();
    expect(subscribers.textContent, "a failed refetch dropped the count it held").toBe(
      `${PROPERTY_LABELS.subscribers}:1`
    );
    expect(
      screen.queryByRole("button", { name: "Unsubscribe" }),
      "a failed refetch dropped the agents it held"
    ).not.toBeNull();
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
      "a failed refetch offered a Retry over the agents it held"
    ).toBeNull();
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// Unsubscribe asks for Confirm first; a write Dispatch refused says
// so, and its Retry sends the same session's unsubscribe again.
test("a failed Unsubscribe says so, with Retry", async () => {
  let subscribed = [SUBSCRIBER];
  // The write Retry sends is held until the case lets it land, so that where the focus is while
  // it is out can be read.
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockImplementation(
    async () => subscribed
  );
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession")
    .mockRejectedValueOnce(new ApiError(403, { code: "FORBIDDEN", error: "not yours to remove" }))
    .mockImplementation(async () => {
      await landed;
      subscribed = [];
    });
  const view = renderPeople();
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    await userEvent.click(subscribers);
    await userEvent.click(screen.getByRole("button", { name: "Unsubscribe" }));
    expect(unsubscribeIssueSession, "Unsubscribe wrote before Confirm").not.toHaveBeenCalled();
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" })
    );

    expect((await screen.findByRole("alert")).textContent).toBe(ANSWER_LINES.unsubscribe_failed);
    expect(unsubscribeIssueSession).toHaveBeenCalledTimes(1);
    expect(unsubscribeIssueSession).toHaveBeenLastCalledWith(KEY, "planner", keyedAttempt);

    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    await userEvent.click(retry);

    await waitFor(() => expect(unsubscribeIssueSession).toHaveBeenCalledTimes(2));
    expect(unsubscribeIssueSession).toHaveBeenLastCalledWith(KEY, "planner", keyedAttempt);
    // Retry stays with its line while its write is out, taking no press but never disabled, and
    // keeps the focus the press gave it.
    await waitFor(() =>
      expect(retry, "Retry takes a press while the write it sent again is out").toHaveAttribute(
        "aria-disabled",
        "true"
      )
    );
    expect(retry, "Retry turned disabled while the write it sent again is out").toBeEnabled();
    expect(
      document.activeElement,
      "Retry lost the focus while the write it sent again is out"
    ).toBe(retry);

    land();
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:0`));
    expect(screen.queryByRole("alert")).toBeNull();
    // Retry went with its line once the write landed, and the agent's row with its Unsubscribe
    // once the agents were read again; no agent follows it, so the focus is on the Subscribers
    // control rather than on the page.
    expect(document.activeElement, "the focus fell to the page with Retry").toBe(subscribers);
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
    unsubscribeIssueSession.mockRestore();
  }
});

// Unsubscribe's Retry sends a write again, but it read busy while that write was out, as a read's
// Retry does, whose press joins the read that is out: a write has nothing to join. It takes no
// press then, as every write's Retry of Details, and a press made meanwhile sends nothing.
test("Unsubscribe's Retry takes no press while the write it sent again is out", async () => {
  let subscribed = [SUBSCRIBER];
  // The write Retry sends is held until the case lets it land.
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockImplementation(
    async () => subscribed
  );
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession")
    .mockRejectedValueOnce(new ApiError(403, { code: "FORBIDDEN", error: "not yours to remove" }))
    .mockImplementation(async () => {
      await landed;
      subscribed = [];
    });
  const view = renderPeople();
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    await userEvent.click(subscribers);
    await userEvent.click(screen.getByRole("button", { name: "Unsubscribe" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" })
    );
    expect((await screen.findByRole("alert")).textContent).toBe(ANSWER_LINES.unsubscribe_failed);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });

    await userEvent.click(retry);
    await waitFor(() => expect(unsubscribeIssueSession).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(
        retry,
        "Unsubscribe's Retry takes a press while the write it sent again is out"
      ).toHaveAttribute("aria-disabled", "true")
    );
    expect(
      retry,
      "Unsubscribe's Retry reads busy while its write is out, as a read's Retry does"
    ).not.toHaveAttribute("aria-busy");
    await userEvent.click(retry);
    expect(
      unsubscribeIssueSession,
      "a press of Retry while its write is out sent the write again"
    ).toHaveBeenCalledTimes(2);

    land();
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:0`));
    expect(unsubscribeIssueSession).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
    unsubscribeIssueSession.mockRestore();
  }
});

// A refresh that fails over the agents held draws no line, and the agents stay drawn. A first
// read of them that failed keeps its line and its Retry drawn while that Retry reads them again,
// so the Retry is never disabled then: Chromium would drop its focus to the page
// (`chromium-focus-fixup.ts`), and a read that failed again would leave it there.
test("a refresh that fails over the subscribed agents held draws no line, and their Retry pressed by keyboard keeps the focus when its read fails again", async () => {
  const refusal = () =>
    new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" });
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([SUBSCRIBER]);
  const restoreFocus = emulateChromiumFocusFixup();
  const view = renderPeople();
  const key = ["subscribers", KEY];
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    await userEvent.click(subscribers);
    await screen.findByRole("button", { name: "Unsubscribe" });
    await failRefreshThenFirstRead(view.client, getIssueSubscribers, "the agents", () =>
      expect(screen.queryByRole("button", { name: "Unsubscribe" })).not.toBeNull()
    );
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<Subscriber[]>();
    getIssueSubscribers.mockImplementationOnce(() => again.promise);

    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(getIssueSubscribers).toHaveBeenCalledTimes(4));
    expect(
      document.activeElement,
      "the subscribers' Retry left the focus on the page while it read"
    ).toBe(retry);

    again.reject(refusal());
    await waitFor(() => expect(view.client.getQueryState(key)?.fetchStatus).toBe("idle"));
    expect(screen.getByRole("alert").textContent).toBe(STATE_LINES.subscribers_unreadable);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBe(retry);
    expect(
      document.activeElement,
      "the subscribers' Retry whose read failed again left the focus on the page"
    ).toBe(retry);
  } finally {
    restoreFocus();
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// A second press of the subscribers' Retry made before React draws the read the first one sent
// finds the render's `isFetching` still false. A press while the read is out joins it, and sends
// no other read. A refresh that fails over the agents held draws no line, so the Retry is a first
// read's that failed.
test("two presses of the subscribers' Retry before a render send one read", async () => {
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([SUBSCRIBER]);
  const view = renderPeople();
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    await failRefreshThenFirstRead(view.client, getIssueSubscribers, "the count", () =>
      expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`)
    );
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // Every read sent from here is held out until the case lets it land.
    const again = Promise.withResolvers<Subscriber[]>();
    getIssueSubscribers.mockImplementation(() => again.promise);

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      retry.click();
      retry.click();
    });
    expect(
      getIssueSubscribers,
      "the second press ended the read the first one sent and sent another"
    ).toHaveBeenCalledTimes(4);
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await act(async () => again.resolve([SUBSCRIBER]));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(getIssueSubscribers).toHaveBeenCalledTimes(4);
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// A first read of the subscribers that failed holds no answer, and a fetch of such a read clears
// its failure as it starts, so the press of its Retry took the line, and the Retry with it, away
// while the read it sent was out: the focus of a reader who pressed Retry by keyboard went with
// it. The line stays, its Retry busy and holding the focus, through a read that fails again, and
// once a read answers the focus goes to the Subscribers control.
test("a first read of the subscribers that failed keeps its line and its Retry, with the focus, while Retry reads again", async () => {
  const refusal = () =>
    new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" });
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers")
    .mockRejectedValueOnce(refusal())
    .mockResolvedValue([SUBSCRIBER]);
  const restoreFocus = emulateChromiumFocusFixup();
  const view = renderPeople();
  try {
    const failed = await screen.findByRole("alert");
    expect(failed.textContent).toBe(STATE_LINES.subscribers_unreadable);
    const subscribers = screen.getByRole("button", { name: SUBSCRIBERS });
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<Subscriber[]>();
    getIssueSubscribers.mockImplementationOnce(() => again.promise);

    act(() => retry.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(getIssueSubscribers).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        retry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      );
    });
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      retry
    );

    await act(async () => again.reject(refusal()));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain.textContent).toBe(STATE_LINES.subscribers_unreadable);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);

    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(
      document.activeElement,
      "the agents answered, and the focus fell to the page rather than to the Subscribers control"
    ).toBe(subscribers);
  } finally {
    restoreFocus();
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// An agent's Unsubscribe goes with its row once the agents are read again. Where the reader
// pressed it and confirmed, the focus goes on to the next agent's Unsubscribe; where another
// session unsubscribed the agent, no press of the reader's took the Unsubscribe away, and the focus
// it held stays on the page's body, as on every page of Dispatch.
test("an Unsubscribe that goes with its agent hands the focus on only where the reader pressed it", async () => {
  const tester: Subscriber = { ...SUBSCRIBER, session_id: "tester", title: "Tester" };
  const reviewer: Subscriber = { ...SUBSCRIBER, session_id: "reviewer", title: "Reviewer" };
  let subscribed = [SUBSCRIBER, tester, reviewer];
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockImplementation(
    async () => subscribed
  );
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession").mockImplementation(
    async (_key, session) => {
      subscribed = subscribed.filter((one) => one.session_id !== session);
    }
  );
  const view = renderPeople();
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:3`));
    await userEvent.click(subscribers);
    const [plannerOff] = await screen.findAllByRole("button", { name: "Unsubscribe" });
    act(() => plannerOff?.focus());

    // Another session unsubscribes the planner.
    subscribed = [tester, reviewer];
    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:2`));
    expect(plannerOff?.isConnected, "the planner's Unsubscribe stayed").toBe(false);
    expect(
      document.activeElement,
      "another session unsubscribed the agent whose Unsubscribe held the focus, and the focus left the page's body"
    ).toBe(document.body);

    const [testerOff, reviewerOff] = screen.getAllByRole("button", { name: "Unsubscribe" });
    if (testerOff === undefined) throw new Error("the tester's Unsubscribe is not drawn");
    await userEvent.click(testerOff);
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" })
    );
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    expect(unsubscribeIssueSession).toHaveBeenLastCalledWith(KEY, "tester", keyedAttempt);
    expect(
      document.activeElement,
      "the reader's own Unsubscribe went with its agent, and the focus did not go to the next agent's"
    ).toBe(reviewerOff);
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
    unsubscribeIssueSession.mockRestore();
  }
});

/** `count` agents subscribed to the issue, numbered from `from` + 1. */
function subscribedAgents(count: number, from = 0): Subscriber[] {
  return Array.from({ length: count }, (_, index) => ({
    ...SUBSCRIBER,
    session_id: `agent-${from + index + 1}`,
    title: `Agent ${from + index + 1}`,
  }));
}

// The subscribed agents are a list of unbounded length, so the list draws its first 20 and, under
// them, Show more, which says how many are left. A keyboard press draws the next 20 and moves the
// focus to the first agent it drew. An agent that subscribes after them joins the list's pages and
// its count, and the control goes once every agent is drawn.
test("the subscribed agents draw 20 at a time under Show more, and an agent read after them joins its pages and its count", async () => {
  let subscribed = subscribedAgents(45);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockImplementation(
    async () => subscribed
  );
  const view = renderPeople();
  const agents = () =>
    within(screen.getByRole("region", { name: "Subscribed agents" })).getAllByRole("listitem");
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:45`));
    await userEvent.click(subscribers);
    expect(agents(), "the list does not draw its first 20 agents").toHaveLength(20);
    const more = screen.getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(25));

    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(agents(), "Show more did not draw the next 20 agents").toHaveLength(40);
    expect(more.textContent).toBe(showMoreName(5));
    expect(
      agents()[20]?.contains(document.activeElement),
      "Show more did not move the focus to the first agent it drew"
    ).toBe(true);

    subscribed = [...subscribed, ...subscribedAgents(1, subscribed.length)];
    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:46`));
    expect(more.textContent, "an agent read after the list's own is not in its count").toBe(
      showMoreName(6)
    );

    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(agents(), "Show more did not draw the agent read after the list's own").toHaveLength(46);
    expect(more.isConnected, "Show more stayed once every agent was drawn").toBe(false);
    expect(agents()[40]?.contains(document.activeElement)).toBe(true);
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// The list draws its first 20 agents. Unsubscribe on the 20th, the last agent drawn, pressed and
// confirmed, takes its row away once its write lands and the agents are read again, and the 21st
// agent, which no row drew, is drawn in its place: the focus goes to that agent's Unsubscribe, at
// the place of the row that went, and not to the Subscribers control. A choice made among the
// controls drawn before the write found no Unsubscribe after the 20th.
test("Unsubscribe of the last agent drawn gives the focus to the Unsubscribe of the agent drawn in its place", async () => {
  let subscribed = subscribedAgents(PAGE_SIZE + 5);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockImplementation(
    async () => subscribed
  );
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession").mockImplementation(
    async (_key, session) => {
      subscribed = subscribed.filter((one) => one.session_id !== session);
    }
  );
  const view = renderPeople();
  const agents = () =>
    within(screen.getByRole("region", { name: "Subscribed agents" })).getAllByRole("listitem");
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:25`));
    await userEvent.click(subscribers);
    expect(agents(), "the list does not draw its first 20 agents").toHaveLength(PAGE_SIZE);
    const last = agents()[PAGE_SIZE - 1];
    if (last === undefined) throw new Error("the list draws no 20th agent");
    await userEvent.click(within(last).getByRole("button", { name: "Unsubscribe" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" })
    );

    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:24`));
    expect(unsubscribeIssueSession).toHaveBeenLastCalledWith(KEY, "agent-20", keyedAttempt);
    expect(last.isConnected, "the 20th agent's row stayed").toBe(false);
    expect(agents(), "the list does not draw 20 agents after the Unsubscribe").toHaveLength(
      PAGE_SIZE
    );
    const inItsPlace = agents()[PAGE_SIZE - 1];
    expect(inItsPlace?.textContent, "the 21st agent is not drawn 20th").toContain("Agent 21");
    expect(
      document.activeElement,
      `Unsubscribe of the last agent drawn did not give the focus to the Unsubscribe of the agent drawn in its place (focus: ${document.activeElement?.textContent})`
    ).toBe(
      inItsPlace === undefined
        ? null
        : within(inItsPlace).getByRole("button", { name: "Unsubscribe" })
    );
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
    unsubscribeIssueSession.mockRestore();
  }
});

// The subscribers' Retry goes with its line once a read of them answers. Where the reader pressed
// it, the focus goes to the Subscribers control; where a read that answers on its own - a window
// focus, an invalidation after a write - takes the line away, no press of the reader's took it,
// and the focus Retry held stays on the page's body.
test("the subscribers' Retry taken away by a read the reader did not press for leaves the focus on the page's body", async () => {
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers")
    .mockRejectedValueOnce(
      new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" })
    )
    .mockResolvedValue([SUBSCRIBER]);
  const view = renderPeople();
  try {
    expect((await screen.findByRole("alert")).textContent).toBe(STATE_LINES.subscribers_unreadable);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    act(() => retry.focus());

    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(retry.isConnected, "the subscribers' Retry stayed once the read answered").toBe(false);
    expect(
      document.activeElement,
      "a read the reader did not press for took the subscribers' Retry away, and the focus left the page's body"
    ).toBe(document.body);
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// `/issues/<key>/collaborators` opens Details once per following of it (`DetailsHistory`), and
// the collaborators' list with it: a reader who closed the list there and follows the same
// address again - `t o`, or a link to the address they are on - finds it open.
test("each following of the collaborators' address opens their list, though the reader closed it", async () => {
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const getIssueCollaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue({
    agents: [],
    issue_key: KEY,
    legion_registry: "ok",
    people: [],
  });
  const address = buildIssuePath({ key: KEY, kind: "collaborators" });
  let navigate: NavigateFunction | undefined;
  function Navigator(): ReactNode {
    navigate = useNavigate();
    return null;
  }
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <QueryClientProvider client={readsHeld()}>
        <Navigator />
        <DetailsPeople issue={issue} state={undefined} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const collaborators = screen.getByRole("button", { name: PROPERTY_LABELS.collaborators });
    expect(collaborators.getAttribute("aria-expanded")).toBe("true");
    await userEvent.click(collaborators);
    expect(collaborators.getAttribute("aria-expanded")).toBe("false");

    // A link to the address the reader is on replaces it with itself (`RowText.tsx`): the same
    // address, under a key of its own.
    act(() => navigate?.(address, { replace: true }));
    expect(
      collaborators.getAttribute("aria-expanded"),
      "following the collaborators' address again left their list shut"
    ).toBe("true");
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
    getIssueCollaborators.mockRestore();
  }
});

// Under "The row you are on", the facts of the row the reader is on: its kind, its state where
// its kind has one, and who wrote it. They draw no line of its text, since the row beside them
// draws it whole.
test("the facts name the focused node's kind, state and author, and draw no first line of its text", async () => {
  const question: FocusedNode = {
    author: { id: "bob", kind: "user" },
    id: "ask-1",
    state: NODE_STATE_WORDS.ask.open,
    text: "Which store holds the drafts?",
    type: "ask",
  };
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={readsHeld()}>
        <FocusedNodeProvider>
          <Focus node={question} />
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(screen.getByTestId("focused-node-facts").getAttribute("data-focused-node")).toBe(
        "ask-1"
      )
    );
    const facts = screen.getByTestId("focused-node-facts");
    expect(within(facts).getByRole("heading", { level: 2 }).textContent).toBe(
      PROPERTY_LABELS.focused_node
    );
    // Each fact as `[label, value]`, in the order Details draws them.
    expect(
      [...facts.querySelectorAll("dl > div")].map((fact) => [
        fact.querySelector("dt")?.textContent,
        fact.querySelector("dd")?.textContent,
      ])
    ).toEqual([
      [`${CONTROL_LABELS.node_kind}:`, sentenceCase(NODE_TYPE_NAMES.ask)],
      [`${PROPERTY_LABELS.status}:`, NODE_STATE_WORDS.ask.open],
      [`${CONTROL_LABELS.written_by}:`, "bob"],
    ]);
    expect(facts.textContent, "the facts drew the row's first line").not.toContain(question.text);
  } finally {
    view.unmount();
  }
});

/** A question a planner session asked on the issue and reworded once, as the issue's questions
 *  read holds it: its author carries the title and the tmux target the session stamped. */
const PLANNERS_QUESTION: IssueAskRow = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: {
    id: "planner-session-0001",
    kind: "session",
    origin: { session_title: "Planner", tmux: "legion:4" },
  },
  block_id: null,
  created_at: "2026-11-15T10:00:00Z",
  edited_at: "2026-11-15T11:00:00Z",
  id: "ask-7",
  issue_key: KEY,
  kind: "question",
  last_reply: null,
  multiple: false,
  options: null,
  question: "Which store holds the drafts?",
  state: "open",
  urgency: "med",
};

/** That question's thread: its one earlier wording, the session that follows it, and the session
 *  holding the issue's claim, which a person's answer reaches too. */
const PLANNERS_THREAD: AskThread = {
  answers: [],
  claimant: "implementer-session-0002",
  edits: [
    {
      at: "2026-11-15T11:00:00Z",
      edited_by: PLANNERS_QUESTION.author,
      previous: {
        multiple: false,
        options: null,
        question: "Where do the drafts go?",
        urgency: "med",
      },
    },
  ],
  followers: [{ session_id: "planner-session-0001", since: "2026-11-15T10:00:00Z" }],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

/** The question's own read, `GET /api/v1/asks/{id}`. */
const PLANNERS_READ: AskRead = { ...PLANNERS_THREAD, ask: PLANNERS_QUESTION };

/** The question's row, as the outline hands it to Details. */
const PLANNERS_NODE: FocusedNode = {
  author: PLANNERS_QUESTION.author,
  id: "ask-7",
  item: { id: "ask-7", kind: "ask" },
  state: NODE_STATE_WORDS.ask.open,
  text: PLANNERS_QUESTION.question,
  type: "ask",
};

/** What the facts of the planner's question draw from its thread: its edit history, whose one
 *  earlier wording its control counts, and who a person's answer reaches, its follower and the
 *  issue's claimant, which its control counts. Each is the ask card's own words. */
async function threadDrawn(facts: HTMLElement): Promise<void> {
  expect(await within(facts).findByText("Show 1 previous version")).toBeDefined();
  expect(within(facts).getByRole("button", { name: "Reaches 2" })).toBeDefined();
}

/** Details as the page holds it around the facts: a drawer that starts shut, which `hold.open`
 *  opens and shuts. */
function FactsInDrawer({
  hold,
  node,
}: {
  hold: { open?: (open: boolean) => void };
  node: FocusedNode;
}): ReactNode {
  const [open, setOpen] = useState(false);
  hold.open = setOpen;
  const drawer: MarginDrawer = { open, setOpen, setPage: () => undefined };
  return (
    <MarginSlotProvider drawer={drawer}>
      <FocusedNodeProvider>
        <Focus node={node} />
        <DetailsFocusedNode issue={issue} state={undefined} />
      </FocusedNodeProvider>
    </MarginSlotProvider>
  );
}

// Where the page holds the issue's questions read with each one's thread, the facts of a focused
// question draw its edit history, its recipients and, behind its asker's name, the asking
// session's copy handles, from that read: they send no read of the question, and no read of the
// issue's questions of their own.
test("a focused question's facts draw its edit history, its recipients and its copy handles from the issue's questions read with their threads, and send no read", async () => {
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(PLANNERS_READ);
  const listIssueAskThreads = spyOn(api, "listIssueAskThreads").mockResolvedValue([]);
  await warmMarkdownRenderer();
  const client = readsHeld();
  client.setQueryData(["asks", KEY, "thread"], [{ ...PLANNERS_QUESTION, thread: PLANNERS_THREAD }]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <FocusedNodeProvider>
          <Focus node={PLANNERS_NODE} />
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(screen.getByTestId("focused-node-facts").getAttribute("data-focused-node")).toBe(
        "ask-7"
      )
    );
    const facts = screen.getByTestId("focused-node-facts");
    await threadDrawn(facts);

    // The handles stand behind the asker's name, shut until a press on it opens them.
    const handles = [
      "Copy session ID planner-session-0001",
      "Copy session title Planner",
      "Copy tmux target legion:4",
    ];
    const drawnHandles = () =>
      handles.filter((name) => within(facts).queryByRole("button", { name }) !== null);
    expect(drawnHandles(), "the handles were open before the asker's name was pressed").toEqual([]);
    const asker = within(facts).getByRole("button", { name: "Planner" });
    expect(asker.getAttribute("aria-expanded")).toBe("false");
    await userEvent.click(asker);
    expect(asker.getAttribute("aria-expanded")).toBe("true");
    expect(drawnHandles()).toEqual(handles);

    expect(
      getAsk,
      "the facts read the question though the page held its thread"
    ).not.toHaveBeenCalled();
    expect(
      listIssueAskThreads,
      "the facts read the issue's questions themselves"
    ).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    listIssueAskThreads.mockRestore();
  }
});

// Where the page holds no read of the issue's questions with their threads, the facts of a
// focused question read that question (`GET /api/v1/asks/{id}`) once Details opens, and never
// while it is shut: a load that leaves Details shut costs no request for it, and Details shut and
// opened again reads it no second time.
test("a focused question's facts read the question once Details opens, and never while it is shut", async () => {
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(PLANNERS_READ);
  const listIssueAskThreads = spyOn(api, "listIssueAskThreads").mockResolvedValue([]);
  await warmMarkdownRenderer();
  const hold: { open?: (open: boolean) => void } = {};
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={readsHeld()}>
        <FactsInDrawer hold={hold} node={PLANNERS_NODE} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(screen.getByTestId("focused-node-facts").getAttribute("data-focused-node")).toBe(
        "ask-7"
      )
    );
    expect(getAsk, "the facts read the question while Details was shut").not.toHaveBeenCalled();

    act(() => hold.open?.(true));
    await threadDrawn(screen.getByTestId("focused-node-facts"));
    expect(getAsk.mock.calls).toEqual([["ask-7"]]);

    act(() => hold.open?.(false));
    act(() => hold.open?.(true));
    await threadDrawn(screen.getByTestId("focused-node-facts"));
    expect(getAsk, "Details opened again read the question again").toHaveBeenCalledTimes(1);
    expect(
      listIssueAskThreads,
      "the facts read the issue's questions themselves"
    ).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    listIssueAskThreads.mockRestore();
  }
});

// A read of the focused question that failed says so in its facts, in the page's words, with
// Retry: a press reads it again, and once that read answers, the line goes with its Retry and the
// focus Retry held goes to the facts' first control rather than to the page.
test("a failed read of the focused question says so in its facts, with Retry", async () => {
  let reads = 0;
  const getAsk = spyOn(api, "getAsk").mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("Dispatch could not read the question");
    return PLANNERS_READ;
  });
  await warmMarkdownRenderer();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={readsHeld()}>
        <FocusedNodeProvider>
          <Focus node={PLANNERS_NODE} />
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const facts = await screen.findByTestId("focused-node-facts");
    const alert = await within(facts).findByRole("alert");
    expect(alert.textContent).toBe(ANSWER_LINES.ask_thread_unreadable);
    expect(within(facts).queryByText("Show 1 previous version")).toBeNull();

    await userEvent.click(within(facts).getByRole("button", { name: CONTROL_LABELS.retry }));
    await threadDrawn(facts);
    expect(getAsk).toHaveBeenCalledTimes(2);
    expect(within(facts).queryByRole("alert")).toBeNull();
    expect(document.activeElement, "the focus fell to the page with the question's Retry").toBe(
      within(facts).getByRole("button", { name: "Planner" })
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

/** A comment of bob's that heads a thread, in the outline's read of the issue's comments. */
function bobsComment(id: string, over: Partial<Comment> = {}): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "bob", kind: "user" },
    body: `Bob's comment ${id}.`,
    created_at: "2026-11-15T10:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...over,
  };
}

/** The focused row of `comment`, as the outline hands it to Details. */
function commentNode(comment: Comment): FocusedNode {
  return {
    author: comment.author,
    id: comment.id,
    item: { id: comment.id, kind: "comment" },
    text: comment.body,
    type: "comment",
  };
}

/** The facts of the focused row over the reader's state as the page holds it (`["user-state"]`),
 *  which a pin writes at once and Dispatch's answer then replaces. */
function FactsOverState(): ReactNode {
  const state = useQuery(userStateQuery()).data;
  return <DetailsFocusedNode issue={issue} state={state} />;
}

// The card controls of a comment that heads a thread and suggests a
// change follow its facts. Each sends its own write, and the facts then draw what that write
// left: Unpin once it is pinned, Reopen once its thread is resolved - with its Accept and Reject
// still there, since resolving decides no suggestion - and neither once the suggestion is taken.
test("a comment's facts pin it, resolve its thread and accept its suggestion", async () => {
  const suggested = bobsComment("comment-3", {
    suggestion: { accepted: null, replace_with: "the drafts store" },
  });
  let comments = [suggested];
  const listComments = spyOn(api, "listComments").mockImplementation(async () => comments);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(readTo(0));
  const putIssueState = spyOn(api, "putIssueState").mockImplementation(async (_key, input) => ({
    dismissed: input.dismissed ?? [],
    last_read_seq: 0,
    pinned: false,
    seq: input.seq ?? 0,
  }));
  // The first Resolve is refused; the second, the facts' own Retry, is held out until the case
  // lets it land, so the case reads where the focus went while that write is out.
  let resolves = 0;
  const held = Promise.withResolvers<void>();
  const resolveComment = spyOn(api, "resolveComment").mockImplementation(async () => {
    resolves += 1;
    if (resolves === 1) throw new Error("Dispatch refused the resolve");
    await held.promise;
    const resolved = { ...suggested, resolved: true };
    comments = [resolved];
    return resolved;
  });
  const acceptComment = spyOn(api, "acceptComment").mockImplementation(async () => {
    const accepted = {
      ...suggested,
      resolved: true,
      suggestion: { accepted: true, replace_with: "the drafts store" },
    };
    comments = [accepted];
    return { ...accepted, lost: false };
  });
  const client = readsHeld();
  holdComments(client, comments);
  client.setQueryData(["user-state"], readTo(0));
  const node = commentNode(suggested);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <FocusedNodeProvider>
          <Focus node={node} />
          <FactsOverState />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByRole("button", { name: CONTROL_LABELS.resolve });
    const controls = document.querySelector<HTMLElement>("[data-focused-controls]");
    if (controls === null) throw new Error("the comment's facts draw no controls");
    const control = (name: string) => within(controls).queryByRole("button", { name });
    expect(control(CONTROL_LABELS.accept)).not.toBeNull();
    expect(control(CONTROL_LABELS.reject)).not.toBeNull();

    await userEvent.click(within(controls).getByRole("button", { name: CONTROL_LABELS.pin }));
    await waitFor(() =>
      expect(putIssueState).toHaveBeenCalledWith(
        KEY,
        { dismissed: [pinnedItemMarker(conversationItemId("comment", "comment-3"))], seq: 1 },
        keyedAttempt
      )
    );
    expect(control(CONTROL_LABELS.unpin)?.getAttribute("aria-pressed")).toBe("true");

    await userEvent.click(within(controls).getByRole("button", { name: CONTROL_LABELS.resolve }));
    await waitFor(() => expect(resolveComment).toHaveBeenCalledWith("comment-3", keyedAttempt));
    // The refused Resolve says so with the facts' own Retry, which goes as it sends the write
    // again: Resolve stays enabled while that write is out, marked busy, so the focus goes to it.
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    await userEvent.click(retry);
    await waitFor(() => expect(resolveComment).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull()
    );
    const resolve = control(CONTROL_LABELS.resolve);
    expect(resolve).toHaveProperty("disabled", false);
    expect(resolve?.getAttribute("aria-busy")).toBe("true");
    expect(document.activeElement, "the focus fell to the page with the facts' Retry").toBe(
      resolve
    );
    held.resolve();
    const reopen = await within(controls).findByRole("button", {
      name: CONTROL_LABELS.reopen_thread,
    });
    // Reopen is the control Resolve was, so the focus is still on it.
    expect(document.activeElement, "the focus left the thread's control as it read Reopen").toBe(
      reopen
    );
    expect(control(CONTROL_LABELS.resolve)).toBeNull();
    expect(control(CONTROL_LABELS.reject)).not.toBeNull();

    await userEvent.click(within(controls).getByRole("button", { name: CONTROL_LABELS.accept }));
    await waitFor(() => expect(acceptComment).toHaveBeenCalledWith("comment-3", keyedAttempt));
    await waitFor(() => expect(control(CONTROL_LABELS.accept)).toBeNull());
    // Accept went with the suggestion it decided, and the focus with it: it is on the thread's
    // Reopen, still drawn, rather than on the page.
    expect(document.activeElement, "the focus fell to the page with Accept").toBe(
      control(CONTROL_LABELS.reopen_thread)
    );
    expect(control(CONTROL_LABELS.reject)).toBeNull();
    expect(control(CONTROL_LABELS.unpin)).not.toBeNull();
    expect(control(CONTROL_LABELS.reopen_thread)).not.toBeNull();
  } finally {
    view.unmount();
    listComments.mockRestore();
    getMyState.mockRestore();
    putIssueState.mockRestore();
    resolveComment.mockRestore();
    acceptComment.mockRestore();
  }
});

// Chromium moves the focus of a focused button that turns disabled to the page, and enabling it
// again does not bring it back (`chromium-focus-fixup.ts` does the same here). A thread's
// Resolve therefore stays enabled while its write is out, marked busy: a keyboard reader who
// pressed it is still on it then, and on the same control once the thread is resolved and it
// reads Reopen.
test("a keyboard Resolve keeps the focus on its control while its write is out and once it reads Reopen", async () => {
  const open = bobsComment("comment-5");
  let comments = [open];
  const listComments = spyOn(api, "listComments").mockImplementation(async () => comments);
  // The write is held out until the case lets it land.
  const held = Promise.withResolvers<void>();
  const resolveComment = spyOn(api, "resolveComment").mockImplementation(async () => {
    await held.promise;
    const resolved = { ...open, resolved: true };
    comments = [resolved];
    return resolved;
  });
  const client = readsHeld();
  holdComments(client, comments);
  const restoreFocus = emulateChromiumFocusFixup();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <FocusedNodeProvider>
          <Focus node={commentNode(open)} />
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const resolve = await screen.findByRole("button", { name: CONTROL_LABELS.resolve });
    resolve.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(resolveComment).toHaveBeenCalledWith("comment-5", keyedAttempt));
    // Resolve as the page draws it while its write is out, whichever way it draws that.
    await waitFor(() => expect(resolve.matches(":disabled, [aria-busy='true']")).toBe(true));
    expect(
      document.activeElement,
      "a keyboard Resolve left the focus on the page while its write was out"
    ).toBe(resolve);

    held.resolve();
    const reopen = await screen.findByRole("button", { name: CONTROL_LABELS.reopen_thread });
    expect(reopen, "Reopen is not the control Resolve was").toBe(resolve);
    expect(document.activeElement, "the focus left Reopen once the thread was resolved").toBe(
      reopen
    );
  } finally {
    restoreFocus();
    view.unmount();
    listComments.mockRestore();
    resolveComment.mockRestore();
  }
});

/** Draws Details on a suggestion of bob's that waits for its decision, gives Accept the focus,
 *  takes the case's own step `before` where it has one, and then has another session accept the
 *  suggestion. Asserts that Accept went and Reopen was drawn, and that the focus Accept held
 *  stayed on the page's body; `when` says when the suggestion was decided, for the failure. */
async function decidedElsewhere(when: string, before?: () => Promise<unknown>): Promise<void> {
  const suggested = bobsComment("comment-6", {
    suggestion: { accepted: null, replace_with: "the drafts store" },
  });
  const client = readsHeld();
  client.setQueryData(["comments", KEY], [suggested]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <FocusedNodeProvider>
          <Focus node={commentNode(suggested)} />
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const accept = await screen.findByRole("button", { name: CONTROL_LABELS.accept });
    act(() => accept.focus());
    await before?.();

    act(() =>
      client.setQueryData(
        ["comments", KEY],
        [
          {
            ...suggested,
            resolved: true,
            suggestion: { accepted: true, replace_with: "the drafts store" },
          },
        ]
      )
    );
    expect(accept.isConnected, "Accept stayed once the suggestion was decided").toBe(false);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.reopen_thread })).toBeDefined();
    expect(
      document.activeElement,
      `another session decided the suggestion ${when}, and the focus left the page's body`
    ).toBe(document.body);
  } finally {
    view.unmount();
  }
}

// Accept and Reject go once the suggestion is decided. The reader's own Accept hands the focus on
// to the thread's control; a decision made in another session is no press of the reader's, so the
// focus Accept held stays on the page's body, as on every page of Dispatch.
test("a suggestion decided in another session leaves the focus Accept held on the page's body", async () => {
  await decidedElsewhere("while the focus was on Accept");
});

// ArrowDown on Accept scrolls Details and acts on nothing, so it is no press of Accept: a
// suggestion decided in another session after it still leaves the focus Accept held on the page's
// body.
test("a suggestion decided in another session after ArrowDown on Accept leaves the focus on the page's body", async () => {
  await decidedElsewhere("after ArrowDown on Accept", () => userEvent.keyboard("{ArrowDown}"));
});

/** One attempt to deliver `comment-2`'s mention to `target`. */
function attempt(target: string, over: Partial<CommentDelivery>): CommentDelivery {
  return {
    attempt: 1,
    comment_id: "comment-2",
    created_at: new Date().toISOString(),
    delivery: "btw",
    duplicate: false,
    envelope_id: null,
    error: null,
    reply_id: null,
    resolve_error: null,
    session_id: null,
    state: "sent",
    target,
    ...over,
  };
}

/** One attempt to deliver `message-1` to the session it reached, `""` where it reached none. */
function messageAttempt(over: Partial<MessageDelivery>): MessageDelivery {
  return {
    accepted_as: null,
    accepted_at: null,
    attempt: 1,
    created_at: new Date().toISOString(),
    delivery: "btw",
    duplicate: false,
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    requested_by: null,
    session_id: "",
    state: "sent",
    ...over,
  };
}

// The Spec's `activity-feed-names-mention-delivery-outcomes`:
// each delivery of the focused comment says how it stands. One that could not reach its agent
// reads "Not delivered", with Retry only while a Retry cannot deliver it twice (`isSafeRetry`),
// and one the listener holds for a stopped session names the wait, as `queuedParts` builds it.
test("a delivery that could not reach its agent reads Not delivered with Retry, and a queued one names its wait", async () => {
  const mentioned = bobsComment("comment-2", {
    deliveries: [
      attempt("session:planner", { error: "no live session planner", state: "failed" }),
      attempt("session:tester", {
        created_at: new Date(Date.now() - 4 * 24 * 60 * 60 * 1000).toISOString(),
        error: "no live session tester",
        state: "failed",
      }),
      attempt("session:reviewer", { state: "queued" }),
    ],
  });
  const listComments = spyOn(api, "listComments").mockResolvedValue([mentioned]);
  // Bob's message to a role: the session it reached answered the first attempt with an error,
  // and the second could not reach the one it named.
  const thread: MessageRead = {
    message: {
      author: { id: "bob", kind: "user" },
      body: "Bob's message.",
      broadcast_id: null,
      created_at: "2026-11-15T10:00:00Z",
      deliveries: [
        messageAttempt({
          envelope_id: "envelope-1",
          error: "the side turn failed",
          state: "failed",
        }),
        messageAttempt({
          attempt: 2,
          error: "no live session reviewer-3",
          session_id: "reviewer-3",
          state: "failed",
        }),
      ],
      ...messageCorrectionFields(),
      id: "message-1",
      in_reply_to: null,
      issue_key: KEY,
      reply_block: null,
      target: "role:legion-p-core-1-reviewer",
    },
    replies: [],
  };
  const listIssueMessages = spyOn(api, "listIssueMessages").mockResolvedValue([thread]);
  const createCommentDelivery = spyOn(api, "createCommentDelivery").mockImplementation(
    async (_id, target, delivery) => attempt(target ?? "", { attempt: 2, delivery })
  );
  const createMessageDelivery = spyOn(api, "createMessageDelivery").mockImplementation(
    async (_id, delivery) => messageAttempt({ attempt: 3, delivery })
  );
  const client = readsHeld();
  // The outline read the comments a minute ago, and the app's client holds a read 30 s
  // (`main.tsx`): the facts read them from the cache all the same, so the focus landing on the
  // comment costs no request.
  client.setDefaultOptions({ queries: { retry: false, staleTime: 30_000 } });
  holdComments(client, [mentioned], { updatedAt: Date.now() - 60_000 });
  client.setQueryData(["issue-messages", KEY], [thread]);
  const drawn = (focused: FocusedNode) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <FocusedNodeProvider>
          <Focus node={focused} />
          <DetailsFocusedNode issue={issue} state={undefined} />
        </FocusedNodeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(drawn(commentNode(mentioned)));
  try {
    await waitFor(() =>
      expect(document.querySelectorAll("[data-deliveries] > li")).toHaveLength(3)
    );
    expect(listComments, "the facts read the outline's comments again").not.toHaveBeenCalled();
    const [failed, old, queued] = [
      ...document.querySelectorAll<HTMLElement>("[data-deliveries] > li"),
    ];
    if (failed === undefined || old === undefined || queued === undefined) {
      throw new Error("the facts draw fewer than three deliveries");
    }
    const retryOf = (row: HTMLElement) =>
      within(row).queryByRole("button", { name: CONTROL_LABELS.retry });
    expect(failed.textContent).toContain(DELIVERY_STATE_WORDS.failed);
    expect(failed.textContent).not.toContain(DELIVERY_STATE_WORDS.sent);
    expect(retryOf(failed)).not.toBeNull();
    // Past the window in which a repeat is recognised, a Retry could deliver it twice.
    expect(old.textContent).toContain(DELIVERY_STATE_WORDS.failed);
    expect(retryOf(old)).toBeNull();
    // A queued mention is neither delivered nor refused, so it has nothing to retry.
    const reviewer = queued.querySelector('[data-agent-name="reviewer"]')?.textContent;
    expect(queued.textContent).toContain(`Queued: ${reviewer} ${queuedWait}`);
    expect(queued.textContent).not.toContain(DELIVERY_STATE_WORDS.sent);
    expect(retryOf(queued)).toBeNull();

    // A mention sent to a role is named by the session that took it, and one no session took
    // by the page's word for an agent: an address is no name. The read of the comments the
    // Retry's write sets off answers with these deliveries.
    const roled = bobsComment("comment-2", {
      deliveries: [
        attempt("role:legion-p-core-1-tester", { session_id: "tester-7" }),
        attempt("role:legion-p-core-1-reviewer", {}),
      ],
    });
    listComments.mockResolvedValue([roled]);
    await userEvent.click(within(failed).getByRole("button", { name: CONTROL_LABELS.retry }));

    await waitFor(() =>
      expect(createCommentDelivery).toHaveBeenCalledWith(
        "comment-2",
        "session:planner",
        "btw",
        keyedAttempt
      )
    );
    expect(createCommentDelivery).toHaveBeenCalledTimes(1);

    await waitFor(() =>
      expect(
        document.querySelector('[data-deliveries] [data-agent-name="tester-7"]'),
        "the mention's delivery is not named by the session it reached"
      ).not.toBeNull()
    );
    const [reached, unheld] = [...document.querySelectorAll<HTMLElement>("[data-deliveries] > li")];
    expect(reached?.textContent).not.toContain("role:");
    expect(unheld?.textContent).toStartWith(`${CONTROL_LABELS.unnamed_recipient} · `);
    // The failed attempt's Retry went once the read its write set off landed, and the focus with
    // it: it is on the thread's Resolve rather than on the page.
    expect(document.activeElement, "the focus fell to the page with the delivery's Retry").toBe(
      screen.getByRole("button", { name: CONTROL_LABELS.resolve })
    );

    // A message's deliveries the same, its Retry sent again for that message.
    view.rerender(
      drawn({
        author: thread.message.author,
        id: "message-1",
        item: { id: "message-1", kind: "message" },
        text: thread.message.body,
        type: "message",
      })
    );
    await waitFor(() =>
      expect(
        document.querySelector('[data-deliveries] [data-agent-name="reviewer-3"]')
      ).not.toBeNull()
    );
    const [answered, refused] = [
      ...document.querySelectorAll<HTMLElement>("[data-deliveries] > li"),
    ];
    if (answered === undefined || refused === undefined) {
      throw new Error("the message's facts draw fewer than two deliveries");
    }
    expect(answered.textContent).toStartWith(`${CONTROL_LABELS.unnamed_recipient} · `);
    expect(answered.textContent).not.toContain("role:");
    expect(retryOf(answered)).toBeNull();
    // What a message's failure says is the message's own card's guidance, not a mention's.
    expect(answered.textContent).toContain(answeredWithErrorGuidance("card"));

    await userEvent.click(within(refused).getByRole("button", { name: CONTROL_LABELS.retry }));

    await waitFor(() =>
      expect(createMessageDelivery).toHaveBeenCalledWith("message-1", "btw", keyedAttempt)
    );
    expect(createMessageDelivery).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    listComments.mockRestore();
    listIssueMessages.mockRestore();
    createCommentDelivery.mockRestore();
    createMessageDelivery.mockRestore();
  }
});

/** A claim of the issue, which the history draws as one line, made `10 - seq` seconds ago, so a
 *  higher sequence is newer and each is today's. */
function claim(seq: number): DispatchEvent {
  return {
    actor: { id: "planner", kind: "session" },
    created_at: new Date(Date.now() - (10 - seq) * 1000).toISOString(),
    id: seq,
    issue_key: KEY,
    notify: false,
    payload: {},
    seq,
    type: "issue.claimed",
  } as DispatchEvent;
}

/** What opens and shuts Details around the history, moves the reader's read mark, and takes the
 *  page to another address: the test's hold on `HistoryInDrawer`. `isOpen` is whether Details is
 *  open, as it was last drawn. */
interface DrawerHold {
  isOpen?: boolean;
  navigate?: NavigateFunction;
  open?: (open: boolean) => void;
  read?: (state: UserState) => void;
}

/** The history in Details as the page draws it: inside the drawer, which starts shut, over the
 *  reader's state of the issue, read through its second event. `of` is the issue, the fixture's
 *  `issue` unless a case gives one. */
function HistoryInDrawer({ hold, of = issue }: { hold: DrawerHold; of?: IssueDetails }): ReactNode {
  const [open, setOpen] = useState(false);
  const [state, setState] = useState(() => readTo(2));
  hold.isOpen = open;
  hold.open = setOpen;
  hold.navigate = useNavigate();
  hold.read = setState;
  const drawer: MarginDrawer = { open, setOpen, setPage: () => undefined };
  return (
    <MarginSlotProvider drawer={drawer}>
      <DetailsHistory issue={of} state={state} />
    </MarginSlotProvider>
  );
}

/** The history's entries as it lays them out one under another: each line of a day's list,
 *  an event's, a thread's or the line that says what is new. Everything inside one stands in it. */
const HISTORY_UNITS = "[data-history-day] > ol > li";

// The line that says what is new stands where the reader's mark
// stood when the history first drew in this visit. The history is not drawn while Details is
// shut, so `DetailsHistory` holds that mark: a move of the mark in between - another tab read
// the issue - leaves the line where the reader found it when Details opens again.
test("the line that says what is new holds across a close and an open of Details", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue(
    [4, 3, 2, 1].map((seq) => claim(seq))
  );
  const hold: DrawerHold = {};
  // The outline read the issue's comments, messages and asks a minute ago, and the app's client
  // holds a read 30 s (`main.tsx`): each open of Details draws the history from them all the
  // same, so an open costs no request but the log's.
  const outlineReads = [
    spyOn(api, "listComments").mockResolvedValue([OWN_COMMENT]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const client = readsHeld();
  client.setDefaultOptions({ queries: { retry: false, staleTime: 30_000 } });
  const minuteAgo = { updatedAt: Date.now() - 60_000 };
  client.setQueryData(["asks", KEY], [], minuteAgo);
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, [], minuteAgo);
  client.setQueryData(["comments", KEY], [OWN_COMMENT], minuteAgo);
  client.setQueryData(["issue-messages", KEY], [], minuteAgo);
  /** The history's entries in the order it draws them: each event by its sequence, and the
   *  line that says what is new by its words. */
  const drawn = () =>
    [...document.querySelectorAll(HISTORY_UNITS)].map(
      (entry) => entry.getAttribute("data-event-seq") ?? entry.textContent
    );
  const atTheMark = ["4", "3", CONTROL_LABELS.history_unread, "2", "1"];
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${KEY}`]}>
      <QueryClientProvider client={client}>
        <HistoryInDrawer hold={hold} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    // Shut, Details draws no history and reads no log.
    expect(screen.queryByTestId("issue-history")).toBeNull();
    expect(getIssueEvents).not.toHaveBeenCalled();

    act(() => hold.open?.(true));
    await waitFor(() => expect(drawn()).toEqual(atTheMark));
    expect(
      outlineReads.map((read) => read.mock.calls.length),
      "the history read the outline's comments, messages or asks again"
    ).toEqual([0, 0, 0]);

    act(() => hold.open?.(false));
    expect(screen.queryByTestId("issue-history")).toBeNull();
    act(() => hold.read?.(readTo(4)));
    act(() => hold.open?.(true));

    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(4));
    expect(drawn()).toEqual(atTheMark);
    expect(outlineReads.map((read) => read.mock.calls.length)).toEqual([0, 0, 0]);
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
    for (const read of outlineReads) read.mockRestore();
  }
});

/** A thread the Legion daemon began, one message written a minute ago: no row of the outline,
 *  an entry of the history (`agents-only.ts`). It is built when the file loads, before any case
 *  sets the clock, so it is dated from the cases' midday rather than from `Date.now()`. */
const AGENTS_THREAD: MessageRead = {
  message: {
    author: { id: "legion-daemon:CORE", kind: "session" },
    body: "The daemon's own words.",
    broadcast_id: null,
    created_at: new Date(NOON.getTime() - 60_000).toISOString(),
    deliveries: [],
    ...messageCorrectionFields(),
    id: "message-agents",
    in_reply_to: null,
    issue_key: KEY,
    reply_block: null,
    target: null,
  },
  replies: [],
};

/** `readsHeld()`'s client with the agents' thread as the issue's one thread of messages
 *  (`AGENTS_THREAD`), and the issue's results and decisions, which the outline lands by too,
 *  held empty; the spy that answers each read of the outline's coverage, which it lands by as
 *  well, with none, and which a case can hold out instead; and the spy that answers each read of
 *  the names the page's words reference with none. The case restores both spies. */
function agentsThreadClient() {
  const getIssueCoverage = spyOn(api, "getIssueCoverage").mockResolvedValue({
    requirements: [],
  } as never);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const client = readsHeld();
  client.setQueryData(["issue-messages", KEY], [AGENTS_THREAD]);
  client.setQueryData(issueResultsQuery(KEY).queryKey, { checks: [] } as never);
  client.setQueryData(["decisions", KEY], { decisions: [], document: null, open_questions: [] });
  return { client, getIssueCoverage, names };
}

// The address of a turn of a thread the history holds (`agents-only.ts`) opens Details at the
// history with that turn marked by the outline's own decision (`address-target.ts`'s
// `useAddressFallback`): once every read the outline's tree is built from has answered, as the
// outline lands on the turn's nearest drawn ancestor. While one of those reads is out the outline
// has landed nowhere, and Details stays shut. Details used to decide from the comments and the
// messages alone, and opened at the turn while the outline still waited.
test("an agents' turn's address opens Details at it once every read the outline lands by has answered", async () => {
  const coverage = Promise.withResolvers<IssueCoverage>();
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const { client, getIssueCoverage, names } = agentsThreadClient();
  getIssueCoverage.mockImplementation(() => coverage.promise);
  const id = AGENTS_THREAD.message.id;
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ id, key: KEY, kind: "message" })]}>
      <QueryClientProvider client={client}>
        <HistoryInDrawer hold={{}} of={{ ...issueDetails(), artifacts: [] }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    // `render` ran the first commit's effects, where Details is opened; one more turn of the
    // loop draws what a state they set asks for, and lets the reads they started settle.
    await act(async () => {});
    expect(
      screen.queryByTestId("issue-history"),
      "Details opened at the agents' turn while a read the outline lands by was out"
    ).toBeNull();
    expect(getIssueCoverage, "nothing read the outline's coverage").toHaveBeenCalledTimes(1);

    await act(async () => coverage.resolve({ requirements: [] } as never));
    await waitFor(() =>
      expect(
        turnMarked(id),
        "the agents' turn's address did not open Details with the turn marked"
      ).toBe(true)
    );
  } finally {
    view.unmount();
    for (const spy of [getIssueCoverage, getIssueEvents, names]) spy.mockRestore();
  }
});

// The address of a turn of a thread the history holds draws the history's entries down to that
// turn, past the first 20, and brings the turn into view once for each following of the
// address: the same address followed a second time while Details is open brings it into view
// again. The landing once happened once for each turn the history drew, so a second following
// of the address left the turn wherever the reader had scrolled it.
test("an agents' turn's address draws the history down to it, and each following of it brings it into view again", async () => {
  // Twenty-five claims newer than the agents' thread, a minute old: its entry is the 26th.
  const log = Array.from({ length: 25 }, (_, index) => claimMade(25 - index, index + 1));
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue(log);
  const { client, getIssueCoverage, names } = agentsThreadClient();
  // Each element the page brings into view.
  const intoView: Element[] = [];
  const scrolls = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    intoView.push(this);
  });
  const id = AGENTS_THREAD.message.id;
  const landings = () =>
    intoView.filter((element) => element.getAttribute("data-history-item") === id).length;
  const hold: DrawerHold = {};
  const address = buildIssuePath({ id, key: KEY, kind: "message" });
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <QueryClientProvider client={client}>
        <HistoryInDrawer hold={hold} of={{ ...issueDetails(), artifacts: [] }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() => {
      expect(turnMarked(id), "the agents' turn's address marked no turn").toBe(true);
      expect(landings(), "the agents' turn's address did not bring the turn into view").toBe(1);
    });
    expect(
      document.querySelectorAll("[data-history-day] > ol > [data-history-entry]"),
      "the history is not drawn down to the turn the address names"
    ).toHaveLength(26);
    expect(hold.isOpen).toBe(true);

    act(() => hold.navigate?.(address));
    await waitFor(() =>
      expect(
        landings(),
        "the agents' turn's address followed again did not bring the turn into view again"
      ).toBe(2)
    );
    expect(turnMarked(id)).toBe(true);
  } finally {
    view.unmount();
    for (const spy of [getIssueCoverage, getIssueEvents, names, scrolls]) spy.mockRestore();
  }
});

// An agent's note to the issue's people is no turn of a thread the history holds: Threads opens
// its address, and Details opens for it no more than for any row of the outline. The daemon's
// thread beside it is the history's, and its address opens Details at its turn.
test("the address of an agent's note opens no Details, and that of the daemon's thread beside it does", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const { client, getIssueCoverage, names } = agentsThreadClient();
  const note: MessageRead = {
    message: {
      ...AGENTS_THREAD.message,
      author: { id: "planner-session", kind: "session" },
      body: "The planner's note to the issue's people.",
      id: "message-note",
    },
    replies: [],
  };
  client.setQueryData(["issue-messages", KEY], [AGENTS_THREAD, note]);
  // The stub models a tree read that returns the note, a node under the issue beside the issue's
  // comment (`holdComments`), held by both tree reads, so the outline lands on the note's row and
  // reads nothing more for it. INFRA-1253's tree read returns no such note (decision 453), and
  // part M rewrites this case under that decision.
  const tree = treeAnswer(
    [
      treeNode(KEY, "issue", null),
      commentTreeNode(OWN_COMMENT),
      treeNode(note.message.id, "message", KEY, {
        author: [note.message.author],
        made_at: note.message.created_at,
        text: note.message.body,
      }),
    ],
    [],
    { issue: KEY }
  );
  client.setQueryData(issueTreeQuery(KEY).queryKey, tree);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, tree);
  const hold: DrawerHold = {};
  const view = render(
    <MemoryRouter
      initialEntries={[buildIssuePath({ id: note.message.id, key: KEY, kind: "message" })]}
    >
      <QueryClientProvider client={client}>
        <HistoryInDrawer hold={hold} of={{ ...issueDetails(), artifacts: [] }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    // Every read the outline lands by is held or answers at once, so these turns of the loop and
    // two frames are past the outline's landing on the note's row, where an open asked for has run.
    await act(async () => {});
    await nextFrames(2);
    expect(hold.isOpen, "the address of an agent's note opened Details").toBe(false);
    expect(screen.queryByTestId("issue-history")).toBeNull();

    const id = AGENTS_THREAD.message.id;
    act(() => hold.navigate?.(buildIssuePath({ id, key: KEY, kind: "message" })));
    await waitFor(() => {
      expect(hold.isOpen, "the address of the daemon's thread left Details shut").toBe(true);
      expect(turnMarked(id)).toBe(true);
    });
  } finally {
    view.unmount();
    for (const spy of [getIssueCoverage, getIssueEvents, names]) spy.mockRestore();
  }
});

/** Whether the history marks the turn `id`, as the address of that turn has it. */
function turnMarked(id: string): boolean {
  return (
    document.querySelector(`[data-history-item="${id}"]`)?.getAttribute("aria-current") === "true"
  );
}

/** Takes away the answer `client` holds under `queryKey` and reads it again, as a reset of the
 *  read does: nothing is held under the key until the read sent again answers. */
function readAgainFromNothing(client: QueryClient, queryKey: readonly unknown[]): void {
  act(() => {
    void client.resetQueries({ queryKey });
  });
}

/** Reads the outline's messages again from nothing (`readAgainFromNothing`), which takes the
 *  agents' turn back and the history with it while Details is shut; then answers that read with
 *  the agents' thread through `answer`, and waits for the history to mark the turn again and two
 *  frames past it, where an open of Details the answer asked for has run. */
async function readMessagesAgainFromNothing(
  client: QueryClient,
  answer: (messages: MessageRead[]) => void
): Promise<void> {
  readAgainFromNothing(client, ["issue-messages", KEY]);
  await waitFor(() =>
    expect(
      screen.queryByTestId("issue-history"),
      "the outline's messages read out again with nothing held left the agents' turn told"
    ).toBeNull()
  );
  await act(async () => answer([AGENTS_THREAD]));
  await waitFor(() =>
    expect(
      turnMarked(AGENTS_THREAD.message.id),
      "the messages read answered again, and the history marked no turn"
    ).toBe(true)
  );
  await nextFrames(2);
}

// Every entry the browser makes has the key "default", a load and a fragment typed in the address
// bar alike, so a fragment typed at `/issues/<key>/activity` is a following of its own though the
// key stays one (`useFollowing`): Details opens again at it, though the reader closed it. Details
// told followings apart by the key and the address without its fragment, took the typed fragment
// for the following it had opened at, and stayed shut.
test("a fragment typed at the history's address opens Details again, though the reader closed it", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const hold: DrawerHold = {};
  const activity = buildIssuePath({ key: KEY, kind: "activity" });
  const view = renderAtBrowserEntry(
    activity,
    <QueryClientProvider client={readsHeld()}>
      <HistoryInDrawer hold={hold} />
    </QueryClientProvider>
  );
  try {
    await waitFor(() =>
      expect(hold.isOpen, "a load of the history's address left Details shut").toBe(true)
    );
    act(() => hold.open?.(false));

    view.follow(`${activity}#history`);
    await waitFor(() =>
      expect(
        hold.isOpen,
        "a fragment typed at the history's address, under the one key the browser gives its entries, left Details shut after the reader closed it"
      ).toBe(true)
    );
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
  }
});

// The address of an agents' turn opens Details at it once per following, and the reader's Close
// stands for the rest of that following. The outline's read of the messages going out again with
// nothing held takes the turn back until it answers, and the turn told again for the same
// following leaves Details shut. A fragment typed at the address is a following of its own: typed
// while a read the outline lands by is out, the outline lets go of the turn until that read
// answers, Details then opens at the turn once, and a later answer of the outline's reads leaves it
// shut. Details told followings apart without the fragment and forgot the one it had opened for
// as the turn was taken back, so it opened again at each answer of the reads.
test("a closed Details stays closed when the outline's reads answer again for the same following of an agents' turn's address, and after a fragment typed there while a read the outline lands by is out, opens once at the turn and stays closed at a later answer of the reads", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  // Each read of the outline's messages is answered through the resolver it finds here.
  let messages = Promise.withResolvers<MessageRead[]>();
  const listIssueMessages = spyOn(api, "listIssueMessages").mockImplementation(
    () => messages.promise
  );
  const { client, getIssueCoverage, names } = agentsThreadClient();
  const hold: DrawerHold = {};
  const id = AGENTS_THREAD.message.id;
  const address = buildIssuePath({ id, key: KEY, kind: "message" });
  const view = renderAtBrowserEntry(
    address,
    <QueryClientProvider client={client}>
      <HistoryInDrawer hold={hold} of={{ ...issueDetails(), artifacts: [] }} />
    </QueryClientProvider>
  );
  try {
    await waitFor(() => {
      expect(turnMarked(id), "the agents' turn's address marked no turn in the history").toBe(true);
      expect(hold.isOpen, "the agents' turn's address left Details shut").toBe(true);
    });
    act(() => hold.open?.(false));

    await readMessagesAgainFromNothing(client, messages.resolve);
    expect(
      hold.isOpen,
      "the outline's reads answered again for the same following of the agents' turn's address, and Details opened again after the reader closed it"
    ).toBe(false);

    // The outline's coverage goes out again with nothing held, and the fragment is typed while it
    // is out.
    const coverage = Promise.withResolvers<IssueCoverage>();
    getIssueCoverage.mockImplementation(() => coverage.promise);
    readAgainFromNothing(client, issueCoverageQuery(KEY).queryKey);
    view.follow(`${address}#reply`);
    await waitFor(() =>
      expect(
        screen.queryByTestId("issue-history"),
        "a fragment typed while the outline's coverage was out left the agents' turn told"
      ).toBeNull()
    );
    expect(hold.isOpen, "a fragment typed while a read was out opened Details").toBe(false);

    await act(async () => coverage.resolve({ requirements: [] } as never));
    await waitFor(() => {
      expect(turnMarked(id), "the coverage answered, and the history marked no turn").toBe(true);
      expect(
        hold.isOpen,
        "the coverage answered after the fragment typed at the agents' turn's address, and Details stayed shut"
      ).toBe(true);
    });
    act(() => hold.open?.(false));
    await nextFrames(2);
    expect(
      hold.isOpen,
      "Details opened again at the turn the fragment's following told, after the reader closed it"
    ).toBe(false);

    messages = Promise.withResolvers<MessageRead[]>();
    await readMessagesAgainFromNothing(client, messages.resolve);
    expect(
      hold.isOpen,
      "a later answer of the outline's reads in the fragment's following opened Details again after the reader closed it"
    ).toBe(false);
  } finally {
    view.unmount();
    for (const spy of [getIssueCoverage, getIssueEvents, listIssueMessages, names]) {
      spy.mockRestore();
    }
  }
});

// Back to an earlier entry of `/issues/<key>/activity` is that entry's following again, and the
// reader left it for the issue's own address, which opens no Details: Details opens again there,
// though the reader closed it before leaving.
test("Back to the history's address from the issue's own address opens Details again, though the reader closed it", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const hold: DrawerHold = {};
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: KEY, kind: "activity" })]}>
      <QueryClientProvider client={readsHeld()}>
        <HistoryInDrawer hold={hold} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(hold.isOpen, "a load of the history's address left Details shut").toBe(true)
    );
    act(() => hold.open?.(false));
    act(() => hold.navigate?.(`/issues/${KEY}`));
    await nextFrames(2);
    expect(hold.isOpen, "the issue's own address opened Details").toBe(false);

    act(() => hold.navigate?.(-1));
    await waitFor(() =>
      expect(
        hold.isOpen,
        "Back to the history's address from the issue's own address left Details shut"
      ).toBe(true)
    );
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
  }
});

// Back to an earlier entry of `/issues/<key>/activity` is that entry's following again. A
// comment's address with a row of the outline opens no Details, so Details forgets the following
// it last opened for there, and Back to the history's address opens it again, though the reader
// closed it at the comment.
test("Back to the history's address from a comment's address with a row of the outline opens Details again, though the reader closed it there", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const { client, getIssueCoverage, names } = agentsThreadClient();
  const hold: DrawerHold = {};
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: KEY, kind: "activity" })]}>
      <QueryClientProvider client={client}>
        <HistoryInDrawer hold={hold} of={{ ...issueDetails(), artifacts: [] }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(hold.isOpen, "a load of the history's address left Details shut").toBe(true)
    );
    // The reader follows a link to their own comment, which heads a thread a person wrote and so
    // is a row of the outline (`OWN_COMMENT`).
    act(() => hold.navigate?.(buildIssuePath({ id: OWN_COMMENT.id, key: KEY, kind: "comment" })));
    act(() => hold.open?.(false));
    await nextFrames(2);
    expect(hold.isOpen, "the address of a comment with a row of the outline opened Details").toBe(
      false
    );

    act(() => hold.navigate?.(-1));
    await waitFor(() =>
      expect(
        hold.isOpen,
        "Back to the history's address from a comment's address with a row of the outline left Details shut"
      ).toBe(true)
    );
  } finally {
    view.unmount();
    for (const spy of [getIssueCoverage, getIssueEvents, names]) spy.mockRestore();
  }
});

/** Where Details' box stands in the window and how tall its view is, in CSS pixels. */
const BOX_TOP = 50;
const BOX_HEIGHT = 850;

/** True where `rect` lies wholly in the view of Details' box. */
function whollyInBox(rect: Pick<DOMRect, "bottom" | "top">): boolean {
  return rect.top >= BOX_TOP && rect.bottom <= BOX_TOP + BOX_HEIGHT;
}

/** How tall each row of Details is that a case does not size. */
const ROW_HEIGHT = 40;
/** How far a row grows when the read of what it shows lands, as Blocked by grows once the
 *  titles of the issues it names are in. */
const GROWTH = 300;

/** Pushes the `issue` scope, which the page's view row pushes (`useIssueKeymap.ts`) and `t o`
 *  is bound in. */
function IssueScope(): null {
  useKeymapScope("issue");
  return null;
}

const READ_THROUGH_TWO = readTo(2);

/** How tall each entry of the history is that a case does not size: a line of a day's list,
 *  `HISTORY_UNITS`. */
const ENTRY_HEIGHT = 50;
/** How tall each line is that the history draws outside its entries and that holds words: its
 *  heading, a day's line, the loading line, Show more. */
const HISTORY_LINE = 20;
/** The history's block in Details. */
const HISTORY_BLOCK = '[data-testid="issue-history"]';

/** What opens and shuts Details around the People rows, and takes the page to another address:
 *  the test's hold on `PeopleInDrawer`. */
interface PeopleHold {
  navigate?: NavigateFunction;
  open?: (open: boolean) => void;
}

/** Details as the new page draws it around its People rows: a drawer, shut, whose box scrolls
 *  what Details holds - a row above the People rows, the People rows, a row below them, and the
 *  history, which an address that opens Details waits for. `of` is the issue. */
function PeopleInDrawer({ hold, of }: { hold: PeopleHold; of: IssueDetails }): ReactNode {
  const [open, setOpen] = useState(false);
  hold.open = setOpen;
  hold.navigate = useNavigate();
  const drawer: MarginDrawer = { open, setOpen, setPage: () => undefined };
  return (
    <MarginSlotProvider drawer={drawer}>
      <IssueScope />
      <aside hidden={!open} {...{ [DETAILS_ATTRIBUTE]: "" }}>
        <div data-testid="details-box" style={{ overflowY: "auto" }}>
          <div>
            <div data-testid="row-above" />
            <DetailsPeople issue={of} state={READ_THROUGH_TWO} />
            <div data-testid="row-below" />
            <DetailsHistory issue={of} state={READ_THROUGH_TWO} />
          </div>
        </div>
      </aside>
    </MarginSlotProvider>
  );
}

/** What a case can draw Details with beyond the People rows' own reads (`drawPeopleInDrawer`). */
interface DetailsDrawn {
  /** Whether the box moves as the browser's own scroll anchoring moves it, across a change of
   *  height a case makes (`grow`), while the box's own anchoring is on: a place keeper that keeps
   *  a place there turns it off (`overflow-anchor: none`). Off unless a case asks for it. */
  browserAnchoring?: boolean;
  /** The client the page reads through, `readsHeld()`'s unless a case gives one. */
  client?: QueryClient;
  /** Whether `performance.now`, which tells the reader's acts and scrolls from the page's
   *  (`ViewportAnchor`), stands still but where the case moves it on (`later`). */
  heldClock?: boolean;
  /** The issue, the fixture's `issue` unless a case gives one. */
  issue?: IssueDetails;
  /** The answer to the history's read of the issue's log, an empty log at once unless a case
   *  holds it back. */
  log?: Promise<DispatchEvent[]>;
  /** The answer to each read of a next page of the log, which Show more sends. */
  older?: () => Promise<DispatchEvent[]>;
}

/** Waits `count` frames of the page. */
async function nextFrames(count: number): Promise<void> {
  for (let frame = 0; frame < count; frame += 1) await act(nextFrame);
}

/**
 * `PeopleInDrawer` at `path`, laid out as Details stacks its rows: each row of the box's content
 * under the one before, from the box's top, moved up by the box's scroll, and each element as
 * the row it stands in, but for the control at the head of a row a control elsewhere opens
 * Details at (`data-details-row`): the first element of what the row holds, as `WideRow` draws
 * the Collaborators control above the list it opens, stands at the row's top, as tall as a row
 * the case does not size. The history's block is laid out inside its row as blocks stack: each
 * entry of the history under the one before (`HISTORY_UNITS`), everything inside an entry as the
 * entry, and each line outside them one `HISTORY_LINE` tall. The box's view is `BOX_HEIGHT`
 * tall (`clientHeight`), what it scrolls is its content's height (`scrollHeight`), a point of
 * the page names what is drawn there (`elementFromPoint`), and `scrollIntoView` scrolls the box
 * as the browser does. happy-dom lays nothing out, never calls a resize observer back and raises
 * no scroll event, so `grow` sets a row's height, by its `data-testid` or its `data-details-row`,
 * or an entry's by its `data-history-entry`, and `resized` calls every observer back, as the
 * browser does once a row's height has changed; `heightsChanged` calls back only the observers of
 * an element whose height changed, as the browser does; and `frame` is the next frame: the box's
 * scroll event, where the box moved since the last one, then the observers of what changed
 * height.
 */
function drawPeopleInDrawer(path: string, drawn: DetailsDrawn = {}) {
  const log = drawn.log ?? Promise.resolve([]);
  const heights = new Map([
    ["row-above", 200],
    ["row-below", 100],
  ]);
  const observing = new Set<{
    readonly callback: ResizeObserverCallback;
    /** Each element the observer watches, with its height when the observer began to watch it
     *  or was last called back for it. */
    readonly heights: Map<Element, number>;
    readonly observer: ResizeObserver;
  }>();
  const RealResizeObserver = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class {
    readonly #held: {
      readonly callback: ResizeObserverCallback;
      readonly heights: Map<Element, number>;
      readonly observer: ResizeObserver;
    };
    constructor(callback: ResizeObserverCallback) {
      this.#held = { callback, heights: new Map(), observer: this as unknown as ResizeObserver };
    }
    observe(target: Element) {
      observing.add(this.#held);
      this.#held.heights.set(target, target.getBoundingClientRect().height);
    }
    unobserve(target: Element) {
      this.#held.heights.delete(target);
    }
    disconnect() {
      observing.delete(this.#held);
      this.#held.heights.clear();
    }
  } as unknown as typeof ResizeObserver;
  let clock = performance.now();
  const heldNow =
    drawn.heldClock === true ? spyOn(performance, "now").mockReturnValue(clock) : null;
  const rectOf = (top: number, height: number) =>
    ({
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    }) as DOMRect;
  const detailsBox = () => document.querySelector<HTMLElement>('[data-testid="details-box"]');
  /** The height of `element`, which stands in the history's block, laid out as a block: an
   *  entry its own, an element that holds others theirs stacked, else a line where it holds words
   *  and nothing where it holds none. */
  const heightIn = (element: Element): number => {
    if (element.matches(HISTORY_UNITS)) {
      return heights.get(element.getAttribute("data-history-entry") ?? "") ?? ENTRY_HEIGHT;
    }
    if (element.childElementCount === 0) return element.textContent === "" ? 0 : HISTORY_LINE;
    let height = 0;
    for (const child of element.children) height += heightIn(child);
    return height;
  };
  const rowHeight = (row: Element) =>
    row.matches(HISTORY_BLOCK)
      ? heightIn(row)
      : (heights.get(
          row.getAttribute("data-testid") ?? row.getAttribute(DETAILS_ROW_ATTRIBUTE) ?? ""
        ) ?? ROW_HEIGHT);
  /** The top and height of `target` in `block`, whose top is `top`, where `block` holds it. */
  const placeIn = (block: Element, top: number, target: Element): DOMRect => {
    if (block === target || block.matches(HISTORY_UNITS)) return rectOf(top, heightIn(block));
    let at = top;
    for (const child of block.children) {
      if (child.contains(target)) return placeIn(child, at, target);
      at += heightIn(child);
    }
    return rectOf(top, heightIn(block));
  };
  /** What is drawn at `y` in `block`, whose top is `top`: the entry there, else the deepest
   *  element there. */
  const drawnAt = (block: Element, top: number, y: number): Element => {
    if (block.matches(HISTORY_UNITS)) return block;
    let at = top;
    for (const child of block.children) {
      const height = heightIn(child);
      if (y < at + height) return drawnAt(child, at, y);
      at += height;
    }
    return block;
  };
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const box = detailsBox();
    if (box === null) return rectOf(0, 0);
    if (this === box) return rectOf(BOX_TOP, BOX_HEIGHT);
    const start = BOX_TOP - box.scrollTop;
    let top = start;
    for (const row of box.firstElementChild?.children ?? []) {
      if (row.contains(this)) {
        if (row.matches(HISTORY_BLOCK)) return placeIn(row, top, this);
        const head = row.matches(`[${DETAILS_ROW_ATTRIBUTE}]`)
          ? row.firstElementChild?.firstElementChild
          : null;
        return head?.contains(this) === true
          ? rectOf(top, ROW_HEIGHT)
          : rectOf(top, rowHeight(row));
      }
      top += rowHeight(row);
    }
    return rectOf(start, top - start);
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation((x, y) => {
    const box = detailsBox();
    if (box === null || x < 0 || x > 100 || y < BOX_TOP || y >= BOX_TOP + BOX_HEIGHT) {
      return null;
    }
    let top = BOX_TOP - box.scrollTop;
    for (const row of box.firstElementChild?.children ?? []) {
      const height = rowHeight(row);
      if (y < top + height) return row.matches(HISTORY_BLOCK) ? drawnAt(row, top, y) : row;
      top += height;
    }
    return box;
  });
  const intoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element,
    how?: boolean | ScrollIntoViewOptions
  ) {
    const box = detailsBox();
    if (box === null || !box.contains(this)) return;
    const at = this.getBoundingClientRect();
    const bottom = BOX_TOP + BOX_HEIGHT;
    const block =
      typeof how === "object" ? (how.block ?? "start") : how === false ? "end" : "start";
    // `nearest` leaves an element wholly in the view, or over all of it, where it is; else it
    // scrolls by the smaller of the two moves that line an edge of the element up with the same
    // edge of the view: an element above the view no taller than it comes in by its top, and a
    // taller one by its foot, and an element below the view the other way about.
    const over = at.top < BOX_TOP;
    const under = at.bottom > bottom;
    const nearest =
      over === under
        ? 0
        : over
          ? Math.max(at.top - BOX_TOP, at.bottom - bottom)
          : Math.min(at.bottom - bottom, at.top - BOX_TOP);
    const by: Record<ScrollLogicalPosition, number> = {
      center: at.top + at.height / 2 - (BOX_TOP + BOX_HEIGHT / 2),
      end: at.bottom - bottom,
      nearest,
      start: at.top - BOX_TOP,
    };
    box.scrollTop += by[block];
  });
  /** The first element of `parent` the browser's own scroll anchoring anchors on: the first one
   *  wholly in the box's view, looked for inside the first one only partly in it. */
  const anchoredOn = (parent: Element): Element | null => {
    for (const child of parent.children) {
      const at = child.getBoundingClientRect();
      if (at.height === 0 || at.bottom <= BOX_TOP || at.top >= BOX_TOP + BOX_HEIGHT) continue;
      if (whollyInBox(at)) return child;
      return anchoredOn(child) ?? child;
    }
    return null;
  };
  const reads = [
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    spyOn(api, "getIssueCollaborators").mockResolvedValue({
      agents: [],
      issue_key: KEY,
      legion_registry: "ok",
      people: [],
    }),
    spyOn(api, "getIssueEvents").mockImplementation((_key, query) =>
      query?.before !== undefined && drawn.older !== undefined ? drawn.older() : log
    ),
  ];
  const hold: PeopleHold = {};
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={drawn.client ?? readsHeld()}>
        <KeymapProvider>
          <PeopleInDrawer hold={hold} of={drawn.issue ?? issue} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const box = screen.getByTestId("details-box");
  Object.defineProperty(box, "clientHeight", { configurable: true, get: () => BOX_HEIGHT });
  Object.defineProperty(box, "scrollHeight", {
    configurable: true,
    get: () => {
      let height = 0;
      for (const row of box.firstElementChild?.children ?? []) height += rowHeight(row);
      return Math.max(height, BOX_HEIGHT);
    },
  });
  // The offset the box's last scroll event reported, which `frame` raises the next one after.
  let reported = box.scrollTop;
  box.addEventListener("scroll", () => {
    reported = box.scrollTop;
  });
  const control = () => {
    const found = box.querySelector<HTMLElement>('[data-details-row="collaborators"] button');
    if (found === null) throw new Error("Details drew no Collaborators control");
    return found;
  };
  return {
    box,
    control,
    /** Waits for the focus to land on the Collaborators control, and the frames after it. */
    async focused(how: string): Promise<void> {
      await waitFor(() =>
        expect(document.activeElement, `${how} did not focus the Collaborators control`).toBe(
          control()
        )
      );
      await nextFrames(2);
    },
    /** Waits for the history to draw `count` entries. */
    async historyDrawn(count: number): Promise<void> {
      await waitFor(() => expect(box.querySelectorAll("[data-history-entry]")).toHaveLength(count));
    },
    /** Sets the height of `row` - a row of the box by its `data-testid` or its
     *  `data-details-row`, or an entry of the history by its `data-history-entry` - `by` taller,
     *  and moves the box as the browser's own scroll anchoring does across that change, where the
     *  case draws it (`browserAnchoring`). */
    grow(row: string, by: number): void {
      const entry = box.querySelector(`[data-history-entry="${CSS.escape(row)}"]`) !== null;
      const anchoring =
        drawn.browserAnchoring === true && box.style.getPropertyValue("overflow-anchor") !== "none";
      const anchor = anchoring ? anchoredOn(box) : null;
      const before = anchor?.getBoundingClientRect().top ?? 0;
      heights.set(row, (heights.get(row) ?? (entry ? ENTRY_HEIGHT : ROW_HEIGHT)) + by);
      if (anchor !== null) box.scrollTop += anchor.getBoundingClientRect().top - before;
    },
    /** The test's hold on Details: what opens it, and what takes the page to another address. */
    hold,
    /** Moves the held clock on by `ms` (`heldClock`). */
    later(ms: number): void {
      clock += ms;
      heldNow?.mockReturnValue(clock);
    },
    resized(): void {
      act(() => {
        for (const { callback, observer } of [...observing]) callback([], observer);
      });
    },
    /** Calls back each observer an element of whose height changed since it began to watch that
     *  element or was last called back for it, and only those, as the browser does, each entry
     *  with the element's box as it now stands. */
    heightsChanged(): void {
      act(() => {
        for (const held of [...observing]) {
          const changed: ResizeObserverEntry[] = [];
          for (const [target, height] of held.heights) {
            const now = target.getBoundingClientRect();
            if (now.height === height) continue;
            held.heights.set(target, now.height);
            changed.push({ contentRect: now, target } as ResizeObserverEntry);
          }
          if (changed.length > 0) held.callback(changed, held.observer);
        }
      });
    },
    /** The next frame: the box's scroll event, where something moved the box since the last one
     *  was raised, and then the observers of each element whose height changed. */
    frame(): void {
      if (box.scrollTop !== reported) {
        act(() => {
          box.dispatchEvent(new Event("scroll"));
        });
      }
      this.heightsChanged();
    },
    restore(): void {
      view.unmount();
      globalThis.ResizeObserver = RealResizeObserver;
      rect.mockRestore();
      point.mockRestore();
      intoView.mockRestore();
      heldNow?.mockRestore();
      for (const read of reads) read.mockRestore();
    },
  };
}

// `t o` brings the collaborators' row into Details' view and focuses its control; a row above it
// that reads what it shows once Details opens - Blocked by, which reads the titles of the issues
// it names - grows when that read lands, after the focus, and pushed the focused control down out
// of Details' view. The control keeps its own place: Details scrolls by as much as the row grew.
// The browser row "t o and a load of /collaborators keep the focused Collaborators control in
// Details' view when a row above it grows" (`e2e/issue-page-m.e2e.ts`) draws the real rows.
test("after t o, a row above the focused Collaborators control that grows scrolls Details by as much, and the control keeps its place", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("to");
    await page.focused("t o");
    const top = page.control().getBoundingClientRect().top;
    const scrolled = page.box.scrollTop;

    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + GROWTH
    );
    expect(
      page.control().getBoundingClientRect().top,
      "the row above grew and moved the focused Collaborators control"
    ).toBe(top);

    // The browser's own scroll anchoring may have moved the box for a growth already: the place
    // is measured afresh, so that move is made good and not added to.
    page.grow("row-above", GROWTH);
    page.box.scrollTop += GROWTH;
    page.resized();
    expect(
      page.box.scrollTop,
      "a growth the browser had already scrolled for was scrolled for again"
    ).toBe(scrolled + 2 * GROWTH);
    expect(page.control().getBoundingClientRect().top).toBe(top);
  } finally {
    page.restore();
  }
});

// In one frame a row above the focused Collaborators control grows while the row below it shrinks
// by as much, as two reads that land together can: what Details' box holds keeps its height, and
// the one child that holds it all observes no change. The hold watches the content around the
// control at every depth, the rows beside the People rows included, and scrolls Details by the
// growth. Watching only the box's one child, it heard of no change, and the growth pushed the
// control down by as much.
test("after t o, a row above the focused Collaborators control that grows while a row below shrinks by as much in one frame leaves the control where it landed", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("to");
    await page.focused("t o");
    const top = page.control().getBoundingClientRect().top;
    const scrolled = page.box.scrollTop;

    const by = 80;
    page.grow("row-above", by);
    page.grow("row-below", -by);
    page.heightsChanged();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + by
    );
    expect(
      page.control().getBoundingClientRect().top,
      "the row above grew and moved the focused Collaborators control"
    ).toBe(top);
  } finally {
    page.restore();
  }
});

// A load of `/issues/<key>/collaborators` lands the same way as `t o`, and holds the focused
// control's place in the same way.
test("a load of /collaborators keeps the focused Collaborators control's place as a row above it grows", async () => {
  const page = drawPeopleInDrawer(buildIssuePath({ key: KEY, kind: "collaborators" }));
  try {
    await page.focused("a load of /collaborators");
    const top = page.control().getBoundingClientRect().top;
    const scrolled = page.box.scrollTop;

    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + GROWTH
    );
    expect(
      page.control().getBoundingClientRect().top,
      "the row above grew and moved the focused Collaborators control"
    ).toBe(top);
  } finally {
    page.restore();
  }
});

// `t o` pressed again at `/collaborators`, once the reader has read far down Details below a
// collaborators' row taller than Details' view - its list open - leaves the focused Collaborators
// control wholly in the view. The opener brings the whole row into the view (`nearest`), which
// from below shows the row's foot, the control at its head still above the view; the hold then
// found the control wholly outside the view and yielded, so nothing brought it back. The hold
// brings the focused control into the view as the focus lands. Found by the tester on a running
// build after the history's Load older was pressed until it went: the control 151 px above
// Details' view at 1440 px, and 435 px at 390.
test("t o pressed again at /collaborators with Details read far below a collaborators' row taller than its view leaves the focused Collaborators control wholly in the view", async () => {
  const log = claimsNewestFirst(30);
  const page = drawPeopleInDrawer(buildIssuePath({ key: KEY, kind: "collaborators" }), {
    log: Promise.resolve(log),
  });
  try {
    await page.focused("a load of /collaborators");
    await page.historyDrawn(PAGE_SIZE);
    // The list the address opened, drawn under the control, makes its row taller than the view.
    page.grow("collaborators", 2 * BOX_HEIGHT);
    page.heightsChanged();
    // The reader leaves the control and reads down to the end of Details.
    act(() => page.control().blur());
    readerScrollsToEnd(page.box);
    expect(
      page.control().getBoundingClientRect().bottom,
      "the reader's place left the Collaborators control in Details' view, so the case proves nothing"
    ).toBeLessThan(BOX_TOP);

    await userEvent.keyboard("to");
    await page.focused("t o");
    await nextFrames(2);
    page.frame();
    const at = page.control().getBoundingClientRect();
    expect(
      whollyInBox(at),
      `t o left the focused Collaborators control at ${at.top} px, out of Details' view (${BOX_TOP} to ${BOX_TOP + BOX_HEIGHT})`
    ).toBe(true);
  } finally {
    page.restore();
  }
});

// `t v` puts the focus on the history once Details draws it, and the history keeps the place it
// landed at in the same way when a row above it grows after the landing.
test("after t v, a row above the focused history that grows scrolls Details by as much, and the history keeps its place", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("tv");
    const history = await screen.findByTestId("issue-history");
    await waitFor(() =>
      expect(document.activeElement, "t v did not focus the history").toBe(history)
    );
    await nextFrames(2);
    const top = history.getBoundingClientRect().top;
    const scrolled = page.box.scrollTop;

    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + GROWTH
    );
    expect(history.getBoundingClientRect().top, "the row above grew and moved the history").toBe(
      top
    );
  } finally {
    page.restore();
  }
});

// `t v` from a load with Details shut reaches the history while its log is still being read, at
// the end of what Details holds; once the log is drawn, the history's own landing brings its
// heading to the top of Details' view. That landing is the place `t v` asks for, not a move the
// hold makes good: the history stays at the top of the box, and a row above it that grows later
// keeps it there.
test("after t v, the history's landing at the top of Details is the place held as a row above it grows", async () => {
  const log = Promise.withResolvers<DispatchEvent[]>();
  const page = drawPeopleInDrawer(`/issues/${KEY}`, { log: log.promise });
  try {
    await userEvent.keyboard("tv");
    const history = await screen.findByTestId("issue-history");
    await waitFor(() =>
      expect(document.activeElement, "t v did not focus the history").toBe(history)
    );
    await nextFrames(2);
    expect(
      history.getBoundingClientRect().top,
      "the history stood at the top of Details before its log was read, so the case proves nothing"
    ).toBeGreaterThan(BOX_TOP);

    await act(async () => log.resolve([]));
    await waitFor(() =>
      expect(
        history.getBoundingClientRect().top,
        "the history's landing did not bring it to the top of Details"
      ).toBe(BOX_TOP)
    );
    // The frame after the landing observes the history's list drawn under its heading.
    page.resized();
    expect(
      history.getBoundingClientRect().top,
      "the hold took the history's landing for a move to make good and put the history back"
    ).toBe(BOX_TOP);

    const scrolled = page.box.scrollTop;
    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + GROWTH
    );
    expect(
      history.getBoundingClientRect().top,
      "the row above grew and moved the history from where it landed"
    ).toBe(BOX_TOP);
  } finally {
    page.restore();
  }
});

// `t v` pressed again at `/activity`, once the reader has read far down Details, brings the
// history's heading back to the top of Details' view: each press is a following of the address of
// its own (`useFollowing`), and each following lands the heading. The landing once told one
// following from the next by the address alone, the same for every following of `/activity`, so
// after the load's landing the key landed nothing and the heading stayed far above the view.
// Found by the tester on a running build after the history's Load older was pressed until it
// went: the heading 39,585 px above Details' view at 1440 px, and 121 px at 390.
test("t v pressed again at /activity with Details read far down lands the history's heading at the top of Details' view", async () => {
  const log = claimsNewestFirst(30);
  const page = drawPeopleInDrawer(buildIssuePath({ key: KEY, kind: "activity" }), {
    log: Promise.resolve(log),
  });
  try {
    await page.historyDrawn(PAGE_SIZE);
    const history = screen.getByTestId("issue-history");
    const heading = history.querySelector<HTMLElement>("h2");
    if (heading === null) throw new Error("the history drew no heading");
    await waitFor(() =>
      expect(
        heading.getBoundingClientRect().top,
        "the load of /activity did not land the history's heading"
      ).toBe(BOX_TOP)
    );
    // The reader reads down to the end of Details.
    readerScrollsToEnd(page.box);
    expect(
      heading.getBoundingClientRect().bottom,
      "the reader's place left the history's heading in Details' view, so the case proves nothing"
    ).toBeLessThan(BOX_TOP);

    await userEvent.keyboard("tv");
    await waitFor(() =>
      expect(document.activeElement, "t v did not focus the history").toBe(history)
    );
    await nextFrames(2);
    page.frame();
    expect(
      heading.getBoundingClientRect().top,
      "t v at /activity left the history's heading out of the top of Details' view"
    ).toBe(BOX_TOP);
  } finally {
    page.restore();
  }
});

// The hold lasts while the focus stays on the control: a key or a turn of the wheel that leaves it
// there leaves the hold on, and a row above that grows later scrolls Details by as much.
test("the hold survives a key and a turn of the wheel, and a later growth above the control scrolls Details by as much", async () => {
  const acts: readonly (readonly [string, (box: HTMLElement) => Promise<void>])[] = [
    [
      "a key",
      async () => {
        await userEvent.keyboard("{Shift}");
      },
    ],
    [
      "a turn of the wheel",
      async (box) => {
        await act(async () => {
          box.dispatchEvent(new WheelEvent("wheel", { bubbles: true, deltaY: 40 }));
        });
      },
    ],
  ];
  for (const [how, readerActs] of acts) {
    const page = drawPeopleInDrawer(`/issues/${KEY}`);
    try {
      await userEvent.keyboard("to");
      await page.focused("t o");
      const top = page.control().getBoundingClientRect().top;
      const scrolled = page.box.scrollTop;

      await readerActs(page.box);
      page.grow("row-above", GROWTH);
      page.resized();
      expect(
        page.box.scrollTop,
        `after ${how}, the row above grew and Details did not scroll by as much`
      ).toBe(scrolled + GROWTH);
      expect(page.control().getBoundingClientRect().top).toBe(top);
    } finally {
      page.restore();
    }
  }
});

/** The reader scrolling Details' box by `by`, and the box's scroll event that reports it, which
 *  happy-dom does not raise for a change of `scrollTop`. */
function readerScrolls(box: HTMLElement, by: number): void {
  act(() => {
    box.scrollTop += by;
    box.dispatchEvent(new Event("scroll"));
  });
}

/** The reader scrolling Details' box down to the end of what it holds (`readerScrolls`). */
function readerScrollsToEnd(box: HTMLElement): void {
  readerScrolls(box, box.scrollHeight - BOX_HEIGHT - box.scrollTop);
}

/** The pointer coming to rest on `entry` at `point` of the page: the `pointerover` and the
 *  `pointermove` the browser raises there. */
function restPointerOn(
  entry: HTMLElement,
  point: { readonly x: number; readonly y: number }
): void {
  act(() => {
    const at = { bubbles: true, clientX: point.x, clientY: point.y };
    entry.dispatchEvent(new PointerEvent("pointerover", at));
    entry.dispatchEvent(new PointerEvent("pointermove", at));
  });
}

/** The entry of the history above `entry` growing by `GROWTH` on `page`, and the observers of its
 *  height called back, as the browser calls them once a height has changed. */
function growEntryAbove(
  page: { grow(row: string, by: number): void; heightsChanged(): void },
  entry: HTMLElement
): void {
  const over = entry.previousElementSibling?.getAttribute("data-history-entry");
  if (typeof over !== "string") throw new Error("no entry above the reader's");
  page.grow(over, GROWTH);
  page.heightsChanged();
}

// A scroll of Details the hold did not make is the reader's view of the control from then on:
// the control is kept where that scroll left it, not put back where it landed.
test("a scroll the reader makes moves the place the hold keeps to where the control then stands", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("to");
    await page.focused("t o");
    const landed = page.control().getBoundingClientRect().top;

    readerScrolls(page.box, 100);
    const seen = page.control().getBoundingClientRect().top;
    expect(seen).toBe(landed - 100);
    const scrolled = page.box.scrollTop;
    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + GROWTH
    );
    expect(
      page.control().getBoundingClientRect().top,
      "the hold put the control back where it landed, undoing the reader's scroll"
    ).toBe(seen);
  } finally {
    page.restore();
  }
});

// The hold's own scroll raises its event a frame later, at the offset the hold left the box at.
// That event is no scroll of the reader's: here the row above grows again before it, and the
// change is observed after it, so taking the event for the reader's would keep the control where
// that second growth pushed it.
test("the hold's own scroll is not taken for the reader's", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("to");
    await page.focused("t o");
    const top = page.control().getBoundingClientRect().top;
    const scrolled = page.box.scrollTop;

    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop).toBe(scrolled + GROWTH);
    page.grow("row-above", GROWTH);
    act(() => {
      page.box.dispatchEvent(new Event("scroll"));
    });
    page.resized();
    expect(
      page.control().getBoundingClientRect().top,
      "the hold took its own scroll for the reader's and kept the control where a growth pushed it"
    ).toBe(top);
    expect(page.box.scrollTop).toBe(scrolled + 2 * GROWTH);
  } finally {
    page.restore();
  }
});

// A scroll that leaves the control wholly outside Details' view lets the hold yield: a row above
// that grows then scrolls nothing, so the rows the reader scrolled to stay where they are. A
// scroll that finds the control in the view again takes it as the place once more.
test("a control scrolled wholly out of Details' view is not held, and is held again once a scroll finds it in view", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("to");
    await page.focused("t o");
    const landed = page.control().getBoundingClientRect();

    // Down past the control, until its foot stands above the box's top.
    readerScrolls(page.box, landed.bottom - BOX_TOP + 100);
    expect(page.control().getBoundingClientRect().bottom).toBeLessThan(BOX_TOP);
    const scrolled = page.box.scrollTop;
    page.grow("row-above", GROWTH);
    page.resized();
    expect(
      page.box.scrollTop,
      "the hold scrolled Details for a control wholly out of its view"
    ).toBe(scrolled);

    // The growth brought the control back into the view; the reader's next scroll finds it there.
    readerScrolls(page.box, -10);
    const seen = page.control().getBoundingClientRect().top;
    expect(seen).toBeGreaterThan(BOX_TOP);
    page.grow("row-above", GROWTH);
    page.resized();
    expect(
      page.control().getBoundingClientRect().top,
      "back in view, the control was not held again"
    ).toBe(seen);
  } finally {
    page.restore();
  }
});

// A row below the focused control that grows moves nothing in view, held or not; a growth above
// it afterwards shows the hold was there all along.
test("a row below the focused Collaborators control that grows scrolls nothing", async () => {
  const page = drawPeopleInDrawer(`/issues/${KEY}`);
  try {
    await userEvent.keyboard("to");
    await page.focused("t o");
    const top = page.control().getBoundingClientRect().top;
    const scrolled = page.box.scrollTop;

    page.grow("row-below", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "a growth below the control scrolled Details").toBe(scrolled);
    expect(page.control().getBoundingClientRect().top).toBe(top);

    page.grow("row-above", GROWTH);
    page.resized();
    expect(page.box.scrollTop, "the row above grew and Details did not scroll by as much").toBe(
      scrolled + GROWTH
    );
    expect(page.control().getBoundingClientRect().top).toBe(top);
  } finally {
    page.restore();
  }
});

/** A claim of the issue made `secondsAgo` seconds before the cases' midday. */
function claimMade(seq: number, secondsAgo: number): DispatchEvent {
  return { ...claim(seq), created_at: new Date(NOON.getTime() - secondsAgo * 1000).toISOString() };
}

/** A log of `count` claims, newest first, each made 9 s before the one listed above it: the
 *  agents' thread, a minute old (`AGENTS_THREAD`), stands in the history under the newest six. */
function claimsNewestFirst(count: number): DispatchEvent[] {
  return Array.from({ length: count }, (_, index) => claimMade(count - index, (index + 1) * 9));
}

/** A full first page of the log, 200 events, whose entries in the history are `entries` claims
 *  (`claimsNewestFirst`) and the agents' thread: the rest are moves of a child's status, which
 *  the outline draws and the history leaves out. With no more entries than one press of Show
 *  more draws, every entry read is drawn, and Show more reads the log's next page. */
function fullPageOfFewEntries(entries: number): DispatchEvent[] {
  const left = Array.from(
    { length: 200 - entries },
    (_, index) =>
      ({ ...claimMade(1000 + index, 1), payload: {}, type: "child.status" }) as DispatchEvent
  );
  return [...left, ...claimsNewestFirst(entries)];
}

/** How long the case lets pass for a scroll not to be taken for the reader's own: past the
 *  window after a wheel turn, a key or a press in which `ViewportAnchor` still takes one for
 *  theirs (`READER_SCROLL_MS`, 500 ms). */
const READER_ACT_PASSED_MS = 1000;

/** Where the reader had scrolled Details to, in a history of 30 claims, before a landing: the
 *  history fills the view, and the People rows are above it. */
const READER_SCROLLED_TO = 1000;

/** The entry of a history of 30 claims the reader's earlier place names, the eighteenth line of
 *  its list: in the middle of the view they scrolled to, and wholly below the view once any of
 *  the landings has scrolled Details up to what it lands at. */
const READER_ENTRY = 17;

/** How far an entry grows while another shrinks by as much (`split` below): no more than an
 *  entry is tall. */
const SPLIT = 40;

/** The earlier places the reader can have left in the history before a key or an address lands
 *  them elsewhere in Details. */
type EarlierPlace = "older" | "pin" | "pointer" | "scroll";

/**
 * Draws Details with the history of `place`'s case open in it, and the reader's place left in
 * that history by `place`: a log of 30 claims and the agents' thread, or, where the reader
 * pressed Show more to read the log's next page, a full page of 200 events that holds 19 claims
 * and the thread, with older pages left to read, whose next page is never answered while the case
 * runs. The box anchors its scroll as the browser does while its own scroll anchoring is on, and
 * the clock the reader's acts are read by stands still until a case moves it on. Returns the
 * page, the entry the reader's place names, and the point of the page the reader looked at: that
 * entry's middle.
 */
async function readerLeftAPlace(place: EarlierPlace) {
  const { client, getIssueCoverage, names } = agentsThreadClient();
  const log = place === "older" ? fullPageOfFewEntries(PAGE_SIZE - 1) : claimsNewestFirst(30);
  const older = Promise.withResolvers<DispatchEvent[]>();
  const reads = [
    getIssueCoverage,
    names,
    // A pin reads the reader's state of the issue, and writes it with the line pinned.
    spyOn(api, "getMyState").mockResolvedValue(READ_THROUGH_TWO),
    spyOn(api, "putIssueState").mockResolvedValue({
      dismissed: [],
      last_read_seq: 2,
      pinned: false,
      seq: 1,
    } as never),
  ];
  const page = drawPeopleInDrawer(`/issues/${KEY}`, {
    browserAnchoring: true,
    client,
    heldClock: true,
    issue: { ...issueDetails(), artifacts: [] },
    log: Promise.resolve(log),
    older: () => older.promise,
  });
  const restore = () => {
    page.restore();
    older.resolve([]);
    for (const read of reads) read.mockRestore();
  };
  try {
    act(() => page.hold.open?.(true));
    // Each claim is an entry, and so is the agents' thread: the first of them are drawn.
    await page.historyDrawn(PAGE_SIZE);
    const entries = [...page.box.querySelectorAll<HTMLElement>("[data-history-entry]")];
    const entry =
      place === "older"
        ? entries.at(-1)
        : page.box.querySelectorAll<HTMLElement>(HISTORY_UNITS).item(READER_ENTRY);
    if (entry === undefined || entry === null) throw new Error("the history drew no such entry");
    // The reader turns the wheel down the history: to the middle of it, or to its end, where Show
    // more is.
    const to = place === "older" ? page.box.scrollHeight - BOX_HEIGHT : READER_SCROLLED_TO;
    act(() => {
      page.box.dispatchEvent(new WheelEvent("wheel", { bubbles: true, deltaY: to }));
    });
    readerScrolls(page.box, to - page.box.scrollTop);
    const seen = entry.getBoundingClientRect();
    expect(whollyInBox(seen), "the reader's entry is not in the view they scrolled to").toBe(true);
    const point = { x: 50, y: seen.top + seen.height / 2 };
    if (place === "pin") {
      const pin = within(entry).getByRole("button", { name: CONTROL_LABELS.pin });
      act(() => pin.focus());
      await userEvent.keyboard("{Enter}");
      await waitFor(() => expect(api.putIssueState).toHaveBeenCalledTimes(1));
    } else if (place === "older") {
      const more = screen.getByRole("button", { name: CONTROL_LABELS.show_more });
      act(() => more.focus());
      await userEvent.keyboard("{Enter}");
      await waitFor(() => expect(more).toHaveAttribute("aria-busy", "true"));
    } else if (place === "pointer") {
      restPointerOn(entry, point);
    }
    return { entry, page, point, restore };
  } catch (error) {
    restore();
    throw error;
  }
}

/** A landing of the sweep below: what puts the reader at a place of Details from the place they
 *  left in the history. */
type Landing = "t o" | "t v" | "turn";

/** A change of height of the sweep below, made in the landing's frame. */
type Growth = "above" | "between" | "split";

const SWEEP_LANDINGS: readonly (readonly [Landing, string])[] = [
  ["t o", "t o"],
  ["t v", "t v"],
  ["turn", "an agents' turn's address"],
];
const SWEEP_PLACES: readonly (readonly [EarlierPlace, string])[] = [
  ["pin", "a history line's Pin pressed by keyboard"],
  ["older", "Show more pressed to read the log's next page"],
  ["scroll", "a scroll of Details with the pointer resting on nothing"],
  ["pointer", "the pointer resting on an entry"],
];
const SWEEP_GROWTHS: readonly (readonly [Growth, string])[] = [
  ["between", "the entry above the reader's entry, between the landed control and it, grows"],
  ["above", "a row above the view grows"],
  ["split", "an entry in the view grows while one two lines under it shrinks by as much"],
];
const SWEEP_ORDERS: readonly (readonly ["after" | "before", string])[] = [
  ["before", "before the landing scroll's own scroll event"],
  ["after", "after the landing scroll's own scroll event"],
];

// Every landing - `t o`, `t v`, and an agents' turn's address, which marks the turn - from every
// place the reader can have left in the history, with a change of height in the landing's frame,
// observed before and after the scroll event of the landing's own scroll: the control the landing
// put the reader at stays where it landed, wholly in view, and the focus with it. The two keepers
// of Details' box keep one place between them: the hold keeps the control `t o` and `t v` focus,
// and while it holds, the history's anchor keeps nothing there (`claimBox`); the history keeps the
// turn an address lands it at (`keepRow`). Neither keeps a row the reader cannot see: the place
// the reader left is wholly below the view once the landing has scrolled Details up, and a change
// of height between the view and that place moves nothing in view (`rowToKeep`).
//
// `t o` after the pointer resting on an entry, with a row above the view that grows before the
// landing scroll's own scroll event, is where two keepers made good one growth twice. The hold
// keeps the control's place in Details' box, where the history's anchor kept the reader's place
// before the landing: the entry the pointer rests on. The hold scrolls Details by the growth in
// the landing frame's observation of it, and the landing's own scroll and the hold's raise one
// scroll event in the next frame. The anchor, keeping its entry too, read that event as one
// scroll with the growth on top of it, and moved Details by the growth a second time: the
// control went up out of the view by as much. The control's top moves only with Details' scroll
// and the rows above it, so a top kept where it landed is Details scrolled by exactly the growth.
for (const [landing, landingName] of SWEEP_LANDINGS) {
  for (const [place, placeName] of SWEEP_PLACES) {
    for (const [growth, growthName] of SWEEP_GROWTHS) {
      for (const [order, orderName] of SWEEP_ORDERS) {
        const name = `after ${placeName}, ${landingName} keeps where it landed when ${growthName}, ${orderName}`;
        test(name, async () => {
          const { entry, page, point, restore } = await readerLeftAPlace(place);
          try {
            let landed: HTMLElement;
            if (landing === "t o") {
              await userEvent.keyboard("to");
              await page.focused("t o");
              landed = page.control();
            } else if (landing === "t v") {
              await userEvent.keyboard("tv");
              const history = screen.getByTestId("issue-history");
              await waitFor(() =>
                expect(document.activeElement, "t v did not focus the history").toBe(history)
              );
              await nextFrames(2);
              // The history's top line, which its landing brings to the top of Details' view.
              const heading = history.querySelector<HTMLElement>("h2");
              if (heading === null) throw new Error("the history drew no heading");
              landed = heading;
            } else {
              // The address is followed a while after the reader's last act: its landing's scroll
              // is no scroll of theirs.
              page.later(READER_ACT_PASSED_MS);
              const id = AGENTS_THREAD.message.id;
              act(() => page.hold.navigate?.(buildIssuePath({ id, key: KEY, kind: "message" })));
              const turn = await waitFor(() => {
                const marked = page.box.querySelector<HTMLElement>(`[data-history-item="${id}"]`);
                expect(marked?.getAttribute("aria-current"), "the address marked no turn").toBe(
                  "true"
                );
                return marked as HTMLElement;
              });
              await nextFrames(2);
              landed = turn;
            }
            const focused = document.activeElement;
            const top = landed.getBoundingClientRect().top;
            expect(
              entry.getBoundingClientRect().top,
              "the landing left the reader's entry in the view, so the case proves nothing"
            ).toBeGreaterThanOrEqual(BOX_TOP + BOX_HEIGHT);

            const grows = () => {
              if (growth === "above") {
                page.grow("row-above", GROWTH);
                return;
              }
              if (growth === "between") {
                const above = entry.previousElementSibling?.getAttribute("data-history-entry");
                if (above === null || above === undefined) throw new Error("no entry above");
                page.grow(above, GROWTH);
                return;
              }
              // The entry at the point the reader looked at before the landing, which the
              // pointer rests on where it rests: the one above it grows, the one under it shrinks.
              const at = document.elementFromPoint(point.x, point.y)?.closest(HISTORY_UNITS);
              const over = at?.previousElementSibling?.getAttribute("data-history-entry");
              const under = at?.nextElementSibling?.getAttribute("data-history-entry");
              if (typeof over !== "string" || typeof under !== "string") {
                throw new Error("no entries around the reader's point");
              }
              page.grow(over, SPLIT);
              page.grow(under, -SPLIT);
            };
            if (order === "before") {
              grows();
              page.heightsChanged();
              page.frame();
            } else {
              page.frame();
              grows();
              page.heightsChanged();
            }
            page.frame();

            const at = landed.getBoundingClientRect();
            expect(at.top, "what the landing put the reader at moved").toBe(top);
            expect(
              whollyInBox(at),
              "what the landing put the reader at is not wholly in view"
            ).toBe(true);
            expect(document.activeElement, "the focus moved").toBe(focused);
          } finally {
            restore();
          }
        });
      }
    }
  }
}

/** How the reader comes to read an entry of the history after `t v`, in the cases below. */
type ReaderOnEntry = "keys" | "pointer";

/** What happens above the reader's entry, in the cases below. */
type AboveTheEntry = "arrives" | "grows";

const READER_ON_ENTRY: readonly (readonly [ReaderOnEntry, string])[] = [
  ["pointer", "the pointer resting on the fourth entry under the heading"],
  ["keys", "a scroll the reader's keys make with the focus left on the history"],
];
const ABOVE_THE_ENTRY: readonly (readonly [AboveTheEntry, string])[] = [
  ["arrives", "a new event arrives at the head of the log"],
  ["grows", "the entry above the reader's grows"],
];

/** The entry of the history the reader reads after `t v`: the fourth line of its list. */
const ENTRY_UNDER_HEADING = 3;

// `t v` lands the history's heading at the top of Details' view and holds the history's place
// there, claiming Details' box from the history's anchor (`claimBox`). The reader then reads an
// entry of the history: the pointer moves onto it, or their keys scroll Details while the focus
// stays on the history. That act names their place in the history: the hold ends, and the
// history's anchor keeps the entry from then on, the one under the pointer or the one in the
// middle of the view. An event that arrives at the head of the log, or an entry above the
// reader's that grows, then leaves the reader's entry where they see it. While the hold kept the
// history's top for the whole read, the anchor kept nothing, and the entry went down by the new
// line, or by the growth.
for (const [reader, readerName] of READER_ON_ENTRY) {
  for (const [above, aboveName] of ABOVE_THE_ENTRY) {
    test(`after t v and ${readerName}, the reader's entry keeps its place when ${aboveName}`, async () => {
      const client = readsHeld();
      const log = claimsNewestFirst(30);
      const page = drawPeopleInDrawer(`/issues/${KEY}`, {
        client,
        heldClock: true,
        log: Promise.resolve(log),
      });
      try {
        act(() => page.hold.open?.(true));
        await page.historyDrawn(PAGE_SIZE);
        await userEvent.keyboard("tv");
        const history = screen.getByTestId("issue-history");
        await waitFor(() =>
          expect(document.activeElement, "t v did not focus the history").toBe(history)
        );
        await nextFrames(2);
        // The frame that reports the landing's own scroll.
        page.frame();
        expect(
          history.querySelector("h2")?.getBoundingClientRect().top,
          "t v did not land the history's heading at the top of Details"
        ).toBe(BOX_TOP);
        const entry = page.box
          .querySelectorAll<HTMLElement>(HISTORY_UNITS)
          .item(ENTRY_UNDER_HEADING);
        if (reader === "pointer") {
          const seen = entry.getBoundingClientRect();
          restPointerOn(entry, { x: 50, y: seen.top + seen.height / 2 });
        } else {
          // The arrow key scrolls Details down by a line of the browser's, the focus left on the
          // history.
          await userEvent.keyboard("{ArrowDown}");
          readerScrolls(page.box, 2 * HISTORY_LINE);
        }
        page.frame();
        const seen = entry.getBoundingClientRect();
        expect(whollyInBox(seen), "the reader's entry is not in the view").toBe(true);

        if (above === "arrives") {
          act(() => prependEventToLog(client, claimMade(log.length + 1, 1)));
          await page.historyDrawn(PAGE_SIZE + 1);
        } else {
          growEntryAbove(page, entry);
        }
        page.frame();

        expect(entry.getBoundingClientRect().top, "the reader's entry moved").toBe(seen.top);
        expect(document.activeElement, "the focus moved").toBe(history);
      } finally {
        page.restore();
      }
    });
  }
}

// `t o` holds the place of the focused Collaborators control, outside the history, and claims
// Details' box for it: a key or a turn of the wheel that names no place leaves that hold on. The
// pointer moving onto an entry of the history names the reader's place there: the hold ends, and
// the history's anchor keeps that entry, so an entry above it that grows leaves it where the
// reader sees it. While the hold kept the control instead, the growth pushed the entry down by as
// much.
test("after t o and the pointer resting on an entry of the history, the reader's entry keeps its place when the entry above the reader's grows", async () => {
  const log = claimsNewestFirst(30);
  const page = drawPeopleInDrawer(`/issues/${KEY}`, { heldClock: true, log: Promise.resolve(log) });
  try {
    act(() => page.hold.open?.(true));
    await page.historyDrawn(PAGE_SIZE);
    await userEvent.keyboard("to");
    await page.focused("t o");
    page.frame();
    const entry = page.box.querySelectorAll<HTMLElement>(HISTORY_UNITS).item(ENTRY_UNDER_HEADING);
    const seen = entry.getBoundingClientRect();
    expect(
      whollyInBox(seen),
      "t o left the entry out of the view, so the case proves nothing"
    ).toBe(true);
    restPointerOn(entry, { x: 50, y: seen.top + seen.height / 2 });
    page.frame();

    growEntryAbove(page, entry);
    page.frame();

    expect(entry.getBoundingClientRect().top, "the reader's entry moved").toBe(seen.top);
    expect(document.activeElement, "the focus moved").toBe(page.control());
  } finally {
    page.restore();
  }
});
