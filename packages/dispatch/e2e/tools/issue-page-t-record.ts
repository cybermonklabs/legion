/** `bun e2e/tools/issue-page-t-record.ts --recorded-on <commit> [--part <commit> ...]`:
 *  records the rows the new issue page draws from its reads into
 *  `e2e/fixtures/issue-page-t-rows.json`, the fixture the rows of the
 *  `e2e/issue-page-t-*.e2e.ts` files compare the tree read's rows with.
 *
 *  What it records. For each of the 14 seeds of `e2e/issue-layout-seed.ts`, in the order that
 *  file writes them, on an emptied database, read by alice with the new page on at 1440 by 900
 *  (`phase`):
 *
 *  - `loadRequests`: the requests under `/api/` the page sent from the navigation until it was
 *    quiet and drew the same rows twice with no request sent between (`watchApiRequests`,
 *    `expectLoadSettled`);
 *  - `opened`: every row as the page opens, folded as it opens, with no body;
 *  - `rows`: every row once every branch is open (`unfoldEveryBranch`), each with the body it
 *    draws while it has the focus;
 *  - `filters`: for Spec, Verification, Plan and Decisions, the keys of the rows the filter
 *    draws, in drawing order, and the count its control draws.
 *
 *  Each row is named by what the seed returned, or a check or a plan task no field names by the
 *  block id the seed wrote (`seedKeys` and `keyedRows`), never by an id the server minted, and
 *  its time by the read it came from, never as an instant. The recorder fails rather than write
 *  a UUID or an instant, and it fails where two rows of the outline have the same key and the
 *  same parent key, since a comparison by key could not tell them apart.
 *
 *  On which commit. The recording base: a commit that holds every part of the new page this
 *  branch holds, and neither the model's tree read nor any commit of this branch after R, the
 *  commit that adds this file. The first base is R. `--recorded-on` names the base, and each
 *  `--part` the commit the base holds of one part of the page that `main` does not hold; give
 *  one `--part` for each such part, and none for a part `main` holds. The fixture lists those
 *  commits sorted, as `parts`. The recorder runs no git, so it writes the commits it is given.
 *  `ISSUE_PAGE_T_RECORDED_ON` and `ISSUE_PAGE_T_PARTS` (a comma-separated list of commits) give
 *  the same values, and a flag wins over its variable.
 *
 *  How to run it, from `packages/dispatch` of a worktree at the base whose `packages/contracts`
 *  and dashboard are built (`bun run build:web`):
 *
 *    bun e2e/harness-up.ts > "$TMPDIR/t-harness.sh" &
 *    …wait for its last line, `export DISPATCH_E2E_REUSE_SERVERS=1`…
 *    . "$TMPDIR/t-harness.sh" && bun e2e/tools/issue-page-t-record.ts --recorded-on <commit> \
 *      --part <commit> ...
 *    …then stop the harness by its own process id.
 *
 *  It empties the database `DATABASE_URL` names before each seed, so it runs only where
 *  `DISPATCH_E2E_REUSE_SERVERS=1` says that the variables are a stack's that `e2e/harness-up.ts`
 *  started; it starts no stack of its own.
 *
 *  Why it runs Playwright's test runner. The seeds call the runner's own API (the fake GitHub's
 *  `seedFakeGithub` calls `test.skip`), which throws outside a test. So the command checks its
 *  arguments, writes a Playwright config of its own into a new directory under `$TMPDIR`, and runs
 *  `playwright test` on this same file with that config: loaded by the runner, the file declares
 *  one test, which does the recording and hands the seeds' records back through a file in that
 *  directory. The command then writes the fixture and deletes the directory. The config is not a
 *  file of the suite, so the suite's projects, its global setup and its fixtures stay out of it;
 *  it carries the suite's own settings a recording reads by: its origin, its desktop browser, its
 *  expect and action timeouts, one worker and no retry.
 *
 *  When to run it again: once `main` is merged forward for the last time before this branch
 *  merges, and whenever a part this branch holds changes what the page draws from its reads,
 *  what the seeds write, what the page reads at load or what a filter draws. So that two runs on
 *  one base write the same bytes, every object's fields are sorted, and the file is written as
 *  Biome formats it, which is also what `bunx biome check .` requires of it.
 */
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { type Browser, devices, type Page, test } from "@playwright/test";

