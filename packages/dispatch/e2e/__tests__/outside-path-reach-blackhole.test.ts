/** The two runs of `e2e/acceptance/outside-path-reach.sh` that need a network which drops a
 *  packet to 192.0.2.1 (TEST-NET-1, RFC 5737) rather than refusing it. The module asks once, with
 *  the script's own probe, before it registers them: where the network refuses the address at once
 *  the two rows are skipped with that reason printed, never passed, and any other answer fails this
 *  file alone. `outside-path-reach-fixtures.ts` holds the fixtures and says how they work.
 */

import { describe, expect } from "bun:test";
import {
  blackhole,
  curl,
  fakeDispatch,
  holdingTarget,
  lineFor,
  reachFixtures,
  row,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

const { drive, list } = reachFixtures();

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
  row(withoutTools || blackholeSkip !== null)(
    "reads a blackholed address as unreachable and passes within 36 seconds",
    async () => {
      const origin = fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(`blackhole ${blackhole.host} ${blackhole.port}`),
        });
        expect(lineFor(run, "blackhole")).toContain(" unreachable; curl exit 28,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
        expect(run.seconds).toBeLessThan(36);
      } finally {
        origin.stop(true);
      }
    }
  );

  row(withoutTools || blackholeSkip !== null)(
    "fails the blackholed run once the holding target is added beside it",
    async () => {
      const origin = fakeDispatch();
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
        origin.stop(true);
        holding.stop(true);
      }
    }
  );
});
