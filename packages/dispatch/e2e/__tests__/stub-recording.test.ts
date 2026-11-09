/** What the normaliser must and must not hide (e2e/stub/recording.ts).
 *
 *  Two things, which the job depends on in turn. The normaliser decides what a committed
 *  recording holds, and the risk it carries is the one every eraser carries: a field erased is
 *  a field whose drift nothing sees, and a credential left in is a credential published. And
 *  `statusDrift` is the whole of what the nightly judges - the status of each route, never a
 *  body's values and never its shape - so these rows hold it to both directions: a route that
 *  answers differently is found, and two recordings that differ only in what a run decides are
 *  not. `e2e/tools/compare-recordings.ts` calls that same function, so what is read here is
 *  what the job runs.
 */
import { describe, expect, test } from "bun:test";

import { recordedOrigin } from "../stub/origin";
import {
  type Har,
  normalise,
  refuseACredential,
  retargeted,
  statusDrift,
  statusesByRoute,
} from "../stub/recording";

const from = "http://127.0.0.1:51234";

/** One recording of one request, with everything a recording of the same request a minute later
 *  would have written differently left as an argument. */
function recorded(options: {
  at?: string;
  body?: string;
  cookie?: string;
  frame?: string;
  status?: number;
  time?: number;
  url?: string;
}): Har {
  return {
    log: {
      version: "1.2",
      creator: { name: "Playwright", version: "1.64.1" },
      browser: { name: "chromium", version: "156.0.8078.4" },
      pages: [
        { id: options.frame ?? "page@1", startedDateTime: options.at ?? "2026-11-09T00:00:00Z" },
      ],
      entries: [
        {
          _frameref: options.frame ?? "frame@1",
          _monotonicTime: options.time ?? 1000,
          _transferSize: 512,
          pageref: options.frame ?? "page@1",
          serverIPAddress: "127.0.0.1",
          startedDateTime: options.at ?? "2026-11-09T00:00:00Z",
          time: options.time ?? 12.5,
          timings: { connect: 1, receive: 2, send: 0.1, wait: 9 },
          request: {
            // A HAR carries its cookies twice, and this is the copy the headers do not cover.
            cookies: [{ name: "dsession", value: options.cookie ?? "2.YWxpY2U.0.1" }],
            bodySize: 0,
            headersSize: 320,
            headers: [
              { name: "Cookie", value: options.cookie ?? "dsession=2.YWxpY2U.0.1" },
              { name: "Host", value: "127.0.0.1:51234" },
              { name: "Accept", value: "application/json" },
            ],
            method: "GET",
            url: `${from}${options.url ?? "/api/v1/inbox"}`,
          },
          response: {
            bodySize: 81,
            headersSize: 210,
            content: {
              mimeType: "application/json",
              size: 81,
              text: options.body ?? '{"rows":[],"total":0}',
            },
            headers: [
              { name: "Date", value: options.at ?? "Mon, 09 Nov 2026 00:00:00 GMT" },
              { name: "Content-Type", value: "application/json" },
            ],
            status: options.status ?? 200,
          },
        },
      ],
    },
  };
}

describe("a committed recording", () => {
  test("names the placeholder origin, not the harness it was recorded against", () => {
    const written = normalise(recorded({}), from);
    expect(written.log.entries[0].request.url).toBe(`${recordedOrigin}/api/v1/inbox`);
    expect(retargeted(JSON.stringify(written), "http://127.0.0.1:4319")).toContain(
      "http://127.0.0.1:4319/api/v1/inbox"
    );
  });

  test("keeps its bodies exactly as the server wrote them", () => {
    // The committed body is what the page is answered with, so a token in place of a timestamp
    // would make the page render an invalid date. Masking belongs to the comparison alone.
    const body = '{"as_of":"2026-11-09T00:51:05.715869Z","rows":[]}';
    expect(normalise(recorded({ body }), from).log.entries[0].response.content.text).toBe(body);
  });

  test("carries no credential, and one that slipped through is refused by name", () => {
    const written = normalise(recorded({}), from);
    expect(JSON.stringify(written)).not.toContain("dsession");
    expect(JSON.stringify(written)).not.toContain("cookies");
    expect(() =>
      refuseACredential({
        log: {
          entries: [
            {
              request: {
                headers: [{ name: "cookie", value: "dsession=2.YWxpY2U.0.1" }],
                method: "GET",
                url: `${recordedOrigin}/api/v1/inbox`,
              },
              response: { content: {}, headers: [], status: 200 },
            },
          ],
        },
      })
    ).toThrow(/still carries the header cookie/);
    expect(() =>
      refuseACredential({
        log: {
          entries: [
            {
              request: {
                cookies: [{ name: "dsession", value: "2.YWxpY2U.0.1" }],
                headers: [],
                method: "GET",
                url: `${recordedOrigin}/api/v1/inbox`,
              },
              response: { content: {}, headers: [], status: 200 },
            },
          ],
        },
      })
    ).toThrow(/still carries a cookies array/);
  });
});

describe("the drift the nightly judges", () => {
  test("two recordings that differ only in what a run decides are not drift", () => {
    // Every one of these differs between two runs of one row against two databases the harness
    // built minutes apart, and none of them is the server answering differently.
    const first = normalise(
      recorded({
        at: "2026-11-09T00:00:00Z",
        body: '{"as_of":"2026-11-09T00:51:05.7Z","id":"42017c43-4482-482a-8604-f183face92e3","rows":[],"total":0}',
        cookie: "dsession=2.YWxpY2U.0.1",
        frame: "frame@1",
        time: 12.5,
      }),
      from
    );
    const second = normalise(
      recorded({
        at: "2026-11-09T01:17:42Z",
        body: '{"as_of":"2026-11-09T02:11:09.3Z","id":"343a818f-e378-4d4b-9dc6-83512adccc7a","rows":[{"anchor":null}],"total":1}',
        cookie: "dsession=2.YWxpY2U.0.9",
        frame: "frame@beef",
        time: 44.25,
      }),
      from
    );
    expect(statusDrift(first, second)).toEqual([]);
  });

  test("a route that answers another status is drift, named with both answers", () => {
    // What the job exists to find: a row's recording holds a 200 and the server now answers
    // 404, so the stubbed row keeps passing on an answer nobody serves any more.
    const drift = statusDrift(
      normalise(recorded({}), from),
      normalise(recorded({ status: 404 }), from)
    );
    expect(drift).toEqual([
      "GET /api/v1/inbox: the recording holds 200, the server now answers 404",
    ]);
  });

  test("a route only one of the two asked is not drift", () => {
    // A page's reads race: one run makes a poll the next does not. A recording the fresh run
    // wrote nothing for at all is a different finding, and the tool reports that one.
    const before = normalise(recorded({}), from);
    const after = normalise(recorded({ url: "/api/v1/inbox/threads" }), from);
    expect(statusDrift(before, after)).toEqual([]);
    expect(statusDrift(after, before)).toEqual([]);
  });

  test("a request the page cancelled is no answer of the server's", () => {
    // A HAR records a cancelled request with a status of -1, which varies with the run.
    const cancelled = normalise(recorded({ status: -1 }), from);
    expect([...statusesByRoute(cancelled).keys()]).toEqual([]);
  });

  test("the ids the server mints are masked out of a route", () => {
    const one = statusesByRoute(
      normalise(recorded({ url: "/api/v1/artifacts/42017c43-4482-482a-8604-f183face92e3" }), from)
    );
    expect([...one.keys()]).toEqual(["GET /api/v1/artifacts/<uuid>"]);
  });
});
