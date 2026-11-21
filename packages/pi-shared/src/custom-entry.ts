/**
 * Readers of the custom entries a plugin writes to its session with `pi.appendEntry`, each a
 * `custom` entry that holds its `data`: every entry of one type (`customEntries`), or the newest
 * one this build can read (`lastCustomEntry`). Each plugin state a worker keeps across a
 * `--resume` relaunch rides a custom entry of its own type (`./phase-stall`'s
 * `legion-phase-stall`, `./fallback-cooldown`'s `legion-model-fallback`), and each of them is
 * restored the same way: newest first, and a record this build cannot read is passed over rather
 * than read in part.
 */

/**
 * The newest `customType` entry of the branch whose data `read` answers for, or undefined when
 * the branch holds none: the walk goes newest first and stops at the first answer, so an entry
 * `read` answers undefined for — an older build's record, one with a field of the wrong type — is
 * passed over and an older readable entry behind it is still found.
 *
 * The branch is read as `readonly unknown[]` and narrowed field by field, as every reader of it
 * in this package is: a host whose entry shape differs leaves the caller with nothing rather than
 * throwing inside an event handler.
 */
export function lastCustomEntry<T>(
  branch: readonly unknown[],
  customType: string,
  read: (data: object) => T | undefined
): T | undefined {
  for (let index = branch.length - 1; index >= 0; index -= 1) {
    const entry = branch[index];
    if (
      typeof entry === "object" &&
      entry !== null &&
      "type" in entry &&
      entry.type === "custom" &&
      "customType" in entry &&
      entry.customType === customType &&
      "data" in entry &&
      typeof entry.data === "object" &&
      entry.data !== null
    ) {
      const value = read(entry.data);
      if (value !== undefined) return value;
    }
  }
  return undefined;
}

/** The data of every `customType` entry among a session's entries (`sessionManager.getBranch` or
 *  `getEntries`: a `pi.appendEntry` write is a `custom` entry holding its `data`), in the order
 *  written. An entry with no `data` is passed over. Each reader checks the data's shape itself. */
export function customEntries(entries: readonly unknown[], customType: string): unknown[] {
  const found: unknown[] = [];
  for (const entry of entries) {
    if (typeof entry !== "object" || entry === null) continue;
    if (!("type" in entry) || entry.type !== "custom") continue;
    if (!("customType" in entry) || entry.customType !== customType) continue;
    if (!("data" in entry)) continue;
    found.push(entry.data);
  }
  return found;
}
