import { afterEach, describe, expect, test } from "bun:test";
import { bearerCredential } from "../bearer";
import { DISPATCH_WRITE_RETRY, DispatchClient, type DispatchWriteRetry } from "../dispatch-http";

// Every Dispatch that carries `b1c42d1d7d` runs its keyed layer on every POST and PATCH route
// (`keyedWrites`, packages/envoy/internal/dispatch/api/request_keys.go): it stores a keyed write once and
// answers a repeat under the same key with the first write's answer. So a keyed write whose answer
// was lost after Dispatch stored it is sent again under its key, and lands once, whatever `/healthz`
// answers: the client asks it nothing first.

/** How the first attempt's answer is lost after Dispatch stored the write. */
type Cut = "the client's silence cut" | "a gateway's 502 page";

/** What `/healthz` does: three ways of being down, and the answer that the keyed layer runs. */
type Health = "refused" | "timed out" | "answering no JSON" | "answering that the keyed layer runs";

const ISSUE_COMMENTS = "/api/v1/issues/DSP-1/comments";

/** A port nothing on this host listens on, so a connection to it is refused. */
const REFUSED = "http://127.0.0.1:1";

/** Dispatch's `/healthz` answer on a build that runs the keyed layer. */
const KEYED_LAYER_HEALTH = { ok: true, db: true, nats: true, idempotency_keys: true };

const comment = { body: "Shipped.", actor: { kind: "session" as const, id: "session-1" } };

const opened: { stop(): void }[] = [];

afterEach(() => {
  for (const made of opened.splice(0)) made.stop();
});

type Comment = { readonly id: string; readonly body: string };

/**
 * The keyed comment store the two Bun stand-ins below keep. Like Dispatch's keyed layer, a request
 * under a key it holds replays the stored comment and any other key stores a new comment. Each
 * request it takes is recorded in `posts`, stored or not.
 */
function keyedStore() {
  const comments: Comment[] = [];
  const posts: { key: string | null; stored: boolean }[] = [];
  const answers = new Map<string, Comment>();
  return {
    comments,
    posts,
    /** The replay of the comment `request`'s key holds, else the comment `request` stores. */
    async take(
      request: Request
    ): Promise<
      { readonly replay: Response } | { readonly made: Comment; readonly key: string | null }
    > {
      const key = request.headers.get("Idempotency-Key");
      const held = key === null ? undefined : answers.get(key);
      if (key !== null && held !== undefined) {
        posts.push({ key, stored: false });
        return {
          replay: Response.json(held, {
            status: 201,
            headers: { "Idempotency-Key": key, "Idempotency-Replayed": "?1" },
          }),
        };
      }
      const input = (await request.json()) as { body: string };
      const made = { id: `comment-${comments.length + 1}`, body: input.body };
      comments.push(made);
      posts.push({ key, stored: true });
      if (key !== null) answers.set(key, made);
      return { made, key };
    },
  };
}

/**
 * A test Dispatch that runs the keyed layer on its comment route: the first write under a key is
 * stored, and every later request under that key is answered the stored answer, with the key and
 * `Idempotency-Replayed: ?1`, and stores nothing. The first attempt's answer is lost after the
 * store, as `cut` says: held until the client cuts the attempt off, or replaced by a gateway's 502
 * page. Its `/healthz` behaves as `health` says; `refused` is a port nothing listens on, which
 * `fetchImpl` sends that request to.
 */
