import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { templateExpressions } from "./css-source";

/**
 * The mechanical half of the dark-mode palette contract: `classes.ts` is the only place a
 * component-facing color className gets built, and `palette.test.ts` verifies every role it
 * exports. Neither guarantees anything about the *components* unless nothing outside `theme/`
 * can write a color utility literal directly — this test is that guarantee. A component that
 * writes `className="bg-white ..."` instead of importing `surfaceBg` from `classes.ts` compiles
 * fine and fails only here. Scans every `.ts`/`.tsx` source file in `web/src` (outside `theme/`)
 * and every `.ts` file in `e2e/` — Playwright specs and helpers are just as capable of writing
 * a raw Tailwind color utility (e.g. as a `page.locator` selector or a `toHaveClass` pattern) as
 * a component is.
 */
const COLOR_FAMILIES = [
  "slate",
  "sky",
  "rose",
  "amber",
  "emerald",
  "red",
  "orange",
  "yellow",
  "lime",
  "green",
  "teal",
  "cyan",
  "blue",
  "indigo",
  "violet",
  "purple",
  "fuchsia",
  "pink",
  "gray",
  "zinc",
  "neutral",
  "stone",
] as const;

/** Numbered shades (`bg-sky-700`) plus the three unnumbered keyword colors (`text-white`,
 * `bg-black`, `border-transparent`) — all three are just as capable of drifting from the
 * documented palette/composite layer as a numbered shade is. */
const RAW_COLOR_PATTERN = new RegExp(
  `\\b(?:text|bg|border|ring|placeholder|divide|outline|caret|accent|decoration|from|via|to)-(?:(?:${COLOR_FAMILIES.join("|")})-\\d{2,3}|white|black|transparent)\\b`,
  "g"
);

const WEB_SRC_ROOT = join(import.meta.dir, "..");
const E2E_ROOT = join(import.meta.dir, "..", "..", "..", "e2e");

function collectSourceFiles(dir: string, root: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (dir === root && entry.name === "theme") {
      continue;
    }
    const entryPath = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...collectSourceFiles(entryPath, root));
      continue;
    }
    const isSource = entry.name.endsWith(".ts") || entry.name.endsWith(".tsx");
    const isUnitTest = entry.name.endsWith(".test.ts") || entry.name.endsWith(".test.tsx");
    if (isSource && !isUnitTest) {
      files.push(entryPath);
    }
  }
  return files;
}

const SCANNED_FILES = [
  ...collectSourceFiles(WEB_SRC_ROOT, WEB_SRC_ROOT).map(
    (file) => [join("web/src", file.slice(WEB_SRC_ROOT.length + 1)), file] as const
  ),
  ...collectSourceFiles(E2E_ROOT, E2E_ROOT).map(
    (file) => [join("e2e", file.slice(E2E_ROOT.length + 1)), file] as const
  ),
];

/** What a scan found: the text, and the line of the source it starts on, counted from 1. */
interface Offender {
  readonly line: number;
  readonly text: string;
}

/** The line of `source` that its character `offset` is on. */
function lineAt(source: string, offset: number): number {
  let line = 1;
  for (let at = source.indexOf("\n"); at !== -1 && at < offset; at = source.indexOf("\n", at + 1)) {
    line++;
  }
  return line;
}

/** Where a string is written over two lines: after a backslash that ends a line inside it, or
 * between its closing quote, a `+` and the opening quote of the string that goes on. */
