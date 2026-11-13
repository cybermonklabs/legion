import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { issueStatuses, statusText } from "../project/board-model";
import { ASK_RESOLUTION_WORDS } from "../refs/actor";
import {
  HELP_LINES,
  KEY_REPORTS,
  MARGIN_SECTION_NAMES,
  MARGIN_SECTION_ORDER,
  PROPERTY_LABELS,
} from "./page-structure";
import {
  ANSWER_LINES,
  ANSWER_PHRASES,
  ANSWER_SOURCES,
  APPROVAL_STATE_WORDS,
  AUTHOR_KIND_WORDS,
  BLOCK_ATTRIBUTE_LABELS,
  BLOCKED_BY_REFUSALS,
  blockAttributeLabel,
  CHECK_WHEN_WORDS,
  CONTROL_LABELS,
  CONVERSATION_STATE_NAMES,
  CONVERSATION_STATE_ORDER,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  FILE_WORDS,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  ISSUE_STATUS_WORDS,
  issueStatusWord,
  NODE_FACT_LABELS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  type NodeState,
  nodeFactLabel,
  nodeStateWord,
  nodeStateWords,
  nodeTypeName,
  PRESET_NAMES,
  PRESET_ORDER,
  PROGRESS_NOUNS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  STATE_LINES,
  UNASKED_SOURCES,
  wordsIn,
} from "./page-words";

/** The block schema the server owns, read here rather than repeated, so a type, an attribute or
 *  a `when` choice added to it fails this file until the page has a word for it. */
interface BlockSchema {
  readonly types: readonly {
    readonly attributes: Readonly<Record<string, { readonly choices?: readonly string[] }>>;
    readonly name: string;
  }[];
}

const schema: BlockSchema = JSON.parse(
  readFileSync(
    join(import.meta.dir, "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json"),
    "utf8"
  )
);

function blockType(name: string) {
  const type = schema.types.find((candidate) => candidate.name === name);
  if (type === undefined) {
    throw new Error(`blocks.json declares no ${name} block`);
  }
  return type;
}

function choicesOf(type: string, attribute: string): readonly string[] {
  const choices = blockType(type).attributes[attribute]?.choices;
  if (choices === undefined) {
    throw new Error(`the ${type} block's ${attribute} declares no choices`);
  }
  return choices;
}

/** Every string a declaration holds, however deeply. The module derives `DECLARED_WORDS` the
 *  same way; this walk is written again here so the test checks the derivation rather than
 *  calling it. */
function phrasesOf(value: unknown): readonly string[] {
  if (typeof value === "string") {
    return [value];
  }
  if (typeof value === "object" && value !== null) {
    return Object.values(value).flatMap(phrasesOf);
  }
  return [];
}

const UNASKED: readonly unknown[] = [
  NODE_TYPE_NAMES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  CONVERSATION_STATE_NAMES,
  ISSUE_STATUS_WORDS,
  EMPTY_STATE_LINES,
  STATE_LINES,
  GAP_LINES,
  GAP_WORDS,
  PROGRESS_NOUNS,
  CHECK_WHEN_WORDS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  APPROVAL_STATE_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  MARGIN_SECTION_NAMES,
  BLOCK_ATTRIBUTE_LABELS,
  NODE_FACT_LABELS,
  AUTHOR_KIND_WORDS,
  FILE_WORDS,
];

const ANSWERS: readonly unknown[] = [ANSWER_LINES, BLOCKED_BY_REFUSALS, HELP_LINES, KEY_REPORTS];

const WORD_SOURCES: readonly unknown[] = [...UNASKED, ...ANSWERS];

test("the conversation's four states are named in the order the panel offers them", () => {
  expect(CONVERSATION_STATE_ORDER.map((state) => CONVERSATION_STATE_NAMES[state])).toEqual([
    "Everything",
    "Pinned",
    "Questions",
    "What people said",
  ]);
});

