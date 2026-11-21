import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  createIssue,
  createProject,
  editArtifact,
  getAsk,
  listIssueAsks,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { type CliRun, cliSession, runDispatch } from "./dispatch-cli";
import { inboxRow, READING_PANE_WIDTH } from "./inbox-pane";
import { expect, test } from "./test";

/**
 * The form of The Spec, end to end: Dispatch refuses an approval request, and a person's approval,
 * when a text of a document under the root of The Spec breaks the form, and the refusal names the
 * node and the rule. The real `dispatch` command (`e2e/dispatch-cli.ts`) and a real browser drive
 * the real server, whose root is `STE-1`: `e2e/run-server.sh` names it in `DISPATCH_SPEC_ROOTS`
 * unless the caller's `DISPATCH_E2E_SPEC_ROOTS` names another root.
 *
 * Every row seeds the same tree in the project STE. The root, an issue outside it, and four issues
 * whose spec holds one statement of three sentences. Three of them are under the issue outside the
 * root when the agent asks for their approval, so Dispatch opens those requests, which is the
 * control of the gate's scope. Then two of them move under the root, so the person's approval of
 * them is judged; the third stays outside until the reopen row moves it. The fourth is under the
 * root from the start, and the agent's request of it is refused.
 */

/** The root of The Spec that `e2e/run-server.sh` names for every harness server. */
const ROOT = "STE-1";
/** The node of the one statement every spec of the tree holds. */
const node = "keep-each-message";
/** A statement of three sentences, where the form allows one or two. */
const threeSentences =
  "Dispatch keeps each message. Dispatch indexes each message. Dispatch shows each message.";
/** What every refusal and every listing says of that statement. */
const detail = "it has 3 sentences, and a statement has one or two";
/** The refusal's line for it in spec.md version 1. */
const findingLine = `- statement ${node} in spec.md version 1: ${detail}.`;
/** The control of the form in the other direction: two sentences of 25 words each, which no rule
 *  refuses (`twentyFiveWords`, `internal/dispatch/api/spec_form_routes_test.go`). */
const twentyFiveWords =
  "Dispatch shows the person each question of each issue that the person follows, with the time of the question and the name of its project. " +
  "Dispatch shows the person each approval request that waits on the person, with the document, its version and the short summary that its agent wrote.";
/** The text a person's refused approval sends to the author with Request changes. */
const changesReason = [
  "Dispatch refused my approval, because 1 text breaks the form of The Spec.",
  findingLine,
  "Fix each text, then request approval again.",
].join("\n");
/** A width under the Inbox's reading column, where the Inbox draws an approval as its own card
 *  with the two options and Answer (`e2e/inbox-pane.ts`). */
const CARD_WIDTH = READING_PANE_WIDTH - 180;
/** The agent's own writes, as the session the command runs as. */
const agent = { actor: { id: cliSession, kind: "session" as const }, as: "agent" as const };

function spec(statement: string): string {
  return `# Spec\n\n:::requirement{#${node}}\n${statement}\n:::\n`;
}

/** What a run printed, both streams: a refusal goes to stderr and an answer to stdout. */
function printed(run: CliRun): string {
  return `${run.stdout}${run.stderr}`;
}

/** The ids of the open approval requests of an issue. */
async function openRequests(issue: string): Promise<string[]> {
  return (await listIssueAsks(issue))
    .filter((ask) => ask.kind === "approval" && ask.state === "open")
    .map((ask) => ask.id);
}

/** The one open approval request of an issue, which the seed opened. */
async function theOpenRequest(issue: string): Promise<string> {
  const open = await openRequests(issue);
  expect(open, `the open approval requests of ${issue}`).toHaveLength(1);
  return open[0] ?? "";
}

