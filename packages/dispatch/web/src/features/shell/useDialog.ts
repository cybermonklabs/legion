import {
  type RefObject,
  useEffect,
  useInsertionEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { NavigationType, useLocation, useNavigationType } from "react-router-dom";

import { inputMethodKey } from "../../lib/input-method-key";
import { DIALOG_SCOPE, isEditableTarget, useKeymap, useKeymapScope } from "./keymap";

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

/** Whether `element` is a field the reader types text into: a `<textarea>`, an `<input>` of a
 *  type that takes text (`isEditableTarget` says which), an editable element, or one whose role
 *  says it is a text box. A dialog that names such a field (`initialFocusRef`) gives it the focus
 *  in the commit that opens it, so the keys typed straight after opening are its text. A button
 *  is none: it takes no text, and a Space or an Enter typed before the reader has seen the dialog
 *  would press it - a Confirm would confirm what the dialog asks, a Close would close the dialog
 *  unread - so the dialog's own element takes those keys, and the button the focus a frame
 *  later. Nor is a `<select>`, which `isEditableTarget` counts for its type-ahead: a key there
 *  picks an option. The element is read as it is drawn when the dialog opens, not by the type
 *  its ref was declared with. */
function takesText(element: HTMLElement): boolean {
  return (
    element.tagName !== "SELECT" &&
    (isEditableTarget(element) || element.matches("[role=searchbox],[role=textbox]"))
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
// the focus is - on its field, on the dialog itself in the first frame of one that names no text
// field or whose field took no focus as it opened, or on the page after a press on its text took
// it there; only the innermost open dialog closes.
// Two dialogs nested by DOM containment (a picker opened inside an already-open sheet) resolve by
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

/** Whether a dialog is open. Its Escape is then its own wherever the focus is: on its field, on
 *  the dialog itself in the frame before its control takes the focus, or on the page after a
 *  press on its text took the focus there. A layer under it that answers Escape itself leaves the
 *  key to it. */
export function dialogOpen(): boolean {
  return openDialogs.size > 0;
}

/**
 * Whether `event` is an Escape that closes Details, the new issue page's drawer: the drawn
 * drawer's own Escape (`features/margin/Margin.tsx`), and the shell's while Details is asked for
 * and not drawn yet (`AppShell`, `app.tsx`), which calls the open off. Both listeners read this
 * one rule, so an Escape that leaves a drawn Details open calls no waiting open off. It passes by:
 * - an Escape already handled;
 * - one that ends an input method's composition (`inputMethodKey`), which is the input method's;
 * - one in a field, inside the drawer or beside it, which keeps its own Escape to leave the field
 *   or close its picker: an editable element, a select, a textarea, and every input but the
 *   button family, a checkbox and a radio. A range, a colour and a file input are fields here,
 *   though they take no typed text (`isEditableTarget` in `keymap.ts` counts only the inputs that
 *   do);
 * - one in a dialog, or under an open dialog (`dialogOpen`) - the navigation's sheet, the list of
 *   keys - which closes on its own Escape and leaves the drawer open under it, wherever the focus
 *   is: on the dialog's field, on the dialog itself (the first frame of one that names no text
 *   field or whose field took no focus as it opened), or on the page (a press on the list's
 *   text).
 */
export function escapeClosesDrawer(event: KeyboardEvent): boolean {
  if (event.key !== "Escape" || event.defaultPrevented || inputMethodKey(event)) return false;
  const { target } = event;
  const field =
    target instanceof HTMLElement &&
    (target.isContentEditable ||
      target.tagName === "SELECT" ||
      target.tagName === "TEXTAREA" ||
      (target instanceof HTMLInputElement &&
        !["button", "checkbox", "radio", "reset", "submit"].includes(target.type)));
  return (
    !field &&
    !(target instanceof Element && target.closest('[role="dialog"]') !== null) &&
    !dialogOpen()
  );
}

/** Where an open dialog gives the focus back when it closes, and whether it made its own element
 *  focusable to take the focus as it opened. */
interface FocusRecord {
  giveBackTo: HTMLElement | null;
  madeFocusable: boolean;
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
  /** Element to focus first instead of the container's first focusable descendant: at once if it
   *  is a text field (`takesText`), else a frame after the dialog's own element took the focus. */
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
 * picker all use this). On open the focus moves at once, in the commit that opens the dialog: to
 * the text field the dialog names (`initialFocusRef`), or, for a dialog that names no text field,
 * to the dialog itself, and a frame later to the control it names or else to its first control. A
 * focus already inside the dialog stays. So no key typed straight after opening reaches a control
 * outside the dialog, nor presses a button inside it. While open it pushes the `dialog` keymap
 * scope, which masks every page and global binding, and registers Escape there for the innermost
 * open dialog, whatever element holds focus; a descendant that owns its Escape (the margin
 * Composer dismissing its picker) stops propagation before the dispatcher sees it. Tab trapping
 * listens on the container.
 */
export function useDialog<T extends HTMLElement>({
  initialFocusRef,
  onClose,
  open,
}: UseDialogOptions): DialogHandle<T> {
  const containerRef = useRef<T>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  // What held the focus before the commit that opens the dialog: the element it was opened from.
  // It is read in an insertion effect, which React runs in the mutation phase of that commit,
  // before the layout phase in which React carries out a descendant's `autoFocus` and runs every
  // layout effect - the dialog's own initial focus below among them - so it is never a focus the
  // dialog's opening moved. The layout effect below gives the focus back to it when the focus is
  // already inside the dialog's element as that effect runs: a field of the dialog that focused
  // itself by `autoFocus` holds it then, and closing the dialog unmounts that field, so closing
  // gives the focus back to the opener, as WAI-ARIA's dialog pattern has it, not to the body.
  const openerRef = useRef<HTMLElement | null>(null);
  useInsertionEffect(() => {
    if (open) {
      openerRef.current =
        document.activeElement instanceof HTMLElement ? document.activeElement : null;
    }
  }, [open]);
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

  // The focus moves in the commit that opens the dialog, before the browser handles another
  // event, so no key typed straight after the press that opened it reaches the control that
  // opened it: a Space on Approve, behind Request changes that Shift+R opens from it, would
  // approve the document (in CI a space typed 32 ms after Shift+R did). Where it moves turns on
  // what the dialog names as the control it opens on (`initialFocusRef`):
  // - A named text field (`takesText`) takes the focus at once. It is the control the dialog is
  //   for the reader to type into first - the search bar's box, the list of keys' filter, Create
  //   issue's title, Request changes' reason - so the keys typed straight after opening are text
  //   for it, and land in it.
  // - A dialog that names another control - a Close, a Confirm - or names none takes the focus
  //   on its own element at once, and gives it to that control, or to its first control, a frame
  //   later (the passive effect below). Such a control is a button as often as not, which a Space
  //   or an Enter typed before the reader has seen the dialog would press: a Confirm would confirm
  //   what the dialog asks, a Close would close it unread. The element takes such a key and does
  //   nothing with it. So does a dialog whose named field takes no focus as it opens (not drawn
  //   yet, or disabled).
  // The element is made focusable (`tabindex="-1"`, out of the Tab order) when it takes the focus
  // here, or when a dialog inside it is to give the focus back to it (`takeOverTargetOfInner`),
  // unless it already is. A focus already inside the dialog's element stays where it is: React
  // runs a component's layout effects after those of the components it renders, so a
  // descendant's `autoFocus` or its own layout effect has landed by now, and is left alone. One
  // that focuses itself in a passive effect - the margin sheet's composer (`MentionComposer`'s
  // `autoFocus`) - runs after every layout effect of the commit, so its focus comes last and
  // stays. A layout effect, so this holds whatever update opened the dialog.
  useLayoutEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    const active = document.activeElement;
    // The focus goes back where it was before this dialog's opening moved it: where it is now, or,
    // when that is inside the dialog's element, where it was before this commit (`openerRef`).
    // Not to the body: a commit that removed the control that held the focus as it opened this
    // dialog leaves it there.
    const before = container?.contains(active) === true ? openerRef.current : active;
    const record: FocusRecord = {
      giveBackTo: before instanceof HTMLElement && before !== document.body ? before : null,
      madeFocusable: false,
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
      const named = initialFocusRef?.current;
      if (named != null && takesText(named)) {
        named.focus();
      }
      if (!container.contains(document.activeElement)) {
        makeFocusable();
        container.focus({ preventScroll: true });
      }
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
  }, [open, initialFocusRef]);

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

    // Where the dialog's element took the focus as it opened, the control the dialog names, or
    // else its first control, takes it a frame later, so a focus a descendant takes in its own
    // effect lands first and is not taken back: the margin sheet's composer focuses its text box
    // in an effect once it is on screen (`MentionComposer`'s `autoFocus` prop). Only a focus still
    // on the dialog itself, or gone from it, is moved. A focus elsewhere inside the element - a
    // named text field that took it as the dialog opened among them - stays, and every record
    // that gives the focus back to the element gives it back to the field instead
    // (`giveBackToField`). The field is the dialog's own: a control inside an open dialog inside
    // this one is not it (`ownField`). The element then stops being focusable, unless the focus
    // is on it, so a press on the dialog's text puts the focus on the page's body, as before this
    // hook took the focus at once.
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
      // The dialog itself holds the focus in the first frame of one that names no text field, or
      // whose field took no focus as it opened: Tab goes to its first control and Shift+Tab to its
      // last, not to the page behind it.
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
    // has drawn the page under it: a control that commit enables again takes the focus. The page
    // does not scroll to it: a reader who scrolled while the dialog was up stays where they
    // scrolled to, as they do when a dialog's chunk lands late (`asked-surface.tsx`).
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
      if (giveBackTo !== null && document.contains(giveBackTo)) {
        giveBackTo.focus({ preventScroll: true });
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
