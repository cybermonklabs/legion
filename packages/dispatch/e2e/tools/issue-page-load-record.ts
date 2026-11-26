/** `bun e2e/tools/issue-page-load-record.ts --recorded-on <commit> [--part <commit> ...] [<log> ...]`:
 *  writes `e2e/fixtures/issue-page-load-rows.json`, the record of the rows the base draws, which
 *  the rows of `c-same-page` in `e2e/issue-page-load-same.e2e.ts` compare the new issue page with
 *  once its load has ended.
 *
 *  What it records. For each view of `LOAD_VIEWS` (`e2e/issue-page-load.ts`) that the base's view
 *  row holds, at 1440 by 900 (the `chromium` project) and at 390 by 844 (the `iphone` project,
 *  with the phone's emulation), on the load rows' worker seed (`seedLargestShape`), in one load of
 *  the view's own address read once the load has ended (`waitForLoadEnd`):
 *
 *  - every row, in drawing order (`LOAD_ROW_SELECTOR`: a view's rows and the children's
 *    section's), each with its key (`data-row-key`; for a filter's row, which draws none, its
 *    block's node id; for a child's entry, the child's key), the key of the row it stands under
 *    (the nearest row before it one `aria-level` up), its level, its address (`rowAddress` of
 *    `e2e/issue-page-load.ts`, for an ask, a comment, a message or a block), its words (its
 *    `textContent`, without the rows inside it, each of which is read as a row of its own), its
 *    links by `href` in order, its tab stops (each element of it whose `tabIndex` is 0 or more,
 *    in order, by its tag, its `role` and its accessible name), its accessible tree
 *    (`locator.ariaSnapshot()`), the words of it no reader sees (`hidden`: the text of an
 *    element that fails `checkVisibility({ opacityProperty: true, visibilityProperty: true })`,
 *    or that an element from it up to its row clips, an element whose `overflow` is not
 *    `visible` and whose `scrollHeight` is larger than its `clientHeight`), and its place in the
 *    server's order (`serverPlace`, below);
 *  - each control that holds a parent's rows past its first page back (`[data-tree-more]`,
 *    `[data-older-comments]`, `[data-tree-page]`), by the parent it pages and its words;
 *  - the focused row;
 *  - at 1440 px, the entries of the Keys list, each by its part, its words, its keys and whether
 *    it is on.
 *
 *  How a row is named. The seed's own writes are named by what it returned (`seedKeys` of
 *  `e2e/issue-page-t.ts`): `{key}` for the issue, `{children[3]}`, `{posts[12]}`,
 *  `{messages[2]}`, and each document's block by its node id, `spec#b-r3`, the block id the seed
 *  wrote. Three things the seed returns no stable name for are named otherwise: an ask, whose id
 *  it does not return, by the question it wrote (`{ask "Should …?"}`, from the issue's asks
 *  read); a reply, since the seed fills `replies` from posts it writes six at a time, so the list
 *  holds them in the order the server answered, by its post and its place among that post's
 *  replies (`{posts[3]/comment@0}`), which the seed writes one after another; and a day the page
 *  draws (`shortDay`), which is when the server took the write, as `{day}`. Each UUID and each
 *  issue's key a row holds is written by its name wherever it stands: in the row's key, its
 *  address, its words, its links, its tab stops' names and its accessible tree.
 *
 *  What it refuses. A record that still holds anything one run minted, which the next would write
 *  otherwise: a UUID, an instant or a time of day, or an issue's key the seed did not return. And
 *  two rows with one key under one parent, which a comparison by key could not tell apart.
 *
 *  What two seeds may differ in (`viewDifferences`). The record is made on one seed and the page
 *  read on another, and `seedLargestShape` writes the children, its own asks and the posts six at
 *  a time (`inBursts`): the server gives those writes their times and keys in the order it took
 *  them, which another run does not repeat. The page draws siblings in the read's order, which
 *  follows those times and keys, so two seeds are not the same data in exactly those places, and
 *  the comparison lets exactly two things differ there:
 *
 *  1. the order of two rows under one parent that the server orders by the same measure (when it
 *     took the write, or the number of the key it gave) where the two seeds' server order of
 *     them differs, or ties: each row carries its rank among its siblings by that measure
 *     (`serverPlace`), and every other pair of rows must stand in the base's order;
 *  2. under a parent whose rows past its first page are held back (`CHILD_PAGE` of
 *     `tree-fold.ts`), which of its server-ordered children stand at the page's edge, and the
 *     rows under them: the number of rows drawn under that parent before its control and the
 *     control's words must be the base's, so a row more, or a row less, is still a difference.
 *
 *  Two focused rows that hold one place in the server's order under one parent are the same
 *  focus. Everything else is compared exactly: every row the seed places, each paired row's
 *  address, words, links, tab stops, accessible tree and visible words, and the Keys list.
 *
 *  How to make it, at the base, with this branch's test files carried by `--patch`, in a builds
 *  Job: run the row `record the rows the base draws @phone` of `e2e/issue-page-load-same.e2e.ts`
 *  with `ISSUE_PAGE_LOAD_RECORD=1` in both projects,
 *
 *    ISSUE_PAGE_LOAD_RECORD=1 bun run e2e -- e2e/issue-page-load-same.e2e.ts \
 *      --project=chromium --project=iphone -g 'record the rows the base draws'
 *
 *  which prints one `LOAD-RECORD <json>` line for each width, then, from `packages/dispatch`,
 *
 *    bun e2e/tools/issue-page-load-record.ts --recorded-on <base> --part <commit> ... <the log>
 *
 *  with the run's log as a file, or on the standard input where no file is named. `--recorded-on`
 *  names the base the run was made at, and each `--part` the commit the base holds of one part of
 *  the page `main` does not hold; the record lists them sorted. The command checks both widths'
 *  lines and the refusals again, and writes the record with every object's fields sorted, as
 *  Biome formats it, so two runs on one base write the same bytes where their seeds agree.
 *
 *  When to make it again: when this branch takes a new head of part T, or `main` once T is on it,
 *  and whenever a change under the base changes what the page draws on the seed. A record is
 *  never edited by hand.
 */
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { expect, type Page } from "@playwright/test";

import { CONTROL_LABELS } from "../../web/src/features/issue-page/page-words";
import { compareTimestamps } from "../../web/src/lib/timestamps";
import { getIssue, listComments, listIssueAsks, listIssueMessages } from "../api";
import { LOAD_ROW_SELECTOR, rowAddress } from "../issue-page-load";
import { seedKeys, sortedFields } from "../issue-page-t";
import type { LargestShapeSeed } from "../issue-page-t-scale";

