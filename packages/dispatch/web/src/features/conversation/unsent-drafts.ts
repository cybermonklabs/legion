import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";

import {
  answerDraftsSignedIn,
  answerDraftsSignedOut,
  reloadAnswerDrafts,
} from "../inbox/answer-drafts";
import { createDraftStore } from "../inbox/draft-store";
import {
  reloadReplyDrafts,
  replyDraftsSignedIn,
  replyDraftsSignedOut,
} from "../inbox/reply-drafts";
import { type Shape, shape } from "../writes/tab-storage";
import type { ComposerAnchor, ComposerKind, ComposerOwner, ReplyTarget } from "./composer-model";
import {
  type HeldSend,
  type KeptDraft,
  keptAnchor,
  keptAsk,
  keptDraft,
  keptKind,
  keptOwner,
  keptReplyTo,
  keptSeeds,
  keptStatement,
  restoredDraft,
} from "./held-sends";
import {
  type SentDraft,
  type SentRequest,
  type StatementAnchor,
  sameStatement,
} from "./send-request";

/**
 * A draft a composer whose address its host moves - the Conversation's docked composer, a phone
 * thread's, an Agents row's, the margin's compose, a comment's editor, the issue page's box -
 * keeps for the address it was written at, once the field no longer shows it: its host moved the
 * composer to another reply, or took it off the page. The address is the reply the draft answers,
 * or none, with the owner it was written for, which is what the composer's line about it names,
 * for a margin compose the words of the document it was written about, and for the issue page's
 * box the statement it comments on. An editor's address is the comment it edits, which its send
 * name names.
 */
export interface AwayDraft {
  /** For a margin compose, the words of the document it was written about, as the compose named
   *  them: the draft comes back in the field only at those words, and beside a compose at other
   *  words it waits until the reader takes it or drops it (`useKeptDrafts`'s `offered`). */
  readonly anchor?: ComposerAnchor;
  /** An ask's settings and options, which a compose of the ask kind keeps with its question. */
  readonly ask?: SentRequest["ask"];
  /** For an edit, the comment's body it was written over, as the editor last saw it: the edit is
   *  put back in the field only while the comment still holds that body, or reads as the edit
   *  would save it, and is offered beside the newer body otherwise (`useKeptDrafts`'s
   *  `changedUnder`). */
  readonly base?: string;
  readonly draft: SentDraft;
  /** The kind the draft was written as - a comment, a suggestion or an ask - which a compose of
   *  another kind it is offered beside names, so what it brings back is never hidden with no
   *  word: an ask's options show only under Ask. */
  readonly kind?: ComposerKind;
  readonly owner: ComposerOwner;
  readonly replyTo: ReplyTarget | null;
  /** For the issue page's box, the mentions it wrote in front of the reply's draft
   *  (`SentRequest.seeds`), so the reader's own move off that reply, once the draft is back in the
   *  field, takes out those and no link the reader wrote; absent where it wrote none. */
  readonly seeds?: SentRequest["seeds"];
  /** For the issue page's box, the statement a comment with no reply is on
   *  (`SentRequest.statement`): the draft comes back in the field only at that statement. */
  readonly statement?: StatementAnchor;
}

/** The address a draft kept by address is kept for (`AwayDraft`): the reply it answers, or none,
 *  for a margin compose the words of the document it was written about, or none for every other
 *  composer, and for the issue page's box the statement it is on, or none. The reply and the
 *  words are named, so a forget says which words it forgets; a composer that names no statement
 *  forgets the draft that is on none. */
export interface AwayAddress {
  readonly anchor: ComposerAnchor | undefined;
  readonly replyTo: ReplyTarget | null;
  readonly statement?: StatementAnchor;
}

/** What a held send's draft is kept as once the send is gone (`AwayDraft`): its text and a
 *  suggestion's replacement, and an ask's fields, for the address it went to - its owner, the
 *  reply it answered, a margin compose's words and the box's statement - in the kind it went out
 *  as. */
export function awayDraftOf(held: HeldSend): AwayDraft {
  const { anchor, kind, owner, replyTo, statement } = held.request;
  return { anchor, ask: held.draft.ask, draft: held.draft.draft, kind, owner, replyTo, statement };
}

/** Whether two drafts answer the same reply, or both answer none. */
export function sameReply(a: ReplyTarget | null, b: ReplyTarget | null): boolean {
  return (a?.id ?? null) === (b?.id ?? null);
}

/** Whether two drafts are for the same target: they answer the same reply and are on the same
 *  statement (`sameStatement`), or answer none or are on none. */
export function sameTarget(
  a: Pick<AwayAddress, "replyTo" | "statement">,
  b: Pick<AwayAddress, "replyTo" | "statement">
): boolean {
  return sameReply(a.replyTo, b.replyTo) && sameStatement(a.statement, b.statement);
}

