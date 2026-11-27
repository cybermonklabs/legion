import { expect, spyOn, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { KeymapProvider } from "./KeymapProvider";
import { type KeyBindingDescription, useKeymap } from "./keymap";
import { ShortcutHelp } from "./ShortcutHelp";

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

/** The list of keys over a page whose `1` is a shortcut of its own, as an issue page's `1` writes
 *  its priority; `?` opens the list, as `app.tsx` binds it. */
function ListOverAPage({ onOne }: { onOne: () => void }): ReactNode {
  const [shown, setShown] = useState<readonly KeyBindingDescription[] | null>(null);
  useKeymap("global", [
    { id: "help", keys: "?", label: "Keyboard shortcuts", run: () => setShown(snapshot) },
    { id: "priority-1", keys: "1", label: "Priority 1", run: onOne },
  ]);
  return <ShortcutHelp onClose={() => setShown(null)} snapshot={shown} />;
}

// The list names its filter as the field it opens on (`useDialog`'s `initialFocusRef`), a box
// that takes text, so the filter takes the focus in the commit that opens the list and the keys
// typed straight after `?` are the filter's, not the page's shortcuts. A loaded machine draws the
// list's first frame late, so the row holds the frames while it types: a filter that waited for
// that frame, as a named button does, would leave those keys on the list itself, and the filter
// empty.
test("a key typed in the first frame of the list of keys lands in its filter and reaches no shortcut of the page", async () => {
  const user = userEvent.setup();
  let ones = 0;
  const view = render(
    <MemoryRouter>
      <KeymapProvider>
        <ListOverAPage onOne={() => (ones += 1)} />
      </KeymapProvider>
    </MemoryRouter>
  );
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
  try {
    await user.keyboard("?1");
    expect(ones, "a key typed in the list's first frame reached the page's shortcut").toBe(0);
    expect(
      screen.getByRole("searchbox", { name: "Filter shortcuts" }),
      "the key typed in the list's first frame missed its filter"
    ).toHaveValue("1");
  } finally {
    frames.mockRestore();
    view.unmount();
  }
});
