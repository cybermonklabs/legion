/**
 * Loads fixture transcripts into a test database in the shape of the session store, so the count's
 * proof, F7's capture and the tester's re-run all load them one way.
 *
 *   bun scripts/skill-cuts/fixturedb.ts --dsn <url> [--parts <n>] [--line <text>] <file>...
 *
 * It makes the two tables the count reads in the database `--dsn` names, when they are missing,
 * with the store's columns: public.omp_session_files (path, content, mtime_ms, title, title_source,
 * title_updated_at, byte_len) and public.omp_session_files_parts (path, start_offset, content),
 * keyed on (path, start_offset). Each file is stored as the store keeps a big file: the file row's
 * content empty, and every byte in parts, start_offset being the part's byte offset. A file loaded
 * again replaces its rows.
 *
 * Each file is split into `--parts` parts (2 when not given) inside one line: the first line that
 * holds `--line`'s text, or else the line that holds the file's middle byte. That line is cut into
 * equal pieces (each cut moved back to a UTF-8 character's start), so a reader that does not join a
 * line split between rows loses it.
 *
 * The rows: `path` is the file's path below this directory's `testdata/` (its absolute path when
 * it is elsewhere); `mtime_ms` is the `timestamp` of the file's last line that has one (an ISO
 * instant, or epoch milliseconds), as a session file's mtime is its last write; `byte_len` is its
 * size; the title columns are null.
 *
 * It refuses a DSN equal to the URL in the file `$OMP_SESSION_SQL_DSN_FILE`, the live session store
 * of a Legion pod. Exit 0 loads every file, exit 1 is a failed load, exit 2 a refusal before any
 * write.
 */
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { SQL } from "bun";

/** The fixtures' directory: a file below it is stored by its path below it. */
export const TESTDATA = path.join(import.meta.dir, "testdata");

/** How each file is cut. */
export interface SplitOptions {
  /** The parts of each file, 2 or more. */
  readonly parts: number;
  /** Cut the first line that holds this text; else the line that holds the middle byte. */
  readonly line?: string;
}

/** The byte offsets where each part after the first starts. Throws when no line can be cut into
 *  `options.parts` pieces. */
export function cutOffsets(bytes: Uint8Array, options: SplitOptions): number[] {
  const { parts, line } = options;
  if (!Number.isInteger(parts) || parts < 2) throw new Error(`--parts ${parts} is not 2 or more`);
  let start: number;
  if (line !== undefined) {
    const at = Buffer.from(bytes).indexOf(Buffer.from(line, "utf8"));
    if (at === -1) throw new Error(`no line holds ${JSON.stringify(line)}`);
    start = at;
  } else {
    start = Math.floor(bytes.length / 2);
  }
  while (start > 0 && bytes[start - 1] !== 0x0a) start--;
  let end = start;
  while (end < bytes.length && bytes[end] !== 0x0a) end++;
  const offsets: number[] = [];
  for (let k = 1; k < parts; k++) {
    let cut = start + Math.floor(((end - start) * k) / parts);
    while (cut > start && ((bytes[cut] ?? 0) & 0xc0) === 0x80) cut--;
    if (cut <= start || cut >= end || cut <= (offsets.at(-1) ?? start)) {
      throw new Error(`the line at byte ${start} is too short to cut into ${parts} parts`);
    }
    offsets.push(cut);
  }
  return offsets;
}

/** The `timestamp` of the last line of `text` that has one, as epoch milliseconds. Throws when no
 *  line has one. */
export function lastTimestampMs(text: string): number {
  const lines = text.split("\n");
  for (let index = lines.length - 1; index >= 0; index--) {
    let value: unknown;
    try {
      value = JSON.parse(lines[index] ?? "");
    } catch {
      continue;
    }
    if (typeof value !== "object" || value === null || !("timestamp" in value)) continue;
    const stamp = value.timestamp;
    const ms =
      typeof stamp === "number" ? stamp : typeof stamp === "string" ? Date.parse(stamp) : NaN;
    if (Number.isFinite(ms)) return ms;
  }
  throw new Error("no line has a timestamp");
}

/** The path a fixture file is stored under. */
export function storedPath(file: string): string {
  const absolute = path.resolve(file);
  const below = path.relative(TESTDATA, absolute);
  const outside = below === ".." || below.startsWith(`..${path.sep}`) || path.isAbsolute(below);
  return outside ? absolute : below;
}

/** Why `dsn` must not be loaded into, or undefined: it is the URL the file
 *  `$OMP_SESSION_SQL_DSN_FILE` holds. */
export function storeRefusal(
  dsn: string,
  env: Readonly<Record<string, string | undefined>>
): string | undefined {
  const file = env.OMP_SESSION_SQL_DSN_FILE;
  if (file === undefined || file === "" || !existsSync(file)) return undefined;
  if (readFileSync(file, "utf8").trim() === dsn.trim()) {
    return "the DSN is the session store's ($OMP_SESSION_SQL_DSN_FILE): fixtures go into a test database only";
  }
  return undefined;
}

/** One file as it was stored. */
export interface LoadedFixture {
  readonly path: string;
  readonly bytes: number;
  readonly offsets: readonly number[];
  readonly mtimeMs: number;
}

/** One file read and cut (`cutOffsets`), ready to store. */
interface PreparedFixture {
  readonly path: string;
  readonly bytes: number;
  readonly mtimeMs: number;
  readonly pieces: readonly { readonly offset: number; readonly content: string }[];
}

/** Each file of `files` read and cut as `options` says. Throws, before anything is written, on a
 *  file that cannot be read or cut, that is not UTF-8, or that has no line with a timestamp. */
