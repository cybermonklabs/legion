/**
 * priorities-seed.ts writes the first priorities from the table a person keeps by hand
 * (dispatch://INFRA-2947/spec, R10). Its one argument is that table's Dispatch document
 * reference, so the script names no deployment, no document and no row of its own.
 *
 * One priority is written for each row whose `Where` cell does not begin with the word `DONE`,
 * in the table's order, with the row's `Priority` cell as the title, the row's own text as the
 * description and the row's `Owner` cell verbatim as the owner. Every issue key written anywhere
 * in the row, a key inside a `dispatch://` reference included, is related to that priority unless
 * its Dispatch status is `done`: `backlog` and `icebox` are related, because priority work that
 * stands in either is what the second fault of that spec reads.
 *
 * Each priority's description ends in the record below, which is what the check of R10 reads and
 * what makes a second run write no second priority and no second relation:
 *
 *     ---
 *
 *     Table row 11 of dispatch://KEY/artifact/some-table-md, read at version 70.
 *     Advances: INFRA-2947, INFRA-2969.
 *     Not advanced: BET-1030 (done), FOO-12 (no issue carries this key).
 *
 * The row number is the idempotency key: a run against a table whose rows moved finds each row's
 * priority by that number and writes nothing, so the priorities it already wrote keep their
 * order.
 *
 * Run: bun scripts/priorities-seed.ts dispatch://KEY/artifact/some-table-md
 */
import { parseArgs } from "node:util";
import type {
  Artifact,
  ArtifactText,
  ArtifactVersionText,
} from "../packages/contracts/src/dispatch-api";
import type { ActiveDispatchConfig } from "../packages/envoy-client/src/dispatch-config";
import { DispatchClient } from "../packages/envoy-client/src/dispatch-http";
import { parseDispatchRef } from "../packages/envoy-client/src/dispatch-owner";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../packages/envoy-client/src/session-settings";

/** Every issue key written in a row, a key inside a `dispatch://` reference included. */
const KEY_PATTERN = /\b[A-Z][A-Z0-9]*-[0-9]+\b/g;

/** The columns this seed reads, as the table's header writes them; `\#` is how a table escapes
 *  the first one's name. A document holding no table with all four is refused. */
const COLUMNS = ["#", "priority", "owner", "where"] as const;

/** A row the seed passes over: its `Where` cell begins with the word `DONE`. */
const DONE_PATTERN = /^DONE\b/;

/** The record's first line, read back off a priority's description to find the row it was
 *  written from. The version is not part of the key: the same row read at a later version is the
 *  same row. */
const MARKER_PATTERN = /^Table row (\S+) of (dispatch:\/\/\S+?), read at version (?:\d+|none)\.$/m;

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "credential">;

/** One row of the table, each cell as it is written. */
export interface TableRow {
  /** The `#` cell: the row number, which is this seed's idempotency key. */
  readonly number: string;
  readonly title: string;
  readonly owner: string;
  readonly where: string;
}

/** Which row of which document a priority was written from, as its description records it. */
export interface RowMarker {
  readonly row: string;
  readonly document: string;
}

/** A key the row names that the seed did not relate, with why. */
export interface ExcludedKey {
  readonly key: string;
  /** The key's Dispatch status, or `no issue carries this key`. */
  readonly reason: string;
}

/** What one row of the table left behind. */
export interface SeededRow {
  readonly row: string;
  readonly title: string;
  /** The priority's id, the one this run wrote or the one the row already had. */
  readonly priority: string;
  /** False where a run before this one wrote the priority, so this run wrote none. */
  readonly written: boolean;
  readonly related: readonly string[];
  readonly excluded: readonly ExcludedKey[];
  /** The keys this run wrote a relation for: fewer than `related` where one already stood. */
  readonly relations: readonly string[];
}

/** What one run wrote, which its own output prints and its test reads. */
export interface SeedReport {
  /** The table's document reference, without a version: what each record names. */
  readonly document: string;
  /** The version read, null for a document that carries none. */
  readonly version: number | null;
  /** The `#` cell of every row passed over, whose `Where` cell begins with `DONE`. */
  readonly passed: readonly string[];
  readonly rows: readonly SeededRow[];
}

/** The `advances` relation and the status of one issue, as the seed reads and writes them. The
 *  TypeScript contract carries neither field yet (the Go model does: `model.Issue`), so the two
 *  routes that name them are read through the request helper below rather than through
 *  DispatchClient, which would type the answer without them. */
interface SeedIssue {
  readonly status: string;
  readonly advances: readonly { readonly id: string; readonly inherited: boolean }[] | null;
  readonly advances_revision: number;
}

