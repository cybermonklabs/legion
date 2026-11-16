import type { BrowserContext } from "@playwright/test";

/**
 * Turns the new issue page on for `login` in `context`: an init script stores `outline` under
 * that login's `issue.page` preference, which `useIssuePageChoice`
 * (`web/src/features/issue-page/choice.ts`) reads.
 *
 * The script runs before any page script of every later document of the context, so it works on
 * the default `context`, on `asUser` and `browser.newContext` contexts, and on `withAlice`'s page
 * through `page.context()`, called before the page's first `goto`. It stores `outline` again at
 * every load, so a row that tests turning the setting off does not call it. It lowercases the
 * login because Dispatch names a person in lowercase: a row that passed `Alice` would otherwise
 * get today's page with no error.
 *
 * The key is written as a literal, as other e2e files write storage keys, so a change of the key
 * in the code fails `issue-page-switch.e2e.ts`'s first row. The `setItem` is inside a `try`
 * because the script runs in every frame of the page, and a frame with no storage would
 * otherwise raise a page error.
 */
export async function withIssuePage(context: BrowserContext, login: string): Promise<void> {
  await context.addInitScript(
    ({ key }) => {
      try {
        window.localStorage.setItem(key, "outline");
      } catch {
        // This frame has no storage of its own, and the page reads none from it.
      }
    },
    { key: `dispatch.issue.page:${login.toLowerCase()}` }
  );
}
