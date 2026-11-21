// A `CardWake` (`src/card-wake.ts`) on a stand-in host whose branch, idle state, start-turn mark
// and managed timer a test drives. Two files share it, which is why it is a module of its own:
// `src/card-wake.test.ts` and the startup hold's rows on the fake clock,
// `src/card-wake-start-hold.test.ts`.
import { type EnvoyPluginInterface, LOCAL_ENVOY_NOTICE } from "@legion/pi-shared/interface";
import type { SessionContext } from "@legion/pi-shared/pi-types";

import { type CardDelivery, CardWake, type CardWakeContext } from "../src/card-wake";

type HostMessage = { readonly role: string } & Record<string, unknown>;

interface Sent {
  readonly message: unknown;
  readonly options: unknown;
}

interface SentCard {
  readonly delivery: CardDelivery;
  /** The record the host hands `message_end` for the card. */
  readonly message: HostMessage;
  /** The entry the host persists for it. */
  readonly entry: unknown;
}

export const interrupted = { stopReason: "aborted", errorMessage: "Interrupted by user" };
export const aside = { deliverAs: "aside", triggerTurn: true } as const;
export const followUp = { deliverAs: "followUp", triggerTurn: false } as const;
/** The session the harness's cards go into, which `isOwnSession` answers for. */
const OWN_SESSION = "ses_own";

/** What `cardWakeHarness` hands a test; each member's doc is at its definition there. */
export interface CardWakeHarness {
  readonly advance: (ms: number) => void;
  readonly clock: () => number;
  readonly branch: unknown[];
  readonly card: (label: string, targeted?: boolean) => SentCard;
  readonly context: CardWakeContext;
  readonly delays: number[];
  readonly fire: () => Promise<void>;
  readonly message: (record: HostMessage, persisted?: boolean) => void;
  readonly reply: (fields?: Record<string, unknown>) => HostMessage;
  readonly sent: Sent[];
  readonly wake: CardWake;
  readonly setIdle: (value: boolean) => void;
  readonly fold: (...cards: SentCard[]) => void;
  readonly endStartup: () => Promise<void>;
  readonly run: () => void;
  readonly end: () => void;
  readonly event: (idleThere: boolean) => void;
  readonly markUnsent: (session?: string) => void;
  readonly markSent: (sentAt: number, session?: string) => void;
  readonly clearMark: () => void;
  readonly startTurn: () => void;
}

/**
 * A `CardWake` on a host whose branch the test writes and whose managed timer it runs (`fire`),
 * recording the delay each timer was set for (`delays`) and every message the wake sends (`sent`).
 * `card` asks how to send one card, as the host is busy unless `setIdle(true)`, and returns the
 * record the host would fold and the entry it would persist; `message` hands the wake a record's
 * `message_end`, persisted first unless told not to; `fold` marks the host idle and hands it cards'
 * records, persisted in order; `reply` is an assistant message ending a step. `reportsIdle: false`
 * is a host context without `isIdle`; `mode` is the context's `mode` (RPC unless named, `null` for
 * a context without one). The wake starts in its startup phase, and `endStartup` ends it with a
 * startup card a run read. The cards go into `OWN_SESSION`; `markUnsent` and `markSent` set the
 * start-turn mark the wake reads (empty until a test sets it) to name a session, `OWN_SESSION`
 * unless told, with its start unsent or sent at a time, and `startTurn` runs the start turn.
 */
