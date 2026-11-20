// The answers Dispatch's routes declare, read from the OpenAPI document the server generates
// (`packages/contracts/schemas/dispatch-openapi.json`, which `envoy-dispatch openapi --check`
// keeps equal to the Go route table). `instance` writes the least or the most answer a schema
// admits, which `src/__tests__/dispatch-write-rows.test.ts` drives every keyed write with.
// `routeAnswer` is the answer a test's fake Dispatch serves for a keyed write: its route's least
// answer with the members the test names, held to the route's schema, so a stub answers what
// Dispatch can answer and fails the test that serves it on the commit that changes the route's
// answer. The other packages' tests take it as `@legion/envoy-client/test/route-answers`.
import { readFileSync } from "node:fs";
import { Ajv2020 } from "ajv/dist/2020";

export type Schema = { readonly [member: string]: unknown };

interface OpenApiDocument {
  readonly components: { readonly schemas: Readonly<Record<string, Schema>> };
  readonly paths: Readonly<
    Record<
      string,
      Readonly<
        Record<
          string,
          { readonly responses: Readonly<Record<string, Schema>>; readonly requestBody?: Schema }
        >
      >
    >
  >;
}

const document = JSON.parse(
  readFileSync(new URL("../../contracts/schemas/dispatch-openapi.json", import.meta.url), "utf8")
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
 *  list empty) or the most one (every member present, none null, every list holding one item).
 *  An instance is one answer, so it refuses a union of two branches that are not null: the
 *  table would try one of them alone, and `answersOf` splits an answer's top into its branches. */
