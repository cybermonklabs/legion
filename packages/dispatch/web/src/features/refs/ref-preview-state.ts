import { refPreviewCardContains } from "./ref-preview-card";
import { REF_PREVIEW_CLOSE_DELAY_MS, REF_PREVIEW_OPEN_DELAY_MS } from "./ref-preview-timing";
import { buildDispatchReference, type DispatchReferenceRoute } from "./routes";

/**
 * The one hover card's state, and the two things every surface that renders a reference needs of
 * it: the attribute that makes a link a trigger, and the close.
 *
 * Apart from `RefPreview.tsx` because the card itself - its queries, its Markdown and the
 * document-level listeners that drive it - is a chunk of its own, loaded once the shell is up
 * (`features/shell/deferred-shell.ts`), while half the pages in the app render triggers. A
 * module holding both would be in the entry chunk for the sake of one attribute, and a reader
 * opening a document would download and evaluate the card before their first block
 * (dispatch://INFRA-341, R11).
 */

/** Every hover-card trigger: a Markdown anchor `collectReferenceAnchors` tagged, a Proof link
 * mark (`data-dispatch-href`, the sanitized mark's real target), or a React link that set
 * `data-dispatch-ref` — see `referenceTriggerProps`. */
export const TRIGGER_SELECTOR = "[data-dispatch-ref],[data-dispatch-href]";

export interface PreviewTarget {
  readonly anchor: HTMLElement;
  readonly route: DispatchReferenceRoute;
}

export interface PreviewState {
  readonly phase: "pending" | "open";
  readonly target: PreviewTarget;
}

let state: PreviewState | undefined;
let openTimer = 0;
let closeTimer = 0;
/** The open delay of the request standing now ran out while the reference was not what the
 *  pointer was on, so the card was not drawn and the request was not dropped either: the page
 *  had moved the reference out from under a still pointer, or drawn something over it. The
 *  reader's next movement onto the reference is what opens it then (`startTheDelayAgain`). */
let theDelayRanOut = false;
const subscribers = new Set<() => void>();

/** What the card's host renders, and what every precondition below is judged against. */
export function previewState(): PreviewState | undefined {
  return state;
}

/** A new target, or none, cancels both timers and the delay a request of the old target ran
 * out, so a timer's callback always acts on the target that armed it and no transition has to
 * clear them for a target change. The comparison is by object identity, and `hoverStart` and
 * `focus` mint a fresh `PreviewTarget` on every call, so keyboard focus landing on the very
 * anchor the pointer just left still cancels that anchor's close. A transition that re-arms a
 * timer on the target already shown clears it itself, which is `leave` alone. */
function setState(next: PreviewState | undefined): void {
  if (next?.target !== state?.target) {
    window.clearTimeout(openTimer);
    window.clearTimeout(closeTimer);
    theDelayRanOut = false;
  }
  state = next;
  for (const notify of subscribers) {
    notify();
  }
}

/** Arms the open delay of `target`: the card is drawn when it runs out, and where the
 *  reference is not what the pointer is on then (`thePointerIsOnIt`) the request stands on
 *  instead, pending and undropped, for the reader's next movement to decide
 *  (`startTheDelayAgain`). */
function armTheOpenDelay(
  target: PreviewTarget,
  thePointerIsOnIt: (anchor: HTMLElement) => boolean
): void {
  window.clearTimeout(openTimer);
  openTimer = window.setTimeout(() => {
    if (thePointerIsOnIt(target.anchor)) {
      setState({ phase: "open", target });
      return;
    }
    theDelayRanOut = true;
  }, REF_PREVIEW_OPEN_DELAY_MS);
}

/** The one hover card's controller. Module-level on purpose: "one card at a time" is a single
 * piece of state, and `RefPreviewHost` drives it from document-level listeners. Each transition
 * has a precondition its one caller checks - `hoverStart`, `leave` and `reenter` the hover zone
 * (`attachTriggers` alone judges that), `focus` `:focus-visible`, `blur` that focus is not moving
 * into the card, and the scroll handler's `reposition` that the card's anchor holds focus - so
 * all of them are the host's, and `closeRefPreview` below is the whole outside surface. */
