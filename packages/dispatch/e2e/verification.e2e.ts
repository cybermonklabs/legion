import {
  EMPTY_STATE_LINES,
  NODE_FACT_LABELS,
  NODE_STATE_WORDS,
} from "../web/src/features/issue/page-words";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  putRepoProject,
  recordCheckResult,
  retractCheckResult,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  blockNodeId,
  branchMoreRows,
  type DrawnFact,
  drawnRows,
  factsOf,
  gotoIssue,
  nodeRow,
  rowBody,
  rowsOfType,
  unfoldEveryBranch,
  visibleText,
} from "./issue-page";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

// The issue's checks, on the Checks preset of the redesigned issue page (dispatch://INFRA-1196):
// each check of the Verification document is a node of the issue's tree, with its state told by
// a word and a shape, its own attributes in its body, and each result recorded against it a node
// under it carrying what that run recorded. Until 2026-11-06 this file read the Verification
// tab's list of cards (`features/verification/VerificationTab.tsx`, `[data-testid=
// "verification-check"]`), which drew every check a second time under the document; the tree
// replaced both the tab and its list, so every row below reads the same facts off the tree.
//
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

/** The Verification document's slug, which is how its blocks are addressed on the page. */
const verification = "verification-md";

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

/** The whole text of one row's body, which is where a node's facts are drawn: a body opens
 *  because the keyboard focus is on its row (R2). */
async function bodyOf(page: Page, id: string): Promise<string> {
  const row = nodeRow(page, id).first();
  await row.focus();
  return (await rowBody(row).innerText()).replace(/\s+/g, " ");
}

/** The facts one row's body draws, label against value (`e2e/issue-page.ts`'s `factsOf`): what
 *  a result counted is a `dt`/`dd` pair with nothing between them, so the body's own text
 *  cannot say whether a count is drawn at all or what it reads. */
async function factsIn(page: Page, id: string): Promise<DrawnFact[]> {
  const row = nodeRow(page, id).first();
  await row.focus();
  return await factsOf(rowBody(row));
}

/** Opens every branch the fold left shut and then every branch it left as one row, which is
 *  where a check's results sit: the page opens folded to the frontier (R1), so a check's own
 *  branch is shut until a reader opens it, and a check is the third drawn level under the
 *  issue, so what it holds then folds to a row saying how many they are until the reader opens
 *  that too. */
async function openResults(page: Page): Promise<void> {
  await unfoldEveryBranch(page);
  for (let opened = 0; opened < 8 && (await branchMoreRows(page).count()) > 0; opened += 1) {
    await branchMoreRows(page).first().getByRole("button").first().click();
  }
}

