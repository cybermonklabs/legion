import type { FakePendingRequest } from "./fake-broker";
import {
  expiredCredentialRequest,
  expiredMachineLogin,
  setMachineLogins,
  setPendingCredentialRequests,
} from "./fake-broker-helpers";
import { expectInboxCount } from "./inbox-threads";
import { type Browser, expect, type Page, rowPerson, test } from "./test";
import { asUser, withAlice } from "./users";

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

/** A secret request waiting on `approver`, as a current broker lists it: marked pending, with
 *  the page on the broker where they decide it. */
const ledgerSecretPage = "https://secrets.internal.example/approve/record-ledger";
function pendingSecret(approver: string): FakePendingRequest {
  return {
    approve_url: ledgerSecretPage,
    approver,
    expires_at: pendingUntil,
    identifiers: ["LEDGER_API_KEY"],
    kind: "agent_secret",
    record_id: "record-ledger",
    requested_at: new Date().toISOString(),
    session: { enrollment: null, request: null },
    state: "pending",
  };
}

/** `approver`'s secret request nobody decided in time. The broker still names its page, which a
 *  decision there would now refuse, so the row must not lead to it. */
const retiredSecretPage = "https://secrets.internal.example/approve/record-retired";
function expiredSecret(approver: string): FakePendingRequest {
  return expiredCredentialRequest(
    {
      approve_url: retiredSecretPage,
      approver,
      identifiers: ["RETIRED_API_KEY"],
      kind: "agent_secret",
      record_id: "record-retired",
      requested_at: askedAt,
      session: { enrollment: null, request: null },
    },
    deadline
  );
}

/** A machine login waiting on `approver`: its row leads to the machine-code page, which also
 *  proves this deployment serves that page, so the expired row's missing link is the row's own. */
function pendingMachine(approver: string): FakePendingRequest {
  return {
    approver,
    expires_at: pendingUntil,
    identifiers: ["worker-8.example.com"],
    kind: "launcher_credential",
    record_id: "record-machine-pending",
    requested_at: new Date().toISOString(),
    session: { enrollment: null, request: null },
    state: "pending",
  };
}

/** The machine login `agent-secrets machine login` opened on worker-9 and nobody decided in
 *  time: its row in the list, and the code its machine printed with the record the broker keeps
 *  for it, still pending, which the broker reads as expired since its deadline passed. */
const expiredMachineRecordID = "record-machine-expired";
function expiredMachine(approver: string): FakePendingRequest {
  return expiredCredentialRequest(
    {
      approver,
      identifiers: ["worker-9.example.com"],
      kind: "launcher_credential",
      record_id: expiredMachineRecordID,
      requested_at: askedAt,
      session: { enrollment: null, request: null },
    },
    deadline
  );
}
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

/** Runs `check` on a page signed in as the row's own person for alice (`rowPerson`, e2e/test.ts),
 *  whom the broker's requests name: the Inbox count those rows read is one person's, and holds
 *  the work of no other row only for a person no other row signs in as. */
async function withOwnAlice(
  browser: Browser,
  check: (page: Page, me: string) => Promise<void>
): Promise<void> {
  const me = rowPerson("alice");
  const context = await asUser(browser, me);
  try {
    await check(await context.newPage(), me);
  } finally {
    await context.close();
  }
}

// The pending request beside it is the control: it is a link to its broker page and is the one
// thing "Needs you" counts, before the dismissal and after it, so the expired row's lack of a
// link and of a count is its own and not a list that failed to load.
test("an expired secret request is marked with its deadline, leads to no decision page, is not counted, and Dismiss takes it off for good", async ({
  browser,
}) => {
  await withOwnAlice(browser, async (page, me) => {
    await setPendingCredentialRequests([pendingSecret(me), expiredSecret(me)]);
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
  await withOwnAlice(browser, async (page, me) => {
    await setPendingCredentialRequests([pendingMachine(me), expiredMachine(me)]);
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
