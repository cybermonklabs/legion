import { type ReactNode, useRef } from "react";

import { useFootInset } from "../../hooks/useFootInset";
import { card, textMutedOnSurface } from "../../theme/classes";
import { keyText } from "./key-text";
import { usePendingChord } from "./keymap";
import { kbdClass } from "./ShortcutHelp";

/** Shows a half-typed chord (`g …`) until it completes or times out, and under it each key that
 *  can follow with what it does (`i` Go to Inbox), as Emacs's which-key does: a next key as `?`
 *  draws it (`keyText`), `⇧+I` on a Mac and `Shift+I` elsewhere for a person's `g Shift+I`.
 *  It is a float of the foot's top layer (`hooks/useFootInset.ts` lists the foot's layers and the
 *  elements in each): it stands 1rem above the status layer's property, which the hook chooses so
 *  the indicator lies over no foot element and stays on the screen, and never lower than its
 *  floor (`--foot-floor`, which its classes set per breakpoint). */
export function ChordIndicator(): ReactNode {
  const pending = usePendingChord();
  const indicatorRef = useRef<HTMLOutputElement>(null);
  useFootInset(indicatorRef, pending.keys !== "", "top");
  if (pending.keys === "") {
    return null;
  }
  return (
    <output
      className={`fixed right-4 bottom-[max(var(--foot-floor),calc(var(--foot-status-inset,0px)_+_1rem))] z-40 flex max-w-xs flex-col gap-1.5 rounded-xl border px-3 py-1.5 text-sm shadow-lg [--foot-floor:8rem] xl:[--foot-floor:4rem] ${card} ${textMutedOnSurface}`}
      data-testid="chord-indicator"
      ref={indicatorRef}
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
              <kbd className={kbdClass}>{keyText(key)}</kbd>
              <span>{label}</span>
            </li>
          ))}
        </ul>
      )}
    </output>
  );
}
