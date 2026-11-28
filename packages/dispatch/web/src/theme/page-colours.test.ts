import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { type ColorPair, PAGE_COLORS } from "./classes";
import { oklchToSrgb8 } from "./contrast";
import { type CssBlock, ruleDeclarations, topLevelBlocks, withoutComments } from "./css-source";
import type { Swatch } from "./palette";

/**
 * The new issue page's colour tokens, held to the approved design's table ("The colours" in the
 * spec of dispatch://INFRA-3913). `styles-css-pin.test.ts` holds each literal of `styles.css` to
 * the swatch its comment names, so a token and its swatch that change together still pass it;
 * this test holds the value itself. The design has no dark scheme: each dark value is a Tailwind
 * shade of the same hue, and the four icon-rail tokens have none, because the rail is dark in both
 * schemes.
 *
 * It holds `PAGE_COLORS` of `classes.ts` to the same table: the contrast checks there measure the
 * swatch that list names for each token in each scheme, a copy made by hand, so each swatch must
 * render the token's value or the checks measure a colour the page does not draw.
 */

type DesignRow = readonly [token: string, light: string, dark?: string];

const DESIGN: readonly DesignRow[] = [
  ["--color-page-paper", "#ffffff", "#020618"],
  ["--color-page-ink", "#1c1917", "#f1f5f9"],
  ["--color-page-words", "#292524", "#e2e8f0"],
  ["--color-page-muted", "#57534e", "#cad5e2"],
  ["--color-page-quiet", "#78716c", "#90a1b9"],
  ["--color-page-line", "#eceae7", "#1d293d"],
  ["--color-page-hair", "#ddd8cf", "#314158"],
  ["--color-page-pane", "#fafaf9", "#0f172b"],
  ["--color-page-mark", "#efedea", "#1d293d"],
  ["--color-page-guide", "#e7e5e4", "#1d293d"],
  ["--color-page-bullet", "#a8a29e", "#62748e"],
  ["--color-page-question", "#2563eb", "#51a2ff"],
  ["--color-page-question-tint", "#eff6ff", "#162456"],
  ["--color-page-person", "#1c1917", "#f1f5f9"],
  ["--color-page-person-tint", "#f6eedf", "#292524"],
  ["--color-page-decision", "#7c3aed", "#a684ff"],
  ["--color-page-decision-tint", "#f5f3ff", "#2f0d68"],
  ["--color-page-statement", "#0f766e", "#00d5be"],
  ["--color-page-statement-tint", "#f0fdfa", "#022f2e"],
  ["--color-page-check", "#4338ca", "#7c86ff"],
  ["--color-page-check-tint", "#eef2ff", "#1e1a4d"],
  ["--color-page-step", "#b45309", "#ffb900"],
  ["--color-page-step-tint", "#fffbeb", "#461901"],
  ["--color-page-child", "#db2777", "#fb64b6"],
  ["--color-page-child-text", "#c6005c", "#fb64b6"],
  ["--color-page-child-tint", "#fdf2f8", "#510424"],
  ["--color-page-lens", "#a21caf", "#ed6aff"],
  ["--color-page-lens-tint", "#fdf4ff", "#4b004f"],
  ["--color-page-agent", "#64748b", "#90a1b9"],
  ["--color-page-issue", "#44403c", "#d6d3d1"],
  ["--color-page-waiting", "#ea580c", "#ff8904"],
  ["--color-page-waiting-tint", "#fff7ed", "#441306"],
  ["--color-page-waiting-text", "#c2410c", "#ffb86a"],
  ["--color-page-passed", "#15803d", "#05df72"],
  ["--color-page-failed", "#b91c1c", "#ff6467"],
  ["--color-page-not-run", "#6b7280", "#99a1af"],
  ["--color-page-in-progress", "#1d4ed8", "#51a2ff"],
  ["--color-icon-rail", "#0b1324"],
  ["--color-icon-rail-text", "#cbd5e1"],
  ["--color-icon-rail-accent", "#38bdf8"],
  ["--color-icon-rail-active", "#1f2636"],
];

const LIGHT = new Map(DESIGN.map(([token, light]) => [token, light] as const));
const DARK = new Map(
  DESIGN.flatMap(([token, , dark]) => (dark === undefined ? [] : [[token, dark] as const]))
);

const STYLES_CSS_PATH = join(import.meta.dir, "..", "styles.css");
const stylesCss = readFileSync(STYLES_CSS_PATH, "utf8");

/** A declaration of either prefix that the table does not hold fails the test. */
const TOKEN_NAME_PATTERN = /^--color-(?:page-|icon-rail)/;