// ---------------------------------------------------------------------------------------------
// The record's shape.
// ---------------------------------------------------------------------------------------------

/** A width a record is made and read at: the `chromium` project's and the `iphone` project's. */
export type RecordWidth = 1440 | 390;

/** The widths, in the order the record holds them. */
export const RECORD_WIDTHS: readonly RecordWidth[] = [1440, 390];

/** The record the rows read, which this command writes. */
export const RECORD_FILE = new URL("../fixtures/issue-page-load-rows.json", import.meta.url);

/** What begins the line the record row prints for its width. */
export const RECORD_LINE = "LOAD-RECORD";

/** The variable under which the record row runs; elsewhere it skips. */
export const RECORD_VARIABLE = "ISSUE_PAGE_LOAD_RECORD";

/** Every row the record reads, inside the new page: a view's and the children's section's. */
export const PAGE_ROWS = LOAD_ROW_SELECTOR.split(", ")
  .map((row) => `[data-issue-page] ${row}`)
  .join(", ");

/** One tab stop of a row: an element whose `tabIndex` is 0 or more. */
export interface TabStop {
  /** Its accessible name: what `aria-labelledby` names, else `aria-label`, else its text, else
   *  its `title`. */
  readonly name: string;
  /** Its `role` attribute, null where it sets none and its tag says it. */
  readonly role: string | null;
  readonly tag: string;
}

/** One row as the browser reads it, before it is named (`readView`). */
export interface PageRow {
  /** `locator.ariaSnapshot()` of the row. */
  readonly aria: string;
  /** `data-child-entry`, a child's key, on a row of the children's section. */
  readonly child: string | null;
  /** The words of the row that no reader sees, in order. */
  readonly hidden: readonly string[];
  /** `data-node-id`. */
  readonly id: string | null;
  /** `aria-level`, null on a row that draws none. */
  readonly level: number | null;
  /** The `href` of each link of the row, in order. */
  readonly links: readonly string[];
  /** `data-row-key`, which a filter's row and a child's entry draw none of. */
  readonly rowKey: string | null;
  readonly tabStops: readonly TabStop[];
  /** `data-node-type`. */
  readonly type: string | null;
  /** The row's `textContent` without the rows inside it, its white space made single spaces. */
  readonly words: string;
}

/** A control that holds a parent's rows past its first page back, as the browser reads it. */
export interface PagingReading {
  /** The index of the last row before it, null where none is. */
  readonly after: number | null;
  /** Which control it is: the row after a node's first page of children (`more`), the control
   *  that reads older comments (`older`), or the one that reads the read's next page (`page`). */
  readonly kind: string;
  /** `aria-level`: the level of the rows it holds back. */
  readonly level: number | null;
  readonly words: string;
}

/** One entry of the Keys list. */
export interface KeysEntry {
  readonly enabled: boolean;
  /** The keys it names, as each `kbd` draws them. */
  readonly keys: readonly string[];
  readonly label: string;
  /** The part of the list it stands in. */
  readonly scope: string;
}

/** What the browser reads of one view once its load has ended (`readView`). */
export interface PageView {
  /** The index of the focused row, null where the focus is in none. */
  readonly focus: number | null;
  /** The Keys list's entries at 1440 px, null at 390 px, which draws no Keys. */
  readonly keys: readonly KeysEntry[] | null;
  readonly paging: readonly PagingReading[];
  readonly rows: readonly PageRow[];
}

/** When the server took a write, or the number of the key it gave an issue. */
export type ServerOrder =
  | { readonly at: string; readonly by: "made" }
  | { readonly at: number; readonly by: "key" };

/** What a row is named by: the issue's key, the name of each id the seed returned, and the
 *  server's order of each write the seed made several at a time (`seedNaming`). */
export interface Naming {
  readonly issue: string;
  readonly names: ReadonlyMap<string, string>;
  readonly order: ReadonlyMap<string, ServerOrder>;
}

/** A row's rank among its siblings that the server orders by the same measure, by it: rows the
 *  server took at one time share a rank. */
export interface ServerPlace {
  readonly by: "key" | "made";
  readonly rank: number;
}

/** One row as the record holds it, named. */
export interface RecordedRow {
  readonly address: string | null;
  readonly aria: string;
  readonly hidden: readonly string[];
  readonly key: string;
  /** `data-node-type`, `child` for a child's entry. */
  readonly kind: string;
  readonly level: number | null;
  readonly links: readonly string[];
  /** The key of the row it stands under, null at the top. */
  readonly parentKey: string | null;
  /** Null for a row the seed places itself. */
  readonly serverPlace: ServerPlace | null;
  readonly tabStops: readonly TabStop[];
  readonly words: string;
}

/** A control that holds a parent's rows past its first page back, as the record holds it. */
export interface PagingControl {
  readonly kind: string;
  readonly level: number | null;
  /** The key of the row whose rows it pages, null at the top. */
  readonly parentKey: string | null;
  readonly words: string;
}

/** A row by its key and its parent's. */
export interface RowName {
  readonly key: string;
  readonly parentKey: string | null;
}

/** One view at one width, as the record holds it. */
export interface ViewRecord {
  readonly focus: RowName | null;
  readonly keys: readonly KeysEntry[] | null;
  readonly paging: readonly PagingControl[];
  readonly rows: readonly RecordedRow[];
}

/** What the record row prints for its width: each view the base draws. */
export interface WidthRecord {
  readonly views: Readonly<Record<string, ViewRecord>>;
  readonly width: RecordWidth;
}

/** The record, as this command writes it. */
export interface RowsRecord {
  /** The base it was made at. */
  readonly commit: string;
  /** The commit the base holds of each part of the page `main` does not hold, sorted. */
  readonly parts: readonly string[];
  /** Each view, by its token, at each width. */
  readonly views: Readonly<Record<string, Readonly<Partial<Record<`${RecordWidth}`, ViewRecord>>>>>;
}

// ---------------------------------------------------------------------------------------------
// Naming the rows, and the refusals.
// ---------------------------------------------------------------------------------------------

/** The page's short form of a day (`node-time.ts`'s `shortDay`), which is when the server took a
 *  write, so it is written as its name. */
const DAY = /\b\d{1,2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)(?: \d{4})?\b/g;

/** What a day is written as. */
const DAY_NAME = "{day}";

/** What the record refuses to hold: what one run minted, which the next would write otherwise. */
const MINTED: readonly { readonly pattern: RegExp; readonly what: string }[] = [
  { pattern: /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i, what: "a UUID" },
  { pattern: /\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}/, what: "an instant" },
  { pattern: /(?<![\w:])\d{1,2}:\d{2}(?![\w:])/, what: "a time of day" },
  // An issue's key is `[A-Z][A-Z0-9]{1,9}-<digits>` (`routes.ts`'s `buildIssuePath`), and the
  // server numbers it in the order it takes the writes.
  { pattern: /(?<![\w-])[A-Z][A-Z0-9]{1,9}-\d+(?![\w-])/, what: "an issue's key" },
];

