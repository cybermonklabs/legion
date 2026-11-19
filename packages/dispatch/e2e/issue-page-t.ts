import { readFileSync } from "node:fs";
import { expect, type Locator, type Page, type Route, test } from "@playwright/test";

import type { IssueTreeAnswer } from "../web/src/api/client";
import type { IssueDecisions } from "../web/src/api/types";
import { CONTROL_LABELS, GAP_LINES } from "../web/src/features/issue-page/page-words";
import type { Filter } from "../web/src/features/issue-page/presets";
import {
  mergeTreePages,
  type TreeIdReads,
  treeIdOf,
} from "../web/src/features/issue-page/tree-read/tree-nodes";
import { parseNodeReference } from "../web/src/features/refs/routes";
import { nextFrames } from "./frames";
import {
  seedConversationState,
  seedDecisionsFilter,
  seedNoChildren,
  seedNoSummary,
  seedPlanOrder,
  seedReplyNesting,
  seedRequestBaseline,
  seedReviewList,
  seedSpineCases,
  seedTwiceTheNodes,
  seedUnbrokenChildTitle,
  seedUnbrokenWords,
  seedWholeIssue,
  seedWholeIssueAdvancing,
} from "./issue-layout-seed";
import { blockNode, focusThePage, nodeRows, rowBody } from "./issue-page-g";
import { issueHeader } from "./issue-page-p";
import { type ApiRequest, type ApiRequests, expectLoadSettled } from "./traffic";

/**
 * What the rows of the new issue page read once the page draws its outline from the tree read,
 * and how each row is named so that two runs, and two heads, can be compared row by row. These
 * rows keep their helpers in this file of their own, as every set of the page's rows does: it
 * reuses the readers and locators of `e2e/issue-page-g.ts` and copies none of them.
 *
 * `drawnRowReadings` reads every `[data-node-row]` of `[data-issue-page]` as `NodeRow.tsx`
 * draws it, in drawing order, with one `page.evaluate` for all the rows and, where it reads the
 * bodies, one more for each row once the focus is on it:
 *
 * - `id` is `data-node-id`, `rowKey` is `data-row-key` (null on a view's row, which draws none),
 *   and `level` is `aria-level`; the row a row sits under is the nearest row before it one level
 *   up (`parentIndex`), as `parentOfRow` in `e2e/issue-page-g.ts` finds it.
 * - `words` are `[data-node-words]` without their trail (`[data-node-trail]`: the chips, the gap
 *   words and a count), a child's facts and any control; a folded row's one line,
 *   `[data-node-first-line]`, where it draws no words.
 * - In the row's group `[data-node-margin]`: `kind` is the row's `data-node-type` and the text of
 *   its tag `[data-node-tag]`, `author` is `[data-node-author]`, `time` is `[data-node-time]` as
 *   drawn (the row draws its time in that span, and no `<time>` element), and `state` is the
 *   row's `data-node-state` with the word `[data-node-state]` draws and `[data-node-unsure]`.
 * - Each `[data-node-chip]`: the words drawn before its name (its tie), its name
 *   (`[data-chip-name]`, or the word for a target deleted) and the `href` of the link the name is.
 *   A chip whose tie is `CONTROL_LABELS.also_under` is one of the other parents the row lists, and
 *   is read into `alsoUnder` rather than `chips`.
 * - Each `[data-gap]`: its `data-gap-kind`, its word on the row, and the sentence the body says
 *   it in (`GAP_LINES`).
 * - `facts`: a child's facts on its row (`[data-child-facts]`), which carry no label, then the
 *   body's facts, each by the label its `<dt>` draws and the value its `<dd>` draws.
 * - `counts`: what a folded row counts (`[data-node-count]`), then the body's line of what the
 *   coverage read counts for a requirement.
 * - `body`: what the row draws while it has the focus beyond its line, read with the focus on it
 *   as the tree's keys put it there (`focusRow` does it by id, which a node drawn under two parents
 *   shares, so this moves it by place): who made the node where the line names nobody ("Author
 *   unknown") or names an agent by its role's word (its whole title), the words drawn whole, an
 *   ask's options with their own words and its answer, and every other line `[data-node-body]`
 *   draws. Never a control: not the row's acts (`[data-row-acts]`), not Keep and Change
 *   (`DecisionActions`), not the control that keeps the body open, nor any line that holds a
 *   control.
 *
 * `seedKeys` and `keyedRows` name each row by what the seed returned rather than by an id the
 * server minted (a comment's, an ask's, a result's, a block's UUID), and write each time as the
 * source it was drawn from rather than as an instant.
 *
 * The rows of `e2e/issue-page-t.e2e.ts` read what the page received besides what it drew:
 * `treeAnswerOf` and `viewAnswerOf` keep each answer of the tree read
 * (`GET /api/v1/issues/{key}/tree`) and of one of its views (`?view=`), every page of a read
 * merged; `decisionsReadOf` keeps the decisions read; `viewNodeIds` names the rows root R2 lets a
 * view draw. `treeReads` and `blocksReads` count what a load or a press sent. Each row's gate is
 * one `owedUnless` call on a probe (`treeReadGate`, `convertedGate`, `decisionsViewGate`,
 * `decisionNodesGate`, `partUGate`, `footLineGate`): where the gate is not on this branch's base
 * the row skips, naming the gate in an `owed` annotation, and never passes, since a row cannot
 * pass on code the base does not hold yet.
 */

/** A row's kind: the model's token for it, and the tag its group draws (a typed kind's word, or a
 *  child's key). */
export interface RowKind {
  readonly tag: string | null;
  readonly type: string | null;
}

/** A row's state: the model's token on the row, the word its group draws for it ("Kept by" and
 *  the person included), and the one word a decision the model marks unsure says of itself. */
export interface RowState {
  readonly token: string | null;
  readonly unsure: string | null;
  readonly word: string | null;
}

/** One chip at the end of a row's words. */
export interface RowChip {
  /** The `href` of the link the chip's name is, or null where its target is drawn as words. */
  readonly address: string | null;
  /** The words drawn before the name: the tie's word, and the line a stale target carries. */
  readonly label: string | null;
  readonly name: string;
}

/** One other parent a row lists ("Also under"). */
export interface RowParent {
  readonly address: string | null;
  readonly name: string;
}

