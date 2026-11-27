import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { KeymapProvider } from "./KeymapProvider";
import { IS_MAC, keyTokens } from "./key-text";
import { RailButton, railText } from "./RailButton";
import { userPreferenceStorageKey } from "./userPreference";

test("RailButton shows its action's keys, announces them and opens what it stands for", async () => {
  const user = userEvent.setup();
  let opened = 0;
  const view = render(
    <>
      <RailButton
        action="global.create"
        className="mt-1"
        label="New issue"
        onOpen={() => (opened += 1)}
      />
      <RailButton action="global.search" className="mt-4" label="Search" onOpen={() => {}} />
    </>
  );

  try {
    const button = screen.getByRole("button", { name: /new issue/i });
    expect(button).toHaveAttribute("aria-keyshortcuts", "C");
    expect(button.querySelector("kbd")?.textContent).toBe("c");
    await user.click(button);
    expect(opened).toBe(1);

    const search = screen.getByRole("button", { name: /search/i });
    expect(search).toHaveAttribute("aria-keyshortcuts", IS_MAC ? "Meta+K" : "Control+K");
    expect(search.querySelector("kbd")?.textContent).toBe(IS_MAC ? "⌘K" : "Ctrl K");
  } finally {
    view.unmount();
  }
});

test("RailButton shows the person's own keys for its action, and none where they took them all", () => {
  const stored = userPreferenceStorageKey("alice", "shell.keymap");
  window.localStorage.setItem(
    stored,
    JSON.stringify({ "global.create": ["n"], "global.search": [] })
  );
  const view = render(
    <KeymapProvider login="alice">
      <RailButton action="global.create" className="mt-1" label="New issue" onOpen={() => {}} />
      <RailButton action="global.search" className="mt-4" label="Search" onOpen={() => {}} />
    </KeymapProvider>
  );

  try {
    const button = screen.getByRole("button", { name: /new issue/i });
    expect(button).toHaveAttribute("aria-keyshortcuts", "N");
    expect(button.querySelector("kbd")?.textContent).toBe("n");
    const search = screen.getByRole("button", { name: "Search" });
    expect(search).not.toHaveAttribute("aria-keyshortcuts");
    expect(search.querySelector("kbd")).toBeNull();
  } finally {
    view.unmount();
    window.localStorage.removeItem(stored);
  }
});

// The rail draws a key in the glyphs `?` and Settings draw (`keyTokens`), running a press's parts
// together as `⌘Esc` on a Mac and spacing them as `Ctrl Esc` elsewhere.
test("RailButton draws a person's key in the help's glyphs", () => {
  const stored = userPreferenceStorageKey("alice", "shell.keymap");
  window.localStorage.setItem(stored, JSON.stringify({ "global.search": ["$mod+Escape"] }));
  const view = render(
    <KeymapProvider login="alice">
      <RailButton action="global.search" className="mt-4" label="Search" onOpen={() => {}} />
    </KeymapProvider>
  );

  try {
    const search = screen.getByRole("button", { name: /^Search/ });
    expect(keyTokens("$mod+Escape")[0]?.token).toBe(IS_MAC ? "⌘+Esc" : "Ctrl+Esc");
    expect(search.querySelector("kbd")?.textContent).toBe(IS_MAC ? "⌘Esc" : "Ctrl Esc");
  } finally {
    view.unmount();
    window.localStorage.removeItem(stored);
  }
});

// The rail reads the table `?` and Settings read (`keyTokens`) and keeps only its own joiner: on a
// Mac a press's parts run together and a letter after a modifier is upper case; elsewhere they
// are spaced. So the modifiers the rail draws are the parts `keyTokens` gives, the key left out.
test("the rail draws Control and Shift in the glyphs of ? and Settings, on a Mac and elsewhere", () => {
  const key = "Control+Shift+e";
  for (const [isMac, drawn, modifiersOf] of [
    [true, "⌃⇧E", (text: string) => [...text].slice(0, -1)],
    [false, "Ctrl Shift E", (text: string) => text.split(" ").slice(0, -1)],
  ] as const) {
    const rail = railText(key, isMac);
    expect(rail, `isMac ${isMac}`).toBe(drawn);
    expect(modifiersOf(rail), `isMac ${isMac}`).toEqual(
      keyTokens(key, isMac)[0]?.parts.slice(0, -1) ?? []
    );
  }
});
