import { afterEach, expect, test } from "bun:test";
import { loopbackHost } from "./loopback-host";

const linux = process.platform === "linux";
const stops: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const stop of stops.splice(0).reverse()) await stop();
});

// A fixed address, 127.0.0.1 or another, would be shared by every server that asks for one, and a
// client left dialing one test's port could reach the next server given that port.
test("on Linux it is an address of 127.0.0.0/8 that is not 127.0.0.1 and not one for every server; 127.0.0.1 elsewhere", () => {
  const hosts = Array.from({ length: 50 }, loopbackHost);
  if (!linux) {
    expect(new Set(hosts)).toEqual(new Set(["127.0.0.1"]));
    return;
  }
  for (const host of hosts) {
    const octets = host.split(".").map(Number);
    expect(octets).toHaveLength(4);
    expect(octets[0]).toBe(127);
    expect(octets.every((octet) => Number.isInteger(octet) && octet >= 0 && octet <= 255)).toBe(
      true
    );
    expect(host).not.toBe("127.0.0.1");
  }
  expect(new Set(hosts).size).toBeGreaterThan(1);
});

// The case the address exists for: a client that dials 127.0.0.1 at the server's port, as one left
// from a server that held that port there does, does not reach the server. Another process may
// hold 127.0.0.1 at that port and answer, so what the row reads is the server's own count.
test.skipIf(!linux)(
  "a server there is reached at its own address and not by a client dialing 127.0.0.1 at its port",
  async () => {
    let served = 0;
    const server = Bun.serve({
      port: 0,
      hostname: loopbackHost(),
      fetch() {
        served += 1;
        return new Response("served");
      },
    });
    stops.push(() => server.stop(true));
    expect(server.url.hostname).not.toBe("127.0.0.1");
    expect(await (await fetch(server.url)).text()).toBe("served");
    await fetch(`http://127.0.0.1:${server.port}/`).then(
      (answer) => answer.body?.cancel(),
      () => undefined
    );
    expect(served).toBe(1);
  }
);
