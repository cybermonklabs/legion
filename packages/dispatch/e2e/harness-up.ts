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

// Until the caller stops it. SIGINT is this script's own: the pool hands that
// signal back once its stacks are up (`e2e/harness-pool.ts`, `unwatchInterrupt`), so from here
// on nothing else handles it, and the stack's processes are this process's children, which a
// Ctrl-C would otherwise leave serving loopback ports.
//
// SIGTERM is not listened for here. The pool keeps its own listener for that one as long as the
// stack is up, and that listener stops this same stack and then re-raises the signal, so this
// process ends with SIGTERM's own disposition. A second listener for it would stop one stack
// twice at once: the other caller finds the Supervisor's children already taken and goes on to
// `pg_ctl -m immediate stop` and to clear the run directory under a server that is still
// shutting down, which the server reports as a failed deregistration (dispatch://INFRA-1410).
const interrupted = Promise.withResolvers<void>();
process.once("SIGINT", () => interrupted.resolve());
await interrupted.promise;
// The stack wrote its output to this process's stderr rather than to a log directory, which is
// what a run of one stack does, so there are no logs to keep.
await stopStacks(false);
