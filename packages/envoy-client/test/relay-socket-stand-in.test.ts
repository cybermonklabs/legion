// packages/envoy-client/test/relay-socket-stand-in.test.ts
//
// The stand-in itself (`./relay-socket-stand-in.ts`): a driver's own routes answer on the one
// origin before the stand-in's trade and its 404, and the relay upgrade never reaches them.

import { describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { opens, relaySocketFixtures } from "./relay-socket-fixtures";
import { startRelaySocketStandIn } from "./relay-socket-stand-in";

const { started, opened } = relaySocketFixtures("relay-socket-stand-in");

describe("a relay socket", () => {
  test("the stand-in asks a test's own routes first, and the trade and 404 take what they leave", async () => {
    // A driver registers and sends at the listener whose socket it opens, and serves its device
    // login's grants at the broker that trades its session's token, so its routes answer on the
    // one origin, the token endpoint included. Each is handed a copy of the request: one it reads
    // and leaves still reaches the trade whole. The relay upgrade is never handed to them.
    const asked: string[] = [];
    // Random, as a broker's token is: the client holds every token it is handed.
    const deviceToken = fixtureSecret("device");
    const server = startRelaySocketStandIn({
      routes: async (request) => {
        const { pathname } = new URL(request.url);
        const grant = new URLSearchParams(await request.text()).get("grant_type") ?? "";
        asked.push(`${request.method} ${pathname}${grant === "" ? "" : ` ${grant}`}`);
        if (pathname === "/v1/subscribe") return Response.json({ registered: true });
        if (grant === "urn:ietf:params:oauth:grant-type:device_code") {
          return Response.json({ access_token: deviceToken });
        }
        return undefined;
      },
    });
    started.push(() => server.stop());
    const registered = await fetch(`${server.url}/v1/subscribe`, { method: "POST" });
    expect(await registered.json()).toEqual({ registered: true });
    const device = await fetch(`${server.url}/v1/oauth/token`, {
      method: "POST",
      body: new URLSearchParams({ grant_type: "urn:ietf:params:oauth:grant-type:device_code" }),
    });
    expect(await device.json()).toEqual({ access_token: deviceToken });
    expect((await fetch(`${server.url}/v1/messages/send`, { method: "POST" })).status).toBe(404);
    // The session's own trade goes through the routes and on to the stand-in's trade.
    await opened(server, "ses_a");
    expect(asked).toEqual([
      "POST /v1/subscribe",
      "POST /v1/oauth/token urn:ietf:params:oauth:grant-type:device_code",
      "POST /v1/messages/send",
      "POST /v1/oauth/token urn:ietf:params:oauth:grant-type:token-exchange",
    ]);
    expect(server.minted.map((token) => token.session)).toEqual(["ses_a"]);
    expect(opens(server).map((upgrade) => upgrade.session)).toEqual(["ses_a"]);
  });
});
