import { beforeAll, expect, test } from "bun:test";
import { referenceReading } from "@legion/contracts";

import { replyPrefill, seededDraft } from "../conversation/composer-draft";

import { loadedEngine, warmMarkdownRenderer } from "./markdown-engine";
import { draftReading, parsedLinkWords } from "./parsed-references";
import { partyMention, partyOfAddress } from "./party-mention";
import {
  draftParties,
  inLinkWords,
  inlineLinks,
  isPartyRoute,
  linkOf,
  partyMentionSpan,
} from "./party-references";
import { readTextReferences } from "./text-references";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const SESSION = "01a1058e-f14f-7684-87eb-3dc885955551";
const AGENT = `dispatch://agent/${SESSION}`;

// Who a draft addresses is in its own words, and a code context is where words are shown rather
// than written to somebody: the server drops a party it holds before any delivery rule reads the
// set (`api.commentPartiesOf`), and the composer must say the same of the draft in front of the
// reader. The three contexts below are the three the shared reader knows (`code-ranges.ts`).

test("a party written as a worded link's address is delivered to", () => {
  expect(draftParties(`Over to [Planner](${AGENT}) now.`)).toEqual({
    named: true,
    targets: [`session:${SESSION}`],
  });
});

test("a party in a fenced code block is no target and names nobody", () => {
  expect(draftParties(`Run this:\n\n\`\`\`sh\ndispatch send ${AGENT}\n\`\`\`\n`)).toEqual({
    named: false,
    targets: [],
  });
});

test("a party in an indented code block is no target and names nobody", () => {
  expect(draftParties(`Pasted from the handbook:\n\n    ${AGENT}\n`)).toEqual({
    named: false,
    targets: [],
  });
});

test("a party in a code span is no target and names nobody", () => {
  expect(draftParties("Run `dispatch message --to dispatch://role/reviewer` first.")).toEqual({
    named: false,
    targets: [],
  });
});

// A person is named but never delivered to (R5), and naming one is what makes a typed `/btw` a
// command: in code it does neither, so the draft's first word stays the reader's own.
test("a person in a code span leaves a draft naming nobody", () => {
  expect(draftParties("Mail `dispatch://person/alice@example.com` about it.")).toEqual({
    named: false,
    targets: [],
  });
});

test("a person named outside code names the draft without delivering to one", () => {
  expect(draftParties("Mail dispatch://person/alice@example.com about it.")).toEqual({
    named: true,
    targets: [],
  });
});

// Each occurrence stands on its own, as it does for the server: the one outside code names the
// party, whatever a fence elsewhere in the draft shows.
test("a party a draft also writes outside code is delivered to", () => {
  expect(
    draftParties("Run `dispatch://role/reviewer` and ask [the reviewer](dispatch://role/reviewer).")
  ).toEqual({ named: true, targets: ["role:reviewer"] });
});

test("a draft naming parties lists each once, in the order it names them", () => {
  expect(
    draftParties(`[Reviewer](dispatch://role/reviewer), [Planner](${AGENT}) and ${AGENT} again.`)
  ).toEqual({ named: true, targets: ["role:reviewer", `session:${SESSION}`] });
});

// The mention a channel takes out of its draft when the reader leaves it (`seededDraft`): the
// link's words with its address, and nothing at all where the only occurrence is in code - that
// text is the reader's own, not an address this channel owes.
test("a party's mention is found with the words that name it", () => {
  const body = `Over to [Planner](${AGENT}) now.`;

  expect(partyMentionSpan(body, `session:${SESSION}`)).toEqual({
    end: body.indexOf(") now.") + 1,
    start: body.indexOf("[Planner]"),
  });
});

test("a party written only in code is mentioned nowhere", () => {
  expect(partyMentionSpan(`Run \`dispatch send ${AGENT}\` first.`, `session:${SESSION}`)).toBe(
    undefined
  );
});

// The draft reader recognizes a session reference even when it cannot become a delivery target.
// This says nothing about whether the server accepts a new comment carrying that reference.
test("a non-routable agent reference names a party in the draft but gives no delivery target", () => {
  expect(draftParties("[x](dispatch://agent/a.b) please look.")).toEqual({
    named: true,
    targets: [],
  });
  expect(partyMentionSpan("[x](dispatch://agent/a.b) please look.", "session:a.b")).toBe(undefined);
});

