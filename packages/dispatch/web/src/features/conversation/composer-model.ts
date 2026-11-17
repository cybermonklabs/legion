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
  readonly parentKind: "comment" | "message";
  /** Canonical targets carried by a comment parent, prefilled as editable @ mentions. */
  readonly mentions?: readonly { readonly target: string; readonly title: string }[];
  /** Kept while old targeted-message threads remain readable; S2 writes comments only. */
  readonly thread?: {
    readonly delivery: DeliveryCapability;
    readonly target: string;
  };
  readonly to?: string;
}

/** One accepted mention: the target, and the span of the body that names it. */
export interface AcceptedMention {
  readonly end: number;
  readonly start: number;
  readonly target: string;
  readonly text: string;
}

/** A composer's field as its host hands it a draft the reader carried there from another
 *  composer with that composer's own control (`MentionComposer`'s `intake`). The draft and the
 *  address are a send's own shapes (`send-request.ts`'s `SentDraft` and `CarriedAddress`), spelled
 *  here from this module's types, since that module reads its types from this one. */
export interface DraftIntake {
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
