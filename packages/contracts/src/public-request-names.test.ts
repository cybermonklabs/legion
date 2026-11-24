import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { PROTOCOL_METHODS, PUBLIC_REQUEST_NAMES } from "./public-request-names";

// The names a credential travels under, which Go's public_names_test.go reads too.
const FORBIDDEN_PATH = new URL(
  "../../envoy/internal/redact/testdata/credential-names.json",
  import.meta.url
);
const forbidden: { names: string[]; headerPatterns: string[] } = JSON.parse(
  readFileSync(FORBIDDEN_PATH, "utf8")
);

// The planted entries the source check refuses or passes, which Go's public_names_test.go reads
// too.
const SOURCES_PATH = new URL(
  "../../envoy/internal/redact/testdata/public-name-sources.json",
  import.meta.url
);
const sources = z
  .strictObject({
    about: z.string(),
    rows: z
      .array(
        z.strictObject({
          id: z.string().min(1),
          source: z.string(),
          route: z.string().min(1),
          refused: z.boolean(),
        })
      )
      .min(1),
  })
  .parse(JSON.parse(readFileSync(SOURCES_PATH, "utf8")));

/** An entry's route: an upper-case method, one space and a path from its root. */
const ROUTE_SHAPE = /^[A-Z]+ \/[^ ]*$/;

/** A route on every path of its method. */
const EVERY_PATH = /^[A-Z]+ \/\*$/;

/** Why an entry's `route` is no route its `source` names, or "" where it is one: a route on every
 *  path of its method is one a protocol of PROTOCOL_METHODS fixes, whose section the source cites,
 *  since no contract writes `/*` and a source that repeats the route names nothing; any other route
 *  is one the source names in its own words. Go's `routeProblem`. */
function routeProblem(source: string, route: string): string {
  if (!ROUTE_SHAPE.test(route)) return 'is no "<METHOD> /<path>"';
  if (EVERY_PATH.test(route)) {
    return PROTOCOL_METHODS.some(
      (protocol) => protocol.route === route && source.includes(protocol.source)
    )
      ? ""
      : "is every path of its method, which no protocol its source cites fixes (PROTOCOL_METHODS)";
  }
  return source.includes(route) ? "" : "is a route its source does not name";
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
      expect(routeProblem(entry.source, route), `${entry.name} is public on ${route}`).toBe("");
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

// Each planted route of testdata/public-name-sources.json is refused or passed as the row says: a
// route on every path of its method passes only from a source that cites a protocol fixing that
// method, never from one that only repeats the route; any other route only from a source that
// names it. Go's TestARouteOnEveryPathIsOneAProtocolItsSourceCitesFixes.
test("a route on every path is one a protocol its source cites fixes", () => {
  for (const { id, source, route, refused } of sources.rows) {
    expect(routeProblem(source, route) !== "", id).toBe(refused);
  }
});
