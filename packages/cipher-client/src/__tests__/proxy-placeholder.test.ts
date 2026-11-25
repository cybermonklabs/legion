// packages/cipher-client/src/__tests__/proxy-placeholder.test.ts
//
// The trade of a person's sign-in for a placeholder of Cipher's proxy (`proxyPlaceholder`): the
// one exchange that asks for no access token, whose answer is a placeholder the broker's proxy
// swaps a token of its own for, and how the session reaches that proxy. The client reads that
// answer strictly and never takes anything else for a placeholder.

import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { newDpopKey } from "../dpop";
import type { SubjectToken, TokenSource } from "../source";
import { BrokerTokenClient } from "../token-client";
import { BrokerTradeFailedError } from "../token-errors";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  FORM_REQUESTED_TOKEN_TYPE,
  GRANT_TYPE_TOKEN_EXCHANGE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_PERSON,
  TOKEN_TYPE_PROXY_PLACEHOLDER,
} from "../wire";
import { refusalOf, stubBroker } from "./stub";

const PERSON = "ada@example.com";
const PLACEHOLDER = `agent-secrets-placeholder-approve-${"A".repeat(32)}`;
const AUTHORITY = "-----BEGIN CERTIFICATE-----\nproxy\n-----END CERTIFICATE-----\n";

/** A source answering `token` under `tokenType` at every read. */
function fixed(token: string, tokenType: string, person?: string): TokenSource {
  return {
    read: async (): Promise<SubjectToken> => ({ token, tokenType }),
    describe: () => `a scripted ${tokenType}`,
    ...(person === undefined ? {} : { person }),
  };
}

/** The broker's answer to a placeholder trade, with `change` applied to it. */
function placeholderAnswer(
  change: (answer: Record<string, unknown>) => void = () => {}
): Record<string, unknown> {
  const answer: Record<string, unknown> = {
    access_token: PLACEHOLDER,
    issued_token_type: TOKEN_TYPE_PROXY_PLACEHOLDER,
    token_type: "N_A",
    expires_in: 900,
    grant_id: "approve:5f070414-1a7c-43a3-8818-cf76eded9806",
    proxy: {
      proxy_url: "http://proxy.example:3128",
      ca_certificate: AUTHORITY,
      proxy_password: PLACEHOLDER,
      names: {
        DISPATCH_APPROVE_TOKEN: {
          value: PLACEHOLDER,
          slots: { value: PLACEHOLDER },
          hosts: ["dispatch.example"],
        },
      },
    },
  };
  change(answer);
  return answer;
}

/** A person's client of the broker at `url`: their identity token as the subject, the machine's as
 *  the actor, under `session`. */
async function personClient(url: string, session = "ada-box-1"): Promise<BrokerTokenClient> {
  return new BrokerTokenClient({
    brokerUrl: url,
    key: await newDpopKey(),
    subject: fixed(fixtureSecret("person"), SUBJECT_TOKEN_TYPE_PERSON, PERSON),
    actor: fixed(fixtureSecret("machine"), SUBJECT_TOKEN_TYPE_GOOGLE),
    session,
    log: () => {},
  });
}

// The trade goes through openid-client, as every trade of this client does, so this answer is also
// the library's reading of a token endpoint answer whose token_type is N_A (RFC 8693 Section
// 2.2.1, the type of a token that is no access token).
test("a sign-in is traded for the proxy placeholder and how the session reaches the proxy", async () => {
  const broker = stubBroker(() => ({ body: placeholderAnswer() }));
  try {
    const client = await personClient(broker.url);
    const held = await client.proxyPlaceholder();
    expect(held).toEqual({
      placeholder: PLACEHOLDER,
      proxyUrl: "http://proxy.example:3128",
      caCertificate: AUTHORITY,
      names: { DISPATCH_APPROVE_TOKEN: { hosts: ["dispatch.example"] } },
    });

    expect(broker.trades).toHaveLength(1);
    const form = broker.trades[0]?.form;
    expect(form?.get(FORM_REQUESTED_TOKEN_TYPE)).toBe(TOKEN_TYPE_PROXY_PLACEHOLDER);
    expect(form?.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
    expect(form?.get("actor_token_type")).toBe(ACTOR_TOKEN_TYPE_MACHINE);
    expect(form?.get("session")).toBe("ada-box-1");
    expect(form?.get("grant_type")).toBe(GRANT_TYPE_TOKEN_EXCHANGE);
    expect(broker.trades[0]?.proofClaims["htm"]).toBe("POST");
  } finally {
    broker.stop();
  }
});

test("an answer that is not a proxy placeholder is refused and never handed out", async () => {
  const rows: Record<string, (answer: Record<string, unknown>) => void> = {
    "an access token's type": (answer) => {
      answer["issued_token_type"] = "urn:ietf:params:oauth:token-type:access_token";
    },
    "a DPoP token_type": (answer) => {
      answer["token_type"] = "DPoP";
    },
    "no expires_in": (answer) => {
      delete answer["expires_in"];
    },
    "a placeholder without the placeholder prefix": (answer) => {
      answer["access_token"] = "a-real-looking-token";
    },
    "a refresh token": (answer) => {
      answer["refresh_token"] = "never-issued";
    },
    "no grant id": (answer) => {
      delete answer["grant_id"];
    },
    "no proxy": (answer) => {
      delete answer["proxy"];
    },
    "a proxy password other than the placeholder": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["proxy_password"] = "another";
    },
    "a proxy URL that is no http URL": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["proxy_url"] = "ftp://proxy.example";
    },
    "no authority": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["ca_certificate"] = "";
    },
    "a name with no host": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["names"] = {
        DISPATCH_APPROVE_TOKEN: { value: PLACEHOLDER, hosts: [] },
      };
    },
    "a name whose value is not the placeholder": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["names"] = {
        DISPATCH_APPROVE_TOKEN: { value: "another", hosts: ["dispatch.example"] },
      };
    },
  };
  for (const [row, change] of Object.entries(rows)) {
    const broker = stubBroker(() => ({ body: placeholderAnswer(change) }));
    try {
      const refusal = await refusalOf(
        async () => await (await personClient(broker.url)).proxyPlaceholder()
      );
      if (!(refusal instanceof BrokerTradeFailedError)) {
        throw new Error(`${row}: answered ${String(refusal)}; want BrokerTradeFailedError`);
      }
      expect(refusal.message).not.toContain(PLACEHOLDER);
    } finally {
      broker.stop();
    }
  }
});

test("a broker's refusal of the placeholder trade is thrown as the trade's refusal", async () => {
  const broker = stubBroker(() => ({
    status: 400,
    body: {
      error: "invalid_request",
      error_description:
        "Only a person's sign-in is traded for the approve capability's proxy placeholder, on a broker that serves it.",
    },
  }));
  try {
    const refusal = await refusalOf(
      async () => await (await personClient(broker.url)).proxyPlaceholder()
    );
    expect(refusal).not.toBeNull();
    expect(String(refusal)).toContain("invalid_request");
  } finally {
    broker.stop();
  }
});
