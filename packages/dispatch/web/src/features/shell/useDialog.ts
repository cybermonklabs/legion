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

/** The mark of an element a dialog gives the focus back to as it closes where the control that
 *  held the focus as it opened has gone since: a row of the new issue page, whose acts are drawn
 *  only while the row holds the focus, so a dialog opened from an act takes the act away as it
 *  takes the focus (decision 396). No element of today's pages carries it. */
export const DIALOG_RETURN_ATTRIBUTE = "data-dialog-return";

/** Where an open dialog gives the focus back when it closes, and whether it made its own element
 *  focusable to take the focus as it opened. */
interface FocusRecord {
  giveBackTo: HTMLElement | null;
  madeFocusable: boolean;
  /** The nearest element around the control that held the focus as the dialog opened that
   *  carries `DIALOG_RETURN_ATTRIBUTE`, which takes the focus back at close where that control
   *  has gone by then; undefined where none does. */
  returnTo?: HTMLElement | null;
}

// Every open dialog's focus record, by its element. A dialog's layout effect adds its own entry
// in the commit that opens it, and its layout cleanup deletes it in the commit that closes it;
// a commit's layout cleanups run before its passive cleanups, so a closing dialog never finds
// itself here. A record's `giveBackTo` is written as its dialog opens, and by other dialogs only
// through three functions below, each in one phase: `takeOverTargetOfInner` (layout),
// `giveBackToField` (frame) and `handOverFocus` (passive cleanup). `ownField` reads the map.
const focusRecords = new Map<HTMLElement, FocusRecord>();

/** Layout phase, by a dialog (`container`, its `record`) opening with the focus already inside
 *  its element, before it adds its own record to the map, so every record found here is
 *  another dialog's. When the focus is in an open dialog inside `container` whose target is
 *  outside `container` - a dialog in a child component that opened in the same commit, whose
 *  layout effect ran first and took the focus - `record` takes that target over, and that dialog
 *  gives the focus back to `container` instead. Returns whether it did. The first such dialog is
 *  enough: in a chain of dialogs opened in one commit, each has already taken over the target of
 *  the one inside it, so only the outermost of them still has a target outside `container`. */
function takeOverTargetOfInner(
  container: HTMLElement,
  active: Element | null,
  record: FocusRecord
): boolean {
  for (const [inner, innerRecord] of focusRecords) {
    const target = innerRecord.giveBackTo;
    const targetOutside = target === null || !container.contains(target);
    if (container.contains(inner) && inner.contains(active) && targetOutside) {
      record.giveBackTo = target;
      innerRecord.giveBackTo = container;
      return true;
    }
  }
  return false;
}

/** The first control of an open dialog's element that no other open dialog inside it holds: the
 *  dialog's own field, which a dialog inside it gives the focus back to. */
function ownField(container: HTMLElement): HTMLElement | undefined {
  const inner = [...focusRecords.keys()].filter(
    (other) => other !== container && container.contains(other)
  );
  return focusableIn(container).find((control) => !inner.some((other) => other.contains(control)));
}

/** Frame phase, by a dialog whose frame found the focus inside its element and not on it, which
 *  is where a dialog opened inside it holds the focus: every record that gives the focus back to
 *  `container` gives it back to `field` instead. */
function giveBackToField(container: HTMLElement, field: HTMLElement): void {
  for (const record of focusRecords.values()) {
    if (record.giveBackTo === container) {
      record.giveBackTo = field;
    }
  }
}

/** Passive cleanup phase, by a closing dialog: whether an open dialog outside the closing one's
 *  element `container` holds the focus. That is a dialog that opened in the commit that closes
 *  this one, or any older open dialog whose control the reader pressed to close this one; it
 *  keeps the focus. Every open dialog whose element holds the focus (each of a nested chain)
 *  takes `giveBackTo`, this dialog's target, when its own is none or is inside `container`: the
 *  commit removed the control that held the focus as it opened, or it was a control of the
 *  closing dialog. A focus still inside `container` has no holder: a dialog that stays mounted
 *  when it closes (the margin sheet's `aside`, the conversation's thread section) still holds the
 *  focus at its cleanup, and an open dialog around it is no holder of that focus. */
