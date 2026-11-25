/**
 * The count of the skill sets' skills in the read count's rows (README.md, "set-count"):
 *
 *   bun scripts/skill-cuts/cli.ts set-count --config <skill-cuts.json> --rows <raw count dir>
 *     --spans <spans.jsonl> --out <dir> [--dotfiles-commit <sha>] [--rows-label <text>]
 *     [--not-read <name>=<reason>]...
 *
 * cli.ts runs `main(argv, io)`; set-count.test.ts calls it with a stand-in gh (`io.gh`), and calls
 * the pure parts. It reads no store and calls no model: it reads its inputs and GitHub, and writes
 * only under `--out`, which must not be inside `--rows`.
 *
 * Inputs.
 *   --config  The configuration (config.ts), which must give `sets` and `dotfilesRepo`.
 *   --rows    The count's raw output, as attribute.ts reads its `--count` (`readCount`, the one
 *             reader of count.sql's rows): `<store>/<YYYY-MM-DD>.txt`, or `<YYYY-MM-DDTHH>.txt` for
 *             each hour of a split day, one count.sql query's psql output each, its count rows and
 *             its summary row; and each store's `<store>/roots.txt`, which readCount refuses a
 *             store's rows without: `count-sql --config <file> --store <store> --query roots`, run
 *             through the store's psqlCommand, its output saved there.
 *   --spans   Each store's earliest record, in the form `attribute --spans` takes.
 *   --dotfiles-commit  The commit of `dotfilesRepo` whose pins it reads (sets.ts), 40 hex digits;
 *             without it, the default branch's commit when it runs.
 *   --rows-label  Words that name the rows (an artifact and its version), written as given.
 *   --not-read  `<name>=<reason>`, for each source of sessions the rows cannot hold (the Claude
 *             Code transcripts), named as not read with its reason.
 *
 * Refusals (exit 2), before any GitHub read: the arguments; a configuration without sets; a
 * `--not-read` that is not `<name>=<reason>`; rows or a spans file it cannot read, rows an earlier
 * count.sql made (a count row with no `calls` or `unpaired_calls`: `readCount`); and rows in
 * which a configured store that has rows lacks a summary row for a day from the window's start to
 * its end (`windowGaps`): no output of that day, not all 24 of its hours, a file that is not one
 * whole count.sql result, or a summary row whose mtime range is not its query's. Each gap is
 * named. A store with no rows at all is named as not read.
 *
 * The rule. A count row counts as the skill S of a set when S is one of the set's
 * skills at its pin (sets.ts: the `skills/<name>/SKILL.md` of its repository at the pinned commit)
 * and its `copy` (`copyColumn`):
 *   - is a URL: one that names the set's repository or an upstream of it (its fork parent or
 *     source) is the column `upstream_url`, any other `other_copy`; a URL is no install directory;
 *   - starts with `<set>:` (a Claude Code `Skill` call's plugin prefix), holds the install
 *     directory of its pin (`/node_modules/<package>/` for a package pin, `/vendor/<dir>/` for an
 *     ensure_vendor pin), or holds `/plugins/cache/` and after it a path part that is the set's id
 *     (Claude Code's plugin cache): `own`;
 *   - names no copy (empty, a `skill://` path, or a name with no `/`, `\` or `:`): `no_copy`;
 *   - is any other path or prefix: `other_copy`.
 * A session is `(store, file hash)`; only a row's paired reads (`hashes`) count, never its unpaired
 * calls. A read that two files of a store hold (one key of count.sql's `calls`: a forked session
 * copies its parent's lines) counts in the file whose hash sorts first, over every query of the
 * store, as attribute.ts counts it (`countOnce`, count.sql's rule applied across queries); a file
 * stays in a row when one of its keys there counts in it. Each session counts once for S, in the
 * first of `own`, `upstream_url`, `other_copy`, `no_copy` that one of its `read` rows of S
 * reaches. `reference_own` counts the sessions with a `reference` row of S at the own copy, apart
 * from those four. Each column is also counted per store. Every skill of each set at its pin is
 * listed, one with no read at 0.
 *
 * Outputs under --out.
 *   exemplar-count.md   The window; the rows read (the label, and the digest of their files); each
 *                       store with its earliest record (the spans file's); each store not read
 *                       and why; the dotfiles commit and each pin with the commit it resolves to;
 *                       the pin changes in the window; the rule; the sessions per skill, and per
 *                       skill and store; the skills with at least AT_LEAST own-copy sessions; the
 *                       count-sql command of each query the rows hold and of each store's
 *                       roots.txt, and this command.
 *   exemplar-count.tsv  TSV_COLUMNS, then each column again per configured store
 *                       (`<column>:<store>`), one row per skill of each set, in the sets' order,
 *                       most own-copy sessions first, a tie by name.
 *   sessions/<set>/<skill>.jsonl
 *                       Each own-copy session of the skill, one line `{"store", "hash"}`, sorted
 *                       by store then hash (an empty file for none): attribute.ts's form.
 *   summary.json        The numbers, the rows' files with their sha256, the stores, the stores
 *                       not read, the pins, the pin changes, the skills at AT_LEAST or more, and
 *                       the commands.
 *
 * Exit status: 0 the count is written; 1 failed (a GitHub read, a set the dotfiles commit does not
 * pin once, a pin that does not resolve, a set whose skills are not at `skills/`); 2 refused.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import {
  type CountRead,
  cell,
  countOnce,
  daysFrom,
  HOURS,
  inside,
  iso,
  keyStore,
  parseSpans,
  type QueryOutput,
  readCount,
  type Span,
  sessionKey,
  sessionLines,
  twoDigits,
} from "./attribute";
import {
  commandRetries,
  configFrom,
  json,
  parse,
  Refused,
  required,
  sha256,
  started,
  writeWhole,
} from "./common";
import type { SkillCutsConfig } from "./config";
import { type Gh, tsv } from "./copies";
import { mtimeRange } from "./countsql";
import {
  COMMIT,
  type PinChange,
  type Pins,
  pinChanges,
  readPins,
  type SetAtPin,
  SetReads,
} from "./sets";

const READ_COLUMNS = ["own", "upstream_url", "other_copy", "no_copy"] as const;
type ReadColumn = (typeof READ_COLUMNS)[number];
/** The columns of a skill's sessions: the four of its `read` rows, in the order a session takes
 *  the first one it reaches, then the reference reads at its own copy. */
