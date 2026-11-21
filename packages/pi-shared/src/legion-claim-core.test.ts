import { expect, jest, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { NoRelaySubjectError } from "@legion/envoy-client/consumer-delivery";
import { EnvoyApiError } from "@legion/envoy-client/transport";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { claimFirstTurnBoundMs, DEFAULT_CLAIM_FIRST_TURN_BOUND_MS } from "./claim-first-turn-bound";
import { createLegionDaemonClient, type DaemonRestartRetry } from "./daemon-client";
import {
  bootLegionClaim,
  claimAgainCures,
  claimAgainReason,
  claimRoleAgain,
  type LegionClaimHost,
} from "./legion-claim-core";
import type { ClaimCapability } from "./legion-phase-ops";

/** The contract the build under test speaks, which its client states on every request. */
const CONTRACT = 60;

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

const registered = daemonFixture("register.json");
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

/** The clock the boot's waits run on here: no case waits in real time, every sleep moves this
 * clock instead, and no attempt's limit fires unless the case's own `restart` fires it. */
function instantWaits(): DaemonRestartRetry {
  let clock = 0;
  return {
    now: () => clock,
    sleep: async (ms: number) => {
      clock += ms;
    },
    attemptLimit: () => new Promise<void>(() => {}),
  };
}

/** A host over a stub daemon whose `register` and `ready` answer as given, recording every step
 * it is asked for, in order, beside the daemon's requests. `register` is handed the attempt's
 * own signal, which a case about an attempt the daemon never answered reads. The boot registers
 * its first-turn signal at the registration, before `claims/ready`, and this host keeps it
 * rather than firing it: a case fires it with `firstTurn()`, and a case that leaves it unfired
 * is a session no turn ever started. */
function claimHost(options: {
  readonly register?: (attempt: AbortSignal | null | undefined) => Response | Promise<Response>;
  readonly ready?: () => Response;
  readonly restart?: DaemonRestartRetry;
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
    if (route === "claims/register")
      return options.register?.(init?.signal) ?? Response.json(registered);
    return options.ready?.() ?? new Response(null, { status: 204 });
  }) as typeof fetch;
  const host: LegionClaimHost = {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    registration,
    restart: options.restart ?? instantWaits(),
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

// A 5xx the daemon itself answered is the daemon serving and refusing, which no wait cures: it
// is logged and left to the daemon's registration deadline, as it always was. An uncoded 502,
// 503 or 504 is not such an answer — it is a proxy in front of a daemon that is not serving —
// and is waited out instead, below.
test("a 5xx the daemon itself answered at claims/register is logged and rejects without stopping", async () => {
  for (const [status, code] of [
    [500, undefined],
    [503, "DISPATCH_UNAVAILABLE"],
  ] as const) {
    const failed = claimHost({
      register: () =>
        Response.json(
          { error: "register failed", ...(code === undefined ? {} : { code }) },
          { status }
        ),
    });
    await expect(bootLegionClaim(failed.host)).rejects.toThrow(
      `POST /legion/v1/claims/register failed with ${status}`
    );
    expect(failed.steps).not.toContain("stop");
    expect(failed.lines).toEqual([
      `claims/register registration failed (${status}): register failed`,
    ]);
  }
});

test("claims/register waits for a daemon that is not serving and registers once it serves again, with nothing charged to the claim", async () => {
  let attempts = 0;
  const { host, steps, lines, capabilities } = claimHost({
    register: () => {
      attempts += 1;
      if (attempts === 1)
        throw Object.assign(new Error("connect ECONNREFUSED"), { code: "ECONNREFUSED" });
      if (attempts === 2) return Response.json({ error: "bad gateway" }, { status: 502 });
      if (attempts === 3) throw new TypeError("fetch failed");
      return Response.json(registered);
    },
  });

  // The name is taken at the first turn, which this case never fires: the boot itself is the
  // whole of what the wait had to finish.
  await bootLegionClaim(host);

  expect(attempts).toBe(4);
  expect(capabilities).toHaveLength(1);
  expect(steps).not.toContain("stop");
  expect(steps).toContain(`claims/ready ${readyBody()}`);
  expect(lines).toEqual([]);
});

test("claims/ready waits for a daemon that is not serving and readies once it serves again", async () => {
  let attempts = 0;
  const { host, steps } = claimHost({
    ready: () => {
      attempts += 1;
      if (attempts <= 2) return Response.json({ error: "bad gateway" }, { status: 503 });
      return new Response(null, { status: 204 });
    },
  });

  await bootLegionClaim(host);

  expect(attempts).toBe(3);
  expect(steps).not.toContain("stop");
  expect(steps.at(-1)).toBe("onRoleRegained");
});

// The daemon's register route runs to its end whatever the caller does
// (`packages/daemon/internal/api/claims.go`: "a caller that hangs up mid-request does not get to
// leave a registration half-recorded"), so what orders two attempts of one boot is this client:
// it ends the attempt it gave up on before it sends the next one, and the claim's machine takes
// them in that order. The worker therefore holds the secret of the registration the daemon
// applied last. This case holds the client's half, over a stub that applies the given-up
// registration when its attempt is ended; the daemon's half, a given-up registration the real
// server finishes after the client hung up while the repeat waits behind it, is
// `TestARegistrationTheClientGaveUpOnIsAppliedBeforeItsRepeat` in `packages/daemon/internal/api`.
test("a register attempt the daemon finishes after the client gave it up is ended before the next one is sent", async () => {
  const applied: string[] = [];
  let attempts = 0;
  const { host, capabilities } = claimHost({
    register: (attempt) => {
      attempts += 1;
      if (attempts === 1) {
        return new Promise<Response>(() => {
          attempt?.addEventListener("abort", () => applied.push("secret-1"), { once: true });
        });
      }
      applied.push("secret-2");
      return Response.json({ ...registered, secret: "secret-2" });
    },
    restart: {
      ...instantWaits(),
      attemptLimit: () => (attempts === 1 ? Promise.resolve() : new Promise<void>(() => {})),
    },
  });

  await bootLegionClaim(host);

  expect(applied).toEqual(["secret-1", "secret-2"]);
  expect(capabilities.at(0)?.secret).toBe("secret-2");
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
  const { host, steps, lines, firstTurn } = claimHost({
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
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await booted.claimed;

  expect(steps).not.toContain("stop");
  expect(steps.filter((step) => step === `claimRole ${claimToken}`)).toHaveLength(4);
  expect(lines.map((line) => line.replace(/: .*$/, ""))).toEqual([
    `claim of Envoy role ${claimToken} was answered 503 at attempt 1, so nothing was claimed; claiming it again in 1 ms`,
    `claim of Envoy role ${claimToken} was answered 503 at attempt 2, so nothing was claimed; claiming it again in 2 ms`,
    `claim of Envoy role ${claimToken} was answered 503 at attempt 3, so nothing was claimed; claiming it again in 4 ms`,
  ]);
  for (const line of lines) {
    expect(line).toContain("the message bus refused the message because its stream is full");
  }
});

test("control: a claim the listener refuses with any other status still stops the session at once, with no retry", async () => {
  const { host, steps, lines, firstTurn } = claimHost({
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
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await booted.claimed;

  expect(steps.slice(-2)).toEqual([`claimRole ${claimToken}`, "stop"]);
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${claimToken}; exiting so the daemon launches it again: envoy: the role claim failed`,
  ]);
});

test("a host that stops while a 503 claim waits to be made again claims no more and stops nothing", async () => {
  const stopping = new AbortController();
  const firstClaim = Promise.withResolvers<void>();
  let claims = 0;
  const { host, steps, firstTurn } = claimHost({
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
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await firstClaim.promise;
  stopping.abort();
  await booted.claimed;

  expect(claims).toBe(1);
  expect(steps).not.toContain("stop");
});

test("the daemon-launched controller's claim is made again on a 503 with the same helper, and any other refusal is thrown at once", async () => {
  const lines: string[] = [];
  let tries = 0;
  await claimRoleAgain(
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
    claimRoleAgain(
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
    claimRoleAgain(
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
  await claimRoleAgain(
    async () => {
      if (++unbounded < 6) throw claimAnswered503();
    },
    "controller",
    { log: () => {}, firstDelayMs: 1 }
  );
  expect(unbounded).toBe(6);
});

const NO_RELAY =
  "this session holds no relay subject, so the listener can give it no consumer and it can receive nothing, because its relay subject could not be opened: Authentication Timeout";

test("a claim refused because the session holds no relay subject is claimed again after a growing wait, in each shape pi-envoy's claim fails in, and the session is not stopped", async () => {
  let refusals = 3;
  const { host, steps, lines, firstTurn } = claimHost({
    overrides: {
      roleClaimRetryFirstDelayMs: 1,
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        if (refusals === 0) return;
        refusals -= 1;
        // The class itself (pi-envoy's registration call throws it bare), a plain error carrying
        // only its name (another plugin's copy of the class), and pi-envoy's session-pass
        // registration error, which carries the refusal as its cause.
        if (refusals === 2) throw new NoRelaySubjectError(NO_RELAY);
        if (refusals === 1)
          throw Object.assign(new Error(NO_RELAY), { name: "NoRelaySubjectError" });
        throw new Error(NO_RELAY, { cause: new NoRelaySubjectError(NO_RELAY) });
      },
    },
  });
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await booted.claimed;

  expect(steps).not.toContain("stop");
  expect(steps.filter((step) => step === `claimRole ${claimToken}`)).toHaveLength(4);
  expect(lines).toEqual(
    [1, 2, 4].map(
      (wait, index) =>
        `claim of Envoy role ${claimToken} was refused before the listener was asked, because this session holds no relay subject yet, at attempt ${index + 1}, so nothing was claimed; claiming it again in ${wait} ms: ${NO_RELAY}`
    )
  );
});

// The boot's claim has no count of tries: a pane whose NATS is down for minutes keeps claiming
// until NATS answers, where a bound would stop it mid-turn. Twelve refusals at waits from 1 ms
// pass the first round's eleven tries and the typed controller's five.
test("a claim refused for want of a relay subject twelve times is claimed again each time, never stops the session, and lands", async () => {
  let refusals = 12;
  const { host, steps, lines, firstTurn } = claimHost({
    overrides: {
      roleClaimRetryFirstDelayMs: 1,
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        if (refusals === 0) return;
        refusals -= 1;
        throw new NoRelaySubjectError(NO_RELAY);
      },
    },
  });
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await booted.claimed;

  expect(steps).not.toContain("stop");
  expect(steps.filter((step) => step === `claimRole ${claimToken}`)).toHaveLength(13);
  expect(lines).toHaveLength(12);
  expect(refusals).toBe(0);
}, 30_000);

test("control: a refusal whose cause is another error is no missing relay, and stops the session at once", async () => {
  const { host, steps, lines, firstTurn } = claimHost({
    overrides: {
      roleClaimRetryFirstDelayMs: 1,
      claimRole: async (role) => {
        steps.push(`claimRole ${role}`);
        throw new Error("registration refused", {
          cause: new Error("Envoy role is held by live session ses_other"),
        });
      },
    },
  });
  const booted = await bootLegionClaim(host);

  firstTurn()?.();
  await booted.claimed;

  expect(steps.filter((step) => step === `claimRole ${claimToken}`)).toHaveLength(1);
  expect(steps.at(-1)).toBe("stop");
  expect(lines).toHaveLength(1);
});

test("each claim again waits out the wait it logged before it claims", async () => {
  // The waits only lengthen under load, so their sum is a floor the claims cannot beat; a loop
  // that logged its waits and did not take them claims all four times within a millisecond.
  let tries = 0;
  const started = performance.now();
  await claimRoleAgain(
    async () => {
      if (++tries < 4) throw new NoRelaySubjectError(NO_RELAY);
    },
    "controller",
    { log: () => {}, firstDelayMs: 40 }
  );
  expect(tries).toBe(4);
  expect(performance.now() - started).toBeGreaterThanOrEqual(40 + 80 + 160 - 5);
});

test("why a claim is made again: the listener's 503 and a missing relay, and nothing else", () => {
  const conflict = new EnvoyApiError({
    method: "POST",
    url: "http://listener.test/v1/roles/set",
    status: 409,
    responseBody: JSON.stringify({ error: "envoy: the role is held by a live session" }),
  });
  expect(claimAgainReason(claimAnswered503())).toBe("busy");
  expect(claimAgainReason(new NoRelaySubjectError(NO_RELAY))).toBe("no-relay");
  expect(claimAgainReason(new Error(NO_RELAY))).toBeUndefined();
  expect(claimAgainReason(conflict)).toBeUndefined();
  expect(claimAgainCures(claimAnswered503())).toBe(true);
  expect(claimAgainCures(new NoRelaySubjectError(NO_RELAY))).toBe(true);
  expect(claimAgainCures(conflict)).toBe(false);
});

// The steady wait of a pane whose NATS stays down: the wait doubles from 1 s and stops at 30 s.
// Fake timers drive it, since the real waits would take minutes.
test("a claim made again waits 1, 2, 4, 8 and 16 s, then 30 s at most, however long the relay stays missing", async () => {
  jest.useFakeTimers();
  try {
    const lines: string[] = [];
    let tries = 0;
    const claimed = claimRoleAgain(
      async () => {
        if (++tries < 9) throw new NoRelaySubjectError(NO_RELAY);
      },
      "implementer",
      { log: (line) => lines.push(line) }
    );
    for (let turn = 0; turn < 8; turn++) {
      for (let flush = 0; flush < 5; flush++) await Promise.resolve();
      jest.advanceTimersByTime(30_000);
    }
    await claimed;
    expect(tries).toBe(9);
    expect(lines.map((line) => /claiming it again in (\d+) ms/.exec(line)?.[1])).toEqual([
      "1000",
      "2000",
      "4000",
      "8000",
      "16000",
      "30000",
      "30000",
      "30000",
    ]);
  } finally {
    jest.useRealTimers();
  }
});

test("a host that stopped before the first claim claims nothing", async () => {
  const stopped = new AbortController();
  stopped.abort();
  let tries = 0;
  await expect(
    claimRoleAgain(
      async () => {
        tries += 1;
      },
      "implementer",
      { log: () => {}, signal: stopped.signal }
    )
  ).rejects.toThrow("the host stopped first");
  expect(tries).toBe(0);
});

// A host that stops before the session's first turn and long before the bound: the bound's timer
// goes with the stop, and the claim step settles at the stop, having claimed nothing, rather than
// waiting out the bound for a session the host ended.
test("a host that stops before the first turn settles its claim step at the stop, and claims nothing", async () => {
  const stopping = new AbortController();
  const { host, steps } = claimHost({ overrides: { signal: stopping.signal } });
  const booted = await bootLegionClaim(host);
  stopping.abort();
  // The bound is the helper's 600 s, so only the stop can settle the step inside this window.
  const settled = await Promise.race([
    booted.claimed.then(() => "settled"),
    Bun.sleep(1_000).then(() => "still waiting"),
  ]);
  expect(settled).toBe("settled");
  expect(steps.filter((step) => step.startsWith("claimRole"))).toEqual([]);
  expect(steps).not.toContain("stop");
});

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
