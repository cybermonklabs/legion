/** The browser suite's quarantine (R4 of dispatch://INFRA-983): the rows that still run but gate
 *  nothing, each owned by an issue.
 *
 * A row the merge job of `.github/workflows/dispatch-browser.yaml` names flaky in two runs of 14
 * days is a defect, and while its owner works on it the row leaves the runs that gate a change -
 * the pull-request lane and the merge queue's full lane, both the shard job's - and keeps running
 * in the nightly's quarantine leg, whose result is reported and fails nothing. So the owner sees
 * whether a fix worked, and nobody else's change is held by it. `quarantine` below is the list,
 * and it holds at most `quarantineLimit` rows.
 *
 * Three readers hold the list to that:
 *
 * - `e2e/playwright.config.ts` passes every project through `quarantined`, which adds one
 *   `grepInvert` pattern to it: under `DISPATCH_E2E_QUARANTINE=exclude` the pattern drops the
 *   quarantined rows, under `only` it drops every other row, and with the variable unset the
 *   project is returned as it is, so a listing or a run that names no such mode reads the whole
 *   suite. The shard job's browser step sets `exclude`; the nightly's quarantine leg sets `only`.
 *   The embedder job sets neither, so it runs a quarantined row of its files as it runs the rest.
 * - `e2e/__tests__/quarantine.test.ts` refuses a list of more than `quarantineLimit` rows, an
 *   entry with no issue or no date or a malformed one, an entry of a file the embedder job runs,
 *   and an entry whose row the suite does not run. It never compares a date with today: a pull
 *   request is no place for a check that turns red on a day nobody chose.
 * - `e2e/tools/quarantine-check.ts`, which the nightly runs, does what depends on the date: it
 *   names a row flaky in two runs of 14 days that is not here, and an entry held longer than 14
 *   days, each with the exact lines to add or to remove.
 *
 * This module imports nothing, so the count step of the `quarantine-rows` job of
 * .github/workflows/dispatch-browser.yaml reads it with Bun alone, before any install or build.
 * The tool needs both, for the listing it reads, and its job makes them.
 */

/** One quarantined row, named as the merge job's `dispatch-browser flaky row` notice names it:
 *  `<file>:<line> [<project>] <title>`. The line is not part of it, because it moves with every
 *  edit above the row; the file, the project and the full title are the row. */
export interface QuarantineEntry {
  /** The spec file, relative to `e2e/`, as the notice prints it: `pictures.e2e.ts`. */
  readonly file: string;
  /** The project the row was flaky in, as the notice prints it between the brackets. */
  readonly project: string;
  /** The row's full title as the notice prints it: its `describe` titles first, each followed by
   *  ` › `, then its own title with its tags. */
  readonly title: string;
  /** The issue that owns the row, as `dispatch://<KEY>`. */
  readonly issue: string;
  /** The day the row came in, `YYYY-MM-DD`. */
  readonly added: string;
}

/** The quarantined rows. An entry is written as `entryLines` prints it, which is what
 *  `e2e/tools/quarantine-check.ts` names as the lines to add or to remove. */
export const quarantine: readonly QuarantineEntry[] = [
  {
    file: "mention-composer.e2e.ts",
    project: "chromium",
    title:
      "a duplicated mention delivery says the listener already had it, and offers no safe retry",
    issue: "dispatch://INFRA-3904",
    added: "2026-11-19",
  },
  {
    file: "board.e2e.ts",
    project: "chromium",
    title:
      "picker keeps a pick on screen: a card dragged to another priority lane while its pick is out stays in that lane",
    issue: "dispatch://INFRA-3904",
    added: "2026-11-20",
  },
];

/** A row a commit on main fixed, which a quarantine entry is not: the row stays in every gating
 *  lane, and the nightly's check counts only the flakes of runs created after `fixed` landed, so
 *  a row flaky twice before its fix does not ask for an entry after it. Once the commit is older
 *  than the check's 14 days every flake it reads is after the fix, so the entry does nothing,
 *  and the check names it for its owner to delete. The check fails on a fix whose commit is not
 *  on main. */
export interface FixEntry {
  readonly file: string;
  readonly project: string;
  readonly title: string;
  /** The issue that owns the row, as `dispatch://<KEY>`. */
  readonly issue: string;
  /** The commit on main that fixed the row: its full 40-character sha, lower case. */
  readonly fixed: string;
}

