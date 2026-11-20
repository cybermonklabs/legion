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
import { ignoredAtStart } from "../../envoy-client/src/signal-ignores";
import { startStacks, stopStacks } from "./harness-pool";
import { shellQuote } from "./local-instance-support";

// SIGINT, SIGHUP and SIGQUIT are this script's own once the stack is up: the pool hands SIGINT
// back then (`e2e/harness-pool.ts`, `unwatchInterrupt`), a closing terminal sends SIGHUP and its
// quit key SIGQUIT, and the stack's processes are this process's children, each started
// detached, in a session of its own (`e2e/local-instance-support.ts`), so none of the three
// reaches them, and the stack would be left serving loopback ports. So each of the three stops
// the stack here.
//
// The listeners are armed before the start, never after it: the pool takes its own SIGINT
// listener off as the start returns, and Bun, as Node does, puts a signal back to its default
// action when its last listener goes, so an interrupt between that and a listener armed here
// afterwards would end this process by that default and leave the stack running
// (`e2e/__tests__/harness-up.test.ts`). They stay armed until this process ends, so a repeat of
// any of the three - a second Ctrl-C while the stack stops, the SIGHUP a terminal and then its
// shell send - finds a listener that does nothing more, rather than the default action half-way
// through the stop, and no second stop starts. A signal during the start is the pool's as well:
// it cancels the start, which stops what it began, and `startStacks` rejects. A signal this script
// was started with ignored, as `nohup bun e2e/harness-up.ts &` ignores SIGHUP, gets no listener
// here (`ignoredAtStart`) and none from the pool: a listener would end the script on the signal it
// was started to outlive, since taking one off brings back the default action, not the ignore.
const stopSignals = ["SIGINT", "SIGHUP", "SIGQUIT"] as const;
const interrupted = Promise.withResolvers<void>();
for (const signal of stopSignals) {
  if (!ignoredAtStart(signal)) process.on(signal, () => interrupted.resolve());
}
const [stack] = await startStacks(1);
for (const [variable, value] of Object.entries(stack.env)) {
  console.log(`export ${variable}=${shellQuote(value)}`);
}
console.log("export DISPATCH_E2E_REUSE_SERVERS=1");

// Until the caller stops it.
//
// The pool answers SIGTERM, SIGHUP and SIGQUIT itself as long as the stack is up, since a
// Playwright run's runner handles none of them: its listener stops this same stack and then
// sends the signal again. Its stop and the one below are one (`stopStacks` waits for a stop under
// way), so a SIGHUP or a SIGQUIT stops the stack once, and the signal the pool sends again finds
// the listener above and does nothing: this script ends as a Ctrl-C ends it. SIGTERM is the pool's
// alone, so this process ends with SIGTERM's own disposition once the stack is stopped, and a
// second SIGTERM ends it at once, the pool's rule for a sender that will not wait.
await interrupted.promise;
// The stack wrote its output to this process's stderr rather than to a log directory, which is
// what a run of one stack does, so there are no logs to keep.
await stopStacks(false);
