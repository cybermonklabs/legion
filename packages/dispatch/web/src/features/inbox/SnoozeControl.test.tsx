import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { watchMoves } from "../../__tests__/laid-out-rows";
import { answerAskSnoozes, answerAskUnsnoozes } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError } from "../../api/client";
import type { InboxRow, IssueDetails } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { writesOf } from "../writes/write-store";
import { SnoozeWrites } from "./ask-snooze-fixture";
import { Inbox } from "./Inbox";
import { InboxDrawer } from "./InboxDrawer";
import {
  installInboxApiMocks,
  issueAsk,
  mockAskReads,
  shortRow,
  widenToReadingPane,
} from "./inbox-fixture";
import { SnoozeControl } from "./SnoozeControl";

// dispatch://INFRA-2160, the table "Where the focus goes back, by opener": a key that opens the
// Snooze list (`b`, `h`) or the priority list (`p`) from the Inbox hands the list the element the
// key was pressed on, and every close gives the focus back there (D5); and the host row of "The
// state table of a pick that opts into D5" of a snooze that lands, whose hand-off wins over that
// target (R8).

installInboxApiMocks();

/** An open question of the issue `key`, which alice holds. */
function ask(id: string, key: string, question: string): InboxRow {
  return issueAsk({
    id,
    issue: { assignee: "alice", key, title: `${key} title` },
    issue_key: key,
    question,
  });
}

const ROWS = [
  ask("ask-a", "CORE-1", "Which approach?"),
  ask("ask-b", "CORE-2", "Which format?"),
  ask("ask-c", "CORE-3", "Which owner?"),
];

/** The Inbox's reads of `rows`, each answered as the server answers them, and the issue reads a
 *  card makes. Answers what puts them back. */
function readsOf(rows: readonly InboxRow[]): () => void {
  const mocks = [
    spyOn(api, "getInbox").mockResolvedValue([...rows]),
    mockAskReads(rows),
    spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails),
  ];
  return () => {
    for (const mock of mocks) mock.mockRestore();
  };
}

/** Draws `content` as the app does: the router, the queries and the keymap around it. */
function renderInApp(content: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>{content}</KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The open list named `label`, or null while none is. */
function openList(label: RegExp): HTMLElement | null {
  return screen.queryByRole("listbox", { name: label });
}

/** Presses `key` with the focus on `from`, which opens the list `label` names with the focus in
 *  its text box, then Escape, which closes it with nothing written and gives the focus back. */
async function openAndEscape(
  user: UserEvent,
  from: HTMLElement,
  key: string,
  label: RegExp
): Promise<void> {
  act(() => from.focus());
  await user.keyboard(key);
  const list = openList(label);
  expect(list, `${key} opens ${label}`).not.toBeNull();
  expect(screen.getByRole("combobox"), `${key} puts the focus in the list's box`).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(openList(label), `Escape closes ${label}`).toBeNull();
}

const SNOOZE_A = /^Snooze CORE-1: Which approach\? options$/;
const PRIORITY_A = /^Priority of CORE-1 options$/;

test("a key from a pane row returns to the row", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const reads = readsOf(ROWS);
  const view = renderInApp(<Inbox />);
  try {
    await screen.findByTestId("inbox-pane");
    const row = await waitFor(() => shortRow("ask-a"));
    for (const key of ["b", "h"]) {
      await openAndEscape(user, row, key, SNOOZE_A);
      expect(row, `the list ${key} opened gives the focus back to its row`).toHaveFocus();
    }
  } finally {
    view.unmount();
    reads();
    narrow();
  }
});

// Below 1280 px the page draws short rows; the cards are the list's that selects nothing, as the
// Peek drawer mounts it (`pane={false}`).
test("a key from a card returns to the card", async () => {
  const user = userEvent.setup();
  const reads = readsOf(ROWS);
  const view = renderInApp(<Inbox pane={false} />);
  try {
    await screen.findByTestId("ask-ask-a");
    const card = shortRow("ask-a");
    for (const [key, label] of [
      ["b", SNOOZE_A],
      ["h", SNOOZE_A],
      ["p", PRIORITY_A],
    ] as const) {
      await openAndEscape(user, card, key, label);
      expect(card, `the list ${key} opened gives the focus back to its card`).toHaveFocus();
    }
  } finally {
    view.unmount();
    reads();
  }
});

