import { type ReactNode, useRef } from "react";

import { borderDefault, card, surfaceMutedBg, textSecondaryOnCanvas } from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";
import { CONTROL, type ControlSize, controlPadding } from "./control-names";

export interface PanelTriggerProps {
  readonly align?: "left" | "right";
  /** The panel's content, handed the one way to close it: a choice taken closes the panel and
   *  puts the reader back on the trigger they came from. */
  readonly children: (close: () => void) => ReactNode;
  /** Said after the label as `Columns · 2`, the way `MultiSelect`'s trigger counts a selection. */
  readonly count?: number;
  readonly label: ReactNode;
  readonly onOpenChange: (open: boolean) => void;
  /** The host owns it, so a page keeps one panel open at a time rather than a row of them. */
  readonly open: boolean;
  readonly panelClassName?: string;
  readonly size?: ControlSize;
  /** The accessible name of both the trigger and the panel: a trigger whose label is a glyph is
   *  nameless without it. */
  readonly title?: string;
  /** For a trigger with a look of its own, such as the table header's funnel. */
  readonly triggerClassName?: string;
}

/**
 * The one button that opens a panel: every menu, column filter and display picker in Dispatch is
 * this component over `AnchoredPanel`, so each is dismissed, placed and named by the same rules
 * and a reader who learns one has learned them all. The panel is a `dialog` the trigger says it
 * opens, and whatever closes it — a choice inside, Escape, a press outside — leaves the reader
 * on the trigger rather than on the document.
 */
export function PanelTrigger({
  align = "left",
  children,
  count,
  label,
  onOpenChange,
  open,
  panelClassName,
  size = "comfortable",
  title,
  triggerClassName,
}: PanelTriggerProps): ReactNode {
  const trigger = useRef<HTMLButtonElement>(null);
  const close = () => {
    onOpenChange(false);
    trigger.current?.focus();
  };
  const ownTrigger = `rounded-lg border text-sm font-semibold whitespace-nowrap ${controlPadding[size]} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`;
  return (
    <>
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={title}
        className={triggerClassName ?? ownTrigger}
        data-control={CONTROL.panelTrigger}
        onClick={() => onOpenChange(!open)}
        ref={trigger}
        title={title}
        type="button"
      >
        {label}
        {count === undefined ? null : ` · ${count}`}
      </button>
      {open ? (
        <AnchoredPanel
          align={align}
          anchorRef={trigger}
          className={
            panelClassName ??
            `z-50 flex max-h-[min(20rem,calc(100dvh-16px))] w-64 max-w-[calc(100vw-16px)] flex-col overflow-y-auto rounded-lg border p-1 text-sm font-normal shadow-lg ${card}`
          }
          label={title ?? (typeof label === "string" ? label : undefined)}
          onDismiss={() => onOpenChange(false)}
          role="dialog"
        >
          {children(close)}
        </AnchoredPanel>
      ) : null}
    </>
  );
}
