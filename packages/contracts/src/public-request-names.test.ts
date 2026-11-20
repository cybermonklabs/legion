import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { PUBLIC_REQUEST_NAMES } from "./public-request-names";
import { requestCredentials } from "./remote-text";

// The names a credential travels under, which Go's public_names_test.go reads too.
const FORBIDDEN_PATH = new URL(
  "../../envoy/internal/redact/testdata/credential-names.json",
  import.meta.url
);
const forbidden: { names: string[]; headerPatterns: string[] } = JSON.parse(
  readFileSync(FORBIDDEN_PATH, "utf8")
);

// Every entry names the contract that declares its value public, and none is a name a credential
// travels under. Go's TestThePublicNameTableHoldsOnlyContractNamesNoCredentialTravelsUnder.
test("the public-name table holds only contract names no credential travels under", () => {
  expect(forbidden.names.length).toBeGreaterThan(0);
  expect(PUBLIC_REQUEST_NAMES.length).toBeGreaterThan(0);
  const patterns = forbidden.headerPatterns.map((pattern) => new RegExp(pattern, "i"));
  for (const entry of PUBLIC_REQUEST_NAMES) {
    expect(entry.source.trim(), `${entry.name} names no contract`).not.toBe("");
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
});

// A value under a name no contract declares public, a new one included, is a credential; a value
// under a listed name is not. Go's TestARequestsValueIsACredentialUnlessItsNameIsPublic.
test("a request's value is a credential unless its name is public", () => {
  const got = requestCredentials(
    "https://broker.example/token?scope=public-scope&new_query=unlisted-query-RDX",
    {
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
