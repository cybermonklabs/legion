/** The two runs of `e2e/acceptance/outside-path-reach.sh` that need a network which drops a
 *  packet to 192.0.2.1 (TEST-NET-1, RFC 5737) rather than refusing it. The module asks once, with
 *  the script's own probe, before it registers them: where the network refuses the address at once
 *  the two rows are skipped with that reason printed, never passed, and any other answer fails this
 *  file alone. `outside-path-reach-fixtures.ts` holds the fixtures and says how they work.
 */

import { describe, expect } from "bun:test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  blackhole,
  curl,
  fakeDispatch,
  holdingTarget,
  lineFor,
  probingRow,
  reachFixtures,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

const { scratch, drive, list, pathWithCurl } = reachFixtures();

/** The script's own probe, run once here against the blackholed address: exit 28 with the lookup
 *  done and no connect is a drop, and the rows run; exit 7 is a network that refuses the address
 *  at once, and they are skipped; anything else is a host that cannot run them, which fails. */
async function blackholeDrops(): Promise<string | null> {
  if (withoutTools) return "bash or curl is missing";
  const child = Bun.spawn(
    [
      curl,
      "-q",
      "--noproxy",
      "*",
      "-g",
      "-sS",
      "-o",
      "/dev/null",
      "--connect-timeout",
      "3",
      "--max-time",
      "5",
      "-w",
      "%{time_namelookup} %{time_connect}",
      `telnet://${blackhole.host}:${blackhole.port}`,
    ],
    { env: { LC_ALL: "C" }, stdin: "ignore", stderr: "pipe", stdout: "pipe" }
  );
  const [code, stdout] = await Promise.all([child.exited, new Response(child.stdout).text()]);
  const [lookup = "", connect = ""] = stdout.trim().split(" ");
  if (code === 28 && /[1-9]/.test(lookup) && /^0\.0+$/.test(connect)) return null;
  if (code === 7) {
    return (
      `${blackhole.host}:${blackhole.port} was refused at once here (curl exit 7, "${stdout.trim()}"): ` +
      "this network rejects that address rather than dropping it, so the blackholed rows cannot run"
    );
  }
  throw new Error(
    `the blackhole probe of ${blackhole.host}:${blackhole.port} gave curl exit ${code}, ` +
      `"${stdout.trim()}": neither a drop nor a refusal, so this host cannot run these rows`
  );
}
const blackholeSkip = await blackholeDrops();
if (blackholeSkip !== null) console.warn(`Skipped the blackholed rows: ${blackholeSkip}.`);

describe.concurrent("outside-path-reach.sh", () => {
  // A dropped address never answers, so a probe of it ends only at a timeout: curl's own, which
  // keeps the run to its rounds, or the kernel's, which on Linux gives up on the SYN after about
  // two minutes, and curl says exit 28 for either. So the row does not read how long the run
  // took, which this host's load moves: it reads what the script asked of the device's curl, the
  // 3 s to connect that the classifier's rules count on and the 5 s in all, in each round. That
  // the probes of a round run at once is `outside-path-reach.test.ts`'s holding-targets row.
  probingRow(withoutTools || blackholeSkip !== null)(
    "reads a blackholed address as unreachable and passes, curl's own timeouts bounding each probe of it",
    async () => {
      const origin = await fakeDispatch();
      const asked = join(scratch, `asked-${randomUUID()}`);
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(`blackhole ${blackhole.host} ${blackhole.port}`),
          },
          {
            path: pathWithCurl(
              `case "$*" in *telnet://${blackhole.host}:${blackhole.port}) echo " $* " >>${asked} ;; esac`
            ),
          }
        );
        expect(lineFor(run, "blackhole")).toContain(" unreachable; curl exit 28,");
        expect(lineFor(run, "blackhole")).toEndWith(
          "; probes: unreachable, unreachable, unreachable, unreachable, unreachable"
        );
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
        expect(
          readFileSync(asked, "utf8")
            .trimEnd()
            .split("\n")
            .map((call) => [
              call.includes(" --connect-timeout 3 "),
              call.includes(" --max-time 5 "),
            ])
        ).toEqual(Array(5).fill([true, true]));
      } finally {
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools || blackholeSkip !== null)(
    "fails the blackholed run once the holding target is added beside it",
    async () => {
      const origin = await fakeDispatch();
      const holding = holdingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(
            `blackhole ${blackhole.host} ${blackhole.port}`,
            `nats 127.0.0.1 ${holding.port}`
          ),
        });
        expect(lineFor(run, "blackhole")).toContain(" unreachable; ");
        expect(lineFor(run, "nats")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached nats");
        expect(run.code).toBe(1);
      } finally {
        await origin.stop();
        holding.stop(true);
      }
    }
  );
});
