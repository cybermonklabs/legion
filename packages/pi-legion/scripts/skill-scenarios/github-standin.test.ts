import { afterAll, beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { z } from "zod";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import { GITHUB_ROUTES, requestSites, UNREACHED } from "./github-routes";
import { GITHUB_LOG, GITHUB_NOT_KEPT, githubRefusal, HEADS_NOT_KEPT, headUnmoved } from "./health";
import { absentFields, restOperation } from "./rest-required";
import { SCENARIOS, workerWorld } from "./run-names";
import { spawnChecked } from "./test-programs";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default (the provenance row took 5.1 s beside a second suite, 2026-11-19), so each gets a
// minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

// The run's Dispatch server reads GitHub through its App to say where a result's commit stands,
// and a world whose GitHub was port 1 told every reviewer a pass stands "not known", Dispatch
// "cannot read the repository", where production answers `no`, that commit being none of the
// compared commits (INFRA-764, 2026-11-16). rig.sh's start_dispatch now starts github-standin.ts
// for each run and points the server's GitHub at it. Its routes are the ones rig.sh's own flows
// were measured to read (github-routes.ts's GITHUB_ROUTES); health.ts's rule 18 reads its log.
const STANDIN = path.join(import.meta.dir, "github-standin.ts");
const RIG = path.join(import.meta.dir, "rig.sh");
const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
const REPO = "fernbrook/widgets";
const AUTHOR = "widgets-implementer[bot]";
const made: string[] = [];
afterAll(() => {
  for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
});
function freshDir(name: string): string {
  const dir = mkdtempSync(path.join(tmpdir(), `${name}-`));
  made.push(dir);
  return dir;
}
/** `git -C dir args…` under the identity its `-c user.*` names: the operator's own
 * GIT_AUTHOR_* and GIT_COMMITTER_*, which a Legion pod sets to its App and git reads ahead of any
 * `-c`, are left out, so the world's commits carry the author the world names. */
function git(dir: string, ...args: string[]): string {
  const own = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !/^GIT_(AUTHOR|COMMITTER)_/.test(name))
  );
  const ran = spawnChecked(
    [
      "git",
      "-c",
      "user.name=t",
      "-c",
      "user.email=t@example.invalid",
      "-c",
      "commit.gpgsign=false",
      "-C",
      dir,
      ...args,
    ],
    { env: { ...own, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (ran.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString().trim();
}
/** Each request line of a stand-in log as a call: its method, its path with every SHA and the
 * installation's id as the template names them, and its media type where it is not GitHub's
 * JSON. The `listening` line is no call. */
function callsOf(log: string): string[] {
  const calls = log
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, string>)
    .filter((line) => line.method !== undefined)
    .map((line) => {
      const asked = `${line.method} ${line.path}`
        .replaceAll(/[0-9a-f]{40}/g, "{sha}")
        .replace(/installations\/\d+/, "installations/{id}")
        .replace(/pulls\/\d+$/, "pulls/{number}")
        .replace(`/repos/${REPO}`, "/repos/{owner}/{repo}");
      return line.accept === "application/vnd.github+json" ? asked : `${asked} (${line.accept})`;
    });
  return [...new Set(calls)].sort();
}

/** A run directory holding a world: a remote whose default branch has one commit and whose issue
 * branch two above it, made by the implementer's App (AUTHOR), which opened the pull request, the
 * world rig.sh's fixture_write writes for it, and the gh answers the stand-in reads the
 * repository and the pull request's title and body from. */
function world() {
  const dir = freshDir("github-world");
  const src = path.join(dir, "src");
  mkdirSync(src);
  git(src, "init", "-q", "-b", "main");
  writeFileSync(path.join(src, "README.md"), "# widgets\n");
  git(src, "add", "-A");
  git(src, "commit", "-qm", "init");
  const base = git(src, "rev-parse", "HEAD");
  git(src, "checkout", "-qb", "legion/WID-1");
  const implementer = ["-c", `user.name=${AUTHOR}`, "-c", `user.email=1+${AUTHOR}@example.invalid`];
  writeFileSync(path.join(src, "greet.ts"), "export const greet = () => 1;\n");
  git(src, "add", "-A");
  git(src, ...implementer, "commit", "-qm", "feat: greet()");
  const first = git(src, "rev-parse", "HEAD");
  writeFileSync(path.join(src, "greet.ts"), "export const greet = () => 2;\n");
  git(src, ...implementer, "commit", "-qam", "fix: greet()");
  const second = git(src, "rev-parse", "HEAD");
  git(dir, "clone", "-q", "--bare", src, "remote.git");
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({
      key: "WID-1",
      repo: REPO,
      pr: 1000,
      branch: "legion/WID-1",
      base: "main",
      head: first,
    })
  );
  const repository = {
    id: 1,
    node_id: "R_kgDOAAAAAQ",
    name: "widgets",
    full_name: REPO,
    owner: { login: "fernbrook", type: "Organization" },
    default_branch: "main",
  };
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      routes: [
        {
          match: "^gh pr view",
          stdout: { number: 1000, id: "PR_kwDOAAAAAc4AAA1000", title: "feat: greet()", body: "b" },
        },
        { match: "^gh api (.* )?/?repos/fernbrook/widgets([?][^ ]*)?( |$)", stdout: repository },
      ],
    })
  );
  return { dir, src, base, first, second };
}

/** The stand-in serving `dir`, its origin, and its log so far. */
async function serving(dir: string) {
  const child = Bun.spawn(["bun", STANDIN, dir, REPO, "main"], {
    stdout: "pipe",
    stderr: "pipe",
  });
  const reader = child.stdout.getReader();
  let log = "";
  while (!/"listening":"127\.0\.0\.1:\d+"/.test(log)) {
    const { value, done } = await reader.read();
    if (done) throw new Error(`the stand-in exited: ${await new Response(child.stderr).text()}`);
    log += new TextDecoder().decode(value);
  }
  const port = /"listening":"127\.0\.0\.1:(\d+)"/.exec(log)?.[1];
  const drain = (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      log += new TextDecoder().decode(value);
    }
  })();
  return {
    origin: `http://127.0.0.1:${port}`,
    log: () => log,
    stop: async () => {
      child.kill();
      await drain;
    },
  };
}

/** A JWT's shape, three base64url parts, as Dispatch signs its App's. */
const JWT = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJJdjEuZTJlZmFrZSJ9.c2lnbmVk";

