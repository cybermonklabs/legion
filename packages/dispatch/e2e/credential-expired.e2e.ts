import type { FakePendingRequest } from "./fake-broker";
import {
  expiredCredentialRequest,
  expiredMachineLogin,
  setMachineLogins,
  setPendingCredentialRequests,
} from "./fake-broker-helpers";
import { expectInboxCount } from "./inbox-threads";
import { expect, type Locator, type Page, type Route, test } from "./test";
import { withAlice } from "./users";

/**
 * dispatch://INFRA-3921: a credential request whose deadline passed before anybody decided it
 * stays in its approver's Inbox, marked expired, until somebody dismisses it. The broker refuses
 * every decision of such a request, so its row leads to no decision page, and nothing waits on
 * the person, so "Needs you" leaves it out. Each row reads the list Dispatch relays from the fake
 * broker (e2e/fake-broker.ts), which lists the pending rows before the expired ones and drops a
 * row once Dispatch relays its dismissal; the reload after a Dismiss reads that list again, so a
 * row still gone then is gone at the broker and not only on the page.
 *
 * The rows carry no `@pr`: the pull-request lane's real-stack share is full
 * (e2e/__tests__/pull-request-lane.test.ts), so they gate `main` on the full lane, at the merge
 * queue and on the nightly.
 */

/** When the expired rows' deadlines passed, as the broker wrote them: an hour before this run.
 *  Dispatch relays the broker's body unchanged, so the page's `<time>` carries this string. */
const deadline = new Date(Date.now() - 3_600_000).toISOString();
/** When they were asked, an hour before that. */
const askedAt = new Date(Date.now() - 2 * 3_600_000).toISOString();
/** The deadline of each pending row, an hour from now. */
const pendingUntil = new Date(Date.now() + 3_600_000).toISOString();

/** A secret request waiting on alice, as a current broker lists it: marked pending, with the page
 *  on the broker where she decides it. */
const ledgerSecretPage = "https://secrets.internal.example/approve/record-ledger";
const pendingSecret: FakePendingRequest = {
  approve_url: ledgerSecretPage,
  approver: "alice",
  expires_at: pendingUntil,
  identifiers: ["LEDGER_API_KEY"],
  kind: "agent_secret",
  record_id: "record-ledger",
  requested_at: new Date().toISOString(),
  session: { enrollment: null, request: null },
  state: "pending",
};

/** Alice's secret request nobody decided in time. The broker still names its page, which a
 *  decision there would now refuse, so the row must not lead to it. */
const retiredSecretPage = "https://secrets.internal.example/approve/record-retired";
const expiredSecret = expiredCredentialRequest(
  {
    approve_url: retiredSecretPage,
    approver: "alice",
    identifiers: ["RETIRED_API_KEY"],
    kind: "agent_secret",
    record_id: "record-retired",
    requested_at: askedAt,
    session: { enrollment: null, request: null },
  },
  deadline
);

/** A machine login waiting on alice: its row leads to the machine-code page, which also proves
 *  this deployment serves that page, so the expired row's missing link is the row's own. */
const pendingMachine: FakePendingRequest = {
  approver: "alice",
  expires_at: pendingUntil,
  identifiers: ["worker-8.example.com"],
  kind: "launcher_credential",
  record_id: "record-machine-pending",
  requested_at: new Date().toISOString(),
  session: { enrollment: null, request: null },
  state: "pending",
};

/** The machine login `agent-secrets machine login` opened on worker-9 and nobody decided in
 *  time: its row in the list, and the code its machine printed with the record the broker keeps
 *  for it, still pending, which the broker reads as expired since its deadline passed. */
const expiredMachineRecordID = "record-machine-expired";
const expiredMachine = expiredCredentialRequest(
  {
    approver: "alice",
    identifiers: ["worker-9.example.com"],
    kind: "launcher_credential",
    record_id: expiredMachineRecordID,
    requested_at: askedAt,
    session: { enrollment: null, request: null },
  },
  deadline
);
const expiredMachineCode = "QRST-5678";
const expiredLogin = expiredMachineLogin(
  expiredMachineCode,
  {
    approver: "alice",
    enrollment: null,
    identifiers: ["worker-9.example.com"],
    kind: "launcher_credential",
    lifetime_seconds: 900,
    reason: "",
    record_id: expiredMachineRecordID,
    requested_at: askedAt,
    rules_version: "v3",
    service: null,
    session: { enrollment: null, request: null },
  },
  deadline
);

