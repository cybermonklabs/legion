import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { ruleDeclarations, topLevelBlocks, withoutComments } from "../../theme/css-source";
import { subsetScriptValue } from "./subset-script";

/**
 * `subset.sh` cuts InterVariable-subset.woff2 to the characters of its UNICODES, and the
 * `Dispatch Inter` face in styles.css repeats them as its `unicode-range`. The two lists are one
 * list: the spec makes a change to one a change to both. The script also holds the file it writes
 * to SUBSET_SHA256, so the committed file must be those bytes. A file replaced by hand, or a range
 * changed in one place alone, fails here.
 */

/** The declarations of each `@font-face` rule of `css`, by property. A `;` or a `}` inside a string
 * or a `url(…)` ends no declaration and no rule. */
function fontFaces(css: string): Map<string, string>[] {
  return topLevelBlocks(withoutComments(css))
    .filter(({ prelude }) => prelude === "@font-face")
    .map(
      ({ body }) =>
        new Map(ruleDeclarations(body).map(({ property, value }) => [property, value] as const))
    );
}

test("the shipped font file is the one subset.sh holds it to", () => {
  const expected = subsetScriptValue("SUBSET_SHA256");
  const font = readFileSync(join(import.meta.dir, "InterVariable-subset.woff2"));
  expect(createHash("sha256").update(font).digest("hex")).toBe(expected);
});

test("subset.sh cuts the characters the face's unicode-range names", () => {
  const face = fontFaces(
    readFileSync(join(import.meta.dir, "..", "..", "styles.css"), "utf8")
  ).find((faceDeclarations) => faceDeclarations.get("font-family") === '"Dispatch Inter"');
  if (face === undefined) {
    throw new Error('styles.css has no @font-face rule whose font-family is "Dispatch Inter"');
  }
  const range = face.get("unicode-range");
  if (!range) {
    throw new Error('the "Dispatch Inter" @font-face rule in styles.css has no unicode-range');
  }
  const [faceEntries, scriptEntries] = [range, subsetScriptValue("UNICODES")].map((list) =>
    list.split(",").map((entry) => entry.trim().toUpperCase())
  );
  expect(faceEntries).toEqual(scriptEntries);
});

test("a face is read past a ; or a } inside a string or a url", () => {
  const faces = fontFaces(
    '/* { */ @font-face { font-family: "Odd;}"; src: url(data:font/woff2;base64,AAAA) format("woff2"), url("a;b.woff2"); unicode-range: U+0000-00FF; } @font-face { font-family: "Plain"; unicode-range: U+0100 }'
  );
  expect(faces.map((face) => face.get("font-family"))).toEqual(['"Odd;}"', '"Plain"']);
  expect(faces[0]?.get("src")).toBe(
    'url(data:font/woff2;base64,AAAA) format("woff2"), url("a;b.woff2")'
  );
  expect(faces[0]?.get("unicode-range")).toBe("U+0000-00FF");
  expect(faces[1]?.get("unicode-range")).toBe("U+0100");
});
