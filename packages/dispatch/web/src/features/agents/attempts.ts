import type { MutationKey } from "@tanstack/react-query";

import type { MessageDelivery } from "../../api/types";
import { deliveryMode, deliveryOutcomeState, rowAnsweredWithError } from "../conversation/delivery";
import type { TargetedMessageAttempt } from "../conversation/TargetedMessageCard";
import type { AgentPart } from "../refs/AgentName";

/** The name a retry of message `messageId`'s delivery goes under (`useKeyedWrite`'s
 *  `mutationKey`), on every page that offers one - an agent's conversation on the Agents page, a
 *  broadcast's recipients, an issue's Conversation - so whichever of them draws the message while
 *  a retry of it is out, before it lands, holds its Retry and its mode changes from that press
 *  until the retry's deadline, as the page it was made on did (`usePendingKeyedPress`). */
export function messageDeliveryRetryKey(messageId: string): MutationKey {
  return ["message-delivery-retry", messageId];
}

/** A message's delivery attempts as `TargetedMessageCard` shows them. Each names the session it
 *  reached (an agent part, drawn by `AgentName`), so an earlier attempt keeps naming that session
 *  after a role hands off; an attempt that recorded none names `targetName`. Shared by an agent
 *  card's conversation and by a broadcast's recipient list, so a delivery reads the same
 *  wherever it is shown. */
export function deliveryAttempts(
  deliveries: readonly MessageDelivery[],
  targetName: AgentPart
): TargetedMessageAttempt[] {
  return deliveries.map((attempt) => ({
    // The route table carries `accepted_as` as a plain string; `user_turn` is the one word that
    // says the session took the message as its user's own turn, and no other reads as one.
    acceptedAs: attempt.accepted_as === "user_turn" ? "user_turn" : null,
    attempt: attempt.attempt,
    answeredWithError: rowAnsweredWithError(attempt),
    createdAt: attempt.created_at,
    // A mode the route table leaves a plain string, narrowed to the closed list the picker
    // offers; Dispatch writes no other, and the card's own default for a missing mode is Send.
    delivery: deliveryMode(attempt.delivery) ?? "steer",
    duplicate: attempt.duplicate,
    error: attempt.error,
    state: deliveryOutcomeState(attempt.state),
    targetName:
      attempt.session_id === ""
        ? targetName
        : { sessionId: attempt.session_id, storedTitle: undefined },
  }));
}
