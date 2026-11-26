/** `bun run dispatch:local`: the e2e harness, left running and seeded, with a signed-in URL.
 *  It starts the harness through e2e/harness-start.ts, which a Playwright run's stack pool
 *  (e2e/harness-pool.ts) starts too, on ports the OS chooses (or those the port variables set),
 *  and prints them, so `DISPATCH_E2E_REUSE_SERVERS=1 <those ports> bunx playwright test ...` runs
 *  any spec against this instance — and empties its database and its fake Envoy, so restart it
 *  to reseed. */
import { fileURLToPath } from "node:url";

import { harnessVariables } from "./harness-ports";
import { harnessProcessCount } from "./harness-processes";
import { type Harness, startHarness } from "./harness-start";
import {
  agentViewRefusal,
  awaitPostgres,
  databaseUrlRefusal,
  handOverMessage,
  Interrupted,
  type PostgresProbe,
  Supervisor,
} from "./local-instance-support";
import { sqlAt } from "./psql";
import { sessionIds } from "./session-ids";

const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
const envoyPackage = fileURLToPath(new URL("../../envoy", import.meta.url));
/** The server packages/envoy/scripts/dev-postgres.sh serves, at its `postgres` database. */
const adminUrl = "postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable";
const localDatabase = "dispatch_local";

/** Every foreground step this command runs. Ctrl-C or a SIGTERM interrupts it from the first step
 *  on, so a stop during the web build, the server build or the seed stops what has started rather
 *  than leaving it listening: the harness stops its own processes when its signal fires. */
const supervisor = new Supervisor();
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => supervisor.interrupt());
}
const untilInterrupted = <T>(step: Promise<T>) => supervisor.untilInterrupted(step);
/** psql against the dev container's admin database, killed by an interrupt like every child. */
const adminSql = (...statements: string[]) =>
  untilInterrupted(sqlAt(adminUrl, statements, supervisor.signal));

function check(ok: boolean, what: string): void {
  if (!ok) throw new Error(`smoke failed: ${what}`);
}

/** The repository's dev Postgres through packages/envoy/scripts/dev-postgres.sh, which creates,
 *  starts or reuses the `dispatch-pg` container and refuses one on any image but its pinned
 *  pgvector one, and its database `dispatch_local`, created once. `boundMs` is how long the
 *  server may take to answer once the script returns. psql runs through e2e/psql.ts, without
 *  PGHOSTADDR: libpq would otherwise connect to that address instead of the URL's host, and no
 *  check here or in the server sees it. */
async function ensureLocalPostgres(boundMs: number): Promise<string> {
  await supervisor.run(["bash", "scripts/dev-postgres.sh"], envoyPackage);
  // Each attempt is a psql of its own, killed once the time the wait left it has passed, so no
  // psql outlives its attempt. The kill's own error says only that it was aborted, so the attempt
  // says what happened. This message is the one a failure names: the kill's timer starts just
  // before the wait's own timer of the same length (`answeredWithin`), and the killed psql
  // rejects before that timer fires.
  const probe: PostgresProbe = async (query, limitMs) => {
    const limit = AbortSignal.timeout(limitMs);
    try {
      return await sqlAt(adminUrl, [query], AbortSignal.any([supervisor.signal, limit]));
    } catch (error) {
      if (!limit.aborted) throw error;
      throw new Error(`psql had not answered within ${(limitMs / 1_000).toFixed(1)} s`);
    }
  };
  await awaitPostgres(new URL(adminUrl).host, probe, boundMs, supervisor.signal);
  if ((await adminSql(`select 1 from pg_database where datname = '${localDatabase}'`)) === "") {
    await adminSql(`create database ${localDatabase}`);
  }
  return adminUrl.replace("/postgres?", `/${localDatabase}?`);
}

let harness: Harness | undefined;

