import { type RefObject, useEffect, useLayoutEffect, useRef, useState } from "react";
import { NavigationType, useLocation, useNavigationType } from "react-router-dom";

import { DIALOG_SCOPE, useKeymap, useKeymapScope } from "./keymap";

/** Below Tailwind's `xl`: the margin is a sheet and the sidebar a drawer. */
export const COMPACT_VIEWPORT_QUERY = "(max-width: 1279px)";
/** Below Tailwind's `md`: a phone, where a margin thread opens full-screen. */
export const PHONE_VIEWPORT_QUERY = "(max-width: 767px)";
/** Tailwind's `md` itself: true exactly when the `md:` classes apply. A caller that pairs this
 *  query with `md:` classes on the same element must keep the two in step, since a predicate a
 *  pixel out leaves a fractional viewport width - zoom, a fractional device-pixel ratio - with
 *  one answer from the query and the other from the classes. The unit matters as much as the
 *  number: Tailwind declares `md` as `48rem` (`--breakpoint-md` in its `theme.css`), and a media
 *  query's `rem` is the browser's default font size, so `768px` agrees with it only at a 16 px
 *  default; at a 20 px one the `md:` classes switch at 960 px while a pixel query still would at
 *  768. */
export const MD_VIEWPORT_QUERY = "(min-width: 48rem)";
/** Tailwind's `xl`: from 1280 px the Inbox shows its reading pane beside the list. In `rem`, and
 *  for `MD_VIEWPORT_QUERY`'s reason: it is paired with `xl:` classes, which Tailwind declares at
 *  `80rem`, so only a `rem` query switches with them at any default font size. */
export const READING_PANE_QUERY = "(min-width: 80rem)";

const focusableSelector = [
  "a[href]",
  "button:not(:disabled)",
  "textarea:not(:disabled)",
  "input:not(:disabled)",
  "select:not(:disabled)",
  '[tabindex]:not([tabindex="-1"])',
].join(", ");

/** The controls focus can reach inside `container`, in order. One under a `hidden` ancestor - a
 *  composer kept mounted, out of sight, for a send it has out - takes no focus, so it is neither
 *  where the trap starts nor where it wraps. */
function focusableIn(container: HTMLElement): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(focusableSelector)].filter(
    (element) => element.closest("[hidden]") === null
  );
}

// Depth-counted so a picker opening inside an already-locked sheet does not unlock scroll
// when the picker (not the sheet) closes first; only the outermost acquire/release touches
// the DOM, and it restores the exact inline values a page may already have set.
const scrollLock = {
  depth: 0,
  saved: { overflow: "", paddingRight: "" },
  acquire(): void {
    if (this.depth === 0) {
      this.saved = {
        overflow: document.body.style.overflow,
        paddingRight: document.body.style.paddingRight,
      };
      const scrollbarWidth = window.innerWidth - document.documentElement.clientWidth;
      document.body.style.overflow = "hidden";
      if (scrollbarWidth > 0) {
        document.body.style.paddingRight = `${scrollbarWidth}px`;
      }
    }
    this.depth += 1;
  },
  release(): void {
    this.depth = Math.max(0, this.depth - 1);
    if (this.depth === 0) {
      document.body.style.overflow = this.saved.overflow;
      document.body.style.paddingRight = this.saved.paddingRight;
    }
  },
};

// Every open dialog's container. Escape is a `dialog`-scope keymap binding so it works wherever
// the focus is - on the dialog itself in its first frame, on its field after that, or on the page
// after a press on its text took it there; only the innermost open dialog closes. Two
// dialogs nested by DOM containment (a picker opened inside an already-open sheet) resolve by
// containment, the nested one winning regardless of which mounted its effect first. Two dialogs
// that are siblings - neither containing the other, such as the phone navigation sheet and a
// second dialog a control inside it opens without closing the sheet first - have no containment
// relationship to resolve by, so the most recently opened of the two wins instead.
const openDialogs = new Set<HTMLElement | null>();

function innermostOpenDialog(): HTMLElement | null | undefined {
  let innermost: HTMLElement | null | undefined;
  for (const container of openDialogs) {
    const containerIsAncestorOfInnermost =
      container !== null &&
      innermost !== null &&
      innermost !== undefined &&
      container.contains(innermost);
    if (innermost === undefined || !containerIsAncestorOfInnermost) {
      innermost = container;
    }
  }
  return innermost;
}

/** Whether a dialog is open. Its Escape is then its own wherever the focus is: on the dialog
 *  itself in the frame before its field takes the focus, or on the page after a press on its
 *  text took the focus there. A layer under it that answers Escape itself leaves the key to it. */
