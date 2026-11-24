/**
 * The titles of a test file's `test(...)` calls, read from its syntax tree without running it,
 * so that a coverage entry's row can be held to a test that exists. A draft matrix file builds
 * most of its titles in loops (`for (const held of HELD) test(`${HOST} · … · ${held}`, …)`), so
 * a title is evaluated over every value of each loop it sits in: string and template literals,
 * the module's own constants and those it imports by a relative path, array and object literals
 * and their fields. A title built from anything else is reported as unread, not guessed.
 */
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import * as ts from "typescript";

type Value = string | number | boolean | readonly Value[] | Fields;
type Fields = { readonly [field: string]: Value };

function isList(value: Value | undefined): value is readonly Value[] {
  return Array.isArray(value);
}

function isFields(value: Value | undefined): value is Fields {
  return typeof value === "object" && !isList(value);
}

/** What a name stands for while a title is read: a loop's value, or a constant to evaluate. */
type Scope = ReadonlyMap<string, Value>;

interface Source {
  readonly path: string;
  readonly sf: ts.SourceFile;
  /** The module's top-level constants, by name, not yet evaluated. */
  readonly constants: ReadonlyMap<string, ts.Expression>;
  /** The names it imports by a relative path: the file and the name exported there. */
  readonly imports: ReadonlyMap<string, { readonly path: string; readonly name: string }>;
}

export interface TestTitles {
  readonly titles: ReadonlySet<string>;
  /** Each `test(...)`, or loop of them, whose title could not be read: its line and why. */
  readonly unread: readonly string[];
}

function resolveImport(from: string, spec: string): string | undefined {
  const base = join(dirname(from), spec);
  return [`${base}.ts`, `${base}.tsx`, join(base, "index.ts"), base].find(existsSync);
}

function readSource(path: string, sources: Map<string, Source>): Source {
  const known = sources.get(path);
  if (known !== undefined) return known;
  const kind = path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const text = readFileSync(path, "utf8");
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true, kind);
  const constants = new Map<string, ts.Expression>();
  const imports = new Map<string, { path: string; name: string }>();
  for (const statement of sf.statements) {
    if (ts.isVariableStatement(statement)) {
      for (const decl of statement.declarationList.declarations) {
        if (ts.isIdentifier(decl.name) && decl.initializer !== undefined) {
          constants.set(decl.name.text, decl.initializer);
        }
      }
    } else if (
      ts.isImportDeclaration(statement) &&
      ts.isStringLiteral(statement.moduleSpecifier) &&
      statement.moduleSpecifier.text.startsWith(".")
    ) {
      const target = resolveImport(path, statement.moduleSpecifier.text);
      const named = statement.importClause?.namedBindings;
      if (target === undefined || named === undefined || !ts.isNamedImports(named)) continue;
      for (const element of named.elements) {
        imports.set(element.name.text, {
          name: (element.propertyName ?? element.name).text,
          path: target,
        });
      }
    }
  }
  const source = { constants, imports, path, sf };
  sources.set(path, source);
  return source;
}

