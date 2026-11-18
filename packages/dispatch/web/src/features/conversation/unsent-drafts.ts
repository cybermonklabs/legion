import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";

import { createDraftStore } from "../inbox/draft-store";
import type { ComposerAnchor, ComposerKind, ComposerOwner, ReplyTarget } from "./composer-model";
import type { HeldSend } from "./held-sends";
import type { SentDraft, SentRequest } from "./send-request";

/**
 * A draft a composer whose address its host moves - the Conversation's docked composer, a phone
 * thread's, an Agents row's, the margin's compose, a comment's editor - keeps for the address it
 * was written at, once the field no longer shows it: its host moved the composer to another
 * reply, or took it off the page. The address is the reply the draft answers, or none, with the
 * owner it was written for, which is what the composer's line about it names, and, for a margin
 * compose, the words of the document it was written about. An editor's address is the comment it
 * edits, which its send name names.
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
}

/** The address a draft kept by address is kept for (`AwayDraft`): the reply it answers, or none,
 *  and for a margin compose the words of the document it was written about, or none for every
 *  other composer. Both are named, so a forget says which words it forgets. */
export interface AwayAddress {
  readonly anchor: ComposerAnchor | undefined;
  readonly replyTo: ReplyTarget | null;
}

/** What a held send's draft is kept as once the send is gone (`AwayDraft`): its text, its
 *  accepted mentions and a suggestion's replacement, and an ask's fields, for the address it went
 *  to - its owner, the reply it answered and a margin compose's words - in the kind it went out
 *  as. */
export function awayDraftOf(held: HeldSend): AwayDraft {
  const { anchor, kind, owner, replyTo } = held.request;
  return { anchor, ask: held.draft.ask, draft: held.draft.draft, kind, owner, replyTo };
}

/** Whether two drafts answer the same reply, or both answer none. */
export function sameReply(a: ReplyTarget | null, b: ReplyTarget | null): boolean {
  return (a?.id ?? null) === (b?.id ?? null);
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
 * to it, and never shows in another thread's composer. A draft is kept whole, its accepted
 * mentions with it, since those are what a reply's mentions are sent from.
 *
 * A composer whose host moves its address keeps a draft by the address too (`AwayDraft`): under
 * its send name, one per reply it was written for, so the reply typed for one message waits for
 * that message while the same composer answers another, and comes back with that message's Reply.
 * The margin's compose keeps one per quote of its document, so text written about one quote never
 * opens in a compose at another. An editor keeps one with the comment's body it was written over,
 * which decides whether the edit comes back in the field or beside a newer body.
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
  /** Keeps `draft` under `name`, or, given none, forgets the draft there. */
  write(name: string, draft: SentDraft | undefined): void;
  /** The draft kept under `name` for the reply `replyTo` answers, or for none, at the words
   *  `anchor` names, or at none. */
  readAway(
    name: string,
    replyTo: ReplyTarget | null,
    anchor?: ComposerAnchor
  ): AwayDraft | undefined;
  /** Every draft kept under `name` by its address, re-rendering whenever they change: the
   *  composer says which are kept and how to go back to each. */
  useAway(name: string): readonly AwayDraft[];
  /** Keeps `away` under `name` for its address (`AwayAddress`), in place of what is kept there. */
  keepAway(name: string, away: AwayDraft): void;
  /** Forgets what is kept under `name` for the address `at` names. */
  forgetAway(name: string, at: AwayAddress): void;
}

function parseAway(kept: string): readonly AwayDraft[] {
  return kept === "" ? [] : (JSON.parse(kept) as AwayDraft[]);
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
      return store.useDraft(name)[0] !== "";
    }
    function useAway(name: string): readonly AwayDraft[] {
      const [kept] = awayStore.useDraft(name);
      return useMemo(() => parseAway(kept), [kept]);
    }
    // A draft is kept by the reply part of its address and the words it is about: one per reply,
    // one for a message that answers none, and one per quote of a margin compose.
    const writeAway = (name: string, at: AwayAddress, away: AwayDraft | undefined) =>
      awayStore.set(name, (kept) => {
        const others = parseAway(kept).filter(
          (other) => !(sameReply(other.replyTo, at.replyTo) && sameAnchor(other.anchor, at.anchor))
        );
        const next = away === undefined ? others : [...others, away];
        return next.length === 0 ? "" : JSON.stringify(next);
      });
    unsent = {
      forgetAway: (name, at) => writeAway(name, at, undefined),
      keepAway: (name, away) =>
        writeAway(name, { anchor: away.anchor, replyTo: away.replyTo }, away),
      read: (name) => {
        const kept = store.read(name);
        return kept === "" ? undefined : (JSON.parse(kept) as SentDraft);
      },
      readAway: (name, replyTo, anchor) =>
        parseAway(awayStore.read(name)).find(
          (away) => sameReply(away.replyTo, replyTo) && sameAnchor(away.anchor, anchor)
        ),
      signedIn: (login) => {
        store.signedIn(login);
        awayStore.signedIn(login);
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
