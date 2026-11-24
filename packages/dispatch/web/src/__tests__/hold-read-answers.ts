/**
 * The late-reads lane: every query's answer reaches its cache a turn of the real event loop after
 * its request has answered.
 *
 * `bun run test:web:late-reads` is `test:web` with this file added last to its `--preload` list.
 * It wraps the `queryFn` of every TanStack query as the query takes its options
 * (`Query.setOptions`), so the wrapped function awaits the request and then one `setImmediate`
 * before it answers. A page draws what needs no read at once, and what a read brings a commit
 * later. A row that awaits the first and then reads the second without waiting for it passes
 * under `test:web` whenever the read's answer lands in the same commit, which is how
 * `settings.test.tsx` passed in a pod and failed on a loaded CI runner: it awaited a heading the
 * settings page draws at once, and then read the sidebar's Settings link, which the sidebar draws
 * only once the inbox, the pinned issues and the projects have answered (`Sidebar.tsx`). Until
 * that row waited for the link, it failed here under every seed.
 *
 * The wait is the real `setImmediate`, which a fake clock a row installs does not hold (Bun
 * 1.4.4): a row on a fake clock gets its answers as it would without this file, one turn later.
 * `setQueryData`, a mutation and a write of the page's store run no `queryFn` and are not held,
 * so a row that seeds its cache or writes is not slowed: what is held is every read a query makes,
 * the sign-in check's among them.
 *
 * Reading a failure. A row that fails here and passes under `test:web` with the same seed reads
 * what a read draws before it waits for it: it waits with a `findBy*` query, or `waitFor`, for
 * the element it reads, as docs/testing.md ("A Dispatch unit test runs in random order") says.
 * Waiting fixes none of three other kinds of failure. Two come from the hold itself. A row
 * that tests when a read answers - the query client's own rows (`query-client.test.ts`), a
 * write's 401 path through the sign-in check (`write-store.test.ts`) - is timing the very answer
 * this file moves. A row that ends while a read it never reads is still out sees that read land
 * between two of its `act` scopes, which React reports and the preload's act check fails
 * (`setup.ts`). The third is a page that never reaches the state a row waits for, which is a bug
 * of the page and not of the row, so no wait can pass it. This lane found one: the Agents
 * header's unread badge still read "New reply 1" ten seconds after its conversation was opened
 * (`AgentsPage.test.tsx`), because a read asked before the mark put back the count the mark had
 * cleared; `storeAgentState` (`features/agents/unread.ts`) now puts the mark's answer over the
 * reads still out.
 */

import { Query } from "@tanstack/react-query";

/** Marks a `queryFn` this file has wrapped, so a query that takes its options again is not
 *  wrapped twice. */
const HELD = Symbol("held by hold-read-answers.ts");

type QueryFunction = ((...args: never[]) => unknown) & { [HELD]?: true };

/** One turn of the real event loop, which a fake clock does not hold. */
function realTurn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

const takeOptions = Query.prototype.setOptions;
Query.prototype.setOptions = function (this: Query, options) {
  takeOptions.call(this, options);
  const asked = this.options.queryFn as QueryFunction | undefined;
  if (typeof asked !== "function" || asked[HELD]) return;
  const held: QueryFunction = async (...args) => {
    const answer = await asked(...args);
    await realTurn();
    return answer;
  };
  held[HELD] = true;
  this.options = { ...this.options, queryFn: held as unknown as typeof this.options.queryFn };
};
