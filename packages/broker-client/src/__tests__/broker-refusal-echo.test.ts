// A failing test for #4140 at aeb4d42f09, found by the acceptance (AcceptE2-2): a broker refusal
// prints the broker's error_description whole (BrokerRefusedError), and a broker can put there the
// subject token the client traded, the machine's own identity token. On the real stack, with
// AGENT_SECRETS_URL naming such a broker, pi-envoy's notice and the Claude Code channel server's
// failure line both carried that token. The error code is printed by its code alone (a broker
// that echoes into `error` printed nothing); the description is not. Put it in
// packages/broker-client/src/__tests__/ and run `bun test src/__tests__/broker-refusal-echo.test.ts`
// in packages/broker-client.
import { describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";

describe("a broker refusal", () => {
  test("names no part of the subject token the broker echoes in its error_description", async () => {
    const subject = `eyJhbGciOiJSUzI1NiJ9.${randomBytes(24).toString("base64url")}.${randomBytes(32).toString("base64url")}`;
    const received: string[] = [];
    const broker = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      async fetch(request) {
        const echoed = new URLSearchParams(await request.text()).get("subject_token") ?? "";
        received.push(echoed);
        return Response.json(
          { error: "invalid_grant", error_description: `refused ${echoed}` },
          { status: 400 }
        );
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
      // The row can fail for the leak: the broker was sent the token, and the trade failed.
      expect(received).toContain(subject);
      expect(failure).not.toBe("traded");
      const signature = subject.split(".")[2] as string;
      expect(failure).not.toContain(signature.slice(0, 16));
    } finally {
      broker.stop(true);
    }
  }, 20_000);
});
