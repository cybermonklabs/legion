import {
  agentFilter,
  agentGroupToggle,
  agentRow,
  type FakeSession,
  openAgentFilters,
  pickAgentFilter,
  setLiveSessions,
  setSessionLive,
  shownAgentRows,
  toggleAgentGroup,
} from "./agents";
import {
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createProject,
  getIssueEvents,
  replyToMessageDelivery,
} from "./api";
import { expectRailAgents } from "./rail";
import { setEventCreatedAt } from "./seed";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * How the Agents page groups and narrows its sessions (INFRA-812, R7; the browser cases of the
 * Verification document's k12, dispatch://INFRA-812/artifact/verification-md).
 *
 * Every listed session sits in one of four counted groups - who needs the reader, who is at
 * work, who is idle and who has gone quiet - and the first matching test wins, so a session
 * with an open ask is in `Needs you` however long the registry has been silent about it. A
 * session the registry no longer lists is in no group at all and keeps its row under `Replied,
 * no longer connected`. The filter row above the groups is three multi-selects, each with a text
 * box of its own, and the selection and the broadcast composer are the ones the page had before.
 *
 * The windowed list of six hundred sessions is `agents-window.e2e.ts`, and the time that list
 * takes to paint is `agent-workspace-timing.e2e.ts` (D14). How tall one of those rows is, which
 * is what the six hundred cost in room, is the last row of this file.
 */

/** The sessions these rows read, by id. Each is a UUID, which Dispatch's agent routes require. */
const ASKER = "01a0e812-0c07-7000-8000-000000000001";
const TALKER = "01a0e812-0c07-7000-8000-000000000002";
const SILENT = "01a0e812-0c07-7000-8000-000000000003";
const DOZING = "01a0e812-0c07-7000-8000-000000000004";
const SLEEPER = "01a0e812-0c07-7000-8000-000000000005";
const ENDED = "01a0e812-0c07-7000-8000-000000000006";

/** How long the registry's silence has to run before a session is `Quiet` (`agent-groups.ts`'s
 *  `QUIET_AFTER_MS`), which the rows below seed either side of. */
const QUIET_AFTER_MS = 10 * 60_000;

/** How tall a row of the page is, by the width it is read at. 46 is the figure the spec's
 *  Design records for the row the page drew before this change ("its row is 42 pixels against
 *  today's 46"), and the row this change draws measures the same 46 at a laptop width. 126 is
 *  this change's own reading at 390, where the directory takes a line of its own and the
 *  controls wrap to lines a thumb can hit. The Design records no phone figure; the page before
 *  this change (f15619830b5a) drew this same fixture's row at 146 px there, so 126 is no taller
 *  than today's row, and the bound pins this change's own height rather than today's. */
const ROW_HEIGHT_PX = { laptop: 46, phone: 126 };

/** One element's box on screen, in whole pixels: what a reader sees, rather than a fraction of
 *  a pixel the layout engine carries. */
async function boxOf(locator: Locator): Promise<{ bottom: number; height: number; top: number }> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error("the element is not laid out");
  return {
    bottom: Math.round(box.y + box.height),
    height: Math.round(box.height),
    top: Math.round(box.y),
  };
}

/** A live session for a row to group. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function session(sessionID: string, title: string, silentFor = 5_000): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - silentFor,
    machine_id: `${title.toLowerCase()}-host`,
    roles: [title.toLowerCase()],
    session_id: sessionID,
    title,
  };
}

/** One open question of this session's, on an issue of the reader's, whose next move is theirs:
 *  what puts a session in `Needs you`. */
async function seedOpenAsk(sessionID: string, issue: string, question: string): Promise<void> {
  await createAsk(issue, { question }, { actor: { id: sessionID, kind: "session" }, as: "agent" });
}

/** Dispatch activity of this session's, `agoMs` old: a comment, with the event the page reads
 *  its age from dated back, since the server stamps every event with its own clock. */
