import { afterEach, expect, mock, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentProps } from "react";
import type { LegionBoardHeader } from "../../api/types";
import {
  DaemonHint,
  IssueViewTabs,
  LayoutSwitch,
  ToggleButton,
  ViewControls,
} from "./IssueViewTabs";
import { BUILT_IN_VIEWS, checkViewName, type SavedView } from "./issues-views";

afterEach(() => cleanup());

const SAVED: readonly SavedView[] = [
  { name: "Mine", query: "f.author=%40me&cols=key,title&layout=table" },
  { name: "Closed lately", query: "view=done&f.closed=7d&cols=key,title&layout=table" },
];

type TabsProps = ComponentProps<typeof IssueViewTabs>;

/** The tab row on Open, unchanged, with Open counted, Done not yet opened and the saved views
 *  counted as the open listing counts them; `props` changes any of it. */
function tabs(props: Partial<TabsProps> = {}) {
  const handlers = {
    onDelete: mock((_name: string) => {}),
    onSelect: mock((_id: string) => {}),
    onSelectSaved: mock((_name: string) => {}),
  };
  render(
    <IssueViewTabs
      active={{ kind: "built-in", id: "open" }}
      boardFailure={undefined}
      changed={false}
      counts={new Map([["open", 3]])}
      daemonFailure={undefined}
      daemonReadError={undefined}
      factsFailure={undefined}
      saved={SAVED}
      savedCounts={new Map([["Mine", 1]])}
      views={BUILT_IN_VIEWS}
      {...handlers}
      {...props}
    />
  );
  return handlers;
}

