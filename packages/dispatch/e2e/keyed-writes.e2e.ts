import { rm, writeFile } from "node:fs/promises";
import { type AddressInfo, createServer, type Socket } from "node:net";

import { type DispatchEvent, IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

import { expiredRetryNote } from "../web/src/features/writes/keyed-write";
import {
  baseUrl,
  createAsk,
  createIssue,
  createMessage,
  createProject,
  devSignInPath,
  getAsk,
  getIssue,
  getIssueEvents,
  getMyState,
  slowWrites,
  userHeaders,
} from "./api";
import { composer, holdFirstRequest } from "./editor";
import { nextFrames } from "./frames";
import {
  cutFirstAnswerBody,
  dropFirstAnswer,
  keptWrites,
  pendingWrites,
  recordUploadAttempts,
  replayed,
  runClockUntil,
  watchAnswers,
  watchWrites,
} from "./keyed-writes";
import { httpsTarget, plainHttpSkipReason } from "./plain-http-origin";
import { sql } from "./psql";
import {
  entryScriptPath,
  failAssetWhen,
  fetchedChunk,
  holdAsset,
  kindsRequests,
  PENDING_WRITES_CHUNK,
  ranChunk,
  recordTabReloads,
  shellLoads,
  tabReloads,
  WRITE_KINDS_CHUNK,
} from "./shell";
import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  type Route,
  test,
} from "./test";
import { asPlainHttpUser, asUser } from "./users";

// Every write the page makes goes under an `Idempotency-Key` minted for the person's one action,
// and the page tries it again under that key until Dispatch answers it (`ApiClient.send`). These
// rows drive the general path against the real server, for three surfaces that are not a send to
// an agent: a comment, a status change and an ask's answer. In each the first attempt reaches
// Dispatch and its answer is lost on the way back (`route.fetch()`, then `route.abort`), so
// Dispatch did the write and the page does not know it; the page asks again under the same key,
// and Dispatch answers the repeat with the answer it stored (`Idempotency-Replayed: ?1`) rather
// than writing a second time.

const agent = {
  actor: { kind: "session" as const, id: "e2e-keyed-writes" },
  as: "agent" as const,
};

/** How much of an upload's body the shell says has gone up (`Uploading… N%`,
 *  `PendingWritesIndicator`), 0 while it names no upload: the page's own account of the progress
 *  that keeps an attempt's deadline from running out. */
async function uploadedPercent(page: Page): Promise<number> {
  // Its one caller reads this inside `expect.poll`, which reads it again until the share holds.
  const shown = /Uploading… (\d+)%/u.exec((await pendingWrites(page).textContent()) ?? "");
  return shown === null ? 0 : Number(shown[1]);
}

/** The events of `type` on `issue`. */
async function eventsOf(issue: string, type: DispatchEvent["type"]): Promise<DispatchEvent[]> {
  return (await getIssueEvents(issue)).filter((event) => event.type === type);
}

/** The `issue.updated` events on `issue` that set its status to `status`, read with the agent
 *  token, which a person's sign-out leaves working. */
async function statusEvents(issue: string, status: string): Promise<DispatchEvent[]> {
  return (await getIssueEvents(issue, {}, agent)).filter(
    (event) => event.type === "issue.updated" && event.payload.status === status
  );
}

/** Fails every `method` request matching `pattern` at the connection until `end`, as a network
 *  that is down fails it: none of them reaches Dispatch. The route stays through a reload. */
async function outage(page: Page, pattern: string, method: string): Promise<{ end: () => void }> {
  let down = true;
  await page.route(pattern, (route) =>
    down && route.request().method() === method
      ? route.abort("internetdisconnected")
      : route.fallback()
  );
  return {
    end: () => {
      down = false;
    },
  };
}

/** Holds `page`'s status changes to `issue` at the network - each goes out and is never answered -
 *  and lets every other request through; then picks `backlog` and adds `label` behind it, which
 *  waits its turn on the issue's line, never sent. */
async function queueBehindAHeldStatus(page: Page, issue: string, label: string): Promise<void> {
  await page.route(`**/api/v1/issues/${issue}`, (route) => {
    const request = route.request();
    if (request.method() === "PATCH" && request.postData()?.includes('"status"')) return;
    return route.fallback();
  });
  await page.bringToFront();
  await page.getByLabel("Status").selectOption("backlog");
  await expect(pendingWrites(page).getByRole("status")).toHaveText("Saving 1 change…");
  const editLabels = page.getByRole("button", { name: "Edit labels" });
  await editLabels.click();
  const labelSearch = page.getByRole("combobox", { name: "Search or create label" });
  await labelSearch.fill(label);
  await expect(page.getByRole("option", { name: `Create "${label}"` })).toBeVisible();
  await labelSearch.press("Enter");
  await editLabels.click();
  await expect(pendingWrites(page)).toContainText(`Labels of ${issue} — Waiting to send`);
}

// @webkit @firefox: the keyed write path's request failure, which each engine hands the page its
// own way.
// @pr: A keyed write: a comment whose answer is lost lands once, under one key.
test("a comment whose answer is lost lands once, under one key @webkit @firefox @pr", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A comment once" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const first = dropFirstAnswer(page, `**${path}`, "POST");
    const landed = replayed(page, "POST", path);
    // The replay waits `FIRST_BACKOFF_MS` after the lost answer; the page's own clock runs that
    // wait out, so nothing here is spent on it.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Said once");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    expect(await first).toBe(201);
    const answer = await runClockUntil(page, landed);
    expect(answer.status()).toBe(201);
    expect(posts).toHaveLength(2);
    expect(posts[0]).toEqual({
      input: expect.objectContaining({ body: "Said once" }),
      key: expect.any(String),
    });
    expect(posts[1]).toEqual(posts[0]);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    await expect(page.getByText(/^Couldn't send/)).toHaveCount(0);
    await expect(page.locator(`[data-turn="comment:${(await answer.json()).id}"]`)).toHaveCount(1);
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// An answer whose body stops part way down says nothing of what Dispatch did, as a connection
// that never answered says nothing, so it is a transport failure and not a refusal: the write
// asks again under its own key. Here the first attempt reaches Dispatch for real and the comment
// is made; the page is handed that answer's own status and headers over a body that errors as it
// is read (`cutFirstAnswerBody`, which says why no `page.route` can do this). The page tries
// again under the same key, Dispatch replays the answer it kept, and the issue holds one comment
// with nothing refused. A page that let that read's failure out of `send` refused the write
// instead, and its Retry, under a key of its own, would have made the comment a second time.
// SLOW(>=2s): a Conversation page load and two real round trips to Dispatch - the attempt whose
// answer is cut under the page, which Dispatch makes the comment on, and the repeat it replays -
// are what is left; the backoff between them is the page's own clock and costs none.
// @webkit @firefox: the keyed write path's request failure, which each engine hands the page its
// own way - here a body read that fails after a 2xx's headers have arrived, where the browser,
// not Dispatch, decides what the page sees.
test("a comment whose answer the browser cannot read is written once @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A comment the browser cannot read" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const made = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" && new URL(response.url()).pathname === path
    );
    const landed = replayed(page, "POST", path);
    await cutFirstAnswerBody(page, "POST", path);
    // The repeat waits `FIRST_BACKOFF_MS` after the failed read; the page's own clock runs that
    // wait out, so nothing here is spent on it.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Read once");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    expect((await made).status()).toBe(201);
    // The failed read ends the attempt without a verdict, so the write is made again under its
    // own key rather than refused; a page that refused it sends no second attempt at all.
    await runClockUntil(page, expect.poll(() => posts.length, { timeout: 15_000 }).toBe(2));
    const answer = await runClockUntil(page, landed);
    expect(answer.status()).toBe(201);
    expect(posts[0]).toEqual({
      input: expect.objectContaining({ body: "Read once" }),
      key: expect.any(String),
    });
    expect(posts[1]).toEqual(posts[0]);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    await expect(page.getByText(/^Couldn't send/)).toHaveCount(0);
    await expect(composer.getByRole("button", { exact: true, name: "Retry" })).toHaveCount(0);
    await expect(page.locator(`[data-turn="comment:${(await answer.json()).id}"]`)).toHaveCount(1);
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// A key names the person's action only for as long as Dispatch keeps it - `IDEMPOTENCY_KEY_TTL_MS`
// from the first request - and a send key carries the millisecond it was minted, so the page can
// tell. Two writes are held at the network and never answered: a status change the shell's
// indicator names, and a comment the composer holds. The tab's own clock then passes that
// lifetime, which costs this row no real time. Neither write goes out again - an attempt under a
// key Dispatch has forgotten would be a second write of the person's action - and each surface
// says so in the reader's words and offers Retry and Discard, the Retry being a new write.
// SLOW(>=2s): a Conversation page load with its header and composer, and the attempts two
// writes make at the network before their keys run out, are what is left: the jump past the key
// lifetime and the straggler window after it are the page's own clock and cost no real time.
// @webkit @firefox: the keyed write path's storage of a tab's unconfirmed write across a reload,
// read back beside a key the server has forgotten, which each engine times and stores its own way.
test("a write whose key has expired stops and offers a new write @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A key that runs out" });
  const before = (await getIssue(issue.key)).status;
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Every attempt of either write, whether or not the route let it reach Dispatch: the request
    // event fires before the route answers.
    const attempts: string[] = [];
    page.on("request", (request) => {
      const { pathname } = new URL(request.url());
      const write = request.method() === "PATCH" || request.method() === "POST";
      if (write && pathname.startsWith(path)) attempts.push(pathname);
    });
    // Neither write ever reaches Dispatch, so neither has a verdict and both keep trying - until
    // their keys run out. The outages are never ended: this row is about what the page does with
    // a key it can no longer use, not about a network that comes back.
    await outage(page, `**${path}`, "PATCH");
    await outage(page, `**${path}/comments`, "POST");
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    await page.getByLabel("Status").selectOption("todo");
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Older than its key");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();
    const indicator = pendingWrites(page);
    await expect(indicator.getByRole("status")).toHaveText("Saving 1 change…");
    await expect.poll(() => attempts.length).toBeGreaterThanOrEqual(2);

    await page.clock.fastForward(IDEMPOTENCY_KEY_TTL_MS + 1_000);
    // Every attempt the route saw by the time the keys ran out. Each write is between attempts,
    // waiting out a backoff on a page timer the jump fires; the one that follows it is refused
    // before it goes out, so this count is the last.
    const spent = attempts.length;
    await runClockUntil(
      page,
      expect(indicator).toContainText(`Not saved: ${issue.key} set to todo`)
    );

    await expect(indicator).toContainText(expiredRetryNote("saved"));
    await expect(indicator.getByRole("button", { exact: true, name: "Retry" })).toBeVisible();
    await expect(indicator.getByRole("button", { exact: true, name: "Discard" })).toBeVisible();
    await expect(composer).toContainText(expiredRetryNote("delivered"));
    await expect(composer.getByRole("button", { exact: true, name: "Retry" })).toBeVisible();
    await expect(composer.getByRole("button", { name: "Discard draft" })).toBeVisible();
    // Nothing more went out under either key, and nothing of either write was made. Running the
    // page's clock past `MAX_BACKOFF_MS` shows every straggler a timer of its own could still
    // have scheduled, which a few seconds of real time would not, and costs none.
    await page.clock.runFor(60_000);
    expect(attempts).toHaveLength(spent);
    expect((await getIssue(issue.key)).status).toBe(before);
    expect(await statusEvents(issue.key, "todo")).toEqual([]);
    expect(await eventsOf(issue.key, "comment.created")).toEqual([]);
  } finally {
    await alice.close();
  }
});

// The status change is the one kept through a reload: the page keeps an unconfirmed write in the
// tab's storage, asks before the tab is left, and the reloaded page sends it again under the same
// key. The shell's pending-writes indicator names it while it is out, on the page that pressed it
// and on the reloaded one, and clears once Dispatch answers it. The reloaded page is a cold load
// that holds the write before it has drawn, and the indicator is a chunk of its own
// (`web/src/features/writes/write-hosts.tsx`) that such a page has not fetched yet, so the row
// reads the indicator there as well as on the page that pressed it.
// @webkit @firefox: the keyed write path's storage of a tab's unconfirmed write across a reload,
// and the ask before the tab unloads.
test("a status change whose answer is lost survives a reload, lands once, and the pending-writes indicator shows it before and after the reload and clears @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A status once" });
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const patches = watchWrites(page, "PATCH", path);
    const prompts: string[] = [];
    page.on("dialog", (dialog) => {
      prompts.push(dialog.type());
      void dialog.accept();
    });
    // The first PATCH reaches Dispatch, which writes the status; its answer is held until the
    // page that asked is gone.
    const reloaded = Promise.withResolvers<void>();
    // The replay the reloaded page sends is held before it reaches Dispatch until this row has
    // read the indicator on that page: an answer back first would clear the indicator before it
    // could be seen. This route is added before `dropFirstAnswer`'s, and Playwright runs the
    // routes of one request last-added first, so the first PATCH is that one's and every PATCH
    // it lets through comes here.
    const indicatorRead = Promise.withResolvers<void>();
    await page.route(`**${path}`, async (route) => {
      if (route.request().method() !== "PATCH") return route.fallback();
      await indicatorRead.promise;
      return route.fallback();
    });
    const reached = dropFirstAnswer(page, `**${path}`, "PATCH", reloaded.promise);
    const resumed = replayed(page, "PATCH", path);
    await page.goto(`/issues/${issue.key}`);
    await page.getByLabel("Status").selectOption("todo");
    expect(await reached).toBe(200);
    const indicator = pendingWrites(page);
    await expect(indicator.getByRole("status")).toHaveText("Saving 1 change…");

    await page.reload();
    reloaded.resolve();
    await expect(
      indicator.getByRole("status"),
      "the reloaded page does not show the write it sends again"
    ).toHaveText("Saving 1 change…");
    indicatorRead.resolve();
    expect((await resumed).status()).toBe(200);
    expect(prompts).toEqual(["beforeunload"]);
    expect(patches).toHaveLength(2);
    expect(patches[0]).toEqual({ input: { status: "todo" }, key: expect.any(String) });
    expect(patches[1]).toEqual(patches[0]);
    await expect(indicator).toHaveCount(0);
    await expect(page.getByLabel("Status")).toHaveValue("todo");
    expect(await getIssue(issue.key)).toMatchObject({ status: "todo" });
    expect(await statusEvents(issue.key, "todo")).toHaveLength(1);
    // A landed write leaves the tab's queue, so the next load resumes nothing.
    await expect.poll(() => keptWrites(page)).toBe(0);
  } finally {
    await alice.close();
  }
});

