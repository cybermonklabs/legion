import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

/**
 * The words a write says while it is out, late or stopped, in one module that imports nothing but
 * a constant of `@legion/contracts`, so every surface that says them - the keyed write
 * (`keyed-write.ts`), the control that stops a late write (`StopTrying.tsx`), and a list of words
 * that must load where no component can, as the issue page's list does for a browser spec's Node
 * loader - reads the one text.
 */

/** How many hours Dispatch keeps a write's key (`IDEMPOTENCY_KEY_TTL_MS`), as a Retry's words say
 *  it. */
const KEY_HOURS = IDEMPOTENCY_KEY_TTL_MS / 3_600_000;

/** The words of `sameKeyRetryNote` around what the write does once it lands, which it says
 *  twice: before the first, between the two, and after the second. */
export const SAME_KEY_RETRY_WORDS = {
  after: " twice.",
  before: "Retry asks again under the same key, so it is ",
  between: ` at most once if you retry within ${KEY_HOURS} hours of the first try. After that Dispatch no longer knows the key: a Retry is a new write and may be `,
} as const;

/** The words of `expiredRetryNote` around what the write does once it lands. */
export const EXPIRED_RETRY_WORDS = {
  after: " a second time if the first one was.",
  before: `Its key is ${KEY_HOURS} hours old or more, so Dispatch no longer holds it: a Retry is a new write, which can be `,
} as const;

/** What a composer's send that ended without Dispatch's verdict says before what its Retry does
 *  (`SendStoppedError` and the errors that extend it): Dispatch has not confirmed it, or a reload
 *  ended it before any answer; and what such a send does once it lands. */
export const STOPPED_SEND_WORDS = {
  interrupted: "Not sent: this page was closed before Dispatch confirmed it.",
  made: "delivered",
  unconfirmed: "Dispatch has not confirmed this message.",
} as const;

/** The words of `stoppedWriteNote`'s first sentence around what the write is and what it may
 *  have done. */
export const STOPPED_WRITE_WORDS = {
  after: ".",
  before: "Dispatch has not confirmed ",
  between: ": it may have been ",
} as const;

/** What a send says once it is past `SEND_DEADLINE_MS`: still the server's, as any late send is,
 *  and still being tried under its key, which Dispatch takes once. */
export const KEYED_SEND_LATE_TEXT =
  "Still sending — Dispatch has not confirmed it yet. It keeps trying, and the message is delivered once.";

/** Why Stop trying takes no press once pressed: what a screen reader reads beside it, and its
 *  title where the surface raises tooltips. */
export const STOPPING_REASON = "The attempt already sent is still out.";

/** The control's words: before the reader stops the write, and once they have. */
export const STOP_TRYING_LABEL = "Stop trying";
export const STOPPING_LABEL = "Stopping…";
