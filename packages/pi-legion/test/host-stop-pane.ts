/**
 * host-stop-pane.ts — one Legion claim in an OS process of its own, under Oh My Pi's own exit
 * callbacks, for `src/host-stop.test.ts`, which sends it a real SIGTERM.
 *
 * Importing `@oh-my-pi/pi-utils` installs Oh My Pi's signal handlers (`postmortem`): a SIGTERM
 * runs the exit callbacks, waits for them, and ends the process with 143. One exit callback here
 * stands in for pi-envoy's stop (`pi-envoy-session-stop`). It writes `release` and lets the daemon
 * refuse the claim's ready. It waits until the claim's exit has asked for the host's stop
 * (`ompHostStop`), and then writes `delete`, which ends it. The claim's exit is `process.exit`, as
 * the Legion entry's is, and it writes `exit <code>` first. Each line is one synchronous write, so
 * no exit loses one.
 *
 * `HOST_STOP_PANE_KIND` is `pane`, a phase worker's claim boot (`createClaimSession`), or
 * `controller`, the daemon-launched controller's claim (`createControllerSession`).
 * `HOST_STOP_PANE_READY` is `at-stop`, where the daemon refuses the claim's ready once the stop has
 * begun, or `now`, where it refuses the ready at once and no stop begins.
 */
import { writeSync } from "node:fs";
import path from "node:path";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { legionRoleClaimBridge } from "@legion/pi-shared/role-claim-bridge";
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";
import { postmortem } from "@oh-my-pi/pi-utils";
import { plugin } from "bun";
import { ompHostStop } from "../src/host-stop";

// The two claim modules import `@oh-my-pi/pi-coding-agent` through
// `@legion/pi-shared/subagent-session`. The host supplies that package at run time, and this
// workspace does not install it. A Bun runtime plugin stands in for it. A static import is resolved
// before any line of this file runs, so the two modules are imported after the plugin is in.
plugin({
  name: "oh-my-pi-host-stand-in",
  setup(build) {
    build.module("@oh-my-pi/pi-coding-agent", () => ({
      exports: { copyToClipboard: async (): Promise<void> => undefined, ...hostAgentRegistryMock },
      loader: "object",
    }));
  },
});
const { createClaimSession } = await import("../src/claim-session");
const { createControllerSession } = await import("../src/controller-session");

const { HOST_STOP_PANE_KIND = "pane", HOST_STOP_PANE_READY = "at-stop" } = process.env;
const SESSION = "ses_host_stop_pane";

const say = (line: string): void => {
  writeSync(1, `${line}\n`);
};

const fixture = (name: string): Response =>
  new Response(
    Bun.file(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)),
    { headers: { "content-type": "application/json" } }
  );

const stopBegan = Promise.withResolvers<void>();
const exitAsked = Promise.withResolvers<void>();

const daemon = createLegionDaemonClient("http://daemon.test", {
  pluginContract: 60,
  fetch: (async (input) => {
    const route = new URL(input.toString()).pathname;
    if (route === "/healthz") return fixture("health.json");
    if (route === "/legion/v1/claims/register") {
      return fixture(
        HOST_STOP_PANE_KIND === "controller" ? "register-controller.json" : "register.json"
      );
    }
    if (route === "/legion/v1/claims/ready") {
      // The daemon's registration deadline retired this claim's process, stopped it, and refuses
      // its ready.
      if (HOST_STOP_PANE_READY === "at-stop") await stopBegan.promise;
      return Response.json({ error: "this claim's process was retired" }, { status: 409 });
    }
    return new Response(null, { status: 204 });
  }) as typeof fetch,
});

// Envoy's one bound instance: every role claim and notice subscription lands.
legionRoleClaimBridge().instances.push({
  claim: async () => undefined,
  subscribe: async () => undefined,
  sessionID: () => SESSION,
});

postmortem.register(
  "pi-envoy-session-stop",
  async () => {
    say("release");
    stopBegan.resolve();
    await exitAsked.promise;
    say("delete");
  },
  { exitOnly: true }
);

const host = {
  exitProcess: (code: number): never => {
    say(`exit ${code}`);
    return process.exit(code);
  },
  hostStop: () => {
    exitAsked.resolve();
    return ompHostStop();
  },
};
const context = {
  sessionManager: { getSessionId: () => SESSION },
  ui: { notify: () => undefined, setWidget: () => undefined },
} as unknown as SessionContext;
const transcript = { sessionFile: "/sessions/-x/host-stop-pane/Worker.jsonl", agentId: SESSION };

say("ready");
const claimed =
  HOST_STOP_PANE_KIND === "controller"
    ? createControllerSession({
        ...host,
        daemon: () => daemon,
        persistedTranscript: async () => transcript,
        pi: {} as PiApi,
      }).handleSessionStart(context)
    : createClaimSession({
        ...host,
        daemon: () => daemon,
        persistedTranscript: async () => transcript,
      }).bootstrap(context);
claimed.catch((error: unknown) => say(`failed: ${String(error)}`));
// Nothing else keeps the process alive until its signal.
setInterval(() => undefined, 2 ** 30);
