// packages/pi-legion/scripts/skill-scenarios/production-dispatch-url-join.test.ts
//
// A read of ProductionDispatch goes to its route joined onto the Dispatch URL's path, the URL's
// query kept as a query (`joinUrl`). Appended to the URL's text, the route sat inside the query
// (`/prefix?key=v/api/v1/issues/...`) and Dispatch was asked the URL's own path. A Dispatch URL
// that is a plain origin is asked as before. The stand-in Dispatch records the path and the query
// of each request.

import { afterEach, describe, expect, test } from "bun:test";
import { bearerCredential } from "@legion/envoy-client/bearer";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { ProductionDispatch } from "./production-dispatch";

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A Dispatch on an origin of its own: it records each request and answers an empty list, at
 *  whatever path it was asked. */
function standIn(): { readonly origin: string; readonly asked: string[][] } {
  const asked: string[][] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(request) {
      const url = new URL(request.url);
      asked.push([url.pathname, url.search]);
      return Response.json([]);
    },
  });
  stops.push(() => server.stop(true));
  return { origin: `http://127.0.0.1:${server.port}`, asked };
}

/** The Dispatch URLs of the rows, each a path under the stand-in's origin, the path each route
 *  must reach Dispatch under, and the query it must reach it with. */
const CASES = [
  ["query", "/prefix?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
  ["query-trailing-slash", "/prefix/?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
  ["no-query", "", "", ""],
] as const;

describe("a production Dispatch read asks its route joined onto the Dispatch URL's path", () => {
  for (const [name, at, under, query] of CASES) {
    test(name, async () => {
      const dispatch = standIn();
      const reader = new ProductionDispatch(
        { url: `${dispatch.origin}${at}`, credential: bearerCredential(fixtureSecret("dsp")) },
        (url, init) => fetch(url, init),
        async () => {}
      );

      const comments = await reader.comments("demo");

      expect({ asked: dispatch.asked, comments }).toEqual({
        asked: [[`${under}/api/v1/issues/demo/comments`, query]],
        comments: [],
      });
    });
  }
});
