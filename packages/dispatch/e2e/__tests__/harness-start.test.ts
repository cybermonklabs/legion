import { beforeAll, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded, watch, whether } from "./bounded-subprocess";

// How e2e/harness-start.ts's start ends. Its wait for each listener has no bound of its own
// (`serving`): it ends on the listener's report, on the listener's exit or its failure to start
// at all, or on the caller's abort, whose Interrupted names the listener it waited for, and each
// wait writes a line as it begins and another as it ends (`announceWait`). Each row but the
// SIGTERM row runs the start in a child of its own, as a deployed run's (`PLAYWRIGHT_BASE_URL`,
// read as the table is built), which starts the fake Envoy first, then the plain-HTTP proxy, and
// no server or database. The child prints what the start answered or threw.
const module = (name: string) => fileURLToPath(new URL(`../${name}`, import.meta.url));
const stubs = temporaryDirectory("harness-start-");
/** A `bun` that holds the fake Envoy's start for good, once it has sent its parent, the child,
 *  SIGUSR2, and runs every other bun command as the real bun. */
const heldBun = join(stubs, "held");
/** The same hold, which writes its pid into `HELD_MARK` before it holds rather than signal its
 *  parent, for a parent that handles no SIGUSR2 (e2e/harness-up.ts). */
const quietlyHeldBun = join(stubs, "held-quietly");
/** A directory holding nothing, for a PATH on which no `bun` is found. */
const noBun = join(stubs, "empty");
/** A preload under which every read of `Date.now()` is ten minutes later than the one before, as
 *  on a machine so loaded that each step of the start takes that long. */
const leapingClock = join(stubs, "leaping-clock.ts");

beforeAll(() => {
  mkdirSync(heldBun);
  mkdirSync(quietlyHeldBun);
  mkdirSync(noBun);
  writeFileSync(
    join(quietlyHeldBun, "bun"),
    `#!/bin/sh
case "$1" in
  */e2e/fake-envoy.ts) echo $$ > "$HELD_MARK"; exec sleep 600 ;;
esac
exec "$REAL_BUN" "$@"
`
  );
  chmodSync(join(quietlyHeldBun, "bun"), 0o755);
  writeFileSync(
    join(heldBun, "bun"),
    `#!/bin/sh
case "$1" in
  */e2e/fake-envoy.ts) kill -USR2 "$PPID"; exec sleep 600 ;;
esac
exec "$REAL_BUN" "$@"
`
  );
  chmodSync(join(heldBun, "bun"), 0o755);
  writeFileSync(
    leapingClock,
    `const real = Date.now.bind(Date);
let ahead = 0;
Date.now = () => real() + (ahead += 600_000);
`
  );
});

/** Runs `startHarness` in a child under `path`, with `preload` where one is named, aborting the
 *  start on SIGUSR2, and answers what the start answered or threw. */
async function started(path: string, preload?: string) {
  const script =
    `const { startHarness } = await import(${JSON.stringify(module("harness-start.ts"))});` +
    `const { Interrupted } = await import(${JSON.stringify(module("local-instance-support.ts"))});` +
    "const stopping = new AbortController();" +
    'process.once("SIGUSR2", () => stopping.abort());' +
    "try { const harness = await startHarness({ signal: stopping.signal });" +
    " console.log(JSON.stringify({ started: Object.keys(harness.env).sort() })); await harness.stop(); }" +
    "catch (error) { console.log(JSON.stringify({ interrupted: error instanceof Interrupted, " +
    "message: error.message })); }";
  const run = await runBounded({
    argv: [process.execPath, ...(preload ? ["--preload", preload] : []), "--eval", script],
    bound: "short",
    env: {
      PATH: path,
      PLAYWRIGHT_BASE_URL: "http://127.0.0.1:9/",
      REAL_BUN: process.execPath,
      TMPDIR: stubs,
    },
    from: import.meta.url,
  });
  expect(run.code, `${run.named} exited ${run.code}:\n${run.stderr}`).toBe(0);
  return {
    ...run,
    read: JSON.parse(run.stdout) as { started?: string[]; interrupted?: boolean; message?: string },
  };
}

test(
  "a start its caller aborts while a listener has not reported names that listener",
  async () => {
    const { named, read, stderr, stdout } = await started(`${heldBun}:${process.env.PATH ?? ""}`);
    expect(read.interrupted, `${named} printed ${stdout}`).toBe(true);
    expect(read.message, stderr).toMatch(
      /^interrupted \d+\.\d s into the wait, before the fake Envoy reported its port$/
    );
    // The wait named what it waited for as it began, which is what a run killed outright leaves,
    // and wrote that it ended once the abort ended it.
    expect(stderr).toMatch(
      /e2e\/harness-start\.ts: waiting, with no bound of its own, until the fake Envoy \(pid \d+\) reports its port\n(.*\n)*e2e\/harness-start\.ts: no longer waiting \(after \d+\.\d s\) until the fake Envoy \(pid \d+\) reports its port\n/
    );
  },
  rowLimit("short")
);

