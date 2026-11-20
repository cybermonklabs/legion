// packages/envoy/scripts/redact-gate/ts-structural.ts
//
// The TypeScript half of cmd/redact-gate's box and holder censuses. It reads, from stdin, the
// snapshot's root, the box (the rule's module, the box's class, the rule's function names and the
// string methods that are pure comparisons) and the workspace packages that can name the box (each
// that depends on the rule's package, and that package). It builds one program per package from
// all of the package's production files, and writes, as JSON on stdout:
//  - box: every use of the box's private storage field, classified by its immediate operation: the
//    argument of a rule call, an operand of a comparison (===, !==, ==, != or a listed comparison
//    method called on it), or the one assignment in construction; any other use fails;
//  - holders: every class, interface and type alias of those files with a property (a `#` field
//    among them) whose type holds the box, through object types, arrays, tuples, Map and Set type
//    arguments and unions, at any depth;
//  - unknown: each file that does not parse or resolve, and a census that lists nothing.

import { existsSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import ts from "typescript";

interface Request {
  readonly root: string;
  readonly box: {
    readonly file: string;
    readonly class: string;
    readonly rule: readonly string[];
    readonly comparisons: readonly string[];
    /** Calls and constructors that parse the text into a boolean, a number or a Date, and throw no
     *  text: `RegExp.prototype.test`, `new Date`. */
    readonly parses: readonly string[];
  };
  readonly packages: readonly string[];
}

interface Finding {
  file: string;
  line: number;
  function: string;
  what: string;
  kind: string;
  ok: boolean;
  why: string;
}

const request = JSON.parse(readFileSync(0, "utf8")) as Request;
const box: Finding[] = [];
const holders: Finding[] = [];
const unknown: string[] = [];
const boxFile = join(request.root, request.box.file);
const rule = new Set(request.box.rule);
const comparisons = new Set(request.box.comparisons);
const parses = new Set(request.box.parses);
let writes = 0;

const test = (f: string): boolean => f.endsWith(".test.ts") || f.includes("/__tests__/");
// Production files: a package's sources, not its tests and not its declaration files. The program
// still holds every file its tsconfig names, declaration files among them, so an ambient module
// declaration resolves as it does under the package's own `tsc`.
const production = (f: string): boolean =>
  f.endsWith(".ts") && !f.endsWith(".d.ts") && !test(f) && f.includes("/src/");

/** The enclosing function or member's name, for a finding. */
function enclosing(node: ts.Node): string {
  for (let n: ts.Node | undefined = node; n; n = n.parent) {
    if (
      (ts.isMethodDeclaration(n) || ts.isGetAccessor(n) || ts.isConstructorDeclaration(n)) &&
      ts.isClassLike(n.parent)
    ) {
      return `${n.parent.name?.text ?? "?"}.${ts.isConstructorDeclaration(n) ? "constructor" : n.name.getText()}`;
    }
    if (ts.isFunctionDeclaration(n) && n.name) return n.name.text;
  }
  return "<module>";
}

/** Whether `type` is the box's class instance type. */
function isBox(type: ts.Type): boolean {
  const symbol = type.getSymbol();
  const declaration = symbol?.declarations?.[0];
  return (
    symbol?.getName() === request.box.class &&
    declaration !== undefined &&
    declaration.getSourceFile().fileName === boxFile
  );
}

/** Whether a value of `type` holds the box, at any depth. */
function holdsBox(
  checker: ts.TypeChecker,
  type: ts.Type,
  depth: number,
  seen: Set<ts.Type>
): boolean {
  if (depth > 8 || seen.has(type)) return false;
  seen.add(type);
  if (isBox(type)) return true;
  if (type.isUnionOrIntersection())
    return type.types.some((t) => holdsBox(checker, t, depth + 1, seen));
  const args =
    (type as ts.TypeReference).typeArguments ??
    checker.getTypeArguments?.(type as ts.TypeReference) ??
    [];
  if (args.some((t) => holdsBox(checker, t, depth + 1, seen))) return true;
  if (type.flags & ts.TypeFlags.Object) {
    for (const property of checker.getPropertiesOfType(type)) {
      const declaration = property.valueDeclaration ?? property.declarations?.[0];
      if (!declaration) continue;
      if (
        holdsBox(checker, checker.getTypeOfSymbolAtLocation(property, declaration), depth + 1, seen)
      )
        return true;
    }
    // A class's # fields are no properties of its type; read its declarations.
    const declaration = type.getSymbol()?.valueDeclaration ?? type.getSymbol()?.declarations?.[0];
    if (declaration && ts.isClassLike(declaration)) {
      for (const member of declaration.members) {
        if (ts.isPropertyDeclaration(member) && ts.isPrivateIdentifier(member.name)) {
          if (holdsBox(checker, checker.getTypeAtLocation(member), depth + 1, seen)) return true;
        }
      }
    }
  }
  return false;
}

/** Classifies one use of the box's storage, `this.#text`, by its immediate operation. */
function storage(
  node: ts.PropertyAccessExpression,
  source: ts.SourceFile,
  checker: ts.TypeChecker
): void {
  const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
  const f: Finding = {
    file: relative(request.root, source.fileName),
    line,
    function: enclosing(node),
    what: `${request.box.class}.${node.name.getText()}`,
    kind: "raw",
    ok: false,
    why: "",
  };
  box.push(f);
  let child: ts.Node = node;
  let parent = node.parent;
  while (ts.isParenthesizedExpression(parent)) {
    child = parent;
    parent = parent.parent;
  }
  if (ts.isBinaryExpression(parent)) {
    const op = parent.operatorToken.kind;
    if (op === ts.SyntaxKind.EqualsToken && parent.left === child) {
      writes++;
      Object.assign(f, { kind: "write", ok: true, why: "the assignment in construction" });
      return;
    }
    if (
      [
        ts.SyntaxKind.EqualsEqualsEqualsToken,
        ts.SyntaxKind.ExclamationEqualsEqualsToken,
        ts.SyntaxKind.EqualsEqualsToken,
        ts.SyntaxKind.ExclamationEqualsToken,
      ].includes(op)
    ) {
      Object.assign(f, {
        kind: "comparison",
        ok: true,
        why: `an operand of ${parent.operatorToken.getText(source)}`,
      });
      return;
    }
  }
  if (ts.isCallExpression(parent) && parent.arguments.some((a) => a === child)) {
    const callee = checker.getSymbolAtLocation(
      ts.isPropertyAccessExpression(parent.expression) ? parent.expression.name : parent.expression
    );
    const resolved =
      callee && callee.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(callee) : callee;
    const name = resolved?.getName() ?? "";
    const declaredIn = resolved?.declarations?.[0]?.getSourceFile().fileName;
    if (rule.has(name) && declaredIn === boxFile) {
      Object.assign(f, { kind: "rule", ok: true, why: `the argument of ${name}` });
      return;
    }
    if (
      parses.has(name) &&
      declaredIn !== undefined &&
      /\/typescript\/lib\/lib\.[^/]*\.d\.ts$/.test(declaredIn)
    ) {
      Object.assign(f, {
        kind: "parse",
        ok: true,
        why: `the argument of ${name}, which answers no text and throws none`,
      });
      return;
    }
    f.why = `the text is the argument of ${name || "a call"}, which is no rule and no parse`;
    return;
  }
  if (ts.isNewExpression(parent) && parent.arguments?.some((a) => a === child)) {
    const name = parent.expression.getText(source);
    if (
      parses.has(`new ${name}`) &&
      checker
        .getSymbolAtLocation(parent.expression)
        ?.declarations?.some((d) =>
          /\/typescript\/lib\/lib\.[^/]*\.d\.ts$/.test(d.getSourceFile().fileName)
        )
    ) {
      Object.assign(f, {
        kind: "parse",
        ok: true,
        why: `the argument of new ${name}, which answers a parsed value and throws no text`,
      });
      return;
    }
    f.why = `the text is the argument of new ${name}, which is no parse`;
    return;
  }
  if (
    ts.isPropertyAccessExpression(parent) &&
    parent.expression === child &&
    ts.isCallExpression(parent.parent) &&
    parent.parent.expression === parent
  ) {
    const method = parent.name.getText(source);
    if (comparisons.has(method)) {
      Object.assign(f, {
        kind: "comparison",
        ok: true,
        why: `the receiver of ${method}, which answers a boolean`,
      });
      return;
    }
    f.why = `the text is the receiver of ${method}, which is no comparison`;
    return;
  }
  f.why = `the text reaches a ${ts.SyntaxKind[parent.kind]}`;
}

for (const pkg of request.packages) {
  const dir = join(request.root, pkg);
  const configPath = join(dir, "tsconfig.json");
  if (!existsSync(configPath)) {
    unknown.push(`${pkg}: no tsconfig.json`);
    continue;
  }
  const config = ts.readConfigFile(configPath, ts.sys.readFile);
  if (config.error) {
    unknown.push(`${pkg}: its tsconfig.json cannot be read`);
    continue;
  }
  // The config file's own path, so `types` and `typeRoots` resolve from the package, as they do
  // under its own `tsc`.
  const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, dir, undefined, configPath);
  const sources = parsed.fileNames.filter(production);
  if (pkg === "packages/contracts" && !sources.includes(boxFile)) sources.push(boxFile);
  const program = ts.createProgram({
    rootNames: [...parsed.fileNames, ...sources],
    options: { ...parsed.options, noEmit: true },
  });
  const checker = program.getTypeChecker();
  for (const fileName of sources) {
    const source = program.getSourceFile(fileName);
    const rel = relative(request.root, fileName);
    if (!source) {
      unknown.push(`${rel}: the compiler did not load it`);
      continue;
    }
    const syntax = program.getSyntacticDiagnostics(source);
    if (syntax.length > 0) {
      unknown.push(
        `${rel}: cannot be parsed: ${ts.flattenDiagnosticMessageText(syntax[0]?.messageText ?? "", " ")}`
      );
      continue;
    }
    const semantic = program
      .getSemanticDiagnostics(source)
      .filter((d) => d.category === ts.DiagnosticCategory.Error);
    if (semantic.length > 0) {
      unknown.push(
        `${rel}: cannot be resolved: ${ts.flattenDiagnosticMessageText(semantic[0]?.messageText ?? "", " ")}`
      );
      continue;
    }
    const visit = (node: ts.Node): void => {
      if (ts.isPropertyAccessExpression(node) && ts.isPrivateIdentifier(node.name)) {
        const symbol = checker.getSymbolAtLocation(node.name);
        const owner = symbol?.valueDeclaration?.parent;
        if (
          owner &&
          ts.isClassDeclaration(owner) &&
          owner.name?.text === request.box.class &&
          owner.getSourceFile().fileName === boxFile
        ) {
          storage(node, source, checker);
        }
      }
      const named =
        ts.isClassDeclaration(node) ||
        ts.isInterfaceDeclaration(node) ||
        ts.isTypeAliasDeclaration(node);
      if (
        named &&
        node.name &&
        !(
          ts.isClassDeclaration(node) &&
          node.name.text === request.box.class &&
          fileName === boxFile
        )
      ) {
        const type = checker.getTypeAtLocation(node.name);
        const instance = ts.isClassDeclaration(node)
          ? checker.getDeclaredTypeOfSymbol(checker.getSymbolAtLocation(node.name) as ts.Symbol)
          : type;
        if (holdsBox(checker, instance, 0, new Set())) {
          const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
          holders.push({
            file: rel,
            line,
            function: "<module>",
            what: `${rel}#${node.name.text}`,
            kind: "holder",
            ok: true,
            why: "",
          });
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
  }
}
if (writes > 1)
  unknown.push(
    `the box's storage is assigned ${writes} times; the box has one assignment, in construction`
  );

process.stdout.write(JSON.stringify({ box, holders, unknown }));