test("a delivery address is read as a party only where a route is written that way", () => {
  expect(partyOfAddress(`session:${SESSION}`)).toEqual({ kind: "agent-session", session: SESSION });
  expect(partyOfAddress("role:reviewer")).toEqual({ kind: "role", role: "reviewer" });
  for (const target of [
    "session:a.b",
    "session:",
    "session:x) [y](https://evil.example",
    "role:Reviewer",
    "role:",
    "agent:s1",
    SESSION,
  ]) {
    expect(partyOfAddress(target)).toBe(undefined);
    expect(partyMention(target, "Planner")).toBe(undefined);
  }
});

// The mention a channel seeds and a saved draft's old `@Name` becomes (`held-sends.ts`): the
// mention sign and the party's name, which every reader then reads as `@` and the party's
// current name (R4).
test("a party's mention is written as the mention sign and its name", () => {
  expect(partyMention(`session:${SESSION}`, "Planner")).toBe(`[@Planner](${AGENT})`);
  expect(partyMention("role:reviewer", " reviewer ")).toBe("[@reviewer](dispatch://role/reviewer)");
  // A party with no name that fits on one line is mentioned by its address.
  expect(partyMention(`session:${SESSION}`, "two\nlines")).toBe(`[@session:${SESSION}](${AGENT})`);
});

// What the composer writes in front of the reader - a channel's seed into an empty draft, and a
// new reply's prefill - ends in one space after its last mention (`composer-draft.ts`), so a word
// the reader types at once is a word of its own after the mention, as a renderer draws it, rather
// than joined to the link's words; the mention still names its party alone. A seed that mentions
// nobody writes nothing, and the reader's own words under a seed are kept as they stand.
test("a seed and a reply's prefill end in one space, so a word typed at once names no other party", () => {
  const owner = { issueKey: "CORE-1", kind: "issue" } as const;
  const reviewer = { target: "role:reviewer", title: "reviewer" };
  const planner = { target: `session:${SESSION}`, title: "Planner" };
  const prefill = (mentions: readonly { target: string; title: string }[]) =>
    replyPrefill({
      actor: { id: "alice", kind: "user" },
      excerpt: "Can this ship?",
      id: "comment-1",
      mentions,
      parentKind: "comment",
    }).body;
  const both = `[@reviewer](dispatch://role/reviewer) [@Planner](${AGENT}) `;

  expect(prefill([planner])).toBe(`[@Planner](${AGENT}) `);
  expect(prefill([reviewer, planner])).toBe(both);
  expect(seededDraft([reviewer, planner], "", owner)).toBe(both);
  expect(prefill([])).toBe("");
  expect(seededDraft([], "", owner)).toBe("");
  expect(seededDraft([planner], "hello", owner)).toBe(`[@Planner](${AGENT}) hello`);
  for (const [seed, carried] of [
    [prefill([planner]), [planner]],
    [prefill([reviewer, planner]), [reviewer, planner]],
    [seededDraft([reviewer, planner], "", owner), [reviewer, planner]],
  ] as const) {
    expect(draftParties(`${seed}hello`).targets).toEqual(carried.map((party) => party.target));
  }
});

// A worded link reads as a party's current name only where its words start with the mention
// sign and its address names a party, a hand-written one as much as a picked one; every other
// worded link keeps its author's words (R8). Which address names a party is this reader's own
// grammar (`isPartyRoute`), and the rest of the rule is `@legion/contracts`'s.
test("a mention is a party's link whose words start with the mention sign", () => {
  const reads = (words: string, address: string) => {
    const route = readTextReferences(address).references[0]?.route;
    return referenceReading({
      address,
      party: route !== undefined && isPartyRoute(route),
      words,
    });
  };
  expect(reads("@Bob", AGENT)).toBe("mention");
  expect(reads("@reviewer", "dispatch://role/reviewer")).toBe("mention");
  expect(reads("@alice", "dispatch://person/alice@example.com")).toBe("mention");
  expect(reads("Bob", AGENT)).toBe("words");
  expect(reads(" @Bob", AGENT)).toBe("words");
  expect(reads("@the fix", "dispatch://CORE-1")).toBe("words");
  expect(reads("@Bob", "dispatch://agent/")).toBe("words");
});

/** The link the first reference `body` holds stands in (`linkOf`). */
function linkAt(body: string) {
  const [found] = readTextReferences(body).references;
  if (found === undefined) throw new Error(`${body} holds no reference`);
  return linkOf(body, found);
}

/** What `partyMentionSpan` takes out of `body` for the session the tests name. */
function mentionTaken(body: string): string | undefined {
  const span = partyMentionSpan(body, `session:${SESSION}`);
  return span === undefined ? undefined : body.slice(span.start, span.end);
}

