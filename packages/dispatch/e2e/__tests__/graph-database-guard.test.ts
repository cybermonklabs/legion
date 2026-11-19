import { expect, test } from "bun:test";
import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const directories = temporaryDirectories("graph-database-guard");
const drivers = ["node-projects.sh", "graph-store.sh", "graph-erase.sh", "graph-sizing.sh"];

// Run each real driver only through its guard. Its first HTTP call leaves a marker and fails,
// so a bad URL can reach neither a real server nor a database even if the guard lets it through.
async function drive(driver: string, databaseUrl: string) {
  const bin = directories();
  const marker = join(bin, "http-started");
  writeFileSync(join(bin, "curl"), `#!/usr/bin/env bash\n: > '${marker}'\nexit 97\n`, {
    mode: 0o755,
  });
  for (const command of ["psql", "envoy-dispatch", "jq"]) {
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
    },
    from: import.meta.url,
  });
  return { ...result, started: existsSync(marker) };
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
        const run = await drive(driver, databaseUrl);
        expect(run.started, run.stderr).toBe(false);
        expect(run.stderr).toContain("DATABASE_URL");
        expect(run.stderr).toContain("loopback");
      },
      rowLimit("short")
    );
  }
}
