/** What `e2e/acceptance/outside-edge-limits.ts` decides: step 4 of the outside driver
 *  (`e2e/acceptance/outside-edge.ts`), which reads the edge's rate rules from a file, gives each
 *  request the driver made to the first rule that matches it, and fails a rule whose peak count in
 *  any window of the rule's length is above half of its threshold. The hosts are example hosts of
 *  the shape the edge's three names have.
 */

import { describe, expect, test } from "bun:test";
import {
  judgeLimits,
  LimitsError,
  parseLimits,
  type Row,
  verdictOf,
} from "../acceptance/outside-edge-limits";

const dispatch = "dispatch.edge.example";
const secrets = "secrets.edge.example";
const envoy = "envoy.edge.example";

/** A limits file of the edge's shape: two thresholds per rule, a minute's and ten minutes'. */
const edgeLimits = [
  "# priority host match path count seconds",
  `1000 ${secrets}  exact          /v1/oauth/device_authorization 20   60`,
  `1000 ${secrets}  exact          /v1/oauth/device_authorization 100  600`,
  `1010 ${secrets}  exact          /v1/oauth/token                240  60`,
  `1010 ${secrets}  exact          /v1/oauth/token                2400 600`,
  `2000 ${envoy}    prefix         /v1/                           600  60`,
  `2000 ${envoy}    prefix         /v1/                           6000 600`,
  `3000 ${dispatch} decoded-prefix /auth/                         60   60`,
  `3000 ${dispatch} decoded-prefix /auth/                         600  600`,
  `3010 ${dispatch} any            -                              1200 60`,
  `3010 ${dispatch} any            -                              12000 600`,
].join("\n");

/** `count` requests to `url`, `everyMs` apart from `startMs`. */
function requests(url: string, count: number, startMs = 0, everyMs = 1) {
  return Array.from({ length: count }, (_, i) => ({ at: startMs + i * everyMs, url }));
}

function rowNamed(rows: readonly Row[], name: string): Row {
  const row = rows.find((one) => one.row === name);
  if (row === undefined) throw new Error(`no row ${name} in ${JSON.stringify(rows)}`);
  return row;
}

