import { describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { join, resolve } from "node:path";
import { buildInto, underDist } from "@legion/test-fixtures/package-build";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

/**
 * Every file `package.json` names is a file the package has. The `types` and `bun` conditions of
 * each `exports` entry, and the top-level `types`, name sources. Each `default` condition and
 * `main` name a file `build` writes under `dist/`: it is what Node, Playwright's config loader and
 * every other reader without the `bun` condition loads. Each build runs into a scratch directory
 * of its own, so it never touches the package's `dist`; the scratch directory stands for `dist/`.
 */

const packageDir = resolve(import.meta.dir, "..");

type ExportEntry = { readonly types: string; readonly bun: string; readonly default: string };
type Manifest = {
  readonly exports: Readonly<Record<string, ExportEntry>>;
  readonly main: string;
  readonly types: string;
};

const manifest = (await Bun.file(join(packageDir, "package.json")).json()) as Manifest;
const entries = Object.values(manifest.exports);

const defaults = [...new Set(entries.map((entry) => underDist(entry.default)))].sort();

const scratch = temporaryDirectories("contracts-build");

describe("package.json's entry points", () => {
  test("each source package.json names exists", () => {
    const sources = [manifest.types, ...entries.flatMap((entry) => [entry.types, entry.bun])];
    expect(sources.filter((path) => !existsSync(join(packageDir, path)))).toEqual([]);
  });

  test("build writes each default condition and main where package.json names it, and nothing else", async () => {
    const written = await buildInto(packageDir, "build", scratch());
    expect(written).toEqual(defaults);
    expect(written).toContain(underDist(manifest.main));
  }, 60_000);

  test("build:dispatch writes each of its entries where package.json names it", async () => {
    const written = await buildInto(packageDir, "build:dispatch", scratch());
    expect(written.length).toBeGreaterThan(0);
    expect(written.filter((path) => !defaults.includes(path))).toEqual([]);
  }, 60_000);
});