export const COLUMNS = [...READ_COLUMNS, "reference_own"] as const;
export type Column = (typeof COLUMNS)[number];
/** Each column's words in exemplar-count.md. */
const COLUMN_WORDS: Readonly<Record<Column, string>> = {
  own: "Own copy",
  upstream_url: "Upstream by URL",
  other_copy: "Only another copy",
  no_copy: "No copy named",
  reference_own: "Reference reads at own copy",
};
const TSV_COLUMNS = ["set", "skill", ...COLUMNS.map((column) => `${column}_sessions`)];
/** The own-copy sessions from which a skill is on the list of the most read. */
export const AT_LEAST = 20;
const PLUGIN_CACHE = "/plugins/cache/";
const CLI = "bun scripts/skill-cuts/cli.ts";

const USAGE =
  "usage: cli.ts set-count --config <skill-cuts.json> --rows <raw count dir> --spans <spans.jsonl> --out <dir> [--dotfiles-commit <sha>] [--rows-label <text>] [--not-read <name>=<reason>]...";

/** What the rule needs of a set at its pin. */
export interface SetRule {
  readonly id: string;
  /** The set's repository and its upstreams, `owner/name`. */
  readonly repos: readonly string[];
  /** The install directory its pin gives, as a copy path holds it. */
  readonly installDir: string;
  readonly skills: readonly string[];
}

/** The rule of each set at its pin. */
export function setRules(sets: readonly SetAtPin[]): SetRule[] {
  return sets.map((set) => ({
    id: set.id,
    repos: [set.repo, ...set.upstreams],
    installDir:
      set.pin.kind === "package" ? `/node_modules/${set.pin.package}/` : `/vendor/${set.pin.dir}/`,
    skills: set.skills,
  }));
}

