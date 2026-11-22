import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded } from "./bounded-subprocess";

// What e2e/harness-up.ts does with a signal that stops its stack: a Ctrl-C, a closing terminal's
// SIGHUP or its quit key's SIGQUIT that lands the moment its stack is up, after the pool has taken
// its own SIGINT listener off (`unwatchInterrupt`) and before anything else has run, and a second
// signal that lands while the stack stops. Each stops the stack once, and the script then ends,
// exit 0, rather than dying of the signal's default action and leaving the stack's detached
// processes serving. The script runs whole, under Bun, with a stand-in for e2e/harness-pool.ts
// loaded in that module's place by a preload: the stand-in's start arms and takes off a SIGINT
// listener of its own, as the pool's does, then sends this process STAND_IN_UP_SIGNAL before it
// returns the stack; its stop sends STAND_IN_STOP_SIGNAL, where one is named, waits until the
// signal has been handled, and prints a line. The stand-in listens for no signal at the moment it
// sends one, so what meets each is the script's own listener or the default action, which ends the
// process as the signal arrives; the stop's wait is a listener it adds only once the signal is
// sent, which Bun runs with the script's when it hands the signal on. A script that arms its
// listener only after the start returns, listens for SIGINT alone, or listens once, has none at
// one of those moments, so the signal's default ends it and the stop never runs or never ends.
// Where a row names a signal the script is started ignoring, the shell ignores it first, as `nohup`
// ignores SIGHUP, and the stand-in's stop says whether the script still ignores it by then: a
// listener for it would have cleared the ignore and ended the script on the signal it was started
// to outlive.
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const stubs = mkdtempSync(join(tmpdir(), "harness-up-"));
afterAll(() => rmSync(stubs, { force: true, recursive: true }));

const pool = join(stubs, "harness-pool.ts");
writeFileSync(
  pool,
  `import { ignoresSignal } from ${JSON.stringify(fileURLToPath(import.meta.resolve("@legion/test-fixtures/held-process")))};

export async function startStacks(count: number) {
  const onInterrupt = (): void => {};
  process.on("SIGINT", onInterrupt);
  await Promise.resolve();
  process.off("SIGINT", onInterrupt);
  const signal = process.env.STAND_IN_UP_SIGNAL as NodeJS.Signals;
  console.error(\`stand-in: \${count} stack up, sending \${signal}\`);
  process.kill(process.pid, signal);
  return [{ env: { STAND_IN_PORT: "5433" } }];
}

export async function stopStacks(keepLogs: boolean) {
  console.log(\`stand-in: ignores SIGHUP \${ignoresSignal(process.pid, "SIGHUP")}\`);
  const again = process.env.STAND_IN_STOP_SIGNAL as NodeJS.Signals | undefined;
  if (again) {
    console.error(\`stand-in: stop begun, sending \${again}\`);
    const handled = Promise.withResolvers<void>();
    process.kill(process.pid, again);
    process.once(again, () => handled.resolve());
    await handled.promise;
  }
  console.log(\`stand-in: stopped, keepLogs=\${keepLogs}\`);
}
`
);
const preload = join(stubs, "preload.ts");
writeFileSync(
  preload,
  `import { plugin } from "bun";
import { readFileSync } from "node:fs";

plugin({
  name: "harness-pool stand-in",
  setup(build) {
    build.onLoad({ filter: /\\/e2e\\/harness-pool\\.ts$/ }, () => ({
      contents: readFileSync(${JSON.stringify(pool)}, "utf8"),
      loader: "ts",
    }));
  },
});
`
);

for (const row of [
  { name: "a Ctrl-C the moment the stack is up", up: "SIGINT" },
  { name: "a SIGHUP the moment the stack is up", up: "SIGHUP" },
  { name: "a SIGQUIT the moment the stack is up", up: "SIGQUIT" },
  { name: "a second Ctrl-C while the stack stops", up: "SIGINT", stop: "SIGINT" },
  { name: "a SIGHUP while the stack stops on a Ctrl-C", up: "SIGINT", stop: "SIGHUP" },
  { name: "a SIGQUIT while the stack stops on a Ctrl-C", up: "SIGINT", stop: "SIGQUIT" },
  { name: "a Ctrl-C to a script started ignoring SIGHUP", up: "SIGINT", ignored: "HUP" },
]) {
  test(
    `${row.name} stops the stack once, not the script alone`,
    async () => {
      const run = await runBounded({
        // Under a shell that sets the core size to 0 and execs the script, so a SIGQUIT that met
        // its default action would write no core into the tree.
        argv: [
          "/bin/sh",
          "-c",
          `${row.ignored === undefined ? "" : `trap '' ${row.ignored}; `}ulimit -c 0 && exec "$@"`,
          "sh",
          process.execPath,
          "--preload",
          preload,
          "e2e/harness-up.ts",
        ],
        bound: "short",
        cwd: dispatchPackage,
        env: {
          HOME: process.env.HOME,
          PATH: process.env.PATH,
          STAND_IN_STOP_SIGNAL: row.stop,
          STAND_IN_UP_SIGNAL: row.up,
          TMPDIR: stubs,
        },
        from: import.meta.url,
      });
      expect(run.stderr, "the stand-in pool was not the one loaded").toContain(
        `stand-in: 1 stack up, sending ${row.up}`
      );
      expect(run.stdout).toContain("export STAND_IN_PORT=5433");
      if (row.ignored !== undefined) {
        expect(
          run.stdout,
          `${run.named}, started with SIG${row.ignored} ignored, no longer ignored it by its stop:\n${run.stderr}`
        ).toContain(`stand-in: ignores SIG${row.ignored} true`);
      }
      expect(
        run.stdout.split("\n").filter((line) => line === "stand-in: stopped, keepLogs=false"),
        `${run.named} did not stop the stack exactly once:\n${run.stderr}`
      ).toHaveLength(1);
      expect(run.code, `${run.named} ended ${run.code}:\n${run.stderr}`).toBe(0);
    },
    rowLimit("short")
  );
}
