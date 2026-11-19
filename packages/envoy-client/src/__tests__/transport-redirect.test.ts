// packages/envoy-client/src/__tests__/transport-redirect.test.ts
//
// No call of the Envoy transport follows a redirect. fetch would send the credential's headers (the
// broker's DPoP proof, or the listener's bearer) and a 307's or 308's body, the message among them,
// on to the origin a 3xx's `Location` names, and Bun drops only `Authorization` and `Cookie` on the
// way. So every call is sent with `redirect: "manual"`, and a 3xx is an `EnvoyRedirectError` that
// names the status and where it points, read before any of its body, and is not sent again. This
// file holds the redirect row of `scripts/checks/redirect-refusals.ts` for the transport's request.

import { expect, test } from "bun:test";
import { inspect } from "node:util";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import {
  REDIRECT_CODES,
  type RedirectServers,
  redirectCase,
  redirectServers,
  UNNAMED_TARGETS,
} from "@legion/test-fixtures/redirect-servers";
import { createEnvoyClient, EnvoyApiError, EnvoyRedirectError } from "../transport";

/** A broker credential as a call presents one: a made-up access token and DPoP proof. */
const madeUpBroker = {
  scheme: "DPoP" as const,
  headers: () => ({ Authorization: "DPoP made-up-access-token", DPoP: "made-up-dpop-proof" }),
  stale: () => {},
  secret: () => "made-up-access-token",
};

const FIX =
  "set the Envoy URL (ENVOY_URL, or envoy.json's envoy.url) to the listener's own address";

/** A client of the first of `servers` (or of `baseUrl`), with the made-up broker credential. */
function clientOf(servers: RedirectServers, baseUrl = servers.origin) {
  return createEnvoyClient({ baseUrl, fetch, credential: madeUpBroker });
}

/** What `call` was refused with, or undefined when it answered. */
async function refusal(call: Promise<unknown>): Promise<unknown> {
  return await call.then(
    () => undefined,
    (error: unknown) => error
  );
}

const send = (servers: RedirectServers, baseUrl?: string) =>
  refusal(
    clientOf(servers, baseUrl).send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_receiver",
      message: "a made-up message",
    })
  );

// One case for each code, so a client that follows again fails at each. A send (a POST with the
// message) and a read of the sessions (a GET) are each refused, each asked of the listener's
// address once, and the other origin receives no request.
for (const code of REDIRECT_CODES) {
  test(redirectCase(code), async () => {
    let asked = 0;
    const servers = redirectServers({
      redirects: () => {
        asked += 1;
        return true;
      },
      atElsewhere: () => Response.json([]),
    });
    servers.code = code;
    try {
      const sent = await send(servers);
      const read = await refusal(clientOf(servers).listSessions());
      expect(servers.received, `the other origin after a ${code}`).toEqual([]);
      expect(asked, `the requests the listener's address got for a ${code}`).toBe(2);
      expect(sent).toBeInstanceOf(EnvoyRedirectError);
      expect((sent as Error).message).toBe(
        `POST ${servers.origin}/v1/messages/send answered ${code}, redirecting to ${servers.elsewhere}, and the Envoy client follows no redirect: ${FIX}`
      );
      expect(read).toBeInstanceOf(EnvoyRedirectError);
      expect((read as EnvoyRedirectError).details).toMatchObject({
        method: "GET",
        status: code,
        target: { kind: "another origin", origin: servers.elsewhere },
      });
    } finally {
      servers.stop();
    }
  });
}

// A client that read a redirect's body before its status would wait on a body that never ends,
// and refuse nothing; the test's limit is that wait's bound. The body it does not read is
// cancelled, so it holds no connection: the client is handed a fetch whose answers carry a body
// that never ends and counts its cancel, and the count is read as soon as the call is refused,
// before the call's own deadline could cancel the body in its place.
test("a redirect whose body never ends is refused naming its status, before any of its body is read, and its body is cancelled", async () => {
  const servers = redirectServers();
  let cancelled = 0;
  const counting = async (input: string | URL | Request, init?: RequestInit) => {
    const answer = await fetch(input, init);
    await answer.body?.cancel();
    const endless = new ReadableStream<Uint8Array>({
      pull: (controller) => controller.enqueue(new TextEncoder().encode("never ends")),
      cancel: () => {
        cancelled += 1;
      },
    });
    return new Response(endless, { status: answer.status, headers: answer.headers });
  };
  try {
    const sent = await refusal(
      createEnvoyClient({
        baseUrl: servers.origin,
        fetch: counting,
        credential: madeUpBroker,
      }).send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_receiver",
        message: "a made-up message",
      })
    );
    expect({ message: (sent as Error).message, cancelled }).toEqual({
      message: `POST ${servers.origin}/v1/messages/send answered 307, redirecting to ${servers.elsewhere}, and the Envoy client follows no redirect: ${FIX}`,
      cancelled: 1,
    });
  } finally {
    servers.stop();
  }
}, 10_000);

// A redirect within the listener's own address names no address the client lacks: the client puts
// the Envoy URL before each of its routes, so that URL's path is what to correct. One that names
// nowhere is not named as the listener's own address, and a host longer than any host name is
// never printed.
test("a redirect to another path of the same address, to no target, or to an overlong host is refused saying so", async () => {
  const servers = redirectServers();
  try {
    const said: string[] = [];
    for (const { location } of UNNAMED_TARGETS) {
      servers.location = location;
      said.push(((await send(servers)) as Error).message);
    }
    expect(said).toEqual(
      UNNAMED_TARGETS.map(
        ({ where, sameAddress }) =>
          `POST ${servers.origin}/v1/messages/send answered 307, ${where}, and the Envoy client follows no redirect: ${sameAddress ? `correct the path of the Envoy URL (ENVOY_URL, or envoy.json's envoy.url), which the client puts before its own; a missing or an extra "/" is the usual cause` : FIX}`
      )
    );
  } finally {
    servers.stop();
  }
});

// The Envoy URL may carry a user and a password, which the client keeps for its requests; neither
// a refused redirect nor a listener's refusal holds either, in its message or in any member. The
// password is made for this run.
test("a refused redirect and a listener's refusal hold no user and no password of the Envoy URL", async () => {
  const password = crypto.randomUUID();
  const servers = redirectServers();
  const refusing = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => new Response("no", { status: 404 }),
  });
  try {
    const userinfo = `made-up-user:${password}@`;
    const redirected = await send(servers, servers.origin.replace("//", `//${userinfo}`));
    const refused = await send(servers, `http://${userinfo}127.0.0.1:${refusing.port}`);
    expect(redirected).toBeInstanceOf(EnvoyRedirectError);
    expect(refused).toBeInstanceOf(EnvoyApiError);
    expect((redirected as Error).message).toBe(
      `POST the configured Envoy URL ${URL_NOT_PRINTED} answered 307, redirecting to ${servers.elsewhere}, and the Envoy client follows no redirect: ${FIX}`
    );
    expect((refused as Error).message).toBe(
      `POST the configured Envoy URL ${URL_NOT_PRINTED} failed with 404: a body that is not the listener's error JSON ("no")`
    );
    for (const error of [redirected, refused]) {
      const shown = inspect(error, { depth: Number.POSITIVE_INFINITY, showHidden: true });
      expect(shown.includes(password) || shown.includes("made-up-user")).toBe(false);
    }
  } finally {
    servers.stop();
    refusing.stop(true);
  }
});
