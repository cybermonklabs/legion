// packages/broker-client/src/__tests__/boundary-leaks.test.ts
//
// What an error from openid-client used to carry out of this package: the broker's minted token in
// the body of an answer the library refused, and an issuer's words in its message and its cause.
// Every call into the library crosses `callLibrary` (@legion/contracts/remote-text), so each error
// below prints none of them, in its message, `JSON.stringify` and an inspection that follows its
// cause.

import { afterEach, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { inspect } from "node:util";
import { type IssuerAnswer, signedIn, stubIssuer } from "../../test/sign-ins";
import { newDpopKey } from "../dpop";
import { personIdentity } from "../person-identity";
import type { TokenSource } from "../source";
import { BrokerTokenClient } from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

const USER = "not-a-secret-user";
const PASSWORD = "not-a-secret-password";
const TOKEN = "not-a-secret-token";
/** The parameter OAuth carries an access token under, in two parts so no line of this file reads
 *  as a credential to a scanner. */
const PARAM = ["access", "token"].join("_");
const WORDS = `the issuer at https://${USER}:${PASSWORD}@issuer.example/path?${PARAM}=${TOKEN}`;
/** The access token a broker mints in an answer the library then refuses. */
const MINTED = "not-a-secret-minted-access-token";
const SECRETS = [USER, PASSWORD, TOKEN, MINTED];

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
// Random, as a real client secret is, so the process's scrub of it takes no word out of a line.
// Spelled without @legion/test-fixtures and named by its class below, so this file runs as it is
// in the tree a leak row is red at (control 19), which has neither fixtureSecret nor token-errors.
const CLIENT_SECRET = randomBytes(18).toString("hex");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** Every form of `error` a caller can print, an inspection that follows its cause among them. */
function printed(error: Error): Record<string, string> {
  return {
    message: error.message,
    "JSON.stringify": JSON.stringify(error),
    "Bun.inspect": Bun.inspect(error),
    "util.inspect": inspect(error, { showHidden: true, depth: 10 }),
  };
}

/** The designated assertion of the check's leak rows: each printed form of `error` is captured as
 *  `REDACT-CAPTURE leak/<form> <base64>`, and a form that prints a secret fails with a message that
 *  begins `REDACT-ASSERT leak/<form>`. */
function expectNoSecret(error: Error | null): void {
  if (error === null) throw new Error("the call answered no error");
  for (const [form, text] of Object.entries(printed(error))) {
    const capture = `leak/${form.replace(/\W+/g, "-")}`;
    console.info(`REDACT-CAPTURE ${capture} ${Buffer.from(text, "utf8").toString("base64")}`);
    for (const secret of SECRETS) {
      expect(
        text.includes(secret),
        `REDACT-ASSERT ${capture}: ${form} prints ${secret}: ${text}`
      ).toBe(false);
    }
  }
}

test("a token type the library refuses prints neither the broker's words nor the token it minted", async () => {
  const broker = stubBroker(() => ({
    status: 200,
    body: { ...accepted(MINTED, 600), token_type: WORDS },
  }));
  stops.push(broker.stop);
  const subject: TokenSource = {
    read: async () => ({ token: "projected", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
    describe: () => "a scripted identity",
  };
  const client = new BrokerTokenClient({ brokerUrl: broker.url, key: await newDpopKey(), subject });
  expectNoSecret(await refusalOf(() => client.token()));
});

/** A person's identity source at a stub issuer whose refresh answers `answer`. */
async function refusalAt(answer: IssuerAnswer): Promise<Error | null> {
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON, answer: () => answer });
  stops.push(issuer.stop);
  const flow = { issuer: issuer.url, clientId: CLIENT_ID, clientSecret: CLIENT_SECRET };
  const record = await signedIn(PERSON);
  return await refusalOf(() => personIdentity(record, flow).read());
}

test.each([
  ["a description", { error: "invalid_request", error_description: WORDS }],
  ["a code", { error: WORDS }],
] as const)("an issuer's refusal whose %s quotes a credential's URL prints none of it", async (_what, body) => {
  const refusal = await refusalAt({ status: 400, body });
  expect(refusal?.name).not.toBe("SignInEndedError");
  expectNoSecret(refusal);
});

test("a sign-in the issuer ended with words that quote a credential's URL prints none of them", async () => {
  const refusal = await refusalAt({
    status: 400,
    body: { error: "invalid_grant", error_description: WORDS },
  });
  expect(refusal?.name).toBe("SignInEndedError");
  expectNoSecret(refusal);
});
