/**
 * Raising the upstream pin is the four-step procedure in `packages/proof-editor/AGENTS.md`, whose
 * second step deletes the tarball the old pin packed. Nothing makes that step happen: the
 * `.gitignore` line tracking the archive is a glob, `!packages/proof-editor/proof-sdk-*.tgz`,
 * written as one so the eight path patterns across the workflows and the two Dockerfiles need no
 * edit when the pin moves, and a glob tracks a forgotten tarball as readily as the live one. A
 * raise that packs the new archive and leaves the old one beside it commits both, and from then on
 * every clone and both image build contexts — each `COPY packages/proof-editor/proof-sdk-*.tgz`
 * takes whatever the glob matches — carry a dead archive nobody installs. A tarball whose name the
 * manifest does not name is the same mistake from the other side: the install reads
 * `dependencies["proof-sdk-upstream"]`, so an archive that value does not name is dead weight
 * whatever it holds, and a value naming no archive is an install that cannot resolve.
 *
 * Nothing else in the tree reports either. `bun.lock` records the `sha512` of the file the manifest
 * names and knows nothing of its neighbours; the globs match both happily; and the suites that read
 * the dependency read what Bun installed, not what the directory holds. So the two cases here read
 * the real `packages/proof-editor` rather than a fixture: the mistake is a fact about the committed
 * tree, and only the tree can state it.
 */

import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// Resolved from this file, never from `process.cwd()`: the suite runs from the package root
// (`bun test src/tests tests`), and CI may run it from anywhere else.
const packageDir = join(import.meta.dir, "..");

const tarballs = readdirSync(packageDir, { withFileTypes: true })
  .filter((entry) => entry.isFile() && /^proof-sdk-.*\.tgz$/.test(entry.name))
  .map((entry) => entry.name);

const manifest = JSON.parse(readFileSync(join(packageDir, "package.json"), "utf8")) as {
  dependencies: Record<string, string>;
};

// `toHaveLength` prints the counts alone, and the names are the point: which archive the raise
// forgot is the whole of the fix.
test("packages/proof-editor holds exactly one packed upstream tarball", () => {
  expect(tarballs, `proof-sdk-*.tgz matched: ${tarballs.join(", ") || "nothing"}`).toHaveLength(1);
});

// Over what the directory holds rather than against `tarballs[0]`, so that a second tarball is
// reported by the case above alone, the one whose message names it: a manifest that still names
// a real archive has nothing wrong with it. With one tarball the two are the same assertion.
test("the manifest's proof-sdk-upstream dependency names that tarball", () => {
  const installs = tarballs.map((name) => `file:./${name}`);
  expect(installs).toContain(manifest.dependencies["proof-sdk-upstream"]);
});