/** The kinds of row an item's own address names (`rowAddress`). */
const ITEM_TYPES: Readonly<Record<string, true>> = { ask: true, comment: true, message: true };

/** Where a row stands, in words: under its parent's key, or at the top. */
function placeWords(parentKey: string | null): string {
  return parentKey === null ? "at the top" : `under ${parentKey}`;
}

/** A row's kind: its node's type, `child` for a child's entry. */
function kindOf(row: PageRow): string {
  return row.child === null ? (row.type ?? "row") : "child";
}

/** The row `index` stands under: the nearest row before it one level up. */
function parentOf(levels: readonly (number | null)[], index: number): number | undefined {
  const level = levels[index] ?? null;
  if (level === null) return undefined;
  for (let before = index - 1; before >= 0; before -= 1) {
    if (levels[before] === level - 1) return before;
  }
  return undefined;
}

/** Which of two writes the server ordered first, by one measure. */
function compareOrder(one: ServerOrder, other: ServerOrder): number {
  if (one.by === "made" && other.by === "made") return compareTimestamps(one.at, other.at);
  if (one.by === "key" && other.by === "key") return one.at - other.at;
  return 0;
}

/** Each row's rank among its siblings the server orders by the same measure. */
function serverPlaces(
  ids: readonly (string | null)[],
  parents: readonly (number | undefined)[],
  order: ReadonlyMap<string, ServerOrder>
): (ServerPlace | null)[] {
  const groups = new Map<string, { readonly index: number; readonly placed: ServerOrder }[]>();
  for (const [index, id] of ids.entries()) {
    const placed = id === null ? undefined : order.get(id);
    if (placed === undefined) continue;
    const group = `${parents[index] ?? -1} ${placed.by}`;
    const members = groups.get(group) ?? [];
    members.push({ index, placed });
    groups.set(group, members);
  }
  const places: (ServerPlace | null)[] = ids.map(() => null);
  for (const members of groups.values()) {
    const sorted = [...members].sort((one, other) => compareOrder(one.placed, other.placed));
    let rank = 0;
    for (const [at, member] of sorted.entries()) {
      const before = sorted[at - 1];
      if (before !== undefined && compareOrder(before.placed, member.placed) !== 0) rank = at;
      places[member.index] = { by: member.placed.by, rank };
    }
  }
  return places;
}

/** What in a text can be an id the server minted: a UUID, or an issue's key, whole. Each is
 *  looked up among the names, so a text is read once however many ids the seed returned. */
const ID_TOKEN =
  /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|(?<![\w-])[A-Z][A-Z0-9]{1,9}-\d+(?![\w-])/g;

/** Writes each id of `names` in a text as `{<its name>}`, and each day as `{day}`. */
function namer(names: ReadonlyMap<string, string>): (text: string) => string {
  return (text) =>
    text
      .replace(ID_TOKEN, (id) => {
        const named = names.get(id);
        return named === undefined ? id : `{${named}}`;
      })
      .replace(DAY, DAY_NAME);
}

/** The address that names a row's node, for an ask, a comment, a message and a block. */
function addressOf(issue: string, row: PageRow): string | null {
  if (row.id === null || row.type === null) return null;
  if (ITEM_TYPES[row.type] !== true && !row.id.includes("#b-")) return null;
  return rowAddress(issue, { id: row.id, type: row.type }) ?? null;
}

/** The rows, named (the head comment's "How a row is named"), and the namer that named them. */
function namedRows(
  rows: readonly PageRow[],
  naming: Naming
): { readonly name: (text: string) => string; readonly rows: RecordedRow[] } {
  const levels = rows.map((row) => row.level);
  const parents = rows.map((_, index) => parentOf(levels, index));
  const drawnIds = rows.map((row) => row.child ?? row.id);
  const places = serverPlaces(drawnIds, parents, naming.order);
  // A write of the server's that the seed returned no name of is named by its parent's node and
  // its place among its siblings in the server's order.
  const names = new Map(naming.names);
  const nodeNames: string[] = [];
  for (const [index, id] of drawnIds.entries()) {
    const parent = parents[index];
    const place = places[index];
    const row = rows[index];
    if (id !== null && row !== undefined && !names.has(id) && place?.by === "made") {
      const above = parent === undefined ? "" : (nodeNames[parent] ?? "");
      names.set(id, `${above}/${kindOf(row)}@${place.rank}`);
    }
    nodeNames.push(id === null ? "" : (names.get(id) ?? id));
  }
  const name = namer(names);
  const keys: string[] = [];
  const ordinals = new Map<string, number>();
  // A row with no key, no node and no child is named by its place: the `n`th of its kind under
  // its parent.
  const placeKey = (parent: number | undefined, parentKey: string | null, kind: string) => {
    const siblings = `${parent ?? -1} ${kind}`;
    const ordinal = ordinals.get(siblings) ?? 0;
    ordinals.set(siblings, ordinal + 1);
    return `${parentKey ?? ""}/${kind}#${ordinal}`;
  };
  const named = rows.map((row, index): RecordedRow => {
    const parent = parents[index];
    const parentKey = parent === undefined ? null : (keys[parent] ?? null);
    const kind = kindOf(row);
    const own = row.rowKey ?? row.id ?? row.child;
    const key = own === null ? placeKey(parent, parentKey, kind) : name(own);
    keys.push(key);
    const address = addressOf(naming.issue, row);
    return {
      address: address === null ? null : name(address),
      aria: name(row.aria),
      hidden: row.hidden.map(name),
      key,
      kind,
      level: row.level,
      links: row.links.map(name),
      parentKey,
      serverPlace: places[index] ?? null,
      tabStops: row.tabStops.map((stop) => ({
        name: name(stop.name),
        role: stop.role,
        tag: stop.tag,
      })),
      words: name(row.words),
    };
  });
  return { name, rows: named };
}

/** The row a paging control pages: the nearest row at or before the one it follows, one level
 *  above the rows it holds back. */
function pagedParent(rows: readonly PageRow[], control: PagingReading): number | undefined {
  if (control.level === null || control.after === null) return undefined;
  for (let at = control.after; at >= 0; at -= 1) {
    if (rows[at]?.level === control.level - 1) return at;
  }
  return undefined;
}

/** Fails where two rows have the same key under the same parent key: a comparison by key could
 *  not tell them apart. */
