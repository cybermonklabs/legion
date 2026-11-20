import { expect, test } from "bun:test";
import { netCallAddress } from "./net-call-address";

// The dial counter of extensions/envoy.test.ts and the held-port preload
// test/nats-default-port-held.ts see a call of port 4222 only through this decoder, so a decoder
// that misreads a form makes both blind to it: the counter's rows then pass with no dial counted.

test("a call names its port and host as node:net reads them: (port, host) or (options)", () => {
  // nats.js's node transport dials `net.createConnection(port, hostname)`.
  expect(netCallAddress([4222, "127.0.0.1"])).toEqual({ port: 4222, host: "127.0.0.1" });
  expect(netCallAddress(["4222", "localhost", () => {}])).toEqual({
    port: 4222,
    host: "localhost",
  });
  expect(netCallAddress([4222, () => {}])).toEqual({ port: 4222, host: "" });
  expect(netCallAddress([{ port: 4222, host: "127.0.0.1" }])).toEqual({
    port: 4222,
    host: "127.0.0.1",
  });
  // `Bun.listen` names its host `hostname`, which is no `host`: only the port is read.
  expect(netCallAddress([{ hostname: "127.0.0.1", port: "4222" }])).toEqual({
    port: 4222,
    host: "",
  });
  expect(netCallAddress([0])).toEqual({ port: 0, host: "" });
});

test("a call that names no TCP port is no address: a unix socket, a blank, a fraction or a port past 65535", () => {
  for (const args of [
    ["/tmp/nats.sock"],
    [{ path: "/tmp/nats.sock" }],
    [""],
    [" "],
    [4222.5],
    [65_536],
    [-1],
    [{ port: undefined }],
    [],
  ]) {
    expect({ args, address: netCallAddress(args) }).toEqual({ args, address: undefined });
  }
});
