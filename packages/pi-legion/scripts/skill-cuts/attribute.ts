/**
 * The attribution of the read count: the count's totals (count.sql's rows, one psql output per
 * store and UTC day of file mtime) set against the copies output's copies (copies.ts), into
 * one row per skill copy with its sessions, the report beside it, and each cut's ranking.
 *
 *   bun scripts/skill-cuts/attribute.ts --config <skill-cuts.json> --count <dir> --copies <dir>
 *     --spans <file> --out <dir> [--models <dir>] [--changed <cut id>=<file>]...
 *
 * `main(argv, io)` is exported so that cli.ts runs it as `skill-cuts attribute`; attribute.test.ts
 * calls the pure parts. It only reads its inputs, and it writes only under `--out`, which must not
 * be inside `--count`, `--copies` or `--models`.
 *
 * Inputs.
 *   --count    The count's raw output. `<store>/<YYYY-MM-DD>.txt` for each store of the
 *              configuration and each UTC day, or `<store>/<YYYY-MM-DD>T<HH>.txt` for each hour of
 *              a day whose query timed out: each is the psql output of one count.sql query
 *              (`psqlRows`), its count rows and then one summary row, as count.sql's header says. A
 *              query that has no `.txt` has no output: it failed, or it has not run yet. `run.log`
 *              lists every query run, `<time>\t<store>\t<day or hour>\trc=<status>\tseconds=<n>\t
 *              <its stderr>`, a `FAILED ...` line for each query given up, and `END <time>` when
 *              the run ended.
 *   --copies   The copies output (copies.ts): skill-copies.tsv, prefix-table.tsv, installers.tsv,
 *              loaders.tsv, manifests.tsv and repositories.tsv, each with the columns copies.ts
 *              exports for it.
 *   --spans    One JSON line per store: `{store, earliest_mtime_ms, files}`, the store's earliest
 *              file mtime (epoch milliseconds) and its number of files.
 *   --models   The models query's raw output (models.sql), in `--count`'s layout and with its own
 *              run.log: `<store>/<YYYY-MM-DD>[T<HH>].txt`, each one models.sql result, a row per
 *              file `{hash, model, source}` and a summary.
 *   --changed  `<cut id>=<file>`, once for each cut of the configuration to rank. The file holds
 *              the cut's changed skills: one skill directory of the cut's repository per line, as
 *              skill-copies.tsv's `path` writes it.
 *
 * Rules.
 *   A session is one transcript file: a 16-hex file hash of a count row, with its store. Each one
 *   is counted once, however many queries of its store hold it: a resumed session whose mtime day
 *   moved while the count ran is in two queries. A `read` row counts in `sessions`, and a
 *   `reference` row (a read of `skill://<name>/<other>` or of `<name>/references/...`) in
 *   `reference_sessions`. A call that has no result in its file is not a read: the report counts
 *   such sessions per store, and no copy counts them.
 *   The copy of a count row is decided in this order:
 *   1. The prefix table. When every prefix-table.tsv row whose pattern matches the row's copy path
 *      maps to the same copy, the row goes to that copy. The match is `prefixRegExp`, the one
 *      matcher that `prefixMatches` uses, compiled once for each row.
 *   2. A checkout (`checkoutRule`). When the row's copy-path prefix ends with a copy's
 *      repository-relative skill directory, at a path boundary, the row goes to that copy: a read
 *      in a checkout of its repository (a dotfiles checkout, a Legion workspace, a relative path
 *      from the session's working directory). The longest such directory wins; copies of several
 *      repositories at it are told apart by the repository's owner and name, else its name, in
 *      the path; a tie left is unattributed. A one-segment skill directory counts only when the
 *      path names its repository.
 *   3. The store's context. The units (packages, plugins, a host's user skills, each named with its
 *      manifest in prefix-table.tsv's `via`) that the store's own step-1 reads come from are the
 *      units its context installs. Their copies are the installers.tsv rows of those units and the
 *      prefix rows' own copies. When they hold exactly one copy of the row's skill name, the row
 *      goes to that copy. A store with no step-1 read installs no unit.
 *   4. Otherwise the row is unattributed for its skill name.
 *   `placer` is these four steps, and the one place a read's copy is decided: `attribute` counts
 *   each row where it puts it, and build.ts's `build-cases` keeps a session only when it puts the
 *   session's read at the cut's copy, given the same `--count` and `--copies`.
 *   Each distinct copy-path prefix (`copyPrefix`: the path up to the skill's directory) that step 1
 *   does not map is listed with its sessions, where they went and the rule that placed them; each
 *   one step 1 maps is listed with its copy. No prefix is dropped.
 *   Two copies of one name are a tie when their sessions differ by at most the number of that
 *   name's unattributed sessions.
 *   A copy is loaded when code, an agent definition or an installer loads it into a session: its
 *   skill-copies.tsv `loaded_by` is not empty, an installers.tsv row loads it (action `loads`: an
 *   agent definition's autoloadSkills, a Python loader's list), a loaders.tsv row of its name
 *   matches `code-load`, or INJECTED_SKILLS holds its name.
 *   Oh My Pi installs a copy when an Oh My Pi session read it (every store holds Oh My Pi
 *   sessions), when a store's context installs it (step 3), or when an installers.tsv row of host
 *   `omp` installs it (an installer script that installs into Oh My Pi, an Oh My Pi plugins
 *   package.json, a package.json with `omp.skills`, the prepack script). Claude Code installs a
 *   copy when an installers.tsv row of host `claude-code` installs it. Status: `read` when the copy
 *   has a session; else `not countable yet` when only Claude Code installs it; else `no Oh My Pi
 *   read; Claude Code not counted yet` when both hosts install it; else `no read`.
 *   With --models, each read session's model is its file's row of the models queries (a file in
 *   two queries takes the model one names; two models keep the first day's and are counted), the
 *   thinking suffix dropped and a Claude Code model id written `anthropic/<id>` (`sessionModel`).
 *   A session whose file's head names no model, and one in no models query's rows, are counted
 *   apart. A model is an Anthropic model when its id (`modelId`, its text after the last `/`)
 *   starts `claude-`, whatever gateway or provider names it; the gateway serves it when its id is
 *   the id of one of the configuration's `servedModels`.
 *
 * Outputs under --out.
 *   skill-read-count.tsv   READ_COUNT_COLUMNS, one row per copy of skill-copies.tsv, in its order:
 *                          repository, path, name, sessions, reference_sessions,
 *                          unattributed_sessions (of the name), tie (each copy of the name it ties
 *                          with, `<repository> <path>`, joined by `; `), loaded (yes or no),
 *                          status.
 *   skill-read-count.md    The end time and the window; the stores with their spans, earliest
 *                          record and earliest read; each failed, split or missing query; the
 *                          attribution rule with the units each store's context installs; the
 *                          unmapped prefixes with their sessions and the rule that placed them;
 *                          the mapped prefixes with their copies; the ties; the loaded list; per
 *                          repository, the skills with no read; the two Claude Code lists; per
 *                          repository, the bytes of its skill names and descriptions at the commit
 *                          the copies output read; the rankings; and, with --models, the models
 *                          of the reading sessions, the models queries' issues and each cut's
 *                          shares.
 *   <cut id>/ranking.json  For each `--changed` cut, the cut repository's changed skills, each
 *                          `{skill, sessions}` (the sessions of that repository's copy), in the
 *                          order of score.ts's `rankSkills` (most sessions first, a tie by name).
 *   <cut id>/sessions/<skill>.jsonl
 *                          For each `--changed` cut, one file per ranked changed skill
 *                          (`sessionFiles`): each read session of the cut repository's copy, after
 *                          every attribution rule, one line `{"store", "hash"}` (the 16-hex file
 *                          hash), sorted by store then hash: what `build-cases --sessions` reads.
 *   <cut id>/models/<skill>.json
 *                          With --models, for each `--changed` cut, one file per sessions file
 *                          (`modelFiles`): the ModelCount of the same sessions, as JSON (sessions,
 *                          byModel, each `{model, anthropic, served, sessions}`, noModel,
 *                          notQueried, notAnthropic, served, withModel): what `behaviour
 *                          --reading-models` reads (ModelCountFile).
 *   models-by-skill.tsv    With --models: MODELS_BY_SKILL_COLUMNS, one row per copy, in
 *                          skill-copies.tsv's order: repository, path, name, sessions, by_model
 *                          (each `<model>=<sessions>`, most first, joined by `; `),
 *                          no_model_sessions, not_queried_sessions, not_anthropic_sessions,
 *                          not_anthropic_share (of the sessions with a model, to 3 places; empty
 *                          when none has one), served_sessions, not_served_sessions and
 *                          not_served_models (each model the gateway does not serve, as in
 *                          by_model).
 *   models-by-cut.tsv      With --models: MODELS_BY_CUT_COLUMNS, each cut's ranked changed skills
 *                          in rank order: cut, rank, skill, path, then the same model columns.
 */
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { z } from "zod";
import { schemaFaults } from "./common";
import { loadConfig, type SkillCutsConfig } from "./config";
import {
  INSTALLERS_COLUMNS,
  LOADERS_COLUMNS,
  MANIFESTS_COLUMNS,
  PREFIX_TABLE_COLUMNS,
  parseTsv,
  prefixRegExp,
  REPOSITORIES_COLUMNS,
  SKILL_COPIES_COLUMNS,
  tsv,
} from "./copies";
import { psqlRows } from "./psql-rows";
import { rankSkills } from "./score";

// ---------------------------------------------------------------------------------------------
// the count's output

const Hash = z.string().regex(/^[0-9a-f]{16}$/, { error: "a file hash of 16 hex characters" });
const Count = z.number().int().nonnegative();
/** A session as the count names it across stores: its store and its file hash. */
export const sessionKey = (store: string, hash: string) => `${store}\t${hash}`;

/** One count row of count.sql. */
export const CountRow = z.strictObject({
  row: z.literal("count"),
  store: z.string(),
  day: z.string().regex(/^\d{4}-\d{2}-\d{2}$/),
  skill: z.string().min(1),
  copy: z.string(),
  kind: z.enum(["read", "reference"]),
  files: Count,
  hashes: z.array(Hash),
  unpaired: Count,
  unpaired_hashes: z.array(Hash),
});
export type CountRow = z.infer<typeof CountRow>;

/** The summary row that ends a count.sql result. */
export const SummaryRow = z.strictObject({
  row: z.literal("summary"),
  store: z.string(),
  mtime_from: z.number().int(),
  mtime_to: z.number().int(),
  files: Count,
  parts: Count,
  bytes: Count,
  lines_parsed: Count,
  bad_lines: Count,
  shared_call_ids: Count,
});
export type SummaryRow = z.infer<typeof SummaryRow>;

const QueryRow = z.discriminatedUnion("row", [CountRow, SummaryRow]);

/** One query's output: its store, its name (`YYYY-MM-DD`, or `YYYY-MM-DDTHH` for an hour), its
 *  count rows and its summary. */
export interface QueryOutput {
  readonly store: string;
  readonly name: string;
  readonly rows: readonly CountRow[];
  readonly summary: SummaryRow;
}

const QUERY_FILE = /^\d{4}-\d{2}-\d{2}(T\d{2})?\.txt$/;

