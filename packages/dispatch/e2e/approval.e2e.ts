import { setTimeout as sleep } from "node:timers/promises";

import type { WriteAdvice } from "@legion/contracts";
import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts/dispatch-tools";
import type { Artifact } from "../web/src/api/types";
import { COPY_REF_SELECTOR } from "../web/src/features/refs/CopyRefButton";

import { setLiveSessions } from "./agents";
import {
  answerAsk,
  createArtifactAsk,
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createNamedVersion,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  getIssueEvents,
  listIssueAsks,
  patchIssue,
  rejectSuggestion,
  requestApproval,
  resolveAsk,
  reviewArtifact,
  setGatedSteps,
  withOpenedAsk,
} from "./api";
import { bodyOf } from "./bodies";
import { documentEditor, needsYouCards, openSpecAndAwaitHeadingIds } from "./editor";
import { peekTrigger } from "./inbox-drawer";
import { askControls, inboxPane, inboxRow, selectRow } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { hasReadingPane } from "./reading-pane";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { assertPageFits, assertWhole } from "./unclipped";
import { asUser } from "./users";

function drawer(page: Page) {
  return page.getByRole("dialog", { name: "Inbox" });
}

/**
 * An approval request's own card, as the Inbox draws one.
 *
 * The Inbox page shows an approval as the document under review, in its reading column from
 * `READING_PANE_WIDTH` (INFRA-575) and in place of the list below it (INFRA-5200), neither of
 * which carries a card; the drawer is the Inbox that still keeps one (`pane={false}`) at every
 * width: it opens over whatever page is showing, so this navigates to one.
 */
async function approvalCard(page: Page, askId: string): Promise<Locator> {
  // The key waits for the page's heading: pressed as the page loads, before the shell has
  // registered its keys, it can open nothing.
  await page.goto("/agents");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("i");
  await expect(drawer(page)).toBeVisible();
  return drawer(page).getByTestId(`ask-${askId}`);
}

const session = {
  actor: { kind: "session" as const, id: "e2e-session", origin: { session_title: "architect" } },
  as: "agent" as const,
};

// @pr: Create through answer to close: an approval requested, answered in the Inbox, made stale by an edit, and changes requested from the header.
test("a spec's approval is a human review pinned to its version: requested by the agent, answered in the Inbox, stale after an edit, changes requested from the header @pr", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  const artifactID = issue.primary_artifact_id;

  // The agent asks for approval; the Inbox shows the request and the two reviews it offers.
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  expect(requested.version).toBe(1);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // From 1280 px an approval's own surface in the Inbox is the reading column (INFRA-575):
    // the document at the version under review, with Approve and Request changes in the
    // column's head. That head is where the reader answers it.
    const head = await askControls(page, requested.ask.id);
    const approve = head.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve).toBeVisible();
    const requestChanges = head.getByRole("button", { exact: true, name: "Request changes" });
    await expect(requestChanges).toBeVisible();
    // Request changes needs a reason, and says so by holding its own button rather than
    // refusing the review silently.
    await requestChanges.click();
    const reason = page.getByRole("dialog", { name: "Request changes" });
    await expect(reason.getByRole("button", { name: "Request changes" })).toBeDisabled();
    await reason.getByRole("button", { name: "Cancel" }).click();
    await expect(reason).toHaveCount(0);
    await approve.click();
    await expect(inboxRow(page, requested.ask.id)).toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");

    // The document header shows the approval pinned to version 1.
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v1" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);

    // The agent edits the spec: the approval goes stale.
    await editArtifact(
      artifactID,
      { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
      session
    );
    await expect
      .poll(async () => ((await getArtifact(artifactID, { login: "alice" })).versions ?? []).length)
      .toBeGreaterThan(1);
    await createNamedVersion(artifactID, "revised", { login: "alice" });
    await page.reload();
    await expect(page.getByRole("button", { name: "Approved v1 · changed since" })).toBeVisible();

    // The human requests changes from the header; a reason is required.
    await page.getByRole("button", { name: "Request changes" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("button", { name: "Request changes" })).toBeDisabled();
    await dialog.getByLabel("Reason").fill("Name the rollback path.");
    await dialog.getByRole("button", { name: "Request changes" }).click();
    await expect(page.getByRole("button", { name: "Changes requested" })).toBeVisible();

    // The history lists both reviews with their versions.
    await page.getByRole("button", { name: "Changes requested" }).click();
    const reviews = page.getByRole("dialog", { name: "Reviews" });
    const afterReview = await getArtifact(artifactID, { login: "alice" });
    const pinned = afterReview.approval?.version;
    expect(afterReview.approval?.state).toBe("changes_requested");
    expect(pinned).toBeGreaterThan(1);
    await expect(reviews).toContainText(`requested changes on v${pinned}`);
    await expect(reviews).toContainText("Name the rollback path.");
    await expect(reviews).toContainText("approved v1");

    // The event contract the daemon consumes.
    const events = (await getIssueEvents(issue.key, {}, { login: "alice" })) as unknown as Array<{
      type: string;
      payload: Record<string, unknown>;
    }>;
    const approved = events.find((event) => event.type === "artifact.approved");
    const changes = events.find((event) => event.type === "artifact.changes_requested");
    expect(approved?.payload).toMatchObject({
      artifact_id: artifactID,
      version: 1,
      ask_id: requested.ask.id,
    });
    expect(changes?.payload).toMatchObject({
      artifact_id: artifactID,
      version: pinned,
      reason: "Name the rollback path.",
      ask_id: null,
    });
  } finally {
    await alice.close();
  }
});

// @pr: A write the server refuses: a reason past the server's bound shows the server's own words in the Request changes dialog.
test("a reason longer than the server takes shows the server's own refusal in the Request changes dialog @pr", async ({
  browser,
}) => {
  await createProject({ key: "LONGREASON", name: "Long reason" });
  const issue = await createIssue({
    project: "LONGREASON",
    spec: "The plan.",
    title: "Reason past the bound",
  });
  const artifactID = issue.primary_artifact_id;
  await requestApproval(artifactID, {}, session);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { exact: true, name: "Request changes" }).click();
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    const field = dialog.getByLabel("Reason");
    await expect(field).toBeVisible();

    // One UTF-16 unit past the bound the server holds a review's reason to. `fill` puts the
    // whole text in through the page's own editing pipeline, the path a paste takes, and the
    // last character is not whitespace, so the server's trim leaves the length as written.
    const reason = `${bodyOf(DISPATCH_EVENT_BODY_MAX)}x`;
    await field.fill(reason);
    await expect.poll(async () => (await field.inputValue()).length).toBe(reason.length);

    await dialog.getByRole("button", { exact: true, name: "Request changes" }).click();

    // What the reader is shown is the server's refusal, which names the field and the bound,
    // in place of the sentence the dialog says for a failure it cannot read.
    await expect(dialog.getByRole("alert")).toHaveText(
      `reason is 1 characters over the ${DISPATCH_EVENT_BODY_MAX}-character limit ` +
        `(${reason.length}/${DISPATCH_EVENT_BODY_MAX})`
    );
    await expect(dialog.getByText("Could not request changes. Try again.")).toHaveCount(0);

    // The draft stays in the field, and no review was written.
    await expect(field).toHaveValue(reason);
    expect((await getArtifact(artifactID, { login: "alice" })).approval?.state).toBe("awaiting");
  } finally {
    await alice.close();
  }
});

/** A document write's response as the restore tests read it: the advice beside its result. */
interface AdvisedWrite {
  readonly advice?: WriteAdvice;
}

/** The decision block the restore tests hold open in a spec. */
const waveAsk =
  ':::ask{#wave urgency="med" multiple="false"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n';

/**
 * Creates a spec holding `spec`, waits for each of its decision blocks to be an ask, has alice
 * approve version 1 from its header, then has the agent change "The plan." by accident. The edit
 * stales the approval and its advice says so. Returns the spec's issue.
 */
async function approveThenChange(
  spec: string,
  blocks: number
): Promise<{ key: string; id: string }> {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec, title: "Design gate" });
  const id = issue.primary_artifact_id;
  await expect
    .poll(async () => (await listIssueAsks(issue.key)).filter((ask) => ask.block_artifact).length)
    .toBe(blocks);
  expect((await reviewArtifact(id, { state: "approved" }, { login: "alice" })).version).toBe(1);
  const edited = (await editArtifact(
    id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  )) as AdvisedWrite;
  expect(edited.advice?.superseded_approval).toEqual({
    approved_by: "alice",
    approved_version: 1,
    version: 2,
  });
  expect((await getArtifact(id, { login: "alice" })).approval).toMatchObject({
    latest_version: 2,
    state: "stale",
    version: 1,
  });
  return { id, key: issue.key };
}

