/**
 * Fills the runner's SQL with the values of one query: count.sql (the read count, for one store and
 * one UTC day or hour of file mtime), models.sql (the model of each session of the same files),
 * pull.sql (a transcript, by path), tasks.sql (first user messages that hold some terms),
 * sessions.sql (the paths of the files a count row names by hash) and session-head.sql (the head
 * of each session a case or a load task names by its path's SHA-256). Each placeholder is the
 * name of a value inside double braces, and each is filled with one whole SQL value: a number, or
 * a string literal with its quotes (`sqlString`), so no value can end the literal it is in. The
 * filled text goes into the store's read command unchanged.
 *
 *   bun scripts/skill-cuts/countsql.ts --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD>
 *     [--hour <0-23>] [--query count|models]
 *
 * prints count.sql (or, with `--query models`, models.sql) filled for that store and day (or for
 * the one hour of that day, which is how a day whose query times out is split), count.sql with the
 * window of the configuration. A store the configuration does not name is refused. Exit 0 prints
 * the query; exit 2 is a refusal before anything is printed.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { loadConfig, type SkillCutsConfig, storeNamed } from "./config";
import type { PullKey } from "./psql-rows";

/** The SQL files of the runner, beside this file. */
type Template =
  | "count.sql"
  | "models.sql"
  | "pull.sql"
  | "tasks.sql"
  | "sessions.sql"
  | "session-head.sql";

/** The text of one of the runner's SQL files. */
function readTemplate(name: Template): string {
  return readFileSync(path.join(import.meta.dir, name), "utf8");
}

/** A SQL string literal holding `value`: in quotes, each quote doubled. The query runs with
 *  standard_conforming_strings on (the server's default), so a backslash is a backslash. */
export function sqlString(value: string): string {
  if (value.includes("\u0000")) throw new Error("a SQL string cannot hold a NUL character");
  return `'${value.replaceAll("'", "''")}'`;
}

/** A SQL text array holding `values`, each a string literal. */
function sqlTextArray(values: readonly string[]): string {
  return `ARRAY[${values.map(sqlString).join(", ")}]::text[]`;
}

const PLACEHOLDER = /\{\{([a-z_]+)\}\}/g;

/**
 * `template` with each placeholder replaced by its value, which must already be one whole SQL
 * value. Throws when a placeholder has no value, or a value has no placeholder: either is a caller
 * that fills the wrong file.
 */
export function fillTemplate(template: string, values: Readonly<Record<string, string>>): string {
  const used = new Set<string>();
  const filled = template.replace(PLACEHOLDER, (_whole, name: string) => {
    const value = values[name];
    if (value === undefined) throw new Error(`no value for the placeholder ${name}`);
    used.add(name);
    return value;
  });
  const unused = Object.keys(values).filter((name) => !used.has(name));
  if (unused.length > 0) throw new Error(`no placeholder for ${unused.join(", ")}`);
  return filled;
}

/** The mtime range of one query, epoch milliseconds, start inclusive and end exclusive. */
interface MtimeRange {
  readonly from: number;
  readonly to: number;
}

const DAY_MS = 86_400_000;
const HOUR_MS = 3_600_000;

/** The mtime range of the UTC day `day` (YYYY-MM-DD), or of its hour `hour` (0 to 23). Throws on a
 *  day that is not a calendar day or an hour out of range. */
export function mtimeRange(day: string, hour?: number): MtimeRange {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  const start = match ? Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3])) : NaN;
  if (Number.isNaN(start) || new Date(start).toISOString().slice(0, 10) !== day) {
    throw new Error(`--day ${JSON.stringify(day)} is not a calendar day as YYYY-MM-DD`);
  }
  if (hour === undefined) return { from: start, to: start + DAY_MS };
  if (!Number.isInteger(hour) || hour < 0 || hour > 23) {
    throw new Error(`--hour ${hour} is not an hour from 0 to 23`);
  }
  return { from: start + hour * HOUR_MS, to: start + (hour + 1) * HOUR_MS };
}

/** An instant as count.sql and tasks.sql compare it: a string literal of its ISO form. Throws on a
 *  text that is not an instant. */
function instant(text: string): string {
  const ms = Date.parse(text);
  if (Number.isNaN(ms)) throw new Error(`${JSON.stringify(text)} is not an instant`);
  return sqlString(new Date(ms).toISOString());
}

/** count.sql filled for the store `store`, which the configuration must name, over `range`, with
 *  the configuration's window. */
function countSqlFor(config: SkillCutsConfig, store: string, range: MtimeRange): string {
  const named = storeNamed(config, store);
  return fillTemplate(readTemplate("count.sql"), {
    store: sqlString(named.name),
    day_start: String(range.from),
    day_end: String(range.to),
    window_start: instant(config.windowStart),
    window_end: instant(config.windowEnd),
  });
}

/** count.sql filled for the store `store` and the UTC day `day`, or its hour `hour`: what
 *  `countsql.ts` and `skill-cuts count-sql` print. */
