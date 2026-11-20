import { expect, spyOn, test } from "bun:test";

import * as textReferences from "../refs/text-references";
import {
  citedReferences,
  knownIssueKeys,
  linkRowReferences,
  rowReferences,
  rowReferenceWords,
} from "./row-references";

// A row draws every reference in its words as a link named from the page's one batched read,
// and asks the server nothing for it.

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

// The batched read resolves an item id or a block id written short in the owner its reference
// names, and answers under the reference as the request wrote it. So an address written short is
// asked as written, beside one written whole, and a row names its link from that answer.
test("an address that writes its id short is asked for as written, and named from its answer", () => {
  const short = "dispatch://CORE-1/comment/c0b271c7";
  const shortBlock = "dispatch://CORE-1/spec#b-abcd1234";
  expect(
    citedReferences(
      `see ${short} and dispatch://CORE-1/comment/c0b271c7-1111-4222-8333-444455556666 and ${shortBlock} and dispatch://CORE-1/spec#b-r1`
    )
  ).toEqual([
    short,
    "dispatch://CORE-1/comment/c0b271c7-1111-4222-8333-444455556666",
    shortBlock,
    "dispatch://CORE-1/spec#b-r1",
  ]);
  const [link] = rowReferences(`see ${short}`, []);
  if (link === undefined) throw new Error("the text holds one reference");
  expect(link.reference).toBe(short);
  expect(rowReferenceWords(link, new Map([[short, "The comment its short id names"]]))).toEqual({
    key: undefined,
    words: "The comment its short id names",
  });
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

// The graph holds no node for a party, so the batched read refuses a request that names one,
// which would leave every row of the tree unnamed. A row asks for none.
test("a party is never asked for", () => {
  const parties =
    "Ask dispatch://role/reviewer, dispatch://person/alice@example.com and " +
    "dispatch://agent/01a1bb84-93b2-74d6-9d65-fb75e08653ce about dispatch://CORE-7.";
  expect(citedReferences(parties)).toEqual(["dispatch://CORE-7"]);
});

/** The links a pass over `text` makes, each as its href and its words. */
function partyLinks(text: string): (string | null)[][] {
  const root = document.createElement("div");
  root.innerHTML = `<p>${text}</p>`;
  linkRowReferences(root, NAMES);
  // A second pass, as a page makes once names arrive, keeps each party's words.
  linkRowReferences(root, NAMES);
  return [...root.querySelectorAll("a")].map((link) => [
    link.getAttribute("href"),
    link.textContent,
  ]);
}

// A row links a party from its address alone, as every other sink of a reference does
// (`referenceHref`): the read brings no name for one, so the link keeps its writer's words.
test("an agent's session links to its live view, in the words its writer wrote", () => {
  const agent = "dispatch://agent/01a1bb84-93b2-74d6-9d65-fb75e08653ce";
  expect(partyLinks(`Ask ${agent}.`)).toEqual([
    ["/agents/01a1bb84-93b2-74d6-9d65-fb75e08653ce/live", agent],
  ]);
});

test("a role links to the agents list that role filters, in the words its writer wrote", () => {
  expect(partyLinks("Ask dispatch://role/reviewer.")).toEqual([
    ["/agents?role=reviewer", "dispatch://role/reviewer"],
  ]);
});

// No page shows a person (R5), so a person's address is drawn as written and links nothing.
test("a person links to nothing and is drawn as written", () => {
  const text = "Ask dispatch://person/alice@example.com about dispatch://CORE-7.";
  expect(partyLinks(text)).toEqual([
    ["/issues/CORE-7", "CORE-7 The header draws the approval of each step"],
  ]);
  expect(rowReferences(text, []).map((one) => one.reference)).toEqual(["dispatch://CORE-7"]);
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

// A short reference two or more targets carry the id of is answered ambiguous, and drawn as
// written with no link, as the tree's chip draws it. A row read only the names, which hold no
// name for it, so it linked the reference and drew it as its short form, which names no one
// target.
const AMBIGUOUS = "dispatch://CORE-1/comment/c0b271c7";

test("a reference the read answered ambiguous is no reference a row links", () => {
  const text = `see ${AMBIGUOUS} and dispatch://CORE-7`;
  expect(rowReferences(text, []).map((one) => one.reference)).toEqual([
    AMBIGUOUS,
    "dispatch://CORE-7",
  ]);
  expect(rowReferences(text, [], new Set([AMBIGUOUS])).map((one) => one.reference)).toEqual([
    "dispatch://CORE-7",
  ]);
});

test("the pass draws an ambiguous reference as written, and a link it made for one as written", () => {
  const painted = `<p>See ${AMBIGUOUS} and dispatch://CORE-7 today</p>`;
  const drawn = `See ${AMBIGUOUS} and CORE-7 The header draws the approval of each step today`;
  const linked = (root: HTMLElement) =>
    [...root.querySelectorAll("a[data-row-reference]")].map((link) =>
      link.getAttribute("data-row-reference")
    );
  const root = document.createElement("div");
  root.innerHTML = painted;
  linkRowReferences(root, NAMES, new Set([AMBIGUOUS]));
  expect(linked(root)).toEqual(["dispatch://CORE-7"]);
  expect(root.textContent).toBe(drawn);

  // A pass before the read answered linked it, and the pass after puts the words back.
  const earlier = document.createElement("div");
  earlier.innerHTML = painted;
  linkRowReferences(earlier, NAMES);
  expect(linked(earlier)).toEqual([AMBIGUOUS, "dispatch://CORE-7"]);
  linkRowReferences(earlier, NAMES, new Set([AMBIGUOUS]));
  expect(linked(earlier)).toEqual(["dispatch://CORE-7"]);
  expect(earlier.textContent).toBe(drawn);
  // A second pass over the same words finds nothing to do.
  linkRowReferences(earlier, NAMES, new Set([AMBIGUOUS]));
  expect(linked(earlier)).toEqual(["dispatch://CORE-7"]);
  expect(earlier.textContent).toBe(drawn);
});

// Each row's words were read again on every draw. A text is read once for each list
// of issue keys it is read against, however often a row draws it; another text, or another list
// of keys, is read anew. A new set of ambiguous references is applied to the kept reading, so the
// row drops the link it names with no second reading and draws no stale one.
test("a text is read once however often a row draws it, and again for another text or other keys", () => {
  const reads = spyOn(textReferences, "readTextReferences");
  try {
    const text = "Kept reading: see dispatch://CORE-206/spec#b-r1 and CORE-207.";
    const issues = ["CORE-207"];
    const first = rowReferences(text, issues);
    expect(rowReferences(text, issues), "a second draw read other references").toEqual(first);
    expect(
      rowReferences(text, [...issues]),
      "the same keys in another list read other references"
    ).toEqual(first);
    expect(reads, "the same text and keys were read more than once").toHaveBeenCalledTimes(1);
    expect(first.map((one) => [one.reference, one.path])).toEqual([
      ["dispatch://CORE-206/spec#b-r1", "/issues/CORE-206/spec#b-r1"],
      ["dispatch://CORE-207", "/issues/CORE-207"],
    ]);

    const ambiguous = new Set(["dispatch://CORE-206/spec#b-r1"]);
    expect(
      rowReferences(text, issues, ambiguous).map((one) => one.reference),
      "a reference read ambiguous since is still linked"
    ).toEqual(["dispatch://CORE-207"]);
    expect(reads, "a new ambiguous set read the text again").toHaveBeenCalledTimes(1);

    expect(rowReferences(text, []).map((one) => one.reference)).toEqual([
      "dispatch://CORE-206/spec#b-r1",
    ]);
    rowReferences(`${text} More.`, issues);
    expect(reads, "other keys or another text were not read").toHaveBeenCalledTimes(3);
  } finally {
    reads.mockRestore();
  }
});

// A text keeps a reading for four lists of keys. A list it is asked again is its newest, so the
// slot a fifth list evicts is the one asked longest ago, and not the one read first. Each list is
// one array throughout, so its id is the same whatever other sets of keys are forgotten.
test("a text's fifth list of keys evicts the slot asked longest ago, and keeps one just asked again", () => {
  const reads = spyOn(textReferences, "readTextReferences");
  try {
    const text = "Slots kept: see CORE-301, CORE-302, CORE-303, CORE-304 and CORE-305.";
    const first = ["CORE-301"];
    const second = ["CORE-302"];
    for (const issues of [first, second, ["CORE-303"], ["CORE-304"]]) {
      rowReferences(text, issues);
    }
    expect(reads, "four lists of keys were not each read once").toHaveBeenCalledTimes(4);
    rowReferences(text, first);
    expect(reads, "a kept list was read again").toHaveBeenCalledTimes(4);
    rowReferences(text, ["CORE-305"]);
    expect(reads, "the fifth list of keys was not read").toHaveBeenCalledTimes(5);
    rowReferences(text, first);
    expect(reads, "the list just asked again was evicted by the fifth").toHaveBeenCalledTimes(5);
    rowReferences(text, second);
    expect(reads, "the list asked longest ago was kept past the fifth").toHaveBeenCalledTimes(6);
  } finally {
    reads.mockRestore();
  }
});