test("the agent's undo of an accidental edit to an approved spec makes that approval current at the undo's version, so no approval request is needed", async ({
  browser,
}) => {
  const issue = await approveThenChange("The plan.", 0);

  // The undo's text is version 1's again: version 3 carries alice's approval of version 1, and
  // the undo superseded no approval.
  const undone = (await editArtifact(
    issue.id,
    { ops: [{ op: "replace", find: "The revised plan.", with: "The plan." }] },
    session
  )) as AdvisedWrite;
  expect(undone.advice?.superseded_approval).toBeUndefined();
  expect((await getArtifact(issue.id, { login: "alice" })).approval).toMatchObject({
    by: { id: "alice", kind: "user" },
    latest_version: 3,
    state: "approved",
    version: 3,
  });
  expect((await getArtifactVersion(issue.id, 3, { login: "alice" })).markdown).toBe(
    (await getArtifactVersion(issue.id, 1, { login: "alice" })).markdown
  );
  const events = (await getIssueEvents(issue.key, {}, { login: "alice" })) as unknown as Array<{
    type: string;
    payload: Record<string, unknown>;
  }>;
  expect(
    events.find((event) => event.type === "artifact.approved" && event.payload.version === 3)
      ?.payload
  ).toMatchObject({
    artifact_id: issue.id,
    reason:
      "Carried forward from the approval of version 1: this version's text is that version's, unchanged.",
  });

  // A request for approval finds the approval at the latest version and opens nothing.
  expect((await requestApproval(issue.id, {}, session)).ask).toBeNull();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v3" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an undo one character away from the approved spec leaves its approval stale", async ({
  browser,
}) => {
  const issue = await approveThenChange("The plan.", 0);

  await editArtifact(
    issue.id,
    { ops: [{ op: "replace", find: "The revised plan.", with: "The plan!" }] },
    session
  );
  expect((await getArtifactVersion(issue.id, 3, { login: "alice" })).markdown).toBe(
    (await getArtifactVersion(issue.id, 1, { login: "alice" })).markdown.replace("plan.", "plan!")
  );
  expect((await getArtifact(issue.id, { login: "alice" })).approval).toMatchObject({
    latest_version: 3,
    state: "stale",
    version: 1,
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v1 · changed since" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve v3" })).toBeVisible();
  } finally {
    await alice.close();
  }
});

// A version carries an approval only where its text is the approved text exactly, or where every
// change is one Dispatch carries an approval over: a reading, a server-owned attribute of an ask
// block, or the decisions step's own write. A version holding the approved text around a block the
// agent reworded is neither, so its approval stays stale.
test("an undo that restores the approved text around a decision block the agent reworded leaves the approval stale", async ({
  browser,
}) => {
  const issue = await approveThenChange(`The plan.\n\n${waveAsk}`, 1);

  await editArtifact(
    issue.id,
    {
      ops: [
        { op: "replace", find: "The revised plan.", with: "The plan." },
        {
          op: "replace",
          find: "How many regions take the first wave?",
          with: "How many regions go in the first wave?",
        },
      ],
    },
    session
  );
  const restored = (await getArtifactVersion(issue.id, 3, { login: "alice" })).markdown;
  expect(restored).toBe(
    (await getArtifactVersion(issue.id, 1, { login: "alice" })).markdown.replace(
      "take the first wave?",
      "go in the first wave?"
    )
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The restored text is one character away from the approved version, so the approval stays
    // stale at version 1 with version 3 the latest. The read is polled rather than slept on:
    // nothing settles into this artifact - `editArtifact` wrote version 3 itself, and no page
    // is in the room - which 16 reads 250 ms apart measured here, every one of them this same
    // answer. A poll that cannot pass at once would say the opposite, so it is the right shape
    // either way: a verdict that arrived late would still be read.
    await expect
      .poll(async () => (await getArtifact(issue.id, { login: "alice" })).approval)
      .toMatchObject({
        latest_version: 3,
        state: "stale",
        version: 1,
      });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v1 · changed since" })).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("an approval request stays in one Inbox card while its document version moves", async ({
  browser,
}) => {
  await createProject({ key: "FOLLOW", name: "Approval follow" });
  const issue = await createIssue({
    project: "FOLLOW",
    spec: "The plan.",
    title: "Follow approval",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = withOpenedAsk(
    await requestApproval(artifactID, { summary: "Caps retries at three attempts." }, session)
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // One request, one item: the row it is, in the band whose turn it is. From 1280 px the
    // card the Inbox drew for it is the reading column's document (INFRA-575), so the row
    // carries the question's own words and the column's head carries Approve.
    const row = inboxRow(page, requested.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "human");

    await editArtifact(
      artifactID,
      { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
      session
    );
    let movedArtifact: Artifact | undefined;
    await expect
      .poll(async () => {
        movedArtifact = await getArtifact(artifactID, { login: "alice" });
        return (movedArtifact.versions ?? []).length;
      })
      .toBeGreaterThan(1);
    if (movedArtifact === undefined) throw new Error("the moved artifact was not read");
    const movedVersion = Math.max(
      ...(movedArtifact.versions ?? []).map((version) => version.number)
    );

    await page.reload();
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    await expect(row).toContainText(
      `Approve spec.md (version ${movedVersion})? Caps retries at three attempts.`
    );
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on agents (1)");

    const handedBack = withOpenedAsk(
      await requestApproval(artifactID, { summary: "Adds the rollback budget." }, session)
    );
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await page.goto("/");
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await expect(row).toContainText(
      `Approve spec.md (version ${movedVersion})? Adds the rollback budget.`
    );
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect(row).toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval)
      .toMatchObject({ state: "approved", version: movedVersion, ask_id: requested.ask.id });
  } finally {
    await alice.close();
  }
});

test("a human comment, revision, and hand-back return an approval card to Waiting on you", async ({
  browser,
}) => {
  await createProject({ key: "TURN", name: "Approval turn" });
  const issue = await createIssue({
    project: "TURN",
    spec: "The plan.",
    title: "Hand-back turn",
  });
  const requested = withOpenedAsk(
    await requestApproval(
      issue.primary_artifact_id,
      { summary: "Names the initial proposal." },
      session
    )
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  );
  await requestApproval(issue.primary_artifact_id, { summary: "Clarifies the rollout." }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // The band the question sits in is the row's, at every width: from 1280 px an approval's
    // card in the Inbox is the reading column's document (INFRA-575), not a card in the list.
    await expect(inboxRow(page, requested.ask.id)).toHaveAttribute("data-inbox-section", "human");
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");
  } finally {
    await alice.close();
  }
});

test("an unchanged hand-back after a human comment and a progress note returns the card to Waiting on you", async ({
  browser,
}) => {
  await createProject({ key: "SAME", name: "Unchanged hand-back" });
  const issue = await createIssue({
    project: "SAME",
    spec: "The plan.",
    title: "Unchanged hand-back",
  });
  const summary = "Names the existing proposal.";
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary }, session)
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Checking the rollout.", turn: "agent" },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = inboxRow(page, requested.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");

    // The same version, question and summary: the hand-back alone changes whose turn it is,
    // and the open Inbox moves on the ask.handed_back event it records, which rewords nothing
    // - the question waits on the reader again, live. The row the reading column is showing
    // is held in the band they saw it in until they look away (INFRA-575, `heldRow`), so its
    // band is read on their next look at the list.
    const handedBack = withOpenedAsk(
      await requestApproval(issue.primary_artifact_id, { summary }, session)
    );
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await expect(page.getByRole("button", { name: /^Blocked on you: 1 item/ })).toBeVisible();
    await page.reload();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await expect(row).toContainText(`Approve spec.md (version 1)? ${summary}`);
    // Nothing the Inbox draws of it - the row's own words, and the column beside them - says
    // the question was reworded.
    await expect(page.locator("main")).not.toContainText("Edited");
    const askEvents = (await getIssueEvents(issue.key, { limit: 200 }, { login: "alice" }))
      .filter((event) => event.type.startsWith("ask."))
      .map((event) => event.type);
    expect(askEvents).toEqual(["ask.opened", "ask.handed_back"]);
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");
  } finally {
    await alice.close();
  }
});

