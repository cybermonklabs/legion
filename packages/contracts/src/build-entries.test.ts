import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, posix, resolve } from "node:path";

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

/** A path package.json gives under `dist/`, as a path inside the build's output directory. */
function underDist(path: string): string {
  const inside = posix.relative("dist", path);
  if (inside.startsWith("..")) throw new Error(`${path} is not under dist/`);
  return inside;
}

const defaults = [...new Set(entries.map((entry) => underDist(entry.default)))].sort();

/** Every scratch directory a build of this file made. `afterAll` removes them, so one whose test
 *  timed out while its build still ran is removed too, which a `finally` in `build` would miss. */
const scratch: string[] = [];
afterAll(() => {
  for (const dir of scratch) rmSync(dir, { force: true, recursive: true });
});

/** Runs one build script of package.json into a scratch directory and lists what it wrote. */
async function build(script: string): Promise<string[]> {
  const out = mkdtempSync(join(tmpdir(), "contracts-build-"));
  scratch.push(out);
  const spawned = Bun.spawn(["bun", "run", script, "--outdir", out], {
    cwd: packageDir,
    stderr: "pipe",
    stdout: "ignore",
  });
  const [stderr, exitCode] = await Promise.all([
    new Response(spawned.stderr).text(),
    spawned.exited,
  ]);
  if (exitCode !== 0) throw new Error(`bun run ${script} exited ${exitCode}: ${stderr}`);
  return [...new Bun.Glob("**/*").scanSync({ cwd: out, onlyFiles: true })].sort();
}

describe("package.json's entry points", () => {
  test("each source package.json names exists", () => {
    const sources = [manifest.types, ...entries.flatMap((entry) => [entry.types, entry.bun])];
    expect(sources.filter((path) => !existsSync(join(packageDir, path)))).toEqual([]);
  });

  test("build writes each default condition and main where package.json names it, and nothing else", async () => {
    const written = await build("build");
    expect(written).toEqual(defaults);
    expect(written).toContain(underDist(manifest.main));
  });

  test("build:dispatch writes each of its entries where package.json names it", async () => {
    const written = await build("build:dispatch");
    expect(written.length).toBeGreaterThan(0);
    expect(written.filter((path) => !defaults.includes(path))).toEqual([]);
  });
});
