import { describe, expect, test } from "bun:test";

import document from "../schemas/dispatch-openapi.json";
import {
  IssueEventPayloadSchema,
  IssueSchema,
  WrittenIssueEventPayloadSchema,
} from "./dispatch-api";

/**
 * The priority an issue takes from the issues above it, as `@legion/contracts` carries it
 * (dispatch://INFRA-5793, J1 and J2).
 *
 * An issue with no priority of its own takes the priority of the nearest issue above it that has
 * one. Every issue read, and the issue of every `issue.*` event, carries that priority as
 * `resolved_priority` (0 to 3, or null) and the key of the issue it comes from as `priority_from`
 * (null where the priority is the issue's own, or where there is none), beside `priority`, which
 * stays the issue's own. The server writes both on every body, null where there is none, so the
 * OpenAPI document marks them required and nullable. The schemas here take them as optional: a
 * Dispatch older than the walk sends neither, and its answers and events still parse.
 *
 * A zod object drops every key it does not declare, so a reader that parsed the new fields without
 * declaring them would succeed and hand its caller nothing. Each row therefore reads what the parse
 * answers, not only whether it succeeded.
 *
 * The second half reads the committed document's descriptions of the two issue writes, which say
 * when a move, a clear or a new child of a P0 issue needs the p0 scope. It reads them sentence by
 * sentence, for the words the route index test reads in the same text
 * (`TestTheRouteIndexSaysWhenAMoveAClearOrANewChildNeedsP0` in
 * `packages/envoy/internal/dispatch/api/issue_resolved_priority_test.go`), since the route index
 * and the document are both written from the one route declaration.
 */

/** ACME-2 as an issue read answers it: every field `IssueSchema` requires, under ACME-1, with no
 *  priority of its own and none of the two fields a Dispatch older than the walk does not send. */
const ISSUE = {
  key: "ACME-2",
  project: "ACME",
  number: 2,
  title: "Wire the importer",
  status: "todo",
  priority: null,
  rank: "0|hzzzzz:",
  labels: [],
  parent: "ACME-1",
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  external_links: [],
  route: null,
  repo: null,
  resolved_repo: null,
  created_by: null,
  created_at: "2026-11-27T00:00:00Z",
  updated_at: "2026-11-27T00:00:00Z",
  closed_at: null,
  primary_artifact_id: "artifact-2",
  last_seq: 3,
} as const;

/** The four bodies a reader meets, each as the fields it sets on ACME-2: the three readings a
 *  current Dispatch writes (a priority taken from ACME-1, a priority of the issue's own, and no
 *  priority anywhere on the chain), and an older Dispatch's, which sends neither field. */
const READINGS = [
  ["a priority it takes from ACME-1", { resolved_priority: 0, priority_from: "ACME-1" }],
  ["a priority of its own", { priority: 2, resolved_priority: 2, priority_from: null }],
  ["no priority on its chain", { resolved_priority: null, priority_from: null }],
  ["neither field, as a Dispatch older than the walk sends it", {}],
] as const;

/** What a parse answered, as a row reads it: the problems zod named, and the value it gave. */
function parsed(schema: { safeParse(input: unknown): unknown }, input: unknown) {
  const answer = schema.safeParse(input) as {
    readonly data?: unknown;
    readonly error?: { readonly issues: readonly unknown[] };
  };
  return { problems: answer.error?.issues ?? [], data: answer.data };
}

describe("the contract's issue schemas carry the priority an issue takes", () => {
  for (const [name, schema] of [
    ["IssueSchema", IssueSchema],
    ["WrittenIssueEventPayloadSchema", WrittenIssueEventPayloadSchema],
  ] as const) {
    test(`${name} reads each reading of an issue's priority and keeps both fields`, () => {
      for (const [reading, fields] of READINGS) {
        const body = { ...ISSUE, ...fields };

        const { problems, data } = parsed(schema, body);

        expect(problems, `${name} refuses an issue with ${reading}`).toEqual([]);
        expect(data, `${name} reads an issue with ${reading} as it came`).toEqual(body);
      }
    });
  }

  test("the bus's reading of an issue.* payload keeps both fields, and still reads one without them", () => {
    // A J3 event: ACME-2's resolved priority moved because a write reached ACME-1, so `changed`
    // names the resolved fields alone.
    const moved = {
      title: "Wire the importer",
      status: "todo",
      priority: null,
      resolved_priority: 0,
      priority_from: "ACME-1",
      changed: ["resolved_priority", "priority_from"],
    };
    const older = {
      title: "Wire the importer",
      status: "todo",
      priority: 1,
      changed: ["priority"],
    };

    expect(
      parsed(IssueEventPayloadSchema, moved).data,
      "IssueEventPayloadSchema keeps resolved_priority and priority_from rather than dropping them"
    ).toEqual(moved);
    expect(
      parsed(IssueEventPayloadSchema, older).data,
      "IssueEventPayloadSchema reads an older Dispatch's payload, which carries neither field"
    ).toEqual(older);
  });
});

