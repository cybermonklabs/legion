// The services the Oh My Pi plugin's live scripts run their sessions against, every one built from
// this checkout: the natsd and Envoy listener of packages/envoy-client/test/envoy-stack.ts, and,
// for a script whose session talks to Dispatch, the Dispatch server of
// packages/envoy-client/test/dispatch-stack.ts with its NATS on that natsd and its deliveries
// through that listener. lib/plugin-smoke.sh starts it for packages/pi-envoy/scripts/smoke-*.sh and
// packages/dispatch/e2e/acceptance/omp-roundtrip.sh:
//
//   bun scripts/e2e/lib/plugin-smoke-stack.ts <work> <state-file> [--dispatch] [--listener-env NAME=VALUE]...
//
// <work> is an empty directory of the run's own: each stack works in a directory it makes there.
// Once every service answers, it writes <state-file> by a rename, so a reader never takes half of
// it: one JSON object, {envoyUrl, natsUrl, monitorUrl}, and with --dispatch {dispatchUrl,
// dispatchToken} too, the server's URL and the agent bearer it accepts. It then holds the services
// until it is signalled (TERM, INT or HUP), stops Dispatch, which drops its database, then the
// listener and natsd, and exits 0. A service that does not come up is one line on stderr naming
// why, and exit 1 once whatever it started is stopped.
//
// --listener-env NAME=VALUE sets one variable of the listener's environment over the stack's own
// (envoy-stack.ts's listenerEnv), as smoke-registration-stall.sh shortens the listener's
// ENVOY_SESSION_LIVE_WINDOW. --dispatch needs DISPATCH_TEST_DATABASE_URL, the PostgreSQL
// dispatch-stack.ts makes the server's database on.
import { mkdirSync, renameSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import {
  type DispatchStack,
  startDispatchStack,
} from "../../../packages/envoy-client/test/dispatch-stack.ts";
import {
  type EnvoyStack,
  startEnvoyStack,
} from "../../../packages/envoy-client/test/envoy-stack.ts";

const USAGE =
  "usage: bun scripts/e2e/lib/plugin-smoke-stack.ts <work> <state-file> [--dispatch] [--listener-env NAME=VALUE]...";

function refuse(message: string): never {
  process.stderr.write(`plugin-smoke-stack: ${message}\n`);
  process.exit(2);
}

const [work, stateFile, ...flags] = process.argv.slice(2);
if (work === undefined || stateFile === undefined) refuse(USAGE);
let withDispatch = false;
const listenerEnv: Record<string, string> = {};
for (let i = 0; i < flags.length; i++) {
  const flag = flags[i];
  if (flag === "--dispatch") {
    withDispatch = true;
  } else if (flag === "--listener-env") {
    const setting = flags[++i] ?? "";
    const equals = setting.indexOf("=");
    if (equals <= 0) refuse(`--listener-env takes NAME=VALUE, not "${setting}"`);
    listenerEnv[setting.slice(0, equals)] = setting.slice(equals + 1);
  } else {
    refuse(`unknown argument ${flag}; ${USAGE}`);
  }
}

let envoy: EnvoyStack | undefined;
let dispatch: DispatchStack | undefined;
let stopping: Promise<void> | undefined;
// Dispatch first: it publishes to the natsd and delivers through the listener the second stops.
const stop = (): Promise<void> =>
  (stopping ??= (async () => {
    await dispatch?.stop();
    await envoy?.stop();
  })());
for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"] as const) {
  process.on(signal, () => {
    void stop().finally(() => process.exit(0));
  });
}

try {
  const envoyWork = path.join(work, "envoy");
  mkdirSync(envoyWork);
  envoy = await startEnvoyStack({ work: envoyWork, listenerEnv });
  if (withDispatch) {
    const dispatchWork = path.join(work, "dispatch");
    mkdirSync(dispatchWork);
    dispatch = await startDispatchStack({ work: dispatchWork, envoy });
  }
} catch (error) {
  process.stderr.write(
    `plugin-smoke-stack: ${error instanceof Error ? error.message : String(error)}\n`
  );
  await stop();
  process.exit(1);
}

const state = {
  envoyUrl: envoy.envoyUrl,
  natsUrl: envoy.natsUrl,
  monitorUrl: envoy.monitorUrl,
  ...(dispatch === undefined ? {} : { dispatchUrl: dispatch.url, dispatchToken: dispatch.token }),
};
writeFileSync(`${stateFile}.tmp`, `${JSON.stringify(state)}\n`, { mode: 0o600 });
renameSync(`${stateFile}.tmp`, stateFile);
// Held until a signal above stops the services: nothing else keeps this process running.
setInterval(() => {}, 1 << 30);
