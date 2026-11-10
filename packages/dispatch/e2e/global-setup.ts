/** Playwright's `globalSetup` (`e2e/playwright.config.ts`): it starts this run's stacks, writes
 *  them into the environment every worker inherits, and checks each of them before any row runs.
 *
 *  It is the hook of every run, including the ones that start nothing, because the decision not
 *  to start is this file's: a run against servers a person started
 *  (`DISPATCH_E2E_REUSE_SERVERS=1`, which is also what `e2e/harness-up.ts` prints) and a run of
 *  the stubbed layer alone (`e2e/stub/selection.ts`) each return here before a directory is made
 *  or a server starts.
 *
 *  A deployed run (`PLAYWRIGHT_BASE_URL`, `bun run e2e:deployed`) does start a stack, and a small
 *  one: `targetedListeners()` keeps the rows marked `deployed` alone (`e2e/harness-listeners.ts`,
 *  read by `e2e/harness-start.ts`), which are the fakes a deployed server is tested with, and
 *  with no `DATABASE_URL` and no Postgres provider the pool starts no database for it
 *  (`e2e/harness-postgres.ts`). The preflight then checks the deployed server against this run's
 *  own fake Envoy, which is the whole point of running it.
 *
 *  What a stack is, and why there is one per worker, is in `e2e/harness-pool.ts`. The pool
 *  reports its stacks on stderr, and this file's own line comes after those, so the log reads in
 *  the order the work happened: the stacks, then the preflight against them.
 *
 *  THE PREFLIGHT. Every fixture a row seeds goes to its stack's fake Envoy, while the server
 *  under test reads whatever its own `ENVOY_URL` names: run-server.sh builds it from
 *  `FAKE_ENVOY_PORT`, and the acceptance Compose file from the `FAKE_ENVOY_PORT` its own `up` was
 *  given. A server reading any other listener would answer rows from sessions none of them
 *  seeded, so a session put in a stack's fake must come back from that stack's server before
 *  anything runs. Every stack is checked, not just the first: each has servers of its own, and a
 *  row that lands in stack 2 is no better off for stack 0 having answered. The check reads that
 *  stack's own variables and hands them to the helpers it shares with the rows, since those read
 *  their own process's stack otherwise: `e2e/api.ts`'s `baseUrl` is the stack of the process that
 *  imported it - port 0 here, since this module is imported before a stack exists - and
 *  `e2e/agents.ts`'s fixture helpers address their own stack's fake Envoy port.
 *
 *  In two-process mode (`e2e/harness-processes.ts`) both processes must answer, each asked pinned
 *  rather than whichever the round-robin proxy picks, and a person signed in on A must be signed
 *  in on B: without one cookie key (run-server.sh's DISPATCH_DEV_SIGNIN_SHARED_KEY) every page
 *  would sign its person out at the first request B answers. Under DISPATCH_E2E_SIGNIN=oidc
 *  (`e2e/harness-signin.ts`) the servers have no dev sign-in to check that with; there the one
 *  spec it runs signs a person in through the issuer and asks both processes who they are.
 */
import { existsSync } from "node:fs";
import { chromium, type FullConfig, type FullProject, firefox, webkit } from "@playwright/test";

import { resetFakeEnvoy, setLiveSessions } from "./agents";
import { baseUrlOf, devSignInPath, listAgents, sessionCookieFrom } from "./api";
import { stubProjectNames } from "./configurations";
import { type Stack, startStacks } from "./harness-pool";
import { harnessPorts, stacksVariable } from "./harness-ports";
import {
  type DispatchProcess,
  dispatchProcesses,
  harnessProcessCount,
  pinnedTo,
} from "./harness-processes";
import { harnessSignIn } from "./harness-signin";
import { stubSelection } from "./stub/selection";

/** What this module's own lines are prefixed with, as the pool's are with its name. */
const named = "e2e/global-setup.ts";

/** The browser engines a Playwright run of `e2e/playwright.config.ts` launches. */
const playwrightEngines = ["chromium", "firefox", "webkit"] as const;
type PlaywrightEngine = (typeof playwrightEngines)[number];

/** The projects of `e2e/playwright.config.ts` whose names do not hold their engine's: `iphone`
 *  and `stub-phone` are an iPhone's viewport in Chromium, and `stub` is Chromium at the desktop
 *  width. Every other project is named for its engine (`chromium-plain-http`, `firefox-alone`,
 *  `webkit-iphone`), and a name this cannot read takes every engine of the mode, so a project
 *  added there is never skipped here. */
