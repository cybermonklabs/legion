// The daemon API contract a plugin build speaks: the number its client states on every request as
// `pluginContract`, its registrations carry, and `read_build` answers. No committed manifest holds
// it. A contract change is one entry, and a build's number is L, the greatest number of
// `released-contracts.json`, plus its count of entries; each entry has one fixture under
// `fixtures/daemon-api/contracts/` (`TestEveryEntryHasOneFixture`), so the count of those files is
// the count of entries, and the daemon built from the same tree serves that number
// (`api.DaemonAPIVersion()`). Every step that packs a plugin writes the number into the manifest it
// packs as `legion.daemonApiVersion` (`scripts/daemon-contract.sh` computes it there): pi-legion's
// `bun pm pack` (release.yaml's pi_legion_build, worker.Dockerfile, scripts/e2e/lib/pack-plugin.sh),
// claude-envoy's `scripts/build.ts --out` and the release's release-claude-envoy-version.sh. The
// bundle inlines that manifest, so an installed plugin reads the field, the number the daemon's
// boot gate holds the installed manifest to. Only code run from a checkout, a test or a plugin
// linked from one, finds no field, and it counts the checkout's own fixtures; the merge order
// changes no build's own number, so the count needs no history.
//
// The entry fixture directory holds only regular files named `<14-digit id>_<slug>.json`, and the
// daemon's `TestEveryEntryHasOneFixture` refuses any other entry there. This module, like
// scripts/daemon-contract.sh and the deployment repository's legion-pins, counts the names of that
// form alone, so every reader of one tree gives one number; it leaves the refusal to that test.

import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

/** The name of an entry fixture: the entry's 14-digit id, `_`, its slug, `.json`. */
const ENTRY_FIXTURE_NAME = /^[0-9]{14}_.+\.json$/;

/** Where a checkout holds the daemon API fixtures, from its root. */
const DAEMON_API_FIXTURES = join("packages", "contracts", "fixtures", "daemon-api");

/** A plugin's `package.json`, as its host imports it: any manifest, which holds the contract under
 *  `legion` once a pack has written it. The index signature lets a manifest that names no `legion`
 *  pass, which a type of optional properties alone refuses. */
export interface PluginManifest {
  readonly legion?: { readonly daemonApiVersion?: number };
  readonly [key: string]: unknown;
}

/**
 * The daemon API contract of the build whose manifest is `manifest`: its `legion.daemonApiVersion`
 * where it holds one, as every packed plugin does. Else, when a directory from `from` up, `from`
 * being the caller's own directory (`import.meta.dir`), is a checkout's root, one that holds
 * `packages/contracts/fixtures/daemon-api/released-contracts.json`, it is the greatest number of
 * that list plus the count of the entry fixtures beside it (the names in `contracts/` of the form
 * `<14-digit id>_<slug>.json`, none when the directory is absent). It throws on a list that is not
 * one of integers in strictly ascending order. Else it throws: an installed plugin that lacks the
 * field was never packed, and it refuses to load rather than speak a number it guessed.
 */
export function daemonContract(manifest: PluginManifest, from: string): number {
  const declared = manifest.legion?.daemonApiVersion;
  if (declared !== undefined) return declared;
  for (let dir = resolve(from); ; dir = dirname(dir)) {
    const fixtures = join(dir, DAEMON_API_FIXTURES);
    const releasedFile = join(fixtures, "released-contracts.json");
    if (existsSync(releasedFile)) {
      return lastReleased(releasedFile) + entryFixtures(join(fixtures, "contracts"));
    }
    if (dirname(dir) === dir) break;
  }
  throw new Error(
    `the plugin's package.json holds no legion.daemonApiVersion, which every pack writes, and ${from} is in no checkout: no directory from it up holds ${join(DAEMON_API_FIXTURES, "released-contracts.json")}`
  );
}

/** L: the greatest number of `released-contracts.json`, which must be a list of integers in
 *  strictly ascending order, so its last. */
function lastReleased(file: string): number {
  const released: unknown = JSON.parse(readFileSync(file, "utf8"));
  if (
    !Array.isArray(released) ||
    released.length === 0 ||
    !released.every(
      (contract, at) => Number.isInteger(contract) && (at === 0 || contract > released[at - 1])
    )
  ) {
    throw new Error(`${file} is not a list of contract numbers in strictly ascending order`);
  }
  return released[released.length - 1] as number;
}

/** The count of entry fixtures in `dir`, its names of the form `<14-digit id>_<slug>.json`; zero
 *  when `dir` is absent, as it is in a tree that holds no entry. */
function entryFixtures(dir: string): number {
  if (!existsSync(dir)) return 0;
  return readdirSync(dir).filter((name) => ENTRY_FIXTURE_NAME.test(name)).length;
}
