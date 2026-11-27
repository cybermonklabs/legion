import { posix } from "node:path";

// A package's build scripts, run as a test runs them: into a directory the test owns, never the
// package's own `dist/`, so a test reads what the script writes and nothing an earlier build left.
// packages/contracts' entry-point test and the dashboard's test of the contracts subpaths it
// imports both read a build this way.

/**
 * Runs `bun run <script> --outdir <out>` in `packageDir` and returns every file the build wrote
 * under `out`, as paths relative to it, sorted. A script that exits non-zero throws, with its
 * standard error.
 */
export async function buildInto(
  packageDir: string,
  script: string,
  out: string
): Promise<string[]> {
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

/**
 * A path a `package.json` gives under `dist/` (an `exports` entry's `default`, or `main`), as the
 * path inside a build's output directory that `buildInto` returns. One outside `dist/` throws.
 */
export function underDist(path: string): string {
  const inside = posix.relative("dist", path);
  if (inside.startsWith("..")) throw new Error(`${path} is not under dist/`);
  return inside;
}
