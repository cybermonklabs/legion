// The DOM this file's imports read is registered by `register-dom.ts`, the preload before this
// one: Bun's preload list is the one order that holds, and an import written first inside a file
// is not (`register-dom.ts` and package.json's `test:web` script say why).
import { afterAll, afterEach, expect, jest } from "bun:test";
import { timeLeft } from "@legion/test-fixtures/wait";
import { configure, getConfig } from "@testing-library/react";
// happy-dom has no IndexedDB; the store of a document's unsent edits (`pending-edits.ts`) runs on
// this in-memory one, which keeps IndexedDB's transaction ordering.
import "fake-indexeddb/auto";
import { notifyManager } from "@tanstack/react-query";
import serverBlockSchema from "../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import { resetReachability } from "../api/reachability";
import type { BlockSchema } from "../api/types";
import { blockSchemaCache } from "../features/doc/schema";
import { resetAnswerDrafts } from "../features/inbox/answer-drafts";
import { resetOutstandingAnswers } from "../features/inbox/outstanding-answers";
import { resetReplyDrafts } from "../features/inbox/reply-drafts";
import { resetPinState } from "../features/issue/pin-write";
import { resetWriteStorage } from "../features/writes/write-storage";
import { type Writes, watchWriteStores } from "../features/writes/write-store";
import { reportableJestDomMatchers } from "./jest-dom-matchers";
import { forgetTestAnswers, requestsThatReachedNetwork } from "./network";
import { resetRouteAnswers } from "./send-answers";

// The DOM assertions (`toBeInTheDocument`, `toHaveAttribute`, `toHaveFocus`, …): one matcher that
// says what it asks for and prints the element it read when it fails, in place of a hand-written
// read of an attribute compared with `toBe`. `jest-dom.d.ts` declares them for `bun:test`, and
// `jest-dom-matchers.ts` gives them the matcher context they build that message with, which
// `expect.extend` does not hand them in a form they can read.
expect.extend(reportableJestDomMatchers());

// Testing Library reads a global `jest` to tell whether fake timers are installed:
// `jestFakeTimersAreEnabled` (@testing-library/dom@10.4.1, dist/@testing-library/dom.esm.js:171)
// begins `if (typeof jest !== 'undefined' && jest !== null)`. Bun's `jest` is a module binding
// from `bun:test` and no global, so without this line every `waitFor` and `findBy*` under
// `jest.useFakeTimers()` takes the real-clock path and polls timers the test is holding still:
// a wait whose node is already there passes by its first synchronous check, and a wait that
// needs the clock to move can never end. That is what dispatch://INFRA-983 measured on a 2-core
// pod, where four of its files fail 16 tests without this line; on the pod this line was written
// in, nothing discriminates it, so `testing-library-fake-timers.test.tsx` holds it in place by
// asserting the global rather than by timing a wait, and says so.
(globalThis as unknown as { jest: typeof jest }).jest = jest;

