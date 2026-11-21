/**
 * The composer census (INFRA-1503), its first part: the program it reads. Every module under the
 * root it is given, tests left out, parsed into syntax trees (no type checker), its imports
 * resolved by path, and its components and their uses indexed, so that the census sees exactly
 * the files under that root. `trees.ts`, with its walk up (`walk.ts`), `forms.ts` and
 * `lifetime.ts` read the census's items out of it, and `census.ts` names them.
 *
 * The census lives here. It began as a scratch script outside the repository (version 2 at
 * dispatch://INFRA-1503/artifact/composer-census-ts), which this module replaces as its source,
 * so that `composer-census.test.ts` runs it at every head.
 */
import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import * as ts from "typescript";

/** A test, a test helper, a fixture or a coverage list, which the census leaves out. */
export function isTestPath(rel: string): boolean {
  return (
    /\.test\.tsx?$/.test(rel) ||
    /(^|\/)__tests__\//.test(rel) ||
    /fixture/i.test(rel) ||
    /\.coverage\.ts$/.test(rel) ||
    rel.endsWith(".d.ts")
  );
}

function listFiles(root: string, dir: string, out: string[]): string[] {
  // Sorted, so that the census meets the mounts and the places they are drawn in one order on
  // every machine.
  for (const name of readdirSync(dir).sort()) {
    const full = join(dir, name);
    if (statSync(full).isDirectory()) listFiles(root, full, out);
    else if (/\.tsx?$/.test(name) && !isTestPath(relative(root, full))) out.push(full);
  }
  return out;
}

interface Binding {
  readonly from: string | undefined;
  readonly name: string;
  /** An import's module as the import names it, a library's where `from` is undefined. */
  readonly spec?: string;
}

/** A module, by its path under the root, which is also its syntax tree's file name. */
export interface Mod {
  readonly rel: string;
  readonly sf: ts.SourceFile;
  readonly imports: Map<string, Binding>;
  readonly decls: Map<string, ts.Node>;
  readonly reexports: Map<string, Binding>;
  readonly stars: string[];
}

/** A component, or a hook read as one: a top-level function of a module, or a class component
 *  read through its `render`. */
export interface Comp {
  readonly id: string;
  readonly name: string;
  readonly mod: Mod;
  readonly fn: ts.SignatureDeclaration & { body?: ts.Node };
  /** A class component's class, whose `render` is `fn`: its props are `this.props`, and its
   *  state `this.state`. */
  readonly cls?: ts.ClassDeclaration;
}

/** One place a component is drawn: a JSX element whose tag names it. */
export interface Use {
  readonly element: ts.Node;
  readonly mod: Mod;
}

export interface Program {
  readonly mods: ReadonlyMap<string, Mod>;
  readonly comps: ReadonlyMap<string, Comp>;
  readonly compByFn: ReadonlyMap<ts.Node, Comp>;
  readonly usesById: ReadonlyMap<string, readonly Use[]>;
}

function resolveSpec(
  mods: ReadonlyMap<string, Mod>,
  from: string,
  spec: string
): string | undefined {
  if (!spec.startsWith(".")) return undefined;
  const base = join(dirname(from), spec);
  for (const candidate of [
    base,
    `${base}.tsx`,
    `${base}.ts`,
    join(base, "index.tsx"),
    join(base, "index.ts"),
  ]) {
    if (mods.has(candidate)) return candidate;
  }
  return undefined;
}

