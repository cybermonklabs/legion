// packages/envoy-client/src/__tests__/dispatch-review-proxy.test.ts
//
// A review sent through Cipher's proxy: where the credential is a person's sign-in on this
// machine, it has an approve route (`RequestCredential.approveRoute`), and `reviewArtifact` sends
// the review there - to the Dispatch URL's own host, through the proxy the route names, with the
// placeholder as the proxy's password and as `Authorization: DPoP <placeholder>`, trusting the
// route's authority, and with no proof of any key of this process. A credential with no approve
// route sends the review as before, and a Dispatch URL the route does not reach sends nothing.

import { expect, test } from "bun:test";
import { createServer } from "node:net";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { keyedLayer } from "../../test/keyed-layer";
import { routeAnswer } from "../../test/route-answers";
import type { ApproveRoute, RequestCredential } from "../bearer";
import {
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  DispatchServiceError,
  DispatchUnreachableError,
  type DispatchWriteRetry,
  DispatchWriteUnsettledError,
} from "../dispatch-http";

const PLACEHOLDER = `agent-secrets-placeholder-approve-${"B".repeat(32)}`;
const AUTHORITY = "-----BEGIN CERTIFICATE-----\nproxy\n-----END CERTIFICATE-----\n";
const ROUTE: ApproveRoute = {
  placeholder: PLACEHOLDER,
  proxyUrl: "http://proxy.example:3128",
  caCertificate: AUTHORITY,
  hosts: ["dispatch.example"],
};
/** The review Dispatch answers, as the versioned review route declares it (`routeAnswer`): the
 *  members the client's write row reads (`WRITE_ROWS.reviewArtifact`) are among them. */
const REVIEW = routeAnswer("POST /api/v1/artifacts/{id}/versions/{number}/reviews", {
  id: "review-1",
  artifact_id: "artifact-1",
  version: 3,
  state: "approved",
  actor: { kind: "user", id: "ada@example.com" },
  created_at: "2026-11-23T12:00:00Z",
});

/** A credential whose own headers are a bearer's, with `route` as its approve route. */
function credential(
  route: ApproveRoute | undefined
): RequestCredential & { readonly asked: number[] } {
  const asked: number[] = [];
  const token = fixtureSecret("box");
  return {
    asked,
    scheme: "DPoP",
    headers: () => ({ Authorization: `DPoP ${token}`, DPoP: "a-proof-of-the-box-key" }),
    approveRoute: async () => {
      asked.push(1);
      return route;
    },
    secret: () => token,
  };
}

interface Sent {
  readonly url: string;
  readonly init: RequestInit & { readonly proxy?: string; readonly tls?: { readonly ca?: string } };
}

/** A fetch that answers `status` with `body` and keeps what it was sent. Its answer is Dispatch's,
 *  a 2xx or a refusal its keyed layer judged, so it names the review's key back (`keyedLayer`). */
function recordingFetch(status: number, body: unknown): typeof fetch & { readonly sent: Sent[] } {
  const sent: Sent[] = [];
  const impl = keyedLayer(
    async (input: Parameters<typeof fetch>[0], init?: RequestInit): Promise<Response> => {
      sent.push({ url: String(input), init: (init ?? {}) as Sent["init"] });
      return Response.json(body, { status });
    },
    { judged: () => true }
  );
  return Object.assign(impl, { sent, preconnect: () => {} }) as typeof fetch & {
    readonly sent: Sent[];
  };
}

test("a person's review goes through Cipher's proxy with the placeholder and no proof", async () => {
  const fetchImpl = recordingFetch(201, REVIEW);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl);
  const review = await client.reviewArtifact("artifact-1", 3, { state: "approved" });
  expect(review as unknown).toEqual(REVIEW);
  expect(fetchImpl.sent).toHaveLength(1);
  const [sent] = fetchImpl.sent;
  expect(sent?.url).toBe("https://dispatch.example/api/v1/artifacts/artifact-1/versions/3/reviews");
  expect(sent?.init.method).toBe("POST");
  const proxy = new URL(sent?.init.proxy ?? "");
  expect(`${proxy.protocol}//${proxy.host}`).toBe("http://proxy.example:3128");
  expect(decodeURIComponent(proxy.password)).toBe(PLACEHOLDER);
  expect(sent?.init.tls?.ca).toBe(AUTHORITY);
  const headers = new Headers(sent?.init.headers);
  expect(headers.get("Authorization")).toBe(`DPoP ${PLACEHOLDER}`);
  expect(headers.get("DPoP")).toBeNull();
  expect(JSON.parse(String(sent?.init.body))).toEqual({ state: "approved" });
});