test("a hand-back is an Activity line, live and after a reload, and leaves one ask card", async ({
  browser,
}) => {
  await createProject({ key: "LINE", name: "Hand-back line" });
  const issue = await createIssue({ project: "LINE", spec: "The plan.", title: "Hand-back line" });
  const requested = withOpenedAsk(
    await requestApproval(
      issue.primary_artifact_id,
      { summary: "Names the existing proposal." },
      session
    )
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The first events subscription resumes from the sign-in check's head. Once its socket opens,
    // the hand-back below reaches this tab.
    const streaming = page.waitForEvent(
      "websocket",
      (socket) => new URL(socket.url()).pathname === "/ws/live"
    );
    await page.goto(`/issues/${issue.key}/activity`);
    await streaming;
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    const handBackLine = rows.filter({
      hasText: `handed “${requested.ask.question}” back for approval`,
    });
    await expect(handBackLine).toHaveCount(0);

    // The human's comment left the request waiting on its agent, so this call hands it back.
    const handedBack = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await expect(handBackLine).toHaveCount(1);
    // The hand-back rewords nothing: no edit line anywhere.
    await expect(rows.filter({ hasText: "edited the question" })).toHaveCount(0);

    await page.reload();
    await expect(handBackLine).toHaveCount(1);

    // The Conversation draws no line for any of it: the ask is still exactly one card.
    await page.getByRole("tab", { name: "Conversation" }).click();
    const askTurn = page.locator(`[data-turn="ask:${requested.ask.id}"]`);
    await expect(askTurn).toHaveCount(1);
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .getByText("back for approval", { exact: false })
    ).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an answer started before a hand-back that rewords nothing is saved, and the card gains no edit history", async ({
  browser,
}) => {
  await createProject({ key: "KEEP", name: "Kept answer" });
  const issue = await createIssue({ project: "KEEP", spec: "The plan.", title: "Kept answer" });
  const summary = "Names the existing proposal.";
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary }, session)
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    // No live socket on this page: the card stays as the human loaded it, mid-answer. An
    // answer can be half-made only where the Inbox draws the question's own card, which for an
    // approval from 1280 px is the drawer rather than the page's reading column (INFRA-575):
    // the column shows the document under review, whose Approve is one press with no draft to
    // hold. The drawer keeps today's card at every width.
    const answering = await alice.newPage();
    await answering.routeWebSocket(/\/ws\/live$/u, (route) =>
      route.close({ code: 1011, reason: "test network drop" })
    );
    await answering.goto("/agents");
    await answering.locator("body").focus();
    await answering.keyboard.press("i");
    const shown = drawer(answering)
      .locator('[data-inbox-section="agent"]')
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(shown).toBeVisible();
    await shown.getByRole("radio", { name: /^Approve/ }).check();

    await requestApproval(issue.primary_artifact_id, { summary }, session);

    // Another page sees the hand-back: the question is the human's turn again, and the card the
    // drawer draws for it was never reworded.
    const watching = await alice.newPage();
    await watching.goto("/");
    await expect(inboxRow(watching, requested.ask.id)).toHaveAttribute(
      "data-inbox-section",
      "human"
    );
    await watching.goto("/agents");
    await watching.locator("body").focus();
    await watching.keyboard.press("i");
    const handedBack = drawer(watching).getByTestId(`ask-${requested.ask.id}`);
    await expect(handedBack).toBeVisible();
    await expect(handedBack).not.toContainText("Edited");

    await shown.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(
      answering.getByText("Your answer was not saved, because the question changed.")
    ).toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(issue.primary_artifact_id, { login: "alice" })).approval)
      .toMatchObject({ state: "approved", version: 1, ask_id: requested.ask.id });
  } finally {
    await alice.close();
  }
});

test("an approval ask's Inbox card shows a question carrying a long summary whole @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  // Nearly as long as the ask bound leaves a summary, budgeted as the server budgets it
  // (`approvalQuestion` in `internal/dispatch/api/reviews.go`): the bound on an ask's question
  // and options together, less the question at the longest version a request can reach, less
  // the two fixed options it carries, less the space before the summary. Cut at a word, so the
  // text still reads as a sentence.
  const options = [
    "Approve",
    "Approve this version of the document.",
    "Request changes",
    "Say what must change before it can be approved.",
  ].reduce((units, text) => units + text.length, 0);
  const room =
    DISPATCH_EVENT_BODY_MAX - "Approve spec.md (version 9999999999)?".length - options - 1;
  const opening =
    "Retries a failed push at most three times, one minute apart, and then stops and names the push that failed. ";
  const ending = "Nothing else in the retry path changes.";
  const filler = opening.repeat(Math.ceil(room / opening.length)).slice(0, room - ending.length);
  const summary = filler.slice(0, filler.lastIndexOf(" ") + 1) + ending;
  expect(summary.length).toBeGreaterThan(room - 15);
  expect(summary.length).toBeLessThanOrEqual(room);
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary }, session)
  );
  expect(requested.ask.question).toBe(`Approve spec.md (version 1)? ${summary}`);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The question's own card is where the Inbox shows the question itself: the drawer's, since
    // the Inbox page's reading column and its reading view show an approval as the document
    // under review rather than a card (INFRA-575, INFRA-5200) and its short row gives the
    // question one line on purpose.
    const card = await approvalCard(page, requested.ask.id);
    await expect(card).toBeVisible();
    const question = card.locator("p", { hasText: ending });
    await expect(question).toHaveText(requested.ask.question);
    // Whole is the card's promise, so the window is made tall enough to hold the whole card:
    // what is left to flag is a clip, not a surface scrolling as it is meant to. A summary at
    // this bound is thousands of lines, past any height a person's window has, so the height
    // is the one the card itself reports rather than a number written here.
    const viewport = page.viewportSize();
    if (viewport !== null) {
      const needed = await question.evaluate((element) => {
        let height = element.scrollHeight;
        for (let node: Element | null = element; node !== null; node = node.parentElement) {
          height = Math.max(height, node.scrollHeight);
        }
        return height;
      });
      await page.setViewportSize({ height: needed + 100, width: viewport.width });
      await expect(question).toHaveText(requested.ask.question);
    }
    // Whole means nothing between the question and its card clips it: no clamp, ellipsis or box
    // shorter or narrower than the text it holds.
    await assertWhole(question);
    await card.screenshot({ path: testInfo.outputPath("approval-long-question.png") });
  } finally {
    await alice.close();
  }
});

// A settlement that writes no version is proved by outlasting the window it would run in: the
// harness holds it to 250 ms (`DISPATCH_TEST_SETTLE_DELAY`, `e2e/run-server.sh`), so a second of
// real time covers four of them. It is the server's own timer, which no page clock reaches.
const SETTLED_MS = 1_000;

// SLOW(>=2s): Two users, two pages, and a settle window that must be outlasted to prove no
// version was written.
test("reading an approved spec and moving the caret through its numbered list leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  // A spec holding a numbered list, which the caret moves through below.
  const issue = await createIssue({
    project: "GATE",
    spec: "## Acceptance\n\n1. The migration ships.\n2. The error rate stays flat.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // The reading column's head is where the Inbox offers the review from 1280 px (INFRA-575).
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");

    // Bob reads the approved spec, and his editor gives its heading an id. Then he clicks into
    // the list and moves the caret, typing nothing: the first such move has his editor label
    // each numbered item, which changes the token again and none of the text.
    const reader = await bob.newPage();
    const opened = await openSpecAndAwaitHeadingIds(
      reader,
      issue.key,
      artifactID,
      "The error rate stays flat."
    );
    await documentEditor(reader).getByText("The migration ships.").click();
    for (const key of ["ArrowDown", "ArrowUp", "End", "Home"]) {
      await reader.keyboard.press(key);
    }
    await expect.poll(async () => (await getArtifactText(artifactID)).token).not.toBe(opened);
    // Settlement runs 250 ms after the room's last update (`DISPATCH_TEST_SETTLE_DELAY`,
    // `e2e/run-server.sh`), and a version it wrote for either update would leave the approval
    // pinned to an older one. A second of real time is four of those windows.
    await sleep(SETTLED_MS);
    const artifact = await getArtifact(artifactID, { login: "alice" });
    expect(artifact.versions).toHaveLength(1);
    expect(artifact.approval).toMatchObject({ latest_version: 1, state: "approved", version: 1 });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("an agent's comment on part of an identifier in an approved spec leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({
    project: "GATE",
    spec: "## Plan\n\nRename the user_id column.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");

    // The comment's mark starts inside `user_id`, splitting the word's text where the
    // underscore sits. That changes nothing a version stores.
    await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "id column" }, body: "Is this indexed?" },
      session
    );
    // Settlement runs 250 ms after the room's last update (`DISPATCH_TEST_SETTLE_DELAY`,
    // `e2e/run-server.sh`), and a version it wrote would leave the approval pinned to an older
    // one. A second of real time is four of those windows.
    await sleep(SETTLED_MS);
    const artifact = await getArtifact(artifactID, { login: "alice" });
    expect(artifact.versions).toHaveLength(1);
    expect(artifact.approval).toMatchObject({ latest_version: 1, state: "approved", version: 1 });
  } finally {
    await alice.close();
  }
});

