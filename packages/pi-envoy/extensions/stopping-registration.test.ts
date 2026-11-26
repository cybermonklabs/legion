import { expect, test } from "bun:test";
import { LISTENER_CALL_RETRY_DELAY_MS } from "@legion/contracts";
import { projectRefusal } from "@legion/envoy-client/test/subscribe-answer";
import { onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import type { EnvoyModule } from "../test/envoy-module";
import {
  createPi,
  sessionContext,
  settle,
  until,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import { claimGranted, response, responseWithRegistration } from "../test/listener-stand-in";
import type { SessionContext } from "../test/stand-in-host";

// A stopping session registers nothing and is told nothing (R24 of dispatch://INFRA-3589), also
// where a registration it sent before its stop is answered only after the stop began
// (`registerWithRelay`'s `stopping`), and it makes no role move once its stop began: a move that
// runs or waits its turn when the stop begins claims nothing after that and tells nothing, and
// `envoy_role_set` is refused. The rows drive the extension through the harness that
// `envoy.test.ts` uses. They live in a file of their own, which holds the rows of this subject
// and stays under 1,000 lines. Each row of a path the stop overtakes has a control row after it:
// the same stand-in with no stop begun, which shows what the row finds absent.

useExtensionHarness();

/** The role the rows below hold. */
const ROLE = "legion-controller";

/** The listener's answer to a read of `ROLE` that finds no live holder. */
function noHolder(): Response {
  return Response.json({ error: `no holder for role ${ROLE}` }, { status: 404 });
}

/** Whether a request to `path` with `init` is the stop's hand-back of `ROLE`: the removal of the
 *  role's topic from the session's interests. */
function handsBackRole(path: string, init: RequestInit | undefined): boolean {
  if (path !== "/v1/interests/unsubscribe") return false;
  const body = JSON.parse(init?.body?.toString() ?? "{}") as { readonly topics?: unknown };
  return Array.isArray(body.topics) && body.topics.includes(`notifications.role.${ROLE}`);
}

/** Whether a request to `path` with `init` is the stop's deregistration of `session`. */
function deregisters(session: string, path: string, init: RequestInit | undefined): boolean {
  return init?.method === "DELETE" && path === `/v1/sessions/${session}`;
}

/** Holds a request's answer on the wire until the row calls what this adds to `held`. */
async function heldOn(held: (() => void)[]): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  held.push(resolve);
  await promise;
}

/** A host for `sessionID` whose heartbeat ticks only when the row runs a tick of `intervals`,
 *  and whose pane notices go to `notifications`. */
function stoppingHost(sessionID: string) {
  const fixture = createPi();
  const intervals: (() => void)[] = [];
  const notifications: string[] = [];
  const base = sessionContext(sessionID);
  const context: SessionContext = {
    ...base,
    setInterval: (callback) => intervals.push(callback),
    ui: { ...base.ui, notify: (message) => notifications.push(message) },
  };
  return { fixture, intervals, notifications, context };
}

/** The root of the GitHub topics an edge listener refuses for a project in the rows below. */
const REFUSED_ROOT = "notifications.github.acme.priv";

/**
 * A session of a fresh instance of the entry (the `query` tag) that follows a topic under
 * `REFUSED_ROOT` and one other. Once the row calls `hold`, the listener holds each registration
 * that names a topic under that root on the wire (`held`), and answers it a project refusal once
 * the row lets it go. The topics of each registration go to `registrations`.
 */
async function projectRefusedSession(query: string, session: string) {
  let holding = false;
  const held: (() => void)[] = [];
  const registrations: string[][] = [];
  globalThis.fetch = async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
    if (url.pathname === "/v1/interests/subscribe") {
      const topics: string[] = Array.isArray(body?.topics) ? body.topics : [];
      registrations.push(topics);
      const refused = topics.filter((topic) => topic.startsWith(REFUSED_ROOT));
      if (holding && refused.length > 0) {
        await heldOn(held);
        return Response.json(projectRefusal(refused), { status: 403 });
      }
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as each row of `envoy.test.ts` loads: the query makes it a
  // module of its own, with state no other row shares, which a static import cannot give.
  const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
  const { fixture, intervals, notifications, context } = stoppingHost(session);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
  if (subscribeTool === undefined) throw new Error("envoy_subscribe was not registered");
  const subscribed = await subscribeTool.execute("", {
    topics: [`${REFUSED_ROOT}.>`, "notifications.dispatch.issue.PRIV-1.>"],
  });
  expect(subscribed.isError).toBeFalsy();
  return {
    fixture,
    context,
    intervals,
    notifications,
    registrations,
    held,
    hold: () => {
      holding = true;
    },
    /** The texts of the session's transcript records that name a refused topic. */
    toldInTranscript: () =>
      fixture.entries
        .map((entry) => ("data" in entry ? entry.data : undefined))
        .map((data) =>
          typeof data === "object" &&
          data !== null &&
          "content" in data &&
          typeof data.content === "string"
            ? data.content
            : ""
        )
        .filter((content) => content.includes(REFUSED_ROOT)),
  };
}

// A heartbeat's registration is on the wire when the session's stop begins, and only then does
// an edge listener answer it a project refusal: the rest is not registered again, and the
// session is told nothing of the refused topic, in its transcript or in its pane.
test("a project refusal answered after the stop began registers nothing again and tells nothing", async () => {
  const run = await projectRefusedSession("refused-while-stopping", "ses_refused_while_stopping");
  try {
    run.hold();
    run.intervals[0]?.();
    await until(() => run.held.length === 1, "the heartbeat's registration on the wire", 10_000);
    const heldAt = run.registrations.length;

    const stopping = run.fixture.handlers.get("session_shutdown")?.({}, run.context);
    for (const answer of run.held.splice(0)) answer();
    await stopping;
    await settle();

    expect(run.registrations.slice(heldAt)).toEqual([]);
    expect(run.toldInTranscript()).toEqual([]);
    expect(run.notifications.filter((message) => message.includes(REFUSED_ROOT))).toEqual([]);
  } finally {
    for (const answer of run.held.splice(0)) answer();
    await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
  }
});

test("control: a project refusal answered with no stop begun registers the rest again and tells the session", async () => {
  const run = await projectRefusedSession("refused-no-stop", "ses_refused_no_stop");
  try {
    run.hold();
    run.intervals[0]?.();
    await until(() => run.held.length === 1, "the heartbeat's registration on the wire", 10_000);
    const heldAt = run.registrations.length;

    for (const answer of run.held.splice(0)) answer();
    await until(() => run.registrations.length > heldAt, "the registration of the rest");
    await settle();

    expect(
      run.registrations
        .slice(heldAt)
        .map((topics) => topics.filter((topic) => topic.startsWith(REFUSED_ROOT)))
    ).toEqual(run.registrations.slice(heldAt).map(() => []));
    expect(run.toldInTranscript()).not.toEqual([]);
    expect(run.notifications.filter((message) => message.includes(REFUSED_ROOT))).not.toEqual([]);
  } finally {
    for (const answer of run.held.splice(0)) answer();
    await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
  }
});

/**
 * A session of a fresh instance of the entry (the `query` tag) whose `envoy_role_set` claims
 * `ROLE`. The listener names the session the role's holder until it loses the claim: at the
 * stop's hand-back of the role, or when the row calls `loseClaim`. It grants each claim, or answers
 * it a 500 once the row calls `failClaims`, or refuses it naming another holder once the row calls
 * `refuseClaims`. A row holds the next registration (`holdRegistration`), the next claim
 * (`holdClaim`) or each deregistration (`holdDeletes`) on the wire, until it answers what that put
 * on `held`. Every request goes to `requests`, method first, the body of each claim to `claims`,
 * and each regain Legion's hook hears to `regained`.
 */
async function roleSession(query: string, session: string) {
  let holds = true;
  let handedBack = false;
  let failing = false;
  let refusingFor: string | undefined;
  let holdingRegistration = false;
  let holdingClaim = false;
  let holdingDeletes = false;
  let registrations = 0;
  const requests: string[] = [];
  const claims: string[] = [];
  const held = {
    registrations: [] as (() => void)[],
    claims: [] as (() => void)[],
    deletes: [] as (() => void)[],
  };
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    requests.push(`${init?.method ?? "GET"} ${path}`);
    if (path === `/v1/roles/${ROLE}`) {
      return holds ? response({ role: ROLE, holder: session, last_seen: 1 }) : noHolder();
    }
    if (path === "/v1/roles/set") {
      claims.push(String(init?.body));
      if (holdingClaim) {
        holdingClaim = false;
        await heldOn(held.claims);
      }
      if (refusingFor !== undefined) {
        return Response.json(
          { error: `role ${ROLE} is held`, role: ROLE, holder: refusingFor },
          { status: 409 }
        );
      }
      return failing
        ? Response.json({ error: "the listener failed" }, { status: 500 })
        : claimGranted(session, ROLE);
    }
    if (handsBackRole(path, init)) {
      holds = false;
      handedBack = true;
    }
    if (deregisters(session, path, init) && holdingDeletes) await heldOn(held.deletes);
    if (path === "/v1/interests/subscribe") {
      registrations += 1;
      if (holdingRegistration) {
        holdingRegistration = false;
        await heldOn(held.registrations);
      }
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
  const regained: string[] = [];
  onEnvoyRoleRegained(async (role, reason) => {
    regained.push(`${role}:${reason}`);
  });
  const { fixture, intervals, notifications, context } = stoppingHost(session);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
  if (roleTool === undefined) throw new Error("envoy_role_set was not registered");
  return {
    fixture,
    context,
    intervals,
    notifications,
    requests,
    claims,
    regained,
    held,
    registrations: () => registrations,
    handedBack: () => handedBack,
    claimRole: () => roleTool.execute("", { role: ROLE }),
    loseClaim: () => {
      holds = false;
    },
    failClaims: () => {
      failing = true;
    },
    refuseClaims: (holder: string) => {
      refusingFor = holder;
    },
    holdRegistration: () => {
      holdingRegistration = true;
    },
    holdClaim: () => {
      holdingClaim = true;
    },
    holdDeletes: () => {
      holdingDeletes = true;
    },
    stop: () => fixture.handlers.get("session_shutdown")?.({}, context),
    /** Answers every call held on the wire, and runs the stop to its end. */
    end: async () => {
      holdingDeletes = false;
      for (const answer of Object.values(held).flatMap((calls) => calls.splice(0))) answer();
      await fixture.handlers.get("session_shutdown")?.({}, context);
    },
  };
}

// A session that holds a role: a heartbeat's registration is on the wire when the session's stop
// begins, and the listener accepts it only once the stop has handed the role back. The pass ends
// there. It reads and claims no role, so it takes back nothing the stop gave up, Legion's regain
// hook does not fire, and the pane hears nothing. The two control rows after it show the role
// move and the regain hook, and the pane's note of a role move.
test("a heartbeat's registration accepted after the stop began makes no role move and tells nothing", async () => {
  const run = await roleSession("accepted-while-stopping", "ses_accepted_while_stopping");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.holdRegistration();
    run.intervals[0]?.();
    await until(
      () => run.held.registrations.length === 1,
      "the heartbeat's registration on the wire",
      10_000
    );
    const notified = run.notifications.length;

    const beforeStop = run.requests.length;
    const stopping = run.stop();
    await until(run.handedBack, "the stop's hand-back of the role");
    for (const answer of run.held.registrations.splice(0)) answer();
    await stopping;
    await settle();

    expect(
      run.requests.slice(beforeStop).filter((request) => request.includes("/v1/roles/"))
    ).toEqual([]);
    expect(run.regained).toEqual([]);
    expect(run.notifications.slice(notified)).toEqual([]);
  } finally {
    await run.end();
  }
});

// The control of the row above: the same registration, accepted with no stop begun once the
// listener lost the role's claim. The pass reads the role, claims it back and fires the regain
// hook. A claim granted tells the pane nothing. The next row shows the move's note.
test("control: a heartbeat's registration accepted with no stop begun after the listener lost the claim claims the role back and fires the regain hook", async () => {
  const run = await roleSession("accepted-no-stop", "ses_accepted_no_stop");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.holdRegistration();
    run.intervals[0]?.();
    await until(
      () => run.held.registrations.length === 1,
      "the heartbeat's registration on the wire",
      10_000
    );

    const before = run.requests.length;
    run.loseClaim();
    for (const answer of run.held.registrations.splice(0)) answer();
    await until(() => run.regained.length > 0, "the regain hook");
    await settle();

    expect(run.requests.slice(before).filter((request) => request.includes("/v1/roles/"))).toEqual([
      `GET /v1/roles/${ROLE}`,
      "POST /v1/roles/set",
    ]);
    expect(run.regained).toEqual([`${ROLE}:reclaimed`]);
  } finally {
    await run.end();
  }
});

// The control of the pane's silence in the stop rows of a role move (the rows above and below
// that end "and tells nothing"): with no stop begun, a heartbeat's role move tells the pane when
// the listener refuses its claim, naming another holder.
test("control: a heartbeat's role move with no stop begun whose claim the listener refuses naming another holder tells the pane", async () => {
  const run = await roleSession("move-refused-no-stop", "ses_move_refused_no_stop");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.loseClaim();
    run.refuseClaims("ses_other_holder");
    const notified = run.notifications.length;

    run.intervals[0]?.();
    await until(() => run.notifications.length > notified, "the pane's note of the role move");
    await settle();

    expect(run.notifications.slice(notified)).toEqual([
      expect.stringContaining(`role ${ROLE} is now held by session ses_other_holder`),
    ]);
  } finally {
    await run.end();
  }
});