/** A priority as `GET` and `POST /api/v1/priorities` answer one (`model.Priority`), of which the
 *  seed reads the id, to relate work to it, and the description, to find the row it holds. */
interface SeedPriority {
  readonly id: string;
  readonly description: string;
}

interface SeedPriorities {
  readonly priorities: readonly SeedPriority[];
}

/** The document a reference names: the owner to read it from, the slug, and the version where the
 *  reference pins one. */
interface DocumentReference {
  /** The reference as a record writes it: the owner and the slug, with no version. */
  readonly document: string;
  readonly owner:
    | { readonly kind: "issue"; readonly issue: string }
    | { readonly kind: "project"; readonly project: string };
  readonly slug: string;
  readonly version?: number;
}

/** `ref` as the document to read, or a refusal naming the grammar. */
export function parseDocumentReference(ref: string): DocumentReference {
  const parsed = parseDispatchRef(ref);
  if (parsed === null || (parsed.kind !== "artifact" && parsed.kind !== "spec")) {
    throw new Error(
      `${JSON.stringify(ref)} is no document reference: name the table as ` +
        "dispatch://KEY/artifact/SLUG, dispatch://PROJECT/artifact/SLUG or dispatch://KEY/spec"
    );
  }
  const holder = parsed.owner.kind === "issue" ? parsed.owner.issue : parsed.owner.project;
  const slug = parsed.kind === "spec" ? "spec" : parsed.id;
  return {
    document:
      parsed.kind === "spec"
        ? `dispatch://${holder}/spec`
        : `dispatch://${holder}/artifact/${slug}`,
    owner: parsed.owner,
    slug,
    ...(parsed.version === undefined ? {} : { version: parsed.version }),
  };
}

/** `line`'s cells where it is a table row, else null. A `\|` is a pipe in a cell rather than the
 *  edge of one, and the empty strings outside the row's own leading and trailing pipes are not
 *  cells. */
function tableCells(line: string): string[] | null {
  const text = line.trim();
  if (!text.startsWith("|")) return null;
  const cells: string[] = [];
  let cell = "";
  for (let at = 0; at < text.length; at += 1) {
    if (text[at] === "\\" && text[at + 1] === "|") {
      cell += "|";
      at += 1;
      continue;
    }
    if (text[at] === "|") {
      cells.push(cell);
      cell = "";
      continue;
    }
    cell += text[at];
  }
  cells.push(cell);
  cells.shift();
  if (cells.at(-1)?.trim() === "") cells.pop();
  return cells.map((part) => part.trim());
}

/** Where each column the seed reads stands in `header`, or null where one is missing. A header
 *  cell is matched with its escape dropped, so the `\#` a table writes is the `#` column. */
function columnPlaces(header: readonly string[]): Record<string, number> | null {
  const places: Record<string, number> = {};
  for (const column of COLUMNS) {
    const at = header.findIndex((cell) => cell.replace(/^\\/, "").toLowerCase() === column);
    if (at === -1) return null;
    places[column] = at;
  }
  return places;
}

/**
 * The rows of the first table in `markdown` carrying all four columns, in the table's order.
 * Refuses a document that holds no such table, a row with an empty `#` cell and two rows with
 * the same one, because the row number is what a second run reads to write nothing twice.
 */
export function parseTable(markdown: string): TableRow[] {
  const lines = markdown.split("\n");
  for (let at = 0; at + 1 < lines.length; at += 1) {
    const header = tableCells(lines[at] ?? "");
    const under = tableCells(lines[at + 1] ?? "");
    // A table's header stands over a row of `---` cells and nothing else.
    if (header === null || under === null) continue;
    if (under.length === 0 || !under.every((cell) => /^:?-+:?$/.test(cell))) continue;
    const places = columnPlaces(header);
    if (places === null) continue;
    const rows: TableRow[] = [];
    const numbers = new Set<string>();
    for (let row = at + 2; row < lines.length; row += 1) {
      const cells = tableCells(lines[row] ?? "");
      if (cells === null) break;
      const cell = (column: string): string => cells[places[column] ?? 0] ?? "";
      const number = cell("#");
      if (number === "") throw new Error(`table row ${rows.length + 1} has an empty # cell`);
      if (/\s/.test(number)) {
        throw new Error(
          `the # cell ${JSON.stringify(number)} holds whitespace: a row number is one word, ` +
            "because each priority's record names it"
        );
      }
      if (numbers.has(number)) throw new Error(`two rows carry the # cell ${number}`);
      numbers.add(number);
      const title = cell("priority");
      // A priority with no title is one Dispatch refuses, so the row is refused here, where
      // nothing has been written yet, rather than part way through a run.
      if (title === "") throw new Error(`row ${number} has an empty Priority cell`);
      rows.push({ number, title, owner: cell("owner"), where: cell("where") });
    }
    return rows;
  }
  throw new Error("this document holds no table with the columns #, Priority, Owner and Where");
}

