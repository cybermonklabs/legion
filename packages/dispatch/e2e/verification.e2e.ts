import {
  createIssue,
  createIssueArtifact,
  createProject,
  putRepoProject,
  recordCheckResult,
  retractCheckResult,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { expect, test } from "./test";
import { asUser } from "./users";

// A result is compared with the head of the pull request Dispatch holds for the check's
// issue. The browser harness runs with NATS disabled, so no `pull_request` envelope can reach
// the server to teach it that head; the record route's own read-through does it instead
// (`readThroughPullRequests`), asking the fake GitHub which pull requests name the result's
// commit and storing that pull request's head. Re-seeding the fake with a later head and
// recording another result moves the head exactly as a push does, which is how a row below
// leaves a result behind at a commit the head has moved past.
const repository = "acme/widgets";
const firstHead = "1".repeat(40);
const secondHead = "2".repeat(40);

/** The spec the checks serve. A `serves` pointer is refused unless it names a block that
 *  exists, so the requirements are written with the issue, before any check points at one. */
const spec = `:::requirement{#r1}
Every check of the Verification document is listed.
:::

:::requirement{#r2}
A check says whether a pass of it stands.
:::
`;

/** Seeds the project and its default repository, the issue and its spec, and the pull request
 *  the results are measured against, whose head is `head` and whose title attributes it to the
 *  issue. */
async function seedIssue(title: string, head: string) {
  await createProject({ key: "VER", name: "Verification" });
  await putRepoProject(repository, { default: true, project: "VER" });
  const issue = await createIssue({ project: "VER", spec, title });
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [head]: [7] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: head, state: "open", title: `${issue.key} the tab` } },
    },
  });
  return issue;
}

