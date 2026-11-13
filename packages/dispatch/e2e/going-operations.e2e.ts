import { getSentMessages, plannerSession, resetFakeEnvoy, setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  getAsk,
  getIssue,
  getIssueArtifactText,
  getIssueEvents,
  getMyGoingSettings,
  listAskFollowers,
  listComments,
  listIssueArtifacts,
  listIssueAsks,
  listIssueMessages,
  listMyGoingRuns,
  mintAgentToken,
  patchIssue,
  setNewIssueSettings,
} from "./api";
import { decisionRecord, modelsLane, scriptChat, scriptDecision } from "./fake-models-helpers";
import {
  answerText,
  askAndWait,
  bar,
  closeBar,
  countGoingWrites,
  decided,
  openBarOver,
  openShell,
  operationRow,
  submitOperation,
  withArguments,
  writeLines,
} from "./going-panel";
import { expect, type Locator, type Page, test } from "./test";

// Check c7 of dispatch://INFRA-1588/artifact/verification-md, serving R4: a confident write runs
// on submit, undo reverses or marks every write, and the two writes that cannot be taken back
// always ask. Every row scripts the fake decision model to choose an action - above the action
// threshold the committed record holds, read from it rather than written here - and the fake
// writing model to write that action's arguments, then drives the bar as a person does: types,
// pauses, submits with `$mod+Enter`, and presses the undo key (`$mod+z`) on another page. What a
// write and its undo did is read back from Dispatch's own routes. Every row runs in the models
// lane alone, each on a database of its own.

/** A confidence above the action threshold: halfway from it to certainty. */
function above(): number {
  return (decisionRecord().action_threshold + 1) / 2;
}

/** The three issues a turn changes, each at `todo`. */
async function seedThree(): Promise<string[]> {
  await createProject({ key: "CORE", name: "Core" });
  const keys: string[] = [];
  for (const title of ["Bearing grease", "Bearing seals", "Bearing shims"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo" });
    keys.push(issue.key);
  }
  return keys;
}

/** One issue to act on, in its own project. */
async function seedOne(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  return (await createIssue({ project: "CORE", title: "Bearing grease" })).key;
}

async function statuses(keys: readonly string[]): Promise<string[]> {
  return Promise.all(keys.map(async (key) => (await getIssue(key)).status));
}

/** Scripts a turn of `action` above the threshold, its invocations `args`. */
async function scriptTurn(
  action: string,
  args: readonly Readonly<Record<string, unknown>>[]
): Promise<void> {
  await scriptDecision({ default: decided(action, above()) });
  await scriptChat({ default: withArguments({ [action]: args }) });
}

/** Opens the bar over the project's issues, asks `words`, and submits the operation the decision
 *  chose once the answer has ended. */
async function runTurn(page: Page, words: string): Promise<void> {
  await openBarOver(page, "/projects/CORE/issues");
  await askAndWait(page, words);
  await expect(operationRow(page)).toBeVisible();
  await submitOperation(page);
}

/** Every line of the turn has landed, `count` of them. */
async function landed(page: Page, count: number): Promise<void> {
  await expect(writeLines(page)).toHaveCount(count);
  for (let index = 0; index < count; index += 1) {
    await expect(writeLines(page).nth(index)).toHaveAttribute("data-state", "done");
  }
}

/** Leaves the bar for `path` and presses the undo key there; answers what the shell says of the
 *  undo once Dispatch answered it. */
async function undoFrom(page: Page, path = "/settings"): Promise<Locator> {
  await closeBar(page);
  await openShell(page, path);
  await page.locator("body").focus();
  await page.keyboard.press("Control+z");
  const notice = page.getByTestId("going-undo-notice");
  await expect(notice).toHaveAttribute("data-state", "undone", { timeout: 20_000 });
  return notice;
}

/** The newest run of the bar alice holds, as Dispatch records it. */
async function newestRun() {
  const page = await listMyGoingRuns({ limit: 1 });
  const run = page.rows?.[0];
  if (run === undefined) throw new Error("alice holds no command-bar run");
  return run;
}

/** Each write of the newest run that went through, and whether an undo of it is recorded ok. */
async function undoneWrites(): Promise<{ action: string; target: string; undone: boolean }[]> {
  const run = await newestRun();
  return (run.writes ?? []).map((write) => ({
    action: write.action,
    target: write.target,
    undone: (run.undos ?? []).some((undo) => undo.write_id === write.id && undo.outcome.ok),
  }));
}

const startWords = "start work on the three CORE bearing issues";

// SLOW(>=2s): a decision and an answer, and a wait that proves no write follows them.
test("the same confident request paused without a submit writes nothing: only the answer streams", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const keys = await seedThree();
  await scriptTurn(
    "dispatch_issue_update",
    keys.map((issue) => ({ issue, status: "in_progress" }))
  );
  const writes = countGoingWrites(page);

  await openBarOver(page, "/projects/CORE/issues");
  await askAndWait(page, startWords);
  await expect(answerText(page)).toContainText("On it.");
  await expect(operationRow(page)).toBeVisible();
  // An absence window: the answer has ended, and nothing may write during this second without
  // a submit.
  await page.waitForTimeout(1_000);

  expect(writes.bodies).toEqual([]);
  await expect(writeLines(page)).toHaveCount(0);
  expect(await statuses(keys)).toEqual(["todo", "todo", "todo"]);
  expect((await newestRun()).writes ?? []).toEqual([]);
});