/** The fixed rows, each written as `entryLines` prints it. */
export const fixes: readonly FixEntry[] = [
  {
    file: "agent-rail.e2e.ts",
    project: "iphone",
    title: "the rail row, the Agents row and the live view each unpin @phone",
    issue: "dispatch://INFRA-3148",
    fixed: "a5348c75fa42c05f5581df45abaf33877c9ce90b",
  },
  {
    file: "close-withdraws-asks.e2e.ts",
    project: "chromium",
    title:
      "a person's Close withdraws the issue's asks, an earlier version keeps its open block, and Reopen brings the asks back @pr",
    issue: "dispatch://INFRA-3904",
    fixed: "708ca88c523acb412bc3b7a52e0ba9fcf5642516",
  },
  {
    file: "project.e2e.ts",
    project: "chromium",
    title:
      "picker keeps a pick on screen: the repository on the issue page, over a read that answers while its save is out",
    issue: "dispatch://INFRA-3904",
    fixed: "c6e58d48fb9efaa5255e557b1aeb57580ed9ce9f",
  },
];

/** How many rows the quarantine may hold (R4 of dispatch://INFRA-983). A fix quarantines nothing,
 *  so `fixes` is not counted. */
export const quarantineLimit = 10;

/** An entry as a file may hold one, before anything has checked it: every field may be missing
 *  or of any type. It is what `quarantineFaults` and `fixFaults` read, so a fixture can plant the
 *  entry the types above would refuse to compile. */
export type UncheckedEntry = {
  readonly [Field in keyof QuarantineEntry | keyof FixEntry]?: unknown;
};

/** What a run asks of the quarantine, from `DISPATCH_E2E_QUARANTINE`: `all` (unset) runs every
 *  row, `exclude` leaves the quarantined rows out, and `only` runs them and nothing else. */
export type QuarantineRun = "all" | "exclude" | "only";

/** What joins a row's `describe` titles to its own, so two rows of one file whose own titles
 *  match but whose blocks differ stay two rows. Playwright's own reports read this way, and so
 *  does every reader here: `rowsOfListing` of `e2e/configurations.ts` and
 *  `e2e/tools/quarantine-check.ts` join a listing's titles with it, and the merge job's
 *  "Reading two" step prints the notice an entry is written from with the same characters. */
export const titleSeparator = " › ";

const issuePattern = /^dispatch:\/\/[A-Z][A-Z0-9]*-[1-9][0-9]*$/;
const datePattern = /^(\d{4})-(\d{2})-(\d{2})$/;

/** The run `value` (`DISPATCH_E2E_QUARANTINE`) asks for, or a refusal naming the three values:
 *  a misspelled mode read as "every row" would put a quarantined row back into a gating run with
 *  nothing to say so. */
export function quarantineRun(value: string | undefined): QuarantineRun {
  if (value === undefined || value === "") return "all";
  if (value === "exclude" || value === "only") return value;
  throw new Error(
    `DISPATCH_E2E_QUARANTINE must be "exclude", "only" or unset, not ${JSON.stringify(value)}. ` +
      "Unset runs every row; `exclude` leaves the rows of e2e/quarantine.ts out, which is what " +
      "the gating lanes run; `only` runs those rows and no other, which is the nightly's " +
      "quarantine leg."
  );
}

/** A row as the quarantine names it: the fields of `QuarantineEntry` that are the row. */
export type QuarantineRow = Pick<QuarantineEntry, "file" | "project" | "title">;

/** `row` as the merge job's notice prints it, without the line: `<file> [<project>] <title>`.
 *  It is what an entry, a listed row and a notice are compared by. */
export function rowName(row: QuarantineRow): string {
  return `${row.file} [${row.project}] ${row.title}`;
}

/** The string Playwright 1.64.1 matches a project's `grep` and `grepInvert` against, for one row:
 *  the project's name, the file's path relative to the config's root, each `describe` title and
 *  the row's own title, joined by single spaces. `createProjectSuite`
 *  (node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:2644-2657)
 *  matches `test._grepTitleWithTags()`, which `_grepBaseTitlePath` builds from
 *  `Suite._collectGrepTitlePath` (playwright/lib/common/index.js:2623-2629 and 2845-2855) over
 *  a project suite that has no parent yet. A tag given in a row's `{ tag }` option would follow
 *  its title there; this suite writes its tags into the title, so the title carries them. */
function grepTitle(row: QuarantineRow): string {
  return [row.project, row.file, ...row.title.split(titleSeparator)].join(" ");
}

/** `project` with the quarantine applied for `run`: under `exclude` one more `grepInvert` pattern
 *  that matches the grep title of each entry's row and nothing else - anchored at both ends, so a
 *  row whose title ends with an entry's, a row of another project and a row of another file all
 *  stay - and under `only` one that matches every grep title but those. The project's own `grep`
 *  and `grepInvert` stay in force, so `only` runs a quarantined row only in a project that
 *  selects it. Under `all`, and under `exclude` with no entry, it is `project` itself. */
export function quarantined<
  Project extends { readonly name: string; readonly grepInvert?: RegExp | RegExp[] },