// @webkit under the second of the two reasons for an engine tag (`e2e/playwright.config.ts`, the
// comment above `tagged`): not a WebKit behaviour of its own - the keyed write path's request
// failure is the one the tagged rows above exercise in every engine - but the engine this row's
// own failures were in, both of them, two timeouts in 213 CI runs. The tag is what keeps the row
// there now that the projects select rows by tag rather than by file (`webkitTagged`); without it
// the row would run only where its defect was never seen.
test("an ask answer whose answer is lost lands once, under one key @webkit", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "An answer once" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    agent
  );
  const path = `/api/v1/asks/${ask.id}/answer`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const first = dropFirstAnswer(page, `**${path}`, "POST");
    const landed = replayed(page, "POST", path);
    // The replay waits `FIRST_BACKOFF_MS` after the lost answer; the page's own clock runs that
    // wait out, so nothing here is spent on it.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = page.getByRole("region", { name: "Conversation" }).getByTestId(`ask-${ask.id}`);
    await card.getByRole("radio", { name: "Ship" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    expect(await first).toBe(200);
    expect((await runClockUntil(page, landed)).status()).toBe(200);
    expect(posts).toHaveLength(2);
    expect(posts[0]).toEqual({
      input: expect.objectContaining({ selected: ["Ship"] }),
      key: expect.any(String),
    });
    expect(posts[1]).toEqual(posts[0]);
    await expect(card).toContainText(/Answered by alice/);
    expect(await getAsk(ask.id)).toMatchObject({ ask: { state: "answered" } });
    expect(await eventsOf(issue.key, "ask.answered")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// A repeat that reaches Dispatch while the first attempt under its key is still running is
// `409 KEY_IN_FLIGHT` with `Retry-After: 1`, and the page asks again under the same key. The
// first attempt is held in Dispatch by a transaction that holds the issue's row, so the 409 is
// the real server's.
// SLOW(>=2s): The first attempt waits inside Dispatch on a Postgres row lock, which is real time
// no page clock can move. 3 s is the shortest hold that still has the key in flight when the
// page's repeat arrives a second after the abort (`FIRST_BACKOFF_MS`), with room for the press
// and the attempt's flight: 2 s left Firefox's press on the wrong side of it.
// @pr: An idempotent repeat: the repeat of a write still running is answered 409 KEY_IN_FLIGHT, and the page asks again until it lands once.
test("a repeat while the first is still running is answered 409 KEY_IN_FLIGHT, and the page asks again until it lands once @pr", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "In flight" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const answers = watchAnswers(page, "POST", path);
    await page.goto(`/issues/${issue.key}/conversation`);

    // Everything the press needs is ready before the row is held, so the hold is spent on the
    // attempt rather than on getting to it.
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Through a busy key");
    // Hold the issue's row for three seconds, so the comment's first attempt waits in Dispatch.
    const holding = sql(
      "begin",
      `select 1 from issues where key = '${issue.key}' for update`,
      "select pg_sleep(3)",
      "commit"
    );
    await expect
      .poll(() =>
        sql(
          "select count(*) from pg_stat_activity where state = 'active' and query like 'select pg_sleep(3)%'"
        )
      )
      .toBe("1");
    // The first attempt goes to Dispatch and waits there; the page is told it failed at once,
    // so it asks again under the key while the first still runs.
    let first = true;
    const firstAnswer = Promise.withResolvers<number>();
    await page.route(`**${path}`, async (route) => {
      if (route.request().method() !== "POST" || !first) return route.fallback();
      first = false;
      void route.fetch().then((response) => firstAnswer.resolve(response.status()));
      return route.abort("failed");
    });
    const landed = replayed(page, "POST", path);
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    await holding;
    expect(await firstAnswer.promise).toBe(201);
    expect((await landed).status()).toBe(201);
    await expect
      .poll(() => answers)
      .toContainEqual({
        code: "KEY_IN_FLIGHT",
        key: posts[0]?.key,
        replayed: undefined,
        retryAfter: "1",
        status: 409,
      });
    const key = posts[0]?.key;
    expect(key).toEqual(expect.any(String));
    expect(posts.every((post) => post.key === key)).toBe(true);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// A write Dispatch takes longer to finish than the page waits on a silent attempt. Dispatch is
// slow on its own side here (the slow-write hook holds every run of the route once its key is
// claimed), not held in the browser: the page's attempt goes silent, it gives up on it at 15 s
// and asks again under the same key, and the first run, which the page's leaving does not cancel,
// still finishes. Every repeat while it runs is `409 KEY_IN_FLIGHT`; the one after is its answer.
// A run that the leaving cancelled would never finish, since every run is held as long. The two
// waits are kept apart: the hold is 4 s of the server's own time, which no page clock can move,
// and the 15 s the page waits on one silent attempt is its own clock's, driven here.
// SLOW(>=2s): Dispatch's own delay is real time on the server (`writes/_test/slow`); only the
// page's 15 s give-up is driven by `page.clock`. 4 s is the shortest hold that still has the key
// claimed when the repeat arrives.
// @webkit @firefox: the keyed write path's abort of an attempt gone silent, which the engine must
// not take for the server's answer.
test("a comment Dispatch takes 20 s to make lands once from the page, every repeat waiting on the first @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A slow comment" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    await slowWrites("POST", path, 4_000);
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const answers = watchAnswers(page, "POST", path);
    const landed = replayed(page, "POST", path);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Slow on the server");
    const pressed = Date.now();
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    // The give-up is fired only once the first attempt has gone out: the repeat that follows it
    // is a second run rather than a wait on the first until Dispatch has claimed the key, and the
    // repeat comes a real second later (`FIRST_BACKOFF_MS`, on a clock that runs at real speed
    // once it has been jumped), which is far more than the attempt needs to reach Dispatch.
    await expect.poll(() => posts.length).toBe(1);
    await page.clock.fastForward(15_500);
    const answer = await landed;
    expect(answer.status()).toBe(201);
    // Every repeat waits its `Retry-After: 1` in real time, since what it waits on - the first
    // run's hold - is the server's own.
    expect(Date.now() - pressed).toBeGreaterThanOrEqual(3_500);
    await expect
      .poll(() => answers)
      .toContainEqual({
        code: "KEY_IN_FLIGHT",
        key: posts[0]?.key,
        replayed: undefined,
        retryAfter: "1",
        status: 409,
      });
    // Every answer is the first run's verdict or a wait on it: nothing else ran.
    expect(answers.every(({ code, status }) => status === 201 || code === "KEY_IN_FLIGHT")).toBe(
      true
    );
    expect(posts.length).toBeGreaterThanOrEqual(2);
    const key = posts[0]?.key;
    expect(key).toEqual(expect.any(String));
    expect(posts.every((post) => post.key === key)).toBe(true);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    await expect(page.getByText(/^Couldn't send/)).toHaveCount(0);
    await expect(page.locator(`[data-turn="comment:${(await answer.json()).id}"]`)).toHaveCount(1);
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await slowWrites("POST", path, 0);
    await alice.close();
  }
});

// An attempt is cut off only after 15 s with no progress, and a body still going up is progress:
// an upload the page sees take twice that long is one attempt, and lands once. Chromium's
// DevTools protocol paces the page's own upload (`Network.emulateNetworkConditions`), so the body
// goes up at the throttle's rate and the page sees its progress as it goes; no other engine the
// suite runs can be throttled that way, so the row runs on Chromium's projects. The transfer is
// real and the deadline is the page's own: the body is sized to four throttled seconds, and the
// page's clock is jumped over the 15 s between two pieces of it, so the row proves the reset
// without spending thirty seconds on it.
// SLOW(>=2s): The upload must really keep moving while the page's clock crosses
// `KEYED_SEND_ATTEMPT_MS`; a throttled 4 s transfer is the shortest one that still emits progress
// across the fast-forwards.
// @pr: Upload and download: a 30-second upload is one attempt under its key, not a second file.
test("an upload throttled to take 30 s is one attempt and lands once @pr", async ({
  browser,
  browserName,
}) => {
  test.skip(browserName !== "chromium", "only Chromium's DevTools protocol paces a page's upload");
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A slow upload" });
  const path = `/api/v1/issues/${issue.key}/artifacts`;
  const bytes = 1024 * 1024;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // A multipart body is no JSON, so the row keeps each attempt's key alone.
    const keys: (string | undefined)[] = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === path) {
        keys.push(request.headers()["idempotency-key"]);
      }
    });
    const answers = watchAnswers(page, "POST", path);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Artifacts" }).click();
    await page.getByLabel("Upload artifact").setInputFiles({
      buffer: Buffer.alloc(bytes, 7),
      mimeType: "application/octet-stream",
      name: "slow-link.bin",
    });
    const throttle = await alice.newCDPSession(page);
    await throttle.send("Network.emulateNetworkConditions", {
      downloadThroughput: -1,
      latency: 0,
      offline: false,
      uploadThroughput: Math.floor(bytes / 4),
    });
    const created = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === path &&
        response.status() === 201
    );
    const pressed = Date.now();
    const pageStarted = await page.evaluate(() => Date.now());
    await page.getByRole("button", { exact: true, name: "Upload" }).click();
    await expect(pendingWrites(page)).toContainText("Uploading…");
    // Each jump is taken between two pieces of the body, which is why it is safe: every piece
    // resets the attempt's deadline (`arrived`, `web/src/api/client.ts`), so the page's clock
    // passes 15 s twice over while the attempt is never silent for 15 s of it. A page that
    // stopped resetting the deadline cuts the attempt off at the first jump.
    for (const gone of [25, 60]) {
      await expect.poll(() => uploadedPercent(page)).toBeGreaterThanOrEqual(gone);
      await page.clock.fastForward(14_000);
    }
    await created;
    expect(Date.now() - pressed).toBeGreaterThanOrEqual(3_500);
    // The page's own clock crossed the deadline twice while the body kept moving.
    expect((await page.evaluate(() => Date.now())) - pageStarted).toBeGreaterThanOrEqual(28_000);
    await throttle.send("Network.emulateNetworkConditions", {
      downloadThroughput: -1,
      latency: 0,
      offline: false,
      uploadThroughput: -1,
    });

    expect(keys).toEqual([expect.any(String)]);
    await expect.poll(() => answers).toEqual([expect.objectContaining({ status: 201 })]);
    await expect(page.getByTestId("artifact-slow-link-bin")).toContainText("1 version");
  } finally {
    await alice.close();
  }
});