function keyedDispatch(cut: Cut, health: Health) {
  const { comments, posts, take } = keyedStore();
  // Requests held unanswered; the row's cleanup answers each, once its client has gone.
  const held: (() => void)[] = [];
  let served = 0;
  const hold = (): Promise<Response> => {
    const { promise, resolve } = Promise.withResolvers<Response>();
    held.push(() => resolve(new Response(null, { status: 499 })));
    return promise;
  };
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/healthz") {
        served += 1;
        if (health === "timed out") return hold();
        if (health === "answering no JSON") {
          return new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
            status: 502,
            headers: { "Content-Type": "text/html" },
          });
        }
        return Response.json(KEYED_LAYER_HEALTH);
      }
      if (request.method !== "POST" || pathname !== ISSUE_COMMENTS) {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      const taken = await take(request);
      if ("replay" in taken) return taken.replay;
      if (posts.length === 1) {
        if (cut === "the client's silence cut") return hold();
        return new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
          status: 502,
          statusText: "Bad Gateway",
          headers: { "Content-Type": "text/html" },
        });
      }
      return Response.json(taken.made, {
        status: 201,
        headers: taken.key === null ? {} : { "Idempotency-Key": taken.key },
      });
    },
  });
  opened.push({
    stop: () => {
      for (const release of held.splice(0)) release();
      server.stop(true);
    },
  });
  const url = `http://127.0.0.1:${server.port}`;
  // Every request the client makes to /healthz, wherever it is sent.
  const asked: string[] = [];
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const { pathname } = new URL(String(input));
    if (pathname !== "/healthz") return fetch(input, init);
    asked.push(`${init?.method ?? "GET"} ${pathname}`);
    return fetch(health === "refused" ? `${REFUSED}/healthz` : input, init);
  }) as typeof fetch;
  return {
    url,
    fetchImpl,
    comments,
    posts,
    asked,
    served: () => served,
  };
}

/**
 * The production pacing, with a clock that moves only when the client pauses, and a silence bound
 * of 200 ms, a real timer, so a cut-off attempt costs the row that much and no more.
 */
function pacing(): DispatchWriteRetry {
  let clock = 0;
  return {
    ...DISPATCH_WRITE_RETRY,
    silenceMs: 200,
    pause: async (ms) => {
      clock += ms;
    },
    now: () => clock,
  };
}

const cuts: readonly Cut[] = ["the client's silence cut", "a gateway's 502 page"];

describe("a keyed write whose answer was lost after Dispatch stored it", () => {
  // The case: `/healthz` is down. A client that asks it before it resends gets no yes, reports
  // that the write may have reached Dispatch and sends it once; the write is sent again under its
  // key instead, and Dispatch answers the repeat with the comment it stored. A probe that times
  // out waits its own bound (5 s at main), so the row's limit is above that.
  for (const cut of cuts) {
    for (const health of ["refused", "timed out", "answering no JSON"] as const) {
      test(`case: lost to ${cut}, with /healthz ${health}, it is sent again under its key and lands once, and /healthz is never asked`, async () => {
        const dispatch = keyedDispatch(cut, health);
        const client = new DispatchClient(
          dispatch.url,
          bearerCredential("secret"),
          dispatch.fetchImpl,
          {
            writeRetry: pacing(),
          }
        );

        // The call's answer, or what it threw, so the one comparison below shows either.
        const got: unknown = await client.comment("DSP-1", comment).then(
          (answer) => ({ answer }),
          (error: Error) => ({ error: `${error.name}: ${error.message}` })
        );

        const key = dispatch.posts[0]?.key;
        expect(key).toEqual(expect.any(String));
        expect({
          got,
          posts: dispatch.posts,
          comments: dispatch.comments.length,
          healthz: dispatch.asked,
        }).toEqual({
          got: { answer: dispatch.comments[0] },
          posts: [
            { key, stored: true },
            { key, stored: false },
          ],
          comments: 1,
          healthz: [],
        });
        expect(dispatch.served()).toBe(0);
      }, 20_000);
    }
  }

  // The control: `/healthz` answers that the keyed layer runs, so a client that asks it and one
  // that does not both send the write again under its key; Dispatch stores it once and the caller
  // gets the stored comment. That shows the rig counts what the server stored and replays it.
  for (const cut of cuts) {
    test(`control: lost to ${cut}, with /healthz answering that the keyed layer runs, it is sent again under its key and lands once`, async () => {
      const dispatch = keyedDispatch(cut, "answering that the keyed layer runs");
      const client = new DispatchClient(
        dispatch.url,
        bearerCredential("secret"),
        dispatch.fetchImpl,
        {
          writeRetry: pacing(),
        }
      );

      const got: unknown = await client.comment("DSP-1", comment).then(
        (answer) => ({ answer }),
        (error: Error) => ({ error: `${error.name}: ${error.message}` })
      );

      const key = dispatch.posts[0]?.key;
      expect(key).toEqual(expect.any(String));
      expect({ got, posts: dispatch.posts, comments: dispatch.comments.length }).toEqual({
        got: { answer: dispatch.comments[0] },
        posts: [
          { key, stored: true },
          { key, stored: false },
        ],
        comments: 1,
      });
      expect(dispatch.comments[0]?.body).toBe("Shipped.");
    }, 20_000);
  }
});

