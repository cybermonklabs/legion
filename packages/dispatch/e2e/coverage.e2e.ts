import type { RequirementCoverage } from "../web/src/api/types";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  documentBase,
  getIssueCoverage,
} from "./api";
import { sql } from "./psql";
import { expect, test } from "./test";
import { asUser } from "./users";

/** Two requirements, so one can be served while the other is not. */
const spec = `:::requirement{#r1}
Every requirement of the spec is listed.
:::

:::requirement{#r2}
A requirement nothing checks is flagged.
:::
`;

/** The block-index reconciliation's lease (docs.BlockIndexLease) as duty.Exclusive takes it: the
 *  first key every loop lease is taken under, 0x4C4F4F50 (duty's leaseNamespace), and the lease's
 *  name. While a session of the row's holds it no pass of the server's runs, which skips a tick
 *  whose lease another session holds. `sql` runs on one psql session of this worker's, so the lock
 *  it takes outlives the call, and the row makes no other `sql` call while it holds it. */
const BLOCK_INDEX_LEASE = "1280266064, hashtext('block-index')";

/** A bullet list past the blocks a version write indexes in its own request (deferBlockIndexBlocks
 *  in docs/reference_excerpts.go, 2,000): a list of n items is n + 1 blocks, and 2,100 items is
 *  the margin e2e/acceptance/coverage-states.sh writes past it. A version holding it leaves its
 *  blocks to the block-index reconciliation. */
const DEFERRED_LIST = Array.from({ length: 2_100 }, () => "- x").join("\n");

/** How long a version whose blocks its write left to the reconciliation waits, once the lease is
 *  given back, until the open page has drawn it indexed: the next tick, which a tick that found the
 *  lease held leaves a whole BlockIndexReconcileInterval out (30 s, docs/block_index.go), that
 *  tick's pass over the version, and the page's refresh on the pass's `artifact.indexed`. That
 *  interval is the deadline the row holds the page to. */
const INDEXED_WITHIN = 45_000;

/** How the coverage read's line for a document that owes its block index ends, after
 *  `<file name> version <n>` (owedGap in api/coverage.go). */
const OWED_LINE_END = " is still being indexed; its coverage shows once that ends.";

/** One requirement's coverage, read once. The flag and the Referenced-by list are read from the
 *  block index of the version just written, so each call site polls this until the answer it
 *  waits for lands rather than trusting the first read. */
async function coverageOf(issue: string, blockId: string): Promise<RequirementCoverage> {
  const coverage = await getIssueCoverage(issue);
  const requirement = (coverage.requirements ?? []).find((row) => row.block_id === blockId);
  if (requirement === undefined) {
    throw new Error(`${issue} has no requirement ${blockId}: ${JSON.stringify(coverage)}`);
  }
  return requirement;
}

