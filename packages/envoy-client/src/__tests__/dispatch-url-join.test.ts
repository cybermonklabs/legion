// packages/envoy-client/src/__tests__/dispatch-url-join.test.ts
//
// Every request DispatchClient sends goes to its route joined onto the configured Dispatch URL's
// path, the URL's query kept ahead of the request's own query, and its user information, host and
// port kept. Resolved against the URL's text with a slash appended, a configured query took the
// slash (`/dispatch?key=v/`), so the request lost the URL's path and its query alike and asked
// `/api/v1/...` on the bare host. A URL with no query is asked as before.
//
// A Dispatch URL that does not parse fails the request in the client's own words. The URL parser's
// error held the URL whole as its `base`, which `JSON.stringify` and inspection print. Each print
// check is the designated assertion of cmd/redact-gate's mutation gate: it prints the output it
// read as `REDACT-CAPTURE <capture> <base64>` and fails with a message that begins
// `REDACT-ASSERT <capture>`.

import { describe, expect, test } from "bun:test";
import { inspect } from "node:util";
import { bearerCredential } from "../bearer";
import { DispatchClient } from "../dispatch-http";
import { fakeSearchResponse } from "./fake-search-response";

const TOKEN = "not-a-secret-token";

const actor = {
  kind: "session" as const,
  id: "session-1",
  origin: { host: "omp" as const, cwd: "/workspace", session_title: "Implement" },
};

/** A fetch that records what each request's URL names (user information, host and port, path
 *  and query) and answers `body` as JSON. */
function recordingFetch(body: unknown, status = 200) {
  const asked: string[][] = [];
  const fetchImpl = (async (input: RequestInfo | URL) => {
    const url = new URL(String(input));
    asked.push([url.username, url.password, url.host, url.pathname, url.search]);
    return new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;
  return { asked, fetchImpl };
}

describe("a Dispatch request goes to its route joined onto the configured URL's path", () => {
  for (const [name, base, origin, query] of [
    ["query", "http://dispatch.test/dispatch?key=v", ["", "", "dispatch.test"], "?key=v"],
    [
      "query-trailing-slash",
      "http://dispatch.test/dispatch/?key=v",
      ["", "", "dispatch.test"],
      "?key=v",
    ],
    [
      "query-userinfo-port",
      "http://user:pass@dispatch.test:8080/dispatch?key=v",
      ["user", "pass", "dispatch.test:8080"],
      "?key=v",
    ],
    ["no-query", "http://dispatch.test/dispatch", ["", "", "dispatch.test"], ""],
  ] as const) {
    test(name, async () => {
      const search = recordingFetch(fakeSearchResponse([]));
      await new DispatchClient(base, bearerCredential("secret"), search.fetchImpl).search(
        "astrolabe sextant",
        { project: "LEGION", limit: 5 }
      );
      const own = "q=astrolabe+sextant&project=LEGION&limit=5";
      expect(search.asked).toEqual([
        [...origin, "/dispatch/api/v1/search", query === "" ? `?${own}` : `${query}&${own}`],
      ]);

      const project = { key: "CORE", name: "Core" };
      const write = recordingFetch(project, 201);
      await new DispatchClient(base, bearerCredential("secret"), write.fetchImpl).createProject({
        key: "CORE",
        name: "Core",
        actor,
      });
      expect(write.asked).toEqual([[...origin, "/dispatch/api/v1/projects", query]]);
    });
  }
});

// `dispatch api`'s read of a route no method names (readRoute) goes where every other route goes:
// joined onto the configured URL's path, the URL's query ahead of the route's own. Resolved
// against the URL's text with a slash appended, it asked /api/v1/... on the bare host and lost the
// URL's query.
describe("dispatch api's route read is joined onto the configured URL's path", () => {
  for (const [name, base, query] of [
    ["query", "http://dispatch.test/dispatch?key=v", "?key=v&scope=open"],
    ["no-query", "http://dispatch.test/dispatch", "?scope=open"],
  ] as const) {
    test(name, async () => {
      const read = recordingFetch({ limits: [] });
      await new DispatchClient(base, bearerCredential("secret"), read.fetchImpl).readRoute(
        "/api/v1/limits?scope=open"
      );
      expect(read.asked).toEqual([["", "", "dispatch.test", "/dispatch/api/v1/limits", query]]);
    });
  }
});

/** The designated assertion: capture text, then fail where it holds the token. */
function requireNone(capture: string, text: string): void {
  console.log(`REDACT-CAPTURE ${capture} ${Buffer.from(text, "utf8").toString("base64")}`);
  expect(
    text.includes(TOKEN),
    `REDACT-ASSERT ${capture}: the output prints the secret ${TOKEN}: ${text}`
  ).toBe(false);
}

describe("a Dispatch URL that does not parse fails the request without printing it", () => {
  for (const [name, base] of [
    ["unparsed-userinfo", `http://user:${TOKEN}@[dispatch`],
    ["unparsed-query", `http://dispatch test/?key=${TOKEN}`],
  ] as const) {
    test(name, async () => {
      const { asked, fetchImpl } = recordingFetch({});
      const refusal = (await new DispatchClient(base, bearerCredential("secret"), fetchImpl)
        .search("astrolabe")
        .then(
          () => null,
          (error: unknown) => error
        )) as Error;
      expect(refusal).toBeInstanceOf(Error);
      expect(asked).toEqual([]);
      const forms = {
        message: refusal.message,
        String: String(refusal),
        "JSON.stringify": JSON.stringify(refusal),
        "Bun.inspect": Bun.inspect(refusal),
        "util.inspect": inspect(refusal, { showHidden: true, depth: 5 }),
        stack: refusal.stack ?? "",
      };
      for (const [form, text] of Object.entries(forms)) {
        requireNone(`dispatch-http.url/${name}/${form}`, text);
      }
      expect(refusal.message).toBe(
        "the configured Dispatch URL is not an absolute URL with a host (not printed)"
      );

      // The twin: the URL parser's own error, which the client threw before, holds the token.
      let parser: unknown;
      try {
        new URL("api/v1/search", `${base}/`);
      } catch (error) {
        parser = error;
      }
      expect(parser).toBeInstanceOf(TypeError);
      expect(Bun.inspect(parser)).toContain(TOKEN);
      expect(JSON.stringify(parser)).toContain(TOKEN);
    });
  }
});
