#!/usr/bin/env bun
// Captures testdata/f9-gh-help.json and testdata/f9-gh-requests.json from the real gh (f9-gh.command
// says when, and with which gh). Each call runs the real gh against a recorder on loopback that
// stands for api.github.localhost, the development host gh reaches over plain HTTP, through
// HTTP_PROXY, with a fake token and an empty gh config: nothing reaches GitHub. The recorder
// answers each GraphQL query with exactly the fields it selects (gh's client refuses any other
// member), each a plausible value by its name, so gh goes on to the request it makes for its
// arguments; that request is what the capture keeps.
//   bun f9-gh-capture.ts <gh> <output directory>
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

const [gh = "gh", out = "."] = Bun.argv.slice(2);
const scratch = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "f9-"));

/** The commands whose flags the stand-in reads, as their help prints them. */
const HELP = [
  "api",
  "pr create",
  "pr edit",
  "pr review",
  "pr merge",
  "pr close",
  "pr reopen",
  "pr ready",
  "pr comment",
  "pr view",
  "pr list",
  "pr checks",
  "pr diff",
  "run view",
  "run list",
];

/** Each call, run in a checkout on branch legion/ACME-12 of acme/widgets. `branch` is what the
 * recorder answers a query for the pull requests of a branch: pull request 7, or none; `detached`
 * runs the call in a checkout on no branch. Pull request 99 is one GitHub holds none of. */
interface Case {
  argv: string[];
  stdin?: string;
  branch?: "one" | "none" | "detached";
}
const CASES: Case[] = [
  // Calls from real runs, on the recorder's repository: the F2 transcript's (testdata/f2) and the
  // merger's of f8.
  { argv: ["pr", "edit", "77", "-R", "acme/widgets", "--body-file", "-"], stdin: "New body.\n" },
  {
    argv: [
      "pr",
      "view",
      "77",
      "-R",
      "acme/widgets",
      "--json",
      "number,state,mergeCommit,headRefOid,body,mergedAt,url",
    ],
  },
  { argv: ["pr", "view", "77", "-R", "acme/widgets", "--json", "body", "--jq", ".body"] },
  { argv: ["pr", "view", "7", "-R", "acme/widgets", "--json", "id", "-q", ".id"] },
  {
    argv: [
      "api",
      "graphql",
      "-F",
      "number=7",
      "-f",
      "owner=acme",
      "-f",
      "name=widgets",
      "-f",
      "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergedAt isInMergeQueue mergeStateStatus headRefOid mergeQueueEntry{state position}}}}",
    ],
  },
  {
    argv: [
      "api",
      "graphql",
      "-f",
      "query=mutation($pr:ID!,$head:GitObjectID!){enqueuePullRequest(input:{pullRequestId:$pr,expectedHeadOid:$head}){mergeQueueEntry{position}}}",
      "-f",
      "pr=PR_node7",
      "-f",
      `head=${"c".repeat(40)}`,
    ],
  },
  {
    argv: [
      "api",
      "graphql",
      "-f",
      "query=mutation($pr:ID!){dequeuePullRequest(input:{id:$pr}){mergeQueueEntry{position}}}",
      "-f",
      "pr=PR_node7",
    ],
  },
  { argv: ["api", "--paginate", "repos/acme/widgets/rules/branches/main"] },
  { argv: ["api", "repos/acme/widgets", "--jq", "{default_branch, allow_merge_commit}"] },
  // gh api: the method named, attached or implied by fields; -H a header.
  {
    argv: ["api", "repos/acme/widgets/pulls", "-f", "head=acme:legion/ACME-13", "-f", "base=main"],
  },
  {
    argv: [
      "api",
      "--method=POST",
      "repos/acme/widgets/pulls/7/reviews",
      "-f",
      "event=APPROVE",
      "-f",
      "body=Fine.",
    ],
  },
  { argv: ["api", "-XPATCH", "repos/acme/widgets/pulls/7", "-f", "body=new"] },
  { argv: ["api", "-X", "PATCH", "repos/acme/widgets/pulls/7", "-f", "state=closed"] },
  {
    argv: [
      "api",
      "-H",
      "Accept: application/vnd.github+json",
      "repos/acme/widgets/pulls/7",
      "-f",
      "body=new",
    ],
  },
  { argv: ["api", "-X", "PUT", "repos/acme/widgets/pulls/7/merge", "-f", "merge_method=squash"] },
  {
    argv: [
      "api",
      "repos/acme/widgets/pulls/7/comments",
      "-f",
      "body=Why?",
      "-f",
      "path=a.go",
      "-F",
      "line=3",
    ],
  },
  { argv: ["api", "repos/acme/widgets/issues/7/comments", "-F", "body=@-"], stdin: "A comment.\n" },
  // gh pr review: short flags, joined short flags, a selector before or after the flags, none.
  { argv: ["pr", "review", "-a"] },
  { argv: ["pr", "review", "-a"], branch: "none" },
  { argv: ["pr", "review", "-a"], branch: "detached" },
  { argv: ["pr", "review", "7", "-r", "-b", "Changes."] },
  { argv: ["pr", "review", "-c", "-b", "A note.", "7"] },
  { argv: ["pr", "review", "7", "--approve", "--body=Fine at B."] },
  { argv: ["pr", "review", "7", "-cb", "Joined."] },
  { argv: ["pr", "review", "https://github.localhost/acme/widgets/pull/7", "-a"] },
  { argv: ["pr", "review", "legion/ACME-12", "-a"] },
  { argv: ["pr", "review", "7"] },
  { argv: ["pr", "review", "7", "-a", "-r"] },
  { argv: ["pr", "review", "7", "-a", "-r", "-b", "x"] },
  { argv: ["pr", "review", "7", "-c"] },
  { argv: ["pr", "review", "99", "-a"] },
  // The other gh pr commands that change a pull request.
  {
    argv: ["pr", "create", "-d", "-H", "legion/ACME-13", "-B", "main", "-t", "T", "-b", "B"],
    branch: "none",
  },
  {
    argv: [
      "pr",
      "create",
      "--head=legion/ACME-13",
      "--base",
      "main",
      "-t",
      "T",
      "--body-file",
      "-",
    ],
    stdin: "Body.\n",
    branch: "none",
  },
  { argv: ["pr", "edit", "7", "-b", "new", "-t", "New title"] },
  { argv: ["pr", "edit", "-b", "branch body"] },
  { argv: ["pr", "edit", "-b", "branch body"], branch: "none" },
  { argv: ["pr", "edit", "99", "-b", "x"] },
  { argv: ["pr", "merge", "7", "-s", "-d"] },
  { argv: ["pr", "merge", "--auto", "--squash"] },
  { argv: ["pr", "merge", "7", "--disable-auto"] },
  { argv: ["pr", "close", "-d", "7"] },
  { argv: ["pr", "close", "7", "-c", "Superseded."] },
  { argv: ["pr", "reopen", "7"] },
  { argv: ["pr", "ready", "7", "--undo"] },
  { argv: ["pr", "comment", "7", "-b", "A comment."] },
  // Reads.
  { argv: ["pr", "checks", "7"] },
  { argv: ["pr", "list", "-H", "legion/ACME-12", "-s", "all", "--json", "number"] },
  { argv: ["pr", "list", "-a", "@me", "-d", "--json", "number"] },
];

