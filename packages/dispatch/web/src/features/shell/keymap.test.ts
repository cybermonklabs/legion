import { expect, test } from "bun:test";
import { act, render } from "@testing-library/react";
import { createElement } from "react";

import { MENU_TRIGGER_ATTRIBUTE, PANEL_ATTRIBUTE } from "../../components/key-targets";
import { KeymapProvider } from "./KeymapProvider";
import {
  actionBinding,
  appKeymap,
  ariaKeyshortcuts,
  CHORD_TIMEOUT_MS,
  createKeymap,
  DIALOG_SCOPE,
  type KeyBinding,
  type Keymap,
  KeymapGateContext,
  modHeld,
  useKeymap,
  useKeymapScope,
} from "./keymap";
import { type KeymapActionId, NO_PERSON_KEYS } from "./keymap-catalogue";
import { setActionKeys } from "./keymap-keys";
import { userPreferenceStorageKey } from "./userPreference";

// A handler outside the registry that takes a `$mod` key itself (the comment box's Ctrl+K) reads
// the press as the registry reads `$mod`: Cmd alone on a Mac, where a text field keeps Control+K,
// and Ctrl alone elsewhere.
test("modHeld is Cmd alone on a Mac and Ctrl alone elsewhere", () => {
  const press = (ctrlKey: boolean, metaKey: boolean) => ({ ctrlKey, metaKey });
  expect(modHeld(press(false, true), true), "Cmd on a Mac").toBe(true);
  expect(modHeld(press(true, false), true), "Control on a Mac").toBe(false);
  expect(modHeld(press(true, true), true), "Control and Cmd on a Mac").toBe(false);
  expect(modHeld(press(true, false), false), "Ctrl elsewhere").toBe(true);
  expect(modHeld(press(false, true), false), "Meta elsewhere").toBe(false);
  expect(modHeld(press(false, false), false), "no modifier").toBe(false);
});

interface Harness {
  advance: (ms: number) => void;
  keymap: Keymap;
  /** Dispatches a keydown on `target` (default: body) and reports whether a binding claimed it. */
  press: (key: string, init?: KeyboardEventInit & { target?: Element }) => boolean;
}

function harness(isMac = false): Harness {
  const timers: { at: number; callback: () => void; handle: number }[] = [];
  let now = 0;
  let nextHandle = 1;
  const keymap = createKeymap({
    clearTimeout: (handle) => {
      const index = timers.findIndex((timer) => timer.handle === handle);
      if (index !== -1) timers.splice(index, 1);
    },
    isMac,
    setTimeout: (callback, ms) => {
      const handle = nextHandle++;
      timers.push({ at: now + ms, callback, handle });
      return handle;
    },
    strict: true,
  });
  return {
    advance: (ms) => {
      now += ms;
      const due = timers.filter((timer) => timer.at <= now);
      for (const timer of due) {
        timers.splice(timers.indexOf(timer), 1);
        timer.callback();
      }
    },
    keymap,
    press: (key, { target = document.body, ...init } = {}) => {
      const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key, ...init });
      target.dispatchEvent(event);
      keymap.handleKeyDown(event);
      return event.defaultPrevented;
    },
  };
}

function binding(id: string, keys: KeyBinding["keys"], extra: Partial<KeyBinding> = {}) {
  let fired = 0;
  const definition: KeyBinding = { id, keys, label: id, run: () => (fired += 1), ...extra };
  return { definition, fired: () => fired };
}

test("? matches the shifted character and $mod matches the platform's command key, never a bare key", () => {
  const { keymap, press } = harness();
  const help = binding("help", "?");
  const search = binding("search", "$mod+k");
  const pin = binding("pin", "Shift+P");
  const priority = binding("priority", "p");
  keymap.register("global", [
    help.definition,
    search.definition,
    pin.definition,
    priority.definition,
  ]);

  expect(press("?", { shiftKey: true })).toBe(true);
  expect(press("/")).toBe(false);
  expect(help.fired()).toBe(1);

  // Away from a Mac `$mod` is Control alone; a row below reads both platforms.
  press("k", { ctrlKey: true });
  expect(press("k")).toBe(false);
  expect(search.fired()).toBe(1);

  press("P", { shiftKey: true });
  press("p");
  expect(pin.fired()).toBe(1);
  expect(priority.fired()).toBe(1);
});

test("$mod+Alt+m preserves AltGr typing away from a Mac and matches Option's physical M key on one", () => {
  const nonMac = harness();
  const nonMacComment = binding("comment", "$mod+Alt+m");
  nonMac.keymap.register("global", [nonMacComment.definition]);

  expect(nonMac.press("m", { altKey: true, code: "KeyM", ctrlKey: true })).toBe(true);
  // German AltGr+M is Ctrl+Alt with this µ key/code pair: it remains character input.
  expect(nonMac.press("µ", { altKey: true, code: "KeyM", ctrlKey: true })).toBe(false);
  // AZERTY puts M where QWERTY has `;`: the character still matches.
  expect(nonMac.press("m", { altKey: true, code: "Semicolon", ctrlKey: true })).toBe(true);
  expect(nonMacComment.fired()).toBe(2);

  const mac = harness(true);
  const macComment = binding("comment", "$mod+Alt+m");
  mac.keymap.register("global", [macComment.definition]);
  // Option turns M into µ on a Mac, so the character alone never names the chord there.
  expect(mac.press("µ", { altKey: true, code: "KeyM", metaKey: true })).toBe(true);
  expect(macComment.fired()).toBe(1);
});

// `$mod` is Cmd on a Mac and Control elsewhere, as tinykeys reads it. A Mac's text field keeps
// Control+K, which deletes to the end of the line, so there `$mod+k` leaves Control+K to the
// field, even for a binding that fires while the reader types.
test("$mod+k fires on Cmd+K and not on Control+K on a Mac, and on Control+K and not on Cmd+K elsewhere", () => {
  const field = document.createElement("input");
  document.body.appendChild(field);
  try {
    const mac = harness(true);
    const macSearch = binding("search", "$mod+k", { inEditable: true });
    mac.keymap.register("global", [macSearch.definition]);
    expect(mac.press("k", { ctrlKey: true, target: field }), "Control+K on a Mac").toBe(false);
    expect(mac.press("k", { metaKey: true, target: field }), "Cmd+K on a Mac").toBe(true);
    expect(macSearch.fired()).toBe(1);

    const other = harness();
    const otherSearch = binding("search", "$mod+k", { inEditable: true });
    other.keymap.register("global", [otherSearch.definition]);
    expect(other.press("k", { metaKey: true, target: field }), "Meta+K elsewhere").toBe(false);
    expect(other.press("k", { ctrlKey: true, target: field }), "Control+K elsewhere").toBe(true);
    expect(otherSearch.fired()).toBe(1);
  } finally {
    field.remove();
  }
});

