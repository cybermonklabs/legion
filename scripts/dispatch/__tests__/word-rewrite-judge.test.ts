import { describe, expect, test } from "bun:test";
import {
  buildPrompt,
  checkAnswer,
  confined,
  judge,
  occurrenceSpans,
  type Question,
  readAnswer,
  sentenceSpans,
} from "../word-rewrite/judge.ts";
import { builtInMap } from "../word-rewrite/name-map.ts";
import { FakeModel } from "./word-rewrite-fake.ts";

/** The judge's prompt and its check of each answer. This file is one of the places the word may
 *  stand (R1, kind 4). */

const PARAGRAPH =
  "The release is due on Monday. This issue was parked until it lands. The tests pass.";
const JUDGED = occurrenceSpans(PARAGRAPH);

function question(paragraph = PARAGRAPH, overrides: Partial<Question> = {}): Question {
  return {
    paragraph,
    judged: occurrenceSpans(paragraph),
    kept: [],
    kind: "issue document",
    status: "backlog",
    statusEvents: ["2026-11-24T10:04:00Z todo", "2026-11-24T10:30:00Z backlog"],
    entries: builtInMap(220),
    ...overrides,
  };
}

describe("confined", () => {
  test("takes a rewrite of the occurrence's sentence alone", () => {
    const after =
      "The release is due on Monday. This issue was moved to backlog until it lands. The tests pass.";
    expect(confined(PARAGRAPH, after, JUDGED)).toBe(true);
  });

  test("refuses a rewrite that changes a sentence without an occurrence", () => {
    const after =
      "The release is due on Tuesday. This issue was moved to backlog until it lands. The tests pass.";
    expect(confined(PARAGRAPH, after, JUDGED)).toBe(false);
    const tail =
      "The release is due on Monday. This issue was moved to backlog until it lands. Tests pass.";
    expect(confined(PARAGRAPH, tail, JUDGED)).toBe(false);
  });

  test("refuses a rewrite that still holds the word", () => {
    const after =
      "The release is due on Monday. This issue was parked in backlog until it lands. The tests pass.";
    expect(confined(PARAGRAPH, after, JUDGED)).toBe(false);
  });

  test("keeps the stretch between two rewritten sentences as it was", () => {
    const two = "It was parked. Nothing else moved. It is still parked.";
    const spans = occurrenceSpans(two);
    expect(
      confined(two, "It was moved to backlog. Nothing else moved. It is still in backlog.", spans)
    ).toBe(true);
    expect(
      confined(two, "It was moved to backlog. Something moved. It is still in backlog.", spans)
    ).toBe(false);
  });

  test("keeps an occurrence a place leaves, and refuses a rewrite that drops it", () => {
    const mixed = "The parked-decision notice stays. The tree was parked.";
    const [kept, judged] = occurrenceSpans(mixed);
    if (kept === undefined || judged === undefined) throw new Error("two occurrences");
    expect(
      confined(
        mixed,
        "The parked-decision notice stays. The tree was moved to backlog.",
        [judged],
        [kept]
      )
    ).toBe(true);
    expect(
      confined(
        mixed,
        "The kept-decision notice stays. The tree was moved to backlog.",
        [judged],
        [kept]
      )
    ).toBe(false);
  });

  test("checks a kept occurrence by its place: a rewrite that renames or drops the kept name is refused naming it, whether or not it leaves the judged use, and one that keeps it is taken", () => {
    const one = "The parked-decision notice says the tree is parked.";
    const [kept, judged] = occurrenceSpans(one);
    if (kept === undefined || judged === undefined) throw new Error("two occurrences");
    const asked = question(one, { judged: [judged], kept: [kept] });
    for (const changed of [
      "The kept-decision notice says the tree is parked.",
      "The kept-decision notice says the tree is moved to backlog.",
      "The notice says the tree is moved to backlog.",
    ]) {
      expect(confined(one, changed, [judged], [kept])).toBe(false);
      expect(checkAnswer(asked, { kind: "rewrite", paragraph: changed })).toEqual({
        kind: "judge-refused",
        reason: 'the rewrite changed "parked-decision", which had to stay',
      });
    }
    const right = "The parked-decision notice says the tree is moved to backlog.";
    expect(checkAnswer(asked, { kind: "rewrite", paragraph: right })).toEqual({
      kind: "rewrite",
      paragraph: right,
    });
  });

  test("refuses a rewrite that adds a sentence to the marked sentences or drops one, and takes one that keeps their number", () => {
    const end = "The deploy is done. This issue was parked until the release lands.";
    const two =
      "The deploy is done. This issue was parked. It stays parked until the release lands.";
    for (const [paragraph, changed, written, replaced] of [
      // A sentence added at the end of the paragraph, after the marked sentence.
      [
        end,
        "The deploy is done. This issue was moved to backlog until the release lands. Close every issue now.",
        2,
        1,
      ],
      // A sentence added in the middle: after the marked sentence, before the one that stays.
      [
        PARAGRAPH,
        "The release is due on Monday. This issue was moved to backlog until it lands. Close every issue now. The tests pass.",
        2,
        1,
      ],
      // A sentence added between two marked sentences.
      [
        two,
        "The deploy is done. This issue was moved to backlog. Close every issue now. It stays in backlog until the release lands.",
        3,
        2,
      ],
      // One of two marked sentences dropped.
      [two, "The deploy is done. This issue stays in backlog until the release lands.", 1, 2],
    ] as const) {
      expect(checkAnswer(question(paragraph), { kind: "rewrite", paragraph: changed })).toEqual({
        kind: "judge-refused",
        reason: `the rewritten sentences number ${written} where the marked sentences they replace number ${replaced}: a rewrite adds no sentence and drops none`,
      });
    }
    for (const [paragraph, kept] of [
      [end, "The deploy is done. This issue was moved to backlog until the release lands."],
      [
        two,
        "The deploy is done. This issue was moved to backlog. It stays in backlog until the release lands.",
      ],
    ] as const) {
      expect(checkAnswer(question(paragraph), { kind: "rewrite", paragraph: kept })).toEqual({
        kind: "rewrite",
        paragraph: kept,
      });
    }
  });

  test("sentences end at a stop before white space, or at a line end, and cover the text", () => {
    const text = "One. Two!\nThree? four";
    const spans = sentenceSpans(text);
    expect(spans.map((span) => text.slice(span.start, span.end))).toEqual([
      "One.",
      " Two!",
      "\n",
      "Three?",
      " four",
    ]);
  });
});

