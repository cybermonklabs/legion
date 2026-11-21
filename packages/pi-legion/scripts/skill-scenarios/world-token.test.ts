import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import { holds, nameWords, vocabulary } from "./run-names";
import { spawnChecked } from "./test-programs";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

// The Dispatch bearer of a run's world is in the file every agent's DISPATCH_TOKEN_FILE names, and
// an agent can `cat` it. It was the Dispatch harness's own `e2e-token` (packages/dispatch/e2e/
// run-server.sh), which names the harness, and no reading of the launch guard saw it, since the
// guard reads what the rig composes (INFRA-764, 2026-11-16). So each batch's bearer is its own, 24
// random bytes in hex, the 48 letters and digits of the agent token a deployment of Dispatch is
// given (packages/envoy/infra/gcp's dispatch_cloud.go), and the run's server is handed the file
// (run-server.sh's DISPATCH_E2E_AGENT_TOKEN_FILE), which e2e/api.ts, and so seed.ts, reads too.
const RIG = path.join(import.meta.dir, "rig.sh");
const fn = scriptFunctions(RIG);
const made: string[] = [];
afterEach(() => {
  for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
});
function freshDir(name: string): string {
  const dir = mkdtempSync(path.join(tmpdir(), `${name}-`));
  made.push(dir);
  return dir;
}
/** `script` run by bash with `args` as $1..., in this process's environment and `env`. */
function bash(script: string, args: string[], env: Record<string, string> = {}) {
  const ran = spawnChecked(["bash", "-c", script, "-", ...args], {
    env: { ...process.env, ...env },
  });
  return { code: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

/** The bearer services_up writes for a batch whose services are the operator's own, under `work`. */
function batchToken(work: string): string {
  const up = bash(
    [
      ...["note", "fail", "given_services", "safe_url", "services_up"].map(fn),
      'until_true() { shift 2; "$@"; }',
      "pg_sql() { printf '%s\\n' 100; }",
      "nats_ready() { :; }",
      "work=$1 S=$1/services tag=t",
      "services_postgres=$S/postgres-url tokens=$S/tokens",
      'mkdir -p "$S"',
      "services_up",
    ].join("\n"),
    [work],
    {
      SKILL_SCENARIOS_POSTGRES_URL: "postgres://rig@127.0.0.1:1/postgres",
      SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:1",
      SKILL_SCENARIOS_PARALLEL: "1",
    }
  );
  expect({ code: up.code, stderr: up.stderr }).toEqual({ code: 0, stderr: "" });
  const file = path.join(work, "services", "tokens", "dispatch-token");
  expect(statSync(file).mode & 0o777).toBe(0o600);
  return readFileSync(file, "utf8");
}

// Catches: a world whose bearer names the Dispatch harness (`e2e-token`), a word of the measurement
// or a name the rig removed from the world, one of no production shape, and one every batch shares.
test("the world's Dispatch token is a batch's own, production-shaped, and names none of the derived words", () => {
  const written = batchToken(freshDir("world-token"));
  expect(written.endsWith("\n")).toBe(true);
  const token = written.trimEnd();
  // The words derived from the rig's scenarios, cases and labels, and the names it removed from
  // the world (run-names.ts), read as the guard reads a name an agent sees.
  expect(nameWords(token, vocabulary(["head", "base"]))).toBeUndefined();
  // And the Dispatch harness's own words, which no derived word covers.
  expect(
    ["e2e", "token", "test", "fake", "fixture", "harness", "seed", "dev"].filter((word) =>
      holds(token, word)
    )
  ).toEqual([]);
  expect(token).toMatch(/^[0-9a-f]{48}$/);
  expect(batchToken(freshDir("world-token"))).not.toBe(written);
}, 60_000);

// Catches: a run's Dispatch server started on the harness's default bearer while the agents read
// the batch's, so every `dispatch` call of the run is refused 401; a seed.ts call of the run that
// sends the default (seed.ts reaches the server through e2e/api.ts, which reads the exported
// file); and a readiness check that names the bearer on its command line.
test("a run's Dispatch server takes the batch's token, and the run's seeds and readiness send it", () => {
  const work = freshDir("world-token-start");
  const co = path.join(work, "checkouts", "head");
  const builds = path.join(
    work,
    "builds",
    new Bun.CryptoHasher("sha256").update(co).digest("hex").slice(0, 16)
  );
  mkdirSync(builds, { recursive: true });
  writeFileSync(path.join(builds, "dispatch-e2e"), "#!/bin/sh\n", { mode: 0o755 });
  mkdirSync(path.join(work, "logs"));
  mkdirSync(path.join(work, "tokens"));
  const token = fixtureSecret("dsp");
  const file = path.join(work, "tokens", "dispatch-token");
  writeFileSync(file, `${token}\n`, { mode: 0o600 });
  const started = bash(
    [
      ...["fail", "build_dir", "start_dispatch", "dispatch_whoami"].map(fn),
      // A child, as run-server.sh is: it sees what start_dispatch exported, and nothing else.
      'start_process() { printenv DISPATCH_E2E_AGENT_TOKEN_FILE >"$work/server-given"; dispatch_pid=1; }',
      'await_start() { shift 4; "$@"; }',
      "report_port() { echo 4242; }",
      'curl() { printf "%s\\n" "$*" >"$work/curl-argv"; cat >"$work/curl-header"; }',
      "work=$1 evidence=$1 co=$2 root=$1 tokens=$1/tokens",
      'start_dispatch "postgres://rig@127.0.0.1:1/run_head?sslmode=disable"',
      'printenv DISPATCH_E2E_AGENT_TOKEN_FILE >"$work/exported"',
    ].join("\n"),
    [work, co]
  );
  expect(started).toMatchObject({ code: 0, stderr: "" });
  const read = (name: string) => readFileSync(path.join(work, name), "utf8");
  expect([read("server-given"), read("exported")]).toEqual([`${file}\n`, `${file}\n`]);
  expect(read("curl-header")).toBe(`Authorization: Bearer ${token}\n`);
  expect(read("curl-argv")).toBe("-fsS -o /dev/null -H @- http://127.0.0.1:4242/api/v1/whoami\n");
}, 60_000);