/** One gap a row names: its kind, its word on the row, and the body's sentence for it, null
 *  where the body is not read or does not say it. */
export interface RowGap {
  readonly kind: string;
  readonly line: string | null;
  readonly word: string;
}

/** One fact: the label the page draws for it, null for a child's facts on its row, and its
 *  value as drawn. */
export interface RowFact {
  readonly name: string | null;
  readonly value: string;
}

/** One choice an ask offers, as the focused row draws it with its own words. */
export interface RowOption {
  readonly chosen: boolean;
  readonly words: string;
}

/** A person's answer an ask's row draws: who answered, what they chose where the ask lists no
 *  options, and what they wrote, every character kept. */
export interface RowAnswer {
  readonly author: string | null;
  readonly choices: readonly string[];
  readonly text: string | null;
}

/** What a row draws while it has the focus, beyond its line (the facts and counts are read into
 *  the row's own `facts` and `counts`, and the gap sentences into its `gaps`). */
export interface RowBody {
  readonly answer: RowAnswer | null;
  /** "Author unknown", or an agent's whole title; null where the line names the author. */
  readonly author: string | null;
  /** Each other line the body draws that holds no control. */
  readonly lines: readonly string[];
  readonly options: readonly RowOption[];
  readonly words: string | null;
}

/** One row of the page, as `drawnRowReadings` reads it. */
export interface RowReading {
  readonly alsoUnder: readonly RowParent[];
  readonly author: string | null;
  /** Null where the bodies were not read. */
  readonly body: RowBody | null;
  readonly chips: readonly RowChip[];
  readonly counts: readonly string[];
  readonly facts: readonly RowFact[];
  readonly gaps: readonly RowGap[];
  readonly id: string | null;
  readonly kind: RowKind;
  readonly level: number | null;
  readonly rowKey: string | null;
  readonly state: RowState | null;
  readonly time: string | null;
  readonly words: string | null;
}

/** What the focused row's body holds, as the browser reads it, before the facts and the lines are
 *  sorted into the row's own fields. */
interface PageBody extends RowBody {
  readonly facts: readonly RowFact[];
}

/** One row as the browser reads it: the gaps without their sentences, which the body says. */
interface PageRow extends Omit<RowReading, "body" | "gaps"> {
  readonly body: PageBody | null;
  readonly gaps: readonly Omit<RowGap, "line">[];
}

/** What `readPageRows` is handed. */
interface PageRead {
  /** The tie word of a chip naming another parent of the row (`CONTROL_LABELS.also_under`). */
  readonly alsoUnder: string;
  /** The place of the one row to read with its body, or null to read every row and no body. */
  readonly focused: number | null;
}

/** The sentence a body says each gap in, by the gap's kind. */
const GAP_LINE_OF: Readonly<Partial<Record<string, string>>> = GAP_LINES;

/** How the body's line of the coverage read's two counts begins (`NodeRow.tsx`'s `NodeBody`). */
const COUNTS_LEAD = `${CONTROL_LABELS.serving_checks}:`;

/**
 * Every row of the page in drawing order (the head comment says what each field is read from).
 * With `bodies` (the default) it then puts the focus on each row in turn and reads what its body
 * draws, and ends with the focus on no row, as `unfoldEveryBranch` does. A row that is not
 * the row read before the focus moved fails the reading, since the bodies would then be another
 * page's.
 */
export async function drawnRowReadings(
  page: Page,
  { bodies = true }: { readonly bodies?: boolean } = {}
): Promise<RowReading[]> {
  // One reading of the rows as the page draws them at this instant, which is all this helper
  // claims: a caller that needs the rows settled reads it inside `steadily` or a poll, and the
  // walk below fails where a row moved before its body was read.
  const drawn = await page.evaluate(readPageRows, {
    alsoUnder: CONTROL_LABELS.also_under,
    focused: null,
  });
  if (!bodies) return drawn.map((row) => readingOf(row, null));
  const readings: RowReading[] = [];
  for (const [index, row] of drawn.entries()) {
    readings.push(readingOf(row, await focusedBody(page, index, row)));
  }
  await focusThePage(page);
  return readings;
}

/** The body row `index` draws once it holds the focus. */
async function focusedBody(page: Page, index: number, row: PageRow): Promise<PageBody> {
  const where = `row ${index} (${row.id ?? "no id"})`;
  const target = nodeRows(page).nth(index);
  await target.focus();
  await expect(target, `${where} never took the focus`).toBeFocused();
  await expect(rowBody(target), `${where} drew no body with the focus`).toHaveCount(1);
  // The two expects above waited for the focus and the body, so this one reading is of a settled
  // row, and a row other than the one read before means the rows moved, not a page still drawing.
  const [focused] = await page.evaluate(readPageRows, {
    alsoUnder: CONTROL_LABELS.also_under,
    focused: index,
  });
  if (focused === undefined || focused.id !== row.id || focused.rowKey !== row.rowKey) {
    throw new Error(
      `${where} is ${focused?.id ?? "no row"} once it has the focus: the rows moved while the ` +
        "reader read their bodies"
    );
  }
  if (focused.body === null) throw new Error(`${where} drew no body with the focus`);
  return focused.body;
}

/** One row's reading, with what its body said sorted into its gaps, facts and counts. */
function readingOf(row: PageRow, body: PageBody | null): RowReading {
  if (body === null) {
    return { ...row, body: null, gaps: row.gaps.map((gap) => ({ ...gap, line: null })) };
  }
  const gaps = row.gaps.map((gap) => {
    const line = GAP_LINE_OF[gap.kind];
    return { ...gap, line: line !== undefined && body.lines.includes(line) ? line : null };
  });
  const said = new Set(gaps.flatMap((gap) => (gap.line === null ? [] : [gap.line])));
  return {
    ...row,
    body: {
      answer: body.answer,
      author: body.author,
      lines: body.lines.filter((line) => !said.has(line) && !line.startsWith(COUNTS_LEAD)),
      options: body.options,
      words: body.words,
    },
    counts: [...row.counts, ...body.lines.filter((line) => line.startsWith(COUNTS_LEAD))],
    facts: [...row.facts, ...body.facts],
    gaps,
  };
}

/**
 * Reads the rows in the browser: every row with no body, or the one row `focused` names with its
 * body. It runs inside the page, so it names nothing outside itself.
 */