test("$mod+Alt+m wants both modifiers: Control, Command, Option or Shift alone never runs it", () => {
  const { keymap, press } = harness();
  const comment = binding("comment", "$mod+Alt+m");
  keymap.register("global", [comment.definition]);

  expect(press("m", { code: "KeyM", ctrlKey: true })).toBe(false);
  expect(press("m", { code: "KeyM", metaKey: true })).toBe(false);
  expect(press("µ", { altKey: true, code: "KeyM" })).toBe(false);
  expect(press("M", { code: "KeyM", shiftKey: true })).toBe(false);
  expect(press("m", { code: "KeyM" })).toBe(false);
  expect(press("M", { altKey: true, code: "KeyM", ctrlKey: true, shiftKey: true })).toBe(false);
  // Another key under the same modifiers.
  expect(press("n", { altKey: true, code: "KeyN", ctrlKey: true })).toBe(false);
  expect(comment.fired()).toBe(0);
});

test("a combo without Alt matches by the character alone, so a layout that moves a letter moves its key", () => {
  const { keymap, press } = harness();
  const priority = binding("priority", "p");
  const pin = binding("pin", "Shift+P");
  const search = binding("search", "$mod+k");
  const p0 = binding("p0", "0");
  keymap.register("global", [
    priority.definition,
    pin.definition,
    search.definition,
    p0.definition,
  ]);

  // Dvorak types `l` on the key QWERTY calls P: that press is `l`, not `p`.
  expect(press("l", { code: "KeyP" })).toBe(false);
  expect(press("p", { code: "KeyR" })).toBe(true);
  expect(press("P", { code: "KeyR", shiftKey: true })).toBe(true);
  expect(press("k", { code: "KeyV", ctrlKey: true })).toBe(true);
  expect(press("k", { code: "KeyK", ctrlKey: true })).toBe(true);
  // AZERTY's top row types `à` unshifted where QWERTY has 0.
  expect(press("à", { code: "Digit0" })).toBe(false);
  expect(press("0", { code: "Digit0", shiftKey: true })).toBe(true);
  expect(priority.fired()).toBe(1);
  expect(pin.fired()).toBe(1);
  expect(search.fired()).toBe(2);
  expect(p0.fired()).toBe(1);
});

test("a chord fires within the timeout, shows its pending prefix, and expires after 1000 ms", () => {
  const { advance, keymap, press } = harness();
  const inbox = binding("go-inbox", "g i");
  keymap.register("global", [inbox.definition]);

  expect(press("g")).toBe(true);
  expect(keymap.chord().keys).toBe("g");
  advance(999);
  press("i");
  expect(inbox.fired()).toBe(1);
  expect(keymap.chord().keys).toBe("");

  press("g");
  advance(1000);
  expect(keymap.chord().keys).toBe("");
  expect(press("i")).toBe(false);
  expect(inbox.fired()).toBe(1);
});

test("a key that does not continue the chord is read afresh on its own", () => {
  const { keymap, press } = harness();
  const inbox = binding("go-inbox", "g i");
  const next = binding("next", "j");
  keymap.register("global", [inbox.definition, next.definition]);

  press("g");
  press("j");
  expect(next.fired()).toBe(1);
  expect(keymap.chord().keys).toBe("");
});

test("registering a strict prefix of an existing sequence throws in strict mode", () => {
  const { keymap } = harness();
  keymap.register("global", [binding("go-inbox", "g i").definition]);

  expect(() => keymap.register("inbox", [binding("g", "g").definition])).toThrow(/prefix/);
  expect(() => keymap.register("global", [binding("search", "$mod+k").definition])).not.toThrow();
  expect(() => keymap.register("global", [binding("mod-k-x", "Control+k x").definition])).toThrow(
    /prefix/
  );
});

// Two keys are one press only as the platform reads `$mod`: on a Mac `$mod+k` is Cmd+K, so
// `Meta+k x` begins with it and `Control+k x` does not; elsewhere the other way round.
test("a prefix is read by the platform's $mod: Meta+k x begins $mod+k on a Mac, and Control+k x elsewhere", () => {
  for (const [isMac, begins, other] of [
    [true, "Meta+k x", "Control+k x"],
    [false, "Control+k x", "Meta+k x"],
  ] as const) {
    const { keymap } = harness(isMac);
    keymap.register("global", [binding("search", "$mod+k").definition]);
    expect(
      () => keymap.register("global", [binding("other", other).definition]),
      `${other}, isMac ${isMac}`
    ).not.toThrow();
    expect(
      () => keymap.register("global", [binding("begins", begins).definition]),
      `${begins}, isMac ${isMac}`
    ).toThrow(/prefix/);
  }
});

test("an open dialog masks every scope beneath it and its own bindings still fire", () => {
  const { keymap, press } = harness();
  const inbox = binding("go-inbox", "g i");
  const next = binding("next", "j");
  const close = binding("close", "Escape");
  keymap.register("global", [inbox.definition]);
  const popInbox = keymap.pushScope("inbox");
  keymap.register("inbox", [next.definition]);
  keymap.register(DIALOG_SCOPE, [close.definition]);

  const popDialog = keymap.pushScope(DIALOG_SCOPE);
  expect(press("g")).toBe(false);
  expect(press("j")).toBe(false);
  expect(press("Escape")).toBe(true);
  expect(close.fired()).toBe(1);

  popDialog();
  press("g");
  press("i");
  press("j");
  expect(inbox.fired()).toBe(1);
  expect(next.fired()).toBe(1);
  popInbox();
  expect(press("j")).toBe(false);
});

test("a page scope mounted while a dialog is open still cannot fire beneath it", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  const close = binding("close", "Escape");
  keymap.register(DIALOG_SCOPE, [close.definition]);
  keymap.register("inbox", [next.definition]);

  // `?` on /agents, then browser Back to `/`: the Inbox pushes its scope above the open dialog.
  const popDialog = keymap.pushScope(DIALOG_SCOPE);
  keymap.pushScope("inbox");
  expect(press("j")).toBe(false);
  expect(next.fired()).toBe(0);
  expect(press("Escape")).toBe(true);
  expect(close.fired()).toBe(1);

  popDialog();
  expect(press("j")).toBe(true);
  expect(next.fired()).toBe(1);
});

test("the innermost scope wins, and a binding whose when() is false lets lower scopes fire", () => {
  const { keymap, press } = harness();
  const globalOpen = binding("open-global", "o");
  const inboxOpen = binding("open-inbox", "o", { when: () => false });
  keymap.register("global", [globalOpen.definition]);
  keymap.pushScope("inbox");
  keymap.register("inbox", [inboxOpen.definition]);

  press("o");
  expect(inboxOpen.fired()).toBe(0);
  expect(globalOpen.fired()).toBe(1);
  expect(keymap.describe().map((entry) => [entry.id, entry.enabled])).toEqual([
    ["open-global", true],
    ["open-inbox", false],
  ]);
});

