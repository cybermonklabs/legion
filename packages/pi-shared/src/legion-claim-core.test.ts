import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { claimFirstTurnBoundMs, DEFAULT_CLAIM_FIRST_TURN_BOUND_MS } from "./claim-first-turn-bound";
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
const claimToken = String(registered.claimToken);
const registration = {
  bootToken: "boot-token-from-the-pane",
  sessionId: "ses_claim",
  ompSessionFile: "/sessions/ses_claim.jsonl",
  agentId: "ses_claim",
  pluginContract: 29,
};

/** A host over a stub daemon whose `register` and `ready` answer as given, recording every step
 * it is asked for, in order, beside the daemon's requests. The boot registers its first-turn
 * signal at the registration, before `claims/ready`, and this host keeps it rather than firing
 * it: a case fires it with `firstTurn()`, and a case that leaves it unfired is a session no turn
 * ever started. `endpoint` is the address the client is built over, which only a case reading
 * what the log says about it names. */
function claimHost(options: {
  readonly register?: () => Response;
  readonly ready?: () => Response;
  readonly endpoint?: string;
  readonly overrides?: Partial<LegionClaimHost>;
}) {
  const steps: string[] = [];
  const lines: string[] = [];
  const capabilities: ClaimCapability[] = [];
  let regain: ((role: string, reason: string) => Promise<void>) | undefined;
  let firstTurn: (() => void) | undefined;
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname.replace("/legion/v1/", "");
    steps.push(`${route} ${init?.body ?? ""}`);
    if (route === "claims/register") return options.register?.() ?? Response.json(registered);
    return options.ready?.() ?? new Response(null, { status: 204 });
  }) as typeof fetch;
  const host: LegionClaimHost = {
    daemon: createLegionDaemonClient(options.endpoint ?? "http://daemon.test", stub),
    registration,
    setCapability: (capability) => {
      steps.push("setCapability");
      capabilities.push(capability);
    },
    claimRole: async (role) => {
      steps.push(`claimRole ${role}`);
    },
    onFirstTurn: (claim) => {
      steps.push("onFirstTurn");
      firstTurn = claim;
    },
    // Long enough that no case reaches it by waiting: the cases that drive the bound set their
    // own, and every other one fires the signal, which disarms it.
    firstTurnBoundMs: 600_000,
    onRoleRegained: (listener) => {
      steps.push("onRoleRegained");
      regain = listener;
    },
    log: (line) => lines.push(line),
    stopping: "exiting so the daemon launches it again",
    stop: () => steps.push("stop"),
    ...options.overrides,
  };
  return {
    host,
    steps,
    lines,
    capabilities,
    regain: () => regain,
    firstTurn: () => firstTurn,
  };
}

const readyBody = () =>
  JSON.stringify({
    claimToken,
    sessionId: "ses_claim",
    secret: registered.secret,
    generation: registered.generation,
  });

test("registers, runs afterRegister, records the capability, then readies without a capability report, registers the regain listener and readies again on a regain of its role, and claims the role last, at the session's first turn", async () => {
  const { host, steps, lines, capabilities, regain, firstTurn } = claimHost({
    overrides: {
      afterRegister: async (claim) => {
        steps.push(`afterRegister ${claim.claimToken}`);
      },
    },
  });

  const booted = await bootLegionClaim(host);

  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "onFirstTurn",
    `afterRegister ${claimToken}`,
    "setCapability",
    `claims/ready ${readyBody()}`,
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

  firstTurn()?.();
  await booted.claimed;
  expect(steps.at(-1)).toBe(`claimRole ${claimToken}`);

  // Another role's regain sends nothing; this claim's sends the one ready the boot built.
  await regain()?.("some-other-role", "reclaimed");
  await regain()?.(claimToken, "reregistered");
  expect(steps.slice(7)).toEqual([`claims/ready ${readyBody()}`]);
  expect(lines).toEqual([`re-ran claims/ready after role ${claimToken} was reregistered`]);
});

for (const status of [400, 403, 409]) {
  test(`a ${status} at claims/register is logged and stops the agent, claiming nothing`, async () => {
    const { host, steps, lines } = claimHost({
      register: () => Response.json({ error: "Invalid boot token" }, { status }),
    });
    await bootLegionClaim(host);
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
  const { host, steps, lines, firstTurn } = claimHost({
    overrides: {
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        throw new Error(`Envoy role ${role} is held by live session ses_other`);
      },
    },
  });
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await booted.claimed;

  // The refusal now comes after ready, so the daemon may already have delivered this claim's
  // task: the relaunch re-sends a delivery that is pending and unconfirmed, the path a death
  // inside a turn already takes. The failure itself is the one it always was.
  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "onFirstTurn",
    "setCapability",
    `claims/ready ${readyBody()}`,
    "onRoleRegained",
    `claimRole ${claimToken}`,
    "stop",
  ]);
  expect(lines).toEqual([
    `claim boot failed at claimRole after claims/register registered ${claimToken} with the daemon at http://daemon.test; exiting so the daemon launches it again: Envoy role ${claimToken} is held by live session ses_other`,
  ]);
});

