// The fields GitHub's REST API description marks required in the success answer of each REST
// operation the world serves, the fields it declares there, and the reading of an answer against
// them: rest-required.json, derived by this module's `derive` from github/rest-api-description at
// DESCRIPTION_COMMIT and never written by hand. A REST route the world serves (gh-routes.ts's
// WORLD_ROUTES, the gh stand-in's, and github-routes.ts's GITHUB_ROUTES, the HTTP stand-in
// Dispatch reads) answers every field its operation's success schema requires, as the
// description reads it: through `$ref`, every part of an `allOf`, the closest of a `oneOf` or an
// `anyOf` (the one the answer lacks the fewest fields of), and each item of an array
// (`absentFields`), the reading of INFRA-764's read search, which found the world's answers short
// of 79 fields of the repository, 37 of the pull request and 17 of every user (2026-11-17). An
// answer GitHub's production read pins no shape of is held to the description's key set besides:
// every field it answers is one the description declares there (`keyDiff`).
//
// The extract keeps, for each route, the operation it is and that operation's success status and
// media type, and the tree of its schema: each named schema it reaches once, under `schemas`, and
// each part of a schema only where something below it is required or declared, so a cycle of
// schemas (a repository's template repository) is a name and no copy. An object's `keys` are
// every field it declares, where it takes no other (no `additionalProperties`); an object that
// takes others, or declares none, has no `keys`, and any field passes there. A route's answer that
// is no JSON (a raw file, a diff, a commit's SHA under its media type) has no schema to check.
//
//   bun rest-required.ts <api.github.com.json>
//
// writes the extract from `descriptions/api.github.com/api.github.com.json` of
// github/rest-api-description at DESCRIPTION_COMMIT (fetch it with `curl -fsSLO
// https://raw.githubusercontent.com/github/rest-api-description/<commit>/descriptions/api.github.com/api.github.com.json`),
// then formats it as the repository's biome does. A route the description has no operation for
// fails the derivation, naming it, unless ANSWERED_AS names the operation GitHub answers it as.
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { WORLD_ROUTES } from "./gh-routes";
import { GITHUB_ROUTES } from "./github-routes";

/** The extract this directory keeps. */
export const REST_REQUIRED = path.join(import.meta.dir, "rest-required.json");
/** The commit of github/rest-api-description the extract is derived from. */
export const DESCRIPTION_COMMIT = "c9c95fe34ced05fe076001f5c55704742bc8e7f8";
const DESCRIPTION_FILE = "descriptions/api.github.com/api.github.com.json";

/** A schema as the extract keeps it: a named schema (`ref`), the items of an array, every part of
 * an `allOf`, the closest of a `oneOf` or an `anyOf`, or an object's required fields and the
 * fields below which something is required. `{}` requires nothing. */
export type Required =
  | { readonly ref: string }
  | { readonly items: Required }
  | { readonly allOf: readonly Required[] }
  | { readonly oneOf: readonly Required[] }
  | { readonly anyOf: readonly Required[] }
  | {
      readonly required?: readonly string[];
      readonly keys?: readonly string[];
      readonly properties?: Readonly<Record<string, Required>>;
    };
const Node: z.ZodType<Required> = z.lazy(() =>
  z.union([
    z.strictObject({ ref: z.string() }),
    z.strictObject({ items: Node }),
    z.strictObject({ allOf: z.array(Node) }),
    z.strictObject({ oneOf: z.array(Node) }),
    z.strictObject({ anyOf: z.array(Node) }),
    z.strictObject({
      required: z.array(z.string()).optional(),
      keys: z.array(z.string()).optional(),
      properties: z.record(z.string(), Node).optional(),
    }),
  ])
);
const Operation = z.strictObject({
  operation_id: z.string(),
  status: z.number(),
  media: z.string(),
  schema: Node.optional(),
});
/** One operation of the description as the extract keeps it: its id, the status of its success,
 * the media type the world answers it in, and the required tree of that answer, where it is JSON. */
export type RestOperation = z.infer<typeof Operation>;
const Extract = z.strictObject({
  source: z.string(),
  commit: z.string(),
  file: z.string(),
  routes: z.record(z.string(), z.string()),
  operations: z.record(z.string(), Operation),
  schemas: z.record(z.string(), Node),
});
type Extract = z.infer<typeof Extract>;

