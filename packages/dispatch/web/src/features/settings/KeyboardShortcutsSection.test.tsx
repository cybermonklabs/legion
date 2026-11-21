import { afterEach, expect, test } from "bun:test";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { jump, onFakeClock } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { KeymapProvider } from "../shell/KeymapProvider";
import { CHORD_TIMEOUT_MS, comboOf, createKeymap } from "../shell/keymap";
import { KEYMAP_ACTIONS, type KeymapActionId } from "../shell/keymap-catalogue";
import { setActionKeys } from "../shell/keymap-keys";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { KeyboardShortcutsSection } from "./KeyboardShortcutsSection";

const ALICE_KEY = userPreferenceStorageKey("alice", "shell.keymap");

afterEach(() => {
  window.localStorage.removeItem(ALICE_KEY);
});

/** The section as Settings draws it, for alice or for nobody signed in, at `address`. */
function drawSection({ address = "/settings", signedIn = true } = {}) {
  return render(
    <MemoryRouter initialEntries={[address]}>
      <KeymapProvider login={signedIn ? "alice" : undefined}>
        <KeyboardShortcutsSection />
      </KeymapProvider>
    </MemoryRouter>
  );
}

/** The section's line for `action`. */
function line(action: KeymapActionId): HTMLElement {
  const found = document.querySelector<HTMLElement>(`li[data-keymap-action="${action}"]`);
  if (found === null) throw new Error(`the section lists no ${action}`);
  return found;
}

/** A control of the line for `action`, by its name. */
function control(action: KeymapActionId, name: string): HTMLElement {
  return within(line(action)).getByRole("button", { name });
}

/** The keys a line draws, as the reader sees them. */
function keysOf(action: KeymapActionId): (string | null)[] {
  return [...line(action).querySelectorAll("kbd")].map((kbd) => kbd.textContent);
}

/** What this browser keeps for alice: the actions she changed and their keys. */
function stored(): unknown {
  return JSON.parse(window.localStorage.getItem(ALICE_KEY) ?? "{}");
}

const RECORDING = "Press the new key… Esc cancels";

