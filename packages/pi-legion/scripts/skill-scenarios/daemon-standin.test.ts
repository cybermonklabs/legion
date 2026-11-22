import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import pkg from "../../package.json";

// The stand-in asks the daemon's own workflow for the phase its role works, which `go run`
// compiles and runs, so a host with no go runs none of these rows.
const skip = skipWithout("go");

const rig = temporaryDirectory("skill-scenarios-daemon-standin");
const standin = path.join(import.meta.dir, "daemon-standin.ts");
/** The boot token each stand-in takes from its file and the helper registers with: random, as every
 * secret this package's tests hand the clients is (src/fixture-secrets.test.ts). */
const BOOT_TOKEN = fixtureSecret("boot");
const started: Bun.Subprocess[] = [];
afterEach(() => {
  for (const child of started.splice(0)) child.kill();
});

/** One stand-in for `role` of issue LWEVAL-1, and the base URL it listens on; with a world file,
 * the world.json rig.sh hands it, it serves the state document and the issue's record. */
async function startStandin(role: string, worldFile?: string): Promise<string> {
  const dir = path.join(rig, `${role}-${started.length}`);
  const boot = `${dir}-boot`;
  writeFileSync(boot, `${BOOT_TOKEN}\n`);
  const world = worldFile === undefined ? [] : [worldFile];
  const child = Bun.spawn(
    ["bun", standin, "0", `${dir}-daemon.log`, boot, "skillsrig", "LWEVAL-1", role, ...world],
    { stdout: "pipe", stderr: "inherit" }
  );
  started.push(child);
  const reader = child.stdout.getReader();
  let seen = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) throw new Error(`the ${role} stand-in ended before it listened: ${seen}`);
    seen += new TextDecoder().decode(value);
    const listening = seen.match(/listening on (http:\/\/127\.0\.0\.1:\d+)/);
    if (listening?.[1] !== undefined) return listening[1];
  }
}

/** A completion of the stand-in's phase, under a grant it minted, naming a handoff record. */
async function complete(base: string, verdict: string) {
  const post = (route: string, body: unknown) =>
    fetch(`${base}${route}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
  const registered = await post("/legion/v1/claims/register", { bootToken: BOOT_TOKEN });
  expect(registered.status).toBe(200);
  const { secret } = (await registered.json()) as { secret: string };
  const grant = await post("/legion/v1/grants", {
    sessionId: "rig-session",
    secret,
    tree: "LWEVAL-1",
    issue: "LWEVAL-1",
  });
  expect(grant.status).toBe(200);
  const { grantId } = (await grant.json()) as { grantId: string };
  const done = await post("/legion/v1/handoff/complete", {
    grantId,
    summary: "the phase is done",
    verdict,
    handoffId: "rig-record",
    handoffVersion: 1,
  });
  return { status: done.status, body: (await done.json()) as Record<string, unknown> };
}

// Catches: a stand-in that takes a tester's completion with no verdict, which the daemon refuses
// (`TESTER_VERDICT_REQUIRED`, packages/daemon/internal/api/handoff.go), so a run whose tester
// forgot the verdict is accepted and scored as the role's failure, where a daemon would have
// refused it and the role could have completed again (INFRA-1303: c-head-2, f-head-3 and
// l-head-3 of the batch of record at 6a643564). The daemon's go run can take minutes on a cold
// build cache, as daemon-pane.test.ts says of the same program: on a new pod's cold cache this row
// ran past 300 s (2026-11-19, at 36155e1a), so each row takes daemon-pane.test.ts's 600 s bound.
test.skipIf(skip)(
  "a tester's completion that names no verdict is refused as the daemon refuses it",
  async () => {
    const base = await startStandin("tester");
    expect(await complete(base, "")).toEqual({
      status: 400,
      body: { code: "TESTER_VERDICT_REQUIRED", error: "tester verdict must be pass or fail" },
    });
    expect((await complete(base, "pass")).status).toBe(200);
  },
  600_000
);

// Catches: a stand-in that takes a verdict from a role that is not the tester, which the daemon
// refuses (`VERDICT_ROLE_FORBIDDEN`, the same file), so a reviewer's run that sends one is
// scored on a completion no daemon would have taken.
test.skipIf(skip)(
  "a completion that names a verdict from a role other than the tester is refused",
  async () => {
    const base = await startStandin("reviewer");
    expect(await complete(base, "pass")).toEqual({
      status: 400,
      body: { code: "VERDICT_ROLE_FORBIDDEN", error: "only a tester may report a verdict" },
    });
    expect((await complete(base, "")).status).toBe(200);
  },
  600_000
);

/** A GET of `route` on the stand-in at `base`: its status and its body. */
async function get(base: string, route: string): Promise<{ status: number; body: unknown }> {
  const answer = await fetch(`${base}${route}`);
  return { status: answer.status, body: await answer.json() };
}

/** `GET /healthz` on the stand-in at `base`: its status, the project its body names, and the
 * contract its header states, which the tool's `read_build` reads. */
async function health(base: string) {
  const answer = await fetch(`${base}/healthz`);
  const { project } = (await answer.json()) as { project?: unknown };
  return {
    status: answer.status,
    project,
    contract: answer.headers.get("Legion-Daemon-Contract"),
  };
}

// Catches: a stand-in whose record or health route answers other than its state document does,
// so the tool's `read_record` (the record route) or its `read_build` (the health route and its
// contract header) reads an issue, a project or a contract the run's `/state` does not name; and
// a stand-in with no world file, whose `/state` is refused, that still serves the issue's record.
test.skipIf(skip)(
  "the record and health routes answer what the state document names, and the record route refuses where the state does",
  async () => {
    const worldFile = path.join(rig, "world.json");
    writeFileSync(worldFile, JSON.stringify({ repo: "example/widgets", base: "main" }));
    const base = await startStandin("tester", worldFile);
    const state = await get(base, "/legion/v1/state");
    expect(state.status).toBe(200);
    const { daemon, issues } = state.body as {
      daemon: { project: string };
      issues: Record<string, unknown>;
    };
    expect(Object.keys(issues)).toEqual(["LWEVAL-1"]);
    expect(await get(base, "/legion/v1/issues/LWEVAL-1/record")).toEqual({
      status: 200,
      body: issues["LWEVAL-1"],
    });
    expect(await get(base, "/legion/v1/issues/LWEVAL-2/record")).toEqual({
      status: 404,
      body: { code: "RECORD_NOT_FOUND", error: "The daemon has no record for LWEVAL-2" },
    });
    const contract = String(pkg.legion.daemonApiVersion);
    expect(await health(base)).toEqual({ status: 200, project: daemon.project, contract });

    // With no world file `/state` is refused, the route not there, and the record route with it;
    // `/healthz` names only the project, so it serves still.
    const worldless = await startStandin("tester");
    for (const route of ["/legion/v1/state", "/legion/v1/issues/LWEVAL-1/record"]) {
      expect(await get(worldless, route)).toEqual({
        status: 404,
        body: { error: `no route for GET ${route}` },
      });
    }
    expect(await health(worldless)).toEqual({ status: 200, project: "skillsrig", contract });
  },
  600_000
);