test("every issue status the board knows has a word, and it is the board's own", () => {
  for (const status of issueStatuses) {
    expect(Object.keys(ISSUE_STATUS_WORDS), `${status} has no word`).toContain(status);
    expect(issueStatusWord(status)).toBe(statusText(status));
  }
  expect(Object.keys(ISSUE_STATUS_WORDS).sort()).toEqual([...issueStatuses].sort());
  expect(issueStatusWord("something-the-server-added")).toBe("something-the-server-added");
});

test("every node type has a plain name, and no name is an internal token", () => {
  expect(NODE_TYPES.length).toBeGreaterThan(0);
  for (const type of NODE_TYPES) {
    const name = NODE_TYPE_NAMES[type];
    expect(name).toMatch(/^[a-z]+(?: [a-z]+)*$/);
    expect(name).not.toBe(name.toUpperCase());
    expect(name.includes("_")).toBe(false);
  }
});

test("every block type the schema declares has a name, and one it does not still reads", () => {
  for (const type of schema.types) {
    expect(Object.keys(NODE_TYPE_NAMES)).toContain(type.name);
    expect(nodeTypeName(type.name)).toBe(
      NODE_TYPE_NAMES[type.name as keyof typeof NODE_TYPE_NAMES]
    );
  }
  expect(Object.keys(NODE_TYPE_NAMES)).not.toContain("gantt_chart");
  expect(nodeTypeName("gantt_chart")).toBe("gantt chart");
});

test("a type the model gives no state has no state words, and the ask, the check and the decision have theirs", () => {
  for (const type of NODE_TYPES) {
    if (type === "ask" || type === "check" || type === "decision") {
      continue;
    }
    expect(nodeStateWords(type)).toBeUndefined();
  }
  // The ask's words are the block schema's `state` choices and the ways a resolution closes a
  // question (`ASK_RESOLUTION_WORDS`): an ask the server writes `resolved` stands in the state
  // its resolution names, so the row says what the card beside it says (`tree-drafts.ts`'s
  // `askState`). `resolved` is in both lists under the one word.
  expect(Object.keys(NODE_STATE_WORDS.ask).sort()).toEqual(
    [...new Set([...choicesOf("ask", "state"), ...Object.keys(ASK_RESOLUTION_WORDS)])].sort()
  );
  expect(Object.keys(NODE_STATE_WORDS.check).sort()).toEqual([
    "failed",
    "no-result",
    "older",
    "passed",
    "retracted",
    "unmarked",
    "unreadable",
  ]);
  // The three the old Decisions tab grouped by, which a decision's row now says in a word.
  expect(Object.keys(NODE_STATE_WORDS.decision).sort()).toEqual([
    "replaced",
    "waiting_review",
    "yours",
  ]);
  expect(nodeStateWord("no-result")).toBe("No result");
  expect(nodeStateWord("answered")).toBe("Answered");
  expect(nodeStateWord("waiting_review")).toBe("Waiting for your review");
});

// A row draws its node's state through `nodeStateWord`, which takes the state alone, so a
// state token two types share must draw the one word: `retracted` is a question its asker took
// back and a check whose results were all withdrawn, and both read "Retracted". Every state of
// every type is held to the one answer here: the day a type declares a token another already
// has under another word, this fails rather than a row drawing the other type's word.
test("every state of every type is drawn by the one word the page declares for it", () => {
  const byType: Readonly<Record<string, Readonly<Record<string, string>>>> = NODE_STATE_WORDS;
  for (const [type, words] of Object.entries(byType)) {
    for (const [state, word] of Object.entries(words)) {
      expect(nodeStateWord(state as NodeState), `${type}'s ${state}`).toBe(word);
    }
  }
});

