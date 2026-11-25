/**
 * The two notes of dispatch://INFRA-4130 a worker's host adds where its agent reads them at the
 * next step, and which both harnesses write: Oh My Pi's at a tool result
 * (`packages/pi-legion/extensions/legion.ts`), Claude Code's at a `PostToolUse` hook
 * (`packages/claude-envoy/src/legion-phase-hook.ts`). They live apart from the take
 * (`./task-take`), which only Oh My Pi's claim runs, and import nothing, so a host that only
 * writes them builds none of it.
 */

/** R1's note: the claim's own task `number` waits for the end of the turn going now. */
export function taskWaitingNote(number: number): string {
  return (
    `Legion: task ${number} of this claim waits for this turn to end. It starts as your next ` +
    "turn when this turn ends, unless the daemon holds or drops it then. Finish what is in hand, " +
    "stop at a safe point, and end this turn. This note carries no task: do not start its work " +
    "in this turn."
  );
}

/** R2's note: the `role`'s task `number` on this issue waits for the end of this claim's turn,
 * which holds it because the roles of an issue share one working copy. */
export function heldTakeNote(role: string, number: number): string {
  return (
    `Legion: the ${role}'s task ${number} on this issue waits for this turn to end, because the ` +
    "roles of an issue share one working copy. It starts when this turn ends, unless the daemon " +
    "holds it then. Finish what is in hand, stop at a safe point, and end this turn. This note " +
    "carries no task."
  );
}

/** What a host adds at one step: R1's note of the claim's own task `waiting` first, where one is
 * given, then R2's note of each take in `held`, joined by a blank line; undefined where there is
 * none. Which number and which takes are due at that step is each host's own rule. */
export function taskNotes(
  waiting: number | undefined,
  held: readonly { role: string; task: number }[]
): string | undefined {
  const notes = waiting === undefined ? [] : [taskWaitingNote(waiting)];
  for (const { role, task } of held) notes.push(heldTakeNote(role, task));
  return notes.length === 0 ? undefined : notes.join("\n\n");
}
