/** The data of every `customType` entry among a session's entries (`sessionManager.getBranch` or
 *  `getEntries`: a `pi.appendEntry` write is a `custom` entry holding its `data`), in the order
 *  written. An entry with no `data` is passed over. Each reader checks the data's shape itself:
 *  the replay ledger's records (`missed-messages.ts`) and the answers messages a request sent
 *  (`session-answers-context.ts`). */
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
