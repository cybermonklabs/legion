// packages/cipher-client/src/__tests__/broker-url-join.test.ts
//
// Every request to the broker goes to its route joined onto the broker URL's path, the URL's query
// kept as a query: the device login's authorization, poll and refresh, and the broker client's
// trade. Appended to the URL's text, the route sat inside a query
// (`/broker?key=v/v1/oauth/token`) and the broker was asked the URL's own path. A broker URL with
// no query is asked as before. The stand-in sits at a path of its own in front of the stand-in
// brokers, records the path and the query each request reached it with, and hands on the request
// with its path less that prefix.

import { afterEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { stubDeviceLoginBroker, writeDeviceLoginFile } from "../../test/device-logins";
import { DeviceLoginIdentity, finishDeviceLogin, startDeviceLogin } from "../device-login";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

const PREFIX = "/broker";

const makeDirectory = temporaryDirectories("broker-url-join");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A stand-in at `PREFIX` on an origin of its own in front of the broker at `brokerUrl`: it
 *  records the path and the query of each request, and hands on the ones under `PREFIX` with
 *  their path less it and no query. */
function prefixed(brokerUrl: string): { readonly origin: string; readonly asked: string[][] } {
  const asked: string[][] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const url = new URL(request.url);
      asked.push([url.pathname, url.search]);
      if (!url.pathname.startsWith(`${PREFIX}/`)) return new Response("not found", { status: 404 });
      const headers = new Headers(request.headers);
      headers.delete("host");
      return await fetch(`${brokerUrl}${url.pathname.slice(PREFIX.length)}`, {
        method: request.method,
        headers,
        body: await request.arrayBuffer(),
      });
    },
  });
  stops.push(() => server.stop(true));
  return { origin: `http://127.0.0.1:${server.port}`, asked };
}

/** The broker URLs of the rows, each a path under the stand-in's origin, and the query each
 *  request must reach the broker with. */
const CASES = [
  ["query", `${PREFIX}?key=v`, "?key=v"],
  ["query-trailing-slash", `${PREFIX}/?key=v`, "?key=v"],
  ["no-query", PREFIX, ""],
] as const;

// The three requests of a device login, each sent on its own so each is held at the base too: the
// authorization; the poll of an authorization the stand-in answered directly; and the refresh of
// a login with no access token, which runs before its first trade.
describe("a device login asks each route joined onto the broker URL's path", () => {
  for (const [name, at, query] of CASES) {
    test(name, async () => {
      const stub = stubDeviceLoginBroker();
      stops.push(stub.stop);
      const front = prefixed(stub.url);
      const brokerUrl = `${front.origin}${at}`;
      const login = { person: "ada@example.com", host: "adas-laptop.local" };
      const settled = (step: Promise<unknown>) =>
        step.then(
          () => "done",
          (error: unknown) => String(error)
        );

      const authorized = await settled(startDeviceLogin({ brokerUrl, ...login }));
      const authorization = front.asked.splice(0);
      const direct = await startDeviceLogin({ brokerUrl: stub.url, ...login });
      const path = join(makeDirectory(), "legion", "device-login.json");
      const polled = await settled(
        finishDeviceLogin({ ...direct, brokerUrl }, { path, sleep: async () => {} })
      );
      const poll = front.asked.splice(0);
      const refreshing = join(makeDirectory(), "legion", "device-login.json");
      await writeDeviceLoginFile(refreshing, brokerUrl);
      const refreshed = await settled(
        new DeviceLoginIdentity({ path: refreshing, brokerUrl }).spend(async () => {})
      );
      const refresh = front.asked.splice(0);

      expect({ authorization, poll, refresh }).toEqual({
        authorization: [[`${PREFIX}/v1/oauth/device_authorization`, query]],
        poll: [[`${PREFIX}/v1/oauth/token`, query]],
        refresh: [[`${PREFIX}/v1/oauth/token`, query]],
      });
      expect([authorized, polled, refreshed]).toEqual(["done", "done", "done"]);
      expect([stub.authorizations.length, stub.polls.length, stub.refreshes.length]).toEqual([
        2, 1, 1,
      ]);
    });
  }
});

describe("the broker client trades at the token route joined onto the broker URL's path", () => {
  for (const [name, at, query] of CASES) {
    test(name, async () => {
      const broker = stubBroker(() => ({ body: accepted("traded", 900) }));
      stops.push(broker.stop);
      const front = prefixed(broker.url);
      const client = new BrokerTokenClient({
        brokerUrl: `${front.origin}${at}`,
        key: await newDpopKey(),
        subject: {
          read: async () => ({ token: "projected", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
        },
      });
      stops.push(() => client.close());

      const refusal = await refusalOf(() => client.token());
      expect(front.asked).toEqual([[`${PREFIX}/v1/oauth/token`, query]]);
      expect(refusal).toBeNull();
      expect(broker.trades).toHaveLength(1);
    });
  }
});