export function dialogOpen(): boolean {
  return openDialogs.size > 0;
}

/** Where an open dialog gives the focus back when it closes, and whether it made its own element
 *  focusable to take the focus as it opened. */
interface FocusRecord {
  giveBackTo: HTMLElement | null;
  madeFocusable: boolean;
}

// Every open dialog's focus record, by its element, from the commit that opens the dialog to the
// commit that closes it. Two dialogs read each other's here. A dialog that closes in the commit
// that opens another (the navigation sheet's Search, which closes the sheet and opens the
// palette) leaves the focus with the new one and hands it its own give-back target. A dialog
// opened inside another in one commit (the margin sheet and its thread) gives the focus back to
// that dialog's field.
const focusRecords = new Map<HTMLElement, FocusRecord>();

export interface UseDialogOptions {
  /** Element to focus first instead of the container's first focusable descendant. */
  initialFocusRef?: RefObject<HTMLElement | null>;
  onClose: () => void;
  open: boolean;
}

export interface DialogHandle<T extends HTMLElement> {
  containerRef: RefObject<T | null>;
}

/**
 * Focus trap, Escape-to-close, focus restoration, and a body scroll lock for an overlay
 * (the search palette, the phone navigation drawer, the phone margin sheet, and the reference
 * picker all use this). On open the focus moves to the dialog itself at once and to its field a
 * frame later, so no key typed in between reaches the page behind it. While open it pushes the
 * `dialog` keymap scope, which masks every page and global binding, and registers Escape there
 * for the innermost open dialog, whatever element holds focus; a descendant that owns its Escape
 * (the margin Composer dismissing its picker) stops propagation before the dispatcher sees it.
 * Tab trapping listens on the container.
 */
export function useDialog<T extends HTMLElement>({
  initialFocusRef,
  onClose,
  open,
}: UseDialogOptions): DialogHandle<T> {
  const containerRef = useRef<T>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useKeymapScope(open ? DIALOG_SCOPE : null);
  useKeymap(
    DIALOG_SCOPE,
    open
      ? [
          {
            id: "dialog.close",
            inEditable: true,
            keys: "Escape",
            label: "Close dialog",
            run: () => onCloseRef.current(),
            when: () => innermostOpenDialog() === containerRef.current,
          },
        ]
      : []
  );

  // This dialog's entry of `focusRecords`, set in the commit that opens it.
  const focusRecord = useRef<FocusRecord | null>(null);

  // The focus moves to the dialog itself at once, in the commit that opens it, so a key typed
  // before its field takes the focus reaches nothing behind it: a Space on the control that
  // opened it would press that control again (Approve, behind Request changes, would approve the
  // document). A focus already inside the dialog stays where it is: an element with React's
  // `autoFocus` attribute took it as it mounted, or Details' drawer became a dialog on a phone
  // while the focus was in it (`Margin.tsx`). The dialog is made focusable for this alone
  // (`tabindex="-1"`, out of the Tab order) unless it already is. A layout effect, so this holds
  // whatever update opened the dialog.
  useLayoutEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    const active = document.activeElement;
    // The focus goes back where it was, unless that was the body: a commit that removed the
    // control that held the focus as it opened this dialog leaves it there, and a dialog it
    // closed hands this one its own target (`focusRecords`).
    const record: FocusRecord = {
      giveBackTo: active instanceof HTMLElement && active !== document.body ? active : null,
      madeFocusable: false,
    };
    focusRecord.current = record;
    if (container === null) {
      return;
    }
    focusRecords.set(container, record);
    if (!container.contains(active)) {
      record.madeFocusable = !container.hasAttribute("tabindex");
      if (record.madeFocusable) {
        container.tabIndex = -1;
      }
      container.focus({ preventScroll: true });
    }
    return () => {
      focusRecords.delete(container);
    };
  }, [open]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    scrollLock.acquire();
    const stopBeingFocusable = () => {
      const record = focusRecord.current;
      if (container !== null && record?.madeFocusable === true) {
        container.removeAttribute("tabindex");
        record.madeFocusable = false;
      }
    };

    // The dialog's field takes the focus a frame later, so a focus a descendant takes in its own
    // effect lands first and is not taken back: the margin sheet's composer focuses its text box
    // in an effect once it is on screen (`MentionComposer`'s `autoFocus` prop). Only a focus still
    // on the dialog itself, or gone from it, is moved. A dialog opened inside this one in the same
    // commit holds the focus instead, and gives it back to this one's field rather than to its
    // element. The element then stops being focusable, so a press on the dialog's text puts the
    // focus where the browser puts it, as before this hook took the focus at once.
    const initialFocusFrame = window.requestAnimationFrame(() => {
      if (container === null) {
        return;
      }
      const active = document.activeElement;
      const field = initialFocusRef?.current ?? focusableIn(container)[0];
      if (active === container || !container.contains(active)) {
        field?.focus();
      } else if (field !== undefined) {
        for (const record of focusRecords.values()) {
          if (record.giveBackTo === container) {
            record.giveBackTo = field;
          }
        }
      }
      const aTarget = [...focusRecords.values()].some((record) => record.giveBackTo === container);
      if (document.activeElement !== container && !aTarget) {
        stopBeingFocusable();
      }
    });

    openDialogs.add(container);

    const handleTab = (event: KeyboardEvent) => {
      if (event.key !== "Tab" || container === null) {
        return;
      }
      const elements = focusableIn(container);
      const first = elements[0];
      const last = elements[elements.length - 1];
      if (first === undefined || last === undefined) {
        event.preventDefault();
        return;
      }
      // The dialog itself holds the focus in its first frame: Tab goes to its first control and
      // Shift+Tab to its last, not to the page behind it.
      if (document.activeElement === container) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus();
      } else if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    container?.addEventListener("keydown", handleTab);

    // A passive effect's cleanup, so the focus goes back after the commit that closes the dialog
    // has drawn the page under it: a control that commit enables again takes the focus.
    return () => {
      window.cancelAnimationFrame(initialFocusFrame);
      openDialogs.delete(container);
      container?.removeEventListener("keydown", handleTab);
      stopBeingFocusable();
      scrollLock.release();
      const giveBackTo = focusRecord.current?.giveBackTo ?? null;
      // A dialog that opened in the commit that closes this one holds the focus. It keeps it, and
      // gives it back where this one would have when its own target is none, or is inside this
      // closing dialog: the commit removed the control that held the focus, or hid it with this
      // dialog.
      const active = document.activeElement;
      const holder =
        container?.contains(active) === true
          ? undefined
          : [...focusRecords].find(([other]) => other.contains(active))?.[1];
      if (holder !== undefined) {
        if (holder.giveBackTo === null || container?.contains(holder.giveBackTo) === true) {
          holder.giveBackTo = giveBackTo;
        }
        return;
      }
      if (giveBackTo !== null && document.contains(giveBackTo)) {
        giveBackTo.focus();
      } else {
        document.body.focus({ preventScroll: true });
      }
    };
  }, [open, initialFocusRef]);

  return { containerRef };
}