function readStatements(mods: ReadonlyMap<string, Mod>, mod: Mod): void {
  for (const statement of mod.sf.statements) {
    if (ts.isImportDeclaration(statement) && ts.isStringLiteral(statement.moduleSpecifier)) {
      const spec = statement.moduleSpecifier.text;
      const from = resolveSpec(mods, mod.rel, spec);
      const clause = statement.importClause;
      if (clause?.name !== undefined) {
        mod.imports.set(clause.name.text, { from, name: "default", spec });
      }
      const named = clause?.namedBindings;
      if (named !== undefined && ts.isNamedImports(named)) {
        for (const element of named.elements) {
          mod.imports.set(element.name.text, {
            from,
            name: (element.propertyName ?? element.name).text,
            spec,
          });
        }
      }
    } else if (ts.isExportDeclaration(statement)) {
      const from =
        statement.moduleSpecifier !== undefined && ts.isStringLiteral(statement.moduleSpecifier)
          ? resolveSpec(mods, mod.rel, statement.moduleSpecifier.text)
          : undefined;
      if (statement.exportClause === undefined) {
        if (from !== undefined) mod.stars.push(from);
      } else if (ts.isNamedExports(statement.exportClause)) {
        for (const element of statement.exportClause.elements) {
          mod.reexports.set(element.name.text, {
            from,
            name: (element.propertyName ?? element.name).text,
          });
        }
      }
    } else if (ts.isFunctionDeclaration(statement) && statement.name !== undefined) {
      mod.decls.set(statement.name.text, statement);
    } else if (ts.isVariableStatement(statement)) {
      for (const declaration of statement.declarationList.declarations) {
        if (ts.isIdentifier(declaration.name)) mod.decls.set(declaration.name.text, declaration);
      }
    } else if (ts.isClassDeclaration(statement) && statement.name !== undefined) {
      mod.decls.set(statement.name.text, statement);
    }
  }
}

/** The function a top-level declaration holds, through `memo(...)` and `forwardRef(...)`. */
export function componentFunction(node: ts.Node): Comp["fn"] | undefined {
  if (ts.isFunctionDeclaration(node) && node.body !== undefined) return node;
  if (!ts.isVariableDeclaration(node) || node.initializer === undefined) return undefined;
  let init: ts.Expression = node.initializer;
  while (ts.isCallExpression(init) && init.arguments.length > 0) {
    const callee = init.expression.getText();
    if (!/(^|\.)(memo|forwardRef)$/.test(callee)) break;
    init = init.arguments[0];
  }
  if (ts.isArrowFunction(init) || ts.isFunctionExpression(init)) return init;
  return undefined;
}

/** A class component's `render`: a top-level class with a `render` method. */
function classRender(node: ts.Node): ts.MethodDeclaration | undefined {
  if (!ts.isClassDeclaration(node)) return undefined;
  return node.members.find(
    (member): member is ts.MethodDeclaration =>
      ts.isMethodDeclaration(member) &&
      member.name.getText() === "render" &&
      member.body !== undefined
  );
}

