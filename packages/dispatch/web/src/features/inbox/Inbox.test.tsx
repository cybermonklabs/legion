import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps the row hold's bare pointerover/pointerout with a relatedTarget, the motion hold's bare pointermove, a blur naming no target, the Enter, Space and `r` presses whose survival only fireEvent reports, and a held `e`'s repeats after its first press handed the reader on, which user-event sends only in one burst with that press.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import {
  drawnPick,
  type HeldNotifications,
  holdNotifications,
  recordCommits,
} from "../../__tests__/commits";
import { type FakeEditor, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { heldNotDisabled } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { pickIn } from "../../__tests__/issue-picks";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { layRowsOut, watchMoves } from "../../__tests__/laid-out-rows";
import {
  highlightedRow,
  rows as listRows,
  optionNames,
  optionOf,
  panelOf,
  pickNamed,
  textBoxes,
} from "../../__tests__/list-picks";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import {
  answerAskSnoozes,
  answerAskUnsnoozes,
  answerInboxDismisses,
  answerIssuePatches,
} from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api, NetworkError, RequestTimeoutError } from "../../api/client";
import { inboxThreadsQuery } from "../../api/queries";
import type {
  Agent,
  Artifact,
  AskRead,
  Comment,
  CredentialPendingRow,
  InboxRow,
  Issue,
  IssueDetails,
  Message,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { QUIET_MS } from "../doc/refocus-when-settled";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, DIALOG_SCOPE } from "../shell/keymap";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { KEYED_SEND_ATTEMPT_MS, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { SnoozeWrites } from "./ask-snooze-fixture";
import { Inbox } from "./Inbox";
import {
  FIXTURE_PROJECT_DEFAULT,
  installInboxApiMocks,
  issueAsk,
  mockAskReads,
  shortRow,
  widenToReadingPane,
  withWaitingOnPerson,
} from "./inbox-fixture";
import {
  address,
  headings,
  holdThreadsRead,
  inboxIssue,
  mockIssueReads,
  movableReadingPaneWidth,
  narrowIssueOf,
  RouteProbe,
  renderInbox,
  rowIds,
  selectedIds,
  soon,
} from "./inbox-page-fixture";
import type { InboxRoute } from "./inbox-route";
import { ROW_IN_HAND_CLASS } from "./option-digits";
import { SnoozeControl } from "./SnoozeControl";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const mocks = installInboxApiMocks();

/** A question on a project document. It has no issue, so the person it reaches is its project's
 *  default assignee - `projectDefault`, `null` for a row the server resolved nobody for. */
function artifactAsk(projectDefault: string | null = FIXTURE_PROJECT_DEFAULT): InboxRow {
  const row: InboxRow = {
    anchor: null,
    answer: null,
    artifact_id: "artifact-1",
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    block_id: null,
    edited_at: null,
    id: "ask-1",
    issue_key: null,
    kind: "question",
    last_reply: null,
    multiple: false,
    opened_event_id: 1,
    options: [],
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
    },
    question: "Does this design need review?",
    priority: null,
    snoozed_until: null,
    state: "open",
    waiting_on: "human",
    urgency: "med",
  };
  return withWaitingOnPerson(row, projectDefault);
}

test("a cold Inbox hydrates every ask thread from its one list response", async () => {
  const rows = Array.from({ length: 20 }, (_, index) => {
    const row = issueAsk({
      id: `ask-${index + 1}`,
      question: `Question ${index + 1}`,
    });
    return {
      ...row,
      thread: {
        answers: [],
        edits: [],
        claimant: null,
        followers: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
        replies: [
          {
            ...commentDeliveryFields(),
            anchor: null,
            ask_id: row.id,
            author: { id: "session-1", kind: "session" as const },
            body: `Reply ${index + 1}`,
            created_at: "2026-09-11T01:00:00Z",
            edited_at: null,
            id: `reply-${index + 1}`,
            issue_key: row.issue_key,
            reply_to: null,
            resolved: false,
            resolved_at: null,
            resolved_by: null,
            suggestion: null,
            turn: "agent" as const,
          },
        ],
      },
    };
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = spyOn(api, "getAsk").mockRejectedValue(new Error("per-row fetch"));
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() =>
      expect(
        rows.map(
          (row) => queryClient.getQueryData<AskRead>(["ask-thread", row.id])?.replies?.[0]?.body
        )
      ).toEqual(rows.map((row) => row.thread.replies?.[0]?.body))
    );
    expect(getInbox).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(getAsk).toHaveBeenCalledTimes(0));
    await waitFor(() => expect(mocks.getReferences).toHaveBeenCalledTimes(0));
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an unchanged old Inbox cache does not refetch every thread on remount", async () => {
  const row = issueAsk({
    id: "ask-old",
    thread: {
      answers: [],
      edits: [],
      claimant: null,
      followers: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
      replies: [
        {
          ...commentDeliveryFields(),
          anchor: null,
          ask_id: "ask-old",
          author: { id: "session-1", kind: "session" },
          body: "Still current.",
          created_at: "2026-09-11T01:00:00Z",
          edited_at: null,
          id: "reply-old",
          issue_key: "CORE-1",
          reply_to: null,
          resolved: false,
          resolved_at: null,
          resolved_by: null,
          suggestion: null,
          turn: "agent",
        },
      ],
    },
  });
  const getInbox = spyOn(api, "getInbox").mockImplementation(
    () => new Promise<InboxRow[]>(() => {})
  );
  const getAsk = spyOn(api, "getAsk").mockRejectedValue(new Error("old cache must not refresh"));
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(["inbox"], [row], { updatedAt: Date.now() - 30_001 });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Still current.");
    await waitFor(() => expect(getAsk).toHaveBeenCalledTimes(0));
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox labels an artifact-owned ask with its project and document page link", async () => {
  const ask = artifactAsk();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // A document ask has no issue, so it reaches its project's default assignee: that person's
  // list is where it shows.
  const view = render(
    <MemoryRouter initialEntries={[`/?assignee=${FIXTURE_PROJECT_DEFAULT}`]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("CORE · Design notes");
    expect(screen.getByRole("link", { name: "CORE · Design notes" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents/design-notes?ask=ask-1"
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox puts every ask waiting on the viewer under Waiting on you", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which approach?");
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you (2)"]);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox keeps rows waiting on agents below Waiting on you without duplicating rows", async () => {
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },

    waiting_on: "agent",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which approach?");
    const headings = screen
      .getAllByRole("heading", { level: 2 })
      .map((heading) => heading.textContent);
    expect(headings).toEqual(["Waiting on you (1)", "Waiting on agents (1)"]);
    expect(screen.getByText(wholeText("Waiting on session:session-…"))).toBeTruthy();
    // No `?agent=`, so no agent filter to clear.
    expect(screen.queryByRole("button", { name: "Clear agent filter" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox keeps an agent's latest reply on its Waiting-on-you row", async () => {
  const askA = issueAsk({
    id: "ask-a",
    last_reply: {
      author: { id: "session-2", kind: "session" },
      created_at: "2026-09-11T01:00:00Z",
    },
  });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T02:00:00Z" },

    waiting_on: "agent",
    question: "Which format?",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which approach?");
    expect(screen.getByText(wholeText("session:session-… replied"))).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox partitions by waiting_on: an agent's progress note keeps its ask under Waiting on agents", async () => {
  const noted = issueAsk({
    id: "ask-noted",
    last_reply: {
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-11T01:00:00Z",
    },
    question: "Which auditor?",
    waiting_on: "agent",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([noted]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: noted,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Which auditor?");
    const headings = screen
      .getAllByRole("heading", { level: 2 })
      .map((heading) => heading.textContent);
    expect(headings).toEqual(["Waiting on agents (1)"]);
    expect(screen.queryByText(wholeText("session:session-… replied"))).not.toBeInTheDocument();
    expect(screen.getByText(wholeText("Waiting on session:session-…"))).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox preserves server priority order within Waiting on you", async () => {
  const p2 = issueAsk({
    created_at: new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString(),
    id: "ask-p2",
    priority: 2,
    question: "P2 earlier action",
  });
  const p0 = issueAsk({
    created_at: new Date(Date.now() - 35 * 60 * 1000).toISOString(),
    id: "ask-p0",
    priority: 0,
    question: "P0 later action",
  });
  const agentWaits = issueAsk({
    id: "ask-agent",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: new Date().toISOString() },

    waiting_on: "agent",
    question: "Waiting on agent",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([p0, agentWaits, p2]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: [p0, p2, agentWaits].find((ask) => ask.id === id) ?? p0,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on&sort=priority"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByRole("heading", { name: "Waiting on you (2)" });
    expect(
      screen
        .getAllByTestId(/^ask-ask-/)
        .filter(
          (card) =>
            card.closest("[data-inbox-section]")?.getAttribute("data-inbox-section") === "human"
        )
        .map((card) => card.dataset.testid)
    ).toEqual(["ask-ask-p0", "ask-ask-p2"]);
    // The banner is gone from this page; the row's own needs-you control carries its count, and
    // its accessible name carries every word the banner said (INFRA-3135, R7).
    expect(screen.getByRole("button", { name: "Blocked on you: 2 items, oldest 2d" })).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }

  const emptyInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const emptyView = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const emptyState = await screen.findByRole("region", { name: "Inbox empty state" });
    expect(within(emptyState).getByText("Nothing needs you")).toBeTruthy();
    expect(screen.queryByText(/Blocked on you:/)).not.toBeInTheDocument();
  } finally {
    emptyView.unmount();
    emptyInbox.mockRestore();
  }
});

// Owner grouping, and the `N more` jump between bands that went with it, are gone: the list is
// banded by the reader's own Group by now (`features/inbox/grouping.ts`). What survives of those
// tests is the rule that a band's heading is a label and not a row.
test("j enters a band at its first row instead of its heading", async () => {
  const user = userEvent.setup();
  const above = inboxIssue("ask-above", "CORE-9", "Above");
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const rows = [above, first, second];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });

  try {
    await screen.findByTestId("ask-ask-first");
    const heading = screen.getByRole("button", { name: "Waiting on you (3)" });
    const aboveRow = screen.getByTestId("ask-ask-above").closest<HTMLElement>("[data-inbox-row]");
    const firstRow = screen.getByTestId("ask-ask-first").closest<HTMLElement>("[data-inbox-row]");
    if (aboveRow === null || firstRow === null) throw new Error("Inbox row missing");

    await user.keyboard("j");
    expect(aboveRow).toHaveFocus();
    await user.keyboard("j");
    expect(firstRow).toHaveFocus();
    expect(document.activeElement).not.toBe(heading);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("k enters a band at its last row instead of its heading", async () => {
  const user = userEvent.setup();
  const first = inboxIssue("ask-first", "CORE-1", "First");
  const second = inboxIssue("ask-second", "CORE-1", "Second");
  const below = inboxIssue("ask-below", "CORE-9", "Below");
  const rows = [first, second, below];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });

  try {
    await screen.findByTestId("ask-ask-first");
    const heading = screen.getByRole("button", { name: "Waiting on you (3)" });
    const lastRow = rowIds().at(-1);
    const belowRow = document.querySelector<HTMLElement>(`[data-inbox-row="${lastRow}"]`);
    const secondToLast = document.querySelector<HTMLElement>(
      `[data-inbox-row="${rowIds().at(-2)}"]`
    );
    if (belowRow === null || secondToLast === null) throw new Error("Inbox row missing");

    await user.keyboard("k");
    expect(belowRow).toHaveFocus();
    await user.keyboard("k");
    expect(secondToLast).toHaveFocus();
    expect(document.activeElement).not.toBe(heading);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

function secretRequest(overrides: Partial<CredentialPendingRow> = {}): CredentialPendingRow {
  return {
    approve_url: "https://secrets.example.com/approve/record-1",
    identifiers: ["DEMO_API_KEY"],
    kind: "agent_secret",
    record_id: "record-1",
    requested_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
    session: { enrollment: null, request: null },
    ...overrides,
  };
}

test("a credential request alone is listed and counted by the Blocked on you control, and nothing says nothing needs you", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  mocks.getCredentialPending.mockResolvedValue({ pending: [secretRequest()] });
  // A listed request names what the broker can still offer; this one needs no machine code.
  const getCredentialCapabilities = spyOn(api, "getCredentialCapabilities").mockResolvedValue({
    machine_code_page: false,
  });
  const { unmount } = renderInbox();
  try {
    const request = await screen.findByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
    expect(request).toHaveAttribute("href", "https://secrets.example.com/approve/record-1");
    expect(screen.getByRole("button", { name: "Blocked on you: 1 item, oldest 3h" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Inbox empty state" })).not.toBeInTheDocument();
    expect(screen.queryByText("Nothing needs you")).not.toBeInTheDocument();
  } finally {
    unmount();
    getCredentialCapabilities.mockRestore();
    getInbox.mockRestore();
  }
});

// INFRA-3921: an expired credential request stays in the section until somebody dismisses it, but
// nobody can decide it, so nothing waits on the reader there: the Inbox still says nothing needs
// them, draws no Blocked on you control, and lists the expired request above that, marked and
// with Dismiss.
test("an expired credential request alone is listed with Dismiss, and the Inbox still says nothing needs you", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  mocks.getCredentialPending.mockResolvedValue({
    pending: [
      secretRequest({
        expires_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
        state: "expired",
      }),
    ],
  });
  const getCredentialCapabilities = spyOn(api, "getCredentialCapabilities").mockResolvedValue({
    machine_code_page: false,
  });
  const { unmount } = renderInbox();
  try {
    const emptyState = await screen.findByRole("region", { name: "Inbox empty state" });
    expect(within(emptyState).getByText("Nothing needs you")).toBeTruthy();
    // The control is found by its role and its name, which is `Blocked on you` alone or with its
    // count after a colon; its text is `Blocked on you · N`.
    expect(screen.queryByRole("button", { name: /^Blocked on you\b/ })).not.toBeInTheDocument();
    const section = screen.getByRole("region", { name: "Credential requests" });
    expect(within(section).getByText("Expired")).toBeTruthy();
    expect(
      within(section).getByRole("button", { name: "Dismiss expired secret request DEMO_API_KEY" })
    ).toBeTruthy();
    expect(within(section).queryByRole("link")).not.toBeInTheDocument();
  } finally {
    unmount();
    getCredentialCapabilities.mockRestore();
    getInbox.mockRestore();
  }
});

test("the Blocked on you control counts credential requests beside the asks waiting on you, oldest of either", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([
    issueAsk({ created_at: new Date().toISOString() }),
  ]);
  mocks.getCredentialPending.mockResolvedValue({
    pending: [
      secretRequest({ requested_at: new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString() }),
    ],
  });
  // A listed request names what the broker can still offer; this one needs no machine code.
  const getCredentialCapabilities = spyOn(api, "getCredentialCapabilities").mockResolvedValue({
    machine_code_page: false,
  });
  const { unmount } = renderInbox();
  try {
    await screen.findByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
    expect(
      await screen.findByRole("button", { name: "Blocked on you: 2 items, oldest 2d" })
    ).toBeTruthy();
  } finally {
    unmount();
    getCredentialCapabilities.mockRestore();
    getInbox.mockRestore();
  }
});

test("a credential list that fails to load keeps the Inbox from saying nothing needs you", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  mocks.getCredentialPending.mockRejectedValue(
    new ApiError(503, { code: "AGENT_SECRETS_UNAVAILABLE" })
  );
  const { unmount } = renderInbox();
  try {
    expect(await screen.findByText("Couldn't load credential requests.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Inbox empty state" })).not.toBeInTheDocument();
  } finally {
    unmount();
    getInbox.mockRestore();
  }
});

// F2 item 3: `Blocked on you · N` counted a credential request as none while its list was out or
// its first read had failed, so the number beside the control was a number of a read nobody had
// answered. It names its narrowing alone until both of its reads are in.
test("Blocked on you draws no number while the credential list is out, and the number once it lands", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const listed = Promise.withResolvers<{ pending: CredentialPendingRow[] }>();
  mocks.getCredentialPending.mockReturnValue(listed.promise);
  const getCredentialCapabilities = spyOn(api, "getCredentialCapabilities").mockResolvedValue({
    machine_code_page: false,
  });
  const { unmount } = renderInbox();
  try {
    // The control is there, since an ask already waits; its count is not, since the requests it
    // would add are unread.
    const control = await screen.findByRole("button", { name: "Blocked on you" });
    expect(control.textContent).toBe("Blocked on you");
    expect(control).toHaveAttribute("title", "Blocked on you");
    expect(screen.queryByText(/Blocked on you · \d/)).not.toBeInTheDocument();

    listed.resolve({ pending: [secretRequest()] });
    const counted = await screen.findByRole("button", {
      name: /^Blocked on you: 2 items, oldest /,
    });
    expect(counted.textContent).toBe("Blocked on you · 2");
  } finally {
    unmount();
    getCredentialCapabilities.mockRestore();
    getInbox.mockRestore();
  }
});

test("Blocked on you draws no number after the credential list's first read failed", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  mocks.getCredentialPending.mockRejectedValue(
    new ApiError(503, { code: "AGENT_SECRETS_UNAVAILABLE" })
  );
  const { unmount } = renderInbox();
  try {
    expect(await screen.findByText("Couldn't load credential requests.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Blocked on you" }).textContent).toBe(
      "Blocked on you"
    );
  } finally {
    unmount();
    getInbox.mockRestore();
  }
});

test("Inbox narrows to one agent's asks from ?agent and clears back to the whole inbox", async () => {
  const user = userEvent.setup();
  const fromPlanner = issueAsk({ id: "ask-planner", question: "Planner question" });
  const plannerWaits = issueAsk({
    id: "ask-planner-waits",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: new Date().toISOString() },

    waiting_on: "agent",
    question: "Planner waits on alice",
  });
  const fromReviewer = issueAsk({
    author: { id: "reviewer-session", kind: "session", origin: { session_title: "Reviewer" } },
    id: "ask-reviewer",
    question: "Reviewer question",
  });
  const rows = [fromPlanner, plannerWaits, fromReviewer];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: rows.find((ask) => ask.id === id) ?? fromPlanner,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/w",
      host: "m",
      last_activity: null,
      last_seen: Date.now(),
      machine_id: "m",
      open_asks: 2,
      roles: [],
      session_id: "session-1",
      state: "live",
      title: "Planner",
    },
  ]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?agent=session-1&group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <RouteProbe />
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Planner question");
    expect(screen.getByText("Planner waits on alice")).toBeTruthy();
    expect(screen.queryByText("Reviewer question")).not.toBeInTheDocument();
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you (1)", "Waiting on agents (1)"]);
    const chip = await screen.findByRole("button", { name: "Clear agent filter" });
    expect(chip.textContent).toBe("Asks from Planner×");
    // The agent's live link sits beside the clear-filter chip, never inside it.
    const chipAgent = chip.closest<HTMLElement>("[data-agent-name]");
    if (chipAgent === null) throw new Error("the agent filter chip is not drawn by AgentName");
    const live = await within(chipAgent).findByRole("link", {
      name: "Open live conversation with Planner",
    });
    expect(live).toHaveAttribute("href", "/agents/session-1/live");
    expect(chip.contains(live)).toBe(false);
    // The Blocked on you control counts the whole inbox, which would contradict the filtered
    // list, so it is absent beside an agent filter. It is found by its role and its name, which
    // is `Blocked on you` alone or with its count after a colon; its text is `Blocked on you · N`.
    expect(screen.queryByRole("button", { name: /^Blocked on you\b/ })).not.toBeInTheDocument();

    await user.click(chip);
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/"));
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox ?section=needs-you keeps only the agent's asks waiting on the viewer", async () => {
  const fromPlanner = issueAsk({ id: "ask-planner", question: "Planner question" });
  const plannerWaits = issueAsk({
    id: "ask-planner-waits",
    last_reply: { author: { id: "alice", kind: "user" }, created_at: new Date().toISOString() },

    waiting_on: "agent",
    question: "Planner waits on alice",
  });
  const rows = [fromPlanner, plannerWaits];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: rows.find((ask) => ask.id === id) ?? fromPlanner,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?agent=session-1&section=needs-you&group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Planner question");
    expect(screen.queryByText("Planner waits on alice")).not.toBeInTheDocument();
    expect(
      screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)
    ).toEqual(["Waiting on you (1)"]);
    // No live agent carries the title, so the chip falls back to the asks' author label.
    expect(screen.getByRole("button", { name: "Clear agent filter" }).textContent).toBe(
      "Asks from session:session-… waiting on you×"
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox filtered to an agent with no open asks says so and still offers to clear", async () => {
  const user = userEvent.setup();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: issueAsk(),
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  });
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = render(
    <MemoryRouter initialEntries={["/?agent=idle-session"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <RouteProbe />
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const emptyState = await screen.findByRole("region", { name: "Inbox empty state" });
    // A session the registry does not list and no listed ask names reads by its short id.
    expect(within(emptyState).getByText("No open asks from session:idle-ses…")).toBeTruthy();
    expect(screen.queryByText("Which approach?")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Clear agent filter" }));
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/"));
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** The agent `?agent=session-1` names, as the registry lists it, by `title`. */
function plannerAgents(title = "Planner") {
  return spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/w",
      host: "m",
      last_activity: null,
      last_seen: Date.now(),
      machine_id: "m",
      open_asks: 1,
      roles: [],
      session_id: "session-1",
      state: "live",
      title,
    },
  ]);
}

/** Tailwind's own definitions of the utilities the chips line and the agent chip's box are laid
 *  out by. happy-dom applies a stylesheet it is given but lays nothing out, so how wide a box may
 *  be is read from its computed style with these in the document. */
const CHIP_WIDTH_CSS =
  ".flex{display:flex}.inline-flex{display:inline-flex}.shrink-0{flex-shrink:0}.min-w-0{min-width:0}.max-w-full{max-width:100%}" +
  String.raw`.\[\&\>\*\]\:shrink-0>*{flex-shrink:0}`;

// The chips line holds no control wider than itself: a chip whose words do not fit is cut short
// with an ellipsis by its own `max-w-full`. The agent narrowing's chip sits in a box with the
// agent's live link, and the line keeps each box it holds from shrinking, so that box must be no
// wider than the line itself, and the chip's own box must give way to the link inside it, so the
// link stays in the line too. Each box from the line's child down to the chip is read from its
// computed style: it is no wider than the box it is in where its `max-width` is that box's width,
// or where it may shrink (`flex-shrink` above 0) below its words (`min-width: 0`) in a flex box.
test("an agent title longer than the chips line draws the agent chip no wider than the line, beside its live link", async () => {
  const title = `Planner of ${"the release train ".repeat(12).trim()}`;
  const fromPlanner = issueAsk({ id: "ask-planner", question: "Planner question" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([fromPlanner]);
  const getAsk = mockAskReads([fromPlanner]);
  const listAgents = plannerAgents(title);
  const css = document.createElement("style");
  css.textContent = CHIP_WIDTH_CSS;
  document.head.appendChild(css);
  const view = render(
    <MemoryRouter initialEntries={["/?agent=session-1"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const chip = await screen.findByRole("button", { name: "Clear agent filter" });
    await waitFor(() => expect(chip.textContent).toBe(`Asks from ${title}×`));
    const strip = chip.closest<HTMLElement>("[data-chip-strip]");
    if (strip === null) throw new Error("the agent chip is drawn outside the chips line");
    // Each box from the line's own child down to the chip, outermost first, by its tag and mark.
    const boxes: HTMLElement[] = [];
    for (
      let box: HTMLElement | null = chip;
      box !== null && box !== strip;
      box = box.parentElement
    ) {
      boxes.unshift(box);
    }
    const named = (box: HTMLElement) =>
      `${box.tagName.toLowerCase()}${box.hasAttribute("data-agent-name") ? "[data-agent-name]" : ""}${box === chip ? '[data-control="chip"]' : ""}`;
    const gives = (box: HTMLElement) => {
      const style = getComputedStyle(box);
      const parent = box.parentElement;
      const inFlex = parent !== null && getComputedStyle(parent).display.endsWith("flex");
      return inFlex && style.flexShrink !== "0" && style.minWidth === "0";
    };
    const [agentBox, chipBox] = boxes;
    const link = within(strip).getByRole("link", {
      name: `Open live conversation with ${title}`,
    });
    expect({
      boxes: boxes.map(
        (box) =>
          `${named(box)}: ${getComputedStyle(box).maxWidth === "100%" || gives(box) ? "no wider than its box" : "as wide as its words"}`
      ),
      link:
        agentBox?.contains(link) && chipBox !== undefined && gives(chipBox)
          ? "the chip gives way to the live link in its box"
          : "the chip keeps its words' width beside the live link",
    }).toEqual({
      boxes: boxes.map((box) => `${named(box)}: no wider than its box`),
      link: "the chip gives way to the live link in its box",
    });
  } finally {
    view.unmount();
    css.remove();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the agent narrowing is the row's shared chip, and its press leaves for the Inbox it narrowed as a link does", async () => {
  const user = userEvent.setup();
  const rows = [issueAsk({ id: "ask-planner", question: "Planner question" })];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listAgents = plannerAgents();
  const narrowed = "/?agent=session-1&q=Planner&assignee=everyone&group=waiting-on&type=question";
  const { unmount } = renderInbox(["/agents", narrowed]);
  try {
    // Found by its name alone, so a chip drawn as anything but the shared one is read, not missed.
    const chip = await screen.findByLabelText("Clear agent filter");
    const type = screen.getByRole("button", { name: "Remove Type: Question filter" });
    expect([chip.tagName, chip.getAttribute("data-control"), chip.className]).toEqual([
      "BUTTON",
      "chip",
      type.className,
    ]);
    expect(chip.textContent).toBe("Asks from Planner×");

    await user.click(chip);
    // The agent, the Type narrowing and the banding go; who the list is for and the text stay.
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/?q=Planner&assignee=everyone")
    );
    expect(screen.queryByLabelText("Clear agent filter")).not.toBeInTheDocument();
    // The press pushed an entry, as following a link does: Back gives the agent's asks back.
    await user.click(screen.getByRole("button", { name: "History back" }));
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe(narrowed));
  } finally {
    unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("under a host that owns the filter, the agent chip's press tells the host and leaves the address", async () => {
  const user = userEvent.setup();
  const rows = [issueAsk({ id: "ask-planner", question: "Planner question" })];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listAgents = plannerAgents();
  const changes: InboxRoute[] = [];
  const view = render(
    <MemoryRouter initialEntries={["/agents"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <RouteProbe />
        <Inbox
          filter={{ agent: "session-1", assignees: [], group: "waiting-on", q: "Planner" }}
          keymapScope={DIALOG_SCOPE}
          onFilterChange={(next) => changes.push(next)}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await user.click(await screen.findByLabelText("Clear agent filter"));
    // The host is told what every other chip's removal tells it, and the page under it stays.
    expect(changes).toEqual([{ assignees: [], item: undefined, q: "Planner" }]);
    expect(screen.getByTestId("location").textContent).toBe("/agents");
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a row being typed into stays under Waiting on you when an agent's note flips its turn, keeps its draft, and moves once the reader leaves it", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which format?",
  });
  const askC = issueAsk({
    id: "ask-c",
    issue: { assignee: "alice", key: "CORE-3", title: "Third issue" },
    issue_key: "CORE-3",
    priority: 0,
    question: "Brand new ask",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: [askA, askB, askC].find((ask) => ask.id === id) ?? askA,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    await user.click(await within(card).findByRole("radio", { name: "Ship" }));
    const field = within(card).getByLabelText("Your answer");
    await user.clear(field);
    await user.click(field);
    await user.paste("Ship it after the audit");
    field.focus();

    // An agent's progress note hands the turn to the agent while a P0 ask arrives above: the row
    // the reader is typing into stays where they see it, with the new turn on its card.
    act(() => {
      queryClient.setQueryData<InboxRow[]>(
        ["inbox"],
        [
          askC,
          askA,
          {
            ...askB,
            last_reply: {
              author: { id: "session-1", kind: "session" },
              created_at: "2026-09-11T01:00:00Z",
            },
            waiting_on: "agent",
          },
        ]
      );
    });

    const moved = screen.getByTestId("ask-ask-b");
    expect(within(moved).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe(
      "Ship it after the audit"
    );
    expect(within(moved).getByRole<HTMLInputElement>("radio", { name: "Ship" }).checked).toBe(true);
    expect(field).toHaveFocus();
    expect(moved).toBe(card);
    expect(headings()).toEqual(["Waiting on you (3)"]);
    // The list sorts itself now (newest first), and the held row keeps its place after ask-a.
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(moved.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("human");
    expect(screen.getByTestId("turn-ask-b").textContent).toBe("Waiting on session:session-…");

    // Their focus leaves the row: it takes its place under Waiting on agents, draft intact.
    // Their pointer, which the clicks above put on the row, leaves with it: the row is held by
    // either hand, and only a reader off it entirely sees it move.
    await user.pointer({ target: document.body });
    act(() => field.blur());
    await waitFor(() =>
      expect(headings()).toEqual(["Waiting on you (2)", "Waiting on agents (1)"])
    );
    expect(moved.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("agent");
    expect(within(moved).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe(
      "Ship it after the audit"
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the reader's own Ask back keeps the row under Waiting on you, unscrolled, until they leave it", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const askC = issueAsk({
    id: "ask-c",
    issue: { assignee: "alice", key: "CORE-3", title: "Third issue" },
    issue_key: "CORE-3",
    question: "Third question?",
  });
  let inboxRows = [askA, askB, askC];
  let repliesB: Comment[] = [];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => inboxRows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: [askA, askB, askC].find((ask) => ask.id === id) ?? askA,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: id === askB.id ? repliesB : [],
  }));
  // The server records the clarification: the turn is the agent's, and the inbox says so on
  // the next refetch.
  const createComment = spyOn(api, "createComment").mockImplementation(async (issueKey, input) => {
    const reply: Comment = {
      anchor: null,
      ask_id: input.ask_id ?? null,
      author: { id: "alice", kind: "user" },
      body: input.body ?? "",
      created_at: "2026-09-11T02:00:00Z",
      edited_at: null,
      id: "reply-1",
      issue_key: issueKey,
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: "agent",
      ...commentDeliveryFields(),
    };
    repliesB = [reply];
    inboxRows = [
      askA,
      {
        ...askB,
        last_reply: { author: reply.author, created_at: reply.created_at },
        waiting_on: "agent",
      },
      askC,
    ];
    return reply;
  });
  const geometry = stackRows();
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    // user-event's hover sends a fixed mouse sequence; the row's hold reads this pointerover alone.
    fireEvent.pointerOver(row);
    const draft = within(card).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Ship what, exactly?");
    const askBack = within(card).getByRole("button", { name: "Ask back" });
    act(() => {
      askBack.focus();
    });
    await user.click(askBack);

    // The reply lands in the card's thread and the card says whose turn it is; Ask back, its
    // text sent, disables itself and hands focus to the row. The row does not leave from under
    // the reader's hand, and the view is not dragged after it.
    await within(card).findByText("Ship what, exactly?");
    await waitFor(() =>
      expect(screen.getByTestId("turn-ask-b").textContent).toBe("Waiting on session:session-…")
    );
    expect(createComment).toHaveBeenCalledWith(
      "CORE-2",
      {
        ask_id: "ask-b",
        body: "Ship what, exactly?",
      },
      keyedAttempt
    );
    expect(row).toHaveFocus();
    expect(row).toHaveAttribute("data-inbox-section", "human");
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(headings()).toEqual(["Waiting on you (3)"]);
    expect(geometry.scrollBy).not.toHaveBeenCalled();

    // Pointer and focus leave the row (here, k moving focus to the row above): it takes its place
    // under Waiting on agents; the row above did not move, so the view stays.
    // user-event's unhover can set no relatedTarget, which is what the row's release reads.
    fireEvent.pointerOut(row, { relatedTarget: document.body });
    // user-event has no bare pointerleave: unhover sends a whole mouse sequence instead.
    fireEvent.pointerLeave(row);
    const rowAbove = document.querySelector<HTMLElement>('[data-inbox-row="ask-a"]');
    if (rowAbove === null) throw new Error("ask-a row missing");
    act(() => rowAbove.focus());
    await waitFor(() => expect(row).toHaveAttribute("data-inbox-section", "agent"));
    expect(rowIds()).toEqual(["ask-a", "ask-c", "ask-b"]);
    expect(headings()).toEqual(["Waiting on you (2)", "Waiting on agents (1)"]);
    expect(geometry.scrollBy).not.toHaveBeenCalled();
  } finally {
    unmount();
    geometry.restore();
    createComment.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an ask answered elsewhere stays in place with its recorded answer while the reader is in its row, then leaves", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const answeredB: InboxRow = {
    ...askB,
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "JSON, always.", user: "bob" },
    state: "answered",
  };
  let threadB = askB;
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : threadB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const field = within(card).getByLabelText("Your answer");
    await user.clear(field);
    await user.click(field);
    await user.paste("Half-typed");
    field.focus();
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");

    // Bob answers it in his own tab: the server drops it from the inbox and the thread records
    // the answer.
    threadB = answeredB;
    await act(async () => {
      queryClient.setQueryData<InboxRow[]>(["inbox"], [askA]);
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", "ask-b"] });
    });

    const kept = await screen.findByTestId("ask-ask-b");
    expect(kept.closest("[data-inbox-row]")).toBe(row);
    expect(within(kept).getByText("bob")).toBeTruthy();
    // The answer is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads.
    expect(await within(kept).findByText("JSON, always.")).toBeTruthy();
    // The answer form is gone; the row keeps keyboard focus so j/k/Escape still start here.
    expect(row).toHaveFocus();

    // Moving on (here, j/k moving focus to the next row) releases it.
    const otherRow = document.querySelector<HTMLElement>('[data-inbox-row="ask-a"]');
    if (otherRow === null) throw new Error("ask-a row missing");
    act(() => otherRow.focus());
    await waitFor(() => expect(screen.queryByTestId("ask-ask-b")).not.toBeInTheDocument());
    expect(screen.getByTestId("ask-ask-a")).toBeTruthy();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The same answer from elsewhere, in the shell's main content, as the app shell draws it. The
// reader's click on the answer field has been answered, so no press of theirs is live when the
// card swaps to its record. Whoever caused the loss, the card hands the focus to its form's own
// row, the reader's own place, with no scroll: the Inbox keeps that row in the list with the
// recorded answer while it holds the focus, and no other row takes it.
test("an ask answered elsewhere while the reader's focus is in its form hands the focus to its own row, which stays in the list with its record", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const answeredB: InboxRow = {
    ...askB,
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "JSON, always.", user: "bob" },
    state: "answered",
  };
  let threadB = askB;
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : threadB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const scrolls = watchScrolls();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <main>
          <Inbox pane={false} />
        </main>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    const field = within(card).getByLabelText("Your answer");
    await user.click(field);
    await afterFrameAndTasks();
    expect(field).toHaveFocus();

    // Bob answers it in his own tab: the server drops it from the inbox and the thread records
    // the answer.
    threadB = answeredB;
    await act(async () => {
      queryClient.setQueryData<InboxRow[]>(["inbox"], [askA]);
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", "ask-b"] });
    });
    // The card swaps to its record, and the answer form goes with the focus in it.
    const kept = await screen.findByTestId("ask-ask-b");
    // The answer is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads.
    expect(await within(kept).findByText("JSON, always.")).toBeTruthy();
    expect(field).not.toBeInTheDocument();
    await afterFrameAndTasks();

    expect(row).toHaveFocus();
    expect(kept.closest("[data-inbox-row]")).toBe(row);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(scrolls.asked()).toEqual([]);
  } finally {
    view.unmount();
    scrolls.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The same answer from elsewhere in the list layout, with the Inbox's keys mounted as well: the
// card hands the focus to its own row, which the Inbox keeps in its place with the record, so the
// Inbox's `j` steps from there onto the next row, as it does from any row, and nothing scrolls.
// The held row leaves once the focus has left it.
test("in the list, an ask answered elsewhere under the reader's focus keeps it on the ask's own row, and j then focuses the next row", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const askC = issueAsk({
    id: "ask-c",
    issue: { assignee: "alice", key: "CORE-3", title: "Third issue" },
    issue_key: "CORE-3",
    question: "Which owner?",
  });
  const answeredB: InboxRow = {
    ...askB,
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "JSON, always.", user: "bob" },
    state: "answered",
  };
  let threadB = askB;
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB, askC]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askB.id ? threadB : id === askC.id ? askC : askA,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const scrolls = watchScrolls();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <Inbox pane={false} />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    const field = within(card).getByLabelText("Your answer");
    await user.click(field);
    await afterFrameAndTasks();
    expect(field).toHaveFocus();

    // Bob answers it in his own tab: the server drops it from the inbox and the thread records
    // the answer, and the card swaps to its record with the focus in its form.
    threadB = answeredB;
    await act(async () => {
      queryClient.setQueryData<InboxRow[]>(["inbox"], [askA, askC]);
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", "ask-b"] });
    });
    const kept = await screen.findByTestId("ask-ask-b");
    expect(await within(kept).findByText("JSON, always.")).toBeTruthy();
    expect(field).not.toBeInTheDocument();
    await afterFrameAndTasks();
    expect(row).toHaveFocus();
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(scrolls.asked()).toEqual([]);

    await user.keyboard("j");
    expect(shortRow("ask-c")).toHaveFocus();
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-c"]));
    expect(scrolls.asked()).toEqual([]);
  } finally {
    view.unmount();
    scrolls.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the reader's own answer leaves the Inbox at once, even though their focus and pointer were on the row", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which format?",
  });
  let inboxRows = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => inboxRows);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : askB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    inboxRows = [askA];
    return {
      ...askB,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    // user-event's hover sends a fixed mouse sequence; the row's hold reads this pointerover alone.
    fireEvent.pointerOver(row);
    await user.click(await within(card).findByRole("radio", { name: "Ship" }));
    const answer = within(card).getByRole("button", { name: "Answer" });
    answer.focus();
    await user.click(answer);
    answer.focus();

    await waitFor(() => expect(screen.queryByTestId("ask-ask-b")).not.toBeInTheDocument());
    expect(screen.getByTestId("ask-ask-a")).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** What the priority menu inside `scope`, a CSS selector, draws (`drawnPick`), or `(none)` while
 *  none is drawn there. Each scope these cases read - a row, the reading column - draws one. */
function drawnPriority(scope: string): string {
  const trigger = document.querySelector<HTMLElement>(`${scope} [data-priority-menu]`);
  return trigger === null ? "(none)" : drawnPick(trigger);
}

// A pick of P2 on a P1 row under `?priority=P1` takes the row out of the filter, and the Inbox
// holds it in place while the reader's hand is on it (`heldRow`), in the band another P1 row
// keeps on the page. The held row is drawn as the Inbox holds it now, not as it was last drawn,
// so its picker draws the pick in every commit from the press on: before TanStack Query tells
// the Inbox of the pick, after it, and after the save lands and the Inbox is read again.
test("a priority pick that takes the row under the reader's hand out of the priority filter draws the pick from the press through the landing", async () => {
  const user = userEvent.setup();
  const ask = inboxIssue("ask-p1", "CORE-1", "Ship it at P1?", { priority: 1 });
  const other = inboxIssue("ask-other", "CORE-2", "Which format?", { priority: 1 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask, other]);
  const getAsk = mockAskReads([ask, other]);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const commits = recordCommits<null>(() => drawnPriority('[data-inbox-row="ask-p1"]'));
  const { queryClient, unmount } = renderInbox("/?priority=P1", { commits, pane: false });
  let notifications: HeldNotifications | undefined;
  try {
    await screen.findByText("Ship it at P1?");
    const row = rowOf("ask-p1");
    // user-event's hover sends a fixed mouse sequence; the row's hold reads this pointerover alone.
    fireEvent.pointerOver(row);
    const priority = within(row).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    await user.click(priority);
    const p2 = within(panelOf(priority)).getByRole("option", { name: "P2" });
    const fromPick = commits.markAt(p2, "click");
    notifications = holdNotifications();
    await user.click(p2);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    notifications.tell();
    const reads = getInbox.mock.calls.length;
    getInbox.mockResolvedValue([{ ...ask, priority: 2 }, other]);
    await act(async () => {
      save.resolve({ ...narrowIssueOf(ask, "alice"), priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));

    // The hand is still on the row, which the filter no longer lists.
    expect(rowIds()).toContain("ask-p1");
    expect(fromPick().map(({ drawn }) => drawn)).toEqual(fromPick().map(() => "P2 P2"));
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// Grouped by priority, a pick of P0 on the one P2 row moves it out of a band no other row is in.
// The reader's hand is on the row - the focus is in its priority list, and the pick gives it back
// to the list's trigger, which the press opened it from - so the row is kept where they saw it,
// and its band is drawn with it: the row, its trigger and the focus on it stay, through the save
// and the read after it. `0` is the list's digit for P0 (dispatch://INFRA-2160, D5), which writes
// it at once whatever the box has narrowed.
test("a priority pick that moves the row under the reader's hand out of a band it was alone in keeps the row, its band and the focus on its trigger", async () => {
  const user = userEvent.setup();
  const alone = inboxIssue("ask-p2", "CORE-1", "Ship it at P2?", { priority: 2 });
  const other = inboxIssue("ask-p1", "CORE-2", "Which format?", { priority: 1 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([alone, other]);
  const getAsk = mockAskReads([alone, other]);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderInbox("/", { pane: false });
  try {
    await screen.findByText("Ship it at P2?");
    const row = rowOf("ask-p2");
    const trigger = within(row).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    await user.click(trigger);
    await user.keyboard("p0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    expect(rowOf("ask-p2"), "the row was drawn anew").toBe(row);
    expect(trigger, "the pick left the focus off the row's priority").toHaveFocus();
    expect(
      headings().some((heading) => heading.startsWith("P2")),
      "the P2 band went"
    ).toBe(true);

    const reads = getInbox.mock.calls.length;
    getInbox.mockResolvedValue([{ ...alone, priority: 0 }, other]);
    await act(async () => {
      save.resolve({ ...narrowIssueOf(alone, "alice"), priority: 0 });
      await save.promise;
    });
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(rowOf("ask-p2"), "the row was drawn anew once the save landed").toBe(row);
    expect(trigger, "the landing left the focus off the row's priority").toHaveFocus();
    expect(trigger).toHaveAccessibleName("Priority of CORE-1 P0");
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The reading column keeps a question whose row has left the list while the reader has an
// unsent answer in its box (`paneRow`). A pick of P2 in the column's own priority picker, under
// `?priority=P1`, takes the row out of the list; the kept question is drawn as the Inbox holds it
// now, so the column's picker draws the pick in every commit from the press on, through the
// landing.
test("a priority pick in the reading column that takes its question out of the priority filter draws the pick from the press through the landing, while an unsent answer keeps the question there", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const askA = inboxIssue("ask-a", "CORE-1", "Which approach?", { priority: 1 });
  const askB = inboxIssue("ask-b", "CORE-2", "Which format?", { priority: 1 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = mockAskReads([askA, askB]);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const commits = recordCommits<null>(() => drawnPriority('[data-testid="inbox-pane"]'));
  const { queryClient, unmount } = renderInbox("/?priority=P1", { commits });
  let notifications: HeldNotifications | undefined;
  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    const pane = screen.getByTestId("inbox-pane");
    const draft = within(pane).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Whichever the mirror already holds.");
    const priority = within(pane).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    await user.click(priority);
    const p2 = within(panelOf(priority)).getByRole("option", { name: "P2" });
    const fromPick = commits.markAt(p2, "click");
    notifications = holdNotifications();
    await user.click(p2);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    notifications.tell();
    const reads = getInbox.mock.calls.length;
    getInbox.mockResolvedValue([{ ...askA, priority: 2 }, askB]);
    await act(async () => {
      save.resolve({ ...narrowIssueOf(askA, "alice"), priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));

    // The list no longer holds the question, and the column still shows it.
    expect(rowIds()).toEqual(["ask-b"]);
    expect(fromPick().map(({ drawn }) => drawn)).toEqual(fromPick().map(() => "P2 P2"));
  } finally {
    notifications?.tell();
    unmount();
    narrow();
    patchIssue.mockRestore();
    getIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// Under typed search only the reader's own typing drops a row at once. A server edit of the text
// the search reads - here the question of the row whose answer box the reader is typing in -
// takes that row out of the typed words' match, and the Inbox holds it where it was, as last
// drawn, with the half-typed answer and the focus still in its box, through each read after.
test("a server edit that takes the row under the reader's hand out of the typed search keeps the row, its half-typed answer and the focus", async () => {
  const user = userEvent.setup();
  const askA = inboxIssue("ask-a", "CORE-1", "Ship it?");
  const askB = inboxIssue("ask-b", "CORE-2", "Ship the docs?");
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = mockAskReads([askA, askB]);
  const { queryClient, unmount } = renderInbox("/?q=ship", { pane: false });
  try {
    const card = await screen.findByTestId("ask-ask-a");
    const draft = within(card).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Ship it on Friday");
    expect(draft).toHaveFocus();

    getInbox.mockResolvedValue([{ ...askA, question: "Rename it?" }, askB]);
    for (const read of ["first", "second"]) {
      await act(async () => {
        await queryClient.refetchQueries({ queryKey: ["inbox"] });
      });
      expect(rowIds(), `after the ${read} read`).toContain("ask-a");
    }
    const kept = within(screen.getByTestId("ask-ask-a")).getByLabelText("Your answer");
    expect(kept).toHaveValue("Ship it on Friday");
    expect(kept).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The row a server edit took out of the typed search stays held, and it is still drawn as the
// Inbox holds it: a pick of P2 in it draws P2 in every commit from the press on, before TanStack
// Query tells the Inbox of it, after, and after the save lands and the Inbox is read again.
test("a priority pick on a row held after a server edit took it out of the typed search draws the pick from the press through the landing", async () => {
  const user = userEvent.setup();
  const ask = inboxIssue("ask-p1", "CORE-1", "Ship it at P1?", { priority: 1 });
  const other = inboxIssue("ask-b", "CORE-2", "Ship the docs?", { priority: 1 });
  const renamed = { ...ask, question: "Rename it?" };
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask, other]);
  const getAsk = mockAskReads([ask, other]);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const commits = recordCommits<null>(() => drawnPriority('[data-inbox-row="ask-p1"]'));
  const { queryClient, unmount } = renderInbox("/?q=ship", { commits, pane: false });
  let notifications: HeldNotifications | undefined;
  try {
    await screen.findByTestId("ask-ask-p1");
    // user-event's hover sends a fixed mouse sequence; the row's hold reads this pointerover alone.
    fireEvent.pointerOver(rowOf("ask-p1"));
    getInbox.mockResolvedValue([renamed, other]);
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toContain("ask-p1");

    const priority = within(rowOf("ask-p1")).getByRole("button", {
      name: pickNamed("Priority of CORE-1"),
    });
    await user.click(priority);
    const p2 = within(panelOf(priority)).getByRole("option", { name: "P2" });
    const fromPick = commits.markAt(p2, "click");
    notifications = holdNotifications();
    await user.click(p2);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    notifications.tell();
    const reads = getInbox.mock.calls.length;
    getInbox.mockResolvedValue([{ ...renamed, priority: 2 }, other]);
    await act(async () => {
      save.resolve({ ...narrowIssueOf(ask, "alice"), priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));

    expect(rowIds()).toContain("ask-p1");
    expect(fromPick().map(({ drawn }) => drawn)).toEqual(fromPick().map(() => "P2 P2"));
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The hold under the typed words lasts only while the reader's words are those: once they type,
// the row a server edit took out of the search is tested against their words again, and goes at
// once, though the pointer still rests on it.
test("a row held after a server edit took it out of the typed search goes once the reader types more words", async () => {
  const user = userEvent.setup();
  const askA = inboxIssue("ask-a", "CORE-1", "Ship it?");
  const askB = inboxIssue("ask-b", "CORE-2", "Ship the docs?");
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = mockAskReads([askA, askB]);
  const { queryClient, unmount } = renderInbox("/?q=ship", { pane: false });
  try {
    await screen.findByTestId("ask-ask-a");
    // user-event's hover sends a fixed mouse sequence; the row's hold reads this pointerover alone.
    fireEvent.pointerOver(rowOf("ask-a"));
    getInbox.mockResolvedValue([{ ...askA, question: "Rename it?" }, askB]);
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toContain("ask-a");

    await user.type(screen.getByRole("searchbox", { name: "Search asks" }), " docs");
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("after the reader's own answer fails, an answer from elsewhere still holds the row they are in", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  let threadB = askB;
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([askA, askB]);
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id: string) => ({
    ask: id === askA.id ? askA : threadB,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  }));
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(new Error("offline"));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox pane={false} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    const field = within(card).getByLabelText("Your answer");
    await user.clear(field);
    await user.click(field);
    await user.paste("JSON");
    const answer = within(card).getByRole("button", { name: "Answer" });
    act(() => {
      answer.focus();
    });
    await user.click(answer);
    // The optimistic removal does not take the row from under the reader: it stays, and the
    // failure shows on it.
    await waitFor(() => expect(within(card).getByRole("alert")).toBeTruthy());
    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(within(card).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe("JSON");
    act(() => {
      field.focus();
    });

    // Bob answers it meanwhile: the reader is still in the row, so it stays.
    threadB = {
      ...askB,
      answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "YAML.", user: "bob" },
      state: "answered",
    };
    await act(async () => {
      queryClient.setQueryData<InboxRow[]>(["inbox"], [askA]);
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", "ask-b"] });
    });

    const kept = await screen.findByTestId("ask-ask-b");
    expect(kept.closest("[data-inbox-row]")).toBe(row);
    // The answer is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads.
    expect(await within(kept).findByText("YAML.")).toBeTruthy();
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an ask card offers backlinks only when the batched count says it has some", async () => {
  const user = userEvent.setup();
  const cited = issueAsk({ id: "ask-cited", referenced_by_count: 2 });
  const uncited = issueAsk({ id: "ask-uncited", referenced_by_count: 0 });
  const rows = [cited, uncited];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    const citedCard = await screen.findByTestId("ask-ask-cited");
    const uncitedCard = screen.getByTestId("ask-ask-uncited");
    expect(within(citedCard).getByRole("button", { name: "Referenced by (2)" })).toBeTruthy();
    expect(within(uncitedCard).queryByText(/Referenced by/)).not.toBeInTheDocument();
    // The count came with the list: no card fetches the graph until the reader opens one.
    await waitFor(() => expect(mocks.getReferences).toHaveBeenCalledTimes(0));

    await user.click(within(citedCard).getByRole("button", { name: "Referenced by (2)" }));
    await waitFor(() => expect(mocks.getReferences).toHaveBeenCalledTimes(1));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an unavailable backlink list stays quiet beside an ask card", async () => {
  const user = userEvent.setup();
  const row = issueAsk({ id: "ask-backlink", referenced_by_count: 1 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([row]);
  const getAsk = mockAskReads([row]);
  mocks.getReferences.mockRejectedValue(new Error("offline"));
  const { unmount } = renderInbox("/", { pane: false });

  try {
    const card = await screen.findByTestId("ask-ask-backlink");
    await user.click(within(card).getByRole("button", { name: "Referenced by (1)" }));
    expect(await within(card).findByText("Referenced by unavailable.")).toBeTruthy();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** Who the filter's popover lists: `GET /users`, read on its first open. */
function mockPeople() {
  return spyOn(api, "listUsers").mockResolvedValue([{ login: "alice" }, { login: "bob" }]);
}

/** Hands the question on from the hand-on list in `scope`, as a reader does: opens the list and
 *  presses `login`'s option, which the list holds once it has read the people as it opened. The
 *  list is portalled onto the body, so its options are reached from the screen. */
async function handOnTo(user: UserEvent, scope: HTMLElement, login: string): Promise<void> {
  await user.click(within(scope).getByRole("button", { name: pickNamed("Hand this question to") }));
  await user.click(await screen.findByRole("option", { name: new RegExp(`^${login}\\b`) }));
}

/** The bar's one **Filter** button, which reveals the five fields it opens. The caller hands its
 *  own user: this file's rows read where the pointer rests, and a user of this helper's own would
 *  leave it somewhere the test does not know about. */
async function openFilterFields(user: UserEvent): Promise<void> {
  const filter = screen.getByRole("button", { name: "Filter" });
  if (filter.getAttribute("aria-expanded") !== "true") await user.click(filter);
}

async function filterTrigger(user: UserEvent): Promise<HTMLElement> {
  await openFilterFields(user);
  return screen.getByRole("button", { name: "Filter by assignee" });
}

/** Opens the assignee filter if it is shut and toggles one of its rows. The popover is portalled
 *  onto the body, so its options are reached from the screen rather than from the list, and from
 *  its own listbox rather than from the screen: every open ask's card carries a people picker of
 *  its own (the hand-on control), whose options name the same people. */
async function toggleAssignee(user: UserEvent, option: RegExp) {
  const trigger = await filterTrigger(user);
  if (trigger.getAttribute("aria-expanded") !== "true") await user.click(trigger);
  const listbox = await screen.findByRole("listbox", { name: "Assignee options" });
  await user.click(await within(listbox).findByRole("option", { name: option }));
}

/** Every scroll the page asks for from now on, by each way it has to scroll: the engine's own
 *  nearest-edge scroll (`scrollIntoView`), and the `scrollBy` of a box or of the window, which a
 *  least-scroll landing makes (`scrollToNearest`). Each spy passes the call on. `asked` names the
 *  ways that were asked, so a row that reads it prints which. */
function watchScrolls(): { asked: () => string[]; clear: () => void; restore: () => void } {
  const spies = [
    ["scrollIntoView", spyOn(Element.prototype, "scrollIntoView")],
    ["a box's scrollBy", spyOn(Element.prototype, "scrollBy")],
    ["the window's scrollBy", spyOn(window, "scrollBy")],
  ] as const;
  return {
    asked: () => spies.filter(([, spy]) => spy.mock.calls.length > 0).map(([way]) => way),
    clear: () => {
      for (const [, spy] of spies) spy.mockClear();
    },
    restore: () => {
      for (const [, spy] of spies) spy.mockRestore();
    },
  };
}
const ROW_HEIGHT = 200;

/** A fixed geometry happy-dom lacks: rows stacked in DOM order, ROW_HEIGHT each, so a row that
 *  changes place in the list changes its measured top and the ViewportAnchor would scroll after
 *  it; `scrollBy` records whether it did. */
function stackRows() {
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const all = [...document.querySelectorAll("[data-inbox-row]")];
    const index = all.indexOf(this);
    const top = index === -1 ? 0 : index * ROW_HEIGHT;
    const height = index === -1 ? all.length * ROW_HEIGHT : ROW_HEIGHT;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 800,
      toJSON: () => ({}),
      top,
      width: 800,
      x: 0,
      y: top,
    };
  });
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  return {
    restore: () => {
      rect.mockRestore();
      scrollBy.mockRestore();
    },
    scrollBy,
  };
}

/** One listed row by its ask id, for the controls that sit on it. */
function rowOf(askId: string): HTMLElement {
  const row = screen.getByTestId(`ask-${askId}`).closest<HTMLElement>("[data-inbox-row]");
  if (row === null) throw new Error(`${askId} row missing`);
  return row;
}

/** The reader, the other person, the issue nobody holds and a document question - every side of
 *  the assignee filter in one list. Neither of the last two names anybody of its own, so both
 *  reach their project's default assignee, as the server resolves them. */
function filterRows(): InboxRow[] {
  return [
    issueAsk({ id: "ask-mine" }),
    issueAsk({
      id: "ask-bob",
      issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
      issue_key: "CORE-2",
      question: "Bob's question?",
    }),
    issueAsk({
      id: "ask-orphan",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Orphan question?",
    }),
    artifactAsk(),
  ];
}

test("an address without ?assignee= lists the reader's own questions alone, under no view switch", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Which approach?");
    expect(rowIds()).toEqual(["ask-mine"]);
    expect(headings()).toEqual(["Waiting on you (1)"]);
    expect((await filterTrigger(user)).textContent).toBe("Assignee · you");
    // The Mine/Everyone switch is gone; the filter is the only way to change who the list is for.
    expect(screen.queryByRole("button", { name: "Mine" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Everyone" })).not.toBeInTheDocument();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("picking another person lists their questions at ?assignee=<email>, under Waiting on a person", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Which approach?");
    // The reader's own list is the one band that can say "you".
    expect(headings()).toEqual(["Waiting on you (1)"]);

    await toggleAssignee(user, /bob/);
    await toggleAssignee(user, /alice/);

    await screen.findByText("Bob's question?");
    expect(screen.getByTestId("location").textContent).toBe("/?assignee=bob&group=waiting-on");
    expect(rowIds()).toEqual(["ask-bob"]);
    expect(headings()).toEqual(["Waiting on a person (1)"]);
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("?assignee=everyone lists every open question, the document ones included", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    // Which rows, not their order: the order is the reader's own Sort by (newest first here).
    expect([...rowIds()].sort()).toEqual(["ask-1", "ask-bob", "ask-mine", "ask-orphan"]);
    expect((await filterTrigger(user)).textContent).toBe("Assignee · everyone");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("?assignee=unassigned lists nothing, and the two rows nobody was handed are their project's", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const unheld = renderInbox("/?assignee=unassigned");
  try {
    // Every question reaches somebody - the person it was handed to, its issue's assignee, else
    // its project's default - so the filter for what nobody holds keeps no row at all.
    expect(await screen.findByText("Nothing matches this filter")).toBeTruthy();
    expect(rowIds()).toEqual([]);
    expect((await filterTrigger(user)).textContent).toBe("Assignee · Unassigned");
  } finally {
    unheld.unmount();
  }

  // The same two rows under the person their project names: neither is missing, both are hers.
  const theirs = renderInbox(`/?assignee=${FIXTURE_PROJECT_DEFAULT}`);
  try {
    await screen.findByText("Orphan question?");
    expect([...rowIds()].sort()).toEqual(["ask-1", "ask-orphan"]);
    // Nobody is left to take either: the issue's question is already held.
    expect(screen.queryByRole("button", { name: /Assign .* to me/ })).not.toBeInTheDocument();
  } finally {
    theirs.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an address naming somebody who has never signed in lists nothing and refuses nothing", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=zoe@example.com");
  try {
    expect(await screen.findByText("Nothing matches this filter")).toBeTruthy();
    expect(rowIds()).toEqual([]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // A filter read from the address says what it is filtering by, listed person or not.
    expect((await filterTrigger(user)).textContent).toBe("Assignee · zoe@example.com");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// What `AssigneeFilter.test.tsx` proved of this control before the Inbox drew it inside its own
// filter panel (INFRA-3135): the words its trigger says, when the people are read, how its rows
// are ordered and detailed, and that a filter read from the address survives a people list that
// never arrives. What `MultiSelect`'s own test already proves of every searchable control - that
// the search narrows the rows and says so when nothing matches - is not repeated here.

/** Opens the assignee control if it is shut, and hands its trigger back. */
async function openAssignee(user: UserEvent): Promise<HTMLElement> {
  const trigger = await filterTrigger(user);
  if (trigger.getAttribute("aria-expanded") !== "true") await user.click(trigger);
  return trigger;
}

/** Each row of the assignee control's own listbox, as its whole text: the tick, the label, then
 *  the muted detail the reader's own row carries. Read from that listbox rather than from the
 *  screen, because every open ask's card carries a people picker of its own. */
async function assigneeOptionTexts(): Promise<string[]> {
  const listbox = await screen.findByRole("listbox", { name: "Assignee options" });
  return within(listbox)
    .getAllByRole("option")
    .map((option) => option.textContent ?? "");
}

test("the assignee control's trigger counts a selection of several rather than naming one", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=alice&assignee=bob");
  try {
    await screen.findByText("Bob's question?");
    expect((await filterTrigger(user)).textContent).toBe("Assignee · 2");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the people are read on the assignee control's first open and then kept, never on a render", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which approach?");
    // An Inbox nobody filters costs no read of who has signed in.
    expect(listUsers).not.toHaveBeenCalled();

    const trigger = await openAssignee(user);
    await screen.findByRole("option", { name: /bob/ });
    expect(listUsers).toHaveBeenCalledTimes(1);

    await user.click(trigger);
    await user.click(trigger);
    await screen.findByRole("option", { name: /bob/ });
    expect(listUsers).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the assignee options list Unassigned first, then the logins, and detail the reader's own row", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    await openAssignee(user);
    // The detail runs into the label in `textContent`: each row ends with how many rows that
    // value holds, counted under the other four fields' filters, and the reader's own says so
    // first. Unassigned reads 0: the two rows nobody holds or was handed fall to their project's
    // default assignee (`FIXTURE_PROJECT_DEFAULT`), as the server's third step puts them.
    expect(await assigneeOptionTexts()).toEqual(["Unassigned0", "aliceyou · 1", "bob1"]);
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The Filter panel counts each value under every filter that is on but its control's own, and
// the typed text is no filter there, as on `main`: text that hides the reader's own question
// leaves it counted under their name, so the counts read what the filters keep, whatever is typed.
test("text typed that hides a row leaves that row counted in the Filter panel", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    await user.type(screen.getByRole("searchbox", { name: "Search asks" }), "Bob");
    await waitFor(() => expect(rowIds()).toEqual(["ask-bob"]));
    await openAssignee(user);
    expect(await assigneeOptionTexts()).toEqual(["Unassigned0", "aliceyou · 1", "bob1"]);
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** Each row of one filter field's own listbox (`label`, as `DIMENSION_LABELS` names it), by the
 *  name it is read out as (`optionNames`). Escape closes the list again, so the next read opens
 *  its own. */
async function fieldOptionNames(user: UserEvent, label: string): Promise<string[]> {
  await openFilterFields(user);
  await user.click(screen.getByRole("button", { name: `Filter by ${label.toLowerCase()}` }));
  const names = optionNames(await screen.findByRole("listbox", { name: `${label} options` }));
  await user.keyboard("{Escape}");
  return names;
}

/** Whether an option's name carries a count of rows (`, 2 rows`). */
const COUNTED = /, \d+ rows?$/;

// INFRA-3135, round 25's B2, by the architect's ruling of 2026-11-24 08:07Z: a count beside a
// value is drawn only once the data it counts has answered. The Type list counted the thread kinds
// over the rows read so far, so while the reader's thread read was out or refused it offered
// "Reply, 0 rows" with two reply threads waiting. The questions' count is the Inbox's own read's,
// which answered, so it stands throughout. Every other field counts asks and threads together, so
// none of its values carries a count until the thread read has answered.
test("the Type list draws no count for the thread kinds while the thread read is out or refused, nor does any other field, and the read's counts once it answers", async () => {
  const user = userEvent.setup();
  const asks = [
    issueAsk({ id: "ask-1", question: "First question?" }),
    issueAsk({ id: "ask-2", question: "Second question?" }),
  ];
  const threads = [replyRow("c1"), replyRow("c2")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(asks);
  const getAsk = mockAskReads(asks);
  const getIssue = mockIssueReads();
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const listMyReplies = spyOn(api, "listMyReplies");
  const listUsers = mockPeople();
  try {
    for (const how of ["held", "refused"] as const) {
      listMyReplies.mockReset();
      // Held: the read is never answered. Refused: Dispatch answers it 500.
      if (how === "held") listMyReplies.mockReturnValue(new Promise<never>(() => {}));
      else {
        listMyReplies.mockRejectedValue(
          new ApiError(500, { code: "TEST_REFUSED", error: "test refusal" })
        );
      }
      const { queryClient, unmount } = renderInbox();
      try {
        await screen.findByText("Second question?");
        // The read is out, or Dispatch has refused it.
        await waitFor(() => expect(listMyReplies).toHaveBeenCalled());
        await waitFor(() =>
          expect(queryClient.getQueryState(inboxThreadsQuery().queryKey)?.status).toBe(
            how === "held" ? "pending" : "error"
          )
        );
        expect(rowIds()).toHaveLength(2);
        expect(await fieldOptionNames(user, "Type"), `thread read ${how}`).toEqual([
          "Question, 2 rows",
          "Decision, 0 rows",
          "Approval, 0 rows",
          "Reply",
          "Agent message",
        ]);
        for (const label of ["Priority", "Assignee"]) {
          const names = await fieldOptionNames(user, label);
          expect(names.length, `thread read ${how}: ${label}'s values`).toBeGreaterThan(0);
          expect(
            names.filter((name) => COUNTED.test(name)),
            `thread read ${how}: ${label} counts asks and threads`
          ).toEqual([]);
        }
      } finally {
        unmount();
      }
    }
    // The control: the same Inbox with its thread read answered counts both reply threads.
    listMyReplies.mockReset();
    listMyReplies.mockResolvedValue({
      as_of: "2026-10-12T00:00:00Z",
      limit: 200,
      offset: 0,
      rows: threads,
      total: threads.length,
    });
    const { unmount } = renderInbox();
    try {
      await waitFor(() => expect(rowIds()).toHaveLength(4));
      expect(await fieldOptionNames(user, "Type")).toEqual([
        "Question, 2 rows",
        "Decision, 0 rows",
        "Approval, 0 rows",
        "Reply, 2 rows",
        "Agent message, 0 rows",
      ]);
      for (const label of ["Priority", "Assignee"]) {
        const names = await fieldOptionNames(user, label);
        expect(names.length, `${label}'s values`).toBeGreaterThan(0);
        expect(names.filter((name) => !COUNTED.test(name))).toEqual([]);
      }
    } finally {
      unmount();
    }
  } finally {
    listUsers.mockRestore();
    listMyReplies.mockRestore();
    listComments.mockRestore();
    getIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a failed people read says so and leaves the assignee filter working from the address", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = spyOn(api, "listUsers").mockRejectedValue(new Error("users unreachable"));
  const { unmount } = renderInbox("/?assignee=carol@example.com");
  try {
    await screen.findByText("Nothing matches this filter");
    await openAssignee(user);
    await screen.findByText("Could not load the people list.");
    // What the address filters by is still there to see and to switch off.
    expect(await assigneeOptionTexts()).toEqual(["Unassigned0", "✓carol@example.com0"]);

    const listbox = await screen.findByRole("listbox", { name: "Assignee options" });
    await user.click(within(listbox).getByRole("option", { name: /carol/ }));
    expect(screen.getByTestId("location").textContent).toBe("/?assignee=everyone");
    await user.click(within(listbox).getByRole("option", { name: /Unassigned/ }));
    expect(screen.getByTestId("location").textContent).toBe("/?assignee=unassigned");
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a selected person the people list does not hold is still a row of the assignee control", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox("/?assignee=carol@example.com");
  try {
    await screen.findByText("Nothing matches this filter");
    await openAssignee(user);
    expect(await assigneeOptionTexts()).toEqual([
      "Unassigned0",
      "aliceyou · 1",
      "bob1",
      "✓carol@example.com0",
    ]);
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("an empty list speaks for whoever it was for: nothing needs you, or nothing needs anyone", async () => {
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(
    rows.filter((row) => row.id !== "ask-mine")
  );
  const getAsk = mockAskReads(rows);
  const own = renderInbox();
  try {
    expect(await screen.findByText("Nothing needs you")).toBeTruthy();
  } finally {
    own.unmount();
  }
  getInbox.mockResolvedValue([]);
  const cleared = renderInbox("/?assignee=everyone");
  try {
    expect(await screen.findByText("Nothing needs anyone")).toBeTruthy();
  } finally {
    cleared.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a filter change replaces the history entry instead of piling one up per pick", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const { unmount } = renderInbox(["/agents", "/"]);
  try {
    await screen.findByText("Which approach?");
    await toggleAssignee(user, /bob/);
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/?assignee=alice&assignee=bob")
    );

    await user.click(screen.getByRole("button", { name: "History back" }));
    // Back leaves the Inbox altogether: the pick replaced the entry it was made on.
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/agents"));
  } finally {
    unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a caller supplying onFilterChange (the Inbox drawer) keeps the filter local: the address is untouched", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = mockPeople();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const changes: InboxRoute[] = [];
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RouteProbe />
        <Inbox
          filter={{}}
          keymapScope={DIALOG_SCOPE}
          onFilterChange={(next) => changes.push(next)}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which approach?");
    await toggleAssignee(user, /bob/);
    expect(changes).toEqual([{ assignees: ["alice", "bob"] }]);
    expect(screen.getByTestId("location").textContent).toBe("/");
    // The caller owns the filter, so the page it is embedded on lists what it lists.
    expect(screen.queryByText("Bob's question?")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listUsers.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Assign to me PATCHes the viewer's lowercase login and keeps its row until the save lands", async () => {
  const user = userEvent.setup();
  // A row the server resolved nobody for, which is what the Unassigned filter and "Assign to
  // me" are written against: every project names a default assignee now, so this is a row an
  // older server answers, and the control's own behaviour is what this reads.
  const orphan = issueAsk(
    {
      id: "ask-orphan",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Orphan question?",
    },
    null
  );
  const rows = [orphan];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=unassigned", { pane: false });
  try {
    await screen.findByText("Orphan question?");
    await user.click(screen.getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    // The optimistic assignment takes the row out of this filter at once; the live write keeps
    // it on screen, so `Assigning…` is shown by the control the reader clicked.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Assign CORE-3 to me" }).textContent).toBe(
        "Assigning…"
      )
    );
    expect(rowIds()).toEqual(["ask-orphan"]);
    // Once the server has recorded it, the refetched inbox carries the new assignee, and with
    // it the person the row waits on (`coalesce(a.asked_of, i.assignee, p.default_assignee)`,
    // which the server resolves).
    getInbox.mockResolvedValue([
      {
        ...orphan,
        issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" },
        waiting_on_person: "alice",
      },
    ]);
    await act(async () => {
      save.resolve(narrowIssueOf(orphan, "alice"));
      await save.promise;
    });
    // Nobody's no longer, the row leaves this filter once the save settles and the refetch
    // lands.
    await waitFor(() => expect(rowIds()).toEqual([]));
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a refused Assign to me keeps its row under the Unassigned filter, with the reason and a working Retry", async () => {
  const user = userEvent.setup();
  const orphan = issueAsk(
    {
      id: "ask-orphan",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Orphan question?",
    },
    null
  );
  const rows = [orphan];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const firstSave = Promise.withResolvers<Issue>();
  let attempts = 0;
  const patchIssue = answerIssuePatches().mockImplementation(async () => {
    attempts += 1;
    return attempts === 1 ? firstSave.promise : narrowIssueOf(orphan, "alice");
  });
  const { unmount } = renderInbox("/?assignee=unassigned", { pane: false });
  try {
    await screen.findByText("Orphan question?");
    await user.click(screen.getByRole("button", { name: "Assign CORE-3 to me" }));
    // Assigned optimistically, the row matches nothing this filter lists - and stays.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Assign CORE-3 to me" }).textContent).toBe(
        "Assigning…"
      )
    );
    expect(rowIds()).toEqual(["ask-orphan"]);

    await act(async () => {
      firstSave.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-3 is closed; reopen it first" })
      );
      await firstSave.promise.catch(() => undefined);
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("CORE-3 is closed; reopen it first");
    expect(rowIds()).toEqual(["ask-orphan"]);

    getInbox.mockResolvedValue([
      {
        ...orphan,
        issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" },
        waiting_on_person: "alice",
      },
    ]);
    await user.click(within(alert).getByRole("button", { name: /Retry/ }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowIds()).toEqual([]));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** What the reader sees of a row's control through a write: each drawing `commits` recorded, told
 *  once for each run of commits that drew it. */
function drawnChanges(commits: readonly { readonly drawn: string }[]): string[] {
  return commits
    .map(({ drawn }) => drawn)
    .filter((drawn, at, all) => at === 0 || all[at - 1] !== drawn);
}

/** What the row of `askId` draws of its Assign to me for `issueKey`: the button, held
 *  (`aria-disabled`, `held-press.ts`) or free, and its words; `(no button)`, or `(no row)` while
 *  the list draws no such row. */
function drawnAssign(askId: string, issueKey: string): string {
  const row = document.querySelector(`[data-inbox-row="${askId}"]`);
  if (row === null) return "(no row)";
  const button = row.querySelector(`button[aria-label="Assign ${issueKey} to me"]`);
  if (button === null) return "(no button)";
  const held = button.getAttribute("aria-disabled") === "true" ? "held" : "free";
  return `${held} ${button.textContent}`;
}

// Under the Unassigned filter the optimistic assignment takes the row out of the filter, and the
// Inbox keeps the row while the save is out (`AskAssignWritesProvider`). It lets go of the row in
// the commit that draws the save's answer, while the button is still held from the press
// (`useHeldThroughAnswer`), so the button goes with its row while it is held, and keeps the focus
// and takes no second press until then.
test("Assign to me stays held until it goes with its row, in the commit that draws its answer", async () => {
  const user = userEvent.setup();
  const orphan = issueAsk(
    {
      id: "ask-orphan",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Orphan question?",
    },
    null
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([orphan]);
  const getAsk = mockAskReads([orphan]);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const commits = recordCommits<null>(() => drawnAssign("ask-orphan", "CORE-3"));
  const { unmount } = renderInbox("/?assignee=unassigned", { commits, pane: false });
  try {
    await screen.findByText("Orphan question?");
    const button = screen.getByRole("button", { name: "Assign CORE-3 to me" });
    const fromPress = commits.markAt(button, "click");
    await user.click(button);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    getInbox.mockResolvedValue([
      {
        ...orphan,
        issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" },
        waiting_on_person: "alice",
      },
    ]);
    await act(async () => {
      save.resolve(narrowIssueOf(orphan, "alice"));
      await save.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual([]));

    // The commit that draws the answer is the one the row goes in: the button is held until
    // then.
    expect(drawnChanges(fromPress())).toEqual(["held Assigning…", "(no row)"]);
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The hand-on's optimistic `waiting_on_person` takes the row out of the reader's own list as
// the write starts, which would unmount the card holding the control: the list holds the write
// itself, by ask, through the context it provides (`AskHandOnWritesContext`), and keeps the row
// while the write is out, so the refusal is shown where the reader pressed rather than lost to a
// remount.
test("a refused hand-on keeps its row in the reader's own list, with the reason and a working Retry", async () => {
  const user = userEvent.setup();
  // Handed to alice on an issue bob holds, so the row is alice's and handing it back to bob
  // takes it out of her list.
  const mine = issueAsk({
    asked_of: "alice",
    id: "ask-mine",
    issue: { assignee: "bob", key: "CORE-4", title: "Bob's issue" },
    issue_key: "CORE-4",
    question: "Hand this on?",
  });
  const rows = [mine];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const people = mockPeople();
  const firstHandOn = Promise.withResolvers<InboxRow>();
  let attempts = 0;
  const handedBack = { ...mine, asked_of: null, waiting_on_person: "bob" };
  const handAskTo = spyOn(api, "handAskTo").mockImplementation(async () => {
    attempts += 1;
    return attempts === 1 ? firstHandOn.promise : handedBack;
  });
  const { unmount } = renderInbox("/", { pane: false });
  try {
    await screen.findByText("Hand this on?");
    await handOnTo(user, rowOf("ask-mine"), "bob");
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-mine", "bob", keyedAttempt));
    // Handed on optimistically, the row waits on somebody else - and stays.
    await waitFor(() => expect(screen.getByText("Handing on…")).toBeTruthy());
    expect(rowIds()).toEqual(["ask-mine"]);

    await act(async () => {
      firstHandOn.reject(
        new ApiError(409, {
          code: "ASK_NOT_OPEN",
          error: "only open asks may be handed to another person",
        })
      );
      await firstHandOn.promise.catch(() => undefined);
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("only open asks may be handed to another person");
    expect(rowIds()).toEqual(["ask-mine"]);

    // The card's own Retry writes again; the row leaves once the server takes it and the
    // refetched inbox says bob waits on it.
    getInbox.mockResolvedValue([handedBack]);
    await user.click(within(alert).getByRole("button", { name: /Retry/ }));
    await waitFor(() => expect(handAskTo).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowIds()).toEqual([]), { timeout: 4000 });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
    handAskTo.mockRestore();
    people.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// INFRA-3135, round 12: the hand-on keeps a press on its badge, or in its list, from the row under
// it by stopping the press, and that also kept the press from the document, where every open panel
// listened for a press outside it: the Filter panel stayed open under a press on a card's badge,
// and two cards' lists could stand open at once. A panel reads that press in the capture phase.
test("a press on a card's Asked of badge closes the open Filter panel, and another card's open hand-on list", async () => {
  const user = userEvent.setup();
  const rows = [
    issueAsk({ id: "ask-a", question: "First question?" }),
    issueAsk({
      id: "ask-b",
      issue: { assignee: "alice", key: "CORE-2", title: "Other thing" },
      issue_key: "CORE-2",
      question: "Second question?",
    }),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const people = mockPeople();
  const { unmount } = renderInbox("/", { pane: false });
  const badge = (askId: string) =>
    within(rowOf(askId)).getByRole("button", { name: pickNamed("Hand this question to") });
  const handOnLists = () =>
    screen.queryAllByRole("listbox", { name: "Hand this question to options" });
  try {
    await screen.findByText("Second question?");
    await openFilterFields(user);
    expect(screen.getByRole("dialog", { name: "Filter" })).toBeTruthy();
    await user.click(badge("ask-a"));
    expect(
      screen.queryByRole("dialog", { name: "Filter" }),
      "a press on the badge left the Filter panel open"
    ).toBeNull();
    await waitFor(() => expect(handOnLists()).toHaveLength(1));
    const first = handOnLists()[0];

    await user.click(badge("ask-b"));
    expect(
      first,
      "a press on another card's badge left the first list open"
    ).not.toBeInTheDocument();
    expect(handOnLists()).toHaveLength(1);
    expect(badge("ask-b")).toHaveAttribute("aria-expanded", "true");
  } finally {
    unmount();
    people.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a sibling ask of the same issue leaving the list does not drop an in-flight Assign to me: its refusal still shows", async () => {
  const user = userEvent.setup();
  // Two rows the server resolved nobody for, which is what "Assign to me" is written against.
  const askA = issueAsk(
    {
      id: "ask-a",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Question A?",
    },
    null
  );
  const askB = issueAsk(
    {
      id: "ask-b",
      issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
      issue_key: "CORE-3",
      question: "Question B?",
    },
    null
  );
  const rows = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount } = renderInbox("/?assignee=everyone", { pane: false });
  try {
    await screen.findByText("Question A?");
    const rowA = () => screen.getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    await user.click(
      within(rowA() as HTMLElement).getByRole("button", { name: "Assign CORE-3 to me" })
    );
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    // Both rows are the viewer's optimistically, so B loses its own control; A's write is live.
    await waitFor(() =>
      expect(
        within(rowA() as HTMLElement).getByRole("button", { name: "Assign CORE-3 to me" })
          .textContent
      ).toBe("Assigning…")
    );
    expect(
      within(rowOf("ask-b")).queryByRole("button", { name: /Assign .* to me/ })
    ).not.toBeInTheDocument();

    // B is answered elsewhere and leaves the list while A's save is still in flight.
    act(() => {
      queryClient.setQueriesData<InboxRow[]>({ queryKey: ["inbox"] }, (current) =>
        current?.filter((row) => row.id !== "ask-b")
      );
    });
    await waitFor(() => expect(screen.queryByText("Question B?")).not.toBeInTheDocument());
    expect(
      within(rowA() as HTMLElement).getByRole("button", { name: "Assign CORE-3 to me" })
    ).toBeTruthy();

    // The server refuses: A rolls back, and its own control shows the reason.
    await act(async () => {
      save.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-3 is closed; reopen it first" })
      );
      await save.promise.catch(() => undefined);
    });
    const alert = await within(rowA() as HTMLElement).findByRole("alert");
    expect(alert).toHaveTextContent("CORE-3 is closed; reopen it first");
    expect(rowIds()).toContain("ask-a");
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Blocked on you counts the reader's own questions, and does not move when the filter widens", async () => {
  const mine = issueAsk({ id: "ask-mine" });
  const bobs = issueAsk({
    id: "ask-bob",
    issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
    issue_key: "CORE-2",
    question: "Bob's question?",
  });
  // Nobody is named on this issue, so its question reaches the project's default assignee.
  const orphan = issueAsk({
    id: "ask-orphan",
    issue: { assignee: null, key: "CORE-3", title: "Nobody's issue" },
    issue_key: "CORE-3",
    question: "Orphan question?",
  });
  const rows = [mine, bobs, orphan];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const own = renderInbox();
  try {
    await screen.findByText("Which approach?");
    expect(rowIds()).toEqual(["ask-mine"]);
    // One question of the three is on the reader's own issue; the needs-you control counts
    // that one, as the banner it replaced did (INFRA-3135, R7).
    expect(screen.getByRole("button", { name: /Blocked on you: 1 item/ })).toBeTruthy();
  } finally {
    own.unmount();
  }
  const everyone = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    expect([...rowIds()].sort()).toEqual(["ask-bob", "ask-mine", "ask-orphan"]);
    // The list widened; what waits on the reader did not, so the control says the same thing.
    expect(screen.getByRole("button", { name: /Blocked on you: 1 item/ })).toBeTruthy();
  } finally {
    everyone.unmount();
  }
  const narrowed = renderInbox(`/?assignee=${FIXTURE_PROJECT_DEFAULT}`);
  try {
    await screen.findByText("Orphan question?");
    expect(screen.getByRole("button", { name: /Blocked on you: 1 item/ })).toBeTruthy();
  } finally {
    narrowed.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the needs-you control narrows the list to what waits on the reader and offers the way back", async () => {
  const user = userEvent.setup();
  const rows = filterRows();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?assignee=everyone");
  try {
    await screen.findByText("Bob's question?");
    await user.click(screen.getByRole("button", { name: /Blocked on you: 1 item/ }));

    // The press writes the section and takes the assignee parameter off, so the list is the
    // reader's own again (INFRA-3135, R7).
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/?section=needs-you")
    );
    expect(rowIds()).toEqual(["ask-mine"]);

    // The narrowing is a chip like any other, so **Clear filters** is the way back out of it.
    expect(screen.getByRole("button", { name: "Remove Waiting on you filter" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/"));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The number counts the whole inbox, so a press that kept a field narrowing or the search text on
// would list fewer asks than it says (R7: the page shows every item the number counts). It takes
// both off, as it takes the assignee off, and keeps how the list is sorted and banded.
test("the needs-you control takes the fields and the search off, so the list holds every ask it counts", async () => {
  const user = userEvent.setup();
  const rows = [
    issueAsk({ id: "ask-mine" }),
    inboxIssue("ask-urgent", "CORE-4", "Ship the fix tonight?", { priority: 0 }),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?priority=P0&q=ship&sort=urgency&group=waiting-on");
  try {
    await screen.findByText("Ship the fix tonight?");
    expect(rowIds()).toEqual(["ask-urgent"]);
    await user.click(screen.getByRole("button", { name: /^Blocked on you: 2 items/ }));

    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe(
        "/?section=needs-you&sort=urgency&group=waiting-on"
      )
    );
    expect([...rowIds()].sort()).toEqual(["ask-mine", "ask-urgent"]);
    expect(screen.getByRole("searchbox", { name: "Search asks" })).toHaveValue("");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Snooze folds a row into Later until the reader un-snoozes it", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  // The server's answer moves with the writes, so the refetch each write triggers returns what
  // the server would now hold rather than resurrecting the pre-snooze list.
  let served: InboxRow[] = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const unsnoozeAsk = answerAskUnsnoozes().mockImplementation(async (id) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: null } : row));
  });
  const getAsk = mockAskReads([askA, askB]);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(screen.getByRole("button", { name: /Blocked on you: 2 items/ })).toBeTruthy();

    const row = screen.getByTestId("ask-ask-b").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    await pickIn(
      user,
      () => within(row).getByLabelText("Snooze CORE-2: Which format?"),
      "Tomorrow"
    );

    // Folded by default: the row leaves the list, and the band's count says where it went.
    await waitFor(() => expect(headings()).toEqual(["Waiting on you (1)", "Later (1)"]));
    expect(rowIds()).toEqual(["ask-a"]);
    expect(screen.getByRole("button", { name: /Blocked on you: 1 item/ })).toBeTruthy();
    expect(snoozeAsk).toHaveBeenCalledTimes(1);
    const [, body] = snoozeAsk.mock.calls[0] ?? [];
    expect(Date.parse(body?.snoozed_until ?? "")).toBeGreaterThan(Date.now());

    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    const later = screen.getByTestId("ask-ask-b").closest<HTMLElement>("[data-inbox-row]");
    expect(later?.getAttribute("data-inbox-section")).toBe("later");

    await user.click(
      within(later as HTMLElement).getByRole("button", { name: "Un-snooze CORE-2: Which format?" })
    );
    await waitFor(() => expect(headings()).toEqual(["Waiting on you (2)"]));
    expect(unsnoozeAsk).toHaveBeenCalledWith("ask-b", undefined, keyedAttempt);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(screen.getByRole("button", { name: /Blocked on you: 2 items/ })).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
    unsnoozeAsk.mockRestore();
  }
});

test("an agent's reply on a snoozed row leaves it in Later and off Waiting on you", async () => {
  const user = userEvent.setup();
  const snoozed = issueAsk({
    id: "ask-snoozed",
    question: "Which format?",
    snoozed_until: soon(),
    waiting_on: "agent",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([snoozed]);
  const getAsk = mockAskReads([snoozed]);
  const { queryClient, unmount } = renderInbox("/", { pane: false });
  try {
    await waitFor(() => expect(headings()).toEqual(["Later (1)"]));
    expect(rowIds()).toEqual([]);

    // The agent answers: the turn is the reader's again and the row carries the agent's reply -
    // exactly the change that could pull a deferred ask back onto the list.
    act(() => {
      queryClient.setQueryData<InboxRow[]>(
        ["inbox"],
        [
          {
            ...snoozed,
            last_reply: {
              author: { id: "session-1", kind: "session" },
              created_at: "2026-09-11T01:00:00Z",
            },
            waiting_on: "human",
          },
        ]
      );
    });

    expect(headings()).toEqual(["Later (1)"]);
    expect(screen.queryByText("Waiting on you")).not.toBeInTheDocument();
    expect(screen.queryByText(/Blocked on you/)).not.toBeInTheDocument();
    expect(rowIds()).toEqual([]);

    // Opening the band shows it there, still deferred, with its return time.
    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    const row = await screen.findByTestId("ask-ask-snoozed");
    expect(row.closest("[data-inbox-section]")?.getAttribute("data-inbox-section")).toBe("later");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a snooze whose moment has passed is an ordinary row again", async () => {
  const lapsed = issueAsk({
    id: "ask-lapsed",
    question: "Which format?",
    snoozed_until: "2020-01-01T00:00:00.000Z",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([lapsed]);
  const getAsk = mockAskReads([lapsed]);
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Which format?");
    expect(headings()).toEqual(["Waiting on you (1)"]);
    expect(rowIds()).toEqual(["ask-lapsed"]);
    expect(screen.getByRole("button", { name: /Blocked on you: 1 item/ })).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a snooze still saving says Snoozing…, not Un-snoozing…, in the band its row is leaving", async () => {
  const user = userEvent.setup();
  const row = issueAsk({ id: "ask-b", question: "Which format?" });
  let served: InboxRow[] = [row];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  // Held open, so the render under test is the one while the write is in flight.
  let settle: (() => void) | undefined;
  const snoozeAsk = answerAskSnoozes().mockImplementation(
    (id, { snoozed_until: until }) =>
      new Promise((resolve) => {
        settle = () => {
          served = served.map((each) =>
            each.id === id ? { ...each, snoozed_until: until } : each
          );
          resolve({ snoozed_until: until });
        };
      })
  );
  const getAsk = mockAskReads([row]);
  const { unmount } = renderInbox("/", { pane: false });
  try {
    await screen.findByText("Which format?");
    const snooze = screen.getByLabelText("Snooze CORE-1: Which format?");
    await pickIn(user, () => snooze, "Tomorrow");

    // The optimistic update has already given the row the moment it is going to, and the row
    // stays in the band the reader saw it in until the save lands, but the label must say what
    // the reader did, not where the row is going. The optimistic return time is shown beside it;
    // that is the snooze being saved.
    await waitFor(() => expect(document.querySelector("[data-inbox-snoozed-until]")).toBeTruthy());
    expect(screen.getByText("Snoozing…")).toBeTruthy();
    expect(screen.queryByText("Un-snoozing…")).not.toBeInTheDocument();

    // Once it settles the row moves into Later and reports its state again, not the last thing
    // done to it.
    await act(async () => {
      settle?.();
      await Promise.resolve();
    });
    await waitFor(() => expect(headings()).toEqual(["Later (1)"]));
    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Un-snooze CORE-1: Which format?" }).textContent
      ).toBe("Un-snooze")
    );
    expect(screen.queryByText("Snoozing…")).not.toBeInTheDocument();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("a refused snooze rolls the row back and says why, where the reader can still see it", async () => {
  const user = userEvent.setup();
  const row = issueAsk({ id: "ask-b", question: "Which format?" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([row]);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
  );
  const getAsk = mockAskReads([row]);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    const snooze = screen.getByLabelText("Snooze CORE-1: Which format?");
    await pickIn(user, () => snooze, "Tomorrow");

    // The row comes back to the band it was in, and the reason comes with it - the control is
    // kept mounted through the fold, so the refusal is not thrown away with the row.
    await waitFor(() => expect(headings()).toEqual(["Waiting on you (1)"]));
    expect(rowIds()).toEqual(["ask-b"]);
    expect(screen.getByText("snoozed_until must be in the future")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Blocked on you: 1 item/ })).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A snooze is a `PUT` of the row's moment. One the reader stopped may have been saved, so the row
// neither says it failed nor shows the stopped write's own words, which are a composer's and
// speak of a message: it says what its Retry does with the snooze.
test("a snooze the reader stopped says what Retry does, not a message's words", async () => {
  const row = issueAsk({ id: "ask-b", question: "Which format?" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([row]);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-b/snooze", new TypeError("Failed to fetch"))
  );
  const getAsk = mockAskReads([row]);
  const { queryClient, unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    jest.useFakeTimers();
    const snooze = screen.getByLabelText("Snooze CORE-1: Which format?");
    await pickIn(userOnFakeClock(), () => snooze, "Tomorrow");
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the snooze", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A press carries every value its Retry sends, the ask it is of among them: a Retry of a snooze
// the reader stopped is of that press's ask under that press's key. The list holds each write by
// its ask, so the control drawn for another ask shows none of it, and the control drawn for the
// ask again shows the stopped write and its Retry. The case draws one control for one ask, then
// for another, then for the first again.
test("a snooze's Retry is of the ask its press was of, under its key, from the control drawn for that ask again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-a/snooze", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const drawn = (askId: string) => (
    <QueryClientProvider client={queryClient}>
      <SnoozeWrites>
        <SnoozeControl askId={askId} label={askId} snoozedUntil={null} />
      </SnoozeWrites>
    </QueryClientProvider>
  );
  const view = render(drawn("ask-a"));
  try {
    const snooze = screen.getByLabelText("Snooze ask-a");
    await pickIn(user, () => snooze, "Tomorrow");
    await stopTheLateWrite(queryClient);
    const stoppedKey = snoozeAsk.mock.calls[0]?.[2]?.key;
    const stopped = snoozeAsk.mock.calls.length;

    view.rerender(drawn("ask-b"));
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();

    view.rerender(drawn("ask-a"));
    // Dispatch answers the Retry once its request is read, inside `act`: the landing moves the
    // list's state, and an answer given at once could land as the click settles, outside `act`.
    const retried = Promise.withResolvers<{ snoozed_until: string }>();
    snoozeAsk.mockImplementation(() => retried.promise);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(1_000);

    expect(snoozeAsk).toHaveBeenCalledTimes(stopped + 1);
    const again = snoozeAsk.mock.calls.at(-1);
    expect(again?.[0]).toBe("ask-a");
    expect(again?.[2]?.key).toBe(stoppedKey);
    await act(async () => {
      retried.resolve({ snoozed_until: again?.[1]?.snoozed_until ?? "" });
      await settle();
    });
  } finally {
    realClock();
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

// The row a snooze out asks the Inbox to hold is its press's ask too: with the control drawn for
// another ask while the snooze of the first is out, the list keeps the first ask's row held,
// holds no row for the ask the control shows, and lets the first go once Dispatch answers.
test("a snooze out when the control is drawn for another ask holds the row of the ask it was pressed on", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<{ snoozed_until: string }>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => landing.promise);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  /** Each set of asks the list held for a snooze out, in order. */
  const held: string[][] = [];
  const onOut = (out: ReadonlySet<string>) => {
    held.push([...out]);
  };
  const drawn = (askId: string) => (
    <QueryClientProvider client={queryClient}>
      <SnoozeWrites onOut={onOut}>
        <SnoozeControl askId={askId} label={askId} snoozedUntil={null} />
      </SnoozeWrites>
    </QueryClientProvider>
  );
  const view = render(drawn("ask-a"));
  try {
    const snooze = screen.getByLabelText("Snooze ask-a");
    await pickIn(user, () => snooze, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    expect(held).toEqual([[], ["ask-a"]]);

    view.rerender(drawn("ask-b"));
    expect(held).toEqual([[], ["ask-a"]]);

    landing.resolve({ snoozed_until: "2026-11-17T09:00:00Z" });
    await waitFor(() => expect(held.length).toBe(3));
    expect(held).toEqual([[], ["ask-a"], []]);
  } finally {
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

/** What the row of `askId` draws of its Snooze: the menu's trigger (`data-inbox-snooze`), which
 *  stays at its one place in the row through a write, held (`aria-disabled`) or free; `(no row)`
 *  while the list draws no such row. */
function drawnSnoozeTrigger(askId: string): string {
  const row = document.querySelector(`[data-inbox-row="${askId}"]`);
  if (row === null) return "(no row)";
  const trigger = row.querySelector("[data-inbox-snooze]");
  if (trigger === null) return "(no trigger)";
  return `Snooze ${trigger.getAttribute("aria-disabled") === "true" ? "held" : "free"}`;
}

// A Snooze pick folds its row into Later, which starts folded, in the commit after the write's
// answer, where the Inbox lets go of the row. The trigger the menu handed the focus back to stays
// drawn and held from the pick through that commit and goes with its row in the commit its hold
// ends. The write is out for two frames and their tasks here, as a server slower than a frame
// keeps it, the trigger keeping the focus, and the snooze's landing hands the focus on to the item
// after the row (R8) - here the row before it, the one row left - not to the page.
test("a Snooze pick holds its menu's trigger through the commit after its answer, and the focus goes to the row left when the row folds away", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  let served: InboxRow[] = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads([askA, askB]);
  const commits = recordCommits<null>(() => drawnSnoozeTrigger("ask-b"));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <commits.Recorded host={null}>
              <Inbox pane={false} />
            </commits.Recorded>
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which format?");
    const trigger = within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?");
    await user.click(trigger);
    const tomorrow = optionOf(trigger, "Tomorrow");
    const fromPick = commits.markAt(tomorrow, "click");
    await user.click(tomorrow);
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    await afterFrameAndTasks();
    await afterFrameAndTasks();
    expect(trigger).toHaveFocus();

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    expect(drawnChanges(fromPick())).toEqual(["Snooze held", "(no row)"]);
    await afterFrameAndTasks();

    expect(rowOf("ask-a")).toHaveFocus();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

/** What the row of `askId` draws of its Snooze control, named for `label`: the Snooze list's
 *  trigger, which every row draws, a snoozed one's included, or Un-snooze where no list is drawn,
 *  held (`aria-disabled`) or free; `(no row)` while the list draws no such row. */
function drawnSnooze(askId: string, label: string): string {
  const row = document.querySelector<HTMLElement>(`[data-inbox-row="${askId}"]`);
  if (row === null) return "(no row)";
  const control =
    within(row).queryByLabelText(`Snooze ${label}`) ??
    row.querySelector(`button[aria-label="Un-snooze ${label}"]`);
  if (control === null) return "(no control)";
  const held = control.getAttribute("aria-disabled") === "true" ? "held" : "free";
  return `${control.matches("[data-inbox-snooze]") ? "Snooze" : "Un-snooze"} ${held}`;
}

/** Presses what the row of `askId` draws of its Snooze control, named for `label`, as a reader
 *  would: a press of the Snooze list's trigger and a pick of Next week from the list it opens - a
 *  held trigger opens none - or a click on Un-snooze where that is drawn instead. */
async function pressDrawnSnooze(user: UserEvent, askId: string, label: string): Promise<void> {
  const row = rowOf(askId);
  const unsnooze = within(row).queryByRole("button", { name: `Un-snooze ${label}` });
  if (unsnooze === null) {
    const trigger = within(row).getByLabelText(`Snooze ${label}`);
    await user.click(trigger);
    if (trigger.getAttribute("aria-expanded") === "true") {
      await user.click(within(panelOf(trigger)).getByRole("option", { name: "Next week" }));
    }
  } else {
    await user.click(unsnooze);
  }
}

// A Snooze pick keeps its row in the band the reader saw it in while the write is out, held and
// showing the moment being saved, and the row moves into Later, which starts folded, in the commit
// after the write's answer, where the Inbox lets go of the row. The list stays drawn and held from
// the pick through that commit - the optimistic moment would draw Un-snooze before it - and goes
// with its row in the commit its hold ends. The write is out for two frames and their tasks here,
// as a server slower than a frame keeps it, and the list's trigger keeps the focus all that time;
// once the row has gone the snooze's landing hands the focus to the row that moves up into its
// place, the item after it (R8), not to the page.
test("a Snooze pick keeps its row in its band, held, through the commit after its answer, and the focus goes to the row that moves up when the row folds away", async () => {
  const user = userEvent.setup();
  const askA = issueAsk({ id: "ask-a" });
  const askB = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const askC = issueAsk({
    id: "ask-c",
    issue: { assignee: "alice", key: "CORE-3", title: "Third issue" },
    issue_key: "CORE-3",
    question: "Which owner?",
  });
  let served: InboxRow[] = [askA, askB, askC];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads([askA, askB, askC]);
  const commits = recordCommits<null>(() => drawnSnooze("ask-b", "CORE-2: Which format?"));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <commits.Recorded host={null}>
              <Inbox pane={false} />
            </commits.Recorded>
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which format?");
    const bands = headings();
    const band = rowOf("ask-b").getAttribute("data-inbox-group");
    const trigger = within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?");
    act(() => trigger.focus());
    await user.click(trigger);
    const tomorrow = within(panelOf(trigger)).getByRole("option", { name: "Tomorrow" });
    const fromPick = commits.markAt(tomorrow, "click");
    await user.click(tomorrow);
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    await afterFrameAndTasks();
    await afterFrameAndTasks();
    expect(trigger).toHaveFocus();
    // Out, the row is where the reader saw it, with the moment being saved before its list.
    expect(headings()).toEqual(bands);
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(rowOf("ask-b").getAttribute("data-inbox-group")).toBe(band);
    expect(rowOf("ask-b").querySelector("[data-inbox-snoozed-until]")).not.toBeNull();

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-c"]));
    expect(drawnChanges(fromPick())).toEqual(["Snooze held", "(no row)"]);
    expect(screen.getByRole("button", { name: /^Later \(1\)/ })).toBeTruthy();
    await afterFrameAndTasks();

    expect(rowOf("ask-c")).toHaveFocus();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// An Un-snooze press is held to the rule a Snooze pick is: its row stays in Later while the write
// is out, and Un-snooze stays drawn there, held, from the press through the commit after the
// answer, beside the row's Snooze list, which every row draws and the write holds too - so it goes
// with its row in the commit its hold ends. The write is out for two frames and their tasks here,
// as a server slower than a frame keeps it. The reader has folded the band the row returns to, so
// the row leaves the page as it leaves Later.
test("an Un-snooze press keeps its button held in Later through the commit after its answer, and its row leaves Later with the button still held", async () => {
  const user = userEvent.setup();
  const askA = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const askB = inboxIssue("ask-b", "CORE-2", "Which format?", { snoozed_until: soon() });
  const askC = inboxIssue("ask-c", "CORE-3", "Which owner?", { snoozed_until: soon() });
  let served: InboxRow[] = [askA, askB, askC];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const unsnoozeAsk = answerAskUnsnoozes().mockImplementation(async (id) => {
    await landing.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: null } : row));
  });
  const getAsk = mockAskReads([askA, askB, askC]);
  const commits = recordCommits<null>(() => drawnSnooze("ask-b", "CORE-2: Which format?"));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <commits.Recorded host={null}>
              <Inbox pane={false} />
            </commits.Recorded>
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which approach?");
    await user.click(screen.getByRole("button", { name: /^Waiting on you \(1\)/ }));
    await user.click(screen.getByRole("button", { name: /^Later \(2\)/ }));
    const button = await screen.findByRole("button", { name: "Un-snooze CORE-2: Which format?" });
    act(() => button.focus());
    const fromPress = commits.markAt(button, "click");
    await user.click(button);
    await waitFor(() => expect(unsnoozeAsk).toHaveBeenCalledTimes(1));
    await afterFrameAndTasks();
    await afterFrameAndTasks();
    // Out, the row is where the reader saw it.
    expect(rowIds()).toEqual(["ask-b", "ask-c"]);
    expect(rowOf("ask-b")).toHaveAttribute("data-inbox-section", "later");

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    await afterFrameAndTasks();
    expect(drawnChanges(fromPress())).toEqual(["Snooze held", "(no row)"]);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    unsnoozeAsk.mockRestore();
  }
});

// The bar's Snooze over marked rows is held to the rule a row's own pick is: each marked row stays
// in the band the reader saw it in while the write is out, and they move into Later together, in
// the commit in which the bar lets go of them, once the write lands.
test("the bar's Snooze keeps its rows in their band until its write lands", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which owner?");
    await user.click(within(rowOf("ask-a")).getByRole("checkbox", { name: /^Select / }));
    await user.click(within(rowOf("ask-b")).getByRole("checkbox", { name: /^Select / }));
    await screen.findByText("2 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));

    // Out, both rows are where the reader saw them.
    expect(screen.getByText("Snoozing 2…")).toBeTruthy();
    expect(headings()).toEqual(["Waiting on you (3)"]);
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    expect(headings()).toEqual(["Waiting on you (1)", "Later (2)"]);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The bar's Snooze write that never answers - offline, each try failing at the network - holds the
// bar's Snooze until its deadline and no longer: past it the write is still tried, and the bar
// still counts it, and a pick over another marked ask then snoozes that one.
test("the bar's Snooze lets go at its write deadline and snoozes again", async () => {
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-a/snooze", new TypeError("Failed to fetch"))
  );
  const getAsk = mockAskReads(rows);
  const { queryClient, unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    await userEvent
      .setup()
      .click(within(rowOf("ask-a")).getByRole("checkbox", { name: /^Select / }));
    await screen.findByText("1 selected");
    const picker = screen.getByRole<HTMLButtonElement>("button", { name: "Snooze selected asks" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    // The bar's list opens on its first choice, which Enter picks.
    await heldUntilLate(user, picker, {
      label: "Ask snoozed",
      name: "the bar's Snooze",
      press: "{Enter}{Enter}",
      queryClient,
      sent: () => pressesOf([snoozeAsk]),
    });
    expect(screen.getByText("Snoozing 1…"), "the bar, past the deadline").toBeTruthy();
    await user.click(within(rowOf("ask-b")).getByRole("checkbox", { name: /^Select / }));
    act(() => picker.focus());
    await user.keyboard("{Enter}{Enter}");
    await tick(1_000);
    expect(
      snoozeAsk.mock.calls.map(([id]) => id),
      "a pick past the deadline snoozes the ask marked since"
    ).toContain("ask-b");
    expect(pressesOf([snoozeAsk]), "a pick past the deadline sends a write of its own").toBe(2);
  } finally {
    realClock();
    unmount();
    queryClient.clear();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The bar's Snooze deep in a long list folds the two rows marked into Later, the row marked last
// among them. The row after them moves up into its place, where the reader can see it, and the row
// takes the focus (R8: the item after the last of the rows acted on is selected and its row has
// the keyboard focus), with no scroll. The list's first row, nine rows above the window, would
// scroll the list back to its top and throw the reader's place away.
test("a bulk Snooze deep in a long list hands the focus to the row that moves up into the place of the row marked last, with no scroll", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 20 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`)
  );
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  // Each row's mark is drawn where its row stands now, 100 px a row, with the tenth row at the
  // window's top: happy-dom lays nothing out, and the hand-off reads what the reader can see.
  const laidOut = layRowsOut("[data-inbox-row]", 9);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Question 20?");
    // The rows in the order the list draws them, and the three this case reads.
    const [marked, last, next] = rowIds().slice(11, 14);
    await user.click(within(rowOf(marked)).getByRole("checkbox", { name: /^Select / }));
    await user.click(within(rowOf(last)).getByRole("checkbox", { name: /^Select / }));
    await screen.findByText("2 selected");
    const landing = rowOf(next);
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(rowIds()).not.toContain(last));
    expect(screen.queryByRole("toolbar", { name: "Selected rows" })).not.toBeInTheDocument();
    await waitFor(() => expect(landing).toHaveFocus());
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    // The bar's list kept its highlighted choice in sight inside its own panel as it opened
    // (`MultiSelect`); nothing else is scrolled. Each is read by its words, so a failure prints
    // words and not a DOM element.
    expect(
      moves.scrolled
        .filter((element) => element.getAttribute("role") !== "option")
        .map((element) => element.outerHTML.slice(0, 120))
    ).toEqual([]);
  } finally {
    unmount();
    moves.restore();
    laidOut();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The same Snooze by the keys, below the column's width: `x` marks two rows, and `b` opens the
// bar's Snooze from the row marked last. Its pick gives the focus back to that row (`MultiSelect`)
// with no scroll: the row is about to fold into Later, and the hand-off hands the reader on to the
// row after the marked ones, which moves up into the first one's place (R8, and 9feff4e4's "the
// next row takes the focus with no scroll while the rows still stand"). A card is tall, and the
// engine's own scroll of one a focus finds partly out of sight moved the list 230 px in card mode
// and 214 px in the drawer, and the row after them ended at the top edge (the tester's round 16).
test("a bulk Snooze by x and b focuses no row with a scroll, and the row after the marked ones takes the focus at the first one's place", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 20 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`)
  );
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  // 100 px a row, with the tenth row at the window's top.
  const laidOut = layRowsOut("[data-inbox-row]", 9);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Question 20?");
    const [first, last, next] = rowIds().slice(11, 14) as [string, string, string];
    for (const id of [first, last]) {
      act(() => rowOf(id).focus());
      await user.keyboard("x");
    }
    await screen.findByText("2 selected");
    const place = rowOf(first).getBoundingClientRect().top;
    const asked = moves.focused.length;

    await user.keyboard("b");
    await user.keyboard("2");
    await waitFor(() => expect(rowIds()).not.toContain(last));
    const landing = rowOf(next);
    await waitFor(() => expect(landing).toHaveFocus());
    // Every focus the page asked a row to take, from `b` on, asked it with no scroll: the row the
    // pick gave it back to, and the row the hand-off gave it to.
    expect(
      moves.focused
        .slice(asked)
        .filter(
          ({ options, target }) => target.matches("[data-inbox-row]") && !options?.preventScroll
        )
        .map(({ target }) => target.getAttribute("data-inbox-row"))
    ).toEqual([]);
    expect(landing.getBoundingClientRect().top).toBe(place);
    // The bar's list kept its highlighted choice in sight inside its own panel as it opened; nothing
    // else is scrolled.
    expect(
      moves.scrolled
        .filter((element) => element.getAttribute("role") !== "option")
        .map((element) => element.outerHTML.slice(0, 120))
    ).toEqual([]);
  } finally {
    unmount();
    moves.restore();
    laidOut();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

/** Starts `window.scrollY` at `scrollY` and makes `window.scrollBy` move it, as happy-dom does
 *  not, for `layRowsDown` and `layItemsDown`. Answers the function that gives the window its own
 *  `scrollY` and `scrollBy` back. */
function scrollWindowFrom(scrollY: number): () => void {
  const scrollYWas = Object.getOwnPropertyDescriptor(window, "scrollY");
  const scrollByWas = window.scrollBy;
  const scrollTo = (value: number) =>
    Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
  scrollTo(scrollY);
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    const by = typeof x === "number" ? y : x.top;
    if (by !== undefined) scrollTo(window.scrollY + by);
  }) as typeof window.scrollBy;
  return () => {
    window.scrollBy = scrollByWas;
    if (scrollYWas === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollYWas);
    }
  };
}

/** How tall `layRowsDown` draws each row. */
const LAID_ROW_PX = 100;

/** Lays the Inbox's rows down a page the window scrolls, as happy-dom does not: each row and its
 *  mark `LAID_ROW_PX` tall, stacked in the order the list draws them below `above()`, the height
 *  of what stands above the rows now, less `window.scrollY`. The list's own box (the anchor's
 *  root) starts where the rows do. `window.scrollY` starts at `scrollY`, and `window.scrollBy`
 *  moves it (`scrollWindowFrom`), so a row the list's own scroll keeping (`ViewportAnchor`) holds
 *  in place moves the window. Every other element keeps happy-dom's own box. Answers the function
 *  that gives happy-dom its own boxes and scroll back. */
function layRowsDown(scrollY: number, above: () => number = () => 0): () => void {
  const real = Element.prototype.getBoundingClientRect;
  const scrolled = scrollWindowFrom(scrollY);
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.matches('[data-testid="inbox-list"] > *')) {
      const count = document.querySelectorAll("[data-inbox-row]").length;
      return new DOMRect(0, above() - window.scrollY, 200, count * LAID_ROW_PX);
    }
    const row = this.matches('[data-inbox-row], input[type="checkbox"]')
      ? this.closest("[data-inbox-row]")
      : null;
    if (row === null) return real.call(this);
    const rows = [...document.querySelectorAll("[data-inbox-row]")];
    const top = above() + rows.indexOf(row) * LAID_ROW_PX - window.scrollY;
    return new DOMRect(0, top, 200, LAID_ROW_PX);
  });
  return () => {
    rect.mockRestore();
    scrolled();
  };
}

/** The mark of the row `id` names, as the list draws it now. */
function markOf(id: string): HTMLElement {
  const mark = document.querySelector<HTMLElement>(
    `[data-inbox-row="${id}"] input[type="checkbox"]`
  );
  if (mark === null) throw new Error(`${id} mark missing`);
  return mark;
}

/** Lists `count` comment threads in the Inbox, and `asks`, each card's issue, comments and thread
 *  answered, which no case here asserts. Answers the function that puts the reads back. */
function listThreads(count: number, asks: readonly InboxRow[] = []): () => void {
  const threads = Array.from({ length: count }, (_, index) => replyRow(`c${index + 1}`));
  const mocks = [
    spyOn(api, "getInbox").mockResolvedValue([...asks]),
    mockAskReads(asks),
    spyOn(api, "listMyReplies").mockResolvedValue({
      as_of: "2026-10-12T00:00:00Z",
      limit: 200,
      offset: 0,
      rows: threads,
      total: threads.length,
    }),
    spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails),
    spyOn(api, "listComments").mockResolvedValue([]),
  ];
  return () => {
    for (const mock of mocks) mock.mockRestore();
  };
}

// The bar's Dismiss deep in the list, over a thread the reader can see. The row after it takes the
// focus (R8: the item after the last of the rows acted on is selected and its row has the keyboard
// focus) while the thread is still drawn. The thread goes, and that row moves up into its place:
// the window stays where the reader put it, and is not scrolled to hold the focused row where it
// stood.
test("a bulk Dismiss of a marked thread above the focused row leaves the window's scroll and the list's scrollTop as they were", async () => {
  const user = userEvent.setup();
  const reads = listThreads(12);
  // Held open, so the list read here is the one the press leaves behind rather than the read the
  // landed press asks for.
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  // Five rows above the window, the eighth row 200 px down it.
  const laidOut = layRowsDown(5 * LAID_ROW_PX);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(12));
    const [marked, after] = rowIds().slice(7, 9) as [string, string];
    await user.click(markOf(marked));
    await screen.findByText("1 selected");
    const place = markOf(marked).getBoundingClientRect().top;
    const list = screen.getByTestId("inbox-list");

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() => expect(rowIds()).not.toContain(marked));
    const landing = shortRow(after);
    await waitFor(() => expect(landing).toHaveFocus());
    expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
    expect(landing.getBoundingClientRect().top).toBe(place);
    expect(window.scrollY).toBe(5 * LAID_ROW_PX);
    expect(list.scrollTop).toBe(0);
    expect(moves.scrolled.length).toBe(0);
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 1 });
    });
    unmount();
    moves.restore();
    laidOut();
    reads();
  }
});

// INFRA-3135, round 29 (the architect's condition (a) of 2026-11-26 05:5xZ): the bar's hand-off
// parks the focus on the mark of the row the reader marked last while that row's write is out, and
// the Inbox holds the row the reader's hand is in. A hold that read the hand alone would hold this
// row for the hand-off's own focus, the write would never be let go of, and the landing would never
// be made: the hold reads the same rows the hand-off is told of (`leavingNow`), so the row goes and
// the landing lands on the row at its place.
test("a bulk Dismiss lets the row it is taking away go though the hand-off parked the focus in it", async () => {
  const user = userEvent.setup();
  const reads = listThreads(12);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  const laidOut = layRowsDown(0);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(12));
    const order = rowIds();
    const marked = order.slice(2, 4);
    // The upper thread is marked last, as a reader marking upward does.
    for (const id of [...marked].reverse()) await user.click(markOf(id));
    await screen.findByText("2 selected");

    const parked = markOf(marked[0] as string);

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    // The bar goes while the rows are still listed, so the focus is parked on the mark of the row
    // marked last, which the press is taking away (`components/SelectionBar.tsx`).
    await waitFor(() => expect(moves.focused.map((move) => move.target)).toContain(parked));
    // The write is still out, and the rows go all the same.
    await waitFor(() => expect(rowIds()).toEqual(order.filter((id) => !marked.includes(id))));
    const landing = shortRow(order[4] as string);
    await waitFor(() => expect(landing).toHaveFocus());
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 2 });
    });
    unmount();
    moves.restore();
    laidOut();
    reads();
  }
});

// The same park and the same hold, under the bar's Snooze: the Inbox keeps a snoozed row in the
// band the reader saw it in until the write lands, so the park stands on that row for as long as
// the write is out. Once the write lands and the read drops the row, the hold lets it go and the
// landing is made.
test("a bulk Snooze lets the row it is taking away go though the hand-off parked the focus in it", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 12 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`)
  );
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landed = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landed.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const laidOut = layRowsDown(0);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Question 12?");
    const order = rowIds();
    const marked = order[2] as string;
    await user.click(markOf(marked));
    await screen.findByText("1 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    // The row is still in its band while the write is out, so the focus is parked on its own mark
    // (`components/SelectionBar.tsx`).
    const parked = markOf(marked);
    await waitFor(() => expect(moves.focused.map((move) => move.target)).toContain(parked));

    await act(async () => {
      landed.resolve();
      await landed.promise;
    });
    await waitFor(() => expect(rowIds()).not.toContain(marked));
    const landing = shortRow(order[3] as string);
    await waitFor(() => expect(landing).toHaveFocus());
  } finally {
    unmount();
    moves.restore();
    laidOut();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// INFRA-3135, round 29 (the architect's ruling of 2026-11-26 07:42Z): a refused bulk Snooze marks
// its asks again, so its bar stays with its refusal and no hand-off runs then. The name of each
// refused ask ends at once, so when the reader then clears the selection, nothing is leaving: the
// landing is made on the mark of the row marked last, which the list holds, and nothing is owed
// after it, so a wheel of theirs moves no focus. A refused name that outlived its refusal would
// owe that landing instead, and the wheel would take the focus off the parked mark.
test("after a refused bulk Snooze, Clear selection lands on the row marked last and owes nothing", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 12 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`)
  );
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => rows);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(new ApiError(409, { error: "conflict" }));
  const getAsk = mockAskReads(rows);
  const laidOut = layRowsDown(0);
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Question 12?");
    const order = rowIds();
    const marked = order[2] as string;
    await user.click(markOf(marked));
    await screen.findByText("1 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    await screen.findByText(/Could not snooze 1 of 1/u);

    await user.click(screen.getByRole("button", { name: "Clear selection" }));
    await waitFor(() => expect(markOf(marked)).toHaveFocus());
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    await afterFrameAndTasks();
    expect(markOf(marked)).toHaveFocus();
  } finally {
    unmount();
    laidOut();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The same refusal under the bar's Dismiss, whose optimistic apply takes the rows out before its
// request is sent: the landing is made on the row now at their place before Dispatch answers, and
// the refusal that puts the rows back moves no focus. It ends the name of each row it put back
// (`useInboxSelection`'s `dismissedHere`), so a row that came back is an ordinary row again: when
// the reader marks one of them and clears the selection, nothing is leaving, the landing is made
// at once on that row's mark, and nothing is owed after it, so a wheel of theirs moves no focus. A
// name that outlived its refusal would owe that landing instead, and the wheel would take the
// focus off the mark the hand-off parked it on.
test("after a refused bulk Dismiss, Clear selection lands on a row that came back and owes nothing", async () => {
  const user = userEvent.setup();
  const reads = listThreads(12);
  // Held, so the landing after the press is made while the write is out.
  const refusal = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(refusal.promise);
  const laidOut = layRowsDown(0);
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(12));
    const order = rowIds();
    const marked = order.slice(2, 4);
    for (const id of [...marked].reverse()) await user.click(markOf(id));
    await screen.findByText("2 selected");
    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() => expect(rowIds()).toEqual(order.filter((id) => !marked.includes(id))));
    const landing = shortRow(order[4] as string);
    await waitFor(() => expect(landing).toHaveFocus());

    await act(async () => {
      refusal.reject(new ApiError(409, { error: "conflict" }));
    });
    await screen.findByText(/could not dismiss/iu);
    await waitFor(() => expect(rowIds()).toEqual(order));
    expect(landing).toHaveFocus();

    // The lower of the two rows that came back, which the press had not marked last.
    const back = marked[1] as string;
    await user.click(markOf(back));
    await screen.findByText("1 selected");
    await user.click(screen.getByRole("button", { name: "Clear selection" }));
    // The landing waits for the foot's own pass alone (`hooks/useFootInset.ts`), not for a name.
    await afterFrameAndTasks();
    expect(screen.queryByText("1 selected")).toBeNull();
    expect(markOf(back)).toHaveFocus();
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    await afterFrameAndTasks();
    expect(markOf(back)).toHaveFocus();
  } finally {
    unmount();
    laidOut();
    reads();
  }
});

// INFRA-2160, round 22 (the architect's condition (2) of 2026-11-26 14:20Z): one bar press, one
// landing. The bar's Dismiss of the list's last two threads hands the reader on to the list's
// empty message (R8), which the list draws only once the threads have gone. The bar's landing
// holds the focus on the list's own stop while they stand, and gives it to the message once the
// list draws it (`useLastMarkedPlace`), and the list's own hand-off gives that target up to it
// (`paneSelection.takeHandOff`). Where both landed, the bar's landing put the focus on the list's
// stop in the commit the threads went in, and the list's own hand-off then moved it to the message.
test("the bar's Dismiss of the list's last threads lands the focus once, on the empty message, and never by the bar's landing and the list's own both", async () => {
  const user = userEvent.setup();
  const reads = listThreads(2);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  const real = HTMLElement.prototype.focus;
  /** Each focus the page asks for from the press on: what takes it, by its name, and whether a
   *  thread was still drawn as it was asked. */
  const moves: { standing: boolean; to: string }[] = [];
  let watching = false;
  const focus = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    if (watching) {
      moves.push({
        standing: document.querySelector("[data-inbox-row]") !== null,
        to: this.getAttribute("aria-label") ?? this.tagName,
      });
    }
    real.call(this, options);
  });
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(2));
    for (const id of rowIds()) await user.click(markOf(id));
    await screen.findByText("2 selected");
    const dismiss = screen.getByRole("button", { name: "Dismiss selected threads" });
    act(() => dismiss.focus());
    watching = true;
    await user.keyboard("{Enter}");
    const empty = await screen.findByRole("region", { name: "Inbox empty state" });
    await waitFor(() => expect(empty).toHaveFocus());
    // The foot's pass, each landing's own second look a microtask after it (`landNearest`), and
    // the commits after them have all run: nothing moves the focus after this.
    await afterFrameAndTasks();
    expect(empty).toHaveFocus();
    expect(
      moves.filter(({ standing }) => !standing).map(({ to }) => to),
      "the focus moves made once the threads had gone"
    ).toEqual(["Inbox empty state"]);
    expect(
      moves.filter(({ standing }) => standing).map(({ to }) => to),
      "the focus moves made while the threads stood"
    ).toEqual(["Inbox list"]);
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 2 });
    });
    unmount();
    focus.mockRestore();
    reads();
  }
});

// The bar's Dismiss over two threads the reader marked and then scrolled above the window. Their
// whole space goes, so every row after them moves up by it: the row now at the place of the row
// marked last, and the next one, end wholly above the window. The row after the threads is the
// item the reader goes to (R8: the item after the last of the rows acted on is selected and its
// row has the keyboard focus), though it ends above the window, 300 to 200 px above its top: it
// takes the focus with no scroll of the engine's own, and is then shown whole at its nearest
// edge, the window's top, by the least scroll, those 300 px (`landNearest`), and not by the
// engine's nearest-edge scroll, which counts the root's scroll padding. The list's own scroll
// keeping moves the window no further.
test("a bulk Dismiss of marked threads that all stood above the view hands the focus to the row after them, and brings that row into view", async () => {
  const user = userEvent.setup();
  const reads = listThreads(12);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  // Five rows above the window: the third and fourth rows, the ones marked, stand 300 and 200 px
  // above its top.
  const laidOut = layRowsDown(5 * LAID_ROW_PX);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(12));
    const order = rowIds();
    const marked = order.slice(2, 4);
    // The upper thread is marked last, as a reader marking upward does.
    for (const id of [...marked].reverse()) await user.click(markOf(id));
    await screen.findByText("2 selected");

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() => expect(rowIds()).toEqual(order.filter((id) => !marked.includes(id))));
    const landing = shortRow(order[4] as string);
    await waitFor(() => expect(landing).toHaveFocus());
    expect(moves.focused.at(-1)?.target).toBe(landing);
    expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
    expect(landing.getBoundingClientRect().top).toBe(0);
    expect(window.scrollY).toBe(2 * LAID_ROW_PX);
    expect(moves.scrolled.length).toBe(0);
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 2 });
    });
    unmount();
    moves.restore();
    laidOut();
    reads();
  }
});

// The bar's Dismiss over the list's last two unread threads, on a phone. **Mark all read** is
// offered only while the list holds an unread thread, so it goes in the commit that takes the
// threads away, and below `sm` the header's line it stood on goes with it: 52 px, the button's 44
// and the row's 8 px gap. The threads free their space, and that line is made up for as any other
// change above the list is: the window scrolls up by the line, and the row after the threads,
// which holds the focus, stands where the row marked last stood.
test("a bulk Dismiss of the last unread threads, whose going takes Mark all read's line out of the header, makes up for that line, and the row after them stands at their place", async () => {
  const user = userEvent.setup();
  // Six questions newer than the threads and six older, so the threads stand between them.
  const asks = Array.from({ length: 12 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`, {
      created_at: index < 6 ? `2026-10-13T0${index}:00:00Z` : `2026-10-0${index - 5}T00:00:00Z`,
    })
  );
  const reads = listThreads(2, asks);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  const MARK_ALL_LINE = 52;
  // Five rows above the window. The rows stand below the header's Mark all read line while it is
  // drawn.
  const laidOut = layRowsDown(5 * LAID_ROW_PX, () =>
    document.querySelector("[data-inbox-mark-all]") === null ? 0 : MARK_ALL_LINE
  );
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(14));
    const order = rowIds();
    const marked = order.slice(6, 8);
    expect(marked).toEqual(["reply:c1", "reply:c2"]);
    expect(document.querySelector("[data-inbox-mark-all]")).not.toBeNull();
    // The upper thread is marked last, as a reader marking upward does.
    for (const id of [...marked].reverse()) await user.click(markOf(id));
    await screen.findByText("2 selected");
    const place = markOf(marked[0] as string).getBoundingClientRect().top;

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() => expect(rowIds()).toEqual(order.filter((id) => !marked.includes(id))));
    expect(document.querySelector("[data-inbox-mark-all]")).toBeNull();
    const landing = shortRow(order[8] as string);
    await waitFor(() => expect(landing).toHaveFocus());
    expect(window.scrollY).toBe(5 * LAID_ROW_PX - MARK_ALL_LINE);
    expect(landing.getBoundingClientRect().top).toBe(place);
    expect(moves.scrolled.length).toBe(0);
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 2 });
    });
    unmount();
    moves.restore();
    laidOut();
    reads();
  }
});

// A Dismiss over a thread near the window's foot, with the row after it standing partly below
// the window, 700 to 800 px down a 768 px window: the bar's Dismiss after a mark, or the thread's
// own Dismiss below the column's width. That row takes the focus (R8: the item after the last of
// the rows acted on is selected and its row has the keyboard focus), and nothing scrolls: the
// thread's own Dismiss gives it the focus while the thread is still drawn, and the bar's landing
// keeps the focus on the thread's own mark while the thread stands and gives it to the row once
// the thread has gone (`landAtPlace`). Scrolled into sight where it stands while the thread is
// drawn, below the thread, the row would carry the view past the thread's place by the thread's
// height once the thread goes. Measured in the browser at 390x664 (table-controls.e2e.ts's bulk
// Dismiss deep in the list): the bar's own focus scrolled the window to the row while the
// threads stood, and their going left it at the window's top, or 1045 px above it for two. The
// thread goes, the row moves up into its place, wholly in the reader's sight, and nothing has
// scrolled.
for (const { name, pressOf } of [
  {
    name: "a bulk Dismiss gives the row after the marked thread the focus with no scroll once the thread has gone",
    pressOf: async (user: UserEvent, thread: string) => {
      await user.click(markOf(thread));
      await screen.findByText("1 selected");
      return () => user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    },
  },
  {
    name: "a thread's own Dismiss gives the row after it the focus with no scroll while the thread still stands above it",
    pressOf: async (user: UserEvent, thread: string) => {
      const dismiss = await waitFor(() => {
        const button = document.querySelector<HTMLElement>(`[data-inbox-dismiss="${thread}"]`);
        if (button === null) throw new Error(`${thread} draws no Dismiss`);
        return button;
      });
      return () => user.click(dismiss);
    },
  },
] satisfies readonly {
  name: string;
  /** Readies the press that dismisses `thread`, and answers it. */
  pressOf: (user: UserEvent, thread: string) => Promise<() => Promise<void>>;
}[]) {
  test(name, async () => {
    const user = userEvent.setup();
    const reads = listThreads(12);
    const settle = Promise.withResolvers<{ dismissed: number }>();
    answerInboxDismisses().mockReturnValue(settle.promise);
    // Four rows above the window: the eleventh row, the one dismissed, stands 600 to 700 px down
    // it, and the twelfth 700 to 800 px.
    const laidOut = layRowsDown(4 * LAID_ROW_PX);
    const moves = watchMoves();
    const { unmount } = renderInbox("/?group=none");
    try {
      await waitFor(() => expect(rowIds()).toHaveLength(12));
      const [dismissed, after] = rowIds().slice(10, 12) as [string, string];
      const press = await pressOf(user, dismissed);
      const place = shortRow(dismissed).getBoundingClientRect().top;

      await press();
      await waitFor(() => expect(rowIds()).not.toContain(dismissed));
      const landing = shortRow(after);
      await waitFor(() => expect(landing).toHaveFocus());
      // Every focus the page asked the landing to take asked it with no scroll.
      const asked = moves.focused.filter((move) => move.target === landing);
      expect(asked.length).toBeGreaterThan(0);
      expect(
        asked
          .filter((move) => move.options?.preventScroll !== true)
          .map((move) => JSON.stringify(move.options ?? "no options"))
      ).toEqual([]);
      expect(moves.scrolled.map((element) => element.outerHTML.slice(0, 120))).toEqual([]);
      expect(landing.getBoundingClientRect().top).toBe(place);
      expect(window.scrollY).toBe(4 * LAID_ROW_PX);
    } finally {
      await act(async () => {
        settle.resolve({ dismissed: 1 });
      });
      unmount();
      moves.restore();
      laidOut();
      reads();
    }
  });
}

// The bar's Dismiss over the list's last thread, which the reader marked and left below the
// window, 800 to 900 px down a 768 px window. No row stands after it, so the row before it, 700
// to 800 px down, takes the focus (R8: the item after the last of the rows acted on, else the
// nearest before it), and no row the press takes away stands above it, so it is shown whole at
// once: the window's bottom edge cuts it by 32 px, and the hand-off scrolls those 32 px, the least
// scroll to the window's own edge (`landNearest`), as the selection bar's own landing does. The
// engine's nearest-edge scroll (`scrollIntoView`) counts the root's `scroll-padding-bottom`, 80 px
// below 1280 px (`styles.css`), and would carry the row 80 px further up than the cut. happy-dom
// scrolls nothing, so the stub below scrolls the window as an engine does under that padding.
test("the row before a bulk Dismiss of the list's last thread, cut by the window's bottom edge by less than the root's scroll padding, is shown whole by the cut alone", async () => {
  const user = userEvent.setup();
  const reads = listThreads(12);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  // Three rows above the window: the eleventh row stands 700 to 800 px down it, and the twelfth,
  // the one dismissed, 800 to 900 px.
  const laidOut = layRowsDown(3 * LAID_ROW_PX);
  /** The root's `scroll-padding-bottom` below 1280 px (`styles.css`). */
  const paddingPx = 80;
  const padded = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    const edge = window.innerHeight - paddingPx;
    const { bottom } = this.getBoundingClientRect();
    if (bottom > edge) window.scrollBy({ top: bottom - edge });
  });
  const { unmount } = renderInbox("/?group=none");
  try {
    await waitFor(() => expect(rowIds()).toHaveLength(12));
    const [before, dismissed] = rowIds().slice(10, 12) as [string, string];
    await user.click(markOf(dismissed));
    await screen.findByText("1 selected");
    const landing = shortRow(before);
    const cut = landing.getBoundingClientRect().bottom - window.innerHeight;
    expect(cut).toBeGreaterThan(0);
    expect(cut).toBeLessThan(paddingPx);

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() => expect(rowIds()).not.toContain(dismissed));
    await waitFor(() => expect(landing).toHaveFocus());
    await waitFor(() => expect(window.scrollY).not.toBe(3 * LAID_ROW_PX));
    // The landing's own second look, a microtask after it (`landNearest`), and the commits after
    // it have all run: nothing moves the window after this.
    await afterFrameAndTasks();
    expect(window.scrollY).toBe(3 * LAID_ROW_PX + cut);
    expect(landing.getBoundingClientRect().bottom).toBe(window.innerHeight);
    expect(landing).toHaveFocus();
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 1 });
    });
    unmount();
    padded.mockRestore();
    laidOut();
    reads();
  }
});

// An answer from a card deep in the list, with the reader's focus moved on to a row below it
// while the answer is out. The Inbox keeps the answered row where the reader saw it until the
// answer lands, and lets it go then: the reader's own press takes it away, so the rows after it
// move up into its place, the focused one with them. The window is not scrolled to hold the
// focused row where it stood.
test("an answer that lands and takes its row away from above the focused row lets the focused row move up into its place, with no scroll", async () => {
  const user = userEvent.setup();
  const answeredAsk = inboxIssue("ask-c", "CORE-3", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const rows = [
    inboxIssue("ask-a", "CORE-1", "First question?"),
    inboxIssue("ask-b", "CORE-2", "Second question?"),
    answeredAsk,
    inboxIssue("ask-d", "CORE-4", "Fourth question?"),
    inboxIssue("ask-e", "CORE-5", "Fifth question?"),
    inboxIssue("ask-f", "CORE-6", "Sixth question?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const landing = Promise.withResolvers<void>();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    await landing.promise;
    served = rows.filter((row) => row.id !== "ask-c");
    return {
      ...answeredAsk,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  // One row above the window: the answered row stands 100 px down it, the focused row 300 px.
  const laidOut = layRowsDown(LAID_ROW_PX);
  const moves = watchMoves();
  const { unmount } = renderInbox("/?group=none", { pane: false });
  try {
    await waitFor(() => expect(rowIds()).toEqual(rows.map((row) => row.id)));
    const card = screen.getByTestId("ask-ask-c");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(card).findByRole("radio", { name: "Ship" }));
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    // Out, the row is where the reader saw it, though the answer took it out of the Inbox read.
    expect(rowIds()).toEqual(rows.map((row) => row.id));

    const focused = markOf("ask-e");
    act(() => focused.focus());
    const place = focused.getBoundingClientRect().top;
    const scrolledBefore = moves.scrolled.length;
    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).not.toContain("ask-c"));
    expect(focused).toHaveFocus();
    expect(focused.getBoundingClientRect().top).toBe(place - LAID_ROW_PX);
    expect(window.scrollY).toBe(LAID_ROW_PX);
    expect(moves.scrolled.length).toBe(scrolledBefore);
  } finally {
    unmount();
    moves.restore();
    laidOut();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The same for a reply, from the card of an ask's Reply row at the list's top, with the reader's
// focus moved on to a row below it while the reply is out. A read of the Inbox's threads drops the
// thread then, and the Inbox keeps its row where the reader saw it until the reply lands, and lets
// it go then: the reader's own press takes it away, so the rows after it move up into its place,
// the focused one with them. The window is not scrolled to hold the focused row where it stood.
test("a reply that lands and lets its row go from above the focused row lets the focused row move up into its place, with no scroll", async () => {
  const user = userEvent.setup();
  const asks = [
    inboxIssue("ask-a", "CORE-1", "First question?"),
    inboxIssue("ask-b", "CORE-2", "Second question?"),
    inboxIssue("ask-c", "CORE-4", "Third question?"),
  ];
  const settled = issueAsk({
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "Ship it.", user: "bob" },
    id: "ask-done",
    issue: { assignee: "bob", key: "CORE-3", title: "Bob's issue" },
    issue_key: "CORE-3",
    question: "Which approach?",
    state: "answered",
  });
  const thread = replyRow("ask-done", {
    issue: { key: "CORE-3", title: "Bob's issue" },
    kind: "ask",
    ref: "/issues/CORE-3/asks/ask-done",
    subject: "Which approach?",
  });
  const threads = (rows: MyReplyRow[]) => ({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows,
    total: rows.length,
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(asks);
  const getAsk = mockAskReads([...asks, settled]);
  const getIssue = mockIssueReads();
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue(threads([thread]));
  const landing = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValue(landing.promise);
  // One row above the window, the Reply row; the focused row stands 100 px down it.
  const laidOut = layRowsDown(LAID_ROW_PX);
  const moves = watchMoves();
  const { queryClient, unmount } = renderInbox("/?group=none", { pane: false });
  try {
    await waitFor(() => expect(rowIds()).toEqual(["reply:ask-done", "ask-a", "ask-b", "ask-c"]));
    const row = shortRow("reply:ask-done");
    await user.click(await within(row).findByRole("textbox", { name: "Reply" }));
    await user.paste("Thanks, shipping it.");
    await user.click(within(row).getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    // A read of the Inbox's threads that no longer lists this one: the row stays while the reply
    // is out.
    listMyReplies.mockResolvedValue(threads([]));
    await act(async () => {
      await queryClient.invalidateQueries();
    });
    expect(rowIds()).toEqual(["reply:ask-done", "ask-a", "ask-b", "ask-c"]);

    const focused = markOf("ask-b");
    act(() => focused.focus());
    const place = focused.getBoundingClientRect().top;
    const scrolledBefore = moves.scrolled.length;
    await act(async () => {
      landing.resolve(postedComment("CORE-3", "ask-done", "Thanks, shipping it."));
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).not.toContain("reply:ask-done"));
    expect(focused).toHaveFocus();
    expect(focused.getBoundingClientRect().top).toBe(place - LAID_ROW_PX);
    expect(window.scrollY).toBe(LAID_ROW_PX);
    expect(moves.scrolled.length).toBe(scrolledBefore);
  } finally {
    unmount();
    moves.restore();
    laidOut();
    createComment.mockRestore();
    listMyReplies.mockRestore();
    getIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The bar's Snooze deep in the list, with the pointer come to rest on a row below the marked rows
// while the write is out. The rows move into Later once it lands, and the rows after them move up
// into their place, the one under the pointer with them: the window is not scrolled to hold that
// row where it stood.
test("a bulk Snooze that lands with the pointer on a row below the marked rows lets the rows after them move up, with no scroll", async () => {
  const user = userEvent.setup();
  const rows = Array.from({ length: 12 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`)
  );
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landed = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landed.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const laidOut = layRowsDown(5 * LAID_ROW_PX);
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Question 12?");
    const [marked, last, , resting] = rowIds().slice(6, 10) as [string, string, string, string];
    await user.click(markOf(marked));
    await user.click(markOf(last));
    await screen.findByText("2 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    const row = document.querySelector<HTMLElement>(`[data-inbox-row="${resting}"]`);
    if (row === null) throw new Error(`${resting} row missing`);
    await user.hover(row);
    const before = row.getBoundingClientRect().top;

    await act(async () => {
      landed.resolve();
      await landed.promise;
    });
    await waitFor(() => expect(rowIds()).not.toContain(last));
    expect(row.getBoundingClientRect().top).toBe(before - 2 * LAID_ROW_PX);
    expect(window.scrollY).toBe(5 * LAID_ROW_PX);
  } finally {
    unmount();
    laidOut();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

/** How tall `layItemsDown` draws a band's heading. */
const LAID_HEADING_PX = 40;

/** Lays the items of the Inbox's list down a page the window scrolls, as happy-dom does not: each
 *  row `LAID_ROW_PX` tall and every other item - a band's heading - `LAID_HEADING_PX`, stacked in
 *  the order the list draws them from the page's top, less `window.scrollY`, and every element in
 *  an item given that item's box. The list's own box (the anchor's root) holds them all, and a
 *  point of the window names the item drawn there (`document.elementFromPoint`). `window.scrollY`
 *  starts at `scrollY`, and `window.scrollBy` moves it (`scrollWindowFrom`), so a row the list's
 *  anchor holds in place moves the window. Answers the function that gives happy-dom its own
 *  boxes, points and scroll back. */
function layItemsDown(scrollY: number): () => void {
  const real = Element.prototype.getBoundingClientRect;
  const scrolled = scrollWindowFrom(scrollY);
  /** Each item of the list with its top and bottom on the page, in the list's order. */
  const laid = () => {
    let bottom = 0;
    return [...document.querySelectorAll('[data-testid="inbox-list"] > * > ul > li')].map(
      (item) => {
        const top = bottom;
        bottom += item.matches("[data-inbox-row]") ? LAID_ROW_PX : LAID_HEADING_PX;
        return { bottom, item, top };
      }
    );
  };
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const items = laid();
    if (this.matches('[data-testid="inbox-list"] > *')) {
      return new DOMRect(0, -window.scrollY, 200, items.at(-1)?.bottom ?? 0);
    }
    const at = items.find(({ item }) => item.contains(this));
    if (at === undefined) return real.call(this);
    return new DOMRect(0, at.top - window.scrollY, 200, at.bottom - at.top);
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    const page = y + window.scrollY;
    return laid().find(({ bottom, top }) => top <= page && page < bottom)?.item ?? null;
  });
  return () => {
    rect.mockRestore();
    point.mockRestore();
    scrolled();
  };
}

// The reader, scrolled down the list, folds the band whose toggle stands at the window's top. Its
// rows stand between the toggle and the row nearest the list's centre, which the anchor holds when
// no row is in hand. The reader's own press takes them away, so they free their space, by the
// 11:4xZ rule of INFRA-3135: the rows after them move up into their place, and the window does not
// scroll to hold the centre row against their going, which moved the toggle down by the band.
test("a band the reader folds above the list's centre row frees its rows' space: the window does not scroll and the toggle stays where it stood", async () => {
  const user = userEvent.setup();
  const rows = [
    ...[1, 2].map((n) => inboxIssue(`ask-p0-${n}`, `CORE-${n}`, `Urgent ${n}?`, { priority: 0 })),
    ...[3, 4].map((n) => inboxIssue(`ask-p1-${n}`, `CORE-${n}`, `Soon ${n}?`, { priority: 1 })),
    ...[5, 6, 7, 8, 9, 10].map((n) =>
      inboxIssue(`ask-p2-${n}`, `CORE-${n}`, `Later ${n}?`, { priority: 2 })
    ),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  // P0's heading and two rows above the window, P1's heading at its top: P1's two rows below it,
  // then P2's heading, and P2's second row across the window's centre, 384 px down it.
  const laidOut = layItemsDown(LAID_HEADING_PX + 2 * LAID_ROW_PX);
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Later 10?");
    expect(headings()).toEqual(["P0 (2)", "P1 (2)", "P2 (6)"]);
    const toggle = screen.getByRole("button", { name: "P1 (2)" });
    const stood = toggle.getBoundingClientRect().top;
    expect(stood).toBe(0);

    await user.click(toggle);
    await waitFor(() => expect(toggle).toHaveAttribute("aria-expanded", "false"));
    expect(rowIds().filter((id) => id.startsWith("ask-p1-"))).toEqual([]);
    expect(window.scrollY).toBe(LAID_HEADING_PX + 2 * LAID_ROW_PX);
    expect(toggle.getBoundingClientRect().top).toBe(stood);
  } finally {
    unmount();
    laidOut();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// A row the bar's Snooze is saving is held as a row's own Snooze pick holds it: the optimistic
// moment would draw Un-snooze, so the list stays drawn, held, with the moment being saved before
// it and `Snoozing…` on its badge, and its status says what is being saved. The hold lasts through
// the commit in which the bar lets go of the row, which is the commit the row moves into Later in.
test("the bar's Snooze holds each marked row's own Snooze, saying it is snoozing, until the rows move into Later", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const marked = [
    ["ask-a", "CORE-1: Which approach?"],
    ["ask-b", "CORE-2: Which format?"],
  ] as const;
  const commits = recordCommits<null>(() =>
    marked.map(([askId, label]) => drawnSnooze(askId, label)).join(", ")
  );
  const { unmount } = renderInbox("/?group=waiting-on", { commits, pane: false });
  try {
    await screen.findByText("Which owner?");
    for (const [askId] of marked) {
      await user.click(within(rowOf(askId)).getByRole("checkbox", { name: /^Select / }));
    }
    await screen.findByText("2 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await user.click(picker);
    const tomorrow = within(panelOf(picker)).getByRole("option", { name: "Tomorrow" });
    const fromPick = commits.markAt(tomorrow, "click");
    await user.click(tomorrow);
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));

    // Out, each marked row draws its Snooze held, with the moment being saved before it.
    for (const [askId, label] of marked) {
      const row = rowOf(askId);
      expect(drawnSnooze(askId, label)).toBe("Snooze held");
      expect(within(row).queryByRole("button", { name: `Un-snooze ${label}` })).toBeNull();
      expect(row.querySelector("[data-inbox-snoozed-until]")).not.toBeNull();
      expect(within(row).getByText("Snoozing…")).toBeTruthy();
      expect(within(row).getByText(`Snoozing ${label}…`)).toHaveAttribute("role", "status");
    }

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    expect(headings()).toEqual(["Waiting on you (1)", "Later (2)"]);
    expect(drawnChanges(fromPick())).toEqual(["Snooze held, Snooze held", "(no row), (no row)"]);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// No press on a row the bar's Snooze is saving writes while that write is out, whatever the row's
// own Snooze draws: on a row the bar is taking out of its band, and on a row in Later the bar is
// snoozing again, whose moment would draw Un-snooze, a press sends neither a snooze of its own nor
// an un-snooze to race the bar's write.
test("a press on the Snooze of a row the bar's Snooze is saving writes nothing, in its band or in Later", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?", { snoozed_until: soon() }),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const unsnoozeAsk = answerAskUnsnoozes().mockResolvedValue(undefined);
  const getAsk = mockAskReads(rows);
  const marked = [
    ["ask-a", "CORE-1: Which approach?"],
    ["ask-b", "CORE-2: Which format?"],
  ] as const;
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which approach?");
    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    await screen.findByText("Which format?");
    for (const [askId] of marked) {
      await user.click(within(rowOf(askId)).getByRole("checkbox", { name: /^Select / }));
    }
    await screen.findByText("2 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));

    for (const [askId, label] of marked) await pressDrawnSnooze(user, askId, label);

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(screen.queryByText("Snoozing 2…")).not.toBeInTheDocument());
    // The bar's own snooze of each row is all that was sent.
    expect(snoozeAsk).toHaveBeenCalledTimes(2);
    expect(unsnoozeAsk).not.toHaveBeenCalled();
    await waitFor(() => expect(headings()).toEqual(["Later (2)"]));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
    unsnoozeAsk.mockRestore();
  }
});

// A bar's Snooze Dispatch refuses for one of its rows leaves that row where the reader saw it the
// whole way: held while the write is out, and once the refusal is in, marked again under the
// reason the bar gives, with its own Snooze given back - the list free and saying nothing -
// while the row Dispatch took moves into Later.
test("a refused bar Snooze leaves its refused row where it was, with its own Snooze enabled", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    if (id === "ask-b") {
      throw new ApiError(400, {
        code: "SNOOZE_INPUT",
        error: "snoozed_until must be in the future",
      });
    }
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const label = "CORE-2: Which format?";
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which owner?");
    for (const askId of ["ask-a", "ask-b"]) {
      await user.click(within(rowOf(askId)).getByRole("checkbox", { name: /^Select / }));
    }
    await screen.findByText("2 selected");
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    expect(drawnSnooze("ask-b", label)).toBe("Snooze held");

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await screen.findByText("Could not snooze 1 of 2: snoozed_until must be in the future.");
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));
    expect(headings()).toEqual(["Waiting on you (2)", "Later (1)"]);
    const row = rowOf("ask-b");
    expect(within(row).getByRole("checkbox", { name: /^Select / })).toBeChecked();
    await waitFor(() => expect(drawnSnooze("ask-b", label)).toBe("Snooze free"));
    expect(row.querySelector("[data-inbox-snoozed-until]")).toBeNull();
    expect(within(row).getByText("Snooze")).toBeTruthy();
    expect(within(row).queryByText(`Snoozing ${label}…`)).toBeNull();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// Escape from the bulk picker is one level out, and a row the bar's own write is still taking
// away is no place to land: it folds into Later once the write lands, no press of the reader's
// live by then, and the focus would fall to the body with it. With only marked rows on the page
// there is no other row, so Escape leaves the picker for the bar, and Escape from the bar clears
// the selection while the write is still out. The pick itself gives the focus back to the mark
// `h` was pressed on (dispatch://INFRA-2160, D5), so the reader goes back to the picker first.
test("Escape from the bulk picker while its snooze is out passes over the rows it is taking away to the bar, and a second Escape clears the selection", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  const mark = (askId: string) => within(rowOf(askId)).getByRole("checkbox", { name: /^Select / });
  try {
    await screen.findByText("Which format?");
    await user.click(mark("ask-a"));
    await user.click(mark("ask-b"));
    await screen.findByText("2 selected");
    await user.keyboard("h");
    const picker = screen.getByLabelText("Snooze selected asks");
    expect(picker).toHaveAttribute("aria-expanded", "true");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    // Out, both marked rows are still drawn where the reader saw them, and both are leaving.
    expect(screen.getByText("Snoozing 2…")).toBeTruthy();
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(mark("ask-b"), "the pick gives the focus back where h was pressed").toHaveFocus();

    act(() => picker.focus());
    await user.keyboard("{Escape}");
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(mark("ask-a")).not.toBeChecked();
    expect(mark("ask-b")).not.toBeChecked();
    expect(screen.getByText("Snoozing 2…")).toBeTruthy();

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual([]));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// Escape from the bulk picker while its write is out goes to the row `h` was pressed on, when the
// write is not taking that row away. When the write is taking that row away, Escape goes to the
// first row the write is not taking away. The first pick marks two of four rows, so two rows stay:
// `h` is pressed on the second of them, and the first is where Escape would go otherwise. The
// second pick marks the first row left, `h` is pressed on that row, and Escape passes over it to
// the row after it. Each part is its own pick: while a write is out with Later folded, the marked
// rows are out of the selection's count, so `h` opens the row's own Snooze and not the bar's.
// Each pick gives the focus back where `h` was pressed (dispatch://INFRA-2160, D5), and the
// reader then goes back to the bar's picker, whose list the held write keeps shut, for Escape.
test("Escape from the bar's Snooze list while its write is out goes to the row h was pressed on, else to the first row the write is not taking away", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
    inboxIssue("ask-d", "CORE-4", "Which date?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  // Each pick's writes wait for the landing that is current when they are sent.
  let landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  const mark = (askId: string) => within(rowOf(askId)).getByRole("checkbox", { name: /^Select / });
  /** Opens the bar's Snooze list with `h` from where the focus stands, picks a moment, and checks
   *  the pick gave the focus back there. */
  const snoozeFromHere = async () => {
    const from = document.activeElement;
    await user.keyboard("h");
    const picker = screen.getByLabelText("Snooze selected asks");
    expect(picker).toHaveAttribute("aria-expanded", "true");
    await pickIn(user, () => picker, "Tomorrow");
    expect(document.activeElement, "the pick gives the focus back where h was pressed").toBe(from);
  };
  /** Escape from the bar's picker, held while the write is out. */
  const escapeFromPicker = async () => {
    act(() => screen.getByLabelText("Snooze selected asks").focus());
    await user.keyboard("{Escape}");
  };
  const land = async () => {
    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
  };
  try {
    await screen.findByText("Which date?");
    await user.click(mark("ask-a"));
    await user.click(mark("ask-b"));
    await screen.findByText("2 selected");
    // From the mark of row b, two steps down stand on row d, which is not marked.
    await user.keyboard("jj");
    expect(rowOf("ask-d")).toHaveFocus();
    await snoozeFromHere();
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    expect(screen.getByText("Snoozing 2…")).toBeTruthy();
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c", "ask-d"]);
    await escapeFromPicker();
    expect(rowOf("ask-d")).toHaveFocus();
    await land();
    await waitFor(() => expect(rowIds()).toEqual(["ask-c", "ask-d"]));

    landing = Promise.withResolvers<void>();
    // `h` is pressed on the mark of row c, a row the second write takes away.
    await user.click(mark("ask-c"));
    await screen.findByText("1 selected");
    await snoozeFromHere();
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(3));
    expect(screen.getByText("Snoozing 1…")).toBeTruthy();
    expect(rowIds()).toEqual(["ask-c", "ask-d"]);
    await escapeFromPicker();
    expect(rowOf("ask-d")).toHaveFocus();
    await land();
    await waitFor(() => expect(rowIds()).toEqual(["ask-d"]));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// Escape from a control inside a row goes back to that row, even while the row's own snooze is
// saving. Here it is pressed on the row's mark, which the snooze does not hold. The write's
// landing then folds the row into Later, and the snooze hands the reader on to the item after the
// row (R8): the reader is still on the row they snoozed, so that item's row takes the focus rather
// than leaving it on the body. `k` from there moves to the first row.
test("Escape onto a row whose own snooze is saving hands the focus to the row after it once the write folds the row away, and k from there moves to the first row", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/?group=waiting-on"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <Inbox pane={false} />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which owner?");
    const trigger = within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?");
    act(() => trigger.focus());
    await pickIn(user, () => trigger, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    expect(drawnSnooze("ask-b", "CORE-2: Which format?")).toBe("Snooze held");
    expect(trigger).toHaveFocus();
    const mark = within(rowOf("ask-b")).getByRole("checkbox", { name: /^Select / });
    act(() => mark.focus());

    await user.keyboard("{Escape}");
    expect(rowOf("ask-b")).toHaveFocus();
    // The write is out past the Escape's own frame and task, as a server slower than a frame
    // keeps it.
    await afterFrameAndTasks();

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-c"]));
    await afterFrameAndTasks();
    expect(rowOf("ask-c")).toHaveFocus();

    await user.keyboard("k");
    expect(rowOf("ask-a")).toHaveFocus();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A snooze Dispatch never answers is bounded by the write deadline: each attempt goes silent until
// its own bound, the write is late at `SEND_DEADLINE_MS`, and the reader stops it there. The row
// is where they saw it the whole time - held while the write is out, its Snooze list given back
// once the stopped write is decided, with that write's note.
test("a snooze whose write never answers keeps its row where it was, and the stop at the deadline gives its Snooze list back with the stopped write's note", async () => {
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => {
    const silent = Promise.withResolvers<{ snoozed_until: string }>();
    setTimeout(
      () =>
        silent.reject(
          new RequestTimeoutError("/api/v1/me/asks/ask-b/snooze", KEYED_SEND_ATTEMPT_MS)
        ),
      KEYED_SEND_ATTEMPT_MS
    );
    return silent.promise;
  });
  const getAsk = mockAskReads(rows);
  const { queryClient, unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    jest.useFakeTimers();
    await pickIn(
      userOnFakeClock(),
      () => within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?"),
      "Tomorrow"
    );
    await tick(1_000);
    // Out and unanswered, the row is where the reader saw it, held.
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(drawnSnooze("ask-b", "CORE-2: Which format?")).toBe("Snooze held");

    await stopTheLateWrite(queryClient);

    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(drawnSnooze("ask-b", "CORE-2: Which format?")).toBe("Snooze free");
    expect(within(rowOf("ask-b")).getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the snooze", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A snooze Dispatch refuses leaves its row where the reader saw it the whole way: in its band,
// held, while the write is out, and there, enabled, with the reason once the refusal is back.
test("a refused snooze keeps its row where it was while it is out, and leaves it there enabled, with the reason", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const refusal = Promise.withResolvers<never>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => refusal.promise);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    await pickIn(
      user,
      () => within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?"),
      "Tomorrow"
    );
    await waitFor(() =>
      expect(rowOf("ask-b").querySelector("[data-inbox-snoozed-until]")).not.toBeNull()
    );
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(drawnSnooze("ask-b", "CORE-2: Which format?")).toBe("Snooze held");

    await act(async () => {
      refusal.reject(
        new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
      );
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(rowOf("ask-b")).getByRole("alert")).toHaveTextContent(
        "snoozed_until must be in the future"
      )
    );
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    await waitFor(() => expect(drawnSnooze("ask-b", "CORE-2: Which format?")).toBe("Snooze free"));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A refused snooze holds nothing: the next read of the Inbox places its row where Dispatch says.
// Here another device has snoozed the row meanwhile, so the read puts it in Later, which starts
// folded. The reader's hand leaves the row first, since a row under the hand is held for that
// reason alone.
test("a refused snooze holds nothing, so a read in which another device snoozed the row puts it in Later", async () => {
  const user = userEvent.setup();
  const askA = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const askB = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served: InboxRow[] = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
  );
  const getAsk = mockAskReads([askA, askB]);
  const { queryClient, unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    await pickIn(
      user,
      () => within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?"),
      "Tomorrow"
    );
    await waitFor(() =>
      expect(within(rowOf("ask-b")).getByRole("alert")).toHaveTextContent(
        "snoozed_until must be in the future"
      )
    );
    await user.click(screen.getByRole("searchbox", { name: "Search asks" }));

    served = [askA, { ...askB, snoozed_until: soon() }];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });

    await waitFor(() => expect(headings()).toEqual(["Waiting on you (1)", "Later (1)"]));
    expect(rowIds()).toEqual(["ask-a"]);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The same with a read that hands the refused row's turn to an agent: the row is drawn in the
// read's own band, and the refusal is still on it.
test("a refused snooze holds nothing, so a read that turns the row to an agent draws it under Waiting on agents", async () => {
  const user = userEvent.setup();
  const askA = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const askB = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served: InboxRow[] = [askA, askB];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
  );
  const getAsk = mockAskReads([askA, askB]);
  const { queryClient, unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which format?");
    await pickIn(
      user,
      () => within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?"),
      "Tomorrow"
    );
    await waitFor(() =>
      expect(within(rowOf("ask-b")).getByRole("alert")).toHaveTextContent(
        "snoozed_until must be in the future"
      )
    );
    await user.click(screen.getByRole("searchbox", { name: "Search asks" }));

    served = [askA, { ...askB, waiting_on: "agent" }];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });

    await waitFor(() => expect(rowOf("ask-b")).toHaveAttribute("data-inbox-group", "agent"));
    expect(headings()).toEqual(["Waiting on you (1)", "Waiting on agents (1)"]);
    expect(within(rowOf("ask-b")).getByRole("alert")).toHaveTextContent(
      "snoozed_until must be in the future"
    );
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The badge's `Snoozing…` is drawn for the eye alone, so the control says what its write is doing
// to a screen reader in a polite status of its own: a snooze or an un-snooze while that write is
// out, and nothing once Dispatch has answered it.
test("a snooze or an un-snooze out says so in the control's status, which says nothing once the write lands", async () => {
  const user = userEvent.setup();
  const snoozed = Promise.withResolvers<{ snoozed_until: string }>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => snoozed.promise);
  const unsnoozed = Promise.withResolvers<void>();
  const unsnoozeAsk = answerAskUnsnoozes().mockImplementation(() => unsnoozed.promise);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const drawn = (snoozedUntil: string | null) => (
    <QueryClientProvider client={queryClient}>
      <SnoozeWrites>
        <SnoozeControl askId="ask-a" label="CORE-1" snoozedUntil={snoozedUntil} />
      </SnoozeWrites>
    </QueryClientProvider>
  );
  const view = render(drawn(null));
  try {
    expect(screen.getByRole("status").textContent).toBe("");
    const snooze = screen.getByLabelText("Snooze CORE-1");
    await pickIn(user, () => snooze, "Tomorrow");
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Snoozing CORE-1…"));
    await act(async () => {
      snoozed.resolve({ snoozed_until: soon() });
      await snoozed.promise;
    });
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe(""));

    view.rerender(drawn(soon()));
    await user.click(screen.getByRole("button", { name: "Un-snooze CORE-1" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Un-snoozing CORE-1…"));
    await act(async () => {
      unsnoozed.resolve();
      await unsnoozed.promise;
    });
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe(""));
  } finally {
    view.unmount();
    snoozeAsk.mockRestore();
    unsnoozeAsk.mockRestore();
  }
});

// R3: every list of options a reader picks from holds a text box that narrows the options. A
// row's Snooze is the shared list's single pick of the presets: pressed, it opens with one text
// box, a typed word leaves the presets it matches, and Enter on the one left snoozes the row.
test("a row's Snooze list opens with a text box, a typed word narrows the presets, and a pick snoozes the row", async () => {
  const user = userEvent.setup();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (_id, { snoozed_until }) => ({
    snoozed_until,
  }));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <SnoozeWrites>
        <SnoozeControl askId="ask-a" label="CORE-1" snoozedUntil={null} />
      </SnoozeWrites>
    </QueryClientProvider>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Snooze CORE-1" });
    expect(textBoxes().length).toBe(0);
    await user.click(trigger);
    const panel = panelOf(trigger);
    const box = within(panel).getByRole("combobox", { name: "Search snooze times" });
    expect(textBoxes().length).toBe(1);
    expect(box).toHaveFocus();
    expect(listRows(panel)).toEqual(["Later today", "Tomorrow", "Next week", "Until I clear it"]);
    await user.keyboard("week");
    expect(listRows(panel)).toEqual(["Next week"]);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    const [askId, body] = snoozeAsk.mock.calls[0] ?? [];
    expect(askId).toBe("ask-a");
    expect(Date.parse(body?.snoozed_until ?? "")).toBeGreaterThan(Date.now());
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

// The same of the selection bar's Snooze, whose pick snoozes every marked ask.
test("the bar's Snooze list opens with a text box, a typed word narrows the presets, and a pick snoozes every marked ask", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which owner?");
    for (const askId of ["ask-a", "ask-b"]) {
      await user.click(within(rowOf(askId)).getByRole("checkbox", { name: /^Select / }));
    }
    await screen.findByText("2 selected");
    const trigger = screen.getByRole("button", { name: "Snooze selected asks" });
    const boxes = textBoxes().length;
    await user.click(trigger);
    const panel = panelOf(trigger);
    const box = within(panel).getByRole("combobox", { name: "Search snooze times" });
    expect(textBoxes().length).toBe(boxes + 1);
    expect(box).toHaveFocus();
    expect(listRows(panel)).toEqual(["Later today", "Tomorrow", "Next week", "Until I clear it"]);
    await user.keyboard("tom");
    expect(listRows(panel)).toEqual(["Tomorrow"]);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    expect(snoozeAsk.mock.calls.map(([id]) => id).sort()).toEqual(["ask-a", "ask-b"]);
    for (const [, body] of snoozeAsk.mock.calls) {
      expect(Date.parse(body.snoozed_until)).toBeGreaterThan(Date.now());
    }
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// One bar pick at a time: while its write is out the bar's Snooze is held. It is held by
// `aria-disabled`, never `disabled`, so it keeps the reader's focus, and no press of it opens its
// list - Enter, Space, an arrow, a typed character or a click - so no second pick is made and
// nothing more is sent. The pick itself gives the focus back to the mark `h` was pressed on
// (dispatch://INFRA-2160, D5), and the reader then goes to the held trigger.
test("the bar's Snooze, held while its write is out, keeps the focus and sends nothing", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/?group=waiting-on", { pane: false });
  try {
    await screen.findByText("Which owner?");
    for (const askId of ["ask-a", "ask-b"]) {
      await user.click(within(rowOf(askId)).getByRole("checkbox", { name: /^Select / }));
    }
    await screen.findByText("2 selected");
    await user.keyboard("h");
    const trigger = screen.getByRole("button", { name: "Snooze selected asks" });
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    const mark = within(rowOf("ask-b")).getByRole("checkbox", { name: /^Select / });
    await pickIn(user, () => trigger, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    expect(mark, "the pick gives the focus back where h was pressed").toHaveFocus();
    act(() => trigger.focus());
    expect(trigger).toHaveFocus();
    expect(heldNotDisabled(trigger as HTMLButtonElement, "the bar's Snooze")).toBe(true);

    for (const keys of ["{Enter}", " ", "{ArrowDown}", "n"]) {
      await user.keyboard(keys);
      expect(trigger).toHaveAttribute("aria-expanded", "false");
      expect(trigger).toHaveFocus();
    }
    await user.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
    expect(document.querySelector('[role="listbox"]')).toBeNull();
    expect(snoozeAsk).toHaveBeenCalledTimes(2);

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    expect(snoozeAsk).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// ----------------------------------------------------------------------------------------
// The reading column (INFRA-575). Every test above this line runs at happy-dom's own 1024 px,
// and those that drive a card in its row draw the list of cards, the one screen that draws cards,
// since the page reads one row at a time at every width. Two ways:
//
// - `renderInbox`'s `pane: false` mounts the list as the Peek drawer does (its own filter,
//   `onFilterChange` and the dialog's key scope): every row that writes a filter or presses a key
//   goes this way.
// - Ten rows render `<Inbox pane={false} />` in a router of their own. They only read a starting
//   filter from the address: they write no filter and press no key, so the drawer's props would
//   change nothing they check.
//
// The tests below widen the window to the column; the reading view below its width is
// `Inbox.reading-view.test.tsx`'s.
// ----------------------------------------------------------------------------------------

test("from 1280 px the Inbox is two columns: short rows in the list, the question in the pane", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "# Which approach?\n\nThe detail.");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const rows = [first, second];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const list = await screen.findByTestId("inbox-list");
    const pane = await screen.findByTestId("inbox-pane");
    // The pane is the column to the right of the list: one row, the list first.
    expect(list.parentElement).toBe(pane.parentElement?.parentElement ?? null);
    expect(list.compareDocumentPosition(pane) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // The card is the pane's, never the row's; the row is the question drawn by the one
    // renderer on one line - the heading's words and the detail after them, clamped by CSS,
    // with no syntax left and no stripper of its own - beside its kind and its age.
    expect(within(list).queryByTestId("ask-ask-a")).not.toBeInTheDocument();
    expect(within(pane).getByTestId("ask-ask-a")).toBeTruthy();
    const row = shortRow("ask-a");
    expect(within(row).getByText("Which approach? The detail.")).toBeTruthy();
    expect(within(row).getByRole("img", { name: "Question" })).toBeTruthy();
    expect(within(row).getByText("CORE-1")).toBeTruthy();
    expect(row.querySelector("time")?.getAttribute("datetime")).toBe(first.created_at);
    // The first row is selected when the Inbox opens, and the address says so.
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(address()).toBe("/?item=ask-a");
    expect(shortRow("ask-b").getAttribute("data-inbox-selected")).toBeNull();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a short row names its kind: a decision block, an approval request, and any other question", async () => {
  const narrow = widenToReadingPane();
  const question = inboxIssue("ask-question", "CORE-1", "Which approach?");
  const decision = inboxIssue("ask-decision", "CORE-2", "Ship behind a flag?", {
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: "ship_it",
  });
  const approval = inboxIssue("ask-approval", "CORE-3", "Approve spec.md (version 4)?", {
    approval: {
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 4,
      version: 4,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    kind: "approval",
  });
  const rows = [question, decision, approval];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    expect(within(shortRow("ask-question")).getByRole("img", { name: "Question" })).toBeTruthy();
    expect(within(shortRow("ask-decision")).getByRole("img", { name: "Decision" })).toBeTruthy();
    expect(within(shortRow("ask-approval")).getByRole("img", { name: "Approval" })).toBeTruthy();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("?item= selects the row it names, and one the list does not hold falls back to the first", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const rows = [first, second];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const named = renderInbox("/?item=ask-b");

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
  } finally {
    named.unmount();
  }

  const gone = renderInbox("/?item=ask-answered-elsewhere");
  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    // The address is rewritten to what the Inbox actually shows, so a reload lands there.
    expect(address()).toBe("/?item=ask-a");
  } finally {
    gone.unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("?item= naming a thread row whose read lands after the asks' selects that row, not the first", async () => {
  const narrow = widenToReadingPane();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const threads = holdThreadsRead();
  const { unmount } = renderInbox("/?item=reply:c1");

  try {
    // The asks' read has landed; the threads' read, where the named row is, is still out, so
    // the column takes no row of its own and the address keeps the reader's.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    expect(selectedIds()).toEqual([]);
    expect(address()).toBe("/?item=reply:c1");

    await threads.land([replyRow("c1")]);
    await waitFor(() => expect(selectedIds()).toEqual(["reply:c1"]));
    expect(address()).toBe("/?item=reply:c1");
  } finally {
    unmount();
    narrow();
    threads.restore();
    listComments.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("?item= naming a thread row whose read lands while a pointer moves over the list keeps that row until the pointer settles, and then selects it", async () => {
  const narrow = widenToReadingPane();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const threads = holdThreadsRead();
  const { unmount } = renderInbox("/?item=reply:c1");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    // A pointer travels over the list as the threads' read lands: the list keeps its rows as they
    // are until the pointer settles (`useMotionHold`), so the named row is not drawn yet, and the
    // column does not take the first row in its place.
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move
    // this hold arms on.
    fireEvent.pointerMove(shortRow("ask-a"));
    await threads.land([replyRow("c1")]);
    expect(rowIds()).toEqual(["ask-a"]);
    expect(selectedIds()).toEqual([]);
    expect(address()).toBe("/?item=reply:c1");

    // Settled, the list draws the thread row, and the address the reader opened selects it.
    await waitFor(() => expect(selectedIds()).toEqual(["reply:c1"]), { timeout: 2000 });
    expect(address()).toBe("/?item=reply:c1");
  } finally {
    unmount();
    narrow();
    threads.restore();
    listComments.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a thread read that answers no rows at all (`rows: null`) still lets the column take its first row", async () => {
  const narrow = widenToReadingPane();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  // The server answers an Inbox with no thread in it with `rows: null` (the schema allows it):
  // the read has landed, and there is nothing for the pointer hold to hand on.
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    as_of: "2026-10-12T02:00:00Z",
    limit: 0,
    offset: 0,
    rows: null,
    total: 0,
  });
  const { unmount } = renderInbox("/");

  try {
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    expect(address()).toBe("/?item=ask-a");
  } finally {
    unmount();
    narrow();
    listMyReplies.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The reader's threads are read apart from the asks, and the list draws the asks first: a link
// or a reload naming a thread is answered once the threads have come, not with the first ask.
test("?item= naming a thread selects it when the threads' read lands after the asks'", async () => {
  const narrow = widenToReadingPane();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const threads = Promise.withResolvers<MyRepliesResponse>();
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(() => threads.promise);
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const { unmount } = renderInbox("/?item=reply%3Athread-1");

  try {
    await waitFor(() => expect(listMyReplies).toHaveBeenCalled());
    await waitFor(() => shortRow("ask-a"));
    expect(selectedIds()).toEqual([]);
    expect(address()).toBe("/?item=reply%3Athread-1");

    await act(async () => {
      threads.resolve({
        as_of: "2026-10-12T00:00:00Z",
        limit: 200,
        offset: 0,
        rows: [replyRow("thread-1")],
        total: 1,
      });
      await threads.promise;
    });
    await waitFor(() => expect(selectedIds()).toEqual(["reply:thread-1"]));
    expect(address()).toBe("/?item=reply%3Athread-1");
  } finally {
    unmount();
    narrow();
    listMyReplies.mockRestore();
    listComments.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("?item= naming a row in the folded Later band opens the band and selects it", async () => {
  const narrow = widenToReadingPane();
  const open = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const later = inboxIssue("ask-later", "CORE-2", "Which format?", { snoozed_until: soon() });
  const rows = [open, later];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?item=ask-later");

  try {
    await screen.findByTestId("inbox-pane");
    // A folded band renders no rows at all, so the row being there is the band being open.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-later"]));
    expect(selectedIds()).toEqual(["ask-later"]);
    expect(screen.getByRole("button", { name: /^Later \(1\)/ })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("j and k move the selection down and up the list, the address following each step", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    // The selection is state, not focus: nothing in the list took the keyboard.
    expect(document.body).toHaveFocus();

    await user.keyboard("k");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    expect(address()).toBe("/?item=ask-a");
    // The last row has nothing below it, and the first nothing above it.
    await user.keyboard("k");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a click on a row selects it", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The Inbox on a route of its own, as the app mounts it (`app.tsx`): a link out of the list
 *  unmounts it, so the address a followed link leaves behind is the link's. */
function renderRoutedInbox() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RouteProbe />
          <Routes>
            <Route element={<Inbox />} path="/" />
            <Route element={<p>The ask in its issue</p>} path="/issues/:key/asks/:id" />
          </Routes>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { unmount: view.unmount };
}

test("a click on a short row's owner link opens the ask inside its issue, and the row leaves that address alone", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderRoutedInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // The question's own words are the row's: clicking them selects it, so the link followed
    // below is on a row the column is not already showing.
    await user.click(within(shortRow("ask-b")).getByText("Which format?"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");

    // The owner link opens the ask inside its issue, and the row holding it does not replace
    // that address with the Inbox's own.
    const owner = within(shortRow("ask-a")).getByRole("link", { name: /CORE-1/ });
    expect(owner).toHaveAttribute("href", "/issues/CORE-1/asks/ask-a");
    await user.click(owner);
    expect(address()).toBe("/issues/CORE-1/asks/ask-a");
    expect(screen.queryByTestId("inbox-list")).not.toBeInTheDocument();
    expect(screen.getByText("The ask in its issue")).toBeTruthy();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a short row's checkbox marks the row without selecting it or writing the address", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));

    await user.click(within(shortRow("ask-b")).getByRole("checkbox"));
    await screen.findByText("1 selected");
    expect((within(shortRow("ask-b")).getByRole("checkbox") as HTMLInputElement).checked).toBe(
      true
    );
    // The mark is the checkbox's whole doing: the column is still on the row it was showing.
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(address()).toBe("/?item=ask-a");
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the reader's own answer takes its row off the list and selects and focuses the next one", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = [second];
    return {
      ...first,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(pane).findByRole("radio", { name: "Ship" }));
    await user.click(within(pane).getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    // R8: once the server records the answer, the next row has the keyboard focus too, so Enter
    // and the digits act on it at once.
    expect(shortRow("ask-b")).toHaveFocus();
    expect(answerAsk).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The reader picks an option and presses Answer, and nothing of theirs is typed into the box:
// the draft that would otherwise keep the question in the column is empty, so this reads the
// hold the answer itself takes. The list keeps the row where the reader saw it while the answer
// is out, though the write takes it out of the Inbox read, so the column keeps the card, which
// shows the refusal.
test("an answer the server refuses keeps its question in the column, and a retry that lands moves on", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<never>();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(() => refusal.promise);
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(pane).findByRole("radio", { name: "Ship" }));
    await user.click(within(pane).getByRole("button", { name: "Answer" }));

    // The answer takes the row out of the Inbox read at once; the list keeps it where the reader
    // saw it while the answer is out, and the column keeps the question, the selection on it.
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(address()).toBe("/?item=ask-a");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-a")).toBeTruthy();
    expect(within(screen.getByTestId("inbox-pane")).getByText("Answering…")).toBeTruthy();

    await act(async () => {
      refusal.reject(
        new ApiError(400, {
          code: "ASK_REVISION_REQUIRED",
          error: "expected_edited_at is required",
        })
      );
      await Promise.resolve();
    });

    // The reason and its Retry are in the card that wrote, under what the reader answered, and
    // the rolled-back row is listed again with the selection still on it.
    const refused = await waitFor(() =>
      within(screen.getByTestId("inbox-pane")).getByRole("alert")
    );
    expect(refused).toHaveTextContent("Could not save your answer.");
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(address()).toBe("/?item=ask-a");

    // The server records the same answer on the card's own Retry: that is when the row leaves
    // and the next visible row in list order is selected.
    answerAsk.mockImplementation(async () => {
      served = [second];
      return {
        ...first,
        answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
        state: "answered",
      };
    });
    await user.click(
      within(screen.getByTestId("inbox-pane")).getByRole("button", { name: "Retry" })
    );

    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(rowIds()).toEqual(["ask-b"]);
    expect(address()).toBe("/?item=ask-b");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a selected row the server drops leaves the list at once and the next row is selected", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const third = inboxIssue("ask-c", "CORE-3", "Which order?");
  let served = [first, second, third];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(served);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // The middle row, so the next row in list order and the first row are not the same answer.
    await user.keyboard("j");
    expect(selectedIds()).toEqual(["ask-b"]);

    // Someone else answers the selected question while the reader is reading it. They have
    // typed nothing into it, so nothing holds it: a withdrawn question with a live answer form
    // invites an answer the server refuses.
    served = [first, third];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toEqual(["ask-a", "ask-c"]);

    // The selection goes where that row was heading - the next row the reader can see, not the
    // first one - the address follows it, and the column shows that row's question.
    expect(selectedIds()).toEqual(["ask-c"]);
    expect(address()).toBe("/?item=ask-c");
    const pane = screen.getByTestId("inbox-pane");
    expect(within(pane).getByTestId("ask-ask-c")).toBeTruthy();
    expect(within(pane).queryByTestId("ask-ask-b")).not.toBeInTheDocument();

    // An empty list shows no row at all, and no column beside it. The address is left as it
    // was: a list holding nothing answers no `?item=`, since it cannot tell a list that is
    // empty from one still loading or failed.
    served = [];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual([]));
    expect(selectedIds()).toEqual([]);
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The ask's thread read once an agent has retracted it: what the card draws its closed state
 *  from, whatever the Inbox list still says. */
function retracted(row: InboxRow): AskRead {
  return {
    answers: [],
    ask: {
      ...row,
      resolution: {
        actor: { id: "session-1", kind: "session" },
        at: "2026-09-11T03:00:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
      state: "resolved",
    },
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
  };
}

test("a question the reader was answering keeps its text in the column, closed, until they move", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  const typed = "Only on a cold start, and only back to the last lease.";
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    const draft = within(pane).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste(typed);

    // The server withdraws the question the reader is answering: it leaves the list like any
    // other dropped row, and its own read says it is closed.
    served = [second];
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-a"], retracted(first));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toEqual(["ask-b"]);

    // The column keeps the question, shows its closed state in place of the answer form, and
    // keeps what the reader typed where they can read and copy it.
    const kept = within(screen.getByTestId("inbox-pane"));
    expect(kept.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    expect(kept.queryByRole("button", { name: "Answer" })).not.toBeInTheDocument();
    expect(document.querySelector("[data-ask-answer]")).toBeNull();
    expect(kept.getByTestId("ask-unsent-answer")).toHaveTextContent(typed);

    // Until the reader selects another row, which shows that row's question and nothing of
    // this one.
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    const after = within(screen.getByTestId("inbox-pane"));
    expect(after.getByTestId("ask-ask-b")).toBeTruthy();
    expect(after.queryByTestId("ask-ask-a")).not.toBeInTheDocument();
    expect(after.queryByTestId("ask-unsent-answer")).not.toBeInTheDocument();

    // The same holds when the row that goes is the last one: an empty list shows no row, and
    // the column is still the reader's text and the question it belongs to.
    const lastTyped = "Two attempts, then the mirror takes over.";
    const lastDraft = after.getByLabelText("Your answer");
    await user.clear(lastDraft);
    await user.click(lastDraft);
    await user.paste(lastTyped);
    served = [];
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-b"], retracted(second));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual([]));
    const last = within(screen.getByTestId("inbox-pane"));
    expect(last.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    expect(last.getByTestId("ask-unsent-answer")).toHaveTextContent(lastTyped);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same withdrawal in the shell's main content, as the app shell draws it, and no press of the
// reader's live when it lands. Whoever caused the loss, the card hands the focus to its form's
// nearest focus stop, in the column the column's own focus stop, with no scroll, and the Inbox's
// `j` answers from there.
test("in the reading column, a question withdrawn under the reader's focus hands it to the column's focus stop, and j then selects the next row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const scrolls = watchScrolls();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-a"]);
    const draft = within(pane).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Only on a cold start, and only back to the last lease.");
    await afterFrameAndTasks();
    expect(draft).toHaveFocus();
    // The opening selection keeps its row in sight (`pane-selection.ts`); what follows is the
    // loss, which scrolls nothing.
    scrolls.clear();

    // The server withdraws the question the reader is answering: it leaves the list, its own
    // read says it is closed, and the card swaps to its record with the focus in its form.
    served = [second];
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-a"], retracted(first));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    expect(rowIds()).toEqual(["ask-b"]);
    const kept = within(screen.getByTestId("inbox-pane"));
    expect(kept.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    expect(draft).not.toBeInTheDocument();
    await afterFrameAndTasks();

    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();
    expect(document.activeElement?.closest("[data-inbox-row]") ?? null).toBeNull();
    expect(scrolls.asked()).toEqual([]);

    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
  } finally {
    view.unmount();
    narrow();
    scrolls.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// A question the server takes off the list while the reader's focus is in its answer box, with
// nothing typed there, so the column keeps nothing of it, in the shell's main content, as the app
// shell draws it. The column goes in the commit that drops the row: React lets go of the column's
// wrapper first and then of the card's form, whose hand-off puts the focus on the column's stop as
// it goes (`AskCard`'s `handOffFocus`). The column drawn next for the next question takes the focus
// on its own stop, so it is not left on the page.
test("in the reading column, a question taken off the list under the focus in its answer box hands the focus to the next question's focus stop", async () => {
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-a"]);
    const answer = within(pane).getByLabelText("Your answer");
    act(() => answer.focus());
    await afterFrameAndTasks();
    expect(answer).toHaveFocus();

    // A refetch the reader did not cause takes the question off the list.
    served = [second];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(rowIds()).toEqual(["ask-b"]);
    expect(answer).not.toBeInTheDocument();
    await afterFrameAndTasks();

    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Question body" })).toHaveFocus();
  } finally {
    view.unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// A control the column's own code takes away under the reader's focus, with no render of the
// Inbox's - a body component's own state - leaves the focus on the page and the Inbox's note of
// the column's focus on the control that went; a control the case puts in the column and takes
// away again stands for it. The reader's `j` from the page moves the selection, and the column
// drawn for the next question takes no focus: no loss of that key took it from the column.
test("in the reading column, j from the page after a control went from the column with no render of the Inbox's leaves the focus on the page", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-a"]);
    const control = document.createElement("button");
    control.type = "button";
    control.textContent = "Show more";
    screen.getByTestId("inbox-pane-scroller").append(control);
    act(() => control.focus());
    await afterFrameAndTasks();
    expect(control).toHaveFocus();
    control.remove();
    await afterFrameAndTasks();
    expect(document.activeElement).toBe(document.body);

    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
    await afterFrameAndTasks();
    expect(document.activeElement).toBe(document.body);
  } finally {
    view.unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The reader's Snooze pick in the column's head, in the shell's main content, as the app shell
// draws it. The write lands, the deferred question leaves the list, and the column is drawn again
// for the next one, whose answer is a text box. The snooze hands the reader on to the next
// question (R8: the item after the snoozed one is selected and its row has the keyboard focus),
// with no scroll of anything but the list's rows, and not to the next question's controls: its
// answer box there would take the reader's next `j` as text. From the row `j` and `k` step the
// selection, and from the column's stop Enter opens the selected question's answer box.
test("a Snooze pick in the column's head hands the focus to the next question's row, and j, k and Enter answer from there", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  // Every element scrolled into view: the list keeps the selected row in sight as the selection
  // moves (`pane-selection.ts`), and nothing else is to scroll.
  const scrolled: Element[] = [];
  const scrolls = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    scrolled.push(this);
  });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-a"]);
    const snooze = within(pane).getByLabelText("Snooze CORE-1: Which approach?");
    act(() => snooze.focus());
    await afterFrameAndTasks();

    await pickIn(user, () => snooze, "Tomorrow");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(snoozeAsk).toHaveBeenCalledTimes(1);
    expect(rowIds()).toEqual(["ask-b", "ask-c"]);
    expect(within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer")).toBeTruthy();
    await afterFrameAndTasks();

    const scroller = () => screen.getByTestId("inbox-pane-scroller");
    expect(shortRow("ask-b")).toHaveFocus();
    // Nothing but the list's rows scrolled: the Snooze list kept its highlighted choice in sight
    // inside its own panel as it opened (`MultiSelect`), which is the list's and no scroll of the
    // page. Each is read by its words, so a failure here prints words and not a DOM element.
    expect(
      scrolled
        .filter(
          (element) =>
            element.closest("[data-inbox-row]") === null &&
            element.getAttribute("role") !== "option"
        )
        .map((element) => element.outerHTML.slice(0, 120))
    ).toEqual([]);

    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));

    await user.keyboard("l");
    expect(scroller()).toHaveFocus();
    await user.keyboard("k");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));

    await user.keyboard("l");
    expect(scroller()).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer")).toHaveFocus();
  } finally {
    view.unmount();
    narrow();
    scrolls.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// Beside the reading column the list keeps the selected row in the reader's sight as `j` walks it
// (`pane-selection.ts`), at its nearest edge by the least scroll, against the window's and each
// box's visible edges (`scrollToNearest`), the rule the hand-off and the selection bar land by.
// Here the eighth row stands 700 to 800 px down a 768 px window, so `j` onto it from the seventh
// scrolls the window by the 32 px the window's bottom edge cuts it by, and the focus stays where
// the reader had it.
test("j onto a row the window's bottom edge cuts, beside the reading column, shows that row whole by the cut alone", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = Array.from({ length: 12 }, (_, index) =>
    inboxIssue(`ask-${index + 1}`, `CORE-${index + 1}`, `Question ${index + 1}?`)
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const laidOut = layRowsDown(0);
  const { unmount } = renderInbox("/?group=none");
  try {
    await screen.findByTestId("inbox-pane");
    const order = rowIds();
    const [seventh, eighth] = order.slice(6, 8) as [string, string];
    const cut = shortRow(eighth).getBoundingClientRect().bottom - window.innerHeight;
    expect(cut).toBeGreaterThan(0);
    await user.click(shortRow(seventh));
    await waitFor(() => expect(selectedIds()).toEqual([seventh]));
    expect(window.scrollY).toBe(0);
    const focused = document.activeElement;

    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual([eighth]));
    await afterFrameAndTasks();
    expect(window.scrollY).toBe(cut);
    expect(shortRow(eighth).getBoundingClientRect().bottom).toBe(window.innerHeight);
    expect(document.activeElement).toBe(focused);
  } finally {
    unmount();
    laidOut();
    narrow();
    getIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

/** Where a question the column holds past its row stood in the list, and where each key goes
 *  from there: the step starts from the place that row held, so `k` never skips the row above
 *  it and `j` never doubles back up the list. A step off either end of the list moves nothing,
 *  exactly as it moves nothing from the row at that end. */
const heldSteps = [
  ["j", "the last row", 2, []],
  ["k", "the last row", 2, ["ask-b"]],
  ["j", "a middle row", 1, ["ask-c"]],
  ["k", "a middle row", 1, ["ask-a"]],
  ["j", "the first row", 0, ["ask-b"]],
  ["k", "the first row", 0, []],
] as const;

test.each(
  heldSteps
)("%s from a question held past %s steps from where that row stood", async (key, _place, heldIndex, expected) => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  const held = rows[heldIndex];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    for (let above = 0; above < heldIndex; above += 1) {
      await user.keyboard("j");
      await waitFor(() => expect(selectedIds()).toEqual([rows[above + 1].id]));
    }
    await waitFor(() => expect(selectedIds()).toEqual([held.id]));
    // Typed and unsent: the column keeps this question once its row goes.
    const draft = within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Whichever the mirror already holds.");

    served = rows.filter((row) => row.id !== held.id);
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", held.id], retracted(held));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual(served.map((row) => row.id)));
    expect(selectedIds()).toEqual([]);

    await user.keyboard(key);
    // Settled first, so a row the step should have left alone has its chance to show up.
    await act(async () => {
      await Promise.resolve();
    });
    expect(selectedIds()).toEqual([...expected]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the keys act on the selected row and the pane: Enter, 1-9, x, h and Escape", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const rows = [first, inboxIssue("ask-b", "CORE-2", "Which format?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");

    // `1` picks the first option of the selected question, inside the pane.
    await user.keyboard("1");
    await waitFor(() =>
      expect((within(pane).getByRole("radio", { name: "Ship" }) as HTMLInputElement).checked).toBe(
        true
      )
    );

    // Enter moves the focus to the pane's answer box, and Escape brings it back to the row.
    await user.keyboard("{Enter}");
    const answerBox = pane.querySelector("[data-ask-answer]");
    expect(answerBox).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();

    // `h` with nothing checked opens the pane head's Snooze list, which the short row no longer
    // carries, on its first choice; Escape closes it, writes nothing, and puts the focus back on
    // the row `h` was pressed on.
    await user.keyboard("h");
    expect(screen.getByRole("listbox", { name: /^Snooze CORE-1/ })).toBeInTheDocument();
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search snooze times" }))).toBe(
      "Later today"
    );
    expect(pane.querySelector("[data-inbox-snooze]")).toHaveAttribute("aria-expanded", "true");
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(shortRow("ask-a")).toHaveFocus();

    // `x` checks the selected row, which is what the bulk bar then counts.
    await user.keyboard("x");
    await screen.findByText("1 selected");
    expect((within(shortRow("ask-a")).getByRole("checkbox") as HTMLInputElement).checked).toBe(
      true
    );
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("p opens the priority menu of an ask of an issue, keeps the focus on its row after a pick, and is not offered on a document's ask", async () => {
  const user = userEvent.setup();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", { priority: 2 });
  const rows = [first, artifactAsk("alice")];
  // The server's answer moves with the write, so the refetch it triggers reads the new priority.
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) => {
    const priority = input.priority ?? null;
    served = served.map((row) => (row.id === first.id ? { ...row, priority } : row));
    return { ...narrowIssueOf(first, "alice"), priority };
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByText("Which approach?");
    const issueRow = rowOf("ask-a");
    act(() => issueRow.focus());
    expect(offered("priority")).toBe(true);
    await user.keyboard("p");
    const trigger = within(issueRow).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    expect(optionOf(trigger, "P2")).toHaveAttribute("aria-selected", "true");
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search priorities" }))).toBe("P2");
    await user.keyboard("{ArrowUp}{Escape}");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(issueRow).toHaveFocus();
    expect(patchIssue).not.toHaveBeenCalled();

    await user.keyboard("p");
    await user.keyboard("0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(1);
    // The ask was the only one at P2, so the pick took the last row out of the band the reader
    // has it in. The list keeps the row there, under the reader's focus, rather than drawing it
    // in no band for a commit and handing the focus to the page.
    await waitFor(() =>
      expect(
        within(issueRow).getByRole("button", { name: pickNamed("Priority of CORE-1") })
      ).toHaveTextContent("P0")
    );
    expect(rowOf("ask-a")).toBe(issueRow);
    expect(issueRow).toHaveFocus();
    // The focus is still on the row, so the next `p` opens its list again, on P0.
    await user.keyboard("p");
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search priorities" }))).toBe("P0");
    await user.keyboard("{Escape}");
    expect(issueRow).toHaveFocus();

    // A question on a project document has no issue, so no priority to open.
    act(() => rowOf("ask-1").focus());
    expect(offered("priority")).toBe(false);
    await user.keyboard("p");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    patchIssue.mockRestore();
  }
});

// R4, R5 (the tester's round 16, defect 1): a menu's trigger is a menu's button, and a character
// typed on it is the page's key, as on its row (`typedOnMenuTrigger`). Below the column's width
// the keys act on the row in hand, which is the row that holds the focus or the row whose Snooze
// or priority does: `b` or `h` on a card's focused Snooze opens its Snooze menu on its first
// choice, `p` there the card's priority menu on its value, and `b` on its focused priority the
// Snooze menu, each with nothing typed in its box; Escape gives the focus back to the control.
test("in the drawer a key typed on a card's focused Snooze or priority opens the menu it names, on its first choice or its value with nothing typed", async () => {
  const user = userEvent.setup();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?", { priority: 2 })];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByText("Which approach?");
    const row = rowOf("ask-a");
    const snooze = within(row).getByLabelText("Snooze CORE-1: Which approach?");
    const priority = within(row).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    for (const [focused, key, opened, first] of [
      [snooze, "b", snooze, "Later today"],
      [snooze, "h", snooze, "Later today"],
      [snooze, "p", priority, "P2"],
      [priority, "p", priority, "P2"],
      [priority, "b", snooze, "Later today"],
    ] as const) {
      const step = `${key} on the focused ${focused === snooze ? "Snooze" : "priority"}`;
      act(() => focused.focus());
      await user.keyboard(key);
      const box = within(panelOf(opened)).getByRole("combobox");
      expect(box, `${step} opens the menu it names`).toHaveFocus();
      expect(box, `${step} types nothing`).toHaveValue("");
      expect(highlightedRow(box), `${step} opens on ${first}`).toBe(first);
      await user.keyboard("{Escape}");
      expect(screen.queryByRole("listbox"), step).not.toBeInTheDocument();
      expect(focused, `Escape after ${step} gives the focus back`).toHaveFocus();
    }
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("a priority picked from the column's head keeps the column on its ask, though the pick empties the band the ask was in", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", { priority: 2 });
  const rows = [first, inboxIssue("ask-b", "CORE-2", "Which format?")];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) => {
    const priority = input.priority ?? null;
    served = served.map((row) => (row.id === first.id ? { ...row, priority } : row));
    return { ...narrowIssueOf(first, "alice"), priority };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-a"]);
    const row = shortRow("ask-a");
    act(() => row.focus());
    await user.keyboard("p");
    expect(highlightedRow(screen.getByRole("combobox", { name: "Search priorities" }))).toBe("P2");
    await user.keyboard("0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await waitFor(() =>
      expect(
        within(screen.getByTestId("inbox-pane")).getByRole("button", {
          name: pickNamed("Priority of CORE-1"),
        })
      ).toHaveTextContent("P0")
    );
    // The selection and the focus stay on the ask whose priority changed.
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(shortRow("ask-a")).toBe(row);
    expect(row).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

test("l reads the column, and Escape returns the focus to the row l was pressed on", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // Both keys are the Inbox scope's, so `?` lists them in the Inbox section.
    expect(
      appKeymap
        .describe()
        .filter((entry) => entry.id === "read-pane" || entry.id === "search")
        .map((entry) => [entry.scope, entry.id, entry.keys])
    ).toEqual([
      ["inbox", "read-pane", ["l"]],
      ["inbox", "search", ["f"]],
    ]);

    // The focus is on the first row and `j` moves the selection off it without moving the
    // focus, so the row `l` is pressed on is not the row the column is showing.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(shortRow("ask-a")).toHaveFocus();

    const scroller = () => screen.getByTestId("inbox-pane-scroller");
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // Escape is the way back to the row the reader came from, not to the selected one.
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);

    // A step taken from inside the column leaves it first, so the focus lands on the selected
    // row rather than on a node the step may unmount - and the record `l` made goes with it,
    // leaving Escape the plain list key it is from a row.
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();
    await user.keyboard("k");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    expect(shortRow("ask-b")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(document.body).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the search box and a row each take the focus out of the column and drop the record", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");
    const box = screen.getByRole("searchbox", { name: "Search asks" });

    // The column is showing ask-b while the focus is on ask-a, so the row `l` records is not
    // the row the column is showing.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // The search box takes the focus, where `f` hands it: one of the ways out of the column
    // that no binding cleared. The reader then enters the column again the way a click does -
    // the scroller itself, with no `l` to record a row - so Escape owes them the row the column
    // is showing, not the row they pressed `l` on before they ever left.
    box.focus();
    expect(box).toHaveFocus();
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();

    // A row taking the focus is the other way out, pointer or Tab: the same rule drops the
    // record there, so a column re-entered by click returns to the row it is showing.
    shortRow("ask-a").focus();
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();
    shortRow("ask-a").focus();
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a window or tab blur keeps the record: the reader has not left the column", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column is showing ask-b while the focus is on ask-a, so the row `l` records is not
    // the row the column is showing and the two ways back are told apart.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // The window or the tab loses the focus: the blur names no target, and nothing inside the
    // document moved, so the scroller still holds the focus. The reader is still in the column,
    // and back on the tab Escape owes them the row they came from, not the selected one.
    // user-event has no blur: the report under test names no target, which its focus API cannot send.
    fireEvent.blur(scroller(), { relatedTarget: null });
    expect(scroller()).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a focus that leaves the column for the body alone drops the record", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column is showing ask-b while the focus is on ask-a, so the row `l` records is not
    // the row the column is showing and the two ways back are told apart.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // A `blur()` of the control holding the focus is the route this stands for, as is a click on
    // the page outside its focusable `main`: the scroller loses the focus to `document.body`,
    // nothing takes it, and so no `focusin` reports the move. The wrapper's `onBlur` - naming no
    // target, with the body holding the focus - is the only report that the reader has left. The
    // keep above is this same event with the focus still in the column, which is why both halves
    // of that null are read.
    act(() => scroller().blur());
    expect(document.body).toHaveFocus();

    // So a column re-entered the way a click takes the reader returns to the question it is
    // showing, not to the row they pressed `l` on before they clicked away.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// This row and the two below drive the one unmount inside the column the Inbox can reach from
// the keyboard: the head's `Assign to me`, which goes once its write lands, leaving the focus it
// held on `document.body` with no blur and no `focusin` behind it. The cleared filter is what
// keeps the column on that question through the assignment - under a `?assignee=` the viewer
// holds, the row would leave the list and the column with it, which is `holdPane`'s drop and not
// the focus rule under test.
//
// What they prove is the rule for a focused node React removes, which is what the keep rests on.
// They do not prove a browser's own ordering through this control: it disables itself while its
// write is out, and Chromium blurs a control it disables where happy-dom does not, so on a real
// page the wrapper's `onBlur` reaches the record first and drops it before the unmount, and
// Escape then goes to the row of the question the column shows (measured in the test phase).
test("an unmount inside the column keeps the record: the focus falls to the body and comes back in", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    // A row the server resolved nobody for, so the column's head draws "Assign to me": the
    // control whose unmount this row is about.
    issueAsk(
      {
        id: "ask-b",
        issue: { assignee: null, key: "CORE-2", title: "Nobody's issue" },
        issue_key: "CORE-2",
        question: "Which format?",
      },
      null
    ),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=everyone");

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column shows ask-b while the focus is on ask-a, so the row `l` records is not the row
    // the column is showing.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // The reader takes the question's issue from the column's own head, with the focus on the
    // control that writes.
    const assign = await within(screen.getByTestId("inbox-pane")).findByRole("button", {
      name: "Assign CORE-2 to me",
    });
    assign.focus();
    expect(assign).toHaveFocus();
    await user.click(assign);
    served = served.map((row) =>
      row.id === "ask-b"
        ? {
            ...row,
            issue: { assignee: "alice", key: "CORE-2", title: "Nobody's issue" },
            waiting_on_person: "alice",
          }
        : row
    );
    await act(async () => {
      save.resolve(narrowIssueOf(rows[1] as InboxRow, "alice"));
      await save.promise;
    });

    // The issue is theirs, so the control goes - an unmount inside the column, which leaves the
    // focus on the body: React fires no blur for a focused node it removes, and the body is
    // focused by nothing, so neither drop of the focus rule has anything to read.
    await waitFor(() =>
      expect(
        within(screen.getByTestId("inbox-pane")).queryByRole("button", {
          name: "Assign CORE-2 to me",
        })
      ).not.toBeInTheDocument()
    );
    expect(selectedIds()).toEqual(["ask-b"]);
    expect(document.body).toHaveFocus();

    // The reader never left the question in front of them, so the record is still theirs: the
    // focus landing back inside the column is no exit, and Escape is still their way back to the
    // row they pressed `l` on.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

test("a focus landing outside the column drops the record an unmount inside it left standing", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    issueAsk(
      {
        id: "ask-b",
        issue: { assignee: null, key: "CORE-2", title: "Nobody's issue" },
        issue_key: "CORE-2",
        question: "Which format?",
      },
      null
    ),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=everyone");

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The same column and the same record as the row above, left standing by the same unmount
    // inside it, with the focus on the body.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    const assign = await within(screen.getByTestId("inbox-pane")).findByRole("button", {
      name: "Assign CORE-2 to me",
    });
    assign.focus();
    await user.click(assign);
    served = served.map((row) =>
      row.id === "ask-b"
        ? {
            ...row,
            issue: { assignee: "alice", key: "CORE-2", title: "Nobody's issue" },
            waiting_on_person: "alice",
          }
        : row
    );
    await act(async () => {
      save.resolve(narrowIssueOf(rows[1] as InboxRow, "alice"));
      await save.promise;
    });
    await waitFor(() =>
      expect(
        within(screen.getByTestId("inbox-pane")).queryByRole("button", {
          name: "Assign CORE-2 to me",
        })
      ).not.toBeInTheDocument()
    );
    expect(document.body).toHaveFocus();

    // Now the reader leaves: `f` hands the focus to the search box. The column loses no focus it
    // was holding, so no blur of its wrapper fires - the focus landing outside it is the whole
    // report, and the document's rule is what reads it.
    await user.keyboard("f");
    expect(screen.getByRole("searchbox", { name: "Search asks" })).toHaveFocus();

    // Back in the column the way a click takes them: Escape owes them the question in front of
    // them, not the row they pressed `l` on before they ever went near the head.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

// The third route through that same unmount, and the one that tells the record's writer apart
// from a writer gated on where the focus is: from the body the unmount leaves the focus on, `l`
// is the keyboard's way back to the document (Enter is the other, into the answer box, and
// writes nothing), and the press finds a record already standing. It must leave that record
// alone, exactly as the click back in two rows above does, or the two ways back into a column
// the reader never left would owe them different rows.
test("l back into a column an unmount dropped the focus out of leaves the record standing", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    issueAsk(
      {
        id: "ask-b",
        issue: { assignee: null, key: "CORE-2", title: "Nobody's issue" },
        issue_key: "CORE-2",
        question: "Which format?",
      },
      null
    ),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=everyone");

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The same column and the same record as the two rows above: ask-a is the row `l` is
    // pressed on, ask-b the question the column shows.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    const assign = await within(screen.getByTestId("inbox-pane")).findByRole("button", {
      name: "Assign CORE-2 to me",
    });
    assign.focus();
    await user.click(assign);
    served = served.map((row) =>
      row.id === "ask-b"
        ? {
            ...row,
            issue: { assignee: "alice", key: "CORE-2", title: "Nobody's issue" },
            waiting_on_person: "alice",
          }
        : row
    );
    await act(async () => {
      save.resolve(narrowIssueOf(rows[1] as InboxRow, "alice"));
      await save.promise;
    });
    await waitFor(() =>
      expect(
        within(screen.getByTestId("inbox-pane")).queryByRole("button", {
          name: "Assign CORE-2 to me",
        })
      ).not.toBeInTheDocument()
    );
    expect(document.body).toHaveFocus();

    // The reader reaches for `l` to get back to the document they were reading. The record they
    // made on the way in stands, so this press writes nothing: it only hands the scroller the
    // focus the unmount took.
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // So Escape owes them the row they pressed `l` on first, not the row of the question the
    // column is showing - which is what a press writing the selected row over the record here
    // would give them, and what the click back in two rows above does not.
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

test("a second l from inside the column leaves the record on the row the reader came from", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // Pressed again from inside the column - a reader scrolling with the arrows and reaching
    // for the key again - `l` only focuses the scroller it is already on. The row it recorded
    // the first time stands: it is the row the reader came from, and writing the selected row
    // over it would send Escape to a row they never pressed the key on.
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a step out of a column held open over a hidden row drops the record, where no blur fires", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    // Typed and unsent, so the column keeps ask-a once the server withdraws its row: a column
    // with no selected row beside it, which is where a step hands the focus to nothing.
    const draft = within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Whichever the mirror already holds.");
    served = rows.filter((row) => row.id !== "ask-a");
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-a"], retracted(rows[0]));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));
    expect(selectedIds()).toEqual([]);

    const scroller = () => screen.getByTestId("inbox-pane-scroller");
    shortRow("ask-c").focus();
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // `j` steps the selection from the place the hidden row held, and has no selected row to
    // hand the focus back to: the scroller keeps it, so no blur fires and the step itself is
    // the only thing that can drop the record. The reader has stepped off the question they
    // pressed `l` on, so Escape owes them the row the column is showing now.
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(scroller()).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a step that moves nothing in a column held open over a hidden row keeps the record", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    // The same column as the test above: ask-a's unsent answer keeps its question here once the
    // server withdraws its row, and the question stood above every row the list has left.
    const draft = within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Whichever the mirror already holds.");
    served = rows.filter((row) => row.id !== "ask-a");
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-a"], retracted(rows[0]));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));
    expect(selectedIds()).toEqual([]);

    const scroller = () => screen.getByTestId("inbox-pane-scroller");
    shortRow("ask-c").focus();
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // `k` has no row to step to - the question the column keeps stood above both rows left - and
    // no selected row to hand the focus back to, so the selection and the focus both stay
    // exactly where they were. The reader has gone nowhere: they are still on the question they
    // pressed `l` on, and the row the key recorded is the only way out the column has.
    await user.keyboard("k");
    expect(selectedIds()).toEqual([]);
    expect(scroller()).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-c")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("an answer given from the column drops the record: Escape lands on the question it shows now", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const second = inboxIssue("ask-b", "CORE-2", "Which format?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    second,
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = rows.filter((row) => row.id !== "ask-b");
    return {
      ...second,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column shows ask-b while the focus is on ask-a, so the row `l` records is neither the
    // question being answered nor a row the answer takes off the list.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // The reader answers the question in front of them from inside the column. Their clicks move
    // no focus, so no blur fires: what ends the record is the column showing another question.
    const pane = screen.getByTestId("inbox-pane");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(pane).findByRole("radio", { name: "Ship" }));
    await user.click(within(pane).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    expect(rowIds()).toEqual(["ask-a", "ask-c"]);

    // Escape owes the reader the question the column is showing now, not the row they pressed
    // `l` on while it was showing the question they have since answered.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-c")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a snooze under h drops the record: Escape lands on the question the column moved to", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column shows ask-b while the focus is on ask-a, so the row `l` records is not the
    // question being deferred.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // `h` opens the column's own Snooze list, and `2` picks Tomorrow in it. The list stands over
    // the page, but it opens from the column and hands the focus back into it, so it keeps the
    // record. The deferral is what ends it, and both of the drops that keep the record's
    // question true cover that between them: the row folds into Later for a commit with nothing
    // shown at all, where the column goes and `holdPane` drops first, and the question the
    // column is handed next is the change the layout effect reads. This row pins where Escape
    // goes, which the two of them agree on; it does not tell them apart.
    await user.keyboard("h");
    expect(screen.getByRole("listbox", { name: /^Snooze CORE-2/ })).toBeInTheDocument();
    await user.keyboard("2");
    expect(snoozeAsk).toHaveBeenCalledTimes(1);

    // The deferred question folds into Later and the column takes the next one, whose row the
    // write's landing gives the keyboard focus (R8).
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    expect(rowIds()).toEqual(["ask-a", "ask-c"]);
    await waitFor(() => expect(shortRow("ask-c")).toHaveFocus());

    // Escape owes the reader the question in front of them, not the row they pressed `l` on
    // while the column was showing the one they deferred.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-c")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("a question the server withdraws drops the record: Escape lands on the one that replaced it", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column shows ask-b while the focus is on ask-a, so the row `l` records is a row the
    // withdrawal leaves exactly where it was.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // The server takes the selected question away while the reader is reading it: nothing of
    // theirs is typed or sent, so the column hands the selection on rather than keeping it. As
    // with the snooze above, both drops cover this row: the withdrawn row leaves the list for a
    // commit with nothing shown, where the column goes and `holdPane` drops the record, and the
    // question the column takes next is the change the layout effect reads after it.
    served = rows.filter((row) => row.id !== "ask-b");
    await act(async () => {
      queryClient.setQueryData<AskRead>(["ask-thread", "ask-b"], retracted(rows[1] as InboxRow));
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-c"]));
    expect(selectedIds()).toEqual(["ask-c"]);

    // Escape owes the reader the question that took its place, not the row they pressed `l` on
    // while the withdrawn one was in front of them.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-c")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the column going when the Inbox narrows drops the record, where nothing else can, and the row it showed is read in place of the list", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(1440);
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column shows ask-b while the focus is on ask-a, so the row `l` records is not the row
    // the column is showing.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // Below the column's width the row the column showed is read in place of the list. The
    // selection stands - the address still names it - so no question changed and the layout
    // effect drops nothing; the scroller goes with the column, taking the focus to the body with
    // no blur and no `focusin` behind it, so neither focus drop reads anything either. The
    // column wrapper's own unmount is the only report there is, which makes this `holdPane`'s
    // row alone.
    act(() => width.setWidth(1279));
    const view = await screen.findByTestId("inbox-reading-view");
    expect(within(view).getByTestId("ask-ask-b")).toBeTruthy();
    expect(screen.getByTestId("inbox-list").closest("[hidden]")).not.toBeNull();
    expect(address()).toBe("/?item=ask-b");
    expect(document.body).toHaveFocus();

    // Widened again, the column comes back on the same question with nothing recorded: Escape
    // owes the reader the question in front of them, never the row they pressed `l` on in the
    // column that has since gone.
    act(() => width.setWidth(1440));
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("the focus moving inside the column keeps the record: Escape still returns to the l row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // The column shows ask-b while the focus is on ask-a, so the two ways back are told apart.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // `h` opens the column's own Snooze list: it stands over the page, portalled out of the
    // column, but it opens from a control inside the column, and the focus moving into it and
    // through its choices names targets the column counts as its own. Escape closes it and hands
    // the focus back to the scroller, the question it shows has not changed - so the record
    // stands and a second Escape is still their way back to the row they came from. This is the
    // keep branch of the blur rule, the drop above it being the one the search box and a row
    // take.
    await user.keyboard("h");
    const menu = panelOf(
      within(screen.getByTestId("inbox-pane")).getByRole("button", { name: /^Snooze CORE-2/ })
    );
    expect(menu).toContainElement(document.activeElement as HTMLElement);
    await user.keyboard("{ArrowDown}");
    expect(menu).toContainElement(document.activeElement as HTMLElement);
    await user.keyboard("{Escape}");
    expect(scroller()).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-a")).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a k taken from inside the column drops the record: Escape lands on the question it stepped to", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = () => screen.getByTestId("inbox-pane-scroller");

    // Two steps with the focus on ask-a put the column on ask-c, so the row `l` records is two
    // rows away from the one the step below lands on.
    shortRow("ask-a").focus();
    await user.keyboard("j");
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    await user.keyboard("l");
    expect(scroller()).toHaveFocus();

    // The ordinary step out of the column: it hands the focus to the selected row first and
    // then steps the selection, so the reader stands on a row in the list with the column one
    // question further up.
    await user.keyboard("k");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(shortRow("ask-c")).toHaveFocus();

    // Back in the column the way a click takes the reader, with no `l` to record a row: Escape
    // owes them the question the column is showing, the record the step ended naming a question
    // they have walked off.
    scroller().focus();
    await user.keyboard("{Escape}");
    expect(shortRow("ask-b")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// R18: Enter carries the reader into the answer box from wherever they stand at the item in hand
// - its row, the page body, the column's scroller after `l`, an option of its card after a click
// - and a button keeps its own Enter.
test("Enter on an option of the card in hand, or on the column's scroller, puts the caret in its answer box", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", {
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    const answerBox = () => pane.querySelector("[data-ask-answer]");
    // A click on an option leaves the focus on its radio, which has no Enter of its own: the key
    // is the reader's way on from the pick into the box, and the pick stands. "Ship" is a
    // Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    const option = await within(pane).findByRole("radio", { name: "Ship" });
    await user.click(option);
    expect(option).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(answerBox()).toHaveFocus();
    expect((option as HTMLInputElement).checked).toBe(true);

    // From the column's own scroller, where `l` puts the reader.
    act(() => shortRow("ask-a").focus());
    await user.keyboard("l");
    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(answerBox()).toHaveFocus();

    // From the list it does its own job: the row hands the focus to the column's answer box.
    act(() => shortRow("ask-a").focus());
    await user.keyboard("{Enter}");
    expect(answerBox()).toHaveFocus();

    // A button of the column is no place Enter starts from: the press is the button's - the
    // head's Copy reference here, beside the card's own.
    const head = pane.querySelector<HTMLElement>("[data-inbox-pane-head]");
    if (head === null) throw new Error("the column drew no head");
    const copy = within(head).getByRole("button", { name: /^Copy reference/ });
    act(() => copy.focus());
    // user-event sends no event whose survival a test can read; only fireEvent returns it.
    expect(fireEvent.keyDown(copy, { key: "Enter" })).toBe(true);
    expect(copy).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("with the column on, Enter is the link's or the button's wherever the reader holds it", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    // A link in another row: the owner link, which opens that ask inside its issue. The
    // registry's handler calls `preventDefault` for every binding that fires, so a binding that
    // claimed Enter here would press the link for nothing and pull the focus into the selected
    // question's answer box instead.
    const link = shortRow("ask-b").querySelector<HTMLElement>("[data-inbox-owner]");
    if (link === null) throw new Error("the ask-b row has no owner link");
    link.focus();
    // user-event sends no event whose survival a test can read; only fireEvent returns it.
    expect(fireEvent.keyDown(link, { key: "Enter" })).toBe(true);
    expect(link).toHaveFocus();
    expect(pane.contains(document.activeElement)).toBe(false);

    // A button outside the list, with the column still showing an answerable question.
    const filter = await filterTrigger(user);
    filter.focus();
    // user-event sends no event whose survival a test can read; only fireEvent returns it.
    expect(fireEvent.keyDown(filter, { key: "Enter" })).toBe(true);
    expect(filter).toHaveFocus();

    // And the row's own Enter still carries the reader into the column's answer box.
    shortRow("ask-a").focus();
    await user.keyboard("{Enter}");
    expect(pane.querySelector("[data-ask-answer]")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The column's scroller with the measures a browser lays out and happy-dom does not: a column
 *  `clientHeight` tall over a document `scrollHeight` tall. */
function measuredScroller(clientHeight: number, scrollHeight: number): HTMLElement {
  const scroller = screen.getByTestId("inbox-pane-scroller");
  Object.defineProperty(scroller, "clientHeight", { configurable: true, value: clientHeight });
  Object.defineProperty(scroller, "scrollHeight", { configurable: true, value: scrollHeight });
  return scroller;
}

/** The digits a card's option rows draw, in their order. */
function digitsOf(card: HTMLElement): (string | null)[] {
  return [...card.querySelectorAll("kbd")].map((digit) => digit.textContent);
}

// R3: the open document has keys of its own, which the reader presses from the list while the
// focus and the selection stay where they are.
test("Space, Shift+Space, g g and Shift+G scroll the column from the list, moving neither the selection nor the focus", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = measuredScroller(400, 4000);
    const row = shortRow("ask-a");
    row.focus();

    // A page is 87.5% of the column's height, the step Chromium pages by.
    await user.keyboard("[Space]");
    expect(scroller.scrollTop).toBe(350);
    await user.keyboard("{Shift>}[Space]{/Shift}");
    expect(scroller.scrollTop).toBe(0);
    await user.keyboard("{Shift>}G{/Shift}");
    expect(scroller.scrollTop).toBe(4000);
    await user.keyboard("gg");
    expect(scroller.scrollTop).toBe(0);
    expect(row).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-a"]);

    // With the focus on nothing, where a reader who has pressed no key yet stands, as on a row.
    act(() => row.blur());
    expect(document.body).toHaveFocus();
    await user.keyboard("[Space]");
    expect(scroller.scrollTop).toBe(350);
    expect(document.body).toHaveFocus();
    expect(selectedIds()).toEqual(["ask-a"]);

    // Inside the column the keys are the browser's own, on whatever there holds the focus: the
    // scroller `l` hands the reader pages itself, so the registry leaves its Space alone.
    await user.keyboard("l");
    expect(scroller).toHaveFocus();
    // user-event sends no event whose survival a test can read; only fireEvent returns it.
    expect(fireEvent.keyDown(scroller, { code: "Space", key: " " })).toBe(true);
    expect(scroller.scrollTop).toBe(350);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("Space and Shift+Space leave a button, a link, a summary, a checkbox, a radio and a menu trigger their own press", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();
  // One control of each kind a press of Space works itself, put in the list beside the rows,
  // where the document's keys are pressed from.
  const controls = document.createElement("div");
  controls.innerHTML =
    '<button type="button">A button</button><a href="/elsewhere">A link</a>' +
    "<details><summary>A summary</summary></details>" +
    '<input aria-label="A checkbox" type="checkbox"><input aria-label="A radio" type="radio">' +
    '<span aria-haspopup="menu" role="button" tabindex="0">A menu trigger</span>';

  try {
    await screen.findByTestId("inbox-pane");
    const scroller = measuredScroller(400, 4000);
    const list = screen.getByTestId("inbox-list").firstElementChild;
    if (list === null) throw new Error("the list drew no rows' root");
    list.append(controls);
    for (const control of controls.querySelectorAll<HTMLElement>(
      "button, a, summary, input, [aria-haspopup]"
    )) {
      control.focus();
      expect(control).toHaveFocus();
      for (const shiftKey of [false, true]) {
        // user-event sends no event whose survival a test can read; only fireEvent returns it.
        expect(fireEvent.keyDown(control, { code: "Space", key: " ", shiftKey })).toBe(true);
      }
      expect(scroller.scrollTop).toBe(0);
    }

    // A row's own checkbox ticks on Space, as the reader asked, and the column stays put.
    const mark = within(shortRow("ask-a")).getByRole("checkbox");
    mark.focus();
    await user.keyboard("[Space]");
    expect((mark as HTMLInputElement).checked).toBe(true);
    expect(scroller.scrollTop).toBe(0);

    // `g g` and Shift+G are no control's own press, so they scroll from a control as from a row.
    await user.keyboard("{Shift>}G{/Shift}");
    expect(scroller.scrollTop).toBe(4000);
    expect(mark).toHaveFocus();
  } finally {
    controls.remove();
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// R17: the digit after the last real option is Other's, and the card in hand draws each digit.
test("the digit after a question's last option picks Other and puts the caret in its box, and the selected card shows each digit", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const three = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }, { label: "Drop" }],
  });
  const nine = inboxIssue("ask-b", "CORE-2", "Which day?", {
    options: Array.from({ length: 9 }, (_, index) => ({ label: `Day ${index + 1}` })),
  });
  const rows = [three, nine];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const first = renderInbox("/?item=ask-a");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    const card = await within(pane).findByTestId("ask-ask-a");
    expect(digitsOf(card)).toEqual(["1", "2", "3", "4"]);
    // Drawn for the eye: each option's name is still its label alone.
    expect(within(card).getByRole("radio", { name: "Other" })).toBeTruthy();
    shortRow("ask-a").focus();
    await user.keyboard("4");
    expect(within(card).getByRole("radio", { name: "Other" })).toBeChecked();
    expect(card.querySelector("[data-ask-answer]")).toHaveFocus();
  } finally {
    first.unmount();
  }

  // The tenth option is `0`, so a question of nine options takes its Other on `0`.
  const second = renderInbox("/?item=ask-b");
  try {
    const pane = await screen.findByTestId("inbox-pane");
    const card = await within(pane).findByTestId("ask-ask-b");
    expect(digitsOf(card)).toEqual(["1", "2", "3", "4", "5", "6", "7", "8", "9", "0"]);
    shortRow("ask-b").focus();
    await user.keyboard("0");
    expect(within(card).getByRole("radio", { name: "Other" })).toBeChecked();
    expect(card.querySelector("[data-ask-answer]")).toHaveFocus();
  } finally {
    second.unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// R17 and R18 below the column's width: the card in hand is the row holding the focus, itself or
// on one of its options, and only that row shows its digits.
test("in the drawer a card's digits show for its row in hand, and the keys act on the card around a focused option", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", {
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
    inboxIssue("ask-b", "CORE-2", "Which name?", {
      options: [{ label: "Rename" }, { label: "Keep" }],
    }),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    const first = await screen.findByTestId("ask-ask-a");
    const second = screen.getByTestId("ask-ask-b");
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    for (const card of [first, second]) {
      expect(digitsOf(card)).toEqual(["1", "2", "3"]);
      for (const digit of card.querySelectorAll("kbd")) {
        expect(digit.parentElement?.className).toContain(ROW_IN_HAND_CLASS);
      }
    }

    // A click on an option leaves the focus there, and Enter carries the reader on into that
    // card's answer box.
    await user.click(within(second).getByRole("radio", { name: "Rename" }));
    await user.keyboard("{Enter}");
    expect(second.querySelector("[data-ask-answer]")).toHaveFocus();

    // The digits act on the card around the focused option, and its Other's pick moves the
    // caret into that card's box; the other card keeps its pick.
    const ship = within(first).getByRole("radio", { name: "Ship" });
    await user.click(ship);
    await user.keyboard("2");
    expect(within(first).getByRole("radio", { name: "Hold" })).toBeChecked();
    expect(ship).toHaveFocus();
    await user.keyboard("3");
    expect(within(first).getByRole("radio", { name: "Other" })).toBeChecked();
    expect(first.querySelector("[data-ask-answer]")).toHaveFocus();
    expect(within(second).getByRole("radio", { name: "Rename" })).toBeChecked();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// R18: `r` is the reply box of the thread in hand, where it has one.
test("r puts the caret in the reply box of the thread in hand, and does nothing on a question", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const open = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const answered = issueAsk({
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "Ship it.", user: "alice" },
    id: "ask-done",
    issue: { assignee: "alice", key: "CORE-2", title: "Build question" },
    issue_key: "CORE-2",
    question: "Which build?",
    state: "answered",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([open]);
  const getAsk = mockAskReads([open, answered]);
  const getIssue = mockIssueReads();
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: [
      replyRow("ask-done", {
        issue: { key: "CORE-2", title: "Build question" },
        kind: "ask",
        ref: "/issues/CORE-2/asks/ask-done",
        subject: "Which build?",
      }),
    ],
    total: 1,
  });
  const { unmount } = renderInbox("/?item=ask-a");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await within(pane).findByTestId("ask-ask-a");
    // An open question takes its answer, not a reply, and draws no reply box: the key is not
    // the Inbox's there.
    shortRow("ask-a").focus();
    // user-event sends no event whose survival a test can read; only fireEvent returns it.
    expect(fireEvent.keyDown(shortRow("ask-a"), { key: "r" })).toBe(true);
    expect(shortRow("ask-a")).toHaveFocus();

    const thread = await waitFor(() => shortRow("reply:ask-done"));
    await user.click(thread);
    await waitFor(() => expect(selectedIds()).toEqual(["reply:ask-done"]));
    // The thread's column is another frame than the question's, so it is read afresh.
    const box = await waitFor(() => {
      const found = screen
        .getByTestId("inbox-pane")
        .querySelector<HTMLElement>("[data-inbox-thread-body] [data-reply-box]");
      if (found === null) throw new Error("the thread drew no reply box");
      return found;
    });
    thread.focus();
    await user.keyboard("r");
    expect(box).toHaveFocus();
  } finally {
    unmount();
    narrow();
    listMyReplies.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// R18, and R11 for the answered question's Reply (#3729): an answered question's thread is a
// Reply row whose card is the question's own card, its reply box under its record, and `r` puts
// the caret in that box from its row below the column's width as beside it (the row above). So
// the card's Reply is the box's own submit, reached from where `r` leaves the reader, as a
// thread's Send is: the action "Reply in the thread of the item in hand".
test("below the column's width r on an answered question's thread row opens its reading view with the caret in the reply box under the question's record", async () => {
  const user = userEvent.setup();
  const answered = issueAsk({
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "Ship it.", user: "alice" },
    id: "ask-done",
    issue: { assignee: "alice", key: "CORE-2", title: "Build question" },
    issue_key: "CORE-2",
    question: "Which build?",
    state: "answered",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getAsk = mockAskReads([answered]);
  const getIssue = mockIssueReads();
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: [
      replyRow("ask-done", {
        issue: { key: "CORE-2", title: "Build question" },
        kind: "ask",
        ref: "/issues/CORE-2/asks/ask-done",
        subject: "Which build?",
      }),
    ],
    total: 1,
  });
  const { unmount } = renderInbox();
  try {
    const row = await waitFor(() => shortRow("reply:ask-done"));
    act(() => row.focus());
    await user.keyboard("r");
    // The short row draws no box: `r` opens the row's reading view at its reply box.
    const view = await screen.findByTestId("inbox-reading-view");
    await waitFor(() =>
      expect(view.querySelector("[data-inbox-thread-body] [data-reply-box]")).toHaveFocus()
    );
    expect(within(view).getByRole("button", { name: "Reply" })).toBeTruthy();
  } finally {
    unmount();
    listMyReplies.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The planner session as the live list holds it, taking a message in either mode. */
const PLANNER_SESSION: Agent = {
  capabilities: ["steer", "btw"],
  dir: "/w",
  host: "m",
  last_activity: null,
  last_seen: Date.now(),
  machine_id: "m",
  open_asks: 0,
  roles: [],
  session_id: "planner",
  state: "live",
  title: "Planner",
};

/** The reads of an Inbox that lists the planner's Agent row alone (`plannerAgentRow`): its
 *  conversation, the message the reader sent and the session's reply, and the live list holding
 *  `sessions`. Returns what puts them back. */
function plannerRowReads(sessions: readonly Agent[]): () => void {
  const message = (id: string, body: string, overrides: Partial<Message> = {}): Message => ({
    author: { id: "alice", kind: "user" },
    body,
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:planner",
    ...overrides,
  });
  const spies = [
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listMyReplies").mockResolvedValue({
      as_of: "2026-10-12T02:00:00Z",
      limit: 200,
      offset: 0,
      rows: [plannerAgentRow()],
      total: 1,
    }),
    spyOn(api, "listAgents").mockResolvedValue([...sessions]),
    spyOn(api, "listAgentMessages").mockResolvedValue([
      {
        message: message("message-1", "Have a look at the build"),
        replies: [
          message("reply-1", "Looked at it.", {
            author: { id: "planner", kind: "session" },
            created_at: "2026-10-12T01:00:00Z",
            in_reply_to: "message-1",
          }),
        ],
      },
    ]),
  ];
  return () => {
    for (const spy of spies) spy.mockRestore();
  };
}

// R18 on an Agent row below the column's width: its box is drawn only while the live list holds
// its session (`AgentThreadBody`), which nothing the Inbox reads before the view draws says. So
// `r` opens the row's view and lands once the thread has drawn: in its box, as on any other
// thread row (the row above) ...
test("below the column's width r on an Agent row whose session is live opens its reading view with the caret in the session's reply box", async () => {
  const user = userEvent.setup();
  const restore = plannerRowReads([PLANNER_SESSION]);
  const { unmount } = renderInbox();
  try {
    const row = await waitFor(() => shortRow("agent:message-1"));
    act(() => row.focus());
    await user.keyboard("r");
    const view = await screen.findByTestId("inbox-reading-view");
    await waitFor(() =>
      expect(
        view.querySelector("[data-inbox-thread-body] [data-inbox-agent-composer] [data-reply-box]")
      ).toHaveFocus()
    );
  } finally {
    unmount();
    restore();
  }
});

// ... and at the view's scroller, where any pick lands, once the thread has said that the live
// list no longer holds the session and drawn no box.
test("below the column's width r on an Agent row whose session is not live opens its reading view at its scroller once the thread says the session has ended", async () => {
  const user = userEvent.setup();
  const restore = plannerRowReads([]);
  const { unmount } = renderInbox();
  try {
    const row = await waitFor(() => shortRow("agent:message-1"));
    act(() => row.focus());
    await user.keyboard("r");
    const view = await screen.findByTestId("inbox-reading-view");
    await within(view).findByTestId("agent-session-ended");
    await waitFor(() => expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus());
    expect(view.querySelector("[data-reply-box]")).toBeNull();
  } finally {
    unmount();
    restore();
  }
});

// R18 on a thread that takes no reply: a comment its author deleted takes none (`409
// POST_DELETED`), so its thread draws no box under it (`KeptReplyToDeleted`). `r` opens the row's
// view at its scroller, where any pick lands, rather than leaving the reader on the page's body
// waiting for a box that never comes.
test("below the column's width r on the short row of a comment thread whose root its author deleted opens its reading view at its scroller", async () => {
  const user = userEvent.setup();
  const deleted = { deleted_at: "2026-10-12T00:40:00Z", deleted_by: { id: "alice", kind: "user" } };
  const comment = (id: string, overrides: Partial<Comment>): Comment => ({
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "",
    created_at: "2026-10-12T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
    ...overrides,
  });
  const spies = [
    spyOn(api, "getInbox").mockResolvedValue([]),
    mockIssueReads(),
    spyOn(api, "listMyReplies").mockResolvedValue({
      as_of: "2026-10-12T02:00:00Z",
      limit: 200,
      offset: 0,
      rows: [replyRow("c1", { ...deleted, subject: "" })],
      total: 1,
    }),
    spyOn(api, "listComments").mockResolvedValue([
      comment("c1", deleted),
      comment("c1-reply", {
        author: { id: "s1", kind: "session" },
        body: "Done.",
        created_at: "2026-10-12T01:00:00Z",
        reply_to: "c1",
      }),
    ]),
  ];
  const { unmount } = renderInbox();
  try {
    const row = await waitFor(() => shortRow("reply:c1"));
    act(() => row.focus());
    await user.keyboard("r");
    const view = await screen.findByTestId("inbox-reading-view");
    await within(view).findByText("Done.");
    await waitFor(() => expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus());
    expect(view.querySelector("[data-reply-box]")).toBeNull();
  } finally {
    unmount();
    for (const spy of spies) spy.mockRestore();
  }
});

// R18 on the thread of a question still open: it waits on its answer, and an open ask takes no
// reply (`takesReply`), so its card draws no reply box and no "Write a reply". The thread is the
// reader's row because they wrote in it while the question waits on somebody else. `r` opens
// the row's view at its scroller, where any pick lands.
test("below the column's width r on the thread row of a question still open opens its reading view at its scroller", async () => {
  const user = userEvent.setup();
  const open = issueAsk({
    asked_of: "bob",
    id: "ask-open",
    issue: { assignee: "bob", key: "CORE-2", title: "Build question" },
    issue_key: "CORE-2",
    question: "Which build?",
  });
  const spies = [
    spyOn(api, "getInbox").mockResolvedValue([]),
    mockAskReads([open]),
    mockIssueReads(),
    spyOn(api, "listMyReplies").mockResolvedValue({
      as_of: "2026-10-12T02:00:00Z",
      limit: 200,
      offset: 0,
      rows: [
        replyRow("ask-open", {
          issue: { key: "CORE-2", title: "Build question" },
          kind: "ask",
          ref: "/issues/CORE-2/asks/ask-open",
          subject: "Which build?",
        }),
      ],
      total: 1,
    }),
  ];
  const { unmount } = renderInbox();
  try {
    const row = await waitFor(() => shortRow("reply:ask-open"));
    act(() => row.focus());
    await user.keyboard("r");
    const view = await screen.findByTestId("inbox-reading-view");
    await within(view).findByTestId("ask-ask-open");
    await waitFor(() => expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus());
    expect(view.querySelector("[data-reply-box], [data-reply-opener]")).toBeNull();
  } finally {
    unmount();
    for (const spy of spies) spy.mockRestore();
  }
});

// R20: Shift+O is `o` in a new tab, and this one keeps its address.
test("Shift+O opens what o opens in a new tab and leaves the Inbox where it is", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const open = spyOn(window, "open").mockImplementation(() => null);
  const { unmount } = renderInbox("/?item=ask-b");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await within(pane).findByTestId("ask-ask-b");
    const owner = pane.querySelector<HTMLAnchorElement>("[data-inbox-owner]");
    if (owner === null) throw new Error("the column's head drew no owner link");
    shortRow("ask-b").focus();
    await user.keyboard("{Shift>}O{/Shift}");
    expect(open).toHaveBeenCalledTimes(1);
    expect(open).toHaveBeenCalledWith(owner.href, "_blank", "noopener");
    expect(address()).toBe("/?item=ask-b");
    expect(shortRow("ask-b")).toHaveFocus();
  } finally {
    unmount();
    narrow();
    open.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// dispatch://INFRA-2160, R24: Shift+F opens the Filter panel and Shift+V the View menu from the
// page, and Escape puts the focus back where it was before the key.
test("Shift+F opens the Filter panel and Shift+V the View menu on their first control, and Escape hands the focus back to where it was", async () => {
  const user = userEvent.setup();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByTestId("ask-ask-a");
    act(() => {
      if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    });
    await user.keyboard("{Shift>}F{/Shift}");
    const panel = screen.getByRole("dialog", { name: "Filter" });
    expect(within(panel).getByRole("button", { name: "Filter by priority" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
    expect(document.activeElement).toBe(document.body);

    const row = shortRow("ask-a");
    act(() => row.focus());
    await user.keyboard("{Shift>}V{/Shift}");
    const menu = screen.getByRole("dialog", { name: "View" });
    // Sort by and Thread order both offer Newest first; Sort by's is the menu's first control.
    expect(within(menu).getAllByRole("radio", { name: "Newest first" })[0]).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "View" })).toBeNull();
    expect(row).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// INFRA-1109's probe: Escape is a panel's while one stands open, never the selection's. With rows
// marked, the View menu opened by a press on its trigger takes the first Escape, which closes it
// and keeps what the reader marked, from the trigger as from where the press left the focus (the
// menu's first control); the second, with the menu shut, clears the selection.
test("with rows selected, the first Escape closes the View menu opened by a press on its trigger and keeps the selection, and the second clears it", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/", { pane: false });
  const mark = (askId: string) =>
    within(rowOf(askId)).getByRole<HTMLInputElement>("checkbox", { name: /^Select / });
  /** Whether the menu and both marks stand as `open` and `marked` say, after `when`. */
  const expectMenuAndMarks = (when: string, open: boolean, marked: boolean) => {
    expect(screen.queryByRole("dialog", { name: "View" }) !== null, `the menu, ${when}`).toBe(open);
    expect(mark("ask-a").checked, `ask-a, ${when}`).toBe(marked);
    expect(mark("ask-b").checked, `ask-b, ${when}`).toBe(marked);
  };
  try {
    await screen.findByText("Which format?");
    await user.click(mark("ask-a"));
    await user.click(mark("ask-b"));
    await screen.findByText("2 selected");
    const trigger = screen.getByRole("button", { name: "View" });

    // The focus on the trigger, as main's probe had it.
    await user.click(trigger);
    expectMenuAndMarks("opened", true, true);
    act(() => trigger.focus());
    await user.keyboard("{Escape}");
    expectMenuAndMarks("after one Escape from the trigger", false, true);

    // The focus where the press leaves it: the menu's first control.
    await user.click(trigger);
    const menu = screen.getByRole("dialog", { name: "View" });
    expect(within(menu).getAllByRole("radio", { name: "Newest first" })[0]).toHaveFocus();
    await user.keyboard("{Escape}");
    expectMenuAndMarks("after one Escape from the menu", false, true);
    expect(screen.getByText("2 selected")).toBeTruthy();

    await user.keyboard("{Escape}");
    expectMenuAndMarks("after the second Escape", false, false);
    expect(screen.queryByText("2 selected")).toBeNull();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The schema a decision block is built with, so a test can construct the node view the real
 *  editor constructs for every `ask` node of the document (`ProofDocument.decisions.test.tsx`
 *  does the same against the same fake editor). */
const askBlockSchema = new Schema({
  nodes: {
    ask: {
      attrs: {
        answer: { default: undefined },
        answered_at: { default: undefined },
        answered_by: { default: undefined },
        blockId: { default: null },
        invalid: { default: undefined },
        multiple: { default: false },
        selected: { default: undefined },
        state: { default: "open" },
        urgency: { default: "med" },
      },
      content: "paragraph+",
      group: "block",
    },
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
  },
});

function askBlockNode(blockId: string, question: string): ProseMirrorNode {
  return askBlockSchema.node("ask", { blockId }, [
    askBlockSchema.node("paragraph", undefined, [askBlockSchema.text(question)]),
  ]);
}

const SPEC_DOCUMENT: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

test("the answer keys act on the selected decision block, not the first one in the document", async () => {
  const user = userEvent.setup();
  // Both questions are decision blocks of one document, so the column draws that document and
  // hosts an answerable card for each of them. The selected one is the second card in the DOM.
  const narrow = widenToReadingPane();
  const blockArtifact = { id: "artifact-1", primary: true, slug: "spec" };
  const first = inboxIssue("ask-a", "CORE-1", "Ship behind a flag?", {
    block_artifact: blockArtifact,
    block_id: "block-a",
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-1", "Rename the flag?", {
    block_artifact: blockArtifact,
    block_id: "block-b",
    options: [{ label: "Rename" }, { label: "Keep" }],
  });
  const rows = [first, second];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(SPEC_DOCUMENT);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    token: "",
    version: null,
  });
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue(
    rows.map((row) => ({ ...row, last_reply: null }))
  );
  const { unmount } = renderInbox("/?item=ask-b", { runtime: fake.runtime });

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-b"]);
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    // The fake editor records the node views the document installs; constructing the `ask` one
    // per block is what the real editor does for the document's two open decisions.
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    act(() => {
      editor.root.append(construct(askBlockNode("block-a", first.question)).dom);
      editor.root.append(construct(askBlockNode("block-b", second.question)).dom);
    });
    const selectedCard = await within(pane).findByTestId("ask-ask-b");
    const firstCard = within(pane).getByTestId("ask-ask-a");
    // Only the selected question's card draws the digits: the keys pick nothing on the other.
    expect(digitsOf(selectedCard)).toEqual(["1", "2", "3"]);
    expect(digitsOf(firstCard)).toEqual([]);

    // `1` is the selected question's first option, not the first block's.
    await user.keyboard("1");
    await waitFor(() =>
      expect(
        (within(selectedCard).getByRole("radio", { name: "Rename" }) as HTMLInputElement).checked
      ).toBe(true)
    );
    expect(
      (within(firstCard).getByRole("radio", { name: "Ship" }) as HTMLInputElement).checked
    ).toBe(false);

    // One Enter opens the selected card's own note disclosure - the first block's stays shut -
    // and leaves the reader in the box it reveals, because the card focuses the field its own
    // render draws (`AskCard.tsx`). `fireEvent` flushes that render inside the press, as a
    // browser does not, so this row cannot prove the browser case: the proof is the e2e row
    // `Enter opens the selected decision block's note field and puts the focus in it`.
    await user.keyboard("{Enter}");
    const answerBox = selectedCard.querySelector("[data-ask-answer]");
    expect(answerBox).not.toBeNull();
    expect(firstCard.querySelector("[data-ask-answer]")).toBeNull();
    expect(answerBox).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getArtifact.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifactText.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    listIssueAsks.mockRestore();
  }
});

/** Draws one decision block in the column's editor, as the real editor draws one for every
 *  `ask` node of the document: the node view stamps its element with the block id, which is
 *  what the column places itself by. */
function drawAskBlock(editor: FakeEditor, blockId: string, question: string): void {
  const nodeViews = editor.viewProps.nodeViews as Record<
    string,
    (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
  >;
  const construct = nodeViews.ask;
  if (construct === undefined) throw new Error("the ask node view was not installed");
  act(() => {
    editor.root.append(construct(askBlockNode(blockId, question)).dom);
  });
}

/** A question written as a decision block of `SPEC_DOCUMENT`. Its block stands before any
 *  heading, so the section the column opens at is the document's start and the placement writes
 *  `scrollTop` itself: happy-dom lays nothing out, so a scroll to a heading would measure a move
 *  of zero and say nothing about where the column went. */
function decisionRow(id: string, blockId: string, question: string): InboxRow {
  return inboxIssue(id, "CORE-1", question, {
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: blockId,
  });
}

/** Every read the column makes while it draws these rows' questions, and the one restore that
 *  puts them all back. `getArtifact` is handed back too, for the row that makes that read fail
 *  and land again. */
function mockDocumentColumnReads(rows: readonly InboxRow[]) {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(SPEC_DOCUMENT);
  const mocks = [
    spyOn(api, "getInbox").mockResolvedValue([...rows]),
    mockAskReads(rows),
    mockIssueReads(),
    getArtifact,
    spyOn(api, "getArtifactText").mockResolvedValue({ markdown: "", token: "", version: null }),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue(
      rows.map((row) => ({ ...row, last_reply: null }))
    ),
  ];
  return {
    getArtifact,
    restore: () => {
      for (const mock of mocks) {
        mock.mockRestore();
      }
    },
  };
}

test("the first draw of a question's document puts the column at the start of its section", async () => {
  const narrow = widenToReadingPane();
  const ask = decisionRow("ask-a", "block-a", "Ship behind a flag?");
  const rows = [ask];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const reads = mockDocumentColumnReads(rows);
  const { unmount } = renderInbox("/?item=ask-a", { runtime: fake.runtime });

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    const scroller = screen.getByTestId("inbox-pane-scroller");
    // The one scroller outlives the row, so it starts wherever the reader last left it.
    scroller.scrollTop = 240;
    drawAskBlock(editor, "block-a", ask.question);
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
  } finally {
    unmount();
    narrow();
    reads.restore();
  }
});

test("a question's document lost and read again leaves the column where the reader left it", async () => {
  // One placement is armed for each row the reader opens, so the document going and coming
  // back - a read that fails and the read after it that lands - arms none. The row waits the
  // hold's quiet window out before it leaves the reader anywhere: while the hold is on, the
  // column is the first placement's to write, and a row that read `scrollTop` before it ended
  // would pass whether or not a second placement had been armed.
  const narrow = widenToReadingPane();
  const ask = decisionRow("ask-a", "block-a", "Ship behind a flag?");
  const rows = [ask];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const reads = mockDocumentColumnReads(rows);
  const { queryClient, unmount } = renderInbox("/?item=ask-a", { runtime: fake.runtime });

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    const scroller = screen.getByTestId("inbox-pane-scroller");
    drawAskBlock(editor, "block-a", ask.question);
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
    // The drawing has been quiet for longer than the hold waits, so the first placement is over.
    await act(async () => {
      await Bun.sleep(QUIET_MS * 3);
    });
    scroller.scrollTop = 240;

    // The document's read fails, and the column falls back to the question's own card.
    reads.getArtifact.mockRejectedValue(new Error("DOCUMENT_UNLOADABLE"));
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["artifact", "artifact-1"] });
    });
    await waitFor(() => expect(fake.connections[0]?.destroyed).toBe(true));

    // The read after it lands, so the column opens the same document again and draws the block.
    reads.getArtifact.mockResolvedValue(SPEC_DOCUMENT);
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["artifact", "artifact-1"] });
    });
    await waitFor(() => expect(fake.connections).toHaveLength(2));
    act(() => fake.sync());
    // The fake's sync reaches every connection it ever made, the one the lost document left
    // behind among them, so the editor to draw in is the one whose root stands in the column.
    const reopened = await waitFor(() => {
      const live = fake.editors.find((candidate) => scroller.contains(candidate.root));
      if (live === undefined) throw new Error("the column mounted no editor of its own");
      return live;
    });
    drawAskBlock(reopened, "block-a", ask.question);
    await within(screen.getByTestId("inbox-pane")).findByTestId("ask-ask-a");

    expect(scroller.scrollTop).toBe(240);
  } finally {
    unmount();
    narrow();
    reads.restore();
  }
});

test("opening another question arms a placement of its own", async () => {
  // Two decision blocks of one document, so the column keeps the document it has open and only
  // the row the reader opened changes. The row waits the hold's quiet window out first, so the
  // column the second row writes is written by the second row's own placement.
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = decisionRow("ask-a", "block-a", "Ship behind a flag?");
  const second = decisionRow("ask-b", "block-b", "Rename the flag?");
  const rows = [first, second];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const reads = mockDocumentColumnReads(rows);
  const { unmount } = renderInbox("/?item=ask-a", { runtime: fake.runtime });

  try {
    await screen.findByTestId("inbox-pane");
    expect(selectedIds()).toEqual(["ask-a"]);
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    const scroller = screen.getByTestId("inbox-pane-scroller");
    drawAskBlock(editor, "block-a", first.question);
    drawAskBlock(editor, "block-b", second.question);
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
    await act(async () => {
      await Bun.sleep(QUIET_MS * 3);
    });
    scroller.scrollTop = 240;

    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
  } finally {
    unmount();
    narrow();
    reads.restore();
  }
});

// R3: a scroll by the document's keys from the list is the reader moving the column, as a press
// in it is, so the placement lets go of it - pressed before the document is read as much as
// after, since the browser row presses Space while the reads are still out.
for (const [name, keys] of [
  ["Space", "[Space]"],
  ["Shift+Space", "{Shift>}[Space]{/Shift}"],
  ["g g", "gg"],
  ["Shift+G", "{Shift>}G{/Shift}"],
] as const) {
  test(`${name} from the list while the question's document opens lets its placement go`, async () => {
    const user = userEvent.setup();
    const narrow = widenToReadingPane();
    const ask = decisionRow("ask-a", "block-a", "Ship behind a flag?");
    const fake = fakeDocumentRuntime({ text: "The live document" });
    const reads = mockDocumentColumnReads([ask]);
    const { unmount } = renderInbox("/?item=ask-a", { runtime: fake.runtime });

    try {
      await screen.findByTestId("inbox-pane");
      const scroller = screen.getByTestId("inbox-pane-scroller");
      shortRow("ask-a").focus();
      await user.keyboard(keys);
      // Where the reader read down to. A placement still holding the column would put it back
      // at the start of the block's section, which is the document's start (`decisionRow`).
      scroller.scrollTop = 240;
      await waitFor(() => expect(fake.connections).toHaveLength(1));
      act(() => fake.sync());
      await waitFor(() => expect(fake.editors).toHaveLength(1));
      const editor = fake.editors[0];
      if (editor === undefined) throw new Error("the column mounted no editor");
      drawAskBlock(editor, "block-a", ask.question);
      await within(screen.getByTestId("inbox-pane")).findByTestId("ask-ask-a");
      expect(scroller.scrollTop).toBe(240);
      expect(shortRow("ask-a")).toHaveFocus();
    } finally {
      unmount();
      narrow();
      reads.restore();
    }
  });
}

// R18: a decision block folds its thread's reply box behind its own "Write a reply", and `r`
// presses that for the reader before it puts the caret in the box it draws. The column draws the
// block's whole document, so the control is the one that names the thread's own ask.
test("r on the thread of a decision block presses its Write a reply and puts the caret in the box", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const done: InboxRow = {
    ...decisionRow("ask-done", "block-a", "Ship behind a flag?"),
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "Ship it.", user: "alice" },
    state: "answered",
  };
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const spies = [
    spyOn(api, "getInbox").mockResolvedValue([]),
    mockAskReads([done]),
    mockIssueReads(),
    spyOn(api, "getArtifact").mockResolvedValue(SPEC_DOCUMENT),
    spyOn(api, "getArtifactText").mockResolvedValue({ markdown: "", token: "", version: null }),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([{ ...done, last_reply: null }]),
    spyOn(api, "listMyReplies").mockResolvedValue({
      as_of: "2026-10-12T00:00:00Z",
      limit: 200,
      offset: 0,
      rows: [
        replyRow("ask-done", {
          kind: "ask",
          ref: "/issues/CORE-1/asks/ask-done",
          subject: "Ship behind a flag?",
        }),
      ],
      total: 1,
    }),
  ];
  const { unmount } = renderInbox("/?item=reply%3Aask-done", { runtime: fake.runtime });

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    drawAskBlock(editor, "block-a", done.question);
    const opener = await within(screen.getByTestId("inbox-pane")).findByRole("button", {
      name: "Write a reply",
    });
    expect(opener).toHaveAttribute("aria-expanded", "false");

    shortRow("reply:ask-done").focus();
    await user.keyboard("r");
    expect(opener).toHaveAttribute("aria-expanded", "true");
    expect(document.activeElement?.matches("[data-reply-box]")).toBe(true);
    expect(document.getElementById(opener.getAttribute("aria-controls") ?? "")).toContainElement(
      document.activeElement as HTMLElement
    );
  } finally {
    unmount();
    narrow();
    for (const spy of spies) spy.mockRestore();
  }
});

// R18 below the column's width, in a decision document that holds another question still open:
// that question's card draws no reply box and says so, and the mark names its own thread. So `r`
// on the answered question's thread waits for that thread's own "Write a reply", even where the
// open question's block is drawn first, and puts the caret in the box it opens, never at the
// view's scroller as though this thread had no box.
test("below the column's width r on a decision block's thread lands in its own reply box, past another open question's card drawn first", async () => {
  const user = userEvent.setup();
  const done: InboxRow = {
    ...decisionRow("ask-done", "block-a", "Ship behind a flag?"),
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "Ship it.", user: "alice" },
    state: "answered",
  };
  const open: InboxRow = {
    ...decisionRow("ask-open", "block-b", "Rename the flag?"),
    asked_of: "bob",
  };
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const threadRow = (ask: InboxRow) =>
    replyRow(ask.id, { kind: "ask", ref: `/issues/CORE-1/asks/${ask.id}`, subject: ask.question });
  const spies = [
    spyOn(api, "getInbox").mockResolvedValue([]),
    mockAskReads([done, open]),
    mockIssueReads(),
    spyOn(api, "getArtifact").mockResolvedValue(SPEC_DOCUMENT),
    spyOn(api, "getArtifactText").mockResolvedValue({ markdown: "", token: "", version: null }),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue(
      [done, open].map((row) => ({ ...row, last_reply: null }))
    ),
    spyOn(api, "listMyReplies").mockResolvedValue({
      as_of: "2026-10-12T00:00:00Z",
      limit: 200,
      offset: 0,
      rows: [threadRow(done), threadRow(open)],
      total: 2,
    }),
  ];
  const { unmount } = renderInbox("/", { runtime: fake.runtime });

  try {
    const row = await waitFor(() => shortRow("reply:ask-done"));
    act(() => row.focus());
    await user.keyboard("r");
    const view = await screen.findByTestId("inbox-reading-view");
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the view mounted no editor");
    // The open question's block first: its card draws no box, and the view lands nowhere yet.
    drawAskBlock(editor, "block-b", open.question);
    await within(view).findByTestId("ask-ask-open");
    await afterFrameAndTasks();
    expect(screen.getByTestId("inbox-pane-scroller")).not.toHaveFocus();

    drawAskBlock(editor, "block-a", done.question);
    const opener = await within(view).findByRole("button", { name: "Write a reply" });
    await waitFor(() => expect(opener).toHaveAttribute("aria-expanded", "true"));
    const box = document.getElementById(opener.getAttribute("aria-controls") ?? "");
    await waitFor(() => expect(box?.querySelector("[data-reply-box]")).toHaveFocus());
  } finally {
    unmount();
    for (const spy of spies) spy.mockRestore();
  }
});

// The same refusal, pressed on the card the reading column draws for a question written as a
// decision block. That card is the document page's own (`ReadingPane` -> `DocumentPane`, in
// `PaneDocument.tsx`, -> `ProofDocument` -> `AskBlockCard`), so no prop of this list reaches
// it: the write it makes is the list's, which it reads from the context the list provides.
// Without it the optimistic `waiting_on_person` drops the row, the column has nothing selected
// to draw, and the refusal comes back to no card at all.
test("a refused hand-on from the column's decision block keeps the row, with the reason on that card", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  // Handed to alice on an issue bob holds, so handing it back to bob takes it out of her list.
  const mine = inboxIssue("ask-mine", "CORE-1", "Ship behind a flag?", {
    asked_of: "alice",
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: "block-a",
    issue: { assignee: "bob", key: "CORE-1", title: "Bob's issue" },
  });
  const rows = [mine];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const people = mockPeople();
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(SPEC_DOCUMENT);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    token: "",
    version: null,
  });
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue(
    rows.map((row) => ({ ...row, last_reply: null }))
  );
  const handOn = Promise.withResolvers<InboxRow>();
  const handAskTo = spyOn(api, "handAskTo").mockImplementation(() => handOn.promise);
  const { unmount } = renderInbox("/?item=ask-mine", { runtime: fake.runtime });

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    const editor = fake.editors[0];
    if (editor === undefined) throw new Error("the column mounted no editor");
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    act(() => {
      editor.root.append(construct(askBlockNode("block-a", mine.question)).dom);
    });
    const card = await within(pane).findByTestId("ask-ask-mine");

    await handOnTo(user, card, "bob");
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-mine", "bob", keyedAttempt));
    // Handed on optimistically, the row waits on somebody else - and the column keeps the card
    // the reader pressed, which is the only surface the server's answer can reach.
    await waitFor(() => expect(within(card).getByText("Handing on…")).toBeTruthy());
    expect(rowIds()).toEqual(["ask-mine"]);

    await act(async () => {
      handOn.reject(
        new ApiError(409, {
          code: "ASK_NOT_OPEN",
          error: "only open asks may be handed to another person",
        })
      );
      await handOn.promise.catch(() => undefined);
    });
    const alert = await within(card).findByRole("alert");
    expect(alert).toHaveTextContent("only open asks may be handed to another person");
    expect(card.isConnected).toBe(true);
    expect(rowIds()).toEqual(["ask-mine"]);
  } finally {
    unmount();
    narrow();
    handAskTo.mockRestore();
    people.mockRestore();
    getArtifact.mockRestore();
    getArtifactBlocks.mockRestore();
    getArtifactText.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    listIssueAsks.mockRestore();
  }
});

// The other side of that hold: a card under the Inbox that speaks for an ask this list draws
// no row for. The card of a Reply row's thread is one - the reader replied in a question that
// waits on somebody else, so her own list holds the thread and hides the ask - and so is every
// ask block of a document open in the column beside the selected one. A hand-on from one of
// those must leave the list as it stands: the hold is for a row the list drew, and nothing here
// is one. Held anyway, the optimistic `waiting_on_person` would draw the hidden question into
// her list while the write is out.
test("a refused hand-on from a Reply-thread card leaves the reader's own list as it was", async () => {
  const user = userEvent.setup();
  // Handed to bob on carol's issue, so alice's own list hides the ask itself; she is in its
  // thread, which is the row she has.
  const theirs = issueAsk({
    asked_of: "bob",
    id: "ask-theirs",
    issue: { assignee: "carol", key: "CORE-2", title: "Carol's issue" },
    issue_key: "CORE-2",
    question: "Which approach?",
  });
  const rows = [theirs];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const people = mockPeople();
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: [
      replyRow("ask-theirs", {
        issue: { key: "CORE-2", title: "Carol's issue" },
        kind: "ask",
        ref: "/issues/CORE-2/asks/ask-theirs",
        subject: "Which approach?",
      }),
    ],
    total: 1,
  });
  const handOn = Promise.withResolvers<InboxRow>();
  const handAskTo = spyOn(api, "handAskTo").mockImplementation(() => handOn.promise);
  const { unmount } = renderInbox("/", { pane: false });
  try {
    const row = await waitFor(() => {
      const found = document.querySelector<HTMLElement>('[data-inbox-row="reply:ask-theirs"]');
      if (found === null) throw new Error("the Reply row has not rendered");
      return found;
    });
    await within(row).findByLabelText("Hand this question to");
    expect(rowIds()).toEqual(["reply:ask-theirs"]);

    // Handed back to the assignee of its own issue, from the thread card.
    await handOnTo(user, row, "carol");
    await waitFor(() =>
      expect(handAskTo).toHaveBeenCalledWith("ask-theirs", "carol", keyedAttempt)
    );
    await waitFor(() => expect(within(row).getByText("Handing on…")).toBeTruthy());
    expect(rowIds()).toEqual(["reply:ask-theirs"]);

    await act(async () => {
      handOn.reject(
        new ApiError(409, {
          code: "ASK_NOT_OPEN",
          error: "only open asks may be handed to another person",
        })
      );
      await handOn.promise.catch(() => undefined);
    });
    // The refusal is on the card that wrote it, and the question alice never listed is still
    // not in her list.
    const alert = await within(row).findByRole("alert");
    expect(alert).toHaveTextContent("only open asks may be handed to another person");
    expect(rowIds()).toEqual(["reply:ask-theirs"]);
  } finally {
    unmount();
    handAskTo.mockRestore();
    listMyReplies.mockRestore();
    people.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("at the column's width with no column on the page, Enter is the focused control's", async () => {
  // The Inbox is wide enough for the reading column and has nothing to select, so no column is
  // mounted: `inbox-pane` is absent, exactly as it is in the commit a snooze of the last
  // selected row leaves behind. The column's keys have nothing to act on there, and the
  // registry's handler calls `preventDefault` for every binding it matches, so a key they claim
  // anyway is a key the control holding the focus never gets - Enter on the assignee filter,
  // on `Answers`, on the `Later` disclosure.
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-list");
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    const filter = await filterTrigger(user);
    filter.focus();
    // Only fireEvent answers whether the event survived its listeners, which user-event cannot: the button's own Enter is the browser's default action, so a swallowed key never presses it.
    expect(fireEvent.keyDown(filter, { key: "Enter" })).toBe(true);
    expect(filter).toHaveFocus();
  } finally {
    unmount();
    narrow();
    getInbox.mockRestore();
  }
});

test("a bulk snooze keeps the list and the picker while it is out, and hands the selection and the focus to the row after the last snoozed one", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  // Held open, so the renders under test are the ones the pick leaves behind.
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    // `x` marks the selected row, `j` moves on, `x` marks that one: the first two rows.
    await user.keyboard("x");
    await user.keyboard("j");
    await user.keyboard("x");
    await screen.findByText("2 selected");
    // Beside the column `j` and `x` move the selection and mark rows with the focus on nothing,
    // and the list `h` opens hands it back there, to the page itself, as it closes
    // (dispatch://INFRA-2160, D5); the bar leaves that hand-back alone, its list being no control
    // of the bar's own (`SelectionBar`).
    expect(document.activeElement).toBe(document.body);
    await user.keyboard("h");
    const list = screen.getByTestId("inbox-list");
    const picker = screen.getByRole("button", { name: "Snooze selected asks" });
    const menu = screen.getByRole("listbox", { name: "Snooze selected asks options" });
    // The bar snoozes only, so its list offers no Un-snooze.
    expect(within(menu).queryByRole("option", { name: "Un-snooze" })).not.toBeInTheDocument();

    await user.keyboard("2");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    // While the write is out the marked rows stay in their band where the reader saw them (as
    // "the bar's Snooze keeps its rows in their band until its write lands" reads), and the list
    // and the trigger are the same nodes: the list stayed, and the trigger is held.
    expect(screen.getByText("Snoozing 2…")).toBeTruthy();
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(screen.getByTestId("inbox-list")).toBe(list);
    expect(screen.getByRole("button", { name: "Snooze selected asks" })).toBe(picker);
    expect(picker).toHaveAttribute("aria-disabled", "true");
    expect(document.activeElement, "the pick gives the focus back to the page").toBe(document.body);

    await act(async () => {
      landing.resolve();
      await Promise.resolve();
    });
    await waitFor(() => expect(screen.queryByText("Snoozing 2…")).not.toBeInTheDocument());
    // The pick has landed: every marked row folds into Later, the bar goes, and the row after
    // the last snoozed one is selected and has the focus (R8), as the pick said.
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    await waitFor(() => expect(shortRow("ask-c")).toHaveFocus());
    expect(selectedIds()).toEqual(["ask-c"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The bar's Snooze at the reading column's width, where the short rows draw no Snooze of their own
// and the column shows a row the reader did not mark: nothing but the bar's own Snooze holds the
// pick's press through its write. The write lands past the pick's own frame and task, as a server
// slower than a frame keeps it, and the bar goes with the rows it snoozed: that going is the pick's
// doing, so the focus goes to the row that moves up into the place of the row marked last, as it
// does in the narrow list, and never to the body. INFRA-2160's R8, merged over this case: that row
// is the item after the last of the snoozed rows, which is selected and takes the keyboard focus
// on the row itself, not on its mark.
test("a bulk Snooze that lands after its pick's frame, at the reading column's width, hands the focus to the row now at the place of the row marked last, never the body", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
    inboxIssue("ask-d", "CORE-4", "Which date?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((each) => (each.id === id ? { ...each, snoozed_until: until } : each));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByTestId("inbox-pane");
    // The third row and then the second: the row marked last is neither the list's first nor the
    // last marked in the list's order. The column stays on the first row, which is not marked.
    await user.click(markOf("ask-c"));
    await user.click(markOf("ask-b"));
    await screen.findByText("2 selected");
    expect(selectedIds()).toEqual(["ask-a"]);
    const picker = screen.getByLabelText("Snooze selected asks");
    await pickIn(user, () => picker, "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    expect(picker).toHaveFocus();
    await afterFrameAndTasks();

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-d"]));
    expect(screen.queryByRole("toolbar", { name: "Selected rows" })).not.toBeInTheDocument();
    await waitFor(() => expect(shortRow("ask-d")).toHaveFocus());
    expect(selectedIds()).toEqual(["ask-d"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

/** How tall `layRowsInBox` draws each row and its mark. */
const BOX_ROW_PX = 100;

/** Lays the Inbox's rows down the list's own box, as happy-dom does not, for the list beside the
 *  reading column: the box, `list`, stands from the window's top to `height` and clips what
 *  overflows it, and each row and its mark are `BOX_ROW_PX` tall, in the order the list draws
 *  them, less the box's `scrollTop`. The box's `scrollTop` starts at 0, and its `scrollBy` moves
 *  it. Every other element keeps happy-dom's own box. Answers the box's scroll, and the function
 *  that gives happy-dom its own boxes back. */
function layRowsInBox(
  list: HTMLElement,
  height: number
): { restore: () => void; top: () => number } {
  let top = 0;
  list.style.overflowY = "auto";
  Object.defineProperty(list, "scrollTop", {
    configurable: true,
    get: () => top,
    set: (value: number) => {
      top = value;
    },
  });
  list.scrollBy = ((options: ScrollToOptions) => {
    top += options.top ?? 0;
  }) as typeof list.scrollBy;
  const real = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this === list) return new DOMRect(0, 0, 600, height);
    const row = this.matches('[data-inbox-row], input[type="checkbox"]')
      ? this.closest("[data-inbox-row]")
      : null;
    if (row === null) return real.call(this);
    const index = [...document.querySelectorAll("[data-inbox-row]")].indexOf(row);
    return new DOMRect(0, index * BOX_ROW_PX - top, 600, BOX_ROW_PX);
  });
  return { restore: () => rect.mockRestore(), top: () => top };
}

// Beside the column, the bar's Dismiss over two threads the reader marked, the upper one last,
// with its row cut by 30 px at the bottom edge of the list's own box, where the bar stands over the
// list from `sm` up. The press selects the row after the marked ones in the commit the bar goes in
// (R8), while the marked rows still stand above it, and the selection's keeping of its row in sight
// scrolled the box to that row there: by the height of the two rows past the cut once they had
// gone, 162 px at 1440x900 (`e2e/table-controls.e2e.ts`, builds Job `infra-2160-r22-l-qs6mx`).
// The bar's hand-off lands that row instead, once the rows have gone and the foot's pass has given
// the list back the room the bar took: the row takes the focus where the row marked last stood,
// and is shown whole by the cut alone (`landNearest`).
test("beside the column the bar's Dismiss with the row marked last cut at the list box's foot gives the row after the marked ones the focus and scrolls the box by that cut alone", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const reads = listThreads(12);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  const { unmount } = renderInbox("/?group=none");
  let laidOut: { restore: () => void; top: () => number } | undefined;
  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(rowIds()).toHaveLength(12));
    // The box ends 700 px down the window.
    const box = layRowsInBox(screen.getByTestId("inbox-list"), 700);
    laidOut = box;
    const order = rowIds();
    const [upper, lower, after] = order.slice(7, 10) as [string, string, string];
    for (const id of [lower, upper]) await user.click(markOf(id));
    await screen.findByText("2 selected");
    // The box scrolled 70 px: the upper row stands at 630 to 730, cut by 30 px at the box's foot.
    act(() => {
      screen.getByTestId("inbox-list").scrollTop = 70;
    });
    const cut = shortRow(upper).getBoundingClientRect().bottom - 700;
    expect(cut).toBe(30);

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() =>
      expect(rowIds()).toEqual(order.filter((id) => id !== upper && id !== lower))
    );
    const landing = shortRow(after);
    await waitFor(() => expect(landing).toHaveFocus());
    // The foot's pass, the landing's own second look a microtask after it (`landNearest`), and
    // the commits after it have all run: nothing moves the box after this.
    await afterFrameAndTasks();
    expect(landing).toHaveFocus();
    expect(selectedIds()).toEqual([after]);
    expect(box.top()).toBe(70 + cut);
    expect(landing.getBoundingClientRect().bottom).toBe(700);
  } finally {
    await act(async () => {
      settle.resolve({ dismissed: 2 });
    });
    unmount();
    laidOut?.restore();
    narrow();
    reads();
  }
});

test("a snooze the column's head writes keeps that column on its row, in flight and refused", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const refusal = Promise.withResolvers<never>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => refusal.promise);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?group=waiting-on");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    // The row stays in the band the reader saw it in while its write is out - which is what
    // keeps the column, and the control that wrote, on the page.
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(shortRow("ask-a")).toHaveAttribute("data-inbox-section", "human");
    expect(within(screen.getByTestId("inbox-pane")).getByText("Snoozing…")).toBeTruthy();

    await act(async () => {
      refusal.reject(
        new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
      );
      await Promise.resolve();
    });

    // The reason and its Retry are in the head that wrote, on the row the reader is still on.
    const head = await waitFor(() => within(screen.getByTestId("inbox-pane")).getByRole("alert"));
    expect(head).toHaveTextContent("snoozed_until must be in the future");
    expect(
      within(screen.getByTestId("inbox-pane")).getByRole("button", { name: "Retry" })
    ).toBeTruthy();
    expect(selectedIds()).toEqual(["ask-a"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The column's head is its question's own: the head's Snooze shows its own question's write, so
// the question the reader moves to is drawn under a head of its own, never under one still saying
// `Snoozing…` of the question before.
test("the question the reader moves to shows no snooze still saving on the one before", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const landing = Promise.withResolvers<never>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => landing.promise);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?group=waiting-on");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    await waitFor(() =>
      expect(within(screen.getByTestId("inbox-pane")).getByText("Snoozing…")).toBeTruthy()
    );

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    const head = within(screen.getByTestId("inbox-pane"));
    expect(head.getByLabelText("Snooze CORE-2: Which format?")).toBeTruthy();
    expect(head.queryByText("Snoozing…")).toBeNull();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The same of a snooze Dispatch refused: its reason and its Retry are the question's it was
// pressed on, so the question the reader moves to is drawn without them.
test("the question the reader moves to shows no refusal of a snooze on the one before", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
  );
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?group=waiting-on");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    await waitFor(() =>
      expect(within(screen.getByTestId("inbox-pane")).getByRole("alert")).toHaveTextContent(
        "snoozed_until must be in the future"
      )
    );

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    const head = within(screen.getByTestId("inbox-pane"));
    expect(head.getByLabelText("Snooze CORE-2: Which format?")).toBeTruthy();
    expect(head.queryByRole("alert")).toBeNull();
    expect(head.queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A snooze the column's head writes is the list's write, not the head's: moving the selection
// takes the head away, and the row stays in the band the reader saw it in while the write is out,
// saying so on the row itself. A refusal leaves the row there, with its reason on the row, though
// the column now shows another question.
test("a snooze from the column's head keeps its row in its band after the selection moves, and a refusal shows its reason on that row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const refusal = Promise.withResolvers<never>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => refusal.promise);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?group=waiting-on");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row is where the reader saw it, though the head that wrote has gone.
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(shortRow("ask-a")).toHaveAttribute("data-inbox-section", "human");
    expect(within(shortRow("ask-a")).getByText("Snoozing…")).toBeTruthy();

    await act(async () => {
      refusal.reject(
        new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
      );
      await refusal.promise.catch(() => undefined);
    });

    await waitFor(() =>
      expect(within(shortRow("ask-a")).getByRole("alert")).toHaveTextContent(
        "snoozed_until must be in the future"
      )
    );
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(shortRow("ask-a")).toHaveAttribute("data-inbox-section", "human");
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The same snooze landing after the selection moved: the row stays in its band while the write
// is out, and moves into Later, which starts folded, once the write lands.
test("a snooze from the column's head moves its row into Later once it lands after the selection moved, not before", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?group=waiting-on");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(shortRow("ask-a")).toHaveAttribute("data-inbox-section", "human");

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });

    await waitFor(() => expect(headings()).toEqual(["Waiting on you (1)", "Later (1)"]));
    expect(rowIds()).toEqual(["ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A hand-on the column's card makes is the list's write, not the card's: moving the selection
// takes the card away, and the row stays where the reader saw it while the write is out, though
// the optimistic `waiting_on_person` takes it out of the reader's own list, with `Handing on…`
// on the row itself. A refusal shows its reason on that row, and in the card drawn for it again,
// with its Retry; a Retry that lands after the reader moved on again lets the row go.
test("a hand-on from the column's card keeps its row while it is out after the selection moves, shows a refusal on the row and in the card drawn again, and lets the row go once it lands", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  // Handed to alice on an issue bob holds, so handing it back to bob takes it out of her list.
  const mine = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    asked_of: "alice",
    issue: { assignee: "bob", key: "CORE-1", title: "Bob's issue" },
  });
  const other = inboxIssue("ask-b", "CORE-2", "Which format?");
  const handedBack: InboxRow = { ...mine, asked_of: null, waiting_on_person: "bob" };
  let served: InboxRow[] = [mine, other];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([mine, other]);
  const getIssue = mockIssueReads();
  const people = mockPeople();
  const refusal = Promise.withResolvers<InboxRow>();
  const landing = Promise.withResolvers<InboxRow>();
  const handAskTo = spyOn(api, "handAskTo")
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(() => landing.promise);
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await handOnTo(user, pane, "bob");
    await waitFor(() => expect(handAskTo).toHaveBeenCalledTimes(1));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row is where the reader saw it, though the card that wrote has gone.
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(within(shortRow("ask-a")).getByText("Handing on…")).toBeTruthy();

    await act(async () => {
      refusal.reject(
        new ApiError(409, {
          code: "ASK_NOT_OPEN",
          error: "only open asks may be handed to another person",
        })
      );
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(shortRow("ask-a")).getByRole("alert")).toHaveTextContent(
        "only open asks may be handed to another person"
      )
    );
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    // The card drawn for the question again shows the refusal, with its Retry.
    await user.click(shortRow("ask-a"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    const refused = within(screen.getByTestId("inbox-pane")).getByRole("alert");
    expect(refused).toHaveTextContent("only open asks may be handed to another person");
    await user.click(within(refused).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(handAskTo).toHaveBeenCalledTimes(2));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);

    served = [handedBack, other];
    await act(async () => {
      landing.resolve(handedBack);
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(handAskTo.mock.calls.map(([askId, askedOf]) => [askId, askedOf])).toEqual([
      ["ask-a", "bob"],
      ["ask-a", "bob"],
    ]);
  } finally {
    unmount();
    narrow();
    handAskTo.mockRestore();
    people.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same for an answer, which takes its row out of the Inbox read the moment it goes: the list
// keeps the row where the reader saw it while the answer is out, saying `Answering…` on it, and a
// refusal puts the row back with its reason on it, and in the card drawn for it again, with its
// Retry. A Retry that lands after the reader moved on again lets the row go.
test("an answer from the column's card keeps its row while it is out after the selection moves, shows a refusal on the row and in the card drawn again, and lets the row go once it lands", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served: InboxRow[] = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<never>();
  const landing = Promise.withResolvers<void>();
  const answerAsk = spyOn(api, "answerAsk")
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(async () => {
      await landing.promise;
      served = [second];
      return {
        ...first,
        answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
        state: "answered",
      };
    });
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(pane).findByRole("radio", { name: "Ship" }));
    await user.click(within(pane).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row is where the reader saw it, though the answer took it out of the Inbox read
    // and the card that wrote has gone.
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(within(shortRow("ask-a")).getByText("Answering…")).toBeTruthy();

    await act(async () => {
      refusal.reject(new ApiError(400, { error: "Dispatch refused this answer." }));
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(shortRow("ask-a")).getByRole("alert")).toHaveTextContent(
        "Could not save your answer."
      )
    );
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    // The card drawn for the question again shows the refusal, with its Retry.
    await user.click(shortRow("ask-a"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    const refused = await within(screen.getByTestId("inbox-pane")).findByRole("alert");
    expect(refused).toHaveTextContent("Could not save your answer.");
    await user.click(within(refused).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(2));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(selectedIds()).toEqual(["ask-b"]);
    expect(answerAsk.mock.calls.map(([askId]) => askId)).toEqual(["ask-a", "ask-a"]);
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same for a decline. Its dialog is modal, so the reader moves the selection while the
// decline is out by going Back, which the dialog does not stop; the dialog goes with the card.
// The list keeps the row where the reader saw it while the decline is out, saying `Declining…`
// on it, and a refusal puts the row back with its reason on it, and in the card drawn for it
// again, under Decline to answer, whose dialog sends the refused note again. One that lands lets
// the row go.
test("a decline from the column's card keeps its row while it is out after the selection moves, shows a refusal on the row and in the card drawn again, and lets the row go once it lands", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served: InboxRow[] = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<never>();
  const declineAsk = spyOn(api, "declineAsk")
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(async () => {
      served = [second];
      return { ...first, state: "resolved" };
    });
  const { unmount } = renderInbox(["/?item=ask-b", "/?item=ask-a"]);

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    await user.click(await within(pane).findByRole("button", { name: "Decline to answer" }));
    const dialog = screen.getByRole("dialog", { name: "Decline to answer" });
    await user.type(within(dialog).getByLabelText("Note (optional)"), "Not mine to say.");
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    await waitFor(() => expect(declineAsk).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole("button", { name: "History back" }));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row is where the reader saw it, though the decline took it out of the Inbox read
    // and the card that wrote has gone.
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(within(shortRow("ask-a")).getByText("Declining…")).toBeTruthy();

    await act(async () => {
      refusal.reject(new ApiError(409, { code: "ASK_CLOSED", error: "This ask is closed." }));
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(shortRow("ask-a")).getByRole("alert")).toHaveTextContent("This ask is closed.")
    );
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    // The card drawn for the question again shows the refusal under Decline to answer, which
    // opens the dialog on the refused decline's note.
    await user.click(shortRow("ask-a"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    const card = screen.getByTestId("inbox-pane");
    expect(within(card).getByRole("alert")).toHaveTextContent("This ask is closed.");
    await user.click(within(card).getByRole("button", { name: "Decline to answer" }));
    const again = screen.getByRole("dialog", { name: "Decline to answer" });
    expect(within(again).getByRole("alert")).toHaveTextContent("This ask is closed.");
    expect(within(again).getByLabelText<HTMLTextAreaElement>("Note (optional)").value).toBe(
      "Not mine to say."
    );
    await user.click(within(again).getByRole("button", { name: "Decline to answer" }));

    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(declineAsk.mock.calls.map(([askId, input]) => [askId, input])).toEqual([
      ["ask-a", { note: "Not mine to say." }],
      ["ask-a", { note: "Not mine to say." }],
    ]);
  } finally {
    unmount();
    narrow();
    declineAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// An Assign to me the column's head makes is the Inbox's write, not the head's: moving the
// selection takes the head away, and under the Unassigned filter the row stays where the reader
// saw it while the save is out, though the optimistic assignee takes it out of the filter, with
// `Assigning…` on the row itself. A refusal ends the hold: the rollback puts the row back in the
// filter, with the reason on the row, and in the head drawn for it again, with its Retry. A Retry
// that lands after the reader moved on again lets the row go.
test("an Assign to me from the column's head keeps its row while it is out after the selection moves, shows a refusal on the row and in the head drawn again, and lets the row go once it lands", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = issueAsk(
    {
      id: "ask-a",
      issue: { assignee: null, key: "CORE-1", title: "Nobody's issue" },
      issue_key: "CORE-1",
      question: "Which approach?",
    },
    null
  );
  const second = issueAsk(
    {
      id: "ask-b",
      issue: { assignee: null, key: "CORE-2", title: "Nobody's other issue" },
      issue_key: "CORE-2",
      question: "Which format?",
    },
    null
  );
  let served: InboxRow[] = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<Issue>();
  const landing = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(() => landing.promise);
  const { unmount } = renderInbox("/?assignee=unassigned");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    await user.click(within(pane).getByRole("button", { name: "Assign CORE-1 to me" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row is where the reader saw it, though its optimistic assignee takes it out of
    // the Unassigned filter and the head that wrote has gone.
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(
      within(shortRow("ask-a")).getByRole("button", { name: "Assign CORE-1 to me" }).textContent
    ).toBe("Assigning…");

    await act(async () => {
      refusal.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed; reopen it first" })
      );
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(shortRow("ask-a")).getByRole("alert")).toHaveTextContent(
        "CORE-1 is closed; reopen it first"
      )
    );
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    // The head drawn for the question again shows the refusal, with its Retry.
    await user.click(shortRow("ask-a"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    const refused = within(screen.getByTestId("inbox-pane")).getByRole("alert");
    expect(refused).toHaveTextContent("CORE-1 is closed; reopen it first");
    await user.click(within(refused).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(
      within(shortRow("ask-a")).getByRole("button", { name: "Assign CORE-1 to me" }).textContent
    ).toBe("Assigning…");

    served = [
      {
        ...first,
        issue: { assignee: "alice", key: "CORE-1", title: "Nobody's issue" },
        waiting_on_person: "alice",
      },
      second,
    ];
    await act(async () => {
      landing.resolve(narrowIssueOf(first, "alice"));
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(patchIssue.mock.calls.map(([key, input]) => [key, input])).toEqual([
      ["CORE-1", { assignee: "alice" }],
      ["CORE-1", { assignee: "alice" }],
    ]);
  } finally {
    unmount();
    narrow();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The comment Dispatch answers a posted question back or reply with: `body` under `askId`, on
 *  `issueKey`, by alice. */
function postedComment(issueKey: string, askId: string, body: string): Comment {
  return {
    anchor: null,
    ask_id: askId,
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-09-11T02:00:00Z",
    edited_at: null,
    id: `${askId}-comment`,
    issue_key: issueKey,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: "agent",
    ...commentDeliveryFields(),
  };
}

// The same for a question back, sent by Ask back instead: it takes nothing out of the Inbox's
// read, but a read that says the asker holds the turn can drop the row while the write is out.
// The row stays where the reader saw it then, saying `Asking back…` on it, and a refusal shows
// its reason on the row, and in the card drawn for it again, with its Retry. A Retry that lands
// lets the row go where the read places it.
test("a question back from the column's card keeps its row while it is out after the selection moves, shows a refusal on the row and in the card drawn again, and lets the row go once it lands", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served: InboxRow[] = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<Comment>();
  const landing = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(() => landing.promise);
  const { queryClient, unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    await user.click(within(pane).getByLabelText("Your answer"));
    await user.paste("Ship what, exactly?");
    await user.click(within(pane).getByRole("button", { name: "Answer" }));
    await user.click(await within(pane).findByRole("button", { name: "Ask back instead" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row says so, though the card that wrote has gone.
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(within(shortRow("ask-a")).getByText("Asking back…")).toBeTruthy();

    await act(async () => {
      refusal.reject(new ApiError(400, { error: "Dispatch refused this comment." }));
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(shortRow("ask-a")).getByRole("alert")).toHaveTextContent(
        "Could not send your clarification."
      )
    );
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    // The card drawn for the question again shows the refusal, with its Retry.
    await user.click(shortRow("ask-a"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    const refused = await within(screen.getByTestId("inbox-pane")).findByRole("alert");
    expect(refused).toHaveTextContent("Could not send your clarification.");
    await user.click(within(refused).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // A read of the Inbox that lists the question no longer - the asker holds its turn - and
    // words the other one afresh, which says the read is drawn: the row stays while the write is
    // out.
    served = [{ ...second, question: "Which format, again?" }];
    await act(async () => {
      await queryClient.invalidateQueries();
    });
    await within(shortRow("ask-b")).findByText("Which format, again?");
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(within(shortRow("ask-a")).getByText("Asking back…")).toBeTruthy();

    await act(async () => {
      landing.resolve(postedComment("CORE-1", "ask-a", "Ship what, exactly?"));
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
    expect(createComment.mock.calls.map(([issueKey, input]) => [issueKey, input])).toEqual([
      ["CORE-1", { ask_id: "ask-a", body: "Ship what, exactly?" }],
      ["CORE-1", { ask_id: "ask-a", body: "Ship what, exactly?" }],
    ]);
  } finally {
    unmount();
    narrow();
    createComment.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same for a reply under a settled ask, from the column of the ask's Reply row. Reply is
// held by `aria-disabled` while it sends, so it keeps the reader's focus. Moving the selection
// takes the column away, and the Reply row stays where the reader saw it while the reply is out,
// though a read of the Inbox's threads drops it then, saying `Replying…` on it. A refusal shows
// its reason on the row, and in the column drawn for the thread again, with its Retry. A Retry
// that lands lets the row go where the read places it.
test("a reply from a thread's column keeps Reply focused while it sends, keeps its row while it is out after the selection moves, shows a refusal on the row and in the column drawn again, and lets the row go once it lands", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const open = inboxIssue("ask-b", "CORE-2", "Which format?");
  const settled = issueAsk({
    answer: { at: "2026-09-11T03:00:00Z", selected: [], text: "Ship it.", user: "bob" },
    id: "ask-done",
    issue: { assignee: "bob", key: "CORE-3", title: "Bob's issue" },
    issue_key: "CORE-3",
    question: "Which approach?",
    state: "answered",
  });
  const thread = replyRow("ask-done", {
    issue: { key: "CORE-3", title: "Bob's issue" },
    kind: "ask",
    ref: "/issues/CORE-3/asks/ask-done",
    subject: "Which approach?",
  });
  const threads = (rows: MyReplyRow[]) => ({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows,
    total: rows.length,
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([open]);
  const getAsk = mockAskReads([open, settled]);
  const getIssue = mockIssueReads();
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue(threads([thread]));
  const refusal = Promise.withResolvers<Comment>();
  const landing = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(() => landing.promise);
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(rowIds()).toContain("reply:ask-done"));
    await user.click(shortRow("reply:ask-done"));
    await waitFor(() => expect(selectedIds()).toEqual(["reply:ask-done"]));
    const pane = screen.getByTestId("inbox-pane");
    await user.click(await within(pane).findByRole("textbox", { name: "Reply" }));
    await user.paste("Thanks, shipping it.");
    const send = within(pane).getByRole("button", { name: "Reply" });
    await user.click(send);
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    // Held while it sends, and still the reader's focus.
    await waitFor(() => expect(send.textContent).toBe("Replying…"));
    expect(send).toHaveAttribute("aria-disabled", "true");
    expect(send).not.toBeDisabled();
    expect(send).toHaveFocus();

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // Out, the row says so, though the column that wrote has gone.
    expect(within(shortRow("reply:ask-done")).getByText("Replying…")).toBeTruthy();

    await act(async () => {
      refusal.reject(new ApiError(400, { error: "Dispatch refused this comment." }));
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(within(shortRow("reply:ask-done")).getByRole("alert")).toHaveTextContent(
        "Could not post your reply."
      )
    );
    expect(selectedIds()).toEqual(["ask-b"]);

    // The column drawn for the thread again shows the refusal, with its Retry.
    await user.click(shortRow("reply:ask-done"));
    await waitFor(() => expect(selectedIds()).toEqual(["reply:ask-done"]));
    const refused = await within(screen.getByTestId("inbox-pane")).findByRole("alert");
    expect(refused).toHaveTextContent("Could not post your reply.");
    await user.click(within(refused).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));

    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    // A read of the Inbox's threads that lists this one no longer, and lists another, which says
    // the read is drawn: the row stays while the reply is out.
    listMyReplies.mockResolvedValue(threads([replyRow("c-fresh", { subject: "Fresh thread" })]));
    await act(async () => {
      await queryClient.invalidateQueries();
    });
    await waitFor(() => expect(rowIds()).toContain("reply:c-fresh"));
    expect(rowIds()).toContain("reply:ask-done");
    expect(within(shortRow("reply:ask-done")).getByText("Replying…")).toBeTruthy();

    await act(async () => {
      landing.resolve(postedComment("CORE-3", "ask-done", "Thanks, shipping it."));
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).not.toContain("reply:ask-done"));
    expect(createComment.mock.calls.map(([issueKey, input]) => [issueKey, input])).toEqual([
      ["CORE-3", { ask_id: "ask-done", body: "Thanks, shipping it." }],
      ["CORE-3", { ask_id: "ask-done", body: "Thanks, shipping it." }],
    ]);
  } finally {
    unmount();
    narrow();
    createComment.mockRestore();
    listMyReplies.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a row the reader defers hands the selection and the focus to the next one, and the last leaves no ?item=", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(address()).toBe("/?item=ask-a");
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );

    // A row the reader defers is a row they have dealt with: the next one takes its place and
    // the address follows, as it does for a row they answer, and its row has the keyboard focus
    // (R8), where the menu had handed it back to the Snooze that wrote.
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    await waitFor(() => expect(shortRow("ask-b")).toHaveFocus());

    await pickIn(
      user,
      () => within(screen.getByTestId("inbox-pane")).getByLabelText("Snooze CORE-2: Which format?"),
      "Tomorrow"
    );

    // With the last row deferred the address names none at all: a reload reads it, and one
    // naming a deferred row would open the folded band in front of the reader again.
    await waitFor(() => expect(address()).toBe("/"));
    expect(selectedIds()).toEqual([]);
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    expect(headings()).toEqual(["Later (2)"]);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// ----------------------------------------------------------------------------------------
// R8: after the reader acts on the item in hand, the item after it is selected and its row has
// the keyboard focus - with the Later band open, below the column's width, and, where no item is
// left, the list's empty message.
// ----------------------------------------------------------------------------------------

/** The answer the server records for `row`: its first option, picked by alice. */
function answeredWith(row: InboxRow, option: string): InboxRow {
  return {
    ...row,
    answer: { at: "2026-09-11T03:00:00Z", selected: [option], text: "", user: "alice" },
    state: "answered",
  };
}

test("with the Later band open, a snooze of the selected row selects and focuses the row after it, not the snoozed row in Later", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
    inboxIssue("ask-z", "CORE-9", "Which week?", { snoozed_until: soon() }),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?item=ask-b");

  try {
    await screen.findByTestId("inbox-pane");
    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c", "ask-z"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    await user.keyboard("h");
    await user.keyboard("2");
    expect(snoozeAsk).toHaveBeenCalledTimes(1);

    // The snoozed row is still drawn, in the open band, and is no longer the one in hand: the
    // row that came after it is, and its row has the focus.
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    expect(shortRow("ask-b")).toHaveAttribute("data-inbox-section", "later");
    expect(address()).toBe("/?item=ask-c");
    await waitFor(() => expect(shortRow("ask-c")).toHaveFocus());
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("in the drawer a snooze gives the next row the focus, and an answer does once the server records it", async () => {
  const user = userEvent.setup();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const rows = [
    first,
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const recorded = Promise.withResolvers<void>();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    await recorded.promise;
    served = served.filter((row) => row.id !== "ask-a");
    return answeredWith(first, "Ship");
  });
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByTestId("ask-ask-b");
    act(() => rowOf("ask-b").focus());
    await user.keyboard("b");
    await user.keyboard("2");
    // No selection here: the row after the snoozed one is the row the focus goes to.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-c"]));
    await waitFor(() => expect(rowOf("ask-c")).toHaveFocus());
    expect(selectedIds()).toEqual([]);

    // An answer moves nothing until the server records it: the row stays under the reader's
    // hand, which is where a refusal would show.
    await user.click(within(rowOf("ask-a")).getByRole("radio", { name: "Ship" }));
    await user.click(within(rowOf("ask-a")).getByRole("button", { name: "Answer" }));
    expect(rowOf("ask-a").contains(document.activeElement)).toBe(true);
    await act(async () => {
      recorded.resolve();
      await Promise.resolve();
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    await waitFor(() => expect(rowOf("ask-c")).toHaveFocus());
  } finally {
    unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// WebKit scrolls a focused element into view only a frame later, by when the list's own scroll
// keeping may have moved the page again, so the hand-off brings the row it focuses into view
// itself where the reader cannot see the whole of it, at its nearest edge by the least scroll
// (`landNearest`): at a phone's width the row after a snoozed one is in the reader's sight the
// moment it has the focus. The rows are laid out below the window here, so the window's scroll
// brings that row's bottom edge to the window's own; a row the reader already sees whole scrolls
// nothing (the bar's hand-off rows, "a bulk Snooze deep in a long list ...").
test("the row a hand-off gives the focus to is scrolled into view once it holds the focus", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads(rows);
  /** Each scroll of the window: the row that held the focus as it was asked, and where the scroll
   *  leaves that row's bottom edge. */
  const scrolled: { bottom: number; focused: string | null }[] = [];
  const scrolls = spyOn(window, "scrollBy").mockImplementation(
    (x?: number | ScrollToOptions, y?: number) => {
      const by = typeof x === "number" ? (y ?? 0) : (x?.top ?? 0);
      const focused = document.activeElement;
      scrolled.push({
        bottom: (focused?.getBoundingClientRect().bottom ?? Number.NaN) - by,
        focused: focused?.getAttribute("data-inbox-row") ?? null,
      });
    }
  );
  // Every row a thousand pixels and more down the page, out of the reader's sight.
  const laidOut = layRowsOut("[data-inbox-row]", -10);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByTestId("ask-ask-b");
    act(() => rowOf("ask-b").focus());
    await user.keyboard("b");
    await user.keyboard("2");
    await waitFor(() => expect(rowOf("ask-c")).toHaveFocus());
    expect(scrolled).toContainEqual({ bottom: window.innerHeight, focused: "ask-c" });
  } finally {
    unmount();
    laidOut();
    scrolls.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// A press's own callbacks run only while the control that made it is mounted (TanStack's
// `mutate` callbacks), so a snooze whose Snooze goes before the server answers is one whose
// landing the Inbox never hears. The hand-off it began goes with the control: kept, it would give
// the row after the snoozed one the focus at every later commit that finds the focus on the page.
test("a snooze whose row the search hides before it lands leaves no hand-off to pull the focus later", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  const arriving = inboxIssue("ask-d", "CORE-4", "Which week?");
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const landing = Promise.withResolvers<void>();
  let landed = false;
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    landed = true;
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads([...rows, arriving]);
  const { queryClient, unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByTestId("ask-ask-a");
    act(() => rowOf("ask-a").focus());
    await user.keyboard("b");
    await user.keyboard("2");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));

    // The search hides the snoozed row while its write is out, and the row's Snooze goes with it;
    // the write lands after that.
    await user.keyboard("f");
    const box = screen.getByRole("searchbox", { name: "Search asks" });
    expect(box).toHaveFocus();
    await user.keyboard("owner");
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(landed).toBe(true));
    await user.clear(box);
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));

    // With the focus on the page, the next commit - an ask arriving - leaves it there.
    act(() => box.blur());
    expect(document.activeElement).toBe(document.body);
    served = [...served, arriving];
    await act(() => queryClient.invalidateQueries());
    await screen.findByTestId("ask-ask-d");
    expect(document.activeElement).toBe(document.body);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("an answer to the only item a search leaves gives the empty message the focus, where the Inbox's keys still work", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const only = inboxIssue("ask-a", "CORE-1", "Which zephyr approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const rows = [only, inboxIssue("ask-b", "CORE-2", "Which format?")];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = served.filter((row) => row.id !== "ask-a");
    return answeredWith(only, "Ship");
  });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?q=zephyr");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(rowIds()).toEqual(["ask-a"]);
    await user.click(within(pane).getByRole("radio", { name: "Ship" }));
    await user.click(within(pane).getByRole("button", { name: "Answer" }));

    const empty = await screen.findByRole("region", { name: "Inbox empty state" });
    expect(empty).toHaveTextContent("Nothing matches this text");
    await waitFor(() => expect(empty).toHaveFocus());
    // A message is no text field, so the Inbox's own keys reach the page from it.
    await user.keyboard("f");
    expect(screen.getByRole("searchbox", { name: "Search asks" })).toHaveFocus();
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The column keeps an answered question past its row until the server decides the answer. Where
// it was the only item in the Inbox, nothing is left once the server records it: the question
// goes with the selection, and the address names no row, even though the list then holds no row
// for the selection's settling to read.
test("an answer to the only item in the Inbox leaves the column showing nothing and the empty message focused", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const only = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  let served: InboxRow[] = [only];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = [];
    return answeredWith(only, "Ship");
  });
  const getAsk = mockAskReads([only]);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    const pane = await screen.findByTestId("inbox-pane");
    expect(address()).toBe("/?item=ask-a");
    await user.click(within(pane).getByRole("radio", { name: "Ship" }));
    await user.click(within(pane).getByRole("button", { name: "Answer" }));

    const empty = await screen.findByRole("region", { name: "Inbox empty state" });
    await waitFor(() => expect(empty).toHaveFocus());
    await waitFor(() => expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument());
    expect(screen.queryByTestId("ask-ask-a")).not.toBeInTheDocument();
    expect(address()).toBe("/");
    expect(answerAsk).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    narrow();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// A dismiss takes its row off the list at once, so the row the column showed goes at the next
// commit. Where it was the last row, the list then holds no row, and the address must not keep
// the dismissed row's name: a reload or a link reads it.
test("a dismiss of the last row beside the column leaves the address naming no row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  let threads = [replyRow("thread-1")];
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async () => ({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: threads,
    total: threads.length,
  }));
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const getIssue = mockIssueReads();
  const dismissed = answerInboxDismisses().mockImplementation(async () => {
    threads = [];
    return { dismissed: 1 };
  });
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["reply:thread-1"]));
    expect(address()).toBe("/?item=reply%3Athread-1");

    await user.keyboard("e");

    const empty = await screen.findByRole("region", { name: "Inbox empty state" });
    await waitFor(() => expect(empty).toHaveFocus());
    expect(dismissed).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    await waitFor(() => expect(address()).toBe("/"));
  } finally {
    unmount();
    narrow();
    dismissed.mockRestore();
    listComments.mockRestore();
    listMyReplies.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// `e` dismisses once per press, as every action that does not step does (`KeyBinding.repeats`). A
// dismiss hands the reader on to the next row at once (R8), so each repeat of a held `e` would
// dismiss a row nobody chose.
test("a held e dismisses the row in hand alone, and its repeats dismiss none of the rows it hands the reader on to", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  // Newest first, A to C: each thread's reply an hour older than the one before.
  let threads = ["a", "b", "c"].map((name, index) => {
    const row = replyRow(`thread-${name}`, { subject: `Thread ${name.toUpperCase()}` });
    return { ...row, reply: { ...row.reply, created_at: `2026-10-12T0${3 - index}:00:00Z` } };
  });
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async () => ({
    as_of: "2026-10-12T05:00:00Z",
    limit: 200,
    offset: 0,
    rows: threads,
    total: threads.length,
  }));
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const getIssue = mockIssueReads();
  const dismissed = answerInboxDismisses().mockImplementation(async (body) => {
    const named = body.threads ?? [];
    threads = threads.filter((thread) => !named.includes(`reply:${thread.thread_id}`));
    return { dismissed: named.length };
  });
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() =>
      expect(rowIds()).toEqual(["reply:thread-a", "reply:thread-b", "reply:thread-c"])
    );
    await waitFor(() => expect(selectedIds()).toEqual(["reply:thread-a"]));

    await user.keyboard("e");
    await waitFor(() => expect(rowIds()).toEqual(["reply:thread-b", "reply:thread-c"]));
    await waitFor(() => expect(selectedIds()).toEqual(["reply:thread-b"]));
    // The key is still down: its repeats come with the reader on the next row. user-event sends a
    // held key's repeats only in one burst with its first keydown, never after a wait.
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "e", repeat: true });
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "e", repeat: true });
    await act(settle);

    expect(dismissed).toHaveBeenCalledTimes(1);
    expect(dismissed.mock.calls[0]?.[0].threads).toEqual(["reply:thread-a"]);
    expect(rowIds()).toEqual(["reply:thread-b", "reply:thread-c"]);
    expect(selectedIds()).toEqual(["reply:thread-b"]);
  } finally {
    unmount();
    narrow();
    dismissed.mockRestore();
    listComments.mockRestore();
    listMyReplies.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// Beside the column only the selected row is the item in hand (R8). The Dismiss the pointer
// reaches on another row takes that row out and leaves the reader on the item they are reading:
// the selection, the address and the focus stay where they were.
test("beside the column a Dismiss on a row that is not selected leaves the selection and ?item= on the selected row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  // Newest first, A to D: each thread's reply an hour older than the one before.
  let threads = ["a", "b", "c", "d"].map((name, index) => {
    const row = replyRow(`thread-${name}`, { subject: `Thread ${name.toUpperCase()}` });
    return { ...row, reply: { ...row.reply, created_at: `2026-10-12T0${4 - index}:00:00Z` } };
  });
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async () => ({
    as_of: "2026-10-12T05:00:00Z",
    limit: 200,
    offset: 0,
    rows: threads,
    total: threads.length,
  }));
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const getIssue = mockIssueReads();
  const dismissed = answerInboxDismisses().mockImplementation(async () => {
    threads = threads.filter((thread) => thread.thread_id !== "thread-c");
    return { dismissed: 1 };
  });
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() =>
      expect(rowIds()).toEqual([
        "reply:thread-a",
        "reply:thread-b",
        "reply:thread-c",
        "reply:thread-d",
      ])
    );
    await waitFor(() => expect(selectedIds()).toEqual(["reply:thread-a"]));
    expect(address()).toBe("/?item=reply%3Athread-a");

    await user.click(
      within(shortRow("reply:thread-c")).getByRole("button", { name: "Dismiss: Thread C" })
    );

    await waitFor(() =>
      expect(rowIds()).toEqual(["reply:thread-a", "reply:thread-b", "reply:thread-d"])
    );
    expect(dismissed).toHaveBeenCalledTimes(1);
    expect(selectedIds()).toEqual(["reply:thread-a"]);
    expect(address()).toBe("/?item=reply%3Athread-a");
    expect(shortRow("reply:thread-d")).not.toHaveFocus();
  } finally {
    unmount();
    narrow();
    dismissed.mockRestore();
    listComments.mockRestore();
    listMyReplies.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same Dismiss with the focus on it, as the pointer's press leaves it, in the shell's main
// content, as the app shell draws it. The focus would go with the row it is in and fall onto the
// page: it goes to the row of the item being read, and to no control of another row.
test("beside the column a Dismiss holding the focus on a row that is not selected hands the focus to the selected row, and to no control of another row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  // Newest first, A to D: each thread's reply an hour older than the one before.
  let threads = ["a", "b", "c", "d"].map((name, index) => {
    const row = replyRow(`thread-${name}`, { subject: `Thread ${name.toUpperCase()}` });
    return { ...row, reply: { ...row.reply, created_at: `2026-10-12T0${4 - index}:00:00Z` } };
  });
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async () => ({
    as_of: "2026-10-12T05:00:00Z",
    limit: 200,
    offset: 0,
    rows: threads,
    total: threads.length,
  }));
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const getIssue = mockIssueReads();
  const dismissed = answerInboxDismisses().mockImplementation(async () => {
    threads = threads.filter((thread) => thread.thread_id !== "thread-c");
    return { dismissed: 1 };
  });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RouteProbe />
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() =>
      expect(rowIds()).toEqual([
        "reply:thread-a",
        "reply:thread-b",
        "reply:thread-c",
        "reply:thread-d",
      ])
    );
    await waitFor(() => expect(selectedIds()).toEqual(["reply:thread-a"]));

    const dismiss = within(shortRow("reply:thread-c")).getByRole("button", {
      name: "Dismiss: Thread C",
    });
    act(() => dismiss.focus());
    expect(dismiss).toHaveFocus();
    await user.click(dismiss);
    await waitFor(() =>
      expect(rowIds()).toEqual(["reply:thread-a", "reply:thread-b", "reply:thread-d"])
    );
    await afterFrameAndTasks();

    expect(dismissed).toHaveBeenCalledTimes(1);
    expect(selectedIds()).toEqual(["reply:thread-a"]);
    expect(address()).toBe("/?item=reply%3Athread-a");
    expect(shortRow("reply:thread-a")).toHaveFocus();
    for (const id of ["reply:thread-b", "reply:thread-d"]) {
      expect(shortRow(id).contains(document.activeElement)).toBe(false);
    }
  } finally {
    view.unmount();
    narrow();
    dismissed.mockRestore();
    listComments.mockRestore();
    listMyReplies.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same Dismiss with the focus on it, while the column keeps a question the list draws no row
// for: the reader's unsent answer keeps it there once a search hides its row (`paneRow`). There
// is no selected row to hand the focus to, so the column's scroller takes it - the stop `l` puts
// the reader on, which is the item being read - and the hidden question stays selected.
test("beside the column a Dismiss holding the focus on another row, while a search hides the selected question, hands the focus to the column's scroller and to no control of a row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const ask = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask]);
  const getAsk = mockAskReads([ask]);
  // Newest first, B to D: each thread's reply an hour older than the one before. Each subject
  // holds "thread", and the question's words do not.
  let threads = ["b", "c", "d"].map((name, index) => {
    const row = replyRow(`thread-${name}`, { subject: `Thread ${name.toUpperCase()}` });
    return { ...row, reply: { ...row.reply, created_at: `2026-10-12T0${3 - index}:00:00Z` } };
  });
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async () => ({
    as_of: "2026-10-12T05:00:00Z",
    limit: 200,
    offset: 0,
    rows: threads,
    total: threads.length,
  }));
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const getIssue = mockIssueReads();
  const dismissed = answerInboxDismisses().mockImplementation(async () => {
    threads = threads.filter((thread) => thread.thread_id !== "thread-c");
    return { dismissed: 1 };
  });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/?item=ask-a"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RouteProbe />
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(rowIds()).toHaveLength(4));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));

    // Typed and unsent, so the column keeps ask-a once the search hides its row.
    const draft = within(screen.getByTestId("inbox-pane")).getByLabelText("Your answer");
    await user.clear(draft);
    await user.click(draft);
    await user.paste("Whichever the mirror already holds.");
    await user.click(screen.getByRole("searchbox", { name: "Search asks" }));
    await user.keyboard("thread");
    await waitFor(() =>
      expect(rowIds()).toEqual(["reply:thread-b", "reply:thread-c", "reply:thread-d"])
    );
    expect(address()).toBe("/?item=ask-a&q=thread");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-a")).toBeTruthy();

    const dismiss = within(shortRow("reply:thread-c")).getByRole("button", {
      name: "Dismiss: Thread C",
    });
    act(() => dismiss.focus());
    expect(dismiss).toHaveFocus();
    await user.click(dismiss);
    await waitFor(() => expect(rowIds()).toEqual(["reply:thread-b", "reply:thread-d"]));
    await afterFrameAndTasks();

    expect(dismissed).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();
    expect(document.activeElement?.closest("[data-inbox-row]") ?? null).toBeNull();
    expect(address()).toBe("/?item=ask-a&q=thread");
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-a")).toBeTruthy();
  } finally {
    view.unmount();
    narrow();
    dismissed.mockRestore();
    listComments.mockRestore();
    listMyReplies.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** The Inbox in the shell's main content, as the app shell draws it, at `route`, its address read
 *  by `RouteProbe`. */
function renderInboxInMain(route: string) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RouteProbe />
          <main>
            <Inbox />
          </main>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// k-next-item, for the Snooze a short row draws beside the column while that row's snooze is
// refused and another question is in hand. R8 hands the reader on from the item in hand alone, so
// a Retry there hands nobody on: the selection and the address stay on the item being read. The
// press left the focus in the row the landing folds into Later; it goes first, with no scroll, to
// the selected row, as a Dismiss there sends it (`dismissFromRow`), and not to a control of the
// row that moves up into the snoozed one's place.
test("beside the column a Snooze Retry on a row that is not selected hands nobody on, and the focus it held goes to the selected row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const refusal = Promise.withResolvers<never>();
  const snoozeAsk = answerAskSnoozes()
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(async (id, { snoozed_until: until }) => {
      served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
      return { snoozed_until: until };
    });
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const moves = watchMoves();
  const view = renderInboxInMain("/");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    await pickIn(
      user,
      () => within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    await user.click(shortRow("ask-c"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    await act(async () => {
      refusal.reject(
        new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until must be in the future" })
      );
      await refusal.promise.catch(() => undefined);
    });

    const retry = await waitFor(() =>
      within(shortRow("ask-a")).getByRole("button", { name: "Retry" })
    );
    await user.click(retry);
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));
    await afterFrameAndTasks();

    expect(selectedIds()).toEqual(["ask-c"]);
    expect(address()).toBe("/?item=ask-c");
    expect(shortRow("ask-c")).toHaveFocus();
    expect(moves.focused.at(-1)?.target).toBe(shortRow("ask-c"));
    expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
    expect(shortRow("ask-b").contains(document.activeElement)).toBe(false);
  } finally {
    view.unmount();
    moves.restore();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// The same rule for an un-snooze from a short row beside the column while another question is in
// hand: the row draws Un-snooze, the list's Un-snooze and Retry once the un-snooze the reader
// pressed in the column's head is refused, and a landing moves the row out of Later. A press of
// any of them hands nobody on, so the selection and the address stay on the item being read, and
// the focus the press left in the row goes first, with no scroll, to the selected row. The landing
// draws the un-snoozed row above the others, and React moves those rows to do it: it focuses the
// moved selected row again, with no options, and puts back the scroll of the boxes around it.
for (const { name, press } of [
  {
    name: "Un-snooze",
    press: (user: UserEvent, row: HTMLElement) =>
      user.click(within(row).getByRole("button", { name: "Un-snooze CORE-1: Which approach?" })),
  },
  {
    name: "the list's Un-snooze",
    press: (user: UserEvent, row: HTMLElement) =>
      pickIn(user, () => within(row).getByLabelText("Snooze CORE-1: Which approach?"), "Un-snooze"),
  },
  {
    name: "an Un-snooze Retry",
    press: (user: UserEvent, row: HTMLElement) =>
      user.click(within(row).getByRole("button", { name: "Retry" })),
  },
] satisfies readonly {
  name: string;
  /** The press on the short row `row`, with the focus left in it. */
  press: (user: UserEvent, row: HTMLElement) => Promise<void>;
}[]) {
  test(`beside the column ${name} on a row that is not selected hands nobody on, and the focus it held goes to the selected row`, async () => {
    const user = userEvent.setup();
    const narrow = widenToReadingPane();
    const rows = [
      inboxIssue("ask-a", "CORE-1", "Which approach?", { snoozed_until: soon() }),
      inboxIssue("ask-b", "CORE-2", "Which format?"),
      inboxIssue("ask-c", "CORE-3", "Which owner?"),
    ];
    let served: InboxRow[] = rows;
    const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
    const refusal = Promise.withResolvers<never>();
    const unsnoozeAsk = answerAskUnsnoozes()
      .mockImplementationOnce(() => refusal.promise)
      .mockImplementationOnce(async (id) => {
        served = served.map((row) => (row.id === id ? { ...row, snoozed_until: null } : row));
      });
    const getAsk = mockAskReads(rows);
    const getIssue = mockIssueReads();
    const moves = watchMoves();
    const view = renderInboxInMain("/");

    try {
      const pane = await screen.findByTestId("inbox-pane");
      await user.click(await screen.findByRole("button", { name: /^Later \(1\)/ }));
      await user.click(await waitFor(() => shortRow("ask-a")));
      await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
      await user.click(within(pane).getByRole("button", { name: /^Un-snooze / }));
      await waitFor(() => expect(unsnoozeAsk).toHaveBeenCalledTimes(1));
      await user.click(shortRow("ask-c"));
      await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
      await act(async () => {
        refusal.reject(new ApiError(409, { code: "ASK_CLOSED", error: "the ask is closed" }));
        await refusal.promise.catch(() => undefined);
      });
      await waitFor(() => within(shortRow("ask-a")).getByRole("button", { name: "Retry" }));
      const movesBefore = moves.focused.length;
      const scrollsBefore = moves.scrolled.length;

      await press(user, shortRow("ask-a"));
      await waitFor(() => expect(unsnoozeAsk).toHaveBeenCalledTimes(2));
      await waitFor(() =>
        expect(shortRow("ask-a")).not.toHaveAttribute("data-inbox-section", "later")
      );
      await afterFrameAndTasks();

      expect(selectedIds()).toEqual(["ask-c"]);
      expect(address()).toBe("/?item=ask-c");
      expect(shortRow("ask-c")).toHaveFocus();
      const toSelected = moves.focused
        .slice(movesBefore)
        .filter((move) => move.target === shortRow("ask-c"));
      expect(toSelected[0]?.options, "the press's own move of the focus").toEqual({
        preventScroll: true,
      });
      expect(
        moves.scrolled
          .slice(scrollsBefore)
          .filter((element) => element.closest("[data-inbox-row]") !== null)
      ).toEqual([]);
    } finally {
      view.unmount();
      moves.restore();
      narrow();
      getAsk.mockRestore();
      getInbox.mockRestore();
      getIssue.mockRestore();
      unsnoozeAsk.mockRestore();
    }
  });
}

// The same rule for the hand-on a short row draws while that row's hand-on is refused: handing the
// question back to bob takes it out of alice's list once it lands. A Retry there moves no
// selection, and the focus the press left in the row goes first to the selected row, not to a
// control of the row that moves up into the handed-on row's place.
test("beside the column a hand-on Retry on a row that is not selected leaves the selection, and the focus it held goes to the selected row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  // Handed to alice on an issue bob holds, so handing it back to bob takes it out of her list.
  const mine = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    asked_of: "alice",
    issue: { assignee: "bob", key: "CORE-1", title: "Bob's issue" },
  });
  const rows = [
    mine,
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    inboxIssue("ask-c", "CORE-3", "Which owner?"),
  ];
  const handedBack: InboxRow = { ...mine, asked_of: null, waiting_on_person: "bob" };
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const people = mockPeople();
  const refusal = Promise.withResolvers<InboxRow>();
  const handAskTo = spyOn(api, "handAskTo")
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(async () => {
      served = [handedBack, ...rows.slice(1)];
      return handedBack;
    });
  const view = renderInboxInMain("/");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    await handOnTo(user, pane, "bob");
    await waitFor(() => expect(handAskTo).toHaveBeenCalledTimes(1));
    await user.click(shortRow("ask-c"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    await act(async () => {
      refusal.reject(
        new ApiError(409, {
          code: "ASK_NOT_OPEN",
          error: "only open asks may be handed to another person",
        })
      );
      await refusal.promise.catch(() => undefined);
    });

    const retry = await waitFor(() =>
      within(shortRow("ask-a")).getByRole("button", { name: "Retry" })
    );
    await user.click(retry);
    await waitFor(() => expect(handAskTo).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));
    await afterFrameAndTasks();

    expect(selectedIds()).toEqual(["ask-c"]);
    expect(address()).toBe("/?item=ask-c");
    expect(shortRow("ask-c")).toHaveFocus();
    expect(shortRow("ask-b").contains(document.activeElement)).toBe(false);
  } finally {
    view.unmount();
    narrow();
    handAskTo.mockRestore();
    people.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// The same rule for the Assign to me a short row draws while that row's Assign to me is refused,
// under the Unassigned filter, which an assignee takes the row out of once it lands. A Retry there
// moves no selection, and the focus the press left in the row goes first to the selected row, not
// to a control of the row that moves up into the assigned row's place.
test("beside the column an Assign to me Retry on a row that is not selected leaves the selection, and the focus it held goes to the selected row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const unheld = (id: string, key: string, question: string) =>
    issueAsk(
      { id, issue: { assignee: null, key, title: "Nobody's issue" }, issue_key: key, question },
      null
    );
  const rows = [
    unheld("ask-a", "CORE-1", "Which approach?"),
    unheld("ask-b", "CORE-2", "Which format?"),
    unheld("ask-c", "CORE-3", "Which owner?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const refusal = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => refusal.promise)
    .mockImplementationOnce(async () => {
      const [first, ...rest] = rows as [InboxRow, ...InboxRow[]];
      served = [
        {
          ...first,
          issue: { assignee: "alice", key: "CORE-1", title: "Nobody's issue" },
          waiting_on_person: "alice",
        },
        ...rest,
      ];
      return narrowIssueOf(first, "alice");
    });
  const view = renderInboxInMain("/?assignee=unassigned");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-a"]));
    await user.click(within(pane).getByRole("button", { name: "Assign CORE-1 to me" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await user.click(shortRow("ask-c"));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    await act(async () => {
      refusal.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed; reopen it first" })
      );
      await refusal.promise.catch(() => undefined);
    });

    const retry = await waitFor(() =>
      within(shortRow("ask-a")).getByRole("button", { name: "Retry" })
    );
    await user.click(retry);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowIds()).toEqual(["ask-b", "ask-c"]));
    await afterFrameAndTasks();

    expect(selectedIds()).toEqual(["ask-c"]);
    expect(address()).toBe("/?item=ask-c&assignee=unassigned");
    expect(shortRow("ask-c")).toHaveFocus();
    expect(shortRow("ask-b").contains(document.activeElement)).toBe(false);
  } finally {
    view.unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

// A search that leaves no row hides the item being read; it does not take it away. The selection
// and the address keep it while nothing is listed, so the reader who clears the text is back on
// that item rather than on the first row.
test("a search that leaves no row keeps the selection and ?item=, so clearing it is back on the item being read", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?item=ask-b");

  try {
    await screen.findByTestId("inbox-pane");
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(selectedIds()).toEqual(["ask-b"]);

    // No row holds a "z", so the first letter already leaves the list empty.
    await user.keyboard("f");
    const box = screen.getByRole("searchbox", { name: "Search asks" });
    expect(box).toHaveFocus();
    await user.keyboard("zephyr");
    const empty = await screen.findByRole("region", { name: "Inbox empty state" });
    expect(empty).toHaveTextContent("Nothing matches this text");
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    expect(address()).toBe("/?item=ask-b&q=zephyr");

    await user.clear(box);
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    expect(selectedIds()).toEqual(["ask-b"]);
    expect(address()).toBe("/?item=ask-b");
    expect(within(await screen.findByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("an empty list with no message to say yet takes the focus itself", async () => {
  const user = userEvent.setup();
  // The credential requests have not answered, so the Inbox cannot say that nothing needs the
  // reader and draws no message.
  mocks.getCredentialPending.mockImplementation(() => new Promise(() => {}));
  const only = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  let served: InboxRow[] = [only];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = [];
    return answeredWith(only, "Ship");
  });
  const getAsk = mockAskReads([only]);
  const { unmount } = renderInbox("/", { pane: false });

  try {
    await screen.findByTestId("ask-ask-a");
    await user.click(within(rowOf("ask-a")).getByRole("radio", { name: "Ship" }));
    await user.click(within(rowOf("ask-a")).getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(rowIds()).toEqual([]));
    const list = screen.getByTestId("inbox-list");
    expect(list).toHaveAttribute("data-inbox-empty");
    expect(screen.queryByRole("region", { name: "Inbox empty state" })).not.toBeInTheDocument();
    await waitFor(() => expect(list).toHaveFocus());
  } finally {
    unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// R8 with the Later band folded: a snooze of the only row the list shows folds it into the band,
// so the list draws no row, though it still holds one - the snoozed question, counted in "Later
// (1)" - and its empty message, which says the list holds nothing, is not drawn. The list itself
// takes the focus, as it does where it has no message to say (above), beside the column and below
// its width alike, and the Inbox's keys work from there.
for (const { name, wide } of [
  { name: "in the drawer", wide: false },
  { name: "beside the column", wide: true },
]) {
  test(`${name} a snooze of the only row shown, with the Later band folded, gives the list itself the focus, where the Inbox's keys still work`, async () => {
    const user = userEvent.setup();
    const narrow = wide ? widenToReadingPane() : () => undefined;
    const only = inboxIssue("ask-a", "CORE-1", "Which approach?");
    let served: InboxRow[] = [only];
    const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
    const snoozeAsk = answerAskSnoozes().mockImplementation(
      async (id, { snoozed_until: until }) => {
        served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
        return { snoozed_until: until };
      }
    );
    const getAsk = mockAskReads([only]);
    const getIssue = mockIssueReads();
    const { unmount } = renderInbox("/", { pane: wide });

    try {
      await screen.findByTestId(wide ? "inbox-pane" : "ask-ask-a");
      act(() => shortRow("ask-a").focus());
      await user.keyboard("b");
      await user.keyboard("2");
      await waitFor(() => expect(rowIds()).toEqual([]));
      expect(snoozeAsk).toHaveBeenCalledTimes(1);
      expect(screen.getByRole("button", { name: /^Later \(1\)/ })).toBeTruthy();
      expect(screen.queryByRole("region", { name: "Inbox empty state" })).not.toBeInTheDocument();
      const list = screen.getByTestId("inbox-list");
      await waitFor(() => expect(list).toHaveFocus());
      // The list is no text field, so the Inbox's own keys reach the page from it.
      await user.keyboard("f");
      expect(screen.getByRole("searchbox", { name: "Search asks" })).toHaveFocus();
    } finally {
      unmount();
      narrow();
      getAsk.mockRestore();
      getInbox.mockRestore();
      getIssue.mockRestore();
      snoozeAsk.mockRestore();
    }
  });
}

// ----------------------------------------------------------------------------------------
// R7 and R11: `a` and Shift+R on the approval request in hand, and the command bar's own actions
// for the item in hand.
// ----------------------------------------------------------------------------------------

/** An approval request of `SPEC_DOCUMENT` at version 4, which the column draws as that document,
 *  and which a card draws as the request's two options. */
function approvalRow(id: string, key: string): InboxRow {
  return inboxIssue(id, key, "Approve spec.md (version 4)?", {
    approval: {
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 4,
      version: 4,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
}

/** `SPEC_DOCUMENT` at version 4, with the approval request open on it. */
const SPEC_AWAITING: Artifact = {
  ...SPEC_DOCUMENT,
  approval: { latest_version: 4, state: "awaiting" },
  versions: [
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-10T00:00:00Z",
      named: false,
      number: 4,
      summary: null,
    },
  ],
};

/** Whether the `?` help shows the Inbox's action `id` as available now. */
function offered(id: string): boolean | undefined {
  return appKeymap.describe().find((entry) => entry.scope === "inbox" && entry.id === id)?.enabled;
}

/** The command bar's rows of the Inbox as it would open now, by label, each with its keys. */
function barRows(): Map<string, readonly string[]> {
  return new Map(
    appKeymap
      .actions()
      .filter((action) => action.scope === "inbox")
      .map((action) => [action.label, action.keys] as const)
  );
}

test("a and Shift+R press the column head's Approve and Request changes, and do nothing before the document's read returns or on another item", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const rows = [
    approvalRow("ask-approval", "CORE-1"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const reads = mockDocumentColumnReads(rows);
  const read = Promise.withResolvers<Artifact>();
  reads.getArtifact.mockImplementation(() => read.promise);
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [],
    created_at: "2026-09-10T00:00:00Z",
    markdown: "# Spec\n",
    named: false,
    number: 4,
    summary: null,
  });
  // The review stays out, so the request stays in hand for the steps after it.
  const review = spyOn(api, "createArtifactVersionReview").mockImplementation(
    () => new Promise(() => {})
  );
  const { unmount } = renderInbox("/?item=ask-approval", { runtime: fake.runtime });

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(reads.getArtifact).toHaveBeenCalled());
    const row = shortRow("ask-approval");
    act(() => row.focus());
    // Before the read returns the head draws no Approve: both keys are not available, and
    // pressing them sends and opens nothing.
    expect(offered("approve")).toBe(false);
    expect(offered("request-changes")).toBe(false);
    await user.keyboard("a");
    await user.keyboard("{Shift>}R{/Shift}");
    expect(review).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument();

    await act(async () => read.resolve(SPEC_AWAITING));
    const head = pane.querySelector<HTMLElement>("[data-inbox-pane-head]");
    if (head === null) throw new Error("the column drew no head");
    await within(head).findByRole("button", { name: "Approve" });
    expect(offered("approve")).toBe(true);
    expect(offered("request-changes")).toBe(true);
    expect(barRows().get("Approve")).toEqual(["a"]);
    expect(barRows().get("Request changes")).toEqual(["Shift+R"]);

    // Shift+R opens the box with the focus in its reason, and Escape closes it sending nothing.
    await user.keyboard("{Shift>}R{/Shift}");
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    await waitFor(() => expect(within(dialog).getByLabelText("Reason")).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument();
    expect(review).not.toHaveBeenCalled();

    // `a` approves the version the column shows, and opens no box.
    act(() => row.focus());
    await user.keyboard("a");
    await waitFor(() => expect(review).toHaveBeenCalledTimes(1));
    expect(review).toHaveBeenCalledWith("artifact-1", 4, { state: "approved" }, keyedAttempt);
    expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument();

    // On a question there is nothing to approve.
    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    await within(screen.getByTestId("inbox-pane")).findByTestId("ask-ask-b");
    expect(offered("approve")).toBe(false);
    expect(offered("request-changes")).toBe(false);
    await user.keyboard("a");
    await user.keyboard("{Shift>}R{/Shift}");
    expect(review).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument();
  } finally {
    unmount();
    narrow();
    reads.restore();
    getArtifactVersion.mockRestore();
    review.mockRestore();
  }
});

/** The reads a short row's reading view draws an approval request of `SPEC_DOCUMENT` from, at
 *  version 4 with its request open, and the review it sends, held out. */
function approvalViewReads(rows: readonly InboxRow[]) {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const reads = mockDocumentColumnReads(rows);
  reads.getArtifact.mockResolvedValue(SPEC_AWAITING);
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [],
    created_at: "2026-09-10T00:00:00Z",
    markdown: "# Spec\n",
    named: false,
    number: 4,
    summary: null,
  });
  const review = spyOn(api, "createArtifactVersionReview").mockImplementation(
    () => new Promise(() => {})
  );
  return {
    restore: () => {
      reads.restore();
      getArtifactVersion.mockRestore();
      review.mockRestore();
    },
    review,
    runtime: fake.runtime,
  };
}

// R7 below 1280 px: a short row draws no Approve, so `a` and Shift+R open the request's reading
// view and act there, as the digits, `y`, `b`, `h`, `p` and `r` do on a short row.
test("below 1280 px a on a focused approval request's short row opens its reading view and approves the version it shows; on a question it opens nothing", async () => {
  const user = userEvent.setup();
  const rows = [
    approvalRow("ask-approval", "CORE-1"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const reads = approvalViewReads(rows);
  const { unmount } = renderInbox("/", { runtime: reads.runtime });

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-approval", "ask-b"]));
    act(() => shortRow("ask-b").focus());
    await user.keyboard("a");
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();

    act(() => shortRow("ask-approval").focus());
    await user.keyboard("a");
    await screen.findByTestId("inbox-reading-view");
    await waitFor(() => expect(reads.review).toHaveBeenCalledTimes(1));
    expect(reads.review).toHaveBeenCalledWith("artifact-1", 4, { state: "approved" }, keyedAttempt);
    expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument();
  } finally {
    unmount();
    reads.restore();
  }
});

test("below 1280 px Shift+R on a focused approval request's short row opens its reading view with the Request changes box, the focus in its reason", async () => {
  const user = userEvent.setup();
  const rows = [
    approvalRow("ask-approval", "CORE-1"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const reads = approvalViewReads(rows);
  const { unmount } = renderInbox("/", { runtime: reads.runtime });

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-approval", "ask-b"]));
    act(() => shortRow("ask-b").focus());
    await user.keyboard("{Shift>}R{/Shift}");
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();

    act(() => shortRow("ask-approval").focus());
    await user.keyboard("{Shift>}R{/Shift}");
    await screen.findByTestId("inbox-reading-view");
    const dialog = await screen.findByRole("dialog", { name: "Request changes" });
    await waitFor(() => expect(within(dialog).getByLabelText("Reason")).toHaveFocus());
    expect(reads.review).not.toHaveBeenCalled();
  } finally {
    unmount();
    reads.restore();
  }
});

test("below 1280 px Escape in the Request changes box Shift+R opened from a short row gives the focus to the reading view's scroller", async () => {
  const user = userEvent.setup();
  const rows = [
    approvalRow("ask-approval", "CORE-1"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const reads = approvalViewReads(rows);
  const { unmount } = renderInbox("/", { runtime: reads.runtime });

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-approval", "ask-b"]));
    act(() => shortRow("ask-approval").focus());
    await user.keyboard("{Shift>}R{/Shift}");
    const view = await screen.findByTestId("inbox-reading-view");
    const dialog = await screen.findByRole("dialog", { name: "Request changes" });
    await waitFor(() => expect(within(dialog).getByLabelText("Reason")).toHaveFocus());

    // The short row the key was pressed on went with the list, so the box gives the focus back
    // to the view's own stop, as it does to the scroller a press inside the view was made from.
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument()
    );
    await waitFor(() => expect(within(view).getByTestId("inbox-pane-scroller")).toHaveFocus());
    expect(reads.review).not.toHaveBeenCalled();
  } finally {
    unmount();
    reads.restore();
  }
});

test("on a card a picks Approve and sends it, and Shift+R picks Request changes, whose reason Escape lets go of back to the row", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-b", "CORE-2", "Which format?"),
    approvalRow("ask-approval", "CORE-1"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  // The answer stays out, so the card stays for the steps after it.
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(() => new Promise(() => {}));
  const { unmount } = renderInbox("/", { pane: false });

  try {
    const card = await screen.findByTestId("ask-ask-approval");
    const row = shortRow("ask-approval");

    // On a question there is nothing to approve.
    await screen.findByTestId("ask-ask-b");
    act(() => shortRow("ask-b").focus());
    expect(offered("approve")).toBe(false);
    expect(offered("request-changes")).toBe(false);
    await user.keyboard("a");
    expect(answerAsk).not.toHaveBeenCalled();

    act(() => row.focus());
    expect(offered("approve")).toBe(true);
    await user.keyboard("{Shift>}R{/Shift}");
    const requestChanges = within(card).getByRole("radio", { name: "Request changes" });
    expect((requestChanges as HTMLInputElement).checked).toBe(true);
    expect(within(card).getByLabelText("Reason (required)")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect((requestChanges as HTMLInputElement).checked).toBe(false);
    expect(row).toHaveFocus();
    expect(answerAsk).not.toHaveBeenCalled();

    await user.keyboard("a");
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]?.[0]).toBe("ask-approval");
    expect(answerAsk.mock.calls[0]?.[1]).toEqual({
      expected_edited_at: null,
      selected: ["Approve"],
    });
  } finally {
    unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("the command bar offers the card's own actions for the row in hand, and runs them on it", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", {
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    // Below 1280 px the question's card is drawn in the reading view of the row the reader
    // opened, and the command bar acts on the row that view shows.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a").querySelector("time") as HTMLElement);
    const view = await screen.findByTestId("inbox-reading-view");
    const card = await within(view).findByTestId("ask-ask-a");
    const row = screen.getByTestId("inbox-pane-scroller");
    act(() => row.focus());
    let offeredRows = barRows();
    for (const label of ["Decline", "Hand on", "Show the asker's handles"]) {
      expect(offeredRows.get(label)).toEqual([]);
    }
    // Ask back takes a press only once something is typed, and is no action before that; nor
    // are the actions of a control the row does not draw.
    for (const label of ["Ask back", "Mark read", "Un-snooze", "Clear the selection"]) {
      expect(offeredRows.has(label)).toBe(false);
    }
    act(() => within(card).getByLabelText("Your answer").focus());
    await user.paste("Which format do we ship in?");
    act(() => row.focus());
    expect(barRows().get("Ask back")).toEqual([]);

    // Chosen, Decline presses the card's own Decline to answer.
    const decline = appKeymap.actions().find((action) => action.label === "Decline");
    act(() => decline?.run());
    expect(await screen.findByRole("dialog", { name: "Decline to answer" })).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Decline to answer" })).not.toBeInTheDocument();

    // With a row marked, the bar's Select all and Clear selection are rows too; Clear shows the
    // Escape that clears the selection off the row.
    act(() => row.focus());
    await user.keyboard("x");
    await screen.findByText("1 selected");
    offeredRows = barRows();
    expect(offeredRows.get("Select all")).toEqual([]);
    expect(offeredRows.get("Clear the selection")).toEqual(["Escape"]);
    const clear = appKeymap.actions().find((action) => action.label === "Clear the selection");
    act(() => clear?.run());
    await waitFor(() => expect(screen.queryByText("1 selected")).not.toBeInTheDocument());
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("in the reading view below 1280 px the command bar offers the card's own actions while the focus is on a control of the card, and runs them on that card", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", {
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a").querySelector("time") as HTMLElement);
    const view = await screen.findByTestId("inbox-reading-view");
    const card = await within(view).findByTestId("ask-ask-a");
    // A press inside a card leaves the focus on the control pressed, not on its row.
    act(() => within(card).getByRole("button", { name: "Decline to answer" }).focus());
    const offeredRows = barRows();
    for (const label of ["Decline", "Hand on", "Show the asker's handles"]) {
      expect(offeredRows.get(label)).toEqual([]);
    }
    const decline = appKeymap.actions().find((action) => action.label === "Decline");
    act(() => decline?.run());
    expect(await screen.findByRole("dialog", { name: "Decline to answer" })).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Decline to answer" })).not.toBeInTheDocument();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("beside the column the command bar presses the column head's Open page", async () => {
  const narrow = widenToReadingPane();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox("/?item=ask-a");

  try {
    const pane = await screen.findByTestId("inbox-pane");
    await within(pane).findByTestId("ask-ask-a");
    act(() => shortRow("ask-a").focus());
    const open = within(pane).getByRole("link", { name: "Open page" });
    // The Inbox here is drawn at every address and writes its own back, so the press is read
    // off the link itself rather than off the address it would leave for.
    let pressed = 0;
    open.addEventListener("click", () => {
      pressed += 1;
    });
    expect(barRows().get("Open page at the section")).toEqual([]);
    const row = appKeymap.actions().find((action) => action.label === "Open page at the section");
    act(() => row?.run());
    expect(pressed).toBe(1);
  } finally {
    unmount();
    narrow();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});