// A repeat of an upload whose first request still runs is answered `409 KEY_IN_FLIGHT`, and each
// engine must hand the page that answer rather than a failed connection, which the page would take
// for a transport failure and back off from. WebKit and Firefox send the whole of
// an upload before they read its answer: an answer Dispatch wrote with the body still coming reached
// them as a reset connection, so Dispatch reads and discards the rest of the body first
// (`drainBeforeAnswer`). The slow-write hook holds the first run 5 s once its key is claimed, and
// the page's clock is jumped past the 15 s it waits on a silent attempt once the whole body is
// up, so it asks again under its key, with the whole 20 MB body each time, while that run still
// holds the key. On Chromium's projects the upload is throttled to take 2 s; no other engine can
// be throttled from Playwright. The page's own `XMLHttpRequest` is the witness of how each
// attempt ended.
// SLOW(>=2s): The 20 MB body is the point - a smaller one fits in the socket buffers and
// Dispatch's 409 would no longer be written while the body is still arriving, which is the drain
// this row proves. Three real uploads of it, plus a 5 s server hold the page's give-up must
// outlast, are the real time left.
// @webkit @firefox: the keyed write path's abort, where the engine sends a whole upload before it
// reads the answer and would otherwise hand the page a reset connection.
test("a 20 MB upload repeated while its first request runs is answered 409 KEY_IN_FLIGHT, never a failed connection, and lands once @webkit @firefox", async ({
  browser,
  browserName,
}) => {
  test.setTimeout(60_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A repeated upload" });
  const path = `/api/v1/issues/${issue.key}/artifacts`;
  const bytes = 20 * 1024 * 1024;
  // The body's file. Its name is the one the test id and the SQL below read, and its type is
  // whatever the browser takes `.bin` for, which is no `text/markdown`, so the page uploads it as
  // the file it was before. It is 20 MB in the row's output directory, which the shard jobs
  // upload whole, so the row removes it once its context is closed.
  const upload = test.info().outputPath("in-flight.bin");
  const alice = await asUser(browser, "alice");
  try {
    await slowWrites("POST", path, 5_000);
    const page = await alice.newPage();
    const attempts = await recordUploadAttempts(page, path);
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Artifacts" }).click();
    // The body goes in as a file on disk, not as a buffer: a buffer is base64-encoded into the
    // protocol message and decoded again in the browser, which cost seconds of both processes'
    // time for 20 MB, where a path hands the browser the file itself.
    await writeFile(upload, Buffer.alloc(bytes, 7));
    await page.getByLabel("Upload artifact").setInputFiles(upload);
    if (browserName === "chromium") {
      const throttle = await alice.newCDPSession(page);
      await throttle.send("Network.emulateNetworkConditions", {
        downloadThroughput: -1,
        latency: 0,
        offline: false,
        uploadThroughput: Math.floor(bytes / 2),
      });
    }
    await page.getByRole("button", { exact: true, name: "Upload" }).click();
    // The whole body is up and nothing is moving either way, so the page's clock can run the
    // attempt's silence out at once; a jump taken while the body still moved would cross a
    // deadline the next piece of it was about to reset.
    await expect(pendingWrites(page)).toContainText("Uploading… 100%");
    await page.clock.fastForward(15_500);
    // Between two attempts the write waits on a page timer - the 409's `Retry-After: 1`, or the
    // backoff after the give-up - and the page's own clock runs that out, so the uploads are the
    // only real time left. The clock is driven only once the attempt before it has ended, never
    // while a body is moving.
    await expect
      .poll(
        async () => {
          const sofar = await attempts();
          if (sofar.at(-1)?.ended !== undefined) await page.clock.runFor(1_200);
          return sofar.at(-1);
        },
        { intervals: [100, 100, 200, 500], timeout: 40_000 }
      )
      .toMatchObject({ ended: "load", status: 201 });

    const [first, ...repeats] = await attempts();
    // The first attempt got no answer while Dispatch held its run: the page gave it up after 15 s
    // of silence (`abort`), or the browser dropped the quiet connection first (`error`) - Firefox
    // closes an HTTP connection that carries no traffic about 6 s after the host's network changes,
    // which a host that adds and removes an interface while the row runs does. Either way it is a
    // transport failure the page asks again after, under the same key.
    expect(["abort", "error"]).toContain(first?.ended);
    expect(first?.status).toBeUndefined();
    // Every repeat was handed Dispatch's answer: none ended as a failed connection.
    expect(repeats.map(({ ended }) => ended)).toEqual(repeats.map(() => "load"));
    const waits = repeats.slice(0, -1);
    expect(waits.length).toBeGreaterThanOrEqual(1);
    for (const wait of waits) {
      expect(wait).toMatchObject({ code: "KEY_IN_FLIGHT", status: 409 });
    }
    const key = first?.key;
    expect(key).toEqual(expect.any(String));
    expect(repeats.every((repeat) => repeat.key === key)).toBe(true);
    await expect(page.getByTestId("artifact-in-flight-bin")).toContainText("1 version");
    expect(
      await sql(
        `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '${issue.key}' and a.name = 'in-flight.bin'`
      )
    ).toBe("1");
  } finally {
    await slowWrites("POST", path, 0);
    await alice.close();
    await rm(upload, { force: true });
  }
});

// A Retry Dispatch's own verdict sends goes under a new key - Dispatch would replay that verdict
// to the old one - and the file the tab kept for the write moves to that key with it, so a
// reload before the new attempt lands makes the upload again from it. The refusal has to reach
// the shell's indicator for the reader to retry the write rather than press Upload afresh, and
// that is what a resumed write does: the Artifacts tab's own control shows its refusal beside
// the file and lets the write go, while a write a reload brought back belongs to the indicator.
// So the first attempt is held at the network and the page reloaded; the attempt the resumed
// write makes is answered as Dispatch answers a multipart body that arrived cut short, which is
// its verdict on that key; and the Retry's attempt is held in turn, so the second reload comes
// before it lands. A page that forgot the file at the Retry resumes nothing after that reload,
// and the issue is left with no file at all.
// SLOW(>=2s): the kept file is only observable across real reloads, so the row pays three loads
// of the issue page - the press, the resume that brings the refusal to the indicator, and the
// resume that makes the upload - with the browser's own "leave this page?" prompt between them.
// @webkit @firefox: whether the engine's IndexedDB gives a kept upload its `File` back across a
// reload - WebKit's stores no `File` at all, which is why this row keeps the bytes with the name
// and type and rebuilds the `File` on the way out (`web/src/features/writes/kept-files.ts`).
test("a retried upload survives a reload with its file @webkit @firefox", async ({ browser }) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A retried upload" });
  const path = `/api/v1/issues/${issue.key}/artifacts`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    // The defect this row guards was a 3.3 ms race - the page named a kept file IndexedDB had
    // not committed yet - and no load this suite can put on a runner reproduces a window that
    // narrow. So the row holds the window open itself: the page's own IndexedDB open of the
    // kept-files database answers its caller a second late, which is longer than every step
    // between the press and the reload. Only the kept-files database is delayed, so the
    // editor's store (`dispatch-pending-edits`) and every other page read are untouched, and
    // both windows the product closes are held open at once: the one at the press
    // (`Writes.run`) and the one at the Retry (`Writes.retry`). The delay is paid by each of
    // the three loads, and by the resumed read after the last of them, which the 10 s bound
    // below absorbs; the row runs in 10 to 12 s (2026-11-22) against the suite's 60 s budget.
    //
    // What the hook needs to be true is not in this file: that `kept-files.ts` opens the
    // database by that name and sets its handler by assignment (`request.onsuccess = …`).
    // Either could change and the hook would then delay nothing, the window would be 3.3 ms
    // again, and this row would pass with the product reverted. So the page counts the opens
    // whose success it actually held back - an open it matched by name but whose handler it
    // never caught counts for nothing - and the row asserts that count at its end, where no
    // wait of its own can close a window: a selector that cannot fail is no guard
    // (`packages/dispatch/README.md`, "End-to-end tests").
    await page.addInitScript(() => {
      let held = 0;
      Object.defineProperty(window, "keptFilesOpensHeld", { get: () => held });
      const open = IDBFactory.prototype.open;
      IDBFactory.prototype.open = function delayed(name: string, version?: number) {
        const request = open.call(this, name, version);
        if (name !== "dispatch-kept-files") return request;
        // The page's handler is taken off the request and called a second after the browser
        // opened the database, so the page waits where it would have gone on. A page that
        // listened for `success` another way would set no handler here, and then this open is
        // not held and is not counted.
        // `Event` here is this file's Dispatch event, so the handler's type is read off
        // `IDBRequest` itself rather than written out.
        let onsuccess: IDBRequest["onsuccess"] = null;
        Object.defineProperty(request, "onsuccess", {
          configurable: true,
          get: () => onsuccess,
          set: (handler: IDBRequest["onsuccess"]) => {
            onsuccess = handler;
          },
        });
        request.addEventListener("success", (event) => {
          setTimeout(() => {
            if (onsuccess === null) return;
            held += 1;
            onsuccess.call(request, event);
          }, 1_000);
        });
        return request;
      };
    });
    /** How many opens of the kept-files database this load has held back a second. */
    const opensHeld = async (): Promise<number> =>
      await page.evaluate(() => Number(Reflect.get(window, "keptFilesOpensHeld") ?? -1));
    // A multipart body is no JSON, so the row keeps each attempt's key alone. The listener is the
    // page's, so it keeps counting across the reloads.
    const keys: (string | undefined)[] = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === path) {
        keys.push(request.headers()["idempotency-key"]);
      }
    });
    // What the route does with the upload's next attempt: hold it, so the reload after it finds
    // the write still out and kept; refuse it as Dispatch refuses a body that arrived cut short,
    // which is its verdict on that key; or let it through to Dispatch.
    let answers: "hold" | "refuse" | "through" = "hold";
    await page.route(`**${path}`, async (route) => {
      if (route.request().method() !== "POST" || answers === "through") return route.fallback();
      if (answers === "hold") return;
      await route.fulfill({
        json: { code: "ARTIFACT_INPUT", error: "invalid multipart body: unexpected EOF" },
        status: 400,
      });
    });
    const openArtifacts = async () => {
      await page.getByRole("tab", { name: "Artifacts" }).click();
    };
    await page.goto(`/issues/${issue.key}`);
    await openArtifacts();
    await page.getByLabel("Upload artifact").setInputFiles({
      buffer: Buffer.from("kept through a retry\n"),
      mimeType: "application/octet-stream",
      name: "once.bin",
    });
    await page.getByRole("button", { exact: true, name: "Upload" }).click();
    const indicator = pendingWrites(page);
    await expect(indicator.getByRole("status")).toHaveText("Saving 1 change…");
    // `keys` is this process's own array, so reading it every 50 ms costs the page nothing: the
    // attempt goes out a second in, once the kept-files database answers, and the default
    // ladder would first see it at 1,850 ms.
    await expect.poll(() => keys.length, { intervals: [50] }).toBe(1);

    // The held attempt leaves the write out and kept, so the reloaded page makes it again on its
    // own - and this time Dispatch's verdict refuses it, in the indicator, with Retry.
    answers = "refuse";
    await page.reload();
    await expect(indicator).toContainText("Not saved: Upload of once.bin");
    await expect.poll(() => keys.length).toBe(2);
    const refused = keys[1];
    expect(refused).toEqual(expect.any(String));

    // The Retry is the person's new write under a key of its own, and the kept file moves to it.
    answers = "hold";
    await indicator.getByRole("button", { exact: true, name: "Retry" }).click();
    // The same for the Retry's attempt, which goes out only once the kept file has moved to its
    // new key.
    await expect.poll(() => keys.length, { intervals: [50] }).toBe(3);
    expect(keys[2]).toEqual(expect.any(String));
    expect(keys[2]).not.toBe(refused);

    // The reload comes before that attempt lands: what goes out after it is the resumed write,
    // made from the file the Retry moved.
    answers = "through";
    await page.reload();
    await openArtifacts();
    await expect(page.getByTestId("artifact-once-bin")).toContainText("1 version", {
      timeout: 10_000,
    });
    await expect(indicator).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
    expect(keys.length).toBeGreaterThanOrEqual(4);
    expect(keys.slice(3).every((key) => key === keys[2])).toBe(true);
    expect(
      await sql(
        `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '${issue.key}' and a.name = 'once.bin'`
      )
    ).toBe("1");
    // Everything above rests on the hook at the head of this row still delaying the page's own
    // opens of the kept-files database, which in turn rests on two facts of `kept-files.ts`
    // this file cannot see: the database's name, and its setting the open handler by
    // assignment. Either could change and the hook would delay nothing, the windows would be
    // 3.3 ms again, and this row would pass with the product reverted. So the last load's
    // count of the opens the hook actually held back - an open matched by name whose handler
    // it never caught counts for nothing - is asserted here, after every step whose timing
    // the row depends on, where reading it costs the row nothing and no wait of its own can
    // close a window the row is meant to reload inside.
    expect(await opensHeld()).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// Signing out discards the person's unsent writes and starts none of them. In each of two tabs of
// alice's, a status change is held at the network, out and never answered, and a label added
// behind it on the same issue waits its turn. Tab 1 signs out and the person confirms the discard:
// tab 1 drops its writes, and tab 2, told over the sign-in channel, drops alice's. Dropping the
// held write must not start the one queued behind it - a page that advanced the line as it
// dropped sent the label at the confirm, and it landed: neither tab makes another write after the
// confirm, and neither issue changes. Both tabs are loaded before either write is pressed, so the
// sign-out comes seconds after the held attempts, well inside the 15 s the page waits on a silent
// one before asking again. The `chromium-plain-http-sign-out` project runs it on a page that is no
// secure context, whose browser has no `navigator.locks`: there the other tab's hold is the lease
// the signing-out tab renews, not its lock.
// SLOW(>=2s): Two tabs are the property - the discard has to reach the tab that did not sign out
// - and each is a real page load with a status change held at the network and a label queued
// behind it. That setup is what is left; the straggler window costs no real time.
// @pr: Sign-in and tokens across two tabs: a confirmed sign-out sends nothing after the confirm, in the tab that signed out and the one that did not.
test("a sign-out confirmed with a write queued behind a held one sends nothing after the confirm, in either tab @pr", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name.startsWith("chromium-plain-http") && httpsTarget,
    plainHttpSkipReason
  );
  await createProject({ key: "KW", name: "Keyed writes" });
  const issues = [
    await createIssue({ project: "KW", title: "Tab one's change" }),
    await createIssue({ project: "KW", title: "Tab two's change" }),
  ];
  const before = await Promise.all(issues.map((issue) => getIssue(issue.key)));
  const alice = testInfo.project.name.startsWith("chromium-plain-http")
    ? await asPlainHttpUser(browser, "alice")
    : await asUser(browser, "alice");
  try {
    let confirmedAt: number | undefined;
    const writes: { at: number; body: string }[] = [];
    const tabs: Page[] = [];
    for (const issue of issues) {
      const page = await alice.newPage();
      const path = `/api/v1/issues/${issue.key}`;
      page.on("dialog", (dialog) => {
        if (dialog.type() === "confirm") confirmedAt ??= Date.now();
        void dialog.accept();
      });
      page.on("request", (request) => {
        if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
          writes.push({ at: Date.now(), body: request.postData() ?? "" });
        }
      });
      await page.clock.install();
      await page.goto(`/issues/${issue.key}`);
      await expect(page.getByRole("button", { name: "Edit labels" })).toBeVisible();
      tabs.push(page);
    }
    for (const [index, page] of tabs.entries()) {
      await queueBehindAHeldStatus(page, issues[index]?.key ?? "", `discarded-${index + 1}`);
    }
    // The labels wait behind the held status changes: none has gone out.
    expect(writes.filter(({ body }) => body.includes('"labels"'))).toEqual([]);

    const [signingOut] = tabs;
    if (signingOut === undefined) throw new Error("no tab was opened");
    await signingOut.bringToFront();
    await signingOut.getByRole("button", { name: "Sign out" }).click();
    await expect(signingOut.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
    expect(confirmedAt).toEqual(expect.any(Number));
    for (const page of tabs) {
      await expect(pendingWrites(page)).toHaveCount(0);
      await expect.poll(() => keptWrites(page)).toBe(0);
    }
    // A write a drop started would have gone out at once; running each tab's own clock past
    // `MAX_BACKOFF_MS` shows every straggler a timer of theirs could still have scheduled, which
    // a few seconds of real time would not, and costs none. `runFor`, not `fastForward`: it
    // fires each repeating timer - the signing-out tab's lease renewal where the browser has no
    // `navigator.locks` - every period it covers.
    for (const page of tabs) await page.clock.runFor(30_000);

    expect(writes.filter(({ at }) => at >= (confirmedAt ?? 0))).toEqual([]);
    const after = await Promise.all(issues.map((issue) => getIssue(issue.key, agent)));
    expect(after.map(({ labels, status }) => ({ labels, status }))).toEqual(
      before.map(({ labels, status }) => ({ labels, status }))
    );
  } finally {
    await alice.close();
  }
});

