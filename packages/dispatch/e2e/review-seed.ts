/** The one corpus both review layers open `e2e/review-routes.ts`'s pages against: the accessibility
 *  pass and the containerised visual review read the same rows, so a finding of one is a finding
 *  about the page the other shot.
 *
 *  It builds on the fixtures that already exist rather than writing a second corpus:
 *  `seedWorkspace()` fills the Inbox, the Agents page, a broadcast, the answers, the projects and
 *  the issue tree, and the delivery fixtures fill `/delivery` and `/pipeline`. What it adds is
 *  the four documents the issue tabs need. */
import { createIssueArtifact, createPlanReview, getIssue, getProjectArtifact } from "./api";
import {
  PIPELINE_AT,
  seedDeliveryFixture,
  seedPipelineRows,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { blockId } from "./editor";
import type { ReviewSeed } from "./review-routes";
import { seedWorkspace } from "./workspace";

/** The decision `decisions.md` records, which the Decisions tab lists under the reader's review
 *  and the Spec tab draws beside the block it shaped. */
const DECISION = "Seed one corpus for both review layers";
const REASON = "Chosen so a scan and a shot read the same page.";

/** A `serves` pointer is refused unless it names a `requirement` block that exists, and
 *  `seedWorkspace`'s spec carries none. Rather than edit `e2e/workspace.ts`, whose spec every
 *  other spec of the suite reads, the requirements live in a document of this seed's own, which
 *  is also the second, non-primary issue artifact the `issue-artifact` row opens: one document
 *  serves both needs, and the issue keeps the one spec `seedWorkspace` wrote. A pointer may name
 *  a block of another document of the same issue's tree, which is what these two are. */
const REQUIREMENTS = `# What the review layers open

:::requirement{#r1}
Every route of the router is reviewed, or excluded with a reason.
:::

:::requirement{#r2}
A page is read once it is drawn, never as a loading skeleton.
:::
`;

/** Seeds the review corpus into an empty database; the caller resets first. */
export async function seedReviewRoutes(): Promise<ReviewSeed> {
  const workspace = await seedWorkspace();
  const issue = workspace.issues.workflow;

  // `seedPipelineFixture()` is `seedDeliveryFixture()`'s settings row written a second time and
  // then the Pipeline's own rows, and `delivery_settings` holds one row, so the two fixtures are
  // seeded as their halves: the settings and the timeline's rows, then `seedPipelineRows()`,
  // which only updates that one row's job names before writing the Pipeline's runs.
  await seedDeliveryFixture();
  await seedPipelineRows();

  const requirements = await createIssueArtifact(issue, {
    content: REQUIREMENTS,
    name: "requirements.md",
  });
  const served = `dispatch://${issue}/artifact/${requirements.artifact.slug}`;

  const plan = await createIssueArtifact(issue, {
    // A `needs` pointer names a task of the plan's own document, so both tasks are written in
    // the one upload that makes the plan.
    content: `How ${issue} is built.

:::plan_task{#t0 serves="${served}#b-r1"}
Write the route table.
:::

:::plan_task{#t1 serves="${served}#b-r2" needs="dispatch://${issue}/artifact/plan-md#b-t0"}
Draw each page and read the locator that says it is drawn.
:::
`,
    name: "plan.md",
  });
  // A plan is approved by its plan review, never by a person, and the tab draws that chip.
  await createPlanReview(plan.artifact.id, 1);

  // No result is recorded against either check: a result's currency is judged against the head
  // of the pull request Dispatch holds for the issue, which would need a fake-GitHub repository
  // this corpus has no other use for (`e2e/verification.e2e.ts`). Both checks therefore read
  // "No result", which is a state the tab draws.
  await createIssueArtifact(issue, {
    content: `How ${issue} is proved.

:::check{#c-routes serves="${served}#b-r1" surface="cli" run="bun test e2e/__tests__/review-routes.test.ts" negative_control="Drop a row and the test names the route it no longer covers."}
Every route of the router is reviewed or excluded.
:::

:::check{#c-ready serves="${served}#b-r2" surface="browser" run="bunx playwright test --config e2e/playwright.config.ts" negative_control="Point a row at a skeleton and the run times out on it."}
Each page is read once it is drawn.
:::
`,
    name: "verification.md",
  });

  const specId = (await getIssue(issue)).primary_artifact_id ?? "";
  const shaped = await blockId(specId, "One command, a seeded corpus, a signed-in browser.");
  await createIssueArtifact(issue, {
    content: `## ${DECISION}\n\n${REASON} Shapes dispatch://${issue}/spec#b-${shaped}.\n`,
    name: "decisions.md",
  });

  // The slug of the `Runbook` document `seedWorkspace` creates: read back rather than assumed,
  // so a rename there fails this seed instead of the page it opens.
  const runbook = await getProjectArtifact("CORE", "runbook");

  return {
    broadcast: workspace.broadcast.id,
    deliveryQuery: TIMELINE_FIXTURE_QUERY,
    issue,
    issueArtifactSlug: requirements.artifact.slug,
    pipelineAt: PIPELINE_AT,
    project: "CORE",
    projectDocumentSlug: runbook.slug,
  };
}