// SLOW(>=2s): 2.3 s - a project, its default repository, a pull request in the fake GitHub, a
// Verification document of three checks and two recorded results, each result's record route
// reading through to that fake for the pull request's head, before one load of the tab, which
// opens the document's live room and draws all three checks with every field of both results.
// @pr: The Verification tab's own row: every check of the document, each with the results recorded against it.
test("the Verification tab lists every check of the document, each with the results recorded against it @pr", async ({
  browser,
}, testInfo) => {
  const issue = await seedIssue("Verification tab", secondHead);
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-passed serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/verification.e2e.ts" negative_control="Draw no checks and the row fails."}
The tab lists every check of the document.
:::

:::check{#c-failed serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Record no result and the check reads no result."}
A check whose one result failed holds no pass that stands.
:::

:::check{#c-none serves="dispatch://${issue.key}/spec#b-r2" surface="api" run="curl the results route" negative_control="Record a result and the check stops reading no result."}
A check nobody has run yet reads no result.
:::
`,
    name: "verification.md",
  });
  await recordCheckResult(issue.key, {
    check: "c-passed",
    commit: secondHead,
    negative_control: "Removed the list and the run failed.",
    observed: "3 checks listed",
    role: "tester",
    run: "bun run e2e -- e2e/verification.e2e.ts",
    surface: "browser",
    verdict: "pass",
  });
  await recordCheckResult(issue.key, {
    check: "c-failed",
    commit: secondHead,
    failed: 2,
    negative_control: "Fixed the panel and the run passed.",
    observed: "2 failed",
    role: "tester",
    run: "bun test web/src",
    surface: "cli",
    verdict: "fail",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    await expect(page.getByTestId("verification-tab")).toBeVisible();

    // Every check of the document, in document order, whatever anyone has recorded against it.
    const checks = page.getByTestId("verification-check");
    await expect(checks).toHaveCount(3);
    await expect(checks.getByTestId("verification-check-pass-stands")).toHaveText([
      "A pass stands",
      "No pass stands",
      "No pass stands",
    ]);

    // The result recorded through the API sits under its own check, and a pass of that check
    // stands, from the role that recorded it.
    const passed = checks.filter({ hasText: "The tab lists every check of the document." });
    await expect(passed).toHaveAttribute("data-pass-stands", "yes");
    await expect(passed.getByTestId("verification-check-roles")).toHaveText("from tester");
    await expect(passed.getByTestId("verification-check-when")).toHaveText("before_merge");
    await expect(passed.getByTestId("verification-check-surface")).toHaveText("browser");
    const result = passed.getByTestId("verification-result");
    await expect(result).toHaveCount(1);
    await expect(result.getByTestId("verification-result-verdict")).toHaveText("Pass");
    // The commit the result ran at is the pull request's head, which is what the check
    // compared with, and the page prints that commit whole.
    await expect(result.getByTestId("verification-result-commit-stands")).toContainText(
      `At the head of pull request 7 ${secondHead}`
    );
    await expect(result.getByTestId("verification-result-check-words")).toHaveText(
      /^This check's words \(version \d+\)$/
    );
    await expect(result.getByTestId("verification-result-role")).toHaveText("tester");
    await expect(result.getByTestId("verification-result-surface")).toHaveText("browser");
    await expect(result.getByTestId("verification-result-observed")).toHaveText("3 checks listed");
    await expect(result.getByTestId("verification-result-commit")).toHaveText(secondHead);
    await expect(result.getByTestId("verification-result-negative-control")).toHaveText(
      "Removed the list and the run failed."
    );
    await expect(result.getByTestId("verification-result-failed")).toHaveText("0");

    // A check that holds one fail answers that no pass of it stands, and counts what that run
    // counted.
    const failed = checks.filter({ hasText: "A check whose one result failed holds no pass" });
    await expect(failed).toHaveAttribute("data-pass-stands", "no");
    await expect(failed.getByTestId("verification-result-verdict")).toHaveText("Fail");
    await expect(failed.getByTestId("verification-result-failed")).toHaveText("2");

    // A check nobody has run says so instead of showing an empty list.
    const none = checks.filter({ hasText: "A check nobody has run yet reads no result." });
    await expect(none).toHaveAttribute("data-pass-stands", "no");
    await expect(none.getByTestId("verification-check-no-results")).toHaveText(
      "No result recorded against this check."
    );
    await expect(none.getByTestId("verification-result")).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("verification-checks.png") });
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 1.9 s here, over the bar on the four-worker report this rule is read against, and
// this pod puts it over in a third of runs. Two results recorded against the document's two
// checks, each one's record route reading through to the fake GitHub for the pull request's
// head, with the fake re-seeded between them so the first result is left behind; then one
// load of the Verification tab. Most of it is the floor every row of these files pays,
// measured on this row: a fresh Chromium context (0.25 s), the cold parse of the dashboard
// bundle a fresh context cannot cache (0.40 s) and the dashboard's own fifteen boot reads
// before the first check draws (0.80 s). The row's own seven assertions are 0.26 s of it.
// INFRA-1109 holds that floor; nothing in the row is removable.
test("a result the pull request has moved past stands at no commit the check compared with", async ({
  browser,
}) => {
  const issue = await seedIssue("Verification comparison", firstHead);
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-past serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Keep the head and the result stands at it."}
A result the head has moved past stands at another commit.
:::

:::check{#c-head serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Move the head again and this one is left behind too."}
A result at the head is a pass that stands.
:::
`,
    name: "verification.md",
  });
  // Recorded at the head as it stands, so the read-through stores that head and the result is
  // at what the check compares with the moment it is written.
  await recordCheckResult(issue.key, {
    check: "c-past",
    commit: firstHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });

  // The work moves on: the pull request's head is now a later commit, and the next result's
  // read-through teaches the server that head.
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [secondHead]: [7] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: secondHead, state: "open", title: `${issue.key} the tab` } },
    },
  });
  await recordCheckResult(issue.key, {
    check: "c-head",
    commit: secondHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    const checks = page.getByTestId("verification-check");

    const past = checks.filter({ hasText: "A result the head has moved past" });
    await expect(past).toHaveAttribute("data-pass-stands", "no");
    await expect(past.getByTestId("verification-check-pass-stands")).toHaveText("No pass stands");
    // The result itself still says it passed; what the page says is that its commit is not one
    // of the commits the check compared with, and the whole commit is there to read.
    await expect(past.getByTestId("verification-result-verdict")).toHaveText("Pass");
    await expect(past.getByTestId("verification-result-commit-stands")).toHaveText(
      "At another commit"
    );
    await expect(past.getByTestId("verification-result-commit")).toHaveText(firstHead);

    const head = checks.filter({ hasText: "A result at the head is a pass that stands." });
    await expect(head).toHaveAttribute("data-pass-stands", "yes");
    await expect(head.getByTestId("verification-check-compared-with")).toContainText(
      `the head of pull request 7 ${secondHead}`
    );
    await expect(head.getByTestId("verification-result-commit-stands")).toContainText(
      `At the head of pull request 7 ${secondHead}`
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 2.1 s - a project, its default repository, a pull request in the fake GitHub, a
// Verification document of two checks, three recorded results and one retraction, each record
// route reading through to that fake for the pull request's head, before one load of the tab.
// The floor the row above measures is most of it.
test("a check is read over every result of it, never over its newest result alone", async ({
  browser,
}) => {
  const issue = await seedIssue("Every result", secondHead);
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-both serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Record the fail alone and no pass stands."}
A pass at the head stands under a later fail at the same head.
:::

:::check{#c-retracted serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Leave the pass alone and it stands."}
A retracted pass stands for nothing.
:::
`,
    name: "verification.md",
  });
  // The tester's pass, then the reviewer's fail at the same commit. The fail is the newest
  // result of the check, and the tester's pass stands at the head all the same.
  await recordCheckResult(issue.key, {
    check: "c-both",
    commit: secondHead,
    observed: "41 passed, 0 failed",
    role: "tester",
    verdict: "pass",
  });
  await recordCheckResult(issue.key, {
    check: "c-both",
    commit: secondHead,
    failed: 1,
    observed: "40 passed, 1 failed",
    role: "reviewer",
    verdict: "fail",
  });
  const retract = await recordCheckResult(issue.key, {
    check: "c-retracted",
    commit: secondHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });
  await retractCheckResult(retract.result.id, {
    reason: "The run was against the wrong branch.",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    const checks = page.getByTestId("verification-check");

    // Both results are drawn, newest first, each with the reading its role recorded. The
    // check's own answer is read over both of them: the tester's pass stands at the head, and
    // the reviewer's later fail takes nothing away from it.
    const both = checks.filter({ hasText: "A pass at the head stands under a later fail" });
    await expect(both.getByTestId("verification-result")).toHaveCount(2);
    await expect(both.getByTestId("verification-result-verdict")).toHaveText(["Fail", "Pass"]);
    await expect(both.getByTestId("verification-result-role")).toHaveText(["reviewer", "tester"]);
    await expect(both.getByTestId("verification-result-observed")).toHaveText([
      "40 passed, 1 failed",
      "41 passed, 0 failed",
    ]);
    await expect(both).toHaveAttribute("data-pass-stands", "yes");
    await expect(both.getByTestId("verification-check-pass-stands")).toHaveText("A pass stands");
    await expect(both.getByTestId("verification-check-roles")).toHaveText("from tester");

    // The one pass of the other check is retracted, so no pass of it stands. The result is
    // neither changed nor hidden: it shows as retracted, with the reason it is wrong.
    const gone = checks.filter({ hasText: "A retracted pass stands for nothing." });
    await expect(gone).toHaveAttribute("data-pass-stands", "no");
    await expect(gone.getByTestId("verification-check-pass-stands")).toHaveText("No pass stands");
    await expect(gone.getByTestId("verification-result-retracted")).toHaveText("Retracted");
    await expect(gone.getByTestId("verification-result-retraction")).toContainText(
      "Retracted: The run was against the wrong branch."
    );
    await expect(gone.getByTestId("verification-result-verdict")).toHaveText("Pass");
    await expect(gone.getByTestId("verification-result-commit")).toHaveText(secondHead);
  } finally {
    await alice.close();
  }
});