test("rejecting a suggestion on part of an identifier in an approved spec leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({
    project: "GATE",
    spec: "## Plan\n\nMigrate snake_case_name first.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  // The suggestion's mark starts inside `snake_case_name`, splitting the word's text beside an
  // underscore; rejecting it removes the mark and joins the text again.
  const suggestion = await createComment(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "case_name" },
      body: "Name the column for what it holds.",
      suggestion: { replace_with: "case_label" },
    },
    session
  );
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");
    const approved = (await getArtifact(artifactID, { login: "alice" })).approval;

    await rejectSuggestion(suggestion.id, { login: "bob" });
    // Settlement runs 250 ms after the room's last update (`DISPATCH_TEST_SETTLE_DELAY`,
    // `e2e/run-server.sh`), and a version it wrote would leave the approval pinned to an older
    // one. A second of real time is four of those windows.
    await sleep(SETTLED_MS);
    expect((await getArtifact(artifactID, { login: "alice" })).approval).toMatchObject({
      latest_version: approved?.version,
      state: "approved",
      version: approved?.version,
    });
  } finally {
    await alice.close();
  }
});

test("an approval request for an unassigned issue's non-primary document remains actionable while the primary is draft", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "GATE", name: "Gate" });
  // A create always lands on a person, so the issue nobody holds is created and then cleared.
  const created = await createIssue(
    { project: "GATE", spec: "Primary review target.", title: "Two approval targets" },
    session
  );
  const issue = await patchIssue(created.key, { assignee: null }, session);
  expect(issue.assignee).toBeNull();
  const supporting = await createIssueArtifact(
    issue.key,
    { content: "Supporting review target.", name: "supporting-design.md" },
    session
  );
  const nonPrimary = withOpenedAsk(await requestApproval(supporting.artifact.id, {}, session));
  await expect
    .poll(
      async () => (await getArtifact(issue.primary_artifact_id, { login: "alice" })).approval?.state
    )
    .toBe("draft");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Nobody holds the shared-token issue, so its request reaches the default assignee of its
    // project - alice, who made GATE - and her own list holds it, where it is as answerable as
    // through the issue itself (the margin below).
    await page.goto("/");
    await expect(inboxRow(page, nonPrimary.ask.id)).toBeVisible();
    // Actionable means the Inbox offers the review where it reads the request: the head over
    // the document under review, in the reading column from 1280 px (INFRA-575) and in place of
    // the list below it (INFRA-5200).
    const controls = await askControls(page, nonPrimary.ask.id);
    await expect(controls.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(
      controls.getByRole("button", { exact: true, name: "Request changes" })
    ).toBeVisible();
    const inboxShot = testInfo.outputPath("approval-non-primary-inbox-1280.png");
    await page.screenshot({ path: inboxShot, fullPage: true });
    await testInfo.attach("non-primary approval in the project default's Inbox", {
      contentType: "image/png",
      path: inboxShot,
    });

    // Every open issue ask remains visible in the issue margin; the target being a secondary
    // document must not hide this decision while the primary approval stays draft.
    await page.goto(`/issues/${issue.key}/artifacts/${supporting.artifact.slug}`);
    const margin = needsYouCards(page);
    const nonPrimaryMarginCard = margin.getByTestId(`ask-${nonPrimary.ask.id}`);
    await expect(nonPrimaryMarginCard).toBeVisible();
    await expect(nonPrimaryMarginCard).toContainText("supporting-design.md");
    await nonPrimaryMarginCard
      .getByRole("radio", { name: "Approve Approve this version of the document." })
      .check();
    await expect(
      nonPrimaryMarginCard.getByRole("button", { exact: true, name: "Answer" })
    ).toBeVisible();
    const issueShot = testInfo.outputPath("approval-non-primary-issue-1280.png");
    await page.screenshot({ path: issueShot, fullPage: true });
    await testInfo.attach("non-primary approval in issue margin", {
      contentType: "image/png",
      path: issueShot,
    });
  } finally {
    await alice.close();
  }
});

test("an approval ask's Inbox row, drawer link and bare ask route land on the spec with the approve control in view, while an anchored ask and an unanchored question still land where they do today", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  // Alice's own issue, so the request is in her Inbox and her drawer, which both open on the
  // reader's own work; the agent still requests the approval.
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
  const specPath = `/issues/${issue.key}/spec?ask=${requested.ask.id}`;

  const anchoredAsk = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote: "The plan" }, question: "Which plan?" },
    session
  );
  const unanchoredAsk = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship it?" },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The Inbox row's own link for the request lands on the spec, approve control in view — not
    // the Conversation turn `useItemLanding` fell through to before this fix. From 1280 px the
    // reading column's head carries the selected question's owner link too (INFRA-575), and it
    // names the same address; following the row's link is the row's click, not the list's
    // selection, so the issue it opens stays open.
    await page.goto("/");
    const head = (await askControls(page, requested.ask.id)).locator("[data-inbox-owner]");
    const owner = inboxRow(page, requested.ask.id).locator("[data-inbox-owner]");
    await expect(owner).toHaveAttribute("href", (await head.getAttribute("href")) ?? "");
    await owner.click();
    await expect(page).toHaveURL(specPath);
    await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();

    // The drawer's card names the document and version it asks about, as a link; following it
    // opens the spec and closes the drawer. The drawer rides the shell the reader is already in,
    // so this leg needs no cold route of its own.
    await navigateInApp(page, "/agents");
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    const drawerCard = drawer(page).getByTestId(`ask-${requested.ask.id}`);
    await expect(drawerCard).toBeVisible();
    const documentLink = drawerCard.getByRole("link", { name: "spec.md, version 1" });
    await expect(documentLink).toBeVisible();
    await documentLink.click();
    await expect(page).toHaveURL(specPath);
    await expect(drawer(page)).toHaveCount(0);

    // An anchored ask still lands beside its document, and an unanchored question still lands
    // on its Conversation turn: the approval fallback only fires when neither applies.
    await page.goto(`/issues/${issue.key}/asks/${anchoredAsk.id}`);
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?ask=${anchoredAsk.id}`);

    await page.goto(`/issues/${issue.key}/asks/${unanchoredAsk.id}`);
    await expect(page).toHaveURL(`/issues/${issue.key}/asks/${unanchoredAsk.id}`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(
      page.locator("#issue-conversation-panel").getByTestId(`ask-${unanchoredAsk.id}`)
    ).toContainText("Ship it?");
  } finally {
    await alice.close();
  }
});

test("an open approval ask's document link reaches the issue's Conversation, and a project document's own approval link reaches its document, while the margin omits the link on both", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue(
    { project: "GATE", spec: "The plan.", title: "Design gate" },
    session
  );
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));

  const doc = await createProjectDocument(
    "GATE",
    { content: "# Runbook\n", name: "runbook.md" },
    session
  );
  const docApproval = withOpenedAsk(await requestApproval(doc.artifact.id, {}, session));
  const docPath = `/projects/GATE/documents/${doc.artifact.slug}?ask=${docApproval.ask.id}`;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The issue's Conversation names the document the open request is about.
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationCard = page
      .locator("#issue-conversation-panel")
      .getByTestId(`ask-${requested.ask.id}`);
    const conversationLink = conversationCard.getByRole("link", { name: "spec.md, version 1" });
    await expect(conversationLink).toBeVisible();
    await conversationLink.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?ask=${requested.ask.id}`);

    // The issue margin lists the same ask (it is already on that document), without the link.
    const marginCard = needsYouCards(page).getByTestId(`ask-${requested.ask.id}`);
    await expect(marginCard).toBeVisible();
    await expect(marginCard.getByRole("link", { name: /spec\.md, version/ })).toHaveCount(0);

    // A project-level document's approval request links its own document the same way. A
    // document has no assignee, so its question reaches the default assignee of its project -
    // alice, who made GATE - and her own list is where the Inbox lists it. The link is the
    // short row's own owner link at every width, the reading pane drawing the document itself in
    // place of a card (INFRA-575, INFRA-5200).
    await page.goto("/");
    const inboxLink = inboxRow(page, docApproval.ask.id).locator("[data-inbox-owner]");
    await expect(inboxLink).toHaveAttribute("href", docPath);

    await page.goto(`/projects/GATE/documents/${doc.artifact.slug}`);
    const docMarginCard = needsYouCards(page).getByTestId(`ask-${docApproval.ask.id}`);
    await expect(docMarginCard).toBeVisible();
    await expect(docMarginCard.getByRole("link", { name: /runbook\.md, version/ })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an approval ask keeps its document link and the version it asked about once it is answered, even after the document moves further", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue(
    { project: "GATE", spec: "The plan.", title: "Design gate" },
    session
  );
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
  await answerAsk(
    requested.ask.id,
    { expected_edited_at: null, selected: ["Approve"] },
    { login: "alice" }
  );

  // The document moves to a later version after the approval; the completed card still names
  // the version this request approved, and its link still opens the current document.
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  );
  await expect
    .poll(
      async () =>
        ((await getArtifact(issue.primary_artifact_id, { login: "alice" })).versions ?? []).length
    )
    .toBeGreaterThan(1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationCard = page
      .locator("#issue-conversation-panel")
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(conversationCard).toContainText("Answered by");
    const link = conversationCard.getByRole("link", { name: "spec.md, version 1" });
    await expect(link).toBeVisible();
    await link.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?ask=${requested.ask.id}`);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// R2, the approval state: each Inbox row of an issue wears the approval state of its issue,
// and its reading pane draws the issue's design gate, in the issue page's words.
// ---------------------------------------------------------------------------------------------

/** The approval mark of one Inbox row, on the page or in the drawer. */
function markOf(scope: Page | Locator, rowId: string): Locator {
  return scope.locator(`[data-inbox-row="${rowId}"] [data-inbox-approval]`);
}

/** The reading column's head. */
function paneHead(page: Page): Locator {
  return inboxPane(page).locator("header").first();
}

/** The head's gate lines, in their order. */
function paneLines(page: Page): Locator {
  return paneHead(page).locator("[data-inbox-approval-line]");
}

/** One gate line of the head, by its step: "" is the line of the primary document alone. */
function paneLine(page: Page, step: string): Locator {
  return paneHead(page).locator(`[data-inbox-approval-line="${step}"]`);
}

/** Reads each gate line the head draws, in order, as the issue page draws a step line: the step's
 *  word, where the line has one, and then its state. */
async function expectGateLines(
  page: Page,
  lines: readonly (readonly [step: string, word: string | undefined, state: string])[]
): Promise<void> {
  await expect(paneLines(page)).toHaveCount(lines.length);
  expect(
    await paneLines(page).evaluateAll((drawn) =>
      drawn.map((line) => line.getAttribute("data-inbox-approval-line"))
    )
  ).toEqual(lines.map(([step]) => step));
  for (const [step, word, state] of lines) {
    const line = paneLine(page, step);
    await expect(line.getByText(state, { exact: true }), `${step}: ${state}`).toBeVisible();
    if (word === undefined) {
      await expect(line, `${step} draws its state alone`).toHaveText(state);
    } else {
      await expect(line.locator(":scope > span").first(), `${step}'s word`).toHaveText(word);
    }
  }
}

