// packages/cipher-client/src/__tests__/released-registration-refusal.test.ts
//
// The token exchange refuses a pod's trade under a registration its registrar released
// (packages/envoy/internal/cipher/oauth/exchange.go, registeredPodSession), and no later trade
// cures that refusal. Its hint is the broker's own fixed words naming the session the trade sent,
// so the client prints it whole, as it prints the refusal of a registration bound to another key.
// Withheld, it leaves a pod's log saying only that a trade was refused, never that the registrar
// released the name, which is the one thing that tells an operator why the pod stopped.

import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";
import { BrokerRefusedError, isFinalRefusal } from "../token-errors";
import { refusalOf, stubBroker } from "./stub";

/** The OAuth library's fixed sentence the broker writes ahead of an `invalid_grant` hint. */
const LIBRARY_INVALID_GRANT =
  "The provided authorization grant (e.g., authorization code, resource owner credentials) or refresh token is invalid, expired, revoked, does not match the redirection URI used in the authorization request, or was issued to another client.";

/** The exchange's refusal of a released registration of `session`, as the broker sends it. */
function released(session: string): string {
  return `${LIBRARY_INVALID_GRANT} Refused: this pod's registration for session '${session}' was released; a pod trades only under a registration its registrar has not released.`;
}

/** The refusal a client trading for `session` reads from a broker that answers `description`. */
async function refusalOf400(session: string, description: string): Promise<BrokerRefusedError> {
  const broker = stubBroker(() => ({
    status: 400,
    body: { error: "invalid_grant", error_description: description },
  }));
  try {
    const client = new BrokerTokenClient({
      brokerUrl: broker.url,
      key: await newDpopKey(),
      session,
      launcherCredential: false,
      subject: {
        read: async () => ({
          token: fixtureSecret("subject"),
          tokenType: "urn:ietf:params:oauth:token-type:jwt",
        }),
        describe: () => "the projected service-account token",
      },
    });
    const refusal = await refusalOf(() => client.token());
    expect(refusal).toBeInstanceOf(BrokerRefusedError);
    return refusal as BrokerRefusedError;
  } finally {
    broker.stop();
  }
}

test("the refusal of a released registration prints whole, and no trade cures it", async () => {
  const refusal = await refusalOf400("ses_released", released("ses_released"));
  expect(refusal.said).toBe(released("ses_released"));
  expect(refusal.message).toBe(
    `the secrets broker refused the projected service-account token: HTTP 400 invalid_grant: ${released("ses_released")}`
  );
  expect(refusal.podSessionUnregistered).toBe(false);
  expect(isFinalRefusal(refusal)).toBe(true);
});

test("the same refusal naming a session the trade did not send is withheld", async () => {
  const refusal = await refusalOf400("ses_released", released("ses_other"));
  expect(refusal.said).toBe("(its text is withheld)");
});
