// packages/envoy-client/src/__tests__/dispatch-dead-token.test.ts
//
// T3 (the review of 34f3f6ae57), the Dispatch client's half of red team 3, on the real broker
// credential: a read or a write whose token Dispatch answers `401 BROKER_TOKEN_NOT_ACTIVE` names
// that presentation to the credential (`RequestCredential.stale(session, presented)`), so the
// credential drops that dead token alone. The host moving to another session while the answer is
// on its way leaves the other session's live token in place: it is presented as it is, and the
// dead session trades afresh the next time it presents. Before, the client told the credential
// nothing of the presentation, so it dropped the session the host was on at that moment: a live
// token traded again for nothing, and the dead one kept for the next call.

import { expect, test } from "bun:test";
import { routeAnswer } from "../../test/route-answers";
import { brokerCredential } from "../broker-credential";
import { DispatchClient, forgetKeyedLayerMemory } from "../dispatch-http";
import { brokerStandIns } from "./broker-stand-ins";

const { podEnvironment, servers, stubBroker } = brokerStandIns("dispatch-dead-token");

/** What Dispatch answers a token its broker no longer holds live (`brokerTokenActor`). */
const DEAD = {
  code: "BROKER_TOKEN_NOT_ACTIVE",
  error: "the secrets broker does not hold this access token as live",
};

/**
 * A Dispatch whose first request's token it calls dead, and which moves the host to `then` while
 * that answer is on its way; a request presenting that token again is dead too, and any other is
 * answered `answer` (the key echoed back where the request carried one).
 */
function deadFirst(moveTo: () => void, answer: (key: string) => Response) {
  const presented: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const token = (request.headers.get("Authorization") ?? "").replace(/^DPoP /, "");
      presented.push(token);
      if (presented.length === 1) {
        moveTo();
        return Response.json(DEAD, { status: 401 });
      }
      if (token === presented[0]) return Response.json(DEAD, { status: 401 });
      return answer(request.headers.get("Idempotency-Key") ?? "");
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, presented };
}

test.each([
  [
    "a read",
    (client: DispatchClient) => client.whoami(),
    () => Response.json({ login: "ses-one", kind: "session" }),
  ],
  [
    "a write",
    (client: DispatchClient) =>
      client.comment("DSP-1", { body: "Shipped.", actor: { kind: "session", id: "ses-one" } }),
    // The comment as its route answers it, the row the write reads (`dispatch-write-rows.ts`).
    (key: string) =>
      Response.json(
        routeAnswer("POST /api/v1/issues/{key}/comments", {
          id: "c-1",
          issue_key: "DSP-1",
          body: "Shipped.",
        }),
        { status: 201, headers: key === "" ? {} : { "Idempotency-Key": key } }
      ),
  ],
] as const)("%s whose token is dead while the host moves to a warm session drops the dead token alone", async (_, call, answer) => {
  forgetKeyedLayerMemory();
  const broker = stubBroker(900);
  let session = "ses-two";
  const dispatch = deadFirst(() => {
    session = "ses-two";
  }, answer);
  const credential = brokerCredential(podEnvironment(broker.url, dispatch.url), () => session);
  const url = `${dispatch.url}/api/v1/whoami`;
  // The session the host moves to already holds a live token.
  const warm = (await credential.headers("GET", url)).Authorization;
  session = "ses-one";

  await call(new DispatchClient(dispatch.url, credential, fetch, { deadlineMs: 10_000 }));

  // ses-one's token went out first and was dead; the resend went as ses-two, with its live token,
  // which no trade replaced.
  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual(["ses-two", "ses-one"]);
  expect(dispatch.presented).toEqual([broker.trades[1]?.token, broker.trades[0]?.token]);
  expect((await credential.headers("GET", url)).Authorization).toBe(warm);
  expect(broker.trades).toHaveLength(2);
  // The dead session trades afresh the next time it presents, rather than present its dead token.
  session = "ses-one";
  expect((await credential.headers("GET", url)).Authorization).toBe(
    `DPoP ${broker.trades[2]?.token}`
  );
  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    "ses-two",
    "ses-one",
    "ses-one",
  ]);
});