test("only inEditable bindings fire while an input, textarea, select, or contentEditable has focus", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  const search = binding("search", "$mod+k", { inEditable: true });
  keymap.register("global", [next.definition, search.definition]);

  const editables = ["input", "textarea", "select"].map((tag) => document.createElement(tag));
  const editor = document.createElement("div");
  editor.contentEditable = "true";
  editables.push(editor);
  const button = document.createElement("button");
  for (const element of [...editables, button]) {
    document.body.appendChild(element);
  }
  try {
    for (const target of editables) {
      expect(press("j", { target })).toBe(false);
      expect(press("k", { ctrlKey: true, target })).toBe(true);
    }
    expect(next.fired()).toBe(0);
    expect(search.fired()).toBe(editables.length);
    expect(press("j", { target: button })).toBe(true);
    expect(next.fired()).toBe(1);
  } finally {
    for (const element of [...editables, button]) {
      element.remove();
    }
  }
});

test("a checkbox, radio or button takes no text, so single keys still fire while one has focus", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  keymap.register("global", [next.definition]);

  const typed = (type: string) => {
    const input = document.createElement("input");
    input.type = type;
    return input;
  };
  const shortcutTargets = [typed("checkbox"), typed("radio"), typed("button")];
  const typing = [typed("text"), typed("search"), typed("unknown-becomes-text")];
  for (const element of [...shortcutTargets, ...typing]) {
    document.body.appendChild(element);
  }
  try {
    for (const target of shortcutTargets) {
      expect(press("j", { target })).toBe(true);
    }
    expect(next.fired()).toBe(shortcutTargets.length);
    for (const target of typing) {
      expect(press("j", { target })).toBe(false);
    }
    expect(next.fired()).toBe(shortcutTargets.length);
  } finally {
    for (const element of [...shortcutTargets, ...typing]) {
      element.remove();
    }
  }
});

/** An open panel as `AnchoredPanel` draws one: an element carrying the keymap's mark, holding the
 *  controls a panel holds - the display menu's radio, a filter panel's checkbox and buttons - one
 *  of them inside a group. The panel itself takes the focus where it holds no control. */
function openPanel(): { controls: HTMLElement[]; panel: HTMLElement; radio: HTMLInputElement } {
  const panel = document.createElement("div");
  panel.setAttribute(PANEL_ATTRIBUTE, "");
  panel.tabIndex = -1;
  const typed = (type: string) => {
    const input = document.createElement("input");
    input.type = type;
    return input;
  };
  const radio = typed("radio");
  const checkbox = typed("checkbox");
  const button = typed("button");
  const group = document.createElement("fieldset");
  const grouped = document.createElement("button");
  group.appendChild(grouped);
  panel.append(radio, checkbox, button, group);
  document.body.appendChild(panel);
  return { controls: [radio, checkbox, button, grouped], panel, radio };
}

// INFRA-3135's review round 1, follow-up F5: the display menu and the filter panels replaced
// selects, where a page's single keys did nothing, with radios and checkboxes, which take no
// text; with a panel open, `j` on its radio walked the table behind it.
test("a key pressed anywhere inside an open panel fires only inEditable bindings, whatever control of it holds the focus", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  const search = binding("search", "$mod+k", { inEditable: true });
  keymap.register("global", [next.definition, search.definition]);
  const { controls, panel } = openPanel();
  try {
    for (const target of [panel, ...controls]) {
      expect(press("j", { target }), `j on ${target.outerHTML}`).toBe(false);
      expect(press("k", { ctrlKey: true, target }), `$mod+k on ${target.outerHTML}`).toBe(true);
    }
    expect(next.fired()).toBe(0);
    expect(search.fired()).toBe(controls.length + 1);
  } finally {
    panel.remove();
  }
});

test("only a key pressed inside a panel is masked: a panel standing open with the focus on the page leaves the page's keys alone", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  keymap.register("global", [next.definition]);
  const { panel, radio } = openPanel();
  const onPage = document.createElement("input");
  onPage.type = "radio";
  document.body.appendChild(onPage);
  try {
    expect(press("j"), "j on the body").toBe(true);
    expect(press("j", { target: onPage }), "j on a radio of the page").toBe(true);
    expect(next.fired()).toBe(2);
    expect(press("j", { target: radio }), "j on the panel's radio").toBe(false);
    expect(next.fired()).toBe(2);
  } finally {
    panel.remove();
    onPage.remove();
  }
});

// INFRA-3135's review round 2: an option list's trigger replaced a `<select>` or a text box,
// where a page's single keys did nothing; a plain button, it let them through, so a digit on the
// issue header's Assignee wrote the priority. Closed or open, the trigger takes a select's keys.
test("a key pressed on an option list's trigger fires only inEditable bindings, open or closed", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  const digit = binding("digit", "2");
  const search = binding("search", "$mod+k", { inEditable: true });
  keymap.register("global", [next.definition, digit.definition, search.definition]);
  const trigger = document.createElement("button");
  trigger.type = "button";
  trigger.setAttribute("aria-haspopup", "listbox");
  const panelTrigger = document.createElement("button");
  panelTrigger.type = "button";
  panelTrigger.setAttribute("aria-haspopup", "dialog");
  document.body.append(trigger, panelTrigger);
  try {
    for (const expanded of ["false", "true"]) {
      trigger.setAttribute("aria-expanded", expanded);
      expect(press("j", { target: trigger }), `j, expanded ${expanded}`).toBe(false);
      expect(press("2", { target: trigger }), `2, expanded ${expanded}`).toBe(false);
      expect(press("k", { ctrlKey: true, target: trigger }), "$mod+k").toBe(true);
    }
    expect(next.fired()).toBe(0);
    expect(digit.fired()).toBe(0);
    expect(search.fired()).toBe(2);
    // A trigger that opens a dialog - a filter panel's, the display menu's - is a plain button.
    expect(press("j", { target: panelTrigger }), "j on a panel's trigger").toBe(true);
    expect(next.fired()).toBe(1);
  } finally {
    trigger.remove();
    panelTrigger.remove();
  }
});

