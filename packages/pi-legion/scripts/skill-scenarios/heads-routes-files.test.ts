// The reads of a head's files and of the workflow runs, over the table rendered from
// heads-routes-fixture.ts's world: `gh run list` and `gh workflow list`, which answer off a
// terminal as gh answers them, and the contents, raw bytes, listings, trees, blobs and commits of
// the base and each head. heads-routes.test.ts holds the rows of every other read of that table,
// and both files read one world.
import { expect, setDefaultTimeout, test } from "bun:test";
import { renderHeadsRoutes } from "./heads-routes";
import {
  answer,
  base64,
  DOCS_TREE,
  GO_CHECKS,
  H702,
  H703,
  MAIN,
  notFound,
  plain,
  README,
  README703,
  routeOf,
  TREE702,
  TREE703,
  USAGE,
  USAGE_FAILED,
  USAGE_TEXT,
  world,
} from "./heads-routes-fixture";
import { fileRowLimit, SCRIPT_RUN } from "./test-limit";

// One row renders the table of a world of its own, which a loaded machine stretches.
setDefaultTimeout(fileRowLimit({ ...SCRIPT_RUN, part: "heads-routes.ts" }));

// Catches: `gh run list` with no route, and a list that is not the REST runs': the repository's
// runs, a head's by its commit or branch (none for another), and each run's fields under `--json`.
// A filter the world cannot apply (a workflow, a status) has no route.
test("gh run list prints the workflow runs, all or one head's, as gh does", () => {
  const failed = renderHeadsRoutes(world({ named: [USAGE_FAILED] }));
  const line = (status: string, workflow: string, branch: string, id: number) =>
    `completed\t${status}\tA usage page (ACME-7)\t${workflow}\t${branch}\tpull_request\t${id}\t0s\t2026-11-22T12:00:00Z`;
  const run1 = line("success", "ci", "legion/ACME-7-702", 1);
  const run42 = line("failure", "usage-test", "legion/ACME-7-702", 42);
  const run101 = line("success", "ci", "legion/ACME-7-703", 101);
  expect(answer(failed, "gh run list -R example/widgets --limit 10")).toBe(
    [run1, run42, run101].join("\n")
  );
  expect(answer(failed, "gh run list")).toBe([run1, run42, run101].join("\n"));
  expect(answer(failed, `gh run list --commit ${H702}`)).toBe([run1, run42].join("\n"));
  expect(answer(failed, "gh run list -b legion/ACME-7-703")).toBe(run101);
  expect(answer(failed, `gh run list --commit=${"9".repeat(40)}`)).toBe("");
  expect(answer(failed, "gh run list --json databaseId,conclusion -L 5")).toMatchObject([
    { databaseId: 1, conclusion: "success", headSha: H702, workflowName: "ci" },
    {
      databaseId: 42,
      status: "completed",
      conclusion: "failure",
      name: "usage-test",
      displayTitle: "A usage page (ACME-7)",
      headBranch: "legion/ACME-7-702",
      event: "pull_request",
      url: "https://github.com/example/widgets/actions/runs/42",
    },
    { databaseId: 101, headSha: H703 },
  ]);
  expect(answer(failed, `gh run list -c ${H703} --json databaseId`)).toMatchObject([
    { databaseId: 101 },
  ]);
  for (const call of ["gh run list --workflow usage-test", "gh run list -s failure --json name"])
    expect(() => routeOf(failed, call)).toThrow("no route names");
  const running = renderHeadsRoutes(world({ named: [GO_CHECKS] }));
  expect(answer(running, `gh run list --commit ${H702}`)).toBe(
    [
      line("success", "ci", "legion/ACME-7-702", 1),
      "in_progress\t\tA usage page (ACME-7)\tgo-checks\tlegion/ACME-7-702\tpull_request\t41\t0s\t2026-11-22T12:00:00Z",
    ].join("\n")
  );
});

