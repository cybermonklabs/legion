import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";

import openapiTS, { astToString } from "openapi-typescript";

// Renders the Dispatch API's TypeScript types from the OpenAPI document the Go route table emits
// (schemas/dispatch-openapi.json, which `go run ./cmd/dispatch openapi --check` holds to the table).
// The module is not committed (.gitignore). The root package.json runs this script as its
// postinstall, so every `bun install` renders it from the document in the tree, and the
// `typecheck` script of every package whose program compiles it runs this script first, so
// `bun run typecheck` reads the document the tree holds now. A `tsc` run by other means, or an
// editor, reads the render of the last install or `bun run gen:api` (in packages/contracts), so
// run one of those after a pull or a branch switch that changes the document. So two branches
// that each add a route never edit one generated file.
//
// Only type checks read the module: dispatch-api.ts re-exports it with `export type *` and every
// other import of it is an `import type`, so Bun erases each of them, and no bundle and no `bun`
// run ever loads it. A type check in a tree that never ran `bun install` fails naming the module
// (TS2307) rather than reading types the tree does not hold. When this script fails inside the
// postinstall, the postinstall removes the module (and fails the install if it cannot) and the
// install goes on: the jobs that never type-check the Dispatch API do not depend on the document,
// and every type check that does fails naming the module rather than reading the render before.
const schemaFile = resolve(import.meta.dir, "../schemas/dispatch-openapi.json");
const output = resolve(import.meta.dir, "../src/dispatch-api.generated.ts");

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
  IssueRoleHolderRecord: true,
  ReplyBlock: true,
  Suggestion: true,
};

type OpenAPIDocument = { components?: { schemas?: Record<string, unknown> } };

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

const document = (await Bun.file(schemaFile).json()) as OpenAPIDocument;
const source = `// Code generated by scripts/gen-api.ts from schemas/dispatch-openapi.json on every \`bun install\`. DO NOT EDIT: it is not committed.

${astToString(await openapiTS(document))}
${renderAliases(document)}

${renderOwnedUnion(document)}
`;

// A module that already holds this render is left alone, so an install that changed nothing
// leaves the file's time where it was and a type checker watching it has nothing to redo.
const outputFile = Bun.file(output);
if (!(await outputFile.exists()) || (await outputFile.text()) !== source) {
  mkdirSync(dirname(output), { recursive: true });
  await Bun.write(output, source);
}
