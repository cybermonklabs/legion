/** Dashboard on today's issue page, with the new page's setting off: what the server decides for
 *  each of its parts is what these rows read - the issue's Legion phase and claims from the rows
 *  the sampler writes, the rounds from the handoffs, which open asks wait on the reader and whose
 *  turn each is, which decisions wait for a review, which approval a later version made stale,
 *  which threads hold a write the reader has not read, how each check stands at the commit the
 *  page names, and what each act in place writes. The tiles row turns the setting on at its end,
 *  to read the new page's Verification view by the rule its tiles count by (R6). */

// The DOM contract these rows read, which the Dashboard tasks build (dispatch://INFRA-5702):
//
// - The tab is the `tab` "Dashboard", the first of the tablist "Issue detail". Its panel is
//   `#issue-dashboard-panel`, role `tabpanel`, and no element of the page carries
//   `data-issue-page`.
// - The four parts are `region`s named by `DASHBOARD_WORDS`' part names: "Phase" (the track),
//   "Waiting on you" (what waits), "Progress" (the tiles) and "Agents at work" (the agents).
// - Phase: a `list` of eight `listitem`s, the steps Admitted, Planning, Implementing, Testing,
//   Reviewing, Accepting, Retro and Merging, each its name and, on Planning, Implementing,
//   Testing and Reviewing, its count of rounds as a number where it has one. The marked step
//   carries `aria-current="step"`. The phase's words ("Waiting on a person", "Integrating",
//   "Held", "In another phase") stand in the region. The region holds no `button` and no `link`.
// - Waiting on you: the count, `data-testid="dashboard-waiting-count"`, whose text is the
//   number of items. Each item is one element with `data-testid="dashboard-item-<kind>-<id>"`,
//   in the order drawn: kind `question` or `approval` with the ask's id, `stale` with the
//   document's id, `decision` with the decision's block id, `thread` with the thread's root id.
//   A question and an approval request draw `[data-ask-body]`; a decision `[data-decision-card]`
//   with "Keep", "Change", the field "Your change" and "Send change"; a stale approval the
//   approval control with "Approve"; a comment thread its reply control (the field "Reply" and
//   "Send") and a `link` to the thread's own address. Five items are drawn, with the `button`
//   "Show the next five", whose text holds the number left. Where nothing waits, the region
//   holds no item and one line of declared words.
// - Progress: four `button`s named "Statements met", "Checks passed", "Children done" and
//   "Questions waiting on others", each holding a `meter` whose `aria-valuenow` is its number and
//   whose `aria-valuemax` is the whole. A press opens under it a `list` named as the tile, of
//   `link`s to what the number counts.
// - Agents at work: a `list` with one `listitem` per role that holds a claim, in the order the
//   Legion state gives, each its role and then its role words.
// - The new page's Verification view, which the tiles row opens at its own address with the
//   setting on (`e2e/issue-page-option.ts`), is the contract `e2e/issue-page-v.ts` states: each
//   statement a `[data-verification-card]` with `data-node-id` and `data-statement-state`, and its
//   word in `span[data-statement-state]`; each check under it a `[data-verification-check]` with
//   `data-node-id` and `data-node-state`.

import { DISPATCH_WRITING_BOXES } from "@legion/contracts";

