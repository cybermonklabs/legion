import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";

import { createDraftStore } from "../inbox/draft-store";
import type { ComposerOwner, ReplyTarget } from "./composer-model";
import type { SentDraft, SentRequest } from "./send-request";

/**
 * A draft a composer whose address its host moves - the Conversation's docked composer, a phone
 * thread's, an Agents row's, the margin's compose - keeps for the address it was written at, once
 * the field no longer shows it: its host moved the composer to another reply, or took it off the
 * page. The address is the reply the draft answers, or none, with the owner it was written for,
 * which is what the composer's line about it names.
 */
export interface AwayDraft {
  /** An ask's settings and options, which a compose of the ask kind keeps with its question. */
  readonly ask?: SentRequest["ask"];
  readonly draft: SentDraft;
  readonly owner: ComposerOwner;
  readonly replyTo: ReplyTarget | null;
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
 */
export interface UnsentDrafts {
  read(name: string): SentDraft | undefined;
  /** Whether a draft is kept under `name`, re-rendering whenever that changes: a thread card
   *  whose reply composer is off the page says it keeps the reply's draft. */
  useKept(name: string): boolean;
  /** Keeps `draft` under `name`, or, given none, forgets the draft there. */
  write(name: string, draft: SentDraft | undefined): void;
  /** The draft kept under `name` for the reply `replyTo` answers, or for none. */
  readAway(name: string, replyTo: ReplyTarget | null): AwayDraft | undefined;
  /** Every draft kept under `name` by its address, re-rendering whenever they change: the
   *  composer says which are kept and how to go back to each. */
  useAway(name: string): readonly AwayDraft[];
  /** Keeps `away` under `name` for its address, or forgets what is kept for `replyTo` there. */
  writeAway(name: string, replyTo: ReplyTarget | null, away: AwayDraft | undefined): void;
}

function parseAway(kept: string): readonly AwayDraft[] {
  return kept === "" ? [] : (JSON.parse(kept) as AwayDraft[]);
}

const drafts = new WeakMap<QueryClient, UnsentDrafts>();

/** The unsent drafts that live with the query client, as the held sends do (`heldSends`): a draft
 *  store holds text, so each draft is kept as its JSON, and the drafts a composer keeps by address
 *  as one JSON list under its send name, in a store of their own. */
export function useUnsentDrafts(): UnsentDrafts {
  const client = useQueryClient();
  let unsent = drafts.get(client);
  if (unsent === undefined) {
    const store = createDraftStore();
    const awayStore = createDraftStore();
    function useKept(name: string): boolean {
      return store.useDraft(name)[0] !== "";
    }
    function useAway(name: string): readonly AwayDraft[] {
      const [kept] = awayStore.useDraft(name);
      return useMemo(() => parseAway(kept), [kept]);
    }
    unsent = {
      read: (name) => {
        const kept = store.read(name);
        return kept === "" ? undefined : (JSON.parse(kept) as SentDraft);
      },
      // A draft is kept by the reply part of its address: one per reply, and one for a message
      // that answers none.
      readAway: (name, replyTo) =>
        parseAway(awayStore.read(name)).find(
          (away) => (away.replyTo?.id ?? null) === (replyTo?.id ?? null)
        ),
      useAway,
      useKept,
      write: (name, draft) => store.set(name, draft === undefined ? "" : JSON.stringify(draft)),
      writeAway: (name, replyTo, away) =>
        awayStore.set(name, (kept) => {
          const others = parseAway(kept).filter(
            (other) => (other.replyTo?.id ?? null) !== (replyTo?.id ?? null)
          );
          const next = away === undefined ? others : [...others, away];
          return next.length === 0 ? "" : JSON.stringify(next);
        }),
    };
    drafts.set(client, unsent);
  }
  return unsent;
}
