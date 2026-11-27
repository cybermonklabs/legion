import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect, useState } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { RegisterRouteDocument } from "../../__tests__/draft-matrix-fixture";
import { realClock } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { specFormRefusal, specFormRefusalBody } from "../../__tests__/spec-form-fixture";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Ask, AskRead, Comment, IssueAskRow, IssueDetails } from "../../api/types";
import { heldSends } from "../conversation/held-sends";
import { SendMovedError } from "../conversation/send-request";
import type { RetypeOutcome } from "../doc/editor";
import { issueAsk } from "../inbox/inbox-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { SendStoppedError } from "../writes/keyed-write";
import { Margin } from "./Margin";
import {
  type DocumentBridge,
  MarginProvider,
  marginComposeSendKey,
  useMargin,
} from "./margin-context";
import {
  anchoredAsk,
  comment,
  DESKTOP_WIDTH,
  issue,
  PHONE_WIDTH,
  SelectedItemLabel,
  specArtifact,
  stubDocumentBridge,
  stubViewport,
} from "./margin-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const secondIssue: IssueDetails = {
  ...issue,
  artifacts: [
    {
      ...specArtifact,
      id: "artifact-2",
      issue_key: "CORE-2",
    },
  ],
  key: "CORE-2",
  number: 2,
  primary_artifact_id: "artifact-2",
  title: "Second issue",
};

const unanchoredRootComment: Comment = {
  ...comment,
  anchor: null,
  body: "Issue-level comment.",
  id: "comment-unanchored-root",
};

const unanchoredAsk: Ask = {
  ...anchoredAsk,
  anchor: null,
  id: "ask-unanchored",
  question: "Approve the release?",
};

/** One row of `listIssueAsks`: the ask plus the list's own `last_reply`. The margin reads the
 *  ask itself and asks for no thread, so a fixture row carries no reply either. */
function askRow(ask: Ask): IssueAskRow {
  return {
    ...ask,
    last_reply: null,
  };
}

/** An ask's thread with nothing on it: no reply, no rewording, no answer, no follower. */
const emptyAskThread = {
  answers: [],
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
} satisfies Omit<AskRead, "ask">;

/** The thread each margin ask card reads for its own ask (`GET /api/v1/asks/{id}`): the ask as
 *  the test has it, and nothing else on it. */
function answerAskThreads(...asks: Ask[]) {
  return spyOn(api, "getAsk").mockImplementation(async (id) => {
    const ask = asks.find((candidate) => candidate.id === id);
    if (ask === undefined) throw new Error(`no fixture ask ${id}`);
    return { ...emptyAskThread, ask };
  });
}

const markComposerAnchor = { artifact: "artifact-1", mark_id: "m-1", quote: "selected" };

/** A margin ask card by its ask, in the group it is listed under: open and waiting on the reader,
 *  or decided. Both groups are one keyed list, so the group is the card's own attribute. */
function marginAskCard(id: string, section: "needs-you" | "decided"): Promise<HTMLElement> {
  return waitFor(() => {
    const card = document.querySelector<HTMLElement>(
      `[data-margin-item="${id}"][data-margin-section="${section}"]`
    );
    if (card === null) throw new Error(`no ${section} card for ${id}`);
    return card;
  });
}

function MarkComposerProbe(): ReactNode {
  const { composeForMark } = useMargin();
  const [firstOutcome, setFirstOutcome] = useState("idle");
  const [secondOutcome, setSecondOutcome] = useState("idle");

  const compose = (anchor: typeof markComposerAnchor, setOutcome: (outcome: string) => void) => {
    void composeForMark({ anchor, kind: "comment" }).then(
      () => setOutcome("saved"),
      (error: unknown) => setOutcome(error instanceof Error ? error.message : String(error))
    );
  };

  return (
    <>
      <button onClick={() => compose(markComposerAnchor, setFirstOutcome)} type="button">
        Compose first
      </button>
      <button
        onClick={() =>
          compose({ artifact: "artifact-1", mark_id: "m-2", quote: "second" }, setSecondOutcome)
        }
        type="button"
      >
        Compose second
      </button>
      <output aria-label="First composer outcome">{firstOutcome}</output>
      <output aria-label="Second composer outcome">{secondOutcome}</output>
    </>
  );
}

function OpenAskComposerButton(): ReactNode {
  const { composeForMark } = useMargin();

  return (
    <button
      onClick={() => {
        void composeForMark({ anchor: markComposerAnchor, kind: "ask" }).catch(() => {});
      }}
      type="button"
    >
      Open ask composer
    </button>
  );
}

function NavigateToSecondIssue(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() => navigate(buildIssuePath({ key: "CORE-2", kind: "issue" }))}
      type="button"
    >
      Open second issue
    </button>
  );
}

function FocusMarkButton(): ReactNode {
  const { focusItemForMark } = useMargin();

  return (
    <button onClick={() => focusItemForMark("m-1")} type="button">
      Focus mark
    </button>
  );
}

function LocationLabel(): ReactNode {
  const { pathname } = useLocation();

  return <output aria-label="Current path">{pathname}</output>;
}

test("clicking a document mark opens its thread in the margin and stays on the document", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const specPath = buildIssuePath({ key: issue.key, kind: "spec" });
  const view = render(
    <MemoryRouter initialEntries={[specPath]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <FocusMarkButton />
          <SelectedItemLabel />
          <LocationLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId(`margin-comment-${comment.id}`);
    await user.click(screen.getByRole("tab", { name: "Pinned" }));
    await user.click(screen.getByRole("button", { name: "Focus mark" }));
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Comments" })).toHaveAttribute(
        "aria-selected",
        "true"
      );
      // Re-query: expanding the thread remounts the card.
      expect(screen.getByTestId(`margin-comment-${comment.id}`)).toHaveAttribute(
        "aria-current",
        "true"
      );
    });
    expect(screen.getByLabelText("Selected margin item").textContent).toBe(comment.id);
    expect(screen.getByLabelText("Current path").textContent).toBe(specPath);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// A document renamed to a slug-shaped name keeps its first slug, and its page opens at its current