// A sign-out whose tab goes before it hears how the sign-out ended leaves the other tabs to ask
// Dispatch. Alice's status change in tab 2 is failing at the connection when tab 1 signs her out,
// so tab 2 holds it, `Waiting for a sign-out in another tab`. Tab 1's `POST /auth/logout` goes to
// a server that takes it and never answers, so the page never hears how it ended: a route left
// unanswered is not enough, since Chromium sends a paused request on to Dispatch once its page
// closes. Either nothing else ends alice's session (the sign-out did not land), or the test ends
// it at Dispatch itself (it landed). Then tab 1 closes, with the network back. Tab 2 reads who is
// signed in: still alice, and the change goes out and lands once; nobody, and it is discarded with
// nothing sent. A page where a closing tab said its sign-out was still out let the change go out
// under a session Dispatch had ended, and kept it for the next sign-in. The
// `chromium-plain-http-sign-out` project runs these on the lease a page without `navigator.locks`
// holds by.
for (const landed of [false, true]) {
  test(`a sign-out ${landed ? "that landed" : "that never reached Dispatch"}, its tab closed before the answer, ${landed ? "discards" : "lets go on"} the write another tab held for it`, async ({
    browser,
  }, testInfo) => {
    test.skip(
      testInfo.project.name.startsWith("chromium-plain-http") && httpsTarget,
      plainHttpSkipReason
    );
    await createProject({ key: "KW", name: "Keyed writes" });
    const issue = await createIssue({ project: "KW", title: "Held for a sign-out" });
    const before = await getIssue(issue.key);
    const path = `/api/v1/issues/${issue.key}`;
    const alice = testInfo.project.name.startsWith("chromium-plain-http")
      ? await asPlainHttpUser(browser, "alice")
      : await asUser(browser, "alice");
    // Where the page's sign-out goes in place of Dispatch: it takes the request and never answers.
    const held: Socket[] = [];
    const silent = createServer((socket) => held.push(socket));
    await new Promise<void>((resolve) => silent.listen(0, "127.0.0.1", resolve));
    const { port } = silent.address() as AddressInfo;
    try {
      const holding = await alice.newPage();
      const writes: number[] = [];
      holding.on("request", (request) => {
        if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
          writes.push(Date.now());
        }
      });
      const network = await outage(holding, `**${path}`, "PATCH");
      await holding.clock.install();
      await holding.goto(`/issues/${issue.key}`);
      const signingOut = await alice.newPage();
      signingOut.on("dialog", (dialog) => void dialog.accept());
      const sent = Promise.withResolvers<void>();
      await signingOut.route("**/auth/logout", async (route) => {
        sent.resolve();
        await route.continue({ url: `http://127.0.0.1:${port}/auth/logout` });
      });
      await signingOut.goto("/");

      await holding.bringToFront();
      await holding.getByLabel("Status").selectOption("backlog");
      await expect(pendingWrites(holding).getByRole("status")).toHaveText("Saving 1 change…");
      await signingOut.bringToFront();
      await signingOut.getByRole("button", { name: "Sign out" }).click();
      await sent.promise;
      // The hold is read before alice's session ends: Dispatch closes the holding tab's live
      // socket as soon as it does, and that tab then reads whoami, so its lapse would otherwise
      // overtake the hold and leave the sign-in page, with no indicator to read.
      await expect(pendingWrites(holding)).toContainText("Waiting for a sign-out in another tab");
      if (landed) {
        const ended = await fetch(new URL("/auth/logout", baseUrl), {
          headers: await userHeaders("alice"),
          method: "POST",
        });
        expect(ended.status).toBe(200);
      }
      network.end();
      const closed = Date.now();
      await signingOut.close();

      if (landed) {
        // The hold ends when the signing-out tab's lock is released - or, where the browser has
        // no `navigator.locks`, when the lease it renewed lapses - and the holding tab's own
        // clock runs that wait out rather than this row.
        await runClockUntil(
          holding,
          expect(pendingWrites(holding)).toHaveCount(0, { timeout: 30_000 }),
          1_000
        );
        await expect.poll(() => keptWrites(holding)).toBe(0);
        // A write the discard started would go out at once; running the tab's clock past
        // `MAX_BACKOFF_MS` shows every straggler a timer of its own could still have scheduled,
        // which a few seconds of real time would not, and costs none.
        await holding.clock.runFor(30_000);
        expect(writes.filter((at) => at >= closed)).toEqual([]);
        expect((await getIssue(issue.key, agent)).status).toBe(before.status);
        expect(await statusEvents(issue.key, "backlog")).toEqual([]);
      } else {
        // The hold lifts, the tab reads that alice is still signed in, and the change goes out on
        // the write's next backoff tick (`pause`, `keyed-write.ts`): all three are page timers,
        // which the tab's own clock runs out.
        await runClockUntil(
          holding,
          expect
            .poll(async () => (await getIssue(issue.key, agent)).status, { timeout: 30_000 })
            .toBe("backlog"),
          2_000
        );
        await expect(pendingWrites(holding)).toHaveCount(0);
        await expect.poll(() => keptWrites(holding)).toBe(0);
        expect(await statusEvents(issue.key, "backlog")).toHaveLength(1);
      }
    } finally {
      await alice.close();
      for (const socket of held) socket.destroy();
      silent.close();
    }
  });
}

// A write belongs to the person who pressed it, and dropping a line of writes starts none of them.
// Alice's status change is held at the network, out and never answered, and a label she added
// waits behind it on the same issue; then bob signs in from another tab of the same browser. Both
// of alice's writes are dropped, and nothing goes out after bob's sign-in: neither the label nor
// another attempt of the held change. A page that advanced the line as it dropped it sent the
// label under bob's cookie, which only Dispatch's `409 LOGIN_CHANGED` stopped.
// SLOW(>=2s): Two real tabs are the property - alice's issue page, with a write queued behind a
// held one, and the tab bob signs in from - and their two loads are what is left; the straggler
// window costs no real time.
test("alice's write queued behind her held one sends nothing once bob signs in from another tab", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Alice's line" });
  const before = await getIssue(issue.key);
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const writes: { at: number; body: string }[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
        writes.push({ at: Date.now(), body: request.postData() ?? "" });
      }
    });
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Edit labels" })).toBeVisible();
    await queueBehindAHeldStatus(page, issue.key, "alice-only");

    const other = await alice.newPage();
    const bobSignedIn = Date.now();
    await other.goto(devSignInPath("bob"));
    await expect
      .poll(() =>
        other.evaluate(async () => {
          const whoami: unknown = await (await fetch("/auth/whoami")).json();
          return typeof whoami === "object" && whoami !== null && "login" in whoami
            ? whoami.login
            : null;
        })
      )
      .toBe("bob");
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
    // A write the drop started would go out at once; running alice's tab past `MAX_BACKOFF_MS`
    // on its own clock shows every straggler a timer of hers could still have scheduled, which a
    // few seconds of real time would not, and costs none. Bob's tab needs no clock: the sign-in
    // that drops her writes travels over the sign-in channel, not a timer.
    await page.clock.runFor(30_000);

    expect(writes.filter(({ at }) => at >= bobSignedIn)).toEqual([]);
    const after = await getIssue(issue.key, agent);
    expect({ labels: after.labels, status: after.status }).toEqual({
      labels: before.labels,
      status: before.status,
    });
  } finally {
    await alice.close();
  }
});

// A pin is a write like any other: pressed while the network is down, it is kept in the tab, the
// shell's pending-writes indicator names it, and the reloaded page sends it again once the network
// is back. The Conversation's pin on a turn goes through the pin queue; the issue header's pin is
// a `PUT` of the issue's own state, which a reload resumes as it resumes a `POST` or a `PATCH`.
//
// A pick of the issue's priority pressed in the same outage is kept the same way, and the reload
// is of Settings, a page that imports neither the pin's kind nor the pick's: a reloaded page
// makes a kept write again by its kind, the kinds are a chunk of their own, and the store's
// take-up of what the tab kept waits until that chunk is in (`web/src/features/writes/kinds.ts`,
// `Writes.resume`), so neither write is dropped for want of its kind.
//
// The take-up waits for that chunk only while the tab keeps a write of the person's to take up,
// and a write pressed while it waits joins its line only once the take-up is done, kept in the
// tab from its press. Where the download then fails, the take-up's ask is one a reader waits on
// (`loadWriteKinds`, under `renderChunk`), so the chunk-failure recovery reloads the page from the
// tab's budget (`web/src/features/shell/DeploymentResilience.tsx`), although the writes host's
// prefetch of the same chunk is out beside it; and the reloaded page takes both writes up and
// sends them.
//
// The pending-writes indicator is a chunk of its own, drawn through `chunkComponent`
// (`web/src/features/writes/write-hosts.tsx`), and in the outage its download fails while the
// browser is offline: the writes host's prefetch of it is held on the wire, the browser goes
// offline, the pin and the pick are pressed - the indicator mounts on that held download - and
// the download fails. An offline failure is an outage, not a replaced deployment, so the recovery
// reloads nothing for it (`web/src/features/shell/DeploymentResilience.tsx`,
// `reloadForChunkFailure`), and the indicator asks again once the browser is back online
// (`importWhenOnline`, `web/src/features/shell/chunk-component.tsx`). Until then the page stays
// drawn: the failure reaches no error boundary, where the app's top one (`web/src/main.tsx`)
// would take the whole page down with nothing to bring it back but a reload by hand. Chromium
// downloads the chunk on that second ask, so no recovery reload is spent before the kinds' below.
// SLOW(>=2s): the kept writes are only observable across real reloads, so the row pays five page
// loads - the issue, Settings and its reload, the issue again, its reload and the recovery's
// reload of it - the wait for the chunks the Conversation fetches once it has drawn before the
// browser goes offline, and the resumed writes' round trips.
test("a pin on a turn and a priority pick pressed during an outage survive a reload on another page, and a write pressed while the kinds of write fail to download survives the reload that failure makes", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A pin through a reload" });
  await createMessage(issue.key, { body: "Pinned through the outage" });
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    const patches = watchWrites(page, "PATCH", path);
    const answers = watchAnswers(page, "PATCH", path);
    const network = await outage(page, "**/api/v1/me/issues/*/state", "PUT");
    const pickNetwork = await outage(page, `**${path}`, "PATCH");
    const load = await shellLoads(page);
    const indicatorCode = await holdFirstRequest(page, PENDING_WRITES_CHUNK);
    await page.goto(`/issues/${issue.key}/conversation`);
    const indicator = await indicatorCode.held;
    const pin = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ has: page.getByText("Pinned through the outage", { exact: true }) })
      .getByRole("button", { name: "Pin" });
    // Everything else the page fetches once it has drawn is in before the browser goes offline,
    // so the held download is the one chunk that fails offline: the turn the pin is pressed on,
    // the writes host's other chunk, the kinds of write, whose fetch shows it came down, and the
    // margin and the preview layer, rendered from their own.
    await expect(pin).toBeVisible();
    await fetchedChunk(page, WRITE_KINDS_CHUNK);
    await load().settled();
    await alice.setOffline(true);
    await pin.click();
    await page.getByRole("combobox", { name: `Priority of ${issue.key}` }).selectOption("1");
    // Vite reports the failed download as `vite:preloadError` before the import's rejection
    // reaches the indicator, and React draws what that rejection sets in a task of its own; the
    // wait ends a frame and a task later, so an error boundary the failure reached is drawn by
    // then. The listener is in the page before the download fails.
    const failure = await page.evaluateHandle(() => {
      const { promise, resolve } = Promise.withResolvers<void>();
      window.addEventListener(
        "vite:preloadError",
        () => requestAnimationFrame(() => setTimeout(resolve)),
        { once: true }
      );
      return { handled: promise };
    });
    await indicator.abort("internetdisconnected");
    await failure.evaluate((reading) => reading.handled);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    await expect(page.getByRole("list", { name: "Conversation turns" })).toBeVisible();

    await alice.setOffline(false);
    await expect(pendingWrites(page)).toBeVisible();
    await expect.poll(() => keptWrites(page)).toBe(2);
    await expect.poll(() => patches.length).toBeGreaterThan(0);
    const pick = patches[0];
    expect(pick?.input).toEqual({ priority: 1 });
    expect((await getMyState())[issue.key]?.dismissed ?? []).toEqual([]);

    await page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("link", { name: "Settings" })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
    await page.reload();
    network.end();
    pickNetwork.end();
    // Answered, so made by the reloaded page: every attempt the page before it made was failed at
    // the connection, and that page is gone before the outage ends.
    await expect
      .poll(() => answers.filter((answer) => answer.status === 200).map((answer) => answer.key), {
        message:
          "the reloaded page sends the priority pick again, under the key it was pressed under",
      })
      .toContain(pick?.key);
    await expect.poll(async () => (await getIssue(issue.key)).priority).toBe(1);
    await expect
      .poll(async () => (await getMyState())[issue.key]?.dismissed ?? [], { timeout: 30_000 })
      .toHaveLength(1);
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);

    // The next document's take-up waits for the kinds only while the tab keeps a write of alice's
    // to take up (`Writes.resume`), so she picks a priority on the issue first while its changes
    // fail at the connection, and the tab keeps the pick. The kinds' chunk is then held on the
    // wire for the reloaded document, and failed once a write is pressed behind the take-up, the
    // writes host has asked for the same download as a prefetch, and the margin and the preview
    // layer have rendered from their own chunks: the take-up's ask is then the one download out
    // that a reader waits on. The writes host asks in an effect of the commit in which the page
    // has drawn (`write-hosts.tsx`), the commit whose effect makes the preview layer want its
    // chunk (`RefPreviewLayer.tsx`), so the layer's render (`settled`) shows that the host asked.
    // Every later document gets the chunk.
    await page.goto(`/issues/${issue.key}`);
    const changesNetwork = await outage(page, `**${path}`, "PATCH");
    await page.getByRole("combobox", { name: `Priority of ${issue.key}` }).selectOption("2");
    await expect.poll(() => keptWrites(page)).toBe(1);
    // This document's own fetch of the kinds is in, so the hold below is the reloaded one's.
    await fetchedChunk(page, WRITE_KINDS_CHUNK);
    const kindsCode = await holdFirstRequest(page, WRITE_KINDS_CHUNK);
    await page.reload();
    const reloadedPatches = watchWrites(page, "PATCH", path);
    const kinds = await kindsCode.held;
    await page.getByLabel("Status").selectOption("todo");
    await expect(pendingWrites(page)).toContainText("Waiting to send");
    expect(reloadedPatches, "a write pressed before the take-up is held").toEqual([]);
    await load().settled();
    changesNetwork.end();
    await kinds.abort();
    await expect
      .poll(() => tabReloads(page).catch(() => null), {
        message: "the kinds' failed download spent one of the tab's recovery reloads",
      })
      .toHaveLength(1);
    await expect
      .poll(async () => (await getIssue(issue.key)).status, {
        message: "the reloaded page sends the write the failed take-up held",
      })
      .toBe("todo");
    await expect
      .poll(async () => (await getIssue(issue.key)).priority, {
        message: "the reloaded page takes up the pick the tab kept",
      })
      .toBe(2);
    // One press, one key, however many attempts the reloaded page made of it.
    const statusChanges = reloadedPatches.filter(
      (write) => typeof write.input === "object" && write.input !== null && "status" in write.input
    );
    expect(new Set(statusChanges.map((status) => status.key)).size).toBe(1);
    for (const status of statusChanges) expect(status.input).toEqual({ status: "todo" });
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
  } finally {
    await alice.close();
  }
});

