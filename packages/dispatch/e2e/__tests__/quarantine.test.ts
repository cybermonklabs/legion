/** The quarantine's rules that hold on every pull request (e2e/quarantine.ts): at most ten rows,
 *  each with an issue that owns it and the day it came in, each a row the suite runs in a project
 *  that can report it flaky and in no file the embedder job or the two-process job runs. What
 *  depends on the date - an entry held longer than 14 days, a row flaky twice that is not here -
 *  is the nightly's (`e2e/tools/quarantine-check.ts`), never this file's: a pull request is no
 *  place for a check that turns red on a day nobody chose.
 *
 *  The faults are read from the module's own list and from the planted ones of
 *  `fixtures/quarantine-entries.fixture.ts`, each refused for the one fault its name says. The
 *  embedder job's files are read from a listing of the command `bun run e2e:embedder` runs, and
 *  the two-process job's from a listing under each of `twoProcessRuns`, both through
 *  `listUnquarantinableFiles` of `./playwright-listing`, which the nightly's check reads them
 *  through too. The rows an entry names are read from a listing of e2e/playwright.config.ts under
 *  DISPATCH_E2E_QUARANTINE=only, which is the nightly's quarantine leg: what it selects is
 *  exactly what the config's own pattern selects, so an entry whose row it does not hold names no
 *  row of the suite.
 *  `fixtures/quarantine.config.ts` lists planted rows the same way, under each of the three
 *  selections, which is what shows the pattern takes the rows its entries name and nothing that
 *  only looks like one. Its listing under `exclude` is split again through `e2e/shard-reporter.ts`
 *  over the shard job's four legs, which shows the split by row time places the rows the pattern
 *  leaves, each on one leg, and brings no quarantined row back.
 */
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { rowsOfListing } from "../configurations";
import { legRowFaults } from "../leg-rows";
import realConfig from "../playwright.config";
import {
  entryLines,
  fixes,
  fixFaults,
  noUnquarantinableFiles,
  quarantine,
  quarantineFaults,
  rowName,
  selectionFaults,
  type UnquarantinableFiles,
} from "../quarantine";
import { rowLimit } from "./bounded-subprocess";
import { plantedQuarantine } from "./fixtures/quarantine.config";
import {
  elevenRows,
  embedderRow,
  looseFix,
  malformed,
  withoutDate,
  withoutIssue,
} from "./fixtures/quarantine-entries.fixture";
import { listing, listUnquarantinableFiles, retained } from "./playwright-listing";

// Started when the file loads, so the seven collect at once. The first lists the real suite under
// the nightly's quarantine leg, the second the embedder job's rows, the next two the two-process
// job's (`listUnquarantinableFiles` starts those three), the other three the planted config under
// each selection.
//
// The first names the entries' files and no other. The pattern `only` adds matches a row's whole
// grep title, which begins with its project and its file, neither holding a space (`quarantined`
// in e2e/quarantine.ts), so it selects no row of a file no entry names, and the files left out
// hold nothing it would have selected. A file that is gone lists nothing, which
// `--pass-with-no-tests` keeps from failing, and its entry is refused below as a row the suite
// does not run. With the quarantine empty no file is named and the listing reads the whole
// suite: no other row proves that `only` with no entry selects nothing.
const quarantinedRows = retained(
  listing(import.meta.url, "e2e/playwright.config.ts", { DISPATCH_E2E_QUARANTINE: "only" }, [
    ...new Set(quarantine.map((entry) => entry.file)),
    "--pass-with-no-tests",
  ])
);
const unquarantinable: Promise<UnquarantinableFiles> = retained(
  listUnquarantinableFiles(import.meta.url).then(({ files }) => files)
);
const plantedConfig = "e2e/__tests__/fixtures/quarantine.config.ts";
const plantedAll = retained(listing(import.meta.url, plantedConfig, {}));
const plantedExcluded = retained(
  listing(import.meta.url, plantedConfig, { DISPATCH_E2E_QUARANTINE: "exclude" })
);
const plantedOnly = retained(
  listing(import.meta.url, plantedConfig, { DISPATCH_E2E_QUARANTINE: "only" }, [
    "--pass-with-no-tests",
  ])
);

