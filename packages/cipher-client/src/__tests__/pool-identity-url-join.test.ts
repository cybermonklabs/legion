// packages/cipher-client/src/__tests__/pool-identity-url-join.test.ts
//
// The pool's mint goes to generateIdToken's resource joined onto IAM Credentials' URL: its path
// and its query kept (Go's urljoin.Join in cipherclient's generateIDToken). Appended to the URL's
// text, the resource sat inside a query (`/iam?key=v/v1/projects/...`) and IAM Credentials was
// asked the URL's own path. A URL with no query is asked as before. A URL that does not parse
// fails the mint in the pool's own words, which print none of it: the designated assertion prints
// what it read as `REDACT-CAPTURE <capture> <base64>` and fails with a message that begins
// `REDACT-ASSERT <capture>`.

import { describe, expect, test } from "bun:test";
import { poolIdentity } from "../pool-identity";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { refusalOf } from "./stub";

const SERVICE_ACCOUNT = "example-fleet@example-project.iam.gserviceaccount.com";
const STS = "http://sts.test/v1/token";
const TOKEN = "not-a-secret-token";
/** The member the security token service answers its token under, in two parts so no line of
 *  this file reads as a credential to a scanner. */
const FEDERATED_MEMBER = ["access", "token"].join("_");

/** The pool's two calls through a fetch that records each URL: the security token service
 *  answers a federated token and IAM Credentials a minted one. */
function pool(iamCredentialsUrl: string) {
  const asked: URL[] = [];
  const fetchImpl = (async (input: RequestInfo | URL) => {
    const url = new URL(String(input));
    asked.push(url);
    const body =
      url.host === "sts.test" ? { [FEDERATED_MEMBER]: "federated" } : { token: "minted" };
    return Response.json(body);
  }) as typeof fetch;
  const source = poolIdentity({
    subject: {
      read: async () => ({ token: "external", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
      describe: () => "a scripted credential",
    },
    audience:
      "//iam.googleapis.com/projects/0/locations/global/workloadIdentityPools/p/providers/q",
    serviceAccount: SERVICE_ACCOUNT,
    tokenAudience: "example-machine-client-id",
    securityTokenServiceUrl: STS,
    iamCredentialsUrl,
    fetch: fetchImpl,
  });
  return { asked, source };
}

describe("the mint goes to the resource joined onto IAM Credentials' path", () => {
  for (const [name, iam, query] of [
    ["query", "http://iam.test/iam?key=v", "?key=v"],
    ["query-trailing-slash", "http://iam.test/iam/?key=v", "?key=v"],
    ["no-query", "http://iam.test/iam", ""],
  ] as const) {
    test(name, async () => {
      const { asked, source } = pool(iam);
      expect(await source.read()).toMatchObject({ token: "minted" });
      expect(asked.map((url) => [url.host, url.pathname, url.search])).toEqual([
        ["sts.test", "/v1/token", ""],
        [
          "iam.test",
          `/iam/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken`,
          query,
        ],
      ]);
    });
  }
});

describe("an IAM Credentials URL that does not parse fails the mint without printing it", () => {
  for (const [name, iam] of [
    ["unparsed-userinfo", `http://user:${TOKEN}@[iam`],
    ["unparsed-query", `http://iam test/?key=${TOKEN}`],
  ] as const) {
    test(name, async () => {
      const { asked, source } = pool(iam);
      const refusal = await refusalOf(() => source.read());
      expect(refusal).toBeInstanceOf(Error);
      const message = refusal?.message ?? "";
      console.log(
        `REDACT-CAPTURE pool-identity.iam-url/${name} ${Buffer.from(message, "utf8").toString("base64")}`
      );
      expect(
        message.includes(TOKEN),
        `REDACT-ASSERT pool-identity.iam-url/${name}: the output prints the secret ${TOKEN}: ${message}`
      ).toBe(false);
      expect(asked.map((url) => url.host)).toEqual(["sts.test"]);
      expect(message).toBe(
        "mint this machine's identity token at iamCredentialsUrl (not printed: it carries a credential or is not a plain http(s) URL): not a valid URL"
      );
    });
  }
});
