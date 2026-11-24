import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  PROTOCOL_METHODS,
  PUBLIC_REQUEST_NAMES,
  type PublicRequestName,
} from "./public-request-names";
import { requestCredentials } from "./remote-text";

// The names a credential travels under, which Go's public_names_test.go reads too.
const FORBIDDEN_PATH = new URL(
  "../../envoy/internal/redact/testdata/credential-names.json",
  import.meta.url
);
const forbidden: { names: string[]; headerPatterns: string[] } = JSON.parse(
  readFileSync(FORBIDDEN_PATH, "utf8")
);

/** An entry's route: an upper-case method, one space and a path from its root. */
const ROUTE_SHAPE = /^[A-Z]+ \/[^ ]*$/;

/** A route on every path of its method. */
const EVERY_PATH = /^[A-Z]+ \/\*$/;

/** Why `route`, one of `entry`'s, is no route its source names, or "" where it is one: a route on
 *  every path of its method is one a protocol of PROTOCOL_METHODS fixes, whose section the source
 *  cites, since no contract writes `/*` and a source that repeats the route names nothing; any
 *  other route is one the source names in its own words. Go's `routeProblem`. */
function routeProblem(entry: PublicRequestName, route: string): string {
  if (!ROUTE_SHAPE.test(route)) return 'is no "<METHOD> /<path>"';
  if (EVERY_PATH.test(route)) {
    return PROTOCOL_METHODS.some(
      (protocol) => protocol.route === route && entry.source.includes(protocol.source)
    )
      ? ""
      : "is every path of its method, which no protocol its source cites fixes (PROTOCOL_METHODS)";
  }
  return entry.source.includes(route) ? "" : "is a route its source does not name";
}

// Every entry names the contract that declares its value public, and each route it is public on
// in its source's words, or for every path of a method a protocol its source cites, and none is a
// name a credential travels under; every protocol is a method's every path an entry cites. Go's
// TestThePublicNameTableHoldsOnlyContractNamesNoCredentialTravelsUnder.
test("the public-name table holds only contract names no credential travels under", () => {
  expect(forbidden.names.length).toBeGreaterThan(0);
  expect(PUBLIC_REQUEST_NAMES.length).toBeGreaterThan(0);
  expect(PROTOCOL_METHODS.length).toBeGreaterThan(0);
  const patterns = forbidden.headerPatterns.map((pattern) => new RegExp(pattern, "i"));
  for (const entry of PUBLIC_REQUEST_NAMES) {
    expect(entry.source.trim(), `${entry.name} names no contract`).not.toBe("");
    for (const route of entry.routes ?? []) {
      expect(routeProblem(entry, route), `${entry.name} is public on ${route}`).toBe("");
    }
    for (const name of forbidden.names) {
      expect(entry.name.toLowerCase(), `${entry.name} is a credential's name`).not.toBe(
        name.toLowerCase()
      );
    }
    for (const pattern of patterns) {
      expect(
        entry.place === "header" && pattern.test(entry.name),
        `${entry.name} matches ${pattern}`
      ).toBe(false);
    }
  }
  for (const protocol of PROTOCOL_METHODS) {
    expect(EVERY_PATH.test(protocol.route), `${protocol.source} is a method's every path`).toBe(
      true
    );
    expect(protocol.source.trim(), `${protocol.route} names its protocol's section`).not.toBe("");
    expect(protocol.words.trim(), `${protocol.source} quotes its own words`).not.toBe("");
    expect(
      PUBLIC_REQUEST_NAMES.some(
        (entry) =>
          (entry.routes ?? []).includes(protocol.route) && entry.source.includes(protocol.source)
      ),
      `an entry public on ${protocol.route} cites ${protocol.source}`
    ).toBe(true);
  }
});

// A route on every path of its method passes only from a source that cites a protocol fixing that
// method, never from one that only repeats the route; any other route only from a source that
// names it. Go's TestARouteOnEveryPathIsOneAProtocolItsSourceCitesFixes.
test("a route on every path is one a protocol its source cites fixes", () => {
  const rows: readonly [source: string, route: string, refused: boolean][] = [
    ["POST /* (the token request, a POST to the path its issuer sets)", "POST /*", true],
    ["RFC 6749 §3.2: the token request", "GET /*", true],
    ["the token request", "POST /*", true],
    ["RFC 6749 §4.1.3; the token request is a POST by RFC 6749 §3.2", "POST /*", false],
    ["POST /v1/oauth/token (the broker's token exchange)", "POST /v1/oauth/token", false],
    ["POST /v1/oauth/revoke (the broker's revocation)", "POST /v1/oauth/token", true],
    ["GET v1/sessions", "GET v1/sessions", true],
  ];
  for (const [source, route, refused] of rows) {
    const entry: PublicRequestName = { place: "field", name: "planted", source, routes: [route] };
    expect(routeProblem(entry, route) !== "", `${route} from ${source}`).toBe(refused);
  }
});

// A value under a name no contract declares public, a new one included, is a credential; a value
// under a listed name, on a route its contract names, is not. Go's
// TestARequestsValueIsACredentialUnlessItsNameIsPublic.
test("a request's value is a credential unless its name is public", () => {
  const got = requestCredentials(
    "https://broker.example/v1/oauth/token?scope=public-scope&new_query=unlisted-query-RDX",
    {
      method: "POST",
      headers: {
        "content-type": "application/x-www-form-urlencoded",
        "user-agent": "public-agent",
        "x-new-header": "unlisted-header-RDX",
      },
      body: new URLSearchParams({
        session: "public-session-name",
        a_new_parameter: "unlisted-value-RDX",
      }),
    }
  );
  for (const want of ["unlisted-value-RDX", "unlisted-query-RDX", "unlisted-header-RDX"]) {
    expect(got).toContain(want);
  }
  for (const value of ["public-session-name", "public-scope", "public-agent"]) {
    expect(got).not.toContain(value);
  }
});