// name too (`artifactAtAddress`): the margin there shows that document's comments, as the page
// shows the document, and none of another document's.
test("the margin of a document opened at its current name, not its slug, lists that document's comments", async () => {
  const notes = {
    ...specArtifact,
    id: "artifact-notes",
    name: "launch-notes",
    primary: false,
    ref_key: "CORE-1/notes-md",
    slug: "notes-md",
  };
  const notesComment: Comment = {
    ...comment,
    anchor: comment.anchor === null ? null : { ...comment.anchor, artifact_id: notes.id },
    body: "A note on the notes.",
    id: "comment-notes",
  };
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), notes],
  });
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment, notesComment]);
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = render(
    <MemoryRouter
      initialEntries={[buildIssuePath({ key: issue.key, kind: "artifact", slug: "launch-notes" })]}
    >
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId(`margin-comment-${notesComment.id}`);
    expect(screen.queryByTestId(`margin-comment-${comment.id}`)).toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

test("focusItemForMark opens the matching thread in the compact sheet", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <FocusMarkButton />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId(`margin-comment-${comment.id}`);
    await user.click(screen.getByRole("button", { name: "Focus mark" }));
    await waitFor(() => {
      const phoneThread = screen.queryByRole("dialog", { name: "Thread" });
      const focusedCard =
        phoneThread === null
          ? card
          : within(phoneThread).getByTestId(`margin-comment-${comment.id}`);
      expect(focusedCard).toHaveAttribute("aria-current", "true");
      expect(screen.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    });
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// A closed issue refuses an ask, so an unsent ask compose ends when its issue closes.
test("Margin hides an open ask composer when its issue closes", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <OpenAskComposerButton />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No comments, asks, or suggestions on this document.");
    await user.click(screen.getByRole("button", { name: "Open ask composer" }));
    await screen.findByRole("form", { name: "Comment composer" });

    act(() => {
      queryClient.setQueryData(["issue", issue.key], {
        ...issue,
        closed_at: "2026-09-09T01:00:00Z",
      });
    });

    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
  }
});

// A closed issue takes a comment, so an unsent comment compose stays open when its issue closes,
// offers no Suggest or Ask, and sends.
test("Margin keeps an open comment composer when its issue closes, and it sends", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  // The send refetches the issue, its inbox and its comments; each answers the seeded set, and
  // the refetch comes after the close, so the issue it answers is the closed one.
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    ...issue,
    closed_at: "2026-09-09T01:00:00Z",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const open = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(open).getByRole("button", { name: "Suggest" })).toBeTruthy();
    act(() => {
      queryClient.setQueryData(["issue", issue.key], {
        ...issue,
        closed_at: "2026-09-09T01:00:00Z",
      });
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Suggest" })).toBeNull());
    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect(within(composer).queryByRole("button", { name: "Ask" })).toBeNull();
    await user.click(within(composer).getByLabelText("Comment"));
    await user.paste("closed");
    await user.click(within(composer).getByRole("button", { name: "Send" }));

    await waitFor(() => {
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
      expect(createComment).toHaveBeenCalledTimes(1);
    });
  } finally {
    view.unmount();
    createComment.mockRestore();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listComments.mockRestore();
  }
});

test("Margin hides an open composer when navigating to a different artifact", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === "CORE-2" ? secondIssue : issue
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <OpenAskComposerButton />
          <NavigateToSecondIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No comments, asks, or suggestions on this document.");
    await user.click(screen.getByRole("button", { name: "Open ask composer" }));
    await screen.findByRole("form", { name: "Comment composer" });
    await user.click(screen.getByRole("button", { name: "Open second issue" }));

    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

test("Margin surfaces and retries a failed fetch for the issue's asks", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const getAsk = answerAskThreads(anchoredAsk);
  const listIssueAsks = spyOn(api, "listIssueAsks")
    .mockRejectedValueOnce(new Error("offline"))
    .mockResolvedValue([askRow(anchoredAsk)]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not load this document's asks.");
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByTestId("ask-ask-1");
    expect(listIssueAsks).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    listIssueAsks.mockRestore();
    getAsk.mockRestore();
  }
});

test("Margin puts an unanchored open ask under Needs you and sends its answer", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    ...unanchoredAsk,
    answer: {
      at: "2026-09-10T00:01:00Z",
      selected: ["Ship"],
      text: null,
      user: "alice",
    },
    state: "answered",
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [unanchoredAsk]);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  // The answer refetches the issue, its asks and the inbox. The inbox lists open asks, and this
  // one has just been answered, so it is no longer in it.
  const getAsk = answerAskThreads(unanchoredAsk);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue(answeredAsk);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(await screen.findByRole("heading", { name: "Needs you" })).toBeTruthy();
    const needsYou = await marginAskCard(unanchoredAsk.id, "needs-you");
    expect(await within(needsYou).findByText(unanchoredAsk.question)).toBeTruthy();
    expect(within(needsYou).getAllByRole("time")).toHaveLength(1);
    expect(within(needsYou).queryByLabelText("Your answer")).not.toBeInTheDocument();
    expect(
      within(needsYou).getByText("Add a note or answer in your own words", { exact: true })
    ).toBeTruthy();
    const shipOption = await within(needsYou).findByRole("radio", { name: "Ship" });
    await user.click(shipOption);
    expect((shipOption as HTMLInputElement).checked).toBe(true);
    await user.click(within(needsYou).getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(answerAsk).toHaveBeenCalledWith(
        unanchoredAsk.id,
        {
          selected: ["Ship"],
          expected_edited_at: null,
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
  }
});

// Dispatch refuses a person's Approve when the document breaks the form of The Spec. The card in
// the issue page's Needs you shows that refusal, picks Request changes with the reason Dispatch
// wrote for the author, and gives the focus back to Answer, so one more press sends the findings.
// The answer takes the ask's row out of the Inbox read while it is out, and the refusal puts the
// row back, which draws the card again: the refusal, the pick and the focus survive that redraw.
test("Margin's Needs you card keeps Dispatch's form refusal of Approve through its redraw, with Request changes picked and the reason filled", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const approval = issueAsk({
    ...unanchoredAsk,
    id: "ask-approval",
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 12)?",
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [approval]);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const getAsk = answerAskThreads(approval);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  // The refusal reads the Inbox again, and the request is still open in it.
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([approval]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(specFormRefusal());
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const before = await marginAskCard(approval.id, "needs-you");
    await user.click(await within(before).findByRole("radio", { name: "Approve" }));
    await user.click(within(before).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));

    const card = await marginAskCard(approval.id, "needs-you");
    const alert = await within(card).findByRole("alert");
    expect(alert.textContent).toBe(
      specFormRefusalBody.error.replace(
        "To send these findings to the author, choose Request changes.",
        "To send these findings to the author, choose Answer."
      )
    );
    expect(
      (within(card).getByRole("radio", { name: "Request changes" }) as HTMLInputElement).checked
    ).toBe(true);
    expect((within(card).getByLabelText("Reason (required)") as HTMLTextAreaElement).value).toBe(
      specFormRefusalBody.changes_reason
    );
    await waitFor(() => expect(within(card).getByRole("button", { name: "Answer" })).toHaveFocus());
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
  }
});

// The answered card moves from Needs you to the decided asks in place - one keyed list - so its
// card is the one the reader was in, with whatever they had started in it (`threads.e2e.ts` types
// a reply there), rather than a fresh mount of it.
test("Margin moves an answered anchored ask out of Needs you in place, without the live event feed", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    ...anchoredAsk,
    answer: {
      at: "2026-09-10T00:01:00Z",
      selected: ["Ship"],
      text: null,
      user: "alice",
    },
    state: "answered",
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [anchoredAsk]);
  queryClient.setQueryData(["asks", issue.key], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue(answeredAsk);
  const refetched = Promise.withResolvers<IssueAskRow[]>();
  const getAsk = answerAskThreads(anchoredAsk);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  // The inbox lists open asks, and this one has just been answered.
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockImplementation(() => refetched.promise);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const needsYou = await marginAskCard(anchoredAsk.id, "needs-you");
    await user.click(await within(needsYou).findByRole("radio", { name: "Ship" }));
    await user.click(within(needsYou).getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(answerAsk).toHaveBeenCalledWith(
        anchoredAsk.id,
        {
          selected: ["Ship"],
          expected_edited_at: null,
        },
        keyedAttempt
      )
    );
    await waitFor(() => expect(listIssueAsks).toHaveBeenCalled());
    const answered = await within(needsYou).findByTestId(`ask-${anchoredAsk.id}`);

    await act(async () => refetched.resolve([askRow(answeredAsk)]));
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Needs you" })).not.toBeInTheDocument()
    );
    expect(screen.getByRole("button", { name: "Open review panel (0 open asks)" })).toBeTruthy();
    const decided = await marginAskCard(anchoredAsk.id, "decided");
    expect(decided).toBe(needsYou);
    expect(within(decided).getByTestId(`ask-${anchoredAsk.id}`)).toBe(answered);
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    listIssueAsks.mockRestore();
    getAsk.mockRestore();
    getIssue.mockRestore();
    getInbox.mockRestore();
  }
});