test("a credential with no approve route sends the review as before, with its own headers", async () => {
  const fetchImpl = recordingFetch(201, REVIEW);
  const box = credential(undefined);
  const client = new DispatchClient("https://dispatch.example", box, fetchImpl);
  await client.reviewArtifact("artifact-1", 3, { state: "approved" });
  expect(box.asked).toHaveLength(1);
  expect(fetchImpl.sent).toHaveLength(1);
  const [sent] = fetchImpl.sent;
  expect(sent?.init.proxy).toBeUndefined();
  expect(new Headers(sent?.init.headers).get("DPoP")).toBe("a-proof-of-the-box-key");
});

test("a Dispatch URL the approve route does not reach sends nothing", async () => {
  for (const base of [
    "https://other.example",
    "http://dispatch.example",
    "https://dispatch.example:8443",
  ]) {
    const fetchImpl = recordingFetch(201, REVIEW);
    const client = new DispatchClient(base, credential(ROUTE), fetchImpl);
    const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refused?.message ?? "").toContain("dispatch.example");
    expect(refused?.message ?? "").not.toContain(PLACEHOLDER);
    expect(fetchImpl.sent).toHaveLength(0);
  }
});

test("Dispatch's own refusal of a review through the proxy (no proxy headers) is Dispatch's answer, without the placeholder", async () => {
  const fetchImpl = recordingFetch(403, { code: "NOT_ALLOWED", error: "not allowed by rule" });
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl);
  const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(refused).toBeInstanceOf(DispatchServiceError);
  const refusal = refused as DispatchServiceError;
  expect([refusal.code, refusal.status, refusal.fromDispatch]).toEqual(["NOT_ALLOWED", 403, true]);
  expect(refusal.message).not.toContain(PLACEHOLDER);
});

/** The production pacing with no time passing: every pause returns at once and the clock stands
 *  still, so a write asks every attempt its schedule allows within the test. */
const NO_WAIT: DispatchWriteRetry = {
  ...DISPATCH_WRITE_RETRY,
  pause: async () => {},
  now: () => 0,
};

/** A fetch that rejects the first `failures` requests as a fetch that reached no server does (a
 *  TypeError), then answers 201 with the review behind Dispatch's keyed layer (`keyedLayer`), and
 *  keeps what each request was sent. */
function unreachableThen(failures: number): typeof fetch & { readonly sent: Sent[] } {
  const sent: Sent[] = [];
  const impl = keyedLayer(
    async (input: Parameters<typeof fetch>[0], init?: RequestInit): Promise<Response> => {
      sent.push({ url: String(input), init: (init ?? {}) as Sent["init"] });
      if (sent.length <= failures)
        throw new TypeError("Unable to connect. Is the computer able to access the url?");
      return Response.json(REVIEW, { status: 201 });
    }
  );
  return Object.assign(impl, { sent, preconnect: () => {} }) as typeof fetch & {
    readonly sent: Sent[];
  };
}

test("a review through the proxy is a keyed write: one Idempotency-Key, asked again under it", async () => {
  const fetchImpl = unreachableThen(1);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const review = await client.reviewArtifact("artifact-1", 3, { state: "approved" });
  expect(review as unknown).toEqual(REVIEW);
  expect(fetchImpl.sent).toHaveLength(2);
  const keys = fetchImpl.sent.map((sent) => new Headers(sent.init.headers).get("Idempotency-Key"));
  expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/);
  expect(keys[1]).toBe(keys[0]);
  for (const sent of fetchImpl.sent) {
    expect(new URL(sent.init.proxy ?? "").host).toBe("proxy.example:3128");
    expect(sent.init.tls?.ca).toBe(AUTHORITY);
    expect(new Headers(sent.init.headers).get("Authorization")).toBe(`DPoP ${PLACEHOLDER}`);
    expect(new Headers(sent.init.headers).get("DPoP")).toBeNull();
  }
});

test("a review the proxy never answers names Cipher's proxy, never this process's configuration", async () => {
  const fetchImpl = unreachableThen(Number.POSITIVE_INFINITY);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(fetchImpl.sent.length).toBe(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
  const message = refused?.message ?? "";
  expect(message).toContain("Cipher's proxy at http://proxy.example:3128");
  expect(message).not.toContain("restart this agent process");
  expect(message).not.toContain(PLACEHOLDER);
});

/** The proxy's own answer, as record.go writes it: `text/plain` "airlock: …", its code in
 *  `Cipher-Proxy-Code` and whether the request may have left it in `Cipher-Proxy-Forwarded`. */
function proxyAnswer(status: number, code: string, forwarded: "false" | "unknown"): Response {
  return new Response(`airlock: ${code.toLowerCase().replaceAll("_", " ")}\n`, {
    status,
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      "Cipher-Proxy-Code": code,
      "Cipher-Proxy-Forwarded": forwarded,
    },
  });
}

