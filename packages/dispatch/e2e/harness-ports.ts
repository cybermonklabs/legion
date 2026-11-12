/** The harness ports, resolved and validated for every reader in `e2e/`.
 *
 * An unset or empty variable is port `0`: the listener binds port 0, so the OS chooses its port,
 * and the process reports the port it got on its listening line. `e2e/harness-start.ts` reads
 * those reports and returns the ports by variable, and `e2e/global-setup.ts` writes them into
 * this process's environment before the first worker starts, so every reader below sees the
 * real ports when it resolves them. A set value is used
 * as given: its listener binds exactly that port, and an occupied one fails with "address already
 * in use" rather than being replaced. `PLAIN_HTTP_PORT` is the plain-HTTP project's browser-facing
 * proxy, which only the TypeScript harness reads. `DISPATCH_E2E_PORT_A` and `DISPATCH_E2E_PORT_B`
 * are Dispatch A's and B's in two-process mode (`e2e/harness-processes.ts`), where
 * `DISPATCH_E2E_PORT` is the round-robin proxy's; unset, each is held before the proxy starts, so
 * the proxy can name it (`e2e/harness-start.ts`). `DISPATCH_E2E_SIGNIN_ISSUER_PORT` is the dev
 * OIDC issuer's under `DISPATCH_E2E_SIGNIN=oidc` (`e2e/harness-signin.ts`), which always serves
 * on a port the OS chooses: `e2e/run-signin-issuer.sh` refuses it set. `REAL_BROKER_PORT` is the
 * real secrets broker's and `DEV_OIDC_ISSUER_PORT` its dev OIDC issuer's (`e2e/run-broker.sh`),
 * the pair `e2e/broker-approval.e2e.ts` drives an approval on. A run starts that pair only when a
 * spec of its can reach it (`e2e/harness-real-broker.ts`), so neither port takes part in the
 * collision check otherwise. The issuer serves on a port the OS chooses, as the sign-in issuer
 * does, and `e2e/run-broker.sh` refuses `DEV_OIDC_ISSUER_PORT` set for it. The broker half reads
 * that variable instead, which `e2e/harness-start.ts` exports from the issuer's report, since the
 * broker takes the issuer's URL as its token issuer and its sign-in provider. `FAKE_EMBEDDER_PORT`
 * is the fake embedder's (`e2e/fake-embedder.ts`), started only by a run whose server embeds
 * there (`e2e/harness-embedder.ts`), so it takes part in the collision check only then.
 * `FAKE_CHAT_MODEL_PORT` and `FAKE_DECISION_MODEL_PORT` are the fake writing model's and the fake
 * decision model's (`e2e/fake-chat-model.ts`, `e2e/fake-decision-model.ts`), started only by the
 * models lane (`e2e/harness-models.ts`), so they take part in the collision check only then.
 *
 * A set value that is not a port in canonical decimal is refused naming its variable, because
 * every later consumer turns it into something that names nothing: `net.connect` raises
 * `ERR_SOCKET_BAD_PORT` on `NaN`, and a URL built from a value like `1e4` or `0x2249` points nowhere
 * anything is listening. `0` is refused too: unset is how a caller asks for a port the OS chooses,
 * and a written `0` would read as a port a URL can name. A leading zero is not canonical either:
 * `08777` and the `8777` it parses to are one port written two ways, and the point of validating
 * here is that only one representation leaves this module.
 *
 * `harnessPorts` reads the environment when it is called, not when this module is imported: the
 * harness's starters (`e2e/harness-start.ts` through `e2e/local-instance.ts` and the docs media
 * harness) import it before the listeners have reported, and the modules they import afterwards
 * must see the ports those set. It opens no socket, so importing it is safe from any process.
 *
 * A run of more than one Playwright worker runs one stack per worker (`e2e/harness-pool.ts`): a
 * whole harness, with a Postgres, ports and a file-store bucket of its own. `globalSetup`
 * (`e2e/global-setup.ts`) starts the pool and writes the whole of it into `DISPATCH_E2E_STACKS`,
 * one JSON record per stack holding that stack's own variables, and it writes nothing else: the
 * plain names are left exactly as the caller had them, so a SECOND `globalSetup` in one process
 * - which is how Playwright's UI mode re-runs - reads the environment the first one found
 * rather than the stopped stacks' ports and databases, and the teardown takes the manifest back
 * out again (`e2e/global-teardown.ts`).
 * `stackValue` below is the one place a reader picks its own: the record `TEST_PARALLEL_INDEX`
 * selects, then the plain name. Playwright sets that variable in each worker process and a
 * restarted worker keeps it, and a process that is no worker has none and so reads the
 * manifest's first record: the config's own evaluation and `globalSetup` itself. The plain
 * names answer where no manifest does - each harness listener the pool starts, which was handed
 * its own stack's values under them, a harness a person left listening, and a deployed run.
 *
 * A run of more than one stack refuses, before anything starts, every variable that names a
 * thing one stack owns (`refuseStackConflicts` at the end of this file).
 */

