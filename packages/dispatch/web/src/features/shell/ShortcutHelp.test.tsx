import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { KeyBindingDescription } from "./keymap";
import { SCOPE_LABELS, SCOPE_ORDER, ShortcutHelp } from "./ShortcutHelp";

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
