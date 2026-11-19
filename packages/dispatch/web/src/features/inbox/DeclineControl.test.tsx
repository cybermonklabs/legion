import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { ApiError, api, type DeclineAskInput } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { Ask, AskRead, InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { AskCard } from "./AskCard";
import { issueAsk } from "./inbox-fixture";

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
function renderCard(
  node: ReactNode,
  queryClient: QueryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  })
) {
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
    // The note is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads.
    await waitFor(() => expect(record.textContent).toContain(note));
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
  const { view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={async (_id, input) => {
        sent.push(input);
        return declined;
      }}
      getAskThread={async () => thread(declined)}
      initialThread={thread(open)}
    />
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
    // The note is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads.
    await waitFor(() => expect(record.textContent).toContain(note));
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
  const { view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={async (_id, input) => {
        sent.push(input);
        return declined;
      }}
      getAskThread={async () => thread(declined)}
      initialThread={thread(open)}
    />
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
  const { view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={() => out.promise}
      getAskThread={async () => thread(open)}
      initialThread={thread(open)}
    />
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

// A read of the thread asked before the decline was answered - an event's refresh of the ask,
// say - answers the question as it stood, open. Landing after the decline, it would draw the open
// question and its Decline to answer again over the record of the decline, until a read asked
// after the decline came back. On Dispatch's client a refresh waits for the running read rather
// than cancelling it, so the row reads on that client.
test("a read of the thread asked before a decline answered does not put back the open question", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  const declined = declinedAsk("");
  const reads: ReturnType<typeof Promise.withResolvers<AskRead>>[] = [];
  const getAskThread = () => {
    const read = Promise.withResolvers<AskRead>();
    reads.push(read);
    return read.promise;
  };
  const { queryClient, view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={async () => declined}
      getAskThread={getAskThread}
      initialThread={thread(open)}
    />,
    new DispatchQueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    })
  );
  const card = within(view.container);
  const badge = () => card.queryByTestId("ask-resolution-badge")?.textContent;
  try {
    const decline = await card.findByRole("button", { name: "Decline to answer" });
    let asked = Promise.resolve();
    await act(async () => {
      asked = queryClient.refetchQueries({ queryKey: ["ask-thread", open.id] });
    });
    expect(reads).toHaveLength(1);

    await user.click(decline);
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    await waitFor(() => expect(badge()).toBe("Declined"));

    // The read answers as Dispatch held the ask when it was asked: open.
    await act(async () => {
      reads[0]?.resolve(thread(open));
      await asked;
    });
    // The decline's answer gave that read up and asked for the thread again, after the decline.
    await waitFor(() => expect(reads).toHaveLength(2));
    expect(badge()).toBe("Declined");
    expect(card.queryByRole("button", { name: "Decline to answer" })).not.toBeInTheDocument();

    // It answers the ask declined. The decline's own refresh of the thread waited behind it, and
    // asks once it is in: Dispatch still holds the decline then.
    await act(async () => {
      reads[1]?.resolve(thread(declined));
    });
    await waitFor(() => expect(reads).toHaveLength(3));
    await act(async () => {
      reads[2]?.resolve(thread(declined));
    });
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(badge()).toBe("Declined");
    expect(card.queryByRole("button", { name: "Decline to answer" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

/** The Inbox's own rows, by id, as its list draws them from its one read (`inboxQuery`). */
function InboxRows(): ReactNode {
  const rows = useQuery(inboxQuery()).data;
  return <output data-testid="inbox-rows">{rows?.map((row) => row.id).join(" ")}</output>;
}

// A decline takes its row out of the Inbox at once. An Inbox read asked while the decline is out -
// an event's refresh, another surface's read - would reach Dispatch before the decline did, and
// bring the declined question back into the list while the decline is still going through. It
// waits for the decline's answer, and then reads what Dispatch holds after it.
test("an Inbox read asked while a decline is out waits for its answer, and brings no row back", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  const declined = declinedAsk("");
  const out = Promise.withResolvers<Ask>();
  const other = issueAsk({ id: "ask-2" });
  // What Dispatch lists before the decline is in, and after it: the declined question gone, and
  // a question asked since.
  let served: InboxRow[] = [issueAsk({ id: open.id }), other];
  const after = [other, issueAsk({ id: "ask-3" })];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  // Dispatch's own client (`main.tsx`), whose reads a write's holds keep back.
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const { view } = renderCard(
    <>
      <InboxRows />
      <AskCard
        ask={open}
        declineAsk={() => out.promise}
        getAskThread={async () => thread(declined)}
        initialThread={thread(open)}
      />
    </>,
    queryClient
  );
  const card = within(view.container);
  const listed = () => card.getByTestId("inbox-rows").textContent;
  try {
    await waitFor(() => expect(listed()).toBe("ask-1 ask-2"));
    expect(getInbox).toHaveBeenCalledTimes(1);
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    await waitFor(() => expect(listed()).toBe("ask-2"));

    await act(async () => {
      void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
    });
    // The read has started, and has asked Dispatch nothing.
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(queryClient.getQueryState(inboxQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(listed()).toBe("ask-2");

    served = after;
    await act(async () => {
      out.resolve(declined);
    });
    await waitFor(() => expect(listed()).toBe("ask-2 ask-3"));
    expect(getInbox.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(listed()).toBe("ask-2 ask-3");
  } finally {
    view.unmount();
    getInbox.mockRestore();
  }
});
