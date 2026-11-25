import { expect, jest, spyOn, test } from "bun:test";
import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { Issue, Priorities, PriorityRow } from "../../api/types";
import { stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { IssuePriorityLine } from "./IssuePriorityLine";

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
  priority_work_in_backlog_or_icebox: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

/** The revision of the issue's own set the line's read answered, which each write carries. */
const REVISION = 41;

// The line's own queries are scoped to this render's container: bun runs every test file in one
// document, and a sibling file's un-unmounted render would otherwise leak its links into a
// `screen`-wide query. The picker's list is portalled to the body, so it is found there by its
// own name, which no other surface carries, and read within itself.
function renderLine(advances: Issue["advances"], closed = false, read: Priorities = body) {
  const listPriorities = spyOn(api, "listPriorities").mockResolvedValue(read);
  const patchIssue = answerIssuePatches();
  patchIssue.mockResolvedValue({} as never);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidations = spyOn(queryClient, "invalidateQueries");
  const line = (entries: Issue["advances"], revision: number) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssuePriorityLine
          disabled={closed}
          issue={{ advances: entries, advances_revision: revision, key: "CORE-9" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
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

test("a closed issue reads its priorities and writes none of them", async () => {
  const view = renderLine([{ id: "rocq-lean", inherited: false }], true);
  try {
    await view.q.findByRole("link", { name: "#2 Rocq and Lean speed work restarted" });
    expect(view.trigger()).toBeDisabled();
  } finally {
    view.unmountAll();
  }
});
