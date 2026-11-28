// packages/cipher-client/src/__tests__/stub.ts
//
// The stub broker the renewal and environment suites trade against, the broker's own answer
// through a key set that cannot be read, and the refusal a suite reads out of a call it expects to
// fail. The stub issuer a person's sign-in is renewed at is in `test/sign-ins.ts`, which
// envoy-client's suites share. The wire suite keeps a recorder of its own, since what it asserts
// is the raw shape of a request rather than the behaviour around it.

import { HEADER_DPOP_HOLDER } from "../wire";

/** One trade as the stub saw it: its form, its DPoP proof, and its holder proof (`DPoP-Holder`),
 *  whose parts are empty where the trade sent none. */
export interface StubTrade {
  readonly form: URLSearchParams;
  readonly proofHeader: Record<string, unknown>;
  readonly proofClaims: Record<string, unknown>;
  readonly holderHeader: Record<string, unknown>;
  readonly holderClaims: Record<string, unknown>;
}

/** What the stub answers one trade with. */
export interface StubAnswer {
  readonly status?: number;
  readonly body: Record<string, unknown>;
  /** A `DPoP-Nonce` to send with the answer, which the next proof must carry. */
  readonly nonce?: string;
  /** Headers to send with the answer, such as a 503's `Retry-After`. */
  readonly headers?: Readonly<Record<string, string>>;
}

/** A broker that answers `answer` to each trade in turn, keeping every trade it was sent. An
 *  answer that is a promise holds the trade until it settles, as a broker that is slow to answer
 *  does. */
export function stubBroker(
  answer: (trade: StubTrade, count: number) => StubAnswer | Promise<StubAnswer>
): {
  readonly url: string;
  readonly trades: StubTrade[];
  stop(): void;
} {
  const trades: StubTrade[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const read = (proof: string, at: number): Record<string, unknown> => {
        const parts = proof.split(".");
        return parts.length < 3
          ? {}
          : (JSON.parse(Buffer.from(parts[at] as string, "base64url").toString("utf8")) as Record<
              string,
              unknown
            >);
      };
      const proof = request.headers.get("DPoP") ?? "";
      const holder = request.headers.get(HEADER_DPOP_HOLDER) ?? "";
      const trade: StubTrade = {
        form: new URLSearchParams(await request.text()),
        proofHeader: read(proof, 0),
        proofClaims: read(proof, 1),
        holderHeader: read(holder, 0),
        holderClaims: read(holder, 1),
      };
      trades.push(trade);
      const given = await answer(trade, trades.length);
      return Response.json(given.body, {
        status: given.status ?? 200,
        headers: {
          ...given.headers,
          ...(given.nonce === undefined ? {} : { "DPoP-Nonce": given.nonce }),
        },
      });
    },
  });
  return { url: `http://127.0.0.1:${server.port}`, trades, stop: () => server.stop(true) };
}

/** An accepted answer carrying `accessToken`, good for `seconds`. */
export function accepted(accessToken: string, seconds: number): Record<string, unknown> {
  return {
    access_token: accessToken,
    issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
    token_type: "DPoP",
    expires_in: seconds,
    credential_id: "cred-1",
  };
}

/** The broker's answers through a key set that cannot be read, captured from its real HTTP
 *  server, read by relative path, never a copy: internal/cipher/api's
 *  `TestTheUnreadableKeySetAnswersAreTheFixture` writes the file from that server and fails
 *  wherever the server answers otherwise, and the Go clients' suites replay the same file. */
const UNREADABLE_KEY_SET_ANSWERS = new URL(
  "../../../envoy/internal/cipher/api/testdata/unreadable-key-set-answers.json",
  import.meta.url
);

/** One answer of that file: its status, its `Retry-After` and `Content-Type`, and its body. */
interface CapturedAnswer {
  readonly status: number;
  readonly retry_after: string;
  readonly content_type: string;
  readonly body: Record<string, unknown>;
}

/**
 * The token endpoint's answer to a trade whose subject or actor token the broker could not check,
 * because the key set of that token's issuer could not be read, as its real server wrote it: 503,
 * OAuth's error object with the code ISSUER_UNAVAILABLE, and `Retry-After: 5`, the default
 * spacing of a failing key set's reads, which the rows about that wait take as it is and the rows
 * about another wait replace ({@link asking}). A file whose answer is not that refuses every row.
 */
export async function keySetOutage(): Promise<StubAnswer> {
  const answers = (await Bun.file(UNREADABLE_KEY_SET_ANSWERS).json()) as {
    readonly token_endpoint: CapturedAnswer;
  };
  const captured = answers.token_endpoint;
  if (captured.status !== 503 || captured.retry_after !== "5") {
    throw new Error(
      `the broker's token endpoint answer through an unreadable key set is ${captured.status} with Retry-After ${JSON.stringify(captured.retry_after)}; these rows read it as 503 asking for 5 s`
    );
  }
  return {
    status: captured.status,
    body: captured.body,
    headers: { "Content-Type": captured.content_type, "Retry-After": captured.retry_after },
  };
}

/** `answer` asking for a wait of `seconds` with its `Retry-After` instead of its own. */
export function asking(answer: StubAnswer, seconds: string): StubAnswer {
  return { ...answer, headers: { ...answer.headers, "Retry-After": seconds } };
}

/** The error `run` rejects with, or null where it resolves. */
export async function refusalOf(run: () => Promise<unknown>): Promise<Error | null> {
  return await run().then(
    () => null,
    (error: unknown) => error as Error
  );
}
