/** What `e2e/acceptance/outside-edge-refusals.sh` decides from the answers it reads, and its
 *  planted-route control. Each row runs the script against a stand-in edge, or a `curl` of the
 *  row's own, from `outside-edge-refusals-fixtures.ts`, whose head says how each answers. Case (b)
 *  of the plan plants a route for exactly one refusal line, once per line, and the run must fail
 *  naming that line and no other: the check's own control, run before merge, as the live canary is
 *  after. The inputs the script refuses are `outside-edge-refusals-input.test.ts`'s rows, and the
 *  signals `outside-edge-refusals-signal.test.ts`'s.
 */

import { describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, readdirSync, symlinkSync, writeFileSync } from "node:fs";
import { hostname } from "node:os";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { rowLimit, runBounded } from "./bounded-subprocess";
import type { Route } from "./outside-edge-refusals-edge";
import {
  canaryBody,
  edgeFixtures,
  httpsArgv,
  httpsControls,
  type Line,
  linesOf,
  markerNotVisible,
  methodRefused,
  notFound,
  passVerdict,
  planted,
  probeLine,
  probesIn,
  production,
  refusals,
  refusalTable,
  refusedLine,
  routed,
  routedTable,
  said,
  scriptName,
  sent,
  sentFor,
  sha,
  standIn,
  unroutedHost,
  unroutedLine,
  visibleControls,
  withoutTools,
} from "./outside-edge-refusals-fixtures";
import { bash, closedPort, curl, verdict } from "./outside-path-reach-fixtures";

const { check, directoryOf, httpsCurl, listFile, planLists, scratch } = edgeFixtures(
  import.meta.url
);

/** The refusal lines of five fields: each has its unrouted control sent before it. */
const fiveFields = refusals.filter((line) => line.expect === undefined);