// Two budgets, because a 2-core pod with its cores busy is slower than either default and a suite
// that waits on the machine reports as a defect of the change under test. One of the two is a
// flag of `package.json`'s test scripts rather than a line of this file, and both are explained
// here, since this file is what every `web/src` test loads.
//
// The per-test deadline is `--timeout 60000`, carried by both command lines that run tests: the
// `test:web` script for `web/src`, and the `bun test --parallel e2e/__tests__` run beside it.
// Bun's own default is 5000 ms, which the lane test of `e2e/__tests__/harness-lanes.test.ts`
// ("the Playwright config refuses a --workers that is not the lane count") fails on a 2-core
// pod, where it reads 4.45 s idle and 6.55 s loaded; the suite's slowest test,
// `row-configurations.test.ts`'s "no row runs twice in one configuration", reads 13.83 s loaded.
// The number is twice the slowest loaded test, and it is sized on the slower of the two pod
// shapes this suite runs in rather than on the one that measured 13.83 s: at code that does not
// differ, dispatch://INFRA-983's pod reads 335 s for the whole suite where this one reads 156 s,
// a factor of 2.15, which puts that same test at about 29.7 s there - inside a 30 s deadline by
// 0.3 s, which is the failure this issue exists to remove rather than a margin. Twice 29.7 s is
// 59.4 s, so 60 s, and it is kept as small as that rule allows, because every hung test costs
// the whole budget on CI as well as in a pod. The flag carries it rather than a
// `jest.setTimeout` here, for two measured reasons: a preload's deadline is not dependable,
// since a 6 s probe in a directory run started by hand ended "this test timed out after 5000ms"
// three times of three with two scratch files beside it and passed with one, and what file of a
// run kept it and what file lost it did not reproduce the same way twice; and the
// `e2e/__tests__` lane loads no preload at all, so there the flag is the only deadline those
// files get. It cuts no deadline a row sets for itself, measured rather than assumed: a row
// carrying `}, 5_000)` slept 1.2 s and passed under `bun test --timeout 200`, so a row's own
// number wins over the flag whichever is larger. Rows that set one today are
// `e2e/__tests__/compare-recordings-refusal.test.ts:28`, `stub-web-server.test.ts:43` and
// `psql-session.test.ts:61,70` at 60 s, equal to the flag, and `MarkdownBody.test.tsx`'s 40 s,
// above the 25 s wait that row must let run out. The list is not a superlative and is not
// checked by anything: a row added tomorrow may set more, as dispatch://INFRA-1571 does with
// a 90 s limit of its own. What lowering the flag would cut is every row that sets none.
//
// The wait budget is set as each wait starts, from the row's own deadline. Every `waitFor` and
// `findBy*` runs inside Testing Library's `asyncWrapper` and reads `asyncUtilTimeout` as the wait
// inside it starts (@testing-library/dom@10.4.1, dist/wait-for.js: `waitForWrapper` and the
// default of `timeout`), so the wrapper below sets it from `timeLeft()`
// (`@legion/test-fixtures/wait`): the time left before the deadline the row-deadline preload
// keeps, which ends a margin before the row's `--timeout`. Every wait of a row, the second
// included, then ends before the row's guard and fails naming what it waited for, rather than as
// a bare test timeout. It chains the wrapper React Testing Library installs, which runs the wait
// outside React's act environment and drains the microtasks the wait left. It replaces one
// number for every wait whatever its row had left: Testing Library's own 1000 ms, which a loaded
// run of this suite passes while a page is still rendering (it failed `settings.test.tsx`'s own
// heading wait and `mermaid-diagrams.test.tsx`'s draw in 13 of 30 loaded runs, each time a
// different row), and then this file's 10 s, which a row's later waits could add up past its
// guard. The row-deadline preload is the last of `test:web`'s list because Bun runs the preloads'
// `afterEach` hooks in that order (measured on Bun 1.4.4): its mark after a row then follows this
// file's `jest.useRealTimers()` below, so a row that left the fake clock held still gets one.
//
// Under `jest.useFakeTimers()` the budget stays 10 s of *fake* time, not of the machine's, and
// does not read `timeLeft()`, which refuses a held clock: Testing Library advances the held clock
// by its polling interval until the wait succeeds or the budget passes, which spends a few
// milliseconds of real time a step, so no load moves it and it ends long before the row's real
// guard. The 10 s replaced Testing Library's 1 s here, and no row of this suite changed verdict
// for it: the files that install a clock and wait - 21 of them when dispatch://INFRA-1405
// measured this on its own tree, about twice that in the merged one - were green before it and
// after it, and a wait that used to succeed cannot start failing because it is given longer.
// What the longer advance can do is let a wait succeed that used to give up - a product timer it
// now reaches - so what the suite gave up with it is the old second as a bound: a held-clock
// `waitFor` does not catch a slowdown that stays under 10 s of fake time, and a failing one
// reports ten times slower than it did.
const FAKE_CLOCK_WAIT_BUDGET_MS = 10_000;
const reactAsyncWrapper = getConfig().asyncWrapper;
configure({
  asyncWrapper: (wait) => {
    configure({
      asyncUtilTimeout: jest.isFakeTimers() ? FAKE_CLOCK_WAIT_BUDGET_MS : timeLeft(),
    });
    return reactAsyncWrapper(wait);
  },
});

// Unit tests have no Dispatch server to answer GET /api/v1/schema/blocks, so the session cache
// starts already holding the checked-in server schema: every MarkdownBody, ask card, and
// document surface renders through the real schema without a network request, and a test that
// wants a different schema replaces it through its own `api.getBlockSchema` spy before the
// first render in its file.
void blockSchemaCache.load(async () => serverBlockSchema as unknown as BlockSchema);

