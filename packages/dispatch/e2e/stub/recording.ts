/** What a committed recording is, and how two of them are compared.
 *
 *  A HAR Playwright writes is never the same twice: it carries the moment of each request, every
 *  timing, the frame and page it came from, the connection's own details, the sizes the transport
 *  made and the `Date` the server stamped. None of that is the contract between the dashboard and
 *  its server, and all of it would make the re-record job
 *  (.github/workflows/dispatch-browser-nightly.yaml) red every night. So a recording is
 *  **normalised as it is written**, and the job compares normalised recordings.
 *
 *  Two forms, and the difference between them is the point:
 *
 *  - `normalise` is what is committed and what is replayed. It erases the volatile transport and
 *    every credential, and it leaves every response body exactly as the server wrote it. A body
 *    with a token in place of its timestamps would make the page render an invalid date, so the
 *    committed body is never rewritten.
 *  - `canonical` is the comparison form, built from a normalised recording and never written
 *    anywhere. It masks the values a second recording of the same row cannot repeat - the ids the
 *    server mints and the times it stamps - inside bodies and URLs alike. It exists so the job
 *    fails on a changed **shape** and not on a changed id.
 *
 *  Every erasure is a field whose drift nothing will see, so the list is short and each entry
 *  carries its reason. `e2e/__tests__/stub-recording.test.ts` holds the two claims that keep it
 *  honest: a body whose value changed still differs after both passes, and a recording that
 *  differs only in what is erased does not.
 */

import { recordedOrigin } from "./origin";

/** As much of a HAR as this module reads. Playwright writes more, and what it writes is kept
 *  except where `normalise` says otherwise. */
export interface HarHeader {
  name: string;
  value: string;
}

export interface HarEntry {
  request: {
    method: string;
    url: string;
    headers: HarHeader[];
    postData?: { mimeType?: string; text?: string };
    [key: string]: unknown;
  };
  response: {
    status: number;
    headers: HarHeader[];
    content: { mimeType?: string; size?: number; text?: string; [key: string]: unknown };
    [key: string]: unknown;
  };
  [key: string]: unknown;
}

export interface Har {
  log: {
    entries: HarEntry[];
    pages?: unknown[];
    creator?: unknown;
    browser?: unknown;
    [key: string]: unknown;
  };
}

/** The entry fields erased, and why each one is not the contract.
 *
 *  `startedDateTime`, `time`, `timings`, `_monotonicTime` - when the request was made and how
 *  long each phase took, which differ between any two runs.
 *  `pageref`, `_frameref` - Playwright's own identifiers for the page and frame, minted per run.
 *  `serverIPAddress`, `_serverPort`, `_securityDetails`, `connection`, `cache` - the transport,
 *  which the replay never uses: `routeFromHAR` answers from the file.
 *  `headersSize`, `bodySize`, `_transferSize` - byte counts of a transport this file replaces;
 *  the body's own `content.size` is kept, since it is derived from the body and drifts with it. */
const erasedEntryFields = [
  "_frameref",
  "_monotonicTime",
  "_securityDetails",
  "_serverPort",
  "_transferSize",
  "cache",
  "connection",
  "pageref",
  "serverIPAddress",
  "startedDateTime",
  "time",
  "timings",
] as const;

/** The message fields erased from a request and from a response.
 *
 *  `bodySize` and `headersSize` are byte counts of a transport this file replaces.
 *
 *  `cookies` is the parallel copy of the `Cookie` and `Set-Cookie` headers that a HAR carries
 *  beside `headers`, and it is the one that caught this layer out: erasing the headers alone
 *  left the `dsession` the sign-in issued in every committed recording, where the drift job
 *  found it. The refusal below reads both shapes now. */
const erasedMessageFields = ["bodySize", "cookies", "headersSize"] as const;

/** The headers erased from every request and response, and why.
 *
 *  A credential, which must never reach a public repository: `cookie` (the `dsession` the sign-in
 *  issued), `set-cookie`, `authorization`, `idempotency-key` (one write's intent, which a replay
 *  of a different write must not reuse).
 *  A value that differs between two runs and says nothing about the answer: `date` (the server's
 *  clock), `host` and `referer` (the origin the recording was made from, which this file no
 *  longer names), `user-agent` and the `sec-ch-ua` family (the browser build, which moves with
 *  Playwright), `if-none-match` (the browser's own cache state). */
