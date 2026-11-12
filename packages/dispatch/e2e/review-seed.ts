/** The one corpus both review layers open `e2e/review-routes.ts`'s pages against: the accessibility
 *  pass and the containerised visual review read the same rows, so a finding of one is a finding
 *  about the page the other shot.
 *
 *  It builds on the fixtures that already exist rather than writing a second corpus:
 *  `seedWorkspace()` fills the Inbox, the Agents page, a broadcast, the answers, the projects and
 *  the issue tree, and the delivery fixtures fill `/delivery` and `/pipeline`. What it adds is
 *  the four documents the issue tabs need, and the one priority the two priority pages are read
 *  against: `seedWorkspace()` writes none, and both of those pages draw a sentence instead of a
 *  row for a deployment that has written no priority. */
import type { Priority } from "../web/src/api/types";
import {
  baseUrl,
  createIssueArtifact,
  createPlanReview,
  getIssue,
  getProjectArtifact,
  patchIssue,
  userHeaders,
} from "./api";
import {
  PIPELINE_AT,
  seedDeliveryFixture,
  seedPipelineRows,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { blockId } from "./editor";
import { sql } from "./psql";
import type { ReviewSeed } from "./review-routes";
import { seedWorkspace, workspaceSessions } from "./workspace";

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

/** The machine `/machines/:machine` is opened on: the one the first seeded session reports.
 *  Read from the registry rather than written here, so a rename in `e2e/workspace.ts` moves
 *  that page's URL with it, and a registry that carries none fails by name rather than opening
 *  `/machines/undefined`, which draws a page saying it knows no such machine. */
function firstMachine(): string {
  const machine = workspaceSessions[0]?.machine_id;
  if (machine === undefined) {
    throw new Error("seedReviewRoutes: no seeded session names a machine");
  }
  return machine;
}

/** The one priority this corpus writes, through `POST /api/v1/priorities`. `e2e/api.ts` holds
 *  no wrapper for that route and this seed is the only caller of it here, so the call is
 *  written out: as the signed-in person, over the cookie `userHeaders` holds, because the
 *  margin of `/priorities/:id` draws who wrote the priority and a bearer would name a session
 *  there instead. */
async function createPriority(): Promise<Priority> {
  const response = await fetch(new URL("/api/v1/priorities", baseUrl), {
    body: JSON.stringify({
      description:
        "Every reader of this deployment reaches the same corpus in one command.\n\n" +
        "The work under it is the seeding, the sign-in and the pages both review layers read.",
      owner: "alice",
      title: "Local testing is one command",
    }),
    headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
    method: "POST",
  });
  if (!response.ok) {
    throw new Error(
      `seedReviewRoutes: POST /api/v1/priorities answered ${response.status} ${await response.text()}`
    );
  }
  return (await response.json()) as Priority;
}

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

  // The freshness rows of the Delivery and the Pipeline pages, pinned to the verdict a
  // reconcile pass reaches here. Nothing seeds a GitHub App for `acme/widgets`, so every pass
  // this server runs against the corpus fails with exactly the error written below, and a
  // failed pass leaves `last_reconcile_at` null and overwrites `last_error` with it
  // (`delivery/reconcile.go`, `store.go`'s `RecordReconcileError`). What is not fixed is
  // WHETHER a pass has run: the first tick is jittered across the five-minute interval
  // (`delivery.ReconcileInterval`), so one run of these pages read "PRs never checked" and the
  // next, ninety seconds later, read "PRs never checked: the GitHub App is not installed on
  // acme/widgets" - eight lines of difference across the two pages, and two labels of
  // different widths, which no mask can cover and no wait can settle inside a row. Writing the
  // verdict here makes both reads the same whether a pass has run or not, as
  // `e2e/delivery.e2e.ts` writes it for the same reason.
  await sql(
    `UPDATE delivery_settings
       SET last_error = 'the GitHub App is not installed on acme/widgets'
     WHERE singleton`
  );

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

  // The priority `/priorities` lists and `/priorities/:id` opens, and the work under it. The
  // workflow issue stands in `in_progress`, so the row counts one in flight and the priority's
  // own page lists it; the freshness-folds issue stands in `backlog`, so it is a row of the
  // second fault list. Every other issue of the corpus that is in flight advances none, which
  // is what fills the first fault list. Both relations are written last, so the activity of
  // each issue keeps the order the rows above it were seeded in.
  const priority = await createPriority();
  await Promise.all([
    patchIssue(issue, { advances: [priority.id] }),
    patchIssue(workspace.issues.folds, { advances: [priority.id] }),
  ]);

  // The slug of the `Runbook` document `seedWorkspace` creates: read back rather than assumed,
  // so a rename there fails this seed instead of the page it opens.
  const runbook = await getProjectArtifact("CORE", "runbook");

  return {
    broadcast: workspace.broadcast.id,
    deliveryQuery: TIMELINE_FIXTURE_QUERY,
    issue,
    issueArtifactSlug: requirements.artifact.slug,
    // The machine of the first seeded session, read from the registry this seed wrote rather
    // than named again here, so a rename in e2e/workspace.ts moves this page's URL with it.
    machine: firstMachine(),
    pipelineAt: PIPELINE_AT,
    // The id the create answered, which is this title written as a slug: read back rather than
    // spelled again here, so the rule that derives it stays the server's.
    priority: priority.id,
    project: "CORE",
    projectDocumentSlug: runbook.slug,
  };
}
