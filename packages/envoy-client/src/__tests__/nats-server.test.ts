import { afterEach, describe, expect, setDefaultTimeout, test } from "bun:test";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { connectedClients } from "../../test/nats-server";

// Each row waits on facts its stand-in monitor reports, never on a span; the bound is a hang guard.
setDefaultTimeout(HANG_GUARD_MS);

/**
 * A stand-in NATS monitor on 127.0.0.1: its first /connz request is answered with `first` (none
 * when `first` is undefined, so that request is held too), and every later request is held until
 * its client cuts it. `held` resolves at the first request it holds; `requests` counts them all.
 */
function standInMonitor(first: string[] | undefined): {
  readonly address: string;
  readonly held: Promise<void>;
  requests(): number;
  stop(): void;
} {
  const { promise: held, resolve: holding } = Promise.withResolvers<void>();
  let requests = 0;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      requests += 1;
      if (requests === 1 && first !== undefined) {
        return Response.json({ connections: first.map((name, i) => ({ cid: i + 1, name })) });
      }
      holding();
      const { promise, resolve } = Promise.withResolvers<Response>();
      request.signal.addEventListener("abort", () => resolve(new Response(null, { status: 499 })), {
        once: true,
      });
      return promise;
    },
  });
  return {
    address: `127.0.0.1:${server.port}`,
    held,
    requests: () => requests,
    stop: () => server.stop(true),
  };
}

describe("connectedClients", () => {
  const monitors: { stop(): void }[] = [];
  afterEach(() => {
    for (const monitor of monitors.splice(0)) monitor.stop();
  });

  // The path every stop takes when a client was left open and no caller named it: the read polls
  // until its window ends, and the window cuts the poll in flight. The earlier poll's answer is
  // the read's answer, so the client is named; before, the cut poll threw and the stop said only
  // that its clients "could not be read".
  test("a poll the window cuts after an earlier poll answered gives that answer", async () => {
    const monitor = standInMonitor(["stray-client"]);
    monitors.push(monitor);
    const window = new AbortController();
    const read = connectedClients(monitor.address, window.signal, []);
    await monitor.held;
    window.abort();
    expect(await read).toEqual(["stray-client"]);
  });

  test("a first poll the window cuts throws, since nothing was read", async () => {
    const monitor = standInMonitor(undefined);
    monitors.push(monitor);
    const window = new AbortController();
    const read = connectedClients(monitor.address, window.signal, []);
    await monitor.held;
    window.abort();
    await expect(read).rejects.toThrow();
  });

  // The read ends on its first answer, since the server's clients are exactly the ones left open.
  // A read that polled again would have its second poll held; the window is aborted at that hold,
  // so such a read still returns, and the count of requests the stand-in saw tells the two apart.
  test("a read ends at its first answer when the server's clients are the ones left open", async () => {
    const monitor = standInMonitor(["leftover-client"]);
    monitors.push(monitor);
    const window = new AbortController();
    monitor.held.then(() => window.abort());
    expect(await connectedClients(monitor.address, window.signal, ["leftover-client"])).toEqual([
      "leftover-client",
    ]);
    expect(monitor.requests()).toBe(1);
  });
});