const erasedHeaders: Record<string, true> = {
  authorization: true,
  cookie: true,
  date: true,
  host: true,
  "idempotency-key": true,
  "if-none-match": true,
  referer: true,
  "sec-ch-ua": true,
  "sec-ch-ua-mobile": true,
  "sec-ch-ua-platform": true,
  "set-cookie": true,
  "user-agent": true,
};

/** The headers a recording must not carry once it is normalised. A recording that still holds one
 *  is refused rather than cleaned later: a credential reviewed into the tree is a credential
 *  published. */
const secretHeaders: Record<string, true> = {
  authorization: true,
  cookie: true,
  "idempotency-key": true,
  "set-cookie": true,
};

function withoutFields<T extends Record<string, unknown>>(
  value: T,
  fields: readonly string[]
): Record<string, unknown> {
  const kept: Record<string, unknown> = {};
  for (const [key, held] of Object.entries(value)) {
    if (!fields.includes(key)) kept[key] = held;
  }
  return kept;
}

/** The recording to commit: the volatile transport and every credential gone, the entries in an
 *  order two recordings of one row agree on, and every URL on the placeholder origin.
 *
 *  `from` is the origin the recording was made against, which is the harness server's and so
 *  carries a port the OS chose.
 *
 *  The sort is stable within one `(method, url)` group, so a row that asks for one URL twice and
 *  is answered twice keeps the two answers in the order it was given them: `routeFromHAR` serves
 *  the first matching entry, and reversing them would answer the first read with the second
 *  answer. */
export function normalise(har: Har, from: string): Har {
  const entries = har.log.entries.map((entry, index) => ({ entry, index }));
  entries.sort((a, b) => {
    const method = a.entry.request.method.localeCompare(b.entry.request.method);
    if (method !== 0) return method;
    const url = a.entry.request.url.localeCompare(b.entry.request.url);
    if (url !== 0) return url;
    return a.index - b.index;
  });
  const normalised: Har = {
    log: {
      ...withoutFields(har.log, ["browser", "creator", "pages"]),
      entries: entries.map(({ entry }) => {
        const kept = withoutFields(entry, [...erasedEntryFields]) as unknown as HarEntry;
        return {
          ...kept,
          request: {
            ...(withoutFields(entry.request, [...erasedMessageFields]) as HarEntry["request"]),
            headers: entry.request.headers.filter(
              (header) => erasedHeaders[header.name.toLowerCase()] !== true
            ),
            // The URL loses the origin it was recorded against, which is the harness server's
            // and carries a port the OS chose. A URL on another origin is left alone: the
            // dashboard asks for nothing off its own origin, so one here is a request this
            // layer does not own and a reader should see it.
            url: entry.request.url.startsWith(from)
              ? `${recordedOrigin}${entry.request.url.slice(from.length)}`
              : entry.request.url,
          },
          response: {
            ...(withoutFields(entry.response, [...erasedMessageFields]) as HarEntry["response"]),
            headers: entry.response.headers.filter(
              (header) => erasedHeaders[header.name.toLowerCase()] !== true
            ),
          },
        };
      }),
    },
  };
  refuseACredential(normalised);
  return normalised;
}

/** Refuses a normalised recording that still carries a credential, naming the entry and the
 *  header. Nothing downstream cleans one up, so this is where a leak is stopped. */