// Catches: `gh workflow list` with no route, a rig error in an architect's run of the other head's
// world (`gh workflow list --repo example/widgets`). It answers the world's workflows, one for each
// name the world's runs carry, each active: under `--json` the four fields gh names, else off a
// terminal each name, state and id with a tab between two and no header. The world's repository may
// be named before the command or after it; another repository, and a limit below the count, have no
// route. The routes stand last in the table, so no call another route answers is answered anew.
test("gh workflow list prints the world's workflows, as gh does", () => {
  const failed = renderHeadsRoutes(world({ named: [USAGE_FAILED] }));
  const listed = answer(failed, "gh workflow list --json id,name,path,state -R example/widgets");
  expect(listed).toEqual([
    { id: expect.any(Number), name: "ci", path: ".github/workflows/ci.yml", state: "active" },
    {
      id: expect.any(Number),
      name: "usage-test",
      path: ".github/workflows/usage-test.yml",
      state: "active",
    },
  ]);
  const workflows = listed as { id: number; name: string; state: string }[];
  expect(new Set(workflows.map((workflow) => workflow.id)).size).toBe(2);
  const text = workflows.map((w) => `${w.name}\t${w.state}\t${w.id}`).join("\n");
  for (const call of [
    "gh workflow list --repo example/widgets",
    "gh workflow list",
    "gh workflow ls -R example/widgets --all",
    "gh -R example/widgets workflow list",
    "gh --repo=example/widgets workflow list -L 10",
    "gh workflow list -R https://github.com/example/widgets",
  ])
    expect({ call, text: answer(failed, call) }).toEqual({ call, text });
  expect(answer(failed, "gh -R example/widgets workflow list --json name")).toEqual(listed);
  for (const call of [
    "gh workflow list -R skills-rig/project",
    "gh -R skills-rig/project workflow list",
    "gh workflow list --limit 1",
  ])
    expect(() => routeOf(failed, call)).toThrow("no route names");
  // The table before these routes is the one the stand-in served: none of it answers the list.
  const before = failed.routes.filter((route) => !/workflow \(list\|ls\)/.test(route.match));
  expect(failed.routes.slice(0, before.length)).toEqual(before);
  expect(
    before.filter((route) =>
      new RegExp(route.match, "s").test("gh workflow list --repo example/widgets")
    )
  ).toEqual([]);
});

