// packages/envoy-client/src/__tests__/dispatch-write-identity.test.ts
//
// A5 (the review of be17859eca), on the real broker credential: a write presents as one identity.
// Dispatch keys a write per login, the person or the service and never the session
// (`requestKeyLogin`, packages/envoy/internal/dispatch/api/request_keys.go), so a repeat as another
// person or as the machine can run the write a second time. The client's pin is stricter on
// purpose, holding the session too, since it cannot tell whether another session trades as the
// same person: a process that switched sessions between two attempts of one write (a pod's
// session switch, `/clear`) trades under the new session, and once an attempt may have landed the
// write ends unsettled with its key and presents nothing again.

import { expect, test } from "bun:test";
import { brokerCredential } from "../broker-credential";
import {
  DispatchClient,
  DispatchWriteUnsettledError,
  forgetKeyedLayerMemory,
} from "../dispatch-http";
import { brokerStandIns } from "./broker-stand-ins";

const { podEnvironment, servers, stubBroker } = brokerStandIns("dispatch-write-identity");

test("a write whose session switches after an attempt that may have landed ends unsettled and presents no other session's token", async () => {
  forgetKeyedLayerMemory();
  let session = "implementer-g1";
  const asked: { readonly authorization: string; readonly key: string }[] = [];
  const dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const key = request.headers.get("Idempotency-Key") ?? "";
      asked.push({ authorization: request.headers.get("Authorization") ?? "", key });
      if (asked.length === 1) {
        // The host moves to another session while Dispatch answers the first attempt.
        session = "implementer-g2";
        return Response.json(
          { code: "UNAVAILABLE", error: "the database is restarting" },
          { status: 503, headers: { "Idempotency-Key": key } }
        );
      }
      return Response.json(
        { comment: { id: "c-1" } },
        { status: 201, headers: { "Idempotency-Key": key } }
      );
    },
  });
  servers.push(dispatch);
  const broker = stubBroker(3_600);
  const dispatchUrl = `http://127.0.0.1:${dispatch.port}`;
  const credential = brokerCredential(podEnvironment(broker.url, dispatchUrl), () => session);
  const client = new DispatchClient(dispatchUrl, credential, fetch, {
    writeRetry: {
      pausesMs: [10, 10],
      withinMs: 5_000,
      silenceMs: 5_000,
      inFlightPauseMs: 10,
      inFlightWithinMs: 5_000,
      runPauseMs: 10,
      runWithinMs: 5_000,
    },
  });

  const error = await client
    .comment("DSP-1", { body: "Shipped.", actor: { kind: "session", id: "session-1" } })
    .then(
      () => undefined,
      (caught: unknown) => caught
    );

  expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
  expect((error as DispatchWriteUnsettledError).key).toBe(asked[0]?.key as string);
  expect(asked).toHaveLength(1);
  // The broker was asked for the second session's token too; it never reached Dispatch.
  expect(broker.trades).toHaveLength(2);
}, 20_000);
