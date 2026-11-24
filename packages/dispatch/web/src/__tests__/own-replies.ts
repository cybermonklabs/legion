import { within } from "@testing-library/react";

/** The Reply buttons `turn` draws for its own post, and not for a reply drawn inside it: the
 *  Conversation's and the Agents page's turns nest their replies' turns (`data-turn`) in their
 *  own. Undefined where no turn was found. */
export function ownReplies(turn: Element | null): number | undefined {
  if (!(turn instanceof HTMLElement)) return undefined;
  return within(turn)
    .queryAllByRole("button", { name: "Reply" })
    .filter((button) => button.closest("[data-turn]") === turn).length;
}
