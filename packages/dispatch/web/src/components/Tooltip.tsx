import {
  type FocusEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import { card } from "../theme/classes";
import { AnchoredPanel } from "./AnchoredPanel";

const TOOLTIP_CLASS = `max-w-72 rounded-lg border p-2 text-xs font-normal shadow-lg ${card}`;

/** What a control that carries a tooltip holds: whether the tooltip is open, the id the control
 *  names it by while it is, and the handlers the control spreads onto itself, which open it on
 *  hover and on keyboard focus and close it as the pointer or the focus leaves. */
export interface TooltipTrigger {
  readonly open: boolean;
  readonly id: string;
  readonly show: () => void;
  readonly hide: () => void;
  readonly props: {
    readonly "aria-describedby": string | undefined;
    readonly onBlur: (event: FocusEvent<HTMLElement>) => void;
    readonly onFocus: () => void;
    readonly onMouseEnter: () => void;
    readonly onMouseLeave: () => void;
  };
}

/**
 * The state of one control's tooltip. While `paused` - a column of the table is dragged - it is
 * shut and neither a hover nor a focus opens it, so the drag ends with no tooltip open that the
 * reader did not ask for again. The focus leaves the control only when it goes outside it: a
 * link inside a claim chip keeps the chip's tooltip open.
 */
export function useTooltip({ paused = false }: { paused?: boolean } = {}): TooltipTrigger {
  const [shown, setShown] = useState(false);
  const id = useId();
  const held = useRef(paused);
  held.current = paused;
  const show = useCallback(() => {
    if (!held.current) setShown(true);
  }, []);
  const hide = useCallback(() => setShown(false), []);
  const leave = useCallback((event: FocusEvent<HTMLElement>) => {
    const next = event.relatedTarget;
    if (next instanceof Node && event.currentTarget.contains(next)) return;
    setShown(false);
  }, []);
  useEffect(() => {
    if (paused) setShown(false);
  }, [paused]);
  const open = shown && !paused;
  return {
    open,
    id,
    show,
    hide,
    props: {
      "aria-describedby": open ? id : undefined,
      onBlur: leave,
      onFocus: show,
      onMouseEnter: show,
      onMouseLeave: hide,
    },
  };
}

/**
 * The one tooltip, after the WAI-ARIA tooltip pattern: a short text or table that hangs off its
 * control while the reader hovers it or the keyboard focuses it, placed by `AnchoredPanel`'s
 * rules as every panel is (`tooltip`). It takes no focus, so the control keeps it, and the control
 * names it (`aria-describedby`) while it is open. It closes as the pointer or the focus leaves,
 * on a press outside it, and on Escape wherever the focus is: a tooltip a hover opened hangs off
 * a control the focus may not be on. Every sortable header of a `DataTable`, Time per phase's
 * cell, a Queue cell's number, a claim chip, and a Board card's Legion measures (`Fact`) and its
 * open asks (`OpenAsks`) draw it.
 */
export function Tooltip({
  anchorRef,
  children,
  trigger,
}: {
  anchorRef: RefObject<HTMLElement | null>;
  children: ReactNode;
  trigger: TooltipTrigger;
}): ReactNode {
  if (!trigger.open) return null;
  return (
    <OpenTooltip anchorRef={anchorRef} trigger={trigger}>
      {children}
    </OpenTooltip>
  );
}

function OpenTooltip({
  anchorRef,
  children,
  trigger,
}: {
  anchorRef: RefObject<HTMLElement | null>;
  children: ReactNode;
  trigger: TooltipTrigger;
}): ReactNode {
  const { hide } = trigger;
  useEffect(() => {
    // Escape closes the tooltip wherever the focus is and goes on to whoever owns it. It is read
    // at the document in the capture phase, where an open panel reads Escape too (`AnchoredPanel`):
    // the panel stops the press from going on past the document, which leaves this listener on the
    // same node to run, so one Escape closes the panel and the tooltip both.
    const onEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") hide();
    };
    document.addEventListener("keydown", onEscape, true);
    return () => document.removeEventListener("keydown", onEscape, true);
  }, [hide]);
  return (
    <AnchoredPanel anchorRef={anchorRef} className={TOOLTIP_CLASS} onDismiss={hide} tooltip>
      <div id={trigger.id} role="tooltip">
        {children}
      </div>
    </AnchoredPanel>
  );
}
