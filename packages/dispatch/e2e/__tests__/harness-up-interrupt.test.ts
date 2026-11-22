import { beforeAll, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { ignoresSignal, skipWithoutIgnoreMasks } from "@legion/test-fixtures/held-process";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, watch, whether } from "./bounded-subprocess";

// What e2e/harness-up.ts over the stack pool does with a Ctrl-C followed by a SIGTERM, which is
// what e2e/visual-run.sh and e2e/acceptance/with-harness.sh send it on a person's Ctrl-C: the
// terminal's SIGINT reaches the whole foreground group, harness-up among them, and the script's
// EXIT trap then sends harness-up a SIGTERM (`kill "$harness"`). Measured against a real stack
// before this row, harness-up stopped it twice at once, once for each signal, and the second stop
// stopped its Postgres under a Dispatch server that was still shutting down. It must stop the
// stack once.
//
// The pool runs for real, under Bun, with a stand-in for e2e/harness-start.ts loaded in that
// module's place by a preload, and on a DATABASE_URL it is given, so no Postgres starts. The
// stand-in harness's stop prints a line as it begins, waits until this process has had SIGUSR2,
// and prints a line as it ends. The rows send SIGINT, SIGTERM and SIGUSR2 one after another once
// the stack is up. Linux hands a process the standard signals that are pending together lowest
// number first, SIGINT, SIGUSR2, SIGTERM, so the SIGTERM can be handled after the SIGUSR2; the
// stand-in therefore releases its stops one turn of the event loop after the SIGUSR2
// (`setImmediate`), which comes after every signal handled in that turn, so every stop the first
// two signals start has begun before any ends. Without that turn the stop the SIGINT began could
// end before the SIGTERM's began, and a harness that stops twice would show one stop. The
// stand-in harness puts this process's pid among the stack's variables, which harness-up prints,
// so each signal goes to harness-up itself. Each run is under a shell that sets the core size to
// 0.
//
// The last row is the background job's own: started as visual-run.sh starts it, harness-up must
// keep the SIGINT its shell started it ignoring, so a Ctrl-C alone stops nothing. It reads that
// from the kernel's mask of what the process ignores, which a listener for SIGINT clears, and is
// skipped by name where the mask cannot be read.
const withoutMasks = skipWithoutIgnoreMasks(
  "packages/dispatch/e2e/__tests__/harness-up-interrupt.test.ts"
);
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const stubs = temporaryDirectory("harness-up-interrupt-");
const start = join(stubs, "harness-start.ts");
const preload = join(stubs, "preload.ts");

beforeAll(() => {
  writeFileSync(
    start,
    `const released = Promise.withResolvers<void>();
process.on("SIGUSR2", () => setImmediate(() => released.resolve()));
export async function startHarness() {
  return {
    env: { STAND_IN_PID: String(process.pid) },
    processes: [],
    firstExit: () => new Promise<never>(() => {}),
    async stop() {
      console.log("stand-in harness: stop begun");
      await released.promise;
      console.log("stand-in harness: stopped");
    },
  };
}
`
  );
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
});

for (const row of [
  {
    // e2e/visual-run.sh and e2e/acceptance/with-harness.sh start harness-up as a background job
    // of a non-interactive shell, which starts it with SIGINT and SIGQUIT ignored.
    name: "a Ctrl-C and then a SIGTERM to e2e/harness-up.ts started as visual-run.sh starts it, a shell's background job, stop its stack once",
    shell: 'ulimit -c 0; "$@" & wait $!',
    ended: ["SIGTERM"],
    keepsSigint: false,
  },
  {
    // A person who runs it in a terminal's foreground starts it with SIGINT at its default.
    name: "a Ctrl-C and then a SIGTERM to e2e/harness-up.ts started with SIGINT at its default stop its stack once",
    shell: 'ulimit -c 0 && exec "$@"',
    ended: ["SIGTERM", 0],
    keepsSigint: false,
  },
  {
    // The SIGINT the shell started it ignoring is still ignored once its stack is up, so the
    // kernel drops a Ctrl-C to it at once and no stop can start from one.
    name: "a Ctrl-C to e2e/harness-up.ts started as a shell's background job finds SIGINT still ignored, and a SIGTERM stops its stack once",
    shell: 'ulimit -c 0; "$@" & wait $!',
    ended: ["SIGTERM"],
    keepsSigint: true,
  },
]) {
  test.skipIf(withoutMasks && row.keepsSigint)(
    row.name,
    async () => {
      const run = Bun.spawn(
        [
          "/bin/sh",
          "-c",
          row.shell,
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
      let pid: number | undefined;
      try {
        // stderr is read whole only once harness-up has ended; stdout as it comes, as
        // e2e/visual-run.sh reads it through its fifo.
        const stderr = new Response(run.stderr).text();
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
        pid = Number(/^export STAND_IN_PID=(\d+)$/m.exec(stdout)?.[1]);
        expect(Number.isInteger(pid) && pid > 0, `no pid in:\n${stdout}`).toBe(true);
        if (row.keepsSigint) {
          expect(
            ignoresSignal(pid, "SIGINT"),
            "harness-up, started with SIGINT ignored, armed a listener for it, which undid the ignore"
          ).toBe(true);
        }
        process.kill(pid, "SIGINT");
        process.kill(pid, "SIGTERM");
        process.kill(pid, "SIGUSR2");
        expect(await whether(ended), "harness-up still ran after its Ctrl-C and SIGTERM").toBe(
          true
        );
        await drained;
        const lines = stdout.split("\n");
        expect(
          {
            begun: lines.filter((line) => line === "stand-in harness: stop begun").length,
            stopped: lines.filter((line) => line === "stand-in harness: stopped").length,
          },
          `harness-up did not stop its stack exactly once:\n${stdout}\n${await stderr}`
        ).toEqual({ begun: 1, stopped: 1 });
        // A shell that waited for harness-up reports the signal that ended it as 128 plus its
        // number; where harness-up took the shell's place (`exec`), Bun reports the signal itself.
        const end: string | number =
          run.signalCode ?? (run.exitCode === 143 ? "SIGTERM" : (run.exitCode ?? -1));
        expect(
          row.ended as readonly (string | number)[],
          `harness-up ended ${run.exitCode} (signal ${run.signalCode})`
        ).toContain(end);
      } finally {
        if (!ended()) run.kill("SIGKILL");
        if (pid !== undefined && !ended()) {
          try {
            process.kill(pid, "SIGKILL");
          } catch {
            // gone already
          }
        }
      }
    },
    rowLimit("short", "short")
  );
}
