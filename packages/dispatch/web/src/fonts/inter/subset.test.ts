import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * `subset.sh` cuts InterVariable-subset.woff2 to the characters of its UNICODES, and the
 * `Dispatch Inter` face in styles.css repeats them as its `unicode-range`. The two lists are one
 * list: the spec makes a change to one a change to both. The script also holds the file it writes
 * to SUBSET_SHA256, so the committed file must be those bytes. A file replaced by hand, or a range
 * changed in one place alone, fails here.
 */

const subsetSh = readFileSync(join(import.meta.dir, "subset.sh"), "utf8");

function scriptValue(name: string): string {
  const match = new RegExp(`^${name}=(\\S+)$`, "m").exec(subsetSh);
  if (match === null) {
    throw new Error(`subset.sh has no ${name}= line`);
  }
  return match[1];
}

function declarations(body: string): Map<string, string> {
  const result = new Map<string, string>();
  for (const declaration of body.split(";")) {
    const colon = declaration.indexOf(":");
    if (colon !== -1) {
      result.set(declaration.slice(0, colon).trim(), declaration.slice(colon + 1).trim());
    }
  }
  return result;
}

test("the shipped font file is the one subset.sh holds it to", () => {
  const expected = scriptValue("SUBSET_SHA256");
  const font = readFileSync(join(import.meta.dir, "InterVariable-subset.woff2"));
  expect(createHash("sha256").update(font).digest("hex")).toBe(expected);
});

test("subset.sh cuts the characters the face's unicode-range names", () => {
  const stylesCss = readFileSync(join(import.meta.dir, "..", "..", "styles.css"), "utf8").replace(
    /\/\*[\s\S]*?\*\//g,
    ""
  );
  const face = [...stylesCss.matchAll(/@font-face\s*\{([^}]*)\}/g)]
    .map((match) => declarations(match[1]))
    .find((faceDeclarations) => faceDeclarations.get("font-family") === '"Dispatch Inter"');
  if (face === undefined) {
    throw new Error('styles.css has no @font-face rule whose font-family is "Dispatch Inter"');
  }
  const range = face.get("unicode-range");
  if (!range) {
    throw new Error('the "Dispatch Inter" @font-face rule in styles.css has no unicode-range');
  }
  const [faceEntries, scriptEntries] = [range, scriptValue("UNICODES")].map((list) =>
    list.split(",").map((entry) => entry.trim().toUpperCase())
  );
  expect(faceEntries).toEqual(scriptEntries);
});
