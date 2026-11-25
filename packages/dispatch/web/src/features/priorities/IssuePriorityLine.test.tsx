import { expect, jest, spyOn, test } from "bun:test";
import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { listOf, panelOf, pickNamed, textBoxes } from "../../__tests__/list-picks";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { Issue, Priorities, PriorityRow } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { NoTooltips } from "../issue-page/NoTooltips";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { IssuePriorityLine } from "./IssuePriorityLine";

/**
 * The issue header's `Advances` item (INFRA-2947, R16 and R3): what it draws for a priority of
 * the issue's own and for one it inherits, what it draws for an issue that advances none, the
 * two writes the control makes, and what it says of one the reader stopped.
 */

function priority(id: string, title: string): PriorityRow {
  return {
    description: "",
    id,
    made_at: "2026-11-01T00:00:00Z",
    made_by: { id: "alice", kind: "user" },
    owner: "INFRA controller",
    rank: id,
    state: "live",
    title,
    work: {
      by_status: {},
      claimed: 0,
      closed_last_7_days: 0,
      newest_event_at: null,
      open_asks: 0,
    },
  };
}

/** The read's answer, in rank order: the line's order ("P2") is a row's place in this list. */
const body: Priorities = {
  priorities: [
    priority("evals-now", "Evals collecting data now"),
    priority("rocq-lean", "Rocq and Lean speed work restarted"),
    priority("oss-verify", "OSS Verify: curl, TLS and Rustls"),
  ],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

/** The revision of the issue's own set the line's read answered, which each press carries. */
const REVISION = 41;

// Queries are scoped to this render's container: bun runs every test file in one document, and
// a sibling file's un-unmounted render would otherwise leak its links into a `screen`-wide query.
// `inMain` mounts the line in the shell's main content, beside a control of the page after it;
// `page` draws it inside the new issue page's `NoTooltips`, through `queryClient` where given;
// `redraw` draws it again for the issue's `advances` a later read answers.
function renderLine(
  advances: Issue["advances"],
  {
    closed = false,
    inMain = false,
    page = false,
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  }: { closed?: boolean; inMain?: boolean; page?: boolean; queryClient?: QueryClient } = {}
) {
  const listPriorities = spyOn(api, "listPriorities").mockResolvedValue(body);
  const patchIssue = answerIssuePatches();
  patchIssue.mockResolvedValue({} as never);
  const invalidations = spyOn(queryClient, "invalidateQueries");
  const tree = (shown: Issue["advances"]) => {
    const drawn = (
      <IssuePriorityLine
        disabled={closed}
        issue={{ advances: shown, advances_revision: REVISION, key: "CORE-9" }}
      />
    );
    const line = page ? <NoTooltips>{drawn}</NoTooltips> : drawn;
    return (
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          {inMain ? (
            <main>
              {line}
              <button type="button">Elsewhere</button>
            </main>
          ) : (
            line
          )}
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const view = render(tree(advances));
  return {
    ...view,
    invalidations,
    listPriorities,
    patchIssue,
    q: within(view.container),
    queryClient,
    redraw: (shown: Issue["advances"]) => view.rerender(tree(shown)),
    unmountAll: () => {
      view.unmount();
      listPriorities.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

test("the line names the priority an issue advances, by its order and title, and links to its page", async () => {
  const view = renderLine([{ id: "rocq-lean", inherited: false }]);
  try {
    const link = await view.q.findByRole("link", {
      name: "P2 Rocq and Lean speed work restarted",
    });
    expect(link).toHaveAttribute("href", "/priorities/rocq-lean");
    expect(view.q.getByTestId("issue-advances")).toHaveTextContent("Advances");
    // The issue's own relation is the issue's to retract, so the entry carries the remove.
    view.q.getByRole("button", {
      name: "Remove P2 Rocq and Lean speed work restarted from CORE-9",
    });
    expect(view.q.queryByText("inherited")).toBeNull();
  } finally {
    view.unmountAll();
  }
});

test("a priority an issue inherits says so on the same line, and is not the child's to remove", async () => {
  const view = renderLine([{ id: "oss-verify", inherited: true }]);
  try {
    const link = await view.q.findByRole("link", { name: "P3 OSS Verify: curl, TLS and Rustls" });
    expect(link).toHaveAttribute("href", "/priorities/oss-verify");
    view.q.getByText("inherited");
    expect(
      view.q.queryByRole("button", {
        name: "Remove P3 OSS Verify: curl, TLS and Rustls from CORE-9",
      })
    ).toBeNull();
  } finally {
    view.unmountAll();
  }
});

test("an issue that advances none carries no line and no empty label, and reads no priority", async () => {
  const view = renderLine([]);
  try {
    expect(view.q.getByTestId("issue-advances")).not.toHaveTextContent("Advances");
    expect(view.q.queryByRole("link")).toBeNull();
    // Only the way to place the issue under one, which a person reaches from a fault row.
    view.q.getByRole("button", { name: "Add a priority to CORE-9" });
    // The list of priorities waits for that control: an issue advancing none reads nothing.
    await waitFor(() => expect(view.listPriorities).not.toHaveBeenCalled());
  } finally {
    view.unmountAll();
  }
});

test("the control adds a priority, writing the issue's own set as advances", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  try {
    await user.click(await view.q.findByRole("button", { name: "Add a priority to CORE-9" }));
    const picker = await view.q.findByRole("button", { name: pickNamed("Priority to advance") });
    await waitFor(() => expect(view.listPriorities).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(picker).toHaveTextContent("Choose a priority"));
    await user.click(picker);
    const list = within(panelOf(picker)).getByRole("listbox");
    // A priority the issue already advances is no choice; the rest are offered in rank order.
    expect(
      within(list)
        .getAllByRole("option")
        .map((option) => option.textContent)
    ).toEqual(["P2 Rocq and Lean speed work restarted", "P3 OSS Verify: curl, TLS and Rustls"]);
    await user.click(
      within(list).getByRole("option", { name: "P2 Rocq and Lean speed work restarted" })
    );
    await user.click(view.q.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    // The whole own set, with the revision of the read it was drawn from.
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-9",
      { advances: ["evals-now", "rocq-lean"], advances_revision: REVISION },
      keyedAttempt
    );
  } finally {
    view.unmountAll();
  }
});

test("a remove writes the issue's own set without that priority, and leaves an inherited one alone", async () => {
  const user = userEvent.setup();
  const view = renderLine([
    { id: "evals-now", inherited: false },
    { id: "rocq-lean", inherited: false },
    { id: "oss-verify", inherited: true },
  ]);
  try {
    await user.click(
      await view.q.findByRole("button", { name: "Remove P1 Evals collecting data now from CORE-9" })
    );
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-9",
      { advances: ["rocq-lean"], advances_revision: REVISION },
      keyedAttempt
    );
  } finally {
    view.unmountAll();
  }
});

// The race the revision closes: this tab read {evals-now}; another writer replaced evals-now with
// oss-verify in one whole-set write; the tab removes evals-now and sends []. Sent bare, the server
// would read that as one act and retract oss-verify, which this reader never saw. Sent with the
// revision this tab read, it is refused: the reader is told why, and the issue is read again so
// the next press carries the revision it holds now.
test("a remove carries the revision it was read at, and a stale one's refusal is shown", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  const refusal =
    "advances revision 41 is not the revision this issue holds, 57: read the issue again and write the set you mean against 57";
  view.patchIssue.mockRejectedValue(
    new ApiError(409, { code: "ADVANCES_REVISION", error: refusal })
  );
  try {
    await user.click(
      await view.q.findByRole("button", { name: "Remove P1 Evals collecting data now from CORE-9" })
    );
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-9",
      { advances: [], advances_revision: REVISION },
      keyedAttempt
    );
    expect((await view.q.findByRole("alert")).textContent).toBe(refusal);
    expect(view.invalidations).toHaveBeenCalledWith({ queryKey: ["issue"] });
  } finally {
    view.unmountAll();
  }
});

// A write the reader stopped may have been saved: the line says so, and that each press after it
// is a new write, since the line offers no Retry of it, rather than a failure it cannot know of.
test("a write the reader stopped says it may have been saved, not that it failed", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  view.patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-9", new TypeError("Failed to fetch"))
  );
  try {
    const remove = await view.q.findByRole("button", {
      name: "Remove P1 Evals collecting data now from CORE-9",
    });
    jest.useFakeTimers();
    await userOnFakeClock().click(remove);
    await stopTheLateWrite(view.queryClient);
    expect(view.q.getByRole("alert").textContent).toBe(
      stoppedWriteNote("what this issue advances", "saved", {
        anew: "It is read again, and adding or removing a priority sends a new write.",
      })
    );
  } finally {
    realClock();
    view.unmountAll();
    view.queryClient.clear();
  }
});

