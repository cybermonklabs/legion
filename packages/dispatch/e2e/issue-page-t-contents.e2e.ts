/** the rows read the Contents pane and sheet beside each view of an issue the server seeds and
 *  serves, and count the tree reads each load and press sends the server */

import type { Locator, Page } from "@playwright/test";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PRESET_NAMES, type Preset } from "../web/src/features/issue-page/page-words";
import { getIssue } from "./api";
import { seedWholeIssue } from "./issue-layout-seed";
import { contentsPane, gotoIssue, outlineControl, outlineSheet, phase } from "./issue-page";
import { DESKTOP, nodeRows, PHONE } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import { drawnContentsEntries } from "./issue-page-j";
import { FILTERS, loadViews, PAGE_VIEWS, ROW_MS } from "./issue-page-t-rows";
import {
  decisionsViewGate,
  heldAnswer,
  outlineAnswerOf,
  outlineReads,
  owedUnless,
  treeReadGate,
  treeReads,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Row 8 of the rows that draw the new issue page from the tree read: the Contents pane stands
 * beside Conversation, each filter and Graph from 1024 px up, the Outline control opens its list
 * as a sheet below, and the three sentences are drawn once in each view (spec R13). The head of
 * `e2e/issue-page-t-rows.ts` says what the rows read and what their gates are.
 */

/** The issue's three sentences, wherever a view draws them (`IssueThreeThings.tsx`). */
function threeThings(scope: Page | Locator): Locator {
  return scope.locator('[data-testid="issue-three-things"]');
}

/** The ids of the entries a Contents list draws, sorted: two lists hold the same entries where
 *  they hold the same ids, whichever outline read each takes its order from. */
async function entriesOf(scope: Locator): Promise<string[]> {
  const ids = await drawnContentsEntries(scope).evaluateAll((all) =>
    all.map((entry) => entry.getAttribute("data-contents-entry") ?? "")
  );
  return ids.sort();
}

/** Opens `view` of issue `key` cold and waits until its load has gone quiet. */
async function openView(page: Page, key: string, view: Preset): Promise<ApiRequests> {
  const traffic = watchApiRequests(page);
  await gotoIssue(page, presetPath(key, view));
  await expectLoadSettled(traffic);
  return traffic;
}

// c-contents-beside.
// SLOW(>=2s): the whole seeded issue, each of six views loaded cold at two widths with each load
// waited out, the Outline sheet opened in each at 390 px, and a filter pressed after a load.
test("the Contents pane stands beside each filter and Graph @phone", async ({ browser }) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const issue = await getIssue(seeded.key);

    // From 1024 px up: the pane, with the three sentences and the list, beside each view, the
    // list holding the same entries in each, and the sentences drawn once, in the pane.
    await page.setViewportSize(DESKTOP);
    let entries: string[] = [];
    for (const view of PAGE_VIEWS) {
      const name = PRESET_NAMES[view];
      // Conversation's read, watched from before its load, which only that view's load reads.
      const conversation =
        view === "conversation" ? outlineAnswerOf(page, "conversation") : undefined;
      const traffic = await openView(page, seeded.key, view);
      await expect(contentsPane(page), `the Contents pane stands beside ${name}`).toHaveCount(1);
      await expect(
        threeThings(contentsPane(page)),
        `at 1440 px, the pane beside ${name} draws no three sentences`
      ).toHaveCount(1);
      await expect(
        threeThings(page),
        `at 1440 px, ${name} draws the three sentences other than once, in the pane`
      ).toHaveCount(1);
      if (conversation !== undefined) {
        await expect
          .poll(async () => (await entriesOf(contentsPane(page))).length, {
            message: "the Contents list beside Conversation holds no entry",
          })
          .toBeGreaterThan(0);
        entries = await entriesOf(contentsPane(page));
        // The two rows under the three sentences take their counts from reads the page already
        // makes: the load sends no read of the tree but Conversation's, R4's view reads and, where
        // the decisions read is the tree read's `view=decisions`, that one (`decisionsViewGate`).
        const views = loadViews(issue, await heldAnswer(conversation, "Conversation's read"));
        const sent = traffic.requests;
        const decisions = decisionsViewGate(sent, seeded.key).met ? ["decisions"] : [];
        expect(
          {
            graph: outlineReads(sent, seeded.key, "graph"),
            kind: outlineReads(sent, seeded.key, "conversation"),
            views: FILTERS.flatMap((one) =>
              treeReads(sent, seeded.key, one) > 0 ? [one] : []
            ).sort(),
          },
          "Conversation's load sends no tree read but its kind= read, R4's view reads and the " +
            "Decisions view's read where the decisions read is one"
        ).toEqual({ graph: 0, kind: 1, views: [...views, ...decisions].sort() });
        // Spec turned on after Conversation has loaded reads Conversation's read from the cache.
        const pressed = traffic.requests.length;
        await pickPreset(page, PRESET_NAMES.spec);
        await expectActivePreset(page, PRESET_NAMES.spec);
        await expectLoadSettled(traffic);
        expect(
          outlineReads(traffic.requests.slice(pressed), seeded.key, "conversation"),
          "turning Spec on after Conversation has loaded sends no second kind= read"
        ).toBe(0);
      } else {
        await expect
          .poll(() => entriesOf(contentsPane(page)), {
            message: `the Contents list beside ${name} holds other entries than beside Conversation`,
          })
          .toEqual(entries);
      }
      if (view === "spec") {
        expect(
          {
            kind: outlineReads(traffic.requests, seeded.key, "conversation"),
            spec: treeReads(traffic.requests, seeded.key, "spec"),
          },
          "a cold load of /spec at 1440 px sends one kind= read beside the view=spec read"
        ).toEqual({ kind: 1, spec: 1 });
      }
      if (view === "graph") {
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /graph sends no kind= read"
        ).toBe(0);
      }
    }

    // Below 1024 px: the three sentences above the view's first row, once, and the Outline
    // control's sheet holding the same list and not the sentences.
    await page.setViewportSize(PHONE);
    for (const view of PAGE_VIEWS) {
      const name = PRESET_NAMES[view];
      const traffic = await openView(page, seeded.key, view);
      // The one control the view row is at 390 px names the view the address opened, Conversation
      // and Graph by their word alone, since neither draws a signal.
      await expectActivePreset(page, name);
      await expect(contentsPane(page), `at 390 px, ${name} draws the Contents pane`).toHaveCount(0);
      await expect(
        threeThings(page),
        `at 390 px, the three sentences stand in ${name} other than once`
      ).toHaveCount(1);
      const [sentences, firstRow] = await Promise.all([
        threeThings(page).boundingBox(),
        nodeRows(page).first().boundingBox(),
      ]);
      expect(
        (sentences?.y ?? Number.POSITIVE_INFINITY) + (sentences?.height ?? 0),
        `at 390 px, the three sentences stand above ${name}'s first row`
      ).toBeLessThanOrEqual(firstRow?.y ?? Number.NEGATIVE_INFINITY);
      if (view === "spec") {
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /spec at 390 px sends no kind= read before the sheet opens"
        ).toBe(0);
      }
      if (view === "verification") {
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /verification at 390 px sends the kind= read with the load"
        ).toBe(1);
      }
      await outlineControl(page).click();
      await expect(
        outlineSheet(page),
        `at 390 px, the Outline control opens no sheet in ${name}`
      ).toHaveCount(1);
      await expect
        .poll(() => entriesOf(outlineSheet(page)), {
          message: `at 390 px, the Outline sheet draws the Contents list in ${name}, with the entries beside Conversation`,
        })
        .toEqual(entries);
      if ((FILTERS as readonly string[]).includes(view)) {
        await expect(
          threeThings(outlineSheet(page)),
          `at 390 px, under ${name} the Outline sheet draws the three sentences`
        ).toHaveCount(0);
      }
      if (view === "spec") {
        await expectLoadSettled(traffic);
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /spec at 390 px sends the kind= read when the sheet opens"
        ).toBe(1);
      }
      await page.keyboard.press("Escape");
      await expect(
        outlineSheet(page),
        `at 390 px, Escape does not close the sheet in ${name}`
      ).toHaveCount(0);
    }
  });
});
