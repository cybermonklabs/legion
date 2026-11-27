import { afterEach, beforeAll, describe, expect, it, jest, mock, spyOn } from "bun:test";
import { mkdirSync, readFileSync, symlinkSync } from "node:fs";
import path from "node:path";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { resetEnvoyPluginInterfaceForTests } from "@legion/pi-shared/interface";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { legionRoleClaimBridge } from "@legion/pi-shared/role-claim-bridge";
import { RoleClaimStoppingError } from "@legion/pi-shared/role-claim-stopping";
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { HOST_STOP_GRACE_MS } from "./host-stop";

// The module reaches the Oh My Pi host through `@legion/pi-shared/subagent-session`, which only
// the binary resolves; the import below must come after the mock.
mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));

const { attributionInputs, createClaimSession } = await import("./claim-session");

restoreEnvironment(["LEGION_BOOT_TOKEN", "LEGION_STATE_DIR", "LEGION_CLAIM_FIRST_TURN_BOUND_MS"]);
afterEach(() => resetEnvoyPluginInterfaceForTests());

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";

// The pane's workspace reaches `attributionInputs` as the daemon set it, and the overlay is
// scoped to that workspace's jj repository; jj matches a scope against the real path of the
// repository it loaded, so a link's own path must never reach the overlay. The path is reserved
// here and the directory made in the `beforeAll` the fixture registers, so a `bun test -t
// <pattern>` that selects no row of this file makes nothing and leaves nothing behind; the link
// and the workspace's store are made in a hook of this file's own, which Bun runs after the
// fixture's.
const REAL_WORKSPACE = temporaryDirectory("claim-session-ws", { real: true });
const WORKSPACE = path.join(REAL_WORKSPACE, "link");
const REPOSITORY = path.join(REAL_WORKSPACE, ".jj", "repo");
beforeAll(() => {
  mkdirSync(REPOSITORY, { recursive: true });
  symlinkSync(REAL_WORKSPACE, WORKSPACE);
});

describe("attributionInputs", () => {
  it("reads the session id, the state directory, the trailer key and the workspace's repository", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
        LEGION_WORKSPACE: WORKSPACE,
      })
    ).toEqual({
      sessionId: ROOT_ID,
      stateDir: "/state",
      issueKey: "ACME-7",
      repository: REPOSITORY,
    });
  });

  // jj matches a `[[--scope]]` against the real path of the repository it loaded, and a path that
  // does not resolve matches nothing and says nothing: the trailer is then the session line alone.
  it.each([
    ["no LEGION_WORKSPACE", undefined],
    ["an empty LEGION_WORKSPACE", ""],
    ["a workspace that does not exist", "/no/such/workspace"],
  ])("takes no repository from %s", (_name, workspace) => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
        ...(workspace === undefined ? {} : { LEGION_WORKSPACE: workspace }),
      }).repository
    ).toBeUndefined();
  });

  // The daemon sets LEGION_ISSUE for every pane; it sets LEGION_ISSUE_TRAILER only for an issue
  // whose repository the deployment names. A caller reading LEGION_ISSUE would stamp a Dispatch
  // key into the commits of a repository it never named (dispatch://INFRA-472).
  it("takes no key from LEGION_ISSUE", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, { LEGION_STATE_DIR: "/state", LEGION_ISSUE: "ACME-7" })
        .issueKey
    ).toBeUndefined();
  });

  it("takes no key from an empty LEGION_ISSUE_TRAILER", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, { LEGION_STATE_DIR: "/state", LEGION_ISSUE_TRAILER: "" })
        .issueKey
    ).toBeUndefined();
  });

  it("yields no session id for a subagent transcript", () => {
    expect(
      attributionInputs("/sessions/-x/2026-08-30T03-47-40-329Z_abc/Worker.jsonl", {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
      }).sessionId
    ).toBeUndefined();
  });

  it("fails the pane that has no state directory", () => {
    expect(() => attributionInputs(ROOT_TRANSCRIPT, {})).toThrow(
      "LEGION_STATE_DIR is required for Legion"
    );
  });
});