/** The rows and the summary of the psql output `text` of one query of the store `store`, each
 *  row parsed by `schema` (`what` names the query's SQL file) and the summary kept apart; `check`
 *  throws on a row the query's own rule refuses. Throws, naming the row, on a row of another shape
 *  or store, a row after the summary or one `check` refuses, and when no summary ends it (the
 *  query did not end). */
function queryRows<Row extends { readonly row: string; readonly store: string }>(
  store: string,
  text: string,
  schema: z.ZodType<Row>,
  what: string,
  check: (row: Exclude<Row, { row: "summary" }>, at: string) => void
): { rows: Exclude<Row, { row: "summary" }>[]; summary: Extract<Row, { row: "summary" }> } {
  const rows: Exclude<Row, { row: "summary" }>[] = [];
  let summary: Extract<Row, { row: "summary" }> | undefined;
  for (const [index, value] of psqlRows(text).entries()) {
    const at = `row ${index + 1}`;
    const parsed = schema.safeParse(value);
    if (!parsed.success) {
      throw new Error(`${at} is not a ${what} row: ${schemaFaults(parsed.error)}`);
    }
    const row = parsed.data;
    if (row.store !== store) throw new Error(`${at} is of the store ${JSON.stringify(row.store)}`);
    if (summary !== undefined) throw new Error(`${at} comes after the summary row`);
    if (row.row === "summary") {
      summary = row as Extract<Row, { row: "summary" }>;
      continue;
    }
    const kept = row as Exclude<Row, { row: "summary" }>;
    check(kept, at);
    rows.push(kept);
  }
  if (summary === undefined) throw new Error("no summary row: the query did not end");
  return { rows, summary };
}

/** The output of the query `name` of the store `store`; throws when the text is not one whole
 *  count.sql result of that store: no psql table, a row of another shape or store, a row after the
 *  summary, hashes that do not match their counts, or no summary (the query did not end). */
export function parseQueryOutput(store: string, name: string, text: string): QueryOutput {
  const { rows, summary } = queryRows(store, text, QueryRow, "count.sql", (row, at) => {
    if (row.hashes.length !== row.files || row.unpaired_hashes.length !== row.unpaired) {
      throw new Error(`${at}: its hashes do not match its files or unpaired counts`);
    }
  });
  return { store, name, rows, summary };
}

/** One query line of the run log. */
export interface LogEntry {
  readonly store: string;
  readonly name: string;
  readonly rc: number;
  /** The query's stderr, as the log keeps it (its first 200 characters, on one line). */
  readonly error: string;
}

/** What the run log says: each query line, each `FAILED` line, each line it could not read, and
 *  the time of its `END` line (undefined while the run goes on). */
export interface RunLog {
  readonly entries: readonly LogEntry[];
  readonly failed: readonly string[];
  readonly unread: readonly string[];
  readonly ended: string | undefined;
}

const LOG_ENTRY =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\t([^\t]+)\t(\d{4}-\d{2}-\d{2}(?:T\d{2})?)\trc=(\d+)\tseconds=[^\t]*(?:\t(.*))?$/;

export function parseRunLog(text: string): RunLog {
  const entries: LogEntry[] = [];
  const failed: string[] = [];
  const unread: string[] = [];
  let ended: string | undefined;
  for (const line of text.split("\n")) {
    const entry = LOG_ENTRY.exec(line);
    if (entry !== null) {
      entries.push({
        store: entry[1] ?? "",
        name: entry[2] ?? "",
        rc: Number(entry[3]),
        error: (entry[4] ?? "").trim(),
      });
    } else if (line.startsWith("FAILED ")) failed.push(line);
    else if (line.startsWith("END ")) ended = line.slice("END ".length).trim();
    else if (/^\d{4}-\d{2}-\d{2}T/.test(line)) unread.push(line);
  }
  return { entries, failed, unread, ended };
}

/** What a directory of query outputs (`--count`, `--models`) holds for the configured stores. */
export interface QueryDir<O> {
  readonly outputs: readonly O[];
  /** Each `.txt` that is not one whole result of its query, and why. */
  readonly unreadable: readonly { store: string; name: string; why: string }[];
  /** The run log, or undefined when the directory holds none. */
  readonly log: RunLog | undefined;
  /** Each directory that the configuration names no store for. */
  readonly otherStores: readonly string[];
}
export type CountRead = QueryDir<QueryOutput>;

/** Reads the query directory `dir` for the stores `stores`, each `.txt` parsed by `parse`. */
export function readQueryDir<O>(
  dir: string,
  stores: readonly string[],
  parse: (store: string, name: string, text: string) => O
): QueryDir<O> {
  if (!existsSync(dir)) throw new Error(`the query directory ${dir} does not exist`);
  const outputs: O[] = [];
  const unreadable: { store: string; name: string; why: string }[] = [];
  for (const store of stores) {
    const storeDir = path.join(dir, store);
    if (!existsSync(storeDir)) continue;
    for (const file of readdirSync(storeDir).sort()) {
      if (!QUERY_FILE.test(file)) continue;
      const name = file.slice(0, -".txt".length);
      try {
        outputs.push(parse(store, name, readFileSync(path.join(storeDir, file), "utf8")));
      } catch (error) {
        unreadable.push({ store, name, why: (error as Error).message });
      }
    }
  }
  const logFile = path.join(dir, "run.log");
  const log = existsSync(logFile) ? parseRunLog(readFileSync(logFile, "utf8")) : undefined;
  const otherStores = readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && !stores.includes(entry.name))
    .map((entry) => entry.name)
    .sort();
  return { outputs, unreadable, log, otherStores };
}

/** Reads the count directory `dir` for the stores `stores`. */
export function readCount(dir: string, stores: readonly string[]): CountRead {
  return readQueryDir(dir, stores, parseQueryOutput);
}

/** A query of the count that has no whole output, or that was split, and what happened to it. */
export interface QueryIssue {
  readonly store: string;
  /** The query's day or hour, or `""` for an issue of the run log itself. */
  readonly name: string;
  readonly what: "failed" | "split" | "no output" | "unreadable" | "run log";
  readonly detail: string;
}

const DAY_MS = 86_400_000;
const STATEMENT_TIMEOUT = /statement timeout/i;
const HOURS = Array.from({ length: 24 }, (_, hour) => hour);
const twoDigits = (value: number) => String(value).padStart(2, "0");

/** The UTC days (YYYY-MM-DD) from the day of `first` to the day of `last`, both included. */
function daysFrom(first: number, last: number): string[] {
  const days: string[] = [];
  for (let at = Math.floor(first / DAY_MS) * DAY_MS; at <= last; at += DAY_MS) {
    days.push(new Date(at).toISOString().slice(0, 10));
  }
  return days;
}

/**
 * Each query that failed, was split into hours, or has no output, from the run log and the
 * directory: a query whose last log line has a non-zero status (a day that timed out is split, and
 * its hours that have no output are named); a `FAILED` line the log lines do not already show; a
 * `.txt` that is not a whole result; and each day of the window (and, once the log has its `END`
 * line, each day from the window's end to the run's end) with no output of the day or of an hour.
 */
export function queryIssues(
  config: SkillCutsConfig,
  count: QueryDir<{ readonly store: string; readonly name: string }>
): QueryIssue[] {
  const issues: QueryIssue[] = [];
  const log = count.log;
  if (log === undefined) {
    issues.push({
      store: "",
      name: "",
      what: "run log",
      detail: "the directory holds no run.log, so no query's status is known",
    });
  }
  for (const line of log?.unread ?? []) {
    issues.push({ store: "", name: "", what: "run log", detail: `a line not read: ${line}` });
  }
  const last = new Map<string, LogEntry>();
  for (const entry of log?.entries ?? []) last.set(`${entry.store}\t${entry.name}`, entry);
  const windowEnd = Date.parse(config.windowEnd);
  const expected = daysFrom(Date.parse(config.windowStart), windowEnd - 1);
  if (log?.ended !== undefined && !Number.isNaN(Date.parse(log.ended))) {
    for (const day of daysFrom(windowEnd, Date.parse(log.ended)))
      if (!expected.includes(day)) expected.push(day);
  }
  for (const { name: store } of config.stores) {
    const names = new Set(
      count.outputs.filter((output) => output.store === store).map((output) => output.name)
    );
    const hoursOf = (day: string) => HOURS.filter((hour) => names.has(`${day}T${twoDigits(hour)}`));
    const listed = new Set<string>();
    const add = (name: string, what: QueryIssue["what"], detail: string) => {
      listed.add(name);
      issues.push({ store, name, what, detail });
    };
    for (const entry of last.values()) {
      if (entry.store !== store || entry.rc === 0) continue;
      if (entry.name.length === 10 && STATEMENT_TIMEOUT.test(entry.error)) {
        const read = hoursOf(entry.name);
        const missing = HOURS.filter((hour) => !read.includes(hour));
        add(
          entry.name,
          "split",
          `timed out, so it was split into hours: ${read.length} of 24 hour queries have output` +
            (missing.length > 0 ? `; hours ${missing.map(twoDigits).join(", ")} have none` : "")
        );
      } else if (!names.has(entry.name)) {
        add(entry.name, "failed", `rc=${entry.rc}${entry.error === "" ? "" : `: ${entry.error}`}`);
      }
    }
    for (const line of log?.failed ?? []) {
      const failed = /^FAILED (\S+) (\d{4}-\d{2}-\d{2})(?: hour (\d+))?/.exec(line);
      if (failed === null || failed[1] !== store) continue;
      const day = failed[2] ?? "";
      const name = failed[3] === undefined ? day : `${day}T${twoDigits(Number(failed[3]))}`;
      if (!listed.has(name)) add(name, "failed", `the run log says: ${line}`);
    }
    for (const unreadable of count.unreadable.filter((item) => item.store === store)) {
      add(unreadable.name, "unreadable", `its output is not a whole result: ${unreadable.why}`);
    }
    for (const day of expected) {
      if (names.has(day) || hoursOf(day).length > 0 || listed.has(day)) continue;
      const entry = last.get(`${store}\t${day}`);
      add(
        day,
        "no output",
        entry === undefined
          ? "the run log has no line for it: not run yet"
          : `the run log gives rc=${entry.rc}, and no output of it is here`
      );
    }
  }
  return issues;
}

// ---------------------------------------------------------------------------------------------
// the models query's output

/** Where models.sql found a file's model. */
export const MODEL_SOURCES = [
  "model_change",
  "session_init",
  "assistant",
  "model attachment",
] as const;

/** One file's row of models.sql. */
export const ModelRow = z.strictObject({
  row: z.literal("model"),
  store: z.string(),
  hash: Hash,
  model: z.string().min(1).nullable(),
  source: z.enum(MODEL_SOURCES).nullable(),
});
export type ModelRow = z.infer<typeof ModelRow>;

/** The summary row that ends a models.sql result. */
export const ModelSummaryRow = z.strictObject({
  row: z.literal("summary"),
  store: z.string(),
  mtime_from: z.number().int(),
  mtime_to: z.number().int(),
  files: Count,
  with_model: Count,
  bytes: Count,
  lines_parsed: Count,
  bad_lines: Count,
});
export type ModelSummaryRow = z.infer<typeof ModelSummaryRow>;

const ModelQueryRow = z.discriminatedUnion("row", [ModelRow, ModelSummaryRow]);

/** One models.sql query's output. */
export interface ModelOutput {
  readonly store: string;
  readonly name: string;
  readonly rows: readonly ModelRow[];
  readonly summary: ModelSummaryRow;
}

