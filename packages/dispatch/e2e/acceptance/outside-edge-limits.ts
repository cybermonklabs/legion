/** Step 4 of the outside driver (`outside-edge.ts`): the peak request rate of the run, against
 *  the edge's rate rules. It imports nothing, so its test runs on any tree.
 *
 *  The rules come from a file of `priority host match path count seconds` lines, one per
 *  threshold: an edge's security policy counts requests per client address per rule, and a rule
 *  may carry two thresholds (a minute's rate and a ban counted over ten minutes), which are two
 *  lines of one priority. `match` is how the rule reads the request's path:
 *    exact           the path is `path`;
 *    prefix          the path starts with `path`;
 *    decoded-prefix  the path, URL-decoded and lower-cased, starts with `path`, as a rule that
 *                    reads `request.path.urlDecode().lower()` does;
 *    any             every path of the host, with `path` written `-`.
 *  The host is compared without case or port. `#` starts a comment line, and a blank line is
 *  skipped.
 *
 *  A request goes to the first rule, by ascending priority, whose host and path match it, as the
 *  policy evaluates its rules, so a sign-in path of Dispatch counts against the sign-in rule and
 *  not the host's catch-all. A rule fails when its peak, the most of its requests inside any one
 *  window of `seconds` (a sliding window, not a clock minute), is above half its `count`. A run
 *  that recorded no request is UNKNOWN: there is nothing to read a peak from. So is a rule whose
 *  host the run asked nothing, whose peak of 0 says nothing of that host; and so is a rule the
 *  run stayed under where the run was cut short (a step it never reached), whose requests are not
 *  a whole run's. A cut run above half a rule's count still fails it.
 */

/** One TSV row of the driver: `row`, `want`, `got` and the row's verdict. */
export interface Row {
  readonly row: string;
  readonly want: string;
  readonly got: string;
  readonly verdict: "PASS" | "FAIL" | "UNKNOWN";
}

/** The run's verdict: FAIL when any row failed, else UNKNOWN when any row could not be read or
 *  there is no row, else PASS. */
export function verdictOf(rows: readonly Row[]): Row["verdict"] {
  if (rows.some((row) => row.verdict === "FAIL")) return "FAIL";
  if (rows.length === 0 || rows.some((row) => row.verdict === "UNKNOWN")) return "UNKNOWN";
  return "PASS";
}

const MATCHES = ["exact", "prefix", "decoded-prefix", "any"] as const;
type Match = (typeof MATCHES)[number];

/** One threshold of one rule. */
export interface Limit {
  readonly priority: number;
  readonly host: string;
  readonly match: Match;
  readonly path: string;
  readonly count: number;
  readonly seconds: number;
  /** The line of the file it was read from, from 1. */
  readonly line: number;
}

/** A limits file that names no rule it can be read by. */
export class LimitsError extends Error {
  override readonly name = "LimitsError";
}

const WHOLE = /^[1-9][0-9]{0,8}$/;
const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;

/** The limits a file's text holds, in its order; any line it cannot read refuses the whole file,
 *  naming that line, since a rule read wrongly is a rule judged wrongly. */
export function parseLimits(text: string): Limit[] {
  const limits: Limit[] = [];
  const lines = text.split("\n");
  for (const [index, raw] of lines.entries()) {
    const line = index + 1;
    const fields = raw
      .replace(/\r$/, "")
      .trim()
      .split(/[ \t]+/);
    if (fields[0] === "" || fields[0]?.startsWith("#")) continue;
    const at = `line ${line}`;
    if (fields.length !== 6) {
      throw new LimitsError(`${at}: want six fields, priority host match path count seconds`);
    }
    const [priority = "", host = "", match = "", path = "", count = "", seconds = ""] = fields;
    if (!WHOLE.test(priority)) {
      throw new LimitsError(`${at}: the priority "${priority}" must be a whole number from 1`);
    }
    if (!HOST.test(host.toLowerCase())) {
      throw new LimitsError(`${at}: the host "${host}" must be a host name`);
    }
    if (!(MATCHES as readonly string[]).includes(match)) {
      throw new LimitsError(`${at}: the match "${match}" is none of ${MATCHES.join(", ")}`);
    }
    if (match === "any" && path !== "-") {
      throw new LimitsError(`${at}: a rule of match any takes the path "-", not "${path}"`);
    }
    if (match !== "any" && !path.startsWith("/")) {
      throw new LimitsError(`${at}: the path "${path}" must start with /`);
    }
    if (!WHOLE.test(count)) {
      throw new LimitsError(`${at}: the count "${count}" must be a whole number from 1`);
    }
    if (!WHOLE.test(seconds)) {
      throw new LimitsError(`${at}: the seconds "${seconds}" must be a whole number from 1`);
    }
    const limit: Limit = {
      priority: Number(priority),
      host: host.toLowerCase(),
      match: match as Match,
      path,
      count: Number(count),
      seconds: Number(seconds),
      line,
    };
    const same = limits.find((one) => one.priority === limit.priority);
    if (
      same !== undefined &&
      (same.host !== limit.host || same.match !== limit.match || same.path !== limit.path)
    ) {
      throw new LimitsError(
        `${at}: rule ${priority} names another host, match or path than its line ${same.line}`
      );
    }
    if (limits.some((one) => one.priority === limit.priority && one.seconds === limit.seconds)) {
      throw new LimitsError(`${at}: rule ${priority} already has a threshold of ${seconds} s`);
    }
    limits.push(limit);
  }
  if (limits.length === 0) throw new LimitsError("the limits file holds no rule");
  return limits;
}