async function seedActivity(sessionID: string, issue: string, agoMs: number): Promise<void> {
  const body = `${sessionID} checking in.`;
  await createComment(issue, { body }, { actor: { id: sessionID, kind: "session" }, as: "agent" });
  const events = await getIssueEvents(issue, { limit: 100 });
  const written = events.find(
    (event) => event.type === "comment.created" && event.payload.body === body
  );
  if (written === undefined) throw new Error("the session's comment wrote no event");
  await setEventCreatedAt(written.id, new Date(Date.now() - agoMs).toISOString());
}

/** One unread reply of this session's to the reader, which is what keeps a row for a session
 *  that has since left the registry. */
async function seedUnreadReply(sessionID: string): Promise<void> {
  const asked = await createAgentMessage(sessionID, {
    body: "How far along?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "Done and gone." },
    { id: sessionID, kind: "session" }
  );
}

/** The group each drawn row is listed under, top to bottom. */
function drawnSections(page: Page): Promise<string[]> {
  return shownAgentRows(page).evaluateAll((rows) =>
    rows.map((row) => row.getAttribute("data-agent-section") ?? "")
  );
}

/** The options a filter's panel is offering now, by their names. */
function filterOptions(page: Page): Locator {
  return page.getByRole("option");
}

/** Opens one filter's panel and leaves it open, which is where its text box is. The three sit
 *  in the panel the row's one **Filter** trigger opens, so that is opened first. */
async function openFilter(page: Page, filter: "group" | "machine" | "role"): Promise<void> {
  await openAgentFilters(page);
  const trigger = agentFilter(page, filter);
  if ((await trigger.getAttribute("aria-expanded")) === "false") await trigger.click();
  await expect(page.getByRole("listbox")).toHaveCount(1);
}

// R7. The four groups, and what each of them holds: a session with an open ask waiting on the
// reader, a session whose Dispatch activity is a minute old, the two the registry has heard from
// inside ten minutes with nothing waiting on the reader - one it has never heard from at all and
// one quiet for five - and the one it has not heard from for an hour. Every group is drawn and
// counted whether or not it holds a row, and each of them folds.
// SLOW(>=2s): five sessions seeded with an ask, a dated comment and their own ages, then one
// page load, read group by group.
test("the Agents page lists its sessions in four collapsible counted groups", async ({
  browser,
}) => {
  await setLiveSessions([
    session(ASKER, "Asker"),
    session(TALKER, "Talker"),
    session(SILENT, "Silent"),
    session(DOZING, "Dozing", 5 * 60_000),
    session(SLEEPER, "Sleeper", 60 * 60_000),
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Grouping" });
  await seedOpenAsk(ASKER, issue.key, "Which release do we cut?");
  await seedActivity(TALKER, issue.key, 60_000);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(agentRow(page, ASKER)).toBeVisible();

    // The four groups are there, named and counted, in the order a person reads them.
    for (const [group, name] of [
      ["needs-you", "Needs you"],
      ["active", "Active"],
      ["idle", "Idle"],
      ["quiet", "Quiet"],
    ] as const) {
      await expect(page.getByTestId(`agents-group-name-${group}`)).toHaveText(name);
    }
    await expect(agentGroupToggle(page, "needs-you")).toHaveAccessibleName("Needs you (1)");
    await expect(agentGroupToggle(page, "active")).toHaveAccessibleName("Active (1)");
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (2)");
    await expect(agentGroupToggle(page, "quiet")).toHaveAccessibleName("Quiet (1)");

    // Each session is in the group its own state earns, and the rows are drawn group by group.
    await expect(agentRow(page, ASKER)).toHaveAttribute("data-agent-section", "needs-you");
    await expect(agentRow(page, TALKER)).toHaveAttribute("data-agent-section", "active");
    await expect(agentRow(page, SILENT)).toHaveAttribute("data-agent-section", "idle");
    await expect(agentRow(page, DOZING)).toHaveAttribute("data-agent-section", "idle");
    await expect(agentRow(page, SLEEPER)).toHaveAttribute("data-agent-section", "quiet");
    // Group by group, and inside a group by name, which is the order `orderAgents` leaves rows
    // nobody has pinned, nothing waits on and Dispatch has not heard from.
    await expect(shownAgentRows(page).locator("h2")).toHaveText([
      "Asker",
      "Talker",
      "Dozing",
      "Silent",
      "Sleeper",
    ]);
    expect(await drawnSections(page)).toEqual(["needs-you", "active", "idle", "idle", "quiet"]);

    // A group folds its rows away - the windowed list draws no row of a closed group, so they
    // are in no part of the page - and opens them again.
    await toggleAgentGroup(page, "idle");
    await expect(agentRow(page, SILENT)).toHaveCount(0);
    await expect(agentRow(page, DOZING)).toHaveCount(0);
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (2)");
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Asker", "Talker", "Sleeper"]);
    await toggleAgentGroup(page, "idle");
    await expect(shownAgentRows(page).locator("h2")).toHaveText([
      "Asker",
      "Talker",
      "Dozing",
      "Silent",
      "Sleeper",
    ]);
  } finally {
    await context.close();
  }
});

// R7, and the plan's gap row for the group tests' overlap: the four tests overlap, and the first
// match wins. A session with an open ask that the registry has not heard from for two hours
// satisfies the first test and the last, and the reader is shown what waits on them, so it is in
// `Needs you` and in no other group - `Quiet`, where its silence alone would put it, is empty
// and drawn empty.
// SLOW(>=2s): a session seeded with an open ask and a two-hour silence, then one page load.
test("a session with an open ask is in Needs you however long the registry has been silent", async ({
  browser,
}) => {
  await setLiveSessions([session(ASKER, "Asker", 2 * 60 * 60_000)]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Precedence" });
  await seedOpenAsk(ASKER, issue.key, "Which of the two do we keep?");

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    const row = agentRow(page, ASKER);
    await expect(row).toBeVisible();

    // The silence is real: it is longer than the one that makes a session quiet.
    expect(2 * 60 * 60_000).toBeGreaterThan(QUIET_AFTER_MS);
    await expect(row).toHaveAttribute("data-agent-section", "needs-you");
    await expect(agentGroupToggle(page, "needs-you")).toHaveAccessibleName("Needs you (1)");
    await expect(agentGroupToggle(page, "active")).toHaveAccessibleName("Active (0)");
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (0)");
    await expect(agentGroupToggle(page, "quiet")).toHaveAccessibleName("Quiet (0)");
    // One row on the page, under the one group: nothing is listed twice.
    await expect(row).toHaveCount(1);
    await expect(shownAgentRows(page)).toHaveCount(1);
  } finally {
    await context.close();
  }
});

// R7, and the plan's gap row for the ended sessions: the groups hold the sessions the registry
// lists, so a session that has ended is in none of them. Its replies are still the reader's to
// read, so it keeps a row of its own under `Replied, no longer connected`, below the four
// groups.
// SLOW(>=2s): a reply seeded through Dispatch, the session then taken out of the registry, and
// one page load.
test("an ended session that replied is under Replied, no longer connected and in no group", async ({
  browser,
}) => {
  await setLiveSessions([session(ENDED, "Ended"), session(SILENT, "Silent")]);
  await seedUnreadReply(ENDED);
  // The listener no longer lists it: the session exited after answering.
  await setSessionLive(ENDED, false);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(agentRow(page, SILENT)).toBeVisible();

    // In no group: not drawn, not kept hidden, and counted in none of the four.
    await expect(agentRow(page, ENDED)).toHaveCount(0);
    await expect(agentGroupToggle(page, "needs-you")).toHaveAccessibleName("Needs you (0)");
    await expect(agentGroupToggle(page, "active")).toHaveAccessibleName("Active (0)");
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (1)");
    await expect(agentGroupToggle(page, "quiet")).toHaveAccessibleName("Quiet (0)");

    // And in the section under them, with the reply the reader has not read.
    const ended = page.getByRole("region", { name: "Replied, no longer connected" });
    await expect(ended).toBeVisible();
    await expect(ended.locator(`[data-agent-name="${ENDED}"]`)).toHaveCount(1);
    await expect(ended).toContainText("New reply 1");
    const groups = await page.getByTestId("agents-group-quiet").boundingBox();
    const below = await ended.boundingBox();
    if (groups === null || below === null) throw new Error("the page is not laid out");
    expect(below.y).toBeGreaterThanOrEqual(groups.y + groups.height);
  } finally {
    await context.close();
  }
});

// R7. The filter row is three multi-selects and not three dropdowns: each opens a panel with a
// text box that narrows its own options, takes more than one tick, says on its own control how
// many are picked, and clears them from inside the panel.
// SLOW(>=2s): four sessions seeded, one page load, then three panels opened, typed in, ticked
// and cleared.
test("each filter on the Agents page is a multi-select with a text box, a count and a clear", async ({
  browser,
}) => {
  await setLiveSessions([
    session(SILENT, "Silent"),
    session(DOZING, "Dozing", 5 * 60_000),
    session(SLEEPER, "Sleeper", 60 * 60_000),
    session(TALKER, "Talker", 60 * 60_000),
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(agentRow(page, SILENT)).toBeVisible();

    // The group filter: its own text box narrows the four group names to the one typed.
    await openFilter(page, "group");
    await expect(filterOptions(page)).toHaveText(["Needs you", "Active", "Idle", "Quiet"]);
    const groupSearch = page.getByRole("combobox", { name: "Search groups" });
    await groupSearch.fill("qui");
    await expect(filterOptions(page)).toHaveText(["Quiet"]);
    await groupSearch.fill("");

    // More than one tick, and the control says how many are picked.
    await page.getByRole("option", { exact: true, name: "Idle" }).click();
    await page.getByRole("option", { exact: true, name: "Quiet" }).click();
    await expect(agentFilter(page, "group")).toHaveText("Group · 2");
    await page.keyboard.press("Escape");
    await expect(shownAgentRows(page).locator("h2")).toHaveText([
      "Dozing",
      "Silent",
      "Sleeper",
      "Talker",
    ]);

    // Ticking narrows: with `Idle` alone the two quiet sessions are gone from the list.
    await pickAgentFilter(page, "group", "Quiet");
    await openAgentFilters(page);
    await expect(agentFilter(page, "group")).toHaveText("Group · 1");
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Dozing", "Silent"]);

    // The panel clears what it picked, and the control goes back to its plain name.
    await openFilter(page, "group");
    await page.getByRole("button", { exact: true, name: "Clear group" }).click();
    await expect(agentFilter(page, "group")).toHaveText("Group");
    await page.keyboard.press("Escape");
    await expect(shownAgentRows(page)).toHaveCount(4);

    // The role filter is the same control over the sessions' own roles.
    await openFilter(page, "role");
    const roleSearch = page.getByRole("combobox", { name: "Search roles" });
    await roleSearch.fill("doz");
    await expect(filterOptions(page)).toHaveText(["dozing"]);
    await page.getByRole("option", { exact: true, name: "dozing" }).click();
    await expect(agentFilter(page, "role")).toHaveText("Role · 1");
    await page.keyboard.press("Escape");
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Dozing"]);
    await openFilter(page, "role");
    await page.getByRole("button", { exact: true, name: "Clear role" }).click();
    await page.keyboard.press("Escape");
    await expect(shownAgentRows(page)).toHaveCount(4);
  } finally {
    await context.close();
  }
});

// R7. Forty machines is the page a reader really has, and a list that long is read by typing:
// the machine filter's own text box narrows it to the hosts whose names hold the word, and two
// ticks list the rows of either machine and of no other.
// SLOW(>=2s): forty sessions, each on a machine of its own, then one page load and two picks
// read off the list.
test("the machine filter narrows forty machines to the ones typed, and two picks list the rows of either", async ({
  browser,
}) => {
  // Forty hosts, of which four are relays: a filter that did not narrow would offer all forty.
  const machines = Array.from({ length: 40 }, (_, index) =>
    index % 10 === 0 ? `relay-${index}` : `build-${index}`
  );
  await setLiveSessions(
    machines.map((machine, index) => ({
      capabilities: ["aside"],
      dir: `/workspaces/host-${index}`,
      last_seen: Date.now() - 5_000,
      machine_id: machine,
      roles: [],
      session_id: `01a0e812-0c07-7000-8000-1${String(index).padStart(11, "0")}`,
      title: `Worker ${index}`,
    }))
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (40)");

    await openFilter(page, "machine");
    await expect(filterOptions(page)).toHaveCount(40);
    await page.getByRole("combobox", { name: "Search machines" }).fill("relay");
    await expect(filterOptions(page)).toHaveText(["relay-0", "relay-10", "relay-20", "relay-30"]);

    // Two of them ticked: the list is the rows of either machine, and nothing else.
    await page.getByRole("option", { exact: true, name: "relay-10" }).click();
    await page.getByRole("option", { exact: true, name: "relay-30" }).click();
    await expect(agentFilter(page, "machine")).toHaveText("Machine · 2");
    await page.keyboard.press("Escape");
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Worker 10", "Worker 30"]);
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (2)");
  } finally {
    await context.close();
  }
});

// R7. What a row carries, and what pinning from it does. The row names the session's state, the
// session itself, how long ago Dispatch heard from it and where it is working, and carries the
// pin; the pin is the person's, so the agent is in the rail on every page from that press. The
// row's two shapes, and the long-name row's height against the heights the page before this
// change measured with those same long fixtures, are `agents.e2e.ts`'s own rows at both widths;
// an ordinary row's height against the figure the spec's Design records is the last row here.
// SLOW(>=2s): a session seeded with Dispatch activity, one page load, then a pin waited on
// until it reaches the rail.
test("an agent row carries its state, its name, its age, its directory and its pin, which reaches the rail", async ({
  browser,
}) => {
  await setLiveSessions([session(TALKER, "Talker")]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Row shape" });
  await seedActivity(TALKER, issue.key, 60_000);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    const row = agentRow(page, TALKER);
    await expect(row).toBeVisible();

    await expect(row.getByRole("img", { exact: true, name: "Running" })).toBeVisible();
    await expect(row.getByRole("button", { exact: true, name: "Talker" })).toBeVisible();
    // How long ago Dispatch heard from it, which the row shows as the relative time the whole
    // app shows: the comment this session wrote was dated a minute back.
    const age = row.locator("time");
    await expect(age).toHaveText(/minutes? ago$/);
    const directory = row.locator("[data-agent-dir]");
    await expect(directory).toHaveAttribute("title", "talker-host · /workspaces/talker");
    await expect(directory).toContainText("talker-host");
    const pin = row.getByRole("button", { exact: true, name: "Pin Talker" });
    await expect(pin).toBeVisible();

    // The age and the directory are on one line of the row with the name beside them: a second
    // line for six hundred rows is six hundred lines.
    const boxes = await Promise.all(
      [age, directory, pin].map(async (part) => {
        const box = await part.boundingBox();
        if (box === null) throw new Error("the row is not laid out");
        return box;
      })
    );
    const top = Math.max(...boxes.map((box) => box.y));
    const bottom = Math.min(...boxes.map((box) => box.y + box.height));
    expect(bottom, `the row wrapped: ${JSON.stringify(boxes)}`).toBeGreaterThan(top);

    // Pinning here puts the agent in the rail, where it stays for every page.
    await pin.click();
    await expect(row.getByRole("button", { exact: true, name: "Unpin Talker" })).toBeVisible();
    await expectRailAgents(page, [TALKER]);
  } finally {
    await context.close();
  }
});

// R7. The selection and the broadcast the page had before this change still work over the
// grouped, windowed list: two rows ticked read as two selected, in the group counts and in the
// line above the list, the composer names both, and its pill takes one of them out again.
// SLOW(>=2s): three sessions seeded, one page load, then two ticks and a pill read off the
// composer.
test("ticking two rows reads two selected, the composer names them, and a pill removes one", async ({
  browser,
}) => {
  await setLiveSessions([
    session(SILENT, "Silent"),
    session(DOZING, "Dozing", 5 * 60_000),
    session(SLEEPER, "Sleeper", 60 * 60_000),
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(agentRow(page, SILENT)).toBeVisible();

    await agentRow(page, SILENT).getByRole("checkbox", { name: "Select Silent" }).check();
    await agentRow(page, DOZING).getByRole("checkbox", { name: "Select Dozing" }).check();
    await expect(page.getByText("2 of 3 matching selected")).toBeVisible();
    await expect(agentGroupToggle(page, "idle")).toHaveAccessibleName("Idle (2, 2 selected)");

    const composer = page.getByRole("region", { name: "Broadcast" });
    await expect(composer.getByRole("heading")).toHaveText("Broadcast to 2 of 2 selected");
    const chips = composer.getByRole("list", { name: "Selected agents" }).getByRole("listitem");
    await expect(chips).toHaveCount(2);
    await expect(chips.nth(0)).toContainText("Silent");
    await expect(chips.nth(1)).toContainText("Dozing");

    // The pill takes its own session out of the broadcast, and the counts follow it. The pill
    // keeps the agent's name as its text, so what it removes is its `title`.
    await composer.getByTitle("Remove Dozing from this broadcast").click();
    await expect(chips).toHaveCount(1);
    await expect(composer.getByRole("heading")).toHaveText("Broadcast to 1 of 1 selected");
    await expect(page.getByText("1 of 3 matching selected")).toBeVisible();
    await expect(
      agentRow(page, DOZING).getByRole("checkbox", { name: "Select Dozing" })
    ).not.toBeChecked();
  } finally {
    await context.close();
  }
});

// R7. How tall a row is, which is what six hundred of them cost a reader in room. The spec's
// Design records the pair this reads against: "its row is 42 pixels against today's 46"
// (dispatch://INFRA-812/spec, Design), measured on the prototype at a laptop width. The row the
// page draws here is 46 pixels at 1440 - today's figure exactly, not the prototype's 42 - so it
// is no taller than the row the page drew before this change, with nothing to spare. At 390 the
// directory takes a line of its own and the controls wrap to lines a thumb can hit, which is
// the shape the page drew before this change at that width. The Design records no figure
// there; the page before this change (f15619830b5a) drew this fixture's row at 146 pixels, and
// the 126 pixels below were measured on this change, so the row is 20 pixels shorter than
// today's and a taller row is caught against this change's own height. The long-name row at both widths, against the heights the page before this change
// measured with those same fixtures, is `agents.e2e.ts`'s two-shape row.
// SLOW(>=2s): one session seeded, one page load, then the row and two of its parts measured.
test("an agent row stands at the height recorded for its width @phone", async ({
  browser,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await setLiveSessions([session(TALKER, "Talker")]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // The laptop width the Design's figure was read at; the phone project is 390 pixels already.
    if (!isPhone) await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/agents");
    const row = agentRow(page, TALKER);
    await expect(row).toBeVisible();

    const rowBox = await boxOf(row);
    const name = await boxOf(row.locator("[data-agent-toggle]"));
    const directory = await boxOf(row.locator("[data-agent-dir]"));
    const recorded = isPhone ? ROW_HEIGHT_PX.phone : ROW_HEIGHT_PX.laptop;
    expect(
      rowBox.height,
      `the row is ${rowBox.height}px tall at ${isPhone ? 390 : 1440}px, against the ${recorded}px ` +
        "recorded for that width"
    ).toBeLessThanOrEqual(recorded);
    // A row drawn with nothing in it would pass a ceiling on its own, so the reading has a floor
    // as well: the row carries the state mark, the name, the age, the directory and the pin.
    expect(rowBox.height).toBeGreaterThan(24);

    // The shape each figure belongs to. From `md` the directory shares the name's line, so the
    // row is one line; at a phone it is on a line of its own under the name, which is why the
    // figure there is the taller one.
    if (isPhone) {
      expect(directory.top, "the directory is on a line under the name").toBeGreaterThanOrEqual(
        name.bottom
      );
    } else {
      expect(directory.top, "the directory is on the name's line").toBeLessThan(name.bottom);
    }
  } finally {
    await context.close();
  }
});
