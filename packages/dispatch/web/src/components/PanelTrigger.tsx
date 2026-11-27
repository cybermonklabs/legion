import { type ReactNode, useRef } from "react";

import { borderDefault, card, surfaceMutedBg, textSecondaryOnCanvas } from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";
import { type BarLook, type BarTone, badged, barButton } from "./bar-look";
import { CONTROL, type ControlName, type ControlSize, controlPadding } from "./control-names";
import { ARM_ON_PRESS, useFocusPlace } from "./focus-place";
import { heldPress } from "./held-press";

/** The frame of a panel a trigger opens: a card over the page, at most 20rem tall and 16rem wide
 *  and never past the window's edges, that scrolls what it cannot hold. The row's `+N more` panel
 *  is drawn in it too (`ControlRow`). */
export const PANEL_FRAME =
  "flex max-h-[min(20rem,calc(100dvh-16px))] w-64 max-w-[calc(100vw-16px)] flex-col overflow-y-auto rounded-lg border shadow-lg";

/**
 * How a trigger is drawn where it does not keep its own look, a bordered button on the muted
 * surface that says its count after its label, at zero as well:
 *
 * - `bar`: the control bar's look (`BarLook`), where the bar draws the trigger: no border and no
 *   fill at rest, on a tint with its count while the count is above zero, and its glyph alone
 *   with the count as a badge where `look.icon` is given, its label and count then its name.
 * - `class`: a look of the host's own, such as the table header's funnel or an item of **More**
 *   (`MoreMenu`), whose count is said as the trigger's own look says it.
 */
export type PanelTriggerAppearance =
  | { readonly kind: "bar"; readonly look: BarLook }
  | { readonly kind: "class"; readonly className: string };

export interface PanelTriggerProps {
  readonly align?: "left" | "right";
  /** How the trigger is drawn (`PanelTriggerAppearance`): its own look where none is given, as a
   *  trigger outside any bar is drawn. */
  readonly appearance?: PanelTriggerAppearance;
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
  /** Said after the label as `Columns · 2`, the way `MultiSelect`'s trigger counts a selection,
   *  at zero as well. In the control bar's look (`appearance`'s `bar`) it is said only while
   *  above zero, on a tint. */
  readonly count?: number;
  /** The id of the line on screen that says why the trigger is held (`ariaDisabled`), named as
   *  its description: a refused action of the bar that opens a panel (`PanelAction`). */
  readonly describedBy?: string;
  /** Whether opening moves the focus into the panel, as every press that opens one does. A page
   *  that opens its panel by itself, from a choice it stored, passes false for that open, so the
   *  focus starts where the page starts (the Board). Read as the panel opens. */
  readonly focusOnOpen?: boolean;
  readonly label: ReactNode;
  readonly onOpenChange: (open: boolean) => void;
  /** The host owns it, so a page keeps one panel open at a time rather than a row of them. */
  readonly open: boolean;
  /** `menuitem` for a trigger that is an item of a menu: an action of the control bar that opens
   *  a panel of its own (**Save view**) and stands in the bar's **More** menu (`MoreMenu`). The
   *  panel it opens hangs off the item and stands over the menu, which stays open under it. A
   *  plain button where unset. */
  readonly role?: "menuitem";
  readonly size?: ControlSize;
  /** A panel of controls - a page's filter panel: its multi-selects, flags and boxes - rather
   *  than a list of rows. The controls stand one under another with a gap between, each at its
   *  own width, so a chip or a flag is a chip and not a bar the panel's width; a control meant
   *  to fill the panel, a search box, says so itself (`w-full`). */
  readonly stacked?: boolean;
  /** The accessible name of both the trigger and the panel: a trigger whose label is a glyph is
   *  nameless without it. */
  readonly title?: string;
  /** The trigger's tooltip where it is not its name: why a refused action of the bar that opens
   *  a panel is held (`PanelAction`). */
  readonly tooltip?: string;
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
  appearance,
  ariaDisabled = false,
  children,
  control = CONTROL.panelTrigger,
  count,
  describedBy,
  focusOnOpen,
  label,
  onOpenChange,
  open,
  role,
  size = "comfortable",
  stacked = false,
  title,
  tooltip,
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
  // In the bar's look the count is said only while above zero, and a glyph drawn alone leaves
  // the label and the count to the name.
  const look = appearance?.kind === "bar" ? appearance.look : undefined;
  const counted = count !== undefined && count > 0;
  const glyph = look?.icon;
  const name =
    title ??
    (glyph === undefined || typeof label !== "string"
      ? undefined
      : counted
        ? `${label} · ${count}`
        : label);
  let face: ReactNode;
  if (look === undefined) {
    face = (
      <>
        {label}
        {count === undefined ? null : ` · ${count}`}
      </>
    );
  } else if (glyph !== undefined) {
    face = badged(glyph, count);
  } else {
    face = (
      <>
        {label}
        {counted ? ` · ${count}` : null}
      </>
    );
  }
  const tone: BarTone = look?.filled === true ? "filled" : counted ? "tint" : "quiet";
  const className =
    appearance === undefined
      ? ownTrigger
      : appearance.kind === "class"
        ? appearance.className
        : barButton(
            size,
            tone,
            glyph !== undefined,
            appearance.look.joined === true ? "rounded-l-lg" : "rounded-lg"
          );
  return (
    <>
      <button
        aria-describedby={describedBy}
        aria-disabled={press["aria-disabled"]}
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={name}
        className={className}
        data-control={control}
        onClick={open ? () => onOpenChange(false) : press.onClick}
        ref={trigger}
        role={role}
        title={tooltip ?? name}
        type="button"
      >
        {face}
      </button>
      {open ? (
        <AnchoredPanel
          align={align}
          anchorRef={trigger}
          className={`${PANEL_FRAME} text-sm font-normal ${stacked ? "items-start gap-2 p-2" : "p-1"} ${card}`}
          focusOnOpen={focusOnOpen}
          label={title ?? (typeof label === "string" ? label : undefined)}
          onDismiss={() => onOpenChange(false)}
          role="dialog"
        >
          {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the focus handler only notes which control of the panel took the focus, for the hand-off when a commit takes that control away (`useFocusPlace`), and the capture handlers only arm that hand-off at a press inside the panel (`armOnPress`), so the box needs no role of its own. */}
          <div className="contents" {...ARM_ON_PRESS} onFocus={noteFocus} ref={content}>
            {children(close)}
          </div>
        </AnchoredPanel>
      ) : null}
    </>
  );
}
