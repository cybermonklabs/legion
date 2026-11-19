/**
 * The TypeScript half of the opener search: every place a TypeScript or JavaScript file git tracks
 * opens a Postgres connection string, read from each file's syntax.
 *
 * An opener is a use of what an opener module exports: `SQL` and `sql` of `bun` (and `Bun.SQL`,
 * `Bun.sql`), and anything of `postgres`, `pg`, `pg-promise`, `slonik`, `@neondatabase/serverless`
 * or `@vercel/postgres`. It follows each such name through its import (named, default or
 * namespace), a `const` alias and a destructuring, and lists each `new`, call and tagged template
 * of it, each mention of it as a value (handed on, put in an array), and each `require` or
 * `import()` of such a module. A call is judged by the URL it is handed: a string literal, or a
 * `const` of one in the same file, holds no setting; any other value, and `sql` of `bun`, which
 * reads the environment's DATABASE_URL, is unjudged, and `db-openers.judged.ts` says why it cannot
 * print the value or the check fails. A name in a type is no use.
 *
 * It reads syntax only: a value handed through a function to an opener in another file is listed
 * where the opener is called, with the function's parameter as its argument.
 */
import ts from "typescript";

/** One listed use of an opener, in the shape the Go half prints a call: its kind is "end" (a
 *  call), "wrapper" (a call in a wrapper, judged at each call of it), "uncalled" (one in a wrapper
 *  no file read calls), "value" (named and not called) or "load" (a module loaded at run time).
 *  Its callee and its argument are their whole source text with the white space folded, since a
 *  judgement of the list names the call by them; `short` cuts them only for printing. */
export interface Listed {
  readonly file: string;
  readonly line: number;
  readonly function: string;
  readonly callee: string;
  readonly argument: string;
  readonly kind: string;
  readonly verdict: "judged" | "unjudged" | "wrapper";
  readonly why: string;
}

/** The modules whose exports open a Postgres connection, and of each, the exports that do. */
const OPENER_MODULES: Record<string, readonly string[] | "every"> = {
  bun: ["SQL", "sql"],
  postgres: "every",
  pg: "every",
  "pg-promise": "every",
  slonik: "every",
  "@neondatabase/serverless": "every",
  "@vercel/postgres": "every",
};

/** What a name of a file stands for: an opener itself, or a module (a namespace or default
 *  import, `Bun`) whose members are openers as `members` says. */
type Binding =
  | { readonly kind: "opener"; readonly callee: string }
  | {
      readonly kind: "module";
      readonly module: string;
      readonly members: readonly string[] | "every";
    };

/** Whether `members` holds `member`. */
const holds = (members: readonly string[] | "every", member: string) =>
  members === "every" || members.includes(member);

/** text cut to 120 characters, for printing. */
export const short = (text: string) => (text.length > 120 ? `${text.slice(0, 120)}…` : text);

/** text with each run of white space folded to one space, as both halves print a call. */
export const folded = (text: string) => text.replace(/\s+/g, " ").trim();

/** Every opener use in `files`, each a path relative to the root with its text. */
export function typescriptOpeners(files: ReadonlyMap<string, string>): Listed[] {
  const listed: Listed[] = [];
  for (const [file, text] of files) listed.push(...fileOpeners(file, text));
  return listed;
}

function scriptKind(file: string): ts.ScriptKind {
  if (/\.tsx$/.test(file)) return ts.ScriptKind.TSX;
  if (/\.jsx$/.test(file)) return ts.ScriptKind.JSX;
  if (/\.[cm]?js$/.test(file)) return ts.ScriptKind.JS;
  return ts.ScriptKind.TS;
}

