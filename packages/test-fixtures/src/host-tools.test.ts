import { expect, test } from "bun:test";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  hostTool,
  requireHostTools,
  skipOutsideCI,
  skipOutsideCIBecause,
  skipWithout,
} from "./host-tools";
import { temporaryDirectories } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-host-tools");

/** Names no host carries a program under. */
const ABSENT = "legion-test-fixtures-no-such-program";
const ABSENT_TOO = "legion-test-fixtures-no-such-program-either";

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

test("hostTool returns the path of a program this host has", () => {
  const sh = hostTool("sh");
  if (sh === null) throw new Error("this host has no sh on PATH");
  expect(existsSync(sh)).toBe(true);
});

test("hostTool returns null for a program this host does not have", () => {
  expect(hostTool(ABSENT)).toBeNull();
});

test("skipWithout is false and prints nothing when the host has every program", () => {
  let skipped: boolean | undefined;
  const warnings = warningsFrom(() => {
    skipped = skipWithout("sh");
  });
  expect(skipped).toBe(false);
  expect(warnings).toEqual([]);
});

test("skipWithout with no names is false and prints nothing", () => {
  let skipped: boolean | undefined;
  const warnings = warningsFrom(() => {
    skipped = skipWithout();
  });
  expect(skipped).toBe(false);
  expect(warnings).toEqual([]);
});

test("skipWithout is true and prints one warning, of the missing programs and this file", () => {
  let skipped: boolean | undefined;
  const warnings = warningsFrom(() => {
    skipped = skipWithout(ABSENT, "sh", ABSENT_TOO);
  });
  expect(skipped).toBe(true);
  expect(warnings).toHaveLength(1);
  const warning = warnings[0] ?? "";
  expect(warning).toContain(`does not have: ${ABSENT}, ${ABSENT_TOO}.`);
  expect(warning).toContain(import.meta.path);
});

test("the warning names the calling file under a path that holds a space", () => {
  // The caller is read from the stack, and a frame's path is not quoted there: a checkout under
  // `…/my repo/…` must still be named whole. It is measured in a process of its own, since the
  // path of this file is the one this process's stack carries.
  const root = join(makeDirectory("spaced"), "my repo");
  mkdirSync(root);
  const caller = join(root, "a.test.ts");
  writeFileSync(
    caller,
    `import { skipWithout } from ${JSON.stringify(join(import.meta.dir, "host-tools.ts"))};\n` +
      `skipWithout(${JSON.stringify(ABSENT)});\n`
  );
  const run = Bun.spawnSync([process.execPath, caller], { stdout: "pipe", stderr: "pipe" });
  expect(run.exitCode).toBe(0);
  expect(run.stderr.toString()).toContain(`Skipped the tests in ${caller}.`);
});

/** Runs `body` with GITHUB_ACTIONS set to `value`, or unset for undefined, and restores it. */
function underActions<T>(value: string | undefined, body: () => T): T {
  const before = process.env.GITHUB_ACTIONS;
  if (value === undefined) delete process.env.GITHUB_ACTIONS;
  else process.env.GITHUB_ACTIONS = value;
  try {
    return body();
  } finally {
    if (before === undefined) delete process.env.GITHUB_ACTIONS;
    else process.env.GITHUB_ACTIONS = before;
  }
}

test("skipOutsideCI is false and prints nothing when the host has every program, in CI or not", () => {
  for (const value of [undefined, "true"]) {
    let skipped: boolean | undefined;
    const warnings = warningsFrom(() => {
      skipped = underActions(value, () => skipOutsideCI("sh"));
    });
    expect({ value, skipped, warnings }).toEqual({ value, skipped: false, warnings: [] });
  }
});

test("skipOutsideCI outside CI is true and prints one warning, of the missing programs and this file", () => {
  let skipped: boolean | undefined;
  const warnings = warningsFrom(() => {
    skipped = underActions(undefined, () => skipOutsideCI(ABSENT, "sh", ABSENT_TOO));
  });
  expect(skipped).toBe(true);
  expect(warnings).toHaveLength(1);
  const warning = warnings[0] ?? "";
  expect(warning).toContain(`Skipped the tests in ${import.meta.path}.`);
  expect(warning).toContain(`does not have: ${ABSENT}, ${ABSENT_TOO}.`);
});

test("skipOutsideCI under GITHUB_ACTIONS throws, naming the missing programs and this file, and skips nothing", () => {
  const warnings = warningsFrom(() => {
    expect(() => underActions("true", () => skipOutsideCI(ABSENT, "sh", ABSENT_TOO))).toThrow(
      `The tests in ${import.meta.path} cannot run on this CI runner. This host does not have: ${ABSENT}, ${ABSENT_TOO}.`
    );
  });
  expect(warnings).toEqual([]);
});

test("skipOutsideCIBecause takes the caller's own reason: none is false, one skips outside CI and throws in it", () => {
  expect(underActions("true", () => skipOutsideCIBecause(""))).toBe(false);
  let skipped: boolean | undefined;
  const warnings = warningsFrom(() => {
    skipped = underActions(undefined, () => skipOutsideCIBecause("This host has no libpq."));
  });
  expect(skipped).toBe(true);
  expect(warnings).toEqual([
    `Skipped the tests in ${import.meta.path}. This host has no libpq. Install what is missing and run the tests again.`,
  ]);
  expect(() => underActions("true", () => skipOutsideCIBecause("This host has no libpq."))).toThrow(
    `The tests in ${import.meta.path} cannot run on this CI runner. This host has no libpq.`
  );
});

test("requireHostTools passes where the host has every program and throws naming the missing ones, in CI or not", () => {
  for (const value of [undefined, "true"]) {
    underActions(value, () => requireHostTools("sh"));
    expect(() => underActions(value, () => requireHostTools(ABSENT, "sh", ABSENT_TOO))).toThrow(
      `This test needs ${ABSENT}, ${ABSENT_TOO} on PATH, which this host does not have; it fails rather than skips.`
    );
  }
});