import { usesFakeEmbedder } from "./harness-embedder";
import { usesFakeModels } from "./harness-models";
import { harnessProcessCount } from "./harness-processes";
import { usesRealBroker } from "./harness-real-broker";
import { harnessSignIn } from "./harness-signin";

/** This run's stacks, as JSON: an array with one record per stack, each record that stack's own
 *  variables by name — its ports, its `DATABASE_URL`, its `DISPATCH_E2E_FILE_STORE_BUCKET`.
 *  `e2e/global-setup.ts` writes it into `process.env` once the pool is up, and Playwright hands
 *  every worker process that environment. */
export const stacksVariable = "DISPATCH_E2E_STACKS";

/** The last manifest parsed, and the text it was parsed from. `harnessPorts()` resolves eleven
 *  variables through the manifest and a row calls it whenever it addresses a listener, so a run
 *  would otherwise parse the same JSON tens of thousands of times. The text is the key, so an
 *  environment that changes - a test that hands in another manifest, a process that clears a
 *  port (`withoutHarnessPort`) - is parsed again. */
let parsedText: string | undefined;
let parsedStacks: readonly Record<string, string>[] | undefined;

function refuseStacks(): never {
  throw new Error(
    `${stacksVariable} must hold this run's stacks as JSON: an array with one object per stack, ` +
      "each mapping a variable name to its value. `globalSetup` writes it when it starts the " +
      "pool (e2e/global-setup.ts); leave it unset to read the plain variable names."
  );
}

function isStackRecord(value: unknown): value is Record<string, string> {
  return (
    typeof value === "object" &&
    value !== null &&
    !Array.isArray(value) &&
    Object.values(value).every((entry) => typeof entry === "string")
  );
}

/** The pool as `env` carries it, or undefined where it carries none, which is every run that
 *  reads the plain names alone: a harness a person started, a deployed run, and each listener
 *  the pool starts. */
function stacks(env: NodeJS.ProcessEnv): readonly Record<string, string>[] | undefined {
  const given = env[stacksVariable] ?? "";
  if (given === "") return undefined;
  if (given === parsedText) return parsedStacks;
  let parsed: unknown;
  try {
    parsed = JSON.parse(given);
  } catch {
    refuseStacks();
  }
  if (!Array.isArray(parsed) || !parsed.every(isStackRecord)) refuseStacks();
  parsedText = given;
  parsedStacks = parsed;
  return parsedStacks;
}

/** The stack the process `env` belongs to. */
export function stackIndex(env: NodeJS.ProcessEnv = process.env): number {
  const given = env.TEST_PARALLEL_INDEX ?? "";
  if (given === "") return 0;
  if (!/^(0|[1-9][0-9]*)$/.test(given)) {
    throw new Error(
      `TEST_PARALLEL_INDEX must be the stack's index, a number from 0 up, not ` +
        `${JSON.stringify(given)}. Playwright sets it in each worker process; leave it unset.`
    );
  }
  return Number(given);
}

/** `variable` as this process's stack carries it: the manifest's record for `stackIndex(env)`,
 *  then the plain name, which a run of one stack, every process the pool starts itself and every
 *  run against servers a person started read. An index the manifest holds no record for falls
 *  through to the plain name rather than refusing, since a process outside the pool has nothing
 *  but those names to read. */
export function stackValue(env: NodeJS.ProcessEnv, variable: string): string | undefined {
  return stacks(env)?.[stackIndex(env)]?.[variable] ?? env[variable];
}

