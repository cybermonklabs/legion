import { hashKey, type MutationKey } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useState } from "react";

import type { ArtifactOwner } from "../../api/client";
import type { Ask } from "../../api/types";
import { linkHoverText, linkText } from "../../theme/classes";
import type { DwellRef } from "../shell/useDwell";
import { onQuote, type QuoteRef } from "../threads/quote-reply";
import { composerFirst } from "../threads/thread-layout";
import type { ThreadOrder } from "../threads/thread-order";
import {
  AskReplyComposer,
  AskThread,
  type AskThreadQuery,
  type CreateReply,
  takesReply,
  UnsentReply,
  useUnsentReply,
} from "./AskThread";

interface AskThreadDisclosureProps {
  ask: Ask;
  thread: AskThreadQuery;
  createReply?: CreateReply;
  embedded?: boolean;
  /** Where a file pasted or dropped into the reply goes (`AskReplyComposer`). */
  uploadOwner?: ArtifactOwner;
  /** The shared thread read mark for the ask (`useMarkThreadRead`), placed on its newest reply
   *  (`AskThread`). */
  markNewest?: DwellRef;
  /** The reply a deep link named (`AskThread`); only the Conversation passes one. */
  currentReplyId?: string;
  /** The order the thread reads in (`AskThread`), which also places this control and the
   *  composer it reveals: under the question in Newest first, at the foot in Oldest first. */
  order: ThreadOrder;
  /** An answered or resolved ask's thread as a text of Quote reply (`AskThread`): a quote handed
   *  to its reply box opens the box. */
  quote?: { readonly key: MutationKey; readonly ref: QuoteRef };
  /** The ids of the replies the reader has not read (`AskThread`). */
  unread?: ReadonlySet<string>;
}

export function AskThreadDisclosure({
  ask,
  thread,
  createReply,
  embedded = false,
  uploadOwner,
  markNewest,
  currentReplyId,
  order,
  quote,
  unread,
}: AskThreadDisclosureProps): ReactNode {
  const [composerOpen, setComposerOpen] = useState(false);
  const composerId = useId();
  // A Quote reply opens Write a reply; the box takes the words as it mounts (`AskReplyComposer`).
  const quoteName = quote === undefined ? undefined : hashKey(quote.key);
  useEffect(
    () => (quoteName === undefined ? undefined : onQuote(quoteName, () => setComposerOpen(true))),
    [quoteName]
  );
  const unsentReply = useUnsentReply(ask);
  if (thread.isError) {
    const message =
      thread.error instanceof Error ? thread.error.message : "Unable to load replies.";
    return (
      <button
        aria-expanded="false"
        className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={() => {
          void thread.refetch();
        }}
        title={message}
        type="button"
      >
        Replies unavailable — retry
      </button>
    );
  }

  const replyCount = (thread.data?.replies ?? []).length;
  if (replyCount === 0 && !takesReply(ask) && unsentReply === "") return null;

  // The composer follows the control that reveals it, so the next Tab from the control lands in
  // the field and a screen reader meets the expanded panel right after its button. The control is
  // "Write a reply" so a screen reader can tell it from the form's own "Reply" submit button.
  // The pair sits where the order puts a reply box: next to the newest message.
  const threadNode =
    replyCount === 0 ? null : (
      <AskThread
        ask={ask}
        currentReplyId={currentReplyId}
        embedded={embedded}
        markNewest={markNewest}
        order={order}
        quote={quote}
        showComposer={false}
        thread={thread}
        unread={unread}
      />
    );
  // An open ask takes no reply composer: a reply the reader typed before it opened again stands
  // in the composer's place, read-only (`useUnsentReply`).
  const composer = !takesReply(ask) ? (
    unsentReply === "" ? null : (
      <UnsentReply className="mt-3" text={unsentReply} />
    )
  ) : (
    <>
      <button
        aria-controls={composerId}
        aria-expanded={composerOpen}
        className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={() => setComposerOpen((open) => !open)}
        type="button"
      >
        Write a reply
      </button>
      {composerOpen ? (
        <div className="mt-2" id={composerId}>
          <AskReplyComposer
            ask={ask}
            createReply={createReply}
            quoteKey={quote?.key}
            uploadOwner={uploadOwner}
          />
        </div>
      ) : null}
    </>
  );

  return composerFirst(order) ? (
    <>
      {composer}
      {threadNode}
    </>
  ) : (
    <>
      {threadNode}
      {composer}
    </>
  );
}
