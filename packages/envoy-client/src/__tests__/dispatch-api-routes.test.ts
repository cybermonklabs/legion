// packages/envoy-client/src/__tests__/dispatch-api-routes.test.ts
//
// The one guard of the table of write routes (`WRITE_ROUTES`, src/dispatch-commands/api.ts), from
// which `dispatch api`'s AskGate refusal and `dispatch proxy`'s 405 name the `dispatch` command that
// makes a write. It drives every method of DispatchClient through a recording fetch and holds the
// table to the writes they send: each write route has a row, and each row is a route some method
// sends. A write method added to DispatchClient later fails here until its route has a row, or it
// joins the methods no `dispatch` command calls, below, with its reason.

import { expect, test } from "bun:test";
import { NO_ROUTE_MESSAGE_PREFIX } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { bearerCredential } from "../bearer";
import { WRITE_ROUTES, writeRoute } from "../dispatch-commands/api";
import { DispatchClient } from "../dispatch-http";
import { CALLS, type ClientMethod, clientMethodNames } from "./dispatch-client-calls";

/** The write methods with no row, each with why: no `dispatch` command calls it, or, for
 *  `sendRoute`, the route it sends is the one its caller names. */
const NO_COMMAND: Readonly<Partial<Record<ClientMethod, string>>> = {
  deliveryReply: "the plugins' answer to a delivery they took (src/delivery.ts)",
  acceptMessageDelivery:
    "Oh My Pi's acceptance of a person's direct message as its user's turn (pi-shared's accept-user-turn.ts)",
  nameArtifactVersion: "no caller outside DispatchClient's own tests",
  sendRoute: "dispatch api's write of the route its caller names (src/dispatch-commands/api.ts)",
};


/** The route a Dispatch older than it has none for, which `syncArchitectureSources` then sends
 *  its older route in place of. */
const SOURCES_SYNC = "/api/v1/projects/DSP/architecture-sources/sync";

test("every write DispatchClient sends has a row in dispatch api's table", async () => {
  const methods = clientMethodNames();
  expect(methods.sort()).toEqual(Object.keys(CALLS).sort());

  const rowless: string[] = [];
  const reached = new Set<string>();
  for (const name of methods as ClientMethod[]) {
    const writes: { readonly method: string; readonly path: string }[] = [];
    const recording = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const { pathname } = new URL(String(input));
      const method = init?.method ?? "GET";
      // A read is answered with an empty object: each method reads nothing it needs from it.
      if (method === "GET") return Response.json({});
      writes.push({ method, path: pathname });
      // Dispatch's no-route answer, so the sync of a project's sources sends its older route too.
      if (method === "POST" && pathname === SOURCES_SYNC) {
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `${NO_ROUTE_MESSAGE_PREFIX}POST ${SOURCES_SYNC}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 }
        );
      }
      return Response.json({});
    }) as typeof fetch;
    // A sleep that waits for nothing, so a write's no-route retries end at once.
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential(fixtureSecret("dsp")),
      recording,
      { sleep: async () => {} }
    );
    await CALLS[name](client).catch(() => undefined);

    if (NO_COMMAND[name] !== undefined) {
      // Still a write method, or its place in the list is stale.
      expect({ name, writes: writes.length > 0 }).toEqual({ name, writes: true });
      continue;
    }
    for (const write of writes) {
      const row = writeRoute(write.method, write.path);
      if (row === undefined) rowless.push(`${name}: ${write.method} ${write.path}`);
      else reached.add(`${row.method} ${row.pattern}`);
    }
  }
  expect(rowless).toEqual([]);
  // Each row is a route some method sends: a row no method sends, or a method the walk no longer
  // reaches, shows here.
  expect([...reached].sort()).toEqual(
    WRITE_ROUTES.map((row) => `${row.method} ${row.pattern}`).sort()
  );
});