/** Dispatch's verdict on a status change, a refusal: the page shows it with Retry and Discard
 *  rather than trying the write again. */
const STATUS_REFUSED = {
  json: { code: "INVALID_ISSUE", error: "the row refuses this status change" },
  status: 400,
};

/** Holds every status change of the issue at `path` at the network, out and unanswered, and
 *  hands the row the first one held, for it to answer once it chooses. */
async function holdStatusChanges(page: Page, path: string): Promise<{ first: Promise<Route> }> {
  const first = Promise.withResolvers<Route>();
  await page.route(`**${path}`, async (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    first.resolve(route);
  });
  return { first: first.promise };
}

/** Changes the status of the issue `key` to todo on its page, and waits until the indicator says
 *  it saves it. The row holds or fails every status change of the issue at the network first, so
 *  the write stays out and the tab keeps it. */
async function keepAStatusChange(page: Page, key: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await page.getByLabel("Status").selectOption("todo");
  await expect(pendingWrites(page).getByRole("status")).toHaveText("Saving 1 change…");
}

/** Reloads `page` with its first request for the kinds of write held on the wire, and answers
 *  with that held download once the reloaded page says alice is signed in. */
async function reloadWithKindsHeld(page: Page): Promise<Route> {
  const kindsCode = await holdFirstRequest(page, WRITE_KINDS_CHUNK);
  await page.reload();
  const held = await kindsCode.held;
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  return held;
}

/** The line that stands in the pending-writes indicator's place while its chunk cannot load
 *  online (`ChangesNotSent`, `web/src/features/writes/write-hosts.tsx`): a status that counts the
 *  changes not sent. It is no region, so `pendingWrites` still locates the indicator alone. */
function changesNotSent(page: Page): Locator {
  return page.getByRole("status").filter({ hasText: /\bchanges? not sent\b/u });
}

// The pending-writes indicator is a chunk of its own, and its download can fail while the browser
// is online - a deployment that replaced the build the page loaded - with no recovery reload to
// follow: the tab's three reloads are spent here, as a tab whose assets kept failing has spent
// them. The indicator's host is in the status corner, outside the page's error boundary, so the
// failure stops at the indicator's own (`ChunkBoundary`, `web/src/features/writes/write-hosts.tsx`),
// and the rest of the page stays drawn and signed in. The app's top boundary (`web/src/main.tsx`)
// would replace the whole app, `AuthGate` with it, so the tab would stop answering another tab's
// sign-out, and stop showing the sign-in page on a 401, while its write went on. In the
// indicator's place a line says that the write still out is not sent, with a Reload
// (`ChangesNotSent`): the indicator is the one place a reader learns of it, so its failure is
// never silent. The writes host's prefetch of the indicator is held on the wire, the status
// change is pressed and held at the network - the indicator mounts on the held download - and
// the download then fails (`failTheIndicatorOverAHeldWrite`). The host stays mounted, so the
// reader who stays on the page must get the indicator back - its Retry, Discard and Stop trying -
// once the chunk can come down: the next write the reader presses draws it afresh
// (`ChunkBoundary`'s `reset`), which asks for the chunk again, and the row lets that request
// through.
// SLOW(>=2s): two cold page loads, the first only to spend the tab's reloads in its own storage.
test("a pending-writes indicator whose chunk fails to download online with no reload to follow leaves the page drawn and signed in, says in its place that the change is not sent, and the next write draws it", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "An indicator that cannot load" });
  const alice = await asUser(browser, "alice");
  try {
    const { page, spent } = await failTheIndicatorOverAHeldWrite(alice, issue.key);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    await expect(page.getByLabel("Status")).toHaveValue("backlog");
    await expect(
      changesNotSent(page),
      "the failure left the write still out with no word in the indicator's place"
    ).toHaveText("1 change not sent");
    expect(await tabReloads(page), "the failure reloaded the page").toEqual(spent);

    // The status change is still out, held at the network, and its field waits for it, so the
    // reader's next write is the issue's pin. `holdFirstRequest` lets every later request for the
    // indicator's chunk through.
    await page.getByRole("button", { name: "Pin issue" }).click();
    await expect(
      pendingWrites(page),
      "the write pressed after the failure did not draw the indicator again"
    ).toBeVisible();
    await expect(changesNotSent(page)).toHaveCount(0);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    expect(await tabReloads(page), "asking again reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
});

/** Opens the issue `key` in a new page of `alice`'s, with the tab's three recovery reloads spent,
 *  every status change of the issue held at the network, out and never answered unless the row
 *  answers it, and the first download of the pending-writes indicator held on the wire. A status
 *  change pressed then mounts the indicator on the held download, which then fails. React
 *  reports a failure an error boundary caught on the console (`onCaughtError`) in the commit
 *  that draws that boundary, which is what this waits for. Answers with the page, the tab's
 *  reloads as spent, and the status change held. */
async function failTheIndicatorOverAHeldWrite(
  alice: BrowserContext,
  key: string
): Promise<{ page: Page; spent: number[]; statusChange: Route }> {
  const page = await alice.newPage();
  await page.goto("/");
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  const spent = await recordTabReloads(page, [3, 2, 1]);
  const statusChanges = await holdStatusChanges(page, `/api/v1/issues/${key}`);
  const indicatorCode = await holdFirstRequest(page, PENDING_WRITES_CHUNK);
  await page.goto(`/issues/${key}`);
  const indicator = await indicatorCode.held;
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  await page.getByLabel("Status").selectOption("backlog");
  const statusChange = await statusChanges.first;

  const caught = page.waitForEvent("console", (message) =>
    message.text().includes("Failed to fetch dynamically imported module")
  );
  await indicator.abort();
  await caught;
  return { page, spent, statusChange };
}

// The same failure, and then the one write the indicator would show turns refused: Dispatch
// answers the status change held at the network with its verdict, a refusal, whose Retry and
// Discard only the indicator offers. How many writes the indicator shows does not change - it is
// still the one - so its boundary draws it afresh whenever which writes it shows, or where one
// stands, changes (`ChunkBoundary`'s `reset`, `web/src/features/writes/write-hosts.tsx`), not on
// a change of how many they are, which would leave the refusal with neither for the life of the
// page.
// SLOW(>=2s): two cold page loads, the first only to spend the tab's reloads in its own storage.
test("a pending-writes indicator whose chunk fails to download online with no reload to follow draws again when its one write is refused", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A refusal the indicator shows" });
  const alice = await asUser(browser, "alice");
  try {
    const { page, spent, statusChange } = await failTheIndicatorOverAHeldWrite(alice, issue.key);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    await expect(pendingWrites(page)).toHaveCount(0);
    expect(await tabReloads(page), "the failure reloaded the page").toEqual(spent);

    // `holdFirstRequest` lets every later request for the indicator's chunk through.
    await statusChange.fulfill(STATUS_REFUSED);
    const refused = pendingWrites(page);
    await expect(refused, "the refusal did not draw the indicator again").toContainText(
      "Not saved:"
    );
    await expect(refused.getByRole("button", { exact: true, name: "Retry" })).toBeVisible();
    await expect(refused.getByRole("button", { exact: true, name: "Discard" })).toBeVisible();
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    expect(await tabReloads(page), "asking again reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
});

// The same failure, and every later download of the indicator's chunk fails too, as on a page
// whose build a deployment replaced. Dispatch then refuses the one write, which draws the
// indicator afresh (`ChunkBoundary`'s `reset`), and its chunk fails again. The line in its place
// then says that the change is not saved (`ChangesNotSent`), so the reader learns of the refusal,
// which only the indicator names, and has the Reload that brings the indicator back.
// SLOW(>=2s): two cold page loads, the first only to spend the tab's reloads in its own storage.
test("a pending-writes indicator whose chunk fails to download online at every ask, with no reload to follow, says in its place that its one write is not saved once Dispatch refuses it", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A refusal the indicator cannot show" });
  const alice = await asUser(browser, "alice");
  try {
    const { page, spent, statusChange } = await failTheIndicatorOverAHeldWrite(alice, issue.key);
    await expect(changesNotSent(page)).toHaveText("1 change not sent");
    // Playwright tries the last route added first, so this one fails every later request for the
    // indicator's chunk, which `holdFirstRequest` would let through.
    const failed = await failAssetWhen(page, PENDING_WRITES_CHUNK, async () => undefined);

    await statusChange.fulfill(STATUS_REFUSED);
    await expect(
      changesNotSent(page),
      "the line in the indicator's place did not say the refused change is not saved"
    ).toHaveText("1 change not sent — Not saved");
    expect(failed(), "the refusal did not ask for the indicator's chunk again").toBeGreaterThan(0);
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    expect(await tabReloads(page), "the failures reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
});

// The same failure, and then the one write the indicator would show lands: no write is left for
// it to show, so its boundary does not draw it afresh, and no request for its chunk goes out. The
// line that stood in its place goes with that write, since no change is out then. A boundary
// that drew the indicator afresh at every change of which writes it shows, its last write going
// among them, asked for the chunk again for a list that draws nothing, and a download that failed
// then could spend one of the tab's recovery reloads for it (`PendingWritesHost`,
// `web/src/features/writes/write-hosts.tsx`). The next write the reader presses asks again (the
// first of these rows).
// SLOW(>=2s): two cold page loads, the first only to spend the tab's reloads in its own storage.
test("a pending-writes indicator whose chunk failed to download online asks for nothing when its last write lands, and the line in its place goes with that write", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "The indicator's last write" });
  const alice = await asUser(browser, "alice");
  try {
    const { page, spent, statusChange } = await failTheIndicatorOverAHeldWrite(alice, issue.key);
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect(changesNotSent(page)).toHaveText("1 change not sent");
    expect(await keptWrites(page)).toBe(1);
    const asked: string[] = [];
    page.on("request", (request) => {
      if (PENDING_WRITES_CHUNK.test(new URL(request.url()).pathname)) asked.push(request.url());
    });

    await statusChange.continue();
    await expect
      .poll(() => keptWrites(page), { message: "the status change never landed" })
      .toBe(0);
    expect((await getIssue(issue.key, agent)).status).toBe("backlog");
    // Two frames after the landing, the page has drawn what it changed, and run the effects of
    // that commit, which is where a boundary drawn afresh asks for its chunk.
    await nextFrames(page);
    expect(asked, "the last write's landing asked for the indicator's chunk again").toEqual([]);
    await expect(
      changesNotSent(page),
      "the line in the indicator's place stayed after the last write landed"
    ).toHaveCount(0);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    expect(await tabReloads(page), "the failure reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
});

// The same failure, and then the browser goes offline, and Dispatch refuses the one write: the
// row answers the status change held at the network with a refusal while the browser is offline.
// The refusal draws the indicator afresh (`ChunkBoundary`'s `reset`), and its ask for the chunk
// fails offline, so it waits for the network (`importWhenOnline`,
// `web/src/features/shell/chunk-loads.ts`). The line stays in the indicator's place all the while
// and says that the change is not saved (`WhileIndicatorLoads`,
// `web/src/features/writes/write-hosts.tsx`): an outage is when the reader needs it most, and a
// line that went with each later ask of the chunk would leave the corner empty until the network
// came back. Once it is back, the indicator asks again, and draws the refusal with its Retry and
// Discard in the line's place.
// SLOW(>=2s): two cold page loads, the first only to spend the tab's reloads in its own storage.
test("a pending-writes indicator whose chunk failed to download online keeps the line in its place while the browser is offline, says there that its one write is not saved once Dispatch refuses it, and draws once the browser is back online", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "An indicator through an outage" });
  const alice = await asUser(browser, "alice");
  try {
    const { page, spent, statusChange } = await failTheIndicatorOverAHeldWrite(alice, issue.key);
    await expect(changesNotSent(page)).toHaveText("1 change not sent");
    // Each ask for the indicator's chunk that fails because the browser is offline.
    const failedOffline: string[] = [];
    page.on("requestfailed", (request) => {
      if (
        PENDING_WRITES_CHUNK.test(new URL(request.url()).pathname) &&
        request.failure()?.errorText === "net::ERR_INTERNET_DISCONNECTED"
      ) {
        failedOffline.push(request.url());
      }
    });

    await alice.setOffline(true);
    await statusChange.fulfill(STATUS_REFUSED);
    await expect(
      changesNotSent(page),
      "the line left the indicator's place once the refusal asked for its chunk offline"
    ).toHaveText("1 change not sent — Not saved");
    await expect
      .poll(() => failedOffline.length, {
        message: "the refusal did not ask for the indicator's chunk while offline",
      })
      .toBeGreaterThan(0);
    await expect(
      changesNotSent(page),
      "the line left the indicator's place while its ask waited for the network"
    ).toHaveText("1 change not sent — Not saved");
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    expect(await tabReloads(page), "the failures reloaded the page").toEqual(spent);

    // `holdFirstRequest` lets every later request for the indicator's chunk through.
    await alice.setOffline(false);
    const refused = pendingWrites(page);
    await expect(refused, "the network coming back did not draw the indicator").toContainText(
      "Not saved:"
    );
    await expect(refused.getByRole("button", { exact: true, name: "Retry" })).toBeVisible();
    await expect(refused.getByRole("button", { exact: true, name: "Discard" })).toBeVisible();
    await expect(changesNotSent(page)).toHaveCount(0);
    expect(await tabReloads(page), "asking again reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
});