/** The output of the models query `name` of the store `store`; throws when the text is not one
 *  whole models.sql result of that store: no psql table, a row of another shape or store, a row
 *  after the summary, a model with no source or a source with no model, a summary whose counts
 *  are not its rows', or no summary. */
export function parseModelOutput(store: string, name: string, text: string): ModelOutput {
  const { rows, summary } = queryRows(store, text, ModelQueryRow, "models.sql", (row, at) => {
    if ((row.model === null) !== (row.source === null)) {
      throw new Error(`${at}: a model needs its source, and a source its model`);
    }
  });
  const withModel = rows.filter((row) => row.model !== null).length;
  if (summary.files !== rows.length || summary.with_model !== withModel) {
    throw new Error(
      `the summary counts ${summary.files} files, ${summary.with_model} with a model; the rows are ${rows.length}, ${withModel} with a model`
    );
  }
  return { store, name, rows, summary };
}

/** The thinking selectors Oh My Pi may write after a model (`anthropic/claude-x:low`). */
const THINKING_SUFFIX = /:(inherit|off|minimal|low|medium|high|xhigh|max|auto)$/;

/** The model id of a `provider/model` (or a bare id): its text after the last `/`. */
export const modelId = (model: string) => model.slice(model.lastIndexOf("/") + 1);

/** A session's model as the count groups it: `provider/model` without a thinking suffix (Claude
 *  Code's model id names no provider, and Claude Code calls Anthropic's Messages API, so a Claude
 *  Code model is written `anthropic/<id>`); whether it is an Anthropic model, by its family (its id
 *  starts `claude-`, whatever gateway or provider names it); and whether the gateway serves it
 *  (its id is the id of one of the configuration's `servedModels`). */
export interface SessionModel {
  readonly model: string;
  readonly anthropic: boolean;
  readonly served: boolean;
}

/** The model of a models.sql row as the count groups it, or null for a row with none; `served`
 *  holds the ids (`modelId`) of the models the gateway serves. */
export function sessionModel(
  row: Pick<ModelRow, "model" | "source">,
  served: ReadonlySet<string>
): SessionModel | null {
  if (row.model === null) return null;
  const stripped = row.model.replace(THINKING_SUFFIX, "");
  const claudeCode = row.source === "assistant" || row.source === "model attachment";
  const model = claudeCode && !stripped.includes("/") ? `anthropic/${stripped}` : stripped;
  const id = modelId(model);
  return { model, anthropic: id.startsWith("claude-"), served: served.has(id) };
}

/** Each session's model across the models queries: `sessionKey` → its model, or null when the
 *  head of its file names none. A file that two queries hold (a resumed session whose mtime day
 *  moved) takes the model one of them names; when they name two models, the first query's (in
 *  day order) is kept and the session is counted in `conflicts`. */
export interface SessionModels {
  readonly models: ReadonlyMap<string, SessionModel | null>;
  readonly conflicts: number;
}

/** The sessions' models of `outputs`, each marked served when the gateway serves it
 *  (`servedModels`, the configuration's). */
export function sessionModels(
  outputs: readonly ModelOutput[],
  servedModels: readonly string[]
): SessionModels {
  const served = new Set(servedModels.map(modelId));
  const models = new Map<string, SessionModel | null>();
  const conflicting = new Set<string>();
  const ordered = outputs.toSorted(
    (a, b) => a.store.localeCompare(b.store) || a.name.localeCompare(b.name)
  );
  for (const output of ordered) {
    for (const row of output.rows) {
      const key = sessionKey(row.store, row.hash);
      const model = sessionModel(row, served);
      const known = models.get(key);
      if (known === undefined || known === null) models.set(key, model);
      else if (model !== null && model.model !== known.model) conflicting.add(key);
    }
  }
  return { models, conflicts: conflicting.size };
}

// ---------------------------------------------------------------------------------------------
// the spans file and the copies output's tables

/** One line of the spans file. */
export const Span = z.looseObject({
  store: z.string().min(1),
  earliest_mtime_ms: z.number().int().nullable(),
  files: Count,
});
export type Span = z.infer<typeof Span>;

/** The spans in `text`, one JSON line each; throws naming the line of a fault. */
export function parseSpans(text: string, source = "the spans file"): Span[] {
  const spans: Span[] = [];
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim() === "") continue;
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch (error) {
      throw new Error(`${source} line ${index + 1} is not JSON: ${(error as Error).message}`);
    }
    const parsed = Span.safeParse(value);
    if (!parsed.success) {
      throw new Error(`${source} line ${index + 1}: ${schemaFaults(parsed.error)}`);
    }
    spans.push(parsed.data);
  }
  return spans;
}

type Columns<C extends readonly string[]> = { readonly [K in C[number]]: string };
export type SkillCopiesRow = Columns<typeof SKILL_COPIES_COLUMNS>;
export type PrefixTableRow = Columns<typeof PREFIX_TABLE_COLUMNS>;
export type InstallersRow = Columns<typeof INSTALLERS_COLUMNS>;
export type LoadersRow = Columns<typeof LOADERS_COLUMNS>;
export type RepositoriesRow = Columns<typeof REPOSITORIES_COLUMNS>;
export type ManifestsRow = Columns<typeof MANIFESTS_COLUMNS>;

/** The tables of the copies output (copies.ts) this file reads. */
export interface R4Tables {
  readonly copies: readonly SkillCopiesRow[];
  readonly prefixes: readonly PrefixTableRow[];
  readonly installers: readonly InstallersRow[];
  readonly loaders: readonly LoadersRow[];
  readonly repositories: readonly RepositoriesRow[];
}

/** The copies output as `readR4` reads it: the tables the attribution reads, and manifests.tsv,
 *  which build.ts's profiles read. */
export interface R4Output extends R4Tables {
  readonly manifests: readonly ManifestsRow[];
}

/** The rows of the TSV `file` of `dir`; throws when its header row is not `columns`, in order. */
function readTable<C extends readonly string[]>(
  dir: string,
  file: string,
  columns: C
): Columns<C>[] {
  const where = path.join(dir, file);
  let text: string;
  try {
    text = readFileSync(where, "utf8");
  } catch (error) {
    throw new Error(`cannot read ${where}: ${(error as Error).message}`);
  }
  const header = text.split("\n")[0] ?? "";
  if (header !== columns.join("\t")) {
    throw new Error(
      `${where}: its columns are ${header.split("\t").join(", ")}, not copies.ts's ${columns.join(", ")}`
    );
  }
  return parseTsv(text) as Columns<C>[];
}

/** The copies output in its directory `dir`. */
export function readR4(dir: string): R4Output {
  return {
    copies: readTable(dir, "skill-copies.tsv", SKILL_COPIES_COLUMNS),
    prefixes: readTable(dir, "prefix-table.tsv", PREFIX_TABLE_COLUMNS),
    installers: readTable(dir, "installers.tsv", INSTALLERS_COLUMNS),
    loaders: readTable(dir, "loaders.tsv", LOADERS_COLUMNS),
    manifests: readTable(dir, "manifests.tsv", MANIFESTS_COLUMNS),
    repositories: readTable(dir, "repositories.tsv", REPOSITORIES_COLUMNS),
  };
}

// ---------------------------------------------------------------------------------------------
// the attribution

/** The installers.tsv actions that put a skill into a host's sessions. */
export const INSTALL_ACTIONS: Readonly<Record<string, true>> = {
  ships: true,
  stages: true,
  installs: true,
  links: true,
  enables: true,
};

/** Skills that code puts into every session, by name, with what does it. */
export const INJECTED_SKILLS: Readonly<Record<string, string>> = {
  "dispatch-first":
    "pi-envoy injects it into each session that has Dispatch (packages/envoy-client/src/dispatch-first.ts)",
};

export const STATUSES = [
  "read",
  "no read",
  "not countable yet",
  "no Oh My Pi read; Claude Code not counted yet",
] as const;
export type Status = (typeof STATUSES)[number];

export const READ_COUNT_COLUMNS = [
  "repository",
  "path",
  "name",
  "sessions",
  "reference_sessions",
  "unattributed_sessions",
  "tie",
  "loaded",
  "status",
] as const;

/**
 * The copy-path prefix of a count row's copy: the path up to and with the skill's directory.
 * `skill://<name>/` for a `skill://` path. Otherwise the last directory named as the skill that a
 * `SKILL.md` (with any read selector after it) or a `references` directory follows; else the
 * directory of a `SKILL.md`; else the directory before the first `references`; else the path's
 * directory.
 */
export function copyPrefix(copyPath: string, skill: string): string {
  if (copyPath.startsWith("skill://")) return `skill://${skill}/`;
  const parts = copyPath.split("/");
  const upTo = (end: number) => `${parts.slice(0, end).join("/")}/`;
  const isSkillFile = (part: string | undefined) => part?.startsWith("SKILL.md") === true;
  for (let index = parts.length - 2; index >= 0; index--) {
    const next = parts[index + 1];
    if (parts[index] === skill && (isSkillFile(next) || next === "references"))
      return upTo(index + 1);
  }
  if (isSkillFile(parts[parts.length - 1]) && parts.length > 1) return upTo(parts.length - 1);
  const references = parts.indexOf("references");
  if (references > 0) return upTo(references);
  return parts.length > 1 ? upTo(parts.length - 1) : copyPath;
}

/** A unit (`<unit> (<repository>:<manifest path>)` in prefix-table.tsv's `via`) as a key. */
const unitKey = (unit: string, repository: string, manifest: string) =>
  `${unit}\t${repository}\t${manifest}`;
/** The unit of a key, as prefix-table.tsv's `via` writes it. */
function unitLabel(key: string): string {
  const [unit = "", repository = "", manifest = ""] = key.split("\t");
  return `${unit} (${repository}:${manifest})`;
}

/** A unit of a prefix row's `via`: `<unit> (<manifest repository>:<manifest path>)`. */
export interface Via {
  readonly unit: string;
  readonly repository: string;
  readonly manifest: string;
}

/** The units a prefix row's `via` names, in order; throws on an item of another shape. */
export function vias(via: string): Via[] {
  return via
    .split("; ")
    .filter((item) => item !== "")
    .map((item) => {
      const match = /^(.+) \(([^\s:()]+\/[^\s:()]+):(.+)\)$/.exec(item);
      if (match === null) {
        throw new Error(
          `a via item that is not <unit> (<repository>:<manifest path>): ${JSON.stringify(item)}`
        );
      }
      return { unit: match[1] ?? "", repository: match[2] ?? "", manifest: match[3] ?? "" };
    });
}

/** The unit keys a prefix row's `via` names (`vias`); throws on an item of another shape. */
export function viaUnits(via: string): string[] {
  return vias(via).map((each) => unitKey(each.unit, each.repository, each.manifest));
}

const copyKey = (repository: string, dir: string) => `${repository}\t${dir}`;
/** A copy as the report names it: `<repository> <path>`. */
const shown = (key: string) => key.replaceAll("\t", " ");

function entry<K, V>(map: Map<K, V>, key: K, make: () => V): V {
  let value = map.get(key);
  if (value === undefined) {
    value = make();
    map.set(key, value);
  }
  return value;
}

