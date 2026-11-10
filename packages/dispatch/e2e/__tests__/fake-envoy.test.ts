import { expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { request as httpRequest } from "node:http";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import type { LegionRole } from "@legion/contracts";

import { type FakeInterest, type FakeSession, legionAgentEntry } from "../agents";
import { withoutHarnessPort } from "../harness-ports";

// The fake Envoy is the browser harness's own fixture (e2e/fake-envoy.ts) and no browser varies
// it, so these tests run here: each starts a fake of its own on a port of its own rather than
// reseeding the listener a browser run shares. With the fake Envoy port unset under every name
// this lane carries it under, the fake binds a port the OS chooses, and the test reads it from
// the fake's listening line. The fake is a `bun` process of its own, as the harness starts it,
// and this file asks with `Bun.fetch`, which is also what the global `fetch` is here: this half
// loads no preload and registers no DOM, so nothing puts happy-dom's `fetch` on globalThis.
const fakeEnvoyModule = fileURLToPath(new URL("../fake-envoy.ts", import.meta.url));

/** A fake Envoy of the test's own, listening, and the way to stop it. */
async function startFakeEnvoy(): Promise<{
  origin: string;
  port: number;
  stop: () => Promise<void>;
}> {
  const env = withoutHarnessPort("fakeEnvoy");
  const fake = spawn("bun", [fakeEnvoyModule], { env, stdio: ["ignore", "pipe", "inherit"] });
  const exited = once(fake, "exit");
  const stop = async () => {
    fake.kill();
    await exited;
  };
  try {
    const ready = Promise.withResolvers<number>();
    let output = "";
    fake.stdout.setEncoding("utf8");
    fake.stdout.on("data", (chunk: string) => {
      output += chunk;
      const listening = /fake envoy listener on 127\.0\.0\.1:(\d+)\n/.exec(output);
      if (listening !== null) ready.resolve(Number(listening[1]));
    });
    fake.once("exit", (code) => {
      ready.reject(new Error(`the fake Envoy exited ${code} before it listened: ${output}`));
    });
    fake.once("error", ready.reject);
    const port = await ready.promise;
    return { origin: `http://127.0.0.1:${port}`, port, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}

/** A fixture write to the fake listening at `origin`. */
function putFixture(origin: string, path: string, body: unknown): Promise<Response> {
  return Bun.fetch(`${origin}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
}

/** One registry entry as the daemon writes it, under a Legion project token of this file's own. */
function entry(session: string, issue: string, role: LegionRole, startedAt: string) {
  return legionAgentEntry({
    issue,
    legionProject: "e2elegion",
    role,
    sessionId: session,
    startedAt,
  });
}

test("an interests seed whose body arrives during a session reseed is kept", async () => {
  const { origin, port, stop } = await startFakeEnvoy();
  try {
    // The interests PUT's headers go first and its body last, with a whole session reseed in
    // between: `conversation.e2e.ts` and `inbox.e2e.ts` send the two seeds at once, and a client
    // that splits a request's headers from its body puts the reseed inside the handler's await.
    const interests: FakeInterest[] = [{ session_id: "e2e-interest", topics: ["dispatch.CORE"] }];
    const body = JSON.stringify(interests);
    const answered = Promise.withResolvers<number>();
    const interestsPut = httpRequest(
      {
        headers: { "Content-Length": Buffer.byteLength(body), "Content-Type": "application/json" },
        host: "127.0.0.1",
        method: "PUT",
        path: "/__fixture/interests",
        port,
      },
      (response) => {
        response.resume();
        response.on("end", () => answered.resolve(response.statusCode ?? 0));
      }
    ).on("error", answered.reject);
    interestsPut.flushHeaders();
    // Long enough for the fixture to have entered the handler and be waiting on the body. A
    // reseed that lands before the handler starts cannot show the loss, so too short a wait
    // passes a broken fixture, never fails a sound one.
    await sleep(100);
    const sessions: FakeSession[] = [{ session_id: "e2e-reseeded", title: "Reseeded" }];
    const reseed = await putFixture(origin, "/__fixture/sessions", sessions);
    expect(reseed.status).toBe(200);
    interestsPut.end(body);
    expect(await answered.promise).toBe(200);

    const stored = await Bun.fetch(`${origin}/v1/interests/`);
    expect(await stored.json()).toEqual(interests);
  } finally {
    await stop();
  }
});

// The registry reads answer from the seeded entries as the listener's do: an issue's sessions by
// role in workflow order and each role token's newest first, `replaced_by`/`replaced_at` from the
// next session of the token to start, and `live` from the state the session's own record puts it
// in (`sessionIsLive` through `session.StateOf`), never from this listener's session cache - so a
// session the cache holds whose record is unresponsive reads `live: false` while it is not gone.
// A session reseed keeps the entries, as the real bucket outlives a session ending.
test("the Legion agent registry reads derive live and replacement, and outlive a session reseed", async () => {
  const { origin, stop } = await startFakeEnvoy();
  try {
    const seeded = [
      entry("new-reviewer", "E2E-1", "reviewer", "2026-10-26T02:00:00Z"),
      entry("old-reviewer", "E2E-1", "reviewer", "2026-10-26T01:00:00Z"),
      entry("implementer", "E2E-1", "implementer", "2026-10-26T00:00:00Z"),
      entry("stale-tester", "E2E-1", "tester", "2026-10-26T00:30:00Z"),
      entry("elsewhere", "E2E-2", "tester", "2026-10-26T00:00:00Z"),
    ];
    expect((await putFixture(origin, "/__fixture/legion-agents", seeded)).status).toBe(200);
    // The reseed puts both sessions in the listener's cache. Only the session's own record
    // decides `live`, as `sessionIsLive` judges it through `session.StateOf`, so the tester is
    // moved past the live window: unresponsive is not gone, and it keeps its registry entry, its
    // role token and its place in the list while reading `live: false`.
    const sessions: FakeSession[] = [
      { session_id: "new-reviewer", title: "Legion reviewer" },
      { session_id: "stale-tester", title: "Legion tester" },
    ];
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);
    const unresponsive = await Bun.fetch(`${origin}/__fixture/sessions/stale-tester`, {
      body: JSON.stringify({ state: "unresponsive" }),
      headers: { "Content-Type": "application/json" },
      method: "PATCH",
    });
    expect(unresponsive.status).toBe(200);

    const issue = await Bun.fetch(`${origin}/v1/legion/agents?issue=E2E-1`);
    expect(issue.status).toBe(200);
    expect(await issue.json()).toMatchObject({
      entries: [
        { live: false, replaced_at: null, replaced_by: null, session_id: "implementer" },
        { live: false, replaced_at: null, replaced_by: null, session_id: "stale-tester" },
        { live: true, replaced_at: null, replaced_by: null, session_id: "new-reviewer" },
        {
          live: false,
          replaced_at: "2026-10-26T02:00:00Z",
          replaced_by: "new-reviewer",
          session_id: "old-reviewer",
        },
      ],
    });

    const role = await Bun.fetch(
      `${origin}/v1/legion/agents/roles/legion-e2elegion-e2e-1-reviewer`
    );
    expect(await role.json()).toMatchObject({
      entries: [{ session_id: "new-reviewer" }, { session_id: "old-reviewer" }],
    });
    const none = await Bun.fetch(`${origin}/v1/legion/agents/roles/legion-e2elegion-e2e-1-merger`);
    expect(await none.json()).toEqual({ entries: [] });

    const old = await Bun.fetch(`${origin}/v1/legion/agents/sessions/old-reviewer`);
    expect(old.status).toBe(200);
    expect(await old.json()).toMatchObject({
      issue: "E2E-1",
      live: false,
      replaced_by: "new-reviewer",
      role_token: "legion-e2elegion-e2e-1-reviewer",
    });
    const unknown = await Bun.fetch(`${origin}/v1/legion/agents/sessions/nobody`);
    expect(unknown.status).toBe(404);
    expect(await unknown.json()).toEqual({ error: "unknown legion agent nobody" });
  } finally {
    await stop();
  }
});

// The registry is a bucket of the listener's own, and a listener that cannot read it answers its
// three reads 503 while every other route serves (packages/envoy/cmd/listener/legion_agents.go,
// `legionAgentRegistry`); Dispatch turns any of those into `legion_registry: "unavailable"`. The
// fixture's registry state is that listener, and the entries it was holding come back untouched
// when the state goes back to `ok` - without which `collaborators.e2e.ts` could not tell the
// notice from a fixture that had simply lost its entries.
test("a registry state other than ok refuses all three registry reads, and ok restores the entries", async () => {
  const { origin, stop } = await startFakeEnvoy();
  const reads = () =>
    Promise.all([
      Bun.fetch(`${origin}/v1/legion/agents?issue=E2E-1`),
      Bun.fetch(`${origin}/v1/legion/agents/roles/legion-e2elegion-e2e-1-reviewer`),
      Bun.fetch(`${origin}/v1/legion/agents/sessions/reviewer`),
    ]);
  try {
    const seeded = [entry("reviewer", "E2E-1", "reviewer", "2026-10-26T01:00:00Z")];
    expect((await putFixture(origin, "/__fixture/legion-agents", seeded)).status).toBe(200);
    const sessions: FakeSession[] = [{ session_id: "reviewer", title: "Legion reviewer" }];
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);

    // Both refusals the real listener gives: the bucket that never opened, which carries no
    // Retry-After, and the read the open bucket did not answer, which carries `Retry-After: 1`.
    for (const state of ["unopened", "unreadable"] as const) {
      expect((await putFixture(origin, "/__fixture/legion-registry", { state })).status).toBe(200);
      const refused = await reads();
      for (const answer of refused) {
        expect(answer.status).toBe(503);
        expect(await answer.json()).toEqual({
          error: expect.stringContaining("legion agent registry"),
        });
      }
      expect(refused.map((answer) => answer.headers.get("Retry-After"))).toEqual(
        state === "unopened" ? [null, null, null] : ["1", "1", "1"]
      );
    }

    // Only the registry: the session list is the listener's other bucket and still answers, which
    // is why this state is not the `lists` one.
    const live = await Bun.fetch(`${origin}/v1/sessions`);
    expect(live.status).toBe(200);
    expect(await live.json()).toMatchObject([{ session_id: "reviewer" }]);
    // And it outlives a session reseed, as the entries do.
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);
    expect((await reads())[0].status).toBe(503);

    expect((await putFixture(origin, "/__fixture/legion-registry", { state: "ok" })).status).toBe(
      200
    );
    const [issue, role, session] = await reads();
    expect(await issue.json()).toMatchObject({ entries: [{ live: true, session_id: "reviewer" }] });
    expect(await role.json()).toMatchObject({ entries: [{ session_id: "reviewer" }] });
    expect(await session.json()).toMatchObject({ issue: "E2E-1", session_id: "reviewer" });

    const unknown = await putFixture(origin, "/__fixture/legion-registry", { state: "sideways" });
    expect(unknown.status).toBe(400);
    expect(await unknown.json()).toEqual({ error: "state must be ok, unopened or unreadable" });
  } finally {
    await stop();
  }
});