// Catches: a route answered for another repository or installation; an installation-token route
// answered without a token the stand-in minted; a pull request whose head is not the remote's tip
// at the call, before or after a push (the head the world reports, in gh's answers and Dispatch's
// alike); a commit's pull requests that do not follow the remote; a commit read in a shape other
// than the one measured; and a route the world was never measured to read answered quietly rather
// than refused, logged with what was asked.
test("the stand-in answers the measured routes from the run's remote, and refuses any other naming it", async () => {
  const { dir, src, base, first, second } = world();
  const github = await serving(dir);
  const zero = "0".repeat(40);
  try {
    const get = (route: string, token?: string, accept = "application/vnd.github+json") =>
      fetch(`${github.origin}${route}`, {
        headers: {
          Accept: accept,
          ...(token === undefined ? {} : { Authorization: `Bearer ${token}` }),
        },
      });
    const installation = await get(`/repos/${REPO}/installation`, JWT);
    expect(installation.status).toBe(200);
    const { id } = (await installation.json()) as { id: number };
    expect((await get("/repos/someone/else/installation", JWT)).status).toBe(404);
    expect((await get(`/repos/${REPO}/installation`)).status).toBe(401);
    const mint = (installationId: number) =>
      fetch(`${github.origin}/app/installations/${installationId}/access_tokens`, {
        method: "POST",
        headers: { Authorization: `Bearer ${JWT}` },
      });
    expect((await mint(id + 1)).status).toBe(404);
    const minted = await mint(id);
    expect(minted.status).toBe(201);
    const { token } = (await minted.json()) as { token: string };
    expect(token).toMatch(/^ghs_[0-9a-f]{36}$/);
    expect((await get(`/repos/${REPO}`, "ghs_not-minted")).status).toBe(401);
    const repository = (await (await get(`/repos/${REPO}`, token)).json()) as {
      default_branch: string;
    };
    expect(repository.default_branch).toBe("main");
    expect((await get("/repos/someone/else", token)).status).toBe(404);
    type Pull = {
      number: number;
      user: { login: string };
      head: { sha: string };
      base: { sha: string };
    };
    const pulls = async (sha: string) => {
      const answered = await get(`/repos/${REPO}/commits/${sha}/pulls`, token);
      const body = (await answered.json()) as Pull[] | { message: string };
      return [
        answered.status,
        Array.isArray(body)
          ? body.map((pull) => [pull.number, pull.head.sha, pull.base.sha])
          : body.message,
      ];
    };
    // A commit of the issue branch is the open pull request's, whose head is the branch's tip; a
    // commit the default branch holds was brought by no pull request of this world.
    expect(await pulls(first)).toEqual([200, [[1000, second, base]]]);
    expect(await pulls(second)).toEqual([200, [[1000, second, base]]]);
    expect(await pulls(base)).toEqual([200, []]);
    expect(await pulls(zero)).toEqual([422, `No commit found for SHA: ${zero}`]);
    const pull = async () => {
      const answered = await get(`/repos/${REPO}/pulls/1000`, token);
      const body = (await answered.json()) as Pull & { commits: number; merged: boolean };
      return [answered.status, body.head.sha, body.user.login, body.commits, body.merged];
    };
    expect(await pull()).toEqual([200, second, AUTHOR, 2, false]);
    expect((await get(`/repos/${REPO}/pulls/1001`, token)).status).toBe(404);
    // A push moves the head the world reports.
    writeFileSync(path.join(src, "greet.ts"), "export const greet = () => 3;\n");
    git(src, "commit", "-qam", "fix: greet() again");
    git(src, "push", "-q", path.join(dir, "remote.git"), "legion/WID-1");
    const third = git(src, "rev-parse", "HEAD");
    expect(await pull()).toEqual([200, third, AUTHOR, 3, false]);
    expect(await pulls(first)).toEqual([200, [[1000, third, base]]]);
    const commit = await get(
      `/repos/${REPO}/commits/${first}`,
      token,
      "application/vnd.github.sha"
    );
    expect([commit.status, await commit.text()]).toEqual([200, first]);
    const absent = await get(`/repos/${REPO}/commits/${zero}`, token, "application/vnd.github.sha");
    expect([absent.status, ((await absent.json()) as { message: string }).message]).toEqual([
      422,
      `No commit found for SHA: ${zero}`,
    ]);
    expect((await get(`/repos/${REPO}/commits/${first}`, token)).status).toBe(501);
    expect((await get(`/repos/${REPO}/pulls/1000/commits`, token)).status).toBe(501);
  } finally {
    await github.stop();
  }
  const lines = github
    .log()
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
  expect(lines[0]).toMatchObject({ listening: expect.stringMatching(/^127\.0\.0\.1:\d+$/) });
  expect(lines.filter((line) => line.refused !== undefined)).toEqual([
    {
      at: expect.any(String),
      method: "GET",
      path: `/repos/${REPO}/commits/${first}`,
      accept: "application/vnd.github+json",
      status: 501,
      refused: "no route of the world answers it",
    },
    {
      at: expect.any(String),
      method: "GET",
      path: `/repos/${REPO}/pulls/1000/commits`,
      accept: "application/vnd.github+json",
      status: 501,
      refused: "no route of the world answers it",
    },
  ]);
  expect(lines.find((line) => line.path === `/repos/${REPO}/installation`)).toMatchObject({
    status: 200,
    route: "GET /repos/{owner}/{repo}/installation",
  });
  // Every success is GitHub's own status for its route (github-routes.ts's `status`, as the
  // tester measured GitHub on 2026-11-16 at 16:29Z): 200 on every read, 201 on the token's mint,
  // and every route of the table answered so at least once.
  expect(GITHUB_ROUTES.map((row) => `${row.route} ${row.status}`)).toEqual([
    "GET /repos/{owner}/{repo}/installation 200",
    "POST /app/installations/{id}/access_tokens 201",
    "GET /repos/{owner}/{repo}/commits/{sha} 200",
    "GET /repos/{owner}/{repo} 200",
    "GET /repos/{owner}/{repo}/commits/{sha}/pulls 200",
    "GET /repos/{owner}/{repo}/pulls/{number} 200",
  ]);
  const succeeded = lines.filter((line) => typeof line.status === "number" && line.status < 400);
  expect(new Set(succeeded.map((line) => `${line.route} ${line.status}`))).toEqual(
    new Set(GITHUB_ROUTES.map((row) => `${row.route} ${row.status}`))
  );
}, 60_000);

// Catches: the pull request Dispatch reads saying another merge state or mergeability than gh
// does, or one frozen into the world: its base's rules decide both (gh-threads.ts's
// `mergeStateOf`, `mergeableOf`), so a base requiring one approval holds it `blocked` until the
// approval stands, and GitHub's REST `mergeable` is true in both (INFRA-764's ruling of
// 2026-11-18; measured that day, a `BLOCKED` pull request of cybermonklabs/legion read
// `MERGEABLE`).
test("the pull request Dispatch reads carries the merge state and mergeability its base's rules leave it in", async () => {
  const { dir, second } = world();
  const file = path.join(dir, "fixtures.json");
  const fixtures = JSON.parse(readFileSync(file, "utf8"));
  const reviews = {
    match: "^gh api .*pulls/1000/reviews",
    methods: ["GET"],
    stdout: [] as object[],
  };
  fixtures.routes.unshift(
    {
      match: "^gh api .*/rules/branches/main",
      methods: ["GET"],
      stdout: [{ type: "pull_request", parameters: { required_approving_review_count: 1 } }],
    },
    reviews
  );
  writeFileSync(file, JSON.stringify(fixtures));
  const github = await serving(dir);
  try {
    const installation = await fetch(`${github.origin}/repos/${REPO}/installation`, {
      headers: { Accept: "application/vnd.github+json", Authorization: `Bearer ${JWT}` },
    });
    const { id } = (await installation.json()) as { id: number };
    const minted = await fetch(`${github.origin}/app/installations/${id}/access_tokens`, {
      method: "POST",
      headers: { Authorization: `Bearer ${JWT}` },
    });
    const { token } = (await minted.json()) as { token: string };
    const pull = async () => {
      const answered = await fetch(`${github.origin}/repos/${REPO}/pulls/1000`, {
        headers: { Accept: "application/vnd.github+json", Authorization: `Bearer ${token}` },
      });
      const body = (await answered.json()) as { mergeable: unknown; mergeable_state: unknown };
      return [answered.status, body.mergeable, body.mergeable_state];
    };
    expect(await pull()).toEqual([200, true, "blocked"]);
    reviews.stdout = [{ state: "APPROVED", commit_id: second, user: { login: "a-reviewer" } }];
    writeFileSync(file, JSON.stringify(fixtures));
    expect(await pull()).toEqual([200, true, "clean"]);
  } finally {
    await github.stop();
  }
}, 60_000);