test("a reply typed into an answered ask's thread survives leaving the Comments tab and back", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    ...anchoredAsk,
    answer: { at: "2026-09-10T00:01:00Z", selected: ["Ship"], text: null, user: "alice" },
    state: "answered",
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [anchoredAsk]);
  queryClient.setQueryData(["asks", issue.key], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue(answeredAsk);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: anchoredAsk,
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
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  // The inbox lists open asks, and this one has just been answered.
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([askRow(answeredAsk)]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByRole("heading", { name: "Needs you" });
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await screen.findByRole("radio", { name: "Ship" }));
    await user.click(screen.getByRole("button", { name: "Answer" }));
    const field = await screen.findByRole("textbox", { name: "Reply" });
    await user.click(field);
    await user.paste("Draft survives");

    // The Comments tab unmounts every ask card while another tab shows; a draft typed into one
    // belongs to the ask, not to that mounted field, and picks back up when the tab returns.
    await user.click(screen.getByRole("tab", { name: "Pinned" }));
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "Pinned" })).toHaveAttribute("aria-selected", "true")
    );
    await user.click(screen.getByRole("tab", { name: "Comments" }));
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "Comments" })).toHaveAttribute("aria-selected", "true")
    );
    const moved = screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement;
    // A fresh field - the tab switch really remounted the card - still carries the draft.
    expect(moved).not.toBe(field);
    expect(moved.value).toBe("Draft survives");
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    listIssueAsks.mockRestore();
    getIssue.mockRestore();
    getInbox.mockRestore();
  }
});

test("Margin leaves an unanchored root out of document review", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [unanchoredRootComment]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() =>
      expect(
        screen.queryByTestId(`margin-comment-${unanchoredRootComment.id}`)
      ).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
  }
});

test("a viewer who mounts after the answer sees the answered anchored ask", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
      mark_id: "m-3",
      orphaned: false,
      quote: "brown",
      version: 1,
    },
    answer: {
      at: "2026-09-10T00:01:00Z",
      selected: [],
      text: "Because it is precise.",
      user: "alice",
    },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "ask-answered",
    issue_key: issue.key,
    kind: "question",
    opened_event_id: 2,
    multiple: false,
    options: [],
    question: "Why brown?",
    state: "answered",
    urgency: "med",
  };
  // Open asks remain in the Needs you group before the historical answered ask, regardless of
  // their document positions.
  const openAsk: Ask = {
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
      mark_id: "m-4",
      orphaned: false,
      quote: "The",
      version: 1,
    },
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-09-10T00:02:00Z",
    edited_at: null,
    id: "ask-open",
    issue_key: issue.key,
    opened_event_id: 3,
    multiple: false,
    options: [],
    question: "Why the?",
    state: "open",
    kind: "question",
    urgency: "med",
  };
  // This viewer's tab never had either ask open in its inbox or query cache - unlike a tab
  // that was present when they were created, both queries start empty.
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const getAsk = answerAskThreads(answeredAsk, openAsk);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([
    askRow(answeredAsk),
    askRow(openAsk),
  ]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId(`ask-${answeredAsk.id}`);
    expect(listIssueAsks).toHaveBeenCalledWith(issue.key, "all");
    expect(card).toHaveTextContent("Answered by alice");
    await waitFor(() => expect(card).toHaveTextContent("Because it is precise."));
    expect(
      Array.from(
        screen
          .getByLabelText("Margin review items")
          .querySelectorAll<HTMLElement>("[data-margin-item]")
      ).map((item) => item.dataset.marginItem)
    ).toEqual([openAsk.id, answeredAsk.id]);
  } finally {
    view.unmount();
    listIssueAsks.mockRestore();
    getAsk.mockRestore();
  }
});

test("a reply to an ask renders exactly once in the margin, not also as a standalone comment", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const askReply: Comment = {
    anchor: null,
    ask_id: anchoredAsk.id,
    turn: "agent",
    author: { id: "alice", kind: "user" },
    body: "Any blockers first?",
    created_at: "2026-09-09T00:01:00Z",
    id: "ask-reply-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
    ...commentDeliveryFields(),
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [askReply]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: anchoredAsk,
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [askReply],
  });
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("ask-ask-1");
    await screen.findByTestId(`thread-${anchoredAsk.id}`);
    await waitFor(() => expect(screen.getAllByText("Any blockers first?")).toHaveLength(1));
    expect(screen.queryByTestId(`margin-comment-${askReply.id}`)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

test("composeForMark opens the composer with the mark anchor and resolves when it saves", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  // The send refetches the issue, its inbox and its comments; each answers the seeded set.
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(composer).getByText("selected")).toBeTruthy();
    await user.click(within(composer).getByLabelText("Comment"));
    await user.paste("why?");
    await user.click(within(composer).getByRole("button", { name: "Send" }));

    await waitFor(() => {
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument();
    });
  } finally {
    view.unmount();
    createComment.mockRestore();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listComments.mockRestore();
  }
});

test("composeForMark rejects when the composer is dismissed unsaved", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    await screen.findByRole("form", { name: "Comment composer" });
    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(screen.getByLabelText("First composer outcome").textContent).toBe(
        "replaced by a newer composer"
      )
    );
    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect(within(composer).getByText("second")).toBeTruthy();
    await user.click(within(composer).getByLabelText("Comment"));
    await user.keyboard("{Escape}");
    await waitFor(() => {
      expect(screen.getByLabelText("Second composer outcome").textContent).toBe("composer closed");
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument();
    });
  } finally {
    view.unmount();
  }
});

function fakeBridge(
  retype: (markId: string, kind: string) => RetypeOutcome,
  artifactId = specArtifact.id
): {
  bridge: DocumentBridge;
  removed: string[];
  retyped: [string, string][];
} {
  const removed: string[] = [];
  const retyped: [string, string][] = [];
  return {
    bridge: stubDocumentBridge(artifactId, {
      removeMark(markId) {
        removed.push(markId);
      },
      retypeMark(markId, kind) {
        retyped.push([markId, kind]);
        return retype(markId, kind);
      },
    }),
    removed,
    retyped,
  };
}

function RegisterBridge({ bridge }: { bridge: DocumentBridge }): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    registerDocument(bridge);
    return () => registerDocument(undefined);
  }, [bridge, registerDocument]);
  return null;
}

/** What a remounted `ProofDocument` does (a new artifact): the old editor unregisters and a fresh
 *  one registers, while the margin and its open composer stay. It does so at once, or in two
 *  steps, with the fresh editor still binding between them. */
