/**
 * Where an ask card's own keys end and a page's begin.
 *
 * A card's options are radios and checkboxes, and its actions are buttons - none of which takes
 * typed text, so the keyboard registry passes single keys straight through to whatever scope the
 * page has pushed (`shell/keymap.ts`'s editable policy). That permission carries an obligation:
 * a page whose key writes something must say for itself where the reader may press it, and the
 * answer on every surface that renders an `AskCard` is the same - not inside the card. The Inbox
 * asks it of `h`, and the issue page of `0`-`3` and `Shift+P`.
 */

/** An ask card anywhere on the page, wherever it is rendered. */
const ASK_CARD = "[data-ask-card]";

/** An ask body that draws an ask to answer, anywhere on the page: `AskBody`'s root while its ask
 *  takes an answer, the one element that carries `data-ask-answering`. Dashboard draws it with no
 *  card around it, and a key pressed on its options and buttons is inside an ask all the same. A
 *  settled ask's record is no such body: `AskCompletionCard` draws it with `data-ask-body` and
 *  without this mark, outside every card (`AskCard`'s settled branch, `doc/AskBlockCard.tsx`), so
 *  the page's keys work on its Change answer and Show earlier answers, as they do on `main`. */
const ASK_ANSWERING = "[data-ask-answering]";

/** Whether the reader is outside every ask card, so a page-level key is the page's to act on. */
export function outsideAskCard(): boolean {
  // The focus on an ask's answering controls is inside an ask, whether a card stands around them
  // or not; anywhere else, only a card's own article holds the page's keys off.
  if (document.activeElement?.closest(ASK_ANSWERING) != null) return false;
  return document.activeElement?.closest(ASK_CARD) == null;
}
