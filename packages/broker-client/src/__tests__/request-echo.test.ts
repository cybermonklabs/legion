// An answer in the broker's place can echo back any credential the trade carried: the subject
// token in its form, its DPoP proof, whatever it sent. openid-client parses the answer as it came
// and throws over it; the error this client hands on prints none of the trade's credentials, while
// its code, which the client acts on, is the one the answer gave (the review of #4368 at
// db7e4a0226, finding 1 and the red team's echo probes). Each print is a designated assertion: it
// writes `REDACT-CAPTURE <capture> <base64>` and fails with a message that begins
// `REDACT-ASSERT <capture>`.

import { expect, test } from "bun:test";
import { inspect } from "node:util";
import { newDpopKey } from "../dpop";
import type { TokenSource } from "../source";
import { BrokerTokenClient } from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { refusalOf, stubBroker } from "./stub";

/** The subject token: the check's own token marker, so a leak of it in the captured output is one
 *  the mutation gate names. */
const SUBJECT = "not-a-secret-token";

const SUBJECT_SOURCE: TokenSource = {
  read: async () => ({ token: SUBJECT, tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
  describe: () => "a scripted identity",
};

/** The designated assertion over every way a caller prints `error`. */
function requireNoSubject(capture: string, error: Error): void {
  const forms: Record<string, string> = {
    message: error.message,
    String: String(error),
    inspect: inspect(error, { depth: 8 }),
    JSON: JSON.stringify(error),
  };
  for (const [form, text] of Object.entries(forms)) {
    console.log(
      `REDACT-CAPTURE ${capture}/${form} ${Buffer.from(text, "utf8").toString("base64")}`
    );
    expect(
      text.includes(SUBJECT),
      `REDACT-ASSERT ${capture}/${form}: the output prints the subject token: ${text}`
    ).toBe(false);
  }
}

test("a refusal whose description echoes the subject token prints none of it, and keeps its code", async () => {
  const broker = stubBroker((trade) => ({
    status: 400,
    body: {
      error: "invalid_grant",
      error_description: `subject_token ${trade.form.get("subject_token")} is not one this broker reads`,
    },
  }));
  try {
    const client = new BrokerTokenClient({
      brokerUrl: broker.url,
      key: await newDpopKey(),
      subject: SUBJECT_SOURCE,
    });
    const refusal = await refusalOf(() => client.token());
    // Named by its class, so this file runs as it is in the tree its leak row is red at (control
    // 19), which has no token-errors.
    expect(refusal?.name).toBe("BrokerRefusedError");
    expect(broker.trades[0]?.form.get("subject_token")).toBe(SUBJECT);
    requireNoSubject("ts.request-echo/description", refusal as Error);
    expect((refusal as { code?: unknown }).code).toBe("invalid_grant");
  } finally {
    broker.stop();
  }
});