/** One copy's counts. */
export interface CopyCount {
  readonly repository: string;
  readonly path: string;
  readonly name: string;
  readonly sessions: number;
  /** The read sessions (`sessionKey`) that `sessions` counts. */
  readonly readSessions: ReadonlySet<string>;
  readonly referenceSessions: number;
  /** The read sessions of the copy's name that no copy got. */
  readonly unattributed: number;
  /** Each copy of the name it ties with, `<repository> <path>`. */
  readonly ties: readonly string[];
  /** What loads it into a session; empty when nothing does. */
  readonly loadedBy: readonly string[];
  readonly ompInstalls: boolean;
  readonly claudeInstalls: boolean;
  readonly status: Status;
}

/** A copy-path prefix the prefix table does not map, and where its sessions went. */
export interface UnmappedPrefix {
  readonly prefix: string;
  readonly skills: readonly string[];
  readonly sessions: number;
  readonly referenceSessions: number;
  /** Each outcome (`<store>: to <copy> ...` or `<store>: unattributed: <why>`) with its sessions,
   *  read and reference together. */
  readonly outcomes: readonly { readonly outcome: string; readonly sessions: number }[];
}

/** A unit a store's context installs, with the sessions of the store that read from it. */
export interface StoreUnit {
  readonly unit: string;
  readonly sessions: number;
  readonly copies: number;
}

/** What the count gives for one store. */
export interface StoreCount {
  readonly store: string;
  readonly context: string;
  readonly outputs: number;
  readonly filesRead: number;
  readonly bytesRead: number;
  readonly badLines: number;
  readonly sharedCallIds: number;
  readonly readSessions: number;
  readonly unpairedSessions: number;
  /** The first-read day of its earliest read in the window, or undefined with none. */
  readonly earliestRead: string | undefined;
  readonly units: readonly StoreUnit[];
}

/** A skill name the count holds reads of that no copy of the copies output has. */
export interface NameWithoutCopy {
  readonly name: string;
  readonly sessions: number;
  readonly referenceSessions: number;
}

export interface Attribution {
  readonly copies: readonly CopyCount[];
  readonly unmapped: readonly UnmappedPrefix[];
  /** Each prefix the table maps, with the copies (`<repository> <path>`) it maps to. */
  readonly mappedPrefixes: ReadonlyMap<string, ReadonlySet<string>>;
  readonly stores: readonly StoreCount[];
  readonly namesWithoutCopy: readonly NameWithoutCopy[];
  /** The distinct (store, file) sessions with a read. */
  readonly readSessions: number;
  /** The distinct read sessions some row of which went to no copy. */
  readonly unattributedSessions: number;
}

type TableDecision =
  | { readonly copy: string; readonly units: readonly string[] }
  | { readonly copy: undefined; readonly copies: readonly string[] };

/** A prefix-table.tsv row as the attribution reads it: its pattern, the last directory name of its
 *  prefix, its copy and the units of its `via`. */
interface PrefixMatcher {
  readonly pattern: RegExp;
  readonly last: string;
  readonly copy: string;
  readonly units: readonly string[];
}

/** The copy prefix-table.tsv's rows map a copy path to. Only the rows whose prefix's last
 *  directory name is a directory of the path (or holds a `*`) are tried. */
function prefixTable(rows: readonly PrefixMatcher[]) {
  const byLast = new Map<string, PrefixMatcher[]>();
  const wild: PrefixMatcher[] = [];
  for (const row of rows) {
    if (row.last.includes("*") || row.last === "") wild.push(row);
    else entry(byLast, row.last, (): PrefixMatcher[] => []).push(row);
  }
  const decisions = new Map<string, TableDecision>();
  return (copyPath: string): TableDecision => {
    const known = decisions.get(copyPath);
    if (known !== undefined) return known;
    const candidates = new Set(wild);
    for (const part of copyPath.split("/"))
      for (const row of byLast.get(part) ?? []) candidates.add(row);
    const matched = [...candidates].filter((row) => row.pattern.test(copyPath));
    const found = [...new Set(matched.map((row) => row.copy))];
    const [only] = found;
    const decision: TableDecision =
      found.length === 1 && only !== undefined
        ? { copy: only, units: [...new Set(matched.flatMap((row) => row.units))] }
        : { copy: undefined, copies: found };
    decisions.set(copyPath, decision);
    return decision;
  };
}

/** What the checkout rule says of a copy-path prefix: the copy it places the read at, or
 *  undefined with the copies it cannot tell apart, and why, in the report's words. */
export interface CheckoutDecision {
  readonly copy: string | undefined;
  readonly why: string;
}

/**
 * The checkout rule, for the copies `copies`: a read whose copy-path prefix (`copyPrefix`) ends
 * with a copy's repository-relative skill directory, at a path boundary, is a read of that copy in
 * a checkout of its repository (`/home/x/.dotfiles/plugins/p/skills/<name>/`, a workspace's
 * `.../<issue>/.claude/skills/<name>/`, a relative `skills/<name>/`). The longest such directory
 * wins. The path before that directory names a repository when two of its segments in a row are
 * the repository's owner and a segment that holds its name, or, less, when a segment holds its
 * name (`.dotfiles` holds `dotfiles`). Copies of several repositories at that directory are told
 * apart by it: the one the path names most; any other tie is unattributed. A skill directory of
 * one segment (a skill at its repository's root) ends every path of a skill of that name, so it
 * matches only a path that names its repository. Undefined when no copy's directory ends the
 * prefix, and for a `skill://` prefix, which names no directory.
 */
export function checkoutRule(
  copies: readonly Pick<SkillCopiesRow, "repository" | "path">[]
): (prefix: string) => CheckoutDecision | undefined {
  const byDir = new Map<string, string[]>();
  for (const row of copies) {
    if (row.path !== "." && row.path !== "")
      entry(byDir, row.path, (): string[] => []).push(row.repository);
  }
  const decisions = new Map<string, CheckoutDecision | undefined>();
  return (prefix) => {
    if (decisions.has(prefix)) return decisions.get(prefix);
    let decision: CheckoutDecision | undefined;
    const segments = prefix.replace(/\/+$/, "").split("/");
    for (let start = 0; start < segments.length && !prefix.startsWith("skill://"); start++) {
      const dir = segments.slice(start).join("/");
      const before = segments.slice(0, start);
      /** 2 when the path names the repository's owner and name, 1 its name, else 0. */
      const named = (repository: string) => {
        const [owner = "", name = ""] = repository.split("/");
        if (before.some((segment, at) => segment === owner && before[at + 1]?.includes(name)))
          return 2;
        return before.some((segment) => segment.includes(name)) ? 1 : 0;
      };
      const oneSegment = start === segments.length - 1;
      const repositories = (byDir.get(dir) ?? []).filter(
        (repository) => !oneSegment || named(repository) > 0
      );
      if (repositories.length === 0) continue;
      const best = Math.max(...repositories.map(named));
      const chosen = repositories.filter((repository) => named(repository) === best);
      const [only] = chosen;
      const ends = `the path ends with ${dir}/`;
      const naming = (repository: string) =>
        `the path names ${best === 2 ? repository : `${repository.split("/")[1]} in a directory`}`;
      if (repositories.length === 1 && only !== undefined) {
        decision = {
          copy: copyKey(only, dir),
          why: `to ${only} ${dir}: ${ends}, its skill directory${oneSegment ? `, and ${naming(only)}` : ""}`,
        };
      } else if (chosen.length === 1 && only !== undefined && best > 0) {
        decision = {
          copy: copyKey(only, dir),
          why: `to ${only} ${dir}: ${ends}, the skill directory of ${repositories.length} repositories, and ${naming(only)}`,
        };
      } else {
        decision = {
          copy: undefined,
          why: `${ends}, the skill directory of ${repositories.length} repositories (${repositories.join(", ")}), and no directory of the path names just one of them`,
        };
      }
      break;
    }
    decisions.set(prefix, decision);
    return decision;
  };
}

/** The rule that placed a read at a copy (the header's steps 1 to 3). */
export type PlacementRule = "prefix table" | "checkout" | "context";

/** Where one read goes: `copy` its copy (`<repository>\t<skill directory>`, as skill-copies.tsv
 *  writes the two) and `rule` the rule that placed it, or neither when no rule places it.
 *  `prefix` is the copy-path prefix of its path (`copyPrefix`).
 *  `outcome`, for a read the prefix table does not place, says where it went and why, as the
 *  report writes it after the store's name (`checkout: to …`, `context: to …`,
 *  `unattributed: …`). */
export interface Placement {
  readonly prefix: string;
  readonly copy: string | undefined;
  readonly rule: PlacementRule | undefined;
  readonly outcome: string | undefined;
}

/** The placing of reads by the rules of this file's header, for one count and one copies output. */
export interface Placer {
  /** Where a read of the copy path `copyPath` of the skill `skill` in the store `store` goes. */
  place(store: string, copyPath: string, skill: string): Placement;
  /** Step 3's units: for each store, each unit its prefix-table reads come from, with their
   *  sessions (file hashes). */
  readonly storeUnits: ReadonlyMap<string, ReadonlyMap<string, ReadonlySet<string>>>;
  /** Each unit's copies: its installer rows, and the copies its prefix rows map to. */
  readonly members: ReadonlyMap<string, ReadonlySet<string>>;
  /** The copies some store's context installs. */
  readonly installedInContext: ReadonlySet<string>;
}

/** The one place a read's copy is decided (the header's steps 1 to 4): `attribute` counts each
 *  row where this puts it, and build.ts keeps a session only when this puts its read at the cut's
 *  copy. Throws when a prefix-table.tsv row, or an installers.tsv row that installs a copy, names
 *  a copy that skill-copies.tsv does not hold. */
