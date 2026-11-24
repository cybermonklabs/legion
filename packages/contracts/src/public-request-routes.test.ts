import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { requestCredentials } from "./remote-text";

// The TypeScript half of one rule: this replays
// packages/envoy/internal/redact/testdata/public-routes.json, that file itself read by relative
// path and never a copy, through requestCredentials, as Go's public_routes_test.go replays it
// through an answer's words: a value under a name the public-name table lists is public on a route
// its contract names, and every other value is a credential like any other.
const TABLE_PATH = new URL(
  "../../envoy/internal/redact/testdata/public-routes.json",
  import.meta.url
);

/** One value a request carries under a name, sent with `method` to `target`, in its query, its
 *  form, its JSON body (a string, or a list of one string for `json-list`) or a header; `on` says
 *  whether the value is public there, its name listed on a route its contract names. */
const RoutedValue = z.strictObject({
  method: z.string(),
  target: z.string().min(1),
  at: z.enum(["query", "form", "json", "json-list", "header"]),
  name: z.string().min(1),
  value: z.string().min(1),
  on: z.boolean(),
});

const Table = z.strictObject({
  about: z.string(),
  groups: z
    .array(
      z.strictObject({
        id: z.string().min(1),
        about: z.string(),
        rows: z.array(RoutedValue).min(1),
      })
    )
    .min(1),
});

const { groups } = Table.parse(JSON.parse(readFileSync(TABLE_PATH, "utf8")));

/** Requires each of `values` to be left out of its request's credentials where it is public and
 *  to be among them anywhere else. */
function requireRoutes(values: readonly z.infer<typeof RoutedValue>[]): void {
  for (const { method, target, at, name, value, on } of values) {
    let url = target;
    const headers: Record<string, string> = {};
    let body: unknown;
    if (at === "query") {
      url += `?${new URLSearchParams({ [name]: value })}`;
    } else if (at === "form") {
      headers["content-type"] = "application/x-www-form-urlencoded";
      body = new URLSearchParams({ [name]: value });
    } else if (at === "header") {
      headers[name] = value;
    } else {
      headers["content-type"] = "application/json";
      body = JSON.stringify({ [name]: at === "json-list" ? [value] : value });
    }
    const credentials = requestCredentials(url, { method, headers, body });
    expect(
      credentials.includes(value),
      `${method} ${target} ${name}: ${on ? "public" : "a credential"} there`
    ).toBe(!on);
  }
}

test("the shared public routes table names each group once", () => {
  const ids = groups.map((group) => group.id);
  expect(new Set(ids).size).toBe(ids.length);
});

for (const group of groups) {
  test(`public routes table group ${group.id} reads as the table says`, () => {
    requireRoutes(group.rows);
  });
}