test("no two types share a glyph token, and no two states of one surface share one", () => {
  const typeTokens = NODE_TYPES.map((type) => GLYPH_TOKENS.types[type]);
  expect(new Set(typeTokens).size).toBe(typeTokens.length);

  const askTokens = Object.values(GLYPH_TOKENS.states.ask);
  const checkTokens = Object.values(GLYPH_TOKENS.states.check);
  const decisionTokens = Object.values(GLYPH_TOKENS.states.decision);
  expect(new Set(askTokens).size).toBe(askTokens.length);
  expect(new Set(checkTokens).size).toBe(checkTokens.length);
  expect(new Set(decisionTokens).size).toBe(decisionTokens.length);
  expect(checkTokens.length).toBe(7);

  const everyToken = [
    ...typeTokens,
    ...askTokens,
    ...checkTokens,
    ...decisionTokens,
    ...Object.values(GLYPH_TOKENS.authors),
  ];
  expect(new Set(everyToken).size).toBe(everyToken.length);
});

test("every glyph token has an accessible name", () => {
  const tokens = [
    ...NODE_TYPES.map((type) => GLYPH_TOKENS.types[type]),
    ...Object.values(GLYPH_TOKENS.states.ask),
    ...Object.values(GLYPH_TOKENS.states.check),
    ...Object.values(GLYPH_TOKENS.states.decision),
    // One shape per kind of author, so a person and an agent never share one (R9).
    ...Object.values(GLYPH_TOKENS.authors),
  ];
  for (const token of tokens) {
    expect(GLYPH_NAMES[token]).toMatch(/\S/);
  }
  expect(Object.keys(GLYPH_NAMES).sort()).toEqual([...tokens].sort());
  expect(GLYPH_NAMES["check-passed"]).toBe("Passed check");
  expect(GLYPH_NAMES["decision-waiting-review"]).toBe("Waiting for your review decision");
  expect(GLYPH_NAMES["plan-task"]).toBe("plan task");
  expect(GLYPH_NAMES[GLYPH_TOKENS.authors.person]).toBe("Written by a person");
  expect(GLYPH_NAMES[GLYPH_TOKENS.authors.agent]).toBe("Written by an agent");
});

/** The sentences of `phrase`: each run up to a full stop, a question mark or an exclamation. */
function sentencesIn(phrase: string): readonly string[] {
  return phrase.split(/(?<=[.!?])\s+/u).filter((sentence) => /\p{L}/u.test(sentence));
}

// R22 and the architect's ruling on it: what the page draws unasked - a name, a state, a label,
// an empty line - is no sentence about the page or about Dispatch. The sentences it may draw
// unasked are the lines that say what a place, a node or the issue holds or lacks; every other
// full stop, and any of the words a sentence about the page or Dispatch is made of, fails here.
test("what the page draws unasked holds no sentence about the page or about Dispatch", () => {
  expect([...UNASKED_SOURCES]).toEqual([...UNASKED]);
  const lines = new Set([EMPTY_STATE_LINES, GAP_LINES, STATE_LINES].flatMap(phrasesOf));
  const offenders: string[] = [];
  for (const phrase of UNASKED.flatMap(phrasesOf)) {
    const words = wordsIn(phrase);
    if (words.includes("dispatch") || words.includes("press")) {
      offenders.push(`"${phrase}" names Dispatch or a press`);
    }
    if (!phrase.trimEnd().endsWith(".")) continue;
    if (!lines.has(phrase)) offenders.push(`"${phrase}" is a sentence and no empty or state line`);
    if (/\bthe page\b/iu.test(phrase) || words.includes("row")) {
      offenders.push(`"${phrase}" is a sentence about the page`);
    }
  }
  expect(offenders).toEqual([]);
  expect(CONTROL_LABELS.deeper_row).toBe("more row below");
  expect(CONTROL_LABELS.deeper_rows).toBe("more rows below");
});