// dispatch://INFRA-2160, R4 and R5 (the tester's round 16): a menu's trigger - the Snooze, the
// priority, the status (`MultiSelect`'s `menu`, marked `MENU_TRIGGER_ATTRIBUTE`) - is a menu's
// button, not a select. A character typed on it while its menu is closed is the page's key, as
// on the row: `b` there opens the Snooze menu, `p` the priority menu, and a digit is the page's
// digit (`typedOnMenuTrigger`). Enter, Space and the arrows stay the trigger's, which open its
// menu, and an open menu's trigger and every other list's trigger keep every key.
test("a character typed on a menu's closed trigger is the page's key, while its Enter, Space and arrows, an open menu's trigger and another list's trigger keep their own", () => {
  const { keymap, press } = harness();
  const snooze = binding("snooze", "b");
  const digit = binding("digit", "2");
  const answer = binding("answer", "Enter");
  const scroll = binding("scroll", "Space");
  const next = binding("next", ["ArrowDown", "ArrowUp"]);
  keymap.register("global", [
    snooze.definition,
    digit.definition,
    answer.definition,
    scroll.definition,
    next.definition,
  ]);
  const trigger = (menu: boolean) => {
    const button = document.createElement("button");
    button.type = "button";
    button.setAttribute("aria-haspopup", "listbox");
    button.setAttribute("aria-expanded", "false");
    if (menu) button.setAttribute(MENU_TRIGGER_ATTRIBUTE, "");
    return button;
  };
  const menu = trigger(true);
  const list = trigger(false);
  document.body.append(menu, list);
  try {
    expect(press("b", { target: menu }), "b on the menu's trigger").toBe(true);
    expect(press("2", { target: menu }), "2 on the menu's trigger").toBe(true);
    expect([snooze.fired(), digit.fired()]).toEqual([1, 1]);
    for (const key of ["Enter", " ", "ArrowDown", "ArrowUp"]) {
      expect(press(key, { target: menu }), `${key} on the menu's trigger`).toBe(false);
      expect(press(key, { altKey: true, target: menu }), `Alt+${key}`).toBe(false);
    }
    expect([answer.fired(), scroll.fired(), next.fired()]).toEqual([0, 0, 0]);
    menu.setAttribute("aria-expanded", "true");
    expect(press("b", { target: menu }), "b on an open menu's trigger").toBe(false);
    expect(press("b", { target: list }), "b on another list's trigger").toBe(false);
    expect(press("2", { target: list }), "2 on another list's trigger").toBe(false);
    expect([snooze.fired(), digit.fired()]).toEqual([1, 1]);
  } finally {
    menu.remove();
    list.remove();
  }
});

test("an already-handled key (defaultPrevented) or one mid-IME-composition is left alone", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  keymap.register("global", [next.definition]);
  const swallow = (event: Event) => event.preventDefault();
  document.body.addEventListener("keydown", swallow);
  try {
    press("j");
    expect(next.fired()).toBe(0);
  } finally {
    document.body.removeEventListener("keydown", swallow);
  }
  expect(press("j", { isComposing: true })).toBe(false);
  expect(next.fired()).toBe(0);
  press("j");
  expect(next.fired()).toBe(1);
});

// Safari sends the key that ends an input method's composition after the composition's end,
// unmarked - `isComposing` false - with keyCode 229 (WebKit bug 165004): that key is the input
// method's as well. An Escape so sent in a field runs no binding, one that fires in a field (a
// dialog's close) included, and neither does a letter; the plain keys after them do.
test("a key an input method sends at keyCode 229, as Safari sends the one that ends a composition, is left alone", () => {
  const { keymap, press } = harness();
  const close = binding("close", "Escape", { inEditable: true });
  const next = binding("next", "j");
  keymap.register("global", [close.definition, next.definition]);
  const field = document.createElement("input");
  document.body.append(field);
  try {
    expect(press("Escape", { keyCode: 229, target: field }), "Escape at keyCode 229").toBe(false);
    expect(close.fired(), "Escape at keyCode 229").toBe(0);
    expect(press("j", { keyCode: 229 }), "j at keyCode 229").toBe(false);
    expect(next.fired(), "j at keyCode 229").toBe(0);

    press("Escape", { target: field });
    expect(close.fired()).toBe(1);
    press("j");
    expect(next.fired()).toBe(1);
  } finally {
    field.remove();
  }
});

test("project and board are page scopes: their bindings are described under their own scope and the innermost wins", () => {
  const { keymap, press } = harness();
  const toggleView = binding("toggle-view", "v");
  const nextCard = binding("next", "j");
  keymap.register("project", [toggleView.definition]);
  keymap.register("board", [nextCard.definition]);
  keymap.pushScope("project");
  keymap.pushScope("board");

  expect(keymap.describe().map((entry) => [entry.scope, entry.id])).toEqual([
    ["project", "toggle-view"],
    ["board", "next"],
  ]);
  expect(press("v")).toBe(true);
  expect(toggleView.fired()).toBe(1);
  expect(press("j")).toBe(true);
  expect(nextCard.fired()).toBe(1);
});

test("a binding two mounted instances register is described once, enabled when either applies", () => {
  const { keymap, press } = harness();
  let focused: "spec" | "runbook" | undefined;
  const spec = binding("comment-on-selection", "$mod+Alt+m", {
    inEditable: true,
    when: () => focused === "spec",
  });
  const runbook = binding("comment-on-selection", "$mod+Alt+m", {
    inEditable: true,
    when: () => focused === "runbook",
  });
  keymap.register("document", [spec.definition]);
  keymap.register("document", [runbook.definition]);
  keymap.pushScope("document");
  keymap.pushScope("document");

  const described = () =>
    keymap.describe().map((entry) => [entry.scope, entry.id, entry.enabled] as const);
  expect(described()).toEqual([["document", "comment-on-selection", false]]);
  focused = "runbook";
  expect(described()).toEqual([["document", "comment-on-selection", true]]);
  expect(press("m", { altKey: true, code: "KeyM", ctrlKey: true })).toBe(true);
  expect([spec.fired(), runbook.fired()]).toEqual([0, 1]);
});

// dispatch://INFRA-2160, R13: `?` over a dialog says what a key does there. While a dialog is open
// the registry reads its scope alone, so over the Inbox drawer the shell's `c` and the page's own
// keys are disabled. An action that the page beneath and the drawer both register is one row,
// under the action's own scope, enabled since the drawer's registration applies. A `beforeScopes`
// binding is read before every scope, so its `when` alone decides.
test("with a dialog open, describe() disables the shell's and the page's bindings and enables the dialog's, and an action the page and the dialog both register is one enabled row under its own scope", () => {
  const { keymap } = harness();
  keymap.register("global", [
    actionBinding({ action: "global.create", run: () => undefined }, NO_PERSON_KEYS),
    binding("copy-link", "y", { beforeScopes: true }).definition,
  ]);
  // The Inbox page, beneath its drawer.
  keymap.register("inbox", [
    actionBinding({ action: "inbox.next", run: () => undefined }, NO_PERSON_KEYS),
    actionBinding({ action: "inbox.snooze", run: () => undefined }, NO_PERSON_KEYS),
  ]);
  keymap.pushScope("inbox");
  // The drawer: the Inbox's `j` again, and its own Escape.
  keymap.register(DIALOG_SCOPE, [
    actionBinding({ action: "inbox.next", run: () => undefined }, NO_PERSON_KEYS),
    binding("close", "Escape", { inEditable: true }).definition,
  ]);
  keymap.pushScope(DIALOG_SCOPE);

  expect(keymap.describe().map((entry) => [entry.scope, entry.id, entry.enabled])).toEqual([
    ["global", "create", false],
    ["global", "copy-link", true],
    ["inbox", "next", true],
    ["inbox", "snooze", false],
    ["dialog", "close", true],
  ]);
});

test("actions() lists each scope below the innermost dialog once, innermost scope first", () => {
  const { keymap } = harness();
  const create = binding("create", "c");
  const toggleView = binding("toggle-view", "v");
  const closePalette = binding("close-palette", "Escape", { inEditable: true });
  keymap.register("global", [create.definition]);
  keymap.register("project", [toggleView.definition]);
  keymap.register(DIALOG_SCOPE, [closePalette.definition]);
  // A page can push its scope twice (a panel kept mounted while hidden); one row all the same.
  keymap.pushScope("project");
  keymap.pushScope("project");
  keymap.pushScope(DIALOG_SCOPE);

  expect(keymap.actions().map((action) => [action.scope, action.id])).toEqual([
    ["project", "toggle-view"],
    ["global", "create"],
  ]);
});