// SLOW(>=2s): one sign-in and one load of the Spec tab, then a second version of the Verification
// document written under that same open page and its flag watched clearing there, then a third
// whose blocks the block-index reconciliation owes while the row holds its lease, and the flag
// watched coming back there once the lease is given back: up to a BlockIndexReconcileInterval
// (30 s, docs/block_index.go) for the next tick, which no page clock can move. The live refresh of
// the coverage read is held to the start of its first fetch plus a second
// (MIN_REFETCH_INTERVAL_MS, web/src/api/query-refresh.ts).
// @pr: The coverage surface's own row: an unserved requirement is flagged on the Spec tab, a check written under the open page clears it, and the flag comes back there once the block index catches up on a version that drops the check.
test("a requirement no check serves is flagged on the Spec tab, and a check that serves it clears the flag @pr", async ({
  browser,
}, testInfo) => {
  test.setTimeout(120_000);
  await createProject({ key: "COV", name: "Coverage" });
  const issue = await createIssue({ project: "COV", spec, title: "Requirement coverage" });
  const oneCheck = `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/coverage.e2e.ts"}
Every requirement of the spec is listed.
:::
`;
  const verification = await createIssueArtifact(issue.key, {
    content: oneCheck,
    name: "verification.md",
  });
  await expect.poll(() => coverageOf(issue.key, "r2").then((row) => row.checks)).toBe(0);

  let leased = false;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);

    const requirements = page.getByTestId("spec-requirement");
    await expect(requirements).toHaveCount(2);
    const served = requirements.filter({ hasText: "Every requirement of the spec is listed." });
    const unserved = requirements.filter({ hasText: "A requirement nothing checks is flagged." });
    await expect(served).toHaveAttribute("data-covered", "true");
    await expect(served.getByTestId("spec-requirement-gap")).toHaveCount(0);
    // The issue has no plan document yet, so the one thing this requirement lacks is a check.
    await expect(unserved).toHaveAttribute("data-covered", "false");
    await expect(unserved.getByTestId("spec-requirement-gap")).toHaveText("has no check");
    await page.screenshot({ path: testInfo.outputPath("spec-requirement-gap.png") });

    // A check that serves it is the whole of what it lacked: the next version of the
    // Verification document clears the flag. That version replaces the whole document, so it
    // names the token of the version it replaces.
    await createIssueArtifact(issue.key, {
      content: `${oneCheck}
:::check{#c2 serves="dispatch://${issue.key}/spec#b-r2" surface="browser" run="bun run e2e -- e2e/coverage.e2e.ts"}
A requirement nothing checks is flagged.
:::
`,
      name: "verification.md",
      precondition: await documentBase(verification.artifact.id),
    });
    await expect.poll(() => coverageOf(issue.key, "r2").then((row) => row.checks)).toBe(1);

    // The page is left open: the Spec tab's coverage read is invalidated live by
    // `artifact.version` (web/src/api/event-refresh.ts), so no reload is needed.
    await expect(unserved).toHaveAttribute("data-covered", "true");
    await expect(page.getByTestId("spec-requirement-gap")).toHaveCount(0);

    // The next version drops c2 and holds a list past the block bound, so its write leaves its
    // blocks to the block-index reconciliation, whose lease the row holds first: the version owes
    // its index for as long as the row says. A try the lease's holder refuses - a tick running now
    // - is made again until that tick ends.
    await expect.poll(() => sql(`select pg_try_advisory_lock(${BLOCK_INDEX_LEASE})`)).toBe("t");
    leased = true;
    const deferred = await createIssueArtifact(issue.key, {
      content: `${oneCheck}\n${DEFERRED_LIST}\n`,
      name: "verification.md",
      precondition: await documentBase(verification.artifact.id),
    });
    // The issue is an issue room, and a room's write that leaves its blocks to the reconciliation
    // owes the index of every document of the room whose version its pass covers, not only the
    // one it wrote (oweVersionsIndex in docs/reference_excerpts.go): the room's tree and spec.md
    // owe it beside verification.md, and the coverage read writes one line for each of them, in
    // the order the documents were made (owedDocumentsStatement in api/coverage.go). The row holds
    // the page to the lines the server writes for the documents it reads as owing, verification.md
    // at the version just written among them, and to no missing check.
    const owing = await getIssueCoverage(issue.key);
    const owedDocuments = owing.owed_documents ?? [];
    expect(owedDocuments).toContainEqual({
      artifact_id: verification.artifact.id,
      version: deferred.version.number,
    });
    const owedLines = (owing.gaps ?? []).filter((line) => line.endsWith(OWED_LINE_END));
    expect(owedLines).toHaveLength(owedDocuments.length);
    expect(owedLines).toContain(
      `verification.md version ${deferred.version.number}${OWED_LINE_END}`
    );
    expect((owing.gaps ?? []).filter((line) => line.endsWith(" has no check"))).toEqual([]);
    // The version's own `artifact.version` refreshes the open page's coverage, which draws each of
    // those lines in place of every missing check while the documents owe their index.
    const owed = page.getByTestId("spec-requirements-note");
    await expect(owed).toHaveText(owedLines);
    await expect(page.getByTestId("spec-requirement-gap")).toHaveCount(0);

    // Given back, the lease lets the next tick index the version, and the open page, never
    // reloaded and never refocused, draws the requirement's missing check again once the pass's
    // `artifact.indexed` reaches it (web/src/api/event-refresh.ts).
    expect(await sql(`select pg_advisory_unlock(${BLOCK_INDEX_LEASE})`)).toBe("t");
    leased = false;
    await expect(owed).toHaveCount(0, { timeout: INDEXED_WITHIN });
    await expect(unserved).toHaveAttribute("data-covered", "false");
    await expect(unserved.getByTestId("spec-requirement-gap")).toHaveText("has no check");
    await expect(served).toHaveAttribute("data-covered", "true");

    // What the pass committed is one artifact.indexed naming every document the room owed, after
    // verification.md's own artifact.version, and no artifact.version of any of them after it: the
    // pass wrote no version nobody wrote, and verification.md's newest is the one just uploaded.
    const document = verification.artifact.id;
    const owedIds = JSON.stringify(owedDocuments.map((row) => row.artifact_id));
    expect(
      await sql(
        `select count(*) from events where issue_key = '${issue.key}' and type = 'artifact.indexed'
           and payload->'artifact_ids' @> '${owedIds}'::jsonb
           and id > (select max(id) from events where type = 'artifact.version' and payload->>'artifact_id' = '${document}')`
      )
    ).toBe("1");
    expect(
      await sql(
        `select count(*) from events
         where type = 'artifact.version' and payload->>'artifact_id' in (select jsonb_array_elements_text('${owedIds}'::jsonb))
           and id > (select max(id) from events where issue_key = '${issue.key}' and type = 'artifact.indexed')`
      )
    ).toBe("0");
    expect(
      await sql(
        `select max((payload->'version'->>'number')::int) from events
         where type = 'artifact.version' and payload->>'artifact_id' = '${document}'`
      )
    ).toBe(String(deferred.version.number));
  } finally {
    try {
      if (leased) await sql(`select pg_advisory_unlock(${BLOCK_INDEX_LEASE})`);
    } finally {
      await alice.close();
    }
  }
});