function handOverFocus(container: HTMLElement | null, giveBackTo: HTMLElement | null): boolean {
  const active = document.activeElement;
  if (container?.contains(active) === true) {
    return false;
  }
  let held = false;
  for (const [other, record] of focusRecords) {
    if (!other.contains(active)) {
      continue;
    }
    held = true;
    if (record.giveBackTo === null || container?.contains(record.giveBackTo) === true) {
      record.giveBackTo = giveBackTo;
    }
  }
  return held;
}

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
 * picker all use this). On open the focus moves at once to the dialog itself, unless it is
 * already inside the dialog, and to its field a frame later, so no key typed in between reaches a
 * control outside the dialog. While open it pushes the
 * `dialog` keymap scope, which masks every page and global binding, and registers Escape there
 * for the innermost open dialog, whatever element holds focus; a descendant that owns its Escape
 * (the margin Composer dismissing its picker) stops propagation before the dispatcher sees it.
 * Tab trapping listens on the container.
 * At close the focus goes back to the element that held it at open; where that element has gone,
 * to the nearest element around it marked `DIALOG_RETURN_ATTRIBUTE` while that one is on the
 * page; else to the body.
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

  // This dialog's record, set by its layout effect in the commit that opens it. It is read through
  // this ref because the dialog may have no element (no entry in `focusRecords`), and because the
  // passive cleanup runs after the layout cleanup has deleted the entry.
  const focusRecord = useRef<FocusRecord | null>(null);

  // The focus moves to the dialog itself at once, in the commit that opens it, so a key typed
  // before its field takes the focus reaches nothing behind it: a Space on the control that
  // opened it would press that control again (Approve, behind Request changes, would approve the
  // document). A focus already inside the dialog's element stays where it is, for example on an
  // element with React's `autoFocus` attribute, which took it as it mounted. The element is made
  // focusable (`tabindex="-1"`, out of the Tab order) when it takes the focus here, or when a
  // dialog inside it is to give the focus back to it (`takeOverTargetOfInner`), unless it already
  // is. A layout effect, so this holds whatever update opened the dialog.
  useLayoutEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    const active = document.activeElement;
    // The focus goes back where it was, unless that was the body: a commit that removed the
    // control that held the focus as it opened this dialog leaves it there.
    const record: FocusRecord = {
      giveBackTo: active instanceof HTMLElement && active !== document.body ? active : null,
      madeFocusable: false,
      // The nearest element around the control that held the focus that carries the return
      // mark, which takes the focus back at close where that control has gone by then.
      returnTo:
        active instanceof HTMLElement
          ? (active.parentElement?.closest<HTMLElement>(`[${DIALOG_RETURN_ATTRIBUTE}]`) ?? null)
          : null,
    };
    focusRecord.current = record;
    if (container === null) {
      return;
    }
    const makeFocusable = () => {
      record.madeFocusable = !container.hasAttribute("tabindex");
      if (record.madeFocusable) {
        container.tabIndex = -1;
      }
    };
    if (!container.contains(active)) {
      makeFocusable();
      container.focus({ preventScroll: true });
    } else if (takeOverTargetOfInner(container, active, record)) {
      // The dialog inside now gives the focus back to this element until this dialog's frame
      // points it at this dialog's field, so the element must take the focus until then: a
      // dialog inside that closes before that frame gives the focus to this element.
      makeFocusable();
    }
    focusRecords.set(container, record);
    return () => {
      focusRecords.delete(container);
    };
  }, [open]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    const record = focusRecord.current;
    scrollLock.acquire();
    const stopBeingFocusable = () => {
      if (container !== null && record?.madeFocusable === true) {
        container.removeAttribute("tabindex");
        record.madeFocusable = false;
      }
    };

    // The dialog's field takes the focus a frame later, so a focus a descendant takes in its own
    // effect lands first and is not taken back: the margin sheet's composer focuses its text box
    // in an effect once it is on screen (`MentionComposer`'s `autoFocus` prop). Only a focus still
    // on the dialog itself, or gone from it, is moved. A focus elsewhere inside the element stays,
    // and every record that gives the focus back to the element gives it back to the field instead
    // (`giveBackToField`). The field is the dialog's own: a control inside an open dialog inside
    // this one is not it (`ownField`). The element then stops being focusable, unless the focus is
    // on it, so a press on the dialog's text puts the focus on the page's body, as before this hook
    // took the focus at once.
    const initialFocusFrame = window.requestAnimationFrame(() => {
      if (container === null) {
        return;
      }
      const active = document.activeElement;
      const field = initialFocusRef?.current ?? ownField(container);
      if (field === undefined) {
        // No field of its own: the element keeps the focus it has, stays focusable, and stays
        // the target a dialog inside it gives the focus back to.
        return;
      }
      if (active === container || !container.contains(active)) {
        field.focus();
      } else {
        giveBackToField(container, field);
      }
      if (document.activeElement !== container) {
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
      const giveBackTo = record?.giveBackTo ?? null;
      if (handOverFocus(container, giveBackTo)) {
        return;
      }
      const returnTo = record?.returnTo ?? null;
      if (
        giveBackTo !== null &&
        !document.contains(giveBackTo) &&
        returnTo !== null &&
        document.contains(returnTo)
      ) {
        returnTo.focus();
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
