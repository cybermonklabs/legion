// packages/envoy/scripts/redact-gate/ts-census.ts
//
// The TypeScript half of cmd/redact-gate's call census. It reads, from stdin, the snapshot's root,
// each changed TypeScript production file with the lines the change added or changed, and the
// rule ("<file>#<name>", the declarations of readRemoteText and redactRemoteText). It builds one
// program per package from that package's tsconfig.json, resolves every identifier on a changed
// line with the compiler's checker (following an import alias to its declaration), and writes,
// as JSON on stdout, each reference whose declaration is the rule, the function it is in, and the
// byte range of every function of each changed file. A file that does not parse, or whose
// package's program does not build, is UNKNOWN.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import ts from "typescript";

interface Request {
  readonly root: string;
  readonly files: Record<string, Record<string, boolean>>;
  readonly rule: readonly string[];
}

const request = JSON.parse(readFileSync(0, "utf8")) as Request;
const calls: unknown[] = [];
const unknown: string[] = [];
const functions: Record<string, Record<string, [number, number]>> = {};
const rule = new Set(request.rule);

/** The package directory (holding tsconfig.json) of a file under packages/<name>/. */
function packageOf(file: string): string {
  let dir = dirname(join(request.root, file));
  while (dir.startsWith(request.root) && !existsSync(join(dir, "tsconfig.json")))
    dir = dirname(dir);
  return dir;
}

/** The name the gate binds a function by: "Class.member", "Class.constructor", "name". */
function functionName(node: ts.Node): string | undefined {
  const named = (n: ts.Node): string | undefined => {
    if (ts.isConstructorDeclaration(n) && ts.isClassLike(n.parent))
      return `${n.parent.name?.text ?? "?"}.constructor`;
    if (
      (ts.isMethodDeclaration(n) ||
        ts.isGetAccessor(n) ||
        ts.isSetAccessor(n) ||
        ts.isPropertyDeclaration(n)) &&
      ts.isClassLike(n.parent)
    ) {
      return `${n.parent.name?.text ?? "?"}.${n.name.getText()}`;
    }
    if (ts.isFunctionDeclaration(n) && n.name) return n.name.text;
    if (
      ts.isVariableDeclaration(n) &&
      ts.isIdentifier(n.name) &&
      n.initializer &&
      (ts.isArrowFunction(n.initializer) || ts.isFunctionExpression(n.initializer))
    )
      return n.name.text;
    return undefined;
  };
  let found: string | undefined;
  for (let n: ts.Node | undefined = node; n; n = n.parent) {
    const name = named(n);
    if (name) found = name; // the outermost named function holds it
  }
  return found;
}

const byPackage = new Map<string, string[]>();
for (const file of Object.keys(request.files)) {
  const pkg = packageOf(file);
  byPackage.set(pkg, [...(byPackage.get(pkg) ?? []), file]);
}

for (const [pkg, files] of byPackage) {
  const configPath = join(pkg, "tsconfig.json");
  const config = ts.readConfigFile(configPath, ts.sys.readFile);
  if (config.error) {
    for (const f of files) unknown.push(`${f}: its package's tsconfig.json cannot be read`);
    continue;
  }
  const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, pkg, undefined, configPath);
  const roots = files.map((f) => join(request.root, f));
  const program = ts.createProgram({
    rootNames: [...parsed.fileNames, ...roots],
    options: { ...parsed.options, noEmit: true },
  });
  const checker = program.getTypeChecker();
  for (const file of files) {
    const source = program.getSourceFile(join(request.root, file));
    if (!source) {
      unknown.push(`${file}: the compiler did not load it`);
      continue;
    }
    const syntax = program.getSyntacticDiagnostics(source);
    if (syntax.length > 0) {
      unknown.push(
        `${file}: cannot be parsed: ${ts.flattenDiagnosticMessageText(syntax[0]?.messageText ?? "", " ")}`
      );
      continue;
    }
    // A name the checker cannot resolve (an import it cannot find, say) would hide a call of the
    // rule, so a file with any semantic error is not read.
    const semantic = program
      .getSemanticDiagnostics(source)
      .filter((d) => d.category === ts.DiagnosticCategory.Error);
    if (semantic.length > 0) {
      unknown.push(
        `${file}: cannot be resolved: ${ts.flattenDiagnosticMessageText(semantic[0]?.messageText ?? "", " ")}`
      );
      continue;
    }
    const lines = request.files[file] ?? {};
    const ranges: Record<string, [number, number]> = {};
    // The compiler's positions count UTF-16 code units; the gate binds by bytes of the file.
    const text = source.getFullText();
    const bytes = (position: number): number => Buffer.byteLength(text.slice(0, position), "utf8");
    const visit = (node: ts.Node): void => {
      const own =
        ts.isConstructorDeclaration(node) ||
        ts.isMethodDeclaration(node) ||
        ts.isGetAccessor(node) ||
        ts.isSetAccessor(node) ||
        ts.isFunctionDeclaration(node) ||
        (ts.isVariableDeclaration(node) &&
          node.initializer !== undefined &&
          (ts.isArrowFunction(node.initializer) || ts.isFunctionExpression(node.initializer)));
      if (own) {
        const name = functionName(node);
        if (name && ranges[name] === undefined)
          ranges[name] = [bytes(node.getStart(source)), bytes(node.getEnd())];
      }
      if (ts.isIdentifier(node)) {
        const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
        if (lines[String(line)]) {
          let symbol = checker.getSymbolAtLocation(node);
          if (symbol && symbol.flags & ts.SymbolFlags.Alias)
            symbol = checker.getAliasedSymbol(symbol);
          const declaration = symbol?.declarations?.[0];
          const isDeclarationName =
            (declaration !== undefined && (declaration as ts.NamedDeclaration).name === node) ||
            ts.isImportSpecifier(node.parent) ||
            ts.isImportClause(node.parent) ||
            ts.isExportSpecifier(node.parent);
          if (declaration && !isDeclarationName) {
            const where = relative(request.root, declaration.getSourceFile().fileName);
            const key = `${where}#${symbol?.getName() ?? ""}`;
            if (rule.has(key)) {
              calls.push({
                file,
                line,
                offset: bytes(node.getStart(source)),
                callee: key,
                function: functionName(node) ?? "<module>",
              });
            }
          }
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
    functions[file] = ranges;
  }
}

process.stdout.write(JSON.stringify({ calls, unknown, functions }));
