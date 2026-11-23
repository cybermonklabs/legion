/**
 * The bound of a test row or hook whose verdict is what it observed, not when it observed it: a row
 * that spawns a child or waits on I/O, and a hook that removes a scratch tree. A hang guard, not a
 * verdict: the row ends when its child exits or its I/O answers, and how long that takes is the
 * machine's, which a loaded box slows past Bun's 5 s default (Bun times out a synchronous hook too,
 * once it returns). The guard is far above any run seen and under a CI job's limit, so a row or
 * hook that never ends still fails, named by Bun ("this test timed out after 120000ms").
 *
 * A test file whose rows state no bound of their own takes it for all of them with
 * `setDefaultTimeout(HANG_GUARD_MS)`, which holds for that file alone; a hook passes it as its
 * second argument.
 */
export const HANG_GUARD_MS = 120_000;