let extract: Extract | undefined;
function loaded(): Extract {
  extract ??= Extract.parse(JSON.parse(readFileSync(REST_REQUIRED, "utf8")));
  return extract;
}

/** The commit of the description the committed extract was derived from. */
export function extractCommit(): string {
  return loaded().commit;
}

/** The operation of the description the world's REST route `route` is (a WORLD_ROUTES key such as
 * `GET repos/{owner}/{repo}/pulls/{number}`, or a GITHUB_ROUTES one), with its key (`GET
 * /repos/{owner}/{repo}/pulls/{pull_number}`), or undefined for a route the extract does not
 * hold. */
export function restOperation(route: string): (RestOperation & { key: string }) | undefined {
  const { routes, operations } = loaded();
  const key = routes[route];
  const operation = key === undefined ? undefined : operations[key];
  return key === undefined || operation === undefined ? undefined : { ...operation, key };
}

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The paths of the fields `schema` requires that `value` lacks, as the description reads them
 * (`.field`, `[]` for an array's items): a named schema is read through its name in `schemas`
 * (the committed extract's), every part of an `allOf` is required, a `oneOf` or an `anyOf` is the
 * variant `value` lacks the fewest fields of, an array's every item is read, and an object's
 * required fields are read on an object and each field below which something is required on the
 * object or array `value` holds there. */
export function absentFields(
  value: unknown,
  schema: Required,
  schemas: Readonly<Record<string, Required>> = loaded().schemas,
  at = ""
): string[] {
  let node = schema;
  while ("ref" in node) {
    const named = schemas[node.ref];
    if (named === undefined) throw new Error(`the extract holds no schema ${node.ref}`);
    node = named;
  }
  if ("items" in node)
    return (Array.isArray(value) ? value : []).flatMap((item) =>
      absentFields(item, node.items, schemas, `${at}[]`)
    );
  if ("allOf" in node) return node.allOf.flatMap((part) => absentFields(value, part, schemas, at));
  if ("oneOf" in node || "anyOf" in node)
    return ("oneOf" in node ? node.oneOf : node.anyOf)
      .map((variant) => absentFields(value, variant, schemas, at))
      .reduce((closest, one) => (one.length < closest.length ? one : closest));
  if (!isObject(value)) return [];
  return [
    ...(node.required ?? []).filter((field) => !(field in value)).map((field) => `${at}.${field}`),
    ...Object.entries(node.properties ?? {}).flatMap(([field, below]) => {
      const held = value[field];
      return typeof held === "object" && held !== null
        ? absentFields(held, below, schemas, `${at}.${field}`)
        : [];
    }),
  ];
}

/** A schema read as an answer's one level meets it: every part of an `allOf` and every `$ref`
 * read through, as the list of the nodes that level holds, a `oneOf` or an `anyOf` left whole. */
function partsOf(schema: Required, schemas: Readonly<Record<string, Required>>): Required[] {
  let node = schema;
  while ("ref" in node) {
    const named = schemas[node.ref];
    if (named === undefined) throw new Error(`the extract holds no schema ${node.ref}`);
    node = named;
  }
  return "allOf" in node ? node.allOf.flatMap((part) => partsOf(part, schemas)) : [node];
}

/** An object's node of the extract: the fields it requires, the fields it declares and the
 * schema of each field below which something is required or declared. */
type ObjectNode = {
  readonly required?: readonly string[];
  readonly keys?: readonly string[];
  readonly properties?: Readonly<Record<string, Required>>;
};

/** The paths of the fields `value` lacks that `schema` requires (`missing`, as `absentFields`
 * reads them) and of the fields it holds that `schema` declares nowhere (`extra`): at each level,
 * the fields every part of an `allOf` declares are the level's, a `oneOf` or an `anyOf` is the
 * variant `value` differs from in the fewest fields, an array's every item is read, and an object
 * no part of which declares `keys` takes any field. Below a field, every part's schema of it is
 * read. */
export function keyDiff(
  value: unknown,
  schema: Required,
  schemas: Readonly<Record<string, Required>> = loaded().schemas,
  at = ""
): { missing: string[]; extra: string[] } {
  const join = (all: readonly { missing: string[]; extra: string[] }[]) => ({
    missing: all.flatMap((one) => one.missing),
    extra: all.flatMap((one) => one.extra),
  });
  const flat: Required[] = [];
  for (const part of partsOf(schema, schemas)) {
    if (!("oneOf" in part) && !("anyOf" in part)) {
      flat.push(part);
      continue;
    }
    const count = (variant: Required) => {
      const diff = keyDiff(value, variant, schemas, at);
      return diff.missing.length + diff.extra.length;
    };
    const variants = "oneOf" in part ? part.oneOf : part.anyOf;
    const closest = variants.reduce((best, one) => (count(one) < count(best) ? one : best));
    flat.push(...partsOf(closest, schemas));
  }
  const items = flat.flatMap((part) => ("items" in part ? [part.items] : []));
  if (items.length > 0)
    return join(
      (Array.isArray(value) ? value : []).map((item) =>
        keyDiff(item, { allOf: items }, schemas, `${at}[]`)
      )
    );
  if (!isObject(value)) return { missing: [], extra: [] };
  const objects = flat.filter((part): part is ObjectNode => !("items" in part));
  const required = [...new Set(objects.flatMap((part) => part.required ?? []))];
  const declaring = objects.filter((part) => part.keys !== undefined);
  const keys = new Set([...declaring.flatMap((part) => part.keys ?? []), ...required]);
  const below = Object.entries(value).flatMap(([field, held]) => {
    const parts = objects.flatMap((part) => {
      const one = part.properties?.[field];
      return one === undefined ? [] : [one];
    });
    return typeof held === "object" && held !== null && parts.length > 0
      ? [keyDiff(held, { allOf: parts }, schemas, `${at}.${field}`)]
      : [];
  });
  return join([
    {
      missing: required.filter((field) => !(field in value)).map((field) => `${at}.${field}`),
      extra:
        declaring.length === 0
          ? []
          : Object.keys(value)
              .filter((field) => !keys.has(field))
              .map((field) => `${at}.${field}`),
    },
    ...below,
  ]);
}

/** The REST routes the world serves: every WORLD_ROUTES route of an HTTP verb, written without
 * its leading slash, and every GITHUB_ROUTES route, written with it, with the media type the
 * world answers it in. */
export function servedRoutes(): { route: string; accept?: string }[] {
  return [
    ...WORLD_ROUTES.filter((row) => /^(GET|POST|PUT|PATCH|DELETE) /.test(row.route)).map((row) => ({
      route: row.route,
    })),
    ...GITHUB_ROUTES.map((row) => ({
      route: row.route,
      ...(row.accept === undefined ? {} : { accept: row.accept }),
    })),
  ];
}

// The description, as far as the derivation reads it.
type Json = Record<string, unknown>;
const JSON_MEDIA = "application/json";

/** The OpenAPI path of `paths` that the world's `template` is, for `method`: the same segments,
 * where a world parameter (`{name}`, `{name+}`) stands for a parameter of the description's. */
function pathOf(paths: Json, method: string, template: string): string | undefined {
  const parameter = (segment: string) => /^\{[^}]+\}$/.test(segment);
  const segments = template.replace(/^\//, "").split("/");
  const found = Object.keys(paths).filter((candidate) => {
    const item = paths[candidate];
    if (!isObject(item) || !isObject(item[method])) return false;
    const theirs = candidate.replace(/^\//, "").split("/");
    return (
      theirs.length === segments.length &&
      theirs.every((segment, index) => {
        const ours = segments[index] ?? "";
        return parameter(segment) ? parameter(ours) : segment === ours;
      })
    );
  });
  if (found.length > 1) throw new Error(`${method} ${template} is ${found.join(" and ")}`);
  return found[0];
}

/** The world's REST routes GitHub serves that its description holds no operation of, each with
 * the description's path whose operation GitHub answers it as. The description at
 * DESCRIPTION_COMMIT holds `/repos/{owner}/{repo}/git/refs` and `/git/refs/{ref}` for their writes
 * alone (POST; PATCH and DELETE), and GitHub serves both reads still, as the refs of a namespace:
 * measured on 2026-11-21 with `gh api -i` against cybermonklabs/legion, `git/refs/heads/main`, a
 * ref named whole, answered one ref (`Date: Sat, 21 Nov 2026 08:43:03 GMT`), as `git/ref/heads/main`
 * did (08:43:02 GMT), `git/refs/heads/mai`, a prefix of names, a list of the refs it starts
 * (08:43:04 GMT), `git/refs?per_page=2` a list of two (08:43:07 GMT), and
 * `git/refs/heads/nosuchbranch-xyz` a 404 whose body names "get all references in a namespace"
 * (08:43:05 GMT). So `git/refs` is the list "List matching references" answers, and
 * `git/refs/{ref}` is held to "Get a reference", the one ref a name read whole answers; its list
 * of a prefix is held to the list's operation by gh-standin.test.ts's row of the refs. */
const ANSWERED_AS: Readonly<Record<string, string>> = {
  "GET repos/{owner}/{repo}/git/refs": "/repos/{owner}/{repo}/git/matching-refs/{ref}",
  "GET repos/{owner}/{repo}/git/refs/{ref+}": "/repos/{owner}/{repo}/git/ref/{ref}",
};

/** The extract of `description` (GitHub's REST API description, parsed) for `served`. */
export function derive(description: Json, served = servedRoutes()): Extract {
  const paths = description.paths;
  const components = description.components;
  if (!isObject(paths) || !isObject(components)) throw new Error("no paths or no components");
  const named = isObject(components.schemas) ? components.schemas : {};
  const responses = isObject(components.responses) ? components.responses : {};
  const deref = (value: unknown, table: Json): unknown => {
    let at = value;
    while (isObject(at) && typeof at.$ref === "string") at = table[at.$ref.split("/").pop() ?? ""];
    return at;
  };

  // Every named schema the served operations reach, and whether anything below it is required.
  const reached = new Map<string, Json>();
  const reach = (schema: unknown): void => {
    if (!isObject(schema)) return;
    if (typeof schema.$ref === "string") {
      const name = schema.$ref.split("/").pop() ?? "";
      if (reached.has(name)) return;
      const body = named[name];
      if (!isObject(body)) throw new Error(`no schema ${name}`);
      reached.set(name, body);
      reach(body);
      return;
    }
    for (const part of [
      schema.items,
      ...(Array.isArray(schema.allOf) ? schema.allOf : []),
      ...(Array.isArray(schema.oneOf) ? schema.oneOf : []),
      ...(Array.isArray(schema.anyOf) ? schema.anyOf : []),
      ...Object.values(isObject(schema.properties) ? schema.properties : {}),
    ])
      reach(part);
  };

  const routes: Record<string, string> = {};
  const operations: Record<
    string,
    { operation_id: string; status: number; media: string; schema?: unknown }
  > = {};
  for (const { route, accept } of served) {
    const [verb = "", template = ""] = route.split(" ");
    const method = verb.toLowerCase();
    const own = pathOf(paths, method, template);
    const aliased = ANSWERED_AS[route];
    if (own !== undefined && aliased !== undefined)
      throw new Error(`the description holds ${verb} ${own}, so ${route} needs no ANSWERED_AS`);
    if (aliased !== undefined && !isObject((paths[aliased] as Json | undefined)?.[method]))
      throw new Error(`ANSWERED_AS names ${aliased} for ${route}, and it has no ${verb}`);
    const found = own ?? aliased;
    if (found === undefined) throw new Error(`no operation of the description is ${route}`);
    // One operation answered in two media types is two answers, each with its own tree: the gh
    // world reads a commit as JSON and the HTTP stand-in as its SHA, and one key for both let the
    // last route served leave the other's answer held to nothing.
    const media = accept ?? JSON_MEDIA;
    const key = media === JSON_MEDIA ? `${verb} ${found}` : `${verb} ${found} ${media}`;
    routes[route] = key;
    const operation = (paths[found] as Json)[method] as Json;
    const answered = Object.entries(isObject(operation.responses) ? operation.responses : {})
      .filter(([status]) => /^2\d\d$/.test(status))
      .sort(([one], [other]) => Number(one) - Number(other))[0];
    if (answered === undefined) throw new Error(`${key} has no success answer`);
    const response = deref(answered[1], responses);
    const content = isObject(response) && isObject(response.content) ? response.content : {};
    const body = content[media];
    const schema = isObject(body) ? body.schema : undefined;
    reach(schema);
    operations[key] = {
      operation_id: String(operation.operationId),
      status: Number(answered[0]),
      media,
      ...(schema === undefined ? {} : { schema }),
    };
  }

  // The fields an object declares where it takes no other: every field of its `properties`, unless
  // its `additionalProperties` lets it hold others.
  const declared = (schema: Json): string[] =>
    isObject(schema.properties) &&
    (schema.additionalProperties === undefined || schema.additionalProperties === false)
      ? Object.keys(schema.properties)
      : [];
  // A schema holds something where it requires a field or declares its fields, or a part of it
  // does: read to a fixed point, since schemas name each other in cycles.
  const holds = new Map<string, boolean>([...reached.keys()].map((name) => [name, false]));
  const holding = (schema: unknown): boolean => {
    if (!isObject(schema)) return false;
    if (typeof schema.$ref === "string")
      return holds.get(schema.$ref.split("/").pop() ?? "") ?? false;
    if ("items" in schema) return holding(schema.items);
    if (Array.isArray(schema.allOf)) return schema.allOf.some(holding);
    const variants = Array.isArray(schema.oneOf)
      ? schema.oneOf
      : Array.isArray(schema.anyOf)
        ? schema.anyOf
        : undefined;
    if (variants !== undefined) return variants.every(holding);
    return (
      (Array.isArray(schema.required) && schema.required.length > 0) ||
      declared(schema).length > 0 ||
      Object.values(isObject(schema.properties) ? schema.properties : {}).some(holding)
    );
  };
  for (let changed = true; changed; ) {
    changed = false;
    for (const [name, body] of reached)
      if (!holds.get(name) && holding(body)) {
        holds.set(name, true);
        changed = true;
      }
  }

  // Each schema as `absentFields` and `keyDiff` read it, in the order the tester's reading takes
  // its parts: an array's items, then an `allOf`, then a `oneOf` or an `anyOf`, then an object.
  const reduce = (schema: unknown): Required => {
    if (!isObject(schema) || !holding(schema)) return {};
    if (typeof schema.$ref === "string") return { ref: schema.$ref.split("/").pop() ?? "" };
    if ("items" in schema) return { items: reduce(schema.items) };
    if (Array.isArray(schema.allOf)) return { allOf: schema.allOf.filter(holding).map(reduce) };
    if (Array.isArray(schema.oneOf)) return { oneOf: schema.oneOf.map(reduce) };
    if (Array.isArray(schema.anyOf)) return { anyOf: schema.anyOf.map(reduce) };
    const required = Array.isArray(schema.required) ? schema.required.map(String) : [];
    const keys = declared(schema);
    const properties = Object.entries(isObject(schema.properties) ? schema.properties : {})
      .filter(([, below]) => holding(below))
      .map(([field, below]) => [field, reduce(below)] as const);
    return {
      ...(required.length > 0 ? { required } : {}),
      ...(keys.length > 0 ? { keys } : {}),
      ...(properties.length > 0 ? { properties: Object.fromEntries(properties) } : {}),
    };
  };
  const sorted = <T>(record: Record<string, T>) =>
    Object.fromEntries(Object.entries(record).sort(([one], [other]) => (one < other ? -1 : 1)));
  return Extract.parse({
    source: "github/rest-api-description",
    commit: DESCRIPTION_COMMIT,
    file: DESCRIPTION_FILE,
    routes: sorted(routes),
    operations: sorted(
      Object.fromEntries(
        Object.entries(operations).map(([key, { schema, ...rest }]) => [
          key,
          { ...rest, ...(schema === undefined ? {} : { schema: reduce(schema) }) },
        ])
      )
    ),
    schemas: sorted(
      Object.fromEntries(
        [...reached].filter(([name]) => holds.get(name)).map(([name, body]) => [name, reduce(body)])
      )
    ),
  });
}

if (import.meta.main) {
  const [file, ...rest] = Bun.argv.slice(2);
  if (file === undefined || rest.length > 0) {
    console.error("usage: bun rest-required.ts <api.github.com.json>");
    process.exit(2);
  }
  const description: unknown = JSON.parse(readFileSync(file, "utf8"));
  if (!isObject(description)) throw new Error(`${file} is no JSON object`);
  writeFileSync(REST_REQUIRED, `${JSON.stringify(derive(description), null, 2)}\n`);
  const formatted = Bun.spawnSync(["bunx", "biome", "format", "--write", REST_REQUIRED], {
    stdout: "inherit",
    stderr: "inherit",
  });
  process.exit(formatted.exitCode);
}