export function countSql(
  config: SkillCutsConfig,
  store: string,
  day: string,
  hour?: number
): string {
  return countSqlFor(config, store, mtimeRange(day, hour));
}

/** models.sql filled for the store `store`, which the configuration must name, and the UTC day
 *  `day`, or its hour `hour`: the files of count.sql's query of that store and day. */
export function modelsSql(
  config: SkillCutsConfig,
  store: string,
  day: string,
  hour?: number
): string {
  const named = storeNamed(config, store);
  const range = mtimeRange(day, hour);
  return fillTemplate(readTemplate("models.sql"), {
    store: sqlString(named.name),
    day_start: String(range.from),
    day_end: String(range.to),
  });
}

/** The rows a transcript pull asks for in one call (pull.sql's page). */
export const PULL_PAGE = 64;

/** pull.sql filled for the file `filePath`, its rows after the place `after` (PULL_START for the
 *  first call). */
export function pullSql(filePath: string, after: PullKey): string {
  if (
    !Number.isSafeInteger(after.o) ||
    after.o < -2 ||
    !Number.isSafeInteger(after.k) ||
    after.k < 0
  ) {
    throw new Error(
      `after offset ${after.o}, slice ${after.k} is not an offset of -2 or more and a slice of 0 or more`
    );
  }
  return fillTemplate(readTemplate("pull.sql"), {
    path: sqlString(filePath),
    after_o: String(after.o),
    after_k: String(after.k),
    page: String(PULL_PAGE),
  });
}

/** tasks.sql filled for the files of `range`, first user messages in [windowStart, windowEnd)
 *  that hold one of `terms`. */
export function tasksSql(
  range: MtimeRange,
  windowStart: string,
  windowEnd: string,
  terms: readonly string[]
): string {
  if (terms.length === 0) throw new Error("the task query needs at least one term");
  return fillTemplate(readTemplate("tasks.sql"), {
    day_start: String(range.from),
    day_end: String(range.to),
    window_start: instant(windowStart),
    window_end: instant(windowEnd),
    terms: sqlTextArray(terms),
  });
}

/** sessions.sql filled for the file hashes `hashes` (16 hex characters each, as count.sql gives
 *  them). */
export function sessionsSql(hashes: readonly string[]): string {
  const bad = hashes.find((hash) => !/^[0-9a-f]{16}$/.test(hash));
  if (bad !== undefined)
    throw new Error(`${JSON.stringify(bad)} is not a file hash of 16 hex characters`);
  return fillTemplate(readTemplate("sessions.sql"), { hashes: sqlTextArray(hashes) });
}

/** session-head.sql filled for the path hashes `pathHashes` (the SHA-256 of each session file's
 *  path in hex, 64 characters, as cases.ts's `pathHash` gives it). */
export function sessionHeadSql(pathHashes: readonly string[]): string {
  const bad = pathHashes.find((hash) => !/^[0-9a-f]{64}$/.test(hash));
  if (bad !== undefined) {
    throw new Error(`${JSON.stringify(bad)} is not a path hash of 64 hex characters`);
  }
  return fillTemplate(readTemplate("session-head.sql"), { hashes: sqlTextArray(pathHashes) });
}

const USAGE =
  "usage: countsql.ts --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD> [--hour <0-23>] [--query count|models]";

/** What `main` writes to. */
export interface Output {
  out(text: string): void;
  err(text: string): void;
}

/** The command. Returns the exit status: 0 (the query is printed) or 2 (refused). */
export function main(argv: readonly string[], io: Output): number {
  const refuse = (why: string) => {
    io.err(`countsql: ${why}`);
    return 2;
  };
  let values: { config?: string; store?: string; day?: string; hour?: string; query?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        config: { type: "string" },
        store: { type: "string" },
        day: { type: "string" },
        hour: { type: "string" },
        query: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    return refuse(`${(error as Error).message}. ${USAGE}`);
  }
  if (values.config === undefined || values.store === undefined || values.day === undefined) {
    return refuse(`--config, --store and --day are required. ${USAGE}`);
  }
  if (values.hour !== undefined && !/^\d{1,2}$/.test(values.hour)) {
    return refuse(`--hour ${JSON.stringify(values.hour)} is not an hour from 0 to 23`);
  }
  const query = values.query ?? "count";
  if (query !== "count" && query !== "models") {
    return refuse(`--query ${JSON.stringify(query)} is neither count nor models`);
  }
  try {
    const config = loadConfig(values.config);
    const hour = values.hour === undefined ? undefined : Number(values.hour);
    io.out((query === "models" ? modelsSql : countSql)(config, values.store, values.day, hour));
    return 0;
  } catch (error) {
    return refuse((error as Error).message);
  }
}

if (import.meta.main) {
  process.exit(
    main(Bun.argv.slice(2), {
      out: (text) => process.stdout.write(text),
      err: (text) => process.stderr.write(`${text}\n`),
    })
  );
}