export function expectRowsNamedApart(rows: readonly RowName[], where: string): void {
  const named = new Set<string>();
  for (const row of rows) {
    const name = JSON.stringify([row.parentKey, row.key]);
    if (named.has(name)) {
      throw new Error(
        `${where}, two rows are named ${row.key} ${placeWords(row.parentKey)}, so a comparison ` +
          "by key and parent key could not tell them apart"
      );
    }
    named.add(name);
  }
}

/** One view as the record holds it: its rows named, its focus and its paging controls by the
 *  rows' names. Fails where two rows have one key under one parent. */
export function viewRecord(drawn: PageView, naming: Naming, where: string): ViewRecord {
  const { name, rows } = namedRows(drawn.rows, naming);
  expectRowsNamedApart(rows, where);
  const rowAt = (index: number | null | undefined) =>
    index === null || index === undefined ? undefined : rows[index];
  const focused = rowAt(drawn.focus);
  return {
    focus: focused === undefined ? null : { key: focused.key, parentKey: focused.parentKey },
    keys: drawn.keys,
    paging: drawn.paging.map((control) => ({
      kind: control.kind,
      level: control.level,
      parentKey: rowAt(pagedParent(drawn.rows, control))?.key ?? null,
      words: name(control.words),
    })),
    rows,
  };
}

/** Fails at the first string of `value`, a field's name included, that holds what one run
 *  minted (`MINTED`), naming where it stands. */
export function expectNothingMinted(value: unknown, path: string): void {
  if (typeof value === "string") {
    for (const { pattern, what } of MINTED) {
      const found = pattern.exec(value);
      if (found !== null) {
        throw new Error(
          `${path} holds ${what}, ${found[0]}, which belongs to this run alone: the next run ` +
            "would write another, so the record cannot hold it"
        );
      }
    }
  } else if (Array.isArray(value)) {
    for (const [index, item] of value.entries()) expectNothingMinted(item, `${path}[${index}]`);
  } else if (typeof value === "object" && value !== null) {
    for (const [field, item] of Object.entries(value)) {
      expectNothingMinted(field, `the name of a field of ${path}`);
      expectNothingMinted(item, `${path}.${field}`);
    }
  }
}

/** What the record row prints for `width`, its fields sorted. Fails where it holds what one run
 *  minted. */
export function widthRecord(
  width: RecordWidth,
  views: Readonly<Record<string, ViewRecord>>
): WidthRecord {
  const record = sortedFields({ views, width }) as WidthRecord;
  expectNothingMinted(record, "the record");
  return record;
}

/** The line the record row prints. */
export function recordLine(record: WidthRecord): string {
  return `${RECORD_LINE} ${JSON.stringify(record)}`;
}

/** The record of one width that `value`, read off line `line` of a log, holds. */
function widthRecordOf(value: unknown, line: number): WidthRecord {
  const held = (typeof value === "object" && value !== null ? value : {}) as {
    readonly views?: unknown;
    readonly width?: unknown;
  };
  if (held.width !== 1440 && held.width !== 390) {
    throw new Error(
      `the ${RECORD_LINE} of line ${line} records ${JSON.stringify(held.width)} px, and a ` +
        "record is made at 1440 and at 390 px"
    );
  }
  if (typeof held.views !== "object" || held.views === null) {
    throw new Error(`the ${RECORD_LINE} of line ${line} holds no views`);
  }
  return value as WidthRecord;
}

/** Every record a log holds: each line that holds `LOAD-RECORD `, read after it. */
export function recordsIn(text: string): WidthRecord[] {
  return text.split("\n").flatMap((line, index) => {
    const at = line.indexOf(`${RECORD_LINE} `);
    if (at === -1) return [];
    const json = line.slice(at + RECORD_LINE.length + 1);
    return [widthRecordOf(JSON.parse(json), index + 1)];
  });
}

/** The base a record was made at, and the commit it holds of each part `main` does not. */
export interface RecordingBase {
  readonly parts: readonly string[];
  readonly recordedOn: string;
}

/** The record, from one line of each width, its fields sorted. Fails where a width has no line or
 *  two, where the two widths hold other views, and where a line holds what one run minted or two
 *  rows with one key under one parent. */
export function rowsRecord(base: RecordingBase, records: readonly WidthRecord[]): RowsRecord {
  const byWidth = new Map<RecordWidth, WidthRecord>();
  for (const record of records) {
    if (byWidth.has(record.width)) {
      throw new Error(
        `two ${RECORD_LINE} lines record ${record.width} px: give the log of one run of the row`
      );
    }
    byWidth.set(record.width, record);
  }
  const views: Record<string, Partial<Record<`${RecordWidth}`, ViewRecord>>> = {};
  const viewsOf: string[][] = [];
  for (const width of RECORD_WIDTHS) {
    const record = byWidth.get(width);
    if (record === undefined) {
      throw new Error(
        `no ${RECORD_LINE} line records ${width} px: the row runs in the chromium project at ` +
          "1440 px and in the iphone project at 390 px, and the record holds both"
      );
    }
    expectNothingMinted(record, `the line of ${width} px`);
    for (const [view, recorded] of Object.entries(record.views)) {
      expectRowsNamedApart(recorded.rows, `${view} at ${width} px`);
      views[view] = { ...views[view], [`${width}`]: recorded };
    }
    viewsOf.push(Object.keys(record.views).sort());
  }
  const [wide, narrow] = viewsOf;
  if (JSON.stringify(wide) !== JSON.stringify(narrow)) {
    throw new Error(
      `the line of 1440 px records ${wide?.join(", ")} and the line of 390 px ` +
        `${narrow?.join(", ")}: one base draws the same views at both widths`
    );
  }
  return sortedFields({ commit: base.recordedOn, parts: base.parts, views }) as RowsRecord;
}

// ---------------------------------------------------------------------------------------------
// The comparison.
// ---------------------------------------------------------------------------------------------

/** What the rows compare, each a clause of `c-same-page`, in the order they are reported. */
export const CLAUSES = [
  "rows",
  "paging",
  "order",
  "address",
  "words",
  "links",
  "tab stops",
  "accessible tree",
  "visibility",
  "focus",
  "keys",
] as const;

export type Clause = (typeof CLAUSES)[number];

/** Each clause in words, for a row's failure. */
export const CLAUSE_WORDS: Readonly<Record<Clause, string>> = {
  "accessible tree": "each row's accessible tree is the base's",
  address: "each row's address is the base's",
  focus: "the focus is on the row the base puts it on",
  keys: "the Keys list holds the keys the base's holds",
  links: "each row's links are the base's, by href in order",
  order: "the rows stand in the base's order",
  paging: "no row past a page of the paging rule is in the document until its control is pressed",
  rows: "each row the record holds as drawn is in the document, and no other",
  "tab stops": "each row's tab stops are the base's, in order",
  visibility: "nothing hides a row's words but content-visibility",
  words: "each row's words are the base's",
};

