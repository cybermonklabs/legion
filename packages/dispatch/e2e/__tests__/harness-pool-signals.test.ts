import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { constants, tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded, watch, whether } from "./bounded-subprocess";

// What e2e/harness-pool.ts does with a signal no Playwright runner handles, once its stack is up:
// a SIGTERM, a closing terminal's SIGHUP, or its quit key's SIGQUIT. Each stops the stack, once,
// and then ends the process with that signal's own disposition, where without the pool's listener
// its default action ended the process at once and left the stack's detached processes serving.
// A SIGHUP or a SIGQUIT that comes again while the stack stops changes nothing. The pool runs in a
// script of its own, under Bun, with a stand-in for e2e/harness-start.ts loaded in that module's
// place by a preload: the stand-in harness's stop sends STOP_SIGNAL, where one is named, waits until
// that signal is handled, and prints a line. The script starts one stack on a DATABASE_URL it names,
// so no Postgres starts, says whether it still ignores SIGHUP, and then sends itself UP_SIGNAL. It
// runs under a shell that sets the core size to 0, so a SIGQUIT that meets its default action
// writes no core into the tree. Where a row names a signal it ignores, the shell ignores that one
// first, as `nohup` ignores SIGHUP for the command it runs; it stays ignored once the stack is up,
// where a listener for it would have cleared the ignore and ended the run on the signal it was
// started to outlive.
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const stubs = mkdtempSync(join(tmpdir(), "harness-pool-signals-"));
afterAll(() => rmSync(stubs, { force: true, recursive: true }));

const start = join(stubs, "harness-start.ts");
writeFileSync(
  start,
  `export async function startHarness() {
  return {
    env: { STAND_IN_PORT: "5433" },
    processes: [],
    firstExit: () => new Promise<never>(() => {}),
    async stop() {
      const again = process.env.STOP_SIGNAL as NodeJS.Signals | undefined;
      if (again) {
        const handled = Promise.withResolvers<void>();
        process.kill(process.pid, again);
        process.once(again, () => handled.resolve());
        await handled.promise;
      }
      console.log("stand-in harness: stopped");
    },
  };
}
`
);
const preload = join(stubs, "preload.ts");
writeFileSync(
  preload,
  `import { plugin } from "bun";
import { readFileSync } from "node:fs";

plugin({
  name: "harness-start stand-in",
  setup(build) {
    build.onLoad({ filter: /\\/e2e\\/harness-start\\.ts$/ }, () => ({
      contents: readFileSync(${JSON.stringify(start)}, "utf8"),
      loader: "ts",
    }));
  },
});
`
);
const script = join(stubs, "pool.ts");
writeFileSync(
  script,
  `import { ignoresSignal } from ${JSON.stringify(fileURLToPath(import.meta.resolve("@legion/test-fixtures/held-process")))};
import { startStacks } from ${JSON.stringify(join(dispatchPackage, "e2e/harness-pool.ts"))};
await startStacks(1, { env: { DATABASE_URL: "postgres://stand-in.invalid/none" } });
console.log("stand-in: stack up");
console.log(\`stand-in: ignores SIGHUP \${ignoresSignal(process.pid, "SIGHUP")}\`);
process.kill(process.pid, process.env.UP_SIGNAL as NodeJS.Signals);
await new Promise(() => {});
`
);

