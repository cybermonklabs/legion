import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { heldNotDisabled } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { textBoxes } from "../../__tests__/list-picks";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { Issue, Priorities, PriorityRow } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { NoTooltips } from "../issue-page/NoTooltips";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { IssuePriorityLine } from "./IssuePriorityLine";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The chips of an issue's priorities in the issue header's state row (INFRA-2947, R16 and R3):
 * what it draws for a priority of the issue's own and for one it inherits, what it draws for an
 * issue that advances none, the writes its picker, the shared `MultiSelect`, makes when it
 * closes, and what it says of one the reader stopped.
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

/** The read's answer, in rank order: the line's order ("#2") is a row's place in this list. */
const body: Priorities = {
  priorities: [
    priority("evals-now", "Evals collecting data now"),
    priority("rocq-lean", "Rocq and Lean speed work restarted"),
    priority("oss-verify", "OSS Verify: curl, TLS and Rustls"),
  ],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

/** The revision of the issue's own set the line's read answered, which each write carries. */
const REVISION = 41;

// The line's own queries are scoped to this render's container: bun runs every test file in one
// document, and a sibling file's un-unmounted render would otherwise leak its links into a
// `screen`-wide query. The picker's list is portalled to the body, so it is found there by its
// own name, which no other surface carries, and read within itself. `page` draws the line inside
// the new issue page's `NoTooltips`, through `queryClient` where given.
function renderLine(
  advances: Issue["advances"],
  closed = false,
  read: Priorities = body,
  {
    page = false,
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  }: { page?: boolean; queryClient?: QueryClient } = {}
) {
  const listPriorities = spyOn(api, "listPriorities").mockResolvedValue(read);
  const patchIssue = answerIssuePatches();
  patchIssue.mockResolvedValue({} as never);
  const invalidations = spyOn(queryClient, "invalidateQueries");
  const line = (entries: Issue["advances"], revision: number) => {
    const drawn = (
      <IssuePriorityLine
        disabled={closed}
        issue={{ advances: entries, advances_revision: revision, key: "CORE-9" }}
      />
    );
    return (
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          {page ? <NoTooltips>{drawn}</NoTooltips> : drawn}
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const view = render(line(advances, REVISION));
  const q = within(view.container);
  return {
    ...view,
    invalidations,
    listPriorities,
    patchIssue,
    q,
    queryClient,
    /** The picker's list, once its trigger has opened it. */
    picker: () => screen.findByRole("listbox", { name: "Priorities options" }),
    /** The issue read again, with another set and revision, under the same line. */
    reread: (entries: Issue["advances"], revision: number) =>
      view.rerender(line(entries, revision)),
    trigger: () => q.getByRole("button", { name: "Edit priorities" }),
    unmountAll: () => {
      view.unmount();
      listPriorities.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

/** Each row the picker offers, in its order: the priority it names, and whether it is ticked. */
function offered(listbox: HTMLElement): [string, string | null][] {
  return within(listbox)
    .getAllByRole("option")
    .map((option) => [
      (option.textContent ?? "").replace("✓", ""),
      option.getAttribute("aria-selected"),
    ]);
}

test("the line names the priority an issue advances, by its order and title, and links to its page", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "rocq-lean", inherited: false }]);
  try {
    const link = await view.q.findByRole("link", {
      name: "#2 Rocq and Lean speed work restarted",
    });
    expect(link).toHaveAttribute("href", "/priorities/rocq-lean");
    expect(link).toHaveTextContent("#2");
    expect(view.q.getByRole("group", { name: "Advances" })).toContainElement(link);
    expect(view.q.queryByRole("link", { name: /inherited/ })).toBeNull();
    // The issue's own relation is the issue's to retract, so the picker holds it ticked.
    await user.click(view.trigger());
    const listbox = await view.picker();
    expect(
      within(listbox).getByRole("option", { name: "#2 Rocq and Lean speed work restarted" })
    ).toHaveAttribute("aria-selected", "true");
  } finally {
    view.unmountAll();
  }
});

test("a priority an issue inherits says so in its chip's name and tooltip, and is not the child's to remove", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "oss-verify", inherited: true }]);
  try {
    const link = await view.q.findByRole("link", {
      name: "#3 OSS Verify: curl, TLS and Rustls, inherited",
    });
    expect(link).toHaveAttribute("href", "/priorities/oss-verify");
    expect(link).toHaveAttribute(
      "title",
      "OSS Verify: curl, TLS and Rustls, inherited from an issue above"
    );
    // The picker offers no row for it: that relation is the ancestor's, and a row is a toggle.
    await user.click(view.trigger());
    const listbox = await view.picker();
    expect(offered(listbox)).toEqual([
      ["#1 Evals collecting data now", "false"],
      ["#2 Rocq and Lean speed work restarted", "false"],
    ]);
    await user.keyboard("{Escape}");
    expect(view.patchIssue).not.toHaveBeenCalled();
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
    expect(view.trigger()).toHaveAttribute("aria-haspopup", "listbox");
    // The list of priorities waits for that control: an issue advancing none reads nothing.
    await waitFor(() => expect(view.listPriorities).not.toHaveBeenCalled());
  } finally {
    view.unmountAll();
  }
});

