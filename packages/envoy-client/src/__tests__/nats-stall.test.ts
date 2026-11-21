import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { until } from "@legion/test-fixtures/wait";
import { connect, Events, StringCodec } from "nats";
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "../../test/envoy-stack";
import { startStallProxy } from "../../test/stall-proxy";
import {
  expectRecoveredByPingOut,
  type NatsStatusEvent,
  RECOVERY_BOUND_MS,
  recoveryBoundMs,
} from "../../test/stall-scenario";
import { natsConnectionOptions } from "../nats-connection";

const codec = StringCodec();

// The minute the spec allows is the sum of the timers that pace a recovery, so it is held on
// those settings rather than read off a clock (R4): nats.js drops a frozen path on the ping tick
// that finds `maxPingOut` pings unanswered, and a redial that hangs too costs its dial timeout
// and the reconnect wait before the next. The rows below show, from the connection's own status
// events, that a recovery takes exactly the steps this sum prices.
test("natsConnectionOptions leaves a frozen path and dials past a hung redial within the spec's minute, by its settings", () => {
  const options = natsConnectionOptions({
    servers: ["nats://127.0.0.1:4222"],
    name: "nats-stall-settings",
    credential: { kind: "none" },
  });
  const left = recoveryBoundMs(options, 0);
  const redialed = recoveryBoundMs(options, 1);
  expect(
    redialed,
    `a frozen path is left after ${left} ms and a hung redial passed after ${redialed} ms`
  ).toBeLessThan(RECOVERY_BOUND_MS);
});

// Reserved here, at the file's module scope: the file's own `afterAll` removes it once the
// `describe` below has stopped the stack writing into it.
const stackDirectory = useEnvoyStackDirectory();

