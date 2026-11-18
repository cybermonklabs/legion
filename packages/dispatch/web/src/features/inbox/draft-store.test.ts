import { expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import { createElement } from "react";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { DraftsNotKept } from "./DraftsNotKept";
import { createDraftStore, DRAFT_KEPT_UNITS, STORE_KEPT_UNITS } from "./draft-store";

/** Where the store named `answer` keeps the draft of `key` in the tab. */
const entry = (key: string) => `dispatch.draft:answer:${key}`;

/** The shell's word on drafts the tab does not keep, or nothing while it keeps every one. */
function notKeptWords(): string {
  return screen.queryByTestId("drafts-not-kept")?.textContent ?? "(nothing said)";
}

/** A tab's storage that holds what it held and refuses every write, as a full one does. */
function refusingStorage(): Storage {
  const { storage } = keptTab();
  return {
    clear: () => storage.clear(),
    getItem: (name) => storage.getItem(name),
    key: (index) => storage.key(index),
    get length() {
      return storage.length;
    },
    removeItem: (name) => storage.removeItem(name),
    setItem: () => {
      throw new DOMException("The quota has been exceeded.", "QuotaExceededError");
    },
  };
}

// A reload loads every module again over the same tab, so a store made over the same storage is
// the store of the next page load.
test("a draft is kept in the tab, and the next page load takes it up once the same person signs in", () => {
  const { storage } = keptTab();
  const before = createDraftStore("answer", storage);
  before.signedIn("alice");
  before.set("ask-1", "My answer");
  const after = createDraftStore("answer", storage);
  const beforeTheSignIn = after.read("ask-1");
  after.signedIn("alice");
  expect({ afterTheSignIn: after.read("ask-1"), beforeTheSignIn }).toEqual({
    afterTheSignIn: "My answer",
    beforeTheSignIn: "",
  });
});

test("the next page load takes up none of another person's drafts, and drops them from the tab", () => {
  const { storage } = keptTab();
  const before = createDraftStore("answer", storage);
  before.signedIn("alice");
  before.set("ask-1", "My answer");
  const after = createDraftStore("answer", storage);
  after.signedIn("bob");
  expect({ kept: storage.getItem(entry("ask-1")), read: after.read("ask-1") }).toEqual({
    kept: null,
    read: "",
  });
});

test("another person's sign-in forgets every draft in the tab too, and a lapse keeps them", () => {
  const { storage } = keptTab();
  const store = createDraftStore("answer", storage);
  store.signedIn("alice");
  store.set("ask-1", "My answer");
  store.signedIn(undefined);
  const lapsed = { kept: storage.getItem(entry("ask-1")) !== null, read: store.read("ask-1") };
  store.signedIn("bob");
  const reloaded = createDraftStore("answer", storage);
  reloaded.signedIn("alice");
  expect({
    afterBob: { kept: storage.getItem(entry("ask-1")), read: store.read("ask-1") },
    lapsed,
    reloadedForAlice: reloaded.read("ask-1"),
  }).toEqual({
    afterBob: { kept: null, read: "" },
    lapsed: { kept: true, read: "My answer" },
    reloadedForAlice: "",
  });
});

test("with nobody signed in yet, a draft stays in the page and nothing is kept in the tab", () => {
  const { storage } = keptTab();
  const store = createDraftStore("answer", storage);
  store.set("ask-1", "My answer");
  expect({ kept: storage.length, read: store.read("ask-1") }).toEqual({
    kept: 0,
    read: "My answer",
  });
});

// The bound of one draft: past it the draft stays in the page, the copy the tab kept of it
// before goes, since a reload would bring back text the reader has since changed, and the shell
// says so. Back within it, the draft is kept and the word goes.
test("a draft past the bound of one draft stays in the page, is not kept, and the page says so", () => {
  const { storage } = keptTab();
  const store = createDraftStore("answer", storage);
  store.signedIn("alice");
  render(createElement(DraftsNotKept));
  act(() => store.set("ask-1", "Short"));
  act(() => store.set("ask-1", "x".repeat(DRAFT_KEPT_UNITS + 1)));
  const past = {
    kept: storage.getItem(entry("ask-1")),
    read: store.read("ask-1").length,
    said: notKeptWords(),
  };
  act(() => store.set("ask-1", "x".repeat(DRAFT_KEPT_UNITS)));
  expect({
    at: { kept: storage.getItem(entry("ask-1")) !== null, said: notKeptWords() },
    past,
  }).toEqual({
    at: { kept: true, said: "(nothing said)" },
    past: {
      kept: null,
      read: DRAFT_KEPT_UNITS + 1,
      said: "A draft you have not sent is longer than 65,536 characters, the most this tab keeps of one draft for a reload, so it is lost if the page reloads. It stays while the page is open.",
    },
  });
});

// The bound of one store: the draft that would take the store past it stays in the page and is
// not kept, the drafts kept before it stay kept, and the shell says so; once a kept draft leaves
// the tab, the one that waited is kept and the word goes.
test("a draft that would take its store past its bound is not kept, the page says so, and it is kept once there is room", () => {
  const { storage } = keptTab();
  const store = createDraftStore("answer", storage);
  store.signedIn("alice");
  render(createElement(DraftsNotKept));
  const drafts = STORE_KEPT_UNITS / DRAFT_KEPT_UNITS;
  act(() => {
    for (let index = 0; index <= drafts; index += 1) {
      store.set(`ask-${index}`, String(index).repeat(DRAFT_KEPT_UNITS));
    }
  });
  const full = {
    kept: Array.from({ length: drafts + 1 }, (_, index) => storage.getItem(entry(`ask-${index}`)))
      .map((kept) => kept !== null)
      .join(" "),
    said: notKeptWords(),
  };
  act(() => store.set("ask-0", ""));
  expect({
    full,
    room: {
      kept: storage.getItem(entry(`ask-${drafts}`)) !== null,
      read: store.read(`ask-${drafts}`).length,
      said: notKeptWords(),
    },
  }).toEqual({
    full: {
      kept: "true true true true false",
      said: "This tab keeps at most 262,144 characters of drafts of one kind for a reload and holds that many already, so a newer draft you have not sent is lost if the page reloads. It stays while the page is open; send or discard an older draft and it is kept.",
    },
    room: { kept: true, read: DRAFT_KEPT_UNITS, said: "(nothing said)" },
  });
});

test("a storage that refuses the write keeps the draft in the page, and the page says so", () => {
  const store = createDraftStore("answer", refusingStorage());
  store.signedIn("alice");
  render(createElement(DraftsNotKept));
  act(() => store.set("ask-1", "My answer"));
  const refused = { read: store.read("ask-1"), said: notKeptWords() };
  act(() => store.set("ask-1", ""));
  expect({ dropped: notKeptWords(), refused }).toEqual({
    dropped: "(nothing said)",
    refused: {
      read: "My answer",
      said: "This browser refused to keep a draft you have not sent (its storage for this tab is full, or turned off), so the draft is lost if the page reloads. It stays while the page is open.",
    },
  });
});
