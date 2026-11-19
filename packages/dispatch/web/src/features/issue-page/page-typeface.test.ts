import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";
import { subsetRanges } from "../../fonts/inter/subset-script";

/**
 * The page draws its own words in its typeface, the cut of Inter that `fonts/inter/subset.sh`
 * writes, whose `unicode-range` is that script's UNICODES (`fonts/inter/subset.test.ts`). A
 * character outside UNICODES is drawn in a face of the reader's own machine, at that face's
 * width, or in none. So every string, template and JSX text of the page's own source - each file
 * of this directory but its tests and fixtures, `page-words.ts` among them - holds only
 * characters UNICODES keeps. A comment is not text the page draws, and neither is a regular
 * expression.
 *
 * A shape the page draws as an inline svg is named by a character it never draws as text:
 * `SHAPE_NAMES` lists each such character with the files that name it, and is held to them.
 */

/** `issue-status.ts`'s `statusGlyph` names a status's shape by a character, which `StatusGlyph`
 *  draws as an svg with no text; the page's typeface lacks ◐. */
const SHAPE_NAMES: Readonly<Record<string, readonly string[]>> = {
  "◐": ["StatusGlyph.tsx", "issue-status.ts"],
};

const ranges = subsetRanges();

/** A character of the page's text that UNICODES does not keep, and where it stands. */
interface Finding {
  character: string;
  line: number;
  path: string;
}

/** Each character UNICODES does not keep in the strings, template parts and JSX text of
 *  `source`, the text of the file at `path`, with the line its text starts on. The parser's tree
 *  holds no comment - a line comment, a block comment, or JSX's braced one - so none is read,
 *  and a regular expression is a node of another kind. */
function charactersOutside(path: string, source: string): Finding[] {
  const file = ts.createSourceFile(
    path,
    source,
    ts.ScriptTarget.Latest,
    true,
    path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  );
  const found: Finding[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isStringLiteral(node) || ts.isTemplateLiteralToken(node) || ts.isJsxText(node)) {
      const line = file.getLineAndCharacterOfPosition(node.getStart()).line + 1;
      for (const character of node.text) {
        const point = character.codePointAt(0) ?? 0;
        if (!ranges.some(([first, last]) => first <= point && point <= last)) {
          found.push({ character, line, path });
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return found;
}

/** A finding as a failure lists it: its file and line, the character and its code point. */
function reading({ character, line, path }: Finding): string {
  const point = (character.codePointAt(0) ?? 0).toString(16).toUpperCase().padStart(4, "0");
  return `${path}:${line}: ${character} (U+${point})`;
}

const pageFiles = readdirSync(import.meta.dir, { encoding: "utf8", recursive: true })
  .filter((path) => /\.tsx?$/.test(path) && !/\.test\.tsx?$/.test(path) && !/fixture/.test(path))
  .sort();

const outside: string[] = [];
const shapesNamed = new Set<string>();
for (const path of pageFiles) {
  const source = readFileSync(join(import.meta.dir, path), "utf8");
  for (const finding of charactersOutside(path, source)) {
    if (SHAPE_NAMES[finding.character]?.includes(path)) {
      shapesNamed.add(`${finding.character} ${path}`);
    } else {
      outside.push(reading(finding));
    }
  }
}

test("the page's own text holds only characters its typeface keeps", () => {
  expect(pageFiles).toContain("page-words.ts");
  expect(outside).toEqual([]);
});

test("each character SHAPE_NAMES lists still names a shape in each file it lists", () => {
  expect([...shapesNamed].sort()).toEqual(
    Object.entries(SHAPE_NAMES)
      .flatMap(([character, paths]) => paths.map((path) => `${character} ${path}`))
      .sort()
  );
});

test("a character named only in a comment is not the page's text, and one in a string is", () => {
  const source = [
    "// ⋯ names the row's menu, which is drawn as an svg",
    "/* ⋯ again, in a block comment */",
    'export const MORE = "⋯";',
    "export const Menu = () => (",
    '  <button type="button">',
    "    {/* ⋯ is drawn as an svg here too */}",
    "  </button>",
    ");",
  ].join("\n");
  expect(charactersOutside("inline.tsx", source).map(reading)).toEqual([
    "inline.tsx:3: ⋯ (U+22EF)",
  ]);
});
