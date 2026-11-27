import { expect, test } from "bun:test";
import {
  claimEnvoyRole,
  type LegionRoleClaimInstance,
  legionRoleClaimBridge,
  subscribeLegionNotice,
} from "@legion/pi-shared/role-claim-bridge";
import { claimRefusedForStop } from "@legion/pi-shared/role-claim-stopping";
import { logger } from "@oh-my-pi/pi-utils";
import { REPLAY_STATE_ENTRY } from "../src/missed-messages";
import type { EnvoyModule } from "../test/envoy-module";
import {
  createPi,
  sessionContext,
  settle,
  until,
  useExtensionHarness,
} from "../test/extension-harness";
import { claimGranted, response, responseWithRegistration } from "../test/listener-stand-in";
import type { SessionContext } from "../test/stand-in-host";

// A session whose stop has begun registers, unsubscribes and claims nothing, writes no ledger
// entry and tells its pane nothing (R24 of dispatch://INFRA-3589), whichever path is still
// running when the stop begins: the stop gates of `../src/stop-gates` and the client's, and the
// guards of the paths no gate covers. The rows drive the extension through the harness that
// `envoy.test.ts` uses, in a file of their own, which holds the rows of this subject and stays
// under 1,000 lines.

useExtensionHarness();

/** A branch entry no reader of the extension acts on: a session with one has history, so its
 *  pass reads the listener's rows for its role (`desiredRole`'s `readRows`). */
const HISTORY = { type: "custom", customType: "an-unrelated-plugin", data: {} };

/** The listener's refusal of a registration, which leaves the pass to throw once its role move
 *  is done. */
function refusedRegistration(): Response {
  return Response.json({ error: "the listener refused this registration" }, { status: 400 });
}

/** A host for `sessionID` with `branch`, whose intervals go to `intervals` and whose pane
 *  notices go to `notifications`. */
function host(
  sessionID: string,
  branch: readonly unknown[],
  intervals: (() => void)[],
  notifications: string[]
): SessionContext {
  const base = sessionContext(sessionID);
  return {
    ...base,
    sessionManager: { ...base.sessionManager, getBranch: () => branch },
    setInterval: (callback) => intervals.push(callback),
    ui: { ...base.ui, notify: (message) => notifications.push(message) },
  };
}

/** Holds a request's answer on the wire until the row calls what this adds to `held`. */
async function heldOn(held: (() => void)[]): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  held.push(resolve);
  await promise;
}