const SPLIT_STRING_JOIN = /\\\r?\n|["'`]\s*\+\s*["'`]/g;

/**
 * Each raw colour utility that `source` writes, with the line it starts on. `source` is read whole
 * and its comments are read too, since Tailwind's scanner reads them. A class name that is written
 * over two lines, `"bg-" +` and `"white"` below it, or `"bg-\` ending the line and `white"` on the
 * next, is found as the one class its pieces make, beside every class that is whole in the source.
 */
function rawColorOffenders(source: string): Offender[] {
  const found = [...source.matchAll(RAW_COLOR_PATTERN)].map((match) => ({
    offset: match.index,
    text: match[0],
  }));
  const cuts: { readonly at: number; readonly length: number }[] = [];
  let joined = "";
  let from = 0;
  for (const join of source.matchAll(SPLIT_STRING_JOIN)) {
    joined += source.slice(from, join.index);
    cuts.push({ at: joined.length, length: join[0].length });
    from = join.index + join[0].length;
  }
  // With no string split, the joined source is the source, and every class in it is found above.
  if (cuts.length > 0) {
    joined += source.slice(from);
    for (const match of joined.matchAll(RAW_COLOR_PATTERN)) {
      const end = match.index + match[0].length;
      if (cuts.some(({ at }) => match.index < at && at < end)) {
        const cut = cuts.filter(({ at }) => at <= match.index);
        found.push({
          offset: match.index + cut.reduce((sum, { length }) => sum + length, 0),
          text: match[0],
        });
      }
    }
  }
  return found
    .sort((a, b) => a.offset - b.offset)
    .map(({ offset, text }) => ({ line: lineAt(source, offset), text }));
}

describe("no raw Tailwind color utilities outside theme/", () => {
  test("scans a non-trivial number of source files", () => {
    // A regression guard on the scan itself: if this collapsed to 0, every check below would
    // vacuously pass without covering anything.
    expect(SCANNED_FILES.length).toBeGreaterThan(20);
  });

  test.each(SCANNED_FILES)("%s has no raw color utility literal", (_relativePath, file) => {
    expect(rawColorOffenders(readFileSync(file, "utf8"))).toEqual([]);
  });

  test("finds a class written over two lines, and one in a comment", () => {
    const source = [
      'const joined = "bg-" +',
      '  "white";',
      'const continued = "px-2 text-\\',
      'slate-700";',
      "// border-sky-500 stays a literal in a comment",
      'const whole = "ring-black";',
      'const fine = "bg-page-paper";',
    ].join("\n");
    expect(rawColorOffenders(source)).toEqual([
      { line: 1, text: "bg-white" },
      { line: 3, text: "text-slate-700" },
      { line: 5, text: "border-sky-500" },
      { line: 6, text: "ring-black" },
    ]);
  });
});

/**
 * What the two tests above guard against: a class name built at runtime by concatenating a
 * prefix onto a swatch name (`` `${prefix}-${swatch.name}` ``) never appears as literal text in
 * any source file, so Tailwind's scanner cannot see it and silently drops it from the built
 * stylesheet (see `classes-in-build-css.ts`). This rejects the construction pattern itself
 * — a hyphen glued directly onto a template-literal expression, with no whitespace between them
 * — inside `classes.ts`, the one file allowed to compose Tailwind classes at all. A comment is no
 * code, so this test can describe the pattern in prose without failing on itself.
 *
 * Each template expression of `source` that has a hyphen glued onto it, before its `${` or after
 * its `}`, with the line the glued text starts on. The scanner that knows strings, comments and
 * templates finds where each expression ends, balanced against the braces and templates inside
 * it, so an expression of many lines or with `{ … }` in it is one expression.
 */
function dynamicClassOffenders(source: string): Offender[] {
  return templateExpressions(source).flatMap(({ start, end }) => {
    const before = source[start - 1] === "-";
    const after = source[end] === "-";
    if (!before && !after) {
      return [];
    }
    const expression = source.slice(start, end).replace(/\s+/g, " ");
    return [
      {
        line: lineAt(source, before ? start - 1 : start),
        text: `${before ? "-" : ""}${expression}${after ? "-" : ""}`,
      },
    ];
  });
}

describe("no dynamic Tailwind class construction in classes.ts", () => {
  test("classes.ts never glues a hyphen directly onto a template expression", () => {
    expect(
      dynamicClassOffenders(readFileSync(join(WEB_SRC_ROOT, "theme", "classes.ts"), "utf8"))
    ).toEqual([]);
  });

  test("finds a hyphen glued onto an expression written over two lines, and reads no comment", () => {
    const source = [
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
      "// a comment may write `${prefix}-${name}` in prose",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
      "/* and so may a block one: `${prefix}-` */",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
      "const address = `https://example.test/${path}-x`;",
      "const spread = `${",
      "  active",
      "    ? primary",
      "    : secondary",
      "}-500`;",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
      "const braces = `${f({ a })}-500`;",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
      "const inner = `${active ? `x-${name}` : secondary}`;",
    ].join("\n");
    expect(dynamicClassOffenders(source)).toEqual([
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the text of the source's expression, not a placeholder.
      { line: 3, text: "${path}-" },
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the text of the source's expression, not a placeholder.
      { line: 4, text: "${ active ? primary : secondary }-" },
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the text of the source's expression, not a placeholder.
      { line: 9, text: "${f({ a })}-" },
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the text of the source's expression, not a placeholder.
      { line: 10, text: "-${name}" },
    ]);
  });
});
