import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { realClock, tick } from "../../__tests__/fake-clock";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { answerAgentStates } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api, NetworkError } from "../../api/client";
import type {
  Agent,
  BroadcastExclusion,
  BroadcastRead,
  Message,
  MessageDelivery,
  UserAgentStates,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import { BroadcastPage } from "./BroadcastPage";
import { messageDeliveryFields } from "./delivery-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const agents: Agent[] = [
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    host: "build-host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: "planner-session",
    state: "live",
    title: "Planner",
  },
];

function attempt(overrides: Partial<MessageDelivery>): MessageDelivery {
  return {
    ...messageDeliveryFields(),
    attempt: 1,
    created_at: new Date().toISOString(),
    delivery: "steer",
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "planner-session",
    state: "pending",
    ...overrides,
  };
}

function broadcast(deliveries: MessageDelivery[]): BroadcastRead {
  return {
    author: { id: "alice", kind: "user" },
    body: "Report status.",
    created_at: new Date().toISOString(),
    delivery: "steer",
    id: "broadcast-1",
    recipients: [
      {
        message: {
          author: { id: "alice", kind: "user" },
          body: "Report status.",
          broadcast_id: "broadcast-1",
          created_at: new Date().toISOString(),
          deliveries,
          ...messageCorrectionFields(),
          id: "message-1",
          in_reply_to: null,
          issue_key: null,
          reply_block: null,
          target: "session:planner-session",
        },
        replies: [],
        session_id: "planner-session",
      },
    ],
  };
}

function renderBroadcast(
  read: BroadcastRead,
  state?: { excluded: readonly BroadcastExclusion[] },
  agentState: UserAgentStates = {},
  markRefused = false
) {
  const getBroadcast = spyOn(api, "getBroadcast").mockResolvedValue(read);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const createMessageDelivery = spyOn(api, "createMessageDelivery").mockResolvedValue(
    attempt({ attempt: 2, state: "sent" })
  );
  const getMyAgentState = spyOn(api, "getMyAgentState").mockResolvedValue(agentState);
  const putAgentState = markRefused
    ? answerAgentStates().mockRejectedValue(
        new ApiError(400, { code: "STATE_INPUT", error: "an old server's refusal" })
      )
    : answerAgentStates().mockResolvedValue({ unread_replies: 0 });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[{ pathname: "/agents/broadcasts/broadcast-1", state }]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<BroadcastPage />} path="/agents/broadcasts/:id" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createMessageDelivery,
    getBroadcast,
    putAgentState,
    queryClient,
    restore: () => {
      putAgentState.mockRestore();
      getMyAgentState.mockRestore();
      createMessageDelivery.mockRestore();
      listAgents.mockRestore();
      getBroadcast.mockRestore();
    },
    view,
  };
}

function threadMessage(id: string, author: Message["author"], body: string): Message {
  return {
    author,
    body,
    broadcast_id: null,
    created_at: new Date().toISOString(),
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: "message-1",
    issue_key: null,
    reply_block: null,
    target: null,
  };
}

/** A broadcast whose one recipient's thread holds two answers from the session and the viewer's
 *  own follow-up between them. */
function answeredBroadcast(): BroadcastRead {
  const read = broadcast([attempt({ envelope_id: "e1", state: "sent" })]);
  const [recipient] = read.recipients ?? [];
  if (recipient === undefined) throw new Error("the fixture has no recipient");
  const session = { id: "planner-session", kind: "session" } as const;
  return {
    ...read,
    recipients: [
      {
        ...recipient,
        replies: [
          threadMessage("answer-1", session, "Standing down."),
          threadMessage("follow-up", { id: "alice", kind: "user" }, "And the build?"),
          threadMessage("answer-2", session, "Green."),
        ],
      },
    ],
  };
}