/** One way the page differs from the record, under one clause. */
export interface Difference {
  readonly clause: Clause;
  readonly line: string;
}

/** A row by its parent's key and its own, which pairs a recorded row with a drawn one. */
function pairOf(row: RowName): string {
  return JSON.stringify([row.parentKey, row.key]);
}

/** The rows a paging control holds back: those of its level under its parent. */
function groupOf(parentKey: string | null, level: number | null): string {
  return JSON.stringify([parentKey, level]);
}

/** `value` as JSON with every object's fields sorted. */
function canonical(value: unknown): string {
  return JSON.stringify(sortedFields(value)) ?? "undefined";
}

/** The index of the row the row `index` stands under: the nearest row before it with its
 *  parent's key. */
function parentIndexOf(rows: readonly RecordedRow[], index: number): number | undefined {
  const parentKey = rows[index]?.parentKey ?? null;
  if (parentKey === null) return undefined;
  for (let before = index - 1; before >= 0; before -= 1) {
    if (rows[before]?.key === parentKey) return before;
  }
  return undefined;
}

/** How the server ordered two rows by one measure, undefined where it did not order both by the
 *  same one. */
function serverSign(one: ServerPlace | null, other: ServerPlace | null): number | undefined {
  if (one === null || other === null || one.by !== other.by) return undefined;
  return Math.sign(one.rank - other.rank);
}

/** Whether the two seeds' servers ordered the pair otherwise, or tied it: then the seed fixes no
 *  order of the two, and none is compared (the head comment's first exception). */
function orderedApart(
  base: readonly [RecordedRow, RecordedRow],
  page: readonly [RecordedRow, RecordedRow]
): boolean {
  const before = serverSign(base[0].serverPlace, base[1].serverPlace);
  const now = serverSign(page[0].serverPlace, page[1].serverPlace);
  if (before === undefined || now === undefined) return false;
  return before === 0 || now === 0 || before !== now;
}

/** The first line on which two accessible trees differ. */
function firstChange(base: string, page: string): string {
  const before = base.split("\n");
  const now = page.split("\n");
  const quoted = (line: string | undefined) =>
    line === undefined ? "no line" : JSON.stringify(line);
  for (let line = 0; line < Math.max(before.length, now.length); line += 1) {
    if (before[line] !== now[line]) {
      return `line ${line + 1} is ${quoted(before[line])} in the base and ${quoted(now[line])} on the page`;
    }
  }
  return "they differ in nothing but their line breaks";
}

/** The words `page` hides that `base` does not, each once for each time more. */
function newlyHidden(base: readonly string[], page: readonly string[]): string[] {
  const left = new Map<string, number>();
  for (const word of base) left.set(word, (left.get(word) ?? 0) + 1);
  return page.filter((word) => {
    const held = left.get(word) ?? 0;
    left.set(word, held - 1);
    return held <= 0;
  });
}

/** Whether two focused rows are one focus: the same row, or rows of one place in the server's
 *  order under one parent, which two seeds fill with different writes. */
function sameFocus(base: ViewRecord, page: ViewRecord): boolean {
  if (base.focus === null || page.focus === null) return base.focus === page.focus;
  const baseFocus = pairOf(base.focus);
  const pageFocus = pairOf(page.focus);
  if (baseFocus === pageFocus) return true;
  const before = base.rows.find((row) => pairOf(row) === baseFocus);
  const now = page.rows.find((row) => pairOf(row) === pageFocus);
  return (
    before !== undefined &&
    now !== undefined &&
    before.parentKey === now.parentKey &&
    serverSign(before.serverPlace, now.serverPlace) === 0
  );
}

/** An entry of the Keys list, in words. */
function keysLine(entry: KeysEntry): string {
  return `${entry.scope}: ${entry.label} (${entry.keys.join(" ")})${entry.enabled ? "" : ", off"}`;
}

/**
 * How the page's view, named as the record names it (`viewRecord`), differs from the record's, by
 * clause in `CLAUSES` order: rows the record holds and the page does not, or the other way round;
 * the rows each paging control holds back and its words; every pair of siblings that stand in
 * another order; each paired row's address, words, links, tab stops, accessible tree and the
 * words it hides; the focus; and the Keys list. Where two seeds differ, as the head comment says,
 * nothing is a difference.
 */
