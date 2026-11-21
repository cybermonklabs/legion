import type { ReactNode } from "react";

import type { GoingRun } from "../../api/types";
import {
  borderDefault,
  secondaryButtonCompact,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { conversationWorking, setLevel, useGoing, useGoingValue } from "./going-store";

/** The three levels, in the order the control lists them (R10's budgets are the server's). */
const LEVELS: readonly { readonly label: string; readonly level: GoingRun["level"] }[] = [
  { label: "Fast", level: "fast" },
  { label: "Standard", level: "standard" },
  { label: "Deep", level: "deep" },
];

/** How much work the next run may do: Fast, Standard or Deep, under the box. */
export function LevelControl(): ReactNode {
  const chosen = useGoingValue((of) => of.level);
  return (
    <fieldset
      className={`flex shrink-0 items-center gap-3 px-3 py-1.5 text-xs ${textSecondaryOnSurface}`}
      data-testid="going-level"
    >
      <legend className="sr-only">How much work a run may do</legend>
      {LEVELS.map(({ label, level }) => (
        <label className="flex cursor-pointer items-center gap-1" key={level}>
          <input
            checked={chosen === level}
            className="size-3.5 cursor-pointer"
            name="going-level"
            onChange={() => setLevel(level)}
            type="radio"
            value={level}
          />
          {label}
        </label>
      ))}
    </fieldset>
  );
}

/**
 * The header of the conversation, in the bar and in the panel: Stop while its run works, History,
 * a new conversation, the button that moves it (Keep at the side over the page, Over the page at
 * the side) and Close. Each press is the surface's own.
 */
export function ConversationHeader({
  history,
  move,
  onClose,
  onHistory,
  onMove,
  onNew,
  onStop,
}: {
  readonly history: boolean;
  readonly move: "Keep at the side" | "Over the page";
  readonly onClose: () => void;
  readonly onHistory: () => void;
  readonly onMove: () => void;
  readonly onNew: () => void;
  readonly onStop: () => void;
}): ReactNode {
  const going = useGoing();
  const working = conversationWorking(going);
  return (
    <div
      className={`flex shrink-0 flex-wrap items-center gap-2 border-b px-3 py-1.5 ${borderDefault}`}
      data-testid="going-header"
    >
      <span className={`mr-auto text-xs ${textMutedOnSurface}`}>
        {working ? "A run works" : going.conversation === undefined ? "" : "Conversation"}
      </span>
      {working ? (
        <button
          className={secondaryButtonCompact}
          data-testid="going-stop"
          onClick={onStop}
          type="button"
        >
          Stop
        </button>
      ) : null}
      <button
        aria-pressed={history}
        className={secondaryButtonCompact}
        data-testid="going-history-button"
        onClick={onHistory}
        type="button"
      >
        History
      </button>
      {going.conversation === undefined ? null : (
        <button
          className={secondaryButtonCompact}
          data-testid="going-new"
          onClick={onNew}
          type="button"
        >
          New
        </button>
      )}
      <button
        className={secondaryButtonCompact}
        data-testid="going-move"
        onClick={onMove}
        type="button"
      >
        {move}
      </button>
      <button
        className={secondaryButtonCompact}
        data-testid="going-close"
        onClick={onClose}
        type="button"
      >
        Close
      </button>
    </div>
  );
}
