import { expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { request as httpRequest } from "node:http";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { type LegionRole, roleToken } from "@legion/contracts";

import { type FakeInterest, type FakeSession, legionAgentEntry } from "../agents";
import { withoutHarnessPort } from "../harness-ports";

// The fake Envoy is the browser harness's own fixture (e2e/fake-envoy.ts) and no browser varies
// it, so these tests run here: each starts a fake of its own on a port of its own rather than
// reseeding the listener a browser run shares. With the fake Envoy port unset under every name
// this stack carries it under, the fake binds a port the OS chooses, and the test reads it from
// the fake's listening line. The fake is a `bun` process of its own, as the harness starts it,
// and this file asks with `Bun.fetch`, which is also what the global `fetch` is here: this half
// loads no preload and registers no DOM, so nothing puts happy-dom's `fetch` on globalThis.
const fakeEnvoyModule = fileURLToPath(new URL("../fake-envoy.ts", import.meta.url));

/** A fake Envoy of the test's own, listening, and the way to stop it. It is started with no bus,
 *  whatever the shell running the tests exported, so no claim a test asks for reaches a NATS
 *  server another stack's Dispatch reads. */
async function startFakeEnvoy(): Promise<{
  origin: string;
  port: number;
  stop: () => Promise<void>;
}> {
  const env = withoutHarnessPort("fakeEnvoy");
  delete env.DISPATCH_E2E_NATS_URLS;
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

/** A fixture write to the fake listening at `origin`: a PUT, or the PATCH that changes one seeded
 *  session (`/__fixture/sessions/<id>`). */
function putFixture(
  origin: string,
  path: string,
  body: unknown,
  method: "PUT" | "PATCH" = "PUT"
): Promise<Response> {
  return Bun.fetch(`${origin}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method,
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
    expect(await none.json()).toEqual({ answering: false, entries: [] });

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

// The refilling answer carries the real listener's `Retry-After: 5` (writeListRefilling) unless a
// test sets its own with the state: a row that waits through several refilling reads waits each
// one's two retries, and only the row pinning what Dispatch passes on needs the listener's 5. The
// figure outlives a session reseed, as the state does, and a value no header can carry is refused.
test("the refilling list answer carries Retry-After 5 unless a test set its own", async () => {
  const { origin, stop } = await startFakeEnvoy();
  const reads = () =>
    Promise.all([Bun.fetch(`${origin}/v1/sessions`), Bun.fetch(`${origin}/v1/interests/`)]);
  try {
    expect((await putFixture(origin, "/__fixture/lists", { state: "refilling" })).status).toBe(200);
    const listener = await reads();
    expect(listener.map((answer) => answer.status)).toEqual([503, 503]);
    expect(listener.map((answer) => answer.headers.get("Retry-After"))).toEqual(["5", "5"]);
    expect(await listener[0].json()).toMatchObject({ code: "refilling" });

    const set = await putFixture(origin, "/__fixture/lists", {
      retry_after_seconds: 1,
      state: "refilling",
    });
    expect(await set.json()).toEqual({ retry_after_seconds: 1, state: "refilling" });
    expect((await reads()).map((answer) => answer.headers.get("Retry-After"))).toEqual(["1", "1"]);
    const sessions: FakeSession[] = [{ session_id: "planner", title: "Planner" }];
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);
    expect((await reads()).map((answer) => answer.headers.get("Retry-After"))).toEqual(["1", "1"]);

    for (const refused of [0, 1.5, -1, "1"]) {
      const answer = await putFixture(origin, "/__fixture/lists", {
        retry_after_seconds: refused,
        state: "refilling",
      });
      expect(answer.status).toBe(400);
    }
    expect((await reads()).map((answer) => answer.headers.get("Retry-After"))).toEqual(["1", "1"]);

    // Set back to the listener's own figure by a state that names none.
    expect((await putFixture(origin, "/__fixture/lists", { state: "refilling" })).status).toBe(200);
    expect((await reads()).map((answer) => answer.headers.get("Retry-After"))).toEqual(["5", "5"]);
  } finally {
    await stop();
  }
});

/** A send to `target` as Dispatch's listener client writes one, under `key`, asking for a
 *  session that is running now when `connectedOnly` is set. */
function send(origin: string, target: string, key: string, connectedOnly = false) {
  return Bun.fetch(`${origin}/v1/messages/send`, {
    body: JSON.stringify({
      expects_reply: false,
      idempotency_key: key,
      message: "Report status.",
      payload: "{}",
      source: "dispatch",
      target_session: target,
      urgency: "normal",
      ...(connectedOnly ? { connected_only: true } : {}),
    }),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  });
}

// A stopped session is one the listener knows only by its known-session record: the per-session
// route answers it only to a read with `stopped` in its `include=` list
// (`include=unresponsive,stopped`, as Dispatch's delivery read asks), a send to it is stored and
// answered 200 `state: "stopped"` beside `recipient_state: "gone"`, and a send that asked for a
// running session is refused 409 with nothing stored. An entry still in place whose plugin has
// gone - the probe's no-responders - answers `stopped` with its own age's state.
test("a stopped session is read only with stopped in include=, and a send to it is stored, or refused under connected_only", async () => {
  const { origin, stop } = await startFakeEnvoy();
  try {
    const sessions: FakeSession[] = [
      { capabilities: ["steer"], session_id: "stopped", title: "Stopped" },
      { capabilities: ["steer"], session_id: "crashed", title: "Crashed" },
    ];
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);
    expect(
      (await putFixture(origin, "/__fixture/sessions/stopped", { stopped: true }, "PATCH")).status
    ).toBe(200);

    const read = (query: string) => Bun.fetch(`${origin}/v1/registry/stopped${query}`);
    expect((await read("")).status).toBe(404);
    expect((await read("?include=unresponsive")).status).toBe(404);
    const known = await read("?include=unresponsive,stopped");
    expect(known.status).toBe(200);
    expect(await known.json()).toEqual({
      capabilities: ["steer"],
      last_seen: expect.any(Number),
      session_id: "stopped",
      state: "stopped",
      title: "Stopped",
      topics: [],
    });
    // The list holds the sessions that are running, whatever a caller asks for.
    const listed = await Bun.fetch(`${origin}/v1/sessions?include=unresponsive,stopped`);
    expect(await listed.json()).toMatchObject([{ session_id: "crashed", state: "live" }]);

    const queued = await send(origin, "stopped", "key-1");
    expect(queued.status).toBe(200);
    expect(await queued.json()).toEqual({
      event_id: "envelope-1",
      last_seen: expect.any(Number),
      recipient: "stopped",
      recipient_state: "gone",
      state: "stopped",
    });
    const refused = await send(origin, "stopped", "key-2", true);
    expect(refused.status).toBe(409);
    expect(await refused.json()).toEqual({ error: "the session is stopped" });

    // The entry the registry still reads live, whose send finds no plugin.
    expect(
      (await putFixture(origin, "/__fixture/sessions/crashed", { send_state: "stopped" }, "PATCH"))
        .status
    ).toBe(200);
    expect((await Bun.fetch(`${origin}/v1/registry/crashed`)).status).toBe(200);
    const crashed = await send(origin, "crashed", "key-3");
    expect(crashed.status).toBe(200);
    expect(await crashed.json()).toMatchObject({ recipient_state: "live", state: "stopped" });
    expect((await send(origin, "crashed", "key-4", true)).status).toBe(409);

    // Started again, the session is a running one on every route.
    expect(
      (await putFixture(origin, "/__fixture/sessions/stopped", { stopped: false }, "PATCH")).status
    ).toBe(200);
    expect((await read("")).status).toBe(200);
    const sent = await send(origin, "stopped", "key-5", true);
    expect(sent.status).toBe(200);
    expect(await sent.json()).toMatchObject({ recipient_state: "live", state: "connected" });
  } finally {
    await stop();
  }
});

/** A role publish as Dispatch's listener client writes one for a role it keeps a message for,
 *  under a key of the role's own, with `keep` as given (left out when undefined). */
function publish(origin: string, role: string, keep?: boolean) {
  return Bun.fetch(`${origin}/v1/messages/publish`, {
    body: JSON.stringify({
      expects_reply: "optional",
      idempotency_key: `key-${role}`,
      message: "Who planned this?",
      payload: "{}",
      source: "dispatch",
      topic: `notifications.role.${role}`,
      ...(keep === undefined ? {} : { keep }),
    }),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  });
}

// The publish route for a role topic, as Dispatch uses it to keep a message for a Legion role the
// registry holds no entry for while its project answers (packages/envoy/cmd/listener/confirmed.go
// roleSend): a role no live session holds keeps the message, answered 202 `kept` with the end of
// its hour, unless the publish asked `keep: false`, which keeps nothing and is the role lookup's
// 404; a role a live session holds takes it, answered 200 naming that holder. Every publish is
// recorded. The registry reads answer the answering flag the fixture last set, false until it is
// set, on the role read and on each entry.
test("a role publish is kept while no live session holds the role, and every publish is recorded", async () => {
  const { origin, stop } = await startFakeEnvoy();
  try {
    const planner = roleToken("e2elegion", "E2E-2", "planner");
    const reviewer = roleToken("e2elegion", "E2E-2", "reviewer");
    const sessions: FakeSession[] = [{ roles: [reviewer], session_id: "holder", title: "Holder" }];
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);

    const before = Date.now();
    const kept = await publish(origin, planner, true);
    expect(kept.status).toBe(202);
    const keptAnswer = (await kept.json()) as Record<string, unknown>;
    expect(keptAnswer).toEqual({
      event_id: "publish-1",
      kept: true,
      kept_reason: "unclaimed",
      kept_until: expect.any(Number),
      topic: `notifications.role.${planner}`,
    });
    expect(Number(keptAnswer.kept_until)).toBeGreaterThanOrEqual(before + 3_600_000);
    const refused = await publish(origin, planner, false);
    expect(refused.status).toBe(404);
    expect(await refused.json()).toEqual({
      claim_revision: 0,
      error: `no holder for role ${planner}`,
      reason: "unclaimed",
    });
    const delivered = await publish(origin, reviewer);
    expect(delivered.status).toBe(200);
    expect(await delivered.json()).toEqual({
      event_id: "publish-3",
      holder: "holder",
      holder_state: "live",
      topic: `notifications.role.${reviewer}`,
    });
    expect(
      (
        await Bun.fetch(`${origin}/v1/messages/publish`, {
          body: JSON.stringify({ message: "m", topic: "notifications.team.ops" }),
          headers: { "Content-Type": "application/json" },
          method: "POST",
        })
      ).status
    ).toBe(400);

    const recorded = await Bun.fetch(`${origin}/__fixture/publishes`);
    expect(await recorded.json()).toMatchObject([
      { idempotency_key: `key-${planner}`, keep: true, topic: `notifications.role.${planner}` },
      { keep: false, topic: `notifications.role.${planner}` },
      { topic: `notifications.role.${reviewer}` },
      { topic: "notifications.team.ops" },
    ]);

    const read = () => Bun.fetch(`${origin}/v1/legion/agents/roles/${planner}`);
    expect(await (await read()).json()).toEqual({ answering: false, entries: [] });
    const answering = await putFixture(origin, "/__fixture/legion-answering", { answering: true });
    expect(await answering.json()).toEqual({ answering: true });
    const stopped = legionAgentEntry({
      issue: "E2E-2",
      legionProject: "e2elegion",
      role: "planner",
      sessionId: "stopped-planner",
      startedAt: "2026-10-26T00:00:00Z",
    });
    expect((await putFixture(origin, "/__fixture/legion-agents", [stopped])).status).toBe(200);
    expect(await (await read()).json()).toMatchObject({
      answering: true,
      entries: [{ answering: true, role_token: planner, session_id: "stopped-planner" }],
    });
    expect(
      (await putFixture(origin, "/__fixture/legion-answering", { answering: "yes" })).status
    ).toBe(400);
  } finally {
    await stop();
  }
});

test("a role claim on the bus names the live holder the role lookup answers, and a listener with no bus writes none", async () => {
  const { origin, stop } = await startFakeEnvoy();
  try {
    const sessions: FakeSession[] = [
      { capabilities: ["steer"], roles: ["shared"], session_id: "first", title: "First" },
      { capabilities: ["steer"], roles: ["shared"], session_id: "second", title: "Second" },
    ];
    expect((await putFixture(origin, "/__fixture/sessions", sessions)).status).toBe(200);
    const claim = (role: string, method: "PUT" | "DELETE" = "PUT") =>
      Bun.fetch(`${origin}/__fixture/role-claims/${role}`, { method });
    const lookup = async () => (await Bun.fetch(`${origin}/v1/roles/shared`)).json();

    // A role no running session holds has no holder to claim it for, bus or none.
    const unheld = await claim("nobody-holds");
    expect(unheld.status).toBe(404);
    expect(await unheld.json()).toEqual({ error: "no live holder for role nobody-holds" });
    // A held role is refused without a bus, writing nothing, and the lookup answers as before.
    const noBus = { error: "this listener has no bus: DISPATCH_E2E_NATS_URLS is unset" };
    const refused = await claim("shared");
    expect(refused.status).toBe(409);
    expect(await refused.json()).toEqual(noBus);
    expect(await lookup()).toMatchObject({ holder: "first" });
    // The holder a claim names is the one the lookup answers, which moves once the first stops.
    expect(
      (await putFixture(origin, "/__fixture/sessions/first", { live: false }, "PATCH")).status
    ).toBe(200);
    expect(await lookup()).toMatchObject({ holder: "second" });
    const release = await claim("shared", "DELETE");
    expect(release.status).toBe(409);
    expect(await release.json()).toEqual(noBus);
  } finally {
    await stop();
  }
});
