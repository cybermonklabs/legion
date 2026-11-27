import type { ReactNode } from "react";

import { kbdHint, railHoverBg, railHoverText, railSecondaryText } from "../../theme/classes";
import { IS_MAC, keyTokens } from "./key-text";
import type { KeymapActionId } from "./keymap-catalogue";
import { useActionKeys } from "./keymap-keys";
import { keyshortcutsOf } from "./ShortcutHelp";

/** One key as the rail writes it: each part in the glyphs `?` and Settings draw (`keyTokens`),
 *  and the rail's own joiner. On a Mac a press's parts run together, as the Mac's menus write a
 *  key, `⌘K` for `$mod+k` and `⇧N` for `Shift+N`; elsewhere they are spaced, `Ctrl K`, `Ctrl ↓`
 *  for `$mod+ArrowDown`. A one-letter key after a modifier is upper case, a key with no modifier
 *  is drawn as it is typed (`c`), and each press of a chord in turn (`g i`). `isMac`, the
 *  browser's own unless a test names one, picks the glyphs and the joiner. */
export function railText(key: string, isMac: boolean = IS_MAC): string {
  return keyTokens(key, isMac)
    .map(({ parts }) => {
      const last = parts[parts.length - 1] ?? "";
      const modifiers = parts.slice(0, -1);
      if (modifiers.length === 0) return last;
      return [...modifiers, last.length === 1 ? last.toUpperCase() : last].join(isMac ? "" : " ");
    })
    .join(" ");
}

/** A rail control that stands for a keyboard action: its label, then the keys that also run it,
 *  which are the person's own (`useActionKeys`), so a person who has not learned the key can
 *  still reach what it opens. `className` carries the control's place in the rail (its top
 *  margin), which differs per call site. */
export function RailButton({
  action,
  className,
  label,
  onOpen,
}: {
  action: KeymapActionId;
  className: string;
  label: string;
  onOpen: () => void;
}): ReactNode {
  const keys = useActionKeys(action);
  return (
    <button
      aria-keyshortcuts={keyshortcutsOf(keys)}
      className={`${className} flex min-h-11 w-full items-center justify-between rounded-lg px-3 text-left text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
      onClick={onOpen}
      type="button"
    >
      <span>{label}</span>
      {keys.length === 0 ? null : (
        <span className="flex items-center gap-1">
          {keys.map((key) => (
            <kbd className={`rounded px-1.5 py-0.5 text-xs font-medium ${kbdHint}`} key={key}>
              {railText(key)}
            </kbd>
          ))}
        </span>
      )}
    </button>
  );
}
