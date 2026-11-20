/**
 * Preloaded into each `reserve-ask.ts` child that `s10-inputs.ts` spawns (`spawnDecision`), and
 * never imported: an error the child does not catch exits with this code rather than with Bun's
 * own 1, which is also the exit `reserve-ask.ts` gives a held ask, so an uncaught error is counted
 * as an error the reserve decided nothing on and is never read as held. `reserve-ask.ts` exits 2
 * on the errors it catches itself (a `CannotRunError`). The handlers change nothing else the child
 * does, and a test spawns a stub through the same preload.
 */
const UNCAUGHT_EXIT = 70;

process.on("uncaughtException", () => process.exit(UNCAUGHT_EXIT));
process.on("unhandledRejection", () => process.exit(UNCAUGHT_EXIT));

export {};
