/** The access tokens e2e/fake-broker.ts answers at the secrets broker's RFC 7662 introspection
 *  route (`POST /v1/oauth/introspect`), which Dispatch asks about every token a caller presents
 *  under the DPoP scheme (packages/envoy/internal/ciphertoken). A real broker's tokens are opaque
 *  and its answer comes from the row each is stored in; a token of the fake carries its answer
 *  itself, as base64url JSON after `fakeBrokerTokenPrefix`, so a caller that holds no address of
 *  the fake - e2e/acceptance/graph-store.sh reaches the harness through the server and the fake
 *  Envoy alone - can still mint a token the fake calls live. The fake verifies no caller, so this
 *  gives a test nothing its fixture routes do not: every token of any other shape is not live. */

/** What a fake token says of itself: the fields of the broker's own answer
 *  (packages/envoy/internal/broker/oauthwire, Introspection) that Dispatch reads. */
export interface FakeIntrospection {
  readonly active: boolean;
  readonly session?: string;
  readonly person?: string;
  readonly service?: string;
  readonly project_scope?: "named" | "person" | "none";
  readonly projects?: readonly string[];
  readonly scope?: string;
  readonly exp?: number;
  readonly client_id?: string;
  readonly jkt?: string;
}

/** What starts every token the fake answers live. */
export const fakeBrokerTokenPrefix = "fake-broker-token.";

/** A token the fake answers `answer` for. */
export function fakeBrokerToken(answer: FakeIntrospection): string {
  return fakeBrokerTokenPrefix + Buffer.from(JSON.stringify(answer)).toString("base64url");
}

/** What the fake answers for `token`: the answer it carries, or not live for a token of any other
 *  shape, as a token the broker never issued is. */
export function fakeBrokerAnswer(token: string): FakeIntrospection {
  if (!token.startsWith(fakeBrokerTokenPrefix)) return { active: false };
  try {
    const answer: unknown = JSON.parse(
      Buffer.from(token.slice(fakeBrokerTokenPrefix.length), "base64url").toString("utf8")
    );
    if (typeof answer === "object" && answer !== null && !Array.isArray(answer)) {
      return answer as FakeIntrospection;
    }
  } catch {
    // A token that does not decode was never one of the fake's.
  }
  return { active: false };
}