/**
 * A release rolling out, as a keyed write meets it: the first request under a key is stored and
 * its answer replaced by a gateway's 502 page; the second request, whatever its key, reaches the
 * older build, which has no route for it (`404 NOT_FOUND`, "no route for …") and stores nothing;
 * every later request reaches the newer build's keyed layer, which answers a key it holds with the
 * stored answer and stores a key it does not hold as a new comment. Its `/healthz` answers that
 * the keyed layer runs, so a client that asks it before it resends after the 502 (as main's did)
 * sends the write again and reaches the no-route retry, which is what the rows judge.
 */
function rollingDispatch() {
  const { comments, posts, take } = keyedStore();
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/healthz") return Response.json(KEYED_LAYER_HEALTH);
      if (request.method !== "POST" || pathname !== ISSUE_COMMENTS) {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      if (posts.length === 1) {
        posts.push({ key: request.headers.get("Idempotency-Key"), stored: false });
        return Response.json(
          { code: "NOT_FOUND", error: `no route for POST ${ISSUE_COMMENTS}` },
          { status: 404 }
        );
      }
      const taken = await take(request);
      if ("replay" in taken) return taken.replay;
      if (posts.length === 1) {
        return new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
          status: 502,
          statusText: "Bad Gateway",
          headers: { "Content-Type": "text/html" },
        });
      }
      return Response.json(taken.made, {
        status: 201,
        headers: taken.key === null ? {} : { "Idempotency-Key": taken.key },
      });
    },
  });
  opened.push({ stop: () => server.stop(true) });
  return { url: `http://127.0.0.1:${server.port}`, comments, posts };
}

describe("a keyed write stored, then answered 502, then answered with no route", () => {
  // The case: the 502 is asked again under the key and meets the older build's 404 with no route,
  // so the client sends the request again once the wait for the newer build is over. That repeat
  // is the same write, under its first key, and Dispatch answers it with the comment it stored.
  test("case: the no-route retry sends it again under its first key, and it lands once", async () => {
    const dispatch = rollingDispatch();
    const client = new DispatchClient(dispatch.url, bearerCredential("secret"), fetch, {
      writeRetry: pacing(),
      sleep: async () => undefined,
    });

    const got: unknown = await client.comment("DSP-1", comment).then(
      (answer) => ({ answer }),
      (error: Error) => ({ error: `${error.name}: ${error.message}` })
    );

    const key = dispatch.posts[0]?.key;
    expect(key).toEqual(expect.any(String));
    expect({ got, posts: dispatch.posts, comments: dispatch.comments.length }).toEqual({
      got: { answer: dispatch.comments[0] },
      posts: [
        { key, stored: true },
        { key, stored: false },
        { key, stored: false },
      ],
      comments: 1,
    });
  });

  // The control: the same three requests, the last under a fresh key, as a retry that mints its
  // own key sends it. The rig stores it a second time, which shows it tells the keys apart.
  test("control: the same requests with the last under a fresh key land twice", async () => {
    const dispatch = rollingDispatch();
    const post = (key: string) =>
      fetch(`${dispatch.url}${ISSUE_COMMENTS}`, {
        method: "POST",
        headers: { "Content-Type": "application/json", "Idempotency-Key": key },
        body: JSON.stringify(comment),
      });

    const statuses = [
      (await post("key-1")).status,
      (await post("key-1")).status,
      (await post("key-2")).status,
    ];

    expect({ statuses, comments: dispatch.comments.length }).toEqual({
      statuses: [502, 404, 201],
      comments: 2,
    });
  });
});

const ISSUE_ARTIFACTS = "/api/v1/issues/DSP-1/artifacts";

