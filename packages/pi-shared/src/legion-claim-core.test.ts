import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LegionTakeResponse, type LegionTask } from "@legion/contracts/legion-api";
import { EnvoyApiError } from "@legion/envoy-client/transport";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { claimCaller, createLegionDaemonClient } from "./daemon-client";
import {
  envoyPluginInterface,
  publishEnvoyPluginInterface,
  resetEnvoyPluginInterfaceForTests,
} from "./interface";
import {
  bootLegionClaim,
  claimRoleAgainOnBusy,
  claimStandsWithoutHandover,
  type LegionClaimHost,
} from "./legion-claim-core";
import type { ClaimCapability } from "./legion-phase-ops";
import { createTaskTake, type TaskTake } from "./task-take";
import { taskTurnMark } from "./task-values";

/** The contract the build under test speaks, which its client states on every request. */
const CONTRACT = 60;

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

const registered = daemonFixture("register.json");
const current = daemonFixture("task.json");
const claimToken = String(registered.claimToken);
/** The boot token the pane hands a registering session. */
const BOOT_TOKEN = fixtureSecret("boot");
const registration = {
  bootToken: BOOT_TOKEN,
  sessionId: "ses_claim",
  ompSessionFile: "/sessions/ses_claim.jsonl",
  agentId: "ses_claim",
  pluginContract: 29,
};

/** A host over a stub daemon whose `register`, `ready` and `task` answer as given, recording
 * every step it is asked for, in order, beside the daemon's requests, and every task value it is
 * handed. */
function claimHost(options: {
  readonly register?: () => Response;
  readonly ready?: () => Response | Promise<Response>;
  readonly task?: () => Response | Promise<Response>;
  readonly overrides?: Partial<LegionClaimHost>;
}) {
  const steps: string[] = [];
  const lines: string[] = [];
  const capabilities: ClaimCapability[] = [];
  const values: LegionTask[] = [];
  let regain: ((role: string, reason: string) => Promise<void>) | undefined;
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname.replace("/legion/v1/", "");
    steps.push(`${route} ${init?.body ?? ""}`);
    if (route === "claims/register") return options.register?.() ?? Response.json(registered);
    if (route === "claims/task") return options.task?.() ?? Response.json(current);
    return options.ready?.() ?? new Response(null, { status: 204 });
  }) as typeof fetch;
  const host: LegionClaimHost = {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    registration,
    setCapability: (capability) => {
      steps.push("setCapability");
      capabilities.push(capability);
    },
    claimRole: async (role) => {
      steps.push(`claimRole ${role}`);
    },
    taskValue: (reading) => {
      steps.push("taskValue");
      values.push(reading);
    },
    afterReadyRead: () => steps.push("afterReadyRead"),
    onRoleRegained: (listener) => {
      steps.push("onRoleRegained");
      regain = listener;
    },
    log: (line) => lines.push(line),
    stopping: "exiting so the daemon launches it again",
    stop: () => steps.push("stop"),
    ...options.overrides,
  };
  return { host, steps, lines, capabilities, values, regain: () => regain };
}

const caller = () => ({
  claimToken,
  sessionId: "ses_claim",
  secret: registered.secret,
  generation: registered.generation,
});
const readyBody = () => JSON.stringify(caller());

