// packages/broker-client/src/__tests__/remote-words.test.ts
//
// The broker's words are its own, and they can quote a URL that carries a credential: a user and
// password in its user information, a token in a query or fragment value. A refusal prints a code
// only where it is one the clients handle, and a description only where it is a known refusal
// (`token-errors.ts`), each read through the box (`RemoteText`), so none of these words prints: in
// its message, in `JSON.stringify`, in Bun's and Node's inspection, and in a registration still
// pending that quotes it. An answer's token_type and credential_expires_at are quoted nowhere.
//
// Each check is the designated assertion of cmd/redact-gate's mutation gate: it prints the output
// it read as `REDACT-CAPTURE <capture> <base64>` and fails with a message that begins
// `REDACT-ASSERT <capture>`.

import { expect, test } from "bun:test";
import { inspect } from "node:util";
import { RemoteText } from "@legion/contracts/remote-text";
import { newDpopKey } from "../dpop";
import { REFUSAL_TEXT_WITHHELD } from "../refusal-text";
import type { TokenSource } from "../source";
import { type BrokerToken, BrokerTokenClient } from "../token-client";
import { BrokerRefusedError, RegistrationPendingError } from "../token-errors";
import { SUBJECT_TOKEN_TYPE_KUBERNETES, TOKEN_TYPE_DPOP } from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

const USER = "not-a-secret-user";
const PASSWORD = "not-a-secret-password";
const TOKEN = "not-a-secret-token";
/** The parameter OAuth carries an access token under, in two parts so no line of this file reads
 *  as a credential to a scanner. */
const PARAM = ["access", "token"].join("_");
const WORDS = `the issuer at https://${USER}:${PASSWORD}@issuer.example/path?${PARAM}=${TOKEN}`;
const CODE = `https://${USER}:${PASSWORD}@issuer.example/code?${PARAM}=${TOKEN}`;
/** The code as a refusal shows a code the clients do not handle: its length as the rule reads it,
 *  none of its characters. */
const CODE_SHOWN = `unrecognised error code (${String(new RemoteText(CODE)).length} chars)`;

const DESCRIPTIONS = [
  { name: "user information and a query value", words: WORDS },
  {
    name: "a fragment's value",
    words: `sign in again at https://issuer.example/callback#${PARAM}=${TOKEN}`,
  },
  { name: "words the rule cannot read", words: `${WORDS} refused the subject token` },
] as const;

