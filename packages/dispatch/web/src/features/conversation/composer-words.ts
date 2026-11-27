import { STOPPED_UPLOAD_WORDS } from "../artifacts/upload-words";
import { KEYED_SEND_LATE_TEXT } from "../writes/write-words";

/**
 * The sentences the mention composer and the parts it draws say in answer to the reader's act, in
 * one module that imports only other modules of words, types and constants of `@legion/contracts`.
 * Every composer reads them here (`MentionComposer.tsx`, `KeptReply.tsx`, `KeptDraftOffers.tsx`,
 * `useKeptDrafts.ts`), and so does a list of words that must load where no component can: the
 * issue page's list is read by a browser spec's Node loader, which cannot load a module that
 * imports a JSON file the way the components do. A sentence that names a session, an author, an
 * address or what a write may have done is cut around that name, and one that differs by the kind
 * of post or by one or many is keyed by it. The words a send says of itself - its mode's command,
 * where it goes, what a stopped one may have done - are a module of their own (`send-words.ts`,
 * which says why): the entry chunk makes a send, and imports those alone.
 */

/** A table of words whose every string may be another host's: the shape of `COMPOSER_WORDS`. */
type WordsLike<T> = { readonly [K in keyof T]: T[K] extends string ? string : WordsLike<T[K]> };

/**
 * What the composer says in answer to the reader's act, on every surface, and what the issue
 * page's box says where its host gives no sentences of its own (`OutlineLook.answers`):
 *
 * - `deletedReply`: Send's refusal of a reply to a post deleted since the reply opened, by the
 *   post's kind, before where the draft goes instead;
 * - `delivery`: what a send to the sessions the draft names would do - the plain mode they take
 *   in place of Send, no mode at all, or not the mode the draft asks for, with the prefixed modes
 *   they would take instead - around the sessions' titles and the modes' names, its verb in one
 *   or many;
 * - `late`: a send past its deadline (`KEYED_SEND_LATE_TEXT`);
 * - `refusals`: why Send refuses an empty draft, a delivery command with nothing after it, and
 *   an edit that waits beside a newer body;
 * - `refused`: a send Dispatch refused, before its words, or the words where it gave none;
 * - `stoppedUpload`: what a file the reader stopped uploading is, and what it may have been.
 */
export const COMPOSER_WORDS = {
  deletedReply: {
    ask: "The ask this reply answers was deleted, so it takes no reply.",
    comment: "The comment this reply answers was deleted, so it takes no reply.",
    message: "The message this reply answers was deleted, so it takes no reply.",
  },
  delivery: {
    deaf: {
      many: " advertise no delivery mode, so sending it records a failed attempt. ",
      one: " advertises no delivery mode, so sending it records a failed attempt. ",
    },
    plain: {
      after: { many: " do not advertise Send.", one: " does not advertise Send." },
      before: "Sending as ",
      between: ": ",
    },
    unsupported: {
      after: ", so sending it records a failed attempt.",
      before: { many: " do not advertise ", one: " does not advertise " },
      instead: {
        after: " instead.",
        article: { aside: "an ", btw: "a " },
        before: " Prefix with ",
        between: " to send it as ",
        or: " or ",
      },
    },
  },
  late: KEYED_SEND_LATE_TEXT,
  refusals: {
    afterCommand: { after: ".", before: "Type the message after " },
    editChanged: "Use your edit or discard it before you save.",
    empty: "Type a message first.",
    emptyQuestion: "Type the question first.",
  },
  refused: { before: "Couldn't send — ", unanswered: "network error" },
  stoppedUpload: STOPPED_UPLOAD_WORDS,
} as const;

/** The words of `COMPOSER_WORDS` with any strings, as a host gives some (`OutlineAnswers`). */
export type ComposerWords = WordsLike<typeof COMPOSER_WORDS>;

/** The control beside a refusal that drops the draft it handed back. */
export const DISCARD_DRAFT_LABEL = "Discard draft";

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

/** The reader's presses for a draft offered beside a field, which drop it or take it into the
 *  field: for a kept draft, and for an edit that waits beside the comment. */
export const OFFER_LABELS = {
  dropDraft: "Discard my draft",
  dropEdit: "Discard my edit",
  takeEdit: "Use my edit",
  takeHere: "Use it here",
} as const;

/** What an edit written over a body the comment no longer holds says, before the edit. */
export const CHANGED_EDIT_LEAD =
  "This comment changed since you wrote your edit. The field shows the comment as it reads now. Your edit is kept here:";

/** The words of `writtenElsewhere`: of a refused reply whose post the host no longer lists or
 *  lists deleted, by the post's kind and what became of it (`gone`); of one whose post stands
 *  (`reply`); and around where a refused message that answers no reply was written for
 *  (`address`). */
export const WRITTEN_ELSEWHERE_WORDS = {
  address: {
    after: ". Go back to it to send it, or discard it.",
    before: "This message was written ",
  },
  gone: {
    after: ". Discard it to write here.",
    became: { deleted: "was deleted, which takes no reply", gone: "is no longer listed" },
    before: {
      ask: "This message was written as a reply to a ask that ",
      comment: "This message was written as a reply to a comment that ",
      message: "This message was written as a reply to a message that ",
    },
  },
  reply:
    "This message was written as a reply. Reply to that message again to send it, or discard it.",
} as const;
