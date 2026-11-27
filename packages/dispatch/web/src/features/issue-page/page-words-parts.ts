/**
 * The join of the issue page's words. `page-words.ts` declares each table of words that the page
 * draws. Each open part of the page keeps its new words in its own file, `page-words-<letter>.ts`.
 * This module names each of those files once, in `WORD_FILES`.
 *
 * `joinWords` makes each table of `page-words.ts`. It copies the table, and merges into the copy
 * the keys that each part's file adds to that table, in the order of `WORD_FILES`. Then it
 * freezes the copy. So two parts that add words change no common line.
 *
 * Where a word goes is the list in the header of `page-words.ts`. The five tables of
 * `CLOSED_TABLE_NAMES` take keys only in `page-words.ts`: `PartWords.add` says what the type check
 * refuses for them, and what only a case of `page-words.test.ts` refuses. A key that two files
 * give is a clash, and `page-words.test.ts` fails on each clash.
 *
 * This module imports nothing from `page-words.ts`, because `page-words.ts` imports it. A part's
 * file imports only the type `PartWords` from here.
 */

import { freeze } from "./page-structure";
import * as partDashboard from "./page-words-dashboard";
import * as partJ from "./page-words-j";
import * as partL from "./page-words-l";
import * as partM from "./page-words-m";
import * as partN from "./page-words-n";
import * as partS from "./page-words-s";
import * as partT from "./page-words-t";
import * as partU from "./page-words-u";
import * as partW from "./page-words-w";
import * as partX from "./page-words-x";
import * as partY from "./page-words-y";
import * as partZ from "./page-words-z";

/** The name of each table that `page-words.ts` makes with `joinWords`, in the order it makes
 *  them. */
export const TABLE_NAMES = [
  "NODE_TYPE_NAMES",
  "BLOCK_TYPE_NAMES",
  "NODE_STATE_WORDS",
  "PRESET_NAMES",
  "FILTER_SIGNAL_WORDS",
  "CONTENTS_MARK_WORDS",
  "SPEC_WORDS",
  "DECISIONS_WORDS",
  "VERIFICATION_WORDS",
  "PLAN_WORDS",
  "NODE_REFERENCES_WORDS",
  "EMPTY_STATE_LINES",
  "GAP_LINES",
  "GAP_WORDS",
  "PROGRESS_NOUNS",
  "STATE_LINES",
  "ANSWER_LINES",
  "CHECK_WHEN_WORDS",
  "RESULT_STANDS_WORDS",
  "RESULT_VERDICT_WORDS",
  "DELIVERY_STATE_WORDS",
  "BLOCKED_BY_REFUSALS",
  "APPROVAL_STATE_WORDS",
  "CONTROL_LABELS",
  "BLOCK_ATTRIBUTE_LABELS",
  "NODE_FACT_LABELS",
  "AUTHOR_KIND_WORDS",
  "FILE_WORDS",
  "GLYPH_TOKENS",
] as const;

/** One table of `page-words.ts`, by its name. */
export type TableName = (typeof TABLE_NAMES)[number];

/** The tables that take keys only in `page-words.ts`. A closed type fixes the keys of each one,
 *  and its `satisfies` reads the literal of `page-words.ts`. So a part's file gives them no key:
 *  `PartWords.add` says how that is held. */
export const CLOSED_TABLE_NAMES = [
  "PRESET_NAMES",
  "CHECK_WHEN_WORDS",
  "RESULT_STANDS_WORDS",
  "DELIVERY_STATE_WORDS",
  "AUTHOR_KIND_WORDS",
] as const satisfies readonly TableName[];

/** A table that takes keys only in `page-words.ts`. */
type ClosedTableName = (typeof CLOSED_TABLE_NAMES)[number];

/** The default export of a part's file. */
export interface PartWords {
  /** The part's new keys for the tables of `page-words.ts`, by the table's name, nested as the
   *  table is.
   *
   *  Each table of `CLOSED_TABLE_NAMES` has the type `never` here. So the type check refuses
   *  words for one of them in a part's file whose `add` has a literal type (a literal, a `const`
   *  of its own or a spread of one), with `satisfies PartWords` or without it. An `add` with a
   *  wide type (`object`, `Record<string, …>`) passes the type check. The case "each key a
   *  part's file adds under add names a table that takes a part's keys" of `page-words.test.ts`
   *  refuses a closed table's key, and a key for a table that `page-words.ts` does not declare,
   *  in any part's file whatever the type of its `add`, and names the file and the table. It is
   *  the only guard for an `add` with a wide type. A key spread from a wide type passes the
   *  `satisfies` of a literal on `main` too. */
  readonly add: { readonly [Name in Exclude<TableName, ClosedTableName>]?: object } & {
    readonly [Name in ClosedTableName]?: never;
  };
  /** The part's own tables that the page draws on load. */
  readonly unasked: readonly unknown[];
  /** The part's own tables that the page draws only after an act of the reader. */
  readonly answers: readonly unknown[];
}

/** A part's file: its name in this directory, and the module that it is. */
export interface WordFile {
  readonly file: string;
  readonly module: { readonly default: PartWords };
}

/** A key that two files give. `key` is the table's name and the key's path, with dots between.
 *  `files` names the file that gave the value that stays, then the second file. */
export interface WordClash {
  readonly key: string;
  readonly files: readonly [string, string];
}

