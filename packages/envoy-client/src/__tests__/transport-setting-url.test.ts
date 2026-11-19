// packages/envoy-client/src/__tests__/transport-setting-url.test.ts
//
// The Envoy client of a URL that `shownSettingUrl` does not show refuses each call before any
// request: the URL's user part may be read in part as the host, and fetch's own error would name
// that host (a lookup of a piece of a token), and a send would be taken as unconfirmed. The
// settings turn Envoy off for such a URL before any client is built; this row holds the client to
// the same rule for a client built from any other value. The control is the same listener's URL
// with its user part percent-encoded, which the listener is asked on. The password is made for this
// run.

import { expect, test } from "bun:test";
import { inspect } from "node:util";
import { unshownSettingUrl } from "@legion/contracts/setting-url";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { createEnvoyClient, EnvoyUrlError } from "../transport";

test("a client of an Envoy URL shownSettingUrl does not show refuses each call before any request, naming its length and cause", async () => {
  const password = fixtureSecret("pw");
  let asked = 0;
  const listener = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      asked += 1;
      return Response.json([]);
    },
  });
  try {
    const refused = [
      `http://made-up-user:ab/${password}@127.0.0.1:${listener.port}`,
      `http://MadeUpTok3n/${password}@127.0.0.1:${listener.port}`,
      `http://127.0.0.1:${listener.port}\n`,
    ];
    const said: { readonly kind: unknown; readonly message: string; readonly holds: boolean }[] =
      [];
    for (const baseUrl of refused) {
      const client = createEnvoyClient({ baseUrl, fetch, retry: false });
      for (const call of [
        client.listSessions(),
        client.send({
          sourceSessionID: "ses_sender",
          targetSessionID: "ses_receiver",
          message: "hi",
        }),
      ]) {
        const error = await call.then(
          () => undefined,
          (thrown: unknown) => thrown
        );
        said.push({
          kind: error instanceof EnvoyUrlError,
          message: error instanceof Error ? error.message : String(error),
          holds: inspect(error, { depth: Number.POSITIVE_INFINITY, showHidden: true }).includes(
            password
          ),
        });
      }
    }
    expect({ said, asked }).toEqual({
      said: refused.flatMap((baseUrl) =>
        [1, 2].map(() => ({
          kind: true,
          message: `the Envoy URL (ENVOY_URL, or envoy.json's envoy.url) must be an http or https URL: ${unshownSettingUrl(baseUrl)}; nothing was sent`,
          holds: false,
        }))
      ),
      asked: 0,
    });
    const encoded = `http://made-up-user:ab%2F${password}@127.0.0.1:${listener.port}`;
    expect(
      await createEnvoyClient({ baseUrl: encoded, fetch, retry: false }).listSessions()
    ).toEqual([]);
    expect(asked).toBe(1);
  } finally {
    listener.stop(true);
  }
});