export function viewDifferences(recorded: ViewRecord, drawn: ViewRecord): Difference[] {
  const found = new Map<Clause, string[]>();
  for (const clause of CLAUSES) found.set(clause, []);
  const say = (clause: Clause, line: string) => {
    found.get(clause)?.push(line);
  };
  const recordedAt = new Map(recorded.rows.map((row, index) => [pairOf(row), index] as const));
  const drawnAt = new Map(drawn.rows.map((row, index) => [pairOf(row), index] as const));

  // The parents whose rows past a page are held back, on either side.
  const paged = new Map<string, string | null>();
  for (const control of [...recorded.paging, ...drawn.paging]) {
    paged.set(groupOf(control.parentKey, control.level), control.parentKey);
  }
  // The row at a page's edge a row stands at or under: a child of a paged parent the server
  // orders, which another seed may hold past the page (the head comment's second exception).
  const edgeOf = (rows: readonly RecordedRow[], index: number): string | null => {
    for (let at: number | undefined = index; at !== undefined; at = parentIndexOf(rows, at)) {
      const row = rows[at];
      if (row === undefined) return null;
      if (row.serverPlace !== null && paged.has(groupOf(row.parentKey, row.level))) {
        return pairOf(row);
      }
    }
    return null;
  };

  for (const [index, row] of recorded.rows.entries()) {
    if (drawnAt.has(pairOf(row))) continue;
    const edge = edgeOf(recorded.rows, index);
    if (edge !== null && !drawnAt.has(edge)) continue;
    say("rows", `the base draws ${row.key} ${placeWords(row.parentKey)}, and the page does not`);
  }
  for (const [index, row] of drawn.rows.entries()) {
    if (recordedAt.has(pairOf(row))) continue;
    const edge = edgeOf(drawn.rows, index);
    if (edge !== null && !recordedAt.has(edge)) continue;
    if (paged.has(groupOf(row.parentKey, row.level))) {
      say(
        "paging",
        `the page draws ${row.key} ${placeWords(row.parentKey)} before the control that pages ` +
          "it is pressed"
      );
    } else {
      say(
        "rows",
        `the page draws ${row.key} ${placeWords(row.parentKey)}, which the base does not`
      );
    }
  }

  for (const [group, parentKey] of paged) {
    const count = (rows: readonly RecordedRow[]) =>
      rows.filter((row) => groupOf(row.parentKey, row.level) === group).length;
    const before = count(recorded.rows);
    const now = count(drawn.rows);
    if (before !== now) {
      say(
        "paging",
        `${placeWords(parentKey)} the base draws ${before} rows before the control that pages ` +
          `them, and the page ${now}`
      );
    }
  }
  const controlOf = (control: PagingControl) =>
    `${groupOf(control.parentKey, control.level)} ${control.kind}`;
  for (const control of recorded.paging) {
    const now = drawn.paging.find((other) => controlOf(other) === controlOf(control));
    if (now === undefined) {
      say(
        "paging",
        `${placeWords(control.parentKey)} the base holds the rows past its page behind ` +
          `${JSON.stringify(control.words)}, and the page draws no such control`
      );
    } else if (now.words !== control.words) {
      say(
        "paging",
        `${placeWords(control.parentKey)} the base holds the rows past its page behind ` +
          `${JSON.stringify(control.words)}, and the page behind ${JSON.stringify(now.words)}`
      );
    }
  }
  for (const control of drawn.paging) {
    if (recorded.paging.some((other) => controlOf(other) === controlOf(control))) continue;
    say(
      "paging",
      `${placeWords(control.parentKey)} the page holds rows back behind ` +
        `${JSON.stringify(control.words)}, where the base draws them all`
    );
  }

  const siblings = new Map<string, RecordedRow[]>();
  for (const row of recorded.rows) {
    if (!drawnAt.has(pairOf(row))) continue;
    const parent = JSON.stringify(row.parentKey);
    const group = siblings.get(parent) ?? [];
    group.push(row);
    siblings.set(parent, group);
  }
  for (const rows of siblings.values()) {
    for (const [at, first] of rows.entries()) {
      for (const second of rows.slice(at + 1)) {
        const firstAt = drawnAt.get(pairOf(first)) ?? -1;
        const secondAt = drawnAt.get(pairOf(second)) ?? -1;
        const drawnFirst = drawn.rows[firstAt];
        const drawnSecond = drawn.rows[secondAt];
        if (drawnFirst === undefined || drawnSecond === undefined) continue;
        if (orderedApart([first, second], [drawnFirst, drawnSecond])) continue;
        if (firstAt > secondAt) {
          say(
            "order",
            `${placeWords(first.parentKey)}, the base draws ${first.key} before ${second.key}, ` +
              `and the page draws ${first.key} after it`
          );
        }
      }
    }
  }

  for (const row of recorded.rows) {
    const page = drawn.rows[drawnAt.get(pairOf(row)) ?? -1];
    if (page === undefined) continue;
    if (row.address !== page.address) {
      say(
        "address",
        `${row.key}: the base's ${JSON.stringify(row.address)}, the page's ${JSON.stringify(page.address)}`
      );
    }
    if (row.words !== page.words) {
      say(
        "words",
        `${row.key}: the base's ${JSON.stringify(row.words)}, the page's ${JSON.stringify(page.words)}`
      );
    }
    if (canonical(row.links) !== canonical(page.links)) {
      say(
        "links",
        `${row.key}: the base links ${canonical(row.links)}, the page ${canonical(page.links)}`
      );
    }
    if (canonical(row.tabStops) !== canonical(page.tabStops)) {
      say(
        "tab stops",
        `${row.key}: the base stops at ${canonical(row.tabStops)}, the page at ${canonical(page.tabStops)}`
      );
    }
    if (row.aria !== page.aria)
      say("accessible tree", `${row.key}: ${firstChange(row.aria, page.aria)}`);
    for (const word of newlyHidden(row.hidden, page.hidden)) {
      say("visibility", `${row.key}: the page hides ${JSON.stringify(word)}, which the base shows`);
    }
  }

  if (!sameFocus(recorded, drawn)) {
    const focusWords = (focus: RowName | null) =>
      focus === null ? "no row" : `${focus.key} ${placeWords(focus.parentKey)}`;
    say(
      "focus",
      `the base puts the focus on ${focusWords(recorded.focus)}, and the page on ${focusWords(drawn.focus)}`
    );
  }

  if (recorded.keys === null || drawn.keys === null) {
    if (recorded.keys !== drawn.keys) {
      say(
        "keys",
        recorded.keys === null
          ? "the page draws a Keys list where the base draws none"
          : "the base draws a Keys list where the page draws none"
      );
    }
  } else {
    const before = recorded.keys.map(keysLine);
    const now = drawn.keys.map(keysLine);
    for (const line of before.filter((entry) => !now.includes(entry))) {
      say("keys", `the base's Keys list holds ${line}, and the page's does not`);
    }
    for (const line of now.filter((entry) => !before.includes(entry))) {
      say("keys", `the page's Keys list holds ${line}, which the base's does not`);
    }
    if (found.get("keys")?.length === 0 && JSON.stringify(before) !== JSON.stringify(now)) {
      say("keys", "the page's Keys list holds the base's entries in another order");
    }
  }

  return CLAUSES.flatMap((clause) => (found.get(clause) ?? []).map((line) => ({ clause, line })));
}

// ---------------------------------------------------------------------------------------------
// What the rows read: the record, the seed's names, and the page.
// ---------------------------------------------------------------------------------------------

/** The record as the rows read it, once. */
let rowsRecordRead: RowsRecord | undefined;

/** The record of `view` at `width`. It is read, never imported, since Playwright loads a spec
 *  under Node, which refuses a JSON import that names no `type: json`. Fails, saying how to make
 *  it, where the record or that view is missing. */
export function recordedView(view: string, width: RecordWidth): ViewRecord {
  const file = relative(packageRoot, fileURLToPath(RECORD_FILE));
  if (!existsSync(RECORD_FILE)) {
    throw new Error(
      `${file} is missing, so there is no record of the rows the base draws to compare the page ` +
        "with. Make it at this branch's base, as the head of e2e/tools/issue-page-load-record.ts " +
        `says: run the row "record the rows the base draws @phone" with ${RECORD_VARIABLE}=1 in ` +
        "the chromium and iphone projects, then bun e2e/tools/issue-page-load-record.ts " +
        "--recorded-on <base> [--part <commit> ...] <its log>"
    );
  }
  rowsRecordRead ??= JSON.parse(readFileSync(RECORD_FILE, "utf8")) as RowsRecord;
  const recorded = rowsRecordRead.views[view]?.[`${width}`];
  if (recorded === undefined) {
    throw new Error(
      `${file}, made at ${rowsRecordRead.commit}, holds no ${view} at ${width} px: make it again ` +
        "at this branch's base, as the head of e2e/tools/issue-page-load-record.ts says"
    );
  }
  return recorded;
}