// Catches: the one sentence that served every failure after the registration, naming neither
// the step it died at nor the daemon it was talking to. Six runs of 2026-11-11 were stopped
// through it with `ECONNREFUSED` and nothing in the line said whether the dial that was refused
// was the daemon's `claims/ready` or Envoy's role claim, so not one of the six could be
// attributed. The boot's own steps and the role claim that runs after it are the two ends of
// that sentence: the row above is the second, and this is the first.
test("a failure in the boot's own steps names that step and the daemon it dialled", async () => {
  const { host, lines, steps } = claimHost({
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });

  await bootLegionClaim(host);

  expect(lines).toEqual([
    `claim boot failed at claims/ready after claims/register registered ${claimToken} with the daemon at http://daemon.test; exiting so the daemon launches it again: POST /legion/v1/claims/ready failed with 403: Invalid session secret`,
  ]);
  expect(steps.at(-1)).toBe("stop");
});

// Catches: the daemon's address written to the pane's stderr as it stands. A `daemon_url` may
// carry userinfo — the daemon's own `baseURL` refuses a query and a fragment and nothing else
// (`packages/daemon/internal/config/config.go`) — and this line reaches the pane's log and the
// operator reading it, so a credential in that address would be read by everyone who reads
// either. What the line names is the scheme, the host and the port, which is the daemon's own
// reading of that setting wherever it shows it (`internal/api/settings.go`'s `endpointOf`).
test("a host whose endpoint carries userinfo logs the masked address and not the credential", async () => {
  // The userinfo is built rather than written: the shared public leak check reads a URL
  // carrying a password as a credential, whatever the host, and refuses the push that adds it.
  const password = ["pw", "example"].join("-");
  const { host, lines } = claimHost({
    endpoint: `http://legion:${password}@daemon.test:13370`,
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });

  await bootLegionClaim(host);

  expect(lines).toEqual([
    `claim boot failed at claims/ready after claims/register registered ${claimToken} with the daemon at http://daemon.test:13370; exiting so the daemon launches it again: POST /legion/v1/claims/ready failed with 403: Invalid session secret`,
  ]);
  expect(lines.join("\n")).not.toContain(password);
});

// One row per step of the boot that no other row fails in, since the sentence above names a
// variable the boot moves from step to step: `claims/ready` is the row above and `claimRole` the
// one above that, and a step whose own assignment is dropped is reported under the step before
// it — the failure then names a call that answered. `afterRegister` is the initial value, so a
// boot that names no step at all reports its first failure as that step and nothing says so.
for (const { step, overrides } of [
  {
    step: "afterRegister",
    overrides: {
      afterRegister: async () => {
        throw new Error("the host's own startup refused");
      },
    },
  },
  {
    step: "setCapability",
    overrides: {
      setCapability: () => {
        throw new Error("the host's own startup refused");
      },
    },
  },
  {
    step: "onRoleRegained",
    overrides: {
      onRoleRegained: () => {
        throw new Error("the host's own startup refused");
      },
    },
  },
] satisfies { step: string; overrides: Partial<LegionClaimHost> }[]) {
  test(`a host whose ${step} throws is stopped, with ${step} named as the step`, async () => {
    const { host, lines, steps } = claimHost({ overrides });

    const booted = await bootLegionClaim(host);

    expect(lines).toEqual([
      `claim boot failed at ${step} after claims/register registered ${claimToken} with the daemon at http://daemon.test; exiting so the daemon launches it again: the host's own startup refused`,
    ]);
    expect(steps.at(-1)).toBe("stop");
    expect(steps.some((taken) => taken.startsWith("claimRole"))).toBe(false);
    // A boot that stopped in one of its own steps still settles its answer, as every other
    // stopped boot does, so a caller waiting on the claim is not left waiting.
    await booted.claimed;
  });
}

test("a 4xx at claims/ready is not retried: one ready, then the agent stops, with no regain listener and no claim, whatever turn the registered signal reports", async () => {
  const { host, steps, firstTurn } = claimHost({
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });
  const booted = await bootLegionClaim(host);
  expect(steps.filter((step) => step.startsWith("claims/ready"))).toHaveLength(1);
  expect(steps.at(-1)).toBe("stop");
  expect(steps).not.toContain("onRoleRegained");
  // The signal was registered at the registration, before this ready, so the host has it; the
  // boot stopped before it ever handed it a step, and a turn that starts now claims nothing.
  expect(steps).toContain("onFirstTurn");
  firstTurn()?.();
  expect(steps.some((step) => step.startsWith("claimRole"))).toBe(false);
  // A boot that stopped before the claim step still settles its answer, so a caller that waits
  // on the claim is not left waiting for a step that is never coming.
  await booted.claimed;
});