/** Reads every module under `root`, tests left out, with its components and their uses. */
export function readProgram(root: string): Program {
  const mods = new Map<string, Mod>();
  for (const path of listFiles(root, root, [])) {
    const rel = relative(root, path);
    const sf = ts.createSourceFile(
      rel,
      readFileSync(path, "utf8"),
      ts.ScriptTarget.Latest,
      true,
      rel.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
    );
    mods.set(rel, {
      decls: new Map(),
      imports: new Map(),
      reexports: new Map(),
      rel,
      sf,
      stars: [],
    });
  }
  for (const mod of mods.values()) readStatements(mods, mod);

  const comps = new Map<string, Comp>();
  const compByFn = new Map<ts.Node, Comp>();
  const usesById = new Map<string, Use[]>();
  const program: Program = { compByFn, comps, mods, usesById };
  for (const mod of mods.values()) {
    for (const [name, decl] of mod.decls) {
      if (!/^[A-Z]/.test(name)) continue;
      const render = classRender(decl);
      const fn = render ?? componentFunction(decl);
      if (fn === undefined) continue;
      const id = `${mod.rel}#${name}`;
      const comp: Comp =
        render === undefined
          ? { fn, id, mod, name }
          : { cls: decl as ts.ClassDeclaration, fn, id, mod, name };
      comps.set(comp.id, comp);
      compByFn.set(fn, comp);
    }
  }
  for (const mod of mods.values()) {
    const visit = (node: ts.Node) => {
      if (ts.isJsxSelfClosingElement(node) || ts.isJsxElement(node)) {
        const id = tagComp(program, mod, node);
        if (id !== undefined) {
          const list = usesById.get(id) ?? [];
          list.push({ element: node, mod });
          usesById.set(id, list);
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(mod.sf);
  }
  return program;
}

/** The module and the declaration a local name of `mod` stands for: its own top-level one, or
 *  the one it imports by name. */
function declNamed(
  program: Program,
  mod: Mod,
  local: string
): { readonly decl: ts.Node; readonly mod: Mod } | undefined {
  const own = mod.decls.get(local);
  if (own !== undefined) return { decl: own, mod };
  const imported = mod.imports.get(local);
  const from = imported?.from === undefined ? undefined : program.mods.get(imported.from);
  const decl = from?.decls.get(imported?.name ?? "");
  return from === undefined || decl === undefined ? undefined : { decl, mod: from };
}

/** The `import("…")` literal under `node`, with the module it sits in. */
function importUnder(
  mod: Mod,
  node: ts.Node
): { readonly mod: Mod; readonly spec: string } | undefined {
  let spec: string | undefined;
  const visit = (current: ts.Node) => {
    if (
      spec === undefined &&
      ts.isCallExpression(current) &&
      current.expression.kind === ts.SyntaxKind.ImportKeyword &&
      current.arguments.length > 0 &&
      ts.isStringLiteral(current.arguments[0])
    ) {
      spec = current.arguments[0].text;
    }
    ts.forEachChild(current, visit);
  };
  visit(node);
  return spec === undefined ? undefined : { mod, spec };
}

/** The module a lazy constant's loader imports: the `import()` written in it, or the one in the
 *  loader it calls - a constant of its own module or another's (`importIssuePage()`,
 *  `importInbox()`), or a property of such a constant object (`issueTabChunks.conversation()`),
 *  each of which holds the one `import()` of its chunk. */
function loadedImport(
  program: Program,
  mod: Mod,
  init: ts.CallExpression
): { readonly mod: Mod; readonly spec: string } | undefined {
  // The loader alone, `lazy(…)`'s or `chunkComponent(…)`'s first argument: the call itself names
  // the wrapper, whose module is no chunk's.
  const loader = init.arguments[0];
  if (loader === undefined) return undefined;
  const inline = importUnder(mod, loader);
  if (inline !== undefined) return inline;
  let found: { readonly mod: Mod; readonly spec: string } | undefined;
  const visit = (node: ts.Node) => {
    if (found !== undefined) return;
    if (ts.isCallExpression(node)) {
      const callee = node.expression;
      if (ts.isIdentifier(callee)) {
        const named = declNamed(program, mod, callee.text);
        if (named !== undefined) found = importUnder(named.mod, named.decl);
      } else if (ts.isPropertyAccessExpression(callee) && ts.isIdentifier(callee.expression)) {
        const named = declNamed(program, mod, callee.expression.text);
        let object =
          named !== undefined && ts.isVariableDeclaration(named.decl)
            ? named.decl.initializer
            : undefined;
        while (
          object !== undefined &&
          (ts.isSatisfiesExpression(object) || ts.isAsExpression(object))
        ) {
          object = object.expression;
        }
        const property =
          object !== undefined && ts.isObjectLiteralExpression(object)
            ? object.properties.find(
                (each) => ts.isPropertyAssignment(each) && each.name.getText() === callee.name.text
              )
            : undefined;
        if (named !== undefined && property !== undefined) found = importUnder(named.mod, property);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(loader);
  return found;
}

/** A constant that loads a component from a chunk of its own: `lazy(() =>
 *  import("./x").then((module) => ({ default: module.Name })))`, or this tree's
 *  `chunkComponent(() => load().then((module) => module.Name), …)`
 *  (`components/chunk-component.tsx`), whose loader may be a constant of its own. Read as the
 *  component it loads, so a tag of it is a use of that component. */
function lazyTarget(program: Program, mod: Mod, decl: ts.Node): string | undefined {
  if (!ts.isVariableDeclaration(decl) || decl.initializer === undefined) return undefined;
  const init = decl.initializer;
  if (!ts.isCallExpression(init)) return undefined;
  const callee = init.expression.getText();
  if (callee !== "lazy" && callee !== "chunkComponent") return undefined;
  const names: string[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isPropertyAccessExpression(node)) names.push(node.name.text);
    ts.forEachChild(node, visit);
  };
  visit(init);
  const loaded = loadedImport(program, mod, init);
  const from =
    loaded === undefined ? undefined : resolveSpec(program.mods, loaded.mod.rel, loaded.spec);
  const target = from === undefined ? undefined : program.mods.get(from);
  if (target === undefined) return undefined;
  for (const name of names) {
    const id = resolveExport(program, target, name, 0);
    if (id !== undefined) return id;
  }
  return undefined;
}

function resolveExport(
  program: Program,
  mod: Mod,
  name: string,
  depth: number
): string | undefined {
  if (depth > 8) return undefined;
  const decl = mod.decls.get(name);
  if (decl !== undefined) return lazyTarget(program, mod, decl) ?? `${mod.rel}#${name}`;
  const re = mod.reexports.get(name);
  if (re !== undefined) {
    if (re.from === undefined) return resolveLocal(program, mod, re.name, depth + 1);
    const target = program.mods.get(re.from);
    return target === undefined ? undefined : resolveExport(program, target, re.name, depth + 1);
  }
  for (const star of mod.stars) {
    const target = program.mods.get(star);
    const id = target === undefined ? undefined : resolveExport(program, target, name, depth + 1);
    if (id !== undefined) return id;
  }
  return undefined;
}

/** The declaration a name of `mod` stands for, by its id (`<module>#<name>`). */
export function resolveLocal(
  program: Program,
  mod: Mod,
  local: string,
  depth = 0
): string | undefined {
  const decl = mod.decls.get(local);
  if (decl !== undefined) return lazyTarget(program, mod, decl) ?? `${mod.rel}#${local}`;
  const imported = mod.imports.get(local);
  if (imported?.from === undefined) return undefined;
  const target = program.mods.get(imported.from);
  return target === undefined
    ? undefined
    : resolveExport(program, target, imported.name, depth + 1);
}

/** The declaration an id (`<module>#<name>`) names. */
export function declOf(program: Program, id: string | undefined): ts.Node | undefined {
  if (id === undefined) return undefined;
  const at = id.lastIndexOf("#");
  return program.mods.get(id.slice(0, at))?.decls.get(id.slice(at + 1));
}

export function tagOf(element: ts.Node): ts.JsxTagNameExpression | undefined {
  if (ts.isJsxSelfClosingElement(element)) return element.tagName;
  if (ts.isJsxElement(element)) return element.openingElement.tagName;
  if (ts.isJsxOpeningElement(element)) return element.tagName;
  return undefined;
}

export function attributesOf(element: ts.Node): ts.JsxAttributes | undefined {
  if (ts.isJsxSelfClosingElement(element)) return element.attributes;
  if (ts.isJsxElement(element)) return element.openingElement.attributes;
  if (ts.isJsxOpeningElement(element)) return element.attributes;
  return undefined;
}

export function attribute(element: ts.Node, name: string): ts.JsxAttribute | undefined {
  for (const property of attributesOf(element)?.properties ?? []) {
    if (ts.isJsxAttribute(property) && property.name.getText() === name) return property;
  }
  return undefined;
}

export function spreads(element: ts.Node): ts.JsxSpreadAttribute[] {
  const properties: readonly ts.JsxAttributeLike[] = attributesOf(element)?.properties ?? [];
  return properties.filter((property): property is ts.JsxSpreadAttribute =>
    ts.isJsxSpreadAttribute(property)
  );
}

/** The component a JSX tag names, by its id, or undefined for an element or a library one. */
export function tagComp(program: Program, mod: Mod, element: ts.Node): string | undefined {
  const tag = tagOf(element);
  if (tag === undefined || !ts.isIdentifier(tag) || !/^[A-Z]/.test(tag.text)) return undefined;
  return resolveLocal(program, mod, tag.text);
}

/** The top-level component a node sits in: only top-level component functions and class
 *  components' `render`s are indexed, so the first one found going up is it. */
export function compOf(program: Program, node: ts.Node): Comp | undefined {
  let current: ts.Node | undefined = node;
  while (current !== undefined) {
    const comp = program.compByFn.get(current);
    if (comp !== undefined) return comp;
    current = current.parent;
  }
  return undefined;
}

/** A top-level function or constant of a module, which a component the module's own or an
 *  importer's uses as a helper. */
export type ModuleHelper = ts.FunctionDeclaration | ts.VariableDeclaration;

/** Whether `node` is a declaration of a named top-level function or constant. */
export function isModuleHelper(node: ts.Node): node is ModuleHelper {
  if (ts.isFunctionDeclaration(node)) {
    return node.name !== undefined && ts.isSourceFile(node.parent);
  }
  return (
    ts.isVariableDeclaration(node) &&
    ts.isIdentifier(node.name) &&
    ts.isVariableStatement(node.parent.parent) &&
    ts.isSourceFile(node.parent.parent.parent)
  );
}

/** The top-level function or constant a node outside every component sits in, if any. */
function moduleHelperOf(node: ts.Node): ModuleHelper | undefined {
  let found: ModuleHelper | undefined;
  for (let current: ts.Node = node; !ts.isSourceFile(current); current = current.parent) {
    if (isModuleHelper(current)) found = current;
  }
  return found;
}

/** A module helper's name. */
export function helperName(helper: ModuleHelper): string {
  return helper.name?.getText() ?? "";
}

/** A module helper's id, which its module's components and its importers resolve it by. */
export function helperId(program: Program, helper: ModuleHelper): string {
  return `${modOf(program, helper).rel}#${helperName(helper)}`;
}

/** Whether an identifier names something where it stands, rather than reading a value: a
 *  declaration, a property, an attribute or a tag. */
function namesHere(node: ts.Identifier): boolean {
  const parent = node.parent;
  if (ts.isPropertyAccessExpression(parent)) return parent.name === node;
  if (
    ts.isJsxOpeningElement(parent) ||
    ts.isJsxSelfClosingElement(parent) ||
    ts.isJsxClosingElement(parent)
  ) {
    return parent.tagName === node;
  }
  if (ts.isBindingElement(parent)) return parent.name === node || parent.propertyName === node;
  return (
    (ts.isPropertyAssignment(parent) ||
      ts.isJsxAttribute(parent) ||
      ts.isParameter(parent) ||
      ts.isVariableDeclaration(parent) ||
      ts.isFunctionDeclaration(parent) ||
      ts.isFunctionExpression(parent) ||
      ts.isClassDeclaration(parent) ||
      ts.isClassExpression(parent) ||
      ts.isMethodDeclaration(parent) ||
      ts.isPropertyDeclaration(parent) ||
      ts.isGetAccessorDeclaration(parent) ||
      ts.isSetAccessorDeclaration(parent)) &&
    parent.name === node
  );
}

const refsMemo = new WeakMap<Program, Map<string, ts.Identifier[]>>();

/** Every identifier of the program that reads the top-level declaration `id` (`<module>#<name>`)
 *  as a value, under whatever name its module knows it by: its own module's and its importers'.
 *  A JSX tag is no such read: a component's tags are its uses (`Program.usesById`). */
export function moduleRefs(program: Program, id: string): readonly ts.Identifier[] {
  const memo = refsMemo.get(program) ?? new Map<string, ts.Identifier[]>();
  refsMemo.set(program, memo);
  const known = memo.get(id);
  if (known !== undefined) return known;
  const out: ts.Identifier[] = [];
  for (const mod of program.mods.values()) {
    const names = new Set(
      [...mod.decls.keys(), ...mod.imports.keys()].filter(
        (name) => resolveLocal(program, mod, name) === id
      )
    );
    if (names.size === 0) continue;
    const visit = (node: ts.Node) => {
      if (
        ts.isTypeNode(node) ||
        ts.isImportDeclaration(node) ||
        ts.isExportDeclaration(node) ||
        ts.isInterfaceDeclaration(node) ||
        ts.isTypeAliasDeclaration(node)
      ) {
        return;
      }
      if (ts.isIdentifier(node)) {
        if (names.has(node.text) && !namesHere(node) && lookup(node, { mod }).kind === "module") {
          out.push(node);
        }
        return;
      }
      ts.forEachChild(node, visit);
    };
    visit(mod.sf);
  }
  memo.set(id, out);
  return out;
}

/** The components a node is drawn in: the one it sits in, or, in a helper of its module (a
 *  top-level function or constant), each component that reads the helper, however many helpers
 *  away. None for a node outside both, as the root `main.tsx` renders, or in a class's method
 *  other than its `render`. */
export function drawnIn(program: Program, node: ts.Node, seen = new Set<ts.Node>()): Comp[] {
  const comp = compOf(program, node);
  if (comp !== undefined) return [comp];
  const helper = moduleHelperOf(node);
  if (helper === undefined || seen.has(helper)) return [];
  seen.add(helper);
  const found = new Set<Comp>();
  for (const ref of moduleRefs(program, helperId(program, helper))) {
    for (const host of drawnIn(program, ref, seen)) found.add(host);
  }
  return [...found];
}

export function modOf(program: Program, node: ts.Node): Mod {
  const mod = program.mods.get(node.getSourceFile().fileName);
  if (mod === undefined) throw new Error(`no module for ${node.getSourceFile().fileName}`);
  return mod;
}

/** Where a node is, as `<path under the root>:<line>`: for a reader of a failure, never a key. */
export function where(node: ts.Node): string {
  const sf = node.getSourceFile();
  const line = sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1;
  return `${sf.fileName}:${line}`;
}

/** A node's own text with its whitespace run together, the form an item's key holds it in. */
export function text(node: ts.Node): string {
  return node.getText().replace(/\s+/g, " ").trim();
}

// ---------------------------------------------------------------------------------------------
// Scope: what an identifier inside a component names.

export type Found =
  | { readonly kind: "prop"; readonly prop: string }
  | { readonly kind: "props-object" }
  | {
      readonly kind: "local";
      readonly decl: ts.VariableDeclaration;
      readonly element: ts.BindingElement | undefined;
    }
  | { readonly kind: "param"; readonly fn: ts.SignatureDeclaration; readonly index: number }
  | { readonly kind: "function"; readonly node: ts.Node }
  | { readonly kind: "module"; readonly node: ts.Node | undefined }
  | { readonly kind: "global" };

export function bindingNamed(
  name: ts.BindingName,
  wanted: string
): ts.BindingElement | true | undefined {
  if (ts.isIdentifier(name)) return name.text === wanted ? true : undefined;
  for (const element of name.elements) {
    if (ts.isOmittedExpression(element)) continue;
    if (ts.isIdentifier(element.name)) {
      if (element.name.text === wanted) return element;
    } else {
      const inner = bindingNamed(element.name, wanted);
      if (inner !== undefined) return inner === true ? element : inner;
    }
  }
  return undefined;
}

function declaredIn(statements: readonly ts.Statement[], name: string): Found | undefined {
  for (const statement of statements) {
    if (ts.isVariableStatement(statement)) {
      for (const decl of statement.declarationList.declarations) {
        const hit = bindingNamed(decl.name, name);
        if (hit !== undefined) {
          return { decl, element: hit === true ? undefined : hit, kind: "local" };
        }
      }
    } else if (ts.isFunctionDeclaration(statement) && statement.name?.text === name) {
      return { kind: "function", node: statement };
    }
  }
  return undefined;
}

/** `this.props` or `this.state` in a class component. */
export function isThis(
  node: ts.Node,
  member: "props" | "state"
): node is ts.PropertyAccessExpression {
  return (
    ts.isPropertyAccessExpression(node) &&
    node.expression.kind === ts.SyntaxKind.ThisKeyword &&
    node.name.text === member
  );
}

/** A class component's local read out of `this.props` (`const { children } = this.props`) is
 *  the prop it names, as a function component's parameter is. */
function classProp(found: Found, comp: Pick<Comp, "cls">): Found {
  if (comp.cls === undefined || found.kind !== "local") return found;
  const init = found.decl.initializer;
  if (init === undefined || !isThis(init, "props")) return found;
  if (found.element === undefined) return { kind: "props-object" };
  let top = found.element;
  while (ts.isBindingElement(top.parent.parent)) top = top.parent.parent;
  return { kind: "prop", prop: (top.propertyName ?? top.name).getText() };
}

/** What `ident` names inside `comp`: one of its props, a local, a parameter of a function inside
 *  it, or something of its module or the globals. Looked up from a module's top level (`{ mod }`,
 *  no `fn`), a name no function or block binds is the module's. */
export function lookup(
  ident: ts.Identifier,
  comp: Pick<Comp, "cls" | "mod"> & { readonly fn?: ts.Node }
): Found {
  const name = ident.text;
  let node: ts.Node | undefined = ident.parent;
  while (node !== undefined) {
    if (ts.isFunctionLike(node)) {
      const params = (node as ts.SignatureDeclaration).parameters;
      for (let index = 0; index < params.length; index++) {
        const hit = bindingNamed(params[index].name, name);
        if (hit === undefined) continue;
        if (node === comp.fn) {
          if (hit === true) return { kind: "props-object" };
          return { kind: "prop", prop: (hit.propertyName ?? hit.name).getText() };
        }
        return { fn: node as ts.SignatureDeclaration, index, kind: "param" };
      }
    }
    if (ts.isBlock(node) || ts.isCaseClause(node) || ts.isDefaultClause(node)) {
      const found = declaredIn(node.statements, name);
      if (found !== undefined) return classProp(found, comp);
    }
    if (
      (ts.isForOfStatement(node) || ts.isForInStatement(node) || ts.isForStatement(node)) &&
      node.initializer !== undefined &&
      ts.isVariableDeclarationList(node.initializer)
    ) {
      for (const decl of node.initializer.declarations) {
        const hit = bindingNamed(decl.name, name);
        if (hit !== undefined) {
          return { decl, element: hit === true ? undefined : hit, kind: "local" };
        }
      }
    }
    if (node === comp.fn) break;
    node = node.parent;
  }
  const mod = comp.mod;
  if (mod.decls.has(name)) return { kind: "module", node: mod.decls.get(name) };
  if (mod.imports.has(name)) return { kind: "module", node: undefined };
  return { kind: "global" };
}

/** The identifiers an expression reads, leaving out property names, keys, JSX tags and types. */
export function readIdentifiers(root: ts.Node, skipFunctions = false): ts.Identifier[] {
  const out: ts.Identifier[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isTypeNode(node)) return;
    if (skipFunctions && node !== root && ts.isFunctionLike(node)) return;
    if (ts.isIdentifier(node)) {
      const parent = node.parent;
      const isName =
        (ts.isPropertyAccessExpression(parent) && parent.name === node) ||
        (ts.isPropertyAssignment(parent) && parent.name === node) ||
        (ts.isJsxAttribute(parent) && parent.name === node) ||
        ((ts.isJsxOpeningElement(parent) ||
          ts.isJsxSelfClosingElement(parent) ||
          ts.isJsxClosingElement(parent)) &&
          parent.tagName === node) ||
        (ts.isBindingElement(parent) && (parent.name === node || parent.propertyName === node)) ||
        (ts.isParameter(parent) && parent.name === node) ||
        (ts.isVariableDeclaration(parent) && parent.name === node);
      if (!isName) out.push(node);
      return;
    }
    ts.forEachChild(node, visit);
  };
  visit(root);
  return out;
}

export function calleeName(expression: ts.Expression): string | undefined {
  let node = expression;
  while (ts.isAwaitExpression(node) || ts.isParenthesizedExpression(node)) node = node.expression;
  if (!ts.isCallExpression(node)) return undefined;
  const callee = node.expression;
  if (ts.isIdentifier(callee)) return callee.text;
  if (ts.isPropertyAccessExpression(callee)) return callee.name.text;
  return undefined;
}

const STATE_HOOKS = new Set(["useState", "useReducer", "useSyncExternalStore"]);
const ROUTE_HOOKS = new Set([
  "useParams",
  "useSearchParams",
  "useLocation",
  "useMatch",
  "useMatches",
  "useNavigation",
]);
const QUERY_HOOKS = new Set(["useQuery", "useSuspenseQuery", "useQueries", "useInfiniteQuery"]);
const MEMO_HOOKS = new Set(["useMemo", "useCallback", "useDeferredValue"]);

/** One thing an expression depends on: a prop of its component by name, or another source
 *  (a state, the route, a read, a hook, a ref, a list's item, a parameter) the walk stops at. */
export interface Dep {
  readonly kind: "prop" | "state" | "route" | "query" | "hook" | "ref" | "list item" | "param";
  readonly name: string;
}

/** The props and state a class component's expression reads through `this`. */
function classReads(root: ts.Node): Dep[] {
  const deps: Dep[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isTypeNode(node)) return;
    for (const member of ["props", "state"] as const) {
      if (!isThis(node, member)) continue;
      const parent = node.parent;
      const name =
        ts.isPropertyAccessExpression(parent) && parent.expression === node
          ? parent.name.text
          : `(all ${member})`;
      deps.push({ kind: member === "props" ? "prop" : "state", name });
    }
    ts.forEachChild(node, visit);
  };
  visit(root);
  return deps;
}

/** What an expression inside `comp` depends on: its props, state, route, reads and hooks. */
export function dependsOn(root: ts.Node, comp: Comp, seen = new Set<ts.Node>()): Dep[] {
  const deps: Dep[] = comp.cls === undefined ? [] : classReads(root);
  for (const ident of readIdentifiers(root)) {
    const found = lookup(ident, comp);
    switch (found.kind) {
      case "prop":
        deps.push({ kind: "prop", name: found.prop });
        break;
      case "props-object": {
        const parent = ident.parent;
        const prop =
          ts.isPropertyAccessExpression(parent) && parent.expression === ident
            ? parent.name.text
            : "(all props)";
        deps.push({ kind: "prop", name: prop });
        break;
      }
      case "local": {
        if (seen.has(found.decl)) break;
        seen.add(found.decl);
        const init = found.decl.initializer;
        const local = ident.text;
        if (init === undefined) break;
        if (ts.isArrowFunction(init) || ts.isFunctionExpression(init)) break;
        const hook = calleeName(init);
        if (hook !== undefined && STATE_HOOKS.has(hook)) {
          deps.push({ kind: "state", name: local });
        } else if (hook !== undefined && ROUTE_HOOKS.has(hook)) {
          deps.push({ kind: "route", name: local });
        } else if (hook !== undefined && QUERY_HOOKS.has(hook)) {
          deps.push({ kind: "query", name: local });
          deps.push(...dependsOn(init, comp, seen));
        } else if (hook === "useRef") {
          deps.push({ kind: "ref", name: local });
        } else if (hook !== undefined && MEMO_HOOKS.has(hook)) {
          deps.push(...dependsOn(init, comp, seen));
        } else if (hook !== undefined && /^use[A-Z]/.test(hook)) {
          deps.push({ kind: "hook", name: local });
          deps.push(...dependsOn(init, comp, seen));
        } else {
          deps.push(...dependsOn(init, comp, seen));
        }
        break;
      }
      case "param": {
        const fn = found.fn;
        if (seen.has(fn)) break;
        seen.add(fn);
        const parent = fn.parent;
        const helper =
          ts.isVariableDeclaration(parent) && ts.isIdentifier(parent.name)
            ? parent.name.text
            : isModuleHelper(fn)
              ? helperName(fn)
              : undefined;
        if (ts.isCallExpression(parent) && ts.isPropertyAccessExpression(parent.expression)) {
          deps.push({ kind: "list item", name: ident.text });
          deps.push(...dependsOn(parent.expression.expression, comp, seen));
        } else if (helper !== undefined) {
          // A helper, the component's own or its module's: each call the component makes hands
          // the parameter its argument.
          for (const call of callsOf(helper, comp)) {
            const argument = call.arguments[found.index];
            if (argument !== undefined) deps.push(...dependsOn(argument, comp, seen));
          }
        } else {
          deps.push({ kind: "param", name: ident.text });
        }
        break;
      }
      default:
        break;
    }
  }
  return deps;
}

export function callsOf(name: string, comp: Comp): ts.CallExpression[] {
  const out: ts.CallExpression[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression)) {
      if (node.expression.text === name) out.push(node);
    }
    ts.forEachChild(node, visit);
  };
  if (comp.fn.body !== undefined) visit(comp.fn.body);
  return out;
}

export function referencesOf(name: string, decl: ts.Node, comp: Comp): ts.Identifier[] {
  const out: ts.Identifier[] = [];
  const visit = (node: ts.Node) => {
    const parent = node.parent;
    const isName =
      parent !== undefined &&
      ((ts.isPropertyAccessExpression(parent) && parent.name === node) ||
        (ts.isPropertyAssignment(parent) && parent.name === node) ||
        (ts.isJsxAttribute(parent) && parent.name === node) ||
        (ts.isBindingElement(parent) && parent.propertyName === node));
    if (ts.isIdentifier(node) && node.text === name && node.parent !== decl && !isName) {
      const found = lookup(node, comp);
      if (
        (found.kind === "local" && found.decl === decl) ||
        (found.kind === "function" && found.node === decl)
      ) {
        out.push(node);
      }
    }
    ts.forEachChild(node, visit);
  };
  if (comp.fn.body !== undefined) visit(comp.fn.body);
  return out;
}
