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

describe("how `--project` is read, which is what the refusals rest on", () => {
  test("one flag takes every value, as Playwright's variadic option does", () => {
    // `--project stub chromium` names two projects. Read as one, the run would answer
    // stub-only and waive the harness refusals and the preflight for the `chromium` rows
    // beside it, which is the whole of what those refusals are for.
    const both = selectionOf(["--project", "stub", "chromium"]);
    expect(both.holdsStubRow).toBe(true);
    expect(both.onlyStub).toBe(false);
    // A value stops at the next option, so the grep is no project.
    expect(selectionOf(["--project", "stub", "--grep", "@pr"]).onlyStub).toBe(true);
  });

  test("a name is matched without regard to case, as Playwright matches it", () => {
    expect(selectionOf(["--project=STUB"]).onlyStub).toBe(true);
    expect(selectionOf(["--project", "Stub-Phone"]).holdsStubRow).toBe(true);
  });

  test("a `*` pattern keeps everything, because this cannot say which projects it names", () => {
    // Playwright expands it against the config's own projects, which this module does not
    // see, so such a run may hold a stubbed row - it gets the server - and may hold a
    // real-stack row - it keeps every refusal and the preflight.
    const everything = selectionOf(["--project", "*"]);
    expect(everything.holdsStubRow).toBe(true);
    expect(everything.onlyStub).toBe(false);
    expect(selectionOf(["--project=stub*"]).onlyStub).toBe(false);
  });
});