test("registers, hands the registration to afterRegister, records the capability, readies without a capability report, reads the task value, claims the role right after, and reads the value again on a regain of its role", async () => {
  const { host, steps, lines, capabilities, values, regain } = claimHost({
    overrides: {
      afterRegister: async (claim) => {
        steps.push(`afterRegister ${claim.claimToken} task ${claim.task?.number}`);
      },
    },
  });

  const boot = await bootLegionClaim(host);

  expect(boot).toBeDefined();
  // The registration's value goes to the host before the ready, and the value read once the
  // ready has answered goes to it before the role claim, and so does the word that the read has
  // answered: the take's mark stands before the role's kept lane is handed over.
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    `afterRegister ${claimToken} task 4`,
    "setCapability",
    `claims/ready ${readyBody()}`,
    `claims/task ${JSON.stringify(caller())}`,
    "taskValue",
    "afterReadyRead",
    `claimRole ${claimToken}`,
    "onRoleRegained",
  ]);
  expect(capabilities).toEqual([
    {
      sessionID: "ses_claim",
      tree: String(registered.tree),
      issue: String(registered.issue),
      role: registered.role as never,
      secret: String(registered.secret),
      claimToken,
      generation: Number(registered.generation),
      promptAgents: registered.promptAgents as never,
    },
  ]);

  // Another role's regain reads nothing; this claim's reads its value, and sends no ready, nor
  // says again that the read after the ready has answered.
  await regain()?.("some-other-role", "reclaimed");
  await regain()?.(claimToken, "reregistered");
  expect(steps.slice(9)).toEqual([`claims/task ${JSON.stringify(caller())}`, "taskValue"]);
  expect(values).toEqual([current as LegionTask, current as LegionTask]);
  expect(lines).toEqual([`read claims/task after role ${claimToken} was reregistered`]);
});

// The ready decision itself can write the claim's value (the
// relaunch record's task), after the registration answered with none, and the listener relays a
// value only once the role claim has changed the role's holder, after it handed the kept lane
// over. So the boot reads the value once the ready has answered and hands it to the host before
// it claims the role: the host's take holds it, and its mark, ahead of every card the lane brings.
test("hands the host the value the ready decision wrote, read once the ready answered, before the role is claimed", async () => {
  const written = { task: { number: 5, epoch: "2026-11-13T16:21:44.123456789Z", revision: 13 } };
  const { host, steps, values, lines } = claimHost({
    register: () => Response.json({ ...registered, task: null, finished: 4 }),
    task: () => Response.json({ ...written, finished: 4 }),
    overrides: {
      afterRegister: async (claim) => {
        steps.push(`afterRegister task ${claim.task?.number ?? "none"}`);
      },
    },
  });

  await bootLegionClaim(host);

  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "afterRegister task none",
    "setCapability",
    `claims/ready ${readyBody()}`,
    `claims/task ${JSON.stringify(caller())}`,
    "taskValue",
    "afterReadyRead",
    `claimRole ${claimToken}`,
    "onRoleRegained",
  ]);
  expect(values).toEqual([{ ...written, finished: 4 } as LegionTask]);
  expect(lines).toEqual([]);
});

// The plan's decision 15, at the card: the session's agent
// subject is open from its start, so a card addressed to the session itself — its own direct
// message — can reach it while the ready, or the read of the claim's value after it, is still on
// its way, and here the registration answered no value. Oh My Pi's host holds its take's
// task-turn mark from the registration until that read has answered (`TaskTake.awaitRead`, ended
// at `afterReadyRead`), and pi-envoy hands a card that finds the session idle over with no turn
// while that mark names the session, so the task the ready decision wrote is the first turn.

/** The session of the claim these cases boot. */
const SESSION = "ses_claim";

/** The daemon's answer to a take of the task the ready decision wrote, as its golden test wrote
 * it. */
const ACCEPTED = LegionTakeResponse.parse(daemonFixture("take-accepted.json"));
const TASK_TEXT = ACCEPTED.outcome === "accepted" ? ACCEPTED.text : "";

/**
 * An Oh My Pi claim's host over `claimHost`'s stub, wired as `packages/pi-legion/src/claim-session.ts`
 * wires its take: built at the registration, holding its value and its task-turn mark from there,
 * the mark the plugin interface's, held whatever value the take holds until the read after the
 * ready has answered. The Envoy entry has published the interface, as it does before any claim
 * boots. `turns` is every turn the session starts, in order: the take's text, and a card's own
 * (`card`). `claimRole`, given, is the role claim, claimed again after a 1 ms wait on a 503.
 */
