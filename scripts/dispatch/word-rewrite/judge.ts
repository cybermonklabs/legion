import type { Model } from "./gateway.ts";
import { applies, type MapEntry, type Reach } from "./name-map.ts";
import { countWord, matches } from "./pattern.ts";
import { ALLOWED_NAMES, type AllowedName } from "./verdicts.ts";

/**
 * The judge of write mode: one model call per paragraph that holds occurrences no program-told
 * place leaves, and `confined`, which refuses an answer that changes more than the sentences of
 * those occurrences. The prompt below is one of the places the word may stand in the repository
 * (R1, kind 4): it must name the word to ask about it.
 */

/** A sentence's span in a text, as JavaScript indexes it. */
export interface Span {
  readonly start: number;
  readonly end: number;
}

/** The sentences of a text, end to end, so that they cover it whole: a sentence ends after `.`,
 *  `!` or `?` followed by white space, or at a line end. */
export function sentenceSpans(text: string): Span[] {
  const spans: Span[] = [];
  let start = 0;
  const boundary = /[.!?](?=\s)|\n/g;
  for (let hit = boundary.exec(text); hit !== null; hit = boundary.exec(text)) {
    const end = hit.index + hit[0].length;
    spans.push({ start, end });
    start = end;
  }
  if (start < text.length || spans.length === 0) spans.push({ start, end: text.length });
  return spans;
}

/** The sentence of `text` that holds the index `at`. */
export function sentenceAt(text: string, at: number): Span {
  return (
    sentenceSpans(text).find((span) => at >= span.start && at < span.end) ?? {
      start: 0,
      end: text.length,
    }
  );
}

/** A character of a name that holds the word, such as `parked-decision`, `park_child` or
 *  `parked_decision.go`. */
const NAME_CHARACTER = /[A-Za-z0-9_./-]/;
/** A mark a name does not end with: a stop after a word, a dash or a slash before it. */
const NAME_EDGE = /[./-]/;

/** The whole name that holds the match from `start` to `end`: the run of name characters around
 *  it, without the marks at its two ends. */
function wholeNameAt(text: string, start: number, end: number): Span {
  let from = start;
  while (from > 0 && NAME_CHARACTER.test(text.charAt(from - 1))) from -= 1;
  let to = end;
  while (to < text.length && NAME_CHARACTER.test(text.charAt(to))) to += 1;
  while (from < start && NAME_EDGE.test(text.charAt(from))) from += 1;
  while (to > end && NAME_EDGE.test(text.charAt(to - 1))) to -= 1;
  return { start: from, end: to };
}

/** The whole names of `text` that hold the word at `spans`, in their order, each name once. */
function wholeNames(text: string, spans: readonly Span[]): string[] {
  const names: string[] = [];
  let last = -1;
  for (const span of [...spans].sort((a, b) => a.start - b.start)) {
    const name = wholeNameAt(text, span.start, span.end);
    if (name.start === last) continue;
    last = name.start;
    names.push(text.slice(name.start, name.end));
  }
  return names;
}

/** How many sentences `text` holds: its spans (`sentenceSpans`) that hold more than white space,
 *  so a line end between two sentences, or an empty text, counts for none. */
function sentenceCount(text: string): number {
  return sentenceSpans(text).filter((span) => /\S/.test(text.slice(span.start, span.end))).length;
}

/**
 * Why `after` is not `before` with only the sentences that hold the judged occurrences rewritten,
 * or null when it is. The answer must hold no trace of the word but the occurrences the
 * paragraph keeps (`kept`, each a span of `before`, which must stay as they are): those a
 * program-told place keeps, and those the answer itself names as left. Each kept occurrence is
 * checked by its place first, before the count of occurrences: the whole names of the kept
 * occurrences must stand among the whole names of `after` that hold the word, in their order, so
 * a rewrite that renames or drops a kept name is refused naming it, whether or not it leaves a
 * judged use of the word beside it. Then `after` must hold the word as many times as `kept` does.
 *
 * The sentences of `before` that hold a judged occurrence are the rewritable parts; every other
 * part must stand in `after` unchanged and in its order: the text before the first rewritable
 * sentence as a prefix, the text after the last as a suffix, and each stretch between two of them
 * after the one before it. The rewritten text between the prefix and the suffix must hold as many
 * sentences (`sentenceCount`) as the text of `before` it replaces, so a rewrite adds no sentence
 * and drops none. The reason quotes the part that changed, for the model's second ask.
 */
