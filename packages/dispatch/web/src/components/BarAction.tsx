import { type ReactNode, useState } from "react";

import { barButton } from "./bar-look";
import { CONTROL, type ControlSize } from "./control-names";
import { heldPress } from "./held-press";
import type { PanelTriggerAppearance, PanelTriggerProps } from "./PanelTrigger";
import { ViewPanel } from "./ViewPanel";

/** What `ViewPanel` takes: `PanelTrigger`'s props, but its `data-control`, which is the panel's
 *  own (`view-panel`). The control bar's hint slot takes these. */
export type ViewPanelProps = Omit<PanelTriggerProps, "control">;

/** Why an action cannot be pressed now, as **New document** and **Upload** carry on a closed
 *  issue: the words, and the id of the line under the bar that shows them, which the action names
 *  as its description. The words are the page's own, never a stored text, so they are `words`
 *  and not `text`, the name of a stored text that the reference census holds to its tree
 *  (`features/refs/surface-lists.test.ts`). */
export interface BarActionRefusal {
  readonly id: string;
  readonly words: string;
}

/** An action of the control bar: a label and what it does. */
export interface BarActionProps {
  /** A check item's state: an action given one is a check item (`menuitemcheckbox`) of **More**
   *  at every width, as the Board's **Show Icebox** and **Lanes** are, and a press on it calls
   *  `onPress`, which flips it. */
  readonly checked?: boolean;
  /** The glyph drawn before the label, and alone where the bar draws the action as its icon
   *  (the main action below 640 px of the bar's width), the label then its name. */
  readonly icon: ReactNode;
  readonly label: string;
  /** Keeps the action in **More** at every width, as the Inbox keeps **Mark all read** there. */
  readonly more?: boolean;
  /** What a press does. For an action with a panel, a press opens the panel and tells the page
   *  so here, where the page sets the panel's fields afresh (**Save view**'s name). */
  readonly onPress: () => void;
  /** A panel of its own, as **Save view** opens one: the panel's content, handed the one way to
   *  close it. The action's button is then a `view-panel` trigger (`ViewPanel`). */
  readonly panel?: ViewPanelProps["children"];
  /** Drawn refused (`aria-disabled`) with its refusal as its description and tooltip, and a
   *  press on it does nothing. Unset while it can be pressed. */
  readonly refusal?: BarActionRefusal;
}

/** The action's glyph and label, or its glyph alone. */
function face(icon: ReactNode, label: string, iconOnly: boolean): ReactNode {
  if (iconOnly) return icon;
  return (
    <>
      {icon}
      {label}
    </>
  );
}

/** An action's panel, open or shut, drawn as a button of the bar (`BarAction`) or as an item of
 *  **More** (`MoreMenu`, which gives it its `role` and its item's look). Its own state, since a
 *  panel the page does not need to read is the action's own; the page hears that it opened
 *  through `onPress`. An action given a refusal is held as `BarAction` holds it: `aria-disabled`,
 *  its refusal as its description and tooltip, and a press on it opens nothing. */
export function PanelAction({
  action: { label, onPress, refusal },
  appearance,
  panel,
  role,
  shown,
  size,
}: {
  readonly action: BarActionProps;
  readonly appearance: PanelTriggerAppearance;
  readonly panel: ViewPanelProps["children"];
  readonly role?: ViewPanelProps["role"];
  /** What the trigger shows: the action's glyph and label, or its glyph alone; on an item of
   *  **More**, its label and refusal. */
  readonly shown: ReactNode;
  readonly size?: ControlSize;
}): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <ViewPanel
      align="right"
      appearance={appearance}
      ariaDisabled={refusal !== undefined}
      describedBy={refusal?.id}
      label={shown}
      onOpenChange={(next) => {
        if (next) onPress();
        setOpen(next);
      }}
      open={open}
      role={role}
      size={size}
      title={label}
      tooltip={refusal?.words}
    >
      {panel}
    </ViewPanel>
  );
}

/**
 * One action of the control bar (`ControlRow`): a quiet button, with no border and no fill at
 * rest, as Primer's ActionBar draws its buttons, or - where the bar draws it as the page's one
 * main action - the one filled button of the page, last in the bar (`filled`). An action given a
 * refusal is drawn refused: `aria-disabled`, so it keeps its Tab stop and its refusal reaches a
 * keyboard and a screen reader, its refusal as its tooltip and, through `aria-describedby`, the
 * page's line under the bar that shows it; and a press on it does nothing. An action given a panel
 * is a `view-panel` trigger (`ViewPanel`) that opens it. Below 640 px of the bar's width the bar
 * moves its actions into **More** (`MoreMenu`) and draws its main action as its glyph alone
 * (`iconOnly`), the label still its name.
 */
export function BarAction({
  filled = false,
  iconOnly = false,
  size = "comfortable",
  ...action
}: BarActionProps & {
  /** The page's one main action, drawn filled. */
  readonly filled?: boolean;
  /** Its glyph alone, its label its name. */
  readonly iconOnly?: boolean;
  readonly size?: ControlSize;
}): ReactNode {
  const { icon, label, onPress, panel, refusal } = action;
  if (panel !== undefined) {
    return (
      <PanelAction
        action={action}
        appearance={{ kind: "bar", look: { filled, icon: iconOnly ? icon : undefined } }}
        panel={panel}
        shown={face(icon, label, iconOnly)}
        size={size}
      />
    );
  }
  return (
    <button
      aria-describedby={refusal?.id}
      aria-label={iconOnly ? label : undefined}
      className={barButton(size, filled ? "filled" : "quiet", iconOnly)}
      data-control={CONTROL.barAction}
      title={refusal?.words ?? (iconOnly ? label : undefined)}
      type="button"
      {...heldPress(refusal !== undefined, onPress)}
    >
      {face(icon, label, iconOnly)}
    </button>
  );
}