// Offline, a write of the line is tried under its key until Dispatch answers it: each try fails at
// the network, and the next waits out its backoff. At its deadline (`SEND_DEADLINE_MS`) the write
// goes late, still tried and now stoppable at the shell's indicator, and the line's hold ends
// there, so a write that never answers holds none of its controls for good, as the Legion daemons
// form's hold ends (`LegionDaemonsSection`). The Remove the reader pressed keeps the focus while
// it is held and once it is let go, under `emulateChromiumFocusFixup` throughout.
test("offline, a priority's Remove is held until its write's deadline, let go there, and keeps the focus throughout", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  view.patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-9", new TypeError("Failed to fetch"))
  );
  const name = "Remove P1 Evals collecting data now from CORE-9";
  const online = onlineManager.isOnline();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const remove = (await view.q.findByRole("button", { name })) as HTMLButtonElement;
    jest.useFakeTimers();
    onlineManager.setOnline(false);
    await pressHeld(userOnFakeClock(), remove, {
      name,
      sent: () => view.patchIssue.mock.calls.length,
    });

    // A second short of the deadline the write is still within it, and Remove still held.
    await tick(SEND_DEADLINE_MS - 1_000);
    expect(writesOf(view.queryClient).rows()[0]?.status).toBe("sending");
    expect(heldNotDisabled(remove, name)).toBe(true);
    expect(remove).toHaveFocus();

    // Past it the write is late and still tried, and Remove is let go with the focus on it.
    await tick(1_000);
    expect(writesOf(view.queryClient).rows()[0]?.status).toBe("late");
    expect(heldNotDisabled(remove, name)).toBe(false);
    expect(remove).toHaveFocus();
  } finally {
    // The line and its reads go before the network returns, so no read goes on after it.
    cleanup();
    view.unmountAll();
    view.queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
  }
});

