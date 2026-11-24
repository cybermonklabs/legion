// The records of the daemon API contracts a build serves, as this package's schema tests read them.
//
// The numbered entries' records are frozen: `fixtures/daemon-api/contract-shapes.json` holds what
// each answer carries at each contract from the floor to L, the greatest number of
// `released-contracts.json`, and `request-shapes.json` what each route a plugin calls reads, and
// `answers.json` and `enumerated-fields.json` hold the rows of those entries alone. Each contract
// change after L is one entry with one fixture, `fixtures/daemon-api/contracts/<id>_<slug>.json`,
// which records what that entry changes and holds no number (`EntryFixture` says what it holds).
// An entry's number is L plus its rank in the build's order, and the record of a number above L is
// L's record with the fixture of each entry at or below it applied in that order. A build stamped
// with main's merge order (`internal/api`'s `contractOrder`) ranks its entries by the stamp; this
// package has no stamp, so it ranks them by id, as an unstamped daemon build does. The daemon's
// shape gate composes records the same way (`internal/api`'s `composeEntries`), and the two are
// held to one set of example fixtures, `testdata/entry-fixture-examples/`, each to the one record
// those compose. The examples are test data, never among the entry fixtures of
// `fixtures/daemon-api/contracts/`, so no count of entries includes them.
//
// Each operation of a fixture has one condition, read against the record before the entry: an added
// key path, field path, domain value, answer or route is absent from it, and a removed one present.
// A fixture that breaks one is refused, naming the entry and the operation, because it was written
// over another set of entries than the ones before it here: an entry that changes what another
// added comes after that entry both in merge order and in id order.

import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** The name of an entry fixture: the entry's id, the UTC time `YYYYMMDDHHMMSS` its author made
 *  it, and its slug. The directory of entry fixtures holds such files and nothing else. */
export const ENTRY_FIXTURE_NAME = /^(\d{14})_([a-z0-9]+(?:-[a-z0-9]+)*)\.json$/;

const word = z.string().min(1);
const words = z.array(word);
const valuesByKeyPath = z.record(word, words);

/** What one entry changes of one answer, as `EntryFixture` says. */
const AnswerChange = z.strictObject({
  answer: word,
  change: z.enum(["add", "remove"]).optional(),
  noBody: z.boolean().optional(),
  addKeyPaths: words.optional(),
  removeKeyPaths: words.optional(),
  addValues: valuesByKeyPath.optional(),
  removeValues: valuesByKeyPath.optional(),
});

/** What one entry changes of one route a plugin calls, as `EntryFixture` says. */
const RouteChange = z.strictObject({
  route: word,
  change: z.enum(["add", "remove"]).optional(),
  addFieldPaths: words.optional(),
  removeFieldPaths: words.optional(),
});

/** One row an entry adds to `answers.json`, as `EntryFixture` says. */
const EntryAnswerRow = z.strictObject({
  id: word,
  routes: words,
  noBody: z.boolean(),
  typedSettings: z.array(z.strictObject({ key: word })).optional(),
});

/** One row an entry adds to `enumerated-fields.json`, as `EntryFixture` says. */
const EntryEnumeratedRow = z.strictObject({ answer: word, keyPath: word, values: words });