export const refPreview = {
  /** Hover began on an anchor outside the hover zone: open its card after the delay unless the
   * pointer leaves first.
   *
   * `thePointerIsOnIt` is asked at the moment the delay runs out, and only the host can answer
   * it: whether the reference is still what the pointer is over is a question about the laid
   * out page - the anchor's box now, and what `elementFromPoint` finds at the place the host
   * last saw the pointer - and this module holds no pointer and reads no layout. A card drawn
   * over a dialog that opened across the reference, or beside a reference the page has slid
   * away from under the reader, previews a link that is not under their pointer, so the answer
   * no leaves the request standing: pending, undropped, for their next movement to decide. */
  hoverStart(
    anchor: HTMLElement,
    route: DispatchReferenceRoute,
    thePointerIsOnIt: (anchor: HTMLElement) => boolean
  ): void {
    const target: PreviewTarget = { anchor, route };
    setState({ phase: "pending", target });
    armTheOpenDelay(target, thePointerIsOnIt);
  },
  /** The reader moved the pointer onto the reference of a request whose delay had already run
   * out over something else - the page had slid it away from under them, or drawn a dialog
   * across it. A pointer arriving on a reference from over something else is the case the
   * delay exists for, a reader passing through, so the request waits a whole delay again from
   * that movement rather than opening on it.
   *
   * Whether the reference is what the pointer is on now is the question `hoverStart` asks and
   * the host answers, and it is asked here rather than before the call so that no movement
   * reads the page's layout unless a request is waiting on one. */
  startTheDelayAgain(thePointerIsOnIt: (anchor: HTMLElement) => boolean): void {
    if (state?.phase === "pending" && theDelayRanOut && thePointerIsOnIt(state.target.anchor)) {
      theDelayRanOut = false;
      armTheOpenDelay(state.target, thePointerIsOnIt);
    }
  },
  /** The pointer left the hover zone: a card that has not opened yet is abandoned, and an open
   * one closes after the bridge delay unless the pointer comes back. */
  leave(): void {
    if (state === undefined) {
      return;
    }
    if (state.phase === "pending") {
      setState(undefined);
      return;
    }
    // Re-arming on the target already shown, which `setState` does not clear for: two pointers
    // leaving the zone within the delay must leave one close timer, not two, or the first one
    // closes the card under the pointer that came back.
    window.clearTimeout(closeTimer);
    closeTimer = window.setTimeout(() => setState(undefined), REF_PREVIEW_CLOSE_DELAY_MS);
  },
  /** The pointer came back into the hover zone before the bridge delay ran out. */
  reenter(): void {
    window.clearTimeout(closeTimer);
  },
  /** Keyboard focus opens the card at once. */
  focus(anchor: HTMLElement, route: DispatchReferenceRoute): void {
    setState({ phase: "open", target: { anchor, route } });
  },
  blur(anchor: HTMLElement): void {
    if (state?.target.anchor === anchor) {
      setState(undefined);
    }
  },
  /** The anchor moved under a reader who did not - the page scrolled under a keyboard-opened
   *  card, a late draw slid the reference out from under a resting pointer: measure it again.
   *  The target is kept, so a pending hover's open delay and an open card's armed close both
   *  run on rather than starting over. */
  reposition(): void {
    if (state !== undefined) {
      setState({ ...state });
    }
  },
};

/** Close the hover card. The one operation with no precondition, so it is the one every other
 * surface may call: a surface that takes the pointer over (the board's drag lift) closes the
 * card rather than driving the hover transitions itself. */
export function closeRefPreview(): void {
  setState(undefined);
}

export function subscribeToPreview(notify: () => void): () => void {
  subscribers.add(notify);
  return () => {
    subscribers.delete(notify);
  };
}

/** The attribute a React-rendered link spreads to become a hover-card trigger; the Markdown and
 * Proof surfaces carry the same attributes on the anchors they render themselves. */
export function referenceTriggerProps(route: DispatchReferenceRoute): {
  "data-dispatch-ref": string;
} {
  return { "data-dispatch-ref": buildDispatchReference(route) };
}

export function triggerOf(node: EventTarget | null): HTMLElement | null {
  return node instanceof Element ? node.closest<HTMLElement>(TRIGGER_SELECTOR) : null;
}

/** The one region the pointer may roam without closing the card: the current target's anchor -
 * the open card's, or the one a pending hover is waiting on - and the card itself. Every pointer
 * transition is judged against this predicate, in the document listeners alone, so no close
 * depends on the order in which two event systems see one `pointerout`. */
export function inHoverZone(node: EventTarget | null): boolean {
  return (
    state !== undefined && (triggerOf(node) === state.target.anchor || refPreviewCardContains(node))
  );
}