// The rows run one after another, not concurrently. Each waits for a bounded child, and the end of
// each child reads the whole process table synchronously to kill what it left (`stopTree` in
// `bounded-subprocess.ts`), which holds this process's event loop meanwhile. On a dev box of
// 19,066 processes one bounded `true` took 6.1 to 8.5 s, and with these rows concurrent 13 of
// their 63 runs were read past the 60 s bound at about 161 s though each had exited, a dash run
// that stops at its first line among them. One at a time, no sibling's read holds the loop while a
// run's bound is counting.
describe(scriptName, () => {
  test.skipIf(withoutTools)(
    "(a) passes when every refusal line is refused, the edge's own 404 with no marker or the answer its line names, after each routed control showed its marker",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("a", { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() });
        expect(linesOf(run, "control"), said(run)).toEqual(visibleControls);
        expect(linesOf(run, "unrouted"), said(run)).toEqual(
          fiveFields.map((line) => unroutedLine(line))
        );
        expect(linesOf(run, "refused"), said(run)).toEqual(refusals.map(refusedLine));
        expect(linesOf(run, "REACHED"), said(run)).toEqual([]);
        expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([]);
        expect(verdict(run), said(run)).toBe(passVerdict);
        expect(run.code, said(run)).toBe(0);
        // The controls first, then each refusal line once, each with its own method, Host and
        // path, and an empty body for a POST, after its unrouted control for a line of five fields.
        expect(await edge.seen()).toEqual([
          ...routed.map((line) => sent(line)),
          ...sentFor(refusals),
        ]);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  for (const line of refusals) {
    test.skipIf(withoutTools)(
      `(b) fails naming ${line.name} and no other line when a route is planted for exactly that line`,
      async () => {
        const edge = await standIn([planted(line), ...production]);
        try {
          const run = await check(`b ${line.name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "control"), said(run)).toEqual(visibleControls);
          expect(linesOf(run, "REACHED"), said(run)).toEqual([
            probeLine("REACHED", line.name, 200, "canary", canaryBody),
          ]);
          expect(linesOf(run, "refused"), said(run)).toEqual(
            refusals.filter((other) => other !== line).map(refusedLine)
          );
          expect(verdict(run), said(run)).toBe(`FAIL: reached ${line.name}`);
          expect(run.code, said(run)).toBe(1);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  const enroll = refusals.find((line) => line.name === "broker-enroll") as Line;
  const pending = refusals.find((line) => line.name === "broker-pending") as Line;

  test.skipIf(withoutTools)(
    "(b) reads a planted route's own 404 that carries a marker as reached: the marker, not the status, says a route took it",
    async () => {
      const edge = await standIn([
        planted(enroll, { status: 404, marker: "broker", body: "broker: not found\n" }),
        ...production,
      ]);
      try {
        const run = await check("b marker 404", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "REACHED"), said(run)).toEqual([
          probeLine("REACHED", "broker-enroll", 404, "broker", "broker: not found\n"),
        ]);
        expect(verdict(run), said(run)).toBe("FAIL: reached broker-enroll");
        expect(run.code, said(run)).toBe(1);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // Any status but 404 is an answer, not a refusal, a redirect included: the script follows none
  // (no `-L`), and an unmarked 3xx is a line reached as an unmarked 200 or 403 is.
  for (const status of [200, 301, 302, 403]) {
    test.skipIf(withoutTools)(
      `(b) reads a planted route's ${status} with no marker as reached: only the edge's own 404 with no marker is a refusal`,
      async () => {
        const edge = await standIn([
          planted(pending, { status, marker: undefined, body: "unmarked\n" }),
          ...production,
        ]);
        try {
          const run = await check(`b unmarked ${status}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "REACHED"), said(run)).toEqual([
            probeLine("REACHED", "broker-pending", status, "none", "unmarked\n"),
          ]);
          expect(verdict(run), said(run)).toBe("FAIL: reached broker-pending");
          expect(run.code, said(run)).toBe(1);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  // The stand-in matches a route on the path as it came on the wire, as an edge's route names it.
  // The script refuses a line whose path holds `%2e%2e`, a dot segment (the input file's rows), so
  // this row asks the stand-in itself, with the host's curl and `--path-as-is`, which sends the
  // path as written on every curl the script takes. A stand-in that read the path decoded, as
  // `Bun.serve`'s request URL has it, would read `/v1/%2e%2e/healthz` as `/healthz`, take no route,
  // and answer 404.
  test.skipIf(withoutTools)(
    "the stand-in routes on the path as it came on the wire, encoded dots and all",
    async () => {
      const path = "/v1/%2e%2e/healthz";
      const host = "secrets.edge.example";
      const edge = await standIn([
        { host, path, status: 200, marker: "canary", body: canaryBody },
        ...production,
      ]);
      try {
        const { code, stdout } = await runBounded({
          argv: [
            curl,
            "-q",
            "--noproxy",
            "*",
            "-g",
            "--path-as-is",
            "-sS",
            "-H",
            `Host: ${host}`,
            "-o",
            "/dev/null",
            "-w",
            "%{http_code}",
            `${edge.address}${path}`,
          ],
          bound: "short",
          env: { HOME: scratch, PATH: process.env.PATH ?? "" },
          from: import.meta.url,
        });
        expect({ code, stdout }).toEqual({ code: 0, stdout: "200" });
        expect(await edge.seen()).toEqual([{ method: "GET", host, path, contentLength: null }]);
      } finally {
        await edge.stop();
      }
    },
    // The host's curl, a bounded child under the short bound.
    rowLimit("short")
  );

  // The dot-segment rule reads only the path before any `?`, and only whole segments: a line whose
  // dots stand in its query, or in a longer segment, `%2e` among them, is taken, and its request
  // goes out with its path as written, for the line and its unrouted control. (The stand-in
  // records the path up to any `?`.)
  const dotted: Line[] = [
    {
      name: "dots-in-query",
      method: "GET",
      sni: "secrets.edge.example",
      host: "secrets.edge.example",
      path: "/v1/pending?next=/../x",
    },
    {
      name: "dots-in-segment",
      method: "GET",
      sni: "secrets.edge.example",
      host: "secrets.edge.example",
      path: "/v1/..x/%2e%2ey",
    },
  ];
  test.skipIf(withoutTools)(
    "takes a line whose dots stand in its query or in a longer segment, and sends its path as written",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("dots taken", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          OUTSIDE_EDGE_ROUTED: listFile(routedTable),
          OUTSIDE_EDGE_REFUSALS: listFile(
            dotted.map((line) => `${line.name} GET ${line.sni} ${line.host} ${line.path}`)
          ),
        });
        expect(linesOf(run, "refused"), said(run)).toEqual(dotted.map(refusedLine));
        expect(verdict(run), said(run)).toBe(
          "PASS: every one of the 2 refusal lines was refused (the edge's own 404 with no marker, or the answer its line names), after each of the 3 routed controls showed its marker"
        );
        expect(run.code, said(run)).toBe(0);
        const recorded = dotted.map((line) => ({ ...line, path: line.path.split("?")[0] ?? "" }));
        expect((await edge.seen()).slice(routed.length)).toEqual(sentFor(recorded));
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // A route that forwards a line without its marker to a backend that answers 404 may give that
  // backend's 404, not the edge's own: then its body differs from the one the line's unrouted
  // control gets for the same path. Such a line reads UNKNOWN, never refused. So does a line whose
  // unrouted control is not answered with the edge's own 404, since then nothing says what that 404
  // is, and one whose 404 and the edge's own are both empty, since a forward's could be too. The
  // verdict names each cause.
  const unlikeVerdict = (names: string) =>
    `UNKNOWN: the 404 with no marker had another body than the edge's own 404 for its path, as a forward could, for ${names}`;
  const uncontrolledVerdict = (names: string) =>
    `UNKNOWN: no unrouted control got the edge's own 404, with no marker and read whole, for ${names}, so nothing says what that 404 is`;
  test.skipIf(withoutTools)(
    "(h) reads UNKNOWN, never refused, when a route forwards a line without a marker to a backend that answers 404",
    async () => {
      const backend = "backend: no such page\n";
      const edge = await standIn([
        planted(pending, { status: 404, marker: undefined, body: backend }),
        ...production,
      ]);
      try {
        const run = await check("h unmarked forward", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "unrouted"), said(run)).toContain(unroutedLine(pending));
        expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([
          `${probeLine("UNKNOWN", pending.name, 404, "none", backend)}; another body than its unrouted control's 404`,
        ]);
        expect(linesOf(run, "refused").length, said(run)).toBe(15);
        expect(verdict(run), said(run)).toBe(unlikeVerdict(pending.name));
        expect(run.code, said(run)).toBe(2);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // Each cause has its own note on the line and its own words in the verdict: a control answered
  // by a route is no 404 of the edge's own, and a control's 404 with another body says the line's
  // body is not that 404.
  const controlTaken: {
    name: string;
    answer: Partial<Route>;
    status: number;
    marker: string;
    note: string;
    says: (names: string) => string;
  }[] = [
    {
      name: "a route's 200, marked",
      answer: {},
      status: 200,
      marker: "canary",
      note: "its unrouted control did not answer 404 with no marker, read whole",
      says: uncontrolledVerdict,
    },
    {
      name: "a 404 with no marker and another body",
      answer: { status: 404, marker: undefined, body: "another 404\n" },
      status: 404,
      marker: "none",
      note: "another body than its unrouted control's 404",
      says: unlikeVerdict,
    },
  ];
  for (const { name, answer, status, marker, note, says } of controlTaken) {
    test.skipIf(withoutTools)(
      `(h) reads UNKNOWN, never refused, when a line's unrouted control is answered ${name}, naming that cause`,
      async () => {
        const body = answer.body ?? canaryBody;
        const edge = await standIn([
          planted({ ...pending, host: unroutedHost }, answer),
          ...production,
        ]);
        try {
          const run = await check(`h control ${status}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "unrouted"), said(run)).toContain(
            probeLine("unrouted", pending.name, status, marker, body)
          );
          expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([
            `${probeLine("UNKNOWN", pending.name, 404, "none", notFound)}; ${note}`,
          ]);
          expect(verdict(run), said(run)).toBe(says(pending.name));
          expect(run.code, said(run)).toBe(2);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  // An edge whose own 404 has an empty body gives the same digest as a forward to a backend whose
  // 404 is empty too, so its lines of five fields read UNKNOWN, never refused, each named in the
  // verdict; a line of six fields is read as ever.
  test.skipIf(withoutTools)(
    "(h) reads UNKNOWN, never refused, when the edge's own 404 and a line's are both empty, naming each line",
    async () => {
      const edge = await standIn(production, "");
      try {
        const run = await check("h empty 404", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "unrouted"), said(run)).toEqual(
          fiveFields.map((line) => unroutedLine(line, ""))
        );
        expect(linesOf(run, "UNKNOWN"), said(run)).toEqual(
          fiveFields.map(
            (line) =>
              `${probeLine("UNKNOWN", line.name, 404, "none", "")}; its unrouted control's 404 had an empty body, as a forward's could`
          )
        );
        expect(linesOf(run, "refused"), said(run)).toEqual(
          refusals.filter((line) => line.expect !== undefined).map(refusedLine)
        );
        expect(verdict(run), said(run)).toBe(
          `UNKNOWN: the edge's own 404 for its path had an empty body, which a forward could give too, for ${fiveFields.map((line) => line.name).join(" ")}`
        );
        expect(run.code, said(run)).toBe(2);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // A body's digest counts only when the hasher exits 0 printing exactly one SHA-256. A hash that
  // failed is no digest: two failed hashes compared equal, so a line whose 404 body differed from
  // the edge's own read refused and the run passed. Each row runs the script on a PATH of its own:
  // links to the host's curl, mktemp, rm and date, and a hasher script, `sha256sum`, or `shasum`
  // alone, which the script takes where no sha256sum is on PATH. In the fault rows a route
  // forwards broker-pending without a marker to a backend whose 404 body is not the edge's own:
  // the false PASS the faults made. A working hasher, the control, reads PASS on equal bodies.
  const hostSha256sum = hostTool("sha256sum");
  /** A PATH directory of the row's own: the host's curl, mktemp, rm and date, and `tool`, a bash
   *  script of `body`. */
  function hasherPath(tool: string, body: string): string {
    const directory = directoryOf("path");
    for (const name of ["curl", "mktemp", "rm", "date"]) {
      const found = hostTool(name);
      if (found !== null) symlinkSync(found, join(directory, name));
    }
    writeFileSync(join(directory, tool), `#!${bash}\n${body}\n`, { mode: 0o755 });
    return directory;
  }
  const hasherFaults = [
    { name: "a sha256sum that exits 1 with no output", tool: "sha256sum", body: "exit 1" },
    {
      name: "a sha256sum that exits 0 printing no SHA-256",
      tool: "sha256sum",
      body: `printf '%s  -\\n' ${"e".repeat(63)}; exit 0`,
    },
    {
      // Not the SHA-256 of an empty body, which the empty-404 rule would read apart.
      name: "a sha256sum that prints a SHA-256 and exits 1",
      tool: "sha256sum",
      body: `printf '%s  -\\n' ${sha("hasher")}; exit 1`,
    },
    { name: "a shasum, the only hasher, that exits 1", tool: "shasum", body: "exit 1" },
  ];
  const unhashedVerdict = `UNKNOWN: the hasher gave no SHA-256 of the 404 body or of the edge's own 404 for its path, so the two could not be compared, for ${fiveFields.map((line) => line.name).join(" ")}`;
  for (const { name, tool, body } of hasherFaults) {
    test.skipIf(withoutTools)(
      `(i) reads UNKNOWN, never refused, with ${name}, naming each line of five fields`,
      async () => {
        const edge = await standIn([
          planted(pending, { status: 404, marker: undefined, body: "backend: no such page\n" }),
          ...production,
        ]);
        try {
          const run = await check(
            `i ${name}`,
            { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
            { path: hasherPath(tool, body) }
          );
          expect(linesOf(run, "UNKNOWN"), said(run)).toEqual(
            fiveFields.map(
              (line) =>
                `UNKNOWN ${line.name} 404 none body-sha256 unreadable; the hasher gave no SHA-256 of its body or its unrouted control's`
            )
          );
          expect(linesOf(run, "refused"), said(run)).toEqual(
            refusals
              .filter((line) => line.expect !== undefined)
              .map((line) => `refused ${line.name} 405 broker body-sha256 unreadable`)
          );
          expect(verdict(run), said(run)).toBe(unhashedVerdict);
          expect(run.code, said(run)).toBe(2);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  // A reached line keeps the run at FAIL whatever the hasher did.
  test.skipIf(withoutTools)(
    "(i) fails naming a line reached when the hasher exits 1 with no output",
    async () => {
      const [first] = refusals as [Line];
      const edge = await standIn([planted(first), ...production]);
      try {
        const run = await check(
          "i reached, hasher failing",
          { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
          { path: hasherPath("sha256sum", "exit 1") }
        );
        expect(linesOf(run, "REACHED"), said(run)).toEqual([
          `REACHED ${first.name} 200 canary body-sha256 unreadable`,
        ]);
        expect(verdict(run), said(run)).toBe(`FAIL: reached ${first.name}`);
        expect(run.code, said(run)).toBe(1);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // The control: the same PATH of the row's own, with a hasher that works, reads PASS where the
  // bodies are equal, through sha256sum and through shasum alone.
  const workingHashers = [
    { name: "sha256sum", tool: "sha256sum", body: `exec ${hostSha256sum} "$@"` },
    {
      name: "shasum, the only hasher",
      tool: "shasum",
      body: `[ "$1 $2" = "-a 256" ] && shift 2; exec ${hostSha256sum} "$@"`,
    },
  ];
  for (const { name, tool, body } of workingHashers) {
    test.skipIf(withoutTools || hostSha256sum === null)(
      `(i) passes with a working ${name} where each line's 404 is the edge's own`,
      async () => {
        const edge = await standIn(production);
        try {
          const run = await check(
            `i working ${name}`,
            { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
            { path: hasherPath(tool, body) }
          );
          expect(linesOf(run, "refused"), said(run)).toEqual(refusals.map(refusedLine));
          expect(verdict(run), said(run)).toBe(passVerdict);
          expect(run.code, said(run)).toBe(0);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  // A refusal line whose sixth field names its answer, `broker-token-get`'s `405:broker`: no route
  // of the edge matches on method, so a GET on the broker's token path reaches the broker, and its
  // own 405, marked, is the refusal. Exactly that status with exactly that marker is refused, and
  // any other answer is a line reached.
  const tokenGet = refusals.find((line) => line.name === "broker-token-get") as Line;

  test.skipIf(withoutTools)(
    "(f) refuses a line whose sixth field names its answer on exactly that answer: broker-token-get's 405 marked broker",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("f expected", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "refused"), said(run)).toContain(
          probeLine("refused", "broker-token-get", 405, "broker", methodRefused)
        );
        expect(verdict(run), said(run)).toBe(passVerdict);
        expect(run.code, said(run)).toBe(0);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  const otherAnswers: {
    name: string;
    routes: Route[];
    status: number;
    marker: string;
    body: string;
  }[] = [
    {
      // The broker's 405 route gone: the Gateway's own 404, with no marker.
      name: "a 404 with no marker",
      routes: production.filter((route) => !(route.path === tokenGet.path && route.status === 405)),
      status: 404,
      marker: "none",
      body: notFound,
    },
    {
      name: "a 405 marked canary",
      routes: [planted(tokenGet, { status: 405, body: methodRefused }), ...production],
      status: 405,
      marker: "canary",
      body: methodRefused,
    },
    {
      name: "a 405 with no marker",
      routes: [
        planted(tokenGet, { status: 405, marker: undefined, body: methodRefused }),
        ...production,
      ],
      status: 405,
      marker: "none",
      body: methodRefused,
    },
    {
      name: "a 200 marked broker",
      routes: [planted(tokenGet, { marker: "broker", body: "token\n" }), ...production],
      status: 200,
      marker: "broker",
      body: "token\n",
    },
  ];
  for (const { name, routes, status, marker, body } of otherAnswers) {
    test.skipIf(withoutTools)(
      `(f) fails naming broker-token-get and no other line when it is answered ${name}, not its 405:broker`,
      async () => {
        const edge = await standIn(routes);
        try {
          const run = await check(`f ${name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "REACHED"), said(run)).toEqual([
            probeLine("REACHED", "broker-token-get", status, marker, body),
          ]);
          expect(linesOf(run, "refused"), said(run)).toEqual(
            refusals.filter((other) => other !== tokenGet).map(refusedLine)
          );
          expect(verdict(run), said(run)).toBe("FAIL: reached broker-token-get");
          expect(run.code, said(run)).toBe(1);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  test.skipIf(withoutTools)(
    "(f) reads UNKNOWN, not refused, when the named answer came that curl did not read whole",
    async () => {
      const { path } = httpsCurl({
        ...httpsControls,
        "secrets.edge.example /v1/oauth/token": {
          status: "405",
          header: "X-Outside-Edge-Backend: broker",
          exit: 28,
          message: "curl: (28) Operation timed out",
        },
      });
      const run = await check(
        "f cut short",
        { OUTSIDE_EDGE_ADDRESS: "192.0.2.10", OUTSIDE_EDGE_ALLOW_HTTP: undefined, ...planLists() },
        { path }
      );
      expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([
        `UNKNOWN broker-token-get 405 broker body-sha256 ${sha("answer 405\n")}; curl exit 28: curl: (28) Operation timed out`,
      ]);
      expect(linesOf(run, "refused").length, said(run)).toBe(15);
      expect(verdict(run), said(run)).toBe(
        "UNKNOWN: no answer could be read whole for broker-token-get"
      );
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // curl's `-D` writes every header block of an answer, an informational `1xx` before the final
  // one, and the status it prints is the final answer's. A marker is read from every block: a line
  // whose sixth field names its answer is refused only when the final answer carries a marker and
  // every marker of every block is the named one, so neither an earlier block nor a second marker
  // in the final one can stand for the answer; a routed control is read the same way; and for a
  // line of five fields a marker in any block is a route reached, as before.
  const blocks = (...each: [number, ...string[]][]) =>
    each
      .map(([status, ...markers]) =>
        [
          `HTTP/1.1 ${status} Stand-in`,
          ...markers.map((value) => `X-Outside-Edge-Backend: ${value}`),
          "",
          "",
        ].join("\\r\\n")
      )
      .join("");
  const finalAnswers: {
    name: string;
    key: string;
    status: string;
    blocks: string;
    line: string;
    verdict: string;
    code: number;
  }[] = [
    {
      name: "a 103 marked broker before a final 405 marked canary, on the line that names 405:broker",
      key: "secrets.edge.example /v1/oauth/token",
      status: "405",
      blocks: blocks([103, "broker"], [405, "canary"]),
      line: `REACHED broker-token-get 405 broker,canary body-sha256 ${sha("answer 405\n")}`,
      verdict: "FAIL: reached broker-token-get",
      code: 1,
    },
    {
      name: "one final 405 carrying two markers, broker then canary, on the line that names 405:broker",
      key: "secrets.edge.example /v1/oauth/token",
      status: "405",
      blocks: blocks([405, "broker", "canary"]),
      line: `REACHED broker-token-get 405 broker,canary body-sha256 ${sha("answer 405\n")}`,
      verdict: "FAIL: reached broker-token-get",
      code: 1,
    },
    {
      name: "a 103 marked broker before a final 405 with no marker, on the line that names 405:broker",
      key: "secrets.edge.example /v1/oauth/token",
      status: "405",
      blocks: blocks([103, "broker"], [405]),
      line: `REACHED broker-token-get 405 broker body-sha256 ${sha("answer 405\n")}`,
      verdict: "FAIL: reached broker-token-get",
      code: 1,
    },
    {
      name: "a 103 marked canary before a final 404 with no marker, on a line of five fields",
      key: "envoy.edge.example /metrics",
      status: "404",
      blocks: blocks([103, "canary"], [404]),
      line: `REACHED listener-metrics 404 canary body-sha256 ${sha("answer 404\n")}`,
      verdict: "FAIL: reached listener-metrics",
      code: 1,
    },
    {
      name: "a routed control whose final answer carries its marker and a second one",
      key: "dispatch.edge.example /healthz",
      status: "200",
      blocks: blocks([200, "dispatch", "canary"]),
      line: `control dispatch-healthz 200 dispatch,canary body-sha256 ${sha("answer 200\n")}`,
      verdict: markerNotVisible("dispatch-healthz"),
      code: 2,
    },
    {
      name: "a routed control whose marker came only on a 103 before its final answer",
      key: "dispatch.edge.example /healthz",
      status: "200",
      blocks: blocks([103, "dispatch"], [200]),
      line: `control dispatch-healthz 200 dispatch body-sha256 ${sha("answer 200\n")}`,
      verdict: markerNotVisible("dispatch-healthz"),
      code: 2,
    },
  ];
  for (const { name, key, status, blocks: written, line, verdict: says, code } of finalAnswers) {
    test.skipIf(withoutTools)(
      `(g) reads the marker of every header block, never the first alone: ${name}`,
      async () => {
        const { path } = httpsCurl({ ...httpsControls, [key]: { status, blocks: written } });
        const run = await check(
          `g ${name}`,
          {
            OUTSIDE_EDGE_ADDRESS: "192.0.2.10",
            OUTSIDE_EDGE_ALLOW_HTTP: undefined,
            ...planLists(),
          },
          { path }
        );
        expect(run.stdout.split("\n"), said(run)).toContain(line);
        expect(verdict(run), said(run)).toBe(says);
        expect(run.code, said(run)).toBe(code);
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  // Each probe's body is bounded (`curl --max-filesize`), so an answer cannot fill the disk: a 404
  // with no marker whose body is past the bound was not read whole, so it proves nothing.
  test.skipIf(withoutTools)(
    "reads UNKNOWN, never refused, when a 404 with no marker carries a body past the 1 MiB bound",
    async () => {
      const pending = refusals.find((line) => line.name === "broker-pending") as Line;
      const edge = await standIn([
        planted(pending, { status: 404, marker: undefined, body: "x".repeat(2 * 2 ** 20) }),
        ...production,
      ]);
      try {
        const run = await check("oversized body", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "UNKNOWN"), said(run)).toHaveLength(1);
        expect(linesOf(run, "UNKNOWN")[0], said(run)).toMatch(
          /^UNKNOWN broker-pending 404 none body-sha256 [0-9a-f]{64}; curl exit 63: /
        );
        expect(verdict(run), said(run)).toBe(
          "UNKNOWN: no answer could be read whole for broker-pending"
        );
        expect(run.code, said(run)).toBe(2);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  for (const control of routed) {
    test.skipIf(withoutTools)(
      `(c) reads UNKNOWN, and probes no refusal line, when the marker of ${control.name} is removed`,
      async () => {
        const routes = production.map((route) =>
          route.marker === control.backend ? { ...route, marker: undefined } : route
        );
        const edge = await standIn(routes);
        try {
          const run = await check(`c ${control.name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "control"), said(run)).toEqual(
            visibleControls.map((shown, index) =>
              routed[index] === control ? shown.replace(` ${control.backend} `, " none ") : shown
            )
          );
          expect(verdict(run), said(run)).toBe(markerNotVisible(control.name));
          expect(run.code, said(run)).toBe(2);
          // The controls alone: no refusal line is read once a marker is not visible.
          expect(await edge.seen()).toEqual(routed.map((line) => sent(line)));
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  test.skipIf(withoutTools)(
    "(c) reads UNKNOWN when a routed control answers with another backend's marker",
    async () => {
      const routes = production.map((route) =>
        route.marker === "dispatch" ? { ...route, marker: "broker" } : route
      );
      const edge = await standIn(routes);
      try {
        const run = await check("c wrong marker", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "control")[0], said(run)).toBe(
          probeLine("control", "dispatch-healthz", 200, "broker", "dispatch healthz\n")
        );
        expect(verdict(run), said(run)).toBe(markerNotVisible("dispatch-healthz"));
        expect(run.code, said(run)).toBe(2);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  const emptyLists: { name: string; lines: string[] }[] = [
    { name: "an empty refusal file", lines: [] },
    { name: "a refusal file of comments and blank lines alone", lines: ["# none yet", "", "  "] },
  ];
  for (const { name, lines } of emptyLists) {
    test.skipIf(withoutTools)(
      `(d) reads UNKNOWN for ${name}, and probes nothing`,
      async () => {
        const edge = await standIn(production);
        try {
          const empty = listFile(lines);
          const run = await check(
            `d ${name}`,
            {
              OUTSIDE_EDGE_ADDRESS: edge.address,
              OUTSIDE_EDGE_ROUTED: listFile(routedTable),
              OUTSIDE_EDGE_REFUSALS: empty,
            },
            { bound: "short" }
          );
          expect(run.stdout, said(run)).toBe(
            `UNKNOWN: ${empty} lists no refusal line: nothing was tried, so nothing is proved\n`
          );
          expect(run.code, said(run)).toBe(2);
          expect(await edge.seen()).toEqual([]);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound: it stops before any probe.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "(d) reads UNKNOWN for an empty routed file: no control, so no refusal can be read",
    async () => {
      const empty = listFile([]);
      const run = await check(
        "d routed",
        {
          OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
          OUTSIDE_EDGE_ROUTED: empty,
          OUTSIDE_EDGE_REFUSALS: listFile(refusalTable),
        },
        { bound: "short" }
      );
      expect(run.stdout, said(run)).toBe(
        `UNKNOWN: ${empty} lists no routed control: no marker can be seen, so no refusal can be read\n`
      );
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound: it stops before any probe.
    rowLimit("short")
  );

  test.skipIf(withoutTools)(
    "(e) reads UNKNOWN when nothing listens at the edge address, never PASS",
    async () => {
      const run = await check("e", {
        OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
        ...planLists(),
      });
      const controls = linesOf(run, "control");
      expect(controls.length, said(run)).toBe(3);
      for (const [index, shown] of controls.entries()) {
        expect(shown, said(run)).toStartWith(
          `control ${routed[index]?.name} 000 none body-sha256 none; curl exit 7: `
        );
      }
      expect(linesOf(run, "refused"), said(run)).toEqual([]);
      expect(verdict(run), said(run)).toBe(markerNotVisible("dispatch-healthz"));
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  test.skipIf(withoutTools)(
    "(e) reads UNKNOWN at an edge address that answers nothing, under the script's own bounds",
    async () => {
      // TEST-NET-1 (RFC 5737), reserved for documentation: a network drops the connect or
      // refuses it, and either way no marker is seen. The default bounds are 3 s to connect, so
      // the three controls take at most about 9 s.
      const run = await check("e blackhole", {
        OUTSIDE_EDGE_ADDRESS: "192.0.2.1",
        OUTSIDE_EDGE_ALLOW_HTTP: undefined,
        OUTSIDE_EDGE_CONNECT_SECONDS: undefined,
        OUTSIDE_EDGE_READ_SECONDS: undefined,
        ...planLists(),
      });
      expect(linesOf(run, "control").length, said(run)).toBe(3);
      for (const shown of linesOf(run, "control")) {
        expect(shown, said(run)).toMatch(/^control \S+ 000 none body-sha256 none; curl exit \d+: /);
      }
      expect(verdict(run), said(run)).toBe(markerNotVisible("dispatch-healthz"));
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  test.skipIf(withoutTools)(
    "probes https://<sni><path> through --resolve <sni>:443:<address>, with each line's Host and method and an empty body for a POST, each line of five fields after its unrouted control, and leaves no scratch behind",
    async () => {
      const { path, log } = httpsCurl(httpsControls);
      const tmp = join(scratch, `tmp-${randomUUID()}`);
      mkdirSync(tmp);
      const run = await check(
        "https",
        {
          OUTSIDE_EDGE_ADDRESS: "192.0.2.10",
          OUTSIDE_EDGE_ALLOW_HTTP: undefined,
          OUTSIDE_EDGE_CONNECT_SECONDS: undefined,
          OUTSIDE_EDGE_READ_SECONDS: undefined,
          ...planLists(),
        },
        { path, tmp }
      );
      expect(linesOf(run, "control"), said(run)).toEqual([
        probeLine("control", "dispatch-healthz", 200, "dispatch", "answer 200\n"),
        probeLine("control", "broker-device", 400, "broker", "answer 400\n"),
        probeLine("control", "listener-v1", 404, "listener", "answer 404\n"),
      ]);
      expect(verdict(run), said(run)).toBe(passVerdict);
      expect(run.code, said(run)).toBe(0);
      const probes = probesIn(log);
      expect(probes.map((probe) => probe.argv)).toEqual([
        ...routed.map((line) => httpsArgv(line, "192.0.2.10")),
        ...refusals.flatMap((line) =>
          line.expect === undefined
            ? [httpsArgv(line, "192.0.2.10", { host: unroutedHost }), httpsArgv(line, "192.0.2.10")]
            : [httpsArgv(line, "192.0.2.10")]
        ),
      ]);
      // Each probe's headers and body go to files of their own under TMPDIR, which the run
      // removes before it exits.
      const files = probes.flatMap((probe) => probe.files);
      expect(new Set(files).size).toBe(files.length);
      for (const file of files) expect(file.startsWith(`${tmp}/`)).toBe(true);
      expect(readdirSync(tmp)).toEqual([]);
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  const unread: { name: string; answer: { status: string; exit: number; message: string } }[] = [
    {
      name: "a TLS failure",
      answer: { status: "000", exit: 60, message: "curl: (60) SSL certificate problem" },
    },
    {
      name: "a connect that failed",
      answer: { status: "000", exit: 7, message: "curl: (7) Failed to connect" },
    },
    {
      name: "a 404 with no marker whose answer was cut short",
      answer: { status: "404", exit: 28, message: "curl: (28) Operation timed out" },
    },
  ];
  for (const { name, answer } of unread) {
    test.skipIf(withoutTools)(
      `reads UNKNOWN naming the refusal line whose probe met ${name}, when no line was reached`,
      async () => {
        const { path } = httpsCurl({
          ...httpsControls,
          "secrets.tailnet.example /v1/oauth/token": answer,
        });
        const run = await check(
          `unread ${answer.exit}`,
          {
            OUTSIDE_EDGE_ADDRESS: "192.0.2.10",
            OUTSIDE_EDGE_ALLOW_HTTP: undefined,
            ...planLists(),
          },
          { path }
        );
        const body = answer.status === "000" ? "none" : sha(`answer ${answer.status}\n`);
        expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([
          `UNKNOWN tailnet-secrets ${answer.status} none body-sha256 ${body}; curl exit ${answer.exit}: ${answer.message}`,
        ]);
        expect(linesOf(run, "refused").length, said(run)).toBe(15);
        expect(verdict(run), said(run)).toBe(
          "UNKNOWN: no answer could be read whole for tailnet-secrets"
        );
        expect(run.code, said(run)).toBe(2);
      },
      // The script, a bounded child under the bound of a run that probes.
      rowLimit("edgeRun")
    );
  }

  test.skipIf(withoutTools)(
    "fails on a reached line whatever else the run could not read, and reads a marked answer cut short as reached",
    async () => {
      const { path } = httpsCurl({
        ...httpsControls,
        "secrets.tailnet.example /v1/oauth/token": {
          status: "000",
          exit: 7,
          message: "curl: (7) Failed to connect",
        },
        "envoy.edge.example /metrics": {
          status: "200",
          header: "X-Outside-Edge-Backend: listener",
          exit: 28,
          message: "curl: (28) Operation timed out",
        },
      });
      const run = await check(
        "reached and unread",
        { OUTSIDE_EDGE_ADDRESS: "192.0.2.10", OUTSIDE_EDGE_ALLOW_HTTP: undefined, ...planLists() },
        { path }
      );
      expect(linesOf(run, "REACHED"), said(run)).toEqual([
        `REACHED listener-metrics 200 listener body-sha256 ${sha("answer 200\n")}; curl exit 28: curl: (28) Operation timed out`,
      ]);
      expect(linesOf(run, "UNKNOWN").length, said(run)).toBe(1);
      expect(verdict(run), said(run)).toBe("FAIL: reached listener-metrics");
      expect(run.code, said(run)).toBe(1);
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  test.skipIf(withoutTools)(
    "reads lists with comments, blank lines, tabs and CRLF line endings",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("crlf", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          OUTSIDE_EDGE_ROUTED: listFile(["# the routed controls", "", ...routedTable], "\r\n"),
          OUTSIDE_EDGE_REFUSALS: listFile([
            "  # name method sni host path",
            ...refusalTable.map((line) => line.replaceAll(/ +/g, "\t")),
            "",
          ]),
        });
        expect(verdict(run), said(run)).toBe(passVerdict);
        expect(run.code, said(run)).toBe(0);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // A run's output goes to logs anyone may read, such as a public CI log, so it names this
  // machine's host name nowhere: neither a HOSTNAME the environment hands bash, which bash keeps,
  // nor the one bash reads from the system where the environment names none.
  test.skipIf(withoutTools)(
    "prints the machine's host name in no line, whether the environment names it or bash reads it",
    async () => {
      const edge = await standIn(production);
      try {
        const planted = `planted-host-${randomUUID()}`;
        for (const { label, name, env } of [
          { label: "planted", name: planted, env: { HOSTNAME: planted } },
          { label: "read", name: hostname(), env: {} },
        ]) {
          const run = await check(`host name ${label}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
            ...env,
          });
          expect(verdict(run), said(run)).toBe(passVerdict);
          expect(run.stdout.split("\n").filter((line) => line.includes(name))).toEqual([]);
        }
      } finally {
        await edge.stop();
      }
    },
    // The script twice, one after the other, each a bounded child under the bound of a run that
    // probes.
    rowLimit("edgeRun", "edgeRun")
  );
});