// Catches: an answer of this stand-in short of a field GitHub's description requires of its
// route's operation — rest-required.json, the extract of github/rest-api-description at
// c9c95fe34ced that the gh stand-in's routes are held to too (gh-standin.test.ts) — read as
// INFRA-764's read search reads one: an installation with no `app_id` or `access_tokens_url` and
// its account a login alone, a repository short of its URLs and times, and a pull request short
// of its `_links`, its user or its counts, in the listing of a commit's pull requests and by its
// number alike; and a route of GITHUB_ROUTES this row makes no call of.
test("every route the HTTP stand-in serves answers every field GitHub's description requires", async () => {
  const { dir, first } = world();
  const github = await serving(dir);
  try {
    const ask = async (method: string, route: string, token: string, accept?: string) => {
      const answered = await fetch(`${github.origin}${route}`, {
        method,
        headers: {
          Accept: accept ?? "application/vnd.github+json",
          Authorization: `Bearer ${token}`,
        },
      });
      const text = await answered.text();
      return [answered.status, accept === undefined ? JSON.parse(text) : text] as const;
    };
    const [, installation] = await ask("GET", `/repos/${REPO}/installation`, JWT);
    const minted = await ask("POST", `/app/installations/${installation.id}/access_tokens`, JWT);
    const token = String(minted[1].token);
    const answers: Record<string, readonly [number, unknown]> = {
      "GET /repos/{owner}/{repo}/installation": [200, installation],
      "POST /app/installations/{id}/access_tokens": minted,
      "GET /repos/{owner}/{repo}/commits/{sha}": await ask(
        "GET",
        `/repos/${REPO}/commits/${first}`,
        token,
        "application/vnd.github.sha"
      ),
      "GET /repos/{owner}/{repo}": await ask("GET", `/repos/${REPO}`, token),
      "GET /repos/{owner}/{repo}/commits/{sha}/pulls": await ask(
        "GET",
        `/repos/${REPO}/commits/${first}/pulls`,
        token
      ),
      "GET /repos/{owner}/{repo}/pulls/{number}": await ask(
        "GET",
        `/repos/${REPO}/pulls/1000`,
        token
      ),
    };
    expect(
      GITHUB_ROUTES.map(({ route }) => {
        const operation = restOperation(route);
        const [answered, answer] = answers[route] ?? [0, undefined];
        return [
          route,
          answered,
          operation?.schema === undefined ? "no operation" : absentFields(answer, operation.schema),
        ];
      })
    ).toEqual(GITHUB_ROUTES.map(({ route, status }) => [route, status, []]));
    // The listing names the pull request it lists, which is no empty list's absence of fields.
    expect(answers["GET /repos/{owner}/{repo}/commits/{sha}/pulls"]?.[1]).toHaveLength(1);
  } finally {
    await github.stop();
  }
}, 60_000);

// Catches: a read Dispatch's code adds later, which a batch would meet as a refusal and no test
// would name; a row of either table whose code is gone; and a site claimed twice.
test("every place Dispatch builds a GitHub request is a route the stand-in serves or one no world reaches, with why", () => {
  const sites = requestSites(path.join(CHECKOUT, "packages", "envoy", "internal", "dispatch"));
  const claimed = [...GITHUB_ROUTES.map((row) => row.site), ...UNREACHED.map((row) => row.site)];
  expect(new Set(claimed).size).toBe(claimed.length);
  expect(claimed.toSorted()).toEqual(sites);
  expect(new Set(GITHUB_ROUTES.map((row) => `${row.route} ${row.accept ?? ""}`)).size).toBe(
    GITHUB_ROUTES.length
  );
  for (const row of [...GITHUB_ROUTES, ...UNREACHED])
    expect(Object.values(row).every((value) => value !== "")).toBe(true);
});

// Catches: a run whose provenance does not name the program its GitHub came from.
test("each run's provenance names the GitHub stand-in and its routes, shared, from the rig's own directory", () => {
  const fn = scriptFunctions(RIG);
  const result = spawnChecked(
    [
      "bash",
      "-c",
      [
        ...["given_services", "safe_url", "file_digest", "system_gitconfig", "services_from"].map(
          fn
        ),
        "pinned_omp() { echo /x/omp; }",
        `here=${import.meta.dir} co=/co pin=omp@pin I=/w/installs/0a1b2c3d4e5f profile=legion`,
        "nats_url=nats://127.0.0.1:4222 pg_url=postgres://127.0.0.1:5432/postgres",
        "services_from /l /d /p",
      ].join("\n"),
    ],
    {
      env: {
        ...process.env,
        SKILL_SCENARIOS_POSTGRES_URL: "postgres://127.0.0.1:5432/postgres",
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
      },
    }
  );
  expect(result.exitCode).toBe(0);
  const digest = (file: string) =>
    `sha256:${new Bun.CryptoHasher("sha256").update(readFileSync(path.join(import.meta.dir, file))).digest("hex")}`;
  for (const program of ["github-standin.ts", "github-routes.ts"])
    expect(result.stdout.toString()).toContain(
      `\n${program} shared ${import.meta.dir} ${digest(program)}\n`
    );
});

