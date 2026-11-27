import { afterAll, describe, expect, test } from "bun:test";
import { bearerCredential } from "../../../packages/envoy-client/src/bearer.ts";
import { get, Rate } from "../display-read-census.ts";

/**
 * Where the display-read census asks Dispatch: each route joined onto the configured URL's path,
 * the configured query ahead of the route's own, against a stand-in Dispatch served under a path
 * and a query that answers 404 anywhere else, as a Dispatch behind a path-routed gateway would.
 */

const ROUTE = "/api/v1/issues?project=ACME";
const LISTED = [{ key: "the-issue" }];

/** Every request the stand-in took, as `<path><query>`. */
const asked: string[] = [];
/** Where the stand-in is served: under `mount`, with `query` ahead of the route's own. */
let served = { mount: "", query: "" };

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  fetch(request) {
    const url = new URL(request.url);
    asked.push(`${url.pathname}${url.search}`);
    if (
      request.headers.get("authorization") === "Bearer token" &&
      url.pathname === `${served.mount}/api/v1/issues` &&
      url.search === (served.query === "" ? "?project=ACME" : `?${served.query}&project=ACME`)
    ) {
      return Response.json(LISTED);
    }
    return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
  },
});
const origin = `http://127.0.0.1:${server.port}`;

afterAll(() => server.stop(true));

/** What one `get` of `ROUTE` against `url` asked, the stand-in served under `mount` and `query`,
 *  and what it answered. */
async function askedBy(url: string, mount: string, query: string) {
  served = { mount, query };
  asked.length = 0;
  const config = { url, credential: bearerCredential("token") };
  const answer = await get(config, ROUTE, new Rate(1000), { requests: 0 }).catch(
    (error: unknown) => error
  );
  return { asked: [...asked], answer };
}

describe("display-read-census's get", () => {
  test("joins the route onto a configured URL's path and keeps its query", async () => {
    const { asked, answer } = await askedBy(
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "key=k3y-made-up"
    );
    expect(asked).toEqual(["/prefix/api/v1/issues?key=k3y-made-up&project=ACME"]);
    expect(answer).toEqual(LISTED);
  });

  test("asks a plain origin at the route itself", async () => {
    const { asked, answer } = await askedBy(origin, "", "");
    expect(asked).toEqual([ROUTE]);
    expect(answer).toEqual(LISTED);
  });
});
