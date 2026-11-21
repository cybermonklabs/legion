// packages/envoy-client/src/__tests__/transport-credential-deadline.test.ts
//
// An Envoy call hands its credential the call's own deadline, so a credential that is still
// waiting (a pod whose session's registration has not landed) fails the call at that deadline
// instead of holding it. broker-credential-registration.test.ts covers what the broker credential
// does with the deadline; this covers that the transport gives it one, and that a real broker
// credential fails fast where its broker cannot answer, so a listener that asks for no credential
// keeps its calls (`transport.ts`, `attemptOnce`).

import { afterEach, expect, mock, test } from "bun:test";
import { LISTENER_CALL_TIMEOUT_MS } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { holdDeadlines } from "@legion/test-fixtures/held-deadlines";
import { settled, until } from "@legion/test-fixtures/wait";
import { brokerCredential } from "../broker-credential";
import { createEnvoyClient, EnvoySendUnconfirmedError } from "../transport";
import { brokerStandIns } from "./broker-stand-ins";

const { podEnvironment, servers } = brokerStandIns("transport-credential-deadline");

// A spy a row left, a failed one's included, wraps nothing of the next row.
afterEach(() => {
  mock.restore();
});

/** The session the credential's refusal names. */
const POD_SESSION = "implementer-g1";

/** The credential's share of a listener attempt (`credentialDeadline`): the call's 5 s
 *  (`LISTENER_CALL_TIMEOUT_MS`) less the request's reserve, 1 s or half the deadline. */
function shareOf(deadlineMs: number): number {
  return Math.ceil(deadlineMs - Math.min(1_000, deadlineMs / 2));
}

test("an Envoy call gives its credential the attempt less the request's reserve, and goes without the credential that did not come", async () => {
  // The credential waits the attempt less the request's reserve (`credentialDeadline`); one still
  // waiting then stops, and the request goes without it in the reserve. A listener that wants a
  // credential answers 401, and the call's error names both halves: that answer, and what the
  // credential said when its wait ended.
  const fetched: (string | null)[] = [];
  const given: (AbortSignal | undefined)[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    retry: false,
    timeoutMs: 200,
    fetch: async (_url, init) => {
      const authorization = new Headers(init?.headers).get("Authorization");
      fetched.push(authorization);
      return authorization === null
        ? Response.json({ error: "this listener wants a credential" }, { status: 401 })
        : Response.json({});
    },
    credential: {
      scheme: "DPoP",
      headers: (_method, _url, _session, signal) => {
        given.push(signal);
        const { promise, reject } = Promise.withResolvers<Readonly<Record<string, string>>>();
        signal?.addEventListener("abort", () => reject(new Error(`pending: ${POD_SESSION}`)), {
          once: true,
        });
        return promise;
      },
      stale: () => {},
      secret: () => "",
    },
  });
  const call = client.getRole("implementer");
  await until("the credential handed its signal", () => given.length === 1);
  expect(given[0], "the call handed its credential no deadline").toBeInstanceOf(AbortSignal);
  // The transport's own deadline timer ends the signal; the transport takes no clock of a test's.
  const waited = given[0] as AbortSignal;
  expect(waited.aborted).toBe(false);
  await expect(call).rejects.toThrow(
    `answered 401, and this machine's Envoy credential could not be minted: pending: ${POD_SESSION}`
  );
  expect(waited.aborted).toBe(true);
  // The call asked the listener once, without the credential that did not come.
  expect(fetched).toEqual([null]);
});

// The listener refuses a request without the credential it wants at its auth gate, before any
// route runs (`apiauth.go`), so a send it answered so stored nothing: the call says the send was
// refused, in both halves' words, and never that it may have arrived with a resend to make. A send
// whose earlier attempt went unanswered stays unconfirmed, since that attempt may have stored it.
test("a send the auth gate refused for want of the credential that could not be minted is refused, not unconfirmed", async () => {
  const pending = {
    scheme: "DPoP" as const,
    headers: (_method: string, _url: string, _session?: string, signal?: AbortSignal) => {
      const { promise, reject } = Promise.withResolvers<Readonly<Record<string, string>>>();
      signal?.addEventListener("abort", () => reject(new Error(`pending: ${POD_SESSION}`)), {
        once: true,
      });
      return promise;
    },
    stale: () => {},
    secret: () => "",
  };
  const answers: Response[] = [];
  const sent: string[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    timeoutMs: 200,
    fetch: async (_url, init) => {
      sent.push(String(JSON.parse(String(init?.body)).idempotency_key));
      return (
        answers.shift() ??
        Response.json({ error: "this listener wants a credential" }, { status: 401 })
      );
    },
    credential: pending,
  });
  const send = () =>
    client.send({ sourceSessionID: "", targetSessionID: "ses_receiver", message: "hello" });

  const refused = await send().catch((error: unknown) => error);
  expect(refused).not.toBeInstanceOf(EnvoySendUnconfirmedError);
  expect((refused as Error).message).toBe(
    `POST http://listener.test/v1/messages/send answered 401, and this machine's Envoy credential could not be minted: pending: ${POD_SESSION}`
  );
  // Both attempts went, under one key: the broker may have come back for the second.
  expect(sent).toHaveLength(2);
  expect(sent[0]).toBe(sent[1] as string);

  // A first attempt the listener may have acted on (a 503) keeps the send unconfirmed.
  sent.length = 0;
  answers.push(Response.json({ error: "restarting" }, { status: 503 }));
  expect(await send().catch((error: unknown) => error)).toBeInstanceOf(EnvoySendUnconfirmedError);
});

