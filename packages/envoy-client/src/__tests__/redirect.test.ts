import { expect, test } from "bun:test";
import { redirectedTo, redirectTarget } from "../redirect";

const redirect = (status: number, location?: string) =>
  new Response(null, { status, headers: location === undefined ? {} : { Location: location } });

// The origin tells a person who wrote `http://` for an `https://` server what to correct: the
// scheme and the host. Never a path, a query, a fragment or a user, any of which can carry a secret.
test("a redirect to another origin names that origin, and nothing else of its Location", () => {
  // A target that carries a user and a password, built from its parts; the password is made for
  // this run, so no line of the test holds a credential.
  const hidden = crypto.randomUUID();
  const withUser = new URL("https://dispatch.example.com/api/v1/issues/ACME-1");
  withUser.username = "user";
  withUser.password = hidden;
  for (const [location, origin] of [
    [
      "https://sso.example.com:8443/start?session=s3cr3t-query#frag",
      "https://sso.example.com:8443",
    ],
    ["https://dispatch.example.com:443/api", "https://dispatch.example.com"],
    [withUser.href, "https://dispatch.example.com"],
    ["//other.example/login?next=%2Fapi", "http://other.example"],
    ["https://dispatch.test/api/v1/x", "https://dispatch.test"],
  ] as const) {
    for (const status of [301, 302, 303, 307, 308]) {
      const target = redirectTarget(redirect(status, location), "http://dispatch.test/api/v1/x");
      expect(target, location).toEqual({ kind: "another origin", origin });
      const words = redirectedTo(target ?? { kind: "no target" });
      expect(words, location).toBe(`redirecting to ${origin}`);
      for (const part of ["/start", "s3cr3t-query", "frag", hidden, "user", "next="]) {
        expect(words, location).not.toContain(part);
      }
    }
  }
});

// A redirect within the request's own origin names no address the client lacks; it says that
// the server sent the request to another path of the same address.
test("a redirect within the request's own origin is another path of the same address", () => {
  for (const location of [
    "/login?next=%2Fapi",
    "?page=2",
    "#part",
    "http://dispatch.test:80/elsewhere",
    "HTTP://DISPATCH.TEST/upper",
  ]) {
    const target = redirectTarget(redirect(307, location), "http://dispatch.test/api/v1/x");
    expect(target, location).toEqual({ kind: "the same origin" });
    expect(redirectedTo({ kind: "the same origin" })).toBe(
      "redirecting to another path of the same address"
    );
  }
});

test("a Location that is missing, empty, unreadable or not http(s) names no target", () => {
  for (const response of [
    redirect(307),
    redirect(307, ""),
    redirect(302, "http://[not a host"),
    redirect(301, "ftp://files.example.com/x"),
    redirect(300, "javascript:alert(1)"),
  ]) {
    expect(redirectTarget(response, "http://dispatch.test/")).toEqual({ kind: "no target" });
  }
  expect(redirectedTo({ kind: "no target" })).toBe("naming no target");
});

test("an answer that is no 3xx names no target, whatever its Location", () => {
  for (const status of [200, 204, 299, 400, 404, 500]) {
    expect(
      redirectTarget(redirect(status, "https://elsewhere.example"), "http://dispatch.test/")
    ).toBeUndefined();
  }
});

// No caller reads a 3xx's body, so the read of where it points cancels that body: one that never
// ends holds no connection. An answer that is no 3xx keeps its body for the caller to read. A
// body that is never cancelled leaves the wait below pending, and the test's limit fails it.
test("a 3xx's body is cancelled as its target is read, and any other answer keeps its body", async () => {
  const cancelled = Promise.withResolvers<void>();
  const redirected = new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("the first chunk of a body that "));
      },
      cancel() {
        cancelled.resolve();
      },
    }),
    { status: 307, headers: { Location: "/elsewhere" } }
  );
  expect(redirectTarget(redirected, "http://dispatch.test/")).toEqual({ kind: "the same origin" });
  await cancelled.promise;
  const plain = new Response("a body to read", { status: 200, headers: { Location: "/x" } });
  expect(redirectTarget(plain, "http://dispatch.test/")).toBeUndefined();
  expect(await plain.text()).toBe("a body to read");
});

// The cancel of a 3xx's body can fail: a body already locked by a reader, and a source whose own
// cancel throws, each reject it. That failure is no answer of the request, so the read of the
// target still names it, and leaves no rejection unhandled. The wait for one is a turn of the event
// loop after the source's cancel ran: an unhandled rejection is reported before that turn.
test("a 3xx whose body cannot be cancelled still names its target, and leaves no rejection unhandled", async () => {
  const unhandled: unknown[] = [];
  const record = (reason: unknown) => unhandled.push(reason);
  process.on("unhandledRejection", record);
  try {
    const asked = Promise.withResolvers<void>();
    const failing = new Response(
      new ReadableStream<Uint8Array>({
        cancel() {
          asked.resolve();
          throw new Error("a made-up failure of the cancel");
        },
      }),
      { status: 307, headers: { Location: "/elsewhere" } }
    );
    const locked = new Response("a body a reader holds", {
      status: 307,
      headers: { Location: "/elsewhere" },
    });
    locked.body?.getReader();
    for (const response of [failing, locked]) {
      expect(redirectTarget(response, "http://dispatch.test/")).toEqual({
        kind: "the same origin",
      });
    }
    await asked.promise;
    const turn = Promise.withResolvers<void>();
    setImmediate(turn.resolve);
    await turn.promise;
    expect(unhandled).toEqual([]);
  } finally {
    process.off("unhandledRejection", record);
  }
});

// A host name holds at most 253 characters, so an origin longer than `https://`, such a name and
// `:65535` names no host a person meant; it prints as another address, with its length.
test("an origin longer than any host name allows prints as another address of its length", () => {
  const longest = `https://${"a".repeat(253)}:65535`;
  expect(longest).toHaveLength(267);
  expect(redirectedTo({ kind: "another origin", origin: longest })).toBe(
    `redirecting to ${longest}`
  );
  const longer = `https://${"a".repeat(254)}:65535`;
  expect(redirectedTo({ kind: "another origin", origin: longer })).toBe(
    "redirecting to another address, whose origin is 268 characters long"
  );
  const target = redirectTarget(
    redirect(307, `http://${"h".repeat(70_000)}.example/x`),
    "http://dispatch.test/"
  );
  expect(redirectedTo(target ?? { kind: "no target" })).toBe(
    "redirecting to another address, whose origin is 70015 characters long"
  );
});