// SLOW(>=2s): 3.4 s - two loads of the issue page, since the backlink panel has no live
// invalidation on `artifact.version` (web/src/api/event-refresh.ts): only the whole-cache
// refresh a feed gap schedules reaches it, 5 s out (WHOLE_CACHE_REFRESH_MS,
// web/src/api/event-feed.ts), where a second load reads the dropped edge back in 1.4 s.
test("a requirement's Referenced-by list shows the plan task that serves it, and loses it when that task goes", async ({
  browser,
}) => {
  await createProject({ key: "COV", name: "Coverage" });
  const issue = await createIssue({ project: "COV", spec, title: "Referenced by a plan task" });
  const plan = await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is built.

:::plan_task{#t1 serves="dispatch://${issue.key}/spec#b-r1"}
List every requirement under the spec.
:::
`,
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  await expect.poll(() => coverageOf(issue.key, "r1").then((row) => row.plan_tasks)).toBe(1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const requirement = page
      .getByTestId("spec-requirement")
      .filter({ hasText: "Every requirement of the spec is listed." });
    await requirement.getByRole("button", { name: "Referenced by" }).click();

    // A `serves` edge read from the requirement says what the plan task does for it, and links
    // to the task's own block.
    const panel = requirement.getByRole("region", { name: "Referenced by" });
    await expect(panel.getByRole("link", { name: `served by · ${issue.key}` })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/plan-md#b-t1`
    );
    await expect(panel).toContainText("List every requirement under the spec.");

    // The next version of the plan drops the task, and with it the edge. It replaces the whole
    // document, so it names the token of the version it replaces.
    await createIssueArtifact(issue.key, {
      content: `How ${issue.key} is built.\n`,
      name: "plan.md",
      precondition: await documentBase(plan.artifact.id),
    });
    await expect.poll(() => coverageOf(issue.key, "r1").then((row) => row.plan_tasks)).toBe(0);

    // The backlink panel has no live invalidation on `artifact.version`
    // (web/src/api/event-refresh.ts), unlike the coverage read of the row above, so the panel is
    // read again on a fresh load rather than waited out on this one.
    await page.reload();
    await requirement.getByRole("button", { name: "Referenced by" }).click();
    await expect(requirement.getByRole("region", { name: "Referenced by" })).toContainText(
      "Nothing references this yet."
    );
  } finally {
    await alice.close();
  }
});
