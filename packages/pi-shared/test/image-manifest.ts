import { mock } from "bun:test";
import * as fs from "node:fs";
import * as fsPromises from "node:fs/promises";

/**
 * Runs `body` while a read of `imageManifest` through `node:fs` `readFileSync` or
 * `node:fs/promises` `readFile` answers `manifest` as JSON. Every other path reads the real file.
 * The two modules are restored when `body` ends, also when it throws.
 *
 * `read_build` must answer the build of the module the process loaded, and never the copy on the
 * image (`/opt/legion/<plugin>/package.json`), which a relaunch does not update. A host reads its
 * build once, when it loads, and the suite's own static import of that host loaded before this
 * mock. So `body` must import the host again, inside `body`, with a query on the specifier: Bun
 * then evaluates the module again under the mock. A read at load and a read inside `readBuild`
 * both meet the mock.
 */
export async function withImageManifest(
  imageManifest: string,
  manifest: Readonly<Record<string, unknown>>,
  body: () => Promise<void>
): Promise<void> {
  const atImagePath = JSON.stringify(manifest);
  const realFs = { ...fs };
  const realFsPromises = { ...fsPromises };
  mock.module("node:fs", () => ({
    ...realFs,
    readFileSync: (
      file: Parameters<typeof fs.readFileSync>[0],
      options?: Parameters<typeof fs.readFileSync>[1]
    ) => (String(file) === imageManifest ? atImagePath : realFs.readFileSync(file, options)),
  }));
  mock.module("node:fs/promises", () => ({
    ...realFsPromises,
    readFile: async (
      file: Parameters<typeof fsPromises.readFile>[0],
      options?: Parameters<typeof fsPromises.readFile>[1]
    ) => (String(file) === imageManifest ? atImagePath : realFsPromises.readFile(file, options)),
  }));
  try {
    await body();
  } finally {
    mock.module("node:fs", () => realFs);
    mock.module("node:fs/promises", () => realFsPromises);
  }
}
