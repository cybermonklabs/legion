import { createHash } from "node:crypto";

import {
  createIssue,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactVersion,
  getSpecNode,
  putSpecSource,
  requestApproval,
  reviewArtifact,
  withOpenedAsk,
} from "./api";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// R22's last paragraph: "Before the person approves on a phone, Dispatch shows a sheet that
// restates what the approval covers. After the approval, the issue shows the change request as the
// person approved it, with its digest." A project names `acme/app` its spec source; one issue of
// that repository writes the first approved spec, and a second changes one statement, adds one
// and removes one. The rows read the sheet on a phone before the person's approval, the approved
// change request on the issue after it, and an issue of no spec source, whose approval on a phone
// shows no sheet.

const session = {
  actor: { kind: "session" as const, id: "e2e-session", origin: { session_title: "architect" } },
  as: "agent" as const,
};

/** The spec source every row's spec-source issues are of. */
const REPO = "acme/app";

/** The first approved spec: two statements about billing and the component they are about. */
const SEED_SPEC = [
  ':::requirement{#an-order-is-paid-once kind="behavior" about="billing"}',
  "An order is paid once.",
  ":::",
  "",
  ':::requirement{#a-bill-names-its-order kind="behavior" about="billing"}',
  "A bill names its order.",
  ":::",
  "",
  ':::component{#billing name="Billing"}',
  "It takes the payment of each order.",
  ":::",
  "",
].join("\n");

/** The change request: one statement changed and one removed against version 1, one added. The
 *  removal's own words are not the approved text it removes. */
const CHANGE_SPEC = [
  "Why billing changes.",
  "",
  ':::requirement{#an-order-is-paid-once kind="behavior" about="billing" base="1"}',
  "An order is paid once, and a second payment is refused.",
  ":::",
  "",
  ':::requirement{#a-refund-returns-the-payment kind="behavior" about="billing"}',
  "A refund returns the payment of an order.",
  ":::",
  "",
  ':::requirement{#a-bill-names-its-order kind="behavior" about="billing" base="1" removes="true"}',
  "This statement goes.",
  ":::",
  "",
].join("\n");

/** Names `acme/app` a spec source of `project`, writes its first approved spec, and opens the
 *  approval request of the issue whose spec is `CHANGE_SPEC`. */
async function seedChangeRequest(project: string) {
  await createProject({ key: project, name: "Spec sheet" });
  await putSpecSource(project, REPO);
  const seed = await createIssue({ project, repo: REPO, spec: SEED_SPEC, title: "Billing" });
  withOpenedAsk(await requestApproval(seed.primary_artifact_id, {}, session));
  await reviewArtifact(seed.primary_artifact_id, { state: "approved" }, { login: "alice" });
  const seeded = await getSpecNode(project, REPO, "an-order-is-paid-once", { login: "alice" });
  expect(seeded.versions?.length).toBe(1);
  const change = await createIssue({ project, repo: REPO, spec: CHANGE_SPEC, title: "Refunds" });
  const requested = withOpenedAsk(await requestApproval(change.primary_artifact_id, {}, session));
  return { change, requested };
}

/** The digest of the stored text of version `number` of the document `id`: what a person's
 *  approval of that version pins while no record of a tree's revision stands for it. */
async function storedDigest(id: string, number: number): Promise<string> {
  const version = await getArtifactVersion(id, number, { login: "alice" });
  return createHash("sha256").update(version.markdown, "utf8").digest("hex");
}

function approvalSheet(page: Page): Locator {
  return page.getByRole("dialog", { name: "What this approval covers" });
}