export function confinedProblem(
  before: string,
  after: string,
  judged: readonly Span[],
  kept: readonly Span[] = []
): string | null {
  if (judged.length === 0) return before === after ? null : "the paragraph had to stay as it was";
  const afterNames = wholeNames(after, occurrenceSpans(after));
  let place = 0;
  for (const name of wholeNames(before, kept)) {
    place = afterNames.indexOf(name, place) + 1;
    if (place === 0) return `the rewrite changed ${JSON.stringify(name)}, which had to stay`;
  }
  if (countWord(after) !== kept.length) {
    return "the rewrite still holds the word where it had to say what the word meant";
  }
  const hot = sentenceSpans(before).filter((span) =>
    judged.some((occurrence) => occurrence.start >= span.start && occurrence.start < span.end)
  );
  const first = hot[0];
  const last = hot.at(-1);
  if (first === undefined || last === undefined) return "no sentence holds a marked occurrence";
  const prefix = before.slice(0, first.start);
  const suffix = before.slice(last.end);
  if (!after.startsWith(prefix)) {
    return `the rewrite changed the text before the marked sentences, which had to stay exactly ${JSON.stringify(prefix)}`;
  }
  if (!after.endsWith(suffix) || prefix.length + suffix.length > after.length) {
    return `the rewrite changed the text after the marked sentences, which had to stay exactly ${JSON.stringify(suffix)}`;
  }
  const middle = after.slice(prefix.length, after.length - suffix.length);
  let cursor = 0;
  for (let index = 0; index + 1 < hot.length; index += 1) {
    const current = hot[index];
    const next = hot[index + 1];
    if (current === undefined || next === undefined) break;
    const between = before.slice(current.end, next.start);
    if (between === "") continue;
    const found = middle.indexOf(between, cursor);
    if (found === -1) {
      return `the rewrite changed the text between two marked sentences, which had to stay exactly ${JSON.stringify(between)}`;
    }
    cursor = found + between.length;
  }
  const replaced = sentenceCount(before.slice(first.start, last.end));
  const written = sentenceCount(middle);
  if (written !== replaced) {
    return `the rewritten sentences number ${written} where the marked sentences they replace number ${replaced}: a rewrite adds no sentence and drops none`;
  }
  return null;
}

/** Whether `after` changes only the sentences of the judged occurrences (`confinedProblem`). */
export function confined(
  before: string,
  after: string,
  judged: readonly Span[],
  kept: readonly Span[] = []
): boolean {
  return confinedProblem(before, after, judged, kept) === null;
}

/** A marked occurrence a rewrite leaves as it was, by its offset in the paragraph, and why. */
export interface Left {
  readonly start: number;
  readonly allowed: AllowedName;
}

/** The judge's answer to one paragraph. A refused answer says why, for the second ask. A
 *  rewrite may leave some of the marked occurrences as they were (`left`, spec version 39): the
 *  write keeps their verdicts under the new paragraph. */
export type Judgement =
  | {
      readonly kind: "rewrite";
      readonly paragraph: string;
      readonly left?: readonly Left[];
    }
  | { readonly kind: "allowed"; readonly allowed: AllowedName }
  | { readonly kind: "judge-refused"; readonly reason: string }
  /** No answer came (the gateway's fault): nothing is decided, and the item waits. */
  | { readonly kind: "no-answer" };

const NOT_THE_SHAPE =
  'the answer was not one JSON object, either {"rewrite": "<the paragraph>"}, {"rewrite": "<the paragraph>", "left": {"<offset>": "<one of the five reasons>"}} or {"allowed": "<one of the five reasons>"}';

/** The occurrences a rewrite names as left, or undefined when `value` is not that object: each
 *  key an offset, each value an allowed reason. */
function leftOf(value: unknown): Left[] | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  const left: Left[] = [];
  for (const [offset, reason] of Object.entries(value)) {
    const allowed = ALLOWED_NAMES.find((name) => name === reason);
    if (!/^(0|[1-9][0-9]*)$/.test(offset) || allowed === undefined) return undefined;
    left.push({ start: Number(offset), allowed });
  }
  return left.sort((a, b) => a.start - b.start);
}

/** Reads the model's text as the JSON the prompt asks for: exactly `{"rewrite": "<paragraph>"}`,
 *  that with `"left"`, or `{"allowed": "<reason>"}`. Anything else is refused. */