/** Lapses alice's session as Dispatch does when it stops naming her: the sign-in check answers
 *  401 from here on, and the page reads it again on the word another tab of this browser sends
 *  when who is signed in there may have changed (`SignInChannel`, `askSignInAgain`), sent here
 *  from a channel of the page's own, which the app's channel hears as another tab's. */
async function lapse(page: Page): Promise<void> {
  await page.route("**/auth/whoami", (route) =>
    route.fulfill({ json: { error: "not signed in" }, status: 401 })
  );
  await page.evaluate(() => {
    const channel = new BroadcastChannel("dispatch-sign-in");
    channel.postMessage(null);
    channel.close();
  });
  await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
}

/** Whether the tab keeps a write Dispatch refused (`WriteStorage`): the store keeps a write's new
 *  standing before it tells the lists of it (`Writes.#update`), in the same task, so once this
 *  reads the refusal every list has read it too. */
function keepsARefusal(page: Page): Promise<boolean> {
  return page.evaluate(() =>
    Object.entries(window.sessionStorage).some(([name, value]) => {
      if (!name.startsWith("dispatch.write:")) return false;
      const kept: unknown = JSON.parse(value);
      return (
        typeof kept === "object" &&
        kept !== null &&
        "state" in kept &&
        typeof kept.state === "object" &&
        kept.state !== null &&
        "status" in kept.state &&
        kept.state.status === "refused"
      );
    })
  );
}

/** What becomes of the kinds of write in `keptThroughALapse`: their download fails, and the
 *  sign-in page's own ask for them goes through or fails too; or it is held on the wire for good,
 *  so they never arrive. */
type KindsThroughALapse = "fail" | "fail on the sign-in page too" | "never arrive";

/**
 * A session that lapses while the store's take-up of what a reload kept still waits for the
 * kinds of write: the sign-in page lists the write the reload kept by its label at once, read
 * from its own record with no kind of write in (`KeptWritesHost`, `Writes.notTakenUp`), however
 * the kinds fare (`kinds`). Where their download failed online with no reload to follow, the
 * sign-in page asks for them again itself as it draws, for the take-up still waiting; where that
 * ask fails too, the window getting the focus back asks once more.
 */
async function keptThroughALapse(browser: Browser, kinds: KindsThroughALapse): Promise<void> {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Kept through a lapse" });
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    await holdStatusChanges(page, path);
    await keepAStatusChange(page, issue.key);
    const spent = await recordTabReloads(page, [3, 2, 1]);

    const held = await reloadWithKindsHeld(page);
    // The writes host starts both chunks of writes in one effect, and no write of the store's
    // draws the indicator yet, so the indicator's fetch says its prefetch of the kinds has joined
    // the held download, and shares its end.
    await fetchedChunk(page, PENDING_WRITES_CHUNK);
    await expect(pendingWrites(page), "the take-up did not wait for the kinds").toHaveCount(0);
    const isKinds = (request: { url: () => string }) =>
      WRITE_KINDS_CHUNK.test(new URL(request.url()).pathname);
    if (kinds !== "never arrive") {
      // The failure is read in this process, from the browser's report of the request, which a
      // reload it began cannot take away with the document. The two frames after it leave the
      // page the time to handle that failure and ask for any reload it would, and the tab's
      // record of its reloads then says whether it did, as the row's last check reads it again.
      const failed = page.waitForEvent("requestfailed", isKinds);
      await held.abort();
      await failed;
      await nextFrames(page);
      expect(await tabReloads(page), "the kinds' failed download reloaded the page").toEqual(spent);
    }
    expect(await keptWrites(page)).toBe(1);

    // The next request for the kinds is the sign-in page's, as it draws: no press is left to ask
    // for them on this page, and the take-up asks only at a press. A later one goes through.
    let signInPageAsked: Promise<unknown> | undefined;
    if (kinds === "fail") signInPageAsked = page.waitForRequest(isKinds);
    if (kinds === "fail on the sign-in page too") {
      let failNext = true;
      await page.route(WRITE_KINDS_CHUNK, async (route) => {
        if (!failNext) return route.fallback();
        failNext = false;
        await route.abort();
      });
      signInPageAsked = page.waitForEvent("requestfailed", isKinds);
    }
    await lapse(page);
    const kept = page.getByRole("region", { name: "Kept changes" });
    await expect(kept, "the sign-in page lists nothing the reload kept").toBeVisible();
    await expect(kept).toContainText("Kept until you sign in again as alice:");
    await expect(kept).toContainText(`${issue.key} set to `);
    // The sign-in page asks for the kinds itself, for the take-up still waiting; where that ask
    // fails, the reader's coming back to the page asks once more.
    if (signInPageAsked !== undefined) await signInPageAsked;
    if (kinds === "fail on the sign-in page too") {
      const askedAgain = page.waitForRequest(isKinds);
      await page.evaluate(() => window.dispatchEvent(new Event("focus")));
      await askedAgain;
    }
    expect(await tabReloads(page), "the failure reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
}

// A session can lapse while the store's take-up of what a reload kept still waits for the kinds
// of write (`Writes.resume`): it takes them up all the same, for the sign-in that follows, and the
// sign-in page lists them at once from their own records. If the kinds' download failed online
// with no reload to follow - the tab's three reloads are spent here - the shell that asked for
// them is gone, so the sign-in page asks for them again itself (`KeptWritesHost`). Alice's status
// change is held at the network, so it stays out and kept; the page reloads with the kinds' chunk
// held on the wire until the writes host has joined that download, which then fails; her session
// then lapses.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads and the browser's own "leave this page?" prompt between them.
test("a session that lapses while the take-up waits for kinds of write that failed to download lists the write the reload kept on the sign-in page", async ({
  browser,
}) => {
  await keptThroughALapse(browser, "fail");
});

// The same, with the sign-in page's own ask for the kinds failing too: the list draws the write
// the reload kept from its record all the same, and the reader's coming back to the page asks for
// the kinds once more (`KeptWritesHost`), for the take-up still waiting.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads and the browser's own "leave this page?" prompt between them.
test("a session that lapses while the kinds of write fail to download on the sign-in page too lists the write the reload kept at once, and asks for the kinds again when the window gets the focus back", async ({
  browser,
}) => {
  await keptThroughALapse(browser, "fail on the sign-in page too");
});

// The sign-in page's list is the reader's one warning, before they sign in as someone else, of
// the writes that sign-in drops, so it is drawn from what the tab keeps, with no kind of write in:
// a slow download of the kinds - here one held on the wire for good - leaves it as full as a
// failed one does. The store's rows exist only once its take-up has the kinds, and a list drawn
// from them alone was empty for as long as they took.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads and the browser's own "leave this page?" prompt between them.
test("a session that lapses while the kinds of write never arrive lists the write the reload kept on the sign-in page by its label", async ({
  browser,
}) => {
  await keptThroughALapse(browser, "never arrive");
});

/** What becomes of every script but the entry in `keptOnAPageThatLoadsSignedOut`: held on the
 *  wire for good, or failed online. */
type OtherScriptsOnASignedOutLoad = "held" | "fail";

/**
 * A page that loads signed out, as a tab does that reloads after its session lapsed: the store
 * takes up nothing until someone signs in (`Writes.resume`, `web/src/app.tsx`), so the sign-in
 * page lists every write the reload kept from its own record, by its label, under its login. The
 * list is in the entry, with no chunk of its own (`web/src/features/writes/write-hosts.tsx`), so
 * no download can hold it back or keep it away. Beside the issue route's chunk that the reload
 * preloads at boot (`preloadRouteChunk`), the sign-in page itself asks for the kinds of write for
 * a take-up that has not begun (`KeptWritesHost`). Here the `/assets/*.js` route lets only the
 * entry script that `index.html` names through, and every other script fares as `otherScripts`
 * says: held on the wire for good, or failed online with the tab's three recovery reloads spent,
 * so no reload follows. A record kept as refused reads "Not saved" and offers Discard, which drops
 * it from the tab with no kind of write in (`Writes.discardKept`). Alice's change of one issue's
 * status is held at the network, so it stays out and kept; her change of another's is refused,
 * and kept so; then her browser drops her session cookie, and the page reloads.
 */
async function keptOnAPageThatLoadsSignedOut(
  browser: Browser,
  otherScripts: OtherScriptsOnASignedOutLoad
): Promise<void> {
  await createProject({ key: "KW", name: "Keyed writes" });
  const outIssue = await createIssue({ project: "KW", title: "Kept out for a signed-out load" });
  const refusedIssue = await createIssue({ project: "KW", title: "Kept refused, signed out" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    await holdStatusChanges(page, `/api/v1/issues/${outIssue.key}`);
    const refusals = await holdStatusChanges(page, `/api/v1/issues/${refusedIssue.key}`);
    await keepAStatusChange(page, outIssue.key);
    await page.goto(`/issues/${refusedIssue.key}`);
    await page.getByLabel("Status").selectOption("backlog");
    await (await refusals.first).fulfill(STATUS_REFUSED);
    await expect.poll(() => keepsARefusal(page), "the refusal never reached the page").toBe(true);
    await expect.poll(() => keptWrites(page)).toBe(2);
    const spent = await recordTabReloads(page, [3, 2, 1]);

    const entry = await entryScriptPath(baseUrl);
    expect(entry, "index.html did not name an entry script under /assets/").toMatch(
      /^\/assets\/[^/]+\.js$/u
    );
    let entryServed = 0;
    const kindsAsked = Promise.withResolvers<string>();
    // Let only the entry script through; every other script (the preloaded issue route chunk,
    // the kinds of write, and any chunk the kept list might depend on) is held or fails online.
    await page.route("**/assets/*.js", (route) => {
      const path = new URL(route.request().url()).pathname;
      if (path === entry) {
        entryServed += 1;
        return route.continue();
      }
      if (WRITE_KINDS_CHUNK.test(path)) kindsAsked.resolve(path);
      return otherScripts === "fail" ? route.abort() : undefined;
    });
    // The browser drops alice's session cookie, so Dispatch names no one to the reloaded page's
    // first read (`readLanding`, `web/src/api/queries.ts`), as it names no one once a session has
    // lapsed. `domcontentloaded` rather than `load`: holding the preloaded route chunk on the wire
    // holds the `load` event with it.
    await alice.clearCookies();
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
    await kindsAsked.promise;
    expect(entryServed, "the /assets/*.js route did not serve the entry script").toBeGreaterThan(0);
    const kept = page.getByRole("region", { name: "Kept changes" });
    await expect(kept, "the sign-in page lists nothing the reload kept").toBeVisible();
    await expect(kept).toContainText("Kept until you sign in again as alice:");
    const rows = kept.locator("[data-kept-write]");
    const out = rows.filter({ hasText: `${outIssue.key} set to ` });
    const refused = rows.filter({ hasText: `${refusedIssue.key} set to ` });
    await expect(out).toHaveCount(1);
    await expect(out).not.toContainText("Not saved");
    await expect(out.getByRole("button")).toHaveCount(0);
    await expect(refused).toContainText("— Not saved");
    const discard = refused.getByRole("button", { exact: true, name: "Discard" });
    await expect(discard).toBeVisible();

    await discard.click();
    await expect(refused).toHaveCount(0);
    await expect.poll(() => keptWrites(page), "Discard left the refusal in the tab").toBe(1);
    await expect(out).toHaveCount(1);
    await expect(page.getByTestId("error-boundary")).toHaveCount(0);
    expect(await tabReloads(page), "a blocked script download reloaded the page").toEqual(spent);
  } finally {
    await alice.close();
  }
}

// The sign-in page's list is the reader's one warning, before they sign in as someone else, of
// the writes that sign-in drops, so on a page that loads signed out it draws from the entry
// alone: holding every script except the entry - including the preloaded issue route chunk and the
// kinds of write that `KeptWritesHost` asks for - holds back none of it, a refusal's Discard
// included.
// SLOW(>=2s): the kept writes are only observable across a real reload, so the row pays three
// issue page loads and the browser's own "leave this page?" prompt between them.
test("a page that loads signed out while every script but the entry is held on the wire lists every write the reload kept on the sign-in page, a refused one as not saved with the Discard that drops it", async ({
  browser,
}) => {
  await keptOnAPageThatLoadsSignedOut(browser, "held");
});

// The same, with every script except the entry failing online and no reload to follow: the list
// draws all the same, since it has no chunk of its own to fail.
// SLOW(>=2s): the kept writes are only observable across a real reload, so the row pays three
// issue page loads and the browser's own "leave this page?" prompt between them.
test("a page that loads signed out while every script but the entry fails to download online lists every write the reload kept on the sign-in page, a refused one as not saved with the Discard that drops it", async ({
  browser,
}) => {
  await keptOnAPageThatLoadsSignedOut(browser, "fail");
});

/** Whether leaving `page` now asks the reader to confirm: a listener of its `beforeunload`
 *  cancelled the event. It also marks the page as being left for the chunk-failure recovery
 *  (`web/src/features/shell/DeploymentResilience.tsx`), until its next press: a row reads it
 *  last. */
function asksBeforeLeaving(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const leaving = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  });
}

