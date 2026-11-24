// packages/envoy-client/src/__tests__/listener-relay-url-join.test.ts
//
// The relay socket's upgrade goes to its route joined onto the listener URL's path, the URL's
// query kept as a query, and the proof of the upgrade names that URL less the query as its `htu`
// (RFC 9449: the target URI without query or fragment). Appended to the URL's text, the route sat
// inside a query (`/listener?key=v/v1/sessions/ses_a/relay`) and the upgrade asked the URL's own
// path. A listener URL with no query is asked as before. The stand-in records the path, the query
// and the proof's `htu` of each upgrade, and takes any of them.

import { afterEach, describe, expect, test } from "bun:test";
import { newDpopKey } from "@legion/cipher-client/dpop";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { ListenerRelay } from "../listener-relay";

const stops: (() => unknown)[] = [];
afterEach(async () => {
  for (const stop of stops.splice(0)) await stop();
});

describe("the relay socket opens at its route joined onto the listener URL's path", () => {
  for (const [name, at, query] of [
    ["query", "/listener?key=v", "?key=v"],
    ["query-trailing-slash", "/listener/?key=v", "?key=v"],
    ["no-query", "/listener", ""],
  ] as const) {
    test(name, async () => {
      const upgrades: string[][] = [];
      const listener = Bun.serve({
        hostname: "127.0.0.1",
        port: 0,
        fetch(request, server) {
          const url = new URL(request.url);
          const proof = (request.headers.get("DPoP") ?? "").split(".")[1] ?? "";
          const claims = JSON.parse(Buffer.from(proof, "base64url").toString("utf8") || "{}");
          upgrades.push([url.pathname, url.search, String(claims.htu)]);
          return server.upgrade(request, {
            headers: { "Sec-WebSocket-Protocol": "envoy-relay.v1" },
          })
            ? undefined
            : new Response("not an upgrade", { status: 400 });
        },
        websocket: { message: () => undefined },
      });
      stops.push(() => listener.stop(true));
      const origin = `http://127.0.0.1:${listener.port}`;
      const key = await newDpopKey();
      const token = fixtureSecret("relay");
      const relay = new ListenerRelay({
        url: `${origin}${at}`,
        session: "ses_a",
        credential: {
          scheme: "DPoP",
          headers: async (method, url) => ({
            Authorization: `DPoP ${token}`,
            DPoP: await key.proof({ method, url, accessToken: token }),
          }),
          secret: () => token,
        },
        log: () => undefined,
      });
      stops.push(() => relay.close());

      await relay.opened(AbortSignal.timeout(10_000));
      expect(upgrades).toEqual([
        ["/listener/v1/sessions/ses_a/relay", query, `${origin}/listener/v1/sessions/ses_a/relay`],
      ]);
    });
  }
});
