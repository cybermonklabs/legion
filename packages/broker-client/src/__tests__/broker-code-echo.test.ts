// A failing test for #4140 at bfb0e7dfeb, found by the acceptance (AcceptE2-2): a broker refusal
// shows the broker's `error` code whenever it reads as an OAuth code (`^[a-z][a-z0-9_]{0,63}$`),
// and a broker can put there the subject token it was sent, lowercased, which the case-sensitive
// scrub of held secrets cannot see. On the real stack (row x-code-lowercased-token), pi-envoy's
// notice and the Claude Code channel server's failure line both carried 60 characters of the
// token's signature this way. The code is to show only when it is one of the OAuth codes the
// clients handle; any other prints as a fixed "unrecognised error code" with its length. Put it in
// packages/broker-client/src/__tests__/ and run `bun test src/__tests__/broker-code-echo.test.ts`
// in packages/broker-client.
import { describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";

/** What the broker answered with, and what the client's error says. */
async function refusedWith(code: (signature: string) => string) {
  const subject = `eyJhbGciOiJSUzI1NiJ9.${randomBytes(24).toString("base64url")}.${randomBytes(48).toString("base64url")}`;
  const signature = subject.split(".")[2] as string;
  const received: string[] = [];
  const sent = code(signature);
  const broker = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      received.push(new URLSearchParams(await request.text()).get("subject_token") ?? "");
      return Response.json({ error: sent, error_description: "" }, { status: 400 });
    },
  });
  try {
    const client = new BrokerTokenClient({
      brokerUrl: `http://127.0.0.1:${broker.port}`,
      key: await newDpopKey(),
      subject: {
        read: async () => ({ token: subject, tokenType: "urn:ietf:params:oauth:token-type:jwt" }),
        describe: () => "the projected service-account token",
      },
    });
    const failure = await client.token().then(
      () => "traded",
      (error: unknown) => (error instanceof Error ? error.message : String(error))
    );
    return { subject, sent, received, failure };
  } finally {
    broker.stop(true);
  }
}

describe("a broker refusal's error code", () => {
  test("names no part of the subject token a broker lowercased into the code", async () => {
    // An `x` first, so the code always reads as an OAuth code: a base64url signature can start with
    // a digit or `_`, which no code does. The 59 characters after it are the token's.
    const { subject, sent, received, failure } = await refusedWith(
      (signature) => `x${signature.toLowerCase().replace(/-/g, "").slice(0, 59)}`
    );
    // The row can fail for the leak: the broker was sent the token, the code it answered reads as
    // an OAuth code, and the trade failed.
    expect(received).toContain(subject);
    expect(sent).toMatch(/^[a-z][a-z0-9_]{0,63}$/);
    expect(failure).not.toBe("traded");
    expect(failure.toLowerCase()).not.toContain(sent.slice(1, 17));
  }, 20_000);

  test("still shows a code the clients handle", async () => {
    const { failure } = await refusedWith(() => "invalid_grant");
    expect(failure).toContain("invalid_grant");
  }, 20_000);
});
