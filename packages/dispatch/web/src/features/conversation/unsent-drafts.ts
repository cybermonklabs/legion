import { type QueryClient, useQueryClient } from "@tanstack/react-query";

import { createDraftStore } from "../inbox/draft-store";
import type { SentDraft } from "./send-request";

/**
 * What the reader typed into a standing box (`MentionComposer`'s `standing`) and has not sent, by
 * the box's send name. It is a draft store of the kind the ask's answer and reply drafts are
 * (`draft-store.ts`): the draft belongs to the thread the box is named for, not to the field
 * showing it. The Inbox's column draws one box per thread and a new one for each row it moves
 * to, so what was typed for a thread waits under that thread's name, comes back when the reader
 * returns to it, and never shows in another thread's box. A draft is kept whole, its accepted
 * mentions with it, since those are what a reply's mentions are sent from.
 */
export interface UnsentDrafts {
  read(name: string): SentDraft | undefined;
  /** Keeps `draft` under `name`, or, given none, forgets the draft there. */
  write(name: string, draft: SentDraft | undefined): void;
}

const drafts = new WeakMap<QueryClient, UnsentDrafts>();

/** The unsent drafts that live with the query client, as the held sends do (`heldSends`): a draft
 *  store holds text, so each draft is kept as its JSON. */
export function useUnsentDrafts(): UnsentDrafts {
  const client = useQueryClient();
  let unsent = drafts.get(client);
  if (unsent === undefined) {
    const store = createDraftStore();
    unsent = {
      read: (name) => {
        const kept = store.read(name);
        return kept === "" ? undefined : (JSON.parse(kept) as SentDraft);
      },
      write: (name, draft) => store.set(name, draft === undefined ? "" : JSON.stringify(draft)),
    };
    drafts.set(client, unsent);
  }
  return unsent;
}
