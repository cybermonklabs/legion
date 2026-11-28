/** The shared search, sort and filter controls where proving them needs a server: seeded counts,
 *  writes, reloads, the keyboard, and the page a recording cannot answer - the Agents page, whose
 *  groups are a session's last-seen time against the clock. The rows are those of the pages that
 *  draw the shared controls: the Agents page, an issue's Artifacts tab and the search bar.
 */

import type { Locator, Route } from "@playwright/test";
import { CONTROL } from "../web/src/components/control-names";
import {
  agentFilter,
  agentGroupToggle,
  agentRow,
  type FakeSession,
  getSentMessages,
  issueSearchIn,
  pickAgentFilter,
  setLiveSessions,
  shownAgentRows,
  toggleAgentGroup,
} from "./agents";
import {
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  listIssueArtifacts,
  patchIssue,
  putAgentState,
  replyToMessageDelivery,
} from "./api";
import { documentEditor, placeCaret } from "./editor";
import { control, controlRow, openFilterPanel } from "./filters";
import { focusHolder } from "./focus";
import { nextFrames, settledScrollTop } from "./frames";
import { expandedState } from "./settled";
import { closeControlPanel, isPhoneProject } from "./table-controls";
import { expect, type Page, test } from "./test";
import { assertPageFits } from "./unclipped";

/** The Agents page's keep row's sessions, by id: UUIDs, which Dispatch's agent routes require. */
const KEPT_AGENT = {
  auditor: "01a0e812-3135-7000-8000-000000000003",
  planner: "01a0e812-3135-7000-8000-000000000001",
  reviewer: "01a0e812-3135-7000-8000-000000000002",
  sleeper: "01a0e812-3135-7000-8000-000000000005",
  tester: "01a0e812-3135-7000-8000-000000000004",
} as const;

/** The Agents page's four groups, in the order it draws them. */
const AGENT_GROUP_ORDER = ["needs-you", "active", "idle", "quiet"] as const;

/** A live session of that row. `last_seen` is taken when the row seeds, never at this module's
 *  scope, which a worker evaluates once and every later row of that worker ages. */
function keptAgent(
  sessionID: string,
  title: string,
  machine: string,
  role: string,
  silentFor = 5_000
): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: `/srv/${title.toLowerCase()}`,
    last_seen: Date.now() - silentFor,
    machine_id: machine,
    roles: [role],
    session_id: sessionID,
    title,
  };
}

/**
 * Five sessions over two machines and three roles, one in each group but Idle, which holds two
 * so a pin has an order to change: the Planner needs the reader (an open ask of its own waits on
 * them), has a question whose next move is its own and a reply the reader has not read; the
 * Reviewer wrote a comment a moment ago; the Auditor and the Tester have done nothing in
 * Dispatch; the Sleeper has not been heard from for an hour. Returns the issue the Planner's asks
 * name, which the search box finds it by.
 */
