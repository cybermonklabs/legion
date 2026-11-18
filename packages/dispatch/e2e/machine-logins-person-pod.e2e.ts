import type { FakeLauncherCredential } from "./fake-broker";
import { setLauncherCredentials } from "./fake-broker-helpers";
import { expect, type Locator, type Page, test } from "./test";
import { withAlice } from "./users";

/** The login service alice's pod account logs in as, which the broker names as the login's
 *  service. The page before this change labelled the row with it, `person-alice on <host>`. */
const alicePodService = "person-alice";

/** Alice's pod login: the token login of a pod whose Kubernetes account the broker's
 *  BROKER_POD_OPERATORS maps to alice. The broker sets its service, the login service the account
 *  logs in as, and its subject, as it does for a service's token login, and it names alice as the
 *  login's person. Only alice's list holds it, and only she may revoke it. */
const alicePod: FakeLauncherCredential = {
  approved_by: "",
  credential_id: "cred-alice-pod",
  expired: false,
  expires_at: "2026-11-18T13:00:00Z",
  host: "people/alice",
  issued_at: "2026-11-18T12:00:00Z",
  listedFor: "alice",
  person: "alice",
  service: alicePodService,
  subject: "system:serviceaccount:people:alice",
};

/** The Legion daemon's token login, a service's login: every signed-in person's list holds it,
 *  anyone signed in may revoke it, and its person is null. */
const daemon: FakeLauncherCredential = {
  approved_by: "",
  credential_id: "cred-daemon",
  expired: false,
  expires_at: "2026-11-18T13:00:00Z",
  host: "legion-system/legion-daemon",
  issued_at: "2026-11-18T12:00:00Z",
  listedFor: "alice",
  person: null,
  service: "legion-daemon",
  subject: "system:serviceaccount:legion-system:legion-daemon",
};

/** The question a row's Revoke asks, which the page asks with `window.confirm`. The dialog is
 *  dismissed, so nothing is revoked and the row stays. */
async function revokeQuestion(page: Page, row: Locator): Promise<string> {
  const asked = Promise.withResolvers<string>();
  page.once("dialog", (dialog) => {
    asked.resolve(dialog.message());
    void dialog.dismiss();
  });
  await row.getByRole("button", { name: "Revoke" }).click();
  return asked.promise;
}

// R5 of dispatch://INFRA-4511, check b8: "Dispatch's page of machine logins shows a person's pod
// login as that person's own, and not as a service's login that anyone signed in may revoke."
// The broker marks such a login with its person (`person` on GET /v1/launcher-credentials), and
// Dispatch relays the broker's answer to the page. So alice's page shows the fake broker's row
// that names her as her own pod: its machine, who approved it, the section's sentence on who
// may revoke it, and its Revoke's question. None of them calls it a service's login. The
// daemon's login beside it names no person and still reads as a service's login. The page
// before this change reads the pod's row as the login of its service, `person-alice`.
test("the machine logins section shows a person's pod login as that person's own, beside a service's login", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setLauncherCredentials([alicePod, daemon]);

    await page.goto("/credentials/machine");
    const section = page.getByRole("region", { name: "Machine logins" });
    const podRow = section.getByRole("row").filter({ hasText: alicePod.credential_id });
    const daemonRow = section.getByRole("row").filter({ hasText: daemon.credential_id });
    await expect(podRow).toHaveCount(1);
    await expect(daemonRow).toHaveCount(1);

    // The pod's row is alice's own pod, and no text in it names the pod's login service.
    await expect(podRow.getByRole("cell").nth(0)).toHaveText("people/alice (your pod)");
    await expect(podRow.getByRole("cell").nth(2)).toHaveText(
      "No one: BROKER_POD_OPERATORS maps system:serviceaccount:people:alice to you"
    );
    await expect(podRow).not.toContainText(alicePodService);
    await expect(section).toContainText(
      "Only you may revoke the logins of your machines and your pods. Anyone signed in may revoke a service's login."
    );
    const podQuestion = await revokeQuestion(page, podRow);
    expect(podQuestion).toBe(
      "Revoke the machine login for people/alice (your pod)? Every session it started ends at once. The pod logs in again with its token, under a new login, while the broker's BROKER_POD_OPERATORS maps its Kubernetes account to you."
    );
    expect(podQuestion).not.toContain(alicePodService);

    // The daemon's row still reads as a service's login.
    await expect(daemonRow.getByRole("cell").nth(0)).toHaveText(
      "legion-daemon on legion-system/legion-daemon"
    );
    await expect(daemonRow.getByRole("cell").nth(2)).toHaveText(
      "No one: system:serviceaccount:legion-system:legion-daemon"
    );
    const daemonQuestion = await revokeQuestion(page, daemonRow);
    expect(daemonQuestion).toContain("its worker pods included");
    expect(daemonQuestion).toContain("BROKER_SERVICE_LOGINS");

    // Both questions were dismissed, so both logins are still listed.
    await expect(podRow).toHaveCount(1);
    await expect(daemonRow).toHaveCount(1);
  });
});
