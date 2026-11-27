/**
 * The Inbox page below the reading column's width (INFRA-5200): short rows, and the one row the
 * reader opens read in place of the list (`ReadingView`), with its way back through the Inbox
 * control, the browser's Back and Escape, and what answering, snoozing, assigning, marking and
 * the server's own moves do to it. Each row stays at happy-dom's own 1024 px, or says where it
 * moves the window (`movableReadingPaneWidth`). The column beside the list is `Inbox.test.tsx`'s.
 */
import { beforeAll, expect, spyOn, test } from "bun:test";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import { afterFrameAndTasks } from "../../__tests__/frames";
import { pickIn } from "../../__tests__/issue-picks";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import {
  answerAskSnoozes,
  answerInboxDismisses,
  answerIssuePatches,
} from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import type { InboxRow, Issue } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { appKeymap } from "../shell/keymap";
import { replyRow } from "../threads/replies-fixture";
import { installInboxApiMocks, issueAsk, mockAskReads } from "./inbox-fixture";
import {
  address,
  headings,
  holdThreadsRead,
  inboxIssue,
  mockIssueReads,
  movableReadingPaneWidth,
  narrowIssueOf,
  renderInbox,
  rowIds,
  selectedIds,
  shortRow,
  soon,
} from "./inbox-page-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

installInboxApiMocks();

/** An unassigned ask row on an unassigned issue. */
function orphanAsk(id: string, key: string, question: string): InboxRow {
  return issueAsk(
    { id, issue: { assignee: null, key, title: "Nobody's issue" }, issue_key: key, question },
    null
  );
}