describe("health's rule 18 over the stand-in's log", () => {
  /** A run directory of `scenario` whose stand-in log holds `log`, or none where it is undefined. */
  const run = (scenario: string, log?: string) => {
    const dir = freshDir("github-health");
    if (log !== undefined) {
      mkdirSync(path.join(dir, "logs"));
      writeFileSync(path.join(dir, GITHUB_LOG), log);
    }
    return { dir, scenario };
  };
  const listening = '{"at":"2026-11-16T15:00:00.000Z","listening":"127.0.0.1:41234"}\n';
  const asked = (status: number, extra: Record<string, unknown>) =>
    `${JSON.stringify({
      at: "2026-11-16T15:00:01.000Z",
      method: "GET",
      path: "/repos/fernbrook/widgets/pulls/1000",
      accept: "application/vnd.github+json",
      status,
      ...extra,
    })}\n`;
  const served = asked(200, { route: "GET /repos/{owner}/{repo}/pulls/{number}" });
  // The worker worlds, whose seeds make their Dispatch ask GitHub, and every other scenario, as
  // the rig's own scenario table names them (run-names.ts's SCENARIOS).
  const WORKER = Object.keys(SCENARIOS).filter(workerWorld);
  const OTHER = Object.keys(SCENARIOS).filter((scenario) => !workerWorld(scenario));

  // Catches: a scenario rig.sh builds a worker world for (its run_<scenario> calls
  // worker_run_world, which seeds results and starts the head watcher), or whose seeds were
  // measured asking GitHub (github-routes.ts), left out of the worker worlds rules 18 and 19 hold
  // to a record, or one rig.sh builds no such world for put in.
  test("the worker worlds of the rig's scenario table are the scenarios rig.sh builds a worker world for", () => {
    const fn = scriptFunctions(RIG);
    const built = Object.keys(SCENARIOS).filter((scenario) => {
      try {
        return fn(`run_${scenario.replaceAll("-", "_")}`).includes("worker_run_world");
      } catch {
        return false;
      }
    });
    expect(WORKER).toEqual(built);
    expect(WORKER).toEqual(["tester-proof", "reviewer-text-findings", "threads-closed"]);
  });

  // Catches absence read as presence: a worker world, whose Dispatch asks GitHub at its seeds,
  // with no record of what it asked, read as one that was refused nothing; and the other
  // direction, a world that starts no stand-in held to one. Each absence is a row of its own,
  // with health's own reason pinned word for word here: the tester's reader, which the fixture
  // below holds health to, is compared with it by verdict alone, and words its reasons its own way.
  test("a worker world whose log is missing is unhealthy, and any other world with none is healthy", () => {
    for (const scenario of WORKER)
      expect(githubRefusal(run(scenario))).toBe(
        "github: logs/github.log is missing, so the run kept no record of what its Dispatch asked GitHub"
      );
    expect(GITHUB_NOT_KEPT.missing).toBe(githubRefusal(run("threads-closed")) ?? "");
    for (const scenario of OTHER) expect(githubRefusal(run(scenario))).toBeUndefined();
  });
  test("a log that is empty is unhealthy in any world", () => {
    for (const scenario of [...WORKER, ...OTHER])
      expect(githubRefusal(run(scenario, ""))).toBe(
        "github: logs/github.log is empty, so the run kept no record of what its Dispatch asked GitHub"
      );
  });
  test("a log that does not open with the stand-in's listening line is unhealthy in any world", () => {
    for (const scenario of [...WORKER, ...OTHER])
      expect(githubRefusal(run(scenario, served))).toBe(
        "github: logs/github.log does not open with the stand-in's listening line, so it is no record of the run's GitHub"
      );
  });
  test("a worker world's log holding the listening line and no request is unhealthy, and any other world's is healthy", () => {
    for (const scenario of WORKER)
      expect(githubRefusal(run(scenario, listening))).toBe(
        "github: logs/github.log holds the stand-in's listening line and no request, so the run's Dispatch never asked GitHub what its world's seeds make it ask"
      );
    for (const scenario of OTHER) expect(githubRefusal(run(scenario, listening))).toBeUndefined();
    expect(githubRefusal(run("reviewer-text-findings", listening + served))).toBeUndefined();
  });

  // Catches: the stand-in started twice in one run, whose second Dispatch's requests the first
  // stand-in never saw.
  test("a second listening line is unhealthy", () => {
    expect(githubRefusal(run("reviewer-text-findings", listening + served + listening))).toBe(
      "github: logs/github.log:3 is a second listening line, so the stand-in started twice in one run"
    );
  });

  // Catches: a request answered that is no route of the table, whatever the line says its route
  // was, read as one of the routes the world was measured to ask for.
  test("a request outside GITHUB_ROUTES is unhealthy, whatever route its line names", () => {
    const outside = asked(200, {
      path: "/repos/fernbrook/widgets/branches/main",
      route: "GET /repos/{owner}/{repo}/branches/{branch}",
    });
    expect(githubRefusal(run("reviewer-text-findings", listening + outside))).toBe(
      "github: logs/github.log:2, GET /repos/fernbrook/widgets/branches/main (Accept: application/vnd.github+json) is no route of GITHUB_ROUTES (github-routes.ts), whatever route the line names"
    );
    const named = asked(200, {
      path: "/repos/fernbrook/widgets/pulls/1000/commits",
      route: "GET /repos/{owner}/{repo}/pulls/{number}",
    });
    expect(githubRefusal(run("reviewer-text-findings", listening + named))).toBe(
      "github: logs/github.log:2, GET /repos/fernbrook/widgets/pulls/1000/commits (Accept: application/vnd.github+json) is no route of GITHUB_ROUTES (github-routes.ts), whatever route the line names"
    );
  });

  // Catches: a success GitHub never gives for its route, the token's mint at 200 or a read at
  // 201, read as GitHub's answer.
  test("a success other than GitHub's own status for its route is unhealthy", () => {
    const mint = asked(200, {
      method: "POST",
      path: "/app/installations/1/access_tokens",
      route: "POST /app/installations/{id}/access_tokens",
    });
    expect(githubRefusal(run("reviewer-text-findings", listening + mint))).toBe(
      "github: logs/github.log:2, POST /app/installations/1/access_tokens (Accept: application/vnd.github+json) answered 200 on POST /app/installations/{id}/access_tokens, where GitHub answers 201"
    );
    const read = asked(201, { route: "GET /repos/{owner}/{repo}/pulls/{number}" });
    expect(githubRefusal(run("reviewer-text-findings", listening + read))).toBe(
      "github: logs/github.log:2, GET /repos/fernbrook/widgets/pulls/1000 (Accept: application/vnd.github+json) answered 201 on GET /repos/{owner}/{repo}/pulls/{number}, where GitHub answers 200"
    );
  });

  // Catches: a route the stand-in refused, which changed what Dispatch told an agent whether or not
  // an agent asked for it; GitHub's own answer declared in one world read as declared in another;
  // and a line no stand-in wrote, its own failure.
  test("a refusal undeclared for the run's scenario makes it unhealthy, naming the route and the scenario", () => {
    const refused = asked(501, { refused: "no route of the world answers it" });
    expect(githubRefusal(run("threads-closed", listening + served + refused))).toBe(
      "github refusal: logs/github.log:3, GET /repos/fernbrook/widgets/pulls/1000 (Accept: application/vnd.github+json) answered 501: no route of the world answers it, which no declaration of threads-closed can take, since GitHub answers every route"
    );
    const missing = asked(404, { route: "GET /repos/{owner}/{repo}/pulls/{number}" });
    const declared = {
      "reviewer-text-findings": [
        { route: "GET /repos/{owner}/{repo}/pulls/{number}", status: 404, reason: "a planted row" },
      ],
    };
    expect(
      githubRefusal(run("reviewer-text-findings", listening + missing), declared)
    ).toBeUndefined();
    expect(githubRefusal(run("threads-closed", listening + missing), declared)).toBe(
      "github refusal: logs/github.log:2, GET /repos/fernbrook/widgets/pulls/1000 (Accept: application/vnd.github+json) answered 404 on GET /repos/{owner}/{repo}/pulls/{number}, which threads-closed does not declare (health.ts GITHUB_DECLARED)"
    );
    expect(githubRefusal(run("threads-closed", listening + missing))).toContain(
      "which threads-closed does not declare"
    );
    expect(
      githubRefusal(run("threads-closed", `${listening}error: the stand-in fell over\n`))
    ).toBe(
      "github: logs/github.log:2 is no line the stand-in writes: error: the stand-in fell over"
    );
  });
});

