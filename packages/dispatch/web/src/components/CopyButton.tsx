import { type MouseEvent, type ReactNode, useEffect, useRef, useState } from "react";

import { focusOnDocument } from "../features/shell/roving";
import { ControlKeyHint, keyshortcutsOf } from "../features/shell/ShortcutHelp";
import { useCopyFeedback } from "../hooks/useCopyFeedback";
import {
  dangerText,
  linkHoverText,
  linkText,
  pageFailedText,
  pageMutedText,
  pageRowAct,
  pageStatusDoneText,
  successText,
} from "../theme/classes";
import { useTooltips } from "./tooltips";

/** Copies `value` to the clipboard and confirms inline for 1.5 s ("Copied") or reports the
 *  failure until the next attempt. With `children` the button shows that label before the
 *  icon (e.g. the tmux target itself); without, it is an icon-only button whose 44 px tap
 *  target shrinks to 32 px on desktop like the issue pin. On failure the hint says to select
 *  the text when the button already shows the value, and otherwise prints the value in a
 *  selectable `<code>` so a manual copy is still one step away.
 *
 *  `value` may be a function of the click, for a button whose modifier keys choose what it
 *  copies; such a button names itself through `label`, since the default `Copy <what> <value>`
 *  needs one value to print. `marks` are the host's own handles on the button, by which its
 *  keys find and press it (the Inbox's command bar, `data-inbox-action`), and `shortcutKeys` the
 *  keys that press it where the host draws them: named in its `aria-keyshortcuts`, the first
 *  drawn inside it after the glyph (`ControlKeyHint`). */
export function CopyButton({
  children,
  className,
  label,
  marks,
  shortcutKeys,
  value,
  what,
}: {
  children?: ReactNode;
  className?: string;
  label?: string;
  marks?: Readonly<Record<`data-${string}`, string>>;
  shortcutKeys?: readonly string[];
  value: string | ((event: MouseEvent<HTMLButtonElement>) => string);
  what: string;
}): ReactNode {
  const tooltips = useTooltips();
  // Inside `NoTooltips` each colour is one of the new issue page's: the control is that page's
  // quiet act, muted words that turn ink and underline under the pointer, and its confirmation
  // is the page's passed green or failed red.
  const look = tooltips ? `${linkText} ${linkHoverText}` : `${pageMutedText} ${pageRowAct}`;
  const { copy, reset, status } = useCopyFeedback();
  // Inside `NoTooltips` the clipboard write is the button's own write. From a press until `status`
  // leaves `idle` the copy is out: the button carries `aria-disabled` (never `disabled`, which
  // would drop the focus to the page's body in a browser) and a press of it does nothing. The
  // press records where the focus is meanwhile: its button, then each element the focus lands on
  // after it. `copyText`'s fallback, which runs where the clipboard is missing or refuses, puts
  // the focus in a temporary text box and gives it back to the element that had it once the box
  // goes. Where that element had left the page before the fallback ran, the focus is left on the
  // document (`focusOnDocument`); it then goes back to the last of those still on the page, the
  // button or wherever the reader moved it while the copy was out: right after `copy` returns,
  // where the fallback ran at once, and once `status` leaves `idle`, where it ran after the
  // clipboard refused. A press clears `status` first, so a copy that lands "Copied" while the
  // last one still shows it changes `status` too. The copy is no longer out then and the press is
  // forgotten, so the next press copies again. A focus anywhere else is left where it is.
  const trail = useRef<FocusTrail | null>(null);
  const [copyOut, setCopyOut] = useState(false);
  useEffect(() => {
    if (status === "idle" || trail.current === null) return;
    returnFocus(trail.current);
    trail.current.stop();
    trail.current = null;
    setCopyOut(false);
  }, [status]);
  useEffect(() => () => trail.current?.stop(), []);
  const name = label ?? (typeof value === "string" ? `Copy ${what} ${value}` : `Copy ${what}`);
  const shape =
    children === undefined
      ? "min-w-11 justify-center md:min-w-8"
      : "max-w-full gap-1 px-1 font-mono text-xs font-medium select-text";
  // Inside `NoTooltips` what the button draws can be a value (an ask card's tmux target), and so
  // is what a failed copy prints: each breaks anywhere, and a button that draws one narrows to
  // its line rather than keep its width. A button that draws only its icon holds no value and
  // keeps its size.
  const width = tooltips || children === undefined ? "shrink-0" : "min-w-0";

  return (
    <>
      <button
        aria-disabled={copyOut || undefined}
        aria-keyshortcuts={keyshortcutsOf(shortcutKeys)}
        aria-label={name}
        className={`inline-flex min-h-11 ${width} items-center rounded-lg md:min-h-8 ${shape} ${look} ${className ?? ""}`}
        {...marks}
        onClick={(event) => {
          if (!tooltips && trail.current !== null) return;
          const text = typeof value === "string" ? value : value(event);
          if (tooltips) {
            copy(text);
            return;
          }
          trail.current = followFocus(event.currentTarget);
          setCopyOut(true);
          reset();
          copy(text);
          returnFocus(trail.current);
        }}
        title={tooltips ? name : undefined}
        type="button"
      >
        {children === undefined ? null : (
          <span className={tooltips ? undefined : "min-w-0 max-w-full [overflow-wrap:anywhere]"}>
            {children}
          </span>
        )}
        <svg
          aria-hidden="true"
          className={children === undefined ? "size-4 shrink-0" : "size-3 shrink-0"}
          fill="none"
          viewBox="0 0 20 20"
        >
          <rect height="10" rx="1" stroke="currentColor" strokeWidth="1.5" width="8" x="7" y="7" />
          <path
            d="M5 13H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h8a1 1 0 0 1 1 1v1"
            stroke="currentColor"
            strokeWidth="1.5"
          />
        </svg>
        <ControlKeyHint keys={shortcutKeys} />
      </button>
      {status === "idle" ? null : (
        <span
          aria-live="polite"
          className={`ml-1 text-xs font-medium ${
            status === "copied"
              ? tooltips
                ? successText
                : pageStatusDoneText
              : tooltips
                ? dangerText
                : pageFailedText
          }`}
        >
          {status === "copied" ? (
            "Copied"
          ) : children === status.failed ? (
            "Copy failed - select the text"
          ) : (
            <>
              Copy failed -{" "}
              <code
                className={`select-all font-mono${tooltips ? "" : " max-w-full [overflow-wrap:anywhere]"}`}
              >
                {status.failed}
              </code>
            </>
          )}
        </span>
      )}
    </>
  );
}

/** Where the focus has been since a copy was pressed, its button first, and what stops the
 *  record. */
interface FocusTrail {
  readonly focused: HTMLElement[];
  readonly stop: () => void;
}

/** Records, from `button` on, each element the focus lands on, until `stop`. */
function followFocus(button: HTMLElement): FocusTrail {
  const focused = [button];
  const record = (event: FocusEvent) => {
    if (event.target instanceof HTMLElement) focused.push(event.target);
  };
  document.addEventListener("focusin", record);
  return { focused, stop: () => document.removeEventListener("focusin", record) };
}

/** Puts a focus a copy left on the document back on the last element of `trail` still on the
 *  page. A focus anywhere else stays where it is. */
function returnFocus(trail: FocusTrail): void {
  if (!focusOnDocument()) return;
  for (let index = trail.focused.length - 1; index >= 0; index -= 1) {
    const element = trail.focused[index];
    if (element?.isConnected) {
      element.focus();
      return;
    }
  }
}