test("every action is listed under its scope, in the help's order, with its keys, whether or not its page is open", () => {
  const view = drawSection();
  try {
    const section = screen.getByRole("region", { name: "Keyboard shortcuts" });
    expect(
      within(section)
        .getAllByRole("heading", { level: 3 })
        .map((heading) => heading.textContent)
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
    expect(section.querySelectorAll("li[data-keymap-action]")).toHaveLength(KEYMAP_ACTIONS.length);
    const board = within(section).getByRole("region", { name: "Board" });
    expect(
      within(board).getByRole("listitem", { name: "Move card down" }).querySelector("kbd")
        ?.textContent
    ).toBe("Shift+J");
    expect(keysOf("inbox.snooze")).toEqual(["b", "h"]);
    expect(control("inbox.snooze", "Change b")).toBeDefined();
    expect(control("inbox.snooze", "Remove h")).toBeDefined();
    // The digits keep their own action: drawn, and changed by nothing.
    expect(keysOf("inbox.option")).toEqual(["1", "9", "0"]);
    expect(within(line("inbox.option")).queryAllByRole("button")).toEqual([]);
    expect(line("inbox.option").textContent).toContain("Fixed");
    // An action with no default key says so, and takes one.
    expect(line("global.go-priorities").textContent).toContain("No key");
    expect(control("global.go-priorities", "Add key")).toBeDefined();
    // The Agents page's `/` and `i` are global keys too, so the defaults carry the notice.
    for (const action of ["agents.search", "global.search-only"] as const) {
      expect(line(action).textContent).toContain("Search does not work on the Agents page.");
    }
    for (const action of ["agents.issue-picker", "global.inbox-drawer"] as const) {
      expect(line(action).textContent).toContain(
        "Open the inbox does not work on the Agents page."
      );
    }
  } finally {
    view.unmount();
  }
});

// The new issue page's keys are the catalogue's (R14): its outline's are listed under Issue, its
// chords are today's tab actions, and Details' key is listed under Global, each with the keys it
// has. Today's issue page and the new one are never drawn together, nor the margin and Details,
// so the keys they share carry no note.
test("the new issue page's actions are listed under their scopes with their default keys", () => {
  const view = drawSection();
  try {
    const section = screen.getByRole("region", { name: "Keyboard shortcuts" });
    const issue = within(section).getByRole("region", { name: "Issue" });
    const listed: readonly (readonly [KeymapActionId, readonly string[]])[] = [
      ["issue.tree-enter", ["Tab", "Shift+Tab"]],
      ["issue.tree-next", ["j", "↓"]],
      ["issue.tree-previous", ["k", "↑"]],
      ["issue.tree-first", ["Home"]],
      ["issue.tree-last", ["End"]],
      ["issue.tree-unfold", ["→"]],
      ["issue.tree-fold", ["←"]],
      ["issue.tree-pin", ["o"]],
      ["issue.tree-keep", ["Shift+K"]],
      ["issue.tree-change", ["Shift+C"]],
      ["issue.tree-link", ["f"]],
      ["issue.tree-zoom-in", ["."]],
      ["issue.tree-zoom-out", [","]],
      ["issue.tree-question", ["q"]],
      ["issue.tree-decision", ["w"]],
      ["issue.tree-reply", ["r"]],
      ["issue.tree-answer", ["a"]],
      ["issue.tree-cycle", ["z"]],
      ["issue.tree-edit", ["Enter"]],
      ["issue.tree-leave-caret", ["Esc"]],
      ["issue.tree-leave", ["Esc"]],
      ["issue.tab-spec", ["t", "s"]],
      ["issue.tab-verification", ["t", "k"]],
      ["issue.tab-plan", ["t", "p"]],
      ["issue.tab-decisions", ["t", "d"]],
      ["issue.tab-conversation", ["t", "c"]],
      ["issue.tab-children", ["t", "h"]],
    ];
    for (const [action, keys] of listed) {
      expect(issue.contains(line(action)), action).toBe(true);
      expect(keysOf(action), action).toEqual([...keys]);
    }
    const global = within(section).getByRole("region", { name: "Global" });
    expect(global.contains(line("global.toggle-details"))).toBe(true);
    expect(keysOf("global.toggle-details")).toEqual(["Shift+M"]);
    for (const action of [
      "issue.tree-next",
      "issue.next-turn",
      "global.toggle-details",
      "global.toggle-margin",
    ] as const) {
      expect(line(action).querySelector("p"), action).toBeNull();
    }
  } finally {
    view.unmount();
  }
});

// The header's keys are drawn on both issue pages: a key of today's Conversation tab is refused
// one as it always was, and so is a key of the new page's outline, each naming the header's
// action.
test("a key of today's issue page or of the new page's outline is refused a header key, naming the header's action", async () => {
  const view = drawSection();
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      await user.click(control("issue.next-turn", "Change j"));
      await user.keyboard("s");
      await jump(CHORD_TIMEOUT_MS);
      expect(within(line("issue.next-turn")).getByRole("alert").textContent).toContain(
        "s is already the key of Open the status menu."
      );
      await user.keyboard("{Escape}");

      await user.click(control("issue.tree-link", "Change f"));
      await user.keyboard("s");
      await jump(CHORD_TIMEOUT_MS);
      expect(within(line("issue.tree-link")).getByRole("alert").textContent).toContain(
        "s is already the key of Open the status menu."
      );
      await user.keyboard("{Escape}");
    });
    expect(stored()).toEqual({});
    expect(keysOf("issue.next-turn")).toEqual(["j"]);
    expect(keysOf("issue.tree-link")).toEqual(["f"]);
  } finally {
    view.unmount();
  }
});

