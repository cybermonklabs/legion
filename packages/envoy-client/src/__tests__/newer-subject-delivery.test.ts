import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { EnvelopeSchema, legacyAgentSubject } from "@legion/contracts";
import { connect, StringCodec } from "nats";
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "../../test/envoy-stack";
import {
  createConsumerDelivery,
  inboundUnavailable,
  registerWithRelay,
} from "../consumer-delivery";
import { createDeliveryLedger } from "../delivery-ledger";
import { createEnvoyClient } from "../transport";

/**
 * A plugin and a listener roll separately, and during a roll the notification stream holds the
 * subjects of the newer build as well as the older one's (a build's ensure adds its own subjects
 * and drops none it does not compile). A client of the older build compiles no subject capturing
 * a topic only the newer build's subjects capture. If it judged capture itself and left such a
 * topic out of its registration, the session's consumer would filter on nothing for it, and the
 * consumer would move past every message the stream stored on it.
 *
 * This row runs the real listener and the real natsd this checkout builds (test/envoy-stack.ts),
 * adds the newer build's subject to the stream as that build's start does, and drives the client's
 * own registration (`registerWithRelay`, which every host makes, over `registrationTopics`) and
 * consumer read (`createConsumerDelivery`). Nothing here fakes a listener call.
 *
 * Run it from packages/envoy-client with `LEGION_E2E_ENVOY_STACK=1 bun test
 * src/__tests__/newer-subject-delivery.test.ts`; without the gate it is skipped.
 */

const STREAM = "ENVOY_NOTIFICATIONS";
/** The subject a newer build adds; this checkout's stream subjects capture nothing under it. */
const NEWER_SUBJECT = "notifications.x.>";
const TOPIC = "notifications.x.y";
const SESSION = "ses_newer_subject_reader";
const SENDER = "ses_newer_subject_sender";
const PLUGIN = { name: "@legion/envoy-client", version: "newer-subject-row" };
const DELIVERY_BOUND_MS = 20_000;

const codec = StringCodec();

// Reserved here, at the file's module scope: the file's own `afterAll` removes it once the
// `describe` below has stopped the stack writing into it.
const stackDirectory = useEnvoyStackDirectory();

describe.skipIf(!envoyStackEnabled)("a topic only a newer build's stream subject captures", () => {
  let stack: EnvoyStack | undefined;

  beforeAll(async () => {
    stack = await startEnvoyStack({ work: stackDirectory });
  }, 180_000);
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  test("is registered by a client that compiles no subject capturing it, and is delivered", async () => {
    if (stack === undefined) throw new Error("the Envoy stack did not start");
    const nats = await connect({ servers: [stack.natsUrl], name: SESSION });
    try {
      // The newer build's start: its subject joins the stream's, which this checkout's listener
      // keeps (it drops no subject it does not compile).
      const jsm = await nats.jetstreamManager();
      const { config } = await jsm.streams.info(STREAM);
      expect(config.subjects).not.toContain(NEWER_SUBJECT);
      await jsm.streams.update(STREAM, { subjects: [...(config.subjects ?? []), NEWER_SUBJECT] });

      const own = legacyAgentSubject(SESSION);
      const delivered: string[] = [];
      const arrived = Promise.withResolvers<void>();
      const failures: string[] = [];
      const ledger = createDeliveryLedger({
        host: {
          sessionID: () => SESSION,
          followedSubjects: () => [own, TOPIC],
          showFailureNotice: async () => undefined,
          failureNoticeFailed: (_notice, error) => failures.push(String(error)),
          log: (message, error) => failures.push(`${message}: ${String(error)}`),
          write: () => undefined,
        },
      });
      const delivery = createConsumerDelivery({
        sessionID: SESSION,
        ledger,
        host: {
          deliver: (frame) => {
            const envelope = EnvelopeSchema.parse(JSON.parse(frame.data));
            if (envelope.topic === TOPIC) {
              delivered.push(envelope.payload_summary);
              arrived.resolve();
            }
            frame.done();
          },
          gap: (notice) => failures.push(notice.message),
          log: (message, error) => failures.push(`${message}: ${String(error)}`),
        },
        publish: (reply, body) => nats.publish(reply, codec.encode(body)),
        persist: () => true,
      });
      const relay = nats.subscribe(delivery.subject);
      void (async () => {
        for await (const message of relay) delivery.receive(message);
      })();
      await nats.flush();

      const client = createEnvoyClient({ baseUrl: stack.envoyUrl, fetch });
      const sent: string[][] = [];
      const answer = await registerWithRelay({
        relay: delivery,
        ownAgentSubject: own,
        followed: [TOPIC],
        outage: inboundUnavailable(
          (why) => failures.push(why),
          () => undefined
        ),
        send: (topics, request) => {
          sent.push([...topics]);
          return client.subscribe({
            sessionID: SESSION,
            directory: "/tmp",
            topics,
            port: 0,
            title: "newer subject reader",
            driving: false,
            selfSubscribed: true,
            plugin: PLUGIN,
            delivery: request,
          });
        },
        answered: (result) => delivery.answered(result.delivery),
      });

      await client.publish({
        sourceSessionID: SENDER,
        topic: TOPIC,
        message: "published under the newer build's subject",
      });
      // The row awaits the frame itself. The bound only turns a frame that never comes (the
      // topic out of the consumer's filter) into a failure that says so, since a real listener
      // and natsd run on the platform clock and no fake timer reaches them.
      const timer = setTimeout(
        () => arrived.reject(new Error(`no frame on ${TOPIC} within ${DELIVERY_BOUND_MS} ms`)),
        DELIVERY_BOUND_MS
      );
      try {
        await arrived.promise;
      } finally {
        clearTimeout(timer);
      }
      expect(delivered).toEqual(["published under the newer build's subject"]);
      // The client carried the topic, and the listener, judging on the deployed stream's
      // subjects, stored it.
      expect(sent).toEqual([[own, TOPIC]]);
      expect(answer.topics).toContain(TOPIC);
      expect(failures).toEqual([]);
    } finally {
      await nats.close();
    }
  }, 60_000);
});
