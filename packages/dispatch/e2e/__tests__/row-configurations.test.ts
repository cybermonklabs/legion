import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  clockProjectNames,
  clockSpecNames,
  duplicateRows,
  type ListedRow,
  listedProjects,
  projectConfigurations,
  projectsOnCommandLine,
  rowsOfListing,
} from "../configurations";
import { harnessProcessCount } from "../harness-processes";
import realConfig from "../playwright.config";
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
// two-core runner. What that costs is read once in this file, at the `listingTimeout` it
// justifies, below.
//
// The third names the three wall-clock projects (`clockProjectNames`, e2e/configurations.ts),
// which carry `default: false` since dispatch://INFRA-1280. A listing that names no project
// holds no row of theirs, so without this one the accounting below would read the suite with
// `heavy-bodies.e2e.ts` and `nested-directive.e2e.ts` missing: 50 rows, which is what this
// head's wall-clock listing holds.
//
// It is taken only where the run has one Dispatch process, because the ambient count reaches
// these listings and two-process mode declares one project, the multiprocess one: measured at
// this head, `--project=chromium-alone --project=firefox-alone --project=webkit-alone --list`
// under DISPATCH_E2E_PROCESSES=2 exits 1 with `Error: Project(s) "chromium-alone",
// "firefox-alone", "webkit-alone" not found. Available projects: "chromium"`. Both assertions
// that read it hold in that mode without it: the one project that mode declares owns rows of
// the real listing, and it declares no wall-clock project to hold to its settings.
const realListing = retained(listing("e2e/playwright.config.ts", ambientProcessCount));
const doubledListing = retained(
  listing("e2e/__tests__/fixtures/doubled-project.config.ts", ambientProcessCount)
);
const clockListing =
  harnessProcessCount === 1
    ? retained(
        listing(
          "e2e/playwright.config.ts",
          ambientProcessCount,
          projectArguments(clockProjectNames)
        )
      )
    : undefined;

/** The configuration each row runs in. A project the map holds no key for gets a configuration of
 *  its own, so its rows are never read as repeats of another project's; the assertion on
 *  `listedProjects` is what names such a project. */
function keyOfRow(configurations: Map<string, string>): (row: ListedRow) => string {
  return (row) => configurations.get(row.project) ?? `no configuration for project ${row.project}`;
}

