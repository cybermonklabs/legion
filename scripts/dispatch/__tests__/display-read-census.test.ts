import { afterAll, describe, expect, spyOn, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { bearerCredential } from "../../../packages/envoy-client/src/bearer.ts";
import fixture from "../__fixtures__/display-read-census.json" with { type: "json" };
import {
  CLASSES,
  canonicalTree,
  censusReport,
  classify,
  type Group,
  get,
  isSubsequence,
  MIN_STORED_PARAGRAPH,
  type PmNode,
  paragraphs,
  Rate,
  readingMode,
  type StoredText,
  storedParagraphs,
  storedTextIn,
  type TextClass,
  versionOrder,
  visibleText,
} from "../display-read-census.ts";
import { holdDeadlines } from "./held-deadlines.ts";

/** One fixture case: the two readings of one text and the class the census puts it in. */
interface FixtureCase {
  readonly id: string;
  readonly group: Group;
  readonly markdown: string;
  /** `the readers` for a reading this repository's own two readers made of `markdown`, `written`
   * for a pair written by hand because no text produced it. */
  readonly from: string;
  readonly why: string;
  readonly go: PmNode | null;
  readonly browser: PmNode | null;
  readonly browserError?: string;
  readonly class: TextClass;
}

const cases = fixture.cases as unknown as readonly FixtureCase[];

function reading(one: FixtureCase) {
  return {
    go: one.go,
    browser: one.browser,
    ...(one.browserError === undefined ? {} : { browserError: one.browserError }),
  };
}

describe("the display read census classes a text", () => {
  for (const one of cases) {
    test(`${one.class}: ${one.id}`, () => {
      expect(classify(reading(one))).toBe(one.class);
    });
  }

  test("every class the census can report has a case", () => {
    expect([...new Set(cases.map((one) => one.class))].sort()).toEqual([...CLASSES].sort());
  });

  test("the readers themselves made every case but the two the fixture says are written", () => {
    expect(cases.filter((one) => one.from === "written").map((one) => one.class)).toEqual([
      "format differs",
      "Go cannot read it",
    ]);
  });

  test("a document version is held to the editing engine, every other text to the read-only one", () => {
    expect(readingMode("document-version")).toBe("version");
    expect(readingMode("comment")).toBe("latest");
    expect(readingMode("ask-answer")).toBe("latest");
  });
});

describe("the comparison the classes rest on", () => {
  test("block ids, null attributes and mark order do not make two trees differ", () => {
    const left: PmNode = {
      type: "doc",
      content: [
        {
          type: "paragraph",
          attrs: { blockId: "b-1" },
          content: [
            {
              type: "text",
              text: "x",
              marks: [{ type: "strong" }, { type: "emphasis", attrs: { marker: "*" } }],
            },
          ],
        },
      ],
    };
    const right: PmNode = {
      type: "doc",
      content: [
        {
          type: "paragraph",
          attrs: { blockId: "b-9", title: null },
          content: [
            {
              type: "text",
              text: "x",
              marks: [{ type: "emphasis", attrs: { marker: "*" } }, { type: "strong" }],
            },
          ],
        },
      ],
    };
    expect(canonicalTree(left)).toBe(canonicalTree(right));
  });

  test("an attribute that is not the block id does make them differ", () => {
    expect(canonicalTree({ type: "doc", attrs: { a: 1 } })).not.toBe(
      canonicalTree({ type: "doc", attrs: { a: 2 } })
    );
  });

  test("a short table body row is padded to the header's width", () => {
    const cell = (alignment: string): PmNode => ({
      type: "table_cell",
      attrs: { alignment, colspan: 1, colwidth: null, rowspan: 1 },
      content: [{ type: "paragraph" }],
    });
    const table = (row: readonly PmNode[]): PmNode => ({
      type: "doc",
      content: [
        {
          type: "table",
          content: [
            { type: "table_header_row", content: [cell("left"), cell("right")] },
            { type: "table_row", content: [...row] },
          ],
        },
      ],
    });
    // The padded cell takes its header cell's alignment, which is `right` here, so a row padded
    // with a `left` cell would not compare equal.
    expect(canonicalTree(table([cell("left")]))).toBe(
      canonicalTree(table([cell("left"), cell("right")]))
    );
    expect(canonicalTree(table([cell("left")]))).not.toBe(
      canonicalTree(table([cell("left"), cell("left")]))
    );
    // A row holding no cell is a table with no body row to either reader, and stays empty.
    expect(canonicalTree(table([]))).not.toBe(canonicalTree(table([cell("left"), cell("right")])));
  });

  test("the visible text is every text node's text, run together", () => {
    expect(
      visibleText({
        type: "doc",
        content: [
          { type: "heading", content: [{ type: "text", text: "Head" }] },
          {
            type: "paragraph",
            content: [
              { type: "text", text: "a" },
              { type: "hardbreak" },
              { type: "text", text: "b" },
            ],
          },
        ],
      })
    ).toBe("Headab");
  });

  test("a subsequence is the whole with characters deleted, equality included", () => {
    expect(isSubsequence("ace", "abcde")).toBe(true);
    expect(isSubsequence("abcde", "abcde")).toBe(true);
    expect(isSubsequence("aec", "abcde")).toBe(false);
    expect(isSubsequence("", "abc")).toBe(true);
    expect(isSubsequence("abc", "")).toBe(false);
  });

  test("versions are read newest first, one document at a time round the documents", () => {
    expect(
      versionOrder([
        { artifact: "long", number: 1 },
        { artifact: "long", number: 2 },
        { artifact: "long", number: 3 },
        { artifact: "short", number: 1 },
      ])
    ).toEqual([
      { artifact: "long", number: 3 },
      { artifact: "short", number: 1 },
      { artifact: "long", number: 2 },
      { artifact: "long", number: 1 },
    ]);
    expect(versionOrder([])).toEqual([]);
  });
});

describe("the report", () => {
  const classed = cases.map((one) => ({
    id: one.id,
    group: one.group,
    class: one.class,
  }));
  const spent = [
    { unit: "issue-comments/A-1", texts: [], requests: 3, at: 10_000, ms: 1_000 },
    { unit: "issue-asks/A-1", texts: [], requests: 2, at: 12_000, ms: 500, error: "404" },
  ];
  const report = censusReport(classed, spent, 4);

  test("every class is printed, a class with nothing in it as zero", () => {
    expect(report.classes.map((count) => count.class)).toEqual([...CLASSES]);
    expect(report.classes.reduce((total, count) => total + count.texts, 0)).toBe(classed.length);
  });

  test("each class splits by group", () => {
    const agree = report.classes.find((count) => count.class === "the two agree");
    expect(agree?.groups).toEqual({ comment: 2, "document-version": 1 });
  });

  test("the groups count the same texts as the classes", () => {
    expect(report.groups.reduce((total, row) => total + row.texts, 0)).toBe(classed.length);
  });

  test("a class that must be empty is reported text by text", () => {
    expect(report.defects.map((row) => row.class).sort()).toEqual([
      "Go cannot read it",
      "Go loses text",
      "the text differs both ways",
    ]);
  });

  test("the requests, the reading time and the refused units come from the units read", () => {
    expect(report.requests).toBe(5);
    expect(report.readSeconds).toBe(3);
    expect(report.classSeconds).toBe(4);
    expect(report.failedUnits).toBe(1);
  });
});

describe("--check-no-stored-text", () => {
  const stored = fixture.guard.stored as readonly StoredText[];
  const index = storedParagraphs(stored);

  test("a paragraph shorter than the guard's length is not indexed", () => {
    expect(paragraphs("Short.")).toEqual([]);
    expect(paragraphs("x".repeat(MIN_STORED_PARAGRAPH))).toEqual([
      "x".repeat(MIN_STORED_PARAGRAPH),
    ]);
  });

  test("a file holding a stored paragraph is refused, and the stored text named", () => {
    const hits = storedTextIn("findings.md", fixture.guard.holds, index);
    expect(hits.map((hit) => hit.id)).toEqual(["version/guard-2/1"]);
    expect(hits[0].group).toBe("document-version");
  });

  test("a file holding none is accepted", () => {
    expect(storedTextIn("findings.md", fixture.guard.clean, index)).toEqual([]);
  });

  test("re-wrapping a stored paragraph does not hide it", () => {
    const wrapped = fixture.guard.holds.replace(/ /gu, "\n");
    expect(storedTextIn("wrapped.md", wrapped, index).map((hit) => hit.id)).toEqual([
      "version/guard-2/1",
    ]);
  });
});

describe("reading production", () => {
  test("display-read-census's read hands its credential the request's deadline", async () => {
    const held = holdDeadlines(() => Response.json([]));
    try {
      const config = { url: "http://dispatch.test", credential: held.credential };
      await get(config, "/api/v1/projects", new Rate(1000), { requests: 0 });
      held.expectEachRequestBoundByItsDeadline("display-read-census's read");
    } finally {
      held.restore();
    }
  });

  // A gateway or a proxy in Dispatch's place can answer a read with a page of its own that
  // echoes the request back, the token with it. The unit's error names such a page in one line
  // with the token that read presented redacted, a 4xx at once and a 5xx once every attempt has
  // failed, and Dispatch's own error JSON as Dispatch wrote it.
  describe("a read refused", () => {
    let status = 403;
    /** A gateway's page echoing the read: its bearer bare on a line of its own, first, so only
     *  the redaction of that very bearer can hide it, then the header as it came. */
    const echoPage = (request: Request): Response => {
      const authorization = request.headers.get("authorization") ?? "";
      return new Response(
        `<html><body>\n${authorization.replace(/^Bearer /, "")}\n<pre>GET /api/v1/projects\n` +
          `authorization: ${authorization}</pre></body></html>\n`,
        { headers: { "Content-Type": "text/html" }, status }
      );
    };
    let answer: (request: Request) => Response = echoPage;
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch: (request) => answer(request),
    });
    afterAll(() => server.stop(true));

    const refusalOf = async (token: string): Promise<{ status: number; message: string }> => {
      const config = {
        url: `http://127.0.0.1:${server.port}`,
        credential: bearerCredential(token),
      };
      try {
        await get(config, "/api/v1/projects", new Rate(1000), { requests: 0 });
      } catch (error) {
        const refused = error as Error & { status: number };
        return { status: refused.status, message: refused.message };
      }
      throw new Error("the read answered");
    };

    test("a 4xx proxy's page is named in one line, the bearer redacted", async () => {
      const token = fixtureSecret("display");
      status = 403;
      answer = echoPage;
      const refused = await refusalOf(token);
      expect(refused.status).toBe(403);
      expect(refused.message).toStartWith("403 a body that is not Dispatch's error JSON (");
      expect(refused.message).not.toContain(token);
      expect(refused.message).not.toContain("\n");
    });

    test("a 5xx proxy's page is named in the last failure, the bearer redacted", async () => {
      const token = fixtureSecret("display");
      status = 502;
      answer = echoPage;
      // The waits between the attempts are what a 5xx earns; the read's answer is what is held.
      const sleep = spyOn(Bun, "sleep").mockResolvedValue(undefined);
      try {
        const refused = await refusalOf(token);
        expect(refused.status).toBe(0);
        expect(refused.message).toStartWith(
          "GET /api/v1/projects: 5 attempts failed: 502 a body that is not Dispatch's error JSON ("
        );
        expect(refused.message).not.toContain(token);
        expect(refused.message).not.toContain("\n");
      } finally {
        sleep.mockRestore();
      }
    });

    test("Dispatch's own error JSON is named as Dispatch wrote it", async () => {
      const refusal = { code: "PROJECT_NOT_FOUND", error: "no project carries the key ACME" };
      answer = () => Response.json(refusal, { status: 404 });
      const refused = await refusalOf(fixtureSecret("display"));
      expect(refused).toEqual({ status: 404, message: `404 ${JSON.stringify(refusal)}` });
    });
  });
});
