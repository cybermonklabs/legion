/** What the palette rows open before they press a key, shared by `keyboard-palette.e2e.ts` and
 *  the rows of that family which run against a recording (`keyboard-palette.stub.e2e.ts`), and the
 *  key a page's `$mod` is (`modKey`), which every row that opens the bar by `$mod+k` in a project
 *  whose user agent names a Mac presses. */
import { expect, type Page } from "@playwright/test";

// The keymap is bound only once sign-in resolves (`AuthGate` renders a skeleton until
// `/auth/whoami` answers), so a key pressed before the page renders reaches no handler.
export async function openIssue(page: Page, issueKey: string, title: string): Promise<void> {
  await page.goto(`/issues/${issueKey}`);
  await expect(
    page.getByTestId("issue-header").getByRole("heading", { level: 1, name: title })
  ).toBeVisible();
  await page.locator("body").focus();
}

// Both paths the palette's searches take. The keystroke tier always carries `keywords_only`, and
// a palette opened on a project or an issue page carries `number_project`, so either request goes
// to `/api/v1/search/results` rather than `/api/v1/search` (`everyBuildReadsSearch`,
// `@legion/contracts`). A route matching one path alone would let the other through, and a stub
// or a hold in either file would miss the request it is about.
export const searchPath = (url: URL): boolean =>
  url.pathname === "/api/v1/search" || url.pathname === "/api/v1/search/results";

/** The key a page's `$mod` is, read from that page's own user agent as the app reads it (`IS_MAC`,
 *  `web/src/features/shell/keymap.ts`): `Meta` (Cmd) where it names a Mac, which the `webkit`
 *  project's Desktop Safari and the phone projects' iPhone 13 both do, and `Control` elsewhere.
 *  Playwright's `ControlOrMeta` cannot stand in for it: that picks by the machine the browser runs
 *  on, not by the user agent the page reads. */
export async function modKey(page: Page): Promise<"Control" | "Meta"> {
  // One read is the answer: a context's user agent is set before the page loads and never changes.
  return (await page.evaluate(() => navigator.userAgent.includes("Mac"))) ? "Meta" : "Control";
}