// A SIGTERM that ends e2e/harness-up.ts while its start waits, as with-harness.sh's cleanup and a
// job's limit send it: the pool stops the start and ends the process by the signal, which it
// raises again, so the start's failure is written by the pool before that, not left to a report of
// the rejection that the signal would cut short. The fake Envoy's start is held, and the SIGTERM is
// sent once the stand-in has written that it holds it. harness-up starts the pool's one stack, so
// its waits' lines name `stack 0`. The row signals harness-up while it runs, so it starts it
// outside `runBounded` (`bounded-subprocess.ts` lists it), under the row's own limit.
test(
  "a SIGTERM to e2e/harness-up.ts while a listener has not reported writes what the start waited for",
  async () => {
    const mark = join(stubs, "held-for-sigterm");
    const run = Bun.spawn([process.execPath, "e2e/harness-up.ts"], {
      cwd: fileURLToPath(new URL("../..", import.meta.url)),
      env: {
        HELD_MARK: mark,
        HOME: process.env.HOME,
        PATH: `${quietlyHeldBun}:${process.env.PATH ?? ""}`,
        PLAYWRIGHT_BASE_URL: "http://127.0.0.1:9/",
        REAL_BUN: process.execPath,
        TMPDIR: stubs,
      },
      stdout: "pipe",
      stderr: "pipe",
    });
    const ended = watch(run.exited);
    try {
      const stderr = new Response(run.stderr).text();
      const stdout = new Response(run.stdout).text();
      expect(
        await whether(() => existsSync(mark), ended),
        "the fake Envoy's start was never held"
      ).toBe(true);
      run.kill("SIGTERM");
      expect(await whether(ended), "harness-up still ran after its SIGTERM").toBe(true);
      await stdout;
      const written = await stderr;
      expect(written).toMatch(
        /e2e\/harness-pool\.ts: the start ended: interrupted \d+\.\d s into the wait, before the fake Envoy reported its port/
      );
      expect(written).toMatch(
        /e2e\/harness-start\.ts: stack 0: waiting, with no bound of its own, until the fake Envoy \(pid \d+\) reports its port\n/
      );
      expect(written).toMatch(
        /e2e\/harness-start\.ts: stack 0: no longer waiting \(after \d+\.\d s\) until the fake Envoy \(pid \d+\) reports its port\n/
      );
      expect(run.signalCode, `harness-up ended ${run.exitCode}`).toBe("SIGTERM");
    } finally {
      if (!ended()) {
        run.kill("SIGKILL");
        // harness-up killed outright stops nothing it started, and the held stand-in leads a
        // process group of its own, so it would sleep on for its 600 s. Its pid is the one it
        // wrote; harness-up stops it on any other ending.
        const held = existsSync(mark) ? Number(readFileSync(mark, "utf8").trim()) : Number.NaN;
        if (Number.isInteger(held) && held > 0) {
          try {
            process.kill(held, "SIGKILL");
          } catch {
            // gone already
          }
        }
      }
    }
  },
  rowLimit("short", "short")
);

// Under Bun a command that cannot be started leaves `exitCode` and `signalCode` null, so a wait
// that read only those would wait for ever; the start is refused at once, naming the error.
test(
  "a listener whose command is not on PATH is refused at once, naming why it could not start",
  async () => {
    const { named, read, stderr, stdout } = await started(noBun);
    expect(read.interrupted, `${named} printed ${stdout}`).toBe(false);
    expect(read.message).toStartWith(
      "The Dispatch e2e harness cannot start: the fake Envoy could not be started: "
    );
    // Bun says `Executable not found in $PATH: "bun"`, and Node `spawn bun ENOENT`.
    expect(read.message).toMatch(/Executable not found in \$PATH: "bun"|spawn bun ENOENT/);
    // A process that never started has no pid, and its wait's line names none.
    expect(stderr).toContain(
      "e2e/harness-start.ts: waiting, with no bound of its own, until the fake Envoy reports its port\n"
    );
    expect(stderr).not.toContain("(pid undefined)");
  },
  rowLimit("short")
);

// The waits keep no clock: a start whose every step reads ten minutes later than the last, which
// failed a bound of 30 s at its first read, still waits for each listener's report.
test(
  "a start under a clock ten minutes later at every read still waits for each listener's report",
  async () => {
    const { named, read, stderr, stdout } = await started(process.env.PATH ?? "", leapingClock);
    expect(read.message, `${named} printed ${stdout}:\n${stderr}`).toBeUndefined();
    expect(read.started).toEqual(["FAKE_ENVOY_PORT", "PLAIN_HTTP_PORT"]);
    // Each listener's wait wrote that it began and that it ended.
    for (const listener of ["the fake Envoy", "the plain-HTTP proxy"]) {
      expect(stderr).toMatch(
        new RegExp(
          `no longer waiting \\(after \\d+\\.\\d s\\) until ${listener} \\(pid \\d+\\) reports its port\\n`
        )
      );
    }
  },
  rowLimit("short")
);
