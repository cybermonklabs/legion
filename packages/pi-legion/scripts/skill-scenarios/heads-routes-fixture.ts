// The world the rows of a rendered heads table read, and the readers of that table: one fixture
// the row files share (heads-routes.test.ts, heads-routes-files.test.ts), so each holds the rows
// of its own reads and neither keeps a copy of the world.
import type { HeadsWorld } from "./heads-answers";
import { type Route, type RouteTable, renderHeadsRoutes } from "./heads-routes";

export const MAIN = "1".repeat(40);
export const H702 = "2".repeat(40);
export const H703 = "3".repeat(40);
export const BASE_TREE = "4".repeat(40);
export const TREE702 = "5".repeat(40);
export const TREE703 = "6".repeat(40);
export const DOCS_TREE = "7".repeat(40);
export const README = "a".repeat(40);
export const README703 = "b".repeat(40);
export const USAGE = "c".repeat(40);
export const USAGE_TEXT =
  "# Usage\n\nRun `widgets --help` to list every command of the widgets CLI.\n";

/** A file's bytes in base64 as heads_routes collects them: 60 characters a line, and a newline. */
export const base64 = (text: string) =>
  `${(
    Buffer.from(text)
      .toString("base64")
      .match(/.{1,60}/g) ?? []
  ).join("\n")}\n`;
export const blob = (file: string, sha: string, text: string) => ({
  path: file,
  mode: "100644",
  type: "blob",
  sha,
  size: Buffer.byteLength(text),
  content: base64(text),
});
export const DIFF702 =
  "diff --git a/docs/usage.md b/docs/usage.md\nnew file mode 100644\nindex 0000000..cccccc\n--- /dev/null\n+++ b/docs/usage.md\n@@ -0,0 +1,3 @@\n+# Usage\n+\n+Run `widgets --help` to list every command of the widgets CLI.\n";
export const DIFF703 =
  "diff --git a/README.md b/README.md\nindex aaaaaa..bbbbbb 100644\n--- a/README.md\n+++ b/README.md\n@@ -1,3 +1,5 @@\n # widgets\n \n Small CLI helpers.\n+\n+Usage: see docs/usage.md.\n";

/** The world of reviewer-own-head: #702 adds docs/usage.md, #703 points the README at it, the
 * base has no protection, and `named`, `named703` and `approved` set what the case changes. */
export function world(
  options: {
    named?: HeadsWorld["pulls"][number]["named"];
    named703?: HeadsWorld["pulls"][number]["named"];
    approved?: boolean;
    only702?: boolean;
  } = {}
): HeadsWorld {
  const pulls: HeadsWorld["pulls"] = [
    {
      number: 702,
      head: H702,
      body: "Dispatch: ACME-7\n\n## For the reviewer\n",
      files: [{ path: "docs/usage.md", additions: 3, deletions: 0 }],
      jobs: ["lint", "test"],
      named: options.named ?? [],
      diff: DIFF702,
      url: "https://github.com/example/widgets/pull/702",
      subject: "docs: the usage page of the widgets CLI (ACME-7)",
    },
    {
      number: 703,
      head: H703,
      body: "Dispatch: ACME-7\n\n## Verification\n",
      files: [{ path: "README.md", additions: 2, deletions: 0 }],
      jobs: ["lint", "test"],
      named: options.named703 ?? [],
      diff: DIFF703,
      url: "https://github.com/example/widgets/pull/703",
      subject: "docs: point the README at the usage page (ACME-7)",
    },
  ];
  return {
    repo: "example/widgets",
    key: "ACME-7",
    title: "A usage page",
    base: "main",
    main: MAIN,
    approved: options.approved ?? false,
    at: "2026-11-22T12:00:00Z",
    pulls: options.only702 === true ? pulls.slice(0, 1) : pulls,
    branches: [
      { name: "legion/ACME-7-702", sha: H702 },
      { name: "legion/ACME-7-703", sha: H703 },
      { name: "main", sha: MAIN },
    ],
    trees: [
      {
        sha: MAIN,
        tree: BASE_TREE,
        message: "init\n",
        parent: "",
        entries: [blob("README.md", README, "# widgets\n\nSmall CLI helpers.\n")],
      },
      {
        sha: H702,
        tree: TREE702,
        message: "docs: the usage page of the widgets CLI (ACME-7)\n",
        parent: MAIN,
        entries: [
          blob("README.md", README, "# widgets\n\nSmall CLI helpers.\n"),
          { path: "docs", mode: "040000", type: "tree", sha: DOCS_TREE, size: 0, content: null },
          blob("docs/usage.md", USAGE, USAGE_TEXT),
        ],
      },
      {
        sha: H703,
        tree: TREE703,
        message: "docs: point the README at the usage page (ACME-7)\n",
        parent: MAIN,
        entries: [
          blob(
            "README.md",
            README703,
            "# widgets\n\nSmall CLI helpers.\n\nUsage: see docs/usage.md.\n"
          ),
        ],
      },
    ],
  };
}

export const GO_CHECKS = {
  name: "go-checks",
  checkRun: 9101,
  run: 41,
  suite: 41,
  status: "in_progress",
};
export const DOCS = { name: "docs", checkRun: 9001, run: 40, suite: 40, status: "completed" };
/** A named job that ended without a pass, and its log: the failed test of the changed page. */
export const USAGE_LOG =
  "usage-test\tRun go test ./docs/...\t--- FAIL: TestTheUsagePageListsEveryCommand (0.01s)\n" +
  "usage-test\tRun go test ./docs/...\t    usage_test.go:21: docs/usage.md names 3 commands; `widgets --help` lists 4: add, export, list, remove\n" +
  "usage-test\tRun go test ./docs/...\tFAIL\n";
export const USAGE_FAILED = {
  name: "usage-test",
  checkRun: 9201,
  run: 42,
  suite: 42,
  status: "completed",
  conclusion: "failure",
  log: USAGE_LOG,
};

/** The route gh-standin.ts answers `call` with: the first whose match names it. */
export function routeOf(table: RouteTable, call: string): Route {
  const route = table.routes.find((candidate) => new RegExp(candidate.match, "s").test(call));
  if (route === undefined) throw new Error(`no route names: ${call}`);
  return route;
}
/** The answer gh-standin.ts gives `call` before any `--json` or `--jq`. */
export const answer = (table: RouteTable, call: string) => routeOf(table, call).stdout;
export const notFound = (doc: string) => ({
  stdout: { message: "Not Found", documentation_url: doc, status: "404" },
  stderr: "gh: Not Found (HTTP 404)\n",
  exit: 1,
  status: 404,
});
/** The table rendered from the world as it stands, which most rows read. */
export const plain = renderHeadsRoutes(world());
