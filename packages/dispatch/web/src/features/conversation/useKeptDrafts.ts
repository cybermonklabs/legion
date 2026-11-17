import {
  type Ref,
  type RefObject,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import type { ArtifactOwner } from "../../api/client";
import type { AskUrgency } from "../../api/types";
import { textareaValue } from "./block-spans";
import {
  type AskOptionDraft,
  type Draft,
  replyPrefill,
  seededDraft,
  submittedAskOptions,
} from "./composer-draft";
import { type AcceptedMention, type DraftIntake, hasUnsavedInput } from "./composer-model";
import type { HeldSend } from "./held-sends";
import {
  type Address,
  addressWords,
  type SentDraft,
  type SentRequest,
  sameAddress,
} from "./send-request";
import type { AwayDraft, UnsentDrafts } from "./unsent-drafts";
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

/** What Send says while the refusal the composer holds lives at another address than the one it
 *  shows now, since its host moved it (`sameAddress`): where the draft was written for, and the
 *  ways out - back there, where Retry sends it, or Discard. A standing box answers what its host
 *  gives it and no Reply the reader presses (`standing`): an Inbox agent thread's box replies to
 *  the thread's newest message, so one that has come since the refusal moved the box, and no
 *  press brings it back. */
function writtenElsewhere(held: SentRequest, standing: boolean): string {
  if (held.replyTo !== null) {
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
 * newer body, and waits beside it (`changedUnder`, `changed`).
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
        : unsent.readAway(awayName, replyTo)
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

/** What the composer keeps of a draft the field does not send, and the edit it offers beside a
 *  newer body. */
export interface KeptDrafts {
  /** An edit written over a body the comment no longer holds (`changedUnder`): refused or kept
   *  for the comment while its editor was off the page, or written in the open editor when another
   *  writer changed the comment under it. It waits beside the newer body the field shows until
   *  the reader takes it back into the field (`takeChanged`) or drops it (`dropChanged`).
   *  Meanwhile the field is read only and Save refuses, so nothing is saved over a body the reader
   *  has not seen, and nothing they wrote goes with no word. */
  readonly changed: SentDraft | undefined;
  /** Drops the edit waiting beside a newer body, with what is kept of it for the comment: Discard
   *  drops the draft the reader wrote there. */
  readonly dropChanged: () => void;
  /** What Send says while the refusal the composer holds lives at another address than the one
   *  it shows (`writtenElsewhere`), else undefined. */
  readonly elsewhere: string | undefined;
  /** Forgets the unsent copy under the send name: from Send on the held send keeps the draft. */
  readonly forgetUnsent: () => void;
  /** Keeps nothing for this address as the composer goes, until a commit still draws it
   *  (`keepsNothing`). */
  readonly keepNothing: () => void;
  /** The drafts kept for other addresses than the one the composer shows, each of which the
   *  composer names with the way back to it. */
  readonly keptAway: readonly AwayDraft[];
  /** Takes an edit waiting beside a newer body back into the field, which the reader saves with
   *  their own press, having had the newer body before them. It is the field's draft from here
   *  on, kept over the body it now replaces should the host take the editor away again, and a
   *  refusal's Retry sends it. */
  readonly takeChanged: () => void;
}

/**
 * Keeps what the reader wrote in a composer and did not send for the address it was written at,
 * as the host moves the composer from address to address or takes it off the page, and puts back
 * what an address keeps when the composer arrives there; takes a draft the reader carried here
 * from another composer (`intake`); and offers an edit written over a body the comment no longer
 * holds beside the newer body (`changed`).
 *
 * The composer's own state is handed in as it stands at this render: the field (`field`), the
 * files pasted into it (`homes`), how many times the reader has moved its address with its own
 * controls (`moves`), and the held-send keep its refusal is written back through (`keepDraft`).
 * `mountedHeld` is the composer's own mark that the field took a draft the reply prefill must not
 * replace, which an arrival at an address that keeps one sets again.
 */
export function useKeptDrafts({
  anchor,
  composerName,
  edit,
  field,
  homes,
  initial,
  inline,
  intake,
  keepDraft,
  mountedHeld,
  moves,
  outgoing,
  owner,
  replaceDraft,
  replyTo,
  seedMentions,
  standing,
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
    readonly homes: DraftHomes;
    readonly initial: MountDraft;
    readonly intake: Ref<DraftIntake> | undefined;
    readonly keepDraft: RefObject<() => void>;
    readonly mountedHeld: RefObject<boolean>;
    readonly moves: number;
    readonly outgoing: ComposerSend;
    readonly replaceDraft: (draft: Draft) => void;
    readonly seedMentions: readonly { readonly target: string; readonly title: string }[];
    readonly unsent: UnsentDrafts;
  }): KeptDrafts {
  const { askOptions, body, mentions, multiple, replacement, urgency } = field;
  const { awayName, unsentName } = keptNames({ composerName, inline, standing });
  const keptAway = unsent
    .useAway(awayName ?? "")
    .filter((away) => (away.replyTo?.id ?? null) !== (replyTo?.id ?? null));
  const [changed, setChanged] = useState<SentDraft | undefined>(initial.changed);
  /** Set as the reader ends the reply here or drops the draft (`keepNothing`), until a commit
   *  still draws this composer: a host that ends the composer with the reply - the phone thread
   *  composer, whose Cancel reply puts its draft in the docked one (`intake`) - has taken the
   *  draft, and a dropped one is gone, so nothing keeps it here as well. A host that unmounts the
   *  composer in the same press draws no commit in between, and the unmount must not keep the
   *  draft the last commit showed. */
  const keepsNothing = useRef(false);
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
  // A draft the reader carried here from another composer goes in the field only while nothing
  // of this composer's own is there to be written over (`DraftIntake`). The host's handle reads
  // the field as it stands, so it is made once.
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
  useImperativeHandle(intake, () => ({ take: (draft, to) => takeDraft.current(draft, to) }), []);
  // What the reader wrote for this address and did not send waits for it once the composer leaves
  // the page (`awayName`): its host took the composer away - the tab, the Agents page, the
  // margin's document, a comment's card - and the composer drawn at this address next shows it.
  // An edit is kept with the comment's body the field started from, so it comes back only over
  // that body. A draft a held send keeps at its own address is that send's, one the reader's
  // Cancel reply handed to the host is wherever the host put it, and an edit waiting beside a
  // newer body stays kept as it was until the reader takes it back or drops it.
  const keepAway = useRef(() => {});
  keepAway.current = () => {
    if (
      awayName === undefined ||
      keepsNothing.current ||
      changed !== undefined ||
      (holdsSend && !refusedElsewhere)
    ) {
      return;
    }
    unsent.writeAway(
      awayName,
      replyTo,
      wroteOver(freshBody)
        ? {
            ask: { multiple, options: submittedAskOptions(askOptions), urgency },
            base: edit?.body,
            draft: { body, mentions, replacement },
            owner,
            replyTo,
          }
        : undefined
    );
  };
  useEffect(() => () => keepAway.current(), []);
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
        unsent.writeAway(awayName, replyTo, { base: writtenOver, draft: written, owner, replyTo });
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
      ? writtenElsewhere(heldRefusal.request, standing)
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
      if (initial.away && awayName !== undefined) unsent.writeAway(awayName, replyTo, undefined);
      return;
    }
    if (sameAddress(before.address, address)) return;
    const refusalAt = (at: Address) =>
      heldRefusal !== undefined && sameAddress(heldRefusal.request, at);
    // An owner or an anchor changing alone - an issue pick, a newer compose on the same
    // document - leaves the field's draft where it is.
    const replyMoved = (before.address.replyTo?.id ?? null) !== (replyTo?.id ?? null);
    const sendOut = holdsSend && heldRefusal === undefined;
    const kept =
      replyMoved && awayName !== undefined && !sendOut
        ? unsent.readAway(awayName, replyTo)
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
      unsent.writeAway(
        awayName,
        before.address.replyTo,
        wroteOver(before.fresh)
          ? { ask, draft: leaving, owner: before.address.owner, replyTo: before.address.replyTo }
          : undefined
      );
    }
    if (awayName === undefined) return;
    if (kept !== undefined) unsent.writeAway(awayName, replyTo, undefined);
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
    changed,
    dropChanged: () => {
      if (changed !== undefined && awayName !== undefined) {
        unsent.writeAway(awayName, replyTo, undefined);
      }
      setChanged(undefined);
    },
    elsewhere,
    forgetUnsent: () => {
      if (unsentName !== undefined) unsent.write(unsentName, undefined);
    },
    keepNothing: () => {
      keepsNothing.current = true;
    },
    keptAway,
    takeChanged: () => {
      if (changed === undefined) return;
      replaceDraft(changed);
      setChanged(undefined);
      if (awayName !== undefined) unsent.writeAway(awayName, replyTo, undefined);
    },
  };
}