// What the page says in answer to the reader's own act - a refusal, a key's report, a failed
// read, a write's line, the description of a box they opened - is its own table, which a browser
// check reads a freshly loaded page for. Each holds at most one sentence and no schema token.
test("every answer to the reader's own act is one sentence with no schema token", () => {
  expect([...ANSWER_SOURCES]).toEqual([...ANSWERS]);
  expect([...ANSWER_PHRASES]).toEqual(ANSWERS.flatMap(phrasesOf));
  for (const answer of [ANSWER_LINES, HELP_LINES, KEY_REPORTS]) {
    for (const line of Object.values(answer)) {
      expect(sentencesIn(line).length, line).toBeLessThanOrEqual(1);
      expect(line, line).not.toMatch(/_|#b-|dispatch:\/\//u);
    }
  }
  // A refusal is cut where the issue it is about goes, so it is read whole, with a key in place.
  for (const { after, before } of Object.values(BLOCKED_BY_REFUSALS)) {
    const whole = `${before}CORE-9${after}`;
    expect(sentencesIn(whole).length, whole).toBeLessThanOrEqual(1);
    expect(whole, whole).not.toMatch(/_|#b-|dispatch:\/\//u);
  }
  // A table holds what it is for and nothing the page says unasked.
  const unasked = new Set(UNASKED.flatMap(phrasesOf));
  expect(
    ANSWERS.flatMap(phrasesOf).filter((phrase) => phrase.length > 3 && unasked.has(phrase))
  ).toEqual([]);
});

test("the five presets are named in the order the row draws them", () => {
  expect(PRESET_ORDER.map((preset) => PRESET_NAMES[preset])).toEqual([
    "Tree",
    "Decisions",
    "Checks",
    "Spec",
    "Plan",
  ]);
  for (const preset of PRESET_ORDER) {
    expect(EMPTY_STATE_LINES[preset]).toMatch(/^[A-Z].*\.$/);
  }
});

test("every empty-state line is one sentence a person reads", () => {
  for (const line of Object.values(EMPTY_STATE_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
  expect(EMPTY_STATE_LINES.spec_summary).toBe("This spec has no summary.");
});

test("every key report is one sentence saying where the key could not go", () => {
  for (const line of Object.values(KEY_REPORTS)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
  expect(KEY_REPORTS.no_next_question).toBe("No further open question.");
});

test("every gap line is one sentence that says what is absent", () => {
  for (const line of Object.values(GAP_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
  expect(GAP_LINES.no_check).toBe("No check serves this requirement.");
});

// A node is a row to the reader, the word the tree's own keys already use, and "node" is the
// model's word: no sentence the page draws says it, and no count reads "1 rows".
test("no word the page declares is the model's word for a node", () => {
  expect(DECLARED_WORDS).not.toContain("node");
  expect(DECLARED_WORDS).not.toContain("nodes");
  expect([CONTROL_LABELS.folded_row, CONTROL_LABELS.deeper_row].join(" ")).not.toMatch(/\brows\b/);
  expect([CONTROL_LABELS.folded_rows, CONTROL_LABELS.deeper_rows].join(" ")).not.toMatch(/\brow\b/);
});

// A state is told apart by its word, so no two states of a check share one, and no other word
// the page draws reads as one of them: a decision's chip once said "Out of date" of a block the
// decision changed after naming, which is not what the check state means. A result that no
// longer speaks for the work is the one other place that word is drawn, because it is the
// same fact: the check reads that state when its newest result reads so.
test("every check state has a word of its own, and the page says it of nothing else", () => {
  const checkWords = Object.values(NODE_STATE_WORDS.check);
  expect(new Set(checkWords).size).toBe(checkWords.length);
  for (const word of [...Object.values(CONTROL_LABELS), ...Object.values(GAP_WORDS)]) {
    expect(checkWords, `"${word}" reads as a check state`).not.toContain(word);
  }
  expect(RESULT_STANDS_WORDS.other_commit).toBe(NODE_STATE_WORDS.check.older);
  expect(checkWords).not.toContain(RESULT_STANDS_WORDS.at_basis);
});

test("every gap a row draws has a short word, one per gap the page names", () => {
  expect(Object.keys(GAP_WORDS).sort()).toEqual(Object.keys(GAP_LINES).sort());
  for (const [kind, word] of Object.entries(GAP_WORDS)) {
    expect(word, `the gap ${kind} is named with an internal token`).toMatch(/^[A-Z][^_.]*$/);
  }
});

test("every property has a plain name, and no name is an internal token", () => {
  for (const [field, label] of Object.entries(PROPERTY_LABELS)) {
    expect(label, `${field} is named with an internal token`).toMatch(/^[A-Z][^_]*$/);
  }
  // The two the model names differently from the page: a reader is told what a route does to
  // their messages, and whose turn it is, in words rather than in the field's own name.
  expect(PROPERTY_LABELS.route).toBe("Messages go to");
  expect(PROPERTY_LABELS.blocked_by).toBe("Blocked by");
});

test("the panel draws three named sections, in the order it names them", () => {
  expect(MARGIN_SECTION_ORDER.map((section) => MARGIN_SECTION_NAMES[section])).toEqual([
    "Development",
    "People",
    "References",
  ]);
});

test("every help line describes its box in one sentence", () => {
  for (const line of Object.values(HELP_LINES)) {
    expect(line).toMatch(/^[A-Z][^_]*\.$/);
  }
});

test("every `when` the block schema declares reads as words", () => {
  expect(Object.keys(CHECK_WHEN_WORDS).sort()).toEqual([...choicesOf("check", "when")].sort());
  expect(CHECK_WHEN_WORDS.before_merge).toBe("before the merge");
  for (const words of Object.values(CHECK_WHEN_WORDS)) {
    expect(words.includes("_")).toBe(false);
  }
});

test("every block attribute the schema declares has a label, and one it does not still reads", () => {
  for (const type of schema.types) {
    for (const attribute of Object.keys(type.attributes)) {
      expect(Object.keys(BLOCK_ATTRIBUTE_LABELS)).toContain(attribute);
      expect(blockAttributeLabel(attribute).includes("_")).toBe(false);
    }
  }
  expect(Object.keys(BLOCK_ATTRIBUTE_LABELS)).not.toContain("owner_of_record");
  expect(blockAttributeLabel("owner_of_record")).toBe("owner of record");
});

test("every fact a node's body draws has a plain label, and a block attribute keeps its own", () => {
  for (const [name, label] of Object.entries(NODE_FACT_LABELS)) {
    expect(label, `the fact ${name} is named with an internal token`).toMatch(/^[a-z][^_]*$/);
  }
  expect(nodeFactLabel("stands")).toBe("how it stands");
  expect(nodeFactLabel("negative_control")).toBe(BLOCK_ATTRIBUTE_LABELS.negative_control);
  expect(nodeFactLabel("owner_of_record")).toBe("owner of record");
});

test("a result's verdict reads as a word", () => {
  expect(Object.keys(RESULT_VERDICT_WORDS).sort()).toEqual(["fail", "pass"]);
  for (const word of Object.values(RESULT_VERDICT_WORDS)) {
    expect(word).toMatch(/^[A-Z][^_]*$/);
  }
});

test("DECLARED_WORDS holds every declared word and no other", () => {
  const derived = new Set(
    WORD_SOURCES.flatMap(phrasesOf).flatMap((phrase) => [...wordsIn(phrase)])
  );
  expect([...derived].sort()).toEqual([...DECLARED_WORDS]);

  for (const phrase of WORD_SOURCES.flatMap(phrasesOf)) {
    for (const word of wordsIn(phrase)) {
      expect(DECLARED_WORDS).toContain(word);
    }
  }
  for (const word of DECLARED_WORDS) {
    expect(WORD_SOURCES.flatMap(phrasesOf).some((phrase) => wordsIn(phrase).includes(word))).toBe(
      true
    );
  }

  expect(DECLARED_WORDS).toContain("merge");
  expect(DECLARED_WORDS).not.toContain("verification");
  expect(DECLARED_WORDS).not.toContain("before_merge");
});
