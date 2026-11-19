import type { Page } from "@playwright/test";

/**
 * How a row reads the typeface the new issue page draws in: `e2e/issue-page-look.e2e.ts` and
 * `e2e/issue-page-j.e2e.ts` both read it. It is a file of its own, holding no test, because
 * Playwright refuses a test file that imports another one.
 */

/** The family `web/src/styles.css` declares for the typeface the app ships and the new issue
 *  page draws in. */
export const TYPEFACE = "Dispatch Inter";

/** The address the page's stylesheet loads the typeface from, read from its own `@font-face`
 *  rule; null when no rule declares the family. c-typeface. */
export function typefaceAddress(page: Page): Promise<string | null> {
  return page.evaluate((family) => {
    const find = (rules: CSSRuleList, base: string): string | null => {
      for (const rule of rules) {
        if (rule instanceof CSSFontFaceRule) {
          const named = rule.style.getPropertyValue("font-family").replace(/["']/g, "").trim();
          const source = /url\(["']?([^"')]+)["']?\)/.exec(rule.style.getPropertyValue("src"));
          if (named === family && source?.[1] !== undefined) return new URL(source[1], base).href;
        } else if (rule instanceof CSSGroupingRule) {
          const found = find(rule.cssRules, base);
          if (found !== null) return found;
        }
      }
      return null;
    };
    for (const sheet of document.styleSheets) {
      const found = find(sheet.cssRules, sheet.href ?? document.baseURI);
      if (found !== null) return found;
    }
    return null;
  }, TYPEFACE);
}

/** How each face of the typeface's family stands in the document's font set: `loaded` once the
 *  file arrived, `error` once its request failed. c-typeface. */
export function typefaceStatuses(page: Page): Promise<string[]> {
  return page.evaluate(
    (family) =>
      [...document.fonts]
        .filter((face) => face.family.replace(/["']/g, "") === family)
        .map((face) => face.status),
    TYPEFACE
  );
}