/** The drawer as the app mounts it: a trigger the reader's focus starts on, and the drawer. */
function DrawerHarness(): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button onClick={() => setOpen(true)} type="button">
        Open inbox
      </button>
      <InboxDrawer onClose={() => setOpen(false)} open={open} />
    </>
  );
}

test("a key in the drawer returns to the row and leaves the drawer open", async () => {
  const user = userEvent.setup();
  const reads = readsOf(ROWS);
  const view = renderInApp(<DrawerHarness />);
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const drawer = await screen.findByRole("dialog", { name: "Inbox" });
    await within(drawer).findByTestId("ask-ask-a");
    const row = within(drawer).getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("the drawer drew no row for ask-a");
    for (const [key, label] of [
      ["b", SNOOZE_A],
      ["h", SNOOZE_A],
      ["p", PRIORITY_A],
    ] as const) {
      await openAndEscape(user, row, key, label);
      expect(row, `the list ${key} opened gives the focus back to its row`).toHaveFocus();
      expect(screen.getByRole("dialog", { name: "Inbox" }), "Escape left the drawer open").toBe(
        drawer
      );
    }
  } finally {
    view.unmount();
    reads();
  }
});

test("a key with items selected opens the bar's list and returns to where it was", async () => {
  const user = userEvent.setup();
  const reads = readsOf(ROWS);
  const view = renderInApp(<Inbox pane={false} />);
  const bar = /^Snooze selected asks options$/;
  try {
    await screen.findByTestId("ask-ask-c");
    const mark = (id: string) =>
      within(shortRow(id)).getByRole("checkbox", { name: /^Select / }) as HTMLElement;
    await user.click(mark("ask-a"));
    await user.click(mark("ask-b"));
    await screen.findByText("2 selected");
    // From the checkbox the reader just ticked, and from a row.
    await openAndEscape(user, mark("ask-b"), "h", bar);
    expect(mark("ask-b"), "the bar's list gives the focus back to the checkbox").toHaveFocus();
    await openAndEscape(user, shortRow("ask-c"), "b", bar);
    expect(shortRow("ask-c"), "the bar's list gives the focus back to the row").toHaveFocus();
  } finally {
    view.unmount();
    reads();
  }
});

// A pick gives the focus back there with no scroll. A card is tall, and the engine's own scroll of
// one partly out of sight moved the list under the reader at the pick (the tester's round 16: 230
// px in card mode); a snooze that lands hands the reader on itself, and brings the next row into
// view once the snoozed one has gone (R8).
test("a pick gives the focus back to the card a key opened the list from with no scroll", async () => {
  const user = userEvent.setup();
  const reads = readsOf(ROWS);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (_id, { snoozed_until: until }) => {
    await landing.promise;
    return { snoozed_until: until };
  });
  const view = renderInApp(<Inbox pane={false} />);
  try {
    await screen.findByTestId("ask-ask-b");
    const card = shortRow("ask-b");
    act(() => card.focus());
    await user.keyboard("b");
    const moves = watchMoves();
    try {
      await user.keyboard("1");
      await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
      expect(card).toHaveFocus();
      expect(
        moves.focused.filter(({ target }) => target === card).map(({ options }) => options)
      ).toEqual([{ preventScroll: true }]);
    } finally {
      moves.restore();
    }
  } finally {
    await act(async () => {
      landing.resolve();
    });
    view.unmount();
    snoozeAsk.mockRestore();
    reads();
  }
});