>(project: Project, entries: readonly QuarantineEntry[], run: QuarantineRun): Project {
  if (run === "all" || (run === "exclude" && entries.length === 0)) return project;
  const rows = entries
    .map((entry) => grepTitle(entry).replace(/[.*+?^${}()|[\]\\/]/g, "\\$&"))
    .join("|");
  // With no entry, `only` runs nothing: the empty pattern matches every grep title.
  const invert =
    run === "exclude"
      ? new RegExp(`^(?:${rows})$`)
      : entries.length === 0
        ? /(?:)/
        : new RegExp(`^(?!(?:${rows})$)`);
  const own =
    project.grepInvert === undefined
      ? []
      : Array.isArray(project.grepInvert)
        ? project.grepInvert
        : [project.grepInvert];
  return { ...project, grepInvert: [...own, invert] };
}

function realDay(value: string): boolean {
  const parts = datePattern.exec(value);
  if (parts === null) return false;
  const [year, month, day] = parts.slice(1).map(Number);
  const date = new Date(Date.UTC(year, month - 1, day));
  return (
    date.getUTCFullYear() === year && date.getUTCMonth() === month - 1 && date.getUTCDate() === day
  );
}

/** Each of `entries` as `quarantineFaults` and `fixFaults` read it: its `name`, `<noun> <n>`
 *  followed by its row where the file, the project and the title are all strings, and that `row`;
 *  a sentence for each of those three fields that is missing or empty (`unnamed`); and whether an
 *  entry above it names the same three (`repeated`). */
function readEntries(
  entries: readonly UncheckedEntry[],
  noun: "entry" | "fix"
): {
  entry: UncheckedEntry;
  name: string;
  repeated: boolean;
  row: QuarantineRow | undefined;
  unnamed: string[];
}[] {
  const seen = new Set<string>();
  return entries.map((entry, index) => {
    const { file, project, title } = entry;
    const row =
      typeof file === "string" && typeof project === "string" && typeof title === "string"
        ? { file, project, title }
        : undefined;
    const name =
      row === undefined ? `${noun} ${index + 1}` : `${noun} ${index + 1} (${rowName(row)})`;
    const unnamed = (["file", "project", "title"] as const)
      .filter((field) => typeof entry[field] !== "string" || entry[field] === "")
      .map((field) => `${name} names no ${field}, and the row is its file, project and title`);
    const key = JSON.stringify([file, project, title]);
    const repeated = seen.has(key);
    seen.add(key);
    return { entry, name, repeated, row, unnamed };
  });
}

/** What is wrong with `entries` as a quarantine, one sentence per fault, each naming the entry:
 *  more than `quarantineLimit` of them, a missing or empty file, project or title, a missing or
 *  malformed issue or date, an entry repeated, a project outside `projects` - the projects a
 *  flaky row can come from, which are the ones that retry a failed row on CI and run in the
 *  nightly's quarantine leg (`e2e/__tests__/quarantine.test.ts` reads them from the config) -
 *  and a file of `embedderFiles`, the files `bun run e2e:embedder` runs. That is the embedder
 *  job of .github/workflows/dispatch-browser.yaml, which runs on both lanes and sets no
 *  `DISPATCH_E2E_QUARANTINE`, and the rows of those files skip wherever the server has no fake
 *  embedder, the nightly's quarantine leg among them (`e2e/__tests__/quarantine.test.ts` reads
 *  the files from a listing of that script's command). It reads no date against today. */
export function quarantineFaults(
  entries: readonly UncheckedEntry[],
  projects: readonly string[],
  embedderFiles: readonly string[]
): string[] {
  const faults: string[] = [];
  if (entries.length > quarantineLimit) {
    faults.push(
      `the quarantine holds ${entries.length} rows, and R4 of dispatch://INFRA-983 allows ` +
        `${quarantineLimit}: fix a quarantined row and take its entry out before another goes in`
    );
  }
  for (const { entry, name, repeated, unnamed } of readEntries(entries, "entry")) {
    const { file, project } = entry;
    faults.push(...unnamed);
    if (entry.issue === undefined) {
      faults.push(`${name} has no issue: every quarantined row is owned by one, dispatch://<KEY>`);
    } else if (typeof entry.issue !== "string" || !issuePattern.test(entry.issue)) {
      faults.push(
        `${name} names its issue as ${JSON.stringify(entry.issue)}, which is no ` +
          "dispatch://<KEY> reference such as dispatch://INFRA-983"
      );
    }
    if (entry.added === undefined) {
      faults.push(`${name} has no date: write the day the row came in as added: "YYYY-MM-DD"`);
    } else if (typeof entry.added !== "string" || !realDay(entry.added)) {
      faults.push(
        `${name} gives the day it came in as ${JSON.stringify(entry.added)}, which is no ` +
          "calendar day written YYYY-MM-DD"
      );
    }
    if (typeof project === "string" && project !== "" && !projects.includes(project)) {
      faults.push(
        `${name} names the project ${project}, whose rows CI never reports flaky and the ` +
          `nightly's quarantine leg does not run; a quarantined row is one of ${projects.join(", ")}`
      );
    }
    if (typeof file === "string" && embedderFiles.includes(file)) {
      faults.push(
        `${name} names the file ${file}, which the embedder job runs on both lanes with no ` +
          "quarantine and the nightly's quarantine leg skips, having no embedder: the entry " +
          "would leave the row gating as before and run it ungated nowhere"
      );
    }
    if (repeated) faults.push(`${name} repeats an entry above it`);
  }
  return faults;
}