/** A fetch that answers each request with the next of `answers` (the last one from then on), and
 *  keeps what it was sent. A 2xx is Dispatch's, behind its keyed layer, so it names the review's
 *  key back (`keyedLayer`); the proxy's own answers are no answer of that layer and name none. */
function scriptedFetch(answers: readonly (() => Response)[]): typeof fetch & {
  readonly sent: Sent[];
} {
  const sent: Sent[] = [];
  const impl = keyedLayer(
    async (input: Parameters<typeof fetch>[0], init?: RequestInit): Promise<Response> => {
      sent.push({ url: String(input), init: (init ?? {}) as Sent["init"] });
      const answer = answers[Math.min(sent.length, answers.length) - 1];
      if (answer === undefined) throw new Error("scriptedFetch was given no answer");
      return answer();
    }
  );
  return Object.assign(impl, { sent, preconnect: () => {} }) as typeof fetch & {
    readonly sent: Sent[];
  };
}

test("the proxy's own refusal, before anything left it, names the proxy and its code and says a new review trades a new grant", async () => {
  const fetchImpl = scriptedFetch([() => proxyAnswer(407, "GRANT_NOT_LIVE", "false")]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(fetchImpl.sent).toHaveLength(1);
  expect(refused).toBeInstanceOf(DispatchServiceError);
  const refusal = refused as DispatchServiceError;
  expect([refusal.code, refusal.status, refusal.fromDispatch]).toEqual([
    "GRANT_NOT_LIVE",
    407,
    false,
  ]);
  expect(refusal.message).toContain("Cipher's proxy at http://proxy.example:3128");
  expect(refusal.message).toContain("nothing reached Dispatch");
  expect(refusal.message).toContain("a new `dispatch review` trades a new grant");
  expect(refusal.message).not.toContain("Dispatch URL");
  expect(refusal.message).not.toContain(PLACEHOLDER);
});

// The placeholder is noted where each attempt presents it (`#write`), as every presentation is, so
// a proxy that echoes it back has it redacted from the refusal's text. The echo carries no scheme
// before it, so only that note redacts it, not the excerpt's reading of an `Authorization` value.
test("the proxy's refusal that echoes the placeholder is told without it", async () => {
  const fetchImpl = scriptedFetch([
    () =>
      new Response(`airlock: grant not live for ${PLACEHOLDER}\n`, {
        status: 407,
        headers: {
          "Content-Type": "text/plain; charset=utf-8",
          "Cipher-Proxy-Code": "GRANT_NOT_LIVE",
          "Cipher-Proxy-Forwarded": "false",
        },
      }),
  ]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(refused).toBeInstanceOf(DispatchServiceError);
  const refusal = refused as DispatchServiceError;
  expect(refusal.code).toBe("GRANT_NOT_LIVE");
  expect(refusal.message).toContain("grant not live for [redacted]");
  expect(refusal.message).not.toContain(PLACEHOLDER);
});

test("the proxy's own failure, before anything left it, is asked again under the write's key", async () => {
  const fetchImpl = scriptedFetch([
    () => proxyAnswer(502, "UPSTREAM_UNREACHABLE", "false"),
    () => Response.json(REVIEW, { status: 201 }),
  ]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const review = await client.reviewArtifact("artifact-1", 3, { state: "approved" });
  expect(review as unknown).toEqual(REVIEW);
  const keys = fetchImpl.sent.map((sent) => new Headers(sent.init.headers).get("Idempotency-Key"));
  expect(keys).toHaveLength(2);
  expect(keys[1]).toBe(keys[0]);
});

test("the proxy's answer after the review may have reached Dispatch leaves the review unsettled, with its key", async () => {
  const fetchImpl = scriptedFetch([() => proxyAnswer(407, "GRANT_NOT_LIVE", "unknown")]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const ended = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(ended).toBeInstanceOf(DispatchWriteUnsettledError);
  const unsettled = ended as DispatchWriteUnsettledError;
  const keys = new Set(
    fetchImpl.sent.map((sent) => new Headers(sent.init.headers).get("Idempotency-Key"))
  );
  expect([...keys]).toEqual([unsettled.key]);
  expect(fetchImpl.sent.length).toBeGreaterThan(1);
  expect(unsettled.message).toContain("Cipher's proxy");
  expect(unsettled.message).not.toContain("Dispatch URL");
  expect(unsettled.message).not.toContain(PLACEHOLDER);
});

test("after an answer that may have reached Dispatch, the proxy's refusal on the retry leaves the review unsettled and neither says nothing reached Dispatch nor advises a new review", async () => {
  const fetchImpl = scriptedFetch([
    () => proxyAnswer(407, "GRANT_NOT_LIVE", "unknown"),
    () => proxyAnswer(407, "GRANT_NOT_LIVE", "false"),
  ]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const ended = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(fetchImpl.sent).toHaveLength(2);
  expect(ended).toBeInstanceOf(DispatchWriteUnsettledError);
  const message = (ended as DispatchWriteUnsettledError).message;
  expect(message).toContain("GRANT_NOT_LIVE");
  expect(message).toContain("Cipher's proxy");
  expect(message).not.toContain("nothing reached Dispatch");
  expect(message).not.toContain("a new `dispatch review` trades a new grant");
  expect(message).toContain("read the write back");
});

/** A fetch that takes each step in turn (the last one from then on): a step that answers is the
 *  answer (a 2xx behind Dispatch's keyed layer, `keyedLayer`), one that names no answer rejects as
 *  a fetch that reached no server does. */
function stepFetch(steps: readonly ((() => Response) | "unreachable")[]): typeof fetch & {
  readonly sent: Sent[];
} {
  const sent: Sent[] = [];
  const impl = keyedLayer(
    async (input: Parameters<typeof fetch>[0], init?: RequestInit): Promise<Response> => {
      sent.push({ url: String(input), init: (init ?? {}) as Sent["init"] });
      const step = steps[Math.min(sent.length, steps.length) - 1];
      if (step === undefined) throw new Error("stepFetch was given no step");
      if (step === "unreachable") {
        throw new TypeError("Unable to connect. Is the computer able to access the url?");
      }
      return step();
    }
  );
  return Object.assign(impl, { sent, preconnect: () => {} }) as typeof fetch & {
    readonly sent: Sent[];
  };
}

test("after an answer that may have reached Dispatch, an unreachable proxy on the retries leaves the review unsettled with no advice to send it again", async () => {
  const fetchImpl = stepFetch([() => proxyAnswer(407, "GRANT_NOT_LIVE", "unknown"), "unreachable"]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const ended = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(fetchImpl.sent).toHaveLength(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
  expect(ended).toBeInstanceOf(DispatchWriteUnsettledError);
  const message = (ended as DispatchWriteUnsettledError).message;
  expect(message).toContain("Cipher's proxy at http://proxy.example:3128");
  expect(message).toContain("The proxy is the secrets broker's.");
  expect(message).not.toContain("restart this agent process");
  expect(message).not.toContain("sent again");
  expect(message).not.toContain("a new `dispatch review`");
  expect(message).toContain("read the write back");
});

test("when only the last attempt may have reached Dispatch, the proxy's refusal names that attempt, not an earlier one", async () => {
  const fetchImpl = scriptedFetch([
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "unknown"),
  ]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const ended = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(fetchImpl.sent).toHaveLength(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
  expect(ended).toBeInstanceOf(DispatchWriteUnsettledError);
  const message = (ended as DispatchWriteUnsettledError).message;
  expect(message).toContain("GRANT_UNREADABLE");
  expect(message).not.toContain("earlier attempt");
  expect(message).toContain("this attempt may have reached Dispatch");
  expect(message).not.toContain("a new `dispatch review`");
});

test("when the proxy refuses every attempt and only the last refusal may have left it, the unsettled write names no earlier attempt", async () => {
  const fetchImpl = scriptedFetch([
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(503, "GRANT_UNREADABLE", "false"),
    () => proxyAnswer(407, "GRANT_NOT_LIVE", "unknown"),
  ]);
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const ended = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(fetchImpl.sent).toHaveLength(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
  expect(ended).toBeInstanceOf(DispatchWriteUnsettledError);
  const message = (ended as DispatchWriteUnsettledError).message;
  expect(message).toContain("GRANT_NOT_LIVE");
  expect(message).toContain("this attempt may have reached Dispatch");
  expect(message).not.toContain("earlier attempt");
});

test("a proxy that refuses the connection on every attempt says the review can be sent again once the broker answers", async () => {
  const sent: Sent[] = [];
  const impl = async (
    input: Parameters<typeof fetch>[0],
    init?: RequestInit
  ): Promise<Response> => {
    sent.push({ url: String(input), init: (init ?? {}) as Sent["init"] });
    // Bun rejects a refused connection with a TypeError carrying the errno code (errors.ts).
    throw Object.assign(
      new TypeError("Unable to connect. Is the computer able to access the url?"),
      {
        code: "ECONNREFUSED",
      }
    );
  };
  const fetchImpl = Object.assign(impl, { preconnect: () => {} }) as typeof fetch;
  const client = new DispatchClient("https://dispatch.example", credential(ROUTE), fetchImpl, {
    writeRetry: NO_WAIT,
  });
  const ended = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
    () => null,
    (error: unknown) => error
  );
  expect(sent.length).toBeGreaterThan(0);
  expect(ended).toBeInstanceOf(DispatchUnreachableError);
  const message = (ended as DispatchUnreachableError).message;
  expect(message).toContain("Cipher's proxy at http://proxy.example:3128");
  expect(message).toContain("The review can be sent again once the broker answers.");
  expect(message).not.toContain("restart this agent process");
});

/**
 * A proxy that refuses every CONNECT itself, as Cipher's proxy does when a placeholder's grant is
 * not live (`proxy.go`, `refuseCredential`, 407) or its store cannot be read (`fail`, 503): its own
 * answer with `Cipher-Proxy-Code` and `Cipher-Proxy-Forwarded: false`, before any tunnel opens.
 * Bun's fetch rejects such an answer as `ERR_PROXY_TUNNEL`, keeping its status and headers.
 */
async function refusingProxy(
  status: number,
  reason: string,
  code: string
): Promise<{ readonly url: string; readonly connects: () => number; close(): void }> {
  let connects = 0;
  const body = `airlock: ${code.toLowerCase().replaceAll("_", " ")}\n`;
  const server = createServer((socket) => {
    socket.once("data", () => {
      connects += 1;
      socket.end(
        `HTTP/1.1 ${status} ${reason}\r\n${status === 407 ? 'Proxy-Authenticate: Basic realm="airlock"\r\n' : ""}Content-Type: text/plain; charset=utf-8\r\nCipher-Proxy-Code: ${code}\r\nCipher-Proxy-Forwarded: false\r\nContent-Length: ${body.length}\r\nConnection: close\r\n\r\n${body}`
      );
    });
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  const { port } = server.address() as { port: number };
  return { url: `http://127.0.0.1:${port}`, connects: () => connects, close: () => server.close() };
}

test("a CONNECT the proxy refuses (407 GRANT_NOT_LIVE) is the proxy's refusal: one CONNECT, and nothing reached Dispatch", async () => {
  const proxy = await refusingProxy(407, "Proxy Authentication Required", "GRANT_NOT_LIVE");
  try {
    const client = new DispatchClient(
      "https://dispatch.example",
      credential({ ...ROUTE, proxyUrl: proxy.url }),
      fetch,
      { writeRetry: NO_WAIT }
    );
    const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
      () => null,
      (error: unknown) => error
    );
    expect(proxy.connects()).toBe(1);
    expect(refused).toBeInstanceOf(DispatchServiceError);
    const refusal = refused as DispatchServiceError;
    expect([refusal.code, refusal.status, refusal.fromDispatch]).toEqual([
      "GRANT_NOT_LIVE",
      407,
      false,
    ]);
    expect(refusal.message).toContain(`Cipher's proxy at ${proxy.url}`);
    expect(refusal.message).toContain("nothing reached Dispatch");
    expect(refusal.message).toContain("a new `dispatch review` trades a new grant");
    expect(refusal.message).not.toContain(PLACEHOLDER);
  } finally {
    proxy.close();
  }
});

test("a CONNECT the proxy fails (503 GRANT_UNREADABLE) is asked again, then ends as the proxy's failure, never unsettled", async () => {
  const proxy = await refusingProxy(503, "Service Unavailable", "GRANT_UNREADABLE");
  try {
    const client = new DispatchClient(
      "https://dispatch.example",
      credential({ ...ROUTE, proxyUrl: proxy.url }),
      fetch,
      { writeRetry: NO_WAIT }
    );
    const refused = await client.reviewArtifact("artifact-1", 3, { state: "approved" }).then(
      () => null,
      (error: unknown) => error
    );
    expect(proxy.connects()).toBe(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
    expect(refused).not.toBeInstanceOf(DispatchWriteUnsettledError);
    expect(refused).toBeInstanceOf(DispatchServiceError);
    const refusal = refused as DispatchServiceError;
    expect([refusal.code, refusal.status]).toEqual(["GRANT_UNREADABLE", 503]);
    expect(refusal.message).toContain("nothing reached Dispatch");
    expect(refusal.message).toContain("once the broker answers");
  } finally {
    proxy.close();
  }
});