// While the store's take-up of what a reload kept waits for the kinds of write
// (`web/src/features/writes/kinds.ts`, `Writes.resume`), the kept write is in the tab's storage
// alone: the store holds no row of it, so the indicator does not name it yet. A sign-out that
// lands discards it all the same, so Sign out pressed then asks first and names it
// (`Writes.unsent`), as it names every write the store holds; and leaving the page asks for it,
// since closing the tab would lose it (`WriteStorage.load`). Alice's status change is held at the
// network, so it stays out and kept; the page reloads with the kinds' chunk held on the wire, so
// the take-up waits; Cancel leaves her signed in, her write still kept.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads and the browser's own "leave this page?" prompt between them.
test("Sign out pressed while the take-up waits for the kinds of write names the write the reload kept, and leaving asks for it", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Kept through the take-up" });
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const questions: string[] = [];
    page.on("dialog", (dialog) => {
      // Sign out's question is cancelled, so it discards nothing; the reload's own "leave this
      // page?" is accepted.
      if (dialog.type() === "confirm") {
        questions.push(dialog.message());
        void dialog.dismiss();
        return;
      }
      void dialog.accept();
    });
    // Every status change is held at the network: out, never answered, and kept in the tab.
    await holdStatusChanges(page, path);
    await keepAStatusChange(page, issue.key);

    await reloadWithKindsHeld(page);
    // The take-up waits: the store holds no row of the write, which the tab keeps.
    await expect(pendingWrites(page)).toHaveCount(0);
    // The names of what the tab keeps, read from each kept write's own record, leaving out read
    // marks as `keptWrites` does: the issue page's own read mark is a quiet write the tab keeps
    // until Dispatch answers it, never shown and never asked about, and whether it is still out
    // at this moment depends on the runner's speed.
    const labels = await page.evaluate(() =>
      Object.entries(window.sessionStorage).flatMap(([name, value]) => {
        if (!name.startsWith("dispatch.write:")) return [];
        const kept: unknown = JSON.parse(value);
        return typeof kept === "object" &&
          kept !== null &&
          "label" in kept &&
          !("presentation" in kept && kept.presentation === "quiet")
          ? [String(kept.label)]
          : [];
      })
    );
    expect(labels).toEqual([expect.stringMatching(new RegExp(`^${issue.key} set to `, "u"))]);

    await page.getByRole("button", { name: "Sign out" }).click();
    await expect
      .poll(() => questions, {
        message: "Sign out did not ask about the write the reload kept before discarding it",
      })
      .toEqual([`Sign out and discard 1 unsent change?\n\n• ${labels[0]}`]);
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    expect(await keptWrites(page)).toBe(1);
    expect(
      await asksBeforeLeaving(page),
      "leaving the page asks nothing for the write the reload kept"
    ).toBe(true);
  } finally {
    await alice.close();
  }
});

/** A party's mention as a composer writes it (`partyLink`): the Planner's session. A comment
 *  that holds it names a party, so it goes in the mode its parties are told in (`delivery`). */
const NAMES_PLANNER = "[@Planner](dispatch://agent/planner-session)";

/** Presses Send on the issue's Conversation composer with `body`. */
async function sendComment(page: Page, body: string): Promise<void> {
  const form = composer(page).last();
  await form.getByLabel("Comment").fill(body);
  await form.getByRole("button", { exact: true, name: "Send" }).click();
}

/** Whether a body the page posted to an issue's comments carries the comment's text. */
function isCommentBody(body: unknown): body is { body: string } {
  return (
    typeof body === "object" && body !== null && "body" in body && typeof body.body === "string"
  );
}

/** Rewrites the one composer send `page`'s tab keeps as an older build kept it: no answer of
 *  whether each draft's text names a party (`OutgoingDraft.namesParty`), and no request of its
 *  own - none of its attempts had gone out - so the reloaded page makes its attempt from the
 *  record (`composerSend`'s `resume`). */
async function keptAsAnOlderBuild(page: Page): Promise<void> {
  const rewritten = await page.evaluate(() => {
    let count = 0;
    for (const [name, value] of Object.entries(window.sessionStorage)) {
      if (!name.startsWith("dispatch.write:")) continue;
      const kept = JSON.parse(value);
      if (kept.kind !== "composer") continue;
      for (const draft of [
        kept.detail.draft.draft,
        kept.detail.request.draft,
        kept.detail.stoppedSend?.draft,
      ]) {
        if (draft !== undefined) delete draft.namesParty;
      }
      window.sessionStorage.setItem(
        name,
        JSON.stringify({ ...kept, carried: null, request: null, sent: false })
      );
      count += 1;
    }
    return count;
  });
  expect(rewritten, "the tab keeps no one composer send to rewrite").toBe(1);
}

/** Opens `issue`'s Conversation with every comment it posts held at the network, out and never
 *  answered, presses `body` there, and rewrites what the tab keeps of it as an older build kept
 *  it (`keptAsAnOlderBuild`). */
async function keepAnOlderBuildsComment(page: Page, issue: string, body: string): Promise<void> {
  const path = `/api/v1/issues/${issue}/comments`;
  await page.route(`**${path}`, (route) =>
    route.request().method() === "POST" ? undefined : route.fallback()
  );
  const posts = watchWrites(page, "POST", path, isCommentBody);
  await page.goto(`/issues/${issue}/conversation`);
  // On a cold page the press waits behind the download of the kinds of write, and the poll after
  // it has only the default time, so that download is in before the press.
  await fetchedChunk(page, WRITE_KINDS_CHUNK);
  await sendComment(page, body);
  await expect.poll(() => posts.length, "the comment never went out").toBeGreaterThan(0);
  await keptAsAnOlderBuild(page);
}

// A comment an older build kept, with no request of its own - the reload came before
// its first attempt went out - holds no answer of whether its text names a party
// (`OutgoingDraft.namesParty`). Working it out takes the grammar of references, which the entry
// leaves out, so the kinds chunk's composer kind does it (`web/src/features/writes/kinds.ts`),
// and the take-up waits for that chunk (`Writes.resume`). While the chunk is held on the wire the
// comment stays kept in the tab, and nothing of it goes out; once the chunk lands it goes out
// once, in the mode the party its text names is told in. A comment pressed while the chunk is
// held waits behind it with the answer its composer read at the press, and goes out once too.
// Every comment is held at the network, so each stays out, and the row reads what went out.
// SLOW(>=2s): the kept comment is only observable across a real reload, so the row pays two issue
// page loads and the browser's own "leave this page?" prompt between them.
test("a comment an older build kept with no request of its own waits kept while the kinds of write are held, then goes out once in its party's mode, and one pressed meanwhile goes out once behind it", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A comment an older build kept" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    const kept = `Kept by an older build, ${NAMES_PLANNER}.`;
    await keepAnOlderBuildsComment(page, issue.key, kept);

    const held = await reloadWithKindsHeld(page);
    const reloaded = watchWrites(page, "POST", path, isCommentBody);
    expect(await keptWrites(page)).toBe(1);
    const pressed = `Pressed while the kinds load, ${NAMES_PLANNER}.`;
    await sendComment(page, pressed);
    await expect.poll(() => keptWrites(page)).toBe(2);
    expect(reloaded, "a comment went out before the kinds of write were in").toEqual([]);

    await held.continue();
    await expect
      .poll(() => new Set(reloaded.map((post) => post.input.body)), {
        message: "the kept comment and the one pressed meanwhile went out once the kinds landed",
      })
      .toEqual(new Set([kept, pressed]));
    expect(reloaded[0]?.input.body, "the kept comment goes first").toBe(kept);
    for (const body of [kept, pressed]) {
      const sent = reloaded.filter((post) => post.input.body === body);
      expect(new Set(sent.map((post) => post.key)).size, `${body} went out twice`).toBe(1);
      for (const post of sent) {
        expect(post.input).toEqual(expect.objectContaining({ body, delivery: "steer" }));
      }
    }
  } finally {
    await alice.close();
  }
});

// The same comment, with the kinds of write failing online at every ask and the tab's three
// reloads spent, so no reload follows and the take-up waits on. The comment is no row of the
// store's, and a composer's send is shown in its composer, never by the pending-writes indicator
// (`presentation: "composer"`); what the reload kept is read from its records alone
// (`Writes.notTakenUp`). So the line in the indicator's place counts it while the kinds' load has
// failed (`ChangesNotSent`, `Writes.kindsFailed`), it stays kept in the tab, and Sign out names it
// (`Writes.unsent`). Once the chunk can come down, the next press asks for it again - here another
// comment, which the indicator does not show either - and the take-up sends the kept comment in
// its party's mode, and the line goes. Leaving still asks for the comments, which are out.
// SLOW(>=2s): the kept comment is only observable across a real reload, so the row pays two issue
// page loads and the browser's own "leave this page?" prompt between them.
test("a comment an older build kept, while the kinds of write fail online with no reload to follow, is counted on the line in the pending-writes indicator's place and named by Sign out, and the line goes once a later load takes it up", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A comment kept through a failure" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const questions: string[] = [];
    page.on("dialog", (dialog) => {
      // Sign out's question is cancelled, so it discards nothing; the reload's own "leave this
      // page?" is accepted.
      if (dialog.type() === "confirm") {
        questions.push(dialog.message());
        void dialog.dismiss();
        return;
      }
      void dialog.accept();
    });
    const kept = `Kept through a failed download, ${NAMES_PLANNER}.`;
    await keepAnOlderBuildsComment(page, issue.key, kept);
    const spent = await recordTabReloads(page, [3, 2, 1]);
    let failing = true;
    await page.route(WRITE_KINDS_CHUNK, (route) => (failing ? route.abort() : route.fallback()));
    await page.reload();
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    const reloaded = watchWrites(page, "POST", path, isCommentBody);

    await expect(changesNotSent(page)).toHaveText("1 change not sent");
    expect(await tabReloads(page), "the kinds' failed download reloaded the page").toEqual(spent);
    expect(await keptWrites(page)).toBe(1);
    await page.getByRole("button", { name: "Sign out" }).click();
    await expect
      .poll(() => questions, { message: "Sign out did not name the comment the reload kept" })
      .toEqual(["Sign out and discard 1 unsent change?\n\n• Message"]);
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    expect(reloaded, "the kept comment went out with no kinds of write in").toEqual([]);

    failing = false;
    const later = `Pressed once the kinds can load, ${NAMES_PLANNER}.`;
    await sendComment(page, later);
    await expect(changesNotSent(page)).toHaveCount(0);
    await expect
      .poll(() => new Set(reloaded.map((post) => post.input.body)), {
        message: "the later load took the kept comment up and sent it",
      })
      .toEqual(new Set([kept, later]));
    for (const post of reloaded.filter((sent) => sent.input.body === kept)) {
      expect(post.input).toEqual(expect.objectContaining({ body: kept, delivery: "steer" }));
    }
    expect(await asksBeforeLeaving(page), "leaving asks nothing for the comments out").toBe(true);
  } finally {
    await alice.close();
  }
});