function prepareFixtures(files: readonly string[], options: SplitOptions): PreparedFixture[] {
  return files.map((file) => {
    const bytes = new Uint8Array(readFileSync(file));
    const cuts = cutOffsets(bytes, options);
    const decoder = new TextDecoder("utf-8", { fatal: true });
    const text = decoder.decode(bytes);
    const bounds = [0, ...cuts, bytes.length];
    const pieces = Array.from({ length: cuts.length + 1 }, (_, k) => ({
      offset: bounds[k] ?? 0,
      content: decoder.decode(bytes.subarray(bounds[k], bounds[k + 1])),
    }));
    return { path: storedPath(file), bytes: bytes.length, mtimeMs: lastTimestampMs(text), pieces };
  });
}

/** Makes the tables in the database `dsn` names when they are missing, and stores each file. */
export async function loadFixtures(
  dsn: string,
  files: readonly string[],
  options: SplitOptions
): Promise<LoadedFixture[]> {
  return storeFixtures(dsn, prepareFixtures(files, options));
}

/** Makes the tables in the database `dsn` names when they are missing, and stores each of the
 *  files `prepared`. */
async function storeFixtures(
  dsn: string,
  prepared: readonly PreparedFixture[]
): Promise<LoadedFixture[]> {
  const sql = new SQL(dsn);
  try {
    await sql.begin(async (tx) => {
      await tx.unsafe(`
        CREATE TABLE IF NOT EXISTS public.omp_session_files (
          path text PRIMARY KEY,
          content text,
          mtime_ms bigint,
          title text,
          title_source text,
          title_updated_at text,
          byte_len bigint
        );
        CREATE TABLE IF NOT EXISTS public.omp_session_files_parts (
          path text NOT NULL,
          start_offset bigint NOT NULL,
          content text,
          PRIMARY KEY (path, start_offset)
        );
      `);
      for (const file of prepared) {
        await tx`DELETE FROM public.omp_session_files_parts WHERE path = ${file.path}`;
        await tx`DELETE FROM public.omp_session_files WHERE path = ${file.path}`;
        await tx`
          INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
          VALUES (${file.path}, '', ${file.mtimeMs}, ${file.bytes})
        `;
        for (const piece of file.pieces) {
          await tx`
            INSERT INTO public.omp_session_files_parts (path, start_offset, content)
            VALUES (${file.path}, ${piece.offset}, ${piece.content})
          `;
        }
      }
    });
  } finally {
    await sql.close();
  }
  return prepared.map((file) => ({
    path: file.path,
    bytes: file.bytes,
    offsets: file.pieces.map((piece) => piece.offset),
    mtimeMs: file.mtimeMs,
  }));
}

const USAGE = "usage: fixturedb.ts --dsn <url> [--parts <n>] [--line <text>] <file>...";

/** The command. Returns the exit status: 0, 1 (a load failed) or 2 (refused). */
export async function main(
  argv: readonly string[],
  env: Readonly<Record<string, string | undefined>>,
  io: { out(text: string): void; err(text: string): void }
): Promise<number> {
  const refuse = (why: string) => {
    io.err(`fixturedb: ${why}`);
    return 2;
  };
  let values: Arguments["values"];
  let positionals: string[];
  try {
    ({ values, positionals } = parse(argv));
  } catch (error) {
    return refuse(`${(error as Error).message}. ${USAGE}`);
  }
  if (values.dsn === undefined || positionals.length === 0) {
    return refuse(`--dsn and at least one file are required. ${USAGE}`);
  }
  if (values.parts !== undefined && !/^\d+$/.test(values.parts)) {
    return refuse(`--parts ${JSON.stringify(values.parts)} is not a number`);
  }
  const parts = values.parts === undefined ? 2 : Number(values.parts);
  if (parts < 2) return refuse(`--parts ${parts} is not 2 or more`);
  let refusal: string | undefined;
  try {
    refusal = storeRefusal(values.dsn, env);
  } catch (error) {
    return refuse(
      `$OMP_SESSION_SQL_DSN_FILE cannot be read, so the DSN cannot be checked against the session store's: ${(error as Error).message}`
    );
  }
  if (refusal !== undefined) return refuse(refusal);
  // Every file is read and cut before anything is written: one that cannot be is refused.
  let prepared: PreparedFixture[];
  try {
    prepared = prepareFixtures(positionals, { parts, line: values.line });
  } catch (error) {
    return refuse((error as Error).message);
  }
  try {
    const loaded = await storeFixtures(values.dsn, prepared);
    for (const file of loaded) {
      io.out(
        `${file.path}: ${file.bytes} bytes in ${file.offsets.length} parts at byte offsets ${file.offsets.join(", ")}, mtime_ms ${file.mtimeMs} (${new Date(file.mtimeMs).toISOString()})`
      );
    }
    return 0;
  } catch (error) {
    io.err(`fixturedb: ${(error as Error).message}`);
    return 1;
  }
}

/** The command line, parsed. */
interface Arguments {
  readonly values: { dsn?: string; parts?: string; line?: string };
  readonly positionals: string[];
}

function parse(argv: readonly string[]): Arguments {
  return parseArgs({
    args: [...argv],
    options: { dsn: { type: "string" }, parts: { type: "string" }, line: { type: "string" } },
    strict: true,
    allowPositionals: true,
  });
}

if (import.meta.main) {
  process.exit(
    await main(Bun.argv.slice(2), process.env, {
      out: (text) => console.log(text),
      err: (text) => console.error(text),
    })
  );
}