test("actions() orders a scope's rows by label, never by when its component registered them", () => {
  const { keymap } = harness();
  // Registration order is mount timing: the same page reached by a navigation and by a reload
  // registers its scopes in a different order, and the highlighted first row must not follow it.
  keymap.register("issue", [binding("set-p1", [], { label: "Set priority P1" }).definition]);
  keymap.register("global", [binding("go-inbox", "g i", { label: "Go to Inbox" }).definition]);
  keymap.register("issue", [binding("close", [], { label: "Close issue" }).definition]);
  keymap.register("global", [binding("create", "c", { label: "Create issue" }).definition]);
  keymap.pushScope("issue");

  expect(keymap.actions().map((action) => action.label)).toEqual([
    "Close issue",
    "Set priority P1",
    "Create issue",
    "Go to Inbox",
  ]);
});

test("actions() omits palette:false, multi-key, inEditable and unavailable bindings", () => {
  const { keymap } = harness();
  const next = binding("next", "j", { palette: false });
  const arrows = binding("arrows", ["ArrowDown", "ArrowUp"]);
  const search = binding("search", "$mod+k", { inEditable: true });
  const move = binding("move", "Shift+J", { when: () => false });
  const open = binding("open", "o");
  keymap.register("board", [
    next.definition,
    arrows.definition,
    search.definition,
    move.definition,
    open.definition,
  ]);
  keymap.pushScope("board");

  expect(keymap.actions().map((action) => action.id)).toEqual(["open"]);
});

test("an inEditable binding is no row unless it says palette: true, which makes it one", () => {
  const { keymap } = harness();
  const back = binding("back", "Escape", { inEditable: true });
  const send = binding("send", "$mod+Enter", { inEditable: true, palette: true });
  keymap.register("board", [back.definition, send.definition]);
  keymap.pushScope("board");

  expect(keymap.actions().map((action) => action.id)).toEqual(["send"]);
});

test("a binding with several keys is a row only when it says so, and the row presses its first key", () => {
  const { keymap } = harness();
  let pressed = "";
  const ascend = binding("ascend", ["h", "ArrowLeft"], {
    label: "Up one level",
    palette: true,
    run: (event) => {
      pressed = event.key;
    },
  });
  const arrows = binding("arrows", ["ArrowDown", "ArrowUp"]);
  keymap.register("architecture", [ascend.definition, arrows.definition]);
  keymap.pushScope("architecture");

  const rows = keymap.actions();
  expect(rows.map((action) => action.id)).toEqual(["ascend"]);
  rows[0]?.run();
  expect(pressed).toBe("h");
});

test("a keyless binding never fires on a key press yet is offered as an action", () => {
  const { keymap, press } = harness();
  const close = binding("close", [], { label: "Close issue" });
  keymap.register("issue", [close.definition]);
  keymap.pushScope("issue");

  expect(press("c")).toBe(false);
  expect(press("Enter")).toBe(false);
  expect(close.fired()).toBe(0);
  expect(keymap.describe().map((entry) => [entry.id, entry.keys])).toEqual([["close", []]]);
  expect(keymap.actions()).toMatchObject([
    { id: "close", keys: [], label: "Close issue", scope: "issue" },
  ]);
});

test("running an action fires its binding once with a keydown carrying its first key", () => {
  const { keymap } = harness();
  const fired: string[] = [];
  keymap.register("global", [
    { id: "snooze", keys: "s", label: "Snooze", run: (event) => fired.push(`snooze:${event.key}`) },
    {
      id: "close",
      keys: [],
      label: "Close issue",
      run: (event) => fired.push(`close:${event.key}`),
    },
  ]);

  for (const action of keymap.actions()) {
    action.run();
  }

  // Sorted, so the assertion is about which key each binding received and that each ran once,
  // not about the row order `actions()` chose (pinned by its own test above).
  expect([...fired].sort()).toEqual(["close:", "snooze:s"]);
});

test("a row runs its binding only if the binding still applies when the row is chosen", () => {
  const { keymap } = harness();
  let closed = true;
  const reopen = binding("reopen", [], { label: "Reopen issue", when: () => closed });
  keymap.register("issue", [reopen.definition]);
  keymap.pushScope("issue");
  const [row] = keymap.actions();

  // The issue was reopened elsewhere while the palette was open: the row's control has gone.
  closed = false;
  row?.run();
  expect(reopen.fired()).toBe(0);

  closed = true;
  row?.run();
  expect(reopen.fired()).toBe(1);
});

test("a row does nothing once its binding's component has unregistered it", () => {
  const { keymap } = harness();
  const close = binding("close", [], { label: "Close issue" });
  const unregister = keymap.register("issue", [close.definition]);
  keymap.pushScope("issue");
  const [row] = keymap.actions();

  // The issue header unmounted while the palette was open (the page fell to its error view).
  unregister();
  row?.run();
  expect(close.fired()).toBe(0);
});

// A held key's repeats run only a binding that steps (`KeyBinding.repeats`, which an action takes
// from `CatalogueEntry.repeats`): a held `j` walks a list, a held Space pages the open document
// and a held Shift+J moves a Board card on. Every other binding runs once per press, and the
// repeats of its key reach no page: a write, whose repeat would write again or, once the write
// hands the reader on (R8), write on an item nobody chose; a digit, whose repeats would tick an
// option of the question handed on to; a menu's opener, which would open it again; a binding
// written out whole; and a chord, which a held key's repeat would complete. A palette row runs
// each as a press does.
test("a held key's repeats run only a binding that steps, never a write, a digit, a menu's opener, a binding written out whole or a chord, and a row runs each", () => {
  const { advance, keymap, press } = harness();
  const ran = new Map<string, number>();
  const action = (name: KeymapActionId) =>
    actionBinding(
      { action: name, run: () => ran.set(name, (ran.get(name) ?? 0) + 1) },
      NO_PERSON_KEYS
    );
  const whole = binding("whole", "w");
  keymap.register("global", [whole.definition]);
  keymap.register("inbox", [
    action("inbox.dismiss"),
    action("inbox.option"),
    action("inbox.snooze"),
    action("inbox.next"),
    action("inbox.scroll-down"),
    action("inbox.scroll-top"),
  ]);
  keymap.register("board", [action("board.move-down")]);
  keymap.pushScope("inbox");
  keymap.pushScope("board");

  for (const [key, init] of [
    ["e", {}],
    ["1", {}],
    ["b", {}],
    ["w", {}],
    ["j", {}],
    [" ", {}],
    ["J", { shiftKey: true }],
  ] as const) {
    expect(press(key, init), `${key}, pressed`).toBe(true);
    expect(press(key, { ...init, repeat: true }), `${key}, its first repeat`).toBe(true);
    expect(press(key, { ...init, repeat: true }), `${key}, its second repeat`).toBe(true);
  }
  expect(Object.fromEntries(ran)).toEqual({
    "board.move-down": 3,
    "inbox.dismiss": 1,
    "inbox.next": 3,
    "inbox.option": 1,
    "inbox.scroll-down": 3,
    "inbox.snooze": 1,
  });
  expect(whole.fired(), "the binding written out whole").toBe(1);

  // A held `g` is one press of it: its repeats complete no `g g`, which two presses do.
  press("g");
  press("g", { repeat: true });
  press("g", { repeat: true });
  expect(ran.get("inbox.scroll-top"), "a held g").toBeUndefined();
  advance(CHORD_TIMEOUT_MS);
  press("g");
  press("g");
  expect(ran.get("inbox.scroll-top"), "g g").toBe(1);

  for (const row of keymap.actions()) {
    if (row.id === "dismiss" || row.id === "snooze") row.run();
  }
  expect([ran.get("inbox.dismiss"), ran.get("inbox.snooze")]).toEqual([2, 2]);
});