test("below 1280 px the page lists short rows, and a row the reader opens is read in place of the list until Inbox takes them back to it", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  // A page under the Inbox in the history, so an entry the Inbox control wrote of its own
  // would show: one Back from the list must leave the Inbox.
  const { unmount } = renderInbox(["/agents", "/"]);

  try {
    const list = await screen.findByTestId("inbox-list");
    // The short rows the column's list draws, and nothing selected: no card, no pane.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    expect(screen.queryByTestId("ask-ask-a")).not.toBeInTheDocument();
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    expect(selectedIds()).toEqual([]);
    expect(address()).toBe("/");

    await user.click(within(shortRow("ask-b")).getByText("Which format?"));
    const view = await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-b");
    expect(within(view).getByTestId("ask-ask-b")).toBeTruthy();
    // In place of the list and the header above it, which stay mounted, hidden.
    expect(list.closest("[hidden]")).not.toBeNull();
    expect(
      screen.getByRole("searchbox", { hidden: true, name: "Search asks" }).closest("[hidden]")
    ).not.toBeNull();
    // The row the reader picked went with the list, so the view takes the focus.
    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();

    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
    expect(list.closest("[hidden]")).toBeNull();
    // Back at the row the reader left.
    expect(shortRow("ask-b")).toHaveFocus();
    // Through the entry the pick wrote, not over it: one Back from the list leaves the Inbox.
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px the browser's Back and Escape leave the reading view for the row it showed, and Enter opens the focused row", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-a");

    await user.click(screen.getByRole("button", { name: "History back" }));
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
    expect(shortRow("ask-a")).toHaveFocus();

    // The keyboard's pick: Enter on the focused row opens it, Escape is the list again.
    shortRow("ask-b").focus();
    await user.keyboard("{Enter}");
    const view = await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-b");
    expect(within(view).getByTestId("ask-ask-b")).toBeTruthy();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
    expect(shortRow("ask-b")).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px an answer given in the reading view reads the next row, and the last one goes back to the list", async () => {
  const user = userEvent.setup();
  const first = inboxIssue("ask-a", "CORE-1", "Which approach?", {
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const second = inboxIssue("ask-b", "CORE-2", "Which format?", {
    options: [{ label: "Markdown" }, { label: "PDF" }],
  });
  let served = [first, second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([first, second]);
  const getIssue = mockIssueReads();
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async (id, input) => {
    const asked = id === first.id ? first : second;
    served = served.filter((row) => row.id !== id);
    return {
      ...asked,
      answer: {
        at: "2026-09-11T03:00:00Z",
        selected: input.selected ?? [],
        text: "",
        user: "alice",
      },
      state: "answered",
    };
  });
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    const view = await screen.findByTestId("inbox-reading-view");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(view).findByRole("radio", { name: "Ship" }));
    await user.click(within(view).getByRole("button", { name: "Answer" }));

    // As the column does: the answered row leaves and the next one is read in its place.
    await waitFor(() => expect(address()).toBe("/?item=ask-b"));
    expect(rowIds()).toEqual(["ask-b"]);
    const next = screen.getByTestId("inbox-reading-view");
    expect(within(next).getByTestId("ask-ask-b")).toBeTruthy();

    await user.click(await within(next).findByRole("radio", { name: "PDF" }));
    await user.click(within(next).getByRole("button", { name: "Answer" }));

    // With nothing left to read, the list, which says so.
    await waitFor(() => expect(address()).toBe("/"));
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(await screen.findByText("Nothing needs you")).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px a snooze from the reading view's head reads the next row, and the last one goes back to the list", async () => {
  const user = userEvent.setup();
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
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    const view = await screen.findByTestId("inbox-reading-view");
    await pickIn(
      user,
      () => within(view).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );

    await waitFor(() => expect(address()).toBe("/?item=ask-b"));
    await pickIn(
      user,
      () =>
        within(screen.getByTestId("inbox-reading-view")).getByLabelText(
          "Snooze CORE-2: Which format?"
        ),
      "Tomorrow"
    );

    await waitFor(() => expect(address()).toBe("/"));
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(headings()).toEqual(["Later (2)"]);
    expect(snoozeAsk).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("below 1280 px a short row's checkbox marks the row without opening it, and x marks the row the reading view shows", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(within(shortRow("ask-b")).getByRole("checkbox"));
    await screen.findByText("1 selected");
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    expect(address()).toBe("/");

    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    await user.keyboard("x");
    await user.keyboard("{Escape}");
    await screen.findByText("2 selected");
    for (const id of ["ask-a", "ask-b"]) {
      expect((within(shortRow(id)).getByRole("checkbox") as HTMLInputElement).checked).toBe(true);
    }
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// Below 1280 px with a row open, the page hides the list under the row's reading view, so the
// selection bar's hand-off reads the view. Clear selection lands the focus on the view's own
// checkbox for the open row, and not on a checkbox in the hidden list, from which a browser drops
// the focus to the page's body: with the open row marked last, and with another row, hidden in the
// list, marked last.
test("below 1280 px with a row open and a row marked, Clear selection on the bar lands the focus on the reading view's own checkbox", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    for (const marked of ["ask-b", "ask-a"]) {
      // Marked in the list, then `ask-b` opened.
      await user.click(within(shortRow(marked)).getByRole("checkbox"));
      await screen.findByText("1 selected");
      await user.click(shortRow("ask-b"));
      const view = await screen.findByTestId("inbox-reading-view");
      expect(view.getAttribute("data-inbox-reading-row")).toBe("ask-b");
      const mark = within(view).getByRole("checkbox", { name: /^Select / });
      expect((mark as HTMLInputElement).checked).toBe(marked === "ask-b");

      await user.click(screen.getByRole("button", { name: "Clear selection" }));
      await waitFor(() =>
        expect(screen.queryByRole("toolbar", { name: "Selected rows" })).toBeNull()
      );
      await waitFor(() => expect(mark).toHaveFocus());
      expect(mark).not.toBeChecked();
      // The view the reader opened still stands: the press took nothing but the marks.
      expect(view.isConnected).toBe(true);

      await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
      await waitFor(() =>
        expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument()
      );
    }
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

/** Where `element` stands, as a string a failed assertion prints whole: in the reading view of
 *  the row it shows, in the list that view hides, or elsewhere on the page, with the element's
 *  accessible name; the body where nothing else holds the focus. */
function placeOf(element: Element | null): string {
  if (element === null || element === document.body) return "the body";
  const view = element.closest<HTMLElement>('[data-testid="inbox-reading-view"]');
  const part =
    view !== null
      ? `the view of ${view.dataset.inboxReadingRow}`
      : element.closest('[data-testid="inbox-list"]') !== null
        ? "the list"
        : "the page";
  return `${part}: ${element.getAttribute("aria-label") ?? element.tagName.toLowerCase()}`;
}

/** Every place the focus lands from now on (`placeOf`), until `stop`. */
function recordFocus(): { landed: string[]; stop: () => void } {
  const landed: string[] = [];
  const onFocus = (event: FocusEvent) => {
    if (event.target instanceof Element) landed.push(placeOf(event.target));
  };
  document.addEventListener("focusin", onFocus, true);
  return { landed, stop: () => document.removeEventListener("focusin", onFocus, true) };
}

/** The row the reading view shows, by its `rowId`, while one is drawn. */
function viewRow(): string | undefined {
  return screen.getByTestId("inbox-reading-view").dataset.inboxReadingRow;
}

/** The reading view's own mark: the checkbox on its first line, Back and Mark. */
function viewMark(): HTMLElement {
  return within(screen.getByTestId("inbox-reading-view")).getByRole("checkbox", {
    name: /^Select /,
  });
}

/** Serves `rows` to the Inbox with every snooze write of theirs held until `land`, after which
 *  each lands as Dispatch takes it and the Inbox reads its row back with its moment. */
function serveHeldSnoozes(rows: readonly InboxRow[]) {
  let served = [...rows];
  const landed = Promise.withResolvers<void>();
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landed.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  return {
    land: () =>
      act(async () => {
        landed.resolve();
        await landed.promise;
      }),
    restore: () => {
      getAsk.mockRestore();
      getInbox.mockRestore();
      getIssue.mockRestore();
      snoozeAsk.mockRestore();
    },
    snoozeAsk,
  };
}

/** The three asks the bar's Snooze cases list: the reader opens and marks the middle one. */
const SNOOZED_ROWS = [
  inboxIssue("ask-a", "CORE-1", "Which approach?"),
  inboxIssue("ask-b", "CORE-2", "Which format?"),
  inboxIssue("ask-c", "CORE-3", "Which name?"),
];

/** Opens `ask-b` of `SNOOZED_ROWS`, marks it in its reading view, and picks the bar's Snooze,
 *  whose write `serveHeldSnoozes` holds: the bar stays up, and the row stays drawn in its view,
 *  while the write is out. Answers the row's mark in its view. */
async function snoozeOpenRow(user: UserEvent): Promise<HTMLElement> {
  await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]));
  await user.click(shortRow("ask-b"));
  await screen.findByTestId("inbox-reading-view");
  const mark = viewMark();
  await user.click(mark);
  await screen.findByText("1 selected");
  await pickIn(user, () => screen.getByLabelText("Snooze selected asks"), "Tomorrow");
  await screen.findByText("Snoozing 1…");
  expect(viewRow()).toBe("ask-b");
  return mark;
}

/** Serves three threads, the ones the bar's Dismiss cases list, with every Dismiss write held
 *  until `restore`, so no read the landed press asks for draws the dismissed thread again. */
function serveThreads(): { restore: () => Promise<void> } {
  const threads = ["c1", "c2", "c3"].map((id) => replyRow(id));
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: threads,
    total: threads.length,
  });
  const getIssue = mockIssueReads();
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const settle = Promise.withResolvers<{ dismissed: number }>();
  answerInboxDismisses().mockReturnValue(settle.promise);
  return {
    restore: async () => {
      await act(async () => {
        settle.resolve({ dismissed: 1 });
      });
      listComments.mockRestore();
      getIssue.mockRestore();
      listMyReplies.mockRestore();
      getInbox.mockRestore();
    },
  };
}

