// packages/cipher-client/src/__tests__/refusal-description.test.ts
//
// A broker's `error_description` is a peer's choice of value, and the OAuth library checks only
// that `error` is a string (oauth4webapi's `parseOAuthResponseErrorBody`). A description that is
// not a string is a refusal with no text, never a TypeError out of the client's own reading of it.

import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";
import { BrokerRefusedError } from "../token-errors";
import { refusalOf, stubBroker } from "./stub";

test.each([
  ["a number", 42],
  ["an object", { text: "a description that is no string" }],
  ["a list", ["pod_session_not_registered: a description that is no string"]],
  ["null", null],
])("an error_description that is %s is a refusal with no text", async (_, description) => {
  const broker = stubBroker(() => ({
    status: 400,
    body: { error: "invalid_grant", error_description: description },
  }));
  try {
    const client = new BrokerTokenClient({
      brokerUrl: broker.url,
      key: await newDpopKey(),
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
    expect(refusal?.message).toBe(
      "the secrets broker refused the projected service-account token: HTTP 400 invalid_grant"
    );
    expect((refusal as BrokerRefusedError).podSessionUnregistered).toBe(false);
  } finally {
    broker.stop();
  }
});
