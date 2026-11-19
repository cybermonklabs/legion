import {
  BLOCK_ATTRIBUTE_LABELS,
  EMPTY_STATE_LINES,
  NODE_FACT_LABELS,
  NODE_STATE_WORDS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
} from "../web/src/features/issue/page-words";
import { blockNodeId } from "../web/src/features/issue/tree-drafts";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifactText,
  putRepoProject,
  recordCheckResult,
  retractCheckResult,
} from "./api";
import { connectedDot, documentEditor, paste, placeCaret } from "./editor";
import { fakeGithubCheckRunReads, seedFakeGithub } from "./fake-github-helpers";
import {
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
import { sql } from "./psql";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

// The issue's checks, on the Verification filter of the redesigned issue page (dispatch://INFRA-1196):
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
// @pr: The Verification filter's own row: every check of the document, each with the results recorded against it.
test("the Verification filter draws every check of the document, each with the results recorded against it @pr", async ({
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
    expect(current).toContain(RESULT_STANDS_WORDS.at_basis);
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
    expect(aged).toContain(RESULT_STANDS_WORDS.other_commit);
    expect(aged).toContain(firstHead);
    const atHead = await bodyOf(page, results[1]?.id ?? "");
    expect(atHead).toContain(RESULT_STANDS_WORDS.at_basis);
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
    expect(await visibleText(page)).toContain(EMPTY_STATE_LINES.verification);
    await expect(rowsOfType(page, "check")).toHaveCount(0);
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
  await putRepoProject(repository, { default: true, project: "VER" });
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

// SLOW(>=2s): 2.6 s - a project, its default repository, a Verification document, a staged pull
// request head and a seeded fake GitHub, before one load of the page, whose results read mints a
// token and reads the head's check runs from that fake. The floor the first row of this file
// pays is most of it.
test("a check with ci_check shows the pass its CI run concluded at the issue's open head", async ({
  browser,
}) => {
  const issue = await seedCICheck("CI proves it", {
    conclusion: "success",
    output: { summary: "41 passed", title: "ci-ok passed" },
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const answers: string[] = [];
    page.on("response", (response) => {
      if (new URL(response.url()).pathname === `/api/v1/issues/${issue.key}/results`) {
        answers.push(response.url());
      }
    });
    await gotoIssue(page, `/issues/${issue.key}/verification`);
    // A check whose pass stands is behind the fold, as the first row of this file says.
    await unfoldEveryBranch(page);
    await expect(rowsOfType(page, "check")).toHaveCount(1);
    const id = blockNodeId(verification, "c-ci");

    // A pass of the check stands, from the run CI concluded, and the check names that run under
    // the label its document draws `ci_check` under.
    const drawn = (await drawnRows(page)).find((row) => row.id === id);
    expect(drawn?.state).toBe("passed");
    expect(await factsIn(page, id)).toContainEqual({
      label: BLOCK_ATTRIBUTE_LABELS.ci_check,
      value: ciRun,
    });

    // Its one result is the one derived from that run, a node under the check drawn as a
    // recorded one is: its verdict, where its commit stands, its role, its run and its commit.
    await openResults(page);
    const results = (await drawnRows(page)).filter((row) => row.type === "result");
    expect(results).toHaveLength(1);
    expect(results[0]?.firstLine).toContain("conclusion: success");
    const facts = await factsIn(page, results[0]?.id ?? "");
    for (const fact of [
      { label: NODE_FACT_LABELS.verdict, value: RESULT_VERDICT_WORDS.pass },
      { label: NODE_FACT_LABELS.stands, value: RESULT_STANDS_WORDS.at_basis },
      { label: NODE_FACT_LABELS.role, value: "ci" },
      { label: NODE_FACT_LABELS.commit, value: secondHead },
    ]) {
      expect(facts).toContainEqual(fact);
    }
    expect(await bodyOf(page, results[0]?.id ?? "")).toContain(ciRun);

    // Each answer of the results route read the head's check runs once, one page of one run.
    await expect
      .poll(
        async () => (await fakeGithubCheckRunReads())[repository]?.[secondHead] === answers.length
      )
      .toBe(true);
    expect(answers.length).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 2.6 s - the same seeding and the same one load of the page as the row above, with
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
    await gotoIssue(page, `/issues/${issue.key}/verification`);
    await unfoldEveryBranch(page);
    const id = blockNodeId(verification, "c-ci");

    // No pass of it stands and no result is under it, and in place of a body that says nothing
    // of why, the check draws the server's own sentence saying what its run showed.
    const drawn = (await drawnRows(page)).find((row) => row.id === id);
    expect(drawn?.state).toBe("no-result");
    const facts = await factsIn(page, id);
    expect(facts).toContainEqual({ label: BLOCK_ATTRIBUTE_LABELS.ci_check, value: ciRun });
    expect(facts).toContainEqual({
      label: NODE_FACT_LABELS.ci_unproved,
      value: `CI run ${ciRun} concluded skipped at ${secondHead}, the head of ${repository}#7, which proves nothing.`,
    });
    await openResults(page);
    expect((await drawnRows(page)).filter((row) => row.type === "result")).toHaveLength(0);
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