function readPageRows({ alsoUnder, focused }: PageRead): PageRow[] {
  // A control's words are no reading of the node: its label is the page's, and what it says
  // after a press (a copy's "Copied") is the press's.
  const controls = "button, input, select, textarea, form, [role='alert'], [aria-live]";
  const text = (element: Element | null | undefined): string | null =>
    element === null || element === undefined
      ? null
      : (element.textContent ?? "").replace(/\s+/g, " ").trim();
  const textWithout = (element: Element | null | undefined, left: string): string | null => {
    if (element === null || element === undefined) return null;
    const copy = element.cloneNode(true) as Element;
    for (const part of copy.querySelectorAll(left)) part.remove();
    return text(copy);
  };
  // The body of a row the focus left stays drawn until another row takes it, so what a row
  // draws on its line is read outside its body.
  const outsideBody = (row: Element, selector: string): Element[] =>
    [...row.querySelectorAll(selector)].filter(
      (found) => found.closest("[data-node-body]") === null
    );
  const wordsOf = (row: Element): string | null => {
    const words = row.querySelector("[data-node-words]");
    return words === null
      ? text(row.querySelector("[data-node-first-line]"))
      : textWithout(words, `[data-node-trail], [data-child-facts], ${controls}`);
  };
  const rows = [...document.querySelectorAll("[data-issue-page] [data-node-row]")];
  const levels = rows.map((row) => {
    // `readPageRows` runs in the page through `page.evaluate`, one synchronous pass over the DOM
    // that no render can interleave, so this level is the one the row drew at that instant.
    const level = Number(row.getAttribute("aria-level") ?? "");
    return Number.isInteger(level) && level > 0 ? level : null;
  });
  const bodyOf = (row: Element): PageBody | null => {
    const body = row.querySelector("[data-node-body]");
    if (body === null) return null;
    const lines: string[] = [];
    for (const part of body.children) {
      if (part.matches("[data-node-author], [data-node-author-title], dl")) continue;
      if (part.matches(controls) || part.querySelector(controls) !== null) continue;
      const line = text(part);
      if (line !== null && line !== "") lines.push(line);
    }
    const answer = row.querySelector("[data-node-answer]");
    return {
      answer:
        answer === null
          ? null
          : {
              author: text(answer.querySelector("[data-answer-author]")),
              choices: [...answer.querySelectorAll("[data-answer-choice]")].map(
                (choice) => text(choice) ?? ""
              ),
              text: answer.querySelector("[data-answer-text]")?.textContent ?? null,
            },
      author: text(
        body.querySelector(":scope > [data-node-author], :scope > [data-node-author-title]")
      ),
      facts: [...body.querySelectorAll(":scope > dl > dt")].map((label) => ({
        name: text(label),
        value: textWithout(label.nextElementSibling, controls) ?? "",
      })),
      lines,
      options: [...row.querySelectorAll("[data-node-options] > li")].map((option) => ({
        chosen: option.getAttribute("data-node-option") === "chosen",
        words: textWithout(option, "[aria-hidden='true']") ?? "",
      })),
      words: wordsOf(row),
    };
  };
  const read = (row: Element, index: number, withBody: boolean): PageRow => {
    const margin = row.querySelector("[data-node-margin]");
    const chips = outsideBody(row, "[data-node-chip]").map((chip) => {
      const parts = [...chip.children];
      const name = parts.at(-1);
      const label = parts
        .slice(0, -1)
        .map((part) => text(part) ?? "")
        .join(" ")
        .trim();
      return {
        address: name?.matches("a") === true ? name.getAttribute("href") : null,
        label: label === "" ? null : label,
        name: text(name) ?? "",
      };
    });
    // Read in the same synchronous pass over the DOM as every other field of the row, so the
    // state it records is the one the row drew at the instant the whole reading stands for.
    const token = row.getAttribute("data-node-state");
    const word = text(margin?.querySelector("[data-node-state]"));
    const unsure = text(margin?.querySelector("[data-node-unsure]"));
    return {
      alsoUnder: chips
        .filter((chip) => chip.label === alsoUnder)
        .map((chip) => ({ address: chip.address, name: chip.name })),
      author: text(margin?.querySelector("[data-node-author]")),
      body: withBody ? bodyOf(row) : null,
      chips: chips.filter((chip) => chip.label !== alsoUnder),
      counts: outsideBody(row, "[data-node-count]").map((count) => text(count) ?? ""),
      facts: outsideBody(row, "[data-child-facts]").map((facts) => ({
        name: null,
        value: text(facts) ?? "",
      })),
      gaps: outsideBody(row, "[data-gap]").map((gap) => ({
        kind: gap.getAttribute("data-gap-kind") ?? "",
        word: text(gap) ?? "",
      })),
      id: row.getAttribute("data-node-id"),
      kind: {
        tag: text(margin?.querySelector("[data-node-tag]")),
        type: row.getAttribute("data-node-type"),
      },
      level: levels[index] ?? null,
      rowKey: row.getAttribute("data-row-key"),
      state: token === null && word === null && unsure === null ? null : { token, unsure, word },
      time: text(margin?.querySelector("[data-node-time]")),
      words: wordsOf(row),
    };
  };
  if (focused === null) return rows.map((row, index) => read(row, index, false));
  const row = rows[focused];
  return row === undefined ? [] : [read(row, focused, true)];
}

/** A string a seed returns that can name a node: an id, a key, a block id or a slug. A seed's
 *  own words hold spaces, and name nothing. */
const ID_SHAPE = /^\S+$/;

/**
 * Each id the seed returned, by the path of its field in what the seed returned:
 * `requirements[1]`, `comments.threads[0]`, `openQuestion.askId`. The walk reads every string of
 * the object and of the objects and lists inside it, each object's fields in sorted order, so
 * the paths do not depend on the order the seed wrote its fields in.
 *
 * One id may stand in several fields: a list of every check and a field for the one that passed,
 * a list of the children and a field for the blocked one, the list of the requirements and the
 * field for the one a comment is anchored to. The id is named by its place in a list where a
 * list holds it, since a list is the seed's account of the nodes it wrote and a single field
 * says what a node is for; among lists, or among single fields, by the first path of the walk.
 *
 * A block of a document is drawn under the node id `<slug>#b-<block id>` (`blockNode`), so
 * each id is named in that form too for each of `slugs`, the slugs of the issue's documents
 * (the seeds of `e2e/issue-layout-seed.ts` return the documents' ids and not their slugs). An id
 * no document holds a block of gets names no row draws, which name nothing.
 */
