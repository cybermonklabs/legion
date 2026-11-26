import type { ReactNode } from "react";

import type { Actor } from "../../api/types";
import { AgentName } from "../refs/AgentName";
import type { ReplyTarget } from "./composer-model";
import { addressWords } from "./send-request";
import type { AwayDraft } from "./unsent-drafts";

/** The words of `KeptReply` around the author's name and the control that goes on with the
 *  reply, by the kind of post it answers (a question's thread is named as a comment's). */
export const KEPT_REPLY_WORDS = {
  after: " is kept. Press ",
  before: { comment: "Your reply to the comment by ", message: "Your reply to the message by " },
  control: { comment: "Reply on that comment", message: "Reply on that message" },
  end: " to go on with it.",
} as const;

/** The words of `GoneReply` around the author's name: by the kind of post the reply answered,
 *  what became of that post, and where the reply is kept. */
export const GONE_REPLY_WORDS = {
  answered: " that your reply answered ",
  became: { deleted: "was deleted, so it takes no reply", gone: "is no longer listed" },
  before: { ask: "The ask by ", comment: "The comment by ", message: "The message by " },
  end: ":",
  kept: ". Your reply is kept here",
  waits: ", to use in this field once it is empty",
} as const;

/** The words of `keptAwayLine` around where a draft that answers no reply was written for. */
export const KEPT_MESSAGE_WORDS = {
  after: " is kept. Press Cancel reply to go on with it.",
  before: "The message you wrote ",
} as const;

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
      {KEPT_REPLY_WORDS.before[parent]}
      <AgentName actor={actor} inline />
      {KEPT_REPLY_WORDS.after}
      {press ?? KEPT_REPLY_WORDS.control[parent]}
      {KEPT_REPLY_WORDS.end}
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
      {GONE_REPLY_WORDS.before[parentKind]}
      <AgentName actor={actor} inline />
      {GONE_REPLY_WORDS.answered}
      {deleted ? GONE_REPLY_WORDS.became.deleted : GONE_REPLY_WORDS.became.gone}
      {GONE_REPLY_WORDS.kept}
      {waits ? GONE_REPLY_WORDS.waits : ""}
      {GONE_REPLY_WORDS.end}
    </>
  );
}

/** What the composer says of a draft it keeps for another address than the one it shows
 *  (`AwayDraft`): whose message or comment that draft answers, or that it answers none, and the
 *  way back to it - that message's Reply, or Cancel reply. Nothing is sent until the reader
 *  presses. */
export function keptAwayLine({ owner, replyTo }: AwayDraft): ReactNode {
  if (replyTo === null) {
    return `${KEPT_MESSAGE_WORDS.before}${addressWords({ owner, replyTo }, "for")}${KEPT_MESSAGE_WORDS.after}`;
  }
  return <KeptReply actor={replyTo.actor} parentKind={replyTo.parentKind} />;
}
