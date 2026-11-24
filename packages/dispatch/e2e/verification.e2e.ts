import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Page, TestInfo } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifactText,
  patchIssue,
  putProjectDefaultRepository,
  recordCheckResult,
  retractCheckResult,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import { connectedDot, documentEditor, paste, placeCaret } from "./editor";
import { fakeGithubCheckRunReads, seedFakeGithub } from "./fake-github-helpers";
import { sql } from "./psql";
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
/** Another repository than the issue's, whose pull request the issue links, and that pull
 *  request's head. */
const otherRepository = "acme/infra";
const thirdHead = "3".repeat(40);

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
  await putProjectDefaultRepository("VER", repository);
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

// SLOW(>=2s): 8.3 s, 6.2 s and 5.0 s over three one-worker runs on this pod. The 8.3 s run,
// measured step by step, spends 7.9 s in the row's body. Seeding through the API is 1.5 s, of
// which the issue's create is 0.77 s (0.76 s of it the server's handler, by its log) and no other
// step is over 0.3 s; the two records are 0.37 s, the first asking the fake GitHub about the
// issue's own repository, the linked pull request and the linked repository's commit before
// reading through to it. Then the browser: a fresh context (0.18 s), its first page (0.67 s), the
// load of the tab (1.29 s), the first assertion's wait while the dashboard makes its boot reads
// and reads the document, its results and the linked head's check runs before the check draws
// (2.98 s), the assertion on the other result's commit (0.37 s), the two other assertions
// (0.18 s) and closing the context (0.32 s). The server's log shows every request answered once,
// with no retry, sleep or timeout behind any step.
test("a result at a linked pull request's head stands at it, and one at another commit of that repository names it", async ({
  browser,
}) => {
  // A second repository the issue does not resolve to, which the issue links a pull request in,
  // and two commits of it: that pull request's head, and one no pull request heads.
  const linkedRepository = "acme/widget-tools";
  const linkedHead = "3".repeat(40);
  const linkedOther = "4".repeat(40);
  const issue = await seedIssue("Verification linked repository", secondHead);
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: `https://github.com/${linkedRepository}/pull/9` }],
  });
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [secondHead]: [7] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: secondHead, state: "open", title: `${issue.key} the tab` } },
    },
    [linkedRepository]: {
      commit_pulls: { [linkedHead]: [9] },
      commits: [linkedOther],
      contents: "read",
      installation_id: 257,
      pull_requests: "read",
      pulls: { "9": { head_sha: linkedHead, state: "open", title: "The second half of the work" } },
    },
  });
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-linked-head serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Move the linked pull request's head and this one is left behind."}
A result at the linked pull request's head stands at it.
:::

:::check{#c-linked-other serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Open a pull request at this commit and it stands at its head."}
A result at another commit of the linked repository names that repository.
:::
`,
    name: "verification.md",
  });
  // Neither commit is the issue's own repository's, so each is taken as the linked repository's,
  // whose open pull request the issue links, and the first record's read-through teaches the
  // server that pull request's head.
  await recordCheckResult(issue.key, {
    check: "c-linked-head",
    commit: linkedHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });
  await recordCheckResult(issue.key, {
    check: "c-linked-other",
    commit: linkedOther,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    const checks = page.getByTestId("verification-check");

    // The head of a pull request in another repository reads by GitHub's own name for it, so it
    // reads apart from pull request 9 of the issue's own repository.
    const head = checks.filter({ hasText: "A result at the linked pull request's head" });
    await expect(head.getByTestId("verification-check-compared-with")).toContainText(
      `the head of ${linkedRepository}#9 ${linkedHead}`
    );
    await expect(head.getByTestId("verification-result-commit-stands")).toContainText(
      `At the head of ${linkedRepository}#9 ${linkedHead}`
    );

    // A commit of that repository no compared head is, is another commit of that repository,
    // which the page names rather than leaving a reader to take it for the issue's own.
    const other = checks.filter({ hasText: "A result at another commit of the linked repository" });
    await expect(other.getByTestId("verification-result-commit-stands")).toHaveText(
      `At another commit of ${linkedRepository}`
    );
    await expect(other.getByTestId("verification-result-commit")).toHaveText(linkedOther);
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
A pass and a fail at the head leave the check not known.
:::

