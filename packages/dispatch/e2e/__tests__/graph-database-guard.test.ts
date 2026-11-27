import { expect, test } from "bun:test";
import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const directories = temporaryDirectories("graph-database-guard");
const drivers = ["node-projects.sh", "graph-store.sh", "graph-sizing.sh"];
// The upgrade drivers, which drop the database DATABASE_URL names and create it empty, each
// through lib-tree.sh's tree_reset_database and its tree_database_parts, with lib.sh's
// loopback_database_url (view-arms-rollback.sh calls tree_database_parts at its start too).
const droppingDrivers = ["issue-artifacts.sh", "one-dependency.sh", "view-arms-rollback.sh"];
// A driver whose seed-decisions.sh seed_reset_database drops the database DATABASE_URL names and
// creates it empty, before it boots anything (answered-decisions.sh calls it too).
const seedingDriver = "answer-attribute-removal.sh";

/** A driver's run: its stderr, and whether it reached curl (started) or psql (connected). */
interface DriverRun {
  readonly stderr: string;
  readonly started: boolean;
  readonly connected: boolean;
}

// Run each real driver only through its guard. Its first HTTP call and its first psql call each
// leave a marker and fail, so a bad URL can reach neither a real server nor a database even if
// the guard lets it through. `env` adds the inputs a driver reads before its guard.
async function drive(
  driver: string,
  databaseUrl: string,
  env: Readonly<Record<string, string>> = {}
): Promise<DriverRun> {
  const bin = directories();
  const marker = join(bin, "http-started");
  const connectMarker = join(bin, "psql-started");
  writeFileSync(join(bin, "curl"), `#!/usr/bin/env bash\n: > '${marker}'\nexit 97\n`, {
    mode: 0o755,
  });
  writeFileSync(join(bin, "psql"), `#!/usr/bin/env bash\n: > '${connectMarker}'\nexit 97\n`, {
    mode: 0o755,
  });
  for (const command of ["envoy-dispatch", "jq"]) {
    writeFileSync(join(bin, command), "#!/usr/bin/env bash\nexit 97\n", { mode: 0o755 });
  }
  const result = await runBounded({
    argv: ["bash", `e2e/acceptance/${driver}`],
    bound: "short",
    cwd: dispatchPackage,
    env: {
      PATH: `${bin}:${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? "",
      DATABASE_URL: databaseUrl,
      DISPATCH_URL: "http://127.0.0.1:8777",
      DISPATCH_TOKEN: "test-token",
      PROJECT: "TEST",
      AGENT_SESSION: "test-session",
      DEV_SIGNIN_LOGIN: "alice",
      FAKE_ENVOY_PORT: "9023",
      PRODUCTION_PROJECT: "test-project",
      PRODUCTION_DATABASE_ID: "test-project:test-database",
      ...env,
    },
    from: import.meta.url,
  });
  return {
    stderr: result.stderr,
    started: existsSync(marker),
    connected: existsSync(connectMarker),
  };
}

// A refusal names DATABASE_URL and the loopback rule, and comes before any HTTP request or psql
// call.
function expectRefused(run: DriverRun) {
  expect(run.started, run.stderr).toBe(false);
  expect(run.connected, run.stderr).toBe(false);
  expect(run.stderr).toContain("DATABASE_URL");
  expect(run.stderr).toContain("loopback");
}

for (const driver of drivers) {
  for (const databaseUrl of [
    "postgres://user@127.0.0.1:5433/mine?sslmode=disable",
    "postgresql://localhost/mine",
    "postgres://user@[::1]:5433/mine",
  ]) {
    test(
      `${driver} accepts the loopback authority ${new URL(databaseUrl).hostname}`,
      async () => {
        const run = await drive(driver, databaseUrl);
        expect(run.started, run.stderr).toBe(true);
      },
      rowLimit("short")
    );
  }

  for (const { shape, databaseUrl } of [
    {
      shape: "a loopback string in a remote URL's path",
      databaseUrl: "postgres://user@remote.invalid/db/@127.0.0.1:5433/",
    },
    {
      shape: "a loopback string in a remote URL's query",
      databaseUrl: "postgres://user@remote.invalid/db?application_name=@127.0.0.1:5433",
    },
    {
      shape: "a host override",
      databaseUrl: "postgres://user@127.0.0.1:5433/db?host=remote.invalid",
    },
    {
      shape: "an encoded host override",
      databaseUrl: "postgres://user@127.0.0.1:5433/db?ho%73t=remote.invalid",
    },
    {
      shape: "a host override after a newline",
      databaseUrl: "postgres://user@127.0.0.1:5433/db?application_name=test\n&host=remote.invalid",
    },
    {
      shape: "a query value holding @ that libpq reads as the host",
      databaseUrl: "postgres://127.0.0.1?application_name=@remote.invalid/db",
    },
    {
      shape: "a query value holding @ after other parameters",
      databaseUrl: "postgres://127.0.0.1?sslmode=disable&x=@remote.invalid:5432/db",
    },
    {
      shape: "a remote host listed before a loopback one",
      databaseUrl: "postgres://u@remote.invalid,@127.0.0.1:5433/db",
    },
    {
      shape: "a remote host and port listed before a loopback one",
      databaseUrl: "postgres://u@remote.invalid:5432,x@127.0.0.1/db",
    },
    {
      shape: "a hostaddr override",
      databaseUrl: "postgres://user@localhost/db?hostaddr=192.0.2.1",
    },
    { shape: "a service override", databaseUrl: "postgres://user@localhost/db?service=remote" },
  ]) {
    test(
      `${driver} refuses ${shape} before any HTTP request`,
      async () => {
        expectRefused(await drive(driver, databaseUrl));
      },
      rowLimit("short")
    );
  }
}

// Each URL names the database the upgrade drivers drop, on a loopback authority, and an override
// libpq connects by instead: a check of the authority's host alone would drop that database on
// another server.
for (const driver of droppingDrivers) {
  for (const { shape, databaseUrl } of [
    {
      shape: "a host override",
      databaseUrl: "postgres://user@127.0.0.1:5433/infra_1253_upgrade?host=remote.invalid",
    },
    {
      shape: "a hostaddr override",
      databaseUrl: "postgres://user@localhost/infra_1253_upgrade?hostaddr=192.0.2.1",
    },
    {
      shape: "a service override",
      databaseUrl: "postgres://user@localhost/infra_1253_upgrade?service=remote",
    },
  ]) {
    test(
      `${driver} refuses ${shape} before any HTTP request or database call`,
      async () => {
        const run = await drive(driver, databaseUrl);
        expectRefused(run);
        // The refusal names the driver its user ran, not lib-tree.sh, which holds the guard.
        expect(run.stderr).toContain(`${driver}: DATABASE_URL`);
      },
      rowLimit("short")
    );
  }
}

// Each URL names a database seed_reset_database drops, on a loopback authority, and an override
// libpq connects by instead: its check of the authority's host alone dropped that database on
// another server.
for (const { shape, databaseUrl } of [
  {
    shape: "a host override",
    databaseUrl: "postgres://user@127.0.0.1:5433/infra_1150_guard?host=remote.invalid",
  },
  {
    shape: "a hostaddr override",
    databaseUrl: "postgres://user@localhost/infra_1150_guard?hostaddr=192.0.2.1",
  },
  {
    shape: "a service override",
    databaseUrl: "postgres://user@localhost/infra_1150_guard?service=remote",
  },
]) {
  test(
    `${seedingDriver} refuses ${shape} before any HTTP request or database call`,
    async () => {
      // BASELINE_SHA names a commit of this checkout, and BASELINE_BINARY spares the run a TMPDIR:
      // the driver reads both before the reset, and never starts that binary.
      const run = await drive(seedingDriver, databaseUrl, {
        BASELINE_SHA: "HEAD",
        BASELINE_BINARY: "/nonexistent/base-dispatch",
      });
      expect(run.started, run.stderr).toBe(false);
      expect(run.connected, run.stderr).toBe(false);
      expect(run.stderr).toContain("seed-decisions.sh: ");
      expect(run.stderr).toContain("loopback");
    },
    rowLimit("short")
  );
}
