// How a surface recognises the one hover card and the reference it belongs to, in a module that
// imports nothing, so a list judging a pointer does not pull the card's own module in with it.
// The reference-preview layer in the entry (`RefPreviewLayer.tsx`) finds the references a reader
// asks a card for here too, for the same reason. The rule of each gesture that asks for a card is
// here once (`pointerAsksForCard`, `focusAsksForCard`, `caretRefPreviewTrigger`): the card's own
// listener (`attachTriggers` in `RefPreview.tsx`) and the failed layer's (`CardAsked` in
// `RefPreviewLayer.tsx`) both read it, so a change of a rule changes both.
// The card itself (`RefPreview`) is `fixed` and portaled into `document.body`, outside every
// list that opened it, and the DOM is the whole tie: the card carries the id below while it is
// shown, and its reference carries that same id as its `aria-describedby`.

/** The id the one hover card carries, and the `aria-describedby` of the reference whose card it
 *  is, for as long as it is open (`RefPreview`). */
export const REF_PREVIEW_CARD_ID = "ref-preview";

/** Whether `node` is the hover card or something inside it. */
export function refPreviewCardContains(node: EventTarget | null): boolean {
  return node instanceof Element && node.closest(`#${REF_PREVIEW_CARD_ID}`) !== null;
}

/** Whether the open card is the doing of a reference inside `root`: a pointer that moved from
 *  `root` onto that card has not left what `root` draws, however far outside it the card sits. */
export function refPreviewOpenedIn(root: Element): boolean {
  return root.querySelector(`[aria-describedby~="${REF_PREVIEW_CARD_ID}"]`) !== null;
}

/** The attribute a surface carries whose references open no card on hover: the new issue page's
 *  frame (R6) - `<main>` in it (`app.tsx`) and Details beside it
 *  (`features/margin/Margin.tsx`) - whose reader works it from the keyboard. A reference there
 *  still opens its card when the keyboard focuses it. */
export const NO_HOVER_PREVIEW_ATTRIBUTE = "data-no-hover-preview";

/** Whether `trigger` sits on a surface whose references open no card on hover. */
export function hoverPreviewsOff(trigger: Element): boolean {
  return trigger.closest(`[${NO_HOVER_PREVIEW_ATTRIBUTE}]`) !== null;
}

/** Every hover-card trigger: a Markdown anchor `collectReferenceAnchors` tagged, a Proof link
 *  mark (`data-dispatch-href`, the sanitized mark's real target), or a React link that set
 *  `data-dispatch-ref` — see `referenceTriggerProps` (`reference-trigger.ts`). */
export const REF_PREVIEW_TRIGGER_SELECTOR = "[data-dispatch-ref],[data-dispatch-href]";

/** The hover-card trigger `node` is in, if any. A text node is read as the element it is in. */
export function refPreviewTriggerOf(node: EventTarget | null | undefined): HTMLElement | null {
  const element = node instanceof Node && !(node instanceof Element) ? node.parentElement : node;
  return element instanceof Element
    ? element.closest<HTMLElement>(REF_PREVIEW_TRIGGER_SELECTOR)
    : null;
}

/** The reference that a `pointerover` asks a card for, or `null`. The pointer asks when it comes
 *  over a reference from outside that reference, with no button down, on a surface that opens a
 *  card on hover. A touch asks for no card: a tap follows the link. A held button asks for no
 *  card: a drag-selection that passes over a link is not a hover. */
export function pointerAsksForCard(event: PointerEvent): HTMLElement | null {
  const reference = refPreviewTriggerOf(event.target);
  return reference !== null &&
    reference !== refPreviewTriggerOf(event.relatedTarget) &&
    event.pointerType !== "touch" &&
    event.buttons === 0 &&
    !hoverPreviewsOff(reference)
    ? reference
    : null;
}

/** The reference that a focus on `target` asks a card for, or `null`: a reference that
 *  `:focus-visible` says the keyboard focused. That is the browser's own verdict of keyboard
 *  against pointer: a mouse click or a tap focuses a link without matching it. */
export function focusAsksForCard(target: EventTarget | null): HTMLElement | null {
  const reference = refPreviewTriggerOf(target);
  return reference?.matches(":focus-visible") ? reference : null;
}

/** The link of an editable document the caret is in, on a surface that opens no card on hover:
 *  the one kind of link the caret opens a card for. A link inside an editor takes no focus of
 *  its own, so the caret entering it is the keyboard reaching it. */
export function caretRefPreviewTrigger(): HTMLElement | null {
  const link = refPreviewTriggerOf(document.getSelection()?.anchorNode);
  return link?.isContentEditable && hoverPreviewsOff(link) ? link : null;
}