/** The width of a row's viewport, which is one its record is made at. */
export function recordWidth(viewport: { readonly width: number } | null): RecordWidth {
  const width = viewport?.width;
  if (width === 1440 || width === 390) return width;
  throw new Error(
    "a record is made and read at 1440 px (the chromium project) and at 390 px (the iphone " +
      `project), and this row's page is ${width === undefined ? "no fixed width" : `${width} px`} wide`
  );
}

/** The names and the server's order the rows are named by (the head comment's "How a row is
 *  named"), read from what the seed returned and from the issue's reads. */
export async function seedNaming(seeded: LargestShapeSeed): Promise<Naming> {
  const [issue, asks, comments, messages] = await Promise.all([
    getIssue(seeded.key),
    listIssueAsks(seeded.key),
    listComments(seeded.key),
    listIssueMessages(seeded.key),
  ]);
  // The seed fills `replies` as the server answers posts it writes six at a time, so the list's
  // order is another in each run: a reply is named by its post and its place in its thread.
  const { replies: _inTheServersOrder, ...returned } = seeded;
  const slugs = (issue.artifacts ?? []).map((artifact) => artifact.slug);
  const names = new Map(seedKeys(returned, slugs));
  for (const ask of asks) {
    if (!names.has(ask.id)) names.set(ask.id, `ask ${JSON.stringify(ask.question)}`);
  }
  const order = new Map<string, ServerOrder>();
  for (const item of [...asks, ...comments])
    order.set(item.id, { at: item.created_at, by: "made" });
  for (const thread of messages) {
    for (const message of [thread.message, ...(thread.replies ?? [])]) {
      order.set(message.id, { at: message.created_at, by: "made" });
    }
  }
  for (const child of seeded.children) {
    const number = /-(\d+)$/.exec(child)?.[1];
    if (number !== undefined) order.set(child, { at: Number(number), by: "key" });
  }
  return { issue: seeded.key, names, order };
}

/** The controls that hold a parent's rows past its first page back, by the attribute each
 *  carries (`TreeRows.tsx`'s `PAGE_CONTROL_MARKS`). */
const PAGING_CONTROLS: Readonly<Record<string, string>> = {
  more: "data-tree-more",
  older: "data-older-comments",
  page: "data-tree-page",
};

/** The list Keys opens (`shell/ShortcutHelp.tsx`). */
const KEYS_LIST = '[role="dialog"][aria-label="Keyboard shortcuts"]';

/** What `readRows` is handed. */
interface RowsQuery {
  readonly paging: Readonly<Record<string, string>>;
  readonly rows: string;
}

/** What `readRows` answers. */
interface RowsRead {
  readonly focus: number | null;
  readonly paging: readonly PagingReading[];
  readonly rows: readonly Omit<PageRow, "aria">[];
}

/**
 * Reads the rows in the browser, as `PageRow` says, but their accessible trees, which Playwright
 * reads. It runs inside the page, so it names nothing outside itself.
 */
function readRows({ paging, rows: selector }: RowsQuery): RowsRead {
  const words = (text: string | null | undefined) => (text ?? "").replace(/\s+/g, " ").trim();
  const rows = [...document.querySelectorAll(selector)];
  const isRow = new Set<Element>(rows);
  const rowOf = (element: Element | null): Element | null => {
    for (let at = element; at !== null; at = at.parentElement) {
      if (isRow.has(at)) return at;
    }
    return null;
  };
  const nameOf = (element: Element): string => {
    const labelledBy = (element.getAttribute("aria-labelledby") ?? "").trim();
    if (labelledBy !== "") {
      return words(
        labelledBy
          .split(/\s+/)
          .map((id) => document.getElementById(id)?.textContent ?? "")
          .join(" ")
      );
    }
    const label = element.getAttribute("aria-label");
    if (label !== null) return words(label);
    return words(element.textContent) || words(element.getAttribute("title"));
  };
  const clipping = new Map<Element, boolean>();
  const clips = (at: Element): boolean => {
    const known = clipping.get(at);
    if (known !== undefined) return known;
    const clipped =
      getComputedStyle(at).overflow !== "visible" && at.scrollHeight > at.clientHeight;
    clipping.set(at, clipped);
    return clipped;
  };
  const seen = (holder: Element, row: Element): boolean => {
    if (!holder.checkVisibility({ opacityProperty: true, visibilityProperty: true })) return false;
    for (let at: Element | null = holder; at !== null; at = at.parentElement) {
      if (clips(at)) return false;
      if (at === row) break;
    }
    return true;
  };
  const read = rows.map((row) => {
    const own = (element: Element) => rowOf(element) === row;
    const copy = row.cloneNode(true);
    if (copy instanceof Element) {
      for (const nested of copy.querySelectorAll(selector)) nested.remove();
    }
    const hidden: string[] = [];
    const texts = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
    for (let text = texts.nextNode(); text !== null; text = texts.nextNode()) {
      const holder = text.parentElement;
      const said = words(text.textContent);
      if (holder === null || said === "" || !own(holder)) continue;
      if (!seen(holder, row)) hidden.push(said);
    }
    const level = row.getAttribute("aria-level");
    return {
      child: row.getAttribute("data-child-entry"),
      hidden,
      id: row.getAttribute("data-node-id"),
      level: level === null ? null : Number(level),
      links: [...row.querySelectorAll("a[href]")]
        .filter(own)
        .map((link) => link.getAttribute("href") ?? ""),
      rowKey: row.getAttribute("data-row-key"),
      tabStops: [row, ...row.querySelectorAll("*")]
        .filter(
          (element) =>
            (element instanceof HTMLElement || element instanceof SVGElement) &&
            element.tabIndex >= 0 &&
            own(element)
        )
        .map((element) => ({
          name: nameOf(element),
          role: element.getAttribute("role"),
          tag: element.localName,
        })),
      type: row.getAttribute("data-node-type"),
      words: words(copy.textContent),
    };
  });
  const controls = Object.entries(paging).flatMap(([kind, attribute]) =>
    [...document.querySelectorAll(`[data-issue-page] [${attribute}]`)].map((control) => ({
      control,
      kind,
    }))
  );
  controls.sort((one, other) =>
    one.control.compareDocumentPosition(other.control) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1
  );
  const focused = rowOf(document.activeElement);
  return {
    focus: focused === null ? null : rows.indexOf(focused),
    paging: controls.map(({ control, kind }) => {
      let after: number | null = null;
      for (const [index, row] of rows.entries()) {
        if (!(row.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_FOLLOWING)) break;
        after = index;
      }
      const level = control.getAttribute("aria-level");
      return {
        after,
        kind,
        level: level === null ? null : Number(level),
        words: words(control.textContent),
      };
    }),
    rows: read,
  };
}

