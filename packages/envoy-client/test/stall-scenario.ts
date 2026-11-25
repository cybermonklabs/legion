import { expect } from "bun:test";
import { timeLeft } from "@legion/test-fixtures/wait";
import { connect, type KV } from "nats";
import { z } from "zod";
import { deliverSubject } from "../src/consumer-delivery";
import { STORED_WHILE_AWAY_SUFFIX } from "../src/delivery-ledger";
import type { EnvoyStack } from "./envoy-stack";
import type { StallProxy } from "./stall-proxy";

/** The spec's bound: a session finds a path that answers within a minute of the freeze. */
export const RECOVERY_BOUND_MS = 60_000;

/**
 * The session's `ENVOY_HEARTBEAT_MS` for this scenario: an hour between heartbeats, past the
 * guard of each row that runs it and the 20-minute job that runs those rows. After the start's
 * registration no heartbeat registers within a row, so the reconnect's registration is the only
 * one that can carry a new epoch. `envoySettings` takes any finite number above zero and holds it
 * under 2 147 483 647 ms (`resolvedInterval`, `src/envoy-settings.ts`), so the hour stands.
 */
export const FREEZE_HEARTBEAT_MS = "3600000";

/** A session plugin under test, registered, whose NATS path `proxy` fronts. */
export interface FreezeScenarioHost {
  /** What the timing log line starts with, such as `pi-envoy stall`. */
  readonly label: string;
  /** The natsd and listener the session runs against. */
  readonly stack: EnvoyStack;
  /** The session the direct messages go to. */
  readonly sessionID: string;
  /** The proxy the session's only NATS connection goes through. */
  readonly proxy: StallProxy;
  /** The text of every message the host showed the session that holds `marker`, in order. */
  shown(marker: string): readonly string[];
  /** Resolves once `condition` holds; throws, naming `what`, when `deadline` passes first, when
   *  a process of `stack` has exited (`EnvoyStack.exited`), checked on each poll, or once the
   *  row has ended. A wait given no deadline ends at its row's guard: for a host that waits
   *  through `until` (`@legion/test-fixtures/wait`), at the row's deadline, naming `what`. */
  waitFor(
    condition: () => boolean | Promise<boolean>,
    deadline: number | undefined,
    what: string
  ): Promise<void>;
}

/** The fields of the listener's session entry (`envoy_sessions`) the scenario reads. */
const SessionEntrySchema = z.object({
  delivery: z.string().optional(),
  epoch: z.string().optional(),
});

/** natsd's `/connz?subs=1`: each connection, when it started and what it subscribes. */
const ConnzSchema = z.object({
  connections: z
    .array(
      z.object({
        start: z.string(),
        subscriptions_list: z.array(z.string()).optional(),
      })
    )
    .optional(),
});

/** The session's entry as the listener stored it; undefined while it has none. */
async function sessionEntry(
  sessions: KV,
  sessionID: string
): Promise<z.infer<typeof SessionEntrySchema> | undefined> {
  const entry = await sessions.get(sessionID);
  if (entry === null || entry.operation !== "PUT") return undefined;
  return SessionEntrySchema.parse(entry.json());
}

/**
 * Whether a connection natsd accepted at or after `since` (`Date.now()` time) subscribes `subject`.
 * The read has no timeout of its own: how long natsd's monitor takes to answer is the host's
 * speed, and a monitor that never answers holds the read to its row's guard.
 */
async function subscribedSince(monitorUrl: string, subject: string, since: number) {
  const response = await fetch(`${monitorUrl}/connz?subs=1`);
  if (!response.ok) return false;
  const connz = ConnzSchema.parse(await response.json());
  return (connz.connections ?? []).some(
    (connection) =>
      Date.parse(connection.start) >= since &&
      (connection.subscriptions_list ?? []).includes(subject)
  );
}

/**
 * The freeze every session plugin must survive. The listener holds the session's durable consumer
 * and relays it on `notifications.envoy.deliver.<id>`: the session's registration is answered
 * `consumer` and its relay subject is subscribed, and a direct message is shown live. Then the
 * session's NATS path is frozen and a second message sent while it is cut off: it must be shown
 * within 60 s of the freeze, after the connection nats.js dialled in place of the frozen one
 * subscribed the relay subject again and the session registered a new epoch, so the listener
 * restarted the relay at the consumer's ack floor. A third message, sent after that registration,
 * is relayed on the new connection. Each of the three is shown once, and none as a frame the
 * session was told it had been away for: the relay delivers all three.
 *
 * The host runs on nats.js's real ping and dial timers against a real listener, so each wait is
 * real time. Only the recovery's wait has a deadline: the message sent while the path was frozen
 * must be shown within `RECOVERY_BOUND_MS` of the freeze, which the scenario asserts. Every other
 * wait passes the host's `waitFor` no deadline, so it ends at its row's guard, naming what it
 * waited for.
 */
