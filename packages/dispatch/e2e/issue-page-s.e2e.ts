/** the rows write through the server and read their own writes back on the page: the first
 *  writes a version of a grandchild's Verification document and reads it on the root's open page
 *  with no reload, through the server's coverage over every issue under the root and the event the
 *  server sends over the live socket, which names the grandchild's ancestors; the second writes an
 *  issue's relations through INFRA-1253's route and reads them in Details, through the server's
 *  issue read and its names read */
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  STATE_LINES,
  VERIFICATION_WORDS,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { buildDispatchReference } from "../web/src/features/refs/routes";
import {
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  documentBase,
  getArtifact,
  getIssueCoverage,
  patchIssue,
} from "./api";
import { specBlockRef } from "./issue-layout-seed-text";
import { gotoIssue, issueRead, openDetails, phase } from "./issue-page";
import { expectOpeningLanded } from "./issue-page-g";
import { detailsRow, marginSlot } from "./issue-page-h";
import { openPreset } from "./issue-page-i";
import { verificationCard } from "./issue-page-v";
import { expect, test } from "./test";

/**
 * Part S of dispatch://INFRA-1196 (dispatch://INFRA-5411), read on the real stack: what a
 * document event of an issue reaches on the new page of an issue above it, and the relations of
 * an issue in its Details. An issue's document events (`artifact.created`, `artifact.version`,
 * `artifact.removed`) name the issue's ancestors, and an ancestor's open page marks that
 * ancestor's tree read, each view of it, and on an upload or a version its coverage read, from
 * them (`web/src/api/event-refresh.ts`). Each row turns the new page on for alice with `phase`
 * (`e2e/issue-page.ts`, through `withIssuePage`) and seeds what it reads through `e2e/api.ts`.
 * The first opens the page with `openPreset` (`e2e/issue-page-i.ts`) and reads the Verification
 * view through `verificationCard` (`e2e/issue-page-v.ts`). The second writes an issue's relations
 * through `PATCH /api/v1/issues/{key}`, opens the issue with `gotoIssue` and Details with
 * `openDetails` (`e2e/issue-page.ts`), and reads Details' rows
 * (`web/src/features/issue-page/IssueMargin.tsx`) through `detailsRow` (`e2e/issue-page-h.ts`).
 *
 * The rows are the browser checks of the Verification document, by their titles:
 *
 * 1. `c-ancestor-coverage`: "a grandchild document version moves the root coverage".
 * 2. `c-relations`: "Details draws every relation of the issue once".
 */

/** The project every row files its issues in. */
const PROJECT = "ANC";

/** The block id of the root's one requirement, which the grandchild's first check serves. */
const REQUIREMENT = "r-drawn";

/** What `seedThreeGenerations` wrote. */
interface ThreeGenerations {
  readonly root: string;
  readonly grandchild: string;
  /** The grandchild's Verification document, which holds no check yet. */
  readonly grandchildVerification: string;
  /** The node id the root's Verification view draws the requirement's card under. */
  readonly requirement: string;
}

/** The checks the root's coverage read counts for its requirement, its own and those of every
 *  issue under it; undefined where the read does not hold the requirement yet. */
async function rootChecks(root: string): Promise<number | undefined> {
  const coverage = await getIssueCoverage(root);
  return (coverage.requirements ?? []).find((row) => row.block_id === REQUIREMENT)?.checks;
}

/**
 * A root with one requirement, a child under it, and a grandchild under the child whose
 * Verification document holds no check yet, so nothing serves the requirement. The seed waits
 * until the root's coverage read holds the requirement, served by no check, so the row reads the
 * view and not the indexing.
 */
async function seedThreeGenerations(): Promise<ThreeGenerations> {
  await createProject({ key: PROJECT, name: "Ancestors" });
  const root = await createIssue({
    force: true,
    project: PROJECT,
    spec: `:::requirement{#${REQUIREMENT}}\nEach issue under the root is drawn on its page.\n:::\n`,
    title: "The root whose requirement a grandchild serves",
  });
  const child = await createIssue({
    force: true,
    parent: root.key,
    project: PROJECT,
    title: "The child between the root and the grandchild",
  });
  const grandchild = await createIssue({
    force: true,
    parent: child.key,
    project: PROJECT,
    title: "The grandchild whose check serves the root",
  });
  const verification = await createIssueArtifact(grandchild.key, {
    content: "How the grandchild is proved.\n",
    name: "verification.md",
  });
  await expect
    .poll(() => rootChecks(root.key), {
      message: `${root.key}'s coverage read does not hold its requirement, served by no check`,
      timeout: 20_000,
    })
    .toBe(0);
  const specSlug = (await getArtifact(root.primary_artifact_id ?? "")).slug;
  return {
    grandchild: grandchild.key,
    grandchildVerification: verification.artifact.id,
    requirement: blockNodeId(specSlug, REQUIREMENT),
    root: root.key,
  };
}