export function seedKeys(seeded: object, slugs: readonly string[]): ReadonlyMap<string, string> {
  const found: { readonly id: string; readonly listed: boolean; readonly path: string }[] = [];
  const walk = (value: unknown, path: string, listed: boolean): void => {
    if (typeof value === "string") {
      if (ID_SHAPE.test(value)) found.push({ id: value, listed, path });
    } else if (Array.isArray(value)) {
      for (const [index, item] of value.entries()) walk(item, `${path}[${index}]`, true);
    } else if (typeof value === "object" && value !== null) {
      const fields: Readonly<Record<string, unknown>> = { ...value };
      for (const field of Object.keys(fields).sort()) {
        walk(fields[field], path === "" ? field : `${path}.${field}`, listed);
      }
    }
  };
  walk(seeded, "", false);
  const keys = new Map<string, string>();
  for (const { id, path } of [
    ...found.filter((one) => one.listed),
    ...found.filter((one) => !one.listed),
  ]) {
    for (const form of [id, ...slugs.map((slug) => blockNode(slug, id))]) {
      if (!keys.has(form)) keys.set(form, path);
    }
  }
  return keys;
}

/** Where a row's time comes from: when its node was made, or when a decision last changed. */
export type TimeSource = "changed" | "made";

/** The source of a drawn time, by the kind of row (the plan's "The tree read's members":
 *  `made_at` for an ask, a comment, a message and a result, and the decisions read's
 *  `changed_at` for a decision). The page draws no time for a document block or a child issue. */
const TIME_SOURCES: Readonly<Partial<Record<string, TimeSource>>> = {
  ask: "made",
  comment: "made",
  decision: "changed",
  message: "made",
  result: "made",
};

/** A row by the names `keyedRows` gives: no id the server minted, and its time by its source. */
export interface KeyedRow {
  readonly alsoUnder: readonly RowParent[];
  readonly author: string | null;
  readonly body: RowBody | null;
  readonly chips: readonly RowChip[];
  readonly counts: readonly string[];
  readonly facts: readonly RowFact[];
  readonly gaps: readonly RowGap[];
  /** What the seed calls the row's node, else the row's path. */
  readonly key: string;
  readonly kind: RowKind;
  readonly level: number | null;
  /** The key of the row this one is drawn under, null at the top. */
  readonly parentKey: string | null;
  readonly state: RowState | null;
  /** The source of the time the row draws, null where it draws none. */
  readonly time: TimeSource | null;
  readonly words: string | null;
}

/**
 * Names each reading: by what `keys` (`seedKeys`) calls its node, and a row whose node no field
 * names by its row path, `<parent key>/<kind>#<n>`, the `n`th (from 0) of the rows of that kind
 * under the same parent row; a row at the top has an empty parent key, so its path starts with
 * `/`. A node drawn under two parents (a check that serves two requirements) is two rows with one
 * key, so a row is named by its key and its parent's key together. The parent is the nearest row
 * before it one level up (`parentIndex`), so a view's rows, which draw no `data-row-key`, are
 * named the same way.
 *
 * What a row reads that names a node by its id is written with the node's key in its place,
 * `{<key>}`: each chip's and other parent's address, the tag (a child's is its key) and a child's
 * facts, which name its blockers. The row's own id, key and parent's key are dropped, since they
 * are made of ids, and the time is written as its source. A row that draws a time on a kind no
 * source is known for fails the naming rather than write the instant.
 */
export function keyedRows(
  readings: readonly RowReading[],
  keys: ReadonlyMap<string, string>
): KeyedRow[] {
  const ids = idPattern(keys);
  const keyedText = (text: string): string =>
    ids === undefined ? text : text.replace(ids, (id) => `{${keys.get(id) ?? id}}`);
  const keyed: KeyedRow[] = [];
  const ordinals = new Map<string, number>();
  for (const [index, reading] of readings.entries()) {
    const parent = parentIndex(readings, index);
    const parentKey = parent === undefined ? null : (keyed[parent]?.key ?? null);
    const kind = reading.kind.type ?? "node";
    const siblings = `${parent ?? -1} ${kind}`;
    const ordinal = ordinals.get(siblings) ?? 0;
    ordinals.set(siblings, ordinal + 1);
    const named = reading.id === null ? undefined : keys.get(reading.id);
    const key = named ?? `${parentKey ?? ""}/${kind}#${ordinal}`;
    keyed.push({
      alsoUnder: reading.alsoUnder.map((other) => ({
        ...other,
        address: other.address === null ? null : keyedText(other.address),
      })),
      author: reading.author,
      body: reading.body,
      chips: reading.chips.map((chip) => ({
        ...chip,
        address: chip.address === null ? null : keyedText(chip.address),
      })),
      counts: reading.counts,
      facts: reading.facts.map((fact) =>
        fact.name === null ? { ...fact, value: keyedText(fact.value) } : fact
      ),
      gaps: reading.gaps,
      key,
      kind: {
        tag: reading.kind.tag === null ? null : keyedText(reading.kind.tag),
        type: reading.kind.type,
      },
      level: reading.level,
      parentKey,
      state: reading.state,
      time: timeSourceOf(reading, key),
      words: reading.words,
    });
  }
  return keyed;
}

/** The place of the row `index` is drawn under: the nearest row before it one level up. */
export function parentIndex(readings: readonly RowReading[], index: number): number | undefined {
  const level = readings[index]?.level ?? null;
  if (level === null) return undefined;
  for (let before = index - 1; before >= 0; before -= 1) {
    if (readings[before]?.level === level - 1) return before;
  }
  return undefined;
}

/** Every id `keys` names, as a whole word of an address: not inside a longer id, except after
 *  `#b-`, where a block's id follows its document's slug. The longest first, so an id is never
 *  taken for a shorter one it starts with. */
