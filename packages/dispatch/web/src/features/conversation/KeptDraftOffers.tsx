import type { MutationKey } from "@tanstack/react-query";
import type { ReactNode } from "react";

import type { Actor } from "../../api/types";
import {
  dangerText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { SendStoppedError } from "../writes/keyed-write";
import type { AgentNames } from "./CachedAuthor";
import type { ComposerKind, ReplyTarget } from "./composer-model";
import { CHANGED_EDIT_LEAD, OFFER_LABELS } from "./composer-words";
import { useHeldSend, useHeldSends } from "./held-sends";
import { GoneReply } from "./KeptReply";
import type { SentDraft } from "./send-request";
import { useUnsentDrafts } from "./unsent-drafts";
import {
  composerNameOf,
  type GoneReplyDraft,
  type KeptDrafts,
  type OfferedDraft,
} from "./useKeptDrafts";

/** Each kind as a kept draft names the one it was written as, beside a compose of another
 *  (`KeptDraftOffers`) or in the margin's list of what its compose kept (`MarginKeptDrafts`). */
export const KIND_WORDS: Record<ComposerKind, string> = {
  ask: "an ask",
  comment: "a comment",
  suggestion: "a suggestion",
};

/** One of the reader's presses for a draft offered beside a field. */
interface OfferPress {
  readonly label: string;
  readonly press: () => void;
}

/**
 * A draft the reader wrote that waits outside the field: the line that says why, the draft's text,
 * a suggestion's replacement where it has one, and a warning where its send may have been
 * delivered (`note`), then the reader's presses for it - one that takes it into the field, which
 * a host that could not send it leaves out (`take`), and one that drops it. The margin draws the
 * drafts its compose kept for words of a document the same way (`MarginKeptDrafts`).
 */
export function DraftOffer({
  body,
  drop,
  lead,
  note,
  replacement = "",
  take,
}: {
  readonly body: string;
  readonly drop: OfferPress;
  readonly lead: ReactNode;
  readonly note?: string;
  readonly replacement?: string;
  readonly take?: OfferPress;
}): ReactNode {
  return (
    <div className="space-y-2">
      <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} role="status">
        <p>{lead}</p>
        <blockquote
          className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
        >
          {/* The reader's own text as the field holds it: the reading list's `kept-draft`. */}
          {body}
        </blockquote>
        {replacement === "" ? null : (
          <>
            <p>Replace with:</p>
            <blockquote
              className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
            >
              {replacement}
            </blockquote>
          </>
        )}
        {note === undefined ? null : <p className={dangerText}>{note}</p>}
      </div>
      <div className="flex flex-wrap gap-3">
        {take === undefined ? null : (
          <button
            className={`text-sm font-medium ${linkText} ${linkHoverText}`}
            onClick={take.press}
            type="button"
          >
            {take.label}
          </button>
        )}
        <button
          className={`text-sm font-medium underline ${dangerText}`}
          onClick={drop.press}
          type="button"
        >
          {drop.label}
        </button>
      </div>
    </div>
  );
}

/**
 * The reply the reader kept for a thread whose root its author deleted, where the host draws no
 * reply composer: the post keeps its place, but Dispatch takes no reply to it (`409
 * POST_DELETED`), so no Reply or Send goes on with the reply. Its own words are drawn instead,
 * from where the thread's reply composer kept them under `mutationKey` - typed and never sent
 * (`unsent-drafts.ts`, an inline reply's or a standing box's), or a send Dispatch refused or the
 * reader stopped (`held-sends.ts`) - saying the post was deleted and giving no word of it, with
 * Discard my draft, which drops them where they are kept. Nothing is sent. The margin's thread
 * card draws it under a deleted root (`ThreadCard`), and so does the Inbox's thread
 * (`ThreadBody`).
 */
export function KeptReplyToDeleted({
  actor,
  agentNames = "live",
  mutationKey,
  parentKind,
}: {
  /** The deleted post's author. */
  readonly actor: Actor;
  /** How the deleted post's author is named (`GoneReply`): `cache` for a host that sends no
   *  agents read when it draws. */
  readonly agentNames?: AgentNames;
  readonly mutationKey: MutationKey;
  readonly parentKind: ReplyTarget["parentKind"];
}): ReactNode {
  const store = useHeldSends();
  const unsent = useUnsentDrafts();
  const name = composerNameOf(mutationKey);
  const held = useHeldSend(mutationKey);
  const typed = unsent.useUnsent(name);
  const refused = held?.status === "refused" ? held : undefined;
  const draft = refused?.draft.draft ?? typed;
  if (draft === undefined) return null;
  if (agentNames === "cache") {
    return (
      <div className="mt-2">
        <DraftOffer
          body={draft.body}
          drop={{
            label: "Discard my draft",
            press: () => {
              if (refused !== undefined) store.discard(mutationKey);
              else unsent.write(name, undefined);
            },
          }}
          lead={<GoneReply actor={actor} agentNames="cache" deleted parentKind={parentKind} />}
          note={
            refused?.refusal instanceof SendStoppedError
              ? "Dispatch has not confirmed this reply: it may have been posted, and if it was, it is listed in this thread."
              : undefined
          }
          replacement={draft.replacement}
        />
      </div>
    );
  }
  return (
    <div className="mt-2">
      <DraftOffer
        body={draft.body}
        drop={{
          label: "Discard my draft",
          press: () => {
            if (refused !== undefined) store.discard(mutationKey);
            else unsent.write(name, undefined);
          },
        }}
        lead={<GoneReply actor={actor} deleted parentKind={parentKind} />}
        note={
          refused?.refusal instanceof SendStoppedError
            ? "Dispatch has not confirmed this reply: it may have been posted, and if it was, it is listed in this thread."
            : undefined
        }
        replacement={draft.replacement}
      />
    </div>
  );
}