/**
 * A hold of `key` as an engine sends one, on the page `KeymapProvider` binds: its press on the
 * element that has the focus, `repeats` repeats, each on the element that has the focus as it is
 * sent, and its release. Answers how many of the repeats reached the document's capture-phase
 * listeners, where a panel's Escape listens (`AnchoredPanel`), and how many were left to the
 * browser to act on: a character typed, a button pressed, a page scrolled.
 */
function hold(
  key: string,
  repeats: number,
  init: KeyboardEventInit = {}
): { acted: number; reached: number } {
  const send = (type: "keydown" | "keyup", repeat: boolean) => {
    const event = new KeyboardEvent(type, {
      bubbles: true,
      cancelable: true,
      key,
      repeat,
      ...init,
    });
    act(() => {
      (document.activeElement ?? document.body).dispatchEvent(event);
    });
    return event;
  };
  send("keydown", false);
  let reached = 0;
  const hear = () => {
    reached += 1;
  };
  document.addEventListener("keydown", hear, true);
  let acted = 0;
  try {
    for (let count = 0; count < repeats; count += 1) {
      if (!send("keydown", true).defaultPrevented) acted += 1;
    }
  } finally {
    document.removeEventListener("keydown", hear, true);
    send("keyup", false);
  }
  return { acted, reached };
}

/** Draws `KeymapProvider`, registers `bindings` under `inbox` and puts `elements` on the page;
 *  answers what takes each of them away again. */
function heldPage(bindings: readonly KeyBinding[], elements: readonly Element[]): () => void {
  const view = render(createElement(KeymapProvider, {}));
  const unregister = appKeymap.register("inbox", bindings);
  const pop = appKeymap.pushScope("inbox");
  document.body.append(...elements);
  return () => {
    for (const element of elements) element.remove();
    pop();
    unregister();
    view.unmount();
  };
}

// The held-key rule the architect ruled on 2026-11-25T04:12Z (62b47002's "a held key acts once but
// for the steps and scrolls that repeat"), which the window's capture phase applies before any
// control hears a key (`Keymap.captureKeyDown`): a repeat of a key that acts - one whose press put
// the caret in a field, an Enter on a button, an Escape a panel took - reaches nothing of the page
// and is left to no action of the browser's. It types nothing in the field the press focused,
// presses the button no second time, and closes no panel around a list an Escape closed. The
// ruling of 2026-11-25T09:24Z keeps a repeat only where it would act: an Escape on a panel's field
// that nothing took acts on nothing, and its repeats go on.
test("a held key that acts reaches nothing with its repeats: none types in the field its press focused, presses the focused button again, or closes the panel around it", () => {
  const row = document.createElement("button");
  const field = document.createElement("input");
  const panel = document.createElement("div");
  panel.setAttribute(PANEL_ATTRIBUTE, "");
  const trigger = document.createElement("button");
  panel.append(trigger);
  let searched = 0;
  const search: KeyBinding = {
    id: "search",
    keys: "f",
    label: "Search",
    run: () => {
      searched += 1;
      field.focus();
    },
  };
  const release = heldPage([search], [row, field, panel]);
  try {
    act(() => row.focus());
    expect(hold("f", 3), "f, whose press puts the caret in a field").toEqual({
      acted: 0,
      reached: 0,
    });
    expect(document.activeElement, "the press put the caret in the field").toBe(field);
    expect(searched, "the press ran its binding once").toBe(1);
    act(() => row.focus());
    expect(hold("Enter", 3), "Enter on a focused button").toEqual({ acted: 0, reached: 0 });
    act(() => trigger.focus());
    expect(hold("Escape", 3), "Escape on a panel's field").toEqual({ acted: 3, reached: 3 });
    // The panel's own Escape (`AnchoredPanel`'s `dismissOnEscape`) takes the press.
    const dismiss = (event: KeyboardEvent) => {
      if (event.key === "Escape") event.preventDefault();
    };
    document.addEventListener("keydown", dismiss, true);
    try {
      expect(hold("Escape", 3), "Escape the panel took").toEqual({ acted: 0, reached: 0 });
    } finally {
      document.removeEventListener("keydown", dismiss, true);
    }
  } finally {
    release();
  }
});

// The same rule's steps and scrolls: a binding that steps (`KeyBinding.repeats`) runs on each
// repeat wherever its press moved the focus, an arrow goes on to whatever has the focus, and Space
// goes on where its press was left to the browser, which scrolls the page by it, but not where a
// control took the press to act, as a card that Space presses does.
test("a held key that moves repeats wherever the focus has gone: a binding that steps runs on each repeat, and an arrow or Space the browser scrolls by goes on", () => {
  const rows = Array.from({ length: 5 }, () => document.createElement("button"));
  const card = document.createElement("div");
  card.tabIndex = 0;
  let pressed = 0;
  card.addEventListener("keydown", (event) => {
    if (event.key !== " ") return;
    event.preventDefault();
    pressed += 1;
  });
  let steps = 0;
  const next: KeyBinding = {
    id: "next",
    keys: "j",
    label: "Next",
    repeats: true,
    run: () => {
      steps += 1;
      rows[steps]?.focus();
    },
  };
  const release = heldPage([next], [...rows, card]);
  try {
    act(() => rows[0]?.focus());
    expect(hold("j", 3), "j, which the binding takes on each repeat").toEqual({
      acted: 0,
      reached: 3,
    });
    expect(steps, "a held j steps once for its press and once for each repeat").toBe(4);
    expect(document.activeElement, "the held j walked to the fifth row").toBe(rows[4]);
    expect(hold("ArrowDown", 3), "ArrowDown").toEqual({ acted: 3, reached: 3 });
    expect(hold(" ", 3), "Space, which the browser scrolls by").toEqual({ acted: 3, reached: 3 });
    act(() => card.focus());
    expect(hold(" ", 3), "Space, which presses the card").toEqual({ acted: 0, reached: 0 });
    expect(pressed, "the held Space pressed the card once").toBe(1);
  } finally {
    release();
  }
});

