import { beforeAll, expect, test } from "bun:test";

import { loadedEngine, warmMarkdownRenderer } from "./markdown-engine";
import { draftReading, parsedLinkWords } from "./parsed-references";
import { partyLink, partyMention } from "./party-mention";
import {
  applyInsertion,
  groupRows,
  insertionFor,
  insertionText,
  kindOrder,
  matchesQuery,
  orderedRows,
  REF_PICKER_HEADINGS,
  type RefPickerRow,
  readTrigger,
} from "./ref-picker";
import { readTextReferences } from "./text-references";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

function row(kind: RefPickerRow["kind"], label: string, reference: string): RefPickerRow {
  return { detail: "", id: `${kind}-${label}`, kind, label, reference };
}

/** One set of rows, one of every kind the three doors rank differently. */
const rows: RefPickerRow[] = [
  row("issue", "Fix the thing", "dispatch://CORE-1"),
  row("document", "goals.md", "dispatch://CORE/artifact/goals-md"),
  row("comment", "a remark", "dispatch://CORE-1/comment/c1"),
  row("component", "The relay", "dispatch://CORE/component/relay"),
  row("agent", "Planner", "dispatch://agent/planner-session"),
  row("role", "reviewer", "dispatch://role/reviewer"),
  row("person", "alice@example.com", "dispatch://person/alice@example.com"),
];

test("the three doors rank one set of rows three ways, and each reaches every kind", () => {
  const kindsOf = (door: "all" | "parties" | "issues") =>
    groupRows(rows, door).map((group) => group.kind);

  expect(kindsOf("all")).toEqual([
    "document",
    "issue",
    "comment",
    "component",
    "agent",
    "role",
    "person",
  ]);
  expect(kindsOf("parties")).toEqual([
    "person",
    "agent",
    "role",
    "document",
    "issue",
    "comment",
    "component",
  ]);
  expect(kindsOf("issues")).toEqual([
    "issue",
    "document",
    "comment",
    "component",
    "agent",
    "role",
    "person",
  ]);
  // Every door reaches every kind: the three orders are the same set, and so is the kind list.
  for (const door of ["all", "parties", "issues"] as const) {
    expect([...kindOrder(door)].sort()).toEqual([...kindOrder("all")].sort());
    expect(orderedRows(groupRows(rows, door))).toHaveLength(rows.length);
  }
});

test("each group is headed by its kind's own name", () => {
  expect(groupRows(rows, "parties").map((group) => group.heading)).toEqual([
    "People",
    "Agents",
    "Roles",
    "Documents",
    "Issues",
    "Comments",
    "Components",
  ]);
  expect(REF_PICKER_HEADINGS["ask-part"]).toBe("Ask parts");
});

test("a trigger opens only at a word boundary", () => {
  const opens = { inAddress: false };
  expect(readTrigger("@")).toEqual({ ...opens, chars: "@", door: "parties", query: "", start: 0 });
  expect(readTrigger("see @pl")).toEqual({
    ...opens,
    chars: "@",
    door: "parties",
    query: "pl",
    start: 4,
  });
  expect(readTrigger("(#CO")).toEqual({
    ...opens,
    chars: "#",
    door: "issues",
    query: "CO",
    start: 1,
  });
  expect(readTrigger("a line\n#")).toEqual({
    ...opens,
    chars: "#",
    door: "issues",
    query: "",
    start: 7,
  });
  expect(readTrigger("see [[go")).toEqual({
    ...opens,
    chars: "[[",
    door: "all",
    query: "go",
    start: 4,
  });

  // Mid-word: an email's `@`, an anchor's `#`, and a `[[` glued to a word open nothing.
  expect(readTrigger("alice@example.com")).toBeUndefined();
  expect(readTrigger("issue#5")).toBeUndefined();
  expect(readTrigger("note[[")).toBeUndefined();
});

test("a space typed after a trigger closes it, which leaves a heading a heading", () => {
  expect(readTrigger("# ")).toBeUndefined();
  expect(readTrigger("# Heading")).toBeUndefined();
  expect(readTrigger("@planner now")).toBeUndefined();
  // The latest trigger still standing is the one that is open.
  expect(readTrigger("# Heading with @pl")).toEqual({
    chars: "@",
    door: "parties",
    inAddress: false,
    query: "pl",
    start: 15,
  });
});

