/** What `e2e/acceptance/outside-path-reach.sh` does with a target list on a pipe when the device's
 *  shell exports TMOUT, which bash's `read` takes as its timeout. Each row gives the list through
 *  `/dev/stdin`, a named pipe or a process substitution, with the host's PATH or bash, curl and
 *  sleep alone, and needs the whole list read: the last line's hub listens, so the run must fail
 *  naming it. Before the script unset TMOUT, a TMOUT of 1 or 0.5 cut the read at the first line,
 *  and bash 5.2 printed PASS on it alone (acceptance's finding on dispatch://INFRA-3862).
 *  So the last line comes only once the script has been reading the list for longer than TMOUT:
 *  `drive` sends it TMOUT seconds after a write that returns only once the script is reading.
 *  bash's read, timed from its own start, has then timed out if TMOUT still holds, and it says so
 *  even when the line is already in the pipe when it next runs (on Linux and under gVisor alike).
 *  So the script's one read of the whole list, had it kept TMOUT, fails every row whose TMOUT this
 *  host's bash takes as a read timeout (bash 3.2 ignores a fractional one), however late this host
 *  runs it, on a host whose new pipes hold at most 64 KiB (`drive`'s feeder says where they do).
 *  A TMOUT the script cannot unset is `outside-path-reach-input.test.ts`'s, and
 *  `outside-path-reach-fixtures.ts` holds the fixtures and says how they work.
 */

import { describe, expect } from "bun:test";
import {
  closedPort,
  closingTarget,
  fakeDispatch,
  holdingTarget,
  lineFor,
  probingRow,
  reachFixtures,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

const { drive, pathOf } = reachFixtures();

type Via = "stdin" | "fifo" | "substitution";
interface Case {
  tmout: string | undefined;
  via: Via;
  path: "full" | "bare";
  hub: "holding" | "closing";
}

const vias: Via[] = ["stdin", "fifo", "substitution"];
const cases: Case[] = [
  // The TMOUTs that cut the read short before the script unset TMOUT.
  ...["1", "0.5"].flatMap((tmout) =>
    vias.flatMap((via) =>
      (["full", "bare"] as const).map((path): Case => ({ tmout, via, path, hub: "holding" }))
    )
  ),
  // A hub that accepts and closes at once, beside the holding ones.
  { tmout: "1", via: "stdin", path: "full", hub: "closing" },
  // No TMOUT, and a TMOUT longer than the 1 s that cut the read before the script unset TMOUT.
  ...[undefined, "3"].flatMap((tmout) =>
    vias.map((via): Case => ({ tmout, via, path: "full", hub: "holding" }))
  ),
];

describe.concurrent("outside-path-reach.sh under an inherited TMOUT", () => {
  for (const { tmout, via, path, hub: kind } of cases) {
    probingRow(withoutTools)(
      `reads the whole list through ${via}, its last line sent ${tmout ?? "0"} s after the script ` +
        `was seen reading it, with ${tmout === undefined ? "no TMOUT" : `TMOUT=${tmout}`}, ` +
        `the ${path} PATH and a ${kind} hub`,
      async () => {
        const hub = kind === "holding" ? holdingTarget() : closingTarget();
        const origin = await fakeDispatch();
        try {
          const run = await drive(
            {
              OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
              ...(tmout === undefined ? {} : { TMOUT: tmout }),
            },
            {
              path: path === "bare" ? pathOf("bash", "curl", "sleep") : undefined,
              via,
              stdin: `closed 127.0.0.1 ${await closedPort()}\n`,
              stdinLater: `hub 127.0.0.1 ${hub.port}\n`,
              stdinPause: tmout ?? "0",
            }
          );
          expect(run.stdout).toMatch(/^outside-path-reach: targets \S+ \(2\)$/m);
          expect(lineFor(run, "hub")).toContain(" REACHED; ");
          expect(verdict(run)).toBe("FAIL: reached hub");
          expect(run.code).toBe(1);
        } finally {
          hub.stop(true);
          await origin.stop();
        }
      }
    );
  }
});
