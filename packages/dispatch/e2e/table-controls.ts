/** The readings the rows of `table-controls.e2e.ts` share, so the rows name one page and one
 *  narrowing each, and a page whose controls move is edited here once.
 *
 *  Nothing here asserts; each function hands back what a row reads, and the row says what it
 *  expects of it. The one exception is the waiting each one does, which is how it knows the
 *  control has done what it was asked.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL } from "../web/src/components/control-names";
import { control, controlRow } from "./filters";
import { expandedState } from "./settled";

/** Whether the row is running in one of the suite's phone projects (`iphone`, `stub-phone`),
 *  where R1's third width is that project's own and the row sets no viewport of its own. */
export function isPhoneProject(testInfo: { project: { name: string } }): boolean {
  return testInfo.project.name.includes("phone");
}

/**
 * Shuts the surface's own filter panel by pressing its trigger again, which is what a reader
 * does and the one way that is safe everywhere: `AnchoredPanel` takes Escape only while the
 * focus is inside the panel or on its anchor, so an Escape pressed after a pick whose control
 * moved the focus elsewhere travels on to the layer behind - on the search bar, to the bar
 * itself, which then closes.
 */
export async function closeControlPanel(within: Locator | Page): Promise<void> {
  const trigger = control(controlRow(within), CONTROL.panelTrigger).first();
  if ((await expandedState("the row's filter trigger", trigger)) === "open") await trigger.click();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
}
