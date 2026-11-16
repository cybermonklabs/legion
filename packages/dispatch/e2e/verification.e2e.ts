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
        async () => (await fakeGithubCheckRunReads())[repository]?.[secondHead] === answers.length
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