// SLOW(>=2s): a decision, an answer, three writes, a page load and an undo of three writes.
test("a confident request changes three issues on submit with no approval step, one line each, and the undo key on another page writes each back by the same route", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const keys = await seedThree();
  await scriptTurn(
    "dispatch_issue_update",
    keys.map((issue) => ({ issue, status: "in_progress" }))
  );
  const writes = countGoingWrites(page);

  await runTurn(page, startWords);

  await landed(page, 3);
  await expect(page.getByTestId("going-confirm-step")).toHaveCount(0);
  expect(writes.bodies.map((body) => [body.invocation, body.approved])).toEqual([
    [0, "auto"],
    [1, "auto"],
    [2, "auto"],
  ]);
  for (const [index, key] of keys.entries()) {
    const line = writeLines(page).nth(index);
    await expect(line).toHaveAttribute("data-item", key);
    await expect(line).toHaveAttribute("data-field", "status");
    await expect(line).toHaveAttribute("data-before", "todo");
    await expect(line).toHaveAttribute("data-now", "in_progress");
  }
  expect(await statuses(keys)).toEqual(["in_progress", "in_progress", "in_progress"]);

  const notice = await undoFrom(page);

  await expect(notice.getByTestId("going-undo-notice-line")).toHaveCount(3);
  expect(await statuses(keys)).toEqual(["todo", "todo", "todo"]);
  // Written back by the issue's own update route, as alice: the issue's last update event is
  // alice's, setting the status the turn found.
  for (const key of keys) {
    const updates = (await getIssueEvents(key, { limit: 100 })).filter(
      (event) => event.type === "issue.updated"
    );
    expect(updates.at(-1)).toMatchObject({
      actor: { id: "alice", kind: "user" },
      payload: { status: "todo" },
    });
  }
});

// SLOW(>=2s): a decision, an answer, a filing, a page load and an undo that closes the issue.
test("a new issue asks first at full confidence and writes nothing until accepted, and undo closes the issue it made with the undo reason and deletes it not", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await setNewIssueSettings({ label: "", session: "" });
  const words = "track the spare bearing stock";
  await scriptDecision({ default: decided("dispatch_issue", 1) });
  await scriptChat({
    default: withArguments({ dispatch_issue: [{ project: "CORE", title: words }] }),
  });
  const writes = countGoingWrites(page);

  await openBarOver(page, "/projects/CORE/issues");
  await askAndWait(page, words);
  const offer = bar(page).getByRole("option", { name: `Make a new issue from “${words}”` });
  await expect(offer).toBeVisible();
  await expect(operationRow(page)).toHaveCount(0);
  await submitOperation(page);
  // An absence window: a submit at full confidence must still write nothing for a new issue.
  await page.waitForTimeout(1_000);
  expect(writes.bodies).toEqual([]);
  expect((await newestRun()).writes ?? []).toEqual([]);

  await offer.click();

  await expect(page.getByTestId("search-new-issue-said")).toContainText("is made");
  expect(writes.bodies).toEqual([
    expect.objectContaining({ approved: "person", invocation: 0, offer: "dispatch_issue" }),
  ]);
  const [made] = await undoneWrites();
  const key = made?.target ?? "";
  expect((await getIssue(key)).title).toBe(words);

  await undoFrom(page);

  const closed = await getIssue(key);
  expect(closed.status).toBe("done");
  const messages = (await listIssueMessages(key)).map((read) => read.message.body);
  expect(messages.some((body) => body.startsWith("Undone from the command bar"))).toBe(true);
  expect(await undoneWrites()).toEqual([{ action: "dispatch_issue", target: key, undone: true }]);
});