/** Whether a draft kept at `away` is the one kept for `at`: the same target and the same words. */
function sameAway(
  away: Pick<AwayDraft, "anchor" | "replyTo" | "statement">,
  at: Pick<AwayDraft, "anchor" | "replyTo" | "statement">
): boolean {
  return sameTarget(away, at) && sameAnchor(away.anchor, at.anchor);
}

/** Whether `a` and `b` name the same words of the same document: the same mark, or, where neither
 *  names one (a closed issue's document, which takes no mark), the same quote at the same reading
 *  of it. A composer with no anchor has none on either side. A kind switch writes the mark again
 *  under a new id, and the margin re-addresses that compose to it, so a draft the compose keeps
 *  is kept under the anchor it shows when it goes. */
export function sameAnchor(a: ComposerAnchor | undefined, b: ComposerAnchor | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  return (
    a.artifact === b.artifact &&
    a.mark_id === b.mark_id &&
    (a.mark_id !== undefined || (a.quote === b.quote && a.occurrence === b.occurrence))
  );
}

/**
 * What the reader typed into a standing box (`MentionComposer`'s `standing`), or into an inline
 * reply, and has not sent, by the composer's send name. It is a draft store of the kind the ask's
 * answer and reply drafts are (`draft-store.ts`): the draft belongs to the thread the composer is
 * named for, not to the field showing it. The Inbox's column draws one box per thread and a new
 * one for each row it moves to, and a thread card mounts its reply composer whenever it opens, so
 * what was typed for a thread waits under that thread's name, comes back when the reader returns
 * to it, and never shows in another thread's composer. A draft is kept whole: who it addresses
 * is in its text (R4), so nothing beside the words is kept with it.
 *
 * A composer whose host moves its address keeps a draft by the address too (`AwayDraft`): under
 * its send name, one per reply it was written for, so the reply typed for one message waits for
 * that message while the same composer answers another, and comes back with that message's Reply.
 * The margin's compose keeps one per quote of its document, so text written about one quote never
 * opens in a compose at another, and the issue page's box one per statement it comments on. An
 * editor keeps one with the comment's body it was written over, which decides whether the edit
 * comes back in the field or beside a newer body.
 */
export interface UnsentDrafts {
  read(name: string): SentDraft | undefined;
  /** Whether a draft is kept under `name`, re-rendering whenever that changes: a thread card
   *  whose reply composer is off the page says it keeps the reply's draft. */
  useKept(name: string): boolean;
  /** Who is signed in now: another person's sign-in forgets every draft in both stores, the
   *  drafts kept by address with them, in the page and in the tab, since each is the person's who
   *  typed it (`DraftStore.signedIn`); the first sign-in takes up the drafts the tab kept for that
   *  person before a reload. */
  signedIn(login: string | undefined): void;
  /** `login`'s sign-out landed: both stores forget every draft of theirs, the drafts kept by
   *  address with them, in the page and in the tab, and take none until someone signs in, so
   *  whoever signs in next at this browser finds nothing they typed (`DraftStore.signedOut`). */
  signedOut(login: string): void;
  /** Keeps `draft` under `name`, or, given none, forgets the draft there. */
  write(name: string, draft: SentDraft | undefined): void;
  /** The draft kept under `name` for the reply `replyTo` answers, or for none, at the words
   *  `anchor` names, or at none, and on the statement `statement` names, or on none. */
  readAway(
    name: string,
    replyTo: ReplyTarget | null,
    anchor?: ComposerAnchor,
    statement?: StatementAnchor
  ): AwayDraft | undefined;
  /** Every draft kept under `name` by its address, re-rendering whenever they change: the
   *  composer says which are kept and how to go back to each. */
  useAway(name: string): readonly AwayDraft[];
  /** Keeps `away` under `name` for its address (`AwayAddress`), in place of what is kept there. */
  keepAway(name: string, away: AwayDraft): void;
  /** Forgets what is kept under `name` for the address `at` names. */
  forgetAway(name: string, at: AwayAddress): void;
}

/** A draft kept by address as the tab holds it: its draft as the shape an older record may still
 *  hold (`KeptDraft`). */
type KeptAway = Omit<AwayDraft, "draft"> & { readonly draft: KeptDraft };

/** A draft kept by address, as the tab keeps it for a reload (`AwayDraft`). */
const keptAway = shape.record({
  anchor: shape.optional(keptAnchor),
  ask: shape.optional(keptAsk),
  base: shape.optional(shape.string),
  draft: keptDraft,
  kind: shape.optional(keptKind),
  owner: keptOwner,
  replyTo: shape.nullable(keptReplyTo),
  seeds: shape.optional(keptSeeds),
  statement: shape.optional(keptStatement),
}) satisfies Shape<KeptAway>;

/** What a store holds under a name, read as JSON: undefined where it holds nothing, or text that
 *  does not parse. The tab can hold that text, or a draft an older release kept in another shape,
 *  so what is read here is trusted only once its shape is checked (`keptAway`, `keptDraft`), and
 *  what does not fit reads as no draft. A draft that fits is read back as a held send's is
 *  (`restoredDraft`): one the build before this one kept holds its mentions in an array beside
 *  a body that only draws them, and they come back as the mentions in the text that name them. */