let branchPulls: "one" | "none" = "one";
let requests: { method: string; path: string; body: unknown }[] = [];

/** One field a query selects: its name, the key the answer gives it, and what it selects. */
interface Selection {
  name: string;
  alias: string;
  selects: Selection[] | null;
}

/** The selection set of a GraphQL document's operation, its fragments spread in place. */
function selections(text: string): Selection[] {
  const tokens =
    text.match(/"(?:[^"\\]|\\.)*"|\.\.\.|[A-Za-z_$][\w]*|-?\d+(?:\.\d+)?|[{}():,!=[\]@]/g) ?? [];
  let at = 0;
  const fragments = new Map<string, Selection[]>();
  const skipArguments = () => {
    if (tokens[at] !== "(") return;
    let depth = 0;
    do {
      if (tokens[at] === "(") depth++;
      if (tokens[at] === ")") depth--;
      at++;
    } while (depth > 0 && at < tokens.length);
  };
  const set = (): Selection[] => {
    const fields: Selection[] = [];
    at++;
    while (at < tokens.length && tokens[at] !== "}") {
      if (tokens[at] === ",") {
        at++;
        continue;
      }
      if (tokens[at] === "...") {
        at++;
        if (tokens[at] === "on") {
          at += 2;
          fields.push(...set());
        } else fields.push({ name: `...${tokens[at++]}`, alias: "", selects: null });
        continue;
      }
      let name = tokens[at++] ?? "";
      const alias = name;
      if (tokens[at] === ":") {
        at++;
        name = tokens[at++] ?? "";
      }
      skipArguments();
      while (tokens[at] === "@") {
        at += 2;
        skipArguments();
      }
      fields.push({ name, alias, selects: tokens[at] === "{" ? set() : null });
    }
    at++;
    return fields;
  };
  let operation: Selection[] = [];
  while (at < tokens.length) {
    if (tokens[at] === "fragment") {
      const name = tokens[at + 1] ?? "";
      at += 4;
      fragments.set(name, set());
    } else if (tokens[at] === "query" || tokens[at] === "mutation") {
      at++;
      if (tokens[at] !== "{" && tokens[at] !== "(") at++;
      skipArguments();
    } else if (tokens[at] === "{") operation = set();
    else at++;
  }
  const spread = (fields: Selection[]): Selection[] =>
    fields.flatMap((field) =>
      field.name.startsWith("...")
        ? spread(fragments.get(field.name.slice(3)) ?? [])
        : [{ ...field, selects: field.selects && spread(field.selects) }]
    );
  return spread(operation);
}

