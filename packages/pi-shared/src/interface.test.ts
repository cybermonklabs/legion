import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { logger } from "@oh-my-pi/pi-utils";
import { matchInjectedUserTurn, noteInjectedUserTurn } from "./injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_KEY,
  ENVOY_PLUGIN_INTERFACE_VERSION,
  type EnvoyPluginInterface,
  envoyPluginInterface,
  LEGACY_LEGION_LOADED_KEY,
  LEGION_PLUGIN_LOADED_KEY,
  publishEnvoyPluginInterface,
  readEnvoyPluginInterface,
  resetEnvoyPluginInterfaceForTests,
} from "./interface";
import {
  claimEnvoyRole,
  type LegionRoleClaimInstance,
  legionRoleClaimBridge,
} from "./role-claim-bridge";
import { clearStartupTurn, markStartupTurnPending, markStartupTurnSent } from "./startup-turn";
import {
  clearTaskTurnPending,
  markTaskTurnPending,
  publishTaskSignal,
  takeTaskSignals,
} from "./task-values";

const store = globalThis as typeof globalThis & { [key: symbol]: unknown };

/** An Envoy instance as envoy.ts binds one, recording the claims that reach it. */
function instance(sessionID: string, claims: string[]): LegionRoleClaimInstance {
  return {
    claim: async (_sessionID, role) => {
      claims.push(role);
    },
    subscribe: async () => undefined,
    sessionID: () => sessionID,
  };
}

/** The object a build at `version` of this module would have created, published from `from`. */
function foreignInterface(version: number, from: string): EnvoyPluginInterface {
  return {
    version,
    publishers: [from],
    roleClaim: {
      instances: [],
      managedSessions: new Set(),
      regained: undefined,
      slackRoleOwned: false,
    },
    injectedUserTurns: new Map(),
    bootstrappedSession: { file: undefined },
    startupTurn: undefined,
    taskValues: { taker: undefined },
    taskTurnPending: undefined,
  };
}

const warnings: logger.LogEvent[] = [];
let stopSink = (): void => undefined;

beforeEach(() => {
  resetEnvoyPluginInterfaceForTests();
  warnings.splice(0);
  stopSink = logger.registerLogSink((event) => {
    if (event.level === "warn") warnings.push(event);
  });
});

afterEach(() => {
  stopSink();
  resetEnvoyPluginInterfaceForTests();
});