import { PRESET_NAMES, PRESET_ORDER } from "../../web/src/features/issue-page/page-words";
import type { Filter } from "../../web/src/features/issue-page/presets";
import { baseUrl, getIssue } from "../api";
import { gotoIssue, presetCount, presetToggle } from "../issue-page";
import { DESKTOP, expectIssuePageDrawn, phase, unfoldEveryBranch } from "../issue-page-g";
import { expectActivePreset, pickPreset } from "../issue-page-i";
import {
  drawnRowReadings,
  FIXTURE_FILE,
  type FilterFixture,
  type KeyedRow,
  keyedRows,
  type RowReading,
  SEEDS,
  type SeededIssue,
  type SeedFixture,
  seedKeys,
  sortedFields,
  steadily,
} from "../issue-page-t";
import { resetDatabase } from "../seed";
import { type ApiRequests, watchApiRequests } from "../traffic";

const usage =
  "usage: bun e2e/tools/issue-page-t-record.ts --recorded-on <commit> [--part <commit> ...]\n" +
  "  --recorded-on  the commit the stack was built from, which holds every part of the new page\n" +
  "                 this branch holds and not the model (else ISSUE_PAGE_T_RECORDED_ON)\n" +
  "  --part         the commit the base holds of one part of the page main does not hold; once\n" +
  "                 for each such part (else ISSUE_PAGE_T_PARTS, a comma-separated list of\n" +
  "                 commits)";

/** The four filters, in the order the view row draws them (`PRESET_ORDER`). Conversation, the
 *  fifth control, is the page with no filter on. */
const FILTERS = PRESET_ORDER.filter((preset): preset is Filter => preset !== "conversation");

/** The suite's own expect and action timeouts (`e2e/playwright.config.ts`), which the recorder's
 *  config sets for its one test as the suite's config does for every row. */
const EXPECT_MS = 15_000;
const ACTION_MS = 45_000;

/** How long the one test may take: every seed, each with a body read for every row. */
const RECORDING_MS = 60 * 60_000;

/** A commit: the short or the full hash. */
const COMMIT = /^[0-9a-f]{7,64}$/;

/** What the recorder refuses to write: an id the server minted, or a moment in time, both of
 *  which belong to one run, so the next run would write them differently. */
const MINTED: readonly { readonly pattern: RegExp; readonly what: string }[] = [
  { pattern: /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i, what: "a UUID" },
  { pattern: /\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}/, what: "an instant" },
];

/** The variable that names the file the test writes the seeds' records to. */
const OUTPUT_VARIABLE = "ISSUE_PAGE_T_RECORDS";

const thisFile = fileURLToPath(import.meta.url);
const packageRoot = fileURLToPath(new URL("../../", import.meta.url));
const repoRoot = fileURLToPath(new URL("../../../../", import.meta.url));
const fixturePath = fileURLToPath(FIXTURE_FILE);

/** The commits the fixture says it was recorded on: the base, and the commit the base holds of
 *  each part of the page `main` does not hold, sorted. */
interface RecordingBase {
  readonly parts: readonly string[];
  readonly recordedOn: string;
}

/** What one filter drew, before its rows are named. */
interface FilterReading {
  readonly count: string | null;
  readonly filter: Filter;
  readonly rows: readonly RowReading[];
}

/** What one seed's page drew, before its rows are named. */
interface PageReading {
  readonly filters: readonly FilterReading[];
  readonly loadRequests: number;
  readonly opened: readonly RowReading[];
  readonly rows: readonly RowReading[];
}

// ---------------------------------------------------------------------------------------------
// The command: its arguments, the runner it starts, and the file it writes.
// ---------------------------------------------------------------------------------------------

function refuse(message: string): never {
  console.error(`${message}\n\n${usage}`);
  process.exit(2);
}