// SLOW(>=2s): a decision, an answer, a comment, a page load and an undo.
test("a comment the turn posted is recorded undone and not deleted", async ({ page }) => {
  modelsLane();
  test.setTimeout(120_000);
  const key = await seedOne();
  const notes = await createIssueArtifact(key, {
    content: "# Notes\n\nThe first line.\n",
    name: "notes.md",
  });
  const body = "The first line reads well.";
  await scriptTurn("dispatch_comment", [
    { artifact: notes.artifact.slug, body, issue: key, quote: "The first line." },
  ]);

  await runTurn(page, "say the first line of the notes reads well");
  await landed(page, 1);
  expect((await listComments(key)).map((comment) => comment.body)).toContain(body);

  const notice = await undoFrom(page);

  await expect(notice.getByTestId("going-undo-notice-line")).toContainText("marked undone");
  expect((await listComments(key)).map((comment) => comment.body)).toContain(body);
  expect((await undoneWrites()).map((write) => [write.action, write.undone])).toEqual([
    ["dispatch_comment", true],
  ]);
});

// SLOW(>=2s): two turns, each a decision, an answer, a write, a page load and an undo.
test("undo writes a document's version back and an ask's text back", async ({ page }) => {
  modelsLane();
  test.setTimeout(150_000);
  const key = await seedOne();
  const notes = await createIssueArtifact(key, {
    content: "# Notes\n\nThe first line.\n",
    name: "notes.md",
  });
  const slug = notes.artifact.slug;
  const before = (await getIssueArtifactText(key, slug)).markdown;
  await scriptTurn("dispatch_doc_edit", [
    {
      artifact: slug,
      issue: key,
      ops: [{ find: "first line", op: "replace", with: "opening line" }],
    },
  ]);

  await runTurn(page, "call it the opening line in the notes");
  await landed(page, 1);
  expect((await getIssueArtifactText(key, slug)).markdown).toContain("opening line");

  await undoFrom(page);
  expect((await getIssueArtifactText(key, slug)).markdown).toBe(before);

  const ask = await createAsk(key, { question: "Ship it on Friday?" });
  await scriptTurn("dispatch_edit_ask", [{ ask: ask.id, question: "Ship it on Monday?" }]);

  await runTurn(page, "move the ship ask to Monday");
  await landed(page, 1);
  expect((await getAsk(ask.id)).ask.question).toBe("Ship it on Monday?");

  await undoFrom(page);
  expect((await getAsk(ask.id)).ask.question).toBe("Ship it on Friday?");
});

// SLOW(>=2s): three turns, each a decision, an answer, a write, a page load and an undo.
test("undo unfollows an ask the turn followed, takes back a claim the turn released, and retracts an ask the turn opened", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(180_000);
  await resetFakeEnvoy();
  await setLiveSessions([plannerSession]);
  const key = await seedOne();

  // A person follows an ask for a session they name: the bar asks for it, as the one argument
  // the follow is missing, by the title the person knows it by.
  const followed = await createAsk(key, { question: "Who reviews the change?" });
  await scriptTurn("dispatch_follow", [{ action: "follow", ask: followed.id }]);
  await runTurn(page, "have the planner follow the review ask");
  const field = bar(page).getByRole("textbox", { name: `Fill the session for ${followed.id}` });
  await expect(field).toBeFocused();
  await field.fill(plannerSession.title);
  await field.press("Enter");
  await landed(page, 1);
  expect(
    ((await listAskFollowers(followed.id)).followers ?? []).map((row) => row.session_id)
  ).toEqual([plannerSession.session_id]);
  await undoFrom(page);
  expect((await listAskFollowers(followed.id)).followers ?? []).toEqual([]);

  await claimIssue(key);
  await scriptTurn("dispatch_claim", [{ issue: key, release: true }]);
  await runTurn(page, "let go of the bearing grease issue");
  await landed(page, 1);
  expect((await getIssue(key)).claim).toBeNull();
  await undoFrom(page);
  expect((await getIssue(key)).claim?.actor).toMatchObject({ id: "alice", kind: "user" });

  const question = "Is the grease rated for the cold?";
  await scriptTurn("dispatch_ask", [{ issue: key, question }]);
  await runTurn(page, "ask whether the grease is rated for the cold");
  await landed(page, 1);
  const opened = (await listIssueAsks(key)).find((row) => row.question === question);
  expect(opened).toBeDefined();
  await undoFrom(page);
  expect((await getAsk(opened?.id ?? "")).ask.resolution?.kind).toBe("retracted");
});

