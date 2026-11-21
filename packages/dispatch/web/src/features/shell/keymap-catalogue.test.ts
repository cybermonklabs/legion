import { expect, test } from "bun:test";

import { actionBinding, createKeymap } from "./keymap";
import { catalogueEntry, KEYMAP_ACTIONS, keysOfAction, NO_PERSON_KEYS } from "./keymap-catalogue";
import { refusalOf } from "./keymap-refusal";

// R13 and R14: the defaults are keys the refusal check itself accepts, so a person who changes
// nothing has no key Settings would refuse them, and every action a later page adds is held to
// the keys of the actions already here.
test("every action's default keys pass the refusal check", () => {
  const refused = KEYMAP_ACTIONS.flatMap((action) => {
    const verdict = refusalOf(action, catalogueEntry(action).keys, NO_PERSON_KEYS);
    return verdict.kind === "refused" ? [`${action}: ${verdict.message}`] : [];
  });
  expect(refused).toEqual([]);
});

// The Agents page's `/` and `i` take global keys on purpose (`agents/keyboard.ts`), and those are
// the only defaults that do: Settings shows the notice for them as it does for a person's key.
test("the only defaults that take a global action's key are the Agents page's / and i", () => {
  const notices = new Set(
    KEYMAP_ACTIONS.flatMap((action) => {
      const verdict = refusalOf(action, catalogueEntry(action).keys, NO_PERSON_KEYS);
      return verdict.kind === "accepted" ? verdict.notices : [];
    })
  );
  expect([...notices].sort()).toEqual([
    "Open the inbox does not work on the Agents page.",
    "Search does not work on the Agents page.",
  ]);
});

test("a key nobody changes is one that keeps its own action, and no other action may take it", () => {
  const free = KEYMAP_ACTIONS.flatMap((action) =>
    (catalogueEntry(action).fixedKeys ?? []).flatMap((key) =>
      refusalOf("global.go-pipeline", [key], NO_PERSON_KEYS).kind === "refused"
        ? []
        : [`${action}: ${key}`]
    )
  );
  expect(free).toEqual([]);
});

// The registry throws on a prefix in development (`appKeymap`'s `strict`): every action at once,
// under its own scope, is the most any set of open pages can register.
test("every action registered at once, with its defaults, is no prefix conflict", () => {
  const keymap = createKeymap({ strict: true });
  for (const action of KEYMAP_ACTIONS) {
    expect(() =>
      keymap.register(catalogueEntry(action).scope, [
        actionBinding({ action, run: () => undefined }, NO_PERSON_KEYS),
      ])
    ).not.toThrow();
  }
});

test("an action's keys are the person's in place of the defaults, then the keys nobody changes", () => {
  expect(keysOfAction("architecture.open", NO_PERSON_KEYS)).toEqual(["o", "Enter"]);
  expect(keysOfAction("architecture.open", { "architecture.open": ["Shift+O"] })).toEqual([
    "Shift+O",
    "Enter",
  ]);
  expect(keysOfAction("inbox.copy-ref", { "inbox.copy-ref": [] })).toEqual([]);
  expect(keysOfAction("inbox.snooze", NO_PERSON_KEYS)).toEqual(["b", "h"]);
});
