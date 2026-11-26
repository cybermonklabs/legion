import { expect, test } from "bun:test";
import { readdirSync } from "node:fs";
import type { Actor, AskResolution } from "../../api/types";
import * as askBodyWords from "../ask-body/ask-body-words";
import * as decisionCardWords from "../decision-card/decision-card-words";
import { compiledBlockSchema } from "../doc/schema";
import { ASK_RESOLUTION_WORDS, askResolutionBadge } from "../refs/actor";
import * as pageStructure from "./page-structure";
import {
  freeze,
  HELP_LINES,
  KEY_REPORTS,
  MARGIN_SECTION_NAMES,
  MARGIN_SECTION_ORDER,
  PROPERTY_LABELS,
} from "./page-structure";
import * as pageWords from "./page-words";
import {
  ANSWER_LINES,
  ANSWER_PHRASES,
  ANSWER_SOURCES,
  APPROVAL_STATE_WORDS,
  AUTHOR_KIND_WORDS,
  BLOCK_ATTRIBUTE_LABELS,
  BLOCK_TYPE_NAMES,
  BLOCKED_BY_REFUSALS,
  blockAttributeLabel,
  CHECK_WHEN_WORDS,
  CONTENTS_MARK_WORDS,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  DECLARED_WORDS,
  DELIVERY_STATE_WORDS,
  EMPTY_STATE_LINES,
  FILE_WORDS,
  FILTER_SIGNAL_WORDS,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  ISSUE_STATUS_WORDS,
  isReader,
  NODE_FACT_LABELS,
  NODE_REFERENCES_WORDS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  type NodeState,
  nodeFactLabel,
  nodeStateWord,
  nodeStateWords,
  nodeTypeName,
  PLAN_WORDS,
  PRESET_NAMES,
  PRESET_ORDER,
  PROGRESS_NOUNS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  SPEC_WORDS,
  STATE_LINES,
  UNASKED_SOURCES,
  VERIFICATION_WORDS,
  wordsIn,
} from "./page-words";
import {
  CLOSED_TABLE_NAMES,
  JOINED_TABLES,
  joinTables,
  type PartWords,
  TABLE_NAMES,
  type TableName,
  WORD_CLASHES,
  WORD_FILES,
  type WordClash,
  type WordFile,
} from "./page-words-parts";

/** The block schema the server owns, read here rather than repeated, so a type, an attribute or
 *  a `when` choice added to it fails this file until the page has a word for it. */
const schema = compiledBlockSchema;

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
  BLOCK_TYPE_NAMES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  FILTER_SIGNAL_WORDS,
  CONTENTS_MARK_WORDS,
  ISSUE_STATUS_WORDS,
  EMPTY_STATE_LINES,
  STATE_LINES,
  GAP_LINES,
  GAP_WORDS,
  PROGRESS_NOUNS,
  CHECK_WHEN_WORDS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  DELIVERY_STATE_WORDS,
  APPROVAL_STATE_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  MARGIN_SECTION_NAMES,
  BLOCK_ATTRIBUTE_LABELS,
  NODE_FACT_LABELS,
  AUTHOR_KIND_WORDS,
  FILE_WORDS,
  SPEC_WORDS,
  DECISIONS_WORDS,
  VERIFICATION_WORDS,
  PLAN_WORDS,
  NODE_REFERENCES_WORDS,
  ...WORD_FILES.flatMap(({ module }) => module.default.unasked),
];

const ANSWERS: readonly unknown[] = [
  ANSWER_LINES,
  BLOCKED_BY_REFUSALS,
  HELP_LINES,
  KEY_REPORTS,
  ...WORD_FILES.flatMap(({ module }) => module.default.answers),
];

const WORD_SOURCES: readonly unknown[] = [...UNASKED, ...ANSWERS];

/** Each export of page-words.ts, by its name. The rows of the join read it. */
const exported = new Map<string, unknown>(Object.entries(pageWords));

