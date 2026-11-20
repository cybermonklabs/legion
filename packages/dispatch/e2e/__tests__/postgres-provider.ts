/** What a test of the stack's code that needs a Postgres does where the run gives it none.
 *
 *  Two kinds of test need one. A test that starts a stack's Postgres takes it from one of the
 *  two providers `e2e/harness-postgres.ts` reads: `DISPATCH_E2E_PG_IMAGE` or
 *  `DISPATCH_E2E_PG_BIN`. A test of the stack's psql, or of the acceptance driver's guard, makes
 *  a database of its own on the server `LEGION_TEST_PG_DSN` names, through psql on PATH.
 *
 *  Outside CI, a developer's run that lacks what a test needs skips the test and says so on one
 *  line. In CI it never skips: the test fails and names what to set, so a job that runs it gives
 *  it a Postgres or goes red. GitHub Actions sets `CI=true` in every job.
 *
 *  Where each test runs. The `dispatch-ui` job of `.github/workflows/ci.yaml` runs this
 *  directory with a Postgres service and `LEGION_TEST_PG_DSN`, and with no stack provider, so its
 *  `bun test` leaves out every file that calls `requirePostgres`, through one
 *  `--path-ignore-patterns` per file in the harness lane's script, `test:harness` of
 *  `packages/dispatch/package.json`, which its `test` script runs, and so does the weekly repeated
 *  job (`test:repeat`). A job of `.github/workflows/dispatch-browser.yaml` that names a provider
 *  runs each of those files. `postgres-provider.test.ts` holds the three together: a file that
 *  calls `requirePostgres` and is not left out of that lane fails the lane, and one that is left
 *  out and that no such job names runs nowhere, which that test refuses.
 *
 *  `bun test` collects only a file whose name carries a `test` or `spec` token, so this module
 *  runs nowhere by itself. */

import { hostTool } from "@legion/test-fixtures/host-tools";

import { pgBinVariable, pgImageVariable } from "../harness-postgres";

/** A variable that names a stack's Postgres provider. */
export type ProviderVariable = typeof pgBinVariable | typeof pgImageVariable;

/** Both providers, for a test that runs on either. */
export const eitherProvider: readonly ProviderVariable[] = [pgBinVariable, pgImageVariable];

/** The variable that names a Postgres server a test may make a database on and drop it again:
 *  an issue pod's shared test database, a developer's server, or a CI job's service. */
export const serverVariable = "LEGION_TEST_PG_DSN";

/** Whether this run is a CI job's. */
export function inCi(env: NodeJS.ProcessEnv = process.env): boolean {
  return (env.CI ?? "").trim().toLowerCase() === "true";
}

/** The first of `needs` that names a provider in `env`, with its value, or undefined. */
function namedProvider(
  needs: readonly ProviderVariable[],
  env: NodeJS.ProcessEnv
): { readonly variable: ProviderVariable; readonly value: string } | undefined {
  for (const variable of needs) {
    const value = (env[variable] ?? "").trim();
    if (value !== "") return { variable, value };
  }
  return undefined;
}

/** Whether the rows of `file` skip: only outside CI, where none of `needs` names a provider, and
 *  then this prints one line that names the file and the variables. In CI it answers false, so
 *  the row runs and `requirePostgres` fails it. */
export function skipsWithoutPostgres(
  file: string,
  needs: readonly ProviderVariable[],
  env: NodeJS.ProcessEnv = process.env
): boolean {
  if (namedProvider(needs, env) !== undefined || inCi(env)) return false;
  console.error(`${file}: skipped, since ${needs.join(" or ")} names no Postgres for its stacks.`);
  return true;
}

/** The provider a row runs with. A row calls this first. It throws where none of `needs` names
 *  one, which is a CI run, since `skipsWithoutPostgres` skips the row everywhere else; the
 *  message names each variable that would give the row its Postgres. */
export function requirePostgres(
  needs: readonly ProviderVariable[],
  env: NodeJS.ProcessEnv = process.env
): { readonly variable: ProviderVariable; readonly value: string } {
  const named = namedProvider(needs, env);
  if (named !== undefined) return named;
  throw new Error(
    "This test of the stack needs a Postgres and this CI job gives it none: set " +
      `${needs.join(" or ")} in the job's step (e2e/harness-postgres.ts), or run the test in a ` +
      "job that sets it."
  );
}

/** What this host and run lack for a row that asks a Postgres server through psql: psql on PATH,
 *  and, where the row needs a server, `serverVariable` naming it; `server` says what it must
 *  name, as "a server this run may read". */
export function missingForServer(
  server: string | undefined,
  env: NodeJS.ProcessEnv = process.env
): string[] {
  return [
    ...(hostTool("psql") === null ? ["psql on this host's PATH"] : []),
    ...(server !== undefined && (env[serverVariable] ?? "").trim() === ""
      ? [`${serverVariable}, naming ${server}`]
      : []),
  ];
}

/** Whether the rows of `file` that need `missing` skip: only outside CI, where `missing` is not
 *  empty, and then this prints one line that names the file and each thing missing. In CI it
 *  answers false, so the row runs and `requireNothingMissing` fails it. */
export function skipsWithout(
  file: string,
  missing: readonly string[],
  env: NodeJS.ProcessEnv = process.env
): boolean {
  if (missing.length === 0 || inCi(env)) return false;
  console.error(`${file}: skipped, since this run does not have: ${missing.join("; ")}.`);
  return true;
}

/** Throws naming each of `missing`. A row whose skip `skipsWithout` decided calls this first
 *  with the same list, so in CI it fails where it would have skipped. */
export function requireNothingMissing(missing: readonly string[]): void {
  if (missing.length === 0) return;
  throw new Error(
    `This test needs a Postgres and this CI job does not have: ${missing.join("; ")}. Give the ` +
      "job what is missing, or run the test in a job that has it."
  );
}