// A mention's words start with `@` right after a `[`, which is a word boundary: the picker must
// open neither over the mention it has just written nor where the caret moves into a written
// link's words, since a pick there would write a link inside a link. In the mention's address
// the `@` stands before the address begins, so it is no door there either.
test("no trigger opens over a written link, though a mention's words start with one", () => {
  const mention = "[@Planner](dispatch://agent/planner-session)";
  expect(readTrigger("[@reviewer](dispatch://role/reviewer)")).toBeUndefined();
  expect(readTrigger(`see ${mention} and`)).toBeUndefined();
  // The caret in the mention's words, at their end, and in its address.
  expect(readTrigger(mention, "[@Plan".length)).toBeUndefined();
  expect(readTrigger(mention, "[@Planner".length)).toBeUndefined();
  expect(readTrigger(mention, "[@Planner](dispatch://agent/plan".length)).toBeUndefined();
  // Words that hold an escaped bracket, as `partyLink` writes a title holding one.
  expect(readTrigger("[@A\\[b\\]](dispatch://agent/a)", "[@A\\[b".length)).toBeUndefined();
  // A `[` the reader typed is still a word boundary while no link closes after the caret, and a
  // link later in the text does not close the words the reader is typing now.
  expect(readTrigger("[@reviewer")).toEqual({
    chars: "@",
    door: "parties",
    inAddress: false,
    query: "reviewer",
    start: 1,
  });
  expect(readTrigger(`ask @pl then ${mention}`, "ask @pl".length)).toEqual({
    chars: "@",
    door: "parties",
    inAddress: false,
    query: "pl",
    start: 4,
  });
});

// A reader writing a link's address reaches for a door to fill it: every door opens on a trigger
// typed in the address, and says it stands there, so the pick writes the address alone.
test("each door opens in a link's address on a trigger typed there, and says it stands there", () => {
  const start = "[the plan](".length;
  const opens = { inAddress: true, start };
  expect(readTrigger("[the plan](#pl")).toEqual({
    ...opens,
    chars: "#",
    door: "issues",
    query: "pl",
  });
  expect(readTrigger("[the plan](@")).toEqual({ ...opens, chars: "@", door: "parties", query: "" });
  expect(readTrigger("[the plan]([[go")).toEqual({
    ...opens,
    chars: "[[",
    door: "all",
    query: "go",
  });
  // A closed address whose caret stands at its end, before the `)`: the pick writes it whole.
  const closed = "[the plan](#plan)";
  const atEnd = readTrigger(closed, "[the plan](#plan".length);
  expect(atEnd).toEqual({ ...opens, chars: "#", door: "issues", query: "plan" });
  if (atEnd === undefined) throw new Error("the trigger reads nothing");
  expect(
    applyInsertion(
      closed,
      insertionFor(row("issue", "Fix", "dispatch://CORE-1"), atEnd, "[the plan](#plan".length)
    ).text
  ).toBe("[the plan](dispatch://CORE-1)");
});

// A pick writes over the trigger through the caret and no further, so a door stands in an
// address only where the caret ends it. Past the `)` that closes the address the caret is in no
// address at all, and a pick there would write over the `)` and leave the link open
// (`[the plan](dispatch://CORE-1`); before the rest of an address it would leave that rest
// behind its own (`[the plan](dispatch://CORE-1an)`).
test("no door opens in a link's address the caret has left, nor before the rest of one", () => {
  expect(readTrigger("[the plan](#plan)")).toBeUndefined();
  expect(readTrigger("see [the plan](#plan).")).toBeUndefined();
  expect(readTrigger("[the plan](@ana)")).toBeUndefined();
  expect(readTrigger("[the plan]([[goals)")).toBeUndefined();
  // The caret in the middle of an address already closed, and of one still being written.
  expect(readTrigger("[the plan](#plan)", "[the plan](#pl".length)).toBeUndefined();
  expect(readTrigger("[the plan](#plan", "[the plan](#pl".length)).toBeUndefined();
  // The caret in an open address still opens its door, and a trigger typed after a closed link
  // is the prose's own.
  expect(readTrigger("[the plan](#pl")).toEqual({
    chars: "#",
    door: "issues",
    inAddress: true,
    query: "pl",
    start: "[the plan](".length,
  });
  expect(readTrigger("[the plan](#plan) and #CO")).toEqual({
    chars: "#",
    door: "issues",
    inAddress: false,
    query: "CO",
    start: "[the plan](#plan) and ".length,
  });
});