test("every node type has a plain name, and no name is an internal token", () => {
  expect(NODE_TYPES.length).toBeGreaterThan(0);
  for (const type of NODE_TYPES) {
    const name = NODE_TYPE_NAMES[type];
    expect(name).toMatch(/^[a-z]+(?: [a-z]+)*$/);
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
  // Spec R2's three, "wherever a decision is drawn", and the change a reader asked for.
  expect(Object.keys(NODE_STATE_WORDS.decision).sort()).toEqual([
    "change_requested",
    "kept",
    "replaced",
    "waiting_review",
  ]);
  expect(nodeStateWord("no-result")).toBe("No result");
  expect(nodeStateWord("answered")).toBe("Answered");
  expect(nodeStateWord("waiting_review")).toBe("Waiting for review");
  expect(nodeStateWord("kept")).toBe("Kept by");
  expect(nodeStateWord("replaced")).toBe("Replaced");
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

  const statementTokens = [...Object.values(GLYPH_TOKENS.statements), GLYPH_TOKENS.no_check];
  expect(new Set(statementTokens).size).toBe(statementTokens.length);

  const everyToken = [
    ...typeTokens,
    ...askTokens,
    ...checkTokens,
    ...decisionTokens,
    ...statementTokens,
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
    // How a statement stands, and that no check serves it (R17).
    ...Object.values(GLYPH_TOKENS.statements),
    GLYPH_TOKENS.no_check,
    // One shape per kind of author, so a person and an agent never share one (R9).
    ...Object.values(GLYPH_TOKENS.authors),
  ];
  for (const token of tokens) {
    expect(GLYPH_NAMES[token]).toMatch(/\S/);
  }
  expect(Object.keys(GLYPH_NAMES).sort()).toEqual([...tokens].sort());
  // A state's shape is named by its type and then its state, the state in lower case.
  expect(GLYPH_NAMES["check-passed"]).toBe("Check: passed");
  expect(GLYPH_NAMES["check-no-result"]).toBe("Check: no result");
  expect(GLYPH_NAMES["check-older"]).toBe("Check: out of date");
  expect(GLYPH_NAMES["check-unmarked"]).toBe("Check: cannot tell");
  expect(GLYPH_NAMES["check-unreadable"]).toBe("Check: could not read");
  expect(GLYPH_NAMES["decision-waiting-review"]).toBe("Decision: waiting for review");
  expect(GLYPH_NAMES["decision-change-requested"]).toBe("Decision: change requested");
  expect(GLYPH_NAMES["ask-declined"]).toBe("Question: declined");
  // "Kept by" is followed by a name on the row; its shape is named without the word that
  // leads to it.
  expect(GLYPH_NAMES["decision-kept"]).toBe("Decision: kept");
  expect(GLYPH_NAMES[GLYPH_TOKENS.statements.met]).toBe(VERIFICATION_WORDS.states.met);
  expect(GLYPH_NAMES[GLYPH_TOKENS.no_check]).toBe(GAP_WORDS.no_check);
  expect(GLYPH_NAMES["plan-task"]).toBe("plan task");
  expect(GLYPH_NAMES[GLYPH_TOKENS.authors.person]).toBe("Written by a person");
  expect(GLYPH_NAMES[GLYPH_TOKENS.authors.agent]).toBe("Written by an agent");
});

/** A line for each key of `table` that `named` does not hold, and for each name of `named` that
 *  `table` does not hold as a key. `path` is the path of `table`, and each line starts with the
 *  path of its key. */
function keyOffenders(path: string, table: object, named: readonly string[]): readonly string[] {
  const keys = new Set(Object.keys(table));
  const names = new Set(named);
  return [
    ...[...keys]
      .filter((key) => !names.has(key))
      .map((key) => `${path}.${key} is a key the shape does not name`),
    ...[...names].filter((name) => !keys.has(name)).map((name) => `${path}.${name} is missing`),
  ];
}

// The `satisfies` of `GLYPH_TOKENS` reads the joined table. It checks that each key is there.
// It does not check that no other key is there. This case refuses an extra key, and also an
// extra key that a part's file adds.
test("every glyph token key is one the page's own tables name, and no other", () => {
  const states: Readonly<Partial<Record<string, object>>> = GLYPH_TOKENS.states;
  const stateWords: Readonly<Record<string, object>> = NODE_STATE_WORDS;
  const offenders = [
    ...keyOffenders("GLYPH_TOKENS", GLYPH_TOKENS, [
      "authors",
      "no_check",
      "states",
      "statements",
      "types",
    ]),
    ...keyOffenders("GLYPH_TOKENS.types", GLYPH_TOKENS.types, NODE_TYPES),
    ...keyOffenders("GLYPH_TOKENS.authors", GLYPH_TOKENS.authors, Object.keys(AUTHOR_KIND_WORDS)),
    ...keyOffenders(
      "GLYPH_TOKENS.statements",
      GLYPH_TOKENS.statements,
      Object.keys(VERIFICATION_WORDS.states)
    ),
    ...keyOffenders("GLYPH_TOKENS.states", states, Object.keys(stateWords)),
    // A type that `states` does not hold is missing in the line above. Its states add no line.
    ...Object.entries(stateWords).flatMap(([type, words]) => {
      const tokens = states[type];
      return tokens === undefined
        ? []
        : keyOffenders(`GLYPH_TOKENS.states.${type}`, tokens, Object.keys(words));
    }),
  ];
  expect(offenders).toEqual([]);
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

test("the six controls are named in the order the row draws them, and none is called Tree", () => {
  expect(PRESET_ORDER.map((preset) => PRESET_NAMES[preset])).toEqual([
    "Dashboard",
    "Conversation",
    "Spec",
    "Verification",
    "Plan",
    "Decisions",
  ]);
  for (const preset of PRESET_ORDER) {
    expect(EMPTY_STATE_LINES[preset]).toMatch(/^[A-Z].*\.$/);
  }
  // The page draws Statement, never Requirement (R22): a requirement of the issue's spec is a
  // statement, and no word the page declares says requirement. One entry is let through by name,
  // `BLOCK_TYPE_NAMES.requirement`: the tag of a requirement block in another document, such as
  // the plan. That block is not a statement, so its tag calls it what it is (decision 246).
  expect(NODE_TYPE_NAMES.requirement).toBe("statement");
  expect(BLOCK_TYPE_NAMES.requirement).toBe("requirement");
  for (const word of [...Object.values(PRESET_NAMES), ...Object.values(FILTER_SIGNAL_WORDS)]) {
    expect(word).not.toMatch(/tree|requirement|proven/iu);
  }
  const { requirement: _letThrough, ...otherBlockNames } = BLOCK_TYPE_NAMES;
  expect(
    WORD_SOURCES.map((table) => (table === BLOCK_TYPE_NAMES ? otherBlockNames : table))
      .flatMap(phrasesOf)
      .filter((phrase) => /requirement/iu.test(phrase)),
    "a declared phrase other than BLOCK_TYPE_NAMES.requirement says requirement"
  ).toEqual([]);
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
  expect(GAP_LINES.no_check).toBe("No check serves this statement.");
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
  // Every phrase the page draws unasked, but for two tables that say a check state's word by
  // design: the states' own, whose ask table holds "Retracted" for a question its asker took
  // back, and a result's standing, which says "Out of date" of the same fact (held below).
  const elsewhere = UNASKED_SOURCES.filter(
    (table) => table !== NODE_STATE_WORDS && table !== RESULT_STANDS_WORDS
  ).flatMap(phrasesOf);
  for (const word of elsewhere) {
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
  const derived = new Set(WORD_SOURCES.flatMap(phrasesOf).flatMap(wordsIn));
  expect([...derived].sort()).toEqual([...DECLARED_WORDS]);

  expect(DECLARED_WORDS).toContain("merge");
  // Verification is the filter's name, his word for what draws the checks (R2, the Design's
  // "The words"), and no longer a word the page holds back.
  expect(DECLARED_WORDS).toContain("verification");

  // The ask body's list and the decision card's are taken in whole (R13): every word of each of
  // their tables is a declared word, though Dashboard's part file splits a table between what the
  // page draws on load and what it says only in answer to the reader's act.
  const lists = [...Object.values(askBodyWords), ...Object.values(decisionCardWords)];
  expect(
    lists
      .flatMap(phrasesOf)
      .flatMap(wordsIn)
      .filter((word) => !DECLARED_WORDS.includes(word))
  ).toEqual([]);
});

// An internal word is how the model or the code spells a thing, not how a person says it: a
// snake_case token, a kebab-case slug, a camelCase name, a block's anchor or an address.
const INTERNAL_WORD = /_|[\p{L}\p{N}]-[\p{L}\p{N}]|\p{Ll}\p{Lu}|#b-|:\/\//u;

// Each table is named by the export of the two modules that is that table, so the test holds
// no list of its own: a table added to either list is read by its own name, and one no export
// names fails here until it is exported.
test("no phrase of the list holds an internal word", () => {
  const names = new Map<unknown, string>();
  for (const [name, value] of [...Object.entries(pageWords), ...Object.entries(pageStructure)]) {
    names.set(value, name);
  }
  const offenders: string[] = [];
  for (const [list, tables] of Object.entries({ UNASKED_SOURCES, ANSWER_SOURCES })) {
    for (const [index, table] of tables.entries()) {
      let name = names.get(table);
      if (name === undefined) {
        name = `${list}[${index}]`;
        offenders.push(`an unnamed table stands at index ${index} of ${list}`);
      }
      for (const phrase of phrasesOf(table)) {
        if (INTERNAL_WORD.test(phrase)) offenders.push(`${name}: ${phrase}`);
      }
    }
  }
  expect(offenders).toEqual([]);
});

// The card draws a closed question's badge through `askResolutionBadge`, and the list reads
// `ASK_RESOLUTION_WORDS` for the row's state, so the two say the one word of each way.
test("each way a question is closed has one word on its card and in the list", () => {
  expect(Object.keys(ASK_RESOLUTION_WORDS).sort()).toEqual([
    "declined",
    "resolved",
    "retracted",
    "withdrawn",
  ]);
  for (const [kind, word] of Object.entries(ASK_RESOLUTION_WORDS)) {
    const resolution: AskResolution = {
      actor: { id: "alice", kind: "user" },
      at: "2026-10-25T00:00:00Z",
      kind,
      reason: "",
    };
    expect(askResolutionBadge(resolution), kind).toBe(word);
    expect(nodeStateWord(kind as NodeState), kind).toBe(word);
  }
});

// The server matches a login in any casing, as `isViewer` does, so a person signed in as `alice`
// whose actor says `Alice` is still the reader; a session acting for them is not.
test("the reader's own words are theirs whatever the case of the login", () => {
  const alice: Actor = { id: "Alice", kind: "user" };
  expect(isReader(alice, "alice")).toBe(true);
  expect(isReader({ ...alice, act: { session: "planner" } }, "alice")).toBe(false);
});

/** `value` as nested lists of its entries. `toEqual` then reads the order of each object's keys,
 *  and it reads a key whose value is `undefined`. */
function entriesOf(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(entriesOf);
  }
  if (typeof value === "object" && value !== null) {
    return Object.entries(value).map(([key, held]) => [key, entriesOf(held)]);
  }
  return value;
}

/** The path of each object in `value` that is not frozen. `value` itself is one of the objects. */
function unfrozenIn(value: unknown, path: string): readonly string[] {
  if (typeof value !== "object" || value === null) {
    return [];
  }
  return [
    ...(Object.isFrozen(value) ? [] : [path]),
    ...Object.entries(value).flatMap(([key, held]) => unfrozenIn(held, `${path}.${key}`)),
  ];
}

/** A made-up file of a part, in the shape that `WORD_FILES` holds. It adds `add` and declares no
 *  table of its own. */
function partFile<const Add extends PartWords["add"]>(letter: string, add: Add) {
  return freeze({
    file: `page-words-${letter}.ts`,
    module: { default: { add, unasked: [], answers: [] } },
  } as const);
}

// The join reads each table that page-words.ts declares under the table's own name. A part's own
// table is read through page-words.ts too, and a case below holds it.
test("every word table page-words.ts declares joins the parts' files, under its own name", () => {
  // The two objects that page-words.ts makes from other words, each with how it makes it. They
  // are no tables of words of its own, so no part adds to them.
  const made: Readonly<Record<string, string>> = {
    GLYPH_NAMES: "`glyphNames()` makes it from other tables",
    ISSUE_STATUS_WORDS: "it is made from the board's statuses (`features/project/board-model.ts`)",
  };
  const partExports = new Set(
    WORD_FILES.flatMap(({ module }) => Object.keys(module).filter((name) => name !== "default"))
  );
  const members = [...exported]
    .filter(
      ([name, value]) =>
        typeof value === "object" &&
        value !== null &&
        !Array.isArray(value) &&
        !Object.hasOwn(made, name) &&
        !partExports.has(name)
    )
    .map(([name]) => name)
    .sort();
  expect(members).toEqual([...TABLE_NAMES].sort());
  expect([...JOINED_TABLES.keys()].sort()).toEqual([...TABLE_NAMES].sort());
  // The name that a `joinWords` call gives is the name of the const that holds its table.
  const misnamed = TABLE_NAMES.filter((name) => exported.get(name) !== JOINED_TABLES.get(name));
  expect(misnamed, "page-words.ts exports another table under each of these names").toEqual([]);
});

/** One case of the join: a table's name and its own words, the files that add to it, and the
 *  table and the clashes that the join gives. */
interface JoinRow {
  readonly title: string;
  readonly name: TableName;
  readonly main: object;
  readonly files: readonly WordFile[];
  readonly table: object;
  readonly clashes: readonly WordClash[];
}

test("the join adds each file's keys after the table's own, nested as the table is, and names each key two files give with both files", () => {
  const rows: readonly JoinRow[] = [
    {
      title:
        "a key that a part adds comes after the table's own keys, and a part that adds to another table adds nothing",
      name: "CONTROL_LABELS",
      main: freeze({ accept: "Accept", retry: "Retry" }),
      files: [
        partFile("j", { CONTROL_LABELS: { about: "About" } }),
        partFile("l", { GAP_WORDS: { no_owner: "No owner" } }),
      ],
      table: { accept: "Accept", retry: "Retry", about: "About" },
      clashes: [],
    },
    {
      title: "a nested key goes into the table's own sub-table",
      name: "NODE_REFERENCES_WORDS",
      main: freeze({ edges: { cites: "cites" }, heading: "Referenced by" }),
      files: [
        partFile("j", {
          NODE_REFERENCES_WORDS: { edges: { constrained_by: "constrained statement" } },
        }),
      ],
      table: {
        edges: { cites: "cites", constrained_by: "constrained statement" },
        heading: "Referenced by",
      },
      clashes: [],
    },
    {
      title: "two parts add different keys to one sub-table, and both keys stay with no clash",
      name: "NODE_REFERENCES_WORDS",
      main: freeze({ edges: { cites: "cites" } }),
      files: [
        partFile("j", {
          NODE_REFERENCES_WORDS: { edges: { constrained_by: "constrained statement" } },
        }),
        partFile("m", { NODE_REFERENCES_WORDS: { edges: { served_by: "served by" } } }),
      ],
      table: {
        edges: { cites: "cites", constrained_by: "constrained statement", served_by: "served by" },
      },
      clashes: [],
    },
    {
      title: "page-words.ts and part J both give `retry`, and the table's own word stays",
      name: "CONTROL_LABELS",
      main: freeze({ retry: "Retry" }),
      files: [partFile("j", { CONTROL_LABELS: { retry: "Try again" } })],
      table: { retry: "Retry" },
      clashes: [{ key: "CONTROL_LABELS.retry", files: ["page-words.ts", "page-words-j.ts"] }],
    },
    {
      title: "parts J and M both give one nested key, and part J's word stays",
      name: "NODE_REFERENCES_WORDS",
      main: freeze({ edges: { cites: "cites" } }),
      files: [
        partFile("j", { NODE_REFERENCES_WORDS: { edges: { served_by: "served by" } } }),
        partFile("m", { NODE_REFERENCES_WORDS: { edges: { served_by: "is served by" } } }),
      ],
      table: { edges: { cites: "cites", served_by: "served by" } },
      clashes: [
        {
          key: "NODE_REFERENCES_WORDS.edges.served_by",
          files: ["page-words-j.ts", "page-words-m.ts"],
        },
      ],
    },
    {
      title: "a key of the table whose value is undefined stays",
      name: "NODE_FACT_LABELS",
      main: freeze({ answer: "answer", unknown: undefined }),
      files: [partFile("s", { NODE_FACT_LABELS: { superseded: "Superseded" } })],
      table: { answer: "answer", unknown: undefined, superseded: "Superseded" },
      clashes: [],
    },
    {
      title:
        "a merge two levels down leaves each frozen input as it was, and the table is frozen at every level",
      name: "GLYPH_TOKENS",
      main: freeze({ states: { check: { failed: "check-failed" } }, types: { ask: "type-ask" } }),
      files: [
        partFile("m", { GLYPH_TOKENS: { states: { check: { stale: "check-stale" } } } }),
        partFile("z", { GLYPH_TOKENS: { states: { lens: { open: "lens-open" } } } }),
      ],
      table: {
        states: {
          check: { failed: "check-failed", stale: "check-stale" },
          lens: { open: "lens-open" },
        },
        types: { ask: "type-ask" },
      },
      clashes: [],
    },
  ];
  for (const row of rows) {
    const inputs = entriesOf([row.main, row.files]);
    const { table, clashes } = joinTables(row.name, row.main, row.files);
    expect(entriesOf(table), row.title).toEqual(entriesOf(row.table));
    expect(clashes, row.title).toEqual(row.clashes);
    expect(unfrozenIn(table, row.name), `${row.title}: these objects are not frozen`).toEqual([]);
    expect(entriesOf([row.main, row.files]), `${row.title}: the join changed its inputs`).toEqual(
      inputs
    );
  }

  // The type check reads the cases below. The type of a joined table is the table's own type
  // with the type of each file's words. So a component reads a key that a part adds as its
  // literal, and a key that no file gives is an error.
  const joined = joinTables(
    "CONTROL_LABELS",
    freeze({ accept: "Accept", sub: { first: "First" } } as const),
    [
      partFile("j", { CONTROL_LABELS: { b: "B" } }),
      partFile("m", { CONTROL_LABELS: { sub: { second: "Second" } } }),
    ] as const
  ).table;
  const added: "B" = joined.b;
  const nested: "Second" = joined.sub.second;
  expect([joined.accept, joined.sub.first, added, nested]).toEqual([
    "Accept",
    "First",
    "B",
    "Second",
  ]);
  // @ts-expect-error No file gives `missing`, so the joined table has no such key.
  expect(joined.missing).toBeUndefined();

  // A part that adds a node type adds its glyph token too. The joined table then satisfies a
  // check over both types, as the `satisfies` of `GLYPH_TOKENS` in page-words.ts reads it.
  const lensFile = partFile("w", {
    GLYPH_TOKENS: { types: { lens: "type-lens" } },
    NODE_TYPE_NAMES: { lens: "lens" },
  });
  const typeNames = joinTables("NODE_TYPE_NAMES", freeze({ ask: "question" } as const), [
    lensFile,
  ] as const).table;
  const glyphTokens = joinTables("GLYPH_TOKENS", freeze({ types: { ask: "type-ask" } } as const), [
    lensFile,
  ] as const).table satisfies { types: Record<"ask" | "lens", string> };
  const lensName: "lens" = typeNames.lens;
  expect([lensName, glyphTokens.types.lens]).toEqual(["lens", "type-lens"]);

  // `add` names only the tables of `TABLE_NAMES`. A table that a part's file declares is not one
  // of them, so a new key for that table goes in that part's file.
  const acceptsPartWords = (_words: PartWords): void => {};
  acceptsPartWords({
    // @ts-expect-error `LENS_WORDS` is not a table of `TABLE_NAMES`.
    add: { LENS_WORDS: { title: "Lens" } },
    unasked: [],
    answers: [],
  });

  // The tables of `CLOSED_TABLE_NAMES` take keys only in page-words.ts, and the type of `add`
  // refuses them.
  acceptsPartWords({
    add: {
      // @ts-expect-error `PRESET_NAMES` takes keys only in page-words.ts.
      PRESET_NAMES: { bogus: "Bogus" },
    },
    unasked: [],
    answers: [],
  });
  acceptsPartWords({
    add: {
      // @ts-expect-error `CHECK_WHEN_WORDS` takes keys only in page-words.ts.
      CHECK_WHEN_WORDS: { bogus: "Bogus" },
    },
    unasked: [],
    answers: [],
  });
  acceptsPartWords({
    add: {
      // @ts-expect-error `RESULT_STANDS_WORDS` takes keys only in page-words.ts.
      RESULT_STANDS_WORDS: { bogus: "Bogus" },
    },
    unasked: [],
    answers: [],
  });
  acceptsPartWords({
    add: {
      // @ts-expect-error `DELIVERY_STATE_WORDS` takes keys only in page-words.ts.
      DELIVERY_STATE_WORDS: { bogus: "Bogus" },
    },
    unasked: [],
    answers: [],
  });
  acceptsPartWords({
    add: {
      // @ts-expect-error `AUTHOR_KIND_WORDS` takes keys only in page-words.ts.
      AUTHOR_KIND_WORDS: { bogus: "Bogus" },
    },
    unasked: [],
    answers: [],
  });

  // The type of `add` refuses words for a closed table where `add` has a literal type, with or
  // without `satisfies PartWords` (`PartWords.add` says so). Here a part's default export has no
  // `satisfies PartWords`, and `add` also names an open table. Then the default export goes into a
  // `WordFile`, as in `WORD_FILES`.
  const noSatisfies = {
    add: { CONTROL_LABELS: {}, PRESET_NAMES: { bogus: "Bogus" } },
    unasked: [],
    answers: [],
  } as const;
  const acceptsWordFile = (_file: WordFile): void => {};
  acceptsWordFile({
    file: "page-words-q.ts",
    // @ts-expect-error `PRESET_NAMES` takes keys only in page-words.ts.
    module: { default: noSatisfies },
  });
  // Here `add` is a const of its own, beside an open table's key.
  const closedAdd = { CONTROL_LABELS: {}, PRESET_NAMES: { bogus: "Bogus" } } as const;
  acceptsPartWords({
    // @ts-expect-error `PRESET_NAMES` takes keys only in page-words.ts.
    add: closedAdd,
    unasked: [],
    answers: [],
  });
});

// A part's file that the join does not read fails here. So does a part that has no file. The
// open parts are lettered, and Dashboard's part (dispatch://INFRA-5702) is named.
test("each open part of the page has its own words file, which the join reads", () => {
  const parts = ["J", "L", "M", "N", "S", "T", "U", "W", "X", "Y", "Z", "Dashboard"];
  const expected = parts.map((part) => `page-words-${part.toLowerCase()}.ts`);
  const read: readonly string[] = WORD_FILES.map(({ file }) => file);
  expect(read).toEqual(expected);
  const inDirectory = readdirSync(import.meta.dir).filter(
    (name) =>
      name.startsWith("page-words-") &&
      name.endsWith(".ts") &&
      name !== "page-words-parts.ts" &&
      !name.endsWith(".test.ts")
  );
  expect(inDirectory.sort()).toEqual([...expected].sort());
});

// Two files that give one key is the refusal: each clash names the key and both files.
test("no key is given by two of the files the page's words are joined from", () => {
  const clashes = WORD_CLASHES.map(
    ({ key, files: [first, second] }) => `${key}: ${first} and ${second}`
  );
  expect(clashes).toEqual([]);
});

// A part's file ends its default export with `satisfies PartWords`. Then the type of `add`
// refuses a key that names no table. Without the `satisfies`, TypeScript still misses such a key
// if `add` also has a key for a table that takes a part's keys. The type of `add` refuses words
// for a table of `CLOSED_TABLE_NAMES` only where `add` has a literal type (`PartWords.add` says
// so): an `add` with a wide type passes it. This case reads `add` as the join reads it. Its
// closed-table branch is the only guard for an `add` with a wide type, so keep it while the type
// check lets one through.
test("each key a part's file adds under add names a table that takes a part's keys", () => {
  const tables: readonly string[] = TABLE_NAMES;
  const closed: readonly string[] = CLOSED_TABLE_NAMES;
  const offenders: string[] = [];
  for (const { file, module } of WORD_FILES) {
    const add: Readonly<Record<string, unknown>> = module.default.add;
    for (const name of Object.keys(add)) {
      if (!tables.includes(name)) {
        offenders.push(`${file}: add.${name} names no table of page-words.ts`);
      } else if (closed.includes(name)) {
        offenders.push(`${file}: add.${name} names a table that takes keys only in page-words.ts`);
      }
    }
  }
  expect(offenders).toEqual([]);
});

// A part's own table is a named export of its file, and page-words.ts exports it again. A table
// of page-words.ts with the same name hides it, and so does a second part's export of that name.
test("each table a part's file declares is read through page-words.ts, and is frozen", () => {
  const offenders: string[] = [];
  for (const { file, module } of WORD_FILES) {
    for (const [name, value] of Object.entries(module)) {
      if (name === "default") continue;
      if (exported.get(name) !== value) {
        offenders.push(`${name} of ${file} is not what page-words.ts exports under that name`);
      }
      for (const path of unfrozenIn(value, name)) {
        offenders.push(`${path} of ${file} is not frozen`);
      }
    }
  }
  expect(offenders).toEqual([]);
});