// The tester's fixture of 2026-11-16, made by its builder, which this tree keeps beside the row
// that reads it (github-head-fixture/build-github-head-fixture.py): 32 cases, 01 to 18 rule 18's
// and 19 to 32 rule 19's, each a directory holding only what the two rules read, with the verdict
// and the reason the tester's own reader (its world-health.py, which stays outside this tree) gives
// it in its manifest.json. Each row builds the fixture afresh, so no copy of it is kept to fall out
// of step with its builder. Where the two readers must agree, this row is the pin: every case
// reads here with the tester's verdict but three, each a difference of design the architect ruled
// on. 01 and 19 are runs of a rig from before the stand-in and the watcher (their services-from
// names neither): the tester keys a record's being due on the run's own files, and these rules on
// the rig's scenario table, so a worker world without its record is unhealthy whatever rig it ran
// on. 15 is the tester's own refusal of a log read with a rig that has no route table, which has
// no meaning for a reader that is the rig's. The two readers agree on verdicts alone: the tester's
// reader words each reason its own way, so health's reason for each case is pinned here, against
// health's own text, and the tester's reason in the manifest is compared with nothing.
describe("the tester's GitHub and head fixture", () => {
  const DIFFER = ["01-older-rig-no-log", "15-log-norig-table", "19-no-watcher-rig"];
  const line = (n: number, commit: string) =>
    `head: pushes.log:${n} pushed ${commit} to refs/heads/legion/WID-1, and `;
  /** Health's reason for each case, the fixture's two commits read from what its build made: the
   * head its world was seeded at and the one its pushes moved the branch to. */
  const reasonsOf = (
    seeded: string,
    pushed: string
  ): Readonly<Record<string, string | undefined>> => ({
    "01-older-rig-no-log": GITHUB_NOT_KEPT.missing,
    "02-no-log": GITHUB_NOT_KEPT.missing,
    "03-no-dispatch-no-log": undefined,
    "04-empty": GITHUB_NOT_KEPT.empty,
    "05-unopened": GITHUB_NOT_KEPT.unopened,
    "06-not-json": "github: logs/github.log:3 is no line the stand-in writes: panic: boom",
    "07-refused":
      "github refusal: logs/github.log:2, GET /repos/fernbrook/widgets/branches/main (Accept: application/vnd.github.sha) answered 501: no route of the world answers it, which no declaration of reviewer-text-findings can take, since GitHub answers every route",
    "08-status-422":
      "github refusal: logs/github.log:2, GET /repos/fernbrook/widgets/commits/0123456789abcdef0123456789abcdef01234567 (Accept: application/vnd.github.sha) answered 422 on GET /repos/{owner}/{repo}/commits/{sha}, which reviewer-text-findings does not declare (health.ts GITHUB_DECLARED)",
    "09-status-404":
      "github refusal: logs/github.log:2, GET /repos/fernbrook/widgets/pulls/9 (Accept: application/vnd.github+json) answered 404 on GET /repos/{owner}/{repo}/pulls/{number}, which reviewer-text-findings does not declare (health.ts GITHUB_DECLARED)",
    "10-route-outside-table":
      "github: logs/github.log:2, GET /repos/fernbrook/widgets/branches/main (Accept: application/vnd.github.sha) is no route of GITHUB_ROUTES (github-routes.ts), whatever route the line names",
    "11-mint-200":
      "github: logs/github.log:2, POST /app/installations/1/access_tokens (Accept: application/vnd.github+json) answered 200 on POST /app/installations/{id}/access_tokens, where GitHub answers 201",
    "12-read-201":
      "github: logs/github.log:2, GET /repos/fernbrook/widgets/commits/54ff56ea0137d652dec48b5b202afcea40a48581 (Accept: application/vnd.github.sha) answered 201 on GET /repos/{owner}/{repo}/commits/{sha}, where GitHub answers 200",
    "13-clean": undefined,
    "14-second-listening":
      "github: logs/github.log:3 is a second listening line, so the stand-in started twice in one run",
    "15-log-norig-table": undefined,
    "16-no-request-world": GITHUB_NOT_KEPT.unasked,
    "17-no-request-no-world": undefined,
    "18-no-status":
      'github: logs/github.log:2 is no line the stand-in writes: {"at": "2026-11-16T17:29:08.200Z", "method": "GET", "path": "/repos/fernbrook/widgets/commits/54ff56ea0137d652dec48b5b202afcea40a48581", "accept": "application/vnd.github.sha", "route": "GET /repos/{o',
    "19-no-watcher-rig": HEADS_NOT_KEPT.missing,
    "20-no-world": undefined,
    "21-no-heads-log": HEADS_NOT_KEPT.missing,
    "22-empty-heads-log": HEADS_NOT_KEPT.empty,
    "23-unopened-heads-log": HEADS_NOT_KEPT.unopened,
    "24-no-push": undefined,
    "25-push-stored": undefined,
    "26-push-no-line": `${line(1, pushed)}logs/heads.log holds no line storing it, so the run's Dispatch went on comparing results with the head before it`,
    "27-push-failed": `${line(1, pushed)}the head watcher could not store it: connection refused`,
    "28-push-failed-then-stored": undefined,
    "29-remote-moved-unlogged": `head: remote.git's refs/heads/legion/WID-1 stands at ${pushed}, not world.json's seeded head ${seeded}, and logs/heads.log holds no line storing it, so a push the record missed left the run's Dispatch comparing results with the head before it`,
    "30-push-other-branch": undefined,
    "31-heads-not-json": "head: logs/heads.log:2 is no line the head watcher writes: garbage",
    "32-no-pushes-log": undefined,
  });
  const BUILDER = path.join(import.meta.dir, "github-head-fixture", "build-github-head-fixture.py");
  /** The fixture the builder makes under `env`, and its manifest.json. python3 runs the builder,
   * and a host without one fails the row rather than skip it: a fixture nobody builds is a reader
   * nobody checks. */
  const built = (env: Record<string, string | undefined> = process.env) => {
    const fixture = path.join(freshDir("github-head-fixture"), "fixture-github-head");
    const ran = spawnChecked(["python3", BUILDER, fixture], { env });
    expect([ran.exitCode, ran.stderr.toString()]).toEqual([0, ""]);
    const text = readFileSync(path.join(fixture, "manifest.json"), "utf8");
    const manifest = JSON.parse(text) as {
      seeded_head: string;
      pushed_head: string;
      cases: { case: string; rule: "github" | "head"; scenario: string; verdict: string }[];
    };
    return { fixture, text, manifest };
  };

  test("every case reads as the tester read it, but the three the architect ruled on, each with its reason", () => {
    const { fixture, manifest } = built();
    const reasons = reasonsOf(manifest.seeded_head, manifest.pushed_head);
    expect(manifest.cases.map((one) => one.case)).toEqual(Object.keys(reasons));
    const differ: string[] = [];
    for (const one of manifest.cases) {
      const read = { dir: path.join(fixture, one.case), scenario: one.scenario };
      const reason = one.rule === "github" ? githubRefusal(read) : headUnmoved(read);
      expect([one.case, reason]).toEqual([one.case, reasons[one.case]]);
      if ((reason === undefined ? "healthy" : "unhealthy") !== one.verdict) differ.push(one.case);
    }
    expect(differ).toEqual(DIFFER);
  }, 60_000);

  // Catches: a builder that commits under whatever identity its environment carries. git reads
  // GIT_AUTHOR_*, GIT_COMMITTER_* and the GIT_CONFIG_COUNT pairs over a caller's -c settings, and
  // a Legion pod exports all of them, so the tester's build of 2026-11-16 committed as the pod's
  // App and another machine's build made other commits. This builds the fixture under a pod's
  // identity and under an environment with no GIT_* at all, and requires one fixture of both. And
  // a fixture that names the directory it was built in, as the tester's archive named its scratch
  // in the origin url of each of its 13 bare remotes: a path of the machine that built it, which
  // is no part of any world.
  test("the builder makes one fixture under any environment its host carries, naming no directory of the build", () => {
    const pod = {
      ...process.env,
      GIT_AUTHOR_NAME: "an-app[bot]",
      GIT_AUTHOR_EMAIL: "1+an-app[bot]@users.noreply.github.com",
      GIT_COMMITTER_NAME: "an-app[bot]",
      GIT_COMMITTER_EMAIL: "1+an-app[bot]@users.noreply.github.com",
      GIT_CONFIG_COUNT: "1",
      GIT_CONFIG_KEY_0: "user.name",
      GIT_CONFIG_VALUE_0: "another-app[bot]",
    };
    const bare = Object.fromEntries(
      Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))
    );
    const fromPod = built(pod);
    const fromBare = built(bare);
    expect(fromPod.text).toBe(fromBare.text);
    for (const { fixture } of [fromPod, fromBare]) {
      const root = path.dirname(fixture);
      const naming = readdirSync(fixture, { recursive: true, encoding: "utf8" }).filter((file) => {
        const full = path.join(fixture, file);
        return statSync(full).isFile() && readFileSync(full).includes(root);
      });
      expect([root, naming]).toEqual([root, []]);
    }
  }, 60_000);
});