test.each([
  ["refuses connections", (): string => "http://127.0.0.1:1"],
  [
    "answers 503",
    (): string => {
      const away = Bun.serve({
        port: 0,
        hostname: "127.0.0.1",
        fetch: () => new Response("the broker is away", { status: 503 }),
      });
      servers.push(away);
      return `http://127.0.0.1:${away.port}`;
    },
  ],
])("a listener that asks for no credential keeps its calls while the broker %s", async (_, broker) => {
  // A pod with no Envoy bearer presents its broker credential on every listener call. The
  // credential fails fast where the broker cannot answer, so the call goes without it and stands
  // on the listener's own answer, at once, rather than waiting out its 5 s attempt: the row holds
  // the credential's share of the attempt (`holdDeadlines`) and never ends it, so a call that
  // waited on its share would never be answered.
  holdDeadlines(shareOf(LISTENER_CALL_TIMEOUT_MS));
  const fetched: { readonly url: string; readonly authorization: string | null }[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    retry: false,
    fetch: async (url, init) => {
      fetched.push({
        url: String(url),
        authorization: new Headers(init?.headers).get("Authorization"),
      });
      return Response.json({
        role: "implementer",
        holder: "ses_holder",
        last_seen: 1,
        state: "live",
      });
    },
    credential: brokerCredential(podEnvironment(broker(), "http://dispatch.test"), POD_SESSION),
  });
  await settled(
    "the call answered with the credential's share held",
    client.getRole("implementer")
  );
  expect(fetched).toHaveLength(1);
  expect(fetched[0]?.authorization).toBeNull();
});

/** A broker at a port of its own that answers each trade as `answer` does. */
function brokerAt(answer: () => Promise<Response>): string {
  const broker = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: answer });
  servers.push(broker);
  return `http://127.0.0.1:${broker.port}`;
}

/** A token the broker grants, good for ten minutes. */
function granted(): Response {
  return Response.json({
    access_token: fixtureSecret("granted"),
    issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
    token_type: "DPoP",
    expires_in: 600,
  });
}

test.each([
  ["is silent", (): string => brokerAt(() => new Promise<Response>(() => {}))],
  [
    "holds no registration for the pod's session yet",
    (): string =>
      brokerAt(async () =>
        Response.json(
          {
            error: "invalid_grant",
            error_description: `pod_session_not_registered: No registration says this pod may write under the session name '${POD_SESSION}'. A pod writes under a session name its deployment registered for it.`,
          },
          { status: 400 }
        )
      ),
  ],
])("a listener that asks for no credential keeps its calls while the broker %s", async (_, broker) => {
  // The credential waits the attempt less the request's reserve (4 s of the listener's 5), then
  // stops, and the call goes without it in that last second and stands on the listener's own
  // answer. The row holds that share (`holdDeadlines`): until it ends, the call sends nothing.
  const share = holdDeadlines(shareOf(LISTENER_CALL_TIMEOUT_MS));
  const fetched: (string | null)[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    retry: false,
    fetch: async (_url, init) => {
      if (init?.signal?.aborted) throw init.signal.reason;
      fetched.push(new Headers(init?.headers).get("Authorization"));
      return Response.json({
        role: "implementer",
        holder: "ses_holder",
        last_seen: 1,
        state: "live",
      });
    },
    credential: brokerCredential(podEnvironment(broker(), "http://dispatch.test"), POD_SESSION),
  });
  const call = client.getRole("implementer");
  await until("the credential's share of the attempt armed", () => share.held.length > 0);
  expect(fetched, "the call went before its credential's share ended").toEqual([]);
  share.end();
  await settled("the call once its credential's share ended", call);
  expect(fetched).toEqual([null]);
});