// The issue header's state row holds the line after the status and the P0-P3 tag, beside the
// assignee, which is no room for a priority's title: each priority is a chip of its place alone,
// its title in its name and in the shared tooltip, and no word stands before them.
test("each priority is a chip whose visible text is its #N alone, and the line draws no Advances", async () => {
  const view = renderLine([
    { id: "rocq-lean", inherited: false },
    { id: "evals-now", inherited: true },
  ]);
  try {
    const own = await view.q.findByRole("link", { name: "#2 Rocq and Lean speed work restarted" });
    const inherited = view.q.getByRole("link", {
      name: "#1 Evals collecting data now, inherited",
    });
    expect(own.textContent).toBe("#2");
    expect(inherited.textContent).toBe("#1");
    expect(own).toHaveAttribute("title", "Rocq and Lean speed work restarted");
    expect(inherited).toHaveAttribute(
      "title",
      "Evals collecting data now, inherited from an issue above"
    );
    // Nothing of the line is a word: the two chips and the picker's "+".
    expect(view.container.textContent).toBe("#2#1+");
    expect(view.q.queryByText("Advances")).toBeNull();
  } finally {
    view.unmountAll();
  }
});

test("the picker adds a priority, writing the issue's own set as advances when it closes", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  try {
    await user.click(view.trigger());
    await waitFor(() => expect(view.listPriorities).toHaveBeenCalledTimes(1));
    expect(await screen.findByRole("combobox", { name: "Priority to advance" })).toHaveFocus();
    const listbox = await view.picker();
    // Every live priority in rank order, the one the issue advances already ticked.
    await waitFor(() =>
      expect(offered(listbox)).toEqual([
        ["#1 Evals collecting data now", "true"],
        ["#2 Rocq and Lean speed work restarted", "false"],
        ["#3 OSS Verify: curl, TLS and Rustls", "false"],
      ])
    );
    await user.click(
      within(listbox).getByRole("option", { name: "#2 Rocq and Lean speed work restarted" })
    );
    // A tick is a draft: the write is the close's, once.
    expect(view.patchIssue).not.toHaveBeenCalled();
    await user.click(view.trigger());
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
    await view.q.findByRole("link", { name: "#1 Evals collecting data now" });
    await user.click(view.trigger());
    const listbox = await view.picker();
    expect(offered(listbox)).toEqual([
      ["#1 Evals collecting data now", "true"],
      ["#2 Rocq and Lean speed work restarted", "true"],
    ]);
    await user.click(within(listbox).getByRole("option", { name: "#1 Evals collecting data now" }));
    await user.keyboard("{Escape}");
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

test("a close that leaves the set as it was writes nothing", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  try {
    await user.click(view.trigger());
    const listbox = await view.picker();
    const first = await within(listbox).findByRole("option", {
      name: "#1 Evals collecting data now",
    });
    await user.click(first);
    await user.click(first);
    await user.click(view.trigger());
    expect(screen.queryByRole("listbox", { name: "Priorities options" })).toBeNull();
    expect(view.patchIssue).not.toHaveBeenCalled();
  } finally {
    view.unmountAll();
  }
});

// The race the revision closes: this tab read {evals-now}; another writer replaced evals-now with
// oss-verify in one whole-set write; the tab removes evals-now and sends []. Sent bare, the server
// would read that as one act and retract oss-verify, which this reader never saw. Sent with the
// revision this tab read, it is refused: the reader is told why, and the issue is read again so
// the next write carries the revision it holds now.
test("a remove carries the revision it was read at, and a stale one's refusal is shown", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  const refusal =
    "advances revision 41 is not the revision this issue holds, 57: read the issue again and write the set you mean against 57";
  view.patchIssue.mockRejectedValue(
    new ApiError(409, { code: "ADVANCES_REVISION", error: refusal })
  );
  try {
    await user.click(view.trigger());
    const listbox = await view.picker();
    await user.click(
      await within(listbox).findByRole("option", { name: "#1 Evals collecting data now" })
    );
    await user.click(view.trigger());
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-9",
      { advances: [], advances_revision: REVISION },
      keyedAttempt
    );
    expect((await view.q.findByRole("alert")).textContent).toBe(refusal);
    // The line shows it (`inline`): nothing of it stands on the shell's line for a Retry, which
    // would send the stale revision again.
    expect(writesOf(view.queryClient).rows()).toEqual([]);
    expect(view.invalidations).toHaveBeenCalledWith({ queryKey: ["issue"] });
  } finally {
    view.unmountAll();
  }
});