function takingHost(
  options: Pick<Parameters<typeof claimHost>[0], "register" | "ready" | "task"> & {
    readonly claimRole?: LegionClaimHost["claimRole"];
  }
) {
  resetEnvoyPluginInterfaceForTests();
  publishEnvoyPluginInterface("pi-envoy");
  const turns: string[] = [];
  let take: TaskTake | undefined;
  let readAnswered: (() => void) | undefined;
  const { claimRole, ...daemon } = options;
  const claim = claimHost({
    ...daemon,
    overrides: {
      afterRegister: async (registered) => {
        take = createTaskTake({
          daemon: { taskSeen: async () => undefined, take: async () => ACCEPTED },
          caller: claimCaller(registered, SESSION),
          run: (text) => {
            turns.push(text);
          },
          setPending: taskTurnMark(SESSION),
          log: { info: () => undefined, warn: () => undefined },
        });
        take.read(registered);
        readAnswered = take.awaitRead();
      },
      taskValue: (reading) => take?.read(reading),
      afterReadyRead: () => readAnswered?.(),
      ...(claimRole === undefined ? {} : { claimRole, roleClaimRetryFirstDelayMs: 1 }),
    },
  });
  return {
    ...claim,
    turns,
    take: () => take,
    /**
     * A card that reaches the idle session, as pi-envoy's card wake hands it over
     * (`CardWake.deliveryFor`, packages/pi-envoy/src/card-wake.ts): with no turn while the
     * task-turn mark names this session, and otherwise a turn of its own, whose text is `text`.
     */
    card: (text: string): void => {
      if (envoyPluginInterface().taskTurnPending?.session !== SESSION) turns.push(text);
    },
  };
}