const namedProjectEngines: Readonly<Record<string, PlaywrightEngine>> = {
  iphone: "chromium",
  stub: "chromium",
  "stub-phone": "chromium",
};

/** The engines the projects of this run launch: two-process mode has the one Chromium project,
 *  and every other run has Chromium, Firefox and WebKit projects. `projects` is
 *  `config.filteredProjects`, the projects this run selected: Playwright narrows that list by
 *  every `--project` of the command line before it runs this hook (`lib/runner/index.js:6466`,
 *  where `config.projects` stays the whole list the config file declares). A name no project
 *  engine is read from leaves the mode's whole set, which refuses too much rather than too
 *  little. */
function neededEngines(projects: readonly FullProject[]): readonly PlaywrightEngine[] {
  const mode: readonly PlaywrightEngine[] =
    harnessProcessCount === 2 ? ["chromium"] : playwrightEngines;
  const engines = new Set<PlaywrightEngine>();
  for (const project of projects) {
    const name = project.name.toLowerCase();
    const engine = namedProjectEngines[name] ?? mode.find((candidate) => name.includes(candidate));
    if (engine === undefined) return mode;
    engines.add(engine);
  }
  return engines.size === 0 ? mode : [...engines];
}

/** Refuses a run whose browsers Playwright has not downloaded, before a directory is made or a
 *  server starts. Playwright's own refusal comes when a worker launches the browser, with every
 *  stack's harness and Postgres already up and minutes of the run gone, and it names `playwright
 *  install`, which is not this package's command.
 *
 *  `e2e/run.ts` asked first whether its command ran Playwright's test runner at all, and exempted
 *  a listing from this check. Neither question is this file's: a listing (`--list`) runs no
 *  `globalSetup` at all, and nothing but a Playwright run reaches this module. */
async function refuseMissingBrowsers(projects: readonly FullProject[]): Promise<void> {
  // Playwright's own launchers, which answer where it put each engine. They are imported
  // statically: the runner that calls this hook has @playwright/test loaded already, so there is
  // nothing left to defer.
  const launchers: Readonly<Record<PlaywrightEngine, { executablePath(): string }>> = {
    chromium,
    firefox,
    webkit,
  };
  const missing = neededEngines(projects)
    .map((engine) => ({ engine, path: launchers[engine].executablePath() }))
    .filter((browser) => !existsSync(browser.path));
  if (missing.length === 0) return;
  const listed = missing.map(({ engine, path }) => `${engine} (no ${path})`).join(", ");
  throw new Error(
    `Playwright has not downloaded ${listed}. Run \`bun run e2e:install\` in packages/dispatch, ` +
      "a download of about 550 MB, then run this again. No harness and no Postgres were started."
  );
}

// WHY `@legion/contracts` IS NOT BUILT HERE. The specs and `e2e/agents.ts` import that package,
// Playwright loads them under Node, and Node takes the package's `default` export condition -
// `dist/`, which is gitignored - where a `bun` run takes its source. A checkout that has never
// built it dies with `Cannot find module @legion/contracts/dist/index.js`. This hook cannot cure
// that: `e2e/playwright.config.ts` imports `e2e/api.ts`, which imports the package, and
// Playwright loads the config before it runs any hook, so the run is already dead when this file
// is reached. The build belongs to whoever starts Playwright: `bun run e2e` and the package's
// other e2e scripts carry `build:contracts`, and every CI job that runs the suite itself carries
// a `Build contracts for dispatch e2e` step (.github/workflows/dispatch-browser.yaml).

/** The environment a reader of `stack` reads: this process's, with the index that selects that
 *  stack's record out of the manifest. `TEST_PARALLEL_INDEX` is the one place a reader picks its
 *  own stack (`e2e/harness-ports.ts`), so the helpers below answer for stack `i` here exactly as
 *  they answer in the worker Playwright gives that index to. The manifest is in `process.env` by
 *  the time this is called - `globalSetup` writes it as soon as the pool is up - and the record
 *  at that index is the stack's own variables, since that is what was written from them. */
function environmentOf(stack: Stack): NodeJS.ProcessEnv {
  return { ...process.env, TEST_PARALLEL_INDEX: String(stack.index) };
}

/** Puts a session in the stack's fake Envoy, asks the stack's server for it, and empties the fake
 *  again, whatever the answer was. */
