import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

/** `packages/dispatch`, where the suite's own command runs, so the child reads whatever
 *  `bunfig.toml` that directory holds. */
const PACKAGE_DIR = join(import.meta.dir, "..", "..", "..");

/** Bun runs a file whose name matches no test pattern only when its path starts with `./`. */
const FIXTURE = "./web/src/__tests__/act-rule.fixture.tsx";

// Kept away from the assertion, so the source excerpt Bun prints when it fails holds no React text.
const NOT_CONFIGURED = "not configured to support act(";
const NOT_WRAPPED = "not wrapped in act(";

/** The `test:web` script's words after its leading `bun`: the preloads and flags of every run. */
function suiteArguments(): string[] {
  const manifest = JSON.parse(readFileSync(join(PACKAGE_DIR, "package.json"), "utf8")) as {
    scripts: { "test:web": string };
  };
  const [runner, ...args] = manifest.scripts["test:web"].split(" ");
  expect(runner).toBe("bun");
  return args;
}

/** A row's name in its `(fail)` line, without the time Bun prints after it. */
const FAILED_ROW = /^\(fail\) (.+?)(?: \[[^\]]*\])?$/;

/**
 * Each failed row's report by the row's name: the lines from the one starting `error: ` that opens
 * the row's message down to its `(fail)` line. The rows are read by name, since the run may order
 * them at random. A line must start with `error: `: in CI Bun also prints a `::error ...` line
 * whose title holds `error: ` too, between the message and the `(fail)` line. A report never
 * reaches above the row printed before it, so one row cannot be read with another's message.
 */
function failedRows(output: string): Map<string, string> {
  const reports = new Map<string, string>();
  let report: string[] = [];
  for (const line of output.split("\n")) {
    if (line.startsWith("error: ")) report = [line];
    else if (report.length > 0) report.push(line);
    const failed = FAILED_ROW.exec(line);
    if (failed?.[1] !== undefined) reports.set(failed[1], report.join("\n"));
    if (failed !== null || line.startsWith("(pass) ")) report = [];
  }
  return reports;
}

// The rule proven end to end: the real `setup.ts`, through the suite's own command, against the
// messages React really prints. A React upgrade that changes the text of the two messages the
// fixture draws fails this row, where every other row would go on passing with the rule silent.
// The child's output is read, not printed, so this file's own log holds none of React's act text.
test("the act rule fails each row React printed an act message in, and the file after its last row", async () => {
  // FORCE_COLOR would make Bun write `✗` where a failed row reads `(fail)`, and NO_COLOR keeps the
  // lines free of colour codes.
  const { FORCE_COLOR: _forced, ...inherited } = process.env;
  const child = Bun.spawn([process.execPath, ...suiteArguments(), FIXTURE], {
    cwd: PACKAGE_DIR,
    env: { ...inherited, NO_COLOR: "1" },
    stderr: "pipe",
    stdout: "ignore",
  });
  // Bun writes the rows, their errors and the summary to stderr; stdout holds only its banner, so
  // it is not read.
  const [output, exitCode] = await Promise.all([new Response(child.stderr).text(), child.exited]);

  // The summary counts the late update as a row of its own.
  const ran = {
    exitCode,
    fail: /^ *(\d+) fail$/m.exec(output)?.[1],
    pass: /^ *(\d+) pass$/m.exec(output)?.[1],
  };
  expect(ran).toEqual({ exitCode: 1, fail: "3", pass: "1" });
  const failed = failedRows(output);
  expect([...failed.keys()].sort()).toEqual([
    "(unnamed)",
    "a state set outside act fails its row",
    "an act inside a waitFor callback fails its row",
  ]);
  const outside = failed.get("a state set outside act fails its row");
  expect(outside).toContain("error: React printed 1 act message(s) in this row:");
  expect(outside).toContain("update outside act in CountSetOutsideAct");
  const inWaitFor = failed.get("an act inside a waitFor callback fails its row");
  expect(inWaitFor).toContain("error: React printed 1 act message(s) in this row:");
  expect(inWaitFor).toContain("act environment off");
  const afterLastRow = failed.get("(unnamed)");
  expect(afterLastRow).toContain(
    "error: React printed 1 act message(s) after this file's last row:"
  );
  expect(afterLastRow).toContain("update outside act in LateTimerCount");
  // React's own lines, counted over the whole output so the count holds in any row order:
  // CountSetOutsideAct's and LateTimerCount's "not wrapped" lines, and the waitFor row's "not
  // configured" line. Neither the rule's message nor the source excerpt Bun prints above it
  // repeats React's words, so a count of React's lines counts each message once; and the recorder
  // still prints every act message, so a recorder that swallowed them would read 0 here.
  expect({
    notConfigured: output.split(NOT_CONFIGURED).length - 1,
    notWrapped: output.split(NOT_WRAPPED).length - 1,
  }).toEqual({ notConfigured: 1, notWrapped: 2 });
});
