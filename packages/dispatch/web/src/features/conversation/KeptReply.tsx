import type { ReactNode } from "react";

import type { Actor } from "../../api/types";
import { AgentName } from "../refs/AgentName";
import { type AgentNames, CachedAuthor } from "./CachedAuthor";
import type { ReplyTarget } from "./composer-model";
import { GONE_REPLY_WORDS, KEPT_MESSAGE_WORDS, KEPT_REPLY_WORDS } from "./composer-words";
import { addressWords } from "./send-request";
import type { AwayDraft } from "./unsent-drafts";

/** A host's one sentence about a post's author, cut where the author's name goes: the name is
 *  drawn between `before` and `after`. */
export interface AroundAuthor {
  readonly after: string;
  readonly before: string;
}

/** A host's own sentence for a kept reply (`KeptReply`), by the kind of post it answers, each
 *  kind by its own name, which names the way back itself. */
export type KeptReplyWords = Readonly<Record<ReplyTarget["parentKind"], AroundAuthor>>;

/** A host's own sentence for a reply kept for a post it no longer lists or lists deleted
 *  (`GoneReply`): what goes before the author's name, by the post's kind, and what goes after
 *  it, by what became of the post (`deleted`, `gone`) and by whether Use it here waits for the
 *  field to be empty (`waits`) or not (`now`). */
export interface GoneReplyWords {
  readonly after: Readonly<
    Record<"deleted" | "gone", { readonly now: string; readonly waits: string }>
  >;
  readonly before: Readonly<Record<ReplyTarget["parentKind"], string>>;
}

/** The sentence said of a reply the reader wrote and that is kept out of sight - by a composer of
 *  a draft it keeps for another reply, by the docked composer of a thread reply put away over its
 *  own draft, and by a closed thread card of its reply: whose comment or message it answers, by
 *  its author, and the control that goes on with it, that comment's or message's Reply unless the
 *  host names the control beside the sentence (`press`). A host with a sentence of its own
 *  (`words`, the issue page's box) says that one, which names the way back itself. */
export function KeptReply({
  actor,
  parentKind = "comment",
  press,
  words,
}: {
  readonly actor: Actor;
  readonly parentKind?: ReplyTarget["parentKind"];
  readonly press?: string;
  readonly words?: KeptReplyWords;
}): ReactNode {
  if (words !== undefined) {
    return (
      <>
        {words[parentKind].before}
        <AgentName actor={actor} inline />
        {words[parentKind].after}
      </>
    );
  }
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
 *  (`KeptDrafts.awayTakes`), the words say so (`waits`). A host with a sentence of its own
 *  (`words`, the issue page's box) says that one. */
export function GoneReply({
  actor,
  agentNames = "live",
  deleted = false,
  parentKind,
  waits = false,
  words,
}: {
  readonly actor: Actor;
  /** How the post's author is named: `live`, as everywhere, draws `AgentName`, whose session
   *  form reads the live agent registry and starts the agents poll; `cache` names the author from
   *  the registry the cache already holds (`CachedAuthor`) and starts no read, for a host that
   *  sends no agents read when it draws (dispatch://INFRA-5702/spec#b-r-reads). With `words`, the
   *  author is always named by `AgentName`, whatever this says. */
  readonly agentNames?: AgentNames;
  readonly deleted?: boolean;
  readonly parentKind: ReplyTarget["parentKind"];
  readonly waits?: boolean;
  readonly words?: GoneReplyWords;
}): ReactNode {
  if (words !== undefined) {
    const after = words.after[deleted ? "deleted" : "gone"];
    return (
      <>
        {words.before[parentKind]}
        <AgentName actor={actor} inline />
        {waits ? after.waits : after.now}
      </>
    );
  }
  if (agentNames === "cache") {
    return (
      <>
        {GONE_REPLY_WORDS.before[parentKind]}
        <CachedAuthor actor={actor} />
        {GONE_REPLY_WORDS.answered}
        {deleted ? GONE_REPLY_WORDS.became.deleted : GONE_REPLY_WORDS.became.gone}
        {GONE_REPLY_WORDS.kept}
        {waits ? GONE_REPLY_WORDS.waits : ""}
        {GONE_REPLY_WORDS.end}
      </>
    );
  }
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
 *  presses. A reply's line is the host's own sentence where it has one (`words`). */
export function keptAwayLine({ owner, replyTo }: AwayDraft, words?: KeptReplyWords): ReactNode {
  if (replyTo === null) {
    return `${KEPT_MESSAGE_WORDS.before}${addressWords({ owner, replyTo }, "for")}${KEPT_MESSAGE_WORDS.after}`;
  }
  return <KeptReply actor={replyTo.actor} parentKind={replyTo.parentKind} words={words} />;
}