function ReopenDocument({ bridge }: { bridge: DocumentBridge }): ReactNode {
  const { registerDocument } = useMargin();
  return (
    <>
      <button
        onClick={() => {
          registerDocument(undefined);
          registerDocument(bridge);
        }}
        type="button"
      >
        Reopen the document
      </button>
      <button onClick={() => registerDocument(undefined)} type="button">
        Close the document's editor
      </button>
      <button onClick={() => registerDocument(bridge)} type="button">
        Bind the reopened editor
      </button>
    </>
  );
}

/** The query client a margin test renders under, which a test can seed before the margin draws. */
function marginQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
}

function renderMarginWithBridge(
  bridge: DocumentBridge,
  reopened?: DocumentBridge,
  queryClient = marginQueryClient()
) {
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  // A send from this margin refetches the issue, its inbox and its comments; each answers the
  // seeded set, so the margin stays as the test set it up.
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
  ];
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterBridge bridge={bridge} />
          {reopened === undefined ? null : <ReopenDocument bridge={reopened} />}
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    ...view,
    unmount: () => {
      view.unmount();
      for (const read of reads) read.mockRestore();
    },
  };
}

test("the composer's kind switch retypes its mark through the document and sends under the new mark", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) =>
    kind === "suggestion"
      ? { refused: "unmarkable" }
      : { markId: `${markId}-${kind}`, placement: null, quote: "selected" }
  );
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const view = renderMarginWithBridge(fake.bridge);

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    const kinds = within(composer).getByRole("group", { name: "Kind" });

    await user.click(within(kinds).getByRole("button", { name: "Suggest" }));
    expect(fake.retyped).toEqual([["m-1", "suggestion"]]);
    expect(within(composer).getByRole("status").textContent).toBe(
      "A suggestion needs whole words inside one table cell. Comment or ask about this selection instead, or close this composer and select again."
    );
    expect(within(kinds).getByRole("button", { name: "Comment" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    await user.click(within(kinds).getByRole("button", { name: "Ask" }));
    expect(fake.retyped).toEqual([
      ["m-1", "suggestion"],
      ["m-1", "ask"],
    ]);
    await waitFor(() =>
      expect(within(kinds).getByRole("button", { name: "Ask" })).toHaveAttribute(
        "aria-pressed",
        "true"
      )
    );
    expect(within(composer).queryByRole("status")).not.toBeInTheDocument();

    await user.click(within(composer).getByLabelText("Question"));
    await user.paste("Why?");
    await user.click(
      within(composer).getAllByRole("button", { name: "Ask" }).at(-1) as HTMLElement
    );
    await waitFor(() => expect(createAsk).toHaveBeenCalledTimes(1));
    expect(createAsk.mock.calls[0]?.[1]).toMatchObject({
      anchor: { artifact: "artifact-1", mark_id: "m-1-ask" },
      question: "Why?",
    });
    expect(fake.removed).toEqual([]);
  } finally {
    view.unmount();
    createAsk.mockRestore();
  }
});

test("the margin removes the composer's mark when the composer is cancelled or replaced, in the document open then", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const reopened = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const view = renderMarginWithBridge(fake.bridge, reopened.bridge);

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(
      within(within(composer).getByRole("group", { name: "Kind" })).getByRole("button", {
        name: "Ask",
      })
    );
    await screen.findByLabelText("Question");

    // A newer composer takes the margin: the retyped mark, not the original, is removed.
    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() => expect(fake.removed).toEqual(["m-1-ask"]));

    // A fresh editor registers under the open composer.
    await user.click(screen.getByRole("button", { name: "Reopen the document" }));

    // Cancelling removes the mark the composer holds, in the document now open.
    await user.click(
      within(screen.getByRole("form", { name: "Comment composer" })).getByLabelText("Comment")
    );
    await user.keyboard("{Escape}");
    await waitFor(() => expect(reopened.removed).toEqual(["m-2"]));
    expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument();
    // The replaced document is told nothing more once the document reopened onto `reopened`.
    expect(fake.removed).toEqual(["m-1-ask"]);
  } finally {
    view.unmount();
  }
});

// While a document's editor is still binding, the margin already shows that document's compose
// (`useMarginSheet` follows the route), so the reader can close it before any bridge of that
// document is registered. Its mark is in that document alone: the margin takes it out once the
// document binds its editor again, and asks the editor that went nothing.
test("a compose closed while its document's editor is still binding takes its mark out once the editor binds", async () => {
  const user = userEvent.setup();
  const retype = (markId: string, kind: string) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  });
  const fake = fakeBridge(retype);
  const reopened = fakeBridge(retype);
  const view = renderMarginWithBridge(fake.bridge, reopened.bridge);
  const removed = () => ({ first: [...fake.removed], reopened: [...reopened.removed] });

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(screen.getByRole("button", { name: "Close the document's editor" }));
    // Nothing is typed, so Close ends the compose at once.
    await user.click(within(composer).getByRole("button", { name: "Close" }));
    await waitFor(() =>
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("composer closed")
    );
    const whileBinding = removed();
    await user.click(screen.getByRole("button", { name: "Bind the reopened editor" }));
    expect({ bound: removed(), whileBinding }).toEqual({
      bound: { first: [], reopened: ["m-1"] },
      whileBinding: { first: [], reopened: [] },
    });
  } finally {
    view.unmount();
  }
});

