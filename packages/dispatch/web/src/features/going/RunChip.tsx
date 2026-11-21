import type { ReactNode } from "react";

import { borderDefault, card, linkText, textPrimaryOnSurface } from "../../theme/classes";
import { openRun, type RunEntry, shownState, useGoing } from "./going-store";
import { type RunState, runEnded } from "./run-states";

/** What the chip says of the run it names, the one whose plan waits first, then the working
 *  ones, then the newest that ended and was not seen. */
function chipText(state: RunState, working: number): string {
  if (state === "waiting") return "A plan waits for you";
  if (working > 0) return working === 1 ? "1 run working" : `${working} runs working`;
  if (state === "done") return "Done";
  return state === "failed" ? "A run failed" : "A run stopped";
}

/**
 * The chip at the foot of the page (R11) while no conversation is on the screen: it says how the
 * runs started on this page stand once the bar is closed - working, a plan that waits, or done -
 * and Open shows that run's conversation again. A run the person has seen end is no longer named.
 */
export function RunChip(): ReactNode {
  const going = useGoing();
  if (going.place !== "closed") {
    return null;
  }
  // Newest first: the chip names the newest run of each kind.
  const runs = [...going.runs.values()]
    .filter((run) => run.startedHere)
    .reverse()
    .map((run): readonly [RunEntry, RunState] => [run, shownState(run)]);
  const working = runs.filter(([, state]) => state === "working");
  const shown =
    runs.find(([run, state]) => !run.seen && state === "waiting") ??
    working[0] ??
    runs.find(([run, state]) => !run.seen && runEnded(state));
  if (shown === undefined) {
    return null;
  }
  const [run, state] = shown;
  return (
    <p
      className={`flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${card} ${borderDefault} ${textPrimaryOnSurface}`}
      data-state={state}
      data-testid="going-chip"
      role="status"
    >
      {chipText(state, working.length)}
      <button
        className={`underline ${linkText}`}
        data-testid="going-chip-open"
        onClick={() => openRun(run.id)}
        type="button"
      >
        Open
      </button>
    </p>
  );
}
