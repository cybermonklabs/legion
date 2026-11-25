/** The credential checks the outside driver's edge stand-ins make, as the edge listener and the
 *  broker's edge make them, so a driver that drops its credential, or names a topic of no project
 *  of its token, fails a row rather than passing against a stand-in that takes anything:
 *  - a listener route (`POST /v1/interests/subscribe`, `POST /v1/messages/send`,
 *    `DELETE /v1/sessions/<id>`) takes only `Authorization: DPoP <token>` with a token the relay
 *    socket stand-in's broker minted for the session the request acts as, live, beside a proof of
 *    that token's key naming the request's method, its URL and the token (`sessionRefusal`);
 *  - a registration names no topic but the session's own agent subject, which is every topic the
 *    driver's session follows (`topicRefusal`), and a refusal names the topics as an edge listener
 *    does, so the client's project-refusal path reads it;
 *  - the broker's token exchange takes only a subject token that the stand-in's own device grant
 *    issued (`tradeRefusal`).
 *  The relay socket stand-in checks the upgrade's token and proof itself
 *  (`@legion/envoy-client/test/relay-socket-stand-in`). */

import { createHash, createPublicKey, type JsonWebKeyInput, verify } from "node:crypto";
import { thumbprintOf } from "@legion/cipher-client/test/sign-ins";
import { legacyAgentSubject } from "@legion/contracts";
import type { RelaySocketStandIn } from "@legion/envoy-client/test/relay-socket-stand-in";
import { expandSubscriptionTopics } from "@legion/envoy-client/transport";

/** A proof's header key and its claims, verified with that key; undefined where the proof is no
 *  compact JWS whose signature verifies. */
function verifiedProof(proof: string):
  | {
      readonly jwk: JsonWebKeyInput["key"];
      readonly claims: Readonly<Record<string, unknown>>;
    }
  | undefined {
  const [header = "", claims = "", signature = ""] = proof.split(".");
  try {
    const read = (part: string): Readonly<Record<string, unknown>> =>
      JSON.parse(Buffer.from(part, "base64url").toString("utf8"));
    const jwk = read(header).jwk as JsonWebKeyInput["key"];
    const key = createPublicKey({ key: jwk, format: "jwk" });
    const signed = Buffer.from(`${header}.${claims}`);
    const raw = Buffer.from(signature, "base64url");
    if (!verify("sha256", signed, { key, dsaEncoding: "ieee-p1363" }, raw)) return undefined;
    return { jwk, claims: read(claims) };
  } catch {
    return undefined;
  }
}

/** The edge listener's refusal of `request`, a listener route acting as `session`, where it does
 *  not carry that session's own live token and a proof of it naming the request: 401, as the
 *  listener answers a request it cannot tie to the session; undefined where it may go on. */
export async function sessionRefusal(
  request: Request,
  relay: RelaySocketStandIn,
  session: string
): Promise<Response | undefined> {
  const refuse = (why: string) =>
    Response.json({ error: why, code: "unauthorized" }, { status: 401 });
  const authorization = request.headers.get("Authorization") ?? "";
  const token = authorization.startsWith("DPoP ") ? authorization.slice("DPoP ".length) : "";
  const minted = relay.minted.find((one) => one.value === token);
  if (minted === undefined) return refuse("the request carries no token this broker minted");
  if (minted.expiresAt <= Date.now()) return refuse("the request's token has lapsed");
  if (minted.session !== session) {
    return refuse(`the request's token names ${minted.session}, not ${session}`);
  }
  const proof = verifiedProof(request.headers.get("DPoP") ?? "");
  const url = new URL(request.url);
  if (
    proof === undefined ||
    (await thumbprintOf(proof.jwk as Parameters<typeof thumbprintOf>[0])) !== minted.jkt ||
    proof.claims.htm !== request.method ||
    proof.claims.htu !== `${url.origin}${url.pathname}` ||
    proof.claims.ath !== createHash("sha256").update(token).digest("base64url")
  ) {
    return refuse("the request's proof does not name this request and its token");
  }
  return undefined;
}

/** The edge listener's refusal of a registration of `session` naming `topics`, any of which is not
 *  the session's own agent subject (in any wire form the client sends it in): 403
 *  `topic_not_in_project`, naming them, as a listener refuses a topic of no project of the
 *  session's token; undefined where every topic is the session's own. */
export function topicRefusal(session: string, topics: unknown): Response | undefined {
  const own = new Set([
    legacyAgentSubject(session),
    ...expandSubscriptionTopics([legacyAgentSubject(session)]),
  ]);
  const named = Array.isArray(topics) ? topics.map(String) : [];
  const refused = named.filter((topic) => !own.has(topic));
  if (refused.length === 0) return undefined;
  return Response.json(
    {
      error: `topics of no project of the session's token: ${refused.join(", ")}`,
      code: "forbidden",
      reason: "topic_not_in_project",
      refused,
    },
    { status: 403 }
  );
}

/** The broker's refusal of a token exchange whose subject token is no access token its device
 *  grant issued (`issued`): 400 `invalid_grant`; undefined where it may go on. */
export function tradeRefusal(
  form: URLSearchParams,
  issued: ReadonlySet<string>
): Response | undefined {
  if (issued.has(form.get("subject_token") ?? "")) return undefined;
  return Response.json(
    {
      error: "invalid_grant",
      error_description: "the subject token is no device login this broker issued",
    },
    { status: 400 }
  );
}