/**
 * One entry fixture, `fixtures/daemon-api/contracts/<id>_<slug>.json`: what one entry changes of
 * the contract, with no number in it. `go test ./internal/api -update` writes it, as what the
 * daemon serves at the entry's number less the record the entries before it compose. Every list
 * is sorted, by answer, route, key path or value, and a key with nothing to say is left out:
 *
 * - `answers`: one element for each answer the entry changes. `change` is `add` where the entry
 *   adds the answer and `remove` where it removes it, and is absent where the answer stands before
 *   and after. `noBody` is, on `add`, whether the new answer writes no body at all; otherwise it is
 *   present only where the entry changes it, and holds the value after. `addKeyPaths` and
 *   `removeKeyPaths` are the key paths of the answer's body the entry adds and removes, and
 *   `addValues` and `removeValues` the values it adds to and removes from the domain held under each
 *   key path. An added answer's record is its `addKeyPaths` and `addValues` alone, and a removed
 *   answer's element holds nothing but its `change`.
 * - `routes`: one element for each route a plugin calls whose request the entry changes, with
 *   `change` as above, and `addFieldPaths` and `removeFieldPaths` the field paths it adds and
 *   removes.
 * - `answerRows`: the rows the entry adds to `answers.json`, without `since`: the entry is their
 *   arrival. A row of an answer no row before it names is the whole row. A row of an answer a row
 *   before it names holds only the routes and the typed settings the entry adds to that row, and
 *   that row's own `noBody`. A typed setting is its key alone: it is typed from the entry on.
 * - `enumeratedRows`: the rows the entry adds to `enumerated-fields.json`, without `since`: one for
 *   each enumerated field the entry adds a value to, its `values` being those the entry adds. A
 *   field no row before it declares is a new row.
 */
export const EntryFixture = z.strictObject({
  answers: z.array(AnswerChange),
  routes: z.array(RouteChange),
  answerRows: z.array(EntryAnswerRow),
  enumeratedRows: z.array(EntryEnumeratedRow),
});
export type EntryFixture = z.infer<typeof EntryFixture>;

/** One record of `contract-shapes.json`: one answer as one contract reads it. A record that holds
 *  no domain for an enumerated field admits no value of it, as one holding an empty domain. */
export interface ContractShape {
  readonly contract: number;
  readonly answer: string;
  readonly noBody: boolean;
  readonly keyPaths: readonly string[];
  readonly domains: Readonly<Record<string, readonly string[]>>;
}

/** One record of `request-shapes.json`: the field paths one contract's plugin sends one route. */
export interface RequestShape {
  readonly contract: number;
  readonly route: string;
  readonly fieldPaths: readonly string[];
}

/** One row of `answers.json` (`internal/api`'s `Answer`). */
export interface AnswerRow {
  readonly id: string;
  readonly routes: readonly string[];
  readonly since: number;
  readonly noBody: boolean;
  readonly typedSettings?: readonly {
    readonly key: string;
    readonly since: number;
    readonly until?: number;
  }[];
}

/** One row of `enumerated-fields.json` (`internal/api`'s `EnumeratedField`). */
export interface EnumeratedFieldRow {
  readonly answer: string;
  readonly keyPath: string;
  readonly values: readonly { readonly value: string; readonly since: number }[];
}

/** Everything of one contract an entry fixture changes: the record of each answer and of each
 *  route at `contract`, and the rows of `answers.json` and `enumerated-fields.json` as they stand
 *  there. The example fixtures' `base.json` and `expected.json` are written in this form. */
export interface ContractRecords {
  readonly contract: number;
  readonly records: readonly ContractShape[];
  readonly requests: readonly RequestShape[];
  readonly answers: readonly AnswerRow[];
  readonly enumeratedFields: readonly EnumeratedFieldRow[];
}

/** One entry fixture at its place in the build's order: `name` is its file's name without
 *  `.json` (`<id>_<slug>`), `number` the contract it is. */
export interface NumberedEntry {
  readonly name: string;
  readonly number: number;
  readonly fixture: EntryFixture;
}

/** The order the fixtures sort lists in, which is Go's `slices.Sort` over strings for the key
 *  paths, routes and words they hold. */