/** The file of each part, in the order of the parts' letters, then Dashboard's
 *  (dispatch://INFRA-5702). The join reads them in this order. */
export const WORD_FILES = [
  { file: "page-words-j.ts", module: partJ },
  { file: "page-words-l.ts", module: partL },
  { file: "page-words-m.ts", module: partM },
  { file: "page-words-n.ts", module: partN },
  { file: "page-words-s.ts", module: partS },
  { file: "page-words-t.ts", module: partT },
  { file: "page-words-u.ts", module: partU },
  { file: "page-words-w.ts", module: partW },
  { file: "page-words-x.ts", module: partX },
  { file: "page-words-y.ts", module: partY },
  { file: "page-words-z.ts", module: partZ },
  { file: "page-words-dashboard.ts", module: partDashboard },
] as const satisfies readonly WordFile[];

/** The type of the words that `File` adds to the table `Name`, or `unknown` if it adds none. */
type AddedIn<
  File extends WordFile,
  Name extends TableName,
> = File["module"]["default"]["add"] extends { readonly [Key in Name]: infer Words }
  ? Words
  : unknown;

/** The types of the words that each of `Files` adds to the table `Name`, intersected. It is
 *  `unknown` if no file adds to the table, so the joined table then has the table's own type. */
type AddedBy<Files extends readonly WordFile[], Name extends TableName> = Files extends readonly [
  infer First extends WordFile,
  ...infer Rest extends readonly WordFile[],
]
  ? AddedIn<First, Name> & AddedBy<Rest, Name>
  : unknown;

/** The file that a clash names for a key of the table's own. */
const TABLE_FILE = "page-words.ts";

/** Whether the join merges into `value`: an object and not an array. */
function isTable(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * The table `name` of `page-words.ts`, joined with the words that each of `files` adds to it,
 * and each key that two of them give. The table is a copy of `main`. Each file's `add[name]` is
 * merged into it in file order:
 *
 * - A key that the table does not have comes after the table's keys.
 * - Where the table and the file both hold an object that is not an array, the merge goes into
 *   that object.
 * - Each other key that a file gives again is a clash. The first value stays.
 *
 * The table is frozen at every level. The join changes none of its inputs.
 */
export function joinTables<
  Name extends TableName,
  Main extends object,
  Files extends readonly WordFile[],
>(
  name: Name,
  main: Main,
  files: Files
): { readonly table: Main & AddedBy<Files, Name>; readonly clashes: readonly WordClash[] } {
  const clashes: WordClash[] = [];
  // For each object of the table, the file that added each key to it after the object was made.
  // A key with no entry came with its object, from the file that gave the object.
  const adders = new Map<object, Map<string, string>>();

  // Merges the `words` of `file` into `target`. `giver` is the file that gave `target`, and
  // `path` names `target` in a clash.
  function merge(
    target: Record<string, unknown>,
    words: object,
    file: string,
    giver: string,
    path: string
  ): void {
    const added = adders.get(target) ?? new Map<string, string>();
    adders.set(target, added);
    for (const [key, word] of Object.entries(words)) {
      const held = target[key];
      const heldGiver = added.get(key) ?? giver;
      if (!Object.hasOwn(target, key)) {
        target[key] = structuredClone(word);
        added.set(key, file);
      } else if (isTable(held) && isTable(word)) {
        merge(held, word, file, heldGiver, `${path}.${key}`);
      } else {
        clashes.push({ key: `${path}.${key}`, files: [heldGiver, file] });
      }
    }
  }

  // The table's own words go in first, so each key of `main` is the table file's.
  const table: Record<string, unknown> = {};
  merge(table, main, TABLE_FILE, TABLE_FILE, name);
  for (const { file, module } of files) {
    const words = module.default.add[name];
    if (words !== undefined) merge(table, words, file, TABLE_FILE, name);
  }
  return freeze({ table: table as Main & AddedBy<Files, Name>, clashes });
}

const joinedTables = new Map<TableName, object>();
const wordClashes: WordClash[] = [];

/** Each table that `joinWords` made, by its name. */
export const JOINED_TABLES: ReadonlyMap<TableName, object> = joinedTables;

/** Each clash that `joinWords` found, in the order that `page-words.ts` makes its tables. */
export const WORD_CLASHES: readonly WordClash[] = wordClashes;

/** The table `name` of `page-words.ts`, joined with the words that each part's file adds to it.
 *  It records the table in `JOINED_TABLES` and each clash in `WORD_CLASHES`. */
export function joinWords<Name extends TableName, Main extends object>(
  name: Name,
  main: Main
): Main & AddedBy<typeof WORD_FILES, Name> {
  const { table, clashes } = joinTables(name, main, WORD_FILES);
  joinedTables.set(name, table);
  wordClashes.push(...clashes);
  return table;
}

/** The parts' own tables that the page draws on load, in file order. `UNASKED_SOURCES` of
 *  `page-words.ts` holds them. */
export const PART_UNASKED_SOURCES: readonly unknown[] = freeze(
  WORD_FILES.flatMap<unknown>(({ module }) => module.default.unasked)
);

/** The parts' own tables that the page draws only after an act of the reader, in file order.
 *  `ANSWER_SOURCES` of `page-words.ts` holds them. */
export const PART_ANSWER_SOURCES: readonly unknown[] = freeze(
  WORD_FILES.flatMap<unknown>(({ module }) => module.default.answers)
);