describe.skipIf(!envoyStackEnabled)("a NATS connection whose path freezes", () => {
  let stack: EnvoyStack | undefined;

  beforeAll(async () => {
    stack = await startEnvoyStack({ work: stackDirectory });
  });
  afterAll(async () => {
    await stack?.stop();
  });

  for (const { row, slug, stalledRedials, unackedBytes } of [
    {
      row: "A: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze",
      slug: "a",
      stalledRedials: 0,
      unackedBytes: 0,
    },
    {
      row: "B: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze when its first redial also hangs, and closes the hung redial's socket once it gives up on it",
      slug: "b",
      stalledRedials: 1,
      unackedBytes: 0,
    },
    {
      row: "C: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze when it wrote data the frozen path never acknowledged",
      slug: "c",
      stalledRedials: 0,
      // Without patches/nats@2.29.3.patch's protocol.js hunk this row must fail: on Bun 1.4.4 a
      // 4 MiB backlog still drains into the kernel and the stale close's flush completes, 8 MiB
      // already hangs it, and 32 MiB leaves a margin.
      unackedBytes: 32 * 1024 * 1024,
    },
  ]) {
    test(row, async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start");
      const proxy = await startStallProxy(stack.natsHostPort);
      const publisher = await connect({ servers: [stack.natsUrl], name: `nats-stall-pub-${slug}` });
      const options = natsConnectionOptions({
        servers: [proxy.url],
        name: `nats-stall-${slug}`,
        credential: { kind: "none" },
      });
      const client = await connect(options);
      try {
        const subject = `nats-stall.${slug}.${crypto.randomUUID()}`;
        const before = Promise.withResolvers<void>();
        let arrivedAt: number | undefined;
        client.subscribe(subject, {
          callback: (error, message) => {
            if (error !== null) return;
            const body = codec.decode(message.data);
            if (body === "before") before.resolve();
            else arrivedAt ??= Date.now();
          },
        });
        await client.flush();
        publisher.publish(subject, codec.encode("before"));
        await publisher.flush();
        await before.promise;

        // Every status event nats.js reports from here on, in order, and when the reconnect came,
        // for the log line only.
        const statuses: NatsStatusEvent[] = [];
        let reconnectedAt: number | undefined;
        void (async () => {
          for await (const status of client.status()) {
            statuses.push({ type: status.type, data: status.data });
            if (status.type === Events.Reconnect) reconnectedAt ??= Date.now();
          }
        })();
        proxy.stallNext(stalledRedials);
        // The redials are the connections the proxy appended after this point. Taking them by an
        // ordinal rather than by acceptedAt >= frozenAt keeps a redial the proxy accepted inside
        // the freeze's own millisecond, which that comparison bins with the connections before it.
        const beforeFreeze = proxy.connections.length;
        const frozenAt = proxy.freeze();
        // The client's own writes after the freeze, in chunks below natsd's max_payload: more than
        // the frozen path's kernel buffers take, so they stay queued in its socket. Nothing awaits
        // a flush of them, which cannot complete.
        const chunk = new Uint8Array(64 * 1024);
        for (let sent = 0; sent < unackedBytes; sent += chunk.length) {
          client.publish(`${subject}.unacked`, chunk);
        }
        // nats.js's own ping and dial timers pace the recovery, so the wait is real time. A publish
        // reaches the client only once it has subscribed again on a path that answers, and each one
        // before that goes to the frozen connection and is lost, so each read of the wait sends one
        // more until one arrives.
        let attempt = 0;
        await until("a publish sent after the freeze to reach the client", async () => {
          if (arrivedAt !== undefined) return true;
          publisher.publish(subject, codec.encode(`after-${attempt}`));
          attempt += 1;
          await publisher.flush();
          return false;
        });

        const reconnectMs = reconnectedAt === undefined ? undefined : reconnectedAt - frozenAt;
        const receiveMs = arrivedAt === undefined ? undefined : arrivedAt - frozenAt;
        console.log(
          `nats-stall row ${slug.toUpperCase()}: reconnect ${reconnectMs === undefined ? "none" : `${(reconnectMs / 1000).toFixed(1)} s`}, first publish received ${receiveMs === undefined ? "never" : `${(receiveMs / 1000).toFixed(1)} s`} after the freeze`
        );
        // The reconnect's status event can follow the first publish that arrived on its path.
        await until("the client to report its reconnect", () => reconnectedAt !== undefined);
        // Within the minute, by the steps it took: the settings row prices a recovery of these
        // steps under it, and the connection's own events show it took them and no others.
        expect(recoveryBoundMs(options, stalledRedials)).toBeLessThan(RECOVERY_BOUND_MS);
        expectRecoveredByPingOut({ options, statuses }, stalledRedials);
        const redials = proxy.connections.slice(beforeFreeze);
        // The stalled redials never subscribed; the one after them did.
        expect(redials.length).toBeGreaterThan(stalledRedials);
        expect(
          redials.slice(0, stalledRedials).every((opened) => opened.subscribedAt === undefined)
        ).toBe(true);
        expect(redials[stalledRedials]?.subscribedAt).toBeDefined();
        // nats.js abandons a dial after its dial timeout; the socket it opened must go with it, so
        // the client holds no socket to a redial it gave up on. The proxy never closes a stalled
        // connection, so its close is the client's. It comes at the give-up, which is before the
        // redial after it: nats.js closes the transport of the dial it gave up on before its dial
        // loop dials again (`protocol.js`, `dial`'s catch, then `dodialLoop`), so a socket closed
        // at any later moment is closed after the proxy accepted that next redial.
        for (const [index, hung] of redials.slice(0, stalledRedials).entries()) {
          await until("the client to close the socket of the redial it gave up on", () => {
            return hung.closedAt !== undefined;
          });
          console.log(
            `nats-stall row ${slug.toUpperCase()} hung redial: accepted ${((hung.acceptedAt - frozenAt) / 1000).toFixed(1)} s after the freeze, closed ${(((hung.closedAt ?? Number.NaN) - hung.acceptedAt) / 1000).toFixed(1)} s after it was accepted`
          );
          expect(hung.closedAt).toBeLessThanOrEqual(redials[index + 1]?.acceptedAt ?? Number.NaN);
        }
      } finally {
        await client.close();
        await publisher.close();
        await proxy.close();
      }
    });
  }
});