function fileOpeners(file: string, text: string): Listed[] {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, scriptKind(file));
  const bindings = new Map<string, Binding>([
    ["Bun", { kind: "module", module: "bun", members: ["SQL", "sql"] }],
  ]);
  const constants = new Map<string, ts.Expression>();
  const listed: Listed[] = [];
  const at = (node: ts.Node) => source.getLineAndCharacterOfPosition(node.getStart()).line + 1;
  const textOf = (node: ts.Node) => folded(node.getText(source));
  const enclosing = (node: ts.Node): string => {
    for (let n: ts.Node | undefined = node.parent; n !== undefined; n = n.parent) {
      if (
        (ts.isFunctionDeclaration(n) || ts.isMethodDeclaration(n) || ts.isFunctionExpression(n)) &&
        n.name !== undefined
      )
        return n.name.getText(source);
      if (ts.isArrowFunction(n) || ts.isFunctionExpression(n)) {
        const parent = n.parent;
        if (ts.isVariableDeclaration(parent)) return parent.name.getText(source);
        if (ts.isPropertyAssignment(parent)) return parent.name.getText(source);
      }
    }
    return "(top level)";
  };

  // The imports, then the aliases, until a pass finds no new name.
  for (const statement of source.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier))
      continue;
    const module = statement.moduleSpecifier.text;
    const members = OPENER_MODULES[module];
    if (members === undefined || !Object.hasOwn(OPENER_MODULES, module)) continue;
    const clause = statement.importClause;
    if (clause === undefined || clause.isTypeOnly) continue;
    if (clause.name !== undefined)
      bindings.set(
        clause.name.text,
        members === "every"
          ? { kind: "opener", callee: `${module} default` }
          : { kind: "module", module, members }
      );
    const named = clause.namedBindings;
    if (named !== undefined && ts.isNamespaceImport(named))
      bindings.set(named.name.text, { kind: "module", module, members });
    if (named !== undefined && ts.isNamedImports(named))
      for (const element of named.elements) {
        if (element.isTypeOnly) continue;
        const exported = (element.propertyName ?? element.name).text;
        if (holds(members, exported))
          bindings.set(element.name.text, { kind: "opener", callee: `${module} ${exported}` });
      }
  }
  const reference = (expression: ts.Expression): Binding | undefined => {
    const e = unwrap(expression);
    if (ts.isIdentifier(e)) return bindings.get(e.text);
    if (ts.isPropertyAccessExpression(e)) {
      const object = reference(e.expression);
      const member = e.name.text;
      if (object?.kind === "opener" && object.callee.endsWith(" default"))
        return { kind: "opener", callee: `${object.callee} ${member}` };
      if (object?.kind === "module" && holds(object.members, member))
        return { kind: "opener", callee: `${object.module} ${member}` };
    }
    return undefined;
  };
  for (let grew = true; grew; ) {
    grew = false;
    const visit = (node: ts.Node): void => {
      if (ts.isVariableDeclaration(node) && node.initializer !== undefined) {
        const list = node.parent;
        const constant =
          ts.isVariableDeclarationList(list) && (list.flags & ts.NodeFlags.Const) !== 0;
        if (ts.isIdentifier(node.name)) {
          if (constant && !constants.has(node.name.text))
            constants.set(node.name.text, node.initializer);
          const target = reference(node.initializer);
          if (target !== undefined && !bindings.has(node.name.text)) {
            bindings.set(node.name.text, target);
            grew = true;
          }
        } else if (ts.isObjectBindingPattern(node.name)) {
          const object = reference(node.initializer);
          if (object?.kind === "module")
            for (const element of node.name.elements) {
              const member = (element.propertyName ?? element.name).getText(source);
              if (!ts.isIdentifier(element.name) || bindings.has(element.name.text)) continue;
              if (holds(object.members, member)) {
                bindings.set(element.name.text, {
                  kind: "opener",
                  callee: `${object.module} ${member}`,
                });
                grew = true;
              }
            }
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
  }

  const judge = (argument: ts.Expression | undefined, depth = 0): [boolean, string] => {
    if (argument === undefined)
      return [false, "no URL: the client reads the environment's DATABASE_URL"];
    const e = unwrap(argument);
    if (ts.isStringLiteral(e) || ts.isNoSubstitutionTemplateLiteral(e)) return [true, "a constant"];
    if (ts.isIdentifier(e) && depth < 8) {
      const value = constants.get(e.text);
      if (value !== undefined) return judge(value, depth + 1);
    }
    if (ts.isObjectLiteralExpression(e) && depth < 8) {
      const parts = e.properties.map((p) =>
        ts.isPropertyAssignment(p) ? judge(p.initializer, depth + 1) : [false, ""]
      );
      if (parts.every(([ok]) => ok)) return [true, "an object of constants"];
    }
    return [false, `the value ${short(textOf(e))}`];
  };

  const used = new Set<ts.Node>();
  const visit = (node: ts.Node): void => {
    if (ts.isTypeNode(node)) return;
    if (ts.isCallExpression(node) && node.arguments.length > 0) {
      const callee = unwrap(node.expression);
      const first = node.arguments[0];
      const loads =
        (callee.kind === ts.SyntaxKind.ImportKeyword ||
          (ts.isIdentifier(callee) && callee.text === "require")) &&
        first !== undefined &&
        ts.isStringLiteralLike(first) &&
        Object.hasOwn(OPENER_MODULES, first.text);
      if (loads) {
        listed.push({
          file,
          line: at(node),
          function: enclosing(node),
          callee: textOf(node),
          argument: "",
          kind: "load",
          verdict: "unjudged",
          why: "a load of an opener module at run time, whose uses this search does not follow",
        });
      }
    }
    if (
      ts.isNewExpression(node) ||
      ts.isCallExpression(node) ||
      ts.isTaggedTemplateExpression(node)
    ) {
      const callee = ts.isTaggedTemplateExpression(node) ? node.tag : node.expression;
      const target = reference(callee);
      if (target?.kind === "opener") {
        markUsed(callee, used);
        const tagged = ts.isTaggedTemplateExpression(node);
        const argument = tagged ? undefined : node.arguments?.[0];
        const defaultClient = target.callee === "bun sql";
        const [ok, why] = defaultClient
          ? [false, "Bun's default client, which reads the environment's DATABASE_URL"]
          : judge(argument);
        listed.push({
          file,
          line: at(node),
          function: enclosing(node),
          callee: target.callee,
          argument: argument === undefined ? "" : textOf(argument),
          kind: "end",
          verdict: ok ? "judged" : "unjudged",
          why,
        });
      }
    }
    if (
      (ts.isIdentifier(node) || ts.isPropertyAccessExpression(node)) &&
      !used.has(node) &&
      !isDeclarationName(node) &&
      !(ts.isVariableDeclaration(node.parent) && node.parent.initializer === node)
    ) {
      const target = reference(node);
      if (target?.kind === "opener" && !inImport(node)) {
        // Bun's default client opens the environment's DATABASE_URL on any use, a method's too.
        const defaultClient = target.callee === "bun sql";
        listed.push({
          file,
          line: at(node),
          function: enclosing(node),
          callee: target.callee,
          argument: textOf(node),
          kind: defaultClient ? "end" : "value",
          verdict: "unjudged",
          why: defaultClient
            ? "Bun's default client, which reads the environment's DATABASE_URL"
            : "named as a value, so what it is later handed is not read",
        });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return listed;
}

/** e without parentheses, `as`, `satisfies` and `!`. */
function unwrap(e: ts.Expression): ts.Expression {
  for (;;) {
    if (ts.isParenthesizedExpression(e) || ts.isAsExpression(e) || ts.isNonNullExpression(e))
      e = e.expression;
    else if (ts.isSatisfiesExpression(e)) e = e.expression;
    else return e;
  }
}

/** Marks `callee` and what it is read through as a use, so none is listed as a value again. */
function markUsed(callee: ts.Expression, used: Set<ts.Node>): void {
  for (let e: ts.Node = callee; ; ) {
    used.add(e);
    if (ts.isParenthesizedExpression(e) || ts.isAsExpression(e) || ts.isNonNullExpression(e))
      e = e.expression;
    else if (ts.isPropertyAccessExpression(e)) e = e.expression;
    else return;
  }
}

/** Whether node is a name that declares or keys something rather than using it: the name a
 *  declaration declares (a variable, a parameter, a property, a function), a key of a
 *  destructuring, or a member's name after a dot. */
function isDeclarationName(node: ts.Node): boolean {
  const parent = node.parent;
  if (parent === undefined) return false;
  if (ts.isBindingElement(parent) && parent.propertyName === node) return true;
  if (ts.isPropertyAccessExpression(parent) && parent.name === node) return true;
  return (
    (ts.isVariableDeclaration(parent) ||
      ts.isParameter(parent) ||
      ts.isPropertyAssignment(parent) ||
      ts.isPropertyDeclaration(parent) ||
      ts.isPropertySignature(parent) ||
      ts.isFunctionDeclaration(parent) ||
      ts.isMethodDeclaration(parent) ||
      ts.isBindingElement(parent)) &&
    parent.name === node
  );
}

function inImport(node: ts.Node): boolean {
  for (let n: ts.Node | undefined = node; n !== undefined; n = n.parent)
    if (ts.isImportDeclaration(n) || ts.isExportDeclaration(n)) return true;
  return false;
}
