import { afterAll, beforeAll } from "bun:test";
import { captureEnvironment, type EnvironmentRestore } from "./environment";
import { temporaryDirectory } from "./temporary-directories";

/**
 * Reserves one directory for the whole test file, points `TMPDIR` at it before the file's first
 * test, and returns its path: every scratch directory a test of the file makes through `tmpdir()`
 * lands inside it, so one `afterAll` removal takes them all, and a file the suite forgot is still
 * gone when the run ends.
 *
 * In `afterAll` it puts `TMPDIR` back to what the caller had, deletes the name again when the
 * caller had none, and removes the directory.
 *
 * Call it at the test file's module scope, above anything that reads `tmpdir()`: it registers its
 * hooks where it is called, and a call inside a test body would point `TMPDIR` at a directory the
 * tests declared before it already ran without. The directory is made, and `TMPDIR` is written,
 * in a `beforeAll`, not at the call: a run that selects no row of the file — `bun test -t
 * <pattern>` — evaluates the file's body and runs neither its `beforeAll` nor its `afterAll`, so
 * a `TMPDIR` written at the call would stay written for every later file of the same process.
 * Nothing exists at the path, and `TMPDIR` still holds the caller's value, until that `beforeAll`
 * has run; a caller that stages something under the root does it in a hook of its own, registered
 * below this call.
 *
 * The path is `realpathSync`'d, so it compares equal to what a child process reports on a machine
 * whose `tmpdir()` is a symlink.
 *
 * Both hooks carry an explicit hang guard — a limit of their own rather than Bun's unwritten 5 s
 * default, which has failed correct runs of the suites that call this fixture — so a capture or
 * a restore that never returns reddens with its test named; the comments beside them have the
 * measurements they are sized from.
 */
export function temporaryTmpdir(prefix: string): string {
  const root = temporaryDirectory(prefix, { real: true });
  let restore: EnvironmentRestore | undefined;
  // A hang guard, not a bound on the work: what can hang is capturing the environment and
  // writing TMPDIR. The body is one read and two assignments: in pi-envoy's askgate.test.ts on
  // this two-CPU pod 0.07 / 0.07 / 0.10 ms (min / median / max) over 3 idle runs and 0.10 /
  // 0.18 / 0.31 ms over 10 runs at 240 busy loops (120 per CPU). So the limit is sized from what
  // Bun charges a hook rather than from that body: its unwritten 5 s default is what failed
  // correct runs of these suites at 5015.84 ms, 11035.86 ms and 15887.94 ms, charges against
  // hooks whose own work was a fraction of this one's, since a starved host charges a hook for
  // the time it is not scheduled. 60_000 is 3.8 times the worst of those charges, and sits well
  // inside the consumers' timeout-minutes: 10 (.github/workflows/ci.yaml, 600 s), so a capture
  // that never returns reddens with its test named instead of cancelling the job, which names
  // nothing.
  beforeAll(() => {
    // `??=`, so a `beforeAll` that runs a second time without its `afterAll` between captures
    // nothing: it would capture the root this fixture set, and the restore would then point
    // every later file of the process at a root this one removed.
    restore ??= captureEnvironment(["TMPDIR"]);
    process.env["TMPDIR"] = root;
  }, 60_000);
  // A hang guard, not a bound on the work: what can hang is restoring the environment. Timed
  // round the body in pi-envoy's askgate.test.ts on this two-CPU pod: 0.10 / 0.10 / 0.22 ms
  // (min / median / max) over 3 idle runs. At 240 busy loops (120 per CPU) it never ran at all,
  // because the removal registered above it was charged past Bun's unwritten 5 s default in all
  // 10 runs and Bun drops the rest of a scope's `afterAll` hooks once one times out, so a run
  // that loses that removal also leaves TMPDIR pointed at the directory it removed. That default
  // is what failed correct runs of these suites at 5015.84 ms, 11035.86 ms and 15887.94 ms,
  // charges against hooks whose own work was a fraction of this one's, so the limit is sized
  // from them: 60_000 is 3.8 times the worst, and sits well inside the consumers'
  // timeout-minutes: 10 (.github/workflows/ci.yaml, 600 s), so a restore that never returns
  // reddens with its test named instead of cancelling the job, which names nothing.
  afterAll(() => {
    restore?.();
    restore = undefined;
  }, 60_000);
  return root;
}