/** Lets every promise queued so far settle. */
async function flush(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

test("a card that reaches the session between the ready and the answer of the read after it — its own direct message — starts no turn before the task the ready decision wrote", async () => {
  const written = { number: 5, epoch: "2026-11-13T16:21:44.123456789Z", revision: 13 };
  const reading = Promise.withResolvers<void>();
  const answer = Promise.withResolvers<void>();
  const host = takingHost({
    register: () => Response.json({ ...registered, task: null, finished: 4 }),
    task: async () => {
      reading.resolve();
      await answer.promise;
      return Response.json({ task: written, finished: 4 });
    },
  });

  const booting = bootLegionClaim(host.host);
  // The ready has answered, and the read after it waits for the daemon.
  await reading.promise;
  host.card("the session's own direct message");
  answer.resolve();
  await booting;
  // The role is claimed, and a card its kept lane hands over finds the task owed.
  host.card("a card the role's kept lane handed over");
  // The take asks once the boot is done, and its accepted text starts the session's first turn.
  host.take()?.ready();
  await flush();
  expect(host.turns).toEqual([TASK_TEXT]);
  expect(envoyPluginInterface().taskTurnPending).toEqual({ session: SESSION });
  host.take()?.runStarted();
  expect(envoyPluginInterface().taskTurnPending).toBeUndefined();
  resetEnvoyPluginInterfaceForTests();
});

for (const [name, answer, logged] of [
  ["holds no value", () => Response.json({ task: null, finished: 4 }), []],
  [
    "fails",
    () => Response.json({ error: "the daemon is restarting" }, { status: 503 }),
    [
      "claims/task after claims/ready failed; the claim's value reaches this session with the daemon's next write of it: POST /legion/v1/claims/task failed with 503: the daemon is restarting",
    ],
  ],
] as const) {
  test(`a card that reaches the session while the read after the ready waits starts no turn, and once that read ${name} the hold ends and a card starts its turn`, async () => {
    const reading = Promise.withResolvers<void>();
    const answered = Promise.withResolvers<void>();
    const host = takingHost({
      register: () => Response.json({ ...registered, task: null, finished: 4 }),
      task: async () => {
        reading.resolve();
        await answered.promise;
        return answer();
      },
    });

    const booting = bootLegionClaim(host.host);
    await reading.promise;
    host.card("the session's own direct message");
    expect(host.turns).toEqual([]);
    answered.resolve();
    await booting;

    // Nothing is owed: the mark is down, and a card starts its turn.
    expect(envoyPluginInterface().taskTurnPending).toBeUndefined();
    host.card("a card after the read answered");
    expect(host.turns).toEqual(["a card after the read answered"]);
    expect(host.lines).toEqual([...logged]);
    resetEnvoyPluginInterfaceForTests();
  });
}

// A ready the daemon refuses with a 4xx (a spent secret, a stale claim) is not tried again, and
// the boot stops the session, as any failure after a registration does: the read after the ready
// is never made and never answers, so the hold the take took at the registration is never ended
// by it, and ends only with the session. A card that reaches the session while the ready is on its
// way, or as the session ends, starts no turn.
test("a ready the daemon refuses with a 4xx stops the session, whose hold ends only with it: no card starts a turn, during the ready or after its refusal", async () => {
  const readying = Promise.withResolvers<void>();
  const refused = Promise.withResolvers<void>();
  const host = takingHost({
    register: () => Response.json({ ...registered, task: null, finished: 4 }),
    ready: async () => {
      readying.resolve();
      await refused.promise;
      return Response.json({ error: "Invalid session secret" }, { status: 403 });
    },
  });

  const booting = bootLegionClaim(host.host);
  await readying.promise;
  host.card("the session's own direct message");
  refused.resolve();
  await expect(booting).resolves.toBeUndefined();

  expect(host.steps.at(-1)).toBe("stop");
  expect(host.steps.some((step) => step.startsWith("claims/task"))).toBe(false);
  expect(host.steps).not.toContain("afterReadyRead");
  expect(host.lines).toEqual([
    `claim boot failed after claims/register registered ${claimToken}; exiting so the daemon launches it again: POST /legion/v1/claims/ready failed with 403: Invalid session secret`,
  ]);
  // The session is on its way out with its hold still up.
  host.card("a card as the session ends");
  expect(envoyPluginInterface().taskTurnPending).toEqual({ session: SESSION });
  expect(host.turns).toEqual([]);
  resetEnvoyPluginInterfaceForTests();
});

test("a value read after the ready that fails is logged, and the role is claimed all the same", async () => {
  const { host, steps, values, lines } = claimHost({
    task: () => Response.json({ error: "the daemon is restarting" }, { status: 503 }),
  });

  await expect(bootLegionClaim(host)).resolves.toBeDefined();

  expect(values).toEqual([]);
  expect(steps.slice(-4)).toEqual([
    `claims/task ${JSON.stringify(caller())}`,
    "afterReadyRead",
    `claimRole ${claimToken}`,
    "onRoleRegained",
  ]);
  expect(steps).not.toContain("stop");
  expect(lines).toEqual([
    "claims/task after claims/ready failed; the claim's value reaches this session with the daemon's next write of it: POST /legion/v1/claims/task failed with 503: the daemon is restarting",
  ]);
});

test("reads the claim's value at the host's ask, at a new epoch of its relay, and a failed read is logged and never rejects", async () => {
  let answer = () => Response.json(current);
  const { host, steps, lines, values } = claimHost({ task: () => answer() });
  const boot = await bootLegionClaim(host);
  // The boot's own read, once its ready answered.
  expect(values).toEqual([current as LegionTask]);

  await boot?.readTask();
  expect(values).toEqual([current as LegionTask, current as LegionTask]);

  answer = () => Response.json({ error: "the daemon is restarting" }, { status: 503 });
  await expect(boot?.readTask()).resolves.toBeUndefined();
  expect(values).toHaveLength(2);
  expect(steps.filter((step) => step.startsWith("claims/task"))).toHaveLength(3);
  expect(lines).toEqual([
    "claims/task at the relay's new epoch failed; the claim's value reaches this session with the daemon's next write of it: POST /legion/v1/claims/task failed with 503: the daemon is restarting",
  ]);
});

for (const status of [400, 403, 409]) {
  test(`a ${status} at claims/register is logged and stops the agent, claiming nothing`, async () => {
    const { host, steps, lines } = claimHost({
      register: () => Response.json({ error: "Invalid boot token" }, { status }),
    });
    await expect(bootLegionClaim(host)).resolves.toBeUndefined();
    expect(steps).toEqual([`claims/register ${JSON.stringify(registration)}`, "stop"]);
    expect(lines).toEqual([`claims/register registration failed (${status}): Invalid boot token`]);
  });
}

test("a 5xx at claims/register is logged and rejects without stopping; a request that never reached the daemon rejects unlogged", async () => {
  const failed = claimHost({
    register: () => Response.json({ error: "register failed" }, { status: 503 }),
  });
  await expect(bootLegionClaim(failed.host)).rejects.toThrow(
    "POST /legion/v1/claims/register failed with 503: register failed"
  );
  expect(failed.steps).not.toContain("stop");
  expect(failed.lines).toEqual(["claims/register registration failed (503): register failed"]);

  const unreachable = claimHost({
    register: () => {
      throw new TypeError("fetch failed");
    },
  });
  await expect(bootLegionClaim(unreachable.host)).rejects.toThrow("fetch failed");
  expect(unreachable.steps).not.toContain("stop");
  expect(unreachable.lines).toEqual([]);
});

// An outage (`@legion/contracts/http-status`: a 5xx, a 408 or a 429) is no answer about the
// registration or the ready. The daemon writes no 408 and no 429, so a proxy between the session
// and the daemon sends either; the stand-in answers each as the daemon client's caller sees it.
const OUTAGES = [408, 429, 500, 501, 503] as const;

for (const status of OUTAGES) {
  test(`a ${status} at claims/register is logged and rejects without stopping`, async () => {
    const { host, steps, lines } = claimHost({
      register: () => Response.json({ error: "register failed" }, { status }),
    });
    await expect(bootLegionClaim(host)).rejects.toThrow(
      `POST /legion/v1/claims/register failed with ${status}: register failed`
    );
    expect(steps).toEqual([`claims/register ${JSON.stringify(registration)}`]);
    expect(lines).toEqual([`claims/register registration failed (${status}): register failed`]);
  });

  test(`a ${status} at claims/ready is sent again, and the boot goes on to the read of the claim's value and the claim once ready answers`, async () => {
    let readies = 0;
    const { host, steps } = claimHost({
      ready: () =>
        ++readies === 1
          ? Response.json({ error: "ready failed" }, { status })
          : new Response(null, { status: 204 }),
    });
    await expect(bootLegionClaim(host)).resolves.toBeDefined();
    expect(steps).toEqual([
      `claims/register ${JSON.stringify(registration)}`,
      "setCapability",
      `claims/ready ${readyBody()}`,
      `claims/ready ${readyBody()}`,
      `claims/task ${JSON.stringify(caller())}`,
      "taskValue",
      "afterReadyRead",
      `claimRole ${claimToken}`,
      "onRoleRegained",
    ]);
  });
}

test("a claim the listener refuses is logged naming the claim token and what stop does, then stops the agent — after the daemon has been told ready", async () => {
  const { host, steps, lines } = claimHost({
    overrides: {
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        throw new Error(`Envoy role ${role} is held by live session ses_other`);
      },
    },
  });

  await expect(bootLegionClaim(host)).resolves.toBeUndefined();

  // The relaunch reads its claim's value at its own registration, so a task the daemon owes
  // this claim is taken by the process that replaces it.
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "setCapability",
    `claims/ready ${readyBody()}`,
    `claims/task ${JSON.stringify(caller())}`,
    "taskValue",
    "afterReadyRead",
    `claimRole ${claimToken}`,
    "stop",
  ]);
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${claimToken}; exiting so the daemon launches it again: Envoy role ${claimToken} is held by live session ses_other`,
  ]);
});

/** The listener's 503 to a role claim whose takeover record did not store, as envoy-client's
 * `setRole` rejects with it after its own two tries. */
function claimAnswered503(): EnvoyApiError {
  return new EnvoyApiError({
    method: "POST",
    url: "http://listener.test/v1/roles/set",
    status: 503,
    responseBody: JSON.stringify({
      error: "envoy: the message bus refused the message because its stream is full",
    }),
  });
}

test("a claim the listener answers 503 is claimed again after a growing wait, and the session is not stopped; the claim lands on the try after the 503s end", async () => {
  let answered503 = 0;
  const { host, steps, lines } = claimHost({
    overrides: {
      roleClaimRetryFirstDelayMs: 1,
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        if (answered503 < 3) {
          answered503++;
          throw claimAnswered503();
        }
      },
    },
  });

  // The role is claimed right after the read that follows the ready, so the boot answers once
  // the claim has landed, and only then hears a regain.
  await expect(bootLegionClaim(host)).resolves.toBeDefined();

  expect(steps).not.toContain("stop");
  expect(steps.filter((step) => step === `claimRole ${claimToken}`)).toHaveLength(4);
  expect(steps.slice(-2)).toEqual([`claimRole ${claimToken}`, "onRoleRegained"]);
  expect(lines.map((line) => line.replace(/: .*$/, ""))).toEqual([
    `claim of Envoy role ${claimToken} was answered 503 at attempt 1; claiming it again in 1 ms`,
    `claim of Envoy role ${claimToken} was answered 503 at attempt 2; claiming it again in 2 ms`,
    `claim of Envoy role ${claimToken} was answered 503 at attempt 3; claiming it again in 4 ms`,
  ]);
  for (const line of lines) {
    expect(line).toContain("the message bus refused the message because its stream is full");
  }
});

test("control: a claim the listener refuses with any other status still stops the session at once, with no retry", async () => {
  const { host, steps, lines } = claimHost({
    overrides: {
      roleClaimRetryFirstDelayMs: 1,
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        throw new EnvoyApiError({
          method: "POST",
          url: "http://listener.test/v1/roles/set",
          status: 500,
          responseBody: JSON.stringify({ error: "envoy: the role claim failed" }),
        });
      },
    },
  });

  await expect(bootLegionClaim(host)).resolves.toBeUndefined();

  expect(steps.slice(-2)).toEqual([`claimRole ${claimToken}`, "stop"]);
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${claimToken}; exiting so the daemon launches it again: envoy: the role claim failed`,
  ]);
});

