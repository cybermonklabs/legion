import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { pickFromMenu } from "../../__tests__/menu-picks";
import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { InboxDrawer } from "./InboxDrawer";
import { installInboxApiMocks, issueAsk, mockAskReads } from "./inbox-fixture";

installInboxApiMocks();

/** A harness matching how `app.tsx` mounts the drawer: a trigger the reader's focus starts on,
 *  the drawer itself as a sibling, state living above both. */
function Harness() {
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

/** What the drawer must never write to: the address of the page it is a peek over. */
function LocationProbe() {
  return <output data-testid="location">{useLocation().search}</output>;
}

function renderDrawer() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <LocationProbe />
          <Harness />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("opens over the triggering control, moves initial focus to Close, and returns it on close", async () => {
  const user = userEvent.setup();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    const opener = screen.getByRole("button", { name: "Open inbox" });
    opener.focus();
    await user.click(opener);

    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const closeButton = within(dialog).getByRole("button", { name: "Close" });
    await waitFor(() => expect(closeButton).toHaveFocus());

    await user.click(closeButton);
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Inbox" })).not.toBeInTheDocument()
    );
    expect(opener).toHaveFocus();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("picking an assignee inside the drawer lists that person there, writes nothing to the address, and leaves the drawer open", async () => {
  const user = userEvent.setup();
  const mine = issueAsk();
  const bobs = issueAsk({
    id: "ask-bob",
    issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
    issue_key: "CORE-2",
    question: "Bob's question?",
  });
  const rows = [mine, bobs];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    await within(dialog).findByText("Which approach?");
    expect(within(dialog).queryByText("Bob's question?")).not.toBeInTheDocument();

    // The five controls sit behind the row's one **Filter** trigger, in a panel `AnchoredPanel`
    // portals onto the body: the trigger is the drawer's, the panel it opens is reached from
    // the screen, as the assignee popover inside it already was.
    await user.click(within(dialog).getByRole("button", { name: "Filter" }));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Filter" })).getByRole("button", {
        name: "Filter by assignee",
      })
    );
    // The popover is portalled onto the body, so it is reached from the screen, not the drawer,
    // and from its own listbox: every open ask's card carries a people picker of its own (the
    // hand-on control), whose options name the same people.
    const listbox = await screen.findByRole("listbox", { name: "Assignee options" });
    await user.click(await within(listbox).findByRole("option", { name: /bob/ }));

    await within(dialog).findByText("Bob's question?");
    expect(screen.getByTestId("location").textContent).toBe("");
    expect(screen.getByRole("dialog", { name: "Inbox" })).toBe(dialog);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    listUsers.mockRestore();
  }
});

