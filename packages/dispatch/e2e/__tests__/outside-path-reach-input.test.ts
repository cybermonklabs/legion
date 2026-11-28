/** What `e2e/acceptance/outside-path-reach.sh` decides before any probe: the inputs it refuses
 *  (the origin, the target list and its lines, the shell, the tools on PATH), the origin's answers
 *  to its two HTTP reads, and its classifier, driven through the script's classify hook. No row
 *  here reaches the probes, so none needs a network beyond the loopback fake Dispatch most of
 *  them stand up. `outside-path-reach-fixtures.ts` holds the fixtures and says how they work.
 */

import { describe, expect } from "bun:test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { hostname } from "node:os";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import {
  bash,
  byteRefusal,
  closedPort,
  fakeDispatch,
  holdingTarget,
  locales,
  reachFixtures,
  row,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

const { scratch, drive, driveEach, targets, written, list, pathOf, pathWithCurl, v6Only } =
  reachFixtures();

describe.concurrent("outside-path-reach.sh", () => {
  // Five recorded probes judged in order, through the script's classify hook, so the odd answer
  // stands at a position each row fixes rather than one a race picks.
  const reached = "28 0.000027 0.000113";
  const refused = "7 0.000010 0.000000";
  const odd35 = "35 0.000010 0.000000";
  const reachedLine = "REACHED; curl exit 28, time_namelookup 0.000027, time_connect 0.000113";
  const odd35Line = "UNKNOWN (curl exit 35, time_namelookup 0.000010, time_connect 0.000000)";

  row(withoutTools)(
    "judges a target's five rounds in order: one connect is reached, one odd answer is unknown, wherever it stands",
    async () => {
      const path = pathOf("bash");
      const allRefused = Array(5).fill(refused) as string[];
      const cases: [string[], string][] = [
        [
          allRefused,
          `unreachable; curl exit 7, time_namelookup 0.000010, time_connect 0.000000; probes: ${Array(5).fill("unreachable").join(", ")}`,
        ],
        [
          [odd35, reached, refused, refused, refused],
          `${reachedLine}; probes: UNKNOWN, REACHED, unreachable, unreachable, unreachable`,
        ],
        [
          [refused, refused, refused, odd35, reached],
          `${reachedLine}; probes: unreachable, unreachable, unreachable, UNKNOWN, REACHED`,
        ],
      ];
      // The one odd answer at each position in turn, the other rounds refused.
      for (let position = 0; position < 5; position++) {
        for (const [answer, line, name] of [
          [reached, reachedLine, "REACHED"],
          [odd35, odd35Line, "UNKNOWN"],
        ] as const) {
          const classes = allRefused.map((_, round) => (round === position ? name : "unreachable"));
          cases.push([
            allRefused.map((each, round) => (round === position ? answer : each)),
            `${line}; probes: ${classes.join(", ")}`,
          ]);
        }
      }
      const recorded = cases.map(([rounds]) => rounds.join("; "));
      const runs = await driveEach(
        recorded.map((each) => ({ OUTSIDE_PATH_CLASSIFY: each })),
        { path }
      );
      expect(
        runs.map((run, n) => ({
          recorded: recorded[n],
          judged: run.stdout.trimEnd(),
          code: run.code,
        }))
      ).toEqual(
        cases.map(([rounds, want]) => ({ recorded: rounds.join("; "), judged: want, code: 4 }))
      );
    }
  );
  row(withoutTools)("fails naming an origin that serves a read with no credential", async () => {
    const origin = await fakeDispatch(200, 200);
    try {
      const where = `http://127.0.0.1:${origin.port}`;
      const run = await drive({ OUTSIDE_ORIGIN: where, OUTSIDE_PATH_TARGETS: await list() });
      expect(verdict(run)).toBe(`FAIL: ${where} served a read with no credential`);
      expect(run.code).toBe(1);
    } finally {
      await origin.stop();
    }
  });

  row(withoutTools)(
    "is unknown when the origin's answers are not Dispatch's: a health route that fails, a read that is not refused, or a refusal that is not Dispatch's own",
    async () => {
      const answers = [
        [503, 401, { code: "NO_IDENTITY" }],
        [200, 403, { code: "NO_IDENTITY" }],
        [200, 302, { code: "NO_IDENTITY" }],
        // A proxy of its own that refuses every path but a health route it answers itself.
        [200, 401, { error: "unauthorized" }],
      ] as const;
      const origins = await Promise.all(
        answers.map(([health, projects, refusal]) => fakeDispatch(health, projects, refusal))
      );
      try {
        const wheres = origins.map((origin) => `http://127.0.0.1:${origin.port}`);
        const runs = await driveEach(
          await Promise.all(
            wheres.map(async (where) => ({
              OUTSIDE_ORIGIN: where,
              OUTSIDE_PATH_TARGETS: await list(),
            }))
          )
        );
        expect(
          runs.map((run, n) => ({ answer: answers[n], verdict: verdict(run), code: run.code }))
        ).toEqual(
          answers.map((answer, n) => ({
            answer,
            verdict: `UNKNOWN: ${wheres[n]} did not answer as Dispatch; reached nothing, so proved nothing`,
            code: 2,
          }))
        );
      } finally {
        await Promise.all(origins.map((origin) => origin.stop()));
      }
    }
  );

  row(withoutTools)(
    "is unknown when nothing answers at the origin, since it reached nothing",
    async () => {
      const where = `http://127.0.0.1:${await closedPort()}`;
      const run = await drive({ OUTSIDE_ORIGIN: where, OUTSIDE_PATH_TARGETS: await list() });
      expect(verdict(run)).toBe(
        `UNKNOWN: ${where} did not answer as Dispatch; reached nothing, so proved nothing`
      );
      expect(run.code).toBe(2);
    }
  );

  // A run's output can be posted where anyone reads it, such as a public CI log, so it names this
  // device's host name nowhere: neither a HOSTNAME the environment hands bash, which bash keeps,
  // nor the one bash reads from the system where the environment names none. Both runs stop at an
  // origin that does not answer, after the header lines, and each must have printed them.
  row(withoutTools)(
    "prints the device's host name in no line, whether the environment names it or bash reads it",
    async () => {
      const where = `http://127.0.0.1:${await closedPort()}`;
      const planted = `planted-host-${randomUUID()}`;
      for (const { name, env } of [
        { name: planted, env: { HOSTNAME: planted } },
        { name: hostname(), env: {} },
      ]) {
        const run = await drive({
          OUTSIDE_ORIGIN: where,
          OUTSIDE_PATH_TARGETS: await list(),
          ...env,
        });
        expect(run.stdout).toContain(`outside-path-reach: origin ${where}\n`);
        expect(run.stdout.split("\n").filter((line) => line.includes(name))).toEqual([]);
      }
    }
  );

  row(withoutTools)(
    "refuses an origin that is not an https origin alone, unless it is the local harness's with the flag set",
    async () => {
      const path = await list();
      const origins = [
        ["http://127.0.0.1:8766", ""],
        ["http://dispatch-outside.example", "1"],
        ["https://dispatch-outside.example/path", "1"],
        ["https://user@dispatch-outside.example", "1"],
        ["https://dispatch-outside.example:0443", "1"],
        ["https://::1", "1"],
        ["https://010.0.0.1", "1"],
        ["https://dispatch-outside.0x1", "1"],
        ["https://[::ffff:127.0.0.1]", "1"],
        ["https://[::127.0.0.1]:8443", "1"],
        ["https://[0:0::ffff:7f00:1]", "1"],
        ["https://[0000:0000:0000:0000:0000:ffff:7f00:0001]:8443", "1"],
        // One rule for the origin's host and a target's: no empty label, and an IPv6 address is
        // hexadecimal digits and colons alone, so curl reads none as a URL range.
        ["https://a..b", "1"],
        ["https://.example", "1"],
        ["https://[1-1:1]", "1"],
        ["https://[fd00::g]", "1"],
        ["https://[1.2.3.4]", "1"],
        ["https://[.::1]", "1"],
        ["ftp://dispatch-outside.example", "1"],
      ] as const;
      const runs = await driveEach(
        origins.map(([origin, allow]) => ({
          OUTSIDE_ORIGIN: origin,
          OUTSIDE_PATH_ALLOW_HTTP: allow,
          OUTSIDE_PATH_TARGETS: path,
        }))
      );
      expect(
        runs.map((run, n) => ({
          origin: origins[n]?.[0],
          verdict: verdict(run).startsWith("UNKNOWN: OUTSIDE_ORIGIN "),
          code: run.code,
        }))
      ).toEqual(origins.map(([origin]) => ({ origin, verdict: true, code: 2 })));
    }
  );

  row(withoutTools)(
    "refuses an origin-read bound that is not a whole number of seconds from 2 to 999999, naming the setting, before going on the network",
    async () => {
      const origin = await fakeDispatch();
      try {
        const path = await list();
        const settings = [
          "OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS",
          "OUTSIDE_PATH_ORIGIN_READ_SECONDS",
        ] as const;
        // A curl built with the synchronous resolver gives up before it resolves a name with under
        // 1000 ms of the bound left, which a 1 s bound reaches once 1 ms has passed; curl reads 0
        // as no bound; and curl 7.55 and later refuse a bound past 2147483 s where a long has 32
        // bits: so 1, 0.5, 0 and 1000000 are refused, as is anything not digits.
        const values = [
          "abc",
          "0",
          "00",
          "1",
          "01",
          "0.0",
          "0.5",
          "1.5",
          "0.0009",
          "1000000",
          "99999999999999999999",
          "-1",
          ".5",
          "5.",
          "1e3",
          "5s",
          "1.2.3",
          " 3",
        ];
        const cases = settings.flatMap((setting) => values.map((value) => ({ setting, value })));
        const runs = await driveEach(
          cases.map(({ setting, value }) => ({
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: path,
            [setting]: value,
          }))
        );
        expect(
          runs.map((run, n) => ({ ...cases[n], verdict: verdict(run), code: run.code }))
        ).toEqual(
          cases.map(({ setting, value }) => ({
            setting,
            value,
            verdict: `UNKNOWN: ${setting} must be a whole number of seconds from 2 to 999999, not ${value}`,
            code: 2,
          }))
        );
        expect(await origin.seen()).toEqual([]);
      } finally {
        await origin.stop();
      }
    }
  );

  row(withoutTools)(
    "classifies each recorded triple by whether TCP connected, and by whether the lookup did, exiting 4, never PASS's 0",
    async () => {
      const path = pathOf("bash");
      const refusals = [
        ["", "UNKNOWN: OUTSIDE_PATH_CLASSIFY is set but empty"],
        ["   ", "UNKNOWN: OUTSIDE_PATH_CLASSIFY is set but empty"],
        ["28 0.1 0.0 7", "UNKNOWN: OUTSIDE_PATH_CLASSIFY is neither"],
        ["7 0.1 0.0; 7 0.1 0.0", "UNKNOWN: OUTSIDE_PATH_CLASSIFY is neither"],
        ["7 0.1 0.0; 7 0.1 0.0; 7 0.1 0.0", "UNKNOWN: OUTSIDE_PATH_CLASSIFY is neither"],
        [Array(6).fill("7 0.1 0.0").join("; "), "UNKNOWN: OUTSIDE_PATH_CLASSIFY is neither"],
      ] as const;
      const classes = [
        // A resolver that never answered: the lookup timed out, so nothing was tried.
        ["28 0.000000 0.000000", "unresolved (lookup timed out)"],
        // A blackholed address: the lookup completed and the connect timed out.
        ["28 0.000027 0.000000", "unreachable"],
        ["28 0.999999 0.000000", "unreachable"],
        // A lookup that took a second or more left the connect too little of its three seconds.
        [
          "28 1.000000 0.000000",
          "UNKNOWN (the lookup took 1.000000 s of the 3 s to connect, too long to read a timed-out connect as unreachable; curl exit 28, time_connect 0.000000)",
        ],
        [
          "28 2.966833 0.000000",
          "UNKNOWN (the lookup took 2.966833 s of the 3 s to connect, too long to read a timed-out connect as unreachable; curl exit 28, time_connect 0.000000)",
        ],
        ["28 0.000027 0.000113", "REACHED"],
        ["7 0.000010 0.000000", "unreachable"],
        // A completed connect is REACHED whatever the exit.
        ["7 0.000010 0.000200", "REACHED"],
        ["6 0.000000 0.000000", "unresolved"],
        [
          "35 0.000010 0.000000",
          "UNKNOWN (curl exit 35, time_namelookup 0.000010, time_connect 0.000000)",
        ],
        ["28  ", "UNKNOWN (curl exit 28, time_namelookup , time_connect )"],
        [
          "28 0.000027 0.0.0",
          "UNKNOWN (curl exit 28, time_namelookup 0.000027, time_connect 0.0.0)",
        ],
      ] as const;
      const runs = await driveEach(
        [...refusals, ...classes].map(([recorded]) => ({ OUTSIDE_PATH_CLASSIFY: recorded })),
        { path }
      );
      expect(
        refusals.map(([recorded, want], n) => ({
          recorded,
          refused: runs[n]?.stdout.startsWith(want),
          code: runs[n]?.code,
        }))
      ).toEqual(refusals.map(([recorded]) => ({ recorded, refused: true, code: 2 })));
      expect(
        classes.map(([triple], n) => ({
          triple,
          class: runs[refusals.length + n]?.stdout.trimEnd(),
          code: runs[refusals.length + n]?.code,
        }))
      ).toEqual(classes.map(([triple, want]) => ({ triple, class: want, code: 4 })));
    }
  );
  for (const shell of ["dash", "zsh"]) {
    row(withoutTools || hostTool(shell) === null)(
      `refuses to run under ${shell}, naming bash`,
      async () => {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: "https://dispatch-outside.example",
            OUTSIDE_PATH_TARGETS: await list(),
          },
          { shell: hostTool(shell) ?? shell }
        );
        expect(run.stdout).toBe("UNKNOWN: run this with bash: bash outside-path-reach.sh\n");
        expect(run.code).toBe(2);
      }
    );
  }

  // bash 3.2 cuts a line at a NUL byte, so `42<NUL>22` would be probed as port 42; bash 5 drops
  // the byte and probes 4222. Neither is the line as written, so a list holding one is refused.
  row(withoutTools)("refuses a target list holding a NUL byte", async () => {
    const holding = holdingTarget();
    const origin = await fakeDispatch();
    try {
      const port = String(holding.port);
      const where = await list(`nats 127.0.0.1 ${port.slice(0, 2)}\0${port.slice(2)}`);
      const run = await drive({
        OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
        OUTSIDE_PATH_TARGETS: where,
      });
      expect(verdict(run)).toBe(byteRefusal(where, readFileSync(where).indexOf(0)));
      expect(run.code).toBe(2);
      expect(await origin.seen()).toEqual([]);
    } finally {
      holding.stop(true);
      await origin.stop();
    }
  });

  // A read that stops before the end of the list, here by a TMOUT=1 the device's shell makes
  // readonly (through BASH_ENV, which bash reads at its start), so the script cannot unset it.
  // The list's first line comes at once and the pipe then stays open with nothing more until the
  // script exits, so the read can end only at its timeout, however late this host runs the script.
  // bash 5 says so with read's status 142; bash 3.2 gives 1 and keeps nothing it read, as at the
  // end of an empty list. Either way the run refuses before it goes on the network.
  row(withoutTools)(
    "refuses a target list whose read stopped before the end of the list",
    async () => {
      const major = Number(
        Bun.spawnSync([bash, "-c", `printf %s "\${BASH_VERSINFO[0]}"`]).stdout.toString()
      );
      const origin = await fakeDispatch();
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: "/dev/stdin",
            BASH_ENV: written("readonly TMOUT=1\n"),
          },
          { stdin: `closed 127.0.0.1 ${await closedPort()}\n`, stdinHeld: true }
        );
        expect(verdict(run)).toBe(
          major >= 4
            ? "UNKNOWN: OUTSIDE_PATH_TARGETS names /dev/stdin, which this device could not read " +
                "whole: the read stopped with status 142 before the end of the list"
            : "UNKNOWN: /dev/stdin lists no target: nothing was tried, so nothing is proved"
        );
        expect(run.code).toBe(2);
        expect(await origin.seen()).toEqual([]);
      } finally {
        await origin.stop();
      }
    }
  );

  row(withoutTools)("is unknown when curl is missing", async () => {
    const origin = await fakeDispatch();
    try {
      const run = await drive(
        { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
        { path: pathOf("bash") }
      );
      expect(verdict(run)).toStartWith("UNKNOWN: curl missing");
      expect(run.code).toBe(2);
    } finally {
      await origin.stop();
    }
  });

  row(withoutTools)("is unknown when sleep is missing", async () => {
    const origin = await fakeDispatch();
    try {
      const run = await drive(
        { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
        { path: pathOf("bash", "curl") }
      );
      expect(verdict(run)).toStartWith("UNKNOWN: sleep missing");
      expect(run.code).toBe(2);
      expect(await origin.seen()).toEqual([]);
    } finally {
      await origin.stop();
    }
  });

  row(withoutTools)("is unknown when curl is older than 7.40", async () => {
    const path = pathWithCurl(
      `case "$*" in *--version*) echo 'curl 7.39.0 (x86_64-redhat-linux-gnu) libcurl/7.39.0'; exit 0 ;; esac`
    );
    const origin = await fakeDispatch();
    try {
      const run = await drive(
        { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
        { path }
      );
      expect(verdict(run)).toBe(
        "UNKNOWN: curl missing or too old: this curl is '7.39.0'; install curl 7.40 or later"
      );
      expect(run.code).toBe(2);
    } finally {
      await origin.stop();
    }
  });

  row(withoutTools)("is unknown when the target file lists no target", async () => {
    const origin = await fakeDispatch();
    try {
      const run = await drive({
        OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
        OUTSIDE_PATH_TARGETS: targets("# nothing listed yet", ""),
      });
      expect(verdict(run)).toEndWith("lists no target: nothing was tried, so nothing is proved");
      expect(verdict(run)).toStartWith("UNKNOWN: ");
      expect(run.code).toBe(2);
    } finally {
      await origin.stop();
    }
  });

  row(withoutTools)("refuses each missing input by name", async () => {
    const noOrigin = await drive({ OUTSIDE_PATH_TARGETS: await list() });
    expect(verdict(noOrigin)).toStartWith("UNKNOWN: OUTSIDE_ORIGIN is unset");
    expect(noOrigin.code).toBe(2);
    const noTargets = await drive({ OUTSIDE_ORIGIN: "https://dispatch-outside.example" });
    expect(verdict(noTargets)).toStartWith("UNKNOWN: OUTSIDE_PATH_TARGETS is unset");
    expect(noTargets.code).toBe(2);
  });

  row(withoutTools)(
    "refuses a target list it cannot read whole or a line it cannot take, before going on the network",
    async () => {
      // A list with a byte outside printable ASCII, tab and LF at `offset`, counted from 0.
      const bad = (text: string, offset: number) => {
        const where = written(text);
        return [where, byteRefusal(where, offset)] as const;
      };
      const origin = await fakeDispatch();
      try {
        const refused = [
          [await list("hub 127.0.0.1"), "is not 'name host port': hub 127.0.0.1"],
          [
            await list("hub svc:envoy-hub 4222"),
            "target hub: svc:envoy-hub is a Tailscale service selector",
          ],
          [await list("hub host_name 4222"), "target hub: host_name is neither a host name"],
          // curl reads [1-1:1] as a range, and would probe telnet://1:4222 in its place.
          [await list("hub 1-1:1 4222"), "target hub: 1-1:1 is neither a host name nor an address"],
          [await list("hub fd00::g 4222"), "target hub: fd00::g is neither a host name nor an"],
          [await list("hub a..b 4222"), "target hub: a..b is neither a host name nor an address"],
          [await list("hub 127.0.0.1 0"), "target hub: 0 is not a TCP port"],
          [await list("hub 127.0.0.1 0443"), "target hub: 0443 is not a TCP port"],
          [await list("hub 127.0.0.1 65536"), "target hub: 65536 is not a TCP port"],
          [await list("hub 127.0.0.1 nats"), "target hub: nats is not a TCP port"],
          // An address written any way but the canonical dotted quad names another host than it
          // seems to: curl reads 172.020.000.021 in octal, as 172.16.0.17.
          [
            await list("hub 172.020.000.021 4222"),
            "target hub: 172.020.000.021 is not an IPv4 address written as four numbers",
          ],
          [await list("hub 127.1 4222"), "target hub: 127.1 is not an IPv4 address written"],
          [await list("hub 10.0.0.256 4222"), "target hub: 10.0.0.256 is not an IPv4 address"],
          [await list("hub 0x7f000001 4222"), "target hub: 0x7f000001 is not an IPv4 address"],
          [await list("hub 127.0.0.0x1 4222"), "target hub: 127.0.0.0x1 is not an IPv4 address"],
          // A file saved with CR line endings alone is one line to `read`, and an inline comment
          // on its first target would cut every later one away: a CR is taken only where an LF
          // follows it at once, so a lone one at the end and one before anything else refuse.
          bad("closed 127.0.0.1 1 # closed port\rhub 127.0.0.1 4222\r", 32),
          bad("a 127.0.0.1 1\nb 127.0.0.1 2\r", 27),
          bad("a 127.0.0.1 1\r\r\n", 13),
          bad("a 127.0.0.1 1\rb 127.0.0.1 2\n", 13),
          // A byte-order mark is taken at offset 0 alone.
          bad("a 127.0.0.1 1\n\ufeffb 127.0.0.1 2\n", 14),
          bad("\ufeff\ufeffa 127.0.0.1 1\n", 3),
          // The offset counts every byte before it, the CR of a CRLF line included.
          bad("a 127.0.0.1 1\r\nb 127.0.0.1 2\nc 127.0.0.1 3 # \u00e9\n", 45),
          // An IPv6 address holding an IPv4 one names another host than the IPv4 address does
          // (::127.0.0.1 is not loopback), or none on a v6-only stack (::ffff:127.0.0.1).
          [
            await list("hub ::127.0.0.1 4222"),
            "target hub: ::127.0.0.1 is an IPv6 form of an IPv4 address; write the IPv4 address itself",
          ],
          [await list("hub ::ffff:127.0.0.1 4222"), "target hub: ::ffff:127.0.0.1 is an IPv6 form"],
          [await list("hub ::FFFF:7f00:1 4222"), "target hub: ::FFFF:7f00:1 is an IPv6 form"],
          // A dot before a colon is no part of any address, and the likeliest typo writes the port
          // into the host.
          [await list("hub .::1 4222"), "target hub: .::1 is neither a host name nor an address"],
          [
            await list("hub a.b::1 4222"),
            "target hub: a.b::1 is neither a host name nor an address",
          ],
          [await list("hub 1.2:3 4222"), "target hub: 1.2:3 is neither a host name nor an address"],
          [
            await list("hub 10.0.0.1:4222 4222"),
            "target hub: 10.0.0.1:4222 is neither a host name nor an address",
          ],
        ] as const;
        const runs = await driveEach(
          refused.map(([where]) => ({
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: where,
          }))
        );
        expect(
          runs.map((run) => ({
            unknown: verdict(run).startsWith("UNKNOWN: "),
            verdict: verdict(run),
            code: run.code,
          }))
        ).toEqual(
          refused.map(([, reason]) => ({
            unknown: true,
            verdict: expect.stringContaining(reason),
            code: 2,
          }))
        );
        expect(await origin.seen()).toEqual([]);
        // A list it cannot open, or a read error, refuses with bash's own words at the end.
        const [directory, missing] = await driveEach(
          [scratch, join(scratch, "no-such-list.txt")].map((where) => ({
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: where,
          }))
        );
        if (directory === undefined || missing === undefined)
          throw new Error("two runs, two results");
        expect(verdict(directory)).toMatch(
          /^UNKNOWN: OUTSIDE_PATH_TARGETS names \S+, which this device could not read whole: .*read error: 0: Is a directory$/
        );
        expect(directory.code).toBe(2);
        expect(verdict(missing)).toEndWith(
          "which this device could not read whole: could not open it"
        );
        expect(missing.code).toBe(2);
        expect(await origin.seen()).toEqual([]);
      } finally {
        await origin.stop();
      }
    }
  );

  // A byte that `read` does not split on, written after an inline comment, puts the next target
  // in the comment: here that target is a listening hub, so the run would pass without it.
  const separators: [string, string][] = [
    ["SOH", "\x01"],
    ["BS", "\x08"],
    ["VT", "\x0b"],
    ["FF", "\x0c"],
    ["CR alone", "\r"],
    ["ESC", "\x1b"],
    ["US", "\x1f"],
    ["DEL", "\x7f"],
    ["NEL", "\u0085"],
    ["LS", "\u2028"],
    ["PS", "\u2029"],
  ];

  row(withoutTools)(
    "refuses a line holding a control character or a Unicode line break, which would hide the target after it",
    async () => {
      const holding = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const closed = await closedPort();
        const cases = separators.flatMap(([name, separator]) =>
          locales.map((locale) => ({
            name,
            locale,
            where: written(
              `closed 127.0.0.1 ${closed} # closed port${separator}hub 127.0.0.1 ${holding.port}\n`
            ),
          }))
        );
        const runs = await driveEach(
          cases.map(({ locale, where }) => ({
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: where,
            LC_ALL: locale,
          }))
        );
        expect(
          runs.map((run, n) => ({
            name: cases[n]?.name,
            locale: cases[n]?.locale,
            said: verdict(run),
            code: run.code,
          }))
        ).toEqual(
          cases.map(({ name, locale, where }) => ({
            name,
            locale,
            said: byteRefusal(where, `closed 127.0.0.1 ${closed} # closed port`.length),
            code: 2,
          }))
        );
        expect(await origin.seen()).toEqual([]);
      } finally {
        holding.stop(true);
        await origin.stop();
      }
    }
  );

  // NEL in Latin-1 is the one byte 0x85, which no UTF-8 line break is: a reader of that encoding
  // splits the line there, and the hub after it would be lost in the comment, the run passing.
  row(withoutTools)(
    "refuses the raw byte 0x85 after a comment, which would hide the target after it",
    async () => {
      const holding = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const prefix = `closed 127.0.0.1 ${await closedPort()} # closed port`;
        const cases = locales.map((locale) => ({
          locale,
          where: written(
            Buffer.concat([
              Buffer.from(prefix),
              Buffer.from([0x85]),
              Buffer.from(`hub 127.0.0.1 ${holding.port}\n`),
            ])
          ),
        }));
        const runs = await driveEach(
          cases.map(({ locale, where }) => ({
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: where,
            LC_ALL: locale,
          }))
        );
        expect(
          runs.map((run, n) => ({ locale: cases[n]?.locale, said: verdict(run), code: run.code }))
        ).toEqual(
          cases.map(({ locale, where }) => ({
            locale,
            said: byteRefusal(where, prefix.length),
            code: 2,
          }))
        );
        expect(await origin.seen()).toEqual([]);
      } finally {
        holding.stop(true);
        await origin.stop();
      }
    }
  );

  row(withoutTools)(
    "refuses every byte after a comment but printable ASCII, tab and LF, which it takes",
    async () => {
      // A curl this old stops a list the script takes before the network, so no probe runs.
      const path = pathWithCurl(
        `case "$*" in *--version*) echo 'curl 7.39.0 (x86_64-redhat-linux-gnu) libcurl/7.39.0'; exit 0 ;; esac`
      );
      const taken =
        "UNKNOWN: curl missing or too old: this curl is '7.39.0'; install curl 7.40 or later";
      const prefix = "closed 127.0.0.1 1 # closed port";
      const cases = locales.flatMap((locale) =>
        Array.from({ length: 256 }, (_, byte) => ({ locale, byte }))
      );
      // 16 runners at once, each running its share of the cases one after another: 16 runs at a
      // time, from 16 children of the test process for the 768 runs. Each runner is a Bun.spawn the
      // test process waits on, and under the load rowTimeout's reading names the row took at most
      // 23.7 s with 16 and 24.2 s with 64. No run reaches the network: a taken byte stops at the
      // old curl, a refused one before it.
      const runners = 16;
      const shares = Array.from({ length: runners }, (_, runner) =>
        cases
          .filter((_, index) => index % runners === runner)
          .map(({ locale, byte }) => ({
            locale,
            byte,
            where: written(
              Buffer.concat([
                Buffer.from(prefix),
                Buffer.from([byte]),
                Buffer.from("hub 127.0.0.1 4222\n"),
              ])
            ),
          }))
      );
      const runs = await Promise.all(
        shares.map((share) =>
          driveEach(
            share.map(({ locale, where }) => ({
              OUTSIDE_ORIGIN: "https://dispatch-outside.example",
              OUTSIDE_PATH_TARGETS: where,
              LC_ALL: locale,
            })),
            { path, together: 1 }
          )
        )
      );
      const wrong = shares.flatMap((share, runner) =>
        share.flatMap(({ locale, byte, where }, n) => {
          const allowed = byte === 0x09 || byte === 0x0a || (byte >= 0x20 && byte <= 0x7e);
          const said = verdict(runs[runner]?.[n] ?? { code: -1, stdout: "", stderr: "" });
          return said === (allowed ? taken : byteRefusal(where, prefix.length))
            ? []
            : [{ locale, byte: byte.toString(16).padStart(2, "0"), said }];
        })
      );
      expect(shares.flat().length).toBe(256 * locales.length);
      expect(wrong).toEqual([]);
    }
  );

  // Each spelling of an IPv4-mapped address, and two with a dotted IPv4 part. A v6-only device
  // (OpenBSD, or Linux with bindv6only=1) connects to none of them, so the hub each names, which
  // listens on 127.0.0.1, would read unreachable there and the run would pass.
  const mapped = [
    "::ffff:127.0.0.1",
    "::127.0.0.1",
    "::ffff:7f00:1",
    "::FFFF:7f00:1",
    "0:0:0:0:0:ffff:7f00:1",
    "0:0::ffff:7f00:1",
    "::0:ffff:7f00:1",
    "0::ffff:7f00:1",
    "0000:0000:0000:0000:0000:ffff:7f00:0001",
  ];

  row(withoutTools)(
    "refuses every spelling of an IPv4-mapped address, which a v6-only device would read unreachable",
    async () => {
      const holding = holdingTarget();
      const origin = await fakeDispatch();
      try {
        const path = v6Only();
        const runs = await driveEach(
          await Promise.all(
            mapped.map(async (host) => ({
              OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
              OUTSIDE_PATH_TARGETS: await list(`hub ${host} ${holding.port}`),
            }))
          ),
          { path }
        );
        expect(
          runs.map((run, n) => ({ host: mapped[n], said: verdict(run), code: run.code }))
        ).toEqual(
          mapped.map((host) => ({
            host,
            said: `UNKNOWN: target hub: ${host} is an IPv6 form of an IPv4 address; write the IPv4 address itself`,
            code: 2,
          }))
        );
        expect(await origin.seen()).toEqual([]);
      } finally {
        holding.stop(true);
        await origin.stop();
      }
    }
  );
});