test("a host that stops while a 503 claim waits to be made again claims no more and stops nothing", async () => {
  const stopping = new AbortController();
  const firstClaim = Promise.withResolvers<void>();
  let claims = 0;
  const { host, steps } = claimHost({
    overrides: {
      // A wait no run of this test reaches: the abort ends it.
      roleClaimRetryFirstDelayMs: 600_000,
      signal: stopping.signal,
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        claims++;
        firstClaim.resolve();
        throw claimAnswered503();
      },
    },
  });
  const booting = bootLegionClaim(host);

  await firstClaim.promise;
  stopping.abort();
  await expect(booting).resolves.toBeUndefined();

  expect(claims).toBe(1);
  expect(steps).not.toContain("stop");
  expect(steps).not.toContain("onRoleRegained");
});

// #4387's handover meets the take: a role claim the listener answered 503 is made again after the
// ready and the read after it, so the take holds the task the ready decision wrote, and its mark,
// while the claim waits. When the claim lands, the listener hands the role over with the messages
// its previous holder did not read, and each finds the task owed and starts no turn before it. The
// task value is no role message (the listener relays it on the session's own subject), so the
// handover neither replays it nor drops it: the take's own text is the session's one first turn.
test("a role claim the listener answers 503 is made again with the take's mark up, so the messages the previous holder did not read, handed over when it lands, start no turn before the task", async () => {
  const written = { number: 5, epoch: "2026-11-13T16:21:44.123456789Z", revision: 13 };
  let answered503 = 0;
  const host = takingHost({
    register: () => Response.json({ ...registered, task: null, finished: 4 }),
    task: () => Response.json({ task: written, finished: 4 }),
    claimRole: async () => {
      if (answered503 < 2) {
        answered503++;
        throw claimAnswered503();
      }
      host.card("a message the previous holder did not read");
    },
  });

  await expect(bootLegionClaim(host.host)).resolves.toBeDefined();

  expect(answered503).toBe(2);
  expect(host.turns).toEqual([]);
  expect(envoyPluginInterface().taskTurnPending).toEqual({ session: SESSION });
  host.take()?.ready();
  await flush();
  expect(host.turns).toEqual([TASK_TEXT]);
  expect(host.steps).not.toContain("stop");
  resetEnvoyPluginInterfaceForTests();
});

