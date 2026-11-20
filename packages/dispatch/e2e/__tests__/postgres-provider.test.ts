/** What `e2e/__tests__/postgres-provider.ts` decides, and where the tests that start a stack's
 *  Postgres run.
 *
 *  A test that needs a Postgres never skips in CI: the helper skips it on a developer's run that
 *  lacks one and fails it in CI. That moves the question to the jobs. The `dispatch-ui` job runs
 *  this directory with no stack provider, so its `bun test` must leave out every file that calls
 *  `requirePostgres`, or that job fails; and a file it leaves out runs nowhere unless a job of
 *  `.github/workflows/dispatch-browser.yaml` that names a provider runs it. The second and third
 *  rows hold the three together, on the real files and on planted ones, so the check is seen to
 *  fail on each fault it is for. */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { basename, join } from "node:path";
import { fileURLToPath } from "node:url";

import { testFiles } from "../row-limits";
import {
  eitherProvider,
  inCi,
  requireNothingMissing,
  requirePostgres,
  skipsWithout,
  skipsWithoutPostgres,
} from "./postgres-provider";

const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const repository = fileURLToPath(new URL("../../../..", import.meta.url));
const workflowPath = join(repository, ".github/workflows/dispatch-browser.yaml");
/** This file, which calls `requirePostgres` in its first row and starts no Postgres. */
const thisFile = `__tests__/${basename(import.meta.path)}`;

test("a run outside CI that lacks a Postgres skips, and a CI run fails instead", () => {
  const image = "DISPATCH_E2E_PG_IMAGE";
  expect(inCi({ CI: "true" })).toBe(true);
  expect(inCi({ CI: " TRUE " })).toBe(true);
  expect(inCi({})).toBe(false);
  expect(inCi({ CI: "1" })).toBe(false);

  expect(skipsWithoutPostgres("a.test.ts", eitherProvider, {})).toBe(true);
  expect(skipsWithoutPostgres("a.test.ts", eitherProvider, { CI: "true" })).toBe(false);
  expect(skipsWithoutPostgres("a.test.ts", eitherProvider, { [image]: "img" })).toBe(false);
  expect(() => requirePostgres(eitherProvider, { CI: "true" })).toThrow(
    "set DISPATCH_E2E_PG_BIN or DISPATCH_E2E_PG_IMAGE in the job's step"
  );
  expect(requirePostgres(eitherProvider, { [image]: " img " })).toEqual({
    variable: image,
    value: "img",
  });

  expect(skipsWithout("a.test.ts", ["psql on this host's PATH"], {})).toBe(true);
  expect(skipsWithout("a.test.ts", ["psql on this host's PATH"], { CI: "true" })).toBe(false);
  expect(skipsWithout("a.test.ts", [], {})).toBe(false);
  expect(() => requireNothingMissing(["psql on this host's PATH"])).toThrow(
    "this CI job does not have: psql on this host's PATH"
  );
  expect(() => requireNothingMissing([])).not.toThrow();
});

/** The faults the check below finds, each as one line naming the file and what is wrong. It
 *  proves that each file calling `requirePostgres` is left out of the lane and named by a step
 *  that exports a provider, and nothing of its rows: a row that no step's `bun test -t` selects
 *  runs nowhere and still passes it. */
