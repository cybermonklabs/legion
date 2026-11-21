import { mkdirSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";

import openapiTS, { astToString } from "openapi-typescript";

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

// The two documents `envoy-dispatch openapi` writes, one per version of the API, and the module
// each renders into. Each is a module of its own because the two documents share schema names
// (`Issue`, `Project`, ...): `/api/v1`'s is re-exported by the `dispatch-api.ts` barrel with the
// hand-written split, and `/api/v2`'s by `dispatch-api-v2.ts` alone, under the subpath export
// `@legion/contracts/dispatch-api-v2`, so no `export *` of one name meets another (TS2308).
// Only `/api/v1`'s module has a split beside it, so only it leaves the split's names out and
// renders `SplitOwnedSchemaName`.
const MODULES = [
  {
    document: "schemas/dispatch-openapi.json",
    output: "src/dispatch-api.generated.ts",
    split: true,
  },
  {
    document: "schemas/dispatch-openapi-v2.json",
    output: "src/dispatch-api-v2.generated.ts",
    split: false,
  },
] as const;

type OpenAPIDocument = { components?: { schemas?: Record<string, unknown> } };

// One exported alias per schema, sorted by name, so a consumer imports `Issue` rather than
// indexing `components["schemas"]["Issue"]`.
function renderAliases(document: OpenAPIDocument, split: boolean) {
  const names = Object.keys(document.components?.schemas ?? {}).sort();
  const unspellable = names.filter((name) => !IDENTIFIER.test(name));
  if (unspellable.length > 0) {
    throw new Error(
      `schema names that are not TypeScript identifiers cannot be exported as aliases: ${unspellable.join(", ")}`
    );
  }
  return names
    .filter((name) => !split || OWNED_BY_THE_SPLIT[name] !== true)
    .map((name) => `export type ${name} = components["schemas"]["${name}"];`)
    .join("\n");
}

// The split's list as a union, for the assertion in `dispatch-api-owned.ts`. A name of the list
// the document does not declare is left out here as it is left out of the aliases, so the union
// always names shapes this module's `components` holds.
function renderOwnedUnion(document: OpenAPIDocument) {
  const owned = Object.keys(document.components?.schemas ?? {})
    .sort()
    .filter((name) => OWNED_BY_THE_SPLIT[name] === true);
  const union = owned.length === 0 ? "never" : owned.map((name) => `"${name}"`).join(" | ");
  return `/** Every schema name whose exported type the hand-written split owns, which\n * \`dispatch-api-owned.ts\` holds to the shape this module declares for it. */\nexport type SplitOwnedSchemaName = ${union};`;
}

// openapi-typescript prints what the TypeScript compiler prints, which is four-space indented and
// unaware of this repository's line width, so each render is piped through Biome, the
// repository's formatter, before it is written or compared: `bunx biome check packages/contracts/`
// has to pass over the file, and a checked tree and a written tree have to agree on its bytes.
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

async function render(module: (typeof MODULES)[number], output: string) {
  const document = (await Bun.file(
    resolve(import.meta.dir, "..", module.document)
  ).json()) as OpenAPIDocument;
  const owned = module.split ? `\n${renderOwnedUnion(document)}\n` : "";
  const rendered = `// Code generated by scripts/gen-api.ts from ${module.document}. DO NOT EDIT.

${astToString(await openapiTS(document))}
${renderAliases(document, module.split)}
${owned}`;
  const formatted = Bun.spawnSync({
    cmd: [biome, "format", `--stdin-file-path=${relative(repoRoot, output)}`],
    cwd: repoRoot,
    stdin: new TextEncoder().encode(rendered),
    stderr: "inherit",
  });
  if (formatted.exitCode !== 0) {
    throw new Error(`biome format failed on ${module.output}`);
  }
  return formatted.stdout.toString();
}

// `--check` compares each fresh render with its committed file and writes nothing, so an OpenAPI
// document regenerated from the Go route table without regenerating its module fails CI instead
// of being rewritten in place. GEN_API_CHECK_ROOT points the comparison at a copy of the outputs
// under another root, laid out by their repository-relative paths, so a test can check a copy
// instead of the tracked files. Unset, `--check` compares the committed files.
const check = process.argv.includes("--check");
const checkRoot = process.env.GEN_API_CHECK_ROOT;
let stale = false;
for (const module of MODULES) {
  const output = resolve(import.meta.dir, "..", module.output);
  const source = await render(module, output);
  if (!check) {
    mkdirSync(dirname(output), { recursive: true });
    await Bun.write(output, source);
    continue;
  }
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
    stale = true;
  }
}
process.exit(stale ? 1 : 0);
