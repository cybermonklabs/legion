/** The two refusals `e2e/harness-postgres.ts` makes before a stack's Postgres is started.
 *
 *  The image provider serves each stack's Postgres on a unix socket in a directory docker mounts
 *  into the container, and a docker engine in a VM carries no unix socket out of that VM. On
 *  darwin, where every engine is such a VM, the provider is refused at once, naming the variable
 *  that runs a local Postgres instead, before any container starts; elsewhere it is made, and
 *  starts nothing until a stack asks.
 *
 *  A stack's URL is refused when its socket file would pass the 107 bytes a unix socket path
 *  holds. These rows call `socketUrl`, which holds that refusal, rather than starting a stack: a
 *  start reaches it only after `go` has printed the connection limit and docker has pulled the
 *  image, minutes of work that say nothing about the path. An earlier draft started one, and its
 *  row timed out at 60 s in CI's dispatch-ui job on the `go` call alone. */
import { afterEach, expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { pgBinVariable, pgImageVariable, socketUrl, stackPostgres } from "../harness-postgres";

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
    `a docker engine in a VM (Docker Desktop, colima, and every engine on macOS) carries no unix socket out of that VM. Name a local Postgres installation's directory, the one holding initdb and pg_ctl, in ${pgBinVariable} instead`
  );
});

test("the image provider is made on linux", () => {
  expect(provider("linux")?.name).toBe("the Postgres image example.invalid/postgres@sha256:0");
});

/** A stack's socket directory `length` bytes long, under a name no run uses. */
const directoryOf = (length: number) => join(tmpdir(), "s".repeat(length - tmpdir().length - 1));

test("a socket path past 107 bytes is refused, naming the path and its length", () => {
  // 94 bytes of directory is the first that does not fit: its socket file is 108 bytes, one past
  // the limit. The row below holds the 93 that does, so this is the length alone.
  const directory = directoryOf(94);
  const socket = join(directory, ".s.PGSQL.5432");
  expect(socket.length).toBe(108);
  expect(() => socketUrl("postgres:dispatch", "dispatch_e2e", directory)).toThrow(
    `the stack's socket ${socket} is 108 bytes, past the 107 a unix socket path holds, so no server could bind it: run it with a shorter TMPDIR.`
  );
});

test("a socket path of exactly 107 bytes is not refused, and names its directory", () => {
  const directory = directoryOf(93);
  expect(join(directory, ".s.PGSQL.5432").length).toBe(107);
  expect(socketUrl("postgres:dispatch", "dispatch_e2e", directory)).toBe(
    `postgres://postgres:dispatch@/dispatch_e2e?host=${encodeURIComponent(directory)}&port=5432&sslmode=disable`
  );
});

test("a socket directory holding a comma is refused, whatever its length", () => {
  expect(() => socketUrl("postgres", "dispatch_e2e", join(tmpdir(), "a,b"))).toThrow(
    "holds a comma, which pgx and libpq read as a list of hosts"
  );
});
