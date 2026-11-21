import type { ReactNode } from "react";

import { kbdHint, railHoverBg, railHoverText, railSecondaryText } from "../../theme/classes";
import { IS_MAC } from "./keymap";
import type { KeymapActionId } from "./keymap-catalogue";
import { useActionKeys } from "./keymap-keys";
import { keyshortcutsOf, keyTokens } from "./ShortcutHelp";

/** The modifiers a Mac's keycaps print as symbols, which `keyTokens` writes as words: on a Mac
 *  the rail runs a press's parts together, as the Mac's menus write a key (`⇧N`, `⌘K`). */
const MAC_MODIFIER_SYMBOLS: Record<string, string> = {
  Control: "⌃",
  Ctrl: "⌃",
  Meta: "⌘",
  Shift: "⇧",
};

/** One key as the rail writes it, in the glyphs `?` and Settings draw (`keyTokens`): `⌘K` on a
 *  Mac and `Ctrl K` elsewhere for `$mod+k`, `Ctrl ↓` for `$mod+ArrowDown`, a key with no modifier
 *  as it is typed (`c`), and each press of a chord in turn (`g i`). */
function railText(key: string): string {
  return keyTokens(key)
    .map(({ parts }) => {
      const last = parts[parts.length - 1] ?? "";
      const modifiers = parts.slice(0, -1);
      if (modifiers.length === 0) return last;
      const shown = IS_MAC
        ? modifiers.map((part) => MAC_MODIFIER_SYMBOLS[part] ?? part)
        : modifiers;
      return [...shown, last.length === 1 ? last.toUpperCase() : last].join(IS_MAC ? "" : " ");
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