/** The seven listings above, which every row below waits for some of, all started at once. */
const sevenListings = [
  "listing",
  "listing",
  "listing",
  "listing",
  "listing",
  "listing",
  "listing",
] as const;

/** The projects a quarantined row can belong to: those that take the config's one retry on CI,
 *  which is what makes a row reportable as flaky, and that a run naming no project selects, which
 *  is how the nightly's quarantine leg runs. The `-alone`, `visual` and stubbed projects set
 *  `retries: 0`, so no row of theirs is ever flaky, and the `-alone` ones carry `default: false`. */
const retryingProjects = (realConfig.projects ?? [])
  .filter((project) => project.retries === undefined && project.default !== false)
  .map((project) => project.name ?? "");

test(
  "the quarantine holds at most ten rows, each with its issue and its date",
  async () => {
    expect(retryingProjects).toContain("chromium");
    expect(quarantineFaults(quarantine, retryingProjects, await unquarantinable)).toEqual([]);
  },
  rowLimit({ atOnce: sevenListings })
);

// A fix entry's shape: its row, a dispatch:// issue and a full lower-case sha. Whether the sha
// is on main is the nightly's to read; the ten-row bound is the quarantine's alone.
test("every fix names its row, its issue and the full sha of its commit", () => {
  expect(fixFaults(fixes, retryingProjects, quarantine)).toEqual([]);
  expect(fixFaults(looseFix, retryingProjects, quarantine)).toEqual([
    "fix 1 (planted.e2e.ts [chromium] a planted row fixed loosely) names its issue as " +
      "undefined, which is no dispatch://<KEY> reference such as dispatch://INFRA-983",
    'fix 1 (planted.e2e.ts [chromium] a planted row fixed loosely) names its fix as "708CA88", ' +
      "which is no full 40-character lower-case commit sha",
  ]);
});

// The nightly's check names the lines to remove for an entry held too long, as `entryLines`
// prints them, and a person removes those lines: an entry written any other way would be lines
// the file does not hold.
test("every entry is written as the nightly's check prints it", () => {
  const source = readFileSync(fileURLToPath(new URL("../quarantine.ts", import.meta.url)), "utf8");
  expect(
    [...quarantine, ...fixes]
      .filter((entry) => !source.includes(entryLines(entry).join("\n")))
      .map(rowName),
    "Write each entry of e2e/quarantine.ts as `entryLines` prints it: one field per line, in " +
      "the order file, project, title, issue, added, and a value that would pass 100 columns " +
      "on the line after its key, which is what Biome keeps."
  ).toEqual([]);
});