// The pending request beside it is the control: it is a link to its broker page and is the one
// thing "Needs you" counts, before the dismissal and after it, so the expired row's lack of a
// link and of a count is its own and not a list that failed to load.
test("an expired secret request is marked with its deadline, leads to no decision page, is not counted, and Dismiss takes it off for good", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setPendingCredentialRequests([pendingSecret, expiredSecret]);
    await page.goto("/");

    const requests = page.getByRole("region", { name: "Credential requests" });
    const pending = requests.getByRole("listitem").filter({ hasText: "LEDGER_API_KEY" });
    const expired = requests.getByRole("listitem").filter({ hasText: "RETIRED_API_KEY" });

    await expect(pending.getByRole("link")).toHaveAttribute("href", ledgerSecretPage);
    await expect(pending.getByRole("link")).toContainText("Secret request");
    await expect(pending.getByText("Expired", { exact: true })).toHaveCount(0);
    await expect(pending.getByRole("button", { name: /^Dismiss/ })).toHaveCount(0);

    await expect(expired).toContainText("Secret request");
    await expect(expired.getByText("Expired", { exact: true })).toBeVisible();
    const passed = expired.getByText(/^Its deadline passed/);
    await expect(passed).toBeVisible();
    await expect(passed.locator("time")).toHaveAttribute("datetime", deadline);
    // No link of any kind: not the broker's page it still names, nor any other decision page.
    await expect(expired.locator("a")).toHaveCount(0);
    await expect(page.locator(`a[href="${retiredSecretPage}"]`)).toHaveCount(0);
    const dismiss = expired.getByRole("button", {
      name: "Dismiss expired secret request RETIRED_API_KEY",
    });
    await expect(dismiss).toBeVisible();
    await expectInboxCount(page, 1);

    await dismiss.click();
    await expect(expired).toHaveCount(0);

    await page.reload();
    await expect(pending.getByRole("link")).toHaveAttribute("href", ledgerSecretPage);
    await expect(expired).toHaveCount(0);
    await expect(requests.getByRole("listitem")).toHaveCount(1);
    await expect(pending.getByRole("link")).toContainText("Secret request");
    await expect(pending.getByText("Expired", { exact: true })).toHaveCount(0);
    await expectInboxCount(page, 1);
  });
});

// A pending machine login's row leads to the machine-code page, where its code decides it; an
// expired one's leads nowhere, since the broker refuses its decision there too.
test("an expired machine login is marked with its deadline, leads to no machine-code page, and Dismiss takes it off for good", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setPendingCredentialRequests([pendingMachine, expiredMachine]);
    await page.goto("/");

    const requests = page.getByRole("region", { name: "Credential requests" });
    const pending = requests.getByRole("listitem").filter({ hasText: "worker-8.example.com" });
    const expired = requests.getByRole("listitem").filter({ hasText: "worker-9.example.com" });

    await expect(pending.getByRole("link")).toHaveAttribute("href", "/credentials/machine");
    await expect(pending.getByText("Expired", { exact: true })).toHaveCount(0);

    await expect(expired.getByText("Machine login", { exact: true })).toBeVisible();
    await expect(expired.getByText("Expired", { exact: true })).toBeVisible();
    const passed = expired.getByText(/^Its deadline passed/);
    await expect(passed).toBeVisible();
    await expect(passed.locator("time")).toHaveAttribute("datetime", deadline);
    await expect(expired.locator("a")).toHaveCount(0);
    await expect(requests.locator('a[href="/credentials/machine"]')).toHaveCount(1);
    await expectInboxCount(page, 1);

    await expired
      .getByRole("button", { name: "Dismiss expired machine login worker-9.example.com" })
      .click();
    await expect(expired).toHaveCount(0);

    await page.reload();
    await expect(pending.getByRole("link")).toHaveAttribute("href", "/credentials/machine");
    await expect(expired).toHaveCount(0);
    await expect(requests.getByRole("listitem")).toHaveCount(1);
    await expectInboxCount(page, 1);
  });
});

// The code still selects the login, so a person who typed it before reading the Inbox learns
// what happened to it: its facts, and one line saying it expired, in place of the two buttons
// whose decision the broker would refuse.
test("the machine-login page shows an expired login's facts and that it expired, with no Approve or Deny", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setMachineLogins([expiredLogin]);

    await page.goto("/credentials/machine");
    await page.getByLabel("Code shown on the machine").fill(expiredMachineCode);
    await page.getByRole("button", { name: "Look up" }).click();

    await expect(page.getByText("This login expired before anyone decided it.")).toBeVisible();
    await expect(page.getByText("worker-9.example.com", { exact: true })).toBeVisible();
    await expect(page.getByText("Machine login", { exact: true })).toBeVisible();
    await expect(page.getByText("15 minutes", { exact: true })).toBeVisible();
    await expect(page.locator(`time[datetime="${deadline}"]`)).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Deny" })).toHaveCount(0);
    await expect(page.getByText(/^Approving lets/)).toHaveCount(0);
  });
});