// SLOW(>=2s): two turns, each a decision, an answer, a write, a page load and an undo.
test("undo leaves a message and an artifact the turn made in place, each marked undone", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(150_000);
  const key = await seedOne();

  const body = "The grease arrives on Thursday.";
  await scriptTurn("dispatch_message", [{ body, issue: key }]);
  await runTurn(page, "tell the issue the grease arrives on Thursday");
  await landed(page, 1);
  const notice = await undoFrom(page);
  await expect(notice.getByTestId("going-undo-notice-line")).toContainText("marked undone");
  expect((await listIssueMessages(key)).map((read) => read.message.body)).toContain(body);
  expect((await undoneWrites()).map((write) => [write.action, write.undone])).toEqual([
    ["dispatch_message", true],
  ]);

  await scriptTurn("dispatch_artifact", [
    { content: "# Stock\n\nTwelve tins.\n", issue: key, name: "stock.md" },
  ]);
  await runTurn(page, "write the stock count down as a document");
  await landed(page, 1);
  const again = await undoFrom(page);
  await expect(again.getByTestId("going-undo-notice-line")).toContainText("marked undone");
  expect((await listIssueArtifacts(key)).map((artifact) => artifact.name)).toContain("stock.md");
  expect((await undoneWrites()).map((write) => [write.action, write.undone])).toEqual([
    ["dispatch_artifact", true],
  ]);
});

// SLOW(>=2s): a decision, an answer and three writes, one of them refused.
test("a write Dispatch refuses shows its refusal in its own line, and the rest land", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const keys = await seedThree();
  const missing = "CORE-999";
  await scriptTurn("dispatch_issue_update", [
    { issue: keys[0], status: "in_progress" },
    { issue: missing, status: "in_progress" },
    { issue: keys[2], status: "in_progress" },
  ]);

  await runTurn(page, startWords);

  await expect(writeLines(page)).toHaveCount(3);
  await expect(writeLines(page).nth(0)).toHaveAttribute("data-state", "done");
  const refused = writeLines(page).nth(1);
  await expect(refused).toHaveAttribute("data-state", "refused");
  await expect(refused).toHaveAttribute("data-item", missing);
  await expect(refused).toContainText("refused:");
  // The route's own code for the issue it could not find, carried into the line.
  await expect(refused).toHaveAttribute("data-code", /^[A-Z_]+$/);
  await expect(writeLines(page).nth(2)).toHaveAttribute("data-state", "done");
  expect(await statuses([keys[0] ?? "", keys[2] ?? ""])).toEqual(["in_progress", "in_progress"]);
});

// SLOW(>=2s): a decision, an answer, a write held in the browser, and an undo behind it.
test("an undo of a write still waiting stops it before it is sent", async ({ page }) => {
  modelsLane();
  test.setTimeout(120_000);
  const keys = await seedThree();
  await scriptTurn(
    "dispatch_issue_update",
    keys.map((issue) => ({ issue, status: "in_progress" }))
  );
  const writes = countGoingWrites(page);
  // The first write is held on its way to Dispatch, so the two behind it on the run's line are
  // still waiting their turn in the page when the person undoes the turn.
  let release: () => void = () => undefined;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  let first = true;
  await page.route("**/api/v1/going/runs/*/writes", async (route) => {
    if (first) {
      first = false;
      await held;
    }
    await route.continue();
  });

  await runTurn(page, startWords);
  await expect(writeLines(page)).toHaveCount(3);
  await expect.poll(() => writes.bodies.length).toBe(1);

  await page.getByTestId("going-operation").getByRole("button", { name: "Undo" }).click();

  await expect(writeLines(page).nth(1)).toHaveAttribute("data-state", "stopped");
  await expect(writeLines(page).nth(2)).toHaveAttribute("data-state", "stopped");
  release();
  await expect(writeLines(page).nth(0)).toHaveAttribute("data-state", "done");
  await expect(page.getByTestId("going-undo-said")).toContainText("Undone.");
  await expect(page.getByTestId("going-undo-said")).toContainText(
    "2 writes still waiting were stopped before they were sent."
  );
  expect(writes.bodies.map((body) => body.invocation)).toEqual([0]);
  expect(await statuses(keys)).toEqual(["todo", "todo", "todo"]);
});