// The agent's side, on the world as rig.sh builds and seeds it: rig.sh itself, up to its command
// dispatch, runs a run of each world with its agents stubbed, on a Postgres of the caller's
// (LEGION_TEST_PG_DSN, whose login must create a database, as a batch's Postgres does; the rows
// skip where it is unset) and a Dispatch server built from this checkout.
//
// Each run's database is named with this suite's own prefix, DATABASE_PREFIX, and never rig.sh's
// `run_`: a batch's runner refuses to start while its Postgres holds a `run_` database its own
// work directory does not name, and on a Legion pod LEGION_TEST_PG_DSN names the Postgres the
// tester's batch runs on. So own_dispatch is given that prefix here, and every database a row
// names is dropped twice over: by its run's own exit trap once its Dispatch has stopped, and by
// the afterAll below whatever ended the row.
const pg = process.env.LEGION_TEST_PG_DSN;
const DATABASE_PREFIX = "github_pin_";
const databases: string[] = [];
let server = "";
beforeAll(() => {
  if (pg === undefined) return;
  server = path.join(freshDir("dispatch-build"), "dispatch-e2e");
  const built = spawnChecked(["go", "build", "-o", server, "./cmd/dispatch"], {
    cwd: path.join(CHECKOUT, "packages", "envoy"),
  });
  if (built.exitCode !== 0) throw new Error(`go build ./cmd/dispatch: ${built.stderr.toString()}`);
}, 600_000);
afterAll(async () => {
  if (pg === undefined || databases.length === 0) return;
  const sql = new Bun.SQL({ url: pg, max: 1 });
  try {
    for (const database of databases.splice(0))
      await sql.unsafe(`DROP DATABASE IF EXISTS "${database}" WITH (FORCE)`);
  } finally {
    await sql.close();
  }
});

/** A run of `call` (a run_<scenario> call of rig.sh) in a world of its own, its agents stubbed,
 * then `after`, every line of both with rig.sh's functions; the scratch it ran in, holding
 * github.log, and its exit. Its exit trap stops every process the run started, whether the row
 * passes or fails, as stop_run stops a run's: the head watcher (start_heads, with TERM, as
 * stop_run stops it), a daemon stand-in an `after` started in the background as `standin`, the
 * run's Dispatch and its GitHub. It stopped the last two alone, and every run of a worker world
 * left its head watcher running past the row, its scratch deleted under it. */
function worldRun(call: string, after: string[] = []) {
  const scratch = freshDir("github-world-run");
  mkdirSync(path.join(scratch, "tmp"));
  const rig = readFileSync(RIG, "utf8");
  const prelude = path.join(scratch, "rig-functions.sh");
  writeFileSync(
    prelude,
    rig
      .slice(0, [...rig.matchAll(/\ncase "\$\{1:-\}" in/g)].at(-1)?.index)
      .replace(/^here=.*$/m, `here='${import.meta.dir}'`)
  );
  const script = [
    `source '${prelude}'`,
    `co='${CHECKOUT}'`,
    'mkdir -p "$(build_dir "$co")" "$work/services" "$tokens" "$SCRATCH/home" "$SCRATCH/state"',
    'cp "$SERVER" "$(build_dir "$co")/dispatch-e2e"',
    `printf '%s\\n' "$PG" >"$services_postgres"`,
    '(umask 077 && openssl rand -hex 24 >"$tokens/dispatch-token")',
    // A run named afresh, as cmd_run names one, whose database own_dispatch names with this
    // suite's prefix (DATABASE_PREFIX, above) rather than rig.sh's `run_`.
    "R=$work/runs/$RUN_ID",
    `eval "$(declare -f own_dispatch | sed "s/database=run_/database=$DATABASE_PREFIX/")"`,
    'declare -f own_dispatch | grep -q "database=$DATABASE_PREFIX" || { echo "own_dispatch did not take the suite database prefix" >&2; exit 3; }',
    "evidence=$R",
    'mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs"',
    // The label's built-from, as cmd_run copies it into the run: the checkout whose daemon the
    // run's seeds and stand-ins write as.
    `printf 'checkout %s\\n' "$co" >"$R/built-from"`,
    "worker_boot() { :; }",
    "start_recorder() { :; }",
    "launch() { :; }",
    "base_env() { :; }",
    "standins() { :; }",
    // A worker run's end judges what its agents wrote (judge.ts's `judgeWorker`), and these
    // agents are stubbed: they wrote nothing, and no transcript names their session.
    "judge_ts() { :; }",
    `trap 'stop_pid "\${watcher_pid:-}" >/dev/null 2>&1; stop_tree "\${standin:-}" >/dev/null 2>&1; stop_tree "\${dispatch_pid:-}" >/dev/null 2>&1; stop_tree "\${github_pid:-}" >/dev/null 2>&1; cp "$R/logs/github.log" "$SCRATCH/github.log"; pg_sql "$PG" "DROP DATABASE IF EXISTS $DATABASE_PREFIX$RUN_ID WITH (FORCE)" >/dev/null 2>&1 || true' EXIT`,
    "results() {",
    '  env -i PATH="$PATH" HOME="$SCRATCH/home" DISPATCH_STATE_DIR="$SCRATCH/state" \\',
    '    DISPATCH_URL="http://127.0.0.1:$dispatch_port" DISPATCH_TOKEN_FILE="$tokens/dispatch-token" \\',
    "    DISPATCH_HOST=omp DISPATCH_SESSION_ID=results-pin \\",
    `    bun '${CHECKOUT}/packages/envoy-client/bin/dispatch.ts' results --issue "$worker_key" >"$SCRATCH/$1.txt" 2>&1`,
    "}",
    call,
    ...after,
  ].join("\n");
  // The run's id and so its database's name, known before it starts, so the afterAll drops it
  // whatever ends the run.
  const id = Array.from(crypto.getRandomValues(new Uint8Array(6)), (byte) =>
    byte.toString(16).padStart(2, "0")
  ).join("");
  databases.push(`${DATABASE_PREFIX}${id}`);
  const ran = spawnChecked(["bash", "-c", script], {
    env: {
      ...process.env,
      PG: pg ?? "",
      SERVER: server,
      SCRATCH: scratch,
      RUN_ID: id,
      DATABASE_PREFIX,
      SKILL_SCENARIOS_WORK: path.join(scratch, "work"),
      TMPDIR: path.join(scratch, "tmp"),
    },
  });
  return { scratch, code: ran.exitCode, stderr: ran.stderr.toString() };
}

