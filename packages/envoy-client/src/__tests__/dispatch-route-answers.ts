// The answers Dispatch's routes declare, read from the OpenAPI document the server generates
// (`packages/contracts/schemas/dispatch-openapi.json`, which `envoy-dispatch openapi --check`
// keeps equal to the Go route table). `instance` writes the least or the most answer a schema
// admits, which `dispatch-write-rows.test.ts` drives every keyed write with. `routeAnswer` is the
// answer a test's fake Dispatch serves for a keyed write: its route's least answer with the members
// the test names, held to the route's schema, so a stub answers what Dispatch can answer and fails
// the test that serves it on the commit that changes the route's answer.
import { readFileSync } from "node:fs";
import { Ajv2020 } from "ajv/dist/2020";

export type Schema = { readonly [member: string]: unknown };

interface OpenApiDocument {
  readonly components: { readonly schemas: Readonly<Record<string, Schema>> };
  readonly paths: Readonly<
    Record<
      string,
      Readonly<Record<string, { readonly responses: Readonly<Record<string, Schema>> }>>
    >
  >;
}

const document = JSON.parse(
  readFileSync(new URL("../../../contracts/schemas/dispatch-openapi.json", import.meta.url), "utf8")
) as OpenApiDocument;

// The schema is reached by a JSON pointer into the document, as `stub-recordings.test.ts` of
// `packages/dispatch` reaches it, so every `$ref` resolves into the document's components.
const ajv = new Ajv2020({ allErrors: true, strict: false, validateFormats: false });
ajv.addSchema({ $id: "dispatch", ...document });

/** `schema` with its `$ref` followed into the document's components. */
function resolved(schema: Schema): Schema {
  const ref = schema.$ref;
  if (typeof ref !== "string") return schema;
  const name = ref.replace("#/components/schemas/", "");
  const target = document.components.schemas[name];
  if (target === undefined) throw new Error(`the document holds no schema ${ref}`);
  return resolved(target);
}

export type Extent = "least" | "most";

/** Past this depth the most instance stops adding optional members, so a schema that holds
 *  itself (a display tree's children) ends. */
const MOST_DEPTH = 6;

/** A value of a string schema: its first enum value, else one of its format. */
function stringOf(schema: Schema): string {
  if (Array.isArray(schema.enum)) return String(schema.enum[0]);
  switch (schema.format) {
    case "date-time":
      return "2026-11-17T01:00:00Z";
    case "uuid":
      return "00000000-0000-4000-8000-000000000001";
    case "email":
      return "ada@example.com";
    case "uri":
      return "https://example.org/";
    default:
      return "x".repeat(typeof schema.minLength === "number" ? Math.max(schema.minLength, 1) : 1);
  }
}

/** The least instance of `raw` (every optional member absent, every nullable member null, every
 *  list empty) or the most one (every member present, none null, every list holding one item). */
export function instance(raw: Schema, extent: Extent, depth = 0): unknown {
  const schema = resolved(raw);
  if ("const" in schema) return schema.const;
  const branches = (schema.anyOf ?? schema.oneOf) as Schema[] | undefined;
  if (branches !== undefined) {
    const isNull = (branch: Schema) => resolved(branch).type === "null";
    if (extent === "least" && branches.some(isNull)) return null;
    const first = branches.find((branch) => !isNull(branch));
    if (first === undefined) return null;
    return instance(first, extent, depth);
  }
  if (Array.isArray(schema.allOf)) {
    return Object.assign(
      {},
      ...(schema.allOf as Schema[]).map((part) => instance(part, extent, depth) as object)
    );
  }
  const types = Array.isArray(schema.type) ? (schema.type as string[]) : [schema.type];
  if (extent === "least" && types.includes("null")) return null;
  const type = types.find((name) => name !== "null") ?? (schema.properties ? "object" : undefined);
  switch (type) {
    case "object": {
      const required = new Set((schema.required as string[] | undefined) ?? []);
      const properties = (schema.properties ?? {}) as Record<string, Schema>;
      const out: Record<string, unknown> = {};
      for (const [name, member] of Object.entries(properties)) {
        const wanted = required.has(name) || (extent === "most" && depth < MOST_DEPTH);
        if (wanted) out[name] = instance(member, required.has(name) ? extent : "most", depth + 1);
      }
      if (extent === "most" && typeof schema.additionalProperties === "object") {
        out.k = instance(schema.additionalProperties as Schema, extent, depth + 1);
      }
      return out;
    }
    case "array":
      return extent === "least" || depth >= MOST_DEPTH
        ? []
        : [instance((schema.items ?? {}) as Schema, extent, depth + 1)];
    case "string":
      return stringOf(schema);
    case "integer":
    case "number": {
      const minimum = typeof schema.minimum === "number" ? schema.minimum : 1;
      return typeof schema.exclusiveMinimum === "number" ? schema.exclusiveMinimum + 1 : minimum;
    }
    case "boolean":
      return extent === "most";
    default:
      return {};
  }
}