// A session that holds a role: a heartbeat's pass finds that the listener lost the claim, and its
// claim of the role again is on the wire when the session's stop begins. The listener answers
// that claim a 500 only once the stop is done, and the client's stop gate refuses the retry the
// 500 asks for, so the pass fails. That failure is no outage to report: no heartbeat follows it,
// so the pane hears nothing and is promised no retry.
test("a heartbeat's role claim that fails after the stop began tells the pane nothing", async () => {
  const run = await roleSession("claim-failed-while-stopping", "ses_claim_failed_while_stopping");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    // The listener drops the claim, so the heartbeat's pass claims the role again.
    run.loseClaim();
    run.failClaims();
    run.holdClaim();
    const claimed = run.claims.length;
    run.intervals[0]?.();
    await until(
      () => run.held.claims.length === 1,
      "the heartbeat's role claim on the wire",
      10_000
    );
    const notified = run.notifications.length;

    await run.stop();
    for (const answer of run.held.claims.splice(0)) answer();
    // The client waits its retry delay out before the stop gate refuses the retry, and nothing
    // marks that refusal, so the row waits the delay out on the real clock too.
    await Bun.sleep(LISTENER_CALL_RETRY_DELAY_MS * 2);
    await settle();

    expect(run.claims.length - claimed).toBe(1);
    expect(run.notifications.slice(notified)).toEqual([]);
  } finally {
    await run.end();
  }
});

