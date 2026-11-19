import type { SessionAnswerRelation } from "@legion/contracts";

/**
 * What an item of Dispatch's answers read (`GET /api/v1/agents/{session_id}/answers`) is to the
 * session, in words. The answers list (`renderSessionAnswerItem`) prints them, and the live card
 * of a reply in an ask's thread (`renderInbound`) names its reader's relation to the reply in the
 * same words, so the two agree for each session the comment named as the ask's owner. They can
 * differ for a session that follows the ask after the comment was written: the card reads the
 * comment's `ask_sessions`, the owners as its write left them, and the answers read decides who
 * owns the ask when it is read.
 */
const RELATION_PHRASES: Readonly<Record<SessionAnswerRelation, string>> = {
  answered_your_ask: "answered your ask",
  answered_an_ask: "answered an ask",
  declined_your_ask: "declined to answer your ask",
  declined_an_ask: "declined to answer an ask",
  replied_on_your_ask: "replied on your ask",
  replied_in_a_comment_thread: "replied in a comment thread",
  commented_on_a_document: "commented on a document",
  commented_on_an_issue: "commented on an issue",
  mentioned_you: "mentioned you",
  sent_you_a_message: "sent you a message",
  replied_to_your_message: "replied to your message",
};

/** Whether a `relation` the server sent is one this build words; the wire type is an open
 *  string, so a relation a newer Dispatch adds is printed as it came. */
function isSessionAnswerRelation(value: string): value is SessionAnswerRelation {
  return Object.hasOwn(RELATION_PHRASES, value);
}

/** `relation` in words: this build's phrase for it, or the relation as it came when this build
 *  words no such relation. */
export function sessionAnswerRelationPhrase(relation: string): string {
  return isSessionAnswerRelation(relation) ? RELATION_PHRASES[relation] : relation;
}
