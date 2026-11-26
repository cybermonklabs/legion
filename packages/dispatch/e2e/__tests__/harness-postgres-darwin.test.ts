/** The image provider serves each stack's Postgres on a unix socket in a directory docker mounts
 *  into the container (`e2e/harness-postgres.ts`), and Docker Desktop on macOS carries no unix
 *  socket out of its VM. On darwin the provider is refused at once, naming the variable that runs a
 *  local Postgres instead, before any container starts; elsewhere it is made, and starts nothing
 *  until a stack asks. */
import { afterEach, expect, test } from "bun:test";

import { pgBinVariable, pgImageVariable, stackPostgres } from "../harness-postgres";

const platform = Object.getOwnPropertyDescriptor(process, "platform");
afterEach(() => {
  if (platform !== undefined) Object.defineProperty(process, "platform", platform);
});

function provider(on: NodeJS.Platform) {
  Object.defineProperty(process, "platform", { value: on, configurable: true });
  return stackPostgres({
    dir: "/nonexistent",
    run: "darwin-row",
    env: { [pgImageVariable]: "example.invalid/postgres@sha256:0" },
  });
}

test("the image provider is refused on darwin, naming the variable that runs a local Postgres", () => {
  expect(() => provider("darwin")).toThrow(
    `Docker Desktop on macOS cannot carry a unix socket out of its VM. Name a local Postgres installation's directory, the one holding initdb and pg_ctl, in ${pgBinVariable} instead`
  );
});

test("the image provider is made on linux", () => {
  expect(provider("linux")?.name).toBe("the Postgres image example.invalid/postgres@sha256:0");
});