// Catches: a read of a file at a head (contents, raw or JSON, a directory, a tree, a blob, the
// commit) with no route, bytes that are not the commit's own, and a path or object no commit
// holds answered instead of GitHub's 404.
test("the files of the base and each head: contents, raw bytes, listings, trees, blobs, commits", () => {
  const usage = answer(plain, `gh api repos/example/widgets/contents/docs/usage.md?ref=${H702}`);
  expect(usage).toEqual({
    type: "file",
    encoding: "base64",
    size: Buffer.byteLength(USAGE_TEXT),
    name: "usage.md",
    path: "docs/usage.md",
    content: base64(USAGE_TEXT),
    sha: USAGE,
    url: `https://api.github.com/repos/example/widgets/contents/docs/usage.md?ref=${H702}`,
    git_url: `https://api.github.com/repos/example/widgets/git/blobs/${USAGE}`,
    html_url: `https://github.com/example/widgets/blob/${H702}/docs/usage.md`,
    download_url: `https://raw.githubusercontent.com/example/widgets/${H702}/docs/usage.md`,
  });
  expect(
    answer(
      plain,
      "gh api -H Accept: application/vnd.github.raw repos/example/widgets/contents/docs/usage.md?ref=legion%2FACME-7-702"
    )
  ).toBe(USAGE_TEXT);
  // The base answers with no ref at all, and its file there is the base's own.
  expect(answer(plain, "gh api repos/example/widgets/contents/README.md")).toMatchObject({
    sha: README,
  });
  expect(
    answer(plain, `gh api repos/example/widgets/contents/README.md?ref=${H703}`)
  ).toMatchObject({
    sha: README703,
  });
  expect(answer(plain, "gh api repos/example/widgets/contents/docs?ref=legion/ACME-7-702")).toEqual(
    [
      {
        type: "file",
        size: Buffer.byteLength(USAGE_TEXT),
        name: "usage.md",
        path: "docs/usage.md",
        sha: USAGE,
        url: "https://api.github.com/repos/example/widgets/contents/docs/usage.md?ref=legion/ACME-7-702".replace(
          "legion/ACME-7-702",
          H702
        ),
      },
    ]
  );
  expect(answer(plain, `gh api repos/example/widgets/contents/?ref=${H702}`)).toMatchObject([
    { type: "file", name: "README.md" },
    { type: "dir", name: "docs", size: 0 },
  ]);
  const { match: _missing, ...missing } = routeOf(
    plain,
    `gh api repos/example/widgets/contents/no/such/file.md?ref=${H702}`
  );
  expect(missing).toEqual(
    notFound("https://docs.github.com/rest/repos/contents#get-repository-content")
  );
  expect(answer(plain, `gh api repos/example/widgets/git/trees/${H702}?recursive=1`)).toMatchObject(
    {
      sha: TREE702,
      truncated: false,
      tree: [
        { path: "README.md", type: "blob", sha: README, size: 30 },
        {
          path: "docs",
          mode: "040000",
          type: "tree",
          sha: DOCS_TREE,
          url: `https://api.github.com/repos/example/widgets/git/trees/${DOCS_TREE}`,
        },
        { path: "docs/usage.md", type: "blob", sha: USAGE },
      ],
    }
  );
  expect(answer(plain, "gh api repos/example/widgets/git/trees/legion/ACME-7-702")).toMatchObject({
    sha: TREE702,
    tree: [{ path: "README.md" }, { path: "docs" }],
  });
  expect(answer(plain, `gh api repos/example/widgets/git/trees/${DOCS_TREE}`)).toEqual({
    sha: DOCS_TREE,
    url: `https://api.github.com/repos/example/widgets/git/trees/${DOCS_TREE}`,
    truncated: false,
    tree: [
      {
        path: "usage.md",
        mode: "100644",
        type: "blob",
        sha: USAGE,
        url: `https://api.github.com/repos/example/widgets/git/blobs/${USAGE}`,
        size: Buffer.byteLength(USAGE_TEXT),
      },
    ],
  });
  expect(routeOf(plain, `gh api repos/example/widgets/git/trees/${"e".repeat(40)}`).status).toBe(
    404
  );
  expect(answer(plain, `gh api repos/example/widgets/git/blobs/${USAGE}`)).toEqual({
    sha: USAGE,
    node_id: "B_rig",
    size: Buffer.byteLength(USAGE_TEXT),
    url: `https://api.github.com/repos/example/widgets/git/blobs/${USAGE}`,
    content: base64(USAGE_TEXT),
    encoding: "base64",
  });
  expect(routeOf(plain, `gh api repos/example/widgets/git/blobs/${"e".repeat(40)}`).status).toBe(
    404
  );
  expect(answer(plain, "gh api repos/example/widgets/commits/legion/ACME-7-702")).toMatchObject({
    sha: H702,
    commit: {
      message: "docs: the usage page of the widgets CLI (ACME-7)\n",
      tree: { sha: TREE702 },
    },
    parents: [{ sha: MAIN }],
    files: [{ filename: "docs/usage.md", status: "added", changes: 3 }],
  });
  expect(answer(plain, "gh api repos/example/widgets/commits/main")).toMatchObject({
    sha: MAIN,
    parents: [],
    files: [],
  });
  expect(answer(plain, `gh api repos/example/widgets/git/commits/${H703}`)).toEqual({
    sha: H703,
    message: "docs: point the README at the usage page (ACME-7)\n",
    tree: { sha: TREE703 },
    parents: [{ sha: MAIN }],
  });
});
