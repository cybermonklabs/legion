/**
 * Preloaded into each `reserve-ask.ts` child that `s10-decision.ts` spawns (`spawnDecision`), and
 * never imported by the parent, so a child's exit is a verdict only where it can be one:
 * - an error the child does not catch exits `UNCAUGHT_EXIT` rather than with Bun's own 1, which is
 *   also the exit `reserve-ask.ts` gives a held ask, so an uncaught error is counted as an error
 *   the reserve decided nothing on and is never read as held (`reserve-ask.ts` exits 2 on the
 *   errors it catches itself, a `CannotRunError`);
 * - a child whose `claim.ts` records that the reserve ended exits `ENDED_EXIT` before it decides:
 *   `reserveRuleOf` would keep every ask, which the decision at 651db8d5 never does under a
 *   reserve that holds. The parent checks the same value before the run; this check reads the
 *   `claim.ts` each child itself loads, whenever it is spawned.
 *
 * The handlers are registered before `claim.ts` is loaded, which a static import would load first:
 * so a `claim.ts` that throws as it loads exits `UNCAUGHT_EXIT` too, never Bun's 1.
 *
 * It changes nothing else the child does. Its bytes are pinned with the decision's closure
 * (`RESERVE_ASK_CLOSURE`), since a preload could change every verdict.
 */
const UNCAUGHT_EXIT = 70;
const ENDED_EXIT = 71;

process.on("uncaughtException", () => process.exit(UNCAUGHT_EXIT));
process.on("unhandledRejection", () => process.exit(UNCAUGHT_EXIT));
// A dynamic import, so `claim.ts` runs after the handlers above: a static one would run it first.
let ended: string | null;
try {
  ended = (await import("./claim")).RESERVE_ENDED;
} catch {
  process.exit(UNCAUGHT_EXIT);
}
if (ended !== null) process.exit(ENDED_EXIT);

// It exports nothing: this makes the file a module, so it may await at its top level.
export {};
