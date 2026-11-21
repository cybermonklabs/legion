// The selection bar over words a reader selected in an ask, a comment or a message (INFRA-3231's
// R7): the document editor's own bar (`@legion/proof-editor`'s
// `dispatch-action-bar.ts`) drawn for the texts outside a document. It offers Quote reply (`q`)
// wherever the text has a reply box, and Define (`d`) where the text offers it (an ask's words),
// each with its key beside its name. Mounted once, by the shell: each text records what it does
// with a selection of its words (`useQuoteTarget`), and the bar asks the one holding the selection.
import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import {
  selectionBarBg,
  selectionBarButtonHoverBg,
  selectionBarKey,
  selectionBarText,
} from "../../theme/classes";
import {
  type QuotableSelection,
  quotableSelection,
  quoteSelection,
  useQuoteTarget,
} from "./quote-reply";

/** How far the bar keeps from the window's edges, and from the words it stands over: nearer than
 *  the document's bar, whose blocks stand further apart than an ask's or a comment's lines, so it
 *  covers no more than the line just above them. */
const MARGIN = 12;
const GAP = 4;
/** How long a touch selection stands still before its bar shows, as the document's waits. */
const TOUCH_SETTLE_MS = 400;

/** Text a reader may quote in a reply, with what the quote does (`useQuoteTarget`): drawn as its
 *  children are, the wrapper taking no box of its own. `onQuote` absent offers no Quote reply
 *  over these words while their thread takes none. */
export function QuotableText({
  children,
  onQuote,
}: {
  children: ReactNode;
  onQuote: ((blocks: readonly string[]) => void) | undefined;
}): ReactNode {
  const ref = useRef<HTMLDivElement>(null);
  useQuoteTarget(ref, onQuote === undefined ? null : { quote: onQuote });
  return (
    <div className="contents" ref={ref}>
      {children}
    </div>
  );
}

/** The selection a Define stepped the bar aside from: the bar stays hidden over it, so the answer
 *  beside the words is the one thing drawn over them, until the reader selects anew. */
let asideFrom: Range | null = null;
const redraws = new Set<() => void>();

/** Steps the bar aside from the current selection, for an action that answers beside it. */
export function stepAsideFromSelection(): void {
  const selection = window.getSelection();
  asideFrom = selection === null || selection.rangeCount === 0 ? null : selection.getRangeAt(0);
  for (const redraw of redraws) redraw();
}

function steppedAside(): boolean {
  const selection = window.getSelection();
  if (asideFrom === null || selection === null || selection.rangeCount === 0) return false;
  const range = selection.getRangeAt(0);
  const same =
    range.startContainer === asideFrom.startContainer &&
    range.startOffset === asideFrom.startOffset &&
    range.endContainer === asideFrom.endContainer &&
    range.endOffset === asideFrom.endOffset;
  if (!same) asideFrom = null;
  return same;
}

/** Places the bar over the selection's first line, centered on it, where there is room above and
 *  else under the whole selection, inside the text's own column where it fits there and else
 *  inside the window, as the document's bar is placed (`positionBar`): it never covers what the
 *  page draws beside or above the text. Over the first line rather than the whole selection, the
 *  bar covers no more of the line above than the words beside where the selection starts. */
function place(bar: HTMLElement, { column, firstLine, rect }: QuotableSelection): void {
  const box = bar.getBoundingClientRect();
  const above = firstLine.top - box.height - GAP;
  const below = rect.bottom + GAP;
  const top =
    above >= MARGIN
      ? above
      : below + box.height <= window.innerHeight - MARGIN
        ? below
        : Math.max(MARGIN, Math.min(rect.top, window.innerHeight - box.height - MARGIN));
  const anchor = above >= MARGIN ? firstLine : rect;
  const center = (anchor.left + anchor.right) / 2;
  const minLeft = Math.max(MARGIN, column.left);
  const maxLeft = Math.min(window.innerWidth - MARGIN, column.right) - box.width;
  const left =
    minLeft <= maxLeft
      ? Math.max(minLeft, Math.min(center - box.width / 2, maxLeft))
      : Math.max(MARGIN, Math.min(center - box.width / 2, window.innerWidth - box.width - MARGIN));
  bar.style.left = `${left}px`;
  bar.style.top = `${top}px`;
}