:::check{#c-retracted serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Leave the pass alone and it stands."}
A retracted pass stands for nothing.
:::
`,
    name: "verification.md",
  });
  // The tester's pass, then the reviewer's fail at the same commit of a before_merge check. A
  // commit holds one answer, so the two leave the head not known, and a later pass would not end
  // that: only a retraction of the wrong one does.
  const pass = await recordCheckResult(issue.key, {
    check: "c-both",
    commit: secondHead,
    observed: "41 passed, 0 failed",
    role: "tester",
    verdict: "pass",
  });
  const fail = await recordCheckResult(issue.key, {
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
    // check's own answer is read over both of them: a pass and a fail at the one head leave it
    // not known, with no role, and the row of that head names both and what ends that state.
    const both = checks.filter({ hasText: "A pass and a fail at the head leave the check" });
    await expect(both.getByTestId("verification-result")).toHaveCount(2);
    await expect(both.getByTestId("verification-result-verdict")).toHaveText(["Fail", "Pass"]);
    await expect(both.getByTestId("verification-result-role")).toHaveText(["reviewer", "tester"]);
    await expect(both.getByTestId("verification-result-observed")).toHaveText([
      "40 passed, 1 failed",
      "41 passed, 0 failed",
    ]);
    await expect(both).toHaveAttribute("data-pass-stands", "unknown");
    await expect(both.getByTestId("verification-check-pass-stands")).toHaveText(
      "Dispatch cannot say whether a pass stands"
    );
    await expect(both.getByTestId("verification-check-roles")).toHaveCount(0);
    await expect(both.getByTestId("verification-check-head")).toHaveText([
      `#7 at ${secondHead}: not known, as pass ${pass.result.id} and fail ${fail.result.id} count at this commit. A later pass does not end this. A retraction of the wrong one ends it, and so does a change of the check's words. A new head of the pull request gets a state of its own.`,
    ]);

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

/** The lines of the heads the real `dispatch results` prints for issueKey, each without the
 *  indent and the `- ` that start it, in the command's own order. */
