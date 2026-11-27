import type { DeliveryCapability } from "../../api/types";

/**
 * The words a send says of itself - the command that picks its mode, where it goes, and what a
 * stopped one may have done - in a module of their own, apart from the rest of the composer's
 * words (`composer-words.ts`). A send is made in the entry chunk (`send-request.ts`, which the
 * held-send store in the entry imports), and the entry is held to its budget
 * (`web/src/asset-sizes-in-build.ts`): through `composer-words.ts` it would carry every sentence
 * the composer says too, which only the pages' chunks draw. So the entry imports these alone.
 * Like `composer-words.ts`, this module imports nothing but types, so the issue page's list of
 * words reads them where no component can load (`issue-page/page-words-l.ts`), and so do the
 * composers (`MentionComposer.tsx`).
 */

/** The command, at the start of a body, that sends it in a mode other than the plain one (Send,
 *  unless the session refuses Send: `plainDelivery`). `deliveryPlan` strips it from what goes out,
 *  and the composer names it where it tells the reader how to send in another mode or what a bare
 *  command still needs. */
export const DELIVERY_COMMANDS = { aside: "/aside", btw: "/btw" } as const satisfies Record<
  Exclude<DeliveryCapability, "steer">,
  string
>;

/** The words `ownerWords` and `addressWords` name where a message goes in: the prepositions an
 *  issue or another document is said with, and the words of a direct message, of another
 *  document and of a reply. */
export const ADDRESS_WORDS = {
  direct: "as a direct message",
  document: "another document",
  prepositions: ["for", "to"],
  reply: "as a reply",
} as const;

/** The words of `movedSendNote`: what the stopped write is, before its address; what it may have
 *  done there; and what a send from here does. */
export const MOVED_SEND_WORDS = {
  anew: "Sending it from here is a new message, which can be delivered a second time if the first one was.",
  made: "delivered there",
  what: "this message, sent ",
} as const;
