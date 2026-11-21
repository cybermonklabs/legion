import { hashKey, type MutationKey } from "@tanstack/react-query";
import { useEffect, useId, useMemo, useRef } from "react";

import { type HeldDraft, type HeldSend, useHeldSend, useHeldSends } from "./held-sends";
import type { Address, CarriedAddress, SentRequest } from "./send-request";

/** A composer's send, read and written through the held-send store (`held-sends.ts`). */
export interface ComposerSend {
  /** The send its target holds - out, or refused - which a composer mounting now shows. */
  readonly held: HeldSend | undefined;
  /** Builds the request and sends it, unless a send under the composer's name is out, or the
   *  refusal it holds lives at another address than the request's (`HeldSends.send`); `build`
   *  runs only when no send is out, handed the refused send it replaces, if any - its request and
   *  what refused it. The send keeps `anchoredAt`, when a margin compose's composer took the words
   *  it is about. */
  readonly send: (
    build: (refused: HeldSend | undefined) => SentRequest,
    anchoredAt: number | undefined
  ) => void;
  /** Whether a send is out, past its deadline included, as of the last render. */
  readonly sending: boolean;
  /** What the server refused, for the reader: a deadline is not a refusal, and the answer that
   *  follows one is. */
  readonly refusal: Error | undefined;
  /** Whether a send is out now, from Send's own task on, before React has rendered it. */
  readonly sendingNow: () => boolean;
  /** Drops a refusal, its draft with it. */
  readonly discard: () => void;
  /** Stops trying a late send (Stop trying; `HeldSends.stop`). */
  readonly stop: () => void;
  /** Keeps what the reader wrote over a refusal, for the composer that mounts next. */
  readonly keep: (draft: HeldDraft) => void;
  /** Takes a refusal that lives at `from`, the address the composer showed, to where the reader
   *  moved the composer with its own controls (`HeldSends.carry`). */
  readonly carry: (from: Address, to: CarriedAddress) => void;
}

/**
 * The send behind a composer, held by its target rather than by the composer: `mutationKey` names
 * both, and a composer with none gets a name of its own mount. `landed` and `refused` are the
 * composer's own part of an outcome - its draft, its host's callbacks - and run only while it is
 * mounted; the store refreshes what a landed send wrote either way.
 */
export function useComposerSend(
  mutationKey: MutationKey | undefined,
  {
    landed,
    refused,
  }: {
    readonly landed: (sent: SentRequest) => void;
    /** A refusal hands back exactly the draft the request turned down, not a later edit. */
    readonly refused: (held: HeldSend) => void;
  }
): ComposerSend {
  const mount = useId();
  const key = useMemo<MutationKey>(() => mutationKey ?? ["composer", mount], [mount, mutationKey]);
  const store = useHeldSends();
  const held = useHeldSend(key);
  const outcomes = useRef({ landed, refused });
  outcomes.current = { landed, refused };
  const id = useMemo(() => hashKey(key), [key]);
  useEffect(
    () =>
      store.onOutcome(({ kind, send }) => {
        if (hashKey(send.mutationKey) !== id) return;
        if (kind === "landed") outcomes.current.landed(send.request);
        if (kind === "refused") outcomes.current.refused(send);
      }),
    [id, store]
  );
  return {
    carry: (from, to) => store.carry(key, from, to),
    discard: () => store.discard(key),
    held,
    keep: (draft) => store.keep(key, draft),
    refusal: held?.status === "refused" ? held.refusal : undefined,
    send: (build, anchoredAt) => {
      if (store.sendingNow(key)) return;
      // No send is out, so what the store holds under the name, if anything, is a refusal.
      store.send(key, build(store.get(key)), anchoredAt);
    },
    sending: held !== undefined && held.status !== "refused",
    sendingNow: () => store.sendingNow(key),
    stop: () => store.stop(key),
  };
}