/** The operation of `route` (`"POST /api/v1/issues/{key}/comments"`, as the document writes it). */
function operationOf(route: string) {
  const [method, path] = route.split(" ") as [string, string];
  const operation = document.paths[path]?.[method.toLowerCase()];
  if (operation === undefined) throw new Error(`the document holds no operation ${route}`);
  return { method: method.toLowerCase(), path, operation };
}

/** The JSON schema `route` declares for its `status`. */
function answerSchema(route: string, status: number): Schema {
  const answer = operationOf(route).operation.responses[String(status)];
  const schema = (answer?.content as Record<string, { schema: Schema }> | undefined)?.[
    "application/json"
  ]?.schema;
  if (schema === undefined) throw new Error(`${route} declares no JSON body for ${status}`);
  return schema;
}

/** Each answer the operation declares for a 2xx: the status, and each top-level branch of its
 *  schema apart (a reply route answers a reply or the delivery it settled). */
export function answersOf(route: string): { status: number; branch: number; schema: Schema }[] {
  const out: { status: number; branch: number; schema: Schema }[] = [];
  const declared = Object.keys(operationOf(route).operation.responses).filter((status) =>
    status.startsWith("2")
  );
  for (const status of declared.map(Number)) {
    const schema = answerSchema(route, status);
    const top = resolved(schema);
    const branches = (top.anyOf ?? top.oneOf) as Schema[] | undefined;
    for (const [branch, part] of (branches ?? [schema]).entries()) {
      out.push({ status, branch, schema: part });
    }
  }
  return out;
}

/** Whether `path` is an instance of the route template `template`. */
export function onRoute(template: string, path: string): boolean {
  const wanted = template.split("/");
  const got = path.split("/");
  return (
    wanted.length === got.length &&
    wanted.every((segment, index) => segment.startsWith("{") || segment === got[index])
  );
}

/** Each way `body` is not an answer `route` declares for `status`, as Ajv words it; none for an
 *  answer it declares. */
export function answerErrors(route: string, status: number, body: unknown): string[] {
  answerSchema(route, status);
  const { method, path } = operationOf(route);
  // A JSON pointer's step escapes `~` and `/` (RFC 6901).
  const pointer = path.replaceAll("~", "~0").replaceAll("/", "~1");
  const validate = ajv.getSchema(
    `dispatch#/paths/${pointer}/${method}/responses/${status}/content/application~1json/schema`
  );
  if (validate === undefined) throw new Error(`Ajv holds no schema of ${route} ${status}`);
  if (validate(body)) return [];
  return (validate.errors ?? []).map((error) => `${error.instancePath || "/"} ${error.message}`);
}

/** The object schemas `raw` stands for once null is set aside: its first branch that is not null,
 *  or each part of an `allOf`. A member is declared by one of them. */
function objectParts(raw: Schema): Schema[] {
  const schema = resolved(raw);
  const branches = (schema.anyOf ?? schema.oneOf) as Schema[] | undefined;
  if (branches !== undefined) {
    const first = branches.find((branch) => resolved(branch).type !== "null");
    return first === undefined ? [] : objectParts(first);
  }
  if (Array.isArray(schema.allOf)) return (schema.allOf as Schema[]).flatMap(objectParts);
  return [schema];
}

/** The least instance of the object `raw` declares, with `members` laid over it: a member that
 *  is an object into the least instance of the object that member declares (also where the least
 *  answer holds null for it), any other value in its place. */
function laidOver(raw: Schema, members: unknown): unknown {
  if (typeof members !== "object" || members === null || Array.isArray(members)) return members;
  const parts = objectParts(raw);
  const out: Record<string, unknown> = Object.assign(
    {},
    ...parts.map((part) => instance({ ...part, type: "object" }, "least") as object)
  );
  for (const [name, value] of Object.entries(members)) {
    const member = parts
      .map((part) => (part.properties as Record<string, Schema> | undefined)?.[name])
      .find((declared) => declared !== undefined);
    out[name] = member === undefined ? value : laidOver(member, value);
  }
  return out;
}

/**
 * The answer of `route` that a test's fake Dispatch serves for a keyed write: the least answer the
 * route declares for `status` (its one 2xx when it declares one), with `members` laid over it. It
 * throws when the result is not an answer the route declares, naming each way it is not, so a stub
 * cannot hold an answer Dispatch cannot give.
 */
export function routeAnswer<T = Record<string, unknown>>(
  route: string,
  members: Record<string, unknown>,
  status?: number
): T {
  const statuses = [...new Set(answersOf(route).map((answer) => answer.status))];
  const declared = status ?? (statuses.length === 1 ? statuses[0] : undefined);
  if (declared === undefined) {
    throw new Error(
      `${route} declares the 2xx statuses ${statuses.join(", ")}: name the one served`
    );
  }
  const answer = laidOver(answerSchema(route, declared), members);
  const errors = answerErrors(route, declared, answer);
  if (errors.length > 0) {
    throw new Error(
      `a stub's ${declared} answer to ${route} is not one it declares: ${errors.join("; ")}`
    );
  }
  return answer as T;
}