/** The engine the page parses with, which the tests load once. */
function engine() {
  const loaded = loadedEngine();
  if (loaded === undefined) throw new Error("the Markdown engine did not load");
  return loaded;
}

/** The words of the link the first reference of `body` stands in, as its parse reads them
 *  (`parsedLinkWords`), with how they are formatted. */
function wordsAt(body: string) {
  const link = linkAt(body);
  return link === undefined ? undefined : parsedLinkWords(engine(), link.markdown);
}

/** What a draft's pill draws for the first reference of `body`, as the chips ask it
 *  (`conversation/draft-pills.tsx`, through `draftReading`). */
function pill(body: string) {
  const [found] = readTextReferences(body).references;
  if (found?.route === undefined) throw new Error(`${body} holds no reference`);
  return draftReading(engine(), body, { ...found, route: found.route });
}

/** What a draft's pill reads the first reference of `body` as. */
function pillReads(body: string): string {
  return pill(body).reads;
}

// What a draft's pill reads a reference by: the link around it, as a reader of the stored text
// sees it - its words, or a picture, which is drawn as the picture and is no link.
test("a link is read around its address, a picture is told apart, and a reference on its own has none", () => {
  expect(linkAt(`Ask [@Planner](${AGENT}) now.`)).toEqual({
    markdown: `[@Planner](${AGENT})`,
    picture: false,
  });
  expect(pill(`Ask [@Planner](${AGENT}) now.`)).toEqual({
    picture: false,
    reads: "mention",
    words: "@Planner",
  });
  expect(pill(`Ask [@Planner \\[2\\]](${AGENT}) now.`)).toEqual({
    picture: false,
    reads: "mention",
    words: "@Planner [2]",
  });
  expect(linkAt(`Ask ${AGENT} now.`)).toBe(undefined);
  expect(pill(`Ask ${AGENT} now.`)).toEqual({ picture: false, reads: "name", words: AGENT });
  expect(pill("![shot.png](dispatch://CORE-1/artifact/shot-png@v1)")).toEqual({
    picture: true,
    reads: "words",
    words: "shot.png",
  });
});

// A hand-written link is read as the renderer reads it, whole: its words may hold brackets that
// balance, a title may follow its address, and its address may stand in angle brackets.
test("a link whose words hold brackets that balance is read whole", () => {
  const body = `Ask [@Alice [x]](${AGENT}) now.`;
  expect(pill(body).words).toBe("@Alice [x]");
  expect(mentionTaken(body)).toBe(`[@Alice [x]](${AGENT})`);
});

test("a link with a title after its address is read to its closing parenthesis", () => {
  const body = `Ask [@Alice](${AGENT} "the planner") now.`;
  expect(pill(body).words).toBe("@Alice");
  expect(mentionTaken(body)).toBe(`[@Alice](${AGENT} "the planner")`);
});

// A link's address read as any span is, its sentence punctuation dropped, is still the address of
// the link it stands in: the pill reads the link's words, as the stored record draws them (R8),
// and taking the mention out takes the whole link.
test("a link whose address ends in sentence punctuation is read around its address", () => {
  expect(linkAt("See [the plan](dispatch://CORE-1.) now.")).toEqual({
    markdown: "[the plan](dispatch://CORE-1.)",
    picture: false,
  });
  const body = `Ask [@Alice](${AGENT}.) now.`;
  expect(pill(body)).toEqual({ picture: false, reads: "mention", words: "@Alice" });
  expect(mentionTaken(body)).toBe(`[@Alice](${AGENT}.)`);
});

test("a link whose address stands in angle brackets is read whole", () => {
  const body = `Ask [@Alice](<${AGENT}>) now.`;
  expect(pill(body).words).toBe("@Alice");
  expect(mentionTaken(body)).toBe(`[@Alice](<${AGENT}>)`);
});

// Markdown reads `\@` as `@`, so a link whose sign is written escaped is the mention the
// rendered record draws (`reference-naming.test.tsx`): the draft's reader takes its words
// through the parse every reader of the record takes them through (`parsedLinkWords`).
test("a link whose words open with an escaped mention sign reads as the mention it is drawn as", () => {
  const body = `Ask [\\@Alice \\(lead\\)](${AGENT}) now.`;
  expect(pill(body)).toEqual({ picture: false, reads: "mention", words: "@Alice (lead)" });
});

