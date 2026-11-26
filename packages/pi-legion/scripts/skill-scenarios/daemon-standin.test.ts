import { expect, setDefaultTimeout, test } from "bun:test";
import { cpSync, existsSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";
import { type LegionRole, roleToken } from "@legion/contracts";
import { LegionActivityRequest, LegionStateResponse } from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import {
  claimRefusal,
  countDefault,
  labelDaemon,
  notCurrentPhase,
  postRefusal,
  READY_RETIRED,
  refusalIn,
  repositoriesLive,
  statesContract,
} from "./daemon-pane";
import { pinnedRead, RECORD_READ, rolesListed, shapeDiff, shapeOf } from "./production-shapes";
import { contractRefusal, issueRecord, sampleFacts } from "./standin-record";
import { stepId } from "./step-records";
import { bundled, spawnChecked, writeGhSelectors } from "./test-programs";
import { WORLD_REPO } from "./world-issue";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

/** The daemon stand-in and the gh stand-in the rows start, each a bundle built from its source
 * for this suite, so no start transpiles it again (test-programs.ts). */
const DAEMON_STANDIN = bundled(path.join(import.meta.dir, "daemon-standin.ts"));
const GH_STANDIN = bundled(path.join(import.meta.dir, "gh-standin.ts"));

/** This checkout, the one the head label is built from, whose daemon a stand-in reads out of the
 * run's built-from by default. */
const CHECKOUT = path.resolve(import.meta.dir, "../../../..");

// The stand-in asks the daemon's own functions for the phase its role works, which `go run`
// compiles, so a host with no go runs none of this.
const skip = skipWithout("go");

// The run directory, reserved at module scope: `temporaryDirectory` registers the hook that
// makes it, and a hook cannot be registered from inside a test.
const work = temporaryDirectory("skill-scenarios-standin");

/** The repository rig.sh's worker world is on (world-issue.ts's WORLD_REPO) and the branch its
 * pull request is open against (its `worker_repo` and `worker_base`, which fixture_write writes
 * into every world.json): the rows below start the stand-in on a world of those, so the record
 * they read is the one a run's agents read. That rig.sh hands the stand-in the run's directory
 * rather than either name, and so can name neither a second time, is rig.test.ts's row "the
 * stand-in is given the run's project, the run's own directory and the phase". */
const WORLD_BASE = "main";
/** The secret the stand-in's registration answers (daemon-standin.ts's SESSION_SECRET), which
 * `standin` names to it (SKILL_SCENARIOS_CLAIM_SECRET) and a claim's later requests carry: random,
 * as the daemon's is, since the plugin holds it (`@legion/contracts/held-secrets`). */
const SECRET = fixtureSecret("reg");
/** The boot token of every run directory below, and the bearer of the run's own Dispatch: random,
 * as rig.sh's are, since the plugin and the Dispatch client hold them. */
const BOOT_TOKEN = fixtureSecret("boot");
const DISPATCH_TOKEN = fixtureSecret("dsp");

/** The handoff word of each role's phase (phase.go's `Handoff`), which the record a role's
 * completion names is of. */
const HANDOFF_WORDS: Record<string, string> = {
  planner: "plan",
  implementer: "implement",
  tester: "test",
  reviewer: "review",
};

/** What a run's seeds leave on its own Dispatch, as far as the stand-in reads it back for the
 * record (daemon-standin.ts's `seededHistory` and `gateNow`) and writes it (`runOutbox`): the
 * run's issue as `GET /api/v1/issues/{key}` answers it — when it was filed, the session that
 * filed it, its spec — with the status it shows now, which a `PATCH` of the issue moves and
 * `statusWrites` keeps the body of, and `outages`, how many of the next `PATCH`es of it Dispatch
 * answers 503 before it takes one; and the spec's approval as `GET /api/v1/artifacts/{id}`
 * carries it, which a row moves as a person's review or a new version of the spec moves it. */
interface SeededWorld {
  readonly issue: { created_at: string; created_by: object | null; primary_artifact_id: string };
  status: string;
  readonly statusWrites: unknown[];
  approval: object | undefined;
  outages?: number;
}

/** A write the stand-in made of the run's Dispatch, as the Dispatch received it. */
interface DispatchWrite {
  readonly method: string;
  readonly route: string;
  readonly body: unknown;
}

/** The session the daemon writes every Dispatch write of the run's issue as
 * (dispatch/client.go's `daemonActor`). */
const DAEMON_ACTOR = { kind: "session", id: "legion-daemon:LWEVAL" };
/** A write of the issue's status or a message, as the daemon sends it. */
const IssueWrite = z.strictObject({
  status: z.string().optional(),
  body: z.string().optional(),
  actor: z.unknown(),
});

/** The writes of `writes` to the run's issue but its role holders and its step records, each
 * `[method, route, what]`, `what` a status write's status or a message's body, its outbox marker
 * held to the outbox's shape (`<!-- legion-outbox:<row> -->` after a blank line: record's
 * `MessagePost.Posted`) and cut; each is held to the daemon's own session. */
function issueWrites(writes: readonly DispatchWrite[]): [string, string, string | undefined][] {
  return writes
    .filter((write) => !write.route.endsWith("/role-holders") && !write.route.includes("/steps/"))
    .map(({ method, route, body }) => {
      const write = IssueWrite.parse(body);
      expect(write.actor).toEqual(DAEMON_ACTOR);
      if (write.body === undefined) return [method, route, write.status];
      const marked = /^([\s\S]*)\n\n<!-- legion-outbox:[1-9]\d* -->$/.exec(write.body);
      expect(marked?.[0]).toBe(write.body);
      return [method, route, marked?.[1]];
    });
}

/** Each set of role holders `writes` published, in the order it published them, each write held
 * to the route and the daemon's own session. */
function published(writes: readonly DispatchWrite[]): unknown[] {
  return writes
    .filter((write) => write.route.endsWith("/role-holders"))
    .map(({ method, route, body }) => {
      expect([method, route]).toEqual(["PUT", "/api/v1/issues/LWEVAL-1/role-holders"]);
      const write = z
        .strictObject({ holders: z.array(z.unknown()), actor: z.unknown() })
        .parse(body);
      expect(write.actor).toEqual(DAEMON_ACTOR);
      return write.holders;
    });
}

/** Each step record write of `writes`, in the order it made them, `[record id, body]`, each held
 * to `PUT /api/v1/issues/LWEVAL-1/steps/{id}` and the daemon's own session, the body without it. */
function stepWrites(writes: readonly DispatchWrite[]): [string, Record<string, unknown>][] {
  return writes
    .filter((write) => write.route.includes("/steps/"))
    .map(({ method, route, body }) => {
      const [, id = ""] = /^\/api\/v1\/issues\/LWEVAL-1\/steps\/([^/]+)$/.exec(route) ?? [];
      expect([method, id]).toEqual(["PUT", expect.stringMatching(/^[0-9a-f-]{36}$/)]);
      const { actor, ...write } = z.looseObject({ actor: z.unknown() }).parse(body);
      expect(actor).toEqual(DAEMON_ACTOR);
      return [id, write];
    });
}

/** The run's own Dispatch as the daemon stand-in reads it: `GET /api/v1/handoffs/{id}` answered
 * from `records`, as Dispatch answers it, and Dispatch's own 404 HANDOFF_NOT_FOUND for an id it
 * holds none of (dispatch/api's `getHandoff`), and, for a run with seeds, its issue, read and
 * written, its messages posted (`POST …/messages`, answered 201) and read back as its events do
 * (`GET …/events?order=desc`, each a `message.created` event carrying the body, paged by `limit`
 * and `before`), its role holders written (`PUT …/role-holders`, answered with the set, as
 * `putIssueRoleHolders` answers it) and its spec (`seeded`); a run with none holds no issue,
 * whose routes are each Dispatch's 404 ISSUE_NOT_FOUND. Each is behind the bearer in `token`: a
 * request with another bearer is Dispatch's 401, and any other route its 404. Every write it is
 * sent, taken or not, is kept in `writes`, in the order it came. */
function dispatchOf(
  records: Record<string, object>,
  token: string,
  seeded?: SeededWorld,
  writes: DispatchWrite[] = []
) {
  const messages: { seq: number; body: string; at: string }[] = [];
  return Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      if (request.headers.get("authorization") !== `Bearer ${token}`)
        return Response.json({ code: "UNAUTHORIZED", error: "no bearer" }, { status: 401 });
      const url = new URL(request.url);
      const route = url.pathname;
      const body: unknown = request.method === "GET" ? undefined : await request.json();
      if (request.method !== "GET") writes.push({ method: request.method, route, body });
      const sub =
        /^\/api\/v1\/issues\/LWEVAL-1(?:\/(messages|role-holders|events|steps\/[^/]+))?$/.exec(
          route
        );
      if (sub !== null && seeded === undefined)
        return Response.json(
          { code: "ISSUE_NOT_FOUND", error: "no issue LWEVAL-1" },
          { status: 404 }
        );
      if (sub?.[1] === "messages" && request.method === "POST") {
        const posted = z.object({ body: z.string(), actor: z.unknown() }).parse(body).body;
        messages.push({ seq: messages.length + 1, body: posted, at: new Date().toISOString() });
        return Response.json({ id: `message-${messages.length}` }, { status: 201 });
      }
      if (sub?.[1] === "events") {
        const before = Number(url.searchParams.get("before") ?? "0");
        const limit = Number(url.searchParams.get("limit") ?? "50");
        return Response.json(
          messages
            .filter((message) => before === 0 || message.seq < before)
            .reverse()
            .slice(0, limit)
            .map((message) => ({
              seq: message.seq,
              type: "message.created",
              created_at: message.at,
              payload: { id: `message-${message.seq}`, body: message.body },
            }))
        );
      }
      if (sub?.[1] === "role-holders" && request.method === "PUT")
        return Response.json({
          issue: "LWEVAL-1",
          holders: z.object({ holders: z.array(z.unknown()) }).parse(body).holders,
        });
      if (sub?.[1]?.startsWith("steps/") && request.method === "PUT")
        return Response.json({ issue: "LWEVAL-1", ...z.looseObject({}).parse(body) });
      if (seeded !== undefined && route === "/api/v1/issues/LWEVAL-1") {
        if (request.method === "PATCH" && (seeded.outages ?? 0) > 0) {
          seeded.outages = (seeded.outages ?? 0) - 1;
          return Response.json({ code: "UNAVAILABLE", error: "Dispatch is down" }, { status: 503 });
        }
        if (request.method === "PATCH") {
          const write = z.looseObject({ status: z.string() }).parse(body);
          seeded.statusWrites.push(write);
          seeded.status = write.status;
        }
        return Response.json({ key: "LWEVAL-1", status: seeded.status, ...seeded.issue });
      }
      if (seeded !== undefined && route === `/api/v1/artifacts/${seeded.issue.primary_artifact_id}`)
        return Response.json({
          id: seeded.issue.primary_artifact_id,
          ...(seeded.approval === undefined ? {} : { approval: seeded.approval }),
        });
      const id = /^\/api\/v1\/handoffs\/([^/]+)$/.exec(route)?.[1];
      const record = id === undefined ? undefined : records[decodeURIComponent(id)];
      return record === undefined
        ? Response.json(
            { code: "HANDOFF_NOT_FOUND", error: `handoff ${id} not found` },
            { status: 404 }
          )
        : Response.json(record);
    },
  });
}

/** The session each role of a seeded world wrote its record as, and the one that filed its
 * issue: ids of this file's own, in the shape of a session's, so a record that names them read
 * them from the run's Dispatch and from no constant of the rig's. */
const SEEDED_SESSIONS = {
  architect: "01a2c001-0000-7000-8000-00000000a001",
  plan: "01a2c002-0000-7000-8000-00000000b002",
  implement: "01a2c003-0000-7000-8000-00000000c003",
  test: "01a2c004-0000-7000-8000-00000000d004",
  review: "01a2c005-0000-7000-8000-00000000e005",
} as const;
type SeededWord = Exclude<keyof typeof SEEDED_SESSIONS, "architect">;
/** The spec of a seeded world's issue, and when the issue was filed. */
const SEEDED_SPEC = "5d0c7a52-9b1e-4c3f-8a67-2e4f6b8d0c13";
const SEEDED_AT = "2026-11-17T10:00:00.123456Z";
/** The record a seed wrote under `word`: version 2 of the implement handoff, so a record that
 * answered every version 1 would show, and version 1 of each other. */
function seededRecord(word: SeededWord) {
  return { id: `seed-${word}`, version: word === "implement" ? 2 : 1 };
}

/** The head of the world the stand-in is started on (world.json's `head`). */
const WORLD_HEAD = "0".repeat(40);
/** The stand-in, started on a port the kernel picks, with its own log and boot token file, on a
 * run directory holding `world`, which defaults to the world rig.sh builds, and in `phase` when
 * the caller names one. `completed` is the roles the run's world says completed their phase with
 * a commit (rig.sh's worker_boot COMPLETED); `checkout` is the checkout the run's built-from
 * names, whose daemon the stand-in answers as, its routes and how it takes a completion among
 * the rest, this one by default and none at all for "". Nothing of the run directory's name is
 * read: the daemon a run's label runs is its checkout's, never the label's. `records` is the
 * handoff records the run's own Dispatch holds (`dispatchOf`), by id: by default `h-review`,
 * the one `complete` names, version 1 of the issue's handoff of this role's phase. `seeds` is
 * the handoff words the run's seeds recorded before it (rig.sh's worker_run_world and its
 * runners): each is written into the run's fixture.json and held by its Dispatch under the
 * session of `SEEDED_SESSIONS`, beside the run's issue and its spec, approved at version 1
 * (`seeded`, which a row moves); none, by default, is a run with no seeds at all. `heads` is the
 * head each seeded implement and test record's body names (#5287's handoff shape), WORLD_HEAD
 * where it names none, as rig.sh's seeds name the implementer's commit, the world's head in every
 * world but those whose reviewer pushed a commit of its own above it. `dir` is the
 * run directory of an earlier stand-in of the same run, for the second agent of a run. `secret`
 * is the secret its registration answers (SKILL_SCENARIOS_CLAIM_SECRET), `SECRET` by default and
 * none for "", which leaves the stand-in to draw its own, as rig.sh's worker_boot does. It prints
 * `listening on http://127.0.0.1:<port>` once the socket is open, which is what this waits for. */
let started = 0;
async function standin(
  role: string,
  startIn = "",
  world = { repo: WORLD_REPO, base: WORLD_BASE },
  {
    completed = "",
    checkout = CHECKOUT,
    records = {
      "h-review": {
        id: "h-review",
        issue_key: "LWEVAL-1",
        phase: HANDOFF_WORDS[role] ?? "",
        version: 1,
        author: { kind: "session", id: "01a0f2cc" },
      },
    } as Record<string, object>,
    seeds = undefined as readonly SeededWord[] | undefined,
    heads = {} as Partial<Record<SeededWord, string>>,
    dir: given = undefined as string | undefined,
    secret = SECRET,
  } = {}
) {
  // One directory per start, so a second stand-in of the same role writes its own log: a test
  // that reads the log reads its own run's lines and no earlier run's.
  started += 1;
  const dir = given ?? path.join(work, `${role}-${started}`);
  mkdirSync(dir, { recursive: true });
  const token = path.join(dir, "boot-token");
  writeFileSync(token, `${BOOT_TOKEN}\n`);
  const bearer = path.join(dir, "dispatch-token");
  writeFileSync(bearer, `${DISPATCH_TOKEN}\n`);
  // The status the seeds leave the issue at is the one the daemon writes as the last seeded
  // phase ends (seed-handoffs.ts's PHASE_SEED): `testing` after the implementer's, and
  // `needs_review` after the tester's, which a review round keeps.
  const seeded: SeededWorld | undefined =
    seeds === undefined
      ? undefined
      : {
          issue: {
            created_at: SEEDED_AT,
            created_by: { kind: "session", id: SEEDED_SESSIONS.architect },
            primary_artifact_id: SEEDED_SPEC,
          },
          status: seeds.includes("test")
            ? "needs_review"
            : seeds.includes("implement")
              ? "testing"
              : "in_progress",
          statusWrites: [],
          approval: { state: "approved", latest_version: 1, version: 1 },
        };
  if (seeds !== undefined)
    writeFileSync(
      path.join(dir, "fixture.json"),
      JSON.stringify({
        issue: "LWEVAL-1",
        covers: "r2",
        ...Object.fromEntries(seeds.map((word) => [word, seededRecord(word)])),
      })
    );
  /** Every write the stand-in made of the run's Dispatch, in the order it made them. */
  const writes: DispatchWrite[] = [];
  const dispatch = dispatchOf(
    // A row's own record of a seeded id stands in the seed's place: one its agent's session
    // wrote, where the seed's is another session's.
    {
      ...Object.fromEntries(
        (seeds ?? []).map((word) => [
          seededRecord(word).id,
          {
            ...seededRecord(word),
            issue_key: "LWEVAL-1",
            phase: word,
            author: { kind: "session", id: SEEDED_SESSIONS[word] },
            ...(word === "implement" || word === "test"
              ? { body: JSON.stringify({ head: heads[word] ?? WORLD_HEAD }) }
              : {}),
          },
        ])
      ),
      ...records,
    },
    DISPATCH_TOKEN,
    seeded,
    writes
  );
  // The world the run keeps, in the shape fixture_write writes it: the stand-in reads the
  // repository its record names, and the branch it gives as the default, out of this file.
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({
      key: "LWEVAL-1",
      branch: "legion/LWEVAL-1",
      pr: 1001,
      head: WORLD_HEAD,
      ...world,
    })
  );
  // The label's built-from, as cmd_run copies it into the run: the checkout whose daemon's routes
  // the stand-in answers.
  if (checkout !== "") writeFileSync(path.join(dir, "built-from"), `checkout ${checkout}\n`);
  const log = path.join(dir, "daemon.log");
  const child = Bun.spawn(
    ["bun", DAEMON_STANDIN, "0", log, token, "skills1", "LWEVAL-1", dir, role, startIn, completed],
    {
      stdout: "pipe",
      stderr: "pipe",
      env: {
        ...process.env,
        DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
        DISPATCH_TOKEN_FILE: bearer,
        SKILL_SCENARIOS_CLAIM_SECRET: secret,
      },
    }
  );
  const reader = child.stdout.getReader();
  let printed = "";
  while (!printed.includes("listening on")) {
    const { value, done } = await reader.read();
    if (done) throw new Error(`the stand-in exited before it listened: ${printed}`);
    printed += new TextDecoder().decode(value);
  }
  const url = /listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(printed)?.[1];
  if (url === undefined) throw new Error(`no address in ${printed}`);
  /** The state body as the stand-in serves it, read through no schema: what a label's own
   * contract is handed, which is the thing a row below reads with two of them. It is read as the
   * label's plugin reads it, stating in `Legion-Plugin-Contract` the contract that plugin speaks,
   * its daemon's own, for a label whose daemon states one (`statesContract`), a checkout's plugin
   * and daemon declaring one number; a row that reads it otherwise asks for it itself. */
  const body = async (): Promise<unknown> => {
    const contract = statesContract(checkout);
    const headers: Record<string, string> =
      contract === undefined ? {} : { "Legion-Plugin-Contract": String(contract) };
    return await (await fetch(`${url}/legion/v1/state`, { headers })).json();
  };
  const record = async () => {
    const state = LegionStateResponse.parse(await body());
    return { issue: state.issues["LWEVAL-1"], repositories: state.repositories };
  };
  const phase = async () => (await record()).issue?.phase;
  /** The settings the state read serves, as the body carries them: read before this checkout's
   * schema, which keeps unknown keys, so a key an older contract requires is visible here. */
  const settings = async () =>
    z.looseObject({ settings: z.record(z.string(), z.unknown()) }).parse(await body()).settings;
  /** `POST /legion/v1/handoff/complete` on a grant minted for it, naming a handoff record when
   * `handoff` and naming none otherwise, which is the completion of a worker that has written
   * no record, with the verdict the role's `legion` tool sends where the agent names none of its
   * own: `pass` for the tester and none for every other role, since the daemon refuses a verdict
   * of any other (VERDICT_ROLE_FORBIDDEN); `extra` is any key a label's own contract sends beside
   * them. */
  const complete = async (handoff: boolean, extra: Record<string, unknown> = {}) => {
    const minted = await (
      await fetch(`${url}/legion/v1/grants`, {
        method: "POST",
        body: JSON.stringify({
          sessionId: "01a0f2cc",
          secret: SECRET,
          tree: "LWEVAL-1",
          issue: "LWEVAL-1",
        }),
      })
    ).json();
    const grant = z.object({ grantId: z.string() }).parse(minted);
    return await fetch(`${url}/legion/v1/handoff/complete`, {
      method: "POST",
      body: JSON.stringify({
        grantId: grant.grantId,
        summary: "the round is in",
        verdict: role === "tester" ? "pass" : "",
        ...(handoff ? { handoffId: "h-review", handoffVersion: 1 } : {}),
        ...extra,
      }),
    });
  };
  /** A POST of `payload` to the stand-in's `route`. */
  const post = (route: string, payload: unknown) =>
    fetch(`${url}${route}`, { method: "POST", body: JSON.stringify(payload) });
  /** Every line of this stand-in's log, parsed, in the order it wrote them. */
  const logged = () =>
    readFileSync(log, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  const stop = () => {
    child.kill();
    dispatch.stop(true);
  };
  /** Stops the run's Dispatch alone, so the stand-in's next read of it gets no answer. */
  const dispatchDown = () => dispatch.stop(true);
  return {
    dir,
    url,
    body,
    phase,
    record,
    complete,
    post,
    settings,
    logged,
    stop,
    seeded,
    writes,
    dispatchDown,
  };
}

