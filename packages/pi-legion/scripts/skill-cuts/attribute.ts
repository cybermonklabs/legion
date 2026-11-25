/**
 * The attribution of the read count: the count's totals (count.sql's rows, one psql output per
 * store and UTC day of file mtime) set against the copies output's copies (copies.ts), into
 * one row per skill copy with its sessions, the report beside it, and each cut's ranking.
 *
 *   bun scripts/skill-cuts/attribute.ts --config <skill-cuts.json> --count <dir> --copies <dir>
 *     --spans <file> --out <dir> [--models <dir>] [--changed <cut or set id>=<file>]...
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
 *              query that has no `.txt` has no output: it failed, or it has not run yet.
 *              Each store with count output needs `<store>/roots.txt` from `count-sql --query
 *              roots`. It names each session's Oh My Pi profile by hash, without paths.
 *              With transcriptStore, `transcripts/coverage.txt` supplies the host spans,
 *              profiles and file hashes. Its daily files contain count rows keyed by host.
 *              Each Postgres store's `hashes.txt` establishes its file ownership.
 *              `run.log` lists every query run, `<time>\t<store>\t<day or hour>\trc=<status>\tseconds=<n>\t
 *              <its stderr>`, a `FAILED ...` line for each query given up, and `END <time>` when
 *              the run ended.
 *   --copies   The copies output (copies.ts): skill-copies.tsv, prefix-table.tsv,
 *              former-stages.tsv, installers.tsv, loaders.tsv, manifests.tsv and
 *              repositories.tsv, each with the columns copies.ts exports for it. An output with
 *              no former-stages.tsv (made before copies.ts wrote it) is refused (exit 1), naming
 *              the file: copies must run again.
 *   --spans    One JSON line per store: `{store, earliest_mtime_ms, files}`, the store's earliest
 *              file mtime (epoch milliseconds) and its number of files.
 *   --models   The models query's raw output (models.sql), in `--count`'s layout and with its own
 *              run.log: `<store>/<YYYY-MM-DD>[T<HH>].txt`, each one models.sql result, a row per
 *              file `{hash, model, source}` and a summary.
 *   --changed  `<cut or set id>=<file>`, once for each cut of the configuration to rank, or each
 *              skill set (`sets`, config.ts's `cutOrSet`), ranked as a cut is, its repository the
 *              set's `repo`. The file holds the cut's changed skills, or the set's skills to rank:
 *              one skill directory of its repository per line, as skill-copies.tsv's `path`
 *              writes it. Below, a set's id stands where a cut's does.
 *
 * Rules.
 *   A session is one transcript file: a 16-hex file hash of a count row, with its store. Each one
 *   is counted once, however many queries of its store hold it: a resumed session whose mtime day
 *   moved while the count ran is in two queries. A read that two files hold (one key of
 *   count.sql's `calls` or `unpaired_calls`: a forked session copies its parent's lines) counts in
 *   the file whose hash sorts first, over every query of the store, by the rule count.sql applies
 *   inside one query (`countOnce`), since a fork written on another mtime day than its parent is
 *   in another query; a file stays in a row when one of its keys there counts in it. A count
 *   whose rows have no `calls` (made by an earlier count.sql) fails the command (exit 1). A `read`
 *   row counts in `sessions`, and a `reference` row (a read of `skill://<name>/<other>` or of
 *   `<name>/references/...`) in `reference_sessions`. A call that has no result in its file is
 *   not a read: the report counts such sessions per store, and no copy counts them.
 *   Transcript files that Postgres holds stay with Postgres. Of the rest, a hash imported by
 *   several transcript hosts is kept on the first host in byte order. The read-key rule applies
 *   across all of those hosts, but their placement groups and coverage are separate.
 *   The copy of a count row is decided in this order:
 *   1. The prefix table. When every prefix-table.tsv row whose pattern matches the row's copy path
 *      maps to the same copy, the row goes to that copy. The match is `prefixRegExp`, the one
 *      matcher that `prefixMatches` uses, compiled once for each row. Only the prefix table can
 *      place reads under the worker image's install location (copies.ts's IMAGE_ROOT,
 *      `/opt/legion/`). A miss or tie leaves an image read unattributed, with any former-stage
 *      note for its path. A prefix row for a Git rename (recorded in former-stages.tsv) maps
 *      the old path to the renamed copy under the name the script staged. The read counts in
 *      the copy's `sessions` and in its `former_name_sessions`, when every row that maps the
 *      path is such a row.
 *   2. A checkout (`checkoutRule`), for a path not under the worker image's install location.
 *      When the row's copy-path prefix ends with a copy's
 *      repository-relative skill directory, at a path boundary, the row goes to that copy: a read
 *      in a checkout of its repository (a dotfiles checkout, a Legion workspace, a relative path
 *      from the session's working directory). The longest such directory wins; copies of several
 *      repositories at it are told apart by how the path names the repository, the most first:
 *      its owner and its name as two directories in a row, its name as a directory, its owner and
 *      a directory that holds its name in a row, a directory that holds its name; a tie left goes
 *      to step 3, among the tied copies alone. A one-segment skill directory counts only when the
 *      path names its repository. A path inside a copy of another repository that an installer
 *      writes (`<dir>/vendor/<name>/`, a package under `node_modules/`, a plugin under
 *      `plugins/cache/`) is a checkout only of a repository that the path from that copy's own
 *      directory on names; when it names none, step 3 decides.
 *   3. The session's profile. For each store and Oh My Pi profile, infer the installed units
 *      from that group's step-1 reads. Their copies are the installers.tsv rows of those units
 *      and the prefix rows' own copies. For the default profile of a devbox store, the units
 *      are also each unit that the dev box's own installers install into Oh My Pi (installers.tsv
 *      rows of the one repository of manifests.tsv that holds an installer script and
 *      `.claude/settings.json`, host `omp`, action ships, stages, links or installs), whether or
 *      not the session read from it: `/home/<user>/.agents/skills/gh/` ties cli/cli with another
 *      repository's gh, and the dev box installs cli/cli's. Place the read only when these units
 *      have one copy of the name, or one of the copies in a checkout tie. A matching former-name
 *      row can also name the copy if one of its units is in the group. With no known profile, this
 *      step cannot place the read. A read under a former name counts in `former_name_sessions`.
 *      A row whose path is inside an `ensure_vendor` clone (`<dir>/vendor/<name>/`) goes to no copy
 *      by this step: no prefix row maps that clone, so the copies output's end time holds no
 *      installer that makes it, and the copies the context installs then are other copies than
 *      the clone the read came from.
 *      A project skill is read inside a project skills directory (`PROJECT_SKILL_DIRS`: `.omp/skills`,
 *      `.claude/skills`, `.agent/skills`, `.agents/skills`, `.codex/skills`, `.opencode/skills`,
 *      `.github/skills`) of a checkout, not of a home directory: an absolute path, or one
 *      relative to the session's working directory. Step 2 places it only at a copy that is in
 *      such a directory too, and this step never places it (apart from a step-2 tie among such
 *      copies): no plugin or other unit that holds a skill of the same name is the project's
 *      skill. With no such copy in the copies output it is unattributed, with that reason.
 *   4. Otherwise the row is unattributed for its skill name: a step-2 tie with the reason of the
 *      tie and of the context.
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
 *   An Oh My Pi read, a context's installed unit, or an `omp` installer row indicates an Oh My Pi
 *   install. A `claude-code` installer row indicates a Claude Code install. A copy with a read
 *   session is `read`. A copy with no such session is `no read` if Claude Code is counted or has
 *   no install of the copy. Without Claude Code coverage, the two legacy statuses distinguish
 *   the copies installed only for Claude Code and those also installed for Oh My Pi.
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
 *                          repository, path, name, sessions, former_name_sessions (of `sessions`,
 *                          those that read it under a former name, step 1 and 3),
 *                          reference_sessions, unattributed_sessions (of the name), tie (each copy
 *                          of the name it ties with, `<repository> <path>`, joined by `; `), loaded
 *                          (yes or no), status, transcript_store_sessions (added read sessions)
 *                          and partial_coverage_hosts (contributing hosts when most added read
 *                          sessions came from partially covered host/format pairs).
 *   skill-read-count.md    The end time and the window; the stores with their spans, earliest
 *                          record and earliest read, and the call keys two files hold (in one
 *                          query, and across queries with the pairs of files that hold them);
 *                          each failed, split or missing query; the attribution rule with the
 *                          transcript-host coverage, sessions added and sessions with reads;
 *                          units each store's context installs; the unmapped prefixes with their
 *                          sessions and the rule that placed them;
 *                          the mapped prefixes with their copies; each copy read under a former
 *                          name, with its sessions and those under that name; the ties; the
 *                          loaded list; per
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
 *   <cut id>/store-only/<skill>.json
 *                          With a configured transcriptStore, for each `--changed` cut, one file
 *                          per sessions file (`storeOnlyFiles`): the skill, its counted sessions
 *                          (the copy's `sessions`, which its ranking counts), those of them only
 *                          the transcript store holds (its `transcript_store_sessions`), which no
 *                          sessions file lists and so no case comes from, and each host's of
 *                          those, as JSON: what `behaviour report --store-only` and `build-cases
 *                          --store-only` read (StoreOnlyFile). With no transcriptStore it writes
 *                          none.
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
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { Refused, schemaFaults, sha256 } from "./common";
import {
  cutOrSet,
  loadConfig,
  type SkillCutsConfig,
  TRANSCRIPT_SOURCE,
  TranscriptHost,
  transcriptStoreName,
} from "./config";
import {
  FORMER_STAGES_COLUMNS,
  IMAGE_ROOT,
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

const TRANSCRIPT_PREFIX = `${TRANSCRIPT_SOURCE}@`;
const Hash = z.string().regex(/^[0-9a-f]{16}$/, { error: "a file hash of 16 hex characters" });
const Key = z.string().regex(/^[0-9a-f]{16}$/, { error: "a call key of 16 hex characters" });
/** Each file of a count row, by its hash, with the keys of its reads that put it in the row. */
const Calls = z.record(Hash, z.array(Key).min(1));
const Count = z.number().int().nonnegative();
/** A session as the count names it across stores: its store and its file hash. */
export const sessionKey = (store: string, hash: string) => `${store}\t${hash}`;
/** The store of the session `key` (a `sessionKey`): a store's name holds no tab. */
export const keyStore = (key: string) => key.slice(0, key.indexOf("\t"));
/** The sessions `keys` (each a `sessionKey`), in their order, as a sessions file's text: one line
 *  `{"store", "hash"}` each. */
export const sessionLines = (keys: readonly string[]) =>
  keys
    .map((key) => {
      const [store = "", hash = ""] = key.split("\t");
      return `${JSON.stringify({ store, hash })}\n`;
    })
    .join("");

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
  calls: Calls,
  unpaired: Count,
  unpaired_hashes: z.array(Hash),
  unpaired_calls: Calls,
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