const SCRIPTED: TokenSource = {
  read: async () => ({ token: "projected", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
  describe: () => "a scripted identity",
};

/** The designated assertion: capture text, then fail where it holds a secret. */
function requireNone(capture: string, text: string): void {
  console.log(`REDACT-CAPTURE ${capture} ${Buffer.from(text, "utf8").toString("base64")}`);
  for (const secret of [USER, PASSWORD, TOKEN]) {
    expect(
      text.includes(secret),
      `REDACT-ASSERT ${capture}: the output prints the secret ${secret}: ${text}`
    ).toBe(false);
  }
}

/** requireNone for every form of `error` a caller can print, under capture/<form>. */
function requireNoneFormatted(capture: string, error: Error): void {
  const forms: Record<string, string> = {
    message: error.message,
    String: String(error),
    "JSON.stringify": JSON.stringify(error),
    "JSON.stringify-held": JSON.stringify({ error }),
    "Bun.inspect": Bun.inspect(error),
    "util.inspect": inspect(error, { showHidden: true, depth: 5 }),
    stack: error.stack ?? "",
  };
  for (const [form, text] of Object.entries(forms)) requireNone(`${capture}/${form}`, text);
}

/** The refusal of one trade the broker answers with `status` and `body`. */
async function refusalFor(status: number, body: Record<string, unknown>): Promise<Error | null> {
  const broker = stubBroker(() => ({ status, body }));
  try {
    const client = new BrokerTokenClient({
      brokerUrl: broker.url,
      key: await newDpopKey(),
      subject: SCRIPTED,
    });
    return await refusalOf(() => client.token());
  } finally {
    broker.stop();
  }
}

const REFUSALS = [
  {
    name: "the code, with a description",
    code: CODE,
    description: "revoked",
    want: `HTTP 400 ${CODE_SHOWN}: ${REFUSAL_TEXT_WITHHELD}`,
  },
  {
    name: "the code, with no description",
    code: CODE,
    description: "",
    want: `HTTP 400 ${CODE_SHOWN}`,
  },
  ...DESCRIPTIONS.map((row) => ({
    name: `the description: ${row.name}`,
    code: "invalid_grant",
    description: row.words,
    want: `HTTP 400 invalid_grant: ${REFUSAL_TEXT_WITHHELD}`,
  })),
];

test.each(
  REFUSALS.map((row) => [row.name, row] as const)
)("a refusal prints none of the broker's words it does not know: %s", async (_name, row) => {
  const refusal = await refusalFor(400, { error: row.code, error_description: row.description });
  expect(refusal).toBeInstanceOf(BrokerRefusedError);
  requireNoneFormatted("ts.BrokerRefusedError", refusal as Error);
  expect(refusal?.message).toBe(`the secrets broker refused a scripted identity: ${row.want}`);
  expect((refusal as BrokerRefusedError).code).toBe(row.code === "invalid_grant" ? row.code : "");
});

test.each(
  DESCRIPTIONS.map((row) => [row.name, row] as const)
)("a registration still pending prints none of the refusal's words: %s", (_name, row) => {
  const refusal = new BrokerRefusedError(
    400,
    "invalid_grant",
    `pod_session_not_registered: ${row.words}`,
    "a pod's projected service-account token"
  );
  const pending = new RegistrationPendingError("session-1", refusal);
  requireNoneFormatted("ts.RegistrationPendingError", pending);
  expect(pending.message).toContain(`(${REFUSAL_TEXT_WITHHELD})`);
  expect(refusal.podSessionUnregistered).toBe(true);
});

test.each(
  DESCRIPTIONS.map((row) => [row.name, row] as const)
)("an answer's credential_expires_at is quoted nowhere: %s", async (_name, row) => {
  const refusal = await refusalFor(200, {
    access_token: "a",
    token_type: "DPoP",
    expires_in: 60,
    issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
    credential_expires_at: row.words,
  });
  requireNone("ts.credential-expires-at", refusal?.message ?? "");
  expect(refusal?.message).toContain("answered a credential_expires_at that is no RFC 3339 time");
});

/** One trade at a stub broker whose accepted answer has `field` set to `value`. */
async function tradeWith(
  field: string,
  value: unknown
): Promise<{ token?: BrokerToken; error?: Error }> {
  const broker = stubBroker(() => ({
    status: 200,
    body: { ...accepted("broker-access-token", 600), [field]: value },
  }));
  try {
    const client = new BrokerTokenClient({
      brokerUrl: broker.url,
      key: await newDpopKey(),
      subject: SCRIPTED,
    });
    return await client.token().then(
      (token) => ({ token }),
      (error: unknown) => ({ error: error instanceof Error ? error : new Error(String(error)) })
    );
  } finally {
    broker.stop();
  }
}

test("a token type is compared as it came and kept as the closed DPoP", async () => {
  const { token, error } = await tradeWith("token_type", "dpop");
  expect(error).toBeUndefined();
  expect(token?.tokenType).toBe(TOKEN_TYPE_DPOP);
});

// `openid-client` refuses a token_type other than bearer and DPoP before this client reads the
// answer, so the token type that reaches `#read` and is refused there is Bearer.
test.each([
  ["token_type", "Bearer", "a token_type that is not DPoP"],
  [
    "credential_expires_at",
    DESCRIPTIONS[0].words,
    "a credential_expires_at that is no RFC 3339 time",
  ],
  ["credential_expires_at", "soon", "a credential_expires_at that is no RFC 3339 time"],
] as const)("an answer whose %s is not this grant's quotes none of it", async (field, value, said) => {
  const { token, error } = await tradeWith(field, value);
  expect(token).toBeUndefined();
  if (error === undefined) throw new Error(`${field} ${value} was taken`);
  requireNoneFormatted("ts.token-answer", error);
  expect(error?.message).toContain(said);
  expect(error?.message).not.toContain(JSON.stringify(value));
});

test("a credential expiry is parsed through the box", async () => {
  const { token, error } = await tradeWith("credential_expires_at", "2026-12-01T00:00:00Z");
  expect(error).toBeUndefined();
  expect(token?.credentialExpiresAt?.toISOString()).toBe("2026-12-01T00:00:00.000Z");
});