test("the daemon-launched controller's claim is made again on a 503 with the same helper, and any other refusal is thrown at once", async () => {
  const lines: string[] = [];
  let tries = 0;
  await claimRoleAgainOnBusy(
    async () => {
      if (++tries < 3) throw claimAnswered503();
    },
    "controller",
    { log: (line) => lines.push(line), firstDelayMs: 1 }
  );
  expect(tries).toBe(3);
  expect(lines).toHaveLength(2);

  const refused = new Error("Envoy role controller is held by live session ses_other");
  await expect(
    claimRoleAgainOnBusy(
      async () => {
        throw refused;
      },
      "controller",
      { log: (line) => lines.push(line), firstDelayMs: 1 }
    )
  ).rejects.toBe(refused);
  expect(lines).toHaveLength(2);
});

test("a claim a person waits on gives up after its counted attempts, throwing the listener's 503; the control, with no bound, lands", async () => {
  const lines: string[] = [];
  let tries = 0;
  const busy = claimAnswered503();
  await expect(
    claimRoleAgainOnBusy(
      async () => {
        tries++;
        throw busy;
      },
      "controller",
      { log: (line) => lines.push(line), firstDelayMs: 1, maxAttempts: 3 }
    )
  ).rejects.toBe(busy);
  expect(tries).toBe(3);
  expect(lines).toHaveLength(2);

  let unbounded = 0;
  await claimRoleAgainOnBusy(
    async () => {
      if (++unbounded < 6) throw claimAnswered503();
    },
    "controller",
    { log: () => {}, firstDelayMs: 1 }
  );
  expect(unbounded).toBe(6);
});

