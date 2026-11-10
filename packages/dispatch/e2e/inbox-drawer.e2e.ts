import { expect, test } from "@playwright/test";

import { getAsk, setAgentStreamResponder } from "./api";
import { attachShot, drawer, seedAsk } from "./inbox-drawer";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox as a drawer over any page (LEGION-547), in the row whose claim reaches the server:
 * a live conversation with a half-typed message under a drawer in which an ask is answered, the
 * answer read back from the server and the draft still there afterwards. Where the drawer opens
 * from, what it covers and the key that toggles it are in `inbox-drawer.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("a half-typed message in a live conversation survives opening the drawer, answering an ask in it and closing it", async ({
  browser,
}, testInfo) => {
  const { askId } = await seedAsk("Ship the drawer behind a flag?");
  const sessionID = "01a0e0c1-0000-7000-8000-0000000d4a3e";
  await setAgentStreamResponder(sessionID, true);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    const composer = page.getByTestId("agent-composer").locator("textarea");
    await composer.fill("half a thought about the rollout");
    // `i` is a page key, so it never fires from inside a text field.
    await composer.blur();

    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    const card = drawer(page).getByTestId(`ask-${askId}`);
    await expect(card).toBeVisible();
    await attachShot(page, testInfo, "inbox-drawer-over-agent-conversation");

    await card.getByRole("radio", { name: "Yes" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(card).toHaveCount(0);
    await expect
      .poll(() => getAsk(askId))
      .toMatchObject({ ask: { answer: { selected: ["Yes"], user: "alice" }, state: "answered" } });

    await page.keyboard.press("Escape");
    await expect(drawer(page)).toHaveCount(0);
    // The conversation underneath was never unmounted: same route, same draft.
    await expect(page).toHaveURL(new RegExp(`/agents/${sessionID}/live$`));
    await expect(composer).toHaveValue("half a thought about the rollout");
  } finally {
    await context.close();
  }
});
