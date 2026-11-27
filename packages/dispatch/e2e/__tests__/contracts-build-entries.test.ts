import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { buildInto, underDist } from "@legion/test-fixtures/package-build";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// Playwright's config loader and the browser suite's Node side load `@legion/contracts` and each
// of its subpaths by the `default` condition of its `exports` entry, a file under
// packages/contracts/dist that `build:dispatch` writes (the dashboard's `build:contracts` runs it).
// Every subpath the dashboard or the proof editor imports must be one of that script's entries:
// one it does not build fails every browser shard on a missing `dist/<subpath>.js`, which a new
// import of `@legion/contracts/http-status` once did. This reads every import of the package in
// both trees, runs `build:dispatch` into a directory of its own, and requires each import's file.

const contracts = fileURLToPath(new URL("../../../contracts/", import.meta.url));
const importers = ["../../", "../../../proof-editor/"].map((tree) =>
  fileURLToPath(new URL(tree, import.meta.url))
);
const scratch = temporaryDirectories("contracts-build-entries");

/** A module specifier of the package in an import, an export or a dynamic import. */
const specifier = /\b(?:from|import)\s*\(?\s*["']@legion\/contracts(?:\/([^"']+))?["']/g;

/** Each subpath of the package a source of `tree` imports, `.` for the package itself. */
function importedSubpaths(tree: string): Set<string> {
  const subpaths = new Set<string>();
  for (const path of new Bun.Glob("**/*.{ts,tsx,mts,js,mjs}").scanSync({ cwd: tree })) {
    if (path.split("/").some((part) => part === "node_modules" || part.startsWith("dist"))) {
      continue;
    }
    for (const match of readFileSync(`${tree}${path}`, "utf8").matchAll(specifier)) {
      subpaths.add(match[1] === undefined ? "." : `./${match[1]}`);
    }
  }
  return subpaths;
}

test(
  "build:dispatch writes the file of every contracts subpath the dashboard and the proof editor import",
  async () => {
    const manifest = JSON.parse(readFileSync(`${contracts}package.json`, "utf8")) as {
      exports: Record<string, { default: string }>;
    };
    const imported = [...new Set(importers.flatMap((tree) => [...importedSubpaths(tree)]))].sort();
    expect(imported).toContain(".");
    const unexported = imported.filter((subpath) => manifest.exports[subpath] === undefined);
    expect(unexported).toEqual([]);

    const written = new Set(await buildInto(contracts, "build:dispatch", scratch()));
    const missing = imported
      .map((subpath) => ({ subpath, file: underDist(manifest.exports[subpath].default) }))
      .filter(({ file }) => !written.has(file));
    expect(missing).toEqual([]);
  },
  {
    // This row drives one `bun run build:dispatch` child, which buildInto starts and nothing else
    // bounds; it bundles four entries in well under a second, so the minute is what stops a build
    // that hangs, as packages/contracts' own entry-point test bounds the same build.
    timeout: 60_000,
  }
);