export function placer(outputs: readonly QueryOutput[], r4: R4Tables): Placer {
  const copies = new Map<string, SkillCopiesRow>();
  for (const row of r4.copies) copies.set(copyKey(row.repository, row.path), row);
  const known = (key: string, where: string) => {
    if (!copies.has(key))
      throw new Error(`${where} names ${shown(key)}, which skill-copies.tsv does not hold`);
    return key;
  };

  // The copies of each unit: its installer rows, and the copies its prefix rows map to.
  const members = new Map<string, Set<string>>();
  for (const [index, row] of r4.installers.entries()) {
    if (!Object.hasOwn(INSTALL_ACTIONS, row.action) || row.copy_path === "") continue;
    const key = known(
      copyKey(row.copy_repository, row.copy_path),
      `installers.tsv row ${index + 2}`
    );
    entry(
      members,
      unitKey(row.unit, row.manifest_repository, row.manifest_path),
      () => new Set()
    ).add(key);
  }
  const decide = prefixTable(
    r4.prefixes.map((row, index) => {
      const key = known(copyKey(row.repository, row.path), `prefix-table.tsv row ${index + 2}`);
      const units = viaUnits(row.via);
      for (const unit of units) entry(members, unit, () => new Set<string>()).add(key);
      const parts = row.prefix.split("/").filter((part) => part !== "");
      return {
        pattern: prefixRegExp(row.prefix),
        last: parts[parts.length - 1] ?? "",
        copy: key,
        units,
      };
    })
  );
  const checkout = checkoutRule(r4.copies);

  // Step 3's units: those each store's own mapped reads come from, with their sessions.
  const storeUnits = new Map<string, Map<string, Set<string>>>();
  for (const output of outputs) {
    for (const row of output.rows) {
      if (row.hashes.length === 0) continue;
      const decision = decide(row.copy);
      if (decision.copy === undefined) continue;
      const units = entry(storeUnits, row.store, () => new Map<string, Set<string>>());
      for (const unit of decision.units) {
        const sessions = entry(units, unit, () => new Set<string>());
        for (const hash of row.hashes) sessions.add(hash);
      }
    }
  }
  const contextCopies = new Map<string, Map<string, Set<string>>>();
  const installedInContext = new Set<string>();
  for (const [store, units] of storeUnits) {
    const byName = new Map<string, Set<string>>();
    for (const unit of units.keys()) {
      for (const key of members.get(unit) ?? []) {
        entry(byName, copies.get(key)?.name ?? "", () => new Set()).add(key);
        installedInContext.add(key);
      }
    }
    contextCopies.set(store, byName);
  }

  const place = (store: string, copyPath: string, skill: string): Placement => {
    const prefix = copyPrefix(copyPath, skill);
    const decision = decide(copyPath);
    if (decision.copy !== undefined) {
      return { prefix, copy: decision.copy, rule: "prefix table", outcome: undefined };
    }
    const byCheckout = checkout(prefix);
    const context = [...(contextCopies.get(store)?.get(skill) ?? [])];
    const table =
      decision.copies.length > 1
        ? `prefix rows match ${decision.copies.length} copies (${decision.copies.map(shown).join(", ")}), and `
        : "";
    if (byCheckout?.copy !== undefined) {
      return {
        prefix,
        copy: byCheckout.copy,
        rule: "checkout",
        outcome: `checkout: ${byCheckout.why}`,
      };
    }
    if (byCheckout !== undefined) {
      return {
        prefix,
        copy: undefined,
        rule: undefined,
        outcome: `unattributed: ${table}${byCheckout.why}`,
      };
    }
    const [only] = context;
    if (context.length === 1 && only !== undefined) {
      return {
        prefix,
        copy: only,
        rule: "context",
        outcome: `context: to ${shown(only)}, the one copy of ${skill} its context installs`,
      };
    }
    return {
      prefix,
      copy: undefined,
      rule: undefined,
      outcome:
        `unattributed: ${table}no copy's skill directory ends the path, and its context installs ` +
        (context.length === 0
          ? `no copy of ${skill}`
          : `${context.length} copies of ${skill} (${context.map(shown).join(", ")})`),
    };
  };
  return { place, storeUnits, members, installedInContext };
}

/** The attribution of the count's rows `outputs` to the copies output's copies, by the rules of
 *  this file's header. Throws as `placer` does, when a prefix-table.tsv row, or an installers.tsv
 *  row that installs a copy, names a copy that skill-copies.tsv does not hold. */
export function attribute(
  config: SkillCutsConfig,
  outputs: readonly QueryOutput[],
  r4: R4Tables
): Attribution {
  const { place, storeUnits, members, installedInContext } = placer(outputs, r4);

  // Each copy's sessions, and each skill name's sessions no copy got, by the row's kind.
  const copySessions = {
    read: new Map<string, Set<string>>(),
    reference: new Map<string, Set<string>>(),
  };
  const unplaced = {
    read: new Map<string, Set<string>>(),
    reference: new Map<string, Set<string>>(),
  };
  const unmapped = new Map<
    string,
    {
      skills: Set<string>;
      sessions: Record<CountRow["kind"], Set<string>>;
      outcomes: Map<string, Set<string>>;
    }
  >();
  const mappedPrefixes = new Map<string, Set<string>>();
  const allReads = new Set<string>();
  const stats = new Map<
    string,
    {
      outputs: number;
      files: number;
      bytes: number;
      bad: number;
      shared: number;
      read: Set<string>;
      unpaired: Set<string>;
      earliest: string | undefined;
    }
  >();
  for (const output of outputs) {
    const store = entry(stats, output.store, () => ({
      outputs: 0,
      files: 0,
      bytes: 0,
      bad: 0,
      shared: 0,
      read: new Set<string>(),
      unpaired: new Set<string>(),
      earliest: undefined as string | undefined,
    }));
    store.outputs++;
    store.files += output.summary.files;
    store.bytes += output.summary.bytes;
    store.bad += output.summary.bad_lines;
    store.shared += output.summary.shared_call_ids;
    for (const row of output.rows) {
      for (const hash of row.unpaired_hashes) store.unpaired.add(hash);
      if (row.hashes.length === 0) continue;
      const sessions = row.hashes.map((hash) => sessionKey(row.store, hash));
      if (row.kind === "read") {
        for (const hash of row.hashes) store.read.add(hash);
        for (const session of sessions) allReads.add(session);
        if (store.earliest === undefined || row.day < store.earliest) store.earliest = row.day;
      }
      const placed = place(row.store, row.copy, row.skill);
      const copy = placed.copy;
      if (placed.outcome === undefined) {
        if (copy !== undefined)
          entry(mappedPrefixes, placed.prefix, () => new Set()).add(shown(copy));
      } else {
        const item = entry(unmapped, placed.prefix, () => ({
          skills: new Set<string>(),
          sessions: { read: new Set<string>(), reference: new Set<string>() },
          outcomes: new Map<string, Set<string>>(),
        }));
        item.skills.add(row.skill);
        const went = entry(
          item.outcomes,
          `${row.store}: ${placed.outcome}`,
          () => new Set<string>()
        );
        for (const session of sessions) {
          item.sessions[row.kind].add(session);
          went.add(session);
        }
      }
      const counted =
        copy === undefined
          ? entry(unplaced[row.kind], row.skill, () => new Set<string>())
          : entry(copySessions[row.kind], copy, () => new Set<string>());
      for (const session of sessions) counted.add(session);
    }
  }

  // Each copy's loaders, installs, ties and status.
  const loadedBy = new Map<string, Set<string>>();
  const loadedByName = new Map<string, Set<string>>();
  const ompInstalled = new Set<string>();
  const claudeInstalled = new Set<string>();
  for (const row of r4.installers) {
    const key = row.copy_path === "" ? undefined : copyKey(row.copy_repository, row.copy_path);
    if (row.action === "loads") {
      const how = `${row.manifest_repository}:${row.manifest_path} (${row.kind} ${row.unit})`;
      if (key === undefined) entry(loadedByName, row.skill, () => new Set()).add(how);
      else entry(loadedBy, key, () => new Set()).add(how);
    }
    if (key === undefined || !Object.hasOwn(INSTALL_ACTIONS, row.action)) continue;
    if (row.host === "omp") ompInstalled.add(key);
    if (row.host === "claude-code") claudeInstalled.add(key);
  }
  for (const loader of r4.loaders) {
    if (!loader.match.split(",").includes("code-load")) continue;
    entry(loadedByName, loader.skill, () => new Set()).add(
      `${loader.repository}:${loader.path} (code-load, ${loader.source})`
    );
  }
  const byName = new Map<string, SkillCopiesRow[]>();
  for (const row of r4.copies) entry(byName, row.name, () => []).push(row);
  const counts = r4.copies.map((row): CopyCount => {
    const key = copyKey(row.repository, row.path);
    const readSessions = copySessions.read.get(key) ?? new Set<string>();
    const sessions = readSessions.size;
    const referenceSessions = copySessions.reference.get(key)?.size ?? 0;
    const free = unplaced.read.get(row.name)?.size ?? 0;
    const ties = (byName.get(row.name) ?? [])
      .filter((other) => other !== row)
      .map((other) => copyKey(other.repository, other.path))
      .filter((other) => Math.abs((copySessions.read.get(other)?.size ?? 0) - sessions) <= free)
      .map(shown);
    const loaders = new Set([
      ...row.loaded_by.split("; ").filter((item) => item !== ""),
      ...(loadedBy.get(key) ?? []),
      ...(loadedByName.get(row.name) ?? []),
    ]);
    const injected = INJECTED_SKILLS[row.name];
    if (injected !== undefined) loaders.add(injected);
    const ompInstalls =
      sessions + referenceSessions > 0 || installedInContext.has(key) || ompInstalled.has(key);
    const claudeInstalls = claudeInstalled.has(key);
    const status = copyStatus(sessions, ompInstalls, claudeInstalls);
    return {
      repository: row.repository,
      path: row.path,
      name: row.name,
      sessions,
      readSessions,
      referenceSessions,
      unattributed: free,
      ties,
      loadedBy: [...loaders].sort(),
      ompInstalls,
      claudeInstalls,
      status,
    };
  });

  const namesWithoutCopy = [...new Set([...unplaced.read.keys(), ...unplaced.reference.keys()])]
    .filter((name) => !byName.has(name))
    .sort()
    .map((name) => ({
      name,
      sessions: unplaced.read.get(name)?.size ?? 0,
      referenceSessions: unplaced.reference.get(name)?.size ?? 0,
    }));
  const unattributedSessions = new Set([...unplaced.read.values()].flatMap((set) => [...set])).size;
  return {
    copies: counts,
    unmapped: [...unmapped]
      .map(([prefix, item]) => ({
        prefix,
        skills: [...item.skills].sort(),
        sessions: item.sessions.read.size,
        referenceSessions: item.sessions.reference.size,
        outcomes: [...item.outcomes]
          .map(([outcome, sessions]) => ({ outcome, sessions: sessions.size }))
          .sort((a, b) => b.sessions - a.sessions || a.outcome.localeCompare(b.outcome)),
      }))
      .sort(
        (a, b) =>
          b.sessions - a.sessions ||
          b.referenceSessions - a.referenceSessions ||
          a.prefix.localeCompare(b.prefix)
      ),
    mappedPrefixes,
    stores: config.stores.map((store): StoreCount => {
      const stat = stats.get(store.name);
      return {
        store: store.name,
        context: store.context,
        outputs: stat?.outputs ?? 0,
        filesRead: stat?.files ?? 0,
        bytesRead: stat?.bytes ?? 0,
        badLines: stat?.bad ?? 0,
        sharedCallIds: stat?.shared ?? 0,
        readSessions: stat?.read.size ?? 0,
        unpairedSessions: stat?.unpaired.size ?? 0,
        earliestRead: stat?.earliest,
        units: [...(storeUnits.get(store.name) ?? [])]
          .map(([unit, sessions]) => ({
            unit: unitLabel(unit),
            sessions: sessions.size,
            copies: members.get(unit)?.size ?? 0,
          }))
          .sort((a, b) => b.sessions - a.sessions || a.unit.localeCompare(b.unit)),
      };
    }),
    namesWithoutCopy,
    readSessions: allReads.size,
    unattributedSessions,
  };
}

/** A copy's status (this file's header): `read` with a read session; else, with no Claude Code
 *  installer, `no read`; else by whether Oh My Pi installs it too. */
function copyStatus(sessions: number, ompInstalls: boolean, claudeInstalls: boolean): Status {
  if (sessions > 0) return "read";
  if (!claudeInstalls) return "no read";
  return ompInstalls ? "no Oh My Pi read; Claude Code not counted yet" : "not countable yet";
}

// ---------------------------------------------------------------------------------------------
// rankings

/** One cut's ranking: its changed skills with the sessions of its repository's copy, in
 *  `rankSkills` order (each row with its skill directory), and each changed skill directory the
 *  copies output holds no copy at. */
export interface Ranking {
  readonly cut: string;
  readonly repository: string;
  readonly rows: readonly {
    readonly skill: string;
    readonly path: string;
    readonly sessions: number;
  }[];
  readonly missing: readonly string[];
}

