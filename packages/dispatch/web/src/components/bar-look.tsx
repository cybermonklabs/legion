import type { ReactNode } from "react";

import {
  bgTransparent,
  focusVisibleRing,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonDisabledText,
  surfaceMutedHoverBg,
  tableFilterActive,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { type ControlSize, controlPadding, toolbarHeight } from "./control-names";

/**
 * How the control bar (`ControlRow`) asks a shared control to draw itself in the bar: with no
 * border and no fill at rest, on a tint while a count it carries is above zero, and with a glyph
 * alone in place of its words where `icon` is given. A control drawn outside any bar is given no
 * look and keeps its own, so `ColumnFilter`'s funnel and a table row's epic actions draw as they
 * always have.
 */
export interface BarLook {
  /** Drawn filled: the page's one main action, the one filled button of the bar (`BarAction`). */
  readonly filled?: boolean;
  /** The glyph drawn alone in place of the control's words, below 640 px of the bar's width; its
   *  words stay its accessible name. A count it carries is drawn as a badge on the glyph. */
  readonly icon?: ReactNode;
  /** The control's trailing corners are square, so the × after it (**Clear filters**) stands
   *  flush beside it, one tinted group of two buttons. */
  readonly joined?: boolean;
}

/** What a button of the bar is painted as: quiet (no border, no fill, at rest), on the tint of a
 *  trigger whose narrowings are on, or filled, the page's one main action. */
export type BarTone = "filled" | "quiet" | "tint";

/** What every button the bar draws shares: one line of words at the bar's type, the ring the
 *  keyboard's focus draws, and the cursor of a press refused (`refused:`, `styles.css`). */
const BUTTON = `inline-flex shrink-0 items-center justify-center gap-1.5 text-sm whitespace-nowrap outline-none focus-visible:ring-2 ${focusVisibleRing} refused:cursor-not-allowed`;

/** Each tone. The quiet one is Primer's ActionBar's and Apple's toolbars' button, with no border
 *  and no fill; the filled one is the page's one main action, the one filled button of the page
 *  (Apple: "only specify one primary action"). */
const TONES: Record<BarTone, string> = {
  filled: `font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`,
  quiet: `font-medium ${bgTransparent} ${textSecondaryOnCanvas} ${surfaceMutedHoverBg} ${secondaryButtonDisabledText}`,
  tint: `font-medium ${tableFilterActive}`,
};

/** A button that shows its glyph alone, at each size: square, at the touch target of
 *  `controlPadding`, which the compact size gives up only from `md`, where a mouse is, and the
 *  toolbar size only from 1280 px up, for the toolbar's one height (`toolbarHeight`). */
const ICON_ONLY: Record<ControlSize, string> = {
  comfortable: "min-h-11 min-w-11 p-2",
  compact: "min-h-11 min-w-11 p-1 md:min-h-8 md:min-w-8",
  toolbar: `min-h-11 min-w-11 p-1 ${toolbarHeight} xl:min-w-8.5`,
};

/** The classes of one button of the bar: its tone, at the page's size, with its words
 *  (`controlPadding`) or with its glyph alone, and its corners. */
export function barButton(
  size: ControlSize,
  tone: BarTone,
  iconOnly = false,
  corners = "rounded-lg"
): string {
  return `${BUTTON} ${corners} ${TONES[tone]} ${iconOnly ? ICON_ONLY[size] : controlPadding[size]}`;
}

/** A count drawn as a badge on a glyph: **Filter**'s, below 640 px of the bar's width. */
const BADGE = `absolute -top-1.5 -right-2 min-w-4 rounded-full px-1 text-center text-[10px] leading-4 font-semibold tabular-nums ${primaryButtonBg}`;

/** `icon` with `count` as a badge on it while the count is above zero. The badge is hidden from a
 *  screen reader, which reads the count in the button's name. */
export function badged(icon: ReactNode, count: number | undefined): ReactNode {
  return (
    <span className="relative inline-flex">
      {icon}
      {count === undefined || count <= 0 ? null : (
        <span aria-hidden="true" className={BADGE}>
          {count}
        </span>
      )}
    </span>
  );
}

function Glyph({ children }: { children: ReactNode }): ReactNode {
  return (
    <svg aria-hidden="true" className="size-4 shrink-0" fill="currentColor" viewBox="0 0 16 16">
      {children}
    </svg>
  );
}

/** A glyph that is a character of text, such as **New message**'s ✎, at the size the bar's
 *  drawn glyphs read at, hidden from a screen reader, which hears the action's name. */
export function textGlyph(char: string): ReactNode {
  return (
    <span aria-hidden="true" className="text-base leading-none">
      {char}
    </span>
  );
}

/** The funnel's outline, which the table header's column filter draws too (`ColumnFilter`). */
export const FUNNEL_PATH = "M1.5 2h13l-5 6.2V13l-3 1.5V8.2z";

/** **Filter**'s glyph: the funnel the table header's column filter draws. */
export const FUNNEL_GLYPH = (
  <Glyph>
    <path d={FUNNEL_PATH} />
  </Glyph>
);

/** The display menu's glyph: three sliders. */
export const SLIDERS_GLYPH = (
  <Glyph>
    <path d="M1.5 3.25h13v1.5h-13zm0 4h13v1.5h-13zm0 4h13v1.5h-13z" />
    <circle cx="5" cy="4" r="2" />
    <circle cx="11" cy="8" r="2" />
    <circle cx="7" cy="12" r="2" />
  </Glyph>
);

/** **More**'s glyph: ⋯. */
export const DOTS_GLYPH = (
  <Glyph>
    <circle cx="3" cy="8" r="1.5" />
    <circle cx="8" cy="8" r="1.5" />
    <circle cx="13" cy="8" r="1.5" />
  </Glyph>
);

/** The hint's glyph: (i). */
export const INFO_GLYPH = (
  <Glyph>
    <path
      d="M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1zm0 1.5a5.5 5.5 0 1 1 0 11 5.5 5.5 0 0 1 0-11z"
      fillRule="evenodd"
    />
    <rect height="4.5" rx="0.75" width="1.5" x="7.25" y="7" />
    <circle cx="8" cy="5" r="0.9" />
  </Glyph>
);
