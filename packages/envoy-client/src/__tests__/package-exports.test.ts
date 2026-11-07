import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

interface PackageManifest {
  readonly exports: Record<string, { readonly bun: string }>;
  readonly scripts: { readonly build: string };
}

const manifest = JSON.parse(
  readFileSync(new URL("../../package.json", import.meta.url), "utf-8")
) as PackageManifest;
const buildEntries = manifest.scripts.build
  .match(/^bun build (.+) --outdir /)?.[1]
  .split(" ")
  .filter((entry) => entry.endsWith(".ts"));

// The suite's 30 s default (test/default-timeout.ts) is this row's hang guard: bun's own unwritten
// 5,000 ms default was a limit on resolving and importing every build entry rather than on a hang.
// Measured in a 2-CPU pod: 0.34 s idle; under 16 busy loops (8 x nproc), 4.33 s at most in a
// whole-suite run and 6.35 s with this file alone, which passes that default. It fires only if an
// import never resolves.
test("exposes and resolves every package build entry", async () => {
  expect(buildEntries).toBeDefined();
  for (const entry of buildEntries ?? []) {
    const exportPath = `./${entry.slice("src/".length, -".ts".length)}`;
    expect(manifest.exports[exportPath]?.bun).toBe(`./${entry}`);
    // The runtime-selected specifier exercises each package export's actual resolver path.
    await import(`@legion/envoy-client/${exportPath.slice(2)}`);
  }
});