// The same race inside one open picker: the draft is drawn from the read the picker opened on,
// so its write carries that read's revision even when the issue is read again before the close.
// Sent with the newer revision, a set drawn from the older read would land over a change this
// reader never saw.
test("a close writes the revision the draft was drawn from, though the issue was read again while it was open", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  try {
    await user.click(view.trigger());
    const listbox = await view.picker();
    await user.click(
      await within(listbox).findByRole("option", { name: "#2 Rocq and Lean speed work restarted" })
    );
    view.reread([{ id: "oss-verify", inherited: false }], 57);
    await user.click(view.trigger());
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-9",
      { advances: ["evals-now", "rocq-lean"], advances_revision: REVISION },
      keyedAttempt
    );
  } finally {
    view.unmountAll();
  }
});

test("a tick past the most priorities an issue advances is refused in the picker and written nowhere", async () => {
  const user = userEvent.setup();
  const many: Priorities = {
    ...body,
    priorities: Array.from({ length: MAX_ISSUE_ADVANCES + 1 }, (_, index) =>
      priority(`p-${index + 1}`, `Priority ${index + 1}`)
    ),
  };
  const own = Array.from({ length: MAX_ISSUE_ADVANCES }, (_, index) => ({
    id: `p-${index + 1}`,
    inherited: false,
  }));
  const view = renderLine(own, false, many);
  try {
    await user.click(view.trigger());
    const listbox = await view.picker();
    const last = `#${MAX_ISSUE_ADVANCES + 1} Priority ${MAX_ISSUE_ADVANCES + 1}`;
    await user.click(await within(listbox).findByRole("option", { name: last }));
    expect(within(listbox).getByRole("option", { name: last })).toHaveAttribute(
      "aria-selected",
      "false"
    );
    await screen.findByText(`An issue advances at most ${MAX_ISSUE_ADVANCES} priorities.`);
    await user.click(view.trigger());
    expect(view.patchIssue).not.toHaveBeenCalled();
  } finally {
    view.unmountAll();
  }
});