/** The fields whose value is a list: a connection's nodes and edges, and introspection's lists,
 * which the recorder answers empty, so gh takes no optional field for granted. */
const LISTS = ["nodes", "edges", "fields", "enumValues", "possibleTypes", "inputFields", "args"];

function answer(fields: Selection[], variables: Record<string, unknown>, parents: string[]) {
  const value: Record<string, unknown> = {};
  for (const field of fields) {
    const here = [...parents, field.name];
    if (field.selects === null) value[field.alias] = leaf(field.name, variables, parents);
    else if (field.name === "nodes" && parents.at(-1) === "pullRequests")
      value[field.alias] = branchPulls === "one" ? [answer(field.selects, variables, here)] : [];
    else if (LISTS.includes(field.name))
      value[field.alias] =
        parents.at(-1) === "__type" ? [] : [answer(field.selects, variables, here)];
    else value[field.alias] = answer(field.selects, variables, here);
  }
  return value;
}

function leaf(name: string, variables: Record<string, unknown>, parents: string[]): unknown {
  const number = Number(variables.pr_number ?? variables.number ?? 7);
  const parent = parents.at(-1);
  const values: Record<string, unknown> = {
    __typename: parent === "repository" ? "Repository" : "PullRequest",
    id: parent === "repository" ? "R_1" : parents.includes("viewer") ? "U_1" : `PR_node${number}`,
    number,
    login: parents.includes("viewer") || parents.includes("author") ? "rig" : "acme",
    name: parent === "repository" || parent === "headRepository" ? "widgets" : "main",
    headRefName: variables.headRefName ?? "legion/ACME-12",
    baseRefName: "main",
    state: "OPEN",
    mergeable: "MERGEABLE",
    mergeStateStatus: "CLEAN",
    viewerPermission: "ADMIN",
    url: `https://github.localhost/acme/widgets/pull/${number}`,
    totalCount: branchPulls === "one" ? 1 : 0,
    position: 1,
    headRefOid: "b".repeat(40),
    endCursor: null,
    body: "old",
    title: "T",
    reviewDecision: "",
    createdAt: "2030-01-07T12:00:00Z",
    updatedAt: "2030-01-07T12:00:00Z",
  };
  if (Object.hasOwn(values, name)) return values[name];
  if (/Count$/.test(name)) return 0;
  if (
    ["isDraft", "isCrossRepository", "isInMergeQueue", "hasNextPage", "locked", "closed"].includes(
      name
    )
  )
    return false;
  if (/^(is|has|viewerCan|viewerDid)|Allowed$|Enabled$/.test(name)) return true;
  return null;
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const text = await request.text();
    let body: unknown = text === "" ? null : text;
    try {
      body = JSON.parse(text);
    } catch {
      // A body that is no JSON is kept as its text.
    }
    requests.push({ method: request.method, path: `${url.pathname}${url.search}`, body });
    if (url.pathname !== "/graphql") {
      if (request.method === "GET" && url.pathname === "/repos/acme/widgets")
        return Response.json({ full_name: "acme/widgets", default_branch: "main" });
      if (request.method === "DELETE") return new Response(null, { status: 204 });
      return Response.json({ number: 7, html_url: "https://github.localhost/acme/widgets/pull/7" });
    }
    const query =
      typeof body === "object" && body !== null && "query" in body ? String(body.query) : "";
    const variables: Record<string, unknown> =
      typeof body === "object" &&
      body !== null &&
      "variables" in body &&
      typeof body.variables === "object" &&
      body.variables !== null
        ? { ...body.variables }
        : {};
    if (variables.pr_number === 99)
      return Response.json({
        data: { repository: { pullRequest: null } },
        errors: [
          {
            type: "NOT_FOUND",
            path: ["repository", "pullRequest"],
            message: "Could not resolve to a PullRequest with the number of 99.",
          },
        ],
      });
    return Response.json({ data: answer(selections(query), variables, []) });
  },
});

