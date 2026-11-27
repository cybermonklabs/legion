import { badgeLow, hoverToDangerText, secondaryButtonText } from "../../theme/classes";

/** The pill a session is listed in (subscribed agents, an ask's followers). */
export const sessionChipClassName = `flex items-center gap-2 rounded-full px-3 py-1 text-sm ${badgeLow.bg} ${badgeLow.text}`;

/** The chip's remove control, red on hover, and dimmed while it cannot be pressed: `disabled`, or
 *  held with `aria-disabled` (the `refused:` variant, `styles.css`). */
export const sessionRemoveButtonClassName = `font-medium ${secondaryButtonText} ${hoverToDangerText} refused:cursor-not-allowed refused:opacity-50`;