// Inside `NoTooltips` the line holds its controls through the reads its write makes again. A
// write that lands just as the browser goes offline has those reads paused by the network, with
// no deadline of their own running: the app's client holds a read of an idle query until its
// paused fetch answers (`DispatchQueryClient`). So the line holds them only up to the request
// deadline (`REQUEST_DEADLINE_MS`), and lets them go there, with the focus on the Remove pressed.
test("offline, inside NoTooltips, a priority's Remove is held through the reads its landed write made only until the request deadline", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }], {
    page: true,
    queryClient: new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } }),
  });
  const answer = Promise.withResolvers<never>();
  view.patchIssue.mockImplementation(() => answer.promise);
  const name = "Remove P1 Evals collecting data now from CORE-9";
  const online = onlineManager.isOnline();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const remove = (await view.q.findByRole("button", { name })) as HTMLButtonElement;
    jest.useFakeTimers();
    await pressHeld(userOnFakeClock(), remove, {
      name,
      sent: () => view.patchIssue.mock.calls.length,
    });
    onlineManager.setOnline(false);
    await act(async () => answer.resolve({} as never));

    // A second short of the request deadline the reads are still paused, and Remove held.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(remove, name)).toBe(true);
    expect(remove).toHaveFocus();

    // At it the line lets Remove go, with the focus on it.
    await tick(1_000);
    expect(heldNotDisabled(remove, name)).toBe(false);
    expect(remove).toHaveFocus();
  } finally {
    cleanup();
    view.unmountAll();
    view.queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
  }
});

// Inside `NoTooltips` a write that lands while the reads it made stale go unanswered: the line
// holds its controls through them up to the request deadline, lets them go there, with the focus
// on the Remove pressed, and says why, in the words it says for a read of the priorities that
// failed and then the error such a read ends with. The line goes once those reads settle.
test("inside NoTooltips, a landed write whose reads have not answered by the request deadline lets Remove go there, and the line says why until they settle", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }], { page: true });
  const answer = Promise.withResolvers<never>();
  view.patchIssue.mockImplementation(() => answer.promise);
  const reread = Promise.withResolvers<Priorities>();
  const name = "Remove P1 Evals collecting data now from CORE-9";
  const why = "Could not load the priorities. Dispatch did not answer within 30 seconds.";
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const remove = (await view.q.findByRole("button", { name })) as HTMLButtonElement;
    view.listPriorities.mockImplementation(() => reread.promise);
    jest.useFakeTimers();
    await pressHeld(userOnFakeClock(), remove, {
      name,
      sent: () => view.patchIssue.mock.calls.length,
    });
    await act(async () => answer.resolve({} as never));
    await tick(0);
    expect(view.listPriorities).toHaveBeenCalledTimes(2);

    // A second short of the request deadline the priorities' read is still out, Remove held, and
    // nothing said.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(remove, name)).toBe(true);
    expect(view.q.queryByText(why)).toBeNull();

    // At it the line lets Remove go, with the focus on it, and says why.
    await tick(1_000);
    expect(heldNotDisabled(remove, name)).toBe(false);
    expect(remove).toHaveFocus();
    expect(view.q.getByRole("alert").textContent).toBe(why);

    // The read answers, and the line goes.
    await act(async () => reread.resolve(body));
    await tick(0);
    expect(view.q.queryByText(why)).toBeNull();
    expect(view.q.queryByRole("alert")).toBeNull();
  } finally {
    reread.resolve(body);
    cleanup();
    view.unmountAll();
    view.queryClient.clear();
    stopFocusFixup();
    realClock();
  }
});

