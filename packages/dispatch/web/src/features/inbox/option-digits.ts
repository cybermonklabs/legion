import { createContext, useContext } from "react";

/**
 * Which card an Inbox draws the option digits on (R17): the card of the item in hand, whose
 * options the digits pick (`inbox.option`, `useInboxKeymap.ts`). Beside the reading column that
 * is the selected question's card, which `askId` names; the column's document can hold other
 * blocks' cards, which draw none. Below the column's width, and in the drawer, it is the card
 * whose row holds the focus, itself or on one of its options, which the browser knows before
 * React does: every card draws its digits there, and only the row in hand shows them
 * (`ROW_IN_HAND_CLASS`).
 *
 * A context rather than a prop, as the hand-on's registration is (`hand-on-live.ts`): the card
 * of a question written as a decision block is the document page's own, which no prop of the
 * Inbox reaches. With no provider - the issue page's Conversation, a document's margin and its
 * decision blocks, the Answers page - a card draws no digits, since no digit picks anything there.
 */
export type OptionDigits =
  | { readonly askId: string | undefined; readonly kind: "selected" }
  | { readonly kind: "focused-row" };

export const OptionDigitsContext = createContext<OptionDigits | undefined>(undefined);

/** The digit an option row draws, for the eye alone: the key, and whether it shows only while
 *  the row it sits in is in hand. */
export interface OptionDigit {
  readonly key: string;
  readonly whileRowInHand: boolean;
}

/** Shows a digit only inside the Inbox row in hand: the row itself focused, or an option of it
 *  (`answerHand` in `useInboxKeymap.ts`). The row is the `group` (`InboxRowShell`). The digit
 *  keeps its space while hidden, so a row taking the focus moves no option's text. */
export const ROW_IN_HAND_CLASS =
  "invisible group-focus:visible group-has-[[data-ask-option]:focus]:visible";

/** The key that picks the option at `index`: `1` to `9`, then `0` for the tenth, and none
 *  past it. */
export function optionKey(index: number): string | undefined {
  if (index < 9) return String(index + 1);
  return index === 9 ? "0" : undefined;
}

/** The place among the options of the option a digit `key` picks, the other way round from
 *  `optionKey`: `1` is the first, and `0` the tenth. */
export function optionIndex(key: string): number {
  return key === "0" ? 9 : Number(key) - 1;
}

/** The digit of each option of the card of `askId`, by the option's place among the card's
 *  options with Other last; undefined for every option where the card draws none. */
export function useOptionDigits(askId: string): (index: number) => OptionDigit | undefined {
  const digits = useContext(OptionDigitsContext);
  const drawn = digits !== undefined && (digits.kind === "focused-row" || digits.askId === askId);
  return (index) => {
    const key = drawn ? optionKey(index) : undefined;
    return key === undefined ? undefined : { key, whileRowInHand: digits?.kind === "focused-row" };
  };
}