/** The command's own home and state directory for one row, removed when the row ends. */
async function withHome(row: (home: string) => Promise<void>): Promise<void> {
  const home = mkdtempSync(join(tmpdir(), "dispatch-spec-form-"));
  try {
    await row(home);
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
}

async function seedTree(home: string) {
  await createProject({ key: "STE", name: "The Spec" });
  const root = await createIssue({ project: "STE", title: "The Spec" });
  // e2e/run-server.sh names this key, so the seed must make it.
  expect(root.key).toBe(ROOT);
  const outside = await createIssue({ project: "STE", title: "Work outside The Spec" });
  // The four specs hold the same statement on purpose, so creation is told they are no
  // duplicates (`force`), which it would otherwise refuse as near copies.
  const broken = (parent: string, title: string) =>
    createIssue({ force: true, parent, project: "STE", spec: spec(threeSentences), title });
  const header = await broken(outside.key, "Approved from the header");
  const card = await broken(outside.key, "Approved from the Inbox card");
  const reopened = await broken(outside.key, "Closed and reopened");
  const requested = await broken(root.key, "Requested by the agent");
  // Outside the root no text is judged: the agent's request of each opens.
  for (const issue of [header, card, reopened]) {
    const request = ["request-approval", "--issue", issue.key, "--summary", "The statement."];
    const opened = await runDispatch(request, home);
    expect(opened.code, printed(opened)).toBe(0);
  }
  const asks = {
    card: await theOpenRequest(card.key),
    header: await theOpenRequest(header.key),
    reopened: await theOpenRequest(reopened.key),
  };
  await patchIssue(header.key, { parent: root.key });
  await patchIssue(card.key, { parent: root.key });
  return { asks, card, header, outside: outside.key, reopened, requested };
}

/** An issue whose spec holds `count` statements of three sentences, each a finding, and whose
 *  request the agent opened outside the root before the issue moved under it, so the person's
 *  approval of it is judged. */
async function brokenRequestUnderRoot(outside: string, count: number): Promise<string> {
  const blocks = Array.from(
    { length: count },
    (_, index) => `:::requirement{#s${index + 1}}\n${threeSentences}\n:::\n`
  );
  const issue = await createIssue({
    force: true,
    parent: outside,
    project: "STE",
    spec: `# Spec\n\n${blocks.join("\n")}`,
    title: `${count} broken statements`,
  });
  const opened = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary: "The statements." }, agent)
  );
  await patchIssue(issue.key, { parent: ROOT });
  return opened.ask.id;
}

/** The height of the reason field and of the text it holds, and the height of the window. */
function reasonFit(field: HTMLTextAreaElement) {
  return {
    held: field.scrollHeight,
    shown: field.clientHeight,
    tall: field.getBoundingClientRect().height,
    window: window.innerHeight,
  };
}

// SLOW(>=2s): eight cold `bun` starts of the real `dispatch` command around one Inbox page load.
test("an agent's request of a broken statement under the root is refused with the finding, spec-form lists it, and the request of the fixed statement reaches the person", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    const request = [
      "request-approval",
      "--issue",
      tree.requested.key,
      "--summary",
      "The statement.",
    ];

    // (a) to (b2) each read their own output, so each is its own reported check
    // (`expect.soft`): a run where one of them is wrong says which.
    // (a) The request is refused, with the code, the finding and the next step, and opens nothing.
    const refused = await runDispatch(request, home);
    expect.soft(refused.code, printed(refused)).toBe(1);
    expect
      .soft(printed(refused))
      .toContain(
        "SPEC_FORM: Dispatch refused this approval request, because 1 text breaks the form of The Spec."
      );
    expect.soft(printed(refused)).toContain(findingLine);
    expect.soft(printed(refused)).toContain("Fix each text, then request approval again.");
    expect.soft(await openRequests(tree.requested.key)).toEqual([]);

    // (b) spec-form shows the same finding before a request.
    const form = await runDispatch(["spec-form", "--issue", tree.requested.key], home);
    expect.soft(form.code, printed(form)).toBe(0);
    expect
      .soft(form.stdout)
      .toContain(
        [
          "spec.md version 1:",
          "Dispatch refuses an approval request of spec.md version 1, because 1 text breaks the form of The Spec.",
          `- statement ${node}: ${detail}.`,
        ].join("\n")
      );

    // (b2) The walk lists each issue under the root with its spec, and the issue outside it not.
    const walk = await runDispatch(["spec-form", "--root", ROOT], home);
    expect.soft(walk.code, printed(walk)).toBe(0);
    const counts = "1 statement, 0 claims, 0 purposes and 0 definitions, with 1 finding.";
    for (const issue of [tree.header, tree.card, tree.requested]) {
      expect
        .soft(walk.stdout)
        .toContain(
          `${issue.key} [${issue.status}] ${issue.title}: ${counts}\n  spec.md version 1 (scope covered): ${counts}`
        );
    }
    expect.soft(walk.stdout).not.toContain(tree.reopened.key);
    expect
      .soft(walk.stdout.trimEnd().split("\n").at(-1))
      .toBe(`Dispatch read 3 issues and 3 documents under ${ROOT}.`);

    // (d) The agent fixes the statement, and the request opens and says what Dispatch read.
    await editArtifact(
      tree.requested.primary_artifact_id,
      { ops: [{ op: "replace", find: threeSentences, with: twentyFiveWords }] },
      agent
    );
    const fixed = await runDispatch(request, home);
    expect(fixed.code, printed(fixed)).toBe(0);
    expect(fixed.stdout).toContain(
      "Dispatch read the form of 1 statement, 0 claims, 0 purposes and 0 definitions against 0 terms: no text breaks it."
    );
    const [opened] = await openRequests(tree.requested.key);
    expect(opened, "the request the fixed statement opened").toBeDefined();
    await page.goto("/");
    await expect(inboxRow(page, opened ?? "")).toBeVisible();
  });
});

// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command around a spec page and a review written in it.
test("the person's Approve on the header of a broken statement under the root shows Dispatch's refusal, and Request changes sends the findings to the author", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    await page.goto(`/issues/${tree.header.key}`);
    const approve = page.getByRole("button", { exact: true, name: "Approve" });
    await approve.click();
    // The header's last line names Request changes. The Needs you card of the same request shows
    // the refusal too, and its last line names Answer.
    const refusal = page
      .getByRole("alert")
      .filter({ hasText: "To send these findings to the author, choose Request changes." });
    await expect(refusal).toContainText(findingLine);
    await expect(refusal).toContainText(
      "To send these findings to the author, choose Request changes."
    );
    await expect(page.getByRole("button", { name: /^Approved/ })).toHaveCount(0);
    // The refusal is an answer, which the header shows: the press keeps the focus, and the
    // shell's corner lists no Retry that Dispatch can only refuse again.
    await expect(approve).toBeFocused();
    await expect(page.getByTestId("pending-writes")).toHaveCount(0);

    await page.getByRole("button", { exact: true, name: "Request changes" }).click();
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    const reason = dialog.getByLabel("Reason");
    await expect(reason).toHaveValue(changesReason);
    // The field shows the whole reason it opens with, so the person reads what Request changes
    // sends to the author without scrolling inside a two-line box.
    await expect
      .poll(() => reason.evaluate((field) => field.scrollHeight <= field.clientHeight))
      .toBe(true);
    await expect(dialog.getByRole("alert")).toHaveCount(0);
    await dialog.getByRole("button", { exact: true, name: "Request changes" }).click();
    await expect(page.getByRole("button", { name: "Changes requested" })).toBeVisible();

    const ref = `dispatch://${tree.header.key}/ask/${tree.asks.header}`;
    const read = await runDispatch(["read", "--ref", ref], home);
    expect(read.code, printed(read)).toBe(0);
    expect(read.stdout).toContain("State: answered");
    expect(read.stdout).toContain("- Selected: Request changes");
    expect(read.stdout).toContain(`- Text: ${changesReason}`);
  });
});

// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command around the Inbox and an answer written in it.
test("the person's Approve on the Inbox card of a broken statement under the root shows Dispatch's refusal and picks Request changes with the findings", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    await page.setViewportSize({ height: 900, width: CARD_WIDTH });
    await page.goto("/");
    const card = page.getByTestId(`ask-${tree.asks.card}`);
    // Each option's name is its label and its description, and Request changes' description
    // holds the word "approved", so each is named by the start of its label.
    await card.getByRole("radio", { name: /^Approve / }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect(card.getByRole("alert")).toContainText(
      "Dispatch refused this approval, because 1 text breaks the form of The Spec."
    );
    await expect(card.getByRole("alert")).toContainText(findingLine);
    // The card has picked Request changes itself, so its last line names the press left, and
    // that press keeps the focus.
    await expect(card.getByRole("alert")).toContainText(
      "To send these findings to the author, choose Answer."
    );
    await expect(card.getByRole("alert")).not.toContainText("choose Request changes");
    await expect(card.getByRole("button", { name: "Answer" })).toBeFocused();
    await expect(card.getByRole("radio", { name: /^Request changes / })).toBeChecked();
    await expect(card.getByLabel("Reason (required)")).toHaveValue(changesReason);

    await card.getByRole("button", { name: "Answer" }).click();
    await expect.poll(async () => (await getAsk(tree.asks.card)).ask.state).toBe("answered");
    const ref = `dispatch://${tree.card.key}/ask/${tree.asks.card}`;
    const read = await runDispatch(["read", "--ref", ref], home);
    expect(read.code, printed(read)).toBe(0);
    expect(read.stdout).toContain("- Selected: Request changes");
    expect(read.stdout).toContain(`- Text: ${changesReason}`);
  });
});