/** Whether a project's own `testMatch` selects a wall-clock spec file, which is what makes a
 *  project a wall-clock one: these projects select by file, where `webkit` and `firefox` select
 *  by an engine tag (e2e/playwright.config.ts, above `aloneProjects`). The question is asked of
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
  return clockSpecNames.some((name) =>
    expressions.some((pattern) => pattern.test(`${name}.e2e.ts`))
  );
}

// The three listings run at once and cost about five and a half seconds together on this
// two-core pod - 5.55 and 5.38 seconds in two runs of this file, measured at this head, of
// which the real listing alone is 3.59 seconds and the wall-clock one 1.25 - so a test may take
// far longer than Bun's five-second default; a minute and a half is the ceiling at which
// something is wrong rather than slow.
const listingTimeout = 90_000;

test(
  "no row runs twice in one configuration, and every project has a configuration and rows",
  async () => {
    const report = await realListing;
    // The wall-clock rows are read beside the default listing's, because the two together are
    // the suite: those projects are out of the default selection, so a listing that names no
    // project holds no row of them.
    const rows = [
      ...rowsOfListing(report),
      ...(clockListing === undefined ? [] : rowsOfListing(await clockListing)),
    ];
    expect(rows.length).toBeGreaterThan(0);
    const configurations = projectConfigurations(realConfig, harnessProcessCount);

    // A project whose configuration cannot be read fails here rather than passing quietly: its
    // rows would be compared against no other project's.
    expect(listedProjects(report).filter((name) => !configurations.has(name))).toEqual([]);

    // Every project of the configuration owns at least one row of those listings. The question
    // is asked of the rows and never of `listedProjects(report)`: Playwright reports every
    // project of the resolved configuration whatever the command selected - this head's
    // wall-clock listing names three projects and reports all fifteen - so an assertion over
    // the project list could not fail, and would pass with the wall-clock listing missing
    // altogether. Measured at this head, the two listings hold 1,349 rows and every project
    // owns some: the smallest is `firefox-download-navigation` with one, and the wall-clock
    // three hold 19, 14 and 17.
    const projectsWithRows = new Set(rows.map((row) => row.project));
    const namedProjects = (realConfig.projects ?? [])
      .map((project) => project.name)
      .filter((name): name is string => name !== undefined);
    expect(
      namedProjects.filter((name) => !projectsWithRows.has(name)),
      "These projects of the configuration own no row of the listings this file reads, so " +
        "nothing here accounts for what they run. A project out of the default selection " +
        "(`default: false`) is listed only where a command names it, and the names this file " +
        "passes are `clockProjectNames`' (e2e/configurations.ts): write the project's name " +
        "there if its rows are read from the wall clock. Otherwise its selectors match no file."
    ).toEqual([]);

    // The config as it stands has no repeat - that is what this run found when it was first
    // written - so this assertion proves nothing about the change that brought it. It is the
    // guard for the project someone adds later in a configuration that already runs the same
    // file: `e2e/__tests__/fixtures/doubled-project.config.ts` is that project, and the test
    // below shows this assertion catches it.
    expect(duplicateRows(rows, keyOfRow(configurations))).toEqual([]);
  },
  listingTimeout
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
  listingTimeout
);

// The four fields that give a wall-clock row its quiet machine and keep it out of a bare run,
// and the list that is how every accounting of this suite finds those rows at all. Nothing else
// in the suite reads them: `workers: 1` and `retries: 0` are what make a reading the page's own
// work rather than the other workers', `default: false` is what keeps 50 rows out of a run that
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
// could take a fourth wall-clock project and leave that script running three of four, green at
// every assertion of this file. This reads the script with the config's own reader of
// `--project` (`projectsOnCommandLine`, e2e/configurations.ts), so both spellings of the option
// are read as Playwright reads them.
//
// It reads what the script runs and not its one line: every e2e script of this package is split
// in two since dispatch://INFRA-1109 - `e2e:clock` builds the contracts and the dashboard and
// then calls `e2e:clock:run`, which is the Playwright command, so that a CI job which took both
// builds from the run's artefacts can run the second half alone. The projects are named in that
// half, so a `bun run <script>` naming a script of this same manifest is followed to it and
// read in its place. A `bun run` naming anything else is left as the words it is:
// `build:contracts` is `bun run --filter @legion/contracts build`, another package's script,
// and no line of this manifest holds it.
function scriptCommandLine(
  scripts: Record<string, string>,
  name: string,
  chain: readonly string[] = []
): string[] {
  if (chain.includes(name)) {
    throw new Error(
      `packages/dispatch/package.json's \`${[...chain, name].join("` runs `")}\`, which is a cycle`
    );
  }
  const line = scripts[name];
  if (line === undefined) {
    throw new Error(`packages/dispatch/package.json holds no \`${name}\` script`);
  }
  const words = line.split(" ");
  return words.flatMap((word, index) =>
    words[index - 2] === "bun" && words[index - 1] === "run" && scripts[word] !== undefined
      ? scriptCommandLine(scripts, word, [...chain, name])
      : [word]
  );
}

test("`bun run e2e:clock` names every wall-clock project, in the list's order", () => {
  const manifestPath = fileURLToPath(new URL("../../package.json", import.meta.url));
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
    scripts: Record<string, string>;
  };
  const named = projectsOnCommandLine(scriptCommandLine(manifest.scripts, "e2e:clock"));
  expect(
    named,
    `\`e2e:clock\` names ${JSON.stringify(named)} and \`clockProjectNames\` ` +
      `(e2e/configurations.ts) holds ${JSON.stringify([...clockProjectNames])}. That script is ` +
      "the whole wall-clock run, so a project the list and the config hold and it does not is " +
      "a project whose rows nothing measures, under a command that reports a green wall-clock " +
      "run. Write each name of the list in packages/dispatch/package.json's `e2e:clock:run` as " +
      "`--project=<name>`, in the list's order."
  ).toEqual([...clockProjectNames]);
});
