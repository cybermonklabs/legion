import type { Actor } from "../web/src/api/types";
import {
  createAsk,
  createIssue,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  getIssue,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { connectedDot, countDocumentSockets, documentEditor, reconnectedSockets } from "./editor";
import { inboxRow } from "./inbox-pane";
import { type Browser, expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// Closing an issue withdraws each of its open asks, naming the closer, and reopening it brings
// back exactly those asks. Every row seeds an issue holding a plain ask, a decision block in its
// spec and an approval request on the spec, opens the issue page and the Inbox, and only then
// closes it, so what is checked is what a reader already looking at the issue sees.

/** The agent that asks: every seeded ask waits on a person, so each is in alice's Inbox. */
const asker = { actor: { id: "e2e-seed", kind: "session" } as Actor, as: "agent" as const };

/** Every fact this file polls for lands in milliseconds: Playwright's default 100/250/500/1000
 *  ladder would first see it a quarter-second late. */
const PROBE = { intervals: [50, 50, 100, 200, 500] };

const decisionBlock =
  ':::ask{#decision urgency="high" multiple="false" state="open"}\nShip now?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n';

/** The decision block as the text routes serve it, and as a stored version of this build holds it
 *  while its ask is open: with none of the server-owned attributes, an ask's state living on the
 *  ask. */
const storedDecision = ':::ask{#decision urgency="high" multiple="false"}\n';
/** The decision's opener in a stored version once the close resolved it: a version keeps the state
 *  of an ask that is not open on its opener, for the release before's readers. */
const storedResolvedDecision =
  ':::ask{#decision urgency="high" multiple="false" state="resolved"}\n';

/** The decision block's state as the text routes answer it, in their `asks` member, read from
 *  the ask: their markdown holds no ask's state. */
async function decisionState(artifactId: string): Promise<string | undefined> {
  const text = await getArtifactText(artifactId);
  expect(text.markdown).toContain(storedDecision);
  return text.asks?.find((entry) => entry.block_id === "decision")?.state;
}

/** The number of the newest version the document has saved. */
async function newestVersion(artifactId: string): Promise<number> {
  const { versions } = await getArtifact(artifactId, { login: "alice" });
  return Math.max(...(versions ?? []).map((version) => version.number));
}

interface Seeded {
  /** Every open ask of the issue: the plain ask, the decision block's and the approval request. */
  askIds: string[];
  artifactId: string;
  key: string;
}

async function seedIssue(project: string): Promise<Seeded> {
  await createProject({ key: project, name: project });
  const issue = await createIssue({
    project,
    spec: "## Plan\n\nContext.\n",
    title: "Close withdraws asks",
  });
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: decisionBlock, op: "insert" }] },
    asker
  );
  // The block is an ask once settlement has indexed it.
  await expect
    .poll(
      async () =>
        ((await getIssue(issue.key)).open_asks ?? []).some((ask) => ask.block_id === "decision"),
      PROBE
    )
    .toBe(true);
  const block = ((await getIssue(issue.key)).open_asks ?? []).find(
    (ask) => ask.block_id === "decision"
  );
  if (block === undefined) {
    throw new Error("the decision block was not indexed");
  }
  // Neither write reads the other, and the ids they return are the only order the row keeps.
  const [plain, requested] = await Promise.all([
    createAsk(issue.key, { question: "Which region first?" }, asker),
    requestApproval(issue.primary_artifact_id, {}, asker),
  ]);
  const approval = withOpenedAsk(requested);
  return {
    askIds: [plain.id, block.id, approval.ask.id],
    artifactId: issue.primary_artifact_id,
    key: issue.key,
  };
}

interface Opened {
  decision: Locator;
  inbox: Page;
  page: Page;
  /** The document sockets the issue page has opened for the issue's spec, open or closed. */
  sockets: () => number;
}

/** Opens alice's Inbox and the issue page, and waits until both show every ask open. The Inbox
 *  is read as the list it is: from 1280 px each question is a short row and only the selected
 *  one is a card, so the rows are what says the issue's three asks are listed. */
