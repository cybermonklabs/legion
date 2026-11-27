import { expect, test } from "bun:test";

import document from "../schemas/dispatch-openapi.json";
import { ActorActSchema, DISPATCH_SCOPES, DISPATCH_TOKEN_SCOPES } from "./dispatch-api";

// The scope `p0` is what a priority of 0 needs: only a credential that holds it may make an
// issue's own priority P0, a person at a browser holds it, and a personal token holds it only
// where its maker gave it. The contract's lists name it beside the scopes a credential already
// holds, an act that a credential holding it made records it, and the document every client is
// typed from says on both issue writes that a priority of 0 needs it.

test("DISPATCH_SCOPES and DISPATCH_TOKEN_SCOPES name p0", () => {
  expect([...DISPATCH_SCOPES]).toEqual(["person", "approve", "p0"]);
  expect([...DISPATCH_TOKEN_SCOPES]).toEqual(["person", "approve", "p0", "read"]);
});

// An act records the credential's whole set (`actScopes` in the Go server), so the act of a token
// holding p0 carries p0, and the schema every bus reader of an act parses must take it.
test("ActorActSchema reads an act that records p0", () => {
  expect(ActorActSchema.parse({ scopes: ["person", "approve", "p0"] })).toEqual({
    scopes: ["person", "approve", "p0"],
  });
});

/** A sentence that names a priority of 0, and a sentence that names the p0 scope, as the route
 *  index's own test reads them (`TestTheRouteIndexSaysAPriorityOf0NeedsP0`, in
 *  packages/envoy/internal/dispatch/api/issue_p0_scope_test.go). */
const NAMES_PRIORITY_0 = /\bpriority (of )?`?0\b/i;
const NAMES_P0 = /\bp0\b/;

/** Where one sentence of a description ends and the next begins. */
const SENTENCE_END = /[.!?](?:\s+|$)/;

// The document is emitted from the Go route table, so this holds the table's own sentence as every
// client of the document reads it: the descriptions of the two writes that take an issue's
// priority each say, in one sentence, that a priority of 0 needs p0.
test("the OpenAPI document says a priority of 0 needs p0 on both issue writes", () => {
  const writes = [
    ["POST /api/v1/issues", document.paths["/api/v1/issues"].post.description],
    ["PATCH /api/v1/issues/{key}", document.paths["/api/v1/issues/{key}"].patch.description],
  ] as const;

  for (const [route, description] of writes) {
    const says = description
      .split(SENTENCE_END)
      .some((sentence) => NAMES_PRIORITY_0.test(sentence) && NAMES_P0.test(sentence));
    expect(
      says,
      `the description of ${route} has no sentence that names a priority of 0 and p0: ${description}`
    ).toBe(true);
  }
});
