import { closeSync, existsSync, openSync, renameSync, watch, writeFileSync } from "node:fs";
import { constants } from "node:os";
import { join } from "node:path";

// A gate in front of the command a CI job's step runs once per check, for a test that must change
// the world between two of those calls: ci-report.live.test.ts ends the run's result grant after
// the first write and before the second. ci-report runs its `--dispatch-cli` one call after
// another, so `--dispatch-cli "bun test/gated-cli.ts bun bin/dispatch.ts"` puts this script in
// front of each call:
//
//   bun test/gated-cli.ts COMMAND [ARGS...]
//
// It numbers its call n by the first `<n>.started` it can create in GATED_CLI_DIR, with O_EXCL,
// so no two calls share a number. Call 1 runs COMMAND at once. Each later call writes
// `<n>.waiting` and runs COMMAND once `release` is in the directory: one check, then fs.watch,
// with the watch started before the check, so a `release` written between the two is not missed.
// A call that sees no `release` within RELEASE_DEADLINE_MS runs nothing, writes no `<n>.exit`,
// says so on standard error and exits 3, so a test that never releases its gate leaves no call
// waiting past its own time limit.
// COMMAND gets this process's standard input, output and error and its environment. The script
// then writes COMMAND's exit status to `<n>.exit` (a signal's as 128 plus its number), whole, as a
// rename puts it in place, and exits with it. With no GATED_CLI_DIR or no COMMAND it runs nothing
// and exits 2. Test-only: nothing outside this package's tests runs it.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { GATED_CLI_DIR: gates = "" } = process.env;

/** How long a later call waits for `release`: under the 180 s limit of each row that gates its
 *  calls, so a row whose release never comes fails on its own and leaves no call behind it. */
const RELEASE_DEADLINE_MS = 120_000;

/** The number of this call: the first n whose `<n>.started` this process created. */
function claimNumber(dir: string): number {
  for (let n = 1; ; n++) {
    try {
      // "wx" is O_CREAT | O_EXCL | O_WRONLY: it fails with EEXIST where another call made the file.
      closeSync(openSync(join(dir, `${n}.started`), "wx"));
      return n;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    }
  }
}

/** Resolves once `release` is in `dir`, and rejects when it is not there within `deadlineMs`. */
function released(dir: string, deadlineMs: number): Promise<void> {
  const release = join(dir, "release");
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const watcher = watch(dir, () => {
    if (existsSync(release)) resolve();
  });
  watcher.on("error", reject);
  const deadline = setTimeout(
    () => reject(new Error(`no ${release} within ${deadlineMs} ms`)),
    deadlineMs
  );
  if (existsSync(release)) resolve();
  return promise.finally(() => {
    clearTimeout(deadline);
    watcher.close();
  });
}

/** Writes `text` to `file` whole: a reader sees no file or all of it. */
function writeWhole(file: string, text: string): void {
  writeFileSync(`${file}.part`, text);
  renameSync(`${file}.part`, file);
}

async function main(argv: readonly string[]): Promise<number> {
  if (gates === "" || argv.length === 0) {
    console.error("gated-cli: use GATED_CLI_DIR=<dir> bun gated-cli.ts COMMAND [ARGS...]");
    return 2;
  }
  const n = claimNumber(gates);
  if (n > 1) {
    writeWhole(join(gates, `${n}.waiting`), "");
    try {
      await released(gates, RELEASE_DEADLINE_MS);
    } catch (error) {
      console.error(`gated-cli: call ${n} runs nothing: ${String(error)}`);
      return 3;
    }
  }
  const child = Bun.spawn([...argv], { stdin: "inherit", stdout: "inherit", stderr: "inherit" });
  await child.exited;
  const status =
    child.exitCode ??
    128 + (child.signalCode === null ? 0 : (constants.signals[child.signalCode] ?? 0));
  writeWhole(join(gates, `${n}.exit`), `${status}\n`);
  return status;
}

process.exit(await main(process.argv.slice(2)));