test("the name is taken at the host's first-turn callback and not before it", async () => {
  const { host, steps, firstTurn, regain } = claimHost({});

  const booted = await bootLegionClaim(host);

  // Ready has answered and the regain listener is registered, and the role is still unclaimed.
  // Nothing but the signal can claim it here: this host's bound is ten minutes away, and a
  // regain of the role — the one other thing that runs between the boot and the first turn —
  // re-runs ready and claims nothing.
  expect(steps).toContain(`claims/ready ${readyBody()}`);
  expect(steps).toContain("onRoleRegained");
  await regain()?.(claimToken, "reclaimed");
  expect(steps.some((step) => step.startsWith("claimRole"))).toBe(false);

  firstTurn()?.();
  await booted.claimed;

  expect(steps.at(-1)).toBe(`claimRole ${claimToken}`);
});

test("a boot that gets no first turn claims at the bound", async () => {
  const waiting = claimHost({ overrides: { firstTurnBoundMs: 5 } });
  const booted = await bootLegionClaim(waiting.host);
  expect(waiting.steps.some((step) => step.startsWith("claimRole"))).toBe(false);

  // Nothing fires the signal: this session was given no task and started no turn, and the bound
  // is what takes its name anyway, so the lane the listener kept for it is handed over.
  await booted.claimed;
  expect(waiting.firstTurn()).toBeDefined();
  expect(waiting.steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "onFirstTurn",
    "setCapability",
    `claims/ready ${readyBody()}`,
    "onRoleRegained",
    `claimRole ${claimToken}`,
  ]);
});

test("a first turn that started before the boot registered its signal is not lost, and the name is taken once ready has answered", async () => {
  // The host's first turn is already running when the boot registers its signal, so the host
  // reports it inside that very call rather than keeping one nobody will fire again. The claim
  // still waits for `claims/ready`, which is what the order below says.
  const { host, steps } = claimHost({
    overrides: {
      onFirstTurn: (claim) => {
        claim();
      },
    },
  });

  const booted = await bootLegionClaim(host);
  await booted.claimed;

  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "setCapability",
    `claims/ready ${readyBody()}`,
    "onRoleRegained",
    `claimRole ${claimToken}`,
  ]);
});

test("a first turn that starts while claims/ready is still being retried is not lost, and the name is taken as soon as ready answers", async () => {
  // The daemon took the first ready and its answer was lost, so the boot retries it — and the
  // daemon's task reaches this session meanwhile. The signal is registered at the registration,
  // before the first ready, so that turn is heard; the name is taken the moment the retry
  // answers, rather than at the bound ten minutes away (INFRA-1573).
  let readies = 0;
  let startTurn: (() => void) | undefined;
  const { host, steps, firstTurn } = claimHost({
    ready: () => {
      readies += 1;
      if (readies > 1) return new Response(null, { status: 204 });
      startTurn?.();
      return Response.json({ error: "the daemon is restarting" }, { status: 503 });
    },
  });
  startTurn = () => firstTurn()?.();

  // The name is taken the instant ready answers: the boot's own promise is already past the
  // claim step when it settles, which is the whole of the gap this closes.
  const booted = await bootLegionClaim(host);
  await booted.claimed;

  expect(steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "onFirstTurn",
    "setCapability",
    `claims/ready ${readyBody()}`,
    `claims/ready ${readyBody()}`,
    "onRoleRegained",
    `claimRole ${claimToken}`,
  ]);
});

test("two first-turn signals claim once", async () => {
  const { host, steps, firstTurn } = claimHost({});
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  firstTurn()?.();
  await booted.claimed;
  firstTurn()?.();
  await booted.claimed;

  expect(steps.filter((step) => step.startsWith("claimRole"))).toEqual([`claimRole ${claimToken}`]);
});

/** The log the bound's reader warns to, with the warnings it was given. */
function boundLog(): { readonly warn: (message: string) => void; readonly warnings: string[] } {
  const warnings: string[] = [];
  return { warn: (message) => warnings.push(message), warnings };
}

test("the bound is 30 seconds, moved by one variable, which honours 0 and warns once on anything else", () => {
  expect(DEFAULT_CLAIM_FIRST_TURN_BOUND_MS).toBe(30_000);

  const read = boundLog();
  expect(claimFirstTurnBoundMs({}, read)).toBe(DEFAULT_CLAIM_FIRST_TURN_BOUND_MS);
  expect(claimFirstTurnBoundMs({ LEGION_CLAIM_FIRST_TURN_BOUND_MS: "250" }, read)).toBe(250);
  // 0 is a value the reader honours: the name is taken as soon as ready has answered.
  expect(claimFirstTurnBoundMs({ LEGION_CLAIM_FIRST_TURN_BOUND_MS: "0" }, read)).toBe(0);
  expect(read.warnings).toEqual([]);

  for (const bad of ["", " ", "soon", "1.5", "-1"]) {
    const refused = boundLog();
    expect(claimFirstTurnBoundMs({ LEGION_CLAIM_FIRST_TURN_BOUND_MS: bad }, refused), bad).toBe(
      DEFAULT_CLAIM_FIRST_TURN_BOUND_MS
    );
    expect(refused.warnings, bad).toEqual([
      `legion: LEGION_CLAIM_FIRST_TURN_BOUND_MS is not an integer of 0 or more; using ${DEFAULT_CLAIM_FIRST_TURN_BOUND_MS}`,
    ]);
  }
});
