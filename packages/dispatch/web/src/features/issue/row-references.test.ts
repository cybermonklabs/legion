import { expect, test } from "bun:test";

import {
  citedReferences,
  knownIssueKeys,
  linkRowReferences,
  rowReferences,
  rowReferenceWords,
} from "./row-references";

// dispatch://INFRA-1196 R19 and R21, the tester's F14 and F17: a row draws every reference in its
// words as a link named from the page's one batched read, and asks the server nothing for it.

const NAMES = new Map([
  ["dispatch://CORE-7", "The header draws the approval of each step"],
  ["dispatch://CORE-1/ask/ask-1", "At which width does the\nidentity block condense?"],
]);

test("what a text cites is every whole address and every bare key, each once", () => {
  expect(
    citedReferences(
      "From: dispatch://CORE-7, and dispatch://CORE-9 R28. See CORE-7 and UTF-16, and CORE-12."
    )
  ).toEqual(["dispatch://CORE-7", "dispatch://CORE-9", "dispatch://UTF-16", "dispatch://CORE-12"]);
  expect(citedReferences("Nothing is cited here.")).toEqual([]);
  expect(citedReferences("")).toEqual([]);
});

test("a bare key links only where the batched read answered that its issue exists", () => {
  const issues = knownIssueKeys(NAMES);
  expect(issues).toEqual(["CORE-7"]);
  const text = "CORE-7 is encoded as UTF-16, as dispatch://CORE-9 says.";
  expect(
    rowReferences(text, issues).map((one) => [text.slice(one.start, one.end), one.reference])
  ).toEqual([
    ["CORE-7", "dispatch://CORE-7"],
    ["dispatch://CORE-9", "dispatch://CORE-9"],
  ]);
});

test("an issue reads as its key and title, and anything else as its target's own words", () => {
  const [issue, unnamed, ask] = rowReferences(
    "dispatch://CORE-7 dispatch://CORE-9 dispatch://CORE-1/ask/ask-1",
    []
  );
  if (issue === undefined || unnamed === undefined || ask === undefined) {
    throw new Error("the text holds three references");
  }
  expect(rowReferenceWords(issue, NAMES)).toEqual({
    key: "CORE-7",
    words: "The header draws the approval of each step",
  });
  // An issue the read has not named yet is its key alone, never an empty gap.
  expect(rowReferenceWords(unnamed, NAMES)).toEqual({ key: "CORE-9", words: "" });
  expect(rowReferenceWords(ask, NAMES)).toEqual({
    key: undefined,
    words: "At which width does the identity block condense?",
  });
  // A target the read has not named reads as its short form, as a link of any body does.
  expect(rowReferenceWords(ask, new Map())).toEqual({ key: undefined, words: "CORE-1 ask" });
});

test("the pass links what painted words cite, and names it again when the names arrive", () => {
  const root = document.createElement("div");
  root.innerHTML =
    "<p>From: dispatch://CORE-7, and <code>dispatch://CORE-7</code> in UTF-16.</p>" +
    '<p><a href="https://example.com">dispatch://CORE-7</a></p>';
  linkRowReferences(root, new Map());
  const links = () => [...root.querySelectorAll<HTMLAnchorElement>("a[data-row-reference]")];
  expect(
    links().map((link) => [link.getAttribute("href"), link.textContent, link.tabIndex])
  ).toEqual([["/issues/CORE-7", "CORE-7", -1]]);
  // The words around it stand, and a reference inside code or inside a link is left as written.
  expect(root.firstElementChild?.textContent).toBe(
    "From: CORE-7, and dispatch://CORE-7 in UTF-16."
  );
  expect(root.querySelector("code")?.textContent).toBe("dispatch://CORE-7");
  expect(root.querySelector('a[href="https://example.com"]')?.textContent).toBe(
    "dispatch://CORE-7"
  );
  linkRowReferences(root, NAMES);
  expect(links().map((link) => link.textContent)).toEqual([
    "CORE-7 The header draws the approval of each step",
  ]);
  // A second pass over the same words makes no second link.
  linkRowReferences(root, NAMES);
  expect(links()).toHaveLength(1);
});
