import { type KeyboardEvent, type ReactNode, useState } from "react";

import { roveFocus } from "../features/shell/roving";
import {
  focusVisibleRing,
  secondaryButtonDisabledText,
  surfaceMutedHoverBg,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { type BarActionProps, PanelAction } from "./BarAction";
import { DOTS_GLYPH } from "./bar-look";
import { CONTROL, type ControlSize } from "./control-names";
import { DisplayMenu, type DisplayMenuProps } from "./DisplayMenu";
import { armHandOff } from "./focus-place";
import { heldPress } from "./held-press";
import { PanelTrigger, type PanelTriggerAppearance } from "./PanelTrigger";

/** An item of **More**: a row the menu's width, at the touch target the display menu's choices
 *  keep. A refused item says why under its label. */
const ITEM = `flex min-h-11 w-full items-center gap-2 rounded px-2 text-left text-sm font-medium outline-none focus-visible:ring-2 ${focusVisibleRing} ${textSecondaryOnCanvas} ${surfaceMutedHoverBg} refused:cursor-not-allowed ${secondaryButtonDisabledText}`;

/** An item that opens a panel of its own draws in every other item's look (`ITEM`): an action's
 *  panel (**Save view**) and the display menu. */
const ITEM_APPEARANCE: PanelTriggerAppearance = { className: ITEM, kind: "class" };

/** The items the arrow keys walk. */
const ITEMS = '[role="menuitem"], [role="menuitemcheckbox"]';

/** The arrow keys walk the items, as they walk the page's other lists (`shell/roving.ts`). A key
 *  pressed in a panel an item opened, which React hands up through the menu though the panel is
 *  drawn elsewhere in the page, is that panel's own. */
function walk(event: KeyboardEvent<HTMLElement>): void {
  const { currentTarget: menu, key, target } = event;
  if (key !== "ArrowDown" && key !== "ArrowUp") return;
  if (!(target instanceof HTMLElement) || !menu.contains(target)) return;
  event.preventDefault();
  roveFocus([...menu.querySelectorAll<HTMLElement>(ITEMS)], target, key === "ArrowDown" ? 1 : -1);
}

/** An item's label, and under it its refusal where it carries one. */
function words({ label, refusal }: BarActionProps): ReactNode {
  return (
    <span className="flex flex-col">
      {label}
      {refusal === undefined ? null : <span className="text-xs font-normal">{refusal.words}</span>}
    </span>
  );
}

/** One item of **More**. A check item leaves the menu open, so the reader can flip the next; any
 *  other press shuts the menu, putting the reader back on **More**, before it acts, and arms the
 *  hand-off on **More** there (`armHandOff`): the press acts from **More** now, so **More**'s
 *  going - its last action leaving, as **Mark all read** does once nothing is unread - is the
 *  press's doing, and the bar hands the focus on (`ControlRow`). An item with a panel of its own
 *  (**Save view** below 640 px of the bar's width) opens its panel over the menu, which stays
 *  open under it. A refused item is held (`aria-disabled`) and a press on it does nothing. */
function Item({ action, close }: { action: BarActionProps; close: () => void }): ReactNode {
  const { checked, icon, label, onPress, panel, refusal } = action;
  if (panel !== undefined) {
    return (
      <PanelAction
        action={action}
        appearance={ITEM_APPEARANCE}
        panel={panel}
        role="menuitem"
        shown={
          <>
            {icon}
            {words(action)}
          </>
        }
      />
    );
  }
  const described = {
    "aria-describedby": refusal?.id,
    "aria-label": refusal === undefined ? undefined : label,
  };
  if (checked !== undefined) {
    return (
      <button
        aria-checked={checked}
        className={ITEM}
        role="menuitemcheckbox"
        type="button"
        {...described}
        {...heldPress(refusal !== undefined, onPress)}
      >
        <span aria-hidden="true" className={checked ? undefined : "invisible"}>
          ✓
        </span>
        {icon}
        {words(action)}
      </button>
    );
  }
  return (
    <button
      className={ITEM}
      role="menuitem"
      type="button"
      {...described}
      {...heldPress(refusal !== undefined, () => {
        close();
        const more = document.activeElement;
        if (more?.getAttribute("data-control") === CONTROL.moreMenu) armHandOff(more);
        onPress();
      })}
    >
      {icon}
      {words(action)}
    </button>
  );
}

export interface MoreMenuProps {
  /** The actions it holds, in the page's order: the ones the page keeps there, and below 640 px
   *  of the bar's width every action of the bar. */
  readonly actions: readonly BarActionProps[];
  /** The display menu, as its first item, where the page sets the bar's narrow display rule and
   *  the bar is narrower than 640 px (the Inbox and its drawer, `ControlRow`). It opens its panel
   *  over the menu, which stays open under it, and keeps its own `data-control`. */
  readonly display?: DisplayMenuProps;
  readonly size?: ControlSize;
}

/**
 * The control bar's **More** (⋯): the menu of the page's actions that have no place in the bar's
 * line - the ones the page keeps there (`more`), as the Inbox keeps **Mark all read**, and below
 * 640 px of the bar's width every action of the bar, and there the Inbox's display menu too. It
 * is a quiet button, its glyph alone and named **More**, that opens a `menu` whose items are
 * buttons (`menuitem`), a check item where the action carries a checked state
 * (`menuitemcheckbox`), and a trigger of its own panel where the item opens one - an action's,
 * as **Save view**'s, or the display menu. The bar draws it only while it holds an item
 * (`ControlRow`).
 */
export function MoreMenu({ actions, display, size = "comfortable" }: MoreMenuProps): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <PanelTrigger
      align="right"
      appearance={{ kind: "bar", look: { icon: DOTS_GLYPH } }}
      control={CONTROL.moreMenu}
      label="More"
      onOpenChange={setOpen}
      open={open}
      size={size}
    >
      {(close) => (
        <div aria-label="More" className="flex w-full flex-col" onKeyDown={walk} role="menu">
          {display === undefined ? null : (
            <DisplayMenu {...display} appearance={ITEM_APPEARANCE} role="menuitem" size={size} />
          )}
          {actions.map((action) => (
            <Item action={action} close={close} key={action.label} />
          ))}
        </div>
      )}
    </PanelTrigger>
  );
}