export function cardWakeHarness(
  options: {
    readonly reportsIdle?: boolean;
    readonly mode?: NonNullable<SessionContext["mode"]> | null;
  } = {}
): CardWakeHarness {
  let clock = 1_000;
  let idle = false;
  const sent: Sent[] = [];
  let mark: EnvoyPluginInterface["startupTurn"];
  const wake = new CardWake({
    now: () => clock,
    sendMessage: (message, sendOptions) => {
      sent.push({ message, options: sendOptions });
    },
    startupTurn: () => mark,
    isOwnSession: (session) => session === OWN_SESSION,
  });
  const branch: unknown[] = [];
  const timers: (() => void | Promise<void>)[] = [];
  const delays: number[] = [];
  const mode = options.mode === undefined ? "rpc" : options.mode;
  const context: CardWakeContext = {
    ...(options.reportsIdle !== false && { isIdle: () => idle }),
    ...(mode !== null && { mode }),
    setTimeout: (callback, delayMs) => {
      timers.push(callback);
      delays.push(delayMs);
    },
    sessionManager: { getBranch: () => branch },
  };
  let step = 0;
  const card = (label: string, targeted = false): SentCard => {
    const delivery = wake.deliveryFor(targeted, context);
    const fields = {
      customType: "envoy-message",
      content: label,
      display: true,
      details: delivery.details,
    };
    step += 1;
    return {
      delivery,
      message: { role: "custom", ...fields, timestamp: step },
      entry: { type: "custom_message", id: `entry-${label}`, ...fields },
    };
  };
  const message = (record: HostMessage, persisted = true): void => {
    if (persisted) {
      step += 1;
      branch.push(
        record.role === "custom"
          ? { type: "custom_message", id: `entry-${step}`, ...record }
          : { type: "message", id: `entry-${step}`, message: record }
      );
    }
    wake.messageEnd(record, context);
  };
  const reply = (fields: Record<string, unknown> = {}): HostMessage => ({
    role: "assistant",
    content: [{ type: "text", text: "done" }],
    stopReason: "stop",
    ...fields,
  });
  const fire = async (): Promise<void> => {
    for (const timer of timers.splice(0)) await timer();
  };
  return {
    advance: (ms: number) => {
      clock += ms;
    },
    /** The harness's clock now. */
    clock: () => clock,
    branch,
    card,
    context,
    delays,
    fire,
    message,
    reply,
    sent,
    wake,
    setIdle: (value: boolean) => {
      idle = value;
    },
    fold: (...cards: SentCard[]) => {
      idle = true;
      for (const folded of cards) {
        branch.push(folded.entry);
        wake.messageEnd(folded.message, context);
      }
    },
    endStartup: async (): Promise<void> => {
      const was = idle;
      idle = true;
      const startup = card("startup");
      message(startup.message);
      message(reply());
      await fire();
      idle = was;
    },
    /** A run starts, as the host emits `agent_start`. */
    run: () => wake.agentStart(),
    end: () => wake.agentEnd(context),
    /** A `session_switch`, `session_compact` or `input` event, which finds the host idle or busy
     *  as told and leaves it as it was. */
    event: (idleThere: boolean) => {
      const was = idle;
      idle = idleThere;
      wake.noteIfIdle(context);
      idle = was;
    },
    /** The start-turn mark names `session` with its start due and unsent, as the operator's
     *  controller's `session_start` marks it. */
    markUnsent: (session = OWN_SESSION) => {
      mark = { session, sentAt: undefined };
    },
    /** The start-turn mark names `session` with its start sent at `sentAt`, as the start command
     *  and the daemon's controller at its ready mark it. */
    markSent: (sentAt: number, session = OWN_SESSION) => {
      mark = { session, sentAt };
    },
    /** The start-turn mark is cleared, as the Legion entry clears it on a fact: the session's first
     *  `agent_start`, its shutdown, a claim refused before the send, a failed send. */
    clearMark: () => {
      mark = undefined;
    },
    /** The start turn runs: its `agent_start` clears the mark, as the Legion entry does, the start
     *  message and the model's reply land in the branch, and its `agent_end` comes on an idle
     *  host. */
    startTurn: () => {
      idle = false;
      wake.agentStart();
      mark = undefined;
      message({ role: "user", content: "start" });
      message(reply());
      idle = true;
      wake.agentEnd(context);
    },
  };
}

/** What the wake sends: one hidden notice for `content`, as a next-turn message that starts one. */
export const wakeMessage = (content: unknown) => ({
  message: {
    content,
    customType: "envoy-message",
    details: LOCAL_ENVOY_NOTICE,
    display: false,
  },
  options: { deliverAs: "nextTurn", triggerTurn: true },
});

/** The text of a message the wake sent, or "" for none. */
export const contentOf = (sent: Sent | undefined): string => {
  const message = sent?.message;
  if (typeof message !== "object" || message === null || !("content" in message)) return "";
  return String(message.content);
};