async function openIssueAndInbox(
  browser: Browser,
  seeded: Seeded,
  { clock = false }: { clock?: boolean } = {}
): Promise<Opened> {
  const alice = await asUser(browser, "alice");
  const inbox = await alice.newPage();
  const page = await alice.newPage();
  const sockets = countDocumentSockets(page, seeded.artifactId);
  // Neither page waits on the other, and each keeps its own checks below.
  if (clock) await page.clock.install();
  await Promise.all([inbox.goto("/"), page.goto(`/issues/${seeded.key}`)]);
  for (const id of seeded.askIds) {
    await expect(inboxRow(inbox, id)).toBeVisible();
  }
  const decision = documentEditor(page).locator('[data-dispatch-ask-block="decision"]');
  await expectOpenDecision(decision);
  await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (3)");
  await expect(page.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
  return { decision, inbox, page, sockets };
}

/** The block hosts the open, answerable card. */
async function expectOpenDecision(decision: Locator): Promise<void> {
  await expect(decision.getByRole("radio", { name: "Ship Release it" })).toBeVisible();
  await expect(
    decision.getByRole("button", { name: "Add a note or answer in your own words" })
  ).toBeVisible();
  await expect(decision.getByTestId("ask-resolution-badge")).toHaveCount(0);
}

/** What the issue page and the Inbox show once the close has withdrawn the asks. */
async function expectWithdrawn(
  { decision, inbox, page }: Opened,
  seeded: Seeded,
  closer: string
): Promise<void> {
  await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
  await expect(page.getByTestId("issue-whose-turn")).toHaveCount(0);
  await expect(decision.getByTestId("ask-resolution-badge")).toHaveText("Withdrawn");
  await expect(decision).toContainText(`Withdrawn when ${closer} closed the issue`);
  // The server's reason repeats the sentence, so the card does not show it.
  await expect(decision).not.toContainText("Withdrawn by the close of");
  await expect(decision.getByRole("radio")).toHaveCount(0);
  await expect(
    decision.getByRole("button", { name: "Add a note or answer in your own words" })
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Awaiting approval" })).toHaveCount(0);
  // The close takes every one of the issue's questions out of the Inbox. From 1280 px the
  // reading column is showing one of them, and a row the reader is reading is held where they
  // last saw it rather than taken from under them (INFRA-575, `heldRow`), so it is on the
  // reader's next look at the list that the Inbox has none of them left.
  await inbox.reload();
  await expect(inbox.getByText("Nothing needs you")).toBeVisible();
  for (const id of seeded.askIds) {
    await expect(inboxRow(inbox, id)).toHaveCount(0);
  }
}

// SLOW(>=2s): two pages of the issue, one of them the document editor with its own socket, then a
// close, the version history, the closed issue's settlement window, a reopen and a reload.
// @pr: Create through answer to close: the Close withdraws the issue's asks and writes one version showing them resolved, and Reopen brings them back in one more.
test("a person's Close withdraws the issue's asks and writes one version showing them resolved, and Reopen brings the asks back in one more @pr", async ({
  browser,
}) => {
  const seeded = await seedIssue("CLOSE");
  const opened = await openIssueAndInbox(browser, seeded, { clock: true });
  const { decision, inbox, page, sockets } = opened;
  try {
    // The close writes the decision's resolved state into its block, as a version keeps it for
    // the release before's readers, and the settlement it leaves owed saves that as one version:
    // the newest version saved before the close is the one it starts from.
    const before = await newestVersion(seeded.artifactId);
    await page.getByRole("button", { name: "Close issue" }).click();
    await expectWithdrawn(opened, seeded, "alice");

    // That version's stored text holds the decision as it was written, with none of the
    // server-owned attributes: a version keeps no state of an open ask, and no answer's words.
    const savedMarkdown = (await getArtifactVersion(seeded.artifactId, before)).markdown;
    expect(savedMarkdown).toContain(storedDecision);
    expect(savedMarkdown).not.toContain("state=");
    // Opened from the history, it shows that text read-only: the question, its options and who
    // asked, with nothing to answer. Its card reads the decision's state from the server's read of
    // the ask, which the close resolved, and draws no record of the withdrawal: only the closed
    // issue's own document shows who withdrew it.
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(before));
    const saved = page
      .getByRole("region", { name: `Document version ${before}` })
      .locator('[data-dispatch-ask-block="decision"]');
    await expect(saved).toContainText("Ship now?");
    await expect(saved.locator('ul[aria-label="Options"] li')).toHaveCount(2);
    await expect(saved.locator("[data-dispatch-ask-asked]")).toBeVisible();
    await expect(saved.getByRole("radio")).toHaveCount(0);
    await expect(saved.getByTestId("ask-resolution-badge")).toHaveCount(0);
    await expect(saved).not.toContainText("Withdrawn");
    await expect(saved.getByText("Resolved", { exact: true })).toBeVisible();
    await page.getByRole("combobox", { name: "Version" }).selectOption("");

    // The close dropped the document's socket, and its reconnect is admitted read-only while the
    // issue is closed: the page a person who reopens after the draw holds, which the Reopen below
    // must make writable. The document socket's first reconnect is a uniform draw from [0, 2 s)
    // (#2323) the page's own clock spends, so a row waits for the state the page settles in and
    // never depends on which side of the draw a run lands. A reload cannot make a closed issue's
    // document editable, so the page names none.
    await reconnectedSockets(page, sockets, 2);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "false");
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);
    // The close's write left the document owed a settlement, which runs on the closed issue
    // 250 ms after it is armed (`DISPATCH_TEST_SETTLE_DELAY`, `e2e/run-server.sh`) and saves one
    // version, the decision's opener showing it resolved, as its ask's node holds it; the text
    // routes answer the same state from the ask in their asks, so an agent reading the document
    // reads the decision resolved.
    await expect.poll(() => decisionState(seeded.artifactId), PROBE).toBe("resolved");
    await expect.poll(() => newestVersion(seeded.artifactId), PROBE).toBe(before + 1);
    expect((await getArtifactVersion(seeded.artifactId, before + 1)).markdown).toContain(
      storedResolvedDecision
    );
    await page.getByRole("button", { name: "Reopen" }).click();
    await expect(page.getByRole("button", { name: "Close issue" })).toBeVisible();
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (3)");
    // The reopen brought the ask back on the server, which the card reads, so the record is gone
    // at once, and it dropped the document's socket again, so its reconnect, a draw of its own, is
    // admitted writable: the row waits for the page that settles, editable, with the block
    // hosting the open, answerable card, and no reload.
    await expect(decision.getByTestId("ask-resolution-badge")).toHaveCount(0);
    await expect(decision).not.toContainText("Withdrawn");
    await reconnectedSockets(page, sockets, 3);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "true");
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);
    await expectOpenDecision(decision);
    await expect(decision.getByRole("radio", { name: "Hold Wait for review" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
    for (const id of seeded.askIds) {
      await expect(inboxRow(inbox, id)).toBeVisible();
    }
    // Reloaded, the block still hosts the open, answerable card. The reload opens a document
    // socket of its own, whose draw the page's clock spends as it did the two before it.
    await page.reload();
    await reconnectedSockets(page, sockets, 4);
    await expect(connectedDot(page)).toHaveText("connected");
    await expectOpenDecision(decision);
    // The reopen wrote the decision open again, which takes its state off, and the settlement it
    // left owed saved that as one more version: the text routes read the decision open again
    // from its ask.
    expect(await decisionState(seeded.artifactId)).toBe("open");
    await expect.poll(() => newestVersion(seeded.artifactId), PROBE).toBe(before + 2);
    expect((await getArtifactVersion(seeded.artifactId, before + 2)).markdown).toContain(
      storedDecision
    );
  } finally {
    await page.context().close();
  }
});

