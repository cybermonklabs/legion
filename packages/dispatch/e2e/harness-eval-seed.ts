/** The eval's seed, which a stack with the real models (`DISPATCH_E2E_MODELS=real`,
 *  e2e/harness-models.ts) serves: the seeded Dispatch of the command bar's eval
 *  (`packages/envoy/going/eval/seed.json`), so a person tries the agent on the items the eval's
 *  tasks name. e2e/harness-start.ts calls it once the fakes serve and before the Dispatch server
 *  starts, so the server boots on the seeded database.
 *
 *  The database is written by `go run ./cmd/going-eval seed` (eval.WriteSeed in
 *  packages/envoy/going/eval), which migrates it and writes each item with the times the seed
 *  fixes, or writes nothing where the database already holds the seed, so a stack started again on
 *  its own data keeps every write made since. The seed's agent sessions are Envoy's, not
 *  Dispatch's: the command writes them to a file, and this module hands them to the stack's fake
 *  Envoy, each live, unresponsive since the time the seed gives, or gone. */

import { execFile } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { harnessVariables } from "./harness-ports";

const envoyPackage = fileURLToPath(new URL("../../envoy", import.meta.url));

/** How long the seed may take: a cold `go run` builds the command first, as run-server.sh builds
 *  the server, so it gets the server's ten minutes. */
const seedMs = 600_000;

/** One agent session of the seed, as `going-eval seed --envoy-fixture-out` writes it
 *  (eval.Session). */
interface SeedSession {
  readonly id: string;
  readonly role: string;
  readonly title: string;
  readonly state: "live" | "unresponsive" | "gone";
  /** When it was last seen. A gone session has none, since Envoy keeps no record of it; every
   *  other session has one, which eval.LoadSeed refuses a seed without. */
  readonly last_seen?: string;
}

/** Seeds the database `environment.DATABASE_URL` names with the eval's seed, and puts the seed's
 *  sessions in the fake Envoy `environment` names the port of. A failure is thrown with the
 *  command's own words. */
export async function seedEval(
  environment: NodeJS.ProcessEnv,
  signal?: AbortSignal
): Promise<void> {
  const databaseURL = environment.DATABASE_URL;
  const envoyPort = environment[harnessVariables.fakeEnvoy];
  if (!databaseURL || !envoyPort) {
    throw new Error(
      `the eval's seed needs DATABASE_URL and ${harnessVariables.fakeEnvoy}, the database it ` +
        "fills and the fake Envoy that holds its sessions"
    );
  }
  const dir = mkdtempSync(join(tmpdir(), "dispatch-e2e-eval-seed-"));
  try {
    const fixture = join(dir, "sessions.json");
    try {
      const { stdout } = await promisify(execFile)(
        "go",
        [
          "run",
          "./cmd/going-eval",
          "seed",
          "--database-url",
          databaseURL,
          "--envoy-fixture-out",
          fixture,
        ],
        { cwd: envoyPackage, env: environment, maxBuffer: 1 << 20, signal, timeout: seedMs }
      );
      process.stderr.write(stdout);
    } catch (error) {
      // execFile's error carries the command's stderr, which says why it refused.
      const stderr =
        error instanceof Error && "stderr" in error ? String(error.stderr ?? "").trim() : "";
      throw new Error(
        `The Dispatch e2e harness could not write the eval's seed${stderr === "" ? "" : `: ${stderr}`}`
      );
    }
    const sessions = JSON.parse(readFileSync(fixture, "utf8")) as SeedSession[];
    await handEnvoy(`http://127.0.0.1:${envoyPort}`, sessions);
  } finally {
    rmSync(dir, { force: true, recursive: true });
  }
}

/** Seeds the fake Envoy at `origin` with `sessions`: each one registered, then an unresponsive one
 *  set so since its last heartbeat and a gone one dropped from the listener's store and list, as
 *  e2e/fake-envoy.ts models a session that has ended. A live one is stamped live at each read. */
async function handEnvoy(origin: string, sessions: readonly SeedSession[]): Promise<void> {
  await fixtureCall(
    origin,
    "PUT",
    "/__fixture/sessions",
    sessions.map((session) => ({ session_id: session.id, title: session.title }))
  );
  for (const session of sessions) {
    const path = `/__fixture/sessions/${encodeURIComponent(session.id)}`;
    if (session.state === "unresponsive") {
      await fixtureCall(origin, "PATCH", path, {
        last_seen: Date.parse(session.last_seen ?? ""),
        state: "unresponsive",
      });
    } else if (session.state === "gone") {
      await fixtureCall(origin, "PATCH", path, { record: "absent" });
    }
  }
}

/** One fixture call to the fake Envoy, refused naming the route on a status other than 200. */
async function fixtureCall(
  origin: string,
  method: string,
  path: string,
  body: unknown
): Promise<void> {
  const response = await fetch(`${origin}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method,
  });
  if (response.status !== 200) {
    throw new Error(
      `the fake Envoy refused ${method} ${path}: ${response.status} ${await response.text()}`
    );
  }
}
