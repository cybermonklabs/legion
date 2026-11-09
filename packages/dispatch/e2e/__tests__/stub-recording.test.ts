/** What the normaliser must and must not hide (e2e/stub/recording.ts).
 *
 *  The normaliser exists so the re-record job is not red every night, and the risk it carries is
 *  the one every eraser carries: a field erased is a field whose drift nothing sees. These rows
 *  hold it to both halves - a changed value survives it, and the volatile transport does not -
 *  and to the refusal that keeps a credential out of a public repository.
 */
import { describe, expect, test } from "bun:test";

import { recordedOrigin } from "../stub/origin";
import { canonical, type Har, normalise, refuseACredential, retargeted } from "../stub/recording";

const from = "http://127.0.0.1:51234";

/** One recording of one request, with everything a recording of the same request a minute later
 *  would have written differently left as an argument. */
function recorded(options: {
  at?: string;
  body?: string;
  cookie?: string;
  frame?: string;
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
            status: 200,
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

describe("comparing two recordings of one row", () => {
  test("a recording that differs only in what is erased compares equal", () => {
    const first = canonical(normalise(recorded({}), from));
    const second = canonical(
      normalise(
        recorded({
          at: "2026-11-09T01:17:42Z",
          cookie: "dsession=2.YWxpY2U.0.9",
          frame: "frame@beef",
          time: 44.25,
        }),
        from
      )
    );
    expect(second).toBe(first);
  });

  test("a server-minted id and timestamp inside a body compare equal", () => {
    const first = canonical(
      normalise(
        recorded({
          body: '{"as_of":"2026-11-09T00:51:05.7Z","id":"42017c43-4482-482a-8604-f183face92e3"}',
        }),
        from
      )
    );
    const second = canonical(
      normalise(
        recorded({
          body: '{"as_of":"2026-11-09T02:11:09.3Z","id":"343a818f-e378-4d4b-9dc6-83512adccc7a"}',
        }),
        from
      )
    );
    expect(second).toBe(first);
  });

  test("a changed value in a body survives both passes and still differs", () => {
    // The claim the eraser's list is held to: this is the drift the job exists to find.
    const first = canonical(normalise(recorded({ body: '{"rows":[],"total":0}' }), from));
    const second = canonical(normalise(recorded({ body: '{"rows":[],"total":1}' }), from));
    expect(second).not.toBe(first);
  });

  test("a changed route still differs", () => {
    const first = canonical(normalise(recorded({ url: "/api/v1/inbox" }), from));
    const second = canonical(normalise(recorded({ url: "/api/v1/inbox/threads" }), from));
    expect(second).not.toBe(first);
  });

  test("two entries of one route and method keep the order they were answered in", () => {
    // `routeFromHAR` answers with the first matching entry, so a row that reads one URL twice
    // and is answered twice would be given the second answer first if the sort reordered them.
    const twice = recorded({ body: "first" });
    twice.log.entries.push({
      ...twice.log.entries[0],
      response: {
        ...twice.log.entries[0].response,
        content: { mimeType: "text/plain", text: "second" },
      },
    });
    const written = normalise(twice, from);
    expect(written.log.entries.map((entry) => entry.response.content.text)).toEqual([
      "first",
      "second",
    ]);
  });
});
