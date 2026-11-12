import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type DeclineAskInput, type WriteBounds } from "../../api/client";
import type { Ask, AskRead } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { AskCard } from "./AskCard";

/** Every decline a card of this file sends, answered by `how`: the card writes through the one
 *  write path, from its request alone (`askDeclineRequest`). */
function declinesWith(
  how: (id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>
): void {
  answerRoute<(id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>>(
    "POST",
    "/api/v1/asks/*/decline"
  ).mockImplementation(how);
}

let whoAmI: Mock<typeof api.whoAmI>;
let getReferences: Mock<typeof api.getReferences>;
let getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;

beforeEach(() => {
  whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "ask-1", kind: "ask" }),
  });
  // Every card with an issue names who the ask reaches, which reads the issue's subscribers;
  // nobody is subscribed here, so the card lists its followers alone.
  getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
});

afterEach(() => {
  whoAmI.mockRestore();
  getReferences.mockRestore();
  getIssueSubscribers.mockRestore();
});

/** An open question: the one ask the decline control is drawn on. */
function openAsk(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which release path?",
    state: "open",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

/** The same question, answered: a card that offers a change of answer, never a decline. */
function answeredAsk(): Ask {
  return openAsk({
    answer: {
      at: "2026-10-08T01:00:00Z",
      selected: ["Ship"],
      text: "First note",
      user: "alice",
    },
    state: "answered",
  });
}

/** An open approval request for version 12 of `spec.md`, which is answered with its two options
 *  and never declined. */
function approvalAsk(): Ask {
  return openAsk({
    approval: {
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 12,
      version: 12,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec-md" },
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 12)?",
  });
}

/** The four sentences Dispatch writes beside a person's note when they decline to answer
 *  (`model.DeclineDispatchWords`), written here as the server writes them. They are for the
 *  agent that asked, so the browser never draws them. */
const DISPATCH_WORDS =
  "The person chose not to answer this question. Decide the next step without an answer. " +
  "Do not ask this question again unless the facts change. Write in your work what you assumed.";

/** The ask as the server hands it back once the person has declined it, with `note` as the
 *  reason they wrote; an empty note leaves an empty reason. */
function declinedAsk(note: string): Ask {
  return openAsk({
    resolution: {
      actor: { id: "alice", kind: "user" },
      at: "2026-10-08T03:00:00Z",
      dispatch_words: DISPATCH_WORDS,
      kind: "declined",
      reason: note,
    },
    state: "resolved",
  });
}

function thread(ask: Ask): AskRead {
  return {
    answers: ask.answer === null ? [] : [ask.answer],
    ask,
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

/** The card under the application's key registry, since Escape reaches an open dialog through
 *  it (`KeymapProvider`, `useDialog`) and nothing else. */
function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>{node}</KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}

/** The sheet under the dialog, which a click anywhere on closes it. It is the dialog's own, not
 *  a card's: nothing else is drawn across the whole viewport. */
function backdrop(container: HTMLElement): Element {
  const found = container.querySelector('[aria-hidden="true"].fixed.inset-0');
  if (found === null) throw new Error("the decline dialog drew no backdrop");
  return found;
}

test("an open question offers Decline to answer, whoever holds the turn", async () => {
  for (const turn of ["human", "agent"]) {
    const ask = openAsk({ waiting_on: turn });
    const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
    const card = within(view.container);
    try {
      expect(await card.findByRole("button", { name: "Decline to answer" })).toBeTruthy();
    } finally {
      view.unmount();
    }
  }
});

test("an approval request offers no decline, which the server refuses", async () => {
  const ask = approvalAsk();
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    await card.findByRole("radio", { name: "Approve" });
    expect(card.queryByRole("button", { name: "Decline to answer" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("an answered ask offers no decline", async () => {
  const ask = answeredAsk();
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    await waitFor(() => expect(card.getByText(/Answered by/)).toBeTruthy());
    expect(card.queryByRole("button", { name: "Decline to answer" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("a declined ask shows Declined and the person's note, and offers no decline", async () => {
  const note = "Ask the release owner instead.";
  const ask = declinedAsk(note);
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    const record = await card.findByTestId("ask-ask-1");
    expect(within(record).getByTestId("ask-resolution-badge").textContent).toBe("Declined");
    expect(record.textContent).toContain("alice declined to answer");
    expect(record.textContent).toContain(note);
    // Dispatch's own words are the asking agent's to read, never the browser's.
    expect(record.textContent).not.toContain("The person chose not to answer");
    expect(card.queryByRole("button", { name: "Decline to answer" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("a decline sends the person's note and closes the question", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  const note = "The release owner decides this one.";
  const declined = declinedAsk(note);
  const sent: DeclineAskInput[] = [];
  declinesWith(async (_id, input) => {
    sent.push(input);
    return declined;
  });
  const { view } = renderCard(
    <AskCard ask={open} getAskThread={async () => thread(declined)} initialThread={thread(open)} />
  );
  const card = within(view.container);
  try {
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByLabelText("Note (optional)"));
    await user.paste(note);
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));

    await waitFor(() => expect(sent).toEqual([{ note }]));
    const record = await card.findByTestId("ask-ask-1");
    expect(within(record).getByTestId("ask-resolution-badge").textContent).toBe("Declined");
    expect(record.textContent).toContain(note);
    expect(card.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("the decline dialog counts what is left to write, and sends no note when empty", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  const declined = declinedAsk("");
  const sent: DeclineAskInput[] = [];
  declinesWith(async (_id, input) => {
    sent.push(input);
    return declined;
  });
  const { view } = renderCard(
    <AskCard ask={open} getAskThread={async () => thread(declined)} initialThread={thread(open)} />
  );
  const card = within(view.container);
  try {
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    const field = within(dialog).getByLabelText("Note (optional)");
    expect(field).toHaveAttribute("maxlength", String(DISPATCH_EVENT_BODY_MAX));
    expect(within(dialog).getByText(`${DISPATCH_EVENT_BODY_MAX} characters left`)).toBeTruthy();

    await user.click(field);
    await user.paste("Four");
    expect(within(dialog).getByText(`${DISPATCH_EVENT_BODY_MAX - 4} characters left`)).toBeTruthy();

    await user.clear(field);
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));

    await waitFor(() => expect(sent).toEqual([{}]));
    const record = await card.findByTestId("ask-ask-1");
    expect(record.textContent).toContain("alice declined to answer");
    // A decline with no note holds no reason, so the line ends with the act itself.
    expect(record.textContent).not.toContain("declined to answer -");
  } finally {
    view.unmount();
  }
});

// Escape and the backdrop close the dialog as Cancel does, and Cancel is disabled while the
// decline is out. The two must refuse with it: a close lets the write's hold go, and the dialog
// is the one surface the refusal is shown on, so a decline the server refuses after a press on
// Escape would leave the person with no word of it.
test("a decline out keeps its dialog through Escape and the backdrop, and shows the refusal", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  // The decline the server has not answered yet, refused by hand once the dialog has stood
  // through both ways out of it.
  const out = Promise.withResolvers<Ask>();
  declinesWith(() => out.promise);
  const { view } = renderCard(
    <AskCard ask={open} getAskThread={async () => thread(open)} initialThread={thread(open)} />
  );
  const card = within(view.container);
  try {
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Declining…" })));

    await user.keyboard("{Escape}");
    expect(card.getByRole("dialog", { name: "Decline to answer" })).toBeTruthy();
    await user.click(backdrop(view.container));
    expect(card.getByRole("dialog", { name: "Decline to answer" })).toBeTruthy();

    out.reject(
      new ApiError(409, { code: "ASK_NOT_DECLINABLE", error: "This question is already closed." })
    );
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "This question is already closed."
    );
    // The question is still the person's to answer: the refusal left the card where it was.
    expect(card.getByRole("dialog", { name: "Decline to answer" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});