/** Opens the middle thread `serveThreads` lists and marks it in its reading view. Answers its
 *  mark there. */
async function markOpenThread(user: UserEvent): Promise<HTMLElement> {
  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c2", "reply:c3"]));
  await user.click(shortRow("reply:c2"));
  await screen.findByTestId("inbox-reading-view");
  expect(viewRow()).toBe("reply:c2");
  const mark = viewMark();
  await user.click(mark);
  await screen.findByText("1 selected");
  return mark;
}

// Below 1280 px the page draws the open row's reading view in the list's place and keeps the list
// mounted under `hidden`, where a browser drops a focus put in it to the page's body. The bar's
// Dismiss of the open row takes the row off the list, and the commit that does draws the list for
// that one commit, before the selection is handed on to the next row, whose view hides the list
// again in the commit after. The hand-off reads the open row from the selection, so its landing
// waits through that commit and is made on the next row's own mark in its view. At 390 px the bar
// stands under the header and no foot pass is pending, so a landing is made in the commit that
// asks for it.
test("at 390 px the bar's Dismiss of the open row lands on the next row's own mark in its reading view, never in the hidden list", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(390);
  const threads = serveThreads();
  const { unmount } = renderInbox("/?group=none");
  const focus = recordFocus();
  try {
    await markOpenThread(user);
    const from = focus.landed.length;

    await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
    await waitFor(() => expect(viewRow()).toBe("reply:c3"));
    await waitFor(() => expect(placeOf(document.activeElement)).toBe(placeOf(viewMark())));
    // Still there once every frame and task the going asked for has run.
    await afterFrameAndTasks();
    expect(placeOf(document.activeElement)).toBe(placeOf(viewMark()));
    expect(focus.landed.slice(from).filter((place) => place.startsWith("the list"))).toEqual([]);
  } finally {
    focus.stop();
    await threads.restore();
    unmount();
    width.restore();
  }
});

// The same for the bar's Snooze of the open row: the bar stays up and the row stays drawn in its
// view while the write is out, and the commit the write lands in takes the row and the bar away
// together and draws the list for that one commit. The landing is made on the next row's own mark
// in its view.
test("at 390 px the bar's Snooze of the open row lands on the next row's own mark in its reading view once the write lands, never in the hidden list", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(390);
  const snoozes = serveHeldSnoozes(SNOOZED_ROWS);
  const { unmount } = renderInbox();
  const focus = recordFocus();
  try {
    await snoozeOpenRow(user);
    const from = focus.landed.length;

    await snoozes.land();
    await waitFor(() => expect(viewRow()).toBe("ask-c"));
    await waitFor(() => expect(placeOf(document.activeElement)).toBe(placeOf(viewMark())));
    await afterFrameAndTasks();
    expect(placeOf(document.activeElement)).toBe(placeOf(viewMark()));
    expect(focus.landed.slice(from).filter((place) => place.startsWith("the list"))).toEqual([]);
  } finally {
    focus.stop();
    unmount();
    width.restore();
    snoozes.restore();
  }
});