test(
  "a quarantine of eleven rows is refused for its size",
  async () => {
    expect(quarantineFaults(elevenRows, retryingProjects, await unquarantinable)).toEqual([
      "the quarantine holds 11 rows, and R4 of dispatch://INFRA-983 allows 10: fix a quarantined " +
        "row and take its entry out before another goes in",
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

test(
  "an entry with no issue is refused, and so is an entry with no date",
  async () => {
    const files = await unquarantinable;
    expect(quarantineFaults(withoutIssue, retryingProjects, files)).toEqual([
      "entry 1 (planted.e2e.ts [chromium] a planted row with no owner) has no issue: every " +
        "quarantined row is owned by one, dispatch://<KEY>",
    ]);
    expect(quarantineFaults(withoutDate, retryingProjects, files)).toEqual([
      "entry 1 (planted.e2e.ts [chromium] a planted row with no date) has no date: write the day " +
        'the row came in as added: "YYYY-MM-DD"',
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

test(
  "an issue that is no dispatch:// reference and a day that is no calendar day are refused",
  async () => {
    expect(quarantineFaults(malformed, retryingProjects, await unquarantinable)).toEqual([
      "entry 1 (planted.e2e.ts [chromium] a planted row written loosely) names its issue as " +
        '"INFRA-983", which is no dispatch://<KEY> reference such as dispatch://INFRA-983',
      "entry 1 (planted.e2e.ts [chromium] a planted row written loosely) gives the day it came in " +
        'as "2026-02-30", which is no calendar day written YYYY-MM-DD',
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

test(
  "an entry of a project that never retries, and an entry written twice, are refused",
  async () => {
    const entry = {
      file: "heavy-bodies.e2e.ts",
      project: "chromium-alone",
      title: "a planted row",
      issue: "dispatch://INFRA-983",
      added: "2026-11-19",
    };
    expect(quarantineFaults([entry, entry], retryingProjects, await unquarantinable)).toEqual([
      "entry 1 (heavy-bodies.e2e.ts [chromium-alone] a planted row) names the project " +
        "chromium-alone, whose rows CI never reports flaky and the nightly's quarantine leg does " +
        `not run; a quarantined row is one of ${retryingProjects.join(", ")}`,
      "entry 2 (heavy-bodies.e2e.ts [chromium-alone] a planted row) names the project " +
        "chromium-alone, whose rows CI never reports flaky and the nightly's quarantine leg does " +
        `not run; a quarantined row is one of ${retryingProjects.join(", ")}`,
      "entry 2 (heavy-bodies.e2e.ts [chromium-alone] a planted row) repeats an entry above it",
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

// The embedder job runs its files with no quarantine, on both lanes, and every other run skips
// their rows for want of the fake embedder, the nightly's quarantine leg among them: an entry of
// such a row would leave it gating every pull request and run it ungated nowhere.
test(
  "an entry of a row only the embedder job runs is refused",
  async () => {
    const files = await unquarantinable;
    // The controls: the planted row is of a file the embedder job's own listing holds, and read
    // against no such file it carries no other fault.
    expect(files.embedder).toContain(embedderRow[0].file);
    expect(quarantineFaults(embedderRow, retryingProjects, noUnquarantinableFiles)).toEqual([]);
    expect(quarantineFaults(embedderRow, retryingProjects, files)).toEqual([
      "entry 1 (search-tiers.e2e.ts [chromium] a planted row of the embedder job) names the file " +
        "search-tiers.e2e.ts, which the embedder job runs on both lanes with no quarantine and " +
        "the nightly's quarantine leg skips, having no embedder: a quarantine entry would leave " +
        "the row gating as before and run it ungated nowhere",
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

// The two-process job runs its files in the full lane with no quarantine, and the nightly's
// quarantine leg runs one server process, so it never runs them: an entry of such a row would
// leave it gating as before and run it ungated nowhere. Its rows are fixed in their own lines or
// given a fix entry, which is what the nightly's check names for them. Both of the job's steps
// are read: `signin.multiprocess.e2e.ts` is listed under DISPATCH_E2E_SIGNIN=oidc alone.
test(
  "an entry of a row only the two-process job runs is refused, naming why",
  async () => {
    const files = await unquarantinable;
    const entry = {
      file: "two-builds.multiprocess.e2e.ts",
      project: "chromium",
      title: "a planted row of the two-process job",
      issue: "dispatch://INFRA-983",
      added: "2026-11-19",
    };
    // The controls: the planted row is of a file the two-process job's listing holds, so is the
    // sign-in spec, and read against no such file the entry carries no other fault.
    expect(files.twoProcess).toContain(entry.file);
    expect(files.twoProcess).toContain("signin.multiprocess.e2e.ts");
    expect(quarantineFaults([entry], retryingProjects, noUnquarantinableFiles)).toEqual([]);
    expect(quarantineFaults([entry], retryingProjects, files)).toEqual([
      "entry 1 (two-builds.multiprocess.e2e.ts [chromium] a planted row of the two-process job) " +
        "names the file two-builds.multiprocess.e2e.ts, which the two-process job runs in the " +
        "full lane with no quarantine and the nightly's quarantine leg, which runs one server " +
        "process, never runs: a quarantine entry would leave the row gating as before and run " +
        "it ungated nowhere",
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

test(
  "every quarantined row is a row the suite runs, and the quarantine selects no other",
  async () => {
    expect(selectionFaults(quarantine, rowsOfListing(await quarantinedRows))).toEqual([]);
  },
  rowLimit({ atOnce: sevenListings })
);

test(
  "the pattern leaves out the rows its entries name and no row that only looks like one",
  async () => {
    const every = rowsOfListing(await plantedAll).map(rowName);
    const left = rowsOfListing(await plantedExcluded).map(rowName);
    const only = rowsOfListing(await plantedOnly);
    const quarantinedNames = [
      "quarantine-rows.fixture.ts [alpha-phone] offline › costs $5 (or more)? @phone",
      "quarantine-rows.fixture.ts [alpha] saves the draft @pr",
    ];
    // The control: with no selection asked for, both quarantined rows are listed with the rest.
    expect(every.sort()).toEqual(
      [
        ...quarantinedNames,
        "quarantine-rows.fixture.ts [alpha] autosave saves the draft @pr",
        "quarantine-rows.fixture.ts [alpha] offline › costs $5 (or more)? @phone",
        "quarantine-rows.fixture.ts [alpha] offline › saves the draft @pr",
        "quarantine-rows.fixture.ts [alpha] saves the draft @pr twice",
      ].sort()
    );
    expect(left.sort()).toEqual(every.filter((name) => !quarantinedNames.includes(name)).sort());
    expect(only.map(rowName).sort()).toEqual(quarantinedNames);
    // The third planted entry names a row the file does not hold, which is the fault the row
    // above reads for the real list.
    expect(selectionFaults(plantedQuarantine, only)).toEqual([
      "quarantine-rows.fixture.ts [alpha] saves the draft is a row the suite does not run: no " +
        "row of alpha has that file and that full title, so it was renamed, moved or deleted, " +
        "or its project no longer selects it; take the entry out, or write the row's title as " +
        "the listing names it",
    ]);
  },
  rowLimit({ atOnce: sevenListings })
);

// The shard job's browser step runs under `exclude`, and its legs place the selection
// e2e/shard-reporter.ts reads in `preprocess`, which Playwright builds after each project's own
// `grepInvert` - the one `quarantined` adds - and from which the reporter only takes rows away.
// So the planted listing under `exclude`, split over the job's four legs through the reporter,
// must hold each row it holds on exactly one leg (`legRowFaults`, the merge job's check), and no
// leg a quarantined row. The four legs are listed at once, each of the planted file alone.
test(
  "the shard legs place the rows the pattern leaves, each on one leg, and no quarantined row",
  async () => {
    const left = rowsOfListing(await plantedExcluded).map(rowName);
    const quarantinedNames = plantedQuarantine.map(rowName);
    const legs = await Promise.all(
      [1, 2, 3, 4].map(async (leg) => {
        const listed = await listing(
          import.meta.url,
          plantedConfig,
          { DISPATCH_E2E_QUARANTINE: "exclude" },
          [`--shard=${leg}/4`, "--pass-with-no-tests"],
          ["./e2e/shard-reporter.ts"]
        );
        return { leg: `shard-${leg}`, ran: rowsOfListing(listed).map(rowName), selection: left };
      })
    );
    expect(left.length).toBeGreaterThan(0);
    expect(legRowFaults(legs)).toEqual([]);
    expect(
      legs.flatMap((leg) => leg.ran.filter((name) => quarantinedNames.includes(name)))
    ).toEqual([]);
  },
  // The planted listing under `exclude`, one of the seven the file starts at once when it loads,
  // then the four legs at once.
  rowLimit({ atOnce: sevenListings }, { atOnce: ["listing", "listing", "listing", "listing"] })
);