// A session with history starts, and the listener refuses its first registration. The pass goes
// on to its role move, which reads the listener's rows for the session's role. The session's stop
// begins while that read is on the wire, and the read is answered only once the stop is done. The
// pass then throws the refusal: that is no outage to report, since the stop deleted the record,
// so the pane is told nothing and no retry interval is armed.
test("a session_start registration that fails after the stop began tells the pane nothing and arms no retry", async () => {
  const session = "ses_start_failed_while_stopping";
  const heldReads: (() => void)[] = [];
  let reads = 0;
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    if (path === "/v1/interests/subscribe") return refusedRegistration();
    if (path === `/v1/interests/${session}`) {
      reads += 1;
      // The second read is the role move's (the first is the inbound's recovery of the
      // session's interests).
      if (reads === 2) await heldOn(heldReads);
      return Response.json({ error: "no interest" }, { status: 404 });
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as each row of `envoy.test.ts` loads: the query makes it a
  // module of its own, with state no other row shares, which a static import cannot give.
  const { default: envoyExtension }: EnvoyModule = await import(
    "./envoy.ts?start-failed-while-stopping"
  );
  const fixture = createPi();
  const intervals: (() => void)[] = [];
  const notifications: string[] = [];
  const context = host(session, [HISTORY], intervals, notifications);
  await envoyExtension(fixture.pi);

  const starting = fixture.handlers.get("session_start")?.({}, context);
  try {
    await until(() => heldReads.length === 1, "the role move's read on the wire", 10_000);
    const notified = notifications.length;
    const armed = intervals.length;

    await fixture.handlers.get("session_shutdown")?.({}, context);
    for (const answer of heldReads.splice(0)) answer();
    await starting;
    await settle();

    expect(notifications.slice(notified)).toEqual([]);
    expect(intervals.length).toBe(armed);
  } finally {
    for (const answer of heldReads.splice(0)) answer();
    await starting;
    await fixture.handlers.get("session_shutdown")?.({}, context);
  }
});

// The control of the row above: the same start, whose role move's read is answered with no stop
// begun. The pass throws the refusal, and the pane is told that the session is not registered, with
// a retry interval armed.
test("control: a session_start registration that fails with no stop begun tells the pane and arms a retry", async () => {
  const session = "ses_start_failed_no_stop";
  const heldReads: (() => void)[] = [];
  let reads = 0;
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    if (path === "/v1/interests/subscribe") return refusedRegistration();
    if (path === `/v1/interests/${session}`) {
      reads += 1;
      if (reads === 2) await heldOn(heldReads);
      return Response.json({ error: "no interest" }, { status: 404 });
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?start-failed-no-stop");
  const fixture = createPi();
  const intervals: (() => void)[] = [];
  const notifications: string[] = [];
  const context = host(session, [HISTORY], intervals, notifications);
  await envoyExtension(fixture.pi);

  const starting = fixture.handlers.get("session_start")?.({}, context);
  try {
    await until(() => heldReads.length === 1, "the role move's read on the wire", 10_000);
    const notified = notifications.length;
    const armed = intervals.length;

    for (const answer of heldReads.splice(0)) answer();
    await starting;
    await settle();

    expect(
      notifications
        .slice(notified)
        .filter((message) => message.startsWith("envoy: this session is not registered ("))
    ).toHaveLength(1);
    expect(intervals.length).toBe(armed + 1);
  } finally {
    for (const answer of heldReads.splice(0)) answer();
    await starting;
    await fixture.handlers.get("session_shutdown")?.({}, context);
  }
});

/** Runs a session's stop (`shutdown`) to its end, answering its DELETE once the session's fetch
 *  holds it on the wire (`heldDeletes`). */
async function stopAnsweringDelete(
  shutdown: () => unknown,
  heldDeletes: (() => void)[]
): Promise<void> {
  const stopping = shutdown();
  await until(() => heldDeletes.length > 0, "the stop's deregistration on the wire");
  for (const answer of heldDeletes.splice(0)) answer();
  await stopping;
}

/**
 * A session of a fresh instance of the entry (the `query` tag) that follows `topic`. Its fetch
 * records each request on `fetched`, method first, and holds the stop's DELETE on the wire
 * (`heldDeletes`), so that the stop still runs when a row calls a tool.
 */
async function followingSession(query: string, session: string, topic: string) {
  const heldDeletes: (() => void)[] = [];
  const fetched: string[] = [];
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    fetched.push(`${init?.method ?? "GET"} ${path}`);
    if (init?.method === "DELETE" && path === `/v1/sessions/${session}`) {
      await heldOn(heldDeletes);
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
  const fixture = createPi();
  const context = host(session, [], [], []);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  const tool = (name: string) => {
    const found = fixture.tools.find((candidate) => candidate.name === name);
    if (found === undefined) throw new Error(`${name} was not registered`);
    return found;
  };
  expect((await tool("envoy_subscribe").execute("", { topics: [topic] })).isError).toBeFalsy();
  return {
    fixture,
    context,
    fetched,
    heldDeletes,
    unsubscribe: () => tool("envoy_unsubscribe").execute("", { topics: [topic] }),
    ledgerEntries: () =>
      fixture.entries.filter(
        (entry) => "customType" in entry && entry.customType === REPLAY_STATE_ENTRY
      ).length,
  };
}

// A session follows a topic, and its stop begins: the stop's DELETE is still on the wire when the
// model calls `envoy_unsubscribe`. The tool is refused naming the stop. It sends the listener no
// removal and writes no ledger entry. The control row after it is the same call with no stop
// begun.
test("envoy_unsubscribe after the stop began sends nothing, writes no ledger entry, and is refused naming the stop", async () => {
  const session = "ses_unsubscribe_while_stopping";
  const run = await followingSession(
    "unsubscribe-while-stopping",
    session,
    "notifications.dispatch.issue.DSP-9.>"
  );
  try {
    const beforeStop = run.fetched.length;
    const stopping = run.fixture.handlers.get("session_shutdown")?.({}, run.context);
    await until(() => run.heldDeletes.length === 1, "the stop's deregistration on the wire");
    const ledgerWritten = run.ledgerEntries();

    const refused = await run.unsubscribe();
    await settle();

    expect(refused).toMatchObject({
      isError: true,
      content: [{ type: "text", text: "this session is stopping, so it unsubscribes nothing" }],
    });
    expect(run.fetched.slice(beforeStop)).toEqual([`DELETE /v1/sessions/${session}`]);
    expect(run.ledgerEntries()).toBe(ledgerWritten);
    for (const answer of run.heldDeletes.splice(0)) answer();
    await stopping;
  } finally {
    for (const answer of run.heldDeletes.splice(0)) answer();
    await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
  }
});

test("control: envoy_unsubscribe with no stop begun sends the removal and writes a ledger entry", async () => {
  const run = await followingSession(
    "unsubscribe-no-stop",
    "ses_unsubscribe_no_stop",
    "notifications.dispatch.issue.DSP-9.>"
  );
  try {
    const before = run.fetched.length;
    const ledgerWritten = run.ledgerEntries();

    const removed = await run.unsubscribe();
    await settle();

    expect(removed.isError).toBeFalsy();
    expect(run.fetched.slice(before)).toContain("POST /v1/interests/unsubscribe");
    expect(run.ledgerEntries()).toBeGreaterThan(ledgerWritten);
  } finally {
    await stopAnsweringDelete(
      () => run.fixture.handlers.get("session_shutdown")?.({}, run.context),
      run.heldDeletes
    );
  }
});

// A session switches to a session with history, and the listener refuses that session's first
// registration. The switch's pass goes on to its role move, and the session's stop begins while
// the move's read of the listener's rows is on the wire. The pass then throws the refusal into
// the switch, which reports a failure to the pane at any other time: the tell gate drops it.
test("a notice a switch raises after the stop began is dropped by the tell gate", async () => {
  const first = "ses_before_switch";
  const switched = "ses_switched_while_stopping";
  const heldReads: (() => void)[] = [];
  let reads = 0;
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
    if (path === "/v1/interests/subscribe" && body?.session_id === switched) {
      return refusedRegistration();
    }
    if (path === `/v1/interests/${switched}`) {
      reads += 1;
      // The second read is the role move's (the first is the inbound's recovery of the
      // session's interests).
      if (reads === 2) await heldOn(heldReads);
      return Response.json({ error: "no interest" }, { status: 404 });
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import(
    "./envoy.ts?switched-while-stopping"
  );
  const fixture = createPi();
  const notifications: string[] = [];
  const intervals: (() => void)[] = [];
  const before = host(first, [], intervals, notifications);
  const after = host(switched, [HISTORY], intervals, notifications);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, before);

  const switching = fixture.handlers.get("session_switch")?.({ reason: "new" }, after);
  try {
    await until(() => heldReads.length === 1, "the switch's role move read on the wire", 10_000);
    const notified = notifications.length;

    await fixture.handlers.get("session_shutdown")?.({}, after);
    for (const answer of heldReads.splice(0)) answer();
    await switching;
    await settle();

    expect(notifications.slice(notified)).toEqual([]);
  } finally {
    for (const answer of heldReads.splice(0)) answer();
    await switching;
    await fixture.handlers.get("session_shutdown")?.({}, after);
  }
});

// The control of the row above: the same switch, whose role move's read is answered with no stop
// begun. The pass throws the refusal into the switch, which tells the pane that it failed.
test("control: a switch whose pass fails with no stop begun tells the pane", async () => {
  const first = "ses_before_switch_no_stop";
  const switched = "ses_switched_no_stop";
  const heldReads: (() => void)[] = [];
  let reads = 0;
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
    if (path === "/v1/interests/subscribe" && body?.session_id === switched) {
      return refusedRegistration();
    }
    if (path === `/v1/interests/${switched}`) {
      reads += 1;
      if (reads === 2) await heldOn(heldReads);
      return Response.json({ error: "no interest" }, { status: 404 });
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?switched-no-stop");
  const fixture = createPi();
  const notifications: string[] = [];
  const intervals: (() => void)[] = [];
  const before = host(first, [], intervals, notifications);
  const after = host(switched, [HISTORY], intervals, notifications);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, before);

  const switching = fixture.handlers.get("session_switch")?.({ reason: "new" }, after);
  try {
    await until(() => heldReads.length === 1, "the switch's role move read on the wire", 10_000);
    const notified = notifications.length;

    for (const answer of heldReads.splice(0)) answer();
    await switching;
    await settle();

    expect(
      notifications
        .slice(notified)
        .filter((message) => message.startsWith("envoy: rebind failed ("))
    ).toHaveLength(1);
  } finally {
    for (const answer of heldReads.splice(0)) answer();
    await switching;
    await fixture.handlers.get("session_shutdown")?.({}, after);
  }
});

// A session whose subagent check could not publish its transcript at the start asks again at the
// next session event. Once the stop has begun, a session event does nothing: the check does not
// run, so it publishes no transcript and logs no failed publish outside the tell gate.
test("a session event after the stop began runs no subagent check", async () => {
  const session = "ses_event_after_stop";
  globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?event-after-stop");
  const fixture = createPi();
  let publishes = 0;
  const base = host(session, [], [], []);
  // The transcript's publish fails, so the check's answer is not kept: each event asks again.
  const context: SessionContext = {
    ...base,
    sessionManager: {
      ...base.sessionManager,
      ensureOnDisk: async () => {
        publishes += 1;
        throw new Error("another writer holds the transcript's publish lock");
      },
    },
  };
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  await fixture.handlers.get("session_shutdown")?.({}, context);
  const before = publishes;

  await fixture.handlers.get("session_switch")?.({ reason: "new" }, context);
  await fixture.handlers.get("session_branch")?.({}, context);
  await fixture.handlers.get("session_tree")?.({}, context);
  await settle();

  expect(publishes).toBe(before);
});

// The control of the row above: the same session, with no stop begun. Each session event asks
// again after the failed publish, so the check publishes the transcript again.
test("control: a session event with no stop begun runs the subagent check again", async () => {
  const session = "ses_event_no_stop";
  globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
  // A fresh instance of the entry, as the row above loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?event-no-stop");
  const fixture = createPi();
  let publishes = 0;
  const base = host(session, [], [], []);
  const context: SessionContext = {
    ...base,
    sessionManager: {
      ...base.sessionManager,
      ensureOnDisk: async () => {
        publishes += 1;
        throw new Error("another writer holds the transcript's publish lock");
      },
    },
  };
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  const before = publishes;
  try {
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, context);
    await fixture.handlers.get("session_branch")?.({}, context);
    await fixture.handlers.get("session_tree")?.({}, context);
    await settle();

    expect(publishes).toBeGreaterThan(before);
  } finally {
    await fixture.handlers.get("session_shutdown")?.({}, context);
  }
});

/** What a call that should fail failed with, or undefined when it did not fail. */
async function failure(call: Promise<void>): Promise<unknown> {
  return call.then(
    () => undefined,
    (error: unknown) => error
  );
}

/** The role and the controller topic the Legion controller's rows below claim and subscribe. */
const CONTROLLER_ROLE = "legion-acme-controller";
const CONTROLLER_NOTICE = "notifications.legion.ACME.controller";

/**
 * A session of a fresh instance of the entry (the `query` tag) that holds `role`. Its fetch hands
 * each request's path and init to `onRequest` first, and answers with what that returns. Where
 * that returns nothing, the listener grants the role's claim (`claimGranted`) and answers every
 * other request as it answers a registration.
 */
async function sessionHoldingRole(
  query: string,
  session: string,
  role: string,
  onRequest: (path: string, init: RequestInit | undefined) => Promise<Response | undefined>
) {
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    const answer = await onRequest(path, init);
    if (answer !== undefined) return answer;
    if (path === "/v1/roles/set") return claimGranted(session, role);
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
  const fixture = createPi();
  const context = host(session, [], [], []);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  await claimEnvoyRole(session, role, context);
  return { fixture, context };
}

/**
 * A session that holds `CONTROLLER_ROLE` (`sessionHoldingRole`). Its fetch records each request on
 * `fetched`, method first, and holds the stop's DELETE on the wire (`heldDeletes`), so that the
 * stop still runs when the subscription is asked for.
 */
async function controllerSession(query: string, session: string) {
  const heldDeletes: (() => void)[] = [];
  const fetched: string[] = [];
  const { fixture, context } = await sessionHoldingRole(
    query,
    session,
    CONTROLLER_ROLE,
    async (path, init) => {
      fetched.push(`${init?.method ?? "GET"} ${path}`);
      if (init?.method === "DELETE" && path === `/v1/sessions/${session}`) {
        await heldOn(heldDeletes);
      }
      return undefined;
    }
  );
  return {
    fixture,
    context,
    fetched,
    heldDeletes,
    subscribe: () => subscribeLegionNotice(session, CONTROLLER_NOTICE, context, CONTROLLER_ROLE),
    registrations: (since: number) =>
      fetched.slice(since).filter((call) => call.endsWith("/v1/interests/subscribe")),
  };
}

// The Legion controller claims its role, and its session's stop begins: the stop's DELETE is
// still on the wire when the controller asks for its controller topic for as long as it holds the
// role. The subscription is refused in the claim's words (`RoleClaimStoppingError`), which the
// controller reads as its session's stop, and nothing is registered for it. The control row after
// it asks for the same subscription with no stop begun.
test("a role-bound notice subscription after the stop began is refused naming the stop and registers nothing", async () => {
  const run = await controllerSession("subscribe-while-stopping", "ses_subscribe_while_stopping");
  try {
    const beforeStop = run.fetched.length;
    const stopping = run.fixture.handlers.get("session_shutdown")?.({}, run.context);
    await until(() => run.heldDeletes.length === 1, "the stop's deregistration on the wire");

    const refused = await failure(run.subscribe());
    await settle();

    expect(claimRefusedForStop(refused)).toBe(true);
    expect(run.registrations(beforeStop)).toEqual([]);
    for (const answer of run.heldDeletes.splice(0)) answer();
    await stopping;
  } finally {
    for (const answer of run.heldDeletes.splice(0)) answer();
    await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
  }
});

test("control: a role-bound notice subscription with no stop begun registers", async () => {
  const run = await controllerSession("subscribe-no-stop", "ses_subscribe_no_stop");
  try {
    const before = run.fetched.length;

    await run.subscribe();
    await settle();

    expect(run.registrations(before)).not.toEqual([]);
  } finally {
    await stopAnsweringDelete(
      () => run.fixture.handlers.get("session_shutdown")?.({}, run.context),
      run.heldDeletes
    );
  }
});

// A pi pane in RPC mode gets no `session_shutdown`, so a Legion claim can come after its stop has
// ended: a first-turn bound, or a claim retried after a 503. The session's instance stays on the
// role-claim bridge after its stop. The claim and a role-bound subscription for its session are
// refused naming the stop, and no other Envoy instance of the process is asked. The stopped
// instance moved to the front of the bridge's list, so a claim for a session no instance serves
// still reaches a live instance. Once the stopped instance is the only one, it refuses that claim,
// but not in the stop's words: the stop is not that session's.
test("a claim for a session whose stop ended is refused naming the stop, and no other instance is asked", async () => {
  const session = "ses_claim_after_stop";
  const role = "legion-acme-repo-1-implementer";
  globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?claim-after-stop");
  // Another live instance of the process, bound before the session's own: it records what the
  // bridge hands it.
  const asked: string[] = [];
  const other: LegionRoleClaimInstance = {
    claim: async (target, claimed) => {
      asked.push(`claim ${target} ${claimed}`);
    },
    subscribe: async (target, topic) => {
      asked.push(`subscribe ${target} ${topic}`);
    },
    sessionID: () => "ses_other_live",
  };
  const { instances } = legionRoleClaimBridge();
  instances.push(other);
  const fixture = createPi();
  const context = host(session, [], [], []);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  await fixture.handlers.get("session_shutdown")?.({}, context);

  const claimed = await failure(claimEnvoyRole(session, role, context));
  const subscribed = await failure(
    subscribeLegionNotice(session, `notifications.role.${role}`, context, role)
  );

  expect(asked).toEqual([]);
  expect(claimRefusedForStop(claimed)).toBe(true);
  expect(claimRefusedForStop(subscribed)).toBe(true);

  await claimEnvoyRole("ses_unserved", role);
  expect(asked).toEqual([`claim ses_unserved ${role}`]);

  instances.splice(instances.indexOf(other), 1);
  const elsewhere = await failure(claimEnvoyRole("ses_unserved", role));
  expect(elsewhere).toBeInstanceOf(Error);
  expect(claimRefusedForStop(elsewhere)).toBe(false);
});

/**
 * Each line the host's logger is handed while `logging()` reads true, debug lines apart, as
 * `<level> <message>`; `stop` ends the recording.
 */
function logLines(logging: () => boolean): { readonly lines: string[]; readonly stop: () => void } {
  const lines: string[] = [];
  const stop = logger.registerLogSink((entry) => {
    if (logging() && entry.level !== "debug") lines.push(`${entry.level} ${entry.message}`);
  });
  return { lines, stop };
}

// The stop's own lines are written while the stop runs (R24 of dispatch://INFRA-3589): the stop
// writes them to the host's logger, which no tell gate holds (`releaseRoleClaims`,
// `HandBackLedger.handBackAll`), and every line of the extension's own goes through the gate,
// which drops it then. So a stop whose hand-back the listener refuses logs that line and no other.
test("a stop whose hand-back the listener refuses logs the stop's own line, and no other line", async () => {
  let stopBegan = false;
  const log = logLines(() => stopBegan);
  const { fixture, context } = await sessionHoldingRole(
    "stop-hand-back-refused",
    "ses_stop_hand_back_refused",
    "reviewer",
    async (path) =>
      stopBegan && path === "/v1/interests/unsubscribe"
        ? Response.json({ error: "the listener refused the release" }, { status: 400 })
        : undefined
  );
  try {
    stopBegan = true;
    await fixture.handlers.get("session_shutdown")?.({}, context);

    expect(log.lines).toEqual(["warn envoy: the stop did not hand back a role this process held"]);
  } finally {
    log.stop();
  }
});

// The release that has not answered in its share of the stop is left, and the stop says so on the
// host's logger, which no tell gate holds.
test("a stop whose release does not answer in its share of the stop logs the stop's own line, and no other line", async () => {
  let stopBegan = false;
  const heldReleases: (() => void)[] = [];
  const log = logLines(() => stopBegan);
  const { fixture, context } = await sessionHoldingRole(
    "stop-release-unanswered",
    "ses_stop_release_unanswered",
    "reviewer",
    async (path) => {
      if (!stopBegan || path !== "/v1/interests/unsubscribe") return undefined;
      await heldOn(heldReleases);
      return response({});
    }
  );
  try {
    stopBegan = true;
    await fixture.handlers.get("session_shutdown")?.({}, context);

    expect(log.lines).toEqual([
      "warn envoy: the listener did not answer the release of the role claims in time",
    ]);
  } finally {
    for (const answer of heldReleases.splice(0)) answer();
    await settle();
    log.stop();
  }
});