/**
 * The same release rolling out, on the multipart upload route: the first upload under a key is
 * stored and answered with a gateway's 502 page, the second meets the older build's 404 with no
 * route, and every later one reaches the keyed layer, which replays the upload a key holds and
 * stores one it does not. Its `/healthz` answers that the keyed layer runs, as the comment rig's
 * does. Each attempt's bytes are kept, so a row tells one encoding of the form from another.
 */
function rollingUploads() {
  const posts: { key: string | null; stored: boolean; bytes: string }[] = [];
  const answers = new Map<string, unknown>();
  let uploads = 0;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/healthz") return Response.json(KEYED_LAYER_HEALTH);
      if (request.method !== "POST" || pathname !== ISSUE_ARTIFACTS) {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      const key = request.headers.get("Idempotency-Key");
      const bytes = await request.text();
      if (posts.length === 1) {
        posts.push({ key, stored: false, bytes });
        return Response.json(
          { code: "NOT_FOUND", error: `no route for POST ${ISSUE_ARTIFACTS}` },
          { status: 404 }
        );
      }
      const held = key === null ? undefined : answers.get(key);
      if (key !== null && held !== undefined) {
        posts.push({ key, stored: false, bytes });
        return Response.json(held, {
          status: 201,
          headers: { "Idempotency-Key": key, "Idempotency-Replayed": "?1" },
        });
      }
      uploads += 1;
      const made = { artifact: { id: `artifact-${uploads}`, issue_key: "DSP-1" } };
      posts.push({ key, stored: true, bytes });
      if (key !== null) answers.set(key, made);
      if (posts.length === 1) {
        return new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
          status: 502,
          statusText: "Bad Gateway",
          headers: { "Content-Type": "text/html" },
        });
      }
      return Response.json(made, {
        status: 201,
        headers: key === null ? {} : { "Idempotency-Key": key },
      });
    },
  });
  opened.push({ stop: () => server.stop(true) });
  return { url: `http://127.0.0.1:${server.port}`, posts, uploads: () => uploads };
}

describe("a multipart upload stored, then answered 502, then answered with no route", () => {
  // The case: the form is encoded once and keyed once, outside the no-route retry, so every
  // attempt is the same bytes under the first key, and the upload lands once.
  test("case: the no-route retry sends the same bytes under its first key, and it lands once", async () => {
    const dispatch = rollingUploads();
    const client = new DispatchClient(dispatch.url, bearerCredential("secret"), fetch, {
      writeRetry: pacing(),
      sleep: async () => undefined,
    });

    const got = await client
      .artifact("DSP-1", {
        name: "notes.txt",
        file: new Blob(["the notes"], { type: "text/plain" }),
      })
      .then(
        () => "answered",
        (error: Error) => `${error.name}: ${error.message}`
      );

    const first = dispatch.posts[0];
    expect(first?.key).toEqual(expect.any(String));
    expect({
      got,
      posts: dispatch.posts.map((post) => ({
        key: post.key,
        stored: post.stored,
        sameBytes: post.bytes === first?.bytes,
      })),
      uploads: dispatch.uploads(),
    }).toEqual({
      got: "answered",
      posts: [
        { key: first?.key, stored: true, sameBytes: true },
        { key: first?.key, stored: false, sameBytes: true },
        { key: first?.key, stored: false, sameBytes: true },
      ],
      uploads: 1,
    });
  });

  // The control: the same three uploads, the last under a fresh key, as a retry that mints its own
  // key sends it. The rig stores it a second time, which shows it tells the keys apart.
  test("control: the same uploads with the last under a fresh key land twice", async () => {
    const dispatch = rollingUploads();
    const post = (key: string) => {
      const form = new FormData();
      form.set("name", "notes.txt");
      form.set("file", new Blob(["the notes"], { type: "text/plain" }), "notes.txt");
      return fetch(`${dispatch.url}${ISSUE_ARTIFACTS}`, {
        method: "POST",
        headers: { "Idempotency-Key": key },
        body: form,
      });
    };

    const statuses = [
      (await post("key-1")).status,
      (await post("key-1")).status,
      (await post("key-2")).status,
    ];

    expect({ statuses, uploads: dispatch.uploads() }).toEqual({
      statuses: [502, 404, 201],
      uploads: 2,
    });
  });
});
