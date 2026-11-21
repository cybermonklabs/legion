import type { ReactNode } from "react";

import { card, textMutedOnSurface } from "../../theme/classes";
import { usePendingChord } from "./keymap";
import { kbdClass } from "./ShortcutHelp";

/** Shows a half-typed chord (`g …`) until it completes or times out, and under it each key that
 *  can follow with what it does (`i` Go to Inbox), as Emacs's which-key does. */
export function ChordIndicator(): ReactNode {
  const pending = usePendingChord();
  if (pending.keys === "") {
    return null;
  }
  return (
    <output
      className={`fixed right-4 bottom-32 z-40 flex max-w-xs flex-col gap-1.5 rounded-xl border px-3 py-1.5 text-sm shadow-lg xl:bottom-16 ${card} ${textMutedOnSurface}`}
      data-testid="chord-indicator"
    >
      <span className="flex items-center gap-1.5">
        <kbd className={kbdClass}>{pending.keys}</kbd>
        <span aria-hidden="true">…</span>
        <span className="sr-only">then…</span>
      </span>
      {pending.next.length === 0 ? null : (
        <ul className="flex flex-col gap-1 pb-0.5">
          {pending.next.map(({ key, label }) => (
            <li className="flex items-center gap-2" key={key}>
              <kbd className={kbdClass}>{key}</kbd>
              <span>{label}</span>
            </li>
          ))}
        </ul>
      )}
    </output>
  );
}
