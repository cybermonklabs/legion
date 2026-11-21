import { expect, test } from "bun:test";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, requireHostTools, skipWithout } from "./host-tools";
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

/**
 * Runs, in a `bun test` of its own, a test file whose rows are declared under `hostGate(needs)`: a
 * gated row, a gated block of two rows, and an ungated row. GITHUB_ACTIONS is set to `actions`, or
 * left unset for undefined, and nothing else of this process's environment reaches it but PATH
 * and HOME, so Bun prints each row's own pass, skip or fail line.
 */
function gatedRun(needs: string, actions: string | undefined) {
  const caller = join(makeDirectory("gated"), "gated.test.ts");
  writeFileSync(
    caller,
    `import { test } from "bun:test";\n` +
      `import { hostGate } from ${JSON.stringify(join(import.meta.dir, "host-tools.ts"))};\n` +
      `const gate = hostGate(${needs});\n` +
      `gate.test("gated row", () => {});\n` +
      `gate.describe("gated block", () => { test("first", () => {}); test("second", () => {}); });\n` +
      `test("ungated row", () => {});\n`
  );
  const run = Bun.spawnSync([process.execPath, "test", caller], {
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: process.env["HOME"] ?? "",
      ...(actions === undefined ? {} : { GITHUB_ACTIONS: actions }),
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  return { caller, code: run.exitCode, said: run.stdout.toString() + run.stderr.toString() };
}

test("hostGate declares bun's own rows where nothing it needs is missing, in CI or not", () => {
  for (const actions of [undefined, "true"]) {
    const r = gatedRun(`{ programs: ["sh"], never: ["sh"] }`, actions);
    expect({ actions, code: r.code }).toEqual({ actions, code: 0 });
    for (const row of ["gated row", "gated block > first", "gated block > second", "ungated row"]) {
      expect(r.said).toContain(`(pass) ${row}`);
    }
    expect(r.said).not.toContain("Skipped the tests");
  }
});

test("hostGate outside CI skips each gated row, with one warning naming the missing programs and the file", () => {
  const r = gatedRun(`{ programs: [${JSON.stringify(ABSENT)}, "sh"] }`, undefined);
  expect(r.code).toBe(0);
  expect(r.said).toContain(
    `Skipped the tests in ${r.caller}. This host does not have: ${ABSENT}. Install what is missing and run the tests again.`
  );
  for (const row of ["gated row", "gated block > first", "gated block > second"]) {
    expect(r.said).toContain(`(skip) ${row}`);
  }
  expect(r.said).toContain("(pass) ungated row");
});

test("hostGate under GITHUB_ACTIONS fails each gated row by its own name, naming the missing programs, and skips none", () => {
  const r = gatedRun(
    `{ programs: [${JSON.stringify(ABSENT)}, ${JSON.stringify(ABSENT_TOO)}] }`,
    "true"
  );
  expect(r.code).toBe(1);
  expect(r.said).toContain(
    `This test cannot run on this host. This host does not have: ${ABSENT}, ${ABSENT_TOO}. Under GITHUB_ACTIONS a test this machine cannot run fails, never skips.`
  );
  for (const row of ["gated row", "gated block > first", "gated block > second"]) {
    expect(r.said).toContain(`(fail) ${row}`);
  }
  expect(r.said).toContain("(pass) ungated row");
  expect(r.said).not.toContain("(skip)");
  expect(r.said).not.toContain("Skipped the tests");
});

test("hostGate takes the caller's own reason under the same rule", () => {
  const needs = `{ reason: "This host has no libpq." }`;
  const outside = gatedRun(needs, undefined);
  expect(outside.code).toBe(0);
  expect(outside.said).toContain(`Skipped the tests in ${outside.caller}. This host has no libpq.`);
  expect(outside.said).toContain("(skip) gated row");
  const inside = gatedRun(needs, "true");
  expect(inside.code).toBe(1);
  expect(inside.said).toContain("(fail) gated row");
  expect(inside.said).toContain("This test cannot run on this host. This host has no libpq.");
});

test("hostGate fails each gated row for a missing program of never on any host, naming it", () => {
  for (const actions of [undefined, "true"]) {
    const r = gatedRun(`{ never: [${JSON.stringify(ABSENT)}] }`, actions);
    expect({ actions, code: r.code }).toEqual({ actions, code: 1 });
    expect(r.said).toContain(
      `This test cannot run on this host. This host does not have: ${ABSENT}, which these tests never skip for. It fails rather than skips.`
    );
    for (const row of ["gated row", "gated block > first", "gated block > second"]) {
      expect(r.said).toContain(`(fail) ${row}`);
    }
    expect(r.said).toContain("(pass) ungated row");
  }
});

test("requireHostTools passes where the host has every program and throws naming the missing ones", () => {
  requireHostTools("sh");
  expect(() => requireHostTools(ABSENT, "sh", ABSENT_TOO)).toThrow(
    `This test needs ${ABSENT}, ${ABSENT_TOO} on PATH, which this host does not have; it fails rather than skips.`
  );
});