/** Every issue key the row names, each once, in the order they are written. */
export function rowKeys(row: TableRow): string[] {
  const text = [row.number, row.title, row.owner, row.where].join("\n");
  return [...new Set(text.match(KEY_PATTERN) ?? [])];
}

/** The record this seed writes at the end of a priority's description. */
export function priorityDescription(
  row: TableRow,
  document: string,
  version: number | null,
  related: readonly string[],
  excluded: readonly ExcludedKey[]
): string {
  const advances = related.length === 0 ? "none" : related.join(", ");
  const held = excluded.map((key) => `${key.key} (${key.reason})`);
  const record = [
    `Table row ${row.number} of ${document}, read at version ${version ?? "none"}.`,
    `Advances: ${advances}.`,
    `Not advanced: ${held.length === 0 ? "none" : held.join(", ")}.`,
  ].join("\n");
  return [row.title, row.where, "---", record].filter((part) => part !== "").join("\n\n");
}

/** The row a priority was written from, as its description records it, or undefined for a
 *  priority this seed did not write. */
export function rowMarker(description: string): RowMarker | undefined {
  const marker = MARKER_PATTERN.exec(description);
  if (marker === null) return undefined;
  const [, row, document] = marker;
  if (row === undefined || document === undefined) return undefined;
  return { row, document };
}

/**
 * A request to Dispatch, presenting the bearer or the access token this machine traded its own
 * identity for, with the proof that goes with it. The three routes it carries - the issue read,
 * the issue patch and the priority write - name `advances` and `model.Priority`, which the
 * TypeScript contract does not carry yet, so they go around DispatchClient, which types neither;
 * the document read above is the client's.
 */