// SLOW(>=2s): six cold `bun` starts of the real `dispatch` command, a close and a reopen, then the Inbox.
test("a reopen leaves a broken request under the root resolved with its finding, and the person's Inbox shows no card for it", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    const key = tree.reopened.key;
    await patchIssue(key, { parent: ROOT });
    const close = ["issue-update", "--issue", key, "--status", "done"];
    const closed = await runDispatch([...close, "--reason", "Close to try the reopen."], home);
    expect(closed.code, printed(closed)).toBe(0);
    const reopen = await runDispatch(["issue-update", "--issue", key, "--status", "todo"], home);
    expect(reopen.code, printed(reopen)).toBe(0);

    const read = await runDispatch(
      ["read", "--ref", `dispatch://${key}/ask/${tree.asks.reopened}`],
      home
    );
    expect(read.code, printed(read)).toBe(0);
    expect(read.stdout).toContain("State: resolved");
    expect(read.stdout).toContain(
      [
        "- Reason: Dispatch did not open this approval request again, because 1 text breaks the form of The Spec.",
        findingLine,
        "Fix each text, then request approval again.",
      ].join("\n")
    );

    // The Inbox lists the card's request, which is still open, so its list is drawn; the
    // reopened request is not in it.
    await page.goto("/");
    await expect(inboxRow(page, tree.asks.card)).toBeVisible();
    await expect(inboxRow(page, tree.asks.reopened)).toHaveCount(0);
  });
});

// The issue page's Needs you card is a second place where the person presses Approve. The
// answer takes the request's row out of the Inbox read while it is out, and the refusal puts it
// back, which draws the card again: the refusal, the pick and the focus survive that redraw, and
// one more press sends the findings to the author.
// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command around the issue page and an answer written in it.
test("the person's Approve on the Needs you card of the issue page shows Dispatch's refusal and picks Request changes with the findings", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${tree.card.key}`);
    const card = page
      .locator(`[data-margin-item="${tree.asks.card}"]`)
      .getByTestId(`ask-${tree.asks.card}`);
    await card.getByRole("radio", { name: /^Approve / }).check();
    // The compact card's disclosure, "Add a note or answer in your own words", also holds the word.
    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    await answer.click();
    await expect(card.getByRole("alert")).toContainText(
      "Dispatch refused this approval, because 1 text breaks the form of The Spec."
    );
    await expect(card.getByRole("alert")).toContainText(findingLine);
    await expect(card.getByRole("alert")).toContainText(
      "To send these findings to the author, choose Answer."
    );
    await expect(card.getByRole("radio", { name: /^Request changes / })).toBeChecked();
    await expect(card.getByLabel("Reason (required)")).toHaveValue(changesReason);
    await expect(answer).toBeFocused();

    await answer.click();
    await expect.poll(async () => (await getAsk(tree.asks.card)).ask.state).toBe("answered");
    const ref = `dispatch://${tree.card.key}/ask/${tree.asks.card}`;
    const read = await runDispatch(["read", "--ref", ref], home);
    expect(read.code, printed(read)).toBe(0);
    expect(read.stdout).toContain("- Selected: Request changes");
    expect(read.stdout).toContain(`- Text: ${changesReason}`);
  });
});