async function headLinesPrinted(issueKey: string): Promise<string[]> {
  const home = mkdtempSync(join(tmpdir(), "dispatch-verification-each-head-"));
  try {
    const read = await runDispatch(["results", "--issue", issueKey], home);
    expect(read.code, read.stderr).toBe(0);
    return read.stdout
      .split("\n")
      .filter((line) => /^ {2}- (?:[\w.-]+\/[\w.-]+)?#\d+ at /.test(line))
      .map((line) => line.slice(4));
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
}

/** The rows of the heads the Verification tab of issueKey shows under its one check, read at each
 *  width of widths, a screenshot kept under the name a width carries. At each width the pill stays
 *  and shows the answer of the whole check, no here, and under it is one row for each line of
 *  wanted, in its words. The server lists the heads newest learned first, so the rows are compared
 *  with wanted as a set and answered in the tab's own order. */
async function headRowsShown(
  page: Page,
  testInfo: TestInfo,
  issueKey: string,
  wanted: readonly string[],
  widths: readonly (readonly [width: number, height: number, screenshot?: string])[]
): Promise<string[]> {
  let rows: string[] = [];
  for (const [width, height, screenshot] of widths) {
    await page.setViewportSize({ height, width });
    await page.goto(`/issues/${issueKey}/verification`);
    const check = page.getByTestId("verification-check");
    await expect(check).toHaveCount(1);
    const heads = check.getByTestId("verification-check-head");
    await expect(heads).toHaveCount(wanted.length);
    rows = await heads.allTextContents();
    expect([...rows].sort()).toEqual([...wanted].sort());
    await expect(check).toHaveAttribute("data-pass-stands", "no");
    await expect(check.getByTestId("verification-check-pass-stands")).toHaveText("No pass stands");
    await expect(check.getByTestId("verification-check-merged")).toHaveCount(0);
    if (screenshot !== undefined) {
      await page.screenshot({ fullPage: true, path: testInfo.outputPath(screenshot) });
    }
  }
  return rows;
}

// SLOW(>=2s): a project, its default repository, two open pull requests in the fake GitHub and one
// of another repository the issue links, a Verification document of one check, three recorded
// results, two of them reading through to that fake for their pull request's head, two runs of the
// real `dispatch results`, and three loads of the tab: one at a desktop width over the issue's own
// two heads, then one at a desktop width and one at a phone width over all three.
//
// Each time, the tab is read before the command, and the two heads of the issue's own repository
// are read before the head of the other one is staged. So a build whose tab shows the pill alone
// fails on the rows of the heads, `verification-check-head`, whatever its command prints and
// whether or not it takes a result at a commit of another repository (R17). That is the fault the
// negative control of v-page-each-head names: this file, run at a commit before the rows.
test("the Verification tab shows the state at each open head", async ({ browser }, testInfo) => {
  await createProject({ key: "VER", name: "Verification" });
  await putProjectDefaultRepository("VER", repository);
  const issue = await createIssue({ project: "VER", spec, title: "Each open head" });
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [firstHead]: [7], [secondHead]: [8] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: {
        "7": { head_sha: firstHead, state: "open", title: `${issue.key} the first change` },
        "8": { head_sha: secondHead, state: "open", title: `${issue.key} the second change` },
      },
    },
    [otherRepository]: {
      commit_pulls: { [thirdHead]: [9] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "9": { head_sha: thirdHead, state: "open", title: "Move the deploy job" } },
    },
  });
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-each serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Record the pass at one head alone and no pass stands."}
A pass must stand at each open head.
:::
`,
    name: "verification.md",
  });
  // Each result's record route reads through to the fake for the pull request whose head its
  // commit is, so the server holds both open heads once both are recorded.
  const pass = await recordCheckResult(issue.key, {
    check: "c-each",
    commit: firstHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });
  const fail = await recordCheckResult(issue.key, {
    check: "c-each",
    commit: secondHead,
    failed: 1,
    observed: "40 passed, 1 failed",
    role: "tester",
    verdict: "fail",
  });
  const ownHeads = [
    `#7 at ${firstHead}: yes, from tester. Pass ${pass.result.id} decides it.`,
    `#8 at ${secondHead}: no, as fail ${fail.result.id} counts there.`,
  ];

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // One row for each open head of the issue's own repository, then the command's lines of the
    // same heads, which are the tab's rows in the tab's own order.
    const ownRows = await headRowsShown(page, testInfo, issue.key, ownHeads, [[1440, 900]]);
    expect(await headLinesPrinted(issue.key)).toEqual(ownRows);

    // A pull request of another repository that the issue links by its URL is the issue's (R19),
    // so its head is a head of the check like the others. The harness runs with NATS disabled, so
    // the head is staged as the `pull_request` envelope would have stored it: one
    // `pull_request_heads` row, attributed to the issue by its link. A result at that head is taken
    // with no read of GitHub (R17), and its line and its row name its repository beside its number
    // (R18).
    const infraPull = `https://github.com/${otherRepository}/pull/9`;
    await patchIssue(issue.key, { external_links: [{ url: infraPull }] });
    await sql(
      `INSERT INTO pull_request_heads (repo, number, head_sha, state, issue_key, attribution_source, title, url, checked_at)
       VALUES ('${otherRepository}', 9, '${thirdHead}', 'open', '${issue.key}', 'external_link', 'Move the deploy job', '${infraPull}', now())`
    );
    const infra = await recordCheckResult(issue.key, {
      check: "c-each",
      commit: thirdHead,
      observed: "12 passed",
      role: "tester",
      verdict: "pass",
    });
    const rows = await headRowsShown(
      page,
      testInfo,
      issue.key,
      [
        ...ownHeads,
        `${otherRepository}#9 at ${thirdHead}: yes, from tester. Pass ${infra.result.id} decides it.`,
      ],
      [
        [1440, 900, "verification-each-head-1440.png"],
        [390, 844, "verification-each-head-phone.png"],
      ]
    );
    expect(await headLinesPrinted(issue.key)).toEqual(rows);
  } finally {
    await alice.close();
  }
});

/** The CI run a `ci_check` check below names. */
const ciRun = "ci-ok";

/** Seeds an issue whose Verification document holds one check CI proves, `c-ci`, which names
 *  the CI run `ciRun`, and the issue's open pull request 7 at `secondHead`, whose head carries
 *  the one check run `run` in the fake GitHub. The harness runs with NATS disabled and the
 *  results read only reads the heads Dispatch holds, so the head is staged as the
 *  `pull_request` envelope would have stored it: one `pull_request_heads` row, attributed to the
 *  issue by its title. */
async function seedCICheck(
  title: string,
  run: { conclusion: string; output: { title: string; summary: string } }
) {
  await createProject({ key: "VER", name: "Verification" });
  await putProjectDefaultRepository("VER", repository);
  const issue = await createIssue({ project: "VER", spec, title });
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-ci serves="dispatch://${issue.key}/spec#b-r1" surface="cli" ci_check="${ciRun}" negative_control="Break the list and the run fails."}
Every check of the document is listed.
:::
`,
    name: "verification.md",
  });
  await seedFakeGithub({
    [repository]: {
      check_runs: {
        [secondHead]: [
          {
            check_suite_id: 41,
            completed_at: "2026-11-12T10:00:00Z",
            conclusion: run.conclusion,
            html_url: `https://github.com/${repository}/actions/runs/40/job/9001`,
            id: 9001,
            name: ciRun,
            output: run.output,
            status: "completed",
          },
        ],
      },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: secondHead, state: "open", title: `${issue.key} the tab` } },
    },
  });
  await sql(
    `INSERT INTO pull_request_heads (repo, number, head_sha, state, issue_key, attribution_source, title, checked_at)
     VALUES ('${repository}', 7, '${secondHead}', 'open', '${issue.key}', 'title_body', '${issue.key} the tab', now())`
  );
  return issue;
}

