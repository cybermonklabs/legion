import type { Actor, DeliveryCapability } from "../../api/types";
import type { RecordMarkPlacement } from "../doc/editor";

/** The composer's data model: where a draft goes, what kind it is, and what it carries. The
 *  composer (`MentionComposer.tsx`), the request it freezes (`send-request.ts`) and every host read
 *  these from here, so no module imports a type back from one that imports it. */

export type ComposerOwner =
  | { readonly kind: "issue"; readonly issueKey: string }
  | { readonly kind: "artifact"; readonly artifactId: string; readonly project: string }
  | { readonly kind: "session"; readonly sessionId: string };

export type ComposerKind = "ask" | "comment" | "suggestion";

/** The text a margin compose is about. On a document that takes edits, the selection bar wrote a
 *  provisional mark over it, which the send names (`mark_id`) and the margin removes or retypes
 *  with the composer. A closed issue's document takes no edit, so there the bar wrote nothing: the
 *  send names the quote and which `occurrence` of it in the document's text the reader selected
 *  (`quoteAnchor`), and the server writes the comment's mark there. */
export type ComposerAnchor =
  | {
      readonly artifact: string;
      readonly mark_id: string;
      readonly occurrence?: never;
      /** The words the editor read on each side of `quote`, in the block it wrote the mark in, and
       *  how many readings of those words that block held, as it reported when the selection bar made
       *  it. The composer keeps it because the editor keeps nothing a rebuilt document or a reconnect
       *  does not take with it, and the margin hands it back when the mark has to be written again
       *  (`DocumentBridge.remarkMark`), where it is what tells the reader's own reading of those words
       *  from another of them. Absent where the editor had none to give, which leaves that recovery
       *  the quote alone. */
      readonly placement?: RecordMarkPlacement;
      readonly quote: string;
    }
  | {
      readonly artifact: string;
      readonly mark_id?: never;
      readonly occurrence: number;
      /** The server anchors a quote-anchored comment itself, so there is no mark to write again. */
      readonly placement?: never;
      readonly quote: string;
    };

/** Why a compose's mark could not be written again after the server refused its send for a mark
 *  the document does not hold, in the reader's words, kept with the mark it answers: the composer
 *  shows it in place of the server's words, and a newer anchor leaves it behind. */
export interface AnchorRefusal {
  readonly markId: string;
  readonly text: string;
}

export interface ReplyTarget {
  /** The parent's author, which the Replying-to chip draws through `AgentName`. */
  readonly actor: Actor;
  readonly excerpt: string;
  readonly id: string;
  /** What the reply answers: a comment (`reply_to`), a message (`in_reply_to`), or an ask, whose
   *  reply is a comment naming it (`ask_id`) - an open ask's question back, or a word under an
   *  answered or resolved one, so a reader can answer the thread they are reading. */
  readonly parentKind: "ask" | "comment" | "message";
  /** Canonical targets carried by a comment parent, prefilled as editable @ mentions. */
  readonly mentions?: readonly { readonly target: string; readonly title: string }[];
  /** Kept while old targeted-message threads remain readable; S2 writes comments only. */
  readonly thread?: {
    readonly delivery: DeliveryCapability;
    readonly target: string;
  };
  readonly to?: string;
}

/**
 * Which replies a composer's host lists now (`MentionComposer`'s `listed`), so that a draft the
 * composer keeps for a reply it does not show (`useKeptDrafts`' `keptAway`) is said to be kept
 * with the way back its line names - that comment's or message's Reply - only while that Reply is
 * there. A draft kept for a reply the host no longer lists is drawn itself, with its own presses
 * (`KeptDraftOffers`).
 *
 * A host that can lose a reply its composer's drafts answer passes the ids of every comment and
 * message its reads hold, whatever its filter, a fold or a Clear shows of them, since the host's
 * own controls reach the Reply of each (`listedReplies`). Every other host passes none, and every
 * kept reply is said to be kept as before: one whose composer answers no reply, one whose reads
 * have not answered yet, and one whose composer answers only the reply drawn with it - a thread
 * card's own reply - which leaves the page with the composer, so no line of the composer's can
 * outlive it.
 */
export type ListedReplies = ReadonlySet<string>;

/** One accepted mention: the target, and the span of the body that names it. */
export interface AcceptedMention {
  readonly end: number;
  readonly start: number;
  readonly target: string;
  readonly text: string;
}

/** A composer's field as its host reaches it with another composer's control, or with a control
 *  the host draws for this composer's draft out of its sight (`MentionComposer`'s `intake`). The
 *  draft and the address are a send's own shapes (`send-request.ts`'s `SentDraft` and
 *  `CarriedAddress`), spelled here from this module's types, since that module reads its types
 *  from this one. */
export interface DraftIntake {
  /** Drops what the composer holds - the field's draft and a send of its own held there - and
   *  ends it, as its own Discard does: the Conversation's Discard my draft for a phone thread
   *  reply put away for a comment the list no longer holds (`usePhoneThread`'s `dropParked`),
   *  whose composer is out of sight. */
  readonly drop: () => void;
  /** Puts `draft`, which the reader carried to `to`, in the field, and says whether it did: a
   *  composer takes it only while it shows `to`, and only while it holds nothing of its own - a
   *  draft typed, or a send out or refused - which it never writes over. */
  readonly take: (
    draft: {
      readonly body: string;
      readonly mentions: readonly AcceptedMention[];
      readonly replacement: string;
    },
    to: { readonly owner: ComposerOwner; readonly replyTo: ReplyTarget | null }
  ) => boolean;
}

/** Whether a draft holds anything the reader wrote: its text, a suggestion's replacement, or an
 *  ask option's label or description. */
export function hasUnsavedInput(
  body: string,
  replacement: string,
  options: readonly { readonly description: string; readonly label: string }[] = []
): boolean {
  return (
    body.trim().length > 0 ||
    replacement.trim().length > 0 ||
    options.some((option) => option.label.trim().length > 0 || option.description.trim().length > 0)
  );
}