/** A new version of the spec `id`, written by the session, which moves its latest version. */
async function editSpec(id: string): Promise<void> {
  const before = (await getArtifact(id, { login: "alice" })).approval?.latest_version ?? 0;
  await editArtifact(
    id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  );
  await expect
    .poll(async () => (await getArtifact(id, { login: "alice" })).approval?.latest_version)
    .toBeGreaterThan(before);
}

/** An issue of `project` with a thread of the reader's in it, which is a Reply row of their
 *  Inbox. It is forced past the near-copy check, which titles of one test would trip. */
async function issueWithThread(project: string, title: string, parent?: string) {
  const issue = await createIssue({
    force: true,
    project,
    spec: "The plan.",
    title,
    ...(parent === undefined ? {} : { parent }),
  });
  const thread = await seedCommentThread(issue.key, `Is "${title}" ready?`, "Read it and see.");
  return { issue, row: replyRowId(thread), spec: issue.primary_artifact_id };
}

// SLOW(>=2s): eight issues seeded, each with a thread and its own approval history, then each of
// five rows selected and its pane read, a card read in the drawer and one Approve waited on.
test("the five approval states of an issue", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "STATES", name: "Five states" });
  await setGatedSteps("STATES", ["spec"]);
  const approve = (id: string) => reviewArtifact(id, { state: "approved" }, { login: "alice" });
  const verificationOf = (key: string) =>
    createIssueArtifact(key, {
      content: "# Verification\n\nOne check.\n",
      name: "verification.md",
    });

  const approved = await issueWithThread("STATES", "Approved at its latest version");
  await approve(approved.spec);
  const awaiting = await issueWithThread("STATES", "Under an open request");
  const request = withOpenedAsk(await requestApproval(awaiting.spec, {}, session));
  const edited = await issueWithThread("STATES", "Approved and then edited");
  await approve(edited.spec);
  await editSpec(edited.spec);
  const sentBack = await issueWithThread("STATES", "Asked for changes");
  await reviewArtifact(
    sentBack.spec,
    { reason: "Name the rollback path.", state: "changes_requested" },
    { login: "alice" }
  );
  const never = await issueWithThread("STATES", "Never requested nor reviewed");
  // The sixth: approved at version 1, edited, and requested again.
  const again = await issueWithThread("STATES", "Approved, edited and requested again");
  await approve(again.spec);
  await editSpec(again.spec);
  const againRequest = withOpenedAsk(await requestApproval(again.spec, {}, session));
  // The seventh: an approved spec and an unapproved Verification document under one request.
  const shared = await issueWithThread("STATES", "An approved spec beside its checks");
  await verificationOf(shared.issue.key);
  await approve(shared.spec);
  const sharedRequest = withOpenedAsk(
    await requestApproval(shared.spec, { include_verification: true }, session)
  );
  // The eighth: an edited spec and the Verification document under one request.
  const both = await issueWithThread("STATES", "An edited spec beside its checks");
  await verificationOf(both.issue.key);
  await editSpec(both.spec);
  withOpenedAsk(await requestApproval(both.spec, { include_verification: true }, session));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The issue page's whole text for the eighth issue's spec, which the pane line holds.
    await page.goto(`/issues/${both.issue.key}`);
    const issueChip = page
      .locator('[data-approval-step="spec"]')
      .getByRole("button", { name: /^Awaiting approval of / });
    await expect(issueChip).toBeVisible();
    const whole = (await issueChip.innerText()).trim();

    await page.goto("/");
    // Each row's mark, read from the list without selecting it, in the issue page's words.
    const states = [
      [approved.row, "Approved v1"],
      [awaiting.row, "Awaiting approval"],
      [edited.row, "Approved v1 · changed since"],
      [sentBack.row, "Changes requested"],
      [never.row, "Not requested"],
    ] as const;
    for (const [row, state] of states) {
      await expect(markOf(page, row), row).toHaveText(state);
    }
    // The word `Draft` is drawn nowhere on the page.
    await expect(page.getByText(/\bDraft\b/)).toHaveCount(0);
    // Each pane line, as the issue page draws a step line: the word Spec, then the state.
    for (const [row, state] of states) {
      await selectRow(page, row);
      await expectGateLines(page, [["spec", "Spec", state]]);
    }

    // The open request's row: the head keeps Approve and Request changes, and its one gate line
    // is the document's own chip, so it draws one approval area.
    let head = await askControls(page, request.ask.id);
    await expect(head.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(head.getByRole("button", { exact: true, name: "Request changes" })).toBeVisible();
    await expect(paneLines(page)).toHaveCount(1);
    await expect(head.getByRole("button", { name: /^Awaiting approval/ })).toHaveCount(1);
    await expect(head.locator("[data-inbox-approval-state]")).toHaveCount(0);

    // The sixth issue: under its request again, with the same controls and one approval area.
    await expect(markOf(page, again.row)).toHaveText("Awaiting approval");
    head = await askControls(page, againRequest.ask.id);
    await expect(head.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(head.getByRole("button", { exact: true, name: "Request changes" })).toBeVisible();
    await expect(paneLines(page)).toHaveCount(1);
    await expect(head.getByRole("button", { name: /^Awaiting approval/ })).toHaveCount(1);
    // The request's card in the drawer draws the approval it follows.
    const card = await approvalCard(page, againRequest.ask.id);
    await expect(card).toContainText("Approved at version 1");
    await expect(card.getByRole("button", { name: "Show diff" })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(drawer(page)).toHaveCount(0);

    // Approving that request moves the sixth issue's thread row and its pane line.
    await page.goto("/");
    const latest = (await getArtifact(again.spec, { login: "alice" })).approval?.latest_version;
    head = await askControls(page, againRequest.ask.id);
    await head.getByRole("button", { exact: true, name: "Approve" }).click();
    await expect(markOf(page, again.row)).toHaveText(`Approved v${latest}`);
    await selectRow(page, again.row);
    await expectGateLines(page, [["spec", "Spec", `Approved v${latest}`]]);

    // The seventh issue: its spec stands approved, so the head draws no Approve, and beside the
    // gate line the approval the request follows.
    head = await askControls(page, sharedRequest.ask.id);
    await expect(head.getByRole("button", { exact: true, name: "Approve" })).toHaveCount(0);
    await expect(paneLine(page, "spec")).toContainText("Approved at version 1");
    await expect(paneLine(page, "spec").getByRole("button", { name: "Show diff" })).toBeVisible();

    // The eighth issue: the mark is the state alone, and the pane line the issue page's text.
    await expect(markOf(page, both.row)).toHaveText("Awaiting approval");
    await selectRow(page, both.row);
    await expect(paneLine(page, "spec")).toContainText(whole);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three issues seeded with a thread each, then a document written and two approvals,
// each waited on live in the row's mark and the pane, and two more rows selected and read.
test("the step that holds the gate", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "FIRSTSTEP", name: "First step" });
  await setGatedSteps("FIRSTSTEP", ["criteria", "spec"]);
  const root = await issueWithThread("FIRSTSTEP", "The gated root");
  await requestApproval(root.spec, {}, session);
  await createProject({ key: "NOSTEP", name: "No gate" });
  const ungated = await issueWithThread("NOSTEP", "An ungated issue");
  const child = await issueWithThread("FIRSTSTEP", "A child of the gated root", root.issue.key);
  await reviewArtifact(child.spec, { state: "approved" }, { login: "alice" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");

    // No Verification document written, the spec under request: the gate waits on the first
    // step, which nobody has asked for.
    await expect(markOf(page, root.row)).toHaveText("Verification · Not requested");
    await selectRow(page, root.row);
    await expectGateLines(page, [
      ["criteria", "Verification", "Not requested"],
      ["spec", "Spec", "Awaiting approval"],
    ]);

    // The Verification document approved, the spec still under request.
    const verification = await createIssueArtifact(root.issue.key, {
      content: "# Verification\n\nOne check.\n",
      name: "verification.md",
    });
    await reviewArtifact(verification.artifact.id, { state: "approved" }, { login: "alice" });
    await expect(markOf(page, root.row)).toHaveText("Spec · Awaiting approval");
    await expectGateLines(page, [
      ["criteria", "Verification", "Approved v1"],
      ["spec", "Spec", "Awaiting approval"],
    ]);

    // Both approved at their latest versions: the mark is the last step.
    await reviewArtifact(root.spec, { state: "approved" }, { login: "alice" });
    await expect(markOf(page, root.row)).toHaveText("Spec · Approved v1");
    await expectGateLines(page, [
      ["criteria", "Verification", "Approved v1"],
      ["spec", "Spec", "Approved v1"],
    ]);

    // An issue of a project that gates nothing, and a child of the gated project: one line for
    // the primary document, and a mark that names no step.
    for (const [row, state] of [
      [ungated.row, "Not requested"],
      [child.row, "Approved v1"],
    ] as const) {
      await expect(markOf(page, row), row).toHaveText(state);
      await selectRow(page, row);
      await expectGateLines(page, [["", undefined, state]]);
    }
  } finally {
    await alice.close();
  }
});

/** Which of the four renderers drew a row: the short rows beside the reading pane, or the cards
 *  below its width. */
type MarkRenderer = "short ask row" | "short thread row" | "ask card" | "thread card";

/** One row whose mark the place test reads: its id, the key of its issue, the renderer that drew
 *  it and its mark's words. */
interface PlacedMark {
  readonly id: string;
  readonly key: string;
  readonly renderer: MarkRenderer;
  readonly words: string;
}

/** The words of row `rowId` that the box of its approval mark covers by more than half a pixel
 *  each way: every text of the row outside the mark, read by the boxes its lines are drawn in. */
async function textUnderMark(page: Page, rowId: string): Promise<string[]> {
  return inboxRow(page, rowId).evaluate((row) => {
    const mark = row.querySelector("[data-inbox-approval]");
    if (mark === null) return ["(the row draws no mark)"];
    const box = mark.getBoundingClientRect();
    const covered: string[] = [];
    const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = (node.textContent ?? "").trim();
      if (text === "" || mark.contains(node)) continue;
      const range = document.createRange();
      range.selectNodeContents(node);
      for (const rect of range.getClientRects()) {
        const across = Math.min(box.right, rect.right) - Math.max(box.left, rect.left);
        const down = Math.min(box.bottom, rect.bottom) - Math.max(box.top, rect.top);
        if (across > 0.5 && down > 0.5) {
          covered.push(text);
          break;
        }
      }
    }
    return covered;
  });
}

