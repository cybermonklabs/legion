import { expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { type HostGateNeeds, hostTool, requireHostTools, skipWithout } from "./host-tools";
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
 * The test file gatedRun runs, as constant text: a gated row, a gated block of two rows, and an
 * ungated row, each of whose bodies appends its own name to the file HOSTGATE_MARKS names. The
 * module it gates with (HOSTGATE_MODULE) and what the gate needs (HOSTGATE_NEEDS, JSON) are read
 * from its environment, so no value is spliced into its code.
 */
const GATED_TEST = `import { test } from "bun:test";
import { appendFileSync } from "node:fs";

const env = process.env;
const mark = (name) => () => appendFileSync(env.HOSTGATE_MARKS, name + "\\n");
// The file lives in a scratch directory, so a static import would need host-tools.ts's path
// written into this text; it is read from the environment instead.
const { hostGate } = await import(env.HOSTGATE_MODULE);
const gate = hostGate(JSON.parse(env.HOSTGATE_NEEDS));
gate.test("gated row", mark("gated row"));
gate.describe("gated block", () => {
  test("first", mark("gated block > first"));
  test("second", mark("gated block > second"));
});
test("ungated row", mark("ungated row"));
`;

/**
 * Runs GATED_TEST in a `bun test` of its own under `hostGate(needs)`. Each row's body appends its
 * name to a file beside the test, which is read back as `ran`: a gate that declared a row but
 * dropped its body would print the row's pass line and leave no name there. GITHUB_ACTIONS is set
 * to `actions`, or left unset for undefined, and nothing else of this process's environment
 * reaches it but PATH, HOME and the file's own three inputs, so Bun prints each row's own pass,
 * skip or fail line.
 */
function gatedRun(needs: HostGateNeeds, actions: string | undefined) {
  const directory = makeDirectory("gated");
  const caller = join(directory, "gated.test.ts");
  const marks = join(directory, "ran.txt");
  writeFileSync(caller, GATED_TEST);
  const run = Bun.spawnSync([process.execPath, "test", caller], {
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: process.env["HOME"] ?? "",
      HOSTGATE_MARKS: marks,
      HOSTGATE_MODULE: join(import.meta.dir, "host-tools.ts"),
      HOSTGATE_NEEDS: JSON.stringify(needs),
      ...(actions === undefined ? {} : { GITHUB_ACTIONS: actions }),
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const ran = existsSync(marks)
    ? readFileSync(marks, "utf8").split("\n").filter(Boolean).sort()
    : [];
  return { caller, code: run.exitCode, said: run.stdout.toString() + run.stderr.toString(), ran };
}

test("hostGate declares bun's own rows where nothing it needs is missing, and runs each one's body, in CI or not", () => {
  for (const actions of [undefined, "true"]) {
    const r = gatedRun({ programs: ["sh"], never: ["sh"] }, actions);
    expect({ actions, code: r.code }).toEqual({ actions, code: 0 });
    for (const row of ["gated row", "gated block > first", "gated block > second", "ungated row"]) {
      expect(r.said).toContain(`(pass) ${row}`);
    }
    expect({ actions, ran: r.ran }).toEqual({
      actions,
      ran: ["gated block > first", "gated block > second", "gated row", "ungated row"],
    });
    expect(r.said).not.toContain("Skipped the tests");
  }
});

test("hostGate outside CI skips each gated row, with one warning naming the missing programs and the file", () => {
  const r = gatedRun({ programs: [ABSENT, "sh"] }, undefined);
  expect(r.code).toBe(0);
  expect(r.said).toContain(
    `Skipped the tests in ${r.caller}. This host does not have: ${ABSENT}. Install what is missing and run the tests again.`
  );
  for (const row of ["gated row", "gated block > first", "gated block > second"]) {
    expect(r.said).toContain(`(skip) ${row}`);
  }
  expect(r.said).toContain("(pass) ungated row");
  expect(r.ran).toEqual(["ungated row"]);
});

test("hostGate under GITHUB_ACTIONS fails each gated row by its own name, naming the missing programs, and skips none", () => {
  const r = gatedRun({ programs: [ABSENT, ABSENT_TOO] }, "true");
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
  expect(r.ran).toEqual(["ungated row"]);
});

test("hostGate takes the caller's own reason under the same rule", () => {
  const needs = { reason: "This host has no libpq." };
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
    const r = gatedRun({ never: [ABSENT] }, actions);
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
