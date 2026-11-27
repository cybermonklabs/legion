import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  busProjectNames,
  busSpecPattern,
  clockProjectNames,
  clockSpecNames,
  clockSpecPattern,
  duplicateRows,
  type ListedRow,
  listedProjects,
  phoneAloneSpecNames,
  phoneAloneSpecPattern,
  projectConfigurations,
  projectsOnCommandLine,
  rowsOfListing,
} from "../configurations";
import { harnessProcessCount } from "../harness-processes";
import realConfig from "../playwright.config";
import { rowLimit } from "./bounded-subprocess";
import doubledConfig, { doubledProject, doubledSpec } from "./fixtures/doubled-project.config";
import { listing, projectArguments, retained } from "./playwright-listing";

// This file reads e2e/playwright.config.ts for its projects' `use` and starts nothing. Importing
// it costs nothing but the read: the config starts no server of its own and refuses no run for
// the harness's sake - `globalSetup` starts the stacks, and nothing here runs it
// (e2e/global-setup.ts).

// The process count reaches the listings below: that is what chooses the project list (the
// config's `projects:`, e2e/playwright.config.ts), and the configurations below are read from
// the config this process imported with the same environment. Nothing else of this shell's
// environment reaches them.
const ambientProcessCount: Record<string, string> =
  process.env.DISPATCH_E2E_PROCESSES === undefined
    ? {}
    : { DISPATCH_E2E_PROCESSES: process.env.DISPATCH_E2E_PROCESSES };

// The three listings are read-only subprocesses, so they start here rather than in their tests:
// the second and third collect while the first does, which is most of this file's cost on a
// two-core runner.
//
// The third names the six `-alone` projects (`clockProjectNames`, e2e/configurations.ts),
// which carry `default: false` since dispatch://INFRA-1280, and the bus project beside them
// (`busProjectNames`, the same module), which carries it too. A listing that names no project
// holds no row of theirs, so without this one the accounting below would read the suite with
// `agent-workspace-timing.e2e.ts`, `heavy-bodies.e2e.ts`, `nested-directive.e2e.ts`,
// `large-documents-measured.e2e.ts`, `large-documents-build-serves.e2e.ts`,
// `large-documents-build-serves.the-decision.e2e.ts`, `large-documents-desktop-build.e2e.ts`,
// the phone half of `large-documents-long-build.e2e.ts` and `answer-parties.bus.e2e.ts`
// missing: the 59 rows of 8 files this head's `-alone` listing holds, and the bus project's.
// One listing for both lists rather than one each, since a listing of the full config is a
// `node` of 300 to 500 MiB (e2e/__tests__/shard-split.test.ts).
//
// It is taken only where the run has one Dispatch process, because the ambient count reaches
// these listings and two-process mode declares one project, the multiprocess one: measured at
// this head, `--project=chromium-alone --project=firefox-alone --project=webkit-alone
// --project=iphone-alone --project=iphone-measure-alone --project=iphone-long-build-alone
// --list` under DISPATCH_E2E_PROCESSES=2 exits 1 with
// `Error: Project(s) "chromium-alone", "firefox-alone", "webkit-alone", "iphone-alone",
// "iphone-measure-alone", "iphone-long-build-alone" not found. Available projects:
// "chromium"`. All three assertions
// that read it hold in that mode without it: the one project that mode declares owns rows of
// the real listing, and it declares no wall-clock project to hold to its settings or to its
// rows.
const realListing = retained(
  listing(import.meta.url, "e2e/playwright.config.ts", ambientProcessCount)
);
const doubledListing = retained(
  listing(import.meta.url, "e2e/__tests__/fixtures/doubled-project.config.ts", ambientProcessCount)
);
const outOfDefaultListing =
  harnessProcessCount === 1
    ? retained(
        listing(
          import.meta.url,
          "e2e/playwright.config.ts",
          ambientProcessCount,
          projectArguments([...clockProjectNames, ...busProjectNames])
        )
      )
    : undefined;

/** The configuration each row runs in. A project the map holds no key for gets a configuration of
 *  its own, so its rows are never read as repeats of another project's; the assertion on
 *  `listedProjects` is what names such a project. */
function keyOfRow(configurations: Map<string, string>): (row: ListedRow) => string {
  return (row) => configurations.get(row.project) ?? `no configuration for project ${row.project}`;
}

