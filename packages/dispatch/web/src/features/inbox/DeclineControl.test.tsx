import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: one row needs a write that passes maxLength.
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, type DeclineAskInput, NetworkError } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type { Ask, AskRead, DispatchEvent, InboxRow } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { stoppedWriteNote } from "../writes/keyed-write";
import { AskCard } from "./AskCard";
import { issueAsk } from "./inbox-fixture";
import { AskDeclineWritesContext, useOutstandingAnswerWrites } from "./outstanding-answers";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

/** An Inbox's declines around `children`, as the Inbox provides them: a card under it makes its
 *  decline through the Inbox's write of its ask. */
function InboxDeclines({ children }: { children: ReactNode }): ReactNode {
  return (
    <AskDeclineWritesContext value={useOutstandingAnswerWrites()}>
      {children}
    </AskDeclineWritesContext>
  );
}

/** Declines an open question on its card, drawn inside an Inbox's declines where `inbox` says
 *  so, the decline never answering, and checks the dialog and its opener are held until the
 *  write's deadline and no longer, and that a press then declines again. */
async function declineLetsGo(inbox: boolean): Promise<void> {
  const ask = openAsk();
  const decline = spyOn(api, "declineAsk").mockRejectedValue(
    new NetworkError("/api/v1/asks/ask-1/decline", new TypeError("Failed to fetch"))
  );
  const card = <AskCard ask={ask} initialThread={thread(ask)} />;
  const { queryClient, view } = renderCard(inbox ? <InboxDeclines>{card}</InboxDeclines> : card);
  const drawn = within(view.container);
  try {
    const prepare = userEvent.setup();
    const opener = await drawn.findByRole("button", { name: "Decline to answer" });
    await prepare.click(opener);
    const dialog = within(drawn.getByRole("dialog", { name: "Decline to answer" }));
    const button = dialog.getByRole<HTMLButtonElement>("button", { name: "Decline to answer" });
    const note = dialog.getByLabelText<HTMLTextAreaElement>("Note (optional)");
    const cancel = dialog.getByRole("button", { name: "Cancel" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([decline]);
    await heldUntilLate(user, button, {
      alongside: (held) => {
        expect(note.readOnly, "the note").toBe(held);
        expect(opener.getAttribute("aria-disabled"), "the opener").toBe(held ? "true" : null);
        expect(opener, "the opener").toHaveTextContent(held ? "Declining…" : "Decline to answer");
        expect(cancel.getAttribute("aria-disabled"), "Cancel").toBe(held ? "true" : null);
      },
      label: "Decline of “Which release path?”",
      name: "Decline to answer",
      queryClient,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent()).toBe(2);
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
    decline.mockRestore();
  }
}

test("Decline to answer lets go at the decline write deadline and sends again", () =>
  declineLetsGo(false));

// Inside an Inbox the decline is the Inbox's write of the ask, which holds the dialog as the
// card's own does: until its deadline and no longer.
test("Decline to answer in an Inbox lets go at the decline write deadline and sends again", () =>
  declineLetsGo(true));

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

// A pick writes into the box without typing, so a note can pass the bound the field's own
// `maxLength` holds typing to. Both ways out of the box answer to one test of that: the button
// and Ctrl+Enter, which has no form to submit and so reaches `onFormlessSubmit`.
test("a note past the bound sends by neither the button nor Ctrl+Enter", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  const sent: DeclineAskInput[] = [];
  const { view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={async (_id, input) => {
        sent.push(input);
        return declinedAsk("");
      }}
      getAskThread={async () => thread(declinedAsk(""))}
      initialThread={thread(open)}
    />
  );
  const card = within(view.container);
  try {
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    const field = within(dialog).getByLabelText("Note (optional)") as HTMLTextAreaElement;
    // What a pick does and typing cannot: a write that passes the box's own `maxLength`, which
    // user-event honours as a browser does, so there is no user-event form for this one.
    fireEvent.change(field, { target: { value: "x".repeat(DISPATCH_EVENT_BODY_MAX + 5) } });
    expect(within(dialog).getByText("-5 characters left")).toBeTruthy();

    const decline = within(dialog).getByRole("button", { name: "Decline to answer" });
    expect(decline).toBeDisabled();
    await user.click(decline);
    await user.click(field);
    await user.keyboard("{Control>}{Enter}{/Control}");
    await user.keyboard("{Meta>}{Enter}{/Meta}");

    // The refused presses are not waited on: a send the box let through would reach
    // `declineAsk` through the write's own queue, and the in-bound send below goes through that
    // same queue. So the row brings the note back inside the bound, presses the keys again, and
    // waits on that one send to land: anything the refused presses let through would stand in
    // `sent` before it.
    fireEvent.change(field, { target: { value: "short" } });
    expect(within(dialog).getByText(`${DISPATCH_EVENT_BODY_MAX - 5} characters left`)).toBeTruthy();
    await user.click(field);
    await user.keyboard("{Control>}{Enter}{/Control}");

    await waitFor(() => expect(sent).toEqual([{ note: "short" }]));
  } finally {
    view.unmount();
  }
});

// Escape and the backdrop close the dialog as Cancel does, and Cancel is held while the decline
// is out. The two must refuse with it: a close lets the write's hold go, and the dialog
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

// While the decline is out, the dialog's note, Cancel and Decline to answer are held, never
// disabled: the browser takes the focus off a control that turns disabled, which would drop the
// keyboard reader who pressed Decline to answer onto the page behind the open dialog, where a Tab
// reaches the card. Held, the pressed control keeps the focus, a second press sends nothing, and
// the dialog's Tab cycle still holds all three, so a Tab and a Shift+Tab stay inside it.
test("Decline to answer keeps the focus inside its dialog while the decline is out, and a press then sends nothing", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  // The decline the server has not answered yet.
  const out = Promise.withResolvers<Ask>();
  const sent: DeclineAskInput[] = [];
  const { view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={(_id, input) => {
        sent.push(input);
        return out.promise;
      }}
      getAskThread={async () => thread(open)}
      initialThread={thread(open)}
    />
  );
  const card = within(view.container);
  try {
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    const note = within(dialog).getByLabelText("Note (optional)") as HTMLTextAreaElement;
    await user.click(note);
    await user.paste("Not ours to decide.");
    const decline = within(dialog).getByRole("button", {
      name: "Decline to answer",
    }) as HTMLButtonElement;
    await pressHeld(user, decline, { name: "Decline to answer", sent: () => sent.length });
    expect(sent).toEqual([{ note: "Not ours to decide." }]);
    const cancel = within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
    expect(heldNotDisabled(note, "the note")).toBe(true);
    expect(heldNotDisabled(cancel, "Cancel")).toBe(true);

    // Where the focus is, named, as a failed check prints it.
    const where = () => {
      const active = document.activeElement;
      if (active === note) return "the note";
      if (active === decline) return "Decline to answer";
      if (active === cancel) return "Cancel";
      return `${active?.tagName ?? "nothing"}, outside the dialog`;
    };
    await user.tab();
    expect(where()).toBe("the note");
    await user.tab({ shift: true });
    expect(where()).toBe("Decline to answer");
    await user.tab({ shift: true });
    expect(where()).toBe("Cancel");
    expect(card.getByRole("dialog", { name: "Decline to answer" })).toBe(dialog);
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

/** The event alice's feed carries once Dispatch has written ask `id`: `type`, by her. */
function askEvent(type: "ask.asked_of" | "ask.resolved", id: string): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-10-08T03:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: { id },
    seq: 1,
    type,
  } as DispatchEvent;
}

// The Inbox lists the questions other people ask, so an Inbox read held until a decline's answer,
// however late, would keep back a question asked meanwhile that long. The decline's own event -
// `ask.resolved` for its ask - says Dispatch has taken it: the held read goes then, and lists the
// new question without the declined one while the decline's answer is still out. Another ask's
// decline, or a hand-on of this ask, leaves the read held.
test("an Inbox read held behind a decline goes on the decline's own event, and shows a question asked meanwhile before its answer", async () => {
  const user = userEvent.setup();
  const open = openAsk();
  const declined = declinedAsk("");
  const out = Promise.withResolvers<Ask>();
  const other = issueAsk({ id: "ask-2" });
  let served: InboxRow[] = [issueAsk({ id: open.id }), other];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
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
  const readInboxAgain = () => {
    void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
  };
  try {
    await waitFor(() => expect(listed()).toBe("ask-1 ask-2"));
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    await waitFor(() => expect(listed()).toBe("ask-2"));

    // Dispatch has not taken the decline, so a read that reached it now would list the question.
    await act(async () => {
      readInboxAgain();
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);

    // Neither another ask's decline nor a hand-on of this one is this decline.
    await act(async () => {
      hearLiveEvent(queryClient, askEvent("ask.resolved", "ask-4"), "alice");
      hearLiveEvent(queryClient, askEvent("ask.asked_of", open.id), "alice");
      readInboxAgain();
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(listed()).toBe("ask-2");

    // Dispatch takes the decline, and somebody asks a new question meanwhile: the decline's event
    // comes, and the read it held goes, with the refresh the event asks for behind it.
    served = [other, issueAsk({ id: "ask-3" })];
    await act(async () => {
      hearLiveEvent(queryClient, askEvent("ask.resolved", open.id), "alice");
      readInboxAgain();
      await settle();
    });
    expect(getInbox.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(listed()).toBe("ask-2 ask-3"));
    // The decline is still out: its dialog stands until Dispatch answers it.
    expect(card.getByRole("dialog", { name: "Decline to answer" })).toBeInTheDocument();

    await act(async () => {
      out.resolve(declined);
    });
    await waitFor(() => expect(card.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(listed()).toBe("ask-2 ask-3");
  } finally {
    view.unmount();
    getInbox.mockRestore();
  }
});

// A decline the reader stopped may have been made: the dialog says so, and that its button is a
// press of its own, rather than a failure it cannot know of.
test("a decline the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const open = openAsk();
  const { queryClient, view } = renderCard(
    <AskCard
      ask={open}
      declineAsk={() =>
        Promise.reject(new NetworkError("/api/v1/asks/ask-1/decline", new TypeError("Failed")))
      }
      getAskThread={async () => thread(open)}
      initialThread={thread(open)}
    />
  );
  const card = within(view.container);
  try {
    // The who-am-I read settles here, in a wait that moves the held clock inside `act`. The
    // button below is found at once, and the wait for it ends before the read's answer lands,
    // which would then update the card outside `act`.
    await waitFor(() => expect(queryClient.getQueryState(["whoami"])?.status).toBe("success"));
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    await stopTheLateWrite(queryClient);
    expect(within(dialog).getByRole("alert").textContent).toBe(
      stoppedWriteNote("your decline", "made", {
        anew: "Pressing Decline to answer again sends a new decline.",
      })
    );
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
  }
});