// SLOW(>=2s): 2.3 s - a project, its default repository, a pull request in the fake GitHub, a
// Verification document of three checks and two recorded results, each result's record route
// reading through to that fake for the pull request's head, before one load of the page, which
// opens the document's live room and draws all three checks with every field of both results.
// @pr: The Checks preset's own row: every check of the document, each with the results recorded against it.
test("the Checks preset draws every check of the document, each with the results recorded against it @pr", async ({
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
    await gotoIssue(page, `/issues/${issue.key}/verification`);
    // The page opens folded to the frontier (R1): a requirement whose every check has passed
    // holds nothing open, so the check under it is a twisty away. The reader opens what the
    // fold shut, and the preset then draws every check of the document.
    await unfoldEveryBranch(page);

    // Every check of the document, each drawn once, with the state its results give it.
    await expect(rowsOfType(page, "check")).toHaveCount(3);
    const checks = (await drawnRows(page)).filter((row) => row.type === "check");
    expect(checks.map((row) => row.id)).toEqual([
      blockNodeId(verification, "c-passed"),
      blockNodeId(verification, "c-failed"),
      blockNodeId(verification, "c-none"),
    ]);
    expect(checks.map((row) => row.state)).toEqual(["passed", "failed", "no-result"]);
    expect(checks.map((row) => row.stateWord)).toEqual([
      NODE_STATE_WORDS.check.passed,
      NODE_STATE_WORDS.check.failed,
      NODE_STATE_WORDS.check["no-result"],
    ]);

    // The check's own attributes are in its body, with `when` as words.
    const passed = await bodyOf(page, blockNodeId(verification, "c-passed"));
    expect(passed).toContain("before the merge");
    expect(passed).toContain("browser");
    expect(passed).toContain("bun run e2e -- e2e/verification.e2e.ts");
    expect(passed).toContain("Draw no checks and the row fails.");

    // The result recorded through the API is a node under its own check, and says what it saw.
    await openResults(page);
    const results = (await drawnRows(page)).filter((row) => row.type === "result");
    expect(results.map((row) => row.firstLine)).toEqual(["3 checks listed", "2 failed"]);
    const current = await bodyOf(page, results[0]?.id ?? "");
    expect(current).toContain("Pass");
    // The commit the result ran at is the pull request's head, which is what the check
    // compared with, so the result still stands; the page draws that commit whole.
    expect(current).toContain("Current");
    expect(current).toContain("tester");
    expect(current).toContain("browser");
    expect(current).toContain(secondHead);
    expect(current).toContain("Removed the list and the run failed.");
    // A run that counted nothing draws no count at all, where the old tab drew three zeroes.
    // Read as facts: a `dt` "failed" over a `dd` "0" would leave the body's text saying
    // "failed 0" with no colon in it, which no reading of that text can tell from a body
    // drawing no count.
    const currentFacts = await factsIn(page, results[0]?.id ?? "");
    for (const label of [
      NODE_FACT_LABELS.failed,
      NODE_FACT_LABELS.skipped,
      NODE_FACT_LABELS.excluded,
    ]) {
      expect(
        currentFacts.map((fact) => fact.label),
        `the passing result draws ${label}, which that run never counted`
      ).not.toContain(label);
    }

    // A check that holds one fail holds no pass that stands, and the result counts what that
    // run counted: the count is the `dd` after the `dt` that reads "failed", and not the "2
    // failed" the result's own observed text says.
    const failed = await bodyOf(page, results[1]?.id ?? "");
    expect(failed).toContain("Fail");
    const failedFacts = await factsIn(page, results[1]?.id ?? "");
    expect(
      failedFacts.filter((fact) => fact.label === NODE_FACT_LABELS.failed),
      "the failing result draws no count of what failed"
    ).toEqual([{ label: NODE_FACT_LABELS.failed, value: "2" }]);

    // A check nobody has run says so in its state and has no result under it.
    const none = (await drawnRows(page)).find(
      (row) => row.id === blockNodeId(verification, "c-none")
    );
    expect(none?.stateWord).toBe(NODE_STATE_WORDS.check["no-result"]);
    expect(none?.expanded).toBeNull();
    await page.screenshot({ path: testInfo.outputPath("checks-preset.png") });
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 1.9 s here, over the bar on the four-worker report this rule is read against, and
// this pod puts it over in a third of runs. Two results recorded against the document's two
// checks, each one's record route reading through to the fake GitHub for the pull request's
// head, with the fake re-seeded between them so the first result is left behind; then one load
// of the issue page. Most of it is the floor every row of these files pays, measured on this
// row: a fresh Chromium context (0.25 s), the cold parse of the dashboard bundle a fresh
// context cannot cache (0.40 s) and the dashboard's own fifteen boot reads before the first
// check draws (0.80 s). INFRA-1109 holds that floor; nothing in the row is removable.
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
    await gotoIssue(page, `/issues/${issue.key}/verification`);
    // The check that passed at the head is behind the fold, as the row above this one says.
    await unfoldEveryBranch(page);
    const checks = (await drawnRows(page)).filter((row) => row.type === "check");
    const past = checks.find((row) => row.id === blockNodeId(verification, "c-past"));
    expect(past?.state).toBe("older");
    expect(past?.stateWord).toBe(NODE_STATE_WORDS.check.older);
    const head = checks.find((row) => row.id === blockNodeId(verification, "c-head"));
    expect(head?.state).toBe("passed");

    // The result itself still says it passed; what changed is that its commit is not one of
    // the commits the check compared with, so it no longer speaks for the work as it stands.
    await openResults(page);
    const results = (await drawnRows(page)).filter((row) => row.type === "result");
    const aged = await bodyOf(page, results[0]?.id ?? "");
    expect(aged).toContain("Pass");
    expect(aged).toContain("Older");
    expect(aged).toContain(firstHead);
    const atHead = await bodyOf(page, results[1]?.id ?? "");
    expect(atHead).toContain("Current");
    expect(atHead).toContain(secondHead);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 2.1 s - a project, its default repository, a pull request in the fake GitHub, a
// Verification document of two checks, three recorded results and one retraction, each record
// route reading through to that fake for the pull request's head, before one load of the page.
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
    await gotoIssue(page, `/issues/${issue.key}/verification`);
    await openResults(page);
    const rows = await drawnRows(page);

    // Both results are nodes under their check, newest first, each saying what its run saw.
    // The check's own state is read over both of them: the tester's pass stands at the head,
    // and the reviewer's later fail takes nothing away from it.
    const both = rows.find((row) => row.id === blockNodeId(verification, "c-both"));
    expect(both?.state).toBe("passed");
    expect(both?.stateWord).toBe(NODE_STATE_WORDS.check.passed);
    const results = rows.filter((row) => row.type === "result");
    expect(results.map((row) => row.firstLine)).toEqual([
      "40 passed, 1 failed",
      "41 passed, 0 failed",
      "41 passed",
    ]);
    expect(
      (await factsIn(page, results[0]?.id ?? "")).filter(
        (fact) => fact.label === NODE_FACT_LABELS.role
      )
    ).toEqual([{ label: NODE_FACT_LABELS.role, value: "reviewer" }]);
    expect(
      (await factsIn(page, results[1]?.id ?? "")).filter(
        (fact) => fact.label === NODE_FACT_LABELS.role
      )
    ).toEqual([{ label: NODE_FACT_LABELS.role, value: "tester" }]);

    // The one pass of the other check is retracted, so no pass of it stands. What the row says
    // is "Retracted": the check neither passed nor failed, and a reader who was shown "Failed"
    // could not tell a run that found a fault from a pass somebody took back. The result is
    // neither changed nor hidden: it is still a node under its check, still saying it passed.
    // What the retraction itself says - the mark and the reason the run was wrong
    // (`e98fc56ef8`) - the preset has nowhere to draw yet.
    const gone = rows.find((row) => row.id === blockNodeId(verification, "c-retracted"));
    expect(gone?.state).toBe("retracted");
    expect(gone?.stateWord).toBe(NODE_STATE_WORDS.check.retracted);
    expect(await bodyOf(page, results[2]?.id ?? "")).toContain("Pass");
  } finally {
    await alice.close();
  }
});

test("an issue with no Verification document says the preset holds no check", async ({
  browser,
}) => {
  await createProject({ key: "VER", name: "Verification" });
  const issue = await createIssue({ project: "VER", title: "Nothing to verify yet" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/verification`);

    // The old tab printed the command that makes the document; the preset says what is absent
    // and never what the reader should do about it (`EMPTY_STATE_LINES`).
    expect(await visibleText(page)).toContain(EMPTY_STATE_LINES.checks);
    await expect(rowsOfType(page, "check")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