// Catches: a world whose Dispatch cannot read its repository (GitHub at port 1, or a stand-in that
// refuses the default-branch read), which answers "not known" where production answers that the
// commit is none of the compared commits; a read of the world's GitHub the measurement did not
// see; and a run of it rule 18 reads as unhealthy.
test.skipIf(pg === undefined)(
  "a reviewer reads a result one push below the head as production answers it, and nothing reads not known",
  () => {
    const { scratch, code, stderr } = worldRun(
      "run_reviewer_text_findings pin proj 0 0 text-only",
      [
        "results at-head",
        'jq -r .head "$R/world.json" >"$SCRATCH/seeded"',
        'git clone -q "$R/remote.git" "$SCRATCH/later"',
        'git -C "$SCRATCH/later" checkout -q "legion/$worker_key"',
        'printf "A later line.\\n" >"$SCRATCH/later/LATER.md"',
        'git -C "$SCRATCH/later" add LATER.md',
        'git -C "$SCRATCH/later" -c user.name=Later -c user.email=later@example.invalid -c commit.gpgsign=false commit -qm "docs: a later line"',
        'git -C "$SCRATCH/later" push -q origin "HEAD:legion/$worker_key"',
        'seed_ts pull-request "$R/fixture.json" "$R/fixtures.json" "$R/remote.git"',
        "results one-below",
      ]
    );
    expect({ code, stderr }).toMatchObject({ code: 0 });
    const read = (name: string) => readFileSync(path.join(scratch, name), "utf8");
    const seeded = read("seeded").trim();
    const atHead = read("at-head.txt");
    const below = read("one-below.txt");
    expect(atHead).toContain("A pass stands: yes");
    expect(below).toContain("A pass stands: no.");
    // Each result, at the head the run was seeded with, is now one push below the head.
    expect(
      below.match(
        new RegExp(
          `pass by [a-z]+ at ${seeded}\\. That commit is none of the compared commits\\.`,
          "g"
        )
      )
    ).toHaveLength(2);
    for (const answer of [atHead, below]) {
      expect(answer).not.toContain("not known");
      expect(answer).not.toContain("cannot read the repository");
    }
    expect(callsOf(read("github.log"))).toEqual([
      "GET /repos/{owner}/{repo}",
      "GET /repos/{owner}/{repo}/commits/{sha} (application/vnd.github.sha)",
      "GET /repos/{owner}/{repo}/commits/{sha}/pulls",
      "GET /repos/{owner}/{repo}/installation",
      "POST /app/installations/{id}/access_tokens",
    ]);
    mkdirSync(path.join(scratch, "logs"));
    writeFileSync(path.join(scratch, GITHUB_LOG), read("github.log"));
    expect(githubRefusal({ dir: scratch, scenario: "reviewer-text-findings" })).toBeUndefined();
  },
  600_000
);

// Catches: a plain-session world whose Dispatch starts asking GitHub, which changes what its
// agent is told and which no other row would see. Each was measured asking nothing.
test.skipIf(pg === undefined)(
  "the plain-session worlds ask GitHub nothing",
  () => {
    for (const call of [
      "run_ask_on_message pin",
      "run_measure_before_ask pin",
      "run_brainstorm pin",
      "run_askgate pin facts",
    ]) {
      const { scratch, code, stderr } = worldRun(call);
      expect({ call, code, stderr }).toMatchObject({ call, code: 0 });
      const log = readFileSync(path.join(scratch, "github.log"), "utf8");
      expect([call, log.split("\n").filter((line) => line !== "").length, callsOf(log)]).toEqual([
        call,
        1,
        [],
      ]);
    }
  },
  600_000
);

