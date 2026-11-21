import {
  type RefObject,
  useEffect,
  useInsertionEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
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
// the focus is, outside the dialog too (a press on the dialog's own text gives the focus to the
// page, and the key then lands there); only the innermost open dialog closes. Two dialogs nested
// by DOM containment (a picker opened inside an already-open sheet) resolve by containment, the
// nested one winning regardless of which mounted its effect first. Two dialogs that are siblings
// - neither containing the other, such as the phone navigation sheet and a second dialog a
// control inside it opens without closing the sheet first - have no containment relationship to
// resolve by, so the most recently opened of the two wins instead.
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

/** Whether a dialog is open. Its Escape is then its own wherever the focus is: in the dialog, or
 *  on the page after a press on its text took the focus there. A layer under it that answers
 *  Escape itself leaves the key to it. */
export function dialogOpen(): boolean {
  return openDialogs.size > 0;
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
 * picker all use this). While open it pushes the `dialog` keymap scope, which masks every page
 * and global binding, and registers Escape there for the innermost open dialog, whatever
 * element holds focus; a descendant that owns its Escape (the margin Composer dismissing its
 * picker) stops propagation before the dispatcher sees it. Tab trapping listens on the container.
 */
export function useDialog<T extends HTMLElement>({
  initialFocusRef,
  onClose,
  open,
}: UseDialogOptions): DialogHandle<T> {
  const containerRef = useRef<T>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  // What held the focus as the dialog opened: the element it was opened from. It is read in an
  // insertion effect, which React runs in the mutation phase of the commit that opens the dialog,
  // before the layout phase in which React carries out a descendant's `autoFocus` and runs every
  // layout effect - the dialog's own initial focus below among them - so it is never a focus the
  // dialog's opening moved. Closing gives the focus back to it while it is still in the
  // document, as WAI-ARIA's dialog pattern has it, and to the body otherwise.
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

  // The initial focus moves in the commit that opens the dialog, before the browser handles
  // another event: keys typed straight after the press that opened it reach the dialog, not the
  // control that opened it. Moved a frame later, it would leave them to the opener: a space typed
  // into Request changes, opened by Shift+R on the Approve button, would press Approve.
  // A descendant that focuses itself as the dialog opens keeps that focus. React runs a
  // component's layout effects after those of the components it renders, so a descendant's
  // `autoFocus` or its own layout effect has already landed inside the container by now, and is
  // left alone. One that focuses itself in a passive effect - the margin sheet's composer
  // (`MentionComposer`'s `autoFocus`) - runs after every layout effect of the commit, so its
  // focus comes last and stays.
  useLayoutEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    if (container?.contains(document.activeElement)) {
      return;
    }
    const initial =
      initialFocusRef?.current ??
      (container === null ? null : (focusableIn(container)[0] ?? container));
    initial?.focus();
  }, [open, initialFocusRef]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    const previouslyFocused = openerRef.current;
    scrollLock.acquire();
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
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    container?.addEventListener("keydown", handleTab);

    return () => {
      openDialogs.delete(container);
      container?.removeEventListener("keydown", handleTab);
      scrollLock.release();
      if (previouslyFocused !== null && document.contains(previouslyFocused)) {
        previouslyFocused.focus();
      } else {
        document.body.focus({ preventScroll: true });
      }
    };
  }, [open]);

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
