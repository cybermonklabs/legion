import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, CheckBasis, CheckBlock, CheckResult } from "../../api/types";
import { VerificationTab } from "./VerificationTab";

const commit = "0123456789abcdef0123456789abcdef01234567";
const otherCommit = "89abcdef0123456789abcdef0123456789abcdef";

/** The head of the pull request the server compared the results with, which is the commit the
 *  results below were recorded at. */
const head: CheckBasis = { commit, kind: "pull_request", pull_request: 7 };

function result(overrides: Partial<CheckResult> = {}): CheckResult {
  return {
    actor: { id: "alice", kind: "user" },
    check_artifact_id: "artifact-1",
    check_block_id: "c1",
    check_issue_key: "CORE-1",
    check_sha256: "sha256",
    check_version: 2,
    check_words_moved: false,
    commit_sha: commit,
    commit_stands: { answer: "at_basis", basis: head },
    created_at: "2026-10-20T10:00:00Z",
    excluded: 0,
    failed: 0,
    id: "result-1",
    issue_key: "CORE-1",
    negative_control: "Removed the panel and the check failed.",
    observed: "41 passed, 0 failed",
    role: "tester",
    run: "bun test web/src",
    screenshots: [],
    session: "session-1",
    skipped: 0,
    surface: "cli",
    verdict: "pass",
    ...overrides,
  };
}

function check(blockId: string, results: CheckResult[], overrides: Partial<CheckBlock> = {}) {
  return {
    artifact_id: "artifact-1",
    block_id: blockId,
    compared_with: [head],
    issue_key: "CORE-1",
    negative_control: "Delete the tab and the check fails.",
    pass_stands: { answer: "yes", roles: ["tester"] },
    ref: `dispatch://CORE-1/artifact/verification-md#b-${blockId}`,
    results,
    run: "bun test web/src",
    sha256: "sha256",
    surface: "cli",
    text: `Check ${blockId}`,
    version: 2,
    when: "before_merge",
    ...overrides,
  } satisfies CheckBlock;
}

const verification: Artifact = {
  created_at: "2026-10-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "verification.md",
  primary: false,
  project: "CORE",
  role: "verification",
  ref_key: "CORE-1/verification-md",
  session_id: null,
  slug: "verification-md",
  versions: [],
};

function renderTab(artifact: Artifact | undefined, checks: CheckBlock[]) {
  const getIssueResults = spyOn(api, "getIssueResults").mockResolvedValue({
    checks,
    issue: "CORE-1",
    verification_artifact_id: artifact?.id ?? "",
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <VerificationTab artifact={artifact} issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getIssueResults, view };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

// The gate's question for each check, which the server answers over every result of it: a
// pass stands, no pass stands, or Dispatch could not read the repository to say.
test("each check reads the server's answer over every result of it", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c-yes", [result()]),
    check("c-no", [result({ failed: 1, id: "result-2", verdict: "fail" })], {
      pass_stands: { answer: "no" },
    }),
    check("c-unknown", [result({ id: "result-3" })], {
      compared_nothing: "repository_unreadable",
      compared_with: [],
      pass_stands: { answer: "unknown" },
    }),
    check("c-none", [], { pass_stands: { answer: "no" } }),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("verification-checks");
  expect(
    screen.getAllByTestId("verification-check").map((row) => row.getAttribute("data-pass-stands"))
  ).toEqual(["yes", "no", "unknown", "no"]);
  expect(
    screen.getAllByTestId("verification-check-pass-stands").map((pill) => pill.textContent)
  ).toEqual([
    "A pass stands",
    "No pass stands",
    "Dispatch cannot say whether a pass stands",
    "No pass stands",
  ]);
  // Only a check whose pass stands names the roles it stands from.
  expect(screen.getAllByTestId("verification-check-roles").map((row) => row.textContent)).toEqual([
    "from tester",
  ]);
  // The check with no result says so where its results would be.
  expect(screen.getByTestId("verification-check-no-results").textContent).toBe(
    "No result recorded against this check."
  );
});

// R9: no state of a check is read from its newest result alone. The newest result of this
// check is a fail, and the pass recorded before it still stands at the head, so the check
// reads that a pass stands and names the role whose pass it is.
test("a check whose newest result failed still reads the pass that stands", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c1", [
      result({ failed: 2, id: "result-new", role: "reviewer", verdict: "fail" }),
      result({ id: "result-old" }),
    ]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("verification-checks");
  expect(screen.getByTestId("verification-check-pass-stands").textContent).toBe("A pass stands");
  expect(screen.getByTestId("verification-check-roles").textContent).toBe("from tester");
  // Both results are drawn, in the order the server served them, each with its own role.
  const rows = screen.getAllByTestId("verification-result");
  expect(
    rows.map((row) => within(row).getByTestId("verification-result-role").textContent)
  ).toEqual(["reviewer", "tester"]);
  expect(
    rows.map((row) => within(row).getByTestId("verification-result-verdict").textContent)
  ).toEqual(["Fail", "Pass"]);
});

