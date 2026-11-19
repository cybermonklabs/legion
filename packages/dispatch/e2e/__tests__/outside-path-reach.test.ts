/** What `e2e/acceptance/outside-path-reach.sh` decides once it probes: every row here gets past
 *  the inputs and reaches the five rounds of probes, against listeners on this host's loopback.
 *  The inputs it refuses and its classifier are `outside-path-reach-input.test.ts`'s, the two runs
 *  that need a network dropping TEST-NET-1 are `outside-path-reach-blackhole.test.ts`'s, and
 *  `outside-path-reach-fixtures.ts` holds the fixtures and says how they work.
 *
 *  The script probes in five rounds 6 s apart, and a probe that connects to a holding listener
 *  ends only at curl's `--max-time` of five seconds, so every row here takes about 29 seconds;
 *  the rows run concurrently.
 */

import { describe, expect } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  bash,
  blackhole,
  closedPort,
  closingTarget,
  fakeDispatch,
  holdingTarget,
  lineFor,
  locales,
  probingRow,
  reachFixtures,
  untilExists,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

const { drive, targets, written, list, pathOf, pathWithCurl, pathRounds, v6Only } = reachFixtures();

describe.concurrent("outside-path-reach.sh", () => {
  probingRow(withoutTools)(
    "passes when the origin answers as Dispatch and every target is unreachable, its control read through time_connect",
    async () => {
      const origin = await fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(),
        });
        // Bun.serve holds the probe's connection, so curl ends at --max-time with 28: the control
        // reads REACHED from time_connect, not from a clean exit.
        expect(lineFor(run, "origin")).toMatch(
          new RegExp(`^control origin 127\\.0\\.0\\.1:${origin.port} REACHED; curl exit 28, `)
        );
        expect(lineFor(run, "closed-a")).toContain(" unreachable; curl exit 7,");
        expect(lineFor(run, "closed-b")).toContain(" unreachable; curl exit 7,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
        expect(await origin.seen()).toEqual([
          { method: "GET", path: "/healthz", credential: null },
          { method: "GET", path: "/api/v1/projects", credential: null },
        ]);
      } finally {
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "fails naming a target that listens, which is the check's negative control",
    async () => {
      const listening = closingTarget();
      const origin = await fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${listening.port}`),
        });
        expect(lineFor(run, "hub")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
      } finally {
        listening.stop(true);
        await origin.stop();
      }
    }
  );

  // Two holding targets beside the holding origin and two closed targets. Each probe of any of the
  // five waits, before it connects, until the probe of every other one in the same round has begun
  // and, in each round but the last, until the sleep after that round has begun. A round that probes
  // its hosts at once, and sleeps while they run, goes on at once. A round that probes any of them
  // only once another has ended, or that waits for its probes before its sleep, never ends, and the
  // row fails at its timeout.
  probingRow(withoutTools)(
    "reads targets that accept and hold the connection as reached, each round probing them at once",
    async () => {
      const first = holdingTarget();
      const second = holdingTarget();
      const closed = [await closedPort(), await closedPort()];
      const origin = await fakeDispatch();
      try {
        const hosts = {
          origin: `127.0.0.1:${origin.port}`,
          "closed-a": `127.0.0.1:${closed[0]}`,
          "closed-b": `127.0.0.1:${closed[1]}`,
          "nats-a": `127.0.0.1:${first.port}`,
          "nats-b": `127.0.0.1:${second.port}`,
        };
        const { path } = pathRounds(
          hosts,
          [
            'if [ -n "$target" ]; then',
            ...Object.keys(hosts).map(
              (other) => `  [ "$target" = ${other} ] || ${untilExists(`"$rounds/${other}-$round"`)}`
            ),
            `  [ "$round" -ge 5 ] || ${untilExists('"$rounds/sleep-$round"')}`,
            "fi",
          ].join("\n")
        );
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: targets(
              `closed-a 127.0.0.1 ${closed[0]}`,
              `closed-b 127.0.0.1 ${closed[1]}`,
              `nats-a 127.0.0.1 ${first.port}`,
              `nats-b 127.0.0.1 ${second.port}`
            ),
          },
          { path }
        );
        expect(lineFor(run, "nats-a")).toContain(" REACHED; curl exit 28,");
        expect(lineFor(run, "nats-b")).toContain(" REACHED; curl exit 28,");
        // Only the host's curl, connecting to the target, reads a round of it REACHED here.
        expect(probesOf(lineFor(run, "nats-a"))).toEqual(Array(5).fill("REACHED"));
        expect(probesOf(lineFor(run, "nats-b"))).toEqual(Array(5).fill("REACHED"));
        expect(verdict(run)).toBe("FAIL: reached nats-a nats-b");
        expect(run.code).toBe(1);
      } finally {
        first.stop(true);
        second.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "reads the same holding target as unreachable once it is closed, and passes",
    async () => {
      const holding = holdingTarget(await closedPort());
      const { port } = holding;
      holding.stop(true);
      const origin = await fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(`nats 127.0.0.1 ${port}`),
        });
        expect(lineFor(run, "nats")).toContain(" unreachable; curl exit 7,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      } finally {
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "is unknown, never a pass, when Dispatch answers but the probe cannot connect to the origin itself",
    async () => {
      const origin = await fakeDispatch();
      try {
        // A curl that answers HTTP as the host's does but whose probe of the origin's own port is
        // refused, as behind a proxy that carries the HTTP requests while the device's own connects
        // go nowhere: every round of every target then proves nothing, and reads `uncounted`.
        const path = pathWithCurl(
          `case "$*" in *telnet://127.0.0.1:${origin.port}*) printf '0.000010 0.000000'; exit 7 ;; esac`
        );
        const run = await drive(
          { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
          { path }
        );
        expect(lineFor(run, "origin")).toContain(" unreachable; curl exit 7,");
        expect(lineFor(run, "closed-a")).toContain(" uncounted (round 1 of 5: ");
        expect(probesOf(lineFor(run, "closed-a"))).toEqual(Array(5).fill("uncounted"));
        expect(verdict(run)).toBe("UNKNOWN: the probe could not connect to the origin itself");
        expect(run.code).toBe(2);
      } finally {
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)("is unknown, never a pass, when a target does not resolve", async () => {
    const origin = await fakeDispatch();
    try {
      const run = await drive({
        OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
        OUTSIDE_PATH_TARGETS: await list("ghost does-not-exist.invalid 4222"),
      });
      expect(lineFor(run, "ghost")).toContain(" unresolved");
      expect(verdict(run)).toBe("UNKNOWN: ghost unresolved or unknown");
      expect(run.code).toBe(2);
    } finally {
      await origin.stop();
    }
  });

  // The two reads of the origin take their bounds from OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS and
  // OUTSIDE_PATH_ORIGIN_READ_SECONDS, and from the script's own 3 s and 10 s when both are unset,
  // as on a device that sets neither, or empty. Every run here sets both, so the unset run has bash
  // unset them before the script starts, through BASH_ENV, which bash reads first. The row's own
  // curl answers the origin as Dispatch and logs what each read asked of curl, so no stand-in's
  // speed decides the row.
  probingRow(withoutTools)(
    "bounds each read of the origin by its two settings, and by 3 s to connect and 10 s in all when neither is set",
    async () => {
      const port = await closedPort();
      const origin = `127.0.0.1:${port}`;
      const answered = async (connect: string, read: string, unset = false) => {
        const log = written("");
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://${origin}`,
            OUTSIDE_PATH_TARGETS: await list(),
            OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS: connect,
            OUTSIDE_PATH_ORIGIN_READ_SECONDS: read,
            ...(unset
              ? {
                  BASH_ENV: written(
                    "unset OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS OUTSIDE_PATH_ORIGIN_READ_SECONDS\n"
                  ),
                }
              : {}),
          },
          {
            path: pathWithCurl(
              'case "$*" in\n' +
                `  *http://${origin}/*) printf '%s\\n' "$*" >>${log} ;;\n` +
                "esac\n" +
                'case "$*" in\n' +
                `  *http://${origin}/healthz) printf 200; exit 0 ;;\n` +
                `  *http://${origin}/api/v1/projects) printf '%s401' '{"code":"NO_IDENTITY"}'; exit 0 ;;\n` +
                `  *telnet://${origin}) printf '0.000010 0.000100'; exit 28 ;;\n` +
                "esac"
            ),
          }
        );
        const reads = readFileSync(log, "utf8")
          .trimEnd()
          .split("\n")
          .map((call) => {
            const words = call.split(" ");
            return {
              path: (words.at(-1) ?? "").replace(`http://${origin}`, ""),
              connect: words[words.indexOf("--connect-timeout") + 1],
              read: words[words.indexOf("--max-time") + 1],
            };
          });
        return { reads, verdict: verdict(run), code: run.code };
      };
      const [unset, empty, given] = await Promise.all([
        answered("870", "870", true),
        answered("", ""),
        answered("7", "0.5"),
      ]);
      expect(unset).toEqual({
        reads: [
          { path: "/healthz", connect: "3", read: "10" },
          { path: "/api/v1/projects", connect: "3", read: "10" },
        ],
        verdict: `PASS: every target is unreachable from this device, and http://${origin} answered as Dispatch`,
        code: 0,
      });
      expect(empty).toEqual(unset);
      expect(given.reads).toEqual([
        { path: "/healthz", connect: "7", read: "0.5" },
        { path: "/api/v1/projects", connect: "7", read: "0.5" },
      ]);
      expect(given.code).toBe(0);
    }
  );

  // Answers only a broken network gives, from a curl that names them: an exit no rule classifies,
  // a probe that printed no times, a name lookup that timed out, and one that took so long the
  // connect had too little of its three seconds left to say anything.
  const odd =
    'case "$*" in\n' +
    "  *telnet://exit35.invalid:*) printf '0.000010 0.000000'; exit 35 ;;\n" +
    "  *telnet://blank.invalid:*) exit 28 ;;\n" +
    "  *telnet://slowdns.invalid:*) printf '0.000000 0.000000'; exit 28 ;;\n" +
    "  *telnet://slow.invalid:*) printf '2.966833 0.000000'; exit 28 ;;\n" +
    "esac";

  probingRow(withoutTools)(
    "is unknown, never a pass, when a probe's answer fits no rule or its lookup timed out or ran long",
    async () => {
      const origin = await fakeDispatch();
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(
              "odd exit35.invalid 4222",
              "blank blank.invalid 4222",
              "slowdns slowdns.invalid 4222",
              "slow slow.invalid 4222"
            ),
          },
          { path: pathWithCurl(odd) }
        );
        expect(lineFor(run, "odd")).toBe(
          "target odd exit35.invalid:4222 UNKNOWN (curl exit 35, time_namelookup 0.000010, time_connect 0.000000); probes: UNKNOWN, UNKNOWN, UNKNOWN, UNKNOWN, UNKNOWN"
        );
        expect(lineFor(run, "blank")).toBe(
          "target blank blank.invalid:4222 UNKNOWN (curl exit 28, time_namelookup , time_connect ); probes: UNKNOWN, UNKNOWN, UNKNOWN, UNKNOWN, UNKNOWN"
        );
        expect(lineFor(run, "slowdns")).toBe(
          "target slowdns slowdns.invalid:4222 unresolved (lookup timed out); curl exit 28, time_namelookup 0.000000, time_connect 0.000000; probes: unresolved, unresolved, unresolved, unresolved, unresolved"
        );
        expect(lineFor(run, "slow")).toBe(
          "target slow slow.invalid:4222 UNKNOWN (the lookup took 2.966833 s of the 3 s to connect, too long to read a timed-out connect as unreachable; curl exit 28, time_connect 0.000000); probes: UNKNOWN, UNKNOWN, UNKNOWN, UNKNOWN, UNKNOWN"
        );
        expect(verdict(run)).toBe("UNKNOWN: odd blank slowdns slow unresolved or unknown");
        expect(run.code).toBe(2);
      } finally {
        await origin.stop();
      }
    }
  );

  /** A PATH whose curl answers the probes of 127.0.0.1:`port` in the first `drops` rounds with
   *  `answer` and hands every later one, and every other call, to the host's curl. `pathRounds`
   *  says why each drop falls on the round it names, however late the probe runs. */
  function pathDropping(port: number, drops: number, answer: string): string {
    return pathRounds(
      { dropped: `127.0.0.1:${port}` },
      `[ "$target" = dropped ] && [ "$round" -le ${drops} ] && { ${answer}; }`
    ).path;
  }

  /** The classes a target's line lists after `probes:`, in the order of its rounds. */
  function probesOf(line: string): string[] {
    return (line.split("; probes: ")[1] ?? "").split(", ");
  }

  probingRow(withoutTools)(
    "probes each target in five rounds and reads it reached when one round of the five connects",
    async () => {
      const holding = holdingTarget();
      const origin = await fakeDispatch();
      try {
        // The first two of the five rounds are lost on the way, as on a network dropping packets.
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(`lossy 127.0.0.1 ${holding.port}`),
          },
          { path: pathDropping(holding.port, 2, "printf '0.000020 0.000000'; exit 28") }
        );
        expect(lineFor(run, "lossy")).toContain(" REACHED; curl exit 28, ");
        expect(probesOf(lineFor(run, "lossy"))).toEqual([
          "unreachable",
          "unreachable",
          "REACHED",
          "REACHED",
          "REACHED",
        ]);
        expect(verdict(run)).toBe("FAIL: reached lossy");
        expect(run.code).toBe(1);
      } finally {
        holding.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "reads a target unreachable only when all five rounds are, and unknown when one of them is",
    async () => {
      const port = await closedPort();
      const origin = await fakeDispatch();
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(`flaky 127.0.0.1 ${port}`),
          },
          { path: pathDropping(port, 1, "printf '0.000010 0.000000'; exit 35") }
        );
        expect(lineFor(run, "flaky")).toStartWith(
          `target flaky 127.0.0.1:${port} UNKNOWN (curl exit 35, `
        );
        expect(probesOf(lineFor(run, "flaky"))).toEqual([
          "UNKNOWN",
          "unreachable",
          "unreachable",
          "unreachable",
          "unreachable",
        ]);
        expect(probesOf(lineFor(run, "closed-a"))).toEqual(Array(5).fill("unreachable"));
        expect(verdict(run)).toBe("UNKNOWN: flaky unresolved or unknown");
        expect(run.code).toBe(2);
      } finally {
        await origin.stop();
      }
    }
  );

  // A round in which the device reached not even the origin proves nothing of a target. Here the
  // hub listens: round 1 loses its SYN and from round 2 on the device reaches nothing, the case the
  // review reproduced, where judging each target on its own rounds printed PASS. A second run
  // loses only the origin's third probe. `pathRounds` says why each answer falls on its round.
  probingRow(withoutTools)(
    "counts a target's round only when the probe connected to the origin in that same round",
    async () => {
      const hub = holdingTarget();
      const refused = "{ printf '0.000010 0.000000'; exit 7; }";
      const origin = await fakeDispatch();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const downPath = pathRounds(
          { origin: `127.0.0.1:${origin.port}`, hub: `127.0.0.1:${hub.port}` },
          "case $target in\n" +
            `  origin) [ "$round" -eq 1 ] || ${refused} ;;\n` +
            `  hub) [ "$round" -eq 1 ] && { printf '0.000020 0.000000'; exit 28; }; ${refused} ;;\n` +
            `  *) case "$*" in *telnet://*) ${refused} ;; esac ;;\n` +
            "esac"
        ).path;
        const oncePath = pathRounds(
          { origin: `127.0.0.1:${origin.port}` },
          `[ "$target" = origin ] && [ "$round" -eq 3 ] && ${refused}`
        ).path;
        const [down, once] = await Promise.all([
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${hub.port}`) },
            { path: downPath }
          ),
          drive({ ...env, OUTSIDE_PATH_TARGETS: await list() }, { path: oncePath }),
        ]);
        expect(probesOf(lineFor(down, "origin"))).toEqual([
          "REACHED",
          "unreachable",
          "unreachable",
          "unreachable",
          "unreachable",
        ]);
        expect(lineFor(down, "hub")).toStartWith(
          `target hub 127.0.0.1:${hub.port} uncounted (round 2 of 5: the probe could not connect ` +
            "to the origin in that round, so it proves nothing of this target); curl exit 7, "
        );
        expect(probesOf(lineFor(down, "hub"))).toEqual([
          "unreachable",
          "uncounted",
          "uncounted",
          "uncounted",
          "uncounted",
        ]);
        expect(verdict(down)).toBe(
          "UNKNOWN: the probe could not connect to the origin in rounds 2, 3, 4, 5 of 5, and a " +
            "round without the origin proves nothing of the targets"
        );
        expect(down.code).toBe(2);
        expect(probesOf(lineFor(once, "origin"))).toEqual([
          "REACHED",
          "REACHED",
          "unreachable",
          "REACHED",
          "REACHED",
        ]);
        expect(probesOf(lineFor(once, "closed-a"))).toEqual([
          "unreachable",
          "unreachable",
          "uncounted",
          "unreachable",
          "unreachable",
        ]);
        expect(verdict(once)).toBe(
          "UNKNOWN: the probe could not connect to the origin in round 3 of 5, and a round " +
            "without the origin proves nothing of the targets"
        );
        expect(once.code).toBe(2);
      } finally {
        hub.stop(true);
        await origin.stop();
      }
    }
  );

  // A tunnel that lost its first handshake packet stays down until WireGuard sends it again, after
  // its RekeyTimeout of 5 s plus up to 334 ms, so probes started together would all fall inside
  // that; the rounds start 6 s apart. Here the hub is down in round 1, its curl answering as a
  // refused connect, and listens from round 2 on. The row reads the spacing from the order of what
  // the script ran, not from a clock this host's load moves: `pathRounds` logs each probe of the hub
  // as it starts and each sleep once it has ended, and the host's `sleep 6` ran to its end between
  // each two probes of the hub. Rounds 4 s apart would log `slept 4`, and rounds that did not wait
  // for their sleep would log two probes in a row.
  probingRow(withoutTools)(
    "sleeps a whole 6 s between each two of its five rounds, so a target down only in the first round reads reached",
    async () => {
      const hub = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const { path, log } = pathRounds(
          { hub: `127.0.0.1:${hub.port}` },
          `[ "$target" = hub ] && [ "$round" -eq 1 ] && { printf '0.000010 0.000000'; exit 7; }`
        );
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${hub.port}`),
          },
          { path }
        );
        expect(probesOf(lineFor(run, "hub"))).toEqual([
          "unreachable",
          "REACHED",
          "REACHED",
          "REACHED",
          "REACHED",
        ]);
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
        expect(readFileSync(log, "utf8")).toBe(
          [1, 2, 3, 4, 5].map((round) => `probe hub ${round}\n`).join("slept 6\n")
        );
      } finally {
        hub.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "is unknown when sleep fails, since the rounds were then not spread apart",
    async () => {
      const path = pathOf("bash", "curl");
      writeFileSync(join(path, "sleep"), `#!${bash}\nexit 1\n`, { mode: 0o755 });
      const origin = await fakeDispatch();
      try {
        const run = await drive(
          { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
          { path }
        );
        expect(verdict(run)).toBe(
          "UNKNOWN: sleep failed between the rounds of probes, so they were not 6 s apart"
        );
        expect(run.code).toBe(2);
      } finally {
        await origin.stop();
      }
    }
  );

  const namesOnly =
    "outside-path-reach: warning: every target is a name, and a name proves only where it " +
    "resolves on this device; list each of our services' addresses on the path as lines of their own";

  probingRow(withoutTools)(
    "warns when the list holds only names, since a name proves only where it resolves here",
    async () => {
      const origin = await fakeDispatch();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const [names, addresses] = await Promise.all([
          drive({
            ...env,
            OUTSIDE_PATH_TARGETS: targets(
              `closed-a localhost ${await closedPort()}`,
              `closed-b localhost ${await closedPort()}`
            ),
          }),
          drive({
            ...env,
            OUTSIDE_PATH_TARGETS: await list(`closed-c localhost ${await closedPort()}`),
          }),
        ]);
        expect(names.stdout.split("\n")).toContain(namesOnly);
        expect(verdict(names)).toStartWith("PASS");
        expect(addresses.stdout).not.toContain("warning");
        expect(verdict(addresses)).toStartWith("PASS");
      } finally {
        await origin.stop();
      }
    }
  );

  // Whether `localhost.` resolves is the test host's resolver's business (a stock ubuntu image's
  // does not), so a curl of the row's own answers for it: the row is about the list taking it.
  probingRow(withoutTools)("takes a name written with its trailing dot", async () => {
    const origin = await fakeDispatch();
    try {
      const port = await closedPort();
      const run = await drive(
        {
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(`dotted localhost. ${port}`),
        },
        {
          path: pathWithCurl(
            `case "$*" in *telnet://localhost.:${port}) printf '0.000010 0.000000'; exit 7 ;; esac`
          ),
        }
      );
      expect(lineFor(run, "dotted")).toStartWith(`target dotted localhost.:${port} unreachable; `);
      expect(verdict(run)).toStartWith("PASS");
      expect(run.code).toBe(0);
    } finally {
      await origin.stop();
    }
  });

  probingRow(withoutTools)(
    "reads a list written with a UTF-8 byte-order mark as it reads one without",
    async () => {
      const listening = closingTarget();
      const origin = await fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(`\ufeffhub 127.0.0.1 ${listening.port}`),
        });
        expect(lineFor(run, "hub")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
      } finally {
        listening.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "reads a target list given on a pipe, which it can read only once",
    async () => {
      const listening = closingTarget();
      const origin = await fakeDispatch();
      try {
        const closed = await closedPort();
        const run = await drive(
          { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: "/dev/stdin" },
          { stdin: `closed 127.0.0.1 ${closed}\nhub 127.0.0.1 ${listening.port}\n` }
        );
        expect(run.stdout).toContain("outside-path-reach: targets /dev/stdin (2)\n");
        expect(lineFor(run, "closed")).toContain(" unreachable; ");
        expect(lineFor(run, "hub")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
      } finally {
        listening.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "fails naming every reached target, whatever else the run could not see",
    async () => {
      const hub = closingTarget();
      const nats = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const [beside, blind] = await Promise.all([
          drive(
            {
              ...env,
              OUTSIDE_PATH_TARGETS: await list(
                "odd exit35.invalid 4222",
                `hub 127.0.0.1 ${hub.port}`,
                "slowdns slowdns.invalid 4222",
                `nats 127.0.0.1 ${nats.port}`
              ),
            },
            { path: pathWithCurl(odd) }
          ),
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${hub.port}`) },
            {
              path: pathWithCurl(
                `case "$*" in *telnet://127.0.0.1:${origin.port}*) printf '0.000010 0.000000'; exit 7 ;; esac`
              ),
            }
          ),
        ]);
        expect(verdict(beside)).toBe("FAIL: reached hub nats");
        expect(beside.code).toBe(1);
        expect(lineFor(blind, "origin")).toContain(" unreachable; ");
        expect(verdict(blind)).toBe("FAIL: reached hub");
        expect(blind.code).toBe(1);
      } finally {
        hub.stop(true);
        nats.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "needs only bash, curl and sleep: with no timeout on PATH it still passes, and still fails",
    async () => {
      const path = pathOf("bash", "curl", "sleep");
      const listening = closingTarget();
      const origin = await fakeDispatch();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const [passing, failing] = await Promise.all([
          drive({ ...env, OUTSIDE_PATH_TARGETS: await list() }, { path }),
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${listening.port}`) },
            { path }
          ),
        ]);
        expect(passing.stderr).toBe("");
        expect(verdict(passing)).toStartWith("PASS");
        expect(passing.code).toBe(0);
        expect(lineFor(failing, "hub")).toContain(" REACHED; ");
        expect(verdict(failing)).toBe("FAIL: reached hub");
        expect(failing.code).toBe(1);
      } finally {
        listening.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "reads what the device itself reaches, whatever its curl configuration or proxy says",
    async () => {
      const holding = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const nats = `nats localhost ${holding.port}`;
        const [credentials, resolve, proxy] = await Promise.all([
          // A ~/.curlrc that sends a credential with every request would make the read the
          // script sends with none come back 200.
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list() },
            {
              curlrc:
                'header = "Authorization: Bearer from-curlrc"\nheader = "Cookie: dsession=from-curlrc"\n',
            }
          ),
          // A ~/.curlrc that resolves one target elsewhere would read it unreachable.
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(nats) },
            { curlrc: `resolve = localhost:${holding.port}:${blackhole.host}\n` }
          ),
          // A proxy the origin is exempt from would carry the probe of every other target, and a
          // proxy that refuses would read each of them unreachable.
          drive({
            ...env,
            OUTSIDE_PATH_TARGETS: await list(nats),
            ALL_PROXY: `socks5://127.0.0.1:${await closedPort()}`,
            NO_PROXY: "127.0.0.1",
          }),
        ]);
        expect(verdict(credentials)).toStartWith("PASS");
        expect(credentials.code).toBe(0);
        expect((await origin.seen()).filter((request) => request.credential !== null)).toEqual([]);
        expect(lineFor(resolve, "nats")).toContain(" REACHED; ");
        expect(verdict(resolve)).toBe("FAIL: reached nats");
        expect(lineFor(proxy, "nats")).toContain(" REACHED; ");
        expect(verdict(proxy)).toBe("FAIL: reached nats");
      } finally {
        holding.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "takes an https origin on its default port or an IPv6 origin, and an IPv6 target, in brackets",
    async () => {
      // A curl that answers as Dispatch at each of these origins, reaches each one's own port,
      // and is refused by the IPv6 target; every other call goes to the host's curl.
      const path = pathWithCurl(
        'case "$*" in\n' +
          "  */healthz*) printf 200; exit 0 ;;\n" +
          `  */api/v1/projects*) printf '%s401' '{"code":"NO_IDENTITY"}'; exit 0 ;;\n` +
          "  *'telnet://dispatch-outside.example:443'* | *'telnet://[fd00::1]:443'* | *'telnet://[fd00::1]:8443'*)\n" +
          "    printf '0.000010 0.000100'; exit 28 ;;\n" +
          "  *'telnet://[::1]:4222'*) printf '0.000010 0.000000'; exit 7 ;;\n" +
          "esac"
      );
      const runs = await Promise.all(
        (
          [
            ["https://dispatch-outside.example", "dispatch-outside.example:443"],
            ["https://[fd00::1]", "[fd00::1]:443"],
            ["https://[fd00::1]:8443/", "[fd00::1]:8443"],
          ] as const
        ).map(async ([origin, control]) => ({
          control,
          run: await drive(
            { OUTSIDE_ORIGIN: origin, OUTSIDE_PATH_TARGETS: await list("v6 ::1 4222") },
            { path }
          ),
        }))
      );
      for (const { control, run } of runs) {
        expect(lineFor(run, "origin")).toStartWith(`control origin ${control} REACHED; `);
        expect(lineFor(run, "v6")).toStartWith("target v6 [::1]:4222 unreachable; ");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      }
    }
  );

  probingRow(withoutTools)(
    "takes CRLF line endings, tabs between fields and comments, and reads every target",
    async () => {
      const holding = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const closed = await closedPort();
        const where = written(
          `closed 127.0.0.1 ${closed} # Zurich, Are ~ {x} | 'y'\r\n` +
            `hub\t127.0.0.1\t${holding.port}\t# o\r\n`
        );
        const runs = await Promise.all(
          locales.map(async (locale) => ({
            locale,
            run: await drive({
              OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
              OUTSIDE_PATH_TARGETS: where,
              LC_ALL: locale,
            }),
          }))
        );
        for (const { locale, run } of runs) {
          expect({
            locale,
            closed: run.stdout.includes(`target closed 127.0.0.1:${closed} unreachable; `),
            hub: run.stdout.includes(`target hub 127.0.0.1:${holding.port} REACHED; `),
            said: verdict(run),
            code: run.code,
          }).toEqual({ locale, closed: true, hub: true, said: "FAIL: reached hub", code: 1 });
        }
      } finally {
        holding.stop(true);
        await origin.stop();
      }
    }
  );

  probingRow(withoutTools)(
    "takes an IPv6 address that is not IPv4-mapped, ffff in a later group and ::7f00:1 included",
    async () => {
      const origin = await fakeDispatch();
      try {
        const taken = [
          "::1",
          "0:0:0:0:0:0:0:1",
          "fd00::1",
          "fd7a:115c:a1e0::a136:6940",
          "2001:db8::ffff:1",
          "::7f00:1",
        ];
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(...taken.map((host, at) => `v6-${at} ${host} 4222`)),
          },
          { path: v6Only() }
        );
        expect(
          taken.map((host, at) =>
            lineFor(run, `v6-${at}`).startsWith(`target v6-${at} [${host}]:4222 unreachable; `)
          )
        ).toEqual(taken.map(() => true));
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      } finally {
        await origin.stop();
      }
    }
  );
});
