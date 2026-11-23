/** What `bun run dispatch:local` (e2e/local-instance.ts) prints once its instance is up. It quotes
 *  through @legion/envoy-client's shellQuote, which node reads only from that package's build, so
 *  it lives apart from e2e/local-instance-support.ts, which Playwright's node runner loads. Bun
 *  runs everything that imports it: e2e/local-instance.ts and e2e/__tests__/psql-password.test.ts.
 *  Nothing here runs on import. */
import { shellQuote } from "@legion/envoy-client/shell-quote";
import { splitDatabaseUrl } from "./pg-url";

/** What `handOverMessage` prints from. */
export interface HandOver {
  readonly baseUrl: string;
  readonly broadcastId: string;
  readonly plannerSessionId: string;
  readonly fakeEnvoy: string;
  /** The database the instance truncates and seeds. */
  readonly databaseUrl: string;
  /** Whether a person gave `databaseUrl` in DATABASE_URL, rather than the command making it on
   *  the repository's dev container. */
  readonly supplied: boolean;
  /** Each port the harness reported, by its variable. */
  readonly ports: Readonly<Record<string, string>>;
  /** The environment the broker switch (e2e/harness-broker.ts) is read from. */
  readonly env: Readonly<Record<string, string | undefined>>;
}

/** What `bun run dispatch:local` prints once its instance is up: the URLs to open, the database,
 *  and a command that runs a spec against the instance, with each harness input it needs.
 *
 *  A password a person supplied in DATABASE_URL is printed nowhere. The database line and the
 *  command carry the URL without it (`splitDatabaseUrl`, the split psql gets), and a line says to
 *  give it in PGPASSWORD instead. The command's run has DISPATCH_E2E_REUSE_SERVERS=1, for which
 *  e2e/global-setup.ts starts no server and no Postgres, so its one reader of DATABASE_URL is
 *  e2e/psql.ts, which hands psql a URL that holds no password whole, with the caller's own
 *  PGPASSWORD in psql's environment. The dev container's URL, which the command makes when no
 *  DATABASE_URL is given, is printed whole: its one password, `dispatch`, is a literal of this
 *  repository (packages/envoy/scripts/dev-postgres.sh, which prints that URL itself). */
export function handOverMessage(handOver: HandOver): string {
  const { baseUrl, env, fakeEnvoy } = handOver;
  const database = handOver.supplied
    ? splitDatabaseUrl(handOver.databaseUrl)
    : { url: handOver.databaseUrl, password: undefined };
  const passwordLeftOut = database.password !== undefined;
  const switches = [
    "DISPATCH_E2E_AGENT_SECRETS_URL",
    "DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE",
  ].filter((variable) => env[variable] !== undefined);
  const harnessEnvironment = [
    `DATABASE_URL=${shellQuote(database.url)}`,
    ...Object.entries(handOver.ports).map(([variable, port]) => `${variable}=${port}`),
    ...switches.map((variable) => `${variable}=${shellQuote(env[variable] ?? "")}`),
  ].join(" ");
  return (
    "\nDispatch local instance is up.\n" +
    `  Open (signed in as alice): ${baseUrl}/auth/_dev/signin?login=alice&next=/\n` +
    `  Sign in as bob:            ${baseUrl}/auth/_dev/signin?login=bob&next=/\n` +
    `  Seeded broadcast:          ${baseUrl}/agents/broadcasts/${handOver.broadcastId}\n` +
    `  Planner's live view:       ${baseUrl}/agents/${handOver.plannerSessionId}/live\n` +
    `  Agent bearer:              e2e-token (for a local Oh My Pi session: DISPATCH_URL=${baseUrl} ` +
    "DISPATCH_TOKEN=e2e-token, with DISPATCH_TOKEN_FILE unset since it wins)\n" +
    `  Database (truncated and reseeded on every start): ${database.url}\n` +
    (passwordLeftOut
      ? "  (The password of the DATABASE_URL you gave is printed nowhere here.)\n"
      : "") +
    `  Fake Envoy: live sessions GET ${fakeEnvoy}/v1/sessions; ` +
    `reseed them with PUT ${fakeEnvoy}/__fixture/sessions\n` +
    "  Run a spec against it from packages/dispatch (this empties the instance; restart it to " +
    "reseed)" +
    (passwordLeftOut
      ? ", with the password of the DATABASE_URL you gave exported as PGPASSWORD in that shell, " +
        "where the run's psql reads it"
      : "") +
    ":\n" +
    `    DISPATCH_E2E_REUSE_SERVERS=1 ${harnessEnvironment} bunx ` +
    "playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts\n" +
    "Open exactly that origin: 'localhost' is another Host and the server refuses it (421).\n" +
    "Press Ctrl-C to stop."
  );
}