// The two facts of R4, kept apart: where the result's commit stands, and whether the check's
// words moved after it was recorded, with both versions as the reader's handles.
test("a result says where its commit stands and which words it was recorded against", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c1", [
      result(),
      result({
        check_version: 1,
        check_words_moved: true,
        commit_sha: otherCommit,
        commit_stands: { answer: "other_commit" },
        id: "result-2",
      }),
      result({
        commit_stands: { answer: "unknown", reason: "read_failed" },
        id: "result-3",
      }),
    ]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("verification-checks");
  const rows = screen.getAllByTestId("verification-result");
  expect(
    rows.map((row) => within(row).getByTestId("verification-result-commit-stands").textContent)
  ).toEqual([
    `At the head of pull request 7 ${commit}`,
    "At another commit",
    "Not known. A read Dispatch made did not answer.",
  ]);
  expect(
    rows.map((row) => within(row).getByTestId("verification-result-check-words").textContent)
  ).toEqual([
    "This check's words (version 2)",
    "An older wording of this check (version 1, the check is at 2)",
    "This check's words (version 2)",
  ]);
  // The commit itself is whole: 40 characters, never cut.
  expect(
    rows.map((row) => within(row).getByTestId("verification-result-commit").textContent)
  ).toEqual([commit, otherCommit, commit]);
});

test("a retracted result shows as retracted with its reason", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check(
      "c1",
      [
        result({
          retracted: {
            actor: { id: "alice", kind: "user" },
            created_at: "2026-10-21T10:00:00Z",
            reason: "The run was against the wrong branch.",
          },
        }),
      ],
      { pass_stands: { answer: "no" } }
    ),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const row = await screen.findByTestId("verification-result");
  expect(within(row).getByTestId("verification-result-retracted").textContent).toBe("Retracted");
  expect(within(row).getByTestId("verification-result-retraction").textContent).toContain(
    "Retracted: The run was against the wrong branch."
  );
  // The result itself is unchanged: it still says it passed, at the commit it ran at.
  expect(within(row).getByTestId("verification-result-verdict").textContent).toBe("Pass");
  expect(screen.getByTestId("verification-check-pass-stands").textContent).toBe("No pass stands");
});

test("a check names what the server compared its results with", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c-branch", [result()], {
      compared_with: [
        { branch: "main", kind: "branch" },
        {
          commit,
          kind: "pull_request",
          learned_at: { at: "2026-10-20T09:00:00Z", kind: "read_at" },
          pull_request: 7,
        },
      ],
    }),
    check("c-nothing", [result({ id: "result-2" })], {
      compared_nothing: "no_pull_request_or_branch",
      compared_with: [],
      pass_stands: { answer: "no" },
    }),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("verification-checks");
  const compared = screen
    .getAllByTestId("verification-check-compared-with")
    .map((row) => row.textContent);
  expect(compared[0]).toContain(`branch main, the head of pull request 7 ${commit}`);
  // A head carries when Dispatch learned it, so a comparison older than a push reads as one.
  expect(compared[0]).toContain("Dispatch read it");
  // A check that compared with nothing says why instead of showing a blank.
  expect(compared[1]).toBe("This issue holds no pull request and no branch.");
});

test("a result shows how it ran, what was seen, its counts, its role and its screenshots", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c1", [
      result({
        excluded: 2,
        failed: 3,
        screenshots: ["dispatch://CORE-1/artifact/shot-png"],
        skipped: 1,
        verdict: "fail",
      }),
    ]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const row = await screen.findByTestId("verification-result");
  const field = (name: string) =>
    within(row).getByTestId(`verification-result-${name}`).textContent;
  expect(field("verdict")).toBe("Fail");
  expect(field("role")).toBe("tester");
  expect(field("surface")).toBe("cli");
  expect(field("run")).toBe("bun test web/src");
  expect(field("observed")).toBe("41 passed, 0 failed");
  expect(field("commit")).toBe(commit);
  expect(field("negative-control")).toBe("Removed the panel and the check failed.");
  expect([field("failed"), field("skipped"), field("excluded")]).toEqual(["3", "1", "2"]);
  expect(within(row).getByTestId("verification-result-screenshot").getAttribute("href")).toBe(
    "/issues/CORE-1/artifacts/shot-png"
  );
  expect(within(row).queryByTestId("verification-result-retracted")).toBeNull();
});

// The reading is the record this page exists for, so a result that carries none says so
// rather than drawing a blank.
test("a result with no reading says it carries none", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c1", [result({ observed: "" })]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const row = await screen.findByTestId("verification-result");
  expect(within(row).getByTestId("verification-result-observed").textContent).toBe(
    "This result carries no reading."
  );
});

test("an issue with no Verification document shows the command that makes one", () => {
  const { getIssueResults, view } = renderTab(undefined, []);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const empty = screen.getByTestId("verification-empty");
  expect(empty.textContent).toContain("CORE-1 has no Verification document.");
  expect(empty.textContent).toContain("dispatch artifact --issue CORE-1 --name verification.md");
  // Nothing is read for an issue that has no such document.
  expect(getIssueResults).not.toHaveBeenCalled();
});
