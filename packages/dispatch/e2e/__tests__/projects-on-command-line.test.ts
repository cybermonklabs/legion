/** The projects a `playwright test` command line names, read as Playwright reads it.
 *
 *  `e2e/playwright.config.ts` asks this once per run, and `e2e/stub/selection.ts` turns the
 *  answer into what the run gets: a run that names the stub projects and nothing else is the
 *  stubbed layer alone, and `globalSetup` starts no stack for it. A reading that names a project
 *  the line did not therefore leaves real-stack rows with nothing to connect to, so the rows
 *  below are mostly about the lines where a `--project` is NOT one: another option's value, or a
 *  file filter after a bare `--`.
 */
import { describe, expect, test } from "bun:test";

import { projectsOnCommandLine } from "../configurations";

describe("the projects a command line names", () => {
  test("every `--project` of the line, in both spellings, lower-cased", () => {
    expect(projectsOnCommandLine(["--project=Stub"])).toEqual(["stub"]);
    expect(projectsOnCommandLine(["--project", "Chromium"])).toEqual(["chromium"]);
    expect(projectsOnCommandLine(["--project=stub", "--project", "chromium"])).toEqual([
      "stub",
      "chromium",
    ]);
  });

  test("`--project` is variadic, so one flag takes every value up to the next option", () => {
    expect(projectsOnCommandLine(["--project", "stub", "chromium", "--list"])).toEqual([
      "stub",
      "chromium",
    ]);
  });

  test("a line that names none", () => {
    expect(projectsOnCommandLine([])).toEqual([]);
    expect(projectsOnCommandLine(["--list", "e2e/smoke.e2e.ts"])).toEqual([]);
  });
});

describe("a `--project` that is no project of the run", () => {
  test("one that is another option's value, which the run itself does not see", () => {
    // Measured: `--output --project=stub e2e/smoke.e2e.ts` hands Playwright `--project=stub` as
    // the output directory - it wrote its artifacts into a folder of that name - and the run
    // selects every project. A reader that took it for a project answered "the stubbed layer
    // alone", `globalSetup` started no stack, and the chromium row died of ECONNREFUSED
    // (dispatch://INFRA-1410).
    expect(projectsOnCommandLine(["--output", "--project=stub", "e2e/smoke.e2e.ts"])).toEqual([]);
    expect(projectsOnCommandLine(["--grep", "--project", "chromium"])).toEqual([]);
    // An optional-value option leaves a dash token alone, so this `--project` is the run's own.
    expect(projectsOnCommandLine(["--shuffle", "--project", "chromium"])).toEqual(["chromium"]);
  });

  test("one after a bare `--`, where every token is a file filter", () => {
    expect(projectsOnCommandLine(["--", "--project", "chromium"])).toEqual([]);
    expect(projectsOnCommandLine(["--project", "stub", "--", "--project=chromium"])).toEqual([
      "stub",
    ]);
  });
});
