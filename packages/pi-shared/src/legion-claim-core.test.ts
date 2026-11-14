import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import type { LegionTask } from "@legion/contracts/legion-api";
import { createLegionDaemonClient } from "./daemon-client";
import { bootLegionClaim, type LegionClaimHost } from "./legion-claim-core";
import type { ClaimCapability } from "./legion-phase-ops";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

const registered = daemonFixture("register.json");
const current = daemonFixture("task.json");
const claimToken = String(registered.claimToken);
const registration = {
  bootToken: "boot-token-from-the-pane",
  sessionId: "ses_claim",
  ompSessionFile: "/sessions/ses_claim.jsonl",
  agentId: "ses_claim",
  pluginContract: 29,
};

/** A host over a stub daemon whose `register`, `ready` and `task` answer as given, recording
 * every step it is asked for, in order, beside the daemon's requests, and every task value it is
 * handed. `taskValue: false` is a host that takes none. */
function claimHost(options: {
  readonly register?: () => Response;
  readonly ready?: () => Response;
  readonly task?: () => Response;
  readonly taskValue?: false;
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
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    registration,
    setCapability: (capability) => {
      steps.push("setCapability");
      capabilities.push(capability);
    },
    claimRole: async (role) => {
      steps.push(`claimRole ${role}`);
    },
    ...(options.taskValue === false
      ? {}
      : {
          taskValue: (reading: LegionTask) => {
            steps.push("taskValue");
            values.push(reading);
          },
        }),
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

test("registers, hands the registration to afterRegister, records the capability, readies without a capability report, claims the role right after, and reads the task value again on a regain of its role", async () => {
  const { host, steps, lines, capabilities, values, regain } = claimHost({
    overrides: {
      afterRegister: async (claim) => {
        steps.push(`afterRegister ${claim.claimToken} task ${claim.task?.number}`);
      },
    },
  });

  const boot = await bootLegionClaim(host);

  expect(boot).toBeDefined();
  // The registration's value goes to the host before the ready and the role claim: the take's
  // mark stands before the role's kept lane is handed over.
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    `afterRegister ${claimToken} task 4`,
    "setCapability",
    `claims/ready ${readyBody()}`,
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

  // Another role's regain reads nothing; this claim's reads its value, and sends no ready.
  await regain()?.("some-other-role", "reclaimed");
  await regain()?.(claimToken, "reregistered");
  expect(steps.slice(6)).toEqual([`claims/task ${JSON.stringify(caller())}`, "taskValue"]);
  expect(values).toEqual([current as LegionTask]);
  expect(lines).toEqual([`read claims/task after role ${claimToken} was reregistered`]);
});

test("reads the claim's value at the host's ask, at a new epoch of its relay, and a failed read is logged and never rejects", async () => {
  let answer = Response.json(current);
  const { host, steps, lines, values } = claimHost({ task: () => answer });
  const boot = await bootLegionClaim(host);

  await boot?.readTask();
  expect(values).toEqual([current as LegionTask]);

  answer = Response.json({ error: "the daemon is restarting" }, { status: 503 });
  await expect(boot?.readTask()).resolves.toBeUndefined();
  expect(values).toHaveLength(1);
  expect(steps.filter((step) => step.startsWith("claims/task"))).toHaveLength(2);
  expect(lines).toEqual([
    "claims/task at the relay's new epoch failed; the claim's value reaches this session with the daemon's next write of it: POST /legion/v1/claims/task failed with 503: the daemon is restarting",
  ]);
});

test("a host that takes no task value reads none, at a regain or at its ask", async () => {
  const { host, steps, regain } = claimHost({ taskValue: false });
  const boot = await bootLegionClaim(host);

  await regain()?.(claimToken, "reclaimed");
  await boot?.readTask();
  expect(steps.some((step) => step.startsWith("claims/task"))).toBe(false);
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
    `claimRole ${claimToken}`,
    "stop",
  ]);
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${claimToken}; exiting so the daemon launches it again: Envoy role ${claimToken} is held by live session ses_other`,
  ]);
});

test("a 4xx at claims/ready is not retried: one ready, then the agent stops, with no role claim and no regain listener", async () => {
  const { host, steps } = claimHost({
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });
  await expect(bootLegionClaim(host)).resolves.toBeUndefined();
  expect(steps.filter((step) => step.startsWith("claims/ready"))).toHaveLength(1);
  expect(steps.at(-1)).toBe("stop");
  expect(steps).not.toContain("onRoleRegained");
  expect(steps.some((step) => step.startsWith("claimRole"))).toBe(false);
});

test("a ready retried after a 503 claims the role as soon as it answers", async () => {
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
    `claimRole ${claimToken}`,
    "onRoleRegained",
  ]);
});