/** Whether the URL `url` names the repository `repo` (`owner/name`, in any case) as a path. */
function namesRepository(url: string, repo: string): boolean {
  const lower = url.toLowerCase();
  const needle = `/${repo.toLowerCase()}`;
  for (let at = lower.indexOf(needle); at >= 0; at = lower.indexOf(needle, at + 1)) {
    if (/^(\.git)?([/?#]|$)/.test(lower.slice(at + needle.length))) return true;
  }
  return false;
}

/** The column a `read` row of a skill of `rule` at the copy `copy` goes to (the header's rule). */
function copyColumn(rule: SetRule, copy: string): ReadColumn {
  if (/^https?:\/\//i.test(copy)) {
    return rule.repos.some((repo) => namesRepository(copy, repo)) ? "upstream_url" : "other_copy";
  }
  if (copy.startsWith(`${rule.id}:`) || copy.includes(rule.installDir)) return "own";
  const cache = copy.indexOf(PLUGIN_CACHE);
  if (
    cache >= 0 &&
    copy
      .slice(cache + PLUGIN_CACHE.length)
      .split("/")
      .includes(rule.id)
  ) {
    return "own";
  }
  if (copy === "" || copy.startsWith("skill://") || !/[/\\:]/.test(copy)) return "no_copy";
  return "other_copy";
}

/** One skill of a set and its sessions (`sessionKey`s) in each column. */
export interface SkillCount {
  readonly set: string;
  readonly skill: string;
  readonly sessions: Readonly<Record<Column, ReadonlySet<string>>>;
}

/** Each skill of each set of `rules`, in their order, with its sessions in `outputs`' rows. */
export function countSets(
  rules: readonly SetRule[],
  outputs: readonly QueryOutput[]
): SkillCount[] {
  const tallies = rules.flatMap((rule) =>
    rule.skills.map((skill) => ({
      rule,
      skill,
      /** Each session's first column of READ_COLUMNS its `read` rows reach, by its index. */
      reads: new Map<string, number>(),
      references: new Set<string>(),
    }))
  );
  const bySkill = new Map<string, typeof tallies>();
  for (const tally of tallies)
    bySkill.set(tally.skill, [...(bySkill.get(tally.skill) ?? []), tally]);
  for (const output of outputs) {
    for (const row of output.rows) {
      for (const tally of bySkill.get(row.skill) ?? []) {
        const column = copyColumn(tally.rule, row.copy);
        if (row.kind === "reference") {
          if (column !== "own") continue;
          for (const hash of row.hashes) tally.references.add(sessionKey(row.store, hash));
          continue;
        }
        const rank = READ_COLUMNS.indexOf(column);
        for (const hash of row.hashes) {
          const key = sessionKey(row.store, hash);
          if (rank < (tally.reads.get(key) ?? READ_COLUMNS.length)) tally.reads.set(key, rank);
        }
      }
    }
  }
  return tallies.map(({ rule, skill, reads, references }) => {
    const sessions: Record<Column, Set<string>> = {
      own: new Set(),
      upstream_url: new Set(),
      other_copy: new Set(),
      no_copy: new Set(),
      reference_own: references,
    };
    for (const [key, rank] of reads) sessions[READ_COLUMNS[rank] ?? "no_copy"].add(key);
    return { set: rule.id, skill, sessions };
  });
}

/** Each gap of the rows: for each configured store that has rows, each query file that is not one
 *  whole result or whose summary row is not of its query's range, and each day from the window's
 *  start to its end with no summary row of the day or of each of its 24 hours. */
function windowGaps(config: SkillCutsConfig, count: CountRead): string[] {
  const gaps: string[] = [];
  const days = daysFrom(Date.parse(config.windowStart), Date.parse(config.windowEnd) - 1);
  for (const { name: store } of config.stores) {
    const outputs = count.outputs.filter((output) => output.store === store);
    const unreadable = count.unreadable.filter((item) => item.store === store);
    if (outputs.length === 0 && unreadable.length === 0) continue;
    for (const item of unreadable) {
      gaps.push(`${store} ${item.name}: not one whole count.sql result: ${item.why}`);
    }
    const covered = new Set<string>();
    for (const output of outputs) {
      const { mtime_from: from, mtime_to: to } = output.summary;
      const [day = "", hour] = output.name.split("T");
      let range: { from: number; to: number };
      try {
        range = mtimeRange(day, hour === undefined ? undefined : Number(hour));
      } catch (error) {
        gaps.push(`${store} ${output.name}: ${messageFor(error)}`);
        continue;
      }
      if (from === range.from && to === range.to) covered.add(output.name);
      else {
        gaps.push(
          `${store} ${output.name}: its summary row covers [${iso(from)}, ${iso(to)}), not that query's [${iso(range.from)}, ${iso(range.to)})`
        );
      }
    }
    for (const day of days) {
      if (covered.has(day)) continue;
      const hours = HOURS.filter((hour) => covered.has(`${day}T${twoDigits(hour)}`));
      if (hours.length === HOURS.length) continue;
      const missing = HOURS.filter((hour) => !hours.includes(hour));
      gaps.push(
        hours.length === 0
          ? `${store} ${day}: no summary row (no output of that day or of its hours)`
          : `${store} ${day}: summary rows of ${hours.length} of its 24 hours; hours ${missing.join(", ")} have none`
      );
    }
  }
  return gaps;
}

/** `arg` as one word of a shell command line. */
function shellWord(arg: string): string {
  return /^[A-Za-z0-9_@%+=:,./-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", `'\\''`)}'`;
}

/** The count-sql command line that makes the rows file `<store>/<file>.txt`: count-sql with the
 *  configuration `config`, the store and `query` (its day and hour, or `--query roots`), run
 *  through the configuration's psql command `psql` with the store's name. */
function countSqlLine(
  config: string,
  psql: string,
  store: string,
  query: readonly string[],
  file: string
): string {
  const words = [CLI, "count-sql --config", shellWord(config), "--store", store, ...query];
  return [...words, "|", shellWord(psql), store, ">", `${store}/${file}.txt`].join(" ");
}

/** One skill's numbers, in total and per store, and its own-copy sessions. */
interface SkillRow {
  readonly set: string;
  readonly skill: string;
  readonly totals: Readonly<Record<Column, number>>;
  readonly byStore: Readonly<Record<string, Readonly<Record<Column, number>>>>;
  readonly own: readonly string[];
}

/** A store as the count read it. */
interface StoreRead {
  readonly store: string;
  readonly context: string;
  readonly earliestRecord: string;
  readonly filesInStore: number | null;
  readonly queries: number;
  readonly sessionsWithRead: number;
}

/** Everything the count writes. */
interface Report {
  readonly config: SkillCutsConfig;
  readonly rowsLabel: string | undefined;
  readonly files: readonly { readonly file: string; readonly sha256: string }[];
  readonly digest: string;
  readonly stores: readonly StoreRead[];
  readonly otherDirs: readonly string[];
  readonly notRead: readonly { readonly name: string; readonly reason: string }[];
  readonly dotfilesRepo: string;
  readonly pins: Pins;
  readonly changes: readonly PinChange[];
  readonly skills: readonly SkillRow[];
  readonly atLeast: readonly SkillRow[];
  readonly commands: { readonly countSql: readonly string[]; readonly setCount: string };
}

/** Each count of `counts` as a row: its totals and per-store numbers; in the sets' order of
 *  `sets`, most own-copy sessions first, a tie by name. */
function skillRows(
  counts: readonly SkillCount[],
  sets: readonly string[],
  stores: readonly string[]
): SkillRow[] {
  const rows = counts.map((count) => {
    const totals = Object.fromEntries(
      COLUMNS.map((column) => [column, count.sessions[column].size])
    ) as Record<Column, number>;
    // One pass over each column's sessions, each counted in its store's numbers.
    const byStore = new Map(
      stores.map((store) => [
        store,
        Object.fromEntries(COLUMNS.map((column) => [column, 0])) as Record<Column, number>,
      ])
    );
    for (const column of COLUMNS) {
      for (const key of count.sessions[column]) {
        const numbers = byStore.get(keyStore(key));
        if (numbers !== undefined) numbers[column] += 1;
      }
    }
    return {
      set: count.set,
      skill: count.skill,
      totals,
      byStore: Object.fromEntries(byStore),
      own: [...count.sessions.own].sort(),
    };
  });
  return rows.sort(
    (a, b) =>
      sets.indexOf(a.set) - sets.indexOf(b.set) ||
      b.totals.own - a.totals.own ||
      a.skill.localeCompare(b.skill)
  );
}

/** `cells` as one Markdown table row, each `|` and backslash of a cell escaped (attribute.ts's
 *  `cell`). */
function tableRow(...cells: readonly (string | number)[]): string {
  return `| ${cells.map(cell).join(" | ")} |`;
}

/** exemplar-count.md's text. */
function markdown(report: Report): string {
  const { config, pins } = report;
  const lines: string[] = [];
  const say = (...text: string[]) => lines.push(...text);
  say(
    "# The skill sets' read count",
    "",
    `The window is [${config.windowStart}, ${config.windowEnd}): count.sql counts a read whose call's timestamp is in it. A session is one transcript file, its store and its file hash; each session counts once for a skill.`,
    "",
    "## The rows read",
    "",
    report.rowsLabel ?? "No --rows-label was given.",
    "",
    `${report.files.length} files of count.sql's output, one query of one store and one UTC day (or hour) of file mtime each. Their digest is sha256 \`${report.digest}\`: the sha256 of one line \`<sha256>  <store>/<file>\` per file, sorted by path, each line ending in a newline (\`sha256sum <files> | LC_ALL=C sort -k 2 | sha256sum\` in the rows directory). summary.json gives each file's sha256.`,
    "",
    "## Stores",
    "",
    tableRow(
      "Store",
      "Context",
      "Earliest record",
      "Files in the store",
      "Queries read",
      "Sessions with a read"
    ),
    "| --- | --- | --- | ---: | ---: | ---: |"
  );
  for (const store of report.stores) {
    say(
      tableRow(
        store.store,
        store.context,
        store.earliestRecord,
        store.filesInStore ?? "not measured",
        store.queries,
        store.sessionsWithRead
      )
    );
  }
  say(
    "",
    "The earliest record and the files in the store are the spans file's.",
    "",
    "## Not read",
    ""
  );
  if (report.notRead.length === 0 && report.otherDirs.length === 0) {
    say("Each configured store has rows, and no --not-read was given.");
  }
  for (const item of report.notRead) say(`- ${item.name}: ${item.reason}`);
  for (const dir of report.otherDirs) {
    say(
      `- The rows directory holds \`${dir}\`, which the configuration names no store for; it is not read.`
    );
  }
  say(
    "",
    "## Pins",
    "",
    `The pins of ${report.dotfilesRepo} at ${pins.commit} (${pins.commitFrom}).`,
    "",
    tableRow(
      "Set",
      "Repository",
      "Upstreams",
      "Pinned by",
      "Pin",
      "Commit",
      "Resolved by",
      "Skills from",
      "Skills"
    ),
    "| --- | --- | --- | --- | --- | --- | --- | --- | ---: |"
  );
  for (const set of pins.sets) {
    say(
      tableRow(
        set.id,
        set.repo,
        set.upstreams.join(", ") || "none",
        `${set.pin.manifest}:${set.pin.line}`,
        set.pin.text,
        set.commit,
        set.resolvedBy,
        set.skillsFrom,
        set.skills.length
      )
    );
  }
  say("", "## Pin changes in the window", "");
  if (report.changes.length === 0) say("No pin of a set changed in the window.");
  else {
    say(
      "Each commit of the window at which a set's pin changed; a read at the install directory counts whichever version it read.",
      "",
      tableRow("Commit", "Date", "Manifest", "Set", "Before", "After"),
      "| --- | --- | --- | --- | --- | --- |"
    );
    for (const change of report.changes) {
      say(
        tableRow(
          change.commit,
          change.date,
          change.manifest,
          change.set,
          change.before,
          change.after
        )
      );
    }
  }
  say(
    "",
    "## The rule",
    "",
    "A count row counts as a skill of a set when the skill is in the set's list at its pin and its copy starts with `<set>:` (a Claude Code Skill call), holds the pin's install directory (`/node_modules/<package>/` or `/vendor/<dir>/`), or holds `/plugins/cache/` and after it a path part that is the set's name (Claude Code's plugin cache): that is its own copy. A copy that is a URL is no install directory: one that names the set's repository or an upstream of it is a read of the upstream by URL, any other another copy. A row with no path and no prefix names no copy; any other path or prefix is another copy. A read that two files of a store hold (a forked session copies its parent's lines) counts in the file whose hash sorts first, whichever queries hold the two, as the attribute command counts it. A session counts once for a skill, in the first of these columns that one of its reads reaches: own copy, upstream by URL, only another copy, no copy named. Reference reads at the own copy are a column of their own.",
    "",
    "## Sessions per skill",
    "",
    tableRow("Set", "Skill", ...COLUMNS.map((column) => COLUMN_WORDS[column])),
    `| --- | --- |${" ---: |".repeat(COLUMNS.length)}`
  );
  for (const row of report.skills) {
    say(tableRow(row.set, row.skill, ...COLUMNS.map((column) => row.totals[column])));
  }
  say(
    "",
    "## Sessions per skill and store",
    "",
    "Each skill and store with a session.",
    "",
    tableRow("Set", "Skill", "Store", ...COLUMNS.map((column) => COLUMN_WORDS[column])),
    `| --- | --- | --- |${" ---: |".repeat(COLUMNS.length)}`
  );
  for (const row of report.skills) {
    for (const [store, numbers] of Object.entries(row.byStore)) {
      if (COLUMNS.every((column) => numbers[column] === 0)) continue;
      say(tableRow(row.set, row.skill, store, ...COLUMNS.map((column) => numbers[column])));
    }
  }
  say("", `## Skills with at least ${AT_LEAST} own-copy sessions`, "");
  if (report.atLeast.length === 0) say("None.");
  for (const row of report.atLeast) say(`- ${row.set} ${row.skill}: ${row.totals.own} sessions`);
  say(
    "",
    "## Commands",
    "",
    "The count-sql command of each query the rows hold, and of each store's roots.txt; its output, run through the configuration's psqlCommand with the store's name, is that file under the rows directory:",
    "",
    "```",
    ...report.commands.countSql,
    "```",
    "",
    "This count, at the same pins:",
    "",
    "```",
    report.commands.setCount,
    "```",
    ""
  );
  return lines.join("\n");
}

/** exemplar-count.tsv's text. */
function table(report: Report): string {
  const stores = report.config.stores.map((store) => store.name);
  const columns = [
    ...TSV_COLUMNS,
    ...stores.flatMap((store) => COLUMNS.map((column) => `${column}_sessions:${store}`)),
  ];
  return tsv(
    columns,
    report.skills.map((row) => [
      row.set,
      row.skill,
      ...COLUMNS.map((column) => row.totals[column]),
      ...stores.flatMap((store) => COLUMNS.map((column) => row.byStore[store]?.[column] ?? 0)),
    ])
  );
}

/** summary.json's value. */
function summary(report: Report): unknown {
  return {
    window: { start: report.config.windowStart, end: report.config.windowEnd },
    rows: { label: report.rowsLabel ?? null, digest: report.digest, files: report.files },
    stores: report.stores,
    notRead: report.notRead,
    dotfiles: {
      repo: report.dotfilesRepo,
      commit: report.pins.commit,
      from: report.pins.commitFrom,
    },
    pins: report.pins.sets.map((set) => ({
      set: set.id,
      repo: set.repo,
      upstreams: set.upstreams,
      manifest: set.pin.manifest,
      line: set.pin.line,
      pin: set.pin.text,
      ref: set.pin.ref,
      commit: set.commit,
      resolvedBy: set.resolvedBy,
      skillsFrom: set.skillsFrom,
      skills: set.skills.length,
    })),
    pinChanges: report.changes,
    skills: report.skills.map((row) => ({
      set: row.set,
      skill: row.skill,
      ...row.totals,
      byStore: row.byStore,
    })),
    atLeast20: report.atLeast.map((row) => ({
      set: row.set,
      skill: row.skill,
      own: row.totals.own,
    })),
    commands: report.commands,
  };
}

/** What `main` uses of the world. */
export interface SetCountIo {
  out(text: string): void;
  /** Writes one line to standard error. */
  err(line: string): void;
  readonly gh: Gh;
  /** The wait before a gh call is sent again; Bun.sleep unless given. */
  sleep?(ms: number): Promise<void>;
}

/** The command. Returns the exit status: 0 (the count is written), 1 (failed), 2 (refused). */
export async function main(argv: readonly string[], io: SetCountIo): Promise<number> {
  try {
    const values = parse(
      argv,
      {
        config: { type: "string" },
        rows: { type: "string" },
        spans: { type: "string" },
        out: { type: "string" },
        "dotfiles-commit": { type: "string" },
        "rows-label": { type: "string" },
        "not-read": { type: "string", multiple: true },
      },
      USAGE
    );
    const given = required(values, ["config", "rows", "spans", "out"], USAGE);
    const config = configFrom(given.config);
    const { sets, dotfilesRepo } = config;
    if (sets === undefined || sets.length === 0 || dotfilesRepo === undefined) {
      throw new Refused(
        `the configuration ${given.config} gives no sets, so there is nothing to count`
      );
    }
    const commit = values["dotfiles-commit"];
    if (commit !== undefined && !COMMIT.test(commit)) {
      throw new Refused(`--dotfiles-commit ${commit} is not a commit's 40 hex digits`);
    }
    const notRead = (values["not-read"] ?? []).map((item) => {
      const at = item.indexOf("=");
      const name = item.slice(0, Math.max(at, 0)).trim();
      const reason = item.slice(at + 1).trim();
      if (at < 0 || name === "" || reason === "") {
        throw new Refused(`--not-read ${JSON.stringify(item)} is not <name>=<reason>`);
      }
      return { name, reason };
    });
    if (inside(given.out, given.rows)) {
      throw new Refused(`--out ${given.out} is inside --rows ${given.rows}, which is only read`);
    }
    const storeNames = config.stores.map((store) => store.name);
    let count: CountRead;
    let spans: Span[];
    try {
      count = readCount(given.rows, storeNames);
      spans = parseSpans(readFileSync(given.spans, "utf8"), given.spans);
    } catch (error) {
      throw new Refused(messageFor(error));
    }
    const gaps = windowGaps(config, count);
    if (gaps.length > 0) {
      throw new Refused(
        `the rows lack a summary row of the window [${config.windowStart}, ${config.windowEnd}), so nothing is counted:\n  ${gaps.join("\n  ")}`
      );
    }

    const reads = new SetReads(io.gh, commandRetries(io, "set-count"));
    // The pin changes need nothing of the pins: both are read at once, the pins' failure first.
    const pinsRead = started(readPins(reads, sets, dotfilesRepo, commit));
    const changesRead = started(
      pinChanges(reads, sets, dotfilesRepo, config.windowStart, config.windowEnd)
    );
    const pins = await pinsRead;
    const changes = await changesRead;
    // A read that a fork and its parent both hold counts in one session, as attribute counts it.
    const once = countOnce(count.outputs).outputs;
    const counts = countSets(setRules(pins.sets), once);

    const outputs = [...count.outputs].sort((a, b) => {
      const [x, y] = [`${a.store}/${a.name}`, `${b.store}/${b.name}`];
      return x < y ? -1 : x > y ? 1 : 0;
    });
    // Each file's digest is of the bytes readCount parsed, so of the rows counted.
    const files = outputs.map((output) => {
      const file = `${output.store}/${output.name}.txt`;
      const digest = count.digests.get(file);
      if (digest === undefined) throw new Error(`the rows file ${file} has no digest`);
      return { file, sha256: digest };
    });
    const stores = config.stores
      .filter(({ name }) => count.outputs.some((output) => output.store === name))
      .map(({ name, context }): StoreRead => {
        const span = spans.find((each) => each.store === name);
        const mine = once.filter((output) => output.store === name);
        const sessions = new Set(
          mine.flatMap((output) =>
            output.rows
              .filter((row) => row.kind === "read")
              .flatMap((row) => row.hashes.map((hash) => sessionKey(name, hash)))
          )
        );
        return {
          store: name,
          context,
          earliestRecord:
            span === undefined
              ? "not in the spans file"
              : span.earliest_mtime_ms === null
                ? "no record"
                : iso(span.earliest_mtime_ms),
          filesInStore: span?.files ?? null,
          queries: mine.length,
          sessionsWithRead: sessions.size,
        };
      });
    const skills = skillRows(
      counts,
      sets.map((set) => set.id),
      storeNames
    );
    const ownCommand = [
      CLI,
      "set-count",
      ...argv,
      ...(commit === undefined ? ["--dotfiles-commit", pins.commit] : []),
    ].map((word, index) => (index < 2 ? word : shellWord(word)));
    const report: Report = {
      config,
      rowsLabel: values["rows-label"],
      files,
      digest: sha256(files.map((file) => `${file.sha256}  ${file.file}\n`).join("")),
      stores,
      otherDirs: count.otherStores,
      notRead: [
        ...storeNames
          .filter((name) => !stores.some((store) => store.store === name))
          .map((name) => ({ name, reason: "the rows hold no output of this store" })),
        ...notRead,
      ],
      dotfilesRepo,
      pins,
      changes,
      skills,
      atLeast: skills
        .filter((row) => row.totals.own >= AT_LEAST)
        .sort((a, b) => b.totals.own - a.totals.own),
      commands: {
        // Each store's day and hour queries, then its roots query: the files the rows hold, in
        // the order of their paths.
        countSql: [...new Set(outputs.map((output) => output.store))].flatMap((store) => [
          ...outputs
            .filter((output) => output.store === store)
            .map((output) => {
              const [day = "", hour] = output.name.split("T");
              const query = [
                "--day",
                day,
                ...(hour === undefined ? [] : ["--hour", String(Number(hour))]),
              ];
              return countSqlLine(given.config, config.psqlCommand, store, query, output.name);
            }),
          countSqlLine(given.config, config.psqlCommand, store, ["--query", "roots"], "roots"),
        ]),
        setCount: ownCommand.join(" "),
      },
    };

    const written: string[] = [];
    const write = (name: string, text: string) => {
      writeWhole(path.join(given.out, name), text);
      written.push(path.join(given.out, name));
    };
    write("exemplar-count.md", markdown(report));
    write("exemplar-count.tsv", table(report));
    for (const row of skills) {
      write(path.join("sessions", row.set, `${row.skill}.jsonl`), sessionLines(row.own));
    }
    write("summary.json", json(summary(report)));
    io.out(
      `set-count: ${outputs.length} queries of ${stores.length} stores; ${skills.length} skills of ${pins.sets.length} sets at ${dotfilesRepo} ${pins.commit}; ${report.atLeast.length} with at least ${AT_LEAST} own-copy sessions; ${changes.length} pin changes in the window\n` +
        written
          .filter((file) => !file.includes(`${path.sep}sessions${path.sep}`))
          .map((file) => `${file}\n`)
          .join("") +
        `${path.join(given.out, "sessions")}/ (${skills.length} files)\n`
    );
    return 0;
  } catch (error) {
    if (error instanceof Refused) {
      io.err(`set-count: ${error.message}`);
      return 2;
    }
    io.err(`set-count: ${messageFor(error)}`);
    return 1;
  }
}