function givenArguments(): { readonly part?: string[]; readonly "recorded-on"?: string } {
  try {
    return parseArgs({
      args: process.argv.slice(2),
      options: { part: { multiple: true, type: "string" }, "recorded-on": { type: "string" } },
      strict: true,
    }).values;
  } catch (error) {
    return refuse(error instanceof Error ? error.message : String(error));
  }
}

/** The base and its parts, from the flags, else from the variables. */
function recordingBase(): RecordingBase {
  const given = givenArguments();
  const recordedOn = given["recorded-on"] ?? process.env.ISSUE_PAGE_T_RECORDED_ON ?? "";
  if (recordedOn === "") {
    refuse("Name the commit the stack was built from: --recorded-on <commit>.");
  }
  if (!COMMIT.test(recordedOn)) {
    refuse(`The recording base ${JSON.stringify(recordedOn)} is not a commit's hash.`);
  }
  const named =
    given.part ??
    (process.env.ISSUE_PAGE_T_PARTS ?? "")
      .split(",")
      .map((entry) => entry.trim())
      .filter((entry) => entry !== "");
  const parts = new Set<string>();
  for (const commit of named) {
    if (!COMMIT.test(commit)) refuse(`The part ${JSON.stringify(commit)} is not a commit's hash.`);
    if (parts.has(commit)) refuse(`The part ${commit} is named twice.`);
    parts.add(commit);
  }
  return { parts: [...parts].sort(), recordedOn };
}

/** The config the runner is started with: this file as its one test file, and the suite's own
 *  settings for what a recording reads (the paragraph of the head comment on the runner). It is
 *  written as data, since a config outside the package could not import the package's
 *  Playwright. */
function configSource(scratch: string): string {
  const config = {
    expect: { timeout: EXPECT_MS },
    fullyParallel: false,
    outputDir: join(scratch, "test-results"),
    reporter: "list",
    retries: 0,
    testDir: dirname(thisFile),
    testMatch: basename(thisFile),
    timeout: RECORDING_MS,
    use: {
      ...devices["Desktop Chrome"],
      actionTimeout: ACTION_MS,
      baseURL: baseUrl,
      headless: true,
      trace: "off",
      viewport: { height: DESKTOP.height, width: DESKTOP.width },
    },
    workers: 1,
  };
  return `export default ${JSON.stringify(config, null, 2)};\n`;
}

/** `json` as Biome formats it, which is what `bunx biome check .` holds a committed file to:
 *  Biome writes a short list on one line, where `JSON.stringify` gives each item a line. */
function biomeFormatted(json: string): string {
  const biome = Bun.which("biome", {
    PATH: [join(repoRoot, "node_modules/.bin"), process.env.PATH ?? ""].join(":"),
  });
  if (biome === null) {
    throw new Error("The recorder writes the fixture through Biome: run `bun install` first.");
  }
  const formatted = Bun.spawnSync({
    cmd: [biome, "format", `--stdin-file-path=${relative(repoRoot, fixturePath)}`],
    cwd: repoRoot,
    stderr: "inherit",
    stdin: new TextEncoder().encode(json),
  });
  if (formatted.exitCode !== 0) throw new Error("biome format refused the fixture");
  const source = formatted.stdout.toString();
  // Biome answers nothing for a file over its size limit (`files.maxSize`, 1 MiB unless the
  // configuration says otherwise), and `biome check` passes over such a file in the same way,
  // so the file is then written as `JSON.stringify` wrote it.
  return source === "" ? json : source;
}

/** The command: checks the arguments, runs the recording in the runner, writes the fixture, and
 *  answers the exit status. */