/** Whether a project's own `testMatch` selects a file of one of the `-alone` legs, which is what
 *  makes a project one of them: these projects select by file, where `webkit` and `firefox`
 *  select by an engine tag (e2e/playwright.config.ts, above `aloneProjects`), and the files are
 *  the config's own two lists - the wall-clock specs (`clockSpecNames`) and the specs whose phone
 *  half runs alone (`phoneAloneSpecNames`, both e2e/configurations.ts). The question is asked of
 *  the config rather than of `clockProjectNames`, because whether a project is named in that
 *  list is exactly what the test below reads this answer to check.
 *
 *  Every `testMatch` of the config is a RegExp, read at this head. A `string` is a glob, which
 *  this cannot expand and which holds no spec's own name: a glob of two stars, a slash and
 *  `*.e2e.ts` selects every wall-clock file and holds neither `heavy-bodies` nor
 *  `nested-directive`, so answering such a pattern at all would answer false for one that does
 *  select the file, and drop its project out of every assertion below. It throws instead,
 *  naming the project a glob was written for. */
function selectsAClockSpec(
  testMatch: string | RegExp | (string | RegExp)[] | undefined,
  project: string
): boolean {
  if (testMatch === undefined) return false;
  const patterns = Array.isArray(testMatch) ? testMatch : [testMatch];
  const expressions = patterns.map((pattern) => {
    if (typeof pattern === "string") {
      throw new Error(
        `${project} writes a \`testMatch\` of ${JSON.stringify(pattern)}, a glob, and this ` +
          "file reads a project's selection as a RegExp: a glob holds no spec's own name, so " +
          "whether it selects a wall-clock file cannot be read from its text. Write the " +
          "pattern as a RegExp, as every project of e2e/playwright.config.ts does, or teach " +
          "this reader to expand a glob."
      );
    }
    return pattern;
  });
  return [...clockSpecNames, ...phoneAloneSpecNames].some((name) =>
    expressions.some((pattern) => pattern.test(`${name}.e2e.ts`))
  );
}

test(
  "no row runs twice in one configuration, and every project has a configuration and rows",
  async () => {
    const report = await realListing;
    // The rows of the projects out of the default selection are read beside the default
    // listing's, because the two together are the suite: a listing that names no project holds
    // no row of those projects.
    const rows = [
      ...rowsOfListing(report),
      ...(outOfDefaultListing === undefined ? [] : rowsOfListing(await outOfDefaultListing)),
    ];
    const configurations = projectConfigurations(realConfig, harnessProcessCount);

    // A project whose configuration cannot be read fails here rather than passing quietly: its
    // rows would be compared against no other project's.
    expect(listedProjects(report).filter((name) => !configurations.has(name))).toEqual([]);

    // Every project of the configuration owns at least one row of those listings. The question
    // is asked of the rows and never of `listedProjects(report)`: Playwright reports every
    // project of the resolved configuration whatever the command selected - the `-alone`
    // listing at 77471f15 names six projects and reports all eighteen - so an assertion over
    // the project list could not fail, and would pass with the `-alone` listing missing
    // altogether. Listed at 77471f15, the two listings hold 1,707 rows and every project
    // owns some: the smallest is `firefox-download-navigation` with one, and the `-alone`
    // six hold 23, 13, 16, 2, 2 and 3.
    const projectsWithRows = new Set(rows.map((row) => row.project));
    const namedProjects = (realConfig.projects ?? [])
      .map((project) => project.name)
      .filter((name): name is string => name !== undefined);
    expect(
      namedProjects.filter((name) => !projectsWithRows.has(name)),
      "These projects of the configuration own no row of the listings this file reads, so " +
        "nothing here accounts for what they run. A project out of the default selection " +
        "(`default: false`) is listed only where a command names it, and the names this file " +
        "passes are `clockProjectNames`' and `busProjectNames`' (e2e/configurations.ts): write " +
        "the project's name in the one its rows belong to. Otherwise its selectors match no file."
    ).toEqual([]);

    // The config as it stands has no repeat - that is what this run found when it was first
    // written - so this assertion proves nothing about the change that brought it. It is the
    // guard for the project someone adds later in a configuration that already runs the same
    // file: `e2e/__tests__/fixtures/doubled-project.config.ts` is that project, and the test
    // below shows this assertion catches it.
    expect(duplicateRows(rows, keyOfRow(configurations))).toEqual([]);
  },
  rowLimit({ atOnce: ["listing", "listing"] })
);

