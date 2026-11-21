import { afterEach, expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { KeymapProvider } from "./KeymapProvider";
import type { KeymapActionId } from "./keymap-catalogue";
import { resetAllKeys, setActionKeys, storedPersonKeys, useActionKeys } from "./keymap-keys";
import { userPreferenceStorageKey } from "./userPreference";

const ALICE_KEY = userPreferenceStorageKey("alice", "shell.keymap");

afterEach(() => {
  window.localStorage.removeItem(ALICE_KEY);
});

/** One reader of an action's keys, which shows them joined by commas. */
function KeysProbe({ action }: { action: KeymapActionId }): ReactNode {
  return createElement("output", { "data-testid": action }, useActionKeys(action).join(","));
}

function drawProbes(login: string | undefined, ...actions: KeymapActionId[]) {
  return render(
    createElement(
      KeymapProvider,
      { login },
      ...actions.map((action) => createElement(KeysProbe, { action, key: action }))
    )
  );
}

const shown = (action: KeymapActionId) => screen.getByTestId(action).textContent;

test("an action takes its defaults until the person changes it, then theirs, in every reader at once", () => {
  const view = drawProbes("alice", "inbox.snooze", "inbox.copy-ref", "architecture.open");
  try {
    expect(shown("inbox.snooze")).toBe("b,h");
    expect(shown("architecture.open")).toBe("o,Enter");

    act(() => setActionKeys("alice", "inbox.snooze", ["Shift+B", "h"]));
    act(() => setActionKeys("alice", "inbox.copy-ref", []));
    act(() => setActionKeys("alice", "architecture.open", ["u"]));
    expect(shown("inbox.snooze")).toBe("Shift+B,h");
    expect(shown("inbox.copy-ref")).toBe("");
    // A key nobody changes stays the action's whatever the person gives it.
    expect(shown("architecture.open")).toBe("u,Enter");
    // The map holds the changed actions and only those.
    expect(JSON.parse(window.localStorage.getItem(ALICE_KEY) ?? "null")).toEqual({
      "architecture.open": ["u"],
      "inbox.copy-ref": [],
      "inbox.snooze": ["Shift+B", "h"],
    });

    // Keys equal to the defaults, and a reset of one action, leave it out of the map.
    act(() => setActionKeys("alice", "inbox.snooze", ["b", "h"]));
    act(() => setActionKeys("alice", "architecture.open", undefined));
    expect(shown("inbox.snooze")).toBe("b,h");
    expect(JSON.parse(window.localStorage.getItem(ALICE_KEY) ?? "null")).toEqual({
      "inbox.copy-ref": [],
    });

    act(() => resetAllKeys("alice"));
    expect(shown("inbox.copy-ref")).toBe("y");
    expect(window.localStorage.getItem(ALICE_KEY)).toBeNull();
  } finally {
    view.unmount();
  }
});

test("with nobody signed in every action takes its defaults, whatever a browser holds", () => {
  window.localStorage.setItem(ALICE_KEY, JSON.stringify({ "inbox.snooze": ["Shift+B"] }));
  const view = drawProbes(undefined, "inbox.snooze");
  try {
    expect(shown("inbox.snooze")).toBe("b,h");
  } finally {
    view.unmount();
  }
});

test("keys another tab stores reach this tab's readers by the window's storage event", () => {
  const view = drawProbes("alice", "inbox.snooze");
  try {
    expect(shown("inbox.snooze")).toBe("b,h");
    // What another tab's write is to this one: the value changes under it, then the browser
    // fires `storage` on this window.
    const stored = JSON.stringify({ "inbox.snooze": ["Shift+B"] });
    window.localStorage.setItem(ALICE_KEY, stored);
    act(() => {
      window.dispatchEvent(
        new StorageEvent("storage", {
          key: ALICE_KEY,
          newValue: stored,
          oldValue: null,
          storageArea: window.localStorage,
        })
      );
    });
    expect(shown("inbox.snooze")).toBe("Shift+B");
  } finally {
    view.unmount();
  }
});

test("a stored key the refusal check refuses now is dropped, and the rest of the map stands", () => {
  window.localStorage.setItem(
    ALICE_KEY,
    JSON.stringify({
      // `e` is Dismiss's, `g` begins Go to Inbox, and `1` keeps its own action.
      "inbox.copy-ref": [],
      "inbox.snooze": ["e", "Shift+B", "g", "1"],
      // An action this build has no more, and a value that is no list of keys.
      "inbox.no-such-action": ["q"],
      "issue.labels": "Shift+L",
    })
  );
  expect(storedPersonKeys("alice")).toEqual({
    "inbox.copy-ref": [],
    "inbox.snooze": ["Shift+B"],
  });
  // One stored string is read once: the same text gives the same object.
  expect(storedPersonKeys("alice")).toBe(storedPersonKeys("alice"));

  window.localStorage.setItem(ALICE_KEY, "not a map");
  expect(storedPersonKeys("alice")).toEqual({});
});

test("a stored plain letter of Search and actions, which also works while the reader types, is dropped", () => {
  window.localStorage.setItem(ALICE_KEY, JSON.stringify({ "global.search": ["u", "$mod+j"] }));
  expect(storedPersonKeys("alice")).toEqual({ "global.search": ["$mod+j"] });
});
