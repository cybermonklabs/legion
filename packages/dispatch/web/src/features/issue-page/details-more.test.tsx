import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
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
import { issueResultsQuery, issueStepsQuery, userStateQuery } from "../../api/queries";
import type {
  Comment,
  CommentDelivery,
  DispatchEvent,
  IssueCoverage,
  IssueDetails,
  MessageDelivery,
  MessageRead,
  Subscriber,
  UserState,
} from "../../api/types";
import { answeredWithErrorGuidance, queuedWait } from "../conversation/delivery";
import { conversationItemId, pinnedItemMarker } from "../issue/pins";
import { buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { issueDetails } from "./decisions-filter/decisions-fixtures";
import { DetailsFocusedNode, DetailsHistory, DetailsPeople } from "./details-more";
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
    title: OWN_COMMENT.body,
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

    // A read that fails once the row holds an answer keeps what it holds: the count and the
    // agents, with the failure and its Retry beside them.
    await userEvent.click(subscribers);
    await screen.findByRole("button", { name: "Unsubscribe" });
    getIssueSubscribers.mockRejectedValueOnce(
      new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" })
    );
    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
    expect((await screen.findByRole("alert")).textContent).toBe(STATE_LINES.subscribers_unreadable);
    expect(subscribers.textContent, "a failed refetch dropped the count it held").toBe(
      `${PROPERTY_LABELS.subscribers}:1`
    );
    expect(
      screen.queryByRole("button", { name: "Unsubscribe" }),
      "a failed refetch dropped the agents it held"
    ).not.toBeNull();
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBeDefined();
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

// With the agents held, a failed read of them keeps its line and its Retry drawn while that
// Retry reads them again, so the Retry is never disabled then: Chromium would drop its focus to
// the page (`chromium-focus-fixup.ts`), and a read that failed again would leave it there.
test("a subscribers' Retry pressed by keyboard over the agents held keeps the focus when its read fails again", async () => {
  const refusal = () =>
    new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" });
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([SUBSCRIBER]);
  const restoreFocus = emulateChromiumFocusFixup();
  const view = renderPeople();
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    await userEvent.click(subscribers);
    await screen.findByRole("button", { name: "Unsubscribe" });
    getIssueSubscribers.mockRejectedValueOnce(refusal());
    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<Subscriber[]>();
    getIssueSubscribers.mockImplementationOnce(() => again.promise);

    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(getIssueSubscribers).toHaveBeenCalledTimes(3));
    expect(
      document.activeElement,
      "the subscribers' Retry left the focus on the page while it read"
    ).toBe(retry);

    again.reject(refusal());
    await waitFor(() =>
      expect(view.client.getQueryState(["subscribers", KEY])?.fetchStatus).toBe("idle")
    );
    expect(screen.getByRole("alert").textContent).toBe(STATE_LINES.subscribers_unreadable);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBe(retry);
    expect(
      document.activeElement,
      "the subscribers' Retry whose read failed again left the focus on the page"
    ).toBe(retry);
    expect(screen.queryByRole("button", { name: "Unsubscribe" })).not.toBeNull();
  } finally {
    restoreFocus();
    view.unmount();
    getIssueSubscribers.mockRestore();
  }
});

// A second press of the subscribers' Retry made before React draws the read the first one sent
// found the render's `isFetching` still false, so it read the agents again: a refetch of a read
// that holds an answer ends the request that is out and sends another. A press while the read is
// out joins it.
test("two presses of the subscribers' Retry before a render send one read", async () => {
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([SUBSCRIBER]);
  const view = renderPeople();
  try {
    const subscribers = await screen.findByRole("button", { name: SUBSCRIBERS });
    await waitFor(() => expect(subscribers.textContent).toBe(`${PROPERTY_LABELS.subscribers}:1`));
    getIssueSubscribers.mockRejectedValueOnce(
      new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "envoy listener unreachable" })
    );
    await act(() => view.client.invalidateQueries({ queryKey: ["subscribers", KEY] }));
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
    ).toHaveBeenCalledTimes(3);
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await act(async () => again.resolve([SUBSCRIBER]));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(getIssueSubscribers).toHaveBeenCalledTimes(3);
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

// `/issues/<key>/collaborators` opens Details once per arrival at it (`DetailsHistory`), and
// the collaborators' list with it: a reader who closed the list there and arrives at the same
// address again - `t o`, or a link to the address they are on - finds it open.
test("each arrival at the collaborators' address opens their list, though the reader closed it", async () => {
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
      "an arrival at the collaborators' address again left their list shut"
    ).toBe("true");
  } finally {
    view.unmount();
    getIssueSubscribers.mockRestore();
    getIssueCollaborators.mockRestore();
  }
});