/** Reads where each row's mark starts, and requires the marks drawn by one renderer to start at
 *  one x, within a pixel. A failure prints every row's x with the renderer that drew it. */
async function expectMarksInOnePlace(page: Page, rows: readonly PlacedMark[]): Promise<void> {
  const placed: (PlacedMark & { x: number })[] = [];
  for (const row of rows) {
    const box = await markOf(page, row.id).boundingBox();
    expect(box, `${row.renderer} ${row.id}: its mark has a box`).not.toBeNull();
    placed.push({ ...row, x: box?.x ?? Number.NaN });
  }
  const report = placed
    .map((row) => `  ${row.renderer} ${row.id} "${row.words}": x = ${row.x}`)
    .join("\n");
  for (const renderer of new Set(placed.map((row) => row.renderer))) {
    const xs = placed.filter((row) => row.renderer === renderer).map((row) => row.x);
    expect(
      Math.max(...xs) - Math.min(...xs),
      `the marks of every ${renderer} start at one x, within a pixel; the marks start at:\n${report}`
    ).toBeLessThanOrEqual(1);
  }
}

/** Where an item of a row is drawn, in the page's pixels. */
interface Box {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
}

/** The row's own items the line of its approval mark and the line of its owner draw, beside the
 *  issue key: the mark, the priority tag, the urgency mark and the copy-reference button. */
const LINE_ITEMS = {
  copy: COPY_REF_SELECTOR,
  mark: "[data-inbox-approval]",
  priority: "[data-inbox-priority]",
  urgency: "[data-inbox-urgency]",
} as const;

/** A box as a failure prints it, to a tenth of a pixel. */
function boxWords(box: Box): string {
  const at = (n: number) => Math.round(n * 10) / 10;
  return `x ${at(box.left)}..${at(box.right)}, y ${at(box.top)}..${at(box.bottom)}`;
}

/** Reads the boxes of row `row`'s own items on the line its approval mark stands on and on the
 *  line its owner stands on - the mark, the priority tag, the urgency mark, the issue key and the
 *  copy-reference button - and requires that no two of them overlap by more than half a pixel
 *  each way and that each stands inside the row's box. The key is the span of the owner's link
 *  that reads it, since a thread row's owner link carries no marker of its own. The two lines are
 *  the band from the top of the mark or the key, whichever is higher, to the bottom of the lower:
 *  what a card draws under them stands on neither. A failure prints the row and every box. */
async function expectLineItemsApart(page: Page, row: PlacedMark): Promise<void> {
  // Every caller reads each row's mark words with a waiting `expect` before this, so the list has
  // settled; the `null` branch only stops a row with no mark or key, which the expect below fails.
  const read = await inboxRow(page, row.id).evaluate(
    (element, { key, selectors }) => {
      const boxOf = (part: Element): Box => {
        const { bottom, left, right, top } = part.getBoundingClientRect();
        return { bottom, left, right, top };
      };
      const keySpan = [...element.querySelectorAll("a > span")].find(
        (span) => span.textContent?.trim() === key
      );
      const mark = element.querySelector(selectors.mark);
      if (keySpan === undefined || mark === null) return null;
      const ends = [boxOf(mark), boxOf(keySpan)];
      const top = Math.min(...ends.map((box) => box.top));
      const bottom = Math.max(...ends.map((box) => box.bottom));
      const items: { box: Box; part: string }[] = [{ box: boxOf(keySpan), part: "key" }];
      for (const [part, selector] of Object.entries(selectors)) {
        for (const item of element.querySelectorAll(selector)) {
          const box = boxOf(item);
          if (Math.min(bottom, box.bottom) - Math.max(top, box.top) > 0.5) {
            items.push({ box, part });
          }
        }
      }
      // The row is an `<li>`, whose text reads with the breaks its lines are drawn with.
      const text = element instanceof HTMLElement ? element.innerText : element.textContent;
      return { items, row: boxOf(element), text: (text ?? "").replace(/\s+/g, " ") };
    },
    { key: row.key, selectors: LINE_ITEMS }
  );
  expect(read, `${row.renderer} ${row.id}: the row draws its mark and its key ${row.key}`).not.toBe(
    null
  );
  if (read === null) return;
  const report = [
    `${row.renderer} ${row.id} "${row.words}" of ${row.key}: row ${boxWords(read.row)}, reading "${read.text}"`,
    ...read.items.map((item) => `  ${item.part}: ${boxWords(item.box)}`),
  ].join("\n");
  const overlaps: string[] = [];
  for (const [index, first] of read.items.entries()) {
    for (const second of read.items.slice(index + 1)) {
      const across =
        Math.min(first.box.right, second.box.right) - Math.max(first.box.left, second.box.left);
      const down =
        Math.min(first.box.bottom, second.box.bottom) - Math.max(first.box.top, second.box.top);
      if (across > 0.5 && down > 0.5) {
        overlaps.push(`${first.part} and ${second.part}: ${across} px across, ${down} px down`);
      }
    }
  }
  expect(
    overlaps,
    `${row.renderer} ${row.id}: no two items of the mark's line and the owner's line overlap by more than half a pixel each way:\n${report}`
  ).toEqual([]);
  const outside = read.items
    .filter(
      ({ box }) =>
        box.left < read.row.left - 0.5 ||
        box.right > read.row.right + 0.5 ||
        box.top < read.row.top - 0.5 ||
        box.bottom > read.row.bottom + 0.5
    )
    .map((item) => item.part);
  expect(
    outside,
    `${row.renderer} ${row.id}: each item of the mark's line and the owner's line stands inside the row's box:\n${report}`
  ).toEqual([]);
}

