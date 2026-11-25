// packages/pi-shared/src/daemon-client-url-join.test.ts
//
// Every daemon request goes to its route joined onto the daemon URL's path, the URL's query kept
// as a query (`joinUrl`): a GET and a POST, which share the client's one request. Appended to the
// URL's text, the route sat inside the query (`/prefix?key=v/legion/v1/claims/ready`) and the
// daemon was asked the URL's own path. A daemon URL that is a plain origin is asked as before.
// The stand-in daemon records the method, the path and the query of each request.

import { afterEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { createLegionDaemonClient } from "./daemon-client";

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A daemon on an origin of its own: it records each request, answers a GET with its contract
 *  header and a POST with the empty 204, at whatever path it was asked. */
function standIn(): { readonly origin: string; readonly asked: string[][] } {
  const asked: string[][] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(request) {
      const url = new URL(request.url);
      asked.push([request.method, url.pathname, url.search]);
      if (request.method === "GET") {
        return Response.json({}, { headers: { "Legion-Daemon-Contract": "60" } });
      }
      return new Response(null, { status: 204 });
    },
  });
  stops.push(() => server.stop(true));
  return { origin: `http://127.0.0.1:${server.port}`, asked };
}

/** The daemon URLs of the rows, each a path under the stand-in's origin, the path each route
 *  must reach the daemon under, and the query it must reach it with. */
const CASES = [
  ["query", "/prefix?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
  ["query-trailing-slash", "/prefix/?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
  ["no-query", "", "", ""],
] as const;

describe("a daemon request asks its route joined onto the daemon URL's path", () => {
  for (const [name, at, under, query] of CASES) {
    test(name, async () => {
      const daemon = standIn();
      const client = createLegionDaemonClient(`${daemon.origin}${at}`, { pluginContract: 60 });

      const contract = await client.daemonContract();
      await client.ready({
        claimToken: "legion-legion-legion-208-architect",
        sessionId: "ses_architect_208",
        secret: fixtureSecret("claim"),
        generation: 3,
      });

      expect({ asked: daemon.asked, contract }).toEqual({
        asked: [
          ["GET", `${under}/healthz`, query],
          ["POST", `${under}/legion/v1/claims/ready`, query],
        ],
        contract: 60,
      });
    });
  }
});