async function main(): Promise<number> {
  const base = recordingBase();
  if (process.env.DISPATCH_E2E_REUSE_SERVERS !== "1") {
    console.error(
      "issue-page-t-record.ts reads a stack that is already running, and empties its database.\n" +
        "Start one from packages/dispatch with\n" +
        '  bun e2e/harness-up.ts > "$TMPDIR/t-harness.sh" &\n' +
        "wait for its last line, `export DISPATCH_E2E_REUSE_SERVERS=1`, then run\n" +
        '  . "$TMPDIR/t-harness.sh" && bun e2e/tools/issue-page-t-record.ts ...'
    );
    return 2;
  }
  const scratch = mkdtempSync(join(tmpdir(), "issue-page-t-record-"));
  try {
    const config = join(scratch, "playwright.config.mjs");
    const records = join(scratch, "records.json");
    writeFileSync(config, configSource(scratch));
    // The package's own `e2e` script runs the same `playwright test`.
    const runner = Bun.spawn(["bunx", "playwright", "test", "--config", config], {
      cwd: packageRoot,
      env: { ...process.env, [OUTPUT_VARIABLE]: records },
      stdio: ["inherit", "inherit", "inherit"],
    });
    const status = await runner.exited;
    // A runner a signal ended answers no status of its own, and still recorded nothing.
    if (status !== 0 || runner.signalCode !== null) return status === 0 ? 1 : status;
    const seeds: unknown = JSON.parse(readFileSync(records, "utf8"));
    const fixture = sortedFields({ parts: base.parts, recordedOn: base.recordedOn, seeds });
    mkdirSync(dirname(fixturePath), { recursive: true });
    writeFileSync(fixturePath, biomeFormatted(`${JSON.stringify(fixture, null, 2)}\n`));
    console.error(`wrote ${relative(process.cwd(), fixturePath)}`);
    return 0;
  } finally {
    rmSync(scratch, { force: true, recursive: true });
  }
}

// ---------------------------------------------------------------------------------------------
// The recording, which runs inside the runner.
// ---------------------------------------------------------------------------------------------

/** Turns `filter` on, reads what it draws once it is steady, and turns it off again, which puts
 *  the page back on Conversation. */
async function readFilter(
  page: Page,
  traffic: ApiRequests,
  filter: Filter
): Promise<FilterReading> {
  const name = PRESET_NAMES[filter];
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  const drawn = await steadily(page, traffic, `the ${name} filter`, async () => ({
    count: await presetCount(page, filter).textContent(),
    rows: await drawnRowReadings(page, { bodies: false }),
  }));
  // Pressing the control that is on turns it off (`IssuePresets.tsx`'s `press`).
  await presetToggle(page, filter).click();
  await expectActivePreset(page, PRESET_NAMES.conversation);
  return { ...drawn, filter };
}

/** Everything the recorder reads of issue `key`'s page, in the order a reader reaches it. */
async function readPage(page: Page, key: string): Promise<PageReading> {
  const traffic = watchApiRequests(page);
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  // The load's requests are counted with the rows, once two readings of both agree, and not
  // the first time the page goes quiet: a read the page sends only after a long render can
  // start after a quiet window, and one recording of `seedWholeIssueAdvancing` counted 17
  // requests where every other load of that seed counted 18.
  const { loadRequests, opened } = await steadily(
    page,
    traffic,
    `${key} as the page opens`,
    async () => ({
      loadRequests: traffic.total(),
      opened: await drawnRowReadings(page, { bodies: false }),
    })
  );
  await unfoldEveryBranch(page);
  await steadily(page, traffic, `${key} with every branch open`, () =>
    drawnRowReadings(page, { bodies: false })
  );
  const rows = await drawnRowReadings(page);
  const filters: FilterReading[] = [];
  for (const filter of FILTERS) filters.push(await readFilter(page, traffic, filter));
  return { filters, loadRequests, opened, rows };
}

/** The number a filter's control draws as its count. */
function countOf(drawn: string | null, where: string): number {
  const count = (drawn ?? "").trim();
  if (!/^\d+$/.test(count)) {
    throw new Error(`${where} draws ${JSON.stringify(drawn)} as its count, which is no number`);
  }
  return Number(count);
}

/** Fails where two rows have the same key under the same parent key: a comparison by key could
 *  not tell them apart. */
function expectRowsNamedApart(rows: readonly KeyedRow[], where: string): void {
  const named = new Set<string>();
  for (const row of rows) {
    const name = JSON.stringify([row.parentKey, row.key]);
    if (named.has(name)) {
      throw new Error(
        `${where}, two rows are named ${row.key} under ${row.parentKey ?? "the issue"}, so a ` +
          "comparison by key and parent key could not tell them apart"
      );
    }
    named.add(name);
  }
}