/** The rows each `-alone` project owns, by the file they stand in and how many of that file
 *  it runs. This is the whole of what the six `-alone` legs run, written out.
 *
 *  It is a count per file, and not a list of titles, because the file is the selector: each of
 *  these projects names its own files in its `testMatch` (`clockSpecsNamed` or
 *  `phoneAloneSpecPattern`, e2e/configurations.ts), so a row's title moves it nowhere, and what
 *  this has to say out loud is a row that left one of those files or arrived in it.
 *  `firefox-alone` runs fewer rows of `heavy-bodies.e2e.ts` than `webkit-alone` because it
 *  inverts the three that read a reader's place (`readersPlaceRows`, e2e/playwright.config.ts),
 *  which is the one title left in that list and drops rows rather than taking them.
 *
 *  Measured with `--list`, and the test below holds this table to the listing at every head;
 *  `.github/ci-notes/dispatch-browser.md` gives the same numbers for the alone job, and says
 *  which rows they are. Moving a wall-clock row to another file, or another project, is an
 *  edit of this table in the same commit, which is what makes such a move something a reader
 *  decides on rather than something a run goes quiet about. */
const clockRowsOwned: Readonly<Record<string, Readonly<Record<string, number>>>> = {
  "chromium-alone": {
    "agent-workspace-timing.e2e.ts": 4,
    "heavy-bodies.e2e.ts": 16,
    "large-documents-desktop-build.e2e.ts": 1,
    "nested-directive.e2e.ts": 2,
  },
  "firefox-alone": { "heavy-bodies.e2e.ts": 13 },
  "iphone-alone": {
    "large-documents-build-serves.e2e.ts": 1,
    "large-documents-build-serves.the-decision.e2e.ts": 1,
  },
  "iphone-long-build-alone": { "large-documents-long-build.e2e.ts": 3 },
  "iphone-measure-alone": { "large-documents-measured.e2e.ts": 2 },
  "webkit-alone": { "heavy-bodies.e2e.ts": 16 },
};

/** The project whose rows are the phone half of `phoneAloneSpecNames`' files; every other project
 *  of `clockRowsOwned` runs files of `clockSpecNames` (both e2e/configurations.ts). */
const phoneLegProject = "iphone-long-build-alone";

/** How many rows `clockRowsOwned` says the projects `counted` keeps run, every file of theirs
 *  counted: the fewest a test that picks those projects' files out of their listing must find.
 *  It reads no pattern, so a test whose pattern matches no file is held to those rows and not to
 *  none. */
function rowsOwnedBy(counted: (project: string) => boolean): number {
  return Object.entries(clockRowsOwned)
    .filter(([project]) => counted(project))
    .flatMap(([, files]) => Object.values(files))
    .reduce((sum, rows) => sum + rows, 0);
}

