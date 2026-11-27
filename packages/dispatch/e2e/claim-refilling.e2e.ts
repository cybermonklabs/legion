import { holdPageClockWhen, setListsState, setLiveSessions, spendPageTimeUntil } from "./agents";
import { claimIssue, createIssue, createProject, mintAgentToken } from "./api";
import { pickFilterOption } from "./filters";
import { expect, test } from "./test";
import { asUser } from "./users";

// A session's claim lapses when its session is no longer running, and the claim chip and the
// issues table's Claimed by filter - its "Nobody or lapsed" value, which `?unclaimed=1` also
// sets - judge that against the live agent list. A list the Envoy listener refuses as
// refilling (ENVOY_REFILLING) says nothing of whether the holder is gone, so neither view may
// call the claim lapsed from it: the chip says it could not check, and the filter keeps the
// claim as held and says why. A complete list that does not name the holder is the one answer
// that lapses the claim, which the second test shows, picking the value in the column itself.
//
// Dispatch passes the listener's `Retry-After` on with that refusal, and the shared retry policy
// leaves a read that names a wait alone for it before each of its two retries, plus a random
// share of it again (`queryRetryDelay`, `web/src/api/query-client.ts`). At the real listener's 5 s
// a view's first refilling read says so only once up to 20 s of the page's own time are out; the
// row's refilling list names `ROW_RETRY_AFTER_SECONDS` instead (`setListsState`), so up to 4 s.
// agent-list-refilling.e2e.ts pins that Dispatch passes the listener's own 5 on. Those waits are
// page timers: the row installs the page's clock before the page loads and moves it until each
// view says what the row asserts (`spendPageTimeUntil`, `e2e/agents.ts`), rather than waiting
// them out.
//
// Under a refilling list each of the shell's 15 s polls of the registry
// (`AGENT_REGISTRY_POLL_MS`) makes the chip drop its note and the filter its own until the
// retries are refused again. So each view is asserted with the page's clock held still at a
// moment it says the list refills; `holdPageClockWhen` (`e2e/agents.ts`) says why the clock is
// held.

const holder = { session_id: "claim-refill-session", title: "Claim refill holder" };
const REFILLING = "The agent list is refilling. Try again in a moment.";
const ROW_RETRY_AFTER_SECONDS = 1;

async function seedSessionClaim(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed while the list refills" });
  const token = await mintAgentToken("claim-refill", "alice");
  await setLiveSessions([holder]);
  await claimIssue(issue.key, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  return issue.key;
}

// @pr: A refilling agent list must never lapse a claim: the chip and the Claimed by filter both read the live list, and both are wrong if this breaks.
test("a refilling agent list never lapses a session's claim on the chip or in the Claimed by filter @pr", async ({
  browser,
}) => {
  const key = await seedSessionClaim();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto(`/issues/${key}`);
    const chip = page.getByTestId("issue-claim");
    await expect(chip).toHaveAttribute("title", /Claimed /);
    await expect(chip).not.toHaveAttribute("title", /no longer running/);

    await setListsState("refilling", { retryAfterSeconds: ROW_RETRY_AFTER_SECONDS });
    await page.reload();
    // The header's notice comes from the issue's subscribers read and the chip's word from the
    // agent list: two reads, each retried on timers of its own, so the clock moves until each
    // says so.
    const notice = page.getByTestId("issue-header").getByTestId("agent-list-refilling");
    await spendPageTimeUntil(page, () => notice.isVisible(), "the header's refilling notice shows");
    await expect(notice).toHaveText(REFILLING);
    await holdPageClockWhen(
      page,
      async () => ((await chip.getAttribute("title")) ?? "").includes(REFILLING),
      "the chip's title names the refilling list"
    );
    await expect(chip).toHaveAttribute(
      "title",
      /Couldn't check whether that session is still running: The agent list is refilling\. Try again in a moment\.$/
    );
    await expect(chip).not.toHaveAttribute("title", /no longer running/);

    // The clock is the context's, so a page loaded while it is held loads held, and the query
    // client hands each read's answer to the views on a page timer: none would reach the issues
    // page. So the clock runs again before that page loads, as it ran for the issue page.
    await page.clock.resume();
    await page.goto("/projects/CORE/issues?unclaimed=1");
    const unchecked = page.getByTestId("unclaimed-unchecked");
    await holdPageClockWhen(page, () => unchecked.isVisible(), "the filter's note shows");
    await expect(unchecked).toHaveText(
      `Couldn't check which claims have lapsed, so every claim counts as held: ${REFILLING}`
    );
    await expect(
      page.getByRole("table", { name: "Project issues table" }).locator(`[data-issue-row="${key}"]`)
    ).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a complete agent list without the holder lapses its claim on the chip and in the Claimed by filter", async ({
  browser,
}) => {
  const key = await seedSessionClaim();
  await setLiveSessions([]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}`);
    await expect(page.getByTestId("issue-claim")).toHaveAttribute("title", /no longer running/);
    // Picked in the column, not through the link's parameter: the same rule, one place.
    await page.goto("/projects/CORE/issues");
    await pickFilterOption(page, "Claimed by", "Nobody or lapsed");
    await expect(page).toHaveURL(/unclaimed=1/);
    await expect(
      page.getByRole("table", { name: "Project issues table" }).locator(`[data-issue-row="${key}"]`)
    ).toHaveCount(1);
    await expect(page.getByTestId("unclaimed-unchecked")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
