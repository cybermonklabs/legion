import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import ts from "typescript";

const generator = resolve(import.meta.dir, "gen-api.ts");
const repoRoot = resolve(import.meta.dir, "../../..");

// The module each document renders into (scripts/gen-api.ts's MODULES), with what the cases below
// read of each: the version its paths are mounted under, and one alias of a schema that document
// declares, which the module must export.
const MODULES = [
  {
    document: "schemas/dispatch-openapi.json",
    output: resolve(import.meta.dir, "../src/dispatch-api.generated.ts"),
    version: "v1",
    alias: 'export type Issue = components["schemas"]["Issue"];',
  },
  {
    document: "schemas/dispatch-openapi-v2.json",
    output: resolve(import.meta.dir, "../src/dispatch-api-v2.generated.ts"),
    version: "v2",
    alias: 'export type AskV2 = components["schemas"]["AskV2"];',
  },
];

type Module = (typeof MODULES)[number];

type Document = {
  paths: Record<string, unknown>;
  components: { schemas: Record<string, { properties?: Record<string, unknown> }> };
};

/** What one module declares, read from its syntax tree: every path of `paths`, every schema of
 * `components.schemas` with the names of its own fields, every schema exported as an alias, and
 * the schemas `SplitOwnedSchemaName` says the hand-written split exports instead. */
