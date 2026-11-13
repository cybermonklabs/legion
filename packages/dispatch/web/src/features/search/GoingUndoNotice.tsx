import type { ReactNode } from "react";

import {
  borderDefault,
  card,
  dangerText,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { dismissLastUndo, undoneText, useLastUndo } from "./going-turns";
import { UndoSaid } from "./OperationRows";

/**
 * What the undo key did (`$mod+z`, bound in `app.tsx`), in the shell's status corner: the key
 * works from any page, the bar's own lines among them or not, so the shell says which turn it
 * undid, one line per write - each written back, reversed, closed or marked undone, or refused
 * for a field another writer changed, naming what changed - until the person dismisses it.
 */
export function GoingUndoNotice(): ReactNode {
  const undo = useLastUndo();
  if (undo === undefined) return null;
  return (
    <section
      aria-label="Command bar undo"
      className={`w-full rounded-lg border p-3 text-sm shadow-lg ${card} ${borderDefault}`}
      data-state={undo.state}
      data-testid="going-undo-notice"
    >
      <p className={`font-medium ${textPrimaryOnSurface}`}>
        {undo.words === "" ? "Your last command-bar turn" : `Your command-bar turn “${undo.words}”`}
      </p>
      {undo.lines.length === 0 ? null : (
        <ul className="mt-1 space-y-1">
          {undo.lines.map((line) => (
            <li
              className={`text-xs ${line.outcome.ok ? textMutedOnSurface : dangerText}`}
              data-item={line.item}
              data-testid="going-undo-notice-line"
              data-undo={line.outcome.ok ? "ok" : "refused"}
              key={line.write_id}
            >
              {line.item === "" ? line.action : line.item}
              {line.field === "" ? "" : ` · ${line.field}`}: {undoneText(line)}
            </li>
          ))}
        </ul>
      )}
      <div className="mt-2 flex items-center justify-between gap-2">
        <UndoSaid undo={undo} />
        {undo.state === "undoing" ? null : (
          <button className={secondaryButtonCompact} onClick={dismissLastUndo} type="button">
            Dismiss
          </button>
        )}
      </div>
    </section>
  );
}
