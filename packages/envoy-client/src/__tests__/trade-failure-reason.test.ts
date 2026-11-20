import { describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { newDpopKey } from "@legion/broker-client/dpop";
import { KNOWN_BROKER_REFUSALS } from "@legion/broker-client/refusal-text";
import { BrokerTokenClient } from "@legion/broker-client/token-client";
import { BrokerRefusedError, TokenClientClosedError } from "@legion/broker-client/token-errors";
import { brokerTokenFor } from "../broker-credential";
import { tradeFailureReason } from "../nats-credential";

// What the NATS line prints of a failed trade for the session's token (`tradeFailureReason`). A
// peer at AGENT_SECRETS_URL can put the subject token it was sent in its refusal's text, so the
// broker's text prints only where it is one of its known refusals (#4140, S1); a trade that got
// no answer keeps its class, which broker-client's own text already holds (#4140, S2).
describe("tradeFailureReason", () => {
  const token = () =>
    `eyJ${randomBytes(18).toString("base64url")}.${randomBytes(24).toString("base64url")}`;

  test("a refusal whose text is not a known refusal prints its status and code, its text withheld", () => {
    const echoed = token();
    const refusal = new BrokerRefusedError(400, "invalid_grant", `refused ${echoed}`, "probe");
    const reasons = [tradeFailureReason(refusal), refusal.message, Bun.inspect(refusal)];
    expect(reasons[0]).toBe(
      "the secrets broker refused the identity traded: HTTP 400 invalid_grant: (its text is withheld)"
    );
    expect(reasons[1]).toBe(
      "the secrets broker refused probe: HTTP 400 invalid_grant: (its text is withheld)"
    );
    for (const reason of reasons) expect(reason).not.toContain(echoed);
  });

  // A code the clients do not handle is a peer's choice of characters, which can be the token
  // lowercased past the case-sensitive scrub: it prints as its length alone (#4140, AcceptE2-2's
  // x-code-lowercased-token).
  test("a code the clients do not handle prints as its length alone", () => {
    const echoed = token();
    expect(tradeFailureReason(new BrokerRefusedError(400, echoed, "", "probe"))).toBe(
      `the secrets broker refused the identity traded: HTTP 400 unrecognised error code (${echoed.length} chars)`
    );
  });

  test.each([
    ...KNOWN_BROKER_REFUSALS,
  ])("the known refusal %p prints as the broker wrote it", (text) => {
    expect(tradeFailureReason(new BrokerRefusedError(403, "invalid_grant", text, "probe"))).toBe(
      `the secrets broker refused the identity traded: HTTP 403 invalid_grant: ${text}`
    );
  });

  // The broker's sentences that carry a remedy print, with only a person or a project in their
  // slots; the same sentence with a token in the slot is withheld.
  test("a broker sentence naming a person prints, and the same sentence with a token in it does not", () => {
    const sentence = (person: string) =>
      `${person} revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with \`agent-secrets login\`.`;
    const named = new BrokerRefusedError(
      400,
      "invalid_grant",
      sentence("ada@example.com"),
      "probe"
    );
    expect(tradeFailureReason(named)).toBe(
      `the secrets broker refused the identity traded: HTTP 400 invalid_grant: ${sentence("ada@example.com")}`
    );
    const echoed = token();
    const refusal = new BrokerRefusedError(400, "invalid_grant", sentence(echoed), "probe");
    expect(tradeFailureReason(refusal)).toBe(
      "the secrets broker refused the identity traded: HTTP 400 invalid_grant: (its text is withheld)"
    );
    expect(Bun.inspect(refusal)).not.toContain(echoed);
  });

  test("a trade that got no answer keeps the failure's class", async () => {
    const client = new BrokerTokenClient({
      brokerUrl: "http://127.0.0.1:1",
      key: await newDpopKey(),
      subject: {
        read: async () => ({ token: token(), tokenType: "urn:ietf:params:oauth:token-type:jwt" }),
        describe: () => "probe",
      },
      log: () => {},
    });
    // The refused connection fails this call at once, since it fails fast, while the retry loop
    // trades again.
    const failure = await client.token(AbortSignal.timeout(1_000)).then(
      () => undefined,
      (error: unknown) => error
    );
    client.close();
    expect(tradeFailureReason(failure)).toBe(
      "trade probe at http://127.0.0.1:1/v1/oauth/token: ECONNREFUSED, and the trade is being retried in the background"
    );
  });

  test("this code's own failures print whole, not as an error whose message is withheld", async () => {
    // The review of 2fa6fbf7cf, m8: a closed client, and a machine whose broker route went away
    // between two trades, printed as "Error (message withheld)".
    const client = new BrokerTokenClient({
      brokerUrl: "http://127.0.0.1:1",
      key: await newDpopKey(),
      subject: {
        read: async () => ({ token: token(), tokenType: "jwt" }),
        describe: () => "probe",
      },
      log: () => {},
    });
    client.close();
    const closed = await client.token().then(
      () => undefined,
      (error: unknown) => error
    );
    expect(closed).toBeInstanceOf(TokenClientClosedError);
    expect(tradeFailureReason(closed)).toBe(
      "the secrets broker client was closed, and trades no more"
    );
    const gone = await brokerTokenFor({ HOME: "/nonexistent-home" }, "ses_a")().then(
      () => undefined,
      (error: unknown) => error
    );
    expect(tradeFailureReason(gone)).toBe(
      "AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY no longer name a broker and an identity to trade"
    );
  });
});