/** Each declaration of `block` whose name starts `--color-page-` or `--color-icon-rail`. */
function tokenDeclarations(block: CssBlock): (readonly [name: string, value: string])[] {
  return ruleDeclarations(block.body)
    .filter(({ property }) => TOKEN_NAME_PATTERN.test(property))
    .map(({ property, value }) => [property, value] as const);
}

/**
 * One line for each way the token declarations of `blocks` differ from `expected`, one column of
 * the table. The design's tokens of one scheme are in one block, so a second block that declares
 * any of them is a difference too.
 */
function differences(
  scheme: "light" | "dark",
  blockName: string,
  blocks: readonly CssBlock[],
  expected: ReadonlyMap<string, string>
): string[] {
  const holders = blocks.map(tokenDeclarations).filter((declarations) => declarations.length > 0);
  if (holders.length === 0) {
    return [
      `styles.css has no ${blockName} block that declares a --color-page- or --color-icon-rail token`,
    ];
  }
  const problems: string[] = [];
  if (holders.length > 1) {
    problems.push(
      `styles.css declares the ${scheme} tokens in ${holders.length} ${blockName} blocks, the design has them in one`
    );
  }
  const found = new Map<string, string[]>();
  for (const [name, value] of holders.flat()) {
    found.set(name, [...(found.get(name) ?? []), value]);
  }
  for (const [name, value] of expected) {
    const values = found.get(name) ?? [];
    if (values.length === 0) {
      problems.push(`${name} (${scheme}): missing, the design has ${value}`);
    } else if (values.length > 1) {
      problems.push(
        `${name} (${scheme}): declared ${values.length} times (${values.join(", ")}), the design has ${value}`
      );
    } else if (values[0] !== value) {
      problems.push(`${name} (${scheme}): found ${values[0]}, the design has ${value}`);
    }
  }
  for (const [name, values] of found) {
    if (!expected.has(name)) {
      problems.push(
        `${name} (${scheme}): found ${values.join(", ")}, the design has no ${scheme} value for it`
      );
    }
  }
  return problems;
}

const topLevel = topLevelBlocks(withoutComments(stylesCss));
const themeBlocks = topLevel.filter((block) => /^@theme(?:\s|$)/.test(block.prelude));
/** The file's other dark `:root`, the pre-hydration fallback's `color` and `background`, declares
 * no token, so `differences` passes over it. */
const darkRootBlocks = topLevel
  .filter((block) => /^@media\s*\(\s*prefers-color-scheme\s*:\s*dark\s*\)$/.test(block.prelude))
  .flatMap((media) => topLevelBlocks(media.body))
  .filter((rule) => rule.prelude === ":root");

/** `#rrggbb` of `swatch`, by the palette's own conversion, as `styles-css-pin.test.ts` reads it. */
function hexOf(swatch: Swatch): string {
  const channels = oklchToSrgb8(swatch.oklch).map((channel) => channel.toString(16));
  return `#${channels.map((channel) => channel.padStart(2, "0")).join("")}`;
}

/**
 * One line for each token of the table whose swatch in `PAGE_COLORS` renders another value than
 * the table's, in either scheme, for each token `PAGE_COLORS` has no swatches for, and for each
 * key of `PAGE_COLORS` the table has no token for. A key of `PAGE_COLORS` is its token's name
 * without `--color-`. A token with no dark value is dark in both schemes, so its dark swatch
 * renders its light value.
 */
function paletteDifferences(): string[] {
  const problems: string[] = [];
  // A key read from the table's token may name no colour of `PAGE_COLORS`.
  const pageColors: Readonly<Partial<Record<string, ColorPair>>> = PAGE_COLORS;
  for (const [token, light, dark] of DESIGN) {
    const key = token.slice("--color-".length);
    const colors = pageColors[key];
    if (colors === undefined) {
      problems.push(`${token}: PAGE_COLORS has no "${key}", the design has ${light}`);
      continue;
    }
    for (const [scheme, swatch, value] of [
      ["light", colors.light, light],
      ["dark", colors.dark, dark ?? light],
    ] as const) {
      const rendered = hexOf(swatch);
      if (rendered !== value) {
        problems.push(
          `${token} (${scheme}): PAGE_COLORS gives ${swatch.name}, which is ${rendered}, the design has ${value}`
        );
      }
    }
  }
  for (const key of Object.keys(PAGE_COLORS)) {
    if (!LIGHT.has(`--color-${key}`)) {
      problems.push(`PAGE_COLORS has "${key}", which the design has no --color-${key} token for`);
    }
  }
  return problems;
}

test("every colour token of the new page has the value of the approved design", () => {
  const problems = [
    ...differences("light", "`@theme`", themeBlocks, LIGHT),
    ...differences("dark", "`@media (prefers-color-scheme: dark) { :root }`", darkRootBlocks, DARK),
    ...paletteDifferences(),
  ];
  expect(problems).toEqual([]);
});