// The record of each run of a step on a world's own Dispatch (main's since #4400), written as the
// label's daemon writes it — the history's runs by the seeds (seed.ts's `steps`), the open one
// and a move's end and next start by the daemon stand-in — and read back with the label's own
// `dispatch steps` and the route's own GET, both the run's own Dispatch server's answer. Catches:
// a world whose `dispatch steps` lists other runs than its handoffs and its record show (none at
// all, which it listed), a run listed twice where the seeds and the stand-in derived two ids for
// it, and a move whose end or next start the label's daemon writes and the world does not.
test.skipIf(pg === undefined)(
  "a world's dispatch steps lists its history and its open step, and after a move the ended step and the next",
  () => {
    const agent = "01a2d000-0000-7000-8000-00000000f006";
    // The run's boot token and the secret the stand-in's registration answers: random, as the
    // daemon's are, since the plugin holds them (`@legion/contracts/held-secrets`).
    const bootToken = fixtureSecret("boot");
    const secret = fixtureSecret("reg");
    const { scratch, code, stderr } = worldRun("run_tester_proof pin proj 0 0", [
      "steps() {",
      '  env -i PATH="$PATH" HOME="$SCRATCH/home" DISPATCH_STATE_DIR="$SCRATCH/state" \\',
      '    DISPATCH_URL="http://127.0.0.1:$dispatch_port" DISPATCH_TOKEN_FILE="$tokens/dispatch-token" \\',
      "    DISPATCH_HOST=omp DISPATCH_SESSION_ID=steps-pin \\",
      `    bun '${CHECKOUT}/packages/envoy-client/bin/dispatch.ts' steps --issue "$worker_key" >"$SCRATCH/$1.txt" 2>&1`,
      `  printf 'Authorization: Bearer %s' "$(cat "$tokens/dispatch-token")" | curl -fsS -H @- \\`,
      '    "http://127.0.0.1:$dispatch_port/api/v1/issues/$worker_key/steps" >"$SCRATCH/$1.json"',
      "}",
      `printf "${bootToken}\\n" >"$SCRATCH/boot-token"`,
      `SKILL_SCENARIOS_CLAIM_SECRET='${secret}' DISPATCH_URL="http://127.0.0.1:$dispatch_port" DISPATCH_TOKEN_FILE="$tokens/dispatch-token" \\`,
      '  bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$SCRATCH/boot-token" proj "$worker_key" "$R" tester >"$SCRATCH/standin.out" 2>&1 &',
      "standin=$!",
      'for _ in $(seq 1200); do grep -q "listening on" "$SCRATCH/standin.out" && break; sleep 0.1; done',
      'url=$(sed -n "s/^listening on //p" "$SCRATCH/standin.out")',
      "steps before",
      `printf '%s' '${JSON.stringify({ bootToken, sessionId: agent, pluginContract: 146 })}' | curl -fsS -X POST "$url/legion/v1/claims/register" --data @- >/dev/null`,
      `printf '%s' '{"secret":"${secret}"}' | curl -fsS -X POST "$url/legion/v1/claims/ready" --data @- >/dev/null`,
      `grant=$(printf '%s' "{\\"sessionId\\":\\"${agent}\\",\\"secret\\":\\"${secret}\\",\\"tree\\":\\"$worker_key\\",\\"issue\\":\\"$worker_key\\"}" | curl -fsS -X POST "$url/legion/v1/grants" --data @- | jq -r .grantId)`,
      'curl -fsS -X POST "$url/legion/v1/phase/backward" -d "{\\"grantId\\":\\"$grant\\",\\"to\\":\\"implementing\\",\\"reason\\":\\"the fix needs code\\"}" >/dev/null',
      "steps after",
      'kill "$standin"',
    ]);
    expect({ code, stderr }).toMatchObject({ code: 0 });
    const read = (name: string) => readFileSync(path.join(scratch, name), "utf8");
    const at = "\\d{4}-\\d\\d-\\d\\dT[0-9:.]+Z";
    const line = (step: string, role: string, type: string, attempt: number, end: string) =>
      new RegExp(
        `^default ${step} step ${role} ${type} attempt ${attempt} legion-proj-wid-\\d+-${role} - ${at} ${end}$`
      );
    const before = read("before.txt").trimEnd().split("\n");
    expect(before[0]).toMatch(/^WID-\d+: 3 steps in 1 tasks$/);
    expect(before.slice(1).map((one, index) => [index, one])).toEqual([
      [0, expect.stringMatching(line("1", "planner", "planning", 1, `${at} passed`))],
      [1, expect.stringMatching(line("2", "implementer", "implementation", 1, `${at} passed`))],
      [2, expect.stringMatching(line("3", "tester", "testing", 1, "running running"))],
    ]);
    const after = read("after.txt").trimEnd().split("\n");
    expect(after[0]).toMatch(/^WID-\d+: 4 steps in 1 tasks$/);
    expect(after.slice(1).map((one, index) => [index, one])).toEqual([
      [0, expect.stringMatching(line("1", "planner", "planning", 1, `${at} passed`))],
      [1, expect.stringMatching(line("2", "implementer", "implementation", 1, `${at} passed`))],
      [2, expect.stringMatching(line("3", "tester", "testing", 1, `${at} failed`))],
      [3, expect.stringMatching(line("2", "implementer", "implementation", 2, "running running"))],
    ]);
    // What the listing does not print, read off the route's own answer: the move's end names the
    // step the work returns to and the session of the agent that ran it, and each history run
    // its handoff record and the session that wrote it.
    const steps = z
      .object({
        tasks: z.array(
          z.object({
            steps: z.array(
              z.looseObject({
                step: z.number(),
                attempt: z.number(),
                session: z.string(),
                returned_to: z.number().nullable(),
                report: z.string().nullable(),
              })
            ),
          })
        ),
      })
      .parse(JSON.parse(read("after.json")))
      .tasks.flatMap((task) => task.steps);
    expect(
      steps.map(({ step, attempt, session, returned_to, report }) => [
        step,
        attempt,
        session === agent ? "agent" : session === "" ? "" : "seeded",
        returned_to,
        report === null ? null : "record",
      ])
    ).toEqual([
      [1, 1, "seeded", null, "record"],
      [2, 1, "seeded", null, "record"],
      [3, 1, "agent", 2, null],
      [2, 2, "", null, null],
    ]);
  },
  600_000
);

/** A process this row recorded: its pid and the start time `/proc/<pid>/stat` gives it (field
 * 22, in clock ticks since boot), read after the name in parentheses, which can hold spaces. */
interface Recorded {
  readonly pid: number;
  readonly started: string;
}
function startTime(stat: string): string {
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19] ?? "";
}
/** Whether the process `recorded` names still runs: its pid names a process that started when it
 * did. A pid another process has taken since names no recorded process. */
function running(recorded: Recorded): boolean {
  try {
    return startTime(readFileSync(`/proc/${recorded.pid}/stat`, "utf8")) === recorded.started;
  } catch {
    return false;
  }
}

// Catches: a world run whose exit leaves the head watcher it started running (start_heads, which
// every worker world's worker_run_world calls). worldRun's exit trap stopped the run's Dispatch
// and GitHub alone, and after a suite six watchers of `github-world-run` scratches stood up to
// two hours old, one under a scratch already deleted. The watcher is read by the pid the run
// recorded and the start time /proc gave it then, never by its name, and a watcher this row finds
// running it stops itself, whether the row passes or fails.
test.skipIf(pg === undefined)(
  "a world run's exit leaves no head watcher of that run running",
  () => {
    const { scratch, code, stderr } = worldRun("run_tester_proof pin proj 0 0", [
      'printf "%s\\n" "$watcher_pid" >"$SCRATCH/watcher-pid"',
      'cat "/proc/$watcher_pid/stat" >"$SCRATCH/watcher-stat"',
    ]);
    const files = ["watcher-pid", "watcher-stat"].map((name) => path.join(scratch, name));
    const watcher: Recorded | undefined = files.every((file) => existsSync(file))
      ? {
          pid: Number(readFileSync(files[0] ?? "", "utf8").trim()),
          started: startTime(readFileSync(files[1] ?? "", "utf8")),
        }
      : undefined;
    try {
      expect({ code, stderr }).toMatchObject({ code: 0 });
      expect(watcher?.pid).toBeGreaterThan(1);
      expect(watcher?.started).toMatch(/^\d+$/);
      expect(watcher !== undefined && running(watcher) ? "running" : "stopped").toBe("stopped");
    } finally {
      if (watcher !== undefined && running(watcher)) process.kill(watcher.pid, "SIGKILL");
    }
  },
  600_000
);