/** The ranking of the cut `cut` of `repository` whose changed skill directories are `changed` (the
 *  lines of its `--changed` file). A directory the copies output holds no copy at is ranked with 0
 *  sessions, under its directory's name, and named in `missing`. */
export function rankChanged(
  attribution: Attribution,
  cut: string,
  repository: string,
  changed: readonly string[]
): Ranking {
  const counts = new Map(
    attribution.copies.map((count) => [copyKey(count.repository, count.path), count])
  );
  const missing: string[] = [];
  const rows = [
    ...new Set(changed.map((line) => line.trim().replace(/\/+$/, "")).filter((l) => l !== "")),
  ].map((dir) => {
    const count = counts.get(copyKey(repository, dir));
    if (count === undefined) missing.push(dir);
    return {
      skill: count?.name ?? path.posix.basename(dir),
      path: dir,
      sessions: count?.sessions ?? 0,
    };
  });
  const byName = new Map<string, (typeof rows)[number][]>();
  for (const row of rows.toSorted((a, b) => b.sessions - a.sessions))
    entry(byName, row.skill, () => []).push(row);
  const ranked = rankSkills(rows).flatMap((skill) => {
    const next = byName.get(skill)?.shift();
    return next === undefined ? [] : [next];
  });
  return { cut, repository, rows: ranked, missing };
}

// ---------------------------------------------------------------------------------------------
// models

/** Some read sessions by model. */
export interface ModelCount {
  readonly sessions: number;
  /** Each model with its sessions, most first, a tie by model. */
  readonly byModel: readonly (SessionModel & { readonly sessions: number })[];
  /** The sessions whose file's head names no model. */
  readonly noModel: number;
  /** The sessions that no models query's rows hold. */
  readonly notQueried: number;
  /** The sessions with a model that is not an Anthropic model (its id does not start `claude-`). */
  readonly notAnthropic: number;
  /** The sessions with a model the gateway serves. */
  readonly served: number;
  /** The sessions with a model. */
  readonly withModel: number;
}

/** A ranked skill's `<cut id>/models/<skill>.json` (`modelFiles`): its ModelCount, as
 *  `behaviour --reading-models` reads it. */
export const ModelCountFile: z.ZodType<ModelCount> = z.strictObject({
  sessions: Count,
  byModel: z.array(
    z.strictObject({
      model: z.string().min(1),
      anthropic: z.boolean(),
      served: z.boolean(),
      sessions: Count,
    })
  ),
  noModel: Count,
  notQueried: Count,
  notAnthropic: Count,
  served: Count,
  withModel: Count,
});

/** The read sessions `sessions` (each a `sessionKey`) by the model `models` gives each. */
export function modelCount(sessions: ReadonlySet<string>, models: SessionModels): ModelCount {
  const byModel = new Map<string, SessionModel & { sessions: number }>();
  let noModel = 0;
  let notQueried = 0;
  let notAnthropic = 0;
  let served = 0;
  for (const session of sessions) {
    const model = models.models.get(session);
    if (model === undefined) notQueried++;
    else if (model === null) noModel++;
    else {
      entry(byModel, model.model, () => ({ ...model, sessions: 0 })).sessions++;
      if (!model.anthropic) notAnthropic++;
      if (model.served) served++;
    }
  }
  return {
    sessions: sessions.size,
    byModel: [...byModel.values()].sort(
      (a, b) => b.sessions - a.sessions || a.model.localeCompare(b.model)
    ),
    noModel,
    notQueried,
    notAnthropic,
    served,
    withModel: sessions.size - noModel - notQueried,
  };
}

/** The share of `count`'s sessions with a model whose model is not an Anthropic model, to 3
 *  places, or `""` when none of them has a model. */
export function notAnthropicShare(count: ModelCount): string {
  return count.withModel === 0 ? "" : (count.notAnthropic / count.withModel).toFixed(3);
}

export const MODEL_COUNT_COLUMNS = [
  "sessions",
  "by_model",
  "no_model_sessions",
  "not_queried_sessions",
  "not_anthropic_sessions",
  "not_anthropic_share",
  "served_sessions",
  "not_served_sessions",
  "not_served_models",
] as const;
export const MODELS_BY_SKILL_COLUMNS = [
  "repository",
  "path",
  "name",
  ...MODEL_COUNT_COLUMNS,
] as const;
export const MODELS_BY_CUT_COLUMNS = [
  "cut",
  "rank",
  "skill",
  "path",
  ...MODEL_COUNT_COLUMNS,
] as const;

/** `<model>=<sessions>` of each of `items`, joined by `; `. */
const modelList = (items: readonly { model: string; sessions: number }[]) =>
  items.map((item) => `${item.model}=${item.sessions}`).join("; ");

/** The cells of MODEL_COUNT_COLUMNS for `count`: `by_model` and `not_served_models` are each
 *  `<model>=<sessions>`, joined by `; `. */
function modelCells(count: ModelCount): (string | number)[] {
  return [
    count.sessions,
    modelList(count.byModel),
    count.noModel,
    count.notQueried,
    count.notAnthropic,
    notAnthropicShare(count),
    count.served,
    count.withModel - count.served,
    modelList(count.byModel.filter((item) => !item.served)),
  ];
}

/** models-by-skill.tsv's text: one row per copy, in skill-copies.tsv's order. */
export function modelsBySkillTsv(attribution: Attribution, models: SessionModels): string {
  return tsv(
    MODELS_BY_SKILL_COLUMNS,
    attribution.copies.map((copy) => [
      copy.repository,
      copy.path,
      copy.name,
      ...modelCells(modelCount(copy.readSessions, models)),
    ])
  );
}

/** Each ranked changed skill of `ranking`, in rank order, with the read sessions (`sessionKey`) of
 *  the cut repository's copy after every attribution rule: none for a directory the copies output
 *  holds no copy at. */
export function rankedSessions(
  attribution: Attribution,
  ranking: Ranking
): { rank: number; skill: string; path: string; sessions: ReadonlySet<string> }[] {
  const copies = new Map(
    attribution.copies.map((copy) => [copyKey(copy.repository, copy.path), copy])
  );
  return ranking.rows.map((row, index) => ({
    rank: index + 1,
    skill: row.skill,
    path: row.path,
    sessions: copies.get(copyKey(ranking.repository, row.path))?.readSessions ?? new Set(),
  }));
}

/** Each ranked changed skill of `ranking` with its read sessions by model, in rank order. */
export function rankingModels(
  attribution: Attribution,
  ranking: Ranking,
  models: SessionModels
): { rank: number; skill: string; path: string; count: ModelCount }[] {
  return rankedSessions(attribution, ranking).map(({ sessions, ...row }) => ({
    ...row,
    count: modelCount(sessions, models),
  }));
}

/** The ranked changed skills of `ranking` that get files of their own under its cut's directory,
 *  in rank order, each with the read sessions of the cut repository's copy. A skill whose name is
 *  no file name, or that two ranked directories share, gets none and is named in `skipped`. */
function filedSkills(
  attribution: Attribution,
  ranking: Ranking
): { rows: { skill: string; sessions: ReadonlySet<string> }[]; skipped: string[] } {
  const ranked = rankedSessions(attribution, ranking);
  const rows: { skill: string; sessions: ReadonlySet<string> }[] = [];
  const skipped: string[] = [];
  for (const row of ranked) {
    const shared = ranked.filter((other) => other.skill === row.skill).length > 1;
    if (!/^[A-Za-z0-9._-]+$/.test(row.skill) || row.skill.startsWith(".") || shared) {
      skipped.push(`${row.skill} (${row.path})`);
      continue;
    }
    rows.push(row);
  }
  return { rows, skipped };
}

/**
 * The sessions files of `ranking`, `sessions/<skill>.jsonl` under its cut's directory, for
 * `build-cases --sessions`: one per ranked changed skill, one line per read session of the cut
 * repository's copy, `{"store", "hash"}`, sorted by store then hash (an empty file for a skill
 * with none). A skill whose name is no file name, or that two ranked directories share, gets no
 * file and is named in `skipped`.
 */
export function sessionFiles(
  attribution: Attribution,
  ranking: Ranking
): { files: { name: string; text: string }[]; skipped: string[] } {
  const { rows, skipped } = filedSkills(attribution, ranking);
  const files = rows.map((row) => {
    const lines = [...row.sessions].sort().map((key) => {
      const [store = "", hash = ""] = key.split("\t");
      return `${JSON.stringify({ store, hash })}\n`;
    });
    return { name: path.join("sessions", `${row.skill}.jsonl`), text: lines.join("") };
  });
  return { files, skipped };
}

/** The models files of `ranking`, `models/<skill>.json` under its cut's directory, for `behaviour
 *  --reading-models`: one per sessions file (`sessionFiles`), the ModelCount of that file's
 *  sessions by the models `models` gives them, as JSON. */
export function modelFiles(
  attribution: Attribution,
  ranking: Ranking,
  models: SessionModels
): { name: string; text: string }[] {
  return filedSkills(attribution, ranking).rows.map((row) => ({
    name: path.join("models", `${row.skill}.json`),
    text: `${JSON.stringify(modelCount(row.sessions, models), null, 2)}\n`,
  }));
}

/** models-by-cut.tsv's text: each cut's ranked changed skills, in rank order. */
export function modelsByCutTsv(
  attribution: Attribution,
  rankings: readonly Ranking[],
  models: SessionModels
): string {
  return tsv(
    MODELS_BY_CUT_COLUMNS,
    rankings.flatMap((ranking) =>
      rankingModels(attribution, ranking, models).map((item) => [
        ranking.cut,
        item.rank,
        item.skill,
        item.path,
        ...modelCells(item.count),
      ])
    )
  );
}

// ---------------------------------------------------------------------------------------------
// output

/** skill-read-count.tsv's text. */
export function readCountTsv(attribution: Attribution): string {
  return tsv(
    READ_COUNT_COLUMNS,
    attribution.copies.map((count) => [
      count.repository,
      count.path,
      count.name,
      count.sessions,
      count.referenceSessions,
      count.unattributed,
      count.ties.join("; "),
      count.loadedBy.length > 0 ? "yes" : "no",
      count.status,
    ])
  );
}

/** `text` as Markdown inline code. */
function code(text: string): string {
  return text.includes("`") ? `\`\` ${text} \`\`` : `\`${text}\``;
}
/** `text` as one Markdown table cell: each backslash escaped first, then each `|`, so a cell's
 * own backslash never escapes the pipe after it. */
const cell = (text: string | number) =>
  String(text).replaceAll("\\", "\\\\").replaceAll("|", "\\|");
const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
/** `count` with `word`, or with `plural` unless the count is 1: `1 session`, `2 sessions`. */
const counted = (count: number, word: string, plural = `${word}s`) =>
  `${count} ${count === 1 ? word : plural}`;

/** What the models directory gives: its outputs, its queries' issues and each session's model. */
export interface ModelsRead {
  readonly dir: QueryDir<ModelOutput>;
  readonly issues: readonly QueryIssue[];
  readonly sessions: SessionModels;
}

/** Everything the report says. */
export interface Report {
  readonly config: SkillCutsConfig;
  readonly attribution: Attribution;
  readonly count: CountRead;
  readonly issues: readonly QueryIssue[];
  readonly spans: readonly Span[];
  readonly r4: R4Tables;
  readonly rankings: readonly Ranking[];
  /** Undefined when no `--models` directory was given. */
  readonly models: ModelsRead | undefined;
}

