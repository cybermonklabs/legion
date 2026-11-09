// TEMPORARY, dispatch://INFRA-2188. This file is deleted in a later commit of this pull request,
// before the head the reviewer reads: it measures, on the kernel that produces the race, how often
// a read of a live pane's transcript lands between a line's bytes and its newline — the read that
// threw `JSON Parse error: Expected '}'` at stall-watchdog-pane.ts:497 in job 128911692874. This
// pod cannot produce it (53,561 reads at idle and 9,283 under a calibrated load, 0 torn), and
// dispatch://INFRA-2035's retro prescribes taking such a measurement where the kernel is: a
// temporary file the `pi-legion` job's own `bun test` already globs, read out of the job's log.
//
// It asserts only that it read the file, so it cannot fail the job whatever it counts.
import { afterEach, expect, test } from "bun:test";
import { readdir, readFile, stat } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import {
  cleanup,
  DEADLINE_MS,
  scratchDir,
  skip,
  startPane,
  warmedWedge,
} from "./stall-watchdog-pane";

const CLEANUP_MS = 120_000;

afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
}, CLEANUP_MS);

/** The pane's scratch root prefix, which is how the loop finds a transcript the pane does not
 * hand out (`ompRoot` in `@legion/pi-shared/test/omp-harness` names the root `legion-<name>-…`). */
const PANE = "stall-probe";

/** How long the loop reads for, once the pane is up: long enough to cover the wedged call and the
 * evidence the sweep writes for it, short beside the job's 10-minute bound. */
const PROBE_MS = 20_000;

/** The one `.jsonl` under a `legion-<PANE>-…/sessions` of this run, once the pane has made it. */
async function findTranscript(): Promise<string | undefined> {
  const root = os.tmpdir();
  let names: string[];
  try {
    names = await readdir(root);
  } catch {
    return undefined;
  }
  for (const name of names) {
    if (!name.startsWith(`legion-${PANE}-`)) continue;
    const sessions = path.join(root, name, "sessions");
    let entries: string[];
    try {
      entries = await readdir(sessions, { recursive: true });
    } catch {
      continue;
    }
    for (const entry of entries) {
      if (!entry.endsWith(".jsonl")) continue;
      const file = path.join(sessions, entry);
      try {
        if ((await stat(file)).isFile()) return file;
      } catch {
        // The pane's cleanup took it; look again.
      }
    }
  }
  return undefined;
}

test.skipIf(skip)(
  "INFRA-2188 probe: how often a read of a live transcript lands mid-line on this kernel",
  async () => {
    const pidFile = path.join(await scratchDir("probe-pid"), "child.pid");
    await startPane({
      name: PANE,
      stateDir: await scratchDir("probe-state"),
      reply: warmedWedge(pidFile, { goOn: false }, "The call came back."),
    });

    let file: string | undefined;
    let reads = 0;
    let torn = 0;
    let longest = 0;
    const messages = new Map<string, number>();
    const tails: string[] = [];
    const deadline = Date.now() + PROBE_MS;
    while (Date.now() < deadline) {
      if (file === undefined) {
        file = await findTranscript();
        if (file === undefined) continue;
      }
      let text: string;
      try {
        text = await readFile(file, "utf8");
      } catch {
        file = undefined;
        continue;
      }
      reads += 1;
      const lines = text.split("\n");
      longest = Math.max(longest, ...lines.map((line) => line.length));
      const tail = lines.at(-1) ?? "";
      if (tail === "") continue;
      torn += 1;
      let message = "parsed: a complete object with no newline yet";
      try {
        JSON.parse(tail);
      } catch (error) {
        message = (error as Error).message;
      }
      messages.set(message, (messages.get(message) ?? 0) + 1);
      if (tails.length < 8)
        tails.push(`len=${tail.length} tail=${JSON.stringify(tail.slice(-60))}`);
    }

    const percent = ((100 * torn) / Math.max(reads, 1)).toFixed(2);
    console.log(
      `INFRA-2188-PROBE reads=${reads} tornTail=${torn} (${percent}%) longestLine=${longest}`
    );
    for (const [message, n] of messages) console.log(`INFRA-2188-PROBE message ${n}x: ${message}`);
    for (const tail of tails) console.log(`INFRA-2188-PROBE ${tail}`);
    // The one assertion: the probe read the transcript at all, so a count of zero is a count and
    // not a loop that never found the file.
    expect(reads).toBeGreaterThan(0);
  },
  DEADLINE_MS + 60_000
);
