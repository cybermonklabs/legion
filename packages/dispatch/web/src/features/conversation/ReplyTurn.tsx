import type { MessageDeliveryMode } from "@legion/contracts";
import { type ReactNode, useContext } from "react";

import type { Actor, DisplayTree, ReplyBlockWithTrees } from "../../api/types";
import { secondaryButtonBorder, textSecondaryOnSurface } from "../../theme/classes";
import { AgentName, type AgentPart } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import type { DispatchReferenceRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import type { DwellRef } from "../shell/useDwell";
import { Avatar } from "./Avatar";
import { ReplyButton } from "./ReplyButton";
import { ReplyQuote } from "./ReplyQuote";
import {
  DeliveryRetry,
  DeliveryStatus,
  type TargetedMessageAttempt,
  takenAsUserTurn,
} from "./TargetedMessageCard";
import { ConversationTurns, turnFocusRing, turnMarks } from "./turn-keys";

/** A turn's action icons - copy-reference, Reply, Pin - beside its body: a row on desktop, a
 *  column on the phone so three 44 px targets do not squeeze the text to a sliver. The column
 *  is reversed so the last icon (Pin) keeps the turn's top-right corner on both layouts. */
export function TurnActions({ children }: { children: ReactNode }): ReactNode {
  return (
    <div className="flex shrink-0 flex-col-reverse items-center self-start sm:flex-row">
      {children}
    </div>
  );
}

/** The replies beneath a thread's root, in the reader's own thread order
 *  (`features/threads/thread-layout.ts`), indented under a rule so the exchange reads as one
 *  thread. */
export function ThreadReplies({ children }: { children: ReactNode }): ReactNode {
  return (
    <ol
      aria-label="Replies"
      className={`mt-2 ml-4 flex flex-col gap-1 border-l-2 pl-3 sm:ml-8 ${secondaryButtonBorder}`}
    >
      {children}
    </ol>
  );
}

interface ReplyDelivery {
  readonly answeredBy?: Actor;
  readonly attempts: readonly TargetedMessageAttempt[];
  readonly retry?: {
    readonly canAside: boolean;
    readonly canBtw: boolean;
    readonly canSteer: boolean;
    readonly onRetry: (delivery: MessageDeliveryMode) => void;
    readonly retrying: boolean;
  };
  readonly targetName: AgentPart;
}

/** One reply in a thread: author, time, the quoted parent, the body, and - when the reply was
 *  itself delivered to a session - what became of that delivery. A reply of an issue draws its
 *  Copy reference beside the Reply button (`reference`); one to a session that names no issue
 *  has none. On the Conversation tab the row is a turn its keys reach (`turn-keys.ts`). */
export function ReplyTurn({
  at,
  author,
  body,
  current = false,
  delivery,
  markRef,
  onReply,
  quote,
  reference,
  replyHeld = false,
  turnID,
}: {
  at: string;
  author: Actor;
  body: ReactNode;
  current?: boolean;
  delivery?: ReplyDelivery;
  /** The shared thread read mark (`useMarkThreadRead`), on the newest reply of a thread: the
   *  element it watches. */
  markRef?: DwellRef;
  onReply?: () => void;
  /** The parent the reply quotes: its author, its body, the block of it the reply answers (its
   *  `reply_block`), and its turn's path. */
  quote?: {
    readonly author: Actor | undefined;
    readonly block?: ReplyBlockWithTrees | null;
    readonly excerpt: string;
    readonly excerptTree?: DisplayTree;
    readonly to?: string;
  };
  /** The reply's own reference, which its Copy reference copies. */
  reference?: DispatchReferenceRoute;
  /** Holds the Reply button while the composer it would address is busy with a send. */
  replyHeld?: boolean;
  turnID: string;
}): ReactNode {
  const inTab = useContext(ConversationTurns);
  // The retry row a delivered reply keeps until the session answers it or took it as its turn.
  const retry =
    delivery !== undefined &&
    delivery.answeredBy === undefined &&
    !takenAsUserTurn(delivery.attempts)
      ? delivery.retry
      : undefined;
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`flex gap-3 rounded-lg py-1 ${inTab ? turnFocusRing : ""}`}
      data-reply=""
      data-turn={turnID}
      ref={markRef}
      {...turnMarks(inTab, reference)}
    >
      <Avatar actor={author} />
      <div className="min-w-0 flex-1">
        <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnSurface}`}>
          <AgentName actor={author} className="font-semibold" inline />
          <Timestamp at={at} />
        </p>
        {quote === undefined ? null : (
          <ReplyQuote
            author={quote.author}
            block={quote.block}
            className="mb-1"
            excerpt={quote.excerpt}
            excerptTree={quote.excerptTree}
            to={quote.to}
          />
        )}
        {body}
        {delivery === undefined ? null : (
          <>
            <DeliveryStatus
              answeredBy={delivery.answeredBy}
              deliveries={delivery.attempts}
              retryRow={retry !== undefined}
              targetName={delivery.targetName}
            />
            {retry === undefined ? null : (
              <DeliveryRetry
                canAside={retry.canAside}
                canBtw={retry.canBtw}
                canSteer={retry.canSteer}
                latest={delivery.attempts.at(-1)}
                onRetry={retry.onRetry}
                retrying={retry.retrying}
                targetName={delivery.targetName}
              />
            )}
          </>
        )}
      </div>
      <TurnActions>
        {reference === undefined ? null : <CopyRefButton route={reference} />}
        {onReply === undefined ? null : <ReplyButton held={replyHeld} onClick={onReply} />}
      </TurnActions>
    </li>
  );
}