async function checkStack(stack: Stack, count: number): Promise<void> {
  const env = environmentOf(stack);
  const ports = harnessPorts(env);
  const url = baseUrlOf(env);
  const which = count === 1 ? "" : ` (stack ${stack.index})`;
  const sentinel = {
    session_id: `e2e-preflight-${process.pid}-${stack.index}`,
    title: "e2e preflight",
  };
  const fake = `the fake Envoy this run started on 127.0.0.1:${ports.fakeEnvoy.port} (${ports.fakeEnvoy.variable}${which})`;
  const targets: readonly (DispatchProcess | undefined)[] =
    harnessProcessCount === 2 ? dispatchProcesses : [undefined];
  await setLiveSessions([sentinel], ports.fakeEnvoy.port);
  try {
    for (const target of targets) {
      const pinned = target === undefined ? "" : ` pinned to Dispatch ${target.toUpperCase()}`;
      const refusal = `The Dispatch e2e harness cannot start: ${url}${which}${pinned}`;
      const agents = await listAgents({ as: "agent", baseUrl: url, process: target }).catch(
        (error: unknown) => {
          throw new Error(
            `${refusal} did not list its agents, so this run cannot tell whether it reads ${fake}: ` +
              (error instanceof Error ? error.message : String(error))
          );
        }
      );
      if (!agents.some((agent) => agent.session_id === sentinel.session_id)) {
        throw new Error(
          `${refusal} does not list the session this run put in ${fake}, so it reads another ` +
            "Envoy listener. Give the server and this run one FAKE_ENVOY_PORT; the acceptance " +
            "Compose file derives the server's ENVOY_URL from the one its `up` is given " +
            "(packages/dispatch/README.md)."
        );
      }
    }
  } finally {
    await resetFakeEnvoy(ports.fakeEnvoy.port);
  }
}

/** Signs a person in on A and asks B who they are. The person row it writes is emptied by the
 *  first row's `resetDatabase`, as every row's are. Two-process mode runs one stack, so the URL
 *  is that stack's proxy. */
async function requireOneCookieKey(url: string): Promise<void> {
  const refusal = "The Dispatch e2e harness cannot start: Dispatch B";
  const signIn = await fetch(new URL(devSignInPath("alice"), url), {
    headers: pinnedTo("a"),
    redirect: "manual",
  });
  const cookie = sessionCookieFrom(signIn);
  if (signIn.status !== 302 || cookie === undefined) {
    throw new Error(
      `The Dispatch e2e harness cannot start: dev sign-in on Dispatch A answered ` +
        `${signIn.status} ${await signIn.text()}`
    );
  }
  const whoami = await fetch(new URL("/auth/whoami", url), {
    headers: { ...pinnedTo("b"), Cookie: cookie },
  });
  if (whoami.status !== 200) {
    throw new Error(
      `${refusal} answered ${whoami.status} ${await whoami.text()} to a session Dispatch A ` +
        "minted, so a person signed in on one process is signed out on the other. Both must sign " +
        "cookies with the key derived from the one DISPATCH_DEV_SIGNIN_SHARED_KEY run-server.sh " +
        "hands them from the harness's DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY (e2e/harness-listeners.ts)."
    );
  }
}

export default async function globalSetup(config: FullConfig): Promise<void> {
  // The two runs that address servers this one does not start. The stub-only answer is read
  // exactly as the config reads it (`e2e/stub/selection.ts`): the config ran first, in this
  // process, so this asks the same question of the same command line and the same environment.
  const stub = stubSelection({
    argv: process.argv,
    env: process.env,
    processes: harnessProcessCount,
    projects: stubProjectNames,
  });
  if (process.env.DISPATCH_E2E_REUSE_SERVERS === "1" || stub.onlyStub) return;

  // Before a directory is made or a server starts, so a run whose browsers are missing says so
  // whatever else is wrong with it.
  await refuseMissingBrowsers(config.filteredProjects);
  // `config.workers` is this run's resolved worker count, a `--workers` on the command line
  // included, and under `--shard` it is this process's own.
  const count = config.workers;
  const stacks = await startStacks(count);
  // Every worker inherits what this writes (Playwright carries the environment `globalSetup`
  // leaves behind into each worker process), and resolves its own stack out of the manifest
  // through `TEST_PARALLEL_INDEX`. Stack 0's values go under the plain names too, so every
  // process that is no worker - this one, and the harness's own children - reads a whole stack.
  process.env[stacksVariable] = JSON.stringify(stacks.map((stack) => stack.env));
  Object.assign(process.env, stacks[0].env);

  for (const stack of stacks) await checkStack(stack, count);
  if (harnessProcessCount === 2 && harnessSignIn === "dev") {
    await requireOneCookieKey(baseUrlOf(environmentOf(stacks[0])));
  }
  const which = count === 1 ? "the stack" : `each of the ${count} stacks`;
  console.error(`${named}: the preflight passed: ${which} answers from this run's fake Envoy`);
}