/** The entries of the Keys list: a press on Keys opens it, and Escape closes it again. */
async function keysList(page: Page): Promise<KeysEntry[]> {
  await page
    .locator("[data-filter-row]")
    .getByRole("button", { exact: true, name: CONTROL_LABELS.keys })
    .click();
  const list = page.locator(KEYS_LIST);
  await expect(list, "a press on Keys opened no list of keys").toBeVisible();
  const entries = await list.evaluate((dialog) =>
    [...dialog.querySelectorAll("section")].flatMap((section) =>
      [...section.querySelectorAll("li")].map((item) => ({
        enabled: item.getAttribute("data-enabled") === "true",
        keys: [...item.querySelectorAll("kbd")].map((key) => (key.textContent ?? "").trim()),
        label: (item.querySelector(":scope > span")?.textContent ?? "").trim(),
        scope: section.getAttribute("aria-label") ?? "",
      }))
    )
  );
  await page.keyboard.press("Escape");
  await expect(list, "Escape left the list of keys open").toHaveCount(0);
  return entries;
}

/**
 * What the page draws of the view it holds, once its load has ended: every row, each with its
 * accessible tree, the focused row, the paging controls, and at 1440 px the Keys list, read last
 * since opening it moves the focus. Fails where the rows change while they are read.
 */
export async function readView(page: Page, width: RecordWidth): Promise<PageView> {
  const read = () => page.evaluate(readRows, { paging: PAGING_CONTROLS, rows: PAGE_ROWS });
  const before = await read();
  const rows = page.locator(PAGE_ROWS);
  const trees: string[] = [];
  for (let index = 0; index < before.rows.length; index += 1) {
    trees.push(await rows.nth(index).ariaSnapshot());
  }
  const after = await read();
  if (JSON.stringify(after) !== JSON.stringify(before)) {
    throw new Error(
      "the page changed while its rows were read, after its load had ended: no record of it " +
        "would hold still"
    );
  }
  return {
    focus: before.focus,
    keys: width === 1440 ? await keysList(page) : null,
    paging: before.paging,
    rows: before.rows.map((row, index) => ({ ...row, aria: trees[index] ?? "" })),
  };
}

// ---------------------------------------------------------------------------------------------
// The command: its arguments, and the file it writes.
// ---------------------------------------------------------------------------------------------

const usage =
  "usage: bun e2e/tools/issue-page-load-record.ts --recorded-on <commit> [--part <commit> ...] " +
  "[<log> ...]\n" +
  "  --recorded-on  the base the record row ran at\n" +
  "  --part         the commit the base holds of one part of the page main does not hold; once\n" +
  "                 for each such part\n" +
  "  <log>          the log of that run, which holds its two LOAD-RECORD lines; the standard\n" +
  "                 input where none is named";

/** A commit: the short or the full hash. */
const COMMIT = /^[0-9a-f]{7,64}$/;

const packageRoot = fileURLToPath(new URL("../../", import.meta.url));
const repoRoot = fileURLToPath(new URL("../../../../", import.meta.url));

function refuse(message: string): never {
  console.error(`${message}\n\n${usage}`);
  process.exit(2);
}

function givenArguments() {
  try {
    return parseArgs({
      allowPositionals: true,
      args: process.argv.slice(2),
      options: { part: { multiple: true, type: "string" }, "recorded-on": { type: "string" } },
      strict: true,
    });
  } catch (error) {
    return refuse(error instanceof Error ? error.message : String(error));
  }
}

/** The base and its parts, from the flags. */
function recordingBase(given: {
  readonly part?: readonly string[];
  readonly "recorded-on"?: string;
}): RecordingBase {
  const recordedOn = given["recorded-on"] ?? "";
  if (recordedOn === "") refuse("Name the base the record row ran at: --recorded-on <commit>.");
  if (!COMMIT.test(recordedOn)) {
    refuse(`The base ${JSON.stringify(recordedOn)} is not a commit's hash.`);
  }
  const parts = new Set<string>();
  for (const commit of given.part ?? []) {
    if (!COMMIT.test(commit)) refuse(`The part ${JSON.stringify(commit)} is not a commit's hash.`);
    if (parts.has(commit)) refuse(`The part ${commit} is named twice.`);
    parts.add(commit);
  }
  return { parts: [...parts].sort(), recordedOn };
}

/** `json` as Biome formats it, which is what `bunx biome check .` holds a committed file to. */
function biomeFormatted(json: string, file: string): string {
  const biome = Bun.which("biome", {
    PATH: [join(repoRoot, "node_modules/.bin"), process.env.PATH ?? ""].join(":"),
  });
  if (biome === null) {
    throw new Error("The command writes the record through Biome: run `bun install` first.");
  }
  const formatted = Bun.spawnSync({
    cmd: [biome, "format", `--stdin-file-path=${relative(repoRoot, file)}`],
    cwd: repoRoot,
    stderr: "inherit",
    stdin: new TextEncoder().encode(json),
  });
  if (formatted.exitCode !== 0) throw new Error("biome format refused the record");
  const source = formatted.stdout.toString();
  // Biome answers nothing for a file over its size limit (`files.maxSize`, 1 MiB unless the
  // configuration says otherwise), and `biome check` passes over such a file in the same way,
  // so the file is then written as `JSON.stringify` wrote it.
  return source === "" ? json : source;
}

/** The command: reads the record's lines, checks them, and writes the record. */
async function main(): Promise<number> {
  const { positionals, values } = givenArguments();
  const base = recordingBase(values);
  const text =
    positionals.length === 0
      ? await Bun.stdin.text()
      : positionals.map((log) => readFileSync(log, "utf8")).join("\n");
  const records = recordsIn(text);
  if (records.length === 0) {
    refuse(
      `${positionals.length === 0 ? "The standard input" : positionals.join(", ")} holds no ` +
        `${RECORD_LINE} line.`
    );
  }
  const record = rowsRecord(base, records);
  const file = fileURLToPath(RECORD_FILE);
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, biomeFormatted(`${JSON.stringify(record, null, 2)}\n`, file));
  console.error(
    `wrote ${relative(process.cwd(), file)}, made at ${base.recordedOn}: ` +
      `${Object.keys(record.views).join(", ")} at ${RECORD_WIDTHS.join(" and ")} px`
  );
  return 0;
}

if (import.meta.main) {
  main().then(
    (status) => process.exit(status),
    (error: unknown) => {
      console.error(error instanceof Error ? error.message : error);
      process.exit(1);
    }
  );
}