// A refused compose's Retry is the same compose sent again, to the same mark: the refusal gives
// way to the Retry's send, which is no Discard, so the mark stays in the document for it. Only
// the reader's own Discard of a refusal takes the mark out.
test("a refused compose's Retry keeps its mark and sends to it", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(new ApiError(400, { code: "INPUT_REFUSED", error: "refused" }))
    .mockResolvedValue({ body: "Kept", id: "comment-1" } as unknown as Comment);
  const view = renderMarginWithBridge(fake.bridge);

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(composer).getByLabelText("Comment"));
    await user.paste("Kept");
    await user.click(within(composer).getByRole("button", { name: "Send" }));
    const retry = await within(composer).findByRole("button", { name: "Retry" });
    await user.click(retry);
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]?.[1]).toMatchObject({
      anchor: { artifact: "artifact-1", mark_id: "m-1" },
    });
    expect(fake.removed).toEqual([]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A selection-bar action while the open compose's own send is out cannot take the margin, and
// the reader sees why: the open compose comes back on screen - here from under the Pinned tab -
// saying its send is still out, until that send lands.
test("a newer compose while the open compose's send is out brings that compose back, saying why", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => sent.promise);
  const view = renderMarginWithBridge(fake.bridge);

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(composer).getByLabelText("Comment"));
    await user.paste("why?");
    await user.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const pinned = screen.getByRole("tab", { name: "Pinned" });
    await user.click(pinned);
    await waitFor(() => expect(pinned).toHaveAttribute("aria-selected", "true"));

    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(screen.getByLabelText("Second composer outcome").textContent).toBe(
        "the open composer's send is out"
      )
    );
    expect(fake.removed).toEqual(["m-2"]);
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "Comments" })).toHaveAttribute("aria-selected", "true")
    );
    const shown = screen.getByRole("form", { name: "Comment composer" });
    expect(within(shown).getByText("selected")).toBeTruthy();
    expect(within(shown).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("why?");
    expect(
      screen.getByText("Still sending this one. Select the text again once it's sent.")
    ).toBeTruthy();

    await act(async () => {
      sent.resolve({ ...comment, body: "why?" });
      await sent.promise;
    });
    await waitFor(() => {
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    });
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

function SecondDocumentCompose(): ReactNode {
  const { composeForMark } = useMargin();
  const [outcome, setOutcome] = useState("idle");
  return (
    <>
      <button
        onClick={() => {
          void composeForMark({
            anchor: { artifact: "artifact-2", mark_id: "m-3", quote: "elsewhere" },
            kind: "comment",
          }).then(
            () => setOutcome("saved"),
            (error: unknown) => setOutcome(error instanceof Error ? error.message : String(error))
          );
        }}
        type="button"
      >
        Compose on the second document
      </button>
      <output aria-label="Second document composer outcome">{outcome}</output>
    </>
  );
}

// Each compose names its own send, so a send the reader left behind with its document holds no
// compose on the next one, and when it lands it closes and settles nothing there. Leaving with the
// send out hands the mark to the margin, which keeps it for the send: the editor's promise
// resolves rather than rejecting, which would take the mark back.
test("a compose after leaving a document mid-send opens, and that send's landing leaves it open", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === "CORE-2" ? secondIssue : issue
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => sent.promise);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <SecondDocumentCompose />
          <NavigateToSecondIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const first = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(first).getByLabelText("Comment"));
    await user.paste("why?");
    await user.click(within(first).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await user.click(screen.getByRole("button", { name: "Open second issue" }));
    await waitFor(() =>
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved")
    );

    await user.click(screen.getByRole("button", { name: "Compose on the second document" }));
    const second = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(second).getByText("elsewhere")).toBeTruthy();
    await user.click(within(second).getByLabelText("Comment"));
    await user.paste("Second draft");

    await act(async () => {
      sent.resolve({ ...comment, body: "why?" });
      await sent.promise;
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    const still = screen.getByRole("form", { name: "Comment composer" });
    expect(within(still).getByText("elsewhere")).toBeTruthy();
    expect(within(still).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("Second draft");
    expect(screen.getByLabelText("Second document composer outcome").textContent).toBe("idle");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    createComment.mockRestore();
  }
});

function NavigateToFirstIssue(): ReactNode {
  const navigate = useNavigate();
  return (
    <button
      onClick={() => navigate(buildIssuePath({ key: "CORE-1", kind: "issue" }))}
      type="button"
    >
      Back to the first issue
    </button>
  );
}

// A compose whose document the reader leaves while its send is out is held: its composer stays,
// hidden, and so does the mark the send names. A refusal while the reader is away hands the draft
// back to it, and coming back finds both, the open compose again.
test("a compose left mid-send keeps its draft, its refusal and its mark for the reader's return", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === "CORE-2" ? secondIssue : issue
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => sent.promise);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterBridge bridge={fake.bridge} />
          <MarkComposerProbe />
          <NavigateToSecondIssue />
          <NavigateToFirstIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const first = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(first).getByLabelText("Comment"));
    await user.paste("why?");
    await user.click(within(first).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole("button", { name: "Open second issue" }));
    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument()
    );
    await act(async () => {
      sent.reject(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
      await sent.promise.catch(() => {});
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    await user.click(screen.getByRole("button", { name: "Back to the first issue" }));
    const back = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(back).getByText("selected")).toBeTruthy();
    expect(within(back).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("why?");
    expect(within(back).getByText("Couldn't send — the server is down")).toBeTruthy();
    // The mark stayed for the send. The editor's promise resolved when the reader left: a
    // rejection would take the mark back.
    expect(fake.removed).toEqual([]);
    expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    createComment.mockRestore();
  }
});

// Two composes, each on its own document, each with its send out: the reader goes back and forth
// between the documents. Whichever document is shown, a newer selection-bar action there waits on
// that document's own send - never another document's, and never none - so its send's outcome
// lands on the compose and the mark it was sent from.
test("a newer compose on a document whose send is out waits on it, after visits to another document with a send out", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === "CORE-2" ? secondIssue : issue
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const firstSend = Promise.withResolvers<Comment>();
  const secondSend = Promise.withResolvers<Comment>();
  const sends = [firstSend.promise, secondSend.promise];
  const createComment = spyOn(api, "createComment").mockImplementation(() => {
    const next = sends.shift();
    if (next === undefined) throw new Error("no send left to answer");
    return next;
  });

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterBridge bridge={fake.bridge} />
          <MarkComposerProbe />
          <SecondDocumentCompose />
          <NavigateToSecondIssue />
          <NavigateToFirstIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const goTo = async (document: "first" | "second", quote: string) => {
    await user.click(
      screen.getByRole("button", {
        name: document === "first" ? "Back to the first issue" : "Open second issue",
      })
    );
    const shown = await screen.findByRole("form", { name: "Comment composer" });
    await waitFor(() => expect(within(shown).getByText(quote)).toBeTruthy());
  };

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const first = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(first).getByLabelText("Comment"));
    await user.paste("On the first");
    await user.click(within(first).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole("button", { name: "Open second issue" }));
    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: "Compose on the second document" }));
    const second = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(second).getByLabelText("Comment"));
    await user.paste("On the second");
    await user.click(within(second).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));

    await goTo("first", "selected");
    await goTo("second", "elsewhere");
    await goTo("first", "selected");

    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(screen.getByLabelText("Second composer outcome").textContent).toBe(
        "the open composer's send is out"
      )
    );
    expect(fake.removed).toEqual(["m-2"]);
    const shown = screen.getByRole("form", { name: "Comment composer" });
    expect(within(shown).getByText("selected")).toBeTruthy();
    expect(within(shown).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("On the first");
    expect(
      screen.getByText("Still sending this one. Select the text again once it's sent.")
    ).toBeTruthy();

    await act(async () => {
      firstSend.reject(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
      await firstSend.promise.catch(() => {});
    });
    const refused = await screen.findByText("Couldn't send — the server is down");
    const back = screen.getByRole("form", { name: "Comment composer" });
    expect(back.contains(refused)).toBe(true);
    expect(within(back).getByText("selected")).toBeTruthy();
    expect(within(back).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("On the first");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    createComment.mockRestore();
  }
});

