// The Evals tab (`/projects/<KEY>/evals`, dispatch://INFRA-2918), which replaced the Delivery and
// Pipeline pages, as the browser rows open it: by the locators its components draw
// (`web/src/features/evals/`), never by a page heading that no longer exists. The delivery
// fixtures of `delivery-seed.ts` are project ACME's, so the rows open ACME's tab.
import { expect, type Locator, type Page } from "./test";

/** The project the delivery fixtures (`delivery-seed.ts`) seed. */
export const DELIVERY_PROJECT = "ACME";

/** The address of `project`'s Evals tab, with `query` (no leading "?") as its URL state: the
 *  window (`window`, or `from` and `to`), the merge timeline's facets, search and `mode`. */
export function evalsTabPath(query = "", project = DELIVERY_PROJECT): string {
  return `/projects/${project}/evals${query === "" ? "" : `?${query}`}`;
}

/** Waits for the tab to be the project page's selected tab (`ProjectPage.tsx`). */
export async function expectEvalsTab(page: Page): Promise<void> {
  await expect(page.getByRole("tab", { name: "Evals" })).toHaveAttribute("aria-selected", "true");
}

/** The tab's section `id` (`data-section`: `cards`, `legion`, `ci`, `deploys`, `timeline`,
 *  `logged`), which `EvalsSection.tsx` and `TimelineSection.tsx` draw. */
export function evalsSection(page: Page, id: string): Locator {
  return page.locator(`[data-section="${id}"]`);
}

/** Waits for section `id` to paint and draw: every view it reads has answered (`data-state`),
 *  and its charts are drawn, not the box of their size it shows below the first screen until the
 *  page draws them (`data-drawn`, `EvalsSection.tsx`). */
export async function paintedSection(page: Page, id: string): Promise<Locator> {
  const section = evalsSection(page, id);
  await expect(section).toHaveAttribute("data-state", "painted");
  await expect(section).not.toHaveAttribute("data-drawn", "false");
  return section;
}

/** Every value `scope`'s graphs draw, as each names it: a node of one value's `data-value`, and
 *  each value a group of a whole range names in its `data-values` (the per-day charts, whose days
 *  one path draws, `features/delivery/Sparkbars.tsx`'s `valueList`). */
export function graphValues(scope: Locator): Promise<string[]> {
  return scope
    .locator("[data-value], [data-values]")
    .evaluateAll((nodes) =>
      nodes.flatMap((node) =>
        node.hasAttribute("data-value")
          ? [node.getAttribute("data-value") ?? ""]
          : (node.getAttribute("data-values") ?? "").split(" ").filter((value) => value !== "")
      )
    );
}

/** Opens the merge timeline, which the tab keeps closed on its first paint (spec R13), and waits
 *  for its project-timeline view's first snapshot to paint it. */
export async function openMergeTimeline(page: Page): Promise<Locator> {
  const timeline = evalsSection(page, "timeline");
  await timeline.getByRole("button", { name: "Show the merge timeline" }).click();
  await expect(timeline).toHaveAttribute("data-state", "painted");
  return timeline;
}

/** Opens `project`'s Evals tab at `query` and its merge timeline, as the Delivery page opened
 *  on its timeline: `mode=list` in `query` shows the merged pull request list. */
export async function openEvalsTimeline(
  page: Page,
  query = "",
  project = DELIVERY_PROJECT
): Promise<Locator> {
  await page.goto(evalsTabPath(query, project));
  await expectEvalsTab(page);
  return openMergeTimeline(page);
}