// The control of the row above: the same failing claim answered with no stop begun. The client
// sends the retry the 500 asks for, and the pass that fails tells the pane.
test("control: a heartbeat's role claim that fails with no stop begun is retried and tells the pane", async () => {
  const run = await roleSession("claim-failed-no-stop", "ses_claim_failed_no_stop");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.loseClaim();
    run.failClaims();
    run.holdClaim();
    const claimed = run.claims.length;
    run.intervals[0]?.();
    await until(
      () => run.held.claims.length === 1,
      "the heartbeat's role claim on the wire",
      10_000
    );
    const notified = run.notifications.length;

    for (const answer of run.held.claims.splice(0)) answer();
    await until(() => run.notifications.length > notified, "the pane's note of the failure");
    await settle();

    expect(run.claims.length - claimed).toBeGreaterThan(1);
    expect(run.notifications.slice(notified)).not.toEqual([]);
  } finally {
    await run.end();
  }
});

/**
 * A session of a fresh instance of the entry (the `query` tag) whose listener holds the next
 * registration on the wire once the row calls `hold`, and answers it only when the row lets it
 * go (`held`).
 */
async function heldRegistrationSession(query: string, session: string) {
  let holding = false;
  const held: (() => void)[] = [];
  globalThis.fetch = async (input, init) => {
    if (new URL(input.toString()).pathname === "/v1/interests/subscribe" && holding) {
      holding = false;
      await heldOn(held);
    }
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
  const { fixture, intervals, notifications, context } = stoppingHost(session);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  return {
    fixture,
    context,
    intervals,
    notifications,
    held,
    hold: () => {
      holding = true;
    },
  };
}

// A heartbeat's registration is on the wire when the session's stop begins, and the listener
// answers it only after the pass's deadline. The pass is abandoned at that deadline, after the
// stop began. That is no stall that recovers, since the DELETE follows it, so the pane hears
// nothing. The wait is real: the queue abandons its task on a real timer, read at the task's
// start. The deadline is never shorter than the refresh interval, so the interval names 25 ms for
// a 1 s deadline to stand; every tick here is hand-driven, so none of them fires on it.
test("a heartbeat's pass abandoned at its deadline after the stop began tells the pane nothing", async () => {
  await withEnvoyTimings({ heartbeatMs: 25, passDeadlineMs: 1_000 }, async () => {
    const run = await heldRegistrationSession(
      "abandoned-while-stopping",
      "ses_abandoned_while_stopping"
    );
    try {
      run.hold();
      const notified = run.notifications.length;
      const ticked = performance.now();
      run.intervals[0]?.();
      await until(() => run.held.length === 1, "the heartbeat's registration on the wire");
      // The pass is still inside its deadline when the stop begins.
      expect(run.notifications.slice(notified)).toEqual([]);

      await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
      // Past the pass's deadline, with room for its timer to run late.
      await Bun.sleep(Math.max(0, ticked + 1_500 - performance.now()));
      await settle();

      expect(run.notifications.slice(notified)).toEqual([]);
    } finally {
      for (const answer of run.held.splice(0)) answer();
      await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
    }
  });
});

// The control of the row above: the same pass, abandoned at its deadline with no stop begun. The
// pane is told that the pass has stalled.
test("control: a heartbeat's pass abandoned at its deadline with no stop begun tells the pane", async () => {
  await withEnvoyTimings({ heartbeatMs: 25, passDeadlineMs: 1_000 }, async () => {
    const run = await heldRegistrationSession("abandoned-no-stop", "ses_abandoned_no_stop");
    try {
      run.hold();
      const notified = run.notifications.length;
      run.intervals[0]?.();
      await until(() => run.held.length === 1, "the heartbeat's registration on the wire");

      // The queue abandons the pass on its own real timer, a second after the pass began.
      await until(() => run.notifications.length > notified, "the pane's note of the stall", 5_000);
      await settle();

      expect(run.notifications.slice(notified)).toHaveLength(1);
    } finally {
      for (const answer of run.held.splice(0)) answer();
      await run.fixture.handlers.get("session_shutdown")?.({}, run.context);
    }
  });
});

// A session that holds a role: a heartbeat's pass finds that the listener lost the claim, and its
// claim of the role again is on the wire when the session's stop begins. The listener grants that
// claim while the stop is still running. The move ends there: Legion's regain hook does not fire,
// and the pane hears nothing. The control row after it shows the regain hook fire, and the row
// "control: a heartbeat's role move with no stop begun whose claim the listener refuses …" shows
// the pane's note of a role move.
test("a heartbeat's role claim granted after the stop began fires no regain hook and tells nothing", async () => {
  const run = await roleSession("reclaimed-while-stopping", "ses_reclaimed_while_stopping");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    // The listener drops the claim, so the heartbeat's pass claims the role again.
    run.loseClaim();
    run.holdClaim();
    run.intervals[0]?.();
    await until(
      () => run.held.claims.length === 1,
      "the heartbeat's role claim on the wire",
      10_000
    );
    const notified = run.notifications.length;

    // The stop's last call, held so that the stop still runs when the claim is answered.
    run.holdDeletes();
    const stopping = run.stop();
    await until(() => run.held.deletes.length === 1, "the stop's deregistration on the wire");
    for (const answer of run.held.claims.splice(0)) answer();
    await settle();

    expect(run.regained).toEqual([]);
    expect(run.notifications.slice(notified)).toEqual([]);
    for (const answer of run.held.deletes.splice(0)) answer();
    await stopping;
  } finally {
    await run.end();
  }
});