test("each tab names its view and its count, the built-in views first, then the saved ones", () => {
  tabs();
  const row = screen.getByRole("tablist", { name: "Views" });
  expect(
    within(row)
      .getAllByRole("tab")
      .map((tab) => tab.getAttribute("aria-label"))
  ).toEqual(["Open, 3 issues", "Done", "Mine, 1 issue", "Closed lately"]);
  expect(screen.getByRole("tab", { name: "Open, 3 issues" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(screen.getByRole("tab", { name: "Done" })).toHaveAttribute("aria-selected", "false");
  // A view with no count draws no number at all, never a 0.
  expect(screen.getByRole("tab", { name: "Done" }).textContent).toBe("Done");
  // No tab name is over 14 characters.
  for (const tab of within(row).getAllByRole("tab"))
    expect((tab.getAttribute("aria-label") ?? "").split(",")[0]?.length).toBeLessThanOrEqual(14);
});

test("the changed tab shows its dot, and a press on it asks for its own view back", async () => {
  const user = userEvent.setup();
  const { onSelect } = tabs({ changed: true });
  const open = screen.getByRole("tab", { name: "Open, 3 issues, changed" });
  expect(open.querySelector("[data-view-changed]")).not.toBeNull();
  // Only the open tab is marked: another view is not changed by the reader's changes.
  expect(screen.getByRole("tab", { name: "Done" }).querySelector("[data-view-changed]")).toBeNull();
  await user.click(open);
  expect(onSelect).toHaveBeenCalledWith("open");
});

test("a press on a saved tab opens that saved view", async () => {
  const user = userEvent.setup();
  const { onSelectSaved } = tabs();
  await user.click(screen.getByRole("tab", { name: "Closed lately" }));
  expect(onSelectSaved).toHaveBeenCalledWith("Closed lately");
});

test("only the open saved tab has a delete button, which deletes that view", async () => {
  const user = userEvent.setup();
  tabs();
  expect(screen.queryByRole("button", { name: /^Delete saved view/ })).toBeNull();
  cleanup();
  const { onDelete } = tabs({
    active: { kind: "saved", name: "Mine" },
    changed: true,
    savedCounts: new Map([["Mine", 2]]),
  });
  expect(screen.getByRole("tab", { name: "Mine, 2 issues, changed" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  const remove = screen.getByRole("button", { name: "Delete saved view Mine" });
  expect(remove).toHaveAttribute("title", "Delete this saved view");
  expect(screen.queryByRole("button", { name: "Delete saved view Closed lately" })).toBeNull();
  // The tablist holds tabs alone: the delete button follows it in the row.
  const tablist = screen.getByRole("tablist", { name: "Views" });
  expect(tablist.contains(remove)).toBe(false);
  expect([...tablist.children].map((child) => child.getAttribute("role"))).toEqual([
    "tab",
    "tab",
    "tab",
    "tab",
  ]);
  await user.click(remove);
  expect(onDelete).toHaveBeenCalledWith("Mine");
});

// A speech user says the words a tab shows. So its name starts with them, in their order (WCAG
// 2.5.3, axe's label-content-name-mismatch): "Open 3" in "Open, 3 issues", never "Open3".
test("each tab's name starts with the words the tab shows, in their order", () => {
  tabs({ changed: true });
  const row = screen.getByRole("tablist", { name: "Views" });
  expect(
    within(row)
      .getAllByRole("tab")
      .map((tab) => tab.textContent)
  ).toEqual(["Open 3", "Done", "Mine 1", "Closed lately"]);
  for (const tab of within(row).getAllByRole("tab")) {
    const shown = (tab.textContent ?? "").split(/\s+/).filter(Boolean);
    const named = (tab.getAttribute("aria-label") ?? "").split(/[\s,]+/).filter(Boolean);
    expect(named.slice(0, shown.length)).toEqual(shown);
  }
});

test("no alert line is drawn while every read answered", () => {
  tabs();
  expect(screen.queryByRole("alert")).toBeNull();
});

test("a failed daemon setting read names its error, with a Retry that reads it again", async () => {
  const user = userEvent.setup();
  const retry = mock(() => {});
  tabs({ daemonFailure: { message: "The store did not answer.", retry, retrying: false } });
  expect(screen.getByRole("alert").textContent).toBe(
    "Could not read the Legion daemon setting: The store did not answer. Retry"
  );
  await user.click(screen.getByRole("button", { name: "Retry reading the Legion daemon setting" }));
  expect(retry).toHaveBeenCalledTimes(1);
  cleanup();
  // While the read is out again, Retry waits for it: QueryError holds the press
  // (`heldPress`, aria-disabled) and keeps the button in the Tab order.
  tabs({ daemonFailure: { message: "The store did not answer.", retry, retrying: true } });
  const held = screen.getByRole("button", { name: "Retry reading the Legion daemon setting" });
  expect(held).toHaveAttribute("aria-disabled", "true");
  await user.click(held);
  expect(retry).toHaveBeenCalledTimes(1);
});

test("a failed daemon read names its whole error on a line of its own", () => {
  const error =
    "dial tcp 10.0.0.9:7400: connect: connection refused (the daemon's state route did not answer)";
  tabs({ daemonReadError: error });
  const alert = screen.getByRole("alert");
  expect(alert.textContent).toBe(`The last read of the Legion daemon failed: ${error}`);
  expect(within(alert).queryByRole("button")).toBeNull();
  cleanup();
  // The daemon gave no words for it.
  tabs({ daemonReadError: "" });
  expect(screen.getByRole("alert").textContent).toBe("The last read of the Legion daemon failed.");
});

test("a failed board read names its error, with a Retry that reads it again", async () => {
  const user = userEvent.setup();
  const retry = mock(() => {});
  tabs({ boardFailure: { message: "The board did not answer.", retry, retrying: false } });
  expect(screen.getByRole("alert").textContent).toBe(
    "Could not read the Legion board: The board did not answer. Retry"
  );
  await user.click(screen.getByRole("button", { name: "Retry reading the Legion board" }));
  expect(retry).toHaveBeenCalledTimes(1);
});

test("a failed items read names its error, with a Retry that reads the facts again", async () => {
  const user = userEvent.setup();
  const retry = mock(() => {});
  tabs({ factsFailure: { message: "No eval legion-work-time.", retry, retrying: false } });
  expect(screen.getByRole("alert").textContent).toBe(
    "Could not read the Legion columns of the closed issues: No eval legion-work-time. Retry"
  );
  await user.click(screen.getByRole("button", { name: "Retry reading the Legion columns" }));
  expect(retry).toHaveBeenCalledTimes(1);
});

test("every failed read is a line of its own, in one place under the tabs", () => {
  const retry = () => {};
  tabs({
    boardFailure: { message: "b", retry, retrying: false },
    daemonFailure: { message: "a", retry, retrying: false },
    daemonReadError: "c",
    factsFailure: { message: "d", retry, retrying: false },
  });
  expect(screen.getAllByRole("alert").map((alert) => alert.textContent)).toEqual([
    "Could not read the Legion daemon setting: a Retry",
    "The last read of the Legion daemon failed: c",
    "Could not read the Legion board: b Retry",
    "Could not read the Legion columns of the closed issues: d Retry",
  ]);
});

// Save view keeps its room in the control row: it is drawn hidden while the view is its own, so
// its showing moves nothing, and it shows while the view is changed.
test("Save view keeps its room while the view is its own, and checks a name before it saves", async () => {
  const user = userEvent.setup();
  const onSave = mock((_name: string) => {});
  const check = (name: string) => checkViewName(name, SAVED);
  const { rerender } = render(
    <ViewControls changed={false} check={check} initialName="" onSave={onSave} />
  );
  expect(screen.queryByRole("button", { name: "Save view" })).toBeNull();
  rerender(<ViewControls changed check={check} initialName="" onSave={onSave} />);
  await user.click(screen.getByRole("button", { name: "Save view" }));
  const box = screen.getByRole("textbox", { name: "Name of the view" });
  await user.type(box, "Open");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(screen.getByRole("alert").textContent).toBe(
    "Open is a built-in view. Type a different name."
  );
  expect(onSave).not.toHaveBeenCalled();
  await user.clear(box);
  await user.type(box, "mine");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(screen.getByRole("alert").textContent).toBe("A saved view has the name Mine. Replace it?");
  await user.click(screen.getByRole("button", { name: "Replace" }));
  expect(onSave).toHaveBeenCalledWith("mine");
  expect(screen.queryByRole("textbox", { name: "Name of the view" })).toBeNull();
});

test("the layout switch and a Board toggle say which choice is pressed", async () => {
  const user = userEvent.setup();
  const onLayout = mock((_layout: string) => {});
  const onToggle = mock((_pressed: boolean) => {});
  render(
    <>
      <LayoutSwitch layout="table" onLayout={onLayout} />
      <ToggleButton onChange={onToggle} pressed={false}>
        Show Icebox
      </ToggleButton>
    </>
  );
  expect(screen.getByRole("button", { name: "Table" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "false");
  await user.click(screen.getByRole("button", { name: "Board" }));
  expect(onLayout).toHaveBeenCalledWith("board");
  const toggle = screen.getByRole("button", { name: "Show Icebox" });
  expect(toggle).toHaveAttribute("aria-pressed", "false");
  await user.click(toggle);
  expect(onToggle).toHaveBeenCalledWith(true);
});

/** The board's header after a good read five minutes ago: 3 issues run of a cap of 4, 2 trees. */
function header(overrides: Partial<LegionBoardHeader> = {}): LegionBoardHeader {
  return {
    facts_cursor_age_seconds: 12,
    facts_updated_at: null,
    queue_length: 5,
    read_at: new Date(Date.now() - 5 * 60_000).toISOString(),
    read_error: null,
    read_ok: true,
    slot_cap: 4,
    slots_in_use: 3,
    trees: 2,
    ...overrides,
  };
}

/** Opens the (i) hint and gives its panel. */
async function openHint(): Promise<HTMLElement> {
  const trigger = screen.getByRole("button", { name: "Legion daemon" });
  expect(trigger).toHaveAttribute("title", "Legion daemon");
  expect(trigger).toHaveAttribute("data-control", "view-panel");
  await userEvent.setup().click(trigger);
  return screen.getByRole("dialog", { name: "Legion daemon" });
}

/** Each line of the hint: its name and its value. */
const hintLines = (panel: HTMLElement) =>
  [...panel.querySelectorAll("dt")].map((name) => [
    name.textContent,
    name.nextElementSibling?.textContent,
  ]);

// R1, R2: the (i) hint gives the capacity in use of its cap, the trees and the last read, and no
// other text: no queue length, and no word "slot".
test("the (i) hint gives Capacity of the cap, Trees and Last read, and no other text", async () => {
  render(<DaemonHint header={header()} reading={false} />);
  const panel = await openHint();
  expect(hintLines(panel)).toEqual([
    ["Capacity", "3 of 4"],
    ["Trees", "2"],
    ["Last read", "5 minutes ago"],
  ]);
  expect(panel.textContent).toBe("Capacity3 of 4Trees2Last read5 minutes ago");
  expect(document.body.innerHTML).not.toMatch(/slot/i);
});

// D13: a good read whose header names no cap gives the issues in use alone, and Trees its 0.
test("after a good read with no cap the hint gives Capacity alone, and Trees 0", async () => {
  render(
    <DaemonHint header={header({ slot_cap: null, slots_in_use: 4, trees: 0 })} reading={false} />
  );
  const panel = await openHint();
  expect(hintLines(panel)).toEqual([
    ["Capacity", "4"],
    ["Trees", "0"],
    ["Last read", "5 minutes ago"],
  ]);
  expect(document.body.innerHTML).not.toMatch(/slot/i);
});

test("before any read of the daemon the hint says only that", async () => {
  render(
    <DaemonHint
      header={header({ read_at: null, read_ok: false, slot_cap: null, slots_in_use: 0, trees: 0 })}
      reading={false}
    />
  );
  const panel = await openHint();
  expect(panel.textContent).toBe("Dispatch has not read the daemon yet.");
  expect(panel.querySelector("dl")).toBeNull();
  expect(document.body.innerHTML).not.toMatch(/slot/i);
});

// Before the board answers, the hint says the read is out; after a failed board read, it says
// so in the alert line's words.
test("before the board answers the hint says it loads, and after a failed board read it says so", async () => {
  const { rerender } = render(<DaemonHint header={undefined} reading />);
  const panel = await openHint();
  expect(panel.textContent).toBe("Loading…");
  rerender(<DaemonHint header={undefined} reading={false} />);
  expect(screen.getByRole("dialog", { name: "Legion daemon" }).textContent).toBe(
    "Could not read the Legion board."
  );
});