// Row 1, `c-ancestor-coverage`. The root's new page is open on Verification, and nothing serves
// its requirement: the summary counts it uncovered, its card carries the gap of a statement no
// check serves, and no line counts checks elsewhere. A session writes the grandchild's
// Verification document again with its first check, which serves the root's requirement. The
// version's event names the grandchild's ancestors, so the root's page marks the root's coverage
// read, which counts every issue under the root, and with no reload the card counts one check
// elsewhere, carries no gap, and the summary counts the requirement covered (decision 402's page
// case). A server whose document events name no ancestors leaves the root's page unable to place
// the grandchild, and the counts stay as they were until a reload.
// SLOW(>=2s): three issues seeded and waited for in the coverage read, the root's page loaded,
// then a version written under it and the page's coverage read waited for, which the scheduler
// starts at most once a second (MIN_REFETCH_INTERVAL_MS, web/src/api/query-refresh.ts).
test("a grandchild document version moves the root coverage", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedThreeGenerations, async (page, seeded) => {
    await openPreset(page, seeded.root, "verification");
    await expectOpeningLanded(page);
    const card = verificationCard(page, seeded.requirement);
    await expect(
      card,
      "the Verification view draws no card for the root's requirement"
    ).toHaveCount(1);
    const covered = page.locator("[data-verification-covered]");
    const elsewhere = card.locator("[data-verification-elsewhere]");
    const coveredOf = (count: number) =>
      `${count} ${VERIFICATION_WORDS.of} 1 ${VERIFICATION_WORDS.covered}`;

    // Before the version: no check serves the requirement, the root's own or one under it.
    await expect(covered, "the summary does not count the requirement uncovered").toHaveText(
      coveredOf(0)
    );
    await expect(
      card.locator('[data-gap-kind="no_check"]'),
      "the requirement's card does not say no check serves it"
    ).toHaveCount(1);
    await expect(elsewhere, "the requirement's card counts checks elsewhere").toHaveCount(0);

    // A session writes the grandchild's Verification document again, its first check serving
    // the root's requirement, and the server counts it in the root's coverage read.
    await createIssueArtifact(
      seeded.grandchild,
      {
        content:
          "How the grandchild is proved.\n\n" +
          `:::check{#c-grandchild serves="${specBlockRef(seeded.root, REQUIREMENT)}"` +
          ' surface="browser" run="bun run e2e -- e2e/issue-page-s.e2e.ts" when="before_merge"}\n' +
          "The grandchild's first check serves the root's requirement.\n:::\n",
        name: "verification.md",
        precondition: await documentBase(seeded.grandchildVerification),
      },
      { as: "agent" }
    );
    await expect
      .poll(() => rootChecks(seeded.root), {
        message: `${seeded.root}'s coverage read does not count the grandchild's check`,
        timeout: 20_000,
      })
      .toBe(1);

    // With no reload, the root's page draws the counts its coverage read now answers.
    await expect(
      elsewhere,
      "the requirement's card does not count the grandchild's check elsewhere with no reload"
    ).toHaveText(`1 ${VERIFICATION_WORDS.elsewhere_one}`);
    await expect(
      covered,
      "the summary does not count the requirement covered with no reload"
    ).toHaveText(coveredOf(1));
    await expect(
      card.locator("[data-gap]"),
      "the requirement's card still carries a gap with no reload"
    ).toHaveCount(0);
  });
});

/** The first line of the message the second row's issue was filed from, and the rest of its body,
 *  which Details draws none of. */
const FILED_LINE = "Split the relations out of the parent.";
const FILED_REST = "The rest can wait for the review.";

/** What a row of Details says where a names read of the issues it names failed, the duplicate's
 *  row among them: `TITLES_READ_FAILED` of `web/src/features/issue-page/IssueMargin.tsx`, written
 *  out here because a browser row cannot load that module: its imports reach
 *  `web/src/features/doc/schema.ts`, which imports the block schema's JSON with no import
 *  attribute. */
const TITLES_READ_FAILED = "Could not read the titles of these issues.";

/** One issue `seedRelations` wrote, by its key and its title. */
interface SeededIssue {
  readonly key: string;
  readonly title: string;
}

/** What `seedRelations` wrote: the issue that holds every relation Details draws, each issue it
 *  is related to, and the message it was filed from, by its canonical reference and the page
 *  that reference opens. */
interface Relations extends SeededIssue {
  readonly blocker: SeededIssue;
  readonly duplicate: SeededIssue;
  readonly message: { readonly path: string; readonly reference: string };
  readonly parent: SeededIssue;
  readonly waiter: SeededIssue;
}