// The same rule's typing: in the field that had the focus before the press, a held key that types
// repeats - a character, Space, Backspace and Delete, and Enter in a field of lines - and so does a
// key that nothing took and that acts on nothing there, Escape and Ctrl+Z among them (the ruling of
// 2026-11-25T09:24Z, which ended the rule's stop of both), but Tab, Enter in a field of one line,
// which sends its form, and a character a control took (a menu's digit, which writes its choice)
// act once.
test("in a field that had the focus before the press, a held key that writes repeats, and so do Escape and a Ctrl key that nothing took, and an Enter that sends and a character a control took act once", () => {
  const input = document.createElement("input");
  const lines = document.createElement("textarea");
  const menuBox = document.createElement("input");
  menuBox.addEventListener("keydown", (event) => {
    if (event.key === "2") event.preventDefault();
  });
  const release = heldPage([], [input, lines, menuBox]);
  try {
    act(() => input.focus());
    for (const key of ["w", " ", "Backspace", "Delete"]) {
      expect(hold(key, 3), key).toEqual({ acted: 3, reached: 3 });
    }
    for (const [key, init, acted] of [
      ["Escape", {}, 3],
      ["Tab", {}, 0],
      ["Enter", {}, 0],
      ["z", { ctrlKey: true }, 3],
    ] as const) {
      expect(hold(key, 3, init), key).toEqual({ acted, reached: acted });
    }
    act(() => lines.focus());
    expect(hold("Enter", 3), "Enter in a field of lines").toEqual({ acted: 3, reached: 3 });
    const submit = (event: KeyboardEvent) => event.preventDefault();
    lines.addEventListener("keydown", submit, { once: true });
    expect(hold("Enter", 3), "Enter taken by a form").toEqual({ acted: 0, reached: 0 });
    act(() => menuBox.focus());
    expect(hold("2", 3), "a digit the menu's box took").toEqual({ acted: 0, reached: 0 });
    expect(hold("w", 3), "a letter the menu's box typed").toEqual({ acted: 3, reached: 3 });
  } finally {
    release();
  }
});

// The architect's ruling of 2026-11-25T09:24Z (the tester's rows at a20666a8): the guard stops only
// the repeats of a key that would act, and reads what would act, not the modifier bits. A key that
// no binding names, whose press nothing took, reaches the browser on each repeat: the browser's own
// zoom on the page, a character typed with AltGr, which Windows reports as Ctrl+Alt, in the field
// the press was made in, and a Mac text field's own Control+H, which deletes back.
test("a held Ctrl+-, which no binding names, leaves each repeat on the page to the browser's zoom", () => {
  const release = heldPage([], []);
  try {
    act(() => {
      if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    });
    expect(hold("-", 3, { code: "Minus", ctrlKey: true }), "Ctrl+- on the page").toEqual({
      acted: 3,
      reached: 3,
    });
  } finally {
    release();
  }
});

test("in a text box the press was made in, a held character typed with AltGr, which Windows reports as Ctrl+Alt, repeats as a plain character does", () => {
  const input = document.createElement("input");
  const release = heldPage([], [input]);
  try {
    act(() => input.focus());
    expect(
      hold("@", 3, { altKey: true, code: "KeyQ", ctrlKey: true }),
      "AltGr+Q, which types @"
    ).toEqual({ acted: 3, reached: 3 });
  } finally {
    release();
  }
});

test("in a text box the press was made in, a held Control+H, a Mac text field's own delete, repeats", () => {
  const input = document.createElement("input");
  const release = heldPage([], [input]);
  try {
    act(() => input.focus());
    expect(hold("h", 3, { code: "KeyH", ctrlKey: true }), "Control+H").toEqual({
      acted: 3,
      reached: 3,
    });
  } finally {
    release();
  }
});

// A hold ends as its key is let go, and as the window loses the focus, which takes the release
// elsewhere: a repeat whose press the page no longer holds is read as one whose press was on
// another element, and types nothing. An input method's key goes on untouched, a repeat or not,
// and a fresh press of the key is a hold again.
test("a repeat after its key's release or the window's loss of the focus types nothing, while an input method's key and a fresh press go on", () => {
  const input = document.createElement("input");
  const release = heldPage([], [input]);
  /** Sends a keydown or a keyup of `w` to the field; answers whether the page kept it from the
   *  browser. */
  const send = (type: "keydown" | "keyup", init: KeyboardEventInit = {}) => {
    const event = new KeyboardEvent(type, { bubbles: true, cancelable: true, key: "w", ...init });
    act(() => {
      input.dispatchEvent(event);
    });
    return event.defaultPrevented;
  };
  try {
    act(() => input.focus());
    expect(send("keydown"), "the press").toBe(false);
    expect(send("keydown", { repeat: true }), "a repeat of the press").toBe(false);
    send("keyup");
    expect(send("keydown", { repeat: true }), "a repeat after the release").toBe(true);
    expect(send("keydown"), "a fresh press").toBe(false);
    act(() => {
      window.dispatchEvent(new Event("blur"));
    });
    expect(send("keydown", { repeat: true }), "a repeat after the window's blur").toBe(true);
    expect(send("keydown", { isComposing: true, repeat: true }), "an IME's key").toBe(false);
    expect(send("keydown"), "a fresh press after the window came back").toBe(false);
    expect(send("keydown", { repeat: true }), "a repeat of the fresh press").toBe(false);
  } finally {
    send("keyup");
    release();
  }
});

test("rows within a scope read alphabetically, a lowercase word among capitalised ones included", () => {
  const { keymap } = harness();
  keymap.register("global", [
    binding("go-settings", "g s", { label: "Go to Settings" }).definition,
    binding("go-project", "g p", { label: "Go to project…" }).definition,
    binding("go-inbox", "g i", { label: "Go to Inbox" }).definition,
  ]);

  expect(keymap.actions().map((action) => action.label)).toEqual([
    "Go to Inbox",
    "Go to project…",
    "Go to Settings",
  ]);
});

test("Space and Shift+Space are two keys, and the space bar presses Space", () => {
  const { keymap, press } = harness();
  const down = binding("scroll-down", "Space");
  const up = binding("scroll-up", "Shift+Space");
  keymap.register("inbox", [down.definition, up.definition]);
  keymap.pushScope("inbox");

  expect(press(" ")).toBe(true);
  expect([down.fired(), up.fired()]).toEqual([1, 0]);
  expect(press(" ", { shiftKey: true })).toBe(true);
  expect([down.fired(), up.fired()]).toEqual([1, 1]);
  // The space character itself names no key.
  expect(() => keymap.register("inbox", [binding("space", " ").definition])).toThrow(/no key/);
});