export function instance(raw: Schema, extent: Extent, depth = 0): unknown {
  const schema = resolved(raw);
  if ("const" in schema) return schema.const;
  const branches = (schema.anyOf ?? schema.oneOf) as Schema[] | undefined;
  if (branches !== undefined) {
    if (extent === "least" && branches.some((branch) => resolved(branch).type === "null")) {
      return null;
    }
    const values = branchesOf(schema);
    if (values.length > 1) {
      throw new Error(
        `a union of ${values.length} branches below an answer's top: an instance writes one of them, and answersOf splits only the top`
      );
    }
    return values[0] === undefined ? null : instance(values[0], extent, depth);
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

/**
 * The route a request of `method` to `path` (its pathname) is on, as the document writes it
 * (`"POST /api/v1/issues/{key}/asks"`), or undefined where the document holds none. Where two
 * templates match, the one with more literal segments is the route, as the server's mux picks it.
 */
export function routeOf(method: string, path: string): string | undefined {
  const literal = (template: string) =>
    template.split("/").filter((segment) => !segment.startsWith("{")).length;
  const templates = Object.entries(document.paths)
    .filter(
      ([template, operations]) => onRoute(template, path) && method.toLowerCase() in operations
    )
    .map(([template]) => template)
    .sort((a, b) => literal(b) - literal(a));
  return templates[0] === undefined ? undefined : `${method.toUpperCase()} ${templates[0]}`;
}

/**
 * The status and the body a stand-in of Dispatch answers a keyed write of `method` to `path` with,
 * where it has no answer of its own for that write: the route's first declared 2xx and its least
 * answer (`instance`, an object or a list as the route declares), so the write reads its row as it
 * reads Dispatch's. Undefined where the document holds no such route.
 */
export function leastAnswerFor(
  method: string,
  path: string
): { readonly status: number; readonly body: unknown } | undefined {
  const route = routeOf(method, path);
  if (route === undefined) return undefined;
  const first = answersOf(route)[0];
  if (first === undefined) return undefined;
  return { status: first.status, body: instance(first.schema, "least") };
}

/** Each way `body` is not an answer `route` declares for `status`, as Ajv words it; none for an
 *  answer it declares. */
export function answerErrors(route: string, status: number, body: unknown): string[] {
  // Throws first, naming the route and the status, for an operation that declares no JSON body.
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

/** The branches `raw` stands for once null is set aside: each branch of its union that is not
 *  null (a union inside a branch read the same way), or `raw` itself. */
function branchesOf(raw: Schema): Schema[] {
  const schema = resolved(raw);
  const branches = (schema.anyOf ?? schema.oneOf) as Schema[] | undefined;
  if (branches === undefined) return [schema];
  return branches.filter((branch) => resolved(branch).type !== "null").flatMap(branchesOf);
}

/** The object schemas one branch stands for: the branch, or each part of its `allOf`. A member is
 *  declared by one of them. */
function objectParts(branch: Schema | undefined): Schema[] {
  if (branch === undefined) return [];
  const schema = resolved(branch);
  if (Array.isArray(schema.allOf)) return (schema.allOf as Schema[]).flatMap(objectParts);
  return [schema];
}

/** The schema one of `parts` declares the member `name` with, or undefined. */
function declaredIn(parts: readonly Schema[], name: string): Schema | undefined {
  return parts
    .map((part) => (part.properties as Record<string, Schema> | undefined)?.[name])
    .find((declared) => declared !== undefined);
}

/** The branch of `raw` that `value` is read against, since an answer is one branch of a union and
 *  not members of two: the first branch that declares each member `value` holds, else the one
 *  that declares the most of its own members (the first of those), so a refusal names what the
 *  nearest branch does not declare. */
function branchFor(raw: Schema, value: unknown): Schema | undefined {
  const branches = branchesOf(raw);
  if (branches.length <= 1) return branches[0];
  const fits = branches.find((branch) => undeclaredIn(branch, value, "") === undefined);
  if (fits !== undefined) return fits;
  const own =
    typeof value === "object" && value !== null && !Array.isArray(value) ? Object.keys(value) : [];
  const declared = branches.map((branch) => {
    const parts = objectParts(branch);
    return own.filter((name) => declaredIn(parts, name) !== undefined).length;
  });
  return branches[declared.indexOf(Math.max(...declared))];
}

/** The least instance of the object `raw` declares, with `members` laid over it: a member that
 *  is an object into the least instance of the object that member declares (also where the least
 *  answer holds null for it), any other value in its place. Of a union, the branch `members` are
 *  read against (`branchFor`). */
function laidOver(raw: Schema, members: unknown): unknown {
  if (typeof members !== "object" || members === null || Array.isArray(members)) return members;
  const parts = objectParts(branchFor(raw, members));
  const out: Record<string, unknown> = Object.assign(
    {},
    ...parts.map((part) => instance({ ...part, type: "object" }, "least") as object)
  );
  for (const [name, value] of Object.entries(members)) {
    const member = declaredIn(parts, name);
    out[name] = member === undefined ? value : laidOver(member, value);
  }
  return out;
}

/** The first member `value` holds, at any depth, that `raw` does not declare, as a dotted path; or
 *  undefined. An object schema that names no properties, or takes others (`additionalProperties`),
 *  declares every member, and each item of a list is read against the list's items. Of a union,
 *  `value` is read against one branch (`branchFor`), so a member that only another branch
 *  declares is undeclared. */
function undeclaredMember(raw: Schema, value: unknown, at: string): string | undefined {
  return undeclaredIn(branchFor(raw, value), value, at);
}

/** `undeclaredMember` within one branch. */
function undeclaredIn(branch: Schema | undefined, value: unknown, at: string): string | undefined {
  const parts = objectParts(branch);
  if (Array.isArray(value)) {
    const items = parts.find((part) => part.items !== undefined)?.items as Schema | undefined;
    if (items === undefined) return undefined;
    for (const [index, item] of value.entries()) {
      const found = undeclaredMember(items, item, `${at}.${index}`);
      if (found !== undefined) return found;
    }
    return undefined;
  }
  if (typeof value !== "object" || value === null) return undefined;
  const open = parts.some(
    (part) =>
      part.properties === undefined ||
      (part.additionalProperties !== undefined && part.additionalProperties !== false)
  );
  if (open) return undefined;
  for (const [name, member] of Object.entries(value)) {
    const path = at === "" ? name : `${at}.${name}`;
    const declared = declaredIn(parts, name);
    if (declared === undefined) return path;
    const found = undeclaredMember(declared, member, path);
    if (found !== undefined) return found;
  }
  return undefined;
}

/**
 * The answer of `route` that a test's fake Dispatch serves for a keyed write: the least answer the
 * route declares for `status` (its one 2xx when it declares one), with `members` laid over it. It
 * throws when a member is one the route does not declare (no object schema of the document is
 * closed, so the schema check alone would pass it), and when the result is not an answer the
 * route declares, naming each way it is not, so a stub cannot hold an answer Dispatch cannot give.
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
  const schema = answerSchema(route, declared);
  const stray = undeclaredMember(schema, members, "");
  if (stray !== undefined) {
    throw new Error(`a stub's answer to ${route} names ${stray}, which the route does not declare`);
  }
  const answer = laidOver(schema, members);
  const errors = answerErrors(route, declared, answer);
  if (errors.length > 0) {
    throw new Error(
      `a stub's ${declared} answer to ${route} is not one it declares: ${errors.join("; ")}`
    );
  }
  return answer as T;
}