/** Runs git in `dir` under an identity of its own and no configuration of the host's. */
function git(dir: string, ...args: string[]): string {
  const result = spawnChecked(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** The pull request of the run under `dir`, in the shapes rig.sh's fixture_write writes it: a
 * bare remote whose issue branch is at the head, no review on it, the create route every world
 * answers a review with, and the merge state GitHub reports while the pull request is at that
 * head, `state` (`CLEAN` unless the row names another), every other head being `CLEAN`
 * (gh-call.ts's `mergeStateOn`), with the gh selectors worker_boot writes beside them
 * (`writeGhSelectors`). `push` puts a commit above the tip and returns it;
 * `approve` and `requestChanges` submit a review of a head through the gh stand-in itself, so the
 * review the daemon stand-in reads is the one the world answered the call with: from a pane that
 * names no role and with a body that holds no Legion footer, or from `as`'s role with its body. */
function pullRequest(dir: string, state = "CLEAN") {
  const work = path.join(dir, "work");
  mkdirSync(work);
  git(work, "init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 1;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "the approved head");
  const head = git(work, "rev-parse", "HEAD");
  git(dir, "clone", "-q", "--bare", work, path.join(dir, "remote.git"));
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      mergeStates: { [head]: state },
      routes: [
        {
          match: "^gh api .*pulls/1001/reviews",
          methods: ["POST"],
          stdout: {
            id: 6001,
            html_url: "https://example.invalid/pull/1001#pullrequestreview-6001",
          },
        },
        { match: "^gh api .*pulls/1001/(reviews|comments)", stdout: [] },
      ],
    })
  );
  writeGhSelectors(dir);
  const review = (event: string, commit: string, as?: { role: string; body: string }) => {
    const body = path.join(dir, `review-${event}.json`);
    writeFileSync(
      body,
      JSON.stringify({ commit_id: commit, event, body: as?.body ?? "the round's review" })
    );
    const call = spawnChecked(
      [
        "bun",
        GH_STANDIN,
        "gh",
        "api",
        "--method",
        "POST",
        `repos/${WORLD_REPO}/pulls/1001/reviews`,
        "--input",
        body,
      ],
      {
        env: {
          ...process.env,
          GH_DATA_DIR: dir,
          ...(as === undefined ? {} : { LEGION_ROLE: as.role }),
        },
      }
    );
    if (call.exitCode !== 0) throw new Error(`the review was refused: ${call.stderr.toString()}`);
  };
  return {
    head,
    push: () => {
      writeFileSync(path.join(work, "docs.md"), `the text fix ${Math.random()}\n`);
      git(work, "add", "-A");
      git(work, "commit", "-qm", "docs: the text fix");
      git(work, "push", "-q", path.join(dir, "remote.git"), "legion/LWEVAL-1");
      return git(work, "rev-parse", "HEAD");
    },
    approve: (commit = head, as?: { role: string; body: string }) => review("APPROVE", commit, as),
    requestChanges: (commit = head, as?: { role: string; body: string }) =>
      review("REQUEST_CHANGES", commit, as),
  };
}

// Catches: a stand-in that takes any review of the review App for the round. Main's engine since
// #4328 (review.go's `byReviewer`, `reviewersClaim`) takes one as the reviewer's own only where
// its Legion footer names the session the reviewer's claim records, which the agent's
// registration writes, and a review that comes while the claim records none waits and is judged
// again at the next read (`RefusedWhileNoSession`). Before #4328 the login decided alone while
// the reviewer's row named no claim, which is the whole of a reviewer's first round until it
// completes, so a footer naming another session, or no footer, ended that round.
test.skipIf(skip)(
  "a review of the review App decides the round only where its footer names the session the reviewer's claim records",
  async () => {
    const footer = (session: string, head: string) =>
      `the round's review\n\n<!-- legion: {"session":"${session}","phase":"review","head":"${head}","entry":"1"} -->`;
    const waits = await standin("reviewer");
    try {
      const pull = pullRequest(waits.dir);
      pull.approve(pull.head, { role: "reviewer", body: footer(AGENT, pull.head) });
      expect(await waits.phase()).toBe("reviewing");
      await boots(waits.post, AGENT);
      expect(await waits.phase()).toBe("accepting");
    } finally {
      waits.stop();
    }
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir);
      await boots(daemon.post, AGENT);
      pull.approve(pull.head, { role: "reviewer", body: footer("another-session", pull.head) });
      expect(await daemon.phase()).toBe("reviewing");
      pull.approve(pull.head, { role: "reviewer", body: "the round's review, with no footer" });
      expect(await daemon.phase()).toBe("reviewing");
      pull.approve(pull.head, { role: "reviewer", body: footer(AGENT, pull.head) });
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

/** A Legion footer naming `session` and, where it is given, `head`, as a reviewer writes one. */
const footerOf = (session: string, head?: string) =>
  `the round's review\n\n<!-- legion: {"session":"${session}","phase":"review"${head === undefined ? "" : `,"head":"${head}"`},"entry":"1"} -->`;

// Catches: a stand-in that takes a review of the review App for the round on its `commit_id`.
// Main's engine since #2539 (review.go's `refusesRound`, intake's `LegionHead`) takes one only
// where the head its Legion footer names is the pull request's head, 40 lowercase hexadecimal
// characters compared exactly, and records its decision against that head (`reviewHead`): an
// approval or a request for changes whose footer names no head, a prefix, the head in capitals or
// another head decides nothing and leaves the record in `reviewing`, and one whose footer names
// the head decides whatever `commit_id` GitHub gives it. A person's review is held to no footer
// (the rows above).
test.skipIf(skip)(
  "a review of the review App decides the round only where its footer names the pull request's head",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir);
      await boots(daemon.post, AGENT);
      const as = (body: string) => ({ role: "reviewer", body });
      pull.requestChanges(pull.head, as(footerOf(AGENT, "0".repeat(40))));
      expect(await daemon.phase()).toBe("reviewing");
      for (const named of [
        undefined,
        pull.head.slice(0, 12),
        pull.head.toUpperCase(),
        "0".repeat(40),
        `${pull.head} `,
      ]) {
        pull.approve(pull.head, as(footerOf(AGENT, named)));
        expect([named, await daemon.phase()]).toEqual([named, "reviewing"]);
      }
      pull.approve("0".repeat(40), as(footerOf(AGENT, pull.head)));
      expect((await served(daemon.body)).issue).toMatchObject({
        phase: "accepting",
        pullRequest: { head: pull.head, reviewDecision: "approved" },
      });
    } finally {
      daemon.stop();
    }
    const rejected = await standin("reviewer");
    try {
      const pull = pullRequest(rejected.dir);
      await boots(rejected.post, AGENT);
      pull.requestChanges("0".repeat(40), {
        role: "reviewer",
        body: footerOf(AGENT, pull.head),
      });
      expect(await rejected.phase()).toBe("implementing");
    } finally {
      rejected.stop();
    }
  },
  600_000
);