export function readAnswer(text: string): Judgement {
  const refused = { kind: "judge-refused", reason: NOT_THE_SHAPE } as const;
  const open = text.indexOf("{");
  const close = text.lastIndexOf("}");
  if (open === -1 || close < open) return refused;
  let value: unknown;
  try {
    value = JSON.parse(text.slice(open, close + 1));
  } catch {
    return refused;
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) return refused;
  const keys = Object.keys(value).sort().join(",");
  if ((keys === "rewrite" || keys === "left,rewrite") && "rewrite" in value) {
    if (typeof value.rewrite !== "string") return refused;
    if (!("left" in value)) return { kind: "rewrite", paragraph: value.rewrite };
    const left = leftOf(value.left);
    if (left === undefined) return refused;
    return left.length === 0
      ? { kind: "rewrite", paragraph: value.rewrite }
      : { kind: "rewrite", paragraph: value.rewrite, left };
  }
  if (keys === "allowed" && "allowed" in value) {
    const allowed = ALLOWED_NAMES.find((name) => name === value.allowed);
    if (allowed !== undefined) return { kind: "allowed", allowed };
  }
  return refused;
}

/** What the judge is told of one paragraph. */
export interface Question {
  readonly paragraph: string;
  /** The occurrences to judge, as spans of the paragraph. */
  readonly judged: readonly Span[];
  /** The occurrences a program-told place keeps, which the answer must keep as they are. */
  readonly kept: readonly Span[];
  /** `title`, `issue document` or `project document`. */
  readonly kind: string;
  /** The issue's status, or null for a project document. */
  readonly status: string | null;
  /** The issue's last five status moves, oldest first, each `<time> <status>`. */
  readonly statusEvents: readonly string[];
  /** The map entries that apply in the item's project. */
  readonly entries: readonly MapEntry[];
}

const SYSTEM = `You edit text that AI agents wrote in Dispatch, an issue tracker. The word "park"
(park, parks, parked, parking, and names that hold it, such as park_child or isParked) is being
removed, because it hides what happened. Each use gets the words of its own meaning:

- A status use: the word said that an issue, a tree, a plan, a branch of work, a release, or a
  document or notes of such work was set aside in the issue tracker (moved to the backlog or the
  icebox, or kept out of the workflow). This is its meaning wherever it speaks of anything of the
  work in the tracker, unless the paragraph shows one of the meanings below. Write Dispatch's
  status words for it: "moved to backlog", "in backlog", "in icebox", "in backlog or icebox", and
  where done or triage are meant too, "out of the workflow". Never write "held", "held back",
  "on hold", "paused", "pending", "shelved", "set aside", "deferred", "suspended" or any other
  word for a status use: only Dispatch's status words.
- A relaunch that the Legion daemon's wake pacing holds back is "delayed", never "backlog".
- A launch that waits for the issue pod's start gate "waits for the pod".
- A person's decision kept for a tree that does not run is a "kept decision".
- For a program's own things (a goroutine, a thread, a lock, a test hook, a request, a document
  update that waits for the updates it needs), write what happens there: blocked, waits, paused,
  held, pending.
- A renamed code name gets its new name exactly, from the list of names you are given. A name
  that is not on the list stays as it is.

Some uses must stay. An occurrence that must stay has one of these reasons:
- "quote": inside a quote of a person's words;
- "word-as-word": the word named as a word, in quotation marks, such as a record of the rule
  against it;
- "other-team-technical": another team's own technical term, such as a parked Linux kernel
  thread or kthread_park;
- "third-party-name": a name that code outside these repositories defines (a crate, an API, a
  field of another program);
- "record": a record kept as the fixed input or expected output of a test or a measurement.

Rewrite only the sentences that hold the marked occurrences that must not stay, each into one
sentence: add no sentence and drop none. Change nothing else: keep every other sentence, every
space, every link and every markdown mark exactly. Keep
each occurrence listed under "keep" exactly as it is. Keep each marked occurrence that must stay
exactly as it is too, and name it under "left" by the offset it is marked at, with its reason.
If every marked occurrence must stay, answer with the reason alone.

Answer with one JSON object and nothing else: {"rewrite": "<the whole paragraph, rewritten>"};
{"rewrite": "<the whole paragraph, rewritten>", "left": {"<offset>": "<reason>"}}, where some
marked occurrences stay; or {"allowed": "quote" | "word-as-word" | "other-team-technical" |
"third-party-name" | "record"}, where every one stays.`;