/** The fields of a count row that name its files' keys. */
const CALL_FIELDS = ["calls", "unpaired_calls"] as const;

/** A count query's output from an earlier count.sql, whose rows name no file's keys: no read in
 *  it can be counted once across queries, so it ends the read of the count (readQueryDir) rather
 *  than being set aside as one unreadable query. */
class EarlierCount extends Error {}

/** The rows and the summary of the psql rows `values` of one query of the store `store`, each
 *  row parsed by `schema` (`what` names the query's SQL file) and the summary kept apart; `check`
 *  throws on a row the query's own rule refuses. Throws, naming the row, on a row of another shape
 *  or store, a row after the summary or one `check` refuses, and when no summary ends it (the
 *  query did not end). */
function queryRows<Row extends { readonly row: string; readonly store: string }>(
  store: string,
  values: readonly unknown[],
  schema: z.ZodType<Row>,
  what: string,
  check: (row: Exclude<Row, { row: "summary" }>, at: string) => void
): { rows: Exclude<Row, { row: "summary" }>[]; summary: Extract<Row, { row: "summary" }> } {
  const rows: Exclude<Row, { row: "summary" }>[] = [];
  let summary: Extract<Row, { row: "summary" }> | undefined;
  for (const [index, value] of values.entries()) {
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
 *  summary, hashes that do not match their counts or their calls, or no summary (the query did not
 *  end). Throws an EarlierCount, naming the field, on a count row with no `calls` or
 *  `unpaired_calls`. */
export function parseQueryOutput(store: string, name: string, text: string): QueryOutput {
  const values = psqlRows(text);
  return parseCountRows(store, name, values);
}

function parseCountRows(store: string, name: string, values: readonly unknown[]): QueryOutput {
  for (const [index, value] of values.entries()) {
    if (typeof value !== "object" || value === null || !("row" in value)) continue;
    if (value.row !== "count") continue;
    const missing = CALL_FIELDS.filter((field) => !Object.hasOwn(value, field));
    if (missing.length > 0) {
      const fields = missing.map((field) => JSON.stringify(field)).join(" and no ");
      throw new EarlierCount(
        `the count query ${store} ${name}: row ${index + 1} has no ${fields} field, so an earlier count.sql made it and its reads cannot be counted once across queries; run the count again with this count.sql`
      );
    }
  }
  /** Whether `calls` names exactly the files of `hashes`. */
  const sameFiles = (calls: Readonly<Record<string, unknown>>, hashes: readonly string[]) =>
    Object.keys(calls).length === hashes.length &&
    hashes.every((hash) => Object.hasOwn(calls, hash));
  const { rows, summary } = queryRows(store, values, QueryRow, "count.sql", (row, at) => {
    if (row.hashes.length !== row.files || row.unpaired_hashes.length !== row.unpaired) {
      throw new Error(`${at}: its hashes do not match its files or unpaired counts`);
    }
    if (!sameFiles(row.calls, row.hashes) || !sameFiles(row.unpaired_calls, row.unpaired_hashes))
      throw new Error(`${at}: its calls or unpaired_calls do not name the files of its hashes`);
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
  /** The SHA-256 of the bytes of each `.txt` parsed into an output, the bytes it was parsed from,
   *  by its path in the directory, `<store>/<file>`. */
  readonly digests: ReadonlyMap<string, string>;
}
export type SessionProfiles = ReadonlyMap<string, string>;
export type CountRead = QueryDir<QueryOutput> & {
  readonly profiles: SessionProfiles;
  readonly transcript?: TranscriptRead;
};

const RootRow = z.strictObject({
  hash: Hash,
  profile: z.string().regex(/^[a-z0-9][a-z0-9._-]{0,63}$/),
});

const TranscriptStore = z
  .string()
  .refine(
    (store) =>
      store.startsWith(TRANSCRIPT_PREFIX) &&
      TranscriptHost.safeParse(store.slice(TRANSCRIPT_PREFIX.length)).success,
    { error: "a transcript store and host label" }
  );
const TranscriptTool = z.strictObject({
  row: z.literal("tool"),
  store: TranscriptStore,
  format: z.string().min(1),
  tool: z.enum(["read", "Read", "Skill"]),
  calls: Count,
});
const CoverageRow = z.strictObject({
  row: z.literal("coverage"),
  store: TranscriptStore,
  format: z.string().min(1),
  profile: RootRow.shape.profile,
  first: z.number().int(),
  last: z.number().int(),
  days: z.array(CountRow.shape.day).min(1),
  hashes: z.array(Hash).min(1),
});
const TranscriptEnd = z.strictObject({
  row: z.literal("end"),
  store: z.literal(TRANSCRIPT_SOURCE),
});

/** Only complete ClickHouse results, with the end row required even for empty days. */
function transcriptRows(text: string): unknown[] {
  const rows = text
    .trimEnd()
    .split("\n")
    .map((line, index): unknown => {
      try {
        return JSON.parse(line);
      } catch {
        throw new Error(`transcript output line ${index + 1} is not JSON`);
      }
    });
  if (!TranscriptEnd.safeParse(rows.pop()).success)
    throw new Error("no end row: the transcript query did not finish");
  return rows;
}

export interface TranscriptDay {
  readonly store: typeof TRANSCRIPT_SOURCE;
  readonly name: string;
  readonly outputs: readonly QueryOutput[];
  readonly tools: readonly z.infer<typeof TranscriptTool>[];
}

/** Parse one ClickHouse day into count.sql query outputs, one per host. */
export function parseTranscriptDay(_store: string, name: string, text: string): TranscriptDay {
  const tools: z.infer<typeof TranscriptTool>[] = [];
  const stores = new Map<string, unknown[]>();
  for (const value of transcriptRows(text)) {
    const parsed = z.object({ row: z.string(), store: TranscriptStore }).parse(value);
    if (parsed.row === "tool") tools.push(TranscriptTool.parse(value));
    else entry(stores, parsed.store, () => []).push(value);
  }
  for (const { store } of tools) {
    if (!stores.has(store)) throw new Error(`no summary for ${store}`);
  }
  return {
    store: TRANSCRIPT_SOURCE,
    name,
    outputs: [...stores].map(([store, rows]) => parseCountRows(store, name, rows)),
    tools,
  };
}

interface HostFormat {
  readonly store: string;
  readonly format: string;
  first: number;
  last: number;
  readonly days: Set<string>;
  readonly hashes: Set<string>;
  added: number;
}

interface TranscriptRead {
  readonly queries: QueryDir<TranscriptDay>;
  readonly coverage: readonly HostFormat[];
  readonly owners: ReadonlyMap<string, string>;
  readonly formats: ReadonlyMap<string, string>;
  readonly coverageRead: boolean;
  /** The SHA-256 of the bytes read of `transcripts/coverage.txt` and of each store's
   *  `<store>/hashes.txt`, by that path. */
  readonly digests: ReadonlyMap<string, string>;
}

/** Exclude Postgres-owned files before `countOnce`, so a fork can keep reads when its only
 *  competing transcript row was excluded. A file imported by several hosts has one owner. */
function readTranscriptCount(
  dir: string,
  stores: readonly string[],
  profiles: Map<string, string>
): TranscriptRead {
  const queries = readQueryDir(dir, [TRANSCRIPT_SOURCE], parseTranscriptDay);
  const coverageFile = path.join(dir, TRANSCRIPT_SOURCE, "coverage.txt");
  const coverageRead = existsSync(coverageFile);
  const coverage = new Map<string, HostFormat>();
  const formats = new Map<string, string>();
  const owners = new Map<string, string>();
  const pg = new Set<string>();
  const digests = new Map<string, string>();
  /** The text of the file `name` (a path in `dir`), the SHA-256 of its bytes kept by that path. */
  const read = (name: string) => {
    const bytes = readFileSync(path.join(dir, name));
    digests.set(name, sha256(bytes));
    return bytes.toString("utf8");
  };
  if (queries.outputs.length > 0 && !coverageRead)
    throw new Error(`${coverageFile} is missing: run count-sql --query transcript-coverage first`);
  if (coverageRead) {
    for (const store of stores) {
      const file = path.join(dir, store, "hashes.txt");
      if (!existsSync(file))
        throw new Error(`${file} is missing: run count-sql --query hashes for ${store}`);
      const schema = z.strictObject({ hash: Hash });
      for (const value of psqlRows(read(`${store}/hashes.txt`))) {
        const parsed = schema.safeParse(value);
        if (!parsed.success) throw new Error(`${file}: ${schemaFaults(parsed.error)}`);
        pg.add(parsed.data.hash);
      }
    }
    const rows = transcriptRows(read(`${TRANSCRIPT_SOURCE}/coverage.txt`)).map((value) => {
      const parsed = CoverageRow.safeParse(value);
      if (!parsed.success) throw new Error(`${coverageFile}: ${schemaFaults(parsed.error)}`);
      if (parsed.data.first > parsed.data.last) throw new Error(`${coverageFile}: reversed span`);
      return parsed.data;
    });
    rows.sort((a, b) => (a.store < b.store ? -1 : Number(a.store > b.store)));
    for (const row of rows) {
      const item = entry(coverage, sessionKey(row.store, row.format), () => ({
        store: row.store,
        format: row.format,
        first: row.first,
        last: row.last,
        days: new Set<string>(),
        hashes: new Set<string>(),
        added: 0,
      }));
      item.first = Math.min(item.first, row.first);
      item.last = Math.max(item.last, row.last);
      for (const day of row.days) item.days.add(day);
      for (const hash of row.hashes) {
        item.hashes.add(hash);
        if (pg.has(hash) || owners.has(hash)) continue;
        owners.set(hash, row.store);
        const key = sessionKey(row.store, hash);
        formats.set(key, row.format);
        profiles.set(key, row.profile);
        item.added++;
      }
    }
  }
  return { queries, coverage: [...coverage.values()], owners, formats, coverageRead, digests };
}

function transcriptOutputs(transcript: TranscriptRead): QueryOutput[] {
  return transcript.queries.outputs.flatMap((day) =>
    day.outputs.map((output) => ({
      ...output,
      rows: output.rows.map((row) => {
        const hashes = row.hashes.filter((hash) => transcript.owners.get(hash) === row.store);
        const unpaired = row.unpaired_hashes.filter(
          (hash) => transcript.owners.get(hash) === row.store
        );
        return {
          ...row,
          hashes,
          files: hashes.length,
          calls: Object.fromEntries(hashes.map((hash) => [hash, row.calls[hash] ?? []])),
          unpaired_hashes: unpaired,
          unpaired: unpaired.length,
          unpaired_calls: Object.fromEntries(
            unpaired.map((hash) => [hash, row.unpaired_calls[hash] ?? []])
          ),
        };
      }),
    }))
  );
}

/** Reads the query directory `dir` for the stores `stores`, each `.txt` read once and parsed by
 *  `parse`. A `.txt` that `parse` throws on is listed as unreadable, but for an EarlierCount,
 *  which ends the read. */
export function readQueryDir<O>(
  dir: string,
  stores: readonly string[],
  parse: (store: string, name: string, text: string) => O
): QueryDir<O> {
  if (!existsSync(dir)) throw new Error(`the query directory ${dir} does not exist`);
  const outputs: O[] = [];
  const unreadable: { store: string; name: string; why: string }[] = [];
  const digests = new Map<string, string>();
  for (const store of stores) {
    const storeDir = path.join(dir, store);
    if (!existsSync(storeDir)) continue;
    for (const file of readdirSync(storeDir).sort()) {
      if (!QUERY_FILE.test(file)) continue;
      const name = file.slice(0, -".txt".length);
      try {
        const bytes = readFileSync(path.join(storeDir, file));
        outputs.push(parse(store, name, bytes.toString("utf8")));
        digests.set(`${store}/${file}`, sha256(bytes));
      } catch (error) {
        if (error instanceof EarlierCount) throw error;
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
  return { outputs, unreadable, log, otherStores, digests };
}

/** Reads the count directory `dir` for the stores `stores`. With the configuration's
 *  `transcriptStore`, it reads the transcript store too (`readTranscriptCount`): its outputs
 *  follow the stores' (one per host and query, store `transcripts@<host>`, without a file that a
 *  store's hashes.txt or a host label before its own holds), and its `digests` also hold those of
 *  the transcript store's query files, of `transcripts/coverage.txt` and of each store's
 *  `hashes.txt`. */
export function readCount(
  dir: string,
  stores: readonly string[],
  config?: SkillCutsConfig
): CountRead {
  const count = readQueryDir(dir, stores, parseQueryOutput);
  const profiles = new Map<string, string>();
  for (const store of new Set(count.outputs.map((output) => output.store))) {
    const file = path.join(dir, store, "roots.txt");
    if (!existsSync(file))
      throw new Error(
        `${file} does not exist: run count-sql --query roots --config <file> --store ${store} and save the store's query output there`
      );
    for (const value of psqlRows(readFileSync(file, "utf8"))) {
      const parsed = RootRow.safeParse(value);
      if (!parsed.success)
        throw new Error(`${file}: not a roots.sql row: ${schemaFaults(parsed.error)}`);
      const { hash, profile } = parsed.data;
      const key = sessionKey(store, hash);
      if (profiles.has(key)) throw new Error(`${file}: duplicate file hash ${hash}`);
      profiles.set(key, profile);
    }
  }
  if (config?.transcriptStore !== undefined) {
    const transcript = readTranscriptCount(dir, stores, profiles);
    return {
      ...count,
      outputs: [...count.outputs, ...transcriptOutputs(transcript)],
      profiles,
      digests: new Map([...count.digests, ...transcript.queries.digests, ...transcript.digests]),
      transcript,
      otherStores: count.otherStores.filter((store) => store !== TRANSCRIPT_SOURCE),
    };
  }
  return { ...count, profiles };
}

/** A file that lists a key in a count row: its hash, and the name of its query. */
interface Holder {
  readonly hash: string;
  readonly name: string;
}

/** What `countOnce` finds in the files of two queries of one store. */
export interface AcrossQueries {
  /** The keys that files of two queries hold. */
  readonly keys: number;
  /** The pairs of files of two queries that hold one key: a fork written on another mtime day
   *  than its parent, with its parent, among them. */
  readonly pairs: number;
}

/** The count's rows with each read counted once across queries, and per store what files of two
 *  queries hold. */
export interface CountedOnce {
  readonly outputs: readonly QueryOutput[];
  readonly acrossQueries: ReadonlyMap<string, AcrossQueries>;
}

/**
 * count.sql's rule for a read that two files hold, applied to every query of each store: a key
 * that several files hold counts in the file whose hash sorts first (in byte order), and a file
 * stays in a row's `hashes` (or `unpaired_hashes`) when one of its keys there counts in it;
 * `files` and `unpaired` follow. Each query applied the rule to its own files, so a query lists
 * a key under one file; a fork written on another mtime day than its parent lists the parent's
 * key in another query. Each row keeps its `calls`, so the rule applied again gives the same
 * rows. A row whose files all keep their place, and an output with no such row changed, are
 * returned as they are.
 */
export function countOnce(outputs: readonly QueryOutput[]): CountedOnce {
  // Host labels separate placement contexts, not the ownership of a shared read key.
  const source = (store: string) =>
    store.startsWith(TRANSCRIPT_PREFIX) ? TRANSCRIPT_SOURCE : store;
  // Per store: each key's first holder, and every holder of a key that more than one holds.
  const stores = new Map<string, { first: Map<string, Holder>; more: Map<string, Holder[]> }>();
  for (const output of outputs) {
    const store = entry(stores, source(output.store), () => ({
      first: new Map<string, Holder>(),
      more: new Map<string, Holder[]>(),
    }));
    for (const row of output.rows) {
      for (const calls of [row.calls, row.unpaired_calls]) {
        for (const [hash, keys] of Object.entries(calls)) {
          for (const key of keys) {
            const first = store.first.get(key);
            if (first === undefined) {
              store.first.set(key, { hash, name: output.name });
              continue;
            }
            if (first.hash === hash && first.name === output.name) continue;
            const held = entry(store.more, key, () => [first]);
            if (!held.some((holder) => holder.hash === hash && holder.name === output.name))
              held.push({ hash, name: output.name });
          }
        }
      }
    }
  }
  // Per store: the file that each key two files hold counts in, and what files of two queries
  // hold.
  const owners = new Map<string, Map<string, string>>();
  const acrossQueries = new Map<string, AcrossQueries>();
  for (const [store, { more }] of stores) {
    const owner = new Map<string, string>();
    const pairs = new Set<string>();
    let keys = 0;
    for (const [key, held] of more) {
      const files = [...new Set(held.map((holder) => holder.hash))].sort();
      const [lowest] = files;
      if (lowest === undefined || files.length < 2) continue;
      owner.set(key, lowest);
      let across = false;
      for (const a of held) {
        for (const b of held) {
          if (a.hash >= b.hash || a.name === b.name) continue;
          across = true;
          pairs.add(`${a.hash}\t${b.hash}`);
        }
      }
      if (across) keys++;
    }
    owners.set(store, owner);
    acrossQueries.set(store, { keys, pairs: pairs.size });
  }
  const counted = outputs.map((output) => {
    const owner = owners.get(source(output.store));
    if (owner === undefined || owner.size === 0) return output;
    /** The files of `hashes` one of whose keys in `calls` counts in them. */
    const kept = (calls: CountRow["calls"], hashes: readonly string[]) =>
      hashes.filter((hash) => (calls[hash] ?? []).some((key) => (owner.get(key) ?? hash) === hash));
    let changed = false;
    const rows = output.rows.map((row) => {
      const hashes = kept(row.calls, row.hashes);
      const unpaired = kept(row.unpaired_calls, row.unpaired_hashes);
      if (hashes.length === row.hashes.length && unpaired.length === row.unpaired_hashes.length)
        return row;
      changed = true;
      return {
        ...row,
        files: hashes.length,
        hashes,
        unpaired: unpaired.length,
        unpaired_hashes: unpaired,
      };
    });
    return changed ? { ...output, rows } : output;
  });
  return { outputs: counted, acrossQueries };
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
export const HOURS = Array.from({ length: 24 }, (_, hour) => hour);
export const twoDigits = (value: number) => String(value).padStart(2, "0");

/** The UTC days (YYYY-MM-DD) from the day of `first` to the day of `last`, both included. */
export function daysFrom(first: number, last: number): string[] {
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
  count: QueryDir<{ readonly store: string; readonly name: string }>,
  transcript?: QueryDir<TranscriptDay>
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
  const stores = config.stores.map(({ name }) => name);
  if (transcript !== undefined) stores.push(TRANSCRIPT_SOURCE);
  for (const store of stores) {
    const input = store === TRANSCRIPT_SOURCE && transcript !== undefined ? transcript : count;
    const days =
      input === transcript ? daysFrom(Date.parse(config.windowStart), windowEnd - 1) : expected;
    const names = new Set(
      input.outputs.filter((output) => output.store === store).map(({ name }) => name)
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
    for (const unreadable of input.unreadable.filter((item) => item.store === store)) {
      add(unreadable.name, "unreadable", `its output is not a whole result: ${unreadable.why}`);
    }
    for (const day of days) {
      if (names.has(day) || listed.has(day)) continue;
      const hours = hoursOf(day);
      if (input !== transcript && hours.length > 0) continue;
      if (hours.length === 24) continue;
      if (hours.length > 0) {
        add(day, "split", `${hours.length} of 24 hour queries have output`);
        continue;
      }
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
  const values = psqlRows(text);
  const { rows, summary } = queryRows(store, values, ModelQueryRow, "models.sql", (row, at) => {
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
export type FormerStagesRow = Columns<typeof FORMER_STAGES_COLUMNS>;
export type InstallersRow = Columns<typeof INSTALLERS_COLUMNS>;
export type LoadersRow = Columns<typeof LOADERS_COLUMNS>;
export type RepositoriesRow = Columns<typeof REPOSITORIES_COLUMNS>;
export type ManifestsRow = Columns<typeof MANIFESTS_COLUMNS>;

/** The tables of the copies output (copies.ts) this file reads. `manifests` (manifests.tsv) is
 *  what the dev box's own repository is found from (`devboxRepositories`); without it no dev box
 *  is known. */
export interface R4Tables {
  readonly copies: readonly SkillCopiesRow[];
  readonly prefixes: readonly PrefixTableRow[];
  readonly formerStages: readonly FormerStagesRow[];
  readonly installers: readonly InstallersRow[];
  readonly loaders: readonly LoadersRow[];
  readonly repositories: readonly RepositoriesRow[];
  readonly manifests?: readonly ManifestsRow[];
}

/** The copies output as `readR4` reads it: the tables the attribution reads, and manifests.tsv,
 *  which build.ts's profiles read too. */
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

/** The copies output in its directory `dir`. Throws, naming the file, on an output made before
 *  copies.ts wrote former-stages.tsv: its reads would be placed without the rows of a skill git
 *  renamed, so that copies output must be made again. */
export function readR4(dir: string): R4Output {
  const formerStages = path.join(dir, "former-stages.tsv");
  if (!existsSync(formerStages)) {
    throw new Error(
      `${formerStages} does not exist: the copies output was made before copies.ts wrote former-stages.tsv, so run copies again`
    );
  }
  return {
    copies: readTable(dir, "skill-copies.tsv", SKILL_COPIES_COLUMNS),
    prefixes: readTable(dir, "prefix-table.tsv", PREFIX_TABLE_COLUMNS),
    formerStages: readTable(dir, "former-stages.tsv", FORMER_STAGES_COLUMNS),
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

/** The installers.tsv actions by which a unit's own manifest ships a skill (INSTALL_ACTIONS but
 *  `enables`, which turns on a plugin another manifest ships). */
export const MEMBER_ACTIONS: Readonly<Record<string, true>> = {
  ships: true,
  stages: true,
  links: true,
  installs: true,
};

/** Claude Code's settings file in a repository, which with an installer script marks the dev
 *  box's own repository. */
export const CLAUDE_SETTINGS = ".claude/settings.json";

/** The repositories of manifests.tsv that hold both an installer script (`installers/*.sh`) and
 *  Claude Code's settings (`.claude/settings.json`), sorted: the dev box's own repository is the
 *  one of them when there is one alone (DEVBOX_INSTALLERS_RULE in build.ts). */
export function devboxRepositories(
  manifests: readonly Pick<ManifestsRow, "repository" | "path" | "kind">[]
): string[] {
  const holding = (kind: string, file?: string) =>
    new Set(
      manifests
        .filter((row) => row.kind === kind && (file === undefined || row.path === file))
        .map((row) => row.repository)
    );
  const settings = holding("claude-settings", CLAUDE_SETTINGS);
  return [...holding("installer-script")].filter((repo) => settings.has(repo)).sort();
}

/** The skills directories in which Oh My Pi 18.18.6 lists the skills of a project (a checkout's
 *  own, build.ts's PROJECT_SKILLS with the source of each): the ones a home directory holds are a
 *  user's, which a plugin or an installer may fill (`~/.claude/skills`, `~/.agents/skills`). */
export const PROJECT_SKILL_DIRS = [
  ".omp/skills",
  ".claude/skills",
  ".agent/skills",
  ".agents/skills",
  ".codex/skills",
  ".opencode/skills",
  ".github/skills",
] as const;

/** A home directory: where a user's own skills directories sit (`/home/<user>`, `/root`,
 *  `/Users/<user>`, `~`). */
const HOME_DIRECTORY = /^(?:~|\/root|\/home\/[^/]+|\/Users\/[^/]+)$/;

/**
 * The project skills directory (`PROJECT_SKILL_DIRS`) that the copy-path prefix `prefix`
 * (`copyPrefix`, ending with the skill's own directory) is directly inside, when the directory
 * above it is no home directory: the read is of a project's own skill, in a checkout (an absolute
 * path, or one relative to the session's working directory). Undefined for a user's skills
 * directory, a path in none, and a `skill://` prefix.
 */
export function projectSkillDir(prefix: string): string | undefined {
  if (prefix.startsWith("skill://")) return undefined;
  const skills = path.posix.dirname(prefix.replace(/\/+$/, ""));
  for (const dir of PROJECT_SKILL_DIRS) {
    if (skills !== dir && !skills.endsWith(`/${dir}`)) continue;
    const above = skills.slice(0, skills.length - dir.length).replace(/\/+$/, "");
    return HOME_DIRECTORY.test(above) ? undefined : dir;
  }
  return undefined;
}

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
  "former_name_sessions",
  "reference_sessions",
  "unattributed_sessions",
  "tie",
  "loaded",
  "status",
  "transcript_store_sessions",
  "partial_coverage_hosts",
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
  readonly transcriptSessions: number;
  /** Of `transcriptSessions`, each transcript host's, most first, a tie by host. */
  readonly transcriptHosts: readonly { readonly host: string; readonly sessions: number }[];
  readonly partialCoverageHosts: readonly string[];
  /** The read sessions (`sessionKey`) that `sessions` counts. */
  readonly readSessions: ReadonlySet<string>;
  /** Of `sessions`, those that read it under a former name (`Placement.formerName`). */
  readonly formerNameSessions: number;
  /** The former names it was read under, sorted. */
  readonly formerNames: readonly string[];
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
  /** The call keys that two files of one query hold: its queries' `shared_call_ids`, summed. */
  readonly sharedCallIds: number;
  /** The call keys that files of two of its queries hold (`countOnce`). */
  readonly sharedAcrossQueries: number;
  /** The pairs of files of two of its queries that hold one call key (`countOnce`). */
  readonly filePairsAcrossQueries: number;
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

/** One host and format of the transcript store's coverage (`transcriptCoverage`). */
export interface TranscriptCoverage {
  readonly store: string;
  readonly format: string;
  readonly first: number;
  readonly last: number;
  readonly gaps: readonly string[];
  readonly sessions: number;
  readonly added: number;
  readonly reads: number;
}

export interface Attribution {
  readonly copies: readonly CopyCount[];
  readonly unmapped: readonly UnmappedPrefix[];
  /** Each prefix the table maps, with the copies (`<repository> <path>`) it maps to. */
  readonly mappedPrefixes: ReadonlyMap<string, ReadonlySet<string>>;
  readonly stores: readonly StoreCount[];
  readonly transcriptCoverage: readonly TranscriptCoverage[];
  readonly claudeCounted: boolean;
  readonly namesWithoutCopy: readonly NameWithoutCopy[];
  /** The distinct (store, file) sessions with a read. */
  readonly readSessions: number;
  /** The distinct read sessions some row of which went to no copy. */
  readonly unattributedSessions: number;
}

/** What the prefix table says of a copy path: the one copy its matching rows map it to, with
 *  their units and, when each of them is a row of a skill git renamed (former-stages.tsv), the
 *  name it had there; or the copies they map it to and the matched former-name rows. */
type TableDecision =
  | {
      readonly copy: string;
      readonly units: readonly string[];
      readonly formerName: string | undefined;
    }
  | {
      readonly copy: undefined;
      readonly copies: readonly string[];
      readonly formerRows: readonly PrefixMatcher[];
    };

/** A prefix-table.tsv row as the attribution reads it: its pattern, the last directory name of its
 *  prefix, its copy, the units of its `via`, and the name the copy had at its prefix when a
 *  former-stages.tsv row of a skill git renamed gives the row (its `skill`), else undefined. */
interface PrefixMatcher {
  readonly pattern: RegExp;
  readonly last: string;
  readonly copy: string;
  readonly units: readonly string[];
  readonly formerName: string | undefined;
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
        ? {
            copy: only,
            units: [...new Set(matched.flatMap((row) => row.units))],
            formerName: matched.every((row) => row.formerName !== undefined)
              ? matched[0]?.formerName
              : undefined,
          }
        : {
            copy: undefined,
            copies: found,
            formerRows: matched.filter((row) => row.formerName !== undefined),
          };
    decisions.set(copyPath, decision);
    return decision;
  };
}

/** What the checkout rule says of a copy-path prefix: the copy it places the read at, or
 *  undefined with the copies it cannot tell apart, and why, in the report's words. `tied`: the
 *  copies (`<repository>\t<skill directory>`) of a tie, the repositories whose skill directory
 *  ends the path that no directory of it tells apart, among which the store's context rule
 *  decides. `toContext`: the path is inside a clone an installer makes and names none of the
 *  repositories whose skill directory ends it, so the rule places nothing and the store's context
 *  rule decides among every copy. */
export interface CheckoutDecision {
  readonly copy: string | undefined;
  readonly why: string;
  readonly tied?: readonly string[];
  readonly toContext?: true;
}

/** A copy of another repository that an installer writes, in a path's segments (`cloneAt`). */
interface InstalledCopy {
  readonly start: number;
  readonly own: number;
  readonly kind: "clone" | "package" | "plugin";
}

/**
 * Where, in the segments `segments` of a path, the copy of another repository that an installer
 * writes starts, of those that start before the segment `end`, after the last of these
 * directories (the nearest to `end`): `vendor/<name>`, an `ensure_vendor` clone (copies.ts,
 * `parseInstaller`: a clone of a repository at `vendor/<name>` of the installer's repository,
 * `kind` "clone"); `node_modules/<name>` and `node_modules/@<scope>/<name>`, a package a plugins
 * package.json installs (copies.ts's `**\/node_modules/<name>/` locations, "package"); and
 * `plugins/cache/<dir>`, a plugin a marketplace installs (`**\/plugins/cache/…`, "plugin").
 * `start` is the copy's first segment, `own` the last segment of its own directory (`<name>`,
 * `@<scope>/<name>`, `<dir>`). Undefined when none starts before `end`.
 */
function cloneAt(segments: readonly string[], end: number): InstalledCopy | undefined {
  let found: InstalledCopy | undefined;
  for (let at = 0; at + 1 < end; at++) {
    const segment = segments[at];
    if (segment === "vendor") found = { start: at + 1, own: at + 1, kind: "clone" };
    else if (segment === "node_modules") {
      const scoped = segments[at + 1]?.startsWith("@") === true && at + 2 < end;
      found = { start: at + 1, own: scoped ? at + 2 : at + 1, kind: "package" };
    } else if (segment === "plugins" && segments[at + 1] === "cache" && at + 2 < end) {
      found = { start: at + 2, own: at + 2, kind: "plugin" };
    }
  }
  return found;
}

/** The directory of the `ensure_vendor` clone (`cloneAt`, kind "clone") that the copy-path
 *  prefix `prefix` (`copyPrefix`, ending with the skill's own directory) is inside, or undefined
 *  when it is inside none. */
function cloneDirectory(prefix: string): string | undefined {
  const segments = prefix.replace(/\/+$/, "").split("/");
  const clone = cloneAt(segments, segments.length - 1);
  return clone?.kind === "clone" ? segments.slice(0, clone.own + 1).join("/") : undefined;
}

/**
 * The checkout rule, for the copies `copies`: a read whose copy-path prefix (`copyPrefix`) ends
 * with a copy's repository-relative skill directory, at a path boundary, is a read of that copy in
 * a checkout of its repository (`/home/x/.dotfiles/plugins/p/skills/<name>/`, a workspace's
 * `.../<issue>/.claude/skills/<name>/`, a relative `skills/<name>/`). The longest such directory
 * wins. The path before that directory names a repository, from the most to the least: two of
 * its segments in a row are the repository's owner and its name; a segment is its name; two of
 * its segments in a row are its owner and a segment that holds its name; a segment holds its name
 * (`.dotfiles` holds `dotfiles`). Copies of several repositories at that directory are told apart
 * by it: the one the path names most; any other tie places nothing, and the context rule decides
 * among the tied copies (`tied`). A skill directory of one segment (a skill at its repository's
 * root) ends every path of a skill of that name, so it matches only a path that names its
 * repository. A path inside a copy of another repository that an installer writes (`cloneAt`:
 * `<dir>/vendor/<name>/`, a package under `node_modules/`, a plugin under `plugins/cache/`) is no
 * checkout of a repository that the path from that copy's own directory on does not name: such a
 * directory matches only a repository named there, and when none is, the rule places nothing and
 * the context rule decides (`toContext`). Undefined when no copy's directory ends the prefix, and
 * for a `skill://` prefix, which names no directory.
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
      const clone = cloneAt(segments, start);
      // Inside a clone, only the clone's own directories name a repository.
      const before = segments.slice(clone?.start ?? 0, start);
      const inside = clone === undefined ? "" : segments.slice(0, clone.own + 1).join("/");
      /** How the path names the repository, the most first: 4 its owner and its name in a row, 3
       *  its name, 2 its owner and a segment that holds its name in a row, 1 a segment that holds
       *  its name, else 0. */
      const named = (repository: string) => {
        const [owner = "", name = ""] = repository.split("/");
        const ownerThen = (match: (segment: string) => boolean) =>
          before.some((segment, at) => segment === owner && match(before[at + 1] ?? ""));
        if (ownerThen((segment) => segment === name)) return 4;
        if (before.includes(name)) return 3;
        if (ownerThen((segment) => segment.includes(name))) return 2;
        return before.some((segment) => segment.includes(name)) ? 1 : 0;
      };
      const oneSegment = start === segments.length - 1;
      const ending = byDir.get(dir) ?? [];
      const repositories = ending.filter(
        (repository) => (!oneSegment && clone === undefined) || named(repository) > 0
      );
      if (repositories.length === 0) {
        if (clone !== undefined && ending.length > 0) {
          decision = {
            copy: undefined,
            why: `the path ends with ${dir}/, the skill directory of ${ending.join(", ")}, inside ${inside}/, a copy of another repository that an installer writes, which names none of them`,
            toContext: true,
          };
          break;
        }
        continue;
      }
      const best = Math.max(...repositories.map(named));
      const chosen = repositories.filter((repository) => named(repository) === best);
      const [only] = chosen;
      const ends = `the path ends with ${dir}/`;
      const naming = (repository: string) =>
        `the path names ${best === 4 || best === 2 ? repository : `${repository.split("/")[1]} in a directory`}${clone === undefined ? "" : ` of the copy an installer writes at ${inside}/`}`;
      if (repositories.length === 1 && only !== undefined) {
        decision = {
          copy: copyKey(only, dir),
          why: `to ${only} ${dir}: ${ends}, its skill directory${oneSegment || clone !== undefined ? `, and ${naming(only)}` : ""}`,
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
          tied: chosen.map((repository) => copyKey(repository, dir)),
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
 *  `unattributed: …`).
 *  `formerName`, for a read placed under a name its copy had before git renamed it: that name,
 *  when every prefix row that maps its path is a former-stages.tsv row of the rename (step 1), or
 *  when its context places it through a matching former-name row (step 3); else undefined. */
export interface Placement {
  readonly prefix: string;
  readonly copy: string | undefined;
  readonly rule: PlacementRule | undefined;
  readonly outcome: string | undefined;
  readonly formerName: string | undefined;
}

/** The placing of reads by the rules of this file's header, for one count and one copies output. */
export interface Placer {
  /** Where the read goes, using the session's profile if the path does not decide. */
  place(store: string, copyPath: string, skill: string, profile: string): Placement;
  /** For each store, each unit its prefix-table reads come from, with their sessions (file
   *  hashes): the units step 3 infers from reads. Step 3's group of a devbox store's default
   *  profile also holds the units the dev box's own installers install (DEVBOX_INSTALLERS_RULE),
   *  which no read need come from, so this does not list them. */
  readonly storeUnits: ReadonlyMap<string, ReadonlyMap<string, ReadonlySet<string>>>;
  /** Each unit's copies: its installer rows, and the copies its prefix rows map to. */
  readonly members: ReadonlyMap<string, ReadonlySet<string>>;
  /** The copies some store's context installs. */
  readonly installedInContext: ReadonlySet<string>;
  /** The count's rows with each read counted once across queries (`countOnce`): the rows this
   *  places reads by, and `attribute` counts. */
  readonly counted: CountedOnce;
}

/** The one place a read's copy is decided (the header's steps 1 to 4), on the rows of `outputs`
 *  with each read counted once across queries (`countOnce`): `attribute` counts each row where
 *  this puts it, and build.ts keeps a session only when this puts its read at the cut's copy.
 *  Throws when a prefix-table.tsv row, or an installers.tsv row that installs a copy, names a copy
 *  that skill-copies.tsv does not hold. */
export function placer(
  outputs: readonly QueryOutput[],
  r4: R4Tables,
  profiles: SessionProfiles,
  devboxStores: ReadonlySet<string> = new Set(),
  pathOnlyStores: ReadonlySet<string> = new Set()
): Placer {
  const counted = countOnce(outputs);
  const copies = new Map<string, SkillCopiesRow>();
  for (const row of r4.copies) copies.set(copyKey(row.repository, row.path), row);
  const known = (key: string, where: string) => {
    if (!copies.has(key))
      throw new Error(`${where} names ${shown(key)}, which skill-copies.tsv does not hold`);
    return key;
  };

  // The former-stages.tsv rows of a skill git renamed, by prefix and copy, each with the name the
  // prepack script staged it under; and the rows no prefix row maps, with their patterns.
  const renamed = new Map<string, string>();
  const unmappedStages: { pattern: RegExp; row: FormerStagesRow }[] = [];
  for (const row of r4.formerStages) {
    if (row.prefix === "") continue;
    if (row.repository === "") unmappedStages.push({ pattern: prefixRegExp(row.prefix), row });
    else if (row.renamed_to !== "")
      renamed.set(`${row.prefix}\t${copyKey(row.repository, row.path)}`, row.skill);
  }

  // The copies of each unit: its installer rows and the copies its prefix rows map to.
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
      const formerName = renamed.get(`${row.prefix}\t${key}`);
      for (const unit of units) entry(members, unit, () => new Set<string>()).add(key);
      const parts = row.prefix.split("/").filter((part) => part !== "");
      return {
        pattern: prefixRegExp(row.prefix),
        last: parts[parts.length - 1] ?? "",
        copy: key,
        units,
        formerName,
      };
    })
  );
  const checkout = checkoutRule(r4.copies);
  // A project skill is of the checkout it is read in, so only a copy in a project skills directory
  // can be it (a read of `.claude/skills/x` is no read of another repository's `skills/x`).
  const checkoutProjects = checkoutRule(
    r4.copies.filter((row) => projectSkillDir(row.path) !== undefined)
  );

  // Each store's units its own mapped reads come from, with their sessions, and step 3's groups:
  // those units by store and profile, to which the dev box's installers' units are added below.
  const storeUnits = new Map<string, Map<string, Set<string>>>();
  const profileUnits = new Map<string, Set<string>>();
  for (const output of counted.outputs) {
    for (const row of output.rows) {
      if (row.hashes.length === 0) continue;
      const decision = decide(row.copy);
      if (decision.copy === undefined) continue;
      const units = entry(storeUnits, row.store, () => new Map<string, Set<string>>());
      for (const unit of decision.units) {
        const sessions = entry(units, unit, () => new Set<string>());
        for (const hash of row.hashes) {
          sessions.add(hash);
          const profile = profiles.get(sessionKey(row.store, hash));
          if (profile !== undefined && profile !== "unknown" && !pathOnlyStores.has(row.store))
            entry(profileUnits, sessionKey(row.store, profile), () => new Set<string>()).add(unit);
        }
      }
    }
  }
  // The dev box's own installers (DEVBOX_INSTALLERS_RULE): a default-profile session of a devbox
  // store has each unit they install into Oh My Pi, whether or not it read from it.
  const [home, ...otherHomes] = devboxRepositories(r4.manifests ?? []);
  if (home !== undefined && otherHomes.length === 0) {
    const installed = new Set<string>();
    for (const row of r4.installers) {
      if (
        row.manifest_repository === home &&
        row.host === "omp" &&
        Object.hasOwn(MEMBER_ACTIONS, row.action) &&
        row.copy_path !== ""
      )
        installed.add(unitKey(row.unit, row.manifest_repository, row.manifest_path));
    }
    for (const store of devboxStores) {
      const units = entry(profileUnits, sessionKey(store, "default"), () => new Set<string>());
      for (const unit of installed) units.add(unit);
    }
  }
  const contextCopies = new Map<string, Map<string, Set<string>>>();
  const installedInContext = new Set<string>();
  for (const [group, units] of profileUnits) {
    const byName = new Map<string, Set<string>>();
    for (const unit of units) {
      for (const key of members.get(unit) ?? []) {
        entry(byName, copies.get(key)?.name ?? "", () => new Set<string>()).add(key);
        installedInContext.add(key);
      }
    }
    contextCopies.set(group, byName);
  }

  /** What a former-stages.tsv row that no prefix row maps says of a path it names: a prepack
   *  script staged the skill there, or a marketplace named a plugin as holding it there. */
  const formerSaid = (row: FormerStagesRow) =>
    row.package.startsWith("plugin:")
      ? `${row.script_repository}'s ${row.script_path} named ${row.package} as holding ${row.skill} here until ${row.until.slice(0, 8)}, and holds it there no longer; ${row.note}`
      : `${row.script_repository}'s ${row.script_path} staged ${row.skill} for ${row.package} here until ${row.until.slice(0, 8)}, and stages it there no longer; ${row.note}`;

  const place = (store: string, copyPath: string, skill: string, profile: string): Placement => {
    const prefix = copyPrefix(copyPath, skill);
    const decision = decide(copyPath);
    if (decision.copy !== undefined) {
      return {
        prefix,
        copy: decision.copy,
        rule: "prefix table",
        outcome: undefined,
        formerName: decision.formerName,
      };
    }
    const prefixTie =
      decision.copies.length > 1
        ? `prefix rows match ${decision.copies.length} copies (${decision.copies.map(shown).join(", ")})`
        : "";
    if (copyPath.startsWith(IMAGE_ROOT)) {
      const reasons: string[] = [];
      if (prefixTie !== "") reasons.push(prefixTie);
      for (const { pattern, row } of unmappedStages) {
        if (pattern.test(copyPath)) reasons.push(formerSaid(row));
      }
      if (reasons.length === 0) reasons.push("no prefix row maps the path");
      return {
        prefix,
        copy: undefined,
        rule: undefined,
        outcome: `unattributed: ${reasons.join("; ")}; only the prefix table places reads under the worker image's install location ${IMAGE_ROOT}${pathOnlyStores.has(store) ? "; the host's context is not known" : ""}`,
        formerName: undefined,
      };
    }
    const table = prefixTie === "" ? "" : `${prefixTie}, and `;
    const project = projectSkillDir(prefix);
    const byCheckout = (project === undefined ? checkout : checkoutProjects)(prefix);
    if (byCheckout?.copy !== undefined) {
      return {
        prefix,
        copy: byCheckout.copy,
        rule: "checkout",
        outcome: `checkout: ${byCheckout.why}`,
        formerName: undefined,
      };
    }
    if (pathOnlyStores.has(store)) {
      return {
        prefix,
        copy: undefined,
        rule: undefined,
        outcome: `unattributed: ${table}${byCheckout?.why ?? "no copy's skill directory ends the path"}; the host's context is not known`,
        formerName: undefined,
      };
    }
    // A read inside an `ensure_vendor` clone that no prefix row maps is of the clone, which the
    // copies output does not hold at its end time; the copies the context installs then are
    // others, so the context rule claims none of them for it.
    const clone = cloneDirectory(prefix);
    if (clone !== undefined) {
      return {
        prefix,
        copy: undefined,
        rule: undefined,
        outcome: `unattributed: ${table}${byCheckout?.why ?? `no copy's skill directory ends the path, which is inside ${clone}/, a clone an installer makes`}; no prefix row maps it, and the context rule claims no copy for a read inside a clone`,
        formerName: undefined,
      };
    }
    // A project's own skill is placed by a copy in a project skills directory alone (the checkout
    // rule above, or the context among such copies a checkout tie leaves): never by a unit the
    // profile installs, a plugin that holds a skill of the same name least of all.
    if (project !== undefined && byCheckout?.tied === undefined) {
      return {
        prefix,
        copy: undefined,
        rule: undefined,
        outcome: `unattributed: ${table}${byCheckout?.why ?? "no copy in a project skills directory ends the path"}; the path is of a project skill (${project} of a checkout, not of a home directory), which only a copy in such a directory places, so no unit's copy of ${skill} is it`,
        formerName: undefined,
      };
    }
    const group = sessionKey(store, profile);
    const context = [...(contextCopies.get(group)?.get(skill) ?? [])];
    const units = profileUnits.get(group);
    const formerRows = decision.formerRows.filter(
      (row) => row.formerName === skill && row.units.some((unit) => units?.has(unit))
    );
    for (const row of formerRows) if (!context.includes(row.copy)) context.push(row.copy);
    // After a checkout tie, the path already says the read is of one of the tied copies, so the
    // context decides among them alone.
    const tied = byCheckout?.tied;
    const why = `${table}${byCheckout?.why ?? "no copy's skill directory ends the path"}`;
    const installed = tied === undefined ? context : context.filter((copy) => tied.includes(copy));
    const [only] = installed;
    if (installed.length === 1 && only !== undefined) {
      const formerName = formerRows.find((row) => row.copy === only)?.formerName;
      return {
        prefix,
        copy: only,
        rule: "context",
        outcome:
          tied === undefined
            ? `context: to ${shown(only)}, the one copy of ${skill} its context installs${formerName === undefined ? "" : `, ${skill} being its former name (former-stages.tsv)`}`
            : `context: to ${shown(only)}: ${why}, and its context installs that one of them`,
        formerName,
      };
    }
    const contextSaid =
      tied === undefined
        ? context.length === 0
          ? `no copy of ${skill}`
          : `${context.length} copies of ${skill} (${context.map(shown).join(", ")})`
        : installed.length === 0
          ? "none of them"
          : `${installed.length} of them (${installed.map(shown).join(", ")})`;
    const formerNote = unmappedStages
      .filter(({ pattern }) => pattern.test(copyPath))
      .map(({ row }) => `; ${formerSaid(row)}`)
      .join("");
    return {
      prefix,
      copy: undefined,
      rule: undefined,
      outcome:
        profile === "unknown"
          ? `unattributed: ${why}; the session's Oh My Pi profile is unknown${formerNote}`
          : `unattributed: ${why}, and its context installs ${contextSaid}${formerNote}`,
      formerName: undefined,
    };
  };
  return { place, storeUnits, members, installedInContext, counted };
}

/** Each host and format of `transcript`'s coverage, in its order: its first and last record (each
 *  clipped to the window), its gap days (a UTC day from its first record's to its last's that no
 *  session of it touched), its sessions in the window, those the count added (`added`: no store's
 *  hashes.txt and no host label before its own holds them), and those of them with a row of a
 *  skill in `outputs` (`reads`: a read or a reference read, never an unpaired call). `outputs` are
 *  `countOnce`'s, so a read two files hold counts in one of them. */
export function transcriptCoverage(
  transcript: TranscriptRead,
  outputs: readonly QueryOutput[]
): TranscriptCoverage[] {
  const read = new Set<string>();
  for (const output of outputs) {
    if (!output.store.startsWith(TRANSCRIPT_PREFIX)) continue;
    for (const row of output.rows) {
      for (const hash of row.hashes) read.add(sessionKey(row.store, hash));
    }
  }
  const reads = new Map<string, number>();
  for (const session of read) {
    const format = transcript.formats.get(session);
    if (format === undefined) continue;
    const key = sessionKey(keyStore(session), format);
    reads.set(key, (reads.get(key) ?? 0) + 1);
  }
  return transcript.coverage.map((item) => ({
    store: item.store,
    format: item.format,
    first: item.first,
    last: item.last,
    gaps: daysFrom(item.first, item.last).filter((day) => !item.days.has(day)),
    sessions: item.hashes.size,
    added: item.added,
    reads: reads.get(sessionKey(item.store, item.format)) ?? 0,
  }));
}

/** The attribution of the count's rows `outputs` to the copies output's copies, by the rules of
 *  this file's header. Throws as `placer` does, when a prefix-table.tsv row, or an installers.tsv
 *  row that installs a copy, names a copy that skill-copies.tsv does not hold. */
export function attribute(
  config: SkillCutsConfig,
  outputs: readonly QueryOutput[],
  r4: R4Tables,
  profiles: SessionProfiles,
  transcript?: TranscriptRead
): Attribution {
  const hostContexts = new Map(
    config.transcriptStore?.hosts.map(({ host, context }) => [transcriptStoreName(host), context])
  );
  const hosts = new Set(transcript?.coverage.map(({ store }) => store));
  const stores = [
    ...config.stores,
    ...[...hosts].map((name) => ({ name, context: hostContexts.get(name) ?? "unknown" })),
  ];
  const queriedDays = new Set(transcript?.queries.outputs.map(({ name }) => name.slice(0, 10)));
  const claudeCounted =
    transcript?.coverage.some(
      ({ format, days }) => format === "claude" && [...days].some((day) => queriedDays.has(day))
    ) ?? false;
  const devboxStores = new Set(
    stores.filter((store) => store.context === "devbox").map((store) => store.name)
  );
  const { place, storeUnits, members, installedInContext, counted } = placer(
    outputs,
    r4,
    profiles,
    devboxStores,
    new Set([...hosts].filter((store) => !hostContexts.has(store)))
  );
  const coverage = transcript === undefined ? [] : transcriptCoverage(transcript, counted.outputs);
  const windowStart = Date.parse(config.windowStart);
  const windowLast = Date.parse(config.windowEnd) - 1;
  const partial = new Set(
    coverage
      .filter((item) => item.first > windowStart || item.last < windowLast || item.gaps.length > 0)
      .map((item) => sessionKey(item.store, item.format))
  );

  // Each copy's sessions, and each skill name's sessions no copy got, by the row's kind.
  const copySessions = {
    read: new Map<string, Set<string>>(),
    reference: new Map<string, Set<string>>(),
  };
  const unplaced = {
    read: new Map<string, Set<string>>(),
    reference: new Map<string, Set<string>>(),
  };
  // Each copy's read sessions placed under a former name, and those names.
  const formerSessions = new Map<string, Set<string>>();
  const formerNames = new Map<string, Set<string>>();
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
  for (const output of counted.outputs) {
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
      const groups = new Map<string, string[]>();
      for (const hash of row.hashes) {
        const key = sessionKey(row.store, hash);
        entry(groups, profiles.get(key) ?? "unknown", (): string[] => []).push(key);
      }
      for (const [profile, sessions] of groups) {
        const placed = place(row.store, row.copy, row.skill, profile);
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
        if (row.kind === "read" && copy !== undefined && placed.formerName !== undefined) {
          entry(formerNames, copy, () => new Set<string>()).add(placed.formerName);
          const former = entry(formerSessions, copy, () => new Set<string>());
          for (const session of sessions) former.add(session);
        }
      }
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
    let transcriptSessions = 0;
    const transcriptHosts = new Map<string, number>();
    let partialSessions = 0;
    const partialHosts = new Set<string>();
    let ompRead = false;
    for (const session of readSessions) {
      const format = transcript?.formats.get(session);
      if (format === undefined || format === "omp") ompRead = true;
      if (format === undefined) continue;
      transcriptSessions++;
      const store = session.slice(0, session.indexOf("\t"));
      const host = store.slice(TRANSCRIPT_PREFIX.length);
      transcriptHosts.set(host, (transcriptHosts.get(host) ?? 0) + 1);
      if (partial.has(sessionKey(store, format))) {
        partialSessions++;
        partialHosts.add(host);
      }
    }
    for (const session of copySessions.reference.get(key) ?? []) {
      const format = transcript?.formats.get(session);
      if (format === undefined || format === "omp") ompRead = true;
    }
    const ompInstalls = ompRead || installedInContext.has(key) || ompInstalled.has(key);
    const claudeInstalls = claudeInstalled.has(key);
    const status = copyStatus(sessions, ompInstalls, claudeInstalls && !claudeCounted);
    return {
      repository: row.repository,
      path: row.path,
      name: row.name,
      sessions,
      transcriptSessions,
      transcriptHosts: [...transcriptHosts]
        .map(([host, count]) => ({ host, sessions: count }))
        .sort((a, b) => b.sessions - a.sessions || a.host.localeCompare(b.host)),
      partialCoverageHosts:
        partialSessions * 2 > transcriptSessions ? [...partialHosts].sort() : [],
      readSessions,
      formerNameSessions: formerSessions.get(key)?.size ?? 0,
      formerNames: [...(formerNames.get(key) ?? [])].sort(),
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
    transcriptCoverage: coverage,
    claudeCounted,
    stores: stores.map((store): StoreCount => {
      const stat = stats.get(store.name);
      return {
        store: store.name,
        context: store.context,
        outputs: stat?.outputs ?? 0,
        filesRead: stat?.files ?? 0,
        bytesRead: stat?.bytes ?? 0,
        badLines: stat?.bad ?? 0,
        sharedCallIds: stat?.shared ?? 0,
        sharedAcrossQueries: counted.acrossQueries.get(store.name)?.keys ?? 0,
        filePairsAcrossQueries: counted.acrossQueries.get(store.name)?.pairs ?? 0,
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

/** A ranked skill's `<cut or set id>/models/<skill>.json` (`modelFiles`): its ModelCount, as
 *  `behaviour --reading-models` and `build-cases --written` read it (`modelCountOf`). */
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

/** The ModelCount `value` holds: the JSON of the models file `file`, which the flag `flag` names.
 *  Throws a refusal when it is none. */
export function modelCountOf(value: unknown, file: string, flag: string): ModelCount {
  const parsed = ModelCountFile.safeParse(value);
  if (!parsed.success) {
    throw new Refused(
      `${flag} ${file} is not a skill's reading sessions by model (attribute's <cut or set id>/models/<skill>.json): ${schemaFaults(parsed.error)}`
    );
  }
  return parsed.data;
}

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

/** Each ranked changed skill of `ranking`, in rank order, with the cut repository's copy's counts
 *  and its read sessions (`sessionKey`) after every attribution rule: no copy and no session for a
 *  directory the copies output holds no copy at. */
export function rankedSessions(
  attribution: Attribution,
  ranking: Ranking
): {
  rank: number;
  skill: string;
  path: string;
  copy: CopyCount | undefined;
  sessions: ReadonlySet<string>;
}[] {
  const copies = new Map(
    attribution.copies.map((copy) => [copyKey(copy.repository, copy.path), copy])
  );
  return ranking.rows.map((row, index) => {
    const copy = copies.get(copyKey(ranking.repository, row.path));
    return {
      rank: index + 1,
      skill: row.skill,
      path: row.path,
      copy,
      sessions: copy?.readSessions ?? new Set(),
    };
  });
}

/** Each ranked changed skill of `ranking` with its read sessions by model, in rank order. */
export function rankingModels(
  attribution: Attribution,
  ranking: Ranking,
  models: SessionModels
): { rank: number; skill: string; path: string; count: ModelCount }[] {
  return rankedSessions(attribution, ranking).map((row) => ({
    rank: row.rank,
    skill: row.skill,
    path: row.path,
    count: modelCount(row.sessions, models),
  }));
}

/** The ranked changed skills of `ranking` that get files of their own under its cut's directory,
 *  in rank order, each with the cut repository's copy's counts and its read sessions that a
 *  Postgres store holds. A skill whose name is no file name, or that two ranked directories
 *  share, gets none and is named in `skipped`. */
function filedSkills(
  attribution: Attribution,
  ranking: Ranking
): {
  rows: { skill: string; copy: CopyCount | undefined; sessions: ReadonlySet<string> }[];
  skipped: string[];
} {
  const ranked = rankedSessions(attribution, ranking);
  const rows: { skill: string; copy: CopyCount | undefined; sessions: ReadonlySet<string> }[] = [];
  const skipped: string[] = [];
  for (const row of ranked) {
    const shared = ranked.filter((other) => other.skill === row.skill).length > 1;
    if (!/^[A-Za-z0-9._-]+$/.test(row.skill) || row.skill.startsWith(".") || shared) {
      skipped.push(`${row.skill} (${row.path})`);
      continue;
    }
    // build-cases can pull only from the Postgres stores. Rankings still use all reads.
    rows.push({
      ...row,
      sessions: new Set([...row.sessions].filter((key) => !key.startsWith(TRANSCRIPT_PREFIX))),
    });
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
  const files = rows.map((row) => ({
    name: path.join("sessions", `${row.skill}.jsonl`),
    text: sessionLines([...row.sessions].sort()),
  }));
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

/** A ranked skill's `<cut id>/store-only/<skill>.json` (`storeOnlyFiles`): its counted sessions
 *  (`counted`, the cut repository's copy's `sessions`, which its ranking counts), those of them
 *  only the transcript store holds (`storeOnly`, its `transcript_store_sessions`), and each
 *  host's of those (`hosts`, most first, a tie by host). A session only the transcript store
 *  holds is in no sessions file, since build-cases pulls a case's session from a Postgres store:
 *  it is never a case source. `behaviour report --store-only` and `build-cases --store-only` read
 *  it (`readStoreOnly`). */
export const StoreOnlyFile = z.strictObject({
  skill: z.string().min(1),
  counted: Count,
  storeOnly: Count,
  hosts: z.array(z.strictObject({ host: z.string().min(1), sessions: Count })),
});
export type StoreOnly = z.infer<typeof StoreOnlyFile>;

/** The store-only files of `ranking`, `store-only/<skill>.json` under its cut's directory: one per
 *  sessions file (`sessionFiles`), the StoreOnly of the skill's copy, as JSON. */
export function storeOnlyFiles(
  attribution: Attribution,
  ranking: Ranking
): { name: string; text: string }[] {
  return filedSkills(attribution, ranking).rows.map(({ skill, copy }) => {
    const storeOnly: StoreOnly = {
      skill,
      counted: copy?.sessions ?? 0,
      storeOnly: copy?.transcriptSessions ?? 0,
      hosts: [...(copy?.transcriptHosts ?? [])],
    };
    return {
      name: path.join("store-only", `${skill}.json`),
      text: `${JSON.stringify(storeOnly, null, 2)}\n`,
    };
  });
}

/** The StoreOnly `file` holds (`storeOnlyFiles`), for `--store-only <file>`. Throws a refusal
 *  naming the file when it cannot be read, is not JSON or is not of StoreOnlyFile's shape. */
export function readStoreOnly(file: string): StoreOnly {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Refused(`cannot read --store-only ${file}: ${messageFor(error)}`);
  }
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new Refused(`--store-only ${file} is not JSON: ${messageFor(error)}`);
  }
  const parsed = StoreOnlyFile.safeParse(value);
  if (!parsed.success) {
    throw new Refused(
      `--store-only ${file} is not a skill's store-only sessions (attribute's <cut id>/store-only/<skill>.json): ${schemaFaults(parsed.error)}`
    );
  }
  return parsed.data;
}

/** Each host of `storeOnly` with its store-only sessions, `<host>: <sessions>`, joined by `, `. */
export function storeOnlyHosts(storeOnly: StoreOnly): string {
  return storeOnly.hosts.map(({ host, sessions }) => `${host}: ${sessions}`).join(", ");
}

/** The end of a reason a skill has no case, or too few informative cases, for a verdict: how many
 *  of its counted sessions only the transcript store holds, so that none of them was a case
 *  source, and on which hosts; "" when none is. */
export function storeOnlyWords(storeOnly: StoreOnly): string {
  return storeOnly.storeOnly === 0
    ? ""
    : `; ${storeOnly.storeOnly} of the skill's ${storeOnly.counted} counted sessions are store-only, so they were never case sources (${storeOnlyHosts(storeOnly)})`;
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
      count.formerNameSessions,
      count.referenceSessions,
      count.unattributed,
      count.ties.join("; "),
      count.loadedBy.length > 0 ? "yes" : "no",
      count.status,
      count.transcriptSessions,
      count.partialCoverageHosts.join("; "),
    ])
  );
}

/** `text` as Markdown inline code. */
function code(text: string): string {
  return text.includes("`") ? `\`\` ${text} \`\`` : `\`${text}\``;
}
/** `text` as one Markdown table cell: each backslash escaped first, then each `|`, so a cell's
 * own backslash never escapes the pipe after it. */
export const cell = (text: string | number) =>
  String(text).replaceAll("\\", "\\\\").replaceAll("|", "\\|");
export const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
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

/** One row of a Stores section's transcript-store table (`transcriptStoreRows`). */
export interface TranscriptStoreRow {
  readonly store: string;
  readonly context: string;
  readonly format: string;
  readonly firstRecord: string;
  readonly lastRecord: string;
  /** What the row's sessions miss of the window: `starts after the window's start`, `ends before
   *  the window's end` and `gap days: <day>, ...`, joined by `; `, or `no gap`. */
  readonly coverage: string;
  readonly sessionsInWindow: number;
  readonly addedSessions: number;
  readonly addedSessionsWithRead: number;
}

/** The rows of a Stores section's transcript-store table, from `transcriptCoverage`'s `coverage`
 *  of `transcript`: each host, by its store label with its configured context (else `unknown`),
 *  and format, in `coverage`'s order; a host of more than one format first gets a row of them
 *  `all` (their days and sessions as one, their added sessions and those with a read summed). */
export function transcriptStoreRows(
  config: SkillCutsConfig,
  transcript: TranscriptRead,
  coverage: readonly TranscriptCoverage[]
): TranscriptStoreRow[] {
  const contexts = new Map(
    config.transcriptStore?.hosts.map(({ host, context }) => [transcriptStoreName(host), context])
  );
  const rows: TranscriptStoreRow[] = [];
  for (const store of new Set(coverage.map((item) => item.store))) {
    const formats = coverage.filter((item) => item.store === store);
    if (formats.length > 1) {
      const hostRows = transcript.coverage.filter((item) => item.store === store);
      const hashes = new Set(hostRows.flatMap((item) => [...item.hashes]));
      const days = new Set(hostRows.flatMap((item) => [...item.days]));
      const first = Math.min(...formats.map((item) => item.first));
      const last = Math.max(...formats.map((item) => item.last));
      formats.unshift({
        store,
        format: "all",
        first,
        last,
        gaps: daysFrom(first, last).filter((day) => !days.has(day)),
        sessions: hashes.size,
        added: formats.reduce((sum, item) => sum + item.added, 0),
        reads: formats.reduce((sum, item) => sum + item.reads, 0),
      });
    }
    for (const item of formats) {
      const limits = [];
      if (item.first > Date.parse(config.windowStart))
        limits.push("starts after the window's start");
      if (item.last < Date.parse(config.windowEnd) - 1) limits.push("ends before the window's end");
      if (item.gaps.length > 0) limits.push(`gap days: ${item.gaps.join(", ")}`);
      rows.push({
        store,
        context: contexts.get(store) ?? "unknown",
        format: item.format,
        firstRecord: iso(item.first),
        lastRecord: iso(item.last),
        coverage: limits.join("; ") || "no gap",
        sessionsInWindow: item.sessions,
        addedSessions: item.added,
        addedSessionsWithRead: item.reads,
      });
    }
  }
  return rows;
}

/** A Stores section's lines on the transcript store: which store or host owns a file, the table
 *  of `transcriptStoreRows`, its queries with output, and each format's added sessions with a
 *  read. */
export function transcriptStoreLines(
  config: SkillCutsConfig,
  transcript: TranscriptRead,
  coverage: readonly TranscriptCoverage[]
): string[] {
  const lines = [
    "",
    "### Transcript store",
    "",
    "A session is in the window when its recorded span overlaps it. The first and last record below are clipped to the window. Postgres owns every file listed by its hashes query, including files with no skill read. Of the remaining files, when several hosts hold the same hash, the first host label in byte order owns it. Added sessions with a read include successful reference reads, but not unpaired calls.",
    "",
    "| Store | Context | Format | First record | Last record | Coverage | Sessions in window | Added sessions | Added sessions with a skill read |",
    "| --- | --- | --- | --- | --- | --- | ---: | ---: | ---: |",
  ];
  for (const row of transcriptStoreRows(config, transcript, coverage)) {
    lines.push(
      `| ${cell(row.store)} | ${cell(row.context)} | ${cell(row.format)} | ${row.firstRecord} | ${row.lastRecord} | ${row.coverage} | ${row.sessionsInWindow} | ${row.addedSessions} | ${row.addedSessionsWithRead} |`
    );
  }
  const observed = new Set(
    transcript.queries.outputs.flatMap(({ tools }) => tools.map(({ format }) => format))
  );
  const resultDays = transcript.queries.outputs.length;
  lines.push("", `Transcript-store count: ${counted(resultDays, "query", "queries")} with output.`);
  if (resultDays === 0) lines.push("No transcript-store read queries have output.");
  for (const format of [...new Set(coverage.map((item) => item.format))].sort()) {
    const reads = coverage
      .filter((item) => item.format === format)
      .reduce((sum, item) => sum + item.reads, 0);
    lines.push(
      `- ${code(format)}: ${counted(reads, "added session")} with a skill read.` +
        (resultDays > 0 && !observed.has(format)
          ? " The queries found no read, Read or Skill calls in this format."
          : "")
    );
  }
  return lines;
}

function transcriptMarkdown(report: Report): string[] {
  const { config, count, attribution } = report;
  const transcript = count.transcript;
  if (config.transcriptStore === undefined)
    return ["- The transcript store is not read: no transcriptStore is configured."];
  if (!transcript?.coverageRead)
    return ["- The transcript store is not read: the count holds no transcript coverage output."];
  const lines = transcriptStoreLines(config, transcript, attribution.transcriptCoverage);
  const marked = attribution.copies.filter((copy) => copy.partialCoverageHosts.length > 0);
  if (marked.length > 0) {
    lines.push(
      "",
      "#### Partial coverage",
      "",
      "Most of the transcript-store sessions added to each of these copies came from the listed hosts in formats that miss part of the window. The same hosts appear in `partial_coverage_hosts` in the TSV."
    );
    for (const copy of marked)
      lines.push(
        `- ${copy.name} (${copy.repository} ${copy.path}): ${copy.partialCoverageHosts.join(", ")}.`
      );
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
    `End time ${config.windowEnd}. The window is [${config.windowStart}, ${config.windowEnd}), ${windowDays} days. Postgres queries read one UTC day (or one hour) of file mtime from the window's start on. Transcript-store queries read one UTC day (or one hour) of call times. A read counts when its call's timestamp is in the window. A session is one transcript file, so a subagent's file is a session of its own, and a file that two queries hold is one session. A read whose call id and timestamp two files hold (a forked session copies its parent's lines) counts in one of them, whichever queries in the source hold the two.`,
    "",
    `${attribution.copies.length} copies of ${names.size} names in ${repositories.length} repositories: ` +
      (attribution.claudeCounted ? STATUSES.slice(0, 2) : STATUSES)
        .map((status) => `${of(status).length} ${status}`)
        .join(", ") +
      `. ${counted(attribution.readSessions, "session")} read a skill; ${attribution.unattributedSessions} of them read a skill that no copy got.`,
    "",
    "## Stores",
    "",
    "| Store | Context | Earliest record | Files in the store | Earliest read in the window | Sessions with a read | Queries with output | Files read | Bytes read | Lines not JSON | Call ids in two files of one query | Call ids in files of two queries | File pairs of two queries with one call id |",
    "| --- | --- | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
  );
  for (const store of attribution.stores) {
    if (store.store.startsWith(TRANSCRIPT_PREFIX)) continue;
    const span = spans.find((candidate) => candidate.store === store.store);
    const earliest =
      span === undefined
        ? "not measured"
        : span.earliest_mtime_ms === null
          ? "no record"
          : iso(span.earliest_mtime_ms);
    say(
      `| ${cell(store.store)} | ${cell(store.context)} | ${earliest} | ${span?.files ?? "not measured"} | ${store.earliestRead ?? "no read"} | ${store.readSessions} | ${store.outputs} | ${store.filesRead} | ${store.bytesRead} | ${store.badLines} | ${store.sharedCallIds} | ${store.sharedAcrossQueries} | ${store.filePairsAcrossQueries} |`
    );
  }
  say("");
  for (const store of attribution.stores) {
    if (!store.store.startsWith(TRANSCRIPT_PREFIX)) {
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
    }
    if (store.unpairedSessions > 0) {
      say(
        `- ${store.store}: a call with no result in its file, in ${counted(store.unpairedSessions, "session")}: such a call is not a read, and no copy counts it.`
      );
    }
  }
  say(...transcriptMarkdown(report));
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
    `1. The prefix table (prefix-table.tsv): a read goes to the copy that all matching rows agree on. Only the prefix table places reads under the worker image's install location (${code(IMAGE_ROOT)}). A miss or tie leaves an image read unattributed, with its former-stage note if there is one. Neither checkout nor context can substitute a copy. A former-stage row for a Git rename maps the old path to the renamed copy. Its reads count in both the copy's total sessions and its sessions under a former name (former_name_sessions).`,
    "2. A checkout, for a path not under the worker image's install location: a read whose path ends with a copy's skill directory in its repository (with a path boundary before it) is a read of that copy in a checkout of its repository; the longest such directory wins. Copies of several repositories at that directory are told apart by the path before it, in this order: the one whose owner and name are two directories of it in a row; else the one whose name is a directory of it; else the one whose owner is a directory followed by one that holds its name; else the one whose name a directory holds (`.dotfiles` holds `dotfiles`). An exact name always wins over a directory that only holds it. A tie left goes to step 3, among the tied copies alone. A skill directory of one segment (a skill at its repository's root) counts only when the path names its repository. A path inside a copy of another repository that an installer writes (`<dir>/vendor/<name>/`, an `ensure_vendor` clone; a package under `node_modules/`; a plugin under `plugins/cache/`) is a checkout only of a repository that the path from that copy's own directory on names; when it names none, step 3 decides.",
    "3. The session's profile: use its profile from roots.txt. For each store and profile, infer installed units from the prefix-table reads of that group; the default profile of a devbox store also has each unit the dev box's own installers install into Oh My Pi (installers.tsv). Use the copies of those units alone, including former names from matching prefix rows. A profile must supply exactly one copy of the name (or one of the tied copies). An unknown profile does not place the read. A read inside a project skills directory of a checkout (not of a home directory) is a project skill: only a copy in such a directory places it (step 2), never this step, so no plugin's copy of its name is it.",
    "4. Otherwise the read is unattributed for its skill name.",
    "",
    "Two copies of one name are a tie when their sessions differ by at most that name's unattributed sessions.",
    "A transcript-store host with a configured context uses the same rules. A host with no configured context uses only the prefix table and the checkout rule.",
    "",
    "### Units read from in each store, across profiles",
    "",
    "Step 3's group of a devbox store's default profile also holds each unit the dev box's own installers install into Oh My Pi, which no read need come from, so these lists leave them out.",
    ""
  );
  for (const store of attribution.stores) {
    if (store.units.length === 0) {
      say(
        `- ${store.store} (${store.context}): none, since no read of this store maps by the prefix table; the context rule places none of its reads${store.context === "devbox" ? " but its default-profile reads of a copy the dev box's own installers install" : ""}.`
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

  say(
    "",
    "### Reads under a former name",
    "",
    "Each copy read under a name it had before git renamed it (former-stages.tsv): its sessions, and of them those under the former name (skill-read-count.tsv's former_name_sessions).",
    ""
  );
  const renamedRead = attribution.copies.filter((copy) => copy.formerNameSessions > 0);
  if (renamedRead.length === 0) say("None.");
  else
    say(
      "| Skill | Copy | Sessions | Under its former name | Former name |",
      "| --- | --- | ---: | ---: | --- |"
    );
  for (const copy of renamedRead) {
    say(
      `| ${cell(copy.name)} | ${cell(`${copy.repository} ${copy.path}`)} | ${copy.sessions} | ${copy.formerNameSessions} | ${cell(copy.formerNames.join(", "))} |`
    );
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
    "A skill that code, an agent definition or an installer loads into a session: its text reaches the session with no read, so its read count leaves out each session that loads it so.",
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

  if (!attribution.claudeCounted) {
    say(
      "## Claude Code",
      "",
      "See Stores for the coverage of the count. These statuses distinguish copies installed by Claude Code when its reads cannot be measured.",
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
  }

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
  if (rankings.length > 0 && count.transcript?.queries.outputs.length)
    say(
      "Rankings include transcript-store sessions. The sessions files list only Postgres sessions, which build-cases can pull."
    );
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
  "usage: attribute.ts --config <skill-cuts.json> --count <dir> --copies <dir> --spans <file> --out <dir> [--models <dir>] [--changed <cut or set id>=<file>]...";

/** What `main` writes to. */
export interface Output {
  out(text: string): void;
  err(text: string): void;
}

/** Whether `inner` is `outer` or a path inside it. set-count.ts refuses an `--out` inside its
 *  `--rows` by it. */
export function inside(inner: string, outer: string): boolean {
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
      return refuse(`--changed ${JSON.stringify(item)} is not <cut or set id>=<file>`);
    const measured = cutOrSet(config, id);
    if (measured === undefined) {
      const named = (kind: string, each: readonly { id: string }[]) =>
        each.length === 0 ? `no ${kind}` : `the ${kind}s ${each.map((c) => c.id).join(", ")}`;
      return refuse(
        `--changed names ${JSON.stringify(id)}, which is no cut or set of the configuration; it names ${named("cut", config.cuts)} and ${named("set", config.sets ?? [])}`
      );
    }
    if (!/^[A-Za-z0-9._-]+$/.test(id) || id === "." || id === "..") {
      return refuse(
        `the ${measured.kind} id ${JSON.stringify(id)} cannot name a directory under --out`
      );
    }
    if (changed.some((entry) => entry.cut === id))
      return refuse(`--changed names the ${measured.kind} ${id} twice`);
    changed.push({ cut: id, repository: measured.repo, file });
  }
  try {
    const stores = config.stores.map((store) => store.name);
    const count = readCount(countDir, stores, config);
    const r4 = readR4(copiesDir);
    const spans = parseSpans(readFileSync(spansFile, "utf8"), spansFile);
    const attribution = attribute(config, count.outputs, r4, count.profiles, count.transcript);
    const issues = queryIssues(config, count, count.transcript?.queries);
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
      if (config.transcriptStore !== undefined)
        writeDir(ranking.cut, "store-only", storeOnlyFiles(attribution, ranking));
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