test("a closed issue reads its priorities and writes none of them", async () => {
  const view = renderLine([{ id: "rocq-lean", inherited: false }], { closed: true });
  try {
    await view.q.findByRole("link", { name: "P2 Rocq and Lean speed work restarted" });
    expect(
      view.q.getByRole("button", {
        name: "Remove P2 Rocq and Lean speed work restarted from CORE-9",
      })
    ).toBeDisabled();
    expect(view.q.getByRole("button", { name: "Add a priority to CORE-9" })).toBeDisabled();
  } finally {
    view.unmountAll();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The issue header's Priority to advance is such a list: its options are every
// priority the issue does not advance yet, which grow with the priorities page. Opened, it holds
// a text box of its own, a word typed into it leaves only the priorities that match, and a pick
// stages that priority, with the focus back on the control and nothing written until Add.
test("the Priority to advance list holds a text box that narrows the priorities, and a pick stages the one Add writes", async () => {
  const user = userEvent.setup();
  const view = renderLine([]);
  const page = within(document.body);
  try {
    await user.click(view.q.getByRole("button", { name: "Add a priority to CORE-9" }));
    const before = new Set(textBoxes());
    await user.click(await view.q.findByRole("button", { name: pickNamed("Priority to advance") }));
    await waitFor(() =>
      expect(page.queryAllByRole("option", { name: /^P3 OSS Verify\b/ }).length).toBeGreaterThan(0)
    );
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the Priority to advance list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    await user.type(own[0] as HTMLElement, "oss");
    await waitFor(() =>
      expect(
        within(list).queryAllByRole("option", { name: /^P1 Evals\b/ }),
        `"oss" leaves P1 in the Priority to advance list`
      ).toHaveLength(0)
    );
    expect(
      within(list).queryAllByRole("option", { name: /^P2 Rocq\b/ }),
      `"oss" leaves P2 in the Priority to advance list`
    ).toHaveLength(0);
    await user.click(within(list).getByRole("option", { name: /^P3 OSS Verify\b/ }));
    const picker = view.q.getByRole("button", { name: pickNamed("Priority to advance") });
    expect(picker).toHaveFocus();
    expect(picker).toHaveAccessibleName("Priority to advance P3 OSS Verify: curl, TLS and Rustls");
    expect(view.patchIssue).not.toHaveBeenCalled();

    await user.click(view.q.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-9",
      { advances: ["oss-verify"], advances_revision: REVISION },
      keyedAttempt
    );
  } finally {
    view.unmountAll();
  }
});

// The `+` is replaced in its place by the form it opens, and its press takes the focus away with
// it. The form lands that focus on its own first control, the Priority to advance list's trigger,
// with no scroll, since the form stands where the `+` stood.
test("a press on Add a priority lands the focus on the Priority to advance list, scrolling nothing", async () => {
  const user = userEvent.setup();
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  const view = renderLine([{ id: "evals-now", inherited: false }], { inMain: true });
  try {
    await user.click(await view.q.findByRole("button", { name: "Add a priority to CORE-9" }));
    const picker = view.q.getByRole("button", { name: pickNamed("Priority to advance") });
    await afterFrameAndTasks();
    expect(picker).toHaveFocus();
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    view.unmountAll();
    scrolls.mockRestore();
  }
});

// A form drawn again with no press of the `+` takes no focus the reader holds elsewhere. A live
// update that fills the issue's own set up to `MAX_ISSUE_ADVANCES` takes the open form away, one
// that takes a priority off again draws it anew with a trigger of its own, and the focus stays on
// the page's control the reader moved it to.
test("the form drawn again by a live update leaves the focus on the control the reader holds", async () => {
  const user = userEvent.setup();
  const own = (count: number) =>
    Array.from({ length: count }, (_, index) => ({ id: `priority-${index}`, inherited: false }));
  const view = renderLine(own(MAX_ISSUE_ADVANCES - 1), { inMain: true });
  try {
    await user.click(view.q.getByRole("button", { name: "Add a priority to CORE-9" }));
    const first = view.q.getByRole("button", { name: pickNamed("Priority to advance") });
    await afterFrameAndTasks();
    expect(first).toHaveFocus();
    const elsewhere = view.q.getByRole("button", { name: "Elsewhere" });
    await user.click(elsewhere);
    expect(elsewhere).toHaveFocus();

    view.redraw(own(MAX_ISSUE_ADVANCES));
    expect(view.q.queryByRole("button", { name: pickNamed("Priority to advance") })).toBeNull();
    view.redraw(own(MAX_ISSUE_ADVANCES - 1));
    const again = view.q.getByRole("button", { name: pickNamed("Priority to advance") });
    expect(again).not.toBe(first);
    await afterFrameAndTasks();
    expect(elsewhere).toHaveFocus();
  } finally {
    view.unmountAll();
  }
});