/** A count's models as a report names them: at most `most`, each `<model> (<sessions>)`, with
 *  `not served` beside a model the gateway does not serve. */
function modelsSaid(count: ModelCount, most: number): string {
  const said = count.byModel
    .slice(0, most)
    .map((item) => `${item.model} (${item.sessions}${item.served ? "" : ", not served"})`)
    .join(", ");
  const more = count.byModel.length - most;
  return more > 0 ? `${said} and ${more} more` : said || "none";
}

/** The report's section on the models of the reading sessions. */
function modelsMarkdown(report: Report): string[] {
  const { attribution, rankings, models, config } = report;
  const lines = ["", "## Models of the reading sessions", ""];
  if (models === undefined) return [...lines, "Not counted: no --models directory was given."];
  const all = modelCount(
    new Set(attribution.copies.flatMap((copy) => [...copy.readSessions])),
    models.sessions
  );
  const files = models.dir.outputs.reduce((sum, output) => sum + output.summary.files, 0);
  const named = models.dir.outputs.reduce((sum, output) => sum + output.summary.with_model, 0);
  lines.push(
    "Each session's model is read from the head of its file (models.sql): the file row's content, then each part that starts below byte 262144, cut to 262144 characters. The head's first `model_change` line of Oh My Pi gives it, else a subagent's `session_init` `resolvedModel`, else Claude Code's first assistant message's model, else its `model` attachment. A thinking suffix (`:low`) is dropped, and Claude Code's model id is written `anthropic/<id>`. A model is an Anthropic model when its id (its text after the last `/`) starts `claude-`, whichever gateway or provider names it. The gateway serves a model when its id is the id of one of the configuration's `servedModels`: " +
      (config.servedModels.length === 0 ? "none." : `${config.servedModels.join(", ")}.`),
    "",
    `The models queries read ${counted(files, "file")}; the head of ${named} of them names a model. The ${counted(all.sessions, "read session")} of the copies: with a model ${all.withModel}; whose file's head names none ${all.noModel}; in no models query's rows ${all.notQueried}. Sessions two queries give two models for (the first day's is kept): ${models.sessions.conflicts}.`,
    "",
    "### Models queries that failed, were split, or have no output",
    ""
  );
  if (models.issues.length === 0) lines.push("None.");
  for (const issue of models.issues) {
    lines.push(
      `- ${issue.store === "" ? "" : `${issue.store} `}${issue.name === "" ? "" : `${issue.name} `}(${issue.what}): ${issue.detail}`
    );
  }
  lines.push(
    "",
    "### The models of the copies' read sessions",
    "",
    "| Model | Anthropic model | Served by the gateway | Sessions |",
    "| --- | --- | --- | ---: |"
  );
  for (const item of all.byModel) {
    lines.push(
      `| ${cell(item.model)} | ${item.anthropic ? "yes" : "no"} | ${item.served ? "yes" : "no"} | ${item.sessions} |`
    );
  }
  lines.push(
    "",
    "### Each cut's ranked changed skills: the share of reading sessions on a model that is not Anthropic's, and the sessions on a model the gateway serves",
    "",
    "The share is of the sessions with a model. models-by-cut.tsv holds each skill's every model, and the models the gateway does not serve. Each ranked skill's counts are also in `<cut id>/models/<skill>.json`, of the sessions its sessions file holds: what `behaviour --reading-models` reads.",
    ""
  );
  if (rankings.length === 0) lines.push("None: no --changed file was given.");
  for (const ranking of rankings) {
    lines.push(
      `#### ${ranking.cut} (${ranking.repository})`,
      "",
      "| Rank | Skill | Sessions | With a model | Not Anthropic | Share not Anthropic | Served | Not served | No model | Not queried | Models |",
      "| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |"
    );
    for (const item of rankingModels(attribution, ranking, models.sessions)) {
      const count = item.count;
      lines.push(
        `| ${item.rank} | ${cell(item.skill)} | ${count.sessions} | ${count.withModel} | ${count.notAnthropic} | ${notAnthropicShare(count) || "none"} | ${count.served} | ${count.withModel - count.served} | ${count.noModel} | ${count.notQueried} | ${cell(modelsSaid(count, 5))} |`
      );
    }
    lines.push("");
  }
  return lines;
}

/** skill-read-count.md's text. */
export function readCountMarkdown(report: Report): string {
  const { config, attribution, count, issues, spans, r4, rankings } = report;
  const lines: string[] = [];
  const say = (...text: string[]) => lines.push(...text);
  const windowStart = Date.parse(config.windowStart);
  const windowEnd = Date.parse(config.windowEnd);
  const windowDays = (windowEnd - windowStart) / DAY_MS;
  const of = (status: Status) => attribution.copies.filter((copy) => copy.status === status);
  const repositories = [...new Set(attribution.copies.map((copy) => copy.repository))].sort();
  const names = new Set(attribution.copies.map((copy) => copy.name));

  say(
    "# Skill read count",
    "",
    `End time ${config.windowEnd}. The window is [${config.windowStart}, ${config.windowEnd}), ${windowDays} days. Each query read the files of one store whose mtime is in one UTC day (or one hour of a day whose query timed out), from the window's start on; a read counts when its call's timestamp is in the window. A session is one transcript file, so a subagent's file is a session of its own, and a file that two queries hold is one session.`,
    "",
    `${attribution.copies.length} copies of ${names.size} names in ${repositories.length} repositories: ` +
      STATUSES.map((status) => `${of(status).length} ${status}`).join(", ") +
      `. ${counted(attribution.readSessions, "session")} read a skill; ${attribution.unattributedSessions} of them read a skill that no copy got.`,
    "",
    "## Stores",
    "",
    "| Store | Context | Earliest record | Files in the store | Earliest read in the window | Sessions with a read | Queries with output | Files read | Bytes read | Lines not JSON | Call ids in two files |",
    "| --- | --- | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |"
  );
  for (const store of attribution.stores) {
    const span = spans.find((candidate) => candidate.store === store.store);
    const earliest =
      span === undefined
        ? "not measured"
        : span.earliest_mtime_ms === null
          ? "no record"
          : iso(span.earliest_mtime_ms);
    say(
      `| ${cell(store.store)} | ${cell(store.context)} | ${earliest} | ${span?.files ?? "not measured"} | ${store.earliestRead ?? "no read"} | ${store.readSessions} | ${store.outputs} | ${store.filesRead} | ${store.bytesRead} | ${store.badLines} | ${store.sharedCallIds} |`
    );
  }
  say("");
  for (const store of attribution.stores) {
    const span = spans.find((candidate) => candidate.store === store.store);
    if (span === undefined) {
      say(
        `- ${store.store}: the spans file has no line for it, so its earliest record is not known.`
      );
    } else if (span.earliest_mtime_ms !== null && span.earliest_mtime_ms > windowStart) {
      const days = Math.round(((windowEnd - span.earliest_mtime_ms) / DAY_MS) * 10) / 10;
      say(
        `- ${store.store} starts after the window's start: its records span ${iso(span.earliest_mtime_ms)} to the end time, ${days} of the window's ${windowDays} days.`
      );
    }
    if (store.unpairedSessions > 0) {
      say(
        `- ${store.store}: a call with no result in its file, in ${counted(store.unpairedSessions, "session")}: such a call is not a read, and no copy counts it.`
      );
    }
  }
  say(
    '- The transcript store is not read: the cluster store that a pod reads holds no Claude Code transcript until the Claude Code transcripts are moved into it, so this count holds no Claude Code session. "Claude Code" below lists the skills that leaves out.'
  );
  for (const dir of count.otherStores) {
    say(
      `- The count directory holds ${code(dir)}, which the configuration names no store for; it is not read.`
    );
  }

  say("", "## Queries that failed, were split, or have no output", "");
  if (issues.length === 0)
    say("None: each query of the window has its output, and the run log lists no failure.");
  for (const issue of issues) {
    say(
      `- ${issue.store === "" ? "" : `${issue.store} `}${issue.name === "" ? "" : `${issue.name} `}(${issue.what}): ${issue.detail}`
    );
  }
  if (count.log !== undefined && count.log.ended === undefined) {
    say(
      "",
      "The run log has no END line: the count was still running, so its days after the window's end are not checked."
    );
  }

  say(
    "",
    "## How a read is attributed",
    "",
    "1. The prefix table (prefix-table.tsv): a read goes to the copy that every row whose prefix matches its copy path maps to, when they map to one copy.",
    "2. A checkout: a read whose path ends with a copy's skill directory in its repository (with a path boundary before it) is a read of that copy in a checkout of its repository; the longest such directory wins. Copies of several repositories at that directory are told apart by the path before it: the one whose owner and name are two directories of it in a row, else the one whose name a directory holds (`.dotfiles` holds `dotfiles`); a tie left is unattributed. A skill directory of one segment (a skill at its repository's root) counts only when the path names its repository.",
    "3. The store's context: the units (packages, plugins, a host's user skills) that the store's own reads of step 1 come from are the units its context installs. Their copies are their installer rows' copies and the copies of their prefix rows. When those hold exactly one copy of the read's skill name, the read goes to that copy.",
    "4. Otherwise the read is unattributed for its skill name.",
    "",
    "Two copies of one name are a tie when their sessions differ by at most that name's unattributed sessions.",
    "",
    "### The units each store's context installs",
    ""
  );
  for (const store of attribution.stores) {
    if (store.units.length === 0) {
      say(
        `- ${store.store} (${store.context}): none, since no read of this store maps by the prefix table; the context rule places none of its reads.`
      );
      continue;
    }
    say(`- ${store.store} (${store.context}):`);
    for (const unit of store.units) {
      say(
        `  - ${unit.unit}: ${counted(unit.sessions, "session")} read from it; it holds ${counted(unit.copies, "copy", "copies")}`
      );
    }
  }

  const unmappedSessions = attribution.unmapped.reduce((sum, item) => sum + item.sessions, 0);
  say(
    "",
    "### Copy paths the prefix table does not map",
    "",
    `${counted(attribution.unmapped.length, "prefix", "prefixes")}. A session is counted under each prefix it read from (${counted(unmappedSessions, "read session")} over all prefixes). Each outcome names the rule that placed its reads (checkout or context), or why they are unattributed.`,
    ""
  );
  if (attribution.unmapped.length > 0) {
    say(
      "| Prefix | Skill | Sessions | Reference sessions | Where they went |",
      "| --- | --- | ---: | ---: | --- |"
    );
    for (const item of attribution.unmapped) {
      const went = item.outcomes
        .map((o) => `${o.outcome} (${counted(o.sessions, "session")})`)
        .join("; ");
      say(
        `| ${cell(code(item.prefix))} | ${cell(item.skills.join(", "))} | ${item.sessions} | ${item.referenceSessions} | ${cell(went)} |`
      );
    }
  }

  say(
    "",
    "### Copy paths the prefix table maps",
    "",
    `${counted(attribution.mappedPrefixes.size, "prefix", "prefixes")}, each placed by the prefix table (step 1).`,
    ""
  );
  if (attribution.mappedPrefixes.size > 0) say("| Prefix | Copy |", "| --- | --- |");
  for (const [prefix, mapped] of [...attribution.mappedPrefixes].sort(([a], [b]) =>
    a.localeCompare(b)
  )) {
    say(`| ${cell(code(prefix))} | ${cell([...mapped].sort().join(", "))} |`);
  }

  say("", "### Ties", "");
  const tied = attribution.copies.filter((copy) => copy.ties.length > 0);
  if (tied.length === 0) say("None.");
  for (const copy of tied) {
    say(
      `- ${copy.name}: ${copy.repository} ${copy.path} (${counted(copy.sessions, "session")}) ties with ${copy.ties.join(", ")}; the name has ${counted(copy.unattributed, "unattributed session")}.`
    );
  }

  say("", "### Skills read that no copy in the copies output has", "");
  if (attribution.namesWithoutCopy.length === 0) say("None.");
  for (const item of attribution.namesWithoutCopy) {
    say(
      `- ${item.name}: ${counted(item.sessions, "session")}, ${counted(item.referenceSessions, "reference session")}`
    );
  }

  say(
    "",
    "## Loaded skills",
    "",
    "A skill that code, an agent definition or an installer loads into a session. The diagnosis of unread skills leaves these out.",
    ""
  );
  const loaded = attribution.copies.filter((copy) => copy.loadedBy.length > 0);
  if (loaded.length === 0) say("None.");
  for (const copy of loaded) {
    say(
      `- ${copy.name} (${copy.repository} ${copy.path}, ${counted(copy.sessions, "session")}): ${copy.loadedBy.join("; ")}`
    );
  }

  say("", "## Skills with no read, per repository", "");
  for (const repository of repositories) {
    const unread = attribution.copies.filter(
      (copy) => copy.repository === repository && copy.status !== "read"
    );
    say(`### ${repository}`, "");
    if (unread.length === 0) say("None.");
    for (const copy of unread) {
      const free =
        copy.unattributed > 0
          ? `; ${counted(copy.unattributed, "session")} read ${copy.name} unattributed`
          : "";
      say(`- ${copy.name} (${copy.path}): ${copy.status}${free}`);
    }
    say("");
  }

  say(
    "## Claude Code",
    "",
    'This count reads no Claude Code session (see "Stores"). Oh My Pi installs a copy when an Oh My Pi session read it (every store holds Oh My Pi sessions), when a store\'s context installs it (the units above), or when an installers.tsv row of host omp installs it (an installer script into Oh My Pi, an Oh My Pi plugins package.json, a package.json with omp.skills, the prepack script). Claude Code installs it when an installers.tsv row of host claude-code does.',
    "",
    "### Not countable yet: only Claude Code installs them, and no Oh My Pi session read them",
    ""
  );
  const notCountable = of("not countable yet");
  if (notCountable.length === 0) say("None.");
  for (const copy of notCountable) say(`- ${copy.name} (${copy.repository} ${copy.path})`);
  say("", "### No Oh My Pi read; Claude Code not counted yet: both hosts install them", "");
  const both = of("no Oh My Pi read; Claude Code not counted yet");
  if (both.length === 0) say("None.");
  for (const copy of both) say(`- ${copy.name} (${copy.repository} ${copy.path})`);

  say(
    "",
    "## Bytes of skill names and descriptions, per repository",
    "",
    "| Repository | Commit | Skills | Name bytes | Description bytes | Total bytes |",
    "| --- | --- | ---: | ---: | ---: | ---: |"
  );
  for (const repository of repositories) {
    const rows = r4.copies.filter((copy) => copy.repository === repository);
    const nameBytes = rows.reduce((sum, copy) => sum + Buffer.byteLength(copy.name, "utf8"), 0);
    const descriptionBytes = rows.reduce((sum, copy) => sum + Number(copy.description_bytes), 0);
    const commit =
      r4.repositories.find((repo) => repo.repository === repository)?.commit ||
      "not in repositories.tsv";
    say(
      `| ${cell(repository)} | ${commit} | ${rows.length} | ${nameBytes} | ${descriptionBytes} | ${nameBytes + descriptionBytes} |`
    );
  }

  say("", "## Rankings", "");
  if (rankings.length === 0) say("None: no --changed file was given.");
  for (const ranking of rankings) {
    const top = ranking.rows
      .slice(0, 5)
      .map((row) => `${row.skill} (${row.sessions})`)
      .join(", ");
    const { skipped } = filedSkills(attribution, ranking);
    say(
      `- ${ranking.cut} (${ranking.repository}): ${ranking.rows.length} changed skills, in ${code(`${ranking.cut}/ranking.json`)}, with each one's reading sessions in ${code(`${ranking.cut}/sessions/<skill>.jsonl`)}; first: ${top || "none"}.` +
        (ranking.missing.length > 0
          ? ` The copies output holds no copy at ${ranking.missing.map(code).join(", ")}: ranked with 0 sessions.`
          : "") +
        (skipped.length > 0
          ? ` No sessions file for ${skipped.join(", ")}: its name is no file name, or two ranked directories share it.`
          : "")
    );
  }
  const unranked = config.cuts.filter((cut) => !rankings.some((ranking) => ranking.cut === cut.id));
  if (unranked.length > 0) {
    say(`- Not ranked (no --changed file): ${unranked.map((cut) => cut.id).join(", ")}.`);
  }
  lines.push(...modelsMarkdown(report));
  return `${lines.join("\n")}\n`;
}