// Catches: a stand-in that judges a review of the review App again at every read, so a push
// after the approval its footer's head decided undoes the approval. The engine judges a review
// once, at the head the record holds when it is delivered, and keeps the decision it recorded
// (`record.ReviewDecision`): the push sets the approval aside (`classify.ApprovalSetAside`), the
// record still reads it, and only an approval of the new head ends the round.
test.skipIf(skip)(
  "an approval its footer's head decided stays the round's decision after a push",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir, "BLOCKED");
      await boots(daemon.post, AGENT);
      pull.approve(pull.head, { role: "reviewer", body: footerOf(AGENT, pull.head) });
      expect((await served(daemon.body)).issue).toMatchObject({
        phase: "reviewing",
        pullRequest: { head: pull.head, reviewDecision: "approved" },
      });
      const fixed = pull.push();
      expect((await served(daemon.body)).issue).toMatchObject({
        phase: "reviewing",
        pullRequest: { head: fixed, reviewDecision: "approved" },
      });
      pull.approve(fixed, { role: "reviewer", body: footerOf(AGENT, fixed) });
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// k1's round (spec v31's R1): the reviewer pushes a text commit of its own while the issue is in
// `reviewing` and then approves the head that carries it, its footer naming that head. The engine
// takes a review App review whose footer names the pull request's head, whoever pushed the head
// (review.go's `refusesRound`, `reviewHead`), so that approval decides the round: it waits in
// `reviewing` while GitHub reports the head `BLOCKED`, as rig.sh's review_world reports it with
// the workflow's thread open, and ends it where the head is `CLEAN`. An approval whose footer
// names the head the reviewer read before its push names a head the pull request no longer has
// and decides nothing. Catches: a stand-in that holds a review's footer to the head the world
// began at rather than to the head the run's remote is at, which would leave k1's approval
// undecided, and one that takes an approval of the head below the reviewer's own commit.
test.skipIf(skip)(
  "the reviewer's approval of the head its own text commit made decides the round, and one of the head below it decides nothing",
  async () => {
    for (const state of ["CLEAN", "BLOCKED"]) {
      const daemon = await standin("reviewer");
      try {
        const pull = pullRequest(daemon.dir, state);
        await boots(daemon.post, AGENT);
        const own = pull.push();
        // The reviewer's own head reports what the world's base leaves it in, as the head below
        // it does (rig.sh's review_world, where the workflow's thread holds it `BLOCKED`).
        const file = path.join(daemon.dir, "fixtures.json");
        const fixtures = JSON.parse(readFileSync(file, "utf8"));
        fixtures.mergeStates[own] = state;
        writeFileSync(file, JSON.stringify(fixtures));
        pull.approve(pull.head, { role: "reviewer", body: footerOf(AGENT, pull.head) });
        expect((await served(daemon.body)).issue).toMatchObject({
          phase: "reviewing",
          pullRequest: { head: own },
        });
        pull.approve(own, { role: "reviewer", body: footerOf(AGENT, own) });
        expect((await served(daemon.body)).issue).toMatchObject({
          phase: state === "CLEAN" ? "accepting" : "reviewing",
          pullRequest: { head: own, reviewDecision: "approved" },
        });
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// The reason daemon-standin.ts's declaration of the footer's `entry` stays "not modelled": no
// scenario gives a reviewer a later round's task, the one task that names the commit the entry
// moves (`Last reviewed commit: <sha>`). Read off rig.sh itself: each runner's boots, in the order
// it makes them, boot no agent after its reviewer, so no run's record re-enters `reviewing` through
// an implementer's and a tester's rounds after the review; and the one reviewer task the rig writes
// names the first round (`review-round "$co" '' 1`: no commit, entry 1). Catches: a runner that
// boots an agent after its reviewer, or a second round's task, left beside a declaration that
// says no scenario reaches either.
test("no scenario gives a reviewer a later round's task, so the footer's entry moves nothing a run reads", () => {
  const script = readFileSync(path.join(import.meta.dir, "rig.sh"), "utf8");
  const booted = new Map<string, string[]>();
  let runner = "";
  for (const line of script.split("\n")) {
    const opened = /^(run_\w+)\(\) \{/.exec(line);
    if (opened?.[1] !== undefined) runner = opened[1];
    if (line === "}") runner = "";
    const boot = /^\s*worker_boot "[^"]*" "\$project" (\w+)/.exec(line);
    if (boot?.[1] !== undefined) booted.set(runner, [...(booted.get(runner) ?? []), boot[1]]);
  }
  expect(booted.has("")).toBe(false);
  const reviewers = [...booted].filter(([, roles]) => roles.includes("reviewer"));
  expect(reviewers.length).toBeGreaterThan(0);
  for (const [name, roles] of reviewers)
    expect([name, roles.indexOf("reviewer")]).toEqual([name, roles.length - 1]);
  expect(
    [...script.matchAll(/daemon-pane\.ts" review-round (.*)\)/g)].map(([, args]) => args)
  ).toEqual([`"$co" '' 1`]);
});

// Catches: a stand-in that moves the issue to `accepting` on the reviewer's completion. Main's
// engine ends the round on the review, not the completion (review.go's `reviewRound`,
// review_test.go's "after the review alone"), so a completed round with no review on the pull
// request is still `reviewing`. The stand-in that moved on any accepted completion
// (`daemon-standin.ts`, round 14) hid the case review 5887659447's finding 3 is about.
test.skipIf(skip)(
  "an accepted completion with no approval leaves the issue in reviewing",
  async () => {
    const daemon = await standin("reviewer");
    try {
      pullRequest(daemon.dir);
      expect(await daemon.phase()).toBe("reviewing");
      expect((await daemon.complete(true)).status).toBe(200);
      expect(await daemon.phase()).toBe("reviewing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that ends the round on the approval whatever GitHub reports for the head.
// The engine moves an approval only on a merge state the issue may advance on
// (`record.MergeState.Mergeable`), and waits in `reviewing` on every other: `BLOCKED`, which a
// check the base requires holds the head in while it runs — the normal case while CI runs at the
// approved head — and `BEHIND`, a head behind its base. `DIRTY`, a head that conflicts with its
// base, no world reports (daemon-standin.ts's `servedPhase`).
test.skipIf(skip)(
  "an approval of a head GitHub reports BLOCKED or BEHIND leaves the issue in reviewing",
  async () => {
    for (const state of ["BLOCKED", "BEHIND"]) {
      const daemon = await standin("reviewer");
      try {
        pullRequest(daemon.dir, state).approve();
        expect((await daemon.complete(true)).status).toBe(200);
        expect([state, await daemon.phase()]).toEqual([state, "reviewing"]);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in whose record never leaves `reviewing`, where a reviewer that reads it
// after an approval the engine would have taken reads a round that never ended; and one that
// takes a narrower set of states than `Mergeable` does. An approval of the head GitHub reports
// `CLEAN`, `UNSTABLE` or `HAS_HOOKS` ends the round with no completion at all.
test.skipIf(skip)(
  "an approval of a head GitHub reports CLEAN, UNSTABLE or HAS_HOOKS moves the issue to accepting",
  async () => {
    for (const state of ["CLEAN", "UNSTABLE", "HAS_HOOKS"]) {
      const daemon = await standin("reviewer");
      try {
        pullRequest(daemon.dir, state).approve();
        expect([state, await daemon.phase()]).toEqual([state, "accepting"]);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in that reads a change request as no decision, or as one of the head alone.
// `reviewRound` ends the round rejected on a change request whatever head it names and whatever
// the merge state, and the issue goes back to `implementing` (`TriggerReviewRejected`), which
// reviewer-text-findings' defect case reaches in `reviewing`; an approval after it moves nothing.
test.skipIf(skip)(
  "a change request sends the issue back to implementing, and an approval after it moves nothing",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir);
      pull.requestChanges("0".repeat(40));
      expect(await daemon.phase()).toBe("implementing");
      pull.approve();
      expect(await daemon.phase()).toBe("implementing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that reads an approval of any head as an approval of the pull request's.
// A push while the record is `reviewing` leaves the approval on a head that is no longer the
// pull request's, which sets it aside (`classify.ApprovalSetAside`, `stuckApprovedOtherCode`),
// and only an approval of the new head ends the round. Once the record is in `accepting` a push
// lands outside any round and moves nothing (review_test.go's "the approval of the head, then a
// push"). The approved head is `BLOCKED` and the fix head `CLEAN`, each by the world's own merge
// state for that head.
test.skipIf(skip)(
  "a push after the approval leaves the issue in reviewing until the new head is approved",
  async () => {
    const daemon = await standin("implementer", "reviewing");
    try {
      const pull = pullRequest(daemon.dir, "BLOCKED");
      pull.approve();
      expect(await daemon.phase()).toBe("reviewing");
      const fixed = pull.push();
      expect(await daemon.phase()).toBe("reviewing");
      pull.approve(fixed);
      expect(await daemon.phase()).toBe("accepting");
      pull.push();
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// GitHub holds a head whose base's ruleset requires review threads resolved `BLOCKED` while a
// review thread is open and reports it `CLEAN` once the last is resolved, and main's engine ends
// the round on an approval that stands only then. Catches: a daemon stand-in that moves on a merge
// state frozen into the world while the gh stand-in answers the threads as the run left them — the
// approval of threads-closed's head waited in `reviewing` for ever, however many
// threads the implementer resolved — and one that reads another state than the one the gh
// stand-in answers `mergeStateStatus` with (gh-threads.ts's `mergeStateOf`, which both read). The
// resolve is the recorded reviewer-text-findings-text-only-head-1/calls.jsonl:29, made through the
// gh stand-in.
test.skipIf(skip)(
  "an approval waits in reviewing while a thread its base requires resolved is open, and moves once the run resolves it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir);
      const file = path.join(daemon.dir, "fixtures.json");
      const fixtures = JSON.parse(readFileSync(file, "utf8"));
      fixtures.routes.push(
        {
          match: "^gh api .*/rules/branches/main",
          stdout: [
            { type: "pull_request", parameters: { required_review_thread_resolution: true } },
          ],
        },
        {
          match: "^gh api .*graphql",
          graph: {
            query: {
              repository: {
                pullRequest: {
                  reviewThreads: [
                    {
                      id: "PRRT_kwDOrig0001",
                      isResolved: false,
                      comments: [{ databaseId: 7001, state: "SUBMITTED", body: "a wording nit" }],
                    },
                  ],
                },
              },
            },
            mutation: { resolveReviewThread: {} },
          },
        }
      );
      writeFileSync(file, JSON.stringify(fixtures));
      pull.approve();
      expect(await daemon.phase()).toBe("reviewing");
      const resolve = spawnChecked(
        [
          "bun",
          GH_STANDIN,
          "gh",
          "api",
          "graphql",
          "-f",
          "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
          "-F",
          "id=PRRT_kwDOrig0001",
        ],
        { env: { ...process.env, GH_DATA_DIR: daemon.dir } }
      );
      expect(resolve.stdout.toString().trim()).toBe(
        JSON.stringify({ data: { resolveReviewThread: { thread: { isResolved: true } } } })
      );
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// GitHub holds a head whose base's ruleset requires an approval `BLOCKED` until one stands, as
// production's `main` does (INFRA-764's ruling of 2026-11-18; measured that day, every
// `REVIEW_REQUIRED` pull request of cybermonklabs/legion against `main` read `BLOCKED`), and the
// run's own approval is the one that lifts it. Catches: a daemon stand-in that reads the merge
// state without the run's reviews, so the approval the run made never stands for it and the
// round waits in `reviewing` for ever, while the gh stand-in, which reads them, answers `CLEAN`;
// and one that moves the record before any approval stands.
test.skipIf(skip)(
  "an approval the base requires lifts the merge state it holds BLOCKED, and the round ends on it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir);
      const file = path.join(daemon.dir, "fixtures.json");
      const fixtures = JSON.parse(readFileSync(file, "utf8"));
      fixtures.routes.push({
        match: "^gh api .*/rules/branches/main",
        stdout: [{ type: "pull_request", parameters: { required_approving_review_count: 1 } }],
      });
      writeFileSync(file, JSON.stringify(fixtures));
      const before = (await daemon.record()).issue;
      expect([before?.phase, before?.pullRequest?.mergeState]).toEqual(["reviewing", "blocked"]);
      pull.approve();
      const after = (await daemon.record()).issue;
      expect([after?.phase, after?.pullRequest?.mergeState]).toEqual(["accepting", "clean"]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that takes the request keys of this checkout's contract alone. An older
// label, main `ff2a10ef` (the base label of 2026-11-12), sends `ready` on every completion, and
// #2539 took it out of this checkout's strict request, so every base completion of 2026-11-12 was
// answered 400 `unrecognized_keys` and that arm measured a world of its own (review 5887659447,
// finding 1). A key no label sends is still refused, as the daemon refuses an undecodable body.
// The label is one whose daemon predates #3659, which reads `ready` again only to refuse `true`
// (the row of READY_RETIRED below holds this checkout's to that).
test.skipIf(skip)(
  "a completion carrying an older label's ready is answered as that label's daemon answered it",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { checkout: unstatedCheckout() });
    try {
      expect((await daemon.complete(true, { ready: true })).status).toBe(200);
      expect((await daemon.complete(true, { pushed: true })).status).toBe(400);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a log whose refusal lines carry no code, where a reader cannot tell the daemon's own
// refusal of a run's completion from a body the stand-in could not take at all (score.ts's
// `unscored`, which makes the second a rig error and keeps the first a reading of the run).
test.skipIf(skip)(
  "a refusal's log line carries its code, and a body the stand-in cannot take carries none",
  async () => {
    const daemon = await standin("reviewer");
    try {
      await daemon.complete(false);
      await daemon.complete(true, { pushed: true });
      const completions = daemon
        .logged()
        .filter((line) => line.path === "/legion/v1/handoff/complete");
      expect(completions).toMatchObject([
        { status: 400, code: "HANDOFF_RECORD_REQUIRED", role: "reviewer" },
        { status: 400, role: "reviewer" },
      ]);
      expect(completions[1]).not.toHaveProperty("code");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that leaves the role off a line. Health and the scorer key a completion by
// the role its line names and by nothing else, and fail closed on a completion line that names
// none (completion-answers.ts's `keyOf`), so every line of a run the stand-in served would read
// as a broken world. Every line but the log's opening one names the role this stand-in serves.
test.skipIf(skip)(
  "every line the stand-in logs names the role it serves",
  async () => {
    const daemon = await standin("implementer", "reviewing");
    try {
      await daemon.body();
      await daemon.complete(false);
      await daemon.complete(true);
      const served = daemon.logged().filter((line) => !("opened" in line));
      expect(served.length).toBeGreaterThanOrEqual(3);
      expect(served.filter((line) => line.role !== "implementer")).toEqual([]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that accepts a completion naming no record for a phase that ends with
// one. The daemon refuses it 400 HANDOFF_RECORD_REQUIRED and names the `dispatch handoff-write`
// that cures it (packages/daemon/internal/api/handoff.go), and the skill has the worker write
// the record and complete again: a 200 here teaches an agent that a phase ends without the
// record the next role reads.
test.skipIf(skip)(
  "a completion of a phase that ends with a record, naming none, is refused as the daemon refuses it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const refused = await daemon.complete(false);
      expect(refused.status).toBe(400);
      expect(await refused.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining("dispatch handoff-write --issue LWEVAL-1 --phase review"),
      });
      // Nothing moved: the round has not ended, so the issue stands in `reviewing`.
      expect(await daemon.phase()).toBe("reviewing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that moves every phase to `accepting`, which would tell a tester its own
// phase had ended. Only the review round ends in `accepting`; every other phase's completion
// ends the run, and no scenario reads the record after it.
test.skipIf(skip)(
  "a completion of another phase leaves the issue where it was",
  async () => {
    const daemon = await standin("tester");
    try {
      expect(await daemon.phase()).toBe("testing");
      expect((await daemon.complete(true)).status).toBe(200);
      expect(await daemon.phase()).toBe("testing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that serves the settings of this checkout's contract alone, which breaks
// every label whose contract requires a key this one dropped — silently, in that label alone
// (the measurement that found it is told once, at `standin-record.ts`'s `settings` line).
test.skipIf(skip)(
  "the state read serves every settings key a label's contract requires",
  async () => {
    const daemon = await standin("reviewer");
    try {
      // `max_fix_attempts` is main ff2a10ef's (contract 58, dropped by 59) and
      // `worker_boot_timeout_seconds` is this checkout's; both are positive integers in each.
      expect(await daemon.settings()).toMatchObject({
        max_fix_attempts: expect.any(Number),
        worker_boot_timeout_seconds: expect.any(Number),
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a state body only this checkout's contract describes. The row above reads two keys
// by name, which is a reader's memory of what the base label requires; this one hands the body
// to that label's own schema. A label's agents load the plugin of their own checkout, so a body
// the head's schema takes and the base's refuses breaks the base arm alone and silently — every
// state read of all 30 base runs of 2026-11-09 was refused `a body the daemon API contract does
// not describe`, and that arm measured a world of its own.
//
// The base label's schema is read from its checkout's own
// `packages/contracts/src/legion-api.ts`, under the directory SKILL_SCENARIOS_BASE_CHECKOUT
// names — the checkout `rig.sh profile base <checkout>` builds that label from. With none named
// there is no second contract on this host to read, and the row says what it wants and skips,
// as a row of this rig does for a program its host has not got.
const baseCheckout = process.env.SKILL_SCENARIOS_BASE_CHECKOUT;
if (baseCheckout === undefined)
  console.warn(
    `Skipped the base label's contract row in ${import.meta.path}. ` +
      "SKILL_SCENARIOS_BASE_CHECKOUT names no checkout to read its " +
      "packages/contracts/src/legion-api.ts from. Name the checkout `rig.sh profile base` was " +
      "built from and run the tests again."
  );
/** A schema as a reader of the served body uses it, which is all this needs of another
 * checkout's zod: the answer, and the sentence a refusal carries. */
type BodyReader = {
  safeParse: (body: unknown) => { success: boolean; error?: { message: string } };
};
test.skipIf(skip || baseCheckout === undefined)(
  "the state body is described by the base label's own contract as well as this checkout's",
  async () => {
    const contract = path.join(baseCheckout ?? "", "packages", "contracts", "src", "legion-api.ts");
    // No static import reaches it: the module is another checkout's, at a path this process is
    // handed at run time.
    const loaded: unknown = await import(contract);
    if (typeof loaded !== "object" || loaded === null || !("LegionStateResponse" in loaded))
      throw new Error(`${contract} exports no LegionStateResponse`);
    /** That checkout's own schema object, built by its own zod and read here for its answer
     * alone, which no type of this checkout's zod describes. */
    const base = loaded.LegionStateResponse as BodyReader;
    const daemon = await standin("reviewer");
    try {
      const served = await daemon.body();
      // This checkout's reader first, so a body neither contract describes is read as that
      // rather than as the base label's refusal.
      const here = LegionStateResponse.safeParse(served);
      expect(here.success ? "described" : here.error.message).toBe("described");
      const there = base.safeParse(served);
      expect(there.success ? "described" : (there.error?.message ?? "refused")).toBe("described");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that serves the role's own phase whatever the run is, so the loop after an
// approval, which runs in `accepting` where the daemon starts no phase, reads `implementing` to
// its implementer and `reviewing` to its reviewer. Each then reads a record saying it holds a
// phase it does not hold, and the step the rule gives for `accepting` is unreachable
// (review-threads.md step 1; rig.sh's run_threads_closed boots both halves there).
test.skipIf(skip)(
  "a boot names the phase the issue stands in, for either half of the loop after an approval",
  async () => {
    for (const role of ["implementer", "reviewer"]) {
      const daemon = await standin(role, "accepting");
      try {
        expect(await daemon.phase()).toBe("accepting");
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

/** The daemon's own refusal of `role`'s late completion while the issue stands in `phase`, as the
 * daemon of `checkout` (this one by default) writes it, read out of its source by the reader the
 * stand-in answers from (daemon-pane.ts's `notCurrentPhase`); daemon-pane.test.ts holds that
 * reading to what the daemon's own source writes, so no copy of the sentence lives in this rig. */
function lateRefusal(role: string, phase: string, checkout = CHECKOUT) {
  const refusal = notCurrentPhase(checkout, role, phase, "LWEVAL-1");
  expect(refusal).toBeDefined();
  return { code: refusal?.code, error: refusal?.message };
}

// Catches: a stand-in that takes a completion from threads-closed's implementer, whose
// implementing round the run's world says already stands (rig.sh's run_threads_closed passes
// COMPLETED), and one that refuses it with an answer the daemon does not give. Either engine
// refuses a late role whose row holds a completion 409 HANDOFF_NOT_CURRENT_PHASE (this checkout's
// `leftItsPhase`, `row.Completed()`; an older daemon's `RoleFor(issue.Phase) != fact.Role`, as
// `olderCheckout` holds it), with that daemon's own sentence for the phase the issue stands in,
// which the skill has the worker quote to its architect. The route's earlier HANDOFF_NO_RUN fence
// is not what a rig half meets: each half is a live registered worker serving generation 1.
test.skipIf(skip)(
  "threads-closed's implementer, whose round stands, is refused as the daemon refuses it",
  async () => {
    for (const [checkout, phase] of [
      [CHECKOUT, "accepting"],
      [CHECKOUT, "reviewing"],
      [olderCheckout(), "accepting"],
    ] as const) {
      const daemon = await standin("implementer", phase, undefined, {
        checkout,
        completed: "implementer",
      });
      try {
        const refused = await daemon.complete(false, { commit: "0".repeat(40) });
        expect([checkout, phase, refused.status]).toEqual([checkout, phase, 409]);
        expect(await refused.json()).toEqual(lateRefusal("implementer", phase, checkout));
        // Nothing moved: the issue stands where it stood, as the refusal says.
        expect(await daemon.phase()).toBe(phase);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in that reads "the implementer has completed" off the phase it was booted in
// rather than off what the run's world says. On the head label an implementer whose row holds no
// completion is one `leftItsPhase` takes, and its completion is then of the implementing
// phase it left, whose record it must name: engine.go refuses one naming none 409
// HANDOFF_RECORD_REQUIRED through `refused`.
test.skipIf(skip)(
  "on the head label, a late implementer the world gives no completed round must name its record",
  async () => {
    const daemon = await standin("implementer", "reviewing");
    try {
      const refused = await daemon.complete(false, { commit: "0".repeat(40) });
      expect(refused.status).toBe(409);
      expect(await refused.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining(
          "the implementer's completion of phase implementing of LWEVAL-1 names no handoff record"
        ),
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

/** The record `complete` names and a newer one of the same phase, as a reviewer that wrote its
 * handoff a second time holds them: each the issue's, written by the session the rows' grants
 * are minted for. */
const TWO_REVIEW_RECORDS = Object.fromEntries(
  [1, 2].map((version) => [
    version === 1 ? "h-review" : "h-review-2",
    {
      id: version === 1 ? "h-review" : "h-review-2",
      issue_key: "LWEVAL-1",
      phase: "review",
      version,
      author: { kind: "session", id: "01a0f2cc" },
    },
  ])
);
/** The daemon's answer to a call that repeats a completion the workflow was already handed
 * (handoff.go's HANDOFF_ALREADY_RECORDED), for the reviewer's completion naming `h-review` in
 * `phase` on the issue's pass `pass`. */
function alreadyRecorded(phase: string, pass: number) {
  return {
    code: "HANDOFF_ALREADY_RECORDED",
    error: `the reviewer completion of phase ${phase} (pass ${pass} through it) naming handoff record h-review (review version 1) was already received; this call changed nothing`,
  };
}

// Catches: a stand-in that decides "record required" from the phase the role was booted in
// rather than the phase the issue stands in now, which answered every completion after a
// reviewer's own REQUEST_CHANGES 400 HANDOFF_RECORD_REQUIRED (the batch at a61c46a7). After the
// round sends the issue to `implementing`, the route holds the reviewer to the record of
// `reviewing`, the phase it left (`phase.Of`), only when the completion names one. This
// checkout's engine then takes the reviewer's late completion (`leftItsPhase`), as one of
// `reviewing`, whose record it must name: naming none is refused 409 HANDOFF_RECORD_REQUIRED by
// the engine, never 400 by the route; naming one is taken and moves nothing. A taken completion
// completes the reviewer's row, so a second late completion, one naming a newer record, is
// refused 409 HANDOFF_NOT_CURRENT_PHASE; and the same call made again is no second completion
// at all but the fact the workflow was already handed, which the daemon answers 409
// HANDOFF_ALREADY_RECORDED before any workflow reads it (intake's `ApplyFact`). This row held
// the repeated call to HANDOFF_NOT_CURRENT_PHASE until the stand-in kept the facts it was
// handed (INFRA-764's ruling on the completions production refuses).
test.skipIf(skip)(
  "on the head label, a reviewer's completion after its change request is answered as leftItsPhase answers it",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { records: TWO_REVIEW_RECORDS });
    try {
      pullRequest(daemon.dir).requestChanges();
      expect(await daemon.phase()).toBe("implementing");
      const unnamed = await daemon.complete(false);
      expect(unnamed.status).toBe(409);
      expect(await unnamed.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining(
          "the reviewer's completion of phase reviewing of LWEVAL-1 names no handoff record: write it with `dispatch handoff-write --issue LWEVAL-1 --phase review --body-file <file>`"
        ),
      });
      expect((await daemon.complete(true, { commit: "0".repeat(40) })).status).toBe(200);
      expect(await daemon.phase()).toBe("implementing");
      const again = await daemon.complete(true, { commit: "0".repeat(40) });
      expect(again.status).toBe(409);
      expect(await again.json()).toEqual(alreadyRecorded("implementing", 1));
      const newer = await daemon.complete(true, { handoffId: "h-review-2", handoffVersion: 2 });
      expect(newer.status).toBe(409);
      expect(await newer.json()).toEqual(lateRefusal("reviewer", "implementing"));
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a head stand-in answering a late reviewer in `accepting` with `ff2a10ef`'s route,
// which held the completion to the record of the phase the issue stands in: `accepting` writes
// none, so a completion naming the review record was refused 409 HANDOFF_RECORD_MISMATCH. This
// checkout's route holds it to `reviewing`, the phase the reviewer left (`phase.Of`), and reads
// the record back (`recordBacked`'s `namesRecord`), so the engine takes it 200 and moves
// nothing. One naming none passes the route and the engine holds it to the record of
// `reviewing`: 409 HANDOFF_RECORD_REQUIRED, never MISMATCH. A taken one completes the row, so a
// later completion naming a newer record is refused 409 HANDOFF_NOT_CURRENT_PHASE, and the same
// call made again 409 HANDOFF_ALREADY_RECORDED, the fact the workflow was already handed (the
// row above says when this row held the repeat to the first of the two).
test.skipIf(skip)(
  "on the head label, a reviewer's completion after an approval that moved the issue is answered as the daemon answers it",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { records: TWO_REVIEW_RECORDS });
    try {
      pullRequest(daemon.dir).approve();
      expect(await daemon.phase()).toBe("accepting");
      const unnamed = await daemon.complete(false);
      expect(unnamed.status).toBe(409);
      expect(await unnamed.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining(
          "the reviewer's completion of phase reviewing of LWEVAL-1 names no handoff record"
        ),
      });
      const named = await daemon.complete(true, { commit: "0".repeat(40) });
      expect(named.status).toBe(200);
      expect(await daemon.phase()).toBe("accepting");
      const again = await daemon.complete(true, { commit: "0".repeat(40) });
      expect(again.status).toBe(409);
      expect(await again.json()).toEqual(alreadyRecorded("accepting", 0));
      const newer = await daemon.complete(true, { handoffId: "h-review-2", handoffVersion: 2 });
      expect(newer.status).toBe(409);
      expect(await newer.json()).toEqual(lateRefusal("reviewer", "accepting"));
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a head stand-in that takes a record named for a phase that writes none. This
// checkout's route still refuses it 409 HANDOFF_RECORD_MISMATCH: the merger's merging, the phase
// its completion is of, ends with no handoff record.
test.skipIf(skip)(
  "on the head label, a completion naming a record of a phase that writes none is refused record-mismatch",
  async () => {
    const daemon = await standin("merger");
    try {
      const named = await daemon.complete(true, { commit: "0".repeat(40) });
      expect(named.status).toBe(409);
      expect(await named.json()).toMatchObject({
        code: "HANDOFF_RECORD_MISMATCH",
        error: expect.stringContaining("phase merging of LWEVAL-1 ends with no handoff record"),
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that takes a record-naming completion and leaves the row open, as the
// engine before main's `row.Completed()` read a row holding a record and no commit; and a
// completion's log line that does not say which phase it was answered on, which is how a person
// reading the run tells the late completion from the reviewer's own. The plugin's completion
// here names a record and no commit, so the late one is a repeat the engine refuses.
test.skipIf(skip)(
  "a completion's log line names the phase it was answered on, and a taken record completes the row",
  async () => {
    const daemon = await standin("reviewer");
    try {
      expect((await daemon.complete(true)).status).toBe(200);
      pullRequest(daemon.dir).approve();
      expect((await daemon.complete(true)).status).toBe(409);
      expect(
        daemon
          .logged()
          .filter((line) => line.path === "/legion/v1/handoff/complete")
          .map((line) => [line.status, line.code, line.phase])
      ).toEqual([
        [200, undefined, "reviewing"],
        [409, "HANDOFF_NOT_CURRENT_PHASE", "accepting"],
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in of an older daemon answered as this checkout's daemon answers, which a
// table keyed by the label's name did once the base label moved to this checkout's daemon. An
// older engine (main `ff2a10ef`'s, as `olderCheckout` holds it) has no `leftItsPhase`: its
// `handoff` refuses every completion from a role that does not run the issue's phase 409
// HANDOFF_NOT_CURRENT_PHASE, whatever record it names, once its route has let it through. That
// route holds a completion to the record of the phase the issue stands in, so in `accepting` it
// refuses a named record 409 HANDOFF_RECORD_MISMATCH first. Both are read out of the checkout the
// run's built-from names (daemon-pane.ts's `labelDaemon`). The completion naming a record
// reports a commit beside it: that route identifies a late completion by its commit, the
// record being none of a phase it holds one to, so the two calls with no commit were one fact,
// and the second is that daemon's 409 HANDOFF_ALREADY_RECORDED, which this row's third call
// holds (it held both to HANDOFF_NOT_CURRENT_PHASE before the stand-in kept its facts).
test.skipIf(skip)(
  "on an older daemon, a reviewer's late completion is refused HANDOFF_NOT_CURRENT_PHASE",
  async () => {
    const checkout = olderCheckout();
    const rejected = await standin("reviewer", "", undefined, { checkout });
    try {
      pullRequest(rejected.dir).requestChanges();
      expect(await rejected.phase()).toBe("implementing");
      for (const handoff of [false, true]) {
        const refused = await rejected.complete(handoff, handoff ? { commit: "0".repeat(40) } : {});
        expect([handoff, refused.status]).toEqual([handoff, 409]);
        expect(await refused.json()).toEqual(lateRefusal("reviewer", "implementing", checkout));
      }
      const repeated = await rejected.complete(true);
      expect(repeated.status).toBe(409);
      expect(await repeated.json()).toEqual({
        code: "HANDOFF_ALREADY_RECORDED",
        error:
          "the reviewer completion of phase implementing (pass 1 through it) at commit  was already received; this call changed nothing",
      });
    } finally {
      rejected.stop();
    }
    const approved = await standin("reviewer", "", undefined, { checkout });
    try {
      pullRequest(approved.dir).approve();
      expect(await approved.phase()).toBe("accepting");
      const unnamed = await approved.complete(false);
      expect(unnamed.status).toBe(409);
      expect(await unnamed.json()).toEqual(lateRefusal("reviewer", "accepting", checkout));
      const named = await approved.complete(true);
      expect(named.status).toBe(409);
      expect(await named.json()).toMatchObject({ code: "HANDOFF_RECORD_MISMATCH" });
    } finally {
      approved.stop();
    }
  },
  600_000
);

// Catches: a stand-in with no `claims/activity` route, which answers the report the agents'
// plugin posts after each turn 404 where the daemon answers it 204 (`s.activity` in
// packages/daemon/internal/api/claims.go). The daemon authenticates the report by the claim's
// secret, as it does `claims/ready`, and refuses a wrong one with claim.InvalidSecret.
test.skipIf(skip)(
  "a registered claim's activity report is answered as the daemon answers it",
  async () => {
    const daemon = await standin("implementer");
    try {
      const registered = z.object({ claimToken: z.string(), secret: z.string() }).parse(
        await (
          await daemon.post("/legion/v1/claims/register", {
            bootToken: BOOT_TOKEN,
            sessionId: "01a0f2cc",
          })
        ).json()
      );
      const report = LegionActivityRequest.parse({
        claimToken: registered.claimToken,
        sessionId: "01a0f2cc",
        secret: registered.secret,
        activity: {
          at: "2026-11-13T18:00:00.000Z",
          backgroundWork: { running: false, kinds: [] },
          phase: { nextReminderAt: null },
        },
      });
      const answered = await daemon.post("/legion/v1/claims/activity", report);
      expect([answered.status, await answered.text()]).toEqual([204, ""]);
      const wrong = await daemon.post("/legion/v1/claims/activity", {
        ...report,
        secret: "not-the-secret", // fixture-secret: a wrong secret this row proves the stand-in refuses, which no client holds
      });
      expect([wrong.status, await wrong.json()]).toEqual([
        403,
        { error: "Invalid session secret" },
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in whose registration answers a secret spelled from words (`rig-secret`) or
// one fixed in its source, where the daemon draws one afresh, 26 letters and digits of base32 (Go's
// `rand.Text()`, packages/daemon/internal/api/claims.go). The agent's plugin holds the secret and
// scrubs every run of 10 of its characters from each line it prints (`@legion/contracts/
// held-secrets`), so a secret of words takes those words out of what a run reads. Two stand-ins
// booted as rig.sh boots them, naming no secret, each answer a secret of that shape, and not the
// same one.
test.skipIf(skip)(
  "the stand-in's registration answers a secret it drew at its boot, in the daemon's shape",
  async () => {
    const answered: string[] = [];
    for (let boot = 0; boot < 2; boot += 1) {
      const daemon = await standin("implementer", "", undefined, { secret: "" });
      try {
        const registered = await daemon.post("/legion/v1/claims/register", {
          bootToken: BOOT_TOKEN,
          sessionId: "01a0f2cc",
        });
        expect(registered.status).toBe(200);
        answered.push(z.object({ secret: z.string() }).parse(await registered.json()).secret);
      } finally {
        daemon.stop();
      }
    }
    for (const secret of answered) expect(secret).toMatch(/^[A-Z2-7]{26}$/);
    expect(answered[0]).not.toBe(answered[1]);
  },
  600_000
);

// Catches: a stand-in that answers a run whose daemon's completion shapes it cannot read as some
// other daemon would. How the run's daemon takes a completion is read out of the checkout its
// built-from names (daemon-pane.ts's `labelDaemon`), never out of the label's name, and a checkout
// whose route is in neither shape fails the boot, naming the file. The checkout holds a daemon's
// API source and engine alone, which is all the boot reads before it.
test.skipIf(skip)(
  "a run whose daemon takes a completion in neither shape fails the boot, naming the file",
  () => {
    const dir = path.join(work, "reviewer-neither-shape");
    const internal = path.join(work, "neither-shape", "packages", "daemon", "internal");
    mkdirSync(path.join(internal, "api"), { recursive: true });
    mkdirSync(path.join(internal, "workflow"), { recursive: true });
    writeFileSync(
      path.join(internal, "api", "server.go"),
      'package api\n\nfunc routes() {\n\tmux.HandleFunc("GET /legion/v1/state", s.state)\n}\n'
    );
    writeFileSync(
      path.join(internal, "api", "handoff.go"),
      "package api\n\nfunc (s *server) handoffComplete() {\n\trecordBacked := writer == grant.Role\n}\n"
    );
    writeFileSync(
      path.join(internal, "workflow", "engine.go"),
      'package workflow\n\nfunc (e *Engine) handoff() {\n\tif RoleFor(issue.Phase) != fact.Role {\n\t\treturn refused("HANDOFF_NOT_CURRENT_PHASE", "no")\n\t}\n}\n'
    );
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "boot-token"), `${BOOT_TOKEN}\n`);
    writeFileSync(path.join(dir, "built-from"), `checkout ${path.join(work, "neither-shape")}\n`);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        branch: "legion/LWEVAL-1",
        pr: 1001,
        head: "0".repeat(40),
        repo: WORLD_REPO,
        base: WORLD_BASE,
      })
    );
    const boot = spawnChecked([
      "bun",
      DAEMON_STANDIN,
      "0",
      path.join(dir, "daemon.log"),
      path.join(dir, "boot-token"),
      "skills1",
      "LWEVAL-1",
      dir,
      "reviewer",
    ]);
    expect(boot.exitCode).toBe(2);
    expect(boot.stderr.toString()).toContain(
      `daemon-standin: the daemon fact \`completion\` of ${path.join(work, "neither-shape")} could not be read`
    );
    expect(boot.stderr.toString()).toContain(
      `${path.join(internal, "api", "handoff.go")} holds neither`
    );
  },
  600_000
);

/** A checkout named `name` under the run directory that holds this checkout's daemon module, for a
 * fixture to change: the module copied; the envoy module linked beside it, which the module's
 * go.mod replaces with its sibling directory (`../envoy`); and this checkout's go.work and
 * go.work.sum copied to its root; and `packages/contracts/fixtures` linked beside them so
 * `statesContract` reads `released-contracts.json` and any entry fixtures (#5399). A checkout's
 * daemon module builds under its own go.work, as rig.sh's builds do (`go_binary`), which
 * reconciles the daemon's and envoy's requirements; with none above it, `go run` builds in module
 * mode, where the envoy module's requirements are read beside the daemon's go.mod alone, and stops
 * at `go: updates to go.mod needed` once envoy requires a newer version of a module than the
 * daemon's go.mod lists. It returns the checkout and its daemon module's directory. */
function daemonCheckout(name: string): { checkout: string; daemon: string } {
  const checkout = path.join(work, name);
  const daemon = path.join(checkout, "packages", "daemon");
  cpSync(path.join(CHECKOUT, "packages", "daemon"), daemon, { recursive: true });
  symlinkSync(path.join(CHECKOUT, "packages", "envoy"), path.join(checkout, "packages", "envoy"));
  mkdirSync(path.join(checkout, "packages", "contracts"), { recursive: true });
  symlinkSync(
    path.join(CHECKOUT, "packages", "contracts", "fixtures"),
    path.join(checkout, "packages", "contracts", "fixtures")
  );
  for (const file of ["go.work", "go.work.sum"])
    cpSync(path.join(CHECKOUT, file), path.join(checkout, file));
  return { checkout, daemon };
}

/** The sentence the older daemon (`olderCheckout`) refuses a late completion with: words of its
 * own, so a stand-in that read this checkout's daemon for that label answers with another. */
const OLDER_SENTENCE =
  "an older daemon says the %s may not finish phase %s of %s; this completion changed nothing";
let older: string | undefined;
/** A second checkout of this one's daemon module, standing in for an older daemon, main
 * `ff2a10ef`'s among them: a copy, so the daemon-pane.go reads that compile the daemon's own
 * functions run in it as they do in any label's checkout, and changed where an older daemon
 * differs: it registers no `claims/turn-outcome` and no `claims/activity`; its engine's `handoff`
 * refuses every late role with `if RoleFor(issue.Phase) != fact.Role { return
 * refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(OLDER_SENTENCE, …)) }` and asks no
 * `leftItsPhase`, nor main's `pastCompletion` since #4611 (the `refused` shape); and its
 * completion route holds a completion's record to the phase the issue stands in
 * (`phase.Handoff(issue.Phase)`, the `issue` shape). Made once for the file. */
function olderCheckout(): string {
  if (older !== undefined) return older;
  const { checkout, daemon } = daemonCheckout("older-checkout");
  const server = path.join(daemon, "internal", "api", "server.go");
  const registered = readFileSync(server, "utf8");
  const fewer = registered
    .split("\n")
    .filter(
      (line) =>
        !line.includes('"POST /legion/v1/claims/turn-outcome"') &&
        !line.includes('"POST /legion/v1/claims/activity"')
    )
    .join("\n");
  expect(registered.length - fewer.length).toBeGreaterThan(0);
  writeFileSync(server, fewer);
  const engine = path.join(daemon, "internal", "workflow", "engine.go");
  const taking = readFileSync(engine, "utf8");
  // The gate goes before this checkout's ask of a late role, which it drops: main's since #4611
  // declares the refusal it may answer late (`var late`) between the two and the ask.
  const refusing = taking.replace(
    /\treported := phase\.Of\(fact\.Role, issue\.Phase\)\n\tcurrent := reported == issue\.Phase\n(?:\tvar late \*intake\.Refusal\n)?\tif !current \{\n[\s\S]*?\n\t\}\n/,
    (block) =>
      `\tif RoleFor(issue.Phase) != fact.Role {\n\t\treturn refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("${OLDER_SENTENCE}", fact.Role, issue.Phase, issue.Key)), nil\n\t}\n${block.slice(0, block.indexOf("\tif !current {"))}`
  );
  expect(refusing).not.toBe(taking);
  writeFileSync(engine, refusing);
  const route = path.join(daemon, "internal", "api", "handoff.go");
  const left = readFileSync(route, "utf8");
  const ofIssue = left
    .replace(
      '\tof := phase.Of(grant.Role, issue.Phase)\n\tif of == "" {\n\t\tof = issue.Phase\n\t}\n',
      "\tof := issue.Phase\n"
    )
    .replace(
      '\tword, writer, handoffPhase := phase.Handoff(of)\n\tnamesRecord := req.HandoffID != "" || req.HandoffVersion != 0\n\trecordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)\n',
      '\tword, writer, handoffPhase := phase.Handoff(issue.Phase)\n\tnamesRecord := req.HandoffID != "" || req.HandoffVersion != 0\n\trecordBacked := handoffPhase && writer == grant.Role\n'
    );
  expect(ofIssue).not.toBe(left);
  writeFileSync(route, ofIssue);
  expect(labelDaemon(checkout)).toEqual({ lateRole: "refused", recordRoute: "issue" });
  older = checkout;
  return checkout;
}

/** A report of one turn under the claim this stand-in serves, for the role `role` (the plugin's
 * `agent_end` hook), with `extra` beside it or over it. */
function turn(role: LegionRole, sequence: number, outcome: string, extra: object = {}) {
  return {
    claimToken: roleToken("skills1", "LWEVAL-1", role),
    sessionId: "01a0f2cc",
    secret: SECRET,
    generation: 1,
    sequence,
    outcome,
    ...extra,
  };
}

// Catches: a head stand-in with no `claims/turn-outcome`, which answered every turn this
// checkout's plugin reports 404 (all twenty head runs of the kept batch logged one), and one that
// counts the turns otherwise than the daemon does. The daemon keeps one report per (generation,
// sequence), so a repeat replaces its report and is counted once (supervise's
// `recordTurnOutcome`), answers the run of refused turns the reports end with (`RefusedRun`), and
// fails the claim when that run reaches the launch failure limit, which the rig runs at its
// default: read here out of the daemon's config source, as the stand-in reads it.
test.skipIf(skip)(
  "on the head label, a turn outcome is answered with the run of refused turns the daemon counts",
  async () => {
    const limit = countDefault(CHECKOUT, "launch_failure_limit");
    expect(limit).toBeGreaterThan(2);
    const daemon = await standin("reviewer");
    try {
      const report = async (sequence: number, outcome: string) => {
        const answer = await daemon.post(
          "/legion/v1/claims/turn-outcome",
          turn("reviewer", sequence, outcome)
        );
        return [answer.status, await answer.json()];
      };
      expect(await report(1, "clean")).toEqual([200, { refusedRun: 0, failed: false }]);
      expect(await report(2, "refused")).toEqual([200, { refusedRun: 1, failed: false }]);
      expect(await report(3, "refused")).toEqual([200, { refusedRun: 2, failed: false }]);
      expect(await report(3, "refused")).toEqual([200, { refusedRun: 2, failed: false }]);
      for (let sequence = 4; sequence < limit + 1; sequence += 1)
        expect(await report(sequence, "refused")).toEqual([
          200,
          { refusedRun: sequence - 1, failed: false },
        ]);
      expect(await report(limit + 1, "refused")).toEqual([
        200,
        { refusedRun: limit, failed: true },
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a turn outcome taken that the daemon's route refuses: a sequence or a generation of 0,
// an outcome neither clean nor refused, and a caller named neither way or both ways are 400; a
// secret or a boot token that is not the claim's is 403; and a report under the launch's boot
// token for another generation than the launch's is 409 (`turnOutcome` in
// packages/daemon/internal/api/claims.go).
test.skipIf(skip)(
  "a turn outcome the daemon's route refuses is refused as it refuses it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const boot = { generation: 1, sequence: 1, outcome: "clean" };
      const answers: [string, number][] = [];
      for (const [name, body] of [
        ["no sequence", turn("reviewer", 0, "clean")],
        ["no generation", turn("reviewer", 1, "clean", { generation: 0 })],
        ["another outcome", turn("reviewer", 1, "maybe")],
        ["neither caller", boot],
        ["both callers", turn("reviewer", 1, "clean", { bootToken: BOOT_TOKEN })],
        ["another secret", turn("reviewer", 1, "clean", { secret: "not-it" })],
        ["another boot token", { ...boot, bootToken: "not-it" }],
        ["another generation", { ...boot, generation: 2, bootToken: BOOT_TOKEN }],
        ["the launch's boot token", { ...boot, bootToken: BOOT_TOKEN }],
      ] as const)
        answers.push([name, (await daemon.post("/legion/v1/claims/turn-outcome", body)).status]);
      expect(answers).toEqual([
        ["no sequence", 400],
        ["no generation", 400],
        ["another outcome", 400],
        ["neither caller", 400],
        ["both callers", 400],
        ["another secret", 403],
        ["another boot token", 403],
        ["another generation", 409],
        ["the launch's boot token", 200],
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that answers a route because it has a case for it rather than because the
// run's label's daemon registers it. An older daemon, main `ff2a10ef`, registers no
// `claims/turn-outcome`, so its runs' plugin never posts one and a daemon of its would answer one
// 404; a daemon registering no such route, read out of its own source, gets that 404 here, and
// the routes it does register are answered. Each 404 carries the body the daemon's catch-all
// writes (`{"error":"no route"}`, api/server.go's mux remainder), where the stand-in wrote a
// sentence of its own naming the route.
test.skipIf(skip)(
  "a route the label's daemon does not register is answered 404, whatever the stand-in has a case for",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { checkout: olderCheckout() });
    try {
      const answered = async (answer: Response) => [answer.status, await answer.json()];
      const noRoute = [404, { error: "no route" }];
      expect(
        await answered(
          await daemon.post("/legion/v1/claims/turn-outcome", turn("reviewer", 1, "clean"))
        )
      ).toEqual(noRoute);
      expect(
        await answered(await daemon.post("/legion/v1/claims/activity", { secret: SECRET }))
      ).toEqual(noRoute);
      expect(await answered(await fetch(`${daemon.url}/legion/v1/claims`))).toEqual(noRoute);
      expect(await daemon.phase()).toBe("reviewing");
      expect(
        daemon
          .logged()
          .filter((line) => line.status === 404)
          .map((line) => [line.method, line.path])
      ).toEqual([
        ["POST", "/legion/v1/claims/turn-outcome"],
        ["POST", "/legion/v1/claims/activity"],
        ["GET", "/legion/v1/claims"],
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

let unstated: string | undefined;
/** A copy of this checkout's daemon module standing in for a daemon before #3659 (dc1bbc995e), the
 * base label's at main 5570d0ac, where the stand-in answers by what #3659 changed: it registers no
 * `GET /legion/v1/issues/{key}/record`, its health route answers its status alone, written as
 * that daemon wrote it (daemon-pane.ts's `HEALTH_UNSTATED`, kept in a comment beside a call that
 * compiles, since the copy's module is compiled), and its completion request ends at the rating of
 * the plan, `ready` read ahead of it (`REQUEST_WITHOUT_READY`). Made once for the file. */
function unstatedCheckout(): string {
  if (unstated !== undefined) return unstated;
  const { checkout, daemon } = daemonCheckout("unstated-checkout");
  const edit = (file: string, from: string | RegExp, to: string) => {
    const at = path.join(daemon, "internal", "api", file);
    const text = readFileSync(at, "utf8");
    const edited = text.replace(from, to);
    expect(edited).not.toBe(text);
    writeFileSync(at, edited);
  };
  edit("server.go", '\t\t{"GET /legion/v1/issues/{key}/record", s.issueRecordRoute},\n', "");
  edit(
    "server.go",
    's.writeAnswer(w, r, AnswerHealth, http.StatusOK, Health{Status: "ok", Project: s.project})',
    's.writeAnswer(w, r, AnswerHealth, http.StatusOK, Health{Status: "ok"}) // writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})'
  );
  edit(
    "handoff.go",
    /(\tPlanRating {5}\*int {7}`json:"planRating,omitempty"`\n\tPlanReason {5}\*string {4}`json:"planReason,omitempty"`\n)([\s\S]*?\tReady \*bool `json:"ready,omitempty"`\n)\}/,
    "$2$1}"
  );
  expect(statesContract(checkout)).toBeUndefined();
  unstated = checkout;
  return checkout;
}

/** The header a daemon since #3659 states its contract in on every answer, and the one a caller
 * states its plugin's in. */
const DAEMON_CONTRACT = "Legion-Daemon-Contract";
const PLUGIN_CONTRACT = "Legion-Plugin-Contract";
/** The state body as far as the rows of #3659's answers read it: the contract mark, the claims'
 * lowest contract and the issue's record, through no contract. */
const MarkedState = z.looseObject({
  contract: z.unknown().optional(),
  lowestPluginContract: z.unknown().optional(),
  issues: z.record(z.string(), z.record(z.string(), z.unknown())),
});

// Catches: a stand-in of a label whose daemon states its contract (main's since #3659, this
// checkout's) that states none, states it on some answers alone (main's #3659 stand-in stated it
// on `GET /healthz` and nowhere else) or states this build's plugin's number rather than the
// label daemon's; a health route that names no project; a record route that answers another key
// than the run's issue, or refuses it in other words than the daemon's; a state served without
// the daemon's contract mark, the claims' lowest contract or a claim's update state; and a read
// placed below the daemon's contract, by its header or by the claims of its pod, answered with
// the daemon's own body, which that daemon would have shaped down, where this stand-in models no
// shaping.
test.skipIf(skip)(
  "on a label whose daemon states its contract, every answer states it, and the health, the record and the state are that daemon's",
  async () => {
    const contract = statesContract(CHECKOUT) ?? 0;
    expect(contract).toBeGreaterThan(188);
    const stated = String(contract);
    const daemon = await standin("reviewer");
    try {
      // The claim registers under the contract the label's plugin declares, its daemon's own.
      const register = (pluginContract: number) =>
        daemon.post("/legion/v1/claims/register", {
          bootToken: BOOT_TOKEN,
          sessionId: AGENT,
          pluginContract,
        });
      expect((await register(contract)).status).toBe(200);
      const answered = async (answer: Response) => [
        answer.status,
        answer.headers.get(DAEMON_CONTRACT),
        answer.status === 204 ? null : await answer.json(),
      ];
      expect(await answered(await fetch(`${daemon.url}/healthz`))).toEqual([
        200,
        stated,
        { status: "ok", project: "skills1" },
      ]);
      expect(await answered(await daemon.post("/legion/v1/claims/ready", { secret: "x" }))).toEqual(
        [403, stated, { error: expect.any(String) }]
      );
      expect(await answered(await fetch(`${daemon.url}/legion/v1/claims`))).toEqual([
        404,
        stated,
        { error: "no route" },
      ]);
      expect(
        await answered(await daemon.post("/legion/v1/claims/ready", { secret: SECRET }))
      ).toEqual([204, stated, null]);
      const read = (route: string, caller?: string) =>
        fetch(`${daemon.url}${route}`, {
          headers: caller === undefined ? {} : { [PLUGIN_CONTRACT]: caller },
        });
      const state = await read("/legion/v1/state", stated);
      expect([state.status, state.headers.get(DAEMON_CONTRACT)]).toEqual([200, stated]);
      const body = MarkedState.parse(await state.json());
      const issue = body.issues["LWEVAL-1"] ?? {};
      expect([body.contract, body.lowestPluginContract]).toEqual([
        { daemon: contract, caller: contract },
        contract,
      ]);
      expect(
        z
          .object({ workers: z.record(z.string(), z.object({ claim: z.looseObject({}) })) })
          .parse(issue).workers.reviewer?.claim
      ).toMatchObject({ updateState: "none" });
      // The record route answers the issue the state holds, whose quiet time is of its own read.
      const record = await read("/legion/v1/issues/LWEVAL-1/record", stated);
      expect([record.status, record.headers.get(DAEMON_CONTRACT)]).toEqual([200, stated]);
      expect(await record.json()).toEqual({ ...issue, quietHours: expect.any(Number) });
      expect(await answered(await read("/legion/v1/issues/LWEVAL-2/record", stated))).toEqual([
        404,
        stated,
        { code: "RECORD_NOT_FOUND", error: "The daemon has no record for LWEVAL-2" },
      ]);
      // A caller stating no contract, or no number, is placed by the claims of its pod, which
      // record the daemon's own; one stating a contract above the daemon's is served its own too.
      for (const caller of [undefined, String(contract + 1), "a lot"]) {
        const own = await read("/legion/v1/state", caller);
        expect([caller, own.status, MarkedState.parse(await own.json()).contract]).toEqual([
          caller,
          200,
          { daemon: contract, caller: contract },
        ]);
      }
      const unshaped = (below: number) => [
        500,
        stated,
        {
          error: `daemon-standin models no body shaped down to contract ${below}: the label's daemon speaks ${contract}, and this stand-in serves its own body alone`,
        },
      ];
      expect(await answered(await read("/legion/v1/state", String(contract - 1)))).toEqual(
        unshaped(contract - 1)
      );
      expect(
        await answered(await read("/legion/v1/issues/LWEVAL-1/record", String(contract - 1)))
      ).toEqual(unshaped(contract - 1));
      // Claims recording a lower contract place a caller that states none, or no number, there.
      expect((await register(146)).status).toBe(200);
      expect(await answered(await read("/legion/v1/state"))).toEqual(unshaped(146));
      expect(await answered(await read("/legion/v1/state", "a lot"))).toEqual(unshaped(146));
      const marked = await read("/legion/v1/state", stated);
      expect(MarkedState.parse(await marked.json()).lowestPluginContract).toBe(146);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in of a label whose daemon predates #3659, the base label's at main 5570d0ac,
// answered as #3659's daemon: a contract stated on its answers, a project on its health route, a
// record route it never registered, a state marked with a contract and a claim's update state, or
// a read shaped by the contract its caller states, which that daemon never read.
test.skipIf(skip)(
  "on a label whose daemon predates #3659, no answer states a contract, and the health, the record and the state are that daemon's",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { checkout: unstatedCheckout() });
    try {
      await boots(daemon.post);
      const answered = async (answer: Response) => [
        answer.status,
        answer.headers.get(DAEMON_CONTRACT),
        answer.status === 204 ? null : await answer.json(),
      ];
      expect(await answered(await fetch(`${daemon.url}/healthz`))).toEqual([
        200,
        null,
        { status: "ok" },
      ]);
      expect(await answered(await daemon.post("/legion/v1/claims/ready", { secret: "x" }))).toEqual(
        [403, null, { error: expect.any(String) }]
      );
      expect(
        await answered(await daemon.post("/legion/v1/claims/activity", { secret: SECRET }))
      ).toEqual([204, null, null]);
      expect(await answered(await fetch(`${daemon.url}/legion/v1/issues/LWEVAL-1/record`))).toEqual(
        [404, null, { error: "no route" }]
      );
      const state = await fetch(`${daemon.url}/legion/v1/state`, {
        headers: { [PLUGIN_CONTRACT]: "1" },
      });
      expect([state.status, state.headers.get(DAEMON_CONTRACT)]).toEqual([200, null]);
      const body = MarkedState.parse(await state.json());
      const reviewer = z
        .object({ workers: z.record(z.string(), z.object({ claim: z.looseObject({}) })) })
        .parse(body.issues["LWEVAL-1"]).workers.reviewer?.claim;
      expect([
        "contract" in body,
        "lowestPluginContract" in body,
        "updateState" in (reviewer ?? {}),
      ]).toEqual([false, false, false]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a completion sending `ready: true` taken by a label whose daemon refuses it (main's
// since #3659, which reads `ready` again only to refuse `true`: no daemon from contract 86 on
// merges), or refused by one whose daemon never refused it; and `ready: false` refused.
test.skipIf(skip)(
  "a completion's ready: true is refused READY_RETIRED where the label's daemon retires it, and taken where it does not",
  async () => {
    const retiring = await standin("reviewer");
    try {
      const refused = await retiring.complete(true, { ready: true });
      expect([refused.status, refused.headers.get(DAEMON_CONTRACT), await refused.json()]).toEqual([
        400,
        String(statesContract(CHECKOUT)),
        { code: READY_RETIRED.code, error: READY_RETIRED.message },
      ]);
      expect("handoffRecord" in ((await served(retiring.body)).issue.workers.reviewer ?? {})).toBe(
        false
      );
      expect((await retiring.complete(true, { ready: false })).status).toBe(200);
    } finally {
      retiring.stop();
    }
    const taking = await standin("reviewer", "", undefined, { checkout: unstatedCheckout() });
    try {
      expect((await taking.complete(true, { ready: true })).status).toBe(200);
    } finally {
      taking.stop();
    }
  },
  600_000
);

// Catches: a stand-in that goes on taking a claim's reports after the daemon failed it. The
// daemon fails the claim when its run of refused turns reaches the launch failure limit, and
// every write of a failed claim clears the capability its secret authenticated (supervise's
// `NormalizeClaim`), so each route that authenticates the secret refuses it `InvalidSecret`
// from then on: the plugin's next turn outcome, its activity report and its grant. The sentence
// is the daemon's own claim refusal, read out of its claim source. The failing report's log line
// says it failed the claim, which health reads.
test.skipIf(skip)(
  "once a turn outcome fails the claim, its secret authenticates nothing",
  async () => {
    const limit = countDefault(CHECKOUT, "launch_failure_limit");
    const invalid = claimRefusal(CHECKOUT, "InvalidSecret");
    const daemon = await standin("reviewer");
    try {
      for (let sequence = 1; sequence <= limit; sequence += 1)
        await daemon.post("/legion/v1/claims/turn-outcome", turn("reviewer", sequence, "refused"));
      const answers = [];
      for (const [route, body] of [
        ["/legion/v1/claims/turn-outcome", turn("reviewer", limit + 1, "clean")],
        ["/legion/v1/claims/activity", { secret: SECRET }],
        [
          "/legion/v1/grants",
          { sessionId: "01a0f2cc", secret: SECRET, tree: "LWEVAL-1", issue: "LWEVAL-1" },
        ],
      ] as const) {
        const answer = await daemon.post(route, body);
        answers.push([route, answer.status, await answer.json()]);
      }
      expect(answers).toEqual(
        ["/legion/v1/claims/turn-outcome", "/legion/v1/claims/activity", "/legion/v1/grants"].map(
          (route) => [route, invalid.status, { error: invalid.message }]
        )
      );
      expect(
        daemon
          .logged()
          .filter((line) => line.path === "/legion/v1/claims/turn-outcome")
          .map((line) => [line.status, line.failed])
      ).toEqual([
        ...Array.from({ length: limit - 1 }, () => [200, false]),
        [200, true],
        [invalid.status, undefined],
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in of an older daemon that answers a late completion with this checkout's
// daemon's sentence rather than its own. The sentence, the phase table the boot reads and how the
// daemon takes a completion come out of the checkout the run's built-from names, here a daemon
// that words the refusal in `OLDER_SENTENCE`.
test.skipIf(skip)(
  "a late completion is refused in the sentence of the label's own daemon",
  async () => {
    const checkout = olderCheckout();
    const daemon = await standin("reviewer", "", undefined, { checkout });
    try {
      pullRequest(daemon.dir).requestChanges();
      expect(await daemon.phase()).toBe("implementing");
      const refused = await daemon.complete(true);
      expect(refused.status).toBe(409);
      const filled = ["reviewer", "implementing", "LWEVAL-1"].reduce(
        (text, value) => text.replace(/%s/, value),
        OLDER_SENTENCE
      );
      expect(await refused.json()).toEqual({ code: "HANDOFF_NOT_CURRENT_PHASE", error: filled });
      expect(lateRefusal("reviewer", "implementing", checkout).error).toBe(filled);
      expect(lateRefusal("reviewer", "implementing").error).not.toBe(filled);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches the same against an older daemon's own source, main `ff2a10ef`'s (2026-11-07), where a
// checkout of it is on the host (SKILL_SCENARIOS_OLDER_CHECKOUT names it; the base label was once
// built from it, and is built from main at the head's newest forward merge now, so the base
// label's checkout is no checkout of it): its engine writes the refusal as
// `refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(<format>, fact.Role, issue.Phase,
// issue.Key))`, its own source reads as the older shapes, and the stand-in answers in that
// format filled.
const olderHost = process.env.SKILL_SCENARIOS_OLDER_CHECKOUT;
if (olderHost === undefined)
  console.warn(
    `Skipped the older daemon's own-sentence and own-moves rows in ${import.meta.path}. ` +
      "SKILL_SCENARIOS_OLDER_CHECKOUT names no checkout of main ff2a10ef to read its " +
      "packages/daemon from. Name one and run the tests again."
  );
test.skipIf(skip || olderHost === undefined)(
  "an older daemon's late completion is refused in that daemon's own sentence",
  async () => {
    const checkout = olderHost ?? "";
    const engine = readFileSync(
      path.join(checkout, "packages", "daemon", "internal", "workflow", "engine.go"),
      "utf8"
    );
    const [, format = ""] =
      /refused\("HANDOFF_NOT_CURRENT_PHASE", fmt\.Sprintf\("((?:[^"\\]|\\.)*)", fact\.Role, issue\.Phase, issue\.Key\)/.exec(
        engine
      ) ?? [];
    expect(format).not.toBe("");
    expect(labelDaemon(checkout)).toEqual({ lateRole: "refused", recordRoute: "issue" });
    const daemon = await standin("reviewer", "", undefined, { checkout });
    try {
      pullRequest(daemon.dir).requestChanges();
      const refused = await daemon.complete(true);
      expect(refused.status).toBe(409);
      expect(await refused.json()).toEqual({
        code: "HANDOFF_NOT_CURRENT_PHASE",
        error: ["reviewer", "implementing", "LWEVAL-1"].reduce(
          (text, value) => text.replace(/%s/, value),
          format
        ),
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Against main `ff2a10ef`'s own daemon, where the host has a checkout of it
// (SKILL_SCENARIOS_OLDER_CHECKOUT): daemon-pane.go's `post-check` builds in that checkout's
// module, as rig.sh's build of each label is made, and reads a post with that checkout's own
// Dispatch reader, which refuses a block past its bound and knows no decision block; and that
// daemon, whose workflow refuses no reason, posts no move's sentence and publishes no role holder
// (each came to main later, #4309 and #4228), takes a move this checkout's daemon refuses and
// writes nothing of it on the run's Dispatch. Catches: a stand-in that answers an older label
// with this checkout's refusals, sentences or publications, which that label's daemon never
// gave, and a post-check that no longer builds against an older daemon module.
test.skipIf(skip || olderHost === undefined)(
  "an older daemon refuses no backward move's reason, posts no sentence and publishes no role holder",
  async () => {
    const checkout = olderHost ?? "";
    expect(postRefusal(checkout, "y".repeat(32_001))).toEqual({
      decision: false,
      sentence:
        "body block 1 (text) is 1 characters over the 32000-character block limit (32001/32000)",
    });
    const decision = 'The plan is wrong.\n\n:::ask{urgency="med"}\nWhich table?\n:::';
    expect(postRefusal(checkout, decision)).toBeUndefined();
    expect(postRefusal(CHECKOUT, decision)?.decision).toBe(true);
    const daemon = await standin("reviewer", "", undefined, { checkout });
    try {
      expect(await askedBack(daemon.post, "implementing", decision)).toEqual([200, {}]);
      expect(await daemon.phase()).toBe("implementing");
      expect(daemon.writes).toEqual([]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in with no `claims/release-records`, which both labels' daemons register and
// the tool's `read_release_records` posts, and one answering it otherwise than `releaseRecords`:
// the body's contract, then the claim's secret, then every release, resume and hold since the
// time named, of which the rig's world has none, as three empty lists.
test.skipIf(skip)(
  "the release records are answered as the daemon answers them in a world that released no pod",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const records = (body: object) =>
        daemon.post("/legion/v1/claims/release-records", {
          claimToken: roleToken("skills1", "LWEVAL-1", "reviewer"),
          secret: SECRET,
          since: "2026-11-01T00:00:00Z",
          ...body,
        });
      const read = await records({});
      expect([read.status, await read.json()]).toEqual([
        200,
        { releases: [], resumes: [], holds: [] },
      ]);
      expect((await records({ since: "yesterday" })).status).toBe(400);
      expect((await records({ secret: "not-it" })).status).toBe(403);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that answers a run whose daemon it cannot read the routes of, as some other
// daemon would answer it. The routes are read out of the checkout the run's built-from names, and
// a run with none fails the boot.
test.skipIf(skip)(
  "a run whose built-from names no checkout fails the boot",
  () => {
    const dir = path.join(work, "reviewer-head-900");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "boot-token"), `${BOOT_TOKEN}\n`);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        branch: "legion/LWEVAL-1",
        pr: 1001,
        head: "0".repeat(40),
        repo: WORLD_REPO,
        base: WORLD_BASE,
      })
    );
    const boot = spawnChecked([
      "bun",
      DAEMON_STANDIN,
      "0",
      path.join(dir, "daemon.log"),
      path.join(dir, "boot-token"),
      "skills1",
      "LWEVAL-1",
      dir,
      "reviewer",
    ]);
    expect(boot.exitCode).toBe(2);
    expect(boot.stderr.toString()).toContain("built-from names no checkout");
  },
  600_000
);

// Catches: a stand-in whose record names a repository or a default branch the run's world has
// not got. Agents read the record for the repository their `gh` calls name and for the branch
// their work goes above, so a second name is a second world, which cost recorded runs their
// review (rig.sh's worker world comment holds that story). Both are the world's own, read
// out of the run's world.json: the branch here is not `main`, so a stand-in that wrote the
// rig's usual default rather than reading the world fails this row. The state's repositories
// block names the same repository, as this checkout's daemon writes it since #4493: the one
// repository the live records name, with both role Apps installed on it, through this
// checkout's own contract, which keeps no member of the mapping before it.
test.skipIf(skip)(
  "the record names the repository and the default branch of the run's own world",
  async () => {
    const daemon = await standin("reviewer", "", { repo: "widgets-org/widgets", base: "trunk" });
    try {
      const { issue, repositories } = await daemon.record();
      expect(issue?.repo).toBe("widgets-org/widgets");
      expect(issue?.defaultBranch).toBe("trunk");
      expect(repositories).toEqual({
        live: [{ repo: "widgets-org/widgets", apps: { implement: true, review: true } }],
        withoutWebhookApp: [],
        caches: [],
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// And the world rig.sh builds is served as it stands, so no row above reads a world of this
// file's own making.
test.skipIf(skip)(
  "the record names rig.sh's own worker world",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const { issue } = await daemon.record();
      expect(issue?.repo).toBe(WORLD_REPO);
      expect(issue?.defaultBranch).toBe(WORLD_BASE);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

/** The `live` list of the state's repositories block as main's daemon writes it since #4493
 * (daemon-pane.ts's `REPOSITORIES_LIVE`), and in its place the lists of the project's mapping that
 * every daemon before it writes (`REPOSITORIES_MAPPED`), kept in comments beside a field that
 * compiles, since the copy's module is compiled, and that writes no `live` key. */
const LIVE_FIELD = '\tLive              []LiveRepositoryView `json:"live"`\n';
const MAPPED_FIELDS =
  '\tLive              []LiveRepositoryView `json:"-"`\n\t// Mapped            []MappedRepositoryView  `json:"mapped"`\n\t// Refused           []RefusedRepositoryView `json:"refused"`\n';
/** A copy of this checkout's daemon module whose state's repositories block is the project's
 * mapping, as main's daemon before #4493 writes it (`MAPPED_FIELDS`), and is this checkout's in all
 * else. Made once for the file. */
let mapping: string | undefined;
function mappingCheckout(): string {
  if (mapping !== undefined) return mapping;
  const { checkout, daemon } = daemonCheckout("mapping-checkout");
  const state = path.join(daemon, "internal", "api", "state.go");
  const live = readFileSync(state, "utf8");
  const mapped = live.replace(LIVE_FIELD, MAPPED_FIELDS);
  expect(mapped).not.toBe(live);
  writeFileSync(state, mapped);
  expect(repositoriesLive(checkout)).toBe(false);
  mapping = checkout;
  return checkout;
}

// Catches: a stand-in that serves a label of main's daemon before #4493 the live records' block,
// which that daemon never wrote, or the block this checkout's contract leaves of the mapping once
// it drops the keys it does not declare, `owners`, `mapped` and `refused`; a mapping naming
// another repository or owner than the run's world's; and a block whose shape is read off this
// build rather than off the label's own source, which this row's label shares in all but that.
test.skipIf(skip)(
  "on a label whose daemon serves the project's mapping, the repositories block is that mapping of the run's own world",
  async () => {
    expect(repositoriesLive(CHECKOUT)).toBe(true);
    const world = { repo: "widgets-org/widgets", base: "trunk" };
    const daemon = await standin("reviewer", "", world, { checkout: mappingCheckout() });
    try {
      const state = z.looseObject({ repositories: z.unknown() }).parse(await daemon.body());
      expect(state.repositories).toEqual({
        owners: ["widgets-org"],
        mapped: [{ repo: "widgets-org/widgets", default: true }],
        refused: [],
        withoutWebhookApp: [],
        caches: [],
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a log nothing writes until the agent's first call, where a run whose agent never
// reached the daemon leaves no daemon.log and scores as the rig error of a record nothing
// wrote, rather than as the run that moved nothing (score.ts's `unscored`). The line is the
// stand-in's own, written when it starts listening, as record-publishes.ts writes its
// `recording` line, and it is the record's first line, before any answer of the run's.
test.skipIf(skip)(
  "the log opens with the stand-in's own line, written before any request is answered",
  async () => {
    const daemon = await standin("reviewer");
    try {
      expect(daemon.logged()).toMatchObject([{ opened: "daemon.log" }]);
      expect(await daemon.phase()).toBe("reviewing");
      const after = daemon.logged();
      expect(after[0]).toMatchObject({ opened: "daemon.log" });
      expect(after[1]).toMatchObject({ path: "/legion/v1/state", status: 200 });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// What daemon-pane.go's `refusal not-current-phase` prints is two lines, the code and then the
// daemon's message, and the stand-in answers a completion of a phase its role does not run with
// exactly that pair or refuses to boot (`refusalAtBoot`). Catches: a printout half read taken
// for a refusal — no message, an empty code, or a message that runs onto a third line, each a
// sentence the daemon never sends — which no row that starts the stand-in on today's daemon can
// reach, since that daemon prints the two lines. No go is needed: this reads the printout alone.
test("a refusal printout is the code and the message on two lines of their own, or no refusal", () => {
  const message = "issue LWEVAL-1 is in accepting, which implementer does not run";
  expect(refusalIn(`HANDOFF_NOT_CURRENT_PHASE\n${message}\n`)).toEqual({
    code: "HANDOFF_NOT_CURRENT_PHASE",
    message,
  });
  for (const printed of [
    "",
    "HANDOFF_NOT_CURRENT_PHASE\n",
    "HANDOFF_NOT_CURRENT_PHASE\n\n",
    `\n${message}\n`,
    `HANDOFF_NOT_CURRENT_PHASE\n${message}`,
    `HANDOFF_NOT_CURRENT_PHASE\n${message}\nand a third line\n`,
  ])
    expect([printed, refusalIn(printed)]).toEqual([printed, undefined]);
});

// The daemon reads the handoff record a completion names back from Dispatch and holds it to the
// issue, the phase's handoff word and the version the completion reports before it records
// anything (packages/daemon/internal/api/handoff.go's `handoffRecord`). Catches: a stand-in that
// takes every named record as found — the success it gave a completion naming a record Dispatch
// has not got, or one of another issue, phase or version — and one whose refusal changed what a
// later completion is answered: each refused completion records nothing, so the one naming the
// round's own record is still taken.
test.skipIf(skip)(
  "a completion naming a record Dispatch has not got, or another issue's, phase's or version's, is refused as the daemon refuses it",
  async () => {
    const record = (issue: string, phase: string, version: number) => ({
      issue_key: issue,
      phase,
      version,
      author: { kind: "session", id: "01a0f2cc" },
    });
    const daemon = await standin("reviewer", "", undefined, {
      records: {
        "h-review": record("LWEVAL-1", "review", 1),
        "h-other-issue": record("LWEVAL-2", "review", 1),
        "h-plan": record("LWEVAL-1", "plan", 1),
      },
    });
    try {
      const read = "`dispatch handoff-read --issue LWEVAL-1 --phase review`";
      const gone = await daemon.complete(true, { handoffId: "h-gone" });
      expect([gone.status, await gone.json()]).toEqual([
        404,
        {
          code: "HANDOFF_RECORD_NOT_FOUND",
          error: `Dispatch has no handoff record h-gone (HANDOFF_NOT_FOUND: handoff h-gone not found): name the record ${read} shows, or write one with \`dispatch handoff-write --issue LWEVAL-1 --phase review --body-file <file>\`, then complete again; this completion changed nothing`,
        },
      ]);
      for (const [id, version, held] of [
        ["h-other-issue", 1, "version 1 of LWEVAL-2's review handoff"],
        ["h-plan", 1, "version 1 of LWEVAL-1's plan handoff"],
        ["h-review", 2, "version 1 of LWEVAL-1's review handoff"],
      ] as const) {
        const named = await daemon.complete(true, { handoffId: id, handoffVersion: version });
        expect([id, named.status, await named.json()]).toEqual([
          id,
          409,
          {
            code: "HANDOFF_RECORD_MISMATCH",
            error: `handoff record ${id} is ${held}, and this completion is of LWEVAL-1's review handoff at version ${version}: name the record ${read} shows; this completion changed nothing`,
          },
        ]);
      }
      expect((await daemon.complete(true)).status).toBe(200);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// The workflow takes a phase worker's move back only to a phase before the one the issue stands
// in, and only from the role running that phase, and refuses every other move 409
// BACKWARD_REFUSED in its own sentence (packages/daemon/internal/workflow/engine.go's
// `backward` and `backwardRefusal`); a move it takes moves the issue there. Catches: a stand-in
// that answers `{}` for any `to` — a move forward, to the phase the issue stands in, to
// `admitted`, or by a role not running the phase — and so tells the agent the issue moved when
// the daemon moved nothing; and one that answers a move it takes `{}` and moves nothing, so
// every later read of the record still shows the phase the move left.
test.skipIf(skip)(
  "a backward move the workflow does not serve is refused BACKWARD_REFUSED, and one it serves moves the issue",
  async () => {
    /** A move of the issue back to `to` through the stand-in that `post` reaches, on a grant
     * minted for it, as the tool's `request_backward_move` makes one. */
    const move = async (
      post: (route: string, payload: unknown) => Promise<Response>,
      to: string
    ) => {
      const minted = await (
        await post("/legion/v1/grants", {
          sessionId: "01a0f2cc",
          secret: SECRET,
          tree: "LWEVAL-1",
          issue: "LWEVAL-1",
        })
      ).json();
      const { grantId } = z.object({ grantId: z.string() }).parse(minted);
      const moved = await post("/legion/v1/phase/backward", {
        grantId,
        to,
        reason: "the fix needs code",
      });
      return [moved.status, await moved.json()];
    };
    const refused = (why: string) => [409, { code: "BACKWARD_REFUSED", error: why }];
    const implementer = await standin("implementer", "reviewing");
    try {
      expect(await move(implementer.post, "implementing")).toEqual(
        refused(
          "a backward move is the role running the issue's phase: reviewing of LWEVAL-1 is the reviewer's, so the implementer's move to implementing changed nothing"
        )
      );
      expect(await implementer.phase()).toBe("reviewing");
    } finally {
      implementer.stop();
    }
    const reviewer = await standin("reviewer");
    try {
      for (const to of ["accepting", "reviewing", "admitted"])
        expect(await move(reviewer.post, to)).toEqual(
          refused(
            `a backward move reaches an earlier phase, and no backward row of reviewing serves ${to}; the reviewer's move of LWEVAL-1 changed nothing`
          )
        );
      expect(await reviewer.phase()).toBe("reviewing");
      expect(await move(reviewer.post, "implementing")).toEqual([200, {}]);
      expect(await reviewer.phase()).toBe("implementing");
    } finally {
      reviewer.stop();
    }
  },
  600_000
);

/** A move of the issue back to `to` for `reason` through the stand-in `post` reaches, on a grant
 * minted for it, as the tool's `request_backward_move` makes one: the answer's status and body. */
async function askedBack(
  post: (route: string, payload: unknown) => Promise<Response>,
  to: string,
  reason: string
): Promise<[number, unknown]> {
  const minted = await (
    await post("/legion/v1/grants", {
      sessionId: AGENT,
      secret: SECRET,
      tree: "LWEVAL-1",
      issue: "LWEVAL-1",
    })
  ).json();
  const { grantId } = z.object({ grantId: z.string() }).parse(minted);
  const moved = await post("/legion/v1/phase/backward", { grantId, to, reason });
  return [moved.status, await moved.json()];
}

// A backward move posts its sentence on the issue (effects.go's `backwardMoveReason`), so the
// engine refuses a sentence Dispatch would refuse there before anything moves
// (`backwardReasonRefusal`): the longest post the outbox could make of it, read by Dispatch's own
// reader, holding a decision block is 409 BACKWARD_REASON_DECISION_BLOCK and one past Dispatch's
// bounds 409 BACKWARD_REASON_TOO_LONG, each in the engine's sentence around Dispatch's. The
// reasons are the daemon's own test's (backward_reason_test.go): a reason of as many blocks as
// Dispatch holds a body to is refused for the block the marker adds. A move the engine takes posts
// its sentence, the role that asked and its words, trimmed and closed with a full stop where they
// end with none, before its status, or alone where the phase it enters shows the status the issue
// already shows; a status the run's Dispatch has not taken is listed with that sentence. Catches:
// a stand-in that takes every reason (it did: 200 `{}` and the issue moved where production
// refused the move and moved nothing) or names one refusal for the other; one that posts no
// sentence, posts it after the status or under another session; an owed write that drops it; and
// one that posts the sentence again at each attempt of a status write Dispatch did not take.
test.skipIf(skip)(
  "a backward move whose sentence Dispatch would refuse is refused in the engine's words, and one it takes posts its sentence before its status",
  async () => {
    const decision =
      ':::ask{urgency="med"}\nWhich table should the plan name?\n\n- users: the old one\n- accounts: the new one\n:::';
    const blocks = Array.from({ length: 32_768 }, () => "y").join("\n\n");
    const asked = (from: string, to: string, by: string, words: string) =>
      `Legion moved this issue from ${from} back to ${to} at the ${by}'s request: ${words}`;
    const reviewer = await standin("reviewer", "", undefined, {
      seeds: ["plan", "implement", "test"],
    });
    try {
      expect(
        await askedBack(reviewer.post, "implementing", `The plan is wrong.\n\n${decision}`)
      ).toEqual([
        409,
        {
          code: "BACKWARD_REASON_DECISION_BLOCK",
          error:
            "the reason holds a decision block, and the move posts the reason on the issue as a message, where nobody can answer one: body block 2 (ask) holds a decision block, which nobody can answer where it stands: ask the question in the issue's own document, as a decision block there, or open an ask with `dispatch ask`; to show a decision block as an example, put it in a fenced code block; the move was not applied, so ask again with a reason that states what is wrong rather than asking a question",
        },
      ]);
      expect(await askedBack(reviewer.post, "implementing", blocks)).toEqual([
        409,
        {
          code: "BACKWARD_REASON_TOO_LONG",
          error:
            "the reason makes a message Dispatch refuses: body is 1 blocks over the 32768-block limit (32769/32768); the move was not applied, so ask again with a shorter reason",
        },
      ]);
      expect(await reviewer.phase()).toBe("reviewing");
      expect(issueWrites(reviewer.writes)).toEqual([]);
      expect(await askedBack(reviewer.post, "implementing", "  the guard needs code  ")).toEqual([
        200,
        {},
      ]);
      expect(await reviewer.phase()).toBe("implementing");
      expect([reviewer.seeded?.status, issueWrites(reviewer.writes)]).toEqual([
        "in_progress",
        [
          [
            "POST",
            "/api/v1/issues/LWEVAL-1/messages",
            asked("reviewing", "implementing", "reviewer", "the guard needs code."),
          ],
          ["PATCH", "/api/v1/issues/LWEVAL-1", "in_progress"],
        ],
      ]);
    } finally {
      reviewer.stop();
    }
    // Implementing and planning show one status, so the move's sentence is posted alone.
    const implementer = await standin("implementer", "implementing", undefined, {
      seeds: ["plan"],
    });
    try {
      const words = "the plan names the wrong table?";
      expect(await askedBack(implementer.post, "planning", words)).toEqual([200, {}]);
      expect(issueWrites(implementer.writes)).toEqual([
        [
          "POST",
          "/api/v1/issues/LWEVAL-1/messages",
          asked("implementing", "planning", "implementer", words),
        ],
      ]);
    } finally {
      implementer.stop();
    }
    // A status the run's Dispatch has not taken is owed, and the state lists it with the sentence
    // its write posts first (`record.StatusWrite`'s `reason`); each attempt after the one that
    // posted the sentence finds it on the issue by its marker and posts it no second time
    // (outbox.go's `message`, `MessageBodiesSince`).
    const owed = await standin("reviewer", "", undefined, { seeds: ["plan", "implement", "test"] });
    try {
      if (owed.seeded !== undefined) owed.seeded.outages = 2;
      expect(await askedBack(owed.post, "implementing", "the fix needs code")).toEqual([200, {}]);
      const { pendingStatusWrites } = z
        .looseObject({ pendingStatusWrites: z.array(z.unknown()) })
        .parse(await owed.body());
      expect(pendingStatusWrites).toEqual([
        {
          issue: "LWEVAL-1",
          payload: {
            kind: "ordinary",
            status: "in_progress",
            observedStatus: "needs_review",
            reason: asked("reviewing", "implementing", "reviewer", "the fix needs code."),
          },
          attempts: 2,
          nextAt: expect.any(String),
          lastError:
            'Dispatch answered PATCH /api/v1/issues/LWEVAL-1 503: {"code":"UNAVAILABLE","error":"Dispatch is down"}',
        },
      ]);
      expect(await owed.phase()).toBe("implementing");
      expect([owed.seeded?.status, issueWrites(owed.writes)]).toEqual([
        "in_progress",
        [
          [
            "POST",
            "/api/v1/issues/LWEVAL-1/messages",
            asked("reviewing", "implementing", "reviewer", "the fix needs code."),
          ],
          ["PATCH", "/api/v1/issues/LWEVAL-1", "in_progress"],
          ["PATCH", "/api/v1/issues/LWEVAL-1", "in_progress"],
          ["PATCH", "/api/v1/issues/LWEVAL-1", "in_progress"],
        ],
      ]);
    } finally {
      owed.stop();
    }
  },
  600_000
);

/** Production's `read_record` as the pin holds it (production-shapes.json's `legion
 * read_record`, read in a Legion issue pod on 2026-11-17), and the keys it holds at its top. */
const PINNED_RECORD = pinnedRead(RECORD_READ);
const PINNED_KEYS = Object.keys(PINNED_RECORD.shape).flatMap(
  (at) => /^\.(\w+)$/.exec(at)?.[1] ?? []
);
/** The members production's record holds that no world of the rig does, each for a reason of
 * the rig's and none of shape (standin-record.ts's header tells each once):
 *   - the architect's `capabilities` and the three times of its turns: no architect's session
 *     runs in a rig run, so none reported its capabilities and the stand-in's daemon recorded no
 *     ready and no turn of it, which is the claim the contract describes as one "whose last turn
 *     ran before the daemon recorded them";
 *   - `step`, the architect's and every worker's: the daemon builds it from the tool frames of
 *     the launcher's worker stream, and a rig agent runs under no launcher. */
const WORLD_NEVER_HOLDS = [
  ".architect.capabilities",
  ".architect.lastTurnStart",
  ".architect.lastTurnEnd",
  ".architect.lastReady",
  ".architect.step",
  ".workers[].claim.step",
];
/** The members the stand-in serves that production's record held none of when the pin was read,
 * each a member main's daemon wrote first after it: each claim's `updateState`, which the record
 * of a daemon since #3659 (2026-11-20) carries on every claim and the pin, read on 2026-11-17,
 * predates (daemon-pane.ts's `statesContract`, standin-record.ts's `daemonContract`). */
const SINCE_PIN = [".architect.updateState", ".workers[].claim.updateState"];
/** The session a row's agent registers under, and the report of its capabilities a session
 * sends on a `claims/ready` of its own (`claim.CapabilityReport`). */
const AGENT = "01a2d000-0000-7000-8000-00000000f006";
const REPORT = {
  measuredAt: "2026-11-17T10:05:00.000Z",
  elapsedMs: 12,
  rows: [
    { name: "github", ok: true, detail: "" },
    { name: "mcp", ok: true, detail: "2 servers" },
  ],
};
/** The claim boot of the plugin against the stand-in `post` reaches: its registration under
 * `session`, declaring the contract its package names, then its ready, with `capabilities`
 * where the row gives a report. */
async function boots(
  post: (route: string, payload: unknown) => Promise<Response>,
  session = AGENT,
  capabilities?: object
) {
  const registered = await post("/legion/v1/claims/register", {
    bootToken: BOOT_TOKEN,
    sessionId: session,
    pluginContract: 146,
  });
  const readied = await post("/legion/v1/claims/ready", {
    secret: SECRET,
    ...(capabilities === undefined ? {} : { capabilities }),
  });
  expect([registered.status, readied.status]).toEqual([200, 204]);
}
/** The state body as far as these rows read it before any contract: the issue's record, and
 * the issues holding a slot. */
const ServedState = z.looseObject({
  admission: z.looseObject({ active: z.array(z.string()) }),
  issues: z.record(
    z.string(),
    z.looseObject({
      architect: z.record(z.string(), z.unknown()).optional(),
      workers: z.record(
        z.string(),
        z.looseObject({ claim: z.looseObject({ locator: z.unknown().optional() }) })
      ),
      pullRequest: z.record(z.string(), z.unknown()).optional(),
    })
  ),
});
/** The record the stand-in whose state `body` reads serves now, and the issues in `active`. */
async function served(body: () => Promise<unknown>) {
  const state = ServedState.parse(await body());
  const issue = state.issues["LWEVAL-1"];
  if (issue === undefined) throw new Error("the state holds no record of LWEVAL-1");
  return { issue, active: state.admission.active };
}
/** Every path and every word of a kind a contract holds to an enum that the contract check hands
 * a label (standin-record.ts's `sampleFacts`, marked as this checkout's daemon marks them:
 * `statesContract`), and what of `issue` is in neither: a member or a word the stand-in serves
 * that no label's contract was held to before its runs began. */
const SAMPLED = sampleFacts(undefined, statesContract(CHECKOUT), repositoriesLive(CHECKOUT)).map(
  ({ facts }) => rolesListed(issueRecord(facts))
);
const ENUMS = ["state", "status", "phase", "mergeState", "reviewDecision", "runtime", "step"];
function words(value: unknown, key = ""): string[] {
  if (typeof value === "string") return ENUMS.includes(key) ? [`${key}=${value}`] : [];
  if (Array.isArray(value)) return value.flatMap((item) => words(item, key));
  if (typeof value !== "object" || value === null) return [];
  return Object.entries(value).flatMap(([name, inner]) => words(inner, name));
}
function unsampled(issue: unknown) {
  const { extra, kinds } = shapeDiff([rolesListed(issue)], { shape: shapeOf(SAMPLED) });
  const sampled = new Set(words(SAMPLED));
  return { extra, kinds, words: words(issue).filter((word) => !sampled.has(word)) };
}
const ALL_SAMPLED = { extra: [], kinds: [], words: [] };

// The record a threads-closed implementer reads before its first write (rig.sh's
// run_threads_closed, booted in `reviewing`), here on a world whose head an approval stands on
// and an open thread holds `BLOCKED`, the base requiring threads resolved, so the record holds the
// round's wait on GitHub's merge state too. Catches: the record of
// INFRA-764's round 25, 8 keys and `workers: {}`, in which every counted threads-closed run saw
// no implementer row where rig.sh said the record held the implementer's completed round; a
// record missing a key production's holds (the tester's read search names fourteen); a row whose
// session, record or commit is no value of the run's world — the sessions are the ones the run's
// Dispatch names each seeded record's author, the records the seeds' own ids and versions, the
// commit the head each seeded record names (the next row holds it below a reviewer's commit); a
// locator in another shape than production's sandbox
// locator or naming another project or issue than the run's; a pull request, a gate or a slot
// the world's other answers contradict; and a member in another kind than production's. The
// members production holds and this read does not are named: `WORLD_NEVER_HOLDS`, and four of
// this state — no capability report yet, no task (the daemon starts no phase for this loop), a
// turn still open, and no move back — each of which the next row reads in a state that has it.
// `mergeWait` is the one member production's read lacks: production's issue stood in
// `implementing`, and a round waits on GitHub's merge state in `reviewing` alone.
test.skipIf(skip)(
  "a read_record answers production's record: its keys, a row for each role the world's history ran, and production's shape",
  async () => {
    const daemon = await standin("implementer", "reviewing", undefined, {
      completed: "implementer",
      seeds: ["plan", "implement", "test", "review"],
    });
    try {
      const pull = pullRequest(daemon.dir, "BLOCKED");
      pull.approve();
      await boots(daemon.post);
      const { issue, active } = await served(daemon.body);
      expect(
        [
          ...PINNED_KEYS,
          ...["key", "generation", "phase", "status", "tree", "repo", "defaultBranch"],
          ...["architect", "workers", "pullRequest", "designGate", "gatedSteps"],
          ...["phaseEntry", "givenEntry"],
        ].filter((key) => !(key in issue))
      ).toEqual([]);
      expect(Object.keys(issue.workers)).toEqual(["implementer", "planner", "reviewer", "tester"]);
      const idle = (word: SeededWord) => ({
        session: SEEDED_SESSIONS[word],
        state: "idle",
        servingGeneration: 1,
        pluginContract: 146,
      });
      const world = "0".repeat(40);
      expect(issue).toMatchObject({
        key: "LWEVAL-1",
        generation: 1,
        phase: "reviewing",
        status: "needs_review",
        tree: "LWEVAL-1",
        repo: WORLD_REPO,
        defaultBranch: WORLD_BASE,
        architect: {
          session: SEEDED_SESSIONS.architect,
          state: "idle",
          servingGeneration: 1,
          pluginContract: 146,
        },
        workers: {
          implementer: {
            claim: { session: AGENT, state: "working", servingGeneration: 1, pluginContract: 146 },
            handoffCommit: world,
            handoffRecord: seededRecord("implement"),
            pass: 0,
            rounds: 0,
          },
          planner: { claim: idle("plan"), handoffRecord: seededRecord("plan"), pass: 0, rounds: 0 },
          tester: {
            claim: idle("test"),
            handoffCommit: world,
            handoffRecord: seededRecord("test"),
            pass: 0,
            rounds: 0,
          },
          reviewer: {
            claim: idle("review"),
            handoffRecord: seededRecord("review"),
            pass: 0,
            rounds: 0,
          },
        },
        pullRequest: {
          repo: WORLD_REPO,
          number: 1001,
          head: pull.head,
          state: "open",
          mergeState: "blocked",
          reviewDecision: "approved",
        },
        designGate: { artifactId: SEEDED_SPEC, currentVersion: 1, approvedVersion: 1 },
        gatedSteps: [
          {
            step: "spec",
            artifactId: SEEDED_SPEC,
            currentVersion: 1,
            approvedVersion: 1,
            status: "approved",
          },
        ],
        slot: { index: 0, admittedAt: SEEDED_AT },
        mergeWait: { waitsOn: pull.head, mergeState: "blocked", checkRuns: 0 },
        phaseEntry: 1,
        givenEntry: 1,
      });
      expect(active).toEqual(["LWEVAL-1"]);
      // A planner's and a reviewer's completion names no commit, as production's rows of the
      // two carry none.
      expect(
        ["planner", "reviewer"].filter((named) => "handoffCommit" in (issue.workers[named] ?? {}))
      ).toEqual([]);
      // Each claim's process is one of the run's own issue pod: production's sandbox locator,
      // naming the run's project and issue, the architect's the pod's first process and the
      // booted agent's its sixth, after the five the history ran.
      const located = z.object({
        runtime: z.literal("sandbox"),
        claim: z.string(),
        incarnation: z.string(),
        sandbox: z.object({
          namespace: z.string(),
          name: z.literal("legion-skills1-lweval-1"),
          podUid: z.uuid(),
          container: z.literal("roles"),
          generation: z.number(),
        }),
      });
      const architect = located.parse(issue.architect?.locator);
      const own = located.parse(issue.workers.implementer?.claim.locator);
      expect([architect.claim, architect.incarnation, architect.sandbox.generation]).toEqual([
        roleToken("skills1", "LWEVAL-1", "architect"),
        `${own.sandbox.podUid}/1`,
        1,
      ]);
      expect([own.claim, own.incarnation, own.sandbox.generation]).toEqual([
        roleToken("skills1", "LWEVAL-1", "implementer"),
        `${own.sandbox.podUid}/6`,
        6,
      ]);
      expect(shapeDiff(rolesListed(issue), PINNED_RECORD)).toEqual({
        missing: [
          ...WORLD_NEVER_HOLDS.slice(0, 5),
          ".workers[].claim.capabilities",
          ".workers[].claim.pendingTask",
          ".workers[].claim.lastTurnEnd",
          ".workers[].claim.step",
          ".workers[].backwardMoves",
        ],
        extra: [...SINCE_PIN, ".mergeWait"],
        kinds: [],
      });
      expect(unsampled(issue)).toEqual(ALL_SAMPLED);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that gives the seeded implementer's and tester's rows the world's head
// where the reviewer's own text commit stands above the commit their records name (rig.sh's
// approved_world in accept, correct and crossing, whose seeds record both passes at the
// implementer's commit): production's rows name the head each role's own completion named, and
// the reviewer's push is no step of theirs, so a row naming the reviewer's commit says the
// implementer and the tester finished at a head neither of them saw.
test.skipIf(skip)(
  "a seeded implementer's and tester's rows name the head their own records name, below a reviewer's commit",
  async () => {
    const below = "1".repeat(40);
    const daemon = await standin("implementer", "reviewing", undefined, {
      completed: "implementer",
      seeds: ["plan", "implement", "test", "review"],
      heads: { implement: below, test: below },
    });
    try {
      await boots(daemon.post);
      const { issue } = await served(daemon.body);
      expect(issue.workers).toMatchObject({
        implementer: { handoffCommit: below, handoffRecord: seededRecord("implement") },
        tester: { handoffCommit: below, handoffRecord: seededRecord("test") },
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// The record a reviewer-text-findings reviewer reads (rig.sh's run_reviewer_text_findings: booted
// in `reviewing`, its own phase, on a world whose tester has finished), and the record after its
// request for changes ended the round. Catches: a booted claim that holds no task where the
// daemon started its phase, or keeps it past the turn that confirmed it; a claim that reports its
// capabilities and whose record holds none; a record that stays as it was when the stand-in
// moves the issue — the phase and its status, the implementer's pass, round and pair of the move
// (workflow's `recordPass` and `recordRound`), its handoff cleared and its entry raised with the
// task queued to its claim (`transition`'s start), and the round's decision on the pull request
// — which is the record a reviewer told to read it after its review would read; and, in the
// state that holds every member the row above lacked, any difference from production's shape but
// `WORLD_NEVER_HOLDS`.
test.skipIf(skip)(
  "a booted reviewer's claim holds the task of its phase, and a request for changes moves the whole record",
  async () => {
    const daemon = await standin("reviewer", "", undefined, {
      seeds: ["plan", "implement", "test"],
    });
    try {
      const pull = pullRequest(daemon.dir, "BLOCKED");
      await boots(daemon.post, AGENT, REPORT);
      const before = (await served(daemon.body)).issue;
      expect(Object.keys(before.workers)).toEqual(["implementer", "planner", "reviewer", "tester"]);
      expect(before).toMatchObject({
        phase: "reviewing",
        status: "needs_review",
        workers: {
          reviewer: {
            claim: {
              session: AGENT,
              state: "working",
              capabilities: { measuredAt: REPORT.measuredAt, ok: ["github", "mcp"], open: [] },
              pendingTask: { phase: "reviewing" },
            },
            pass: 0,
            rounds: 0,
          },
        },
        pullRequest: { head: pull.head, state: "open", mergeState: "blocked" },
        phaseEntry: 1,
        givenEntry: 1,
      });
      // No review has decided the round, the reviewer has retired no task and named no record,
      // and a round nobody approved waits on nothing from GitHub; its turn began where it said
      // it could be prompted, and has not ended.
      const reviewer = before.workers.reviewer ?? { claim: {} };
      expect([
        "reviewDecision" in (before.pullRequest ?? {}),
        "mergeWait" in before,
        "servingGeneration" in reviewer.claim,
        "lastTurnEnd" in reviewer.claim,
        "handoffRecord" in reviewer,
        "lastTurnStart" in reviewer.claim,
        "lastReady" in reviewer.claim,
      ]).toEqual([false, false, false, false, false, true, true]);
      expect(unsampled(before)).toEqual(ALL_SAMPLED);

      pull.requestChanges();
      expect(
        (await daemon.post("/legion/v1/claims/turn-outcome", turn("reviewer", 1, "clean"))).status
      ).toBe(200);
      const { issue } = await served(daemon.body);
      expect(issue).toMatchObject({
        phase: "implementing",
        status: "in_progress",
        workers: {
          implementer: {
            claim: {
              session: SEEDED_SESSIONS.implement,
              state: "idle",
              pendingTask: { phase: "implementing" },
            },
            pass: 1,
            rounds: 1,
            backwardMoves: { "reviewing-implementing": 1 },
          },
          reviewer: { claim: { state: "idle", servingGeneration: 1 } },
          tester: { handoffCommit: "0".repeat(40), handoffRecord: seededRecord("test") },
        },
        pullRequest: { reviewDecision: "changes_requested" },
        slot: { index: 0, admittedAt: SEEDED_AT },
        phaseEntry: 2,
        givenEntry: 2,
      });
      const implementer = issue.workers.implementer ?? {};
      expect([
        "handoffCommit" in implementer,
        "handoffRecord" in implementer,
        "pendingTask" in (issue.workers.reviewer?.claim ?? {}),
        "lastTurnEnd" in (issue.workers.reviewer?.claim ?? {}),
      ]).toEqual([false, false, false, true]);
      expect(shapeDiff(rolesListed(issue), PINNED_RECORD)).toEqual({
        missing: WORLD_NEVER_HOLDS,
        extra: SINCE_PIN,
        kinds: [],
      });
      expect(unsampled(issue)).toEqual(ALL_SAMPLED);
      // A review after the round has ended decides no round: the record keeps the decision
      // that ended it (`api.PullRequestView`: from the review that decided it until the next
      // round opens), where a record read off the world's last review would call it approved.
      pull.approve();
      expect((await served(daemon.body)).issue).toMatchObject({
        phase: "implementing",
        pullRequest: { reviewDecision: "changes_requested" },
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

/** A move of the issue back to `to` through the stand-in `post` reaches, on a grant minted for
 * it, as the tool's `request_backward_move` makes one: the answer's status. */
async function movesBack(
  post: (route: string, payload: unknown) => Promise<Response>,
  to: string
): Promise<number> {
  const minted = await (
    await post("/legion/v1/grants", {
      sessionId: AGENT,
      secret: SECRET,
      tree: "LWEVAL-1",
      issue: "LWEVAL-1",
    })
  ).json();
  const { grantId } = z.object({ grantId: z.string() }).parse(minted);
  return (await post("/legion/v1/phase/backward", { grantId, to, reason: "the fix needs code" }))
    .status;
}

// Catches: a pull request the record reads once, at boot, so a push the gh stand-in already
// answers at its new head is a head the record does not name; an issue the round's approval
// moved to `accepting` that still holds its slot and stands in `admission.active` (an issue past
// its review holds none: record's `PastReview`), or whose acceptance was queued to no one (the
// architect's entry and the task of it, of no phase); a completion the stand-in took that its
// role's row does not carry, so the next reader finds no handoff to read; and a worker's own
// move back that counts as a review round, or that leaves the handoff of the role it starts.
test.skipIf(skip)(
  "the record follows the world: a push, an approval that ends the round, a completion, and a worker's move back",
  async () => {
    const reviewer = await standin("reviewer", "", undefined, {
      seeds: ["plan", "implement", "test"],
    });
    try {
      const pull = pullRequest(reviewer.dir);
      await boots(reviewer.post);
      pull.approve();
      const accepted = await served(reviewer.body);
      expect(accepted.issue).toMatchObject({
        phase: "accepting",
        status: "needs_review",
        architect: { state: "idle" },
        pullRequest: { head: pull.head, mergeState: "clean", reviewDecision: "approved" },
        phaseEntry: 2,
        givenEntry: 2,
      });
      expect([
        accepted.active,
        "slot" in accepted.issue,
        "mergeWait" in accepted.issue,
        "phase" in
          z.looseObject({ pendingTask: z.looseObject({}) }).parse(accepted.issue.architect)
            .pendingTask,
      ]).toEqual([[], false, false, false]);
      expect(unsampled(accepted.issue)).toEqual(ALL_SAMPLED);
      const pushed = pull.push();
      expect((await served(reviewer.body)).issue).toMatchObject({
        phase: "accepting",
        pullRequest: { head: pushed, reviewDecision: "approved" },
      });
    } finally {
      reviewer.stop();
    }
    const tester = await standin("tester", "", undefined, { seeds: ["plan", "implement"] });
    try {
      await boots(tester.post);
      const commit = "7".repeat(40);
      expect((await tester.complete(true, { commit })).status).toBe(200);
      expect((await served(tester.body)).issue).toMatchObject({
        phase: "testing",
        status: "testing",
        workers: {
          tester: { handoffCommit: commit, handoffRecord: { id: "h-review", version: 1 } },
        },
      });
      expect(await movesBack(tester.post, "implementing")).toBe(200);
      const { issue } = await served(tester.body);
      expect(issue).toMatchObject({
        phase: "implementing",
        status: "in_progress",
        workers: {
          implementer: {
            claim: { pendingTask: { phase: "implementing" } },
            pass: 1,
            rounds: 0,
            backwardMoves: { "testing-implementing": 1 },
          },
          tester: { handoffCommit: commit, handoffRecord: { id: "h-review", version: 1 } },
        },
        phaseEntry: 2,
        givenEntry: 2,
      });
      expect("handoffRecord" in (issue.workers.implementer ?? {})).toBe(false);
      expect(unsampled(issue)).toEqual(ALL_SAMPLED);
    } finally {
      tester.stop();
    }
  },
  600_000
);

/** The identity of the run of `phase`'s step on LWEVAL-1, as every write of its record names it:
 * the step's place, kind, role and type (workflow/steps.go's `stepOf`), the claim of the role that
 * runs it in the rows' project `skills1`, and the issue's generation. */
function stepIdentity(phase: "testing" | "implementing") {
  const [step, role, type] =
    phase === "testing" ? [3, "tester", "testing"] : [2, "implementer", "implementation"];
  const agent = roleToken("skills1", "LWEVAL-1", role as LegionRole);
  return { task: "default", step, kind: "step", role, type, agent, generation: 1 };
}

// Catches: a stand-in that writes no record of the steps the label's daemon records (main's since
// #4400), or writes one otherwise than the daemon's executor sends it (outbox.go's
// `publishStepRecord`, workflow/steps.go's `startStep` and `endStep`): the start of the step the
// issue stands in at its boot, its identity and the moment alone; at a worker's move back, the end
// of that step, failed, returning to the place of the step the move enters, at the moment its
// start wrote and the moment now, with the session of the agent that ran it and no model, since
// that agent's transcript names none; and the start of the next run of the step it enters, the
// second of its place, under that role's claim. Each goes under the daemon's own session, by the
// id the daemon derives, the end by its start's id.
test.skipIf(skip)(
  "a stand-in writes the start of its step at its boot, and at a move that step's end and the next one's start",
  async () => {
    const tester = await standin("tester", "", undefined, { seeds: ["plan", "implement"] });
    try {
      const opened = stepWrites(tester.writes);
      expect(opened).toEqual([
        [
          stepId("LWEVAL-1", 1, "testing", 1),
          { ...stepIdentity("testing"), started_at: expect.any(String) },
        ],
      ]);
      await boots(tester.post);
      expect(await movesBack(tester.post, "implementing")).toBe(200);
      expect(stepWrites(tester.writes).slice(1)).toEqual([
        [
          stepId("LWEVAL-1", 1, "testing", 1),
          {
            ...stepIdentity("testing"),
            started_at: opened[0]?.[1].started_at,
            ended_at: expect.any(String),
            result: "failed",
            returned_to: 2,
            session: AGENT,
          },
        ],
        [
          stepId("LWEVAL-1", 1, "implementing", 2),
          { ...stepIdentity("implementing"), started_at: expect.any(String) },
        ],
      ]);
    } finally {
      tester.stop();
    }
  },
  600_000
);

// Against main `ff2a10ef`'s own daemon, where the host has a checkout of it
// (SKILL_SCENARIOS_OLDER_CHECKOUT): its outbox has no step record, so its stand-in writes none,
// where this checkout's writes the start of its step on the same run. Catches: a stand-in that
// writes a record no daemon of that label writes, whose Dispatch serves no route for it either.
test.skipIf(skip || olderHost === undefined)(
  "a stand-in of main ff2a10ef's daemon writes no step record",
  async () => {
    for (const [checkout, written] of [
      [olderHost ?? "", 0],
      [CHECKOUT, 1],
    ] as const) {
      const tester = await standin("tester", "", undefined, { checkout });
      try {
        expect([checkout, stepWrites(tester.writes).length]).toEqual([checkout, written]);
      } finally {
        tester.stop();
      }
    }
  },
  600_000
);

/** A handoff record of `word` at `version` the session the rows' grants are minted for wrote,
 * or `session` did, by its id. */
function ownRecord(id: string, word: string, version: number, session = "01a0f2cc") {
  return {
    [id]: {
      id,
      issue_key: "LWEVAL-1",
      phase: word,
      version,
      author: { kind: "session", id: session },
      body: JSON.stringify({ head: WORLD_HEAD }),
    },
  };
}

// The completions production refuses of a role whose round the world's seeds stand for
// (threads-closed: the implementer's implementing round and the reviewer's review round, each
// with its record on its row). Catches: a stand-in whose engine reads no row but the ones the
// rig names (`COMPLETED`), which took a threads-closed reviewer's first late completion 200 —
// 3 of INFRA-764 round 25's runs, in `accepting` — where the daemon's `leftItsPhase` refuses a
// role whose row holds a completion; one that takes a completion naming a record another
// session wrote, the seed's, which the daemon's route refuses 403 HANDOFF_SESSION_MISMATCH and
// tells the claim to write its own (measured on round 25: each of the 31 records an agent wrote
// named the session that agent registered, so the check holds a record to the right session);
// and one that answers each in words of its own rather than the daemon's.
test.skipIf(skip)(
  "a role whose seeded round stands is refused as the daemon refuses it: late, and naming a record another session wrote",
  async () => {
    const seeds = ["plan", "implement", "test", "review"] as const;
    const mismatch = (word: SeededWord, version: number) => ({
      code: "HANDOFF_SESSION_MISMATCH",
      error: `handoff record seed-${word} (${word} version ${version}) was written by session ${SEEDED_SESSIONS[word]}, not by this claim's session 01a0f2cc: write the handoff again with \`dispatch handoff-write --issue LWEVAL-1 --phase ${word} --body-file <file>\` from this session, then complete naming the new record; this completion changed nothing`,
    });
    for (const phase of ["reviewing", "accepting"]) {
      const implementer = await standin("implementer", phase, undefined, {
        seeds,
        records: ownRecord("own-implement", "implement", 3),
      });
      try {
        const seeded = await implementer.complete(true, {
          handoffId: "seed-implement",
          handoffVersion: 2,
        });
        expect([phase, seeded.status, await seeded.json()]).toEqual([
          phase,
          403,
          mismatch("implement", 2),
        ]);
        const own = await implementer.complete(true, {
          handoffId: "own-implement",
          handoffVersion: 3,
          commit: "7".repeat(40),
        });
        expect([phase, own.status, await own.json()]).toEqual([
          phase,
          409,
          lateRefusal("implementer", phase),
        ]);
      } finally {
        implementer.stop();
      }
    }
    // The reviewer booted where the round has ended: its seeded completion stands, so its
    // first completion here is a late one of a completed row, whatever record it names.
    const late = await standin("reviewer", "accepting", undefined, {
      seeds,
      records: ownRecord("own-review", "review", 2),
    });
    try {
      const seeded = await late.complete(true, { handoffId: "seed-review", handoffVersion: 1 });
      expect([seeded.status, await seeded.json()]).toEqual([403, mismatch("review", 1)]);
      const own = await late.complete(true, { handoffId: "own-review", handoffVersion: 2 });
      expect([own.status, await own.json()]).toEqual([409, lateRefusal("reviewer", "accepting")]);
      expect(
        late
          .logged()
          .filter((line) => line.path === "/legion/v1/handoff/complete")
          .map((line) => [line.status, line.code])
      ).toEqual([
        [403, "HANDOFF_SESSION_MISMATCH"],
        [409, "HANDOFF_NOT_CURRENT_PHASE"],
      ]);
    } finally {
      late.stop();
    }
    // In `reviewing`, its own phase, a completion naming a record of its own is taken, and the
    // row then carries that record.
    const current = await standin("reviewer", "reviewing", undefined, {
      seeds,
      records: ownRecord("own-review", "review", 2),
    });
    try {
      const own = await current.complete(true, { handoffId: "own-review", handoffVersion: 2 });
      expect(own.status).toBe(200);
      expect((await served(current.body)).issue.workers.reviewer).toMatchObject({
        handoffRecord: { id: "own-review", version: 2 },
      });
    } finally {
      current.stop();
    }
  },
  600_000
);

// Catches: a stand-in that takes the record a role's last completion named as a new phase's
// handoff (the engine's HANDOFF_NOT_NEW: the row's `LastHandoff`, which a seed's completion
// set as much as one taken here), or answers the seeded completion sent again as a new one (its
// fact is one the workflow was handed: HANDOFF_ALREADY_RECORDED); and one that refuses a record
// the daemon's own session wrote in the words for another worker's, where the daemon says the
// record was carried from before handoffs moved to the Dispatch issue. The seeded records here
// are ones the completing session wrote itself, which is the one case the route's session check
// lets through. Another fact naming the same record is the tester's alone: a verdict is the one
// field a role may report otherwise of one handoff, and only the tester reports one
// (VERDICT_ROLE_FORBIDDEN, the row below).
test.skipIf(skip)(
  "the record a role's row last named is no new handoff, and a completion the workflow was handed is not handed again",
  async () => {
    const reviewer = await standin("reviewer", "reviewing", undefined, {
      seeds: ["plan", "implement", "test", "review"],
      records: {
        ...ownRecord("seed-review", "review", 1),
        ...ownRecord("daemon-review", "review", 3, "legion-daemon:LWEVAL"),
      },
    });
    try {
      const named = { handoffId: "seed-review", handoffVersion: 1 };
      // The seeded completion, sent again: the reviewer's of `reviewing`, with no verdict.
      const again = await reviewer.complete(true, { ...named, verdict: "" });
      expect([again.status, await again.json()]).toEqual([
        409,
        {
          code: "HANDOFF_ALREADY_RECORDED",
          error:
            "the reviewer completion of phase reviewing (pass 0 through it) naming handoff record seed-review (review version 1) was already received; this call changed nothing",
        },
      ]);
      const carried = await reviewer.complete(true, {
        handoffId: "daemon-review",
        handoffVersion: 3,
      });
      expect([carried.status, await carried.json()]).toEqual([
        403,
        {
          code: "HANDOFF_SESSION_MISMATCH",
          error:
            "handoff record daemon-review (review version 3) was written by the daemon's own session legion-daemon:LWEVAL, as a record carried from before handoffs moved to the Dispatch issue is, not by this claim's session 01a0f2cc: read it with `dispatch handoff-read --issue LWEVAL-1 --phase review`, write it again with `dispatch handoff-write --issue LWEVAL-1 --phase review --body-file <file>` from this session, then complete naming the new record; this completion changed nothing",
        },
      ]);
      // Neither wrote the row.
      expect((await served(reviewer.body)).issue.workers.reviewer).toMatchObject({
        handoffRecord: seededRecord("review"),
      });
    } finally {
      reviewer.stop();
    }
    const tester = await standin("tester", "", undefined, {
      seeds: ["plan", "implement", "test"],
      records: ownRecord("seed-test", "test", 1),
    });
    try {
      const named = { handoffId: "seed-test", handoffVersion: 1 };
      // The seeded completion, sent again: the tester's of `testing`, with its passing verdict.
      const again = await tester.complete(true, named);
      expect([again.status, await again.json()]).toEqual([
        409,
        {
          code: "HANDOFF_ALREADY_RECORDED",
          error:
            "the tester completion of phase testing (pass 0 through it) naming handoff record seed-test (test version 1) was already received; this call changed nothing",
        },
      ]);
      // The same record under another verdict is another fact, of a record that is not new.
      const stale = await tester.complete(true, { ...named, verdict: "fail" });
      expect([stale.status, await stale.json()]).toEqual([
        409,
        {
          code: "HANDOFF_NOT_NEW",
          error:
            "the tester named handoff record seed-test for its previous phase of LWEVAL-1; write this phase's handoff with `dispatch handoff-write --issue LWEVAL-1 --phase test --body-file <file>` before completing",
        },
      ]);
      expect((await served(tester.body)).issue.workers.tester).toMatchObject({
        handoffRecord: seededRecord("test"),
      });
    } finally {
      tester.stop();
    }
  },
  600_000
);

// Catches: a stand-in that takes a rating of the plan on a completion that does not end the
// retro, which main's engine since #4488 refuses past HANDOFF_NOT_NEW (400 PLAN_RATING_UNEXPECTED,
// in its words: daemon-pane.ts's `PLAN_RATING_UNEXPECTED`, naming the phase the completion is of);
// one that refuses it before HANDOFF_NOT_NEW; and one that records the refused completion, whose
// fact the engine leaves unspent, so that the same completion sent again without the rating is
// answered HANDOFF_ALREADY_RECORDED where the daemon takes it. The `legion` tool sends a rating its
// agent gives on any completion (legion-phase-ops.ts's `executePhaseOp`).
test.skipIf(skip)(
  "a completion that rates the plan outside the retro is refused past HANDOFF_NOT_NEW, recording nothing",
  async () => {
    const tester = await standin("tester", "", undefined, {
      seeds: ["plan", "implement", "test"],
      records: { ...ownRecord("seed-test", "test", 1), ...ownRecord("new-test", "test", 2) },
    });
    try {
      const rating = { planRating: 4, planReason: "the plan fit the work" };
      const stale = await tester.complete(true, {
        handoffId: "seed-test",
        handoffVersion: 1,
        verdict: "fail",
        ...rating,
      });
      expect([stale.status, z.object({ code: z.string() }).parse(await stale.json()).code]).toEqual(
        [409, "HANDOFF_NOT_NEW"]
      );
      const named = { handoffId: "new-test", handoffVersion: 2 };
      const rated = await tester.complete(true, { ...named, ...rating });
      expect([rated.status, await rated.json()]).toEqual([
        400,
        {
          code: "PLAN_RATING_UNEXPECTED",
          error:
            "this completion of phase testing of LWEVAL-1 names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded",
        },
      ]);
      expect((await served(tester.body)).issue.workers.tester).toMatchObject({
        handoffRecord: seededRecord("test"),
      });
      expect((await tester.complete(true, named)).status).toBe(200);
      expect((await served(tester.body)).issue.workers.tester).toMatchObject({
        handoffRecord: { id: "new-test", version: 2 },
      });
    } finally {
      tester.stop();
    }
  },
  600_000
);

/** How the daemon's refusal of a body holding U+0000 ends, after where the character stands: in
 * main's words since #4463 (2d3d73331b, this checkout's), and in #4488's before them, main
 * `724196ef`'s (daemon-pane.ts's `NUL_REFUSAL`). */
const NUL_REFUSED = "which the daemon API refuses rather than store it as U+FFFD";
const NUL_UNSTORABLE = "which the daemon cannot store";
let unstorable: string | undefined;
/** A copy of this checkout's daemon module whose decoder refuses a body holding U+0000 in #4488's
 * words, as main `724196ef`'s does, and is this checkout's in all else. Made once for the file. */
function unstorableCheckout(): string {
  if (unstorable !== undefined) return unstorable;
  const { checkout, daemon } = daemonCheckout("unstorable-checkout");
  const storable = path.join(daemon, "internal", "api", "storable_text.go");
  const refusing = readFileSync(storable, "utf8");
  const unstoring = refusing.replace(
    `at character %d, ${NUL_REFUSED}"`,
    `at character %d, ${NUL_UNSTORABLE}"`
  );
  expect(unstoring).not.toBe(refusing);
  writeFileSync(storable, unstoring);
  unstorable = checkout;
  return checkout;
}

// Catches: a stand-in that takes a request body a string of which holds U+0000 for a label whose
// daemon refuses one, main's since #4488, which refuses it 400 before any route reads it, the
// claim's secret and the grant among them (claims.go's `decodeJSON`, storable_text.go's
// `unstorableJSON`), in its words (daemon-pane.ts's `NUL_REFUSAL`): main's since #4463 for this
// checkout, and #4488's for a copy of it in those words and for main `724196ef` where
// SKILL_SCENARIOS_BASE_CHECKOUT names it; one that answers a label in another label's words; one
// that names a later string than the first the body writes, or a nested one otherwise than
// Dispatch's walk names it, a member's name among them; and one that counts where the character
// stands in UTF-16 units rather than code points. An agent's summary or reason is its own text,
// which the `legion` tool sends as the agent gave it.
test.skipIf(skip)(
  "a body holding U+0000 is refused before its route reads it, naming the first string that holds one, in the label's own words",
  async () => {
    for (const [checkout, words] of [
      [CHECKOUT, NUL_REFUSED],
      [unstorableCheckout(), NUL_UNSTORABLE],
      ...(baseCheckout === undefined ? [] : [[baseCheckout, NUL_UNSTORABLE] as const]),
    ] as const) {
      const daemon = await standin("reviewer", "", undefined, { checkout });
      try {
        const refusal = (place: string, at: number) => ({
          error: `invalid request body: ${place} holds a NUL character (U+0000) at character ${at}, ${words}`,
        });
        const summary = await daemon.complete(true, {
          summary: "the 🦀 round\u0000 is in",
          planReason: "\u0000",
        });
        expect([checkout, summary.status, await summary.json()]).toEqual([
          checkout,
          400,
          refusal("summary", 12),
        ]);
        const ready = await daemon.post("/legion/v1/claims/ready", {
          secret: fixtureSecret("other"),
          capabilities: {
            measuredAt: "2026-11-19T10:00:00Z",
            rows: [
              { name: "github", ok: true, detail: "ok" },
              { name: "mcp", ok: false, detail: "off\u0000" },
            ],
          },
        });
        expect([checkout, ready.status, await ready.json()]).toEqual([
          checkout,
          400,
          refusal("capabilities.rows[1].detail", 4),
        ]);
        const named = await daemon.post("/legion/v1/grants", { "session\u0000Id": "01a0f2cc" });
        expect([checkout, named.status, await named.json()]).toEqual([
          checkout,
          400,
          refusal("session\\u0000Id", 8),
        ]);
        expect(daemon.logged().filter((line) => line.status === 400)).toHaveLength(3);
        expect([checkout, (await daemon.complete(true)).status]).toEqual([checkout, 200]);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// What main `ff2a10ef`'s daemon answers a body holding U+0000, derived from three reads and not
// from a booted daemon (the architect's ruling of 2026-11-19T22:19Z): PostgreSQL on this pod
// refuses U+0000 in text (`invalid byte sequence for encoding "UTF8": 0x00`) and in jsonb
// (`unsupported Unicode escape sequence`), read with psql at 22:1xZ that day; that daemon's
// decoder takes the body (api/claims.go's `decodeJSON`, no walk of its strings), its engine writes
// a taken completion's summary and commit onto the role's row (workflow/engine.go:580) through
// `insert into phases (… summary …)`, text columns (record/store.go:504-512), and a taken backward
// move's reason into the task of the role it starts (workflow/engine.go:923, effects.go:449-450),
// an outbox payload in a jsonb column (store/migrations/0004_record.up.sql:76); and its routes
// answer a failed apply 500 FACT_APPLY_FAILED, "could not apply handoff fact" for a completion
// (api/handoff.go:336-337) and "could not apply fact" for a move (api/architect.go:262-263). A
// ready's capability detail is cleaned of every control character before it is kept
// (capabilities/capabilities.go:288-296), so a ready writes none. Each row boots the stand-in on
// that checkout (SKILL_SCENARIOS_OLDER_CHECKOUT), whose source the stand-in reads the shape of.
//
// Catches: a stand-in that answers a completion whose summary holds U+0000 with a success that
// daemon's store never gives, or with main's refusal, which that daemon's decoder does not make;
// one that records a completion whose apply failed, whose fact is then unspent; and one that
// fails a ready, which writes no such text.
test.skipIf(skip || olderHost === undefined)(
  "an older daemon takes a body holding U+0000, and its store fails the completion that writes it",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { checkout: olderHost ?? "" });
    try {
      const ready = await daemon.post("/legion/v1/claims/ready", {
        secret: SECRET,
        capabilities: {
          measuredAt: "2026-11-19T10:00:00Z",
          rows: [{ name: "mcp", ok: false, detail: "off\u0000" }],
        },
      });
      expect(ready.status).toBe(204);
      const stored = await daemon.complete(true, { summary: "the 🦀 round\u0000 is in" });
      expect([stored.status, await stored.json()]).toEqual([
        500,
        { code: "FACT_APPLY_FAILED", error: "could not apply handoff fact" },
      ]);
      expect((await served(daemon.body)).issue.workers.reviewer).not.toHaveProperty(
        "handoffRecord"
      );
      expect((await daemon.complete(true)).status).toBe(200);
      expect(daemon.logged().filter((line) => line.status === 400)).toEqual([]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that moves the issue on a backward move whose reason holds U+0000, which
// main `ff2a10ef`'s store fails where the task of the role the move starts carries the reason
// (the reads above), answered 500 FACT_APPLY_FAILED with nothing moved; and one that keeps that
// failure for a move the same reason without the character then makes.
test.skipIf(skip || olderHost === undefined)(
  "an older daemon's store fails a backward move whose reason holds U+0000, and nothing moves",
  async () => {
    const daemon = await standin("reviewer", "", undefined, { checkout: olderHost ?? "" });
    try {
      expect(await askedBack(daemon.post, "implementing", "the fix needs\u0000 code")).toEqual([
        500,
        { code: "FACT_APPLY_FAILED", error: "could not apply fact" },
      ]);
      expect(await daemon.phase()).toBe("reviewing");
      expect(await askedBack(daemon.post, "implementing", "the fix needs code")).toEqual([200, {}]);
      expect(await daemon.phase()).toBe("implementing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

/** A copy of this checkout's daemon module whose state answer counts a pull request's fix
 * attempts, as main `ff2a10ef`'s does (its api's `PullRequestView.FixAttempts`, which its
 * contract requires), and is this checkout's in all else. Made once for the file. */
let counting: string | undefined;
function countingCheckout(): string {
  if (counting !== undefined) return counting;
  const { checkout, daemon } = daemonCheckout("counting-checkout");
  const state = path.join(daemon, "internal", "api", "state.go");
  const uncounted = readFileSync(state, "utf8");
  const counted = uncounted.replace(COUNTLESS_VIEW, COUNTED_VIEW);
  expect(counted).not.toBe(uncounted);
  writeFileSync(state, counted);
  counting = checkout;
  return checkout;
}
/** The end of the state's pull request view as main's daemon writes it since #3659, its round's
 * decision and then the flag of the one copy a caller below contract 59 is served the count on
 * (daemon-pane.ts's `PULL_REQUEST_SHOWN_BELOW`), and as main `ff2a10ef`'s ends it, with the count
 * of fix attempts for every caller (`PULL_REQUEST_COUNTED`): the flag stays a field of the view,
 * ahead of the decision, since the copy's module is compiled and its MarshalJSON reads it. */
const COUNTLESS_VIEW =
  /\tReviewDecision string `json:"reviewDecision,omitempty"`\n\t\/\/ fixAttemptsShown [\s\S]*?\tfixAttemptsShown bool\n\}/;
const COUNTED_VIEW =
  '\tfixAttemptsShown bool\n\tReviewDecision string `json:"reviewDecision,omitempty"`\n\tFixAttempts    int    `json:"fixAttempts"`\n}';

// Catches: a stand-in that serves a label of main `ff2a10ef`'s shape a pull request with no count
// of fix attempts, which that label's contract requires (its legion-api.ts's `fixAttempts`), so
// every `read_record` of its agents is refused; and one that serves the count to a label whose
// daemon keeps none, main's since contract 59: this checkout, and main `724196ef` where
// SKILL_SCENARIOS_BASE_CHECKOUT names it. Every check of a world's head concludes `success`
// (rig.sh's `concluded_checks`), so the count is 0.
test.skipIf(skip)(
  "the pull request counts its fix attempts where the label's daemon serves the count, and only there",
  async () => {
    const checkouts: [string, boolean][] = [
      [CHECKOUT, false],
      [countingCheckout(), true],
      ...(baseCheckout === undefined ? [] : [[baseCheckout, false] as [string, boolean]]),
    ];
    for (const [checkout, counts] of checkouts) {
      const daemon = await standin("reviewer", "", undefined, { checkout });
      try {
        const state = z
          .object({
            issues: z.record(
              z.string(),
              z.object({ pullRequest: z.record(z.string(), z.unknown()) })
            ),
          })
          .parse(await daemon.body());
        const pull = state.issues["LWEVAL-1"]?.pullRequest ?? {};
        expect([checkout, pull.fixAttempts]).toEqual([checkout, counts ? 0 : undefined]);
        expect([checkout, Object.keys(pull).at(-1)]).toEqual([
          checkout,
          counts ? "fixAttempts" : "state",
        ]);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in that takes a completion's verdict from any role, and a tester's completion
// with none. The daemon's completion route refuses, before the fact and so writing nothing, a
// verdict from any role but the tester (400 VERDICT_ROLE_FORBIDDEN), which the reviewer's prompt
// names and the `legion` tool sends whenever the agent names one, and a tester's that is neither
// `pass` nor `fail` (400 TESTER_VERDICT_REQUIRED), each in every daemon's words
// (daemon-pane.ts's VERDICT_REFUSALS). The same completions without the fault are taken.
test.skipIf(skip)(
  "a completion's verdict is the tester's alone, and the tester's is pass or fail",
  async () => {
    const reviewer = await standin("reviewer");
    try {
      const refused = await reviewer.complete(true, { verdict: "pass" });
      expect([refused.status, await refused.json()]).toEqual([
        400,
        { code: "VERDICT_ROLE_FORBIDDEN", error: "only a tester may report a verdict" },
      ]);
      expect("handoffRecord" in ((await served(reviewer.body)).issue.workers.reviewer ?? {})).toBe(
        false
      );
      expect((await reviewer.complete(true)).status).toBe(200);
    } finally {
      reviewer.stop();
    }
    const tester = await standin("tester");
    try {
      for (const verdict of ["", "pending"]) {
        const refused = await tester.complete(true, { verdict });
        expect([verdict, refused.status, await refused.json()]).toEqual([
          verdict,
          400,
          { code: "TESTER_VERDICT_REQUIRED", error: "tester verdict must be pass or fail" },
        ]);
      }
      expect((await tester.complete(true, { verdict: "fail" })).status).toBe(200);
    } finally {
      tester.stop();
    }
  },
  600_000
);

let codeless: string | undefined;
/** A third checkout of this one's daemon module, standing in for a source whose markers of three
 * completion refusals are gone stale: a copy, as `olderCheckout` is, in whose `api/handoff.go`
 * and `workflow/engine.go` none of HANDOFF_SESSION_MISMATCH, HANDOFF_ALREADY_RECORDED and
 * HANDOFF_NOT_NEW is named. Made once for the file. */
function codelessCheckout(): string {
  if (codeless !== undefined) return codeless;
  const { checkout, daemon } = daemonCheckout("codeless-checkout");
  let renamed = 0;
  for (const file of ["api/handoff.go", "workflow/engine.go"]) {
    const source = path.join(daemon, "internal", file);
    writeFileSync(
      source,
      readFileSync(source, "utf8").replaceAll(
        /"HANDOFF_(SESSION_MISMATCH|ALREADY_RECORDED|NOT_NEW)"/g,
        () => {
          renamed += 1;
          return '"HANDOFF_REFUSED"';
        }
      )
    );
  }
  expect(renamed).toBeGreaterThanOrEqual(3);
  expect(labelDaemon(checkout)).toEqual(labelDaemon(CHECKOUT));
  codeless = checkout;
  return checkout;
}

// Catches: a stand-in that reads a refusal its label's source does not name as one that daemon
// lacks, and so answers the label as an older daemon: the seeded completion sent again, the
// record its row last named and a record another session wrote were each taken here, where the
// row above refuses all three. Every daemon the rig runs names the three (this checkout's and main
// `ff2a10ef`'s), so a source that names one nowhere holds a marker gone stale, and the boot is
// refused, naming the code and its file (daemon-pane.ts's `labelShapes`), as rig.sh's
// label_facts refuses the batch before any run (the architect's ruling of 2026-11-18). This row
// held all three taken before that ruling.
test.skipIf(skip)(
  "a daemon whose source names a completion's refusal nowhere is refused at the boot, naming the code and its file",
  () => {
    const checkout = codelessCheckout();
    const dir = path.join(work, "reviewer-codeless");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "boot-token"), `${BOOT_TOKEN}\n`);
    writeFileSync(path.join(dir, "built-from"), `checkout ${checkout}\n`);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        branch: "legion/LWEVAL-1",
        pr: 1001,
        head: "0".repeat(40),
        repo: WORLD_REPO,
        base: WORLD_BASE,
      })
    );
    const boot = spawnChecked([
      "bun",
      DAEMON_STANDIN,
      "0",
      path.join(dir, "daemon.log"),
      path.join(dir, "boot-token"),
      "skills1",
      "LWEVAL-1",
      dir,
      "reviewer",
      "reviewing",
    ]);
    expect({ code: boot.exitCode, stdout: boot.stdout.toString() }).toEqual({
      code: 2,
      stdout: "",
    });
    expect(boot.stderr.toString()).toBe(
      `daemon-standin: the daemon fact \`shapes\` of ${checkout} could not be read: the daemon source of ${checkout} is no shape of the completion refusal HANDOFF_SESSION_MISMATCH this rig reads: every daemon the rig runs holds "\\"HANDOFF_SESSION_MISMATCH\\"" in packages/daemon/internal/api/handoff.go, and this source does not\n`
    );
  },
  600_000
);

// Every move of the record writes the status its phase shows to the run's own Dispatch, as the
// daemon's outbox runs a `dispatch_status` row, under the daemon's own session
// (`legion-daemon:<PROJECT>`, the session the world's seeds write each status as). Catches: a
// record that moves while the issue on Dispatch keeps the status of the phase it left, so
// `dispatch read` and `read_record` disagree after a reviewer's request for changes
// (`needs_review` there, `in_progress` here); a write made under another identity than the
// daemon's, or for a move into a phase that shows the status the record already holds (an
// approval's move to `accepting`); a write over a status someone else gave the issue since the
// move (the outbox's row writes only while Dispatch still shows the status the move observed);
// a write the run's Dispatch did not take that is forgotten rather than owed; and a write
// Dispatch refused for good, of an issue it holds none of, owed for ever where the outbox
// finishes it refused (outbox.go's `permanentRefusal`), as this owed it.
test.skipIf(skip)(
  "a move of the record writes its phase's status to the run's Dispatch as the daemon's session, once, owes one Dispatch did not take, and finishes one Dispatch refused for good",
  async () => {
    const seeds = ["plan", "implement", "test"] as const;
    const pending = async (daemon: { body: () => Promise<unknown> }) =>
      z.looseObject({ pendingStatusWrites: z.array(z.unknown()) }).parse(await daemon.body())
        .pendingStatusWrites;
    const rejected = await standin("reviewer", "", undefined, { seeds });
    try {
      const world = rejected.seeded;
      pullRequest(rejected.dir, "BLOCKED").requestChanges();
      expect(await rejected.phase()).toBe("implementing");
      expect([world?.status, world?.statusWrites, await pending(rejected)]).toEqual([
        "in_progress",
        [{ status: "in_progress", actor: { kind: "session", id: "legion-daemon:LWEVAL" } }],
        [],
      ]);
      // A later read writes nothing more.
      expect(await rejected.phase()).toBe("implementing");
      expect(world?.statusWrites).toHaveLength(1);
    } finally {
      rejected.stop();
    }
    const approved = await standin("reviewer", "", undefined, { seeds });
    try {
      pullRequest(approved.dir).approve();
      expect(await approved.phase()).toBe("accepting");
      expect([approved.seeded?.status, approved.seeded?.statusWrites]).toEqual([
        "needs_review",
        [],
      ]);
    } finally {
      approved.stop();
    }
    // A person closed the issue between the review and the read that finds the round over: the
    // status they gave stands.
    const closed = await standin("reviewer", "", undefined, { seeds });
    try {
      pullRequest(closed.dir, "BLOCKED").requestChanges();
      if (closed.seeded !== undefined) closed.seeded.status = "done";
      expect(await closed.phase()).toBe("implementing");
      expect([closed.seeded?.status, closed.seeded?.statusWrites, await pending(closed)]).toEqual([
        "done",
        [],
        [],
      ]);
    } finally {
      closed.stop();
    }
    // A worker's own move back is answered once its status is on the run's Dispatch: no read
    // of the state comes between the move and what `dispatch read` shows.
    const moved = await standin("tester", "", undefined, { seeds: ["plan", "implement"] });
    try {
      await boots(moved.post);
      expect(await movesBack(moved.post, "implementing")).toBe(200);
      expect([moved.seeded?.status, moved.seeded?.statusWrites]).toEqual([
        "in_progress",
        [{ status: "in_progress", actor: { kind: "session", id: "legion-daemon:LWEVAL" } }],
      ]);
    } finally {
      moved.stop();
    }
    // The run's Dispatch does not answer: the move is taken and nothing is written. The state
    // of a run with seeds cannot be built without that Dispatch either (its gate), so the write
    // it owes is read below, in a run with none.
    const tester = await standin("tester", "", undefined, { seeds: ["plan", "implement"] });
    try {
      await boots(tester.post);
      tester.dispatchDown();
      expect(await movesBack(tester.post, "implementing")).toBe(200);
      expect(z.looseObject({ error: z.string() }).parse(await tester.body()).error).toStartWith(
        "the state could not be built: "
      );
      expect(tester.seeded?.statusWrites).toEqual([]);
    } finally {
      tester.stop();
    }
    // A run whose Dispatch holds no issue: Dispatch refuses the status's read for good, 404
    // ISSUE_NOT_FOUND, so the row is finished refused and owed no more.
    const unseeded = await standin("reviewer");
    try {
      pullRequest(unseeded.dir, "BLOCKED").requestChanges();
      expect(await unseeded.phase()).toBe("implementing");
      expect(await pending(unseeded)).toEqual([]);
    } finally {
      unseeded.stop();
    }
    // A run with no seeds reads no gate, so with its Dispatch down its state shows the write it
    // owes.
    const down = await standin("reviewer");
    try {
      pullRequest(down.dir, "BLOCKED").requestChanges();
      down.dispatchDown();
      expect(await down.phase()).toBe("implementing");
      expect(await pending(down)).toEqual([
        {
          issue: "LWEVAL-1",
          payload: { kind: "ordinary", status: "in_progress", observedStatus: "needs_review" },
          attempts: 2,
          nextAt: expect.any(String),
          lastError: expect.any(String),
        },
      ]);
    } finally {
      down.stop();
    }
  },
  600_000
);

// The second agent of a run reads the first's claim (rig.sh's run_threads_closed boots the
// reviewer on the run directory the implementer left, each with a stand-in of its own that
// appends to the run's one daemon.log). Catches: a stand-in that knows no agent but its own, so
// the reviewer's record names the implementer by the session a seed wrote its handoff as, which
// is not the session that pushed the fix the reviewer is reading, and gives the two agents'
// processes one generation.
test.skipIf(skip)(
  "the second agent of a run reads the claim the first left: its session, its turn, and the process before its own",
  async () => {
    const seeds = ["plan", "implement", "test", "review"] as const;
    const first = await standin("implementer", "reviewing", undefined, {
      completed: "implementer",
      seeds,
    });
    const fixer = "01a2d000-0000-7000-8000-00000000f007";
    try {
      await boots(first.post, fixer);
      expect(
        (await first.post("/legion/v1/claims/turn-outcome", turn("implementer", 1, "clean"))).status
      ).toBe(200);
    } finally {
      first.stop();
    }
    const at = (route: string) =>
      first.logged().find((line) => line.path === `/legion/v1/claims/${route}`)?.at;
    const second = await standin("reviewer", "reviewing", undefined, {
      completed: "implementer",
      seeds,
      dir: first.dir,
    });
    try {
      await boots(second.post);
      const { issue } = await served(second.body);
      expect(issue).toMatchObject({
        workers: {
          implementer: {
            claim: {
              session: fixer,
              state: "idle",
              locator: { sandbox: { generation: 6 } },
              lastTurnStart: at("ready"),
              lastReady: at("ready"),
              lastTurnEnd: at("turn-outcome"),
            },
            handoffRecord: seededRecord("implement"),
          },
          reviewer: {
            claim: { session: AGENT, state: "working", locator: { sandbox: { generation: 7 } } },
            handoffRecord: seededRecord("review"),
          },
        },
      });
      expect([at("ready"), at("turn-outcome")].map((time) => typeof time)).toEqual([
        "string",
        "string",
      ]);
    } finally {
      second.stop();
    }
  },
  600_000
);

// The daemon publishes the session holding each live role of the issue to Dispatch (`PUT
// /api/v1/issues/{key}/role-holders`, outbox.go's `publishRoleHolders`): at its boot, at a
// registration and at a claim it fails (the claim writes that change a role's holder, store's
// `putClaim`), and at a ready that carries no capability report (api's `ready`), each the whole
// live set — a claim registered, ready, working or idle that records a session — by role, as the
// daemon's own session. So the run's Dispatch holds the tree's architect, each role the world's
// history ran and each agent the run booted before this one, and the booted agent from its
// registration until its claim fails. Catches: a stand-in that publishes nothing (it published
// nothing), so a world's Dispatch holds no role production's does; one that publishes at no boot,
// or not at a registration, a boot's ready or a failure; one that publishes at a ready of the
// session's own, which changes no holder; and one that leaves out the history's idle claims or
// keeps a claim it failed.
test.skipIf(skip)(
  "the session holding each live role of the issue is published to the run's Dispatch as the daemon publishes it",
  async () => {
    const limit = countDefault(CHECKOUT, "launch_failure_limit");
    const seeds = ["plan", "implement", "test"] as const;
    const history = [
      { role: "architect", session: SEEDED_SESSIONS.architect },
      { role: "implementer", session: SEEDED_SESSIONS.implement },
      { role: "planner", session: SEEDED_SESSIONS.plan },
      { role: "tester", session: SEEDED_SESSIONS.test },
    ];
    const booted = [...history.slice(0, 3), { role: "reviewer", session: AGENT }, history[3]];
    const reviewer = await standin("reviewer", "", undefined, { seeds });
    try {
      expect(published(reviewer.writes)).toEqual([history]);
      await boots(reviewer.post);
      expect(published(reviewer.writes)).toEqual([history, booted, booted]);
      // A ready of the session's own carries its capability report and changes no holder.
      const own = await reviewer.post("/legion/v1/claims/ready", {
        secret: SECRET,
        capabilities: REPORT,
      });
      expect(own.status).toBe(204);
      expect(published(reviewer.writes)).toHaveLength(3);
      for (let sequence = 1; sequence <= limit; sequence += 1)
        await reviewer.post(
          "/legion/v1/claims/turn-outcome",
          turn("reviewer", sequence, "refused")
        );
      expect(published(reviewer.writes)).toEqual([history, booted, booted, history]);
    } finally {
      reviewer.stop();
    }
    // The second agent of a run: the first agent's claim, idle, holds its role, and the role of
    // the agent booting now is held by nobody until it registers, its claim launching.
    const first = await standin("implementer", "reviewing", undefined, {
      completed: "implementer",
      seeds: [...seeds, "review"],
    });
    const fixer = "01a2d000-0000-7000-8000-00000000f007";
    try {
      await boots(first.post, fixer);
      expect(published(first.writes).at(-1)).toEqual([
        history[0],
        { role: "implementer", session: fixer },
        history[2],
        { role: "reviewer", session: SEEDED_SESSIONS.review },
        history[3],
      ]);
    } finally {
      first.stop();
    }
    const second = await standin("reviewer", "reviewing", undefined, {
      completed: "implementer",
      seeds: [...seeds, "review"],
      dir: first.dir,
    });
    try {
      expect(published(second.writes)).toEqual([
        [history[0], { role: "implementer", session: fixer }, history[2], history[3]],
      ]);
    } finally {
      second.stop();
    }
  },
  600_000
);

// Catches: a design gate the record makes up — approved whatever the run's Dispatch says of the
// issue's spec, or one a write of the spec does not move — and a state read that answers a
// record of what the world last said, or of no gate, when the run's Dispatch cannot be read for
// the spec. Each approval is the one Dispatch's `GET /api/v1/artifacts/{id}` carries
// (dispatch/model's `ArtifactApproval`): approved at the latest version; `stale`, approved at an
// older one; a request for changes at the latest; and a spec nobody ever reviewed.
test.skipIf(skip)(
  "the design gate is the spec's approval as the run's Dispatch holds it now",
  async () => {
    const daemon = await standin("tester", "", undefined, { seeds: ["plan", "implement"] });
    const seeded = daemon.seeded;
    if (seeded === undefined) throw new Error("the stand-in was started with no seeds");
    const gate = async () => {
      const { designGate, gatedSteps } = z
        .looseObject({ designGate: z.unknown().optional(), gatedSteps: z.unknown() })
        .parse((await served(daemon.body)).issue);
      return { designGate, gatedSteps };
    };
    const spec = { step: "spec", artifactId: SEEDED_SPEC };
    try {
      seeded.approval = { state: "stale", latest_version: 2, version: 1 };
      expect(await gate()).toEqual({
        designGate: { artifactId: SEEDED_SPEC, currentVersion: 2, approvedVersion: 1 },
        gatedSteps: [{ ...spec, currentVersion: 2, approvedVersion: 1, status: "waiting" }],
      });
      seeded.approval = {
        state: "changes_requested",
        latest_version: 2,
        version: 2,
        reason: "say what R2 covers",
      };
      expect(await gate()).toEqual({
        designGate: { artifactId: SEEDED_SPEC, currentVersion: 2, approvedVersion: null },
        gatedSteps: [
          {
            ...spec,
            currentVersion: 2,
            approvedVersion: null,
            status: "changes_requested",
            changesReason: "say what R2 covers",
          },
        ],
      });
      // A request for changes at an older version than the spec stands at is none of this one
      // (the daemon's `ToldChangesVersion == LatestVersion`): the step waits, with no reason.
      seeded.approval = {
        state: "changes_requested",
        latest_version: 3,
        version: 2,
        reason: "say what R2 covers",
      };
      expect(await gate()).toEqual({
        designGate: { artifactId: SEEDED_SPEC, currentVersion: 3, approvedVersion: null },
        gatedSteps: [{ ...spec, currentVersion: 3, approvedVersion: null, status: "waiting" }],
      });
      seeded.approval = { state: "draft", latest_version: 1 };
      expect(await gate()).toEqual({
        designGate: { artifactId: SEEDED_SPEC, currentVersion: 1, approvedVersion: null },
        gatedSteps: [{ ...spec, currentVersion: 1, approvedVersion: null, status: "waiting" }],
      });
      // The run's Dispatch no longer answers for the spec: no record, and why.
      seeded.approval = undefined;
      daemon.dispatchDown();
      const unread = await fetch(`${daemon.url}/legion/v1/state`);
      expect(unread.status).toBe(500);
      expect(z.object({ error: z.string() }).parse(await unread.json()).error).toStartWith(
        "the state could not be built: "
      );
      expect(daemon.logged().at(-1)).toMatchObject({ path: "/legion/v1/state", status: 500 });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// A run with no seeds, which is every row above this section and no run of the rig. Catches: a
// record that makes up a history where the world seeded none (an architect, the rows of roles
// that never ran, a gate on no document); a claim that reads as working before its agent has
// said it can be prompted; and a pull request other than the one the run's world.json names,
// open at the head the run began at, with no merge state where the world holds no answers to
// read one from.
test.skipIf(skip)(
  "a run with no seeds holds its own claim alone, launching until it registers, and no history",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const launching = (await served(daemon.body)).issue;
      expect(Object.keys(launching.workers)).toEqual(["reviewer"]);
      expect(launching).toMatchObject({
        workers: { reviewer: { claim: { session: "", state: "launching" }, pass: 0, rounds: 0 } },
        pullRequest: { repo: WORLD_REPO, number: 1001, head: "0".repeat(40), state: "open" },
        gatedSteps: [{ step: "spec", approvedVersion: null, status: "unregistered" }],
        phaseEntry: 1,
        givenEntry: 1,
      });
      expect([
        "architect" in launching,
        "designGate" in launching,
        "mergeState" in (launching.pullRequest ?? {}),
      ]).toEqual([false, false, false]);
      expect(unsampled(launching)).toEqual(ALL_SAMPLED);
      // The tree's quiet time is the time since the daemon last saw it move, which is its own
      // start until a request of the claim's is taken, and the registration is one.
      const tree = (issue: object) =>
        z.looseObject({ treeMovedAt: z.string(), quietHours: z.number() }).parse(issue);
      await Bun.sleep(40);
      const quiet = tree((await served(daemon.body)).issue);
      expect(quiet.treeMovedAt).toBe(tree(launching).treeMovedAt);
      expect(quiet.quietHours).toBeGreaterThanOrEqual(30 / 3_600_000);
      expect(quiet.quietHours).toBeLessThanOrEqual(
        (Date.now() - Date.parse(quiet.treeMovedAt)) / 3_600_000
      );
      const registered = await daemon.post("/legion/v1/claims/register", {
        bootToken: BOOT_TOKEN,
        sessionId: AGENT,
        pluginContract: 144,
      });
      expect(registered.status).toBe(200);
      const { issue } = await served(daemon.body);
      expect(issue.workers.reviewer).toMatchObject({
        claim: { session: AGENT, state: "registered", pluginContract: 144 },
      });
      expect(Date.parse(tree(issue).treeMovedAt)).toBeGreaterThan(Date.parse(quiet.treeMovedAt));
    } finally {
      daemon.stop();
    }
  },
  600_000
);

/** The stand-in's boot on the run directory `dir`, a tester's, under `env`: its exit and what it
 * printed to stderr. Awaited rather than spawned synchronously, since the Dispatch a row gives
 * it is served by this process. */
async function bootOn(dir: string, env: Record<string, string>) {
  const child = Bun.spawn(
    [
      "bun",
      DAEMON_STANDIN,
      "0",
      path.join(dir, "daemon.log"),
      path.join(dir, "boot-token"),
      "skills1",
      "LWEVAL-1",
      dir,
      "tester",
    ],
    { stdout: "ignore", stderr: "pipe", env: { ...process.env, ...env } }
  );
  // A boot that is not refused listens until it is stopped, which is this row's failure and
  // not its wait.
  const timer = setTimeout(() => child.kill(), 120_000);
  try {
    const [code, err] = await Promise.all([child.exited, new Response(child.stderr).text()]);
    return { code, err };
  } finally {
    clearTimeout(timer);
  }
}

// Catches: a stand-in that boots on seeds it cannot read back — a record the run's Dispatch
// holds none of, or holds as another issue's, or a run that names no Dispatch at all — and
// serves a record of some other history: the rows of roles whose records are not there. Such a
// world is one the rig seeded wrongly, refused at the boot, naming the seeds and why, where no
// agent has started.
test.skipIf(skip)(
  "a stand-in whose run's seeds cannot be read back from its Dispatch refuses to boot, naming why",
  async () => {
    const dir = path.join(work, "unread-seeds");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "boot-token"), `${BOOT_TOKEN}\n`);
    writeFileSync(path.join(dir, "dispatch-token"), `${DISPATCH_TOKEN}\n`);
    writeFileSync(path.join(dir, "built-from"), `checkout ${CHECKOUT}\n`);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        branch: "legion/LWEVAL-1",
        pr: 1001,
        head: "0".repeat(40),
        repo: WORLD_REPO,
        base: WORLD_BASE,
      })
    );
    const fixture = path.join(dir, "fixture.json");
    writeFileSync(fixture, JSON.stringify({ issue: "LWEVAL-1", plan: seededRecord("plan") }));
    const refused = `daemon-standin: the history ${fixture} names could not be read back from the run's Dispatch: `;
    expect(await bootOn(dir, { DISPATCH_URL: "" })).toEqual({
      code: 2,
      err: `${refused}the run names no Dispatch (DISPATCH_URL)\n`,
    });
    const issue = {
      created_at: SEEDED_AT,
      created_by: { kind: "session", id: SEEDED_SESSIONS.architect },
      primary_artifact_id: SEEDED_SPEC,
    };
    const held = { id: "seed-plan", issue_key: "LWEVAL-1", phase: "plan", version: 1 };
    for (const [records, why] of [
      [
        {},
        'Dispatch answered GET /api/v1/handoffs/seed-plan 404: {"code":"HANDOFF_NOT_FOUND","error":"handoff seed-plan not found"}',
      ],
      [
        {
          "seed-plan": {
            ...held,
            issue_key: "OTHER-9",
            author: { kind: "session", id: SEEDED_SESSIONS.plan },
          },
        },
        `${fixture} names seed-plan as version 1 of LWEVAL-1's plan handoff, which the run's Dispatch holds as version 1 of OTHER-9's plan handoff`,
      ],
    ] as const) {
      const dispatch = dispatchOf(records, DISPATCH_TOKEN, {
        issue,
        status: "in_progress",
        statusWrites: [],
        approval: undefined,
      });
      try {
        expect(
          await bootOn(dir, {
            DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
            DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
          })
        ).toEqual({ code: 2, err: `${refused}${why}\n` });
      } finally {
        dispatch.stop(true);
      }
    }
  },
  600_000
);

/** A checkout holding a contract, `packages/contracts/src/legion-api.ts` with `source`, and the
 * daemon source the bodies a label is handed are built by: `packages/daemon/internal/api/state.go`
 * with `state` (this checkout's where none is given: daemon-pane.ts's `servesFixAttempts` and
 * `repositoriesLive`), and this checkout's api sources the contract its answers state is read
 * from beside it (`statesContract`, and shaping.go for `repositoriesLive`), with the list of
 * released contracts and the entry fixtures that number is counted from since #5399, under this
 * file's work directory. */
function contractCheckout(name: string, source: string, state?: string): string {
  const checkout = path.join(work, name);
  const dir = path.join(checkout, "packages", "contracts", "src");
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "legion-api.ts"), source);
  const api = path.join(checkout, "packages", "daemon", "internal", "api");
  mkdirSync(api, { recursive: true });
  const own = path.join(CHECKOUT, "packages", "daemon", "internal", "api");
  for (const file of ["server.go", "shaping.go", "version.go", "updatestate.go"])
    cpSync(path.join(own, file), path.join(api, file));
  writeFileSync(
    path.join(api, "state.go"),
    state ?? readFileSync(path.join(own, "state.go"), "utf8")
  );
  const released = path.join("packages", "contracts", "fixtures", "daemon-api");
  mkdirSync(path.join(checkout, released), { recursive: true });
  cpSync(
    path.join(CHECKOUT, released, "released-contracts.json"),
    path.join(checkout, released, "released-contracts.json")
  );
  // A tree that holds no entry has no directory of entry fixtures.
  if (existsSync(path.join(CHECKOUT, released, "contracts")))
    cpSync(path.join(CHECKOUT, released, "contracts"), path.join(checkout, released, "contracts"), {
      recursive: true,
    });
  return checkout;
}
/** A contract of a label whose daemon runs its claims in tmux panes alone: it describes a state
 * body unless a claim of it is located anywhere else, and says where, as zod names a path. */
const TMUX_ONLY_CONTRACT = `
const located = (issue) => [["architect", issue.architect], ...Object.entries(issue.workers).map(([role, row]) => ["workers." + role + ".claim", row.claim])];
export const LegionStateResponse = {
  safeParse(body) {
    for (const [key, issue] of Object.entries(body.issues))
      for (const [at, claim] of located(issue))
        if (claim?.locator !== undefined && claim.locator.runtime !== "tmux")
          return { success: false, error: { message: "issues." + key + "." + at + ".locator.runtime: Invalid input: expected \\"tmux\\"" } };
    return { success: true };
  },
};
`;

// Catches: a label started on a contract that does not describe the state its stand-in serves,
// which that label's agents meet in their own turns, every `read_record` of theirs refused `a
// body the daemon API contract does not describe`, while the other label's read theirs: the base
// arm of 2026-11-09 measured a world of its own so (30 runs). The check hands every kind of
// body the stand-in can serve (standin-record.ts's `sampleFacts`) to the schema of the label's
// own checkout and names the first it refuses with the schema's own sentence; a checkout whose
// contract cannot be read, or exports no schema, is refused too, naming the file. This
// checkout's own contract describes them all.
test("a label's own contract is handed every state body the stand-in serves, and a refusal names the body and the error", async () => {
  expect(await contractRefusal(CHECKOUT)).toBeUndefined();
  const refusing = contractCheckout("tmux-only-contract", TMUX_ONLY_CONTRACT);
  const contract = path.join(refusing, "packages", "contracts", "src", "legion-api.ts");
  const refusal = `${contract} does not describe the state body the stand-in serves (planning, pull request 0): issues.WID-1.architect.locator.runtime: Invalid input: expected "tmux"`;
  expect(await contractRefusal(refusing)).toBe(refusal);
  expect(
    await contractRefusal(
      contractCheckout(
        "any-contract",
        "export const LegionStateResponse = { safeParse: () => ({ success: true }) };\n"
      )
    )
  ).toBeUndefined();
  const schemaless = contractCheckout("schemaless-contract", "export const Other = 1;\n");
  expect(await contractRefusal(schemaless)).toBe(
    `${path.join(schemaless, "packages", "contracts", "src", "legion-api.ts")} exports no LegionStateResponse to read a state body with`
  );
  const missing = path.join(work, "no-contract");
  expect(await contractRefusal(missing)).toStartWith(
    `${path.join(missing, "packages", "contracts", "src", "legion-api.ts")} could not be loaded: `
  );
  // As rig.sh's services_up runs it for each label of a batch: the refusal names the label.
  const program = path.join(import.meta.dir, "standin-record.ts");
  const refused = spawnChecked(["bun", program, "contract", "base", refusing]);
  expect([refused.exitCode, refused.stderr.toString()]).toEqual([
    1,
    `standin-record contract: label base: ${refusal}\n`,
  ]);
  const described = spawnChecked(["bun", program, "contract", "head", CHECKOUT]);
  expect([described.exitCode, described.stderr.toString()]).toEqual([0, ""]);
  const usage = spawnChecked(["bun", program, "contract", "head"]);
  expect([usage.exitCode, usage.stderr.toString()]).toEqual([
    2,
    "usage: bun standin-record.ts contract <label> <checkout>\n",
  ]);
}, 120_000);

// The same check against the base label's own checkout, where the host names one
// (SKILL_SCENARIOS_BASE_CHECKOUT, the row "the state body is described by the base label's own
// contract" above says what it is): every kind of body, where that row hands the label's schema
// the one body a reviewer's stand-in serves at its boot.
test.skipIf(baseCheckout === undefined)(
  "the base label's own contract describes every state body the stand-in serves",
  async () => {
    expect(await contractRefusal(baseCheckout ?? "")).toBeUndefined();
  },
  120_000
);

/** A contract that requires a pull request's count of fix attempts, as main `ff2a10ef`'s does
 * (its legion-api.ts's `fixAttempts`), and one that refuses the count, as no contract does: a
 * reading of what the bodies a label is handed carry. */
const COUNTING_CONTRACT = `
export const LegionStateResponse = {
  safeParse(body) {
    for (const [key, issue] of Object.entries(body.issues))
      if (issue.pullRequest !== undefined && typeof issue.pullRequest.fixAttempts !== "number")
        return { success: false, error: { message: "issues." + key + ".pullRequest.fixAttempts: Invalid input: expected number, received undefined" } };
    return { success: true };
  },
};
`;
const COUNTLESS_CONTRACT = `
export const LegionStateResponse = {
  safeParse(body) {
    for (const [key, issue] of Object.entries(body.issues))
      if (issue.pullRequest !== undefined && "fixAttempts" in issue.pullRequest)
        return { success: false, error: { message: "issues." + key + ".pullRequest: Unrecognized key: fixAttempts" } };
    return { success: true };
  },
};
`;

// Catches: a label of main `ff2a10ef`'s shape handed bodies with no count of fix attempts, which
// its contract requires, so the check refused the label whose shape the stand-in reads (it did:
// `standin-record contract` of an extract of ff2a10ef refused `fixAttempts`, which contract 59
// removed on 2026-11-09); one whose daemon keeps no count, main's since contract 59, handed bodies
// carrying one; and a daemon source of neither shape read as either, rather than refused naming
// its marker. The bodies are the ones the stand-in serves the label (daemon-standin.ts's
// `FIX_ATTEMPTS`), read out of its own source.
test("a label's own contract is handed the count of fix attempts where its daemon serves one, and none where it serves none", async () => {
  const state = readFileSync(
    path.join(CHECKOUT, "packages", "daemon", "internal", "api", "state.go"),
    "utf8"
  );
  const counted = state.replace(COUNTLESS_VIEW, COUNTED_VIEW);
  expect(counted).not.toBe(state);
  const counting = contractCheckout("counting-contract", COUNTING_CONTRACT, counted);
  expect(await contractRefusal(counting)).toBeUndefined();
  const uncounted = contractCheckout("counting-contract-uncounted", COUNTING_CONTRACT);
  expect(await contractRefusal(uncounted)).toBe(
    `${path.join(uncounted, "packages", "contracts", "src", "legion-api.ts")} does not describe the state body the stand-in serves (planning, pull request 0): issues.WID-1.pullRequest.fixAttempts: Invalid input: expected number, received undefined`
  );
  expect(await contractRefusal(contractCheckout("countless-contract", COUNTLESS_CONTRACT))).toBe(
    undefined
  );
  const overcounted = contractCheckout("countless-contract-counted", COUNTLESS_CONTRACT, counted);
  expect(await contractRefusal(overcounted)).toBe(
    `${path.join(overcounted, "packages", "contracts", "src", "legion-api.ts")} does not describe the state body the stand-in serves (planning, pull request 0): issues.WID-1.pullRequest: Unrecognized key: fixAttempts`
  );
  const unread = contractCheckout("counting-contract-unread", COUNTING_CONTRACT, "package api\n");
  expect(await contractRefusal(unread)).toStartWith(
    `the daemon source of ${unread} is no shape of the state's pull request this rig reads: `
  );
  if (olderHost !== undefined) expect(await contractRefusal(olderHost)).toBeUndefined();
}, 120_000);

/** A contract that requires the repositories block of main's daemon since #4493, its `live`
 * list, and one that requires the project's mapping before it, its `mapped` list: a reading of
 * which block the bodies a label is handed carry. */
const LIVE_CONTRACT = `
export const LegionStateResponse = {
  safeParse(body) {
    return Array.isArray(body.repositories.live)
      ? { success: true }
      : { success: false, error: { message: "repositories.live: Invalid input: expected array, received undefined" } };
  },
};
`;
const MAPPING_CONTRACT = `
export const LegionStateResponse = {
  safeParse(body) {
    return Array.isArray(body.repositories.mapped)
      ? { success: true }
      : { success: false, error: { message: "repositories.mapped: Invalid input: expected array, received undefined" } };
  },
};
`;

// Catches: a label handed bodies whose repositories block is another daemon's than its own: the
// mapping for a label of main's daemon since #4493, this checkout, or the live records' block for
// one before it, read off this build rather than off the label's source (daemon-pane.ts's
// `repositoriesLive`); and a daemon source of neither shape read as either, rather than refused
// naming its markers.
test("a label's own contract is handed the repositories block its daemon serves, the live records' since #4493 and the mapping before it", async () => {
  const state = readFileSync(
    path.join(CHECKOUT, "packages", "daemon", "internal", "api", "state.go"),
    "utf8"
  );
  const mapped = state.replace(LIVE_FIELD, MAPPED_FIELDS);
  expect(mapped).not.toBe(state);
  const refused = (checkout: string, list: string) =>
    `${path.join(checkout, "packages", "contracts", "src", "legion-api.ts")} does not describe the state body the stand-in serves (planning, pull request 0): repositories.${list}: Invalid input: expected array, received undefined`;
  expect(await contractRefusal(contractCheckout("live-contract", LIVE_CONTRACT))).toBeUndefined();
  const liveOnMapping = contractCheckout("live-contract-mapped", LIVE_CONTRACT, mapped);
  expect(await contractRefusal(liveOnMapping)).toBe(refused(liveOnMapping, "live"));
  expect(
    await contractRefusal(contractCheckout("mapping-contract-mapped", MAPPING_CONTRACT, mapped))
  ).toBeUndefined();
  const mappingOnLive = contractCheckout("mapping-contract", MAPPING_CONTRACT);
  expect(await contractRefusal(mappingOnLive)).toBe(refused(mappingOnLive, "mapped"));
  const unread = contractCheckout(
    "live-contract-unread",
    LIVE_CONTRACT,
    state.replace(LIVE_FIELD, LIVE_FIELD.replace('json:"live"', 'json:"-"'))
  );
  expect(await contractRefusal(unread)).toStartWith(
    `the daemon source of ${unread} is no shape of the state's repositories block this rig reads: `
  );
}, 120_000);
