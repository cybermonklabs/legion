import { mkdirSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";

import openapiTS, { astToString } from "openapi-typescript";

const schemaFile = resolve(import.meta.dir, "../schemas/dispatch-openapi.json");
const output = resolve(import.meta.dir, "../src/dispatch-api.generated.ts");
const repoRoot = resolve(import.meta.dir, "../../..");

// A schema name reaches the file as an exported alias, so it has to be spellable as one.
const IDENTIFIER = /^[A-Za-z_$][A-Za-z0-9_$]*$/;

// The shapes whose exported type the hand-written split owns, as `z.output` of the zod schema
// that reads them, because the two spellings are the same type: a `tsc` probe over both
// directions says each of these is mutually assignable with the alias this file would render, so
// taking the type from the schema moves no consumer and no route answer. `dispatch-api.ts`
// re-exports this module and the split together, and one name cannot come from both, so the
// alias is left out here rather than shadowed there.
//
// A name is added here only with that probe run both ways: where the two differ - `Actor`, whose
// schema reads `kind` as a literal union while the document says `string`, and the shapes with a
// field the bus takes as optional - the document's reading is the one the route table answers
// with and the alias stays.
//
// A probe run once says nothing about the next regeneration, which is why this list is rendered
// into the generated module as `SplitOwnedSchemaName` as well: `dispatch-api-owned.ts` answers
// that union with the schema reading each of these names, and asserts the two assignable both
// ways at compile time. So a Go change that moves one of these shapes reds the typecheck by the
// name it moved, instead of leaving the split's reading quietly wrong.
const OWNED_BY_THE_SPLIT: Record<string, true> = {
  ActorOrigin: true,
  Anchor: true,
  AskAnchorArtifact: true,
  AskApprovalDocument: true,
  AskBlockArtifact: true,
  AskOption: true,
  BlockPathEntry: true,
  ExternalLink: true,
  InboxDocument: true,
  ReplyBlock: true,
  Suggestion: true,
};

type OpenAPISchema = { $ref?: string; oneOf?: OpenAPISchema[] };
type OpenAPIResponse = { content?: Record<string, { schema?: OpenAPISchema }> };
type OpenAPIDocument = {
  components?: { schemas?: Record<string, unknown> };
  paths?: Record<string, Record<string, { responses?: Record<string, OpenAPIResponse> }>>;
};

const SCHEMA_REFERENCE = "#/components/schemas/";

// The name the union of every refusal body is exported under, which no schema may take.
const REFUSAL_UNION = "DispatchRefusal";

// One exported alias per schema, sorted by name, so a consumer imports `Issue` rather than
// indexing `components["schemas"]["Issue"]`.
function renderAliases(document: OpenAPIDocument) {
  const names = Object.keys(document.components?.schemas ?? {}).sort();
  const unspellable = names.filter((name) => !IDENTIFIER.test(name));
  if (unspellable.length > 0) {
    throw new Error(
      `schema names that are not TypeScript identifiers cannot be exported as aliases: ${unspellable.join(", ")}`
    );
  }
  return names
    .filter((name) => OWNED_BY_THE_SPLIT[name] !== true)
    .map((name) => `export type ${name} = components["schemas"]["${name}"];`)
    .join("\n");
}

// The same list as a union, for the assertion in `dispatch-api-owned.ts`. A name of the list the
// document does not declare is left out here as it is left out of the aliases, so the union
// always names shapes this module's `components` holds.
function renderOwnedUnion(document: OpenAPIDocument) {
  const owned = Object.keys(document.components?.schemas ?? {})
    .sort()
    .filter((name) => OWNED_BY_THE_SPLIT[name] === true);
  const union = owned.length === 0 ? "never" : owned.map((name) => `"${name}"`).join(" | ");
  return `/** Every schema name whose exported type the hand-written split owns, which\n * \`dispatch-api-owned.ts\` holds to the shape this module declares for it. */\nexport type SplitOwnedSchemaName = ${union};`;
}

// Every body a route refuses a request with: each schema an operation answers at a 4xx or 5xx
// status or as its default answer, the common `{error, code}` (`ErrorBody`) among them. A client
// reads a refusal before its `code` says which one it holds, so this union is what it reads one as
// (`DispatchServiceErrorShape`), and a refusal body a route grows reaches that reading by
// regeneration. A refusal schema that names no component cannot be named in the union, so the
// render refuses it rather than leave that refusal out.
function renderRefusalUnion(document: OpenAPIDocument) {
  if (document.components?.schemas?.[REFUSAL_UNION] !== undefined) {
    throw new Error(`the schema ${REFUSAL_UNION} takes the name of the union of refusal bodies`);
  }
  const names = new Set<string>();
  for (const [pattern, operations] of Object.entries(document.paths ?? {})) {
    for (const [method, operation] of Object.entries(operations)) {
      for (const [status, response] of Object.entries(operation.responses ?? {})) {
        if (status !== "default" && Number(status) < 400) continue;
        const schema = response.content?.["application/json"]?.schema;
        if (schema === undefined) continue;
        for (const shape of schema.oneOf ?? [schema]) {
          if (!shape.$ref?.startsWith(SCHEMA_REFERENCE)) {
            throw new Error(
              `${method.toUpperCase()} ${pattern} answers ${status} with a schema that names no component, which ${REFUSAL_UNION} cannot name`
            );
          }
          names.add(shape.$ref.slice(SCHEMA_REFERENCE.length));
        }
      }
    }
  }
  if (names.size === 0) {
    throw new Error(`the document names no refusal body for ${REFUSAL_UNION} to hold`);
  }
  const union = [...names]
    .sort()
    .map((name) => `components["schemas"]["${name}"]`)
    .join(" | ");
  return `/** Every body a route refuses a request with: each schema an operation answers at a 4xx or\n * 5xx status or as its default answer. A client reads a refusal as \`DispatchServiceErrorShape\`\n * before its \`code\` says which of these it holds. */\nexport type ${REFUSAL_UNION} = ${union};`;
}

const document = (await Bun.file(schemaFile).json()) as OpenAPIDocument;
const rendered = `// Code generated by scripts/gen-api.ts from schemas/dispatch-openapi.json. DO NOT EDIT.

${astToString(await openapiTS(document))}
${renderAliases(document)}

${renderOwnedUnion(document)}

${renderRefusalUnion(document)}
`;

// openapi-typescript prints what the TypeScript compiler prints, which is four-space indented and
// unaware of this repository's line width, so the render is piped through Biome, the repository's
// formatter, before it is written or compared: `bunx biome check packages/contracts/` has to pass
// over the file, and a checked tree and a written tree have to agree on its bytes.
const biome = Bun.which("biome", {
  PATH: [
    resolve(repoRoot, "packages/contracts/node_modules/.bin"),
    resolve(repoRoot, "node_modules/.bin"),
    process.env.PATH ?? "",
  ].join(":"),
});
if (biome === null) {
  throw new Error("gen-api needs Biome: run `bun install` at the repository root");
}
const formatted = Bun.spawnSync({
  cmd: [biome, "format", `--stdin-file-path=${relative(repoRoot, output)}`],
  cwd: repoRoot,
  stdin: new TextEncoder().encode(rendered),
  stderr: "inherit",
});
if (formatted.exitCode !== 0) {
  throw new Error("biome format failed");
}
const source = formatted.stdout.toString();

// `--check` compares the fresh render with the committed file and writes nothing, so an OpenAPI
// document regenerated from the Go route table without regenerating this module fails CI instead
// of being rewritten in place.
if (process.argv.includes("--check")) {
  // GEN_API_CHECK_ROOT points the comparison at a copy of the output under another root, laid out
  // by its repository-relative path, so a test can check a copy instead of the tracked file.
  // Unset, `--check` compares the committed file.
  const checkRoot = process.env.GEN_API_CHECK_ROOT;
  const path = checkRoot ? resolve(checkRoot, relative(repoRoot, output)) : output;
  if (source !== (await Bun.file(path).text())) {
    Bun.spawnSync({
      cmd: ["diff", "-u", path, "-"],
      stdin: new TextEncoder().encode(source),
      stderr: "inherit",
      stdout: "inherit",
    });
    console.error(
      `${path} is stale: run \`bun run gen:api\` in packages/contracts and commit the result.`
    );
    process.exit(1);
  }
  process.exit(0);
}

mkdirSync(dirname(output), { recursive: true });
await Bun.write(output, source);