// With nothing kept there is no take-up to wait for the kinds of write, so their download
// failing online with no reload to follow leaves no line in the indicator's place: the line
// counts only changes not sent (`ChangesNotSent`).
test("the kinds of write failing online with no reload to follow and nothing kept draw no line in the pending-writes indicator's place", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Nothing kept" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const spent = await recordTabReloads(page, [3, 2, 1]);
    await page.route(WRITE_KINDS_CHUNK, (route) => route.abort());
    const failed = page.waitForEvent("requestfailed", (request) =>
      WRITE_KINDS_CHUNK.test(new URL(request.url()).pathname)
    );
    await page.reload();
    await failed;
    await nextFrames(page);
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    await expect(changesNotSent(page)).toHaveCount(0);
    expect(await tabReloads(page)).toEqual(spent);
  } finally {
    await alice.close();
  }
});

// A sign-out needs no kind of write, so it goes out at once while the store's take-up of what a
// reload kept still waits for the kinds (`Writes.run`): only sending a kept write waits for them.
// Alice has two tabs, a status change of hers held at the network in each, out and kept. The
// first reloads with the kinds' chunk held on the wire for good, so its take-up never ends. She
// signs out there and confirms: the sign-out goes out, her session ends, and the second tab,
// told over the sign-in channel, drops her write and keeps none of it (`Writes.dropLogin`), as the
// first keeps none of the write its reload kept. A sign-out held for the take-up never went out.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays the second
// tab's load, two issue page loads and the browser's own "leave this page?" prompt between them.
test("Sign out pressed while the take-up waits for kinds of write that never arrive goes out at once, and no tab keeps the person's writes", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issues = [
    await createIssue({ project: "KW", title: "Kept through the reload" }),
    await createIssue({ project: "KW", title: "Held in the other tab" }),
  ];
  const alice = await asUser(browser, "alice");
  try {
    const tabs: Page[] = [];
    for (const issue of issues) {
      const page = await alice.newPage();
      // Sign out's question, and the reload's own "leave this page?", are both accepted.
      page.on("dialog", (dialog) => void dialog.accept());
      await holdStatusChanges(page, `/api/v1/issues/${issue.key}`);
      await keepAStatusChange(page, issue.key);
      tabs.push(page);
    }
    const [reloading, other] = tabs;
    if (reloading === undefined || other === undefined) throw new Error("no tab was opened");
    await reloading.bringToFront();
    await reloadWithKindsHeld(reloading);
    // The take-up waits: the store holds no row of the write, which the tab keeps.
    await expect(pendingWrites(reloading)).toHaveCount(0);
    expect(await keptWrites(reloading)).toBe(1);
    expect(await keptWrites(other)).toBe(1);

    const signingOut = reloading.waitForRequest(
      (request) =>
        request.method() === "POST" && new URL(request.url()).pathname === "/auth/logout",
      { timeout: 10_000 }
    );
    await reloading.getByRole("button", { name: "Sign out" }).click();
    await signingOut;
    for (const page of tabs) {
      await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
      await expect.poll(() => keptWrites(page)).toBe(0);
      await expect(page.getByRole("region", { name: "Kept changes" })).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

/** The login (`Dispatch-Login`) of each PATCH of `path` that `page` sends from here on, in the
 *  order they go out. */
function patchLogins(page: Page, path: string): (string | undefined)[] {
  const logins: (string | undefined)[] = [];
  page.on("request", (request) => {
    if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
      logins.push(request.headers()["dispatch-login"]);
    }
  });
  return logins;
}

/**
 * Keeps alice's change of the status of the issue `key` through a reload whose take-up of what
 * the reload kept waits for the rest of the row (`Writes.resume`), and answers with a reader of
 * the login (`Dispatch-Login`) of each status change the page sent from the reload on. Every
 * status change of the issue is held at the network, so hers stays out and the tab keeps it.
 * Every request for the kinds of write is held on the wire from before the reload and never
 * answered (`holdAsset`). A reload starts a new document, which has the kinds only from a request
 * of its own, and the route holds each such request. While a route is set, Playwright turns
 * Chromium's cache off (playwright-core 1.64.1, `_updateProtocolRequestInterceptionForSession`),
 * so no cached copy reaches the page around the route. The document before the reload has its
 * own download of the kinds in first (`fetchedChunk`), so each request the row counts is the
 * reloaded document's. Before it answers, it checks that the take-up still waits: the reloaded
 * page asked for the kinds and no such request has ended, the tab still keeps the write, the
 * store holds no row of it, and no status change went out. So a row that uses it cannot pass
 * because the kinds came in.
 */
async function keptWhileTheKindsNeverArrive(
  page: Page,
  key: string
): Promise<() => (string | undefined)[]> {
  const path = `/api/v1/issues/${key}`;
  // The reload's own "leave this page?" is accepted.
  page.on("dialog", (dialog) => void dialog.accept());
  const logins = patchLogins(page, path);
  await holdStatusChanges(page, path);
  await keepAStatusChange(page, key);
  await fetchedChunk(page, WRITE_KINDS_CHUNK);
  const kinds = kindsRequests(page);
  await holdAsset(page, WRITE_KINDS_CHUNK);
  const sentBefore = logins.length;
  // `domcontentloaded` rather than `load`: a script download held from before the `load` event
  // holds that event with it.
  await page.reload({ waitUntil: "domcontentloaded" });
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  await expect
    .poll(() => kinds.asked.size, { message: "the reloaded page did not ask for the kinds" })
    .toBeGreaterThan(0);
  expect(kinds.ended.size, "a download of the kinds ended").toBe(0);
  expect(await keptWrites(page), "the tab does not keep the write").toBe(1);
  await expect(pendingWrites(page), "the take-up did not wait for the kinds").toHaveCount(0);
  expect(logins.slice(sentBefore), "a status change went out after the reload").toEqual([]);
  return () => logins.slice(sentBefore);
}

/** Signs bob in from another tab of `page`'s browser, which changes the cookie every tab sends,
 *  and waits until `page` draws bob's shell: it heard of the sign-in over the sign-in channel,
 *  and `AuthGate` told its stores of bob before it drew that shell. */
async function bobSignsInFromAnotherTab(page: Page): Promise<void> {
  const other = await page.context().newPage();
  await other.goto(devSignInPath("bob"));
  await expect(page.getByText("Signed in as bob")).toBeVisible();
}

// Another person's sign-in forgets at once what the reload kept of the person before, with no
// kind of write in (`Writes.signedIn`, `#dropKept`). `AuthGate` tells the write store of a
// sign-in as soon as it reads it (`web/src/app.tsx`). A store told only once the kinds were in
// kept alice's write in the tab for as long as the kinds took, here for good. Alice's status
// change is kept through a reload whose take-up waits for kinds that never arrive
// (`keptWhileTheKindsNeverArrive`); then bob signs in from another tab of the same browser. Her
// tab drops her write at once. The page then reloads as bob with nothing held on the wire. Once
// the reloaded page has run the kinds, it has sent nothing, the tab keeps nothing, and the issue's
// status on Dispatch is unchanged.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads, the browser's own "leave this page?" prompt between them, the tab bob signs in from
// and the reload as bob.
test("bob's sign-in from another tab while the take-up waits for kinds of write that never arrive forgets alice's kept write at once, and a reload as bob sends nothing of hers", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Kept through the reload" });
  const before = (await getIssue(issue.key)).status;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sent = await keptWhileTheKindsNeverArrive(page, issue.key);

    await bobSignsInFromAnotherTab(page);
    await expect
      .poll(() => keptWrites(page), {
        message: "bob's sign-in did not forget the write the reload kept for alice",
      })
      .toBe(0);

    await page.unroute(WRITE_KINDS_CHUNK);
    await page.unroute(`**/api/v1/issues/${issue.key}`);
    await page.reload();
    await expect(page.getByText("Signed in as bob")).toBeVisible();
    // The reloaded page has fetched the kinds and run them, so no write it took up waits for them.
    await ranChunk(page, WRITE_KINDS_CHUNK);
    expect(sent(), "the page sent a status change after alice's reload").toEqual([]);
    expect(await keptWrites(page)).toBe(0);
    expect((await getIssue(issue.key, agent)).status).toBe(before);
  } finally {
    await alice.close();
  }
});

// A drop of the person a take-up began for ends that take-up at once, however long the kinds take
// (`#dropKept`, `#endTakeUp`), so a write pressed after the drop joins its line at once.
// `AuthGate` tells the write store of a sign-in as soon as it reads it (`web/src/app.tsx`). A
// store told only once the kinds were in held bob's change behind alice's take-up for good.
// Alice's status change is kept through a reload whose take-up waits for kinds that never arrive
// (`keptWhileTheKindsNeverArrive`); then bob signs in from another tab of the same browser and
// changes the same issue's status in her tab. His change goes out under his login, and no change
// goes out under hers.
// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads, the browser's own "leave this page?" prompt between them, and the tab bob signs in
// from.
test("bob's status change while the take-up waits for kinds of write that never arrive goes out under his login once his sign-in from another tab drops alice's take-up", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Kept through the reload" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sent = await keptWhileTheKindsNeverArrive(page, issue.key);

    await bobSignsInFromAnotherTab(page);
    await page.getByLabel("Status").selectOption("backlog");
    await expect.poll(sent, { message: "bob's status change did not go out" }).toContain("bob");
    expect(
      sent().filter((login) => login !== "bob"),
      "a status change went out under a login other than bob's"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the kept write is only observable across a real reload, so the row pays two issue
// page loads, the browser's own "leave this page?" prompt between them, and the resumed write's
// round trip after it - in Firefox, one of the engines this spec runs in because each keeps a
// tab's storage and asks before it unloads its own way.
// @webkit @firefox: the keyed write path's storage of a tab's unconfirmed write across a reload,
// and the ask before the tab unloads.
test("the issue header's pin pressed during an outage survives a reload and lands once the network is back @webkit @firefox", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A header pin through a reload" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    const puts = watchWrites(page, "PUT", `/api/v1/me/issues/${issue.key}/state`);
    const network = await outage(page, "**/api/v1/me/issues/*/state", "PUT");
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { name: "Pin issue" }).click();
    await expect(pendingWrites(page)).toBeVisible();
    await expect.poll(() => keptWrites(page)).toBe(1);
    expect((await getMyState())[issue.key]?.pinned ?? false).toBe(false);

    await page.reload();
    network.end();
    await expect
      .poll(async () => (await getMyState())[issue.key]?.pinned ?? false, { timeout: 30_000 })
      .toBe(true);
    await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
    // The same press before and after the reload: one body, the header's own write.
    expect(puts.length).toBeGreaterThanOrEqual(2);
    for (const put of puts) expect(put.input).toEqual({ pinned: true });
  } finally {
    await alice.close();
  }
});

// A write belongs to the person who pressed it. Alice changes a status while the network is down;
// then bob signs in from another tab of the same browser, which replaces the cookie every tab
// sends. Alice's write is dropped, in memory and in the tab's storage, and nothing runs as bob: an
// attempt that still reaches Dispatch names alice (`Dispatch-Login`), so Dispatch refuses it
// `409 LOGIN_CHANGED` before anything runs.
// @pr: Sign-in and tokens across two tabs: bob's sign-in drops alice's pending write, and nothing of hers runs as him.
test("a write pending for alice is dropped when bob signs in from another tab, and nothing runs as bob @pr", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Alice's change" });
  const before = (await getIssue(issue.key)).status;
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const answers = watchAnswers(page, "PATCH", path);
    const logins = patchLogins(page, path);
    const network = await outage(page, `**${path}`, "PATCH");
    await keepAStatusChange(page, issue.key);
    await expect.poll(() => keptWrites(page)).toBe(1);

    const other = await alice.newPage();
    await other.goto(devSignInPath("bob"));
    await expect
      .poll(() =>
        other.evaluate(async () => {
          const whoami: unknown = await (await fetch("/auth/whoami")).json();
          return typeof whoami === "object" && whoami !== null && "login" in whoami
            ? whoami.login
            : null;
        })
      )
      .toBe("bob");
    network.end();

    await expect(pendingWrites(page)).toHaveCount(0, { timeout: 45_000 });
    await expect.poll(() => keptWrites(page)).toBe(0);
    // Every attempt named alice, and each that reached Dispatch was refused before it ran.
    expect(logins.every((login) => login === "alice")).toBe(true);
    for (const answer of answers) {
      expect(answer).toMatchObject({ code: "LOGIN_CHANGED", status: 409 });
    }
    expect((await getIssue(issue.key)).status).toBe(before);
    expect(await statusEvents(issue.key, "todo")).toEqual([]);
  } finally {
    await alice.close();
  }
});