// The issue page's Conversation tab shows the request in a card of its own, beside the Needs you
// card of the margin. The refusal belongs to the request, so both cards show it and pick Request
// changes. The focus stays on Answer in the card that the person pressed: the margin's card takes
// no focus, and the page does not move to it.
// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command around the issue page and an answer written in it.
test("the person's refused Approve on the Conversation tab's card keeps the focus in that card, and the Needs you card beside it shows the refusal too", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${tree.card.key}`);
    await page.getByRole("tab", { name: "Conversation" }).first().click();
    const card = page.locator(`[data-turn] [data-testid="ask-${tree.asks.card}"]`);
    const margin = page
      .locator(`[data-margin-item="${tree.asks.card}"]`)
      .getByTestId(`ask-${tree.asks.card}`);
    await card.getByRole("radio", { name: /^Approve / }).check();
    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    await answer.click();

    for (const each of [card, margin]) {
      await expect(each.getByRole("alert")).toContainText(
        "Dispatch refused this approval, because 1 text breaks the form of The Spec."
      );
      await expect(each.getByRole("radio", { name: /^Request changes / })).toBeChecked();
      await expect(each.getByLabel("Reason (required)")).toHaveValue(changesReason);
    }
    await expect(answer).toBeFocused();
    // An absence window: the margin's card, drawn with the refusal, must not take the focus after.
    await page.waitForTimeout(1000);
    await expect(answer).toBeFocused();
    expect(await margin.evaluate((element) => element.contains(document.activeElement))).toBe(
      false
    );
  });
});

// One request has one refusal on the page. A refused Approve in the issue page's header shows in
// the Needs you card of the same request, which picks Request changes with the reason, and a
// refused Approve in that card shows in the header. The focus stays where the person pressed.
// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command around two issue pages and two answers written in them.
test("the person's refused Approve in the issue page's header shows in the Needs you card of the same request, and a refusal in that card shows in the header", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    await page.setViewportSize({ height: 900, width: 1440 });
    const headerRefusal = page
      .getByRole("alert")
      .filter({ hasText: "To send these findings to the author, choose Request changes." });
    const approve = page.getByRole("button", { exact: true, name: "Approve" });

    await page.goto(`/issues/${tree.header.key}`);
    const headerCard = page
      .locator(`[data-margin-item="${tree.asks.header}"]`)
      .getByTestId(`ask-${tree.asks.header}`);
    await approve.click();
    await expect(headerRefusal).toContainText(findingLine);
    await expect(headerCard.getByRole("alert")).toContainText(
      "To send these findings to the author, choose Answer."
    );
    await expect(headerCard.getByRole("radio", { name: /^Request changes / })).toBeChecked();
    await expect(headerCard.getByLabel("Reason (required)")).toHaveValue(changesReason);
    await expect(approve).toBeFocused();

    await page.goto(`/issues/${tree.card.key}`);
    const card = page
      .locator(`[data-margin-item="${tree.asks.card}"]`)
      .getByTestId(`ask-${tree.asks.card}`);
    await card.getByRole("radio", { name: /^Approve / }).check();
    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    await answer.click();
    await expect(card.getByRole("alert")).toContainText(findingLine);
    await expect(headerRefusal).toContainText(findingLine);
    await expect(answer).toBeFocused();
  });
});

// Nothing that the person typed is lost: after a refused Approve the reason field holds Dispatch's
// reason, then one empty line, then the person's note as it was. The draft belongs to the request,
// so the Needs you card beside the pressed Conversation card shows the same text.
// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command around the issue page and an answer written in it.
test("the person's note survives a refused Approve: the field holds Dispatch's reason, an empty line and the note, in each card of the request", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${tree.card.key}`);
    await page.getByRole("tab", { name: "Conversation" }).first().click();
    const card = page.locator(`[data-turn] [data-testid="ask-${tree.asks.card}"]`);
    const margin = page
      .locator(`[data-margin-item="${tree.asks.card}"]`)
      .getByTestId(`ask-${tree.asks.card}`);
    const note = "I read each statement, and I approve it.";
    await card.getByRole("radio", { name: /^Approve / }).check();
    await card.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    await card.getByLabel("Reason", { exact: true }).fill(note);
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    for (const each of [card, margin]) {
      await expect(each.getByRole("alert")).toContainText("Dispatch refused this approval");
      await expect(each.getByLabel("Reason (required)")).toHaveValue(`${changesReason}\n\n${note}`);
    }
  });
});

// The reason field of the Inbox card grows with the reason Dispatch wrote, up to half the height
// of the window, and scrolls past that: a reason of three findings that is shorter than half the
// window shows whole on a phone, and one of twenty-five stops at half the window, so Answer stays
// on the screen. The refusal above the field lists each finding.
// SLOW(>=2s): three cold `bun` starts of the real `dispatch` command around the Inbox and two refused answers.
test("the Inbox card's reason field at 390 px shows a refusal of three findings whole, and stops a refusal of twenty-five at half the window", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await withHome(async (home) => {
    const tree = await seedTree(home);
    const three = await brokenRequestUnderRoot(tree.outside, 3);
    const many = await brokenRequestUnderRoot(tree.outside, 25);
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto("/");
    for (const [ask, whole] of [
      [three, true],
      [many, false],
    ] as const) {
      const card = page.getByTestId(`ask-${ask}`);
      await card.getByRole("radio", { name: /^Approve / }).check();
      await card.getByRole("button", { name: "Answer" }).click();
      await expect(card.getByRole("alert")).toContainText("Dispatch refused this approval");
      const field = card.getByLabel("Reason (required)");
      await expect(field).toHaveValue(/^Dispatch refused my approval/);
      // Each poll answers what it measured, so a failure names the heights it read.
      const measured = async () => {
        // expect.poll calls this again until the shape it wants holds, so one reading is safe here.
        const fit = await field.evaluate(reasonFit);
        const shape =
          fit.held <= fit.shown
            ? "whole"
            : Math.abs(fit.tall - fit.window / 2) <= 1
              ? "stops at half the window"
              : "scrolls under half the window";
        return `${shape}: ${fit.held} px of text, a field of ${fit.shown} px, a window of ${fit.window} px`;
      };
      await expect.poll(measured).toMatch(whole ? /^whole:/ : /^stops at half the window:/);
    }
  });
});