// The state table's host row: a snooze that lands takes its item out of the list, and R8's
// hand-off then gives the row after it the selection and the focus, over the row the list gave
// the focus back to as it closed.
test("a snooze that lands hands the focus to the next row", async () => {
  const user = userEvent.setup();
  let served: InboxRow[] = ROWS;
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads(ROWS);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    closed_at: null,
  } as unknown as IssueDetails);
  const landing = Promise.withResolvers<void>();
  const snoozeAsk = answerAskSnoozes().mockImplementation(async (id, { snoozed_until: until }) => {
    await landing.promise;
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const view = renderInApp(<Inbox pane={false} />);
  try {
    await screen.findByTestId("ask-ask-b");
    const row = shortRow("ask-b");
    act(() => row.focus());
    await user.keyboard("b");
    await user.keyboard("1");
    await waitFor(() => expect(snoozeAsk).toHaveBeenCalledTimes(1));
    // While the write is out the reader is back on the row the key was pressed on.
    expect(row).toHaveFocus();
    await act(async () => {
      landing.resolve();
      await landing.promise;
    });
    await waitFor(() =>
      expect(
        [...document.querySelectorAll("[data-inbox-row]")].map((each) =>
          each.getAttribute("data-inbox-row")
        )
      ).toEqual(["ask-a", "ask-c"])
    );
    await waitFor(() => expect(shortRow("ask-c")).toHaveFocus());
  } finally {
    view.unmount();
    snoozeAsk.mockRestore();
    getIssue.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// A row's own snooze write that never answers - offline, each try failing at the network - holds
// Un-snooze and the list until its deadline and no longer: past it the write is still tried, and
// a press then saves again, behind it on the ask's snooze line, so it never lands before it.
test("Un-snooze lets go at the snooze write deadline and saves again", async () => {
  const unsnooze = answerAskUnsnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-a/snooze", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SnoozeWrites>
            <SnoozeControl
              askId="ask-a"
              label="CORE-1: Which approach?"
              snoozedUntil="2099-01-01T09:00:00Z"
            />
          </SnoozeWrites>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const button = screen.getByRole<HTMLButtonElement>("button", {
      name: "Un-snooze CORE-1: Which approach?",
    });
    const trigger = screen.getByRole("button", { name: /^Snooze CORE-1: Which approach\?/ });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([unsnooze]);
    await heldUntilLate(user, button, {
      alongside: (held) => {
        expect(trigger.getAttribute("aria-disabled"), "the list's trigger").toBe(
          held ? "true" : null
        );
      },
      label: "Ask brought back now",
      name: "Un-snooze",
      queryClient,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(
      writesOf(queryClient)
        .rows()
        .filter((row) => row.label === "Ask brought back now")
        .map((row) => row.status),
      "a press past the deadline saves again, behind the late one"
    ).toEqual(["late", "queued"]);
    expect(sent(), "the press behind the late one, before its turn").toBe(1);
    expect(button, "Un-snooze, behind the late save").not.toHaveAttribute("aria-disabled");
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
    unsnooze.mockRestore();
  }
});

// The bar's Snooze write over a row that never answers - offline, each try failing at the network
// - holds that row's Snooze until its deadline and no longer: past it the bar's write is still
// tried, and a pick from the row then saves a snooze of its own, behind the bar's on the ask's
// snooze line, so the bar's never lands after it and puts back the moment the row replaced.
test("a row's Snooze lets go at the bar's snooze write deadline and saves again", async () => {
  const reads = readsOf(ROWS);
  const snoozeAsk = answerAskSnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-a/snooze", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <Inbox pane={false} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByTestId("ask-ask-a");
    await userEvent
      .setup()
      .click(within(shortRow("ask-a")).getByRole("checkbox", { name: /^Select / }));
    await screen.findByText("1 selected");
    const bar = screen.getByRole("button", { name: "Snooze selected asks" });
    const trigger = screen.getByRole<HTMLButtonElement>("button", {
      name: /^Snooze CORE-1: Which approach\?/,
    });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([snoozeAsk]);
    // The bar's list opens on its first choice, which Enter picks.
    await heldUntilLate(user, trigger, {
      label: "Ask snoozed",
      name: "the row's Snooze",
      press: "{Enter}{Enter}",
      queryClient,
      sent,
      start: bar,
    });
    await user.keyboard("{Enter}{Enter}");
    await tick(1_000);
    expect(
      writesOf(queryClient)
        .rows()
        .filter((row) => row.label === "Ask snoozed")
        .map((row) => row.status),
      "a pick from the row past the deadline saves again, behind the bar's late one"
    ).toEqual(["late", "queued"]);
    expect(sent(), "the row's pick behind the bar's late one, before its turn").toBe(1);
    expect(trigger, "the row's Snooze, behind the late save").not.toHaveAttribute("aria-disabled");
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
    reads();
  }
});