/**
 * An issue that holds every relation Details draws: its parent, an issue it waits on, an issue
 * that waits on it, the issue it repeats (`duplicate_of`) and a message of its parent that it was
 * filed from (`filed_from`), the last two written through INFRA-1253's route,
 * `PATCH /api/v1/issues/{key}`. The issue it repeats holds neither of those two relations itself.
 */
async function seedRelations(): Promise<Relations> {
  await createProject({ key: PROJECT, name: "Ancestors" });
  // `force` past the possible-duplicate guard: these titles are deliberately close.
  const seeded = async (title: string, parent?: string): Promise<SeededIssue> => {
    const made = await createIssue({ force: true, parent, project: PROJECT, title });
    return { key: made.key, title };
  };
  const parent = await seeded("The parent the relations sit under");
  const blocker = await seeded("The issue the relations wait on");
  const duplicate = await seeded("The issue the relations repeat");
  const related = await seeded("The issue that holds every relation", parent.key);
  const waiter = await seeded("The issue that waits on the relations");
  await patchIssue(waiter.key, { blocked_by: [related.key] });
  const message = await createMessage(parent.key, { body: `${FILED_LINE} ${FILED_REST}` });
  const reference = `dispatch://${parent.key}/message/${message.id}`;
  await patchIssue(related.key, {
    blocked_by: [blocker.key],
    duplicate_of: duplicate.key,
    filed_from: reference,
  });
  return {
    ...related,
    blocker,
    duplicate,
    message: { path: `/issues/${parent.key}/messages/${message.id}`, reference },
    parent,
    waiter,
  };
}