/**
 * What a composer offers beside its field that the field does not hold (`useKeptDrafts`): an
 * edit written over a body the comment no longer holds (`changed`), a draft a margin compose
 * kept for other words of its document (`offered`), and each reply it keeps whose comment or
 * message its host no longer lists, or lists deleted (`gone`). Each waits with the words that say
 * why, and with the reader's presses for it: one takes it into the field, where the composer can
 * send it, and the other drops it. Nothing is saved or sent meanwhile.
 */
export function KeptDraftOffers({
  changed,
  gone,
  goneTakes,
  kind,
  offered,
  onDiscardChanged,
  onDropGone,
  onDropOffered,
  onTakeChanged,
  onTakeGone,
  onTakeOffered,
}: {
  readonly changed: SentDraft | undefined;
  readonly gone: readonly GoneReplyDraft[];
  /** Where Use it here can put a draft of `gone` (`KeptDrafts.awayTakes`). */
  readonly goneTakes: KeptDrafts["awayTakes"];
  /** The kind the composer writes now, which an offered draft names its own beside. */
  readonly kind: ComposerKind;
  readonly offered: OfferedDraft | undefined;
  /** Discard my edit: the composer's Discard, which drops the draft the reader wrote there. */
  readonly onDiscardChanged: () => void;
  /** Discard my draft for a draft of `gone`. */
  readonly onDropGone: (away: GoneReplyDraft) => void;
  readonly onDropOffered: () => void;
  readonly onTakeChanged: () => void;
  /** Use it here for a draft of `gone`, drawn while `goneTakes` says `"here"`. */
  readonly onTakeGone: (away: GoneReplyDraft) => void;
  /** Use my draft, which a compose that could not send the draft - a kind its closed issue
   *  refuses - leaves out, so the offer waits until the reader drops it. */
  readonly onTakeOffered: (() => void) | undefined;
}): ReactNode {
  return (
    <>
      {/* An edit written over a body the comment no longer holds: the field shows the comment as
          it reads now, and the edit waits beside it with the words that say why, until the reader
          takes it back into the field or drops it. Nothing is saved meanwhile. */}
      {changed === undefined ? null : (
        <DraftOffer
          body={changed.body}
          drop={{ label: OFFER_LABELS.dropEdit, press: onDiscardChanged }}
          lead={CHANGED_EDIT_LEAD}
          take={{ label: OFFER_LABELS.takeEdit, press: onTakeChanged }}
        />
      )}
      {/* A draft the margin's compose kept for other words of the document: a newer compose there
          carried nothing, so it waits beside this one, naming the words it was written for, and
          the kind it was written as where this compose is another, since what it brings back -
          an ask's options, a suggestion's replacement - shows only under that kind. Only the
          reader's own press puts it in this field, and nothing is sent meanwhile. A draft whose
          send the reader stopped may have been delivered there, and taken here it goes as a new
          message, so the offer says so before that press, as the stopped send's note did. */}
      {offered === undefined ? null : (
        <DraftOffer
          body={offered.draft.body}
          drop={{ label: "Discard my draft", press: onDropOffered }}
          // The words the draft was written for, as the document has them: `anchor-quote`.
          lead={`You wrote this draft${offered.kind === undefined || offered.kind === kind ? "" : ` as ${KIND_WORDS[offered.kind]},`} for other words: “${offered.anchor.quote}”. It is kept here:`}
          note={offered.note}
          replacement={offered.draft.replacement}
          take={
            onTakeOffered === undefined
              ? undefined
              : { label: "Use my draft", press: onTakeOffered }
          }
        />
      )}
      {/* A reply kept for a comment or message the host no longer lists, or lists deleted, which
          takes no reply: the Reply its kept line would name goes nowhere, so the reply itself
          waits here, with the words that say whose comment or message it answered and what
          became of it - never a word of that post - and Discard my draft. At the owner's own
          address, Use it here puts it in the field as a message that answers none, once nothing
          of the field's own is there to be written over, and the words say when it waits for
          that. */}
      {gone.map((away) => (
        <DraftOffer
          body={away.draft.body}
          drop={{ label: OFFER_LABELS.dropDraft, press: () => onDropGone(away) }}
          key={away.replyTo.id}
          lead={
            <GoneReply
              actor={away.replyTo.actor}
              deleted={away.listing === "deleted"}
              parentKind={away.replyTo.parentKind}
              waits={goneTakes === "once empty"}
            />
          }
          replacement={away.draft.replacement}
          take={
            goneTakes === "here"
              ? { label: OFFER_LABELS.takeHere, press: () => onTakeGone(away) }
              : undefined
          }
        />
      ))}
    </>
  );
}