/** What is wrong with `entries` as the list of fixed rows, one sentence per fault: a missing or
 *  empty file, project or title, a missing or malformed issue, a `fixed` that is not a full
 *  40-character lower-case sha, a project outside `projects` (as `quarantineFaults` reads it),
 *  an entry repeated, and a row both fixed and quarantined. Whether the commit is on main is the
 *  nightly's to read, through GitHub. */
export function fixFaults(
  entries: readonly UncheckedEntry[],
  projects: readonly string[],
  quarantined: readonly QuarantineEntry[]
): string[] {
  const held = new Set(quarantined.map(rowName));
  const faults: string[] = [];
  for (const { entry, name, repeated, row, unnamed } of readEntries(entries, "fix")) {
    const { project } = entry;
    faults.push(...unnamed);
    if (typeof entry.issue !== "string" || !issuePattern.test(entry.issue)) {
      faults.push(
        `${name} names its issue as ${JSON.stringify(entry.issue)}, which is no ` +
          "dispatch://<KEY> reference such as dispatch://INFRA-983"
      );
    }
    if (typeof entry.fixed !== "string" || !/^[0-9a-f]{40}$/.test(entry.fixed)) {
      faults.push(
        `${name} names its fix as ${JSON.stringify(entry.fixed)}, which is no full ` +
          "40-character lower-case commit sha"
      );
    }
    if (typeof project === "string" && project !== "" && !projects.includes(project)) {
      faults.push(
        `${name} names the project ${project}, whose rows CI never reports flaky; a fixed row ` +
          `is one of ${projects.join(", ")}`
      );
    }
    if (row !== undefined && held.has(rowName(row))) {
      faults.push(`${name} is quarantined too: a row is fixed or quarantined, not both`);
    }
    if (repeated) faults.push(`${name} repeats a fix above it`);
  }
  return faults;
}

/** What is wrong with the rows a listing under `DISPATCH_E2E_QUARANTINE=only` selected, against
 *  the entries it was listed with: an entry whose row it did not select - the suite has no such
 *  row in that project, under that file and title - and a selected row no entry names, which is a
 *  row whose grep title another row shares (Playwright joins a row's titles with spaces, so
 *  `a › b c` and `a b › c` read alike there). */
export function selectionFaults(
  entries: readonly QuarantineEntry[],
  selected: readonly QuarantineRow[]
): string[] {
  const named = new Set(entries.map(rowName));
  const listed = new Set(selected.map(rowName));
  const faults: string[] = [];
  for (const entry of entries) {
    if (!listed.has(rowName(entry))) {
      faults.push(
        `${rowName(entry)} is a row the suite does not run: no row of ${entry.project} has ` +
          "that file and that full title, so it was renamed, moved or deleted, or its project " +
          "no longer selects it; take the entry out, or write the row's title as the listing " +
          "names it"
      );
    }
  }
  for (const row of selected) {
    if (!named.has(rowName(row))) {
      faults.push(
        `the quarantine selects ${rowName(row)}, which no entry names: its grep title is a ` +
          "quarantined row's, so Playwright cannot tell the two apart"
      );
    }
  }
  return faults;
}

/** `entry` as `quarantine` or `fixes` above holds it, in the layout Biome keeps: one field per
 *  line, and a field whose line would pass biome.json's `lineWidth` of 100 with its value on the
 *  line after its key, which is where Biome breaks such a property. So the lines
 *  `e2e/tools/quarantine-check.ts` names to add or to remove are the lines the file holds, and
 *  `e2e/__tests__/quarantine.test.ts` holds every real entry to them. */
export function entryLines(entry: QuarantineEntry | FixEntry): string[] {
  const last =
    "added" in entry ? (["added", entry.added] as const) : (["fixed", entry.fixed] as const);
  const fields = [
    ["file", entry.file],
    ["project", entry.project],
    ["title", entry.title],
    ["issue", entry.issue],
    last,
  ].flatMap(([field, value]) => {
    const line = `    ${field}: ${JSON.stringify(value)},`;
    return line.length <= 100 ? [line] : [`    ${field}:`, `      ${JSON.stringify(value)},`];
  });
  return ["  {", ...fields, "  },"];
}
