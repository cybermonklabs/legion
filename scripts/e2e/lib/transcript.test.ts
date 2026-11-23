import { describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// lib/transcript.sh's transcript_to, as an entry script runs it: sourced under `set -euo
// pipefail`, called before the run's first output, and followed by the run, here one line on
// standard output that stands in for a proof's PASS and one on standard error that stands in for
// its diagnostics. A transcript the helper cannot open must end the run there, naming the file, and
// never let the run go on to its PASS with no transcript of it.
const helper = join(import.meta.dir, "transcript.sh");
const dir = temporaryDirectory("transcript-test");
const pass = "stand-in e2e: PASS";
const diagnostic = "stand-in e2e: a diagnostic on standard error";

/**
 * The entry script's start: $1 is the helper, $2 the transcript, $3 the run's line on standard
 * output and $4 its line on standard error.
 */
const entry = `set -euo pipefail
. "$1"
transcript_to "$2"
echo "$3"
echo "$4" >&2
`;

/** The run with its transcript at file: its exit status and both streams. */
function run(file: string) {
  const result = Bun.spawnSync(["bash", "-c", entry, "run", helper, file, pass, diagnostic], {
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

describe("transcript_to", () => {
  test("a file it can open takes the run's every line from both streams, which still reach the reader, exit 0", () => {
    const file = join(dir, "transcript.log");
    const { code, stdout, stderr } = run(file);
    expect(readFileSync(file, "utf8")).toBe(`${pass}\n${diagnostic}\n`);
    expect(stdout).toBe(`${pass}\n${diagnostic}\n`);
    expect(stderr).not.toContain(diagnostic);
    expect(code).toBe(0);
  });

  test("a file whose directory does not exist ends the run before its PASS, naming the file", () => {
    const file = join(dir, "no-such-directory", "transcript.log");
    const { code, stdout, stderr } = run(file);
    expect(stdout).not.toContain(pass);
    expect(stderr).toContain(file);
    expect(code).not.toBe(0);
  });

  test("a directory where the file should be ends the run before its PASS, naming the file", () => {
    const file = join(dir, "a-directory.log");
    mkdirSync(file);
    const { code, stdout, stderr } = run(file);
    expect(stdout).not.toContain(pass);
    expect(stderr).toContain(file);
    expect(code).not.toBe(0);
  });
});