/** Resolves once Dispatch's check states settler has settled every due mark of `issueKey` and
 *  published its states at `head`. Every Dispatch process runs the settler, the head's insert
 *  marks the issue's states owed, and its settle reads the check runs of a check CI proves at the
 *  head as a results read does; it deletes a mark in the transaction that publishes the states
 *  its reads computed. So a count of the fake's reads taken now holds every read of the settler's,
 *  and a results read, which marks nothing, starts no settle after it. */
async function checkStatesSettled(issueKey: string, head: string): Promise<void> {
  await expect
    .poll(async () =>
      sql(
        `SELECT (EXISTS (SELECT 1 FROM check_states WHERE issue_key = '${issueKey}' AND head_sha = '${head}' AND check_block_id = '')
           AND NOT EXISTS (SELECT 1 FROM check_states_owed WHERE issue_key = '${issueKey}' AND due_at <= now()))::text`
      )
    )
    .toBe("true");
}

// SLOW(>=2s): 2.6 s - a project, its default repository, a Verification document, a staged pull
// request head and a seeded fake GitHub, before one load of the tab, whose results read mints a
// token and reads the head's check runs from that fake. The floor the first row of this file
// pays is most of it.
test("a check with ci_check shows the pass its CI run concluded at the issue's open head", async ({
  browser,
}) => {
  const issue = await seedCICheck("CI proves it", {
    conclusion: "success",
    output: { summary: "41 passed", title: "ci-ok passed" },
  });
  // The check states settler reads the head's check runs too: the reads the page causes are
  // counted from once it has settled.
  await checkStatesSettled(issue.key, secondHead);
  const settlerReads = (await fakeGithubCheckRunReads())[repository]?.[secondHead] ?? 0;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const answers: string[] = [];
    page.on("response", (response) => {
      if (new URL(response.url()).pathname === `/api/v1/issues/${issue.key}/results`) {
        answers.push(response.url());
      }
    });
    await page.goto(`/issues/${issue.key}/verification`);
    const check = page.getByTestId("verification-check");
    await expect(check).toHaveCount(1);

    // The check names the run that proves it, and a pass of it stands, from ci.
    await expect(check.getByTestId("verification-check-ci")).toHaveText(ciRun);
    await expect(check).toHaveAttribute("data-pass-stands", "yes");
    await expect(check.getByTestId("verification-check-roles")).toHaveText("from ci");

    // Its one result is the one derived from that run, drawn as a recorded one is.
    const result = check.getByTestId("verification-result");
    await expect(result).toHaveCount(1);
    await expect(result.getByTestId("verification-result-role")).toHaveText("ci");
    await expect(result.getByTestId("verification-result-verdict")).toHaveText("Pass");
    await expect(result.getByTestId("verification-result-run")).toHaveText(ciRun);
    await expect(result.getByTestId("verification-result-observed")).toContainText(
      "conclusion: success"
    );
    await expect(result.getByTestId("verification-result-commit")).toHaveText(secondHead);
    await expect(result.getByTestId("verification-result-commit-stands")).toContainText(
      `At the head of pull request 7 ${secondHead}`
    );

    // Each answer of the results route read the head's check runs once, one page of one run.
    await expect
      .poll(
        async () =>
          ((await fakeGithubCheckRunReads())[repository]?.[secondHead] ?? 0) - settlerReads ===
          answers.length
      )
      .toBe(true);
    expect(answers.length).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 2.6 s - the same seeding and the same one load of the tab as the row above, with
// the fake's run skipped rather than passed.
test("a check with ci_check whose CI run was skipped says its run proved nothing", async ({
  browser,
}) => {
  const issue = await seedCICheck("CI skipped it", {
    conclusion: "skipped",
    output: { summary: "", title: "" },
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    const check = page.getByTestId("verification-check");
    await expect(check.getByTestId("verification-check-ci")).toHaveText(ciRun);
    await expect(check.getByTestId("verification-check-ci-unproved")).toHaveText(
      `CI run ${ciRun} concluded skipped at ${secondHead}, the head of ${repository}#7, which proves nothing.`
    );
    await expect(check).toHaveAttribute("data-pass-stands", "no");
    await expect(check.getByTestId("verification-result")).toHaveCount(0);
    await expect(check.getByTestId("verification-check-no-results")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

/** The four check blocks a person can paste that each break one rule of a check CI proves, by
 *  block id, with the rule's own words. `c-old` reuses the id of a check that held a recorded
 *  result, which was deleted from the document before the paste. */
const brokenCIChecks = [
  {
    block: `:::check{#c-run ci_check="${ciRun}" run="bun test web/src" negative_control="Break it and the run fails."}\nA check CI proves that names a run.\n:::\n`,
    id: "c-run",
    rule: "a check proved by CI carries no run",
  },
  {
    block: `:::check{#c-when ci_check="${ciRun}" when="after_merge" negative_control="Break it and the run fails."}\nA check CI proves after the merge.\n:::\n`,
    id: "c-when",
    rule: "ci_check takes when of before_merge",
  },
  {
    block: `:::check{#c-control ci_check="${ciRun}"}\nA check CI proves with no negative control.\n:::\n`,
    id: "c-control",
    rule: "a check proved by CI names its negative control",
  },
  {
    block: `:::check{#c-old ci_check="${ciRun}" negative_control="Break it and the run fails."}\nA check that already holds a result.\n:::\n`,
    id: "c-old",
    rule: "this check already holds a recorded result, so it does not become CI's; write a new check block under a new id and delete this one",
  },
] as const;

// SLOW(>=2s): 4.6 s - a project, its default repository, a pull request in the fake GitHub, a
// Verification document, a recorded result reading through to that fake and a block deletion,
// then a load of the document that opens its live room, the paste, the room's settlement and its
// repair reaching the editor, and a second load, of the Activity tab.
// The live editor writes through the document socket, which refuses no update, so the four rules
// reach a browser's write at the room's settlement: it deletes the `ci_check` of each block that
// breaks one and posts one block.invalid naming the block and the rule, which the issue's
// Activity reads as the refusal. A valid check pasted on a fresh id keeps its `ci_check`.
test("a pasted check block whose ci_check breaks a rule loses it, and the Activity names the rule", async ({
  browser,
}) => {
  const issue = await seedIssue("CI refusals", secondHead);
  const created = await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-old serves="dispatch://${issue.key}/spec#b-r1" surface="cli" run="bun test web/src" negative_control="Break the list and the run fails."}
A check a role ran.
:::

End.
`,
    name: "verification.md",
  });
  const artifactId = created.artifact.id;
  await recordCheckResult(issue.key, {
    check: "c-old",
    commit: secondHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });
  await editArtifact(artifactId, { ops: [{ block: "c-old", op: "delete" }] });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/artifacts/${created.artifact.slug}`);
    await expect(connectedDot(page)).toHaveText("connected");
    await placeCaret(page, "after", "End.");
    const valid = `:::check{#c-fresh ci_check="${ciRun}" negative_control="Break it and the run fails."}\nA check CI proves under a new id.\n:::\n`;
    await paste(page, {
      html: "",
      text: [...brokenCIChecks.map(({ block }) => block), valid].join("\n"),
    });

    // Once the room has settled, the stored document holds all five blocks, and only the valid
    // one carries a ci_check.
    await expect
      .poll(async () => {
        const { markdown } = await getArtifactText(artifactId);
        const blocks = [...markdown.matchAll(/^:::check\{#([a-z-]+)([^}]*)\}/gm)];
        return Object.fromEntries(blocks.map(([, id, rest]) => [id, rest?.includes("ci_check")]));
      })
      .toEqual({
        "c-control": false,
        "c-fresh": true,
        "c-old": false,
        "c-run": false,
        "c-when": false,
      });

    // The editor shows the same: no broken block draws a ci_check, and the valid one does.
    const editor = documentEditor(page);
    const ciCheckOf = (id: string) =>
      editor.locator(`[data-block-id="${id}"] [data-proof-block-attribute="ci_check"]`);
    for (const { id } of brokenCIChecks) {
      await expect(editor.locator(`[data-block-id="${id}"]`)).toBeVisible();
      await expect(ciCheckOf(id)).toHaveCount(0);
    }
    await expect(ciCheckOf("c-fresh")).toHaveText(ciRun);

    // The Activity names each refusal, by block and rule, and none for the valid block.
    await page.goto(`/issues/${issue.key}/activity`);
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    for (const { id, rule } of brokenCIChecks) {
      await expect(rows.filter({ hasText: `refused check ${id}: ${rule}` })).toHaveCount(1);
    }
    await expect(rows.filter({ hasText: "refused check c-fresh" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
