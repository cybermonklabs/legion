import { useState } from "react";

import type { Agent, MessageDeliveryMode } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { broadcastDelivery } from "../conversation/delivery";
import { retriesUnderItsKey } from "../writes/keyed-write";
import type { RestoredBroadcast } from "./broadcast-plan";
import type { BroadcastSend } from "./broadcast-queue";

/** The broadcast being composed on the Agents page, or in the Inbox's compose dialog. */
export interface BroadcastComposition {
  /** The mode the sender picked, or null until they pick one: the page then sends in the mode
   *  the selection takes (`broadcastDelivery`). */
  readonly delivery: MessageDeliveryMode | null;
  readonly draft: string;
  /** A refused send's whole request and the key it goes under again, put back by Restore draft
   *  and dropped by the first edit. */
  readonly restored: RestoredBroadcast | null;
  readonly selected: ReadonlySet<string>;
  /** The key the next live send carries; `sent` replaces it. */
  readonly sendKey: string;
  readonly setDelivery: (delivery: MessageDeliveryMode) => void;
  readonly setDraft: (draft: string) => void;
  readonly setSelected: (update: (current: ReadonlySet<string>) => ReadonlySet<string>) => void;
  /** Restore draft: a refused send's message, mode, selection and request come back; `refusal`,
   *  what ended it, decides the key a re-send goes under (`retriesUnderItsKey`). */
  readonly restore: (send: BroadcastSend, refusal: Error) => void;
  /** Send handed the request to the queue: the composer empties, and the next send gets a key. */
  readonly sent: () => void;
  /** Send handed the request to a host that keeps the composition on screen while the request is
   *  out, the Inbox's compose dialog: the message, the mode and the selection stay, and the next
   *  send gets a key. A refusal puts the request back with its key's rule (`restore`). */
  readonly pressed: () => void;
}

type CompositionState = Omit<
  BroadcastComposition,
  "pressed" | "restore" | "sent" | "setDelivery" | "setDraft" | "setSelected"
>;

/**
 * The mode a composition shows and sends, on the Agents page and in the Inbox's compose dialog. A
 * restored request is re-sent word for word until an edit drops it, so its own mode comes first,
 * then the mode the sender picked. Until they pick one, the broadcast goes in the one its
 * selection of `agents` takes (`broadcastDelivery`): BTW, or Aside when a selected session takes
 * Aside and not BTW.
 */
export function compositionDelivery(
  composition: BroadcastComposition,
  agents: readonly Agent[]
): MessageDeliveryMode {
  return (
    composition.restored?.input.delivery ??
    composition.delivery ??
    broadcastDelivery(
      agents.flatMap((agent) =>
        composition.selected.has(agent.session_id) ? [agent.capabilities ?? []] : []
      )
    )
  );
}

/**
 * The composition is page state, not the composer's: the composer unmounts whenever the selection
 * empties, and clearing a selection to pick again must not throw away a typed message or its mode.
 * Its only mutators are an edit, a restore and a send, so the rules live here. One key per send
 * (`Idempotency-Key`): the key changes only once Send has handed the request on - to the queue
 * (`sent`), or to the Inbox's compose dialog's own write (`pressed`) - so two presses of one
 * composition name one broadcast on the server, and the send after it never reuses its key.
 * Every edit - the message, the mode, the selection - drops a restored request: until then
 * Send re-sends it word for word, so the server sees the request it refused even after the
 * registry changed (`composedBroadcast`), under its own key when it ended without Dispatch's
 * verdict and under a new one when Dispatch refused it.
 */
export function useBroadcastComposition(): BroadcastComposition {
  const [state, setState] = useState<CompositionState>(() => ({
    delivery: null,
    draft: "",
    restored: null,
    selected: new Set(),
    sendKey: newSendKey(),
  }));
  return {
    ...state,
    pressed: () => setState((current) => ({ ...current, sendKey: newSendKey() })),
    restore: (send, refusal) =>
      setState((current) => ({
        ...current,
        delivery: send.picked ? send.input.delivery : null,
        draft: send.input.body,
        restored: {
          input: send.input,
          key: retriesUnderItsKey(refusal) ? send.key : newSendKey(),
        },
        selected: new Set(send.selected),
      })),
    sent: () =>
      setState((current) => ({
        ...current,
        draft: "",
        restored: null,
        selected: new Set(),
        sendKey: newSendKey(),
      })),
    setDelivery: (delivery) => setState((current) => ({ ...current, delivery, restored: null })),
    setDraft: (draft) => setState((current) => ({ ...current, draft, restored: null })),
    setSelected: (update) =>
      setState((current) => ({ ...current, restored: null, selected: update(current.selected) })),
  };
}
