// The census of `callLibrary` (packages/contracts/src/remote-text.ts), the one way this workspace
// calls openid-client, and the TypeScript half of boundary/main.go beside it. It reads, through
// the TypeScript compiler's own checker, every export of openid-client at the version the lockfile
// pins — each function, each class's constructor, and each method of a class's instances and of
// the class itself, any of which can throw — and then every production source file of the
// projects it is given, and fails on each reference to one of them that is not inside the arrow
// function passed alone to `callLibrary`, nor inside a function nested in that arrow, naming the
// line. Inside the arrow a reference must stay in the call the arrow makes: what a call or a `new`
// there calls, or an argument of one, alone or in array and object literals and conditionals. One
// the arrow answers, or keeps any other way, hands the library's function out of `callLibrary`,
// to be called outside it. No list of names is written here: the closed set is the library's.
//
// Usage, from the repository root: bun packages/envoy/internal/redact/testdata/wholeuse/boundary.ts [-api] [tsconfig.json ...]
// With no tsconfig it reads every packages/*/tsconfig.json.
import { readdirSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import ts from "typescript";

const LIBRARY = "openid-client";

const args = process.argv.slice(2);
const listAPI = args.includes("-api");
const root = process.cwd();
const named = args.filter((arg) => arg !== "-api");
const configs =
  named.length > 0
    ? named
    : readdirSync(join(root, "packages"))
        .map((name) => join(root, "packages", name, "tsconfig.json"))
        .filter((path) => ts.sys.fileExists(path));

/** The symbol a reference names, past an import's alias. */
function target(checker: ts.TypeChecker, symbol: ts.Symbol | undefined): ts.Symbol | undefined {
  if (symbol === undefined) return undefined;
  return symbol.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(symbol) : symbol;
}

/** Each export of the library's module: its functions, constructors and methods, by symbol. */
function libraryAPI(checker: ts.TypeChecker, module: ts.Symbol): Map<ts.Symbol, string> {
  const api = new Map<ts.Symbol, string>();
  for (const exported of checker.getExportsOfModule(module)) {
    const symbol = target(checker, exported);
    if (symbol === undefined) continue;
    const name = exported.getName();
    if (symbol.flags & ts.SymbolFlags.Function) api.set(symbol, `function ${name}`);
    if (!(symbol.flags & ts.SymbolFlags.Class)) continue;
    api.set(symbol, `constructor ${name}`);
    const declaration = symbol.valueDeclaration ?? symbol.declarations?.[0];
    const sides = [checker.getDeclaredTypeOfSymbol(symbol)];
    if (declaration !== undefined)
      sides.push(checker.getTypeOfSymbolAtLocation(symbol, declaration));
    for (const side of sides) {
      for (const member of side.getProperties()) {
        if (member.flags & ts.SymbolFlags.Method)
          api.set(member, `method ${name}.${member.getName()}`);
      }
    }
  }
  return api;
}

/** Whether `node` is the arrow function passed alone to `callLibrary`. */
function boundaryArrow(checker: ts.TypeChecker, node: ts.Node): boolean {
  if (!ts.isArrowFunction(node) || !ts.isCallExpression(node.parent)) return false;
  const call = node.parent;
  if (call.arguments.length !== 1 || call.arguments[0] !== node) return false;
  const callee = target(checker, checker.getSymbolAtLocation(call.expression));
  const file = callee?.declarations?.[0]?.getSourceFile().fileName ?? "";
  return callee?.getName() === "callLibrary" && file.endsWith("/contracts/src/remote-text.ts");
}

/** Whether the reference `node` lies inside a boundary arrow, and in no other function first. */
function insideBoundary(checker: ts.TypeChecker, node: ts.Node): boolean {
  for (let at: ts.Node = node.parent; !ts.isSourceFile(at); at = at.parent) {
    if (boundaryArrow(checker, at)) return true;
    if (ts.isFunctionLike(at)) return false;
  }
  return false;
}

/** Whether the reference `node` stays in the call the boundary arrow makes: it is what a call or a
 *  `new` calls, or an argument of one, alone or inside array and object literals and the branches
 *  of a conditional. */
function staysInCall(node: ts.Node): boolean {
  let at: ts.Node =
    ts.isPropertyAccessExpression(node.parent) && node.parent.name === node ? node.parent : node;
  for (;;) {
    const parent = at.parent;
    if (ts.isCallExpression(parent) || ts.isNewExpression(parent)) {
      return parent.expression === at || (parent.arguments ?? []).some((arg) => arg === at);
    }
    if (
      ts.isParenthesizedExpression(parent) ||
      ts.isArrayLiteralExpression(parent) ||
      ts.isSpreadElement(parent) ||
      ts.isObjectLiteralExpression(parent) ||
      ts.isSpreadAssignment(parent) ||
      ts.isShorthandPropertyAssignment(parent) ||
      (ts.isPropertyAssignment(parent) && parent.initializer === at) ||
      (ts.isConditionalExpression(parent) && parent.condition !== at)
    ) {
      at = parent;
      continue;
    }
    return false;
  }
}

/** The fetch a reference to the library's `customFetch` hands it: the value of `[customFetch]:` in
 *  an object literal, or the right side of `x[customFetch] = …`; undefined for any other reference,
 *  which hands nothing the census can read and so fails. */
function customFetchValue(node: ts.Node): ts.Expression | undefined {
  const parent = node.parent;
  if (ts.isComputedPropertyName(parent) && ts.isPropertyAssignment(parent.parent)) {
    return parent.parent.initializer;
  }
  if (
    ts.isElementAccessExpression(parent) &&
    parent.argumentExpression === node &&
    ts.isBinaryExpression(parent.parent) &&
    parent.parent.left === parent &&
    parent.parent.operatorToken.kind === ts.SyntaxKind.EqualsToken
  ) {
    return parent.parent.right;
  }
  return undefined;
}

/** Whether `value` is a call of `refusalFetch` from `@legion/contracts/remote-text`, which records
 *  each refused request's credentials for the boundary. */
function refusalFetchCall(checker: ts.TypeChecker, value: ts.Expression): boolean {
  if (!ts.isCallExpression(value)) return false;
  const callee = target(checker, checker.getSymbolAtLocation(value.expression));
  const declaration = callee?.declarations?.[0];
  return (
    callee?.name === "refusalFetch" &&
    declaration !== undefined &&
    declaration.getSourceFile().fileName.endsWith("/contracts/src/remote-text.ts")
  );
}

const lines: string[] = [];
const apiCounts = new Map<string, number>();
let apiSize = 0;
let references = 0;
let failed = 0;
let files = 0;
const listed = new Set<string>();
for (const config of configs) {
  const read = ts.readConfigFile(config, ts.sys.readFile);
  if (read.error)
    throw new Error(`${config}: ${ts.flattenDiagnosticMessageText(read.error.messageText, "\n")}`);
  const parsed = ts.parseJsonConfigFileContent(read.config, ts.sys, dirname(resolve(config)));
  const program = ts.createProgram(parsed.fileNames, parsed.options);
  const checker = program.getTypeChecker();
  const resolved = ts.resolveModuleName(
    LIBRARY,
    resolve(config),
    parsed.options,
    ts.sys
  ).resolvedModule;
  const libraryFile =
    resolved === undefined ? undefined : program.getSourceFile(resolved.resolvedFileName);
  const module = libraryFile === undefined ? undefined : checker.getSymbolAtLocation(libraryFile);
  if (module === undefined) continue;
  const api = libraryAPI(checker, module);
  const customFetchSymbol = checker
    .getExportsOfModule(module)
    .map((exported) => target(checker, exported))
    .find((symbol) => symbol?.name === "customFetch");
  if (apiSize === 0) {
    apiSize = api.size;
    for (const kind of api.values()) {
      const what = kind.split(" ")[0] as string;
      apiCounts.set(what, (apiCounts.get(what) ?? 0) + 1);
      if (listAPI) console.log(`LIBRARY\t${kind}`);
    }
  }
  const fileNames = new Set(parsed.fileNames);
  for (const source of program.getSourceFiles()) {
    const name = source.fileName;
    if (
      source.isDeclarationFile ||
      !fileNames.has(name) ||
      name.includes("/node_modules/") ||
      name.includes("/__tests__/") ||
      name.includes("/test/") ||
      name.endsWith(".test.ts")
    ) {
      continue;
    }
    files++;
    const visit = (node: ts.Node): void => {
      const flag = (at: ts.Node, kind: string, why: string): void => {
        const position = source.getLineAndCharacterOfPosition(at.getStart(source));
        const where = `${relative(root, name)}:${position.line + 1}:${position.character + 1}`;
        if (listed.has(where)) return;
        listed.add(where);
        references++;
        failed++;
        lines.push(`FAIL\t${where}\t${kind}\t${why}`);
      };
      // A dynamic import of the library answers its module namespace, whose every function the
      // census cannot follow once it is held: it is the library's only outside callLibrary.
      if (
        ts.isCallExpression(node) &&
        node.expression.kind === ts.SyntaxKind.ImportKeyword &&
        node.arguments.length > 0 &&
        ts.isStringLiteralLike(node.arguments[0] as ts.Expression) &&
        (node.arguments[0] as ts.StringLiteralLike).text === LIBRARY &&
        !insideBoundary(checker, node)
      ) {
        flag(node, "dynamic import", "a dynamic import of the library outside callLibrary");
      }
      if (
        ts.isIdentifier(node) &&
        !ts.isPartOfTypeNode(node) &&
        !ts.isImportSpecifier(node.parent)
      ) {
        const symbol = target(checker, checker.getSymbolAtLocation(node));
        // The library's module namespace (`import * as ns`) is read only as `ns.<member>`, whose
        // member the rules below hold: held, destructured or handed out, its functions leave the
        // census's sight.
        if (
          symbol !== undefined &&
          symbol === module &&
          !ts.isNamespaceImport(node.parent) &&
          !(ts.isPropertyAccessExpression(node.parent) && node.parent.expression === node)
        ) {
          flag(
            node,
            "module namespace",
            "the library's namespace is held, destructured or handed out, not read as ns.<member>"
          );
        }
        if (symbol !== undefined && symbol === customFetchSymbol) {
          const at = source.getLineAndCharacterOfPosition(node.getStart(source));
          const where = `${relative(root, name)}:${at.line + 1}:${at.character + 1}`;
          const value = customFetchValue(node);
          const ok = value !== undefined && refusalFetchCall(checker, value);
          if (!ok) failed++;
          lines.push(
            `${ok ? "ok" : "FAIL"}\t${where}\tcustomFetch hand-off\t${
              ok
                ? "a fetch refusalFetch made"
                : "the fetch handed to openid-client is not a call of refusalFetch"
            }`
          );
        }
        const kind = symbol === undefined ? undefined : api.get(symbol);
        if (kind !== undefined) {
          const at = source.getLineAndCharacterOfPosition(node.getStart(source));
          const where = `${relative(root, name)}:${at.line + 1}:${at.character + 1}`;
          if (!listed.has(where)) {
            listed.add(where);
            references++;
            const inside = insideBoundary(checker, node);
            const ok = inside && staysInCall(node);
            if (!ok) failed++;
            const why = ok
              ? "inside callLibrary"
              : inside
                ? "inside callLibrary but not called or passed there, so it is handed out and called outside"
                : "not inside the arrow passed alone to callLibrary";
            lines.push(`${ok ? "ok" : "FAIL"}\t${where}\t${kind}\t${why}`);
          }
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
  }
}
for (const line of lines.sort()) console.log(line);
if (apiSize === 0) {
  console.log(`UNKNOWN: no project resolves ${LIBRARY}, so there is nothing to check`);
  process.exit(2);
}
console.log(
  `API\t${LIBRARY}\t${[...apiCounts].map(([what, count]) => `${count} ${what}s`).join(", ")}`
);
console.log(
  `SUMMARY\tlibrary API=${apiSize}\tproduction files=${files}\treferences=${references}\tfailures=${failed}`
);
process.exit(failed > 0 ? 1 : 0);