function parsed(kept: string): unknown {
  if (kept === "") return undefined;
  try {
    return JSON.parse(kept);
  } catch {
    return undefined;
  }
}

/** The drafts kept by address under a name: those of its list that fit `keptAway`. */
function parseAway(kept: string): readonly AwayDraft[] {
  const list = parsed(kept);
  return Array.isArray(list)
    ? list.filter(keptAway).map((away) => ({ ...away, draft: restoredDraft(away.draft) }))
    : [];
}

/** The draft kept under a name, where it fits `keptDraft`. */
function parseSent(kept: string): SentDraft | undefined {
  const draft = parsed(kept);
  return keptDraft(draft) ? restoredDraft(draft) : undefined;
}

const drafts = new WeakMap<QueryClient, UnsentDrafts>();

/** The unsent drafts that live with the query client, as the held sends do (`heldSends`): a draft
 *  store holds text, so each draft is kept as its JSON, and the drafts a composer keeps by address
 *  as one JSON list under its send name, in a store of their own. The tab keeps both for a reload
 *  of the page, under `unsent` and `away` (`draft-store.ts`), as it keeps the held sends; the
 *  page has one query client, so the next load's client takes up what this one kept. */
export function unsentDraftsOf(client: QueryClient): UnsentDrafts {
  let unsent = drafts.get(client);
  if (unsent === undefined) {
    const store = createDraftStore("unsent");
    const awayStore = createDraftStore("away");
    function useKept(name: string): boolean {
      const [kept] = store.useDraft(name);
      return useMemo(() => parseSent(kept) !== undefined, [kept]);
    }
    function useAway(name: string): readonly AwayDraft[] {
      const [kept] = awayStore.useDraft(name);
      return useMemo(() => parseAway(kept), [kept]);
    }
    // A draft is kept by the reply part of its address, the words it is about and the statement
    // it is on: one per reply, one for a message that answers none, one per quote of a margin
    // compose, and one per statement of the issue page's box.
    const writeAway = (
      name: string,
      at: Pick<AwayDraft, "anchor" | "replyTo" | "statement">,
      away: AwayDraft | undefined
    ) =>
      awayStore.set(name, (kept) => {
        const others = parseAway(kept).filter((other) => !sameAway(other, at));
        const next = away === undefined ? others : [...others, away];
        return next.length === 0 ? "" : JSON.stringify(next);
      });
    unsent = {
      forgetAway: (name, at) => writeAway(name, at, undefined),
      keepAway: (name, away) => writeAway(name, away, away),
      read: (name) => parseSent(store.read(name)),
      readAway: (name, replyTo, anchor, statement) =>
        parseAway(awayStore.read(name)).find((away) =>
          sameAway(away, { anchor, replyTo, statement })
        ),
      signedIn: (login) => {
        store.signedIn(login);
        awayStore.signedIn(login);
      },
      signedOut: (login) => {
        store.signedOut(login);
        awayStore.signedOut(login);
      },
      useAway,
      useKept,
      write: (name, draft) => store.set(name, draft === undefined ? "" : JSON.stringify(draft)),
    };
    drafts.set(client, unsent);
  }
  return unsent;
}

/** The unsent drafts of the query client this component renders under (`unsentDraftsOf`). */
export function useUnsentDrafts(): UnsentDrafts {
  return unsentDraftsOf(useQueryClient());
}

/** Who is signed in now, told to every draft store of the page: the composers' drafts and the
 *  ones each keeps by address, which live with `client` (`unsentDraftsOf`), and the asks' answers
 *  and replies (`answer-drafts.ts`, `reply-drafts.ts`). Another person's sign-in forgets every
 *  draft, a lapse keeps them, and the first sign-in takes up what the tab kept for that person
 *  before a reload (`DraftStore.signedIn`). */
export function draftStoresSignedIn(client: QueryClient, login: string | undefined): void {
  unsentDraftsOf(client).signedIn(login);
  answerDraftsSignedIn(login);
  replyDraftsSignedIn(login);
}

/** `login`'s sign-out landed, told to every draft store of the page (`draftStoresSignedIn`):
 *  each forgets every draft of theirs, in the page and in the tab, and takes none until someone
 *  signs in (`DraftStore.signedOut`). */
export function draftStoresSignedOut(client: QueryClient, login: string): void {
  unsentDraftsOf(client).signedOut(login);
  answerDraftsSignedOut(login);
  replyDraftsSignedOut(login);
}

/** What a reload of the page does to the draft stores modules hold, the asks' answers and
 *  replies, for the draft matrix: the page forgets them, and the tab keeps them for the next
 *  sign-in (`DraftStore.reload`). The composers' drafts live with the page's query client, which
 *  the next load makes anew. */
export function draftStoresReloaded(): void {
  reloadAnswerDrafts();
  reloadReplyDrafts();
}
