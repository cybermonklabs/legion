// packages/envoy-client/src/__tests__/transport-url-join.test.ts
//
// Every request the Envoy client sends goes to its route joined onto the configured listener URL's
// path, the URL's query kept ahead of the route's own. Appended to the URL's text, the route sat
// inside the query (`/prefix?key=v/v1/sessions`), so the listener was asked the URL's own path. A
// listener URL with no path or query is asked as before. The stand-in records the path and the
// query of each request it is asked.
//
// A listener URL the join refuses fails the call before anything is sent, in fixed words that
// quote none of it.

import { afterEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { createEnvoyClient, EnvoySendUnconfirmedError } from "../transport";

const stops: (() => unknown)[] = [];
afterEach(async () => {
  for (const stop of stops.splice(0)) await stop();
});

/** A listener stand-in that records `[method, path, query]` of each request and answers `body`. */
function standIn(body: string) {
  const asked: string[][] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      asked.push([request.method, url.pathname, url.search]);
      return new Response(body, { headers: { "Content-Type": "application/json" } });
    },
  });
  stops.push(() => server.stop(true));
  return { asked, origin: `http://127.0.0.1:${server.port}` };
}

describe("an Envoy request goes to its route joined onto the listener URL's path", () => {
  for (const [name, at, prefix, query] of [
    ["path-and-query", "/prefix?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
    ["path-trailing-slash-and-query", "/prefix/?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
    ["plain-origin", "", "", ""],
  ] as const) {
    test(name, async () => {
      const { asked, origin } = standIn("[]");
      const client = createEnvoyClient({ baseUrl: `${origin}${at}`, fetch, retry: false });

      await client.listSessions();
      await client.unsubscribe({ sessionID: "ses_a", topics: ["agent.ses_a"] });
      await client.unregisterSession({ sessionID: "ses_a", clientInstanceID: "inst-1" });

      const own = "client_instance_id=inst-1";
      expect(asked).toEqual([
        ["GET", `${prefix}/v1/sessions`, query],
        ["POST", `${prefix}/v1/interests/unsubscribe`, query],
        ["DELETE", `${prefix}/v1/sessions/ses_a`, query === "" ? `?${own}` : `${query}&${own}`],
      ]);
    });
  }

  test("an answer that is not JSON names the joined URL it came from", async () => {
    const { origin } = standIn("not json");
    const client = createEnvoyClient({ baseUrl: `${origin}/prefix`, fetch, retry: false });
    await expect(client.listSessions()).rejects.toThrow(
      `GET ${origin}/prefix/v1/sessions answered with`
    );
  });
});

describe("a listener URL the join refuses fails the call before it is sent", () => {
  const user = "user-7f2a";
  const madeUp = fixtureSecret("pw");
  const base = `http://${user}:${madeUp}@[listener`;
  const refused =
    "the configured Envoy URL is not an absolute URL with a host, or its route is not a relative reference (neither is printed)";

  test("a read and a send", async () => {
    const asked: string[] = [];
    const recording = (async (input: string | URL | Request) => {
      asked.push(String(input));
      return new Response("[]");
    }) as typeof fetch;
    const client = createEnvoyClient({ baseUrl: base, fetch: recording });

    const read = (await client.listSessions().then(
      () => null,
      (error: unknown) => error
    )) as Error;
    expect(read.message).toBe(`GET was not sent: ${refused}`);

    // A send that sent nothing is refused as such, never left unconfirmed.
    const send = (await client
      .send({ source: "human", targetSessionID: "ses_b", message: "hello" })
      .then(
        () => null,
        (error: unknown) => error
      )) as Error;
    expect(send).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect(send.message).toBe(`POST was not sent: ${refused}`);

    expect(asked).toEqual([]);
    for (const error of [read, send]) expect(Bun.inspect(error)).not.toContain(madeUp);
  });
});