// What a wall-clock row's quiet machine rests on, now that the file is the selector: that each
// of those projects runs the rows of its own files, and that no other project runs any of
// them. Until sjawhar/legion#2697 three of these projects picked their rows out of
// `large-documents-measured.e2e.ts` by patterns over the titles, and the projects above them
// inverted the same patterns, so a row renamed - or a row added to that file under a title no
// pattern matched - ran four workers wide in `chromium` or `iphone` instead, where a reading
// of the wall clock is a reading of the other three lanes, with every suite green and nothing
// in this file to say so. The two halves below are that hole closed: the table above fails
// when a row the wall-clock projects own stops being owned by them, and the filter under it
// fails when a parallel project picks one up.
test(
  "each wall-clock project runs the rows of its own files, and no other project runs one",
  async () => {
    // The two clauses below range over the default selection's rows, each picking out by a test
    // of its own the rows no project of that selection may list. Neither says anything of a
    // listing read as empty - a report `rowsOfListing` no longer reads - or of a test that picks
    // out nothing: a pattern that matches no file, or a phone leg the configurations do not
    // hold, which the phone test reads as no row at all. So the listing holds rows, and, where
    // the run declares the wall-clock projects, each test picks out of their own listing at
    // least every row `clockRowsOwned` says the projects it stands for run. That bound is counted
    // by project and not by the test's own pattern, which would hold a pattern that matches no
    // file to the rows of no file, that is to none.
    const listed = rowsOfListing(await realListing);
    const clockRows =
      outOfDefaultListing === undefined
        ? undefined
        : rowsOfListing(await outOfDefaultListing).filter((row) =>
            clockProjectNames.includes(row.project)
          );
    const configurations = projectConfigurations(realConfig, harnessProcessCount);
    const phoneLeg = configurations.get(phoneLegProject);
    const inAClockFile = (row: ListedRow) => clockSpecPattern.test(row.file);
    const inThePhoneLeg = (row: ListedRow) =>
      phoneAloneSpecPattern.test(row.file) &&
      phoneLeg !== undefined &&
      configurations.get(row.project) === phoneLeg;
    expect(listed.length, "the default selection's listing holds no row").toBeGreaterThan(0);
    if (clockRows !== undefined) {
      expect(
        clockRows.filter(inAClockFile).length,
        "the wall-clock projects' rows that `clockSpecPattern` picks out of their own listing"
      ).toBeGreaterThanOrEqual(rowsOwnedBy((project) => project !== phoneLegProject));
      expect(
        clockRows.filter(inThePhoneLeg).length,
        "the rows of `iphone-long-build-alone`'s configuration that the phone-half test picks " +
          "out of the wall-clock listing"
      ).toBeGreaterThanOrEqual(rowsOwnedBy((project) => project === phoneLegProject));
    }

    // No project of the default selection holds a row of a wall-clock file. This is read of
    // the listing rather than of the config's `testIgnore`, so a project added later with no
    // `testIgnore` of its own - which inherits this config's, holding no wall-clock file - is
    // caught by the rows it lists.
    const parallel = listed
      .filter(inAClockFile)
      .map((row) => `${row.project} runs ${row.file} › ${row.title}`)
      .sort();
    expect(
      parallel,
      "A project of the default selection lists a row of a wall-clock file. Those rows claim " +
        "a wall-clock time and are read one at a time on a machine of their own, so a project " +
        "that runs as wide as the run's own workers measures its own contention there. Every " +
        "parallel project names `clockSpecPattern` in its `testIgnore` " +
        "(e2e/playwright.config.ts); a project that names a `testIgnore` of its own replaces " +
        "this config's, so it has to name that pattern too."
    ).toEqual([]);

    // The phone half of a file `iphone-long-build-alone` owns runs there and nowhere else: no
    // project of the default selection in that project's configuration lists a row of it. Its
    // desktop half is `chromium`'s, a configuration of its own, and stays in the listing. The
    // duplicate check above would catch `iphone` taking it back as a repeat; this says which
    // leg it was moved to and why.
    const phoneHalf = listed
      .filter(inThePhoneLeg)
      .map((row) => `${row.project} runs ${row.file} › ${row.title}`)
      .sort();
    expect(
      phoneHalf,
      "A project of the default selection lists the phone half of a file whose phone half is a " +
        "leg of its own (`phoneAloneSpecNames`, e2e/configurations.ts). Those rows were the tail " +
        "of the `iphone` listing and landed in the last shard together, which is what the leg " +
        "took them out of: name `phoneAloneSpecPattern` in that project's `testIgnore`."
    ).toEqual([]);

    if (clockRows === undefined) return;
    const owned: Record<string, Record<string, number>> = {};
    for (const row of clockRows) {
      owned[row.project] ??= {};
      const files = owned[row.project] ?? {};
      files[row.file] = (files[row.file] ?? 0) + 1;
    }
    expect(
      owned,
      "The rows the wall-clock projects run are not the rows `clockRowsOwned` above says they " +
        "run. A file missing from a project, or a count short of its line, is a measurement " +
        "nothing takes under a run that reports green - the row is somewhere else, or it is " +
        "nowhere. Either move the row back into the file its project selects, or, where the " +
        "move is the point, write the new file in `clockSpecNames` " +
        "(e2e/configurations.ts), name it in the project's `testMatch` and say so here."
    ).toEqual(clockRowsOwned);
  },
  rowLimit({ atOnce: ["listing", "listing"] })
);

test(
  "a project that repeats another's rows in its configuration is caught",
  async () => {
    const report = await doubledListing;
    const rows = rowsOfListing(report);
    const configurations = projectConfigurations(doubledConfig, harnessProcessCount);
    expect(configurations.get(doubledProject)).toBe(configurations.get("chromium"));

    const repeated = rows.filter((row) => row.file === doubledSpec && row.project === "chromium");
    if (repeated.length === 0) {
      throw new Error(
        `The listing holds no \`chromium\` row of ${doubledSpec}, so the added project repeats ` +
          "nothing and this control shows nothing. DISPATCH_E2E_PROCESSES is " +
          `${process.env.DISPATCH_E2E_PROCESSES ?? "unset"}: at 2 the config selects the ` +
          "two-process project list, whose one project runs the multiprocess specs alone, not " +
          `${doubledSpec}. Unset DISPATCH_E2E_PROCESSES to run this test.`
      );
    }
    const duplicates = duplicateRows(rows, keyOfRow(configurations));
    expect(duplicates.map((duplicate) => `${duplicate.file} ${duplicate.title}`).sort()).toEqual(
      repeated.map((row) => `${row.file} ${row.title}`).sort()
    );
    for (const duplicate of duplicates) {
      expect([...duplicate.projects].sort()).toEqual(["chromium", doubledProject].sort());
    }
  },
  rowLimit("listing")
);