function idPattern(keys: ReadonlyMap<string, string>): RegExp | undefined {
  const ids = [...keys.keys()].sort((a, b) => b.length - a.length || (a < b ? -1 : a > b ? 1 : 0));
  if (ids.length === 0) return undefined;
  const alternatives = ids.map((id) => id.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|");
  return new RegExp(`(?:(?<![\\w-])|(?<=#b-))(?:${alternatives})(?![\\w-])`, "g");
}

/** The source of the time `reading` draws, or null where it draws none. */
function timeSourceOf(reading: RowReading, key: string): TimeSource | null {
  if (reading.time === null) return null;
  const source = reading.kind.type === null ? undefined : TIME_SOURCES[reading.kind.type];
  if (source === undefined) {
    throw new Error(
      `the row ${key} draws a time on a ${reading.kind.type ?? "row of no kind"}, and no read ` +
        "it draws a time from is known for that kind, so the time cannot be written as its source"
    );
  }
  return source;
}

// ---------------------------------------------------------------------------------------------
// The seeds and the fixture the rows compare with, and the steady reading both read by.
// ---------------------------------------------------------------------------------------------

/** What every seed returns: the key of the issue the page opens. */
export interface SeededIssue {
  readonly key: string;
}

/** A seed, by the name the fixture records it under. */
export type NamedSeed = readonly [string, () => Promise<SeededIssue>];

/** The 14 seeds of `e2e/issue-layout-seed.ts`, in the order that file writes them: what the
 *  recorder records, and what the rows read. */
export const SEEDS: readonly NamedSeed[] = Object.entries({
  seedWholeIssue,
  seedWholeIssueAdvancing,
  seedSpineCases,
  seedNoChildren,
  seedNoSummary,
  seedReplyNesting,
  seedUnbrokenWords,
  seedUnbrokenChildTitle,
  seedRequestBaseline,
  seedTwiceTheNodes,
  seedConversationState,
  seedDecisionsFilter,
  seedReviewList,
  seedPlanOrder,
} satisfies Readonly<Record<string, () => Promise<SeededIssue>>>);

/** One filter of a seed in the fixture: the keys of the rows it drew, and its control's count. */
export interface FilterFixture {
  readonly count: number;
  readonly rows: readonly string[];
}

/** One seed in the fixture `e2e/tools/issue-page-t-record.ts` writes. */
export interface SeedFixture {
  readonly filters: Readonly<Partial<Record<TreeView, FilterFixture>>>;
  readonly loadRequests: number;
  readonly opened: readonly KeyedRow[];
  readonly rows: readonly KeyedRow[];
}

/** The part of the fixture the rows read. */
interface RowsFixture {
  readonly seeds: Readonly<Partial<Record<string, SeedFixture>>>;
}

/** The fixture `e2e/tools/issue-page-t-record.ts` writes and the rows read. */
export const FIXTURE_FILE = new URL("./fixtures/issue-page-t-rows.json", import.meta.url);

let rowsFixture: RowsFixture | undefined;

/** What the fixture recorded of `seed`. The file is read, never imported: Playwright loads a spec
 *  under Node, which refuses a JSON import that names no `type: json`, and one such import stops
 *  the listing of the whole suite. */
export function fixtureSeed(seed: string): SeedFixture {
  rowsFixture ??= JSON.parse(readFileSync(FIXTURE_FILE, "utf8")) as RowsFixture;
  const recorded = rowsFixture.seeds[seed];
  if (recorded === undefined) throw new Error(`the fixture records no seed named ${seed}`);
  return recorded;
}

/** `value` with every object's fields in sorted order at every depth, so the bytes of the fixture
 *  do not depend on the order a reader or a seed wrote its fields in, and two readings compare by
 *  what they hold. A list keeps its order, which is the page's. */
export function sortedFields(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortedFields);
  if (typeof value !== "object" || value === null) return value;
  const fields: Readonly<Record<string, unknown>> = { ...value };
  return Object.fromEntries(
    Object.keys(fields)
      .sort()
      .map((field) => [field, sortedFields(fields[field])])
  );
}

/** How many readings `steadily` takes before it gives up on the page settling. */
const STEADY_TRIES = 20;

/**
 * Reads `read` until two readings in a row are the same, as the recorder read what the fixture
 * holds: before each reading the page goes quiet (`expectLoadSettled`), and after it two frames
 * pass, so a row or a count that is still arriving is not read half drawn. A page that never
 * settles fails the row.
 */
export async function steadily<T>(
  page: Page,
  traffic: ApiRequests,
  what: string,
  read: () => Promise<T>
): Promise<T> {
  let last: string | undefined;
  for (let tries = 0; tries < STEADY_TRIES; tries += 1) {
    await expectLoadSettled(traffic);
    const reading = await read();
    const said = JSON.stringify(reading);
    if (said === last) return reading;
    last = said;
    await nextFrames(page);
  }
  throw new Error(`${what} changed at each of ${STEADY_TRIES} readings, so it never settled`);
}

// ---------------------------------------------------------------------------------------------
// What the page received of the tree read and its views, and what a load or a press sent.
// ---------------------------------------------------------------------------------------------

/** One of the four filters, by the `view` its read of the tree sends (`presets.ts`' `Filter`). */
export type TreeView = Filter;

/** The tree read's path, whatever issue it reads. */
const TREE_PATH = /^\/api\/v1\/issues\/[^/]+\/tree$/;

/** The path of issue `key`'s tree read. */
function treePath(key: string): string {
  return `/api/v1/issues/${encodeURIComponent(key)}/tree`;
}

/** Matches every request of issue `key`'s tree read, with or without a query: the whole tree, a
 *  view of it (`?view=`) and each next page (`?cursor=`). One function per call, so a row that
 *  routes a hold through it unroutes the same one. */
export function treeReadRoute(key: string): (url: URL) => boolean {
  const path = treePath(key);
  return (url) => url.pathname === path;
}

/** What `refuseTreeRead` hands its row, to count its refusals and to end them. */
export interface RefusedTreeRead {
  /** Takes the refusal off, so the tree read reaches the server again. */
  lift(): Promise<void>;
  /** How many requests of the tree read have been refused so far, the page's own retries
   *  included. */
  refusals(): number;
  /** Settles once `count` requests of the tree read have been refused in all, and not before. */
  refused(count: number): Promise<void>;
}