// The parse decodes an entity and pairs formatting runs as the record's parser pairs them, which
// no reading of the characters short of that parser does: `&#64;` is the mention sign, the runs
// of `**@a*` pair nothing, so its words open with `*`, and `*@a**b*` is one emphasis over all of
// its words.
test("a draft's link words are the parser's, entities and delimiter runs alike", () => {
  expect(pill(`Ask [&#64;Alice](${AGENT}) now.`)).toEqual({
    picture: false,
    reads: "mention",
    words: "@Alice",
  });
  expect(wordsAt(`Ask [**@a*](${AGENT}) now.`)).toEqual({ formatting: "plain", words: "**@a*" });
  expect(pillReads(`Ask [**@a*](${AGENT}) now.`)).toBe("words");
  expect(wordsAt(`Ask [*@a**b*](${AGENT}) now.`)).toEqual({ formatting: "plain", words: "@a**b" });
  expect(pillReads(`Ask [*@a**b*](${AGENT}) now.`)).toBe("mention");
});

// The formatting rule every reader of a record applies (`@legion/contracts`'s
// `dispatch-mentions.ts`), read through a draft's parse, in its three cases. First: code
// anywhere in a link's words keeps the words.
test("a draft's link with code anywhere in its words reads as its words", () => {
  expect(wordsAt(`Ask [\`@Alice\`](${AGENT}) now.`)).toEqual({
    formatting: "code",
    words: "@Alice",
  });
  expect(pillReads(`Ask [\`@Alice\`](${AGENT}) now.`)).toBe("words");
  expect(pillReads(`Ask [@Alice \`lead\`](${AGENT}) now.`)).toBe("words");
});

// Second: formatting over all of a link's words is the same record as that formatting around
// the link, so the link is still a mention, and its words are read without the runs that
// write the formatting - inside the brackets, around the link, or around the sentence.
test("a draft's link whose words are all under one formatting reads as a mention", () => {
  expect(wordsAt(`Ask [**@Alice**](${AGENT}) now.`)).toEqual({
    formatting: "plain",
    words: "@Alice",
  });
  expect(pillReads(`Ask [**@Alice**](${AGENT}) now.`)).toBe("mention");
  expect(pillReads(`Ask *[@Alice](${AGENT})* now.`)).toBe("mention");
  expect(pillReads(`~~Ask [@Alice](${AGENT}) now.~~`)).toBe("mention");
});

// Third: formatting over some of a link's words makes them the author's words, and so does a
// hard break among them, which cuts the words as such formatting does.
test("a draft's link whose words are formatted in part reads as its words", () => {
  expect(wordsAt(`Ask [@Alice **lead**](${AGENT}) now.`)).toEqual({
    formatting: "mixed",
    words: "@Alice lead",
  });
  expect(pillReads(`Ask [@Alice **lead**](${AGENT}) now.`)).toBe("words");
  expect(pillReads(`Ask [*@Alice* lead](${AGENT}) now.`)).toBe("words");
  expect(pillReads(`Ask [@Alice ~~lead~~](${AGENT}) now.`)).toBe("words");
  expect(wordsAt(`Ask [@Alice\\\nlead](${AGENT}) now.`)).toEqual({
    formatting: "mixed",
    words: "@Alice lead",
  });
  expect(pillReads(`Ask [@Alice\\\nlead](${AGENT}) now.`)).toBe("words");
});

// The renderer links no reference a link's words hold (`RefLink.tsx`'s `linkPassReads`), while a
// link's address is the reference the link is.
test("a reference in a link's words is told from the link's address", () => {
  const inWords = (body: string) =>
    readTextReferences(body).references.map((found) => [found.text, inLinkWords(body, found)]);
  expect(inWords("[see CORE-1 here](https://x)")).toEqual([["CORE-1", true]]);
  expect(inWords("[CORE-1](dispatch://CORE-2)")).toEqual([
    ["CORE-1", true],
    ["dispatch://CORE-2", false],
  ]);
  expect(inWords("CORE-1 and [the fix](dispatch://CORE-2)")).toEqual([
    ["CORE-1", false],
    ["dispatch://CORE-2", false],
  ]);
  // A bracket a code span holds closes no link's words, as CommonMark reads a code span first.
  expect(inWords("[CORE-1 `]` too](dispatch://CORE-2)")).toEqual([
    ["CORE-1", true],
    ["dispatch://CORE-2", false],
  ]);
  // A link's address may run on far longer than any a person types - a dashboard's URL with its
  // query - and it is a link to both CommonMark parsers however long it is, so the key in its
  // words is in a link's words, which the renderer links none of.
  const longAddress = `https://grafana.example/d/x?${"var=1&".repeat(200)}`;
  expect(inWords(`[CORE-12 p99](${longAddress}) is slow`)).toEqual([["CORE-12", true]]);
  expect(inlineLinks(`[CORE-12 p99](${longAddress}) is slow`).map((link) => link.close)).toEqual([
    `[CORE-12 p99](${longAddress})`.length,
  ]);
});