test("the browser-side filter matches a row's words, its detail and its address", () => {
  const agent = {
    ...row("agent", "Planner", "dispatch://agent/planner-session"),
    detail: "/w/one",
  };
  expect(matchesQuery(agent, "")).toBe(true);
  expect(matchesQuery(agent, "plan")).toBe(true);
  expect(matchesQuery(agent, "/w/")).toBe(true);
  expect(matchesQuery(agent, "planner-session")).toBe(true);
  expect(matchesQuery(agent, "reviewer")).toBe(false);
});

test("a party pick inserts its mention and every other pick the bare address", () => {
  const prose = { inAddress: false };
  // A mention's words are `@` and the name: a reader reads it as `@` and the party's current
  // name, so a session renamed since still reads as it is called now (R4).
  expect(insertionText(row("agent", "Planner", "dispatch://agent/planner-session"), prose)).toBe(
    "[@Planner](dispatch://agent/planner-session)"
  );
  expect(insertionText(row("role", "reviewer", "dispatch://role/reviewer"), prose)).toBe(
    "[@reviewer](dispatch://role/reviewer)"
  );
  expect(
    insertionText(row("person", "alice@example.com", "dispatch://person/alice@example.com"), prose)
  ).toBe("[@alice@example.com](dispatch://person/alice@example.com)");
  // A name holding a bracket cannot break out of the link's words.
  expect(insertionText(row("agent", "Planner [2]", "dispatch://agent/two"), prose)).toBe(
    "[@Planner \\[2\\]](dispatch://agent/two)"
  );

  expect(insertionText(row("issue", "Fix the thing", "dispatch://CORE-1"), prose)).toBe(
    "dispatch://CORE-1"
  );
  expect(
    insertionText(row("component", "The relay", "dispatch://CORE/component/relay"), prose)
  ).toBe("dispatch://CORE/component/relay");
  expect(
    insertionText(row("document-block", "First words", "dispatch://CORE-1/spec#b-one"), prose)
  ).toBe("dispatch://CORE-1/spec#b-one");
});

/** The engine the page parses with, which the tests load once. */
function engine() {
  const loaded = loadedEngine();
  if (loaded === undefined) throw new Error("the Markdown engine did not load");
  return loaded;
}

/** What a draft's chip reads the first reference of `body` as, with the words it reads, as the
 *  chips ask it (`draftReading`). */
function chip(body: string) {
  const [found] = readTextReferences(body).references;
  if (found?.route === undefined) throw new Error(`${body} holds no reference`);
  return draftReading(engine(), body, { ...found, route: found.route });
}

// A name may hold a character Markdown reads as formatting - a session titled `fix *the* cache`.
// Written as it is, the formatting covers a part of the link's words, which makes them their
// author's words rather than a mention, and they never read as the party's current name again
// (`@legion/contracts`'s `isMentionWords`). So every writer of a mention writes the name as
// literal words: the pick in a box and in the live composer (`insertionText`), and the seed a
// channel owes its party and a kept draft's rewrite (`partyMention`). Every reader takes the
// words through the parse, the chips among them, so they read as the name and the link as the
// mention. A host that writes marks takes the name's own characters, which its document escapes
// as it stores them.
test("a party's name holding Markdown's markup is written as literal words, which read as a mention", () => {
  const agent = "dispatch://agent/planner-session";
  const picked = row("agent", "fix *the* cache", agent);
  const written = `[@fix \\*the\\* cache](${agent})`;
  expect(insertionText(picked, { inAddress: false })).toBe(written);
  expect(partyMention("session:planner-session", "fix *the* cache")).toBe(written);
  expect(chip(`Ask ${written} now.`)).toEqual({
    picture: false,
    reads: "mention",
    words: "@fix *the* cache",
  });

  const odd = "a_b `c` ~~d~~ \\e [f]";
  const oddWritten = partyLink(odd, agent);
  expect(oddWritten).toBe(`[@a\\_b \\\`c\\\` \\~\\~d\\~\\~ \\\\e \\[f\\]](${agent})`);
  expect(chip(`Ask ${oddWritten} now.`)).toEqual({
    picture: false,
    reads: "mention",
    words: `@${odd}`,
  });

  const trigger = readTrigger("ask @fi");
  if (trigger === undefined) throw new Error("the trigger reads nothing");
  expect(insertionFor(picked, trigger, "ask @fi".length).link).toEqual({
    href: agent,
    words: "@fix *the* cache",
  });
});

