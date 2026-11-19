// packages/broker-client/src/__tests__/session-name-not-this-registrars.test.ts
//
// The token exchange refuses a pod's trade under a session name another daemon holds: a daemon
// other than the one the name is bound to for good wrote the pod's live registration
// (packages/envoy/internal/broker/oauth/exchange.go, registeredPodSession). No later trade cures
// that refusal. Its description is `session_name_not_this_registrars:` and the broker's own fixed
// words naming the session the trade sent, so the client prints it whole, as it prints the
// refusal of a released registration. Withheld, it leaves a pod's log saying only that a trade
// was refused, never that the name belongs to another daemon, which is the one thing that tells
// an operator why the pod stopped. The exchange refuses a live registration of a name bound to no
// daemon with the same code and words of its own, and the client prints that whole too.

import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";
import { BrokerRefusedError, isFinalRefusal } from "../token-errors";
import { SESSION_NAME_NOT_THIS_REGISTRARS } from "../wire";
import { refusalOf, stubBroker } from "./stub";

/** The exchange's refusal of a pod another daemon registered under `session`, as the broker
 *  sends it. */
function notThisRegistrars(session: string): string {
  return `${SESSION_NAME_NOT_THIS_REGISTRARS}: The session name '${session}' belongs to another daemon: a pod trades only under a session name the daemon that registered it holds.`;
}

/** The exchange's refusal of a pod registered under `session` where `session` is bound to no
 *  daemon, as the broker sends it. */
function boundToNone(session: string): string {
  return `${SESSION_NAME_NOT_THIS_REGISTRARS}: The session name '${session}' is bound to no daemon: a pod trades only under a session name the daemon that registered it holds.`;
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

test("the refusal of a session name another daemon holds prints whole, and no trade cures it", async () => {
  const refusal = await refusalOf400("ses_held", notThisRegistrars("ses_held"));
  expect(refusal.said).toBe(notThisRegistrars("ses_held"));
  expect(refusal.message).toBe(
    `the secrets broker refused the projected service-account token: HTTP 400 invalid_grant: ${notThisRegistrars("ses_held")}`
  );
  expect(refusal.podSessionUnregistered).toBe(false);
  expect(isFinalRefusal(refusal)).toBe(true);
});

test("the refusal of a session name bound to no daemon prints whole, and no trade cures it", async () => {
  const refusal = await refusalOf400("ses_unbound", boundToNone("ses_unbound"));
  expect(refusal.said).toBe(boundToNone("ses_unbound"));
  expect(refusal.message).toBe(
    `the secrets broker refused the projected service-account token: HTTP 400 invalid_grant: ${boundToNone("ses_unbound")}`
  );
  expect(refusal.podSessionUnregistered).toBe(false);
  expect(isFinalRefusal(refusal)).toBe(true);
});

test("the same refusal naming a session the trade did not send is withheld", async () => {
  const refusal = await refusalOf400("ses_held", notThisRegistrars("ses_other"));
  expect(refusal.said).toBe("(its text is withheld)");
});
