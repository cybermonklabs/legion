import { expect, test } from "bun:test";
import { LEGION_CLAIM_STATES, LEGION_PHASES } from "@legion/contracts/legion-api";

import { claimStateWord, ordinalEnding, phaseWord } from "./legion-block";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  NODE_STATE_WORDS,
  PHASE_WORDS,
  ROLE_STATE_WORDS,
  UNASKED_SOURCES,
  wordsIn,
} from "./page-words";
import { JOINED_TABLES, WORD_FILES } from "./page-words-parts";

// The Legion block's words (dispatch://INFRA-4870/spec#b-r-block): the phase words and the
// claim-state words of the parent's "Words for the state" (dispatch://INFRA-1336/spec), which
// `page-words-dashboard.ts` holds once, and the block's own place, wait and read words under
// `CONTROL_LABELS.legion`.

/** Each string `value` holds, however deeply, with its path from `path`. */
function entriesIn(value: unknown, path: string): readonly (readonly [string, string])[] {
  if (typeof value === "string") return [[path, value]];
  if (typeof value === "object" && value !== null) {
    return Object.entries(value).flatMap(([key, held]) => entriesIn(held, `${path}.${key}`));
  }
  return [];
}

/** Each string `value` holds, however deeply. */
function phrasesOf(value: unknown): readonly string[] {
  return entriesIn(value, "").map(([, phrase]) => phrase);
}

/** The words of `phrase` that `DECLARED_WORDS` does not hold. */
function undeclared(phrase: string): readonly string[] {
  return wordsIn(phrase).filter((word) => !DECLARED_WORDS.includes(word));
}

/** The word of each phase, as the parent's "Words for the state" gives it, the word of
 *  `awaiting_children`, which Dashboard's phase track gives it (dispatch://INFRA-5702), and the
 *  word of `awaiting_merge`, the old value that daemons before contract 86 send. */
const PHASE_READINGS: Readonly<Record<string, string>> = {
  admitted: "Admitted",
  awaiting_person: "Waiting on a person",
  awaiting_children: "Waiting on children",
  planning: "Planning",
  implementing: "Implementing",
  testing: "Testing",
  reviewing: "Reviewing",
  accepting: "Accepting",
  retro: "Retro",
  merging: "Merging",
  integrating: "Integrating",
  held: "Held",
  awaiting_merge: "Merging",
};

/** The word of each claim state, as the parent's "Words for the state" gives it. */
const CLAIM_STATE_READINGS: Readonly<Record<string, string>> = {
  queued: "starting",
  launch_uncertain: "starting",
  launching: "starting",
  shim_connected: "starting",
  registered: "idle",
  ready: "idle",
  working: "in a turn",
  idle: "idle",
  suspended: "paused",
  failed: "failed",
};

test("every phase the contract lists has one word, and done and unrecorded have no block", () => {
  const phaseWords = phrasesOf(PHASE_WORDS);
  for (const phase of [...LEGION_PHASES, "awaiting_merge"]) {
    if (phase === "done" || phase === "unrecorded") {
      expect(phaseWord(phase), phase).toBeUndefined();
      continue;
    }
    const word = phaseWord(phase) ?? `no word for ${phase}`;
    expect(word, phase).toBe(PHASE_READINGS[phase]);
    expect(phaseWords, phase).toContain(word);
    expect(word, `${phase} reads as a phase the page does not know`).not.toBe(PHASE_WORDS.another);
    expect(undeclared(word), phase).toEqual([]);
  }
  for (const phase of ["awaiting_review", "constructor", ""]) {
    expect(phaseWord(phase), phase).toBe("In another phase");
  }
});

test("every claim state the contract lists has one word, and retired has none", () => {
  const stateWords = phrasesOf(ROLE_STATE_WORDS);
  for (const state of LEGION_CLAIM_STATES) {
    if (state === "retired") {
      expect(claimStateWord(state), state).toBeUndefined();
      continue;
    }
    const word = claimStateWord(state) ?? `no word for ${state}`;
    expect(word, state).toBe(CLAIM_STATE_READINGS[state]);
    expect(stateWords, state).toContain(word);
    expect(word, `${state} reads as a state the page does not know`).not.toBe(
      ROLE_STATE_WORDS.another
    );
    expect(undeclared(word), state).toEqual([]);
  }
  for (const state of ["hibernating", "constructor", ""]) {
    expect(claimStateWord(state), state).toBe("in another state");
  }
});