test("two presses within one second are one key of two presses, and no page key acts on either", async () => {
  // In the capture phase, before the recorder: what the page's keymap would read, which skips a
  // press a handler took (`defaultPrevented`); and, in the bubble phase, whether one reached it.
  const pressed: KeyboardEvent[] = [];
  const reached: string[] = [];
  const press = (event: KeyboardEvent) => pressed.push(event);
  const reach = (event: KeyboardEvent) => reached.push(event.key);
  window.addEventListener("keydown", press, { capture: true });
  window.addEventListener("keydown", reach);
  const view = drawSection();
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      const add = control("global.go-priorities", "Add key");
      await user.click(add);
      expect(document.activeElement).toBe(add);
      expect(add.textContent).toBe(RECORDING);
      await user.keyboard("g");
      expect(add.textContent).toBe("g then … Esc cancels");
      await jump(CHORD_TIMEOUT_MS - 100);
      await user.keyboard("r");
    });
    expect(stored()).toEqual({ "global.go-priorities": ["g r"] });
    expect(keysOf("global.go-priorities")).toEqual(["g", "r"]);
    expect(line("global.go-priorities").textContent).toContain("Added");
    // The control shows the words its name reads, which a voice user says to press it.
    expect(control("global.go-priorities", "Change g then r").textContent).toBe("g then r");
    expect(pressed.map((event) => [event.key, event.defaultPrevented])).toEqual([
      ["g", true],
      ["r", true],
    ]);
    expect(reached).toEqual([]);
  } finally {
    view.unmount();
    window.removeEventListener("keydown", press, { capture: true });
    window.removeEventListener("keydown", reach);
  }
});

test("Change records the next press in the key's place, and a modifier alone is no press", async () => {
  const view = drawSection();
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      const change = control("inbox.snooze", "Change b");
      await user.click(change);
      expect(document.activeElement).toBe(change);
      await user.keyboard("{Shift}");
      await jump(CHORD_TIMEOUT_MS);
      expect(change.textContent).toBe(RECORDING);
      await user.keyboard("{Shift>}B{/Shift}");
      await jump(CHORD_TIMEOUT_MS);
      // The same control, which kept the focus, now holds the new key.
      expect(document.activeElement).toBe(change);
    });
    expect(stored()).toEqual({ "inbox.snooze": ["Shift+B", "h"] });
    expect(keysOf("inbox.snooze")).toEqual(["Shift+B", "h"]);
    expect(line("inbox.snooze").textContent).toContain("Changed from b or h");
  } finally {
    view.unmount();
  }
});

test("a key another action holds is refused with that action's name, and the recorder waits for another", async () => {
  const view = drawSection();
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      const add = control("inbox.snooze", "Add key");
      await user.click(add);
      const refusal = () => within(line("inbox.snooze")).getByRole("alert");

      await user.keyboard("e");
      await jump(CHORD_TIMEOUT_MS);
      expect(refusal().textContent).toContain("Dismiss, or Move to Inbox");
      expect(add.textContent).toBe(RECORDING);
      expect(add.getAttribute("aria-describedby")).toBe(refusal().id);

      // Two presses a second apart are two keys: `g` alone, the first press of Go to Inbox's.
      await user.keyboard("g");
      await jump(CHORD_TIMEOUT_MS);
      expect(refusal().textContent).toContain("Go to Inbox");

      await user.keyboard("1");
      await jump(CHORD_TIMEOUT_MS);
      expect(refusal().textContent).toContain("1 is not free");
      expect(stored()).toEqual({});

      await user.keyboard("c");
      await jump(CHORD_TIMEOUT_MS);
    });
    expect(within(line("inbox.snooze")).queryByRole("alert")).toBeNull();
    expect(stored()).toEqual({ "inbox.snooze": ["b", "h", "c"] });
    expect(keysOf("inbox.snooze")).toEqual(["b", "h", "c"]);
    for (const action of ["inbox.snooze", "global.create"] as const) {
      expect(line(action).textContent).toContain("Create issue does not work on the Inbox.");
    }
  } finally {
    view.unmount();
  }
});