// SLOW(>=2s): a decision, an answer, three writes, a second writer, a page load and an undo.
test("an undo of a field a second writer changed is refused for that field, naming what changed, and the rest of the turn is undone", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const keys = await seedThree();
  await scriptTurn(
    "dispatch_issue_update",
    keys.map((issue) => ({ issue, status: "in_progress" }))
  );

  await runTurn(page, startWords);
  await landed(page, 3);
  await patchIssue(keys[1] ?? "", { status: "needs_review" }, { login: "bob" });

  const notice = await undoFrom(page);

  const changed = notice.locator(`[data-testid="going-undo-notice-line"][data-item="${keys[1]}"]`);
  await expect(changed).toHaveAttribute("data-undo", "refused");
  await expect(changed).toContainText("needs_review");
  for (const key of [keys[0], keys[2]]) {
    await expect(
      notice.locator(`[data-testid="going-undo-notice-line"][data-item="${key}"]`)
    ).toHaveAttribute("data-undo", "ok");
  }
  expect(await statuses(keys)).toEqual(["todo", "needs_review", "todo"]);
});

// SLOW(>=2s): a decision, an answer and a send the person approves.
test("a request to hand work to the live agent on the issue asks first at full confidence and sends nothing until the person accepts", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await resetFakeEnvoy();
  await setLiveSessions([plannerSession]);
  const key = await seedOne();
  const token = await mintAgentToken("going-operations", "alice");
  await claimIssue(key, {
    actor: {
      id: plannerSession.session_id,
      kind: "session",
      origin: { session_title: plannerSession.title },
    },
    as: "agent",
    token,
  });
  await scriptDecision({ default: decided("dispatch_send_to_agent", 1) });
  await scriptChat({
    default: withArguments({
      dispatch_send_to_agent: [
        {
          body: `${key}: write up the grease rota`,
          delivery: "aside",
          session: plannerSession.session_id,
        },
      ],
    }),
  });
  const writes = countGoingWrites(page);

  await openBarOver(page, `/issues/${key}`);
  await askAndWait(page, "write up the grease rota");
  const offer = bar(page).getByRole("option", { name: `Send this to the agent on ${key}` });
  await expect(offer).toBeVisible();
  await expect(operationRow(page)).toHaveCount(0);
  await submitOperation(page);
  // An absence window: a submit at full confidence must still send nothing to the agent.
  await page.waitForTimeout(1_000);
  expect(writes.bodies).toEqual([]);
  expect(await getSentMessages()).toHaveLength(0);

  await offer.click();

  await expect(page.getByTestId("search-send-to-agent-said")).toContainText("Sent to");
  expect(writes.bodies).toEqual([
    expect.objectContaining({ approved: "person", invocation: 0, offer: "dispatch_send_to_agent" }),
  ]);
  await expect.poll(async () => (await getSentMessages()).length).toBe(1);
});

// SLOW(>=2s): two turns, each a decision, an answer and three writes, and the Settings page.
test("with the per-person setting off a change runs with no prompt, with it on the same change asks first, and it is off by default", async ({
  page,
}) => {
  modelsLane();
  test.setTimeout(150_000);
  const keys = await seedThree();
  await scriptTurn(
    "dispatch_issue_update",
    keys.map((issue) => ({ issue, status: "in_progress" }))
  );
  const writes = countGoingWrites(page);

  expect(await getMyGoingSettings()).toEqual({ confirm_writes: false });
  await page.goto("/settings");
  const toggle = page.getByRole("checkbox", { name: "Ask before every write" });
  await expect(toggle).not.toBeChecked();

  await runTurn(page, startWords);
  await landed(page, 3);
  await expect(page.getByTestId("going-confirm-step")).toHaveCount(0);
  expect(writes.bodies.map((body) => body.approved)).toEqual(["auto", "auto", "auto"]);

  await page.keyboard.press("Escape");
  for (const key of keys) await patchIssue(key, { status: "todo" });
  await page.goto("/settings");
  await toggle.check();
  await expect.poll(() => getMyGoingSettings()).toEqual({ confirm_writes: true });

  await runTurn(page, startWords);
  const step = page.getByTestId("going-confirm-step");
  await expect(step).toBeVisible();
  await expect(step.getByTestId("going-write-line")).toHaveCount(3);
  // An absence window: with the setting on, nothing may be written before the person confirms.
  await page.waitForTimeout(1_000);
  expect(writes.bodies).toHaveLength(3);
  expect(await statuses(keys)).toEqual(["todo", "todo", "todo"]);

  await step.getByRole("button", { name: "Confirm" }).click();

  await landed(page, 3);
  expect(writes.bodies.slice(3).map((body) => body.approved)).toEqual([
    "confirmed",
    "confirmed",
    "confirmed",
  ]);
  expect(await statuses(keys)).toEqual(["in_progress", "in_progress", "in_progress"]);
});
