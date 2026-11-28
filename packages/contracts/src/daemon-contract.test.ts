import { afterAll, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { daemonContract } from "./daemon-contract";

// A plugin's contract in each place its code runs (c-entry-numbers): a packed plugin's manifest
// holds the field, a checkout counts its own fixtures, and an installed bundle that lacks the field
// refuses to load. Each case runs in a scratch tree of its own outside the repository, so the
// repository's own fixtures decide none of them.

const scratch = mkdtempSync(join(tmpdir(), "daemon-contract-"));
afterAll(() => rmSync(scratch, { force: true, recursive: true }));

/** The cutover's `released-contracts.json`, in short: the frozen part of `SupportedContracts`. */
const RELEASED = [46, 48, 228, 232];
const NO_FIELD = { name: "@sjawhar/pi-legion" };

/** A checkout's root under the scratch directory: `released` as the text of its
 *  `released-contracts.json`, and the files `entries` names in `contracts/` beside it, or no
 *  `contracts/` directory at all for null. Answers the directory of a plugin's source in it, the
 *  `from` a test or a linked plugin passes, and the path of `released-contracts.json`. */
function checkout(name: string, released: unknown, entries: readonly string[] | null) {
  const root = join(scratch, name);
  const fixtures = join(root, "packages", "contracts", "fixtures", "daemon-api");
  const releasedFile = join(fixtures, "released-contracts.json");
  mkdirSync(fixtures, { recursive: true });
  writeFileSync(releasedFile, JSON.stringify(released));
  if (entries !== null) {
    mkdirSync(join(fixtures, "contracts"));
    for (const entry of entries) writeFileSync(join(fixtures, "contracts", entry), "{}");
  }
  const plugin = join(root, "packages", "pi-legion", "extensions");
  mkdirSync(plugin, { recursive: true });
  return { plugin, releasedFile };
}

test("the manifest's field is the contract, wherever the code runs", () => {
  const { plugin } = checkout("declared", RELEASED, ["20261125010101_probe.json"]);
  expect(daemonContract({ legion: { daemonApiVersion: 240 } }, plugin)).toBe(240);
  const installed = join(scratch, "declared-install", "pi-legion", "dist");
  mkdirSync(installed, { recursive: true });
  expect(daemonContract({ legion: { daemonApiVersion: 240 } }, installed)).toBe(240);
});

test("in a checkout a manifest with no field speaks L plus the count of entry fixtures", () => {
  const entries = ["20261124235959_earlier.json", "20261125010101_later-one.json"];
  expect(daemonContract(NO_FIELD, checkout("two-entries", RELEASED, entries).plugin)).toBe(234);
  // The cutover's tree holds no entry, and so no contracts/ directory: 0 entries.
  expect(daemonContract(NO_FIELD, checkout("no-directory", RELEASED, null).plugin)).toBe(232);
});

// Only `<14-digit id>_<slug>.json` names an entry fixture. Another name in the directory is no
// entry, and is not counted; the daemon's TestEveryEntryHasOneFixture refuses it.
test("a name in contracts/ that is not <14-digit id>_<slug>.json is not counted", () => {
  const entries = [
    "20261125010101_probe.json",
    "probe.json",
    "2026112501010_short-id.json",
    "20261125010102_.json",
    "20261125010103_probe.yaml",
    "README.md",
  ];
  expect(daemonContract(NO_FIELD, checkout("other-names", RELEASED, entries).plugin)).toBe(233);
});

test("a directory that is no checkout, with a manifest that holds no field, refuses", () => {
  // An installed bundle: dist/ of a plugin under a directory no checkout holds.
  const installed = join(scratch, "install", "node_modules", "@sjawhar", "pi-legion", "dist");
  mkdirSync(installed, { recursive: true });
  expect(() => daemonContract(NO_FIELD, installed)).toThrow(
    `the plugin's package.json holds no legion.daemonApiVersion, which every pack writes, and ${installed} is in no checkout: no directory from it up holds packages/contracts/fixtures/daemon-api/released-contracts.json`
  );
});

test("a released-contracts.json that is not a list of integers in strictly ascending order refuses, naming it", () => {
  for (const [name, released] of [
    ["descending", [232, 48, 46]],
    ["repeated", [46, 48, 48, 232]],
    ["empty", []],
    ["string", [46, "48", 232]],
    ["object", { L: 232 }],
  ] as const) {
    const { plugin, releasedFile } = checkout(`released-${name}`, released, null);
    expect(() => daemonContract(NO_FIELD, plugin)).toThrow(
      `${releasedFile} is not a list of contract numbers in strictly ascending order`
    );
  }
});
