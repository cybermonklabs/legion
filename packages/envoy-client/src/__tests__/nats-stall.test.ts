import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { settled, until } from "@legion/test-fixtures/wait";
import { connect, Events, StringCodec } from "nats";
import {
  type EnvoyStack,
  envoyStackEnabled,
  releaseAll,
  startStoppableEnvoyStack,
  throwIfExited,
  throwIfRowEnded,
  useEnvoyStackDirectory,
} from "../../test/envoy-stack";
import { startStallProxy } from "../../test/stall-proxy";
import { RECOVERY_BOUND_MS } from "../../test/stall-scenario";
import { natsConnectionOptions } from "../nats-connection";

const codec = StringCodec();

/** `ms` as the seconds this file's logs print. */
const seconds = (ms: number): string => `${(ms / 1000).toFixed(1)} s`;

// Reserved here, at the file's module scope: the file's own `afterAll` removes it once the
// `describe` below has stopped the stack writing into it.
const stackDirectory = useEnvoyStackDirectory();

describe.skipIf(!envoyStackEnabled)("a NATS connection whose path freezes", () => {
  let stack: EnvoyStack | undefined;
  let stopStack: (() => Promise<void>) | undefined;

  beforeAll(async () => {
    const start = startStoppableEnvoyStack({ work: stackDirectory });
    stopStack = start.stop;
    // What can hang is the stack's start, which builds natsd and the listener with `go build`
    // and waits for each to serve: the start the live describes of pi-envoy and claude-envoy make.
    stack = await settled("the Envoy stack's start", start.started);
  });
  afterAll(async () => {
    // What can hang is the stop of natsd or the listener, which a start cut off makes too.
    if (stopStack !== undefined) await settled("the Envoy stack's stop", stopStack());
  });

  // The row running: its signal, which each of its waits checks on every poll, and the close of
  // each thing it opened, held from the moment the open starts. The `afterEach` below aborts the
  // one and runs the others, so a row that ended, at its guard included, stops polling and leaves
  // no client, publisher or proxy open into the row after it.
  let rowEnd = new AbortController();
  const closes: (() => Promise<void>)[] = [];
  /** `opening`, whose result the row closes once it has ended, an open its guard cut off
   *  included: the close is held from the moment the open starts, and waits for the open to
   *  settle. */
  const closedWithRow = <T extends { close(): Promise<void> }>(opening: Promise<T>): Promise<T> => {
    closes.push(async () => {
      await (await opening.catch(() => undefined))?.close();
    });
    return opening;
  };
  beforeEach(() => {
    rowEnd = new AbortController();
  });
  afterEach(async () => {
    rowEnd.abort(new Error("the row ended"));
    // What can hang is a close, or a connect the row's end cut off, which its own dial timeout
    // ends: nats.js's 20 s default for the publisher, `NATS_DIAL_TIMEOUT_MS`'s 10 s for the
    // client. The opens run one after another, so at most one is pending.
    await settled("the row's closes", releaseAll(closes, "the row's closes"));
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
      // Held in a const, so the waits' probes below read it narrowed.
      const envoy = stack;
      if (envoy === undefined) throw new Error("the Envoy stack did not start");
      const { signal: ended } = rowEnd;
      const proxy = await closedWithRow(startStallProxy(envoy.natsHostPort));
      const publisher = await closedWithRow(
        connect({ servers: [envoy.natsUrl], name: `nats-stall-pub-${slug}` })
      );
      const client = await closedWithRow(
        connect(
          natsConnectionOptions({
            servers: [proxy.url],
            name: `nats-stall-${slug}`,
            credential: { kind: "none" },
          })
        )
      );
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
      await settled("the message published before the freeze", before.promise);

      let reconnectedAt: number | undefined;
      void (async () => {
        for await (const status of client.status()) {
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
      // What is measured is nats.js's own wall-clock ping and dial timers, so the wait is real
      // time. A publish reaches the client only once it has subscribed again on a path that
      // answers, and each one before that goes to the frozen connection and is lost, so one
      // goes out every quarter second, the row's pacing of its send, until one arrives or the
      // bound passes. A natsd or listener that exits meanwhile fails the wait by name
      // (`EnvoyStack.exited`), and so does the row's end.
      const recovering = "a publish to arrive, or the recovery's bound to pass";
      let attempt = 0;
      const publish = (): void => {
        // A row that ended publishes nothing more: the `afterEach` closes the publisher after it
        // has aborted the row's signal.
        if (ended.aborted) return;
        publisher.publish(subject, codec.encode(`after-${attempt}`));
        attempt += 1;
      };
      publish();
      const publishing = setInterval(publish, 250);
      try {
        await until(recovering, () => {
          throwIfRowEnded(ended, `waiting for ${recovering}`);
          throwIfExited(envoy, `waiting for ${recovering}`);
          return arrivedAt !== undefined || Date.now() - frozenAt >= RECOVERY_BOUND_MS;
        });
      } finally {
        clearInterval(publishing);
      }

      const reconnectMs = reconnectedAt === undefined ? undefined : reconnectedAt - frozenAt;
      const receiveMs = arrivedAt === undefined ? undefined : arrivedAt - frozenAt;
      const reconnected = reconnectMs === undefined ? "none" : seconds(reconnectMs);
      const received = receiveMs === undefined ? "never" : seconds(receiveMs);
      const label = `nats-stall row ${slug.toUpperCase()}`;
      console.log(
        `${label}: reconnect ${reconnected}, first publish received ${received} after the freeze`
      );
      expect(reconnectMs).toBeDefined();
      expect(reconnectMs ?? Number.POSITIVE_INFINITY).toBeLessThan(RECOVERY_BOUND_MS);
      expect(receiveMs).toBeDefined();
      expect(receiveMs ?? Number.POSITIVE_INFINITY).toBeLessThan(RECOVERY_BOUND_MS);
      const redials = proxy.connections.slice(beforeFreeze);
      // The stalled redials never subscribed; the one after them did.
      expect(redials.length).toBeGreaterThan(stalledRedials);
      expect(
        redials.slice(0, stalledRedials).every((opened) => opened.subscribedAt === undefined)
      ).toBe(true);
      expect(redials[stalledRedials]?.subscribedAt).toBeDefined();
      // nats.js abandons a dial after its dial timeout; the socket it opened must go with it, so
      // the client holds no socket to a redial it gave up on. The proxy never closes a stalled
      // connection, so its close is the client's. Real nats.js dial timers: the wait polls the
      // real clock, and fails by name once natsd or the listener has exited or the row has
      // ended. It has no deadline of its own: if the client never closes the socket, the wait
      // fails at its row's deadline, naming the close it waited for.
      const closing = "the client to close a hung redial";
      for (const hung of redials.slice(0, stalledRedials)) {
        const closedAt = await until(closing, () => {
          throwIfRowEnded(ended, `waiting for ${closing}`);
          throwIfExited(envoy, `waiting for ${closing}`);
          return hung.closedAt;
        });
        const accepted = seconds(hung.acceptedAt - frozenAt);
        const closed = seconds(closedAt - hung.acceptedAt);
        console.log(
          `${label} hung redial: accepted ${accepted} after the freeze, closed ${closed} after it was accepted`
        );
      }
      // What can hang is a wait for the freeze's recovery or for the client's close of a hung
      // redial.
    });
  }
});
