/** The one corpus both review layers open `e2e/review-routes.ts`'s pages against: the accessibility
 *  pass and the containerised visual review read the same rows, so a finding of one is a finding
 *  about the page the other shot.
 *
 *  It builds on the fixtures that already exist rather than writing a second corpus:
 *  `seedWorkspace` fills the Inbox, the Agents page, a broadcast, the answers, the projects and
 *  the issue tree, and the delivery fixtures fill `/delivery` and `/pipeline`. What it adds is
 *  the four documents the issue tabs need, and the one priority the two priority pages are read
 *  against: `seedWorkspace` writes none, and both of those pages draw a sentence instead of a
 *  row for a deployment that has written no priority. */
import {
  createIssueArtifact,
  createPlanReview,
  createPriority,
  getArtifact,
  getBroadcast,
  getIssue,
  getProjectArtifact,
  patchIssue,
  putAgentState,
  putDocumentRead,
} from "./api";
import {
  PIPELINE_AT,
  seedDeliveryFixture,
  seedPipelineRows,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { blockId } from "./editor";
import { awaitFact } from "./local-instance-support";
import { sql } from "./psql";
import type { ReviewSeed, SeededReadMarks } from "./review-routes";
import { seedWorkspace, workspaceSessions } from "./workspace";

/** The decision a `decision` block of the issue records, which the Decisions tab lists under the
 *  reader's review and the Spec tab draws beside the block it shaped. */
const DECISION = "Seed one corpus for both review layers";
const REASON = "Chosen so a scan and a shot read the same page.";

/** The person `/people/:login` is opened on: the login `seedWorkspace()` makes every human
 *  write of this corpus as (`e2e/workspace.ts`'s `alice`, which is also `e2e/api.ts`'s default
 *  author), and the login both layers sign in as (`withAlice`, `e2e/users.ts`). Her page is
 *  therefore the comments she wrote here, drawn as rows rather than as the empty state. */
const PERSON = "alice";

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

/** The one priority this corpus writes. It is written as the signed-in person rather than over
 *  the agent bearer, because the margin of `/priorities/:id` draws who wrote the priority and a
 *  bearer would name a session there instead. Its description holds a link, so the accessibility
 *  pass reads a link inside the rendered description a writer may open into its editor: a
 *  control wrapping that text would put the link inside it (axe's `nested-interactive`). */
function createReviewPriority() {
  return createPriority({
    description:
      "Every reader of this deployment reaches the same corpus in one command.\n\n" +
      "The work under it is the seeding, the sign-in and the pages both review layers read, " +
      "from [the priorities page](/priorities).",
    owner: "alice",
    title: "Local testing is one command",
  });
}

/** Writes, before any row draws a page, the read marks the pages of the shot set write as they
 *  are drawn, so each page shows the same reads whichever pages a run drew before it. A read mark
 *  cannot be taken back: an agent's `read_through` only moves forward and its `read_replies` only
 *  add ids (`putUserAgentState`, `api/state.go`), and a document's read version is what the Spec
 *  tab compares its blocks against. So a page that wrote one changed every page drawn after it -
 *  the rail's Agents badge read "New replies 2" before the broadcast page and "New reply 1" after
 *  it - and the visual layer, which seeds once for the whole run, held each of those two states
 *  in the baselines of the rows on either side.
 *
 *  A full visual run writes three marks while its rows draw, read from its stack's request log:
 *  the broadcast page's read of the replies its recipients wrote (`BroadcastPage.tsx`,
 *  `useMarkShownRepliesRead`), and the Spec tab's read of the issue's spec at its newest version,
 *  in the desktop row and again in the phone row (`useSpecMarks`). These are those writes, the same
 *  requests the pages send; after them each page's own write changes nothing. It answers the marks
 *  it wrote, which the visual rows' guard lets a page write again only exactly as they stand
 *  (`isSeededReadMark`, e2e/visual-guard.ts): a page of the shot set that comes to write another
 *  mark as it draws needs that mark written here, and until it is, the guard stops the write and
 *  fails the row that made it, by name. */
async function markWhatTheShotPagesRead(
  broadcastId: string,
  issue: string,
  specId: string
): Promise<SeededReadMarks> {
  const broadcast = await getBroadcast(broadcastId);
  const agents: Record<string, readonly string[]> = {};
  let replies = 0;
  for (const recipient of broadcast.recipients ?? []) {
    // The page's own filter: a reply its session wrote, by kind and id, which is what the server
    // takes in `read_replies` and refuses the whole write over one that fails it.
    const shown = (recipient.replies ?? [])
      .filter(
        (reply) => reply.author.kind === "session" && reply.author.id === recipient.session_id
      )
      .map((reply) => reply.id);
    if (shown.length > 0) {
      await putAgentState(recipient.session_id, { read_replies: shown });
      agents[recipient.session_id] = [...(agents[recipient.session_id] ?? []), ...shown];
      replies += shown.length;
    }
  }
  if (replies === 0) {
    throw new Error(
      "seedReviewRoutes: the broadcast holds no reply its recipients wrote, so its page marks " +
        "nothing read and the rows' Agents badge reads a state no baseline holds"
    );
  }
  if (specId === "") {
    throw new Error(`seedReviewRoutes: ${issue} has no spec for its Spec tab to read`);
  }
  // The server lists a document's versions oldest first, and the Spec tab shows the one with the
  // highest number (`versionsNewestFirst`, then the first): so the mark is the highest number, read
  // from every version, and never the list's first, which is the newest only for a one-version spec.
  const numbers = ((await getArtifact(specId)).versions ?? []).map((version) => version.number);
  if (numbers.length === 0) {
    throw new Error(`seedReviewRoutes: ${issue}'s spec has no version for its Spec tab to read`);
  }
  const version = Math.max(...numbers);
  await putDocumentRead(specId, version);
  return { agents, documents: { [specId]: version } };
}

/** Waits, within `boundMs`, until the document service owes no document a settlement, then holds
 *  that the corpus records no check result and holds no pull request's head, so no read of a shot
 *  page has a settlement to run or a commit to ask GitHub about. */
async function awaitNothingOwed(boundMs: number): Promise<void> {
  await awaitFact({
    boundMs,
    fact: "seedReviewRoutes: the document service owes no document of the corpus a settlement",
    holds: (owed) => owed === "",
    // A tenth of a second between reads, as the seed's other waits read.
    intervalMs: 100,
    read: () =>
      sql(
        `SELECT coalesce(string_agg(artifact_id::text, ', ' ORDER BY artifact_id), '')
           FROM doc_settlements_pending`
      ),
    show: (owed) => `settlements owed to ${owed}`,
  });
  const held = await sql(
    `SELECT (SELECT count(*) FROM check_results) || ' ' || (SELECT count(*) FROM pull_request_heads)`
  );
  if (held !== "0 0") {
    const [results, heads] = held.split(" ");
    throw new Error(
      `seedReviewRoutes: the corpus records ${results} check result(s) and ${heads} pull request ` +
        "head(s): the Verification tab's read would ask GitHub which pull request holds a " +
        "result's commit and write the answer, a write the visual rows' corpus must not take"
    );
  }
}

/** Seeds the review corpus into an empty database; the caller resets first. `boundMs` is the
 *  bound of the seed's waits, which `seedWorkspace` gives each of its own: the caller's, so a
 *  browser row's seed gives up at the bound that row's assertions use. */
export async function seedReviewRoutes({
  boundMs,
}: {
  readonly boundMs: number;
}): Promise<ReviewSeed> {
  const workspace = await seedWorkspace({ boundMs });
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

  // No result is recorded against either check: where a result's commit stands is judged against
  // the head of the pull request Dispatch holds for the issue, which would need a fake-GitHub
  // repository this corpus has no other use for (`e2e/verification.e2e.ts`). Both checks
  // therefore read "No result", which is a state the tab draws.
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
  // The decision's words cite the block it shaped as well as its `shaped` attribute tying them:
  // a decision card draws its words, so the card beside the spec draws that block's own words,
  // which `e2e/review-routes.ts`'s `issue-spec` row waits on to know the reference graph answered.
  await createIssueArtifact(issue, {
    content: `:::decision{#d1 shaped="dispatch://${issue}/spec#b-${shaped}"}\n### ${DECISION}\n\n${REASON} Shapes dispatch://${issue}/spec#b-${shaped}.\n:::\n`,
    name: "choices.md",
  });

  // The priority `/priorities` lists and `/priorities/:id` opens, and the work under it. The
  // workflow issue stands in `in_progress`, so the row counts one in flight and the priority's
  // own page lists it; the freshness-folds issue stands in `backlog`, so it is a row of the
  // second fault list. Every other issue of the corpus that is in flight advances none, which
  // is what fills the first fault list. Both relations are written last, so the activity of
  // each issue keeps the order the rows above it were seeded in, and one after the other: the
  // priority's margin lists its history newest first by the order the store wrote it, so two
  // writes made at once land in either order and the page differs between two runs of one tree
  // (the 68 pixels the visual review read on `priority` in two rounds).
  const priority = await createReviewPriority();
  await patchIssue(issue, { advances: [priority.id] });
  await patchIssue(workspace.issues.folds, { advances: [priority.id] });

  // The slug of the `Runbook` document `seedWorkspace` creates: read back rather than assumed,
  // so a rename there fails this seed instead of the page it opens.
  const runbook = await getProjectArtifact("CORE", "runbook");

  // Nothing owed before any row draws: a settlement the document service still owes runs when a
  // page opens that document's room and writes a version the shot would draw, and a result's
  // commit is one `GET /issues/{key}/results` asks GitHub the pull request of, writing what it
  // learns. The rows stop every write their pages send (e2e/visual-guard.ts) and a Go test holds
  // what the server writes while it answers their reads (visual_shot_reads_test.go); these two
  // the seed must leave settled itself.
  await awaitNothingOwed(boundMs);

  // Last, so the spec's newest version is the one its tab will show.
  const readMarks = await markWhatTheShotPagesRead(workspace.broadcast.id, issue, specId);

  return {
    broadcast: workspace.broadcast.id,
    deliveryQuery: TIMELINE_FIXTURE_QUERY,
    issue,
    issueArtifactSlug: requirements.artifact.slug,
    // The machine of the first seeded session, read from the registry this seed wrote rather
    // than named again here, so a rename in e2e/workspace.ts moves this page's URL with it.
    machine: firstMachine(),
    person: PERSON,
    pipelineAt: PIPELINE_AT,
    readMarks,
    // The id the create answered, which is this title written as a slug: read back rather than
    // spelled again here, so the rule that derives it stays the server's.
    priority: priority.id,
    project: "CORE",
    projectDocumentSlug: runbook.slug,
  };
}
