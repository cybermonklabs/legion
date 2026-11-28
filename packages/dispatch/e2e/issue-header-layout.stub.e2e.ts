/** Today's issue page on the layout layer's stress issue (`e2e/layout-seed.ts`), read for two facts
 *  the layout rule's own rows (`e2e/layout-rule.stub.e2e.ts`) do not read: how tall the header is
 *  below its title with Details closed, and whether every tab of the issue is whole and opens its
 *  panel. Each row reads what the page draws from its answers and its sockets, so each runs
 *  against its recording (packages/dispatch/README.md, "The stubbed layer"); re-record after a
 *  change to the stress layer or to what the page reads:
 *  `bun run e2e:stub:record -- issue-header-layout.stub`. */
import { expect, type Page } from "@playwright/test";

import { expectTimeout } from "./expect-timeout";
import { issueDetailsButton } from "./issue-details";
import { pinFontFaces } from "./layout-check";
import type { LayoutSeed } from "./layout-routes";
import { seedLayoutStress } from "./layout-seed";
import { routeLayoutSockets } from "./layout-sockets";
import { stubbed } from "./stub/stub-test";

const test = stubbed(() => seedLayoutStress({ boundMs: expectTimeout }));

// The rows measure text, so they draw it as the layout rows do: in the pinned faces
// (`e2e/layout-check.ts`), in `en-US` and UTC, so a run on any machine reads the heights CI reads.
test.use({ locale: "en-US", timezoneId: "UTC" });

/** How tall today's issue header may be below its title block, with Details closed, on the stress
 *  issue (dispatch://INFRA-5200): the part of the header the layout rule lays out. The title's
 *  length is its author's text, so it is not bounded here. Measured on the stress issue,
 *  2026-11-23: 321 px at 1280 and 519 at 360; one line more read 373 and 571 (a dummy 44 px
 *  line), so each bound fails a header that piles one more line on. */
const BELOW_TITLE_MAX: Readonly<Record<number, number>> = { 360: 560, 1280: 360 };

/** Opens the stress issue's Conversation at `width`, a minute after the seed as the layout rows
 *  hold the clock (`e2e/layout-check.ts`), and waits for its header and its tabs. */
async function openIssue(page: Page, scene: LayoutSeed, width: number): Promise<void> {
  await page.clock.setFixedTime(new Date(Date.parse(scene.seededAt) + 60_000));
  await page.setViewportSize({ height: 900, width });
  await page.goto(`/issues/${scene.issue}/conversation`);
  await pinFontFaces(page);
  const header = page.getByTestId("issue-header");
  // Drawn in full: the title, the claim and the labels.
  await expect(header.getByRole("heading", { level: 1 })).toBeVisible();
  await expect(header.getByTestId("issue-claim")).toBeVisible();
  await expect(header.locator('[data-layout-group="labels"] > *').first()).toBeVisible();
  await expect(page.getByRole("tablist", { name: "Issue detail" }).getByRole("tab")).toHaveCount(
    10
  );
}

test("today's issue header below its title, with Details closed, is at most 360 px tall at 1280 px and 560 at 360 @pr", async ({
  page,
  scene,
}, testInfo) => {
  const keep = await routeLayoutSockets(page, testInfo);
  try {
    const below: Record<number, number> = {};
    for (const width of [360, 1280]) {
      await openIssue(page, scene, width);
      const header = page.getByTestId("issue-header");
      await expect(issueDetailsButton(page)).toHaveAttribute("aria-expanded", "false");
      // The header's bottom less the title block's bottom, and where each group sits inside the
      // header (its top and height), for the record.
      const measured = await header.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const identity = element.querySelector('[data-testid="issue-identity"]');
        const groups: Record<string, string> = {
          actions: '[data-testid="issue-actions"]',
          attention: '[data-testid="issue-attention"]',
          identity: '[data-testid="issue-identity"]',
          labels: '[data-testid="issue-labels"]',
          state: '[data-testid="issue-state"]',
        };
        return {
          below:
            identity === null
              ? Number.POSITIVE_INFINITY
              : Math.round(box.bottom - identity.getBoundingClientRect().bottom),
          parts: Object.fromEntries(
            Object.entries(groups).map(([name, selector]) => {
              const part = element.querySelector(selector)?.getBoundingClientRect();
              return [
                name,
                part === undefined
                  ? null
                  : `${Math.round(part.top - box.top)}+${Math.round(part.height)}`,
              ];
            })
          ),
          total: Math.round(box.height),
        };
      });
      below[width] = measured.below;
      console.log(
        `LAYOUT issue-header at ${width}: total ${measured.total}, below the title ${measured.below}, ${JSON.stringify(measured.parts)}`
      );
      const shot = testInfo.outputPath(`issue-header-${width}.png`);
      await header.screenshot({ path: shot });
      await testInfo.attach(`issue header, Details closed (${width}px)`, {
        contentType: "image/png",
        path: shot,
      });
    }
    for (const width of [360, 1280]) {
      expect(below[width], `the header below its title at ${width} px`).toBeLessThanOrEqual(
        BELOW_TITLE_MAX[width] ?? 0
      );
    }
  } finally {
    keep();
  }
});

test("every issue tab lies inside the tab list and the viewport, and opens its panel, at 360 and 1280 px @pr", async ({
  page,
  scene,
}, testInfo) => {
  const keep = await routeLayoutSockets(page, testInfo);
  try {
    for (const width of [360, 1280]) {
      await openIssue(page, scene, width);
      const tablist = page.getByRole("tablist", { name: "Issue detail" });
      const tabs = tablist.getByRole("tab");
      const list = await tablist.boundingBox();
      expect(list, `the tab list's box at ${width} px`).not.toBeNull();
      const listLeft = list?.x ?? 0;
      const listRight = listLeft + (list?.width ?? 0);
      // `openIssue` waited for the count of ten, so one read lists every tab.
      for (const tab of await tabs.all()) {
        const box = await tab.boundingBox();
        const name = await tab.textContent();
        const left = box?.x ?? Number.NEGATIVE_INFINITY;
        const right = left + (box?.width ?? Number.POSITIVE_INFINITY);
        expect(left, `${name} at ${width} px starts inside the tab list`).toBeGreaterThanOrEqual(
          listLeft - 0.5
        );
        expect(right, `${name} at ${width} px ends inside the tab list`).toBeLessThanOrEqual(
          listRight + 0.5
        );
        expect(left, `${name} at ${width} px starts inside the viewport`).toBeGreaterThanOrEqual(0);
        expect(right, `${name} at ${width} px ends inside the viewport`).toBeLessThanOrEqual(width);
      }
      // The same ten tabs as above: a press changes the selected tab, never the list.
      for (const tab of await tabs.all()) {
        await tab.click();
        await expect(tab).toHaveAttribute("aria-selected", "true");
        const panel = page.locator(`[id="${await tab.getAttribute("aria-controls")}"]`);
        await expect(panel, `${await tab.textContent()}'s panel at ${width} px`).toBeVisible();
      }
    }
  } finally {
    keep();
  }
});