/**
 * Refuses issue `key`'s tree read (`treeReadRoute`), with a 500 in the server's error shape, from
 * the next request the page sends until `lift`, as many times as the page asks it: what a row
 * reads of a view whose documents' blocks the tree read answers when that read fails. A refusal
 * is counted once its answer has been sent, so a row that waits on `refused(n)` reads a page that
 * has had its nth refusal.
 */
export async function refuseTreeRead(page: Page, key: string): Promise<RefusedTreeRead> {
  const route = treeReadRoute(key);
  let refusals = 0;
  // Each wait a row asked for, by the count it waits on: the refusal that reaches a wait's count
  // is the one that settles it. The package compiles against ES2022, whose library names no type
  // for what `Promise.withResolvers` returns, so the two parts used are named here.
  const waiting = new Map<
    number,
    { readonly promise: Promise<void>; readonly resolve: () => void }
  >();
  const refuse = async (request: Route) => {
    await request.fulfill({ json: { code: "INTERNAL", error: "boom" }, status: 500 });
    refusals += 1;
    waiting.get(refusals)?.resolve();
    waiting.delete(refusals);
  };
  await page.route(route, refuse);
  return {
    lift: () => page.unroute(route, refuse),
    refusals: () => refusals,
    refused: (count) => {
      if (refusals >= count) return Promise.resolve();
      const wait = waiting.get(count) ?? Promise.withResolvers<void>();
      waiting.set(count, wait);
      return wait.promise;
    },
  };
}

/** What the page received of the tree route, for one view or none. */
export interface TreeAnswerWatch {
  /** The newest read whose every page has answered, its pages merged; undefined until one has. */
  answer(): IssueTreeAnswer | undefined;
}

/** One page of a read as the page received it: in which place it arrived, the cursor its request
 *  sent (none for a read's first page), and the page. */
interface ReceivedPage {
  readonly arrived: number;
  readonly cursor: string | null;
  readonly page: IssueTreeAnswer;
}

/** Keeps every answer of the tree route whose `view` is `view` (none for null), from now on. */
function watchTreeRoute(page: Page, view: TreeView | null): TreeAnswerWatch {
  const received: ReceivedPage[] = [];
  let arrivals = 0;
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (
      response.request().method() !== "GET" ||
      !TREE_PATH.test(url.pathname) ||
      url.searchParams.get("view") !== view ||
      !response.ok()
    ) {
      return;
    }
    const arrived = arrivals;
    arrivals += 1;
    const cursor = url.searchParams.get("cursor");
    void response.json().then(
      (body: IssueTreeAnswer) => {
        received.push({ arrived, cursor, page: body });
      },
      // A body the browser let go of before it was read, as the page moved on, is no answer.
      () => undefined
    );
  });
  return { answer: () => newestRead(received) };
}

/** The newest read every page of which has answered: a read starts at a page its request sent no
 *  cursor for, and goes on with each page whose cursor the page before it answered. */
function newestRead(received: readonly ReceivedPage[]): IssueTreeAnswer | undefined {
  const reads: IssueTreeAnswer[][] = [];
  for (const { cursor, page } of [...received].sort((a, b) => a.arrived - b.arrived)) {
    const read = reads[reads.length - 1];
    if (cursor === null) reads.push([page]);
    else if (read !== undefined && read[read.length - 1]?.cursor === cursor) read.push(page);
  }
  for (let at = reads.length - 1; at >= 0; at -= 1) {
    const [first, ...rest] = reads[at] ?? [];
    if (first !== undefined && (rest[rest.length - 1] ?? first).cursor === null) {
      return mergeTreePages([first, ...rest]);
    }
  }
  return undefined;
}

/** The tree read the page receives with no view, every page of its newest read merged. Start it
 *  before the navigation, so the first load's read is kept. */
export function treeAnswerOf(page: Page): TreeAnswerWatch {
  return watchTreeRoute(page, null);
}

/** `treeAnswerOf` for the read of one view, `?view=<filter>`. */
export function viewAnswerOf(page: Page, filter: TreeView): TreeAnswerWatch {
  return watchTreeRoute(page, filter);
}

/** Waits until `watch` holds a read whose every page answered, and answers it. */
export async function heldAnswer(watch: TreeAnswerWatch, what: string): Promise<IssueTreeAnswer> {
  const message = `${what} never answered its last page`;
  await expect.poll(() => watch.answer() !== undefined, { message }).toBe(true);
  const answer = watch.answer();
  if (answer === undefined) throw new Error(message);
  return answer;
}

/** Root R2's rows of each filter: the kinds a view draws as rows of its own, the nodes its query
 *  took, and as context, the nodes it holds to place them. Spec draws the statements;
 *  Verification each statement as context, with the checks under it and their results; Plan the
 *  plan tasks; Decisions the questions and the decisions. */
export const R2_ROW_KINDS: Readonly<
  Record<TreeView, { readonly context: readonly string[]; readonly query: readonly string[] }>
> = {
  decisions: { context: [], query: ["ask", "decision"] },
  plan: { context: [], query: ["plan_task"] },
  spec: { context: [], query: ["requirement"] },
  verification: { context: ["requirement"], query: ["check", "result"] },
};

/** The main kind each filter's control counts (R2). */
export const R2_COUNTED_KINDS: Readonly<Record<TreeView, readonly string[]>> = {
  decisions: ["ask", "decision"],
  plan: ["plan_task"],
  spec: ["requirement"],
  verification: ["check"],
};

/** A node a view draws as a row, by the id its row carries, and how its view marks it. */
export interface ViewNodeId {
  readonly id: string;
  readonly mark: "context" | "query";
}

/**
 * The rows root R2 lets `filter` draw of its view read `answer`: each node of this issue of the
 * filter's own kinds that the view's query took, and each of its context kinds the view holds as
 * context, by the id `treeIdOf` gives it (the id its row carries), once each, in the answer's
 * order. `reads` are the issue read and the asks read, which `treeIdOf` names an issue's
 * documents and an ask's block by.
 */
export function viewNodeIds(
  answer: IssueTreeAnswer,
  filter: TreeView,
  reads: TreeIdReads
): ViewNodeId[] {
  const idOf = treeIdOf(answer, reads);
  const kinds = R2_ROW_KINDS[filter];
  const named = new Set<string>();
  return answer.nodes.flatMap((node): ViewNodeId[] => {
    const mark = node.mark === "query" ? "query" : "context";
    const foreign = node.issue !== undefined && node.issue !== reads.issue.key;
    const id = idOf(node.id);
    if (foreign || !kinds[mark].includes(node.supertag) || named.has(id)) return [];
    named.add(id);
    return [{ id, mark }];
  });
}

