// packages/envoy-client/src/__tests__/url-join.test.ts
//
// `joinUrl` (url-join.ts): a path joined onto a configured URL's path, its query kept ahead of the
// path's own, and refusals in words that quote neither text.

import { describe, expect, test } from "bun:test";
import { joinUrl } from "../url-join";

/** The credential the refused bases and refs carry in their user information and their query; no
 *  error of `joinUrl` may hold it, or any other part of either text. */
const MARKER = "not-a-secret-token";

describe("joinUrl joins the path onto the base's path", () => {
  for (const [name, base, ref, want, path] of [
    ["leading-slash", "http://host/api?key=v", "/v1/x", "http://host/api/v1/x?key=v", "/api/v1/x"],
    [
      "no-leading-slash",
      "http://host/api?key=v",
      "v1/x",
      "http://host/api/v1/x?key=v",
      "/api/v1/x",
    ],
    [
      "ref-query",
      "http://host/api?key=v",
      "/v1/x?a=b",
      "http://host/api/v1/x?key=v&a=b",
      "/api/v1/x",
    ],
    ["base-no-query", "http://host/api", "v1/x?a=b", "http://host/api/v1/x?a=b", "/api/v1/x"],
    [
      "userinfo-port",
      "https://user:pass@host:8443/api",
      "v1/x",
      "https://user:pass@host:8443/api/v1/x",
      "/api/v1/x",
    ],
    ["trailing-slash", "http://host/api/", "v1/x", "http://host/api/v1/x", "/api/v1/x"],
    [
      "trailing-slash-query",
      "http://host/api/?key=v",
      "/v1/x",
      "http://host/api/v1/x?key=v",
      "/api/v1/x",
    ],
    ["ref-trailing-slash", "http://host/api", "v1/x/", "http://host/api/v1/x/", "/api/v1/x/"],
    ["no-path", "http://host", "sse", "http://host/sse", "/sse"],
    ["root-path", "http://host/", "/sse", "http://host/sse", "/sse"],
    ["fragment", "http://host/api?key=v#frag", "v1/x", "http://host/api/v1/x?key=v", "/api/v1/x"],
    [
      "query-as-written",
      "http://host/api?sig=a%2Bb+c&flag",
      "v1?y=1",
      "http://host/api/v1?sig=a%2Bb+c&flag&y=1",
      "/api/v1",
    ],
    ["dot-segments", "http://host/api/", "./v1/../v2/x", "http://host/api/v2/x", "/api/v2/x"],
    ["empty-ref", "http://host/api/?key=v#frag", "", "http://host/api/?key=v", "/api/"],
    ["query-only", "http://host/api/?key=v", "?a=b", "http://host/api/?key=v&a=b", "/api/"],
  ] as const) {
    test(name, () => {
      const joined = joinUrl(base, ref);
      expect(joined.href).toBe(want);
      expect(joined.pathname).toBe(path);
    });
  }
});

// A segment `encodeURIComponent` wrote reaches the request as the concatenation it replaces wrote
// it - an "@" escaped as it wrote it, an escaped slash as %2F, a space and a percent escaped once -
// and the path the server reads is the same.
describe("joinUrl keeps an escaped segment as written", () => {
  for (const [name, segment] of [
    ["at", "example-fleet@example-project.iam.gserviceaccount.com"],
    ["escaped-slash", "a/b"],
    ["space-percent", "a b%c"],
    ["query-fragment", "a?b#c"],
  ] as const) {
    test(name, () => {
      const ref = `v1/things/${encodeURIComponent(segment)}:do`;
      const concatenated = `http://host/base/${ref}`;
      const joined = joinUrl("http://host/base", ref);
      expect(joined.href).toBe(concatenated);
      expect(joined.pathname).toBe(new URL(concatenated).pathname);
      expect(decodeURIComponent(joined.pathname)).toBe(`/base/v1/things/${segment}:do`);
    });
  }
});

describe("joinUrl refuses in words that quote neither text", () => {
  const good = `http://not-a-secret-user:${MARKER}@host/api?token=${MARKER}`;
  for (const [name, base, ref] of [
    ["base-unparsed", `http://not-a-secret-user:${MARKER}@host:99999/api?token=${MARKER}`, "v1/x"],
    ["base-relative", `/api?token=${MARKER}`, "v1/x"],
    ["base-no-host", `file:///api?token=${MARKER}`, "v1/x"],
    ["base-opaque", `mailto:${MARKER}@example.com`, "v1/x"],
    ["base-empty", "", "v1/x"],
    ["ref-unparsed", good, `v1/x%zz?token=${MARKER}`],
    ["ref-scheme", good, `http://other/v1/x?token=${MARKER}`],
    ["ref-host", good, `//other/v1/x?token=${MARKER}`],
    ["ref-empty-host", good, "//"],
    ["ref-fragment", good, `/v1/x#${MARKER}`],
    ["ref-empty-fragment", good, "/v1/x#"],
    ["ref-colon-first-segment", good, `1a:${MARKER}`],
    ["ref-control-byte", good, `v1/x\u007f${MARKER}`],
    ["ref-backslash", good, `\\\\other\\v1?token=${MARKER}`],
  ] as const) {
    test(name, () => {
      const error = thrownBy(() => joinUrl(base, ref));
      if (!(error instanceof Error)) throw new Error(`joinUrl threw no Error: ${typeof error}`);
      expect(error.message).toStartWith(
        name.startsWith("base") ? "joinUrl: the URL" : "joinUrl: the path"
      );
      expect(error.cause).toBeUndefined();
      for (const piece of [MARKER, "not-a-secret-user"]) {
        expect(error.message).not.toContain(piece);
        expect(String(error.stack)).not.toContain(piece);
      }
    });
  }

  // The twin of base-unparsed: the URL parser's own error carries the text it refused.
  test("the parser's own error for the unparsed base carries it", () => {
    const error = thrownBy(
      () => new URL(`http://not-a-secret-user:${MARKER}@host:99999/api?token=${MARKER}`)
    );
    if (!(error instanceof Error && "input" in error))
      throw new Error("the parser's error names no input");
    expect(String(error.input)).toContain(MARKER);
  });
});

/** What `run` threw, or undefined when it returned. */
function thrownBy(run: () => unknown): unknown {
  try {
    run();
  } catch (error) {
    return error;
  }
  return undefined;
}