// The bar's Dismiss of the open row goes with the bar, parks the focus on the row's own mark in its
// reading view while the row is still drawn, and owes the landing. Here the press's own apply waits
// on the reads it cancels (`useInboxDismiss`), so the row stays drawn in its view while the reader
// acts. Their wheel or key there is their act, read once it has been answered: it leaves the focus
// in the view, which is the open row for where the focus stands (`MarkedList`'s `row`), and that
// row is leaving, so the landing is owed again. The apply then takes the row away and its view
// with it, which drops the focus onto the body, and the landing is made on the next row's own mark
// in its view.
test.each([
  "a wheel",
  "a key",
] as const)("at 390 px, with the landing of the bar's Dismiss of the open row owed, %s in the row's reading view owes it again, made on the next row's mark once the row goes", async (reader) => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(390);
  const threads = serveThreads();
  const { queryClient, unmount } = renderInbox("/?group=none");
  try {
    const mark = await markOpenThread(user);
    const apply = Promise.withResolvers<void>();
    const cancel = spyOn(queryClient, "cancelQueries").mockReturnValue(apply.promise);
    try {
      await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
      expect(screen.queryByRole("toolbar", { name: "Selected rows" })).toBeNull();
      await waitFor(() => expect(placeOf(document.activeElement)).toBe(placeOf(mark)));
      if (reader === "a wheel") {
        mark.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
      } else {
        await user.keyboard("{ArrowDown}");
      }
      // Read once the act has been answered: the focus is still on the mark, in the leaving row.
      await afterFrameAndTasks();
      expect(placeOf(document.activeElement)).toBe(placeOf(mark));
      expect(viewRow()).toBe("reply:c2");
    } finally {
      cancel.mockRestore();
    }

    await act(async () => {
      apply.resolve();
      await apply.promise;
    });
    await waitFor(() => expect(viewRow()).toBe("reply:c3"));
    await waitFor(() => expect(placeOf(document.activeElement)).toBe(placeOf(viewMark())));
  } finally {
    await threads.restore();
    unmount();
    width.restore();
  }
});

// The bar's Snooze of the open row keeps the bar up while its write is out, with the row still
// drawn in its reading view. The reader unticks the row's own mark there with a click, which
// leaves the focus on it, in a row the press names as leaving: the view is that row for where the
// focus stands (`MarkedList`'s `row`). The write lands and takes the row, its view and the bar
// away in one commit, which drops the focus onto the body before the bar's going can read it. The
// hand-off remembered the mark in its leaving row, so the going owes the landing, and it is made
// on the next row's own mark in its view.
test("at 390 px, with the bar's Snooze of the open row out, an untick of the row's mark in its reading view owes the landing, made on the next row's mark once the row goes", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(390);
  const snoozes = serveHeldSnoozes(SNOOZED_ROWS);
  const { unmount } = renderInbox();
  try {
    const mark = await snoozeOpenRow(user);
    await user.click(mark);
    expect(mark).not.toBeChecked();
    await afterFrameAndTasks();
    expect(placeOf(document.activeElement)).toBe(placeOf(mark));
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeInTheDocument();

    await snoozes.land();
    await waitFor(() => expect(viewRow()).toBe("ask-c"));
    await waitFor(() => expect(placeOf(document.activeElement)).toBe(placeOf(viewMark())));
  } finally {
    unmount();
    width.restore();
    snoozes.restore();
  }
});

