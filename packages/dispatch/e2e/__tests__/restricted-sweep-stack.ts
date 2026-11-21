/** `bun e2e/__tests__/restricted-sweep-stack.ts <head | base | keyed>`, from packages/dispatch: one
 *  stack of the e2e harness (`e2e/harness-pool.ts`, whose Dispatch server `e2e/run-server.sh`
 *  starts), seeded through its own routes, and `e2e/acceptance/restricted-sweep.sh` run against
 *  it as bob, a restricted member whom OWN's access list names and TGT's does not. It prints one
 *  JSON line, what each run answered, and stops the stack however it ends.
 *  `restricted-sweep.test.ts` runs it.
 *
 *  - `head` serves this checkout's build. It sweeps TGT, whose objects the seed below holds, and
 *    EMPTY, a project with no issue, which the full member finds nothing of; it splits the sweep
 *    of TGT into fills, a writer and a runner; and it reads the server's own route probes (`go run
 *    ./internal/dispatch/api/cmd/routeprobes`), which the test holds the sweep's kinds to.
 *  - `base` serves a build with no read check (`noReadCheckBase`: the merge base of this checkout
 *    and the branch the read check's pull request targets, or the commit before the one that
 *    added the check), and sweeps TGT once and split. That build has no read check, so bob reads
 *    every project.
 *  - `keyed` serves this checkout's build too, and sweeps a target project keyed as the route
 *    index's example issue key.
 *
 *  Every write goes through the server's routes as a person or an agent makes it, but three the
 *  stack has no route for: bob's member kind, which the outside sign-in writes and this stack does
 *  not have; a merged pull request of TGT's repository, which the delivery reconcile writes from
 *  GitHub; and the project bob's own session worked in, which the event log records only for a
 *  session a broker-issued token proved, and this stack's agents hold the shared bearer. Each is
 *  written as its writer writes it (`restricted-member.e2e.ts` and `acceptance/project-access.sh`
 *  write the first two, `events.queueSessionProject` the third). */

import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { startStacks, stopStacks } from "../harness-pool";
import { sqlAt } from "../psql";
import { dispatchPackage, readCheckFile, sweepScript } from "./restricted-sweep-index";

/** The migrations that give a build restricted members, which write `member_kind`. */
const migrations = "packages/envoy/internal/dispatch/store/migrations/*.up.sql";

const bob = "bob@outside.example";
const carol = "carol@example.com";
/** The agent bearer `e2e/run-server.sh` pins. */
const sharedToken = "e2e-token";
/** The session the seed's agent writes name, and the one the fake Envoy lists: a session uuid, as
 *  Envoy names one, which the live socket's agent view of it reads. */
export const worker = "5e55107a-0c0a-4f00-8b2c-7a26e7000001";
/** bob's own session, in OWN, which the live check's control message goes to. */
const ownSession = "ses-own-worker";
/** How long one sweep may run before it is stopped: a run against the local stack takes about 40 s
 *  with no pacing and a 5 s window. */
const sweepMs = 600_000;

const repositoryRoot = fileURLToPath(new URL("../../../..", import.meta.url));

/** What one sweep answered. */
export interface SweepRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** The split, as the outside acceptance runs it: fills as the full member, then a runner with the
 *  member's cookie alone beside a writer, which is stopped once the runner ends. A runner and a
 *  writer run only where the fills were written. */
export interface SplitRuns {
  readonly fills: SweepRun;
  readonly runner?: SweepRun;
  readonly writer?: SweepRun;
}

/** What each child prints, by the build it serves: `head` its sweeps of TGT and EMPTY, its split
 *  of TGT and the server's route probes as `routeprobes` printed them, `base` the commit it built,
 *  its sweep of TGT and its split of TGT, and `keyed` (this checkout's build too) the key its
 *  target project is given and its sweep of that project. */
export interface StackRuns {
  readonly head: {
    readonly target: SweepRun;
    readonly empty: SweepRun;
    readonly split: SplitRuns;
    readonly probes: string;
  };
  readonly base: { readonly base: string; readonly target: SweepRun; readonly split: SplitRuns };
  readonly keyed: { readonly key: string; readonly target: SweepRun };
}

