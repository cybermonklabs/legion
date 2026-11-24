// packages/pi-legion/src/holders-print.test.ts
//
// The box print test of this package's holders (@legion/test-fixtures/print-test): each holds the
// probe in every box field, printed by every printer at every placement, beside its twin, the same
// shape with the probe as a string there. stall-evidence.ts's NoticeRateLimited and NoticeAttempt
// are its own; this test builds values of their shapes, each holding the listener's refusal
// (`EnvoyApiError`) built from an answer whose every word quotes the probe.

import { expect, test } from "bun:test";
import { EnvoyApiError } from "@legion/envoy-client/transport";
import { type Holder, printTest, REMOTE_WORDS } from "@legion/test-fixtures/print-test";

/** The listener's refusal of a publish for the sender's rate, whose every word quotes the probe. */
const rateLimited = {
  error: REMOTE_WORDS,
  code: REMOTE_WORDS,
  reason: REMOTE_WORDS,
  expected: [REMOTE_WORDS],
  refused: [REMOTE_WORDS],
  holder: REMOTE_WORDS,
  retry_after_seconds: 15,
};
const refusal = new EnvoyApiError({
  method: "POST",
  url: "http://listener/v1/messages/publish",
  status: 429,
  responseBody: JSON.stringify(rateLimited),
});

/** `EnvoyApiError`'s twin: an Error whose words are public strings, as they came, and whose
 *  message quotes them. */
class EnvoyApiErrorTwin extends Error {
  override readonly name = "EnvoyApiError";
  readonly details = {
    method: "POST",
    url: "http://listener/v1/messages/publish",
    status: 429,
    responseBody: JSON.stringify(rateLimited),
  };
  readonly expected = [REMOTE_WORDS];
  readonly code = REMOTE_WORDS;
  readonly reason = REMOTE_WORDS;
  readonly holder = REMOTE_WORDS;
  readonly refused = [REMOTE_WORDS];
  readonly retryAfterSeconds = 15;
  constructor() {
    super(`${REMOTE_WORDS} (reason: ${REMOTE_WORDS})`);
  }
}

const holders: Holder[] = [
  {
    name: "pi-legion.NoticeRateLimited",
    kind: "object",
    boxed: { kind: "rate-limited", retryAfterSeconds: 15, error: refusal },
    same: { kind: "rate-limited", retryAfterSeconds: 15, error: new EnvoyApiErrorTwin() },
  },
  {
    // A refused notice's attempt holds the refusal as it came, whatever it is.
    name: "pi-legion.NoticeAttempt",
    kind: "object",
    boxed: { kind: "refused", error: refusal },
    same: { kind: "refused", error: new EnvoyApiErrorTwin() },
  },
];

for (const holder of holders) {
  test(`box print test: ${holder.name} prints no secret by any printer at any placement`, () => {
    expect(printTest(holder).map((f) => f.why)).toEqual([]);
  });
}