test("an Alt letter on a Mac is read by its key's place, and the key it writes presses it again", () => {
  const optionM = { altKey: true, code: "KeyM", ctrlKey: false, key: "µ", metaKey: false };
  expect(comboOf({ ...optionM, shiftKey: false }, true)).toBe("Alt+m");
  expect(comboOf({ ...optionM, key: "Â", shiftKey: true }, true)).toBe("Alt+Shift+M");
  // Elsewhere Alt leaves the character as it is, and the key is the character.
  expect(comboOf({ ...optionM, shiftKey: false }, false)).toBe("Alt+µ");

  const ran: string[] = [];
  const keymap = createKeymap({ isMac: true });
  keymap.register("global", [
    {
      id: "recorded",
      keys: comboOf({ ...optionM, shiftKey: false }, true) ?? "",
      label: "Recorded",
      run: (event) => ran.push(event.key),
    },
  ]);
  keymap.handleKeyDown(new KeyboardEvent("keydown", { ...optionM, shiftKey: false }));
  expect(ran).toEqual(["µ"]);

  // The other presses, as a binding writes them.
  const plain = { altKey: false, code: "", ctrlKey: false, metaKey: false, shiftKey: false };
  expect(comboOf({ ...plain, key: "k", metaKey: true }, true)).toBe("$mod+k");
  expect(comboOf({ ...plain, ctrlKey: true, key: "k" }, false)).toBe("$mod+k");
  expect(comboOf({ ...plain, ctrlKey: true, key: "k" }, true)).toBe("Control+k");
  expect(comboOf({ ...plain, key: "B", shiftKey: true }, false)).toBe("Shift+B");
  expect(comboOf({ ...plain, key: "?", shiftKey: true }, false)).toBe("?");
  expect(comboOf({ ...plain, key: " ", shiftKey: true }, false)).toBe("Shift+Space");
  expect(comboOf({ ...plain, key: "Shift", shiftKey: true }, false)).toBeNull();
  expect(comboOf({ ...plain, key: "Dead" }, false)).toBeNull();
});

// Option with E, I, U or N types a dead key on a Mac (`key: "Dead"`), which names nothing a
// binding can hold, but the press is the letter at its place, as the registry matches it.
test("an Option key that types a dead key on a Mac is read by its key's place", () => {
  const optionE = { altKey: true, code: "KeyE", ctrlKey: false, key: "Dead", metaKey: false };
  expect(comboOf({ ...optionE, shiftKey: false }, true)).toBe("Alt+e");
  // A place that is no letter or digit, Option+` among them, names no key, and the registry
  // matches no key by such a place either.
  expect(comboOf({ ...optionE, code: "Backquote", shiftKey: false }, true)).toBeNull();
  // Elsewhere Alt leaves the character as it is, and a dead key is no press.
  expect(comboOf({ ...optionE, shiftKey: false }, false)).toBeNull();

  const ran: string[] = [];
  const keymap = createKeymap({ isMac: true });
  keymap.register("global", [
    {
      id: "recorded",
      keys: comboOf({ ...optionE, shiftKey: false }, true) ?? "",
      label: "Recorded",
      run: (event) => ran.push(event.code),
    },
  ]);
  keymap.handleKeyDown(new KeyboardEvent("keydown", { ...optionE, shiftKey: false }));
  expect(ran).toEqual(["KeyE"]);
});

test("Remove takes one key away, and Reset gives the action its defaults again", async () => {
  const user = userEvent.setup();
  const view = drawSection();
  try {
    const add = control("inbox.copy-ref", "Add key");
    await user.click(control("inbox.copy-ref", "Remove y"));
    expect(stored()).toEqual({ "inbox.copy-ref": [] });
    expect(keysOf("inbox.copy-ref")).toEqual([]);
    expect(line("inbox.copy-ref").textContent).toContain("No key");
    expect(line("inbox.copy-ref").textContent).toContain("Changed from y");
    // The control pressed went with the key, so the focus is on the action's Add key.
    expect(document.activeElement).toBe(add);

    await user.click(control("inbox.copy-ref", "Reset"));
    expect(window.localStorage.getItem(ALICE_KEY)).toBeNull();
    expect(keysOf("inbox.copy-ref")).toEqual(["y"]);
    expect(line("inbox.copy-ref").hasAttribute("data-changed")).toBe(false);
    expect(within(line("inbox.copy-ref")).queryByRole("button", { name: "Reset" })).toBeNull();
    expect(document.activeElement).toBe(add);
  } finally {
    view.unmount();
  }
});

