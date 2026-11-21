import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { connect, createInbox, Events, headers, type Msg, type NatsConnection, nkeys } from "nats";
import { createConsumerDelivery, deliverSubject, RelayHeader } from "../consumer-delivery";
import { natsAuthOptions } from "../nats-auth";
import {
  NATS_START_HOOK_MS,
  NATS_STOP_HOOK_MS,
  natsHookDeadline,
  natsServerWithConfig,
  useNatsServerDirectory,
} from "./nats-server";

/** nats.js types its `nkeys` export `any`. */
const testKeys: { createUser(): { getSeed(): Uint8Array; getPublicKey(): string } } = nkeys;

const serverDirectory = useNatsServerDirectory();

const SESSION = "ses_reply_grant";
const AGENT = `notifications.agent.${SESSION}`;
/** A 26-character token of the alphabet the listener's and Dispatch's tokens use
 *  (Go's crypto/rand.Text). */
const token = () =>
  Array.from(
    crypto.getRandomValues(new Uint8Array(26)),
    (byte) => "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"[byte % 32]
  ).join("");
/** The reply subject a listener (bus.SessionReplySubject) asks this session on now:
 *  `<inbox>.<session id>.<token>`. */
const sessionReply = () => `${createInbox()}.${SESSION}.${token()}`;

/** The row's bound, passed to the row. Its waits give up 5 s inside it, so a wait that gives up
 *  fails naming what it waited for rather than as the row's timeout. */
const ROW_MS = 60_000;

/** Polls `holds` every 20 ms until it is true, failing with `what` at `deadline` (a
 *  `performance.now()` reading). The answers and the server's refusals cross a real socket from a
 *  real server, so there is no clock to fake; the wait is on the condition itself, never a fixed
 *  sleep, and its only bound is the row's. */
async function waitFor(holds: () => boolean, deadline: number, what: string): Promise<void> {
  while (!holds()) {
    if (performance.now() > deadline) {
      throw new Error(`timed out waiting for ${what}, ${ROW_MS / 1000 - 5} s into the row`);
    }
    await Bun.sleep(20);
  }
}