/** One request the driver made: when, in milliseconds, and to what URL. */
export interface Counted {
  readonly at: number;
  readonly url: string;
}

/** Whether `limit`'s rule matches a request to `host` for `path`. */
function matches(limit: Limit, host: string, path: string): boolean {
  if (limit.host !== host) return false;
  switch (limit.match) {
    case "exact":
      return path === limit.path;
    case "prefix":
      return path.startsWith(limit.path);
    case "decoded-prefix": {
      let decoded = path;
      try {
        decoded = decodeURIComponent(path);
      } catch {
        // A path that does not decode is read as it came, as the decoded rule could not read it
        // otherwise either.
      }
      return decoded.toLowerCase().startsWith(limit.path);
    }
    case "any":
      return true;
  }
}

/** The most of `times` (sorted) inside any one window of `windowMs`. */
function peak(times: readonly number[], windowMs: number): number {
  let most = 0;
  let first = 0;
  for (let last = 0; last < times.length; last += 1) {
    while ((times[last] ?? 0) - (times[first] ?? 0) >= windowMs) first += 1;
    most = Math.max(most, last - first + 1);
  }
  return most;
}

/** What step 4 found: a row per threshold, and the peak per minute of each host the run asked. */
export interface Judged {
  readonly rows: Row[];
  readonly hostPeaks: { readonly host: string; readonly perMinute: number }[];
}

/** Judges the run's `requests` against `limits` (the module's header says how). `cut`, where set,
 *  says why the run's requests are not a whole run's. */
export function judgeLimits(
  limits: readonly Limit[],
  requests: readonly Counted[],
  cut?: string
): Judged {
  if (requests.length === 0) {
    return {
      rows: [
        {
          row: "limits",
          want: "the driver's own requests, counted",
          got: "no request was recorded, so no peak can be read",
          verdict: "UNKNOWN",
        },
      ],
      hostPeaks: [],
    };
  }
  const priorities = [...new Set(limits.map((limit) => limit.priority))].sort((a, b) => a - b);
  const byPriority = new Map<number, number[]>(priorities.map((priority) => [priority, []]));
  const byHost = new Map<string, number[]>();
  for (const request of [...requests].sort((a, b) => a.at - b.at)) {
    const url = new URL(request.url);
    const host = url.hostname.toLowerCase();
    byHost.set(host, [...(byHost.get(host) ?? []), request.at]);
    const rule = priorities.find((priority) =>
      limits.some((limit) => limit.priority === priority && matches(limit, host, url.pathname))
    );
    if (rule !== undefined) byPriority.get(rule)?.push(request.at);
  }
  const rows = [...limits]
    .sort((a, b) => a.priority - b.priority || a.seconds - b.seconds)
    .map((limit): Row => {
      const row = `limit-${limit.priority}-${limit.seconds}s`;
      const path = limit.match === "any" ? "" : ` ${limit.path}`;
      const want = `at most ${limit.count / 2} requests in ${limit.seconds} s to ${limit.host} ${limit.match}${path} (half of ${limit.count})`;
      if (!byHost.has(limit.host)) {
        return {
          row,
          want,
          got: `the run asked ${limit.host} nothing, so no peak of it can be read`,
          verdict: "UNKNOWN",
        };
      }
      const most = peak(byPriority.get(limit.priority) ?? [], limit.seconds * 1000);
      const got = `peak ${most} in ${limit.seconds} s from ${limit.host}`;
      if (most * 2 > limit.count) return { row, want, got, verdict: "FAIL" };
      if (cut !== undefined) {
        return {
          row,
          want,
          got: `${got}, but ${cut}, so no whole run was counted`,
          verdict: "UNKNOWN",
        };
      }
      return { row, want, got, verdict: "PASS" };
    });
  const hostPeaks = [...byHost.entries()]
    .map(([host, times]) => ({ host, perMinute: peak(times, 60_000) }))
    .sort((a, b) => a.host.localeCompare(b.host));
  return { rows, hostPeaks };
}