/** What `filter`'s control counts of its view read `answer`: the nodes of its main kind the
 *  view's query took, each once (root R2). */
export function viewCount(answer: IssueTreeAnswer, filter: TreeView): number {
  const kinds = R2_COUNTED_KINDS[filter];
  return new Set(
    answer.nodes.flatMap((node) =>
      node.mark === "query" && kinds.includes(node.supertag) ? [node.id] : []
    )
  ).size;
}

/** How many reads of issue `key`'s tree `requests` hold for `view` (none for null): each read's
 *  first page, a request that sends no cursor, since a read asks for its next pages itself. */
export function treeReads(
  requests: readonly ApiRequest[],
  key: string,
  view: TreeView | null
): number {
  return requests.filter((request) => {
    const url = new URL(request.path, "http://requests.invalid");
    return (
      request.method === "GET" &&
      url.pathname === treePath(key) &&
      url.searchParams.get("view") === view &&
      !url.searchParams.has("cursor")
    );
  }).length;
}

/** Every request of `requests` that is no page of a read of issue `key`'s view `view`, as its
 *  method and path: what a press sent beside its view's own read. */
export function besideViewRead(
  requests: readonly ApiRequest[],
  key: string,
  view: TreeView
): string[] {
  return requests.flatMap((request) => {
    const url = new URL(request.path, "http://requests.invalid");
    const own =
      request.method === "GET" &&
      url.pathname === treePath(key) &&
      url.searchParams.get("view") === view;
    return own ? [] : [`${request.method} ${request.path}`];
  });
}

/** The reads of a document's blocks (`GET /api/v1/artifacts/{id}/blocks`) `requests` hold, by
 *  path. */
export function blocksReads(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) =>
    request.method === "GET" && request.route === "/api/v1/artifacts/:id/blocks"
      ? [request.path]
      : []
  );
}

/** The decisions read the page received: today's (`GET /api/v1/issues/{key}/decisions`), or once
 *  the model moves it onto the tree read, the tree read's view `?view=decisions`. */
export type DecisionsRead =
  | { readonly answer: IssueDecisions; readonly kind: "decisions" }
  | { readonly answer: IssueTreeAnswer; readonly kind: "view" };

/** What the page received of its decisions read. */
export interface DecisionsReadWatch {
  /** The newest answer, the view's where the page read the view; undefined before one came. */
  read(): DecisionsRead | undefined;
}

/** Keeps the decisions read of issue `key` the page receives from now on, in either form. */
export function decisionsReadOf(page: Page, key: string): DecisionsReadWatch {
  const view = viewAnswerOf(page, "decisions");
  const path = `/api/v1/issues/${encodeURIComponent(key)}/decisions`;
  let newest: IssueDecisions | undefined;
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (response.request().method() !== "GET" || url.pathname !== path || !response.ok()) return;
    void response.json().then(
      (body: IssueDecisions) => {
        newest = body;
      },
      () => undefined
    );
  });
  return {
    read: () => {
      const answer = view.answer();
      if (answer !== undefined) return { answer, kind: "view" };
      return newest === undefined ? undefined : { answer: newest, kind: "decisions" };
    },
  };
}

/** When each decision of `read` last changed (`changed_at`), by every id a row may draw it by:
 *  its block's id alone, and `<slug>#b-<block id>` for each of `slugs`, the slugs of the issue's
 *  documents; for a decision node of a view, its own id and its document's form of it. */
export function decisionTimes(
  read: DecisionsRead | undefined,
  slugs: readonly string[]
): ReadonlyMap<string, string> {
  const times = new Map<string, string>();
  const at = (block: string, own: readonly string[], changed: unknown): void => {
    if (typeof changed !== "string") return;
    for (const id of [block, ...own, ...slugs.map((slug) => blockNode(slug, block))]) {
      times.set(id, changed);
    }
  };
  if (read?.kind === "decisions") {
    for (const decision of read.answer.decisions ?? []) {
      at(decision.block_id, [], decision.changed_at);
    }
  } else if (read?.kind === "view") {
    for (const node of read.answer.nodes) {
      if (node.supertag !== "decision") continue;
      const own = node.document === undefined ? [] : [blockNode(node.document.slug, node.id)];
      at(node.id, own, node.changed_at);
    }
  }
  return times;
}

/** Whether the seed's decisions are nodes of the tree read `answer`: it holds a decision node of
 *  this issue, and every decision the decisions read answers is one - a decision node of this
 *  issue whose id is the decision's `block_id`, in the document the decision's `ref` names where
 *  the node names its document. The check is this file's own, so the page's matching of
 *  decisions to nodes can move under it. */
export function decisionsAreNodes(
  answer: IssueTreeAnswer,
  read: DecisionsRead | undefined
): boolean {
  const own = new Map(
    answer.nodes.flatMap((node) =>
      node.supertag === "decision" && node.issue === undefined ? [[node.id, node] as const] : []
    )
  );
  if (own.size === 0 || read === undefined) return false;
  if (read.kind === "view") return true;
  return (read.answer.decisions ?? []).every((decision) => {
    const node = own.get(decision.block_id);
    if (node === undefined) return false;
    if (node.document === undefined) return true;
    const route = parseNodeReference(decision.ref);
    return route?.kind === "artifact" && route.slug === node.document.slug;
  });
}

// ---------------------------------------------------------------------------------------------
// The gates: a row, or a clause in a row of its own, whose gate is not on this branch's base
// skips.
// ---------------------------------------------------------------------------------------------

/** One gate a row waits on: whether its probe found it on this branch's base, and what the row
 *  owes while it is not, naming the gate, what the probe read and how an owed row counts. */
export interface Gate {
  readonly met: boolean;
  readonly owed: string;
}

/** How every owed row counts, which its `owed` annotation says after naming its gate: a row cannot
 *  pass on code the base does not hold yet, and a failure there would say nothing of the page. */
export const OWED_BY = "a row whose gate is not on the base counts as skipped, never as passed";