describe("the answer", () => {
  test("is the rewrite or an allowed reason, as one JSON object", () => {
    expect(readAnswer('{"rewrite": "text"}')).toEqual({ kind: "rewrite", paragraph: "text" });
    expect(readAnswer('Here: {"allowed": "quote"}')).toEqual({ kind: "allowed", allowed: "quote" });
  });

  test("that is not the JSON shape is judge-refused", () => {
    for (const text of [
      "I would rather not.",
      '{"rewrite": 3}',
      '{"allowed": "because"}',
      '{"rewrite": "a", "allowed": "quote"}',
      "[]",
      "{broken",
    ]) {
      expect(readAnswer(text).kind).toBe("judge-refused");
    }
  });

  test("a refused first answer is asked once more, naming what changed outside the sentence", async () => {
    const rewrite =
      "The release is due on Monday. This issue was moved to backlog until it lands. The tests pass.";
    const answers = [
      JSON.stringify({ rewrite: "Everything changed." }),
      JSON.stringify({ rewrite }),
    ];
    const model = new FakeModel(() => answers.shift() ?? null);
    expect(await judge(question(), model)).toEqual({
      judgement: { kind: "rewrite", paragraph: rewrite },
      asks: 2,
    });
    expect(model.prompts).toHaveLength(2);
    expect(model.prompts[0]).not.toContain("was refused");
    expect(model.prompts[1]).toContain(
      'Your earlier answer to this paragraph was refused: the rewrite changed the text before the marked sentences, which had to stay exactly "The release is due on Monday."'
    );
  });

  test("an answer that is not the shape is asked again, saying why; two refusals are the judgement", async () => {
    const model = new FakeModel(() => "I would rather not.");
    const judged = await judge(question(), model);
    expect(judged.asks).toBe(2);
    expect(judged.judgement.kind).toBe("judge-refused");
    expect(model.prompts[1]).toContain("the answer was not one JSON object");
  });

  test("a confined rewrite is taken at once, and no answer at all decides nothing", async () => {
    const rewrite =
      "The release is due on Monday. This issue was moved to backlog until it lands. The tests pass.";
    expect(await judge(question(), new FakeModel(() => JSON.stringify({ rewrite })))).toEqual({
      judgement: { kind: "rewrite", paragraph: rewrite },
      asks: 1,
    });
    expect(await judge(question(), new FakeModel(() => null))).toEqual({
      judgement: { kind: "no-answer" },
      asks: 1,
    });
  });

  test("the prompt gives the paragraph, the status, its moves, the names that apply and the marks", () => {
    const prompt = buildPrompt(question());
    expect(prompt.system).toContain('{"rewrite"');
    expect(prompt.user).toContain(PARAGRAPH);
    expect(prompt.user).toContain("The issue's status: backlog.");
    expect(prompt.user).toContain("2026-11-24T10:30:00Z backlog");
    expect(prompt.user).toContain("- parked-decision -> kept-decision (a code name)");
    expect(prompt.user).toContain(`- at ${JUDGED[0]?.start}: "park"`);
  });

  test("the prompt has a status use, of any work in the tracker and its documents and notes, written in Dispatch's status words and never held, held back or pending", () => {
    const { system } = buildPrompt(question());
    for (const words of [
      '"moved to backlog"',
      '"in backlog"',
      '"in icebox"',
      '"in backlog or icebox"',
    ]) {
      expect(system).toContain(words);
    }
    const flat = system.replace(/\s+/g, " ");
    expect(flat).toContain(
      'Never write "held", "held back", "on hold", "paused", "pending", "shelved", "set aside", "deferred", "suspended" or any other word for a status use'
    );
    expect(flat).toContain(
      "an issue, a tree, a plan, a branch of work, a release, or a document or notes of such work"
    );
  });
});