test("a fresh process's first trade that outlasts the first attempt's share: the call is answered with the token, after one 401", async () => {
  // Every hook and command line call is a fresh process, so its first listener call waits on
  // its first trade. Each attempt's credential waits 4 s of the attempt's 5; the first ends
  // without a token, goes without it, and a listener that wants one answers 401, which the
  // call tries again; the next attempt's credential is waiting on the same trade when it lands.
  // The row holds each share (`holdDeadlines`) and the broker's answer (`release`), and ends the
  // first share, then answers the trade inside the second: no real time passes between them.
  const share = holdDeadlines(shareOf(LISTENER_CALL_TIMEOUT_MS));
  const release = Promise.withResolvers<void>();
  let trades = 0;
  const broker = brokerAt(async () => {
    trades += 1;
    await release.promise;
    return granted();
  });
  const fetched: (string | null)[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    fetch: async (_url, init) => {
      if (init?.signal?.aborted) throw init.signal.reason;
      const authorization = new Headers(init?.headers).get("Authorization");
      fetched.push(authorization);
      return authorization === null
        ? Response.json({ error: "this listener wants a credential" }, { status: 401 })
        : Response.json({ role: "implementer", holder: "ses_holder", last_seen: 1, state: "live" });
    },
    credential: brokerCredential(podEnvironment(broker, "http://dispatch.test"), POD_SESSION),
  });
  const call = client.getRole("implementer");
  await until(
    "the first trade under way and its share armed",
    () => trades > 0 && share.held.length > 0
  );
  share.end();
  await until(
    "the second attempt's share armed, after one call without the credential",
    () => fetched.length === 1 && share.held.length > 1
  );
  release.resolve();
  await settled("the call answered with the token", call);
  expect(fetched.at(-1)).toStartWith("DPoP ");
  expect(fetched.filter((one) => one === null)).toHaveLength(1);
});

test("inside the renewal window, a broker that hangs leaves the call its live token and the time to send it", async () => {
  // The token in hand is inside its renewal window, so the call waits for the renewal the
  // credential starts, and the broker never answers it. At the end of the credential's share of
  // the attempt the credential answers with the live token (`BrokerTokenClient.token`), and the
  // request goes out with it in the time left, rather than on a deadline already spent.
  let trades = 0;
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async () => {
      trades += 1;
      if (trades > 1) return await new Promise<Response>(() => {});
      // Good for 200 s: inside the 5-minute renewal window from the first.
      return Response.json({
        access_token: fixtureSecret("live"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 200,
      });
    },
  });
  servers.push(broker);
  const fetched: (string | null)[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    retry: false,
    timeoutMs: 1_000,
    fetch: async (_url, init) => {
      // As the runtime's fetch does: a request sent on a spent signal is never sent.
      if (init?.signal?.aborted) throw init.signal.reason;
      fetched.push(new Headers(init?.headers).get("Authorization"));
      return Response.json({
        role: "implementer",
        holder: "ses_holder",
        last_seen: 1,
        state: "live",
      });
    },
    credential: brokerCredential(
      podEnvironment(`http://127.0.0.1:${broker.port}`, "http://dispatch.test"),
      POD_SESSION
    ),
  });
  await client.getRole("implementer");
  const live = fetched[0];
  expect(live).toStartWith("DPoP ");
  await client.getRole("implementer");
  expect(trades).toBe(2);
  expect(fetched).toEqual([live, live]);
});

test("an open listener's 403 to a call that went without its credential stands as the listener's answer", async () => {
  // A listener that requires a credential answers a request without one 401 and nothing else
  // (`apiauth.go`); a 403 is the listener's own refusal of what the call asked, such as a send's
  // capability, and its reason is the call's answer, not a credential that could not be minted.
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    retry: false,
    fetch: async (_url, init) => {
      if (init?.signal?.aborted) throw init.signal.reason;
      return Response.json(
        { error: "this session may not send to that one", reason: "capability" },
        { status: 403 }
      );
    },
    credential: brokerCredential(
      podEnvironment(
        brokerAt(() => new Promise<Response>(() => {})),
        "http://dispatch.test"
      ),
      POD_SESSION
    ),
  });
  const refused = await client.getRole("implementer").catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(Error);
  expect((refused as Error).message).toContain("this session may not send to that one");
  expect((refused as Error).message).not.toContain("could not be minted");
});

test("a deregistration given a shutdown's remaining time goes out in it, without a credential still waiting", async () => {
  // pi-envoy's shutdown has 1 s for its DELETE (`SHUTDOWN_DEADLINE_MS`). Given that time, the
  // call makes one attempt whose credential waits on half of it, so the DELETE goes without the
  // credential a silent broker holds back, inside the shutdown's second. The row holds that share
  // of the 750 ms it gives (`holdDeadlines`): a credential given any longer share is never held,
  // and the row's wait for it to be armed fails at its deadline.
  const share = holdDeadlines(shareOf(750));
  const deleted: (string | null)[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    fetch: async (_url, init) => {
      if (init?.signal?.aborted) throw init.signal.reason;
      deleted.push(new Headers(init?.headers).get("Authorization"));
      return Response.json({ kept: false });
    },
    credential: brokerCredential(
      podEnvironment(
        brokerAt(() => new Promise<Response>(() => {})),
        "http://dispatch.test"
      ),
      POD_SESSION
    ),
  });
  const unregistering = client.unregisterSession({ sessionID: POD_SESSION, timeoutMs: 750 });
  await until("the credential's share of the 750 ms armed", () => share.held.length > 0);
  expect(deleted, "the DELETE went before the credential's share ended").toEqual([]);
  share.end();
  await settled("the DELETE once the credential's share ended", unregistering);
  expect(deleted).toEqual([null]);
});