// One compose is open per document (`composeForMark`): a compose on the second document leaves
// the first document's compose open, its mark with it, and a newer compose replaces only the one
// on its own document, whose editor is the one registered then. So the margin takes each replaced
// or cancelled compose's provisional mark out of that compose's own document, and never asks
// another document to remove it.
test("a compose on another document leaves the first open with its mark, and each newer compose takes out its own document's mark", async () => {
  const user = userEvent.setup();
  const retype = (markId: string, kind: string) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  });
  const first = fakeBridge(retype);
  const second = fakeBridge(retype, "artifact-2");
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === "CORE-2" ? secondIssue : issue
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterRouteDocument first={first.bridge} second={second.bridge} />
          <MarkComposerProbe />
          <SecondDocumentCompose />
          <NavigateToSecondIssue />
          <NavigateToFirstIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const removed = () => ({ first: [...first.removed], second: [...second.removed] });

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const opened = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(opened).getByLabelText("Comment"));
    await user.paste("On the first");

    await user.click(screen.getByRole("button", { name: "Open second issue" }));
    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: "Compose on the second document" }));
    const onSecond = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(onSecond).getByText("elsewhere")).toBeTruthy();
    // The first document's compose is still open, and neither document lost a mark.
    expect({
      outcome: screen.getByLabelText("First composer outcome").textContent,
      removed: removed(),
    }).toEqual({ outcome: "idle", removed: { first: [], second: [] } });

    await user.click(screen.getByRole("button", { name: "Back to the first issue" }));
    const back = await screen.findByRole("form", { name: "Comment composer" });
    await waitFor(() => expect(within(back).getByText("selected")).toBeTruthy());
    expect(within(back).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("On the first");

    // A newer compose on the first document replaces its compose, and its mark leaves that
    // document; the second document's compose stays open with its own.
    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(screen.getByLabelText("First composer outcome").textContent).toBe(
        "replaced by a newer composer"
      )
    );
    expect({
      outcome: screen.getByLabelText("Second document composer outcome").textContent,
      removed: removed(),
    }).toEqual({ outcome: "idle", removed: { first: ["m-1"], second: [] } });

    // Closing the second document's compose there takes its mark out of that document alone.
    await user.click(screen.getByRole("button", { name: "Open second issue" }));
    const stillOpen = await screen.findByRole("form", { name: "Comment composer" });
    await waitFor(() => expect(within(stillOpen).getByText("elsewhere")).toBeTruthy());
    await user.click(within(stillOpen).getByRole("button", { name: "Close" }));
    await waitFor(() =>
      expect(screen.getByLabelText("Second document composer outcome").textContent).toBe(
        "composer closed"
      )
    );
    expect(removed()).toEqual({ first: ["m-1"], second: ["m-3"] });
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

/** The line beside a newer compose that offers the draft kept for the words `quote`. */
const offerLine = (quote: string) =>
  `You wrote this draft for other words: “${quote}”. It is kept here:`;

// A newer compose on the same document is the host's move for the open one, which carries
// nothing: it opens a composer of its own with an empty field, read only while the first
// compose's draft waits beside it naming the words it was written for. Only the reader's own
// press puts that draft in the field, or drops it.
test("a newer compose on the document offers the open compose's draft beside it, and only the reader's press takes it", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const view = renderMarginWithBridge(fake.bridge);
  const shown = () => {
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByLabelText<HTMLTextAreaElement>("Comment");
    return {
      field: field.value,
      offered: within(composer).queryByText(offerLine("selected")) !== null,
      readOnly: field.readOnly,
      sendRefused:
        within(composer).queryByText("Use your draft or discard it before you send.") !== null,
    };
  };

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const first = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(first).getByLabelText("Comment"));
    await user.paste("why?");

    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(
        within(screen.getByRole("form", { name: "Comment composer" })).getByText("second")
      ).toBeTruthy()
    );
    await screen.findByText(offerLine("selected"));
    expect(shown()).toEqual({ field: "", offered: true, readOnly: true, sendRefused: true });

    await user.click(screen.getByRole("button", { name: "Use my draft" }));
    expect(shown()).toEqual({ field: "why?", offered: false, readOnly: false, sendRefused: false });

    // Taken, the draft is the second compose's: a newer compose offers it for those words, and
    // Discard my draft drops it with nothing put in the field.
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    await screen.findByText(offerLine("second"));
    await user.click(screen.getByRole("button", { name: "Discard my draft" }));
    await waitFor(() => expect(screen.queryByText(offerLine("second"))).toBeNull());
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByLabelText<HTMLTextAreaElement>("Comment");
    expect({
      field: field.value,
      quote: within(composer).queryByText("selected") !== null,
    }).toEqual({ field: "", quote: true });
  } finally {
    view.unmount();
  }
});

// A stopped send may have been delivered where it went, and a draft taken from it into another
// compose goes as a new message under a key of its own. So when a stopped compose gives way to a
// newer one on its document, the offer beside the newer compose says what the stopped send's
// note said, in the words a stopped send moved elsewhere carries (`SendMovedError`), before the
// reader can press Use my draft. Taken, the draft is carried here as the reader's own move carries
// a stopped send (`HeldSends.carryInto`), so the same words stay with Discard draft until this
// compose sends it or the reader discards it.
test("a stopped compose that gives way to a newer one is offered saying it may have been delivered, and says so until discarded", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: "selected",
  }));
  const queryClient = marginQueryClient();
  const owner = { issueKey: issue.key, kind: "issue" } as const;
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  jest.useFakeTimers();
  try {
    act(() => {
      heldSends(queryClient).send(marginComposeSendKey(specArtifact.id), {
        anchor: markComposerAnchor,
        ask: { multiple: false, options: [], urgency: "med" },
        draft: { body: "Stopped words", replacement: "" },
        edit: undefined,
        kind: "comment",
        owner,
        plainDelivery: "steer",
        replyTo: null,
        sendKey: "stopped-send",
      });
    });
    await stopTheLateWrite(queryClient);
  } finally {
    realClock();
  }
  const view = renderMarginWithBridge(fake.bridge, undefined, queryClient);
  const stoppedNote = new SendStoppedError().message;
  const movedWords = new SendMovedError({ owner, replyTo: null }).message;

  try {
    await screen.findByText(stoppedNote);
    await user.click(screen.getByRole("button", { name: "Compose second" }));
    const offer = (await screen.findByText(offerLine("selected"))).closest('[role="status"]');
    expect({
      draft: offer?.textContent?.includes("Stopped words") ?? false,
      stoppedNoteLeft: screen.queryByText(stoppedNote) !== null,
      stoppedWords: offer?.textContent?.includes(movedWords) ?? false,
    }).toEqual({ draft: true, stoppedNoteLeft: false, stoppedWords: true });

    await user.click(screen.getByRole("button", { name: "Use my draft" }));
    const taken = screen.getByRole("form", { name: "Comment composer" });
    expect({
      field: within(taken).getByLabelText<HTMLTextAreaElement>("Comment").value,
      offered: screen.queryByText(offerLine("selected")) !== null,
      stoppedWords: within(taken).queryByText(movedWords) !== null,
    }).toEqual({ field: "Stopped words", offered: false, stoppedWords: true });
    await user.click(within(taken).getByRole("button", { name: "Discard draft" }));
    expect({
      compose: screen.queryAllByRole("textbox", { name: "Comment" }).length,
      stoppedWords: screen.queryByText(movedWords) !== null,
    }).toEqual({ compose: 0, stoppedWords: false });
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A draft kept for an ask keeps its options and settings (`AwayDraft.ask`) and its kind, and Use
// my draft puts them back with its question. Offered beside a compose of another kind, the offer
// says the kind it was written as, so options it brings back are never hidden with no word. While
// the offer waits, the newer compose's own ask fields are read only, as its question is, so taking
// the draft writes over nothing the reader typed.
test("Use my draft puts back an ask's options and settings with its question, offered as an ask", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: markId.startsWith("m-1") ? "selected" : "second",
  }));
  const view = renderMarginWithBridge(fake.bridge);
  const composer = () => screen.getByRole("form", { name: "Comment composer" });
  const switchToAsk = async () => {
    const kinds = within(composer()).getByRole("group", { name: "Kind" });
    await user.click(within(kinds).getByRole("button", { name: "Ask" }));
    await within(composer()).findByLabelText("Question");
  };
  const askFields = () => {
    const shown = within(composer());
    return {
      description: shown.getByLabelText<HTMLInputElement>("Option 1 description").value,
      // A control inside a disabled fieldset is disabled, which its own property does not show.
      disabled: shown.getByLabelText("Option 1 label").closest("fieldset[disabled]") !== null,
      first: shown.getByLabelText<HTMLInputElement>("Option 1 label").value,
      high: shown.getByRole("button", { name: "High" }).getAttribute("aria-pressed"),
      multiple: shown.getByRole<HTMLInputElement>("checkbox", { name: "Allow multiple" }).checked,
      question: shown.getByLabelText<HTMLTextAreaElement>("Question").value,
      second: shown.queryByLabelText<HTMLInputElement>("Option 2 label")?.value ?? "(none)",
    };
  };

  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    await screen.findByRole("form", { name: "Comment composer" });
    await switchToAsk();
    const first = within(composer());
    await user.click(first.getByLabelText("Question"));
    await user.paste("Which one?");
    await user.click(first.getByRole("button", { name: "High" }));
    await user.click(first.getByRole("checkbox", { name: "Allow multiple" }));
    await user.click(first.getByLabelText("Option 1 label"));
    await user.paste("Ship");
    await user.click(first.getByLabelText("Option 1 description"));
    await user.paste("Proceed now");
    await user.click(first.getByRole("button", { name: "Add option" }));
    await user.click(first.getByLabelText("Option 2 label"));
    await user.paste("Hold");

    await user.click(screen.getByRole("button", { name: "Compose second" }));
    // The newer compose is a comment, so the offer names the kind the draft was written as.
    await screen.findByText(
      "You wrote this draft as an ask, for other words: “selected”. It is kept here:"
    );
    const asComment = screen.queryByText(offerLine("selected")) !== null;
    await switchToAsk();
    const asAsk = screen.queryByText(offerLine("selected")) !== null;
    const waiting = askFields();
    await user.click(screen.getByRole("button", { name: "Use my draft" }));
    expect({ asAsk, asComment, taken: askFields(), waiting }).toEqual({
      asAsk: true,
      asComment: false,
      taken: {
        description: "Proceed now",
        disabled: false,
        first: "Ship",
        high: "true",
        multiple: true,
        question: "Which one?",
        second: "Hold",
      },
      waiting: {
        description: "",
        disabled: true,
        first: "",
        high: "false",
        multiple: false,
        question: "",
        second: "(none)",
      },
    });
  } finally {
    view.unmount();
  }
});