// Row 2, `c-relations`. Details draws the issue's five relations once each: Parent by the
// parent's key as a link; Blocked by, Blocks and the issue it repeats each by the issue's key in
// bold as a link, with its title; and the message it was filed from by the page's word for a
// message and the message's first line, as a link that opens the message. The issue it repeats
// holds neither of the two relations INFRA-1253 added, and its Details draws neither row. On a
// load whose names read of the two relations fails, Details' two relation rows say so, each with
// Retry, the header keeps every value it drew, and Retry draws the rows again. Each relation's
// link is followed by pointer and by keyboard. A page that draws neither row fails at its first
// reading of the issue's duplicate.
// SLOW(>=2s): five issues and a message seeded, the issue's page loaded four more times to
// follow its two links two ways, and a read the page asks again twice, 500 ms apart, before it
// says that read failed (`queryRetry`, web/src/api/query-client.ts).
test("Details draws every relation of the issue once", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedRelations, async (page, seeded) => {
    // The names read of the two relations fails while `refusing` holds, each time the page asks
    // it, and no other read does: the page asks a failed read twice more before it says so.
    const relations = [
      buildDispatchReference({ key: seeded.duplicate.key, kind: "issue" }),
      seeded.message.reference,
    ]
      .sort()
      .join(" ");
    let refusing = false;
    let refused = 0;
    await page.route("**/api/v1/reference-targets?**", (route) => {
      const refs = new URL(route.request().url()).searchParams.getAll("ref").sort().join(" ");
      if (!refusing || refs !== relations) return route.fallback();
      refused += 1;
      return route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "refused" }),
        contentType: "application/json",
        status: 500,
      });
    });
    const path = `/issues/${seeded.key}`;
    const header = page.getByTestId("issue-header");
    const duplicateRow = detailsRow(page, PROPERTY_LABELS.duplicate_of);
    const filedRow = detailsRow(page, PROPERTY_LABELS.filed_from);
    const duplicateLink = duplicateRow.getByRole("link", {
      exact: true,
      name: seeded.duplicate.key,
    });
    const filedLink = filedRow.getByRole("link", { exact: true, name: FILED_LINE });
    const expectEveryRelation = async (when: string) => {
      for (const label of [
        PROPERTY_LABELS.parent,
        PROPERTY_LABELS.blocked_by,
        PROPERTY_LABELS.blocks,
        PROPERTY_LABELS.duplicate_of,
        PROPERTY_LABELS.filed_from,
      ]) {
        await expect(
          detailsRow(page, label),
          `${when}, Details draws ${label} not once`
        ).toHaveCount(1);
      }
      await expect(
        detailsRow(page, PROPERTY_LABELS.parent).getByRole("link", {
          exact: true,
          name: seeded.parent.key,
        }),
        `${when}, Parent does not link the parent`
      ).toHaveAttribute("href", `/issues/${seeded.parent.key}`);
      for (const [label, related] of [
        [PROPERTY_LABELS.blocked_by, seeded.blocker],
        [PROPERTY_LABELS.blocks, seeded.waiter],
        [PROPERTY_LABELS.duplicate_of, seeded.duplicate],
      ] as const) {
        const row = detailsRow(page, label);
        const link = row.getByRole("link", { exact: true, name: related.key });
        await expect(link, `${when}, ${label} does not link ${related.key}`).toHaveAttribute(
          "href",
          `/issues/${related.key}`
        );
        await expect(row, `${when}, ${label} does not draw ${related.key}'s title`).toContainText(
          related.title
        );
        expect(
          await link.evaluate((element) => Number(getComputedStyle(element).fontWeight)),
          `${when}, ${label} does not draw ${related.key}'s key in bold`
        ).toBeGreaterThanOrEqual(600);
      }
      await expect(
        filedRow,
        `${when}, the message's row does not name it by its kind and its first line`
      ).toHaveText(
        `${PROPERTY_LABELS.filed_from}${NODE_REFERENCES_WORDS.kinds.message} ${FILED_LINE}`
      );
      await expect(filedRow).not.toContainText(FILED_REST);
      await expect(
        filedLink,
        `${when}, the message's row does not open the message`
      ).toHaveAttribute("href", seeded.message.path);
      await expect(
        marginSlot(page).getByRole("alert"),
        `${when}, Details says a failure`
      ).toHaveCount(0);
    };

    // The names read answers: Details draws every relation once.
    await gotoIssue(page, path);
    await openDetails(page);
    await expectEveryRelation("with the names read answered");
    const drawnHeader = await header.innerText();

    // A load whose names read of the relations fails: the two relation rows say so, each with
    // Retry, drawing the duplicate by its key and the message by its kind's word alone, and the
    // header keeps every value it drew.
    refusing = true;
    await page.reload();
    await issueRead(page, path);
    await openDetails(page);
    for (const [label, row, words] of [
      [PROPERTY_LABELS.duplicate_of, duplicateRow, TITLES_READ_FAILED],
      [PROPERTY_LABELS.filed_from, filedRow, STATE_LINES.filed_from_unreadable],
    ] as const) {
      await expect(row.getByRole("alert"), `${label} does not say the read failed`).toHaveText(
        words
      );
      await expect(
        row.getByRole("button", { exact: true, name: CONTROL_LABELS.retry }),
        `${label} offers no Retry of the failed read`
      ).toBeVisible();
    }
    expect(refused, "the failed read was asked more than three times").toBeLessThanOrEqual(3);
    await expect(duplicateLink, "the duplicate is not drawn by its key").toBeVisible();
    await expect(duplicateRow).not.toContainText(seeded.duplicate.title);
    await expect(
      filedRow.getByRole("link", { exact: true, name: NODE_REFERENCES_WORDS.kinds.message }),
      "the message is not drawn by its kind's word"
    ).toHaveAttribute("href", seeded.message.path);
    await expect(
      marginSlot(page).getByRole("alert"),
      "a row besides the two relation rows says the failure"
    ).toHaveCount(2);
    expect(await header.innerText(), "the header lost a value it drew").toBe(drawnHeader);
    await expect(header.getByRole("alert"), "the header says the failure").toHaveCount(0);

    // Retry, pressed by keyboard, draws the rows again, and hands its focus to its row's link.
    refusing = false;
    await filedRow.getByRole("button", { exact: true, name: CONTROL_LABELS.retry }).focus();
    await page.keyboard.press("Enter");
    await expectEveryRelation("after Retry");
    await expect(
      filedLink,
      "Retry's answer did not hand the focus to the row's link"
    ).toBeFocused();
    expect(await header.innerText(), "the header lost a value it drew").toBe(drawnHeader);

    // Each relation's link is followed by pointer and by keyboard.
    for (const [what, link, to] of [
      ["the duplicate", duplicateLink, `/issues/${seeded.duplicate.key}`],
      ["the message", filedLink, seeded.message.path],
    ] as const) {
      for (const by of ["pointer", "keyboard"] as const) {
        await gotoIssue(page, path);
        await openDetails(page);
        await expect(link, `Details draws no link to ${what}`).toBeVisible();
        if (by === "pointer") {
          await link.click();
        } else {
          await link.focus();
          await page.keyboard.press("Enter");
        }
        await expect
          .poll(() => new URL(page.url()).pathname, {
            message: `the ${by} did not open ${what} from its link`,
          })
          .toBe(new URL(to, page.url()).pathname);
      }
    }

    // The issue it repeats holds neither relation, and its Details draws neither row.
    await gotoIssue(page, `/issues/${seeded.duplicate.key}`);
    await openDetails(page);
    await expect(
      detailsRow(page, PROPERTY_LABELS.blocks),
      "the duplicate's Details has not read what waits on it"
    ).toContainText(CONTROL_LABELS.none);
    await expect(duplicateRow, "an issue that repeats none draws its row").toHaveCount(0);
    await expect(filedRow, "an issue filed from nothing draws its row").toHaveCount(0);
  });
});
