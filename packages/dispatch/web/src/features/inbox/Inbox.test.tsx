import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps the row hold's bare pointerover/pointerout with a relatedTarget, a blur naming no target, the Enter, Space and `r` presses whose survival only fireEvent reports, and a held `e`'s repeats after its first press handed the reader on, which user-event sends only in one burst with that press.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import {
  type Commits,
  drawnPick,
  type HeldNotifications,
  holdNotifications,
  recordCommits,
} from "../../__tests__/commits";
import { type FakeEditor, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { afterRescue, RescuedMain } from "../../__tests__/focus-rescue";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { pickNamed } from "../../__tests__/list-picks";
import { choiceOf, pickFromMenu } from "../../__tests__/menu-picks";
import {
  answerAskSnoozes,
  answerInboxDismisses,
  answerIssuePatches,
} from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api, NetworkError } from "../../api/client";
import type {
  Artifact,
  AskRead,
  Comment,
  CredentialPendingRow,
  InboxRow,
  Issue,
  IssueDetails,
  MyRepliesResponse,
} from "../../api/types";
import { QUIET_MS } from "../doc/refocus-when-settled";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, DIALOG_SCOPE } from "../shell/keymap";
import { READING_PANE_QUERY } from "../shell/useDialog";
import { replyRow } from "../threads/replies-fixture";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { Inbox } from "./Inbox";
import {
  FIXTURE_PROJECT_DEFAULT,
  installInboxApiMocks,
  issueAsk,
  mockAskReads,
  withWaitingOnPerson,
} from "./inbox-fixture";
import type { InboxRoute } from "./inbox-route";
import { ROW_IN_HAND_CLASS } from "./option-digits";
import { SnoozeControl } from "./SnoozeControl";

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

function inboxIssue(
  id: string,
  key: string,
  question: string,
  overrides: Partial<InboxRow> = {}
): InboxRow {
  return issueAsk({
    id,
    issue: { assignee: "alice", key, title: `${key} title` },
    issue_key: key,
    question,
    ...overrides,
  });
}

