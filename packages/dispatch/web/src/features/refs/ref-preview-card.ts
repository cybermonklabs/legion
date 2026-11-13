// How a surface recognises the one hover card and the reference it belongs to, in a module that
// imports nothing, so a list judging a pointer does not pull the card's own module in with it.
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

/** The attribute a surface carries whose references open no card on hover: the issue page's
 *  frame (dispatch://INFRA-1196 R6, `features/shell/page-frame.tsx`), whose reader works it from
 *  the keyboard. A reference there still opens its card when the keyboard focuses it. */
export const NO_HOVER_PREVIEW_ATTRIBUTE = "data-no-hover-preview";

/** Whether `trigger` sits on a surface whose references open no card on hover. */
export function hoverPreviewsOff(trigger: Element): boolean {
  return trigger.closest(`[${NO_HOVER_PREVIEW_ATTRIBUTE}]`) !== null;
}