for (const row of [
  { up: "SIGTERM" },
  { up: "SIGHUP" },
  { up: "SIGQUIT" },
  { up: "SIGHUP", stop: "SIGHUP" },
  { up: "SIGQUIT", stop: "SIGHUP" },
  { up: "SIGTERM", stop: "SIGQUIT" },
  { up: "SIGTERM", ignored: "HUP" },
] as const) {
  const ignored = "ignored" in row ? row.ignored : undefined;
  test(
    `a ${row.up}${"stop" in row ? ` and a ${row.stop} while the stack stops` : ""}${ignored === undefined ? "" : ` to a run started ignoring SIG${ignored}`} stops the pool's stack once and ends the process by the ${row.up}`,
    async () => {
      const run = await runBounded({
        argv: [
          "/bin/sh",
          "-c",
          `${ignored === undefined ? "" : `trap '' ${ignored}; `}ulimit -c 0 && exec "$@"`,
          "sh",
          process.execPath,
          "--preload",
          preload,
          script,
        ],
        bound: "short",
        cwd: dispatchPackage,
        env: {
          HOME: process.env.HOME,
          PATH: process.env.PATH,
          STOP_SIGNAL: "stop" in row ? row.stop : undefined,
          TMPDIR: stubs,
          UP_SIGNAL: row.up,
        },
        from: import.meta.url,
      });
      expect(run.stdout, `${run.named}:\n${run.stderr}`).toContain("stand-in: stack up");
      if (ignored !== undefined) {
        expect(
          run.stdout,
          `${run.named}, started with SIG${ignored} ignored, no longer ignored it once its stack was up:\n${run.stderr}`
        ).toContain(`stand-in: ignores SIG${ignored} true`);
      }
      expect(
        run.stdout.split("\n").filter((line) => line === "stand-in harness: stopped"),
        `${run.named} did not stop the stack exactly once:\n${run.stderr}`
      ).toHaveLength(1);
      expect(run.code, `${run.named} ended ${run.code}:\n${run.stderr}`).toBe(
        128 + constants.signals[row.up]
      );
    },
    rowLimit("short")
  );
}

// e2e/harness-up.ts over this pool, with the same stand-in harness: a SIGHUP or a SIGQUIT once its
// stack is up reaches both harness-up's own listener and the pool's, and each calls `stopStacks`.
// The two are one stop, since `stopStacks` waits for a stop under way: the stand-in harness's stop
// runs once, and the script then ends, exit 0, as a Ctrl-C ends it. Two stops of one stack at once
// would each take the other's work (`stopStacks`). The signal is sent once the script has printed
// its last export line, the moment its stack is up; the row reads the script's output as it comes,
// so it starts the script itself rather than through `runBounded`, which hands the output over
// only at the end, and kills it at the short bound should it never end.
for (const signal of ["SIGHUP", "SIGQUIT"] as const) {
  test(
    `a ${signal} to e2e/harness-up.ts over the pool stops its stack once`,
    async () => {
      const run = Bun.spawn(
        [
          "/bin/sh",
          "-c",
          'ulimit -c 0 && exec "$@"',
          "sh",
          process.execPath,
          "--preload",
          preload,
          "e2e/harness-up.ts",
        ],
        {
          cwd: dispatchPackage,
          env: {
            DATABASE_URL: "postgres://stand-in.invalid/none",
            HOME: process.env.HOME,
            PATH: process.env.PATH,
            TMPDIR: stubs,
          },
          stdout: "pipe",
          stderr: "pipe",
        }
      );
      const ended = watch(run.exited);
      try {
        // stderr is read whole only once harness-up has ended: its text settles at its end, so a
        // message that awaited it before the signal would wait for an end nothing has asked for.
        const stderr = new Response(run.stderr).text();
        // The output is read as it comes, and to its end, while the row waits on the line and then
        // on the exit, as harness-up's own caller reads it (e2e/visual-run.sh's fifo).
        const reader = run.stdout.getReader();
        const decoder = new TextDecoder();
        let stdout = "";
        const drained = (async () => {
          for (;;) {
            const { value, done } = await reader.read();
            if (done) return;
            stdout += decoder.decode(value);
          }
        })();
        expect(
          await whether(() => stdout.includes("export DISPATCH_E2E_REUSE_SERVERS=1\n"), ended),
          `the stack never came up; harness-up printed:\n${stdout}`
        ).toBe(true);
        run.kill(signal);
        expect(await whether(ended), `harness-up still ran after its ${signal}`).toBe(true);
        await drained;
        expect(
          stdout.split("\n").filter((line) => line === "stand-in harness: stopped"),
          `harness-up over the pool did not stop its stack exactly once on a ${signal}:\n${await stderr}`
        ).toHaveLength(1);
        expect(
          run.exitCode,
          `harness-up ended ${run.exitCode} (signal ${run.signalCode}):\n${await stderr}`
        ).toBe(0);
      } finally {
        if (!ended()) run.kill("SIGKILL");
      }
    },
    rowLimit("short", "short")
  );
}
