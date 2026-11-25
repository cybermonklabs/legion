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

/** What is said of a reply the reader wrote and that is kept for a comment or message its host no
 *  longer lists (`ListedReplies`), whose Reply is gone with it, or for one its author deleted
 *  (`deleted`), which keeps its place and takes no reply (`409 POST_DELETED`): the reply's text is
 *  drawn after these words with its own presses (`DraftOffer`), since no Reply goes on with it,
 *  and no word of the post it answered. Where Use it here waits for the field to be empty
 *  (`KeptDrafts.awayTakes`), the words say so (`waits`). */
export function GoneReply({
  actor,
  deleted = false,
  parentKind,
  waits = false,
}: {
  readonly actor: Actor;
  readonly deleted?: boolean;
  readonly parentKind: ReplyTarget["parentKind"];
  readonly waits?: boolean;
}): ReactNode {
  return (
    <>
      The {parentKind} by <AgentName actor={actor} inline /> that your reply answered{" "}
      {deleted ? "was deleted, so it takes no reply" : "is no longer listed"}. Your reply is kept
      here{waits ? ", to use in this field once it is empty" : ""}:
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