/** The narrow `Issue` a PATCH answers with, for an inbox row's issue. */
function narrowIssueOf(row: InboxRow, assignee: string | null): Issue {
  return {
    advances: [],
    advances_revision: 0,
    assignee,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    closed_at: null,
    created_at: row.created_at,
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: row.issue_key ?? "",
    labels: [],
    last_seq: 2,
    number: 3,
    parent: null,
    blocked_by: [],
    plan_artifact_id: "artifact-plan",
    decisions_artifact_id: "",
    primary_artifact_id: "artifact-3",
    priority: null,
    project: "CORE",
    rank: "U",
    route: null,
    repo: null,
    resolved_repo: null,
    status: "todo",
    title: row.issue?.title ?? "",
    updated_at: row.created_at,
    verification_artifact_id: "artifact-verification",
  };
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
        <Inbox />
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
        <Inbox />
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
        <Inbox />
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
    expect(screen.queryByRole("link", { name: "Clear agent filter" })).not.toBeInTheDocument();
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
        <Inbox />
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
        <Inbox />
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
        <Inbox />
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
  const { unmount } = renderInbox("/?group=waiting-on");

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
  const { unmount } = renderInbox("/?group=waiting-on");

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
    const chip = await screen.findByRole("link", { name: "Clear agent filter" });
    expect(chip.textContent).toBe("Asks from Planner · clear");
    expect(chip).toHaveAttribute("href", "/");
    // The agent's live link sits beside the clear-filter link, never inside it.
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
    expect(screen.getByRole("link", { name: "Clear agent filter" }).textContent).toBe(
      "Asks from session:session-… waiting on you · clear"
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Inbox filtered to an agent with no open asks says so and still offers to clear", async () => {
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
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const emptyState = await screen.findByRole("region", { name: "Inbox empty state" });
    // A session the registry does not list and no listed ask names reads by its short id.
    expect(within(emptyState).getByText("No open asks from session:idle-ses…")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Clear agent filter" })).toHaveAttribute("href", "/");
    expect(screen.queryByText("Which approach?")).not.toBeInTheDocument();
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
        <Inbox />
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
  const { unmount } = renderInbox("/?group=waiting-on");

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
    askBack.focus();
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
        <Inbox />
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

// The same answer from elsewhere, with the page's focus rescue mounted as the app shell mounts it
// (`RescuedMain`). The reader's click on the answer field has been answered, so no press of
// theirs is live when the card swaps to its record. Whoever caused the loss, the card hands the
// focus to its form's own row, the reader's own place, with no scroll: the Inbox keeps that row
// in the list with the recorded answer while it holds the focus, and no other row takes it.
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
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RescuedMain>
          <Inbox />
        </RescuedMain>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId("ask-ask-b");
    const row = card.closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    const field = within(card).getByLabelText("Your answer");
    await user.click(field);
    await afterRescue();
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
    await afterRescue();

    expect(row).toHaveFocus();
    expect(kept.closest("[data-inbox-row]")).toBe(row);
    expect(rowIds()).toEqual(["ask-a", "ask-b"]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    scrolls.mockRestore();
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
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RescuedMain>
            <Inbox />
          </RescuedMain>
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
    await afterRescue();
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
    await afterRescue();
    expect(row).toHaveFocus();
    expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]);
    expect(scrolls).not.toHaveBeenCalled();

    await user.keyboard("j");
    expect(shortRow("ask-c")).toHaveFocus();
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-c"]));
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    scrolls.mockRestore();
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
        <Inbox />
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
  const { queryClient, unmount } = renderInbox("/?priority=P1", { commits });
  let notifications: HeldNotifications | undefined;
  try {
    await screen.findByText("Ship it at P1?");
    const row = rowOf("ask-p1");
    // user-event's hover sends a fixed mouse sequence; the row's hold reads this pointerover alone.
    fireEvent.pointerOver(row);
    const trigger = within(row).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    await user.click(trigger);
    const p2 = choiceOf(trigger, "P2");
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
    const trigger = within(pane).getByRole("button", { name: pickNamed("Priority of CORE-1") });
    await user.click(trigger);
    const p2 = choiceOf(trigger, "P2");
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
  const { queryClient, unmount } = renderInbox("/?q=ship");
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
  const { queryClient, unmount } = renderInbox("/?q=ship", { commits });
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

    const trigger = within(rowOf("ask-p1")).getByRole("button", {
      name: pickNamed("Priority of CORE-1"),
    });
    await user.click(trigger);
    const p2 = choiceOf(trigger, "P2");
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
  const { queryClient, unmount } = renderInbox("/?q=ship");
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
        <Inbox />
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
    answer.focus();
    await user.click(answer);
    // The optimistic removal does not take the row from under the reader: it stays, and the
    // failure shows on it.
    await waitFor(() => expect(within(card).getByRole("alert")).toBeTruthy());
    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(within(card).getByLabelText<HTMLTextAreaElement>("Your answer").value).toBe("JSON");
    field.focus();

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
  const { unmount } = renderInbox();

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
  const { unmount } = renderInbox();

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

/** The address the Inbox is on, and a way back through the history it has written. */
function RouteProbe() {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output data-testid="location">{`${location.pathname}${location.search}`}</output>
      <button onClick={() => navigate(-1)} type="button">
        History back
      </button>
    </>
  );
}

/** `route` is the whole history when it is a list, the last entry being where the reader is:
 *  whether a filter change replaces its entry or pushes one is only visible from a history that
 *  has something underneath it. `runtime` is for the tests whose reading column opens a
 *  document: that column mounts `ProofDocument`, which wants the document runtime and the
 *  margin the app shell gives it, and every other test's column shows a card and needs
 *  neither. `commits` records each commit of the Inbox; it has no one value a host passes, so
 *  each commit's `host` is null. */
function renderInbox(
  route: string | readonly string[] = "/",
  { commits, runtime }: { commits?: Commits<null>; runtime?: DocumentRuntimeValue } = {}
) {
  const entries = typeof route === "string" ? [route] : [...route];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const inbox = (
    <KeymapProvider>
      <RouteProbe />
      {commits === undefined ? (
        <Inbox />
      ) : (
        <commits.Recorded host={null}>
          <Inbox />
        </commits.Recorded>
      )}
    </KeymapProvider>
  );
  const view = render(
    <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
      <QueryClientProvider client={queryClient}>
        {runtime === undefined ? (
          inbox
        ) : (
          <DocumentRuntime.Provider value={runtime}>
            <MarginProvider>{inbox}</MarginProvider>
          </DocumentRuntime.Provider>
        )}
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

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

/** The list's band headings, in order. The reading column's own heading - the title of the item
 *  it shows - is none of them. */
function headings(): string[] {
  return screen
    .getAllByRole("heading", { level: 2 })
    .filter((heading) => heading.closest('[data-testid="inbox-pane"]') === null)
    .map((heading) => heading.textContent ?? "");
}

/** The rows' ask ids in DOM order: the order the reader sees. */
function rowIds(): string[] {
  return [...document.querySelectorAll("[data-inbox-row]")].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
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
  const { unmount } = renderInbox("/?assignee=unassigned");
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
  const { unmount } = renderInbox("/?assignee=unassigned");
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
// Inbox lets go of it in the effects of the commit that draws the save's answer. The button is
// held from the press through that commit, so it goes with its row in the commit its hold ends:
// the press is still live then, and the page's rescue reads the row's going as its doing
// (`causedByReader`). A button let go in the answer's own commit would end the press a commit
// before the row goes, where a browser can run the rescue's check in between.
test("Assign to me stays held through the commit after its answer, and goes with its row in the commit its hold ends", async () => {
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
  const { unmount } = renderInbox("/?assignee=unassigned", { commits });
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

    // `Assign to me` again is the commit that draws the answer: still held there, and the next
    // drawing is the row gone.
    expect(drawnChanges(fromPress())).toEqual(["held Assigning…", "held Assign to me", "(no row)"]);
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// The hand-on's optimistic `waiting_on_person` takes the row out of the reader's own list as
// the write starts, which would unmount the card holding the control: the card registers its
// write with the list through the context the list provides (`hand-on-live.ts`), so the
// refusal is shown where the reader pressed, by the instance that wrote, rather than lost to a
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
  const { unmount } = renderInbox("/");
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
  const { unmount } = renderInbox("/");
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
  const { queryClient, unmount } = renderInbox("/?assignee=everyone");
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

/** An hour from now, as the server stores it: inside any snooze window the reader can pick. */
function soon(hours = 3): string {
  return new Date(Date.now() + hours * 60 * 60 * 1000).toISOString();
}

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
  const unsnoozeAsk = spyOn(api, "unsnoozeAsk").mockImplementation(async (id) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: null } : row));
  });
  const getAsk = mockAskReads([askA, askB]);
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Which format?");
    expect(headings()).toEqual(["Waiting on you (2)"]);
    expect(screen.getByRole("button", { name: /Blocked on you: 2 items/ })).toBeTruthy();

    const row = screen.getByTestId("ask-ask-b").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-b row missing");
    await pickFromMenu(
      user,
      within(row).getByLabelText("Snooze CORE-2: Which format?"),
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
    expect(unsnoozeAsk).toHaveBeenCalledWith("ask-b", keyedAttempt);
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
  const { queryClient, unmount } = renderInbox();
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

test("a snooze still saving says Snoozing…, not Un-snoozing…, in the band it has moved to", async () => {
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
  const { unmount } = renderInbox();
  try {
    await screen.findByText("Which format?");
    await pickFromMenu(user, screen.getByLabelText("Snooze CORE-1: Which format?"), "Tomorrow");

    // The optimistic update has already put the row in Later, which is right - it is where the
    // snooze is going - but the label must say what the reader did, not where the row landed.
    await waitFor(() => expect(headings()).toEqual(["Later (1)"]));
    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    expect(screen.getByText("Snoozing…")).toBeTruthy();
    expect(screen.queryByText("Un-snoozing…")).not.toBeInTheDocument();
    // The optimistic return time is shown beside it; that is the snooze being saved.
    expect(document.querySelector("[data-inbox-snoozed-until]")).toBeTruthy();

    // Once it settles the row reports its state again, not the last thing done to it.
    await act(async () => {
      settle?.();
      await Promise.resolve();
    });
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
  const { unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Which format?");
    await pickFromMenu(user, screen.getByLabelText("Snooze CORE-1: Which format?"), "Tomorrow");

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
  const { queryClient, unmount } = renderInbox("/?group=waiting-on");
  try {
    await screen.findByText("Which format?");
    jest.useFakeTimers();
    await pickFromMenu(
      userOnFakeClock(),
      screen.getByLabelText("Snooze CORE-1: Which format?"),
      "Tomorrow"
    );
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
// the reader stopped is of that press's ask under that press's key, whichever ask the control is
// drawn for by then. The case draws one control for one ask and then for another, which is where
// the two differ.
test("a snooze's Retry is of the ask its press was of, whichever ask the control draws by then", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-a/snooze", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const onLive = () => {};
  const drawn = (askId: string) => (
    <QueryClientProvider client={queryClient}>
      <SnoozeControl askId={askId} label={askId} onLive={onLive} snoozedUntil={null} />
    </QueryClientProvider>
  );
  const view = render(drawn("ask-a"));
  try {
    await pickFromMenu(user, screen.getByLabelText("Snooze ask-a"), "Tomorrow");
    await stopTheLateWrite(queryClient);
    const stoppedKey = snoozeAsk.mock.calls[0]?.[2]?.key;
    const stopped = snoozeAsk.mock.calls.length;

    view.rerender(drawn("ask-b"));
    snoozeAsk.mockImplementation(async (_id, { snoozed_until }) => ({ snoozed_until }));
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(1_000);

    expect(snoozeAsk).toHaveBeenCalledTimes(stopped + 1);
    const again = snoozeAsk.mock.calls.at(-1);
    expect(again?.[0]).toBe("ask-a");
    expect(again?.[2]?.key).toBe(stoppedKey);
  } finally {
    realClock();
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

// The row a live snooze asks the Inbox to hold is its press's ask too: drawn for another ask
// while the snooze of the first is out, the control keeps the first ask's row held, holds no row
// for the ask it shows, and lets the first go once Dispatch answers.
test("a snooze out when the control is drawn for another ask holds the row of the ask it was pressed on", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<{ snoozed_until: string }>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(() => landing.promise);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const liveness: [string, boolean][] = [];
  const onLive = (askId: string, live: boolean) => {
    liveness.push([askId, live]);
  };
  const drawn = (askId: string) => (
    <QueryClientProvider client={queryClient}>
      <SnoozeControl askId={askId} label={askId} onLive={onLive} snoozedUntil={null} />
    </QueryClientProvider>
  );
  const view = render(drawn("ask-a"));
  try {
    await pickFromMenu(user, screen.getByLabelText("Snooze ask-a"), "Tomorrow");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    expect(liveness).toEqual([["ask-a", true]]);

    view.rerender(drawn("ask-b"));
    expect(liveness).toEqual([["ask-a", true]]);

    landing.resolve({ snoozed_until: "2026-11-17T09:00:00Z" });
    await waitFor(() => expect(liveness.length).toBe(2));
    expect(liveness).toEqual([
      ["ask-a", true],
      ["ask-a", false],
    ]);
  } finally {
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

/** What the row of `askId` draws of its Snooze: the menu's trigger (`data-inbox-snooze`), which
 *  stays at its one place in the row through a write, held (`aria-disabled`) or free; `(no row)`
 *  while the list draws no such row. */
function drawnSnooze(askId: string): string {
  const row = document.querySelector(`[data-inbox-row="${askId}"]`);
  if (row === null) return "(no row)";
  const trigger = row.querySelector("[data-inbox-snooze]");
  if (trigger === null) return "(no trigger)";
  return `Snooze ${trigger.getAttribute("aria-disabled") === "true" ? "held" : "free"}`;
}

// A Snooze pick folds its row into Later, which starts folded, in the commit after the write's
// answer, where the Inbox lets go of the row. The pick is the reader's press, and the row's going
// is its doing only while the trigger the menu handed the focus back to is held
// (`causedByReader`), so the trigger stays drawn and held from the pick through that commit and
// goes with its row in the commit its hold ends. The write is out across two of the rescue's
// checks, as a server slower than a frame keeps it, the trigger keeping the focus, and the
// snooze's landing hands the focus on to the item after the row (R8) - here the row before it,
// the one row left - not to the page.
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
  const commits = recordCommits<null>(() => drawnSnooze("ask-b"));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RescuedMain>
            <commits.Recorded host={null}>
              <Inbox />
            </commits.Recorded>
          </RescuedMain>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText("Which format?");
    const trigger = within(rowOf("ask-b")).getByLabelText("Snooze CORE-2: Which format?");
    await user.click(trigger);
    const tomorrow = choiceOf(trigger, "Tomorrow");
    const fromPick = commits.markAt(tomorrow, "click");
    await user.click(tomorrow);
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    await afterRescue();
    await afterRescue();
    expect(trigger).toHaveFocus();

    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    expect(drawnChanges(fromPick())).toEqual(["Snooze held", "(no row)"]);
    await afterRescue();

    expect(rowOf("ask-a")).toHaveFocus();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

// ----------------------------------------------------------------------------------------
// The reading column (INFRA-575). Every test above this line runs at happy-dom's own 1024 px,
// where the Inbox is the list of cards it has always been - the Inbox below 1280 px, which the
// spec keeps unchanged - so the tests below say where they stand by widening the window first.
// ----------------------------------------------------------------------------------------

/** The width the Inbox shows its reading column from (`READING_PANE_QUERY`, 1280 px). happy-dom
 *  answers `matchMedia` from the window it is given, so the window is the whole fixture. */
function widenToReadingPane(): () => void {
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width: 1440 });
  return () => window_.happyDOM.setViewport({ width: was });
}

/** A width the test can move while the Inbox is mounted, which `widenToReadingPane` cannot:
 *  happy-dom answers `matchMedia` from its own window, but it delivers no `change` to a list
 *  made before `setViewport`, so a column already standing never hears the viewport narrow.
 *  This is the stub `__tests__/responsive-shell.test.tsx` installs - one object per query,
 *  answering `matches` for the query it was asked - with the one addition a move needs: it keeps
 *  the listeners `useMediaQuery` registers, so `setWidth` delivers the `change` a browser sends.
 */
function movableReadingPaneWidth(width: number): {
  restore: () => void;
  setWidth: (next: number) => void;
} {
  const originalMatchMedia = window.matchMedia;
  const changeListeners = new Set<(event: Event) => void>();
  let now = width;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") changeListeners.add(listener);
      },
      addListener: () => {},
      dispatchEvent: () => true,
      get matches() {
        return query === READING_PANE_QUERY && now >= 1280;
      },
      media: query,
      onchange: null,
      removeEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") changeListeners.delete(listener);
      },
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  return {
    restore: () => {
      window.matchMedia = originalMatchMedia;
    },
    setWidth: (next: number) => {
      now = next;
      for (const listener of changeListeners) listener(new Event("change"));
    },
  };
}

/** One row by its ask id: a short row holds no card, so there is no `ask-<id>` to find it by. */
function shortRow(askId: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-inbox-row="${askId}"]`);
  if (row === null) throw new Error(`${askId} row missing`);
  return row;
}

/** The ask ids of the rows marked selected: exactly one, or the test has found a second. */
function selectedIds(): string[] {
  return [...document.querySelectorAll('[data-inbox-selected="true"]')].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

function address(): string {
  return screen.getByTestId("location").textContent ?? "";
}

/** The pane reads one field of the question's issue: whether it is closed. */
function mockIssueReads() {
  return spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails);
}

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
    // The card is the pane's, never the row's; the row is the question's first line, its kind
    // and its age.
    expect(within(list).queryByTestId("ask-ask-a")).not.toBeInTheDocument();
    expect(within(pane).getByTestId("ask-ask-a")).toBeTruthy();
    const row = shortRow("ask-a");
    expect(within(row).getByText("Which approach?")).toBeTruthy();
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

test("below 1280 px the Inbox is today's Inbox: the card in the row, no pane and no ?item=", async () => {
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    const card = await screen.findByTestId("ask-ask-a");
    expect(card.closest("[data-inbox-row]")).toBe(shortRow("ask-a"));
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    expect(selectedIds()).toEqual([]);
    expect(address()).toBe("/");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
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
// hold the answer itself takes. Reverted, the optimistic removal hands the selection on at once
// and unmounts the card, which is the only surface the refusal is shown on.
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

    // The optimistic removal takes the row at once; the column keeps the question, because the
    // answer is still the reader's to finish, and the selection stays on it.
    await waitFor(() => expect(rowIds()).toEqual(["ask-b"]));
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

// The same withdrawal with the page's focus rescue mounted as the app shell mounts it
// (`RescuedMain`), and no press of the reader's live when it lands. Whoever caused the loss, the
// card hands the focus to its form's nearest focus stop, in the column the column's own focus
// stop, with no scroll, and the Inbox's `j` answers from there. Left to the rescue, the focus
// would land on a control of the record drawn in the form's place, and a text box there would
// take the `j` as text.
test("in the reading column, a question withdrawn under the reader's focus hands it to the column's focus stop, and j then selects the next row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const second = inboxIssue("ask-b", "CORE-2", "Which format?");
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <RescuedMain>
            <Inbox />
          </RescuedMain>
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
    await afterRescue();
    expect(draft).toHaveFocus();
    // The opening selection keeps its row in sight (`pane-selection.ts`); what follows is the
    // loss, which scrolls nothing.
    scrolls.mockClear();

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
    await afterRescue();

    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();
    expect(document.activeElement?.closest("[data-inbox-row]") ?? null).toBeNull();
    expect(scrolls).not.toHaveBeenCalled();

    await user.keyboard("j");
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(within(screen.getByTestId("inbox-pane")).getByTestId("ask-ask-b")).toBeTruthy();
  } finally {
    view.unmount();
    narrow();
    scrolls.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
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

    // `h` with nothing checked opens the pane head's Snooze menu, which the short row no longer
    // carries, on its first choice; Escape closes it, writes nothing, and puts the focus back on
    // the row `h` was pressed on.
    await user.keyboard("h");
    const snoozeMenu = screen.getByRole("menu", { name: /^Snooze CORE-1/ });
    expect(within(snoozeMenu).getByRole("menuitem", { name: "Later today" })).toHaveFocus();
    expect(pane.querySelector("[data-inbox-snooze]")).toHaveAttribute("aria-expanded", "true");
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
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
  const { unmount } = renderInbox();

  try {
    await screen.findByText("Which approach?");
    const issueRow = rowOf("ask-a");
    act(() => issueRow.focus());
    expect(offered("priority")).toBe(true);
    await user.keyboard("p");
    const menu = screen.getByRole("menu", { name: "Priority of CORE-1" });
    const checked = within(menu).getByRole("menuitemradio", { name: "P2" });
    expect(checked).toHaveAttribute("aria-checked", "true");
    expect(checked).toHaveFocus();
    await user.keyboard("{ArrowUp}{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
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
    // The focus is still on the row, so the next `p` opens its menu again, on P0.
    await user.keyboard("p");
    expect(screen.getByRole("menuitemradio", { name: "P0" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(issueRow).toHaveFocus();

    // A question on a project document has no issue, so no priority to open.
    act(() => rowOf("ask-1").focus());
    expect(offered("priority")).toBe(false);
    await user.keyboard("p");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    patchIssue.mockRestore();
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
    expect(screen.getByRole("menuitemradio", { name: "P2" })).toHaveFocus();
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

    // `h` opens the column's own Snooze menu, and `2` picks Tomorrow in it. The menu stands over
    // the page, but it opens from the column and hands the focus back into it, so it keeps the
    // record. The deferral is what ends it, and both of the drops that keep the record's
    // question true cover that between them: the row folds into Later for a commit with nothing
    // shown at all, where the column goes and `holdPane` drops first, and the question the
    // column is handed next is the change the layout effect reads. This row pins where Escape
    // goes, which the two of them agree on; it does not tell them apart.
    await user.keyboard("h");
    expect(screen.getByRole("menu", { name: /^Snooze CORE-2/ })).toBeInTheDocument();
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

test("the column going when the Inbox narrows drops the record, where nothing else can", async () => {
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

    // Below the column's width the Inbox is the list of cards it has always been. The selection
    // stands - the address still names it - so no question changed and the layout effect drops
    // nothing; the scroller goes with the column, taking the focus to the body with no blur and
    // no `focusin` behind it, so neither focus drop reads anything either. The wrapper's own
    // unmount is the only report there is, which makes this `holdPane`'s row alone.
    act(() => width.setWidth(1279));
    await waitFor(() => expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument());
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

    // `h` opens the column's own Snooze menu: it stands over the page, portalled out of the
    // column, but it opens from a control inside the column, and the focus moving into it and
    // through its choices names targets the column counts as its own. Escape closes it and hands
    // the focus back to the scroller, the question it shows has not changed - so the record
    // stands and a second Escape is still their way back to the row they came from. This is the
    // keep branch of the blur rule, the drop above it being the one the search box and a row
    // take.
    await user.keyboard("h");
    const menu = screen.getByRole("menu", { name: /^Snooze CORE-2/ });
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
    shortRow("ask-a").focus();
    await user.keyboard("l");
    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(answerBox()).toHaveFocus();

    // From the list it does its own job: the row hands the focus to the column's answer box.
    shortRow("ask-a").focus();
    await user.keyboard("{Enter}");
    expect(answerBox()).toHaveFocus();

    // A button of the column is no place Enter starts from: the press is the button's - the
    // head's Copy reference here, beside the card's own.
    const head = pane.querySelector<HTMLElement>("[data-inbox-pane-head]");
    if (head === null) throw new Error("the column drew no head");
    const copy = within(head).getByRole("button", { name: /^Copy reference/ });
    copy.focus();
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
test("below the column's width a card's digits show for its row in hand, and the keys act on the card around a focused option", async () => {
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
  const { unmount } = renderInbox();

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
  const { unmount } = renderInbox("/");

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

// The same refusal, pressed on the card the reading column draws for a question written as a
// decision block. That card is the document page's own (`ReadingPane` -> `DocumentPane`, in
// `PaneDocument.tsx`, -> `ProofDocument` -> `AskBlockCard`), so no prop of this list reaches
// it: the registration it holds its row with is the one it reads from the context the list
// provides. Without it the optimistic `waiting_on_person` drops the row, the column has
// nothing selected to draw, and the refusal comes back to no card at all.
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
// those must leave the list as it stands: the hold is for a card the list can drop, and
// nothing here can drop this one. Held anyway, the optimistic `waiting_on_person` would draw
// the hidden question into her list, and a refusal would leave it there for good - the write
// stays failed until it is tried again, and no card unmounts to let it go.
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
  const { unmount } = renderInbox("/");
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
    // With the focus on nothing, the menu hands it back to its own trigger as it closes.
    await user.keyboard("h");
    const list = screen.getByTestId("inbox-list");
    const picker = screen.getByRole("button", { name: "Snooze selected asks" });
    const menu = screen.getByRole("menu", { name: "Snooze selected asks" });
    // The bar snoozes only, so its menu offers no Un-snooze.
    expect(within(menu).queryByRole("menuitem", { name: "Un-snooze" })).not.toBeInTheDocument();

    await user.keyboard("2");
    // Every marked row folds into Later at once, so the selection - the row the column was
    // showing among them - goes to the row after the last of them, as the pick said.
    await waitFor(() => expect(rowIds()).toEqual(["ask-c"]));
    await waitFor(() => expect(selectedIds()).toEqual(["ask-c"]));
    // The list and the trigger are the same nodes: the list stayed, and the keyboard is still in
    // the bar it was in, on a trigger held while its write is out.
    expect(screen.getByTestId("inbox-list")).toBe(list);
    expect(screen.getByRole("button", { name: "Snooze selected asks" })).toBe(picker);
    expect(picker).toHaveFocus();
    expect(picker).toHaveAttribute("aria-disabled", "true");

    await act(async () => {
      landing.resolve();
      await Promise.resolve();
    });
    await waitFor(() => expect(screen.queryByText("Snoozing 2…")).not.toBeInTheDocument());
    // The pick has landed: the bar goes, and the picker no longer keeps the focus - the row
    // after the last snoozed one has it (R8), selected.
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
    await pickFromMenu(
      user,
      within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );
    // The optimistic snooze has folded the row into Later, and the row is listed inside that
    // fold while its write is out - which is what keeps the column, and the control that
    // wrote, on the page.
    await waitFor(() => expect(headings()).toEqual(["Waiting on you (1)", "Later (1)"]));
    expect(selectedIds()).toEqual(["ask-a"]);
    expect(shortRow("ask-a")).toHaveAttribute("data-inbox-section", "later");
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

// The column's head is its question's own: the head's Snooze holds the write it made, so the
// question the reader moves to is drawn under a head of its own, never under one still saying
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
    await pickFromMenu(
      user,
      within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
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
    await pickFromMenu(
      user,
      within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
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
    await pickFromMenu(
      user,
      within(pane).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );

    // A row the reader defers is a row they have dealt with: the next one takes its place and
    // the address follows, as it does for a row they answer, and its row has the keyboard focus
    // (R8), where the menu had handed it back to the Snooze that wrote.
    await waitFor(() => expect(selectedIds()).toEqual(["ask-b"]));
    expect(address()).toBe("/?item=ask-b");
    await waitFor(() => expect(shortRow("ask-b")).toHaveFocus());

    await pickFromMenu(
      user,
      within(screen.getByTestId("inbox-pane")).getByLabelText("Snooze CORE-2: Which format?"),
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

test("below the column's width a snooze gives the next row the focus, and an answer does once the server records it", async () => {
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
  const { unmount } = renderInbox();

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
// keeping may have moved the page again, so the hand-off scrolls the row it focuses into view
// itself, as the page's focus rescue does (`focusIntoView`): at a phone's width the row after a
// snoozed one is in the reader's sight the moment it has the focus.
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
  /** Each row scrolled into view, with how, and whether it held the focus as it was. */
  const scrolled: { focused: boolean; options: unknown; row: string | null }[] = [];
  const scrolls = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element,
    options?: boolean | ScrollIntoViewOptions
  ) {
    scrolled.push({
      focused: document.activeElement === this,
      options,
      row: this.getAttribute("data-inbox-row"),
    });
  });
  const { unmount } = renderInbox();

  try {
    await screen.findByTestId("ask-ask-b");
    act(() => rowOf("ask-b").focus());
    await user.keyboard("b");
    await user.keyboard("2");
    await waitFor(() => expect(rowOf("ask-c")).toHaveFocus());
    expect(scrolled).toContainEqual({
      focused: true,
      options: { block: "nearest", inline: "nearest" },
      row: "ask-c",
    });
  } finally {
    unmount();
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
  const { queryClient, unmount } = renderInbox();

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

// `e` dismisses once per press (`CatalogueEntry.writes`). A dismiss hands the reader on to the
// next row at once (R8), so each repeat of a held `e` would dismiss a row nobody chose.
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
  const { unmount } = renderInbox();

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
  const { unmount } = renderInbox();

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
  const { unmount } = renderInbox();

  try {
    const card = await screen.findByTestId("ask-ask-a");
    const row = shortRow("ask-a");
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
  }
});

test("below the column the command bar offers the card's own actions while the focus is on a control of the card, and runs them on that card", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", {
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const { unmount } = renderInbox();

  try {
    const card = await screen.findByTestId("ask-ask-a");
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
