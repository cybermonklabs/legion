// packages/cipher-client/src/__tests__/oauth-refusal-class.test.ts
//
// Every client reads an OAuth error the broker or an issuer answered by
// `LibraryError.answeredOAuthError`, the code oauth4webapi gives its `ResponseBodyError`, and no
// longer by the error's name. The library names each error by its `constructor.name`, so a
// subclass of the class (or a bundle that renames it) answered the broker's refusal as a failure
// of the trade, and any error named `ResponseBodyError` read as the broker's refusal. The row is
// the broker's refusal (`brokerRefusal`), with the library's own class.

import { expect, test } from "bun:test";
import { LibraryError } from "@legion/contracts/remote-text";
import { ResponseBodyError } from "openid-client";
import { brokerRefusal } from "../broker-request";
import { BrokerRefusedError, BrokerTradeFailedError } from "../token-errors";

const ENDPOINT = "http://broker.test/v1/oauth/token";

/** The library's own class under a name of its own. */
class RenamedResponseBodyError extends ResponseBodyError {}

test("an OAuth error answered under another class name is the broker's refusal", () => {
  const thrown = new RenamedResponseBodyError(
    "server responded with an error in the response body",
    {
      cause: { error: "invalid_grant", error_description: "the broker refused this request" },
      response: new Response(null, { status: 400 }),
    }
  );
  // The library names it by its class: the name is not ResponseBodyError, the code is the class's.
  expect([thrown.name, thrown.code]).toEqual([
    "RenamedResponseBodyError",
    "OAUTH_RESPONSE_BODY_ERROR",
  ]);

  const refusal = brokerRefusal("trade", "the pod's identity", ENDPOINT, new LibraryError(thrown));
  expect(refusal).toBeInstanceOf(BrokerRefusedError);
  expect((refusal as BrokerRefusedError).status).toBe(400);
});

test("an error only named ResponseBodyError is a failure of the trade, not a refusal", () => {
  const thrown = Object.assign(new Error("server responded with an error in the response body"), {
    name: "ResponseBodyError",
    status: 400,
    error: "invalid_grant",
  });

  const refusal = brokerRefusal("trade", "the pod's identity", ENDPOINT, new LibraryError(thrown));
  expect(refusal).toBeInstanceOf(BrokerTradeFailedError);
  expect(refusal.message).toBe(
    "trade the pod's identity at http://broker.test/v1/oauth/token: HTTP 400: ResponseBodyError (message withheld)"
  );
});
