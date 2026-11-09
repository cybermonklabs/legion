import { expect, test } from "@playwright/test";

import type { FakeGrant } from "./fake-broker";
import { setCredentialGrants } from "./fake-broker-helpers";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The judge as the broker records it: `requests.decided_by` and the approval event's own audit
 *  actor are both `judge:` and the id of the model that decided it. */
const judge = "judge:claude-opus-4-6";

/** A grant the broker's model judge approved in alice's stead. The judge holds nothing of the
 *  grant afterwards, so the grant records alice, who may end it, while the request it rests on
 *  records the judge, who decided it. The two fields differ here and nowhere else, which is what
 *  the row below is about. */
const judged: FakeGrant = {
  approver: "alice",
  created_at: "2026-10-03T12:07:00Z",
  decided_by: judge,
  enrollment: { kind: "pod", operator: "", runtime_id: "pod-7", slot: null },
  expires_at: "2026-10-03T12:22:00Z",
  grant_id: "11111111-1111-4111-8111-111111111111",
  granted: "approval",
  listedFor: "alice",
  names: ["DEEL_API_KEY"],
  record_id: "aa11bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900",
};

/** A grant alice approved herself on the broker's own page, which names her both ways. */
const byAlice: FakeGrant = {
  ...judged,
  created_at: "2026-10-03T12:08:00Z",
  decided_by: "alice",
  grant_id: "22222222-2222-4222-8222-222222222222",
  names: ["BILLING_KEY"],
  record_id: "bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900aa11",
};

// A person reads Live grants to see what is live in their name and to end what should not be.
// A grant the model judge approved is in that list because the record named alice as its
// approver, and alice never saw the request: the judge decided it. So the row has to say the
// judge decided it. The row read "Approved by alice", off the grant's own approver, which is a
// person who decided nothing.
//
// Alice's own grant is seeded beside it, where the approver and the decider are one person, so
// the row that names the judge is held against a row that names a person on the same page.
test("Live grants says the model judge decided a judged grant, and alice decided her own", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setCredentialGrants([judged, byAlice]);

    await page.goto("/settings");
    const grants = page.getByRole("region", { name: "Live grants" });
    const judgedRow = grants.getByRole("row").filter({ hasText: "DEEL_API_KEY" });
    const ownRow = grants.getByRole("row").filter({ hasText: "BILLING_KEY" });
    await expect(judgedRow).toHaveCount(1);
    await expect(ownRow).toHaveCount(1);

    await expect(judgedRow.getByText(`Approved by ${judge}`)).toBeVisible();
    await expect(judgedRow.getByText("Approved by alice", { exact: true })).toHaveCount(0);
    await expect(ownRow.getByText("Approved by alice", { exact: true })).toBeVisible();

    // The judge changes nothing about who may end the grant: alice is its approver, so the
    // broker allows her revoke and the row offers one.
    await expect(judgedRow.getByRole("button", { name: "Revoke" })).toBeVisible();
  });
});