/** The listener's 503 to a role claim with `body`, as envoy-client's `setRole` rejects with it. */
function claimAnswered503With(body: Record<string, string>): EnvoyApiError {
  return new EnvoyApiError({
    method: "POST",
    url: "http://listener.test/v1/roles/set",
    status: 503,
    responseBody: JSON.stringify(body),
  });
}

// The one claim 503 that is no claim holding nothing: the claim landed and stands, and only the
// handover of the session it replaces is not recorded. The listener names it by its code alone:
// the same text with no code, or with another code, reads as a claim 503 that wrote nothing. Each
// wait's log line hands the caller the 503 to tell them apart.
test("a claim 503 whose claim stands is told from one that wrote nothing, and each wait hands its 503 to the log", async () => {
  const handoverText =
    "the claim landed, but the handover record of the session it replaces did not store, so claim again";
  const handover = claimAnswered503With({ error: handoverText, code: "handover_unrecorded" });
  expect(claimStandsWithoutHandover(handover)).toBe(true);
  expect(claimStandsWithoutHandover(claimAnswered503())).toBe(false);
  // The code names its 503 by itself: the handover's text with no code, or with any other code,
  // is not this one.
  expect(claimStandsWithoutHandover(claimAnswered503With({ error: handoverText }))).toBe(false);
  expect(
    claimStandsWithoutHandover(claimAnswered503With({ error: handoverText, code: "another_code" }))
  ).toBe(false);
  expect(claimStandsWithoutHandover(new Error(handoverText))).toBe(false);

  const handed: unknown[] = [];
  let tries = 0;
  await claimRoleAgainOnBusy(
    async () => {
      if (++tries < 2) throw handover;
    },
    "controller",
    { log: (_line, error) => handed.push(error), firstDelayMs: 1 }
  );
  expect(handed).toEqual([handover]);
});

