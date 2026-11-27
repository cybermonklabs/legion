import { PRESET_ORDER } from "../web/src/features/issue-page/page-words";
import { DESKTOP } from "./issue-page-g";
import { readScaleLoad, unheldScaleBounds } from "./issue-page-n";
import { withIssuePage } from "./issue-page-option";
import { IPHONE_13 } from "./issue-page-p";
import { type ScaleCensusKey, seedScaleCopy } from "./scale-seed";
import { type Browser, expect, test } from "./test";
import { asUser } from "./users";

/** Seeds `census`'s copy and reads a cold load under every control of the view row
 *  (`PRESET_ORDER`) at the active project's width (1440 px on chromium, 390 px on iphone). */
export async function runScaleCopy(browser: Browser, census: ScaleCensusKey): Promise<void> {
  const viewport = test.info().project.name === "iphone" ? IPHONE_13 : DESKTOP;
  expect(
    unheldScaleBounds(),
    "R21's bounds that this row reads nowhere and AWAITING_SCALE does not name: the pull request that takes a bound off the list adds its reading"
  ).toEqual([]);
  const { copy, reader } = await seedScaleCopy(census);
  // A read that two controls' loads make is timed once: its answer does not change with the
  // control that asked.
  const timed = new Set<string>();
  for (const preset of PRESET_ORDER) {
    const context = await asUser(browser, reader);
    try {
      await withIssuePage(context, reader);
      const page = await context.newPage();
      await page.setViewportSize(viewport);
      await readScaleLoad(page, copy, preset, reader, timed);
    } finally {
      await context.close();
    }
  }
}