// R14: a Reset gives back keys another action may hold now; it is refused, naming that action,
// so no action loses a key the person gave it.
test("Reset is refused, naming the action, where another action holds one of its defaults now", async () => {
  const user = userEvent.setup();
  // Dismiss gave up `e`, which Snooze then took; Create issue gave up `c`, which Copy reference
  // took as the first press of a key of two.
  setActionKeys("alice", "inbox.dismiss", []);
  setActionKeys("alice", "inbox.snooze", ["b", "h", "e"]);
  setActionKeys("alice", "global.create", []);
  setActionKeys("alice", "inbox.copy-ref", ["c x"]);
  const given = {
    "global.create": [],
    "inbox.copy-ref": ["c x"],
    "inbox.dismiss": [],
    "inbox.snooze": ["b", "h", "e"],
  };
  const view = drawSection();
  try {
    const reset = control("inbox.dismiss", "Reset");
    await user.click(reset);
    const refusal = within(line("inbox.dismiss")).getByRole("alert");
    expect(refusal.textContent).toBe(
      "Reset is refused: e is already the key of Snooze the focused ask, or every selected ask. Change that action's keys first."
    );
    expect(reset.getAttribute("aria-describedby")).toBe(refusal.id);
    expect(document.activeElement).toBe(reset);
    expect(stored()).toEqual(given);
    expect(keysOf("inbox.snooze")).toEqual(["b", "h", "e"]);
    expect(keysOf("inbox.dismiss")).toEqual([]);

    // A default that is the first press of a key another action holds now.
    await user.click(control("global.create", "Reset"));
    expect(within(line("global.create")).getByRole("alert").textContent).toBe(
      "Reset is refused: c is the first press of c x, Copy the row's reference. Change that action's keys first."
    );
    expect(stored()).toEqual(given);
    expect(keysOf("inbox.copy-ref")).toEqual(["c", "x"]);

    // Once the person takes the key from the other action, Reset gives the defaults back.
    await user.click(control("inbox.snooze", "Remove e"));
    expect(within(line("global.create")).queryByRole("alert")).toBeNull();
    await user.click(control("inbox.dismiss", "Reset"));
    expect(within(line("inbox.dismiss")).queryByRole("alert")).toBeNull();
    expect(keysOf("inbox.dismiss")).toEqual(["e"]);
    expect(keysOf("inbox.snooze")).toEqual(["b", "h"]);
    // Snooze's keys are its defaults again, so the map holds no word of Snooze or Dismiss.
    expect(stored()).toEqual({ "global.create": [], "inbox.copy-ref": ["c x"] });
  } finally {
    view.unmount();
  }
});

test("Reset all gives every action its defaults again", async () => {
  const user = userEvent.setup();
  setActionKeys("alice", "inbox.snooze", ["Shift+B", "h"]);
  setActionKeys("alice", "global.create", ["n"]);
  setActionKeys("alice", "global.go-priorities", ["g r"]);
  const view = drawSection();
  try {
    expect(keysOf("inbox.snooze")).toEqual(["Shift+B", "h"]);
    expect(document.querySelectorAll("li[data-changed]")).toHaveLength(3);
    await user.click(screen.getByRole("button", { name: "Reset all" }));
    expect(window.localStorage.getItem(ALICE_KEY)).toBeNull();
    expect(document.querySelectorAll("li[data-changed]")).toHaveLength(0);
    expect(keysOf("inbox.snooze")).toEqual(["b", "h"]);
    expect(keysOf("global.create")).toEqual(["c"]);
    expect(keysOf("global.go-priorities")).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("Escape, and Tab out of the recorder, end a recording with nothing changed", async () => {
  const view = drawSection();
  try {
    await onFakeClock(async () => {
      const user = userOnFakeClock();
      const add = control("inbox.snooze", "Add key");
      await user.click(add);
      await user.keyboard("q{Escape}");
      await jump(CHORD_TIMEOUT_MS);
      expect(add.textContent).toBe("Add key");
      expect(document.activeElement).toBe(add);

      await user.click(add);
      await user.keyboard("q");
      await user.tab();
      await jump(CHORD_TIMEOUT_MS);
      expect(add.textContent).toBe("Add key");
      expect(document.activeElement).not.toBe(add);
    });
    expect(stored()).toEqual({});
  } finally {
    view.unmount();
  }
});

test("with nobody signed in the section lists every key and changes none", () => {
  const view = drawSection({ signedIn: false });
  try {
    const section = screen.getByRole("region", { name: "Keyboard shortcuts" });
    expect(section.textContent).toContain("Sign in to change them.");
    expect(keysOf("inbox.snooze")).toEqual(["b", "h"]);
    expect(within(section).queryAllByRole("button")).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("an address that names the section moves the focus to it", async () => {
  const view = drawSection({ address: "/settings#keyboard-shortcuts" });
  try {
    const section = screen.getByRole("region", { name: "Keyboard shortcuts" });
    await waitFor(() => expect(document.activeElement).toBe(section));
  } finally {
    view.unmount();
  }
});