/** What a row holds of the reads the page makes of one route: `hold` keeps every later GET the
 *  route names unanswered, and `out` counts how many are kept. A kept GET is never answered, so
 *  the page gives it up at its own 30 s deadline. */
interface BoundReads {
  readonly hold: () => void;
  readonly out: () => number;
}

/** Routes the page's GETs that `matches` names, which go through until `hold` is called. */
async function routeBoundReads(page: Page, matches: (url: URL) => boolean): Promise<BoundReads> {
  let holding = false;
  let kept = 0;
  await page.route(matches, async (route: Route) => {
    if (!holding || route.request().method() !== "GET") return route.fallback();
    kept += 1;
    return undefined;
  });
  return {
    hold: () => {
      holding = true;
    },
    out: () => kept,
  };
}

/** A second expired secret request of alice's, beside `expiredSecret`. */
const lapsedSecret = expiredCredentialRequest(
  {
    approve_url: "https://secrets.internal.example/approve/record-lapsed",
    approver: "alice",
    identifiers: ["LAPSED_API_KEY"],
    kind: "agent_secret",
    record_id: "record-lapsed",
    requested_at: askedAt,
    session: { enrollment: null, request: null },
  },
  deadline
);

/** What an expired row says once the 30 s bound ran out on the list its Dismiss read again. */
const LIST_NOT_READ =
  "Couldn't load credential requests. Dispatch did not answer within 30 seconds.";

// The tester's red row of round 21 (read-bound.ts:40-47: a read that has not settled is said "for
// the latest read" alone, and "forgets at once why any earlier read had not settled"; the
// architect's ruling of 2026-11-26T04:19Z). Every expired request's row owns a `useReadBound`
// (CredentialRequestsSection.tsx:107), though every Dismiss reads the one list again, so a Dismiss
// of the second request begins a new read of the list and takes nothing away from the line the
// first Dismiss left for the read before it. At c3b536cc the first row still says "Couldn't load
// credential requests. Dispatch did not answer within 30 seconds." for the whole 15 s the row
// waits after the second dismissal landed. Measured in Chromium and Firefox, twice each.
// SLOW(>=2s): two expired requests seeded, one Inbox page open, two dismissals written and the
// page's clock moved once past the 30 s bound.
// @firefox: the timers of a read's own deadline and of the bound a held control waits on, which
// each engine runs.
test("a Dismiss of a second expired request takes away the line the first Dismiss left for the list read that had not settled @firefox", async ({
  page,
}) => {
  const step = (text: string) => `Dismiss of two expired credential requests: ${text}`;
  await setPendingCredentialRequests([expiredSecret, lapsedSecret]);
  const listReads = await routeBoundReads(
    page,
    (url) => url.pathname === "/api/v1/credential-requests"
  );
  await page.clock.install();
  await page.goto("/");
  const requests = page.getByRole("region", { name: "Credential requests" });
  const first = requests.getByRole("listitem").filter({ hasText: "RETIRED_API_KEY" });
  const second = requests.getByRole("listitem").filter({ hasText: "LAPSED_API_KEY" });
  const dismissOf = (row: Locator) => row.getByRole("button", { name: /^Dismiss expired/ });
  await expect(
    dismissOf(first),
    step("the first expired request is listed with its Dismiss.")
  ).toBeVisible();
  await expect(
    dismissOf(second),
    step("the second expired request is listed with its Dismiss.")
  ).toBeVisible();
  const unreadLine = (row: Locator) =>
    row.locator("p").filter({ hasText: /did not answer within 30 seconds/ });
  const dismissals = (id: string) =>
    page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/v1/credential-requests/${id}/dismiss`
    );

  listReads.hold();
  const firstAnswer = dismissals("record-retired");
  await dismissOf(first).click();
  expect((await firstAnswer).ok(), step("the first dismissal lands at Dispatch.")).toBe(true);
  await expect
    .poll(listReads.out, { message: step("the list read the first dismissal starts is out.") })
    .toBeGreaterThan(0);
  await page.clock.fastForward(30_000);
  await expect(
    unreadLine(first),
    step("at the 30 s bound the first request's row says why the list is not read.")
  ).toHaveText([LIST_NOT_READ]);

  const secondAnswer = dismissals("record-lapsed");
  await dismissOf(second).click();
  expect((await secondAnswer).ok(), step("the second dismissal lands at Dispatch.")).toBe(true);
  await expect(
    unreadLine(first),
    step(
      "the first request's row loses its line as the second dismissal lands and a new read of the list begins."
    )
  ).toHaveCount(0);
});