test("a 4xx at claims/ready is not retried: one ready, then the agent stops, with no role claim and no regain listener", async () => {
  const { host, steps } = claimHost({
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });
  await expect(bootLegionClaim(host)).resolves.toBeUndefined();
  expect(steps.filter((step) => step.startsWith("claims/ready"))).toHaveLength(1);
  expect(steps.at(-1)).toBe("stop");
  expect(steps).not.toContain("afterReadyRead");
  expect(steps).not.toContain("onRoleRegained");
  expect(steps.some((step) => step.startsWith("claimRole"))).toBe(false);
});

test("a ready retried after a 503 reads the value and claims the role as soon as it answers", async () => {
  let readies = 0;
  const { host, steps } = claimHost({
    ready: () => {
      readies += 1;
      if (readies > 1) return new Response(null, { status: 204 });
      return Response.json({ error: "the daemon is restarting" }, { status: 503 });
    },
  });

  await bootLegionClaim(host);

  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "setCapability",
    `claims/ready ${readyBody()}`,
    `claims/ready ${readyBody()}`,
    `claims/task ${JSON.stringify(caller())}`,
    "taskValue",
    "afterReadyRead",
    `claimRole ${claimToken}`,
    "onRoleRegained",
  ]);
});

// Contract entry 20261125170509: the registration names the session this one replaces, the claim's
// session the daemon forgot, and the role claim carries it, so the listener hands that session's
// unread messages here. The claim is made where the boot makes it, right after the ready and the
// read after it; a claim the listener answered 503 is made again naming it again.
test("the role claim names the session the registration says this one replaces, on every try", async () => {
  let answered503 = 0;
  const claims: [string, string | undefined][] = [];
  const { host, steps } = claimHost({
    register: () => Response.json({ ...registered, replaces: "ses_replaced" }),
    overrides: {
      roleClaimRetryFirstDelayMs: 1,
      claimRole: async (role, replaces) => {
        steps.push(`claimRole ${role}`);
        claims.push([role, replaces]);
        if (answered503++ === 0) throw claimAnswered503();
      },
    },
  });

  await expect(bootLegionClaim(host)).resolves.toBeDefined();

  expect(claims).toEqual([
    [claimToken, "ses_replaced"],
    [claimToken, "ses_replaced"],
  ]);
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "setCapability",
    `claims/ready ${readyBody()}`,
    `claims/task ${JSON.stringify(caller())}`,
    "taskValue",
    "afterReadyRead",
    `claimRole ${claimToken}`,
    `claimRole ${claimToken}`,
    "onRoleRegained",
  ]);
});

test("control: a registration that replaces no session claims naming none", async () => {
  const claims: [string, string | undefined][] = [];
  const { replaces: _none, ...golden } = registered;
  const { host } = claimHost({
    register: () => Response.json(golden),
    overrides: {
      claimRole: async (role, replaces) => {
        claims.push([role, replaces]);
      },
    },
  });

  await expect(bootLegionClaim(host)).resolves.toBeDefined();

  expect(claims).toEqual([[claimToken, undefined]]);
});