async function main(): Promise<void> {
  // 1. Preconditions: no deployed-server override, the tools, a loopback database.
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("unset PLAYWRIGHT_BASE_URL: this command starts its own servers");
  }
  // It builds, seeds and hands out one server, where two-process mode needs B's build of its own
  // (DISPATCH_E2E_WEB_DIST_B) and serves only the multiprocess specs `bun run e2e:multiprocess` runs.
  if (harnessProcessCount !== 1) {
    throw new Error(
      "unset DISPATCH_E2E_PROCESSES: this command runs one Dispatch server; " +
        "`bun run e2e:multiprocess` runs two"
    );
  }
  const given = process.env.DATABASE_URL?.trim();
  for (const tool of ["go", "psql", "flock", "openssl", "python3", ...(given ? [] : ["docker"])]) {
    if (!Bun.which(tool)) throw new Error(`${tool} is required on PATH`);
  }
  const refusal = given ? databaseUrlRefusal(given) : undefined;
  if (refusal !== undefined) throw new Error(refusal);

  // 2. Postgres: DATABASE_URL as given (it is truncated and seeded), else the repo's dev container.
  // It stays ahead of the web build, so a database problem is the failure reported.
  // A new container runs initdb before it takes connections, so the wait gives it 30 s.
  const postgresBoundMs = 30_000;
  const databaseUrl = given || (await ensureLocalPostgres(postgresBoundMs));
  process.env.DATABASE_URL = databaseUrl;

  // 3. The SPA, which the server reads from web/dist on every request.
  await supervisor.run(["bun", "run", "build:web"], dispatchPackage);

  // 4. The harness, ready once every listener reported its port and the server's /healthz
  // answered. An interrupt while it starts stops what it started, and it rejects with Interrupted.
  harness = await startHarness({ signal: supervisor.signal });
  // The e2e modules read the ports when they are first imported, so they are imported only now.
  Object.assign(process.env, harness.env);
  const { baseUrl, listAgents, listBroadcasts, userHeaders } = await import("./api");
  const { resetDatabase } = await import("./seed");
  const { seedWorkspace, workspaceSessionIds } = await import("./workspace");

  // 5. Reset, seed, and prove the signed-in surfaces over HTTP before handing out the URL. The
  // broadcast's recipient ORDER is deliberately not checked here: e2e/broadcast.e2e.ts owns that
  // assertion, and this instance has to start on a server without the fix so that spec can go
  // red against it. The reset runs only now, after the server's own loopback check let it boot.
  await untilInterrupted(resetDatabase());
  // The Inbox was read settling in 230 ms (the sampler e2e/workspace.ts cites); 15 s is far
  // above that, so only a seed that does not settle fails it.
  const seedBoundMs = 15_000;
  const seeded = await untilInterrupted(
    seedWorkspace({ boundMs: seedBoundMs, signal: supervisor.signal })
  );
  const whoami = await fetch(`${baseUrl}/auth/whoami`, { headers: await userHeaders("alice") });
  const identity = (await whoami.json()) as { login?: string };
  check(whoami.status === 200 && identity.login === "alice", "signed-in whoami");
  console.log(seeded.inboxLine);
  const [newest] = await listBroadcasts({ login: "alice" });
  check(newest?.id === seeded.broadcast.id, "broadcast listed");
  const agents = new Set((await listAgents({ login: "alice" })).map((agent) => agent.session_id));
  check(
    workspaceSessionIds.every((id) => agents.has(id)),
    "agents listed"
  );
  // The live view rides the live socket, not the API, and Dispatch refuses an agent view whose
  // session id is not a uuid: a seed of named sessions passes every check above and still reads
  // "This session's conversation is unavailable" at /agents/<id>/live.
  // Dispatch answers an agent view's subscribe at once, accepted or refused, so 10 s fails only
  // a Dispatch that does not answer.
  const liveViewBoundMs = 10_000;
  const liveView = await untilInterrupted(
    agentViewRefusal(baseUrl, await userHeaders("alice"), sessionIds.planner, liveViewBoundMs)
  );
  check(liveView === undefined, `the Planner's live view: ${liveView}`);

  // 6. Hand over, and run until Ctrl-C or until one of the processes stops by itself. A password
  // the person gave in DATABASE_URL is printed nowhere (`handOverMessage`).
  console.log(
    handOverMessage({
      baseUrl,
      broadcastId: seeded.broadcast.id,
      plannerSessionId: sessionIds.planner,
      fakeEnvoy: `http://127.0.0.1:${harness.env[harnessVariables.fakeEnvoy]}`,
      databaseUrl,
      supplied: Boolean(given),
      ports: harness.env,
      env: process.env,
    })
  );
  try {
    await untilInterrupted(harness.firstExit());
  } catch (error) {
    if (error instanceof Interrupted) return;
    throw error;
  }
}

let exitCode = 0;
try {
  await main();
  console.log("Stopping the Dispatch local instance.");
} catch (error) {
  if (error instanceof Interrupted) {
    // The harness's start names what it was waiting for when the Ctrl-C came
    // (`interruptedBefore`, e2e/local-instance-support.ts): its waits have no bound of their own,
    // so that name is the only account of a process that never came up. A bare "interrupted" says
    // nothing more than this line, and is left out.
    const why = ["", "interrupted"].includes(error.message) ? "" : `: ${error.message}`;
    console.error(`dispatch:local interrupted before the instance was up${why}`);
    exitCode = 130;
  } else {
    console.error(
      `dispatch:local failed: ${error instanceof Error ? error.message : String(error)}`
    );
    exitCode = 1;
  }
}
await harness?.stop();
await supervisor.stop();
process.exit(exitCode);