// A `<` and an `&` open inline HTML and a character reference: a session titled
// `Fix <Suspense> fallback` written as it is reads `<Suspense>` as a tag, which holds none of the
// words, and a name holding `&amp;` reads back as `&`. Escaped, each reads as the character it is.
test("a party's name holding an angle bracket or an ampersand reads back as written", () => {
  const agent = "dispatch://agent/s1";
  expect(
    ["Fix <Suspense> fallback", "Tom &amp; Jerry"].map(
      (name) => parsedLinkWords(engine(), partyLink(name, agent))?.words
    )
  ).toEqual(["@Fix <Suspense> fallback", "@Tom &amp; Jerry"]);
  expect(partyLink("Fix <Suspense> fallback", agent)).toBe(
    `[@Fix \\<Suspense> fallback](${agent})`
  );
  expect(partyLink("Tom &amp; Jerry", agent)).toBe(`[@Tom \\&amp; Jerry](${agent})`);
  // The seed a channel owes its party and a kept draft's rewrite write the same link.
  expect(partyMention("session:s1", "Fix <Suspense> fallback")).toBe(
    partyLink("Fix <Suspense> fallback", agent)
  );
});

// In a link's address the link around the pick already has its words: a party is its bare
// address there, as every other kind is, and a mention would be a link inside a link.
test("a pick in a link's address inserts the bare address, a party's too, and no link", () => {
  const text = "[the plan](@";
  const trigger = readTrigger(text);
  if (trigger === undefined) throw new Error("the trigger reads nothing");
  for (const party of [
    row("agent", "Planner", "dispatch://agent/planner-session"),
    row("role", "reviewer", "dispatch://role/reviewer"),
    row("person", "alice@example.com", "dispatch://person/alice@example.com"),
  ]) {
    expect(insertionText(party, trigger)).toBe(party.reference);
    expect(insertionFor(party, trigger, text.length)).toEqual({
      end: text.length,
      start: "[the plan](".length,
      text: party.reference,
      trigger: "@",
    });
  }
  expect(
    applyInsertion(
      text,
      insertionFor(
        row("agent", "Planner", "dispatch://agent/planner-session"),
        trigger,
        text.length
      )
    )
  ).toEqual({
    caret: "[the plan](dispatch://agent/planner-session".length,
    text: "[the plan](dispatch://agent/planner-session",
  });
});

test("an issue picked in a link's address completes the link", () => {
  const text = "[the plan](#CO";
  const trigger = readTrigger(text);
  if (trigger === undefined) throw new Error("the trigger reads nothing");
  const insertion = insertionFor(row("issue", "Fix", "dispatch://CORE-1"), trigger, text.length);
  expect(insertion).toEqual({
    end: text.length,
    start: "[the plan](".length,
    text: "dispatch://CORE-1",
    trigger: "#",
  });
  expect(applyInsertion(`${text}) first`, insertion)).toEqual({
    caret: "[the plan](dispatch://CORE-1".length,
    text: "[the plan](dispatch://CORE-1) first",
  });
});

test("an insertion replaces the trigger through the caret and keeps the text after it", () => {
  const text = "see #CO and the rest";
  const caret = "see #CO".length;
  const trigger = readTrigger(text.slice(0, caret));
  if (trigger === undefined) throw new Error("the trigger reads nothing");
  const insertion = insertionFor(row("issue", "Fix", "dispatch://CORE-1"), trigger, caret);

  expect(insertion).toEqual({
    end: 7,
    start: 4,
    text: "dispatch://CORE-1",
    trigger: "#",
  });
  expect(applyInsertion(text, insertion)).toEqual({
    caret: "see dispatch://CORE-1".length,
    text: "see dispatch://CORE-1 and the rest",
  });
});

// A host that writes marks rather than Markdown takes the mention apart (the document body's
// `DocumentRefPicker`): the words it shows and the address it names, which is the same mention
// `text` writes as Markdown.
test("a party's insertion carries its words and its address, and no other pick does", () => {
  const text = "ask @pl";
  const trigger = readTrigger(text);
  if (trigger === undefined) throw new Error("the trigger reads nothing");

  const party = insertionFor(
    row("agent", "Planner", "dispatch://agent/planner-session"),
    trigger,
    text.length
  );
  expect(party).toEqual({
    end: 7,
    link: { href: "dispatch://agent/planner-session", words: "@Planner" },
    start: 4,
    text: "[@Planner](dispatch://agent/planner-session)",
    trigger: "@",
  });

  const issue = insertionFor(row("issue", "Fix", "dispatch://CORE-1"), trigger, text.length);
  expect(issue.link).toBeUndefined();
});