// SLOW(>=2s): it seeds an approved spec, opens the sheet twice and waits for the approval to be
// written.
test("on a phone, Approve of a spec-source issue first shows a sheet of each added, changed and removed node with its text and the digest the approval pins @phone @phone-only", async ({
  browser,
}, testInfo) => {
  const { change, requested } = await seedChangeRequest("SHEETA");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // The approval request's own card on the Inbox page, as a phone draws it.
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await card.getByRole("radio", { name: /^Approve/ }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    const sheet = approvalSheet(page);
    await expect(sheet).toBeVisible();
    const node = (id: string) => sheet.locator(`[data-node-id="${id}"]`);
    await expect(sheet.locator("[data-node-id]")).toHaveCount(3);
    await expect(node("an-order-is-paid-once")).toHaveAttribute("data-what", "changed");
    await expect(node("an-order-is-paid-once").getByTestId("approval-sheet-text")).toHaveText(
      "An order is paid once, and a second payment is refused."
    );
    await expect(node("an-order-is-paid-once").getByTestId("approval-sheet-approved")).toHaveText(
      "An order is paid once."
    );
    await expect(node("a-refund-returns-the-payment")).toHaveAttribute("data-what", "added");
    await expect(
      node("a-refund-returns-the-payment").getByTestId("approval-sheet-text")
    ).toHaveText("A refund returns the payment of an order.");
    await expect(node("a-bill-names-its-order")).toHaveAttribute("data-what", "removed");
    await expect(node("a-bill-names-its-order").getByTestId("approval-sheet-text")).toHaveText(
      "A bill names its order."
    );
    const digest = sheet.getByTestId("approval-sheet-digest");
    await expect(digest).toHaveText(await storedDigest(change.primary_artifact_id, 1));
    await sheet.screenshot({ path: testInfo.outputPath("approval-sheet-phone.png") });

    // Cancel approves nothing: the request still waits.
    await sheet.getByRole("button", { exact: true, name: "Cancel" }).click();
    await expect(sheet).toHaveCount(0);
    expect(
      (await getArtifact(change.primary_artifact_id, { login: "alice" })).approval?.state
    ).toBe("awaiting");

    // The sheet's Approve records the approval, which pins the digest the sheet showed.
    const shown = await storedDigest(change.primary_artifact_id, 1);
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await approvalSheet(page).getByRole("button", { exact: true, name: "Approve" }).click();
    await expect
      .poll(
        async () =>
          (await getArtifact(change.primary_artifact_id, { login: "alice" })).approval?.state
      )
      .toBe("approved");
    const approval = (await getArtifact(change.primary_artifact_id, { login: "alice" })).approval;
    expect(approval?.content_sha256).toBe(shown);
    const refund = await getSpecNode("SHEETA", REPO, "a-refund-returns-the-payment", {
      login: "alice",
    });
    expect(refund.versions?.at(-1)?.provenance.issue).toBe(change.key);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): it seeds and approves two change requests, then edits the issue after the approval.
test("after the approval, the issue shows the change request as approved, each text read from its approved version with its provenance, and the digest @phone", async ({
  browser,
}, testInfo) => {
  const { change, requested } = await seedChangeRequest("SHEETB");
  await reviewArtifact(change.primary_artifact_id, { state: "approved" }, { login: "alice" });
  const approval = (await getArtifact(change.primary_artifact_id, { login: "alice" })).approval;
  expect(approval?.state).toBe("approved");
  expect(approval?.ask_id).toBe(requested.ask.id);
  // After the approval the issue's own blocks change; the approved change request does not.
  await editArtifact(
    change.primary_artifact_id,
    {
      ops: [
        {
          find: "A refund returns the payment of an order.",
          op: "replace",
          with: "A refund returns the whole payment, written after the approval.",
        },
      ],
    },
    session
  );
  await expect
    .poll(
      async () =>
        (await getArtifact(change.primary_artifact_id, { login: "alice" })).approval?.state
    )
    .toBe("stale");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${change.key}`);
    const approved = page.getByRole("region", { name: "Approved change request" });
    await expect(approved).toBeVisible();
    const node = (id: string) => approved.locator(`[data-node-id="${id}"]`);
    await expect(approved.locator("[data-node-id]")).toHaveCount(3);
    for (const [id, what, text, version] of [
      [
        "an-order-is-paid-once",
        "changed",
        "An order is paid once, and a second payment is refused.",
        2,
      ],
      ["a-refund-returns-the-payment", "added", "A refund returns the payment of an order.", 1],
      ["a-bill-names-its-order", "removed", "A bill names its order.", 1],
    ] as const) {
      await expect(node(id)).toHaveAttribute("data-what", what);
      await expect(node(id).getByTestId("approved-change-request-text")).toHaveText(text);
      const read = await getSpecNode("SHEETB", REPO, id, { login: "alice" });
      const written =
        what === "removed"
          ? read.removals?.at(-1)
          : read.versions?.find((entry) => entry.version === version);
      expect(written?.provenance.issue).toBe(change.key);
      expect(written?.provenance.ask).toBe(requested.ask.id);
      const provenance = node(id).getByTestId("approved-change-request-provenance");
      await expect(provenance).toContainText("alice");
      await expect(provenance.getByRole("time")).toHaveAttribute("datetime", written?.at ?? "");
      await expect(provenance.getByRole("link", { name: "the approval request" })).toHaveAttribute(
        "href",
        new RegExp(requested.ask.id)
      );
    }
    await expect(approved).not.toContainText("written after the approval");
    await expect(approved.getByTestId("approved-change-request-digest")).toHaveText(
      approval?.content_sha256 ?? "no digest"
    );
    await approved.screenshot({
      path: testInfo.outputPath(`approved-change-request-${testInfo.project.name}.png`),
    });
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): it waits for the approval to be written.
test("on a phone, Approve of an issue of no spec source sends the approval as today, with no sheet @phone @phone-only", async ({
  browser,
}) => {
  await createProject({ key: "SHEETC", name: "No spec source" });
  const issue = await createIssue({
    project: "SHEETC",
    spec: CHANGE_SPEC,
    title: "Refunds elsewhere",
  });
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await card.getByRole("radio", { name: /^Approve/ }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect
      .poll(
        async () =>
          (await getArtifact(issue.primary_artifact_id, { login: "alice" })).approval?.state
      )
      .toBe("approved");
    await expect(approvalSheet(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