/** Each harness listener's port variable. */
export const harnessVariables = {
  devIssuer: "DEV_OIDC_ISSUER_PORT",
  dispatch: "DISPATCH_E2E_PORT",
  dispatchA: "DISPATCH_E2E_PORT_A",
  dispatchB: "DISPATCH_E2E_PORT_B",
  fakeBroker: "FAKE_BROKER_PORT",
  fakeChatModel: "FAKE_CHAT_MODEL_PORT",
  fakeDecisionModel: "FAKE_DECISION_MODEL_PORT",
  fakeEmbedder: "FAKE_EMBEDDER_PORT",
  fakeEnvoy: "FAKE_ENVOY_PORT",
  fakeGithub: "FAKE_GITHUB_PORT",
  plainHttp: "PLAIN_HTTP_PORT",
  realBroker: "REAL_BROKER_PORT",
  signinIssuer: "DISPATCH_E2E_SIGNIN_ISSUER_PORT",
} as const;

export type HarnessListenerKey = keyof typeof harnessVariables;

/** One harness listener's port, beside the variable a refusal has to name. `port` is 0 when the
 *  variable is unset: the listener binds port 0, and the OS chooses. */
export interface HarnessPort {
  readonly port: number;
  readonly variable: string;
}

/** `env` with `listener`'s port unset under every name a child this process starts would read it
 *  under: the plain variable and the entry this process's stack carries it under in the manifest.
 *  A spec that starts a second listener of its own wants the OS to choose its port
 *  (`e2e/__tests__/fake-envoy.test.ts`, `e2e/__tests__/plain-http-proxy.test.ts`,
 *  `e2e/plain-http-proxy.e2e.ts`), and in a pooled run both hold the port, so dropping the plain
 *  one alone would still hand the child the port the harness listener holds, and it would die on
 *  `EADDRINUSE` before it printed its listening line. The child keeps `TEST_PARALLEL_INDEX`, so
 *  the stack it resolves is the one cleared here, and every other stack's record travels
 *  untouched: a child that starts a listener of its own is still part of this run. */
export function withoutHarnessPort(
  listener: HarnessListenerKey,
  env: NodeJS.ProcessEnv = process.env
): NodeJS.ProcessEnv {
  const variable = harnessVariables[listener];
  const rest = { ...env };
  delete rest[variable];
  const pool = stacks(env);
  const index = stackIndex(env);
  const own = pool?.[index];
  if (pool !== undefined && own !== undefined) {
    const cleared = { ...own };
    delete cleared[variable];
    rest[stacksVariable] = JSON.stringify(
      pool.map((record, at) => (at === index ? cleared : record))
    );
  }
  return rest;
}

function harnessPort(env: NodeJS.ProcessEnv, variable: string): HarnessPort {
  const given = stackValue(env, variable) ?? "";
  if (given === "") return { port: 0, variable };
  const port = Number(given);
  if (!/^[1-9][0-9]{0,4}$/.test(given) || port > 65535) {
    throw new Error(
      `${variable} must be a port number, not ${JSON.stringify(given)}. Leave it unset to have ` +
        "the OS choose one."
    );
  }
  return { port, variable };
}

/** The ports by listener, the one place a port is paired with its variable, as `env` names them
 *  now. Two set variables naming one port are refused: each would pass a per-port check, and
 *  every consumer would then fail in its own words — the second listener dying on `EADDRINUSE`
 *  without naming a variable. Refused here, where the ports resolve, so every caller refuses it
 *  the same way, including the Bun listeners. A's and B's ports take part only in two-process
 *  mode, the one mode that listens on them, the sign-in issuer's only under
 *  `DISPATCH_E2E_SIGNIN=oidc`, and the real broker's and its issuer's only when this run starts
 *  that pair (`e2e/harness-real-broker.ts`). */