import {
  DECLARED_WORDS,
  VERIFICATION_WORDS,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import {
  blockNodeId,
  checkStanding,
  namedCommit,
} from "../web/src/features/issue-page/statement-rules";
import {
  getArtifact,
  getAsk,
  getComment,
  getIssue,
  getIssueCoverage,
  getIssueDecisions,
  getIssueResults,
  getMessage,
  listComments,
  listIssueAsks,
  listMyReplies,
} from "./api";
import {
  type DashboardSeed,
  EVERY_CLAIM_STATE,
  type LegionStateSeed,
  type MainHolds,
  mainHolds,
  READER,
  ROUNDS,
  readLegionIssue,
  SEEDED_LEGION,
  seedDashboard,
  seedPhaseIssue,
  seedQuietIssue,
  type TileCount,
  UNLISTED,
  type WaitingItem,
  writeRounds,
} from "./dashboard-seed";
import { focusThePage } from "./issue-page-g";
import { openPreset } from "./issue-page-i";
import { withIssuePage } from "./issue-page-option";
import { verificationCard } from "./issue-page-v";
import { expect, type Locator, type Page, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/** Today's ten tabs, by the names and in the order `features/issue/issue-tab-list.ts` gives them
 *  at the merge-base `ac1c9a33`; Artifacts carries its count where the issue holds one. */
const TODAYS_TABS: readonly (string | RegExp)[] = [
  "Spec",
  "Verification",
  "Plan",
  "Steps",
  "Conversation",
  "Children",
  /^Artifacts( \d+)?$/,
  "Activity",
  "Decisions",
  "Collaborators",
];

/** The eight steps of the track, in order (R3). */
const STEPS = [
  "Admitted",
  "Planning",
  "Implementing",
  "Testing",
  "Reviewing",
  "Accepting",
  "Retro",
  "Merging",
] as const;

/** The words a phase draws beside the track (R3). */
const PHASE_LINES = ["Waiting on a person", "Integrating", "Held", "In another phase"] as const;

/** The role words of each claim state (R7): any other state reads "in another state", and a
 *  `retired` claim is not listed. */
const ROLE_WORDS: Readonly<Record<string, string>> = {
  failed: "failed",
  idle: "idle",
  launch_uncertain: "starting",
  launching: "starting",
  queued: "starting",
  ready: "idle",
  registered: "idle",
  shim_connected: "starting",
  suspended: "paused",
  working: "in a turn",
};

const TILES = {
  children: "Children done",
  met: "Statements met",
  passed: "Checks passed",
  questions: "Questions waiting on others",
} as const;

/** Which main this run's server is, read once per worker (R27). */
let holds: MainHolds | undefined;

test.beforeAll(async () => {
  holds = await mainHolds();
});

function thisMain(): MainHolds {
  if (holds === undefined) throw new Error("the file's beforeAll read no main");
  return holds;
}

/** `text` as a pattern that matches it literally. */
function literal(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** Opens `key`'s Dashboard at its own address and answers its panel, failing the row where the
 *  page draws none. */
async function openDashboard(page: Page, key: string): Promise<Locator> {
  await page.goto(`/issues/${key}/dashboard`);
  const panel = page.locator("#issue-dashboard-panel");
  await expect(panel, "today's page draws no Dashboard panel").toBeVisible();
  await expect(panel).toHaveAttribute("role", "tabpanel");
  await expect(page.getByRole("tab", { exact: true, name: "Dashboard" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(page.locator("[data-issue-page]")).toHaveCount(0);
  return panel;
}

function waitingOf(panel: Locator): Locator {
  return panel.getByRole("region", { exact: true, name: "Waiting on you" });
}

function itemsOf(waiting: Locator): Locator {
  return waiting.locator('[data-testid^="dashboard-item-"]');
}

function testIdOf(item: WaitingItem): string {
  return `dashboard-item-${item.kind}-${item.id}`;
}

function itemOf(waiting: Locator, item: WaitingItem): Locator {
  return waiting.getByTestId(testIdOf(item));
}

/** The test id of each item drawn, in the order drawn. */
function drawnItems(waiting: Locator): Promise<string[]> {
  return itemsOf(waiting).evaluateAll((items) =>
    items.map((item) => item.getAttribute("data-testid") ?? "")
  );
}

function countOf(waiting: Locator): Locator {
  return waiting.getByTestId("dashboard-waiting-count");
}

function nextFive(waiting: Locator): Locator {
  return waiting.getByRole("button", { name: /^Show the next five/ });
}

/** Draws every item of the list: one press of the control for each five past the first. */
async function drawEveryItem(waiting: Locator, seed: DashboardSeed): Promise<void> {
  await expect(countOf(waiting)).toHaveText(String(seed.items.length));
  for (let drawn = 5; drawn < seed.items.length; drawn += 5) await nextFive(waiting).click();
  await expect(itemsOf(waiting)).toHaveCount(seed.items.length);
}

test("Dashboard is today's first tab, opens at /dashboard and loads its chunk only when opened", async ({
  page,
}) => {
  const key = await seedPhaseIssue(SEEDED_LEGION);
  const chunks: string[] = [];
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (request.resourceType() === "script" && path.startsWith("/assets/DashboardTab-")) {
      chunks.push(path);
    }
  });
  const reads = watchApiRequests(page);

  // The bare address still opens Spec, with Dashboard first and today's ten tabs after it.
  await page.goto(`/issues/${key}`);
  const tabs = page.getByRole("tablist", { name: "Issue detail" }).getByRole("tab");
  await expect(tabs).toHaveText(["Dashboard", ...TODAYS_TABS]);
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expectLoadSettled(reads);
  expect(chunks, "today's page fetched Dashboard's chunk before the tab opened").toEqual([]);

  // Opening the tab fetches its chunk and writes its address.
  await page.getByRole("tab", { exact: true, name: "Dashboard" }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${key}/dashboard$`));
  await expect(page.locator("#issue-dashboard-panel")).toBeVisible();
  await expect
    .poll(() => chunks.length, { message: "opening the tab fetched no Dashboard chunk" })
    .toBe(1);

  // The address opens the tab.
  await openDashboard(page, key);

  // No key opens it: `t w` is a chord today's page does not bind.
  await page.goto(`/issues/${key}`);
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await focusThePage(page);
  await page.keyboard.press("t");
  const chord = page.getByTestId("chord-indicator");
  await expect(chord).toHaveText(/t/);
  await page.keyboard.press("w");
  await expect(chord).toHaveCount(0);
  await expect(page).toHaveURL(new RegExp(`/issues/${key}$`));
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(page.getByRole("tab", { exact: true, name: "Dashboard" })).toHaveAttribute(
    "aria-selected",
    "false"
  );
});

/** The issue's Legion state of each phase row, with one claim, so the agents part stands
 *  wherever the issue is one Legion runs. */
function phaseState(phase: LegionStateSeed["phase"], configured = true): LegionStateSeed {
  return { configured, phase, roles: [{ role: "architect", state: "working" }] };
}

/** The track's region, failing the row where Dashboard draws none. */
async function trackOf(panel: Locator): Promise<Locator> {
  const track = panel.getByRole("region", { exact: true, name: "Phase" });
  await expect(track, "Dashboard draws no phase track").toBeVisible();
  await expect(track.getByRole("listitem")).toHaveText(
    STEPS.map((step) => new RegExp(`^${step}\\b`))
  );
  await expect(track.getByRole("button")).toHaveCount(0);
  await expect(track.getByRole("link")).toHaveCount(0);
  return track;
}

/** The track marks `marked` (none where null) and stands beside `words` alone of the phase
 *  words (none where null). */
async function expectPhase(
  track: Locator,
  marked: (typeof STEPS)[number] | null,
  words: (typeof PHASE_LINES)[number] | null
): Promise<void> {
  const current = track.locator('[aria-current="step"]');
  if (marked === null) {
    await expect(current).toHaveCount(0);
  } else {
    await expect(current).toHaveCount(1);
    await expect(current).toHaveText(new RegExp(`^${marked}\\b`));
  }
  for (const line of PHASE_LINES) {
    if (line === words) await expect(track).toContainText(line);
    else await expect(track).not.toContainText(line);
  }
}

test.describe("the phase track reads each phase the issue's Legion state gives", () => {
  test("the phase track reads planning", async ({ page }) => {
    const key = await seedPhaseIssue(phaseState("planning"));
    await writeRounds(key);
    const track = await trackOf(await openDashboard(page, key));
    await expectPhase(track, "Planning", null);
    // Planning, Implementing and Testing carry their rounds; Reviewing, with none, and the four
    // steps that take no handoff carry no count.
    expect(ROUNDS).toEqual({ implement: 3, plan: 2, review: 0, test: 1 });
    await expect(track.getByRole("listitem")).toHaveText([
      /^Admitted$/,
      /^Planning\s*2$/,
      /^Implementing\s*3$/,
      /^Testing\s*1$/,
      /^Reviewing$/,
      /^Accepting$/,
      /^Retro$/,
      /^Merging$/,
    ]);
  });

  for (const row of [
    {
      marked: "Admitted",
      phase: "awaiting_person",
      title: "awaiting_person",
      words: "Waiting on a person",
    },
    { marked: null, phase: "held", title: "held", words: "Held" },
    { marked: "Merging", phase: "integrating", title: "integrating", words: "Integrating" },
    { marked: "Merging", phase: "awaiting_merge", title: "awaiting_merge", words: null },
    {
      marked: null,
      phase: UNLISTED,
      title: "a phase the list does not hold",
      words: "In another phase",
    },
  ] as const) {
    test(`the phase track reads ${row.title}`, async ({ page }) => {
      const key = await seedPhaseIssue(phaseState(row.phase));
      const track = await trackOf(await openDashboard(page, key));
      await expectPhase(track, row.marked, row.words);
    });
  }

  for (const row of [
    { legion: phaseState("done"), title: "no track and no agents where the phase is done" },
    {
      legion: phaseState("unrecorded"),
      title: "no track and no agents where the phase is unrecorded",
    },
    {
      legion: phaseState(null),
      title: "no track and no agents where the Legion state holds no phase",
    },
    {
      legion: phaseState("planning", false),
      title: "no track and no agents where the project names no daemon",
    },
  ]) {
    test(row.title, async ({ page }) => {
      const key = await seedPhaseIssue(row.legion);
      const panel = await openDashboard(page, key);
      await expect(waitingOf(panel), "Dashboard draws no list of what waits").toBeVisible();
      await expect(panel.getByRole("region", { exact: true, name: "Phase" })).toHaveCount(0);
      await expect(panel.getByRole("region", { exact: true, name: "Agents at work" })).toHaveCount(
        0
      );
    });
  }
});

test("an issue Legion does not run draws no track, no agents and the nothing-waits line", async ({
  page,
}) => {
  const key = await seedQuietIssue();
  expect((await readLegionIssue(key)).configured, "the quiet issue's project names a daemon").toBe(
    false
  );
  const panel = await openDashboard(page, key);
  const waiting = waitingOf(panel);
  await expect(waiting, "Dashboard draws no list of what waits").toBeVisible();
  await expect(panel.getByRole("region", { exact: true, name: "Phase" })).toHaveCount(0);
  await expect(panel.getByRole("region", { exact: true, name: "Agents at work" })).toHaveCount(0);
  await expect(itemsOf(waiting)).toHaveCount(0);
  await expect(nextFive(waiting)).toHaveCount(0);
  // One line, of the page's declared words, says nothing waits.
  const lines = (await waiting.innerText())
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && line !== "Waiting on you" && !/^\d+$/.test(line));
  expect(lines, "the list of what waits holds no line, or more than one").toHaveLength(1);
  expect(
    wordsIn(lines[0] ?? "").filter((word) => !DECLARED_WORDS.includes(word)),
    "the nothing-waits line draws words the page's list does not declare"
  ).toEqual([]);
});

// SLOW(>=2s): the whole Dashboard seed - an issue with seven children, six handoffs, five checks
// and four results read through to the fake GitHub, six questions, five decisions, two approvals
// and three threads, each written through its route - before one load of the tab.
test("what waits on the person lists each item once, newest first, five at a time", async ({
  page,
}) => {
  const seed = await seedDashboard(thisMain());
  // What decides the list's scope, as the server reads it: `me/replies` lists the Legion
  // daemon's thread beside the other two and leaves out the open ask the questions part holds;
  // the spec's approval waits on a request, and the Verification document's is stale, by the
  // reader.
  const unread = ((await listMyReplies({ issue: seed.key, state: "unread" })).rows ?? []).map(
    (row) => row.thread_id
  );
  expect(unread).toEqual(
    expect.arrayContaining([seed.threads.daemon, seed.threads.comment, seed.threads.message])
  );
  expect(unread).not.toContain(seed.asks.replied);
  const documents = (await getIssue(seed.key)).artifacts ?? [];
  const approvalOf = (id: string) => documents.find((document) => document.id === id)?.approval;
  expect(approvalOf(seed.specArtifactId)?.state).toBe("awaiting");
  expect(approvalOf(seed.verificationArtifactId)).toMatchObject({
    by: { id: READER },
    state: "stale",
  });

  const waiting = waitingOf(await openDashboard(page, seed.key));
  await expect(waiting, "Dashboard draws no list of what waits").toBeVisible();
  await expect(countOf(waiting)).toHaveText(String(seed.items.length));
  await expect
    .poll(() => drawnItems(waiting), { message: "the first five are not the newest five" })
    .toEqual(seed.items.slice(0, 5).map(testIdOf));
  await expect(nextFive(waiting)).toContainText(String(seed.items.length - 5));
  await nextFive(waiting).click();
  await expect
    .poll(() => drawnItems(waiting), { message: "the list is not each item once, newest first" })
    .toEqual(seed.items.map(testIdOf));
  await expect(nextFive(waiting)).toHaveCount(0);

  // The Legion daemon's thread is no item; the open ask stands once, in the questions part; the
  // spec's stale approval folds into its request's item, which names the version it asks for.
  await expect(itemOf(waiting, { id: seed.threads.daemon, kind: "thread" })).toHaveCount(0);
  await expect(itemOf(waiting, { id: seed.asks.replied, kind: "thread" })).toHaveCount(0);
  await expect(itemOf(waiting, { id: seed.specArtifactId, kind: "stale" })).toHaveCount(0);
  await expect(itemOf(waiting, { id: seed.verificationArtifactId, kind: "stale" })).toHaveCount(1);
  const request = itemOf(waiting, { id: seed.asks.approval.id, kind: "approval" });
  await expect(request.locator("[data-ask-body]")).toHaveCount(1);
  await expect(request).toContainText(new RegExp(`\\b${seed.asks.approval.version}\\b`));
  for (const item of seed.items.filter((one) => one.kind === "question")) {
    await expect(itemOf(waiting, item).locator("[data-ask-body]")).toHaveCount(1);
  }
  for (const item of seed.items.filter((one) => one.kind === "decision")) {
    await expect(itemOf(waiting, item).locator("[data-decision-card]")).toHaveCount(1);
  }
});

// SLOW(>=2s): the whole Dashboard seed, then seven acts in place, each read back from the server.
test("each item is acted on in place and leaves the list and the count", async ({ page }) => {
  const seed = await seedDashboard(thisMain());
  const waiting = waitingOf(await openDashboard(page, seed.key));
  await expect(waiting, "Dashboard draws no list of what waits").toBeVisible();
  await expect(countOf(waiting)).toHaveText(String(seed.items.length));
  const origin = await page.evaluate(() => performance.timeOrigin);
  let left = seed.items.length;
  const gone = async (item: WaitingItem) => {
    left -= 1;
    await expect(itemOf(waiting, item), `${testIdOf(item)} stays in the list`).toHaveCount(0);
    await expect(countOf(waiting)).toHaveText(String(left));
  };

  // A thread item links to the thread's own address.
  await expect(
    itemOf(waiting, { id: seed.threads.message, kind: "thread" }).locator(
      `a[href$="/issues/${seed.key}/messages/${seed.threads.message}"]`
    )
  ).not.toHaveCount(0);
  await expect(
    itemOf(waiting, { id: seed.threads.comment, kind: "thread" }).locator(
      `a[href$="/issues/${seed.key}/comments/${seed.threads.comment}"]`
    )
  ).not.toHaveCount(0);

  // The approval request, answered with Approve.
  const request: WaitingItem = { id: seed.asks.approval.id, kind: "approval" };
  // The ask body answers an approval request as every surface does: its Approve option, then
  // Answer.
  await itemOf(waiting, request)
    .getByRole("radio", { name: /^Approve/ })
    .check();
  await itemOf(waiting, request).getByRole("button", { exact: true, name: "Answer" }).click();
  await gone(request);
  await expect
    .poll(async () => (await getArtifact(seed.specArtifactId)).approval?.state)
    .toBe("approved");

  // The stale Verification approval, approved again at the newest version.
  const stale: WaitingItem = { id: seed.verificationArtifactId, kind: "stale" };
  await itemOf(waiting, stale)
    .getByRole("button", { name: /^Approve v\d+$/ })
    .click();
  await gone(stale);
  await expect
    .poll(async () => {
      const approval = (await getArtifact(seed.verificationArtifactId)).approval;
      return [approval?.state, approval?.version === approval?.latest_version];
    })
    .toEqual(["approved", true]);

  // A Change on the second decision, anchored to its words.
  const changed: WaitingItem = { id: seed.decisions.second.id, kind: "decision" };
  const changedCard = itemOf(waiting, changed).locator("[data-decision-card]");
  await changedCard.getByRole("button", { exact: true, name: "Change" }).click();
  await changedCard.getByRole("textbox", { name: "Your change" }).fill("Count the threads apart.");
  await changedCard.getByRole("button", { exact: true, name: "Send change" }).click();
  await gone(changed);
  await expect
    .poll(async () =>
      (await listComments(seed.key, seed.decisionsDocumentId))
        .filter((comment) => comment.body === "Count the threads apart.")
        .map((comment) => [comment.author.id, comment.anchor?.quote ?? ""])
    )
    .toEqual([[READER, expect.stringContaining(seed.decisions.second.words)]]);

  // Keep on the first decision.
  const kept: WaitingItem = { id: seed.decisions.first.id, kind: "decision" };
  await itemOf(waiting, kept)
    .locator("[data-decision-card]")
    .getByRole("button", { exact: true, name: "Keep" })
    .click();
  await gone(kept);
  if (!thisMain().decisionsAreNodes) {
    await expect
      .poll(async () => {
        const decision = ((await getIssueDecisions(seed.key)).decisions ?? []).find(
          (one) => one.block_id === seed.decisions.first.id
        );
        return [decision?.kept?.kept_by, decision?.waiting_review];
      })
      .toEqual([READER, false]);
  }

  // A question answered by one of its options.
  const byOption: WaitingItem = { id: seed.asks.byOption.id, kind: "question" };
  await itemOf(waiting, byOption).getByRole("radio", { name: seed.asks.byOption.option }).check();
  await itemOf(waiting, byOption).getByRole("button", { exact: true, name: "Answer" }).click();
  await gone(byOption);
  await expect
    .poll(async () => {
      const { ask } = await getAsk(seed.asks.byOption.id);
      return [ask.state, ask.answer?.selected];
    })
    .toEqual(["answered", [seed.asks.byOption.option]]);

  // A question answered in words.
  const inWords: WaitingItem = { id: seed.asks.inWords, kind: "question" };
  await itemOf(waiting, inWords).locator("[data-ask-answer]").fill("Nothing waits on you.");
  await itemOf(waiting, inWords).getByRole("button", { exact: true, name: "Answer" }).click();
  await gone(inWords);
  await expect
    .poll(async () => {
      const { ask } = await getAsk(seed.asks.inWords);
      return [ask.state, ask.answer?.text];
    })
    .toEqual(["answered", "Nothing waits on you."]);

  // A reply to the comment thread, which moves the reader's read mark past the planner's reply.
  const thread: WaitingItem = { id: seed.threads.comment, kind: "thread" };
  await itemOf(waiting, thread).getByRole("textbox", { name: "Reply" }).fill("Thank you.");
  await itemOf(waiting, thread).getByRole("button", { exact: true, name: "Send" }).click();
  await gone(thread);
  await expect
    .poll(async () => {
      const replies = (await getComment(seed.threads.comment)).replies ?? [];
      const unread = (await listMyReplies({ issue: seed.key, state: "unread" })).rows ?? [];
      return [
        replies.some((reply) => reply.body === "Thank you." && reply.author.id === READER),
        unread.some((row) => row.thread_id === seed.threads.comment),
      ];
    })
    .toEqual([true, false]);

  // Every act left in place: the page never loaded again.
  expect(await page.evaluate(() => performance.timeOrigin), "the page loaded again").toBe(origin);
});

/** What each tile counts, computed from the reads Dashboard reads by R6's rule: the commit the
 *  page names is the newest standing result's (`namedCommit`), and a check passed at it where it
 *  reads Passed (`checkStanding`) on a result recorded at that commit, or on its own where no
 *  result names a commit; the statements of `coverage` each of whose checks in `results` (by
 *  their `serves`) passed at it; the children done of the issue read; and the open questions
 *  whose turn is not the reader's. */
async function countedTiles(key: string): Promise<Record<keyof typeof TILES, TileCount>> {
  const statements = ((await getIssueCoverage(key)).requirements ?? []).map(
    (requirement) => requirement.block_id
  );
  const checks = (await getIssueResults(key)).checks ?? [];
  const serves = (check: object): readonly unknown[] =>
    "serves" in check && Array.isArray(check.serves) ? check.serves : [];
  const commit = namedCommit(checks);
  const passed = checks.filter((check) => {
    const standing = checkStanding(check);
    return (
      standing.state === "passed" &&
      (commit === undefined || standing.stands?.commit_sha === commit)
    );
  });
  const issue = await getIssue(key);
  const children = issue.children ?? [];
  const open = (await listIssueAsks(key)).filter((ask) => ask.state === "open");
  return {
    children: {
      ids: children.filter((child) => child.status === "done").map((child) => child.key),
      whole: children.length,
    },
    met: {
      ids: statements.filter((statement) => {
        const served = checks.filter((check) =>
          serves(check).includes(`dispatch://${key}/spec#b-${statement}`)
        );
        return served.length > 0 && served.every((check) => passed.includes(check));
      }),
      whole: statements.length,
    },
    passed: { ids: passed.map((check) => check.block_id), whole: checks.length },
    questions: {
      ids: open
        .filter((ask) => !(ask.waiting_on === "human" && ask.waiting_on_person === READER))
        .map((ask) => ask.id),
      whole: open.length,
    },
  };
}

// SLOW(>=2s): the whole Dashboard seed, then four presses, each opening the list under its tile.
test("the four tiles count what the reads hold and open their lists", async ({ page }) => {
  const seed = await seedDashboard(thisMain());
  const panel = await openDashboard(page, seed.key);
  const progress = panel.getByRole("region", { exact: true, name: "Progress" });
  await expect(progress, "Dashboard draws no tiles").toBeVisible();

  const counted = await countedTiles(seed.key);
  const sorted = (tile: TileCount) => ({ ids: [...tile.ids].sort(), whole: tile.whole });
  for (const tile of Object.keys(TILES) as (keyof typeof TILES)[]) {
    expect(
      sorted(counted[tile]),
      `the reads count ${tile} otherwise than the seed made it`
    ).toEqual(sorted(seed.tiles[tile]));
  }

  /** Each of `hrefs` that links `id`. */
  const linking = (hrefs: readonly string[], id: string) =>
    hrefs.filter((href) => new RegExp(`(?:^|[/#=-])${literal(id)}(?:$|[/?#&])`).test(href));
  const linked: Partial<Record<keyof typeof TILES, readonly string[]>> = {};
  for (const [tile, name] of Object.entries(TILES) as [keyof typeof TILES, string][]) {
    const { ids, whole } = counted[tile];
    const button = progress.getByRole("button", { name: new RegExp(`^${name}`) });
    const meter = button.getByRole("meter");
    await expect(meter, `the ${name} tile`).toHaveAttribute("aria-valuenow", String(ids.length));
    await expect(meter, `the ${name} tile`).toHaveAttribute("aria-valuemax", String(whole));
    await expect(button).toContainText(String(ids.length));
    await button.click();
    await expect(button).toHaveAttribute("aria-expanded", "true");
    const links = progress.getByRole("list", { exact: true, name }).getByRole("link");
    await expect(links).toHaveCount(ids.length);
    const hrefs = await links.evaluateAll((anchors) =>
      anchors.map((anchor) => anchor.getAttribute("href") ?? "")
    );
    for (const id of ids) {
      expect(linking(hrefs, id), `the ${name} list does not link ${id} once`).toHaveLength(1);
    }
    linked[tile] = hrefs;
  }

  // R6: the statement whose two checks passed at different commits. Its older check reads Passed
  // on its own row, and Out of date at the commit the page names, the newer check's: the statement
  // counts in no Met tile, and that check in no passed tile.
  const { newer, older, statement } = seed.twoCommits;
  const olderCheck = ((await getIssueResults(seed.key)).checks ?? []).find(
    (check) => check.block_id === older
  );
  expect(
    olderCheck === undefined ? undefined : checkStanding(olderCheck).state,
    `${older} does not read Passed on its own row`
  ).toBe("passed");
  expect(counted.met.ids, `${statement} counts as met`).not.toContain(statement);
  expect(counted.passed.ids, `${older} counts as passed`).not.toContain(older);
  expect(counted.passed.ids, `${newer} does not count as passed`).toContain(newer);
  expect(linking(linked.met ?? [], statement), `the met list links ${statement}`).toEqual([]);
  expect(linking(linked.passed ?? [], older), `the passed list links ${older}`).toEqual([]);

  // The new page's Verification view reads each statement by the same rule: that statement
  // Unknown while both its checks read Passed on their rows, and Met exactly the statements the
  // Met tile counts.
  await withIssuePage(page.context(), READER);
  await openPreset(page, seed.key, "verification");
  const specSlug = (await getArtifact(seed.specArtifactId)).slug;
  const verificationSlug = (await getArtifact(seed.verificationArtifactId)).slug;
  const card = verificationCard(page, blockNodeId(specSlug, statement));
  for (const id of [older, newer]) {
    await expect(
      card.locator(
        `[data-verification-check][data-node-id="${blockNodeId(verificationSlug, id)}"]`
      ),
      `the Verification view does not draw ${id} Passed on its row`
    ).toHaveAttribute("data-node-state", "passed");
  }
  await expect(card, `the Verification view does not draw ${statement} Unknown`).toHaveAttribute(
    "data-statement-state",
    "unknown"
  );
  await expect(card.locator("span[data-statement-state]")).toHaveText(
    VERIFICATION_WORDS.states.unknown
  );
  for (const id of ((await getIssueCoverage(seed.key)).requirements ?? []).map(
    (requirement) => requirement.block_id
  )) {
    await expect(
      verificationCard(page, blockNodeId(specSlug, id)),
      `the Verification view and the Met tile read ${id} otherwise`
    ).toHaveAttribute(
      "data-statement-state",
      counted.met.ids.includes(id) ? "met" : /^(?:not_met|unknown)$/
    );
  }
});

test("the agents at work are the roles that hold a claim, in the role words", async ({ page }) => {
  const key = await seedPhaseIssue({
    configured: true,
    phase: "implementing",
    roles: EVERY_CLAIM_STATE,
  });
  // The roles in the order the issue's Legion state gives them, a retired claim among them.
  const roles = (await readLegionIssue(key)).state?.roles ?? [];
  expect(roles.map((role) => role.state).sort()).toEqual(
    EVERY_CLAIM_STATE.map((role) => role.state).sort()
  );
  const listed = roles.filter((role) => role.state !== "retired");

  const panel = await openDashboard(page, key);
  const agents = panel.getByRole("region", { exact: true, name: "Agents at work" });
  await expect(agents, "Dashboard draws no agents at work").toBeVisible();
  await expect(agents.getByRole("listitem")).toHaveText(
    listed.map(
      (role) =>
        new RegExp(
          `^${literal(role.role)}\\b[\\s\\S]*\\b${ROLE_WORDS[role.state] ?? "in another state"}$`,
          "i"
        )
    )
  );
});

/** Whether a read of Dashboard's first load is one R8 names on this main, for `seed`: the issue,
 *  its open asks, its handoffs, its Legion state, the reader's unread replies of the issue with
 *  no `limit`, the own route of each thread drawn, the decisions and the Changes' comments of
 *  the decisions document, the results and the coverage, the batched reference targets, and a
 *  picture's version with the summary. Where dispatch://INFRA-1253 is on main, its tree read. */
function namedByR8(seed: DashboardSeed, method: string, sent: URL): boolean {
  if (method !== "GET") return false;
  const issue = `/api/v1/issues/${seed.key}`;
  const query = [...sent.searchParams]
    .map(([name, value]) => `${name}=${value}`)
    .sort()
    .join("&");
  const threads = seed.items.filter((item) => item.kind === "thread").map((item) => item.id);
  switch (sent.pathname) {
    case issue:
    case `${issue}/handoffs`:
    case `${issue}/legion`:
    case `${issue}/decisions`:
    case `${issue}/results`:
    case `${issue}/coverage`:
    case `${issue}/summary`:
      return query === "";
    case `${issue}/asks`:
      return query === "state=open";
    case "/api/v1/me/replies":
      return query === `issue=${seed.key}&state=unread`;
    case `${issue}/comments`:
      return query === `artifact=${seed.decisionsDocumentId}`;
    case "/api/v1/reference-targets":
      return true;
    case `${issue}/tree`:
      return seed.holds.treeRead;
  }
  const own = /^\/api\/v1\/(?:comments|issues\/[^/]+\/messages)\/([^/]+)$/.exec(sent.pathname);
  if (own !== null) return threads.includes(own[1] ?? "");
  return new RegExp(`^${literal(issue)}/artifacts/[^/]+/versions/\\d+$`).test(sent.pathname);
}

// SLOW(>=2s): the whole Dashboard seed, two loads of today's page and a press of the tab, each
// read until nothing has been in flight for 1.5 s.
test("Dashboard sends only the reads R8 names", async ({ context, page }) => {
  const seed = await seedDashboard(thisMain());
  const reads = watchApiRequests(page);
  await page.goto(`/issues/${seed.key}`);
  await expectLoadSettled(reads);
  // Today's page with no Dashboard, over the same time: what it sends is the shell's own.
  const control = await context.newPage();
  const shell = watchApiRequests(control);
  await control.goto(`/issues/${seed.key}`);
  await expectLoadSettled(shell);

  const tab = page.getByRole("tab", { exact: true, name: "Dashboard" });
  await expect(tab, "today's page draws no Dashboard tab").toBeVisible();
  // The shell's own polls - the reads of no issue that today's page with no Dashboard made as it
  // loaded, the agents registry and the credential requests among them - repeat on their own
  // clock, on either page, at any time after: a window the control page polled in is no window
  // this page polls in. A read of the issue is held to the windows below.
  const shellPolls = new Set(
    shell.requests
      .filter((sent) => !sent.route.includes("/issues/"))
      .map((sent) => `${sent.method} ${sent.route}`)
  );
  reads.reset();
  shell.reset();
  await tab.click();
  const waiting = waitingOf(page.locator("#issue-dashboard-panel"));
  await expect(waiting, "Dashboard draws no list of what waits").toBeVisible();
  await expect(itemsOf(waiting)).toHaveCount(5);
  await expectLoadSettled(reads);
  const shellRoutes = new Set(shell.requests.map((sent) => `${sent.method} ${sent.route}`));
  const dashboards = reads.requests.filter(
    (sent) =>
      !shellRoutes.has(`${sent.method} ${sent.route}`) &&
      !shellPolls.has(`${sent.method} ${sent.route}`)
  );
  expect(
    dashboards
      .filter((sent) => !namedByR8(seed, sent.method, new URL(sent.path, "http://dashboard")))
      .map((sent) => `${sent.method} ${sent.path}`),
    "Dashboard's first load sent reads R8 does not name"
  ).toEqual([]);
  expect(
    dashboards.filter((sent) =>
      /^\/api\/v1\/(?:comments\/[^/?]+|issues\/[^/]+\/messages\/[^/?]+|asks\/[^/?]+)$/.test(
        sent.path
      )
    ).length,
    "Dashboard read more than five threads"
  ).toBeLessThanOrEqual(5);

  // The reply control reads the agents only once the reader reaches it.
  reads.reset();
  await itemOf(waiting, { id: seed.threads.comment, kind: "thread" })
    .getByRole("textbox", { name: "Reply" })
    .focus();
  await expectLoadSettled(reads);
  expect(
    reads.requests
      .filter(
        (sent) =>
          !shellRoutes.has(`${sent.method} ${sent.route}`) &&
          !shellPolls.has(`${sent.method} ${sent.route}`)
      )
      .filter(
        (sent) =>
          !(sent.method === "GET" && sent.path === "/api/v1/agents") &&
          !namedByR8(seed, sent.method, new URL(sent.path, "http://dashboard"))
      )
      .map((sent) => `${sent.method} ${sent.path}`),
    "reaching the reply control sent a read other than the agents"
  ).toEqual([]);
  await control.close();
});

/** Every string the reads of `seed`'s issue hold, its whitespace folded: the records whose text
 *  Dashboard may draw. */
async function recordTexts(seed: DashboardSeed): Promise<readonly string[]> {
  const reads: unknown[] = [
    await getIssue(seed.key),
    await listIssueAsks(seed.key),
    await getIssueDecisions(seed.key),
    await getIssueResults(seed.key),
    await getIssueCoverage(seed.key),
    await listMyReplies({ issue: seed.key, state: "unread" }),
    await getComment(seed.threads.comment),
    await getMessage(seed.key, seed.threads.message),
    await readLegionIssue(seed.key),
    ...(await Promise.all(
      seed.items.filter((item) => item.kind === "question").map((item) => getAsk(item.id))
    )),
  ];
  const strings: string[] = [];
  const walk = (value: unknown): void => {
    if (typeof value === "string") strings.push(value.replace(/\s+/g, " ").trim());
    else if (typeof value === "object" && value !== null) Object.values(value).forEach(walk);
  };
  walk(reads);
  return strings.filter((text) => text !== "");
}

// SLOW(>=2s): the whole Dashboard seed, every item drawn, and ten reads of what the issue holds.
test("every text node of Dashboard is a number, a name, a time, a declared word or a record's text", async ({
  page,
}) => {
  const seed = await seedDashboard(thisMain());
  const panel = await openDashboard(page, seed.key);
  const waiting = waitingOf(panel);
  await expect(waiting, "Dashboard draws no list of what waits").toBeVisible();
  await drawEveryItem(waiting, seed);
  await expect(panel.getByRole("region", { exact: true, name: "Progress" })).toBeVisible();
  await expect(panel.getByRole("region", { exact: true, name: "Agents at work" })).toBeVisible();

  const drawn = await panel.evaluate((root) => {
    const found: { text: string; time: boolean }[] = [];
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const parent = node.parentElement;
      const text = (node.textContent ?? "").replace(/\s+/g, " ").trim();
      if (parent === null || text === "" || !parent.checkVisibility()) continue;
      found.push({ text, time: parent.closest("time") !== null });
    }
    return found;
  });
  const records = await recordTexts(seed);
  const names = new Set(seed.names.flatMap(wordsIn));
  // The words the shared controls Dashboard draws carry as their own, the same on every surface:
  // each writing box's hint (`DISPATCH_WRITING_BOXES`, the shared field reads its own), and the
  // one approval control's actions, which name the version they approve (`doc/ApprovalChip.tsx`,
  // R5).
  const hints = new Set(DISPATCH_WRITING_BOXES.map((row) => row.hint));
  const shared = (text: string) => hints.has(text) || /^Approve v\d+$/.test(text);
  // A reference in a record's words is drawn by its target's name, which a chip cuts to its
  // room with an ellipsis: the words before it are the target's own.
  const cut = (text: string) => text.replace(/…$/u, "");
  const explained = ({ text, time }: { text: string; time: boolean }) =>
    time ||
    shared(text) ||
    wordsIn(text).every(
      (word) => DECLARED_WORDS.includes(word) || /^v?\d+$/.test(word) || names.has(word)
    ) ||
    records.some((record) => record.includes(cut(text)));
  expect(
    drawn.filter((node) => !explained(node)).map((node) => node.text),
    "Dashboard draws text that is no number, name, time, declared word or record's text"
  ).toEqual([]);
  expect(
    drawn.filter((node) => node.text.includes("dispatch://")).map((node) => node.text),
    "Dashboard draws a reference by its address rather than its name"
  ).toEqual([]);
});

// SLOW(>=2s): the whole Dashboard seed before one load of the tab and one press of the control.
test("the decision card on Dashboard draws the author from the read it makes", async ({ page }) => {
  const seed = await seedDashboard(thisMain());
  const waiting = waitingOf(await openDashboard(page, seed.key));
  await expect(waiting, "Dashboard draws no list of what waits").toBeVisible();
  await drawEveryItem(waiting, seed);
  const cards = waiting.locator("[data-decision-card]");
  await expect(cards).toHaveCount(2);
  for (const decision of [seed.decisions.second, seed.decisions.first]) {
    const card = itemOf(waiting, { id: decision.id, kind: "decision" }).locator(
      "[data-decision-card]"
    );
    await expect(card).toContainText(decision.heading);
    // On the bare `decisions` member no read holds an author, so every card says so; where the
    // read gives `decided_by`, the card names it.
    await expect(card).toContainText(seed.decisions.author);
    if (thisMain().decisionsAreNodes) await expect(card).not.toContainText("Author unknown");
  }
});
