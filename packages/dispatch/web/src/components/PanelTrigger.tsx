import { type ReactNode, useRef } from "react";

import { borderDefault, card, surfaceMutedBg, textSecondaryOnCanvas } from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";
import { CONTROL, type ControlName, type ControlSize, controlPadding } from "./control-names";
import { useFocusPlace } from "./focus-place";
import { heldPress } from "./held-press";

export interface PanelTriggerProps {
  readonly align?: "left" | "right";
  /** Holds the trigger while a write it leads to is out - an epic's actions while a table action
   *  is out (`heldPress`): it keeps the focus and its Tab stop, says it cannot be pressed
   *  (`aria-disabled`), and a press on it opens nothing. A press still closes a panel it has
   *  open, so a hold that comes while the panel is open never leaves the reader unable to shut
   *  it. */
  readonly ariaDisabled?: boolean;
  /** The panel's content, handed the one way to close it: a choice taken closes the panel and
   *  puts the reader back on the trigger they came from. */
  readonly children: (close: () => void) => ReactNode;
  /** Which control of the vocabulary this trigger is, written into its `data-control`: a display
   *  menu, a columns menu and a column filter are each a button that opens a panel, and each is
   *  found by its own name while there is still one definition of the button and its panel. */
  readonly control?: ControlName;
  /** Said after the label as `Columns · 2`, the way `MultiSelect`'s trigger counts a selection. */
  readonly count?: number;
  /** Whether opening moves the focus into the panel, as every press that opens one does. A page
   *  that opens its panel by itself, from a choice it stored, passes false for that open, so the
   *  focus starts where the page starts (the Board). Read as the panel opens. */
  readonly focusOnOpen?: boolean;
  readonly label: ReactNode;
  readonly onOpenChange: (open: boolean) => void;
  /** The host owns it, so a page keeps one panel open at a time rather than a row of them. */
  readonly open: boolean;
  readonly size?: ControlSize;
  /** A panel of controls - a page's filter panel: its multi-selects, flags and boxes - rather
   *  than a list of rows. The controls stand one under another with a gap between, each at its
   *  own width, so a chip or a flag is a chip and not a bar the panel's width; a control meant
   *  to fill the panel, a search box, says so itself (`w-full`). */
  readonly stacked?: boolean;
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
 * opens, and whatever closes it — a choice inside, Escape, Tab or Shift+Tab past its last or
 * first control, a press outside — leaves the reader on the trigger rather than on the document,
 * so Tab never carries the reader out of a panel to the page behind it.
 *
 * A press that takes away the control holding the focus while the panel stays open - the search
 * bar's Project menu, whose trigger goes once a project is picked, or its Author box once an
 * author is taken - hands the focus to the control now at that control's place in the panel,
 * else to the one before it, else to the panel itself, from which Escape still closes it
 * (`useFocusPlace`). A control of a panel opened inside this one counts at the place of the
 * trigger that opened it.
 */
export function PanelTrigger({
  align = "left",
  ariaDisabled = false,
  children,
  control = CONTROL.panelTrigger,
  count,
  focusOnOpen,
  label,
  onOpenChange,
  open,
  size = "comfortable",
  stacked = false,
  title,
  triggerClassName,
}: PanelTriggerProps): ReactNode {
  const trigger = useRef<HTMLButtonElement>(null);
  // The panel's content, without a box of its own (`contents`), so the panel lays it out as it
  // always has: the container a focused control in the panel is counted in.
  const content = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    () => content.current,
    (container) => {
      const panel = container.parentElement;
      return panel instanceof HTMLElement ? panel : null;
    }
  );
  const close = () => {
    onOpenChange(false);
    trigger.current?.focus();
  };
  // Held, the trigger refuses only an open: a press on it while its panel is open closes it.
  const press = heldPress(ariaDisabled, () => onOpenChange(true));
  const ownTrigger = `rounded-lg border text-sm font-semibold whitespace-nowrap refused:opacity-40 ${controlPadding[size]} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`;
  return (
    <>
      <button
        aria-disabled={press["aria-disabled"]}
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={title}
        className={triggerClassName ?? ownTrigger}
        data-control={control}
        onClick={open ? () => onOpenChange(false) : press.onClick}
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
          className={`flex max-h-[min(20rem,calc(100dvh-16px))] w-64 max-w-[calc(100vw-16px)] flex-col overflow-y-auto rounded-lg border text-sm font-normal shadow-lg ${stacked ? "items-start gap-2 p-2" : "p-1"} ${card}`}
          focusOnOpen={focusOnOpen}
          label={title ?? (typeof label === "string" ? label : undefined)}
          onDismiss={() => onOpenChange(false)}
          role="dialog"
        >
          {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only notes which control of the panel took the focus, for the hand-off when a commit takes that control away (`useFocusPlace`), so the box needs no role of its own. */}
          <div className="contents" onFocus={noteFocus} ref={content}>
            {children(close)}
          </div>
        </AnchoredPanel>
      ) : null}
    </>
  );
}
