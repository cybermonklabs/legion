import { type ReactNode, useId } from "react";

import {
  focusVisibleRing,
  selectedCardBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../theme/classes";
import { CONTROL, type ControlSize } from "./control-names";
import { PanelTrigger, type PanelTriggerAppearance } from "./PanelTrigger";

/** One of a section's mutually exclusive choices, as the menu names it. */
export interface DisplayMenuOption {
  readonly label: string;
  readonly value: string;
}

/** One pick the menu offers: a heading and the choices under it. */
export interface DisplayMenuSection {
  /** Tells this section's radios apart from the next section's; never read by anything else. */
  readonly id: string;
  readonly label: string;
  readonly onChange: (value: string) => void;
  readonly options: readonly DisplayMenuOption[];
  /** Which option is taken. A value naming no option leaves the section with none checked. */
  readonly value: string;
}

export interface DisplayMenuProps {
  /** How the trigger is drawn (`PanelTriggerAppearance`): in the control bar's look where the bar
   *  draws the menu - quiet, and its glyph alone below 640 px of the bar's width, its label still
   *  its name - and in **More**'s item's look where **More** holds it (`role`). A menu outside
   *  any bar is given none and keeps `PanelTrigger`'s own look. */
  readonly appearance?: PanelTriggerAppearance;
  /** The trigger's text and the panel's name: the Inbox's menu is **View**, the Issues tab's
   *  **Group by**. */
  readonly label: string;
  readonly onOpenChange: (open: boolean) => void;
  /** The host owns it, so a page keeps one panel open at a time. */
  readonly open: boolean;
  /** `menuitem` where a menu holds the display menu as one of its items: the control bar's
   *  **More** below 640 px of the bar's width on the Inbox (`MoreMenu`), which draws it in its
   *  own item's look (`appearance`). */
  readonly role?: "menuitem";
  readonly sections: readonly DisplayMenuSection[];
  readonly size?: ControlSize;
}

/** One of a section's choices: a radio, so the browser reads the section as one pick and the
 *  arrow keys walk it. The whole row is the label, which is what gives it its 44 px target. */
function Choice({
  chosen,
  group,
  label,
  onChoose,
  value,
}: {
  chosen: boolean;
  /** The radio group this choice belongs to. */
  group: string;
  label: string;
  onChoose: (value: string) => void;
  value: string;
}): ReactNode {
  return (
    <label
      className={`flex min-h-11 w-full cursor-pointer items-center gap-2 rounded px-2 text-left text-sm ${
        chosen ? `font-semibold ${selectedCardBg} ${textSecondaryOnSurface}` : textSecondaryOnCanvas
      }`}
    >
      <input
        checked={chosen}
        className={`size-4 shrink-0 outline-none focus-visible:ring-2 ${focusVisibleRing}`}
        name={group}
        onChange={() => onChoose(value)}
        type="radio"
        value={value}
      />
      {label}
    </label>
  );
}

/**
 * The one display menu: the button that opens a page's list settings - how the list is ordered,
 * how it is banded, which of its lists is showing - and the panel holding them, one `fieldset`
 * with a `legend` per pick so the browser reads each section as one choice rather than as a
 * handful of loose controls.
 *
 * It is one `PanelTrigger` under its own name, so the display menu is dismissed, placed and named
 * by the rules every other panel in Dispatch follows, and a row finds it by `data-control` rather
 * than by a hook of one page's own. A choice leaves the panel open: a reader setting the order
 * usually sets the banding next.
 *
 * A section's value is a plain `string` because a menu holds sections of different value types at
 * once - the Inbox's sort, its banding and its thread order are three unrelated unions - and one
 * type parameter cannot stand for all of them. The caller, which knows each section's own type,
 * narrows the value it is handed back.
 */
export function DisplayMenu({
  appearance,
  label,
  onOpenChange,
  open,
  role,
  sections,
  size,
}: DisplayMenuProps): ReactNode {
  const id = useId();
  return (
    <PanelTrigger
      align={role === undefined ? "left" : "right"}
      appearance={appearance}
      control={CONTROL.displayMenu}
      label={label}
      onOpenChange={onOpenChange}
      open={open}
      role={role}
      size={size}
    >
      {() =>
        sections.map((section) => (
          <fieldset className="py-1" key={section.id}>
            <legend
              className={`px-2 pb-1 text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}
            >
              {section.label}
            </legend>
            {section.options.map((option) => (
              <Choice
                chosen={option.value === section.value}
                group={`${id}-${section.id}`}
                key={option.value}
                label={option.label}
                onChoose={section.onChange}
                value={option.value}
              />
            ))}
          </fieldset>
        ))
      }
    </PanelTrigger>
  );
}