test("a beforeScopes binding wins over the innermost scope and an open dialog, even in a field, until its when is false", () => {
  const { keymap, press } = harness();
  let onLink = true;
  const copyLink = binding("copy-link", "y", { beforeScopes: true, when: () => onLink });
  const copyRow = binding("copy-ref", "y");
  const closeDialog = binding("close", "y");
  keymap.register("global", [copyLink.definition]);
  keymap.pushScope("inbox");
  keymap.register("inbox", [copyRow.definition]);
  keymap.register(DIALOG_SCOPE, [closeDialog.definition]);

  expect(press("y")).toBe(true);
  expect([copyLink.fired(), copyRow.fired()]).toEqual([1, 0]);
  // Its `when` is its only guard: a field's focus does not hold it back.
  expect(press("y", { target: document.createElement("input") })).toBe(true);
  expect(copyLink.fired()).toBe(2);

  const popDialog = keymap.pushScope(DIALOG_SCOPE);
  expect(press("y")).toBe(true);
  expect([copyLink.fired(), closeDialog.fired()]).toEqual([3, 0]);
  popDialog();

  onLink = false;
  expect(press("y")).toBe(true);
  expect([copyLink.fired(), copyRow.fired()]).toEqual([3, 1]);
});

test("a pending chord lists each key that can follow with its label, innermost scope first and once each", () => {
  const { advance, keymap, press } = harness();
  keymap.register("global", [
    binding("go-inbox", "g i", { label: "Go to Inbox" }).definition,
    binding("go-settings", "g s", { label: "Go to Settings" }).definition,
    binding("go-documents", "g d", { label: "Go to Documents", when: () => false }).definition,
  ]);
  keymap.pushScope("inbox");
  keymap.register("inbox", [
    binding("scroll-top", "g g", { label: "Scroll to the top" }).definition,
    binding("inbox-settings", "g s", { label: "Inbox settings" }).definition,
  ]);

  expect(keymap.chord()).toEqual({ keys: "", next: [] });
  press("g");
  expect(keymap.chord()).toEqual({
    keys: "g",
    next: [
      { key: "g", label: "Scroll to the top" },
      { key: "s", label: "Inbox settings" },
      { key: "i", label: "Go to Inbox" },
    ],
  });
  // One snapshot per chord, as `useSyncExternalStore` needs.
  expect(keymap.chord()).toBe(keymap.chord());
  advance(1000);
  expect(keymap.chord()).toEqual({ keys: "", next: [] });

  // While a dialog is open its scope is the only one a key is read through, and the hint's too.
  keymap.register(DIALOG_SCOPE, [binding("dialog-go", "g x", { label: "Dialog step" }).definition]);
  keymap.pushScope(DIALOG_SCOPE);
  press("g");
  expect(keymap.chord()).toEqual({ keys: "g", next: [{ key: "x", label: "Dialog step" }] });
});

test("actions() offers one row for an action several lists register, and it runs the one that applies", () => {
  const { keymap } = harness();
  let focused: "main" | "beside" = "beside";
  const main = binding("open", "o", { label: "Open", when: () => focused === "main" });
  const beside = binding("open", "o", { label: "Open", when: () => focused === "beside" });
  keymap.register("project", [main.definition]);
  keymap.register("project", [beside.definition]);
  keymap.pushScope("project");

  const rows = keymap.actions();
  expect(rows.map((row) => [row.scope, row.id])).toEqual([["project", "open"]]);
  rows[0]?.run();
  expect([main.fired(), beside.fired()]).toEqual([0, 1]);

  focused = "main";
  keymap.actions()[0]?.run();
  expect([main.fired(), beside.fired()]).toEqual([1, 1]);
});

test("aria-keyshortcuts names $mod as the platform's one key, a letter in upper case, and no chord", () => {
  expect(ariaKeyshortcuts(["$mod+k"], true)).toBe("Meta+K");
  expect(ariaKeyshortcuts(["$mod+k"], false)).toBe("Control+K");
  expect(ariaKeyshortcuts(["$mod+Alt+m"], true)).toBe("Meta+Alt+M");
  expect(ariaKeyshortcuts(["c"])).toBe("C");
  expect(ariaKeyshortcuts(["b", "Shift+H", "g i", "Alt+Enter"])).toBe("B Shift+H Alt+Enter");
  expect(ariaKeyshortcuts(["g g"])).toBe("");
});

test("a person's key for an action registers the binding again under that key", () => {
  const stored = userPreferenceStorageKey("alice", "shell.keymap");
  window.localStorage.removeItem(stored);
  let snoozed = 0;
  function Page(): null {
    useKeymapScope("inbox");
    useKeymap("inbox", [{ action: "inbox.snooze", run: () => (snoozed += 1) }]);
    return null;
  }
  const snoozeKeys = () =>
    appKeymap.describe().find((entry) => entry.scope === "inbox" && entry.id === "snooze")?.keys;
  const press = (init: KeyboardEventInit) =>
    act(() => {
      window.dispatchEvent(new KeyboardEvent("keydown", { cancelable: true, ...init }));
    });
  const view = render(createElement(KeymapProvider, { login: "alice" }, createElement(Page)));

  try {
    expect(snoozeKeys()).toEqual(["b", "h"]);
    act(() => setActionKeys("alice", "inbox.snooze", ["Shift+B"]));
    expect(snoozeKeys()).toEqual(["Shift+B"]);
    press({ key: "h" });
    expect(snoozed).toBe(0);
    press({ key: "B", shiftKey: true });
    expect(snoozed).toBe(1);
  } finally {
    view.unmount();
    window.localStorage.removeItem(stored);
  }
});

// dispatch://INFRA-2160, R13: a gate (`KeymapGateContext`) is a condition every binding beneath it
// also needs, as the Inbox drawer gates its tree on no dialog standing over it; `?` shows the
// binding as the registry reads it, and a binding registered outside the gate (a dialog's own
// Escape) is left to its own `when`.
test("a binding beneath a gate fires and is enabled only while the gate holds, and one registered outside it is its own", () => {
  let gateHolds = false;
  let snoozed = 0;
  let own = 0;
  function Page(): null {
    useKeymapScope("inbox");
    useKeymap("inbox", [{ action: "inbox.snooze", run: () => (snoozed += 1) }]);
    useKeymap("inbox", [{ id: "own", keys: "q", label: "Its own", run: () => (own += 1) }], {
      outsideGate: true,
    });
    return null;
  }
  const snoozeEnabled = () =>
    appKeymap.describe().find((entry) => entry.scope === "inbox" && entry.id === "snooze")?.enabled;
  const press = (key: string) =>
    act(() => {
      window.dispatchEvent(new KeyboardEvent("keydown", { cancelable: true, key }));
    });
  const view = render(
    createElement(
      KeymapProvider,
      {},
      createElement(KeymapGateContext.Provider, { value: () => gateHolds }, createElement(Page))
    )
  );

  try {
    press("b");
    expect(snoozed).toBe(0);
    expect(snoozeEnabled()).toBe(false);
    press("q");
    expect(own).toBe(1);

    gateHolds = true;
    press("b");
    expect(snoozed).toBe(1);
    expect(snoozeEnabled()).toBe(true);
  } finally {
    view.unmount();
  }
});