// A route a test answers - `answerRoute` above this seam, `answerRequests` at it - is that test's:
// the next test answers its own.
afterEach(resetRouteAnswers);
afterEach(forgetTestAnswers);

// A request answered by nothing fails its test (`network.ts`), and one a page swallowed in a
// `catch`, or a write loop made after the file's last test - a retry no test is left to fail -
// fails the file.
const reportReachedNetwork = () => {
  const reached = requestsThatReachedNetwork();
  if (reached.length === 0) return;
  throw new Error(`requests reached the network, answered by nothing: ${reached.join(", ")}`);
};
afterEach(reportReachedNetwork);
afterAll(reportReachedNetwork);

class WebSocketStub {
  binaryType = "arraybuffer";
  identifier = 0;
  readyState = 0;

  addEventListener(..._args: unknown[]): void {}

  close(): void {
    this.readyState = 3;
  }

  removeEventListener(..._args: unknown[]): void {}

  send(..._args: unknown[]): void {}
}

globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;

// A failing assertion prints the values it compared, and happy-dom's DOM nodes print as their
// whole object graph - megabytes for one element, which buries or replaces the failure. Nodes
// print as their own markup instead. Bun asks a value for this only when a single-value matcher
// reports (`toBe`, `toBeNull`, `toBeUndefined`, `toContain`); the diff matchers
// (`toEqual`, `toStrictEqual`, `toMatchObject`, `toHaveBeenCalledWith`) and snapshots walk the
// object themselves and ignore it, so compare a node with `toBe` or map it to a string first.
Object.defineProperty(Node.prototype, Bun.inspect.custom, {
  configurable: true,
  value(this: Node): string {
    // A document has no markup and an empty `textContent`, so the node name is what is left to
    // print for one.
    return this instanceof Element
      ? this.outerHTML
      : `${this.nodeName} ${JSON.stringify(this.textContent)}`;
  },
  writable: true,
});

// TanStack batches observer notifications through a scheduler that happy-dom does not
// drive promptly; flushing synchronously keeps query updates inside React's act() scope
// so assertions after setQueryData do not wait on a stalled timer.
notifyManager.setScheduler((callback) => callback());

// Ask-reply and ask-answer drafts live in module-level stores that outlive every render, so a
// draft one test leaves behind would pre-fill the next test's composer or answer box for the
// same ask id. An answer still out when a test ends is held in one of its own
// (`outstanding-answers.ts`), which would keep the next test's reading column on that ask id.
afterEach(resetReplyDrafts);
afterEach(resetAnswerDrafts);
afterEach(resetOutstandingAnswers);

// A write a test leaves out keeps writing what it keeps, and the next test's signed-in app would
// take it up again (`Writes.resume`): each test's are let go, their trying stopped first, since a
// store outlives its test and would go on retrying into the tests and files after it
// (`watchWriteStores`). Without this, one process running every file leaves writes retrying into
// later files - sends to an agent session (`POST /api/v1/agents/<session>/messages`) and asks'
// answers (`POST /api/v1/asks/<id>/answer`), in numbers that vary from run to run - and each is
// a request no handler answers (`network.ts`), which fails whichever test is running then. So is
// the issue state the last pin answered let go, which the next pin would otherwise write over.
const madeStores = new Set<Writes>();
watchWriteStores((store) => madeStores.add(store));
afterEach(() => {
  for (const store of madeStores) store.clear();
  madeStores.clear();
});
afterEach(resetWriteStorage);
afterEach(resetPinState);
// Whether Dispatch could be reached is the page's to learn afresh: a request one test failed would
// otherwise make the next test's first answer start every waiting write's backoff over.
afterEach(resetReachability);

// A fake clock a test installed is put back here as well as in that test's own `finally` or its
// file's `afterEach`, because Bun keeps it across files: a test that installs it and then fails
// before its `try` - an awaited helper of its own, say - would otherwise leave every later test
// in the process, and every later file, with timers that never fire, so one regression reads as
// timeouts everywhere and the real failure is buried. This runs after the hooks above, and
// `useRealTimers` on a test that installed nothing does nothing.
afterEach(() => {
  jest.useRealTimers();
});
