/** `bun e2e/harness-up.ts`: one stack, left running, with its variables on stdout as shell
 *  exports. It does not exit when it has printed them - the stack lives as long as it does - so a
 *  caller starts it in the background, reads its output down to the
 *  `DISPATCH_E2E_REUSE_SERVERS` line, applies those lines to its own environment, runs whatever
 *  addresses the stack, and sends it a SIGTERM when that is done.
 *
 *  `e2e/visual-run.sh` reads the output through a fifo rather than a file: it makes the fifo,
 *  redirects this process into it, holds the read end open for the whole run, and runs each
 *  `export` line it reads with `eval` until the `DISPATCH_E2E_REUSE_SERVERS` line arrives. A
 *  file would have to be polled for that line, and a reader that closed the pipe would kill
 *  this process with SIGPIPE if it ever wrote again.
 *
 *  A person types the same three steps against a file, from `packages/dispatch`:
 *
 *    bun e2e/harness-up.ts > exports.sh &
 *    …wait for the last line, `export DISPATCH_E2E_REUSE_SERVERS=1`, then `. ./exports.sh`…
 *    bunx playwright test --config e2e/playwright.config.ts --ui
 *    kill "$!"
 *
 *  It has two callers. `e2e/visual-run.sh` runs its Playwright inside the pinned container, and
 *  that image carries the three browser engines and node, and no bun, no psql and no Go
 *  toolchain, so the run inside it cannot start a stack of its own: `globalSetup` would have
 *  nothing to build the server with and nothing to seed the database through. The script
 *  therefore starts the stack on the host through this, hands the container the variables
 *  below, and the run inside reads `DISPATCH_E2E_REUSE_SERVERS=1` and starts nothing
 *  (`e2e/global-setup.ts`). The other caller is a person who wants Playwright's UI mode against
 *  a stack that outlives one session: a UI session starts stacks of its own when a client
 *  connects, and the lines above hand it one that is up already instead.
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
