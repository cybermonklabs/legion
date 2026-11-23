import { hashKey, type MutationKey } from "@tanstack/react-query";
import {
  type Ref,
  type RefObject,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import type { ArtifactOwner } from "../../api/client";
import type { AskUrgency } from "../../api/types";
import { SendStoppedError } from "../writes/keyed-write";
import { textareaValue } from "./block-spans";
import {
  type AskOptionDraft,
  type Draft,
  replyPrefill,
  seededDraft,
  submittedAskOptions,
} from "./composer-draft";
import {
  type AcceptedMention,
  type ComposerAnchor,
  type ComposerKind,
  type DraftIntake,
  hasUnsavedInput,
  type ReplyListing,
  type ReplyTarget,
} from "./composer-model";
import type { HeldSend, HeldSends } from "./held-sends";
import {
  type Address,
  addressWords,
  movedSendNote,
  type SentDraft,
  type SentRequest,
  sameAddress,
} from "./send-request";
import {
  type AwayDraft,
  awayDraftOf,
  sameAnchor,
  sameReply,
  type UnsentDrafts,
} from "./unsent-drafts";
import type { ComposerSend } from "./useComposerSend";
import type { DraftHomes } from "./useDraftHomes";

/** The address a composer shows, as its props name it: an edit's comment with the body the
 *  edit opened on. */
interface ShownAddress extends Address {
  readonly edit: { readonly body: string; readonly id: string } | undefined;
}

/** What decides where a composer keeps what the reader has not sent: its own send name, and
 *  whether it is a standing box or an inline reply (`keptNames`). */
interface KeptUnder {
  readonly composerName: string | undefined;
  readonly inline: boolean;
  readonly standing: boolean;
}

/** The send names a composer keeps what the reader has not sent under: a standing box and an
 *  inline reply keep the draft whole (`unsentName`), and every other named composer keeps it by
 *  the address it was written at (`awayName`). */
function keptNames({ composerName, inline, standing }: KeptUnder) {
  return {
    /** Where every other named composer keeps a draft by the address it was written at
     *  (`AwayDraft`): its host moves it from reply to reply - a Reply on a message or a comment -
     *  or takes it off the page, so a draft is the reply's it answers, or its owner's when it
     *  answers none, and waits under the send name until the composer is back there. An edit's
     *  address is the comment it edits, which its send name names, and its draft is kept with the
     *  comment's body it was written over (`AwayDraft.base`). */
    awayName: standing || inline ? undefined : composerName,
    /** Where a standing box, and an inline reply, keep the draft the reader has not sent: under
     *  the send name, for the composer that mounts there next. An inline reply's composer leaves
     *  the page with its thread - Cancel reply, another thread, a phone thread's Back - and the
     *  draft is that thread's until the reader sends or drops it. */
    unsentName: standing || inline ? composerName : undefined,
  };
}

/** The name of the composer that sends under `mutationKey` (`KeptUnder.composerName`): its send's
 *  own name, which is what it keeps the reader's drafts under (`keptNames`). The margin keeps and
 *  reads a compose's drafts under it from outside the composer (`giveWay`, `useMarginSheet`). */
export function composerNameOf(mutationKey: MutationKey): string {
  return hashKey(mutationKey);
}

/**
 * What a margin compose's refusal becomes once a newer compose on its document takes its place,
 * which is the host's move for it and carries nothing. A send the reader stopped may have been
 * delivered, so it is not reduced to a draft: it waits under `aside` unchanged, its mark with it
 * (`HeldSends.setAside`), for the newer compose to offer (`stoppedOfferOf`). Any other refusal
 * goes, its mark with it, and its draft is kept under the compose's name for the words it was
 * written about and in the kind it was written as (`awayDraftOf`), where the compose keeps a draft
 * by address (`keptNames`' `awayName`) and the newer compose offers it (`keptForOtherWords`).
 */
export function giveWay(
  refused: HeldSend & { readonly status: "refused" },
  aside: MutationKey,
  store: HeldSends,
  unsent: UnsentDrafts
): void {
  if (refused.refusal instanceof SendStoppedError) {
    store.setAside(refused.mutationKey, aside);
    return;
  }
  unsent.keepAway(composerNameOf(refused.mutationKey), awayDraftOf(refused));
  store.discard(refused.mutationKey);
}

/** What Send says while the refusal the composer holds lives at another address than the one it
 *  shows now, since its host moved it (`sameAddress`): where the draft was written for, and the
 *  ways out - back there, where Retry sends it, or Discard. A standing box answers what its host
 *  gives it and no Reply the reader presses (`standing`): an Inbox agent thread's box replies to
 *  the thread's newest message, so one that has come since the refusal moved the box, and no
 *  press brings it back. Nor does one where the host no longer lists the comment or message the
 *  refusal answers, or lists it deleted, which takes no reply (`listingOf`): its Reply goes
 *  nowhere, so Discard is the way out, and the draft stays in the field until the reader takes
 *  it. */
function writtenElsewhere(
  held: SentRequest,
  standing: boolean,
  listingOf: (reply: ReplyTarget) => ReplyListing
): string {
  if (held.replyTo !== null) {
    const listing = listingOf(held.replyTo);
    if (listing !== "standing") {
      return (
        `This message was written as a reply to a ${held.replyTo.parentKind} that ` +
        `${listing === "deleted" ? "was deleted, which takes no reply" : "is no longer listed"}. ` +
        "Discard it to write here."
      );
    }
    return standing
      ? "This message was written as a reply to an earlier message, and this box now replies " +
          "to a newer one. Discard it to write a reply here."
      : "This message was written as a reply. " +
          "Reply to that message again to send it, or discard it.";
  }
  return `This message was written ${addressWords(held, "for")}. Go back to it to send it, or discard it.`;
}

/** Whether an edit whose draft is `draft`, written over the comment's body `base`, waits beside
 *  the comment rather than going in the field, now that the comment reads `body` (`changed`): the
 *  comment changed since the edit was written, and not to what the edit would save. A save sends
 *  the field trimmed, and the field reads each CR LF as LF, so a comment that reads as the edit
 *  would save it leaves nothing to offer. */
function changedUnder(draft: SentDraft, base: string | undefined, body: string): boolean {
  return base !== body && textareaValue(draft.body).trim() !== textareaValue(body).trim();
}

/** What the composer's field opens with (`mountDraft`). */
export interface MountDraft {
  readonly ask: SentRequest["ask"] | undefined;
  /** The field took the draft kept for this address, which is kept there no longer. */
  readonly away: boolean;
  readonly body: string;
  /** An edit that waits beside the newer body the field opens on (`changedUnder`). */
  readonly changed: SentDraft | undefined;
  readonly mentions: AcceptedMention[];
  readonly replacement: string;
  /** The field took a draft - a held send's, or one kept under the send name or for the
   *  address - which the reply prefill must not replace. */
  readonly taken: boolean;
}

/**
 * What a composer's field opens with: the held send's draft (`held`), else the unsent draft kept
 * under the send name or for this address, else what the channel seeds. A refusal the host has
 * moved the composer off since - another reply, a newer message - gives way to a draft the reader
 * wrote here and that was kept for it, since the refusal keeps its own draft for its own address.
 * An edit refused or kept over a body another writer has changed since - one the comment no longer
 * holds, and not into what the edit would save - is not put back in the field, which opens on the
 * newer body, and waits beside it (`changedUnder`, `changed`). A margin compose takes only a draft
 * kept for its own words: one kept for other words of the document waits beside it (`offered`).
 */
export function mountDraft({
  anchor,
  edit,
  held,
  owner,
  replyTo,
  seedMentions,
  unsent,
  ...names
}: ShownAddress &
  KeptUnder & {
    readonly held: HeldSend | undefined;
    readonly seedMentions: readonly { readonly target: string; readonly title: string }[];
    readonly unsent: UnsentDrafts;
  }): MountDraft {
  const { awayName, unsentName } = keptNames(names);
  const unsentHere = unsentName === undefined ? undefined : unsent.read(unsentName);
  const kept:
    | { readonly ask?: SentRequest["ask"]; readonly base?: string; readonly draft: SentDraft }
    | undefined =
    unsentHere === undefined
      ? awayName === undefined
        ? undefined
        : unsent.readAway(awayName, replyTo, anchor)
      : { draft: unsentHere };
  const heldElsewhere =
    held?.status === "refused" && !sameAddress(held.request, { anchor, edit, owner, replyTo });
  const seeded = seededDraft(seedMentions, undefined, owner);
  const fresh = {
    ask: undefined,
    away: false,
    body: edit?.body ?? seeded.body,
    changed: undefined,
    mentions: seeded.mentions,
    replacement: "",
    taken: false,
  };
  if (held !== undefined && !(heldElsewhere && kept !== undefined)) {
    if (
      held.status === "refused" &&
      edit !== undefined &&
      changedUnder(held.draft.draft, held.request.edit?.base, edit.body)
    ) {
      return { ...fresh, changed: held.draft.draft };
    }
    return {
      ask: held.draft.ask,
      away: false,
      body: held.draft.draft.body,
      changed: undefined,
      mentions: [...held.draft.draft.mentions],
      replacement: held.draft.draft.replacement,
      taken: true,
    };
  }
  if (kept !== undefined) {
    if (edit !== undefined && changedUnder(kept.draft, kept.base, edit.body)) {
      return { ...fresh, changed: kept.draft };
    }
    return {
      ask: kept.ask,
      away: unsentHere === undefined,
      body: kept.draft.body,
      changed: undefined,
      mentions: [...kept.draft.mentions],
      replacement: kept.draft.replacement,
      taken: true,
    };
  }
  return fresh;
}

/** A draft a margin compose kept for words of its document (`AwayDraft.anchor`). */
type KeptForWords = AwayDraft & { readonly anchor: ComposerAnchor };

/**
 * A draft a margin compose offers beside its field, kept for other words of its document than the
 * compose shows now (`KeptDrafts.offered`): one its name keeps by address (`keptForOtherWords`), or
 * the send the reader stopped there, which waits aside unchanged and may have been delivered
 * (`stoppedOfferOf`). Each carries the reader's two presses for it, which move it where it is
 * kept, so the composer never asks which of the two it offers.
 */
export interface OfferedDraft extends AwayDraft {
  readonly anchor: ComposerAnchor;
  /** Discard my draft: drops it where it is kept. */
  readonly drop: () => void;
  /** What the offer says of a send the reader stopped: it may have been delivered where it went,
   *  and taken here it goes as a new message (`movedSendNote`). */
  readonly note: string | undefined;
  /** Use my draft, once the field holds the draft: a draft kept by address is kept there no
   *  longer, and a stopped send is carried into the compose at `at`, its words and its kind, as
   *  the reader's own move carries one (`HeldSends.carryInto`), so it says it may have been
   *  delivered until that compose sends it or the reader discards it (`SendMovedError`). */
  readonly take: (at: Pick<SentRequest, "anchor" | "kind">) => void;
}

/** What a margin compose offers of the send the reader stopped on its document, once that send
 *  waits aside (`giveWay`): its draft, at the words and in the kind it went out with
 *  (`awayDraftOf`), saying it may have been delivered. The compose's host gives it the presses
 *  that move that send (`MarginComposerSlot`), and the margin reads it to keep a compose that
 *  offers it open when its issue closes (`useMarginSheet`). */
export function stoppedOfferOf(
  held: HeldSend | undefined
): Omit<OfferedDraft, "drop" | "take"> | undefined {
  if (held?.status !== "refused" || held.request.anchor === undefined) return undefined;
  return { ...awayDraftOf(held), anchor: held.request.anchor, note: movedSendNote(held.request) };
}

/** The newest of the drafts a composer's name keeps by address (`kept`) for the reply `replyTo`
 *  answers and for other words of the document than `anchor` names: the one a margin compose at
 *  `anchor` offers beside its field while no send the reader stopped waits aside for it
 *  (`KeptDrafts.offered`). Only a composer with an anchor - a margin compose - offers one. The
 *  margin reads it too, to keep a compose that offers a draft open when its issue closes
 *  (`useMarginSheet`). */
export function keptForOtherWords(
  kept: readonly AwayDraft[],
  anchor: ComposerAnchor | undefined,
  replyTo: ReplyTarget | null
): KeptForWords | undefined {
  if (anchor === undefined) return undefined;
  return kept
    .filter(
      (away): away is KeptForWords =>
        sameReply(away.replyTo, replyTo) &&
        away.anchor !== undefined &&
        !sameAnchor(away.anchor, anchor)
    )
    .at(-1);
}

/** A draft the composer keeps for a reply whose comment or message its host no longer lists, or
 *  lists deleted (`ListedReplies`), with which of the two it is (`listing`). */
export type GoneReplyDraft = AwayDraft & {
  readonly listing: Exclude<ReplyListing, "standing">;
  readonly replyTo: ReplyTarget;
};

/** What the composer keeps of a draft the field does not send, the edit it offers beside a newer
 *  body, the draft a margin compose offers beside a compose at other words, and the presses for a
 *  draft kept for a reply its host no longer lists. */
export interface KeptDrafts {
  /** Where Use it here can put a draft kept for another reply (`goneAway`) whose comment or
   *  message its host no longer lists (`ListedReplies`), as the composer draws it beside its field
   *  (`KeptDraftOffers`): into this field, while the composer shows its owner's own address and
   *  the field holds nothing of the reader's own and no send (`"here"`); not yet, while that field
   *  holds a draft or a send of its own, which Use it here never writes over (`"once empty"`); and
   *  nowhere while the composer answers a reply or edits a comment, where such a draft answers
   *  nothing (`"nowhere"`). */
  readonly awayTakes: "here" | "nowhere" | "once empty";
  /** An edit written over a body the comment no longer holds (`changedUnder`): refused or kept
   *  for the comment while its editor was off the page, or written in the open editor when another
   *  writer changed the comment under it. It waits beside the newer body the field shows until
   *  the reader takes it back into the field (`takeChanged`) or drops it (`dropChanged`).
   *  Meanwhile the field is read only and Save refuses, so nothing is saved over a body the reader
   *  has not seen, and nothing they wrote goes with no word. */
  readonly changed: SentDraft | undefined;
  /** Discard my draft for a draft kept for another address (`goneAway`): forgets it where it is
   *  kept, in the page and in the tab. */
  readonly dropAway: (away: AwayDraft) => void;
  /** Drops the edit waiting beside a newer body, with what is kept of it for the comment: Discard
   *  drops the draft the reader wrote there. */
  readonly dropChanged: () => void;
  /** Drops the draft waiting beside the compose (`offered`), as its own press does
   *  (`OfferedDraft.drop`): Discard my draft, and the composer's own Discard, which Close and
   *  Escape ask for over it. */
  readonly dropOffered: () => void;
  /** What Send says while the refusal the composer holds lives at another address than the one
   *  it shows (`writtenElsewhere`), else undefined. */
  readonly elsewhere: string | undefined;
  /** Forgets the unsent copy under the send name: from Send on the held send keeps the draft. */
  readonly forgetUnsent: () => void;
  /** The drafts kept for other replies than the one the composer shows whose comment or message
   *  its host no longer lists, or lists deleted (`ListedReplies`): the Reply a kept line would
   *  name goes nowhere, so the composer draws each itself beside its field (`KeptDraftOffers`),
   *  with Discard my draft (`dropAway`) and, where `awayTakes` says so, Use it here
   *  (`takeAway`). */
  readonly goneAway: readonly GoneReplyDraft[];
  /** Writes what the field holds back to the held refusal now, as the debounced write does: the
   *  margin asks for it before it sends a refused compose again itself (`ComposerBridge`). */
  readonly keepDraft: () => void;
  /** Keeps nothing for this address as the composer goes, until a commit still draws it
   *  (`keepsNothing`). */
  readonly keepNothing: () => void;
  /** The drafts kept for other addresses than the one the composer shows, each of which the
   *  composer names with the way back to it: every one but those of `goneAway`. */
  readonly keptAway: readonly AwayDraft[];
  /** A draft kept for other words of the document than the ones this compose shows: a newer
   *  compose on the document is the host's move for the one before it, which carries nothing,
   *  so that compose's draft - typed, or handed back by a refusal - stays with the words it was
   *  written about, under the send name, and a send the reader stopped waits aside unchanged
   *  (`giveWay`). It never goes in this field by itself. It waits beside the compose, with words
   *  naming the quote it was written for, the kind it was written as where this compose is
   *  another, and for a stopped send the words saying it may have been delivered, until the
   *  reader takes it into the field (`takeOffered`) or drops it (`dropOffered`); meanwhile the
   *  field and an ask's fields are read only and Send refuses, so nothing the reader types is
   *  written over by it. Close and Escape ask `Discard draft?` over it as over the field's own
   *  draft, and Discard drops it with that. A stopped send, which the composer's host hands it
   *  (`stoppedAside`), is offered first, then the newest draft (`keptForOtherWords`). */
  readonly offered: OfferedDraft | undefined;
  /** Use it here for a draft kept for another reply (`goneAway`), while `awayTakes` says
   *  `"here"`: puts it in the field, under the channel's own seed as a draft that arrives at this
   *  address takes it, and forgets it where it was kept. From here on it is the draft of the
   *  address the composer shows, which the reader sends with their own press. */
  readonly takeAway: (away: AwayDraft) => void;
  /** Takes an edit waiting beside a newer body back into the field, which the reader saves with
   *  their own press, having had the newer body before them. It is the field's draft from here
   *  on, kept over the body it now replaces should the host take the editor away again, and a
   *  refusal's Retry sends it. */
  readonly takeChanged: () => void;
  /** Takes the draft waiting beside the compose (`offered`) into the field, by the reader's own
   *  press, with an ask's options and settings kept with it: from here on it is the draft of the
   *  words the compose shows, and the offer's own press moves what was kept of it
   *  (`OfferedDraft.take`). */
  readonly takeOffered: () => void;
}

/** How long after the last keystroke what the reader wrote over a refusal is written back to the
 *  held send, which is what a reload restores (`write-storage.ts`). Short enough that a reader who
 *  stops typing and reloads keeps their words, long enough that a burst of typing writes once. */
const KEEP_DRAFT_MS = 300;

/**
 * Keeps what the reader wrote in a composer and did not send for the address it was written at,
 * as the host moves the composer from address to address or takes it off the page, and puts back
 * what an address keeps when the composer arrives there; writes what the reader wrote over a
 * refusal back to the held send (`keepDraft`); takes a draft the reader carried here from another
 * composer, and drops the draft for a control its host draws out of the composer's sight
 * (`intake`); offers an edit written over a body the comment no longer holds beside the newer body
 * (`changed`); offers a margin compose's draft kept for other words of the document beside the
 * compose at these (`offered`); and splits the drafts kept for other replies into those it names
 * with the way back (`keptAway`) and those whose comment or message its host no longer lists, or
 * lists deleted (`goneAway`), which it drops, or takes into the field (`dropAway`, `takeAway`).
 *
 * The composer's own state is handed in as it stands at this render: the field (`field`), the
 * files pasted into it (`homes`), how many times the reader has moved its address with its own
 * controls (`moves`), and its send (`outgoing`). `mountedHeld` is the composer's own mark that
 * the field took a draft the reply prefill must not replace, which an arrival at an address that
 * keeps one sets again. `replaceDraft` and `replaceAsk` put a draft, and an ask's options and
 * settings, in the fields that edit them, `discard` holds the composer's own Discard, and
 * `listingOf` says how the host lists each reply.
 */
export function useKeptDrafts({
  anchor,
  composerName,
  discard,
  edit,
  field,
  homes,
  initial,
  inline,
  intake,
  kind,
  listingOf,
  mountedHeld,
  moves,
  outgoing,
  owner,
  replaceAsk,
  replaceDraft,
  replyTo,
  seedMentions,
  standing,
  stoppedAside,
  unsent,
}: ShownAddress &
  KeptUnder & {
    readonly field: {
      readonly askOptions: readonly AskOptionDraft[];
      readonly body: string;
      readonly mentions: readonly AcceptedMention[];
      readonly multiple: boolean;
      readonly replacement: string;
      readonly urgency: AskUrgency;
    };
    /** The composer's own Discard, as it stands at the latest render, which the host's handle on
     *  the field runs (`DraftIntake.drop`). */
    readonly discard: RefObject<() => void>;
    readonly homes: DraftHomes;
    readonly initial: MountDraft;
    readonly intake: Ref<DraftIntake> | undefined;
    readonly kind: ComposerKind;
    /** How the host lists the comment or message `reply` answers (`ListedReplies`): a draft kept
     *  for a reply it no longer lists, or lists deleted, is drawn itself (`goneAway`), and a
     *  refusal held for a reply the composer has left goes back to it by its Reply only while it
     *  stands (`writtenElsewhere`). */
    readonly listingOf: (reply: ReplyTarget) => ReplyListing;
    readonly mountedHeld: RefObject<boolean>;
    readonly moves: number;
    readonly outgoing: ComposerSend;
    readonly replaceAsk: (ask: SentRequest["ask"]) => void;
    readonly replaceDraft: (draft: Draft) => void;
    readonly seedMentions: readonly { readonly target: string; readonly title: string }[];
    /** The send the reader stopped at this compose's document, set aside for it, as its host
     *  offers it (`stoppedOfferOf`), or none. */
    readonly stoppedAside: OfferedDraft | undefined;
    readonly unsent: UnsentDrafts;
  }): KeptDrafts {
  const { askOptions, body, mentions, multiple, replacement, urgency } = field;
  const { awayName, unsentName } = keptNames({ composerName, inline, standing });
  const keptByAddress = unsent.useAway(awayName ?? "");
  // A draft kept for a reply the host no longer lists, or lists deleted - which takes no reply -
  // has lost the Reply its line names, so it is drawn itself beside the field (`KeptDraftOffers`),
  // saying which, and every other one with its line.
  const goneAway: GoneReplyDraft[] = [];
  const keptAway: AwayDraft[] = [];
  for (const away of keptByAddress) {
    if (sameReply(away.replyTo, replyTo)) continue;
    if (away.replyTo === null) {
      keptAway.push(away);
      continue;
    }
    const listing = listingOf(away.replyTo);
    if (listing === "standing") keptAway.push(away);
    else goneAway.push({ ...away, listing, replyTo: away.replyTo });
  }
  // Only a composer with an anchor keeps drafts by it, so only a margin compose offers one: the
  // send the reader stopped, set aside for it, first, then the newest draft kept for other words.
  // A kept draft's two presses both forget it where it is kept, since Use my draft has put it in
  // the field by then. Built once per draft kept, not on every keystroke.
  const keptOffer =
    stoppedAside === undefined ? keptForOtherWords(keptByAddress, anchor, replyTo) : undefined;
  const offered = useMemo((): OfferedDraft | undefined => {
    if (anchor === undefined) return undefined;
    if (stoppedAside !== undefined) return stoppedAside;
    if (keptOffer === undefined || awayName === undefined) return undefined;
    const forget = () => unsent.forgetAway(awayName, keptOffer);
    return { ...keptOffer, drop: forget, note: undefined, take: forget };
  }, [anchor, awayName, keptOffer, stoppedAside, unsent]);
  const [changed, setChanged] = useState<SentDraft | undefined>(initial.changed);
  /** Set as the reader ends the reply here or drops the draft (`keepNothing`), until a commit
   *  still draws this composer: a host that ends the composer with the reply - the phone thread
   *  composer, whose Cancel reply puts its draft in the docked one (`intake`) - has taken the
   *  draft, and a dropped one is gone, so nothing keeps it here as well. A host that unmounts the
   *  composer in the same press draws no commit in between, and the unmount must not keep the
   *  draft the last commit showed. */
  const keepsNothing = useRef(false);
  // What the reader wrote over a refusal goes back to the held send (`keep`, which a send still
  // out ignores), so the composer that mounts next shows it - and so a reload does: a refusal's
  // draft is the only copy of that message, and the entry it is kept in is what survives the
  // page (`write-storage.ts`). It is written as this composer unmounts, a short wait after each
  // keystroke, and as the page goes, which no unmount follows. A refusal the host has moved the
  // composer off (`elsewhere`) keeps the draft it was written with: what is typed over it there
  // is for that other address. So does an edit's refusal while it waits beside a newer body
  // (`changed`): the field shows that body, not the reader's edit.
  const keepDraft = useRef(() => {});
  keepDraft.current = () => {
    if (elsewhere !== undefined || changed !== undefined) return;
    outgoing.keep({
      ask: { multiple, options: submittedAskOptions(askOptions), urgency },
      draft: { body, mentions, replacement },
    });
  };
  useEffect(() => () => keepDraft.current(), []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the draft's own fields are the trigger; the write reads them as they stand
  useEffect(() => {
    const write = window.setTimeout(() => keepDraft.current(), KEEP_DRAFT_MS);
    return () => window.clearTimeout(write);
  }, [askOptions, body, mentions, multiple, replacement, urgency]);
  useEffect(() => {
    const keepNow = () => keepDraft.current();
    window.addEventListener("pagehide", keepNow);
    return () => window.removeEventListener("pagehide", keepNow);
  }, []);
  /** Whether a send of this composer's holds its draft: out, or refused. */
  const holdsSend = outgoing.held !== undefined;
  /** The refusal this composer holds, and whether its host has moved the composer off the address
   *  that refusal's draft lives at (`sameAddress`): another reply, a newer message. */
  const heldRefusal = outgoing.held?.status === "refused" ? outgoing.held : undefined;
  const refusedElsewhere =
    heldRefusal !== undefined &&
    !sameAddress(heldRefusal.request, { anchor, edit, owner, replyTo });
  /** What the field holds at this address before the reader writes anything: the comment an edit
   *  opens with, a reply's prefill, or what the channel seeds. */
  const freshBody =
    edit?.body ??
    (replyTo === null
      ? seededDraft(seedMentions, undefined, owner).body
      : replyPrefill(replyTo).body);
  /** Whether the field holds words the reader wrote for the address whose untouched field reads
   *  `fresh`: more than that, and other than a held refusal's own draft, which its held send
   *  keeps. Only such a draft is kept for an address the composer leaves. */
  const wroteOver = (fresh: string): boolean =>
    hasUnsavedInput(body === fresh ? "" : body, replacement, askOptions) &&
    (heldRefusal === undefined ||
      body !== heldRefusal.draft.draft.body ||
      replacement !== heldRefusal.draft.draft.replacement);
  // A draft kept for a reply its host no longer lists has no Reply to go back by, so Use it here
  // takes it into the field at the owner's own address - the issue, the session - where it goes
  // on as a message that answers none. It never writes over a draft or a send of the field's own,
  // and waits until the field holds neither.
  const awayTakes =
    replyTo !== null || edit !== undefined
      ? "nowhere"
      : holdsSend || wroteOver(freshBody)
        ? "once empty"
        : "here";
  // A draft the reader carried here from another composer goes in the field only while nothing
  // of this composer's own is there to be written over (`DraftIntake`), and a host's control for
  // a draft out of the reader's sight drops it as the composer's own Discard does. The host's
  // handle reads the field as it stands, so it is made once.
  const takeDraft = useRef<DraftIntake["take"]>(() => false);
  takeDraft.current = (draft, to) => {
    if (
      holdsSend ||
      hasUnsavedInput(body, replacement, askOptions) ||
      !sameAddress({ anchor, edit, owner, replyTo }, { ...to, anchor: undefined, edit: undefined })
    ) {
      return false;
    }
    replaceDraft(draft);
    return true;
  };
  useImperativeHandle(
    intake,
    () => ({
      drop: () => discard.current(),
      take: (draft, to) => takeDraft.current(draft, to),
    }),
    [discard]
  );
  // What the reader wrote for this address and did not send waits for it once the composer leaves
  // the page (`awayName`): its host took the composer away - the issue the reader left, the Agents
  // page, the margin's document, a newer compose on that document, a comment's card - and the
  // composer drawn at this address next shows it. A margin compose's draft is kept for the words
  // it was written about (`AwayDraft.anchor`), with the kind it was written as. An edit is kept
  // with the comment's body the field started from, so it comes back only over that body. A draft
  // a held send keeps at its own address is that send's, one the reader's Cancel reply handed to
  // the host is wherever the host put it, and an edit waiting beside a newer body stays kept as it
  // was until the reader takes it back or drops it. It answers whether it kept a draft.
  const keepAway = useRef((): boolean => false);
  keepAway.current = () => {
    if (
      awayName === undefined ||
      keepsNothing.current ||
      changed !== undefined ||
      (holdsSend && !refusedElsewhere)
    ) {
      return false;
    }
    const kept = wroteOver(freshBody);
    if (kept) {
      unsent.keepAway(awayName, {
        anchor,
        ask: { multiple, options: submittedAskOptions(askOptions), urgency },
        base: edit?.body,
        draft: { body, mentions, replacement },
        kind,
        owner,
        replyTo,
      });
    } else {
      unsent.forgetAway(awayName, { anchor, replyTo });
    }
    return kept;
  };
  useEffect(() => () => void keepAway.current(), []);
  const takeBack = useRef(() => {});
  takeBack.current = () => {
    if (awayName !== undefined) unsent.forgetAway(awayName, { anchor, replyTo });
  };
  // A reload, and the tab's closing, end the page with no unmount, so the draft is kept for its
  // address as the page goes too (`pagehide`), which the tab's storage holds over a reload
  // (`draft-store.ts`). A page the browser keeps and shows again (`pageshow`, `persisted`) still
  // holds the draft in this field, so it takes back what it kept: kept for the address it shows,
  // the draft would come back in its field again after it was sent or dropped.
  useEffect(() => {
    let keptAsThePageWent = false;
    const going = () => {
      keptAsThePageWent = keepAway.current();
    };
    const shownAgain = (event: PageTransitionEvent) => {
      if (!event.persisted || !keptAsThePageWent) return;
      keptAsThePageWent = false;
      takeBack.current();
    };
    window.addEventListener("pagehide", going);
    window.addEventListener("pageshow", shownAgain);
    return () => {
      window.removeEventListener("pagehide", going);
      window.removeEventListener("pageshow", shownAgain);
    };
  }, []);
  // An edit's body is the draft when the edit opens, and the field takes the comment again
  // whenever it changes under the open edit: another writer saved it, or a save of this editor's
  // landed. What the reader wrote over the body before is not written over with no word: unless
  // the comment now reads as the edit would save it (`changedUnder`), it waits beside the newer
  // body (`changed`), as it does when the editor comes back to a comment that changed while it
  // was away. It is kept at once where the editor looks for it when it mounts - the held refusal,
  // or the draft kept for the comment with the body it was written over - so the host can take
  // the editor away meanwhile and lose nothing. While a save of this editor's is out, the send
  // keeps the draft and the change is likely its own, so the field only takes the body.
  const editBody = edit?.body;
  const editedFrom = useRef(editBody);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the comment's body is the trigger; the field, the send and the keeps are read as they stand at the change
  useEffect(() => {
    if (editBody === undefined || editedFrom.current === editBody) return;
    const writtenOver = editedFrom.current;
    editedFrom.current = editBody;
    const written = { body, mentions, replacement };
    if (
      changed === undefined &&
      !(holdsSend && heldRefusal === undefined) &&
      body !== writtenOver &&
      changedUnder(written, writtenOver, editBody)
    ) {
      if (heldRefusal !== undefined) keepDraft.current();
      else if (awayName !== undefined) {
        unsent.keepAway(awayName, {
          anchor,
          base: writtenOver,
          draft: written,
          owner,
          replyTo,
        });
      }
      setChanged(written);
    }
    replaceDraft({ body: editBody, mentions: [] });
  }, [editBody]);
  // The unsent draft is written under its send name while no send of its own holds the draft, and
  // while the host has the composer away from a refusal's address (`refusedElsewhere`): what the
  // reader types there is that other address's, and the refusal keeps the draft it was written
  // with. From Send on the held send keeps it, so Send drops the unsent copy in its own task
  // (`forgetUnsent`): a composer drawn after that send landed must not bring back words already
  // sent.
  const keepsUnsent = wroteOver("");
  useEffect(() => {
    if (unsentName === undefined || (holdsSend && !refusedElsewhere)) return;
    unsent.write(unsentName, keepsUnsent ? { body, mentions, replacement } : undefined);
  }, [body, holdsSend, keepsUnsent, mentions, refusedElsewhere, replacement, unsent, unsentName]);
  // A refusal lives at the address its send went to, or the one the reader has carried it to
  // since (`sameAddress`; the own moves below). A host can move the composer to another while it
  // holds one - another row or thread, a Reply on a message - and from there neither Send nor
  // Retry is that refusal's Retry: Send says where the draft was written for, and Discard drops
  // it. The store refuses such a press too (`HeldSends.send`), whichever render made it.
  const elsewhere =
    heldRefusal !== undefined && refusedElsewhere
      ? writtenElsewhere(heldRefusal.request, standing, listingOf)
      : undefined;
  // The reader's own moves of the composer's address - Cancel reply, and a control its host
  // draws for it (`ownMoves`) - take the draft with them, and what it holds: its refusal
  // (`HeldSends.carry`) and the files pasted into it (`DraftHomes.carry`). Each is counted in the
  // press that makes it, so a commit that shows another address with another count is the
  // reader's move. One that shows another address with the count where it was is the host's -
  // another row, another thread, a Reply on a message - and carries nothing: a refusal and a file
  // stay with the address they were made for, and the composer says so (`elsewhere`, `landedFor`,
  // `pastedElsewhere`). Only what lives where the composer was goes along, so a move away from an
  // address the host took the composer to leaves the draft where it was written.
  //
  // Each reply is an address of its own for what the field holds. A host's move to another reply
  // keeps the field's draft for the one it leaves - in its refusal, where it lives there, else
  // under the send name for that reply (`awayName`), which the composer says (`keptAwayLine`) -
  // and puts in the field what the reply it arrives at keeps: its refusal's draft, a draft kept
  // for it, the refusal the host moved it off, or else what a new reply starts with. The
  // reader's own move onto a reply that keeps a draft of its own - Cancel reply back to a
  // message kept for the issue, or to a refusal's own address - carries nothing either, as the
  // phone thread composer's Cancel reply carries nothing over the docked composer's own draft:
  // nothing is written over, and each draft stays where it was written.
  const shown = useRef<
    | {
        readonly address: Address;
        /** The address's untouched field (`freshBody`). */
        readonly fresh: string;
        readonly moves: number;
        readonly uploadOwner: ArtifactOwner | undefined;
      }
    | undefined
  >(undefined);
  useLayoutEffect(() => {
    const address: Address = { anchor, edit, owner, replyTo };
    const before = shown.current;
    shown.current = { address, fresh: freshBody, moves, uploadOwner: homes.uploadOwner };
    // A commit that still draws this composer after the reader's Cancel reply or Discard: the
    // host kept the composer, and what the field holds now is still this composer's to keep.
    keepsNothing.current = false;
    if (before === undefined) {
      // The field took the draft kept for this address, which is kept there no longer.
      if (initial.away && awayName !== undefined) {
        unsent.forgetAway(awayName, { anchor, replyTo });
      }
      return;
    }
    if (sameAddress(before.address, address)) return;
    const refusalAt = (at: Address) =>
      heldRefusal !== undefined && sameAddress(heldRefusal.request, at);
    // An owner or an anchor changing alone leaves the field's draft where it is: an issue pick,
    // and a margin compose's own kind switch or the mark the margin writes again for it, which
    // re-address the same compose. A newer compose on the document is a composer of its own
    // (`MarginSheet` keys the margin's composer by its compose), whose draft stays kept for its
    // own words.
    const replyMoved = !sameReply(before.address.replyTo, replyTo);
    const sendOut = holdsSend && heldRefusal === undefined;
    const kept =
      replyMoved && awayName !== undefined && !sendOut
        ? unsent.readAway(awayName, replyTo, anchor)
        : undefined;
    const arrivesAtOwn = replyMoved && !sendOut && (refusalAt(address) || kept !== undefined);
    if (before.moves !== moves && !arrivesAtOwn) {
      outgoing.carry(before.address, { owner, replyTo });
      homes.carry(before.uploadOwner);
      return;
    }
    if (!replyMoved || sendOut) return;
    const leaving = { body, mentions, replacement };
    const ask = { multiple, options: submittedAskOptions(askOptions), urgency };
    if (refusalAt(before.address)) {
      // Written now, since the debounced write reads the address the composer shows by then.
      outgoing.keep({ ask, draft: leaving });
    } else if (awayName !== undefined) {
      if (wroteOver(before.fresh)) {
        unsent.keepAway(awayName, {
          anchor: before.address.anchor,
          ask,
          draft: leaving,
          kind,
          owner: before.address.owner,
          replyTo: before.address.replyTo,
        });
      } else {
        unsent.forgetAway(awayName, before.address);
      }
    }
    if (awayName === undefined) return;
    if (kept !== undefined) unsent.forgetAway(awayName, { anchor, replyTo });
    const next = refusalAt(address)
      ? heldRefusal?.draft.draft
      : (kept?.draft ??
        heldRefusal?.draft.draft ??
        (replyTo === null ? seededDraft(seedMentions, undefined, owner) : undefined));
    // Arriving at a reply that keeps nothing, with no refusal held, is a new reply: the prefill
    // fills the field.
    if (next === undefined) return;
    replaceDraft(next);
    mountedHeld.current = true;
  });
  return {
    awayTakes,
    changed,
    dropAway: (away) => {
      if (awayName !== undefined) {
        unsent.forgetAway(awayName, { anchor: away.anchor, replyTo: away.replyTo });
      }
    },
    dropChanged: () => {
      if (changed !== undefined && awayName !== undefined) {
        unsent.forgetAway(awayName, { anchor, replyTo });
      }
      setChanged(undefined);
    },
    dropOffered: () => offered?.drop(),
    elsewhere,
    forgetUnsent: () => {
      if (unsentName !== undefined) unsent.write(unsentName, undefined);
    },
    goneAway,
    keepDraft: () => keepDraft.current(),
    keepNothing: () => {
      keepsNothing.current = true;
    },
    keptAway,
    offered,
    takeAway: (away) => {
      if (awayTakes !== "here" || awayName === undefined) return;
      replaceDraft({
        ...seededDraft(seedMentions, away.draft, owner),
        replacement: away.draft.replacement,
      });
      unsent.forgetAway(awayName, { anchor: away.anchor, replyTo: away.replyTo });
    },
    takeChanged: () => {
      if (changed === undefined) return;
      replaceDraft(changed);
      setChanged(undefined);
      if (awayName !== undefined) unsent.forgetAway(awayName, { anchor, replyTo });
    },
    takeOffered: () => {
      if (offered === undefined || anchor === undefined) return;
      replaceDraft(offered.draft);
      if (offered.ask !== undefined) replaceAsk(offered.ask);
      offered.take({ anchor, kind });
    },
  };
}