const PANE_SESSION = "ses_pane_stopping";
/** A transcript no session id is read from: the boot writes no jj attribution. */
const SUBAGENT_TRANSCRIPT = "/sessions/-x/2026-08-30T03-47-40-329Z_abc/Worker.jsonl";
const STATE_DIR = temporaryDirectory("claim-session-state");

/** Lets every callback already queued run: the claim step's failure path is microtasks alone. */
function settled(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** The pane's claim, booted against a daemon that registers it and answers its ready with
 * `options.ready` (a 204 by default), with a first-turn bound of 0. Envoy's one bound instance
 * refuses each claim with `options.claimError`, by default naming the session's stop
 * (`RoleClaimStoppingError`), as pi-envoy's instance does once its stop began; `reached` settles
 * once a claim reached it. The host's stop is `options.hostStop`; by default none has begun.
 * Every exit the claim makes is recorded, in `exits` and as `exit <code>` on `options.steps`, and
 * `exited` settles at the first. */
function pane(
  options: {
    readonly ready?: () => Response;
    readonly hostStop?: () => Promise<void> | undefined;
    readonly steps?: string[];
    readonly claimError?: (role: string) => Error;
  } = {}
) {
  process.env.LEGION_BOOT_TOKEN = fixtureSecret("boot");
  process.env.LEGION_STATE_DIR = STATE_DIR;
  process.env.LEGION_CLAIM_FIRST_TURN_BOUND_MS = "0";
  const registered = readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
    "utf8"
  );
  const daemon = createLegionDaemonClient("http://daemon.test", {
    pluginContract: 60,
    fetch: (async (input) => {
      const route = new URL(input.toString()).pathname;
      if (route.endsWith("/claims/register")) {
        return new Response(registered, { headers: { "content-type": "application/json" } });
      }
      if (route.endsWith("/claims/ready") && options.ready !== undefined) return options.ready();
      return new Response(null, { status: 204 });
    }) as typeof fetch,
  });
  const claims: string[] = [];
  const reached = Promise.withResolvers<void>();
  legionRoleClaimBridge().instances.push({
    claim: async (_sessionID, role) => {
      claims.push(role);
      reached.resolve();
      throw options.claimError?.(role) ?? new RoleClaimStoppingError(role);
    },
    subscribe: async () => undefined,
    sessionID: () => PANE_SESSION,
  });
  const exits: number[] = [];
  const exited = Promise.withResolvers<void>();
  const claim = createClaimSession({
    daemon: () => daemon,
    persistedTranscript: async () => ({ sessionFile: SUBAGENT_TRANSCRIPT, agentId: PANE_SESSION }),
    exitProcess: (code) => {
      exits.push(code);
      options.steps?.push(`exit ${code}`);
      exited.resolve();
      return undefined as never;
    },
    hostStop: options.hostStop ?? (() => undefined),
  });
  const context = {
    sessionManager: { getSessionId: () => PANE_SESSION },
  } as unknown as SessionContext;
  return { claim, context, claims, exits, exited: exited.promise, reached: reached.promise };
}

// The pane's stop has begun when its first-turn bound comes (R24 of dispatch://INFRA-3589): the
// claim it would make exits nothing and writes nothing, so the stop's role release and DELETE
// run to their end. Either the session's `session_shutdown` told the claim session so, or, in
// RPC mode where a SIGTERM emits none, Envoy refused the claim naming the stop. The control is
// the row after these.
describe("a claim whose session's stop has begun", () => {
  it("is not made once the session's shutdown stopped the claim session, and exits nothing", async () => {
    const { claim, context, claims, exits } = pane();
    const lines = spyOn(console, "error").mockImplementation(() => undefined);
    try {
      await claim.bootstrap(context);
      // The bound is armed and has not run: the stop begins first.
      claim.stop();
      // A timer armed after the bound, with the same delay of 0 ms, runs after the bound's: had the
      // stop left the bound armed, its claim would have started by then. It costs no wait.
      const afterBound = Promise.withResolvers<void>();
      setTimeout(afterBound.resolve, 0);
      await afterBound.promise;
      await settled();

      expect(claims).toEqual([]);
      expect(exits).toEqual([]);
      expect(lines).not.toHaveBeenCalled();
    } finally {
      lines.mockRestore();
    }
  });

  it("that Envoy refuses naming the stop, at the bound, says nothing and exits nothing", async () => {
    const { claim, context, claims, exits, reached } = pane();
    const lines = spyOn(console, "error").mockImplementation(() => undefined);
    try {
      await claim.bootstrap(context);
      await reached;
      await settled();

      expect(claims).toHaveLength(1);
      expect(exits).toEqual([]);
      expect(lines).not.toHaveBeenCalled();
    } finally {
      lines.mockRestore();
    }
  });
});