/** The prompt of one question; `refusal` is why the first answer was refused, for a second ask. */
export function buildPrompt(
  question: Question,
  refusal?: string
): { system: string; user: string } {
  const quoted = (span: Span) => JSON.stringify(question.paragraph.slice(span.start, span.end));
  const names = question.entries.map(
    (entry) => `- ${entry.old} -> ${entry.new}${entry.codeName ? " (a code name)" : ""}`
  );
  const lines = [
    `Kind of text: ${question.kind}.`,
    `The issue's status: ${question.status ?? "none (a project document)"}.`,
    `Its last status moves: ${question.statusEvents.length === 0 ? "none" : question.statusEvents.join("; ")}.`,
    "",
    "Renamed names that apply here:",
    ...(names.length === 0 ? ["- none"] : names),
    "",
    "The paragraph, between the lines:",
    "-----",
    question.paragraph,
    "-----",
    "",
    "Marked occurrences (character offsets in the paragraph):",
    ...question.judged.map((span) => `- at ${span.start}: ${quoted(span)}`),
    ...(question.kept.length === 0
      ? []
      : ["Keep exactly:", ...question.kept.map((span) => `- at ${span.start}: ${quoted(span)}`)]),
    ...(refusal === undefined
      ? []
      : [
          "",
          `Your earlier answer to this paragraph was refused: ${refusal}. Answer again, changing only the sentences that hold the marked occurrences.`,
        ]),
  ];
  return { system: SYSTEM, user: lines.join("\n") };
}

/** The entries of the map that apply in a project. */
export function applyingEntries(entries: readonly MapEntry[], reach: Reach): MapEntry[] {
  return entries.filter((entry) => applies(entry, reach));
}

/** An answer held to its question, whether the model or the tester (the settle command) gave
 *  it: the occurrences a rewrite names as left must be marked ones, and stay as they were with
 *  the sentences they alone stand in; at least one marked occurrence must be rewritten; and the
 *  rewrite must pass `confinedProblem`. A refused answer says why. */
export function checkAnswer(question: Question, answer: Judgement): Judgement {
  if (answer.kind !== "rewrite") return answer;
  const left = answer.left ?? [];
  const marked = new Set(question.judged.map((span) => span.start));
  const stray = left.find((one) => !marked.has(one.start));
  if (stray !== undefined) {
    return {
      kind: "judge-refused",
      reason: `"left" names the offset ${stray.start}, where no marked occurrence stands`,
    };
  }
  const leftStarts = new Set(left.map((one) => one.start));
  const rewritten = question.judged.filter((span) => !leftStarts.has(span.start));
  if (rewritten.length === 0) {
    return {
      kind: "judge-refused",
      reason: "every marked occurrence stays, so the answer is the reason alone",
    };
  }
  const problem = confinedProblem(question.paragraph, answer.paragraph, rewritten, [
    ...question.kept,
    ...question.judged.filter((span) => leftStarts.has(span.start)),
  ]);
  return problem === null ? answer : { kind: "judge-refused", reason: problem };
}

/** One ask of the model, its answer held to the shape and to `checkAnswer`. */
async function askOnce(question: Question, model: Model, refusal?: string): Promise<Judgement> {
  const text = await model.ask(buildPrompt(question, refusal));
  if (text === null) return { kind: "no-answer" };
  return checkAnswer(question, readAnswer(text));
}

/** The judgement of one paragraph and how many asks it took: a refused first answer is asked
 *  once more, naming why it was refused. */
export interface Judged {
  readonly judgement: Judgement;
  readonly asks: 1 | 2;
}

/** One question put to the model, and once more when its first answer is refused. A second
 *  refusal is the judgement, which the writer keeps as `judge-refused` (the code `j` and a
 *  `refused` entry of the state): no later run asks about it again while its text stands. */
export async function judge(question: Question, model: Model): Promise<Judged> {
  const first = await askOnce(question, model);
  if (first.kind !== "judge-refused") return { judgement: first, asks: 1 };
  return { judgement: await askOnce(question, model, first.reason), asks: 2 };
}

/** The spans of every occurrence of a text. */
export function occurrenceSpans(text: string): Span[] {
  return matches(text).map((match) => ({ start: match.start, end: match.end }));
}