function stackTestFaults(input: {
  /** Each test file of the directory, relative to `e2e`, with its source. */
  readonly files: readonly { readonly file: string; readonly source: string }[];
  /** The `test:harness` script of packages/dispatch/package.json: the harness lane, which its
   *  `test` script runs in the `dispatch-ui` job, and `test:repeat` in the weekly repeated job. */
  readonly laneScript: string;
  /** The `run:` text of every step of dispatch-browser.yaml, with the job it is in. */
  readonly steps: readonly { readonly job: string; readonly run: string }[];
}): string[] {
  const stackFiles = input.files
    .filter(({ source }) => /\brequirePostgres\(/.test(source))
    .map(({ file }) => `e2e/${file}`);
  const ignored = [...input.laneScript.matchAll(/--path-ignore-patterns[ =](\S+)/g)].map(
    (match) => match[1] ?? ""
  );
  const faults: string[] = [];
  for (const file of stackFiles) {
    if (!ignored.includes(file)) {
      faults.push(
        `${file} calls requirePostgres, and the dispatch-ui lane runs it with no provider`
      );
    }
    const runners = input.steps.filter(
      ({ run }) =>
        /\bbun test\b/.test(run) &&
        run.includes(file) &&
        /export DISPATCH_E2E_PG_(?:BIN|IMAGE)=\S/.test(run)
    );
    if (runners.length === 0) {
      faults.push(`${file} is run by no step of dispatch-browser.yaml that names a provider`);
    }
  }
  for (const pattern of ignored) {
    if (!stackFiles.includes(pattern)) {
      faults.push(`${pattern} is left out of the dispatch-ui lane, and calls no requirePostgres`);
    }
  }
  return faults;
}

/** The steps of the workflow, read as YAML. */
function workflowSteps(text: string): { job: string; run: string }[] {
  const parsed = Bun.YAML.parse(text) as {
    jobs?: Record<string, { steps?: { run?: unknown }[] }>;
  };
  return Object.entries(parsed.jobs ?? {}).flatMap(([job, body]) =>
    (body.steps ?? [])
      .filter((step) => typeof step.run === "string")
      .map((step) => ({ job, run: step.run as string }))
  );
}

test("every test that requires a stack's Postgres is left out of the dispatch-ui lane and run where a job names a provider", () => {
  const files = testFiles(e2eDirectory)
    .filter((file) => file !== thisFile)
    .map((file) => ({ file, source: readFileSync(join(e2eDirectory, file), "utf8") }));
  const manifest = JSON.parse(readFileSync(join(dispatchPackage, "package.json"), "utf8")) as {
    scripts: Record<string, string>;
  };
  const steps = workflowSteps(readFileSync(workflowPath, "utf8"));
  // The control that the reads saw anything: a check over no file and no step finds no fault.
  expect(files.filter(({ source }) => source.includes("requirePostgres(")).length).toBeGreaterThan(
    0
  );
  expect(steps.length).toBeGreaterThan(0);
  // The `dispatch-ui` job's `test` runs e2e/__tests__ through the harness lane, whose script holds
  // the files it leaves out.
  expect(manifest.scripts.test).toContain("bun run test:harness e2e/__tests__");
  expect(
    stackTestFaults({ files, laneScript: manifest.scripts["test:harness"] ?? "", steps }),
    "Leave each such file out of packages/dispatch/package.json's `test:harness` script with a " +
      "--path-ignore-patterns of its own, and run it in a step of " +
      ".github/workflows/dispatch-browser.yaml that exports the provider it needs."
  ).toEqual([]);
});

test("the check names a stack test the lane still runs, one no job runs, and an ignore of nothing", () => {
  const stack = { file: "__tests__/a.test.ts", source: "requirePostgres([pgImageVariable]);" };
  const plain = { file: "__tests__/b.test.ts", source: "test('x', () => {});" };
  const lane = "bun test --path-ignore-patterns e2e/__tests__/a.test.ts e2e/__tests__";
  const named = "export DISPATCH_E2E_PG_IMAGE=image\nbun test e2e/__tests__/a.test.ts";
  // The control: the planted inputs as they should be find no fault.
  expect(
    stackTestFaults({ files: [stack, plain], laneScript: lane, steps: [{ job: "v", run: named }] })
  ).toEqual([]);
  expect(
    stackTestFaults({
      files: [stack, plain],
      laneScript: "bun test e2e/__tests__",
      steps: [{ job: "v", run: named }],
    })
  ).toEqual([
    "e2e/__tests__/a.test.ts calls requirePostgres, and the dispatch-ui lane runs it with no provider",
  ]);
  expect(
    stackTestFaults({
      files: [stack, plain],
      laneScript: lane,
      steps: [{ job: "v", run: "bun test e2e/__tests__/a.test.ts" }],
    })
  ).toEqual([
    "e2e/__tests__/a.test.ts is run by no step of dispatch-browser.yaml that names a provider",
  ]);
  expect(
    stackTestFaults({
      files: [stack, plain],
      laneScript: `${lane} --path-ignore-patterns e2e/__tests__/b.test.ts`,
      steps: [{ job: "v", run: named }],
    })
  ).toEqual([
    "e2e/__tests__/b.test.ts is left out of the dispatch-ui lane, and calls no requirePostgres",
  ]);
});
