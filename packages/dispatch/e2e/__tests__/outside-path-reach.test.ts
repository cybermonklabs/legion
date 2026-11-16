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
import { randomUUID } from "node:crypto";
import { mkdirSync, watch, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import type { TCPSocketListener } from "bun";
import {
  bash,
  blackhole,
  closedPort,
  closingTarget,
  fakeDispatch,
  holdingTarget,
  lineFor,
  locales,
  reachFixtures,
  row,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

const { scratch, drive, targets, written, list, pathOf, pathWithCurl, v6Only } = reachFixtures();

describe.concurrent("outside-path-reach.sh", () => {
  row(withoutTools)(
    "passes when the origin answers as Dispatch and every target is unreachable, its control read through time_connect",
    async () => {
      const origin = fakeDispatch();
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
        expect(origin.seen).toEqual([
          { method: "GET", path: "/healthz", credential: null },
          { method: "GET", path: "/api/v1/projects", credential: null },
        ]);
      } finally {
        origin.stop(true);
      }
    }
  );

  row(withoutTools)(
    "fails naming a target that listens, which is the check's negative control",
    async () => {
      const origin = fakeDispatch();
      const listening = closingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${listening.port}`),
        });
        expect(lineFor(run, "hub")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
      } finally {
        origin.stop(true);
        listening.stop(true);
      }
    }
  );

  // Two holding targets beside the holding origin: probed one after another in each round they
  // would take 39 s or more, so a run under 36 shows each round probes them at once.
  row(withoutTools)(
    "reads targets that accept and hold the connection as reached, each round probing them at once",
    async () => {
      const origin = fakeDispatch();
      const first = holdingTarget();
      const second = holdingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(
            `nats-a 127.0.0.1 ${first.port}`,
            `nats-b 127.0.0.1 ${second.port}`
          ),
        });
        expect(lineFor(run, "nats-a")).toContain(" REACHED; curl exit 28,");
        expect(lineFor(run, "nats-b")).toContain(" REACHED; curl exit 28,");
        expect(verdict(run)).toBe("FAIL: reached nats-a nats-b");
        expect(run.code).toBe(1);
        expect(run.seconds).toBeLessThan(36);
      } finally {
        origin.stop(true);
        first.stop(true);
        second.stop(true);
      }
    }
  );

  row(withoutTools)(
    "reads the same holding target as unreachable once it is closed, and passes",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget(await closedPort());
      const { port } = holding;
      holding.stop(true);
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(`nats 127.0.0.1 ${port}`),
        });
        expect(lineFor(run, "nats")).toContain(" unreachable; curl exit 7,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      } finally {
        origin.stop(true);
      }
    }
  );

  row(withoutTools)(
    "is unknown, never a pass, when Dispatch answers but the probe cannot connect to the origin itself",
    async () => {
      const origin = fakeDispatch();
      // A curl that answers HTTP as the host's does but whose probe of the origin's own port is
      // refused, as behind a proxy that carries the HTTP requests while the device's own connects
      // go nowhere: every round of every target then proves nothing, and reads `uncounted`.
      const path = pathWithCurl(
        `case "$*" in *telnet://127.0.0.1:${origin.port}*) printf '0.000010 0.000000'; exit 7 ;; esac`
      );
      try {
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
        origin.stop(true);
      }
    }
  );

  row(withoutTools)("is unknown, never a pass, when a target does not resolve", async () => {
    const origin = fakeDispatch();
    try {
      const run = await drive({
        OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
        OUTSIDE_PATH_TARGETS: await list("ghost does-not-exist.invalid 4222"),
      });
      expect(lineFor(run, "ghost")).toContain(" unresolved");
      expect(verdict(run)).toBe("UNKNOWN: ghost unresolved or unknown");
      expect(run.code).toBe(2);
    } finally {
      origin.stop(true);
    }
  });

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

  row(withoutTools)(
    "is unknown, never a pass, when a probe's answer fits no rule or its lookup timed out or ran long",
    async () => {
      const origin = fakeDispatch();
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
        origin.stop(true);
      }
    }
  );

  /** A PATH whose curl answers the first `drops` probes of 127.0.0.1:`port` with `answer` and hands
   *  every later one, and every other call, to the host's curl. The rounds start 6 s apart, so the
   *  drops fall on the first rounds; a `mkdir` claims each one all the same. */
  function pathDropping(port: number, drops: number, answer: string): string {
    const counter = join(scratch, `drops-${randomUUID()}`);
    mkdirSync(counter);
    const mkdir = hostTool("mkdir") ?? "mkdir";
    const claims = Array.from(
      { length: drops },
      (_, drop) => `${mkdir} ${counter}/${drop} 2>/dev/null`
    );
    return pathWithCurl(
      `case "$*" in *telnet://127.0.0.1:${port}\\ *|*telnet://127.0.0.1:${port}) ` +
        `if ${claims.join(" || ")}; then ${answer}; fi ;; esac`
    );
  }

  /** The classes a target's line lists after `probes:`, in the order of its rounds. */
  function probesOf(line: string): string[] {
    return (line.split("; probes: ")[1] ?? "").split(", ");
  }

  row(withoutTools)(
    "probes each target in five rounds and reads it reached when one round of the five connects",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget();
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
        origin.stop(true);
        holding.stop(true);
      }
    }
  );

  row(withoutTools)(
    "reads a target unreachable only when all five rounds are, and unknown when one of them is",
    async () => {
      const origin = fakeDispatch();
      const port = await closedPort();
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
        origin.stop(true);
      }
    }
  );

  // A round in which the device reached not even the origin proves nothing of a target. Here the
  // hub listens: round 1 loses its SYN and from round 2 on the device reaches nothing, the case the
  // review reproduced, where judging each target on its own rounds printed PASS. A second run
  // loses only the origin's third probe.
  row(withoutTools)(
    "counts a target's round only when the probe connected to the origin in that same round",
    async () => {
      const origin = fakeDispatch();
      const hub = holdingTarget();
      const claims = join(scratch, `claims-${randomUUID()}`);
      mkdirSync(claims);
      const mkdir = hostTool("mkdir") ?? "mkdir";
      const refused = "printf '0.000010 0.000000'; exit 7";
      const origin1 = `*telnet://127.0.0.1:${origin.port}`;
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const [down, once] = await Promise.all([
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${hub.port}`) },
            {
              path: pathWithCurl(
                'case "$*" in\n' +
                  `  ${origin1}) ${mkdir} ${claims}/origin 2>/dev/null || { ${refused}; } ;;\n` +
                  `  *telnet://127.0.0.1:${hub.port}) ${mkdir} ${claims}/hub 2>/dev/null && ` +
                  `{ printf '0.000020 0.000000'; exit 28; }; ${refused} ;;\n` +
                  `  *telnet://*) ${refused} ;;\n` +
                  "esac"
              ),
            }
          ),
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list() },
            {
              path: pathWithCurl(
                `case "$*" in ${origin1}) ` +
                  `if ${mkdir} ${claims}/1 2>/dev/null || ${mkdir} ${claims}/2 2>/dev/null; then :; ` +
                  `elif ${mkdir} ${claims}/3 2>/dev/null; then ${refused}; fi ;; esac`
              ),
            }
          ),
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
        origin.stop(true);
        hub.stop(true);
      }
    }
  );

  // A tunnel that lost its first handshake packet stays down until WireGuard sends it again, after
  // its RekeyTimeout of 5 s plus up to 334 ms; probes started together would all fall inside that.
  // The hub's port refuses until 3.5 s after the first probe of it, which the fake curl marks with a
  // builtin and a watch here sees, so the row needs nothing of the shell's and runs on any bash. The
  // hub's listener stamps each connect with this process's own clock, and the row reads the gaps
  // between those stamps alone, each stamped the same way: rounds 4 s apart would give 4 s gaps,
  // so 5 s leaves a second for this host's scheduling. The 3.5 s is on the real clock on purpose:
  // the script is another process, timed by its own sleep, so no fake timer here can stand in.
  row(withoutTools)(
    "starts its five rounds 6 s apart, so a path down for the first 3.5 s cannot hide a target",
    async () => {
      const origin = fakeDispatch();
      const stamps = join(scratch, `stamps-${randomUUID()}`);
      mkdirSync(stamps);
      const port = await closedPort();
      const connects: number[] = [];
      let first: number | undefined;
      let hub: TCPSocketListener | undefined;
      const up = () => {
        hub = Bun.listen({
          hostname: "127.0.0.1",
          port,
          socket: {
            open(socket) {
              connects.push(performance.now());
              socket.write("INFO {}\r\n");
            },
            data() {},
          },
        });
      };
      const watcher = watch(stamps, (_, name) => {
        if (name !== "first" || first !== undefined) return;
        first = performance.now();
        setTimeout(up, 3500);
      });
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${port}`),
          },
          {
            path: pathWithCurl(
              `case "$*" in *telnet://127.0.0.1:${port}) : >>${stamps}/first ;; esac`
            ),
          }
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
        expect(connects.length).toBe(4);
        const gaps = connects.slice(1).map((connect, at) => (connect - (connects[at] ?? 0)) / 1000);
        expect(gaps.map((gap) => gap >= 5)).toEqual([true, true, true]);
      } finally {
        watcher.close();
        origin.stop(true);
        hub?.stop(true);
      }
    }
  );

  row(withoutTools)(
    "is unknown when sleep fails, since the rounds were then not spread apart",
    async () => {
      const origin = fakeDispatch();
      const path = pathOf("bash", "curl");
      writeFileSync(join(path, "sleep"), `#!${bash}\nexit 1\n`, { mode: 0o755 });
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
        origin.stop(true);
      }
    }
  );

  const namesOnly =
    "outside-path-reach: warning: every target is a name, and a name proves only where it " +
    "resolves on this device; list each of our services' addresses on the path as lines of their own";

  row(withoutTools)(
    "warns when the list holds only names, since a name proves only where it resolves here",
    async () => {
      const origin = fakeDispatch();
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
        origin.stop(true);
      }
    }
  );

  // Whether `localhost.` resolves is the test host's resolver's business (a stock ubuntu image's
  // does not), so a curl of the row's own answers for it: the row is about the list taking it.
  row(withoutTools)("takes a name written with its trailing dot", async () => {
    const origin = fakeDispatch();
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
      origin.stop(true);
    }
  });

  row(withoutTools)(
    "reads a list written with a UTF-8 byte-order mark as it reads one without",
    async () => {
      const origin = fakeDispatch();
      const listening = closingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(`\ufeffhub 127.0.0.1 ${listening.port}`),
        });
        expect(lineFor(run, "hub")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
      } finally {
        origin.stop(true);
        listening.stop(true);
      }
    }
  );

  row(withoutTools)(
    "reads a target list given on a pipe, which it can read only once",
    async () => {
      const origin = fakeDispatch();
      const listening = closingTarget();
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
        origin.stop(true);
        listening.stop(true);
      }
    }
  );

  row(withoutTools)(
    "fails naming every reached target, whatever else the run could not see",
    async () => {
      const origin = fakeDispatch();
      const hub = closingTarget();
      const nats = holdingTarget();
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
        origin.stop(true);
        hub.stop(true);
        nats.stop(true);
      }
    }
  );

  row(withoutTools)(
    "needs only bash, curl and sleep: with no timeout on PATH it still passes, and still fails",
    async () => {
      const path = pathOf("bash", "curl", "sleep");
      const origin = fakeDispatch();
      const listening = closingTarget();
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
        origin.stop(true);
        listening.stop(true);
      }
    }
  );

  row(withoutTools)(
    "reads what the device itself reaches, whatever its curl configuration or proxy says",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget();
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
        expect(origin.seen.filter((request) => request.credential !== null)).toEqual([]);
        expect(lineFor(resolve, "nats")).toContain(" REACHED; ");
        expect(verdict(resolve)).toBe("FAIL: reached nats");
        expect(lineFor(proxy, "nats")).toContain(" REACHED; ");
        expect(verdict(proxy)).toBe("FAIL: reached nats");
      } finally {
        origin.stop(true);
        holding.stop(true);
      }
    }
  );

  row(withoutTools)(
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

  row(withoutTools)(
    "takes CRLF line endings, tabs between fields and comments, and reads every target",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget();
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
        origin.stop(true);
        holding.stop(true);
      }
    }
  );

  row(withoutTools)(
    "takes an IPv6 address that is not IPv4-mapped, ffff in a later group and ::7f00:1 included",
    async () => {
      const origin = fakeDispatch();
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
        origin.stop(true);
      }
    }
  );
});
