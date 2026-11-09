/** What a run wants of the stubbed layer, read as `e2e/playwright.config.ts` reads it.
 *
 *  The three answers decide a lot and are easy to get wrong in one direction without anyone
 *  noticing in the other: a run given the static file server where there is none to start dies
 *  before its first row, and a run refused it has no page at all. These rows fix the cases the
 *  config cannot show on a pull request, the deployed run and two-process mode among them.
 */
import { describe, expect, test } from "bun:test";

import { stubOnlyVariable, stubRecordVariable, stubSelection } from "../stub/selection";

const projects = ["stub", "stub-phone"];

function selectionOf(argv: readonly string[], env: NodeJS.ProcessEnv = {}, processes = 1) {
  return stubSelection({ argv, env, processes, projects });
}

describe("which runs hold a stubbed row", () => {
  test("a run that names a stub project does, and one that names only others does not", () => {
    expect(selectionOf(["--project=stub"]).holdsStubRow).toBe(true);
    expect(selectionOf(["--project", "stub-phone"]).holdsStubRow).toBe(true);
    expect(selectionOf(["--project=chromium", "--project=stub"]).holdsStubRow).toBe(true);
    // The case that killed the full lane: the containerised visual job, where no bun exists.
    expect(selectionOf(["--project=visual", "--project=visual-phone"]).holdsStubRow).toBe(false);
    expect(selectionOf(["--no-deps", "--project=webkit-alone"]).holdsStubRow).toBe(false);
  });

  test("a run that names no project does, since it runs every project of its mode", () => {
    expect(selectionOf([]).holdsStubRow).toBe(true);
  });

  test("a deployed run and two-process mode hold none", () => {
    // `bun run e2e:deployed` names no project and builds no web/dist: given the server it
    // would either refuse to start or replay these rows against a stale local build rather
    // than against the deployed image.
    expect(selectionOf([], { PLAYWRIGHT_BASE_URL: "https://dispatch.example" }).holdsStubRow).toBe(
      false
    );
    expect(selectionOf([], {}, 2).holdsStubRow).toBe(false);
  });
});

describe("which runs are the stubbed layer alone", () => {
  test("a run of the stub projects and nothing else, which meets no harness refusal", () => {
    expect(selectionOf(["--project=stub", "--project=stub-phone"]).onlyStub).toBe(true);
    expect(selectionOf(["--project=stub", "--project=chromium"]).onlyStub).toBe(false);
    expect(selectionOf([]).onlyStub).toBe(false);
  });

  test("a recording run is not, because its rows meet the real harness", () => {
    const recording = selectionOf(["--project=stub"], { [stubRecordVariable]: "1" });
    expect(recording.recording).toBe(true);
    expect(recording.onlyStub).toBe(false);
    expect(recording.holdsStubRow).toBe(true);
  });

  test("the answer is written to the environment, so every worker reads the one reading", () => {
    // A worker re-imports the config with Playwright's own argv, so it can read the command
    // line of no one; without this it would meet a refusal the run was exempt from.
    const env: NodeJS.ProcessEnv = {};
    expect(selectionOf(["--project=stub"], env).onlyStub).toBe(true);
    expect(env[stubOnlyVariable]).toBe("1");
    expect(selectionOf(["--project=visual"], env).onlyStub).toBe(true);
  });
});