/** Reads that the issue key of ask row `row` stands on the line of its approval mark, and the
 *  start of its title after it on that line: where the line has room, the owner is drawn beside
 *  the marks, with its title cut. */
async function expectOwnerBesideMarks(page: Page, row: PlacedMark): Promise<void> {
  const boxes = await inboxRow(page, row.id).evaluate((element) => {
    const boxOf = (part: Element | null | undefined): Box | null => {
      if (part === null || part === undefined) return null;
      const { bottom, left, right, top } = part.getBoundingClientRect();
      return { bottom, left, right, top };
    };
    const key = element.querySelector("[data-inbox-owner] > span");
    return {
      key: boxOf(key),
      mark: boxOf(element.querySelector("[data-inbox-approval]")),
      title: boxOf(key?.nextElementSibling),
    };
  });
  const { key, mark, title } = boxes;
  const onMarkLine = (box: Box) =>
    mark !== null && Math.min(box.bottom, mark.bottom) - Math.max(box.top, mark.top) > 0.5;
  expect(
    key !== null &&
      title !== null &&
      onMarkLine(key) &&
      onMarkLine(title) &&
      title.left >= key.right &&
      title.right - title.left > 0.5,
    `${row.renderer} ${row.id}: its key and the start of its title stand on its mark's line: ${JSON.stringify(boxes)}`
  ).toBe(true);
}

// SLOW(>=2s): four issues seeded with a thread, an ask and an approval each, then eight rows
// measured box by box at two window sizes.
test("the approval mark stands at one place on every row", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "MARKPLACE", name: "One place" });
  await setGatedSteps("MARKPLACE", ["spec"]);
  // Four issues whose rows differ in everything beside the mark: titles of 12 and of 80
  // characters, an urgent ask and a calm one, a priority or none, and a mark of two lengths. The
  // last one's long mark, priority and urgency fill its short ask row's line at 1440, so its key
  // moves under them (`fullLine`); every other short ask row has room for its key beside them.
  const seeds = [
    { changed: false, fullLine: false, priority: 1, title: "Lease window", urgency: "high" },
    {
      changed: false,
      fullLine: false,
      priority: null,
      title: "Retire the old mirror retry budget once every region reads the shared lease sets",
      urgency: "low",
    },
    { changed: true, fullLine: false, priority: null, title: "Mirror retry", urgency: "low" },
    {
      changed: true,
      fullLine: true,
      priority: 3,
      title: "Move the nightly ledger replays onto the new storage cluster before the cutover.",
      urgency: "high",
    },
  ] as const;
  // Each issue's ask row and thread row, as the short rows and as the cards draw them, and the
  // short ask rows whose line has room for the key beside the marks.
  const short: PlacedMark[] = [];
  const cards: PlacedMark[] = [];
  const roomy: PlacedMark[] = [];
  for (const seed of seeds) {
    const { issue, row, spec } = await issueWithThread("MARKPLACE", seed.title);
    if (seed.priority !== null) await patchIssue(issue.key, { priority: seed.priority });
    const ask = await createAsk(
      issue.key,
      {
        options: [{ label: "Ship" }, { label: "Hold" }],
        question: `Ship "${seed.title}"?`,
        urgency: seed.urgency,
      },
      session
    );
    await reviewArtifact(spec, { state: "approved" }, { login: "alice" });
    if (seed.changed) await editSpec(spec);
    const words = seed.changed ? "Approved v1 · changed since" : "Approved v1";
    const { key } = issue;
    const shortAsk: PlacedMark = { id: ask.id, key, renderer: "short ask row", words };
    short.push(shortAsk, { id: row, key, renderer: "short thread row", words });
    if (!seed.fullLine) roomy.push(shortAsk);
    cards.push(
      { id: ask.id, key, renderer: "ask card", words },
      { id: row, key, renderer: "thread card", words }
    );
  }

  /** The list draws the eight rows and no other, each wearing its mark; the marks of one
   *  renderer start at one x; no mark covers any other text of its row; and no item of the
   *  mark's line or the owner's line draws over another or past its row. */
  const expectMarksInPlace = async (page: Page, rows: readonly PlacedMark[]) => {
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    for (const row of rows) {
      await expect(markOf(page, row.id), `${row.renderer} ${row.id}`).toHaveText(row.words);
    }
    await expectMarksInOnePlace(page, rows);
    for (const row of rows) {
      expect(
        await textUnderMark(page, row.id),
        `${row.renderer} ${row.id}: the text its mark covers`
      ).toEqual([]);
      await expectLineItemsApart(page, row);
    }
  };

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // Beside the reading pane: the two short rows, which draw no card.
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/");
    expect(hasReadingPane(page)).toBe(true);
    await expect(inboxPane(page)).toBeVisible();
    await expectMarksInPlace(page, short);
    await expect(page.locator("[data-inbox-row] [data-inbox-thread-card]")).toHaveCount(0);
    // Where the line has room, the owner stands beside the marks: the key, then the start of its
    // title.
    for (const row of roomy) await expectOwnerBesideMarks(page, row);

    // Below its width: the two cards. The Inbox page lists short rows there too (INFRA-5200), so
    // the cards are the drawer's, opened over a page that draws no Inbox list of its own.
    await page.setViewportSize({ height: 768, width: 1024 });
    await page.goto("/agents");
    await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    for (const row of cards) {
      await expect(
        row.renderer === "ask card"
          ? page.getByTestId(`ask-${row.id}`)
          : inboxRow(page, row.id).locator("[data-inbox-thread-card]"),
        `${row.renderer} ${row.id}`
      ).toBeVisible();
    }
    await expectMarksInPlace(page, cards);
  } finally {
    await alice.close();
  }
});

/** Reads that the approval mark and the issue key of row `rowId` stand inside the row's card,
 *  left and right. Neither shrinks, so where the two do not fit on one line the key must move
 *  under the mark rather than run past the card's edge. */
async function expectMarkAndKeyInCard(scope: Page | Locator, rowId: string): Promise<void> {
  const boxes = await scope.locator(`[data-inbox-row="${rowId}"]`).evaluate((row) => {
    const across = (element: Element | null) => {
      const box = element?.getBoundingClientRect();
      return box === undefined ? null : { left: box.left, right: box.right };
    };
    return {
      card: across(row),
      key: across(row.querySelector("[data-inbox-owner] > span")),
      mark: across(row.querySelector("[data-inbox-approval]")),
    };
  });
  const { card } = boxes;
  for (const part of ["mark", "key"] as const) {
    const at = boxes[part];
    expect(
      at !== null && card !== null && at.left >= card.left - 0.5 && at.right <= card.right + 0.5,
      `${rowId}: the ${part} stands inside its card: ${JSON.stringify(boxes)}`
    ).toBe(true);
  }
}