/** The document's response shapes, read once by name. */
const SCHEMAS: Record<string, Record<string, unknown>> = document.components.schemas;

/** The types one declared property takes: its own `type`, a string or a list, and the type of
 *  each member of an `anyOf` or a `oneOf` it is built from. */
function typesOf(property: unknown): string[] {
  if (property === null || typeof property !== "object") return [];
  const types: string[] = [];
  if ("type" in property) {
    const { type } = property;
    if (typeof type === "string") types.push(type);
    if (Array.isArray(type)) types.push(...type.filter((item) => typeof item === "string"));
  }
  for (const key of ["anyOf", "oneOf"] as const) {
    const members = key in property ? (property as Record<string, unknown>)[key] : undefined;
    if (Array.isArray(members)) types.push(...members.flatMap(typesOf));
  }
  return [...new Set(types)].sort();
}

/** One declared shape's property, or undefined where the shape declares none of that name. */
function propertyOf(schema: string, field: string): unknown {
  const properties = SCHEMAS[schema]?.properties;
  if (properties === null || typeof properties !== "object") return undefined;
  return (properties as Record<string, unknown>)[field];
}

/** The fields one declared shape requires. */
function requiredOf(schema: string): string[] {
  const required = SCHEMAS[schema]?.required;
  return Array.isArray(required) ? required.filter((item) => typeof item === "string") : [];
}

describe("the committed OpenAPI document declares the priority an issue takes", () => {
  // The issue read and the listing's row: the two shapes a reader of an issue meets first. Every
  // other response shape that carries a priority is held to the same rule by the server's own
  // inventory (`TestEveryIssueRowShapeCarriesTheResolvedPriority`).
  test.each([
    ["Issue", "resolved_priority", "integer"],
    ["Issue", "priority_from", "string"],
    ["IssueSummary", "resolved_priority", "integer"],
    ["IssueSummary", "priority_from", "string"],
  ] as const)("%s declares %s, required, as a nullable %s", (schema, field, type) => {
    const property = propertyOf(schema, field);

    expect(property, `the committed document's ${schema} declares ${field}`).toBeDefined();
    expect(typesOf(property), `${schema}.${field} is a ${type} or null`).toEqual(
      [type, "null"].sort()
    );
    expect(
      requiredOf(schema),
      `${schema} requires ${field}: the server writes it on every body, null where there is none`
    ).toContain(field);
  });
});

// The words a sentence of a route's description is read for: the route index test's own.
const MOVE = /\bmov(?:e|es|ing)\b/i;
const PARENT = /\bparent\b/i;
const CLEAR = /\bclear(?:s|ing|ed)?\b/i;
const NEEDS_P0 = /\bneeds? (?:the )?p0\b/i;
const NO_PRIORITY = /\bno priority\b/i;
const READS_P0 = /\bP0\b|\b[Pp]riority (?:of )?0\b/;
const ARCHITECT = /\barchitect\b/i;

/** The document's operations, read once by path. */
const PATHS: Record<string, Record<string, unknown>> = document.paths;

/** The description the committed document gives one operation, or "" where it gives none. */
function descriptionOf(method: string, path: string): string {
  const operation = PATHS[path]?.[method.toLowerCase()];
  if (operation === null || typeof operation !== "object" || !("description" in operation)) {
    return "";
  }
  return typeof operation.description === "string" ? operation.description : "";
}

describe("the committed OpenAPI document says when a move, a clear or a new child needs p0", () => {
  test.each([
    ["PATCH", "/api/v1/issues/{key}", "a move under a parent needs p0", [MOVE, PARENT, NEEDS_P0]],
    ["PATCH", "/api/v1/issues/{key}", "a clear needs p0", [CLEAR, NEEDS_P0]],
    [
      "POST",
      "/api/v1/issues",
      "a new issue with no priority under a parent that reads P0 needs p0 unless the parent's architect files it",
      [NO_PRIORITY, PARENT, READS_P0, NEEDS_P0, ARCHITECT],
    ],
  ] as const)("%s %s's description says that %s", (method, path, says, words) => {
    const description = descriptionOf(method, path);
    expect(description, `the committed document describes ${method} ${path}`).not.toBe("");
    // A sentence is the text cut at each full stop, as the route index test cuts it.
    const sentences = description.split(/[.!?](?:\s+|$)/);

    const saying = sentences.filter((sentence) => words.every((word) => word.test(sentence)));

    // The sentences that say a write needs p0 are what a reader of the failure compares, so they
    // are printed rather than the whole description.
    const needingP0 = sentences.filter((sentence) => NEEDS_P0.test(sentence));
    expect(
      saying.length,
      `${method} ${path} has no sentence that says ${says} (one matching each of ` +
        `${words.map(String).join(", ")}); its sentences that say a write needs p0: ` +
        JSON.stringify(needingP0)
    ).toBeGreaterThan(0);
  });
});