// The control of the row above: the same claim granted with no stop begun. The regain hook fires.
test("control: a heartbeat's role claim granted with no stop begun fires the regain hook", async () => {
  const run = await roleSession("reclaimed-no-stop", "ses_reclaimed_no_stop");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.loseClaim();
    run.holdClaim();
    run.intervals[0]?.();
    await until(
      () => run.held.claims.length === 1,
      "the heartbeat's role claim on the wire",
      10_000
    );

    for (const answer of run.held.claims.splice(0)) answer();
    await until(() => run.regained.length > 0, "the regain hook");
    await settle();

    expect(run.regained).toEqual([`${ROLE}:reclaimed`]);
  } finally {
    await run.end();
  }
});

// A session that holds a role: the model claims it again, and that claim is on the wire. A
// heartbeat's pass registers, and its role move waits its turn behind the claim. The session's
// stop begins and hands the role back, and only then is the claim answered. The waiting move
// claims nothing after the stop began, so it takes back nothing the stop gave up, Legion's regain
// hook does not fire, and the pane hears nothing. The tool says the session is stopping. The
// control row after it shows the claim and the regain hook, and the row "control: a heartbeat's
// role move with no stop begun whose claim the listener refuses …" shows the pane's note of a
// role move.
test("a role move that waits its turn when the stop begins claims nothing and tells nothing", async () => {
  const run = await roleSession("queued-while-stopping", "ses_queued_while_stopping");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.holdClaim();
    const claiming = run.claimRole();
    await until(() => run.held.claims.length === 1, "the tool's claim on the wire");
    const registered = run.registrations();
    run.intervals[0]?.();
    await until(() => run.registrations() > registered, "the heartbeat's registration", 10_000);
    // The pass's role move is queued a few turns after its registration is answered.
    await settle();
    const notified = run.notifications.length;

    const claimsBeforeStop = run.claims.length;
    // The stop's last call, held so that the stop still runs when the claim is answered.
    run.holdDeletes();
    const stopping = run.stop();
    await until(run.handedBack, "the stop's hand-back of the role");
    await until(() => run.held.deletes.length === 1, "the stop's deregistration on the wire");
    for (const answer of run.held.claims.splice(0)) answer();
    const claimed = await claiming;
    await settle();

    expect(run.claims.slice(claimsBeforeStop)).toEqual([]);
    expect(run.regained).toEqual([]);
    expect(run.notifications.slice(notified)).toEqual([]);
    expect(claimed).toMatchObject({
      isError: true,
      content: [
        {
          type: "text",
          text: `this session is stopping, so it claims no role: role ${ROLE} is not held here`,
        },
      ],
    });
    for (const answer of run.held.deletes.splice(0)) answer();
    await stopping;
  } finally {
    await run.end();
  }
});