function byString(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

function sorted(list: Iterable<string>): string[] {
  return [...list].sort(byString);
}

/** The words `list` holds twice, sorted: a fixture names each key path, value or route once. */
function repeated(list: readonly string[]): string[] {
  return sorted(new Set(list.filter((item, at) => list.indexOf(item) !== at)));
}

/** A mutable record of one answer, while the entries are applied to it. */
interface AnswerRecord {
  noBody: boolean;
  keyPaths: Set<string>;
  domains: Map<string, Set<string>>;
}

/**
 * The record of each entry's number, in the order of `entries`: `base`, the record of the contract
 * before the first of them, with the fixture of each entry up to that one applied in turn. It
 * throws on a fixture that breaks the condition of one of its operations, naming the entry and the
 * operation, and on one that says something no operation can mean (an added answer that names no
 * `noBody`, a removed one that names a key path, an answer or a route named twice).
 */
export function composeEntries(
  base: ContractRecords,
  entries: readonly NumberedEntry[]
): ContractRecords[] {
  const answers = new Map<string, AnswerRecord>(
    base.records.map((record) => [
      record.answer,
      {
        noBody: record.noBody,
        keyPaths: new Set(record.keyPaths),
        domains: new Map(Object.entries(record.domains).map(([key, held]) => [key, new Set(held)])),
      },
    ])
  );
  const routes = new Map<string, Set<string>>(
    base.requests.map((request) => [request.route, new Set(request.fieldPaths)])
  );
  let answerRows: AnswerRow[] = base.answers.map((row) => ({ ...row }));
  let enumeratedRows: EnumeratedFieldRow[] = base.enumeratedFields.map((row) => ({ ...row }));
  const composed: ContractRecords[] = [];
  for (const entry of entries) {
    const refused = (operation: string): Error =>
      new Error(
        `entry ${entry.name} (contract ${entry.number}) ${operation}. A fixture applies over the entries before it in the build's order, stamped or by id: an entry that changes what another added comes after it in both orders, so give it a later id (scripts/daemon-contract-entry.sh) and write its fixture again (go test ./internal/api -update-entry=<id>)`
      );
    applyAnswers(entry, answers, refused);
    applyRoutes(entry, routes, refused);
    answerRows = withAnswerRows(entry, answerRows, refused);
    enumeratedRows = withEnumeratedRows(entry, enumeratedRows, refused);
    composed.push({
      contract: entry.number,
      records: sorted(answers.keys()).map((answer) => {
        const record = answers.get(answer) as AnswerRecord;
        return {
          contract: entry.number,
          answer,
          noBody: record.noBody,
          keyPaths: sorted(record.keyPaths),
          domains: Object.fromEntries(
            sorted(record.domains.keys()).map((key) => [
              key,
              sorted(record.domains.get(key) as Set<string>),
            ])
          ),
        };
      }),
      requests: sorted(routes.keys()).map((route) => ({
        contract: entry.number,
        route,
        fieldPaths: sorted(routes.get(route) as Set<string>),
      })),
      answers: answerRows,
      enumeratedFields: enumeratedRows,
    });
  }
  return composed;
}

/** `entry`'s answers applied to `answers`, each condition read against the record before it. */
function applyAnswers(
  entry: NumberedEntry,
  answers: Map<string, AnswerRecord>,
  refused: (operation: string) => Error
): void {
  const named = entry.fixture.answers.map((change) => change.answer);
  for (const answer of repeated(named)) throw refused(`names the ${answer} answer twice`);
  for (const change of entry.fixture.answers) {
    const before = answers.get(change.answer);
    const addKeyPaths = change.addKeyPaths ?? [];
    const removeKeyPaths = change.removeKeyPaths ?? [];
    const addValues = change.addValues ?? {};
    const removeValues = change.removeValues ?? {};
    for (const keyPath of repeated([...addKeyPaths, ...removeKeyPaths])) {
      throw refused(`names \`${keyPath}\` of the ${change.answer} answer twice`);
    }
    if (change.change === "remove") {
      if (before === undefined) {
        throw refused(
          `removes the ${change.answer} answer, and the record before it holds no such answer`
        );
      }
      if (
        change.noBody !== undefined ||
        addKeyPaths.length + removeKeyPaths.length > 0 ||
        Object.keys(addValues).length + Object.keys(removeValues).length > 0
      ) {
        throw refused(`removes the ${change.answer} answer and changes it too`);
      }
      answers.delete(change.answer);
      continue;
    }
    let record: AnswerRecord;
    if (change.change === "add") {
      if (before !== undefined) {
        throw refused(
          `adds the ${change.answer} answer, and the record before it holds that answer already`
        );
      }
      if (change.noBody === undefined) {
        throw refused(`adds the ${change.answer} answer and names no noBody for it`);
      }
      if (removeKeyPaths.length > 0 || Object.keys(removeValues).length > 0) {
        throw refused(`adds the ${change.answer} answer and removes from it too`);
      }
      record = { noBody: change.noBody, keyPaths: new Set(), domains: new Map() };
    } else {
      if (before === undefined) {
        throw refused(
          `changes the ${change.answer} answer, and the record before it holds no such answer`
        );
      }
      if (change.noBody !== undefined && change.noBody === before.noBody) {
        throw refused(
          `sets noBody of the ${change.answer} answer to ${change.noBody}, and the record before it holds ${before.noBody} already`
        );
      }
      record = {
        noBody: change.noBody ?? before.noBody,
        keyPaths: new Set(before.keyPaths),
        domains: new Map([...before.domains].map(([key, held]) => [key, new Set(held)])),
      };
    }
    for (const keyPath of addKeyPaths) {
      if (before?.keyPaths.has(keyPath)) {
        throw refused(
          `adds \`${keyPath}\` to the ${change.answer} answer, and the record before it holds that key path already`
        );
      }
      record.keyPaths.add(keyPath);
    }
    for (const keyPath of removeKeyPaths) {
      if (!before?.keyPaths.has(keyPath)) {
        throw refused(
          `removes \`${keyPath}\` from the ${change.answer} answer, and the record before it holds no such key path`
        );
      }
      record.keyPaths.delete(keyPath);
    }
    for (const keyPath of sorted(Object.keys(addValues))) {
      const held = record.domains.get(keyPath) ?? new Set<string>();
      const values = addValues[keyPath] ?? [];
      for (const value of repeated(values)) {
        throw refused(`names \`${value}\` at \`${keyPath}\` of the ${change.answer} answer twice`);
      }
      for (const value of values) {
        if (before?.domains.get(keyPath)?.has(value)) {
          throw refused(
            `adds \`${value}\` at \`${keyPath}\` of the ${change.answer} answer, and the record before it admits that value already`
          );
        }
        held.add(value);
      }
      record.domains.set(keyPath, held);
    }
    for (const keyPath of sorted(Object.keys(removeValues))) {
      const held = record.domains.get(keyPath);
      for (const value of removeValues[keyPath] ?? []) {
        if (!before?.domains.get(keyPath)?.has(value)) {
          throw refused(
            `removes \`${value}\` at \`${keyPath}\` of the ${change.answer} answer, and the record before it admits no such value`
          );
        }
        held?.delete(value);
      }
      // A domain the entry empties is no domain: a record that holds none for a field admits no
      // value of it, as one holding an empty domain does.
      if (held?.size === 0) record.domains.delete(keyPath);
    }
    answers.set(change.answer, record);
  }
}

/** `entry`'s routes applied to `routes`, each condition read against the record before it. */
function applyRoutes(
  entry: NumberedEntry,
  routes: Map<string, Set<string>>,
  refused: (operation: string) => Error
): void {
  for (const route of repeated(entry.fixture.routes.map((change) => change.route))) {
    throw refused(`names ${route} twice`);
  }
  for (const change of entry.fixture.routes) {
    const before = routes.get(change.route);
    const addFieldPaths = change.addFieldPaths ?? [];
    const removeFieldPaths = change.removeFieldPaths ?? [];
    for (const fieldPath of repeated([...addFieldPaths, ...removeFieldPaths])) {
      throw refused(`names \`${fieldPath}\` of ${change.route} twice`);
    }
    if (change.change === "remove") {
      if (before === undefined) {
        throw refused(`removes ${change.route}, and the record before it holds no such route`);
      }
      if (addFieldPaths.length + removeFieldPaths.length > 0) {
        throw refused(`removes ${change.route} and changes it too`);
      }
      routes.delete(change.route);
      continue;
    }
    if (change.change === "add") {
      if (before !== undefined) {
        throw refused(`adds ${change.route}, and the record before it holds that route already`);
      }
      if (removeFieldPaths.length > 0) {
        throw refused(`adds ${change.route} and removes from it too`);
      }
    } else if (before === undefined) {
      throw refused(`changes ${change.route}, and the record before it holds no such route`);
    }
    const fieldPaths = new Set(before);
    for (const fieldPath of addFieldPaths) {
      if (before?.has(fieldPath)) {
        throw refused(
          `adds \`${fieldPath}\` to ${change.route}, and the record before it holds that field path already`
        );
      }
      fieldPaths.add(fieldPath);
    }
    for (const fieldPath of removeFieldPaths) {
      if (!before?.has(fieldPath)) {
        throw refused(
          `removes \`${fieldPath}\` from ${change.route}, and the record before it holds no such field path`
        );
      }
      fieldPaths.delete(fieldPath);
    }
    routes.set(change.route, fieldPaths);
  }
}

/** `rows` with `entry`'s rows of `answers.json` added, each arriving at the entry's number. */
function withAnswerRows(
  entry: NumberedEntry,
  rows: readonly AnswerRow[],
  refused: (operation: string) => Error
): AnswerRow[] {
  const after = [...rows];
  for (const id of repeated(entry.fixture.answerRows.map((row) => row.id))) {
    throw refused(`adds two rows of the ${id} answer to answers.json`);
  }
  for (const added of entry.fixture.answerRows) {
    const typed = (added.typedSettings ?? []).map(({ key }) => ({ key, since: entry.number }));
    const at = after.findIndex((row) => row.id === added.id);
    const before = after[at];
    if (before === undefined) {
      after.push({
        id: added.id,
        routes: [...added.routes],
        since: entry.number,
        noBody: added.noBody,
        ...(typed.length > 0 ? { typedSettings: typed } : {}),
      });
      continue;
    }
    if (added.noBody !== before.noBody) {
      throw refused(
        `adds to the ${added.id} row of answers.json with noBody ${added.noBody}, and that row holds ${before.noBody}`
      );
    }
    for (const route of added.routes) {
      if (before.routes.includes(route)) {
        throw refused(
          `adds ${route} to the ${added.id} row of answers.json, and that row holds it already`
        );
      }
    }
    for (const { key } of typed) {
      if ((before.typedSettings ?? []).some((setting) => setting.key === key && !setting.until)) {
        throw refused(
          `types \`settings.${key}\` on the ${added.id} row of answers.json, and that row types it already`
        );
      }
    }
    const typedSettings = [...(before.typedSettings ?? []), ...typed];
    after[at] = {
      ...before,
      routes: [...before.routes, ...added.routes],
      ...(typedSettings.length > 0 ? { typedSettings } : {}),
    };
  }
  return after;
}

/** `rows` with `entry`'s rows of `enumerated-fields.json` added, each value arriving at the
 *  entry's number and each row's values sorted by value; `refused` names a row that adds a value
 *  the row already holds. */
function withEnumeratedRows(
  entry: NumberedEntry,
  rows: readonly EnumeratedFieldRow[],
  refused: (operation: string) => Error
): EnumeratedFieldRow[] {
  const after = [...rows];
  const fields = entry.fixture.enumeratedRows.map((row) => `${row.answer} ${row.keyPath}`);
  for (const field of repeated(fields)) {
    throw refused(`adds two rows of ${field} to enumerated-fields.json`);
  }
  for (const added of entry.fixture.enumeratedRows) {
    const at = after.findIndex(
      (row) => row.answer === added.answer && row.keyPath === added.keyPath
    );
    const before = after[at];
    // The row grows value by value, as the daemon's gate grows it (`withEnumeratedRows` in
    // `internal/api`), so a value an added row names twice is refused as one the row before held.
    const held = new Set(before?.values.map(({ value }) => value));
    for (const value of added.values) {
      if (held.has(value)) {
        throw refused(
          `adds \`${value}\` to \`${added.keyPath}\` of the ${added.answer} answer in enumerated-fields.json, and that row holds it already`
        );
      }
      held.add(value);
    }
    const values = [
      ...(before?.values ?? []),
      ...added.values.map((value) => ({ value, since: entry.number })),
    ].sort((a, b) => byString(a.value, b.value));
    const row = { answer: added.answer, keyPath: added.keyPath, values };
    if (before === undefined) after.push(row);
    else after[at] = row;
  }
  return after;
}

/** One entry fixture of a directory, by its name, with its id and slug. */
export interface EntryFixtureFile {
  readonly name: string;
  readonly id: string;
  readonly slug: string;
  readonly fixture: EntryFixture;
}

/** Every entry fixture of `dir`, in id order, each parsed with `EntryFixture`; none where `dir`
 *  is absent, as it is in a tree that holds no entry. `only` says whether the directory holds
 *  entry fixtures and nothing else, so another file of it is refused. */
export function entryFixtures(dir: string, only: boolean): EntryFixtureFile[] {
  if (!existsSync(dir)) return [];
  const files: EntryFixtureFile[] = [];
  for (const name of sorted(readdirSync(dir))) {
    const match = ENTRY_FIXTURE_NAME.exec(name);
    if (match === null) {
      if (only) throw new Error(`${path.join(dir, name)} is no entry fixture <id>_<slug>.json`);
      continue;
    }
    const parsed = EntryFixture.safeParse(JSON.parse(readFileSync(path.join(dir, name), "utf8")));
    if (!parsed.success) {
      throw new Error(`${path.join(dir, name)} is no entry fixture: ${parsed.error.message}`);
    }
    files.push({
      name: name.slice(0, -".json".length),
      id: match[1] as string,
      slug: match[2] as string,
      fixture: parsed.data,
    });
  }
  return files;
}

/** The records of the build this checkout is, by `daemonApiRecords`. */
export interface DaemonApiRecords {
  /** L, the greatest number of `released-contracts.json`. */
  readonly last: number;
  /** The build's own number, L plus its count of entries. */
  readonly contract: number;
  /** Each entry, in id order, with its number. */
  readonly entries: readonly { readonly name: string; readonly number: number }[];
  /** Every record of `contract-shapes.json`, then the record of each answer at each entry's
   *  number. */
  readonly records: readonly ContractShape[];
  /** Every record of `request-shapes.json`, then the record of each route at each entry's
   *  number. */
  readonly requests: readonly RequestShape[];
  /** The rows of `answers.json`, then each entry's. */
  readonly answers: readonly AnswerRow[];
  /** The rows of `enumerated-fields.json`, then each entry's. */
  readonly enumeratedFields: readonly EnumeratedFieldRow[];
}

/** The records of the build whose daemon API fixtures are `fixtureDir`
 *  (`fixtures/daemon-api`): the frozen records, and those its entry fixtures compose over L's,
 *  ranked by id. */
export function daemonApiRecords(fixtureDir: string): DaemonApiRecords {
  const read = (name: string): unknown =>
    JSON.parse(readFileSync(path.join(fixtureDir, name), "utf8"));
  const released = z
    .array(z.number().int().positive())
    .min(1)
    .parse(read("released-contracts.json"));
  const last = Math.max(...released);
  const records = read("contract-shapes.json") as ContractShape[];
  const requests = read("request-shapes.json") as RequestShape[];
  const answers = read("answers.json") as AnswerRow[];
  const enumeratedFields = read("enumerated-fields.json") as EnumeratedFieldRow[];
  const base: ContractRecords = {
    contract: last,
    records: records.filter((record) => record.contract === last),
    requests: requests.filter((request) => request.contract === last),
    answers,
    enumeratedFields,
  };
  if (base.records.length === 0) {
    throw new Error(`contract-shapes.json holds no record of ${last}, L`);
  }
  const entries = entryFixtures(path.join(fixtureDir, "contracts"), true).map((file, rank) => ({
    name: file.name,
    number: last + rank + 1,
    fixture: file.fixture,
  }));
  const composed = composeEntries(base, entries);
  const newest = composed.at(-1) ?? base;
  return {
    last,
    contract: newest.contract,
    entries: entries.map(({ name, number }) => ({ name, number })),
    records: [...records, ...composed.flatMap((contract) => contract.records)],
    requests: [...requests, ...composed.flatMap((contract) => contract.requests)],
    answers: newest.answers,
    enumeratedFields: newest.enumeratedFields,
  };
}
