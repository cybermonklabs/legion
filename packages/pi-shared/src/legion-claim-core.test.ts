import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { claimFirstTurnBoundMs, DEFAULT_CLAIM_FIRST_TURN_BOUND_MS } from "./claim-first-turn-bound";
import {
  CLAIM_BOOT_ATTEMPT_TIMEOUT_MS,
  CLAIM_BOOT_BUDGET_MS,
  createLegionDaemonClient,
} from "./daemon-client";
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

/** Who the pane's environment says this process is, as every ending's line names it. */
const identity = {
  role: "implementer",
  issue: "INFRA-2883",
  tree: "INFRA-2000",
  daemonUrl: "http://daemon.test",
};

/** An answer the daemon never sends: the attempt's own bound is what ends the call. */
const hangs = (): Promise<Response> => Promise.withResolvers<Response>().promise;

/** The failure a request to a daemon nothing is listening for throws. */
function connectionRefused(): Error {
  return Object.assign(new Error("Unable to connect. Is the computer able to access the url?"), {
    code: "ECONNREFUSED",
  });
}

/** Rejects when `signal` aborts, with what aborted it, as fetch rejects a request whose signal
 * aborted; it never settles otherwise, so a case can hand back an answer that never comes. */
function aborts(signal: AbortSignal | null | undefined): Promise<never> {
  const { promise, reject } = Promise.withResolvers<never>();
  signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
  return promise;
}

/** A host over a stub daemon whose `register` and `ready` answer as given, recording every step
 * it is asked for, in order, beside the daemon's requests. The boot registers its first-turn
 * signal at the registration, before `claims/ready`, and this host keeps it rather than firing
 * it: a case fires it with `firstTurn()`, and a case that leaves it unfired is a session no turn
 * ever started.
 *
 * Its bounds are its own, small ones: production's are 6 and 20 seconds, which no test waits
 * out. A case that counts attempts or spends the budget sets its own in `overrides`. */
function claimHost(options: {
  readonly register?: () => Response | Promise<Response>;
  readonly ready?: () => Response | Promise<Response>;
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
    const answer =
      route === "claims/register"
        ? (options.register?.() ?? Response.json(registered))
        : (options.ready?.() ?? new Response(null, { status: 204 }));
    return await Promise.race([answer, aborts(init?.signal)]);
  }) as typeof fetch;
  const host: LegionClaimHost = {
    daemon: createLegionDaemonClient("http://daemon.test", stub),
    registration,
    identity,
    bounds: { attemptMs: 40, budgetMs: 200 },
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

/** What every ending that is not a registered claim shows: `stop` called exactly once, and one
 * line naming the role, the issue, the tree, the daemon's address, the call that failed and the
 * cause — and naming the claim where one was registered. The line carries no boot token and no
 * secret: it reaches a log an operator reads. */
function expectEnding(
  booted: { readonly steps: string[]; readonly lines: string[] },
  expected: { readonly call: string; readonly cause: string; readonly claim?: string }
): void {
  expect(booted.steps.filter((step) => step === "stop")).toEqual(["stop"]);
  expect(booted.lines).toHaveLength(1);
  const line = booted.lines[0] ?? "";
  expect(line).toStartWith(
    `claim boot failed: role ${identity.role}, issue ${identity.issue}, tree ${identity.tree}, daemon ${identity.daemonUrl}; ${expected.call} failed: `
  );
  expect(line).toInclude(expected.cause);
  expect(line).toEndWith(
    `; ${expected.claim === undefined ? "no claim was registered" : `the claim ${expected.claim} was registered`}; exiting so the daemon launches it again`
  );
  expect(line).not.toInclude(registration.bootToken);
  expect(line).not.toInclude(String(registered.secret));
}

const readyBody = () =>
  JSON.stringify({
    claimToken,
    sessionId: "ses_claim",
    secret: registered.secret,
    generation: registered.generation,
  });

/** The register requests the stub was sent, which count a retried call's attempts. */
const registers = (steps: string[]): string[] =>
  steps.filter((step) => step.startsWith("claims/register"));

/** The ready requests the stub was sent, the same. */
const readies = (steps: string[]): string[] =>
  steps.filter((step) => step.startsWith("claims/ready"));

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
  test(`a ${status} at claims/register is logged and stops the agent, claiming nothing, and is never sent again`, async () => {
    const booted = claimHost({
      register: () => Response.json({ error: "Invalid boot token" }, { status }),
    });
    await bootLegionClaim(booted.host);
    expect(booted.steps).toEqual([`claims/register ${JSON.stringify(registration)}`, "stop"]);
    expectEnding(booted, {
      call: "claims/register",
      cause: `POST /legion/v1/claims/register failed with ${status}: Invalid boot token`,
    });
  });
}