export function refuseACredential(har: Har): void {
  for (const entry of har.log.entries) {
    for (const held of [entry.request, entry.response]) {
      // A HAR carries its cookies twice: as headers, and as this parallel array. Erasing one
      // and not the other is exactly the mistake this refusal exists to catch.
      const cookies = held.cookies;
      if (Array.isArray(cookies) && cookies.length > 0) {
        throw new Error(
          `This recording still carries a cookies array on ${entry.request.method} ` +
            `${entry.request.url}. A recording is committed to a public repository, so the ` +
            "normaliser erases every credential as it writes one; this one was not written " +
            "through e2e/stub/recording.ts's `normalise`."
        );
      }
    }
    for (const headers of [entry.request.headers, entry.response.headers]) {
      for (const header of headers) {
        if (secretHeaders[header.name.toLowerCase()] === true) {
          throw new Error(
            `This recording still carries the header ${header.name} on ` +
              `${entry.request.method} ${entry.request.url}. A recording is committed to a ` +
              "public repository, so the normaliser erases every credential as it writes one; " +
              "this one was not written through e2e/stub/recording.ts's `normalise`."
          );
        }
      }
    }
  }
}

/** A UUID, which is what the server mints an id as. It is masked in a URL, so two recordings of
 *  one row name the same route rather than two. */
const uuidPattern = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi;

/** What the dashboard asked of the server in one recording, and how the server answered: each
 *  route it asked, with the statuses that route answered with.
 *
 *  **It compares routes and statuses, never a body's values and never a body's shape, and that
 *  is the whole of the judgement in this file.** Two recordings of one row are made against two
 *  databases the harness built minutes apart, and the server answers each with ids it minted,
 *  counters it incremented, times it stamped, lists that are empty in one run and not the
 *  other, and nullable fields that are null in one and an object in the other. A comparison of
 *  values, or even of key paths, is red every night on all of that and tells a reader nothing;
 *  masking enough of it to be green would mask the very things the job exists to watch.
 *
 *  So the two guards divide the work. A body is held to its declared shape on **every pull
 *  request**, against the contract the Go route table generates
 *  (`e2e/__tests__/stub-recordings.test.ts`), which is where a renamed, removed or retyped
 *  field fails - on the commit that causes it. This job watches what a schema cannot see and
 *  what does not vary with the data: a route that answers another status than the recording
 *  holds. A recording whose row now gets a 404 or a 403 where it was given a 200 is stale, and
 *  a stubbed row would keep passing on the old answer until someone looked. A route one
 *  recording holds and the other does not is left alone, because a page's reads race: the
 *  comparison is over the routes both recordings asked (`e2e/tools/compare-recordings.ts`). */
export function shapes(har: Har): Map<string, string> {
  const statuses = new Map<string, Set<number>>();
  for (const entry of har.log.entries) {
    // A request the page cancelled, which a HAR records with a status of -1 or 0: the server
    // answered nothing, so there is nothing of the server's to compare, and whether a
    // navigation beat a read is the run's own race.
    if (entry.response.status <= 0) continue;
    // The route, not the URL: the ids the server minted are masked, and the query is left out
    // because a page polls - two recordings of one row ask `?limit=1&order=desc` and
    // `?after=2&limit=1` of the same route depending on what the first read answered. A query
    // that changed for a reason is caught where it matters: `routeFromHAR` matches a request by
    // its whole URL, so such a row fails its own replay, which is louder than this job.
    const route = new URL(entry.request.url).pathname.replace(uuidPattern, "<uuid>");
    const asked = `${entry.request.method} ${route}`;
    const answered = statuses.get(asked) ?? new Set<number>();
    answered.add(entry.response.status);
    statuses.set(asked, answered);
  }
  const byRoute = new Map<string, string>();
  for (const [asked, answered] of statuses) {
    byRoute.set(asked, [...answered].sort().join(", "));
  }
  return byRoute;
}

export function canonical(har: Har): string {
  return `${[...shapes(har)]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([asked, answered]) => `${asked} ${answered}`)
    .join("\n")}\n`;
}

/** A committed recording's text with the placeholder origin replaced by `origin`, which is the
 *  run's own stub server. `routeFromHAR` matches a request by its whole URL, so a recording is
 *  unusable until this is done. */
export function retargeted(text: string, origin: string): string {
  return text.replaceAll(recordedOrigin, origin);
}

/** A recording as it is written to disk: two spaces and a trailing newline, so a diff reads. */
export function written(har: Har): string {
  return `${JSON.stringify(har, null, 2)}\n`;
}
