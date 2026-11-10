import { expect, test } from "bun:test";

import {
  duplicateRows,
  type ListedRow,
  listedProjects,
  projectConfigurations,
  rowsOfListing,
} from "../configurations";
import { harnessProcessCount } from "../harness-processes";
import { listing, retained } from "./playwright-listing";

// e2e/playwright.config.ts reads the command line as it evaluates: with no harness port set it
// refuses the run unless that line is a listing, which starts no server and runs no spec
// (`isListMode` and the unset-ports refusal after it, e2e/playwright.config.ts). This file reads
// the config for its projects' `use` and starts nothing, so it says `--list` as the listings below
// do, and puts the command line back for whatever test file this worker runs next. The imports
// are dynamic because a static one would evaluate the config before this line could say so.
const commandLine = process.argv;
process.argv = [...commandLine, "--list"];
const { default: realConfig } = await import("../playwright.config");
const {
  default: doubledConfig,
  doubledProject,
  doubledSpec,
} = await import("./fixtures/doubled-project.config");
process.argv = commandLine;

// The process count reaches the listings below: that is what chooses the project list (the
// config's `projects:`, e2e/playwright.config.ts), and the configurations below are read from
// the config this process imported with the same environment. Nothing else of this shell's
// environment reaches them.
const ambientProcessCount: Record<string, string> =
  process.env.DISPATCH_E2E_PROCESSES === undefined
    ? {}
    : { DISPATCH_E2E_PROCESSES: process.env.DISPATCH_E2E_PROCESSES };

// Both listings are read-only subprocesses, so they start here rather than in their tests: the
// second collects while the first does, which is most of this file's cost on a two-core runner.
const realListing = retained(listing("e2e/playwright.config.ts", ambientProcessCount));
const doubledListing = retained(
  listing("e2e/__tests__/fixtures/doubled-project.config.ts", ambientProcessCount)
);

/** The configuration each row runs in. A project the map holds no key for gets a configuration of
 *  its own, so its rows are never read as repeats of another project's; the assertion on
 *  `listedProjects` is what names such a project. */
function keyOfRow(configurations: Map<string, string>): (row: ListedRow) => string {
  return (row) => configurations.get(row.project) ?? `no configuration for project ${row.project}`;
}

// The two listings run at once and cost about seven seconds together on a two-core runner, so a
// test may take far longer than Bun's five-second default; a minute and a half is the ceiling at
// which something is wrong rather than slow.
const listingTimeout = 90_000;

test(
  "no row runs twice in one configuration, and every project has one",
  async () => {
    const report = await realListing;
    const rows = rowsOfListing(report);
    expect(rows.length).toBeGreaterThan(0);
    const configurations = projectConfigurations(realConfig, harnessProcessCount);

    // A project whose configuration cannot be read fails here rather than passing quietly: its
    // rows would be compared against no other project's.
    expect(listedProjects(report).filter((name) => !configurations.has(name))).toEqual([]);

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
