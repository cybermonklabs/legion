/** What `e2e/acceptance/outside-path-reach.sh` does with a target list on a pipe when the device's
 *  shell exports TMOUT, which bash's `read` takes as its timeout. Each row gives the list through
 *  `/dev/stdin`, a named pipe or a process substitution, its second line 2 s after its first, with
 *  the host's PATH or bash, curl and sleep alone, and needs the whole list read: the second line's
 *  hub listens, so the run must fail naming it. Before the script unset TMOUT, a TMOUT of 1 or 0.5
 *  cut the read at the first line, and bash 5.2 printed PASS on it alone (acceptance's finding on
 *  dispatch://POMJU-3862). The rows with no TMOUT and with TMOUT=3 read the whole list either way.
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
  reachFixtures,
  row,
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
  // Controls: no TMOUT, and one longer than the 2 s the second line takes.
  ...[undefined, "3"].flatMap((tmout) =>
    vias.map((via): Case => ({ tmout, via, path: "full", hub: "holding" }))
  ),
];

describe.concurrent("outside-path-reach.sh under an inherited TMOUT", () => {
  for (const { tmout, via, path, hub: kind } of cases) {
    row(withoutTools)(
      `reads the whole list through ${via}, its second line 2 s late, with ` +
        `${tmout === undefined ? "no TMOUT" : `TMOUT=${tmout}`}, the ${path} PATH and a ${kind} hub`,
      async () => {
        const origin = fakeDispatch();
        const hub = kind === "holding" ? holdingTarget() : closingTarget();
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
            }
          );
          expect(run.stdout).toMatch(/^outside-path-reach: targets \S+ \(2\)$/m);
          expect(lineFor(run, "hub")).toContain(" REACHED; ");
          expect(verdict(run)).toBe("FAIL: reached hub");
          expect(run.code).toBe(1);
        } finally {
          origin.stop(true);
          hub.stop(true);
        }
      }
    );
  }
});