describe("outside-edge-limits.ts", () => {
  test("gives each request to the first rule that matches it and passes every rule at or under half", () => {
    const limits = parseLimits(edgeLimits);
    const judged = judgeLimits(limits, [
      ...requests(`https://${secrets}/v1/oauth/device_authorization`, 10),
      // A sign-in route by its decoded, lower-cased path, as the rule reads it: rule 3000 and
      // not 3010.
      ...requests(`https://${dispatch}/%61uth/passkey/login/begin`, 30),
      ...requests(`https://${dispatch}/api/v1/projects?x=1`, 5),
      ...requests(`https://${envoy}/v1/messages/send`, 2),
    ]);
    expect(rowNamed(judged.rows, "limit-1000-60s")).toEqual({
      row: "limit-1000-60s",
      want: `at most 10 requests in 60 s to ${secrets} exact /v1/oauth/device_authorization (half of 20)`,
      got: `peak 10 in 60 s from ${secrets}`,
      verdict: "PASS",
    });
    expect(rowNamed(judged.rows, "limit-3000-60s").got).toBe(`peak 30 in 60 s from ${dispatch}`);
    expect(rowNamed(judged.rows, "limit-3010-60s").got).toBe(`peak 5 in 60 s from ${dispatch}`);
    expect(rowNamed(judged.rows, "limit-2000-600s").got).toBe(`peak 2 in 600 s from ${envoy}`);
    expect(rowNamed(judged.rows, "limit-1010-60s").got).toBe(`peak 0 in 60 s from ${secrets}`);
    expect(judged.rows).toHaveLength(10);
    expect(verdictOf(judged.rows)).toBe("PASS");
    expect(judged.hostPeaks).toEqual([
      { host: dispatch, perMinute: 35 },
      { host: envoy, perMinute: 2 },
      { host: secrets, perMinute: 10 },
    ]);
  });

  test("a threshold set to 1 fails its rule, naming the host", () => {
    const limits = parseLimits(
      edgeLimits.replace(`/v1/                           600  60`, `/v1/ 1 60`)
    );
    const judged = judgeLimits(limits, requests(`https://${envoy}/v1/subscribe`, 1));
    expect(rowNamed(judged.rows, "limit-2000-60s")).toEqual({
      row: "limit-2000-60s",
      want: `at most 0.5 requests in 60 s to ${envoy} prefix /v1/ (half of 1)`,
      got: `peak 1 in 60 s from ${envoy}`,
      verdict: "FAIL",
    });
    expect(verdictOf(judged.rows)).toBe("FAIL");
  });

  test("one over half fails and exactly half passes", () => {
    const limits = parseLimits(edgeLimits);
    const half = judgeLimits(
      limits,
      requests(`https://${secrets}/v1/oauth/device_authorization`, 10)
    );
    expect(rowNamed(half.rows, "limit-1000-60s").verdict).toBe("PASS");
    const over = judgeLimits(
      limits,
      requests(`https://${secrets}/v1/oauth/device_authorization`, 11)
    );
    expect(rowNamed(over.rows, "limit-1000-60s").verdict).toBe("FAIL");
    expect(rowNamed(over.rows, "limit-1000-600s").verdict).toBe("PASS");
  });

  test("a peak is the most requests in any window of the rule's length, not in a clock minute", () => {
    const limits = parseLimits(`3010 ${dispatch} any - 4 60`);
    // At 50 s, 59 s, 61 s and 109 s: three fall inside one 60 s window (50 to 110 s) though no
    // clock minute holds more than two, and the fourth is 59 s after the first of them.
    const judged = judgeLimits(
      limits,
      [50_000, 59_000, 61_000, 109_000].map((at) => ({ at, url: `https://${dispatch}/` }))
    );
    expect(rowNamed(judged.rows, "limit-3010-60s").got).toBe(`peak 4 in 60 s from ${dispatch}`);
    expect(rowNamed(judged.rows, "limit-3010-60s").verdict).toBe("FAIL");
    const spread = judgeLimits(
      limits,
      [0, 60_000, 120_000].map((at) => ({ at, url: `https://${dispatch}/` }))
    );
    expect(rowNamed(spread.rows, "limit-3010-60s").got).toBe(`peak 1 in 60 s from ${dispatch}`);
  });

  test("a host is read without its case or port, and a request no rule matches counts in its host's peak alone", () => {
    const limits = parseLimits(`3010 ${dispatch} any - 10 60`);
    const judged = judgeLimits(limits, [
      { at: 0, url: `https://DISPATCH.EDGE.EXAMPLE:443/` },
      { at: 1, url: "http://127.0.0.1:9/elsewhere" },
    ]);
    expect(rowNamed(judged.rows, "limit-3010-60s").got).toBe(`peak 1 in 60 s from ${dispatch}`);
    expect(judged.hostPeaks).toEqual([
      { host: "127.0.0.1", perMinute: 1 },
      { host: dispatch, perMinute: 1 },
    ]);
  });

  test("no request recorded is UNKNOWN, never a pass", () => {
    const judged = judgeLimits(parseLimits(edgeLimits), []);
    expect(judged.rows).toEqual([
      {
        row: "limits",
        want: "the driver's own requests, counted",
        got: "no request was recorded, so no peak can be read",
        verdict: "UNKNOWN",
      },
    ]);
    expect(verdictOf(judged.rows)).toBe("UNKNOWN");
  });

  test("a rule whose host the run asked nothing is UNKNOWN, never a pass at a peak of 0", () => {
    // A limits file that names another host than the run's: every rule of that host would pass
    // at a peak of 0, which says nothing of it.
    const limits = parseLimits(`${edgeLimits}\n4000 other.edge.example any - 10 60`);
    const judged = judgeLimits(limits, requests(`https://${dispatch}/`, 3));
    expect(rowNamed(judged.rows, "limit-4000-60s")).toEqual({
      row: "limit-4000-60s",
      want: "at most 5 requests in 60 s to other.edge.example any (half of 10)",
      got: "the run asked other.edge.example nothing, so no peak of it can be read",
      verdict: "UNKNOWN",
    });
    expect(rowNamed(judged.rows, "limit-1000-60s").verdict).toBe("UNKNOWN");
    // A host the run asked, whose rule no request matched, still reads its peak of 0.
    expect(rowNamed(judged.rows, "limit-3000-60s")).toMatchObject({
      got: `peak 0 in 60 s from ${dispatch}`,
      verdict: "PASS",
    });
    expect(verdictOf(judged.rows)).toBe("UNKNOWN");
  });

  test("a run cut short passes no rule: one it stayed under is UNKNOWN, one it went over fails", () => {
    const limits = parseLimits(`3010 ${dispatch} any - 4 60`);
    const cut = "the run did not reach relay: the sign-in did not pass";
    const under = judgeLimits(limits, requests(`https://${dispatch}/`, 2), cut);
    expect(rowNamed(under.rows, "limit-3010-60s")).toEqual({
      row: "limit-3010-60s",
      want: `at most 2 requests in 60 s to ${dispatch} any (half of 4)`,
      got: `peak 2 in 60 s from ${dispatch}, but ${cut}, so no whole run was counted`,
      verdict: "UNKNOWN",
    });
    expect(verdictOf(under.rows)).toBe("UNKNOWN");
    const over = judgeLimits(limits, requests(`https://${dispatch}/`, 3), cut);
    expect(rowNamed(over.rows, "limit-3010-60s")).toMatchObject({
      got: `peak 3 in 60 s from ${dispatch}`,
      verdict: "FAIL",
    });
    // The control: the same count from a whole run passes.
    expect(
      rowNamed(judgeLimits(limits, requests(`https://${dispatch}/`, 2)).rows, "limit-3010-60s")
        .verdict
    ).toBe("PASS");
  });

  test("no row is UNKNOWN, and a FAIL outweighs an UNKNOWN", () => {
    expect(verdictOf([])).toBe("UNKNOWN");
    const row = (verdict: Row["verdict"]): Row => ({ row: "r", want: "w", got: "g", verdict });
    expect(verdictOf([row("PASS"), row("UNKNOWN")])).toBe("UNKNOWN");
    expect(verdictOf([row("UNKNOWN"), row("FAIL"), row("PASS")])).toBe("FAIL");
  });

  for (const [why, text, message] of [
    ["an empty file", "# only a comment\n\n", "the limits file holds no rule"],
    [
      "a line of five fields",
      `3010 ${dispatch} any - 10`,
      "line 1: want six fields, priority host match path count seconds",
    ],
    [
      "an unknown match",
      `3010 ${dispatch} regex .* 10 60`,
      'line 1: the match "regex" is none of exact, prefix, decoded-prefix, any',
    ],
    [
      "a path for any",
      `3010 ${dispatch} any /x 10 60`,
      'line 1: a rule of match any takes the path "-", not "/x"',
    ],
    [
      "a relative path",
      `3000 ${dispatch} prefix auth/ 10 60`,
      'line 1: the path "auth/" must start with /',
    ],
    [
      "a count that is no whole number",
      `3010 ${dispatch} any - 1.5 60`,
      'line 1: the count "1.5" must be a whole number from 1',
    ],
    [
      "a zero window",
      `3010 ${dispatch} any - 10 0`,
      'line 1: the seconds "0" must be a whole number from 1',
    ],
    [
      "a host with a port",
      `3010 ${dispatch}:443 any - 10 60`,
      `line 1: the host "${dispatch}:443" must be a host name`,
    ],
    [
      "two thresholds of one window",
      `3010 ${dispatch} any - 10 60\n3010 ${dispatch} any - 20 60`,
      "line 2: rule 3010 already has a threshold of 60 s",
    ],
    [
      "one priority with two matches",
      `3010 ${dispatch} any - 10 60\n3010 ${envoy} any - 20 600`,
      "line 2: rule 3010 names another host, match or path than its line 1",
    ],
  ] as const) {
    test(`refuses ${why}, naming the line`, () => {
      let caught: unknown;
      try {
        parseLimits(text);
      } catch (error) {
        caught = error;
      }
      expect(caught).toBeInstanceOf(LimitsError);
      expect((caught as Error).message).toBe(message);
    });
  }
});