/** A checkout on branch legion/ACME-12 of the recorder's repository, and one on no branch. */
const checkout = path.join(scratch, "checkout");
const detached = path.join(scratch, "detached");
for (const [dir, branch, ...more] of [
  [checkout, "legion/ACME-12"],
  [detached, "main", ["checkout", "-q", "--detach"]],
] as const) {
  mkdirSync(dir);
  for (const args of [
    ["init", "-q", "-b", branch],
    ["commit", "-q", "--allow-empty", "-m", "c"],
    ["remote", "add", "origin", "https://github.localhost/acme/widgets.git"],
    ...more,
  ]) {
    const git = Bun.spawnSync(["git", ...args], {
      cwd: dir,
      env: {
        PATH: process.env.PATH ?? "",
        HOME: scratch,
        GIT_CONFIG_NOSYSTEM: "1",
        GIT_AUTHOR_NAME: "r",
        GIT_AUTHOR_EMAIL: "r@example.invalid",
        GIT_COMMITTER_NAME: "r",
        GIT_COMMITTER_EMAIL: "r@example.invalid",
      },
    });
    if (git.exitCode !== 0) throw new Error(git.stderr.toString());
  }
}

/** The real gh with `args`, as a pane runs it, against the recorder. The recorder answers in this
 * process, so the call must not block it. */
async function run(args: string[], stdin?: string, cwd = checkout) {
  const config = mkdtempSync(path.join(scratch, "config-"));
  const proxy = `http://127.0.0.1:${server.port}`;
  const child = Bun.spawn([gh, ...args], {
    cwd,
    stdin: stdin === undefined ? "ignore" : new TextEncoder().encode(stdin),
    stdout: "pipe",
    stderr: "pipe",
    env: {
      PATH: process.env.PATH ?? "",
      HOME: config,
      GH_CONFIG_DIR: config,
      GH_HOST: "github.localhost",
      GH_ENTERPRISE_TOKEN: fixtureSecret("gh"),
      HTTP_PROXY: proxy,
      http_proxy: proxy,
      GH_NO_UPDATE_NOTIFIER: "1",
      GH_PROMPT_DISABLED: "1",
      GH_PAGER: "cat",
      NO_COLOR: "1",
    },
  });
  const timer = setTimeout(() => child.kill(), 20_000);
  const [stdout, stderr, exit] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  clearTimeout(timer);
  rmSync(config, { recursive: true, force: true });
  return { exit, stdout, stderr };
}

const help: Record<string, string> = {};
for (const command of HELP) help[command] = (await run([...command.split(" "), "--help"])).stdout;
writeFileSync(path.join(out, "f9-gh-help.json"), `${JSON.stringify(help, null, 2)}\n`);

const captured = [];
for (const each of CASES) {
  branchPulls = each.branch === "none" ? "none" : "one";
  requests = [];
  const { exit, stderr } = await run(
    each.argv,
    each.stdin,
    each.branch === "detached" ? detached : checkout
  );
  captured.push({
    argv: each.argv,
    ...(each.stdin === undefined ? {} : { stdin: each.stdin }),
    branch: each.branch ?? "one",
    exit,
    stderr,
    // gh's own GraphQL queries by their operation's first line; a query the call names, whole.
    requests: requests.map(({ method, path: route, body }) => {
      if (typeof body !== "object" || body === null || !("query" in body))
        return { method, path: route, body };
      const query = String(body.query).trim();
      const variables = "variables" in body ? body.variables : undefined;
      return each.argv[1] === "graphql"
        ? { method, path: route, body }
        : { method, path: route, operation: query.split(/[({]/)[0]?.trim(), variables };
    }),
  });
}
writeFileSync(path.join(out, "f9-gh-requests.json"), `${JSON.stringify(captured, null, 2)}\n`);
await server.stop(true);
rmSync(scratch, { recursive: true, force: true });