// The control of the row above: the same move waiting behind the tool's claim, with no stop
// begun. The listener lost the role's claim meanwhile, as the stop's hand-back would have made it.
// Once the tool's claim is answered, the move claims the role back and fires the regain hook, and
// the tool's claim lands.
test("control: a role move that waits its turn with no stop begun claims the role back and fires the regain hook", async () => {
  const run = await roleSession("queued-no-stop", "ses_queued_no_stop");
  try {
    expect((await run.claimRole()).isError).toBeFalsy();
    run.holdClaim();
    const claiming = run.claimRole();
    await until(() => run.held.claims.length === 1, "the tool's claim on the wire");
    const registered = run.registrations();
    run.intervals[0]?.();
    await until(() => run.registrations() > registered, "the heartbeat's registration", 10_000);
    await settle();

    const claimsBefore = run.claims.length;
    run.loseClaim();
    for (const answer of run.held.claims.splice(0)) answer();
    const claimed = await claiming;
    await until(() => run.regained.length > 0, "the regain hook");
    await settle();

    expect(run.claims.slice(claimsBefore)).not.toEqual([]);
    expect(run.regained).toEqual([`${ROLE}:reclaimed`]);
    expect(claimed.isError).toBeFalsy();
  } finally {
    await run.end();
  }
});