// The control of the rows above: the same claim, made at the bound with no stop begun, which
// Envoy refuses for another reason. The pane says why on its log line and exits 1.
describe("a claim with no stop begun", () => {
  it("control: that Envoy refuses for another reason at the bound says why and exits 1", async () => {
    const { claim, context, claims, exits, exited } = pane({
      claimError: (role) => new Error(`Envoy role ${role} is held by live session ses_other`),
    });
    const lines = spyOn(console, "error").mockImplementation(() => undefined);
    try {
      await claim.bootstrap(context);
      await exited;
      await settled();

      expect(claims).toHaveLength(1);
      expect(exits).toEqual([1]);
      expect(lines).toHaveBeenCalledTimes(1);
    } finally {
      lines.mockRestore();
    }
  });
});

/** The daemon refuses the claim's ready: its registration deadline retired this claim's process
 * and stopped it (`registrationDeadline`, `packages/daemon/internal/supervise/table.go`). A 4xx is
 * a refusal that no retry changes. */
const readyRefused = (): Response =>
  Response.json({ error: "this claim's process was retired" }, { status: 409 });

/** The host's stop, begun before the daemon's refusal comes: pi-envoy's role release, and then,
 * when the row calls `end`, its DELETE, which ends the stop. Each step is recorded on `steps`. */
function hostStopRunning(steps: string[]) {
  steps.push("release");
  const ended = Promise.withResolvers<void>();
  return {
    hostStop: () => ended.promise,
    end: () => {
      steps.push("delete");
      ended.resolve();
    },
  };
}

// R16 of dispatch://INFRA-3589: the daemon's registration deadline retired the pane's claim and
// stopped its process. In RPC mode the SIGTERM runs pi-envoy's stop (its role release, then its
// DELETE) and emits no `session_shutdown`, so the claim session has not heard of the stop when the
// daemon refuses its `claims/ready`. The exit that the refusal makes waits for the host's stop to
// end, at most its bound, and keeps its status. With no stop begun, it exits at once.
describe("a claim the daemon refuses while the host's stop runs", () => {
  it("exits only after the stop's DELETE, with status 1", async () => {
    const steps: string[] = [];
    const stop = hostStopRunning(steps);
    const { claim, context, exited } = pane({
      ready: readyRefused,
      hostStop: stop.hostStop,
      steps,
    });
    const lines = spyOn(console, "error").mockImplementation(() => undefined);
    try {
      // The boot has met the refusal once it has settled.
      await claim.bootstrap(context);
      stop.end();
      await exited;

      expect(steps).toEqual(["release", "delete", "exit 1"]);
      expect(lines).toHaveBeenCalledTimes(1);
    } finally {
      lines.mockRestore();
    }
  });

  it("exits at once where no stop has begun", async () => {
    const steps: string[] = [];
    const { claim, context } = pane({ ready: readyRefused, steps });
    const lines = spyOn(console, "error").mockImplementation(() => undefined);
    try {
      await claim.bootstrap(context);

      expect(steps).toEqual(["exit 1"]);
    } finally {
      lines.mockRestore();
    }
  });

  it("exits at the bound where the stop does not end", async () => {
    const steps: string[] = [];
    const stop = hostStopRunning(steps);
    const { claim, context, exited } = pane({
      ready: readyRefused,
      hostStop: stop.hostStop,
      steps,
    });
    const lines = spyOn(console, "error").mockImplementation(() => undefined);
    jest.useFakeTimers();
    try {
      await claim.bootstrap(context);
      jest.advanceTimersByTime(HOST_STOP_GRACE_MS - 1);
      await Promise.resolve();
      expect(steps).toEqual(["release"]);
      jest.advanceTimersByTime(1);
      await exited;

      expect(steps).toEqual(["release", "exit 1"]);
    } finally {
      jest.useRealTimers();
      lines.mockRestore();
    }
  });
});