test("a 5xx at claims/register is retried until the budget is spent, then logged and stops the agent, with the last failure in the line", async () => {
  const booted = claimHost({
    register: () => Response.json({ error: "register failed" }, { status: 503 }),
    overrides: { bounds: { attemptMs: 20, budgetMs: 120 } },
  });

  await bootLegionClaim(booted.host);

  expect(registers(booted.steps).length).toBeGreaterThan(1);
  expectEnding(booted, {
    call: "claims/register",
    cause:
      "the last attempt failed with: POST /legion/v1/claims/register failed with 503: register failed",
  });
});

test("a registration answered below 400 with a body the contract does not describe stops the agent and is never sent again", async () => {
  // A 200 the daemon API contract does not describe: a success that is not one. It is the
  // daemon's own answer, so no retry can change it.
  const booted = claimHost({ register: () => Response.json({ generation: 1 }) });

  await bootLegionClaim(booted.host);

  expect(registers(booted.steps)).toHaveLength(1);
  expectEnding(booted, {
    call: "claims/register",
    cause: "POST /legion/v1/claims/register answered 200 with a body the daemon API contract",
  });
});

test("a registration that never reaches the daemon is retried, then logged and stops the agent", async () => {
  const booted = claimHost({
    register: () => {
      throw connectionRefused();
    },
    overrides: { bounds: { attemptMs: 20, budgetMs: 120 } },
  });

  await bootLegionClaim(booted.host);

  expect(registers(booted.steps).length).toBeGreaterThan(1);
  // The line carries the failure's own sentence, which is what a reader of the log sees of an
  // ECONNREFUSED: the code is what classified it as one to retry.
  expectEnding(booted, { call: "claims/register", cause: "Unable to connect" });
});

test("a claims/register the daemon never answers is bounded, retried and then stops the agent", async () => {
  const booted = claimHost({
    register: hangs,
    overrides: { bounds: { attemptMs: 20, budgetMs: 120 } },
  });

  const started = Date.now();
  await bootLegionClaim(booted.host);

  // Each attempt ended at its own bound rather than hanging, and the whole boot ended inside
  // its budget: a hung daemon no longer leaves a session alive with no claim.
  expect(registers(booted.steps).length).toBeGreaterThan(1);
  expect(Date.now() - started).toBeLessThan(2_000);
  expectEnding(booted, { call: "claims/register", cause: "The operation timed out" });
});

test("a claims/ready the daemon never answers is bounded, retried and then stops the agent, naming the claim the registration issued", async () => {
  const booted = claimHost({
    ready: hangs,
    overrides: { bounds: { attemptMs: 20, budgetMs: 120 } },
  });

  await bootLegionClaim(booted.host);

  expect(readies(booted.steps).length).toBeGreaterThan(1);
  expect(booted.steps).not.toContain("onRoleRegained");
  expectEnding(booted, {
    call: "claims/ready",
    cause: "The operation timed out",
    claim: claimToken,
  });
});

test("the budget covers every attempt together: several of them spend it and the boot ends", async () => {
  const booted = claimHost({
    register: () => Response.json({ error: "the daemon is restarting" }, { status: 500 }),
    overrides: { bounds: { attemptMs: 5, budgetMs: 120 } },
  });

  const started = Date.now();
  await bootLegionClaim(booted.host);
  const spent = Date.now() - started;

  expect(registers(booted.steps).length).toBeGreaterThanOrEqual(3);
  // The budget is what ended it, not an attempt count: it ended at the budget, not before it.
  expect(spent).toBeGreaterThanOrEqual(100);
  expect(spent).toBeLessThan(2_000);
  expectEnding(booted, {
    call: "claims/register",
    cause: "got no answer inside the claim boot's budget of 120 ms, which is spent",
  });
});

test("a 5xx the daemon stops sending is retried until it answers: the third attempt registers, and nothing stops", async () => {
  let attempts = 0;
  const booted = claimHost({
    register: () => {
      attempts += 1;
      return attempts > 2
        ? Response.json(registered)
        : Response.json({ error: "the daemon is restarting" }, { status: 503 });
    },
    overrides: { bounds: { attemptMs: 40, budgetMs: 2_000 } },
  });

  const claim = await bootLegionClaim(booted.host);
  booted.firstTurn()?.();
  await claim.claimed;

  expect(registers(booted.steps)).toHaveLength(3);
  expect(booted.steps).not.toContain("stop");
  expect(booted.lines).toEqual([]);
  expect(booted.steps.at(-1)).toBe(`claimRole ${claimToken}`);
});