// The model calls `envoy_role_set` while the session's stop runs. The tool refuses, naming the
// stop, and sends the listener no role call at all.
test("envoy_role_set called once the stop has begun is refused naming the stop and sends nothing", async () => {
  const session = "ses_role_set_while_stopping";
  let stopBegan = false;
  const heldDeletes: (() => void)[] = [];
  const roleCallsAfterStop: string[] = [];
  globalThis.fetch = async (input, init) => {
    const path = new URL(input.toString()).pathname;
    if (stopBegan && path.startsWith("/v1/roles/")) roleCallsAfterStop.push(path);
    if (path === "/v1/roles/set") return claimGranted(session, ROLE);
    // The stop's last call, held so that the stop still runs when the tool is called.
    if (deregisters(session, path, init)) await heldOn(heldDeletes);
    return responseWithRegistration(input, init, {});
  };
  // A fresh instance of the entry, as the first row loads: no static import gives one.
  const { default: envoyExtension }: EnvoyModule = await import(
    "./envoy.ts?role-set-while-stopping"
  );
  const { fixture, context } = stoppingHost(session);
  await envoyExtension(fixture.pi);
  await fixture.handlers.get("session_start")?.({}, context);
  const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
  if (roleTool === undefined) throw new Error("envoy_role_set was not registered");

  try {
    stopBegan = true;
    const stopping = fixture.handlers.get("session_shutdown")?.({}, context);
    await until(() => heldDeletes.length === 1, "the stop's deregistration on the wire");

    const refused = await roleTool.execute("", { role: ROLE });
    await settle();

    expect(roleCallsAfterStop).toEqual([]);
    expect(refused).toMatchObject({
      isError: true,
      content: [
        {
          type: "text",
          text: `this session is stopping, so it claims no role: role ${ROLE} is not held here`,
        },
      ],
    });
    for (const answer of heldDeletes.splice(0)) answer();
    await stopping;
  } finally {
    for (const answer of heldDeletes.splice(0)) answer();
    await fixture.handlers.get("session_shutdown")?.({}, context);
  }
});