// A closed issue refuses a suggestion, so a suggestion compose ends when its issue closes. One that
// offers a draft stays, showing it, until the reader drops it. It offers no Use my draft: taken
// into a field that cannot send, the draft would go with the compose once nothing is offered.
test("a suggestion compose that offers a draft when its issue closes stays with it, offers only Discard my draft, and ends once it is dropped", async () => {
  const user = userEvent.setup();
  const fake = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    placement: null,
    quote: markId.startsWith("m-1") ? "selected" : "second",
  }));
  const queryClient = marginQueryClient();
  const view = renderMarginWithBridge(fake.bridge, undefined, queryClient);
  try {
    await user.click(screen.getByRole("button", { name: "Compose first" }));
    const first = await screen.findByRole("form", { name: "Comment composer" });
    await user.click(within(first).getByLabelText("Comment"));
    await user.paste("why?");
    await user.click(screen.getByRole("button", { name: "Compose second" }));
    await screen.findByText(offerLine("selected"));
    const second = screen.getByRole("form", { name: "Comment composer" });
    const kinds = within(second).getByRole("group", { name: "Kind" });
    await user.click(within(kinds).getByRole("button", { name: "Suggest" }));
    await within(second).findByLabelText("Reason");
    act(() => {
      queryClient.setQueryData(["issue", issue.key], {
        ...issue,
        closed_at: "2026-09-09T01:00:00Z",
      });
    });
    // A closed issue's compose draws no kind switch.
    await waitFor(() => expect(screen.queryByRole("group", { name: "Kind" })).toBeNull());
    const closed = screen.getByRole("form", { name: "Comment composer" });
    const offer = within(closed)
      .getByText(/for other words: “selected”/)
      .closest('[role="status"]');
    expect({
      draft: offer?.textContent?.includes("why?") ?? false,
      drop: within(closed).queryByRole("button", { name: "Discard my draft" }) !== null,
      sendRefused:
        within(closed).queryAllByText("This issue is closed. Reopen it to send.").length > 0,
      take: within(closed).queryByRole("button", { name: "Use my draft" }) !== null,
    }).toEqual({ draft: true, drop: true, sendRefused: true, take: false });

    await user.click(within(closed).getByRole("button", { name: "Discard my draft" }));
    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull()
    );
  } finally {
    view.unmount();
  }
});

test("Margin links an orphaned ask to its original document version", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const orphanedAsk: Ask = {
    ...anchoredAsk,
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
      mark_id: "m-2",
      orphaned: true,
      quote: "Review",
      version: 1,
    },
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [orphanedAsk]);
  queryClient.setQueryData(["asks", issue.key], [orphanedAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const getAsk = answerAskThreads(orphanedAsk);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const link = await screen.findByRole("link", { name: "View original text" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/artifacts/spec?v=1&ask=ask-1");
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

test("the desktop margin takes no width on a route that has no margin", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  // This margin's issue is never seeded: it reads the issue, its asks and its comments, and
  // each of them is empty here.
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const inbox = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(screen.queryByTestId("desktop-margin-shell")).not.toBeInTheDocument();
    expect(screen.queryByRole("separator", { name: "Resize margin" })).not.toBeInTheDocument();
    expect(
      screen.queryByText("Open an issue or document to review its margin.")
    ).not.toBeInTheDocument();
  } finally {
    inbox.unmount();
  }

  const onIssue = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(await screen.findByTestId("desktop-margin-shell")).toBeTruthy();
  } finally {
    onIssue.unmount();
    getIssue.mockRestore();
    listIssueAsks.mockRestore();
    listComments.mockRestore();
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});

test("desktop margin resize handle supports keyboard adjustments and reset", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  function ResizableMargin(): ReactNode {
    const [width, setWidth] = useState(384);
    return <Margin onWidthChange={setWidth} width={width} />;
  }
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <ResizableMargin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const handle = await screen.findByRole("separator", { name: "Resize margin" });
    expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
      "width: 384px"
    );

    // The handle takes the key where it is: a click on it would start the drag its pointer
    // capture holds, which is not what this row is about.
    handle.focus();
    await user.keyboard("{ArrowLeft}");
    await waitFor(() =>
      expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
        "width: 408px"
      )
    );

    await user.dblClick(handle);
    await waitFor(() =>
      expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
        "width: 384px"
      )
    );
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});