/** Whether `value` is a sweep's answer as `SweepRun` spells it. */
function isSweepRun(value: unknown): value is SweepRun {
  return (
    typeof value === "object" &&
    value !== null &&
    "code" in value &&
    typeof value.code === "number" &&
    "stdout" in value &&
    typeof value.stdout === "string" &&
    "stderr" in value &&
    typeof value.stderr === "string"
  );
}

/** Whether `value` is a split's answers as `SplitRuns` spells them. */
function isSplitRuns(value: unknown): value is SplitRuns {
  if (typeof value !== "object" || value === null) return false;
  if (!("fills" in value) || !isSweepRun(value.fills)) return false;
  return (
    (!("runner" in value) || isSweepRun(value.runner)) &&
    (!("writer" in value) || isSweepRun(value.writer))
  );
}

/** Whether `value` is what the child of `which` prints. */
function isRuns<Which extends keyof StackRuns>(
  which: Which,
  value: unknown
): value is StackRuns[Which] {
  if (typeof value !== "object" || value === null) return false;
  if (!("target" in value) || !isSweepRun(value.target)) return false;
  if (which === "head") {
    return (
      "empty" in value &&
      isSweepRun(value.empty) &&
      "split" in value &&
      isSplitRuns(value.split) &&
      "probes" in value &&
      typeof value.probes === "string"
    );
  }
  return which === "base"
    ? "base" in value &&
        typeof value.base === "string" &&
        "split" in value &&
        isSplitRuns(value.split)
    : "key" in value && typeof value.key === "string";
}

/** `line`, the last line the child of `which` printed, read back as what that child prints, or
 *  undefined where it is not that: no JSON, or JSON of another shape. */
export function printedRuns<Which extends keyof StackRuns>(
  which: Which,
  line: string
): StackRuns[Which] | undefined {
  let read: unknown;
  try {
    read = JSON.parse(line);
  } catch {
    return undefined;
  }
  return isRuns(which, read) ? read : undefined;
}