// A phrase elsewhere that holds one of the two lists' words where it says another thing, by its
// path, with the thing it says.
const HOMONYMS: Readonly<Record<string, string>> = {
  "NODE_FACT_LABELS.failed": "a fact of a check's result",
  "CONTROL_LABELS.step_results.failed": "how a run of a step ended",
  "CONTROL_LABELS.legion.read_failed.after": "the end of the block's line: Legion's read failed",
};

// The tables `page-words.ts` declares are the ones it joins (`JOINED_TABLES`). The board's words
// for an issue's status (`ISSUE_STATUS_WORDS`, which holds "Testing" and "Retro") are made from
// `features/project/board-model.ts`, and the issue's status is no Legion phase.
test("the phase words and the claim-state words are declared in one file", () => {
  const listed = new Set([...phrasesOf(PHASE_WORDS), ...phrasesOf(ROLE_STATE_WORDS)]);
  const elsewhere = [
    ...[...JOINED_TABLES].flatMap(([name, table]) => entriesIn(table, name)),
    ...WORD_FILES.filter(({ file }) => file !== "page-words-dashboard.ts").flatMap(
      ({ file, module }) =>
        Object.entries(module)
          .filter(([name]) => name !== "default")
          .flatMap(([name, table]) => entriesIn(table, `${file}: ${name}`))
    ),
  ];
  expect(
    elsewhere
      .filter(([path, phrase]) => listed.has(phrase) && !Object.hasOwn(HOMONYMS, path))
      .map(([path, phrase]) => `${path}: "${phrase}"`)
  ).toEqual([]);
});

test("an ordinal ends as English ends it", () => {
  const ordinals = [1, 2, 3, 4, 11, 12, 13, 21, 22, 23, 72, 101, 111, 112].map(
    (place) => `${place}${ordinalEnding(place)}`
  );
  expect(ordinals).toEqual([
    "1st",
    "2nd",
    "3rd",
    "4th",
    "11th",
    "12th",
    "13th",
    "21st",
    "22nd",
    "23rd",
    "72nd",
    "101st",
    "111th",
    "112th",
  ]);
});

// The rules `page-words.test.ts` holds every unasked phrase to, read on the block's own words:
// no word names Dispatch or a press, no phrase is a sentence (only an empty, gap or state line
// is), none is a check state's word or says requirement, none holds an internal word, and none
// says node. `CONTROL_LABELS` is one of the unasked sources, so that file's cases read them too.
test("the block's words keep the rules of unasked words", () => {
  expect(UNASKED_SOURCES).toContain(CONTROL_LABELS);
  const checkWords: readonly string[] = Object.values(NODE_STATE_WORDS.check);
  const internalWord = /_|[\p{L}\p{N}]-[\p{L}\p{N}]|\p{Ll}\p{Lu}|#b-|:\/\//u;
  const offenders: string[] = [];
  for (const phrase of phrasesOf(CONTROL_LABELS.legion)) {
    const words = wordsIn(phrase);
    if (words.includes("dispatch") || words.includes("press")) {
      offenders.push(`"${phrase}" names Dispatch or a press`);
    }
    if (phrase.trimEnd().endsWith(".")) offenders.push(`"${phrase}" is a sentence`);
    if (checkWords.includes(phrase)) offenders.push(`"${phrase}" reads as a check state`);
    if (/requirement/iu.test(phrase)) offenders.push(`"${phrase}" says requirement`);
    if (internalWord.test(phrase)) offenders.push(`"${phrase}" holds an internal word`);
    if (words.includes("node") || words.includes("nodes")) offenders.push(`"${phrase}" says node`);
    if (undeclared(phrase).length > 0) offenders.push(`"${phrase}" holds an undeclared word`);
  }
  expect(offenders).toEqual([]);
  expect(CONTROL_LABELS.legion.heading).toBe("Legion status");
  // A row that finds the block by its heading does not find the list of Legion's agents.
  expect(CONTROL_LABELS.collaborators_legion).not.toContain(CONTROL_LABELS.legion.heading);
});

// The parent's "Words for the state": the page does not write the word "slot"
// (dispatch://INFRA-3221/ask/bb6606c6-1254-43e7-8ab3-bf98bceb6509).
test("no word of the block is slot", () => {
  const phrases = [
    ...phrasesOf(CONTROL_LABELS.legion),
    ...phrasesOf(PHASE_WORDS),
    ...phrasesOf(ROLE_STATE_WORDS),
  ];
  expect(phrases.length).toBeGreaterThan(0);
  expect(
    phrases.filter((phrase) => wordsIn(phrase).some((word) => word === "slot" || word === "slots"))
  ).toEqual([]);
});
