import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  budgetReadings,
  entryChunk,
  MAX_ASSET_BYTES,
  MAX_ENTRY_BYTES,
  oversizedAssets,
} from "../asset-sizes-in-build";

const dirs: string[] = [];

afterEach(() => {
  for (const dir of dirs.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

function assetsDir(files: Record<string, number>): string {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-assets-"));
  dirs.push(dir);
  for (const [name, bytes] of Object.entries(files)) {
    const file = join(dir, name);
    mkdirSync(join(file, ".."), { recursive: true });
    writeFileSync(file, new Uint8Array(bytes));
  }
  return dir;
}

test("a build whose every file fits the server's limit passes", () => {
  const dir = assetsDir({
    "index-a.js": MAX_ASSET_BYTES,
    "lib-a.js": 1_747_955,
    "fonts/inter.woff2": 1,
  });
  expect(oversizedAssets(dir)).toEqual([]);
});

test("a file one byte over the limit, at any depth, is named with its size", () => {
  const dir = assetsDir({
    "index-a.js": 10,
    "huge-a.js": MAX_ASSET_BYTES + 1,
    "nested/huge-b.wasm": 9 * 1024 * 1024,
  });
  expect(oversizedAssets(dir)).toEqual([
    { path: "huge-a.js", bytes: MAX_ASSET_BYTES + 1 },
    { path: "nested/huge-b.wasm", bytes: 9 * 1024 * 1024 },
  ]);
});

/**
 * A build directory of the shape `bun run build:web` writes: an `index.html` carrying the
 * inline chunk-failure script (which has no `src`), the module script naming the entry, and the
 * stylesheet after it, with the files it names below `assets/`.
 */
function distDir(entry: string, files: Record<string, number>): string {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-dist-"));
  dirs.push(dir);
  writeFileSync(
    join(dir, "index.html"),
    [
      "<!doctype html>",
      '<html lang="en">',
      "  <head>",
      "    <script>",
      "      (() => { window.addEventListener('error', () => {}); })();",
      "    </script>",
      `    <script type="module" crossorigin src="${entry}"></script>`,
      '    <link rel="stylesheet" crossorigin href="/assets/index-CV-Ii6LJ.css">',
      "  </head>",
      '  <body><div id="root"></div></body>',
      "</html>",
    ].join("\n")
  );
  for (const [name, bytes] of Object.entries(files)) {
    const file = join(dir, name);
    mkdirSync(join(file, ".."), { recursive: true });
    writeFileSync(file, new Uint8Array(bytes));
  }
  return dir;
}

test("a build whose entry is one byte over the budget is refused, and the refusal names it", () => {
  // The arm that refuses, which is the one `main` exits 1 on: the reading says the entry's
  // bytes, the budget and why that chunk is the one held to it. Deleting the comparison in
  // `budgetReadings` leaves a build CI waves through, and fails this row.
  const dir = distDir("/assets/index-080_OX6C.js", {
    "assets/index-080_OX6C.js": MAX_ENTRY_BYTES + 1,
    "assets/mermaid-render.worker-zV4qaloj.js": 1_565_608,
  });
  expect(entryChunk(dir)).toEqual({
    path: "assets/index-080_OX6C.js",
    bytes: MAX_ENTRY_BYTES + 1,
  });
  const refusals = budgetReadings(dir).filter((reading) => reading.refused);
  expect(refusals.map((reading) => reading.message)).toEqual([
    `web/dist/assets/index-080_OX6C.js is ${MAX_ENTRY_BYTES + 1} bytes, over the ${MAX_ENTRY_BYTES}-byte entry budget: it is the chunk web/dist/index.html names in its module script, so every page of the dashboard parses all of it before it draws anything, and a new browser context has none of it cached.`,
  ]);
});

test("a build whose entry is the budget exactly passes, and the larger chunks beside it are not the entry", () => {
  // The arm that passes, and what keeps this budget the entry's alone: the Mermaid worker and
  // the ECharts chunk are larger than the entry and no landing fetches either, so the reading
  // takes the file `index.html` names and not the largest file of the build. This head's own
  // build is this arm: `bun run build:web` writes an entry of 490,496 bytes against a budget of
  // 500,000 (measured 2026-11-12), and `bun run check:asset-sizes` prints the passing line.
  const dir = distDir("/assets/index-080_OX6C.js", {
    "assets/index-080_OX6C.js": MAX_ENTRY_BYTES,
    "assets/index-DgQRkS4v.js": 1_134_772,
    "assets/mermaid-render.worker-zV4qaloj.js": 1_565_608,
  });
  expect(entryChunk(dir)).toEqual({ path: "assets/index-080_OX6C.js", bytes: MAX_ENTRY_BYTES });
  expect(budgetReadings(dir)).toEqual([
    {
      message: `Every file of web/dist/assets is at most ${MAX_ASSET_BYTES} bytes.`,
      refused: false,
    },
    {
      message: `The entry chunk web/dist/assets/index-080_OX6C.js is ${MAX_ENTRY_BYTES} bytes, inside the ${MAX_ENTRY_BYTES}-byte budget.`,
      refused: false,
    },
  ]);
});

test("a build naming no module script is refused rather than measured", () => {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-dist-"));
  dirs.push(dir);
  writeFileSync(
    join(dir, "index.html"),
    "<!doctype html><html><head><script>var a = 1;</script></head><body></body></html>"
  );
  expect(() => entryChunk(dir)).toThrow("names 0 module scripts");
});
