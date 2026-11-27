import type { MutationKey } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useRef } from "react";
import { flushSync } from "react-dom";

import type { Actor } from "../../api/types";
import {
  countBefore,
  focusableIn,
  focusLost,
  inSightNow,
  placesFrom,
} from "../../components/focus-place";
import { takesText } from "../../components/key-targets";
import {
  dangerText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { SendStoppedError } from "../writes/keyed-write";
import type { ComposerKind, ReplyTarget } from "./composer-model";
import { CHANGED_EDIT_LEAD, OFFER_LABELS } from "./composer-words";
import { useHeldSend, useHeldSends } from "./held-sends";
import { GoneReply, type GoneReplyWords } from "./KeptReply";
import type { MentionFieldHandle } from "./MentionField";
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
 * drafts its compose kept for words of a document the same way (`MarginKeptDrafts`). An offer
 * that holds its field read only gives its words the id the field names as its description
 * (`id`), and after a key typed in that field says, after those words, what the field waits for
 * (`typed`).
 */
export function DraftOffer({
  body,
  drop,
  id,
  lead,
  note,
  replacement = "",
  take,
  typed,
}: {
  readonly body: string;
  readonly drop: OfferPress;
  readonly id?: string;
  readonly lead: ReactNode;
  readonly note?: string;
  readonly replacement?: string;
  readonly take?: OfferPress;
  readonly typed?: string;
}): ReactNode {
  return (
    <div className="space-y-2">
      <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} id={id} role="status">
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
        {typed === undefined ? null : <p>{typed}</p>}
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
 * Commits `press`, a press of an offer (`DraftOffer`) that takes the offer away and with it the
 * button that held the focus, at once (`flushSync`), so the page holds what the press left; and
 * where the focus went with the button (`focusLost`), hands it to the control `next` names in that
 * page, with no scroll. A text box takes it with the caret at the end of what it holds: a draft
 * taken in is ready to go on. The caret is set first and the focus restores it, so no engine
 * scrolls to show it. A host that moved the focus itself, or took `next`'s control away, keeps
 * that. A composer's offers hand it to its field (`KeptDraftOffers`), and so does the reply the
 * Conversation's docked composer draws for a phone thread (`ConversationTab`); the margin's kept
 * drafts hand it along their list (`MarginKeptDrafts`), and a deleted post's kept reply and kept
 * edit to the control nearest their place (`handFocusFrom`).
 */
export function handFocusOn(press: () => void, next: () => HTMLElement | null): void {
  flushSync(press);
  const control = next();
  if (control === null || !focusLost()) return;
  if (control instanceof HTMLTextAreaElement) {
    control.setSelectionRange(control.value.length, control.value.length);
  }
  control.focus({ preventScroll: true });
}

/** The places a press that takes an offer away hands the focus it leaves along, innermost
 *  first (`handFocusFrom`): the margin reply the offer stands in, then the margin's card or the
 *  Inbox's thread around it. */
const OFFER_PLACES = ["[data-reply]", "[data-margin-item], [data-inbox-thread]"];

/**
 * Commits `press`, a press that takes `offer` away and with it the button that held the focus,
 * and hands the focus it leaves (`handFocusOn`) to the control nearest the offer's place that
 * the reader can see once the press is done (`inSightNow`): in each place around the offer,
 * innermost first (`OFFER_PLACES`), the first control after it, which stands at its place once
 * it has gone, then each control before it, walking back. The controls are read while the offer
 * stands; one inside it goes with it, and one that takes typed text is passed over: the reader
 * never chose such a field, and their next key would be typed into it. The page's component
 * hand-offs walk by the same rule (`useFocusPlace`, `placesFrom`). A control of the inner place is
 * one of the outer place too, so each is read once, at its first place. With no such control in
 * sight the focus stays where the press left it. A deleted post's kept reply hands it so
 * (`KeptReplyToDeleted`), and so does its kept edit (`ThreadCard`'s `DeletedCommentEdit`).
 */
export function handFocusFrom(press: () => void, offer: Element | null): void {
  const controls = new Set<HTMLElement>(
    offer === null
      ? []
      : OFFER_PLACES.flatMap((selector) => {
          const place = offer.closest(selector);
          if (place === null) return [];
          const reachable = focusableIn(place).filter(
            (control) => !offer.contains(control) && !takesText(control)
          );
          return placesFrom(reachable, countBefore(reachable, offer), () => true);
        })
  );
  handFocusOn(press, () => {
    const clips = new Map<Element, boolean>();
    return (
      [...controls].find((control) => control.isConnected && inSightNow(control, clips)) ?? null
    );
  });
}

/**
 * The reply the reader kept for a thread whose root its author deleted, where the host draws no
 * reply composer: the post keeps its place, but Dispatch takes no reply to it (`409
 * POST_DELETED`), so no Reply or Send goes on with the reply. Its own words are drawn instead,
 * from where the thread's reply composer kept them under `mutationKey` - typed and never sent
 * (`unsent-drafts.ts`, an inline reply's or a standing box's), or a send Dispatch refused or the
 * reader stopped (`held-sends.ts`) - saying the post was deleted and giving no word of it, with
 * Discard my draft, which drops them where they are kept and hands the focus it leaves to the
 * control nearest the offer's place (`handFocusFrom`). Nothing is sent. The margin's thread card
 * draws it under a deleted root (`ThreadCard`), and so does the Inbox's thread (`ThreadBody`).
 */
export function KeptReplyToDeleted({
  actor,
  mutationKey,
  parentKind,
}: {
  /** The deleted post's author. */
  readonly actor: Actor;
  readonly mutationKey: MutationKey;
  readonly parentKind: ReplyTarget["parentKind"];
}): ReactNode {
  const offer = useRef<HTMLDivElement>(null);
  const store = useHeldSends();
  const unsent = useUnsentDrafts();
  const name = composerNameOf(mutationKey);
  const held = useHeldSend(mutationKey);
  const typed = unsent.useUnsent(name);
  const refused = held?.status === "refused" ? held : undefined;
  const draft = refused?.draft.draft ?? typed;
  if (draft === undefined) return null;
  return (
    <div className="mt-2" ref={offer}>
      <DraftOffer
        body={draft.body}
        drop={{
          label: "Discard my draft",
          press: () =>
            handFocusFrom(() => {
              if (refused !== undefined) store.discard(mutationKey);
              else unsent.write(name, undefined);
            }, offer.current),
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
 * why - a host's own sentences for the edit and the replies where it has them (`words`, the issue
 * page's box) - and with the reader's presses for it: one takes it into the field, where the
 * composer can send it, and the other drops it. Either press hands the focus to the field
 * (`field`). Nothing is saved or sent meanwhile. The edit and the offered draft each hold the
 * field read only while they wait, so the one that does gives its words the id the field names
 * as its description (`heldId`), and once a key was typed in the field (`typedOver`) says after
 * them what the field waits for.
 */
export function KeptDraftOffers({
  changed,
  field,
  gone,
  goneTakes,
  heldId,
  kind,
  offered,
  onDiscardChanged,
  onDropGone,
  onDropOffered,
  onTakeChanged,
  onTakeGone,
  onTakeOffered,
  typedOver,
  words,
}: {
  readonly changed: SentDraft | undefined;
  /** The composer's body field, whose text box each press of an offer here hands the focus to
   *  (`MentionFieldHandle.textarea`). */
  readonly field: RefObject<MentionFieldHandle | null>;
  readonly gone: readonly GoneReplyDraft[];
  /** Where Use it here can put a draft of `gone` (`KeptDrafts.awayTakes`). */
  readonly goneTakes: KeptDrafts["awayTakes"];
  /** The id the offer that holds the field read only gives its words, which the field names as
   *  its description (`DraftOffer.id`): the edit, else the offered draft. */
  readonly heldId: string;
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
  /** Whether the reader typed a key in the field while an offer here held it read only, which
   *  wrote nothing there. */
  readonly typedOver: boolean;
  /** The host's own sentence before an edit that waits beside a newer comment (`changedLead`),
   *  and for a reply whose post is gone (`goneReply`); with none, the composer's own. */
  readonly words?: { readonly changedLead: string; readonly goneReply: GoneReplyWords };
}): ReactNode {
  /** Each press below takes its offer away, and the field takes the focus it leaves
   *  (`handFocusOn`). A host that moved the focus itself, or took the composer away with Discard
   *  my edit, the composer's Discard (`onClose`), keeps that. */
  const intoField = (press: () => void) => () =>
    handFocusOn(press, () => field.current?.textarea() ?? null);
  return (
    <>
      {/* An edit written over a body the comment no longer holds: the field shows the comment as
          it reads now, and the edit waits beside it with the words that say why, until the reader
          takes it back into the field or drops it. Nothing is saved meanwhile, and the field is
          read only, which a key typed there has the edit say. */}
      {changed === undefined ? null : (
        <DraftOffer
          body={changed.body}
          drop={{ label: OFFER_LABELS.dropEdit, press: intoField(onDiscardChanged) }}
          id={heldId}
          lead={words?.changedLead ?? CHANGED_EDIT_LEAD}
          take={{ label: OFFER_LABELS.takeEdit, press: intoField(onTakeChanged) }}
          typed={typedOver ? "Use or discard your kept edit first." : undefined}
        />
      )}
      {/* A draft the margin's compose kept for other words of the document: a newer compose there
          carried nothing, so it waits beside this one, naming the words it was written for, and
          the kind it was written as where this compose is another, since what it brings back -
          an ask's options, a suggestion's replacement - shows only under that kind. Only the
          reader's own press puts it in this field, and nothing is sent meanwhile. A draft whose
          send the reader stopped may have been delivered there, and taken here it goes as a new
          message, so the offer says so before that press, as the stopped send's note did. The
          field is read only meanwhile, so a key typed there has the offer say what the field
          waits for, by the presses it draws: a compose its closed issue refuses draws Use my draft
          only once the issue reopens. Only a margin compose offers a draft, and it edits no
          comment, so this offer and the edit never wait together; were they to, the edit holds
          the field's description. */}
      {offered === undefined ? null : (
        <DraftOffer
          body={offered.draft.body}
          drop={{ label: "Discard my draft", press: intoField(onDropOffered) }}
          id={changed === undefined ? heldId : undefined}
          // The words the draft was written for, as the document has them: `anchor-quote`.
          lead={`You wrote this draft${offered.kind === undefined || offered.kind === kind ? "" : ` as ${KIND_WORDS[offered.kind]},`} for other words: “${offered.anchor.quote}”. It is kept here:`}
          note={offered.note}
          replacement={offered.draft.replacement}
          take={
            onTakeOffered === undefined
              ? undefined
              : { label: "Use my draft", press: intoField(onTakeOffered) }
          }
          typed={
            !typedOver || changed !== undefined
              ? undefined
              : onTakeOffered === undefined
                ? "Reopen the issue to use your kept draft, or discard it."
                : "Use or discard your kept draft first."
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
          drop={{ label: OFFER_LABELS.dropDraft, press: intoField(() => onDropGone(away)) }}
          key={away.replyTo.id}
          lead={
            <GoneReply
              actor={away.replyTo.actor}
              deleted={away.listing === "deleted"}
              parentKind={away.replyTo.parentKind}
              waits={goneTakes === "once empty"}
              words={words?.goneReply}
            />
          }
          replacement={away.draft.replacement}
          take={
            goneTakes === "here"
              ? { label: OFFER_LABELS.takeHere, press: intoField(() => onTakeGone(away)) }
              : undefined
          }
        />
      ))}
    </>
  );
}