function BarButton({
  hotkey,
  label,
  onPress,
  touch,
}: {
  hotkey: string;
  label: string;
  onPress: () => void;
  touch: boolean;
}): ReactNode {
  return (
    <button
      className={`flex items-center gap-1.5 rounded-full ${touch ? "min-h-11 px-3.5 text-sm" : "px-1.5 py-0.5"} ${selectionBarButtonHoverBg}`}
      data-selection-action={label}
      onClick={onPress}
      type="button"
    >
      {label}
      <kbd
        className={`min-w-[18px] rounded px-1 text-center font-mono text-xs leading-4 font-medium ${selectionBarKey}`}
      >
        {hotkey}
      </kbd>
    </button>
  );
}

/** The bar, while the reader has words of an ask, a comment or a message selected. */
export function SelectionBar(): ReactNode {
  const [shown, setShown] = useState<{ selection: QuotableSelection; touch: boolean } | null>(null);
  const barRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let pointerDown = false;
    let pointerType: string | null = null;
    let timer: number | undefined;
    const touch = () =>
      pointerType === "touch" ||
      (pointerType === null && window.matchMedia?.("(pointer: coarse)").matches === true);
    const read = () => {
      timer = undefined;
      // A drag that is still selecting has not chosen its words yet.
      const selection = pointerDown || steppedAside() ? null : quotableSelection();
      setShown(selection === null ? null : { selection, touch: touch() });
    };
    const schedule = () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(read, touch() && !pointerDown ? TOUCH_SETTLE_MS : 0);
    };
    const down = (event: PointerEvent) => {
      if (barRef.current?.contains(event.target as Node)) return;
      pointerType = event.pointerType;
      pointerDown = true;
      schedule();
    };
    const up = () => {
      if (!pointerDown) return;
      pointerDown = false;
      schedule();
    };
    redraws.add(read);
    document.addEventListener("selectionchange", schedule);
    document.addEventListener("pointerdown", down, true);
    document.addEventListener("pointerup", up, true);
    document.addEventListener("pointercancel", up, true);
    document.addEventListener("focusin", schedule);
    window.addEventListener("scroll", schedule, true);
    window.addEventListener("resize", schedule);
    return () => {
      window.clearTimeout(timer);
      redraws.delete(read);
      document.removeEventListener("selectionchange", schedule);
      document.removeEventListener("pointerdown", down, true);
      document.removeEventListener("pointerup", up, true);
      document.removeEventListener("pointercancel", up, true);
      document.removeEventListener("focusin", schedule);
      window.removeEventListener("scroll", schedule, true);
      window.removeEventListener("resize", schedule);
    };
  }, []);

  useLayoutEffect(() => {
    const bar = barRef.current;
    if (bar !== null && shown !== null && !shown.touch) place(bar, shown.selection);
  }, [shown]);

  if (shown === null) return null;
  const { selection, touch } = shown;
  const define = selection.target.define;
  return createPortal(
    <div
      aria-label="Selected words"
      // On a touch screen the bar docks at the foot of the screen, clear of the browser's own
      // selection menu, as the document's does.
      className={`fixed z-[190] flex items-center gap-1.5 rounded-full border px-2 py-1.5 text-xs ${selectionBarBg} ${selectionBarText} ${
        touch
          ? "bottom-[calc(env(safe-area-inset-bottom,0px)+12px)] left-1/2 max-w-[calc(100vw-24px)] -translate-x-1/2"
          : ""
      }`}
      data-selection-bar=""
      // A press on the bar must not take the reader's selection, which its actions read.
      onPointerDown={(event) => event.preventDefault()}
      ref={barRef}
      role="toolbar"
      // Off screen until placed beside the words, so it is never drawn a frame elsewhere.
      style={touch ? undefined : { left: -9999, top: -9999 }}
    >
      <BarButton hotkey="q" label="Quote reply" onPress={quoteSelection} touch={touch} />
      {define === undefined ? null : (
        <BarButton hotkey="d" label="Define" onPress={define} touch={touch} />
      )}
    </div>,
    document.body
  );
}