// The four fields that give a wall-clock row its quiet machine and keep it out of a bare run,
// and the list that is how every accounting of this suite finds those rows at all. Nothing else
// in the suite reads them: `workers: 1` and `retries: 0` are what make a reading the page's own
// work rather than the other workers', `default: false` is what keeps 59 rows out of a run that
// names no project, and a `dependencies` is what pulled the whole suite in behind a file filter
// until dispatch://INFRA-1280 (e2e/playwright.config.ts, above `aloneProjects`).
//
// This reads the imported configuration and no listing, so it holds in two-process mode as it
// stands: that mode declares one project, which selects the multiprocess specs and is no
// wall-clock project, so the loop runs over nothing there and the equality before it is read
// where the project list holds those projects at all, as the wall-clock listing above is.
test("the wall-clock projects are the one list, in its order, and run their rows alone", () => {
  const clockProjects = (realConfig.projects ?? []).filter((project) =>
    selectsAClockSpec(project.testMatch, project.name ?? "a project with no name")
  );
  if (harnessProcessCount === 1) {
    expect(
      clockProjects.map((project) => project.name),
      "The wall-clock projects of e2e/playwright.config.ts and `clockProjectNames` " +
        "(e2e/configurations.ts) are one list written twice, so they hold the same names in " +
        "the same order - the config's. Every reader of that list passes over a name it does " +
        "not hold: no listing would name that project and no accounting of this suite would " +
        "see its rows. Write the names there as this config declares them."
    ).toEqual([...clockProjectNames]);
  }
  for (const project of clockProjects) {
    const named = project.name ?? "a wall-clock project with no name";
    expect(
      project.workers,
      `${named} sets \`workers: ${JSON.stringify(project.workers)}\`, and a wall-clock project ` +
        "runs one row at a time: its rows claim a wall-clock time, which two of them drawing at " +
        "once on one machine makes a reading of the other's work. Set `workers: 1`."
    ).toBe(1);
    expect(
      project.retries,
      `${named} sets \`retries: ${JSON.stringify(project.retries)}\`, and a wall-clock project ` +
        "takes no retry: a threshold a regression misses half the time would fail the first " +
        "attempt, pass the retry, be reported `flaky` and leave the run green. Set `retries: 0`."
    ).toBe(0);
    expect(
      project.default,
      `${named} sets \`default: ${JSON.stringify(project.default)}\`, and a wall-clock project ` +
        "is out of the default selection: a run that names no project would otherwise schedule " +
        "its rows beside three other lanes, which is the contention their readings cannot " +
        "survive. Set `default: false`."
    ).toBe(false);
    expect(
      project.dependencies ?? [],
      `${named} names \`dependencies\`, which a wall-clock project has none of: a dependency is ` +
        "a project selected to run before it, and a file filter narrows the project it selects " +
        "and never one pulled in that way, so dispatch://INFRA-1280 deleted them. Delete this " +
        "one too; `--project` is how one of these projects is run."
    ).toEqual([]);
  }
});

// `bun run e2e:clock` is the one command that runs every wall-clock row, and it spells the
// projects out on its own command line, where no test above reaches: the config and the list
// could take a sixth wall-clock project and leave that script running five of six, green at
// every assertion of this file. This reads the script with the config's own reader of
// `--project` (`projectsOnCommandLine`, e2e/configurations.ts), so both spellings of the option
// are read as Playwright reads them.
test("`bun run e2e:clock` names every wall-clock project, in the list's order", () => {
  const manifestPath = fileURLToPath(new URL("../../package.json", import.meta.url));
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
    scripts: Record<string, string>;
  };
  const named = projectsOnCommandLine(manifest.scripts["e2e:clock"]?.split(" ") ?? []);
  expect(
    named,
    `\`e2e:clock\` names ${JSON.stringify(named)} and \`clockProjectNames\` ` +
      `(e2e/configurations.ts) holds ${JSON.stringify([...clockProjectNames])}. That script is ` +
      "the whole wall-clock run, so a project the list and the config hold and it does not is " +
      "a project whose rows nothing measures, under a command that reports a green wall-clock " +
      "run. Write each name of the list in packages/dispatch/package.json as " +
      "`--project=<name>`, in the list's order."
  ).toEqual([...clockProjectNames]);
});