// A link holds no link: once one closes, no bracket open before it opens one, while a bracket
// opened after it may, and a picture's caption may hold a link. A draft's pills ask about every
// reference of one text in turn, and the text is read for its links once (`inlineLinks`).
test("a link holds no link, a picture's caption may, and one text is read for its links once", () => {
  const body = [
    `[a [b](${AGENT}) c](dispatch://CORE-2)`,
    "[ [d](dispatch://CORE-4) [e](dispatch://CORE-5) ](dispatch://CORE-6)",
    "![x [y](dispatch://CORE-3)](dispatch://CORE-1/artifact/p-png@v1)",
  ].join(" ");
  expect(
    inlineLinks(body).map((link) => [body.slice(link.addressStart, link.addressEnd), link.picture])
  ).toEqual([
    [AGENT, false],
    ["dispatch://CORE-4", false],
    ["dispatch://CORE-5", false],
    ["dispatch://CORE-3", false],
    ["dispatch://CORE-1/artifact/p-png@v1", true],
  ]);
  expect(inlineLinks(body)).toBe(inlineLinks(body));
});

// A backslash escapes ASCII punctuation only, as CommonMark and goldmark - under the display tree
// the page draws - read it: before a space or a line break it is a backslash, and the space or
// the line break does what it does anywhere, while before a bracket or a parenthesis it takes it.
test("a backslash escapes punctuation only, so a space or a line break after one still ends what it ends", () => {
  const addresses = (body: string) =>
    inlineLinks(body).map((link) => body.slice(link.addressStart, link.addressEnd));
  // The space ends the address `a\`, and `b` is no title.
  expect(addresses("[x](a\\ b)")).toEqual([]);
  // The line break ends an address in angle brackets.
  expect(addresses("[x](<a\\\nb>)")).toEqual([]);
  // The blank line ends the paragraph, the bracket open in it and the title open in it.
  expect(addresses("[x\\\n\ny](dispatch://CORE-1)")).toEqual([]);
  expect(addresses('[x](a "T\\\n\nU")')).toEqual([]);
  // A space after a backslash in the words closes no bracket, and punctuation is taken.
  expect(addresses("[x\\ y](dispatch://CORE-1)")).toEqual(["dispatch://CORE-1"]);
  expect(addresses("[x\\]y](dispatch://CORE-1)")).toEqual(["dispatch://CORE-1"]);
  expect(addresses("[x](a\\(b)")).toEqual(["a\\(b"]);
  expect(addresses("[x](a\\b)")).toEqual(["a\\b"]);
  // An escaped `!` opens no picture; one after an escaped backslash does.
  expect(inlineLinks("\\![x](a)").map((link) => link.picture)).toEqual([false]);
  expect(inlineLinks("\\\\![x](a)").map((link) => link.picture)).toEqual([true]);
});

// A `](` that opens no link - parentheses that never balance, a title that never closes - is read
// through stops found once for the whole text, never across the rest of its paragraph, so a draft
// of thousands of them is read in time that grows with its length, not with its square (8,000 of
// either took 6 to 12 seconds read to the end of the text), and still reads the link after them.
test("a draft of thousands of `](` that open no link is read in linear time, and its real link still found", () => {
  const tail = ` [@Alice](${AGENT}) now.`;
  // The third: every title closes at the one `)`, and a long run of spaces after it stands
  // before something that is no `)`.
  for (const body of [
    `${"[a](((".repeat(8000)}${tail}`,
    `${"[a](x (".repeat(8000)}${tail}`,
    `${"[a](x (".repeat(8000)})${" ".repeat(8000)}.${tail}`,
  ]) {
    const started = performance.now();
    const links = inlineLinks(body);
    expect(performance.now() - started).toBeLessThan(3000);
    expect(links.map((link) => body.slice(link.addressStart, link.addressEnd))).toEqual([AGENT]);
  }
  // A blank line ends the paragraph, and the link with it.
  expect(inlineLinks(`[@Alice](\n\n${AGENT})`)).toEqual([]);
  expect(inlineLinks(`[@Alice](${AGENT} "the\n\nplanner")`)).toEqual([]);
  expect(inlineLinks(`[@Alice](\n${AGENT} "the\nplanner")`)).toHaveLength(1);
});