describe("the Envoy plugin interface", () => {
  test("is created by whoever touches it first, and a later publisher joins that same object with the consumer's state intact", () => {
    // A Legion consumer runs before any Envoy entry published: it records a sent turn on the
    // object it created.
    noteInjectedUserTurn("ses_1", "ship it", "m-1");
    const consumerView = envoyPluginInterface();
    expect(readEnvoyPluginInterface()).toEqual({ kind: "absent" });

    const published = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");

    expect(published).toBe(consumerView);
    expect(published?.publishers).toEqual(["file:///plugins/pi-envoy/dist/envoy.js"]);
    expect(store[ENVOY_PLUGIN_INTERFACE_KEY]).toBe(consumerView);
    expect(
      matchInjectedUserTurn("ses_1", {
        role: "user",
        timestamp: 7,
        content: [{ type: "text", text: "ship it" }],
      })
    ).toBe("m-1");
  });

  test("a second publisher at the same version joins, and the instance it pushes is reachable through the first publisher's object", async () => {
    const first = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    const second = publishEnvoyPluginInterface("file:///checkout/packages/pi-envoy/envoy.ts");
    if (first === undefined || second === undefined) throw new Error("a publish was refused");
    const claims: string[] = [];
    second.roleClaim.instances.push(instance("ses_pane", claims));

    await claimEnvoyRole("ses_pane", "legion-omp-controller");

    expect(second).toBe(first);
    expect(first.publishers).toEqual([
      "file:///plugins/pi-envoy/dist/envoy.js",
      "file:///checkout/packages/pi-envoy/envoy.ts",
    ]);
    expect(first.roleClaim.instances).toHaveLength(1);
    expect(claims).toEqual(["legion-omp-controller"]);
    expect(legionRoleClaimBridge().managedSessions.has("ses_pane")).toBe(true);
    expect(warnings).toEqual([]);
  });

  test("a second publisher at another version warns once naming both versions and both entries, and does not join", () => {
    const other = ENVOY_PLUGIN_INTERFACE_VERSION + 1;
    const foreign = foreignInterface(other, "file:///plugins/pi-envoy-next/dist/envoy.js");
    store[ENVOY_PLUGIN_INTERFACE_KEY] = foreign;

    const refused = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    const refusedAgain = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");

    expect(refused).toBeUndefined();
    expect(refusedAgain).toBeUndefined();
    expect(foreign.publishers).toEqual(["file:///plugins/pi-envoy-next/dist/envoy.js"]);
    expect(warnings).toHaveLength(1);
    const [warning] = warnings;
    expect(warning?.message).toContain(`version ${other}`);
    expect(warning?.message).toContain(`version ${ENVOY_PLUGIN_INTERFACE_VERSION}`);
    expect(warning?.message).toContain("file:///plugins/pi-envoy-next/dist/envoy.js");
    expect(warning?.message).toContain("file:///plugins/pi-envoy/dist/envoy.js");
    // Consumers keep reading the one object, whatever its version.
    expect(envoyPluginInterface()).toBe(foreign);
  });

  test("reads absent until an Envoy entry published, mismatch for another version with the first publisher, and present otherwise", () => {
    expect(readEnvoyPluginInterface()).toEqual({ kind: "absent" });
    envoyPluginInterface();
    expect(readEnvoyPluginInterface()).toEqual({ kind: "absent" });

    const other = ENVOY_PLUGIN_INTERFACE_VERSION + 1;
    store[ENVOY_PLUGIN_INTERFACE_KEY] = foreignInterface(
      other,
      "file:///plugins/pi-envoy-next/dist/envoy.js"
    );
    expect(readEnvoyPluginInterface()).toEqual({
      kind: "mismatch",
      found: other,
      expected: ENVOY_PLUGIN_INTERFACE_VERSION,
      from: "file:///plugins/pi-envoy-next/dist/envoy.js",
    });

    resetEnvoyPluginInterfaceForTests();
    const published = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    if (published === undefined) throw new Error("the publish was refused");
    expect(readEnvoyPluginInterface()).toEqual({ kind: "present", iface: published });
  });

  test("names the symbols the daemon's probe reads from globalThis, the old package's marker among them", () => {
    expect(Symbol.keyFor(ENVOY_PLUGIN_INTERFACE_KEY)).toBe(
      "legion.pi-shared.envoy-plugin-interface"
    );
    expect(Symbol.keyFor(LEGION_PLUGIN_LOADED_KEY)).toBe("legion.pi-legion.loaded");
    expect(Symbol.keyFor(LEGACY_LEGION_LOADED_KEY)).toBe("legion.pi-envoy.legion-loaded");
  });

  // Version 5: the mark carries a start that is due and not sent yet (`sentAt` undefined), which
  // the operator's controller sets at `session_start` and its start command's send overwrites.
  test("carries the startup-turn mark unsent, then sent, cleared only by the session it names and only at this build's version", () => {
    publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    const iface = envoyPluginInterface();
    expect(iface.startupTurn).toBeUndefined();

    markStartupTurnPending("ses_controller");
    expect(iface.startupTurn).toEqual({ session: "ses_controller", sentAt: undefined });
    clearStartupTurn("ses_task_subagent");
    expect(iface.startupTurn).toEqual({ session: "ses_controller", sentAt: undefined });

    const before = Date.now();
    markStartupTurnSent("ses_controller");
    expect(iface.startupTurn?.session).toBe("ses_controller");
    expect(iface.startupTurn?.sentAt).toBeGreaterThanOrEqual(before);
    expect(iface.startupTurn?.sentAt).toBeLessThanOrEqual(Date.now());

    clearStartupTurn("ses_controller");
    expect(iface.startupTurn).toBeUndefined();

    // An object another build's Envoy entry published is left as it is.
    const foreign = foreignInterface(
      ENVOY_PLUGIN_INTERFACE_VERSION + 1,
      "file:///plugins/pi-envoy-next/dist/envoy.js"
    );
    foreign.startupTurn = { session: "ses_controller", sentAt: undefined };
    store[ENVOY_PLUGIN_INTERFACE_KEY] = foreign;
    clearStartupTurn("ses_controller");
    expect(foreign.startupTurn).toEqual({ session: "ses_controller", sentAt: undefined });
  });

  // Version 6: the claims' task values, which the Envoy entry publishes and the Legion entry's
  // claim takes, and the mark the Legion entry's take keeps while a task's turn is pending.
  test("is version 6, carrying the task values' one taker and the task-turn mark", () => {
    expect(ENVOY_PLUGIN_INTERFACE_VERSION).toBe(6);
    const published = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    const iface = envoyPluginInterface();
    expect(published).toBe(iface);
    expect(iface.taskValues.taker).toBeUndefined();
    expect(iface.taskTurnPending).toBeUndefined();

    // A signal with no taker reaches nobody; the last taker set takes every signal after it.
    const failures: string[] = [];
    const log = (message: string) => failures.push(message);
    const value = {
      kind: "value",
      session: "ses_pane",
      value: { claim: "legion-a-a-1-implementer", epoch: "e1", revision: 2, op: "put", task: 3 },
    } as const;
    publishTaskSignal(iface, value, log);
    const first: unknown[] = [];
    const second: unknown[] = [];
    takeTaskSignals((signal) => first.push(signal));
    takeTaskSignals((signal) => second.push(signal));
    publishTaskSignal(iface, value, log);
    publishTaskSignal(iface, { kind: "epoch", session: "ses_pane" }, log);
    expect(first).toEqual([]);
    expect(second).toEqual([value, { kind: "epoch", session: "ses_pane" }]);
    // An interface the Envoy entry did not publish into is none to publish on.
    publishTaskSignal(undefined, value, log);
    expect(second).toHaveLength(2);

    // A taker that throws is reported, and the publisher goes on.
    takeTaskSignals(() => {
      throw new Error("the taker failed");
    });
    publishTaskSignal(iface, value, log);
    expect(failures).toEqual(["the Legion entry's task value taker threw"]);

    // The mark names one session, and only that session clears it.
    markTaskTurnPending("ses_pane");
    expect(iface.taskTurnPending).toEqual({ session: "ses_pane" });
    clearTaskTurnPending("ses_task_subagent");
    expect(iface.taskTurnPending).toEqual({ session: "ses_pane" });
    clearTaskTurnPending("ses_pane");
    expect(iface.taskTurnPending).toBeUndefined();
  });
});