function evaluate(
  node: ts.Expression,
  scope: Scope,
  source: Source,
  sources: Map<string, Source>,
  depth = 0
): Value | undefined {
  if (depth > 16) return undefined;
  const again = (inner: ts.Expression, at: Source = source, innerScope: Scope = scope) =>
    evaluate(inner, innerScope, at, sources, depth + 1);
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
  if (ts.isNumericLiteral(node)) return Number(node.text);
  if (node.kind === ts.SyntaxKind.TrueKeyword) return true;
  if (node.kind === ts.SyntaxKind.FalseKeyword) return false;
  if (
    ts.isParenthesizedExpression(node) ||
    ts.isAsExpression(node) ||
    ts.isSatisfiesExpression(node) ||
    ts.isNonNullExpression(node) ||
    ts.isTypeAssertionExpression(node)
  ) {
    return again(node.expression);
  }
  if (ts.isTemplateExpression(node)) {
    let out = node.head.text;
    for (const span of node.templateSpans) {
      const part = again(span.expression);
      if (typeof part !== "string" && typeof part !== "number") return undefined;
      out += `${part}${span.literal.text}`;
    }
    return out;
  }
  if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.PlusToken) {
    const left = again(node.left);
    const right = again(node.right);
    const joinable = (value: Value | undefined) =>
      typeof value === "string" || typeof value === "number";
    return joinable(left) && joinable(right) ? `${left}${right}` : undefined;
  }
  if (ts.isArrayLiteralExpression(node)) {
    const values: Value[] = [];
    for (const element of node.elements) {
      const value = again(element);
      if (value === undefined) return undefined;
      values.push(value);
    }
    return values;
  }
  if (ts.isObjectLiteralExpression(node)) {
    const fields: Record<string, Value> = {};
    for (const property of node.properties) {
      // A field that is no value of its own (a callback, a call) is left out: a title that
      // reads it is then unread.
      const value = ts.isPropertyAssignment(property)
        ? again(property.initializer)
        : ts.isShorthandPropertyAssignment(property)
          ? again(property.name)
          : undefined;
      if (value !== undefined && property.name !== undefined) {
        fields[property.name.getText()] = value;
      }
    }
    return fields;
  }
  if (ts.isPropertyAccessExpression(node)) {
    const object = again(node.expression);
    return isFields(object) ? object[node.name.text] : undefined;
  }
  if (ts.isIdentifier(node)) {
    const bound = scope.get(node.text);
    if (bound !== undefined) return bound;
    const constant = source.constants.get(node.text);
    // A module constant is evaluated where it is declared, with no loop's values in scope.
    if (constant !== undefined) return again(constant, source, new Map());
    const imported = source.imports.get(node.text);
    if (imported === undefined) return undefined;
    const from = readSource(imported.path, sources);
    const exported = from.constants.get(imported.name);
    return exported === undefined ? undefined : again(exported, from, new Map());
  }
  return undefined;
}

/** The scope a loop's binding opens for one of its values: a name, or a destructured object. */
function bind(initializer: ts.ForInitializer, value: Value, scope: Scope): Scope | undefined {
  if (!ts.isVariableDeclarationList(initializer)) return undefined;
  const name = initializer.declarations[0]?.name;
  if (name === undefined) return undefined;
  const next = new Map(scope);
  if (ts.isIdentifier(name)) {
    next.set(name.text, value);
    return next;
  }
  if (!ts.isObjectBindingPattern(name) || !isFields(value)) return undefined;
  for (const element of name.elements) {
    if (!ts.isIdentifier(element.name)) return undefined;
    const field = value[(element.propertyName ?? element.name).getText()];
    if (field !== undefined) next.set(element.name.text, field);
  }
  return next;
}

function isTestCall(node: ts.Node): node is ts.CallExpression {
  return (
    ts.isCallExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === "test"
  );
}

/** Whether a `test(...)` call is anywhere inside `node`, so that a loop of other work is left
 *  alone however little of it the census can read. */
function holdsTest(node: ts.Node): boolean {
  return isTestCall(node) || ts.forEachChild(node, holdsTest) === true;
}

/** Reads the titles of every `test(...)` of the file at `path`. */
export function testTitles(path: string): TestTitles {
  const sources = new Map<string, Source>();
  const source = readSource(path, sources);
  const titles = new Set<string>();
  const unread: string[] = [];
  const line = (node: ts.Node) =>
    source.sf.getLineAndCharacterOfPosition(node.getStart(source.sf)).line + 1;
  const read = (statements: readonly ts.Statement[], scope: Scope) => {
    for (const statement of statements) {
      if (ts.isForOfStatement(statement)) {
        if (!holdsTest(statement.statement)) continue;
        const values = evaluate(statement.expression, scope, source, sources);
        const body = ts.isBlock(statement.statement)
          ? statement.statement.statements
          : [statement.statement];
        if (!isList(values)) {
          unread.push(`line ${line(statement)}, a loop over values the census cannot read`);
          continue;
        }
        for (const value of values) {
          const inner = bind(statement.initializer, value, scope);
          if (inner === undefined) {
            unread.push(`line ${line(statement)}, a loop binding the census cannot read`);
            break;
          }
          read(body, inner);
        }
      } else if (ts.isBlock(statement)) {
        read(statement.statements, scope);
      } else if (ts.isExpressionStatement(statement) && isTestCall(statement.expression)) {
        const [first] = statement.expression.arguments;
        const title = first === undefined ? undefined : evaluate(first, scope, source, sources);
        if (typeof title === "string") titles.add(title);
        else unread.push(`line ${line(statement)}, a title the census cannot read`);
      }
    }
  };
  read(source.sf.statements, new Map());
  return { titles, unread };
}