test("a failure of afterRegister stops the agent, naming the step and the claim", async () => {
  const booted = claimHost({
    overrides: {
      afterRegister: async () => {
        throw new Error("jj config path --user found no user configuration");
      },
    },
  });

  await bootLegionClaim(booted.host);

  expect(booted.steps).not.toContain("setCapability");
  expectEnding(booted, {
    call: "afterRegister",
    cause: "jj config path --user found no user configuration",
    claim: claimToken,
  });
});

test("an afterRegister that outlives the budget stops the agent rather than waiting it out", async () => {
  const booted = claimHost({
    overrides: {
      // A hook that never settles: `jj config path --user` has a 10 second bound of its own,
      // which is longer than what the boot can spare once the registration has answered.
      afterRegister: () => Promise.withResolvers<void>().promise,
      bounds: { attemptMs: 20, budgetMs: 80 },
    },
  });

  const started = Date.now();
  await bootLegionClaim(booted.host);

  expect(Date.now() - started).toBeLessThan(2_000);
  expect(booted.steps).not.toContain("setCapability");
  expectEnding(booted, {
    call: "afterRegister",
    cause: "it outlived the claim boot's budget of 80 ms, which is spent",
    claim: claimToken,
  });
});

test("a failure of setCapability stops the agent, naming the step and the claim", async () => {
  const booted = claimHost({
    overrides: {
      setCapability: () => {
        throw new Error("the capability of another session is already recorded");
      },
    },
  });

  await bootLegionClaim(booted.host);

  expect(readies(booted.steps)).toHaveLength(0);
  expectEnding(booted, {
    call: "setCapability",
    cause: "the capability of another session is already recorded",
    claim: claimToken,
  });
});

test("a claim the listener refuses is logged naming the claim token and what stop does, then stops the agent — after the daemon has been told ready", async () => {
  const booted = claimHost({
    overrides: {
      claimRole: async (role) => {
        booted.steps.push(`claimRole ${role}`);
        throw new Error(`Envoy role ${role} is held by live session ses_other`);
      },
    },
  });
  const claim = await bootLegionClaim(booted.host);

  booted.firstTurn()?.();
  await claim.claimed;

  // The refusal now comes after ready, so the daemon may already have delivered this claim's
  // task: the relaunch re-sends a delivery that is pending and unconfirmed, the path a death
  // inside a turn already takes. The failure itself is the one it always was.
  expect(booted.steps).toEqual([
    `claims/register ${JSON.stringify(registration)}`,
    "onFirstTurn",
    "setCapability",
    `claims/ready ${readyBody()}`,
    "onRoleRegained",
    `claimRole ${claimToken}`,
    "stop",
  ]);
  expectEnding(booted, {
    call: "claimRole",
    cause: `Envoy role ${claimToken} is held by live session ses_other`,
    claim: claimToken,
  });
});

test("a 4xx at claims/ready is not retried: one ready, then the agent stops, with no regain listener and no claim, whatever turn the registered signal reports", async () => {
  const booted = claimHost({
    ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
  });
  const claim = await bootLegionClaim(booted.host);
  expect(readies(booted.steps)).toHaveLength(1);
  expect(booted.steps.at(-1)).toBe("stop");
  expect(booted.steps).not.toContain("onRoleRegained");
  // The signal was registered at the registration, before this ready, so the host has it; the
  // boot stopped before it ever handed it a step, and a turn that starts now claims nothing.
  expect(booted.steps).toContain("onFirstTurn");
  booted.firstTurn()?.();
  expect(booted.steps.some((step) => step.startsWith("claimRole"))).toBe(false);
  expectEnding(booted, {
    call: "claims/ready",
    cause: "POST /legion/v1/claims/ready failed with 403: Invalid session secret",
    claim: claimToken,
  });
  // A boot that stopped before the claim step still settles its answer, so a caller that waits
  // on the claim is not left waiting for a step that is never coming.
  await claim.claimed;
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
  let readyAnswers = 0;
  let startTurn: (() => void) | undefined;
  const { host, steps, firstTurn } = claimHost({
    ready: () => {
      readyAnswers += 1;
      if (readyAnswers > 1) return new Response(null, { status: 204 });
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

test("the boot's two bounds hold its policy: an attempt above the daemon's own bound and below the budget, and a budget below the host's handler bound", () => {
  // The numbers are read from the package, so moving either one moves this row with it rather
  // than the policy silently.
  expect(CLAIM_BOOT_ATTEMPT_TIMEOUT_MS).toBeGreaterThan(5_000);
  expect(CLAIM_BOOT_ATTEMPT_TIMEOUT_MS).toBeLessThan(CLAIM_BOOT_BUDGET_MS);
  expect(CLAIM_BOOT_BUDGET_MS).toBeLessThan(25_000);
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