async function request(
  config: DispatchConfig,
  method: string,
  path: string,
  body?: unknown
): Promise<Response> {
  const url = `${config.url}${path}`;
  return fetch(url, {
    method,
    headers: {
      ...(await config.credential.headers(method, url)),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function send<T>(
  config: DispatchConfig,
  method: string,
  path: string,
  body?: unknown
): Promise<T> {
  const response = await request(config, method, path, body);
  if (!response.ok) {
    throw new Error(`${method} ${path}: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as T;
}

/** The issue `key`, or undefined where no issue carries that key. */
async function readIssue(config: DispatchConfig, key: string): Promise<SeedIssue | undefined> {
  const path = `/api/v1/issues/${encodeURIComponent(key)}`;
  const response = await request(config, "GET", path);
  if (response.status === 404) {
    await response.text();
    return undefined;
  }
  if (!response.ok) {
    throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as SeedIssue;
}

/**
 * Every issue the run reads, read once however many rows name it, and kept current as relations
 * are written: a second relation on one issue carries the revision the first write answered.
 */
class Issues {
  readonly #config: DispatchConfig;
  readonly #read = new Map<string, SeedIssue | undefined>();

  constructor(config: DispatchConfig) {
    this.#config = config;
  }

  async issue(key: string): Promise<SeedIssue | undefined> {
    if (!this.#read.has(key)) this.#read.set(key, await readIssue(this.#config, key));
    return this.#read.get(key);
  }

  /**
   * States that `key` advances `priority`, where it does not already. Answers whether this wrote
   * a relation. The write carries the revision the read it answers returned, so a set changed
   * since that read is refused rather than written over.
   */
  async relate(key: string, priority: string): Promise<boolean> {
    const issue = await this.issue(key);
    if (issue === undefined) throw new Error(`no issue carries the key ${key}`);
    // The issue's own relations: an inherited one is no relation of its own, so no write of
    // this issue states or retracts it.
    const own = (issue.advances ?? []).filter((entry) => !entry.inherited).map((entry) => entry.id);
    if (own.includes(priority)) return false;
    const written = await send<SeedIssue>(
      this.#config,
      "PATCH",
      `/api/v1/issues/${encodeURIComponent(key)}`,
      { advances: [...own, priority], advances_revision: issue.advances_revision }
    );
    this.#read.set(key, written);
    return true;
  }
}

/** The table's text and the version it was read at. */
async function readTable(
  config: DispatchConfig,
  reference: DocumentReference
): Promise<{ markdown: string; version: number | null }> {
  const client = new DispatchClient(config.url, config.credential);
  const owner = reference.owner;
  const artifact: Artifact =
    owner.kind === "issue"
      ? await client.getIssueArtifact(owner.issue, reference.slug)
      : await client.getProjectArtifact(owner.project, reference.slug);
  if (reference.version !== undefined) {
    const pinned: ArtifactVersionText = await client.docRead(artifact.id, reference.version);
    return { markdown: pinned.markdown, version: pinned.number };
  }
  const text: ArtifactText = await client.docRead(artifact.id);
  return { markdown: text.markdown, version: text.version };
}

/**
 * Writes the priorities of `reference`'s table, and the relations from the work each row names.
 * A row whose priority a run before this one wrote keeps that priority, its order and its text,
 * and this run states only the relations that are missing from it.
 */
export async function seedPriorities(
  config: DispatchConfig,
  reference: string
): Promise<SeedReport> {
  const document = parseDocumentReference(reference);
  const table = await readTable(config, document);
  const rows = parseTable(table.markdown);
  const held = new Map<string, string>();
  for (const priority of (await send<SeedPriorities>(config, "GET", "/api/v1/priorities"))
    .priorities) {
    const marker = rowMarker(priority.description);
    if (marker !== undefined && marker.document === document.document) {
      held.set(marker.row, priority.id);
    }
  }
  const issues = new Issues(config);
  const passed: string[] = [];
  const seeded: SeededRow[] = [];
  for (const row of rows) {
    if (DONE_PATTERN.test(row.where)) {
      passed.push(row.number);
      continue;
    }
    const related: string[] = [];
    const excluded: ExcludedKey[] = [];
    for (const key of rowKeys(row)) {
      const issue = await issues.issue(key);
      if (issue === undefined) excluded.push({ key, reason: "no issue carries this key" });
      else if (issue.status === "done") excluded.push({ key, reason: issue.status });
      else related.push(key);
    }
    const written = held.get(row.number);
    const priority =
      written ??
      (
        await send<SeedPriority>(config, "POST", "/api/v1/priorities", {
          title: row.title,
          description: priorityDescription(
            row,
            document.document,
            table.version,
            related,
            excluded
          ),
          owner: row.owner,
        })
      ).id;
    const relations: string[] = [];
    for (const key of related) if (await issues.relate(key, priority)) relations.push(key);
    seeded.push({
      row: row.number,
      title: row.title,
      priority,
      written: written === undefined,
      related,
      excluded,
      relations,
    });
  }
  return { document: document.document, version: table.version, passed, rows: seeded };
}

function parseArguments(argv: readonly string[]): string {
  const { positionals } = parseArgs({ args: [...argv], allowPositionals: true, strict: true });
  const [reference] = positionals;
  if (positionals.length !== 1 || reference === undefined) {
    throw new Error(
      "usage: bun scripts/priorities-seed.ts <dispatch://KEY/artifact/SLUG>: one argument, the " +
        "priorities table's document reference"
    );
  }
  return reference;
}

function printReport(report: SeedReport): void {
  console.log(`${report.document}, read at version ${report.version ?? "none"}`);
  for (const row of report.rows) {
    const wrote = row.written ? "wrote" : "kept";
    const held = row.excluded.map((key) => `${key.key} (${key.reason})`);
    console.log(
      `row ${row.row}: ${wrote} ${row.priority}; ` +
        `related ${row.relations.length === 0 ? "none" : row.relations.join(", ")} of ` +
        `${row.related.length === 0 ? "none" : row.related.join(", ")}; ` +
        `not related ${held.length === 0 ? "none" : held.join(", ")}`
    );
  }
  const wrote = report.rows.filter((row) => row.written).length;
  console.log(
    `${report.rows.length} rows, ${wrote} priorities written, ` +
      `${report.rows.reduce((count, row) => count + row.relations.length, 0)} relations written, ` +
      `${report.passed.length} rows passed over (DONE): ${report.passed.join(", ") || "none"}`
  );
}

async function run(reference: string): Promise<void> {
  const { dispatch: resolution, warnings } = sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  // A checkout's OpenCode-era file, which is not read, is said on every run while it is there.
  for (const warning of warnings) console.error(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    throw new Error(
      resolution.error === null
        ? `Dispatch is not configured: ${resolution.notice}`
        : `dispatch config: ${resolution.error}`
    );
  }
  printReport(await seedPriorities(resolution, reference));
}

if (import.meta.main) await run(parseArguments(Bun.argv.slice(2)));
