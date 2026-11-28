import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";

import { DRAWING_SURFACES } from "./plan-surfaces";

// The inventory of the pages that draw a planned change of an item they show (spec R4): the issue
// list, the board, the issue page, its Children tab and a document page. Each mounts the drawing
// (`PlannedChanges`) with the surface of its own kind, and no other page mounts one, so a page
// added to the list or dropped from it is seen here.

const WEB_SRC = join(import.meta.dir, "..", "..");

/** Every component file under `dir`, tests left out. */
function sources(dir: string): string[] {
  return readdirSync(dir, { recursive: true, encoding: "utf8" })
    .filter((path) => path.endsWith(".tsx") && !path.endsWith(".test.tsx"))
    .map((path) => join(dir, path));
}

test("everyDrawingSurfaceIsListed", () => {
  const mounting = sources(WEB_SRC)
    .filter((path) => !path.startsWith(join(WEB_SRC, "features", "going")))
    .filter((path) => /<PlannedChanges\b/.test(readFileSync(path, "utf8")))
    .map((path) => relative(WEB_SRC, path))
    .sort();
  expect(mounting).toEqual(DRAWING_SURFACES.map((surface) => surface.file).sort());
  for (const surface of DRAWING_SURFACES) {
    const source = readFileSync(join(WEB_SRC, surface.file), "utf8");
    // The surface the page gives the drawing: the object literal after `<PlannedChanges`.
    const drawing = source.slice(source.indexOf("<PlannedChanges")).split("/>")[0] ?? "";
    expect({ kind: drawing.match(/kind: "(\w+)"/)?.[1], page: surface.page }).toEqual({
      kind: surface.kind,
      page: surface.page,
    });
  }
});
