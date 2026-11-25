import { describe, expect, test } from "bun:test";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// hook-prompt-two-builds.sh's run_subtest reads one Go subtest's result from `go test -v`'s output
// and its exit status. A case takes only PASS or FAIL as a result. A skip is never a pass, since
// `go test` exits 0 for a test it skipped. A PASS line beside a failing exit (the parent test or
// the binary failed after the subtest) is no pass either. The rows drive the script's own function
// over a stand-in `go` on PATH that prints the row's output and exits with the row's status. The
// function before this rule read the PASS line alone, so the first row read PASS there.

const script = join(import.meta.dir, "..", "hook-prompt-two-builds.sh");
const subtest = "TheHookPromptsOfAReleasedRoleReachItsWokenSessionOnce";
const runSubtest = scriptFunctions(script)("run_subtest");

function read(output: string, status: number): string {
  const work = mkdtempSync(join(tmpdir(), "hook-prompt-two-builds-"));
  try {
    mkdirSync(join(work, "tree", "packages", "daemon"), { recursive: true });
    mkdirSync(join(work, "bin"));
    const go = join(work, "bin", "go");
    writeFileSync(
      go,
      `#!/usr/bin/env bash\ncat ${JSON.stringify(join(work, "output"))}\nexit ${status}\n`
    );
    chmodSync(go, 0o755);
    writeFileSync(join(work, "output"), output);
    const run = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -uo pipefail\nsubtest=${subtest}\n${runSubtest}\nrun_subtest "$1" "$1" "$2"`,
        "bash",
        join(work, "tree"),
        join(work, "log"),
      ],
      { env: { ...process.env, PATH: `${join(work, "bin")}:${process.env.PATH}` } }
    );
    expect(run.stderr.toString()).toBe("");
    return run.stdout.toString().trim();
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

describe("run_subtest", () => {
  test.each([
    [
      "a PASS line beside a failing exit",
      `    --- PASS: TestIdleReleaseAndWake/${subtest} (1s)\n--- FAIL: TestIdleReleaseAndWake (2s)\nFAIL\n`,
      1,
      "UNKNOWN",
    ],
    ["a skipped parent test", "--- SKIP: TestIdleReleaseAndWake (0s)\nPASS\nok\n", 0, "SKIP"],
    [
      "a skipped subtest",
      `    --- SKIP: TestIdleReleaseAndWake/${subtest} (0s)\nPASS\nok\n`,
      0,
      "SKIP",
    ],
    ["a run that printed no line of the subtest", "ok\n", 0, "UNKNOWN"],
    ["control: a pass", `    --- PASS: TestIdleReleaseAndWake/${subtest} (1s)\nok\n`, 0, "PASS"],
    [
      "control: a failure",
      `    --- FAIL: TestIdleReleaseAndWake/${subtest} (1s)\nFAIL\n`,
      1,
      "FAIL",
    ],
  ] as const)("reads %s", (_, output, status, want) => {
    expect(read(output, status)).toBe(want);
  });
});
