/** The image provider serves each stack's Postgres on a unix socket in a directory docker mounts
 *  into the container (`e2e/harness-postgres.ts`), and a docker engine in a VM carries no unix
 *  socket out of that VM. On darwin, where every engine is such a VM, the provider is refused at
 *  once, naming the variable that runs a local Postgres instead, before any container starts;
 *  elsewhere it is made, and starts nothing until a stack asks.
 *
 *  A stack's start is refused too when its socket file would pass the 107 bytes a unix socket
 *  path holds: the image provider's server would otherwise bind that path inside its container,
 *  where nothing this process reads says why, and the run would spend the whole ready wait on a
 *  server that can never answer. */
import { afterEach, expect, test } from "bun:test";
import { mkdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { pgBinVariable, pgImageVariable, stackPostgres } from "../harness-postgres";

const platform = Object.getOwnPropertyDescriptor(process, "platform");
const made: string[] = [];
afterEach(() => {
  if (platform !== undefined) Object.defineProperty(process, "platform", platform);
  for (const directory of made.splice(0)) rmSync(directory, { force: true, recursive: true });
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
    `a docker engine in a VM (Docker Desktop, colima, and every engine on macOS) carries no unix socket out of that VM. Name a local Postgres installation's directory, the one holding initdb and pg_ctl, in ${pgBinVariable} instead`
  );
});

test("the image provider is made on linux", () => {
  expect(provider("linux")?.name).toBe("the Postgres image example.invalid/postgres@sha256:0");
});

/** Stack 0's socket file under a run directory of `length` bytes: what `socketUrl` measures. */
const socketOf = (length: number) =>
  join(`${tmpdir()}/`.padEnd(length, "d"), "pg-0", ".s.PGSQL.5432");

/** Starts stack 0 of the image provider under a run directory of `length` bytes, which this makes
 *  so the refusal is the path's length and not a missing directory. */
function startUnder(length: number) {
  const dir = `${tmpdir()}/`.padEnd(length, "d");
  mkdirSync(dir, { recursive: true });
  made.push(dir);
  const made0 = stackPostgres({
    dir,
    run: "socket-path-row",
    env: { [pgImageVariable]: "example.invalid/postgres@sha256:0" },
  });
  if (made0 === undefined) throw new Error("the image provider was not made");
  return made0.start(0);
}

test("a stack whose socket path would pass 107 bytes is refused at once, naming the path and its length", async () => {
  // 89 bytes of run directory is the first that does not fit: its socket file is 108 bytes, one
  // past the limit. The sibling row below holds the 88 that does, so this is the length alone.
  const refused = socketOf(89);
  expect(refused.length).toBe(108);
  await expect(startUnder(89)).rejects.toThrow(
    `the stack's socket ${refused} is 108 bytes, past the 107 a unix socket path holds, so no server could bind it: run it with a shorter TMPDIR.`
  );
});

test("a stack whose socket path is exactly 107 bytes is not refused for its length", async () => {
  // The longest run directory that fits. Its start gets past `socketUrl` and fails at `docker
  // run`, which this image name has no image for: the refusal names the image, not the path.
  expect(socketOf(88).length).toBe(107);
  await expect(startUnder(88)).rejects.toThrow(/did not start from example\.invalid\/postgres/);
});