/**
 * Goes on where `gate` is met; where it is not, records an `owed` annotation naming it and skips
 * the row, which then counts as skipped and never as passed (`OWED_BY` says why). Call it
 * first thing a gate can be read, before the row asserts anything it waits on.
 */
export function owedUnless(gate: Gate): void {
  if (gate.met) return;
  test.info().annotations.push({ description: gate.owed, type: "owed" });
  test.skip(true, gate.owed);
}

/**
 * Waits until every event of the seed has landed: the workspace's event head, which the sign-in
 * check names (`event_head`), reads the same twice a second apart. A first load opened before
 * then would hear the seed's last events through the live feed and read the tree again for them,
 * which a row's count of one tree read would take for the page's own. Read through the signed-in
 * page's own request context.
 */
export async function seedEventsLanded(page: Page): Promise<void> {
  let last: string | undefined;
  await expect
    .poll(
      async () => {
        const response = await page.request.get("/auth/whoami");
        const answer: unknown = await response.json();
        const head =
          answer !== null &&
          typeof answer === "object" &&
          "event_head" in answer &&
          typeof answer.event_head === "string"
            ? answer.event_head
            : undefined;
        const still = head !== undefined && head === last;
        last = head;
        return still;
      },
      {
        intervals: [1_000],
        message: "the workspace's event head was still moving after the seed",
        timeout: 30_000,
      }
    )
    .toBe(true);
}

/** The model's tree read, which answers 404 for issue `key` while the route is absent. Read
 *  through the signed-in page's own request context. */
export async function treeReadGate(page: Page, key: string): Promise<Gate> {
  const response = await page.request.get(treePath(key));
  return {
    met: response.status() !== 404,
    owed:
      "owed until the model's tree read is on this branch's base: the route answers 404 here; " +
      OWED_BY,
  };
}

/** The model's issues born converted: issue `key`'s tree read says it is one. */
export async function convertedGate(page: Page, key: string): Promise<Gate> {
  const path = treePath(key);
  const response = await page.request.get(path);
  expect(response.ok(), `GET ${path} answers ${response.status()}`).toBe(true);
  const { converted } = (await response.json()) as IssueTreeAnswer;
  return {
    met: converted === true,
    owed:
      "owed until the model's issues born converted are on this branch's base: the tree read " +
      `answers converted: false here; ${OWED_BY}`,
  };
}

/** The model's decisions read moved onto the tree read: what the page sent, `requests`, holds a
 *  read of issue `key`'s tree with `view=decisions`. */
export function decisionsViewGate(requests: readonly ApiRequest[], key: string): Gate {
  return {
    met: treeReads(requests, key, "decisions") > 0,
    owed:
      "owed until the model's decisions view of the tree read is on this branch's base: the " +
      `page reads its decisions here with no view=decisions read of the tree; ${OWED_BY}`,
  };
}

/** What `decisionNodesGate` and `partUGate` read where they are not met: the seed's decisions,
 *  written as decision blocks of the spec, are no nodes of the tree read this base answers. */
const DECISIONS_ARE_NO_NODES =
  "the seed's decisions are no nodes of the tree read this base answers";

/** The decisions work that makes each decision a node of the tree read (`decisionsAreNodes`). */
export function decisionNodesGate(answer: IssueTreeAnswer, read: DecisionsRead | undefined): Gate {
  return {
    met: decisionsAreNodes(answer, read),
    owed:
      "owed until decisions are nodes of the tree read on this branch's base: " +
      `${DECISIONS_ARE_NO_NODES}; ${OWED_BY}`,
  };
}

/** The page drawing the decisions from the tree read's nodes, which waits on the decisions work
 *  that makes them nodes reaching this branch's base (`decisionsAreNodes`). */
export function partUGate(answer: IssueTreeAnswer, read: DecisionsRead | undefined): Gate {
  return {
    met: decisionsAreNodes(answer, read),
    owed:
      "owed until the page draws the decisions from the tree read's nodes, once decisions are " +
      `nodes of the tree read on this branch's base: ${DECISIONS_ARE_NO_NODES}; ${OWED_BY}`,
  };
}

/** How many frames the page's writer looks for its line in after a Reply (`outline-writer.tsx`:
 *  ten after the first), and one more, as the rows of `e2e/issue-page-g.e2e.ts` wait them out. */
const WRITER_FRAMES = 12;

/** The line at the foot of the outline, which a row's Reply puts the caret in: once a Reply was
 *  pressed, the focus is on a field the reader types in, inside the page. The work that draws
 *  the line is not on this branch's base (`slots/foot.tsx` draws nothing until it is). */
export async function footLineGate(page: Page): Promise<Gate> {
  for (let passed = 0; passed < WRITER_FRAMES; passed += 2) await nextFrames(page);
  const met = await page.evaluate(() => {
    const focused = document.activeElement;
    return (
      focused instanceof HTMLElement &&
      focused.closest("[data-issue-page]") !== null &&
      (focused.isContentEditable ||
        focused.matches("textarea, input:not([type]), input[type=text]"))
    );
  });
  return {
    met,
    owed:
      "owed until the line at the foot of the outline is on this branch's base: a Reply puts " +
      `the caret in no line here; ${OWED_BY}`,
  };
}

// ---------------------------------------------------------------------------------------------
// What the page draws beside the rows.
// ---------------------------------------------------------------------------------------------

/** The status control of the page's header (`header[data-issue-header]`), the one place the
 *  new page draws the issue's own status: the outline draws no row for the issue. */
export function headerStatus(page: Page): Locator {
  return issueHeader(page).getByRole("combobox", { exact: true, name: "Status" });
}

/** One node a filter draws: a row, or a check's result inside its row (the Verification view). */
export interface DrawnNode {
  /** Whether it is drawn as context (`data-node-context`). */
  readonly context: boolean;
  readonly id: string;
  readonly type: string;
}

/** Every node the page draws with its id and its kind (`[data-node-id][data-node-type]`), in
 *  drawing order: each row, and each result the Verification view draws inside its check's row. */
export function drawnNodes(page: Page): Promise<DrawnNode[]> {
  return page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>("[data-issue-page] [data-node-id][data-node-type]"),
      (element) => ({
        context: element.hasAttribute("data-node-context"),
        id: element.dataset.nodeId ?? "",
        type: element.dataset.nodeType ?? "",
      })
    )
  );
}