export function harnessPorts(
  env: NodeJS.ProcessEnv = process.env
): Record<HarnessListenerKey, HarnessPort> {
  const ports = {
    devIssuer: harnessPort(env, harnessVariables.devIssuer),
    dispatch: harnessPort(env, harnessVariables.dispatch),
    dispatchA: harnessPort(env, harnessVariables.dispatchA),
    dispatchB: harnessPort(env, harnessVariables.dispatchB),
    fakeBroker: harnessPort(env, harnessVariables.fakeBroker),
    fakeChatModel: harnessPort(env, harnessVariables.fakeChatModel),
    fakeDecisionModel: harnessPort(env, harnessVariables.fakeDecisionModel),
    fakeEmbedder: harnessPort(env, harnessVariables.fakeEmbedder),
    fakeEnvoy: harnessPort(env, harnessVariables.fakeEnvoy),
    fakeGithub: harnessPort(env, harnessVariables.fakeGithub),
    plainHttp: harnessPort(env, harnessVariables.plainHttp),
    realBroker: harnessPort(env, harnessVariables.realBroker),
    signinIssuer: harnessPort(env, harnessVariables.signinIssuer),
  };
  const given = Object.values(ports).filter(
    (entry) =>
      entry.port !== 0 &&
      (harnessProcessCount === 2 || (entry !== ports.dispatchA && entry !== ports.dispatchB)) &&
      (harnessSignIn === "oidc" || entry !== ports.signinIssuer) &&
      (usesRealBroker || (entry !== ports.devIssuer && entry !== ports.realBroker)) &&
      (usesFakeEmbedder || entry !== ports.fakeEmbedder) &&
      (usesFakeModels || (entry !== ports.fakeChatModel && entry !== ports.fakeDecisionModel))
  );
  const collisions = [...new Set(given.map((entry) => entry.port))]
    .map((port) => ({
      port,
      variables: given.filter((entry) => entry.port === port).map((entry) => entry.variable),
    }))
    .filter((collision) => collision.variables.length > 1);
  if (collisions.length > 0) {
    const named = collisions
      .map(({ port, variables }) => {
        const names =
          variables.length > 2
            ? `${variables.slice(0, -1).join(", ")} and ${variables[variables.length - 1]}`
            : variables.join(" and ");
        return `${names} name port ${port}`;
      })
      .join("; ");
    throw new Error(
      `The Dispatch e2e harness cannot start: ${named}. Give each harness server its own port, ` +
        "or leave the variables unset to have the OS choose."
    );
  }
  return ports;
}

/** Why a run of more than one stack refuses a variable the caller set. Each of them names a
 *  thing ONE stack owns, so a run that honoured it would hand every stack the one database, the
 *  one set of servers or the one port - and the failure would read as the product's rather than
 *  the run's, because `e2e/test.ts` empties that database before every row, so the rows would
 *  delete each other's fixtures and fail on what was no longer there. The port variables are
 *  refused for the last of those reasons, so they share it in the refusal below. */
const stackConflictReasons: Record<string, string> = {
  DATABASE_URL:
    "each stack runs its own Postgres, whose URL the harness hands its servers and its psql",
  DISPATCH_E2E_PROCESSES:
    "two-process mode is two servers on one stack's database, which one stack runs",
  DISPATCH_E2E_REUSE_SERVERS:
    "the servers a person started are one set of servers, which every stack would share",
  PLAYWRIGHT_BASE_URL: "a deployed run has one server, which every stack would share",
};

/** The variables `env` sets that a run of more than one stack cannot honour, in the order a
 *  message names them. `DISPATCH_E2E_PROCESSES` counts only at `2`: `1` is what one stack runs. */
export function stackConflicts(env: NodeJS.ProcessEnv): readonly string[] {
  const named = Object.keys(stackConflictReasons).filter((variable) =>
    variable === "DISPATCH_E2E_PROCESSES"
      ? env[variable] === "2"
      : (env[variable] ?? "").trim() !== ""
  );
  const ports = Object.values(harnessVariables).filter(
    (variable) => (env[variable] ?? "").trim() !== ""
  );
  return [...named, ...ports].sort();
}

/** Refuses a run of `stacks` stacks whose caller set a variable one stack owns, naming the first
 *  of them: `stackConflicts` is the whole list, and a caller that unsets this one is refused the
 *  next. `e2e/global-setup.ts` calls it before anything starts AND before its own early return
 *  on `DISPATCH_E2E_REUSE_SERVERS=1`, because that variable is the worst of these: the hook
 *  would otherwise return and leave every worker on the one server and the one database.
 *
 *  One stack is refused nothing, whoever asks: every variable here names something ONE stack
 *  owns, and a run of one owns it. The rule is held here rather than at the call alone, so no
 *  caller can refuse an ordinary one-worker run by passing it along. */
export function refuseStackConflicts(stacks: number, env: NodeJS.ProcessEnv = process.env): void {
  if (stacks < 2) return;
  const [variable] = stackConflicts(env);
  if (variable === undefined) return;
  const reason =
    stackConflictReasons[variable] ??
    "a port is one listener's, and each stack needs listeners of its own";
  throw new Error(
    `${stacks} Playwright workers run ${stacks} stacks, each a harness of its own, so ` +
      `${variable} must not be set: ${reason}. Unset ${variable}, or run one worker, which is ` +
      "one stack: `--workers` is how a run asks for more, and this config's own default is 1."
  );
}