export async function runFreezeScenario(host: FreezeScenarioHost): Promise<void> {
  const { label, stack, sessionID, proxy, shown, waitFor } = host;
  // The subject the session's own connection takes every message on.
  const sessionSubject = deliverSubject(sessionID);
  const send = async (marker: string) => {
    const answer = await fetch(`${stack.envoyUrl}/v1/messages/send`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target_session: sessionID, source: "agent", message: marker }),
    });
    expect(answer.status).toBe(200);
  };
  const run = crypto.randomUUID();
  // The scenario's own connection, straight to natsd, reads the listener's session entry.
  const observer = await connect({
    servers: [stack.natsUrl],
    name: `stall-observer-${run}`,
    // The handshake may take until the row's deadline: nats.js's 20 s default could fail the row
    // on a loaded host on the observer's own handshake.
    timeout: timeLeft(),
  });
  try {
    const sessions = await observer
      // Each JetStream request may take the time left before the row's deadline as read here, at
      // least what is left when any later request is made: nats.js's 5 s default could fail the
      // row on a loaded host on the bind's account check below or a read of the session entry.
      .jetstream({ timeout: timeLeft() })
      .views.kv("envoy_sessions", { bindOnly: true });
    let firstEpoch: string | undefined;
    await waitFor(
      async () => {
        const entry = await sessionEntry(sessions, sessionID);
        if (entry === undefined) return false;
        firstEpoch = entry.delivery === "consumer" ? entry.epoch : undefined;
        if (firstEpoch === undefined) return false;
        return await subscribedSince(stack.monitorUrl, sessionSubject, 0);
      },
      undefined,
      "the session's registration answered consumer, with its relay subject subscribed"
    );

    const live = `stall-live-before-${run}`;
    await send(live);
    await waitFor(() => shown(live).length > 0, undefined, "the live message");

    const frozenAt = proxy.freeze();
    const missed = `stall-missed-${run}`;
    await send(missed);
    await waitFor(
      () => shown(missed).length > 0,
      frozenAt + RECOVERY_BOUND_MS,
      "the message sent while the path was frozen"
    );
    const shownAt = Date.now();
    const resubscribed = proxy.connections.find(
      (opened) => opened.acceptedAt >= frozenAt && opened.subscribedAt !== undefined
    );
    console.log(
      `${label}: resubscribed ${(((resubscribed?.subscribedAt ?? Number.NaN) - frozenAt) / 1000).toFixed(1)} s, missed message shown ${((shownAt - frozenAt) / 1000).toFixed(1)} s after the freeze`
    );
    expect(resubscribed?.subscribedAt).toBeDefined();
    expect((resubscribed?.subscribedAt ?? Number.POSITIVE_INFINITY) - frozenAt).toBeLessThan(
      RECOVERY_BOUND_MS
    );
    expect(shownAt - frozenAt).toBeLessThan(RECOVERY_BOUND_MS);
    // The connection that replaced the frozen one carries the session's relay subject.
    expect(await subscribedSince(stack.monitorUrl, sessionSubject, frozenAt)).toBe(true);
    // The session registers a new epoch on the new connection, still on consumer delivery. That
    // registration is the session's own request to the listener, and the missed message's
    // delivery is the listener's work: nothing orders the two. Under 120 busy loops (the 96 rung)
    // claude-envoy's row once saw the message shown 1.8 s after the resubscribe while the entry
    // still named the first epoch (Job infra-1580-krows-kc-g4zvc, run 10, at 39bc1254), so the
    // scenario waits for the entry to name a new epoch rather than read it once. The wait has no
    // deadline: a session that never registers one fails it at its row's guard.
    let renewed: z.infer<typeof SessionEntrySchema> | undefined;
    await waitFor(
      async () => {
        renewed = await sessionEntry(sessions, sessionID);
        return renewed?.delivery === "consumer" && renewed.epoch !== firstEpoch;
      },
      undefined,
      "the session's registration of a new epoch"
    );
    expect(renewed?.delivery).toBe("consumer");
    expect(renewed?.epoch).toBeDefined();
    expect(renewed?.epoch).not.toBe(firstEpoch);

    const after = `stall-live-after-${run}`;
    await send(after);
    await waitFor(() => shown(after).length > 0, undefined, "the message after the reconnect");

    // Each shown once, and the relay delivered all three, so none carries the line a frame stored
    // while the session was away would carry.
    for (const marker of [live, missed, after]) {
      const messages = shown(marker);
      expect(messages).toHaveLength(1);
      expect(messages[0]?.includes(STORED_WHILE_AWAY_SUFFIX)).toBe(false);
    }
  } finally {
    await observer.close();
  }
}