// A write the reader stopped may have been saved: the line says so, and that each press after it
// is a new write, since the line offers no Retry of it, rather than a failure it cannot know of.
test("a write the reader stopped says it may have been saved, not that it failed", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  view.patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-9", new TypeError("Failed to fetch"))
  );
  try {
    // A remove is an untick in the picker, written as it closes.
    await user.click(view.trigger());
    const listbox = await view.picker();
    await user.click(
      await within(listbox).findByRole("option", { name: "#1 Evals collecting data now" })
    );
    jest.useFakeTimers();
    await userOnFakeClock().keyboard("{Escape}");
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

/** Opens the picker, unticks the issue's one own priority and closes the picker with Escape under
 *  a held clock: the close writes the set, and the write is left out for the case to answer. The
 *  trigger then holds the focus and is held (`aria-disabled`, never `disabled`), and a press of it,
 *  by the keyboard and by the pointer, opens no picker and sends nothing more. */
async function closeAndHold(
  view: {
    picker: () => Promise<HTMLElement>;
    trigger: () => HTMLElement;
    patchIssue: { mock: { calls: readonly unknown[] } };
  },
  name: string,
  beforeClose?: () => void
): Promise<void> {
  const trigger = view.trigger() as HTMLButtonElement;
  const user = userEvent.setup();
  await user.click(trigger);
  const listbox = await view.picker();
  await user.click(
    await within(listbox).findByRole("option", { name: "#1 Evals collecting data now" })
  );
  jest.useFakeTimers();
  beforeClose?.();
  const held = userOnFakeClock();
  await held.keyboard("{Escape}");
  await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
  expect(trigger).toHaveFocus();
  expect(heldNotDisabled(trigger, name)).toBe(true);
  await held.keyboard("{Enter}");
  await held.click(trigger);
  expect(screen.queryByRole("listbox", { name: "Priorities options" })).toBeNull();
  expect(view.patchIssue).toHaveBeenCalledTimes(1);
  expect(trigger).toHaveFocus();
}

// Offline, a write of the line is tried under its key until Dispatch answers it: each try fails at
// the network, and the next waits out its backoff. At its deadline (`SEND_DEADLINE_MS`) the write
// goes late, still tried and now stoppable at the shell's indicator, and the line's hold ends
// there, so a write that never answers holds none of its controls for good, as the Legion daemons
// form's hold ends (`LegionDaemonsSection`). The picker's trigger keeps the focus while it is
// held and once it is let go, under `emulateChromiumFocusFixup` throughout.
test("offline, the priorities picker's trigger is held until its write's deadline, let go there, and keeps the focus throughout", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  view.patchIssue.mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-9", new TypeError("Failed to fetch"))
  );
  const name = "Edit priorities";
  const online = onlineManager.isOnline();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const trigger = view.trigger() as HTMLButtonElement;
    await closeAndHold(view, name, () => onlineManager.setOnline(false));

    // A second short of the deadline the write is still within it, and the trigger still held.
    await tick(SEND_DEADLINE_MS - 1_000);
    expect(writesOf(view.queryClient).rows()[0]?.status).toBe("sending");
    expect(heldNotDisabled(trigger, name)).toBe(true);
    expect(trigger).toHaveFocus();

    // Past it the write is late and still tried, and the trigger is let go with the focus on it.
    await tick(1_000);
    expect(writesOf(view.queryClient).rows()[0]?.status).toBe("late");
    expect(heldNotDisabled(trigger, name)).toBe(false);
    expect(trigger).toHaveFocus();
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

// Inside `NoTooltips` the line holds its trigger through the reads its write makes again. A write
// that lands just as the browser goes offline has those reads paused by the network, with no
// deadline of their own running: the app's client holds a read of an idle query until its paused
// fetch answers (`DispatchQueryClient`). So the line holds the trigger only up to the request
// deadline (`REQUEST_DEADLINE_MS`), and lets it go there, with the focus on it.
test("offline, inside NoTooltips, the priorities picker's trigger is held through the reads its landed write made only until the request deadline", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }], false, body, {
    page: true,
    queryClient: new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } }),
  });
  const answer = Promise.withResolvers<never>();
  view.patchIssue.mockImplementation(() => answer.promise);
  const name = "Edit priorities";
  const online = onlineManager.isOnline();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const trigger = view.trigger() as HTMLButtonElement;
    await closeAndHold(view, name);
    onlineManager.setOnline(false);
    await act(async () => answer.resolve({} as never));

    // A second short of the request deadline the reads are still paused, and the trigger held.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(trigger, name)).toBe(true);
    expect(trigger).toHaveFocus();

    // At it the line lets the trigger go, with the focus on it.
    await tick(1_000);
    expect(heldNotDisabled(trigger, name)).toBe(false);
    expect(trigger).toHaveFocus();
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
// holds its trigger through them up to the request deadline, lets it go there, with the focus on
// it, and says why, in the words it says for a read of the priorities that failed and then the
// error such a read ends with. The line goes once those reads settle.
test("inside NoTooltips, a landed write whose reads have not answered by the request deadline lets the picker's trigger go there, and the line says why until they settle", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }], false, body, { page: true });
  const answer = Promise.withResolvers<never>();
  view.patchIssue.mockImplementation(() => answer.promise);
  const reread = Promise.withResolvers<Priorities>();
  const name = "Edit priorities";
  const why = "Could not load the priorities. Dispatch did not answer within 30 seconds.";
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const trigger = view.trigger() as HTMLButtonElement;
    await closeAndHold(view, name);
    view.listPriorities.mockImplementation(() => reread.promise);
    await act(async () => answer.resolve({} as never));
    await tick(0);
    expect(view.listPriorities).toHaveBeenCalledTimes(2);

    // A second short of the request deadline the priorities' read is still out, the trigger
    // held, and nothing said.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(trigger, name)).toBe(true);
    expect(view.q.queryByText(why)).toBeNull();

    // At it the line lets the trigger go, with the focus on it, and says why.
    await tick(1_000);
    expect(heldNotDisabled(trigger, name)).toBe(false);
    expect(trigger).toHaveFocus();
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

