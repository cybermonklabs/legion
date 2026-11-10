/** `bun e2e/harness-up.ts`: one stack, left running, with its variables on stdout as shell
 *  exports. It does not exit when it has printed them - the stack lives as long as it does - so a
 *  caller starts it in the background, reads its output down to the
 *  `DISPATCH_E2E_REUSE_SERVERS` line, runs whatever addresses the stack, and sends it a SIGTERM
 *  when that is done:
 *
 *    bun e2e/harness-up.ts > "$exports" &
 *    …read "$exports" until the last line arrives, then `. "$exports"`…
 *    …run the tests…
 *    kill "$!"
 *
 *  It exists for `e2e/visual-run.sh`, whose Playwright runs inside the pinned container. That
 *  image carries the three browser engines and node, and no bun, no psql and no Go toolchain, so
 *  the run inside it cannot start a stack of its own: `globalSetup` would have nothing to build
 *  the server with and nothing to seed the database through. The script therefore starts the
 *  stack on the host through this, hands the container the variables below, and the run inside
 *  reads `DISPATCH_E2E_REUSE_SERVERS=1` and starts nothing (`e2e/global-setup.ts`).
 *
 *  `e2e/local-instance.ts` (`bun run dispatch:local`) keeps its own job and is not merged into
 *  this one: it is a Dispatch for a person to open, on the repository's dev Postgres, seeded with
 *  `e2e/workspace.ts` and printing a signed-in URL. This prints an environment and seeds nothing.
 *
 *  Every value is shell-quoted, since a `DATABASE_URL` carries characters a reader would
 *  otherwise have to think about (`e2e/local-instance-support.ts`). */
import { startStacks, stopStacks } from "./harness-pool";
import { shellQuote } from "./local-instance-support";

const [stack] = await startStacks(1);
for (const [variable, value] of Object.entries(stack.env)) {
  console.log(`export ${variable}=${shellQuote(value)}`);
}
console.log("export DISPATCH_E2E_REUSE_SERVERS=1");

// Until the caller stops it. The stack's processes are this process's children, so a signal that
// ends this run has to stop them rather than leave them serving loopback ports.
const interrupted = Promise.withResolvers<void>();
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.once(signal, () => interrupted.resolve());
}
await interrupted.promise;
// The stack wrote its output to this process's stderr rather than to a log directory, which is
// what a run of one stack does, so there are no logs to keep.
await stopStacks(false);