// What a bus row's verdict rests on: that it runs where its stack has a bus, and nowhere else.
// A project of the default selection that listed one would run it on a stack with no bus, where
// the fake listener refuses the read of the frames and the row fails; and a bus project that
// listed a row of any other file would run that row against a server whose outbox publishes,
// which no other row is written for. Read of the listings rather than of the config's
// `testIgnore`, as the wall-clock files are above, so a project added later with selectors of
// its own is caught by the rows it lists.
test(
  "the bus project runs the rows of the bus files, and no other project runs one",
  async () => {
    const inABusFile = (row: ListedRow) => busSpecPattern.test(row.file);
    const named = (rows: readonly ListedRow[]) =>
      rows.map((row) => `${row.project} runs ${row.file} › ${row.title}`).sort();
    const listed = rowsOfListing(await realListing);
    expect(listed.length, "the default selection's listing holds no row").toBeGreaterThan(0);
    expect(
      named(listed.filter(inABusFile)),
      "A project of the default selection lists a row of a bus file (`busSpecPattern`, " +
        "e2e/configurations.ts). Those rows read the frames a stack's bus carried, and only a " +
        "bus project's stack has one: name `busSpecPattern` in that project's `testIgnore`."
    ).toEqual([]);
    if (outOfDefaultListing === undefined) return;
    const outOfDefault = rowsOfListing(await outOfDefaultListing);
    const busRows = outOfDefault.filter((row) => busProjectNames.includes(row.project));
    expect(busRows.length, "the bus projects' rows in their own listing").toBeGreaterThan(0);
    expect(
      named(busRows.filter((row) => !inABusFile(row))),
      "A bus project lists a row of a file that is no bus file: it would run against a server " +
        "whose outbox publishes, which that row is not written for."
    ).toEqual([]);
    expect(
      named(
        outOfDefault.filter((row) => inABusFile(row) && !busProjectNames.includes(row.project))
      ),
      "A project out of the default selection that is no bus project lists a row of a bus file."
    ).toEqual([]);
  },
  rowLimit({ atOnce: ["listing", "listing"] })
);

// The fields that keep the bus project's rows on a stack of their own and out of a bare run:
// `default: false`, since a bus run is one the hook refuses to mix with any other project
// (`busRunOf`, e2e/configurations.ts); `workers: 1`, one stack and one bus; and `retries: 0`, so
// a copy the outbox lost once is a failure and not a flake (e2e/playwright.config.ts, above
// `busProjects`). Two-process mode declares the multiprocess project alone, and so no bus
// project.
test("each bus project is out of the default selection and runs one row at a time", () => {
  for (const name of busProjectNames) {
    const project = (realConfig.projects ?? []).find((candidate) => candidate.name === name);
    if (harnessProcessCount !== 1) {
      expect(project, `${name} in two-process mode`).toBeUndefined();
      continue;
    }
    expect(project, `${name} is no project of e2e/playwright.config.ts`).toBeDefined();
    expect(project?.default, `${name}'s \`default\``).toBe(false);
    expect(project?.workers, `${name}'s \`workers\``).toBe(1);
    expect(project?.retries, `${name}'s \`retries\``).toBe(0);
  }
});

// `bun run e2e:bus` is the command that runs every bus row, and it spells the projects out on its
// own command line, read here as the `e2e:clock` row above reads that script.
test("`bun run e2e:bus` names every bus project", () => {
  const manifestPath = fileURLToPath(new URL("../../package.json", import.meta.url));
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
    scripts: Record<string, string>;
  };
  const named = projectsOnCommandLine(manifest.scripts["e2e:bus"]?.split(" ") ?? []);
  expect(
    named,
    `\`e2e:bus\` names ${JSON.stringify(named)} and \`busProjectNames\` (e2e/configurations.ts) ` +
      `holds ${JSON.stringify([...busProjectNames])}. Write each name of the list in ` +
      "packages/dispatch/package.json as `--project=<name>`, in the list's order."
  ).toEqual([...busProjectNames]);
});
