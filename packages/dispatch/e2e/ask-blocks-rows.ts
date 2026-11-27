// What the rows of `ask-blocks.e2e.ts` and `ask-blocks-paste.e2e.ts` share: the ask card a
// decision block hosts, and the wait for the block to host it. A helper one of those files needs
// stays in that file.
import { expect, type Locator } from "@playwright/test";

/** The shared ask card a decision block hosts once the server has indexed the block: the same
 * `AskCard` the Inbox renders, in its compact variant. Its `data-testid` names the ask id in
 * every state (open card, answered or resolved record). */
export function hostedCard(block: Locator): Locator {
  return block.locator("article[data-testid^=ask-]");
}

/** Waits for the block to host its card: the server has indexed the block into an ask. */
export async function expectHosted(block: Locator): Promise<Locator> {
  const card = hostedCard(block);
  await expect(card).toBeVisible();
  return card;
}