test("margin keeps an ask draft through parent, width, and viewport layout updates", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
  const changeListeners: Array<(event: Event) => void> = [];
  let compact = false;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") {
          changeListeners.push(listener);
        }
      },
      addListener: () => {},
      dispatchEvent: () => true,
      get matches() {
        return query === "(max-width: 1279px)" && compact;
      },
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  function MarginHarness(): ReactNode {
    const [parentRender, setParentRender] = useState(0);
    const [width, setWidth] = useState(384);
    return (
      <>
        <button onClick={() => setParentRender((current) => current + 1)} type="button">
          Rerender parent
        </button>
        <output aria-label="Parent render">{parentRender}</output>
        <Margin onWidthChange={setWidth} width={width} />
      </>
    );
  }
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <OpenAskComposerButton />
          <MarginHarness />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No comments, asks, or suggestions on this document.");
    await user.click(screen.getByRole("button", { name: "Open ask composer" }));
    const question = await screen.findByLabelText("Question");
    await user.click(question);
    await user.paste("Keep this draft");

    await user.click(screen.getByRole("button", { name: "Rerender parent" }));
    expect(screen.getByLabelText("Parent render").textContent).toBe("1");
    expect((screen.getByLabelText("Question") as HTMLTextAreaElement).value).toBe(
      "Keep this draft"
    );

    screen.getByRole("separator", { name: "Resize margin" }).focus();
    await user.keyboard("{ArrowLeft}");
    await waitFor(() =>
      expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
        "width: 408px"
      )
    );
    expect((screen.getByLabelText("Question") as HTMLTextAreaElement).value).toBe(
      "Keep this draft"
    );

    act(() => {
      compact = true;
      for (const listener of changeListeners) {
        listener(new Event("change"));
      }
    });
    await waitFor(() =>
      expect(screen.queryByTestId("desktop-margin-shell")).not.toBeInTheDocument()
    );

    act(() => {
      compact = false;
      for (const listener of changeListeners) {
        listener(new Event("change"));
      }
    });
    await waitFor(() =>
      expect((screen.getByLabelText("Question") as HTMLTextAreaElement).value).toBe(
        "Keep this draft"
      )
    );
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});

const documentArtifact = {
  ...specArtifact,
  issue_key: null,
  primary: false,
};

function documentComment(id: string, body: string, replyTo: string | null = null): Comment {
  return {
    ...comment,
    anchor: {
      artifact_id: documentArtifact.id,
      block_id: null,
      mark_id: `${id}-mark`,
      orphaned: false,
      quote: body,
      version: 1,
    },
    body,
    id,
    issue_key: null,
    reply_to: replyTo,
  };
}

function renderDocumentMargin(comments: Comment[]) {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(
    ["artifact-ref", `${documentArtifact.project}/${documentArtifact.slug}`],
    documentArtifact
  );
  queryClient.setQueryData(["artifact", documentArtifact.id, "asks"], []);
  queryClient.setQueryData(["artifact", documentArtifact.id, "comments"], comments);
  const listArtifactComments = spyOn(api, "listArtifactComments").mockResolvedValue(comments);
  // Each card's edit reads that comment's blocks; none of these comments is indexed, so a save
  // carries no block ids.
  const commentBlocks = spyOn(api, "commentBlocks").mockImplementation(async (id) => ({
    blocks: [],
    indexed: false,
    item: graphNode({ id, kind: "comment" }),
  }));
  // A save refetches the margin's own reads: the document's asks and the viewer's inbox, both
  // empty here as the cache was seeded.
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listArtifactAsks = spyOn(api, "listArtifactAsks").mockResolvedValue([]);
  const view = render(
    <MemoryRouter
      initialEntries={[
        buildProjectPath({
          kind: "document",
          project: documentArtifact.project,
          slug: documentArtifact.slug,
        }),
      ]}
    >
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { commentBlocks, getInbox, listArtifactAsks, listArtifactComments, view };
}

async function expandThreadCard(user: UserEvent, card: HTMLElement): Promise<void> {
  const collapsed = card.querySelector<HTMLButtonElement>('button[aria-expanded="false"]');
  if (collapsed === null) {
    throw new Error("Expected a collapsed thread card.");
  }
  await user.click(collapsed);
}

async function saveMarginEdit(user: UserEvent, card: HTMLElement, body: string): Promise<void> {
  await expandThreadCard(user, card);
  await waitFor(() =>
    expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(1)
  );
  await user.click(within(card).getByRole("button", { name: "Edit" }));
  const field = screen.getByLabelText("Edit comment");
  await user.click(field);
  await user.keyboard("{Control>}a{/Control}");
  await user.paste(body);
  await user.click(screen.getByRole("button", { name: "Save" }));
}

test("Margin keeps sibling edit controls hidden while an owner save is pending", async () => {
  const user = userEvent.setup();
  const saving = documentComment("saving-comment", "Saving comment");
  const sibling = documentComment("sibling-comment", "Sibling comment");
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const { commentBlocks, getInbox, listArtifactAsks, listArtifactComments, view } =
    renderDocumentMargin([saving, sibling]);

  try {
    const savingCard = await screen.findByTestId(`margin-comment-${saving.id}`);
    await saveMarginEdit(user, savingCard, "Updated saving comment");
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(
        saving.id,
        { body: "Updated saving comment" },
        keyedAttempt
      )
    );

    const siblingCard = screen.getByTestId(`margin-comment-${sibling.id}`);
    await expandThreadCard(user, siblingCard);
    await waitFor(() =>
      expect(within(siblingCard).queryAllByRole("button", { name: "Edit" })).toHaveLength(0)
    );
    expect((within(siblingCard).getByLabelText("Reply") as HTMLTextAreaElement).disabled).toBe(
      false
    );

    await act(async () => {
      save.resolve(saving);
      await save.promise;
    });
    await waitFor(() =>
      expect(within(siblingCard).getAllByRole("button", { name: "Edit" })).toHaveLength(1)
    );
  } finally {
    save.resolve(saving);
    view.unmount();
    editComment.mockRestore();
    listArtifactComments.mockRestore();
    commentBlocks.mockRestore();
    getInbox.mockRestore();
    listArtifactAsks.mockRestore();
  }
});

test("Margin restores edit controls and the draft when an owner save rejects", async () => {
  const user = userEvent.setup();
  const root = documentComment("root-comment", "Root comment");
  const reply = documentComment("reply-comment", "Reply comment", root.id);
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const { commentBlocks, getInbox, listArtifactAsks, listArtifactComments, view } =
    renderDocumentMargin([root, reply]);

  try {
    const card = await screen.findByTestId(`margin-comment-${root.id}`);
    await expandThreadCard(user, card);
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    await user.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    const field = screen.getByLabelText("Edit comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Draft survives");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(root.id, { body: "Draft survives" }, keyedAttempt)
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect((screen.getByLabelText("Reply") as HTMLTextAreaElement).disabled).toBe(false);

    await act(async () => {
      save.reject(new Error("offline"));
      await Promise.resolve();
    });
    await screen.findByRole("alert");
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    expect((screen.getByLabelText("Edit comment") as HTMLTextAreaElement).value).toBe(
      "Draft survives"
    );
  } finally {
    save.resolve(root);
    view.unmount();
    editComment.mockRestore();
    listArtifactComments.mockRestore();
    commentBlocks.mockRestore();
    getInbox.mockRestore();
    listArtifactAsks.mockRestore();
  }
});