function declaredBy(source: string) {
  const file = ts.createSourceFile("dispatch-api.generated.ts", source, ts.ScriptTarget.Latest);
  const paths = new Set<string>();
  const schemas = new Map<string, string[]>();
  const aliases = new Set<string>();
  const owned = new Set<string>();
  const nameOf = (name: ts.PropertyName) =>
    ts.isIdentifier(name) || ts.isStringLiteral(name) ? name.text : name.getText(file);
  // A schema's own fields are the property members of its object type, or of each object type
  // an intersection joins, which is how an object with a union beside it is rendered.
  const fieldsOf = (type: ts.TypeNode | undefined): string[] => {
    if (type === undefined) return [];
    if (ts.isTypeLiteralNode(type)) {
      return type.members
        .filter(ts.isPropertySignature)
        .map((member) => nameOf(member.name))
        .sort();
    }
    if (ts.isIntersectionTypeNode(type)) return type.types.flatMap(fieldsOf).sort();
    if (ts.isParenthesizedTypeNode(type)) return fieldsOf(type.type);
    return [];
  };
  for (const statement of file.statements) {
    // What a consumer can import is what the module exports, so a declaration without `export`
    // declares nothing here.
    const exported =
      ts.canHaveModifiers(statement) &&
      (ts.getModifiers(statement) ?? []).some(
        (modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword
      );
    if (!exported) continue;
    if (ts.isInterfaceDeclaration(statement) && statement.name.text === "paths") {
      for (const member of statement.members) {
        if (member.name !== undefined) paths.add(nameOf(member.name));
      }
    }
    if (ts.isInterfaceDeclaration(statement) && statement.name.text === "components") {
      const block = statement.members.find(
        (member) => member.name !== undefined && nameOf(member.name) === "schemas"
      );
      if (
        block !== undefined &&
        ts.isPropertySignature(block) &&
        block.type !== undefined &&
        ts.isTypeLiteralNode(block.type)
      ) {
        for (const schema of block.type.members.filter(ts.isPropertySignature)) {
          schemas.set(nameOf(schema.name), fieldsOf(schema.type));
        }
      }
    }
    if (ts.isTypeAliasDeclaration(statement)) {
      const name = statement.name.text;
      if (name === "SplitOwnedSchemaName") {
        const members = ts.isUnionTypeNode(statement.type)
          ? statement.type.types
          : [statement.type];
        for (const member of members) {
          if (ts.isLiteralTypeNode(member) && ts.isStringLiteral(member.literal))
            owned.add(member.literal.text);
        }
      } else if (statement.type.getText(file) === `components["schemas"]["${name}"]`) {
        aliases.add(name);
      }
    }
  }
  return { paths, schemas, aliases, owned };
}

/** Every way the module falls short of the document: a path it does not declare, a schema it
 * does not declare or declares with other fields, and a schema it neither exports as an alias
 * nor names as the split's. Empty when the module declares the document whole. */
function shortfalls(document: Document, source: string): string[] {
  const module = declaredBy(source);
  const problems: string[] = [];
  for (const path of Object.keys(document.paths)) {
    if (!module.paths.has(path)) problems.push(`the module declares no path ${path}`);
  }
  for (const [name, schema] of Object.entries(document.components.schemas)) {
    const fields = module.schemas.get(name);
    if (fields === undefined) {
      problems.push(`the module declares no schema ${name}`);
      continue;
    }
    const want = Object.keys(schema.properties ?? {}).sort();
    if (fields.join(",") !== want.join(",")) {
      problems.push(
        `the module declares ${name} as [${fields.join(", ")}], and the document as [${want.join(", ")}]`
      );
    }
    if (!module.aliases.has(name) && !module.owned.has(name)) {
      problems.push(`the module exports no alias for ${name} and does not name it as the split's`);
    }
  }
  return problems;
}

/** Runs the generator, which renders every module of MODULES, and answers `module`'s render. */
async function render(module: Module): Promise<string> {
  const spawned = Bun.spawn(["bun", generator], { cwd: repoRoot, stderr: "pipe", stdout: "pipe" });
  const stderr = await new Response(spawned.stderr).text();
  expect(stderr).toBe("");
  expect(await spawned.exited).toBe(0);
  return Bun.file(module.output).text();
}

async function documentOf(module: Module): Promise<Document> {
  return (await Bun.file(resolve(import.meta.dir, "..", module.document)).json()) as Document;
}

/** The real jj on PATH, and whether this run is on GitHub Actions (`GITHUB_ACTIONS`, not `CI`,
 * which agent harnesses on the devbox export), as adopt-api-layout.test.ts reads them. */
const JJ = Bun.which("jj");
const ON_ACTIONS = process.env.GITHUB_ACTIONS === "true";

/** `output`'s path if the commit checked out at `root` holds it, and empty when none does. A
 * checkout jj answers for (`jj root`) is read with jj, as adopt-api-layout.sh reads it: jj tracks
 * files itself, and in a colocated workspace git's index can still hold the conflict stages of a
 * merge jj resolved, which jj never resets, so git would name a module no commit holds. Every
 * other checkout is read with git. */
function committedModule(root: string, output: string): string {
  const path = relative(repoRoot, output);
  const jj =
    JJ !== null &&
    Bun.spawnSync({ cmd: [JJ, "root"], cwd: root, stdout: "pipe", stderr: "pipe" }).exitCode === 0;
  const cmd = jj
    ? [JJ as string, "file", "list", "-r", "@", "--", path]
    : ["git", "ls-files", "--", path];
  const done = Bun.spawnSync({ cmd, cwd: root, stdout: "pipe", stderr: "pipe" });
  if (done.exitCode !== 0) {
    throw new Error(
      `${cmd.join(" ")} exited ${done.exitCode} in ${root}, so whether it holds ${path} is unknown: ${done.stderr.toString()}`
    );
  }
  return done.stdout.toString();
}

for (const module of MODULES) {
  const name = relative(repoRoot, module.output);

  describe(`gen-api: ${name}`, () => {
    // The module is rendered from its document on every install and committed nowhere, so the
    // link this holds is the generator's own: what it writes declares every route and every
    // schema of the document, field for field, and exports each schema under its own name or
    // leaves it to the split by name. The generator renders with openapi-typescript, so a slow
    // first import on a cold runner is why the bound is 120 s.
    test(`the module declares every path and every schema of ${module.document}, field for field`, async () => {
      const source = await render(module);
      const document = await documentOf(module);
      expect(source).toStartWith(`// Code generated by scripts/gen-api.ts from ${module.document}`);
      expect(source).toContain(module.alias);
      expect(Object.keys(document.paths).length).toBeGreaterThan(100);
      expect(Object.keys(document.components.schemas).length).toBeGreaterThan(100);
      expect(shortfalls(document, source)).toEqual([]);
    }, 120_000);

    // The control: a document holding a path, a schema and a field the module does not declare
    // is three shortfalls, so the check above can see each kind it passes.
    test("a path, a schema and a field the module leaves out are each a shortfall", async () => {
      const source = await render(module);
      const document = await documentOf(module);
      document.paths[`/api/${module.version}/_planted`] = {};
      document.components.schemas.PlantedSchema = { properties: { a: {} } };
      const issue = document.components.schemas.Issue;
      if (issue === undefined)
        throw new Error(`${module.document} declares no Issue schema to plant a field on`);
      issue.properties = { ...issue.properties, planted_field: {} };
      expect(shortfalls(document, source)).toEqual([
        `the module declares no path /api/${module.version}/_planted`,
        expect.stringContaining("the module declares Issue as ["),
        "the module declares no schema PlantedSchema",
      ]);
    }, 120_000);

    // The control for exports: an alias written without `export` is one no consumer imports, so
    // it is a shortfall by its name.
    test("an alias the module declares without exporting it is a shortfall", async () => {
      const source = (await render(module)).replace(
        'export type Issue = components["schemas"]["Issue"];',
        'type Issue = components["schemas"]["Issue"];'
      );
      const document = await documentOf(module);
      expect(source).toContain('\ntype Issue = components["schemas"]["Issue"];');
      expect(shortfalls(document, source)).toEqual([
        "the module exports no alias for Issue and does not name it as the split's",
      ]);
    }, 120_000);
  });

  describe(`the generated module ${name}`, () => {
    // It is in no commit: every install renders it. A branch made before that, merged with main
    // without packages/contracts/scripts/adopt-api-layout.sh, can carry it back, and this is what
    // stops that merge.
    test("is in no commit of this checkout", () => {
      expect(committedModule(repoRoot, module.output)).toBe("");
    });

    // The control: a scratch repository whose commit holds the module is read as holding it.
    test("a commit that holds it is read as holding it", () => {
      const root = mkdtempSync(join(tmpdir(), "gen-api-committed-"));
      try {
        mkdirSync(dirname(join(root, name)), { recursive: true });
        writeFileSync(join(root, name), "// committed\n");
        for (const cmd of [
          ["git", "init", "-q", "."],
          ["git", "add", "-A"],
        ]) {
          expect(Bun.spawnSync({ cmd, cwd: root }).exitCode).toBe(0);
        }
        expect(committedModule(root, module.output)).toBe(`${name}\n`);
      } finally {
        rmSync(root, { force: true, recursive: true });
      }
    });

    // A jj-colocated workspace after a merge of main that adopt-api-layout.sh resolved, as every
    // Legion pod is: the branch changed the module, main deleted and ignored it, `jj new` made the
    // merge and `jj file untrack` took the module out of it. jj leaves git's index holding the
    // module's conflict stages, so the control is that git still names it there: a reading that
    // asked git first would call this checkout one that holds the module, which no commit does.
    test.skipIf(JJ === null && !ON_ACTIONS)(
      "after a jj merge the adopter resolved, it is read as in no commit",
      () => {
        if (JJ === null)
          throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
        const root = mkdtempSync(join(tmpdir(), "gen-api-jj-merge-"));
        const env = {
          ...process.env,
          GIT_AUTHOR_NAME: "t",
          GIT_AUTHOR_EMAIL: "t@example.invalid",
          GIT_COMMITTER_NAME: "t",
          GIT_COMMITTER_EMAIL: "t@example.invalid",
          JJ_USER: "t",
          JJ_EMAIL: "t@example.invalid",
          JJ_CONFIG: "/dev/null",
        };
        const must = (cmd: string[]) => {
          const done = Bun.spawnSync({ cmd, cwd: root, env, stdout: "pipe", stderr: "pipe" });
          if (done.exitCode !== 0) {
            throw new Error(`${cmd.join(" ")} exited ${done.exitCode}: ${done.stderr.toString()}`);
          }
          return done.stdout.toString();
        };
        const write = (file: string, text: string) => {
          mkdirSync(dirname(join(root, file)), { recursive: true });
          writeFileSync(join(root, file), text);
        };
        try {
          must(["git", "init", "-q", "-b", "main", "."]);
          write(".gitignore", "node_modules/\n");
          write(name, "// committed render\n");
          must(["git", "add", "-A"]);
          must(["git", "commit", "-qm", "base"]);
          must(["git", "checkout", "-qb", "branch"]);
          write(name, "// the branch's render\n");
          must(["git", "commit", "-qam", "branch"]);
          must(["git", "checkout", "-q", "main"]);
          write(".gitignore", `node_modules/\n${name}\n`);
          must(["git", "rm", "-q", name]);
          must(["git", "commit", "-qam", "main"]);
          must(["git", "checkout", "-q", "branch"]);
          must([JJ, "git", "init", "--colocate"]);
          must([JJ, "new", "branch", "main", "-m", "merge main"]);
          // The adopter renders the module and then untracks it.
          write(name, "// rendered\n");
          must([JJ, "file", "untrack", "--", name]);
          expect(must([JJ, "file", "list", "-r", "@", "--", name])).toBe("");
          expect(must(["git", "ls-files", "--", name])).not.toBe("");
          expect(committedModule(root, module.output)).toBe("");
        } finally {
          rmSync(root, { force: true, recursive: true });
        }
      },
      60_000
    );
  });
}