// The shared consumer delivery is the one place both plugins answer the listener's relay frames
// and connection probe (pi-envoy's and claude-envoy's `publish` is `connection.publish(reply, …)`).
// Here it runs on a real nats.js connection authenticated as a session user whose grant is a
// session's own: publish its replies under `_INBOX.*.<session id>.>` (`bus.SessionReplies`) alone.
// Every answer to a frame or probe asked on a reply subject naming the session reaches the
// requester; the same answers on the shapes the listener asked on before - its reply inbox and a
// counter, and a plain inbox for the probe - are refused by the server and reach nobody, which is
// the control that the grant is enforced and that the watcher hears what the session publishes.
//
// Docker-backed: CI runs this package's tests with LEGION_E2E=1 after pulling the image.
describe.skipIf(process.env.LEGION_E2E !== "1")("a session's replies under its own grant", () => {
  const user = testKeys.createUser();
  const seed = new TextDecoder().decode(user.getSeed());
  /** The listener's stand-in: an nkey user the server grants everything. */
  const requesterUser = testKeys.createUser();
  const requesterSeed = new TextDecoder().decode(requesterUser.getSeed());
  const server = natsServerWithConfig(
    serverDirectory,
    `jetstream {}
authorization {
  users = [
    { nkey: ${JSON.stringify(requesterUser.getPublicKey())} }
    { nkey: ${JSON.stringify(user.getPublicKey())}, permissions: { publish: { allow: [${JSON.stringify(`_INBOX.*.${SESSION}.>`)}, ${JSON.stringify(`agentstream.${SESSION}.frames`)}] }, subscribe: { allow: [${JSON.stringify(deliverSubject(SESSION))}, ${JSON.stringify(`agentstream.${SESSION}.control`)}] } } }
  ]
}
`
  );
  const open: NatsConnection[] = [];
  /** The teardown's bound: the connections' closes, in the 60 s this hook had before, and then
   *  the container's removal. */
  const STOP_HOOK_MS = 60_000 + NATS_STOP_HOOK_MS;

  beforeAll(() => server.start(natsHookDeadline(NATS_START_HOOK_MS)), NATS_START_HOOK_MS);
  afterAll(async () => {
    const deadline = natsHookDeadline(STOP_HOOK_MS);
    for (const connection of open) await connection.close();
    await server.stop(deadline);
  }, STOP_HOOK_MS);

  test(
    "answers a frame and a probe asked on its own reply prefix, and nothing else gets out",
    async () => {
      const deadline = performance.now() + ROW_MS - 5_000;
      // nats.js gives a first connect its own 20 s by default; the row's deadline is its bound.
      const session = await connect({
        servers: [server.url],
        name: "session-reply-grant",
        ...natsAuthOptions({
          kind: "seed",
          seed: { value: seed, source: "the test's session user" },
        }),
        reconnect: false,
        timeout: Math.max(1, deadline - performance.now()),
      });
      open.push(session);
      const refusals: string[] = [];
      void (async () => {
        for await (const status of session.status()) {
          if (status.type === Events.Error) refusals.push(String(status.data));
        }
      })();
      const requester = await connect({
        servers: [server.url],
        ...natsAuthOptions({
          kind: "seed",
          seed: { value: requesterSeed, source: "the test's requester user" },
        }),
        reconnect: false,
        timeout: Math.max(1, deadline - performance.now()),
      });
      open.push(requester);
      const heard: Array<readonly [string, string]> = [];
      requester.subscribe("_INBOX.>", {
        callback: (_error, message) => heard.push([message.subject, message.string()]),
      });
      await requester.flush();

      const delivery = createConsumerDelivery({
        sessionID: SESSION,
        ledger: { delivered: () => false, liveFailed: async () => true },
        host: {
          deliver: (frame) => frame.done(),
          gap: () => undefined,
          log: () => undefined,
        },
        publish: (reply, body) => session.publish(reply, new TextEncoder().encode(body)),
        persist: () => true,
      });
      session.subscribe(delivery.subject, {
        callback: (_error, message: Msg) => delivery.receive(message),
      });
      await session.flush();

      const frame = (seq: number, reply: string) => {
        const h = headers();
        h.set(RelayHeader.subject, AGENT);
        h.set(RelayHeader.streamSeq, String(seq));
        h.set(RelayHeader.streamID, "stream-reply-grant");
        h.set(RelayHeader.delivered, "1");
        h.set(RelayHeader.storedAt, new Date().toISOString());
        const body = JSON.stringify({ event_id: `evt-reply-grant-${seq}`, source: "agent" });
        requester.publish(deliverSubject(SESSION), new TextEncoder().encode(body), {
          reply,
          headers: h,
        });
      };
      const probe = (reply: string) => {
        const h = headers();
        h.set(RelayHeader.probe, "1");
        requester.publish(deliverSubject(SESSION), new Uint8Array(), { reply, headers: h });
      };
      const answersOn = (subject: string) =>
        heard.filter(([to]) => to === subject).map(([, body]) => body);

      const ownFrame = sessionReply();
      const ownProbe = sessionReply();
      frame(1, ownFrame);
      probe(ownProbe);
      await waitFor(
        () => answersOn(ownFrame).length === 2 && answersOn(ownProbe).length === 1,
        deadline,
        `received and done on ${ownFrame} and done on ${ownProbe}`
      );
      expect(answersOn(ownFrame)).toEqual(["received", "done"]);
      expect(answersOn(ownProbe)).toEqual(["done"]);
      expect(refusals).toEqual([]);

      // The control: the reply shapes the listener asked on before this change.
      const inbox = createInbox();
      const counterFrame = `${inbox}.2`;
      const plainProbe = createInbox();
      frame(2, counterFrame);
      probe(plainProbe);
      await waitFor(() => refusals.length === 3, deadline, "the server's three refusals");
      await session.flush();
      await requester.flush();
      expect(refusals).toEqual([
        "PERMISSIONS_VIOLATION",
        "PERMISSIONS_VIOLATION",
        "PERMISSIONS_VIOLATION",
      ]);
      expect(answersOn(counterFrame)).toEqual([]);
      expect(answersOn(plainProbe)).toEqual([]);
      expect(heard.map(([to]) => to).sort()).toEqual([ownFrame, ownFrame, ownProbe].sort());
    },
    ROW_MS
  );
});