test("below 1280 px Escape in the reading view's answer box is the box's, and leaves the view only from outside a text field", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    const view = await screen.findByTestId("inbox-reading-view");
    const box = view.querySelector<HTMLElement>("[data-ask-answer]");
    if (box === null) throw new Error("the reading view drew no answer box");
    await user.click(box);
    await user.keyboard("Ship it");

    // In the box Escape is the box's own: the view, the question, the words typed and the focus
    // all stay. Without the text-field guard the view's Escape would take the focus out of the
    // box to the view's scroller, the step out of the question card.
    await user.keyboard("{Escape}");
    expect(address()).toBe("/?item=ask-a");
    expect(screen.getByTestId("inbox-reading-view")).toBe(view);
    expect(within(view).getByTestId("ask-ask-a")).toBeTruthy();
    expect(box).toHaveValue("Ship it");
    expect(box).toHaveFocus();

    // From outside a text field it is the way back to the list, at the row the view showed.
    act(() => {
      screen.getByTestId("inbox-pane-scroller").focus();
    });
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
    expect(shortRow("ask-a")).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px Assign to me in the reading view's head PATCHes the viewer's login", async () => {
  const user = userEvent.setup();
  // A row the server resolved nobody for, which is what "Assign to me" is drawn on (the drawer's
  // card row above says why such a row is an older server's).
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
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=unassigned");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-orphan"]));
    // The short row carries no such control: the head of the view the row opens does.
    expect(
      within(shortRow("ask-orphan")).queryByRole("button", { name: "Assign CORE-3 to me" })
    ).toBeNull();
    await user.click(shortRow("ask-orphan"));
    const view = await screen.findByTestId("inbox-reading-view");
    await user.click(within(view).getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    await act(async () => {
      save.resolve(narrowIssueOf(orphan, "alice"));
      await save.promise;
    });
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px a row the column chose by itself is no row the reader opened: narrowing shows the list", async () => {
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
    // Beside the list the column shows the first row the reader can see, by itself.
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(address()).toBe("/?item=ask-a"));

    // Narrowed, the reader is shown the list, and the address drops the row they never picked.
    act(() => width.setWidth(1279));
    await waitFor(() => expect(address()).toBe("/"));
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    expect(screen.getByTestId("inbox-list").closest("[hidden]")).toBeNull();

    // A row the reader picks beside the column is theirs, and narrowing keeps it open.
    act(() => width.setWidth(1440));
    await waitFor(() => expect(address()).toBe("/?item=ask-a"));
    await user.click(shortRow("ask-b"));
    await waitFor(() => expect(address()).toBe("/?item=ask-b"));
    act(() => width.setWidth(1279));
    const view = await screen.findByTestId("inbox-reading-view");
    expect(within(view).getByTestId("ask-ask-b")).toBeTruthy();
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px the way back from an opened row stays on the Inbox after a Back and a width change", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(1024);
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox(["/agents", "/"]);

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    await user.click(screen.getByRole("button", { name: "History back" }));
    await waitFor(() => expect(address()).toBe("/"));

    // Wider, the column picks a row by itself over the list's entry; narrower again, the list.
    act(() => width.setWidth(1440));
    await waitFor(() => expect(address()).toBe("/?item=ask-a"));
    act(() => width.setWidth(1024));
    await waitFor(() => expect(address()).toBe("/"));
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();

    // A row opened now goes back to that list, not past it.
    await user.click(shortRow("ask-b"));
    const view = await screen.findByTestId("inbox-reading-view");
    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(address()).toBe("/"));
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px a reload onto an opened row keeps its way back: the Inbox control goes back through the entry the open pushed", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  // The history a reload keeps: the entry the open pushed holds its mark, the Inbox remounts.
  const { unmount } = renderInbox([
    "/agents",
    "/",
    { pathname: "/", search: "?item=ask-a", state: { inboxOverList: true } },
  ]);

  try {
    const view = await screen.findByTestId("inbox-reading-view");
    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(address()).toBe("/"));
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px Escape on an answer option steps out to the view and keeps the pick; the next Escape is the list", async () => {
  const user = userEvent.setup();
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
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    await user.click(shortRow("ask-a"));
    const view = await screen.findByTestId("inbox-reading-view");
    const ship = await within(view).findByRole("radio", { name: "Ship" });
    await user.click(ship);
    expect(ship).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.getByTestId("inbox-reading-view")).toBe(view);
    expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus();
    expect(ship).toBeChecked();
    // Not a palette row: in the palette Escape closes the palette.
    expect(appKeymap.actions().map((action) => action.id)).not.toContain("leave-reading");

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px Enter that opens a row leaves the focus where the next Enter reaches the answer box", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    shortRow("ask-b").focus();
    await user.keyboard("{Enter}");
    const view = await screen.findByTestId("inbox-reading-view");
    await waitFor(() => expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus());
    await user.keyboard("{Enter}");
    expect(view.querySelector("[data-ask-answer]")).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px `h` on a focused ask row opens its view at the head's Snooze, and with rows marked reaches the bar the view shows", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    shortRow("ask-b").focus();
    await user.keyboard("h");
    const view = await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-b");
    await waitFor(() => expect(document.activeElement?.matches("[data-inbox-snooze]")).toBe(true));
    expect(view.contains(document.activeElement)).toBe(true);

    // `x` marks the row the view shows, and the view says so: its own checkbox and the bar.
    screen.getByTestId("inbox-pane-scroller").focus();
    await user.keyboard("x");
    expect(within(view).getByRole("checkbox", { name: /^Select / })).toBeChecked();
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toHaveTextContent("1 selected");
    await user.keyboard("h");
    expect(screen.getByLabelText("Snooze selected asks")).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px a row the column handed its own pick on to is no row the reader opened either", async () => {
  const width = movableReadingPaneWidth(1440);
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox();

  try {
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(address()).toBe("/?item=ask-a"));
    // The column's own pick leaves without the reader doing anything: the next row is shown.
    served = [rows[1] as InboxRow];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(address()).toBe("/?item=ask-b"));

    act(() => width.setWidth(1279));
    await waitFor(() => expect(address()).toBe("/"));
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    expect(screen.getByTestId("inbox-list").closest("[hidden]")).toBeNull();
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px when the open row leaves an empty list, the list is back and its keys work", async () => {
  const user = userEvent.setup();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox(["/agents", "/"]);

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-a");

    // The server takes the last row away while it is open.
    served = [];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(address()).toBe("/"));
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    // `f` reaches the search box again, and the way back from the list is the page before it.
    document.body.focus();
    await user.keyboard("f");
    expect(screen.getByRole("searchbox", { name: "Search asks" })).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px `1`-`9` and `y` on a focused ask row open it and act in its view", async () => {
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
    shortRow("ask-a").focus();
    await user.keyboard("2");
    const view = await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-a");
    await waitFor(() =>
      expect((within(view).getByRole("radio", { name: "Hold" }) as HTMLInputElement).checked).toBe(
        true
      )
    );

    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    shortRow("ask-b").focus();
    await user.keyboard("y");
    const next = await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-b");
    // The head's copy control was pressed: it says how the copy went.
    expect(await within(next).findByText(/^(Copied|Copy failed - select the text)$/)).toBeTruthy();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px the reader's own Assign to me under Unassigned moves the view to the next row, as the column does, and says nothing left", async () => {
  const user = userEvent.setup();
  const rows = [
    orphanAsk("ask-oa", "CORE-3", "First orphan?"),
    orphanAsk("ask-ob", "CORE-4", "Second orphan?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { unmount } = renderInbox("/?assignee=unassigned");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-oa", "ask-ob"]));
    await user.click(shortRow("ask-oa"));
    const view = await screen.findByTestId("inbox-reading-view");
    await user.click(within(view).getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    getInbox.mockResolvedValue([
      {
        ...(rows[0] as InboxRow),
        issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" },
        waiting_on_person: "alice",
      },
      rows[1] as InboxRow,
    ]);
    await act(async () => {
      save.resolve(narrowIssueOf(rows[0] as InboxRow, "alice"));
      await save.promise;
    });

    await waitFor(() => expect(address()).toBe("/?item=ask-ob&assignee=unassigned"));
    expect(screen.getByTestId("inbox-reading-view").getAttribute("data-inbox-reading-row")).toBe(
      "ask-ob"
    );
    expect(screen.queryByText("This item left your Inbox.")).toBeNull();
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px an open row that only leaves the list's filter goes back to the list without saying it left the Inbox", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?", { priority: 1 }),
    inboxIssue("ask-b", "CORE-2", "Which format?", { priority: 1 }),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox("/?priority=P1");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    await user.unhover(shortRow("ask-a"));

    // Somebody else moves the issue to P2: the question is still the reader's, out of P1.
    served = [{ ...(rows[0] as InboxRow), priority: 2 }, rows[1] as InboxRow];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/?priority=P1");
    expect(screen.queryByText("This item left your Inbox.")).toBeNull();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a search typed beside the column keeps the column's own pick, so narrowing shows the list and leaves the focus in the search box", async () => {
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
    await waitFor(() => expect(address()).toBe("/?item=ask-a"));
    const search = screen.getByRole("searchbox", { name: "Search asks" });
    await user.type(search, "approach");
    await waitFor(() => expect(address()).toBe("/?item=ask-a&q=approach"));

    // Narrowing drops the column's own pick, which no reading view was ever drawn for: the
    // reader is still typing, so the focus stays in the box rather than moving to a row.
    act(() => width.setWidth(1279));
    await waitFor(() => expect(address()).toBe("/?q=approach"));
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    expect(screen.getByTestId("inbox-list").closest("[hidden]")).toBeNull();
    expect(search).toHaveFocus();
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("a search typed over a row opened below 1280 px keeps its way back: the Inbox control goes back through the open's entry", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(1024);
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox(["/agents", "/"]);

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    act(() => width.setWidth(1440));
    await screen.findByTestId("inbox-pane");
    await user.type(screen.getByRole("searchbox", { name: "Search asks" }), "approach");
    await waitFor(() => expect(address()).toBe("/?item=ask-a&q=approach"));
    act(() => width.setWidth(1024));
    const view = await screen.findByTestId("inbox-reading-view");

    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(address()).toBe("/"));
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px Escape on the view head's Snooze goes back to the list, at the row", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    shortRow("ask-b").focus();
    await user.keyboard("h");
    await screen.findByTestId("inbox-reading-view");
    await waitFor(() => expect(document.activeElement?.matches("[data-inbox-snooze]")).toBe(true));

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
    expect(shortRow("ask-b")).toHaveFocus();
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px an open row that leaves on its own brings the list back at its place and says so", async () => {
  const user = userEvent.setup();
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { queryClient, unmount } = renderInbox(["/agents", "/"]);

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");
    await user.unhover(shortRow("ask-a"));

    // The server takes the open question away; the reader did nothing to it.
    served = [rows[1] as InboxRow];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/");
    expect(screen.getByText("This item left your Inbox.").getAttribute("role")).toBe("status");
    expect(shortRow("ask-b")).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px an open row the reader's own Assign to me left in the list, withdrawn later, brings the list back and says it left", async () => {
  const user = userEvent.setup();
  const rows = [
    orphanAsk("ask-oa", "CORE-3", "First orphan?"),
    orphanAsk("ask-ob", "CORE-4", "Second orphan?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  // Everyone: no assignee narrowing, so the question the reader takes stays listed.
  const { queryClient, unmount } = renderInbox("/?assignee=everyone");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-oa", "ask-ob"]));
    await user.click(shortRow("ask-oa"));
    const view = await screen.findByTestId("inbox-reading-view");
    await user.unhover(shortRow("ask-oa"));
    await user.click(within(view).getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    served = [
      {
        ...(rows[0] as InboxRow),
        issue: { assignee: "alice", key: "CORE-3", title: "Nobody's issue" },
        waiting_on_person: "alice",
      },
      rows[1] as InboxRow,
    ];
    await act(async () => {
      save.resolve(narrowIssueOf(rows[0] as InboxRow, "alice"));
      await save.promise;
    });
    // The question the reader took stays in their list, and open.
    await waitFor(() =>
      expect(within(view).queryByRole("button", { name: /Assign CORE-3 to me/ })).toBeNull()
    );
    expect(address()).toBe("/?item=ask-oa&assignee=everyone");

    // The agent then withdraws it. The reader's write is over, so this is the server's move: the
    // list comes back and says so, rather than the view swapping in a question they never opened.
    served = [rows[1] as InboxRow];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(address()).toBe("/?assignee=everyone");
    expect(screen.getByText("This item left your Inbox.")).toBeTruthy();
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

// A write marks its ask as the reader's own while it is pending or failed (`AskWrites.live`): a
// refused Assign to me is still the reader's write, so the row leaving after it is their own
// move, and the view moves on as it does for a write that is out.
test("below 1280 px an open row whose own Assign to me was refused, withdrawn later, moves the view to the next row and says nothing left", async () => {
  const user = userEvent.setup();
  const rows = [
    orphanAsk("ask-oa", "CORE-3", "First orphan?"),
    orphanAsk("ask-ob", "CORE-4", "Second orphan?"),
  ];
  let served: InboxRow[] = rows;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  // Everyone: no assignee narrowing, so the question stays listed through the write.
  const { queryClient, unmount } = renderInbox("/?assignee=everyone");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-oa", "ask-ob"]));
    await user.click(shortRow("ask-oa"));
    const view = await screen.findByTestId("inbox-reading-view");
    await user.unhover(shortRow("ask-oa"));
    await user.click(within(view).getByRole("button", { name: "Assign CORE-3 to me" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-3", { assignee: "alice" }, keyedAttempt)
    );
    await act(async () => {
      save.reject(
        new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-3 is closed; reopen it first" })
      );
      await save.promise.catch(() => undefined);
    });
    // Refused: the question is still listed and open, with the reason on it.
    expect(await within(view).findByRole("alert")).toHaveTextContent(
      "CORE-3 is closed; reopen it first"
    );
    expect(address()).toBe("/?item=ask-oa&assignee=everyone");

    // The agent then withdraws it. The refused write is still the reader's, so the view moves to
    // the next row and says nothing left.
    served = [rows[1] as InboxRow];
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(address()).toBe("/?item=ask-ob&assignee=everyone"));
    expect(screen.getByTestId("inbox-reading-view").getAttribute("data-inbox-reading-row")).toBe(
      "ask-ob"
    );
    expect(screen.queryByText("This item left your Inbox.")).toBeNull();
  } finally {
    unmount();
    patchIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px the line saying the open row left is a live region that is always there, and the reader's next key or a wider window ends it", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(1024);
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
  const line = () => document.querySelector("[data-inbox-row-left]");
  const withdraw = async (id: string) => {
    await user.click(shortRow(id));
    await screen.findByTestId("inbox-reading-view");
    await user.unhover(shortRow(id));
    served = served.filter((row) => row.id !== id);
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["inbox"] });
    });
    await waitFor(() => expect(line()?.textContent).toBe("This item left your Inbox."));
  };

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b", "ask-c"]));
    // The region is there before it has anything to say, so a screen reader hears the line as
    // it appears in it.
    expect(line()?.getAttribute("role")).toBe("status");
    expect(line()?.textContent).toBe("");

    await withdraw("ask-a");
    await user.keyboard("j");
    expect(line()?.textContent).toBe("");
    expect(line()?.getAttribute("role")).toBe("status");

    await withdraw("ask-b");
    act(() => width.setWidth(1440));
    await screen.findByTestId("inbox-pane");
    await waitFor(() => expect(line()?.textContent).toBe(""));
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px `h` on a snoozed row opens nothing, and leaves no Snooze for a later row to take the focus to", async () => {
  const user = userEvent.setup();
  const open = inboxIssue("ask-a", "CORE-1", "Which approach?");
  const later = inboxIssue("ask-later", "CORE-2", "Which format?", { snoozed_until: soon() });
  const rows = [open, later];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox();

  try {
    await waitFor(() => expect(rowIds()).toContain("ask-a"));
    await user.click(screen.getByRole("button", { name: /^Later \(1\)/ }));
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-later"]));
    shortRow("ask-later").focus();
    await user.keyboard("h");
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    expect(address()).toBe("/");

    await user.keyboard("k");
    await user.keyboard("{Enter}");
    await screen.findByTestId("inbox-reading-view");
    await waitFor(() => expect(screen.getByTestId("inbox-pane-scroller")).toHaveFocus());
    expect(document.activeElement?.matches("[data-inbox-snooze]")).toBe(false);
  } finally {
    unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("beside the column, Back over a row opened below 1280 px goes back past it", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(1024);
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox(["/agents", "/"]);

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-b"));
    await screen.findByTestId("inbox-reading-view");
    expect(address()).toBe("/?item=ask-b");
    act(() => width.setWidth(1440));
    await screen.findByTestId("inbox-pane");

    await user.click(screen.getByRole("button", { name: "History back" }));
    await waitFor(() => expect(address()).toBe("/?item=ask-a"));
    act(() => width.setWidth(1024));
    await waitFor(() => expect(address()).toBe("/"));
    expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px a reload onto a thread row whose read lands after the asks' opens that row, with its way back", async () => {
  const user = userEvent.setup();
  const rows = [inboxIssue("ask-a", "CORE-1", "Which approach?")];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const threads = holdThreadsRead();
  // The history a reload keeps: the entry the open pushed over the list's own names the thread.
  const { unmount } = renderInbox([
    "/agents",
    "/",
    { pathname: "/", search: "?item=reply:c1", state: { inboxOverList: true } },
  ]);

  try {
    // The asks' read has landed and the list draws its row; the threads' read is still out, so
    // the row the address names is not listed yet - which is no row that left.
    await waitFor(() => expect(rowIds()).toEqual(["ask-a"]));
    expect(address()).toBe("/?item=reply:c1");

    await threads.land([replyRow("c1")]);
    const view = await screen.findByTestId("inbox-reading-view");
    expect(view.getAttribute("data-inbox-reading-row")).toBe("reply:c1");
    expect(address()).toBe("/?item=reply:c1");

    // And its way back is still the one step through the entry the open pushed.
    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(address()).toBe("/"));
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    threads.restore();
    listComments.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});

test("below 1280 px clearing the agent filter beside the column keeps a row's way back to one step", async () => {
  const user = userEvent.setup();
  const width = movableReadingPaneWidth(1024);
  const rows = [
    inboxIssue("ask-a", "CORE-1", "Which approach?"),
    inboxIssue("ask-b", "CORE-2", "Which format?"),
  ];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const getIssue = mockIssueReads();
  const { unmount } = renderInbox(["/agents", "/?agent=session-1"]);
  const params = () => new URLSearchParams(address().split("?")[1] ?? "");

  try {
    await waitFor(() => expect(rowIds()).toEqual(["ask-a", "ask-b"]));
    await user.click(shortRow("ask-a"));
    await screen.findByTestId("inbox-reading-view");

    // Widened, the header shows again with the filter's chip. Clearing it pushes an entry, as a
    // link does, that is no row's opening.
    act(() => width.setWidth(1440));
    await screen.findByTestId("inbox-pane");
    await user.click(screen.getByRole("button", { name: "Clear agent filter" }));
    await waitFor(() => expect(params().get("agent")).toBeNull());
    expect(params().get("item")).toBe("ask-a");

    // Narrowed again, one press of the view's Inbox control is the list.
    act(() => width.setWidth(1024));
    const view = await screen.findByTestId("inbox-reading-view");
    await user.click(within(view).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(screen.queryByTestId("inbox-reading-view")).not.toBeInTheDocument());
    expect(params().get("item")).toBeNull();
    // The browser's Back then undoes the clear: the entry before it is the row the reader
    // opened under the filter, and the view's Inbox control leaves that in one step too.
    await user.click(screen.getByRole("button", { name: "History back" }));
    await waitFor(() => expect(params().get("agent")).toBe("session-1"));
    expect(params().get("item")).toBe("ask-a");
    const reopened = await screen.findByTestId("inbox-reading-view");
    await user.click(within(reopened).getByRole("button", { name: "Back to the Inbox list" }));
    await waitFor(() => expect(address()).toBe("/?agent=session-1"));
    await user.click(screen.getByRole("button", { name: "History back" }));
    expect(address()).toBe("/agents");
  } finally {
    unmount();
    width.restore();
    getAsk.mockRestore();
    getInbox.mockRestore();
    getIssue.mockRestore();
  }
});
