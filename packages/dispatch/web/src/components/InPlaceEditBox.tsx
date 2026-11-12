import {
  type ComponentPropsWithRef,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useRef,
} from "react";

import {
  bgTransparent,
  borderDefault,
  borderStrongHover,
  borderTransparent,
  card,
  focusBorder,
  focusVisibleRing,
  surfaceMutedStrongBg,
  textPrimaryOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";

/**
 * The one grid cell the read view and the edit view are drawn in. Both take the same cell, so a
 * switch between them moves nothing beside them; the edit view is taken out of the flow on top
 * of it (`coverRule` below), so the read view alone sizes the cell.
 */
const CELL = "col-start-1 row-start-1";

/**
 * The floor the editor's own surface takes: twenty characters, which is the width the editor has
 * at 1536 px and wider, where the slot is sized by its content. A box sized by an 11-character
 * heading would leave about 100 px to type in. The surface is the cell where the edit view
 * covers the read view, and the panel where it opens over it.
 */
const WIDTH_FLOOR = "min-w-[20ch]";

/**
 * The box rules: padding, border, radius and type metrics, one per place a header editor sits.
 * They live here and nowhere else, and one rule is applied to the read view and the edit view
 * alike, so no call site can give its two views different metrics — which is what moved every
 * control beside the title between the mousedown that ended an edit and the mouseup.
 *
 * - `title`: the issue title's own heading-sized box.
 * - `rail`: a pill in the issue header's details rail (the message route, the parent).
 */
export const inPlaceBoxRules = {
  rail: `inline-flex min-h-11 max-w-[14ch] shrink-0 items-center truncate rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`,
  title: `min-w-0 rounded-lg border px-2 py-1 text-[22px] leading-7 font-semibold tracking-tight md:py-0 ${borderTransparent} ${textPrimaryOnSurface}`,
} as const;

export type InPlaceBox = keyof typeof inPlaceBoxRules;

/** What the read view adds to the box rule: how its own text behaves in that box. */
const readRules: Record<InPlaceBox, string> = {
  // A pill is one line and ellipsises (`truncate` in the box rule); the heading wraps instead,
  // and wrapping is what decides the cell's height, so it is the component's and not a caller's.
  rail: "",
  title: "break-words",
};

/** What a read view that can still be opened adds: the affordance that says it is editable. */
const openableRules: Record<InPlaceBox, string> = {
  // The pill is a button and says so already.
  rail: "",
  title: `cursor-text ${borderStrongHover}`,
};

/**
 * The edit view that covers the read view: out of the flow, so it contributes nothing to the
 * cell's intrinsic width and the read view keeps sizing the cell while it is open. An `input`
 * in the flow would bring its own default width of about twenty characters to a content-sized
 * slot, and the box would still grow for a title shorter than that.
 */
const coverRule = `absolute inset-x-0 top-0 outline-none ${bgTransparent} ${borderStrongHover} ${focusBorder}`;

/**
 * The edit view that opens over the read view: a fixed panel `AnchoredPanel` portals to the
 * body, so an editor that cannot fit the read view's box — one wanting a label, a field, Save,
 * Cancel and an error line — costs the row no layout at all.
 */
const panelRule = `z-50 w-72 max-w-[calc(100vw-1rem)] rounded-lg border p-2 shadow-lg ${WIDTH_FLOOR} ${card} ${borderDefault}`;

/**
 * Everything the read view must carry, for the caller to spread onto the element it draws. The
 * box rule and the behaviour of an in-place edit are in this object alone: what opens the
 * editor, and what keeps the read view mounted but out of the way while it is open.
 */
export interface InPlaceReadProps {
  readonly "aria-hidden"?: true;
  readonly className: string;
  readonly inert?: boolean;
  readonly onClick?: () => void;
  readonly onFocus?: () => void;
  readonly onKeyDown?: (event: KeyboardEvent<HTMLElement>) => void;
  readonly tabIndex?: number;
}

/** The edit view's field. No `className` and no `style`: its box is this component's. */
export type InPlaceInputProps = Omit<ComponentPropsWithRef<"input">, "className" | "style">;

type InPlaceEditBoxProps = {
  /** Which box rule this editor draws. */
  box: InPlaceBox;
  /** Where the cell sits in its parent's layout — never a box rule, which is this component's. */
  className?: string;
  /** Nothing can be edited here (a closed issue): the read view opens nothing. */
  disabled?: boolean;
  editing: boolean;
  /** What names this editor in the DOM, so a call-site row can read every one of them. */
  name: string;
  onOpen: () => void;
  read: (props: InPlaceReadProps) => ReactNode;
} & (
  | {
      /** The edit view fits the read view's box and covers it. */
      overlay: "cover";
      input: InPlaceInputProps;
    }
  | {
      /** The edit view cannot fit that box, so it opens over it. */
      overlay: "panel";
      children: ReactNode;
      /** An outside press or Escape: the editor's own discard, as Cancel does it. */
      onDismiss: () => void;
      onSubmit: (event: FormEvent<HTMLFormElement>) => void;
    }
);

/**
 * The box every in-place editor of the issue header draws.
 *
 * The read view and the edit view share one grid cell and one box rule, the read view stays
 * mounted and sizes that cell in both states, and the cell takes a floor of twenty characters.
 * While the edit view covers it, the read view holds the box's size and nothing else: out of the
 * accessibility tree, out of the tab order, inert, invisible while it keeps its space (never
 * `display: none`), and carrying none of the handlers that would reopen the editor from a copy
 * nobody can see. An editor whose edit view cannot fit that box opens over it in an
 * `AnchoredPanel` instead, where its read view stays visible and keeps its place in the row: it
 * is the panel's anchor and the control the reader pressed, so it stays in the tab order too.
 */
export function InPlaceEditBox(props: InPlaceEditBoxProps): ReactNode {
  const { box, className, disabled = false, editing, name, onOpen, read } = props;
  const cell = useRef<HTMLDivElement>(null);
  const rule = inPlaceBoxRules[box];
  const covered = editing && props.overlay === "cover";
  const open = disabled ? undefined : onOpen;
  let readProps: InPlaceReadProps;
  if (covered) {
    readProps = {
      "aria-hidden": true,
      className: `${CELL} ${rule} ${readRules[box]} invisible`,
      inert: true,
      tabIndex: -1,
    };
  } else {
    const shared = {
      className: `${CELL} ${rule} ${readRules[box]} ${disabled ? "" : openableRules[box]}`,
      onClick: open,
    };
    readProps =
      props.overlay === "panel"
        ? shared
        : {
            ...shared,
            // A heading is not a control, so the keyboard needs both halves of one: a stop in
            // the tab order, and Enter or Space to open the editor from it.
            onFocus: open,
            onKeyDown:
              open === undefined
                ? undefined
                : (event: KeyboardEvent<HTMLElement>) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      open();
                    }
                  },
            tabIndex: disabled ? -1 : 0,
          };
  }

  return (
    <div
      className={`relative grid ${props.overlay === "cover" ? WIDTH_FLOOR : ""} ${className ?? ""}`}
      data-in-place-edit={name}
      ref={cell}
    >
      {read(readProps)}
      {!editing ? null : props.overlay === "cover" ? (
        <input
          {...props.input}
          className={`${CELL} ${rule} ${coverRule}`}
          data-in-place-edit-editor={name}
        />
      ) : (
        <AnchoredPanel anchorRef={cell} className={panelRule} onDismiss={props.onDismiss}>
          {/* Enter saves from anywhere in the panel, which is what the form is for; Escape and
              an outside press reach `onDismiss` through `AnchoredPanel`, which takes the press
              at the document's capture phase. */}
          <form
            className="flex flex-wrap items-center gap-2"
            data-in-place-edit-editor={name}
            onSubmit={props.onSubmit}
          >
            {props.children}
          </form>
        </AnchoredPanel>
      )}
    </div>
  );
}
