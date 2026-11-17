import type { ReactNode } from "react";

import type { Actor } from "../../api/types";
import { AgentName } from "../refs/AgentName";
import type { ReplyTarget } from "./composer-model";
import { addressWords } from "./send-request";
import type { AwayDraft } from "./unsent-drafts";

/** The sentence said of a reply the reader wrote and that is kept out of sight - by a composer of
 *  a draft it keeps for another reply, by the docked composer of a thread reply put away over its
 *  own draft, and by a closed thread card of its reply: whose comment or message it answers, by
 *  its author, and the control that goes on with it, that comment's or message's Reply unless the
 *  host names the control beside the sentence (`press`). */
export function KeptReply({
  actor,
  parentKind = "comment",
  press,
}: {
  readonly actor: Actor;
  readonly parentKind?: ReplyTarget["parentKind"];
  readonly press?: string;
}): ReactNode {
  const parent = parentKind === "message" ? "message" : "comment";
  return (
    <>
      Your reply to the {parent} by <AgentName actor={actor} inline /> is kept. Press{" "}
      {press ?? `Reply on that ${parent}`} to go on with it.
    </>
  );
}

/** What the composer says of a draft it keeps for another address than the one it shows
 *  (`AwayDraft`): whose message or comment that draft answers, or that it answers none, and the
 *  way back to it - that message's Reply, or Cancel reply. Nothing is sent until the reader
 *  presses. */
export function keptAwayLine({ owner, replyTo }: AwayDraft): ReactNode {
  if (replyTo === null) {
    return `The message you wrote ${addressWords({ owner, replyTo }, "for")} is kept. Press Cancel reply to go on with it.`;
  }
  return <KeptReply actor={replyTo.actor} parentKind={replyTo.parentKind} />;
}
