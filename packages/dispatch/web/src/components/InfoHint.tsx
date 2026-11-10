import { type ReactNode, useId, useRef, useState } from "react";
import {
  borderDefault,
  card,
  focusRingColour,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";

/**
 * The one way a control explains itself: a small (i) beside it, whose text opens on hover, on
 * focus or on a tap and closes on Escape or a press outside. A label under a control says what
 * it is; what it means goes here, so a page reads as labels rather than paragraphs.
 *
 * It is built on `AnchoredPanel`, so it is placed, dismissed and returns focus exactly as every
 * other popover in Dispatch does.
 */
export function InfoHint({
  children,
  label,
}: {
  children: ReactNode;
  /** The button's accessible name: what the text is about ("About work time"). */
  label: string;
}): ReactNode {
  const anchorRef = useRef<HTMLButtonElement>(null);
  /** A hover or a focus shows the text while it lasts; a press keeps it until the next press,
   *  Escape or an outside press, which is what a tap needs. */
  const [hovered, setHovered] = useState(false);
  const [held, setHeld] = useState(false);
  const open = hovered || held;
  const textId = useId();
  const close = () => {
    setHovered(false);
    setHeld(false);
  };
  return (
    <>
      {/* Below 1280px every button is 44px by 44px (styles.css), which is the hit area this
          wants and far more circle than it wants to draw: the button itself draws nothing, and
          the 16px circle inside it is the (i) the reader sees, at every width. */}
      <button
        aria-describedby={open ? textId : undefined}
        aria-expanded={open}
        aria-label={label}
        className="group inline-flex size-5 shrink-0 items-center justify-center outline-none"
        onBlur={() => setHovered(false)}
        onClick={(event) => {
          // A header's own control: the press is the hint's, never the sort's.
          event.stopPropagation();
          setHeld((shown) => !shown);
        }}
        onFocus={() => setHovered(true)}
        onMouseEnter={() => setHovered(true)}
        onMouseLeave={() => setHovered(false)}
        ref={anchorRef}
        type="button"
      >
        <span
          className={`inline-flex size-4 items-center justify-center rounded-full border text-[0.625rem] leading-none font-semibold group-focus-visible:ring-2 ${borderDefault} ${textMutedOnCanvas} ${focusRingColour}`}
        >
          i
        </span>
      </button>
      {open ? (
        <AnchoredPanel
          anchorRef={anchorRef}
          className={`max-w-64 rounded-lg border p-2 text-xs ${card} ${textSecondaryOnCanvas}`}
          onDismiss={close}
        >
          <span id={textId}>{children}</span>
        </AnchoredPanel>
      ) : null}
    </>
  );
}