// The page shows a recipient's whole thread, which holds the viewer's own follow-ups beside the
// session's answers. The read marks the session's messages alone: the server refuses an id the
// session did not write, and one such id in the list would leave every answer on the page unread.
test("opening a broadcast marks the recipient's answers read by id, and only the session's own", async () => {
  const page = renderBroadcast(answeredBroadcast(), undefined, {
    "planner-session": { unread_replies: 2 },
  });
  try {
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith(
        "planner-session",
        { read_replies: ["answer-1", "answer-2"] },
        expect.objectContaining({ key: expect.any(String) })
      )
    );
    expect(page.putAgentState).toHaveBeenCalledTimes(1);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A mark Dispatch refuses - an old server's 400 during a rolling deploy - is sent once and then
// the row waits for the unread count or the replies it shows to change. A refetch of the broadcast
// renders the same replies in a new array, as every render does, and sends nothing more.
test("a refused mark is sent once and then left alone, however often the row re-renders", async () => {
  jest.useFakeTimers();
  const page = renderBroadcast(
    answeredBroadcast(),
    undefined,
    { "planner-session": { unread_replies: 2 } },
    true
  );
  try {
    await waitFor(() => expect(page.putAgentState).toHaveBeenCalledTimes(1));
    await page.queryClient.invalidateQueries({ queryKey: ["broadcast"] });
    await tick(500);
    expect(page.putAgentState).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    page.view.unmount();
    page.restore();
  }
});

test("a broadcast page keeps the server exclusion reason as written", async () => {
  const serverExcluded = [
    {
      reason: "does not advertise btw",
      session_id: "reviewer-session",
      title: "Reviewer",
    },
  ] satisfies BroadcastExclusion[];
  const page = renderBroadcast(broadcast([attempt({ state: "sent" })]), {
    excluded: serverExcluded,
  });

  try {
    expect(
      await screen.findByText(
        (_, element) =>
          element?.textContent ===
          "Excluded: Reviewer (does not advertise btw). Nothing was sent to them."
      )
    ).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// LEGION-233 review, P2. A send a worker is still carrying must offer nothing: the mode-change
// buttons are a genuine second frame, and taking one while the worker was sending delivered the
// message twice.
test("a recipient whose send is still outstanding offers no retry at all", async () => {
  const page = renderBroadcast(broadcast([attempt({})]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).getByText(wholeText("Sending to Planner (Send)"))).toBeTruthy();
    // textContent, never the elements: a failed toEqual on DOM nodes serialises the whole tree
    // and can take minutes to report.
    expect(
      within(row)
        .queryAllByRole("button")
        .map((button) => button.textContent)
    ).toEqual([]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The other half: a process that died mid-delivery leaves a pending attempt nobody holds. Moving
// that recipient only by a delivery in a DIFFERENT mode would put a second frame wherever the
// first landed, so its own mode is the only one offered, and it is offered.
test("a pending attempt nobody is carrying offers a same-mode retry and no mode change", async () => {
  const stranded = attempt({ created_at: new Date(Date.now() - 5 * 60_000).toISOString() });
  const page = renderBroadcast(broadcast([stranded]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const buttons = within(row)
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(buttons).toEqual(["Retry"]);
    expect(
      within(row).getByText(
        (_, element) =>
          element?.textContent ===
          "Nobody is carrying this send. Retry uses Send again, which cannot deliver it twice unless the session restarted since it was sent."
      )
    ).toBeTruthy();
    const user = userEvent.setup();
    await user.click(within(row).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "steer", keyedAttempt)
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A stranded recipient's Retry is held while its retry is out: by `aria-disabled`, not
// `disabled`, which Chromium answers by taking the focus off the button (`pressHeld`). So the
// keyboard reader who pressed it stays on it, and a press of it then, by key or by pointer, sends
// nothing.
test("a stranded recipient's Retry keeps the keyboard focus while its retry is out, and a second press sends nothing", async () => {
  const stranded = attempt({ created_at: new Date(Date.now() - 5 * 60_000).toISOString() });
  const page = renderBroadcast(broadcast([stranded]));
  // Dispatch answers the retry only once the case lets it.
  const delivered = Promise.withResolvers<MessageDelivery>();
  page.createMessageDelivery.mockImplementation(() => delivered.promise);
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const retry = within(row).getByRole("button", { name: "Retry" });
    await pressHeld(userEvent.setup(), retry, {
      name: "Retry",
      sent: () => page.createMessageDelivery.mock.calls.length,
    });
    expect(retry).toBeEnabled();
    await act(async () => delivered.resolve(attempt({ attempt: 2, state: "sent" })));
    await waitFor(() => expect(retry).not.toHaveAttribute("aria-disabled"));
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test.each([
  { name: "Retry", mode: "Send", stranded: true },
  { name: "Retry", mode: "Aside", stranded: false },
  { name: "Use BTW instead", mode: "BTW", stranded: false },
  { name: "Use Send instead", mode: "Send", stranded: false },
] as const)("$name ($mode) lets go at the broadcast retry deadline and sends again", async ({
  name,
  mode,
  stranded,
}) => {
  const delivery = stranded
    ? attempt({ created_at: new Date(Date.now() - 5 * 60_000).toISOString() })
    : attempt({ delivery: "aside", error: "no live session planner-session", state: "failed" });
  const page = renderBroadcast(broadcast([delivery]));
  page.createMessageDelivery.mockRejectedValue(
    new NetworkError("/api/v1/messages", new TypeError("Failed to fetch"))
  );
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const button = within(row).getByRole<HTMLButtonElement>("button", { name });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([page.createMessageDelivery]);
    await heldUntilLate(user, button, {
      label: `Delivery to Planner again, as ${mode}`,
      name,
      queryClient: page.queryClient,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent()).toBe(2);
  } finally {
    realClock();
    page.view.unmount();
    page.queryClient.clear();
    page.restore();
  }
});

// No move of the reader's draws a recipient's row again within the page: the rows are keyed by
// their session and the page has no fold or tab. A broadcast read that fails does: the page
// draws its failure in place of every row (`BroadcastPage`), and the read that answers next
// draws the rows again, here while a Retry's write is still out. The Retry drawn again holds
// from that press's own state (`usePendingKeyedPress`), as the one pressed did: a press of it
// sends no second retry, and it lets go at the retry's deadline (`SEND_DEADLINE_MS`). Dispatch
// never answers the retry: each try fails at the network and is tried again under its key.
test("a recipient's Retry drawn again after a failed broadcast read while its retry's write is out holds until the retry's deadline and sends no second retry", async () => {
  const stranded = attempt({ created_at: new Date(Date.now() - 5 * 60_000).toISOString() });
  const page = renderBroadcast(broadcast([stranded]));
  page.createMessageDelivery.mockRejectedValue(
    new NetworkError("/api/v1/messages", new TypeError("Failed to fetch"))
  );
  const retryOf = () =>
    within(screen.getByRole("article", { name: "Planner" })).getByRole<HTMLButtonElement>(
      "button",
      { name: "Retry" }
    );
  const sent = () => pressesOf([page.createMessageDelivery]);
  try {
    await screen.findByRole("article", { name: "Planner" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(retryOf());
    await tick(0);
    expect(sent()).toBe(1);

    page.getBroadcast.mockRejectedValueOnce(
      new NetworkError("/api/v1/broadcasts/broadcast-1", new TypeError("Failed to fetch"))
    );
    await act(() => page.queryClient.invalidateQueries({ queryKey: ["broadcast"] }));
    await tick(0);
    expect(screen.queryByRole("article", { name: "Planner" })).toBeNull();
    await act(() => page.queryClient.invalidateQueries({ queryKey: ["broadcast"] }));
    await tick(0);
    expect(
      heldNotDisabled(retryOf(), "Retry"),
      "the Retry drawn again let go while its retry was out"
    ).toBe(true);
    await user.click(retryOf());
    await tick(0);
    expect(sent(), "a press of the Retry drawn again sent a second retry").toBe(1);

    await tick(SEND_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(retryOf(), "Retry")).toBe(true);
    await tick(1_000);
    expect(heldNotDisabled(retryOf(), "Retry")).toBe(false);
  } finally {
    realClock();
    page.view.unmount();
    page.queryClient.clear();
    page.restore();
  }
});

// The row's promise is the dashboard's one promise, and it expires with the same window: past it
// nothing recognises the resumed frame as a repeat (`DELIVERY_DUPLICATE_WINDOW_MS`).
test("a stranded send older than the duplicate window is not promised a single delivery", async () => {
  const aged = attempt({
    created_at: new Date(Date.now() - DELIVERY_DUPLICATE_WINDOW_MS - 60_000).toISOString(),
  });
  const page = renderBroadcast(broadcast([aged]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).queryByText(/cannot deliver it twice/)).not.toBeInTheDocument();
    expect(within(row).getByText(/may deliver it twice/)).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A settled failure is different: nothing is outstanding, so a mode change sends rather than
// duplicates, and the agent card's full retry row applies.
test("a failed attempt offers the same-mode retry and the mode changes", async () => {
  const failed = attempt({ error: "no live session", state: "failed" });
  const page = renderBroadcast(broadcast([failed]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const buttons = within(row)
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(buttons).toEqual(["Retry", "Use BTW instead"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a delivered recipient offers nothing", async () => {
  const page = renderBroadcast(broadcast([attempt({ envelope_id: "e1", state: "sent" })]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).getByText(wholeText("Sent to Planner (Send)"))).toBeTruthy();
    // The recipient is drawn by `AgentName`: its current name, its running state and a link to
    // its live conversation.
    const heading = within(row).getByRole("heading", { level: 3 });
    const recipient = heading.querySelector('[data-agent-name="planner-session"]');
    expect(recipient?.getAttribute("data-agent-state")).toBe("running");
    expect(within(heading).getByRole("link", { name: "Planner" })).toHaveAttribute(
      "href",
      "/agents/planner-session/live"
    );
    // textContent, never the elements: a failed toEqual on DOM nodes serialises the whole tree
    // and can take minutes to report.
    expect(
      within(row)
        .queryAllByRole("button")
        .map((button) => button.textContent)
    ).toEqual([]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// LEGION-540. The broadcast's body and each recipient's answer are Markdown their authors wrote,
// shown whole on this page: a numbered list reads as a list, not as one run-on line with its
// numbers dropped (the `inline` rendering this page used before), and no syntax shows.
test("the broadcast's body and a recipient's answer render as Markdown blocks, lists included", async () => {
  const read = answeredBroadcast();
  const session = { id: "planner-session", kind: "session" } as const;
  const listed: BroadcastRead = {
    ...read,
    body: "Three things:\n\n1. **Stop** the deploy\n2. Check `deploy.yml`",
    recipients: (read.recipients ?? []).map((recipient) => ({
      ...recipient,
      replies: [threadMessage("answer-1", session, "Done:\n\n- stopped\n- *green*")],
    })),
  };
  const page = renderBroadcast(listed);
  try {
    const heading = await screen.findByRole("heading", { name: "Broadcast to 1 agent" });
    const header = heading.closest("header");
    if (header === null) throw new Error("the broadcast has no header");
    await waitFor(() =>
      expect(Array.from(header.querySelectorAll("ol > li"), (item) => item.textContent)).toEqual([
        "Stop the deploy",
        "Check deploy.yml",
      ])
    );
    expect(header.querySelector("ol > li > strong")?.textContent).toBe("Stop");
    expect(header.querySelector("ol > li > code")?.textContent).toBe("deploy.yml");
    expect(header.textContent).not.toContain("**");

    const row = await screen.findByRole("article", { name: "Planner" });
    await waitFor(() =>
      expect(Array.from(row.querySelectorAll("ul > li"), (item) => item.textContent)).toEqual([
        "stopped",
        "green",
      ])
    );
    expect(row.querySelector("ul > li > em")?.textContent).toBe("green");
    expect(row.textContent).not.toContain("- stopped");
  } finally {
    page.view.unmount();
    page.restore();
  }
});
