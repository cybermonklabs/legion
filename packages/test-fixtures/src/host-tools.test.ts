import { expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { basename, delimiter, dirname, join, sep } from "node:path";
import { restoreEnvironment } from "./environment";
import {
  type HostGateNeeds,
  hostTool,
  pythonInterpreter,
  requireHostTools,
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

// A child under a HOME of the test's own, with no python3 on its PATH at all: an interpreter
// resolved by name there would fail, or be another one. Skipped where the host has no python3.
test.skipIf(hostTool("python3") === null)(
  "pythonInterpreter names an interpreter that runs with no PATH under a HOME of its own",
  () => {
    const python = pythonInterpreter();
    expect(python.startsWith("/")).toBe(true);
    const run = Bun.spawnSync([python, "-I", "-c", "import sys; print(sys.executable)"], {
      env: { PATH: "", HOME: makeDirectory("home") },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect({ code: run.exitCode, stderr: run.stderr.toString() }).toEqual({ code: 0, stderr: "" });
    expect(run.stdout.toString().trim()).toBe(python);
  }
);

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

restoreEnvironment(["PATH"]);

/** A directory holding a stand-in `mise` and `tool`, a link to it, as mise installs its shims:
 *  `mise which tool` prints `which` and exits 0, or exits 1 where `which` is null. */
function miseShims(which: string | null): string {
  const directory = makeDirectory();
  const answer = which === null ? "exit 1" : `echo '${which}'; exit 0`;
  writeFileSync(
    join(directory, "mise"),
    `#!/bin/sh\n[ "$1" = which ] && { ${answer}; }\necho "mise ERROR shim" >&2\nexit 1\n`,
    { mode: 0o755 }
  );
  symlinkSync(join(directory, "mise"), join(directory, "tool"));
  return directory;
}

/** A directory holding an executable `tool` of its own. */
function realTool(): string {
  const directory = makeDirectory();
  writeFileSync(join(directory, "tool"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  return directory;
}

test("hostTool is the first program on PATH where no mise shim comes before it", () => {
  const [first, second] = [realTool(), realTool()];
  process.env["PATH"] = [first, second].join(delimiter);
  expect(hostTool("tool")).toBe(join(first, "tool"));
  expect(hostTool(ABSENT)).toBeNull();
});

test("hostTool takes what mise names for a shim, or the program after the shim on PATH", () => {
  const named = realTool();
  const after = realTool();
  process.env["PATH"] = [miseShims(join(named, "tool")), after].join(delimiter);
  expect(hostTool("tool")).toBe(join(named, "tool"));
  // `mise which` names nothing where no version of the tool is set; the shim would fall back.
  process.env["PATH"] = [miseShims(null), after].join(delimiter);
  expect(hostTool("tool")).toBe(join(after, "tool"));
  // A shim with nothing after it is no program at all.
  process.env["PATH"] = miseShims(null);
  expect(hostTool("tool")).toBeNull();
});

test("hostTool runs no mise for a program that is no link to it, even with a mise beside it", () => {
  // A host without mise, or one whose program is a real file: what is first on PATH is the answer,
  // and the walk never starts a process. The `mise` here records any start of it.
  const directory = realTool();
  const ran = join(makeDirectory(), "mise-ran");
  writeFileSync(join(directory, "mise"), `#!/bin/sh\n: > '${ran}'\nexit 1\n`, { mode: 0o755 });
  process.env["PATH"] = [directory, realTool()].join(delimiter);
  expect(hostTool("tool")).toBe(join(directory, "tool"));
  expect(existsSync(ran)).toBe(false);
});

test("hostTool takes a name that holds a path separator as one file, under the same rules", () => {
  const directory = realTool();
  const after = realTool();
  process.env["PATH"] = after;
  // The file itself, and not the PATH's `tool`.
  expect(hostTool(join(directory, "tool"))).toBe(join(directory, "tool"));
  expect(hostTool(join(directory, ABSENT))).toBeNull();
  // A mise shim named by its path is what mise names for it, never the shim itself.
  const named = realTool();
  expect(hostTool(join(miseShims(join(named, "tool")), "tool"))).toBe(join(named, "tool"));
  // A shim mise names nothing for is no program: no PATH entry follows a file the caller named.
  expect(hostTool(join(miseShims(null), "tool"))).toBeNull();
  // A file at or below `outside` is no answer either.
  expect(hostTool(join(directory, "tool"), directory)).toBeNull();
  expect(hostTool(join(directory, "tool"), after)).toBe(join(directory, "tool"));
});

test("hostTool skips a directory at or below the one it is told to stay outside", () => {
  const standIn = realTool();
  const below = join(standIn, "bin");
  mkdirSync(below);
  writeFileSync(join(below, "tool"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  const after = realTool();
  process.env["PATH"] = [standIn, below, after].join(delimiter);
  expect(hostTool("tool", standIn)).toBe(join(after, "tool"));
  expect(hostTool("tool")).toBe(join(standIn, "tool"));
});

test("hostTool returns an absolute path for a relative name or a relative PATH entry, as Bun.which does", () => {
  // A relative path means a different file from another working directory: a child started
  // elsewhere, or a link made from it, would address the wrong one.
  const directory = realTool();
  const before = process.cwd();
  try {
    process.chdir(directory);
    process.env["PATH"] = realTool();
    expect(hostTool(`.${sep}tool`)).toBe(join(directory, "tool"));
    process.chdir(dirname(directory));
    expect(hostTool(join(basename(directory), "tool"))).toBe(join(directory, "tool"));
    process.env["PATH"] = basename(directory);
    expect(hostTool("tool")).toBe(join(directory, "tool"));
  } finally {
    process.chdir(before);
  }
});

test("hostTool holds what a shim names to the directory it is told to stay outside", () => {
  // The shim lies outside the stand-in directory; what mise names for it lies inside.
  const standIn = realTool();
  const after = realTool();
  const shims = miseShims(join(standIn, "tool"));
  process.env["PATH"] = [shims, after].join(delimiter);
  // On PATH, the walk goes on to the next `tool`, as it does where mise names nothing.
  expect(hostTool("tool", standIn)).toBe(join(after, "tool"));
  expect(hostTool("tool")).toBe(join(standIn, "tool"));
  // Named by its path, the shim has no next entry, so it is no program.
  expect(hostTool(join(shims, "tool"), standIn)).toBeNull();
  expect(hostTool(join(shims, "tool"))).toBe(join(standIn, "tool"));
});