// Inside `NoTooltips` the line a write's reads the bound ended draws is that write's own: the next
// write takes it away as it lands, though the earlier reads are still out, and that write's reads
// answering say nothing more. Dispatch holds the first write's read of the priorities until the
// row answers it, so the next write's read, which waits for that one (`DispatchQueryClient`),
// answers after it.
test("inside NoTooltips, the line a write's reads the bound ended draws goes as the next write lands, whose reads answer", async () => {
  const view = renderLine([{ id: "evals-now", inherited: false }], false, body, {
    page: true,
    queryClient: new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } }),
  });
  // Each write's answer, held until the row gives it, so what it sets off runs in `act`.
  const answers: ((answer: never) => void)[] = [];
  view.patchIssue.mockImplementation(() => {
    const { promise, resolve } = Promise.withResolvers<never>();
    answers.push(resolve);
    return promise;
  });
  const reread = Promise.withResolvers<Priorities>();
  const name = "Edit priorities";
  const why = "Could not load the priorities. Dispatch did not answer within 30 seconds.";
  try {
    const trigger = view.trigger() as HTMLButtonElement;
    await closeAndHold(view, name);
    view.listPriorities.mockImplementationOnce(() => reread.promise);
    await act(async () => answers[0]?.({} as never));
    await tick(0);
    expect(view.listPriorities).toHaveBeenCalledTimes(2);
    await tick(REQUEST_DEADLINE_MS);
    expect(heldNotDisabled(trigger, name)).toBe(false);
    expect(view.q.getByRole("alert").textContent).toBe(why);

    const user = userOnFakeClock();
    await user.click(trigger);
    const listbox = screen.getByRole("listbox", { name: "Priorities options" });
    await user.click(within(listbox).getByRole("option", { name: "#1 Evals collecting data now" }));
    await user.keyboard("{Escape}");
    await tick(0);
    expect(view.patchIssue).toHaveBeenCalledTimes(2);
    await act(async () => answers[1]?.({} as never));
    await tick(0);
    expect(heldNotDisabled(trigger, name)).toBe(true);
    expect(
      view.q.queryByText(why),
      "the earlier write's reads kept their line past the next write"
    ).toBeNull();

    await act(async () => reread.resolve(body));
    await tick(0);
    expect(heldNotDisabled(trigger, name)).toBe(false);
    expect(view.q.queryByRole("alert")).toBeNull();
  } finally {
    cleanup();
    view.unmountAll();
    view.queryClient.clear();
    realClock();
  }
});

test("a closed issue reads its priorities and writes none of them", async () => {
  const view = renderLine([{ id: "rocq-lean", inherited: false }], true);
  try {
    await view.q.findByRole("link", { name: "#2 Rocq and Lean speed work restarted" });
    expect(view.trigger()).toBeDisabled();
  } finally {
    view.unmountAll();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The issue header's priorities picker is such a list: its options are every live
// priority, which grow with the priorities page. Opened, it holds a text box of its own, and a
// word typed into it leaves only the priorities that match.
test("the priorities picker holds a text box that narrows the priorities", async () => {
  const user = userEvent.setup();
  const view = renderLine([{ id: "evals-now", inherited: false }]);
  try {
    const before = new Set(textBoxes());
    await user.click(view.trigger());
    const listbox = await view.picker();
    await waitFor(() =>
      expect(within(listbox).queryAllByRole("option", { name: /OSS Verify\b/ })).toHaveLength(1)
    );
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the priorities picker holds no text box of its own").toHaveLength(1);
    await user.type(own[0] as HTMLElement, "oss");
    await waitFor(() =>
      expect(
        within(listbox).queryAllByRole("option", { name: /Evals collecting data now$/ }),
        `"oss" leaves #1 in the priorities picker`
      ).toHaveLength(0)
    );
    expect(
      within(listbox).queryAllByRole("option", { name: /Rocq and Lean speed work restarted$/ }),
      `"oss" leaves #2 in the priorities picker`
    ).toHaveLength(0);
    expect(within(listbox).getAllByRole("option", { name: /OSS Verify\b/ })).toHaveLength(1);
  } finally {
    view.unmountAll();
  }
});