/** `git -C repository ARGS`: its exit status and its output. */
function git(repository: string, ...args: string[]): { code: number; out: string; err: string } {
  const done = Bun.spawnSync(["git", "-C", repository, ...args], {
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: done.exitCode,
    err: done.stderr.toString().trim(),
    out: done.stdout.toString().trim(),
  };
}

/** The commit a build with no read check is made from, in `repository`, for this checkout's HEAD.
 *
 *  `override` (DISPATCH_E2E_SWEEP_BASE) names it outright. Otherwise it is read from git, never
 *  written here, so a squash merge that rewrites the history below HEAD strands no commit: it is
 *  the merge base of HEAD and `branch` (DISPATCH_E2E_SWEEP_BASE_BRANCH, the branch the read
 *  check's pull request targets, `origin/main` once the check is on main); and where that merge
 *  base holds the read check already, as main does once the check's pull request has merged, it is
 *  the first parent of the commit of `branch`'s first-parent line that added it, which a squash
 *  merge makes the build just before the check. Whichever it is must hold restricted members (a
 *  migration that writes `member_kind`) and no read check, or this throws, naming it and the two
 *  variables: a base with the check would pass the sweep, and one without restricted members
 *  would refuse the seed, so neither is the control. */
export function noReadCheckBase(
  repository: string,
  override: string | undefined,
  branch: string
): string {
  const variables =
    "Name the build in DISPATCH_E2E_SWEEP_BASE, or the branch the read check's pull request targets in DISPATCH_E2E_SWEEP_BASE_BRANCH";
  const commitOf = (ref: string): string => {
    const read = git(repository, "rev-parse", "--verify", "--quiet", `${ref}^{commit}`);
    if (read.code !== 0 || read.out === "")
      throw new Error(`${ref} names no commit this clone holds. ${variables}.`);
    return read.out;
  };
  const holdsReadCheck = (commit: string) =>
    git(repository, "cat-file", "-e", `${commit}:${readCheckFile}`).code === 0;
  let commit: string;
  let how: string;
  if (override !== undefined && override !== "") {
    commit = commitOf(override);
    how = `DISPATCH_E2E_SWEEP_BASE's ${override}`;
  } else {
    const bases = git(repository, "merge-base", "--all", "HEAD", commitOf(branch));
    const found = bases.code === 0 ? bases.out.split("\n").filter((line) => line !== "") : [];
    const only = found.length === 1 ? found[0] : undefined;
    if (only === undefined) {
      throw new Error(
        `HEAD and ${branch} have ${found.length} merge bases (${found.join(", ") || bases.err}), so no one build is the base. ${variables}.`
      );
    }
    commit = only;
    how = `the merge base of HEAD and ${branch}`;
    if (holdsReadCheck(commit)) {
      const added = git(
        repository,
        "log",
        "--first-parent",
        "--diff-filter=A",
        "--format=%H",
        commit,
        "--",
        readCheckFile
      )
        .out.split("\n")
        .filter((line) => line !== "")
        .at(0);
      if (added === undefined)
        throw new Error(
          `${how}, ${commit}, holds ${readCheckFile}, and no commit of its first-parent line added it. ${variables}.`
        );
      commit = commitOf(`${added}^1`);
      how = `the first parent of ${added}, which added the read check on ${branch}'s first-parent line`;
    }
  }
  if (holdsReadCheck(commit)) {
    throw new Error(
      `${how}, ${commit}, holds the read check (${readCheckFile}), so its build is no control. ${variables}.`
    );
  }
  if (git(repository, "grep", "-l", "-e", "member_kind", commit, "--", migrations).code !== 0) {
    throw new Error(
      `${how}, ${commit}, holds no restricted members (no migration writes member_kind), so the seed cannot make bob one. ${variables}.`
    );
  }
  return commit;
}

type Credential = { readonly cookie: string } | { readonly bearer: string };

/** One request to `origin` as `credential`: a person's cookie with the origin's Origin, as the
 *  dashboard sends it, or a bearer. It refuses any status but `want`, naming what was sent. */
async function send(
  origin: string,
  credential: Credential,
  method: string,
  path: string,
  want: number,
  body?: unknown
): Promise<unknown> {
  const headers: Record<string, string> =
    "cookie" in credential
      ? { Cookie: credential.cookie, Origin: origin }
      : { Authorization: `Bearer ${credential.bearer}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(`${origin}${path}`, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers,
    method,
  });
  const text = await response.text();
  if (response.status !== want) {
    throw new Error(`${method} ${path} answered ${response.status}, want ${want}: ${text}`);
  }
  return text === "" ? undefined : JSON.parse(text);
}

/** Puts `body` at a fake's fixture route, as its own tests seed it. */
async function fixture(port: string, path: string, body: unknown): Promise<void> {
  const response = await fetch(`http://127.0.0.1:${port}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok)
    throw new Error(`PUT ${path} of the fake on ${port} answered ${response.status}`);
}

/** The session cookie the dev sign-in route mints for `login`. */
async function signIn(origin: string, login: string): Promise<string> {
  const response = await fetch(`${origin}/auth/_dev/signin?login=${encodeURIComponent(login)}`, {
    redirect: "manual",
  });
  const cookie = response.headers
    .getSetCookie()
    .map((line) => line.split(";")[0] ?? "")
    .find((pair) => pair.startsWith("dsession="));
  if (cookie === undefined) {
    throw new Error(
      `the dev sign-in of ${login} answered ${response.status} with no session cookie`
    );
  }
  return cookie;
}

/** The variable `name` of a stack's `env`; it throws, naming it, where the stack carries none,
 *  rather than address a port or a database the stack does not name. */
function stackVariable(env: Readonly<Record<string, string | undefined>>, name: string): string {
  const value = env[name];
  if (value === undefined || value === "") throw new Error(`the stack carries no ${name}`);
  return value;
}

/** Writes, through the stack's server at `origin`, the projects, people and objects every sweep
 *  of this file reads, and answers bob's cookie, carol's personal token and cookie, and the
 *  worker's conversation file. TGT holds an object of every kind a route names: an issue whose
 *  spec has blocks, a child issue, a comment of carol's and one of an agent's, an ask, a message,
 *  a handoff, a document of the issue and one of the project, a repository the App is installed
 *  on with a pull request the GitHub proxy reads, a priority the issue advances, a broadcast to its
 *  agent, a direct message from carol to that agent, a merged pull request and a deploy run, and
 *  a file of its agent's conversation; and, so that the routes that read them can fail, a
 *  decision record of the issue, its agent subscribed to it, an open ask its agent wrote, an
 *  architecture source of the repository, a Legion daemon the project names, an open P0 issue
 *  nobody owns, and the agent's conversation as its live view replays it. OWN holds bob's own
 *  issue and document, and bob's own session, which has commented on OWN's issue. EMPTY holds
 *  nothing. */
async function seed(
  origin: string,
  env: Readonly<Record<string, string | undefined>>,
  target = "TGT"
): Promise<Credentials> {
  const database = stackVariable(env, "DATABASE_URL");
  const asCarol = { cookie: await signIn(origin, carol) };
  const agent = { bearer: sharedToken };
  const actor = { kind: "session", id: worker };
  const first = `${target}-1`;
  const repository = `acme/${target.toLowerCase()}-repo`;
  const priority = `${target.toLowerCase()}-work`;
  await signIn(origin, bob);
  await sqlAt(database, [`update people set member_kind = 'restricted' where email = '${bob}'`]);
  const cookie = await signIn(origin, bob);

  for (const key of ["OWN", target, "EMPTY"]) {
    await send(origin, asCarol, "POST", "/api/v1/projects", 201, { key, name: `Project ${key}` });
  }
  await send(
    origin,
    asCarol,
    "PUT",
    `/api/v1/projects/OWN/access/people/${encodeURIComponent(bob)}`,
    200
  );
  await fixture(stackVariable(env, "FAKE_GITHUB_PORT"), "/__fixture/repos", {
    [repository]: {
      contents: "read",
      installation_id: 71,
      // The component model the project's architecture source reads.
      files: { "reef.md": "---\ntitle: Reef\n---\nThe target reef.\n" },
      pulls: {
        "1": {
          head_sha: "1111111111111111111111111111111111111111",
          state: "open",
          title: `The change of ${first}`,
        },
      },
    },
  });
  await fixture(stackVariable(env, "FAKE_ENVOY_PORT"), "/__fixture/sessions", [
    {
      session_id: worker,
      title: `Works on ${target}`,
      projects: [target],
      capabilities: ["aside", "btw", "steer"],
    },
    {
      session_id: ownSession,
      title: "Works on OWN",
      projects: ["OWN"],
      capabilities: ["aside", "btw", "steer"],
    },
  ]);
  // The worker follows the issue, as `dispatch subscribe` writes a session's interest.
  await fixture(stackVariable(env, "FAKE_ENVOY_PORT"), "/__fixture/interests", [
    { session_id: worker, topics: [`notifications.dispatch.issue.${first}.>`] },
  ]);

  const spec = "# Spec\n\nThe first paragraph.\n";
  await send(origin, asCarol, "POST", "/api/v1/issues", 201, {
    project: target,
    title: "Chart the target reef",
    spec,
  });
  await send(origin, asCarol, "POST", "/api/v1/issues", 201, {
    project: "OWN",
    title: "Chart the own reef",
    spec,
  });
  await send(origin, asCarol, "POST", "/api/v1/issues", 201, {
    project: target,
    title: "A child of the target reef",
    parent: first,
  });
  const remark = await send(origin, asCarol, "POST", `/api/v1/issues/${first}/comments`, 201, {
    body: "A remark.",
  });
  // A comment of bob's own project that cites the target's issue in prose by each form a person
  // or an agent writes: a reference, as the server spells one (read from the remark's own node),
  // the bare key, a dashboard URL and a project path. The member reads it in OWN's events, the
  // people pages and search, and none of it is a row of the target.
  const remarkId =
    typeof remark === "object" && remark !== null && "id" in remark && typeof remark.id === "string"
      ? remark.id
      : undefined;
  if (remarkId === undefined)
    throw new Error(`the remark answered no id: ${JSON.stringify(remark)}`);
  const node = await send(origin, asCarol, "GET", `/api/v1/comments/${remarkId}/blocks`, 200);
  const ref =
    typeof node === "object" &&
    node !== null &&
    "item" in node &&
    typeof node.item === "object" &&
    node.item !== null &&
    "ref" in node.item &&
    typeof node.item.ref === "string"
      ? node.item.ref
      : "";
  const prefix = ref.slice(0, ref.indexOf(`${first}/`));
  if (prefix === "") throw new Error(`the remark's node names no reference of ${first}: ${ref}`);
  for (const body of [
    `This follows ${prefix}${first}, the target plan, and ${first} itself. ` +
      `See https://dispatch.example/issues/${first} and /projects/${target} for the plan.`,
    // A comment whose whole body is a link, and one whose first line is: the people pages title a
    // comment by its first line.
    `https://dispatch.example/issues/${first}`,
    `${prefix}${first}\nThe plan is there.`,
    // A comment that is the bare key, and one whose first line is.
    first,
    `${first}\nThe plan is there.`,
  ]) {
    await send(origin, asCarol, "POST", "/api/v1/issues/OWN-1/comments", 201, { body });
  }
  await send(origin, agent, "POST", `/api/v1/issues/${first}/comments`, 201, {
    body: "An agent remark.",
    actor,
  });
  // bob's own session works in OWN: it comments on OWN's issue, and the project it acted in is
  // recorded as the event log records it for a proved session, so bob sees the session and the
  // live check's control message to it reaches bob's feed. A build that records no session's
  // projects (the control's, which holds no read check) has no table to write it to.
  await send(origin, agent, "POST", "/api/v1/issues/OWN-1/comments", 201, {
    body: "An agent remark of bob's own session.",
    actor: { kind: "session", id: ownSession },
  });
  await sqlAt(database, [
    "do $$ begin if to_regclass('public.session_projects') is not null then " +
      "execute 'insert into session_projects (session_id, project_key) values ($1, $2)' " +
      `using '${ownSession}', 'OWN'; end if; end $$`,
  ]);
  await send(origin, asCarol, "POST", `/api/v1/issues/${first}/asks`, 201, {
    question: "Is this the right plan?",
  });
  // An open ask the worker writes, which the open asks its session authored list
  // (GET /api/v1/asks/open?author_session=).
  await send(origin, agent, "POST", `/api/v1/issues/${first}/asks`, 201, {
    question: "Which reef is charted first?",
    actor,
  });
  await send(origin, asCarol, "POST", `/api/v1/issues/${first}/messages`, 201, {
    body: "A message from carol.",
  });
  await send(origin, agent, "POST", `/api/v1/issues/${first}/handoffs/plan`, 201, {
    body: JSON.stringify({ next: "seeded" }),
    actor,
  });
  await send(origin, asCarol, "POST", `/api/v1/issues/${first}/artifacts`, 201, {
    name: "notes.md",
    content: "# Notes\n\nCarol's notes.\n",
  });
  await send(origin, asCarol, "POST", `/api/v1/projects/${target}/artifacts`, 201, {
    name: "Playbook.md",
    content: "# Playbook\n\nHow we work.\n",
  });
  // bob's own document, which the document socket's control opens.
  await send(origin, asCarol, "POST", "/api/v1/projects/OWN/artifacts", 201, {
    name: "Guide.md",
    content: "# Guide\n\nHow bob works.\n",
  });
  // A decision record of the issue, which GET /api/v1/issues/{key}/decisions reads.
  await send(origin, asCarol, "POST", `/api/v1/issues/${first}/artifacts`, 201, {
    name: "decisions.md",
    content:
      "# Decisions\n\n## Chart the reef first\n\nThe reef is charted before anyone sails it.\n",
  });
  await send(origin, asCarol, "PATCH", `/api/v1/issues/${first}`, 200, { repo: repository });
  await send(origin, asCarol, "POST", "/api/v1/priorities", 201, {
    id: priority,
    title: "The target work",
  });
  await send(origin, asCarol, "PATCH", `/api/v1/issues/${first}`, 200, { advances: [priority] });
  // An open P0 issue of the target that nobody claimed or routed, which every delivery measure's
  // answer lists, whatever its facets (delivery.ListUnownedP0).
  await send(origin, asCarol, "POST", "/api/v1/issues", 201, {
    project: target,
    title: "An unowned P0 of the target reef",
    priority: 0,
  });
  // The repository as the project's architecture source, imported once, which
  // GET /api/v1/projects/{key}/architecture reads.
  await send(
    origin,
    asCarol,
    "PUT",
    `/api/v1/projects/${target}/architecture-sources/${repository}`,
    200,
    { branch: "main" }
  );
  const imported = await send(
    origin,
    asCarol,
    "POST",
    `/api/v1/projects/${target}/architecture-sources/${repository}/sync`,
    200
  );
  if (
    typeof imported !== "object" ||
    imported === null ||
    !("last_error" in imported) ||
    imported.last_error !== null
  ) {
    throw new Error(`the architecture source's import answered ${JSON.stringify(imported)}`);
  }
  // A Legion daemon the project names, which GET /api/v1/projects/{key}/legion/board answers with
  // empty columns until it is read; port 9 is closed, so it never is.
  await send(origin, asCarol, "PUT", `/api/v1/projects/${target}/legion/daemon`, 200, {
    state_url: "http://127.0.0.1:9",
  });
  // The worker's conversation, as its live view replays it to a viewer the server lets watch it
  // (the in-memory relay the test hooks serve).
  await send(
    origin,
    asCarol,
    "POST",
    `/api/v1/agents/${worker}/stream/_test/publish?as=responder`,
    200,
    {
      responding: true,
    }
  );
  await send(
    origin,
    asCarol,
    "POST",
    `/api/v1/agents/${worker}/stream/_test/publish?as=replay`,
    200,
    {
      v: 1,
      kind: "replay",
      turns: [{ role: "assistant", text: "The reef is charted." }],
    }
  );
  await send(origin, asCarol, "POST", "/api/v1/broadcasts", 201, {
    body: "A broadcast to the worker.",
    delivery: "btw",
    session_ids: [worker],
  });
  await send(origin, asCarol, "POST", `/api/v1/agents/${worker}/messages`, 201, {
    body: "A direct message from carol to the worker.",
    delivery: "btw",
  });
  await send(origin, asCarol, "PUT", "/api/v1/settings/delivery", 200, {
    deploy_repo: repository,
    deploy_workflow_path: ".github/workflows/deploy.yml",
    production_job_names: ["release"],
    pr_checks_workflow_path: ".github/workflows/ci.yml",
    population_authors: ["octocat"],
  });
  // A merged pull request of the target's repository and a deploy run of it, which the delivery
  // reconcile writes from GitHub and this stack never reaches, written as it writes them.
  await sqlAt(database, [
    "insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key) " +
      `values ('${repository}', 7, 'The change of ${first}', 'https://github.com/${repository}/pull/7', ` +
      `'octocat', now() - interval '3 hours', now() - interval '1 hour', '${first}')`,
    "insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, " +
      `completed_at, conclusion, url, head_branch, event) values ('${repository}', 900, 'deploy', ` +
      "'1111111111111111111111111111111111111111', now() - interval '50 minutes', now() - interval '50 minutes', " +
      "now() - interval '50 minutes', now() - interval '45 minutes', 'success', " +
      `'https://github.com/${repository}/actions/runs/900', 'main', 'push')`,
    "insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) values " +
      `('${repository}', 900, 'release', now() - interval '50 minutes', now() - interval '46 minutes', 'success')`,
  ]);
  // A file of the worker's conversation, sent as the agent writes one, which no route lists.
  const form = new FormData();
  form.append("name", "reef-shot.png");
  form.append("actor", JSON.stringify(actor));
  form.append(
    "file",
    new Blob([Buffer.from("\x89PNG\r\n\x1a\nrestricted-sweep")], { type: "image/png" }),
    "reef-shot.png"
  );
  const uploaded = await fetch(`${origin}/api/v1/agents/${worker}/artifacts`, {
    body: form,
    headers: { Authorization: `Bearer ${sharedToken}` },
    method: "POST",
  });
  const file: unknown = await uploaded.json();
  const slug =
    typeof file === "object" &&
    file !== null &&
    "artifact" in file &&
    typeof file.artifact === "object" &&
    file.artifact !== null &&
    "slug" in file.artifact
      ? file.artifact.slug
      : undefined;
  if (uploaded.status !== 201 || typeof slug !== "string") {
    throw new Error(
      `the worker's conversation file answered ${uploaded.status}: ${JSON.stringify(file)}`
    );
  }
  const minted = await send(origin, asCarol, "POST", "/api/v1/me/agent-tokens", 201, {
    name: "restricted-sweep",
  });
  const token =
    typeof minted === "object" && minted !== null && "token" in minted ? minted.token : undefined;
  if (typeof token !== "string") throw new Error("the token mint answered no token");
  return { cookie, fullCookie: asCarol.cookie, token, conversationFile: `${worker}/${slug}` };
}

/** What a sweep is run with: bob's cookie, carol's token and cookie, and the conversation file. */
interface Credentials {
  readonly cookie: string;
  readonly fullCookie: string;
  readonly token: string;
  readonly conversationFile: string;
}

/** One run of the sweep in an environment of `inputs` alone, beside PATH, HOME and TMPDIR, and
 *  unpaced; an input set undefined is left out. `stop`, once it resolves, stops the run as a
 *  driver stops a writer: SIGTERM to its exact process. */
async function sweepWith(
  inputs: Readonly<Record<string, string | undefined>>,
  stop?: Promise<void>
): Promise<SweepRun> {
  const env: Record<string, string> = {
    PATH: process.env.PATH ?? "",
    HOME: process.env.HOME ?? "",
    TMPDIR: tmpdir(),
    SWEEP_PACE_SECONDS: "0",
  };
  for (const [name, value] of Object.entries(inputs)) if (value !== undefined) env[name] = value;
  const child = Bun.spawn(["bash", sweepScript], {
    cwd: dispatchPackage,
    env,
    stderr: "pipe",
    stdout: "pipe",
  });
  const timer = setTimeout(() => child.kill("SIGKILL"), sweepMs);
  void stop?.then(() => child.kill("SIGTERM"));
  try {
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    return { code, stderr, stdout };
  } finally {
    clearTimeout(timer);
  }
}

/** One run of the sweep against `origin` for `project`, in one process, with a 5 s window. */
function sweep(origin: string, project: string, credentials: Credentials): Promise<SweepRun> {
  return sweepWith({
    SWEEP_ORIGIN: origin,
    SWEEP_PROJECT: project,
    SWEEP_WRITE_ISSUE: `${project}-1`,
    SWEEP_FULL_TOKEN: credentials.token,
    SWEEP_RESTRICTED_COOKIE: credentials.cookie,
    SWEEP_MEMBER_SESSION: ownSession,
    SWEEP_FULL_COOKIE: credentials.fullCookie,
    SWEEP_LIVE_SECONDS: "5",
    SWEEP_CONVERSATION_FILE: credentials.conversationFile,
  });
}

/** The split of `project` against `origin`, as `SplitRuns` says: the fills, written under
 *  `scratch`, then the writer, writing every second from the fills, and the runner, which holds no
 *  full member's credential; the writer is stopped once the runner ends. The runner's window is
 *  20 s, so a write the writer makes each second falls inside it whatever a busy machine holds one
 *  write up by. */
async function split(
  origin: string,
  project: string,
  credentials: Credentials,
  scratch: string
): Promise<SplitRuns> {
  const file = join(scratch, `fills-${project}.tsv`);
  const fills = await sweepWith({
    SWEEP_MODE: "fills",
    SWEEP_ORIGIN: origin,
    SWEEP_PROJECT: project,
    SWEEP_MEMBER: bob,
    SWEEP_FULL_TOKEN: credentials.token,
    SWEEP_FULL_COOKIE: credentials.fullCookie,
    SWEEP_CONVERSATION_FILE: credentials.conversationFile,
    SWEEP_FILLS_FILE: file,
  });
  if (fills.code !== 0) return { fills };
  const runnerDone = Promise.withResolvers<void>();
  const writer = sweepWith(
    {
      SWEEP_MODE: "writer",
      SWEEP_ORIGIN: origin,
      SWEEP_PROJECT: project,
      SWEEP_WRITE_ISSUE: `${project}-1`,
      SWEEP_FULL_TOKEN: credentials.token,
      SWEEP_FULL_COOKIE: credentials.fullCookie,
      SWEEP_FILLS_FILE: file,
      SWEEP_WRITE_SECONDS: "600",
      SWEEP_WRITE_EVERY: "1",
    },
    runnerDone.promise
  );
  try {
    const runner = await sweepWith({
      SWEEP_ORIGIN: origin,
      SWEEP_PROJECT: project,
      SWEEP_RESTRICTED_COOKIE: credentials.cookie,
      SWEEP_MEMBER_SESSION: ownSession,
      SWEEP_FILLS_FILE: file,
      // The shortest window a runner reads: it leaves the writer's 15 s delivery bound of it.
      SWEEP_LIVE_SECONDS: "30",
    });
    runnerDone.resolve();
    return { fills, runner, writer: await writer };
  } finally {
    runnerDone.resolve();
  }
}

/** Runs `argv` to its end, and throws naming it and its output unless it exits 0. */
function run(argv: readonly string[], what: string): string {
  const done = Bun.spawnSync([...argv], { stderr: "pipe", stdout: "pipe" });
  if (done.exitCode !== 0) {
    throw new Error(
      `${what}: ${argv.join(" ")} exited ${done.exitCode}: ${done.stderr.toString()}`
    );
  }
  return done.stdout.toString();
}

/** Builds the server of `commit` from its own tree, which `git archive` writes under `scratch`,
 *  and answers the binary's path. */
function buildBase(commit: string, scratch: string): string {
  const tree = join(scratch, "base");
  mkdirSync(tree);
  run(
    [
      "bash",
      "-o",
      "pipefail",
      "-c",
      'git -C "$1" archive "$2" packages/envoy | tar -x -C "$3"',
      "bash",
      repositoryRoot,
      commit,
      tree,
    ],
    `the tree of ${commit}`
  );
  const binary = join(scratch, "envoy-dispatch");
  run(
    ["go", "-C", join(tree, "packages/envoy"), "build", "-o", binary, "./cmd/dispatch"],
    `the server of ${commit}`
  );
  return binary;
}

/** The project key of the issue key the stack's own route index (GET /api/v1) gives as an example
 *  in its text: the search route's description writes one issue key in the forms a person writes
 *  it, upper and lower case among them, which tells it from other `NAME-8` tokens (`UTF-8`). A
 *  deployment whose target project has that key reads it in the index, which names no row of it.
 *  It throws where the index gives no such example. */
async function routeIndexKey(origin: string): Promise<string> {
  const response = await fetch(`${origin}/api/v1`, {
    headers: { Authorization: `Bearer ${sharedToken}` },
  });
  const text = await response.text();
  const key = [...text.matchAll(/\b([A-Z][A-Z0-9]{1,9})-([1-9][0-9]*)\b/g)]
    .filter(([, name = "", number = ""]) => text.includes(`${name.toLowerCase()}-${number}`))
    .map(([, name = ""]) => name)
    .find((name) => !["OWN", "EMPTY"].includes(name));
  if (response.status !== 200 || key === undefined) {
    throw new Error(
      `GET /api/v1 answered ${response.status} and gives no issue key as an example: ${text.slice(0, 300)}`
    );
  }
  return key;
}

if (import.meta.main) {
  const which = Bun.argv[2];
  if (which !== "head" && which !== "base" && which !== "keyed") {
    console.error("usage: bun e2e/__tests__/restricted-sweep-stack.ts <head | base | keyed>");
    process.exit(2);
  }
  const scratch = mkdtempSync(join(tmpdir(), "restricted-sweep-stack-"));
  try {
    // An empty dashboard: no row here loads a page, and the checkout need not have built one.
    process.env.DISPATCH_E2E_SERVER_WEB_DIST = join(scratch, "web");
    mkdirSync(process.env.DISPATCH_E2E_SERVER_WEB_DIST);
    const base =
      which === "base"
        ? noReadCheckBase(
            repositoryRoot,
            process.env.DISPATCH_E2E_SWEEP_BASE,
            process.env.DISPATCH_E2E_SWEEP_BASE_BRANCH || "origin/main"
          )
        : undefined;
    if (base !== undefined) process.env.DISPATCH_E2E_SERVER_BIN = buildBase(base, scratch);
    const [stack] = await startStacks(1);
    try {
      if (stack === undefined) throw new Error("the harness started no stack");
      const origin = `http://127.0.0.1:${stackVariable(stack.env, "DISPATCH_E2E_PORT")}`;
      let runs: StackRuns[keyof StackRuns];
      if (which === "keyed") {
        const key = await routeIndexKey(origin);
        const credentials = await seed(origin, stack.env, key);
        runs = { key, target: await sweep(origin, key, credentials) };
      } else if (base === undefined) {
        const credentials = await seed(origin, stack.env);
        runs = {
          target: await sweep(origin, "TGT", credentials),
          empty: await sweep(origin, "EMPTY", credentials),
          split: await split(origin, "TGT", credentials, scratch),
          probes: run(
            [
              "go",
              "-C",
              join(repositoryRoot, "packages/envoy"),
              "run",
              "./internal/dispatch/api/cmd/routeprobes",
            ],
            "the route probes"
          ),
        };
      } else {
        const credentials = await seed(origin, stack.env);
        runs = {
          base,
          target: await sweep(origin, "TGT", credentials),
          split: await split(origin, "TGT", credentials, scratch),
        };
      }
      console.log(JSON.stringify(runs));
    } finally {
      await stopStacks(false);
    }
  } finally {
    rmSync(scratch, { force: true, recursive: true });
  }
}
