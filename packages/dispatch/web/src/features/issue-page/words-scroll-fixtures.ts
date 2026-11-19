import { readFileSync } from "node:fs";
import { join } from "node:path";

/** One rule of `styles.css`: its selector list, kept whole, since a comma inside `:is(…)`
 *  separates no selectors, and its declarations, each written `property: value`. */
export interface StyleRule {
  readonly declared: ReadonlySet<string>;
  readonly selectors: string;
}

/**
 * The rules of `styles.css` that give a block in a node's words a sideways scroll of its own
 * (decision 282): each names `[data-node-words]` and declares `overflow-x: auto`. A unit test's
 * DOM applies the rules a test gives it, and `getComputedStyle` reads them back, but it never
 * loads `styles.css`: a case that reads what these rules do appends them itself
 * (`wordsScrollSheet`). Throws where `styles.css` holds none, so no case passes on no rules.
 */
export function wordsScrollRules(): StyleRule[] {
  const css = readFileSync(join(import.meta.dir, "..", "..", "styles.css"), "utf8").replace(
    /\/\*[\s\S]*?\*\//g,
    ""
  );
  const rules = [...css.matchAll(/([^{};]+)\{([^{}]*)\}/g)].flatMap(
    ([, selectors = "", body = ""]) => {
      const declared = new Set(
        body
          .split(";")
          .map((one) => one.replace(/\s+/g, " ").trim())
          .filter(Boolean)
      );
      return selectors.includes("[data-node-words]") && declared.has("overflow-x: auto")
        ? [{ declared, selectors: selectors.trim() }]
        : [];
    }
  );
  if (rules.length === 0)
    throw new Error("no rule of styles.css scrolls a block in a node's words");
  return rules;
}

/** `wordsScrollRules` as one style sheet, for the test to append to the document and remove. */
export function wordsScrollSheet(): HTMLStyleElement {
  const sheet = document.createElement("style");
  sheet.textContent = wordsScrollRules()
    .map(({ declared, selectors }) => `${selectors} { ${[...declared].join("; ")}; }`)
    .join("\n");
  return sheet;
}
