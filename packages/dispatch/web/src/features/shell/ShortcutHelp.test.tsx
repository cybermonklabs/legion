import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import { actionBinding, createKeymap, type KeyBindingDescription } from "./keymap";
import { catalogueEntry, KEYMAP_ACTIONS, keysOfAction, NO_PERSON_KEYS } from "./keymap-catalogue";
import { KeyHints, SCOPE_LABELS, SCOPE_ORDER, ShortcutHelp } from "./ShortcutHelp";

const snapshot: KeyBindingDescription[] = [
  { enabled: true, id: "next", keys: ["j"], label: "Next component", scope: "architecture" },
  { enabled: true, id: "toggle-view", keys: ["v"], label: "Toggle List / Board", scope: "project" },
  { enabled: true, id: "search", keys: ["$mod+k"], label: "Search", scope: "global" },
];

test("? lists the Architecture section between Project and Board, with its bindings", () => {
  const view = render(
    <MemoryRouter>
      <ShortcutHelp onClose={() => {}} snapshot={snapshot} />
    </MemoryRouter>
  );
  try {
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    const sections = within(dialog).getAllByRole("region");
    expect(sections.map((section) => section.getAttribute("aria-label"))).toEqual([
      "Global",
      "Project",
      "Architecture",
    ]);
    expect(
      within(screen.getByRole("region", { name: "Architecture" })).getByText("Next component")
    ).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("every scope has a section: `?` draws only the scopes `SCOPE_ORDER` lists", () => {
  // `SCOPE_LABELS` is the record the compiler holds to `KeymapScope`; `SCOPE_ORDER` is a plain
  // array nothing holds to it, and a scope left out of it is drawn in no section at all.
  const ordered: string[] = [...SCOPE_ORDER];
  expect(ordered.sort()).toEqual(Object.keys(SCOPE_LABELS).sort());
});

/** Where the router is, as a test reads it. */
function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{`${location.pathname}${location.hash}`}</output>;
}

// R14: the help names where its keys are changed, and following the link closes it.
test("the help links to Settings' Keyboard shortcuts, and a click on the link closes it", async () => {
  const user = userEvent.setup();
  let closed = 0;
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <ShortcutHelp onClose={() => (closed += 1)} snapshot={snapshot} />
      <LocationProbe />
    </MemoryRouter>
  );
  try {
    const link = within(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).getByRole(
      "link",
      { name: "Change these keys in Settings" }
    );
    expect(link.getAttribute("href")).toBe("/settings#keyboard-shortcuts");
    await user.click(link);
    expect(screen.getByTestId("location").textContent).toBe("/settings#keyboard-shortcuts");
    expect(closed).toBeGreaterThan(0);
  } finally {
    view.unmount();
  }
});

// R13: every page's actions are the catalogue's, and `?` draws each scope that has one
// registered, with the catalogue's keys: the Board, the Project, the Architecture page, the Agents
// page and the Delivery list among them.
test("the help draws a section, with the catalogue's keys, for each scope that has a registered action", () => {
  const keymap = createKeymap();
  for (const action of KEYMAP_ACTIONS) {
    keymap.register(catalogueEntry(action).scope, [
      actionBinding({ action, run: () => undefined }, NO_PERSON_KEYS),
    ]);
  }
  const view = render(
    <MemoryRouter>
      <ShortcutHelp onClose={() => {}} snapshot={keymap.describe()} />
    </MemoryRouter>
  );
  try {
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    expect(
      within(dialog)
        .getAllByRole("region")
        .map((section) => section.getAttribute("aria-label"))
    ).toEqual([
      "Global",
      "Project",
      "Architecture",
      "Board",
      "Issue",
      "Document",
      "Inbox",
      "Agents",
      "Delivery",
    ]);
    const unlisted = KEYMAP_ACTIONS.filter((action) => {
      const entry = catalogueEntry(action);
      if (keysOfAction(action, NO_PERSON_KEYS).length === 0) return false;
      const section = within(dialog).getByRole("region", { name: SCOPE_LABELS[entry.scope] });
      return !within(section)
        .getAllByRole("listitem")
        .some((row) => row.firstChild?.textContent === entry.label);
    });
    expect(unlisted).toEqual([]);

    const row = (scope: string, label: string) =>
      within(within(dialog).getByRole("region", { name: scope }))
        .getAllByRole("listitem")
        .find((item) => item.firstChild?.textContent === label);
    const kbds = (scope: string, label: string) =>
      [...(row(scope, label)?.querySelectorAll("kbd") ?? [])].map((kbd) => kbd.textContent);
    expect(kbds("Board", "Move card down")).toEqual(["Shift+J"]);
    expect(kbds("Project", "Open")).toEqual(["o"]);
    expect(kbds("Architecture", "Up one level")).toEqual(["h", "←"]);
    expect(kbds("Agents", "Search agents")).toEqual(["/"]);
    expect(kbds("Delivery", "Next pull request")).toEqual(["↓"]);
    expect(kbds("Inbox", "Snooze the focused ask, or every selected ask")).toEqual(["b", "h"]);
    expect(kbds("Inbox", "Pick an option of the ask in hand")).toEqual(["1", "9", "0"]);
    expect(kbds("Inbox", "Scroll the open document up")).toEqual(["Shift+Space"]);
    expect(kbds("Issue", "Set priority P0–P3")).toEqual(["0", "3"]);
  } finally {
    view.unmount();
  }
});

test("digits in a row are one run of keys, and a 0 after the 9 is the tenth", () => {
  const tenth = render(<KeyHints keys={["1", "2", "3", "4", "5", "6", "7", "8", "9", "0"]} />);
  expect(tenth.container.textContent).toBe("1–9,0");
  tenth.unmount();
  const ten = render(<KeyHints keys={["0", "1", "2", "3", "4", "5", "6", "7", "8", "9"]} />);
  expect(ten.container.textContent).toBe("0–9");
  ten.unmount();
  const space = render(<KeyHints keys={["Space", "Shift+Space"]} />);
  expect(space.container.textContent).toBe("SpaceorShift+Space");
  space.unmount();
});
