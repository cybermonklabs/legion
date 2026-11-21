import { describe, expect, test } from "bun:test";
import { join } from "node:path";

// lib/rig.sh's until_true: a bounded wait polls its command with the command's output hidden, so a
// command the script never defined answered exit 127 on every poll and read as a wait that ran
// out (a 120 s timeout per run of project-access.sh, whose walk polled a function its library no
// longer had). These cases source the rig as a proof does and time the wait.

const root = join(import.meta.dir, "..", "..", "..");

/** The shell a case runs: the rig as a proof sources it, with the note and the fail every proof
 *  defines, then until_true with $2 as its bound and the rest as its description and command. */
const rig = `set -euo pipefail
note() { :; }
fail() { echo "$*" >&2; exit 1; }
root=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
evidence=$work
timeout_hook=''
. "$root/scripts/e2e/lib/rig.sh"
shift
until_true "$@"
echo waited
`;

/** What until_true answered, and how long it took in seconds. */
function wait(...args: string[]): { code: number; out: string; err: string; seconds: number } {
  const started = performance.now();
  const run = Bun.spawnSync(["bash", "-c", rig, "rig", root, ...args], {
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: run.exitCode,
    out: run.stdout.toString().trim(),
    err: run.stderr.toString().trim(),
    seconds: (performance.now() - started) / 1000,
  };
}

describe("until_true", () => {
  test("a command the script never defined fails at once, naming it, not at the bound", () => {
    const answer = wait("5", "both servers to owe no vector", "indexed", "main", "twin");
    expect(answer.code).toBe(1);
    expect(answer.out).toBe("");
    expect(answer.err).toBe(
      "until_true: indexed is no function, builtin or program, so waiting for both servers to owe no vector could never end"
    );
    expect(answer.seconds).toBeLessThan(3);
  });

  test("a builtin that holds returns at once", () => {
    expect(wait("5", "the builtin", "true")).toMatchObject({ code: 0, out: "waited", err: "" });
  });

  test("a builtin that never holds still runs to its bound and fails timed out", () => {
    // $SECONDS counts whole seconds, so a bound of 2 waits more than one second of wall time.
    const answer = wait("2", "a file that is never written", "test", "-e", "/nonexistent/never");
    expect(answer.code).toBe(1);
    expect(answer.err).toMatch(
      /^timed out after 2s \(\d+ polls\) waiting for a file that is never written$/
    );
    expect(answer.seconds).toBeGreaterThanOrEqual(1);
  });
});