// SLOW(>=2s): two issues seeded, a Verification document approved and then edited and polled
// until stale, then the Inbox and the drawer over an issue page each read and measured to fit.
// The two short rows beside the pane are read by the five states and the live change, at the
// pane's width. Below the pane's width the Inbox page lists short rows too (INFRA-5200), and the
// drawer each row as a card, a claim the desktop projects cannot make, so the row runs on the
// phone projects alone. @phone @phone-only
test("the approval mark at phone width @phone @phone-only", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "PHONEMARK", name: "Phone mark" });
  const issue = await createIssue({
    project: "PHONEMARK",
    spec: "The plan.",
    title: "An approved issue",
  });
  await reviewArtifact(issue.primary_artifact_id, { state: "approved" }, { login: "alice" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the approved plan?" },
    session
  );
  const row = replyRowId(
    await seedCommentThread(issue.key, "Is the plan approved?", "It is, at version 1.")
  );
  // The longest mark a row draws beside the longest key a project takes, in its widest letters:
  // a root issue of a project that gates both steps, whose Verification was approved and then
  // changed.
  await createProject({ key: "WMWMWMWMWM", name: "Wide keys" });
  await setGatedSteps("WMWMWMWMWM", ["criteria", "spec"]);
  const wide = await createIssue({
    project: "WMWMWMWMWM",
    spec: "The plan.",
    title: "A root issue whose checks moved",
  });
  const checks = await createIssueArtifact(wide.key, {
    content: "# Verification\n\nOne check.\n",
    name: "verification.md",
  });
  await reviewArtifact(checks.artifact.id, { state: "approved" }, { login: "alice" });
  await editArtifact(
    checks.artifact.id,
    { ops: [{ op: "replace", find: "One check.", with: "Two checks." }] },
    session
  );
  await expect
    .poll(async () => (await getArtifact(checks.artifact.id, { login: "alice" })).approval?.state)
    .toBe("stale");
  const wideAsk = await createAsk(
    wide.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the checked plan?" },
    session
  );
  const longMark = "Verification · Approved v1 · changed since";

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    expect(hasReadingPane(page)).toBe(false);
    // Below the pane's width the Inbox page lists short rows (INFRA-5200), each wearing the mark,
    // and draws no card in them.
    await expect(inboxRow(page, ask.id)).toBeVisible();
    await expect(page.getByTestId(`ask-${ask.id}`)).toHaveCount(0);
    await expect(markOf(page, ask.id)).toHaveText("Approved v1");
    await expect(inboxRow(page, row).locator("[data-inbox-thread-card]")).toHaveCount(0);
    await expect(markOf(page, row)).toHaveText("Approved v1");
    // The long mark and the long key stay inside their row.
    await expect(markOf(page, wideAsk.id)).toHaveText(longMark);
    await expectMarkAndKeyInCard(page, wideAsk.id);
    await assertPageFits(page);

    // The drawer draws each row as a card over another page, and each card wears the mark.
    await page.goto(`/issues/${issue.key}`);
    await peekTrigger(page).click();
    await expect(drawer(page)).toBeVisible();
    await expect(drawer(page).getByTestId(`ask-${ask.id}`)).toBeVisible();
    await expect(
      drawer(page).locator(`[data-inbox-row="${row}"] [data-inbox-thread-card]`)
    ).toBeVisible();
    await expect(markOf(drawer(page), ask.id)).toHaveText("Approved v1");
    await expect(markOf(drawer(page), row)).toHaveText("Approved v1");
    await expect(markOf(drawer(page), wideAsk.id)).toHaveText(longMark);
    await expectMarkAndKeyInCard(drawer(page), wideAsk.id);
    await assertPageFits(page);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): six rows seeded over a project document, an agent and three issues, then five rows
// dismissed one press at a time and the Dismissed view loaded and read.
test("a row with no issue carries no mark", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "NOISSUE", name: "No issue" });
  // Three items of no issue: an ask on a project document, a thread on one, and an Agent row.
  const runbook = await createProjectDocument("NOISSUE", {
    content: "# Runbook\n\nRestart it.\n",
    name: "Runbook",
  });
  const docAsk = await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    session
  );
  const docRoot = await createArtifactComment(runbook.artifact.id, {
    body: "Is the runbook current?",
  });
  await createArtifactComment(
    runbook.artifact.id,
    { body: "It is.", reply_to: docRoot.id },
    asSession()
  );
  const agent = agentRowId(
    await seedAgentThread({ body: "Check the runbook, please.", replies: ["Checked."] })
  );
  const docThread = replyRowId(docRoot.id);
  // Three that belong to an issue: one nobody asked about, one approved, and one approved and
  // then closed.
  const unasked = await issueWithThread("NOISSUE", "Nobody asked about it");
  const approved = await issueWithThread("NOISSUE", "Approved");
  await reviewArtifact(approved.spec, { state: "approved" }, { login: "alice" });
  const closed = await issueWithThread("NOISSUE", "Approved and closed");
  await reviewArtifact(closed.spec, { state: "approved" }, { login: "alice" });
  await patchIssue(closed.issue.key, { status: "done" });

  const marked = [
    [unasked.row, "Not requested"],
    [approved.row, "Approved v1"],
    [closed.row, "Approved v1"],
  ] as const;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    for (const id of [docAsk.id, docThread, agent]) {
      await expect(inboxRow(page, id), id).toBeVisible();
      await expect(markOf(page, id), id).toHaveCount(0);
    }
    for (const [id, state] of marked) {
      await expect(markOf(page, id), id).toHaveText(state);
    }

    // The Dismissed view draws the same rows the same way.
    for (const id of [docThread, agent, ...marked.map(([row]) => row)]) {
      await selectRow(page, id);
      await page.locator(`[data-inbox-dismiss="${id}"]`).first().click();
      await expect(inboxRow(page, id), id).toHaveCount(0);
    }
    await page.goto("/?show=dismissed");
    for (const id of [docThread, agent]) {
      await expect(inboxRow(page, id), id).toBeVisible();
      await expect(markOf(page, id), id).toHaveCount(0);
    }
    for (const [id, state] of marked) {
      await expect(markOf(page, id), id).toHaveText(state);
    }
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): six approval changes written while the page is open, each waited on live in two
// marks and the pane's line with the ask's selection read after each, then the Dismissed view
// loaded and one more approval waited on.
test("a change recorded while the Inbox is open", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "LIVEGATE", name: "A moving gate" });
  const issue = await createIssue({ project: "LIVEGATE", spec: "The plan.", title: "Live gate" });
  const spec = issue.primary_artifact_id;
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Start" }, { label: "Wait" }], question: "Start the work?" },
    session
  );
  const row = replyRowId(await seedCommentThread(issue.key, "Is the plan read?", "Not yet."));
  const dismissed = replyRowId(
    await seedCommentThread(issue.key, "Who reads the plan?", "Alice does.")
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // The thread the Dismissed view shows at the end goes out of the Inbox first.
    await selectRow(page, dismissed);
    await page.locator(`[data-inbox-dismiss="${dismissed}"]`).first().click();
    await expect(inboxRow(page, dismissed)).toHaveCount(0);
    await selectRow(page, ask.id);

    /** Both marks, the pane's gate line and the ask's selection after each refresh. */
    const reads = async (state: string) => {
      await expect(markOf(page, ask.id), `the ask row at ${state}`).toHaveText(state);
      await expect(markOf(page, row), `the thread row at ${state}`).toHaveText(state);
      await expect(paneLine(page, ""), `the pane's line at ${state}`).toHaveText(state);
      await expect(inboxRow(page, ask.id), `the ask stays selected at ${state}`).toHaveAttribute(
        "data-inbox-selected",
        "true"
      );
    };
    await reads("Not requested");

    // An approval request opened, then answered with an approval.
    await requestApproval(spec, {}, session);
    await reads("Awaiting approval");
    await reviewArtifact(spec, { state: "approved" }, { login: "alice" });
    await reads("Approved v1");
    // A new version written.
    await editArtifact(
      spec,
      { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
      session
    );
    await reads("Approved v1 · changed since");
    // A request for changes.
    await reviewArtifact(
      spec,
      { reason: "Name the rollback path.", state: "changes_requested" },
      { login: "alice" }
    );
    await reads("Changes requested");
    // A second request, resolved with no review.
    const second = withOpenedAsk(await requestApproval(spec, {}, session));
    await reads("Awaiting approval");
    await resolveAsk(second.ask.id, { kind: "retracted", reason: "Asked too early." }, session);
    await reads("Changes requested");

    // The approval again, with the Dismissed view open on the dismissed thread of the issue.
    await page.goto("/?show=dismissed");
    await expect(markOf(page, dismissed)).toHaveText("Changes requested");
    const latest = (await getArtifact(spec, { login: "alice" })).approval?.latest_version;
    await reviewArtifact(spec, { state: "approved" }, { login: "alice" });
    await expect(markOf(page, dismissed)).toHaveText(`Approved v${latest}`);
  } finally {
    await alice.close();
  }
});