// Under "The row you are on", the facts of the row the reader is on:
// its kind with its first line, its state where its kind has one, and who wrote it.
test("the facts name the focused node's kind, first line, state and author", async () => {
  const question: FocusedNode = {
    author: { id: "bob", kind: "user" },
    id: "ask-1",
    state: NODE_STATE_WORDS.ask.open,
    title: "Which store holds the drafts?",
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
      [`${sentenceCase(NODE_TYPE_NAMES.ask)}:`, "Which store holds the drafts?"],
      [`${PROPERTY_LABELS.status}:`, NODE_STATE_WORDS.ask.open],
      [`${CONTROL_LABELS.written_by}:`, "bob"],
    ]);
  } finally {
    view.unmount();
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
    title: comment.body,
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

    // A mention sent to a role is named by the session that took it, and one no session took
    // by the page's word for an agent: an address is no name.
    const roled = bobsComment("comment-2", {
      deliveries: [
        attempt("role:legion-p-core-1-tester", { session_id: "tester-7" }),
        attempt("role:legion-p-core-1-reviewer", {}),
      ],
    });
    listComments.mockResolvedValue([roled]);
    await act(() => client.invalidateQueries({ queryKey: ["comments", KEY] }));
    await waitFor(() =>
      expect(
        document.querySelector('[data-deliveries] [data-agent-name="tester-7"]'),
        "the mention's delivery is not named by the session it reached"
      ).not.toBeNull()
    );
    const [reached, unheld] = [...document.querySelectorAll<HTMLElement>("[data-deliveries] > li")];
    expect(reached?.textContent).not.toContain("role:");
    expect(unheld?.textContent).toStartWith(`${CONTROL_LABELS.unnamed_recipient} · `);
    // The failed attempt's Retry went once the read after it landed, and the focus with it: it
    // is on the thread's Resolve rather than on the page.
    expect(document.activeElement, "the focus fell to the page with the delivery's Retry").toBe(
      screen.getByRole("button", { name: CONTROL_LABELS.resolve })
    );

    // A message's deliveries the same, its Retry sent again for that message.
    view.rerender(
      drawn({
        author: thread.message.author,
        id: "message-1",
        item: { id: "message-1", kind: "message" },
        title: thread.message.body,
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

/** What opens and shuts Details around the history, and moves the reader's read mark: the
 *  test's hold on `HistoryInDrawer`. */
interface DrawerHold {
  open?: (open: boolean) => void;
  read?: (state: UserState) => void;
}

/** The history in Details as the page draws it: inside the drawer, which starts shut, over the
 *  reader's state of the issue, read through its second event. `of` is the issue, the fixture's
 *  `issue` unless a case gives one. */
function HistoryInDrawer({ hold, of = issue }: { hold: DrawerHold; of?: IssueDetails }): ReactNode {
  const [open, setOpen] = useState(false);
  const [state, setState] = useState(() => readTo(2));
  hold.open = setOpen;
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

/** A thread only an agent wrote in, one message written a minute ago: no row of the outline,
 *  an entry of the history (`agents-only.ts`). It is built when the file loads, before any case
 *  sets the clock, so it is dated from the cases' midday rather than from `Date.now()`. */
const AGENTS_THREAD: MessageRead = {
  message: {
    author: { id: "planner", kind: "session" },
    body: "The agents' own words.",
    broadcast_id: null,
    created_at: new Date(NOON.getTime() - 60_000).toISOString(),
    deliveries: [],
    ...messageCorrectionFields(),
    id: "message-agents",
    in_reply_to: null,
    issue_key: KEY,
    reply_block: null,
    target: "role:legion-p-core-1-reviewer",
  },
  replies: [],
};

/** `readsHeld()`'s client with the agents' thread as the issue's one thread of messages
 *  (`AGENTS_THREAD`), and the issue's results and decisions, which the outline lands by too,
 *  held empty; and the spy that answers each read of the names the page's words reference with
 *  none, which the case restores. */
function agentsThreadClient() {
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const client = readsHeld();
  client.setQueryData(["issue-messages", KEY], [AGENTS_THREAD]);
  client.setQueryData(issueResultsQuery(KEY).queryKey, { checks: [] } as never);
  client.setQueryData(["decisions", KEY], { decisions: [], document: null, open_questions: [] });
  return { client, names };
}

// The address of a turn only agents wrote in opens Details at the history with that turn marked
// by the outline's own decision (`address-target.ts`'s `useAddressFallback`): once every read the
// outline's tree is built from has answered, as the outline lands on the turn's nearest drawn
// ancestor. While one of those reads is out the outline has landed nowhere, and Details stays
// shut. Details used to decide from the comments and the messages alone, and opened at the turn
// while the outline still waited.
test("an agents' turn's address opens Details at it once every read the outline lands by has answered", async () => {
  const coverage = Promise.withResolvers<IssueCoverage>();
  const getIssueCoverage = spyOn(api, "getIssueCoverage").mockImplementation(
    () => coverage.promise
  );
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const { client, names } = agentsThreadClient();
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
        document.querySelector(`[data-history-item="${id}"]`)?.getAttribute("aria-current"),
        "the agents' turn's address did not open Details with the turn marked"
      ).toBe("true")
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
 *  heading, a day's line, the loading line, Load older. */
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
  /** The answer to each read of a next page of the log, which Load older sends. */
  older?: () => Promise<DispatchEvent[]>;
}

/** Waits `count` frames of the page. */
async function nextFrames(count: number): Promise<void> {
  for (let frame = 0; frame < count; frame += 1) await act(nextFrame);
}

/**
 * `PeopleInDrawer` at `path`, laid out as Details stacks its rows: each row of the box's content
 * under the one before, from the box's top, moved up by the box's scroll, and each element as
 * the row it stands in. The history's block is laid out inside its row as blocks stack: each
 * entry of the history under the one before (`HISTORY_UNITS`), everything inside an entry as the
 * entry, and each line outside them one `HISTORY_LINE` tall. The box's view is `BOX_HEIGHT`
 * tall (`clientHeight`), what it scrolls is its content's height (`scrollHeight`), a point of
 * the page names what is drawn there (`elementFromPoint`), and `scrollIntoView` scrolls the box
 * as the browser does. happy-dom lays nothing out, never calls a resize observer back and raises
 * no scroll event, so `grow` sets a row's height, or an entry's by its `data-history-entry`, and
 * `resized` calls every observer back, as the browser does once a row's height has changed;
 * `heightsChanged` calls back only the observers of an element whose height changed, as the
 * browser does; and `frame` is the next frame: the box's scroll event, where the box moved since
 * the last one, then the observers of what changed height.
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
      : (heights.get(row.getAttribute("data-testid") ?? "") ?? ROW_HEIGHT);
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
        return row.matches(HISTORY_BLOCK) ? placeIn(row, top, this) : rectOf(top, rowHeight(row));
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
    // `nearest` leaves an element wholly in the view, or over all of it, where it is.
    const over = at.top < BOX_TOP;
    const under = at.bottom > bottom;
    const nearest =
      over === under ? 0 : over ? at.top - BOX_TOP : Math.min(at.bottom - bottom, at.top - BOX_TOP);
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
    /** Sets the height of `row` - a row of the box by its `data-testid`, or an entry of the
     *  history by its `data-history-entry` - `by` taller, and moves the box as the browser's own
     *  scroll anchoring does across that change, where the case draws it (`browserAnchoring`). */
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
 * pressed Load older, of 200, a full page with older ones left to read, whose next page is never
 * answered while the case runs. The box anchors its scroll as the browser does while its own
 * scroll anchoring is on, and the clock the reader's acts are read by stands still until a case
 * moves it on. Returns the page, the entry the reader's place names, and the point of the page
 * the reader looked at: that entry's middle.
 */
async function readerLeftAPlace(place: EarlierPlace) {
  const { client, names } = agentsThreadClient();
  const log = claimsNewestFirst(place === "older" ? 200 : 30);
  const older = Promise.withResolvers<DispatchEvent[]>();
  const reads = [
    spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
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
    // Each claim is an entry, and so is the agents' thread.
    await waitFor(() =>
      expect(page.box.querySelectorAll("[data-history-entry]")).toHaveLength(log.length + 1)
    );
    const entries = [...page.box.querySelectorAll<HTMLElement>("[data-history-entry]")];
    const entry =
      place === "older"
        ? entries.at(-1)
        : page.box.querySelectorAll<HTMLElement>(HISTORY_UNITS).item(READER_ENTRY);
    if (entry === undefined || entry === null) throw new Error("the history drew no such entry");
    // The reader turns the wheel down the history: to the middle of it, or to its end, where Load
    // older is.
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
      const loadOlder = screen.getByRole("button", { name: CONTROL_LABELS.load_older });
      act(() => loadOlder.focus());
      await userEvent.keyboard("{Enter}");
      await waitFor(() => expect(loadOlder).toHaveAttribute("aria-busy", "true"));
    } else if (place === "pointer") {
      act(() => {
        const at = { bubbles: true, clientX: point.x, clientY: point.y };
        entry.dispatchEvent(new PointerEvent("pointerover", at));
        entry.dispatchEvent(new PointerEvent("pointermove", at));
      });
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
  ["older", "Load older pressed"],
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
              // The address arrives a while after the reader's last act: its landing's scroll
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