test("Snooze inside the drawer folds the row into Later and gives the next row the focus, same as the Inbox page", async () => {
  const user = userEvent.setup();
  const second = issueAsk({
    id: "ask-b",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  let served: InboxRow[] = [issueAsk(), second];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = spyOn(api, "snoozeAsk").mockImplementation(async (id, until) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads([issueAsk(), second]);
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    await within(dialog).findByText("Which approach?");

    const row = within(dialog).getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-a row missing");
    await pickFromMenu(
      user,
      within(row).getByLabelText("Snooze CORE-1: Which approach?"),
      "Tomorrow"
    );

    await waitFor(() =>
      expect(within(dialog).queryByText("Which approach?")).not.toBeInTheDocument()
    );
    expect(snoozeAsk).toHaveBeenCalledTimes(1);
    // R8: the row after the snoozed one has the keyboard focus, inside the drawer.
    await waitFor(() =>
      expect(within(dialog).getByTestId("ask-ask-b").closest("[data-inbox-row]")).toHaveFocus()
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("Answering the only ask inside the drawer submits it, removes it from the list and gives the empty message the focus, same as the Inbox page", async () => {
  const user = userEvent.setup();
  const withOptions = issueAsk({ options: [{ label: "Ship" }, { label: "Hold" }] });
  let served = [withOptions];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([withOptions]);
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = [];
    return {
      ...withOptions,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const card = await within(dialog).findByTestId("ask-ask-a");
    // "Ship" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    await user.click(await within(card).findByRole("radio", { name: "Ship" }));
    await user.click(within(card).getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(within(dialog).queryByTestId("ask-ask-a")).not.toBeInTheDocument());
    expect(answerAsk).toHaveBeenCalledTimes(1);
    // R8: no item is left, so the drawer's empty message has the keyboard focus.
    const empty = within(dialog).getByRole("region", { name: "Inbox empty state" });
    await waitFor(() => expect(empty).toHaveFocus());
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Escape with a row focused backs out of the row first; only a second Escape closes the drawer", async () => {
  const user = userEvent.setup();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    // The drawer is drawn at once, over the inbox's loading skeleton, so the row is waited for.
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const row = (await within(dialog).findByTestId("ask-ask-a")).closest<HTMLElement>(
      "[data-inbox-row]"
    );
    if (row === null) throw new Error("ask-a row missing");
    row.focus();
    expect(row).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Inbox" })).not.toBeNull();
    expect(document.activeElement).not.toBe(row);

    // Nothing is focused now, so the press reaches the keymap's window listener through the body.
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Inbox" })).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// R20: the drawer's `o` has its new-tab form too, and the page under the drawer keeps its address.
test("Shift+O on a row of the drawer opens what o opens in a new tab, and the drawer and the page stay", async () => {
  const user = userEvent.setup();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const open = spyOn(window, "open").mockImplementation(() => null);
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const row = within(dialog).getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-a row missing");
    const owner = row.querySelector<HTMLAnchorElement>("[data-inbox-owner]");
    if (owner === null) throw new Error("the ask-a row draws no owner link");
    row.focus();

    await user.keyboard("{Shift>}O{/Shift}");
    expect(open).toHaveBeenCalledTimes(1);
    expect(open).toHaveBeenCalledWith(owner.href, "_blank", "noopener");
    expect(screen.getByRole("dialog", { name: "Inbox" })).toBe(dialog);
    expect(screen.getByTestId("location").textContent).toBe("");
    expect(row).toHaveFocus();
  } finally {
    view.unmount();
    open.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

// dispatch://INFRA-2160, R24: the drawer's Filter panel and View menu take their keys too, under
// the drawer's own scope, and Escape out of either leaves the drawer open.
test("Shift+F and Shift+V in the drawer open its Filter panel and its View menu, and Escape hands the focus back and keeps the drawer", async () => {
  const user = userEvent.setup();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const row = within(dialog).getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-a row missing");
    act(() => row.focus());

    await user.keyboard("{Shift>}F{/Shift}");
    const panel = screen.getByRole("dialog", { name: "Filter" });
    expect(within(panel).getByRole("button", { name: "Filter by priority" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
    expect(row).toHaveFocus();

    await user.keyboard("{Shift>}V{/Shift}");
    const menu = screen.getByRole("dialog", { name: "View" });
    // Sort by and Thread order both offer Newest first; Sort by's is the menu's first control.
    expect(within(menu).getAllByRole("radio", { name: "Newest first" })[0]).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "View" })).toBeNull();
    expect(row).toHaveFocus();
    expect(screen.getByRole("dialog", { name: "Inbox" })).toBe(dialog);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("at 1440 px the drawer is still today's Inbox: the card in the row, no reading pane", async () => {
  const user = userEvent.setup();
  // A peek is a strip over the page it covers, so `Inbox pane={false}` holds at every width;
  // happy-dom answers `matchMedia` from the window, which is how a width is set here.
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width: 1440 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    await user.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const card = await within(dialog).findByTestId("ask-ask-a");

    expect(card.closest("[data-inbox-row]")?.getAttribute("data-inbox-row")).toBe("ask-a");
    expect(screen.queryByTestId("inbox-pane")).not.toBeInTheDocument();
    expect(document.querySelector("[data-inbox-selected]")).toBeNull();
    // The address under the drawer is still the page's own.
    expect(screen.getByTestId("location").textContent).toBe("");
  } finally {
    view.unmount();
    window_.happyDOM.setViewport({ width: was });
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});