// ---------------------------------------------------------------------------------------------
// the command

const USAGE =
  "usage: attribute.ts --config <skill-cuts.json> --count <dir> --copies <dir> --spans <file> --out <dir> [--models <dir>] [--changed <cut id>=<file>]...";

/** What `main` writes to. */
export interface Output {
  out(text: string): void;
  err(text: string): void;
}

/** Whether `inner` is `outer` or a path inside it. */
function inside(inner: string, outer: string): boolean {
  const relative = path.relative(path.resolve(outer), path.resolve(inner));
  return (
    relative === "" ||
    (relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
  );
}

/** The command. Returns the exit status: 0 (the count is written), 1 (an input could not be read
 *  or does not fit), 2 (refused). */
export function main(argv: readonly string[], io: Output): number {
  const refuse = (why: string) => {
    io.err(`attribute: ${why}`);
    return 2;
  };
  let values: {
    config?: string;
    count?: string;
    copies?: string;
    spans?: string;
    out?: string;
    models?: string;
    changed?: string[];
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        config: { type: "string" },
        count: { type: "string" },
        copies: { type: "string" },
        spans: { type: "string" },
        out: { type: "string" },
        models: { type: "string" },
        changed: { type: "string", multiple: true },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    return refuse(`${(error as Error).message}. ${USAGE}`);
  }
  const {
    config: configFile,
    count: countDir,
    copies: copiesDir,
    spans: spansFile,
    out,
    models: modelsDir,
  } = values;
  if (
    configFile === undefined ||
    countDir === undefined ||
    copiesDir === undefined ||
    spansFile === undefined ||
    out === undefined
  ) {
    return refuse(`--config, --count, --copies, --spans and --out are required. ${USAGE}`);
  }
  for (const [flag, dir] of [
    ["--count", countDir],
    ["--copies", copiesDir],
    ["--models", modelsDir],
  ] as const) {
    if (dir === undefined) continue;
    if (inside(out, dir))
      return refuse(`--out ${out} is inside ${flag} ${dir}, which is only read`);
  }
  let config: SkillCutsConfig;
  try {
    config = loadConfig(configFile);
  } catch (error) {
    return refuse((error as Error).message);
  }
  const changed: { cut: string; repository: string; file: string }[] = [];
  for (const item of values.changed ?? []) {
    const at = item.indexOf("=");
    const id = at < 0 ? "" : item.slice(0, at);
    const file = at < 0 ? "" : item.slice(at + 1);
    if (id === "" || file === "")
      return refuse(`--changed ${JSON.stringify(item)} is not <cut id>=<file>`);
    const cut = config.cuts.find((candidate) => candidate.id === id);
    if (cut === undefined) {
      return refuse(
        `--changed names the cut ${JSON.stringify(id)}, which the configuration does not; it names ${config.cuts.map((c) => c.id).join(", ")}`
      );
    }
    if (!/^[A-Za-z0-9._-]+$/.test(id) || id === "." || id === "..") {
      return refuse(`the cut id ${JSON.stringify(id)} cannot name a directory under --out`);
    }
    if (changed.some((entry) => entry.cut === id))
      return refuse(`--changed names the cut ${id} twice`);
    changed.push({ cut: id, repository: cut.repo, file });
  }
  try {
    const stores = config.stores.map((store) => store.name);
    const count = readCount(countDir, stores);
    const r4 = readR4(copiesDir);
    const spans = parseSpans(readFileSync(spansFile, "utf8"), spansFile);
    const attribution = attribute(config, count.outputs, r4);
    const issues = queryIssues(config, count);
    let models: ModelsRead | undefined;
    if (modelsDir !== undefined) {
      const dir = readQueryDir(modelsDir, stores, parseModelOutput);
      models = {
        dir,
        issues: queryIssues(config, dir),
        sessions: sessionModels(dir.outputs, config.servedModels),
      };
    }
    const rankings = changed.map((item) =>
      rankChanged(
        attribution,
        item.cut,
        item.repository,
        readFileSync(item.file, "utf8").split("\n")
      )
    );
    mkdirSync(out, { recursive: true });
    const written: string[] = [];
    const write = (name: string, text: string) => {
      const file = path.join(out, name);
      mkdirSync(path.dirname(file), { recursive: true });
      writeFileSync(file, text);
      written.push(file);
    };
    write("skill-read-count.tsv", readCountTsv(attribution));
    write(
      "skill-read-count.md",
      readCountMarkdown({ config, attribution, count, issues, spans, r4, rankings, models })
    );
    /** Writes `files` (each named below the cut's directory) into its directory `dir`, made even
     *  when they are none, and lists that directory once. */
    const writeDir = (
      cut: string,
      dir: string,
      files: readonly { name: string; text: string }[]
    ) => {
      const full = path.join(out, cut, dir);
      mkdirSync(full, { recursive: true });
      for (const file of files) writeFileSync(path.join(out, cut, file.name), file.text);
      written.push(`${full}/ (${files.length} files)`);
    };
    for (const ranking of rankings) {
      const rows = ranking.rows.map(({ skill, sessions }) => ({ skill, sessions }));
      write(path.join(ranking.cut, "ranking.json"), `${JSON.stringify(rows, null, 2)}\n`);
      writeDir(ranking.cut, "sessions", sessionFiles(attribution, ranking).files);
      if (models !== undefined)
        writeDir(ranking.cut, "models", modelFiles(attribution, ranking, models.sessions));
    }
    if (models !== undefined) {
      write("models-by-skill.tsv", modelsBySkillTsv(attribution, models.sessions));
      write("models-by-cut.tsv", modelsByCutTsv(attribution, rankings, models.sessions));
    }
    const read = attribution.copies.filter((copy) => copy.status === "read").length;
    io.out(
      `${count.outputs.length} query outputs, ${attribution.copies.length} copies (${read} read), ` +
        `${attribution.readSessions} read sessions (${attribution.unattributedSessions} unattributed), ` +
        `${attribution.unmapped.length} unmapped prefixes, ${issues.length} queries failed, split or without output` +
        (models === undefined
          ? ""
          : `; ${models.dir.outputs.length} models query outputs, ${models.issues.length} failed, split or without output`) +
        "\n" +
        written.map((file) => `${file}\n`).join("")
    );
    return 0;
  } catch (error) {
    io.err(`attribute: ${(error as Error).message}`);
    return 1;
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