/** One seed's readings, each row named by what the seed returned (`seedKeys`, `keyedRows`). */
function seedRecord(
  drawn: PageReading,
  keys: ReadonlyMap<string, string>,
  seed: string
): SeedFixture {
  const opened = keyedRows(drawn.opened, keys);
  const rows = keyedRows(drawn.rows, keys);
  expectRowsNamedApart(opened, `In ${seed} as the page opens`);
  expectRowsNamedApart(rows, `In ${seed} with every branch open`);
  const filters = drawn.filters.map(({ count, filter, rows: filterRows }) => {
    const record: FilterFixture = {
      count: countOf(count, `${seed}'s ${PRESET_NAMES[filter]} control`),
      rows: keyedRows(filterRows, keys).map((row) => row.key),
    };
    return [filter, record] as const;
  });
  return { filters: Object.fromEntries(filters), loadRequests: drawn.loadRequests, opened, rows };
}

/** Empties the database, seeds it with `seed`, and records what the page draws of the issue. */
async function recordSeed(
  browser: Browser,
  name: string,
  seed: () => Promise<SeededIssue>
): Promise<SeedFixture> {
  await resetDatabase();
  const recorded: SeedFixture[] = [];
  await phase(browser, seed, async (page, seeded) => {
    const drawn = await readPage(page, seeded.key);
    // A block's node is named under its document's slug (`seedKeys`), and the seeds return the
    // documents' ids and not their slugs. The read comes after the page has drawn everything it
    // records, so it cannot change any of it.
    const slugs = ((await getIssue(seeded.key)).artifacts ?? []).map((artifact) => artifact.slug);
    recorded.push(seedRecord(drawn, seedKeys(seeded, slugs), name));
  });
  const [record] = recorded;
  if (record === undefined) throw new Error(`${name} recorded nothing`);
  // Checked seed by seed, so a value of one run fails the recording at the seed that drew it.
  expectNothingMinted(record, `the fixture's seeds.${name}`);
  return record;
}

/** The one test the runner finds in this file: every seed, in order, and their records written
 *  where the command that started the runner reads them. */
async function recordEverySeed(browser: Browser): Promise<void> {
  const output = process.env[OUTPUT_VARIABLE] ?? "";
  if (output === "") {
    throw new Error(
      `${OUTPUT_VARIABLE} names no file: run this file as \`bun e2e/tools/issue-page-t-record.ts\`, ` +
        "which starts the runner itself"
    );
  }
  const seeds: Record<string, SeedFixture> = {};
  for (const [name, seed] of SEEDS) {
    const record = await recordSeed(browser, name, seed);
    seeds[name] = record;
    console.error(
      `${name}: ${record.loadRequests} requests at load, ${record.opened.length} rows as the ` +
        `page opens, ${record.rows.length} with every branch open`
    );
  }
  writeFileSync(output, JSON.stringify(seeds));
}

// ---------------------------------------------------------------------------------------------
// Shared by both.
// ---------------------------------------------------------------------------------------------

/** Fails at the first string of `value`, a field's name included, that holds a UUID or an
 *  instant, naming where it stands. */
function expectNothingMinted(value: unknown, path: string): void {
  if (typeof value === "string") {
    for (const { pattern, what } of MINTED) {
      const found = pattern.exec(value);
      if (found !== null) {
        throw new Error(
          `${path} holds ${what}, ${found[0]}, which belongs to this run alone: the next run ` +
            "would write another, so the fixture cannot hold it"
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

// Run as a command, this file starts the runner on itself; loaded by the runner, it is the test.
if (import.meta.main) {
  main().then(
    (status) => process.exit(status),
    (error: unknown) => {
      console.error(error);
      process.exit(1);
    }
  );
} else {
  test("records the rows the new issue page draws from F's reads", async ({ browser }) => {
    await recordEverySeed(browser);
  });
}