// SLOW(>=2s): the six-call seed and two pages, one of them the document editor with its own
// socket, before the close is made.
test("an agent session's close names the session on the withdrawn decision", async ({
  browser,
}) => {
  const seeded = await seedIssue("AGENT");
  const opened = await openIssueAndInbox(browser, seeded);
  try {
    await patchIssue(
      seeded.key,
      { status: "done" },
      { actor: { id: "closer-session", kind: "session" }, as: "agent" }
    );
    await expectWithdrawn(opened, seeded, "session:closer-s…");
  } finally {
    await opened.page.context().close();
  }
});

// SLOW(>=2s): the six-call seed and two pages, one of them the document editor with its own
// socket, before the close is made.
test("the Legion daemon's close names the daemon whole on the withdrawn decision", async ({
  browser,
}) => {
  const seeded = await seedIssue("DAEMON");
  const opened = await openIssueAndInbox(browser, seeded);
  try {
    // The request the daemon's `SetStatus` sends: its bearer and its own session identity.
    await patchIssue(
      seeded.key,
      { status: "done" },
      { actor: { id: "legion-daemon:E2E", kind: "session" }, as: "agent" }
    );
    await expectWithdrawn(opened, seeded, "legion-daemon:E2E");
  } finally {
    await opened.page.context().close();
  }
});

test("a closed issue's document names no unsent edit to a person who typed nothing", async ({
  browser,
}) => {
  // A closed issue whose spec an agent wrote, opened by a person who types nothing: the page must
  // not tell them an edit saved in their browser cannot be sent.
  await createProject({ key: "READ", name: "READ" });
  const issue = await createIssue(
    { project: "READ", spec: "## Plan\n\nContext.\n", title: "Closed" },
    asker
  );
  await patchIssue(issue.key, { status: "done" }, asker);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The second load reads what the first left in this browser's store of unsent edits.
    for (const load of ["first", "reload"]) {
      if (load === "first") {
        await page.goto(`/issues/${issue.key}/spec`);
      } else {
        await page.reload();
      }
      await expect(documentEditor(page)).toContainText("Context.");
      await expect(
        page.getByText("This issue is closed. Its document is read-only.")
      ).toBeVisible();
      await expect(connectedDot(page)).toHaveAccessibleName("connected");
    }
  } finally {
    await alice.close();
  }
});
