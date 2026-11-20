import { expect, test } from "bun:test";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { captureEnvironment } from "./environment";
import { hostTool, requireOrSkip, skipWithout } from "./host-tools";
import { temporaryDirectories } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-host-tools");

/** Names no host carries a program under. */
const ABSENT = "legion-test-fixtures-no-such-program";
const ABSENT_TOO = "legion-test-fixtures-no-such-program-either";
/** The job the gates below name; no job of ci.yaml is called this. */
const JOB = "test-fixtures-no-such-job";
/** A prerequisite no run has, named as a caller names one. */
const WHAT = "LEGION_TEST_FIXTURES_NO_SUCH_VARIABLE, naming nothing";

/** Runs `body` with `console.warn` collected rather than printed, and returns what it printed. */
function warningsFrom(body: () => void): string[] {
  const collected: string[] = [];
  const printed = console.warn;
  console.warn = (...parts: unknown[]) => {
    collected.push(parts.map((part) => String(part)).join(" "));
  };
  try {
    body();
  } finally {
    console.warn = printed;
  }
  return collected;
}

/** Runs `body` with `GITHUB_ACTIONS=true`, as every step of a GitHub Actions job runs, and puts
 *  back what the name held before. */
function onActions<T>(body: () => T): T {
  const restore = captureEnvironment(["GITHUB_ACTIONS"]);
  process.env["GITHUB_ACTIONS"] = "true";
  try {
    return body();
  } finally {
    restore();
  }
}

/**
 * Runs `source` as the file `caller`, with the gates imported, in a process of its own whose
 * environment has no `GITHUB_ACTIONS`, as a developer's run and a pod's have none. The skip is
 * shown there rather than in this process: the test-fixtures job runs this file under
 * `GITHUB_ACTIONS=true`, where the gates throw instead, and a row that skipped only off GitHub
 * Actions would show nothing in the one run CI makes of it.
 */
function offActions(
  source: string,
  caller = join(makeDirectory("caller"), "a.test.ts")
): {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
  readonly caller: string;
} {
  writeFileSync(
    caller,
    `import { requireOrSkip, skipWithout } from ${JSON.stringify(join(import.meta.dir, "host-tools.ts"))};\n` +
      source
  );
  const env = { ...process.env };
  delete env["GITHUB_ACTIONS"];
  const run = Bun.spawnSync([process.execPath, caller], { env, stdout: "pipe", stderr: "pipe" });
  return {
    code: run.exitCode,
    stdout: run.stdout.toString(),
    stderr: run.stderr.toString(),
    caller,
  };
}

test("hostTool returns the path of a program this host has", () => {
  const sh = hostTool("sh");
  if (sh === null) throw new Error("this host has no sh on PATH");
  expect(existsSync(sh)).toBe(true);
});

test("hostTool returns null for a program this host does not have", () => {
  expect(hostTool(ABSENT)).toBeNull();
});

test("skipWithout is false and prints nothing when the host has every program, on GitHub Actions too", () => {
  const skipped: boolean[] = [];
  const warnings = warningsFrom(() => {
    skipped.push(skipWithout(["sh"], JOB));
    skipped.push(onActions(() => skipWithout(["sh"], JOB)));
  });
  expect(skipped).toEqual([false, false]);
  expect(warnings).toEqual([]);
});

test("skipWithout with no names is false and prints nothing", () => {
  let skipped: boolean | undefined;
  const warnings = warningsFrom(() => {
    skipped = onActions(() => skipWithout([], JOB));
  });
  expect(skipped).toBe(false);
  expect(warnings).toEqual([]);
});

test("off GitHub Actions, skipWithout is true and prints one warning, of the missing programs and the calling file", () => {
  const run = offActions(
    `console.log(skipWithout(${JSON.stringify([ABSENT, "sh", ABSENT_TOO])}, ${JSON.stringify(JOB)}));\n`
  );
  expect(run.code).toBe(0);
  expect(run.stdout).toBe("true\n");
  expect(run.stderr).toBe(
    `Skipped the tests in ${run.caller}. This host does not have: ${ABSENT}, ${ABSENT_TOO}. ` +
      "Install the missing programs and run the tests again.\n"
  );
});

test("the warning names the calling file under a path that holds a space", () => {
  // The caller is read from the stack, and a frame's path is not quoted there: a checkout under
  // `…/my repo/…` must still be named whole.
  const root = join(makeDirectory("spaced"), "my repo");
  mkdirSync(root);
  const run = offActions(
    `skipWithout(${JSON.stringify([ABSENT])}, ${JSON.stringify(JOB)});\n`,
    join(root, "a.test.ts")
  );
  expect(run.code).toBe(0);
  expect(run.stderr).toContain(`Skipped the tests in ${run.caller}.`);
});

test("on GitHub Actions, skipWithout throws, naming the missing programs, the job and the calling file", () => {
  const warnings = warningsFrom(() => {
    expect(() => onActions(() => skipWithout([ABSENT, "sh", ABSENT_TOO], JOB))).toThrow(
      `a prerequisite that ci.yaml's ${JOB} job must give on GitHub Actions is missing: ` +
        `${ABSENT}, ${ABSENT_TOO} on this host's PATH, for the tests in ${import.meta.path}`
    );
  });
  expect(warnings).toEqual([]);
});

test("requireOrSkip is false and prints nothing when the prerequisite is present, on GitHub Actions too", () => {
  const skipped: boolean[] = [];
  const warnings = warningsFrom(() => {
    skipped.push(requireOrSkip(WHAT, true, JOB));
    skipped.push(onActions(() => requireOrSkip(WHAT, true, JOB)));
  });
  expect(skipped).toEqual([false, false]);
  expect(warnings).toEqual([]);
});

test("off GitHub Actions, requireOrSkip is true and prints one warning, of what is missing and the calling file", () => {
  const run = offActions(
    `console.log(requireOrSkip(${JSON.stringify(WHAT)}, false, ${JSON.stringify(JOB)}));\n`
  );
  expect(run.code).toBe(0);
  expect(run.stdout).toBe("true\n");
  expect(run.stderr).toBe(
    `Skipped the tests in ${run.caller}. This run does not have: ${WHAT}. ` +
      "Supply what is missing and run the tests again.\n"
  );
});

test("on GitHub Actions, requireOrSkip throws, naming what is missing, the job and the calling file", () => {
  const warnings = warningsFrom(() => {
    expect(() => onActions(() => requireOrSkip(WHAT, false, JOB))).toThrow(
      `a prerequisite that ci.yaml's ${JOB} job must give on GitHub Actions is missing: ${WHAT}, ` +
        `for the tests in ${import.meta.path}`
    );
  });
  expect(warnings).toEqual([]);
});