async function seedKeptAgents(): Promise<string> {
  await setLiveSessions([
    keptAgent(KEPT_AGENT.planner, "Planner", "box-1", "planner"),
    keptAgent(KEPT_AGENT.reviewer, "Reviewer", "box-1", "reviewer"),
    keptAgent(KEPT_AGENT.auditor, "Auditor", "box-2", "tester"),
    keptAgent(KEPT_AGENT.tester, "Tester", "box-2", "tester"),
    keptAgent(KEPT_AGENT.sleeper, "Sleeper", "box-2", "tester", 60 * 60_000),
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agents kept" });
  const asPlanner = {
    actor: { id: KEPT_AGENT.planner, kind: "session" as const },
    as: "agent" as const,
  };
  await createAsk(issue.key, { question: "Which release?" }, asPlanner);
  // The reader has answered the second, so its next move is the Planner's.
  const answered = await createAsk(issue.key, { question: "Which branch?" }, asPlanner);
  await createComment(issue.key, { ask_id: answered.id, body: "Which one do you mean?" });
  await createComment(
    issue.key,
    { body: "Reviewing the diff." },
    { actor: { id: KEPT_AGENT.reviewer, kind: "session" }, as: "agent" }
  );
  const asked = await createAgentMessage(KEPT_AGENT.planner, {
    body: "How far along?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "Nearly there." },
    { id: KEPT_AGENT.planner, kind: "session" }
  );
  return issue.key;
}

/** Reads each group heading's name and count, finding each heading by its test id, in
 *  `AGENT_GROUP_ORDER`. */
async function expectAgentGroups(
  page: Page,
  headings: readonly [string, string, string, string],
  what: string
): Promise<void> {
  for (const [index, group] of AGENT_GROUP_ORDER.entries()) {
    await expect(
      agentGroupToggle(page, group),
      `${what}: the ${group} group's heading and count`
    ).toHaveAccessibleName(headings[index] ?? "");
  }
}

// R6 on the Agents page. What the page offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Fields: the group (`features/agents/AgentsPage.tsx:932-949`), the machine (:950-962) and the
//   role (:963-975), each a multi-select, and the search box (:978-990), which matches a
//   session's title, directory, machine, id and the issues its open asks name
//   (`features/agents/agent-search.ts:67-76`). Each is put on and the rows it leaves read, with
//   the group counts under it, and its chip taken off again.
// - Sort: no control. In a group, pinned rows first, then who needs the reader, open asks, newest
//   activity and title (`AgentsPage.tsx:114-144`). Read as the rows' order, and again after a pin.
// - Grouping: Needs you, Active, Idle and Quiet (`features/agents/agent-groups.ts:21-67`,
//   `AgentsPage.tsx:146-173`), each folding (:854-895, :1523-1540). Each group's place is read,
//   and one is folded and opened.
// - Counts: each group heading's `(N)` and `(N, K selected)` (`AgentsPage.tsx:885-887`,
//   `features/agents/selection.ts:61-66`); the select-all header's `N matching` and
//   `K of N matching selected` (`AgentsPage.tsx:1032-1034`, `selection.ts:45-59`); a row's
//   `New reply N` (`AgentsPage.tsx:721-735`), `Needs you N` (:736-744) and `Waiting on agent N`
//   (:745-760); the composer's `Broadcast to N of M selected` (:1136). Each is read.
// - Views: none; the page offers no choice of view.
// - Bulk action: a row's checkbox (`AgentsPage.tsx:631-638`), select-all (:1022-1031), Clear
//   selection (:1035-1043) and the broadcast composer's Send (:1048-1219). Driven to a send, and
//   the fake Envoy read for the one message each selected session was sent.
// - Keys (`features/agents/keyboard.ts:148-244`): `/` (:155-161), `j` (:164-171), `k`
//   (:172-179), Enter (:180-189), `i` (:190-217), Shift+P (:218-224), `x` (:225-232) and Escape
//   (:233-243); and `g a`, the global binding that opens the page (`app.tsx:576`). Each pressed
//   and what it did read.
// - Stored for the reader: the pins (`features/agents/pins.ts:61-125`) and the read mark an
//   opened row writes (`features/agents/unread.ts:128-145`), both the reader's agent state in
//   Dispatch; the page kept nothing in the browser (no `useUserPreference`, no storage). Both are
//   found again after a reload.
// SLOW(>=2s): five seeded sessions worked through every narrowing, count, key, pin and a send.
test("the Agents page keeps every narrowing, grouping, count, key, pin and broadcast it had @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const issue = await seedKeptAgents();
  const main = page.getByTestId("main-content");
  const titles = shownAgentRows(page).locator("h2");
  const planner = agentRow(page, KEPT_AGENT.planner);
  const plannerToggle = planner.getByRole("button", { exact: true, name: "Planner" });
  const reviewer = agentRow(page, KEPT_AGENT.reviewer);
  const tester = agentRow(page, KEPT_AGENT.tester);
  const everyRow = ["Planner", "Reviewer", "Auditor", "Tester", "Sleeper"];
  const opened = ["Needs you (1)", "Active (1)", "Idle (2)", "Quiet (1)"] as const;

  // `g a`, from another page.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(
    page.getByRole("heading", { level: 1, name: "Agents" }),
    "the g a key opened no Agents page"
  ).toBeVisible();
  await expect(planner).toBeVisible();
  if (phone) await assertPageFits(page);

  // The four groups with their counts, and the order inside them.
  await expectAgentGroups(page, opened, "the page as it opens");
  await expect(titles, "the rows' order").toHaveText(everyRow);
  await expect(
    main.getByText("5 matching", { exact: true }),
    "the select-all header's count"
  ).toBeVisible();
  await expect(
    planner.getByRole("link", { exact: true, name: "Needs you 1" }),
    "the row's Needs you count"
  ).toBeVisible();
  await expect(
    planner.getByRole("link", { exact: true, name: "Waiting on agent 1" }),
    "the row's Waiting on agent count"
  ).toBeVisible();
  const unread = planner.getByRole("button", { exact: true, name: "New reply 1 from Planner" });
  await expect(unread, "the row's count of replies not read").toBeVisible();

  // A group's fold.
  await toggleAgentGroup(page, "idle");
  await expect(titles, "the Idle group's fold").toHaveText(["Planner", "Reviewer", "Sleeper"]);
  await toggleAgentGroup(page, "idle");
  await expect(titles).toHaveText(everyRow);

  // The search box, by a title and by the issue an open ask names.
  const search = control(controlRow(main), CONTROL.searchBox);
  await search.fill("reviewer");
  await expect(titles, "the search box, by a title").toHaveText(["Reviewer"]);
  await expectAgentGroups(
    page,
    ["Needs you (0)", "Active (1)", "Idle (0)", "Quiet (0)"],
    "the search box"
  );
  await expect(
    main.getByText("1 matching", { exact: true }),
    "the select-all header's count under the search box"
  ).toBeVisible();
  await search.fill(issue);
  await expect(titles, "the search box, by an open ask's issue").toHaveText(["Planner"]);
  await search.fill("");
  await expect(titles).toHaveText(everyRow);

  // The three multi-selects, each with its chip.
  const chips = control(controlRow(main), CONTROL.chip);
  for (const [field, option, shown, headings] of [
    ["group", "Quiet", ["Sleeper"], ["Needs you (0)", "Active (0)", "Idle (0)", "Quiet (1)"]],
    [
      "machine",
      "box-2",
      ["Auditor", "Tester", "Sleeper"],
      ["Needs you (0)", "Active (0)", "Idle (2)", "Quiet (1)"],
    ],
    ["role", "reviewer", ["Reviewer"], ["Needs you (0)", "Active (1)", "Idle (0)", "Quiet (0)"]],
  ] as const) {
    await openFilterPanel(page, main);
    await expect(agentFilter(page, field), `the ${field} narrowing`).toBeVisible();
    await pickAgentFilter(page, field, option);
    await expect(titles, `the ${field} narrowing`).toHaveText(shown);
    await expectAgentGroups(page, headings, `the ${field} narrowing`);
    await expect(chips, `the ${field} narrowing's chip`).toHaveCount(1);
    await chips.click();
    await expect(titles, `the ${field} narrowing's chip taken off`).toHaveText(everyRow);
  }

  // The read mark: the count's own press opens the conversation, which reads it. The row is
  // shut again, and the focus left in it.
  await unread.click();
  await expect(unread, "opening the row left its replies unread").toHaveCount(0);
  await plannerToggle.click();

  // The page's own keys. `/` is the page's search box, shadowing the global search bar; `j` from
  // the box takes the first row, the one the press above left the focus in.
  await page.keyboard.press("/");
  await expect(search, "the / key").toBeFocused();
  await expect(page.getByRole("dialog", { name: "Search" })).toHaveCount(0);
  await search.blur();
  await page.keyboard.press("j");
  await expect(planner, "the j key").toBeFocused();
  await page.keyboard.press("j");
  await expect(reviewer, "the j key, again").toBeFocused();
  await page.keyboard.press("k");
  await expect(planner, "the k key").toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    planner.getByRole("textbox", { name: "Comment" }),
    "the Enter key, into the row's composer"
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(planner, "the Escape key, out of the composer").toBeFocused();
  const plannerMark = planner.getByRole("checkbox", { name: "Select Planner for broadcast" });
  await page.keyboard.press("x");
  await expect(plannerMark, "the x key").toBeChecked();
  await page.keyboard.press("x");
  await expect(plannerMark).not.toBeChecked();
  await page.keyboard.press("j");
  await expect(reviewer).toBeFocused();
  await page.keyboard.press("i");
  const picker = reviewer.getByRole("button", { name: "Choose issue" });
  await expect(picker, "the i key").toHaveAttribute("aria-expanded", "true");
  await expect(issueSearchIn(reviewer)).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(picker, "the Escape key, out of the Issue list").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  await expect(reviewer).toBeFocused();
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await expect(tester).toBeFocused();
  await page.keyboard.press("Shift+P");
  await expect(
    tester.getByRole("button", { name: "Unpin Tester" }),
    "the Shift+P key"
  ).toBeVisible();
  const pinned = ["Planner", "Reviewer", "Tester", "Auditor", "Sleeper"];
  await expect(titles, "the pin's place at the head of its group").toHaveText(pinned);

  // The read mark, read where the page draws it. The Planner answers a second message while its
  // row is shut, which marks nothing read, so its badge counts that one reply; a mark that was
  // lost would count the first reply too.
  await plannerToggle.click();
  await expect(plannerToggle, "the Planner's row did not shut").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  const askedAgain = await createAgentMessage(KEPT_AGENT.planner, {
    body: "And the tests?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    askedAgain.id,
    { attempt: 1, body: "Green." },
    { id: KEPT_AGENT.planner, kind: "session" }
  );
  const plannerBadge = planner.getByRole("button", { name: /^New repl/ });
  await expect(plannerBadge, "the Planner's reply to a shut row").toHaveAccessibleName(
    "New reply 1 from Planner"
  );

  // What the page stores for the reader, found again after a reload.
  await page.reload();
  await expect(
    tester.getByRole("button", { name: "Unpin Tester" }),
    "the pin, after a reload"
  ).toBeVisible();
  await expect(titles, "the pinned order, after a reload").toHaveText(pinned);
  // The badge is drawn from the read state the reload loads, so its number is read only once
  // that state has answered: one reply unread, the one written after the mark above.
  await expect(plannerBadge, "the read mark, after a reload").toHaveAccessibleName(
    "New reply 1 from Planner"
  );

  // The bulk action: select-all over a narrowing, Clear selection, then two rows and a send.
  await pickAgentFilter(page, "role", "tester");
  await main.getByRole("checkbox", { exact: true, name: "Select all matching agents" }).check();
  await expect(
    main.getByText("3 of 3 matching selected", { exact: true }),
    "select-all"
  ).toBeVisible();
  await expectAgentGroups(
    page,
    ["Needs you (0)", "Active (0)", "Idle (2, 2 selected)", "Quiet (1, 1 selected)"],
    "select-all"
  );
  const composer = page.getByRole("region", { name: "Broadcast" });
  await expect(composer, "select-all drew no broadcast composer").toBeVisible();
  await main.getByRole("button", { exact: true, name: "Clear selection" }).click();
  await expect(composer, "Clear selection").toHaveCount(0);
  await chips.click();
  await expect(titles).toHaveText(pinned);
  await reviewer.getByRole("checkbox", { name: "Select Reviewer for broadcast" }).check();
  await tester.getByRole("checkbox", { name: "Select Tester for broadcast" }).check();
  await expect(
    main.getByText("2 of 5 matching selected", { exact: true }),
    "two rows' checkboxes"
  ).toBeVisible();
  await expect(
    composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" }),
    "the composer's count"
  ).toBeVisible();
  await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
  await composer.getByRole("button", { name: "Send to 2" }).click();
  await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
  await expect
    .poll(
      async () =>
        (await getSentMessages())
          .filter((sent) => sent.message === "Report status.")
          .map((sent) => String(sent.target_session))
          .sort(),
      { message: "the broadcast sent no message to each selected session" }
    )
    .toEqual([KEPT_AGENT.reviewer, KEPT_AGENT.tester].sort());
});

// R6 on an issue's Artifacts tab. What the tab offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Field: the one text box, which keeps an artifact whose name or kind holds the words
//   (`features/artifacts/ArtifactsTab.tsx:243`, :281-289, :306-317) and says so when none does
//   (:333-338). Worked by a name, by a kind and by words nothing holds.
// - Sort: no control. The primary document first, then the newest written, then the id
//   (:269-280). Read as the rows' order, and again once an upload is the newest.
// - Grouping: none.
// - Counts: each row's `N versions` (:196-202) and the references' `References (N)` (:116-150).
//   Both read; the references opened and their rows counted against the number.
// - Views: the references, shut until opened (:121-127). Opened.
// - Bulk action: none; a row's own controls (copy, approval, Download, Remove, :211-231) act on
//   that row alone.
// - The strip's two controls that add to the list: New document (:318-323,
//   `features/artifacts/CreateDocument.tsx`) and Upload (:324, `features/artifacts/ArtifactUpload.tsx`).
//   Each used, and what it added read in the list.
// - Keys: the tab's list answers none; `t a`, the issue page's binding that opens the tab
//   (`features/issue/IssuePage.tsx:153`, :324-330). Pressed.
// - Stored for the reader: nothing; the words were the tab's own state (:243), kept nowhere.
// SLOW(>=2s): four seeded documents, an upload and a new document, read in the tab.
test("an issue's Artifacts tab keeps its narrowing, its order, its counts, its strip and its key @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const home = await createIssue({ project: "CORE", title: "Runbook home" });
  const runbook = await createIssueArtifact(home.key, {
    content: "# Runbook\n\nThe steps, in order.\n",
    name: "runbook.md",
  });
  const issue = await createIssue({
    project: "CORE",
    spec: `# Kept spec\n\nThe steps are dispatch://${home.key}/artifact/${runbook.artifact.slug}.\n`,
    title: "Artifacts kept",
  });
  const plan = await createIssueArtifact(issue.key, {
    content: "# Plan\n\nThe first draft.\n",
    name: "plan.md",
  });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Notes\n\nKept as written.\n",
    name: "notes.md",
  });
  // The plan takes a second version after the notes were written, so it is the newer of the two.
  await editArtifact(
    plan.artifact.id,
    { ops: [{ find: "first draft", op: "replace", with: "second draft" }] },
    { as: "agent" }
  );
  await expect
    .poll(async () => ((await getArtifact(plan.artifact.id)).versions ?? []).length)
    .toBe(2);
  const spec = (await listIssueArtifacts(issue.key)).find((artifact) => artifact.primary);
  if (spec === undefined) throw new Error("the issue holds no primary document");

  // `t a`, from the issue's own page. On a phone the review panel opens over the page as a
  // dialog, which holds the keyboard, so it is shut first.
  await page.goto(`/issues/${issue.key}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  if (phone) {
    const reviewPanel = page.getByRole("button", { name: /review panel/i });
    if ((await expandedState("the review panel", reviewPanel)) === "open") {
      await reviewPanel.click();
    }
  }
  await page.locator("body").focus();
  await page.keyboard.press("t");
  await page.keyboard.press("a");
  await expect(page, "the t a key").toHaveURL(new RegExp(`/issues/${issue.key}/artifacts$`));
  const main = page.getByTestId("main-content");
  const rows = main.locator('[data-testid^="artifact-"]');
  const order = () =>
    rows.evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-testid") ?? ""));
  await expect(rows).toHaveCount(3);
  if (phone) await assertPageFits(page);

  // The order, and each row's count of versions.
  expect(await order(), "the order: the primary document, then the newest written").toEqual([
    `artifact-${spec.slug}`,
    `artifact-${plan.artifact.slug}`,
    `artifact-${notes.artifact.slug}`,
  ]);
  await expect(rows.first().getByText("Primary", { exact: true })).toBeVisible();
  await expect(
    main.getByTestId(`artifact-${plan.artifact.slug}`),
    "a row's count of versions"
  ).toContainText("doc · 2 versions");
  await expect(main.getByTestId(`artifact-${notes.artifact.slug}`)).toContainText(
    "doc · 1 version"
  );

  // The references' count, which is the number of rows they open on.
  const references = main.getByText(/^References \(\d+\)$/);
  await expect(references, "the references' count").toBeVisible();
  const counted = Number(/\((\d+)\)/.exec((await references.textContent()) ?? "")?.[1]);
  await references.click();
  const referenced = main.getByRole("region", { name: "References" });
  await expect(referenced.getByRole("listitem"), "the references' count").toHaveCount(counted);
  await expect(referenced.getByRole("link", { exact: true, name: "runbook.md" })).toBeVisible();

  // Upload, which adds a file - a kind the text box narrows by.
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from("one line\n"),
    mimeType: "text/plain",
    name: "trace.log",
  });
  await main.getByRole("button", { exact: true, name: "Upload" }).click();
  const trace = main.getByTestId("artifact-trace-log");
  await expect(trace, "Upload").toContainText("file · 1 version");
  expect(await order(), "the order, with the upload the newest written").toEqual([
    `artifact-${spec.slug}`,
    "artifact-trace-log",
    `artifact-${plan.artifact.slug}`,
    `artifact-${notes.artifact.slug}`,
  ]);

  // The text box, by a name, by a kind, and by words no artifact holds.
  const filter = control(controlRow(main), CONTROL.searchBox);
  await filter.fill("plan");
  await expect(rows, "the text box, by a name").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", `artifact-${plan.artifact.slug}`);
  await filter.fill("file");
  await expect(rows, "the text box, by a kind").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", "artifact-trace-log");
  await filter.fill("nothing holds this");
  await expect(rows).toHaveCount(0);
  await expect(
    main.getByText("No artifacts match this filter.", { exact: true }),
    "the text box's empty state"
  ).toBeVisible();
  await filter.fill("");
  await expect(rows).toHaveCount(4);

  // New document, which opens what it made; the tab then lists it.
  await main.getByRole("button", { exact: true, name: "New document" }).click();
  const form = page.getByRole("form", { name: "New document" });
  await form.getByRole("textbox", { exact: true, name: "Title" }).fill("Drafted");
  await form.getByRole("button", { exact: true, name: "Create" }).click();
  await expect(page, "New document").toHaveURL(
    new RegExp(`/issues/${issue.key}/artifacts/drafted`)
  );
  await page.goto(`/issues/${issue.key}/artifacts`);
  await expect(rows, "New document's row").toHaveCount(5);
  await expect(rows.nth(1), "New document's row").toContainText("Drafted");
});

/** The search bar's keep row's seed: four issues and a comment holding `harbour`, over two
 *  projects, two authors and two statuses; a project document holding it too, whose paragraph
 *  the writing cursor goes into; and twenty-two `beacon` issues, more than the bar lists at once
 *  (`searchDefaultLimit`, `packages/envoy/internal/dispatch/api/search.go`). */
async function seedKeptSearch(): Promise<{ documentSlug: string; rollout: string }> {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  const rollout = await createIssue({ project: "CORE", title: "Harbour rollout" });
  await createComment(rollout.key, { body: "The harbour tide decides the day." });
  const lights = await createIssue({ project: "CORE", title: "Harbour lights" });
  await patchIssue(lights.key, { status: "in_progress" });
  await createIssue({ project: "CORE", title: "Harbour pilot" }, { login: "bob" });
  await createIssue({ project: "OPS", title: "Harbour crane" }, { login: "bob" });
  const document = await createProjectDocument("CORE", {
    content: "# Harbour notes\n\nThe sextant sights the horizon.\n",
    name: "Harbour notes",
  });
  for (let index = 1; index <= 22; index++) {
    await createIssue({ project: "CORE", title: `Beacon ${index}` });
  }
  return { documentSlug: document.artifact.slug, rollout: rollout.key };
}

/** The owners the bar's hits are grouped under, each as its group is named: `KEY: title`. */
function hitOwners(bar: Locator): Promise<string[]> {
  return bar
    .locator(
      '#search-results [id^="search-group-issue:"], #search-results [id^="search-group-document:"]'
    )
    .evaluateAll((heads) => heads.map((head) => head.getAttribute("aria-label") ?? "").sort());
}

// R6 on the search bar. What the bar offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Fields: the words in the box (`features/search/SearchPalette.tsx:724-742`); Project
//   (`features/search/SearchFilterBar.tsx:149-156`), Status (:157-164), Author, taken on Enter
//   (:165-181), and Updated since (:182-198), each a removable chip once set (:137-148,
//   `features/search/search-filters.ts:65-89`) and each a parameter of the search (:91-104). Each
//   is set, the hits it leaves read, and its chip taken off again.
// - Sort: none to choose; the hits come in the order the search ranks them.
// - Grouping: the hits under the issue or document they belong to (`SearchPalette.tsx:472`,
//   :780-851, `features/search/search-model.ts:25-44`); the Recent, Actions and Projects groups
//   above them (:546-574), and More actions, This issue's agent and New issue below (:575-596).
//   The hits' groups, Recent, Actions, Projects and New issue are read. More actions holds what
//   the page's keys offer, and This issue's agent only a live session holding the issue.
// - Count: `Showing the best N of M matches` (:891-896). Read.
// - Views: the recent items while the box is empty, the hits while it holds words (:507-524).
//   Both read.
// - Bulk action: none.
// - Keys: in the box, ArrowDown and ArrowUp move the highlight (:685-700) and Enter opens it
//   (:701-704); Tab narrows the scope to the block being written in and Backspace on an empty
//   box widens it to the project (`features/search/useBarScope.tsx:72-90`); `$mod+k` shuts the
//   bar (`SearchPalette.tsx:433-441`) and so does Escape (`features/shell/useDialog.ts:124-137`);
//   and the three global bindings that open it, `$mod+k`, `/` and `g p` (`app.tsx:547-597`).
//   Each pressed and what it did read.
// - Stored for the reader: the recent items, in local storage under `search.recents`
//   (`features/search/recents.ts:26-87`). Found again after a reload.
// SLOW(>=2s): twenty-six seeded issues and a document, the bar opened six times over four pages.
test("the search bar keeps its narrowings, its groups, its count, its recent items and its keys @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const seed = await seedKeptSearch();
  const bar = page.getByRole("dialog", { name: "Search" });
  const box = bar.getByRole("combobox", { name: "Search" });
  const scope = bar.getByTestId("search-scope");
  // Four issues, and the project document, which is grouped under its own name.
  const harbour = [
    "CORE-1: Harbour rollout",
    "CORE-2: Harbour lights",
    "CORE-3: Harbour pilot",
    "CORE: Harbour notes",
    "OPS-1: Harbour crane",
  ].sort();

  // The three keys that open the bar, and the two that shut it.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  for (const [keys, shut] of [
    [["/"], "Escape"],
    [["g", "p"], "Control+k"],
    [["Control+k"], "Escape"],
  ] as const) {
    await page.locator("body").focus();
    for (const key of keys) await page.keyboard.press(key);
    await expect(bar, `the ${keys.join(" ")} key`).toBeVisible();
    await expect(box).toBeFocused();
    await page.keyboard.press(shut);
    await expect(bar, `the ${shut} key in the bar`).toHaveCount(0);
  }

  // The words, and the hits grouped under what they belong to.
  await page.keyboard.press("Control+k");
  await box.fill("harbour");
  await expect.poll(() => hitOwners(bar), { message: "the words' hits" }).toEqual(harbour);
  await expect(
    bar.getByRole("group", { name: "CORE-1: Harbour rollout" }).getByRole("option"),
    "an issue's group, holding the issue and its comment"
  ).toHaveCount(2);
  if (phone) await assertPageFits(page);

  // The four narrowings, each from the one panel and each a chip.
  const chips = control(controlRow(bar), CONTROL.chip);
  const filters = page.getByRole("dialog", { name: "Filter" });
  const tomorrow = await page.evaluate(() => {
    const day = new Date();
    day.setDate(day.getDate() + 1);
    const pad = (value: number) => String(value).padStart(2, "0");
    return `${day.getFullYear()}-${pad(day.getMonth() + 1)}-${pad(day.getDate())}`;
  });
  const pick = async (list: string, option: string) => {
    await filters.getByRole("button", { exact: true, name: list }).click();
    await page
      .getByRole("listbox", { name: `${list} options` })
      .getByRole("option", { exact: true, name: option })
      .click();
  };
  for (const [narrowing, field, set, left] of [
    [
      "Project",
      filters.getByRole("button", { exact: true, name: "Project" }),
      () => pick("Project", "OPS"),
      ["OPS-1: Harbour crane"],
    ],
    [
      "Status",
      filters.getByRole("button", { exact: true, name: "Status" }),
      () => pick("Status", "In progress"),
      ["CORE-2: Harbour lights"],
    ],
    [
      "Author",
      filters.getByRole("searchbox", { name: "Author" }),
      async () => {
        await filters.getByRole("searchbox", { name: "Author" }).fill("bob");
        await page.keyboard.press("Enter");
      },
      ["CORE-3: Harbour pilot", "OPS-1: Harbour crane"],
    ],
    [
      "Updated since",
      filters.getByLabel("Updated since"),
      () => filters.getByLabel("Updated since").fill(tomorrow),
      [],
    ],
  ] as const) {
    await openFilterPanel(page, bar);
    await expect(field, `the ${narrowing} narrowing`).toBeVisible();
    await set();
    await closeControlPanel(bar);
    await expect(
      chips.filter({ hasText: new RegExp(`^${narrowing} `) }),
      `the ${narrowing} narrowing's chip`
    ).toHaveCount(1);
    await expect
      .poll(() => hitOwners(bar), { message: `the ${narrowing} narrowing` })
      .toEqual([...left].sort());
    await chips.click();
    await expect(chips).toHaveCount(0);
    await expect.poll(() => hitOwners(bar)).toEqual(harbour);
  }

  // The count of what the bar holds back, and the projects the words name.
  await box.fill("beacon");
  await expect(
    bar.getByText("Showing the best 20 of 22 matches", { exact: true }),
    "the count of matches"
  ).toBeVisible();
  await box.fill("ops");
  await expect(
    bar.getByRole("group", { name: "Projects" }).getByRole("option"),
    "the Projects group"
  ).toContainText(["Ops"]);

  // The arrows move the highlight over the hits, and Enter opens the one it is on.
  await box.fill("harbour");
  await expect.poll(() => hitOwners(bar)).toEqual(harbour);
  const first = await box.getAttribute("aria-activedescendant");
  await page.keyboard.press("ArrowDown");
  await expect(box, "the ArrowDown key").not.toHaveAttribute("aria-activedescendant", first ?? "");
  await page.keyboard.press("ArrowUp");
  await expect(box, "the ArrowUp key").toHaveAttribute("aria-activedescendant", first ?? "");
  await page.keyboard.press("Enter");
  await expect(bar, "the Enter key").toHaveCount(0);
  await expect(page, "the Enter key").toHaveURL(/\/(issues|projects)\//);

  // Over an issue: the page's actions on an empty box, Backspace widening the scope from the
  // issue to its project, and the new issue the bar offers words nothing holds, which it files
  // in the page's project.
  await page.goto(`/issues/${seed.rollout}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(scope).toHaveText(new RegExp(`Acting on ${seed.rollout}`));
  await expect(
    bar.getByRole("group", { name: "Actions" }).getByRole("option"),
    "the Actions group"
  ).not.toHaveCount(0);
  await page.keyboard.press("Backspace");
  await expect(scope, "the Backspace key").toHaveText(/Acting on the project CORE/);
  await box.fill("zebrafish");
  await expect(
    bar.getByRole("group", { name: "New issue" }).getByRole("option", {
      name: /Make a new issue from .zebrafish./,
    }),
    "the New issue group"
  ).toHaveCount(1);
  await page.keyboard.press("Escape");

  // Tab narrows it to the block the writing cursor is in.
  await page.goto(`/projects/CORE/documents/${seed.documentSlug}`);
  await expect(documentEditor(page)).toContainText("The sextant sights the horizon.");
  await placeCaret(page, "after", "sextant");
  await expect(documentEditor(page)).toBeFocused();
  await page.keyboard.press("Control+k");
  await expect(scope).toHaveText(/Harbour notes/);
  await page.keyboard.press("Tab");
  await expect(scope, "the Tab key").toHaveText(/The sextant sights the horizon/);
  await page.keyboard.press("Escape");

  // The recent items, kept in the browser and listed again after a reload.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(box).toHaveValue("");
  const recent = bar.getByRole("group", { name: "Recent" });
  await expect(
    recent.getByRole("option", { name: new RegExp(seed.rollout) }),
    "a recent issue, after a reload"
  ).toHaveCount(1);
  await expect(
    recent.getByRole("option", { name: /Harbour notes/ }),
    "a recent document, after a reload"
  ).toHaveCount(1);
  expect(
    await page.evaluate(() => localStorage.getItem("dispatch.search.recents:alice")),
    "the recent items' stored value"
  ).toContain(`/issues/${seed.rollout}`);
});

// SLOW(>=2s): the search bar and its Filter panel opened twice, for a typed date and a set one.
// The Filter panel draws its Updated since box only while no date is set (`SearchPalette.tsx`),
// and the box takes a typed date once its year has four digits, so the reader's own date takes
// the focused box away. A date set or typed in the box is the reader's press, whether the engine
// fires `change` before the page drops the box (Chromium, for a typed date) or `input` alone
// (Firefox, as the last digit completes the date, and Playwright's fill(), the value set without
// keys as assistive technology sets it). A caused walk-back passes over Author, which takes typed
// text, so the reader's date keeps the focus inside the Filter panel on no text field; a loss
// misread as uncaused leaves the focus on the page's body, outside that panel. WebKit's date box
// takes no typed date from Playwright's keys, so the typed case is not read there. A focused
// control taken away, for which Firefox and WebKit fire no `focusout`: the clause of the `webkit`
// and `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("a date the reader gives the search bar's Updated since is their press, so the focus stays in the Filter panel @firefox @webkit", async ({
  browserName,
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Harbour lights" });
  await page.setViewportSize({ height: 900, width: 1440 });
  const wrong: string[] = [];
  for (const how of ["typed", "set"] as const) {
    if (how === "typed" && browserName === "webkit") continue;
    await page.goto("/");
    await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
    await page.keyboard.press("Control+k");
    const bar = page.getByRole("dialog", { name: "Search" });
    await expect(bar).toBeVisible();
    await page.keyboard.type("Harbour");
    await control(controlRow(bar), CONTROL.panelTrigger).click();
    const filters = page.getByRole("dialog", { name: "Filter" });
    const since = filters.getByLabel("Updated since");
    await expect(since).toBeVisible();
    const day = await page.evaluate(() => {
      const next = new Date();
      next.setDate(next.getDate() + 1);
      const pad = (value: number) => String(value).padStart(2, "0");
      return {
        iso: `${next.getFullYear()}-${pad(next.getMonth() + 1)}-${pad(next.getDate())}`,
        typed: `${pad(next.getMonth() + 1)}${pad(next.getDate())}${next.getFullYear()}`,
      };
    });
    if (how === "typed") {
      await since.focus();
      await page.keyboard.type(day.typed);
    } else {
      await since.fill(day.iso);
    }
    await expect(since).toHaveCount(0);
    // Every hand-off of the loss runs within a frame and a task of it, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // The answer is read now: every hand-off of the loss has run, and nothing else moves the focus.
    const holder = await focusHolder(page);
    // Read after the same hand-offs as the holder above, so where the focus is is settled.
    const inFilter = await filters.evaluate((panel) => panel.contains(document.activeElement));
    if (!inFilter)
      wrong.push(`a ${how} date: the focus is on ${holder.name}, outside the Filter panel`);
    else if (holder.takesText)
      wrong.push(`a ${how} date: the focus is in ${holder.name}, which takes text`);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): three live agents pinned again at each of two widths, and the Agents page opened
// at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-a5b-h and -a5b-b at
// 3fce6150 and da0804df, and infra-3135-a5m at main's merge base 143b57a7): three live agents
// are pinned, Alpha first, and the reader presses Enter on Alpha's Unpin on the Agents page. The
// page lists pinned agents first, in the order they were pinned (`orderAgents`,
// features/agents/AgentsPage.tsx), so Alpha's row moves to the end of the list, below Beta's and
// Gamma's, and the focus follows it to that row's Pin Alpha, in Chromium, Firefox and WebKit at
// both commits. At 1440x900 the three rows stand in the window and Pin Alpha with them, at
// 505 px. At 390x664 the rows are 126 px tall. In Chromium and Firefox the focus put on Unpin
// Alpha has scrolled the window to 328 px, and Pin Alpha lands at 546 px, in sight. In WebKit the
// window stands at 58 px, Pin Alpha lands at 816 px, below the 664 px window, and nothing
// scrolls, at both commits. Main does the same in WebKit: Pin Alpha at 505 px at 1440, and at
// 816 px with the window held at 58 px at 390. The architect's rulings: a landing the reader
// caused must be in view (2026-11-19 10:3xZ, WCAG 2.2's 2.4.11 Focus Not Obscured), and the one
// scroll a landing may make is to the nearest edge of a control partly in sight, the least
// distance that shows it whole (2026-11-21 12:3xZ). Which control the press lands on is not read
// here: 2026-11-19 22:3xZ names the same control in the row now at the pressed row's place, and
// main's page means the focus to follow the row to where it lands (AgentsPage.tsx,
// `focusAfterPin`). A focused control taken away, for which Firefox and WebKit fire no
// `focusout` (React moves the row's node): the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("Unpin on a pinned agent keeps the keyboard focus on a control in sight, on the Agents page at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  const pinnedAgent = (sessionId: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: "pin-host",
    roles: [],
    session_id: sessionId,
    title,
  });
  const alpha = pinnedAgent("01a0e200-4848-7473-acc5-0000000000a5", "Alpha");
  const beta = pinnedAgent("01a0e200-4848-7473-acc5-0000000000b5", "Beta");
  const gamma = pinnedAgent("01a0e200-4848-7473-acc5-0000000000c5", "Gamma");
  const agents = [alpha, beta, gamma];
  await setLiveSessions(agents);
  /** What holds the focus and where the window stands: the focused element's name, the agent row
   *  it is in, its top and height, and whether its box stands wholly inside the window. */
  const landing = () =>
    page.evaluate(() => {
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { height: 0, inSight: false, name: "the body", row: null, scrollY, top: null };
      }
      const own = focused.getBoundingClientRect();
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return {
        height: Math.round(own.height),
        inSight:
          own.top >= -1 &&
          own.bottom <= window.innerHeight + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        name: `${focused.tagName.toLowerCase()} "${words}"`,
        row: focused.closest("[data-agent-row]")?.getAttribute("data-agent-row") ?? null,
        scrollY,
        top: Math.round(own.top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Agents page at ${size.width}x${size.height}`;
    // Pinned again in this order at each width, so Alpha is the first pinned row each time.
    for (const agent of agents) await putAgentState(agent.session_id, { pinned: false });
    for (const agent of agents) await putAgentState(agent.session_id, { pinned: true });
    await page.goto("/agents");
    const unpin = agentRow(page, alpha.session_id).getByRole("button", {
      exact: true,
      name: "Unpin Alpha",
    });
    await expect(unpin).toBeVisible();
    await expect(
      agentRow(page, gamma.session_id).getByRole("button", { exact: true, name: "Unpin Gamma" })
    ).toBeVisible();
    await unpin.focus();
    await nextFrames(page);
    // One instant is the point: where the window stands just before the press.
    const before = await landing();
    await page.keyboard.press("Enter");
    // The pin's write has landed once Alpha's row offers to pin it again.
    await expect(
      agentRow(page, alpha.session_id).getByRole("button", { exact: true, name: "Pin Alpha" })
    ).toBeVisible();
    // The poll waits, up to the expect timeout, for the focus the moved row took to land again.
    await expect
      .poll(async () => (await landing()).top !== null)
      .toBe(true)
      .catch(() => undefined);
    await settledScrollTop(page);
    // One read is the answer: every hand-off of the press has run, and the window holds still.
    const now = await landing();
    const rowTitle = agents.find((agent) => agent.session_id === now.row)?.title ?? now.row;
    const what = now.row === null ? now.name : `${now.name} in ${rowTitle}'s row`;
    const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}`;
    if (now.top === null) {
      wrong.push(`${where}: after Enter on Unpin Alpha the focus is on the body (${numbers})`);
    } else if (!now.inSight) {
      wrong.push(
        `${where}: after Enter on Unpin Alpha the focus is on ${what}, out of sight at ${now.top} px in a ${size.height} px window (${numbers})`
      );
    }
    // The 12:3xZ allowance: the landing's own height, and a pixel for the rounding of a box.
    const allowed = now.height + 1;
    if (Math.abs(now.scrollY - before.scrollY) > allowed) {
      wrong.push(
        `${where}: after Enter on Unpin Alpha the window scrolled ${now.scrollY - before.scrollY} px, more than the landing's own height, to ${what} (${numbers})`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** Where the keyboard focus is, read once: the focused element's tag and name, whether it is the
 *  body, whether it stands inside an element `inside` matches (a CSS selector, or none), whether
 *  its box stands wholly inside the window, and where the window stands. */
function focusPlace(
  page: Page,
  inside?: string
): Promise<{ body: boolean; inSight: boolean; inside: boolean; name: string; scrollY: number }> {
  return page.evaluate((inside) => {
    const scrollY = Math.round(window.scrollY);
    const focused = document.activeElement;
    if (!(focused instanceof HTMLElement) || focused === document.body) {
      return { body: true, inSight: false, inside: false, name: "the body", scrollY };
    }
    const own = focused.getBoundingClientRect();
    const words = (focused.getAttribute("aria-label") || focused.textContent || "")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 40);
    return {
      body: false,
      inSight:
        own.top >= -1 &&
        own.bottom <= window.innerHeight + 1 &&
        own.left >= -1 &&
        own.right <= window.innerWidth + 1,
      inside: inside !== null && focused.closest(inside) !== null,
      name: `${focused.tagName.toLowerCase()} "${words}"`,
      scrollY,
    };
  }, inside ?? null);
}

// SLOW(>=2s): an issue's references read answered, failed over its answer and retried, at each
// of two widths.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-n2-h and -n2-b at 3fce6150
// and da0804df, and this row in -n-red at 3fce6150): an issue's Artifacts tab has read the issue's
// references once, and a message on another issue that mentions it makes the live feed read them
// again, which fails (500, three times by the shared retry policy). The tab says "References
// unavailable" with Retry, over the answer the read still holds. At the commits the row was
// measured on, Retry was natively disabled while its read was out (`disabled={retrying}` in
// components/QueryError.tsx, given `retrying={references.isFetching}` by
// features/artifacts/ArtifactsTab.tsx): Enter on Retry, with the read it sends held, drew Retry
// disabled, and the focus fell to the body for the whole hold, in Chromium, Firefox and WebKit at
// 1440x900 and 390x664, at both commits. A read that holds no answer yet is another path, which
// this row does not take: TanStack sets it back to pending the moment Retry sends, so the line and
// Retry go in the press's own render. The rule: a control held while the read its press started is
// out is held through components/held-press.ts, never natively disabled, so it keeps the keyboard
// focus (its own doc comment). A focused control disabled under the press, which each engine
// moves to the body at its own time: the clause of the `webkit` and `firefox` projects that names
// the page-own focus row (e2e/playwright.config.ts).
test("a references Retry on the Artifacts tab keeps the keyboard focus while its read is out @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a read answered, failed through the shared policy's retries and held.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  // The references read of the issue the row is on: answered, failed or held as the row says.
  const reads = { held: [] as Route[], mode: "answer" as "answer" | "fail" | "hold", path: "" };
  await page.route(
    (url) => url.pathname === reads.path,
    async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (reads.mode === "hold") {
        reads.held.push(route);
        return;
      }
      if (reads.mode === "answer") return route.continue();
      await route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "The references read failed." }),
        contentType: "application/json",
        status: 500,
      });
    }
  );
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Artifacts tab at ${size.width}x${size.height}`;
    const issue = await createIssue({ project: "CORE", title: `Lease format at ${size.width}` });
    const source = await createIssue({ project: "OPS", title: `Mirror plan at ${size.width}` });
    await createMessage(source.key, { body: `Depends on dispatch://${issue.key}.` });
    reads.mode = "answer";
    reads.path = `/api/v1/issues/${issue.key}/references`;
    await page.goto(`/issues/${issue.key}/artifacts`);
    // The read has answered once the tab counts what references the issue.
    await expect(
      page.locator("summary").filter({ hasText: /^Referenced by \(1\)$/ })
    ).toBeVisible();
    reads.mode = "fail";
    await createMessage(source.key, { body: `Also dispatch://${issue.key}.` });
    // The live feed reads the references again, and the shared policy retries the 500 twice.
    const alert = page.getByRole("alert").filter({ hasText: "References unavailable" });
    await expect(alert).toBeVisible();
    await alert.getByRole("button", { exact: true, name: "Retry" }).focus();
    reads.mode = "hold";
    await page.keyboard.press("Enter");
    await expect.poll(() => reads.held.length, `${where}: Retry sent no read`).toBe(1);
    // The focus fixup of a disabled control runs in a rendering update; two frames hold one.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: Retry's read is held, and its press's renders have all run.
    const holding = await focusPlace(page);
    if (holding.body) {
      wrong.push(`${where}: with the read Retry sent held the focus is on the body`);
    }
    reads.mode = "answer";
    for (const route of reads.held.splice(0)) await route.continue();
    await expect(alert).toHaveCount(0);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});