/** Tracks a media query so mobile-only dialog behaviour does not activate on desktop. */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);

  useEffect(() => {
    const list = window.matchMedia(query);
    const update = () => setMatches(list.matches);
    update();
    list.addEventListener("change", update);
    return () => list.removeEventListener("change", update);
  }, [query]);

  return matches;
}

/** Closes an open overlay when the route changes under the reader. A push or a pop is the reader
 *  moving and closes it; the mount render does not. A `replace` is two different things, which
 *  the navigation type alone cannot tell apart, so the rule turns on the address. A replace that
 *  moves the address is the page correcting itself under a reader who has not moved - a route
 *  settling in place, a filter or a selection writing itself into the address - and leaves the
 *  overlay open; closing on a key change alone shut the command bar a reader opened over a
 *  project page still redirecting its bare path (dispatch://INFRA-2452). A replace that leaves
 *  the address unchanged is the reader moving: React Router makes a `<Link>` click a replace
 *  whenever the link's target equals the address already shown, as the Inbox drawer's owner link
 *  to the issue being read is, and that click closes the overlay. Key and address are both
 *  recorded on every run, so the next navigation is compared against the location the reader is
 *  on. */
export function useCloseOnNavigation(open: boolean, onClose: () => void): void {
  const lastLocationKey = useRef<string | undefined>(undefined);
  const lastAddress = useRef<string | undefined>(undefined);
  const location = useLocation();
  const navigationType = useNavigationType();
  useEffect(() => {
    const address = `${location.pathname}${location.search}${location.hash}`;
    const changed =
      lastLocationKey.current !== undefined && lastLocationKey.current !== location.key;
    const moved = lastAddress.current !== address;
    lastLocationKey.current = location.key;
    lastAddress.current = address;
    if (open && changed && !(navigationType === NavigationType.Replace && moved)) {
      onClose();
    }
  }, [
    location.hash,
    location.key,
    location.pathname,
    location.search,
    navigationType,
    onClose,
    open,
  ]);
}
